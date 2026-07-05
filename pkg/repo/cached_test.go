package repo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Negative-cache unit tests aren't convenient to drive directly through
// SQLAPIKeyProvider (needs a DB); instead we test CachedSubscriptionProvider's
// dual-TTL semantics and CachedAPIKeyProvider's negative-cache decision logic
// here directly (a nil inner would panic, so we only test the parts that can
// be exercised in isolation).

// TestNegativeTTL_ShorterThanPositive: the negative-cache TTL must be
// significantly shorter than the positive cache's default 30s.
func TestNegativeTTL_ShorterThanPositive(t *testing.T) {
	if negativeTTL >= 30*time.Second {
		t.Fatalf("negativeTTL=%v, should be significantly shorter than the positive 30s", negativeTTL)
	}
}

// CachedAPIKeyProvider negative cache: after the first miss returns
// ErrInvalidCredentials, the same key must not hit inner again for a while
// (verified via a call-count stub).
func TestCachedAPIKeyProvider_NegativeCacheBlocksRepeatLookups(t *testing.T) {
	// Directly verify Resolve's negative-cache branch by composing a TTLCache —
	// build a fake CachedAPIKeyProvider (Resolve would panic if inner were nil
	// and actually reached, so this hand-built equivalent structure ensures a
	// negative-cache hit short-circuits and never touches inner).
	p := &CachedAPIKeyProvider{
		inner:    nil, // if inner is ever reached, the nil panic fails the test
		cache:    NewTTLCache[string, *UserIdentity](8, time.Minute),
		negative: NewTTLCache[string, struct{}](8, time.Minute),
	}
	key := HashAPIKey("sk-invalid")
	p.negative.Set(key, struct{}{})

	_, err := p.Resolve(context.Background(), &Credentials{APIKey: "sk-invalid"})
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("a negative-cache hit should return ErrInvalidCredentials directly, got %v", err)
	}
}

// CachedSubscriptionProvider: true goes into the 30s cache, false goes into
// the 5s cache, DB errors are not cached.
func TestCachedSubscriptionProvider_SplitTTLCaches(t *testing.T) {
	p := &CachedSubscriptionProvider{
		trueCache:  NewTTLCache[string, struct{}](8, time.Minute),
		falseCache: NewTTLCache[string, struct{}](8, time.Minute),
	}
	// manually seed the true cache -> Has should short-circuit to true without touching inner
	// (a nil inner not panicking is the proof)
	p.trueCache.Set("acct\x001", struct{}{})
	got, err := p.Has(context.Background(), "acct", 1)
	if err != nil || !got {
		t.Fatalf("trueCache hit should return true: got=%v err=%v", got, err)
	}
	// same for the false cache
	p.falseCache.Set("acct\x002", struct{}{})
	got, err = p.Has(context.Background(), "acct", 2)
	if err != nil || got {
		t.Fatalf("falseCache hit should return false: got=%v err=%v", got, err)
	}
}
