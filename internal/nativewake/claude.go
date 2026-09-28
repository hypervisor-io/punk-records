package nativewake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Claude transport. It wakes an existing Claude Code session by posting to
// the session's OWN messaging inbox socket (the endpoint the host exports to
// its hooks as CLAUDE_CODE_MESSAGING_SOCKET with CLAUDE_CODE_MESSAGING_TOKEN
// alongside). The wire is newline-delimited JSON: an optional first auth
// line {"type":"auth","token":<token>} followed by the user frame
// {"type":"user","message":{"role":"user","content":<nudge>}}. Frame forms
// and timings are live-verified against the installed Claude Code 2.1.283
// (W6, docs/superpowers/native-wake-acceptance.md; official docs
// code.claude.com/docs/en/cross-session-messaging; corroborated by
// anthropics/claude-code issue 93720 and the binary's own inject recipe).
//
// Security and privacy rules this transport enforces:
//
//   - The socket path and token NEVER appear in errors, logs, diagnostics,
//     argv or persisted state. Go's net.OpError embeds the unix path, so
//     every dial/write failure is collapsed to a static generic message;
//     only context errors (which carry no paths) pass through verbatim.
//   - The socket path must be absolute and must resolve to an actual unix
//     socket; a symlink is refused rather than followed, so a swapped path
//     cannot redirect the token to an attacker-chosen endpoint.
//   - The nudge text is authored by the runner (never a peer body) and is
//     JSON-encoded, so quotes/newlines/control bytes cannot inject extra
//     frames or terminal sequences.
//
// Outcome mapping: a complete write of both frames is host handoff only
// (Outcome.Unconfirmed: the host acknowledged nothing, the model certainly
// has not replied). A short or failed write is ambiguous - the peer may
// have received a prefix - so it is a generic retryable error that consumes
// wake budget, never ErrUnavailable. ErrUnavailable is returned only for a
// definite missing target: invalid/missing/non-socket path, or a refused or
// vanished listener. Unsupported platforms fail open with an explicit
// unsupported error.

const (
	// claudeWriteTimeout bounds the frame write when the caller's context
	// has no deadline of its own. The real inbox closes a connection that
	// sends no complete line within 30s; this stays well under that.
	claudeWriteTimeout = 15 * time.Second
	// claudeMaxNudgeBytes bounds one nudge text (same bound as codex).
	claudeMaxNudgeBytes = 8192
	// claudeMaxFrameBytes bounds the marshaled auth+user payload as a
	// sanity cap on a single write.
	claudeMaxFrameBytes = claudeMaxNudgeBytes + 4096
)

// errClaudeClosed reports use of a closed transport.
var errClaudeClosed = errors.New("nativewake: claude transport closed")

// NewClaudeTransport returns a Transport that posts wake nudges to the
// Claude Code own-session inbox at socket, authenticating with token (the
// session's CLAUDE_CODE_MESSAGING_TOKEN). Each Probe/Wake opens a fresh
// short-lived connection, matching the host's "open the connection only
// when the message you are posting is ready" contract; nothing is held
// open between calls.
func NewClaudeTransport(socket, token string) Transport {
	return &ClaudeTransport{socket: socket, token: token}
}

// ClaudeTransport implements Transport for Claude Code own-session inboxes.
type ClaudeTransport struct {
	socket string
	token  string

	mu     sync.Mutex
	closed bool
}

var _ Transport = (*ClaudeTransport)(nil)

// Probe verifies the target exists and is reachable without posting
// anything: the path must be an actual unix socket (not a symlink, not a
// regular file) and a dial must succeed. The connection is closed
// immediately; no frame is ever written during Probe.
func (t *ClaudeTransport) Probe(ctx context.Context) error {
	if err := t.checkOpen(); err != nil {
		return err
	}
	conn, err := claudeDial(ctx, t.socket)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Wake hands one nudge to the host by writing the auth line (when a token
// is configured) and the user frame, fully, on a fresh bounded connection.
// A complete write returns Outcome{Unconfirmed: true}: bytes handed to the
// socket, nothing more proven.
func (t *ClaudeTransport) Wake(ctx context.Context, text string) (Outcome, error) {
	if err := t.checkOpen(); err != nil {
		return Outcome{}, err
	}
	if text == "" {
		return Outcome{}, errors.New("nativewake: claude wake text is empty")
	}
	if len(text) > claudeMaxNudgeBytes {
		return Outcome{}, errors.New("nativewake: claude wake text too large")
	}
	payload, err := claudeFrames(t.token, text)
	if err != nil {
		return Outcome{}, err
	}
	conn, err := claudeDial(ctx, t.socket)
	if err != nil {
		return Outcome{}, err
	}
	// The connection dies with the call: cancellation via AfterFunc
	// unblocks an in-flight write, and the write is always
	// deadline-bounded so a wedged peer cannot hang the runner.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() { _ = conn.Close() }()
	if err := claudePost(ctx, conn, payload); err != nil {
		return Outcome{}, err
	}
	return Outcome{Unconfirmed: true}, nil
}

// Close marks the transport closed. It never blocks and is idempotent;
// because connections are per-call there is nothing else to tear down.
func (t *ClaudeTransport) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}

func (t *ClaudeTransport) checkOpen() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errClaudeClosed
	}
	return nil
}

// claudeFrames builds the exact wire payload: optional auth line then the
// user frame, each terminated by a newline. JSON encoding is the escaping
// boundary; the peer never receives a raw nudge byte.
func claudeFrames(token, text string) ([]byte, error) {
	var payload []byte
	if token != "" {
		auth, err := json.Marshal(map[string]string{"type": "auth", "token": token})
		if err != nil {
			return nil, errors.New("nativewake: claude auth frame build failed")
		}
		payload = append(append(payload, auth...), '\n')
	}
	user, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	})
	if err != nil {
		return nil, errors.New("nativewake: claude user frame build failed")
	}
	payload = append(append(payload, user...), '\n')
	if len(payload) > claudeMaxFrameBytes {
		return nil, errors.New("nativewake: claude wake payload too large")
	}
	return payload, nil
}

// claudePost writes payload fully to conn, bounded by the context deadline
// (or claudeWriteTimeout when the context has none). A short write or any
// write error is a generic ambiguous failure: the peer may hold a prefix,
// so the caller must treat it as budget-consuming and never as "no target".
// Underlying network errors embed the socket path and are deliberately
// dropped; only ctx.Err() (path-free) is returned verbatim.
func claudePost(ctx context.Context, conn net.Conn, payload []byte) error {
	deadline := time.Now().Add(claudeWriteTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetWriteDeadline(deadline)
	total := 0
	for total < len(payload) {
		n, err := conn.Write(payload[total:])
		total += n
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return errors.New("nativewake: claude wake write failed")
		}
		if n == 0 {
			return errors.New("nativewake: claude wake write failed")
		}
	}
	return nil
}

// genericUnavailable is the only unavailable signal; it never names the
// path or hints at the cause class beyond "no live target".
func genericUnavailable() error {
	return fmt.Errorf("%w: claude wake target not reachable", ErrUnavailable)
}
