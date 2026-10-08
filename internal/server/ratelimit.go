package server

import (
	"math"
	"sync"
	"time"
)

// rateLimiter is a per-key token bucket, implemented in-process with the
// standard library only.
type rateLimiter struct {
	mu      sync.Mutex
	perSec  float64
	burst   float64
	buckets map[string]*tokenBucket
}

// tokenBucket is the mutable token state of one key.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

// newRateLimiter builds a limiter refilling at perMin tokens per minute with a
// bucket capacity of burst.
func newRateLimiter(perMin, burst int) *rateLimiter {
	return &rateLimiter{
		perSec:  float64(perMin) / 60,
		burst:   float64(burst),
		buckets: make(map[string]*tokenBucket),
	}
}

// allow reports whether key may proceed at now, consuming one token. Keys are
// independent: exhausting one does not affect another.
func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.perSec)
		b.last = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
