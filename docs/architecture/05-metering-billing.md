# 05 — Recording, Metering & Billing

本文记录网关需要沉淀的数据及其通道。网关要记录三类东西：

1. **内容记录**：request / response 内容，用于审计、排障、回放、合规。
2. **Usage 计量**：token / 音频 / 图片等资源消耗，用于发送给计费平台。
3. **请求响应指标**：latency、status、endpoint、重试链路等，用于监控、调度和排障。

三类数据不能混成一个事件。它们的体积、敏感性、可靠性要求、消费方都不同。

## 1. 三条记录通道

| 通道 | 内容 | 主要消费者 | 可靠性要求 |
|------|------|------------|------------|
| Content Log | 请求体、响应体、上游请求/响应，可选脱敏/采样 | 审计、排障、合规、回放 | 可配置；通常异步，允许采样 |
| Usage Event | `domain.Usage` + 身份/模型/endpoint/time meta | 计费平台、用量报表、TPM 后扣 | 高；失败不阻塞响应，但必须有补偿/重试 |
| Metrics / Trace | duration、status、error class、attempts、decision | Prometheus、日志、调度评分 | 高吞吐低成本；不记录大 body |

设计原则：

- request / response 内容不进入 usage 事件。
- usage 事件不携带完整 prompt / completion。
- metrics 不携带大 payload，只记录标签和数值。
- 三条通道都用 `request_id` / `trace_id` 关联。

## 2. Content Log

内容记录是可选能力，默认不应假设所有部署都开启完整 body 记录。原因：

- request / response 可能包含敏感数据。
- 响应流可能很大。
- 流式场景需要边读边写，不能为了记录阻塞主链路。

内容记录建议基于 `pkg/upstream` hooks：

| Hook | 记录内容 | 用途 |
|------|----------|------|
| `ClientRequestObserver` | 客户端原始请求 body | 用户视角审计 |
| `UpstreamRequestObserver` | 翻译后发给上游的 body | 上游协议排障 |
| `UpstreamChunkObserver` | 上游原始响应 chunk | 上游回放 / fixture |
| `ClientChunkObserver` | 客户端实际收到的响应 chunk | 用户视角审计 / 对账 |
| `AttemptCompleteObserver` | 单次上游尝试结果 | attempt 级排障 |

推荐记录形态：

```go
type ContentRecord struct {
    RequestID   string
    TraceID     string
    AccountID   string
    APIKeyID    string
    SubAccountID string
    Model       string
    Vendor      string
    EndpointID  string

    Direction   string // client_request | upstream_request | upstream_chunk | client_chunk
    Protocol    string
    Modality    string
    ContentType string

    Body        []byte // 或 object storage pointer
    BodySHA256  string
    Truncated   bool
    Redacted    bool
    CreatedAt   time.Time
}
```

实现约束：

- hook 内如需持久化 chunk，必须 copy bytes；hook 回调收到的 slice 可能复用。
- 记录器必须支持异步 buffer / backpressure 策略，不能无限阻塞响应流。
- 默认 backpressure 策略为 drop oldest，并记录 dropped counter；需要强审计时可配置为 block 或写 object storage pointer，但不能作为默认主链路行为。
- 必须支持按账号、模型、endpoint、状态码、采样率开关。
- 必须支持 max body size；超出后截断或写 object storage pointer。
- 需要合规时先脱敏再落盘；脱敏失败按配置选择 drop 或写摘要。

## 3. Usage Event

Usage 是计费平台消费的资源消耗事件。来源是 translator / usage extractor：

```go
type Usage struct {
    Input  int64
    Output int64
    Total  int64
    Truncated bool

    Raw        json.RawMessage
    Source     string // upstream | extracted | estimated
    Estimator  string // tiktoken | naive_chars | vendor_default
    Confidence string // exact | derived | approximate

    Meta UsageMeta
}
```

字段语义：

- `Input` / `Output` / `Total` 是网关尽量提取的通用 token 字段，用于基础统计和 TPM 后扣。
- `Truncated` 表示响应未完整完成，例如客户端断开或流式响应中途停止。
- `Raw` 是上游原始 usage 对象，原样发送给计费平台。
- `Source` / `Estimator` / `Confidence` 标识 usage 来源和可信度，避免把估算值伪装成上游真实值。
- 复杂计费维度由下游计费平台通过规则解析 `Raw`，网关不维护厂商专有字段枚举。

不再在网关定义 `Details map[MetricKey]int64` 这类扩展 key。原因是 usage 维度由厂商持续演进，放在网关枚举会让计费规则变化依赖网关发版。下游可以根据 `vendor` / `model` / `protocol` / 请求发生时间选择解析规则。

Usage 来源优先级：

1. 上游返回原始 usage：填 `Raw`，通用字段可从 Raw 提取，`Source=upstream`，`Confidence=exact`。
2. 上游没有标准 usage，但 translator 能稳定解析响应里的等价字段：填通用字段，`Source=extracted`，`Confidence=derived`。
3. 上游没有 usage：使用 tokenizer 或字符数兜底估算，`Source=estimated`，`Estimator=tiktoken` 或 `naive_chars`，`Confidence=approximate`。

tiktoken 只做兜底估算：

- 不能覆盖上游真实 usage。
- 不能保证适配所有 vendor tokenizer。
- 多模态、tool call、reasoning token 可能不准。
- 估算结果可用于 TPM 后扣和保底用量，计费平台可按规则决定是否采信。

`naive_chars` 表示按字符数做粗略估算，具体除数由配置决定；不要把 `chars/4` 这类特定英文经验值固化成枚举语义。

M7 在响应 forward 结束后写：

```go
rc.Usage = fwd.Usage
```

M10 在 `c.Next()` 后补齐 meta 并发布 usage outbox。若发生跨 model fallback，usage meta 中的 `Model` 必须是实际成功 endpoint 对应的 model，而不是原始请求 model。

异常路径下的 Usage：

- 流式响应中途客户端断开时，若已经能统计部分 token，则发布截断前累计 usage，`Truncated=true`，`Confidence=approximate`；若完全无法统计，则不构造通用 token 字段，但仍可发布带错误状态的 meta 事件。
- 上游 5xx / 网络错误后切换到下一个 endpoint 的失败 attempt 不产生 Usage；最终成功 attempt 产生 Usage。
- 响应已经开始后发生错误，M10 仍使用 `context.Background()` 加超时发布已知 Usage，避免客户端断开导致 usage 丢失。

## 4. Usage Meta

`UsageMeta` 用于计费平台关联身份、模型、路由和请求发生时间：

```go
type UsageMeta struct {
    AccountID    string
    Model        string
    Vendor       string
    EndpointID   string
    SubAccountID string
    APIKeyID     string
    ServiceID    string
    RequestID    string
    TraceID      string
    StartTime    time.Time
    EndTime      time.Time
    TTFTMs       int64
    TotalLatency int64
}
```

字段来源：

| Meta 字段 | 来源 |
|-----------|------|
| `RequestID` | M1 `rc.RequestID` |
| `TraceID` | `TraceIDFromCtx(rc.Ctx)` |
| `AccountID` / `SubAccountID` / `APIKeyID` | M2 `rc.Identity` |
| `Model` / `ServiceID` | M7 `rc.RoutedModelService`，未 fallback 时等于 M5 `rc.ModelService` |
| `Vendor` / `EndpointID` | M7 `rc.Endpoint` |
| `StartTime` | M1 `rc.StartTime` |
| `EndTime` / `TotalLatency` | M10 当前时间 |

`TTFTMs` 当前暂未捕获。

## 5. Usage Outbox

当前接口：

```go
type OutboxPublisher interface {
    Publish(c context.Context, evt *OutboxEvent) error
}

type OutboxEvent struct {
    Payload []byte
    Key     string
}
```

Usage Event payload 使用 JSON，建议 envelope 形态：

```go
type UsageEvent struct {
    SchemaVersion string    `json:"schema_version"` // "usage.v1"
    EventID       string    `json:"event_id"`
    RequestID     string    `json:"request_id"`
    TraceID       string    `json:"trace_id"`
    Usage         Usage     `json:"usage"`
    CreatedAt     time.Time `json:"created_at"`
}
```

Kafka topic 建议默认 `llm-gateway.usage`。partition key 使用 `AccountID`，让同一计费主体的事件尽量保持顺序；没有 AccountID 时退化为 `RequestID`。顶层 `RequestID` / `TraceID` 是便利字段，必须与 `Usage.Meta` 内同名字段一致；如果消费端发现冲突，以 `Usage.Meta` 为准并记录坏事件。`CreatedAt` 表示 outbox 入队时间，不等同于请求完成时间；请求时序分析应使用 `Usage.Meta.StartTime` / `Usage.Meta.EndTime`。

schema 演进通过 `schema_version` 做向后兼容分支，不在同一版本中删除字段。破坏性变更必须显式迁移：优先切新 topic（例如 `llm-gateway.usage.v2`）并经历双写期和 consumer 切换；若继续使用同一 topic，必须允许多 schema 共存，consumer 按 `schema_version` 路由解析。

M10 使用 `context.Background()` 加超时发布，避免客户端断开导致 usage 不写出。发布失败不影响业务响应。

实现：

- `file`：JSONL append，适合本地/兜底。
- `kafka`：同步 producer。
- `async_kafka`：buffer、重试、backoff、DLQ topic。

故障语义：

| Driver | 故障模式 | 默认行为 | 可观测 |
|--------|----------|----------|--------|
| `file` | 磁盘满 / IO error | drop event + error log | `llm_gateway_usage_publish_total{backend="file",result="error"}` |
| `kafka` | broker / leader / 网络不可用 | retry 到 publish timeout，失败后 drop event + error log | `llm_gateway_usage_publish_total{backend="kafka",result="error"}` |
| `async_kafka` | buffer 满 | 默认 drop oldest；可配置 block，但必须有 timeout | `llm_gateway_outbox_dropped_total{driver="async_kafka"}` / buffer depth |
| `async_kafka` | 重试耗尽 | 写 DLQ topic；DLQ 失败则 error log + metric | `llm_gateway_outbox_dlq_total` |

可靠性要求：

- Usage event 是计费输入，必须优先保证可补偿。
- 网关不能因为 outbox 短暂失败阻塞用户响应。
- `file` driver 仅适合本地开发或临时排障；生产必须使用 `async_kafka` + DLQ，并监控队列长度、失败率、DLQ 数量。

## 6. Pricing

目标设计里，网关不做计价，也不需要在请求路径上查询 active price。

网关只产生计费所需的事实数据：

- account / API key / sub account。
- model / service id。
- vendor / endpoint。
- request_id / trace_id。
- request start time / end time。
- usage 数值。

具体价格匹配和金额计算由下游计费平台完成。计费平台根据 usage event 中的请求发生时间（通常使用 `StartTime`，必要时可结合 `EndTime`）去匹配当时生效的价格版本。

这样做的好处：

- 网关不感知复杂价格规则，避免请求路径依赖 pricing 查询。
- 改价、补账、重算都在计费平台完成。
- usage event 是事实记录，不是金额结算结果。

网关不做：

- 小时/天级账单聚合。
- 账户余额扣费。
- pricing rule 在线计算。
- active price 查询。
- 金额生成。

## 7. Metrics / Trace

指标记录请求响应的运行状态，不记录大 payload。

当前 M10 记录：

- `llm_gateway_http_request_duration_seconds`
- scheduling decision trace

建议指标维度：

| 指标 | 维度 | 用途 |
|------|------|------|
| request duration | method, path, status, model, vendor, endpoint_id | SLA / 延迟 |
| upstream duration | vendor, endpoint_id, model, result, error_class | 调度评分 / 排障 |
| usage publish | result, backend | 计费链路健康 |
| content log publish | result, backend, sampled | 内容记录链路健康 |
| scheduler attempt | model, routed_model, vendor, endpoint_id, class, attempt_role | fallback / cooldown 分析 |
| scheduling duration | model, attempts | 调度 filter / pick / report 总耗时 |
| eligibility filter duration | model | 资格过滤性能 |
| policy cache hit | layer, result | quota policy 缓存命中率 |
| outbox publish latency | driver, result | outbox 写入延迟 |
| outbox buffer depth | driver | `async_kafka` buffer 占用 |
| ratelimit charge result | dimension, result | TPM 后扣失败可见度 |
| tpm overflow | layer, dimension | TPM 后扣超过配置上限的次数 |

指标用于 runtime scoring 时，只读聚合后的轻量统计，不读取 Content Log。

## 8. 与限流的关系

限流不依赖 Content Log。

RPM / RPS 在请求前 reserve；TPM 在 usage 产出后按 `Usage.Total` charge。若 `Usage.Total` 来自 tiktoken 估算，后扣也必须保留 `Source=estimated` / `Confidence=approximate` 标记，供下游识别。

若 translator / extractor 只拿到上游原始 usage 但无法稳定提取 `Total`，仍然发布 usage event 给下游计费平台，但本次请求不做网关侧 TPM 后扣。

因此 usage 捕获和通用字段提取覆盖率直接影响：

- 计费完整性。
- TPM 后扣准确性。
- 用量报表准确性。

新增协议或 translator 时，必须尽量保留上游原始 usage 到 `Raw`。如果能稳定提取通用 `Total`，则填充 `Input` / `Output` / `Total`；如果不能提取，仍应把 `Raw` 交给下游计费规则处理。

## 9. 记录策略

不同通道的默认策略应不同：

| 数据 | 默认策略 |
|------|----------|
| Usage Event | 默认开启 |
| Metrics / Trace | 默认开启 |
| Client request body | 默认关闭或采样 |
| Client response body | 默认关闭或采样 |
| Upstream request / response | 默认关闭，仅排障开启 |

内容记录的开关建议支持：

- 按 account / API key。
- 按 model / endpoint / vendor。
- 按错误状态。
- 按采样率。
- 按最大 body 大小。
- 按字段脱敏规则。

## 10. 与 CDC 的关系

Usage Event 通道与 admin → gateway 配置传播的 [CDC（06 §8）](./06-pluggable-infra.md#8-cdcadmin--gateway-数据传播)
是**两条独立通道**，不要互相复用：

| 维度 | Usage Event Outbox | CDC |
|------|--------------------|-----|
| 数据流向 | gateway → 下游计费 | admin → gateway |
| 触发源 | 请求完成后 M10 主动 publish | admin 写 MySQL 后 Debezium 捕获 binlog |
| 传输 | Kafka topic `llm-gateway.usage` | Redis Stream `llm_gateway.llm_gateway.<table>` |
| 可靠性 | DLQ + 重试，失败不阻塞响应 | 最终一致 + L3 SQL 兜底，consumer 退避重连 |
| schema 演进 | `schema_version` + 切新 topic | Debezium 自然兼容 unknown field |

CDC 是**配置数据**的传播通道，不承载 usage / 内容。把 usage 写到 CDC stream
不对（破坏 admin → gateway 的单向性 + Redis Stream 没有 DLQ 语义）；反过来把
admin 配置变更走 usage outbox 也不对（计费 consumer 不应该看到 schema 类事件）。

## 11. 演进规则

- 修改 usage 原始字段传递策略时同步更新本文档、usage extractor / translator 和下游计费平台 schema。
- 修改 usage meta 字段时同步更新下游 billing pipeline schema。
- Usage outbox 发布必须保持“失败不影响业务响应”的语义；需要强一致计费时应在下游补偿，而不是阻塞 M10。
- Content Log 不能复用 Usage Event schema；两者必须独立演进。
- 指标标签不得包含 request / response body 或高基数字段。
- 网关不得在请求路径上计算金额；价格匹配由下游按请求发生时间完成。
- 不要把 Usage Event 与 CDC stream 互相复用（§10）。
