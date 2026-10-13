package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/navyamanthena/snip/internal/resp"
)

// contract runs the same behaviour checks against every Store implementation.
func contract(t *testing.T, s Store, prefix string) {
	ctx := context.Background()
	code := prefix + "abc123"

	if err := s.Create(ctx, Link{ID: "id-1", Code: code, URL: "https://go.dev", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, Link{Code: code, URL: "https://other.dev"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create: got %v want ErrExists", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.IncrClicks(ctx, code, "id-1"); err != nil {
			t.Fatal(err)
		}
	}
	l, err := s.Get(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	if l.URL != "https://go.dev" || l.Clicks != 3 {
		t.Fatalf("got %+v", l)
	}
	if _, err := s.Get(ctx, prefix+"missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: got %v", err)
	}
}

func TestMemoryStore(t *testing.T) {
	contract(t, NewMemoryStore(), "")
}

func TestMemoryStoreExpiry(t *testing.T) {
	m := NewMemoryStore()
	exp := time.Now().Add(time.Minute)
	_ = m.Create(context.Background(), Link{Code: "x", URL: "https://a.b", ExpiresAt: &exp})
	m.now = func() time.Time { return exp.Add(time.Second) }
	if _, err := m.Get(context.Background(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected expired link to be gone, got %v", err)
	}
}

func TestRedisStore(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set")
	}
	c := resp.New(addr)
	defer c.Close()
	ctx := context.Background()
	prefix := "test" + time.Now().Format("150405.000000")
	contract(t, NewRedisStore(c), prefix)

	// Expiring links disappear once their TTL passes.
	exp := time.Now().Add(150 * time.Millisecond)
	s := NewRedisStore(c)
	if err := s.Create(ctx, Link{Code: prefix + "ttl", URL: "https://a.b", ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := s.Get(ctx, prefix+"ttl"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected expiry, got %v", err)
	}
}

// Reviewer's race: clicks must never outlive their link or leak into a new
// link that reuses the same alias.
func TestRedisClicksAreTiedToTheirLink(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set")
	}
	c := resp.New(addr)
	defer c.Close()
	ctx := context.Background()
	s := NewRedisStore(c)
	code := "reuse" + time.Now().Format("150405.000000")

	// Clicking a link that doesn't exist creates no counter.
	if err := s.IncrClicks(ctx, code, "any"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v want ErrNotFound", err)
	}
	if n, _ := c.Do(ctx, "EXISTS", clicksKey(code)); n != int64(0) {
		t.Fatal("orphan click counter was created")
	}

	// The counter expires together with its link.
	exp := time.Now().Add(400 * time.Millisecond)
	if err := s.Create(ctx, Link{ID: "old", Code: code, URL: "https://old.example", ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		s.IncrClicks(ctx, code, "old")
	}
	ttl, _ := c.Do(ctx, "PTTL", clicksKey(code))
	if ms, _ := ttl.(int64); ms <= 0 || ms > 400 {
		t.Fatalf("click counter TTL should match the link, got %v", ttl)
	}

	// After expiry, a new link with the same alias starts from zero clicks.
	time.Sleep(500 * time.Millisecond)
	if err := s.Create(ctx, Link{ID: "new", Code: code, URL: "https://new.example"}); err != nil {
		t.Fatal(err)
	}
	l, _ := s.Get(ctx, code)
	if l.URL != "https://new.example" || l.Clicks != 0 {
		t.Fatalf("old clicks leaked into reused alias: %+v", l)
	}
	// A stale counter left behind somehow is also cleared on create.
	c.Do(ctx, "DEL", linkKey(code))
	c.Do(ctx, "SET", clicksKey(code), "99")
	s.Create(ctx, Link{ID: "newer", Code: code, URL: "https://newer.example"})
	if l, _ := s.Get(ctx, code); l.Clicks != 0 {
		t.Fatalf("stale counter survived create: %+v", l)
	}
	// Non-expiring links keep a non-expiring counter.
	s.IncrClicks(ctx, code, "newer")
	if ttl, _ := c.Do(ctx, "PTTL", clicksKey(code)); ttl != int64(-1) {
		t.Fatalf("counter for permanent link should not expire, got %v", ttl)
	}
}

// Reviewer's case: a link expires, its alias is reused, and then the old
// redirect's asynchronous click arrives. It must not count for the new link.
func lateClickScenario(t *testing.T, s Store, code string, expire func()) {
	t.Helper()
	ctx := context.Background()
	exp := time.Now().Add(300 * time.Millisecond)
	if err := s.Create(ctx, Link{ID: "old-link", Code: code, URL: "https://old.example", ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	old, _ := s.Get(ctx, code) // the redirect resolved the old link...
	expire()                   // ...then the link expired...
	if err := s.Create(ctx, Link{ID: "new-link", Code: code, URL: "https://new.example"}); err != nil {
		t.Fatal(err) // ...and the alias was reused.
	}
	// The old redirect's click finally runs.
	if err := s.IncrClicks(ctx, old.Code, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late click for replaced link: got %v want ErrNotFound", err)
	}
	if l, _ := s.Get(ctx, code); l.Clicks != 0 {
		t.Fatalf("late click from the old link was counted on the new one: %+v", l)
	}
	// Clicks for the new link still count.
	if err := s.IncrClicks(ctx, code, "new-link"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.Get(ctx, code); l.Clicks != 1 {
		t.Fatalf("new link click not counted: %+v", l)
	}
}

func TestLateClickMemory(t *testing.T) {
	m := NewMemoryStore()
	lateClickScenario(t, m, "reused", func() {
		m.now = func() time.Time { return time.Now().Add(time.Hour) }
	})
}

func TestLateClickRedis(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set")
	}
	c := resp.New(addr)
	defer c.Close()
	lateClickScenario(t, NewRedisStore(c), "late"+time.Now().Format("150405.000000"), func() {
		time.Sleep(400 * time.Millisecond)
	})
}
