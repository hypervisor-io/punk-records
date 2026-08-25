package api

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ipLimiter is a per-IP token bucket with idle eviction: intake
// endpoints face the internet-ish; a broken webhook sender must not be
// able to grind the coordinator.
type ipLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
	rps     rate.Limit
	burst   int
}

type ipBucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func newIPLimiter(rps float64, burst int) *ipLimiter {
	l := &ipLimiter{buckets: map[string]*ipBucket{}, rps: rate.Limit(rps), burst: burst}
	go func() {
		for range time.Tick(5 * time.Minute) {
			l.mu.Lock()
			cutoff := time.Now().Add(-15 * time.Minute)
			for ip, b := range l.buckets {
				if b.seen.Before(cutoff) {
					delete(l.buckets, ip)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *ipLimiter) allow(remoteAddr string) bool {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		ip = remoteAddr
	}
	l.mu.Lock()
	b, ok := l.buckets[ip]
	if !ok {
		b = &ipBucket{lim: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[ip] = b
	}
	b.seen = time.Now()
	l.mu.Unlock()
	return b.lim.Allow()
}

// rateLimit guards a route group. 429 with Retry-After on overflow.
func (s *Server) rateLimit(rps float64, burst int) func(http.Handler) http.Handler {
	lim := newIPLimiter(rps, burst)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !lim.allow(r.RemoteAddr) {
				w.Header().Set("Retry-After", "1")
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
