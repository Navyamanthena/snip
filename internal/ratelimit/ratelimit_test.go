package ratelimit

import (
	"testing"
	"time"
)

func TestBurstThenRefill(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(2, 3) // 2 tokens/sec, burst 3
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("ip"); !ok {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	ok, wait := l.Allow("ip")
	if ok || wait <= 0 {
		t.Fatalf("4th request should be limited, got ok=%v wait=%v", ok, wait)
	}

	now = now.Add(500 * time.Millisecond) // refills 1 token
	if ok, _ := l.Allow("ip"); !ok {
		t.Fatal("should be allowed after refill")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l := New(1, 1)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal()
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("other key must have its own bucket")
	}
}

func TestCleanup(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(1, 1)
	l.now = func() time.Time { return now }
	l.Allow("a")
	now = now.Add(time.Hour)
	if n := l.Cleanup(time.Minute); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
}
