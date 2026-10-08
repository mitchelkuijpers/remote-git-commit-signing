package server

import (
	"math"
	"sync"
	"time"
)

// bucketTTL is how long an idle per-identity bucket is retained. Idle buckets
// are reclaimed lazily from allow so the map cannot grow without bound as
// allowlisted VM identities churn over the fleet's lifetime. It is far longer
// than any refill interval, so a bucket is never discarded while it still holds
// meaningful state — and an idle bucket would have refilled to full anyway.
const bucketTTL = 30 * time.Minute

// sweepBatch bounds how many buckets are examined for expiry per allow call, so
// eviction stays amortized O(1) however large the map grows.
const sweepBatch = 16

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

	l.evictIdle(now)

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

// evictIdle drops up to sweepBatch buckets that have been idle longer than
// bucketTTL. Deleting entries during a range is safe in Go. Scanning a bounded
// batch per call keeps the cost amortized O(1) while still reclaiming every idle
// identity over time.
func (l *rateLimiter) evictIdle(now time.Time) {
	examined := 0
	for key, b := range l.buckets {
		if now.Sub(b.last) > bucketTTL {
			delete(l.buckets, key)
		}
		examined++
		if examined >= sweepBatch {
			break
		}
	}
}
