package anthropic

import (
	"encoding/json"

	"github.com/zereker-labs/ai-gateway/pkg/adapter"
	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Classify 实现 adapter.Classifier，覆盖 DefaultClassifier 给 Anthropic 协议族细化分类。
//
// **Anthropic error JSON shape**（顶层 type=error）：
//
//	{ "type": "error", "error": { "type": "...", "message": "..." } }
//
// **error.type 枚举（Anthropic API 文档）**：
//   - invalid_request_error  → 客户端错（4xx）
//   - authentication_error   → 401（key 无效）
//   - permission_error       → 403
//   - not_found_error        → 404
//   - rate_limit_error       → 429
//   - api_error              → 5xx 上游内部错
//   - overloaded_error       → 529 / 5xx 容量错（应当走 ErrRateLimit/capacity，不该按
//                               transient 短 cooldown，否则会 hammer）
//
// **细分规则**（HTTP-status 之外的判断）：
//   - error.type=overloaded_error  → ErrRateLimit（容量类，cooldown 应较长）
//   - error.type=invalid_request_error → ErrInvalid（即使 status 是 5xx 也按客户端错）
//   - 其他：走 DefaultClassifier
//
// **body 解析失败 / 截断时**：fallback 到 DefaultClassifier。
func (Factory) Classify(httpStatus int, body []byte) *domain.AdapterError {
	base := adapter.DefaultClassifier{}.Classify(httpStatus, body)

	if len(body) == 0 {
		return base
	}
	var probe struct {
		Type  string `json:"type"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return base
	}
	if probe.Error == nil {
		return base
	}
	if probe.Error.Message != "" {
		base.UpstreamMessage = probe.Error.Message
	}

	switch probe.Error.Type {
	case "overloaded_error":
		base.Class = domain.ErrRateLimit
	case "invalid_request_error":
		base.Class = domain.ErrInvalid
	case "authentication_error", "permission_error":
		base.Class = domain.ErrPermanent
	}
	return base
}

// 编译期断言 Factory 实现 adapter.Classifier。
var _ adapter.Classifier = Factory{}
