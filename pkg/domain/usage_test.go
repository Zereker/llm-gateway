package domain

import "testing"

// MetricKey 是 string 别名常量；这里只断言常量值，作为兼容性保护
// （改动 string 字面值会影响 outbox JSON shape）。
func TestMetricKey_StableConstants(t *testing.T) {
	cases := map[MetricKey]string{
		CachedInputTokens:   "cached_input_tokens",
		CacheCreationTokens: "cache_creation_tokens",
		AudioInputSeconds:   "audio_input_seconds",
		AudioOutputSeconds:  "audio_output_seconds",
		VideoOutputSeconds:  "video_output_seconds",
		ImageInputCount:     "image_input_count",
		ImageOutputCount:    "image_output_count",
		TextCharCount:       "text_char_count",
	}
	for k, want := range cases {
		if string(k) != want {
			t.Errorf("MetricKey=%q, want=%q", string(k), want)
		}
	}
}

func TestUsage_ZeroValue(t *testing.T) {
	var u Usage
	if u.Total != 0 || u.Input != 0 || u.Output != 0 || u.Reasoning != 0 {
		t.Errorf("zero usage has nonzero fields: %+v", u)
	}
	if u.Details != nil {
		t.Errorf("zero usage Details should be nil, got %v", u.Details)
	}
}

func TestUsage_DetailsAttachable(t *testing.T) {
	u := Usage{
		Input:  100,
		Output: 200,
		Total:  300,
	}
	u.Details = map[MetricKey]int64{
		CachedInputTokens:   50,
		CacheCreationTokens: 10,
	}
	if u.Details[CachedInputTokens] != 50 {
		t.Errorf("Details[CachedInputTokens]=%d, want=50", u.Details[CachedInputTokens])
	}
}
