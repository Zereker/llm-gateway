package middleware

// 客户端可用的 X-Gateway-* 请求头常量。
//
// 这些 header 让客户端按请求覆盖 gateway 默认行为：
//
//	X-Gateway-Timeout:           per-request 超时（duration string，如 "30s"）；只能比 cfg.timeout 更严
//	X-Gateway-Max-Attempts:      M7 跨 endpoint 重试上限（int）；只能比 cfg.scheduler.max_attempts 更小
//	X-Gateway-Fallback-Models:   L3 跨模型降级序列（逗号分隔 model 名）；当前 model 全部 endpoints
//	                             跑完都失败时按列表顺序换 model 重 try。空 = L3 关闭。
//	X-Gateway-Session:           会话亲和 key；同 session 的请求粘到同一上游 endpoint
//	                             （prefix/KV cache 命中）。需 scoring/affinity 开启才生效；空 = 不粘。
//
// 所有 header 解析失败时静默 fallback 到 cfg 默认；不让畸形 header 阻断请求。
//
// 命名约定：所有 gateway 自定义 header 都用 X-Gateway-* 前缀，跟 vendor / 客户端 header 区分。
const (
	HeaderGatewayTimeout        = "X-Gateway-Timeout"
	HeaderGatewayMaxAttempts    = "X-Gateway-Max-Attempts"
	HeaderGatewayFallbackModels = "X-Gateway-Fallback-Models"
	HeaderGatewaySession        = "X-Gateway-Session"
	// X-Gateway-Cache（请求）：off = 本次不查/不写缓存；on = 强制缓存（即使 temperature≠0，
	// 客户端自负非确定性风险）。缺省：仅缓存非流式 + temperature=0 的确定性请求。
	// X-Gateway-Cache（响应）：hit = 本次响应来自缓存。
	HeaderGatewayCache = "X-Gateway-Cache"
)
