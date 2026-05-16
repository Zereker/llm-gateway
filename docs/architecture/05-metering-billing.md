# 05 — Metering & Billing

本文定义计量与计价层：`domain.Usage` 数据总线、`TokenExtractor` 提取层、`PricingSpec` 版本化定价、在线计量管道（应用层 → Kafka）+ 离线计价聚合（流处理器）的 Kappa 架构。

> **阅读前**：[02](02-protocol-translation.md) 的 `adapter.ResponseSession`；[01](01-request-pipeline.md) 的 M10 Tracing 契约；[06](06-pluggable-infra.md) 的 `usage.OutboxPublisher` / `archive.Store` 抽象接口。

## 1. 范围与目标

**范围**：从 Adapter 拿到上游响应那一刻起，到把"按请求时价格计算的成本"产出为账单事件为止的整条链路。**反向通道**（账单系统 → 网关的欠费 / 状态广播）参见 §13。

**目标**：

| # | 目标 | 成功判据 |
|---|------|---------|
| G1 | 限流 / 明细 / 聚合 / 账单**共享同一份 Usage** | 删除其中任一消费者，其他路径不受影响；新增维度改一处，多处同步 |
| G2 | 新增计价规则**不改代码** | 改 `ModelService.SpecDetail` JSON（含可选 CEL 表达式）即可 |
| G3 | 新厂商 Usage 提取**集中到一处** | 不在 Adapter 散写 JSON 字段取数；走 `TokenExtractor` 注册 |
| G4 | 新维度（音频秒 / 视频时长 / 图像张数）改动 ≤ 3 处 | `domain.MetricKey` 集中枚举；Extractor 填充 + Pricing 配置 |
| G5 | **以请求发生时的价格计价**（point-in-time billing） | 价格改动后，未上报的请求按改动前价格结算 |
| G6 | 计量数据强可靠 | 应用 crash / Kafka 不可用都不丢；本地日志 = 单一事实源 |

## 2. 设计原则

| # | 原则 | 含义 |
|---|------|------|
| U1 | **Usage = 数据总线** | 所有下游（限流 / 明细 / 计价 / 监控）只读同一份 `domain.Usage`；杜绝各自解析上游响应 |
| U2 | **流式 / 非流式同接口** | `Extractor.NewSession()` → `Session.Feed/Finalize`；非流式即 Feed 一次的特例 |
| U3 | **价格 = 数据 + 表达式** | 默认走结构化字段（input/output 单价 + 倍率）；例外走 CEL 表达式 |
| U4 | **在线只计量，离线做计价聚合** | 应用层只产出 `domain.Usage` 事件；聚合（小时窗口）+ 计价由流处理器执行 |
| U5 | **请求时价（point-in-time billing）** | 消息携带价格版本指纹；流处理器按指纹查 history 表得到当时的价格 |
| U6 | **本地日志 = 单一事实源** | 应用层同步 fsync 本地日志再异步发 Kafka；Kafka 失败不影响业务返回；旁路日志收集器归档到对象存储 |

## 3. 数据结构

### 3.1 domain.Usage

```go
// pkg/domain/usage.go
package usage

import "time"

// Usage 是单次请求的资源消耗快照。所有下游消费者共享同一份。
type Usage struct {
    // 主字段（语义公约）
    Input     int64 // 输入 token 数；约定包含所有 cache 相关部分（见 5.1 缓存语义归一）
    Output    int64 // 输出 token 数
    Total     int64 // 总数；有值时以此为准；无值时 = Input + Output + Reasoning
    Reasoning int64 // 推理 token（OpenAI o-系列、Gemini thoughts、DeepSeek reasoning_content）

    // 扩展维度（按需填充；Key 集中定义见 3.2）
    Details map[MetricKey]int64

    // 元信息
    Meta Meta
}

type Meta struct {
    Model        string // 客户端可见的模型名
    Vendor       string // 实际命中的上游厂商
    EndpointID   string // 实际命中的 endpoint
    UserID       string
    APIKeyID     string
    ServiceID    string
    RequestID    string
    TraceID      string
    StartTime    time.Time // 请求进入网关时间
    EndTime      time.Time // Finalize 时刻
    TTFTMs       int64     // first token / chunk 时刻 - StartTime
    TotalLatency int64     // EndTime - StartTime

    // 价格版本指纹（用于离线 Enrich）
    Pricing PricingFingerprint
}

type PricingFingerprint struct {
    ModelServiceID    int64
    ServiceUpdateTime time.Time
}
```

### 3.2 domain.MetricKey

```go
// pkg/domain/usage.go
package usage

// MetricKey 集中定义所有可能的扩展维度，杜绝散字符串。
type MetricKey string

const (
    CachedInputTokens   MetricKey = "cached_input_tokens"   // 输入中的缓存命中部分
    CacheCreationTokens MetricKey = "cache_creation_tokens" // 写缓存的 token
    AudioInputSeconds   MetricKey = "audio_input_seconds"
    AudioOutputSeconds  MetricKey = "audio_output_seconds"
    VideoOutputSeconds  MetricKey = "video_output_seconds"
    ImageInputCount     MetricKey = "image_input_count"
    ImageOutputCount    MetricKey = "image_output_count"
    TextCharCount       MetricKey = "text_char_count" // TTS 场景按字符数

    // ... 新增维度在此处加常量；同步本文档第 3.2 节
)
```

> **lint 规则**：CI 中加自定义检查，禁止业务代码用字面量字符串作为 `Details` 的 key；必须引用 `usage.XxxKey` 常量。

## 4. TokenExtractor 接口

```go
// pkg/usage/extractor.go
package usage

import "context"

// Extractor 把上游响应转成 Usage。
// 一个 Extractor 对应一种"上游响应格式"，可被多个 Adapter 复用（如 OpenAI / Azure / DeepSeek 都用 OpenAICompat）。
type Extractor interface {
    Name() string
    NewSession(ctx context.Context, meta Meta) Session
}

// Session 流式 / 非流式统一接口。
//
// 流式：for chunk { Feed(chunk) }；最后 Finalize
// 非流式：Feed(fullBody) 一次；然后 Finalize
type Session interface {
    Feed(chunk []byte) error
    Finalize() (*Usage, error)
}
```

### 4.1 注册表

```go
// pkg/usage/extractor.go
package usage

var registry = map[string]Extractor{}

func Register(name string, e Extractor) {
    registry[name] = e
}

func Get(name string) Extractor {
    return registry[name]
}
```

### 4.2 内置 Extractor 清单（首批）

| Name | 覆盖的厂商 / 协议 |
|------|------------------|
| `openai_compat` | OpenAI / Azure OpenAI / DeepSeek / Mistral / 任何 OpenAI 兼容 SaaS / 自部署 vLLM-OpenAI / Ollama-OpenAI |
| `anthropic` | Anthropic `/v1/messages` 原生 |
| `google_gemini` | Google Gemini `usageMetadata` |
| `aws_bedrock` | AWS Bedrock 二进制 EventStream |
| `text_char_len` | TTS 按字符数 |
| `task_duration` | 视频 / 长音频任务（duration 秒数）|

每个 Extractor 在自己的包里 `init()` 注册；`adapter` 通过 `Adapter.UsageExtractor() string` 声明用哪一个：

```go
// pkg/adapter/openai/adapter.go (示例)
func (*Adapter) UsageExtractor() string { return "openai_compat" }
```

> Adapter 不提供 `UsageExtractor()` 时（默认返回 `""`），Session 的 `Finalize` 返回 `*Usage = nil`，仅做 metric 不入计量管道。

### 4.3 缓存语义归一

约定：**`Usage.Input` 始终包含 cache_read + cache_creation 部分**。

- `openai_compat` Extractor：上游 `prompt_tokens` 已含 cached → 直接用
- `anthropic` Extractor：上游 `cache_read_input_tokens` + `cache_creation_input_tokens` 单列；归一时加到 Input 总和

`Details[CachedInputTokens]` 单独保留 cached 部分供监控 / 计价分档：

```go
usage.Input = upstream.PromptTokens + upstream.CacheReadInput + upstream.CacheCreationInput
usage.Details[CachedInputTokens] = upstream.CacheReadInput
usage.Details[CacheCreationTokens] = upstream.CacheCreationInput
```

> 与 Envoy AI Gateway 一致；下游一套逻辑，对账不会出错。

### 4.4 本地兜底（tokenizer-based fallback）

**问题**：部分上游在以下场景不返回 `usage`：

- 自部署 vLLM / Ollama / TGI 默认配置（未开启 `--enable-usage-tokens` 等）
- 流式响应中途中断（client cancel / 上游 5xx）
- 厂商灰度功能（如新 reasoning 模型早期版本）

此时 `Session.Finalize()` 返回 `*Usage = nil`，事件**不进计量管道**，下游账单缺失。

**对策**：在 Extractor 层提供 **tokenizer fallback**，估算 input / output token，标记 `Source = Estimated`，让计量管道照常消费，账单层自行决定是否计费 / 折扣 / 告警。

**接口扩展**：

```go
// pkg/usage/domain.go
type UsageSource int8
const (
    SourceUpstream  UsageSource = iota // 上游 usage 字段
    SourceEstimated                    // 本地 tokenizer 估算
    SourceMixed                        // input=上游, output=估算（或反之）
)

type Usage struct {
    Input, Output, Total int64
    Source               UsageSource         // 新增
    // ... 其他字段
}
```

**注册新 Extractor**：

```go
// pkg/usage/extractor/tiktoken/extractor.go
package tiktoken

import "github.com/zereker/llm-gateway/pkg/usage"

func init() {
    usage.Register("tiktoken_fallback", &Extractor{
        encoder: pickEncoder,  // 按 model 选 cl100k_base / o200k_base / ...
    })
}
```

**Adapter 声明兜底**（可选第二选择）：

```go
type Adapter interface {
    UsageExtractor() string         // 主：如 "openai_compat"
    UsageFallback() string          // 兜底：如 "tiktoken_fallback"；返回 "" 表示不兜底
}
```

**Session 内部决策**（在 `pkg/usage` 默认 Session wrapper 中实现）：

```
Finalize():
    u, err := primary.Finalize()
    if u != nil && u.Output > 0 {
        return u, err                       // 上游有 usage，照常用
    }
    if fallbackName == "" {
        return u, err                       // 没配兜底，维持现状（*Usage = nil）
    }
    est := fallback.Estimate(messages, completionText)
    if u == nil {
        u = &Usage{Source: SourceEstimated, Input: est.Input, Output: est.Output, ...}
    } else {
        // 上游只有 input，没 output
        u.Output = est.Output
        u.Source = SourceMixed
    }
    return u, err
```

**Tokenizer 选择**（首批支持）：

| 编码 | 适用模型 |
|------|---------|
| `cl100k_base` | GPT-4 / GPT-3.5-turbo / text-embedding-ada-002 |
| `o200k_base`  | GPT-4o / GPT-4o-mini / o1 / o3 |
| `claude` (近似)| Anthropic 模型；官方未开源 tokenizer，用 `cl100k_base` 近似 + 经验系数 |
| `llama3` | Meta Llama 3 / Llama 3.1（开源 BPE）|
| `qwen` | Qwen 2 / Qwen 2.5（开源）|
| `char_div` | 兜底：字符数 ÷ 经验系数（按模型语言），用于无 tokenizer 时不丢日志 |

**实现选项**（按优先级）：

1. **`github.com/pkoukk/tiktoken-go`** — 纯 Go 实现的 tiktoken；启动加载 BPE 表（~1 MB）；约 50 ns/token，单核可达 20M tokens/s
2. **CGo 绑定 `tiktoken-rs`** — 性能更好，但引入 CGo 依赖，破坏交叉编译；**不推荐**
3. **`huggingface/tokenizers` 的 Go 绑定** — 覆盖 Llama / Qwen 等；同样 CGo，按需

> 默认实现选 1；性能敏感场景再讨论。

**精度承诺与对账**：

- 估算结果**不保证位精确**；用于"账单不丢"而非"账单准"
- `Usage.Source = Estimated/Mixed` 是计量管道的**一等字段**：
  - Kafka 消息中包含；下游 Flink 可按 Source 分桶聚合
  - Prometheus metric `usage.extracted_total{source}` 拆维度
  - 价格表可对 `Estimated` 应用折扣 / 上限 / 告警（如 `Estimated` 占比 > 1% 触发告警）

**配置**：

```yaml
# gateway.yaml
usage:
  fallback:
    driver: tiktoken           # tiktoken | charlen | none
    default_encoder: cl100k_base
    encoder_mapping:           # 按模型名 prefix 匹配
      "gpt-4o": o200k_base
      "claude-": claude
      "llama-3": llama3
      "qwen2": qwen
```

> 默认 `driver: none`（保持现状：上游无 usage 则不入计量），需要兜底的部署改为 `tiktoken`。

## 5. PricingSpec

### 5.1 存储

复用 `domain.ModelServiceSnapshot.SpecDetail`（JSON 字段）。**不新增 `pricing_spec` 表**。

```go
// 通过 SpecDetail json.RawMessage 解码得到 PricingSpec
type PricingSpec struct {
    BaseUnit string // "1K_tokens" / "1_second" / "1_image"

    Rates struct {
        Input        float64 // 每 BaseUnit 单价（输入）
        Output       float64
        CachedRead   float64
        CachedWrite  float64
        AudioSecond  float64
        VideoSecond  float64
        ImageCount   float64
        TextChar     float64
        // ... 其他维度按需扩展
    }

    ModelRatio  float64                // 模型整体倍率（默认 1.0）
    GroupRatios map[string]float64     // 按 identity.Group 倍率（"reserved": 0.5 等）
    TieredPrices []TierStop             // 阶梯价（可选）

    Expression string // CEL 表达式；非空则覆盖默认 Calculator
}

type TierStop struct {
    Threshold int64   // input 超过此阈值切换到下一档
    Input     float64
    Output    float64
}
```

### 5.2 版本化（请求时价）

价格随时间会变，账单必须按"请求发生时 T0 的价格"结算。设计如下：

1. **`model_service` 表保持 UPDATE 风格**（继续修改 `SpecDetail`）
2. **新增 `model_service_spec_history` 归档表**：每次改价在**同一事务内**追加一条快照
3. **消息只带版本指纹** `(ModelServiceID, ServiceUpdateTime)`，约 50 字节
4. **流处理器查 history 表**得到当时的价格；本地 LRU cache 降 DB 压力

#### 5.2.1 history 表 DDL

```sql
CREATE TABLE model_service_spec_history (
    id                  BIGINT PRIMARY KEY AUTO_INCREMENT,
    model_service_id    BIGINT       NOT NULL,
    service_update_time TIMESTAMP(6) NOT NULL,
    spec_detail         JSON         NOT NULL,
    archived_at         TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX idx_lookup (model_service_id, service_update_time)
);
```

> **只 INSERT 不 UPDATE 不 DELETE**；任何 `(model_service_id, service_update_time)` 永远能查到原始价格。

#### 5.2.2 改价流程（事务双写）

```go
// 服务层伪代码
func UpdatePricing(ctx context.Context, msID int64, newSpec PricingSpec) error {
    return tx.Run(ctx, func(tx Tx) error {
        if err := tx.UpdateModelServiceSpec(msID, newSpec); err != nil {
            return err
        }
        ms, _ := tx.GetModelService(msID) // 拿到 update_time = NOW()
        return tx.InsertSpecHistory(msID, ms.UpdateTime, newSpec)
    })
}
```

任一失败回滚；`UPDATE model_service` 成功但 `INSERT history` 失败必须重试或告警，否则后续按指纹查 history 会失败。

## 6. 在线管道（应用层）

### 6.1 数据流

```
M7 Schedule:
  callAdapter → response chunks
  session = adapter.NewResponseSession()
  session.Feed(chunk)*
  usage, _, _ = session.Finalize()      // Adapter 内部已委托给 usage.Extractor
  rc.Usage = usage

M10 Tracing (defer):
  // Phase 0：补价格指纹
  rc.Usage.Meta.Pricing = PricingFingerprint{
      ModelServiceID:    rc.ModelService.ID,
      ServiceUpdateTime: rc.ModelService.UpdateTime,
  }

  // Phase 1：本地日志同步 fsync（单一事实源）
  if err := localLog.WriteSync(usageEventBytes); err != nil {
      rc.Logger.Error("usage local log failed", "err", err)
      // 业务已返回 200；告警依赖磁盘监控
      return
  }

  // Phase 2：异步发 Kafka（ack=1，失败不影响业务）
  if err := bus.Publish(ctx, usageEventBytes); err != nil {
      metric.Inc(metric.UsageKafkaFailedTotal)
      // 不阻塞；日志已落盘，旁路 agent 会归档；Kafka 恢复后从冷备回放
  }
```

### 6.2 Kafka 消息 schema（usage-events）

```json
{
  "event_id":      "tr_abc123",
  "request_time":  "2026-05-02T10:23:45Z",
  "user_id":       "alice",
  "api_key_id":    "ak_xxx",
  "service_id":    "svc_gpt4o",
  "model":         "gpt-4o",
  "vendor":        "openai",
  "endpoint_id":   "ep_001",

  "pricing": {
    "model_service_id":    12345,
    "service_update_time": "2026-04-18T09:00:00Z"
  },

  "usage": {
    "input":     1200,
    "output":    450,
    "total":     1650,
    "reasoning": 0,
    "details": {
      "cached_input_tokens":   800,
      "cache_creation_tokens": 0
    }
  },

  "latency": { "ttft_ms": 320, "total_ms": 2800 }
}
```

**幂等键**：`event_id = trace_id`；流处理器 5 分钟窗口去重。

### 6.3 本地日志 + 旁路归档

应用层只负责**写本地日志**：

```go
// pkg/usage/locallog/locallog.go
type LocalLog interface {
    WriteSync(payload []byte) error // append + fsync；失败返回错
}
```

默认实现：`zap` + `lumberjack` ring（按文件大小滚动 + gzip 压缩）。

**归档**由独立的日志收集器（Filebeat / Fluent Bit / Vector / 等）配置完成，应用代码 0 行：

```yaml
# 部署侧示例（Filebeat）
filebeat.inputs:
  - type: filestream
    paths: ["/var/log/llm-gateway/usage-*.log.gz"]
    parsers: [{ ndjson: {} }]
output.s3:    # 或任何 S3 兼容对象存储
  bucket: "llm-gateway-usage-archive"
  prefix: "%{+yyyy/MM/dd/HH}/"
```

### 6.4 EventBus 抽象（[06] 中定义）

应用层不直接依赖 Kafka SDK；通过 `usage.OutboxPublisher` 接口注入：

```go
// pkg/usage/outbox.go
package usage

import "context"

type EventBus interface {
    Publish(ctx context.Context, evt *Event) error
}

type Event struct {
    Payload []byte // 已序列化的 JSON / Protobuf
    Key     string // 分区键，默认 EndpointID
}
```

**默认实现选项**（详见 [06]）：
- `kafka` — 生产推荐；Sarama / kgo 客户端
- `file` — 本地文件流；用于本地开发 / 单实例部署
- `memory` — 仅用于单元测试

## 7. 离线管道（流处理器）

### 7.1 拓扑

```
Source: Kafka usage-events
   ↓
Dedup: keyBy(event_id) + 5 min 窗口
   ↓
Enrich: lookup model_service_spec_history
        by (ModelServiceID, ServiceUpdateTime)
        本地 LRU cache (50k 条, 1h TTL)
        miss 时带全局 rate limit 保护 DB
   ↓
Price: PricingSpec + Usage → per-request cost
   ├─ Expression 非空 → CEL eval
   └─ 否则 → DefaultCalculator
   ↓
Window: tumbling window (1 hour, keyBy=ServiceID)
        allowed lateness 10 min
   ↓
Sink1: Kafka billing-events  → 下游计费系统
Sink2: 明细库（按需独立链路：另一组消费者直接消费 usage-events 写到 ClickHouse / Postgres / 等）
```

> 流处理器选型与代码不在本文档范围；可选 Apache Flink / Apache Beam / 自研 Go 流处理（如基于 sarama + window）。

### 7.2 默认 Calculator

```
cost = (
    Σ_dim usage[dim] * rates[dim] / BaseUnit
) * ModelRatio * GroupRatio
```

dim 遍历 `Input / Output / Reasoning / 各 Details key`。

### 7.3 CEL 表达式

可用变量：`input / output / total / reasoning / details / model / vendor / group / request_time`

例：

```cel
// 按 vendor 分价
vendor == "openai" ? input * 0.005 + output * 0.015
                   : input * 0.003 + output * 0.012

// 阶梯 + 缓存折扣
(input > 100000 ? input * 0.004 : input * 0.005)
+ output * 0.015
+ details.cached_input_tokens * 0.001
```

CEL 引擎统一加载；表达式合法性在 `UpdatePricing` 时通过 `cel.Compile` 校验。

### 7.4 Sink 输出 schema

```json
{
  "event_id":      "tr_abc123",
  "service_id":    "svc_gpt4o",
  "user_id":       "alice",
  "request_time":  "2026-05-02T10:23:45Z",
  "cost":          0.0125,
  "currency":      "USD",
  "formulas": [
    { "key": "input_tokens",  "value": 1200, "unit": "token", "rate": 0.005, "subtotal": 6.0 },
    { "key": "output_tokens", "value": 450,  "unit": "token", "rate": 0.015, "subtotal": 6.75 }
  ],
  "pricing": {
    "model_service_id":    12345,
    "service_update_time": "2026-04-18T09:00:00Z"
  }
}
```

## 8. 价格变更 + 回放

```
T0:  请求 A 发生
     消息带指纹 (id=100, T-10) → 发 Kafka

T1:  改价（事务）：
     UPDATE model_service.spec_detail; UpdateTime=T1
     INSERT history (id=100, T1, 新 spec)

T2:  请求 B 发生
     消息带指纹 (id=100, T1) → 发 Kafka

T3:  流处理器消费：
     A 的指纹 (id=100, T-10) → 查 history → 拿老 spec → 老价
     B 的指纹 (id=100, T1)   → 查 history → 拿新 spec → 新价

T4:  发现 T1 改价数字错了：
     再次 UPDATE + INSERT history (id=100, T4, 修正 spec)
     已结算的 A、B 不变（账单已落地）
     若要回算 T1~T4 期间的错单：从冷备回放消息 → 修改 history 中 T1 版本的 spec → 重跑流处理器
```

**性质**：
- `model_service_spec_history` 只 INSERT，任何指纹永远能查到原始 spec
- 消息重复 / 乱序 / 延迟天然幂等（`event_id` Dedup + 指纹查同条 history）

## 9. 与 OpenAI 兼容客户端的协作

OpenAI SSE 流式默认**不返回** usage 字段；客户端必须在请求 body 里带 `stream_options: {include_usage: true}`。

为避免依赖客户端配合，由 Adapter 在 `TransformRequest` 中**自动注入**（仅当 `NativeProtocol == OpenAI && Stream == true && 客户端未显式声明`）：

```go
body = sjson.SetBytes(body, "stream_options.include_usage", true)
```

详见 [02-protocol-translation](02-protocol-translation.md) 第 6 节 Step 3。

## 10. 可观测性

```
usage.extractor.session_total{extractor, vendor}
usage.extractor.session_duration_ms{extractor, vendor, quantile}
usage.extractor.feed_chunks_total{extractor, vendor}
usage.extractor.usage_extracted_total{extractor, vendor}
usage.extractor.usage_missing_total{extractor, vendor}     # Finalize 返回 nil 的次数

usage.localllog.write_total{result}
usage.localllog.write_duration_ms{quantile}
usage.bus.publish_total{result}                            # result=success/failed
usage.bus.publish_duration_ms{quantile}

pricing.lookup_total{result}                                # result=cache_hit/cache_miss/db_failed
pricing.lookup_cache_size
pricing.calculator_duration_ms{quantile}
```

## 11. 测试矩阵

| # | 场景 | 预期 |
|---|------|-----|
| M1 | OpenAI 流式 + include_usage 自动注入 | Extractor.Finalize 拿到 prompt_tokens / completion_tokens |
| M2 | OpenAI 非流式 | Feed 一次 + Finalize 拿到 usage |
| M3 | Anthropic 流式 cache_read 单列 | Usage.Input 含 cache_read；Details[CachedInputTokens] 单列 |
| M4 | Bedrock 二进制 EventStream | EventStream 解码后 Usage 正确 |
| M5 | Adapter 未实现 UsageExtractor | Finalize 返回 nil，rc.Usage = nil；M10 跳过 Publish |
| M6 | Phase 1 写日志失败 | 业务 200 仍返回；告警 metric |
| M7 | Phase 2 Kafka 失败 | 业务 200；本地日志已写；agent 后续归档 |
| M8 | 改价后历史请求按旧价 | A 用 T-10 价；B 用 T1 价 |
| M9 | history 表查不到指纹 | 流处理器走 DefaultPricing 兜底 + 告警 + DLQ |
| M10 | CEL 表达式合法性 | UpdatePricing 时 `cel.Compile` 校验失败拒绝写入 |
| M11 | LRU cache 命中率 | 100 万请求中 > 99% cache hit |
| M12 | event_id 重复 | 流处理器 Dedup 后只算一次 |

## 12. 演进规则

- **新增 Usage 维度**：在 `domain.MetricKey` 加常量；对应 Extractor 填充；本文档第 3.2 节同步；如需参与计价，`PricingSpec.Rates` 加字段
- **新增 Extractor**：新建 `pkg/usage/<name>/`；`init()` 注册；Adapter 通过 `UsageExtractor()` 声明；本文档第 4.2 节同步
- **修改 Pricing JSON schema**：向后兼容（旧字段保留）；history 表无需迁移（历史快照保持原 schema）
- **修改 Kafka 消息 schema**：版本化 `event_schema_version` 字段；流处理器按版本分支处理
- **修改本地日志 / 归档策略**：本文档第 6.3 节同步；评估 Filebeat 等部署侧配置变更
- **修改 account-status schema / 拦截策略**：本文档第 13 节同步；账单系统侧需协同发版（compacted topic 不能强制清空，schema 必须向后兼容）

## 13. 反向通道：account-status 广播（草案）

> **状态**：v0.1 后扩展，尚未落地。本节定义"账单系统 → 网关"反向控制流的契约：账单系统判定欠费 / 配额超限后，如何让网关 M4 Budget 在秒级拦下后续请求，且**不引入网关到账单系统的同步调用**。

### 13.1 范围与目标

**范围**：从账单系统判定一个账户进入 `suspended`（或恢复 `active`）那一刻起，到网关 M4 Budget 把该账户的请求拒掉为止的整条反向链路。
不含：账单系统内部如何聚合 / 阈值判定（属账单系统自身设计）。

**目标**：

| # | 目标 | 成功判据 |
|---|------|---------|
| B1 | 账户状态变更秒级到达网关 | p99 < 5s（账单聚合周期 + Kafka 端到端延迟）|
| B2 | 网关 M4 不发任何同步 RPC | 每请求零 RPC / Redis RTT；纯本地内存 map 查询 |
| B3 | 网关重启后状态可重建 | 启动期从 compacted topic 拉一份全量快照 |
| B4 | Kafka 故障降级有界 | 长时间断连进入 fail-stale，旧状态继续生效 + metric 告警 |
| B5 | 双向通道职责清晰 | `usage-events`（出，§6.2）与 `account-status`（入，§13.4）两条 topic 互不耦合 |

### 13.2 设计原则

| # | 原则 | 含义 |
|---|------|------|
| BR1 | **Push > Pull** | 网关订阅 topic 拿状态变更，不轮询 admin DB / Redis |
| BR2 | **Compacted topic 即快照** | `cleanup.policy=compact`；topic 自身就是当前所有 key 的最新状态，无需额外 snapshot API |
| BR3 | **每实例独立 consumer group** | `gateway-budget-{instance_id}`；每实例必须看到全量事件，禁止瓜分 partition |
| BR4 | **Fail-open + readiness gate** | warm-up 期 `/readyz` 返回 503；K8s 不切流量；不会因为没拿到状态就拒服务 |
| BR5 | **运行期 fail-stale** | Kafka 断连不清空缓存；用旧状态继续拦截；metric 告警 |
| BR6 | **状态扁平、面向枚举** | `status ∈ {active, suspended, throttled}`；不广播"剩余额度"等细节（属账单系统内部） |
| BR7 | **网关不订阅账单系统内部 topic** | 账单系统对外只承诺 `account-status` 一条 topic 的 schema；其内部聚合 topic 与网关无关 |

### 13.3 与现有数据流的关系

```
   ┌──────────┐  usage-events (§6.2)   ┌─────────┐  billing-events (§7.4)  ┌─────────┐
   │ gateway  │ ─────────────────────► │ stream  │ ──────────────────────► │ billing │
   │  (M10)   │      Kafka             │ proc.   │      Kafka              │ system  │
   └──────────┘                        └─────────┘                         └────┬────┘
        ▲                                                                       │
        │  account-status (§13.4)                                               │
        │  Kafka, compacted                                                     │
        └───────────────────────────────────────────────────────────────────────┘
                  网关 M4 Budget 内嵌 consumer，本地内存判定，零 RPC
```

> **职责切分**：
> - 流处理器（§7）只负责"算 + 压缩流量"，不直接改账户状态
> - 账单系统消费 `billing-events` 后，按业务规则判定 → 发 `account-status` 广播
> - 网关不感知账单系统内部存在；只读 `account-status` 一个契约

### 13.4 account-status Kafka 配置

```
topic:                     account-status
cleanup.policy:            compact
min.cleanable.dirty.ratio: 0.1
segment.ms:                600000        # 10min；让 compaction 较快生效
retention.ms:              -1            # 永久保留 + compaction
partitions:                N             # ≥ 网关实例数；后续按租户量扩
key:                       {scope}:{tenant_id}[:{api_key_id}]
```

**关键**：`cleanup.policy=compact` 让 Kafka 把同一 key 的旧 value 回收，topic 永远只保留每个 key 的最新状态。新启动的网关从 earliest 消费即可拿到当前全量状态。

### 13.5 消息 schema

```json
{
  "tenant_id":    "t_alice",
  "api_key_id":   "ak_xxx",         // 可空；空时表示 tenant 级
  "scope":        "tenant",         // tenant | api_key | model_service
  "status":       "suspended",      // active | suspended | throttled
  "reason":       "monthly_quota_exceeded",
  "version":      42,               // 单调递增；用于幂等 + 乱序检测
  "effective_at": "2026-05-10T08:30:00Z",
  "ttl_sec":      3600              // 可选；suspended 自动过期回 active（账单系统兜底）
}
```

**删除语义**：账单系统**不发** tombstone（null value）；恢复活跃用 `status=active` 显式覆盖。tombstone 在 compaction 后会引起"key 是不存在还是已活跃"的歧义。

**幂等 + 乱序**：网关按 `version` 比较，旧消息丢弃。同一 key 内 version 严格单调由账单系统保证（建议来源：MySQL 自增 ID 或 Snowflake）。

### 13.6 BudgetGate 实现

接口已在 [06] 定义；新增 `kafka` driver：

```go
// pkg/budget/kafka_gate.go
type kafkaGate struct {
    cache       sync.Map      // key="{scope}:{tenant_id}[:{api_key_id}]" -> Status
    ready       atomic.Bool
    lastVersion sync.Map      // 同 key -> 最大已 apply 的 version
}

type Status struct {
    Status      string
    Reason      string
    Version     int64
    EffectiveAt time.Time
}

func (g *kafkaGate) Allow(ctx context.Context, ident *domain.UserIdentity) error {
    if !g.ready.Load() {
        metric.Inc(metric.BudgetWarmupPassthroughTotal)
        return nil // fail-open during warm-up
    }
    if v, ok := g.cache.Load(keyOf(ident)); ok {
        st := v.(Status)
        if st.Status == StatusSuspended {
            return domain.NewErrPermanent("account_suspended", st.Reason)
        }
    }
    return nil
}

func (g *kafkaGate) Ready() bool { return g.ready.Load() }
```

后台 goroutine 持续消费 topic：

```
for msg := range consumer.Messages() {
    apply(msg)               // 写 sync.Map；按 version 拒绝乱序旧消息
    if !g.ready.Load() && consumerLag() == 0 {
        g.ready.Store(true)  // 第一次追上 HWM 时翻牌
    }
}
```

### 13.7 冷启动 / Warm-up

1. 网关启动 → consumer 从 earliest 开始消费
2. `/readyz` 增加 `budgetGate.Ready()` check：与现有 `repo.CheckSchema` 共同决定是否 200
3. consumer 第一次消费到 high watermark → `ready.Store(true)`
4. K8s readiness probe 通过 → 流量切入

**估算**：百万级租户、每条消息 ~200B，compacted 后 topic 大小 ~200MB；千兆网卡满速 < 2s 拉完。绝大多数场景 warm-up < 10s。

**超时兜底**：`warmup_timeout` 配置上限（默认 30s）；超时仍未追上 HWM 也强制 `ready=true` 并告警，避免启动卡死。

### 13.8 运行期降级

| 故障 | 网关行为 | 监控 |
|------|---------|------|
| Kafka broker 短暂不可达（< 30s） | 用本地缓存继续拦截；consumer 自动重连 | `budget_consumer_disconnected_total` |
| 长时间断连（> 5min） | 缓存继续生效（fail-stale）；告警通知运维 | `budget_consumer_stale_seconds` |
| 消息积压 | lag metric 告警；不影响拦截正确性（旧状态仍在） | `budget_consumer_lag_max` |
| version 倒退 / 乱序 | 按 version 比较丢弃旧消息 | `budget_out_of_order_dropped_total` |
| 反复 panic（apply 异常） | 由 M9 Recover 兜底；consumer 重启 | `budget_apply_panic_total` |

### 13.9 与 M4 Budget 的协作

`pkg/middleware/budget.go`（M4）形态不变：调用 `BudgetGate.Allow(ident)`。本节只是新增一个 driver；`cmd/gateway/main.go` 的 `buildBudgetGate` 按 §[06] Pluggable infra 约定加 `case "kafka"` 分支。

```yaml
# configs/local/gateway.yaml
budget:
  driver: kafka            # alwayspass | kafka
  kafka:
    brokers:        ["localhost:9092"]
    topic:          "account-status"
    group_id:       "gateway-budget-${HOSTNAME}"  # 每实例唯一
    warmup_timeout: 30s
    fail_open:      true   # 默认 true；改 false 即 fail-close（不推荐）
```

### 13.10 可观测性

```
budget.consumer.lag_max{topic}
budget.consumer.disconnected_total
budget.consumer.stale_seconds                # 距上次成功消费的秒数
budget.cache.size                            # 当前缓存的 key 总数
budget.warmup.passthrough_total              # warm-up 期间 fail-open 计数
budget.warmup.duration_seconds               # 启动到首次 ready 的耗时
budget.allow.denied_total{reason}            # 拦截计数（含 reason 维度）
budget.out_of_order_dropped_total            # 按 version 丢弃的旧消息
budget.apply_panic_total
```

### 13.11 测试矩阵

| # | 场景 | 预期 |
|---|------|-----|
| B1 | 网关启动期收到请求 | fail-open 放行；passthrough metric +1；`/readyz=503` |
| B2 | warm-up 完成后收到 suspended 租户请求 | 拒绝；返回 `ErrPermanent("account_suspended")` |
| B3 | 同租户 status: active → suspended → active | 第二次请求拒；第三次放行 |
| B4 | version 倒退消息 | 丢弃；`out_of_order` metric +1；缓存不变 |
| B5 | Kafka 短暂断连后重连 | 旧状态继续生效；重连后 lag 追平；无误拦 |
| B6 | Compacted topic 网关重启 | 从 earliest 拉到全量当前状态；warm-up 内完成 |
| B7 | 配置 `driver=alwayspass` | M4 一律放行；不创建 consumer |
| B8 | warmup_timeout 超时 | 强制 ready=true；告警 metric +1；`/readyz` 转 200 |
| B9 | api_key 级 + tenant 级同时存在 | 优先 api_key 级；缺失再回落 tenant 级 |

### 13.12 演进点

- **细粒度状态**：当前只支持 `active/suspended/throttled` 三态。后续如要广播"剩余额度数值"，建议另开 `account-quota` topic，避免冲淡 status 语义
- **反向告警通道**：网关在 BudgetGate 持续被拒后，可选反向往 `account-status` 发 ack 事件，让账单系统知道"拦截已生效"。v1 不做
- **跨集群广播**：单 Kafka 集群即可；多 region 部署时通过 MirrorMaker / Confluent Replicator 同步 `account-status`
