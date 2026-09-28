package main

// W6 final assembled END-TO-END native wake acceptance
// (cmd/punk/native_wake_integration_test.go, 2026-09-28 native-wake plan).
//
// Unlike internal/nativewake/acceptance_test.go (which locks the native
// wire contracts in isolation) and connect_wake_test.go (which verifies
// generation and fail-open), THIS file drives the assembled feature through
// real processes only - no in-process substitutions, no overlay:
//
//   actual built punk connect --wake  -> generated hooks in an isolated home
//   actual generated ensure command   -> detached runner (real built punk)
//   real disposable punk serve        -> real sqlite DB, real HTTP, real SSE
//   fake native endpoint, real wire   -> claude: fake own-session unix
//                                        socket; codex: fake daemon control
//                                        socket spoken to THROUGH THE
//                                        ACTUAL INSTALLED `codex app-server
//                                        proxy` subprocess the runner spawns;
//                                        the fake PINS the final wake wire
//                                        (thread/queue/add + experimentalApi
//                                        + UUIDv4 clientUserMessageId) and
//                                        REJECTS turn/start so the race
//                                        cannot regress
//   enqueue from a registered peer    -> SSE hint -> one native nudge
//   actual generated inbox command    -> same-session UserPromptSubmit
//                                        payload leases, delivers and ACKs
//   actual generated stop command     -> worker exits, SSE detaches,
//                                        lifecycle files cleaned
//
// Safety: every subprocess gets an isolated HOME / CODEX_HOME /
// XDG_STATE_HOME / PUNK_CREDENTIALS under t.TempDir(); the live Punk server
// on :9090 and the user's real sessions are never contacted; no secret is
// written anywhere. The codex leg requires the installed codex CLI on PATH
// (only its proxy subprocess is used; it connects to the FAKE control
// socket, never a real daemon).
//
// Protocol note for the fake codex daemon: the control socket speaks
// WebSocket (one JSON-RPC message per TEXT frame, client frames masked),
// live-verified against codex-cli 0.157.1 - see
// docs/superpowers/native-wake-acceptance.md. The daemon answers both the
// current turn/start wake method and the thread/queue/add method W3 is
// switching to, so this test stays valid across that switch.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// nativeWakeEnv is the isolated environment every punk subprocess runs
// with. PATH is inherited (the codex leg needs the installed codex CLI and
// `go build` output needs nothing else); everything identity- or
// state-bearing is redirected under the rig's temp dirs.
func nativeWakeEnv(r *nativeWakeRig) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.home,
		"USERPROFILE=" + r.home,
		"CODEX_HOME=" + r.codexHome,
		"XDG_STATE_HOME=" + r.stateHome,
		"PUNK_CREDENTIALS=" + filepath.Join(r.home, "credentials.json"),
		"PUNK_API_KEY=",
		"PUNK_URL=" + r.srvURL,
		"PUNK_MESSAGING=",
		"CLAUDE_CODE_MESSAGING_SOCKET=" + r.claudeSocket,
		"CLAUDE_CODE_MESSAGING_TOKEN=" + r.claudeToken,
	}
}

// nativeWakeRig is one fully assembled environment: a real `punk serve`
// process over a real disposable sqlite DB, plus the isolated homes the
// connect / hook subprocesses use.
type nativeWakeRig struct {
	bin          string
	root         string // repo root (fixtures, build)
	home         string
	work         string
	stateHome    string
	codexHome    string
	srvURL       string
	serveLog     string
	serveCmd     *exec.Cmd
	claudeSocket string
	claudeToken  string
}

func nativeWakeBuild(t *testing.T) (bin, root string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	root = filepath.Join(filepath.Dir(here), "..", "..")
	bin = filepath.Join(t.TempDir(), "punk")
	build := exec.Command("go", "build", "-o", bin, "./cmd/punk")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build punk: %v %s", err, out)
	}
	return bin, root
}

func newNativeWakeRig(t *testing.T) *nativeWakeRig {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unix detachment + unix sockets")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh for generated shell commands")
	}
	bin, root := nativeWakeBuild(t)

	// Reserve a loopback port for the disposable server.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	r := &nativeWakeRig{
		bin:  bin,
		root: root,
	}
	r.home = t.TempDir()
	r.work = t.TempDir()
	r.stateHome = filepath.Join(t.TempDir(), "state")
	r.codexHome = filepath.Join(t.TempDir(), "codexhome")
	r.srvURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	r.serveLog = filepath.Join(t.TempDir(), "serve.log")
	r.claudeSocket = filepath.Join(t.TempDir(), "claude-inbox.sock")
	r.claudeToken = "integ-child-token-not-a-real-secret"

	cfgDir := t.TempDir()
	cfg := filepath.Join(cfgDir, "config.yaml")
	cfgBody := fmt.Sprintf("http:\n  addr: \"127.0.0.1:%d\"\ndb:\n  driver: sqlite\n  dsn: %s\nspecs:\n  dir: %s\nauthz:\n  enforcement: \"off\"\nlog:\n  level: \"warn\"\n",
		port, filepath.ToSlash(filepath.Join(cfgDir, "punk.db")), filepath.ToSlash(filepath.Join(cfgDir, "specs")))
	if err := os.MkdirAll(filepath.Join(cfgDir, "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	env := nativeWakeEnv(r)
	run := func(args ...string) ([]byte, []byte) {
		t.Helper()
		cmd := exec.Command(r.bin, args...)
		cmd.Dir = r.work
		cmd.Env = env
		var out, errw bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errw
		if err := cmd.Run(); err != nil {
			t.Fatalf("punk %v: %v\nstdout=%s\nstderr=%s", args, err, out.String(), errw.String())
		}
		return out.Bytes(), errw.Bytes()
	}
	// NOTE: Go's flag package stops at the first non-flag argument, so the
	// --config flag MUST precede the action verb ("migrate --config X up").
	// "migrate up --config X" would silently use the cwd-relative default
	// config/DSN instead. The guard below makes any such regression fail
	// fast on the intended temp DB rather than touching a stray file.
	run("migrate", "--config", cfg, "up")
	if _, err := os.Stat(filepath.Join(cfgDir, "punk.db")); err != nil {
		t.Fatalf("migrate did not create the configured temp DB %s: %v (flag order regression?)",
			filepath.Join(cfgDir, "punk.db"), err)
	}

	logf, err := os.Create(r.serveLog)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logf.Close() }()
	serve := exec.Command(r.bin, "serve", "--config", cfg)
	serve.Dir = r.work
	serve.Env = env
	serve.Stdout = logf
	serve.Stderr = logf
	if err := serve.Start(); err != nil {
		t.Fatalf("start punk serve: %v", err)
	}
	r.serveCmd = serve
	t.Cleanup(func() {
		_ = serve.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = serve.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = serve.Process.Kill()
			<-done
		}
	})

	// Health: the members route answers on the reserved port.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(r.srvURL + "/v1/namespaces/w6-health/members")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return r
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("punk serve did not become healthy; log:\n%s", nativeWakeReadFile(t, r.serveLog))
	return nil
}

func nativeWakeReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(unreadable %s: %v)", path, err)
	}
	return string(raw)
}

// stateDirFor returns the per-session wake state dir the lifecycle uses,
// mirroring wakeSessionDir's hashed tokens (read-only introspection for
// assertions; the hashing is hookcli-internal).
func (r *nativeWakeRig) stateDirFor(client, sessionID string) string {
	token := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return "s-" + fmt.Sprintf("%x", sum[:])[:32]
	}
	return filepath.Join(r.stateHome, "punk", "wake", token(client), token(sessionID))
}

// nativeWakeCommands reads one generated hooks document and returns the
// punk wake and punk inbox command strings by event, so the test executes
// the very bytes connect wrote.
func nativeWakeCommands(t *testing.T, hooksFile string) map[string]map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s not JSON: %v", hooksFile, err)
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("no hooks object in %s: %s", hooksFile, raw)
	}
	out := map[string]map[string][]string{}
	for ev, v := range hooks {
		if out[ev] == nil {
			out[ev] = map[string][]string{}
		}
		var visit func(any)
		visit = func(x any) {
			switch n := x.(type) {
			case []any:
				for _, e := range n {
					visit(e)
				}
			case map[string]any:
				if cmd, ok := n["command"].(string); ok {
					switch {
					case strings.Contains(cmd, " hook wake "):
						out[ev]["wake"] = append(out[ev]["wake"], cmd)
					case strings.Contains(cmd, " hook inbox "):
						out[ev]["inbox"] = append(out[ev]["inbox"], cmd)
					}
				}
				if nested, ok := n["hooks"]; ok {
					visit(nested)
				}
			}
		}
		visit(v)
	}
	return out
}

// nativeWakePayload builds one native hook payload from the recorded
// testdata fixture with the event, session id and cwd overridden.
func nativeWakePayload(t *testing.T, root, client, event, sessionID, cwd string) []byte {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join(root, "internal", "hookcli", "testdata", "inbox", client+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(fixture, &p); err != nil {
		t.Fatal(err)
	}
	p["hook_event_name"] = event
	p["session_id"] = sessionID
	p["cwd"] = cwd
	if event == "UserPromptSubmit" {
		p["prompt"] = "host turn prompt carrying the punk nudge"
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// nativeWakeRunCommand executes one generated shell command with the rig's
// isolated environment and the given stdin, returning stdout/stderr.
func nativeWakeRunCommand(t *testing.T, r *nativeWakeRig, command string, stdin []byte, timeout time.Duration) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = r.work
	cmd.Env = nativeWakeEnv(r)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errw bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errw
	err := cmd.Run()
	if err != nil {
		t.Fatalf("generated command %q: %v\nstdout=%s\nstderr=%s", command, err, out.String(), errw.String())
	}
	return out.String(), errw.String()
}

func (r *nativeWakeRig) httpJSON(t *testing.T, method, path string, body any) map[string]any {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, r.srvURL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("%s %s: status %d: %s", method, path, resp.StatusCode, out)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("%s %s: decode: %v", method, path, err)
	}
	return decoded
}

// fakeClaudeInboxInteg is the Claude own-session inbox stand-in: it accepts
// ANY number of connections (the runner probes with a bare dial-and-close
// and wakes with auth+user frames), recording every complete line.
type fakeClaudeInboxInteg struct {
	listener net.Listener
	mu       sync.Mutex
	lines    []string
}

func newFakeClaudeInboxInteg(t *testing.T, path string) *fakeClaudeInboxInteg {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeClaudeInboxInteg{listener: l}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				rd := bufio.NewReader(conn)
				for {
					line, err := rd.ReadString('\n')
					if line != "" {
						f.mu.Lock()
						f.lines = append(f.lines, strings.TrimRight(line, "\n"))
						f.mu.Unlock()
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return f
}

func (f *fakeClaudeInboxInteg) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

// fakeCodexDaemonInteg is the codex shared-daemon control-socket stand-in.
// It speaks the live-verified WebSocket-over-UDS protocol and pins the
// FINAL wake wire contract: initialize MUST declare
// capabilities.experimentalApi=true, and the nudge MUST be submitted via
// thread/queue/add with a UUIDv4 clientUserMessageId. turn/start is
// REJECTED and recorded as a violation - after the steering-race review
// the production transport must never send it, and this test fails loudly
// if it ever does, so the integrated regression cannot reintroduce the
// idle/start race.
type fakeCodexDaemonInteg struct {
	listener net.Listener
	threadID string
	mu       sync.Mutex
	nudges   []string
	methods  []string
	// turnStartSeen records any turn/start attempt (must stay false).
	turnStartSeen bool
	// experimentalDeclared records the initialize capability declaration.
	experimentalDeclared bool
	// queueMsgIDs records every accepted clientUserMessageId.
	queueMsgIDs []string
}

func newFakeCodexDaemonInteg(t *testing.T, path, threadID string) *fakeCodexDaemonInteg {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeCodexDaemonInteg{listener: l, threadID: threadID}
	t.Cleanup(func() { _ = l.Close() })
	go d.serve()
	return d
}

func (d *fakeCodexDaemonInteg) record(method string, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.methods = append(d.methods, method)
	if text != "" {
		d.nudges = append(d.nudges, text)
	}
}

func (d *fakeCodexDaemonInteg) nudgesSeen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.nudges...)
}

func (d *fakeCodexDaemonInteg) methodsSeen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.methods...)
}

func (d *fakeCodexDaemonInteg) violations() (turnStart bool, experimental bool, msgIDs []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.turnStartSeen, d.experimentalDeclared, append([]string(nil), d.queueMsgIDs...)
}

// uuidV4RE matches exactly the UUIDv4 textual form the production
// transport mints (newClientUserMessageID: version 4, variant 10).
var uuidV4RE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func (d *fakeCodexDaemonInteg) serve() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		go d.serveConn(conn)
	}
}

func (d *fakeCodexDaemonInteg) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	rd, err := wsServerHandshakeInteg(conn)
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(120 * time.Second))
	respond := func(id any, result map[string]any) {
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		_ = wsWriteServerTextInteg(conn, payload)
	}
	for {
		payload, err := wsReadFrameInteg(rd)
		if err != nil || payload == nil {
			return
		}
		var msg map[string]any
		if err := json.Unmarshal(payload, &msg); err != nil {
			return
		}
		method, _ := msg["method"].(string)
		params, _ := msg["params"].(map[string]any)
		if params == nil {
			params = map[string]any{}
		}
		nudgeText := func() string {
			input, _ := params["input"].([]any)
			if len(input) == 0 {
				return ""
			}
			first, _ := input[0].(map[string]any)
			s, _ := first["text"].(string)
			return s
		}
		reject := func(id any, code int, message string) {
			payload, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"error": map[string]any{"code": code, "message": message},
			})
			_ = wsWriteServerTextInteg(conn, payload)
		}
		switch method {
		case "initialize":
			d.record(method, "")
			// The experimental gate: the live daemon rejects gated methods
			// unless initialize declared capabilities.experimentalApi; the
			// fake records the declaration and refuses a regression.
			caps, _ := params["capabilities"].(map[string]any)
			exp, _ := caps["experimentalApi"].(bool)
			d.mu.Lock()
			d.experimentalDeclared = exp
			d.mu.Unlock()
			if !exp {
				reject(msg["id"], -32600, "Invalid request: thread/queue/add requires capabilities.experimentalApi")
				continue
			}
			respond(msg["id"], map[string]any{
				"userAgent":      "codex-tui/0.157.1 (w6-integ-fake-daemon)",
				"codexHome":      "/tmp/w6-integ-codex-home",
				"platformFamily": "unix",
				"platformOs":     "linux",
			})
		case "initialized":
			d.record(method, "")
			// Notification: no response (live-verified).
		case "thread/loaded/list":
			d.record(method, "")
			respond(msg["id"], map[string]any{"data": []string{d.threadID}, "nextCursor": nil})
		case "thread/resume":
			d.record(method, "")
			respond(msg["id"], map[string]any{
				"thread": map[string]any{
					"id":     d.threadID,
					"status": map[string]any{"type": "idle"},
				},
			})
		case "turn/start":
			// Forbidden wake method: turn/start on a thread that turned
			// active after the status read STEERS the in-flight human turn
			// (the idle/start race, live-reproduced 2026-09-28 10:12). The
			// final transport must submit via thread/queue/add only.
			d.record(method, "")
			d.mu.Lock()
			d.turnStartSeen = true
			d.mu.Unlock()
			reject(msg["id"], -32601, "turn/start is not a wake method; use thread/queue/add")
		case "thread/queue/add":
			// clientUserMessageId is REQUIRED on the real daemon and must
			// be a UUIDv4 (the production transport mints a fresh
			// crypto-random one per invocation).
			id, _ := params["clientUserMessageId"].(string)
			if id == "" {
				reject(msg["id"], -32600, "Invalid request: missing field `clientUserMessageId`")
				continue
			}
			if !uuidV4RE.MatchString(id) {
				reject(msg["id"], -32600, "Invalid request: clientUserMessageId must be a UUIDv4")
				continue
			}
			d.mu.Lock()
			d.queueMsgIDs = append(d.queueMsgIDs, id)
			d.mu.Unlock()
			d.record(method, nudgeText())
			respond(msg["id"], map[string]any{
				"queuedSubmission": map[string]any{"id": "queued-w6-integ-1"},
			})
		default:
			reject(msg["id"], -32601, "Method not found")
		}
	}
}

// ---- minimal RFC 6455 helpers (test-only) -------------------------------

func wsAcceptKeyInteg(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// wsServerHandshakeInteg performs the server upgrade and returns the
// connection's bufio.Reader (frames already buffered stay readable).
func wsServerHandshakeInteg(conn net.Conn) (*bufio.Reader, error) {
	rd := bufio.NewReader(conn)
	req, err := http.ReadRequest(rd)
	if err != nil {
		return nil, err
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAcceptKeyInteg(key) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		return nil, err
	}
	return rd, nil
}

// wsWriteServerTextInteg writes one unmasked server text frame.
func wsWriteServerTextInteg(w io.Writer, payload []byte) error {
	header := []byte{0x81}
	if n := len(payload); n < 126 {
		header = append(header, byte(n))
	} else if n < 1<<16 {
		header = append(header, 126, byte(n>>8), byte(n))
	} else {
		header = append(header, 127)
		for shift := 56; shift >= 0; shift -= 8 {
			header = append(header, byte(n>>shift))
		}
	}
	_, err := w.Write(append(header, payload...))
	return err
}

// wsReadFrameInteg reads one frame, unmasking client frames; non-text
// frames yield (nil, nil).
func wsReadFrameInteg(r io.Reader) ([]byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	opcode := head[0] & 0x0F
	n := int64(head[1] & 0x7F)
	var ext [8]byte
	switch n {
	case 126:
		if _, err := io.ReadFull(r, ext[:2]); err != nil {
			return nil, err
		}
		n = int64(ext[0])<<8 | int64(ext[1])
	case 127:
		if _, err := io.ReadFull(r, ext[:8]); err != nil {
			return nil, err
		}
		for i := 0; i < 8; i++ {
			n = n<<8 | int64(ext[i])
		}
	}
	var mask [4]byte
	hasMask := head[1]&0x80 != 0
	if hasMask {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return nil, err
		}
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	if hasMask {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	if opcode != 1 {
		return nil, nil
	}
	return payload, nil
}

// ---- the assembled acceptance -------------------------------------------

// nativeWakeAssembled runs the whole bridge for one client end to end and
// asserts every hand-off along the way.
func nativeWakeAssembled(t *testing.T, client string) {
	r := newNativeWakeRig(t)

	// 1. ACTUAL installed connect --wake: generated hooks in the isolated
	//    home, using the built punk binary against the disposable server.
	connect := exec.Command(r.bin, "connect", client, "--wake", "--no-mcp", "--no-skill", "--url", r.srvURL)
	connect.Dir = r.work
	connect.Env = nativeWakeEnv(r)
	if out, err := connect.CombinedOutput(); err != nil {
		t.Fatalf("punk connect %s --wake: %v\n%s", client, err, out)
	}
	hooksFile := filepath.Join(r.home, ".claude", "settings.json")
	if client == "codex" {
		hooksFile = filepath.Join(r.codexHome, "hooks.json")
	}
	cmds := nativeWakeCommands(t, hooksFile)
	if len(cmds["SessionStart"]["wake"]) != 1 || !strings.Contains(cmds["SessionStart"]["wake"][0], "--action ensure") {
		t.Fatalf("SessionStart wake commands = %v", cmds["SessionStart"])
	}
	if len(cmds["SessionEnd"]["wake"]) != 1 || !strings.Contains(cmds["SessionEnd"]["wake"][0], "--action stop") {
		t.Fatalf("SessionEnd wake commands = %v", cmds["SessionEnd"])
	}
	if len(cmds["UserPromptSubmit"]["inbox"]) == 0 {
		t.Fatalf("UserPromptSubmit inbox commands = %v", cmds["UserPromptSubmit"])
	}
	ensureCmd := cmds["SessionStart"]["wake"][0]
	stopCmd := cmds["SessionEnd"]["wake"][0]
	inboxCmd := cmds["UserPromptSubmit"]["inbox"][0]

	// The namespace the hooks resolve for the workspace cwd (no git remote
	// in a temp dir, so the path-derived namespace is deterministic).
	ns, _ := hookcli.ProjectNamespace(r.work)

	// 2. Fake native endpoints on the real wire. The codex leg is reached
	//    THROUGH the actual installed `codex app-server proxy` the runner
	//    spawns (the proxy dials this fake control socket).
	var (
		claudeInbox *fakeClaudeInboxInteg
		codexDaemon *fakeCodexDaemonInteg
	)
	sessionID := "w6-claude-integ-session-1"
	if client == "codex" {
		if _, err := exec.LookPath("codex"); err != nil {
			t.Skip("installed codex CLI not on PATH")
		}
		sessionID = "01a0e5c9-310a-71b2-a865-4ea46cf32db8"
		codexDaemon = newFakeCodexDaemonInteg(t,
			filepath.Join(r.codexHome, "app-server-control", "app-server-control.sock"), sessionID)
	} else {
		claudeInbox = newFakeClaudeInboxInteg(t, r.claudeSocket)
	}
	address := client + ":" + sessionID

	// 3. ACTUAL generated ensure command -> detached runner (a real punk
	//    process) registering and streaming SSE against the real server.
	nativeWakeRunCommand(t, r, ensureCmd,
		nativeWakePayload(t, r.root, client, "SessionStart", sessionID, r.work), 30*time.Second)

	// The runner takes a moment to register and open its stream; the
	// member list must show the wake address listening.
	listening := func() bool {
		out := r.httpJSON(t, http.MethodGet, "/v1/namespaces/"+ns+"/members", nil)
		list, _ := out["members"].([]any)
		for _, m := range list {
			if mm, ok := m.(map[string]any); ok && mm["agent"] == address {
				lv, _ := mm["listening"].(bool)
				return lv
			}
		}
		return false
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if listening() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !listening() {
		t.Fatalf("wake listener %s never opened its SSE stream (members not listening); serve log:\n%s",
			address, nativeWakeReadFile(t, r.serveLog))
	}

	// The worker PID is recorded (never signalled) for liveness assertions.
	var workerPID int
	if raw := nativeWakeReadFile(t, filepath.Join(r.stateDirFor(client, sessionID), "listener.json")); raw != "" {
		var rec struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal([]byte(raw), &rec) == nil {
			workerPID = rec.PID
		}
	}
	if workerPID == 0 {
		t.Fatalf("no listener record with a pid under %s", r.stateDirFor(client, sessionID))
	}
	alive := func(pid int) bool {
		return pid > 0 && syscall.Kill(pid, 0) == nil
	}
	if !alive(workerPID) {
		t.Fatalf("recorded worker pid %d is not alive", workerPID)
	}

	// 4. A registered peer enqueues one durable message for the session.
	r.httpJSON(t, http.MethodPost, "/v1/namespaces/"+ns+"/members",
		map[string]string{"agent": "opencode:peer", "role": "peer"})
	sent := r.httpJSON(t, http.MethodPost, "/v1/namespaces/"+ns+"/messages", map[string]string{
		"sender":    "opencode:peer",
		"recipient": address,
		"body":      "integ peer body: assembled wake acceptance",
	})
	msgID, _ := sent["id"].(string)
	if msgID == "" {
		t.Fatalf("enqueue returned no id: %v", sent)
	}

	// 5. The assembled bridge must deliver ONE native nudge on the real
	//    wire: enqueue -> SSE hint -> runner -> native transport. The nudge
	//    is runner-authored (namespace + address + read instructions), so
	//    it must name both and never the peer body.
	nudgeArrived := func() bool {
		if client == "codex" {
			for _, n := range codexDaemon.nudgesSeen() {
				if strings.Contains(n, ns) && strings.Contains(n, address) && !strings.Contains(n, "integ peer body") {
					return true
				}
			}
			return false
		}
		for _, line := range claudeInbox.received() {
			var frame map[string]any
			if json.Unmarshal([]byte(line), &frame) != nil {
				continue
			}
			if frame["type"] != "user" {
				continue
			}
			msg, _ := frame["message"].(map[string]any)
			content, _ := msg["content"].(string)
			if strings.Contains(content, ns) && strings.Contains(content, address) && !strings.Contains(content, "integ peer body") {
				return true
			}
		}
		return false
	}
	deadline = time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if nudgeArrived() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !nudgeArrived() {
		dump := ""
		if client == "codex" {
			dump = "daemon methods: " + fmt.Sprint(codexDaemon.nudgesSeen())
		} else {
			dump = "inbox lines: " + fmt.Sprint(claudeInbox.received())
		}
		stateDir := r.stateDirFor(client, sessionID)
		t.Fatalf("no native nudge reached the fake %s endpoint within 45s (%s); worker log:\n%s\nserve log tail:\n%s",
			client, dump,
			nativeWakeReadFile(t, filepath.Join(stateDir, "gen"))+"\n(search generation dirs for worker.log)",
			nativeWakeReadFile(t, r.serveLog))
	}

	// The codex leg must have gone through the ACTUAL installed `codex
	// app-server proxy` subprocess the runner spawns: the fake control
	// socket only ever speaks the live-verified WebSocket JSON-RPC
	// sequence, so these methods can only have arrived over that wire.
	if client == "codex" {
		seen := strings.Join(codexDaemon.methodsSeen(), ",")
		for _, want := range []string{"initialize", "initialized", "thread/loaded/list", "thread/resume"} {
			if !strings.Contains(seen, want) {
				t.Fatalf("fake daemon never saw %q; methods seen: %s", want, seen)
			}
		}
		if !strings.Contains(seen, "thread/queue/add") {
			t.Fatalf("fake daemon saw no thread/queue/add wake submission; methods seen: %s", seen)
		}
		// NEGATIVE assertion - the race pin: turn/start must NEVER appear
		// on the wake wire. A regression to turn/start reintroduces the
		// idle/start steering race (live-reproduced 2026-09-28 10:12).
		turnStart, experimental, msgIDs := codexDaemon.violations()
		if turnStart {
			t.Fatalf("REGRESSION: the runner submitted turn/start (steering race); only thread/queue/add is allowed. Methods: %s", seen)
		}
		// The final wire contract: initialize declared
		// capabilities.experimentalApi (the gate thread/queue/add needs).
		if !experimental {
			t.Fatal("REGRESSION: initialize did not declare capabilities.experimentalApi=true; thread/queue/add is experimental-gated")
		}
		// Every accepted submission carried a fresh UUIDv4
		// clientUserMessageId (the production mints one per invocation).
		if len(msgIDs) == 0 {
			t.Fatal("no accepted thread/queue/add submission recorded")
		}
		for _, id := range msgIDs {
			if !uuidV4RE.MatchString(id) {
				t.Fatalf("clientUserMessageId %q is not a UUIDv4", id)
			}
		}
	}

	// The runner never leases or ACKs: the message must still be unread.
	if out := r.httpJSON(t, http.MethodGet, "/v1/namespaces/"+ns+"/messages/count?agent="+address, nil); out["unread"] != float64(1) {
		t.Fatalf("unread count after nudge = %v, want 1 (runner must not ACK)", out)
	}

	// 6. ACTUAL generated inbox command on the SAME session's
	//    UserPromptSubmit: the host, woken by the nudge, runs its inbox
	//    hook which leases, delivers and ACKs the message.
	stdout, _ := nativeWakeRunCommand(t, r, inboxCmd,
		nativeWakePayload(t, r.root, client, "UserPromptSubmit", sessionID, r.work), 30*time.Second)
	if !strings.Contains(stdout, "integ peer body: assembled wake acceptance") {
		t.Fatalf("generated inbox hook did not deliver the peer body; stdout:\n%s", stdout)
	}
	if out := r.httpJSON(t, http.MethodGet, "/v1/namespaces/"+ns+"/messages/count?agent="+address, nil); out["unread"] != float64(0) {
		t.Fatalf("unread count after generated inbox delivery = %v, want 0 (hook must ACK)", out)
	}

	// 7. ACTUAL generated stop command (SessionEnd): the worker exits, the
	//    SSE stream detaches, the lifecycle files are cleaned.
	nativeWakeRunCommand(t, r, stopCmd,
		nativeWakePayload(t, r.root, client, "SessionEnd", sessionID, r.work), 30*time.Second)
	deadline = time.Now().Add(15 * time.Second)
	stopped := func() bool {
		return !alive(workerPID) && !listening()
	}
	for time.Now().Before(deadline) {
		if stopped() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !stopped() {
		t.Fatalf("stop did not clean up: worker alive=%v listening=%v; worker log:\n%s",
			alive(workerPID), listening(), nativeWakeReadFile(t, r.serveLog))
	}
	if _, err := os.Stat(filepath.Join(r.stateDirFor(client, sessionID), "listener.json")); !os.IsNotExist(err) {
		t.Fatalf("listener record survived stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.stateDirFor(client, sessionID), "current")); !os.IsNotExist(err) {
		t.Fatalf("current-generation pointer survived stop: %v", err)
	}
}

func TestNativeWakeAssembledClaudeCode(t *testing.T) {
	nativeWakeAssembled(t, "claude-code")
}

func TestNativeWakeAssembledCodex(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("requires the installed codex CLI (only its app-server proxy is spawned)")
	}
	nativeWakeAssembled(t, "codex")
}
