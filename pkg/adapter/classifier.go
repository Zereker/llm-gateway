package adapter

import "github.com/zereker-labs/ai-gateway/pkg/ctx"

// Classifier 把上游 HTTP 状态 + body 映射到 ctx.AdapterError。
//
// Adapter 可实现 Classifier 接管特定厂商的 error schema；未实现走 DefaultClassifier。
type Classifier interface {
	Classify(httpStatus int, body []byte) *ctx.AdapterError
}

// DefaultClassifier 仅按 HTTP 状态分类。
type DefaultClassifier struct{}

// Classify 按 HTTP 状态映射到 ErrorClass。
func (DefaultClassifier) Classify(httpStatus int, body []byte) *ctx.AdapterError {
	e := &ctx.AdapterError{
		HTTPStatus:      httpStatus,
		UpstreamMessage: string(body),
	}
	switch {
	case httpStatus == 429:
		e.Class = ctx.ErrRateLimit
	case httpStatus == 401, httpStatus == 403:
		e.Class = ctx.ErrPermanent
	case httpStatus >= 400 && httpStatus < 500:
		e.Class = ctx.ErrInvalid
	case httpStatus >= 500:
		e.Class = ctx.ErrTransient
	default:
		e.Class = ctx.ErrUnknown
	}
	return e
}
