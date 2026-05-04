package adapter

import "github.com/zereker-labs/ai-gateway/pkg/domain"

// Classifier 把上游 HTTP 状态 + body 映射到 domain.AdapterError。
//
// **可选接口**：Factory 可以同时实现 Classifier 接管特定厂商的 error schema；
// M7 在 dispatch 时按 type assertion 检查 — 实现了就用，没实现走 HTTP-only 分类。
//
// **典型用途**：HTTP status 单维度不够细分时
//   - 同样的 429：OpenAI 区分 `insufficient_quota`（permanent）vs 真 rate-limit（capacity）
//   - 200 + 错误 body：少数厂商把错误塞 200 响应里，HTTP-only 看不出来
//   - 5xx 细分：Anthropic 的 529 overloaded_error 应当走 capacity 而非 transient
//
// **没实现 Classifier 的 Factory**：M7 直接用 HTTP-status → ErrorClass 兜底；
// 不报错，只是分类粒度粗一些。
//
// 契约：
//   - Classifier 实现 MUST be safe for concurrent use（多 goroutine 同时分类）。
//   - body 入参：实现不可保留 slice 引用 —— 如要存进返回的 AdapterError，必须 string(body) / 拷贝。
//   - body 可能是部分（M7 limit-read 1KiB），实现要 tolerant of truncated JSON。
type Classifier interface {
	Classify(httpStatus int, body []byte) *domain.AdapterError
}

// DefaultClassifier 仅按 HTTP 状态分类。Adapter 不实现 Classifier 时的 fallback。
type DefaultClassifier struct{}

// Classify 按 HTTP 状态映射到 ErrorClass。
func (DefaultClassifier) Classify(httpStatus int, body []byte) *domain.AdapterError {
	e := &domain.AdapterError{
		HTTPStatus:      httpStatus,
		UpstreamMessage: string(body),
	}
	switch {
	case httpStatus == 429:
		e.Class = domain.ErrRateLimit
	case httpStatus == 401, httpStatus == 403:
		e.Class = domain.ErrPermanent
	case httpStatus >= 400 && httpStatus < 500:
		e.Class = domain.ErrInvalid
	case httpStatus >= 500:
		e.Class = domain.ErrTransient
	default:
		e.Class = domain.ErrUnknown
	}
	return e
}
