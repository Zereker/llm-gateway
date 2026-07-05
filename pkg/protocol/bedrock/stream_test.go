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

// encodeFrame uses the smithy encoder to build a Bedrock-style event-stream frame:
// payload = {"bytes": base64(anthropic event)}.
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
		t.Fatal("eventstream response should return a decoded reader")
	}
	out, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("read decoded: %v", err)
	}
	s := string(out)
	// each frame → one line of Anthropic SSE data:
	for _, want := range []string{
		`data: {"type":"message_start"`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`,
		`data: {"type":"message_stop"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("decoded output missing %q\nfull text:\n%s", want, s)
		}
	}
	// SSE separator
	if !strings.Contains(s, "\n\n") {
		t.Error("missing SSE event separator")
	}
}

// encodeException builds a Bedrock exception frame with :message-type=exception.
func encodeException(t *testing.T, w io.Writer, excType, body string) {
	t.Helper()
	msg := eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("exception")},
			{Name: ":exception-type", Value: eventstream.StringValue(excType)},
		},
		Payload: []byte(body),
	}
	if err := eventstream.NewEncoder().Encode(w, msg); err != nil {
		t.Fatalf("encode exception: %v", err)
	}
}

// A mid-stream exception frame must be recognized as an error (rather than being silently swallowed as a clean truncation).
func TestBedrock_DecodeTransport_ExceptionFrameSurfacesError(t *testing.T) {
	var raw bytes.Buffer
	encodeFrame(t, &raw, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`)
	encodeException(t, &raw, "modelStreamErrorException", `{"message":"throttled"}`)

	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		Body:   io.NopCloser(&raw),
	}
	dec := Factory{}.DecodeTransport(resp)
	if dec == nil {
		t.Fatal("eventstream response should return a decoded reader")
	}
	out, err := io.ReadAll(dec)
	if err == nil {
		t.Fatal("ReadAll after an exception frame should return an error (must not silently truncate)")
	}
	if !strings.Contains(err.Error(), "modelStreamErrorException") {
		t.Errorf("error should contain the exception type, got: %v", err)
	}
	// Normal content preceding the exception frame should still be decoded.
	if !strings.Contains(string(out), `"text":"Hi"`) {
		t.Errorf("content preceding the exception should already be decoded, got: %s", out)
	}
}

func TestBedrock_DecodeTransport_NonStreamReturnsNil(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(`{"id":"x"}`)),
	}
	f := Factory{}
	if f.DecodeTransport(resp) != nil {
		t.Error("non-eventstream (JSON) response should return nil (no frame decoding)")
	}
}

func TestBedrockURL(t *testing.T) {
	base := "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude/invoke"
	if got := bedrockURL(base, false); got != base {
		t.Errorf("non-streaming should be unchanged: %s", got)
	}
	want := "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude/invoke-with-response-stream"
	if got := bedrockURL(base, true); got != want {
		t.Errorf("streaming URL = %s, want %s", got, want)
	}
	// already a streaming endpoint → unchanged
	if got := bedrockURL(want, true); got != want {
		t.Errorf("already-streaming endpoint should be unchanged: %s", got)
	}
}
