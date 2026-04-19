// Package ratelimit implements a per-key token-bucket limiter used by the
// signaling server to bound per-IP reserve and join attempts.
//
// The bucket math is delegated to golang.org/x/time/rate; this package only
// adds the keyed map, opportunistic idle GC, and an injectable clock for
// deterministic tests.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// KeyedLimiter gates events by key using an independent token bucket per key.
// The zero value is not usable: callers must set Rate and Burst. Methods are
// safe for concurrent use.
type KeyedLimiter struct {
	// Rate is the bucket refill rate, in tokens per second.
	Rate rate.Limit
	// Burst is the maximum bucket size, in tokens.
	Burst int
	// IdleTTL, if positive, evicts a key's bucket once it has been idle for
	// at least IdleTTL. Eviction is opportunistic (piggybacked on Allow calls).
	IdleTTL time.Duration
	// Now is a testing hook. If nil, time.Now is used.
	Now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// Allow reports whether the next event for key is permitted, consuming a
// token if so.
func (l *KeyedLimiter) Allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = make(map[string]*bucket)
	}
	l.gcLocked(now)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{limiter: rate.NewLimiter(l.Rate, l.Burst)}
		l.buckets[key] = b
	}
	b.lastSeen = now
	return b.limiter.AllowN(now, 1)
}

// Len returns the number of tracked keys (for tests and metrics).
func (l *KeyedLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

func (l *KeyedLimiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// gcLocked evicts at most a handful of idle entries to keep map size in check
// without paying an O(n) pass on every call.
func (l *KeyedLimiter) gcLocked(now time.Time) {
	if l.IdleTTL <= 0 {
		return
	}
	threshold := now.Add(-l.IdleTTL)
	const maxScan = 16
	i := 0
	for k, b := range l.buckets {
		if b.lastSeen.Before(threshold) {
			delete(l.buckets, k)
		}
		i++
		if i >= maxScan {
			break
		}
	}
}
