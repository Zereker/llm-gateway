package middleware

import (
	"testing"

	"github.com/zereker/llm-gateway/internal/domain"
)

func TestEffectiveAttemptOverrideUsesTightestLimit(t *testing.T) {
	policy := &domain.ModelRoutingDecision{MaxAttempts: 2}
	tests := []struct {
		header string
		policy *domain.ModelRoutingDecision
		want   string
	}{
		{header: "", policy: policy, want: "2"},
		{header: "1", policy: policy, want: "1"},
		{header: "3", policy: policy, want: "2"},
		{header: "invalid", policy: policy, want: "2"},
		{header: "3", policy: nil, want: "3"},
		{header: "3", policy: &domain.ModelRoutingDecision{}, want: "3"},
	}

	for _, tc := range tests {
		if got := effectiveAttemptOverride(tc.header, tc.policy); got != tc.want {
			t.Errorf("header=%q policy=%+v: got %q, want %q", tc.header, tc.policy, got, tc.want)
		}
	}
}
