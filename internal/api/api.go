// Package api exposes the shortener over HTTP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/navyamanthena/snip/internal/ratelimit"
	"github.com/navyamanthena/snip/internal/shortener"
	"github.com/navyamanthena/snip/internal/store"
)

// Server wires handlers, middleware and dependencies together.
type Server struct {
	svc     *shortener.Service
	store   store.Store
	limiter *ratelimit.Limiter
	baseURL string
	log     *slog.Logger

	// TrustedProxies are the load balancers allowed to set X-Forwarded-For.
	TrustedProxies []*net.IPNet

	requests  atomic.Int64
	redirects atomic.Int64
	limited   atomic.Int64
}

func NewServer(svc *shortener.Service, st store.Store, limiter *ratelimit.Limiter, baseURL string, log *slog.Logger) *Server {
	return &Server{svc: svc, store: st, limiter: limiter, baseURL: strings.TrimRight(baseURL, "/"), log: log}
}

// Handler returns the full HTTP handler with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/links", s.rateLimited(s.createLink))
	mux.HandleFunc("GET /api/links/{code}", s.getLink)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /{code}", s.redirect)
	return s.recoverer(s.logging(mux))
}

type createRequest struct {
	URL      string `json:"url"`
	Alias    string `json:"alias,omitempty"`
	TTLHours int    `json:"ttlHours,omitempty"`
}

type linkResponse struct {
	store.Link
	ShortURL string `json:"shortUrl"`
}

// newLinkResponse hides the internal link ID from API clients.
func (s *Server) newLinkResponse(l store.Link) linkResponse {
	l.ID = ""
	return linkResponse{Link: l, ShortURL: s.baseURL + "/" + l.Code}
}

func (s *Server) createLink(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := decodeStrict(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Check the raw number first: multiplying a huge value by time.Hour
	// would overflow and silently wrap to a small, valid-looking duration.
	if req.TTLHours < 0 || req.TTLHours > shortener.MaxTTLHours {
		writeError(w, http.StatusBadRequest, shortener.ErrInvalidTTL.Error())
		return
	}
	link, err := s.svc.Create(r.Context(), shortener.CreateRequest{
		URL:   req.URL,
		Alias: req.Alias,
		TTL:   time.Duration(req.TTLHours) * time.Hour,
	})
	switch {
	case errors.Is(err, shortener.ErrInvalidURL), errors.Is(err, shortener.ErrInvalidAlias),
		errors.Is(err, shortener.ErrInvalidTTL):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, shortener.ErrAliasTaken):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.log.Error("create link", "err", err)
		writeError(w, http.StatusInternalServerError, "could not create link")
		return
	}
	writeJSON(w, http.StatusCreated, s.newLinkResponse(link))
}

func (s *Server) getLink(w http.ResponseWriter, r *http.Request) {
	link, err := s.svc.Resolve(r.Context(), r.PathValue("code"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "link not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, s.newLinkResponse(link))
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	link, err := s.svc.Resolve(r.Context(), code)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	s.redirects.Add(1)
	// Count the click off the request path so redirects stay fast.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.svc.RecordClick(ctx, link); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("record click", "code", code, "err", err)
		}
	}()
	http.Redirect(w, r, link.URL, http.StatusFound)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int64{
		"requests_total":     s.requests.Load(),
		"redirects_total":    s.redirects.Load(),
		"rate_limited_total": s.limited.Load(),
	})
}

// --- middleware ---

func (s *Server) rateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := s.limiter.Allow(s.clientIP(r)); !ok {
			s.limited.Add(1)
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next(w, r)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.requests.Add(1)
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic", "value", v, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// clientIP uses the left-most X-Forwarded-For entry when behind a proxy.
// clientIP returns the address used for rate limiting.
//
// X-Forwarded-For is set by whoever sends the request, so it is only
// believed when the direct peer is one of our own trusted proxies. Even
// then, the header is read from right to left: entries appended by our
// proxies are skipped, and the first address added by anyone else is the
// client. Without trusted proxies the header is ignored entirely, so a
// client cannot dodge the limit by inventing a new "IP" per request.
func (s *Server) clientIP(r *http.Request) string {
	peer := remoteHost(r.RemoteAddr)
	if !s.isTrusted(peer) {
		return peer
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(parts[i])
		if ip == "" {
			continue
		}
		if net.ParseIP(ip) == nil {
			break // malformed entry: stop trusting the rest of the header
		}
		if !s.isTrusted(ip) {
			return ip
		}
	}
	return peer
}

func (s *Server) isTrusted(ip string) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, n := range s.TrustedProxies {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// ParseCIDRs parses a comma-separated list like "10.0.0.0/8,127.0.0.1".
// Bare IPs are treated as single-address networks.
func ParseCIDRs(list string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "/") {
			if ip := net.ParseIP(item); ip != nil && ip.To4() != nil {
				item += "/32"
			} else {
				item += "/128"
			}
		}
		_, n, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: %w", item, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// decodeStrict decodes exactly one JSON value and rejects anything after it
// (e.g. `{"a":1}garbage`), which json.Decoder alone would silently ignore.
func decodeStrict(body io.Reader, v any) error {
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("unexpected data after JSON body")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
