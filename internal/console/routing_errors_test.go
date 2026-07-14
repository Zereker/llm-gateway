package console

import "testing"

func TestRoutingValidationErrorsAreStable(t *testing.T) {
	if got := (&InvalidRoutingPolicyError{Reason: "bad policy"}).Error(); got != "routing policy invalid: bad policy" {
		t.Fatalf("policy error = %q", got)
	}
	if got := (&InvalidRoutingCostError{Reason: "bad cost"}).Error(); got != "routing cost invalid: bad cost" {
		t.Fatalf("cost error = %q", got)
	}
}
