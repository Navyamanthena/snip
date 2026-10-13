package shortener

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/navyamanthena/snip/internal/store"
)

func TestCreateGeneratesCode(t *testing.T) {
	s := New(store.NewMemoryStore())
	l, err := s.Create(context.Background(), CreateRequest{URL: "https://example.com/a?b=c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Code) != codeLength {
		t.Fatalf("code %q has length %d", l.Code, len(l.Code))
	}
	got, err := s.Resolve(context.Background(), l.Code)
	if err != nil || got.URL != "https://example.com/a?b=c" {
		t.Fatalf("resolve: %v %v", got, err)
	}
}

func TestCreateValidation(t *testing.T) {
	s := New(store.NewMemoryStore())
	ctx := context.Background()
	cases := []struct {
		name string
		req  CreateRequest
		want error
	}{
		{"empty url", CreateRequest{URL: ""}, ErrInvalidURL},
		{"no scheme", CreateRequest{URL: "example.com"}, ErrInvalidURL},
		{"ftp scheme", CreateRequest{URL: "ftp://example.com"}, ErrInvalidURL},
		{"javascript", CreateRequest{URL: "javascript:alert(1)"}, ErrInvalidURL},
		{"short alias", CreateRequest{URL: "https://a.co", Alias: "ab"}, ErrInvalidAlias},
		{"bad alias chars", CreateRequest{URL: "https://a.co", Alias: "has space"}, ErrInvalidAlias},
		{"reserved alias", CreateRequest{URL: "https://a.co", Alias: "api"}, ErrInvalidAlias},
		{"ttl too short", CreateRequest{URL: "https://a.co", TTL: time.Minute}, ErrInvalidTTL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.Create(ctx, c.req); !errors.Is(err, c.want) {
				t.Fatalf("got %v want %v", err, c.want)
			}
		})
	}
}

func TestAliasTaken(t *testing.T) {
	s := New(store.NewMemoryStore())
	ctx := context.Background()
	if _, err := s.Create(ctx, CreateRequest{URL: "https://a.co", Alias: "my-link"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, CreateRequest{URL: "https://b.co", Alias: "my-link"}); !errors.Is(err, ErrAliasTaken) {
		t.Fatalf("got %v", err)
	}
}

func TestTTLSetsExpiry(t *testing.T) {
	s := New(store.NewMemoryStore())
	l, err := s.Create(context.Background(), CreateRequest{URL: "https://a.co", TTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if l.ExpiresAt == nil || l.ExpiresAt.Sub(l.CreatedAt) != 24*time.Hour {
		t.Fatalf("bad expiry %+v", l)
	}
}

// collidingStore reports the first N creates as collisions.
type collidingStore struct {
	*store.MemoryStore
	collisions int
}

func (c *collidingStore) Create(ctx context.Context, l store.Link) error {
	if c.collisions > 0 {
		c.collisions--
		return store.ErrExists
	}
	return c.MemoryStore.Create(ctx, l)
}

func TestRetriesOnCollision(t *testing.T) {
	s := New(&collidingStore{MemoryStore: store.NewMemoryStore(), collisions: 3})
	if _, err := s.Create(context.Background(), CreateRequest{URL: "https://a.co"}); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	s = New(&collidingStore{MemoryStore: store.NewMemoryStore(), collisions: 10})
	if _, err := s.Create(context.Background(), CreateRequest{URL: "https://a.co"}); err == nil {
		t.Fatal("expected failure after max retries")
	}
}

func TestRandomCodeAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		c, _ := randomCode(codeLength)
		for _, r := range c {
			if !containsRune(alphabet, r) {
				t.Fatalf("unexpected char %q", r)
			}
		}
		if seen[c] {
			t.Fatalf("duplicate code %q", c)
		}
		seen[c] = true
	}
}

func containsRune(s string, r rune) bool {
	for _, x := range s {
		if x == r {
			return true
		}
	}
	return false
}
