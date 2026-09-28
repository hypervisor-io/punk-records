// Package nativewake_test holds the W6 independent acceptance layer for the
// native wake bridge. It is deliberately an EXTERNAL test package: it must
// not duplicate W1-W5 unit tests, and it must keep proving the wire contracts
// even while transports are refactored underneath.
//
// What these tests lock down, and where each fact came from:
//
//   - The Claude own-session inbox socket protocol (auth line + user frame)
//     was verified live on the installed Claude Code 2.1.283 binary and
//     corroborated by official docs (code.claude.com/docs/en/cross-session-
//     messaging): the binary itself logs the exact recipe at inbox start:
//     echo '{"type":"auth","token":"..."}'; echo '{"type":"user","message":
//     {"role":"user","content":"hello"}}' | socat - UNIX-CONNECT:<sock>.
//     The auth line is optional on Linux; a connection that sends no complete
//     line within 30s is closed. An own-child post to an idle session starts
//     a new turn that runs the normal UserPromptSubmit hook path.
//
//   - The Codex 0.157.1 daemon control socket speaks WEBSOCKET over the
//     Unix socket (HTTP Upgrade handshake, one JSON-RPC message per WS text
//     frame, client frames masked per RFC 6455), NOT JSONL; `codex
//     app-server proxy` is a raw stdio<->socket byte relay, so a client of
//     the proxy must produce and consume WebSocket framing on its stdio.
//     Verified live against the installed codex-cli 0.157.1 and source at
//     rust-v0.157.1 (app-server-transport/src/transport/unix_socket.rs,
//     stdio-to-uds/src/lib.rs, cli/tests/queue.rs).
//
//   - The JSON-RPC method sequence and response shapes for the wake flow
//     (the production W3 transport's path): initialize with
//     capabilities.experimentalApi=true (queue methods are experimental-
//     gated) -> initialized -> thread/loaded/list (hook session_id must be
//     among the loaded thread ids) -> thread/resume {threadId,
//     excludeTurns} returning thread.id == thread.sessionId == the hook
//     session_id (root-thread identity) with status idle ->
//     thread/queue/add {threadId, input:[{type:"text",...}],
//     clientUserMessageId} (clientUserMessageId is REQUIRED: the live
//     daemon answers -32600 "Invalid request: missing field
//     `clientUserMessageId`" without it) -> queuedSubmission{id}. Wake
//     submits via queue/add, never turn/start: turn/start on a thread that
//     turned active after the status read would STEER the human's turn,
//     while queue/add enqueues and the daemon auto-dispatches on idle
//     (both paths live-proven; queue selected for the final architecture).
//
// The live evidence itself (real codex TUI in an isolated CODEX_HOME with a
// mock Responses-API model; real claude in an isolated temp workspace) is
// recorded in docs/superpowers/native-wake-acceptance.md. These hermetic
// tests exist so the verified contracts stay executable after the fact.
package nativewake_test

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/api"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/nativewake"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// wakeRig is a real punk router over a real sqlite DB on a real httptest
// listener - the acceptance gate the spec demands for the enqueue -> SSE ->
// nudge -> separate delivery chain. No live server, live DB or live Punk
// instance is ever touched: everything lives under t.TempDir().
type wakeRig struct {
	ts *httptest.Server
}

func newWakeRig(t *testing.T) *wakeRig {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "wake.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg := region.New(db, time.Now)
	srv := api.New(slog.New(slog.DiscardHandler), api.Deps{Region: reg, Bus: bus.New()})
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return &wakeRig{ts: ts}
}

func (r *wakeRig) url(format string, args ...any) string {
	return r.ts.URL + fmt.Sprintf(format, args...)
}

// postJSON is a small helper for the JSON POSTs the wake flow uses.
func postJSON(t *testing.T, url string, body any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("POST %s: status %d: %s", url, resp.StatusCode, out)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("POST %s: decode: %v", url, err)
	}
	return decoded
}

// getJSON is a small helper for the JSON GETs the wake flow uses.
func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("GET %s: status %d: %s", url, resp.StatusCode, out)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("GET %s: decode: %v", url, err)
	}
	return decoded
}

// fakeClaudeInbox is a stand-in for one Claude Code session's own messaging
// socket, implementing exactly the protocol verified against the installed
// 2.1.283 binary: newline-delimited JSON frames where the first line may be
// {"type":"auth","token":...} (optional on Linux) and the wake payload is
// {"type":"user","message":{"role":"user","content":...}}. It records every
// line it received and closes after the client half-closes, mirroring the
// real inbox's close-on-EOF behavior. The client under test is the
// PRODUCTION W2 transport (nativewake.NewClaudeTransport): this fake stays
// deliberately hand-rolled so the acceptance layer shares no code with the
// transport it locks down.
type fakeClaudeInbox struct {
	listener net.Listener
	mu       sync.Mutex
	lines    []string
	ready    chan struct{}
}

func newFakeClaudeInbox(t *testing.T) *fakeClaudeInbox {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeClaudeInbox{listener: l, ready: make(chan struct{})}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		close(f.ready)
		// Accept in a loop: the production transport opens one fresh
		// connection per operation (Probe and Wake each dial), matching
		// the real inbox's one-connection-per-post behavior.
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				// The real inbox closes a connection that sends no complete
				// line within 30s; the fake keeps the same shape but a
				// test-friendly deadline so a stuck client fails fast
				// instead of hanging.
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
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

func (f *fakeClaudeInbox) addr() string { return f.listener.Addr().String() }

func (f *fakeClaudeInbox) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

// TestAcceptanceClaudeWakeOverRealRouter is the spec's acceptance bullet 1
// for the Claude leg: enqueue on a REAL punk HTTP router -> SSE hint -> ONE
// native nudge to a fake own-session socket -> the message stays unread and
// unchanged until a SEPARATE inbox delivery ACKs it. The runner never leases
// or acknowledges; only the separate delivery step does.
func TestAcceptanceClaudeWakeOverRealRouter(t *testing.T) {
	rig := newWakeRig(t)
	inbox := newFakeClaudeInbox(t)
	<-inbox.ready

	ns := "wake-accept-claude"
	wakeAddr := "claude:wake-session-1"

	// The wake listener registers its address, exactly like nativewake.Run;
	// the peer that will enqueue the nudge registers too (the router only
	// accepts sends from registered members).
	postJSON(t, rig.url("/v1/namespaces/%s/members", ns),
		map[string]string{"agent": wakeAddr, "role": "claude-code wake listener"})
	postJSON(t, rig.url("/v1/namespaces/%s/members", ns),
		map[string]string{"agent": "opencode:peer", "role": "peer"})

	// Subscribe the addressed SSE hint stream BEFORE enqueuing. The request
	// carries a cancelable context so the test never leaves the stream (and
	// the httptest server's Close) waiting on an open connection.
	type sseHit struct{}
	hits := make(chan sseHit, 8)
	sseCtx, sseCancel := context.WithCancel(context.Background())
	t.Cleanup(sseCancel)
	req, err := http.NewRequestWithContext(sseCtx, http.MethodGet,
		rig.url("/v1/namespaces/%s/messages/events?agent=%s", ns, wakeAddr), nil)
	if err != nil {
		t.Fatal(err)
	}
	sseDone := make(chan error, 1)
	go func() {
		resp, err := rig.ts.Client().Do(req)
		if err != nil {
			sseDone <- err
			return
		}
		defer func() { _ = resp.Body.Close() }()
		rd := bufio.NewReader(resp.Body)
		for {
			line, err := rd.ReadString('\n')
			if strings.HasPrefix(line, "event: inbox") {
				hits <- sseHit{}
			}
			if err != nil {
				sseDone <- nil
				return
			}
		}
	}()

	// Let the stream connect (and emit its initial backlog hint).
	select {
	case <-hits:
	case <-time.After(5 * time.Second):
		t.Fatal("no initial SSE hint")
	}

	// A peer enqueues a durable message for the wake address.
	sent := postJSON(t, rig.url("/v1/namespaces/%s/messages", ns), map[string]string{
		"sender":    "opencode:peer",
		"recipient": wakeAddr,
		"body":      "peer body: check the schema migration",
	})
	msgID, _ := sent["id"].(string)
	if msgID == "" {
		t.Fatalf("send returned no id: %v", sent)
	}

	// The router must surface the enqueue as an SSE inbox hint.
	select {
	case <-hits:
	case <-time.After(5 * time.Second):
		t.Fatal("no SSE hint after enqueue")
	}

	// ONE native nudge through the PRODUCTION W2 transport: probe the
	// own-session socket, then post the verified frames with the session's
	// exported token. Unconfirmed handoff only - the host acknowledged
	// nothing and the message below must stay unread.
	const token = "session-child-token-value"
	const nudge = "punk wake notification: read your punk inbox"
	tr := nativewake.NewClaudeTransport(inbox.addr(), token)
	defer func() { _ = tr.Close() }()
	if err := tr.Probe(context.Background()); err != nil {
		t.Fatalf("claude transport probe: %v", err)
	}
	out, err := tr.Wake(context.Background(), nudge)
	if err != nil {
		t.Fatalf("claude transport wake: %v", err)
	}
	if out.Accepted || !out.Unconfirmed {
		t.Fatalf("wake outcome = %+v, want unconfirmed handoff only", out)
	}

	// The fake inbox must have received exactly the verified frames:
	// optional auth line first, then the user message frame. The server
	// reader runs asynchronously, so wait for both lines to land.
	var got []string
	for deadline := time.Now().Add(5 * time.Second); ; {
		got = inbox.received()
		if len(got) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(got) != 2 {
		t.Fatalf("inbox received %d lines, want 2 (auth, user): %q", len(got), got)
	}
	var authFrame map[string]any
	if err := json.Unmarshal([]byte(got[0]), &authFrame); err != nil {
		t.Fatalf("auth line not JSON: %v", err)
	}
	if authFrame["type"] != "auth" || authFrame["token"] != token {
		t.Fatalf("auth frame mismatch: %v", authFrame)
	}
	var userFrame map[string]any
	if err := json.Unmarshal([]byte(got[1]), &userFrame); err != nil {
		t.Fatalf("user line not JSON: %v", err)
	}
	if userFrame["type"] != "user" {
		t.Fatalf("user frame type mismatch: %v", userFrame)
	}
	msg, _ := userFrame["message"].(map[string]any)
	if msg == nil || msg["role"] != "user" || msg["content"] != nudge {
		t.Fatalf("user frame message mismatch: %v", userFrame)
	}

	// The nudge is handoff, not delivery: the message must still be unread,
	// body unchanged, because the runner never ACKs.
	unread := getJSON(t, rig.url("/v1/namespaces/%s/messages/count?agent=%s", ns, wakeAddr))
	if v, _ := unread["unread"].(float64); v != 1 {
		t.Fatalf("unread count after nudge = %v, want 1 (runner must not ACK)", unread)
	}

	// SEPARATE inbox delivery (what the real Claude session's own hooks do
	// later): read the full message, verify the body, then ACK.
	gotMsgs := getJSON(t, rig.url("/v1/namespaces/%s/messages?agent=%s&limit=25", ns, wakeAddr))
	list, _ := gotMsgs["messages"].([]any)
	if len(list) != 1 {
		t.Fatalf("inbox read returned %d messages, want 1", len(list))
	}
	first, _ := list[0].(map[string]any)
	if first["body"] != "peer body: check the schema migration" {
		t.Fatalf("body changed before separate delivery: %v", first["body"])
	}
	acked := postJSON(t, rig.url("/v1/namespaces/%s/messages/ack", ns),
		map[string]any{"agent": wakeAddr, "ids": []string{msgID}})
	if v, _ := acked["acked"].(float64); v != 1 {
		t.Fatalf("ack result = %v, want 1", acked)
	}
	// After the separate ACK the inbox is empty.
	unread = getJSON(t, rig.url("/v1/namespaces/%s/messages/count?agent=%s", ns, wakeAddr))
	if v, _ := unread["unread"].(float64); v != 0 {
		t.Fatalf("unread count after separate ACK = %v, want 0", unread)
	}
}

// --- Minimal WebSocket server framing (RFC 6455 subset) -------------------
//
// The codex daemon control socket is a WebSocket server over a Unix socket,
// and `codex app-server proxy` is a raw byte relay, so the fake daemon must
// speak WebSocket on its side of the relay. These helpers implement exactly
// the server subset: HTTP Upgrade handshake, one text frame per message,
// server frames unmasked (client frames masked and enforced below only by
// the production client library; this fake stays deliberately hand-rolled
// so the acceptance layer does not share code with the transport under
// test).

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func wsAcceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// wsReadFrame reads one frame and returns text payloads (opcode 1).
func wsReadFrame(r io.Reader) ([]byte, error) {
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
		return nil, nil // non-text (close/ping): ignored by this subset
	}
	return payload, nil
}

// wsServerHandshake performs the server side of the upgrade on conn and
// returns the bufio.Reader wrapping it so the caller keeps any frames the
// handshake read may have already buffered (a second, separate Reader would
// silently lose them).
func wsServerHandshake(conn net.Conn) (*bufio.Reader, error) {
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
		"Sec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		return nil, err
	}
	return rd, nil
}

// fakeCodexDaemon is a stand-in for the codex 0.157.1 shared daemon's
// control socket, speaking EXACTLY the protocol verified live: WebSocket
// over UDS; initialize -> result; initialized -> nothing; thread/loaded/
// list -> {data:[threadIDs]}; thread/resume answers a root thread whose id
// equals its sessionId (the hook session_id mapping verified live) with
// status idle; turn/start -> {turn:{id,status inProgress}}. It records
// every JSON-RPC message received so the test can lock the exact wire the
// production transport produces.
type fakeCodexDaemon struct {
	listener net.Listener
	// seen records every full JSON-RPC payload in arrival order.
	mu   sync.Mutex
	seen []string
	// conns counts accepted connections.
	conns int
	// experimentalOK mirrors the daemon's connection gate: queue methods
	// are rejected unless initialize declared capabilities.experimentalApi
	// (message_processor.rs). The fake enforces it so a transport
	// regression that forgets the capability fails these tests.
	experimentalOK bool
	// queuedWhileActive counts queue/add calls that arrived while the
	// simulated host was already active (the idle->send race path).
	queuedWhileActive int
	// hostActiveAfterResume simulates a human turn starting right after
	// the resume response reported idle.
	hostActiveAfterResume bool
	threadID              string
}

func newFakeCodexDaemon(t *testing.T, threadID string) *fakeCodexDaemon {
	t.Helper()
	return newFakeCodexDaemonCfg(t, fakeDaemonCfg{threadID: threadID})
}

// fakeDaemonCfg configures the acceptance fake; zero values select the
// happy wake path.
type fakeDaemonCfg struct {
	threadID              string
	hostActiveAfterResume bool
}

func newFakeCodexDaemonCfg(t *testing.T, cfg fakeDaemonCfg) *fakeCodexDaemon {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app-server-control.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeCodexDaemon{
		listener:              l,
		threadID:              cfg.threadID,
		hostActiveAfterResume: cfg.hostActiveAfterResume,
	}
	t.Cleanup(func() { _ = l.Close() })
	go d.serve()
	return d
}

func (d *fakeCodexDaemon) addr() string { return d.listener.Addr().String() }

func (d *fakeCodexDaemon) record(payload string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, payload)
}

// payloads returns every received JSON-RPC payload in order.
func (d *fakeCodexDaemon) payloads() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// methodsOf returns the method names of the received requests and
// notifications, in order.
func (d *fakeCodexDaemon) methodsOf(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, p := range d.payloads() {
		var msg map[string]any
		if err := json.Unmarshal([]byte(p), &msg); err != nil {
			t.Fatalf("fake daemon received non-JSON payload %q: %v", p, err)
		}
		if m, _ := msg["method"].(string); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// lastParamsOf returns the params of the last received call to method.
func (d *fakeCodexDaemon) lastParamsOf(t *testing.T, method string) map[string]any {
	t.Helper()
	var params map[string]any
	for _, p := range d.payloads() {
		var msg map[string]any
		if err := json.Unmarshal([]byte(p), &msg); err != nil {
			t.Fatal(err)
		}
		if m, _ := msg["method"].(string); m == method {
			params, _ = msg["params"].(map[string]any)
		}
	}
	if params == nil {
		t.Fatalf("no %s call recorded", method)
	}
	return params
}

func (d *fakeCodexDaemon) connCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conns
}

func (d *fakeCodexDaemon) queuedWhileActiveCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.queuedWhileActive
}

func (d *fakeCodexDaemon) serve() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		d.mu.Lock()
		d.conns++
		d.mu.Unlock()
		go d.serveConn(conn)
	}
}

func (d *fakeCodexDaemon) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	rd, err := wsServerHandshake(conn)
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	respond := func(id any, result map[string]any) {
		payload, _ := json.Marshal(map[string]any{"id": id, "result": result})
		// Server frames are unmasked: build the header manually.
		header := []byte{0x81}
		if n := len(payload); n < 126 {
			header = append(header, byte(n))
		} else {
			header = append(header, 126, byte(n>>8), byte(n))
		}
		_, _ = conn.Write(append(header, payload...))
	}
	respondErr := func(id any, code int, message string) {
		payload, _ := json.Marshal(map[string]any{
			"id":    id,
			"error": map[string]any{"code": code, "message": message},
		})
		header := []byte{0x81, byte(len(payload))}
		_, _ = conn.Write(append(header, payload...))
	}
	for {
		payload, err := wsReadFrame(rd)
		if err != nil || payload == nil {
			return
		}
		d.record(string(payload))
		var msg map[string]any
		if err := json.Unmarshal(payload, &msg); err != nil {
			return
		}
		method, _ := msg["method"].(string)
		params, _ := msg["params"].(map[string]any)
		if params == nil {
			params = map[string]any{}
		}
		switch method {
		case "initialize":
			// The real daemon records capabilities per connection and
			// rejects experimental methods unless experimentalApi was
			// declared here (message_processor.rs).
			caps, _ := params["capabilities"].(map[string]any)
			d.mu.Lock()
			d.experimentalOK = caps != nil && caps["experimentalApi"] == true
			d.mu.Unlock()
			respond(msg["id"], map[string]any{
				"userAgent":      "codex-tui/0.157.1 (fake-daemon)",
				"codexHome":      "/tmp/fake-codex-home",
				"platformFamily": "unix",
				"platformOs":     "linux",
			})
		case "initialized":
			// Notification: no response, verified live.
		case "thread/loaded/list":
			respond(msg["id"], map[string]any{
				"data":       []string{"stale-thread-1", d.threadID},
				"nextCursor": nil,
			})
		case "thread/resume":
			// Root-thread identity as verified live: id == sessionId ==
			// the hook session_id, idle and ready for a queued nudge.
			respond(msg["id"], map[string]any{
				"thread": map[string]any{
					"id":        d.threadID,
					"sessionId": d.threadID,
					"status":    map[string]any{"type": "idle"},
				},
			})
		case "thread/queue/add":
			// The live daemon rejects a missing clientUserMessageId with
			// -32600 (W6 live proof, serde-enforced); the fake enforces
			// the identical rule so a client regression fails the test.
			if id, _ := params["clientUserMessageId"].(string); id == "" {
				respondErr(msg["id"], -32600, "Invalid request: missing field `clientUserMessageId`")
				continue
			}
			// Experimental gate exactly as message_processor.rs applies
			// it for queue methods.
			if !d.experimentalOK {
				respondErr(msg["id"], -32600, "thread/queue/add requires experimentalApi capability")
				continue
			}
			if d.hostActiveAfterResume {
				// The human turn won the race after the idle status was
				// read: the submission still queues (auto-dispatch on
				// idle), never steers.
				d.mu.Lock()
				d.queuedWhileActive++
				d.mu.Unlock()
			}
			respond(msg["id"], map[string]any{
				"queuedSubmission": map[string]any{
					"id":                  "queued-accept-1",
					"input":               params["input"],
					"clientUserMessageId": params["clientUserMessageId"],
				},
			})
		case "turn/start":
			// The wake transport must never send this; recorded so the
			// assertions can prove it never happens.
			respond(msg["id"], map[string]any{
				"turn": map[string]any{"id": "turn-accept-1", "status": "inProgress"},
			})
		default:
			payload, _ := json.Marshal(map[string]any{
				"id":    msg["id"],
				"error": map[string]any{"code": -32601, "message": "Method not found"},
			})
			header := []byte{0x81, byte(len(payload))}
			_, _ = conn.Write(append(header, payload...))
		}
	}
}

// TestAcceptanceCodexWakeProtocolThroughRealProxy locks the verified codex
// 0.157.1 wake protocol through the PRODUCTION W3 transport and the REAL
// installed `codex app-server proxy` binary: the transport's WebSocket
// client must pass byte-for-byte through the real raw relay into the fake
// daemon control socket, complete initialize with the experimentalApi
// capability queue/add needs + initialized, require the hook session_id in
// thread/loaded/list, resume it without override settings (id ==
// sessionId == session_id, status idle), and hand the nudge to the host
// via thread/queue/add - receiving a usable queuedSubmission back, the
// host enqueue. No live user socket, daemon or app-server process is
// involved; the proxy only relays to the test's fake endpoint.
func TestAcceptanceCodexWakeProtocolThroughRealProxy(t *testing.T) {
	codexBin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex CLI not installed; real-proxy acceptance skipped")
	}
	const threadID = "01a0e5c9-310a-71b2-a865-4ea46cf32db8"
	daemon := newFakeCodexDaemon(t, threadID)

	tr := nativewake.NewCodexTransport(codexBin, daemon.addr(), threadID)
	defer func() { _ = tr.Close() }()

	// Probe: loaded-thread gate + resume identity must pass.
	if err := tr.Probe(context.Background()); err != nil {
		t.Fatalf("probe through real proxy: %v", err)
	}
	// Wake: the nudge enqueues via thread/queue/add on the idle thread.
	const nudge = "punk wake notification: read your punk inbox"
	out, err := tr.Wake(context.Background(), nudge)
	if err != nil || !out.Accepted {
		t.Fatalf("wake out=%+v err=%v", out, err)
	}

	// Probe and Wake reuse one proxy connection: exactly one initialize,
	// one initialized, loaded/list + resume twice, one thread/queue/add.
	want := []string{
		"initialize", "initialized",
		"thread/loaded/list", "thread/resume",
		"thread/loaded/list", "thread/resume",
		"thread/queue/add",
	}
	got := daemon.methodsOf(t)
	if len(got) != len(want) {
		t.Fatalf("daemon saw %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("daemon method order: %v, want %v", got, want)
		}
	}
	if n := daemon.connCount(); n != 1 {
		t.Fatalf("daemon connections=%d, want 1 (healthy conn is reused)", n)
	}

	// initialize must have declared the experimentalApi capability
	// (queue methods are experimental-gated; the fake daemon enforces it,
	// so a regression fails here even before this explicit assertion).
	init := daemon.lastParamsOf(t, "initialize")
	caps, _ := init["capabilities"].(map[string]any)
	if caps == nil || caps["experimentalApi"] != true {
		t.Fatalf("initialize must declare capabilities.experimentalApi=true: %v", init)
	}

	// thread/resume must carry the thread id and excludeTurns, and no
	// override settings (the live-proven no-override contract).
	resume := daemon.lastParamsOf(t, "thread/resume")
	if resume["threadId"] != threadID || resume["excludeTurns"] != true {
		t.Fatalf("resume params=%v", resume)
	}
	for _, banned := range []string{"model", "cwd", "sandbox", "approvalPolicy", "config"} {
		if _, has := resume[banned]; has {
			t.Fatalf("resume must not override %s: %v", banned, resume)
		}
	}

	// thread/queue/add must carry exactly the nudge as text input and a
	// per-invocation clientUserMessageId (uuid text form).
	queue := daemon.lastParamsOf(t, "thread/queue/add")
	if queue["threadId"] != threadID {
		t.Fatalf("queue params=%v", queue)
	}
	input, _ := queue["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("queue input=%v", input)
	}
	item, _ := input[0].(map[string]any)
	if item["type"] != "text" || item["text"] != nudge {
		t.Fatalf("nudge text mangled through real proxy: %v", item)
	}
	msgID, _ := queue["clientUserMessageId"].(string)
	if !uuidLike(msgID) {
		t.Fatalf("clientUserMessageId not uuid-form: %q", msgID)
	}
}

// TestAcceptanceCodexRaceQueuesNotSteers locks the architecture decision
// behind queue/add (review finding: idle->turn/start TOCTOU can steer a
// human prompt): the thread reported idle at resume, then a human turn
// starts before the wake submission. Through the REAL installed proxy the
// production transport must submit ONLY a thread/queue/add (enqueued for
// auto-dispatch), never a turn/start, and report the host enqueue.
func TestAcceptanceCodexRaceQueuesNotSteers(t *testing.T) {
	codexBin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex CLI not installed; real-proxy acceptance skipped")
	}
	const threadID = "01a0e5c9-310a-71b2-a865-4ea46cf32db8"
	daemon := newFakeCodexDaemonCfg(t, fakeDaemonCfg{
		threadID:              threadID,
		hostActiveAfterResume: true,
	})

	tr := nativewake.NewCodexTransport(codexBin, daemon.addr(), threadID)
	defer func() { _ = tr.Close() }()

	out, err := tr.Wake(context.Background(), "punk wake notification: read your punk inbox")
	if err != nil || !out.Accepted {
		t.Fatalf("wake out=%+v err=%v (racing human turn must queue, not fail)", out, err)
	}
	got := daemon.methodsOf(t)
	for _, m := range got {
		if m == "turn/start" {
			t.Fatalf("turn/start sent while host active (would steer the human turn): %v", got)
		}
	}
	if n := daemon.queuedWhileActiveCount(); n != 1 {
		t.Fatalf("queue/add while host active=%d, want 1", n)
	}
	if !strings.Contains(strings.Join(got, ","), "thread/queue/add") {
		t.Fatalf("no queue/add submitted: %v", got)
	}
}

// uuidLike checks the RFC 4122 text form the clientUserMessageId uses.
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func uuidLike(s string) bool { return uuidRe.MatchString(s) }
