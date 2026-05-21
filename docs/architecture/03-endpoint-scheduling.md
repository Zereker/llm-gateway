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
| `pkg/middleware/schedule.go` (M7) | 遍历 `rc.ModelChain`、拉候选、调用资格过滤、driver loop、写 RC |
| `pkg/selector` | 对一批候选做 filter / pick / report / decision 记录 |
| `pkg/selector/eligibility` | 纯函数资格过滤：modality / native protocol / adapter / translator 可用性 |
| `pkg/invoker` | adapter / translator lookup、HTTP Do、响应 forward、错误分类 |
| `pkg/repo` | SQL endpoint reader |

`pkg/selector` 不应该持有 repo 依赖，也不应该自己切换 model。跨模型 fallback 属于业务语义变化：**校验在 M5 完成**，**切 model 的外层循环留在 M7**。`pkg/selector` 完全无感知。

M5 已经把 `rc.ModelChain = [primary, fb1, fb2, ...]`（已校验过的 `*ModelService` 序列）准备好，M7 直接遍历，不再做 catalog/subscription 调用。这样 M7 是纯驱动循环，没有 per-request msCache / subCache 状态，也没有"找不到 fallback 静默 continue"分支——找不到的 fallback 在 M5 阶段就被剔除了。

目标流程：

```text
# M5（model_service.go）已写好 rc.ModelChain = [primary, ...validated fallbacks]
# M7（schedule.go）：

for modelService in rc.ModelChain:
    candidates = EndpointReader.ListForModel(modelService.Model, group)
    candidates = eligibilityFilter(candidates, envelope)
    if candidates empty:
        continue

    for attempts < maxAttempts:
        ep = Scheduler.Pick(Request{Candidates, ExcludeIDs: excluded})
        if ep == nil:
            break

        outcome = Sender.Send(ep, envelope, rawBody)
        Scheduler.Report(ep, outcome.ToScheduleResult())

        if invalid request:
            abort 400
        if success:
            Sender.Forward(writer, response, responseHandler)
            rc.RoutedModelService = modelService
            rc.Endpoint = ep
            rc.Usage = forward.Usage
            return
        excluded.add(ep.ID)

abort 503
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

    NativeProtocol Protocol
    Modalities    []Modality

    Auth         EndpointAuth
    Routing      EndpointRouting
    Quota        EndpointQuota
    Capabilities EndpointCapabilities
}
```

候选查询按 `(model, group)` 匹配 enabled 且未软删的 endpoint，按 weight 降序返回。endpoint 是全局池，不带 account_id；主账号可见性在 M5 subscription 阶段处理。

`EndpointReader` 与 M5 的 `ModelCatalog` 同源，生产实现走
[06 §8 TieredCache](./06-pluggable-infra.md#8-cdcsql--gateway-数据传播)：SQL 改
`endpoints` 表 → Debezium event → L1 失效；下次 `ListForModel` 走 L3 SQL loader
拉最新值。当前 v0.4 默认只对 `model_services` 接入，`endpoints` 接入按
[06 §8.4](./06-pluggable-infra.md#84-适用表) 路径推进；未接入时走 SQL 直查。

`EndpointCapabilities.SelfHosted` 决定 `FormSelfHosted`，不是从 vendor 名推断。

endpoint 必须显式声明：

- `native_protocol`：该 endpoint 原生使用的上游协议，例如 `openai` 或 `responses`。
- `supported_modalities`：该 endpoint 能承接的模态，例如 chat、embedding、image。

## 3. 候选资格过滤

候选资格过滤应在进入 `pkg/selector` 之前完成。规则：

1. endpoint 必须支持 `env.Modality`。
2. endpoint 的 `native_protocol` 等于 `env.SourceProtocol`，或存在 `(env.SourceProtocol, endpoint.native_protocol)` translator。
3. endpoint 缺少 adapter、native protocol 配置非法、translator 未注册时，不进入本次 selection。

这些问题不是上游失败，不应该进入 `Scheduler.Report`，也不应该触发 cooldown。否则会把“不支持当前请求”的 endpoint 误标成坏 endpoint。

资格过滤是 M7 driver loop 的硬前置。缺 adapter、缺 native protocol、缺 translator 都是“不具备承接能力”，不能进入 retry/cooldown。

实现上放在 `pkg/selector/eligibility`，保持纯函数形态，输入 `RequestEnvelope`、candidate endpoints、adapter registry reader、translator registry reader，输出 eligible endpoints 和被剔除原因。M7 只调用它并记录 trace/metric，不内联复杂判断。

## 4. Scheduler 只做批内选择

目标接口应保持小，不再引入 per-request `Selection` 状态机：

```go
type Scheduler interface {
    Pick(ctx context.Context, req Request) (*domain.Endpoint, error)
    Report(ctx context.Context, ep *domain.Endpoint, result Result)
}

type Candidate struct {
    Endpoint        *domain.Endpoint
    EffectiveWeight float64
}

type Request struct {
    Model               string
    Group               string
    TPMCost             uint32
    Candidates          []Candidate
    ExcludeIDs          map[int64]struct{}
    PrefixKey           []byte
}
```

`Request` 不应包含 `LoadFallback`、`FallbackModels` 或 attempts 状态。M7 已经把某个 model 的候选列表算好后，scheduler 只处理这一批候选。未启用 runtime scoring 时，M7 用 `Endpoint.Weight` 构造 `Candidate{EffectiveWeight: weight}`。

M7 自己维护：

- `maxAttempts`：来自配置，可被 `X-Gateway-Max-Attempts` 降低。
- `ExcludeIDs`：本次请求里已经尝试过的 endpoint。
- `decisions`：每次 attempt 的结果，用于写 `rc.SchedulingDecision`。

`Pick` 是无状态选择：输入候选和排除集，输出一个 endpoint。`Report` 只负责把失败反馈给 cooldown / metric，不决定下一步控制流。`WeightedRandom` 始终基于 `Candidate.EffectiveWeight` 选择，不直接读 `Endpoint.Weight`。

## 5. 重试模型

保留两层就够：

- **同 model 换 endpoint**：一次调用失败且错误可重试时，M7 把 endpoint 加入 `ExcludeIDs`，下一轮 `Pick` 选择其它 endpoint。
- **跨 model fallback**：只有请求带 `X-Gateway-Fallback-Models` 时，`rc.ModelChain` 长度 > 1，M7 外层循环切到下一个 model。

默认不需要 L1 同 endpoint retry。网络抖动可以通过同 model 其它 endpoint 承接；如果未来确实需要同 endpoint retry，再作为显式配置加回来。

`ClassInvalid` 表示请求本身无效，例如 translator 请求转换失败，不重试其它 endpoint，也不进入 fallback model。

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

`pkg/invoker` 把 HTTP/网络/adapter classifier 结果转换成该分类，M7 再反馈给 `Scheduler.Report`。

adapter 未注册、协议不匹配、translator 未注册应在候选资格过滤阶段剔除，不作为上游 `permanent` 失败上报。

## 7. Filter 链

当前可保留的 filter：

- `cooldown`：排除短期失败的 endpoint。
- `limit_read`：排除 endpoint quota 超限的 endpoint。
- `weighted_random`：按 weight 选择一个 endpoint。

`prefix_cache` / `busy` 属于 self-hosted 优化，可以保留实现，但不应成为主流程理解成本。它们必须是可选 filter，并且放在资格过滤之后。

`limit_read` 只能基于 `SnapshotBatch` 做 read-only 过滤。endpoint RPM/RPS reserve 必须发生在 M7 选中 endpoint 之后，而不是 filter 阶段。

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

endpoint quota 不应作为有副作用的候选 filter。候选阶段最多使用 `SnapshotBatch` 做 read-only 过滤；真正扣减只发生在 M7 选中某个 endpoint 后。

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

M7 将 `schedule.Decision` 转成 `domain.SchedulingDecision`：

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
- 跨 model fallback 只来自 `X-Gateway-Fallback-Models`；header 解析 + catalog/subscription 校验在 M5 完成，M7 只消费 `rc.ModelChain`。
- 新增 endpoint native protocol / modality 配置时，先补候选资格过滤，再让请求进入 retry/cooldown。
- 不能把协议不支持归类成上游失败；这会放大无效重试并污染 cooldown。
- 新增 filter 要在 `cmd/gateway buildSchedulerFilters` 中注册名称，并保持可选。
- runtime scoring 只能调整有效权重，不应该重新引入 per-request 状态机。
