package middleware

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/moderation"
)

// attachEnvelopeFor attaches an Envelope to the RC for Moderation tests
func attachEnvelopeFor(model string) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Envelope = &domain.RequestEnvelope{
			SourceProtocol: domain.ProtoOpenAI,
			Modality:       domain.ModalityChat,
			Model:          model,
			RawBytes:       []byte(`{"model":"` + model + `"}`),
		}
		c.Next()
	}
}

func TestModeration_NilModerator_PassThrough(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(),
		attachEnvelopeFor("x"),
		Moderation(WithModerator(nil)),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Errorf("status=%d, want=200", w.Code)
	}
}

func TestModeration_500_EnvelopeMissing(t *testing.T) {
	mod := &stubModerator{}
	r := newGinTest(TraceContext(), Recover(), Moderation(WithModerator(mod)))
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 500 {
		t.Fatalf("status=%d, want=500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "M3 Envelope did not run") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestModeration_CheckInputOK_InjectsModeratorInCtx(t *testing.T) {
	mod := &stubModerator{}
	var ctxMod Moderator
	r := newGinTest(TraceContext(), Recover(),
		attachEnvelopeFor("gpt-4o"),
		Moderation(WithModerator(mod)),
	)
	r.POST("/x", func(c *gin.Context) {
		ctxMod = moderation.FromContext(c.Request.Context())
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if ctxMod == nil {
		t.Fatal("Moderator not injected into ctx")
	}
	if mod.inputCalls.Load() != 1 {
		t.Errorf("CheckInput calls=%d, want=1", mod.inputCalls.Load())
	}
	if mod.lastInputModel != "gpt-4o" {
		t.Errorf("CheckInput got model=%q, want=gpt-4o", mod.lastInputModel)
	}
}

func TestModeration_CheckInputReject_400_Invalid(t *testing.T) {
	mod := &stubModerator{checkInputErr: errors.New("profanity detected")}
	r := newGinTest(TraceContext(), Recover(),
		attachEnvelopeFor("x"),
		Moderation(WithModerator(mod)),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 400 {
		t.Fatalf("status=%d, want=400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "content rejected") {
		t.Errorf("body=%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "profanity") {
		t.Errorf("body should wrap moderator err: %s", w.Body.String())
	}
}

// =============================================================================
// moderation_handler decorator: moderation.WrapStream / moderatedResponseHandler
// =============================================================================

// fakeHandler implements translator.ResponseHandler, returning the Feed input unchanged.
type fakeHandler struct {
	feeds   [][]byte
	flush   []byte
	usage   *domain.Usage
	feedErr error
}

func (h *fakeHandler) Feed(chunk []byte) ([]byte, error) {
	h.feeds = append(h.feeds, chunk)
	if h.feedErr != nil {
		return nil, h.feedErr
	}
	return chunk, nil
}
func (h *fakeHandler) Flush() ([]byte, *domain.Usage, error) {
	return h.flush, h.usage, nil
}

func TestWrapWithModerator_NoModeratorInCtx_ReturnsInner(t *testing.T) {
	inner := &fakeHandler{}
	got := moderation.WrapStream(context.TODO(), inner)
	if got != inner {
		t.Errorf("expected inner returned when ctx is nil")
	}
}

func TestModeratedResponseHandler_Feed_AbortsOnViolation(t *testing.T) {
	mod := &stubModerator{checkOutputErr: errors.New("hate speech")}
	inner := &fakeHandler{}
	ctx := moderation.ContextWithModerator(context.Background(), mod)
	h := moderation.WrapStream(ctx, inner)

	out, err := h.Feed([]byte("bad chunk"))
	if err == nil {
		t.Fatal("expected error")
	}
	if len(out) != 0 {
		t.Errorf("violated bytes should be dropped, got=%q", string(out))
	}

	// subsequent Feed calls short-circuit
	out2, err2 := h.Feed([]byte("more"))
	if err2 == nil || !errors.Is(err2, moderation.ErrViolated) {
		t.Errorf("subsequent Feed should short-circuit with moderation.ErrViolated, got=%v", err2)
	}
	if len(out2) != 0 {
		t.Errorf("short-circuit should return no bytes, got=%q", string(out2))
	}
}

func TestModeratedResponseHandler_Feed_PassThroughOnOK(t *testing.T) {
	mod := &stubModerator{}
	inner := &fakeHandler{}
	ctx := moderation.ContextWithModerator(context.Background(), mod)
	h := moderation.WrapStream(ctx, inner)

	out, err := h.Feed([]byte("clean"))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if string(out) != "clean" {
		t.Errorf("out=%q", string(out))
	}
	if mod.outputCalls.Load() != 1 {
		t.Errorf("CheckOutput calls=%d, want=1", mod.outputCalls.Load())
	}
}

func TestModeratedResponseHandler_Flush_RunsCheckOutputOnFinal(t *testing.T) {
	mod := &stubModerator{}
	inner := &fakeHandler{flush: []byte("final bytes"), usage: &domain.Usage{Total: 50}}
	ctx := moderation.ContextWithModerator(context.Background(), mod)
	h := moderation.WrapStream(ctx, inner)

	out, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if string(out) != "final bytes" {
		t.Errorf("out=%q", string(out))
	}
	if usage == nil || usage.Total != 50 {
		t.Errorf("usage=%+v", usage)
	}
	if mod.outputCalls.Load() != 1 {
		t.Errorf("Flush should call CheckOutput once, got=%d", mod.outputCalls.Load())
	}
}

func TestModeratedResponseHandler_Flush_ViolatedFromStream_DropsFinal(t *testing.T) {
	mod := &stubModerator{checkOutputErr: errors.New("violated")}
	inner := &fakeHandler{flush: []byte("never_sent"), usage: &domain.Usage{Total: 1}}
	ctx := moderation.ContextWithModerator(context.Background(), mod)
	h := moderation.WrapStream(ctx, inner)

	// first, Feed triggers a violation
	_, _ = h.Feed([]byte("bad"))

	out, _, err := h.Flush()
	if err == nil || !errors.Is(err, moderation.ErrViolated) {
		t.Errorf("Flush should return moderation.ErrViolated, got=%v", err)
	}
	if len(out) != 0 {
		t.Errorf("final bytes should be dropped, got=%q", string(out))
	}
}
