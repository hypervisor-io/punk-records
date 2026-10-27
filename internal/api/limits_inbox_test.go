package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// openInboxStream starts an SSE request carrying optional extra headers
// (the X-Forwarded-For cases below need them), on the exact pattern of
// openMessagingStream (messaging_handlers_test.go).
func openInboxStream(t *testing.T, s *Server, path string, headers map[string]string) *streamHandle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:1234" // the acceptance's single local peer
	for k, v := range headers {
		req.Header.Set(k, v)
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

// inboxGet is a plain GET with optional extra headers, for the refused
// stream cases (the refusal must arrive before any SSE header is
// written, so it cannot go through the streaming helper).
func inboxGet(t *testing.T, s *Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:1234" // the acceptance's single local peer
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

// TestStreamCapPerInboxDistinctAddresses is P3-A rework acceptance (1):
// 20 distinct agent addresses connecting from 127.0.0.1 with no subject
// (the zero-key bootstrap) each hold a stream at once. The cap is keyed
// per inbox - subject+namespace+agent - so distinct addresses never
// share a pool, and the per-IP backstop's default (256) is far above 20.
func TestStreamCapPerInboxDistinctAddresses(t *testing.T) {
	s, _ := limitsServer(t, Limits{})
	for i := 0; i < 20; i++ {
		path := fmt.Sprintf("/v1/namespaces/ns/messages/events?agent=addr-%02d", i)
		h := openInboxStream(t, s, path, nil)
		if _, ok := h.nextFrame(2 * time.Second); !ok {
			t.Fatalf("stream for distinct address %d never opened; the cap must key per inbox, not per caller", i)
		}
	}
}

// TestStreamCapFifthStreamForSameAddressRejected is P3-A rework
// acceptance (2): the default per-inbox cap is 4 - enough for reconnect
// overlap - so the fifth concurrent stream for the SAME address is 429
// before any SSE header is written.
func TestStreamCapFifthStreamForSameAddressRejected(t *testing.T) {
	s, _ := limitsServer(t, Limits{})
	path := "/v1/namespaces/ns/messages/events?agent=bob"
	for i := 0; i < 4; i++ {
		h := openInboxStream(t, s, path, nil)
		if _, ok := h.nextFrame(2 * time.Second); !ok {
			t.Fatalf("stream %d for the same address never opened", i+1)
		}
	}
	rec := inboxGet(t, s, path, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fifth stream for one address = %d, want 429: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("stream 429 must carry Retry-After")
	}
	if strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("refused stream must not be a half-open SSE response: %q", rec.Header().Get("Content-Type"))
	}
}

// TestStreamIPBackstopTripsAtConfiguredValue is P3-A rework acceptance
// (3): the per-IP ceiling is the anti-runaway backstop - with it
// configured low, the next DISTINCT-address stream from the same peer IP
// is refused even though every per-inbox key is distinct, so only the
// shared per-IP backstop can have tripped.
func TestStreamIPBackstopTripsAtConfiguredValue(t *testing.T) {
	s, _ := limitsServer(t, Limits{StreamIPLimit: 3})
	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("/v1/namespaces/ns/messages/events?agent=addr-%02d", i)
		h := openInboxStream(t, s, path, nil)
		if _, ok := h.nextFrame(2 * time.Second); !ok {
			t.Fatalf("backstop stream %d never opened", i+1)
		}
	}
	rec := inboxGet(t, s, "/v1/namespaces/ns/messages/events?agent=addr-99", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("stream past the per-IP backstop = %d, want 429: %s", rec.Code, rec.Body)
	}
}

// TestStreamXFFIgnoredUnlessPeerTrusted is P3-A rework acceptance (4):
// X-Forwarded-For never keys the per-IP backstop unless the peer itself
// is a configured trusted proxy; when it is, the rightmost non-trusted
// XFF entry (the client, behind a trusted chain) is the key.
func TestStreamXFFIgnoredUnlessPeerTrusted(t *testing.T) {
	events := func(agent string) string {
		return "/v1/namespaces/ns/messages/events?agent=" + agent
	}

	// Default config trusts no proxy: the backstop keys the PEER IP, so
	// two different XFF values cannot buy a second bucket at cap 1.
	s, _ := limitsServer(t, Limits{StreamIPLimit: 1})
	h := openInboxStream(t, s, events("a1"), map[string]string{"X-Forwarded-For": "9.9.9.9"})
	if _, ok := h.nextFrame(2 * time.Second); !ok {
		t.Fatal("first stream behind an untrusted peer never opened")
	}
	rec := inboxGet(t, s, events("a2"), map[string]string{"X-Forwarded-For": "8.8.8.8"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("untrusted peer: XFF must not buy a second bucket = %d, want 429: %s", rec.Code, rec.Body)
	}

	// With 127.0.0.1 trusted, the XFF client IP keys the backstop:
	// distinct forwarded clients get distinct buckets even at cap 1...
	s2, _ := limitsServer(t, Limits{StreamIPLimit: 1, TrustedProxies: []string{"127.0.0.1/32"}})
	if _, ok := openInboxStream(t, s2, events("a1"), map[string]string{"X-Forwarded-For": "9.9.9.9"}).nextFrame(2 * time.Second); !ok {
		t.Fatal("first forwarded client never opened")
	}
	if _, ok := openInboxStream(t, s2, events("a2"), map[string]string{"X-Forwarded-For": "8.8.8.8"}).nextFrame(2 * time.Second); !ok {
		t.Fatal("second forwarded client never opened; a trusted proxy's XFF must key the backstop")
	}
	// ...and a third stream for the FIRST forwarded client is refused by
	// that client's own bucket.
	rec = inboxGet(t, s2, events("a3"), map[string]string{"X-Forwarded-For": "9.9.9.9"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third stream for one forwarded client = %d, want 429: %s", rec.Code, rec.Body)
	}
}

// TestInboxStreamKeyDistinct pins the per-inbox key construction: the
// verified subject, the namespace and the agent address each distinguish
// inboxes, and the namespace-wide stream (no agent) degrades to
// subject+namespace.
func TestInboxStreamKeyDistinct(t *testing.T) {
	base := InboxStreamKey("subj", "ns", "agent")
	if InboxStreamKey("subj", "ns", "agent") != base {
		t.Fatal("key is not deterministic")
	}
	for _, c := range []struct{ subject, ns, agent string }{
		{"subj2", "ns", "agent"},
		{"subj", "ns2", "agent"},
		{"subj", "ns", "agent2"},
		{"subj", "ns", ""}, // the namespace-wide stream
	} {
		if got := InboxStreamKey(c.subject, c.ns, c.agent); got == base {
			t.Fatalf("InboxStreamKey(%q,%q,%q) collides with the base key", c.subject, c.ns, c.agent)
		}
	}
}

// TestEffectiveClientIPTrustedProxy pins the XFF rule at the pure-helper
// level: an untrusted peer's XFF is ignored entirely; a trusted peer
// without XFF falls back to the peer IP; a trusted peer's XFF resolves
// rightmost-untrusted (skip trusted entries from the right, first
// non-trusted wins); malformed entries are skipped; an all-trusted or
// all-malformed chain falls back to the peer IP.
func TestEffectiveClientIPTrustedProxy(t *testing.T) {
	s, _ := limitsServer(t, Limits{TrustedProxies: []string{"10.0.0.0/8", "127.0.0.1/32"}})
	req := func(remote, xff string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	// (127.0.0.1 is in the trusted set above, so use an unlisted peer
	// for the ignored-XFF case.)
	if got := s.effectiveClientIP(req("192.0.2.10:443", "9.9.9.9")); got != "192.0.2.10" {
		t.Fatalf("untrusted peer: XFF must be ignored, got %q want the peer IP", got)
	}
	if got := s.effectiveClientIP(req("10.0.0.5:443", "")); got != "10.0.0.5" {
		t.Fatalf("trusted peer without XFF must fall back to the peer IP, got %q", got)
	}
	if got := s.effectiveClientIP(req("10.0.0.5:443", "9.9.9.9, 10.0.0.5")); got != "9.9.9.9" {
		t.Fatalf("rightmost-untrusted must win, got %q want 9.9.9.9", got)
	}
	if got := s.effectiveClientIP(req("10.0.0.5:443", "garbage, 8.8.8.8")); got != "8.8.8.8" {
		t.Fatalf("malformed XFF entries must be skipped, got %q want 8.8.8.8", got)
	}
	if got := s.effectiveClientIP(req("10.0.0.5:443", "10.0.0.9, 10.0.0.8")); got != "10.0.0.5" {
		t.Fatalf("all-trusted chain must fall back to the peer IP, got %q", got)
	}
}
