package api

import (
	"net"
	"net/http"
	"sync"
)

// Request limits (task P3-A): a bound on inbound request work so a single
// client cannot exhaust the coordinator. Three independent caps:
//
//   - a request-body cap applied to every method that carries a JSON body,
//     so an endpoint without its own http.MaxBytesReader still cannot be
//     made to read unbounded input into memory;
//   - a per-IP token bucket on the two endpoints that write (message send
//     and member register);
//   - a concurrent stream/await cap, keyed per subject+IP, on the SSE
//     endpoints and the MCP await_messages tool - the long-lived
//     operations that hold a goroutine and a bus subscription open.
//
// Limits is the configurable surface. Its zero value is valid: every unset
// field falls back to the generous package default below, so an ordinary
// deployment never notices a cap and an operator only sets a knob to make
// one tighter.

// Limits bounds inbound request work. Zero fields take the default.
type Limits struct {
	// MaxBodyBytes caps the body of POST/PUT/PATCH requests. Default
	// defaultMaxBodyBytes.
	MaxBodyBytes int64
	// RateLimitRPS and RateLimitBurst are the per-IP token bucket for the
	// message-send and member-register endpoints. Defaults
	// defaultRateLimitRPS / defaultRateLimitBurst.
	RateLimitRPS   float64
	RateLimitBurst int
	// StreamLimit caps concurrent message streams (SSE) and awaits
	// (MCP await_messages) held by one subject+IP. Default
	// defaultStreamLimit.
	StreamLimit int
}

// The defaults are deliberately generous: they exist to stop a runaway or
// hostile client, not to shape normal traffic.
const (
	// defaultMaxBodyBytes is 8 MiB, deliberately ABOVE the 5 MiB
	// maxAgentHookBody cap (agent_handlers.go) so that endpoint keeps
	// returning its own 400 for an oversized hook payload instead of the
	// middleware's 413. It still bounds every other JSON endpoint, most of
	// which had no cap at all.
	defaultMaxBodyBytes = 8 << 20
	// defaultRateLimitRPS / defaultRateLimitBurst match the burst the
	// agent-hook group already uses (server.go), which is generous for
	// message sends: an agent that bursts 100 sends then settles to 50/s
	// never sees a 429.
	defaultRateLimitRPS   = 50
	defaultRateLimitBurst = 100
	// defaultStreamLimit is 8 concurrent streams/awaits per subject+IP -
	// more than a single agent session ever needs, few enough that one
	// client cannot hold the bus or the server's goroutines hostage.
	defaultStreamLimit = 8
)

// withDefaults fills every unset field with its generous default.
func (l Limits) withDefaults() Limits {
	if l.MaxBodyBytes <= 0 {
		l.MaxBodyBytes = defaultMaxBodyBytes
	}
	if l.RateLimitRPS <= 0 {
		l.RateLimitRPS = defaultRateLimitRPS
	}
	if l.RateLimitBurst <= 0 {
		l.RateLimitBurst = defaultRateLimitBurst
	}
	if l.StreamLimit <= 0 {
		l.StreamLimit = defaultStreamLimit
	}
	return l
}

// bodyLimit caps the request body of every method that can carry one
// (POST/PUT/PATCH). A declared Content-Length over the cap is refused with
// 413 before the handler runs; otherwise the body is wrapped in
// http.MaxBytesReader so an undeclared (chunked) oversize body still cannot
// be read past the cap. GET/HEAD/DELETE are untouched - no JSON endpoint
// reads a body from them.
//
// It is wired as router middleware (New), which is what makes the cap
// apply to EVERY JSON endpoint in this package, including the ones that
// predate this task and never installed their own MaxBytesReader. An
// endpoint with a tighter cap of its own (agent_handlers.go,
// messaging_diagnostics.go) still applies it inside its handler; the
// middleware's looser cap lets those requests through to produce their
// existing response.
func (s *Server) bodyLimit(next http.Handler) http.Handler {
	limit := s.limits.MaxBodyBytes
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
		default:
			next.ServeHTTP(w, r)
			return
		}
		if r.ContentLength > limit {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": "request body too large",
			})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// writeRateLimited is the shared 429 shape for the per-IP and per-stream
// caps: a Retry-After of one second (the token bucket refills well inside
// that at the default rate; a stream slot frees as soon as the holder
// disconnects) and a JSON body naming what was exhausted.
func writeRateLimited(w http.ResponseWriter, message string) {
	w.Header().Set("Retry-After", "1")
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": message})
}

// concurrencyLimiter caps how many operations holding the same key may run
// at once. acquire reports false when the key is already at the cap; every
// successful acquire must be paired with exactly one release. The stream
// and await handlers release via defer, which is what frees the slot when
// a client disconnects or the context is cancelled - the cap is a
// concurrency bound, never a permanent ban.
type concurrencyLimiter struct {
	mu     sync.Mutex
	limit  int
	active map[string]int
}

func newConcurrencyLimiter(limit int) *concurrencyLimiter {
	if limit <= 0 {
		limit = defaultStreamLimit
	}
	return &concurrencyLimiter{limit: limit, active: map[string]int{}}
}

// acquire reserves one slot for key, or reports false when it is full.
func (c *concurrencyLimiter) acquire(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[key] >= c.limit {
		return false
	}
	c.active[key]++
	return true
}

// release returns one slot for key. It is a no-op when the key holds none,
// so a stray double release can never drive the count negative.
func (c *concurrencyLimiter) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch n := c.active[key]; {
	case n > 1:
		c.active[key] = n - 1
	default:
		delete(c.active, key)
	}
}

// inUse reports the slots currently held for key (tests and diagnostics).
func (c *concurrencyLimiter) inUse(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active[key]
}

// streamKey identifies a stream/await holder: the verified subject plus the
// peer IP. Subject alone would let one credential open the cap's worth of
// streams from many hosts; IP alone would let one host starve every subject
// behind it. An unauthenticated caller (the zero-key bootstrap, unit tests)
// has an empty subject, so the key degrades to IP-only.
func streamKey(r *http.Request) string {
	return verifiedSubject(r) + "\x00" + clientIP(r.RemoteAddr)
}

// clientIP strips the port from a RemoteAddr, tolerating a bare host (some
// proxies and httptest values) by returning it unchanged.
func clientIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// newSendLimiter builds the per-IP bucket the write endpoints share.
func newSendLimiter(l Limits) *ipLimiter {
	return newIPLimiter(l.RateLimitRPS, l.RateLimitBurst)
}
