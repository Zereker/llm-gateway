package middleware

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// stubDetector / stubParser let tests force specific outcomes.
type stubDetector struct {
	proto domain.Protocol
	mod   domain.Modality
}

func (s stubDetector) Detect(_ string, _ []byte) (domain.Protocol, domain.Modality) {
	return s.proto, s.mod
}

type stubParser struct {
	out domain.CanonicalRequest
	err error
}

func (s stubParser) Parse(_ []byte, _ domain.Protocol, _ domain.Modality) (domain.CanonicalRequest, error) {
	return s.out, s.err
}

func TestEnvelope_PopulatesRC(t *testing.T) {
	r := newGinTest(
		TraceContext(),
		Recover(),
		Envelope(EnvelopeDeps{
			Detector: stubDetector{proto: domain.ProtoOpenAI, mod: domain.ModalityChat},
			Parser:   stubParser{out: domain.CanonicalRequest{Model: "gpt-4o"}},
		}),
	)
	var captured *domain.RequestEnvelope
	r.POST("/x", func(c *gin.Context) {
		captured = GetRequestContext(c).Envelope
		c.Status(200)
	})

	body := `{"model":"gpt-4o"}`
	req := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if captured == nil {
		t.Fatal("envelope nil")
	}
	if string(captured.RawBytes) != body {
		t.Errorf("RawBytes = %q, want %q", captured.RawBytes, body)
	}
	if captured.SourceProtocol != domain.ProtoOpenAI {
		t.Errorf("SourceProtocol = %v", captured.SourceProtocol)
	}
	if captured.Parsed.Model != "gpt-4o" {
		t.Errorf("Parsed.Model = %q", captured.Parsed.Model)
	}
	if captured.RequestTime.IsZero() {
		t.Error("RequestTime zero")
	}
}

func TestEnvelope_RestoresBody(t *testing.T) {
	r := newGinTest(
		TraceContext(),
		Recover(),
		Envelope(EnvelopeDeps{
			Detector: stubDetector{proto: domain.ProtoOpenAI, mod: domain.ModalityChat},
			Parser:   stubParser{},
		}),
	)
	r.POST("/x", func(c *gin.Context) {
		// downstream re-reads body — should still get full payload
		readBack, _ := io.ReadAll(c.Request.Body)
		if string(readBack) != `{"a":1}` {
			t.Errorf("body re-read = %q, want %q", readBack, `{"a":1}`)
		}
		c.Status(200)
	})

	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"a":1}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestEnvelope_RejectsUnknownProtocol(t *testing.T) {
	r := newGinTest(
		TraceContext(),
		Recover(),
		Envelope(EnvelopeDeps{
			Detector: stubDetector{proto: domain.ProtoUnknown},
			Parser:   stubParser{},
		}),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unknown source protocol") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestEnvelope_ParserError(t *testing.T) {
	r := newGinTest(
		TraceContext(),
		Recover(),
		Envelope(EnvelopeDeps{
			Detector: stubDetector{proto: domain.ProtoOpenAI, mod: domain.ModalityChat},
			Parser:   stubParser{err: errors.New("bad json")},
		}),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{garbage}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bad json") {
		t.Errorf("body should include parser error: %s", w.Body.String())
	}
}

func TestEnvelope_DefaultDetectorParser_Integration(t *testing.T) {
	// Compose with the real DefaultDetector + DefaultParser to ensure
	// they wire correctly with M3.
	r := newGinTest(
		TraceContext(),
		Recover(),
		Envelope(EnvelopeDeps{Detector: DefaultDetector{}, Parser: DefaultParser{}}),
	)
	var got *domain.RequestEnvelope
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		got = GetRequestContext(c).Envelope
		c.Status(200)
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	if got.SourceProtocol != domain.ProtoOpenAI || got.Modality != domain.ModalityChat {
		t.Errorf("got proto=%v modality=%v", got.SourceProtocol, got.Modality)
	}
	if got.Parsed.Model != "gpt-4o" {
		t.Errorf("Parsed.Model = %q", got.Parsed.Model)
	}
}
