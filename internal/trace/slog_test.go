package trace

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestSlogTracer_Log(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	tr := NewSlogTracer(logger)

	tr.Log(context.Background(), "scheduling_decision", map[string]any{"vendor": "openai"})

	got := buf.String()
	if !strings.Contains(got, "scheduling_decision") {
		t.Fatalf("missing message: %s", got)
	}
	if !strings.Contains(got, "openai") {
		t.Fatalf("missing payload: %s", got)
	}
}

func TestSlogTracer_NilLoggerFallback(t *testing.T) {
	tr := NewSlogTracer(nil)
	if tr.Logger == nil {
		t.Fatal("Logger should fall back to slog.Default()")
	}
	tr.Log(context.Background(), "test", nil) // shouldn't panic
}

func TestSlogTracer_StartSpan_NoOp(t *testing.T) {
	tr := NewSlogTracer(nil)
	ctx, span := tr.StartSpan(context.Background(), "test_span")
	if ctx == nil {
		t.Fatal("StartSpan should return non-nil ctx")
	}
	span.SetAttribute("k", "v") // shouldn't panic
	span.End()                  // shouldn't panic
}
