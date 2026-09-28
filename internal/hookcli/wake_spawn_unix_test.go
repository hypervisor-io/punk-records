//go:build unix

package hookcli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/nativewake"
)

// Real-spawn tests for the Unix detachment helper: workers are helper
// processes (this test binary re-executed). Proves the private config
// rides the pipe intact, the spawn returns promptly without waiting on
// the child, the child runs in the private state dir, the worker log is
// truncated at spawn, and a wedged child is killed and reaped instead of
// left as a zombie - all with disposable paths.

// TestWakeSpawnHelperProcess is the spawned child, not a real test.
// PUNK_WAKE_HELPER_MODE=sleep wedges it without reading stdin.
func TestWakeSpawnHelperProcess(t *testing.T) {
	if os.Getenv("PUNK_WAKE_HELPER") != "1" {
		return
	}
	if os.Getenv("PUNK_WAKE_HELPER_MODE") == "sleep" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("PUNK_WAKE_HELPER_OUT"), raw, 0o600); err != nil {
		os.Exit(3)
	}
	if cwd, err := os.Getwd(); err == nil {
		_ = os.WriteFile(os.Getenv("PUNK_WAKE_HELPER_OUT")+".cwd", []byte(cwd), 0o600)
	}
	os.Exit(0)
}

func TestWakeSpawnProcessDetached(t *testing.T) {
	wakeTestEnv(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "got-config.json")
	t.Setenv("PUNK_WAKE_HELPER", "1")
	t.Setenv("PUNK_WAKE_HELPER_OUT", out)
	t.Setenv("PUNK_WAKE_HELPER_MODE", "")

	config := []byte(`{"client":"claude-code","token":"secret-token-w4-test"}`)
	logPath := filepath.Join(dir, wakeWorkerLog)
	// A stale log from a previous generation is truncated at spawn.
	if err := os.WriteFile(logPath, []byte("stale content from an older worker"), 0o600); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	start := time.Now()
	pid, err := wakeSpawnProcess(
		[]string{os.Args[0], "-test.run=TestWakeSpawnHelperProcess"}, config, logPath, workDir)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("spawn returned pid %d", pid)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("spawn blocked on the child")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, rerr := os.ReadFile(out)
		if rerr == nil {
			if !bytes.Equal(raw, config) {
				t.Fatalf("config changed in transit: %q", raw)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper never received the config")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cwd, err := os.ReadFile(out + ".cwd"); err != nil || string(cwd) != workDir {
		t.Fatalf("child cwd %q (%v), want %q", cwd, err, workDir)
	}
	if st, err := os.Stat(logPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("worker log %v mode %v", err, st)
	}
	if raw, _ := os.ReadFile(logPath); strings.Contains(string(raw), "stale content") {
		t.Fatal("worker log was not truncated at spawn")
	}
}

func TestWakeSpawnProcessWedgedChildKilledAndReaped(t *testing.T) {
	wakeTestEnv(t)
	t.Setenv("PUNK_WAKE_HELPER", "1")
	t.Setenv("PUNK_WAKE_HELPER_MODE", "sleep")
	t.Setenv("PUNK_WAKE_HELPER_OUT", filepath.Join(t.TempDir(), "never"))

	// A child that never drains stdin wedges a config larger than the
	// pipe buffer: the write must time out, the child must be killed AND
	// reaped, and the error must be the generic pipe reason.
	old := wakeConfigWriteTimeout
	wakeConfigWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { wakeConfigWriteTimeout = old })
	config := bytes.Repeat([]byte("x"), 1<<20)
	start := time.Now()
	pid, err := wakeSpawnProcess(
		[]string{os.Args[0], "-test.run=TestWakeSpawnHelperProcess"},
		config, filepath.Join(t.TempDir(), wakeWorkerLog), t.TempDir())
	if err == nil {
		t.Fatalf("wedged child spawned as pid %d", pid)
	}
	if err != errWakeConfigPipe {
		t.Fatalf("generic pipe error expected, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("wedged spawn did not fail promptly")
	}
}

// The run child must wait boundedly for a superseded worker's state-dir
// lock (the old runner's marker poll needs a beat to fire), then start
// normally - never die on the transient lock, never double-run.
func TestWakeRunRetriesStateLockAfterReplacement(t *testing.T) {
	wakeTestEnv(t)
	const ns = "ns-lock"
	srv := wakeRunFixture(t, ns, nil)

	dir := t.TempDir()
	marker := filepath.Join(dir, wakeControlFile)
	if err := os.WriteFile(marker, []byte("gen-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The superseded worker still holds the runner's lifetime flock in
	// the per-identity ledger dir (StateDir/wake-<hash>/nativewake.lock);
	// release it after a beat, as its marker watch would.
	identity := srv.URL + "\n" + ns + "\n" + "claude-code:sess-1"
	sum := sha256.Sum256([]byte(identity))
	lockDir := filepath.Join(dir, "wake-"+hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, "nativewake.lock")
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Skipf("flock unavailable: %v", err)
	}
	defer func() { _ = lf.Close() }()

	fake := &fakeWakeTransport{}
	cc := wakeChildConfig{Config: nativewake.Config{
		Client: "claude-code", SessionID: "sess-1", Namespace: ns, URL: srv.URL,
		StateDir: dir, ControlPath: marker, Generation: "gen-2", MaxWakes: -1,
	}}
	raw, _ := json.Marshal(cc)
	old := wakeNewTransport
	wakeNewTransport = func(wakeChildConfig) (nativewake.Transport, error) { return fake, nil }
	t.Cleanup(func() { wakeNewTransport = old })

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Wake(wakeOpts("claude-code", wakeActionRun, ns), strings.NewReader(string(raw)), io.Discard)
	}()
	time.Sleep(400 * time.Millisecond) // still locked: run must be retrying
	if fake.probes.Load() != 0 {
		t.Fatal("worker probed before the state lock was released")
	}
	_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	deadline := time.Now().Add(10 * time.Second)
	for fake.wakes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.wakes.Load() != 1 {
		t.Fatalf("worker never started after lock release (wakes=%d)", fake.wakes.Load())
	}
	_ = os.Remove(marker)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not exit after marker removal")
	}
}
