package api

import (
	"net"
	"net/http"
	"strings"
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
	// StreamLimit caps concurrent message streams (SSE) and awaits (MCP
	// await_messages) one INBOX may hold. An inbox is the verified
	// subject plus namespace plus agent address (InboxStreamKey), so
	// distinct agent addresses never share a pool - the P3-A rework:
	// the earlier subject+IP key gave every local agent (zero-key
	// bootstrap: empty subject, 127.0.0.1) and every agent behind one
	// central proxy (shared proxy IP) a single shared pool. Default
	// defaultStreamLimit - enough for reconnect overlap, few enough that
	// one address cannot park the server.
	StreamLimit int
	// StreamIPLimit is the separate per-IP backstop on the same
	// long-lived operations: one peer address may hold at most this many
	// streams/awaits across ALL inboxes. Much higher than StreamLimit on
	// purpose - it exists to stop a runaway or hostile host, not to shape
	// normal coordination traffic. Default defaultStreamIPLimit.
	StreamIPLimit int
	// TrustedProxies lists CIDRs whose peers may speak for the real
	// client with X-Forwarded-For (used for the per-IP backstop key
	// only, via Server.effectiveClientIP). Empty - the default - means
	// X-Forwarded-For is never trusted.
	TrustedProxies []string
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
	// defaultStreamLimit is 4 concurrent streams/awaits per INBOX
	// (subject+namespace+agent address): enough that a reconnect can
	// overlap its own previous stream during a restart, few enough that
	// one address cannot hold the bus or the server's goroutines hostage.
	defaultStreamLimit = 4
	// defaultStreamIPLimit is the per-IP backstop: one peer address may
	// hold this many streams/awaits across all inboxes. Deliberately
	// much higher than the per-inbox cap - it stops runaway hosts, not
	// coordination traffic (many agents legitimately share one proxy IP).
	defaultStreamIPLimit = 256
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
	if l.StreamIPLimit <= 0 {
		l.StreamIPLimit = defaultStreamIPLimit
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

// InboxStreamKey identifies one inbox: the verified subject, the
// namespace, and the agent address the stream or await reads (the SSE
// ?agent= parameter, the await_messages agent argument). Keying per
// inbox - not per caller - is the P3-A rework: subject+IP gave the
// zero-key bootstrap (empty subject, 127.0.0.1) and every agent behind
// one central proxy one shared pool. The namespace-wide stream has no
// agent, so its key degrades to subject+namespace. mcpserver's await
// cap reuses this same key (it imports api; api never imports
// mcpserver, so there is no cycle).
func InboxStreamKey(subject, namespace, agent string) string {
	return subject + "\x00" + namespace + "\x00" + agent
}

// streamSlots is one stream's reservation across both caps: the
// per-inbox slot and the per-IP backstop slot. The zero value releases
// nothing, so a refused acquire is safe to defer.
type streamSlots struct {
	inboxKey, ipKey string
}

// acquireStream reserves one stream under BOTH caps: the per-inbox cap
// (subject+namespace+agent) trips first - the meaningful coordination
// signal - and the per-IP backstop second, releasing the inbox slot if
// the backstop refuses. ok=false means nothing is held.
func (s *Server) acquireStream(r *http.Request, namespace, agent string) (streamSlots, bool) {
	sl := streamSlots{
		inboxKey: InboxStreamKey(verifiedSubject(r), namespace, agent),
		ipKey:    s.effectiveClientIP(r),
	}
	if !s.streams.acquire(sl.inboxKey) {
		return streamSlots{}, false
	}
	if !s.ipStreams.acquire(sl.ipKey) {
		s.streams.release(sl.inboxKey)
		return streamSlots{}, false
	}
	return sl, true
}

// releaseStream frees both of a stream's slots; a no-op on the zero
// value, so it pairs safely with a refused acquire.
func (s *Server) releaseStream(sl streamSlots) {
	if sl.ipKey != "" {
		s.ipStreams.release(sl.ipKey)
	}
	if sl.inboxKey != "" {
		s.streams.release(sl.inboxKey)
	}
}

// parseTrustedProxies resolves the configured CIDR strings once at
// server construction. Invalid entries were rejected by config
// validation, so a parse failure here is skipped defensively rather
// than fatal.
func parseTrustedProxies(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// effectiveClientIP returns the address the per-IP backstop keys on.
// It is the peer IP unless the peer is a configured trusted proxy; then
// the client IP comes from X-Forwarded-For using the rightmost-
// untrusted convention (the standard real-ip rule: scanning the list
// from the right, the first entry that is not itself a trusted proxy is
// the originating client - with a single trusted hop, the rightmost
// entry is the proxy and the one before it the client). Malformed
// entries are skipped; an absent, empty, all-trusted or all-malformed
// chain falls back to the peer IP. X-Forwarded-For NEVER affects the
// per-inbox key or the token-bucket rate limiter.
func (s *Server) effectiveClientIP(r *http.Request) string {
	peer := clientIP(r.RemoteAddr)
	peerIP := net.ParseIP(peer)
	if peerIP == nil {
		return peer
	}
	trusted := false
	for _, n := range s.trustedProxies {
		if n.Contains(peerIP) {
			trusted = true
			break
		}
	}
	if !trusted {
		return peer
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		ip := net.ParseIP(candidate)
		if ip == nil {
			continue // malformed entry: skip, keep scanning leftward
		}
		isTrusted := false
		for _, n := range s.trustedProxies {
			if n.Contains(ip) {
				isTrusted = true
				break
			}
		}
		if !isTrusted {
			return candidate
		}
	}
	return peer
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
