package nativewake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// Wire tests against a fake codex daemon. The fake daemon is a REAL
// WebSocket-over-Unix-socket server (x/net/websocket behind http.Server)
// speaking the app-server protocol verified against codex-cli 0.157.1:
// one JSON-RPC message per WS text frame, client frames masked. The
// production transport reaches it through a compiled test double of
// `codex app-server proxy` (testdata/codexfakeproxy) - a raw stdio<->
// socket byte relay with the real proxy's connect-failure behavior - and
// one test additionally runs the REAL installed proxy binary. No live
// user session, daemon or app-server is ever contacted.

// ---- fake proxy binary ----------------------------------------------------

var (
	proxyBuildOnce sync.Once
	proxyBinPath   string
	proxyBuildErr  error
)

// fakeProxyBinary builds the raw-relay test double once per test binary.
func fakeProxyBinary(t *testing.T) string {
	t.Helper()
	proxyBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "codexfakeproxy-build")
		if err != nil {
			proxyBuildErr = err
			return
		}
		out := filepath.Join(dir, "codexfakeproxy")
		src := filepath.Join("testdata", "codexfakeproxy", "main.go")
		cmd := exec.Command("go", "build", "-o", out, src)
		if cmd.Run() != nil {
			proxyBuildErr = fmt.Errorf("go build %s failed", src)
			return
		}
		proxyBinPath = out
	})
	if proxyBuildErr != nil {
		t.Skipf("fake proxy unavailable: %v", proxyBuildErr)
	}
	return proxyBinPath
}

// fakeProxy wires one fake-proxy invocation: a state dir for argv/pid/
// start-count assertions and the socket the relay must dial.
type fakeProxy struct {
	dir  string
	sock string
}

func newFakeProxy(t *testing.T, sock string) *fakeProxy {
	t.Helper()
	fp := &fakeProxy{dir: t.TempDir(), sock: sock}
	t.Setenv("PUNK_FAKE_PROXY_STATE", fp.dir)
	t.Setenv("PUNK_FAKE_PROXY_SOCK", sock)
	return fp
}

func (f *fakeProxy) argv() string {
	raw, err := os.ReadFile(filepath.Join(f.dir, "argv"))
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(raw), "\n")
}

func (f *fakeProxy) starts() int {
	raw, err := os.ReadFile(filepath.Join(f.dir, "starts"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return n
}

// pidsGone waits until every recorded proxy pid has fully exited and been
// reaped (no /proc entry means no zombie either).
func (f *fakeProxy) pidsGone(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "pids"))
	if err != nil {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, pidStr := range strings.Fields(string(raw)) {
		for {
			if _, err := os.Stat("/proc/" + pidStr); os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("proxy pid %s still present (not killed/reaped)", pidStr)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// ---- fake daemon control socket -------------------------------------------

// daemonCfg configures one fake daemon scenario. Zero values select the
// happy wake path.
type daemonCfg struct {
	threadID string

	loaded         []string // default [threadID]
	resumeStatus   string   // default "idle"
	resumeThread   string   // default threadID; different => mismatch test
	resumeNoThread bool     // resume result without a thread
	resumeErr      bool     // resume answers a JSON-RPC error
	queueErr       bool     // thread/queue/add answers a JSON-RPC error
	queueNoID      bool     // queue/add result without a usable submission id
	queueNoCapErr  bool     // queue/add answers the experimental-gate error

	// hostActiveAfterResume simulates the idle->send race: resume answers
	// idle, then a human turn starts before the wake submission arrives.
	// A queue/add submission must still be accepted (enqueued for later
	// dispatch) and a turn/start must NEVER be sent.
	hostActiveAfterResume bool

	hangInit        bool // never answer initialize (first conn only)
	garbageInit     bool // non-JSON text frame after initialize
	oversizeInit    bool // >1MiB text frame after initialize
	closeAfterInit  bool // close the WS conn right after initialize (first conn only)
	closeAfterQueue bool // close the WS conn right after receiving queue/add
	floodOnResume   int  // notification frames after the resume response
	injectApproval  bool // server-initiated approval request after resume
}

// fakeDaemon is the in-process stand-in for the codex shared daemon's
// control socket: WebSocket over a Unix socket, app-server JSON-RPC
// inside the frames.
type fakeDaemon struct {
	sock   string
	ln     net.Listener
	srv    *http.Server
	cfg    daemonCfg
	mu     sync.Mutex
	frames []string // every JSON-RPC message received, in order
	conns  int      // completed WS handshakes
	// experimentalOK mirrors the daemon's connection gate: queue methods
	// are rejected unless initialize declared capabilities.experimentalApi.
	experimentalOK bool
	// queuedWhileActive counts queue/add calls that arrived while the
	// simulated host was already active (the race path).
	queuedWhileActive int
	closed            chan struct{}
}

func newFakeDaemon(t *testing.T, cfg daemonCfg) *fakeDaemon {
	t.Helper()
	// Subtest names in t.TempDir can exceed the Unix socket path limit even
	// with an ordinary TMPDIR. Keep the socket fixture name short instead.
	// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=verification test=TestCodexRPCErrorResults,TestCodexBrokenResultShapes
	dir, err := os.MkdirTemp("", "punk-wake-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "app-server-control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDaemon{sock: sock, ln: ln, cfg: cfg, closed: make(chan struct{})}
	handler := websocket.Handler(func(ws *websocket.Conn) {
		d.mu.Lock()
		d.conns++
		connNo := d.conns
		d.mu.Unlock()
		d.serveConn(ws, connNo)
	})
	d.srv = &http.Server{Handler: handler}
	go func() { _ = d.srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = d.srv.Close()
		_ = ln.Close()
		close(d.closed)
	})
	return d
}

func (d *fakeDaemon) addr() string { return d.sock }

func (d *fakeDaemon) record(frame string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.frames = append(d.frames, frame)
}

func (d *fakeDaemon) received() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.frames...)
}

func (d *fakeDaemon) connCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conns
}

func (d *fakeDaemon) queuedWhileActiveCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.queuedWhileActive
}

func (d *fakeDaemon) send(ws *websocket.Conn, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = websocket.Message.Send(ws, payload)
}

func (d *fakeDaemon) serveConn(ws *websocket.Conn, connNo int) {
	defer func() { _ = ws.Close() }()
	first := connNo == 1
	respond := func(id any, result any) {
		d.send(ws, map[string]any{"id": id, "result": result})
	}
	respondErr := func(id any, code int, message string) {
		d.send(ws, map[string]any{"id": id, "error": map[string]any{"code": code, "message": message}})
	}
	loaded := d.cfg.loaded
	if loaded == nil {
		loaded = []string{"other-thread", d.cfg.threadID}
	}
	resumeStatus := d.cfg.resumeStatus
	if resumeStatus == "" {
		resumeStatus = "idle"
	}
	resumeThread := d.cfg.resumeThread
	if resumeThread == "" {
		resumeThread = d.cfg.threadID
	}
	for {
		var data []byte
		if err := websocket.Message.Receive(ws, &data); err != nil {
			return
		}
		d.record(string(data))
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		var params struct {
			ClientInfo   map[string]any `json:"clientInfo"`
			Capabilities *struct {
				ExperimentalAPI bool `json:"experimentalApi"`
			} `json:"capabilities"`
			ThreadID            string          `json:"threadId"`
			ClientUserMessageID string          `json:"clientUserMessageId"`
			Input               json.RawMessage `json:"input"`
		}
		if len(msg.Params) > 0 {
			if err := json.Unmarshal(msg.Params, &params); err != nil {
				return
			}
		}
		switch msg.Method {
		case "initialize":
			if d.cfg.hangInit && first {
				// Never answer; stay until the client goes away or the
				// test tears the daemon down.
				gone := make(chan struct{})
				go func() {
					var sink []byte
					for {
						if websocket.Message.Receive(ws, &sink) != nil {
							close(gone)
							return
						}
					}
				}()
				select {
				case <-d.closed:
				case <-gone:
				}
				return
			}
			// The real daemon records capabilities per connection and
			// rejects experimental methods unless experimentalApi was
			// declared here (message_processor.rs).
			d.mu.Lock()
			d.experimentalOK = params.Capabilities != nil && params.Capabilities.ExperimentalAPI
			d.mu.Unlock()
			respond(msg.ID, map[string]any{
				"userAgent":      "codex-tui/0.157.1 (fake-daemon)",
				"codexHome":      "/tmp/fake-codex-home",
				"platformFamily": "unix",
				"platformOs":     "linux",
			})
			switch {
			case d.cfg.garbageInit:
				_ = websocket.Message.Send(ws, []byte("this is not json"))
			case d.cfg.oversizeInit:
				_ = websocket.Message.Send(ws, []byte(strings.Repeat("a", 2<<20)))
			case d.cfg.closeAfterInit && first:
				return // closing the conn drops the relay too
			}
		case "initialized":
			// Notification: no response, exactly like the live daemon.
		case "thread/loaded/list":
			respond(msg.ID, map[string]any{"data": loaded, "nextCursor": nil})
		case "thread/resume":
			switch {
			case d.cfg.resumeErr:
				respondErr(msg.ID, -32602, "bad")
			case d.cfg.resumeNoThread:
				respond(msg.ID, map[string]any{})
			default:
				// With hostActiveAfterResume, a human turn starts right
				// after this idle status was read; the queue/add handler
				// below counts submissions that raced the active host.
				respond(msg.ID, map[string]any{"thread": map[string]any{
					"id":        resumeThread,
					"sessionId": d.cfg.threadID,
					"status":    map[string]any{"type": resumeStatus},
				}})
			}
			if d.cfg.injectApproval {
				// Server-initiated approval request: the transport must
				// keep going and never write any reply to it.
				d.send(ws, map[string]any{
					"id":     777,
					"method": "item/commandExecution/requestApproval",
					"params": map[string]any{
						"threadId": d.cfg.threadID, "callId": "c1",
						"command": []string{"rm", "-rf", "/"}, "cwd": "/",
					},
				})
				d.send(ws, map[string]any{
					"id": 778, "method": "applyPatchApproval", "params": map[string]any{},
				})
			}
			for i := 0; i < d.cfg.floodOnResume; i++ {
				d.send(ws, map[string]any{
					"method": "item/agentMessage/delta",
					"params": map[string]any{"delta": "x"},
				})
			}
		case "thread/queue/add":
			if d.cfg.closeAfterQueue {
				return
			}
			// The live daemon rejects a missing clientUserMessageId with
			// -32600 (W6 live proof, serde-enforced in
			// protocol/v2/thread.rs); the fake enforces the identical rule.
			if params.ClientUserMessageID == "" {
				respondErr(msg.ID, -32600, "Invalid request: missing field `clientUserMessageId`")
				continue
			}
			// Experimental gate exactly as message_processor.rs applies
			// it: a connection that did not declare experimentalApi gets
			// the gate error for queue methods. This catches a transport
			// regression that forgets the capability at initialize.
			if !d.experimentalOK {
				respondErr(msg.ID, -32600, "thread/queue/add requires experimentalApi capability")
				continue
			}
			if d.cfg.hostActiveAfterResume {
				d.mu.Lock()
				d.queuedWhileActive++
				d.mu.Unlock()
			}
			switch {
			case d.cfg.queueErr:
				respondErr(msg.ID, -32001, "Server overloaded; retry later.")
			case d.cfg.queueNoID:
				respond(msg.ID, map[string]any{"queuedSubmission": map[string]any{}})
			case d.cfg.queueNoCapErr:
				respondErr(msg.ID, -32600, "thread/queue/add requires experimentalApi capability")
			default:
				respond(msg.ID, map[string]any{"queuedSubmission": map[string]any{
					"id":                  "queued-submission-1",
					"input":               params.Input,
					"clientUserMessageId": params.ClientUserMessageID,
				}})
			}
		default:
			respondErr(msg.ID, -32601, "Method not found")
		}
	}
}

// ---- helpers ---------------------------------------------------------------

// uuidLike checks the RFC 4122 text form the clientUserMessageId uses.
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func uuidLike(s string) bool { return uuidRe.MatchString(s) }

func decodeFrame(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("frame not JSON: %q: %v", line, err)
	}
	return m
}

func methodsOf(t *testing.T, frames []string) []string {
	t.Helper()
	var out []string
	for _, f := range frames {
		m := decodeFrame(t, f)
		if method, _ := m["method"].(string); method != "" {
			out = append(out, method)
		}
	}
	return out
}

// ---- tests -----------------------------------------------------------------

func TestCodexWakeHappyWireExact(t *testing.T) {
	const tid = "0198f3c0-1111-7abc-def0-1234567890ab"
	d := newFakeDaemon(t, daemonCfg{threadID: tid})
	fp := newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	defer func() { _ = tr.Close() }()

	out, err := tr.Wake(context.Background(), "nudge text")
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if !out.Accepted || out.Unconfirmed {
		t.Fatalf("outcome=%+v", out)
	}

	frames := d.received()
	if len(frames) != 5 {
		t.Fatalf("sent %d frames, want 5: %q", len(frames), frames)
	}
	// 1: initialize, with the experimentalApi capability queue/add needs
	f := decodeFrame(t, frames[0])
	if f["method"] != "initialize" {
		t.Fatalf("frame0=%v", f)
	}
	if id, ok := f["id"].(float64); !ok || id < 1 {
		t.Fatalf("frame0 id=%v", f["id"])
	}
	params := f["params"].(map[string]any)
	info := params["clientInfo"].(map[string]any)
	if info["name"] != "punk-native-wake" || info["version"] == "" {
		t.Fatalf("clientInfo=%v", info)
	}
	caps, _ := params["capabilities"].(map[string]any)
	if caps == nil || caps["experimentalApi"] != true {
		t.Fatalf("initialize must declare capabilities.experimentalApi=true (queue/add is experimental-gated): %v", params)
	}
	// 2: initialized notification, no id, no params
	f = decodeFrame(t, frames[1])
	if f["method"] != "initialized" {
		t.Fatalf("frame1=%v", f)
	}
	if _, has := f["id"]; has {
		t.Fatalf("initialized must not carry id: %v", f)
	}
	if _, has := f["params"]; has {
		t.Fatalf("initialized must not carry params: %v", f)
	}
	// 3: thread/loaded/list
	f = decodeFrame(t, frames[2])
	if f["method"] != "thread/loaded/list" {
		t.Fatalf("frame2=%v", f)
	}
	// 4: thread/resume without overrides
	f = decodeFrame(t, frames[3])
	if f["method"] != "thread/resume" {
		t.Fatalf("frame3=%v", f)
	}
	params = f["params"].(map[string]any)
	if params["threadId"] != tid {
		t.Fatalf("resume params=%v", params)
	}
	if params["excludeTurns"] != true {
		t.Fatalf("resume params=%v", params)
	}
	for _, banned := range []string{"model", "cwd", "sandbox", "approvalPolicy", "config", "baseInstructions", "developerInstructions"} {
		if _, has := params[banned]; has {
			t.Fatalf("resume must not override %s: %v", banned, params)
		}
	}
	// 5: thread/queue/add with text input and a per-invocation
	// clientUserMessageId, nothing else. NEVER turn/start.
	f = decodeFrame(t, frames[4])
	if f["method"] != "thread/queue/add" {
		t.Fatalf("frame4=%v (wake must submit via thread/queue/add, never turn/start)", f)
	}
	params = f["params"].(map[string]any)
	if params["threadId"] != tid {
		t.Fatalf("queue params=%v", params)
	}
	input := params["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("queue input=%v", input)
	}
	item := input[0].(map[string]any)
	if item["type"] != "text" || item["text"] != "nudge text" {
		t.Fatalf("queue input item=%v", item)
	}
	msgID, _ := params["clientUserMessageId"].(string)
	if !uuidLike(msgID) {
		t.Fatalf("clientUserMessageId must be a uuid-form id, got %q", msgID)
	}
	for _, banned := range []string{"model", "cwd", "sandboxPolicy", "approvalPolicy", "effort"} {
		if _, has := params[banned]; has {
			t.Fatalf("thread/queue/add must not override %s: %v", banned, params)
		}
	}
	// Only ever `app-server proxy --sock <path>`: no app-server proper.
	if got := fp.argv(); got != "app-server proxy --sock "+d.addr() {
		t.Fatalf("argv=%q", got)
	}
}

func TestCodexProbeHappy(t *testing.T) {
	const tid = "thread-1"
	d := newFakeDaemon(t, daemonCfg{threadID: tid})
	newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	defer func() { _ = tr.Close() }()
	if err := tr.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	// Probe stops after resume: no wake submission at all.
	for _, m := range methodsOf(t, d.received()) {
		if m == "thread/queue/add" || m == "turn/start" {
			t.Fatalf("probe sent a wake submission: %v", methodsOf(t, d.received()))
		}
	}
}

func TestCodexUnloadedThreadUnavailable(t *testing.T) {
	const tid = "thread-1"
	d := newFakeDaemon(t, daemonCfg{threadID: tid, loaded: []string{"other-thread"}})
	newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	defer func() { _ = tr.Close() }()
	if err := tr.Probe(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("probe err=%v want ErrUnavailable", err)
	}
	if _, err := tr.Wake(context.Background(), "hi"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wake err=%v want ErrUnavailable", err)
	}
	for _, m := range methodsOf(t, d.received()) {
		if m == "thread/resume" || m == "turn/start" || m == "thread/queue/add" {
			t.Fatalf("unloaded thread must not resume or submit anything: %v", methodsOf(t, d.received()))
		}
	}
}

func TestCodexStatusMapping(t *testing.T) {
	const tid = "thread-1"
	cases := []struct {
		name         string
		resumeStatus string
		wantErr      error
	}{
		{"idle", "idle", nil},
		{"activeWaitingOnApproval", "active", ErrBusy},
		{"systemError", "systemError", ErrBusy},
		{"unknownFutureStatus", "weirdFutureStatus", ErrBusy}, // unknown -> defer
		{"notLoaded", "notLoaded", ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDaemon(t, daemonCfg{threadID: tid, resumeStatus: tc.resumeStatus})
			newFakeProxy(t, d.addr())
			tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
			defer func() { _ = tr.Close() }()
			_, err := tr.Wake(context.Background(), "nudge")
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("wake: %v", err)
				}
				if !strings.Contains(strings.Join(methodsOf(t, d.received()), ","), "thread/queue/add") {
					t.Fatal("idle thread was not nudged")
				}
			} else {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("wake err=%v want %v", err, tc.wantErr)
				}
				for _, m := range methodsOf(t, d.received()) {
					if m == "thread/queue/add" {
						t.Fatalf("non-idle thread got a queue/add: %v", methodsOf(t, d.received()))
					}
				}
			}
		})
	}
}

// TestCodexRaceQueuesNotSteers locks the reason wake uses thread/queue/add
// instead of turn/start: the thread reported idle at resume time, but a
// human turn starts before the submission arrives. queue/add enqueues
// for later auto-dispatch (safe); turn/start at that moment would STEER
// the human's turn. The fake daemon enforces the contract: it counts
// queue/add calls that raced an active host, and any turn/start would
// show up in the recorded methods and fail the test.
func TestCodexRaceQueuesNotSteers(t *testing.T) {
	const tid = "thread-1"
	d := newFakeDaemon(t, daemonCfg{threadID: tid, hostActiveAfterResume: true})
	newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	defer func() { _ = tr.Close() }()

	out, err := tr.Wake(context.Background(), "nudge")
	if err != nil || !out.Accepted {
		t.Fatalf("wake out=%+v err=%v (racing human turn must queue, not fail)", out, err)
	}
	got := methodsOf(t, d.received())
	for _, m := range got {
		if m == "turn/start" {
			t.Fatalf("turn/start sent while host active (would steer the human turn): %v", got)
		}
	}
	if n := d.queuedWhileActiveCount(); n != 1 {
		t.Fatalf("queue/add while active=%d, want 1", n)
	}
	if !strings.Contains(strings.Join(got, ","), "thread/queue/add") {
		t.Fatalf("no queue/add submitted: %v", got)
	}
}

func TestCodexMismatchedReturnedThreadID(t *testing.T) {
	const tid = "thread-1"
	d := newFakeDaemon(t, daemonCfg{threadID: tid, resumeThread: "someone-else"})
	newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	defer func() { _ = tr.Close() }()
	err := tr.Probe(context.Background())
	if err == nil || errors.Is(err, ErrBusy) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("probe err=%v want a definite mismatch error", err)
	}
	if _, err := tr.Wake(context.Background(), "hi"); err == nil || errors.Is(err, ErrBusy) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("wake err=%v want a definite mismatch error", err)
	}
	for _, m := range methodsOf(t, d.received()) {
		if m == "thread/queue/add" {
			t.Fatalf("mismatched thread must not submit a queue/add: %v", methodsOf(t, d.received()))
		}
	}
}

func TestCodexRPCErrorResults(t *testing.T) {
	const tid = "thread-1"
	t.Run("resume error", func(t *testing.T) {
		d := newFakeDaemon(t, daemonCfg{threadID: tid, resumeErr: true})
		newFakeProxy(t, d.addr())
		tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
		defer func() { _ = tr.Close() }()
		err := tr.Probe(context.Background())
		if err == nil || errors.Is(err, ErrBusy) || errors.Is(err, ErrUnavailable) {
			t.Fatalf("probe err=%v want definite error", err)
		}
	})
	t.Run("queue/add error", func(t *testing.T) {
		d := newFakeDaemon(t, daemonCfg{threadID: tid, queueErr: true})
		newFakeProxy(t, d.addr())
		tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
		defer func() { _ = tr.Close() }()
		out, err := tr.Wake(context.Background(), "hi")
		if err == nil || errors.Is(err, ErrBusy) || errors.Is(err, ErrUnavailable) {
			t.Fatalf("wake err=%v want definite error", err)
		}
		if out.Accepted {
			t.Fatal("error result must not count as accepted enqueue")
		}
	})
	t.Run("experimental capability gate error", func(t *testing.T) {
		// A server answering the experimental gate error (what the real
		// daemon does when the connection did not declare experimentalApi
		// at initialize) must surface as a definite failure, never as an
		// accepted enqueue.
		d := newFakeDaemon(t, daemonCfg{threadID: tid, queueNoCapErr: true})
		newFakeProxy(t, d.addr())
		tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
		defer func() { _ = tr.Close() }()
		out, err := tr.Wake(context.Background(), "hi")
		if err == nil || errors.Is(err, ErrBusy) || errors.Is(err, ErrUnavailable) {
			t.Fatalf("wake err=%v want definite gate error", err)
		}
		if out.Accepted {
			t.Fatal("capability-gate error must not count as accepted enqueue")
		}
	})
}

func TestCodexBrokenResultShapes(t *testing.T) {
	const tid = "thread-1"
	t.Run("resume result without thread", func(t *testing.T) {
		d := newFakeDaemon(t, daemonCfg{threadID: tid, resumeNoThread: true})
		newFakeProxy(t, d.addr())
		tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
		defer func() { _ = tr.Close() }()
		out, err := tr.Wake(context.Background(), "hi")
		if err == nil {
			t.Fatal("broken result must be an error, never a silent success")
		}
		if out.Accepted {
			t.Fatal("broken result must not be accepted")
		}
	})
	t.Run("queue result without submission id", func(t *testing.T) {
		d := newFakeDaemon(t, daemonCfg{threadID: tid, queueNoID: true})
		newFakeProxy(t, d.addr())
		tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
		defer func() { _ = tr.Close() }()
		out, err := tr.Wake(context.Background(), "hi")
		if err == nil {
			t.Fatal("broken result must be an error, never a silent success")
		}
		if out.Accepted {
			t.Fatal("broken result must not be accepted")
		}
	})
}

func TestCodexMalformedAndOversizeFrames(t *testing.T) {
	const tid = "thread-1"
	t.Run("garbage", func(t *testing.T) {
		d := newFakeDaemon(t, daemonCfg{threadID: tid, garbageInit: true})
		newFakeProxy(t, d.addr())
		tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
		defer func() { _ = tr.Close() }()
		err := tr.Probe(context.Background())
		if err == nil || errors.Is(err, ErrBusy) || errors.Is(err, ErrUnavailable) {
			t.Fatalf("probe err=%v want definite transport error", err)
		}
	})
	t.Run("oversize", func(t *testing.T) {
		d := newFakeDaemon(t, daemonCfg{threadID: tid, oversizeInit: true})
		newFakeProxy(t, d.addr())
		tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
		defer func() { _ = tr.Close() }()
		err := tr.Probe(context.Background())
		if err == nil || errors.Is(err, ErrBusy) || errors.Is(err, ErrUnavailable) {
			t.Fatalf("probe err=%v want definite transport error", err)
		}
	})
}

func TestCodexProxyExitBeforeHandshakeUnavailable(t *testing.T) {
	// No daemon listening: the fake relay (like the real proxy) exits 1
	// with no stdout, which must map to ErrUnavailable (daemon down).
	sock := filepath.Join(t.TempDir(), "missing.sock")
	newFakeProxy(t, sock)
	tr := NewCodexTransport(fakeProxyBinary(t), sock, "thread-1")
	defer func() { _ = tr.Close() }()
	if err := tr.Probe(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("probe err=%v want ErrUnavailable (daemon down)", err)
	}
}

func TestCodexServerRequestsNeverAnswered(t *testing.T) {
	const tid = "thread-1"
	d := newFakeDaemon(t, daemonCfg{threadID: tid, injectApproval: true})
	newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	defer func() { _ = tr.Close() }()
	out, err := tr.Wake(context.Background(), "nudge")
	if err != nil || !out.Accepted {
		t.Fatalf("wake out=%+v err=%v", out, err)
	}
	// The daemon asks for approvals; the transport must never have
	// written any reply carrying the server request's id.
	for _, f := range d.received() {
		m := decodeFrame(t, f)
		if idv, has := m["id"]; has {
			id, _ := idv.(float64)
			if id == 777 || id == 778 {
				t.Fatalf("transport answered a server request: %s", f)
			}
		}
	}
	if got := methodsOf(t, d.received()); len(got) != 5 {
		t.Fatalf("client sent %v, want exactly the 5 wake frames", got)
	}
}

func TestCodexNotificationFloodDoesNotBlock(t *testing.T) {
	const tid = "thread-1"
	d := newFakeDaemon(t, daemonCfg{threadID: tid, floodOnResume: 5000})
	newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	defer func() { _ = tr.Close() }()
	done := make(chan struct{})
	var out Outcome
	var err error
	go func() {
		defer close(done)
		out, err = tr.Wake(context.Background(), "nudge")
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("notification flood blocked the wake")
	}
	if err != nil || !out.Accepted {
		t.Fatalf("wake out=%+v err=%v", out, err)
	}
}

func TestCodexCancellationClosesAndReapsProxy(t *testing.T) {
	const tid = "thread-1"
	// Serve everything but never answer the first connection's
	// initialize, so cancellation hits a live half-finished operation.
	d := newFakeDaemon(t, daemonCfg{threadID: tid, hangInit: true})
	fp := newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	defer func() { _ = tr.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tr.Wake(ctx, "nudge")
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wake err=%v want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not return promptly")
	}
	// The proxy we spawned must be killed and reaped (no zombie).
	fp.pidsGone(t)
	// A later operation still works on a fresh proxy.
	if _, err := tr.Wake(context.Background(), "nudge"); err != nil {
		t.Fatalf("wake after cancel: %v", err)
	}
	if fp.starts() != 2 {
		t.Fatalf("proxy starts=%d want 2", fp.starts())
	}
}

func TestCodexReconnectsOnceAfterConnectionDrop(t *testing.T) {
	const tid = "thread-1"
	// The daemon drops the first WebSocket connection right after
	// answering initialize; later connections serve the full flow.
	d := newFakeDaemon(t, daemonCfg{threadID: tid, closeAfterInit: true})
	fp := newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	defer func() { _ = tr.Close() }()
	out, err := tr.Wake(context.Background(), "nudge")
	if err != nil || !out.Accepted {
		t.Fatalf("wake out=%+v err=%v (want one reconnect then success)", out, err)
	}
	if got := d.connCount(); got != 2 {
		t.Fatalf("daemon connections=%d want 2", got)
	}
	if got := fp.starts(); got != 2 {
		t.Fatalf("proxy starts=%d want 2", got)
	}
	// Exactly one thread/queue/add despite the reconnect.
	subs := 0
	for _, m := range methodsOf(t, d.received()) {
		if m == "thread/queue/add" {
			subs++
		}
	}
	if subs != 1 {
		t.Fatalf("thread/queue/add sent %d times, want 1", subs)
	}
}

func TestCodexNoDuplicateTurnAfterPostWriteFailure(t *testing.T) {
	const tid = "thread-1"
	// The daemon closes the WebSocket right after receiving the
	// thread/queue/add (the response never arrives). The transport must
	// NOT reconnect and resend the submission.
	d := newFakeDaemon(t, daemonCfg{threadID: tid, closeAfterQueue: true})
	fp := newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	defer func() { _ = tr.Close() }()
	_, err := tr.Wake(context.Background(), "nudge")
	if err == nil || errors.Is(err, ErrBusy) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("wake err=%v want definite ambiguous failure", err)
	}
	if got := fp.starts(); got != 1 {
		t.Fatalf("proxy starts=%d want 1 (no reconnect after submission write)", got)
	}
}

func TestCodexCloseIsIdempotentAndStopsOperations(t *testing.T) {
	const tid = "thread-1"
	d := newFakeDaemon(t, daemonCfg{threadID: tid})
	fp := newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	if err := tr.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	fp.pidsGone(t)
	if err := tr.Probe(context.Background()); err == nil {
		t.Fatal("probe after close must fail")
	}
	if _, err := tr.Wake(context.Background(), "hi"); err == nil {
		t.Fatal("wake after close must fail")
	}
}

func TestCodexInvalidConfiguration(t *testing.T) {
	// Empty binary, missing binary and invalid thread ids are permanent:
	// ErrUnavailable without ever spawning a process.
	const tid = "thread-1"
	d := newFakeDaemon(t, daemonCfg{threadID: tid})
	fp := newFakeProxy(t, d.addr())
	missing := filepath.Join(fp.dir, "does-not-exist")
	for name, tc := range map[string]struct {
		binary, socket, tid string
	}{
		"empty binary":    {"", "", tid},
		"missing binary":  {missing, "", tid},
		"empty thread id": {fakeProxyBinary(t), "", ""},
		"bad thread id":   {fakeProxyBinary(t), "", "bad thread id!"},
	} {
		t.Run(name, func(t *testing.T) {
			tr := NewCodexTransport(tc.binary, tc.socket, tc.tid)
			defer func() { _ = tr.Close() }()
			if err := tr.Probe(context.Background()); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("probe err=%v want ErrUnavailable", err)
			}
		})
	}
	if got := fp.starts(); got != 0 {
		t.Fatalf("proxy started %d times, want 0", got)
	}
}

func TestCodexWakeTextBounds(t *testing.T) {
	const tid = "thread-1"
	d := newFakeDaemon(t, daemonCfg{threadID: tid})
	fp := newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	defer func() { _ = tr.Close() }()
	if _, err := tr.Wake(context.Background(), ""); err == nil {
		t.Fatal("empty wake text accepted")
	}
	if _, err := tr.Wake(context.Background(), strings.Repeat("x", codexMaxNudgeBytes+1)); err == nil {
		t.Fatal("oversize wake text accepted")
	}
	if got := fp.starts(); got != 0 {
		t.Fatalf("text validation must run before spawning: starts=%d", got)
	}
}

func TestCodexDefaultSocketOmittedFromArgv(t *testing.T) {
	const tid = "thread-1"
	d := newFakeDaemon(t, daemonCfg{threadID: tid})
	fp := newFakeProxy(t, d.addr())                      // sets PUNK_FAKE_PROXY_SOCK default
	tr := NewCodexTransport(fakeProxyBinary(t), "", tid) // no --sock
	defer func() { _ = tr.Close() }()
	if err := tr.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := fp.argv(); got != "app-server proxy" {
		t.Fatalf("argv=%q want no --sock with default socket", got)
	}
}

// TestCodexRealInstalledProxyWire drives the production transport through
// the REAL installed `codex app-server proxy` binary (raw relay) into the
// fake daemon control socket. No live user socket or app-server process
// is involved; the proxy only relays bytes to the test's fake endpoint.
func TestCodexRealInstalledProxyWire(t *testing.T) {
	codexBin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex CLI not installed; real-proxy wire test skipped")
	}
	const tid = "0198f3c0-1111-7abc-def0-1234567890ab"
	d := newFakeDaemon(t, daemonCfg{threadID: tid})
	tr := NewCodexTransport(codexBin, d.addr(), tid)
	defer func() { _ = tr.Close() }()

	if err := tr.Probe(context.Background()); err != nil {
		t.Fatalf("probe through real proxy: %v", err)
	}
	out, err := tr.Wake(context.Background(), "PUNK WAKE NONCE w3-real-proxy")
	if err != nil || !out.Accepted {
		t.Fatalf("wake through real proxy out=%+v err=%v", out, err)
	}
	// Probe + wake reuse one connection: one initialize, one queue/add.
	got := methodsOf(t, d.received())
	want := []string{"initialize", "initialized", "thread/loaded/list", "thread/resume", "thread/loaded/list", "thread/resume", "thread/queue/add"}
	if len(got) != len(want) {
		t.Fatalf("methods=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("methods=%v want %v", got, want)
		}
	}
	// The nudge text arrived verbatim through the real relay, with a
	// uuid-form clientUserMessageId.
	frames := d.received()
	sub := decodeFrame(t, frames[len(frames)-1])
	params := sub["params"].(map[string]any)
	input := params["input"].([]any)
	item := input[0].(map[string]any)
	if item["text"] != "PUNK WAKE NONCE w3-real-proxy" {
		t.Fatalf("nudge text mangled through real proxy: %v", item)
	}
	if msgID, _ := params["clientUserMessageId"].(string); !uuidLike(msgID) {
		t.Fatalf("clientUserMessageId not uuid-form: %q", msgID)
	}
}

// TestCodexReapNoZombies guards against teardown leaving a zombie: force
// a hanging handshake, cancel, then verify the proxy process fully went
// away.
func TestCodexReapNoZombies(t *testing.T) {
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("no /proc, zombie check unavailable")
	}
	const tid = "thread-1"
	d := newFakeDaemon(t, daemonCfg{threadID: tid, hangInit: true})
	fp := newFakeProxy(t, d.addr())
	tr := NewCodexTransport(fakeProxyBinary(t), d.addr(), tid)
	defer func() { _ = tr.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = tr.Probe(ctx)
	fp.pidsGone(t)
}
