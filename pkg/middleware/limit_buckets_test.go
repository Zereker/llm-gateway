package middleware

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// 直接调内部 buildBuckets / appendLayerBuckets，覆盖 key 命名 + additive 展开。

func TestBuildBuckets_BothLayersNil_Empty(t *testing.T) {
	id := &repo.UserIdentity{AccountID: "acc1", APIKeyID: "ak1"}
	deps := LimitDeps{
		Policies: ratelimit.NewPolicyCache(&stubQPProvider{}, time.Minute),
	}
	buckets, tpmKeys, err := buildBuckets(context.Background(), deps, id, "gpt-4o", 100)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(buckets) != 0 || len(tpmKeys) != 0 {
		t.Errorf("expected empty, got buckets=%d tpmKeys=%d", len(buckets), len(tpmKeys))
	}
}

func TestBuildBuckets_KeyNaming_Account(t *testing.T) {
	js, _ := json.Marshal(map[string]any{
		"default": map[string]any{"rpm": 60, "tpm": 100000, "rps": 5},
	})
	pol := &repo.QuotaPolicy{ID: 1, RuleJSON: js}
	pc := ratelimit.NewPolicyCache(
		&stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}},
		time.Minute,
	)
	deps := LimitDeps{Policies: pc}
	id := &repo.UserIdentity{
		AccountID:            "acc1",
		APIKeyID:             "ak1",
		AccountQuotaPolicyID: i64(1),
	}

	buckets, tpmKeys, err := buildBuckets(context.Background(), deps, id, "gpt-4o", 500)
	if err != nil {
		t.Fatalf("err=%v", err)
	}

	want := map[string]bool{
		"rl:quota:account:acc1:*:rpm": false,
		"rl:quota:account:acc1:*:tpm": false,
		"rl:quota:account:acc1:*:rps": false,
	}
	for _, b := range buckets {
		if _, ok := want[b.Key]; ok {
			want[b.Key] = true
		} else {
			t.Errorf("unexpected bucket key: %q", b.Key)
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("missing bucket: %s", k)
		}
	}
	if len(tpmKeys) != 1 || tpmKeys[0] != "rl:quota:account:acc1:*:tpm" {
		t.Errorf("tpmKeys=%+v", tpmKeys)
	}
}

func TestBuildBuckets_PerModel_AdditiveWithDefault(t *testing.T) {
	js, _ := json.Marshal(map[string]any{
		"default":   map[string]any{"rpm": 60},
		"per_model": map[string]any{"gpt-4o": map[string]any{"rpm": 10}},
	})
	pol := &repo.QuotaPolicy{ID: 1, RuleJSON: js}
	pc := ratelimit.NewPolicyCache(
		&stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}},
		time.Minute,
	)
	deps := LimitDeps{Policies: pc}
	id := &repo.UserIdentity{
		AccountID:            "acc1",
		APIKeyID:             "ak1",
		AccountQuotaPolicyID: i64(1),
	}

	buckets, _, _ := buildBuckets(context.Background(), deps, id, "gpt-4o", 100)
	if len(buckets) != 2 {
		t.Fatalf("buckets=%d, want=2 (default + per-model)", len(buckets))
	}
	// 默认 scope "*"，per-model scope "gpt-4o"
	hasDefault := false
	hasPerModel := false
	for _, b := range buckets {
		if strings.Contains(b.Key, ":*:") {
			hasDefault = true
		}
		if strings.Contains(b.Key, ":gpt-4o:") {
			hasPerModel = true
		}
	}
	if !hasDefault || !hasPerModel {
		t.Errorf("default=%v per-model=%v", hasDefault, hasPerModel)
	}
}

func TestBuildBuckets_PerModel_NoMatch_DefaultOnly(t *testing.T) {
	js, _ := json.Marshal(map[string]any{
		"default":   map[string]any{"rpm": 60},
		"per_model": map[string]any{"gpt-4o": map[string]any{"rpm": 10}},
	})
	pol := &repo.QuotaPolicy{ID: 1, RuleJSON: js}
	pc := ratelimit.NewPolicyCache(
		&stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}},
		time.Minute,
	)
	deps := LimitDeps{Policies: pc}
	id := &repo.UserIdentity{AccountID: "acc1", AccountQuotaPolicyID: i64(1)}

	// 请求 claude-3，per_model 没 match → 只命中 default
	buckets, _, _ := buildBuckets(context.Background(), deps, id, "claude-3", 100)
	if len(buckets) != 1 {
		t.Fatalf("buckets=%d, want=1 (default only)", len(buckets))
	}
}

func TestBuildBuckets_TPMCost_Uses_PassedCost(t *testing.T) {
	js, _ := json.Marshal(map[string]any{"default": map[string]any{"tpm": 100000}})
	pol := &repo.QuotaPolicy{ID: 1, RuleJSON: js}
	pc := ratelimit.NewPolicyCache(
		&stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}},
		time.Minute,
	)
	deps := LimitDeps{Policies: pc}
	id := &repo.UserIdentity{AccountID: "acc1", AccountQuotaPolicyID: i64(1)}

	buckets, _, _ := buildBuckets(context.Background(), deps, id, "x", 555)
	if len(buckets) != 1 {
		t.Fatalf("buckets=%d", len(buckets))
	}
	if buckets[0].Cost != 555 {
		t.Errorf("tpm cost=%d, want=555", buckets[0].Cost)
	}
	if buckets[0].Window != time.Minute {
		t.Errorf("window=%v, want=1m", buckets[0].Window)
	}
}

func TestBuildBuckets_BothLayers_NoDoubleCount(t *testing.T) {
	js1, _ := json.Marshal(map[string]any{"default": map[string]any{"rpm": 60}})
	js2, _ := json.Marshal(map[string]any{"default": map[string]any{"rpm": 30}})
	pol1 := &repo.QuotaPolicy{ID: 1, RuleJSON: js1}
	pol2 := &repo.QuotaPolicy{ID: 2, RuleJSON: js2}
	pc := ratelimit.NewPolicyCache(
		&stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol1, 2: pol2}},
		time.Minute,
	)
	deps := LimitDeps{Policies: pc}
	id := &repo.UserIdentity{
		AccountID:            "acc1",
		APIKeyID:             "ak1",
		AccountQuotaPolicyID: i64(1),
		APIKeyQuotaPolicyID:  i64(2),
	}

	buckets, _, _ := buildBuckets(context.Background(), deps, id, "x", 100)
	if len(buckets) != 2 {
		t.Fatalf("buckets=%d, want=2 (account + apikey, additive)", len(buckets))
	}
	scopes := map[string]bool{}
	for _, b := range buckets {
		// account / apikey 必须各一个 bucket
		if strings.Contains(b.Key, ":account:") {
			scopes["account"] = true
		}
		if strings.Contains(b.Key, ":apikey:") {
			scopes["apikey"] = true
		}
	}
	if !scopes["account"] || !scopes["apikey"] {
		t.Errorf("missing layer: %+v", scopes)
	}
}

func TestPickTightestBucket(t *testing.T) {
	if got := pickTightestBucket(nil); got != nil {
		t.Errorf("nil input should return nil, got %+v", got)
	}
	buckets := []ratelimit.Bucket{
		{Key: "a", Limit: 100},
		{Key: "b", Limit: 10},
		{Key: "c", Limit: 50},
	}
	got := pickTightestBucket(buckets)
	if got == nil || got.Key != "b" {
		t.Errorf("tightest=%+v, want key=b", got)
	}
}
