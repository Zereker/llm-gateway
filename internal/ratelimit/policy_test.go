package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

type policySourceStub struct {
	raw   map[int64][]byte
	err   error
	calls int
}

func (s *policySourceStub) RuleJSONByID(_ context.Context, id int64) ([]byte, error) {
	s.calls++
	return s.raw[id], s.err
}

func TestPolicyCacheParsesAndCachesAdditiveRules(t *testing.T) {
	s := &policySourceStub{raw: map[int64][]byte{1: []byte(`{
		"default":{"rpm":100},"per_model":{"gpt-4o":{"tpm":2000}}
	}`)}}
	cache := NewPolicyCache(s, time.Minute)
	rule, err := cache.Get(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	rules := rule.PickRulesAdditive("gpt-4o")
	if len(rules) != 2 || rules[0].Scope != "*" || rules[1].Scope != "gpt-4o" {
		t.Fatalf("rules = %#v", rules)
	}
	if _, err := cache.Get(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if s.calls != 1 {
		t.Fatalf("source calls = %d, want 1", s.calls)
	}
}

func TestPolicyCacheRejectsMalformedJSON(t *testing.T) {
	s := &policySourceStub{raw: map[int64][]byte{1: []byte(`{"default":`)}}
	if _, err := NewPolicyCache(s, time.Minute).Get(context.Background(), 1); err == nil {
		t.Fatal("want malformed policy error")
	}
}

func TestPolicyCachePropagatesSourceError(t *testing.T) {
	s := &policySourceStub{err: errors.New("database unavailable")}
	if _, err := NewPolicyCache(s, time.Minute).Get(context.Background(), 1); err == nil {
		t.Fatal("want source error")
	}
}
