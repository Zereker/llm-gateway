package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProtocol_String(t *testing.T) {
	cases := []struct {
		p    Protocol
		want string
	}{
		{ProtoUnknown, "unknown"},
		{ProtoOpenAI, "openai"},
		{ProtoAnthropic, "anthropic"},
		{ProtoGemini, "gemini"},
		{ProtoBedrock, "bedrock"},
		{ProtoCustom, "custom"},
		{ProtoResponses, "responses"},
		{Protocol(99), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.p.String(); got != tc.want {
				t.Errorf("got=%q, want=%q", got, tc.want)
			}
		})
	}
}

func TestProtocol_Parse(t *testing.T) {
	cases := []struct {
		in   string
		want Protocol
	}{
		{"openai", ProtoOpenAI},
		{"anthropic", ProtoAnthropic},
		{"gemini", ProtoGemini},
		{"bedrock", ProtoBedrock},
		{"custom", ProtoCustom},
		{"responses", ProtoResponses},
		{"unknown", ProtoUnknown},
		{"", ProtoUnknown},
		{"garbage", ProtoUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := ParseProtocol(tc.in); got != tc.want {
				t.Errorf("ParseProtocol(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestProtocol_JSONRoundTrip(t *testing.T) {
	cases := []Protocol{ProtoOpenAI, ProtoAnthropic, ProtoGemini, ProtoBedrock, ProtoCustom, ProtoResponses}
	for _, p := range cases {
		t.Run(p.String(), func(t *testing.T) {
			data, err := json.Marshal(p)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			want := `"` + p.String() + `"`
			if string(data) != want {
				t.Errorf("Marshal = %s, want %s", data, want)
			}
			var got Protocol
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got != p {
				t.Errorf("RoundTrip = %v, want %v", got, p)
			}
		})
	}
}

func TestProtocol_UnmarshalJSON_StrictRejectsUnknown(t *testing.T) {
	var p Protocol
	err := json.Unmarshal([]byte(`"invalid"`), &p)
	if err == nil {
		t.Fatalf("want error for unknown protocol, got nil")
	}
	if !strings.Contains(err.Error(), `unknown protocol "invalid"`) {
		t.Errorf("err message = %q, want to contain 'unknown protocol \"invalid\"'", err)
	}
}

func TestProtocol_UnmarshalJSON_EmptyAndUnknownTokens(t *testing.T) {
	// 空字符串 + 字面 "unknown" 都解析成 ProtoUnknown 不报错
	for _, in := range []string{`""`, `"unknown"`} {
		var p Protocol
		if err := json.Unmarshal([]byte(in), &p); err != nil {
			t.Errorf("Unmarshal(%s) err = %v", in, err)
		}
		if p != ProtoUnknown {
			t.Errorf("Unmarshal(%s) = %v, want ProtoUnknown", in, p)
		}
	}
}

func TestModality_String(t *testing.T) {
	cases := []struct {
		m    Modality
		want string
	}{
		{ModalityChat, "chat"},
		{ModalityEmbedding, "embedding"},
		{ModalityImage, "image"},
		{ModalityRerank, "rerank"},
		{ModalityTTS, "tts"},
		{ModalityASR, "asr"},
		{ModalityTask, "task"},
		{Modality(99), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.m.String(); got != tc.want {
				t.Errorf("got=%q, want=%q", got, tc.want)
			}
		})
	}
}

func TestBudgetStatus_String(t *testing.T) {
	cases := []struct {
		s    BudgetStatus
		want string
	}{
		{BudgetUnknown, "unknown"},
		{BudgetActive, "active"},
		{BudgetInactive, "inactive"},
		{BudgetStatus(99), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.s.String(); got != tc.want {
				t.Errorf("got=%q, want=%q", got, tc.want)
			}
		})
	}
}

func TestAttemptOutcome_String(t *testing.T) {
	cases := []struct {
		o    AttemptOutcome
		want string
	}{
		{AttemptUnknown, "unknown"},
		{AttemptSuccess, "success"},
		{AttemptRetry, "retry"},
		{AttemptFallback, "fallback"},
		{AttemptFail, "fail"},
		{AttemptOutcome(99), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.o.String(); got != tc.want {
				t.Errorf("got=%q, want=%q", got, tc.want)
			}
		})
	}
}
