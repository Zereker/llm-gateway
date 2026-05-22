# 03 — Endpoint Scheduling

本文记录 M7 端点调度边界。调度层的目标不是做一个通用策略框架，而是把一次请求可靠地送到一个合格 endpoint：

1. 只尝试能承接当前请求的 endpoint。
2. 同一 model 内按 cooldown / endpoint quota / weight 选择 endpoint。
3. 失败时换下一个 endpoint。
4. 跨 model fallback 只按调用方 header 显式声明执行。

## 1. 简化后的边界

| 包 | 职责 |
|----|------|
| `pkg/middleware/model_service.go` (M5) | 解析 `X-Gateway-Fallback-Models`、逐 model 走 catalog + subscription、把已校验序列写到 `rc.ModelChain` |
| `pkg/middleware/schedule.go` (M7) | **thin adapter**：RC ↔ `dispatch.Input/Outcome` 映射；content log enrichment；总 metric。**不做调度决策** |
| `pkg/dispatch` | **调度执行时序的唯一所有者**：Dispatcher.Dispatch / step 主循环；4 个 port（CandidateSource / Selector / InvokerFactory / EndpointQuota）+ 3 个 Policy（AttemptCap / RetryPolicy / FallbackPolicy）+ `filterEligible` 内部 helper |
| `pkg/dispatch/adapters/` | 把 primitive 包桥成 dispatch port：selector → Selector / invoker → InvokerFactory / ratelimit → EndpointQuota |
| `pkg/selector` | selection primitives：对一批候选做 filter / scorer / picker。**不持有 repo，不知道 protocol / handler / fallback** |
| `pkg/invoker` | 拿 Handler 跑 `PrepareCall + HTTP Do + 响应 forward + 错误分类`（**不做协议查找**——dispatch 已通过 `protocol.Lookup` 拿到 Handler） |
| `pkg/protocol` | facade：`Handler = Factory + Translator + Quirks`；消费侧只看 `Handler / Lookup` |
| `pkg/ratelimit` | bucket / store primitives；`dispatch/adapters.EndpointQuotaAdapter` 把它接成 `dispatch.EndpointQuota` |
| `pkg/repo` | SQL endpoint reader + TTL LRU cached wrapper |

**关键边界**：执行时序（候选拉取 / eligibility / 选择 / 前扣 / 调用 / 上报 /
retry / fallback / 后扣）属于 `dispatch.Dispatcher`，**不在** middleware。M7
始终是 thin adapter——把 `*domain.RequestContext` 映射成 `dispatch.Input`，
调 `dispatcher.Dispatch(ctx, w, input)`，再把 `dispatch.Outcome` 映射回 RC（写
`RoutedModelService` / `Usage` / `Error` / `SchedulingDecision`）+ HTTP 响应。

**为什么这样分**：
- 调度时序对外不变（fallback chain / retry / quota / streaming 是网关核心契约）
  ——把它放到独立 package + 4 port，单测不依赖 gin / RC，配 fake port 跑得通
- middleware 关注请求生命周期 + RC 字段，timing 抽掉之后 schedule.go 缩到 ~165 行
- 跨 model fallback 在 dispatch 的 outer reducer（`Switch` action），M5 准备好
  `rc.ModelChain`，dispatch 按序消费；selector 完全不知道 fallback 概念

M5 已经把 `rc.ModelChain = [primary, fb1, fb2, ...]`（已校验过的 `*ModelService` 序列）准备好，dispatch.Dispatcher 直接消费，不再做 catalog/subscription 调用。找不到的 fallback 在 M5 阶段就被剔除了。

实际执行流（`pkg/dispatch/dispatcher.go`）：

```text
# M7 middleware（thin adapter）：
input := dispatch.Input{
    Envelope: rc.Envelope, Identity: rc.Identity,
    ModelChain: rc.ModelChain, Handlers: rc.Handlers, ...
}
outcome := dispatcher.Dispatch(ctx, w, input)
rc.RoutedModelService = outcome.RoutedModel
rc.SchedulingDecision = outcome.Decision
rc.Usage = outcome.Usage
// outcome.Result != Streamed 时按 HTTPCode / Class / Reason 写错误

# dispatch.Dispatcher.Dispatch（outer reducer）：
state := newState(input, AttemptCap.Resolve(input))
for {
    switch action := step(ctx, w, state).(type) {
    case Continue: continue                       # 同 model 再选
    case Switch:   state.SetModel(action.Next)    # FallbackPolicy 切下一个 model
    case Stream:   return state.Outcome()         # 已 StreamTo + ChargeUsage
    case Abort:    state.SetAbort(action); return state.Outcome()
    }
}

# dispatch.Dispatcher.step（单次 attempt）：
if state.Exhausted() → Abort{NoEndpoint, 503}
candidates := CandidateSource.ListForModel(ctx, model, group)
eligible   := filterEligible(candidates, env, handlers)         # 纯函数
if len(eligible) == 0 → FallbackPolicy.OnExhausted(state)
ep := Selector.Pick(ctx, eligible, query)                       # selector primitives
if denied := EndpointQuota.Reserve(ctx, ep) → Verdict + RetryPolicy.Decide
handler := state.Handlers().Get(ep, srcProto)                   # protocol.Lookup
res := InvokerFactory.For(ep, handler, env).Invoke(ctx)         # HTTP Do
state.Record(ep, verdict); Selector.Report(ctx, ep, verdict)
action := RetryPolicy.Decide(state, verdict)
if Stream → res.StreamTo(w); state.ApplyStream(rep); EndpointQuota.ChargeUsage(...)
return action
```

跨 model fallback 不能绕过模型可见性：M5 对每个 fallback model 都走完整 catalog + subscription 校验后才能进入 `rc.ModelChain`。fallback 不存在 / 未订阅 / 依赖瞬时报错时**静默剔除**（不阻断请求；primary 已经验过，request 仍然继续）。

## 2. Endpoint 数据

`domain.Endpoint` 是纯 domain 类型；repo 只负责把 SQL row 转成 domain。

目标字段示意：

```go
type Endpoint struct {
    ID      int64
    Name    string
    Vendor  string
    Model   string
    Group   string
    Weight  uint32
    Enabled bool

    Protocol Protocol          // endpoint 上游协议（v0.6 起 endpoint 级）

    Auth         EndpointAuth
    Routing      EndpointRouting
    Quota        EndpointQuota
    Capabilities EndpointCapabilities // 含 Modalities 子字段（v0.7）
    Quirks       json.RawMessage      // body / header 微调 DSL，pkg/protocol/quirks
}
```

候选查询按 `(model, group)` 匹配 enabled 且未软删的 endpoint，按 weight 降序返回。endpoint 是全局池，不带 account_id；主账号可见性在 M5 subscription 阶段处理。

`EndpointReader` 与 M5 的 `ModelCatalog` 同源，生产实现走
[06 §8 repo 缓存](./06-pluggable-infra.md#8-repo-缓存deployer-sql--gateway-数据传播)：
SQL 改 `endpoints` 表 → gateway repo 进程内 TTL LRU（默认 30s）自然过期后
miss 走 SQL 直查取到新值。`CachedEndpointReader` 同时维护 list cache
（`"model\x00group"` key）和 id cache，参数见 [06 §8.2](./06-pluggable-infra.md#82-适用表与默认参数)。

`EndpointCapabilities.SelfHosted` 决定 `FormSelfHosted`，不是从 vendor 名推断。

endpoint 字段约束：

- `Protocol`（核心列，**必填**）：该 endpoint 上游使用的协议，例如 `openai` /
  `anthropic` / `gemini` / `responses`。零值（ProtoUnknown）会让 `DefaultLookup.Get`
  返 nil → eligibility 剔除。
- `Capabilities.Modalities`（JSON 列子字段，**推荐显式声明**，可空）：该 endpoint
  实际承接的模态白名单，例如 `["chat"]` / `["embedding", "rerank"]`。
  - 非空：narrow vendor 上限，eligibility 要求**本字段 + vendor `SupportedModalities`
    都包含**当前请求模态（intersection；防 deployer widen vendor 实际能力）
  - 空：兼容旧数据 / 不想声明的场景，eligibility fall back 到 vendor `SupportedModalities`

## 3. 候选资格过滤

候选资格过滤应在进入 `pkg/selector` 之前完成。规则：

1. `protocol.Lookup.Get(ep, env.SourceProtocol)` 返 nil → 没有可用 Handler（缺
   vendor Factory、缺 translator、或 `ep.Protocol == ProtoUnknown`）→ 剔除。
2. 模态不支持 → 剔除。语义是 **narrow 不能 widen**：endpoint `Capabilities.Modalities`
   和 vendor `Handler.Capabilities().SupportedModalities` 都非空时**两者都要包含**当前模态；
   单边非空看那一边；都空 = 不限模态。防 deployer 误配 widen vendor 实际能力。

这些问题不是上游失败，不应该进入 `Scheduler.Report`，也不应该触发 cooldown。否则会把“不支持当前请求”的 endpoint 误标成坏 endpoint。

资格过滤是 dispatcher driver loop 的硬前置。缺 Factory、协议不匹配、缺 translator、模态不
匹配都是“不具备承接能力”，不能进入 retry/cooldown。

实现在 `pkg/dispatch/eligibility.go`（dispatch 内部 helper，不是独立 package），纯函数；
输入 `*domain.RequestEnvelope`、candidate endpoints、`protocol.Lookup`，输出 eligible
endpoints。dispatcher 在 `step()` 内调用，不内联复杂判断。

## 4. Selector 只做批内选择

`pkg/selector` 是 selection primitives——对一批候选跑 filter / scorer / picker，
输出一个 endpoint。**完全不知道** dispatch / protocol / handler / fallback 的存在。

接口形态（`pkg/selector/types.go`）：

```go
type Scheduler interface {
    Pick(ctx context.Context, req *Request) (*domain.Endpoint, error)
    Report(ctx context.Context, ep *domain.Endpoint, result Result)
}

type Candidate struct {
    Endpoint        *domain.Endpoint
    EffectiveWeight float64
}

type Request struct {
    Model      string
    Group      string
    Candidates []Candidate
    ExcludeIDs map[int64]struct{}
    PrefixKey  []byte
}
```

`Request` 不带 `LoadFallback` / `FallbackModels` / `attempts` 状态——这些都是
`dispatch.Dispatcher` 的内部状态（`state` 结构里的 `attempts` / `excluded` /
`modelChain` / `decisions`）。

dispatch 通过 `pkg/dispatch/adapters/SelectorAdapter` 把 `selector.Scheduler`
桥成 `dispatch.Selector`（接受 eligible endpoints + PickQuery）。selector 拿到
的永远是已 eligible 的候选列表，自己只跑 filter chain → scorer → picker。

`Pick` 无状态：输入候选 + 排除集，输出一个 endpoint。`Report` 只负责把失败反馈
给 cooldown / stats，不决定下一步控制流（控制流是 `dispatch.RetryPolicy.Decide`
和 `dispatch.FallbackPolicy.OnExhausted` 的事）。`WeightedRandomPicker` 始终基于
`Candidate.EffectiveWeight` 选择，不直接读 `Endpoint.Weight`。

## 5. 重试模型

保留两层就够，由 `dispatch.Dispatcher` 维护：

- **同 model 换 endpoint**（dispatcher 内层 `Continue` action）：一次调用失败且
  错误可重试时，state 把 endpoint 加入 `excluded`，下一轮 `step` 自然不会再 Pick。
- **跨 model fallback**（dispatcher 外层 `Switch` action）：只有请求带
  `X-Gateway-Fallback-Models` 时，`rc.ModelChain` 长度 > 1；`FallbackPolicy.OnExhausted`
  在当前 model 候选耗尽时返 `Switch{Next: 下一个 model}`，outer reducer 切 model
  继续下一轮 step。

两层都由 `dispatch.RetryPolicy.Decide` / `dispatch.FallbackPolicy.OnExhausted`
决定，不在 middleware 也不在 selector 里散落控制流。

- `cap`（最大 attempts）：`AttemptCap.Resolve(input)` 决定；默认实现
  `HeaderAttemptCap` 接受 `X-Gateway-Max-Attempts` header 覆盖（**只能让默认更严**）。
- `excluded`：state 维护，跨 model 累加。
- `decisions`：state 维护，每次 `Record(ep, verdict)` append；终态时 `finalize()`
  写到 `outcome.Decision`（即使 0 attempt 也填，详见 dispatch.Outcome.Decision 契约）。

默认不需要 L1 同 endpoint retry。网络抖动可以通过同 model 其它 endpoint 承接；如果未来确实需要同 endpoint retry，再作为显式 `RetryPolicy` 实现加回来，不在内部隐式开启。

`ClassInvalid` 表示请求本身无效（例如 translator 请求转换失败 / quirks compile 失败），
`DefaultRetry` 直接返 `Abort{400}`，不重试其它 endpoint，也不进 fallback model。

### 跨模型 fallback

模型之间能力不保证兼容。工具调用、结构化输出、上下文长度、视觉输入、reasoning 参数、响应风格都可能不同；网关无法可靠判断一个 fallback model 是否符合业务预期。

因此跨模型 fallback 只能由业务方在请求里显式给出：

```http
X-Gateway-Fallback-Models: gpt-4o-mini,deepseek-v3
```

网关只按声明顺序尝试这些模型的 endpoint，不做自动模型替换，也不根据 默认链路隐式降级。未带该 header 时，即使其它模型有可用 endpoint，也只在当前请求 model 内换 endpoint。

header 解析 + 校验全部在 **M5（`pkg/middleware/model_service.go`）** 完成，结果写到 `rc.ModelChain`。M7 不再读 header、不再调 catalog/subscription。规则：

- 去重并保持首次出现顺序；与 primary 同名的也剔除。
- 空 model 直接忽略。
- fallback model 数量上限，默认 3（`middleware.MaxFallbackModels`）。
- 每个 fallback model 都走 catalog + subscription 校验；任何一项失败（找不到 / 未订阅 / 依赖错）→ **静默剔除**该 fallback，不阻断主请求。
- primary 自身的校验失败仍然按原行为 abort（404 / 403 / 503）——fallback 解析失败不能"救回"已经失效的 primary。
- `rc.ModelChain[0] == rc.ModelService`，长度 ≥ 1。
- `SchedulingDecision.Attempt` 必须记录本次 attempt 对应的 model；`AttemptRole` 按在 chain 里的位置赋值（`[0]` → `primary`，其余 → `fallback`）。

## 6. 错误分类

调度层使用 `schedule.ErrorClass`：

| 类别 | 语义 | 是否继续尝试 |
|------|------|--------------|
| `success` | HTTP 2xx 且协议层成功 | 否 |
| `transient` | 5xx、网络错误、timeout、DNS 等 | 是 |
| `capacity` | 429 或 overloaded | 是 |
| `permanent` | 已选 endpoint 的上游 401/403/配置错 | 是，换 endpoint |
| `invalid` | 客户端 4xx 或翻译失败 | 否 |
| `unknown` | 无法分类 | 是 |

`pkg/invoker` 把 HTTP / 网络 / Handler `Classifier` 结果转换成该分类，dispatcher 再反馈给 `Scheduler.Report`。

vendor Factory 未注册、`ep.Protocol == ProtoUnknown`、translator 未注册都应在候选资格过滤阶段剔除（详见 §3），不作为上游 `permanent` 失败上报。

## 7. Filter 链

当前可保留的 filter：

- `cooldown`：排除短期失败的 endpoint。
- `limit_read`：排除 endpoint quota 超限的 endpoint。
- `weighted_random`：按 weight 选择一个 endpoint。

`prefix_cache` / `busy` 属于 self-hosted 优化，可以保留实现，但不应成为主流程理解成本。它们必须是可选 filter，并且放在资格过滤之后。

`limit_read` 只能基于 `SnapshotBatch` 做 read-only 过滤。endpoint RPM/RPS reserve 必须发生在 dispatcher Pick 出 endpoint 之后（`EndpointQuota.Reserve`），而不是 filter 阶段。

## 8. Runtime Scoring（后续演进）

当前调度只使用静态 `endpoint.weight`。这简单可控，但没有把运行时质量量化进选择过程：

- latency：最近窗口平均延迟 / p95 / EMA。
- success rate：最近窗口成功率、5xx、429、timeout 比例。
- cost：同一模型不同 vendor / endpoint 的成本倍率。

这部分应该作为 soft scoring 加入，而不是 hard filter。hard filter 负责“能不能选”，scoring 负责“更倾向选谁”。

目标流程：

```text
eligible candidates
  -> hard filters: cooldown / quota / busy-threshold
  -> scoring: latency / success_rate / cost 调整有效权重
  -> weighted pick by effective_weight
```

不要把 scoring 做成普通 `Filter`，因为 `Filter` 的语义是输入候选、输出候选，无法表达“调权重但不淘汰”。目标抽象可以单独建：

```go
type Scorer interface {
    Score(ctx context.Context, candidates []Candidate, req Request) []Candidate
}
```

引入 `Scorer` 后，`WeightedRandom` 应基于 `Candidate.EffectiveWeight` 选择，而不是直接读 `Endpoint.Weight`。

建议的第一版公式：

```text
effective_weight =
  base_weight
  * success_factor
  * latency_factor
  * cost_factor
```

约束：

- `base_weight` 来自 endpoint 行的 weight 列，保留人工运营控制权。
- `success_factor` / `latency_factor` 使用滑动窗口或 EMA，避免单次抖动影响过大。
- `cost_factor` 使用 endpoint 配置中的成本权重或离线下发的倍率，不在调度热路径实时查价或算账。
- 每个 factor 设置上下限，例如 `[0.1, 2.0]`，避免某个指标把权重打爆。
- 缺少运行时数据的新 endpoint 给中性 factor `1.0`，并保留少量探索流量。

运行时数据来源应独立成 `EndpointStatsStore`，由 `Scheduler.Report` 或 tracing/metric 异步写入。`Pick` 只读取轻量快照，不访问慢查询或复杂聚合。

`EndpointStatsStore` 和 Metrics 不是同一层：Metrics / Trace 是观测输出，保留较丰富标签；`EndpointStatsStore` 是调度内部读模型，只保存按 endpoint 聚合后的 EMA / 滑动窗口摘要。`Scheduler.Report` 可以同时写 metrics 和 stats store，但 `Pick` 只能读 stats store 的轻量快照。

### endpoint quota

endpoint quota 不应作为有副作用的候选 filter。候选阶段最多使用 `SnapshotBatch` 做 read-only 过滤；真正扣减只发生在 dispatcher Pick 之后（`EndpointQuota.Reserve` 前扣 / `ChargeUsage` 后扣）。

选中后：

- endpoint RPM/RPS 使用 `ReserveBatch` 前扣；超限则上报 `capacity`，排除该 endpoint 后继续选择。
- endpoint TPM 在 usage 产生后使用 `ChargeBatch` 后扣；后扣不改变本次响应。

## 9. Cooldown

gateway 当前装配 Redis cooldown manager，duration 来自 `scheduler.cooldown` 配置：

- transient
- capacity
- permanent
- invalid
- unknown

`Scheduler.Report` 对 retryable 失败 best-effort 标记 cooldown；标记失败不阻断请求。

资格过滤失败不进入 cooldown。

## 10. Health Probing（延后）

目标版本以被动 cooldown 和 runtime stats 为主，不在主链路引入主动探测。自部署 endpoint 的 active probe、启动期 warmup、恢复期探测属于后续能力：

- probe 结果不能替代资格过滤；协议/模态不支持仍然必须被剔除。
- probe 只影响 endpoint 健康或 scoring factor，不直接改业务配置。
- probe 结果可写入 `EndpointStatsStore`，作为 `success_factor` / `latency_factor` 的输入之一。

## 11. SchedulingDecision

`dispatch.Outcome.Decision` 由 `state.finalize()` 永远生成（即使 0 attempt 也填，详见 Outcome.Decision 契约）：

```go
type Attempt struct {
    Index       int
    Model       string
    EndpointID  string
    AttemptRole string // primary | fallback
    Outcome     AttemptOutcome
    LatencyMs   int64
    ErrorClass  string
}
```

`AttemptRole` 表示本次 attempt 对应的 model 是原始请求 model（`primary`）还是来自 `X-Gateway-Fallback-Models` 的 fallback model；它是 trace、metric `attempt_role` label（见 [08-observability §3](./08-observability.md#3-metrics)）和告警分析的同一信息源。

Outcome 推导：

- success -> `success`
- 最后一个失败 -> `fail`
- 中间失败 -> `fallback`

跨 model fallback 后的 attempts 继续追加到同一个 decision，通过 `Model` + `EndpointID` + `AttemptRole` 明确每次尝试的路由对象。

## 12. 演进规则

- `pkg/selector` 只处理一批候选，不负责从 repo 加载 fallback model。
- 跨 model fallback 只来自 `X-Gateway-Fallback-Models`；header 解析 + catalog/subscription 校验在 M5 完成，dispatch.Dispatcher 直接消费 `rc.ModelChain`，middleware 只透传。
- 新增 endpoint native protocol / modality 配置时，先补候选资格过滤，再让请求进入 retry/cooldown。
- 不能把协议不支持归类成上游失败；这会放大无效重试并污染 cooldown。
- 新增 filter 要在 `cmd/gateway buildSchedulerFilters` 中注册名称，并保持可选。
- runtime scoring 只能调整有效权重，不应该重新引入 per-request 状态机。
