package usage

import (
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// SchemaVersionV1 当前 Usage Event schema 版本（docs/05 §5 + docs/08 §5）。
const SchemaVersionV1 = "usage.v1"

// UsageEvent Kafka topic `billing.usage.recorded.v1` 的 envelope（命名按领域.实体.事件.版本，跟生产者解耦；详见 docs/05 §5）。
//
// JSON 形态参见 docs/08 §5；partition key 由调用方决定（推荐 account_id）。
//
// 字段：
//   - schema_version: 用于向后兼容；break change 切新 topic
//   - event_id:       唯一事件 ID（ULID / random hex）
//   - request_id / trace_id: 便利字段；权威值在 Usage.Meta
//   - usage:          domain.Usage（含 Meta）
//   - created_at:     outbox 入队时间；请求发生时间在 usage.meta.start_time
type UsageEvent struct {
	SchemaVersion string       `json:"schema_version"`
	EventID       string       `json:"event_id"`
	RequestID     string       `json:"request_id"`
	TraceID       string       `json:"trace_id"`
	Usage         domain.Usage `json:"usage"`
	CreatedAt     time.Time    `json:"created_at"`
}
