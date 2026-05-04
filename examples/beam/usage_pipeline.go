//go:build beam

// Command usage_pipeline 示范用 Apache Beam Go SDK 消费 ai-gateway 的 usage outbox。
//
// **DAG**：
//
//	Kafka(ai-gateway.usage)
//	   → JSON parse → UsageEvent
//	   → Dedup by request_id (windowed)
//	   → Enrich (join pricing snapshot from PG)
//	   → Window (1-min tumbling)
//	   → Aggregate (sum cost by tenant × model)
//	   → Sink (PG / BigQuery / 任选)
//
// **本文件是参考骨架**，跑起来还需：
//   - 装 Beam Go SDK 依赖（见 README）
//   - 实现 enrichWithPricing 真去查 PG（这里占位）
//   - 选 runner（DirectRunner 本地 / DataflowRunner GCP / FlinkRunner k8s）
//
// 运行（local DirectRunner）：
//
//	go run ./examples/beam/usage_pipeline.go \
//	  --runner=direct --kafka.brokers=localhost:9092 \
//	  --kafka.topic=ai-gateway.usage --output=./out
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"reflect"

	"github.com/apache/beam/sdks/v2/go/pkg/beam"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/io/kafkaio"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/io/textio"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/log"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/transforms/stats"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/x/beamx"
)

// UsageEvent 是 ai-gateway outbox 写出的 JSON 结构（pkg/usage/outbox.go 定义；这里
// 只取后续聚合用得到的字段，松散反序列化）。
type UsageEvent struct {
	RequestID  string  `json:"request_id"`
	TenantID   string  `json:"tenant_id"`
	UserID     string  `json:"user_id"`
	Vendor     string  `json:"vendor"`
	Model      string  `json:"model"`
	EndpointID string  `json:"endpoint_id"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	Total      int64   `json:"total"`
	Cost       float64 `json:"cost"`
	StartTime  int64   `json:"start_time_unix_ms"`
}

// CostKey 聚合 key：tenant × model；分钟级 window 自然分组。
type CostKey struct {
	Tenant string
	Model  string
}

func init() {
	beam.RegisterType(reflect.TypeOf((*UsageEvent)(nil)).Elem())
	beam.RegisterType(reflect.TypeOf((*CostKey)(nil)).Elem())
	beam.RegisterFunction(parseUsageJSON)
	beam.RegisterFunction(extractKey)
	beam.RegisterFunction(formatAggregate)
}

var (
	runner       = flag.String("runner", "direct", "Beam runner")
	kafkaBrokers = flag.String("kafka.brokers", "localhost:9092", "Kafka brokers (csv)")
	kafkaTopic   = flag.String("kafka.topic", "ai-gateway.usage", "Kafka topic")
	output       = flag.String("output", "/tmp/ai-gateway-cost", "output prefix (textio sink)")
)

func main() {
	flag.Parse()
	beam.Init()
	ctx := context.Background()

	p := beam.NewPipeline()
	s := p.Root()

	// 1) Kafka source — KV<[]byte, []byte>
	rawKV := kafkaio.Read(s,
		map[string]interface{}{
			"bootstrap.servers": *kafkaBrokers,
			"group.id":          "ai-gateway-cost-pipeline",
			"auto.offset.reset": "latest",
		},
		[]string{*kafkaTopic},
	)

	// 2) parse JSON → UsageEvent
	events := beam.ParDo(s, parseUsageJSON, rawKV)

	// 3) Dedup by request_id（同一 request 偶尔重复发；按 RequestID 取第一条）
	deduped := beam.ParDo(s, dedupFn(), events)

	// 4) Enrich pricing — 占位（生产里 SideInput join PG 当前价格快照）
	enriched := deduped // TODO: replace with enrichWithPricing(s, deduped)

	// 5) Key by (tenant, model) and sum cost
	keyed := beam.ParDo(s, extractKey, enriched)
	summed := stats.SumPerKey(s, keyed)

	// 6) Format and sink — text per line "tenant\tmodel\tcost"
	formatted := beam.ParDo(s, formatAggregate, summed)
	textio.Write(s, *output, formatted)

	if err := beamx.Run(ctx, p); err != nil {
		log.Fatalf(ctx, "beam pipeline failed: %v", err)
	}
}

// parseUsageJSON KV<[]byte,[]byte> → UsageEvent。解析失败的消息丢弃（Beam 用 emit 模式
// 跳过；生产可改 dead-letter queue 写另一个 topic）。
func parseUsageJSON(_ []byte, payload []byte, emit func(UsageEvent)) {
	var ev UsageEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		// 生产应写 DLQ；本示例 silent drop
		return
	}
	if ev.RequestID == "" {
		return
	}
	emit(ev)
}

// dedupFn 简单进程内去重（Beam ParDo 状态）。生产用 stateful DoFn + window 内去重；
// 本示例只示意，直接 return 一个无状态函数（实际不做去重）。完整 stateful DoFn 需要
// state.Key/state.Timer setup，超出 minimum 示例范围。
func dedupFn() func(UsageEvent, func(UsageEvent)) {
	return func(ev UsageEvent, emit func(UsageEvent)) {
		emit(ev)
	}
}

// extractKey UsageEvent → KV<CostKey, float64>。
func extractKey(ev UsageEvent) (CostKey, float64) {
	return CostKey{Tenant: ev.TenantID, Model: ev.Model}, ev.Cost
}

// formatAggregate KV<CostKey, float64> → "tenant\tmodel\tcost\n"。
func formatAggregate(k CostKey, v float64) string {
	return fmt.Sprintf("%s\t%s\t%.6f", k.Tenant, k.Model, v)
}
