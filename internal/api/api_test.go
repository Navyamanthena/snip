package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/navyamanthena/snip/internal/ratelimit"
	"github.com/navyamanthena/snip/internal/shortener"
	"github.com/navyamanthena/snip/internal/store"
)

func newTestServer(limiter *ratelimit.Limiter) (*httptest.Server, *store.MemoryStore) {
	st := store.NewMemoryStore()
	if limiter == nil {
		limiter = ratelimit.New(1000, 1000)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(shortener.New(st), st, limiter, "http://sn.ip", log)
	return httptest.NewServer(srv.Handler()), st
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	res, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestCreateAndRedirect(t *testing.T) {
	ts, st := newTestServer(nil)
	defer ts.Close()

	res := post(t, ts.URL+"/api/links", `{"url":"https://go.dev/doc","alias":"godocs"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", res.StatusCode)
	}
	var body linkResponse
	_ = json.NewDecoder(res.Body).Decode(&body)
	if body.ShortURL != "http://sn.ip/godocs" {
		t.Fatalf("shortUrl %q", body.ShortURL)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := client.Get(ts.URL + "/godocs")
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "https://go.dev/doc" {
		t.Fatalf("redirect: %d %q", r.StatusCode, r.Header.Get("Location"))
	}

	// Click is recorded asynchronously.
	deadline := time.Now().Add(time.Second)
	for {
		l, _ := st.Get(t.Context(), "godocs")
		if l.Clicks == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("click not recorded, clicks=%d", l.Clicks)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestErrors(t *testing.T) {
	ts, _ := newTestServer(nil)
	defer ts.Close()

	cases := []struct {
		body string
		want int
	}{
		{`not json`, http.StatusBadRequest},
		{`{"url":"https://a.co"}TRAILING`, http.StatusBadRequest},
		{`{"url":"notaurl"}`, http.StatusBadRequest},
		{`{"url":"https://a.co","alias":"dup"}`, http.StatusCreated},
		{`{"url":"https://b.co","alias":"dup"}`, http.StatusConflict},
	}
	for _, c := range cases {
		if res := post(t, ts.URL+"/api/links", c.body); res.StatusCode != c.want {
			t.Fatalf("%s: got %d want %d", c.body, res.StatusCode, c.want)
		}
	}
	if res, _ := http.Get(ts.URL + "/nope123"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown code: %d", res.StatusCode)
	}
	if res, _ := http.Get(ts.URL + "/api/links/nope123"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown stats: %d", res.StatusCode)
	}
}

func TestRateLimit(t *testing.T) {
	ts, _ := newTestServer(ratelimit.New(0.001, 2))
	defer ts.Close()
	for i := 0; i < 2; i++ {
		if res := post(t, ts.URL+"/api/links", `{"url":"https://a.co"}`); res.StatusCode != http.StatusCreated {
			t.Fatalf("request %d: %d", i, res.StatusCode)
		}
	}
	res := post(t, ts.URL+"/api/links", `{"url":"https://a.co"}`)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("expected 429 with Retry-After, got %d", res.StatusCode)
	}
}

func TestHealth(t *testing.T) {
	ts, _ := newTestServer(nil)
	defer ts.Close()
	if res, _ := http.Get(ts.URL + "/healthz"); res.StatusCode != http.StatusOK {
		t.Fatalf("health: %d", res.StatusCode)
	}
}

// BenchmarkRedirect measures the redirect hot path (memory store, no network).
func BenchmarkRedirect(b *testing.B) {
	st := store.NewMemoryStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(shortener.New(st), st, ratelimit.New(1e9, 1e9), "http://sn.ip", log)
	h := srv.Handler()
	_, _ = shortener.New(st).Create(b.Context(), shortener.CreateRequest{URL: "https://go.dev", Alias: "bench"})
	req := httptest.NewRequest(http.MethodGet, "/bench", nil)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
}

func postFrom(t *testing.T, url, body, xff string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/api/links", bytes.NewBufferString(body))
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Reviewer's case: spoofing X-Forwarded-For must not reset the rate limit.
func TestSpoofedForwardedForDoesNotBypassRateLimit(t *testing.T) {
	ts, _ := newTestServer(ratelimit.New(0.001, 2))
	defer ts.Close()
	for i := 0; i < 2; i++ {
		postFrom(t, ts.URL, `{"url":"https://a.co"}`, "")
	}
	for i := 0; i < 5; i++ {
		res := postFrom(t, ts.URL, `{"url":"https://a.co"}`, fmt.Sprintf("203.0.113.%d", i))
		if res.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("spoofed header %d bypassed the limit: %d", i, res.StatusCode)
		}
	}
}

func TestClientIPWithTrustedProxy(t *testing.T) {
	trusted, err := ParseCIDRs("127.0.0.1, 10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{TrustedProxies: trusted}
	cases := []struct {
		remote, xff, want string
	}{
		{"198.51.100.7:5000", "1.2.3.4", "198.51.100.7"},             // untrusted peer: header ignored
		{"127.0.0.1:5000", "", "127.0.0.1"},                          // trusted peer, no header
		{"127.0.0.1:5000", "198.51.100.9", "198.51.100.9"},           // real client behind our proxy
		{"127.0.0.1:5000", "6.6.6.6, 198.51.100.9", "198.51.100.9"},  // forged left entry ignored
		{"127.0.0.1:5000", "198.51.100.9, 10.1.2.3", "198.51.100.9"}, // skip our inner proxies
		{"127.0.0.1:5000", "garbage", "127.0.0.1"},                   // malformed header
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := s.clientIP(r); got != c.want {
			t.Fatalf("remote=%s xff=%q: got %s want %s", c.remote, c.xff, got, c.want)
		}
	}
	if _, err := ParseCIDRs("not-an-ip"); err == nil {
		t.Fatal("expected error for invalid proxy")
	}
}

// Reviewer's cases: overflowing TTL and trailing garbage after JSON.
func TestReviewerInputsAreRejected(t *testing.T) {
	ts, _ := newTestServer(nil)
	defer ts.Close()
	cases := map[string]int{
		`{"url":"https://a.co","ttlHours":5124095578}`: http.StatusBadRequest,
		`{"url":"https://a.co","ttlHours":8761}`:       http.StatusBadRequest,
		`{"url":"https://a.co","ttlHours":-1}`:         http.StatusBadRequest,
		`{"url":"https://a.co"}INVALID`:                http.StatusBadRequest,
		`{"url":"https://a.co","ttlHours":24}`:         http.StatusCreated,
	}
	for body, want := range cases {
		if res := post(t, ts.URL+"/api/links", body); res.StatusCode != want {
			t.Fatalf("%s: got %d want %d", body, res.StatusCode, want)
		}
	}
}

// The internal link ID is never exposed by the API.
func TestLinkIDIsNotExposed(t *testing.T) {
	ts, _ := newTestServer(nil)
	defer ts.Close()
	res := post(t, ts.URL+"/api/links", `{"url":"https://a.co","alias":"noid"}`)
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), `"id"`) {
		t.Fatalf("response leaks internal id: %s", body)
	}
}
