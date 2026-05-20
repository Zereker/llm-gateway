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

// RedisCooldownManager 的 SET/MGET 由 ratelimit/redis_test.go 用 miniredis 跑通；
// 这里只测纯函数与 fallback 行为。

func TestNewRedisCooldownManager_ImplementsInterface(t *testing.T) {
	var _ CooldownManager = NewRedisCooldownManager(nil, CooldownDurations{})
}
