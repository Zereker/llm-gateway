package moderation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

func env(body string) *domain.RequestEnvelope {
	return &domain.RequestEnvelope{RawBytes: []byte(body)}
}

// ---- DenylistGuard ----

func TestDenylistGuard_Input(t *testing.T) {
	g, err := NewDenylistGuard([]string{`(?i)\bssn\b`, `\d{3}-\d{2}-\d{4}`}, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// 命中
	if err := g.CheckInput(context.Background(), env(`{"messages":[{"content":"my SSN is here"}]}`)); !errors.Is(err, ErrDenied) {
		t.Errorf("命中 SSN 应 ErrDenied, got %v", err)
	}
	if err := g.CheckInput(context.Background(), env(`{"content":"123-45-6789"}`)); !errors.Is(err, ErrDenied) {
		t.Errorf("命中 SSN 数字应 ErrDenied, got %v", err)
	}
	// 干净
	if err := g.CheckInput(context.Background(), env(`{"content":"hello world"}`)); err != nil {
		t.Errorf("干净输入应放行, got %v", err)
	}
	// 错误不泄漏 pattern（只暴露 guard 存在）
	if err := g.CheckInput(context.Background(), env(`{"content":"SSN"}`)); err != nil && strings.Contains(err.Error(), "ssn") {
		t.Errorf("错误串不该含 pattern: %v", err)
	}
}

func TestDenylistGuard_OutputGatedByFlag(t *testing.T) {
	off, _ := NewDenylistGuard([]string{`secret`}, false)
	if err := off.CheckOutput(context.Background(), []byte("this is secret")); err != nil {
		t.Errorf("check_output=false 应不扫输出, got %v", err)
	}
	on, _ := NewDenylistGuard([]string{`secret`}, true)
	if err := on.CheckOutput(context.Background(), []byte("this is secret")); !errors.Is(err, ErrDenied) {
		t.Errorf("check_output=true 命中应 ErrDenied, got %v", err)
	}
}

func TestDenylistGuard_BadPatternFailsFast(t *testing.T) {
	if _, err := NewDenylistGuard([]string{`(unclosed`}, false); err == nil {
		t.Error("坏正则应返错（启动 fail-fast）")
	}
}

// ---- Chain ----

type stubGuard struct{ inErr, outErr error }

func (g stubGuard) CheckInput(context.Context, *domain.RequestEnvelope) error { return g.inErr }
func (g stubGuard) CheckOutput(context.Context, []byte) error                 { return g.outErr }

func TestChain_InputFailFastWithAttribution(t *testing.T) {
	blockErr := errors.New("blocked")
	chain := NewChain(
		NamedGuard{Name: "pass", Guard: stubGuard{}},
		NamedGuard{Name: "pii", Guard: stubGuard{inErr: blockErr}},
		NamedGuard{Name: "never", Guard: stubGuard{inErr: errors.New("should not reach")}},
	)
	err := chain.CheckInput(context.Background(), env(`{}`))
	if err == nil || !strings.HasPrefix(err.Error(), "pii:") {
		t.Errorf("应被 pii guard 拦并带名字, got %v", err)
	}
	if !errors.Is(err, blockErr) {
		t.Errorf("应 wrap 原始 error, got %v", err)
	}
}

func TestChain_AllPass(t *testing.T) {
	chain := NewChain(
		NamedGuard{Name: "a", Guard: stubGuard{}},
		NamedGuard{Name: "b", Guard: stubGuard{}},
	)
	if err := chain.CheckInput(context.Background(), env(`{}`)); err != nil {
		t.Errorf("全放行应 nil, got %v", err)
	}
	if err := chain.CheckOutput(context.Background(), []byte("x")); err != nil {
		t.Errorf("输出全放行应 nil, got %v", err)
	}
}

func TestChain_OutputAttribution(t *testing.T) {
	chain := NewChain(
		NamedGuard{Name: "clean", Guard: stubGuard{}},
		NamedGuard{Name: "denylist", Guard: stubGuard{outErr: ErrDenied}},
	)
	err := chain.CheckOutput(context.Background(), []byte("bad"))
	if err == nil || !strings.HasPrefix(err.Error(), "denylist:") {
		t.Errorf("输出应被 denylist 拦, got %v", err)
	}
}

// Chain / DenylistGuard 都满足 Moderator（能直接插进 M8）。
func TestGuardsAreModerators(t *testing.T) {
	var _ Moderator = (*Chain)(nil)
	var _ Moderator = (*DenylistGuard)(nil)
}
