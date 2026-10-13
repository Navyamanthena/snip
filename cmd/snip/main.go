// Command snip runs the URL shortener HTTP server.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/navyamanthena/snip/internal/api"
	"github.com/navyamanthena/snip/internal/ratelimit"
	"github.com/navyamanthena/snip/internal/resp"
	"github.com/navyamanthena/snip/internal/shortener"
	"github.com/navyamanthena/snip/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	addr := env("ADDR", ":8080")
	baseURL := env("BASE_URL", "http://localhost:8080")

	var st store.Store
	if redisAddr := os.Getenv("REDIS_ADDR"); redisAddr != "" {
		client := resp.New(redisAddr)
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := client.Ping(ctx); err != nil {
			log.Error("cannot reach redis", "addr", redisAddr, "err", err)
			os.Exit(1)
		}
		cancel()
		st = store.NewRedisStore(client)
		log.Info("using redis store", "addr", redisAddr)
	} else {
		st = store.NewMemoryStore()
		log.Warn("REDIS_ADDR not set, using in-memory store (data is lost on restart)")
	}

	limiter := ratelimit.New(1, 10) // each IP: 10 creates at once, then 1/sec
	go func() {
		for range time.Tick(time.Minute) {
			limiter.Cleanup(10 * time.Minute)
		}
	}()

	trusted, err := api.ParseCIDRs(os.Getenv("TRUSTED_PROXIES"))
	if err != nil {
		log.Error("invalid TRUSTED_PROXIES", "err", err)
		os.Exit(2)
	}
	server := api.NewServer(shortener.New(st), st, limiter, baseURL, log)
	server.TrustedProxies = trusted

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown: finish in-flight requests on Ctrl+C / SIGTERM.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
