package gemini

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/domain"
)

// session **slim 版**：只管 HTTP 层（URL + auth header）。
//
// body 翻译 / 响应翻译 / usage 提取全在 pkg/translator/openai_gemini.translator。
//
// auth 三种形态由 newTokenProvider 统一抽象（gemini-key / vertex-adc / oauth2-sa）；
// session 不知道具体类型。
//
// **流式**：streaming=true（客户端发 stream:true，由 Factory 从原始 body 判定）时，
// URL 换成 :streamGenerateContent?alt=sse，上游返 Gemini SSE 流；shape 翻译（Gemini
// SSE chunk → OpenAI SSE chunk）在 openai_gemini responseHandler 里做。
type session struct {
	ctx       context.Context
	ep        *domain.Endpoint
	tp        tokenProvider
	streaming bool

	closed bool
}

func newSession(c context.Context, ep *domain.Endpoint, tp tokenProvider, streaming bool) *session {
	return &session{ctx: c, ep: ep, tp: tp, streaming: streaming}
}

// geminiStreamURL 流式时把 :generateContent 端点换成 :streamGenerateContent?alt=sse。
//   - alt=sse 让 Gemini 返 SSE（data: {json}\n\n）而非默认的 JSON 数组分帧。
//   - 已是 :streamGenerateContent 端点 → 只补 alt=sse。
//   - 保留 base 上已有的 query（如 ?key=...）。
//   - 非标准 URL（找不到 :generateContent）→ 原样（deployer 可能已直接给流式端点）。
func geminiStreamURL(base string) string {
	if strings.Contains(base, ":streamGenerateContent") {
		return ensureAltSSE(base)
	}
	if i := strings.LastIndex(base, ":generateContent"); i >= 0 {
		return ensureAltSSE(base[:i] + ":streamGenerateContent" + base[i+len(":generateContent"):])
	}
	return base
}

func ensureAltSSE(u string) string {
	if strings.Contains(u, "alt=sse") {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "alt=sse"
}

// BuildRequest 构造 *http.Request：
//   - URL: ep.Routing.URL（约定填完整 :generateContent 端点）；流式时改写成
//     :streamGenerateContent?alt=sse。
//   - 加 vendor-specific auth header（x-goog-api-key 或 Authorization: Bearer）
//   - body: translator 已翻好（OpenAI ChatCompletion → Gemini generateContent）
func (s *session) BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("gemini: ep.routing.url empty")
	}
	url := s.ep.Routing.URL
	if s.streaming {
		url = geminiStreamURL(url)
	}
	req, err := http.NewRequestWithContext(s.ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// 先 quirks header
	for k, vs := range extraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// 再协议必需 header（覆盖）
	req.Header.Set("Content-Type", "application/json")

	hdrName, hdrValue, err := s.tp.AuthHeader(s.ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set(hdrName, hdrValue)

	return req, nil
}

func (s *session) Close() error {
	s.closed = true
	return nil
}

// 编译期断言。
var _ protocol.Session = (*session)(nil)
