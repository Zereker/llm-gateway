# 03a — Schedule 速查 / 上手伴读

这是 [03-endpoint-scheduling.md](./03-endpoint-scheduling.md) 的入门视角：先把 schedule
全貌讲一遍（数据流、各包职责、关键数据结构、装配点），再回到 03 看每条规则的设计理由。

> 03 讲 **why**，本文讲 **what / where**。改主链路代码仍以 03 为准。

## 0. TL;DR

调度的执行时序由 **`dispatch.Dispatcher`** 拥有，`middleware/schedule.go` 只是一个
thin adapter——把 gin / RC 映射成 `dispatch.Input`，跑 `Dispatch()`，再把
`dispatch.Outcome` 映射回 RC + HTTP。下面是真正的执行流：

```text
middleware/schedule.go (M7 thin adapter):
  envelope/identity/modelChain/handlers → dispatch.Input
  outcome := dispatcher.Dispatch(ctx, w, input)
  outcome → rc.{RoutedModelService, Usage, Error, SchedulingDecision} + HTTP

dispatch.Dispatcher.Dispatch (pkg/dispatch/dispatcher.go):
  state := newState(input, AttemptCap.Resolve(input))
  for {
      action := step(ctx, w, state)
      switch action {
      case Continue: 同 model 再选一个
      case Switch:   切下一个 model（FallbackPolicy 触发）
      case Stream:   流已写出，return Outcome
      case Abort:    终止，return Outcome
      }
  }

dispatch.Dispatcher.step (单次 attempt):
  if Exhausted → Abort(NoEndpoint, 503)
  candidates := CandidateSource.ListForModel(model, group)
  eligible   := filterEligible(candidates, env, handlers)   # pkg/dispatch/eligibility.go
  if no eligible → FallbackPolicy.OnExhausted(state)
  ep := Selector.Pick(eligible, query)                       # pkg/selector → adapters.SelectorAdapter
  if denied := EndpointQuota.Reserve(ep) → Verdict + RetryPolicy.Decide
  handler := state.Handlers().Get(ep, srcProto)              # protocol.Lookup
  res := InvokerFactory.For(ep, handler, env).Invoke(ctx)    # pkg/invoker → adapters.InvokerFactoryAdapter
  Selector.Report(ep, verdict)
  switch RetryPolicy.Decide(verdict):
  case Stream:   res.StreamTo(w) + EndpointQuota.ChargeUsage  # TPM 后扣
  case Continue / Switch / Abort: return action
```

三层职责分工：

| 谁 | 负责 |
|----|------|
| `pkg/middleware/schedule.go` | RC ↔ dispatch.Input/Outcome 映射；content log enrichment；metric `scheduling_duration_seconds`；**不做调度决策** |
| `dispatch.Dispatcher` (`pkg/dispatch`) | 调度执行时序的**唯一**所有者：候选 → 资格过滤 → 选择 → 前扣 → 调用 → 上报 → retry/fallback → 后扣 |
| `pkg/selector` | 在一批候选里跑 filter chain → scorer → picker，输出 1 个 endpoint。**无状态**，不知道 protocol / handler / repo / fallback |
| `pkg/invoker` | 拿 Handler 跑 `PrepareCall + HTTP Do + 响应 forward + 错误归类`（不做协议查找——dispatch 已通过 `protocol.Lookup` 拿到 Handler） |
| `pkg/protocol` | facade：Handler = Factory + Translator + Quirks；消费侧只看 Handler / Lookup |
| `pkg/ratelimit` | bucket / store primitives；`dispatch/adapters.EndpointQuotaAdapter` 把它接成 `dispatch.EndpointQuota` |
| `pkg/dispatch/adapters/` | 把上面 4 个 primitive 包桥成 dispatch 的 4 个 port（CandidateSource / Selector / InvokerFactory / EndpointQuota），把组合逻辑跟 primitives 解耦 |

`pkg/selector` 不持有 repo，不知道 fallback model 存在。跨 model fallback 是业务语义，
留在 dispatch.Dispatcher 的 outer loop（FallbackPolicy 触发 Switch action）。

## 1. 各包 / 各文件职责一览

```
pkg/middleware/schedule.go     M7 thin adapter（gin.HandlerFunc Schedule()）：
                               RC → dispatch.Input → dispatcher.Dispatch → RC

pkg/dispatch/                  调度执行时序的所有者
    dispatcher.go              Dispatcher.Dispatch / step 主循环
    eligibility.go             纯函数 filterEligible：输入 candidates + envelope + protocol.Lookup
                               输出 eligible endpoints；语义见 §2
    state.go                   per-request state；finalize 永远生成 SchedulingDecision
    action.go / verdict.go     Continue / Switch / Stream / Abort + Verdict 类型
    ports.go                   4 个 port 接口（CandidateSource / Selector / InvokerFactory / EndpointQuota）
    policy.go / fallback_chain.go / retry_default.go
                               AttemptCap / RetryPolicy / FallbackPolicy 默认实现
    cap_header.go              X-Gateway-Max-Attempts header 解析
    adapters/                  把 primitive 包桥成 dispatch port
        selector.go            selector.Scheduler → dispatch.Selector
        invoker.go             invoker.Sender   → dispatch.InvokerFactory
        quota.go               ratelimit.Store  → dispatch.EndpointQuota

pkg/selector/                  selection primitives，**不知道** protocol / handler / repo
    types.go                   Candidate / Request / Result / ErrorClass / Scheduler 接口
    scheduler.go               defaultScheduler：Pick（filter→scorer→picker）+ Report
    filter.go                  Filter 接口 + runChain
    cooldown.go                CooldownManager + RedisCooldownManager + CooldownFilter
    limit_filter.go            LimitReadFilter（SnapshotBatch 只读）
    busy.go / prefix_cache.go  self-hosted 优化 filter
    weighted.go                Picker 接口 + WeightedRandomPicker
    scorer.go                  Scorer + EndpointStatsStore + DefaultScorer

pkg/invoker/                   HTTP 调用 + forward stream，不做协议查找
pkg/ratelimit/                 Store / Bucket / endpoint bucket helpers
pkg/protocol/                  Handler facade + Factory/Session + quirks

cmd/gateway/                   composition root
    main.go                    把 primitives 串成 dispatch.Dispatcher
    dispatch_wiring.go         buildDispatcher：dispatch.New(WithCandidates / WithSelector /
                               WithInvokerFactory / WithQuota / WithCap / WithRetry /
                               WithFallback / WithTracer)
```

## 2. 三层过滤的语义边界（**这是 schedule 最容易搞混的点**）

| 层 | 谁 | 语义 | 失败后果 |
|----|----|------|----------|
| **Eligibility** | `pkg/dispatch/eligibility.go`（dispatch 内部 helper，不是独立 package） | 能不能承接：protocol.Lookup 拿不到 Handler / 模态不支持 | 剔除，**不入 cooldown，不算上游失败** |
| **Hard Filter** | `pkg/selector.Filter`（cooldown / limit_read / busy / prefix_cache） | 此刻该不该选：在冷却 / 配额耗尽 / 太忙 / prefix 亲和 | 当次 Pick 不选；不直接终止请求 |
| **Soft Scoring** | `pkg/selector.Scorer` | 更倾向选谁：success/latency/cost 调 `EffectiveWeight` | 只调权重，**不淘汰**候选 |
| **Selector** | `pkg/selector.Selector`（默认 weighted_random） | 在筛后候选里按 `EffectiveWeight` 选 1 | 全 0 → nil → 内层 break |

**核心原则**（03 §3）：能力性问题（缺 vendor Factory / translator / ep.Protocol unknown）必须在
Eligibility 阶段剔除——绝不能让它进 `Scheduler.Report`，否则会把"不支持"误标成"坏 ep"
触发 cooldown，污染后续选择。

## 3. 关键数据结构（types.go）

```go
type Candidate struct {
    Endpoint        *domain.Endpoint
    EffectiveWeight float64           // 静态时 = ep.Weight；Scorer 启用时被调权
}

type Request struct {
    Model      string                  // 当前轮次的 model（primary 或某个 fallback）
    Group      string                  // rc.Identity.Group
    Candidates []Candidate             // eligibility 之后的候选
    ExcludeIDs map[int64]struct{}      // 本请求已尝试过的 ep
    PrefixKey  []byte                  // 仅 PrefixCacheFilter 用
}

type Result struct {
    Class    ErrorClass                // 决定 cooldown TTL + 是否重试
    HTTPCode int
    Reason   string
    Latency  time.Duration
}

type Scheduler interface {
    Pick(ctx, *Request) (*domain.Endpoint, error)
    Report(ctx, *domain.Endpoint, Result)
}
```

**Request 故意不带** `attempts` / `fallbackModels` / `LoadFallback`——这些都是
dispatch.Dispatcher 外层 reducer 的职责，scheduler 只看一批候选。

## 4. ErrorClass 五分类速查

| Class | 触发场景 | IsRetryable | Cooldown |
|-------|----------|-------------|----------|
| `success` | HTTP 2xx + 协议层成功 | false | 不冷却 |
| `transient` | 5xx / 网络 / timeout / DNS | true | 按配置 TTL |
| `capacity` | 上游 429 / overloaded / 本地 reserve 超限 | true | 按配置 TTL |
| `permanent` | 上游 401 / 403 / 配置错 | true（换 ep） | 按配置 TTL |
| `invalid` | 客户端 4xx（除 401/403/429）/ translator 转换失败 | **false** | 不冷却 |
| `unknown` | 分类不出来 | true | **不冷却**（防止把分类 bug 放大成全集冷却） |

注意两个特殊点：

- `invalid` 命中时 M7 直接 `abort 400`，**既不**换 ep **也不**切 fallback model（参考
  `outcome.Class.IsRetryable()` + `errors.Is(callErr, upstream.ErrInvalidRequest)`）。
- `unknown` 虽 retryable 但 `Scheduler.Report` 里特判**不写 cooldown**（避免分类盲区污染冷却）。

## 5. 重试模型（两层，互补）

```
内层（同 model 换 endpoint）：
  失败 + retryable → excluded[ep.ID] = struct{}{} → 继续 Pick
  attempts 计入 totalAttempts，受 attemptsCap 限制
  (attemptsCap = min(cfg.MaxAttempts, X-Gateway-Max-Attempts) , 默认 3)

外层（跨 model fallback）：
  仅当 request 带 X-Gateway-Fallback-Models 才会切
  上限 MaxFallbackModels = 3（去重保序）
  每个 fallback model 都要重新过 M5（catalog + subscription）
  totalAttempts 在所有 model 之间累加，不重置
```

**关键**：同 endpoint retry（L1 retry）已经不再做——网络抖动靠"同 model 换 ep"承接。
未来如需再加，必须作为显式配置加回来，不能在 schedule 内部隐式开启。

## 6. Cooldown 流程

```
Scheduler.Report(ep, result) [scheduler.go:107]
  ├─ Stats.Record(ep.ID, result)          # 写 EndpointStatsStore（Scorer 输入）
  └─ if result.Class.IsRetryable()
        && result.Class != ClassUnknown   # unknown 不冷却
        && Cooldown != nil
        → Cooldown.Mark(ep.ID, class)     # Redis SET cd:endpoint:<id> <class> EX <ttl>
                                           # best-effort：err 仅记 log，不阻塞
```

**Redis 视角**：

```
key:   cd:endpoint:<id>
value: ErrorClass 字符串（诊断用）
TTL:   按 class 配置（CooldownDurations.Get）
```

后到的 Mark **直接覆盖** TTL —— 持续失败 = 持续冷却（符合预期）。

CooldownFilter 走 MGET 批量查；**Redis 错时 fail-open**（保留所有候选），避免 Redis
抖动 = 503 风暴。

## 7. Endpoint Quota（与 M6 用户侧 quota 严格分层）

| 时机 | 操作 | Bucket Key |
|------|------|------------|
| eligibility 之后 / Pick filter 链内 | `LimitReadFilter` 用 `SnapshotBatch` **只读**剔除已耗尽 | `rl:endpoint:<id>:rpm`、`...:rps` |
| Pick 选中 ep 后 / Send 之前 | M7 用 `ReserveBatch` 前扣 RPM/RPS；超限 → 反馈 capacity + 排除 ep + 继续 Pick | 同上 |
| Forward 完成（响应已结束） | M7 用 `ChargeBatch` 后扣 TPM（cost = `rc.Usage.Total`） | `rl:endpoint:<id>:tpm` |

**为什么前扣不在 filter 阶段做**：filter 输入是"候选集"，如果在那里 reserve，未被选中
的候选也被扣了 quota，会显著放大错误。所以 filter **永远只能 SnapshotBatch 只读**；
Reserve 只发生在已选中之后。

**TPM 必须后扣**：因为请求时不知道 usage.Total（要等 stream 结束 / 上游响应）。
TPM 超限只发 metric，不阻塞**本次**响应；下次请求才被 read filter 屏蔽。

## 8. Runtime Scoring（可选层）

默认关闭（`cfg.Scoring.Enabled = false`），启用后链路变成：

```
filter chain → Scorer.Score(candidates) → Selector.Select
                     ↑
       EndpointStatsStore.Snapshot(ep.ID)
                     ↑
       Scheduler.Report → Stats.Record（每次都写）
```

`DefaultScorer` 公式：

```
effective_weight = base_weight * success_factor * latency_factor
success_factor    = clamp(stats.SuccessRate,                [0.1, 2.0])
latency_factor    = clamp(latencyBaselineMs / stats.LatencyMs, [0.1, 2.0])
SampleCount < minSamples（默认 5）→ 中性 factor 1.0（保留探索）
```

`InMemoryStatsStore` 用 EMA（默认 decay=0.2）；多副本部署下每实例独立累积——如需跨副本
一致，把 store 换成 Redis-backed 实现，接口不变。

## 9. Header 速查

| Header | 含义 | 解析规则 |
|--------|------|----------|
| `X-Gateway-Fallback-Models` | 跨 model fallback 列表（逗号分隔） | 去重保序，空忽略，超出 `MaxFallbackModels=3` 截断 |
| `X-Gateway-Max-Attempts` | 客户端要求更紧的 attempts 上限 | 仅当 < cfg 默认值才生效，**不能**反向放大 |

**客户端只能让默认更严**——这是配置原则，避免恶意请求把网关 attempts 拉爆。

## 10. SchedulingDecision 写入点

```go
rc.SchedulingDecision = &domain.SchedulingDecision{
    Model:       rc.ModelService.Model,   // 原始请求 model
    RoutedModel: routedModelOf(rc),       // 实际命中的 model（含 fallback）
    UserGroup:   rc.Identity.Group,
    Attempts:    []domain.Attempt{...},    // 每次 Pick + Send 一条
    DurationMs:  ...,
}
```

每个 `Attempt`：

```go
type Attempt struct {
    Index       int        // 1, 2, 3 ... 跨 model 累加
    Model       string     // 本次 attempt 用的 model
    EndpointID  string
    AttemptRole string     // "primary" | "fallback"
    LatencyMs   int64
    ErrorClass  string
    Outcome     string     // success | fallback | fail
}
```

Outcome 三态推导：成功 = `success`；中间失败 = `fallback`；最后一次失败 = `fail`。

## 11. Metric 写入点

| Metric | 标签 | 写入位置 |
|--------|------|---------|
| `scheduling_duration_seconds` | model, attempts | M7 thin adapter defer 结束时 |
| `invoker_attempts_total` | model, routed_model, vendor, endpoint_id, attempt_role, result, error_class | 每次 Invoker.Invoke 之后（dispatch adapter） |
| `rate_limit_decisions_total` | scope="endpoint", dimension, result="violated" | EndpointQuota.Reserve 超限时 |
| `rate_limit_charge_total` | dimension="tpm", result | EndpointQuota.ChargeUsage 时 |
| `tpm_overflow_total` | layer="endpoint", dimension="tpm" | endpoint TPM 后扣溢出时 |
| `rate_limit_fail_open_total` | scope="endpoint", dimension="any" | LimitReadFilter Redis 错 fail-open 时 |
| `llm_gateway_repo_cache_total` | table, result | repo TTL LRU cache hit/miss/error |

Dispatch 内部还有 OTel span（`dispatch.request` / `dispatch.attempt`），attrs 含
model / endpoint.id / vendor / verdict.{stage,class,http_code,reason} / dispatch.outcome
/ dispatch.routed_model / dispatch.attempts，详见 [08 §4](./08-observability.md#4-tracing)。

完整 metric 契约见 [08-observability.md §3](./08-observability.md#3-metrics)。

## 12. 装配点（cmd/gateway/main.go + cmd/gateway/dispatch_wiring.go）

实际装配分两层：先把 selector / invoker / ratelimit 各自的 primitives 拼出来，
再喂给 `buildDispatcher` 组合成 `dispatch.Dispatcher`，最后把 Dispatcher（**而不是**
selector / sender）注入到 M7 middleware。

```go
// === main.go 准备 primitives ===

// 1. Cooldown manager
cooldown := selector.NewRedisCooldownManager(rdb, selector.CooldownDurations{...})

// 2. Filter chain（按 cfg.Selector.Filters 顺序）
filters := buildSchedulerFilters(cfg.Selector.Filters, rateStore, cooldown)

// 3. Scorer + Stats（可选）
stats, scorer := buildScoring(cfg.Scoring)

// 4. Scheduler primitives（selector.Scheduler 接口；纯批内 Pick + Report）
sched := selector.New(selector.Config{
    Filters: filters, Picker: selector.NewWeightedRandomPicker(),
    Cooldown: cooldown, Scorer: scorer, Stats: stats,
})

// 5. Sender primitives（invoker.Sender；纯 HTTP Do + forward）
sender := invoker.New(senderOpts...)

// === dispatch_wiring.go 把 primitives 组合成 Dispatcher ===

dispatcher := buildDispatcher(
    adaptEndpoints(endpointReader),  // CandidateSource 桥接 repo.EndpointReader
    sched,                            // Selector 桥（pkg/dispatch/adapters/SelectorAdapter）
    sender,                           // InvokerFactory 桥（adapters/InvokerFactoryAdapter）
    rateStore,                        // EndpointQuota 桥（adapters/EndpointQuotaAdapter；ratelimit.Store）
    cfg.Selector.MaxAttempts,         // AttemptCap.Default
    dispatchTracer,                   // OTel tracer，spans dispatch.request / dispatch.attempt
)

// buildDispatcher 内部：
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

// === 注入到 router.Deps；M7 middleware 只看 Dispatcher，不看 selector / sender ===
Dispatcher: dispatcher,
```

`buildSchedulerFilters` 把 yaml 里的字符串名映射到 Filter 实例：

| 名字 | 实现 |
|------|------|
| `cooldown` | `NewCooldownFilter(cd)` |
| `limit_read` | `NewLimitReadFilter(rateStore)` |
| `prefix_cache` | `NewPrefixCacheFilter(0)`（vnodes=64） |
| `busy` | `NewBusyFilter(0)`（threshold=0.85） |
| `weighted_random` | 忽略（已是 Selector，单独配） |
| 其它 | **panic**（fail-fast 暴露配置错） |

## 13. 配置 YAML 速查

```yaml
selector:
  max_attempts: 3
  filters:                  # 顺序敏感
    - cooldown              # 最便宜的过滤，放前面
    - limit_read            # endpoint quota 只读过滤
    # - busy                # 可选：self-hosted 负载阈值
    # - prefix_cache        # 可选：与 weighted_random 二选一
  cooldown:
    transient: 30s
    capacity:  10s
    permanent: 5m
    invalid:   0s           # 不冷却（语义见 §4）
    unknown:   0s           # 不冷却（语义见 §4）

scoring:
  enabled: false            # 默认关；启用后走 runtime scoring
  ema_decay: 0.2
  min_samples: 5
  latency_baseline: 200ms
```

Cooldown 时长里 0 = 不冷却；deployer 给 invalid / unknown 配 0 是默认推荐。

## 14. 演进规则（与 03 §12 对齐 / 简版）

1. 跨 model fallback 只能来自客户端 header，不能由 gateway 默认链路隐式降级。
2. 新增 endpoint Protocol / Capabilities.Modalities 配置时，先扩 eligibility，再让请求落到 retry/cooldown。
3. 新加 Filter：实现 `selector.Filter` → 在 `cmd/gateway/buildSchedulerFilters` 注册名字 → 加 yaml 字段。
4. 新加 Scorer / Stats 实现：接口在 `pkg/selector/scorer.go`；多副本一致性需求时把 InMemoryStatsStore 换成 Redis 实现，接口不变。
5. `pkg/selector` 永远不持有 repo 依赖；要查 SQL 的事都属于 dispatch port adapter 或 cmd 装配。
6. Runtime Scoring 只能改 `EffectiveWeight`，不能淘汰候选，更不能引入 per-request 状态机。
7. **职责不要塞回 middleware**：候选拉取、eligibility、retry/fallback 决策、quota
   reserve/charge 都在 `dispatch.Dispatcher`。M7 middleware 永远是 thin adapter，
   只做 RC ↔ dispatch.Input/Outcome 映射 + content log enrichment + 总 metric。

## 15. 看代码顺序建议

第一次上手按这个顺序读最快：

1. [03-endpoint-scheduling.md](./03-endpoint-scheduling.md) §1（流程图）→ §3（eligibility）→ §6（错误分类）
2. `pkg/middleware/schedule.go` `Schedule()`（thin adapter，~165 行，看 RC ↔ Input/Outcome 映射）
3. `pkg/dispatch/dispatcher.go` `Dispatch` / `step`（调度时序主循环，~250 行）
4. `pkg/dispatch/ports.go`（4 个 port 接口 ~150 行；理解 dispatch 的接缝）
5. `pkg/dispatch/eligibility.go`（纯函数 ~70 行）
6. `pkg/dispatch/adapters/`（3 个文件 ~200 行：把 selector / invoker / ratelimit 桥成 port）
7. `pkg/selector/types.go`（Candidate / Request / Result 等数据结构）
8. `pkg/selector/scheduler.go` `defaultScheduler.Pick / Report`（~50 行实质逻辑）
9. 各 Filter（cooldown / limit_filter / busy / prefix_cache），按需读
10. 想理解 runtime scoring 再回 `pkg/selector/scorer.go`

读完这一圈，schedule 模块的所有控制流 + 数据流就在脑里了。
