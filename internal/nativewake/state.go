package nativewake

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Persistent wake state: the reserved-attempt budget and the unread-set
// cooldown fingerprint survive a listener restart, so a daemon bounce
// cannot reset the storm cap. The file never carries credentials, peer
// message bodies, or transport details - only timestamps and one opaque
// metadata fingerprint.
//
// Corrupt state fails closed: Run refuses to start rather than silently
// reset the budget and risk a wake storm.

const wakeStateFile = "nativewake-state.json"

// wakeLockFile is the run-lifetime advisory lock inside StateDir.
const wakeLockFile = "nativewake.lock"

// wakeStateVersion is the only schema this build reads.
const wakeStateVersion = 1

// maxStoredAttempts bounds the attempts vector a state file may carry;
// anything larger is treated as corruption, not as a quota.
const maxStoredAttempts = 4096

// wakeState is the on-disk JSON document (versioned for migration).
type wakeState struct {
	Version int `json:"version"`
	// Identity scopes the budget to one endpoint + namespace + address
	// (URL\nNS\naddress). A StateDir reused for a different session or
	// server never inherits the previous owner's quota.
	Identity string `json:"identity,omitempty"`
	// Attempts are reserved wake-attempt timestamps (unix nanos), oldest
	// first. An attempt is reserved on disk BEFORE any host handoff, so a
	// crash mid-write still counts it (conservative storm accounting).
	Attempts []int64 `json:"attempts,omitempty"`
	// Fingerprint identifies the unread metadata set the last successful
	// wake was for; LastWakeAt (unix nanos) starts its cooldown.
	Fingerprint string `json:"fingerprint,omitempty"`
	LastWakeAt  int64  `json:"last_wake_at,omitempty"`
}

// validate rejects a state file that cannot be trusted as a quota
// ledger: wrong schema version, an unbounded or unsorted attempts
// vector, or timestamps beyond reasonable clock skew.
func (st *wakeState) validate(now time.Time) error {
	if st.Version != wakeStateVersion {
		return fmt.Errorf("wake state version %d, want %d", st.Version, wakeStateVersion)
	}
	if len(st.Attempts) > maxStoredAttempts {
		return fmt.Errorf("wake state carries %d attempts (max %d)", len(st.Attempts), maxStoredAttempts)
	}
	skew := now.Add(time.Minute).UnixNano()
	for i, a := range st.Attempts {
		if i > 0 && a < st.Attempts[i-1] {
			return fmt.Errorf("wake state attempts unsorted at index %d", i)
		}
		if a > skew {
			return fmt.Errorf("wake state attempt %d is in the future", i)
		}
	}
	return nil
}

// loadWakeState reads the state file; a missing file or a foreign
// identity (different server, namespace or address) is a fresh budget.
// Corrupt or untrustworthy content fails closed.
func loadWakeState(dir, identity string) (*wakeState, error) {
	raw, err := os.ReadFile(filepath.Join(dir, wakeStateFile))
	if err != nil {
		if os.IsNotExist(err) {
			return &wakeState{Version: wakeStateVersion, Identity: identity}, nil
		}
		return nil, fmt.Errorf("read wake state: %w", err)
	}
	var st wakeState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("wake state corrupt (budget preserved by refusing to run): %w", err)
	}
	if err := st.validate(time.Now()); err != nil {
		return nil, fmt.Errorf("wake state invalid (refusing to run): %w", err)
	}
	if st.Identity != identity {
		return &wakeState{Version: wakeStateVersion, Identity: identity}, nil
	}
	return &st, nil
}

// saveWakeState writes atomically (tmp + rename) with owner-only
// permissions, leaving no temp files behind.
func saveWakeState(dir string, st *wakeState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".wake-state-*.tmp")
	if err != nil {
		return fmt.Errorf("state tmp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, wakeStateFile)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("state rename: %w", err)
	}
	return nil
}

// pruneAttempts drops reserved attempts older than the sliding window.
func pruneAttempts(st *wakeState, now time.Time, window time.Duration) {
	cutoff := now.Add(-window).UnixNano()
	keep := 0
	for _, a := range st.Attempts {
		if a > cutoff {
			st.Attempts[keep] = a
			keep++
		}
	}
	st.Attempts = st.Attempts[:keep]
}

// reserveAttempt appends one attempt timestamp and persists it before
// the caller attempts any host handoff.
func reserveAttempt(dir string, st *wakeState, now time.Time) error {
	st.Attempts = append(st.Attempts, now.UnixNano())
	return saveWakeState(dir, st)
}

// refundAttempt removes the most recently reserved attempt (a deferral
// that never reached the host) and persists the smaller budget.
func refundAttempt(dir string, st *wakeState) error {
	if len(st.Attempts) == 0 {
		return nil
	}
	st.Attempts = st.Attempts[:len(st.Attempts)-1]
	return saveWakeState(dir, st)
}
