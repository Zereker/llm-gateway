# 06 — Pluggable Infrastructure

本文定义网关与外部基础设施的所有交互接口：身份系统、预算 / 配额、配置中心、共享缓存 / 限流存储、计量事件总线、内容审核、可观测性。

每个接口都有：
- **`interface` 签名**（生产代码契约）
- **默认实现**（零外部依赖，适合本地开发 / 单机部署）
- **可选实现**（生产场景可挂的常见基础设施）
- **注入点**（在哪个 middleware / 组件 Deps 中使用）

> **阅读前**：[01](01-request-pipeline.md) 的 middleware Deps；[03](03-endpoint-scheduling.md) 的 CooldownManager Store；[04](04-rate-limiting.md) 的 Lua 脚本调用；[05](05-metering-billing.md) 的 EventBus。

## 1. 设计原则

| # | 原则 | 含义 |
|---|------|------|
| I1 | **零外部依赖即可启动** | 所有接口都有内置默认实现（in-memory / file），单机运行不需要 Redis / Kafka / etcd / 任何 SaaS |
| I2 | **生产可挂常见组件** | 同一接口，开发用本地实现、生产挂 Redis / etcd / Kafka，无需改业务代码 |
| I3 | **Deps 显式注入** | Middleware / 组件通过构造函数或 `Deps` struct 接收接口实例；杜绝全局单例 |
| I4 | **NoOp 实现** | 可选功能（审核 / Tracer 等）允许 nil 或 NoOp，不影响主链路 |
| I5 | **无锁逃生** | 失败必须有兜底策略（默认放行 / 本地缓存 / 旁路告警），不能因一个外部依赖挂掉拖垮全量请求 |

## 2. 包结构

```
pkg/
├── auth/
│   ├── provider.go             # IdentityProvider 接口
│   ├── apikey/                 # 默认：基于配置文件 / DB 的 API Key 鉴权
│   └── jwt/                    # 可选：JWT (HS256 / RS256) 鉴权
│
├── budget/
│   ├── checker.go              # Checker 接口
│   ├── alwayspass/             # 默认：永远通过
│   └── inmemory/               # 可选：内存配额
│
├── config/
│   ├── store.go                # Store 接口
│   ├── file/                   # 默认：本地 YAML / TOML 文件 + fsnotify 热加载
│   ├── etcd/                   # 可选：etcd v3 + Watch
│   └── sqlite/                 # 可选：SQLite + 轮询
│
├── cache/
│   ├── store.go                # Store 接口（KV + 原子操作 + Lua）
│   ├── memory/                 # 默认：sync.Map + 周期清理
│   └── redis/                  # 可选：go-redis
│
├── eventbus/                   # usage.OutboxPublisher 实现
│   ├── file/                   # 默认：本地 JSONL append
│   ├── memory/                 # 仅测试
│   └── kafka/                  # 可选：sarama / kgo
│
├── moderation/
│   ├── moderator.go            # Moderator 接口
│   ├── noop/                   # 默认：什么都不做
│   └── openai/                 # 可选：调 OpenAI moderation API
│
└── tracing/
    ├── tracer.go               # Tracer 接口
    ├── slog/                   # 默认：stdlib slog 输出
    └── otel/                   # 可选：OpenTelemetry
```

## 3. middleware.IdentityProvider

M2 Auth middleware 的依赖（详见 [01] 第 5 节）。

```go
// pkg/middleware/provider.go
package middleware

import "context"

type Credentials struct {
    APIKey       string // "Authorization: Bearer xxx" 或 "X-API-Key: xxx" 提取
    BearerToken  string // JWT 形态时使用
    Headers      map[string]string // 完整透传，自定义实现可用
}

type Identity struct {
    UserID    string
    APIKeyID  string
    Group     string // "default" / "reserved" / 任意自定义
    External  bool   // true = 外部用户走预算检查；false = 内部 / 免费
}

type IdentityProvider interface {
    Resolve(ctx context.Context, creds *Credentials) (*Identity, error)
}
```

### 3.1 默认：APIKey 文件 / 内存

```go
// pkg/middleware/apikey/provider.go
package apikey

type Provider struct {
    Keys map[string]domain.UserIdentity // key=APIKey 字符串
}

func NewFromFile(path string) (*Provider, error) {
    // 解析 YAML：
    // - api_key: "sk-xxx"
    //   user_id: "alice"
    //   group: "default"
    //   external: true
}

func (p *Provider) Resolve(ctx context.Context, c *middleware.Credentials) (*domain.UserIdentity, error) {
    id, ok := p.Keys[c.APIKey]
    if !ok {
        return nil, errors.New("unknown api key")
    }
    return &id, nil
}
```

### 3.2 可选：JWT

```go
// pkg/middleware/jwt/provider.go
package jwt

type Provider struct {
    Issuer    string
    JWKSURL   string // 远端公钥
    Algorithm string // "HS256" / "RS256"
    Secret    []byte // HS256 用
}

// Resolve 校验 JWT 签名 + 过期；从 claims 中提取 user_id / group / external
```

## 4. middleware.BudgetGate

M4 Budget middleware 的依赖。

```go
// pkg/middleware/checker.go
package middleware

import (
    "context"

    "github.com/zereker-labs/ai-gateway/pkg/domain"
)

type Checker interface {
    Check(ctx context.Context, userID string) (domain.BudgetStatus, error)
}
```

### 4.1 默认：AlwaysPass

```go
// pkg/middleware/alwayspass/checker.go
package alwayspass

type Checker struct{}

func (Checker) Check(_ context.Context, _ string) (domain.BudgetStatus, error) {
    return domain.BudgetActive, nil
}
```

> 适用场景：单机 / 内部部署；无付费体系；零依赖启动。

### 4.2 可选：内存配额

```go
// pkg/middleware/inmemory/checker.go
package inmemory

type Checker struct {
    store map[string]domain.BudgetStatus // 由外部管理（Admin API 写）
    mu    sync.RWMutex
}

func (c *Checker) Set(userID string, status domain.BudgetStatus) { /* mu Lock + write */ }
func (c *Checker) Check(_ context.Context, userID string) (domain.BudgetStatus, error) {
    c.mu.RLock(); defer c.mu.RUnlock()
    if s, ok := c.store[userID]; ok { return s, nil }
    return domain.BudgetActive, nil // 默认放行
}
```

### 4.3 自定义实现

接入外部计费系统（如自家订阅、Stripe、AWS Marketplace）时，实现 `Checker`：

```go
type StripeChecker struct {
    Client *stripe.Client
    Cache  cache.Store // 三级缓存兜底
}

func (c *StripeChecker) Check(ctx, userID string) (domain.BudgetStatus, error) {
    // 1. L1 本地内存
    // 2. L2 Cache (Redis)
    // 3. L3 Stripe API
    // 失败默认 Active + 告警
}
```

## 5. store.KV

`modelservice.Loader` / `ratelimit.ConfigStore` / `schedule.Profile` 等都依赖此接口下发配置。

```go
// pkg/store/store.go
package config

import (
    "context"
    "encoding/json"
)

type Store interface {
    // Get 读单个 key
    Get(ctx context.Context, key string) (json.RawMessage, error)

    // List 读 prefix 下所有 (key, value)
    List(ctx context.Context, prefix string) (map[string]json.RawMessage, error)

    // Watch 订阅 prefix 下的变更事件（增 / 改 / 删）
    Watch(ctx context.Context, prefix string) (<-chan Event, error)

    // Put 写入；Admin API 用
    Put(ctx context.Context, key string, value json.RawMessage) error

    // Delete 删除；Admin API 用
    Delete(ctx context.Context, key string) error
}

type Event struct {
    Type  EventType
    Key   string
    Value json.RawMessage
}

type EventType int

const (
    EventPut EventType = iota
    EventDelete
)
```

### 5.1 默认：文件 + fsnotify

```go
// pkg/store/file/store.go
package file

type Store struct {
    Root string // 文件根目录；每个 key 对应一个 .json 文件
    // 用 fsnotify 监听文件变更，触发 Watch event
}
```

> 配置文件结构示例：
> ```
> /etc/ai-gateway/
> ├── modelservice/svc_gpt4o.json
> ├── ratelimit/apikey/ak_xxx/svc_gpt4o.json
> └── scheduling/profile/svc_gpt4o.json
> ```

### 5.2 可选：etcd v3

```go
// pkg/store/etcd/store.go
package etcd

type Store struct {
    Client *clientv3.Client
    Prefix string // "/ai-gateway/"
}
```

支持原生 Watch，事件实时推送；强一致；多实例共享。

### 5.3 可选：SQLite + 轮询

```go
// pkg/store/sqlite/store.go
package sqlite

type Store struct {
    DB *sql.DB
    PollInterval time.Duration // Watch 通过周期 SELECT 模拟
}
```

适合单机 + 持久化场景；Watch 延迟取决于轮询周期。

### 5.4 Key 分层约定

```
/ai-gateway/
├── modelservice/{service_id}              → domain.ModelServiceSnapshot
├── ratelimit/
│   ├── apikey/{api_key_id}/{service_id}   → domain.LayerSpec
│   ├── user/{user_id}/{service_id}        → domain.LayerSpec
│   └── service/{service_id}               → ratelimit.ServiceLimits
├── scheduling/
│   ├── profile/{service_id}               → schedule.Profile
│   └── endpoint/{endpoint_id}             → domain.Endpoint
├── identity/{user_id}                     → domain.UserIdentity (可选；APIKey 实现可不用)
└── budget/{user_id}                       → domain.BudgetStatus (可选)
```

## 6. cache.Store

限流 Lua 脚本 / Cooldown Manager / 配置二级缓存的存储后端。

```go
// pkg/cache.go (deleted; semantics moved into ratelimit Checker impl)
package cache

import (
    "context"
    "time"
)

type Store interface {
    // 基础 KV
    Get(ctx context.Context, key string) ([]byte, error) // 不存在返回 nil, nil
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
    Del(ctx context.Context, key string) error
    Exists(ctx context.Context, key string) (bool, error)

    // 原子计数
    Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
    IncrBy(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error)

    // 限流脚本（Lua / lua-style）
    EvalLimit(ctx context.Context, key string, cap, incr int64, ttlSec int64) (current int64, blocked bool, err error)
}
```

### 6.1 默认：内存

```go
// pkg/cache.go (deleted)
package memory

type Store struct {
    items map[string]item
    mu    sync.RWMutex
    // 周期 GC：扫过期 key 删除
}

type item struct {
    value     []byte
    expiresAt time.Time
}
```

> 单实例使用；进程重启丢失；测试 / 单机部署足够。

### 6.2 可选：Redis

```go
// pkg/cache.go (deleted)
package redis

type Store struct {
    Client redis.UniversalClient
    Prefix string // "ai-gateway:"
}

// EvalLimit 用 Lua 脚本（详见 [04] 第 5.1 节）
```

支持集群 / 哨兵 / 主从；多实例共享；生产推荐。

## 7. usage.OutboxPublisher

[05] 计量事件的传输通道。

```go
// pkg/usage/outbox.go（接口定义见 [05] 第 6.4 节）
```

### 7.1 默认：本地文件 JSONL

```go
// pkg/usage/file_outbox.go (内置默认实现)
package file

type Bus struct {
    Writer *zap.Logger // 配 lumberjack
}

func (b *Bus) Publish(ctx context.Context, evt *usage.OutboxEvent) error {
    b.Writer.Info("usage_event", zap.ByteString("payload", evt.Payload))
    return nil // zap 同步 writer 失败抛错
}
```

> 适合本地开发；生产用 Filebeat 等收集到 ELK / S3。

### 7.2 可选：Kafka

```go
// pkg/usage/kafka/bus.go
package kafka

type Bus struct {
    Producer sarama.SyncProducer
    Topic    string
}

func (b *Bus) Publish(ctx context.Context, evt *usage.OutboxEvent) error {
    msg := &sarama.ProducerMessage{
        Topic: b.Topic,
        Key:   sarama.StringEncoder(evt.Key),
        Value: sarama.ByteEncoder(evt.Payload),
    }
    _, _, err := b.Producer.SendMessage(msg)
    return err
}
```

> 配 `acks=1` + `lz4` 压缩 + `linger.ms=10` 平衡延迟 / 吞吐。详见 [05] 第 6 节。

### 7.3 可选：内存（仅测试）

```go
// pkg/usage/memory/bus.go
package memory

type Bus struct {
    Channel chan *usage.OutboxEvent
}
```

## 8. middleware.Moderator

M8 Content Moderation middleware 的依赖（可选，允许 nil）。

```go
// pkg/middleware/moderator.go
package middleware

import (
    "context"

    "github.com/zereker-labs/ai-gateway/pkg/domain"
)

type Moderator interface {
    CheckInput(ctx context.Context, env *domain.RequestEnvelope) error  // 违规返回 error
    CheckOutput(ctx context.Context, chunk []byte) error            // 流式审核（Session 集成）
}
```

### 8.1 默认：NoOp

无审核需求时直接传 `nil`；M8 检测到 nil 即跳过。

### 8.2 可选：调 OpenAI moderation API

```go
// pkg/middleware/openai/moderator.go
package openai

type Moderator struct {
    Client *openai.Client
    Categories []string // ["hate", "sexual", "violence", ...]
}
```

### 8.3 可选：本地分类器

```go
// pkg/middleware/local/moderator.go
package local

type Moderator struct {
    Model *llama.Model // ggml/llama.cpp 加载本地模型分类
}
```

## 9. trace.Tracer

M10 Tracing middleware 的依赖。

```go
// pkg/trace/tracer.go
package trace

import "context"

type Tracer interface {
    // Log 写一条结构化日志（带 trace_id 等上下文）
    Log(ctx context.Context, name string, payload any)

    // Span 开启一个 span（可选 OTel 集成）
    StartSpan(ctx context.Context, name string) (context.Context, Span)
}

type Span interface {
    SetAttribute(key string, value any)
    End()
}
```

### 9.1 默认：stdlib slog

```go
// pkg/trace/slog/tracer.go
package slog

type Tracer struct {
    Logger *slog.Logger
}

func (t *Tracer) Log(ctx context.Context, name string, payload any) {
    t.Logger.InfoContext(ctx, name, slog.Any("payload", payload))
}
// StartSpan 返回 NoOp Span
```

### 9.2 可选：OpenTelemetry

```go
// pkg/trace/otel/tracer.go
package otel

type Tracer struct {
    Tracer otel.Tracer
}

// 完整接入 OTLP exporter（Jaeger / Zipkin / Tempo / vendors）
```

## 10. modelservice.Loader

M5 ModelService middleware 的依赖；底层走 `store.KV` + LRU 缓存。

```go
// pkg/domain/loader.go
package modelservice

import "context"

type Loader interface {
    GetByModel(ctx context.Context, model string) (*Snapshot, error)
    List(ctx context.Context) ([]*Snapshot, error)
}
```

### 10.1 默认实现：从 store.KV 加载

```go
// pkg/domain/loader/loader.go
package loader

type Loader struct {
    Store store.KV
    Cache *lru.Cache[string, *domain.ModelServiceSnapshot] // model name → snapshot
    // Watch /modelservice/* 自动 invalidate
}

func New(s store.KV, cacheSize int) *Loader {
    l := &Loader{Store: s, Cache: lru.New(cacheSize)}
    go l.watch() // Watch + invalidate
    return l
}
```

## 11. middleware.Detector / middleware.Parser

M3 Envelope middleware 的依赖（详见 [02] 第 3.4 节）。

```go
// pkg/domain/default/detector.go (默认实现示例)
package default

type Detector struct {
    PathRules map[string]domain.Protocol // "/v1/chat/completions" → ProtoOpenAI
}
```

默认实现按 URL 路径前缀匹配；body 特征做兜底。可由用户替换为自定义识别（如基于 `User-Agent`）。

## 12. 注入示例（cmd/gateway/main.go 骨架）

```go
package main

import (
    "log"
    "os"

    "github.com/gin-gonic/gin"

    "github.com/zereker-labs/ai-gateway/pkg/middleware/apikey"
    "github.com/zereker-labs/ai-gateway/pkg/middleware/alwayspass"
    "github.com/zereker-labs/ai-gateway/pkg/cache/memory"
    "github.com/zereker-labs/ai-gateway/pkg/store/file"
    "github.com/zereker-labs/ai-gateway/pkg/usage/file"
    "github.com/zereker-labs/ai-gateway/pkg/middleware"
    "github.com/zereker-labs/ai-gateway/pkg/trace/slog"
    "github.com/zereker-labs/ai-gateway/pkg/middleware"

    // 注册 Adapter
    _ "github.com/zereker-labs/ai-gateway/pkg/adapter/openai"
    _ "github.com/zereker-labs/ai-gateway/pkg/adapter/anthropic"
    // ...

    // 注册 TokenExtractor
    _ "github.com/zereker-labs/ai-gateway/pkg/usage/extractor/openai_compat"
    _ "github.com/zereker-labs/ai-gateway/pkg/usage/extractor/anthropic"
)

func main() {
    cfgStore, _ := file.New("/etc/ai-gateway")
    cacheStore := memory.New()
    bus, _ := file.NewEventBus("/var/log/ai-gateway/usage.log")
    tracer := slog.New(slog.Default())

    deps := middleware.Deps{
        Auth:         middleware.AuthDeps{Provider: apikey.MustNewFromFile("/etc/ai-gateway/apikeys.yaml")},
        Envelope:     middleware.EnvelopeDeps{Detector: defaultDetector(), Parser: defaultParser()},
        Budget:       middleware.BudgetDeps{Checker: alwayspass.Checker{}},
        ModelService: middleware.ModelServiceDeps{Loader: loader.New(cfgStore, 1000)},
        Limit:        middleware.LimitDeps{Checker: limit.NewDefaultChecker(cacheStore, cfgStore)},
        Schedule: middleware.ScheduleDeps{
            Scheduler: scheduling.NewDefaultScheduler(cfgStore),
            Executor:  scheduling.NewExecutor(...),
        },
        Moderation: middleware.ModerationDeps{Moderator: nil}, // NoOp
        Tracing:    middleware.TracingDeps{UsageBus: bus, Tracer: tracer},
    }

    if err := deps.Validate(); err != nil {
        log.Fatal(err)
    }

    r := gin.New()
    middleware.Register(r, deps)
    r.POST("/v1/chat/completions", handler)
    r.POST("/v1/messages", handler)
    // ...

    if err := r.Run(":8080"); err != nil {
        log.Fatal(err)
    }
}

// handler 极简：所有工作 middleware 已完成
func handler(c *gin.Context) {
    // 响应已由 M7 Schedule 写出；这里不做任何事
}
```

> **生产部署**只需替换默认实现：
> ```go
> cfgStore, _ := etcd.New(etcdClient, "/ai-gateway/")
> cacheStore := redis.New(redisClient, "ai-gateway:")
> bus, _ := kafka.NewEventBus(kafkaProducer, "usage-events")
> ```
> 业务代码 0 改动。

## 13. 生产部署对照表

| 接口 | 本地 / 单机 | 生产推荐 | 说明 |
|------|-----------|---------|------|
| `middleware.IdentityProvider` | `apikey` (file) | `apikey` (DB) / `jwt` (JWKS) | 自定义实现接入企业 SSO / IAM |
| `middleware.BudgetGate` | `alwayspass` | 自定义对接计费系统 | 若有付费体系 |
| `store.KV` | `file` | `etcd` | 多实例需要强一致 + Watch |
| `cache.Store` | `memory` | `redis` | 多实例共享限流桶 / cooldown |
| `usage.OutboxPublisher` | `file` | `kafka` | 离线计价聚合需要 |
| `middleware.Moderator` | `nil` (NoOp) | `openai` / 自建 | 合规要求时启用 |
| `trace.Tracer` | `slog` | `otel` | OTLP exporter 接 Jaeger / Tempo / vendor |

## 14. 演进规则

- **新增接口**：在 `pkg/<area>/` 加包；至少提供一个默认实现（最好两个：NoOp + minimal）；本文档第 2 节同步包结构
- **修改接口签名**：评估对所有现有实现的影响；考虑向后兼容（新方法可选 / 提供 default 实现）
- **新增默认实现**：在对应包下加子包；本文档第 13 节对照表同步
- **示例 main.go**：保持本文档第 12 节示例与 `cmd/gateway/main.go` 对齐
