package repo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// 负缓存单测不方便直连 SQLAPIKeyProvider（需要 DB），这里直接测
// CachedSubscriptionProvider 的双 TTL 语义 + CachedAPIKeyProvider 的负缓存
// 判断逻辑（用 nil inner 会 panic，所以只测能独立测的部分）。

// TestNegativeTTL_ShorterThanPositive 负缓存 TTL 必须显著短于正缓存默认 30s。
func TestNegativeTTL_ShorterThanPositive(t *testing.T) {
	if negativeTTL >= 30*time.Second {
		t.Fatalf("negativeTTL=%v，应显著短于正向 30s", negativeTTL)
	}
}

// CachedAPIKeyProvider 负缓存：第一次 miss 得到 ErrInvalidCredentials 后，
// 同 key 短期内不再打 inner（用打桩计数验证）。
func TestCachedAPIKeyProvider_NegativeCacheBlocksRepeatLookups(t *testing.T) {
	// 通过组合 TTLCache 直接验证 Resolve 的负缓存分支——构造一个假的
	// CachedAPIKeyProvider（inner 为 nil 时 Resolve 会 panic，所以这里手工搭
	// 等价结构：negative cache 命中则直接短路，不触达 inner）。
	p := &CachedAPIKeyProvider{
		inner:    nil, // 若触达 inner 会 nil panic —— 测试即失败
		cache:    NewTTLCache[string, *UserIdentity](8, time.Minute),
		negative: NewTTLCache[string, struct{}](8, time.Minute),
	}
	key := HashAPIKey("sk-invalid")
	p.negative.Set(key, struct{}{})

	_, err := p.Resolve(context.Background(), &Credentials{APIKey: "sk-invalid"})
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("负缓存命中应直接返 ErrInvalidCredentials，got %v", err)
	}
}

// CachedSubscriptionProvider：true 进 30s cache、false 进 5s cache、DB 错不缓存。
func TestCachedSubscriptionProvider_SplitTTLCaches(t *testing.T) {
	p := &CachedSubscriptionProvider{
		trueCache:  NewTTLCache[string, struct{}](8, time.Minute),
		falseCache: NewTTLCache[string, struct{}](8, time.Minute),
	}
	// 手工塞 true cache → Has 短路返回 true，不触达 inner（nil inner 不 panic 即证明）
	p.trueCache.Set("acct\x001", struct{}{})
	got, err := p.Has(context.Background(), "acct", 1)
	if err != nil || !got {
		t.Fatalf("trueCache 命中应返 true：got=%v err=%v", got, err)
	}
	// false cache 同理
	p.falseCache.Set("acct\x002", struct{}{})
	got, err = p.Has(context.Background(), "acct", 2)
	if err != nil || got {
		t.Fatalf("falseCache 命中应返 false：got=%v err=%v", got, err)
	}
}
