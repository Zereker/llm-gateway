# 09 — Billing Aggregation（下游消费侧聚合规范）

本文规范**下游 Flink job 如何按时间窗口 group by 消费** [05 §3 Usage Event](./05-metering-billing.md#3-usage-event) + [§5 Usage Outbox](./05-metering-billing.md#5-usage-outbox) 产出的 usage events，产出**按主账号分批、含子账号 × 模型明细行**的账单批次。

> **职责定位**：[05 §6 Pricing](./05-metering-billing.md#6-pricing) 明确"网关不做账单聚合 / 余额扣费 / 在线价格查询 / 金额生成"——本文档是这些下游职责的具体落地规范。 [00 §2 非目标](./00-overview.md#2-非目标) 同步声明"不在 gateway 进程内做账单聚合；gateway 只产出 usage 事件，计价聚合由下游任务完成"。
>
> Usage event 的 Kafka envelope 在 [08 §5](./08-observability.md#5-usage-event) 已定义，本文**只消费、不重述**。

## 1. 范围与目标

**范围**：从 Flink job 订阅 `billing.usage.recorded.v1` 那一刻起，到把"按主账号分批、含子账号 × 模型明细、含 cost 金额"的账单批次写入 sink 为止的整条离线链路。

**不在范围**：网关侧 usage event 产出（[05 §3](./05-metering-billing.md#3-usage-event) ~ [§5](./05-metering-billing.md#5-usage-outbox)）；账单系统的对账 / 出账 / 余额扣费逻辑。

**目标**：

| # | 目标 | 成功判据 |
|---|------|---------|
| A1 | **按主账号分批**，一条输出消息 = 一个 `account_id` 在一个时间窗内的账单批次 | 同窗口同主账号永远只有一条输出；幂等键 = `(account_id, window_start)` |
| A2 | 批次内**平铺 `(sub_account_id, model, vendor)` 明细行**，附主账号级 totals | 下游按行做表存储 / 按主账号对总额，两者都不需要二次 group by |
| A3 | **请求时价计价**（point-in-time billing） | 按请求 `end_time` 落入 `pricing_versions.effective_from / effective_to` 区间查 active 版本；改价后历史窗口的 usage 仍按当时价格结算 |
| A4 | **event-time 语义**，按 `usage.meta.end_time` 划窗 | 跨窗口的请求归属到 end_time 所在窗口；late event 走 DLQ 不静默丢 |
| A5 | **价格查询失败不能默认 0 元** | enrich 失败 → `cost=null` + `enrichment_failed=true` + 走 DLQ + alert |
| A6 | **Sink 可扩展**，起步只写日志 | 抽 `BillingBatchSink` 接口；新增 Kafka / HTTP driver 不改 pipeline |
| A7 | **Exactly-once 聚合，at-least-once sink** | Flink checkpoint + Kafka source offset；下游按 `event_id` 去重 |

## 2. 设计原则

继承 [05 §1 设计原则](./05-metering-billing.md#1-三条记录通道)（Usage Event 是计费事实通道、不携带 prompt / completion、必须可补偿），补充本层专有原则：

| # | 原则 | 含义 |
|---|------|------|
| A-P1 | **不修改网关侧任何契约** | Usage event schema 来自 [08 §5](./08-observability.md#5-usage-event)，本层只读；网关侧字段补充另开 PR |
| A-P2 | **聚合纯函数化** | `f(window_events) → batch` 不读时钟、不读 DB；价格查询在独立 enrich 算子 |
| A-P3 | **价格查询有界且可缓存** | JDBC + 本地 LRU cache；cache miss 时全局 rate limit 保护 admin DB |
| A-P4 | **DLQ 优先于静默丢弃** | late event / enrich 失败 / sink 失败统一走 DLQ + alert，业务可补单 |
| A-P5 | **Sink 接口化** | `BillingBatchSink` 接口；driver 通过配置切换，与网关侧 [06 pluggable infra](./06-pluggable-infra.md) 同心智 |
| A-P6 | **找不到价格 ≠ 0 元** | enrichment_failed=true 优先于"假装算出来 0 元"；下游可补单 |

## 3. 工程位置与部署形态

```text
flink/billing-aggregator/
├── pom.xml                                  # 或 build.gradle.kts
├── src/main/java/com/zereker/billing/
│   ├── Main.java                            # Flink job 入口
│   ├── source/UsageEventDeserializer.java   # JSON → POJO
│   ├── domain/UsageEvent.java               # 镜像 docs/08 §5 envelope
│   ├── domain/BillingBatch.java             # 输出 schema POJO
│   ├── agg/LineKey.java                     # (sub_account_id, model, vendor)
│   ├── agg/AggregateFunction.java           # Flink AggregateFunction 实现
│   ├── pricing/PricingResolver.java         # JDBC + Caffeine LRU
│   ├── pricing/CostCalculator.java          # 纯函数：(qty, rule) → cost
│   └── sink/BillingBatchSink.java           # 接口 + LogSink / KafkaSink
├── src/main/resources/
│   ├── application.yaml                     # 见 §10
│   └── log4j2.xml
└── src/test/...
```

- 独立 Flink 集群部署（Java 17 + Flink 1.20+）；不与 gateway / admin 同进程
- 提交方式：`flink run -d billing-aggregator.jar --config application.yaml`
- 与 gateway / admin 的运维耦合点只有两个：消费 Kafka topic + JDBC 连 admin MySQL

## 4. 输入契约

**Topic**：`billing.usage.recorded.v1`（[08 §5](./08-observability.md#5-usage-event) 定义；DLQ topic `billing.usage.recorded.v1.dlq` 不在本 job 消费范围）

**Envelope**：完整 schema 见 [08 §5](./08-observability.md#5-usage-event) 与 [05 §5](./05-metering-billing.md#5-usage-outbox)。本节只声明 job 读取的字段：

| JSON 路径 | 用途 | 缺失处理 |
|---|---|---|
| `event_id` | 幂等键 | 必填；缺失走 DLQ |
| `usage.meta.account_id` | **keyBy 分组键**；批次主键 | 必填；缺失走 DLQ |
| `usage.meta.sub_account_id` | 明细行维度 | 可空；空时填 `"_default"` |
| `usage.meta.model` | 明细行维度 | 可空；空时填 `"_unknown"` |
| `usage.meta.vendor` | 明细行维度 | 可空；空时填 `"_unknown"` |
| `usage.meta.service_id` | 审计 + service_id rename 兜底（§6.2 fallback） | 可空 |
| `usage.meta.model_service_id` | **pricing 查询主键**（§6.1） | 必填；缺失走 §6.2 fallback；fallback 也失败 → 该 line `enrichment_failed=true` |
| `usage.meta.service_update_time` | 审计 / 当时 model_service 版本快照 | 可空（v1 enrich 不强依赖） |
| `usage.meta.end_time` | **event time timestamp + pricing 查询时点** | 必填；缺失走 DLQ |
| `usage.input` / `usage.output` / `usage.total` | 累加量 | 缺失按 0 处理 |
| `usage.confidence` | 透传（不参与聚合） | — |
| `usage.raw` | 透传到 line 的 `raw_passthrough`（默认丢弃以控批次体积；配置可开） | — |

**字段语义** 严格对齐 [05 §4 Usage Meta](./05-metering-billing.md#4-usage-meta) 与 `pkg/domain/usage.go` 的 `UsageMeta`。`model_service_id` 与 `service_update_time` 在 M5 拿到 ModelService 之后写入（M10 `fillUsageMeta` 与 `Model / ServiceID` 一道按"routed 优先"拷贝；详见 [05 §4](./05-metering-billing.md#4-usage-meta) 末段）。

## 5. 拓扑

```text
Kafka Source (billing.usage.recorded.v1)
    │  group_id=billing-aggregator
    │  start_offset=earliest（首次） / 后续从 checkpoint
    ▼
Deserialize + Validate
    │  缺必填字段 → DLQ
    ▼
Assign Watermarks
    │  ts = usage.meta.end_time
    │  watermark = max(ts) - bounded_out_of_orderness(默认 1min)
    ▼
keyBy(account_id)
    ▼
Tumbling EventTime Window
    │  size = §10 配置（默认 1h）
    │  allowed_lateness = §10 配置（默认 10min）
    │  side output for late events → DLQ sink
    ▼
Aggregate(LineKey → LineAggregate)
    │  LineKey = (sub_account_id, model, vendor)
    │  LineAggregate { requests, input_tokens, output_tokens, total_tokens }
    │  纯累加；不查 DB
    ▼
Enrich Pricing
    │  per-line lookup: (account_id, service_id, end_time) → PricingRule
    │  Caffeine LRU cache 50k / 1h TTL
    │  miss + DB 失败 → 标记 line.enrichment_failed=true，cost=null
    ▼
Calculate Cost
    │  per-line: cost = CostCalculator.apply(LineAggregate, PricingRule)
    │  account totals = Σ lines.cost / requests / tokens
    ▼
Emit BillingBatch
    │  event_id = "agg_" + accountId + "_" + windowStart(ISO8601 basic)
    ▼
Sink (driver-switched)
    │  log | kafka | http   （默认 log）
    │  失败 → DLQ sink
    └── DLQ sink （late events + enrich 失败 + emit 失败）
```

**关键设计点**：
- `keyBy = account_id`：直接对应"按主账号分批"的业务诉求
- 不做 dedup 阶段：Flink Kafka source + checkpoint 已保证 exactly-once 消费；网关 outbox 已做 event_id 唯一化，无需额外 5min dedup 窗口
- 价格查询独立成 enrich 算子（A-P2 聚合纯函数化）；可单独限流、单独 retry

## 6. 价格查询（PricingResolver）

[05 §6 Pricing](./05-metering-billing.md#6-pricing) 已声明"下游计费平台根据 usage event 中的请求发生时间去匹配当时生效的价格版本"。本节是 Flink job 内的具体实现。

### 6.1 数据源

直连 admin 侧 MySQL（与 gateway 同库），表 `pricing_versions`（DDL 见 `pkg/infra/schema.sql`）。SQL：

```sql
SELECT id, rule_json, effective_from, effective_to
FROM pricing_versions
WHERE account_id        = ?
  AND model_service_id  = ?
  AND rule_class        = ?
  AND effective_from   <= ?
  AND (effective_to IS NULL OR effective_to > ?)
ORDER BY effective_from DESC
LIMIT 1;
```

参数：
- `account_id` ← `usage.meta.account_id`
- `model_service_id` ← `usage.meta.model_service_id`（M5 已拍下，直接用；缺失时走 §6.2 fallback）
- `rule_class` ← `application.yaml` 配置，默认 `"standard"`（与 `pkg/repo/pricing.go` 一致）
- `t` ← `usage.meta.end_time`（point-in-time billing）

SQL 直接命中 `pricing_versions.idx_active_lookup(account_id, model_service_id, rule_class, effective_from)`。

> `usage.meta.service_update_time` 在 v1 enrich 中**不**作为主参数（pricing_versions 是独立改价表，effective_from 与 model_services.updated_at 不一一对应）；保留作为**审计字段**与未来"网关侧拍 pricing fingerprint"演进的预留位。

### 6.2 Fallback：缺失 `model_service_id` 时的兼容路径

`usage.meta.model_service_id` 在 [05 §4](./05-metering-billing.md#4-usage-meta) 落地后已是必填，但本 job 仍要兼容历史 event（schema 升级前的旧消息）。fallback 二跳：

```sql
SELECT id FROM model_services WHERE service_id = ? AND status = 'enabled' LIMIT 1;
```

- 启动期一次性加载全表 → broadcast state
- 每 10min 增量刷新（model_services 变更频率低，不需要 sub-second 同步）
- 命中 → 用查到的 id 走 §6.1
- 不命中 → line `enrichment_failed=true`、走 DLQ

> 上游 schema 长期稳定后（即历史 event 都已消费完），本 fallback 路径可移除；本 job 启动期通过 `aggregator.pricing.fallback_lookup_total` metric 观察使用频率。

### 6.3 缓存策略

- Caffeine LRU：max_size=50000、TTL=1h
- 缓存键：`(account_id, model_service_id, rule_class, end_time_truncated_to_hour)`
- miss 时：通过 `Semaphore` 限并发到 admin DB（默认 16，可配）
- DB 调用失败 retry 3 次（指数退避），仍失败 → `enrichment_failed=true`

### 6.4 失败语义

| 场景 | 行为 |
|---|---|
| `service_id` 在 `model_services` 找不到 | line.cost=null, enrichment_failed=true, batch 仍 emit, 同时复制一份到 DLQ |
| `pricing_versions` 无 active 版本 | 同上 |
| JDBC connection failure（>3 次重试） | 同上；alert `aggregator.pricing.db_failed_total` |
| `rule_json` 解析失败 | 同上；alert `aggregator.pricing.rule_parse_failed_total` |

**禁止**：把 `cost` 默认为 0、把找不到价格的 line 静默丢弃。原则 A-P6：找不到价格 ≠ 0 元——离线场景翻译为"DLQ + alert，由人工 / 后续补单解决"，绝不能伪装成"正常 0 元入账"。

### 6.5 rule_json 形态

`pricing_versions.rule_json` 由 admin 侧定义、产品 / 运营维护。完整 schema（v1）：

```json
{
  "version":   1,
  "currency":  "CNY",
  "base_unit": "1K_tokens",
  "rates": {
    "input_tokens":          0.036,
    "output_tokens":         0.180,
    "cache_read_tokens":     0.0036,
    "cache_5m_write_tokens": 0.045,
    "cache_1h_write_tokens": 0.072
  },
  "model_ratio": 1.0,
  "tiered_prices": [
    { "threshold_dim": "input_tokens", "threshold": 272000,
      "rates": { "input_tokens": 0.072, "output_tokens": 0.360 } }
  ]
}
```

字段语义：

| 字段 | 含义 |
|---|---|
| `base_unit` | `1K_tokens` / `1M_tokens` / `1_request` / `1_image` / `1_second` |
| `rates` | metric_key（与 [§15.3 MetricKeys 字典](#153-metrickeys-字典) 对齐）→ 单价 |
| `model_ratio` | 可选；整体倍率（默认 1.0） |
| `tiered_prices` | 可选；按某 dimension 阶梯切换 rates；首个 `threshold_dim` 超过 `threshold` 的 tier 替换 base rates |

**Calculator 算法**（纯函数）：

```
rates = pickTieredOrBase(rule, agg.dimensions)
cost  = Σ (agg.dimensions[k] * rates[k] / baseUnitDivisor)
cost *= rule.model_ratio
```

`agg.dimensions` 的 key 全部由 §15 Nacos 侧 ExtractorSpec 决定，pricing 侧只需声明对应 rate 即可——**rule.rates 多出的 key 不报错（容错），dimension 没有 rate 的也不算钱（cost 不贡献，alerts 留给 metric 监控）**。

## 7. 输出 Schema（BillingBatch）

```json
{
  "schema_version":  "billing-batch.v1",
  "event_id":        "agg_acct_abc_20260517T090000Z",
  "window_start":    "2026-05-17T09:00:00Z",
  "window_end":      "2026-05-17T10:00:00Z",
  "account_id":      "acct_abc",
  "currency":        "CNY",
  "totals": {
    "requests": 183,
    "dimensions": {
      "input_tokens":         10000,
      "output_tokens":        15000,
      "cache_read_tokens":    40000,
      "cache_5m_write_tokens": 2000
    },
    "cost": 3.294
  },
  "lines": [
    {
      "sub_account_id": "sub_001",
      "model":          "claude-opus-4-7",
      "vendor":         "anthropic",
      "service_id":     "svc_claude_opus_4_7",
      "requests":       30,
      "dimensions": {
        "input_tokens":         10000,
        "output_tokens":        15000,
        "cache_read_tokens":    40000,
        "cache_5m_write_tokens": 2000,
        "cache_1h_write_tokens":    0
      },
      "cost":             3.294,
      "rule_class":       "standard",
      "enrichment_failed": false
    }
  ],
  "stats": {
    "events_consumed":     183,
    "events_late_dropped": 0,
    "lines_enrich_failed": 0
  },
  "generated_at": "2026-05-17T10:00:12Z"
}
```

**Schema 公约**：所有数值维度统一塞进 `dimensions` map（key 来自 [§15.3](#153-metrickeys-字典)）。`requests` 是顶层单独字段（语义上不是"消耗维度"而是"事件数"）。`cost` 在 line 和 totals 两层都暴露——line 级是单 (sub_account, model, vendor) 维度的金额，totals 是窗口主账号总额。

**演进**：新增维度只改 §15 Nacos 配置 + admin 侧 rule_json 加 rate 即可，**`BillingBatch` 输出 schema 不变**（dimensions map 自动多个 key）。

**字段语义**：

| 字段 | 含义 |
|---|---|
| `schema_version` | 当前固定 `billing-batch.v1`；break change 切新 sink topic / 文件路径 |
| `event_id` | **幂等键**：`"agg_" + account_id + "_" + window_start(ISO8601 basic, UTC)`；下游按此去重 |
| `window_start` / `window_end` | UTC ISO-8601；左闭右开 |
| `account_id` | 等于 keyBy 键 |
| `currency` | 取自第一条 line 的 `rule_json.currency`；同窗口 line 货币不一致时 batch.currency=`"MIXED"` + enrichment_failed=true + alert |
| `totals.cost` | `Σ lines[*].cost`（null 累加按 0；但 `stats.lines_enrich_failed > 0` 即 batch 进 DLQ 复本） |
| `lines[].cost` | null ⇔ enrichment_failed=true；下游必须能处理 null |
| `stats` | 透明度字段；不强校验 |
| `generated_at` | sink emit 时刻（processing time） |

**约束**：
- `lines` 数组按 `(sub_account_id, model, vendor)` 字典序排序，确保同窗口同 account 二次 emit 字节级一致（exactly-once + idempotent sink）
- `lines` 数组不去重——同一 `(sub, model, vendor)` 全窗口期内只可能有一条（AggregateFunction 保证）
- 总条目数无上限；超大主账号（百万子账号 × 多模型）注意 sink 单消息大小限制

## 8. 时间语义

| 项 | 取值 |
|---|---|
| Event time 字段 | `usage.meta.end_time` |
| Bounded out-of-orderness | 默认 1min（配置项 `window.out_of_orderness`） |
| Window type | tumbling，event time |
| Window size | 默认 1h（配置项 `window.size`；支持 5m / 15m / 1h / 1d） |
| Window 时区 | UTC（避免 DST 边界 bug）；下游按需 reproject |
| Allowed lateness | 默认 10min（配置项 `window.allowed_lateness`） |
| Late event 去向 | side output → DLQ sink；带原 event 全文 + `dropped_reason="late"` |

**为什么用 EndTime 不用 StartTime**：
1. usage 数字本身只有 Finalize（end_time）那一刻才完整；start_time 阶段还在变
2. 业内出账惯例（AWS / Azure / OpenAI）都按完成时刻入账
3. Outbox publish 时刻 ≈ end_time，watermark 推进自然；用 start_time 会让长会话产生大量"看起来 late"的事件

**Tradeoff**：跨窗口的长请求（如 09:55 发起 / 10:05 结束）整笔归 10:00 那个窗口，不切分。LLM 请求绝大多数秒级 / 分钟级，影响很小；若后续大量小时级长任务（长视频生成），考虑按时长摊分（另开 PR 讨论）。

## 9. Sink 抽象

```java
public interface BillingBatchSink extends Serializable, AutoCloseable {
    String name();
    void emit(BillingBatch batch) throws Exception;
}
```

**起步实现**：

| Driver | 用途 | 实现 |
|---|---|---|
| `log` | 默认；本地开发 + 生产起步 | log4j2 JSON 滚动文件 `/var/log/billing-aggregator/batches.jsonl`，按大小滚动 + gzip；同时 stdout（容器友好） |
| `kafka` | 预留；下游订阅 | topic `billing.account.aggregated.v1`，partition key = `account_id`，envelope 直接是 §7 schema |
| `http` | 预留；推账单系统 | POST `{base_url}/billing/batches`，header `X-Idempotency-Key: {event_id}`；非 2xx → DLQ |
| `dlq` | late event / enrich 失败 / sink 失败的兜底 | 同 `log` 但路径独立 `/var/log/billing-aggregator/dlq.jsonl` |

**驱动选择**：`application.yaml` 的 `sink.driver` 字段切换，与 gateway 侧 [06 pluggable infra](./06-pluggable-infra.md) 同心智——driver 字符串切换，主 pipeline 不感知具体实现。

**禁止**：sink 内部做业务判断（如"金额 < 0.01 不发"）；过滤逻辑全部在上游聚合阶段做完。

## 10. 配置

```yaml
# flink/billing-aggregator/src/main/resources/application.yaml
job:
  name: billing-aggregator
  parallelism: 4
  checkpoint:
    interval: 60s
    mode: EXACTLY_ONCE
    timeout: 5m

source:
  type: kafka
  bootstrap_servers: ["kafka-broker-0:9092", "kafka-broker-1:9092"]
  topic: billing.usage.recorded.v1
  group_id: billing-aggregator
  start_offset: earliest          # 首次启动；后续从 checkpoint
  properties:
    enable.auto.commit: false     # Flink 自己管 offset

window:
  size: 1h                        # 5m | 15m | 1h | 1d
  allowed_lateness: 10m
  out_of_orderness: 1m

pricing:
  jdbc:
    url:      "jdbc:mysql://admin-db:3306/llm_gateway?useUnicode=true&characterEncoding=utf8mb4"
    username: "${PRICING_DB_USER}"
    password: "${PRICING_DB_PASSWORD}"
    pool:
      max_size: 16
      connection_timeout: 5s
  rule_class: standard
  cache:
    max_size: 50000
    ttl: 1h
  retry:
    max_attempts: 3
    backoff_initial: 100ms
    backoff_max: 2s

sink:
  driver: log                     # log | kafka | http
  log:
    path: /var/log/billing-aggregator/batches.jsonl
    rotate_size: 100MB
    compress_rotated: true
  kafka:                          # 仅 driver=kafka 生效
    bootstrap_servers: ["kafka-broker-0:9092"]
    topic: billing.account.aggregated.v1
  http:                           # 仅 driver=http 生效
    base_url: "https://billing.internal/api/v1"
    timeout: 10s

dlq:
  driver: log                     # 与 sink 独立配置
  log:
    path: /var/log/billing-aggregator/dlq.jsonl

observability:
  metrics:
    reporter: prometheus          # Flink 原生支持
    port: 9249
```

**配置原则**（对齐 [07 配置规范](./07-configuration.md)）：
- 凭证通过环境变量注入，不写明文
- 任何 `driver` 字段都允许配置未识别 driver → job 启动期 fail-fast
- window.size 改动需要触发"窗口对齐重启"（drain + 新窗口）

## 11. 幂等与一致性

| 阶段 | 保证 | 机制 |
|---|---|---|
| Source → Window | exactly-once | Flink Kafka source + checkpoint barrier |
| Window → Enrich | exactly-once | keyed state 持久化到 checkpoint |
| Enrich → Sink | at-least-once | sink 自身一般非事务（log / kafka non-tx / http） |
| 下游去重 | 由消费方负责 | 按 `event_id`（即 `agg_{account_id}_{window_start}`）作主键 |

**幂等细节**：
- 同窗口同主账号永远只有一个 `event_id`（拼接结构决定）
- Flink restart from checkpoint 可能重发同一 `event_id` 的 batch；payload **字节级一致**（lines 排序、totals 由 lines 推导）
- 下游不去重则会重复入账；推荐下游建唯一索引 `UNIQUE(event_id)`

## 12. 可观测性

**Metrics**（Prometheus + Flink 原生）：

```text
aggregator.source.records_consumed_total{topic}
aggregator.source.records_invalid_total{reason}      # 缺必填字段
aggregator.source.lag_max{topic, partition}

aggregator.window.events_per_window{account_id}      # histogram
aggregator.window.late_events_total
aggregator.window.batches_emitted_total{account_id}

aggregator.pricing.lookup_total{result}              # cache_hit | cache_miss | db_failed | rule_not_found
aggregator.pricing.lookup_duration_ms{quantile}
aggregator.pricing.cache_size
aggregator.pricing.db_failed_total

aggregator.cost.calc_duration_ms{quantile}
aggregator.cost.lines_enrich_failed_total

aggregator.sink.emit_total{driver, result}           # success | retry | dlq
aggregator.sink.emit_duration_ms{driver, quantile}
aggregator.dlq.records_total{reason}                 # late | enrich_failed | sink_failed
```

**告警建议**（与 [08 §8](./08-observability.md#8-告警建议) 同心智）：

| 告警 | 触发条件 | 严重度 |
|---|---|---|
| usage 消费延迟 | `aggregator.source.lag_max > 60000` 持续 5min | P2 |
| 价格查询全失败 | `aggregator.pricing.db_failed_total` 1min > 100 | P1 |
| DLQ 异常增长 | `aggregator.dlq.records_total` 1h > 1000 | P2 |
| 批次 emit 失败 | `aggregator.sink.emit_total{result="dlq"}` > 0 持续 5min | P1 |
| Checkpoint 失败 | Flink 原生 `flink_jobmanager_job_lastCheckpointAlignmentBuffered` | P2 |

## 13. 测试矩阵

| # | 场景 | 预期 |
|---|------|------|
| A1 | 单 account 单 sub_account 单 model，窗口内 N 次请求 | 一条 batch；totals = lines[0]；event_id 稳定 |
| A2 | 单 account × 多 sub_account × 多 model | lines 按字典序；totals = Σ lines |
| A3 | 跨窗口请求（end_time 落在窗口边界） | 归属 end_time 所在窗口；下一窗口不重复计入 |
| A4 | late event（end_time 早于 watermark - allowed_lateness） | 进 DLQ；不影响已 emit 的 batch |
| A5 | 重复 event_id 的 usage event | Flink + outbox 保证唯一；本 job 不做 dedup |
| A6 | 价格 enrichment 失败 | line.cost=null + enrichment_failed=true；batch 仍 emit；同时复制一份到 DLQ |
| A7 | LRU cache 命中率 | 100 万请求 > 99% cache hit |
| A8 | watermark 推进与窗口 firing | bounded out-of-orderness 1min；窗口在 watermark > window_end + allowed_lateness 时 fire |
| A9 | sink 失败（如 disk full） | 重试 3 次 → DLQ；不丢数据 |
| A10 | Flink restart from checkpoint | 同一 (account, window) 重发的 batch 字节级一致；下游按 event_id 去重无副作用 |
| A11 | service_id 在 model_services 找不到 | line 标 enrichment_failed；不抛 exception |
| A12 | window.size 配置变更（1h → 15m） | 重启 job + 重置 group_id；旧窗口 drain，新窗口对齐到 quarter hour |
| A13 | 货币不一致（同 account 同窗口 lines 出现 USD + CNY） | batch.currency=`MIXED` + enrichment_failed=true + DLQ |
| A14 | 主账号一个窗口内 0 请求 | 不 emit 空 batch |

## 14. 与 [05](./05-metering-billing.md) / [08](./08-observability.md) 的关系

| 维度 | docs/05 | docs/08 | docs/09（本文档） |
|---|---|---|---|
| 在线计量产出 | §3 Usage Event / §4 Usage Meta / §5 Usage Outbox | §5 envelope schema | 只消费引用 |
| Kafka envelope schema | §5（接口与 driver） | **§5（线上 payload）** | 只消费 |
| 离线聚合工程 | §6 声明"网关不做" | — | **§5 拓扑 / §7 schema** |
| 价格存储 | — | — | §6 消费 `pricing_versions` |
| 价格查询 | §6 声明"按请求发生时间匹配" | — | §6 JDBC + Caffeine 落地 |
| 单条请求计价 | §6 声明"网关不做金额生成" | — | §6 PricingResolver + CostCalculator |
| 聚合输出 schema | §6 声明"网关不做账单聚合" | — | **§7 per-window-per-account 批次** |
| 可观测性 | §7 Metrics / Trace | §1~§4 + §8 | §12 |
| Sink 抽象 | §5 OutboxPublisher（网关侧） | — | §9 BillingBatchSink（消费侧）|

**升级路径**：
- 历史 event 消费完毕后：§6.2 fallback 二跳可移除，主路径完全依赖 `usage.meta.model_service_id`
- 若 admin 侧 `pricing_versions` rule schema 演进（如加 cache_read 单价 / 阶梯价 / 倍率）：`CostCalculator` 内分支处理，pipeline 不变
- 若网关侧引入 pricing fingerprint（在 M5 直接拍下 active pricing 的 effective_from 写入 Meta）：本 job §6.1 SQL 改为按指纹精确查 history，`service_update_time` 升级为主参数
- 若网关侧增加 Modality / Reasoning 维度的 usage 字段：`LineKey` 不变；`LineAggregate` 加累加字段；`schema_version` 升 `billing-batch.v2`

## 15. Nacos extractor spec（数据驱动 metering）

### 15.1 职责切分

```
              ┌────────────────────┐
              │   admin (MySQL)    │   产品 / 运营定价
              │  pricing_versions  │   metric_key → 单价
              └─────────┬──────────┘
                        │ JDBC (§6.1)
                        ▼
                  ┌──────────┐
       Nacos ───> │  Flink   │   ← 本项目；
       extractor  │   job    │     代码层面零 vendor 字眼，
       specs ────>│          │     只跑通用 SpEL + 累加 + Calculator
                  └────┬─────┘
                       ▲
                       │ Kafka
                       │
                ┌───────────┐
                │  gateway  │   usage events（含 raw 透传）
                └───────────┘
```

**两份配置职责互补**：

| 维度 | admin / `pricing_versions` | Nacos / `extractor-*.yaml` |
|---|---|---|
| 谁维护 | 产品 / 运营 | 运营 / 接 vendor 的工程同学 |
| 内容 | 每个 metric_key 的单价（CNY/USD/...） | 怎么从 vendor 原始 usage 解析出每个 metric_key 的数值 |
| 修改触发 | 产品改价 | 接新 vendor 或 vendor schema 变更 |
| 数据流 | JDBC 查 active 版本 + Caffeine cache | 启动期拉 + Nacos listener 推 |
| 历史化 | append-only，按 end_time 落区间 | Nacos 自身版本历史 |
| 关联键 | `(account_id, model_service_id, rule_class)` | `meta.vendor`（默认） |

### 15.2 ExtractorSpec YAML schema

每个 Nacos dataId 一份 spec，格式：

```yaml
# Nacos dataId: extractor-anthropic.yaml
name: anthropic                 # 与 dataId 去掉 extractor- 前缀 + .yaml 后缀一致
metrics:
  input_tokens:
    expr: "#path(#root, 'input_tokens') ?: 0"
  output_tokens:
    expr: "#path(#root, 'output_tokens') ?: 0"
  cache_read_tokens:
    expr: "#path(#root, 'cache_read_input_tokens') ?: 0"
  cache_5m_write_tokens:
    expr: "#path(#root, 'cache_creation.ephemeral_5m_input_tokens') ?: 0"
  cache_1h_write_tokens:
    expr: "#path(#root, 'cache_creation.ephemeral_1h_input_tokens') ?: 0"
```

**表达式语法**：Spring SpEL + 一个 helper function `#path(root, dotted)`：

| 元素 | 说明 |
|---|---|
| `#root` | 当前事件的 `usage.raw` 经 Jackson 转成的 `Map<String, Object>` |
| `#root['key']` | SpEL 内置 map indexer；找不到抛 NPE，必须包 `?:` 兜底 |
| `#path(#root, 'a.b.c')` | helper 函数，逐层 get，任何一层缺失返回 null |
| `?:` | SpEL Elvis 运算符：`a ?: b` = `a != null ? a : b` |
| `+ - * /` | 标准算术 |
| 三元 `cond ? a : b` | 标准 |

**禁止**：
- 直接调 vendor SDK method（spec 是数据，不是代码）
- 引用 Java 类型 `T(...)`（StandardEvaluationContext 当前未限制，但**约定不用**）
- I/O / 反射 / 进程操作

### 15.3 MetricKeys 字典

extractor / pricing 两侧都引用同一份 key 字典（`pkg/extractor/MetricKeys.java`）：

```
input_tokens                 cached_input_tokens          cache_read_tokens
cache_5m_write_tokens        cache_1h_write_tokens        output_tokens
reasoning_tokens             audio_input_seconds          audio_output_seconds
image_input_count            image_output_count           text_char_count
requests
```

**演进**：新增维度改 `MetricKeys.java` 加常量 → Nacos spec 加 metric → admin rule_json 加 rate。Calculator / 聚合算子完全不动。

### 15.4 vendor → spec 映射

默认：spec 名 = `meta.vendor` 字符串。

若不一致（例：Azure OpenAI 与 OpenAI 共享响应格式但 vendor 字段是 `"azure"`），在 `application.yaml` 配 mapping：

```yaml
extractor:
  mapping:
    azure:    openai_responses
    deepseek: openai_responses
```

仍然 zero-code change：加新 vendor 只动 YAML 配置。

### 15.5 热更新与失败语义

| 场景 | 行为 |
|---|---|
| 启动期 Nacos 不可达 | 启动 fail-fast；不让 job 起来在空 registry 上跑（避免 silent zero-cost） |
| 运行期 spec 推送变更 | Nacos listener 重新编译 + 原子替换；编译失败保留旧 spec + WARN |
| 运行期 Nacos 短暂断连 | 用本地 cache 的最后一份 spec 继续；Nacos 自动重连恢复 |
| 某 vendor 没注册 spec | event 仍流过；`dimensions` 为 null；下游 EnrichFn 标 `enrichment_failed=true` → DLQ |
| spec 表达式 eval 抛异常 | 该 metric 取 0；WARN log + metric 计数；其他 metrics 正常 |

### 15.6 Claude Opus 4.7 端到端算账（worked example）

**产品定价**（admin `pricing_versions.rule_json`，CNY / 千 tokens）：

```json
{
  "version":  1,
  "currency": "CNY",
  "base_unit": "1K_tokens",
  "rates": {
    "input_tokens":          0.036,
    "output_tokens":         0.180,
    "cache_read_tokens":     0.0036,
    "cache_5m_write_tokens": 0.045,
    "cache_1h_write_tokens": 0.072
  }
}
```

**Nacos extractor spec**（`extractor-anthropic.yaml`，见 §15.2）

**Anthropic 上游 raw 响应**（`usage.raw`）：

```json
{
  "input_tokens": 10000,
  "output_tokens": 15000,
  "cache_read_input_tokens": 40000,
  "cache_creation": {
    "ephemeral_5m_input_tokens": 2000,
    "ephemeral_1h_input_tokens": 0
  }
}
```

**算账过程**：

```
ExtractMetricsFn  → dimensions {
    input_tokens:          10000,
    output_tokens:         15000,
    cache_read_tokens:     40000,
    cache_5m_write_tokens: 2000,
    cache_1h_write_tokens: 0
}

CostCalculator  (base_unit=1K_tokens, divisor=1000):
    10000  * 0.036  / 1000 = 0.36
    15000  * 0.180  / 1000 = 2.70
    40000  * 0.0036 / 1000 = 0.144
     2000  * 0.045  / 1000 = 0.09
        0  * 0.072  / 1000 = 0
                             -------
    cost                   = 3.294 CNY
```

`CostCalculatorTest.claude_opus_4_7_five_dimensions` 单测验证此算式（`flink/billing-aggregator/src/test/java/com/zereker/billing/pricing/CostCalculatorTest.java`）。

## 16. 演进规则

- **新增 metric 维度**（如视频时长 / 图片张数 / 推理 token 分价）：1) `MetricKeys.java` 加常量 → 2) Nacos spec 加 metric expr → 3) admin `rule_json.rates` 加单价。三处都是配置 / 字典，不改算子。
- **新增聚合维度**（如按 endpoint_id / api_key_id 切 line）：在 `LineKey` 加字段；schema_version 升级到 `billing-batch.v2`；新 topic 或新 sink 路径；旧消费方按 v1 继续
- **新增 Sink driver**：实现 `BillingBatchSink` 接口；`application.yaml` 注册；不改 pipeline 主路径
- **新增 vendor**：1) Nacos 加 `extractor-<vendor>.yaml` → 2) admin 给该 vendor 的 model_service 配 pricing_versions。**完全不动代码**。
- **修改 window.size 默认值**：本文档第 §10 同步；告警阈值同步
- **修改输出 schema**：向后兼容追加（新增字段下游默认忽略）；break change 必须切 schema_version 并经历双写
- **新增 pricing rule_class**：`application.yaml` 加配置；rule_json schema 由 [05 §6](./05-metering-billing.md#6-pricing) + admin 侧管控，本 job 透传
- **网关侧扩 UsageMeta 字段**：本 job 优先消费新字段，缺失时按本文档 §6 fallback；不破坏 v1 兼容
