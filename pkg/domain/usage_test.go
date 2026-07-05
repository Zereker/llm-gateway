package domain

import (
	"encoding/json"
	"testing"
)

// Usage new schema (docs/05 §3): Raw / Source / Estimator / Confidence / Truncated.
// No more Details map[MetricKey]int64 / Reasoning fields.

func TestUsage_ZeroValue(t *testing.T) {
	var u Usage
	if u.Total != 0 || u.Input != 0 || u.Output != 0 {
		t.Errorf("zero usage has nonzero fields: %+v", u)
	}
	if u.Raw != nil {
		t.Errorf("zero usage Raw should be nil, got %v", u.Raw)
	}
	if u.Source != "" {
		t.Errorf("zero usage Source should be empty, got %q", u.Source)
	}
}

func TestUsage_UpstreamExact(t *testing.T) {
	u := Usage{
		Input:      100,
		Output:     50,
		Total:      150,
		Raw:        json.RawMessage(`{"prompt_tokens":100,"completion_tokens":50}`),
		Source:     UsageSourceUpstream,
		Confidence: UsageConfidenceExact,
	}
	if u.Source != "upstream" {
		t.Errorf("Source = %q", u.Source)
	}
	if u.Confidence != "exact" {
		t.Errorf("Confidence = %q", u.Confidence)
	}
}

func TestUsage_EstimatedApproximate(t *testing.T) {
	u := Usage{
		Input:      100,
		Output:     50,
		Source:     UsageSourceEstimated,
		Estimator:  UsageEstimatorNaiveChars,
		Confidence: UsageConfidenceApproximate,
	}
	if u.Estimator != "naive_chars" {
		t.Errorf("Estimator = %q", u.Estimator)
	}
}

func TestUsage_TruncatedFlag(t *testing.T) {
	u := Usage{Truncated: true}
	if !u.Truncated {
		t.Error("Truncated should round-trip")
	}
}

// UsageSource / UsageEstimator / UsageConfidence constant value stability
func TestUsageEnum_StableValues(t *testing.T) {
	cases := map[string]string{
		string(UsageSourceUpstream):        "upstream",
		string(UsageSourceExtracted):       "extracted",
		string(UsageSourceEstimated):       "estimated",
		string(UsageEstimatorTiktoken):     "tiktoken",
		string(UsageEstimatorNaiveChars):   "naive_chars",
		string(UsageConfidenceExact):       "exact",
		string(UsageConfidenceDerived):     "derived",
		string(UsageConfidenceApproximate): "approximate",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("constant value drift: got=%q want=%q", got, want)
		}
	}
}
