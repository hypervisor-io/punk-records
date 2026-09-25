package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// messagingRig wires the messaging surface with no auth (unit tests):
// region store plus the in-process bus the SSE stream hints from.
type messagingRig struct {
	s      *Server
	region *region.Store
	bus    *bus.Bus
}

func messagingServer(t *testing.T) *messagingRig {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "msg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	clk := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clk = clk.Add(time.Millisecond)
		return clk
	}
	rig := &messagingRig{region: region.New(db, now), bus: bus.New()}
	rig.s = New(testLogger(), Deps{Region: rig.region, Bus: rig.bus})
	return rig
}

func (g *messagingRig) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	g.s.Router().ServeHTTP(rec, req)
	return rec
}

func (g *messagingRig) register(t *testing.T, ns, agent string) {
	t.Helper()
	rec := g.do(t, http.MethodPost, "/v1/namespaces/"+ns+"/members", `{"agent":"`+agent+`","role":"test"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("register %s@%s = %d: %s", agent, ns, rec.Code, rec.Body)
	}
}

// openMessagingStream starts an SSE request against any Server and
// reuses the A02 stream drain helpers (same package).
func openMessagingStream(t *testing.T, s *Server, path, token string) *streamHandle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	pr, pw := io.Pipe()
	rec := &streamRecorder{ResponseRecorder: httptest.NewRecorder(), w: pw}
	done := make(chan struct{})
	go func() {
		s.Router().ServeHTTP(rec, req)
		_ = pw.Close()
		close(done)
	}()
	h := &streamHandle{frames: make(chan sseFrame, 64), cancel: cancel, done: done}
	go func() {
		defer close(h.frames)
		r := bufio.NewReader(pr)
		for {
			ev, data, err := readFrameOrEOF(r)
			h.frames <- sseFrame{event: ev, data: data, err: err}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return h
}

func TestMessagingMembersRegisterAndList(t *testing.T) {
	g := messagingServer(t)

	rec := g.do(t, http.MethodPost, "/v1/namespaces/ns-x/members", `{"agent":"alice","role":"worker"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Namespace string `json:"namespace"`
		Agent     string `json:"agent"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Namespace != "ns-x" || out.Agent != "alice" || out.Status != "registered" {
		t.Fatalf("register body = %+v", out)
	}

	// Registration is idempotent (re-join updates role).
	rec = g.do(t, http.MethodPost, "/v1/namespaces/ns-x/members", `{"agent":"alice","role":"worker2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-register = %d: %s", rec.Code, rec.Body)
	}

	rec = g.do(t, http.MethodGet, "/v1/namespaces/ns-x/members", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("members = %d: %s", rec.Code, rec.Body)
	}
	var list struct {
		Members []region.Member `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Members) != 1 || list.Members[0].Agent != "alice" {
		t.Fatalf("members = %+v", list.Members)
	}

	// Validation: agent is required.
	rec = g.do(t, http.MethodPost, "/v1/namespaces/ns-x/members", `{"role":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("register without agent = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestMessagingSendReadAckRoundtrip(t *testing.T) {
	g := messagingServer(t)
	g.register(t, "ns-x", "alice")
	g.register(t, "ns-x", "bob")

	rec := g.do(t, http.MethodPost, "/v1/namespaces/ns-x/messages",
		`{"sender":"alice","recipient":"bob","body":"hi bob","idempotency_key":"k1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body)
	}
	var msg region.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.ID == "" || msg.Namespace != "ns-x" || msg.Sender != "alice" || msg.Recipient != "bob" ||
		msg.Body != "hi bob" || msg.CreatedAt == "" || msg.AckedAt != "" {
		t.Fatalf("message = %+v", msg)
	}

	// Threaded reply referencing the first message.
	rec = g.do(t, http.MethodPost, "/v1/namespaces/ns-x/messages",
		fmt.Sprintf(`{"sender":"bob","recipient":"alice","body":"re: hi","reply_to":%q}`, msg.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("reply = %d: %s", rec.Code, rec.Body)
	}

	// Idempotent retry: same key + same payload returns the stored
	// message, no duplicate.
	rec = g.do(t, http.MethodPost, "/v1/namespaces/ns-x/messages",
		`{"sender":"alice","recipient":"bob","body":"hi bob","idempotency_key":"k1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("retry send = %d: %s", rec.Code, rec.Body)
	}
	var dup region.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &dup); err != nil {
		t.Fatal(err)
	}
	if dup.ID != msg.ID {
		t.Fatalf("idempotent retry got id %q, want %q", dup.ID, msg.ID)
	}

	// Conflicting reuse of the key with a different payload: 409.
	rec = g.do(t, http.MethodPost, "/v1/namespaces/ns-x/messages",
		`{"sender":"alice","recipient":"bob","body":"DIFFERENT","idempotency_key":"k1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflicting idempotency key = %d, want 409: %s", rec.Code, rec.Body)
	}

	read := func(agent string) []region.Message {
		t.Helper()
		rec := g.do(t, http.MethodGet, "/v1/namespaces/ns-x/messages?agent="+agent, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("read %s = %d: %s", agent, rec.Code, rec.Body)
		}
		var out struct {
			Messages []region.Message `json:"messages"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Messages
	}

	msgs := read("bob")
	if len(msgs) != 1 || msgs[0].ID != msg.ID {
		t.Fatalf("bob unread = %+v", msgs)
	}
	if got := read("alice"); len(got) != 1 {
		t.Fatalf("alice unread = %+v, want the reply", got)
	}

	// Read never acknowledges: bob's message is still there.
	if got := read("bob"); len(got) != 1 {
		t.Fatalf("read acknowledged: %+v", got)
	}

	// ACK only the supplied id, idempotently.
	rec = g.do(t, http.MethodPost, "/v1/namespaces/ns-x/messages/ack",
		fmt.Sprintf(`{"agent":"bob","ids":[%q]}`, msg.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("ack = %d: %s", rec.Code, rec.Body)
	}
	var ackOut struct {
		Acked int64 `json:"acked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ackOut); err != nil {
		t.Fatal(err)
	}
	if ackOut.Acked != 1 {
		t.Fatalf("acked = %d, want 1", ackOut.Acked)
	}
	if got := read("bob"); len(got) != 0 {
		t.Fatalf("acked message still unread: %+v", got)
	}
	rec = g.do(t, http.MethodPost, "/v1/namespaces/ns-x/messages/ack",
		fmt.Sprintf(`{"agent":"bob","ids":[%q]}`, msg.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-ack = %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ackOut); err != nil {
		t.Fatal(err)
	}
	if ackOut.Acked != 0 {
		t.Fatalf("re-ack = %d, want 0 (idempotent)", ackOut.Acked)
	}
}

func TestMessagingNamespaceIsolation(t *testing.T) {
	g := messagingServer(t)
	g.register(t, "ns-a", "alice")
	g.register(t, "ns-a", "bob")
	g.register(t, "ns-b", "alice")
	g.register(t, "ns-b", "bob")

	rec := g.do(t, http.MethodPost, "/v1/namespaces/ns-a/messages",
		`{"sender":"alice","recipient":"bob","body":"for ns-a only"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body)
	}
	rec = g.do(t, http.MethodGet, "/v1/namespaces/ns-b/messages?agent=bob", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read ns-b = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "for ns-a only") {
		t.Fatalf("ns-a message leaked into ns-b: %s", rec.Body)
	}
}

func TestMessagingValidation(t *testing.T) {
	g := messagingServer(t)
	g.register(t, "ns-x", "alice")
	g.register(t, "ns-x", "bob")

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   []int
	}{
		{"missing_recipient", http.MethodPost, "/v1/namespaces/ns-x/messages", `{"sender":"alice","body":"x"}`, []int{http.StatusBadRequest}},
		{"unregistered_recipient", http.MethodPost, "/v1/namespaces/ns-x/messages", `{"sender":"alice","recipient":"carol","body":"x"}`, []int{http.StatusBadRequest, http.StatusNotFound}},
		{"unregistered_sender", http.MethodPost, "/v1/namespaces/ns-x/messages", `{"sender":"carol","recipient":"bob","body":"x"}`, []int{http.StatusBadRequest, http.StatusNotFound}},
		{"oversize_body", http.MethodPost, "/v1/namespaces/ns-x/messages", `{"sender":"alice","recipient":"bob","body":"` + strings.Repeat("x", 16*1024+1) + `"}`, []int{http.StatusBadRequest}},
		{"reply_to_missing", http.MethodPost, "/v1/namespaces/ns-x/messages", `{"sender":"alice","recipient":"bob","body":"x","reply_to":"no-such-id"}`, []int{http.StatusBadRequest, http.StatusNotFound}},
		{"body_namespace_mismatch", http.MethodPost, "/v1/namespaces/ns-x/messages", `{"namespace":"ns-y","sender":"alice","recipient":"bob","body":"x"}`, []int{http.StatusBadRequest}},
		{"read_missing_agent", http.MethodGet, "/v1/namespaces/ns-x/messages", "", []int{http.StatusBadRequest}},
		{"ack_missing_agent", http.MethodPost, "/v1/namespaces/ns-x/messages/ack", `{"ids":["m1"]}`, []int{http.StatusBadRequest}},
		{"ack_too_many_ids", http.MethodPost, "/v1/namespaces/ns-x/messages/ack", `{"agent":"bob","ids":[` + strings.Repeat(`"x",`, 100) + `"x"]}`, []int{http.StatusBadRequest}},
		{"events_missing_agent", http.MethodGet, "/v1/namespaces/ns-x/messages/events", "", []int{http.StatusBadRequest}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := g.do(t, tc.method, tc.path, tc.body)
			for _, want := range tc.want {
				if rec.Code == want {
					return
				}
			}
			t.Fatalf("%s %s = %d, want one of %v: %s", tc.method, tc.path, rec.Code, tc.want, rec.Body)
		})
	}

	// Limit is bounded, not an error: a huge limit clamps.
	rec := g.do(t, http.MethodGet, "/v1/namespaces/ns-x/messages?agent=bob&limit=99999", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read with huge limit = %d: %s", rec.Code, rec.Body)
	}
}

// TestMessagingSenderDefault: an explicit sender is the contract, but an
// empty sender resolves to the verified credential subject (the same
// identity default the MCP tools apply).
func TestMessagingSenderDefault(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	if err := g.region.Register(ctx, "ns-a", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := g.region.Register(ctx, "ns-a", "bob", ""); err != nil {
		t.Fatal(err)
	}
	rec := g.do(t, http.MethodPost, "/v1/namespaces/ns-a/messages", `{"recipient":"bob","body":"no sender field"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body)
	}
	var msg region.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Sender != "alice" {
		t.Fatalf("sender = %q, want verified subject alice", msg.Sender)
	}
}

func TestMessageEventsInitialHintAndBusWake(t *testing.T) {
	g := messagingServer(t)
	g.register(t, "ns-x", "alice")
	g.register(t, "ns-x", "bob")

	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-x/messages/events?agent=bob", "")
	f, ok := h.nextFrame(3 * time.Second)
	if !ok || f.event != "inbox" {
		t.Fatalf("initial hint = %+v (ok=%v), want inbox", f, ok)
	}
	if !strings.Contains(f.data, `"agent":"bob"`) {
		t.Fatalf("initial hint data = %q", f.data)
	}

	// A send through the HTTP surface publishes the bus hint AFTER the
	// durable write; the stream wakes without any model polling.
	rec := g.do(t, http.MethodPost, "/v1/namespaces/ns-x/messages",
		`{"sender":"alice","recipient":"bob","body":"secret body never broadcast"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body)
	}
	f, ok = h.nextFrame(3 * time.Second)
	if !ok || f.event != "inbox" {
		t.Fatalf("bus hint = %+v (ok=%v), want inbox", f, ok)
	}
	if strings.Contains(f.data, "secret body never broadcast") {
		t.Fatalf("message body leaked onto the hint stream: %q", f.data)
	}
}

// TestMessageEventsReconciliation: the bus is lossy by design, so the
// stream rechecks durable storage on a bounded timer; a message written
// without a bus event (here: direct Store.SendMessage, bypassing the
// publishing surface) still produces a hint.
func TestMessageEventsReconciliation(t *testing.T) {
	old := messageReconcile
	messageReconcile = 30 * time.Millisecond
	t.Cleanup(func() { messageReconcile = old })

	g := messagingServer(t)
	g.register(t, "ns-x", "alice")
	g.register(t, "ns-x", "bob")

	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-x/messages/events?agent=bob", "")
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "inbox" {
		t.Fatalf("initial hint = %+v (ok=%v)", f, ok)
	}

	if _, err := g.region.SendMessage(context.Background(), region.MessageInput{
		Namespace: "ns-x", Sender: "alice", Recipient: "bob", Body: "durable only",
	}); err != nil {
		t.Fatal(err)
	}
	f, ok := h.nextFrame(3 * time.Second)
	if !ok || f.event != "inbox" {
		t.Fatalf("reconciliation hint = %+v (ok=%v), want inbox", f, ok)
	}
}

// TestMessageEventsGrantRevocation: a revoked namespace grant stops
// delivery mid-stream; the next delivery attempt closes the stream
// instead of notifying.
func TestMessageEventsGrantRevocation(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	if err := g.region.Register(ctx, "ns-a", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := g.region.Register(ctx, "ns-a", "bob", ""); err != nil {
		t.Fatal(err)
	}

	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-a/messages/events?agent=bob", g.token)
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "inbox" {
		t.Fatalf("initial hint = %+v (ok=%v)", f, ok)
	}
	if err := g.az.Revoke(ctx, "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(bus.Event{Kind: region.MessageEventKind, Key: region.MessageEventKey("ns-a", "bob")})
	f, closed := h.closedWithin(3 * time.Second)
	if !closed {
		t.Fatalf("stream not closed after grant revocation (frame %+v)", f)
	}
	if f.err != io.EOF {
		t.Fatalf("stream ended with err = %v, want io.EOF (server-side close)", f.err)
	}
}

// TestMessageEventsKeyRevocationOnKeepalive: a revoked API key on an
// idle stream is caught by the keepalive tick, which revalidates the
// credential and ends the stream.
func TestMessageEventsKeyRevocationOnKeepalive(t *testing.T) {
	old := messageKeepalive
	messageKeepalive = 50 * time.Millisecond
	t.Cleanup(func() { messageKeepalive = old })

	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	if err := g.region.Register(ctx, "ns-a", "bob", ""); err != nil {
		t.Fatal(err)
	}

	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-a/messages/events?agent=bob", g.token)
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "inbox" {
		t.Fatalf("initial hint = %+v (ok=%v)", f, ok)
	}
	if err := g.keys.Revoke(ctx, "alice-key"); err != nil {
		t.Fatal(err)
	}
	f, closed := h.closedWithin(3 * time.Second)
	if !closed {
		t.Fatalf("stream not closed after key revocation on an idle stream (frame %+v)", f)
	}
	if f.err != io.EOF {
		t.Fatalf("stream ended with err = %v, want io.EOF (server-side close)", f.err)
	}
}

// TestMessageEventsCancellation: client disconnect ends the handler
// (no leaked goroutine spinning on the bus channel).
func TestMessageEventsCancellation(t *testing.T) {
	g := messagingServer(t)
	g.register(t, "ns-x", "bob")

	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-x/messages/events?agent=bob", "")
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "inbox" {
		t.Fatalf("initial hint = %+v (ok=%v)", f, ok)
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler still running after client cancel")
	}
}
