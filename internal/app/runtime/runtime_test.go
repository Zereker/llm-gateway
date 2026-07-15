package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/infra"
)

func TestNewUsesDefaultLogger(t *testing.T) {
	t.Parallel()

	runtime := New(nil)
	if runtime.log == nil {
		t.Fatal("New(nil) returned a runtime without a logger")
	}
}

func TestCloseRunsClosersInReverseOrderOnce(t *testing.T) {
	t.Parallel()

	runtime := New(slog.Default())
	var order []string
	runtime.AddCloser("first", func() error {
		order = append(order, "first")
		return nil
	})
	runtime.AddCloser("second", func() error {
		order = append(order, "second")
		return nil
	})

	runtime.Close()
	runtime.Close()

	want := []string{"second", "first"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("close order = %v, want %v", order, want)
	}
}

func TestCloseLogsErrorAndContinues(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	runtime := New(logger)
	var closed bool
	runtime.AddCloser("healthy", func() error {
		closed = true
		return nil
	})
	runtime.AddCloser("broken", func() error {
		return errors.New("close boom")
	})

	runtime.Close()

	if !closed {
		t.Fatal("a failing closer prevented the remaining closer from running")
	}
	logLine := output.String()
	if !strings.Contains(logLine, "close failed") ||
		!strings.Contains(logLine, "broken") ||
		!strings.Contains(logLine, "close boom") {
		t.Fatalf("close error log = %q", logLine)
	}
}

func TestNilRuntimeClose(t *testing.T) {
	t.Parallel()

	var runtime *Runtime
	runtime.Close()
}

func TestOpenDBRegistersClose(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping OpenDB integration test")
	}

	runtime := New(slog.Default())
	db, err := runtime.OpenDB(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping before Close: %v", err)
	}

	runtime.Close()
	if err := db.Ping(); err == nil {
		t.Fatal("Ping after Close succeeded; DB was not closed")
	}
}

func TestNewKafkaProducerRejectsBadConfigWithoutCloser(t *testing.T) {
	runtime := New(slog.Default())
	if _, err := runtime.NewKafkaProducer(infra.KafkaConfig{}); err == nil {
		t.Fatal("NewKafkaProducer with empty brokers succeeded")
	}
	if len(runtime.closers) != 0 {
		t.Fatalf("closers = %d, want 0 after failed construction", len(runtime.closers))
	}
}

func TestServeContextShutsDownAndClosesResources(t *testing.T) {
	runtime := New(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	var closed atomic.Bool
	runtime.AddCloser("resource", func() error {
		closed.Store(true)
		return nil
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runtime.serveContext(ctx, addr, http.NotFoundHandler(), time.Second, 2*time.Second)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveContext: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveContext did not return after cancellation")
	}

	if !closed.Load() {
		t.Fatal("runtime resources were not closed")
	}
}

func TestServeReturnsListenErrorAndClosesResources(t *testing.T) {
	t.Parallel()

	runtime := New(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	var closed bool
	runtime.AddCloser("resource", func() error {
		closed = true
		return nil
	})

	err := runtime.Serve("127.0.0.1:-1", http.NotFoundHandler(), time.Second, time.Second)
	if err == nil {
		t.Fatal("Serve() error = nil, want an invalid-address listen error")
	}
	if !closed {
		t.Fatal("Serve() did not close runtime resources after the server failed")
	}
}
