package auth

import (
	"testing"
	"time"
)

const testWindow = 1 * time.Hour // effectively non-expiring for these tests

func TestRateLimiterWindow(t *testing.T) {
	l := NewRateLimiter(testWindow, 16)
	key := "k"
	// 5 allowed, 6th rejected (limit 5).
	for i := 0; i < 5; i++ {
		if !l.Allow(key, 5) {
			t.Fatalf("attempt %d rejected within limit", i+1)
		}
	}
	if l.Allow(key, 5) {
		t.Fatal("6th attempt within the window was allowed")
	}
	// A different key is independent.
	if !l.Allow("other", 5) {
		t.Fatal("other key affected by k's window")
	}
}

func TestRateLimiterReset(t *testing.T) {
	l := NewRateLimiter(testWindow, 16)
	for i := 0; i < 5; i++ {
		l.Allow("k", 5)
	}
	l.Reset("k")
	if !l.Allow("k", 5) {
		t.Fatal("allowed attempt after Reset")
	}
}

func TestRateLimiterSetLimit(t *testing.T) {
	l := NewRateLimiter(testWindow, 16)
	for i := 0; i < 3; i++ {
		l.Allow("k", 10)
	}
	l.SetLimit("k", 3)
	if l.Allow("k", 10) {
		t.Fatal("SetLimit did not shrink the window")
	}
}

func TestRateLimiterIsolationManyKeys(t *testing.T) {
	l := NewRateLimiter(testWindow, 64)
	for i := 0; i < 64; i++ {
		l.Allow(string(rune('a'+i%26))+string(rune('a'+i/26)), 100)
	}
	// All keys used exactly once: all independent.
	for i := 0; i < 64; i++ {
		if !l.Allow(string(rune('a'+i%26))+string(rune('a'+i/26)), 100) {
			t.Fatalf("key %d wrongly throttled", i)
		}
	}
}
