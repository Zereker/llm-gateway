package quirks

import (
	"errors"
	"testing"
)

func TestGet_UnregisteredVendorReturnsNil(t *testing.T) {
	Reset()
	if r := Get("nope"); r != nil {
		t.Fatalf("Get(unregistered) = %v, want nil", r)
	}
}

func TestRegister_SingleRewriterAppliedOnce(t *testing.T) {
	Reset()
	called := 0
	Register("acme", RewriterFunc(func(body []byte) ([]byte, error) {
		called++
		return append(body, '!'), nil
	}))

	r := Get("acme")
	if r == nil {
		t.Fatal("Get returned nil")
	}
	out, err := r.Rewrite([]byte("hi"))
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if string(out) != "hi!" || called != 1 {
		t.Errorf("out=%q called=%d, want hi! 1", out, called)
	}
}

func TestRegister_MultipleRewritersChain(t *testing.T) {
	Reset()
	Register("acme", RewriterFunc(func(b []byte) ([]byte, error) {
		return append(b, 'a'), nil
	}))
	Register("acme", RewriterFunc(func(b []byte) ([]byte, error) {
		return append(b, 'b'), nil
	}))
	Register("acme", RewriterFunc(func(b []byte) ([]byte, error) {
		return append(b, 'c'), nil
	}))

	out, _ := Get("acme").Rewrite([]byte("x"))
	if string(out) != "xabc" {
		t.Errorf("chain order broken: out=%q", out)
	}
}

func TestChain_ErrorStopsExecution(t *testing.T) {
	Reset()
	boom := errors.New("boom")
	calls := 0
	Register("acme", RewriterFunc(func(b []byte) ([]byte, error) {
		calls++
		return b, boom
	}))
	Register("acme", RewriterFunc(func(b []byte) ([]byte, error) {
		calls++
		return b, nil
	}))

	_, err := Get("acme").Rewrite([]byte("x"))
	if !errors.Is(err, boom) {
		t.Errorf("err=%v, want boom", err)
	}
	if calls != 1 {
		t.Errorf("calls=%d, want 1 (chain should stop on err)", calls)
	}
}

func TestChain_NilElementSkipped(t *testing.T) {
	c := Chain{nil, RewriterFunc(func(b []byte) ([]byte, error) {
		return append(b, '!'), nil
	}), nil}
	out, err := c.Rewrite([]byte("hi"))
	if err != nil || string(out) != "hi!" {
		t.Errorf("out=%q err=%v", out, err)
	}
}

func TestRegister_NilRewriterIgnored(t *testing.T) {
	Reset()
	Register("acme", nil) // 不该 panic 也不该污染 registry
	if r := Get("acme"); r != nil {
		t.Errorf("Get after nil Register = %v, want nil", r)
	}
}
