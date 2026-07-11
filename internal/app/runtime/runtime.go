// Package runtime owns process infrastructure and lifecycle. HTTP routing and
// serving remain independent from application dependency construction.
package runtime

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

	"github.com/zereker/llm-gateway/pkg/infra"
	"github.com/zereker/llm-gateway/pkg/metric"
)

// Runtime owns opened infrastructure and closes it in reverse order.
type Runtime struct {
	mu      sync.Mutex
	closers []closeEntry
	log     *slog.Logger
}

type closeEntry struct {
	name string
	fn   func() error
}

func New(log *slog.Logger) *Runtime {
	if log == nil {
		log = slog.Default()
	}
	return &Runtime{log: log}
}

func (r *Runtime) AddCloser(name string, fn func() error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closers = append(r.closers, closeEntry{name: name, fn: fn})
}

func (r *Runtime) OpenDB(cfg infra.DBConfig) (*sqlx.DB, error) {
	db, err := infra.Open(cfg)
	if err != nil {
		return nil, err
	}
	r.AddCloser("db", db.Close)
	return db, nil
}

func (r *Runtime) OpenRedis(cfg infra.RedisConfig) (*redis.Client, error) {
	client, err := infra.OpenRedis(cfg)
	if err != nil {
		return nil, err
	}
	r.AddCloser("redis", client.Close)
	return client, nil
}

func (r *Runtime) NewKafkaProducer(cfg infra.KafkaConfig) (*infra.KafkaProducer, error) {
	producer, err := infra.NewKafkaProducer(cfg)
	if err != nil {
		return nil, err
	}
	r.AddCloser("kafka", producer.Close)
	return producer, nil
}

func (r *Runtime) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	closers := r.closers
	r.closers = nil
	r.mu.Unlock()
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i].fn(); err != nil {
			r.log.Warn("close failed", "name", closers[i].name, "err", err)
		}
	}
}

// Serve runs an h2c-capable HTTP server until a signal or serving error.
func (r *Runtime) Serve(addr string, handler http.Handler, readTimeout, shutdownTimeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Addr: addr, Handler: h2c.NewHandler(handler, &http2.Server{}),
		ReadHeaderTimeout: readTimeout,
	}
	serverErr := make(chan error, 1)
	go func() {
		r.log.Info("server listening", "addr", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()
	var serveErr error
	select {
	case serveErr = <-serverErr:
	case <-ctx.Done():
		r.log.Info("shutdown signal received")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		r.log.Warn("http shutdown", "err", err)
		metric.Inc(metric.RequestAbortedByShutdown, "route", "*")
	}
	r.Close()
	return serveErr
}
