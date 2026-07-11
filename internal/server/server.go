// Package server is the process lifecycle manager: it centralizes the boilerplate of
// "open infra dependencies + run the service + gracefully exit on signal + close all
// dependencies in reverse order" in one place.
//
// cmd/gateway wires itself up through this package, saving it from writing its own
// lifecycle code.
//
// Usage:
//
//	s := server.New(slog.Default())
//	sqldb,    _ := s.OpenDB(cfg.Database)             // automatically registers db.Close
//	producer, _ := s.NewKafkaProducer(cfg.Kafka)      // automatically registers producer.Close
//	s.AddCloser("file-outbox", outbox.Close)          // custom cleanup
//	engine := router.NewEngine(...)
//	return s.Serve(addr, engine, readTimeout, shutdownTimeout)
//
// Three things happen automatically:
//
//  1. SIGINT / SIGTERM triggers graceful shutdown
//  2. http.Server.Shutdown waits for in-flight requests
//  3. All registered closers run in reverse order (LIFO) — dependencies created later
//     are closed first, avoiding dangling references
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/zereker/llm-gateway/internal/infra"
	"github.com/zereker/llm-gateway/internal/metric"
)

// Server holds the closer chain plus a logger; it is not a forced singleton — each
// binary creates its own via New.
type Server struct {
	mu      sync.Mutex
	closers []closeEntry
	log     *slog.Logger
}

type closeEntry struct {
	name string
	fn   func() error
}

// New constructs an empty Server; falls back to slog.Default() when log is nil.
func New(log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{log: log}
}

// AddCloser registers a cleanup callback.
//
// The close order is LIFO (last registered, first closed), the reverse of the order
// dependencies were established — this way an upper-layer dependency (e.g. a repo
// holding a *sqlx.DB) is never used after its underlying dependency (the *sqlx.DB
// itself) has already been closed.
func (s *Server) AddCloser(name string, fn func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closers = append(s.closers, closeEntry{name: name, fn: fn})
}

// OpenDB constructs a *sqlx.DB via infra.Open and automatically registers its Close.
func (s *Server) OpenDB(cfg infra.DBConfig) (*sqlx.DB, error) {
	db, err := infra.Open(cfg)
	if err != nil {
		return nil, err
	}
	s.AddCloser("db", db.Close)
	return db, nil
}

// NewKafkaProducer constructs a producer via infra.NewKafkaProducer and automatically
// registers its Close.
//
// The same producer can feed multiple outboxes (usage / audit / ...); when the cmd
// assembly layer holds a shared producer reference, open it **only once** via this
// method — do not also AddCloser outbox.Close (it would double-close).
func (s *Server) NewKafkaProducer(cfg infra.KafkaConfig) (*infra.KafkaProducer, error) {
	p, err := infra.NewKafkaProducer(cfg)
	if err != nil {
		return nil, err
	}
	s.AddCloser("kafka", p.Close)
	return p, nil
}

// OpenRedis constructs a *redis.Client via infra.OpenRedis and automatically registers
// its Close.
//
// gateway fails fast on ping at startup; M6 RateLimit requires Redis (there is no
// longer an in-memory fallback).
func (s *Server) OpenRedis(cfg infra.RedisConfig) (*redis.Client, error) {
	rdb, err := infra.OpenRedis(cfg)
	if err != nil {
		return nil, err
	}
	s.AddCloser("redis", rdb.Close)
	return rdb, nil
}

// Close runs all registered closers in reverse order. Idempotent (safe to call
// repeatedly).
//
// Use this for test / non-Serve paths (e.g. an error fast-fail path in a cmd's
// internal buildEngine). Serve also calls it before exiting; calling it again does
// not re-close the same resources (the internal closer list is cleared).
func (s *Server) Close() {
	if s == nil {
		// An error fast-fail path may run the defer cleanup before server.New's
		// return value has even landed (the named return gets overwritten to nil
		// by `return nil, ...`) — nil collapses to a no-op so that the real
		// startup error surfaces instead of being masked by a nil-deref panic.
		return
	}
	s.mu.Lock()
	closers := s.closers
	s.closers = nil
	s.mu.Unlock()

	for i := len(closers) - 1; i >= 0; i-- {
		e := closers[i]
		if err := e.fn(); err != nil {
			s.log.Warn("close failed", "name", e.name, "err", err)
		}
	}
}

// Serve starts the http.Server and blocks until SIGINT/SIGTERM is received or the
// server itself errors, then does a graceful Shutdown + Close (running the closer
// chain in reverse order).
//
// Return value: an error that occurred while the server was running (listen failure,
// accept failure, etc.); shutdown errors are only logged, not returned.
func (s *Server) Serve(addr string, handler http.Handler, readTimeout, shutdownTimeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return s.serveCtx(ctx, addr, handler, readTimeout, shutdownTimeout)
}

// serveCtx is Serve's internal implementation: canceling ctx triggers shutdown, which
// makes it convenient for tests to simulate a signal via cancel.
//
// **HTTP/2 (h2c) support**: wraps the handler with h2c.NewHandler so clients can use
// plaintext HTTP/2 (via PRI upgrade or prior knowledge). HTTP/1.1 clients are
// unaffected — the h2c handler automatically distinguishes protocols per request based
// on ALPN/preface. Production ingress typically terminates TLS and then speaks h2c to
// the upstream gateway; this configuration supports that directly.
func (s *Server) serveCtx(ctx context.Context, addr string, handler http.Handler, readTimeout, shutdownTimeout time.Duration) error {
	h2s := &http2.Server{}
	srv := &http.Server{
		Addr:              addr,
		Handler:           h2c.NewHandler(handler, h2s),
		ReadHeaderTimeout: readTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		s.log.Info("server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	var serveErr error
	select {
	case err := <-serverErr:
		serveErr = err
	case <-ctx.Done():
		s.log.Info("shutdown signal received")
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		// docs/08 §3: count of requests forcibly cut off by shutdown timeout (route dimension is unknown, unified as "*")
		s.log.Warn("http shutdown", "err", err)
		metric.Inc(metric.RequestAbortedByShutdown, "route", "*")
	}
	s.Close()
	return serveErr
}
