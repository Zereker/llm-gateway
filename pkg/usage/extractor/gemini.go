package extractor

import (
	"encoding/json"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// NewGemini 构造一个 Gemini 协议 usage Session。
//
// 适用场景（按上游协议匹配）：
//   - openai_gemini：上游 Gemini（OpenAI 客户端 → Gemini 上游，目前 buffer-then-translate）
//
// **Gemini usage shape**（顶层 usageMetadata）：
//
//	{ "candidates": [...],
//	  "usageMetadata": {
//	      "promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15
//	  } }
//
// **v0.5 限制**：只支持非流式 body 模式。Gemini SSE 流式（streamGenerateContent）
// 在 v0.5 不支持，所以这里也不实现 SSE 解析。v0.6 加流式翻译时再补 SSE 路径。
func NewGemini() Session { return &geminiSession{} }

type geminiSession struct {
	buf   []byte
	usage *domain.Usage
}

func (s *geminiSession) Feed(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	s.buf = append(s.buf, chunk...)
}

func (s *geminiSession) Final() *domain.Usage {
	if s.usage != nil {
		return s.usage
	}
	if len(s.buf) == 0 {
		return nil
	}
	var resp struct {
		UsageMetadata *struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
			TotalTokenCount      int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(s.buf, &resp); err != nil {
		return nil
	}
	if resp.UsageMetadata == nil {
		return nil
	}
	s.usage = &domain.Usage{
		Input:  resp.UsageMetadata.PromptTokenCount,
		Output: resp.UsageMetadata.CandidatesTokenCount,
		Total:  resp.UsageMetadata.TotalTokenCount,
	}
	return s.usage
}
