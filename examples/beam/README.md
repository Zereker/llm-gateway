# examples/beam — usage outbox 流处理示例

把 llm-gateway 的 usage outbox（Kafka topic `llm-gateway.usage`）接到 Apache Beam 做计费聚合的参考骨架。

## 架构

```
llm-gateway (M10 Tracing)
    ↓ JSON event
Kafka [llm-gateway.usage]
    ↓
Beam pipeline (本示例)
    ↓
Sink (text / PG / BigQuery)
```

每个 usage event 是 `UsageEvent` JSON（pkg/usage/outbox.go 写出）：

```json
{
  "request_id": "req_abc123",
  "account_id":  "demo-acme",
  "sub_account_id":    "alice",
  "vendor":     "openai",
  "model":      "gpt-4o",
  "endpoint_id":"openai-prod-1",
  "input":      120,
  "output":     58,
  "total":      178,
  "cost":       0.00088,
  "start_time_unix_ms": 1709876543210
}
```

## DAG（usage_pipeline.go）

1. **Source** — Kafka consumer
2. **Parse** — JSON → `UsageEvent`
3. **Dedup** — by request_id（防 retry 双发）— 示例占位
4. **Enrich** — join pricing snapshot — 示例占位
5. **Window** — 1-min tumbling
6. **Aggregate** — sum cost by (account, model)
7. **Sink** — text file（生产改 PG / BigQuery / Postgres）

## 依赖

文件顶部有 `//go:build beam` build tag，**默认不参与主 module 编译**——避免拉一堆 Beam transitive deps 进主项目（仅 build tag `beam` 启用时才编译）。

跑这个示例有两种方式：

**方式 A — 独立 module**（推荐）：

```bash
cd examples/beam
go mod init beam-pipeline
go get github.com/apache/beam/sdks/v2/go/...
go run -tags beam ./usage_pipeline.go --kafka.brokers=localhost:9092 --output=/tmp/cost
```

**方式 B — 主 module 加 deps**（不推荐；污染主 go.mod）：

```bash
go get github.com/apache/beam/sdks/v2/go/...
go run -tags beam ./examples/beam/usage_pipeline.go ...
```

或 Dataflow runner（GCP）：

```bash
go run ./usage_pipeline.go \
  --runner=dataflow \
  --project=my-gcp-project \
  --region=us-central1 \
  --staging_location=gs://my-bucket/staging \
  --kafka.brokers=10.0.0.1:9092
```

## 生产改造点

| 项 | 示例里 | 生产应做 |
|---|---|---|
| Dedup | passthrough | stateful DoFn + 窗口内 set 去重 |
| Enrich pricing | passthrough | SideInput 加载 PG `pricing_versions` 表，按 (account, model, time) 查当时单价 |
| Window | 隐式（SumPerKey 全量） | `beam.WindowInto(window.NewFixedWindows(1 * time.Minute))` |
| Sink | textio | jdbcio (PG) / bigqueryio (BQ) |
| DLQ | silent drop | Tag.Output 写错误事件到 DLQ topic |
| Schema 演进 | hard-coded struct | 用 protobuf / Avro + Schema Registry |

## 替代方案

不喜欢 Beam？可以用：

- **Flink**（Java/Scala，Kafka native）
- **Spark Structured Streaming**（Scala/Python）
- **Materialize / RisingWave**（SQL streaming）
- **简单 Go consumer**（自己写 group consumer + window 聚合，最少依赖）

llm-gateway 不绑定任何流处理器；Kafka topic schema 是合约。
