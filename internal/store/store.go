// Package store persists short links.
package store

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound means the code does not exist or has expired.
	ErrNotFound = errors.New("link not found")
	// ErrExists means the code is already taken.
	ErrExists = errors.New("code already exists")
)

// Link is a shortened URL.
type Link struct {
	// ID uniquely identifies this link, even if its code is later reused by
	// a new link after expiry. Clicks are only counted when the ID matches.
	ID        string     `json:"id,omitempty"`
	Code      string     `json:"code"`
	URL       string     `json:"url"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	Clicks    int64      `json:"clicks"`
}

// Store is implemented by RedisStore (production) and MemoryStore (tests / local).
type Store interface {
	// Create saves the link only if the code is unused (returns ErrExists otherwise).
	Create(ctx context.Context, l Link) error
	Get(ctx context.Context, code string) (Link, error)
	// IncrClicks counts a click for the link with this code, but only if it
	// is still the same link (linkID matches); otherwise ErrNotFound.
	IncrClicks(ctx context.Context, code, linkID string) error
	Ping(ctx context.Context) error
}
