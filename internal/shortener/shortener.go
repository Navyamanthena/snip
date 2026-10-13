// Package shortener contains the core rules for creating short links.
package shortener

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/navyamanthena/snip/internal/store"
)

const (
	alphabet   = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	codeLength = 7 // 62^7 ≈ 3.5 trillion possible codes
	maxRetries = 5
	maxTTL     = 365 * 24 * time.Hour

	// MaxTTLHours is the largest ttlHours accepted from clients (365 days).
	MaxTTLHours = 365 * 24
)

var (
	ErrInvalidURL   = errors.New("url must be an absolute http or https URL")
	ErrInvalidAlias = errors.New("alias must be 3-32 characters: letters, digits, '-' or '_'")
	ErrAliasTaken   = errors.New("alias is already taken")
	ErrInvalidTTL   = errors.New("ttl must be between 1 hour and 365 days")

	aliasPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)
	reserved     = map[string]bool{"api": true, "healthz": true, "metrics": true}
)

// Service creates and resolves links.
type Service struct {
	store store.Store
	now   func() time.Time
}

func New(s store.Store) *Service { return &Service{store: s, now: time.Now} }

// CreateRequest is what a caller asks for.
type CreateRequest struct {
	URL   string
	Alias string        // optional custom code
	TTL   time.Duration // optional; 0 = never expires
}

// Create validates the request and stores a new link, generating a random
// code (and retrying on the rare collision) unless an alias is given.
func (s *Service) Create(ctx context.Context, req CreateRequest) (store.Link, error) {
	target, err := normalizeURL(req.URL)
	if err != nil {
		return store.Link{}, err
	}
	id, err := randomCode(16)
	if err != nil {
		return store.Link{}, err
	}
	link := store.Link{ID: id, URL: target, CreatedAt: s.now().UTC()}
	if req.TTL != 0 {
		if req.TTL < time.Hour || req.TTL > maxTTL {
			return store.Link{}, ErrInvalidTTL
		}
		exp := link.CreatedAt.Add(req.TTL)
		link.ExpiresAt = &exp
	}

	if req.Alias != "" {
		if !aliasPattern.MatchString(req.Alias) || reserved[strings.ToLower(req.Alias)] {
			return store.Link{}, ErrInvalidAlias
		}
		link.Code = req.Alias
		if err := s.store.Create(ctx, link); err != nil {
			if errors.Is(err, store.ErrExists) {
				return store.Link{}, ErrAliasTaken
			}
			return store.Link{}, err
		}
		return link, nil
	}

	for i := 0; i < maxRetries; i++ {
		code, err := randomCode(codeLength)
		if err != nil {
			return store.Link{}, err
		}
		link.Code = code
		err = s.store.Create(ctx, link)
		if err == nil {
			return link, nil
		}
		if !errors.Is(err, store.ErrExists) {
			return store.Link{}, err
		}
	}
	return store.Link{}, fmt.Errorf("could not generate a unique code after %d attempts", maxRetries)
}

// Resolve returns the link for a code.
func (s *Service) Resolve(ctx context.Context, code string) (store.Link, error) {
	return s.store.Get(ctx, code)
}

// RecordClick counts a click for the exact link that was resolved, so a
// click finishing after the link expired can't land on a newer link that
// reused the same code.
func (s *Service) RecordClick(ctx context.Context, link store.Link) error {
	return s.store.IncrClicks(ctx, link.Code, link.ID)
}

func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 2048 {
		return "", ErrInvalidURL
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", ErrInvalidURL
	}
	return u.String(), nil
}

// randomCode returns n characters drawn uniformly from the base62 alphabet
// using a cryptographically secure source, so codes can't be guessed.
func randomCode(n int) (string, error) {
	b := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = alphabet[idx.Int64()]
	}
	return string(b), nil
}
