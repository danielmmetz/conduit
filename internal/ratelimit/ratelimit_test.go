package ratelimit_test

import (
	"testing"
	"time"

	"github.com/danielmmetz/conduit/internal/ratelimit"
)

func TestAllow_BurstThenDeny(t *testing.T) {
	now := time.Unix(0, 0)
	l := ratelimit.KeyedLimiter{
		Rate:  1,
		Burst: 3,
		Now:   func() time.Time { return now },
	}
	for i := range 3 {
		if !l.Allow("a") {
			t.Fatalf("call %d denied, want allowed", i)
		}
	}
	if l.Allow("a") {
		t.Fatalf("4th call allowed, want denied")
	}
}

func TestAllow_RefillsOverTime(t *testing.T) {
	now := time.Unix(0, 0)
	l := ratelimit.KeyedLimiter{
		Rate:  1,
		Burst: 2,
		Now:   func() time.Time { return now },
	}
	if !l.Allow("a") {
		t.Fatalf("first call denied")
	}
	if !l.Allow("a") {
		t.Fatalf("second call denied — burst not honored")
	}
	if l.Allow("a") {
		t.Fatalf("third call should be denied before refill")
	}
	now = now.Add(1500 * time.Millisecond)
	if !l.Allow("a") {
		t.Fatalf("after 1.5s one token should be available")
	}
	if l.Allow("a") {
		t.Fatalf("second call after refill should be denied")
	}
}

func TestAllow_KeysIndependent(t *testing.T) {
	now := time.Unix(0, 0)
	l := ratelimit.KeyedLimiter{
		Rate:  1,
		Burst: 1,
		Now:   func() time.Time { return now },
	}
	if !l.Allow("a") {
		t.Fatalf("a denied")
	}
	if !l.Allow("b") {
		t.Fatalf("b denied — keys should be independent")
	}
	if l.Allow("a") {
		t.Fatalf("a second call allowed, want denied")
	}
}

func TestAllow_IdleTTLEvicts(t *testing.T) {
	now := time.Unix(0, 0)
	l := ratelimit.KeyedLimiter{
		Rate:    1,
		Burst:   1,
		IdleTTL: time.Minute,
		Now:     func() time.Time { return now },
	}
	l.Allow("a")
	if got := l.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
	now = now.Add(2 * time.Minute)
	// Any call exercises the opportunistic GC.
	l.Allow("b")
	if got := l.Len(); got != 1 {
		t.Fatalf("Len = %d after eviction, want 1", got)
	}
}
