package selector

import "testing"

func TestCooldownKey(t *testing.T) {
	if got := cooldownKey(42); got != "cd:endpoint:42" {
		t.Errorf("got=%q, want=cd:endpoint:42", got)
	}
	if got := cooldownKey(0); got != "cd:endpoint:0" {
		t.Errorf("got=%q", got)
	}
}

// RedisCooldownManager's SET/MGET are exercised via miniredis in ratelimit/redis_test.go;
// here we only test pure functions and fallback behavior.

func TestNewRedisCooldownManager_ImplementsInterface(t *testing.T) {
	var _ CooldownManager = NewRedisCooldownManager(nil, CooldownDurations{})
}
