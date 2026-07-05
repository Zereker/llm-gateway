package moderation

import (
	"context"
	"fmt"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Guardrails 框架：把"单个 Moderator"泛化成"一条 guard 链"。
//
// **关键设计**：Chain 自身实现 Moderator 接口——所以它直接插进现有 M8 Moderation
// 中间件 + WrapStream 输出装饰器,下游**零改动**。加 guard = 往链里塞一个,不动主链路。
//
// 每个 guard 也是一个 Moderator（CheckInput 前置 / CheckOutput 逐 chunk 后置）。

// NamedGuard 一个带名字的 guard；名字用于阻断时归因（哪条 guard 拦的）。
type NamedGuard struct {
	Name  string
	Guard Moderator
}

// Chain 顺序跑每个 guard；任一 CheckInput/CheckOutput 返错即整体 block（错误里带
// guard 名,不带敏感细节——错误会冒到客户端 400 body,见 M8 middleware）。
type Chain struct {
	guards []NamedGuard
}

// NewChain 组装 guard 链。空链 = 永远放行。
func NewChain(guards ...NamedGuard) *Chain {
	return &Chain{guards: guards}
}

// CheckInput 顺序跑每个 guard 的 CheckInput，第一个 block 即返（fail-fast）。
func (c *Chain) CheckInput(ctx context.Context, env *domain.RequestEnvelope) error {
	for _, g := range c.guards {
		if err := g.Guard.CheckInput(ctx, env); err != nil {
			return fmt.Errorf("%s: %w", g.Name, err)
		}
	}
	return nil
}

// CheckOutput 顺序跑每个 guard 的 CheckOutput（逐 chunk）。
func (c *Chain) CheckOutput(ctx context.Context, chunk []byte) error {
	for _, g := range c.guards {
		if err := g.Guard.CheckOutput(ctx, chunk); err != nil {
			return fmt.Errorf("%s: %w", g.Name, err)
		}
	}
	return nil
}

// 编译期断言：Chain 是 Moderator，可直接插进 M8。
var _ Moderator = (*Chain)(nil)
