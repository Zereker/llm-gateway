[English](05-metering-billing.md) | [简体中文](05-metering-billing.zh-CN.md)

# 05 — 记录、计量和计费

该文档记录了网关需要持久化的数据及其通道。网关需要记录三类东西：

1. **内容记录**：请求/响应内容，用于审核、故障排除、重播、合规性。
2. **使用计量**：令牌/音频/图像等资源消耗，发送到计费平台。
3. **请求/响应指标**：延迟、状态、端点、重试链等，用于监控、调度和故障排除。

这三种数据不得合并为单个事件。它们在体积、灵敏度、可靠性要求和消费者方面有所不同。

## 1. 三个记录通道

|频道|内容 |初级消费者|可靠性要求|
|------|------|------------|------------|
|内容日志 |请求正文、响应正文、上游请求/响应，可选编辑/采样 ​​|审核、故障排除、合规性、重播 |可配置；通常是异步的，允许采样|
|使用事件 | `domain.Usage` + 身份/模型/端点/时间元 |计费平台、使用报告、TPM 后期收费 |高的;失败不得阻止响应，但必须有补偿/重试 |
| 指标 / trace | 持续时间、状态、错误类别、尝试、决策 | Prometheus、日志、调度分数 | 高吞吐、低成本；不记录大型请求体 |

设计原则：

- 请求/响应内容不进入使用事件。
- 使用事件不带有完整的提示/完成。
- 指标不携带大量有效负载，仅携带标签和数值。
- 所有三个通道均通过 `request_id` / `trace_id` 关联。

<a id="2-content-log"></a>
## 2. 内容日志

内容录制是一项可选功能；不应假定默认情况下所有部署都启用全身记录。理由：

- 请求/响应可能包含敏感数据。
- 响应流可能非常大。
- 流式场景需要并发读写，不能为了录制而阻塞主链。

内容录制应基于`internal/invoker`钩子：

|钩|录制内容 |目的|
|------|----------|------|
| `ClientRequestObserver` |客户原始请求正文 |用户视角审计|
| `UpstreamRequestObserver` |转换正文发送至上游|上游协议故障排除 |
| `UpstreamChunkObserver` |原始上游响应块 |上游重播/赛程|
| `ClientChunkObserver` |客户端实际收到的响应块 |用户视角审计/对账 |
| `AttemptCompleteObserver` |单次上游尝试的结果 |尝试级故障排除 |

推荐的记录结构：

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

    Body        []byte // or object storage pointer
    BodySHA256  string
    Truncated   bool
    Redacted    bool
    CreatedAt   time.Time
}
```

实施限制：

- 如果一个钩子需要持久化一个块，它必须复制字节；钩子回调接收到的切片可以被重用。
- 记录器必须支持异步缓冲区/反压策略，并且不得无限期地阻塞响应流。
- 默认的反压策略是drop-oldest，记录了下降的计数器；当需要强审计时，可以将其配置为阻止或写入对象存储指针，但这不能是默认的主链行为。
- 必须支持按账户、模型、端点、状态代码和采样率进行切换。
- 必须支持最大请求体尺寸；超出时截断或写入对象存储指针。
- 当需要合规时，在坚持之前进行编辑；如果编辑失败，请根据配置选择删除或写入摘要。

输出后端（驱动程序）仅支持 `none` / `file`：

- `none`：完全禁用，零开销。
- `file`：JSONL 附加到本地文件。

故意**不**在网关进程中嵌入 Kafka / S3 / Loki / ES 或其他生产者。内容日志本质上是一个日志记录/审核通道，而不是业务事件（与第 3 条中的使用事件不同）；在典型的部署中，它有多个下游消费者——归档（S3 / OSS）、搜索（Loki / ES）、内容安全后审查（Kafka）、训练数据反馈。让网关处理多接收器交付会将所有这些下游的可用性耦合到主链中。

正确的结构是使 `file` 成为唯一出口，并使用成熟的日志收集器（例如 Fluent-bit / Vector）负责扇出+重试+接收器适配：

```text
gateway ──→ content.jsonl ──→ fluent-bit / vector ──┬─→ S3 / OSS         (archiving + training data)
                                                    ├─→ Loki / ES        (troubleshooting search / compliance)
                                                    ├─→ Kafka topic      (content-safety post-review pipeline)
                                                    └─→ ...
```

文件本身的旋转/压缩/清理由外部 logrotate 或日志收集器处理（Fluent-bit 的尾部输入支持索引节点跟踪），而不是在网关进程内部处理。添加/调整接收器只需要更改 Fluent-bit 配置；网关不需要发布。

与使用事件的比较：

|尺寸|使用事件 (§3-5) |内容日志（本节）|
|------|---------------------|---------------------|
|自然 |财务对账所依赖的商业事件 |日志/审计 |
|后端 | `file` / `kafka` / `async_kafka` | `file`（网关唯一出口）|
|下游|计费平台（单一消费者）|多个接收器，由 fluid-bit 呈扇形分布 |
|损失成本|严重（错过账单）|采样/丢弃最旧的是可以容忍的 |
|架构演变| `schema_version` + 双写切换主题 |由 JSONL 字段演变而来；消费者容忍未知领域|

## 3. 使用事件

用量是计费平台消耗的资源消耗事件。它的来源是转换器/用量提取器：

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

- `Input` / `Output` / `Total` 是网关尽可能提取的通用令牌字段，用于基本统计和 TPM 后收费。 `Total` 的确切组合目标是**供应商自己的基于令牌的速率限制计数**，而不是成本模型 - 每个供应商的两者都有所不同（例如，供应商可能会以折扣方式对某个维度进行计费，同时仍将其与自己的速率限制进行计算，反之亦然）。 Anthropic 是具体案例：`Total = input_tokens + cache_creation_input_tokens + output_tokens`，镜像 Anthropic 自己的 ITPM/OTPM（缓存写入会计入 ITPM，即使它是按溢价计费的；对于除 Haiku 3.5 之外的任何当前型号，缓存读取都不会计入 ITPM，因此尽管也计费，但它仍然被排除在外）。添加新供应商时，请检查其速率限制文档，而不仅仅是其定价文档。
- `Truncated` 表示响应未完全完成，例如客户端断开连接或流响应中途停止。
- `Raw` 为上游原始使用对象，原样转发至计费平台。
- `Source` / `Estimator` / `Confidence` 识别使用的来源和可信度，以避免将估计值伪装成真实的上游数据。
- 下游计费平台从`Raw`中通过规则解析出复杂的计费维度；网关不维护特定于供应商的字段的枚举。

网关中不再定义像 `Details map[MetricKey]int64` 这样的扩展密钥。原因是每个供应商的使用维度不断变化。在网关中枚举它们将使计费规则更改取决于网关版本。下游可以根据 `vendor` / `model` / `protocol` / 请求发生的时间来选择解析规则。

使用来源优先级：

1、上游返回原始用量：填写`Raw`；通用字段可以从Raw中提取，`Source=upstream`、`Confidence=exact`。
2. 上游没有标准用量，但转换器可以可靠地从响应中解析等效字段：填写通用字段，`Source=extracted`、`Confidence=derived`。
3.上游没有用途：回退到分词器或字符计数估计，`Source=estimated`、`Estimator=tiktoken`或`naive_chars`， `Confidence=approximate`。

tiktoken 只是一个回退估计：

- 它不能覆盖真正的上游使用。
- 它不能保证覆盖每个供应商的标记器。
- 多模式、工具调用和推理标记可能不准确。
- 估算可用于 TPM 后期费用和场地使用；计费平台可以通过规则决定是否信任它。

`naive_chars`表示根据字符数粗略估计，具体除数由配置决定；不要将像 `chars/4` 这样的英语特定启发式硬编码到枚举语义中。

响应转发完成后M7写入：

```go
rc.Usage = fwd.Usage
```

M10 填写 `c.Next()` 后剩余的元并发布使用Outbox。如果发生跨模型回退，则使用元中的 `Model` 必须是实际成功的端点的模型，而不是原始请求模型。

异常路径上的用量：

- 如果客户端中途断线且已有部分token计数，则发布截止前累计使用量，即`Truncated=true`、`Confidence=approximate`；如果根本不可能进行计数，则不要构造通用令牌字段，但仍可能会发布具有错误状态的元事件。
- 在上游 5xx/网络错误后切换到下一个端点的失败尝试不会产生用量；最终成功的尝试产生了Usage。
- 如果在响应已经开始后发生错误，M10 仍会使用 `context.Background()` 发布已知的用量，并设置超时，以避免由于客户端断开连接而丢失用量。

## 4. 使用元数据

计费平台使用 `UsageMeta` 关联身份、型号、路由和请求发生的时间：

```go
type UsageMeta struct {
    AccountID         string
    Model             string
    Vendor            string
    EndpointID        string
    SubAccountID      string
    APIKeyID          string
    ServiceID         string
    RequestedModel    string
    RoutingPolicyID   string
    RoutingPolicyVersion uint64
    RoutingReason     string
    ModelServiceID    int64       // pricing lookup fingerprint; same source as ServiceID's RoutedModelService
    ServiceUpdateTime time.Time   // snapshot of model_services.updated_at
    RequestID         string
    TraceID           string
    StartTime         time.Time
    EndTime           time.Time
    TTFTMs            int64
    TotalLatency      int64
}
```

领域起源：

|元字段 |产地 |
|-----------|------|
| `RequestID` | M1 `rc.RequestID` |
| `TraceID` | `TraceIDFromCtx(c.Request.Context())` |
| `AccountID` / `SubAccountID` / `APIKeyID` | M2 `rc.Identity` |
| `Model` / `ServiceID` / `ModelServiceID` / `ServiceUpdateTime` | M7 `rc.RoutedModelService`，等于没有回退时的 M5 `rc.ModelService` |
| `RequestedModel` / `RoutingPolicyID` / `RoutingPolicyVersion` / `RoutingReason` | M5 `rc.ModelRoutingDecision`；对于具体请求，策略字段为空 |
| `Vendor` / `EndpointID` | M7 `rc.Endpoint` |
| `StartTime` | M1 `rc.StartTime` |
| `EndTime` / `TotalLatency` | M10 当前时间 |

`TTFTMs` 尚未捕获。

**在 `ModelServiceID` / `ServiceUpdateTime`**：这是下游计费聚合器的定价查找指纹。一旦M5获取到ModelService，它就已经持有`rc.ModelService`上的ID + UpdatedAt； M10 的 `fillUsageMeta` 以及 `Model` / `ServiceID` 在“路由优先”的基础上将它们复制到 Meta 中，确保在回退后所有 4 个字段都描述了同一型号计费。 **网关侧仍不查询主动定价**（§6原则不变）；它仅传递两个 model_service 字段作为下游价格查找的稳定指针。

<a id="5-usage-outbox"></a>
## 5. 使用Outbox

当前界面：

```go
type OutboxPublisher interface {
    Publish(c context.Context, evt *OutboxEvent) error
}

type OutboxEvent struct {
    Payload []byte
    Key     string
}
```

使用事件负载使用 JSON，采用以下推荐的请求信封结构：

```go
type UsageEvent struct {
    SchemaVersion string    `json:"schema_version"` // "usage.v1"
    EventID       string    `json:"event_id"`
    Usage         Usage     `json:"usage"`          // includes Meta; request_id / trace_id are inside Meta
    CreatedAt     time.Time `json:"created_at"`
}
```

建议的默认 Kafka 主题是 `billing.usage.recorded.v1`。主题命名遵循 **domain.entity.event.version** 约定，与生产服务的名称解耦 - 主题描述“这是什么业务事件”（已记录计费用量），而不是“谁发送的”。这使得下游计费/对账/配额服务可以按业务域订阅；如果多个服务后来产生使用事件，它们仍然使用相同的主题，避免像 `llm-gateway.usage` / `embedding-gateway.usage` / `image-gateway.usage` 这样的碎片。

分区键使用`AccountID`，因此同一计费主体的事件尽可能保持有序；当没有 AccountID 时，它会回退到 `Usage.Meta.RequestID`。 `request_id` / `trace_id` 仅放置在内部`Usage.Meta` — 在请求信封顶层不重复，以消除双写入不同步的潜在错误。 `CreatedAt` 是事件排队到Outbox的时间，不等于请求完成的时间；请求的时序分析应使用 `Usage.Meta.StartTime` / `Usage.Meta.EndTime`。

模式演化通过 `schema_version` 上的向后兼容分支进行；同一版本中的字段不会被删除。必须显式迁移重大更改：更喜欢切换到具有双写入周期和消费者切换的新主题（`billing.usage.recorded.v2`）；如果继续使用同一个Topic，必须允许多个Schema共存，消费者按`schema_version`进行路由/解析。Topic名称中的`.v1`是Topic级别的物理隔离，是与请求信封分离的机制`schema_version`：主题升级更改代理路由，模式升级更改字段解析。

M10 使用 `context.Background()` 进行发布并设置超时，以避免客户端断开连接而导致用量被写出。发布失败不影响业务响应。

实施：

- `file`：JSONL追加，仅适合本地开发/单机部署。
- `kafka`：同步生产者，没有本地副本（如果代理宕机，它就会丢失；不推荐）。
- `async_kafka`：缓冲、重试、退避、DLQ 主题；短暂的经纪商信号可以被挽救，但在长时间的停机期间它仍然会丢失。
- `file_and_kafka`：**建议用于生产** - 事务Outbox模式。文件是真相的来源
  （同步提交），Kafka是异步广播（尽力而为）。代理故障域与
  磁盘故障域，因此它们不会同时发生故障；当代理关闭时，文件已经被
  已提交，并且外部重播工具读取该文件以将丢失的事件重新发送到
  Kafka（消费端通过`event_id`进行幂等去重）。

失败语义：

|司机 |失效模式|默认行为 |可观察性|
|--------|----------|----------|--------|
| `file` |磁盘已满/IO错误|删除事件+错误日志| `llm_gateway_usage_publish_total{backend="file",result="error"}` |
| `kafka` |经纪人/领导者/网络不可用|重试直到发布超时，然后在失败时删除事件+错误日志 | `llm_gateway_usage_publish_total{backend="kafka",result="error"}` |
| `async_kafka` |缓冲区已满 |默认删除最旧的；可以配置为阻塞，但必须有超时 | `llm_gateway_outbox_dropped_total{driver="async_kafka"}` / 缓冲区深度 |
| `async_kafka` |重试已用尽|写入DLQ主题；如果 DLQ 失败，错误日志 + 指标 | `llm_gateway_outbox_dlq_total` |
| `file_and_kafka` |经纪人/网络不可用|文件已提交； Kafka 异步重试，然后在重试次数耗尽后写入 DLQ（如果已配置）或仅写入一个指标； **无数据丢失** | `llm_gateway_outbox_kafka_publish_error_total` |
| `file_and_kafka` |磁盘已满/IO错误| **严重** — 文件提交失败；仍然尝试 Kafka 但返回错误； M10 计数 `usage.publish.error` | `llm_gateway_outbox_file_error_total` |
| `file_and_kafka` |双重失败|文件错误返回M10； Kafka 错误被纳入指标 |上述两个指标同时递增 |

为什么不重用 `async_kafka + DLQ` 而不是 `file_and_kafka`：DLQ 位于与主要主题**相同的代理集群**上，因此如果代理完全关闭，则也无法写入 DLQ。 `file_and_kafka` 将文件和代理放在不同的故障域中，因此代理故障不会丢失数据。在双写下，DLQ 降级为“单消息级错误回退”（broker 在线，但消息本身有问题——太大、schema 无效、ACL 拒绝等），这是可选的，不是必需的。

可靠性要求：

- 使用事件是计费输入，并且必须优先考虑可补偿。
- 网关不得因为短暂的Outbox故障而阻止用户响应。
- `file`驱动仅适合本地开发或临时故障排除。
- 生产必须使用`file_and_kafka`：文件提供持久性回退，Kafka提供低延迟广播；并监控
  `outbox_file_error_total`（严重，磁盘问题），`outbox_kafka_publish_error_total`（数据
  安全但需要重播），以及 Kafka 消费者滞后。

## 6. 定价

在目标设计中，网关不做定价，也不需要查询请求路径上的活跃价格。

网关仅生成计费所需的事实数据：

- 账户/API密钥/子账户。
- 型号/服务 ID。
- 供应商/端点。
- 请求 ID / 跟踪 ID。
- 请求开始时间/结束时间。
- 使用价值。

具体的比价和金额计算由下游计费平台完成。计费平台根据使用事件中的请求发生时间，匹配当时有效的价格版本（通常为8`StartTime`，如果需要，可结合`EndTime`）。

这种方法的好处：

- 网关不知道复杂的定价规则，避免了定价查询对请求路径的依赖。
- 价格变更、更正和重新计算都发生在计费平台上。
- 使用事件是事实记录，不是货币结算结果。

网关不执行以下操作：

- 每小时/每日账单汇总。
- 账户余额扣除。
- 在线定价规则计算。
- 主动价格查询。
- 金额生成。

## 7. 指标/跟踪

指标记录请求和响应的运行时状态，而不记录大量负载。

M10目前记录：

- `llm_gateway_http_request_duration_seconds`
- 调度决策跟踪

推荐指标尺寸：

|指标|尺寸|目的|
|------|------|------|
|请求持续时间|方法、路径、状态、模型、供应商、endpoint_id | SLA / 延迟 |
|上游持续时间|供应商、端点 ID、模型、结果、错误类 |调度分数/故障排除|
|使用发布 |结果，后端 |计费链健康状况|
|内容日志发布|结果，后端，采样 |内容记录链健康度|
|调度程序尝试|模型、routed_model、供应商、endpoint_id、类、attempt_role |回退/冷却分析|
|调度持续时间|模型，尝试|调度过滤/挑选/报告的总时间|
|资格过滤持续时间|型号|资格过滤性能|
|策略缓存命中|层，结果 |配额策略缓存命中率|
|Outbox发布延迟 |驱动程序，结果 |Outbox写入延迟 |
|Outbox缓冲区深度 |司机 | `async_kafka` 缓冲区占用 |
|限速收费结果|维度，结果 |了解 TPM 充电后故障 |
| tpm 溢出 |层、维度 | TPM 后充电超过配置上限的次数 |

当指标用于运行时评分时，只会读取轻量级聚合统计信息，而不会读取内容日志。

## 8. 与速率限制的关系

速率限制不依赖于内容日志。

RPM/RPS在请求之前保留； TPM在产生使用后按照`Usage.Total`进行计费。如果`Usage.Total`来自tiktoken估算，则后充仍必须保留`Source=estimated` / `Confidence=approximate`标记，以便下游可以识别。

如果转换器/提取器仅获取上游的原始用量，但无法可靠地提取`Total`，则该使用事件仍会发布到下游计费平台，但该请求不会经过网关侧TPM后计费。

因此，使用捕获和通用字段提取覆盖率直接影响：

- 计费完整性。
- TPM 充电后精度。
- 使用报告的准确性。

当添加新的协议或转换器时，必须尽可能保留原始的上游用量为`Raw`。如果能可靠提取通用`Total`，则填写`Input` / `Output` / `Total`；如果不能，`Raw`仍应交给下游计费规则。

## 9. 记录策略

不同的通道应该有不同的默认值：

|数据|默认策略 |
|------|----------|
|使用事件 |默认开启 |
|指标/跟踪|默认开启 |
|客户请求正文 |默认关闭或采样 |
|客户端响应正文 |默认关闭或采样 |
|上游请求/响应 |默认关闭，仅在故障排除时启用 |

内容录制切换应支持：

- 通过账户/API 密钥。
- 按型号/端点/供应商。
- 按错误状态。
- 按采样率。
- 按最大请求体尺寸限制。
- 按字段编辑规则。

## 10. 与repo缓存的关系

使用事件通道（网关 → 下游计费）和 SQL → 网关配置传播
（repo 的进程内 TTL LRU 缓存，[06 §8](./06-pluggable-infra.zh-CN.md#8-repo-cache-deployer-sql--gateway-data-propagation)）
是**两个独立的通道**，并且不得相互重复使用：

|尺寸|使用事件Outbox |回购 TTL 缓存 |
|------|--------------------|---------------|
|数据方向|网关→下游计费| SQL → 网关 |
|触发|请求完成后M10主动发布 | SQL直接查找缓存未命中|
|交通 |卡夫卡主题`billing.usage.recorded.v1` |进程内 LRU；没有跨进程通道|
|可靠性 | DLQ+重试，失败不阻塞响应 | TTL 过期后回退到源； MySQL失败直接返回503 |
|架构演变| `schema_version` + 切换到新主题 |回购结构演变 |

存储库缓存只是对**配置数据**的读取路径优化；它不携带用途/内容。将用量写入
存储库缓存不匹配（使用的是事件流，而不是查找数据）；相反，通过使用路由 SQL 配置更改
Outbox也是错误的（计费消费者不应该看到模式类型事件）。

## 11.演进规则

- 当更改传递原始使用字段的策略时，请一起更新此文档、用量提取器/转换器和下游计费平台架构。
- 更改使用元字段时，一起更新下游计费管道架构。
- 使用Outbox发布必须保留“失败不影响业务响应”的语义；如果需要强一致的计费，补偿下游而不是阻塞M10。
- 内容日志不得重复使用使用事件模式；两者必须独立发展。
- 指标标签不得包含请求/响应正文或高基数字段。
- 网关不得计算请求路径上的金额；下游根据请求发生的时间进行价格匹配。
- 不要互相重复使用使用事件和存储库缓存通道（§10）。

## 12.下游消费者

§6 已经规定网关不执行“账单聚合/余额扣除/在线价格查找/金额生成”。这些都是下游的责任
计费平台，并且**超出此存储库的范围** - 此存储库仅生成 Kafka `billing.usage.recorded.v1`
事件；下游消费者实现聚合+查价+自行计费。

本文档定义了什么是使用事件、网关如何生成它、Outbox驱动程序和可靠性语义以及网关端
定价边界（“网关不做什么”列表）。对使用事件模式的任何更改都必须触发同步
下游消费者的评价。
