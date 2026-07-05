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
