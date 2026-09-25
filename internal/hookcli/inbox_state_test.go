package hookcli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The inbox state file is keyed by server + namespace + address: two
// servers (or two namespaces on one server) serving the same session
// address must never share a continuation budget or a registration mark.
func TestInboxStatePathKeysServerNamespaceAndAddress(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := inboxStatePath("claude-code", "http://a:9090", "ns1", "claude-code:s1")
	for name, other := range map[string]string{
		"server":    inboxStatePath("claude-code", "http://b:9090", "ns1", "claude-code:s1"),
		"namespace": inboxStatePath("claude-code", "http://a:9090", "ns2", "claude-code:s1"),
		"address":   inboxStatePath("claude-code", "http://a:9090", "ns1", "claude-code:s2"),
	} {
		if other == base {
			t.Errorf("state path does not vary with %s: %s", name, base)
		}
	}
	if again := inboxStatePath("claude-code", "http://a:9090", "ns1", "claude-code:s1"); again != base {
		t.Errorf("state path not deterministic: %s vs %s", again, base)
	}
	root := os.Getenv("XDG_STATE_HOME")
	if !strings.HasPrefix(base, filepath.Join(root, "punk", "inbox", "claude-code")+string(filepath.Separator)) {
		t.Errorf("state path %s not under $XDG_STATE_HOME/punk/inbox/<client>/", base)
	}
	// A hostile client name or address must never escape the directory.
	evil := inboxStatePath("../../etc", "http://a", "ns", "../../x")
	if !strings.HasPrefix(evil, filepath.Join(root, "punk", "inbox")+string(filepath.Separator)) ||
		strings.Contains(strings.TrimPrefix(evil, root), "..") {
		t.Errorf("state path escaped: %s", evil)
	}
}

func TestInboxStateDefaultsToLocalStateWhenXDGUnset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	p := inboxStatePath("codex", "http://a", "ns", "codex:s")
	if !strings.HasPrefix(p, filepath.Join(home, ".local", "state", "punk", "inbox", "codex")) {
		t.Errorf("default state path %s not under ~/.local/state/punk/inbox", p)
	}
}

func TestInboxStateCorruptFileIsIgnoredAndRewritten(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := inboxStatePath("claude-code", "http://a", "ns", "claude-code:s")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen inboxState
	err := updateInboxState(p, func(s *inboxState) {
		seen = *s
		s.RegisteredAt = 42
	})
	if err != nil {
		t.Fatalf("corrupt state must not be fatal: %v", err)
	}
	if seen.RegisteredAt != 0 || len(seen.Continuations) != 0 {
		t.Errorf("corrupt state leaked values: %+v", seen)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var back inboxState
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("state not rewritten as valid JSON: %q (%v)", raw, err)
	}
	if back.RegisteredAt != 42 {
		t.Errorf("rewritten state lost update: %+v", back)
	}
}

// reserveContinuation is the cap: max continuations inside a sliding
// window, the (max+1)th inside the window is refused, and the first one
// after the window has slid past the old entries is allowed again.
func TestInboxReserveContinuationSlidingWindow(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := inboxStatePath("claude-code", "http://a", "ns", "claude-code:s")
	t0 := time.Unix(1_800_000_000, 0)
	window := 10 * time.Minute
	for i := 0; i < 5; i++ {
		ok, err := reserveContinuation(p, t0.Add(time.Duration(i)*time.Second), 5, window)
		if err != nil || !ok {
			t.Fatalf("continuation %d refused: ok=%v err=%v", i+1, ok, err)
		}
	}
	if ok, _ := reserveContinuation(p, t0.Add(9*time.Minute), 5, window); ok {
		t.Fatal("6th continuation inside the window was allowed")
	}
	// All five were at t0..t0+4s; at t0+10m+5s they have all aged out.
	if ok, err := reserveContinuation(p, t0.Add(window+5*time.Second), 5, window); err != nil || !ok {
		t.Fatalf("first continuation after the window refused: ok=%v err=%v", ok, err)
	}
	if ok, _ := reserveContinuation(p, t0, 0, window); ok {
		t.Error("max=0 must disable continuation")
	}
	if ok, _ := peekContinuation(p, t0.Add(window+6*time.Second), 5, window); !ok {
		t.Error("peek must report room after the window")
	}
}

// Concurrent hook subprocesses (a Stop and a UserPromptSubmit firing
// back to back) must never both win the last slot: the update is a
// locked read-modify-write, not just an atomic rename. Goroutines in one
// process exercise the same OS file lock the subprocesses take.
func TestInboxReserveContinuationSerializesConcurrentWriters(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := inboxStatePath("claude-code", "http://a", "ns", "claude-code:s")
	now := time.Unix(1_800_000_000, 0)
	const racers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := reserveContinuation(p, now, 5, 10*time.Minute)
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			if ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 5 {
		t.Fatalf("granted %d continuations under contention, want exactly 5", granted)
	}
	raw, _ := os.ReadFile(p)
	var s inboxState
	if err := json.Unmarshal(raw, &s); err != nil || len(s.Continuations) != 5 {
		t.Fatalf("state after race: %q err=%v", raw, err)
	}
}

func TestInboxStateRecordsAckedIDsBounded(t *testing.T) {
	var s inboxState
	for i := 0; i < inboxAckMemory+10; i++ {
		s.rememberAcked([]string{"id" + string(rune('a'+i%26)) + time.Duration(i).String()})
	}
	if len(s.LastAckIDs) != inboxAckMemory {
		t.Fatalf("ack memory %d, want bounded at %d", len(s.LastAckIDs), inboxAckMemory)
	}
}

// The cap must hold across real OS processes, not only goroutines: the
// test binary re-executes itself as N concurrent reservers.
func TestInboxReserveContinuationAcrossProcesses(t *testing.T) {
	if p := os.Getenv("PUNK_INBOX_RESERVE_CHILD"); p != "" {
		ok, err := reserveContinuation(p, time.Unix(1_800_000_000, 0), 5, 10*time.Minute)
		if err != nil {
			os.Exit(3)
		}
		if ok {
			os.Exit(0)
		}
		os.Exit(1)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := inboxStatePath("codex", "http://a", "ns", "codex:s")
	const procs = 10
	cmds := make([]*exec.Cmd, procs)
	for i := range cmds {
		cmds[i] = exec.Command(os.Args[0], "-test.run=^TestInboxReserveContinuationAcrossProcesses$")
		cmds[i].Env = append(os.Environ(), "PUNK_INBOX_RESERVE_CHILD="+path)
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	granted := 0
	for _, c := range cmds {
		err := c.Wait()
		var ee *exec.ExitError
		switch {
		case err == nil:
			granted++
		case errors.As(err, &ee) && ee.ExitCode() == 1:
		default:
			t.Fatalf("child failed: %v", err)
		}
	}
	if granted != 5 {
		t.Fatalf("%d processes won a continuation, want exactly 5", granted)
	}
}
