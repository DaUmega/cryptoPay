package main

// Per-IP rate limiting for the HTTP server. This is the only defence
// the service has against a single client hammering it: the chain APIs
// are already rate-limited by their operators, so an attacker who can
// make cryptopay poll faster than its own limit, or create invoices in
// a loop, can get the operator's IP throttled or banned upstream.
// The limiter is deliberately generous (120 requests per minute per
// IP, burst 30) so a normal pay page polling every 10 seconds and a
// merchant creating invoices never notice it, while a loop does.
// It lives outside routes() so unit tests that hit routes() directly
// are not throttled, and main() wraps the mux with it in production.

import (
	"net"
	"net/http"
	"sync"
	"time"
)

type rateLimiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*tokenBucket
	now     func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMin, burst int) *rateLimiter {
	return &rateLimiter{
		rate: float64(perMin) / 60, burst: float64(burst),
		buckets: map[string]*tokenBucket{}, now: time.Now,
	}
}

func (l *rateLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		b = &tokenBucket{tokens: l.burst, last: l.now()}
		l.buckets[ip] = b
	}
	now := l.now()
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (l *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r)) {
			writeErr(w, 429, "rate limited")
			return
		}
		next.ServeHTTP(w, r)
	})
}
