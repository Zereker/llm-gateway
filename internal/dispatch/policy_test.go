package dispatch

import (
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
)

// =============================================================================
// DefaultRetry unit tests
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
// ModelChainFallback unit tests
// =============================================================================

func TestModelChainFallback_OnExhausted_HasNext(t *testing.T) {
	in := newTestInput("gpt-4", "gpt-3.5", "gpt-3")
	s := newState(in, 3)
	// currently at idx=0; remaining = [gpt-3.5, gpt-3]
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
	in := newTestInput("gpt-4")
	s := newState(in, 3)
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
// HeaderAttemptCap unit tests
// =============================================================================

func TestHeaderAttemptCap_Resolve(t *testing.T) {
	tests := []struct {
		name string
		def  int
		hdr  string
		want int
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
			in := Input{AttemptCapOverride: tc.hdr}
			got := HeaderAttemptCap{Default: tc.def}.Resolve(in)
			if got != tc.want {
				t.Fatalf("Resolve(def=%d hdr=%q) = %d, want %d", tc.def, tc.hdr, got, tc.want)
			}
		})
	}
}

func TestHeaderAttemptCap_ZeroInput(t *testing.T) {
	got := HeaderAttemptCap{Default: 5}.Resolve(Input{})
	if got != 5 {
		t.Fatalf("Resolve(Input{}) = %d, want 5", got)
	}
}

// =============================================================================
// state finalize unit tests — ensure attempts outcome is filled in correctly
// =============================================================================

func TestState_FinalizeStreamedFillsLastAsSuccess(t *testing.T) {
	in := newTestInput("gpt-4")
	s := newState(in, 3)

	s.Record(newTestEP(1), Verdict{Class: ClassTransient, Latency: time.Millisecond})
	s.Record(newTestEP(2), Verdict{Class: ClassSuccess, Latency: time.Millisecond})
	s.ApplyStream(StreamReport{Usage: &domain.Usage{Total: 1}})

	if s.Outcome().Decision == nil {
		t.Fatalf("SchedulingDecision missing")
	}
	atts := s.Outcome().Decision.Attempts
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
	in := newTestInput("gpt-4")
	s := newState(in, 3)

	s.Record(newTestEP(1), Verdict{Class: ClassTransient, Latency: time.Millisecond})
	s.Record(newTestEP(2), Verdict{Class: ClassPermanent, Latency: time.Millisecond})
	s.SetAbort(Abort{Result: OutcomeNoEndpoint, HTTPCode: 503, Reason: "x"})

	atts := s.Outcome().Decision.Attempts
	if atts[0].Outcome != domain.AttemptFallback {
		t.Fatalf("attempt[0] = %s, want fallback", atts[0].Outcome)
	}
	if atts[1].Outcome != domain.AttemptFail {
		t.Fatalf("attempt[1] = %s, want fail", atts[1].Outcome)
	}
}

// TestState_NoAttemptsStillProducesDecision verifies the contract that
// Outcome.Decision is **always filled in** (even with 0 attempts), so
// downstream audit / log / metric code doesn't need to special-case nil.
func TestState_NoAttemptsStillProducesDecision(t *testing.T) {
	in := newTestInput("gpt-4")
	s := newState(in, 3)
	s.SetAbort(Abort{Result: OutcomeDepFail, HTTPCode: 503})

	d := s.Outcome().Decision
	if d == nil {
		t.Fatal("Decision should always be filled, got nil")
	}
	if d.Model != "gpt-4" {
		t.Errorf("Decision.Model = %q, want gpt-4", d.Model)
	}
	if d.RoutedModel != "gpt-4" {
		t.Errorf("Decision.RoutedModel = %q, want primary fallback gpt-4", d.RoutedModel)
	}
	if len(d.Attempts) != 0 {
		t.Errorf("Decision.Attempts = %d, want 0", len(d.Attempts))
	}
}

// actionsEqual is a simple Action comparison — type switch on the concrete type + key fields.
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
