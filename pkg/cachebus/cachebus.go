// Package cachebus 是控制面 → 数据面的**窄缓存失效通道**（Redis pub/sub）。
//
// **为什么存在**：两个面默认通过 MySQL + 数据面 TTL 缓存（≤30s）被动同步——这对
// 加 endpoint / 改价格这类**功能**变更够用。但 api_key **吊销**是**安全**事件：
// 让泄漏的 key 再有效 30s 不可接受。cachebus 只给这类安全敏感失效开一条精准通道：
// 控制面 PUBLISH 一条 `apikey:<hash>`，数据面订阅后 evict 那一个 key，把窗口从
// ≤30s 收到亚秒级。
//
// **有意收窄**：它**不是**通用缓存一致性协议——只广播"这个 key/资源失效了，谁缓存
// 了就删"。不做版本、不做全量刷新、不进认证热路径（认证仍走本地 LRU，只有失效事件
// 才碰 Redis pub/sub）。Redis 不可用时退化成纯 TTL（打 warn），不阻塞任何一面。
//
// **已知窄窗（evict-without-version）**：一个在吊销**之前**就开始、在 evict 之后才
// 完成的 in-flight Resolve，可能把"当时还有效"的身份写回缓存，最长残留一个正向 TTL
// （30s）。窗口 = 单次 Resolve 跨越吊销的时长，很窄但真实存在。彻底关闭需要给缓存项
// 引入 generation/version token；当前以 30s TTL 作兜底，属可接受取舍。
package cachebus

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// DefaultChannel 是默认的 Redis pub/sub 频道名。
const DefaultChannel = "llm-gateway:cache:invalidate"

// Kind 标记失效的资源类型。Phase 0/1 只有 apikey；后续可加 endpoint / subscription。
type Kind string

const (
	// KindAPIKey：Key 是 api_key_hash（SHA-256 hex）。
	KindAPIKey Kind = "apikey"
)

// Invalidation 一条失效消息。线格式：`<kind>:<key>`。
type Invalidation struct {
	Kind Kind
	Key  string
}

func (inv Invalidation) encode() string { return string(inv.Kind) + ":" + inv.Key }

func decode(payload string) (Invalidation, bool) {
	i := strings.IndexByte(payload, ':')
	if i <= 0 || i == len(payload)-1 {
		return Invalidation{}, false
	}
	return Invalidation{Kind: Kind(payload[:i]), Key: payload[i+1:]}, true
}

// Publisher 往失效频道发消息（控制面用）。
type Publisher struct {
	rdb     *redis.Client
	channel string
}

// NewPublisher 构造 Publisher；channel 为空用 DefaultChannel。
func NewPublisher(rdb *redis.Client, channel string) *Publisher {
	if channel == "" {
		channel = DefaultChannel
	}
	return &Publisher{rdb: rdb, channel: channel}
}

// Invalidate PUBLISH 一条失效消息。best-effort：Redis 挂了返错由调用方 warn，
// 不阻塞控制面写操作（DB 已经落库，TTL 兜底最终一致）。
func (p *Publisher) Invalidate(ctx context.Context, inv Invalidation) error {
	if p == nil || p.rdb == nil {
		return nil
	}
	if err := p.rdb.Publish(ctx, p.channel, inv.encode()).Err(); err != nil {
		return fmt.Errorf("cachebus: publish: %w", err)
	}
	return nil
}

// Subscriber 订阅失效频道，把每条消息派发给 handler（数据面用）。
type Subscriber struct {
	rdb     *redis.Client
	channel string
	handler func(Invalidation)
}

// NewSubscriber 构造 Subscriber；channel 为空用 DefaultChannel。
func NewSubscriber(rdb *redis.Client, channel string, handler func(Invalidation)) *Subscriber {
	if channel == "" {
		channel = DefaultChannel
	}
	return &Subscriber{rdb: rdb, channel: channel, handler: handler}
}

// Start 订阅频道并起后台 goroutine 派发。**同步确认订阅成功**后才返回——返回后
// 的 Publish 一定被投递（测试可确定性断言）。返回的 stop 关闭订阅、结束 goroutine。
func (s *Subscriber) Start(ctx context.Context) (stop func(), err error) {
	pubsub := s.rdb.Subscribe(ctx, s.channel)
	// Receive 一次确认订阅已建立（*redis.Subscription），失败即 Redis 不可用。
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, fmt.Errorf("cachebus: subscribe: %w", err)
	}
	ch := pubsub.Channel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range ch {
			if inv, ok := decode(msg.Payload); ok && s.handler != nil {
				s.handler(inv)
			}
		}
	}()
	return func() {
		_ = pubsub.Close()
		<-done
	}, nil
}
