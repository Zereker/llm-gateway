package adapter

import "github.com/zereker-labs/ai-gateway/pkg/domain"

// Classifier 把上游 HTTP 状态 + body 映射到 domain.AdapterError。
//
// Adapter 可实现 Classifier 接管特定厂商的 error schema；未实现走 DefaultClassifier。
//
// 契约：
//   - Classifier 实现 MUST be safe for concurrent use（多 goroutine 同时分类）。
//   - body 入参：实现不可保留 slice 引用 —— 如要存进返回的 AdapterError，必须 string(body) / 拷贝。
type Classifier interface {
	Classify(httpStatus int, body []byte) *domain.AdapterError
}

// DefaultClassifier 仅按 HTTP 状态分类。
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
