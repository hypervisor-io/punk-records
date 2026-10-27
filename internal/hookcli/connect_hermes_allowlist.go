package hookcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Hermes' shell-hook allowlist: the consent file Hermes checks before
// running a shell hook without prompting. Format verified against
// hermes-agent's own agent/shell_hooks.py (8301654) and
// hermes-agent.nousresearch.com/docs/user-guide/features/hooks:
//
//   - Path: $HERMES_HOME/shell-hooks-allowlist.json when HERMES_HOME is
//     set, else the platform default (~/.hermes on POSIX,
//     %LOCALAPPDATA%\hermes on Windows).
//   - Schema: {"approvals":[{"event":<string>,"command":<string>,
//     "approved_at":<ISO-8601 Z>,"script_mtime_at_approval":<ISO-8601 Z
//     or null>}]}.
//   - Hermes matches approvals by EXACT (event, command) string equality,
//     so the entries written here carry byte-for-byte the same command
//     strings ConnectHermes writes into config.yaml (punkHermesHookCommand
//     and punkInboxHookCommand) - any drift means the approval never
//     matches and Hermes prompts anyway.
//   - A missing or invalid file behaves as {"approvals":[]} (Hermes
//     recreates it on the first interactive approval).
//
// One coverage fact the connect output must never contradict (P4-D spike,
// verified against hermes-agent's source): shell hooks run in the Hermes
// CLI and Gateway only - the Desktop backend never registers them, so an
// allowlist entry approves nothing there.

// hermesAllowlistFile is the allowlist's file name inside the Hermes home
// directory, on every platform.
const hermesAllowlistFile = "shell-hooks-allowlist.json"

// HermesAllowlistPath returns the shell-hook allowlist path Hermes itself
// reads: $HERMES_HOME/shell-hooks-allowlist.json when HERMES_HOME is set,
// else %LOCALAPPDATA%\hermes on Windows and ~/.hermes everywhere else.
// An unresolvable home falls back to a relative .hermes path, the same
// degradation CredentialsPath applies.
func HermesAllowlistPath() string {
	if h := os.Getenv("HERMES_HOME"); h != "" {
		return filepath.Join(h, hermesAllowlistFile)
	}
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "hermes", hermesAllowlistFile)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".hermes", hermesAllowlistFile)
	}
	return filepath.Join(home, ".hermes", hermesAllowlistFile)
}

// hermesApproval is one allowlist entry, field for field Hermes' own
// schema. ScriptMtimeAtApproval is a pointer so the JSON null the schema
// allows is distinguishable from an absent field on both decode and
// encode.
type hermesApproval struct {
	Event                 string  `json:"event"`
	Command               string  `json:"command"`
	ApprovedAt            string  `json:"approved_at"`
	ScriptMtimeAtApproval *string `json:"script_mtime_at_approval"`
}

// ConnectHermesAllowlist merges punk's shell-hook approvals into a Hermes
// shell-hooks-allowlist.json, so Hermes runs punk's hooks without a
// consent prompt. It gives the same guarantees ConnectHermes gives for
// config.yaml, applied to this JSON file:
//
//   - allowlistPath is read and parsed whole. A missing file is treated as
//     an empty document, not an error, so connect can create the allowlist
//     fresh; Hermes itself recreates a missing file the same way. Any
//     other read failure, a parse failure, or a wrong-shaped document
//     (root not an object, "approvals" not an array, trailing content)
//     leaves the file completely untouched and returns an error - the
//     caller (punk connect hermes) turns that into the hooks_auto_accept
//     guidance rather than failing the connect.
//   - Foreign approvals - anything whose command punk does not manage,
//     including differently-shaped elements punk cannot decode at all -
//     are kept as-is and in their original order, unknown fields included.
//   - punk-managed entries (isPunkManagedHermes or isPunkManagedInbox, so
//     a stale entry from a relocated binary or an old URL is still
//     recognized as ours) that match one of the desired (event, command)
//     pairs EXACTLY are kept in place with their original timestamps -
//     Hermes matches by string equality and ignores the timestamps for
//     matching, so refreshing them would rewrite the file on every
//     connect for nothing and break the changed=false no-op check.
//     punk-managed entries that match nothing desired (stale path, stale
//     URL) are dropped and replaced by fresh ones appended last, never
//     duplicated.
//   - Fresh entries carry approved_at = now (UTC, ISO-8601 Z) and
//     script_mtime_at_approval = the mtime of the punk binary the command
//     invokes, or null when it cannot be stat'd.
//   - The write goes through writePreservingSymlinkAndMode (symlink
//     resolved, existing file mode preserved, atomic temp file + rename),
//     shared with every other Connect* writer here.
//
// messaging adds the approval for punk's inbox entry ("punk hook inbox
// --client hermes ..." on pre_llm_call), mirroring ConnectHermesMessaging.
//
// Like ConnectHermes, the merge re-encodes the whole document, so the
// FIRST run against a hand-formatted file reports changed=true even when
// it is semantically a no-op; every subsequent run is byte-identical and
// reports changed=false.
func ConnectHermesAllowlist(allowlistPath, punkPath, serverURL string, messaging bool) (changed bool, err error) {
	return connectHermesAllowlist(allowlistPath, punkPath, serverURL, messaging, time.Now)
}

// connectHermesAllowlist is ConnectHermesAllowlist with an injectable
// clock, so tests can pin approved_at.
func connectHermesAllowlist(allowlistPath, punkPath, serverURL string, messaging bool, now func() time.Time) (changed bool, err error) {
	desired := hermesDesiredApprovals(punkPath, serverURL, messaging)
	doc, existing, err := loadHermesAllowlist(allowlistPath)
	if err != nil {
		return false, err
	}

	matched := make([]bool, len(desired))
	kept := make([]json.RawMessage, 0, len(doc.Approvals)+len(desired))
	for _, e := range doc.Approvals {
		var a hermesApproval
		if json.Unmarshal(e, &a) == nil && a.Command != "" && isPunkManagedHermesApproval(a.Command, punkPath) {
			// punk-managed: kept only when it exactly approves one of the
			// entries punk wires now; a stale one is dropped and replaced
			// by the fresh append below.
			if i := findHermesApproval(desired, matched, a.Event, a.Command); i >= 0 {
				matched[i] = true
				kept = append(kept, e)
			}
			continue
		}
		// Foreign (or undecodable): preserved as-is, in order.
		kept = append(kept, e)
	}

	approvedAt := now().UTC().Format(time.RFC3339)
	mtime := hermesScriptMtime(punkPath)
	for i, d := range desired {
		if matched[i] {
			continue
		}
		d.ApprovedAt = approvedAt
		d.ScriptMtimeAtApproval = mtime
		raw, err := json.Marshal(d)
		if err != nil {
			return false, fmt.Errorf("encode approval: %w", err)
		}
		kept = append(kept, raw)
	}

	out, err := encodeHermesAllowlist(kept)
	if err != nil {
		return false, fmt.Errorf("encode allowlist: %w", err)
	}
	if existing != nil && bytes.Equal(out, existing) {
		return false, nil
	}
	if err := writePreservingSymlinkAndMode(allowlistPath, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// hermesDesiredApprovals lists the (event, command) pairs punk wires into
// config.yaml right now: one capture approval per wired event (all four
// share one command string - see punkHermesHookCommand) and, with
// messaging, the pre_llm_call inbox approval. Timestamps are left for the
// caller to stamp on the entries it actually appends.
func hermesDesiredApprovals(punkPath, serverURL string, messaging bool) []hermesApproval {
	command := punkHermesHookCommand(punkPath, serverURL)
	out := make([]hermesApproval, 0, len(hermesHookEvents)+1)
	for _, ev := range hermesHookEvents {
		out = append(out, hermesApproval{Event: ev, Command: command})
	}
	if messaging {
		out = append(out, hermesApproval{
			Event:   "pre_llm_call",
			Command: punkInboxHookCommand(punkPath, "hermes", "context", "", serverURL, ""),
		})
	}
	return out
}

// isPunkManagedHermesApproval reports whether an allowlist entry's
// command is one punk manages: either the capture entry or the inbox
// entry for this client. The two detectors stay separate (see
// isPunkManagedInbox's doc comment) so a capture approval never dedups an
// inbox approval.
func isPunkManagedHermesApproval(cmd, punkPath string) bool {
	return isPunkManagedHermes(cmd, punkPath) || isPunkManagedInbox(cmd, punkPath, "hermes")
}

// findHermesApproval returns the index of the not-yet-matched desired
// approval with exactly this (event, command) pair - Hermes' own matching
// rule - or -1 when there is none.
func findHermesApproval(desired []hermesApproval, matched []bool, event, command string) int {
	for i, d := range desired {
		if !matched[i] && d.Event == event && d.Command == command {
			return i
		}
	}
	return -1
}

// hermesScriptMtime returns the punk binary's mtime as the schema's
// ISO-8601 Z string, or nil (JSON null) when the binary cannot be stat'd.
// The field is informational for Hermes - matching is exact (event,
// command) equality - so a null here never weakens an approval.
func hermesScriptMtime(punkPath string) *string {
	fi, err := os.Stat(punkPath)
	if err != nil {
		return nil
	}
	s := fi.ModTime().UTC().Format(time.RFC3339)
	return &s
}

// hermesAllowlistDoc is the raw decode shape: approvals stay RawMessage
// so foreign entries survive the merge byte-for-byte (unknown fields and
// key order included) instead of round-tripping through a struct that
// only knows punk's four.
type hermesAllowlistDoc struct {
	Approvals []json.RawMessage `json:"approvals"`
}

// loadHermesAllowlist reads and parses allowlistPath. A missing file
// yields an empty document and nil existing bytes (not an error), the
// same normalization loadHermesConfig applies, matching Hermes' own
// "missing file behaves as {"approvals":[]}" rule. An empty or
// whitespace-only file yields an empty document and its raw bytes. Any
// other read failure, a parse failure, a wrong-shaped root or "approvals"
// value, or trailing content after the document returns the error
// alongside a nil-existing - callers must treat that as "do not write".
// A JSON null at "approvals" decodes to a nil slice and is treated
// exactly like an absent key.
func loadHermesAllowlist(path string) (doc hermesAllowlistDoc, existing []byte, err error) {
	raw, readErr := os.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return hermesAllowlistDoc{}, nil, nil
	}
	if readErr != nil {
		return hermesAllowlistDoc{}, nil, fmt.Errorf("read allowlist %s: %w", path, readErr)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return hermesAllowlistDoc{}, raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil {
		return hermesAllowlistDoc{}, nil, fmt.Errorf("parse allowlist %s: %w", path, err)
	}
	// Reject anything after the first JSON value, the loadSettings
	// discipline: a merge must never silently discard a second value or a
	// malformed tail when it rewrites the file.
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return hermesAllowlistDoc{}, nil, fmt.Errorf("parse allowlist %s: trailing content: %w", path, err)
	}
	return doc, raw, nil
}

// encodeHermesAllowlist renders the merged document the way
// ConnectHermesAllowlist persists it: 2-space indent, HTML escaping off
// (command strings hold paths and URLs, not markup), trailing newline.
func encodeHermesAllowlist(approvals []json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		Approvals []json.RawMessage `json:"approvals"`
	}{Approvals: approvals}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
