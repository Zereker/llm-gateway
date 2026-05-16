package domain

import "testing"

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
