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

// stubParser lets tests force specific outcomes from M3.
type stubParser struct {
	out domain.CanonicalRequest
	err error
}

func (s stubParser) Parse(_ []byte, _ domain.Protocol, _ domain.Modality) (domain.CanonicalRequest, error) {
	return s.out, s.err
}

// envelopeChain 给测试装一条 minimum 链：TraceContext + Recover + WithSourceProtocol + Envelope。
// proto = ProtoUnknown 时跳过 WithSourceProtocol 用于"忘挂打标"的负面 case。
func envelopeChain(proto domain.Protocol, parser Parser) *gin.Engine {
	if proto == domain.ProtoUnknown {
		return newGinTest(TraceContext(), Recover(), Envelope(EnvelopeDeps{Parser: parser}))
	}
	return newGinTest(
		TraceContext(),
		Recover(),
		WithSourceProtocol(proto, domain.ModalityChat),
		Envelope(EnvelopeDeps{Parser: parser}),
	)
}

func TestEnvelope_PopulatesRC(t *testing.T) {
	r := envelopeChain(domain.ProtoOpenAI, stubParser{out: domain.CanonicalRequest{Model: "gpt-4o"}})
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
	r := envelopeChain(domain.ProtoOpenAI, stubParser{})
	r.POST("/x", func(c *gin.Context) {
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

func TestEnvelope_RejectsMissingProtocolTag(t *testing.T) {
	// 路由忘挂 WithSourceProtocol → Envelope 应 500
	r := envelopeChain(domain.ProtoUnknown, stubParser{})
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "WithSourceProtocol") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestEnvelope_ParserError(t *testing.T) {
	r := envelopeChain(domain.ProtoOpenAI, stubParser{err: errors.New("bad json")})
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
