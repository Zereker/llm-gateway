[English](03a-schedule-overview.md) | [简体中文](03a-schedule-overview.zh-CN.md)

# 03a — 调度安排快速参考/入职伴侣

这是 [03-endpoint-scheduling.zh-CN.md](./03-endpoint-scheduling.zh-CN.md) 的初学者视角：第一次演练
整个进度图（数据流、每个包的职责、关键数据结构、装配点），
然后回到03了解每条规则背后的设计原理。

> 03 解释了**为什么**，本文档解释了**什么/在哪里**。对于主路径代码的更改，03 仍然有效。

## 0. TL;DR

调度执行时序归**`dispatch.Dispatcher`**所有； `middleware/schedule.go` 只是一个
瘦适配器 - 它将 gin / RC 映射到 `dispatch.Input`，运行 `Dispatch()`，然后映射
`dispatch.Outcome` 回到 RC + HTTP。下面是实际的执行流程：

```text
middleware/schedule.go (M7 thin adapter):
  envelope/identity/modelChain/handlers → dispatch.Input
  outcome := dispatcher.Dispatch(ctx, w, input)
  outcome → rc.{RoutedModelService, Usage, Error, SchedulingDecision} + HTTP

dispatch.Dispatcher.Dispatch (internal/dispatch/dispatcher.go):
  state := newState(input, AttemptCap.Resolve(input))
  for {
      action := step(ctx, w, state)
      switch action {
      case Continue: pick another one on the same model
      case Switch:   switch to the next model (triggered by FallbackPolicy)
      case Stream:   stream already written, return Outcome
      case Abort:    terminate, return Outcome
      }
  }

dispatch.Dispatcher.step (a single attempt):
  if Exhausted → Abort(NoEndpoint, 503)
  candidates := CandidateSource.ListForModel(model, group)
  eligible   := filterEligible(candidates, env, handlers)   # internal/dispatch/eligibility.go
  if no eligible → FallbackPolicy.OnExhausted(state)
  ep := Selector.Pick(eligible, query)                       # internal/selector → adapters.SelectorAdapter
  if denied := EndpointQuota.Reserve(ep) → Verdict + RetryPolicy.Decide
  handler := state.Handlers().Get(ep, srcProto)              # protocol.Lookup
  res := InvokerFactory.For(ep, handler, env).Invoke(ctx)    # internal/invoker → adapters.InvokerFactoryAdapter
  Selector.Report(ep, verdict)
  switch RetryPolicy.Decide(verdict):
  case Stream:   res.StreamTo(w) + EndpointQuota.ChargeUsage  # deducted after TPM
  case Continue / Switch / Abort: return action
```

三层职责划分：

|谁 |负责|
|----|------|
| `internal/middleware/schedule.go` | RC ↔ 调度.输入/结果映射；内容日志丰富；指标 `scheduling_duration_seconds`； **不做出调度决定** |
| `dispatch.Dispatcher` (`internal/dispatch`) |调度执行时序的**唯一**拥有者：候选项→资格筛选→选择→预扣→调用→报告→重试/回退→扣后|
| `internal/selector` |对一批候选者运行过滤器链 → 评分器 → 选择器，输出 1 个端点。 **无状态**，不知道协议/处理器/存储库/回退|
| `internal/invoker` |获取一个Handler并运行`PrepareCall + HTTP Do + response forward + error classification`（不进行协议查找——dispatch已经通过`protocol.Lookup`获得了Handler）|
| `internal/protocol` |门面：Handler = Factory + Translator + Quirks；消费者只能看到Handler / Lookup |
| `internal/ratelimit` |存储桶/存储原语； `dispatch/adapters.EndpointQuotaAdapter` 将其连接到 `dispatch.EndpointQuota` |
| `internal/dispatch/adapters/` |将上面的4个原语包桥接到dispatch的4个端口（CandidateSource / Selector / InvokerFactory / EndpointQuota），将组合逻辑与原语解耦 |

`internal/selector` 没有存储库，并且不知道回退模型的存在。跨模型回退是
业务语义，停留在`dispatch.Dispatcher`的外循环（FallbackPolicy触发Switch动作）。

## 1. 包/文件职责概述

```
internal/middleware/schedule.go     M7 thin adapter (gin.HandlerFunc Schedule()):
                               RC → dispatch.Input → dispatcher.Dispatch → RC

internal/dispatch/                  owner of scheduling execution timing
    dispatcher.go              Dispatcher.Dispatch / step main loop
    eligibility.go             pure function filterEligible: takes candidates + envelope + protocol.Lookup
                               outputs eligible endpoints; semantics in §2
    state.go                   per-request state; finalize always produces a SchedulingDecision
    action.go / verdict.go     Continue / Switch / Stream / Abort + Verdict types
    ports.go                   4 port interfaces (CandidateSource / Selector / InvokerFactory / EndpointQuota)
    policy.go / fallback_chain.go / retry_default.go
                               default implementations of AttemptCap / RetryPolicy / FallbackPolicy
    cap_header.go              X-Gateway-Max-Attempts header parsing
    adapters/                  bridges primitive packages into dispatch ports
        selector.go            selector.Scheduler → dispatch.Selector
        invoker.go             invoker.Sender   → dispatch.InvokerFactory
        quota.go               ratelimit.Store  → dispatch.EndpointQuota

internal/selector/                  selection primitives, **unaware** of protocol / handler / repo
    types.go                   Candidate / Request / Result / ErrorClass / Scheduler interfaces
    scheduler.go               defaultScheduler: Pick (filter→scorer→picker) + Report
    filter.go                  Filter interface + runChain
    cooldown.go                CooldownManager + RedisCooldownManager + CooldownFilter
    limit_filter.go            LimitReadFilter (SnapshotBatch, read-only)
    busy.go / prefix_cache.go  self-hosted optimization filters
    weighted.go                Picker interface + WeightedRandomPicker
    scorer.go                  Scorer + EndpointStatsStore + DefaultScorer

internal/invoker/                   HTTP invocation + forward stream, does not do protocol lookup
internal/ratelimit/                 Store / Bucket / endpoint bucket helpers
internal/protocol/                  Handler facade + Factory/Session + quirks

internal/app/gateway/          composition root
    app.go                     wires primitives together into dispatch.Dispatcher
    dispatch.go                buildDispatcher: dispatch.New(WithCandidates / WithSelector /
                               WithInvokerFactory / WithQuota / WithCap / WithRetry /
                               WithFallback / WithTracer)
```

## 2. 三个过滤层的语义边界（**调度中最容易混淆的点**）

|层|谁 |语义 |失败的后果|
|----|----|------|----------|
| **资格** | `internal/dispatch/eligibility.go`（调度的内部助手，不是独立的包）|它能处理这个问题吗：protocol.Lookup 找不到不受支持的处理器/模式 |排除，**不进入冷却，不计为上游失败** |
| **硬过滤器** | `internal/selector.Filter`（冷却/ limit_read / busy / prefix_cache）|是否应该立即选择：正在冷却/配额耗尽/太忙/前缀亲和力|没有选择这个Pick；不直接终止请求|
| **软评分** | `internal/selector.Scorer` |谁是首选：根据成功/延迟/成本调整 `EffectiveWeight` |只调整权重，**不淘汰**候选项 |
| **选择器** | `internal/selector.Selector`（默认加权随机）|从 `EffectiveWeight` 过滤的候选者中选择 1 |全零→零→内断|

**核心原则**（03 §3）：能力问题（缺少供应商工厂/转换器/ep.Protocol 未知）
必须在资格阶段排除 - 它们绝不能达到 `Scheduler.Report`，否则
“不受支持”被错误地标记为“坏EP”，触发冷却并污染后续选择。

## 3. 关键数据结构（types.go）

```go
type Candidate struct {
    Endpoint        *domain.Endpoint
    EffectiveWeight float64           // = ep.Weight when static; adjusted when Scorer is enabled
}

type Request struct {
    Model      string                  // the model for the current round (primary or a fallback)
    Group      string                  // rc.Identity.Group
    Candidates []Candidate             // candidates after eligibility
    ExcludeIDs map[int64]struct{}      // eps already tried in this request
    PrefixKey  []byte                  // used only by PrefixCacheFilter
}

type Result struct {
    Class    ErrorClass                // determines cooldown TTL + whether to retry
    HTTPCode int
    Reason   string
    Latency  time.Duration
}

type Scheduler interface {
    Pick(ctx, *Request) (*domain.Endpoint, error)
    Report(ctx, *domain.Endpoint, Result)
}
```

**请求故意不携带** `attempts` / `fallbackModels` / `LoadFallback` - 这些都是
Dispatcher的外层reducer的职责；调度程序只查看一批候选者。

## 4.ErrorClass六向分类快速参考

|班级 |触发场​​景|可重试 |冷却时间|
|-------|----------|-------------|----------|
| `success` | HTTP 2xx + 协议层成功 |假 |无冷却时间|
| `transient` | 5xx / 网络 / 超时 / DNS |真实 |每个配置的 TTL |
| `capacity` |上游 429 / 超载 / 超出本地储备 |真实 |每个配置的 TTL |
| `permanent` |上游 401 / 403 / 配置错误 | true（切换 ep）|每个配置的 TTL |
| `invalid` |客户端 4xx（401/403/429 除外）/转换器转换失败 | **假** |无冷却时间|
| `unknown` |无法分类 |真实 | **无冷却时间**（以防止分类错误被放大为全面冷却时间）|

特别注意两点：

- 当 `invalid` 被击中时，`dispatch.DefaultRetry.Decide` 直接返回 `Abort{HTTPCode: 400}`，
  外部减速器接收到它后， `state.SetAbort` + 退出循环 - 它**都不**切换 ep
  **nor** 切换回退模型（`PrepareError{Phase:PhaseTranslate|PhaseQuirks}` 也采用此路径）。
- `unknown` 是可重试的，但 `Scheduler.Report` 特殊情况**不写冷却时间**
  （避免分类盲点污染冷却时间）。

## 5.重试模型（两层，互补）

```
Inner layer (same model, switch endpoint):
  failure + retryable → excluded[ep.ID] = struct{}{} → continue Pick
  attempts count toward totalAttempts, bounded by attemptsCap
  (attemptsCap = min(cfg.MaxAttempts, X-Gateway-Max-Attempts), default 3)

Outer layer (cross-model fallback):
  switches only when the request carries X-Gateway-Fallback-Models
  cap MaxFallbackModels = 3 (dedup, order preserved)
  each fallback model must go through M5 again (catalog + subscription)
  totalAttempts accumulates across all models, never resets
```

**关键点**：不再进行同端点重试（L1 重试）——网络抖动现在被吸收
“相同型号，切换EP”。如果将来再次需要，必须将其作为显式配置添加回来，
未在调度内隐式打开。

## 6. 冷却流程

```
Scheduler.Report(ep, result) [scheduler.go:107]
  ├─ Stats.Record(ep.ID, result)          # writes to EndpointStatsStore (Scorer input)
  └─ if result.Class.IsRetryable()
        && result.Class != ClassUnknown   # unknown does not cool down
        && Cooldown != nil
        → Cooldown.Mark(ep.ID, class)     # Redis SET cd:endpoint:<id> <class> EX <ttl>
                                           # best-effort: errors only logged, never block
```

**从Redis的角度来看**：

```
key:   cd:endpoint:<id>
value: ErrorClass string (for diagnostics)
TTL:   configured per class (CooldownDurations.Get)
```

后来的马克**直接覆盖** TTL - 持续失败=持续冷却（如预期）。

CooldownFilter 通过 MGET 进行批量查询； **Redis 错误时失败打开**（保留所有候选），以避免
Redis抖动变成503风暴。

## 7. 端点配额（与 M6 用户端配额严格分层）

|时间 |运营|桶钥匙|
|------|------|------------|
|资格后/Pick过滤器链内| `LimitReadFilter` 使用 `SnapshotBatch` **只读** 来排除已经耗尽的 | `rl:endpoint:<id>:rpm`, `...:rps` |
|在 Pick 选择 ep 之后/在 Invoke | 之前`dispatch.Dispatcher.step` 调用 `EndpointQuota.Reserve(ctx, ep)` （`adapters/EndpointQuotaAdapter` 包装 `ratelimit.Store.ReserveBatch` 用于 RPM/RPS预扣除）；超限 → `QuotaVerdict` 变为 `Verdict{Class: ClassCapacity}` → `RetryPolicy.Decide` 一般返回 `Continue` 进行切换EP |同上 |
| StreamTo 完成后（响应已完成）|调度程序获取 `StreamReport.Usage` 后，它调用 `EndpointQuota.ChargeUsage(ctx, ep, usage)` （`adapters/EndpointQuotaAdapter` 包装） `ratelimit.Store.ChargeBatch`、`cost = usage.Total`、即发即忘）| `rl:endpoint:<id>:tpm` |

**为什么在过滤器阶段不进行预推导**：过滤器的输入是“候选集”——如果保留
发生在那里，甚至没有被选中的候选项也会被扣除他们的配额，显着
放大错误。因此过滤器**只能执行只读的 SnapshotBatch**；
预订仅在调度员挑选后进行。

**必须事后扣除TPM**：因为在请求时`usage.Total`是未知的（必须等待
流完成）。调度程序只有在获取到 `StreamReport.Usage` 后才知道真实的 token 用量，
然后它才会调用 `ChargeUsage`。当 TPM 超过限制时，仅发出一个指标；事实并非如此
阻止**此**响应（它已经被流出）；下一个请求将被阻止
通过 `LimitReadFilter`。

## 8.运行时评分（可选层）

默认关闭（`cfg.Scoring.Enabled = false`）；一旦启用，管道将变为：

```
filter chain → Scorer.Score(candidates) → Selector.Select
                     ↑
       EndpointStatsStore.Snapshot(ep.ID)
                     ↑
       Scheduler.Report → Stats.Record (writes every time)
```

`DefaultScorer` 公式：

```
effective_weight = base_weight * success_factor * latency_factor
success_factor    = clamp(stats.SuccessRate,                [0.1, 2.0])
latency_factor    = clamp(latencyBaselineMs / stats.LatencyMs, [0.1, 2.0])
SampleCount < minSamples (default 5) → neutral factor 1.0 (preserve exploration)
```

`InMemoryStatsStore`使用EMA（默认衰减=0.2）；在多副本部署下每个实例
独立累积 - 如果需要跨副本一致性，请将存储交换为 Redis 支持的存储
实施；界面保持不变。

## 9. 标题快速参考

|标题 |意义|解析规则|
|--------|------|----------|
| `X-Gateway-Fallback-Models` |跨模型回退列表（逗号分隔）|重复数据删除、顺序保留、空忽略、截断超出 `MaxFallbackModels=3` |
| `X-Gateway-Max-Attempts` |客户要求更严格的尝试上限|仅当 < cfg 默认值时生效，**不能**加宽它 |

**客户端只能把默认设置的更严格**——这是一个配置原则，防止恶意
请求炸毁网关的尝试。

## 10. SchedulingDecision 的写入位置

```go
rc.SchedulingDecision = &domain.SchedulingDecision{
    Model:       rc.ModelService.Model,   // the original requested model
    RoutedModel: routedModelOf(rc),       // the model actually hit (including fallback)
    UserGroup:   rc.Identity.Group,
    Attempts:    []domain.Attempt{...},    // one entry per Pick + Send
    DurationMs:  ...,
}
```

每个`Attempt`：

```go
type Attempt struct {
    Index       int        // 1, 2, 3 ... accumulates across models
    Model       string     // the model used for this attempt
    EndpointID  string
    AttemptRole string     // "primary" | "fallback"
    LatencyMs   int64
    ErrorClass  string
    Outcome     string     // success | fallback | fail
}
```

结果导出了三种状态： success = `success`；中间故障= `fallback`；
最终失败= `fail`。

## 11. 指标写入位置

|指标|标签|写位置 |
|--------|------|---------|
| `scheduling_duration_seconds` |模型，尝试|在 M7 薄型适配器的延迟结束时 |
| `invoker_attempts_total` |模型、routed_model、供应商、endpoint_id、attempt_role、结果、error_class |每次 Invoker.Invoke （调度适配器）之后 |
| `rate_limit_decisions_total` |范围=“端点”，维度，结果=“违反”|当 EndpointQuota.Reserve 超过限制时 |
| `rate_limit_charge_total` |维度=“tpm”，结果|在 EndpointQuota.ChargeUsage |
| `tpm_overflow_total` |层=“端点”，维度=“tpm”|当端点TPM扣除后溢出时
| `rate_limit_fail_open_total` |范围=“端点”，维度=“任意”|当 LimitReadFilter 因 Redis 错误而无法打开时 |
| `llm_gateway_repo_cache_total` |表格，结果 | repo TTL LRU 缓存命中/未命中/错误 |

Dispatch 还具有内部 OTel 范围 (`dispatch.request` / `dispatch.attempt`)，其属性包括
模型/端点.id/供应商/判决。{阶段，类，http_code，原因}/dispatch.outcome
/dispatch.routed_model /dispatch.attempts — 有关详细信息，请参阅 [08 §4](./08-observability.zh-CN.md#4-tracing)。

完整的指标合约位于 [08-observability.zh-CN.md §3](./08-observability.zh-CN.md#3-metrics)。

## 12.装配点数(`internal/app/gateway/app.go` + `dispatch.go`)

实际的连接发生在两层：首先组装选择器/调用器/速率限制的原语
分别输入 `buildDispatcher` 组成 `dispatch.Dispatcher`，最后
将调度程序（**不是**选择器/发送器）注入到 M7 中间件中。

```go
// === main.go prepares primitives ===

// 1. Cooldown manager
cooldown := selector.NewRedisCooldownManager(rdb, selector.CooldownDurations{...})

// 2. Filter chain (in the order of cfg.Selector.Filters)
filters := buildSchedulerFilters(cfg.Selector.Filters, rateStore, cooldown)

// 3. Scorer + Stats (optional)
stats, scorer := buildScoring(cfg.Scoring)

// 4. Scheduler primitives (selector.Scheduler interface; pure in-batch Pick + Report)
sched := selector.New(selector.Config{
    Filters: filters, Picker: selector.NewWeightedRandomPicker(),
    Cooldown: cooldown, Scorer: scorer, Stats: stats,
})

// 5. Sender primitives (invoker.Sender; pure HTTP Do + forward)
sender := invoker.New(senderOpts...)

// === dispatch_wiring.go composes the primitives into a Dispatcher ===

dispatcher := buildDispatcher(
    adaptEndpoints(endpointReader),  // CandidateSource bridge for repo.EndpointReader
    sched,                            // Selector bridge (internal/dispatch/adapters/SelectorAdapter)
    sender,                           // InvokerFactory bridge (adapters/InvokerFactoryAdapter)
    rateStore,                        // EndpointQuota bridge (adapters/EndpointQuotaAdapter; ratelimit.Store)
    cfg.Selector.MaxAttempts,         // AttemptCap.Default
    dispatchTracer,                   // OTel tracer, spans dispatch.request / dispatch.attempt
)

// inside buildDispatcher:
//   dispatch.New(
//       dispatch.WithCandidates(candidates),
//       dispatch.WithSelector(adapters.NewSelector(sched)),
//       dispatch.WithInvokerFactory(adapters.NewInvokerFactory(sender)),
//       dispatch.WithQuota(adapters.NewEndpointQuota(rateStore)),
//       dispatch.WithCap(dispatch.HeaderAttemptCap{Default: maxAttempts}),
//       dispatch.WithRetry(dispatch.DefaultRetry{}),
//       dispatch.WithFallback(dispatch.ModelChainFallback{}),
//       dispatch.WithTracer(tracer),
//   )

// === injected into router.Deps; M7 middleware only sees the Dispatcher, not selector / sender ===
Dispatcher: dispatcher,
```

`buildSchedulerFilters` 将字符串名称从 yaml 映射到 Filter 实例：

|名称 |实施|
|------|------|
| `cooldown` | `NewCooldownFilter(cd)` |
| `limit_read` | `NewLimitReadFilter(rateStore)` |
| `prefix_cache` | `NewPrefixCacheFilter(0)` (vnodes=64) |
| `busy` | `NewBusyFilter(0)`（阈值=0.85）|
| `weighted_random` |忽略（它已经是选择器，单独配置）|
|还有什么吗| **恐慌**（快速失败以暴露配置错误）|

## 13. 配置 YAML 快速参考

```yaml
selector:
  max_attempts: 3
  filters:                  # order-sensitive
    - cooldown              # cheapest filter, put it first
    - limit_read            # endpoint quota read-only filter
    # - busy                # optional: self-hosted load threshold
    # - prefix_cache        # optional: pick either this or weighted_random
  cooldown:
    transient: 30s
    capacity:  10s
    permanent: 5m
    invalid:   0s           # no cooldown (semantics in §4)
    unknown:   0s           # no cooldown (semantics in §4)

scoring:
  enabled: false            # off by default; enabling switches to runtime scoring
  ema_decay: 0.2
  min_samples: 5
  latency_baseline: 200ms
```

在冷却持续时间中，0=无冷却；部署者将 invalid/unknown 设置为 0 是建议的默认值。

## 14. 演进规则（与 03 §12 一致/删节）

1. 跨模型回退只能来自客户端标头，绝不会通过网关的默认路径隐式降级。
2. 当添加新的端点Protocol / Capability.Modalities配置时，首先扩展资格，然后才让请求陷入重试/冷却。
3. 添加新的过滤器：执行 `selector.Filter` → 在 `cmd/gateway/buildSchedulerFilters` 中注册名称 → 添加 yaml 字段。
4.添加新的Scorer/Stats实现：接口在`internal/selector/scorer.go`；当需要跨副本一致性时，将 InMemoryStatsStore 替换为 Redis 实现，接口不变。
5. `internal/selector`从不持有repo依赖；任何需要查询 SQL 的东西都属于调度端口适配器或 cmd 连接。
6.运行时评分只能调整`EffectiveWeight`；它不能消除候选者，更不用说引入每个请求的状态机了。
7. **不要将责任推回中间件**：候选项获取、资格、重试/回退决策、配额
   保留/充电都在`dispatch.Dispatcher`中。M7中间件始终是一个薄适配器，
   仅执行 RC ↔ 调度。输入/结果映射 + 内容日志丰富 + 整体指标。

## 15.建议的代码阅读顺序

最快的入门路径是按以下顺序阅读：

1. [03-endpoint-scheduling.zh-CN.md](./03-endpoint-scheduling.zh-CN.md) §1（流程图）→§3（资格）→§6（错误分类）
2. `internal/middleware/schedule.go` `Schedule()`（瘦适配器，~165行，查看RC↔输入/输出映射）
3. `internal/dispatch/dispatcher.go` `Dispatch` / `step`（主调度定时循环，约250行）
4. `internal/dispatch/ports.go`（4个端口接口~150线；了解调度的接缝）
5. `internal/dispatch/eligibility.go`（纯函数~70行）
6. `internal/dispatch/adapters/`（3个文件~200行：将选择器/调用者/速率限制桥接到端口）
7. `internal/selector/types.go`（Candidate/Request/Result等数据结构）
8. `internal/selector/scheduler.go` `defaultScheduler.Pick / Report`（~50行实质性逻辑）
9.每个Filter（cooldown/limit_filter/busy/prefix_cache），根据需要
10. 要了解运行时评分，请返回 `internal/selector/scorer.go`

过了这一关，调度模块的所有控制流+数据流就都在你脑子里了。
