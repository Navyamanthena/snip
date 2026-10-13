// Package ratelimit implements a per-client token-bucket rate limiter.
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// Limiter allows each key `burst` requests at once, refilling at `rate`
// tokens per second. Idle buckets are evicted to bound memory.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
	now     func() time.Time
}

func New(ratePerSec float64, burst int) *Limiter {
	return &Limiter{
		buckets: make(map[string]*bucket),
		rate:    ratePerSec,
		burst:   float64(burst),
		now:     time.Now,
	}
}

// Allow reports whether the key may make a request now, and if not, how long
// until it may retry.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
	b.lastSeen = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

// Cleanup removes buckets idle longer than maxIdle. Call it periodically.
func (l *Limiter) Cleanup(maxIdle time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-maxIdle)
	removed := 0
	for k, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, k)
			removed++
		}
	}
	return removed
}
