// Package usage 定义计量数据总线 Usage 与扩展维度 MetricKey。
//
// 所有下游消费者（限流 / 计价 / 明细）共享同一份 Usage；
// 详见 docs/architecture/05-metering-billing.md。
package usage

import (
	"time"

	"github.com/zereker-labs/ai-gateway/internal/modelservice"
)

// MetricKey 集中定义所有可能的扩展维度，杜绝散字符串。
type MetricKey string

const (
	CachedInputTokens   MetricKey = "cached_input_tokens"
	CacheCreationTokens MetricKey = "cache_creation_tokens"
	AudioInputSeconds   MetricKey = "audio_input_seconds"
	AudioOutputSeconds  MetricKey = "audio_output_seconds"
	VideoOutputSeconds  MetricKey = "video_output_seconds"
	ImageInputCount     MetricKey = "image_input_count"
	ImageOutputCount    MetricKey = "image_output_count"
	TextCharCount       MetricKey = "text_char_count"
)

// Usage 单次请求的资源消耗快照。
type Usage struct {
	// 主字段（语义公约）
	Input     int64 // 输入 token 数；约定包含所有 cache 相关部分
	Output    int64 // 输出 token 数
	Total     int64 // 总数；有值时以此为准；无值时 = Input + Output + Reasoning
	Reasoning int64 // 推理 token（OpenAI o-系列、Gemini thoughts、DeepSeek reasoning_content）

	// 扩展维度（按需填充；Key 集中定义见上方常量）
	Details map[MetricKey]int64

	// 元信息
	Meta Meta
}

// Meta 计量事件的元信息。
type Meta struct {
	Model        string
	Vendor       string
	EndpointID   string
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
	Pricing modelservice.PricingSnapshot
}
