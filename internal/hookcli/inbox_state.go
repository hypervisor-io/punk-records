package hookcli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Inbox state: one small JSON file per (server, namespace, address)
// under $XDG_STATE_HOME/punk/inbox/<client>/ (~/.local/state when
// unset). punk hook inbox is a fresh subprocess per hook event, so this
// file is the only memory it has between events: whether the address is
// already registered as a namespace member, the continuation timestamps
// the loop cap counts, and the last acknowledged ids.
//
// Two hook subprocesses for one session can run back to back or
// overlap (a Stop hook and the next UserPromptSubmit), so every
// read-modify-write uses a sibling .lock file: flock on Unix; on non-Unix
// platforms (including Windows), an O_EXCL-created lock file with cleanup
// of files older than 10 seconds, presumed abandoned. The atomic
// temp-file rename alone would only prevent torn files, not two
// processes both reading "4 continuations" and both writing 5.

// inboxAckMemory bounds how many recently acknowledged ids the state
// file remembers (a read/ack race guard, never a source of truth).
const inboxAckMemory = 64

// inboxLockTimeout bounds how long a hook waits for the state lock; on
// timeout the caller fails safe (no continuation, re-register later).
var inboxLockTimeout = 2 * time.Second

type inboxState struct {
	Server        string   `json:"server,omitempty"`
	Namespace     string   `json:"namespace,omitempty"`
	Address       string   `json:"address,omitempty"`
	RegisteredAt  int64    `json:"registered_at,omitempty"`
	Continuations []int64  `json:"continuations"`
	LastAckIDs    []string `json:"last_ack_ids"`
}

func (s *inboxState) rememberAcked(ids []string) {
	s.LastAckIDs = append(s.LastAckIDs, ids...)
	if over := len(s.LastAckIDs) - inboxAckMemory; over > 0 {
		s.LastAckIDs = append([]string(nil), s.LastAckIDs[over:]...)
	}
}

func (s *inboxState) recentlyAcked(id string) bool {
	for _, v := range s.LastAckIDs {
		if v == id {
			return true
		}
	}
	return false
}

var inboxUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// inboxSafe maps s onto a short filename-safe slug. Dots are replaced
// too, so no component can ever be "." or "..".
func inboxSafe(s string, max int) string {
	out := inboxUnsafe.ReplaceAllString(s, "_")
	if len(out) > max {
		out = out[:max]
	}
	if out == "" {
		out = "_"
	}
	return out
}

func inboxStateRoot() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "punk", "inbox")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "punk-state", "inbox")
	}
	return filepath.Join(home, ".local", "state", "punk", "inbox")
}

// inboxStatePath is the state file for one delivery identity. The name
// keeps a readable address slug but the hash of server, namespace and
// address is what makes it unique: the same session address against two
// servers or two namespaces never shares a cap or a registration mark.
func inboxStatePath(client, server, namespace, address string) string {
	sum := sha256.Sum256([]byte(server + "\x00" + namespace + "\x00" + address))
	name := inboxSafe(address, 48) + "-" + hex.EncodeToString(sum[:8]) + ".json"
	return filepath.Join(inboxStateRoot(), inboxSafe(client, 32), name)
}

// readInboxState loads the state without locking. A missing or corrupt
// file is an empty state, never an error: the next update rewrites it.
func readInboxState(path string) inboxState {
	var s inboxState
	raw, err := os.ReadFile(path)
	if err != nil {
		return inboxState{}
	}
	if json.Unmarshal(raw, &s) != nil {
		return inboxState{}
	}
	return s
}

// updateInboxState runs fn on the current state under the exclusive
// state lock and writes the result atomically (temp file + rename).
func updateInboxState(path string, fn func(*inboxState)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("inbox state dir: %w", err)
	}
	unlock, err := lockInboxFile(path+".lock", inboxLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	s := readInboxState(path)
	fn(&s)
	if s.Continuations == nil {
		s.Continuations = []int64{}
	}
	if s.LastAckIDs == nil {
		s.LastAckIDs = []string{}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeAtomic(path, append(raw, '\n'), 0o600)
}

// errInboxLockTimeout: another hook held the state lock too long.
var errInboxLockTimeout = errors.New("inbox state: lock timeout")

func pruneContinuations(c []int64, now time.Time, window time.Duration) []int64 {
	cutoff := now.Add(-window).Unix()
	kept := c[:0:0]
	for _, t := range c {
		if t > cutoff {
			kept = append(kept, t)
		}
	}
	return kept
}

// peekContinuation reports, without reserving, whether the sliding
// window still has room. Used to decide the mode before any fetch; the
// binding decision is reserveContinuation's.
func peekContinuation(path string, now time.Time, max int, window time.Duration) (bool, error) {
	if max <= 0 {
		return false, nil
	}
	s := readInboxState(path)
	return len(pruneContinuations(s.Continuations, now, window)) < max, nil
}

// reserveContinuation atomically takes one continuation slot: at most
// max inside window, pruning entries that aged out. It is the single
// admission point for the cap, so concurrent hooks cannot overshoot it.
// Any error means no slot (fail safe: fall back to context mode).
func reserveContinuation(path string, now time.Time, max int, window time.Duration) (bool, error) {
	if max <= 0 {
		return false, nil
	}
	granted := false
	err := updateInboxState(path, func(s *inboxState) {
		s.Continuations = pruneContinuations(s.Continuations, now, window)
		if len(s.Continuations) < max {
			s.Continuations = append(s.Continuations, now.Unix())
			granted = true
		}
	})
	if err != nil {
		return false, err
	}
	return granted, nil
}

// refundContinuation gives back a slot reserved at stamp whose
// continuation never reached the client (print failed, adapter declined).
func refundContinuation(path string, stamp int64) error {
	return updateInboxState(path, func(s *inboxState) {
		for i := len(s.Continuations) - 1; i >= 0; i-- {
			if s.Continuations[i] == stamp {
				s.Continuations = append(s.Continuations[:i], s.Continuations[i+1:]...)
				return
			}
		}
	})
}
