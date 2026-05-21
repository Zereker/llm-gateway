package openai

import (
	"encoding/json"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/domain"
)

// Classify 实现 adapter.Classifier，覆盖 DefaultClassifier 给 OpenAI 协议族细化分类。
//
// **OpenAI error JSON shape**：
//
//	{ "error": { "type": "...", "code": "...", "message": "...", "param": null } }
//
// **细分规则**（HTTP-status 之外的判断）：
//   - 429 + code="insufficient_quota"  → ErrPermanent（账户额度耗尽，长 cooldown；不该按
//     瞬时 rate-limit 几秒后重试）
//   - 429 + code="rate_limit_exceeded" → ErrRateLimit（默认行为，但显式标）
//   - 400 + code="context_length_exceeded" → ErrInvalid（客户端请求过长，换 ep 也没用）
//   - 401 + type="invalid_api_key"     → ErrPermanent（默认 401 已经是 Permanent，但
//     把 UpstreamMessage 填准）
//   - 其他：走 DefaultClassifier 的 status-only 分类
//
// **body 解析失败时**（不合法 JSON / 截断）：fallback 到 DefaultClassifier，不报错。
func (Factory) Classify(httpStatus int, body []byte) *domain.AdapterError {
	base := adapter.DefaultClassifier{}.Classify(httpStatus, body)

	if len(body) == 0 {
		return base
	}
	var probe struct {
		Error *struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Error == nil {
		return base
	}
	// 把上游原始 message 透到 base（覆盖 DefaultClassifier 的 string(body) 全文）
	if probe.Error.Message != "" {
		base.UpstreamMessage = probe.Error.Message
	}

	switch httpStatus {
	case 429:
		switch probe.Error.Code {
		case "insufficient_quota":
			base.Class = domain.ErrPermanent
		case "rate_limit_exceeded", "":
			base.Class = domain.ErrRateLimit
		}
	case 400:
		if probe.Error.Code == "context_length_exceeded" {
			base.Class = domain.ErrInvalid
		}
	}
	return base
}

// 编译期断言 Factory 实现 adapter.Classifier。
var _ adapter.Classifier = Factory{}
