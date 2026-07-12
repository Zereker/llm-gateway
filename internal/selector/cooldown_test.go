package selector

import (
	"testing"
	"time"
)

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

func TestResolveCooldownTTL(t *testing.T) {
	d := CooldownDurations{Capacity: 60 * time.Second, Transient: 30 * time.Second}

	cases := []struct {
		name       string
		class      ErrorClass
		retryAfter time.Duration
		want       time.Duration
	}{
		{"no hint uses class duration", ClassCapacity, 0, 60 * time.Second},
		{"hint overrides class duration", ClassCapacity, 5 * time.Second, 5 * time.Second},
		{"hint below floor clamps up", ClassCapacity, 200 * time.Millisecond, resetTTLFloor},
		{"hint above cap clamps down", ClassCapacity, 24 * time.Hour, resetTTLCap},
		{"disabled class ignores hint", ClassInvalid, 5 * time.Second, 0},
		{"hint applies to transient too", ClassTransient, 3 * time.Second, 3 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveCooldownTTL(d, tc.class, tc.retryAfter); got != tc.want {
				t.Errorf("resolveCooldownTTL = %v, want %v", got, tc.want)
			}
		})
	}
}

// jitterTTL: 抖动必须落在 ±10% 区间内，0/负值原样返回。
func TestJitterTTL_Bounds(t *testing.T) {
	base := 30 * time.Second
	for i := 0; i < 1000; i++ {
		got := jitterTTL(base)
		if got < 27*time.Second || got > 33*time.Second {
			t.Fatalf("jitterTTL(%v) = %v, out of ±10%% bounds", base, got)
		}
	}
	if got := jitterTTL(0); got != 0 {
		t.Fatalf("jitterTTL(0) = %v, want 0", got)
	}
}
