package policy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/domain"
)

func TestJSONDocumentAdapterExtractsSupportedShapes(t *testing.T) {
	body := []byte(`{
		"model":"x","system":"sys","instructions":"inst","prompt":"image prompt",
		"messages":[
			{"role":"user","content":"plain"},
			{"role":"user","content":[{"type":"text","text":"block"},{"type":"image_url","image_url":{"url":"secret-not-text"}}]}
		],
		"input":[{"role":"user","content":[{"type":"input_text","text":"response input"}]}],
		"systemInstruction":{"parts":[{"text":"gemini system"}]},
		"contents":[{"role":"user","parts":[{"text":"gemini input"},{"inlineData":{"data":"not text"}}]}],
		"tools":[{"function":{"description":"must not be mutable"}}]
	}`)
	segments, err := (JSONDocumentAdapter{}).Extract(body, domain.ProtoOpenAI, domain.ModalityChat)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string, len(segments))
	for _, segment := range segments {
		got[segment.Target] = string(segment.Text)
	}
	for target, want := range map[string]string{
		"/system": "sys", "/instructions": "inst", "/prompt": "image prompt",
		"/messages/0/content": "plain", "/messages/1/content/0/text": "block",
		"/input/0/content/0/text":         "response input",
		"/systemInstruction/parts/0/text": "gemini system",
		"/contents/0/parts/0/text":        "gemini input",
	} {
		if got[target] != want {
			t.Fatalf("target %s=%q want %q; all=%v", target, got[target], want, got)
		}
	}
	if strings.Contains(strings.Join(mapsValues(got), "|"), "must not be mutable") ||
		strings.Contains(strings.Join(mapsValues(got), "|"), "secret-not-text") {
		t.Fatalf("non-content fields extracted: %v", got)
	}
}

func TestJSONDocumentAdapterExtractsProviderResponseShapes(t *testing.T) {
	body := []byte(`{
		"choices":[{"text":"completion","message":{"content":"chat"}}],
		"content":[{"type":"text","text":"anthropic"}],
		"output":[{"content":[{"type":"output_text","text":"responses"}]}],
		"candidates":[{"content":{"parts":[{"text":"gemini"}]}}]
	}`)
	segments, err := (JSONDocumentAdapter{}).Extract(body, domain.ProtoOpenAI, domain.ModalityChat)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, segment := range segments {
		got[segment.Target] = string(segment.Text)
	}
	for target, want := range map[string]string{
		"/choices/0/text": "completion", "/choices/0/message/content": "chat",
		"/content/0/text": "anthropic", "/output/0/content/0/text": "responses",
		"/candidates/0/content/parts/0/text": "gemini",
	} {
		if got[target] != want {
			t.Fatalf("target %s=%q want %q; all=%v", target, got[target], want, got)
		}
	}
}

func TestJSONDocumentAdapterAppliesMutationsAtomically(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"card 4111"}],"temperature":0.7}`)
	mutations := []Mutation{{ID: "m1", Kind: MutationRedact, Target: "/messages/0/content", Replacement: []byte("card [MASKED]")}}
	rebuilt, err := (JSONDocumentAdapter{}).Apply(body, domain.ProtoOpenAI, domain.ModalityChat, mutations)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(rebuilt, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "x" || doc["temperature"].(float64) != 0.7 || strings.Contains(string(rebuilt), "4111") {
		t.Fatalf("rebuilt=%s", rebuilt)
	}

	for name, invalid := range map[string][]Mutation{
		"routing field": {{ID: "x", Kind: MutationRedact, Target: "/model", Replacement: []byte("other")}},
		"duplicate": {
			{ID: "x", Kind: MutationRedact, Target: "/messages/0/content", Replacement: []byte("a")},
			{ID: "y", Kind: MutationRedact, Target: "/messages/0/content", Replacement: []byte("b")},
		},
		"invalid utf8": {{ID: "x", Kind: MutationRedact, Target: "/messages/0/content", Replacement: []byte{0xff}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (JSONDocumentAdapter{}).Apply(body, domain.ProtoOpenAI, domain.ModalityChat, invalid); !errors.Is(err, ErrInvalidMutation) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func mapsValues(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}

	return out
}
