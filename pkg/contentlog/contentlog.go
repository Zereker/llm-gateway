// Package contentlog 内容记录通道：审计 / 排障 / 合规 / 回放
//（docs/architecture/05-metering-billing.md §2 + docs/08 §6）。
//
// 默认关闭；按需开启。通过 pkg/upstream hooks 接入 Sender 字节流：
//
//	ClientRequest   → 客户端原始请求 body
//	UpstreamRequest → 翻译后发上游 body
//	UpstreamChunk   → 上游原始响应 chunk
//	ClientChunk     → 客户端实际收到 chunk
//
// **三大特性**：
//   - **采样**：按比例 / 按 account / endpoint 决定是否记录
//   - **Backpressure**：buffer 满时 drop_oldest / drop_newest / block；block 必有 timeout
//   - **脱敏**：可选 Redactor 钩子；失败按配置 drop 或写摘要
//
// **不能影响业务响应**（docs/05 §2）：异步 buffer，hook 内 enqueue 后立即返回；
// 失败仅 metric / log，不抛错。
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

// Direction 内容方向（docs/05 §2 表）。
type Direction string

const (
	DirClientRequest   Direction = "client_request"
	DirUpstreamRequest Direction = "upstream_request"
	DirUpstreamChunk   Direction = "upstream_chunk"
	DirClientChunk     Direction = "client_chunk"
)

// Record 单条内容记录（docs/05 §2）。
type Record struct {
	RequestID    string    `json:"request_id"`
	TraceID      string    `json:"trace_id"`
	AccountID    string    `json:"account_id"`
	APIKeyID     string    `json:"api_key_id"`
	SubAccountID string    `json:"sub_account_id"`
	Model        string    `json:"model"`
	Vendor       string    `json:"vendor"`
	EndpointID   string    `json:"endpoint_id"`

	Direction   Direction `json:"direction"`
	Protocol    string    `json:"protocol"`
	Modality    string    `json:"modality"`
	ContentType string    `json:"content_type,omitempty"`

	Body       []byte    `json:"body,omitempty"`        // truncated 后的 body；如配置 object storage 则为空
	BodySHA256 string    `json:"body_sha256"`
	Truncated  bool      `json:"truncated"`
	Redacted   bool      `json:"redacted,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Publisher 后端实现：异步 buffer + 真正写出。
//
// 同 OutboxPublisher 一样的可插拔模型：file / kafka / async_kafka。
type Publisher interface {
	Publish(ctx context.Context, r *Record) error
}

// Redactor 可选脱敏钩子。返回 (脱敏后 body, 是否已脱敏)；error = 脱敏失败。
//
// 实现 MUST be safe for concurrent use。
type Redactor interface {
	Redact(direction Direction, body []byte) ([]byte, bool, error)
}

// =============================================================================
// Config
// =============================================================================

// BackpressureStrategy buffer 满时的处理策略。
type BackpressureStrategy int

const (
	BackpressureDropOldest BackpressureStrategy = iota // 默认；丢最老一条
	BackpressureDropNewest                             // 丢本次（不入队）
	BackpressureBlock                                  // 阻塞直到 timeout；不能用作默认
)

// Config Logger 装配参数。
type Config struct {
	Publisher    Publisher
	Redactor     Redactor // nil = 不脱敏
	SampleRate   float64  // [0,1]；1.0 = 全量；0 = 全丢
	MaxBodyBytes int      // > 0 时截断 body；<= 0 = 不截断
	BufferSize   int      // 异步队列容量；默认 1024
	Backpressure BackpressureStrategy
	BlockTimeout time.Duration // BackpressureBlock 才有意义
	OnDrop       func(reason string) // 可选回调（metric / log）
}

// Logger 内容记录入口；实现 upstream Hook 各 Observer 接口。
//
// 单实例可作为 hook 同时挂多个 Observer 子接口（实现 ClientRequestObserver /
// UpstreamRequestObserver / UpstreamChunkObserver / ClientChunkObserver）。
type Logger struct {
	cfg     Config
	queue   chan *Record
	stop    chan struct{}
	wg      sync.WaitGroup
	dropped atomic.Int64

	// 上下文采样状态（per-request 决定是否抽样；避免一个请求里 client / upstream
	// 一半在采另一半丢）
	sampleSeed atomic.Uint64
}

// New 构造 Logger 并启动 worker。
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

// Close 优雅停止 worker：等队列排空后退出。block 直到 worker 退出。
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

// Dropped 截至当前累计丢弃数。
func (l *Logger) Dropped() int64 { return l.dropped.Load() }

// =============================================================================
// upstream Observer 接口
// =============================================================================

// OnClientRequest 实现 ClientRequestObserver。
func (l *Logger) OnClientRequest(ctx context.Context, ep *domain.Endpoint, body []byte) {
	l.enqueue(ctx, ep, DirClientRequest, body)
}

// OnUpstreamRequest 实现 UpstreamRequestObserver。
func (l *Logger) OnUpstreamRequest(ctx context.Context, ep *domain.Endpoint, body []byte) {
	l.enqueue(ctx, ep, DirUpstreamRequest, body)
}

// OnUpstreamChunk 实现 UpstreamChunkObserver。
//
// **chunk 在回调返回后失效**——必须 copy。
func (l *Logger) OnUpstreamChunk(ctx context.Context, ep *domain.Endpoint, chunk []byte) {
	l.enqueue(ctx, ep, DirUpstreamChunk, chunk)
}

// OnClientChunk 实现 ClientChunkObserver。
func (l *Logger) OnClientChunk(ctx context.Context, ep *domain.Endpoint, chunk []byte) {
	l.enqueue(ctx, ep, DirClientChunk, chunk)
}

// =============================================================================
// 内部：采样 / 截断 / 脱敏 / 入队
// =============================================================================

func (l *Logger) enqueue(ctx context.Context, ep *domain.Endpoint, dir Direction, body []byte) {
	if l == nil || l.cfg.Publisher == nil {
		return
	}
	if !l.shouldSample() {
		metric.Inc(metric.ContentLogPublishTotal, "backend", "logger", "result", "sampled_out", "sampled", "false")
		return
	}

	// 1) copy body（chunk slice 在 callback 返回后失效）
	bodyCopy := append([]byte(nil), body...)

	// 2) 截断
	truncated := false
	if l.cfg.MaxBodyBytes > 0 && len(bodyCopy) > l.cfg.MaxBodyBytes {
		bodyCopy = bodyCopy[:l.cfg.MaxBodyBytes]
		truncated = true
	}

	// 3) 脱敏
	redacted := false
	if l.cfg.Redactor != nil {
		out, ok, err := l.cfg.Redactor.Redact(dir, bodyCopy)
		if err != nil {
			// 脱敏失败：保守 drop（避免泄露未脱敏内容）
			l.drop("redact_failed")
			return
		}
		bodyCopy = out
		redacted = ok
	}

	// 4) hash（脱敏后的）
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

	// 5) 入队（按 backpressure 策略）
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
				// 丢最老的，腾位置
				select {
				case <-l.queue:
					l.drop("drop_oldest")
				default:
					// 别人正好消费走了，再试一次入队
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

// shouldSample 简单 0..1 概率采样（math/rand 全局，thread-safe）。
//
// 上下文级一致性不在 v0.5 范围（同请求里 client/upstream 一半采一半丢可接受）。
func (l *Logger) shouldSample() bool {
	if l.cfg.SampleRate >= 1.0 {
		return true
	}
	if l.cfg.SampleRate <= 0 {
		return false
	}
	// 简单 RNG；不引入额外依赖
	v := l.sampleSeed.Add(0x9E3779B97F4A7C15) // golden ratio constant
	frac := float64(v%1000_000) / 1_000_000.0
	return frac < l.cfg.SampleRate
}

// worker 单线程消费 queue，调 Publisher.Publish。
func (l *Logger) worker() {
	defer l.wg.Done()
	for {
		select {
		case <-l.stop:
			// 排空剩余
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
// ctx 提取 request 元信息（避免 logger 直依赖 middleware）
// =============================================================================

// 跟 middleware.requestContextKey 用同一 typed key 形态，但 contentlog 无法 import
// middleware（会形成循环依赖）。通过 docs/08 §1 的字段规范，logger 走 ctxValueLookup
// 接口让 caller（M7 / Tracing）填上 enrichment。
type ctxEnrichKey struct{}

// EnrichCtx 把单次请求的元信息塞进 ctx；Sender 会带 ctx 透传到所有 hook 回调。
func EnrichCtx(ctx context.Context, e RequestEnrich) context.Context {
	return context.WithValue(ctx, ctxEnrichKey{}, e)
}

// RequestEnrich 一次请求要附加到 Record 上的元信息。
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

// formatInt 简单 int64 → string（避免 import strconv 在多处用）
func formatInt(v int64) string {
	if v == 0 {
		return ""
	}
	// 用 stdlib strconv 即可，但本包没用其它 strconv 调用，inline 一个简单实现
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

// 占位防 unused import
var _ = errors.New
