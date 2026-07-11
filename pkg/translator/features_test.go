package translator

import (
	"reflect"
	"sort"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

func TestDroppedRequestFeatures_OpenAISource(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "plain text only",
			body: `{"model":"x","messages":[{"role":"user","content":"hi"}]}`,
			want: nil,
		},
		{
			name: "tools defined",
			body: `{"model":"x","tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"hi"}]}`,
			want: []string{"tools"},
		},
		{
			name: "empty tools array is not a feature",
			body: `{"model":"x","tools":[],"messages":[{"role":"user","content":"hi"}]}`,
			want: nil,
		},
		{
			name: "assistant tool_calls",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"id":"1","function":{"name":"f"}}]}]}`,
			want: []string{"tool_calls"},
		},
		{
			name: "tool result message",
			body: `{"messages":[{"role":"tool","content":"result","tool_call_id":"1"}]}`,
			want: []string{"tool_calls"},
		},
		{
			name: "multimodal image part",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"http://x"}}]}]}`,
			want: []string{"multimodal"},
		},
		{
			name: "text-only array content is not multimodal",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := droppedRequestFeatures(domain.ProtoOpenAI, []byte(tc.body))
			assertFeatures(t, got, tc.want)
		})
	}
}

func TestDroppedRequestFeatures_AnthropicSource(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "text block only",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			want: nil,
		},
		{
			name: "tool_use block",
			body: `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"f","input":{}}]}]}`,
			want: []string{"tool_calls"},
		},
		{
			name: "tool_result block",
			body: `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"r"}]}]}`,
			want: []string{"tool_calls"},
		},
		{
			name: "image block",
			body: `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`,
			want: []string{"multimodal"},
		},
		{
			name: "tools plus image",
			body: `{"tools":[{"name":"f"}],"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`,
			want: []string{"tools", "multimodal"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := droppedRequestFeatures(domain.ProtoAnthropic, []byte(tc.body))
			assertFeatures(t, got, tc.want)
		})
	}
}

// ReportLossyRequest must never panic on malformed / empty bodies.
func TestReportLossyRequest_MalformedIsSafe(t *testing.T) {
	for _, body := range []string{"", "not json", "{}", `{"messages":"oops"}`} {
		ReportLossyRequest(domain.ProtoOpenAI, domain.ProtoGemini, []byte(body))
	}
}

func assertFeatures(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(got)
	sort.Strings(want)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("features = %v, want %v", got, want)
	}
}
