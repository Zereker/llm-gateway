# 05 — Metering & Billing

本文定义计量与计价层：`domain.Usage` 数据总线、`TokenExtractor` 提取层、`PricingSpec` 版本化定价、在线计量管道（应用层 → Kafka）+ 离线计价聚合（流处理器）的 Kappa 架构。

> **阅读前**：[02](02-protocol-translation.md) 的 `adapter.ResponseSession`；[01](01-request-pipeline.md) 的 M10 Tracing 契约；[06](06-pluggable-infra.md) 的 `usage.OutboxPublisher` / `archive.Store` 抽象接口。

## 1. 范围与目标

**范围**：从 Adapter 拿到上游响应那一刻起，到把"按请求时价格计算的成本"产出为账单事件为止的整条链路。

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
    paths: ["/var/log/ai-gateway/usage-*.log.gz"]
    parsers: [{ ndjson: {} }]
output.s3:    # 或任何 S3 兼容对象存储
  bucket: "ai-gateway-usage-archive"
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
