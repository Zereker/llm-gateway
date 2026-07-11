package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileOutbox_PublishAppendsJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.log")
	o, err := NewFileOutbox(path)
	if err != nil {
		t.Fatalf("NewFileOutbox: %v", err)
	}
	defer o.Close()

	if err := o.Publish(context.Background(), &OutboxEvent{Payload: []byte(`{"a":1}`), Key: "k1"}); err != nil {
		t.Fatalf("Publish 1: %v", err)
	}
	if err := o.Publish(context.Background(), &OutboxEvent{Payload: []byte(`{"a":2}`), Key: "k2"}); err != nil {
		t.Fatalf("Publish 2: %v", err)
	}

	data, _ := os.ReadFile(path)
	got := string(data)
	wantLines := []string{`{"a":1}`, `{"a":2}`}
	for _, w := range wantLines {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s in: %q", w, got)
		}
	}
	if strings.Count(got, "\n") != 2 {
		t.Errorf("want 2 newlines, got: %q", got)
	}
}

func TestFileOutbox_NilEventReturnsError(t *testing.T) {
	dir := t.TempDir()
	o, _ := NewFileOutbox(filepath.Join(dir, "u.log"))
	defer o.Close()

	if err := o.Publish(context.Background(), nil); err == nil {
		t.Fatal("want error for nil event")
	}
}

func TestFileOutbox_EmptyPathRejected(t *testing.T) {
	if _, err := NewFileOutbox(""); err == nil {
		t.Fatal("want error for empty path")
	}
}

func TestFileOutbox_CloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	o, _ := NewFileOutbox(filepath.Join(dir, "u.log"))
	if err := o.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatalf("second close (idempotent): %v", err)
	}
}
