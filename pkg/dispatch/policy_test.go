package dispatch

import (
	"testing"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// =============================================================================
// DefaultRetry 单测
// =============================================================================

func TestDefaultRetry_Decide(t *testing.T) {
	cases := []struct {
		name    string
		verdict Verdict
		wantAct Action
	}{
		{"success → Stream", Verdict{Class: ClassSuccess}, Stream{}},
		{"invalid → Abort 400", Verdict{Class: ClassInvalid, Reason: "bad"}, Abort{Result: OutcomeInvalid, Class: ClassInvalid, HTTPCode: 400, Reason: "bad"}},
		{"transient → Continue", Verdict{Class: ClassTransient}, Continue{}},
		{"capacity → Continue", Verdict{Class: ClassCapacity}, Continue{}},
		{"permanent → Continue", Verdict{Class: ClassPermanent}, Continue{}},
		{"unknown → Continue", Verdict{Class: ClassUnknown}, Continue{}},
	}

	r := DefaultRetry{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Decide(nil, tc.verdict)
			if !actionsEqual(got, tc.wantAct) {
				t.Fatalf("Decide(%v) = %#v, want %#v", tc.verdict, got, tc.wantAct)
			}
		})
	}
}

// =============================================================================
// ModelChainFallback 单测
// =============================================================================

func TestModelChainFallback_OnExhausted_HasNext(t *testing.T) {
	rc := newTestRC("gpt-4", "gpt-3.5", "gpt-3")
	s := newState(rc, 3)
	// 当前在 idx=0；remaining = [gpt-3.5, gpt-3]
	got := ModelChainFallback{}.OnExhausted(s)
	sw, ok := got.(Switch)
	if !ok {
		t.Fatalf("want Switch, got %#v", got)
	}
	if sw.Next.Model != "gpt-3.5" {
		t.Fatalf("want next=gpt-3.5, got %s", sw.Next.Model)
	}
}

func TestModelChainFallback_OnExhausted_NoMore(t *testing.T) {
	rc := newTestRC("gpt-4")
	s := newState(rc, 3)
	got := ModelChainFallback{}.OnExhausted(s)
	ab, ok := got.(Abort)
	if !ok {
		t.Fatalf("want Abort, got %#v", got)
	}
	if ab.Result != OutcomeNoEndpoint || ab.HTTPCode != 503 {
		t.Fatalf("want NoEndpoint 503, got Result=%s HTTPCode=%d", ab.Result, ab.HTTPCode)
	}
}

// =============================================================================
// HeaderAttemptCap 单测
// =============================================================================

func TestHeaderAttemptCap_Resolve(t *testing.T) {
	tests := []struct {
		name    string
		def     int
		hdr     string
		want    int
	}{
		{"default only", 3, "", 3},
		{"header tighter wins", 3, "1", 1},
		{"header looser ignored", 3, "10", 3},
		{"header equal ignored", 3, "3", 3},
		{"header 0 ignored", 3, "0", 3},
		{"header negative ignored", 3, "-1", 3},
		{"header non-numeric ignored", 3, "abc", 3},
		{"default zero falls back to 3", 0, "", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc := &domain.RequestContext{Extras: map[string]any{}}
			if tc.hdr != "" {
				rc.Extras[HeaderKey] = tc.hdr
			}
			got := HeaderAttemptCap{Default: tc.def}.Resolve(rc)
			if got != tc.want {
				t.Fatalf("Resolve(def=%d hdr=%q) = %d, want %d", tc.def, tc.hdr, got, tc.want)
			}
		})
	}
}

func TestHeaderAttemptCap_NilRequest(t *testing.T) {
	got := HeaderAttemptCap{Default: 5}.Resolve(nil)
	if got != 5 {
		t.Fatalf("Resolve(nil) = %d, want 5", got)
	}
}

// =============================================================================
// state finalize 单测——保证 attempts outcome 正确填充
// =============================================================================

func TestState_FinalizeStreamedFillsLastAsSuccess(t *testing.T) {
	rc := newTestRC("gpt-4")
	s := newState(rc, 3)

	s.Record(newTestEP(1), Verdict{Class: ClassTransient, Latency: time.Millisecond})
	s.Record(newTestEP(2), Verdict{Class: ClassSuccess, Latency: time.Millisecond})
	s.ApplyStream(StreamReport{Usage: &domain.Usage{Total: 1}})

	if rc.SchedulingDecision == nil {
		t.Fatalf("SchedulingDecision missing")
	}
	atts := rc.SchedulingDecision.Attempts
	if len(atts) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(atts))
	}
	if atts[0].Outcome != domain.AttemptFallback {
		t.Fatalf("attempt[0] = %s, want fallback", atts[0].Outcome)
	}
	if atts[1].Outcome != domain.AttemptSuccess {
		t.Fatalf("attempt[1] = %s, want success", atts[1].Outcome)
	}
}

func TestState_FinalizeAbortFillsLastAsFail(t *testing.T) {
	rc := newTestRC("gpt-4")
	s := newState(rc, 3)

	s.Record(newTestEP(1), Verdict{Class: ClassTransient, Latency: time.Millisecond})
	s.Record(newTestEP(2), Verdict{Class: ClassPermanent, Latency: time.Millisecond})
	s.SetAbort(Abort{Result: OutcomeNoEndpoint, HTTPCode: 503, Reason: "x"})

	atts := rc.SchedulingDecision.Attempts
	if atts[0].Outcome != domain.AttemptFallback {
		t.Fatalf("attempt[0] = %s, want fallback", atts[0].Outcome)
	}
	if atts[1].Outcome != domain.AttemptFail {
		t.Fatalf("attempt[1] = %s, want fail", atts[1].Outcome)
	}
}

func TestState_NoAttemptsNoDecision(t *testing.T) {
	rc := newTestRC("gpt-4")
	s := newState(rc, 3)
	s.SetAbort(Abort{Result: OutcomeDepFail, HTTPCode: 503})

	if rc.SchedulingDecision != nil {
		t.Fatalf("expected no SchedulingDecision when no attempts; got %+v", rc.SchedulingDecision)
	}
}

// actionsEqual 简易 Action 比较——type switch 看类型 + 关键字段。
func actionsEqual(a, b Action) bool {
	switch ax := a.(type) {
	case Continue:
		_, ok := b.(Continue)
		return ok
	case Stream:
		_, ok := b.(Stream)
		return ok
	case Switch:
		bx, ok := b.(Switch)
		return ok && bx.Next == ax.Next
	case Abort:
		bx, ok := b.(Abort)
		return ok && bx.Result == ax.Result && bx.Class == ax.Class && bx.HTTPCode == ax.HTTPCode && bx.Reason == ax.Reason
	}
	return false
}
