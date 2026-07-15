package runtime

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
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
