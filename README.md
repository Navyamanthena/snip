# Snip: URL Shortener in Go

A production-style URL shortening service written in Go with **zero third-party
dependencies**. It includes its own Redis client that speaks the RESP wire protocol directly.

**Stack:** Go 1.24 (standard library only) · Redis · Docker · GitHub Actions

## Features

- **Short links**: cryptographically random 7-character base62 codes (≈3.5 trillion
  combinations) with automatic retry on collision.
- **Custom aliases and expiring links**: `my-resume` style aliases plus optional TTLs, which
  are enforced with native Redis key expiry.
- **Atomic writes**: Lua scripts guarantee two requests can never claim the same code, and
  click counters always share their link's lifetime (no orphaned or carried-over counts).
- **Click analytics**: counted asynchronously so redirects stay fast.
- **Rate limiting**: per-IP token bucket (burst 10, then 1 request/second) with a `Retry-After`
  header and automatic eviction of idle buckets. `X-Forwarded-For` is only trusted from
  configured proxies (`TRUSTED_PROXIES`), so clients can't dodge the limit by spoofing it.
- **Hand-written Redis client**: RESP2 encoder/decoder with a connection pool, deadlines and
  broken-connection handling (`internal/resp`).
- **Operational basics**: structured JSON logging (`log/slog`), panic recovery, `/healthz`,
  `/metrics`, server timeouts and graceful shutdown.

## Architecture

```
client ──▶ net/http mux ──▶ middleware (recover → log → rate limit)
                               │
                               ▼
                       shortener.Service  (validation, code generation)
                               │
                               ▼
                 store.Store interface
                 ├── RedisStore  (internal/resp client, production)
                 └── MemoryStore (tests / local without Redis)
```

The `Store` interface lets the same contract test run against both implementations.

## API

```bash
# Create a short link (alias and ttlHours are optional)
curl -X POST localhost:8080/api/links \
  -d '{"url":"https://github.com/navyamanthena","alias":"gh","ttlHours":72}'
# → {"code":"gh","url":"https://github.com/navyamanthena","shortUrl":"http://localhost:8080/gh",...}

curl -i localhost:8080/gh            # 302 redirect
curl localhost:8080/api/links/gh     # link details + click count
curl localhost:8080/healthz          # health check
curl localhost:8080/metrics          # request / redirect / rate-limit counters
```

| Status | When |
|--------|------|
| 201 | Link created |
| 400 | Invalid URL, alias or TTL |
| 409 | Alias already taken |
| 429 | Rate limit exceeded |

## Run it

```bash
# With Docker (Redis + app)
docker compose up --build

# Or locally (in-memory store, no Redis needed)
go run ./cmd/snip

# Or locally against Redis
REDIS_ADDR=localhost:6379 go run ./cmd/snip
```

| Env var | Default | Meaning |
|---------|---------|---------|
| `REDIS_ADDR` | (unset = in-memory) | Redis address |
| `ADDR` | `:8080` | Listen address |
| `BASE_URL` | `http://localhost:8080` | Prefix for returned short URLs |
| `TRUSTED_PROXIES` | (none) | Comma-separated IPs/CIDRs of load balancers allowed to set `X-Forwarded-For` |

## Tests

```bash
go test -race -cover ./...                         # unit + HTTP tests
REDIS_ADDR=localhost:6379 go test ./...            # also runs the real-Redis tests
go test -bench=Redirect ./internal/api             # redirect hot-path benchmark
```

Coverage is about 80–100% per package. CI runs the full suite against a real Redis container.
