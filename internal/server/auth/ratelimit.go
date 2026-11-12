package auth

import (
	"sync"
	"time"
)

// RateLimiter is a bounded fixed-window counter. Keys are opaque
// strings ("ip:1.2.3.4", "email:user@example.com"); each key allows
// `limit` hits per window.
//
// It is in-memory (per server process) — the right scope for login
// throttling on a self-hosted single-instance server. The map is
// bounded: expired entries are purged on each call, and if the map
// still exceeds the cap, oldest entries are dropped (a dropped key
// simply resets its throttle — acceptable versus unbounded memory
// growth on a long-running server).
type RateLimiter struct {
	mu     sync.Mutex
	window time.Duration
	cap    int
	limit  map[string]int // optional per-key limit override
	hits   map[string]winState
}

type winState struct {
	count int
	start time.Time
}

// NewRateLimiter builds a limiter. maxKeys caps memory (10000 when
// <= 0).
func NewRateLimiter(window time.Duration, maxKeys int) *RateLimiter {
	if maxKeys <= 0 {
		maxKeys = 10000
	}
	return &RateLimiter{
		window: window,
		cap:    maxKeys,
		limit:  map[string]int{},
		hits:   map[string]winState{},
	}
}

// SetLimit overrides the per-window limit for one key.
func (l *RateLimiter) SetLimit(key string, limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit[key] = limit
}

// Allow reports whether a hit on key is permitted, and records it.
func (l *RateLimiter) Allow(key string, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim, ok := l.limit[key]; ok && lim > 0 {
		limit = lim
	}
	l.purgeLocked()

	now := time.Now()
	w, ok := l.hits[key]
	if !ok || now.Sub(w.start) >= l.window {
		l.hits[key] = winState{count: 1, start: now}
		return true
	}
	if w.count >= limit {
		return false
	}
	w.count++
	l.hits[key] = w
	return true
}

// Reset clears a key's counter (e.g. after a successful login).
func (l *RateLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

// purgeLocked drops expired entries; if the map still exceeds the cap,
// it drops arbitrary entries until it is back under.
func (l *RateLimiter) purgeLocked() {
	now := time.Now()
	for k, w := range l.hits {
		if now.Sub(w.start) >= l.window {
			delete(l.hits, k)
		}
	}
	for len(l.hits) > l.cap {
		for k := range l.hits {
			delete(l.hits, k)
			break
		}
	}
}
