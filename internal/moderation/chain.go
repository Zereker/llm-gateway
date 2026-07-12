package moderation

import (
	"context"
	"fmt"

	"github.com/zereker/llm-gateway/internal/domain"
)

// Guardrails framework: generalizes a "single Moderator" into "a chain of guards".
//
// **Key design**: Chain itself implements the Moderator interface — so it
// plugs directly into the existing M8 Moderation middleware and the
// WrapStream output decorator with **zero changes** downstream. Adding a
// guard just means appending one to the chain; the main path is untouched.
//
// Each guard is itself a Moderator (CheckInput pre-side / CheckOutput per-chunk post-side).

// NamedGuard is a guard with a name; the name is used for attribution when it
// blocks a request (which guard rejected it).
type NamedGuard struct {
	Name  string
	Guard Moderator
}

// Chain runs each guard in order; if any CheckInput/CheckOutput returns an
// error, the whole request is blocked (the error carries the guard's name but
// no sensitive details — it can bubble up into the client's 400 body, see the
// M8 middleware).
type Chain struct {
	guards []NamedGuard
}

// NewChain assembles a guard chain. An empty chain always passes.
func NewChain(guards ...NamedGuard) *Chain {
	return &Chain{guards: guards}
}

// CheckInput runs each guard's CheckInput in order, returning as soon as one blocks (fail-fast).
func (c *Chain) CheckInput(ctx context.Context, env *domain.RequestEnvelope) error {
	for _, g := range c.guards {
		if err := g.Guard.CheckInput(ctx, env); err != nil {
			return fmt.Errorf("%s: %w", g.Name, err)
		}
	}

	return nil
}

// CheckOutput runs each guard's CheckOutput in order (chunk by chunk).
func (c *Chain) CheckOutput(ctx context.Context, chunk []byte) error {
	for _, g := range c.guards {
		if err := g.Guard.CheckOutput(ctx, chunk); err != nil {
			return fmt.Errorf("%s: %w", g.Name, err)
		}
	}

	return nil
}

// Compile-time assertion: Chain is a Moderator and can be plugged directly into M8.
var _ Moderator = (*Chain)(nil)
