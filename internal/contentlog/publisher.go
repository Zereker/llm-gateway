package contentlog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// =============================================================================
// FilePublisher: JSONL append
// =============================================================================

// FilePublisher serializes a Record to JSONL and appends it to a file.
//
// This is the only real backend Content Log currently supports: the gateway only writes
// local JSONL, and fluent-bit / vector ships it downstream to the various sinks (archival /
// retrieval / post-hoc content-safety review / training-data feedback). See
// docs/architecture/05-metering-billing.md §2 + docs/07-configuration.md §2.
//
// File rotation / compression / cleanup is handled by an external logrotate or log
// collector, not by this process.
type FilePublisher struct {
	mu sync.Mutex
	w  io.WriteCloser
}

// NewFilePublisher opens (or creates) the file at the given path for append writes.
func NewFilePublisher(path string) (*FilePublisher, error) {
	// Content logs can contain prompts, responses, identifiers, and policy
	// metadata. New files are owner-readable only; operators that deliberately
	// share them with a collector can widen permissions outside the process.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("contentlog: open file: %w", err)
	}

	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("contentlog: secure file permissions: %w", err)
	}

	return &FilePublisher{w: f}, nil
}

// Publish serializes the record and appends it as a JSON line plus a newline.
func (p *FilePublisher) Publish(_ context.Context, r *Record) error {
	buf, err := json.Marshal(r)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, err := p.w.Write(buf); err != nil {
		return err
	}

	_, err = p.w.Write([]byte("\n"))

	return err
}

// Close closes the file.
func (p *FilePublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.w == nil {
		return nil
	}

	err := p.w.Close()
	p.w = nil

	return err
}
