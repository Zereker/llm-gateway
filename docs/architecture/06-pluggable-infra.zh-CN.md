[English](06-pluggable-infra.md) | [简体中文](06-pluggable-infra.zh-CN.md)

# 06 — 可插拔基础设施

该文档记录了基础设施的可插拔边界，以及域/中间件/存储库之间的依赖方向。本文档是目标边界——它不描述遗留的兼容性实现；代码应该向本文档收敛。

核心原则：

1. `internal/domain` 定义网关的业务模型，不引用 `internal/repo`，且不得使用 repo 结构体别名。
2. `internal/middleware` 定义了它本身需要的最小依赖接口。
3、`internal/repo`是SQL实现层，可以实现中间件定义的接口。
4. repo接口和实现返回`domain`结构体，而不是将repo模型泄漏到上层。
5. 中间件构建使用选项模式，可以轻松地在单元测试中注入存根/伪造。

`internal/domain` 的存在不应该只是一个导入路径包装器。如果domain通过类型别名指向repo，业务层看起来依赖于domain，但实际上仍然将SQL模式、ORM标签和Scanner/Valuer拖到schedule、translator和upstream等包中。稍后，当替换存储实现、编写单元测试或调整表结构时，您将被存储库类型拖后腿。

## 1. 依赖方向

目标依赖方向：

```text
internal/domain                         pure business structs, no repo import
      ▲
      ├── internal/middleware           defines middleware's minimal interfaces, calls schedule / upstream
      │       └── internal/selector     pure scheduling logic and eligibility, holds no repo
      │
      └── internal/repo                 SQL schema model + SQL implementation, adapts and returns domain types

cmd/gateway                         wires up repo, middleware, schedule and infra drivers
```

禁止方向：

```text
domain -> repo
middleware -> repo concrete model
repo interface -> middleware-only contract leakage
```

这样做的目的：

- 域不受 SQL 标签/gorm 标签/扫描仪/评估器污染。
- 中间件单元测试不需要构建存储库模型或 SQL 结构。
- repo可以自由调整其存储结构，只要适应域输出即可。
- 替换SQL实现时，只需要实现中间件的小接口即可。

## 2. 启动依赖

`cmd/gateway`的目标启动依赖：

|依赖|目的|必填 |
|------|------|----------|
| YAML 配置 |服务器、数据库、redis、中间件、调度器、Outbox、trace等配置|必填 |
| SQL 数据库 |主账户、API 密钥、模型服务、订阅、端点、配额策略 |必填 |
| Redis | M6 速率限制、调度程序冷却 |必填 |
|文件或 Kafka Outbox |使用事件输出|必填，选一项 |
| OTel 采集器 |当跟踪驱动程序为 `otel` 时使用可选|
| OpenAI 审核 API |当审核驱动程序为 `openai` 时使用 |可选|

数据库 Schema 来源是 `internal/infra/migrations/` 下只增不改的文件。Gateway 启动时先运行
`infra.Migrate`，校验已记录的 Schema 版本，再在接收流量前运行
`repo.CheckSchema`。因此启动阶段需要 DDL 权限；Schema 演进方式与当前限制见
[07 §3](./07-configuration.zh-CN.md#3架构迁移)。

定价不会在网关的热路径上进行主动价格查找；价格匹配和金额计算由下游计费平台根据请求发生的时间进行。

## 3. 领域模型

`internal/domain` 应该只包含网关业务层所需的结构体，例如：

- `UserIdentity`
- `Credentials`
- `ModelService`
- `Endpoint`
- `QuotaPolicy` / `QuotaRule`
- `RequestEnvelope`
- `Usage`
- `SchedulingDecision`

域结构的要求：

- 没有 `db` / `gorm` 标签。
- 不执行 SQL `Scanner` / `Valuer`。
- 请勿导入 `internal/repo`。
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

repo 内部可以有像 `repo.EndpointRow` / `repo.EndpointRecord` 这样的表结构，然后将它们转换为 `domain.Endpoint`。

复杂的 JSON 列遵循相同的规则：业务含义包含在域类型中； SQL 编码/解码、`Scanner` / `Valuer` 和数据库默认值适应位于存储库行类型或存储库内部帮助程序中。不要仅仅为了重用 `Scan` / `Value` 方法而将存储库类型重新公开给域。

## 4. 中间件拥有的接口

每个中间件都定义了它自己所需的最小接口。接口位于 `internal/middleware` 或与该中间件相邻的文件中，并返回 `domain` 类型。

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

这些接口不应该作为上层合约存在于 `internal/repo` 中。 `internal/repo`仅提供实现。

## 5. Repo 作为实现层

`internal/repo` 可以包含两种结构：

1. SQL行/记录：携带`db` / `gorm`标签，Scanner / Valuer，接近schema。
2. SQL reader/provider实现：查询数据库，将行转换为`domain`。

推荐的迁移结构：

```text
internal/domain/endpoint.go      the real domain.Endpoint, no db/gorm tags
internal/repo/endpoint_row.go    endpointRow / EndpointRow, carries SQL tags and column encoding/decoding
internal/repo/endpoint_reader.go SQL-queries rows, and returns domain.Endpoint
```

repo 可以使用嵌入来减少重复字段，但不应该让嵌入反过来污染域：

```go
type endpointRow struct {
    domain.Endpoint

    Capabilities endpointCapabilitiesJSON `db:"capabilities"`
    AuthConfig    endpointAuthJSON         `db:"auth_config"`
}
```

如果嵌入使标签、零值或 JSON 列行为不清楚，请改用显式 `ToDomain()` 映射器。优先考虑清晰的边界而不是最少的代码。

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

编译时断言可以在 repo 包中声明：

```go
var _ middleware.EndpointReader = (*SQLEndpointReader)(nil)
```

这表示repo适配中间件，而不是中间件依赖repo。

实施可以逐个实体地迁移。建议从 `Endpoint` 开始，因为它同时涵盖了路由、协议功能、身份验证配置、配额和 JSON 列；一旦该实体正常工作，请将模式复制到 `UserIdentity`、`Credentials`、`ModelService`、 `Secret`、`QuotaPolicy` 等结构。

每个实体迁移的接受标准：

- `internal/domain` 中没有 `type X = repo.X`。
- `internal/domain` 不导入 `internal/repo`。
- 存储库读取器/供应商的公共返回值使用 `domain` 类型。
- 中间件/调度/转换器/上游不接收存储库行类型。
- `go test ./...`通行证。

还应该检查依赖闭包：

```bash
go list -deps ./internal/domain | rg '/internal/repo$'
go list -deps ./internal/selector | rg '/internal/repo$'
go list -deps ./internal/translator | rg '/internal/repo$'
go list -deps ./internal/invoker | rg '/internal/repo$'
```

这些命令的目标是没有输出。 `internal/repo` 本身依赖于 `internal/domain` 是可以的。

<a id="6-middleware-options"></a>
## 6. 中间件选项

中间件构建使用 **interface-Option 模式**，与 `otelgin v0.68.0` 对齐
(`opentelemetry-go-contrib/instrumentation/github.com/gin-gonic/gin/otelgin`):

- `Option` 是一个接口而不是函数类型，以便将来可以轻松地通过非函数实现进行扩展（有状态选项）。
- `optionFunc` 适配器将 `func(*cfg)` 改编为选项，因此调用站点的语法不会更改。
- 中间件构建一次解决所有依赖关系（包括OTel `TracerProvider` /
  `Propagators`)，并且闭合件容纳示踪剂；热路径进行**零**查找。
- 每个中间件都带有`WithXxxTracerProvider(tp oteltrace.TracerProvider) XxxOption`；
  如果未提供，它将回退到 `otel.GetTracerProvider()`，从而可以轻松地在单元测试中注入内存中导出器。

目标结构：

```go
type AuthOption interface {
    apply(*authConfig)
}

type authOptionFunc func(*authConfig)

func (f authOptionFunc) apply(c *authConfig) { f(c) }

type authConfig struct {
    identity       IdentityResolver
    tracerProvider oteltrace.TracerProvider
}

func WithIdentityResolver(r IdentityResolver) AuthOption {
    return authOptionFunc(func(c *authConfig) { c.identity = r })
}

func WithAuthTracerProvider(tp oteltrace.TracerProvider) AuthOption {
    return authOptionFunc(func(c *authConfig) {
        if tp != nil {
            c.tracerProvider = tp
        }
    })
}

func Auth(opts ...AuthOption) gin.HandlerFunc {
    cfg := authConfig{}
    for _, opt := range opts {
        opt.apply(&cfg)
    }
    if cfg.identity == nil {
        panic("middleware.Auth: WithIdentityResolver required")
    }
    if cfg.tracerProvider == nil {
        cfg.tracerProvider = otel.GetTracerProvider()
    }
    tracer := cfg.tracerProvider.Tracer(ScopeName)

    return func(c *gin.Context) {
        ctx, span := tracer.Start(c.Request.Context(), "auth.lookup")
        defer span.End()
        c.Request = c.Request.WithContext(ctx)

        rc := GetRequestContext(c)
        // ... the handler calls dependencies with the local ctx (cfg.dep.Call(ctx, ...)); do not attach ctx onto RC
        _ = rc
    }
}
```

M7 / M10 /其他中间件遵循相同的结构：

```go
type ScheduleOption interface { apply(*scheduleConfig) }

func WithEndpointReader(r EndpointReader) ScheduleOption
func WithScheduler(s selector.Scheduler) ScheduleOption
func WithSender(s *invoker.Sender) ScheduleOption
func WithScheduleTracerProvider(tp oteltrace.TracerProvider) ScheduleOption
```

单元测试注入存根：

```go
r.Use(middleware.Auth(
    middleware.WithIdentityResolver(fakeIdentity{}),
    middleware.WithAuthTracerProvider(testTP), // optional; noop if not provided
))
```

选项模式规则：

- 当缺少所需的依赖项时快速失败（构建时出现恐慌）。
- 为可选依赖项提供明确的默认值，例如moderator nil = 直通，TracerProvider nil = otel 全局。
- 直通快速路径（无审核器/无预算门/无速率限制存储）直接返回
  `func(c) { c.Next() }` 在构建时 - **甚至没有打开跟踪器** - 保存一个
  `Tracer()` 在启动时调用，每个请求一次跨度开始/结束。
- 选项只做装配，不做IO；构造函数不得打开 DB/Redis 连接（资源是
  由 `internal/app/runtime` 管理）。
- 同一中间件的所有 `WithXxx*` 选项必须共享相同的 `XxxOptionFunc` 适配器
  类型；不要为单个选项引入单独的结构选项类型。

M1 `TraceContext`是最完整的参考实现：除了`WithTraceContextTracerProvider`之外，它还提供
`WithTraceContextPropagators` / `WithSpanNameFormatter` / `WithTraceContextSpanStartOptions`,
完全镜像otelgin的`WithPropagators` / `WithSpanNameFormatter` / `WithSpanStartOptions`。

## 7.Redis

Redis 承载两种共享状态：

1. **限速桶**：`ratelimit.RedisStore`实现对用户侧RPM/RPS进行预扣，对TPM进行后扣，并在选择端点后进行配额预留/计费。
2. **Cooldown**：`selector.NewRedisCooldownManager` 记录故障端点的短期隔离状态。
   `CooldownManager`接口是`Mark(ctx, endpointID, class, retryAfter)` / `InCooldown(ctx, ids)` /
   `Clear(ctx, endpointID)` — `Mark` 的 `retryAfter` 携带上游自己的重置感知恢复提示
   TTL (docs/03 §9) 和 `Clear` 支持运行状况探测器的探测门控早期发布 (docs/03 §10)。

没有内存存储作为网关生产回退。在多个副本中，速率限制和冷却时间都必须共享 Redis。

单元测试可以使用假商店/假 CooldownManager，但只能在测试包内使用，不能作为生产驱动程序。

<a id="8-repo-cache-deployer-sql--gateway-data-propagation"></a>
## 8. Repo 缓存：部署器 SQL → 网关数据传播

网关启动应用版本化架构更改；业务行由 SQL 或可选控制台维护。
网关的数据平面对于 MySQL 来说是 **100% 只读** — 不能插入/更新/删除。
两侧之间的传播桥不需要实时失效通道（Debezium /
Outbox表等）——回购层足以依靠进程内的数据
TTL LRU 缓存：

```text
deployer --SQL INSERT/UPDATE--> MySQL
                                  │
                            (request)│
                                    ▼
                  gateway: repo.CachedXxxReader (TTL LRU, default 30s)
                                    │ miss
                                    ▼
                  L3: direct MySQL query (repo.SQLXxxReader.Get*)
```

### 8.1 组件

|层|角色 |实施|
|----|------|------|
| MySQL |真相来源| `internal/infra/migrations/*.sql` |
| `repo.TTLCache[K, V]` |进程内LRU + TTL；不缓存未找到| `internal/repo/cache.go` |
| `repo.CachedXxxReader` | 5 个 SQL 读取器/提供程序的缓存包装器 | `internal/repo/cached.go` |

<a id="82-applicable-tables-and-default-parameters"></a>
### 8.2 适用表和默认参数

|缓存包装|包装的 SQL 阅读器 |缓存键 |默认上限/ttl |
|---|---|---|---|
| `CachedAPIKeyProvider` | `SQLAPIKeyProvider` | `HashAPIKey(plain)` | 10240 / 30 秒 |
| `CachedModelServiceReader` | `SQLModelServiceReader` | `model` | 256 / 30 秒 |
| `CachedEndpointReader` | `SQLEndpointReader` | `"model\x00group"` + `id` | 1024+4096 / 30 秒 |
| `CachedQuotaPolicyProvider` | `SQLQuotaPolicyProvider` | `id` | 128 / 30 秒 |
| `CachedSubscriptionProvider` | `SQLSubscriptionProvider` | `accountID\x00modelServiceID` | 10240 / 30 秒 |

### 8.3 失效语义

**默认情况下 TTL，API 密钥的针对性失效** - 大多数记录依赖于自然 TTL 过期。控制台可以发布尽力而为的缓存总线事件以撤销 API 密钥； TTL 仍然是回退方案。

- 端点、配额和定价更改通常接受有限的 TTL 窗口。
- 当控制台配置了 Redis 时，API 密钥撤销可以通过缓存总线在亚秒内传播。

### 8.4 不缓存“未找到”

当加载器返回 nil/0 时，它**不**设置 - 这可以避免负缓存卡住
“刚刚创建的资源”，让新添加的数据在 TTL 窗口内丢失并消失
返回源（最坏的情况，每个请求一次 L3 行程，并且它命中下一组）。
唯一的例外： `CachedSubscriptionProvider` 缓存 `false` （缺少订阅是一个公共路径，
并且返回源头的成本很高）。

### 8.5 未完成的事情

- **无 L2 Redis 共享缓存**：每个网关副本维护自己的进程内缓存； L1 未命中
  它直接进入 L3 MySQL。简单，并且不存在跨副本一致性问题。
- **无 CDC / binlog 监听**：数据平面是 100% 只读，因此不需要基于推送的
  失效通道； TTL已经足够了。
- **没有陈旧的同时重新验证/刷新提前**：在 TTL 到期时，它会被直接驱逐，下一个
  Get 在未命中时返回到源。如果需要异步刷新，请根据指标做出决定。

## 9.预算门

M4预算可更换：

- `alwayspass`：默认，始终通过。
- `inmemory`：进程内余额跟踪，适用于演示/单实例。

**`inmemory` 绝不能与多个副本一起使用**：余额是每个进程 - N 个副本
每个都独立扣除，有效地授予 N 倍的预算，并且在某个时间重置为零
滚动重启。多副本部署应使用 `alwayspass`（由
下游计费系统）或实现外部计费`BudgetGate`（共享存储）。

添加新的外部记账系统时，实现中间件的`BudgetGate`接口，并通过`cmd/gateway`中的选项注入。

## 10. 策略执行和审核兼容性

M8的主要扩展点是`policy.Engine`，注入
`middleware.WithPolicyEngine`。它显式返回 `allow`、`deny`，或者
`redact`决定；请参阅[策略执行](10-policy-enforcement.zh-CN.md)。

内置 moderation 配置仍然可以替换：

- `none`：默认，跳过审核。
- `openai`：调用OpenAI审核API，需要`moderation.api_key`。

`Moderator` 实现统一通过 `moderation.ModeratorEngine` 转换。
未提供引擎或 moderator 时，M8 直接放行。显式提供的策略引擎优先于
moderator adapter。

## 11. 记录/使用事件

使用事件通过 `usage_events.driver` 选择，目前有两个互斥驱动（通过 `internal/usage.OutboxPublisher` 接口实现）：

- `file`：本地 JSONL 追加；持久化、采集和重放由部署方负责。
- `kafka`：`async: false` 等待 Broker 确认；`async: true` 使用内存 best-effort 重试队列与可选 DLQ。

两种模式都不是数据库事务 Outbox。文件实现没有投递状态或重放 Worker，异步 Kafka
队列也可能在进程故障时丢失缓冲事件。

有关完整配置模式，请参阅 [07-configuration §2 `usage_events`](./07-configuration.zh-CN.md#2-gatewayyaml)，有关故障语义，请参阅 [05-metering-billing §5](./05-metering-billing.zh-CN.md#5-usage-outbox)。

内容日志是一个单独的通道，并且不重用使用事件架构。内容记录器可以通过 `upstream.WithHooks(...)` 连接。

Kafka 的异步缓冲区、最大重试次数、退避和 DLQ 主题在
`usage_events.kafka.*` 配置块中声明。Producer 关闭由 `internal/app/runtime`
集中管理（请参阅 §12 优雅关闭顺序）。

## 12. 追踪

跟踪驱动程序：

- `slog`：默认、结构化日志记录。
- `otel`：初始化OTLP提供程序，并在退出时通过服务器关闭器调用`Shutdown`。

`trace.CtxHandler` 包装了 slog JSON 处理器，以便 `slog.InfoContext` / `ErrorContext` 自动携带trace_id、span_id 和行李。

禁止直接在请求路径上调用无上下文方法，例如 `slog.Info` / `slog.Error` / `slog.Warn` 。实现应添加 lint 或测试扫描，以确保所有日志记录入口点都使用 `slog.*Context`。

**中间件 OTel 集成镜像 otelgin v0.68.0**：所有中间件构建
`tracer := cfg.tracerProvider.Tracer(ScopeName)`一次在施工时，并且关闭保存它。 M1 跟踪上下文
是完整的参考（`WithTraceContextTracerProvider` / `WithTraceContextPropagators` /
`WithSpanNameFormatter` / `WithTraceContextSpanStartOptions`，在§6)中扩展；其他
中间件仅提供`WithXxxTracerProvider`（默认为OTel全局），span名称使用
直接固定动词（`auth.lookup` / `catalog.resolve` / `ratelimit.reserve` / `schedule.pick`
/ `moderation.check` / `tracing.commit`）。

OTel属性命名优先遵循OpenTelemetry `gen_ai.*` / HTTP semconv标准；当
缺少标准字段，请使用 `llm_gateway.*` 前缀。完整属性列表和推荐
跨度结构保持在 [08-可观察性 §4 Tracing](./08-observability.zh-CN.md#4-tracing) 中，而不是
此处重复；指标命名和维度位于 [08-可观察性 §3 Metrics](./08-observability.zh-CN.md#3-metrics)。

## 13. 服务器生命周期

`internal/app/runtime.Runtime` 负责：

- 打开 DB/Redis/Kafka 生产者。
- 注册关闭器。
- 启动 HTTP 服务。
- 捕获 SIGTERM/SIGINT。
- 正常关闭。
- 以相反的顺序关闭资源。

如果`cmd/gateway`的`buildEngine`中途失败，它会推迟`srv.Close()`来清理已经打开的资源。

活跃度/准备度：

- `/healthz` 是 liveness — 它仅表示进程的事件循环仍然可以响应，并且不依赖于 SQL / Redis / Kafka。
- `/readyz` 是就绪状态 — 检查 SQL 和 Redis 是否可达；它不会检查 Kafka/Outbox，因为用量发布失败不应导致网关退出流量。
- 如果就绪状态持续失败超过配置的阈值，则可以使活性也返回失败，以避免 Pod 无限期地保持未就绪状态。

优雅关闭命令：

1. 收到SIGTERM/SIGINT后，HTTP服务器停止接受新请求。
2.等待正在进行的请求完成，以`server.shutdown_timeout`为界，默认30秒。
3. 启用异步 Kafka 时，排空并关闭其 Producer / Publisher。
4. 关闭Redis 客户端。
5. 关闭数据库池。

超过关闭超时的正在进行的请求会被中断，记录为 `llm_gateway_request_aborted_by_shutdown_total`。关闭命令在等待请求完成之前不得关闭 Kafka/Redis/DB，否则 M6 的 post-side、M10 的Outbox和跟踪包将失去依赖关系。

## 14. 管理边界

网关启动拥有架构更改；业务行由部署者 SQL 或控制台维护。下表属于该维护范围：

- 账户
- api_keys
- 配额策略
- 路由策略
- 路由成本配置文件
- 策略定义
- 策略绑定
- 模型服务
- 账户模型订阅
- 端点
- 定价版本

网关对这些表是只读的，但审计类型字段除外，例如上次使用的 API 密钥（如果已实现），应单独调用。

## 15.演进规则

- 禁止在 `internal/domain` 中添加 `internal/repo` 的导入或类型别名。
- 对于一个新的中间件，首先在中间件包中定义最小接口，然后有repo
  实施它；选项使用接口 + optionFunc 结构，与 otelgin v0.68.0 (§6) 一致。
- 回购返回域结构 - 回购行类型不得泄漏给中间件。
- 添加新的基础设施驱动程序时，请在配置、cmd 构建功能、示例中一致地注册它
  配置，以及这个文档。
- 当所需的启动依赖项发生变化时，[00-overview](00-overview.zh-CN.md)的启动流程必须更新。
- 不要在文档中声称“零外部依赖启动”，除非代码实际上提供了
  可运行的 DB/Redis 替代实现。
- 添加新的存储库缓存包装器（第 8.2 节）必须与以下内容同步：定义缓存键、
  默认 cap/ttl，以及本文档的 §8.2 表。

## 16. 已知限制（安全/技术债务 - 在进行更改之前阅读）

- **API 密钥哈希是无盐单轮 SHA-256** (`repo.HashAPIKey`)：如果 api_keys 表泄漏，
  弱密钥可以被暴力破解离线/相同的密钥可以跨账户关联。网关确实
  本身不生成密钥（部署者插入哈希），因此无法保证密钥熵。缓解措施：
  部署器生成≥256位随机密钥。根本原因方向：HMAC-SHA256(pepper, key) — 需要
  双读迁移（同时查询旧哈希和新哈希一段时间），尚未安排。
- **data_key (KEK) 没有旋转路径**：密文带有 `v1:` 前缀，但没有 v2 解密
  链——如果怀疑 KEK 泄露，唯一的选择是停止服务并手动
  解密/重新加密所有端点.auth。根因方向：`SetDataKeys(new, old...)`多键
  解密+后台重新加密，尚未安排。
- **速率限制与 Redis 集群不兼容**：请参阅 [04 §7a](./04-rate-limiting.zh-CN.md#7a-redis-deployment-shape-limitations)。
- **OTel Baggage 不得注入上游请求**：内部租户标识符位于
  行李；上游客户端只允许注入traceparent（参见注释
  `internal/trace/otel.go`）。
