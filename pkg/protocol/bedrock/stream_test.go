package bedrock

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/smithy-go/eventstream"
)

// encodeFrame 用 smithy encoder 造一个 Bedrock 风格的 event-stream 帧:
// payload = {"bytes": base64(anthropic event)}。
func encodeFrame(t *testing.T, w io.Writer, anthropicEvent string) {
	t.Helper()
	payload := []byte(`{"bytes":"` + base64.StdEncoding.EncodeToString([]byte(anthropicEvent)) + `"}`)
	if err := eventstream.NewEncoder().Encode(w, eventstream.Message{Payload: payload}); err != nil {
		t.Fatalf("encode frame: %v", err)
	}
}

func TestBedrock_DecodeTransport_EventStreamToAnthropicSSE(t *testing.T) {
	var raw bytes.Buffer
	encodeFrame(t, &raw, `{"type":"message_start","message":{"role":"assistant"}}`)
	encodeFrame(t, &raw, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`)
	encodeFrame(t, &raw, `{"type":"message_stop"}`)

	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		Body:   io.NopCloser(&raw),
	}
	dec := Factory{}.DecodeTransport(resp)
	if dec == nil {
		t.Fatal("eventstream 响应应返回解码 reader")
	}
	out, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("read decoded: %v", err)
	}
	s := string(out)
	// 每帧 → 一行 Anthropic SSE data:
	for _, want := range []string{
		`data: {"type":"message_start"`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`,
		`data: {"type":"message_stop"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("解码输出缺 %q\n全文:\n%s", want, s)
		}
	}
	// SSE 分隔
	if !strings.Contains(s, "\n\n") {
		t.Error("缺 SSE 事件分隔符")
	}
}

func TestBedrock_DecodeTransport_NonStreamReturnsNil(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(`{"id":"x"}`)),
	}
	f := Factory{}
	if f.DecodeTransport(resp) != nil {
		t.Error("非 eventstream(JSON)响应应返 nil(不解帧)")
	}
}

func TestBedrockURL(t *testing.T) {
	base := "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude/invoke"
	if got := bedrockURL(base, false); got != base {
		t.Errorf("非流式应不变: %s", got)
	}
	want := "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude/invoke-with-response-stream"
	if got := bedrockURL(base, true); got != want {
		t.Errorf("流式 URL = %s, want %s", got, want)
	}
	// 已是流式端点 → 不变
	if got := bedrockURL(want, true); got != want {
		t.Errorf("已流式端点应不变: %s", got)
	}
}
