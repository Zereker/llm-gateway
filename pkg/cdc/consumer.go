package cdc

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// EventHandler 单 stream 单事件的回调；通常由 TieredCache 实现：
//   - upsert → invalidate L1（下次请求走 L3 重新 load，或直接更新 L1）
//   - delete → invalidate L1
//
// table 是 stream 的来源表（"model_services" 等）。
type EventHandler func(ctx context.Context, table string, event *Event) error

// StreamConsumer 单 goroutine 阻塞 XREAD 多个 Redis Stream key；每个 stream 对应一个 table。
//
// **位点**：消费者侧不持久化 stream offset；启动时从 "$"（最新）开始读，
// 因为：
//   - 真权威是 MySQL（cold start 时 gateway 直接走 L3 MySQL fallback）
//   - 重复消费一条 event 是幂等的（invalidate + 重新 load 多次没副作用）
//
// 想做 at-least-once 持久化的，加 consumer group + XACK。本 v1 用 fan-out（每个
// gateway 实例独立 XREAD），适合多副本部署。
//
// **错误恢复**：XREAD 报错 → 退避重连；不 panic。
type StreamConsumer struct {
	rdb     *redis.Client
	streams map[string]string // stream_key → table_name
	handler EventHandler
	logger  *slog.Logger

	blockTimeout time.Duration
	batchCount   int64

	stop   chan struct{}
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

// ConsumerConfig 装配 StreamConsumer。
type ConsumerConfig struct {
	Redis        *redis.Client
	Streams      map[string]string // stream key → table name (e.g. "llm_gateway.llm_gateway.model_services" → "model_services")
	Handler      EventHandler      // 必填
	BlockTimeout time.Duration     // XREAD BLOCK 时间；默认 5s
	BatchCount   int64             // 单次 XREAD COUNT 上限；默认 50
	Logger       *slog.Logger
}

func NewStreamConsumer(cfg ConsumerConfig) *StreamConsumer {
	if cfg.BlockTimeout <= 0 {
		cfg.BlockTimeout = 5 * time.Second
	}
	if cfg.BatchCount <= 0 {
		cfg.BatchCount = 50
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &StreamConsumer{
		rdb:          cfg.Redis,
		streams:      cfg.Streams,
		handler:      cfg.Handler,
		logger:       log,
		blockTimeout: cfg.BlockTimeout,
		batchCount:   cfg.BatchCount,
		stop:         make(chan struct{}),
	}
}

// Run 启动后台 goroutine 持续 XREAD（非阻塞返回）。
func (c *StreamConsumer) Run(ctx context.Context) {
	if c.rdb == nil || c.handler == nil || len(c.streams) == 0 {
		c.logger.Warn("cdc.StreamConsumer: missing Redis/Handler/Streams; not running")
		return
	}
	c.wg.Add(1)
	go c.loop(ctx)
}

// Stop 等待后台 goroutine 退出。
func (c *StreamConsumer) Stop() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.stop)
	c.mu.Unlock()
	c.wg.Wait()
}

func (c *StreamConsumer) loop(parentCtx context.Context) {
	defer c.wg.Done()

	// stream offsets: 从 "$" 开始（仅新事件）。每次 XREAD 后用返回的最后 id 推进。
	offsets := make(map[string]string, len(c.streams))
	for k := range c.streams {
		offsets[k] = "$"
	}

	for {
		select {
		case <-c.stop:
			return
		case <-parentCtx.Done():
			return
		default:
		}

		// 构造 XREAD 入参：streams + 各自 offset
		args := &redis.XReadArgs{
			Block: c.blockTimeout,
			Count: c.batchCount,
		}
		args.Streams = make([]string, 0, len(c.streams)*2)
		// XREAD 协议：先所有 stream key，再所有 offset
		for key := range c.streams {
			args.Streams = append(args.Streams, key)
		}
		for _, key := range args.Streams[:len(c.streams)] {
			args.Streams = append(args.Streams, offsets[key])
		}

		ctx, cancel := context.WithTimeout(parentCtx, c.blockTimeout+2*time.Second)
		streams, err := c.rdb.XRead(ctx, args).Result()
		cancel()
		if err != nil {
			if errors.Is(err, redis.Nil) || errors.Is(err, context.DeadlineExceeded) {
				continue // BLOCK 超时无消息；正常
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			c.logger.WarnContext(parentCtx, "cdc: XREAD error", "err", err.Error())
			// 退避
			select {
			case <-time.After(1 * time.Second):
			case <-c.stop:
				return
			}
			continue
		}

		for _, s := range streams {
			table := c.streams[s.Stream]
			for _, msg := range s.Messages {
				// Debezium Server Redis sink 把 envelope 写在 message field "value"（默认）
				// 或者直接是顶层 key/value pair。两种都试。
				raw := extractMessageBody(msg.Values)
				if len(raw) == 0 {
					offsets[s.Stream] = msg.ID
					continue
				}
				event, err := ParseEvent(raw)
				if err != nil {
					c.logger.WarnContext(parentCtx, "cdc: parse event", "stream", s.Stream, "id", msg.ID, "err", err.Error())
					offsets[s.Stream] = msg.ID
					continue
				}
				if hErr := c.handler(parentCtx, table, event); hErr != nil {
					c.logger.WarnContext(parentCtx, "cdc: handler error", "table", table, "err", hErr.Error())
					// 错了也推进 offset（避免循环）
				}
				offsets[s.Stream] = msg.ID
			}
		}
	}
}

// extractMessageBody 从 Redis XADD field-value map 抠出 Debezium 消息体。
//
// Debezium Server Redis sink 默认把 envelope 写在 "value" field（key=record key，
// value=Debezium envelope JSON 串）。
func extractMessageBody(values map[string]interface{}) []byte {
	// 优先 "value" 字段
	if v, ok := values["value"]; ok {
		switch x := v.(type) {
		case string:
			return []byte(x)
		case []byte:
			return x
		}
	}
	// fallback: 找第一个 string 值
	for _, v := range values {
		switch x := v.(type) {
		case string:
			return []byte(x)
		case []byte:
			return x
		}
	}
	return nil
}
