package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// limitsServer wires a Server with explicit request limits plus the memory,
// region and bus dependencies the capped routes need.
func limitsServer(t *testing.T, lim Limits) (*Server, *region.Store) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	clk := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clk = clk.Add(time.Millisecond)
		return clk
	}
	reg := region.New(db, now)
	s := New(testLogger(), Deps{
		Memory: memory.New(db, now),
		Region: reg,
		Bus:    bus.New(),
		Limits: lim,
	})
	return s, reg
}

// TestRequestLimitDefaults pins the generous defaults and, crucially, that
// the body default sits ABOVE the agent-hook handler's own cap: if it did
// not, the middleware would answer 413 where that endpoint documents 400.
func TestRequestLimitDefaults(t *testing.T) {
	got := Limits{}.withDefaults()
	if got.MaxBodyBytes != defaultMaxBodyBytes {
		t.Fatalf("MaxBodyBytes default = %d, want %d", got.MaxBodyBytes, defaultMaxBodyBytes)
	}
	if got.RateLimitRPS != defaultRateLimitRPS || got.RateLimitBurst != defaultRateLimitBurst {
		t.Fatalf("rate defaults = %v/%d, want %v/%d", got.RateLimitRPS, got.RateLimitBurst, defaultRateLimitRPS, defaultRateLimitBurst)
	}
	if got.StreamLimit != defaultStreamLimit {
		t.Fatalf("StreamLimit default = %d, want %d", got.StreamLimit, defaultStreamLimit)
	}
	if got.MaxBodyBytes <= maxAgentHookBody {
		t.Fatalf("body default %d must exceed maxAgentHookBody %d, or the hook endpoint's 400 becomes a middleware 413",
			got.MaxBodyBytes, maxAgentHookBody)
	}
	// A partially-set struct keeps its explicit values.
	partial := Limits{MaxBodyBytes: 4096, StreamLimit: 3}.withDefaults()
	if partial.MaxBodyBytes != 4096 || partial.StreamLimit != 3 {
		t.Fatalf("explicit values were overwritten: %+v", partial)
	}
	if partial.RateLimitRPS != defaultRateLimitRPS {
		t.Fatalf("unset rate should default: %+v", partial)
	}
}

// TestBodyLimitOversizeBody is the oversize-body acceptance case: a JSON
// POST over the cap is refused with 413 and a clear message, while a body
// under the cap still reaches the handler.
func TestBodyLimitOversizeBody(t *testing.T) {
	s, _ := limitsServer(t, Limits{MaxBodyBytes: 1024})

	// Under the cap: the request reaches the handler (this body is not
	// valid JSON, so the handler answers 400 - the point is that it is NOT
	// the middleware's 413).
	under := do(t, s, http.MethodPost, "/v1/namespaces/ns/memories", strings.Repeat("x", 512))
	if under.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("under-cap body was rejected as too large: %d %s", under.Code, under.Body)
	}

	// Over the cap: 413 with a clear message, before the handler runs.
	over := do(t, s, http.MethodPost, "/v1/namespaces/ns/memories", strings.Repeat("x", 4096))
	if over.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body = %d, want 413: %s", over.Code, over.Body)
	}
	if !strings.Contains(over.Body.String(), "too large") {
		t.Fatalf("413 body must explain the cap: %s", over.Body)
	}

	// The cap is method-scoped: a GET is untouched even with a large body.
	get := do(t, s, http.MethodGet, "/v1/namespaces", "")
	if get.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", get.Code, get.Body)
	}
}

// TestBodyLimitDefaultStillLetsTighterHandlerCapAnswer proves the global
// default cap does not shadow an endpoint's own (tighter) MaxBytesReader:
// the agent-hook endpoint keeps returning its documented 400 for a body
// over maxAgentHookBody, while a body over the MIDDLEWARE cap is 413.
func TestBodyLimitDefaultStillLetsTighterHandlerCapAnswer(t *testing.T) {
	s := testServer(t) // default limits (8 MiB body cap)

	hookOver := `{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/x","prompt_id":"p1","prompt":"` +
		strings.Repeat("x", maxAgentHookBody+1) + `"}`
	rec := do(t, s, http.MethodPost, "/v1/agent/hooks", hookOver)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("hook body just over its own cap = %d, want 400 (the handler's, not the middleware's 413): %s", rec.Code, rec.Body)
	}

	middlewareOver := strings.Repeat("x", defaultMaxBodyBytes+1)
	rec = do(t, s, http.MethodPost, "/v1/agent/hooks", middlewareOver)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("hook body over the middleware cap = %d, want 413: %s", rec.Code, rec.Body)
	}
}

// TestRateLimitSendAndRegister is the rate-limit acceptance case: the
// message-send and member-register endpoints share one per-IP bucket, and
// overflow is 429 with Retry-After.
func TestRateLimitSendAndRegister(t *testing.T) {
	frozen := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	old := limiterNow
	limiterNow = func() time.Time { return frozen }
	t.Cleanup(func() { limiterNow = old })

	// Burst 2, no refill while the clock is frozen.
	s, _ := limitsServer(t, Limits{RateLimitRPS: 1, RateLimitBurst: 2})

	register := `{"agent":"alice","role":"worker"}`
	for i := 0; i < 2; i++ {
		rec := do(t, s, http.MethodPost, "/v1/namespaces/ns/members", register)
		if rec.Code != http.StatusOK {
			t.Fatalf("register %d = %d, want 200: %s", i, rec.Code, rec.Body)
		}
	}
	rec := do(t, s, http.MethodPost, "/v1/namespaces/ns/members", register)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("register over the bucket = %d, want 429: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("429 must carry Retry-After")
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Fatalf("429 body = %s", rec.Body)
	}

	// The same bucket guards sends.
	rec = do(t, s, http.MethodPost, "/v1/namespaces/ns/messages",
		`{"sender":"alice","recipient":"bob","body":"hi"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("send over the shared bucket = %d, want 429: %s", rec.Code, rec.Body)
	}

	// A read is not rate limited (only the write endpoints are).
	rec = do(t, s, http.MethodGet, "/v1/namespaces/ns/members", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list members = %d, want 200 (reads are not capped): %s", rec.Code, rec.Body)
	}
}

// TestStreamCapReleasesOnDisconnect is the stream-cap acceptance case: the
// per-subject+IP concurrent stream cap refuses the over-cap stream with 429
// and, critically, frees the slot when a holder disconnects - so a client
// that drops its connection is never locked out.
func TestStreamCapReleasesOnDisconnect(t *testing.T) {
	s, reg := limitsServer(t, Limits{StreamLimit: 2})
	ctx := context.Background()
	if err := reg.Register(ctx, "ns", "bob", "worker"); err != nil {
		t.Fatal(err)
	}
	path := "/v1/namespaces/ns/messages/events?agent=bob"

	h1 := openMessagingStream(t, s, path, "")
	if _, ok := h1.nextFrame(2 * time.Second); !ok {
		t.Fatal("stream 1 never produced its initial frame")
	}
	h2 := openMessagingStream(t, s, path, "")
	if _, ok := h2.nextFrame(2 * time.Second); !ok {
		t.Fatal("stream 2 never produced its initial frame")
	}

	// Third stream: over the cap, refused before any SSE header is written.
	rec := do(t, s, http.MethodGet, path, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap stream = %d, want 429: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("stream 429 must carry Retry-After")
	}
	if strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("over-cap stream must not be a half-open SSE response: %q", rec.Header().Get("Content-Type"))
	}

	// Disconnect one holder. done closes only after the handler returned,
	// which is after its deferred release ran.
	h1.cancel()
	<-h1.done

	// The freed slot is immediately reusable.
	h3 := openMessagingStream(t, s, path, "")
	if _, ok := h3.nextFrame(2 * time.Second); !ok {
		t.Fatal("stream after a disconnect was still refused; the cap did not release")
	}
}

// TestStreamCapCoversNamespaceStream proves the operator stream shares the
// same per-subject+IP cap as the inbox stream.
func TestStreamCapCoversNamespaceStream(t *testing.T) {
	s, _ := limitsServer(t, Limits{StreamLimit: 1})

	h := openMessagingStream(t, s, "/v1/namespaces/ns/messages/events?agent=bob", "")
	if _, ok := h.nextFrame(2 * time.Second); !ok {
		t.Fatal("inbox stream never opened")
	}
	rec := do(t, s, http.MethodGet, "/v1/namespaces/ns/messages/stream", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("namespace stream over the shared cap = %d, want 429: %s", rec.Code, rec.Body)
	}

	h.cancel()
	<-h.done
	// After release the namespace stream is admitted (it streams, so run it
	// through the same pipe helper and expect its ready frame).
	h2 := openMessagingStream(t, s, "/v1/namespaces/ns/messages/stream", "")
	if _, ok := h2.nextFrame(2 * time.Second); !ok {
		t.Fatal("namespace stream never opened after the slot was released")
	}
}

// TestConcurrencyLimiterCapsAndReleases unit-tests the slot bookkeeping
// directly: independent keys, refusal at the cap, reuse after release, and
// an over-release that must not corrupt the count.
func TestConcurrencyLimiterCapsAndReleases(t *testing.T) {
	l := newConcurrencyLimiter(2)
	if !l.acquire("a") || !l.acquire("a") {
		t.Fatal("the first two acquires must succeed")
	}
	if l.acquire("a") {
		t.Fatal("a third acquire must be refused at cap 2")
	}
	if !l.acquire("b") {
		t.Fatal("a different key must be independent")
	}
	if n := l.inUse("a"); n != 2 {
		t.Fatalf("inUse(a) = %d, want 2", n)
	}
	l.release("a")
	if !l.acquire("a") {
		t.Fatal("a released slot must be reusable")
	}
	// Over-release must be a no-op, not a negative count that hands out
	// extra slots.
	l.release("a")
	l.release("a")
	l.release("a")
	if n := l.inUse("a"); n != 0 {
		t.Fatalf("inUse(a) after over-release = %d, want 0", n)
	}
	if !l.acquire("a") || !l.acquire("a") {
		t.Fatal("cap must be intact after an over-release")
	}
	if l.acquire("a") {
		t.Fatal("over-release must not have widened the cap")
	}
}
