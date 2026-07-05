package usage

import (
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// SchemaVersionV1 is the current Usage Event schema version (docs/05 §5 + docs/08 §5).
const SchemaVersionV1 = "usage.v1"

// UsageEvent is the envelope for the Kafka topic `billing.usage.recorded.v1`
// (named as domain.entity.event.version, decoupled from the producer; see
// docs/05 §5 for details).
//
// See docs/08 §5 for the JSON shape; the partition key is chosen by the
// caller (account_id is recommended).
//
// Fields:
//   - schema_version: used for backward compatibility; a breaking change switches to a new topic
//   - event_id:       unique event ID (ULID / random hex)
//   - usage:          domain.Usage (includes Meta; request_id / trace_id live inside Meta)
//   - created_at:     time the event was enqueued into the outbox; the time the request
//     actually happened is in usage.meta.start_time
type UsageEvent struct {
	SchemaVersion string       `json:"schema_version"`
	EventID       string       `json:"event_id"`
	Usage         domain.Usage `json:"usage"`
	CreatedAt     time.Time    `json:"created_at"`
}
