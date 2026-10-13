package store

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/navyamanthena/snip/internal/resp"
)

// RedisStore keeps each link as JSON under "link:<code>" and its click
// counter under "clicks:<code>". Expiring links use native Redis TTLs, so
// expired data is cleaned up by Redis itself.
type RedisStore struct {
	c *resp.Client
}

func NewRedisStore(c *resp.Client) *RedisStore { return &RedisStore{c: c} }

func linkKey(code string) string   { return "link:" + code }
func clicksKey(code string) string { return "clicks:" + code }

// createScript claims the code and clears any click counter left over from
// an earlier link with the same code, in one atomic step.
// KEYS: 1 link, 2 clicks. ARGV: 1 JSON, 2 TTL ms ("0" = never expires).
const createScript = `
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
if ARGV[2] == '0' then
  redis.call('SET', KEYS[1], ARGV[1])
else
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
end
redis.call('DEL', KEYS[2])
return 1`

func (s *RedisStore) Create(ctx context.Context, l Link) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	ttl := "0"
	if l.ExpiresAt != nil {
		ms := time.Until(*l.ExpiresAt).Milliseconds()
		if ms <= 0 {
			ms = 1
		}
		ttl = strconv.FormatInt(ms, 10)
	}
	reply, err := s.c.Do(ctx, "EVAL", createScript, "2", linkKey(l.Code), clicksKey(l.Code), string(data), ttl)
	if err != nil {
		return err
	}
	if reply != int64(1) {
		return ErrExists
	}
	return nil
}

func (s *RedisStore) Get(ctx context.Context, code string) (Link, error) {
	reply, err := s.c.Do(ctx, "MGET", linkKey(code), clicksKey(code))
	if err != nil {
		return Link{}, err
	}
	vals, _ := reply.([]any)
	raw, ok := vals[0].(string)
	if !ok {
		return Link{}, ErrNotFound
	}
	var l Link
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		return Link{}, err
	}
	if c, ok := vals[1].(string); ok {
		l.Clicks, _ = strconv.ParseInt(c, 10, 64)
	}
	return l, nil
}

// incrScript counts a click only if the link still exists, and gives the
// counter exactly the link's remaining lifetime, atomically. So a counter
// can never outlive its link or leak into a future link that reuses the code.
// The link's ID is checked too: a slow click from an expired link must not
// be counted against a new link that has since reused the same code.
// KEYS: 1 link, 2 clicks. ARGV: 1 link ID.
// Returns the new count, or -1 if the link is gone or has been replaced.
const incrScript = `
local raw = redis.call('GET', KEYS[1])
if not raw then return -1 end
if cjson.decode(raw)['id'] ~= ARGV[1] then return -1 end
local n = redis.call('INCR', KEYS[2])
local ttl = redis.call('PTTL', KEYS[1])
if ttl > 0 then
  redis.call('PEXPIRE', KEYS[2], ttl)
else
  redis.call('PERSIST', KEYS[2])
end
return n`

func (s *RedisStore) IncrClicks(ctx context.Context, code, linkID string) error {
	reply, err := s.c.Do(ctx, "EVAL", incrScript, "2", linkKey(code), clicksKey(code), linkID)
	if err != nil {
		return err
	}
	if reply == int64(-1) {
		return ErrNotFound
	}
	return nil
}

func (s *RedisStore) Ping(ctx context.Context) error { return s.c.Ping(ctx) }
