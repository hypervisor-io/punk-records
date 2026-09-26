package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/region"
)

// TestMessageStreamReadyFrame: connecting always yields an initial
// `ready` frame naming the namespace, before any traffic, so a client
// knows the stream is live.
func TestMessageStreamReadyFrame(t *testing.T) {
	g := messagingServer(t)
	g.register(t, "ns-x", "alice")

	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-x/messages/stream", "")
	f, ok := h.nextFrame(3 * time.Second)
	if !ok || f.event != "ready" {
		t.Fatalf("initial frame = %+v (ok=%v), want ready", f, ok)
	}
	if !strings.Contains(f.data, `"namespace":"ns-x"`) {
		t.Fatalf("ready data = %q", f.data)
	}
}

// TestMessageStreamMessageFrameOnSend: a send through the HTTP surface
// produces a `message` frame naming the id and recipient, never the
// body, on the namespace-wide stream.
func TestMessageStreamMessageFrameOnSend(t *testing.T) {
	g := messagingServer(t)
	g.register(t, "ns-x", "alice")
	g.register(t, "ns-x", "bob")

	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-x/messages/stream", "")
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "ready" {
		t.Fatalf("initial frame = %+v (ok=%v), want ready", f, ok)
	}

	rec := g.do(t, http.MethodPost, "/v1/namespaces/ns-x/messages",
		`{"sender":"alice","recipient":"bob","body":"secret body never broadcast"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body)
	}
	var sent region.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}

	f, ok := h.nextFrame(3 * time.Second)
	if !ok || f.event != "message" {
		t.Fatalf("message frame = %+v (ok=%v), want message", f, ok)
	}
	if strings.Contains(f.data, "secret body never broadcast") {
		t.Fatalf("message body leaked onto the namespace stream: %q", f.data)
	}
	var got struct {
		ID        string `json:"id"`
		Recipient string `json:"recipient"`
	}
	if err := json.Unmarshal([]byte(f.data), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != sent.ID || got.Recipient != "bob" {
		t.Fatalf("message frame data = %+v, want id=%s recipient=bob", got, sent.ID)
	}
}

// TestMessageStreamNoFrameForOtherNamespace: a send in another
// namespace must never reach this namespace's stream.
func TestMessageStreamNoFrameForOtherNamespace(t *testing.T) {
	g := messagingServer(t)
	g.register(t, "ns-x", "alice")
	g.register(t, "ns-y", "carol")
	g.register(t, "ns-y", "dave")

	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-x/messages/stream", "")
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "ready" {
		t.Fatalf("initial frame = %+v (ok=%v), want ready", f, ok)
	}

	rec := g.do(t, http.MethodPost, "/v1/namespaces/ns-y/messages",
		`{"sender":"carol","recipient":"dave","body":"hi dave"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body)
	}
	if f, ok := h.nextFrame(300 * time.Millisecond); ok {
		t.Fatalf("frame leaked from another namespace: %+v", f)
	}
}

// TestMessageStreamAckFrameFromHTTPAck: acknowledging through the HTTP
// surface produces an `ack` frame naming the agent and the acked ids.
func TestMessageStreamAckFrameFromHTTPAck(t *testing.T) {
	g := messagingServer(t)
	g.register(t, "ns-x", "alice")
	g.register(t, "ns-x", "bob")

	rec := g.do(t, http.MethodPost, "/v1/namespaces/ns-x/messages",
		`{"sender":"alice","recipient":"bob","body":"hi bob"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body)
	}
	var sent region.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}

	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-x/messages/stream", "")
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "ready" {
		t.Fatalf("initial frame = %+v (ok=%v), want ready", f, ok)
	}

	rec = g.do(t, http.MethodPost, "/v1/namespaces/ns-x/messages/ack",
		`{"agent":"bob","ids":["`+sent.ID+`"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("ack = %d: %s", rec.Code, rec.Body)
	}

	f, ok := h.nextFrame(3 * time.Second)
	if !ok || f.event != "ack" {
		t.Fatalf("ack frame = %+v (ok=%v), want ack", f, ok)
	}
	var got struct {
		Agent string   `json:"agent"`
		IDs   []string `json:"ids"`
	}
	if err := json.Unmarshal([]byte(f.data), &got); err != nil {
		t.Fatal(err)
	}
	if got.Agent != "bob" || len(got.IDs) != 1 || got.IDs[0] != sent.ID {
		t.Fatalf("ack frame data = %+v, want agent=bob ids=[%s]", got, sent.ID)
	}
}

// TestMessageStreamAckFrameFromDirectPublish: the stream reacts to any
// MessageAckEvent on the bus, not only ones the HTTP ack handler
// produces (the MCP ack_messages tool publishes the same way).
func TestMessageStreamAckFrameFromDirectPublish(t *testing.T) {
	g := messagingServer(t)
	g.register(t, "ns-x", "bob")

	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-x/messages/stream", "")
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "ready" {
		t.Fatalf("initial frame = %+v (ok=%v), want ready", f, ok)
	}

	g.bus.Publish(region.MessageAckEvent("ns-x", "bob", []string{"id-1", "id-2"}))

	f, ok := h.nextFrame(3 * time.Second)
	if !ok || f.event != "ack" {
		t.Fatalf("ack frame = %+v (ok=%v), want ack", f, ok)
	}
	var got struct {
		Agent string   `json:"agent"`
		IDs   []string `json:"ids"`
	}
	if err := json.Unmarshal([]byte(f.data), &got); err != nil {
		t.Fatal(err)
	}
	if got.Agent != "bob" || len(got.IDs) != 2 || got.IDs[0] != "id-1" || got.IDs[1] != "id-2" {
		t.Fatalf("ack frame data = %+v", got)
	}
}

// TestMessageStreamGrantRevocation: a revoked namespace grant stops
// delivery mid-stream; the next delivery attempt closes the stream
// instead of notifying.
func TestMessageStreamGrantRevocation(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	if err := g.region.Register(ctx, "ns-a", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := g.region.Register(ctx, "ns-a", "bob", ""); err != nil {
		t.Fatal(err)
	}

	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-a/messages/stream", g.token)
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "ready" {
		t.Fatalf("initial frame = %+v (ok=%v), want ready", f, ok)
	}
	if err := g.az.Revoke(ctx, "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(region.MessageEvent(&region.Message{Namespace: "ns-a", Recipient: "bob", ID: "m1"}))
	f, closed := h.closedWithin(3 * time.Second)
	if !closed {
		t.Fatalf("stream not closed after grant revocation (frame %+v)", f)
	}
}

// TestMessageStreamCancellation: client disconnect ends the handler
// (no leaked goroutine spinning on the bus channel).
func TestMessageStreamCancellation(t *testing.T) {
	g := messagingServer(t)
	g.register(t, "ns-x", "bob")

	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-x/messages/stream", "")
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "ready" {
		t.Fatalf("initial frame = %+v (ok=%v), want ready", f, ok)
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler still running after client cancel")
	}
}
