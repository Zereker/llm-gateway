package identity

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestOpenAIIdentityInjectsStreamUsage(t *testing.T) {
	in := []byte(`{"model":"gpt-4o","stream":true,"stream_options":{"other":true}}`)
	out, err := (openaiTranslator{}).TranslateRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		StreamOptions map[string]bool `json:"stream_options"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if !body.StreamOptions["include_usage"] || !body.StreamOptions["other"] {
		t.Fatalf("stream_options = %#v", body.StreamOptions)
	}
}

func TestIdentityTranslatorsPassThrough(t *testing.T) {
	in := []byte(`{"model":"x"}`)
	for name, tr := range map[string]interface {
		TranslateRequest([]byte) ([]byte, error)
	}{
		"anthropic": anthropicTranslator{},
		"responses": responsesTranslator{},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := tr.TranslateRequest(in)
			if err != nil || !bytes.Equal(out, in) {
				t.Fatalf("out=%s err=%v", out, err)
			}
		})
	}
}

func TestOpenAIIdentityResponsePassesChunksAndExtractsUsage(t *testing.T) {
	h := (openaiTranslator{}).NewResponseHandler()
	chunk := []byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n")
	out, err := h.Feed(chunk)
	if err != nil || !bytes.Equal(out, chunk) {
		t.Fatalf("Feed out=%q err=%v", out, err)
	}
	_, usage, err := h.Flush()
	if err != nil || usage == nil || usage.Total != 5 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
}
