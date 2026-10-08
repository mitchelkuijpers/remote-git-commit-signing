package server

import (
	"testing"
	"time"
)

// TestRateLimiterEvictsIdleBuckets covers lazy expiry: a bucket idle longer
// than bucketTTL is dropped on a later access, so the map cannot grow without
// bound as allowlisted VM identities churn, and a subsequent request recreates
// a fresh bucket.
func TestRateLimiterEvictsIdleBuckets(t *testing.T) {
	l := newRateLimiter(60, 10)
	base := time.Unix(1_700_000_000, 0)

	if !l.allow("vm-a", base) {
		t.Fatal("first request for vm-a denied")
	}
	first := l.buckets["vm-a"]
	if first == nil {
		t.Fatal("vm-a bucket missing after first allow")
	}

	// A sweep while a bucket is still fresh must keep it.
	if !l.allow("vm-b", base.Add(time.Minute)) {
		t.Fatal("first request for vm-b denied")
	}
	if _, ok := l.buckets["vm-a"]; !ok {
		t.Fatal("active vm-a bucket was evicted before its TTL elapsed")
	}

	// Much later, a new identity triggers a sweep that reclaims the idle bucket.
	later := base.Add(bucketTTL + time.Hour)
	if !l.allow("vm-c", later) {
		t.Fatal("first request for vm-c denied")
	}
	if _, ok := l.buckets["vm-a"]; ok {
		t.Fatal("idle vm-a bucket was not evicted")
	}
	if _, ok := l.buckets["vm-b"]; ok {
		t.Fatal("idle vm-b bucket was not evicted")
	}

	// Signing again recreates the bucket rather than reusing the stale one.
	if !l.allow("vm-a", later) {
		t.Fatal("request for vm-a after eviction denied")
	}
	if got := l.buckets["vm-a"]; got == nil || got == first {
		t.Fatalf("vm-a bucket after re-allow = %v, want a freshly recreated bucket", got)
	}
}
