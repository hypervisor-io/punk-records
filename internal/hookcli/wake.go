package hookcli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/nativewake"
)

// wake.go is the lifecycle half of the opt-in native wake bridge
// (2026-09-28 native-wake plan, W4): "punk hook wake --action ensure"
// rechecks runtime availability and guarantees ONE detached listener
// per native session, "--action stop" (SessionEnd) tears it down, and
// "--action run" is the listener itself. The generated hook entries
// (wake_connect.go, W5) only ever invoke ensure and stop; run exists
// solely as the detached child ensure spawns.
//
// Contracts this file keeps:
//   - Fail open, always: Wake returns nil for every failure, prints
//     nothing to stdout (there is not even an out parameter), and logs
//     one concise secret-free line to stderr. A dead server, a missing
//     native endpoint or an unsupported platform must never block or
//     break the host session. Diagnostics from the run child are fixed
//     generic strings: a wrapped error can embed the server URL or a
//     socket path, and the child log must never carry either.
//   - Secrets move by pipe only: the native token and the Punk API key
//     travel inside the private config JSON ensure writes into the run
//     child's stdin. They never appear in argv (a credential-bearing
//     server URL is rejected outright), persisted lifecycle files,
//     diagnostics or log output.
//   - Singleton by random generation: every spawn mints a fresh
//     crypto-random generation and its OWN generation directory
//     (gen/<generation>/control) under the shared identity state dir, so
//     a stop+ensure pair can never resurrect a cancelled worker by
//     rewriting the identical marker text. Dedup uses a separate config
//     fingerprint: an unchanged configuration with a live worker is a
//     no-op. The runner's budget file and lifetime lock stay in the
//     identity dir (nativewake owns both), so a namespace or endpoint
//     replacement cancels the old worker through its marker and the new
//     run child retries the state lock boundedly until the old runner
//     releases it.
//   - Stop never kills: stop removes the current generation's marker and
//     the listener record under the same lifecycle lock ensure holds,
//     and lets the worker's own marker watch end it. No PID is ever
//     signalled by number, so PID reuse cannot kill a stranger. A stale
//     SessionEnd from a different session process (or server) is
//     refused through the recorded endpoint/URL identity - see wakeStop.
//   - Detachment: the worker is spawned in its own session/process
//     group with stdin as a short-lived pipe (closed right after the
//     config write, kill+reap on any pipe failure), stdout/stderr
//     redirected to a per-generation log truncated at spawn, and the
//     child's working directory set to the private state dir so a Codex
//     proxy subprocess never inherits (or writes into) the user's
//     workspace or home. Platforms without Unix detachment fail open
//     explicitly.

// WakeOpts configures one punk hook wake invocation.
type WakeOpts struct {
	Client    string // claude-code | codex
	Action    string // ensure | stop | run
	BaseURL   string
	APIKey    string
	Namespace string // --ns; else PUNK_NAMESPACE; else server binding for the session address; else server lookup by cwd
	Enabled   bool   // --messaging was written into the hook entry
}

// wake actions.
const (
	wakeActionEnsure = "ensure"
	wakeActionStop   = "stop"
	wakeActionRun    = "run"
)

const (
	// wakeLockTimeout bounds the per-session lifecycle lock; a wedged
	// peer must never hang a native hook.
	wakeLockTimeout = 2 * time.Second
	// wakeMaxConfigBytes bounds the private config JSON run reads.
	wakeMaxConfigBytes = 64 << 10
	// wakeLockRetryWindow bounds how long the run child waits for a
	// superseded worker's state-dir lock; the old runner's marker poll
	// plus shutdown takes seconds at most.
	wakeLockRetryWindow  = 12 * time.Second
	wakeLockRetryPolling = 250 * time.Millisecond
)

// wakeConfigWriteTimeout bounds the config pipe write into a freshly
// spawned worker; a var so tests can shorten it.
var wakeConfigWriteTimeout = 5 * time.Second

// wake lifecycle files inside one session's identity state dir. The
// control marker lives in a per-generation subdirectory; the identity
// dir itself holds the lifecycle lock, the current-generation pointer,
// the listener record and the runner's budget/lock files. None of them
// ever stores a token or key.
const (
	wakeCurrentFile  = "current"       // raw text of the live generation
	wakeListenerFile = "listener.json" // wakeListenerRecord
	wakeLockFile     = "lock"          // lifecycle flock
	wakeGenDir       = "gen"           // <generation>/control + worker.log
	wakeControlFile  = "control"
	wakeWorkerLog    = "worker.log"
	// wakeWorkerLock is the flock the run child holds inside its
	// generation dir for its whole lifetime: process proof that cannot
	// be faked by PID reuse.
	wakeWorkerLock = "worker.lock"
	// wakeLegacyControl is the pre-generation-dir marker layout; removed
	// on sight so a worker written by an older build still stops.
	wakeLegacyControl = "control"
)

const (
	// wakeStartupGrace is the only window in which a live PID alone
	// (worker lock not yet acquired) counts as a running worker.
	wakeStartupGrace = 15 * time.Second
	// wakeLockProbeTimeout bounds a worker-lock liveness probe.
	wakeLockProbeTimeout = 200 * time.Millisecond
)

// wakeChildConfig is the private JSON document ensure pipes into the run
// child's stdin. Endpoint/Token/Binary are transport-specific: the
// Claude socket path and messaging token, or the codex binary and
// control socket. The embedded nativewake.Config carries everything the
// runner needs, including APIKey - private, never persisted by either
// side.
type wakeChildConfig struct {
	nativewake.Config
	Endpoint string `json:"endpoint,omitempty"`
	Token    string `json:"token,omitempty"`
	Binary   string `json:"binary,omitempty"`
}

// wakeListenerRecord is the observable lifecycle file (no secrets). The
// fingerprint dedups unchanged ensure runs; endpoint and URL let a stale
// SessionEnd recognise it is looking at a different session process or
// server before it tears anything down.
type wakeListenerRecord struct {
	PID         int    `json:"pid"`
	Generation  string `json:"generation"`
	Fingerprint string `json:"fingerprint"`
	Endpoint    string `json:"endpoint,omitempty"`
	Namespace   string `json:"namespace"`
	URL         string `json:"url"`
	StartedAt   string `json:"started_at"`
}

// wakeWorkerCommand builds the detached worker argv. It is a variable so
// tests can point the spawn at a helper process. No secret is ever an
// argument: endpoint, token and API key ride the stdin config only, and
// ensure has already rejected a credential-bearing URL.
var wakeWorkerCommand = func(cc wakeChildConfig) []string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "punk"
	}
	args := []string{exe, "hook", "wake", "--client", cc.Client, "--action", wakeActionRun, "--url", cc.URL}
	if cc.Namespace != "" {
		args = append(args, "--ns", cc.Namespace)
	}
	return append(args, "--messaging")
}

// wakeSpawn starts the detached worker; platform files provide the real
// implementation. A variable so tests can observe argv and the piped
// config instead of spawning. workDir becomes the child's working
// directory (the private identity state dir).
var wakeSpawn = wakeSpawnProcess

// wakeNewTransport builds the native transport for a run config; a
// variable so tests can substitute a fake without native endpoints.
var wakeNewTransport = func(cc wakeChildConfig) (nativewake.Transport, error) {
	switch cc.Client {
	case "claude-code":
		return nativewake.NewClaudeTransport(cc.Endpoint, cc.Token), nil
	case "codex":
		return nativewake.NewCodexTransport(cc.Binary, cc.Endpoint, cc.SessionID), nil
	}
	return nil, fmt.Errorf("no wake transport for client %q", cc.Client)
}

// Wake runs one punk hook wake invocation. It always returns nil: every
// failure is one concise stderr line and an inert hook.
func Wake(opts WakeOpts, stdin io.Reader, errw io.Writer) error {
	opts.BaseURL = strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	name := strings.ToLower(strings.TrimSpace(opts.Client))
	action := strings.ToLower(strings.TrimSpace(opts.Action))
	switch name {
	case "claude-code", "codex":
	default:
		wakeNote(errw, "unsupported client %q (wake is installed for claude-code and codex)", opts.Client)
		return nil
	}
	switch action {
	case wakeActionEnsure:
		wakeEnsure(name, opts, stdin, errw)
	case wakeActionStop:
		wakeStop(name, opts, stdin, errw)
	case wakeActionRun:
		wakeRun(name, opts, stdin, errw)
	default:
		wakeNote(errw, "unknown --action %q (want ensure, stop or run)", opts.Action)
	}
	return nil
}

func wakeNote(errw io.Writer, format string, args ...any) {
	fmt.Fprintf(errw, "punk hook wake: "+format+"\n", args...)
}

// ---- ensure / stop -----------------------------------------------------

// wakeSessionIdentity reads the native hook payload for the session id
// and cwd the lifecycle is scoped to.
func wakeSessionIdentity(client string, stdin io.Reader) (sessionID, cwd string, err error) {
	raw, err := io.ReadAll(io.LimitReader(stdin, maxStdinBytes))
	if err != nil {
		return "", "", fmt.Errorf("read stdin: %w", err)
	}
	p, err := parseInboxPayload(client, "", raw)
	if err != nil {
		return "", "", fmt.Errorf("bad payload: %w", err)
	}
	if !validAddressPart(p.SessionID) {
		return "", "", errors.New("payload carries no usable session id")
	}
	return p.SessionID, p.CWD, nil
}

// wakeEnsure rechecks runtime availability and ensures exactly one
// detached listener for this session: an unchanged configuration with a
// live worker is a no-op; any other outcome supersedes - cancel the old
// generation through its marker and spawn a fresh one. A missing or
// disabled capability tears the managed listener down instead of
// starting one.
func wakeEnsure(client string, opts WakeOpts, stdin io.Reader, errw io.Writer) {
	sessionID, cwd, err := wakeSessionIdentity(client, stdin)
	if err != nil {
		wakeNote(errw, "%v; no listener ensured", err)
		return
	}
	dir := wakeSessionDir(client, sessionID)
	if !inboxEnabled(opts.Enabled) {
		// Disabled (PUNK_MESSAGING=0 wins over --messaging): tear the
		// managed listener down under the same lock spawns take, and
		// stay quiet about why.
		wakeTeardownLocked(dir, errw)
		return
	}
	// A credential-bearing URL would put the secret in the worker's
	// argv; refuse the whole ensure instead.
	if u, perr := url.Parse(opts.BaseURL); perr != nil || u.User != nil {
		wakeNote(errw, "server URL must not carry credentials; no listener ensured")
		return
	}
	// Resolved fresh on every ensure with the session address on the
	// lookup: a moved inbox binding changes the namespace, the
	// fingerprint follows, and the old generation is superseded below.
	ns, err := resolveInboxNamespace(InboxOpts{Namespace: opts.Namespace, BaseURL: opts.BaseURL, APIKey: opts.APIKey}, cwd, client+":"+sessionID)
	if err != nil {
		wakeNote(errw, "namespace resolution failed; no listener ensured")
		return
	}
	endpoint, token, binary, err := wakeEndpoint(client)
	if err != nil {
		// Missing socket/thread capability: unavailable, fail open. Any
		// previously ensured listener for this session is stale (the
		// endpoint is gone), so tear it down too.
		wakeTeardownLocked(dir, errw)
		wakeNote(errw, "native wake unavailable for %s: %v", client, err)
		return
	}

	cc := wakeChildConfig{Config: nativewake.Config{
		Client:    client,
		SessionID: sessionID,
		Namespace: ns,
		URL:       opts.BaseURL,
		APIKey:    opts.APIKey,
		StateDir:  dir,
		// MaxWakes sentinel contract (nativewake withDefaults): negative
		// asks the runner for the shared default (5 per window), an
		// explicit 0 disables wakes (monitor and report only). The env
		// fallback must therefore be -1, never 0.
		MaxWakes:       envInt("PUNK_MESSAGING_MAX_CONTINUE", -1),
		Window:         time.Duration(envInt("PUNK_MESSAGING_CONTINUE_WINDOW_SECONDS", 0)) * time.Second,
		SenderPrefixes: inboxAllowlist(),
	}, Endpoint: endpoint, Token: token, Binary: binary}
	fingerprint := wakeFingerprint(cc)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		wakeNote(errw, "state dir unavailable; no listener ensured")
		return
	}
	unlock, err := lockInboxFile(filepath.Join(dir, wakeLockFile), wakeLockTimeout)
	if err != nil {
		wakeNote(errw, "state lock unavailable; no listener ensured")
		return
	}
	defer unlock()

	// Dedup on the config fingerprint, not the generation: same config
	// plus a provably running worker with a current marker is a no-op.
	// "Running" means the worker's lifetime lock is held; a bare live
	// PID counts only inside the startup grace window, so a reused PID
	// can never pin a dead listener's record forever.
	if rec, ok := wakeReadListener(dir); ok && rec.Fingerprint == fingerprint &&
		wakeMarkerCurrent(dir, rec.Generation) && wakeWorkerRunning(dir, rec) {
		return
	}
	// Remember the outgoing worker for rollback: if the replacement
	// never starts, a still-running previous listener gets its marker,
	// pointer and record back instead of an honest but silent gap.
	oldRec, hadOld := wakeReadListener(dir)
	oldRunning := hadOld && wakeWorkerRunning(dir, oldRec)
	wakeCancelGenerations(dir) // supersede: old workers cancel on their markers

	generation, err := wakeNewGeneration()
	if err != nil {
		wakeNote(errw, "generation mint failed; no listener ensured")
		return
	}
	genDir := wakeGenerationDir(dir, generation)
	if err := os.MkdirAll(genDir, 0o700); err != nil {
		wakeNote(errw, "state dir unavailable; no listener ensured")
		return
	}
	// Write the marker and pointer BEFORE spawning: the new worker's
	// pre-run check finds its generation already in place, and a
	// superseded worker cancels itself on the removed old marker.
	marker := filepath.Join(genDir, wakeControlFile)
	if err := writeAtomic(marker, []byte(generation+"\n"), 0o600); err != nil {
		wakeNote(errw, "control marker unavailable; no listener ensured")
		return
	}
	if err := writeAtomic(filepath.Join(dir, wakeCurrentFile), []byte(generation), 0o600); err != nil {
		wakeNote(errw, "control marker unavailable; no listener ensured")
		return
	}
	cc.Generation = generation
	cc.ControlPath = marker
	cfgJSON, err := json.Marshal(cc)
	if err != nil {
		wakeNote(errw, "config encode failed; no listener ensured")
		return
	}
	pid, err := wakeSpawn(wakeWorkerCommand(cc), cfgJSON, filepath.Join(genDir, wakeWorkerLog), dir)
	if err != nil {
		wakeCancelGenerations(dir) // remove the worker that never started
		if oldRunning {
			// Roll back: the previous worker still holds its lifetime
			// lock, so it is alive (or exiting and the next ensure
			// respawns). Restore its generation so a failed replacement
			// does not silently leave no listener at all.
			wakeRestoreGeneration(dir, oldRec)
			wakeNote(errw, "listener failed to start; previous listener kept")
		} else {
			wakeNote(errw, "listener failed to start")
		}
		return
	}
	rec := wakeListenerRecord{PID: pid, Generation: generation, Fingerprint: fingerprint,
		Endpoint: endpoint, Namespace: ns, URL: opts.BaseURL,
		StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if raw, merr := json.Marshal(rec); merr == nil {
		_ = writeAtomic(filepath.Join(dir, wakeListenerFile), raw, 0o600)
	}
}

// wakeStop tears the session's listener down under the lifecycle lock:
// the current generation's marker and the listener record are removed
// and no PID is signalled. It runs on SessionEnd and acts whether or not
// messaging is enabled.
//
// Stale-stop guard: a SessionEnd hook carries no generation, so stop
// proves it is looking at ITS listener before tearing down: when the
// recorded endpoint (or server URL) is known and the current hook
// environment resolves a different one, the record belongs to a newer
// session process (or another server) and is left alone. When the
// endpoint is no longer resolvable (session already gone) teardown
// proceeds - the worker would exit on its own failed probe anyway.
// Residual race, documented: a stale stop arriving with an identical
// endpoint and URL cannot be told apart and still removes the marker;
// the next ensure respawns.
func wakeStop(client string, opts WakeOpts, stdin io.Reader, errw io.Writer) {
	sessionID, _, err := wakeSessionIdentity(client, stdin)
	if err != nil {
		wakeNote(errw, "%v; nothing to stop", err)
		return
	}
	dir := wakeSessionDir(client, sessionID)
	if rec, ok := wakeReadListener(dir); ok {
		if ep, _, _, eerr := wakeEndpoint(client); eerr == nil && rec.Endpoint != "" && ep != rec.Endpoint {
			wakeNote(errw, "listener endpoint differs; leaving it alone")
			return
		}
		if rec.URL != "" && opts.BaseURL != "" && rec.URL != opts.BaseURL {
			wakeNote(errw, "listener server differs; leaving it alone")
			return
		}
	}
	wakeTeardownLocked(dir, errw)
}

// wakeTeardownLocked removes the current generation's marker and the
// listener record under the same lifecycle lock ensure's spawn decisions
// take, so a teardown can never interleave with a spawn. The runner's
// budget state is left intact so a later ensure keeps the storm-cap
// history.
func wakeTeardownLocked(dir string, errw io.Writer) {
	if _, err := os.Stat(dir); err != nil {
		return // nothing was ever ensured for this session
	}
	unlock, err := lockInboxFile(filepath.Join(dir, wakeLockFile), wakeLockTimeout)
	if err != nil {
		// A wedged lock holder must not let teardown race a spawn:
		// leaving the listener running is the safe failure.
		wakeNote(errw, "state lock unavailable; listener left running")
		return
	}
	defer unlock()
	wakeCancelGenerations(dir)
}

// wakeCancelGenerations removes every control marker the lifecycle may
// have written: all generation dirs, the legacy flat marker, the
// current-generation pointer and the listener record. The runner's own
// files (budget, state lock) are never touched. Caller holds the
// lifecycle lock.
func wakeCancelGenerations(dir string) {
	_ = os.RemoveAll(filepath.Join(dir, wakeGenDir))
	_ = os.Remove(filepath.Join(dir, wakeLegacyControl))
	_ = os.Remove(filepath.Join(dir, wakeCurrentFile))
	_ = os.Remove(filepath.Join(dir, wakeListenerFile))
}

// wakeMarkerCurrent reports whether generation is both the pointed-at
// generation and present as a marker with matching raw content.
func wakeMarkerCurrent(dir, generation string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, wakeCurrentFile))
	if err != nil || strings.TrimSpace(string(raw)) != generation {
		return false
	}
	mraw, err := os.ReadFile(filepath.Join(wakeGenerationDir(dir, generation), wakeControlFile))
	if err != nil {
		return false
	}
	return strings.TrimRight(string(mraw), "\r\n") == generation
}

// wakeWorkerRunning reports whether the recorded worker provably runs:
// its per-generation lifetime lock is held (acquired at run start, held
// until exit), or - only inside the startup grace window between spawn
// and lock acquisition - its PID is alive. PID evidence alone after the
// grace window is worthless (the number may belong to a stranger), so a
// dead worker's record can never pin the session forever.
func wakeWorkerRunning(dir string, rec wakeListenerRecord) bool {
	if wakeWorkerLockHeld(wakeGenerationDir(dir, rec.Generation)) {
		return true
	}
	if rec.PID > 0 && wakePidAlive(rec.PID) {
		if started, err := time.Parse(time.RFC3339, rec.StartedAt); err == nil &&
			time.Since(started) < wakeStartupGrace {
			return true
		}
	}
	return false
}

// wakeWorkerLockHeld probes the run child's lifetime flock. A vanished
// generation dir is "not held"; any other acquisition failure counts as
// held (conservative: never double-spawn on an ambiguous probe).
func wakeWorkerLockHeld(genDir string) bool {
	unlock, err := lockInboxFile(filepath.Join(genDir, wakeWorkerLock), wakeLockProbeTimeout)
	if err == nil {
		unlock()
		return false
	}
	return !errors.Is(err, os.ErrNotExist)
}

// wakeRestoreGeneration puts a rolled-back listener's marker, pointer
// and record back after a failed replacement spawn (see wakeEnsure).
func wakeRestoreGeneration(dir string, rec wakeListenerRecord) {
	gdir := wakeGenerationDir(dir, rec.Generation)
	if err := os.MkdirAll(gdir, 0o700); err != nil {
		return
	}
	_ = writeAtomic(filepath.Join(gdir, wakeControlFile), []byte(rec.Generation+"\n"), 0o600)
	_ = writeAtomic(filepath.Join(dir, wakeCurrentFile), []byte(rec.Generation), 0o600)
	if raw, err := json.Marshal(rec); err == nil {
		_ = writeAtomic(filepath.Join(dir, wakeListenerFile), raw, 0o600)
	}
}

// wakeReadListener loads the listener record, if any.
func wakeReadListener(dir string) (wakeListenerRecord, bool) {
	var rec wakeListenerRecord
	raw, err := os.ReadFile(filepath.Join(dir, wakeListenerFile))
	if err != nil || json.Unmarshal(raw, &rec) != nil || rec.Generation == "" {
		return wakeListenerRecord{}, false
	}
	return rec, true
}

// ---- run (the detached worker) ------------------------------------------

// wakeRun is the listener process. It reads the one private config JSON
// ensure piped to stdin (then closes stdin), verifies its generation
// marker is still current - a stop or replacement that landed between
// spawn and start wins - and runs the nativewake runner. A superseded
// predecessor may still hold the state-dir lock briefly (its marker poll
// has to fire first), so a lock-busy failure is retried for a bounded
// window before giving up. Every diagnostic is a fixed generic string:
// the child log must never carry a wrapped error's URL or path.
func wakeRun(client string, opts WakeOpts, stdin io.Reader, errw io.Writer) {
	raw, err := io.ReadAll(io.LimitReader(stdin, wakeMaxConfigBytes))
	if c, ok := stdin.(io.Closer); ok {
		_ = c.Close() // config received; never hold the parent's pipe open
	}
	if err != nil {
		wakeNote(errw, "config unreadable; not starting")
		return
	}
	var cc wakeChildConfig
	if err := json.Unmarshal(raw, &cc); err != nil {
		wakeNote(errw, "bad config; refusing to run")
		return
	}
	if cc.Client != client {
		wakeNote(errw, "config client mismatch; refusing to run")
		return
	}
	if cc.ControlPath == "" || cc.Generation == "" {
		wakeNote(errw, "config carries no control marker; refusing to run")
		return
	}
	if u, perr := url.Parse(cc.URL); perr != nil || u.User != nil {
		wakeNote(errw, "config URL rejected; refusing to run")
		return
	}
	mraw, err := os.ReadFile(cc.ControlPath)
	if err != nil || strings.TrimRight(string(mraw), "\r\n") != cc.Generation {
		wakeNote(errw, "control marker gone or superseded; not starting")
		return
	}
	// Hold the generation's worker lock for the whole run: it is the
	// lifecycle's process proof (PID reuse cannot fake an flock). A
	// held lock means this run is a duplicate spawn of the same
	// generation - leave, the first one owns it.
	unlock, err := lockInboxFile(filepath.Join(filepath.Dir(cc.ControlPath), wakeWorkerLock), wakeLockTimeout)
	if err != nil {
		wakeNote(errw, "worker lock held; another run of this generation owns the session")
		return
	}
	defer unlock()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	deadline := time.Now().Add(wakeLockRetryWindow)
	for {
		tr, terr := wakeNewTransport(cc)
		if terr != nil {
			wakeNote(errw, "no transport for this client; not starting")
			return
		}
		// Run owns and closes the transport on every exit, including a
		// failed lock acquisition: each attempt builds a fresh one.
		err := nativewake.Run(ctx, cc.Config, tr)
		switch {
		case err == nil:
			return // marker stop or interrupt: normal listener end
		case errors.Is(err, nativewake.ErrUnavailable):
			wakeNote(errw, "native target unavailable; listener stopped")
			return
		case wakeStateLockBusy(err) && time.Now().Before(deadline):
			// The superseded worker still holds the state dir; its
			// marker watch cancels it within one poll interval.
			if !sleepFor(ctx, wakeLockRetryPolling) {
				return
			}
			continue
		case wakeStateLockBusy(err):
			wakeNote(errw, "previous listener did not release the state lock in time; not starting")
			return
		default:
			wakeNote(errw, "listener ended with an error")
			return
		}
	}
}

func sleepFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// wakeStateLockBusy reports whether a Run failure is the per-identity
// state-dir lock held by the previous generation (exported sentinel
// since the planner-authorized types.go addition; the string fallback
// covers a mixed-version nativewake).
func wakeStateLockBusy(err error) bool {
	if errors.Is(err, nativewake.ErrStateLocked) {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "state dir already locked")
}

// ---- identity, paths, endpoint ------------------------------------------

// wakeFingerprint digests the listener's whole configuration identity.
// It is the dedup key only - unlike the generation it never lands in a
// control marker, so a stop+ensure pair cannot resurrect a cancelled
// worker by reproducing its marker text.
func wakeFingerprint(cc wakeChildConfig) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		cc.URL, cc.Namespace, cc.Client, cc.SessionID, cc.Endpoint,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// wakeNewGeneration mints one fresh crypto-random generation per spawn.
func wakeNewGeneration() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// wakeGenerationDir scopes one generation's control marker and log.
func wakeGenerationDir(dir, generation string) string {
	return filepath.Join(dir, wakeGenDir, wakePathTokenSafe(generation))
}

// wakePathTokenSafe rejects anything but a plain hex generation, so the
// current-pointer file can never steer a removal outside the state dir.
func wakePathTokenSafe(s string) string {
	for _, r := range s {
		if (r < 'a' || r > 'f') && (r < '0' || r > '9') {
			return ""
		}
	}
	return s
}

// wakeStateRoot is the root below which every per-session wake state
// dir lives: an absolute $XDG_STATE_HOME/punk/wake, else
// ~/.local/state/punk/wake. A relative XDG_STATE_HOME is invalid per
// the basedir spec and falls back consistently, never landing in a
// caller-controlled cwd.
func wakeStateRoot() string {
	if x := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "punk", "wake")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "punk-wake")
	}
	return filepath.Join(home, ".local", "state", "punk", "wake")
}

// wakeSessionDir scopes all lifecycle files to one client session.
func wakeSessionDir(client, sessionID string) string {
	return filepath.Join(wakeStateRoot(), wakePathToken(client), wakePathToken(sessionID))
}

// wakePathToken maps an identity token to one path segment as a digest
// of the ORIGINAL string. Hashing always (instead of sanitising) keeps
// distinct ids distinct after separator folding ("a:b" vs "a_b") and
// makes traversal ("..", "/", control bytes) impossible by construction.
func wakePathToken(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "s-" + hex.EncodeToString(sum[:])[:32]
}

// WakeListenerStatus reports whether a native wake listener was
// requested for this session - a listener record with a current
// generation marker - and whether the session's native endpoint is
// available right now. Requested is NOT a liveness promise: the worker
// may have died without clearing its record. The inbox routing guidance
// uses this to describe capability honestly instead of claiming there is
// no idle wake at all.
func WakeListenerStatus(client, sessionID string) (requested, capable bool) {
	client = strings.ToLower(strings.TrimSpace(client))
	switch client {
	case "claude-code", "codex":
	default:
		return false, false
	}
	if !validAddressPart(sessionID) {
		return false, false
	}
	dir := wakeSessionDir(client, sessionID)
	rec, ok := wakeReadListener(dir)
	if !ok || !wakeMarkerCurrent(dir, rec.Generation) {
		return false, false
	}
	_, _, _, err := wakeEndpoint(client)
	return true, err == nil
}

// wakeEndpoint resolves the native wake endpoint for client WITHOUT
// starting anything: the Claude own-session messaging socket from the
// hook's inherited environment, or the codex shared daemon's control
// socket by path existence alone (a probe that spawned a daemon would
// create exactly the second app-server the design forbids). A missing
// capability is an error, and the caller fails open.
func wakeEndpoint(client string) (endpoint, token, binary string, err error) {
	switch client {
	case "claude-code":
		socket := strings.TrimSpace(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"))
		if socket == "" {
			return "", "", "", errors.New("CLAUDE_CODE_MESSAGING_SOCKET is not set")
		}
		st, serr := os.Stat(socket)
		if serr != nil || st.Mode()&os.ModeSocket == 0 {
			return "", "", "", errors.New("messaging socket is not available")
		}
		return socket, os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN"), "", nil
	case "codex":
		bin, lerr := exec.LookPath("codex")
		if lerr != nil {
			return "", "", "", errors.New("codex binary not found")
		}
		sock := codexControlSocket()
		if _, serr := os.Stat(sock); serr != nil {
			return "", "", "", errors.New("codex app-server control socket is not available")
		}
		return sock, "", bin, nil
	}
	return "", "", "", fmt.Errorf("no wake endpoint for client %q", client)
}

// codexControlSocket is the shared app-server daemon's control socket:
// $CODEX_HOME/app-server-control/app-server-control.sock.
func codexControlSocket() string {
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".codex")
		}
	}
	return filepath.Join(home, "app-server-control", "app-server-control.sock")
}
