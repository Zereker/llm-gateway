package adapters

import (
	"errors"
	"testing"

	"github.com/zereker/llm-gateway/internal/dispatch"
	"github.com/zereker/llm-gateway/internal/invoker"
	"github.com/zereker/llm-gateway/internal/moderation"
	"github.com/zereker/llm-gateway/internal/policy"
	"github.com/zereker/llm-gateway/internal/selector"
)

func TestClassMappings(t *testing.T) {
	cases := []struct {
		dispatch dispatch.Class
		selector selector.ErrorClass
		invoker  invoker.Class
	}{
		{dispatch.ClassSuccess, selector.ClassSuccess, invoker.ClassSuccess},
		{dispatch.ClassTransient, selector.ClassTransient, invoker.ClassTransient},
		{dispatch.ClassCapacity, selector.ClassCapacity, invoker.ClassCapacity},
		{dispatch.ClassPermanent, selector.ClassPermanent, invoker.ClassPermanent},
		{dispatch.ClassInvalid, selector.ClassInvalid, invoker.ClassInvalid},
		{dispatch.ClassUnknown, selector.ClassUnknown, invoker.ClassUnknown},
	}
	for _, tc := range cases {
		if got := dispatchClassToSelector(tc.dispatch); got != tc.selector {
			t.Errorf("dispatchClassToSelector(%v) = %v", tc.dispatch, got)
		}
		if got := invokerClassToDispatch(tc.invoker); got != tc.dispatch {
			t.Errorf("invokerClassToDispatch(%v) = %v", tc.invoker, got)
		}
	}
}

func TestForwardResultToStreamReport(t *testing.T) {
	tests := []struct {
		name         string
		forward      invoker.ForwardResult
		class        dispatch.Class
		code         int
		reason       string
		localFailure bool
		prewrite     bool
	}{
		{name: "success", forward: invoker.ForwardResult{Committed: true}, prewrite: false},
		{
			name: "stream processing failure", forward: invoker.ForwardResult{FeedErr: errors.New("feed")},
			class: dispatch.ClassTransient, code: 503, reason: "response stream processing failed", prewrite: true,
		},
		{
			name: "policy enforcement failure", forward: invoker.ForwardResult{FeedErr: moderation.ErrPolicyEnforcement},
			class: dispatch.ClassTransient, code: 503, reason: "response policy enforcement failed", localFailure: true, prewrite: true,
		},
		{
			name: "policy denial", forward: invoker.ForwardResult{FeedErr: errors.Join(moderation.ErrPolicyEnforcement, policy.ErrDenied)},
			class: dispatch.ClassInvalid, code: 400, reason: "content rejected by response policy", localFailure: true, prewrite: true,
		},
		{
			name: "committed failure", forward: invoker.ForwardResult{FeedErr: moderation.ErrPolicyEnforcement, Committed: true},
			localFailure: true, prewrite: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := forwardResultToStreamReport(tc.forward)
			if report.LocalFailure != tc.localFailure || (report.Prewrite != nil) != tc.prewrite {
				t.Fatalf("report=%+v", report)
			}
			if tc.prewrite && (report.Prewrite.Class != tc.class || report.Prewrite.HTTPCode != tc.code || report.Prewrite.Reason != tc.reason) {
				t.Fatalf("prewrite=%+v", report.Prewrite)
			}
		})
	}
}
