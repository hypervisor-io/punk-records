package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/region"
)

func deliveryHTTPFixture(t *testing.T) (*messagingRig, region.Message, region.Message) {
	t.Helper()
	g := messagingServer(t)
	var messages []region.Message
	for _, ns := range []string{"other", "team"} {
		g.register(t, ns, "alice")
		g.register(t, ns, "bob")
		rec := g.do(t, http.MethodPost, "/v1/namespaces/"+ns+"/messages", `{"sender":"alice","recipient":"bob","body":"full body","idempotency_key":"retry"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("send = %d %s", rec.Code, rec.Body)
		}
		var m region.Message
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, m)
	}
	return g, messages[0], messages[1]
}

func deliveryHTTPMessages(t *testing.T, rec *httptest.ResponseRecorder) []region.Message {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("read = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Messages []region.Message `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Messages
}

func TestMessageDeliveryHTTPLeaseSentCountAndID(t *testing.T) {
	g, decoy, m := deliveryHTTPFixture(t)
	url := "/v1/namespaces/team/messages"
	rows := deliveryHTTPMessages(t, g.do(t, http.MethodGet, url+"?agent=bob&lease_seconds=60&leased_by=hook-a", ""))
	if len(rows) != 1 || rows[0].ID != m.ID || rows[0].LeasedBy != "hook-a" {
		t.Fatalf("lease = %+v", rows)
	}
	rows = deliveryHTTPMessages(t, g.do(t, http.MethodGet, url+"?agent=bob&lease_seconds=60&leased_by=hook-b", ""))
	if len(rows) != 0 {
		t.Fatalf("duplicate delivery: %+v", rows)
	}
	for _, tc := range []struct {
		owner string
		n     int
	}{{"hook-b", 0}, {"hook-a", 1}} {
		rec := g.do(t, http.MethodPost, url+"/ack", fmt.Sprintf(`{"agent":"bob","leased_by":%q,"ids":[%q,%q]}`, tc.owner, m.ID, decoy.ID))
		var out struct{ Acked int }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || out.Acked != tc.n {
			t.Fatalf("ACK = %d %s (%v)", rec.Code, rec.Body, err)
		}
	}
	rows = deliveryHTTPMessages(t, g.do(t, http.MethodGet, url+"?agent=bob&id="+m.ID, ""))
	if len(rows) != 1 || rows[0].ID != m.ID || rows[0].Body != m.Body || rows[0].AckedAt == "" || rows[0].LeasedUntil != "" {
		t.Fatalf("ID = %+v", rows)
	}
	rows = deliveryHTTPMessages(t, g.do(t, http.MethodGet, url+"?box=sent&sender=alice", ""))
	if len(rows) != 1 || rows[0].ID != m.ID || rows[0].AckedAt == "" {
		t.Fatalf("sent = %+v", rows)
	}
	rows = deliveryHTTPMessages(t, g.do(t, http.MethodGet, "/v1/namespaces/other/messages?agent=bob&id="+m.ID, ""))
	if len(rows) != 0 {
		t.Fatalf("cross-ns ID = %+v", rows)
	}
	for _, tc := range []struct {
		ns string
		n  int
	}{{"other", 1}, {"team", 0}} {
		rec := g.do(t, http.MethodGet, "/v1/namespaces/"+tc.ns+"/messages/count?agent=bob", "")
		var out struct{ Unread int }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || out.Unread != tc.n {
			t.Fatalf("count = %d %s %v", rec.Code, rec.Body, err)
		}
	}
}

func TestMessageDeliveryHTTPBacklogAndRetry(t *testing.T) {
	g, _, m := deliveryHTTPFixture(t)
	g.region.MaxUnreadPerRecipient = 1
	url := "/v1/namespaces/team/messages"
	if rec := g.do(t, http.MethodPost, url, `{"sender":"alice","recipient":"bob","body":"new"}`); rec.Code != 429 {
		t.Fatalf("backlog = %d %s", rec.Code, rec.Body)
	}
	rec := g.do(t, http.MethodPost, url, `{"sender":"alice","recipient":"bob","body":"full body","idempotency_key":"retry"}`)
	var retry region.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &retry); err != nil || rec.Code != 201 || retry.ID != m.ID {
		t.Fatalf("retry = %d %s %v", rec.Code, rec.Body, err)
	}
	rows := deliveryHTTPMessages(t, g.do(t, http.MethodGet, "/v1/namespaces/other/messages?agent=bob", ""))
	if len(rows) != 1 {
		t.Fatalf("decoy = %+v", rows)
	}
}

func TestMessageDeliveryHTTPValidationBeforeStreaming(t *testing.T) {
	g, _, _ := deliveryHTTPFixture(t)
	for _, suffix := range []string{"?agent=%20bob", "?agent=bob&lease_seconds=-1", "?agent=bob&lease_seconds=no", "?agent=bob&lease_seconds=301&leased_by=x", "?agent=bob&lease_seconds=60", "?agent=bob&leased_by=x", "?agent=bob&box=wrong", "?agent=bob&box=sent&lease_seconds=1&leased_by=x"} {
		for _, path := range []string{"messages", "messages/events"} {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			req := httptest.NewRequest(http.MethodGet, "/v1/namespaces/team/"+path+suffix, nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			g.s.Router().ServeHTTP(rec, req)
			cancel()
			if rec.Code != 400 {
				t.Errorf("%s%s = %d, want 400", path, suffix, rec.Code)
			}
		}
	}
}

func TestMessageDeliveryHTTPAuthorization(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	for _, ns := range []string{"ns-b", "ns-a"} {
		for _, a := range []string{"alice", "bob"} {
			if err := g.region.Register(ctx, ns, a, ""); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := g.region.SendMessage(ctx, region.MessageInput{Namespace: ns, Sender: "alice", Recipient: "bob", Body: ns}); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"messages?agent=bob&id=whatever", "messages?box=sent&sender=alice", "messages/count?agent=bob", "messages?agent=bob&lease_seconds=60&leased_by=x"} {
		if rec := g.do(t, http.MethodGet, "/v1/namespaces/ns-b/"+path, "", nil); rec.Code != 403 {
			t.Fatalf("cross grant %s = %d", path, rec.Code)
		}
	}
	if err := g.az.Revoke(ctx, "alice", "ns-a", authz.OpWrite); err != nil {
		t.Fatal(err)
	}
	if rec := g.do(t, http.MethodGet, "/v1/namespaces/ns-a/messages?agent=bob&lease_seconds=60&leased_by=x", "", nil); rec.Code != 403 {
		t.Fatalf("read-only lease = %d %s", rec.Code, rec.Body)
	}
	if rec := g.do(t, http.MethodGet, "/v1/namespaces/ns-a/messages?agent=bob", "", nil); rec.Code != 200 {
		t.Fatalf("read control = %d", rec.Code)
	}
}

func TestMessageEventsIdleGrantRevocation(t *testing.T) {
	oldKeep, oldReconcile := messageKeepalive, messageReconcile
	messageKeepalive, messageReconcile = 25*time.Millisecond, time.Hour
	t.Cleanup(func() { messageKeepalive, messageReconcile = oldKeep, oldReconcile })
	g := authzBoundaryServer(t, true)
	// Older message and stream in another authorized namespace must survive
	// revoking only the target namespace, even with the same address.
	ctx := context.Background()
	if err := g.az.Grant(ctx, "alice", "ns-b", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"ns-b", "ns-a"} {
		if err := g.region.Register(ctx, ns, "bob", ""); err != nil {
			t.Fatal(err)
		}
		m, err := g.region.SendMessage(ctx, region.MessageInput{Namespace: ns, Sender: "bob", Recipient: "bob", Body: "idle control"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.region.AckMessages(ctx, ns, "bob", []string{m.ID}); err != nil {
			t.Fatal(err)
		}
	}
	decoy := openMessagingStream(t, g.s, "/v1/namespaces/ns-b/messages/events?agent=bob", g.token)
	if f, ok := decoy.nextFrame(time.Second); !ok || f.event != "inbox" {
		t.Fatal("missing control hint")
	}
	h := openMessagingStream(t, g.s, "/v1/namespaces/ns-a/messages/events?agent=bob", g.token)
	if f, ok := h.nextFrame(time.Second); !ok || f.event != "inbox" {
		t.Fatal("missing initial hint")
	}
	if err := g.az.Revoke(context.Background(), "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	if f, closed := h.closedWithin(time.Second); !closed {
		t.Fatalf("idle grant revoke did not close: %+v", f)
	}
	select {
	case <-decoy.done:
		t.Fatal("namespace-specific revocation closed decoy stream")
	case <-time.After(75 * time.Millisecond):
	}
}
