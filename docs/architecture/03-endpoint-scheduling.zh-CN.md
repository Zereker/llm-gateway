[English](03-endpoint-scheduling.md) | [简体中文](03-endpoint-scheduling.zh-CN.md)

# 03 — 端点调度

本文档记录了M7端点调度边界。调度层的目标不是构建通用策略框架，而是可靠地将请求传递到一个合格的端点：

1. 仅尝试能够实际服务当前请求的端点。
2. 在同一模型内，通过冷却时间/端点配额/权重选择端点。
3. 失败时，移动到下一个端点。
4. 跨模型回退仅在通过调用方标头显式声明时运行。

## 1. 简化边界

|套餐 |责任|
|----|----|
| `internal/middleware/model_service.go` (M5) |解析 `X-Gateway-Fallback-Models`，遍历每个模型的目录 + 订阅，将验证的序列写入 `rc.ModelChain` |
| `internal/middleware/schedule.go` (M7) | **薄适配器**：地图 RC ↔ `dispatch.Input/Outcome`；内容日志丰富；总体指标。 **不做出任何调度安排决定** |
| `internal/dispatch` | **调度执行顺序的唯一拥有者**：Dispatcher.Dispatch/step主循环； 4 个端口 (CandidateSource / Selector / InvokerFactory / EndpointQuota) + 3 个策略 (AttemptCap / RetryPolicy / FallbackPolicy) + 内部 `filterEligible` helper |
| `internal/dispatch/adapters/` |将原始包桥接到调度端口：选择器→选择器/调用器→InvokerFactory/速率限制→EndpointQuota |
| `internal/selector` |选择原语：过滤/评分/挑选一批候选者。 **不持有存储库，对协议/处理器/回退一无所知** |
| `internal/invoker` |获取一个 Handler 并运行 `PrepareCall + HTTP Do + response forwarding + error classification` （**不进行协议查找**——dispatch 已经通过 `protocol.Lookup` 获取了 Handler） |
| `internal/protocol` |门面：`Handler = Factory + Translator + Quirks`；消费者只能看到`Handler / Lookup` |
| `internal/ratelimit` |存储桶/存储原语； `dispatch/adapters.EndpointQuotaAdapter` 将其连接到 `dispatch.EndpointQuota` |
| `internal/repo` | SQL 端点读取器 + TTL LRU 缓存包装器 |

**关键边界**：执行顺序（候选项获取/资格/选择/预收费/调用/报告/
重试/回退/后充电）属于 `dispatch.Dispatcher`，**不是**中间件。 M7
始终是一个瘦适配器 - 它将 `*requeststate.State` 映射到 `dispatch.Input`，
调用 `dispatcher.Dispatch(ctx, w, input)`，然后将 `dispatch.Outcome` 映射回 RC（写入
`RoutedModelService` / `Usage` / `Error` / `SchedulingDecision`) + HTTP 响应。

**为什么要这样分割**：
- 调度顺序是外部合约，不得更改（回退链/重试/配额/流是核心网关合约）
  — 将其放入具有 4 个端口的独立包中，可以让单元测试避免 gin / RC，而是针对假端口运行
- 中间件关心请求生命周期+RC字段；一旦提取了时序，schedule.go 就会缩减至约 165 行
- 跨模型回退存在于调度的外部减速器中（`Switch`操作）； M5准备
  `rc.ModelChain`，dispatch按顺序消费；选择器对回退概念一无所知

M5 已经准备好 `rc.ModelChain = [primary, fb1, fb2, ...]` （经过验证的 `*ModelService` 序列）， `dispatch.Dispatcher` 直接使用它，无需任何进一步的目录/订阅调用。无法找到的回退已在 M5 阶段被丢弃。

实际执行流程（`internal/dispatch/dispatcher.go`）：

```text
# M7 middleware (thin adapter):
input := dispatch.Input{
    Envelope: rc.Envelope, Identity: rc.Identity,
    ModelChain: rc.ModelChain, Handlers: rc.Handlers, ...
}
outcome := dispatcher.Dispatch(ctx, w, input)
rc.RoutedModelService = outcome.RoutedModel
rc.SchedulingDecision = outcome.Decision
rc.Usage = outcome.Usage
// when outcome.Result != Streamed, write the error by HTTPCode / Class / Reason

# dispatch.Dispatcher.Dispatch (outer reducer):
state := newState(input, AttemptCap.Resolve(input))
for {
    switch action := step(ctx, w, state).(type) {
    case Continue: continue                       # reselect within the same model
    case Switch:   state.SetModel(action.Next)    # FallbackPolicy switches to the next model
    case Stream:   return state.Outcome()         # already StreamTo + ChargeUsage
    case Abort:    state.SetAbort(action); return state.Outcome()
    }
}

# dispatch.Dispatcher.step (a single attempt):
if state.Exhausted() → Abort{NoEndpoint, 503}
candidates := CandidateSource.ListForModel(ctx, model, group)
eligible   := filterEligible(candidates, env, handlers)         # pure function
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

跨模型回退无法绕过模型可见性：M5 在输入 `rc.ModelChain` 之前对每个回退模型运行完整的目录 + 订阅验证。如果回退不存在/未订阅/遇到暂时性依赖错误，则它会被**静默删除**（请求不会被阻止；主模型已被验证并且请求将继续）。

## 2. 端点数据

`domain.Endpoint`是纯域名类型；该存储库仅负责将 SQL 行转换为域对象。

示例性目标字段：

```go
type Endpoint struct {
    ID      int64
    Name    string
    Vendor  string
    Model   string
    Group   string
    Weight  uint32
    Enabled bool

    Protocol Protocol          // upstream protocol for the endpoint (endpoint-level since v0.6)

    Auth         EndpointAuth
    Routing      EndpointRouting
    Quota        EndpointQuota
    Capabilities EndpointCapabilities // includes the Modalities subfield (v0.7)
    Quirks       json.RawMessage      // body / header tuning DSL, internal/protocol/quirks
}
```

候选查询按 `(model, group)` 匹配启用的非软删除端点，并返回按权重降序排序的端点。端点形成一个全局池，没有account_id；主账户可见性在 M5 订阅阶段处理。

`EndpointReader`与M5的`ModelCatalog`共享其真实来源；生产实施如下
[06 §8 存储库缓存](./06-pluggable-infra.zh-CN.md#8-repo-cache-deployer-sql--gateway-data-propagation)：
对 `endpoints` 表进行 SQL 更改后，网关存储库的进程内 TTL LRU（默认情况下为 30 秒）
自然过期，下一次未命中就直接到SQL去取新值。
`CachedEndpointReader` 维护两个列表缓存
（密钥为 `"model\x00group"`）和 id 缓存；有关参数，请参阅[06 §8.2](./06-pluggable-infra.zh-CN.md#82-applicable-tables-and-default-parameters)。

`EndpointCapabilities.SelfHosted` 确定 `FormSelfHosted`;它不是从供应商名称推断出来的。

端点字段约束：

- `Protocol`（核心列，**必需**）：此端点使用的上游协议，例如`openai` /
  `anthropic` / `gemini` / `responses`。零值（ProtoUnknown）使得`DefaultLookup.Get`
  返回 nil → 资格下降。
- `Capabilities.Modalities`（JSON 列子字段，**建议显式声明**，可能为空）：
  该端点实际服务的模式白名单，例如`["chat"]` / `["embedding", "rerank"]`。
  - 非空：缩小供应商上限；资格要求**此字段和供应商的
    `SupportedModalities`** 包含当前请求的模式（交叉点；防止部署者
    扩大供应商的实际能力）
  - 空：为了向后兼容旧数据/不需要声明的情况，资格会回退
    发送至供应商的 `SupportedModalities`

<a id="3-candidate-eligibility-filtering"></a>
## 3.候选项资格过滤

输入 `internal/selector` 之前必须完成候选项资格筛选。规则：

1.如果`protocol.Lookup.Get(ep, env.SourceProtocol)`返回nil→没有可用的Handler（缺少
   供应商工厂、缺少转换器或 `ep.Protocol == ProtoUnknown`) → 已删除。
2. 不受支持的模式 → 已删除。语义是**狭窄，从不扩大**：当两个端点的
   `Capabilities.Modalities` 和供应商的 `Handler.Capabilities().SupportedModalities` 非空，**两者都必须
   包括**当前的模式；如果只有一侧非空，则检查该侧；如果两者都为空，则没有
   方式限制。
   这可以防止部署者的错误配置，从而扩大供应商的实际能力。

这些不是上游故障，不得报告给`Scheduler.Report`，并且不得触发冷却。否则，简单地“不支持当前请求”的端点会被错误地标记为坏端点。

资格过滤是调度程序驱动程序循环的硬性前提条件。缺少工厂、协议不匹配、缺少转换器和模态不匹配都是“缺乏服务能力”的情况，并且不得进入重试/冷却。

在 `internal/dispatch/eligibility.go` （内部调度助手，而不是独立包）中实现，作为纯粹的
功能；它需要 `*domain.RequestEnvelope`、候选端点和 `protocol.Lookup` 作为输入和输出
合格的端点。调度程序在 `step()` 中调用它，而不是内联复杂的条件。

## 4.选择器只做批量选择

`internal/selector` 提供选择原语 - 对一批候选者运行过滤器/评分器/选择器
输出一个端点。它**完全不知道**调度/协议/处理器/回退。

接口结构（`internal/selector/types.go`）：

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

`Request` 不包含 `LoadFallback` / `FallbackModels` / `attempts` 状态 - 这些是所有内部状态
`dispatch.Dispatcher` (`attempts` / `excluded` / `modelChain` / `decisions` 结构中的 `state` 字段）。

Dispatch 使用 `internal/dispatch/adapters/SelectorAdapter` 将 `selector.Scheduler` 桥接到 `dispatch.Selector` （其中
接受符合条件的端点 + PickQuery）。选择器总是收到已经符合资格的候选者列表，
并且只运行自己的过滤器链→记分器→选择器。

`Pick` 是无状态的：它将候选者 + 一组排除集作为输入并输出一个端点。 `Report` 仅供稿
失败返回到冷却/统计，并且不决定下一个控制流步骤（控制流属于
`dispatch.RetryPolicy.Decide` 和 `dispatch.FallbackPolicy.OnExhausted`)。

有两个选择器，通过 `gateway.yaml` 中的 `selector.picker` 选择：

- **`weighted_random`**（默认）— 按 `EffectiveWeight` 概率分布选择 1。始终选择
  基于`Candidate.EffectiveWeight`，从不直接读取`Endpoint.Weight`。
- **`p2c`**（两种选择的幂） - 通过 `EffectiveWeight` 对两个不同的候选样本进行采样，然后取其中一个
  待处理的调用较少（由 `selector.Inflight` 跟踪：调度程序在 Pick 上递增，在 Pick 上递减
  匹配的报告，因此计数器覆盖了上游响应标头的窗口 - 确切地说
  过载的上游队列）。平局为较高的 `EffectiveWeight`。这与运行时评分组成：
  评分改变了采样概率，P2C 通过实时负载打破了平局。计数器是每个进程的；每个
  副本平衡自己的视图（标准的 P2C 部署结构）。

<a id="5-retry-model"></a>
## 5. 重试模型

两层就够了，由`dispatch.Dispatcher`维护：

- **在同一模型内切换端点**（调度程序的内部 `Continue` 操作）：当调用失败并出现
  可重试错误，state将端点添加到`excluded`，接下来的`step`自然不会再Pick了。
- **跨模型回退**（调度程序的外部`Switch`动作）：仅当请求携带
  `X-Gateway-Fallback-Models` `rc.ModelChain` 的长度是否> 1； `FallbackPolicy.OnExhausted`
  一旦当前模型的候选者耗尽，并且外部减速器返回 `Switch{Next: next model}`
  切换模型并继续下一轮步骤。

两层均由 `dispatch.RetryPolicy.Decide` / `dispatch.FallbackPolicy.OnExhausted` 决定；
控制流永远不会分散在中间件或选择器中。

- `cap`（最大尝试次数）：由`AttemptCap.Resolve(input)`决定；默认实现
  `HeaderAttemptCap` 接受 `X-Gateway-Max-Attempts` 标头覆盖（**只能使默认更严格**）。
- `excluded`：由国家维护，跨模型累积。
- `decisions`：由国家维护；每个 `Record(ep, verdict)` 附加一个条目；在终端状态下，
  `finalize()` 将其写入 `outcome.Decision`（即使尝试 0 次也已填充 - 请参阅 `dispatch.Outcome.Decision`
  详细合同）。

默认不需要L1同端点重试。网络抖动可以被同一网络内的其他端点吸收
模型；如果将来确实需要同端点重试，则应将其作为显式添加回来
`RetryPolicy` 实现，内部未隐式启用。

`ClassInvalid` 表示请求本身无效（例如转换器请求转换失败/Quirks编译
失败）； `DefaultRetry` 直接返回 `Abort{400}`，无需重试其他端点或进入回退模型。

### 跨模型回退

模型之间的兼容性永远无法得到保证。工具调用、结构化输出、上下文长度、视觉输入、
推理参数和响应风格都可以不同；网关无法可靠判断是否为回退模型
满足业务期望。

因此，跨模型回退只能由调用者在请求中显式给出：

```http
X-Gateway-Fallback-Models: gpt-4o-mini,deepseek-v3
```

网关仅按照声明的顺序尝试这些模型的端点；它从不执行自动模型
替代，也不是基于某些默认链的隐式降级。当此标头不存在时，仅网关
即使其他模型有可用的端点，也会在当前请求的模型中切换端点。

头解析+验证完全在**M5(`internal/middleware/model_service.go`)**中完成，结果为
写入`rc.ModelChain`。M7不再读取标题或调用目录/订阅。规则：

- 重复数据删除，同时保留首次出现顺序；与主名称匹配的条目也会被删除。
- 空模型将被简单地忽略。
- 回退模型数量上限，默认为 3 (`middleware.MaxFallbackModels`)。
- 每个回退模型都经过目录+订阅验证；如果任何检查失败（未找到/未
  已订阅/依赖错误）→ 该回退被**默默地删除**，而不会阻止主请求。
- 验证主节点本身失败仍然遵循原始行为并中止 (404 / 403 / 503) - 回退
  解析失败无法“拯救”已经无效的主节点。
- `rc.ModelChain[0] == rc.ModelService`，长度≥1。
- `SchedulingDecision.Attempt` 必须记录该尝试对应的型号； `AttemptRole` 分配者
  在链中的位置（`[0]`→`primary`，其余→`fallback`）。

## 6. 错误分类

Dispatch 使用 `dispatch.Class` （外部 Verdict 字段），而选择器内部使用语义
相当于 `selector.ErrorClass`；两者在 `internal/dispatch/adapters/` 中双向映射（
dispatch→selector 方向为 `adapters/selector.go`，selector→dispatch 方向为
`adapters/invoker.go`'s `invokerClassToDispatch`)，保持调度独立于调用者/选择器类型和选择器
与调度类型无关：

|类别 |意义|继续重试吗？ |
|------|------|--------------|
| `success` | HTTP 2xx 和协议层的成功 |没有 |
| `transient` | 5xx、网络错误、超时、DNS等 |是的 |
| `capacity` | 429 或超载 |是的 |
| `permanent` |来自选定端点的上游 401/403/config 错误 |是的，切换端点 |
| `invalid` |客户端 4xx 或转换失败 |没有 |
| `unknown` |无法分类 |是的 |

`internal/invoker`将HTTP/network/Handler `Classifier`结果转换成这个分类，并且调度程序
将其反馈给 `Scheduler.Report`。

未注册的供应商 Factory、`ep.Protocol == ProtoUnknown` 或未注册的转换人员都应删除
在候选项资格过滤阶段（参见§3），并且不得报告为上游 `permanent` 失败。

## 7. 过滤器链

目前保留的过滤器：

- `cooldown`：排除最近出现短期故障的端点。
- `limit_read`：排除超出端点配额的端点。
- `prefix_cache`：提升可能命中前缀缓存的端点优先级。
- `busy`：排除超过繁忙阈值的端点。

`prefix_cache` / `busy` 是资格过滤之后执行的可选自托管优化。
最终选择不属于过滤器：过滤链执行后，由 `selector.picker` 在 `weighted_random` 和 `p2c` 之间选择策略。

`limit_read` 只能基于 `SnapshotBatch` 进行只读过滤。必须进行端点 RPM/RPS 预留
在调度程序选择端点（`EndpointQuota.Reserve`）之后，而不是在过滤器阶段。

## 8. 运行时评分（选择加入，默认禁用）

**实施状态**：已交付（`internal/selector` DefaultScorer + EndpointStatsStore）； `scoring.enabled` 已关闭
默认。启用后，`Scheduler.Report` 写入 EMA 统计数据，`Pick` 使用以下命令调整 `EffectiveWeight`
成功/延迟因素。统计数据存储有两个驱动程序：`inmemory`（每个副本，独立）和`redis`
（跨副本共享，一致评分）—参见 07-configuration 中的 `scoring:` 部分。接下来是
设计原则。

目前默认调度只使用静态的`endpoint.weight`，这个简单可控，但是
不将运行时质量纳入选择：

- 延迟：最近窗口平均延迟/p95/EMA。
- 成功率：最近窗口成功率，以及 5xx / 429 / 超时比率。
- 成本：同一模型的跨供应商/端点的成本乘数。

这应该作为软评分添加，而不是硬过滤器。硬过滤器决定“这个可以选择吗”，评分
决定“首选哪一个”。

目标流量：

```text
eligible candidates
  -> hard filters: cooldown / quota / busy-threshold
  -> scoring: latency / success_rate / cost adjust the effective weight
  -> weighted pick by effective_weight
```

评分不应该建模为普通的`Filter`，因为`Filter`的语义是“输入候选，输出
候选项”，不能表达“调整重量而不消除”。目标抽象可以是它自己的类型：

```go
type Scorer interface {
    Score(ctx context.Context, candidates []Candidate, req Request) []Candidate
}
```

一旦引入`Scorer`，`WeightedRandom`应该基于`Candidate.EffectiveWeight`进行选择，而不是
直接拨打`Endpoint.Weight`。

建议的第一版本公式：

```text
effective_weight =
  base_weight
  * success_factor
  * latency_factor
  * cost_factor
```

限制条件：

- `base_weight`来自端点行的权重列，保留手动操作控制。
- `success_factor` / `latency_factor` 使用滑动窗口或EMA，避免单一影响过大
  数据点。
- `cost_factor` 使用端点配置或离线分布式乘数的成本权重；它一定不能
  在调度热路径上实时查找价格或计算成本。
- 每个因素都有一个上限/下限，例如`[0.1, 2.0]`，防止任何单一指标夸大重量。
- 缺乏运行时数据的新端点获得中性因子`1.0`，并保留少量探索流量。

运行时数据的来源应该是独立的`EndpointStatsStore`，由异步写入
`Scheduler.Report` 或通过跟踪/指标。 `Pick` 只读取轻量级快照，执行速度不慢
查询或复杂的聚合。

`EndpointStatsStore` 和 Metrics 不是同一层：Metrics / Trace 是可观察性输出并保持丰富
标签； `EndpointStatsStore`是调度程序的内部读取模型，仅存储每个端点聚合的EMA /
滑动窗口摘要。 `Scheduler.Report` 可以写入指标和统计存储，但 `Pick` 只能读取
统计存储的轻量级快照。

### 端点配额

端点配额不能是具有副作用的候选过滤器。候选阶段最多可以使用
`SnapshotBatch` 用于只读过滤；实际扣除仅在调度员选择后发生
端点（`EndpointQuota.Reserve`预充电/`ChargeUsage`后充电）。

选择后：

- 端点RPM/RPS使用`ReserveBatch`作为预充电；如果超过限制，则报告为`capacity`，并且
  在继续选择之前，端点被排除。
- 端点 TPM 使用 `ChargeBatch` 作为使用量产生后的后付费；后充电不影响
  当前的响应。

## 9. 冷却时间

网关当前连接了 Redis 冷却管理器，持续时间来自 `selector.cooldown`
配置：

- 瞬态
- 容量
- 永久
- 无效
- 未知

`Scheduler.Report` 尽力标记可重试失败的冷却时间；标记失败不会阻止请求。

资格过滤失败永远不会进入冷却时间。

**重置感知 TTL**：当失败的上游响应携带自己的恢复提示时，冷却 TTL 如下
上游而不是静态每类持续时间。 `internal/invoker` 解析，按优先级顺序：`Retry-After`
（延迟秒或 HTTP 日期），OpenAI 样式 `x-ratelimit-reset-requests` / `x-ratelimit-reset-tokens` (Go
持续时间；两个桶都必须清除，因此取两者中的最大值），并采用人类可读的
`anthropic-ratelimit-*-reset`（RFC 3339，相同的最大规则）。提示穿过
`invoker.Outcome.RetryAfter → dispatch.Verdict.RetryAfter → selector.Result.RetryAfter`进入
`CooldownManager.Mark`，它被夹紧到 `[1s, 10m]` — 地板吸收亚秒级的搅动，盖子停止
端点中毒导致病理性“24 小时内重试”。配置持续时间为 0 的类别保持选择状态
出去；该提示永远不会重新启用它。然后，有效 TTL 会出现 ±10% 的抖动：供应商范围内的事件会冷却
整批端点具有相同的 TTL，并且没有抖动，它们都会在同一时刻恢复
并重新形成同步重试风暴。

冷却时间也可以提前结束——请参阅第 10 节中的探针门控恢复。

## 10. 健康探测

`internal/health.Prober` 主动探测**自托管**端点（`capabilities.self_hosted=true`
`capabilities.health_probe_endpoint` 设置）以固定间隔（`health.enabled`，默认关闭；请参阅 docs/07）。
供应商 API 端点永远不会被探测——那里的探测会消耗第三方的配额。

约束条件（与原设计相同）：

- 探测结果不能替代资格筛选；不支持协议/模式的端点
  仍然必须被丢弃。
- 探针仅影响端点健康信号；他们从不直接改变业务配置。
- 探测结果通过与 `Scheduler.Report` 相同的路径写入 `EndpointStatsStore`，作为
  `success_factor` / `latency_factor`。
- 主请求路径在探测时永远不会阻塞；被动冷却仍然是权威的失败信号。

**探针门控恢复**（`health.recover_cooldown`，默认关闭）：启用后，探针快照
每轮前目标都处于冷却状态； **成功**冷却端点调用的探测
`CooldownManager.Clear`，在 TTL 到期之前将其释放回旋转状态。因此，恢复被确认为
一个探针，而不是在 TTL 耗尽后花费用户请求来“试水”。这是严格
仅发布：失败的探测永远不会创建或延长冷却时间（探测失败和业务调用失败
不是相同的信号——探测 URL 可能已关闭，而推理仍然有效，反之亦然）。每早
释放发出 `llm_gateway_health_recover_total{endpoint_id}`。

## 11. 调度决策

`dispatch.Outcome.Decision` 始终由 `state.finalize()` 生成（即使尝试了 0 次，请参阅
结果.决策合同详情）：

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

`AttemptRole` 指示此尝试的模型是原始请求模型 (`primary`) 还是回退模型
型号来自 `X-Gateway-Fallback-Models`；它与跟踪使用的信息源相同，指标
`attempt_role`标签（参见[08-可观察性§3](./08-observability.zh-CN.md#3-metrics)）和警报分析。

结果推导：

- 成功 -> `success`
- 上次失败 -> `fail`
- 中间故障 -> `fallback`

跨模型回退后的尝试继续附加到相同的决策，其中 `Model` + `EndpointID` +
`AttemptRole` 明确每次尝试的路由目标。

## 12.演进规则

- `internal/selector` 仅处理单批候选项；它不负责从存储库加载回退模型。
- 跨模型回退仅来自 `X-Gateway-Fallback-Models`；头解析+目录/订阅验证发生在M5中，`dispatch.Dispatcher`直接消耗`rc.ModelChain`，中间件只传递它。
- 添加端点本机协议/模态配置时，首先添加候选资格过滤器，然后再让请求进入重试/冷却。
- 协议不兼容绝不能归类为上游故障；这样做会放大无用的重试并污染冷却时间。
- 新过滤器必须按名称注册在 `cmd/gateway buildSchedulerFilters` 中，并保持可选。
- 运行时评分可能仅调整有效权重；它不能重新引入每个请求的状态机。
