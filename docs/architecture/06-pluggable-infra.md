# 06 — Pluggable Infrastructure

本文记录基础设施可插拔边界，以及 domain / middleware / repo 的依赖方向。本文是目标边界，不描述兼容旧实现；代码落地时应按本文档收敛。

核心原则：

1. `pkg/domain` 定义网关业务模型，不引用 `pkg/repo`，不能使用 repo 结构别名。
2. `pkg/middleware` 定义自己需要的最小依赖接口。
3. `pkg/repo` 是 SQL 实现层，可以实现 middleware 定义的接口。
4. repo 接口和实现返回 `domain` 结构，而不是把 repo model 泄漏给上层。
5. middleware 构建使用 option pattern，便于单测注入 stub / fake。

`pkg/domain` 的存在不应只是 import 路径包装。如果 domain 通过 type alias 指向 repo，业务层看似依赖 domain，实际仍把 SQL schema、ORM tag、Scanner / Valuer 带进 schedule、translator、upstream 等包。后续替换存储实现、做单元测试或调整表结构时，都会被 repo 类型反向牵住。

## 1. 依赖方向

目标依赖方向：

```text
pkg/domain                         纯业务结构，无 repo import
      ▲
      ├── pkg/middleware           定义 middleware 最小接口，调用 schedule / upstream
      │       └── pkg/schedule     调度纯逻辑和 eligibility，不持有 repo
      │
      └── pkg/repo                 SQL schema model + SQL 实现，适配并返回 domain 类型

cmd/gateway                         装配 repo、middleware、schedule 和 infra driver
```

禁止方向：

```text
domain -> repo
middleware -> repo concrete model
repo interface -> middleware-only contract 泄漏
```

这样做的目的：

- domain 不被 SQL tag / gorm tag / Scanner / Valuer 污染。
- middleware 单测不需要构造 repo model 或 SQL 结构。
- repo 可以自由调整存储结构，只要适配成 domain 输出。
- 替换 SQL 实现时，只要实现 middleware 的小接口即可。

## 2. 启动依赖

`cmd/gateway` 目标启动依赖：

| 依赖 | 用途 | 是否必需 |
|------|------|----------|
| YAML config | server、database、redis、middleware、scheduler、outbox、trace 等配置 | 必需 |
| SQL DB | 主账号、API key、model service、subscription、endpoint、quota policy | 必需 |
| Redis | M6 rate limit、scheduler cooldown | 必需 |
| file 或 Kafka outbox | usage 事件输出 | 必需选择一种 |
| OTel collector | trace driver 为 `otel` 时使用 | 可选 |
| OpenAI moderation API | moderation driver 为 `openai` 时使用 | 可选 |

DB schema 真相在 `pkg/infra/schema.sql`。gateway 只 `repo.CheckSchema`，不 AutoMigrate，不创建表。

Pricing 不在 gateway 热路径做 active price 查询；价格匹配和金额计算由下游计费平台按请求发生时间完成。

## 3. Domain 模型

`pkg/domain` 应只包含网关业务层需要的结构，例如：

- `UserIdentity`
- `Credentials`
- `ModelService`
- `Endpoint`
- `QuotaPolicy` / `QuotaRule`
- `RequestEnvelope`
- `Usage`
- `SchedulingDecision`

domain 结构要求：

- 不包含 `db` / `gorm` tag。
- 不实现 SQL `Scanner` / `Valuer`。
- 不 import `pkg/repo`。
- 字段表达业务语义，而不是表结构。

反例：

```go
type Endpoint = repo.Endpoint
```

目标：

```go
package domain

type Endpoint struct {
    ID           int64
    Name         string
    Vendor       string
    Model        string
    Group        string
    Weight       uint32
    Enabled      bool
    NativeProtocol Protocol
    Modalities    []Modality

    Auth         EndpointAuth
    Routing      EndpointRouting
    Quota        EndpointQuota
    Capabilities EndpointCapabilities
}
```

repo 内部可以有 `repo.EndpointRow` / `repo.EndpointRecord` 表结构，再转换成 `domain.Endpoint`。

复杂 JSON 列也遵循同一规则：业务含义放在 domain 类型里；SQL 编解码、`Scanner` / `Valuer`、数据库默认值适配放在 repo row 类型或 repo 内部 helper 中。不要为了复用 `Scan` / `Value` 方法把 repo 类型重新暴露给 domain。

## 4. Middleware-owned Interfaces

每个 middleware 定义自己需要的最小接口。接口放在 `pkg/middleware` 或该 middleware 的近邻文件中，返回 `domain` 类型。

示例：

```go
// M2 Auth
type IdentityResolver interface {
    Resolve(ctx context.Context, creds *domain.Credentials) (*domain.UserIdentity, error)
}

// M5 ModelService
type ModelCatalog interface {
    GetByModel(ctx context.Context, model string) (*domain.ModelService, error)
}

type SubscriptionChecker interface {
    HasModel(ctx context.Context, accountID string, modelServiceID int64) (bool, error)
}

// M6 RateLimit
type QuotaPolicyReader interface {
    GetQuotaPolicy(ctx context.Context, id int64) (*domain.QuotaPolicy, error)
}

// M7 Schedule
type EndpointReader interface {
    ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
}
```

这些接口不应该放在 `pkg/repo` 作为上层契约。`pkg/repo` 只提供实现。

## 5. Repo 作为实现层

`pkg/repo` 可以包含两类结构：

1. SQL row / record：带 `db` / `gorm` tag、Scanner / Valuer，贴近 schema。
2. SQL reader/provider 实现：查询数据库，把 row 转成 `domain`。

推荐迁移形态：

```text
pkg/domain/endpoint.go      真实 domain.Endpoint，无 db/gorm tag
pkg/repo/endpoint_row.go    endpointRow / EndpointRow，承载 SQL tag 与列编解码
pkg/repo/endpoint_reader.go SQL 查询 row，并返回 domain.Endpoint
```

repo 可以用嵌入减少重复字段，但不要让嵌入反过来污染 domain：

```go
type endpointRow struct {
    domain.Endpoint

    Capabilities endpointCapabilitiesJSON `db:"capabilities"`
    AuthConfig    endpointAuthJSON         `db:"auth_config"`
}
```

如果嵌入导致 tag、零值或 JSON 列行为不清晰，就使用显式 `ToDomain()` mapper。优先保证边界清楚，而不是追求最少代码。

示例：

```go
package repo

type EndpointRow struct {
    ID      int64  `db:"id"`
    Vendor  string `db:"vendor"`
    Routing JSONRouting `db:"routing"`
    // ...
}

func (r *EndpointRow) ToDomain() *domain.Endpoint {
    return &domain.Endpoint{
        ID:     r.ID,
        Vendor: r.Vendor,
        // ...
    }
}

type SQLEndpointReader struct {
    db *sqlx.DB
}

func (r *SQLEndpointReader) ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error) {
    // query rows
    // map rows -> domain.Endpoint
}
```

编译期断言可以在 repo 包里声明：

```go
var _ middleware.EndpointReader = (*SQLEndpointReader)(nil)
```

这表示 repo 适配 middleware，而不是 middleware 依赖 repo。

实现时可以按实体逐步迁移。建议先选 `Endpoint`，因为它同时覆盖路由、协议能力、认证配置、quota 和 JSON 列；这个实体跑通后，再复制到 `UserIdentity`、`Credentials`、`ModelService`、`Secret`、`QuotaPolicy` 等结构。

每个实体迁移的验收点：

- `pkg/domain` 中没有 `type X = repo.X`。
- `pkg/domain` 不 import `pkg/repo`。
- repo reader/provider 的 public 返回值使用 `domain` 类型。
- middleware / schedule / translator / upstream 不接收 repo row 类型。
- `go test ./...` 通过。

依赖闭包也应纳入检查：

```bash
go list -deps ./pkg/domain | rg '/pkg/repo$'
go list -deps ./pkg/schedule | rg '/pkg/repo$'
go list -deps ./pkg/translator | rg '/pkg/repo$'
go list -deps ./pkg/upstream | rg '/pkg/repo$'
```

这些命令目标是无输出。`pkg/repo` 自己依赖 `pkg/domain` 是允许的。

## 6. Middleware Options

middleware 构建应使用 option pattern，替代越来越大的 `Deps` struct。这样单测可以只替换关心的依赖。

目标形态：

```go
type AuthOption func(*authConfig)

type authConfig struct {
    identity IdentityResolver
}

func WithIdentityResolver(r IdentityResolver) AuthOption {
    return func(c *authConfig) { c.identity = r }
}

func Auth(opts ...AuthOption) gin.HandlerFunc {
    cfg := authConfig{}
    for _, opt := range opts {
        opt(&cfg)
    }
    if cfg.identity == nil {
        panic("middleware.Auth: IdentityResolver required")
    }
    // return handler
}
```

M7 也应类似：

```go
type ScheduleOption func(*scheduleConfig)

func WithEndpointReader(r EndpointReader) ScheduleOption
func WithScheduler(s schedule.Scheduler) ScheduleOption
func WithSender(s *upstream.Sender) ScheduleOption
```

单测可以这样写：

```go
r.Use(middleware.Auth(
    middleware.WithIdentityResolver(fakeIdentity{}),
))
```

Option pattern 规则：

- 必需依赖缺失时 fail fast。
- 可选依赖给明确默认值，例如 moderator nil = pass-through。
- option 只做装配，不做 IO。
- 构造函数不要打开 DB / Redis；资源由 `cmd/gateway` 或 `pkg/server` 管理。

## 7. Redis

Redis 承担两类共享状态：

1. **Rate limit buckets**：`ratelimit.RedisStore` 实现用户侧 RPM/RPS 前扣、TPM 后扣，以及 endpoint 选中后的 quota reserve / charge。
2. **Cooldown**：`schedule.NewRedisCooldownManager` 记录失败 endpoint 的短期隔离状态。

没有内存 Store 作为 gateway 生产兜底。多副本下限流和 cooldown 必须共享。

单测可以使用 fake store，但 fake store 只在测试包内使用，不作为生产 driver。

## 8. BudgetGate

M4 Budget 可替换：

- `alwayspass`：默认，永远通过。
- `inmemory`：进程内余额跟踪，适合 demo/单实例。

新增外部账务系统时，实现 middleware 侧的 `BudgetGate` 接口，并在 `cmd/gateway` 中用 option 注入。

## 9. Moderation

M8 Moderation 可替换：

- `none`：默认，跳过审核。
- `openai`：调用 OpenAI moderation API，需要 `moderation.api_key`。

返回 nil moderator 时 M8 pass-through。

## 10. Recording / Outbox

Usage outbox 由 `outbox.driver` 选择，三个互斥 driver：

- `file`：本地 JSONL append；仅适合本地开发或临时排障。
- `kafka`：同步 Kafka producer；发布完成才返回，延迟较高，无内存 buffer。
- `async_kafka`：异步 buffer + 重试 + backoff + DLQ topic；生产推荐。

完整配置 schema 见 [07-configuration §2 `outbox`](./07-configuration.md#2-gatewayyaml)，故障语义见 [05-metering-billing §5](./05-metering-billing.md#5-usage-outbox)。

Content Log 是独立通道，不复用 Usage Event schema。内容记录器可通过 `upstream.WithHooks(...)` 装配。

`async_kafka` 的 buffer、max retries、backoff、DLQ topic 在 `outbox.kafka.*` 配置块声明。producer 关闭由 `pkg/server` 统一管理（见 §12 graceful shutdown 顺序）。

## 11. Tracing

trace driver：

- `slog`：默认，结构化日志。
- `otel`：初始化 OTLP provider，退出时通过 server closer 调用 `Shutdown`。

`trace.CtxHandler` 包装 slog JSON handler，让 `slog.InfoContext` / `ErrorContext` 自动带上 trace_id、span_id 和 baggage。

请求路径禁止直接调用 `slog.Info` / `slog.Error` / `slog.Warn` 等不带 context 的方法。实现时应增加 lint 或测试扫描，确保日志入口都使用 `slog.*Context`。

OTel attribute 命名优先采用 OpenTelemetry `gen_ai.*` 语义约定；缺少标准字段时使用 `llm_gateway.*` 前缀。完整 attribute 清单与建议 span 结构由 [08-observability §4 Tracing](./08-observability.md#4-tracing) 维护，本文不再重复；指标命名与维度见 [08-observability §3 Metrics](./08-observability.md#3-metrics)。

## 12. Server 生命周期

`pkg/server.Server` 负责：

- 打开 DB / Redis / Kafka producer。
- 注册 closer。
- Serve。
- 捕获 SIGTERM/SIGINT。
- graceful shutdown。
- 倒序 close 资源。

`cmd/gateway` 的 `buildEngine` 如果中途失败，会 defer `srv.Close()` 清理已打开资源。

Liveness / readiness：

- `/healthz` 是 liveness，只表示进程事件循环仍可响应，不依赖 SQL / Redis / Kafka。
- `/readyz` 是 readiness，检查 SQL 和 Redis 可达；不检查 Kafka / outbox，因为 usage 发布失败不应导致网关被摘流量。
- readiness 持续失败超过配置阈值后，可以让 liveness 返回失败，避免 pod 长期 not-ready 卡死。

Graceful shutdown 顺序：

1. 收到 SIGTERM/SIGINT 后，HTTP server 停止接受新请求。
2. 等待 in-flight 请求完成，受 `server.shutdown_timeout` 控制，默认 30s。
3. flush 并关闭 `async_kafka` producer / outbox。
4. 关闭 Redis client。
5. 关闭 DB pool。

超过 shutdown timeout 的 in-flight 请求会被中断，并记录 `llm_gateway_request_aborted_by_shutdown_total`。关闭顺序不能先关 Kafka/Redis/DB 再等待请求，否则 M6 post-side、M10 outbox 和 tracing 收尾会失去依赖。

## 13. Admin 边界

gateway 不拥有 schema 和配置写入。以下资源由 admin 管理：

- accounts
- api_keys
- quota_policies
- model_services
- account_model_subscriptions
- endpoints
- pricing_versions

gateway 对这些表只读，除了 API key last used 等审计类字段如有实现可单独说明。

## 14. 演进规则

- 禁止在 `pkg/domain` 新增对 `pkg/repo` 的 import 或 type alias。
- 新 middleware 先在 middleware 包定义最小接口，再让 repo 实现。
- repo 返回 domain 结构，不能把 repo row 类型泄漏给 middleware。
- 新增 infra driver 时，在 config、cmd build 函数、示例配置和本文档同步登记。
- 启动必需依赖变化时，必须更新 [00-overview](00-overview.md) 的启动流程。
- 不要在文档中宣称“零外部依赖启动”，除非代码重新提供可运行的 DB/Redis 替代实现。
