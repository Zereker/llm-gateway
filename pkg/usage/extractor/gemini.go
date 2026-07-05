package extractor

import (
	"encoding/json"

	"github.com/zereker/llm-gateway/pkg/domain"
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
// **只解非流式 JSON body**：这是设计选择,不是缺口。Gemini SSE 流式
// （streamGenerateContent）的 usage 由 openai_gemini responseHandler 直接从末帧的
// usageMetadata 抽（见该包 translateChunk），不走本 extractor——所以本 Session 保持
// JSON-only,Final() 把整个 buf 当一个 JSON 解析。JSON 路径才用它。
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
	raw, _ := json.Marshal(resp.UsageMetadata)
	s.usage = &domain.Usage{
		Input:      resp.UsageMetadata.PromptTokenCount,
		Output:     resp.UsageMetadata.CandidatesTokenCount,
		Total:      resp.UsageMetadata.TotalTokenCount,
		Raw:        raw,
		Source:     domain.UsageSourceUpstream,
		Confidence: domain.UsageConfidenceExact,
	}
	return s.usage
}
