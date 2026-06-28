package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Rate-limit parameters for the auth endpoints. A fixed window keyed by client
// IP is enough here: the dashboard is single-replica and ClusterIP-only
// (INV-05), so an in-memory limiter needs no shared store, and behind a reverse
// proxy RemoteAddr is the proxy's address — acceptable because the dashboard is
// not internet-facing, so the limiter is a brute-force speed bump, not a
// public-abuse control.
const (
	rateLimitMax    = 20
	rateLimitWindow = time.Minute
)

// rateLimiter is a fixed-window per-IP counter. The map is the only mutable
// global state in the package and is mutex-guarded; stale windows are evicted
// lazily on access so memory stays bounded to the set of recently-seen IPs.
type rateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	counts map[string]*rateWindow
}

type rateWindow struct {
	count       int
	windowStart time.Time
}

func newRateLimiter(max int, window time.Duration, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{
		max:    max,
		window: window,
		now:    now,
		counts: make(map[string]*rateWindow),
	}
}

// allow reports whether the IP may make another request in the current window
// and records the attempt when it does.
func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	rl.evictLocked(now)

	w, ok := rl.counts[ip]
	if !ok || now.Sub(w.windowStart) >= rl.window {
		rl.counts[ip] = &rateWindow{count: 1, windowStart: now}
		return true
	}
	if w.count >= rl.max {
		return false
	}
	w.count++
	return true
}

// evictLocked drops windows that have fully elapsed so the map cannot grow
// without bound. Caller holds rl.mu.
func (rl *rateLimiter) evictLocked(now time.Time) {
	for ip, w := range rl.counts {
		if now.Sub(w.windowStart) >= rl.window {
			delete(rl.counts, ip)
		}
	}
}

// middleware enforces the limit, returning 429 over budget.
func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientIP(r)) {
			writeJSONError(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP strips the port from RemoteAddr. A malformed RemoteAddr falls back to
// the raw value so the key is still stable per connection.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
