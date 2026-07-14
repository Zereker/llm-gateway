package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServeRecordedReplaysStreamingFixture(t *testing.T) {
	recordedReplies = map[string][]byte{
		"model": []byte(`{"choices":[{"message":{"content":"non-stream"}}]}`),
	}
	recordedStreamReplies = map[string][]byte{
		"model": []byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"),
	}

	recorder := httptest.NewRecorder()
	started := time.Now()
	serveRecorded(recorder, "model", true)

	if elapsed := time.Since(started); elapsed < 5*time.Millisecond {
		t.Fatalf("stream replay completed before its TTFT delay: %s", elapsed)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream body does not contain the terminator: %q", body)
	}
}
