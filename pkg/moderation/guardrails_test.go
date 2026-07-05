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
	// matches
	if err := g.CheckInput(context.Background(), env(`{"messages":[{"content":"my SSN is here"}]}`)); !errors.Is(err, ErrDenied) {
		t.Errorf("matching an SSN should give ErrDenied, got %v", err)
	}
	if err := g.CheckInput(context.Background(), env(`{"content":"123-45-6789"}`)); !errors.Is(err, ErrDenied) {
		t.Errorf("matching an SSN number should give ErrDenied, got %v", err)
	}
	// clean
	if err := g.CheckInput(context.Background(), env(`{"content":"hello world"}`)); err != nil {
		t.Errorf("clean input should pass, got %v", err)
	}
	// error must not leak the pattern (only reveal that a guard exists)
	if err := g.CheckInput(context.Background(), env(`{"content":"SSN"}`)); err != nil && strings.Contains(err.Error(), "ssn") {
		t.Errorf("error string should not contain the pattern: %v", err)
	}
}

func TestDenylistGuard_OutputGatedByFlag(t *testing.T) {
	off, _ := NewDenylistGuard([]string{`secret`}, false)
	if err := off.CheckOutput(context.Background(), []byte("this is secret")); err != nil {
		t.Errorf("check_output=false should not scan the output, got %v", err)
	}
	on, _ := NewDenylistGuard([]string{`secret`}, true)
	if err := on.CheckOutput(context.Background(), []byte("this is secret")); !errors.Is(err, ErrDenied) {
		t.Errorf("check_output=true match should give ErrDenied, got %v", err)
	}
}

func TestDenylistGuard_BadPatternFailsFast(t *testing.T) {
	if _, err := NewDenylistGuard([]string{`(unclosed`}, false); err == nil {
		t.Error("a bad regex should return an error (fail-fast at startup)")
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
		t.Errorf("should be blocked by the pii guard with its name attached, got %v", err)
	}
	if !errors.Is(err, blockErr) {
		t.Errorf("should wrap the original error, got %v", err)
	}
}

func TestChain_AllPass(t *testing.T) {
	chain := NewChain(
		NamedGuard{Name: "a", Guard: stubGuard{}},
		NamedGuard{Name: "b", Guard: stubGuard{}},
	)
	if err := chain.CheckInput(context.Background(), env(`{}`)); err != nil {
		t.Errorf("all guards passing should give nil, got %v", err)
	}
	if err := chain.CheckOutput(context.Background(), []byte("x")); err != nil {
		t.Errorf("output all passing should give nil, got %v", err)
	}
}

func TestChain_OutputAttribution(t *testing.T) {
	chain := NewChain(
		NamedGuard{Name: "clean", Guard: stubGuard{}},
		NamedGuard{Name: "denylist", Guard: stubGuard{outErr: ErrDenied}},
	)
	err := chain.CheckOutput(context.Background(), []byte("bad"))
	if err == nil || !strings.HasPrefix(err.Error(), "denylist:") {
		t.Errorf("output should be blocked by the denylist, got %v", err)
	}
}

// Chain / DenylistGuard both satisfy Moderator (can be plugged directly into M8).
func TestGuardsAreModerators(t *testing.T) {
	var _ Moderator = (*Chain)(nil)
	var _ Moderator = (*DenylistGuard)(nil)
}
