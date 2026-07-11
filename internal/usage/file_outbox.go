package usage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// FileOutbox appends OutboxEvent.Payload to a local file in JSONL format.
//
// Design goals: source of truth (write failures must be transparently
// propagated) + high throughput (target 10k+ QPS).
//
// Performance notes:
//
//   - Single syscall write: payload + '\n' are merged into one buffer and
//     written with a single Write call, avoiding the double-syscall overhead
//     of "Write(payload) + Write('\n')" (each user-space <-> kernel-space
//     switch costs ~0.5-2μs).
//   - No explicit lock: *os.File.Write internally uses an fdmutex to
//     serialize writes on the same FD — as long as each Publish is a single
//     Write call, the kernel guarantees the whole line lands atomically and
//     won't interleave with another goroutine's Publish.
//   - buffer pool: uses sync.Pool to reuse the byte slice used for
//     appending, reducing GC pressure. Buffers grown by an oversized payload
//     are not returned to the pool (truncated per maxPooledBufSize) to avoid
//     a long tail bloating the pool.
//
// Not written via log/slog: slog's `Handler.Handle()` error is swallowed by
// `_ = ...`, so the caller has no way to know whether the write actually
// succeeded — which violates the observability requirement of being a
// source of truth. See docs/05 §5 for details.
type FileOutbox struct {
	mu sync.RWMutex // guards f against Close racing with concurrent Publish
	// (on the shutdown-timeout path a handler goroutine may still be in
	// Publish; without the lock, Close setting f to nil would cause a nil
	// deref panic)
	f *os.File
}

// Pooled buffer, reused for the append buffer; payloads are typically a few
// hundred bytes to a few KB, so we pre-allocate starting at 1KiB.
var fileOutboxBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 1024)
		return &b
	},
}

// maxPooledBufSize: buffers whose cap exceeds this are not returned to the
// pool (avoids the pool being bloated by an occasional oversized message).
const maxPooledBufSize = 64 * 1024

// NewFileOutbox opens (or creates) the file at path, writing in append mode.
//
// The directory containing path is created automatically.
func NewFileOutbox(path string) (*FileOutbox, error) {
	if path == "" {
		return nil, errors.New("usage: FileOutbox path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &FileOutbox{f: f}, nil
}

// Publish implements OutboxPublisher.Publish.
//
// Writes evt.Payload + '\n' in a single syscall; no fsync is performed (v0.1
// trades durability for throughput — the OS page cache flushes to disk
// asynchronously; callers needing stronger durability should fsync as needed).
func (o *FileOutbox) Publish(_ context.Context, evt *OutboxEvent) error {
	if evt == nil {
		return errors.New("usage: FileOutbox.Publish: nil event")
	}
	bp := fileOutboxBufPool.Get().(*[]byte)
	buf := (*bp)[:0]
	buf = append(buf, evt.Payload...)
	buf = append(buf, '\n')

	// RLock blocks Close from concurrently setting f to nil; the write
	// itself is still serialized by *os.File's internal fdmutex, and a
	// single Write call guarantees the kernel lands the whole line
	// atomically without interleaving with other Publish calls.
	o.mu.RLock()
	f := o.f
	var err error
	if f == nil {
		err = errors.New("usage: FileOutbox: closed")
	} else {
		_, err = f.Write(buf)
	}
	o.mu.RUnlock()

	// A buffer that a large payload grew is not returned to the pool, to
	// avoid subsequent small publishes continuously holding a large amount of memory
	if cap(buf) <= maxPooledBufSize {
		*bp = buf[:0]
		fileOutboxBufPool.Put(bp)
	}
	return err
}

// Close closes the underlying file; implements io.Closer so it can be called
// during graceful shutdown. Idempotent.
//
// The write lock waits for all in-flight Publish calls' RLocks to release
// before closing the file — so a handler goroutine still running on the
// shutdown-timeout path won't hit a nil deref, and subsequent Publish calls
// get a "closed" error.
func (o *FileOutbox) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.f == nil {
		return nil
	}
	err := o.f.Close()
	o.f = nil
	return err
}

// Compile-time assertion: FileOutbox satisfies OutboxPublisher + io.Closer.
var (
	_ OutboxPublisher = (*FileOutbox)(nil)
	_ io.Closer       = (*FileOutbox)(nil)
)
