package middleware

import (
	"strings"
	"testing"
)

func TestGenTraceID_Format(t *testing.T) {
	id := genTraceID()
	if !strings.HasPrefix(id, "tr_") {
		t.Errorf("got %q, want prefix tr_", id)
	}
	if len(id) != 3+16 { // "tr_" + 16 hex
		t.Errorf("len = %d, want %d", len(id), 3+16)
	}
}

func TestGenRequestID_Format(t *testing.T) {
	id := genRequestID()
	if !strings.HasPrefix(id, "req_") {
		t.Errorf("got %q, want prefix req_", id)
	}
	if len(id) != 4+12 { // "req_" + 12 hex
		t.Errorf("len = %d, want %d", len(id), 4+12)
	}
}

func TestGenTraceID_Unique(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 1000; i++ {
		id := genTraceID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate trace ID after %d iterations: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}
