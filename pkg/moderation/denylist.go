package moderation

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// DenylistGuard 基于正则的内容拦截 guard——命中任一 pattern 即 block。
//
// 便宜的确定性护栏(PII 关键字 / 敏感词 / 注入探针等),补在可能很贵的 LLM moderator
// 之前。CheckInput 扫请求 body(env.RawBytes);check_output=true 时也逐 chunk 扫响应。
//
// **不泄漏命中的 pattern**：阻断错误只说"blocked by content policy",避免把 deny
// 规则通过 400 body 暴露给客户端探测(M8 会把错误串拼进响应)。命中细节进 span/log。
//
// **流式输出是 best-effort，不是硬保证**：check_output 逐 chunk 扫的是**已翻译的
// SSE 分帧字节**（data: {...}\n\n），不是解码后的正文。流式里每个 token 各自成一
// 帧，帧与帧之间夹着 JSON/SSE framing，所以跨帧拆分的模式（如正文 "kill" 被切成
// "ki"/"ll" 两帧）扫不出来——即便跨 chunk 缓冲也拼不回连续正文。要**完全阻止**违规
// 输出，必须走非流式（buffer-then-scan）：非流式路径 Flush 一次拿到整个 body，扫的
// 是完整文本，能真正拦下。安全关键的 denylist 应配合非流式使用；流式下它只能拦到
// 落在单帧内的模式。CheckInput（前置、整 body 一次过）不受此限。
type DenylistGuard struct {
	patterns    []*regexp.Regexp
	checkOutput bool
}

// ErrDenied 通用阻断错误(不含命中的 pattern)。
var ErrDenied = errors.New("blocked by content policy")

// NewDenylistGuard 编译 patterns（Go RE2 语法）。任一编译失败即返错（启动 fail-fast）。
func NewDenylistGuard(patterns []string, checkOutput bool) (*DenylistGuard, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("denylist: invalid pattern %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}
	return &DenylistGuard{patterns: compiled, checkOutput: checkOutput}, nil
}

// CheckInput 扫请求 body。
func (g *DenylistGuard) CheckInput(_ context.Context, env *domain.RequestEnvelope) error {
	if env == nil {
		return nil
	}
	return g.scan(env.RawBytes)
}

// CheckOutput 逐 chunk 扫响应（仅 check_output=true 时）。
//
// **流式下 chunk 是单个 SSE 帧的字节**——跨帧拆分的模式扫不出来（见类型文档）。
// 非流式(buffer-then-translate)时 Flush 把整个 body 作为一个 chunk 送进来，扫的是
// 完整正文，才是硬保证。
func (g *DenylistGuard) CheckOutput(_ context.Context, chunk []byte) error {
	if !g.checkOutput {
		return nil
	}
	return g.scan(chunk)
}

func (g *DenylistGuard) scan(b []byte) error {
	for _, re := range g.patterns {
		if re.Match(b) {
			return ErrDenied
		}
	}
	return nil
}

// 编译期断言。
var _ Moderator = (*DenylistGuard)(nil)
