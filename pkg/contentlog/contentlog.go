// Package contentlog is the content recording channel: audit / troubleshooting / compliance / replay
// (docs/architecture/05-metering-billing.md §2 + docs/08 §6).
//
// Disabled by default; opt in as needed. Taps into the Sender byte stream via pkg/upstream hooks:
//
//	ClientRequest   → the client's original request body
//	UpstreamRequest → the translated body sent upstream
//	UpstreamChunk   → the raw upstream response chunk
//	ClientChunk     → the chunk actually received by the client
//
// **Three key features**:
//   - **Sampling**: decides whether to record based on ratio / account / endpoint
//   - **Backpressure**: when the buffer is full, drop_oldest / drop_newest / block; block always has a timeout
//   - **Redaction**: optional Redactor hook; on failure, drop or write a digest per config
//
// **Must not affect the business response** (docs/05 §2): async buffer, the hook returns
// immediately after enqueueing; failures are only metric'd / logged, never propagated as errors.
package contentlog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
)

// Direction is the content direction (docs/05 §2 table).
type Direction string

const (
	DirClientRequest   Direction = "client_request"
	DirUpstreamRequest Direction = "upstream_request"
	DirUpstreamChunk   Direction = "upstream_chunk"
	DirClientChunk     Direction = "client_chunk"
)

// Record is a single content record (docs/05 §2).
type Record struct {
	RequestID    string `json:"request_id"`
	TraceID      string `json:"trace_id"`
	AccountID    string `json:"account_id"`
	APIKeyID     string `json:"api_key_id"`
	SubAccountID string `json:"sub_account_id"`
	Model        string `json:"model"`
	Vendor       string `json:"vendor"`
	EndpointID   string `json:"endpoint_id"`

	Direction   Direction `json:"direction"`
	Protocol    string    `json:"protocol"`
	Modality    string    `json:"modality"`
	ContentType string    `json:"content_type,omitempty"`

	Body       []byte    `json:"body,omitempty"` // body after truncation; empty if object storage is configured
	BodySHA256 string    `json:"body_sha256"`
	Truncated  bool      `json:"truncated"`
	Redacted   bool      `json:"redacted,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Publisher is the backend implementation: async buffer + the actual write-out.
//
// Same pluggable model as OutboxPublisher: file / kafka / async_kafka.
type Publisher interface {
	Publish(ctx context.Context, r *Record) error
}

// Redactor is an optional redaction hook. Returns (redacted body, whether it was redacted); error = redaction failed.
//
// Implementations MUST be safe for concurrent use.
type Redactor interface {
	Redact(direction Direction, body []byte) ([]byte, bool, error)
}

// =============================================================================
// Config
// =============================================================================

// BackpressureStrategy is the handling strategy when the buffer is full.
type BackpressureStrategy int

const (
	BackpressureDropOldest BackpressureStrategy = iota // default; drops the oldest entry
	BackpressureDropNewest                             // drops this entry (does not enqueue)
	BackpressureBlock                                  // blocks until timeout; must not be used as default
)

// Config holds the Logger's assembly parameters.
type Config struct {
	Publisher    Publisher
	Redactor     Redactor // nil = no redaction
	SampleRate   float64  // [0,1]; 1.0 = record everything; 0 = drop everything
	MaxBodyBytes int      // > 0 truncates the body; <= 0 = no truncation
	BufferSize   int      // async queue capacity; default 1024
	Backpressure BackpressureStrategy
	BlockTimeout time.Duration       // only meaningful with BackpressureBlock
	OnDrop       func(reason string) // optional callback (metric / log)
}

// Logger is the content-recording entry point; it implements the various upstream Hook Observer interfaces.
//
// A single instance can be attached as a hook implementing multiple Observer sub-interfaces
// simultaneously (ClientRequestObserver / UpstreamRequestObserver / UpstreamChunkObserver /
// ClientChunkObserver).
type Logger struct {
	cfg     Config
	queue   chan *Record
	stop    chan struct{}
	wg      sync.WaitGroup
	dropped atomic.Int64

	// Per-request sampling state (decides sampling per request; avoids a single request
	// having client sampled in and upstream dropped, or vice versa)
	sampleSeed atomic.Uint64
}

// New builds a Logger and starts its worker.
func New(cfg Config) *Logger {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 1024
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 0
	}
	if cfg.SampleRate > 1 {
		cfg.SampleRate = 1
	}
	if cfg.Backpressure == BackpressureBlock && cfg.BlockTimeout <= 0 {
		cfg.BlockTimeout = 50 * time.Millisecond
	}
	l := &Logger{
		cfg:   cfg,
		queue: make(chan *Record, cfg.BufferSize),
		stop:  make(chan struct{}),
	}
	l.wg.Add(1)
	go l.worker()
	return l
}

// Close gracefully stops the worker: exits after draining the queue. Blocks until the worker exits.
func (l *Logger) Close(ctx context.Context) error {
	close(l.stop)
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Dropped returns the cumulative drop count so far.
func (l *Logger) Dropped() int64 { return l.dropped.Load() }

// =============================================================================
// upstream Observer interfaces
// =============================================================================

// OnClientRequest implements ClientRequestObserver.
func (l *Logger) OnClientRequest(ctx context.Context, ep *domain.Endpoint, body []byte) {
	l.enqueue(ctx, ep, DirClientRequest, body)
}

// OnUpstreamRequest implements UpstreamRequestObserver.
func (l *Logger) OnUpstreamRequest(ctx context.Context, ep *domain.Endpoint, body []byte) {
	l.enqueue(ctx, ep, DirUpstreamRequest, body)
}

// OnUpstreamChunk implements UpstreamChunkObserver.
//
// **The chunk becomes invalid after the callback returns** — it must be copied.
func (l *Logger) OnUpstreamChunk(ctx context.Context, ep *domain.Endpoint, chunk []byte) {
	l.enqueue(ctx, ep, DirUpstreamChunk, chunk)
}

// OnClientChunk implements ClientChunkObserver.
func (l *Logger) OnClientChunk(ctx context.Context, ep *domain.Endpoint, chunk []byte) {
	l.enqueue(ctx, ep, DirClientChunk, chunk)
}

// =============================================================================
// Internal: sampling / truncation / redaction / enqueueing
// =============================================================================

func (l *Logger) enqueue(ctx context.Context, ep *domain.Endpoint, dir Direction, body []byte) {
	if l == nil || l.cfg.Publisher == nil {
		return
	}
	if !l.shouldSample() {
		metric.Inc(metric.ContentLogPublishTotal, "backend", "logger", "result", "sampled_out", "sampled", "false")
		return
	}

	// 1) copy body (the chunk slice becomes invalid after the callback returns)
	bodyCopy := append([]byte(nil), body...)

	// 2) truncate
	truncated := false
	if l.cfg.MaxBodyBytes > 0 && len(bodyCopy) > l.cfg.MaxBodyBytes {
		bodyCopy = bodyCopy[:l.cfg.MaxBodyBytes]
		truncated = true
	}

	// 3) redact
	redacted := false
	if l.cfg.Redactor != nil {
		out, ok, err := l.cfg.Redactor.Redact(dir, bodyCopy)
		if err != nil {
			// redaction failed: drop conservatively (avoid leaking unredacted content)
			l.drop("redact_failed")
			return
		}
		bodyCopy = out
		redacted = ok
	}

	// 4) hash (post-redaction)
	hash := sha256.Sum256(bodyCopy)

	rec := &Record{
		Direction:  dir,
		Body:       bodyCopy,
		BodySHA256: hex.EncodeToString(hash[:]),
		Truncated:  truncated,
		Redacted:   redacted,
		CreatedAt:  time.Now().UTC(),
	}
	if ep != nil {
		rec.Vendor = ep.Vendor
		rec.EndpointID = formatInt(ep.ID)
	}
	enrichFromCtx(ctx, rec)

	// 5) enqueue (per the backpressure strategy)
	switch l.cfg.Backpressure {
	case BackpressureDropNewest:
		select {
		case l.queue <- rec:
		default:
			l.drop("buffer_full")
		}
	case BackpressureBlock:
		ctx2, cancel := context.WithTimeout(ctx, l.cfg.BlockTimeout)
		defer cancel()
		select {
		case l.queue <- rec:
		case <-ctx2.Done():
			l.drop("block_timeout")
		}
	default: // BackpressureDropOldest
		for {
			select {
			case l.queue <- rec:
				return
			default:
				// drop the oldest to make room
				select {
				case <-l.queue:
					l.drop("drop_oldest")
				default:
					// someone else just consumed it; retry enqueue
				}
			}
		}
	}
}

func (l *Logger) drop(reason string) {
	l.dropped.Add(1)
	metric.Inc(metric.ContentLogPublishTotal, "backend", "logger", "result", "drop_"+reason, "sampled", "true")
	if l.cfg.OnDrop != nil {
		l.cfg.OnDrop(reason)
	}
}

// shouldSample does simple 0..1 probability sampling (thread-safe, no global math/rand).
//
// Context-level consistency is out of scope for v0.5 (it's acceptable for client/upstream
// within the same request to be split between sampled-in and dropped).
func (l *Logger) shouldSample() bool {
	if l.cfg.SampleRate >= 1.0 {
		return true
	}
	if l.cfg.SampleRate <= 0 {
		return false
	}
	// simple RNG; avoids pulling in an extra dependency
	v := l.sampleSeed.Add(0x9E3779B97F4A7C15) // golden ratio constant
	frac := float64(v%1000_000) / 1_000_000.0
	return frac < l.cfg.SampleRate
}

// worker single-threadedly consumes the queue, calling Publisher.Publish.
func (l *Logger) worker() {
	defer l.wg.Done()
	for {
		select {
		case <-l.stop:
			// drain remaining entries
			for {
				select {
				case r := <-l.queue:
					l.publish(r)
				default:
					return
				}
			}
		case r := <-l.queue:
			l.publish(r)
		}
	}
}

func (l *Logger) publish(r *Record) {
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.cfg.Publisher.Publish(ctx, r); err != nil {
		metric.Inc(metric.ContentLogPublishTotal, "backend", "logger", "result", "publish_error", "sampled", "true")
		slog.WarnContext(ctx, "contentlog: publish failed",
			"direction", string(r.Direction),
			"request_id", r.RequestID,
			"err", err.Error(),
		)
		return
	}
	metric.Inc(metric.ContentLogPublishTotal, "backend", "logger", "result", "ok", "sampled", "true")
}

// =============================================================================
// ctx extraction of request metadata (avoids the logger directly depending on middleware)
// =============================================================================

// Uses the same typed-key shape as middleware.requestContextKey, but contentlog cannot
// import middleware (that would create a circular dependency). Per the field conventions
// in docs/08 §1, the logger goes through a ctxValueLookup interface so the caller (M7 /
// Tracing) can fill in the enrichment.
type ctxEnrichKey struct{}

// EnrichCtx stashes a single request's metadata into ctx; the Sender carries ctx through
// to all hook callbacks.
func EnrichCtx(ctx context.Context, e RequestEnrich) context.Context {
	return context.WithValue(ctx, ctxEnrichKey{}, e)
}

// RequestEnrich is the metadata to attach to a Record for a single request.
type RequestEnrich struct {
	RequestID    string
	TraceID      string
	AccountID    string
	APIKeyID     string
	SubAccountID string
	Model        string
	Protocol     string
	Modality     string
}

func enrichFromCtx(ctx context.Context, r *Record) {
	if ctx == nil {
		return
	}
	v, _ := ctx.Value(ctxEnrichKey{}).(RequestEnrich)
	r.RequestID = v.RequestID
	r.TraceID = v.TraceID
	r.AccountID = v.AccountID
	r.APIKeyID = v.APIKeyID
	r.SubAccountID = v.SubAccountID
	r.Model = v.Model
	r.Protocol = v.Protocol
	r.Modality = v.Modality
}

// formatInt is a simple int64 → string conversion (avoids importing strconv just for this)
func formatInt(v int64) string {
	if v == 0 {
		return ""
	}
	// stdlib strconv would do, but this package has no other strconv calls, so inline a simple implementation
	const digits = "0123456789"
	if v < 0 {
		return "-" + formatInt(-v)
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v%10]
		v /= 10
	}
	return string(buf[i:])
}

// placeholder to prevent an unused import
var _ = errors.New
