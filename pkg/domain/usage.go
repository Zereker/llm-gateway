package domain

import (
	"encoding/json"
	"time"
)

// Usage 单次请求的资源消耗事件。
//
// 按 docs/architecture/05-metering-billing.md §3 定义；下游计费平台按
// `Raw` + (vendor, model, protocol, request time) 自己定计费规则，
// 网关不维护厂商专有字段枚举。
//
// **Source / Estimator / Confidence**：标识 usage 来源 + 可信度——
// 估算值不会被伪装成上游真实值。
//
//	upstream  / exact       : 上游原生返回了 usage
//	extracted / derived     : translator 从 response 抠出来
//	estimated / approximate : tokenizer 或字符数兜底
//
// **Raw**：上游原始 usage JSON，原样发给计费平台。即使 translator 提取通用
// 字段失败也保留 Raw，下游可按规则解析。
//
// **Truncated**：流式响应中途断开 / 客户端关连接时为 true；下游可按 Confidence
// 决定是否采信本次 usage。
type Usage struct {
	Input     int64 `json:"input"`              // 通用 input token 数；通常等于 prompt + 系统消息（含 cache 部分）
	Output    int64 `json:"output"`             // 通用 output token 数
	Total     int64 `json:"total"`              // 总数；有值时以此为准；无值时 = Input + Output
	Truncated bool  `json:"truncated,omitempty"` // 响应是否未完整完成

	Raw json.RawMessage `json:"raw,omitempty"` // 上游原始 usage 对象（透传给下游计费）

	// Source / Estimator / Confidence — 标识 usage 来源与可信度
	Source     UsageSource     `json:"source,omitempty"`     // upstream | extracted | estimated
	Estimator  UsageEstimator  `json:"estimator,omitempty"`  // tiktoken | naive_chars | vendor_default | ""
	Confidence UsageConfidence `json:"confidence,omitempty"` // exact | derived | approximate

	Meta UsageMeta `json:"meta"`
}

// UsageSource 标识 usage 字段是怎么得到的。
type UsageSource string

const (
	UsageSourceUpstream  UsageSource = "upstream"  // 上游返回了原生 usage
	UsageSourceExtracted UsageSource = "extracted" // translator 解析 response 字段
	UsageSourceEstimated UsageSource = "estimated" // tokenizer / char 估算
)

// UsageEstimator 估算时使用的算法（Source=estimated 时填）。
type UsageEstimator string

const (
	UsageEstimatorNone          UsageEstimator = ""                // 非估算路径
	UsageEstimatorTiktoken      UsageEstimator = "tiktoken"        // OpenAI tiktoken
	UsageEstimatorNaiveChars    UsageEstimator = "naive_chars"     // 按字符数粗估
	UsageEstimatorVendorDefault UsageEstimator = "vendor_default"  // 厂商提供的 tokenizer
)

// UsageConfidence 字段的可信度。
type UsageConfidence string

const (
	UsageConfidenceExact       UsageConfidence = "exact"       // 上游精确数
	UsageConfidenceDerived     UsageConfidence = "derived"     // translator 解析
	UsageConfidenceApproximate UsageConfidence = "approximate" // 估算
)

// UsageMeta 计量事件的元信息，用于计费平台关联身份 / 模型 / 路由 / 请求发生时间。
//
// 字段来源参见 docs/architecture/05-metering-billing.md §4。
type UsageMeta struct {
	AccountID    string `json:"account_id"`                // 主账号 pin / 计费主体（M2 写入）
	Model        string `json:"model"`                     // 实际路由模型；跨 model fallback 时取 RoutedModelService.Model
	Vendor       string `json:"vendor"`                    // endpoint vendor
	EndpointID   string `json:"endpoint_id"`
	SubAccountID string `json:"sub_account_id"`            // 子账户 / 操作者
	APIKeyID     string `json:"api_key_id"`
	ServiceID    string `json:"service_id,omitempty"`      // model_services.service_id（字符串可重命名）

	// ModelServiceID / ServiceUpdateTime —— pricing 查询指纹（docs/05 §4 + docs/09 §6）。
	//
	// 下游 billing aggregator 用 (account_id, model_service_id, service_update_time)
	// 直接命中 pricing_versions 索引，无需经 service_id → id 二跳。
	// 与 Model / ServiceID 一致取自 RoutedModelService（fallback 后实际计费的模型）。
	ModelServiceID    int64     `json:"model_service_id,omitempty"`
	ServiceUpdateTime time.Time `json:"service_update_time,omitempty"`

	RequestID    string    `json:"request_id"`
	TraceID      string    `json:"trace_id,omitempty"`
	StartTime    time.Time `json:"start_time"`
	EndTime      time.Time `json:"end_time"`
	TTFTMs       int64     `json:"ttft_ms,omitempty"`       // 流式响应首字节耗时；非流式为 0
	TotalLatency int64     `json:"total_latency,omitempty"` // 网关端到端 latency，ms
}
