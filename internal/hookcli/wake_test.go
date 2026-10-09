package hookcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/nativewake"
)

// W4 native wake lifecycle tests (2026-09-28 plan + planner review):
// ensure/stop/run semantics, random-generation singleton with
// fingerprint dedup, namespace replacement, stop+ensure resurrection
// safety, state-lock retry, disabled teardown, stale-stop endpoint
// guard, path-traversal-proof identity hashing, secret hygiene (pipe
// only, never argv or disk, credential URLs rejected), endpoint probes
// that start nothing, and the run worker's marker watch against a
// disposable HTTP/SSE server. All state roots, homes and endpoints are
// per-test temp dirs.

// wakeTestEnv isolates every environment input the lifecycle reads.
func wakeTestEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("PUNK_MESSAGING", "")
	t.Setenv("PUNK_NAMESPACE", "")
	t.Setenv("PUNK_MESSAGING_FROM", "")
	t.Setenv("PUNK_MESSAGING_MAX_CONTINUE", "")
	t.Setenv("PUNK_MESSAGING_CONTINUE_WINDOW_SECONDS", "")
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "")
}

// wakeStdin builds a Claude-shaped native hook payload.
func wakeStdin(sessionID, cwd string) io.Reader {
	return strings.NewReader(fmt.Sprintf(`{"session_id":%q,"cwd":%q,"hook_event_name":"SessionStart"}`,
		sessionID, cwd))
}

// claudeSocketFixture creates a real Unix socket so the endpoint probe's
// stat sees a socket node, and points the messaging env at it.
func claudeSocketFixture(t *testing.T) (socket, token string) {
	t.Helper()
	socket = filepath.Join(t.TempDir(), "cc.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	token = "secret-token-w4-test"
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket)
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", token)
	return socket, token
}

// spawnRecorder replaces wakeSpawn and records every call.
type spawnRecorder struct {
	mu   sync.Mutex
	argv [][]string
	cfg  [][]byte
	pid  int
	fail error
}

func (r *spawnRecorder) install(t *testing.T) {
	t.Helper()
	r.pid = os.Getpid() // alive, so a matching fingerprint suppresses respawn
	old := wakeSpawn
	wakeSpawn = func(argv []string, config []byte, logPath, workDir string) (int, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.fail != nil {
			return 0, r.fail
		}
		r.argv = append(r.argv, append([]string(nil), argv...))
		r.cfg = append(r.cfg, append([]byte(nil), config...))
		return r.pid, nil
	}
	t.Cleanup(func() { wakeSpawn = old })
}

func (r *spawnRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.argv)
}

func (r *spawnRecorder) last(t *testing.T) ([]string, wakeChildConfig) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.argv) == 0 {
		t.Fatal("worker was never spawned")
	}
	var cc wakeChildConfig
	if err := json.Unmarshal(r.cfg[len(r.cfg)-1], &cc); err != nil {
		t.Fatalf("spawned config does not decode: %v", err)
	}
	return r.argv[len(r.argv)-1], cc
}

func wakeOpts(client, action, ns string) WakeOpts {
	return WakeOpts{Client: client, Action: action, BaseURL: "http://127.0.0.1:19797",
		APIKey: "secret-key-w4-test", Namespace: ns, Enabled: true}
}

func wakeRunErr(t *testing.T, opts WakeOpts, stdin io.Reader) string {
	t.Helper()
	var errw strings.Builder
	if err := Wake(opts, stdin, &errw); err != nil {
		t.Fatalf("Wake must always fail open, got %v", err)
	}
	return errw.String()
}

func TestWakeEnsureSpawnsDetachedWorker(t *testing.T) {
	wakeTestEnv(t)
	socket, token := claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	opts := wakeOpts("claude-code", wakeActionEnsure, "ns-a")
	wakeRunErr(t, opts, wakeStdin("sess-1", "/work/x"))

	argv, cc := rec.last(t)
	if got := argv[1:]; strings.Join(got, " ") != "hook wake --client claude-code --action run --url http://127.0.0.1:19797 --ns ns-a --messaging" {
		t.Fatalf("worker argv %v", argv)
	}
	if cc.Endpoint != socket || cc.Token != token || cc.APIKey != "secret-key-w4-test" {
		t.Fatalf("config lost endpoint/token/key: %+v", cc)
	}
	if cc.Client != "claude-code" || cc.SessionID != "sess-1" || cc.Namespace != "ns-a" {
		t.Fatalf("config identity: %+v", cc.Config)
	}
	dir := wakeSessionDir("claude-code", "sess-1")
	if cc.StateDir != dir {
		t.Fatalf("config state dir %q, identity dir %q", cc.StateDir, dir)
	}
	// The control marker lives in the generation's own directory and
	// holds exactly the raw generation text plus one newline.
	if cc.ControlPath != filepath.Join(wakeGenerationDir(dir, cc.Generation), wakeControlFile) {
		t.Fatalf("control path %q not generation-scoped under %s", cc.ControlPath, dir)
	}
	raw, err := os.ReadFile(cc.ControlPath)
	if err != nil {
		t.Fatal("control marker missing after ensure")
	}
	if string(raw) != cc.Generation+"\n" {
		t.Fatalf("marker %q, generation %q", raw, cc.Generation)
	}
	if len(cc.Generation) != 32 {
		t.Fatalf("generation %q is not 16 random bytes hex", cc.Generation)
	}
	cur, err := os.ReadFile(filepath.Join(dir, wakeCurrentFile))
	if err != nil || string(cur) != cc.Generation {
		t.Fatalf("current pointer %q (%v)", cur, err)
	}
	listener, err := os.ReadFile(filepath.Join(dir, wakeListenerFile))
	if err != nil {
		t.Fatal("listener record missing after ensure")
	}
	var recd wakeListenerRecord
	if err := json.Unmarshal(listener, &recd); err != nil || recd.PID != rec.pid ||
		recd.Generation != cc.Generation || recd.Namespace != "ns-a" ||
		recd.Endpoint != socket || recd.URL != opts.BaseURL || recd.Fingerprint == "" {
		t.Fatalf("listener record %s (%v)", listener, err)
	}
}

func TestWakeEnsureMaxWakesSentinel(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	// Unset env: the config must carry the negative sentinel so the
	// runner applies its default of 5 (0 would disable all wakes).
	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	_, cc := rec.last(t)
	if cc.MaxWakes != -1 {
		t.Fatalf("unset env must produce the -1 default sentinel, got %d", cc.MaxWakes)
	}

	// Explicit 0 is preserved as "wakes disabled".
	t.Setenv("PUNK_MESSAGING_MAX_CONTINUE", "0")
	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-2", "/work/x"))
	_, cc = rec.last(t)
	if cc.MaxWakes != 0 {
		t.Fatalf("explicit PUNK_MESSAGING_MAX_CONTINUE=0 must survive, got %d", cc.MaxWakes)
	}
}

func TestWakeEnsureSecretsOnlyInPipe(t *testing.T) {
	wakeTestEnv(t)
	_, token := claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	argv, _ := rec.last(t)
	for _, a := range argv {
		if strings.Contains(a, token) || strings.Contains(a, "secret-key-w4-test") {
			t.Fatalf("secret leaked into argv: %v", argv)
		}
	}
	// Nothing under the state root may contain either secret.
	root := wakeStateRoot()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(raw), token) || strings.Contains(string(raw), "secret-key-w4-test") {
			t.Errorf("secret persisted in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWakeEnsureRejectsCredentialURL(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	opts := wakeOpts("claude-code", wakeActionEnsure, "ns-a")
	opts.BaseURL = "http://user:secret-key-w4-test@127.0.0.1:19797"
	log := wakeRunErr(t, opts, wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 0 {
		t.Fatal("credential-bearing URL spawned a worker")
	}
	if !strings.Contains(log, "credentials") || strings.Contains(log, "secret-key-w4-test") {
		t.Fatalf("concise secret-free rejection expected, got %q", log)
	}
}

func TestWakeEnsureDuplicateIsNoOp(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 1 {
		t.Fatalf("duplicate ensure spawned %d workers", rec.calls())
	}
}

// wakeRewriteListener edits the on-disk listener record in place.
func wakeRewriteListener(t *testing.T, dir string, mutate func(*wakeListenerRecord)) {
	t.Helper()
	rec, ok := wakeReadListener(dir)
	if !ok {
		t.Fatal("no listener record to rewrite")
	}
	mutate(&rec)
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(dir, wakeListenerFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A live PID alone past the startup grace window is NOT proof the worker
// runs: the number may have been reused by a stranger. With no worker
// lock held, ensure must restart the listener.
func TestWakeEnsureUnrelatedPIDTriggersRestart(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{} // pid = this test process: alive but unrelated
	rec.install(t)

	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	_, first := rec.last(t)
	dir := wakeSessionDir("claude-code", "sess-1")
	// Age the record past the grace window; the marker stays current.
	wakeRewriteListener(t, dir, func(r *wakeListenerRecord) {
		r.StartedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	})
	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 2 {
		t.Fatalf("stale PID-only record must not block respawn, got %d spawns", rec.calls())
	}
	_, second := rec.last(t)
	if first.Generation == second.Generation {
		t.Fatal("restart minted the same generation")
	}
}

// The worker's lifetime flock is the durable process proof: with the
// lock held, even a dead PID and an ancient record dedup to a no-op;
// once released, the same record restarts.
func TestWakeEnsureWorkerLockProvesRunning(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	_, first := rec.last(t)
	dir := wakeSessionDir("claude-code", "sess-1")
	wakeRewriteListener(t, dir, func(r *wakeListenerRecord) {
		r.PID = 1 << 22 // dead
		r.StartedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	})
	unlock, err := lockInboxFile(filepath.Join(wakeGenerationDir(dir, first.Generation), wakeWorkerLock), wakeLockTimeout)
	if err != nil {
		t.Fatalf("test could not take the worker lock: %v", err)
	}
	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 1 {
		t.Fatal("held worker lock must dedup even with a dead recorded PID")
	}
	unlock()
	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 2 {
		t.Fatal("released worker lock with dead PID must restart")
	}
}

// A failed replacement spawn rolls back to the previous listener when
// that worker is still provably running, and stays honestly empty when
// it is not.
func TestWakeEnsureSpawnFailureRollback(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	_, first := rec.last(t)
	dir := wakeSessionDir("claude-code", "sess-1")

	// Replacement spawn fails while the old worker is still "running"
	// (fresh record, live PID inside the grace window): roll back.
	rec.fail = errWakeUnsupportedForTest
	log := wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-b"), wakeStdin("sess-1", "/work/x"))
	if !strings.Contains(log, "previous listener kept") {
		t.Fatalf("rollback note %q", log)
	}
	if !wakeMarkerCurrent(dir, first.Generation) {
		t.Fatal("rollback did not restore the previous generation marker")
	}
	rec.fail = nil

	// Same failure with the old worker NOT provably running: honest gap.
	wakeRewriteListener(t, dir, func(r *wakeListenerRecord) {
		r.PID = 1 << 22
		r.StartedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	})
	rec.fail = errWakeUnsupportedForTest
	log = wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-b"), wakeStdin("sess-1", "/work/x"))
	if strings.Contains(log, "kept") {
		t.Fatalf("no rollback expected for a dead worker, got %q", log)
	}
	if _, err := os.Stat(filepath.Join(dir, wakeCurrentFile)); !os.IsNotExist(err) {
		t.Fatal("failed replacement with dead predecessor left a marker")
	}
}

func TestWakeEnsureNamespaceReplacement(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	_, first := rec.last(t)
	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-b"), wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 2 {
		t.Fatalf("namespace change must respawn, got %d spawns", rec.calls())
	}
	argv, second := rec.last(t)
	if first.Generation == second.Generation {
		t.Fatal("namespace change must mint a fresh generation")
	}
	if second.Namespace != "ns-b" || !strings.Contains(strings.Join(argv, " "), "--ns ns-b") {
		t.Fatalf("replacement config/argv still on old namespace: %v %+v", argv, second.Config)
	}
	// The old generation's marker is gone (its worker cancels itself),
	// the new one is current.
	if _, err := os.Stat(first.ControlPath); !os.IsNotExist(err) {
		t.Fatal("old generation marker survived replacement")
	}
	if !wakeMarkerCurrent(wakeSessionDir("claude-code", "sess-1"), second.Generation) {
		t.Fatal("new generation marker not current")
	}
}

func TestWakeStopEnsureDoesNotResurrect(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	_, first := rec.last(t)
	wakeRunErr(t, wakeOpts("claude-code", wakeActionStop, "ns-a"), wakeStdin("sess-1", "/work/x"))
	// A quick ensure after stop must spawn a NEW worker with a FRESH
	// generation: the cancelled first worker can never see its marker
	// text reappear and keep running.
	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 2 {
		t.Fatalf("stop+ensure produced %d spawns", rec.calls())
	}
	_, second := rec.last(t)
	if first.Generation == second.Generation {
		t.Fatal("ensure after stop reproduced the cancelled generation")
	}
	if _, err := os.Stat(first.ControlPath); !os.IsNotExist(err) {
		t.Fatal("cancelled generation marker resurrected")
	}
	if !wakeMarkerCurrent(wakeSessionDir("claude-code", "sess-1"), second.Generation) {
		t.Fatal("post-stop generation not current")
	}
}

func TestWakeEnsureDisabledTearsDown(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 1 {
		t.Fatal("setup ensure did not spawn")
	}
	dir := wakeSessionDir("claude-code", "sess-1")

	// PUNK_MESSAGING=0 wins over --messaging and tears the listener down.
	t.Setenv("PUNK_MESSAGING", "0")
	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 1 {
		t.Fatal("disabled ensure spawned")
	}
	if _, err := os.Stat(filepath.Join(dir, wakeCurrentFile)); !os.IsNotExist(err) {
		t.Fatal("disabled ensure left the current pointer")
	}
	if _, err := os.Stat(filepath.Join(dir, wakeListenerFile)); !os.IsNotExist(err) {
		t.Fatal("disabled ensure left the listener record")
	}

	// A disabled ensure with no prior listener stays fully inert.
	opts := wakeOpts("claude-code", wakeActionEnsure, "ns-a")
	opts.Enabled = false
	t.Setenv("PUNK_MESSAGING", "")
	wakeRunErr(t, opts, wakeStdin("sess-2", "/work/x"))
	if rec.calls() != 1 {
		t.Fatal("ensure without opt-in spawned")
	}
}

func TestWakeEnsureUnavailableEndpoint(t *testing.T) {
	wakeTestEnv(t)
	rec := &spawnRecorder{}
	rec.install(t)
	// No CLAUDE_CODE_MESSAGING_SOCKET: capability missing.

	log := wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 0 {
		t.Fatal("unavailable endpoint spawned a worker")
	}
	if !strings.Contains(log, "unavailable") || strings.Contains(log, "secret") {
		t.Fatalf("concise secret-free diagnostic expected, got %q", log)
	}
	if _, err := os.Stat(filepath.Join(wakeSessionDir("claude-code", "sess-1"), wakeCurrentFile)); !os.IsNotExist(err) {
		t.Fatal("unavailable endpoint left a marker")
	}
}

func TestWakeEnsureStaleSocketTearsDown(t *testing.T) {
	wakeTestEnv(t)
	socket, _ := claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 1 {
		t.Fatal("setup ensure did not spawn")
	}
	// Socket vanished: the ensured listener's endpoint is gone.
	_ = os.Remove(socket)
	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	dir := wakeSessionDir("claude-code", "sess-1")
	if _, err := os.Stat(filepath.Join(dir, wakeCurrentFile)); !os.IsNotExist(err) {
		t.Fatal("vanished endpoint must tear the stale listener down")
	}
}

func TestWakeEnsureSpawnFailureLeavesNoMarker(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{fail: errWakeUnsupportedForTest}
	rec.install(t)

	log := wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	if !strings.Contains(log, "listener failed to start") {
		t.Fatalf("spawn failure must be reported generically, got %q", log)
	}
	dir := wakeSessionDir("claude-code", "sess-1")
	if _, err := os.Stat(filepath.Join(dir, wakeCurrentFile)); !os.IsNotExist(err) {
		t.Fatal("failed spawn left a marker behind")
	}
}

var errWakeUnsupportedForTest = fmt.Errorf("spawn unsupported in test")

func TestWakeStopRemovesMarkerNeverKills(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	dir := wakeSessionDir("claude-code", "sess-1")
	wakeRunErr(t, wakeOpts("claude-code", wakeActionStop, "ns-a"), wakeStdin("sess-1", "/work/x"))
	for _, f := range []string{wakeCurrentFile, wakeListenerFile, wakeGenDir} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Fatalf("stop left %s", f)
		}
	}
	// Stop is idempotent and quiet on a session with nothing running.
	wakeRunErr(t, wakeOpts("claude-code", wakeActionStop, "ns-a"), wakeStdin("sess-1", "/work/x"))
	// A garbage payload fails open with a note.
	log := wakeRunErr(t, wakeOpts("claude-code", wakeActionStop, "ns-a"), strings.NewReader("not json"))
	if !strings.Contains(log, "nothing to stop") {
		t.Fatalf("stop note %q", log)
	}
}

func TestWakeStopStaleEndpointRefuses(t *testing.T) {
	wakeTestEnv(t)
	socket, _ := claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	dir := wakeSessionDir("claude-code", "sess-1")

	// A stale SessionEnd from a DIFFERENT session process (its hook env
	// carries a different own-session socket) must not remove the new
	// listener's generation.
	other, _ := claudeSocketFixture(t)
	log := wakeRunErr(t, wakeOpts("claude-code", wakeActionStop, "ns-a"), wakeStdin("sess-1", "/work/x"))
	if !strings.Contains(log, "endpoint differs") {
		t.Fatalf("stale stop note %q", log)
	}
	if _, err := os.Stat(filepath.Join(dir, wakeCurrentFile)); err != nil {
		t.Fatal("stale stop removed the new listener")
	}

	// The owning session's own stop (same endpoint) tears it down.
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket)
	wakeRunErr(t, wakeOpts("claude-code", wakeActionStop, "ns-a"), wakeStdin("sess-1", "/work/x"))
	if _, err := os.Stat(filepath.Join(dir, wakeCurrentFile)); !os.IsNotExist(err) {
		t.Fatal("owning stop did not tear down")
	}
	_ = other
}

func TestWakePathTokenHashesEverything(t *testing.T) {
	// Separator folding must not collide: "a:b" and "a_b" hash their
	// originals, not a sanitised form.
	if wakePathToken("a:b") == wakePathToken("a_b") {
		t.Fatal("distinct ids collapsed to one path token")
	}
	// Traversal is impossible by construction.
	for _, hostile := range []string{"..", "../..", "/etc", "a/b", `\x`, "s\x00id"} {
		tok := wakePathToken(hostile)
		if strings.ContainsAny(tok, "/\\") || tok == ".." || strings.ContainsRune(tok, 0) {
			t.Fatalf("path token %q for %q escapes", tok, hostile)
		}
	}
	root := wakeStateRoot()
	dir := wakeSessionDir("claude-code", "../../../etc")
	if !strings.HasPrefix(dir, root+string(os.PathSeparator)) {
		t.Fatalf("session dir %q escapes root %q", dir, root)
	}
}

func TestWakeStateRootRejectsRelativeXDG(t *testing.T) {
	wakeTestEnv(t)
	t.Setenv("XDG_STATE_HOME", "relative/state")
	root := wakeStateRoot()
	if !filepath.IsAbs(root) {
		t.Fatalf("relative XDG_STATE_HOME produced relative root %q", root)
	}
	if strings.Contains(root, "relative/state") {
		t.Fatalf("relative XDG_STATE_HOME was used: %q", root)
	}
}

func TestWakeCodexEndpointProbeStartsNoDaemon(t *testing.T) {
	wakeTestEnv(t)
	rec := &spawnRecorder{}
	rec.install(t)

	// Fake codex binary on PATH; no control socket yet.
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, "codex")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	log := wakeRunErr(t, wakeOpts("codex", wakeActionEnsure, "ns-a"), wakeStdin("thread-1", "/work/x"))
	if rec.calls() != 0 || !strings.Contains(log, "control socket") {
		t.Fatalf("missing daemon socket must fail open without spawning: calls=%d log=%q", rec.calls(), log)
	}

	// Socket appears: ensure resolves binary+socket and spawns.
	sock := codexControlSocket()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wakeRunErr(t, wakeOpts("codex", wakeActionEnsure, "ns-a"), wakeStdin("thread-1", "/work/x"))
	argv, cc := rec.last(t)
	if cc.Binary != exe || cc.Endpoint != sock || cc.SessionID != "thread-1" {
		t.Fatalf("codex config %+v", cc)
	}
	if got := strings.Join(argv[1:], " "); !strings.Contains(got, "--client codex --action run") {
		t.Fatalf("codex worker argv %v", argv)
	}
}

func TestWakeUnknownClientAndActionFailOpen(t *testing.T) {
	wakeTestEnv(t)
	if log := wakeRunErr(t, wakeOpts("cursor", wakeActionEnsure, "ns-a"), wakeStdin("s", "/w")); !strings.Contains(log, "unsupported client") {
		t.Fatalf("client note %q", log)
	}
	if log := wakeRunErr(t, wakeOpts("claude-code", "dance", "ns-a"), wakeStdin("s", "/w")); !strings.Contains(log, "unknown --action") {
		t.Fatalf("action note %q", log)
	}
}

// closeTracker proves run closes stdin after reading the config.
type closeTracker struct {
	io.Reader
	closed *atomic.Bool
}

func (c closeTracker) Close() error { c.closed.Store(true); return nil }

func TestWakeRunRejectsBadConfigAndClosesStdin(t *testing.T) {
	wakeTestEnv(t)
	closed := &atomic.Bool{}
	log := wakeRunErr(t, wakeOpts("claude-code", wakeActionRun, "ns-a"),
		closeTracker{strings.NewReader("not json"), closed})
	if !closed.Load() {
		t.Fatal("run did not close stdin")
	}
	if !strings.Contains(log, "bad config") {
		t.Fatalf("config note %q", log)
	}
}

func TestWakeRunWithoutMarkerRefuses(t *testing.T) {
	wakeTestEnv(t)
	cc := wakeChildConfig{Config: nativewake.Config{
		Client: "claude-code", SessionID: "sess-1", Namespace: "ns-a",
		URL: "http://127.0.0.1:19797", StateDir: t.TempDir(),
		ControlPath: filepath.Join(t.TempDir(), wakeControlFile), Generation: "gen",
	}}
	raw, _ := json.Marshal(cc)
	called := &atomic.Bool{}
	old := wakeNewTransport
	wakeNewTransport = func(wakeChildConfig) (nativewake.Transport, error) {
		called.Store(true)
		return nil, fmt.Errorf("must not be constructed")
	}
	t.Cleanup(func() { wakeNewTransport = old })
	log := wakeRunErr(t, wakeOpts("claude-code", wakeActionRun, "ns-a"), strings.NewReader(string(raw)))
	if called.Load() {
		t.Fatal("transport constructed despite missing marker")
	}
	if !strings.Contains(log, "superseded") {
		t.Fatalf("marker note %q", log)
	}
}

// fakeWakeTransport records Probe/Wake calls for the run-worker tests.
type fakeWakeTransport struct {
	probes   atomic.Int32
	wakes    atomic.Int32
	lastText atomic.Value
}

func (f *fakeWakeTransport) Probe(context.Context) error { f.probes.Add(1); return nil }
func (f *fakeWakeTransport) Close() error                { return nil }
func (f *fakeWakeTransport) Wake(_ context.Context, text string) (nativewake.Outcome, error) {
	f.wakes.Add(1)
	f.lastText.Store(text)
	return nativewake.Outcome{Accepted: true}, nil
}

// wakeRunFixture is a disposable Punk HTTP/SSE server for run tests.
func wakeRunFixture(t *testing.T, ns string, diagHits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "/v1/namespaces/" + ns
		switch r.URL.Path {
		case base + "/members":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
		case base + "/messages/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done() // hold the stream until the run cancels
		case base + "/messages":
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{
				{"id": "m1", "sender": "peer:alpha", "created_at": "2026-09-28T00:00:00Z"},
			}})
		case base + "/messages/count":
			_ = json.NewEncoder(w).Encode(map[string]int{"unread": 1})
		case base + "/messages/diagnostics":
			if diagHits != nil {
				diagHits.Add(1)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWakeRunWorkerLifecycle(t *testing.T) {
	wakeTestEnv(t)
	const ns = "ns-run"
	var diagHits atomic.Int32
	srv := wakeRunFixture(t, ns, &diagHits)

	dir := t.TempDir()
	marker := filepath.Join(dir, wakeControlFile)
	fake := &fakeWakeTransport{}
	cc := wakeChildConfig{Config: nativewake.Config{
		Client: "claude-code", SessionID: "sess-1", Namespace: ns, URL: srv.URL,
		APIKey: "secret-key-w4-test", StateDir: dir, ControlPath: marker, Generation: "gen-1",
		MaxWakes: -1, // sentinel: runner default budget
	}, Endpoint: "sock-ep", Token: "secret-token-w4-test"}
	raw, _ := json.Marshal(cc)
	old := wakeNewTransport
	wakeNewTransport = func(got wakeChildConfig) (nativewake.Transport, error) {
		if got.Endpoint != "sock-ep" || got.Token != "secret-token-w4-test" || got.APIKey != "secret-key-w4-test" {
			t.Errorf("config did not survive the pipe: %+v", got)
		}
		return fake, nil
	}
	t.Cleanup(func() { wakeNewTransport = old })

	// Marker in place before run starts, as ensure leaves it.
	if err := os.WriteFile(marker, []byte("gen-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var errw strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := Wake(wakeOpts("claude-code", wakeActionRun, ns), strings.NewReader(string(raw)), &errw); err != nil {
			t.Errorf("Wake: %v", err)
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for fake.wakes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.wakes.Load() != 1 {
		t.Fatalf("expected exactly one wake, got %d", fake.wakes.Load())
	}
	text, _ := fake.lastText.Load().(string)
	if !strings.Contains(text, ns) || !strings.Contains(text, "claude-code:sess-1") {
		t.Fatalf("nudge carries namespace and address only: %q", text)
	}
	if strings.Contains(text, "secret") {
		t.Fatalf("nudge leaked a secret: %q", text)
	}
	// Marker removal is the stop path: the worker ends itself.
	_ = os.Remove(marker)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not exit after marker removal")
	}
	if diagHits.Load() == 0 {
		t.Fatal("no diagnostic observation reached the server")
	}
	// The run held its generation worker lock for its whole lifetime and
	// released it at exit.
	if _, err := lockInboxFile(filepath.Join(dir, wakeWorkerLock), wakeLockTimeout); err != nil {
		t.Fatalf("worker lock still held after exit: %v", err)
	}
	// Child diagnostics are fixed generic strings: no URLs, no paths.
	if strings.Contains(errw.String(), "http") || strings.Contains(errw.String(), dir) {
		t.Fatalf("child log leaked detail: %q", errw.String())
	}
}

func TestWakeListenerStatusAndGuidance(t *testing.T) {
	wakeTestEnv(t)
	socket, _ := claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)

	// Nothing ensured: the default guidance states there is no idle wake.
	if requested, capable := WakeListenerStatus("claude-code", "sess-1"); requested || capable {
		t.Fatal("status without a listener must be false/false")
	}
	g := inboxRoutingGuidance("ns-a", "claude-code:sess-1", false, false)
	if !strings.Contains(g, "There is no idle wake") {
		t.Fatalf("default guidance changed: %q", g)
	}

	// Listener ensured: guidance says requested + capable, never "live".
	wakeRunErr(t, wakeOpts("claude-code", wakeActionEnsure, "ns-a"), wakeStdin("sess-1", "/work/x"))
	requested, capable := WakeListenerStatus("claude-code", "sess-1")
	if !requested || !capable {
		t.Fatalf("ensured listener status %v/%v", requested, capable)
	}
	g = inboxRoutingGuidance("ns-a", "claude-code:sess-1", requested, capable)
	if !strings.Contains(g, "capability note, not a liveness guarantee") {
		t.Fatalf("requested+capable guidance %q", g)
	}

	// Endpoint gone: capability drops, the request stays visible.
	_ = os.Remove(socket)
	requested, capable = WakeListenerStatus("claude-code", "sess-1")
	if !requested || capable {
		t.Fatalf("stale-endpoint status %v/%v", requested, capable)
	}
	g = inboxRoutingGuidance("ns-a", "claude-code:sess-1", requested, capable)
	if !strings.Contains(g, "endpoint is currently unavailable") {
		t.Fatalf("requested+unavailable guidance %q", g)
	}
}
