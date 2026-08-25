package hookcli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// hermesHookEvents lists the four Hermes Agent shell-hook events
// ConnectHermes wires a punk entry into. Each name is Hermes' own event key
// under the "hooks" mapping in ~/.hermes/config.yaml (hermes-agent.
// nousresearch.com/docs/user-guide/features/hooks). The set matches
// translateHermes' switch exactly - wiring an event with no translation
// would spawn a punk subprocess per occurrence only for it to forward
// nothing.
//
// Hermes documents four separate hook systems; only this one (shell hooks)
// spawns a subprocess with the event JSON on stdin, which is the only kind
// a punk binary can be wired into. The other three are Python-side
// (gateway event hooks under ~/.hermes/hooks/ and plugin hooks under
// ~/.hermes/plugins/, both requiring a handler module) or server-side
// (outbound webhooks, which POST to a URL - punk's own /v1/agent/hooks
// could in principle receive those directly, but the payload would arrive
// unauthenticated-by-punk and untranslated, so it is not what this wires).
//
// See translateHermes' doc comment for why on_session_end and pre_tool_call
// are deliberately left unwired.
var hermesHookEvents = []string{"on_session_start", "pre_llm_call", "post_tool_call", "post_llm_call"}

// hermesHookTimeoutSec is the per-hook timeout punk's managed entries
// declare. Hermes documents this field's default as 60 seconds with a
// maximum of 300; 10 matches every other agent's punk entry and stays well
// clear of the client's own 2-second HTTP timeout (httpClient, hookcli.go),
// so a hung memory server can never hold a user's Hermes turn for longer
// than that.
const hermesHookTimeoutSec = 10

// ConnectHermes merges punk hook entries into a Hermes Agent config.yaml,
// preserving user entries and every key punk does not manage. It gives the
// same guarantees as the JSON-config writers in this package
// (ConnectClaudeCode, ConnectCursor, ConnectCopilot), applied to YAML:
//
//   - configPath is read and parsed whole. A missing file is treated as an
//     empty document, not an error, so connect can create config.yaml
//     fresh. Any other read failure, or a parse failure, leaves the file
//     completely untouched - this is a live user config and a failed merge
//     must never clobber it.
//   - A wrong-shaped value at any key punk touches (root, "hooks", or one
//     of the four wired event keys) is refused with an error naming the
//     key, and the file is left untouched, rather than being silently
//     replaced with punk's own shape. An anchor/alias node at those
//     positions is refused too: writing through an alias would mutate
//     whatever anchor it points at, elsewhere in the user's document.
//   - Idempotent: an existing punk-managed entry for an event (see
//     isPunkManagedHermes) is replaced in place, never duplicated; foreign
//     entries, including hostile non-mapping sequence elements, are kept
//     as-is and in their original order.
//   - The write goes through writePreservingSymlinkAndMode (symlink
//     resolved, existing file mode preserved, atomic temp file + rename),
//     shared with every other Connect* writer here.
//
// One behavior differs from the JSON writers and is deliberate: the merge
// round-trips the document through yaml.v3's Node API and re-encodes it, so
// a hand-formatted config.yaml comes back with normalized indentation (2
// spaces) even where punk changed nothing. Comments (head, line and foot),
// key order, anchors and per-scalar quoting style all survive the
// round-trip because they are carried on the nodes themselves; whitespace
// layout does not. The consequence for the changed=false no-op check below
// is only that the FIRST connect against a hand-formatted file reports
// changed=true even if it is semantically a no-op; every subsequent run
// re-encodes byte-identically and correctly reports changed=false.
//
// Keys under "hooks" that punk does not wire are never inspected or
// rewritten in place - including "outbound", which is Hermes' webhook list
// living in the same mapping as the event keys and holding a completely
// different element shape (name/url/events/secret_env).
func ConnectHermes(configPath, punkPath, serverURL string) (changed bool, err error) {
	doc, existing, err := loadHermesConfig(configPath)
	if err != nil {
		return false, err
	}
	root := doc.Content[0]

	hooks, err := hermesChildMapping(root, "hooks", configPath)
	if err != nil {
		return false, err
	}

	command := punkHermesHookCommand(punkPath, serverURL)
	for _, ev := range hermesHookEvents {
		entries, err := hermesChildSequence(hooks, ev, configPath)
		if err != nil {
			return false, err
		}
		entries.Content = mergeHermesEntries(entries.Content, punkPath, command)
	}

	out, err := encodeHermesConfig(doc)
	if err != nil {
		return false, fmt.Errorf("encode config: %w", err)
	}
	if existing != nil && bytes.Equal(out, existing) {
		return false, nil
	}
	if err := writePreservingSymlinkAndMode(configPath, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// loadHermesConfig reads and parses configPath into a YAML document node
// whose single content child is guaranteed to be a mapping ConnectHermes
// can write into. A missing file, an empty file, or a file holding just the
// YAML null literal all yield a fresh empty mapping and nil existing bytes
// (the same normalization loadSettings applies to a JSON `null` body, and
// for the same reason: every caller would otherwise have to special-case a
// document with no root value).
//
// Any other read failure, a parse failure, or a root value that is present
// but not a mapping returns the error alongside a nil document - callers
// must treat that as "do not write". A multi-document file (--- separated)
// is refused as well: punk has no basis for picking which document holds
// Hermes' config, and merging into the first one would silently ignore the
// rest.
func loadHermesConfig(path string) (doc *yaml.Node, existing []byte, err error) {
	raw, readErr := os.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return newHermesDoc(), nil, nil
	}
	if readErr != nil {
		return nil, nil, fmt.Errorf("read config %s: %w", path, readErr)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return newHermesDoc(), raw, nil
	}

	// Decoded through a Decoder rather than yaml.Unmarshal specifically to
	// SEE a second document: Unmarshal silently decodes only the first one
	// in a "---"-separated stream, which would let a merge land in document
	// one while Hermes reads its config from another.
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var parsed yaml.Node
	if err := dec.Decode(&parsed); err != nil {
		return nil, nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, nil, fmt.Errorf("config %s holds multiple YAML documents; refusing to modify it", path)
	case errors.Is(err, io.EOF):
		// The expected case: exactly one document.
	default:
		return nil, nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	// Decoding into a Node yields a DocumentNode wrapping the root value. A
	// document whose root is the null literal decodes to a single
	// null-tagged scalar; normalize that to an empty mapping rather than
	// erroring on it below.
	if parsed.Kind != yaml.DocumentNode || len(parsed.Content) == 0 {
		return nil, nil, fmt.Errorf("config %s has no YAML document; refusing to modify it", path)
	}
	root := parsed.Content[0]
	if isHermesNull(root) {
		return newHermesDoc(), raw, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("config root is not a mapping; refusing to modify %s", path)
	}
	return &parsed, raw, nil
}

// newHermesDoc builds the empty document ConnectHermes starts from when
// there is nothing on disk to merge into.
func newHermesDoc() *yaml.Node {
	return &yaml.Node{
		Kind: yaml.DocumentNode,
		Content: []*yaml.Node{
			{Kind: yaml.MappingNode, Tag: "!!map"},
		},
	}
}

// isHermesNull reports whether n is the YAML null literal (an explicit
// "null"/"~", or a key written with no value at all, both of which decode
// to a null-tagged scalar). Callers treat it exactly like an absent key.
func isHermesNull(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// hermesChildMapping returns the mapping stored at key inside parent,
// creating and inserting an empty one when the key is absent or null. A
// present value of any other kind is an error naming the key: silently
// replacing it would destroy user config. An alias node is refused for the
// same reason a wrong shape is - it points at an anchor defined elsewhere
// in the document, so appending through it would edit that anchor's target
// rather than this key.
func hermesChildMapping(parent *yaml.Node, key, path string) (*yaml.Node, error) {
	existing := yamlMapValue(parent, key)
	if existing != nil && !isHermesNull(existing) {
		if existing.Kind == yaml.AliasNode {
			return nil, fmt.Errorf("%s is a YAML alias; refusing to modify %s", key, path)
		}
		if existing.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s is not a mapping; refusing to modify %s", key, path)
		}
		return existing, nil
	}
	created := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	yamlMapSet(parent, key, created)
	return created, nil
}

// hermesChildSequence is hermesChildMapping's sequence twin, used for the
// per-event entry lists under "hooks". Same absent/null-creates,
// wrong-shape-refuses, alias-refuses contract.
func hermesChildSequence(parent *yaml.Node, key, path string) (*yaml.Node, error) {
	existing := yamlMapValue(parent, key)
	if existing != nil && !isHermesNull(existing) {
		if existing.Kind == yaml.AliasNode {
			return nil, fmt.Errorf("hooks.%s is a YAML alias; refusing to modify %s", key, path)
		}
		if existing.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("hooks.%s is not a sequence; refusing to modify %s", key, path)
		}
		return existing, nil
	}
	created := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	yamlMapSet(parent, key, created)
	return created, nil
}

// yamlMapValue returns the value node stored under key in the mapping node
// m, or nil when the key is absent. Mapping content alternates key, value,
// key, value; only scalar keys are compared, so a mapping using a complex
// (sequence/mapping) key never matches and is skipped rather than
// misidentified by its rendered form.
func yamlMapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Kind == yaml.ScalarNode && m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlMapSet stores val under key in the mapping node m, replacing the
// existing value in place when the key is already present (preserving key
// order and any comment attached to the key node) and appending a new
// key/value pair otherwise.
func yamlMapSet(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Kind == yaml.ScalarNode && m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		val)
}

// mergeHermesEntries returns one event's entry list with any stale
// punk-managed entry (see isPunkManagedHermesEntry) removed and a fresh one
// carrying command appended last. Foreign entries - including sequence
// elements that are not mappings at all - are kept untouched and in their
// original order.
func mergeHermesEntries(entries []*yaml.Node, punkPath, command string) []*yaml.Node {
	kept := make([]*yaml.Node, 0, len(entries)+1)
	for _, e := range entries {
		if isPunkManagedHermesEntry(e, punkPath) {
			continue
		}
		kept = append(kept, e)
	}
	return append(kept, hermesEntryNode(command))
}

// hermesEntryNode builds the mapping node punk writes for one event:
// {command: "<command>", timeout: <hermesHookTimeoutSec>}. No "matcher" key
// is emitted: Hermes documents matcher as optional and tool-events-only,
// and punk wants every tool call captured, not a filtered subset.
//
// The command scalar is emitted double-quoted unconditionally rather than
// left to the encoder's plain-style heuristics. punkPath and serverURL are
// operator-supplied (a --url flag, $PUNK_URL, os.Executable), not attacker
// data, but per .claude/rules/ai.md ("ALWAYS unconditionally escape
// LLM-authored strings embedded in structured formats") the same discipline
// is applied to every value spliced into a structured document: a path or
// URL containing a colon-space, a leading "&"/"*"/"!", or a newline would
// otherwise change the document's structure instead of its content. The
// yaml encoder performs the escaping itself for a DoubleQuotedStyle scalar.
func hermesEntryNode(command string) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "command"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: command, Style: yaml.DoubleQuotedStyle},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "timeout"},
			{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(hermesHookTimeoutSec)},
		},
	}
}

// isPunkManagedHermesEntry reports whether entry is a punk-managed Hermes
// hook entry: a mapping whose "command" value is a scalar string that
// isPunkManagedHermes recognizes. Anything else - a bare scalar element, a
// nested sequence, a mapping with no "command" key or a non-scalar one - is
// never treated as ours, so a hostile or merely differently-shaped element
// is left alone rather than silently dropped.
func isPunkManagedHermesEntry(entry *yaml.Node, punkPath string) bool {
	if entry == nil || entry.Kind != yaml.MappingNode {
		return false
	}
	cmd := yamlMapValue(entry, "command")
	if cmd == nil || cmd.Kind != yaml.ScalarNode || cmd.Value == "" {
		return false
	}
	return isPunkManagedHermes(cmd.Value, punkPath)
}

// isPunkManagedHermes is the "hermes" binding of the shared
// isPunkManagedFromAgent (connect.go); see its doc comment for the
// primary/fallback rule pair and the binary-relocation tradeoff.
func isPunkManagedHermes(cmd, punkPath string) bool {
	return isPunkManagedFromAgent(cmd, punkPath, "hermes")
}

// punkHermesHookCommand builds the shell command punk's managed Hermes hook
// entries invoke, sharing quotePunkPath's whitespace-quoting with every
// other agent's command builder so isPunkManagedHermes' optional-quote
// tolerance applies identically on a later run.
//
// One command string serves all four wired events: Hermes' shell-hook
// payload carries hook_event_name (unlike Antigravity's event-blind
// payloads, which forced per-event command strings), so translateHermes
// identifies the event from stdin and no --event flag is needed.
func punkHermesHookCommand(punkPath, serverURL string) string {
	return fmt.Sprintf("%s hook --from hermes --url %s", quotePunkPath(punkPath), serverURL)
}

// encodeHermesConfig renders the merged document the way ConnectHermes
// persists it: 2-space indent and a trailing newline (yaml.v3's encoder
// always terminates the final line). The encoder is explicitly closed
// before the buffer is read - Close is what flushes the final document, so
// reading buf earlier would yield truncated output.
func encodeHermesConfig(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
