package nativewake

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// Codex transport. It wakes an existing Codex session through the shared
// app-server owning daemon by running ONLY `codex app-server proxy
// [--sock <path>]` and speaking the app-server JSON-RPC protocol as
// WEBSOCKET over the proxy's stdio: the daemon control socket is a
// WebSocket-over-Unix-socket endpoint (HTTP Upgrade handshake, one
// JSON-RPC message per WebSocket text frame, client frames masked per
// RFC 6455) and the proxy is a raw stdio<->socket byte relay that adds
// no protocol, so a client of the proxy carries the WebSocket framing
// itself (W6 live proof, openai/codex rust-v0.157.1: stdio-to-uds/src/
// lib.rs, app-server-transport/src/transport/unix_socket.rs, cli/tests/
// queue.rs). It never starts a second app-server, never creates or forks
// a thread, and never overrides thread settings: the only client requests
// are initialize, thread/loaded/list, thread/resume and thread/queue/add.
//
// JSON-RPC shapes verified against the same source (app-server-protocol
// protocol/v2/thread.rs ThreadQueueAdd{Params,Response}, schema/json/
// {ClientRequest,JSONRPCMessage}.json) and live-proven by W6: the daemon
// control socket relays through the installed codex-cli 0.157.1 proxy;
// thread/queue/add requires initialize.capabilities.experimentalApi=true
// and a clientUserMessageId, and answers queuedSubmission{id}.
//
// Outcome mapping: a thread absent from thread/loaded/list (or reporting
// status notLoaded) is ErrUnavailable; an active, errored or unrecognised
// runtime status is ErrBusy (deferral, and no queue stacking while the
// host is busy). Wake submission uses thread/queue/add, NOT turn/start:
// turn/start on a thread that became active after the status read would
// STEER the human's in-flight turn, while queue/add merely enqueues and
// the daemon auto-dispatches when the thread is idle again (W6 live
// proof of both paths). A successful queue/add is host ENQUEUE only:
// Outcome.Accepted never means the model consumed the nudge.
//
// Each Wake invocation generates one crypto-random UUID
// clientUserMessageId before submitting, so any single invocation's
// submission is identifiable upstream; the transport never retransmits
// after a submission write (the daemon queue has no client-id dedup,
// source-verified in ext/queue/src/service.rs enqueue), and the
// no-reconnect-after-write rule stands. Server-initiated requests
// (permission approvals and friends) are never answered, notifications
// are consumed without blocking, and a broken proxy is closed and
// reaped, with at most one reconnect per operation.

const (
	// codexRequestTimeout bounds one JSON-RPC request/response exchange.
	codexRequestTimeout = 15 * time.Second
	// codexHandshakeTimeout bounds the proxy spawn to WebSocket-upgrade
	// completion.
	codexHandshakeTimeout = 10 * time.Second
	// codexMaxMessage bounds one WebSocket text frame's payload; a larger
	// frame declares the proxy stream broken.
	codexMaxMessage = 1 << 20
	// codexMaxNotifications bounds notification accounting on one
	// connection; beyond it frames are still drained (so an event flood
	// can neither block request processing nor grow memory), they just
	// stop being counted.
	codexMaxNotifications = 4096
	// codexMaxLoadedPages bounds thread/loaded/list pagination.
	codexMaxLoadedPages = 16
	// codexMaxNudgeBytes bounds one nudge text.
	codexMaxNudgeBytes = 8192
	// codexReapTimeout bounds the kill-to-reap wait when closing a proxy.
	codexReapTimeout = 3 * time.Second
)

// Runtime status values from ThreadStatus (protocol/v2/thread.rs).
const (
	codexStatusIdle      = "idle"
	codexStatusNotLoaded = "notLoaded"
)

// errCodexConnDead marks a lost or broken proxy connection; withConn may
// reconnect once per operation (never after turn/start was written).
var errCodexConnDead = errors.New("codex proxy connection lost")

// NewCodexTransport returns a Transport that wakes the Codex thread with
// the given session id through the owning app-server daemon reached at
// socket (empty socket uses the daemon's default control socket). The
// proxy subprocess is started lazily on first use; nothing runs between
// calls.
func NewCodexTransport(binary, socket, sessionID string) Transport {
	return &CodexTransport{binary: binary, socket: socket, threadID: sessionID}
}

// CodexTransport implements Transport for Codex shared-daemon sessions.
type CodexTransport struct {
	binary   string
	socket   string
	threadID string

	mu     sync.Mutex
	conn   *codexConn
	closed bool
}

var _ Transport = (*CodexTransport)(nil)

// Probe reports whether the target thread is a live, attachable native
// target: proxy handshake succeeds and the thread is loaded and resumable
// in the owning daemon. A busy (active) thread still probes fine; only a
// missing daemon or an unloaded thread is ErrUnavailable.
func (t *CodexTransport) Probe(ctx context.Context) error {
	if err := t.validate(); err != nil {
		return err
	}
	return t.withConn(ctx, func(c *codexConn) (wroteSubmission bool, err error) {
		if err := t.requireLoaded(ctx, c); err != nil {
			return false, err
		}
		if _, err := t.attachThread(ctx, c); err != nil {
			return false, err
		}
		return false, nil
	})
}

// Wake hands one nudge text to the host by queueing it on the target
// thread via thread/queue/add. The queue gate still requires the thread
// to report idle at resume time (no stacking while busy), but the
// SUBMISSION is a queue add, never a turn start: if a human turn races
// in between the status read and the send, the nudge simply queues and
// the daemon auto-dispatches it when the thread returns to idle (W6 live
// proof), instead of steering the human's turn the way turn/start would.
// A successful queue/add response is host enqueue only (Outcome.Accepted,
// never model receipt).
func (t *CodexTransport) Wake(ctx context.Context, text string) (Outcome, error) {
	if err := t.validate(); err != nil {
		return Outcome{}, err
	}
	if text == "" {
		return Outcome{}, errors.New("nativewake: codex wake text is empty")
	}
	if len(text) > codexMaxNudgeBytes {
		return Outcome{}, errors.New("nativewake: codex wake text too large")
	}
	// One stable clientUserMessageId per Wake invocation, minted before
	// any connection work: whatever happens, this invocation submits at
	// most once under this id, and the transport never retransmits.
	msgID, err := newClientUserMessageID()
	if err != nil {
		return Outcome{}, fmt.Errorf("nativewake: codex message id: %w", err)
	}
	var out Outcome
	err = t.withConn(ctx, func(c *codexConn) (wroteSubmission bool, err error) {
		if err := t.requireLoaded(ctx, c); err != nil {
			return false, err
		}
		th, err := t.attachThread(ctx, c)
		if err != nil {
			return false, err
		}
		if th.Status == nil || th.Status.Type != codexStatusIdle {
			switch {
			case th.Status != nil && th.Status.Type == codexStatusNotLoaded:
				return false, fmt.Errorf("%w: thread %s not loaded", ErrUnavailable, threadToken(t.threadID))
			default:
				// active, systemError or unrecognised: defer; queueing on
				// top of a busy host would only stack nudges.
				return false, fmt.Errorf("%w: thread %s not idle", ErrBusy, threadToken(t.threadID))
			}
		}
		// thread/queue/add may reach the host from here on: every failure
		// path below returns wroteSubmission=true FIRST, so no reconnect
		// can ever retransmit the submission (the daemon queue has no
		// client-id dedup - source-verified in ext/queue/src/service.rs
		// enqueue - so a resend could duplicate the nudge).
		var res cdQueueAddResult
		err = c.request(ctx, "thread/queue/add", cdQueueAddParams{
			ThreadID:            t.threadID,
			Input:               []cdUserInput{{Type: "text", Text: text}},
			ClientUserMessageID: msgID,
		}, &res)
		if err != nil {
			return true, err
		}
		if res.QueuedSubmission == nil || res.QueuedSubmission.ID == "" {
			// Accepted/error distinction: a result without a usable
			// queuedSubmission is a broken response, not an enqueue.
			return true, fmt.Errorf("nativewake: codex thread/queue/add returned no queued submission: %w", errCodexConnDead)
		}
		out = Outcome{Accepted: true}
		return true, nil
	})
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
}

// Close terminates any live proxy subprocess (kill and reap) and makes
// later operations fail. It is idempotent.
func (t *CodexTransport) Close() error {
	t.mu.Lock()
	t.closed = true
	c := t.conn
	t.conn = nil
	t.mu.Unlock()
	if c != nil {
		c.teardown()
	}
	return nil
}

// validate checks spawn preconditions. Missing binary or socket, or an
// unusable session id, can never become a live target: ErrUnavailable.
func (t *CodexTransport) validate() error {
	if !validSessionToken(t.threadID) {
		return fmt.Errorf("%w: invalid codex thread id", ErrUnavailable)
	}
	if t.binary == "" {
		return fmt.Errorf("%w: codex binary is required", ErrUnavailable)
	}
	if _, err := exec.LookPath(t.binary); err != nil {
		return fmt.Errorf("%w: codex binary unusable", ErrUnavailable)
	}
	return nil
}

// withConn runs fn on a connected proxy. On a lost or hung connection it
// closes and reaps the proxy and retries at most once, and only when fn
// had not yet written its wake submission (a duplicate nudge is worse
// than a skipped retry). Caller cancellation always tears the proxy down.
func (t *CodexTransport) withConn(ctx context.Context, fn func(*codexConn) (bool, error)) error {
	for attempt := 0; ; attempt++ {
		c, err := t.dial(ctx)
		if err != nil {
			// A proxy that died mid-handshake (for example right after
			// answering initialize) gets one fresh dial; a missing
			// daemon (ErrUnavailable) never retries.
			if errors.Is(err, errCodexConnDead) && attempt == 0 && ctx.Err() == nil {
				continue
			}
			return err
		}
		wroteTurn, err := fn(c)
		if err == nil {
			return nil
		}
		dead := errors.Is(err, errCodexConnDead)
		if dead || ctx.Err() != nil {
			// The connection is broken or the caller is gone: close and
			// reap the proxy we own.
			t.dropConn(c)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !wroteTurn && attempt == 0 {
				continue // one bounded reconnect per operation
			}
		}
		return err
	}
}

// dial returns a handshake-complete connection, reusing a live one.
func (t *CodexTransport) dial(ctx context.Context) (*codexConn, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("nativewake: codex transport closed")
	}
	if t.conn != nil && t.conn.alive() {
		return t.conn, nil
	}
	if t.conn != nil {
		t.conn.teardown()
		t.conn = nil
	}
	c, err := startCodexProxy(ctx, t.binary, t.socket)
	if err != nil {
		return nil, err
	}
	t.conn = c
	// Protocol handshake: initialize request, then the initialized
	// notification (the server rejects everything before this on a
	// connection). The WebSocket upgrade already happened in
	// startCodexProxy, so output existed; a failure here is a broken or
	// hung protocol handshake, not a missing daemon.
	var init struct {
		UserAgent string `json:"userAgent"`
	}
	if err := c.request(ctx, "initialize", cdInitializeParams{
		ClientInfo:   cdClientInfo{Name: "punk-native-wake", Title: "Punk Records wake listener", Version: "1"},
		Capabilities: &cdCapabilities{ExperimentalAPI: true},
	}, &init); err != nil {
		t.conn = nil
		if errors.Is(err, errCodexConnDead) {
			c.teardown()
			return nil, fmt.Errorf("nativewake: codex proxy unusable during handshake: %w", errCodexConnDead)
		}
		c.teardown()
		return nil, err
	}
	if init.UserAgent == "" {
		t.conn = nil
		c.teardown()
		return nil, fmt.Errorf("nativewake: codex initialize returned no user agent: %w", errCodexConnDead)
	}
	if err := c.notify("initialized"); err != nil {
		t.conn = nil
		c.teardown()
		return nil, err
	}
	return c, nil
}

// dropConn forgets c if it is still the current connection and reaps it.
func (t *CodexTransport) dropConn(c *codexConn) {
	t.mu.Lock()
	if t.conn == c {
		t.conn = nil
	}
	t.mu.Unlock()
	c.teardown()
}

// requireLoaded insists the target thread is loaded in the owning
// daemon's memory (thread/loaded/list, bounded pagination).
func (t *CodexTransport) requireLoaded(ctx context.Context, c *codexConn) error {
	params := struct {
		Cursor *string `json:"cursor,omitempty"`
	}{}
	for page := 0; page < codexMaxLoadedPages; page++ {
		var res cdLoadedListResult
		if err := c.request(ctx, "thread/loaded/list", params, &res); err != nil {
			return err
		}
		for _, id := range res.Data {
			if id == t.threadID {
				return nil
			}
		}
		if res.NextCursor == nil || *res.NextCursor == "" {
			break
		}
		params.Cursor = res.NextCursor
	}
	return fmt.Errorf("%w: codex thread %s not loaded", ErrUnavailable, threadToken(t.threadID))
}

// attachThread resumes the thread without any override settings and
// verifies the daemon really attached the requested thread.
func (t *CodexTransport) attachThread(ctx context.Context, c *codexConn) (*cdThread, error) {
	var res cdResumeResult
	// excludeTurns only skips history hydration for this connection; it
	// is not a thread setting override.
	if err := c.request(ctx, "thread/resume", cdResumeParams{
		ThreadID:     t.threadID,
		ExcludeTurns: true,
	}, &res); err != nil {
		return nil, err
	}
	if res.Thread == nil || res.Thread.ID == "" {
		return nil, fmt.Errorf("nativewake: codex thread/resume returned no thread: %w", errCodexConnDead)
	}
	if res.Thread.ID != t.threadID {
		return nil, fmt.Errorf("nativewake: codex thread/resume returned thread %s for requested %s",
			threadToken(res.Thread.ID), threadToken(t.threadID))
	}
	return res.Thread, nil
}

// threadToken keeps thread ids machine-short for error text.
func threadToken(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ---- wire types (stable app-server protocol surface) --------------------

type cdInitializeParams struct {
	ClientInfo   cdClientInfo    `json:"clientInfo"`
	Capabilities *cdCapabilities `json:"capabilities,omitempty"`
}

// cdCapabilities opts into the experimental app-server surface;
// thread/queue/add is experimental-gated (message_processor.rs rejects
// gated methods unless experimentalApi was declared at initialize).
type cdCapabilities struct {
	ExperimentalAPI bool `json:"experimentalApi"`
}

type cdClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type cdLoadedListResult struct {
	Data       []string `json:"data"`
	NextCursor *string  `json:"nextCursor"`
}

type cdResumeParams struct {
	ThreadID     string `json:"threadId"`
	ExcludeTurns bool   `json:"excludeTurns"`
}

type cdThread struct {
	ID     string          `json:"id"`
	Status *cdThreadStatus `json:"status"`
}

type cdThreadStatus struct {
	Type string `json:"type"`
}

type cdResumeResult struct {
	Thread *cdThread `json:"thread"`
}

type cdUserInput struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// cdQueueAddParams is ThreadQueueAddParams (protocol/v2/thread.rs):
// threadId, input and the REQUIRED clientUserMessageId (the live daemon
// answers -32600 without it).
type cdQueueAddParams struct {
	ThreadID            string        `json:"threadId"`
	Input               []cdUserInput `json:"input"`
	ClientUserMessageID string        `json:"clientUserMessageId"`
}

// cdQueueAddResult is ThreadQueueAddResponse: queuedSubmission{id, ...}.
type cdQueueAddResult struct {
	QueuedSubmission *cdQueuedSubmission `json:"queuedSubmission"`
}

type cdQueuedSubmission struct {
	ID string `json:"id"`
}

// newClientUserMessageID mints the per-Wake-invocation
// clientUserMessageId: a crypto-random UUID (RFC 4122 v4 form, matching
// the daemon's own uuid-string convention for client ids; the server
// accepts any non-empty string and only fills one in when absent).
func newClientUserMessageID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ---- WebSocket-over-proxy connection --------------------------------------

// wireFrame is the undecoded JSON-RPC envelope. A frame with method and
// no id is a notification; method plus id is a server-initiated request;
// id alone is a response to one of ours.
type wireFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *wireRPCError   `json:"error"`
}

type wireRPCError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *wireRPCError) Error() string {
	return "codex rpc error " + strconv.FormatInt(e.Code, 10)
}

type wireRequest struct {
	ID     int64  `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// startCodexProxy launches `binary app-server proxy [--sock socket]` (it
// never spawns app-server proper) and completes the WebSocket client
// handshake over the relay's stdio, which is what the daemon control
// socket speaks on the other end.
func startCodexProxy(ctx context.Context, binary, socket string) (*codexConn, error) {
	args := []string{"app-server", "proxy"}
	if socket != "" {
		args = append(args, "--sock", socket)
	}
	cmd := exec.Command(binary, args...)
	codexSetProcAttr(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Proxy diagnostics stay private; they never reach punk output.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: codex proxy start failed", ErrUnavailable)
	}
	c := &codexConn{
		cmd:    cmd,
		stdin:  stdin,
		done:   make(chan struct{}),
		waited: make(chan struct{}),
	}
	// Any byte ever read from the proxy (the WebSocket handshake
	// response included) proves the daemon side produced output.
	c.stdoutSeen = &seenReader{r: stdout, mark: c.markOutput}
	rwc := &stdioConn{r: c.stdoutSeen, w: stdin, closeWrite: stdin.Close}
	go c.reapLoop()

	ws, err := dialWS(ctx, rwc)
	if err != nil {
		// The frame reader never started: tell the reaper it may reap.
		c.finish()
		if ctx.Err() != nil {
			c.teardown()
			return nil, ctx.Err()
		}
		if errors.Is(err, errCodexConnDead) {
			// A proxy that exited by itself without producing any output
			// is a missing daemon/socket (ErrUnavailable, the run
			// terminates); anything else is a broken or hung handshake.
			if c.selfExitedNoOutput() {
				return nil, fmt.Errorf("%w: codex proxy exited before handshake", ErrUnavailable)
			}
			c.teardown()
			return nil, fmt.Errorf("nativewake: codex proxy unusable during handshake: %w", errCodexConnDead)
		}
		c.teardown()
		return nil, err
	}
	c.ws = ws
	go c.readLoop()
	return c, nil
}

// dialWS performs the RFC 6455 client handshake over rwc with a hard
// deadline, so a hung relay cannot wedge an operation.
func dialWS(ctx context.Context, rwc io.ReadWriteCloser) (*websocket.Conn, error) {
	cfg, err := websocket.NewConfig("ws://codex-app-server/", "http://punk-records.local/")
	if err != nil {
		return nil, err
	}
	type result struct {
		ws  *websocket.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ws, err := websocket.NewClient(cfg, rwc)
		ch <- result{ws, err}
	}()
	ctx, cancel := context.WithTimeout(ctx, codexHandshakeTimeout)
	defer cancel()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("nativewake: codex websocket handshake: %w: %v", errCodexConnDead, r.err)
		}
		return r.ws, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// stdioConn adapts the proxy's stdin/stdout pipes to the
// io.ReadWriteCloser the WebSocket client dials over: reads observe
// stdout (through the seenReader), writes go to stdin, Close half-closes
// stdin.
type stdioConn struct {
	r          io.Reader
	w          io.Writer
	closeWrite func() error
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *stdioConn) Close() error                { return c.closeWrite() }

// seenReader marks on the first successful read, proving the proxy
// produced output (used to tell a dead daemon from a broken stream).
type seenReader struct {
	r    io.Reader
	mark func()
	seen bool
}

func (s *seenReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 && !s.seen {
		s.seen = true
		s.mark()
	}
	return n, err
}

// codexConn is one live proxy subprocess and its WebSocket reader.
type codexConn struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	ws         *websocket.Conn
	stdoutSeen *seenReader

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan *wireFrame
	nextID  int64

	notifyCount int

	done        chan struct{} // reader finished (or never started)
	doneOnce    sync.Once
	waited      chan struct{} // process reaped
	teardownOne sync.Once
	gotOutput   bool // any proxy stdout byte was ever read
	exitState   int  // 0 unknown, 1 exited non-zero, 2 exited zero
	waitErr     error
}

func (c *codexConn) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// selfExitedNoOutput reports a proxy that terminated on its own before
// writing any protocol output: the owning daemon or its socket is not
// there. A process torn down by us or one that produced (even broken)
// output does not qualify.
func (c *codexConn) selfExitedNoOutput() bool {
	// Give the reaper a moment to observe a self-exit; a live process
	// will simply not report within the window.
	select {
	case <-c.waited:
	case <-time.After(time.Second):
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.gotOutput && c.exitState == 1
}

// readLoop consumes every WebSocket text frame. Notifications and
// server-initiated requests are dropped (never answered, so no permission
// is ever approved from here); responses are routed to their waiter.
// Malformed or oversized frames declare the connection broken.
func (c *codexConn) readLoop() {
	defer c.finish()
	for {
		var data []byte
		if err := websocket.Message.Receive(c.ws, &data); err != nil {
			return
		}
		if len(data) > codexMaxMessage {
			// Oversized frame: broken stream.
			return
		}
		var f wireFrame
		if err := json.Unmarshal(data, &f); err != nil {
			// Broken stream: stop routing, let waiters see a dead conn.
			return
		}
		switch {
		case f.Method != "" && len(f.ID) == 0:
			// Notifications are consumed and dropped; the saturated
			// counter keeps accounting bounded under event floods while
			// the stream keeps draining (responses still route).
			c.mu.Lock()
			if c.notifyCount < codexMaxNotifications {
				c.notifyCount++
			}
			c.mu.Unlock()
		case f.Method != "":
			// Server-initiated request (approvals, tool callbacks...).
			// Deliberately never answered: denying or approving on the
			// user's behalf is not this listener's job.
		default:
			if ch := c.takePending(string(f.ID)); ch != nil {
				select {
				case ch <- &f:
				default:
				}
			}
		}
	}
}

// finish closes the done channel exactly once: no frame will ever be
// delivered on this connection again, so the reaper may collect the
// process.
func (c *codexConn) finish() { c.doneOnce.Do(func() { close(c.done) }) }

func (c *codexConn) markOutput() {
	c.mu.Lock()
	c.gotOutput = true
	c.mu.Unlock()
}

// reapLoop waits for the reader to finish, then reaps the process so no
// zombie is left behind even if teardown is never called.
func (c *codexConn) reapLoop() {
	<-c.done
	c.waitErr = c.cmd.Wait()
	c.mu.Lock()
	if c.waitErr != nil {
		c.exitState = 1
	} else if c.cmd.ProcessState != nil {
		c.exitState = 2
	}
	c.mu.Unlock()
	close(c.waited)
}

// teardown closes stdin, kills the proxy and waits (bounded) for the
// reap. Idempotent and safe during live operations.
func (c *codexConn) teardown() {
	c.teardownOne.Do(func() {
		_ = c.stdin.Close()
		codexKill(c.cmd)
	})
	select {
	case <-c.waited:
	case <-time.After(codexReapTimeout):
	}
}

func (c *codexConn) takePending(key string) chan *wireFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.pending[key]
	delete(c.pending, key)
	return ch
}

// writeMessage sends one JSON-RPC message as a single masked WebSocket
// frame. The payload MUST go out as a text frame (string payload: the
// Message codec maps []byte to binary frames, which the codex daemon's
// text-only WebSocket protocol does not accept). A failed write means
// the pipe (and with it the connection) is gone.
func (c *codexConn) writeMessage(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := websocket.Message.Send(c.ws, string(raw)); err != nil {
		return fmt.Errorf("nativewake: codex proxy write: %w: %v", errCodexConnDead, err)
	}
	return nil
}

// notify sends a notification (no id, no response expected).
func (c *codexConn) notify(method string) error {
	return c.writeMessage(map[string]string{"method": method})
}

// request sends one JSON-RPC request and waits for its response, bounded
// by codexRequestTimeout. Errors carrying errCodexConnDead mean the
// connection is unusable (EOF, broken frame, write failure or timeout).
func (c *codexConn) request(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	if c.pending == nil {
		c.pending = make(map[string]chan *wireFrame)
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	ch := make(chan *wireFrame, 1)
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()

	if err := c.writeMessage(wireRequest{ID: id, Method: method, Params: params}); err != nil {
		return err
	}

	tctx, cancel := context.WithTimeout(ctx, codexRequestTimeout)
	defer cancel()
	select {
	case f := <-ch:
		if f.Error != nil {
			return f.Error
		}
		if out != nil {
			if err := json.Unmarshal(f.Result, out); err != nil {
				return fmt.Errorf("nativewake: codex %s result undecodable: %w: %v", method, errCodexConnDead, err)
			}
		}
		return nil
	case <-c.done:
		return fmt.Errorf("nativewake: codex proxy stream ended during %s: %w", method, errCodexConnDead)
	case <-tctx.Done():
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("nativewake: codex %s timed out: %w", method, errCodexConnDead)
	}
}
