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

func TestJSONDocumentAdapterRejectsUnsupportedDocuments(t *testing.T) {
	adapter := JSONDocumentAdapter{}
	for name, body := range map[string][]byte{
		"malformed":     []byte(`{"messages":`),
		"trailing":      []byte(`{} {}`),
		"non object":    []byte(`[]`),
		"empty content": nil,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := adapter.Extract(body, domain.ProtoOpenAI, domain.ModalityChat); !errors.Is(err, ErrUnsupportedDocument) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestJSONDocumentAdapterExtractsEmbeddingAndMixedContent(t *testing.T) {
	adapter := JSONDocumentAdapter{}
	tests := []struct {
		name     string
		body     string
		modality domain.Modality
		want     map[string]string
	}{
		{
			name: "embedding array", modality: domain.ModalityEmbedding,
			body: `{"input":["first","",17,"second"]}`,
			want: map[string]string{"/input/0": "first", "/input/3": "second"},
		},
		{
			name: "input variants", modality: domain.ModalityChat,
			body: `{"input":"plain","text":"top","content":["nested",7,{"text":"block"}],"choices":[7,{"delta":{"content":"delta"}}]}`,
			want: map[string]string{
				"/input": "plain", "/text": "top", "/content/0": "nested",
				"/content/2/text": "block", "/choices/1/delta/content": "delta",
			},
		},
		{
			name: "malformed provider containers", modality: domain.ModalityChat,
			body: `{"messages":[7,{}],"choices":{},"contents":[7,{"parts":[8,{}, {"text":"ok"}]}],"candidates":[8,{}, {"content":{"parts":[{"text":"candidate"}]}}],"output":[8,{"text":"out"}]}`,
			want: map[string]string{
				"/contents/1/parts/2/text": "ok", "/candidates/2/content/parts/0/text": "candidate", "/output/1/text": "out",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			segments, err := adapter.Extract([]byte(tc.body), domain.ProtoOpenAI, tc.modality)
			if err != nil {
				t.Fatal(err)
			}
			got := make(map[string]string, len(segments))
			for _, segment := range segments {
				got[segment.Target] = string(segment.Text)
			}
			for target, want := range tc.want {
				if got[target] != want {
					t.Fatalf("target %s=%q want %q; all=%v", target, got[target], want, got)
				}
			}
		})
	}
}

func TestJSONDocumentAdapterApplyNoMutationsPreservesDocument(t *testing.T) {
	body := []byte(`{"input":"hello","count":1}`)
	rebuilt, err := (JSONDocumentAdapter{}).Apply(body, domain.ProtoOpenAI, domain.ModalityChat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(rebuilt) || !strings.Contains(string(rebuilt), `"input":"hello"`) {
		t.Fatalf("rebuilt=%s", rebuilt)
	}
}

func TestJSONPointerHelpersAndFailures(t *testing.T) {
	parts, err := parsePointer(`/a~1b/c~0d`)
	if err != nil || len(parts) != 2 || parts[0] != "a/b" || parts[1] != "c~d" {
		t.Fatalf("parts=%v err=%v", parts, err)
	}
	if escaped := escapePointer("a~/b"); escaped != "a~0~1b" {
		t.Fatalf("escaped=%q", escaped)
	}
	for _, pointer := range []string{"", "missing-slash"} {
		if _, err := parsePointer(pointer); !errors.Is(err, ErrInvalidMutation) {
			t.Fatalf("parsePointer(%q) err=%v", pointer, err)
		}
	}

	tests := []struct {
		name    string
		doc     any
		pointer string
	}{
		{"empty pointer", map[string]any{"x": "y"}, ""},
		{"missing map child", map[string]any{}, "/missing/value"},
		{"bad array traversal", []any{"x"}, "/bad/value"},
		{"array traversal range", []any{"x"}, "/2/value"},
		{"scalar traversal", map[string]any{"x": "text"}, "/x/value"},
		{"map leaf not text", map[string]any{"x": 1}, "/x"},
		{"array leaf invalid", []any{"x"}, "/bad"},
		{"array leaf range", []any{"x"}, "/2"},
		{"array leaf not text", []any{1}, "/0"},
		{"scalar leaf", "text", "/x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := setPointer(tc.doc, tc.pointer, "replacement"); !errors.Is(err, ErrInvalidMutation) {
				t.Fatalf("err=%v", err)
			}
		})
	}

	doc := map[string]any{"items": []any{"first"}}
	if err := setPointer(doc, "/items/0", "changed"); err != nil {
		t.Fatal(err)
	}
	if doc["items"].([]any)[0] != "changed" {
		t.Fatalf("doc=%v", doc)
	}
}

func mapsValues(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}

	return out
}
