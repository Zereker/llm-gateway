package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/infra"
)

func TestServer_AddCloserOrderIsLIFO(t *testing.T) {
	s := New(silent())
	var order []string
	s.AddCloser("a", func() error { order = append(order, "a"); return nil })
	s.AddCloser("b", func() error { order = append(order, "b"); return nil })
	s.AddCloser("c", func() error { order = append(order, "c"); return nil })

	s.Close()

	want := []string{"c", "b", "a"}
	if len(order) != 3 {
		t.Fatalf("ran %d closers, want 3", len(order))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, order[i], want[i])
		}
	}
}

func TestServer_CloseChainContinuesOnError(t *testing.T) {
	s := New(silent())
	var ran []string
	s.AddCloser("a", func() error { ran = append(ran, "a"); return nil })
	s.AddCloser("b", func() error { ran = append(ran, "b"); return errors.New("boom") })
	s.AddCloser("c", func() error { ran = append(ran, "c"); return nil })

	s.Close()

	if len(ran) != 3 {
		t.Errorf("ran = %v, want all 3 (error in middle should not stop chain)", ran)
	}
}

func TestServer_CloseIsIdempotent(t *testing.T) {
	s := New(silent())
	calls := 0
	s.AddCloser("x", func() error { calls++; return nil })

	s.Close()
	s.Close() // second call should not run again (closer list already cleared)
	s.Close()

	if calls != 1 {
		t.Errorf("calls = %d, want 1 (Close must be idempotent)", calls)
	}
}

func TestServer_OpenDBRegistersClose(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping OpenDB integration test")
	}
	s := New(silent())
	db, err := s.OpenDB(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if db == nil {
		t.Fatal("db is nil")
	}
	if err := db.Ping(); err != nil {
		t.Errorf("Ping before Close: %v", err)
	}

	s.Close()

	if err := db.Ping(); err == nil {
		t.Error("Ping after Close should fail (db should be closed)")
	}
}

func TestServer_NewKafkaProducerRegistersClose(t *testing.T) {
	s := New(silent())
	p, err := s.NewKafkaProducer(infra.KafkaConfig{Brokers: []string{"localhost:9092"}})
	if err != nil {
		t.Fatalf("NewKafkaProducer: %v", err)
	}
	if p == nil {
		t.Fatal("producer is nil")
	}

	// The close chain should reach kafka's Close (closing a never-used producer is a no-op, no error)
	s.Close()
}

func TestServer_NewKafkaProducer_BadConfig(t *testing.T) {
	s := New(silent())
	_, err := s.NewKafkaProducer(infra.KafkaConfig{}) // empty brokers
	if err == nil {
		t.Fatal("want error for empty brokers")
	}
	// Key: a closer must not be registered on failure
	if len(s.closers) != 0 {
		t.Errorf("closers = %d, want 0 after failed open", len(s.closers))
	}
}

func TestServer_ServeShutdownOnCtxCancel(t *testing.T) {
	s := New(silent())

	var closeRan atomic.Bool
	s.AddCloser("test", func() error { closeRan.Store(true); return nil })

	// Grab a free port first (avoids listen conflicts from OS port reuse)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.serveCtx(ctx, addr, handler, time.Second, 2*time.Second)
	}()

	// Wait for the listener to come up before sending a verification request (up to 1s)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel() // trigger shutdown

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveCtx returned err: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveCtx didn't return after ctx cancel")
	}

	if !closeRan.Load() {
		t.Error("close chain didn't run on shutdown")
	}
}

func TestServer_ServeReturnsListenError(t *testing.T) {
	s := New(silent())
	// A privileged port like "0.0.0.0:1" is rejected in most environments, but a more
	// reliable approach is to use a clearly invalid addr: ":99999" (port out of range)
	err := s.serveCtx(context.Background(), ":99999",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
		time.Second, time.Second)
	if err == nil {
		t.Error("want listen error for invalid port")
	}
}

// silent returns a logger that discards all output, keeping test output clean.
func silent() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
