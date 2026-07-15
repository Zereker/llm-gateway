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
	"github.com/zereker/llm-gateway/internal/policy"
)

type policyEngineFunc func(context.Context, policy.EvaluationInput) (policy.Decision, error)

func (f policyEngineFunc) Evaluate(ctx context.Context, input policy.EvaluationInput) (policy.Decision, error) {
	return f(ctx, input)
}

type policyResolverFunc func(context.Context, policy.Subject) (*policy.Definition, error)

func (f policyResolverFunc) Resolve(ctx context.Context, subject policy.Subject) (*policy.Definition, error) {
	return f(ctx, subject)
}

type capturedPolicyAudit struct {
	name    string
	payload any
}

type capturePolicyAuditTracer struct {
	logs []capturedPolicyAudit
}

func (t *capturePolicyAuditTracer) Log(_ context.Context, name string, payload any) {
	t.logs = append(t.logs, capturedPolicyAudit{name: name, payload: payload})
}

func middlewarePolicyDecision(action policy.Action) policy.Decision {
	decision := policy.Decision{
		Action: action,
		Policy: policy.PolicyRef{ID: "middleware-test", Version: 1, Scope: policy.Scope{Kind: policy.ScopeGlobal}},
		RuleID: "test-rule", ReasonCode: "test-reason",
	}
	if action == policy.ActionRedact {
		decision.Mutations = []policy.Mutation{{ID: "mask", Kind: policy.MutationRedact, Target: "request.body", Replacement: []byte("secret")}}
	}

	return decision
}

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
	if strings.Contains(w.Body.String(), "profanity") {
		t.Errorf("body leaked moderator detail: %s", w.Body.String())
	}
}

func TestModerationPolicyEngineAllowRecordsInputAndOutputAudit(t *testing.T) {
	tracer := &capturePolicyAuditTracer{}
	engine := policyEngineFunc(func(_ context.Context, input policy.EvaluationInput) (policy.Decision, error) {
		if input.Model != "gpt-4o" || input.Modality != domain.ModalityChat {
			t.Fatalf("input metadata=%+v", input)
		}

		return middlewarePolicyDecision(policy.ActionAllow), nil
	})
	r := newGinTest(TraceContext(), Recover(), attachEnvelopeFor("gpt-4o"), Moderation(
		WithPolicyEngine(engine),
		WithPolicyAuditTracer(tracer),
	))
	r.POST("/x", func(c *gin.Context) {
		mod := moderation.FromContext(c.Request.Context())
		if mod == nil {
			t.Fatal("output policy adapter was not injected")
		}
		if err := mod.CheckOutput(c.Request.Context(), []byte("clean")); err != nil {
			t.Fatal(err)
		}
		if len(tracer.logs) != 0 {
			t.Fatalf("policy audits must flush after c.Next: %+v", tracer.logs)
		}
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(tracer.logs) != 2 {
		t.Fatalf("audits=%+v", tracer.logs)
	}
	inputAudit, inputOK := tracer.logs[0].payload.(policy.AuditRecord)
	outputAudit, outputOK := tracer.logs[1].payload.(policy.AuditRecord)
	if tracer.logs[0].name != "policy_decision" || tracer.logs[1].name != "policy_decision" ||
		!inputOK || !outputOK || inputAudit.Stage != policy.StageInput || outputAudit.Stage != policy.StageOutput {
		t.Fatalf("audits=%+v", tracer.logs)
	}
}

func TestModerationPolicyEngineDeniesWithoutLeakingInternalReason(t *testing.T) {
	tracer := &capturePolicyAuditTracer{}
	engine := policyEngineFunc(func(context.Context, policy.EvaluationInput) (policy.Decision, error) {
		decision := middlewarePolicyDecision(policy.ActionDeny)
		decision.ReasonCode = "private-rule-name"

		return decision, nil
	})
	r := newGinTest(TraceContext(), Recover(), attachEnvelopeFor("x"), Moderation(
		WithPolicyEngine(engine),
		WithPolicyAuditTracer(tracer),
	))
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 400 || strings.Contains(w.Body.String(), "private-rule-name") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(tracer.logs) != 1 {
		t.Fatalf("deny audit was not flushed: %+v", tracer.logs)
	}
	audit, ok := tracer.logs[0].payload.(policy.AuditRecord)
	if !ok || audit.Action != policy.ActionDeny || audit.ReasonCode != "private-rule-name" || audit.Enforcement != policy.EnforcementDenied {
		t.Fatalf("deny audit=%+v", tracer.logs[0])
	}
}

func TestModerationEngineErrorDoesNotExposeCause(t *testing.T) {
	secret := "detector matched customer card 4111"
	engine := policyEngineFunc(func(context.Context, policy.EvaluationInput) (policy.Decision, error) {
		return policy.Decision{}, errors.New(secret)
	})
	r := newGinTest(TraceContext(), Recover(), attachEnvelopeFor("x"), Moderation(WithPolicyEngine(engine)))
	r.POST("/x", func(c *gin.Context) { c.Status(200) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), "policy engine unavailable") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestModerationPolicyEngineFailuresAreClosed(t *testing.T) {
	tests := map[string]policyEngineFunc{
		"engine error": func(context.Context, policy.EvaluationInput) (policy.Decision, error) {
			return policy.Decision{}, errors.New("dependency failed")
		},
		"invalid decision": func(context.Context, policy.EvaluationInput) (policy.Decision, error) {
			return policy.Decision{}, nil
		},
		"redact without executor": func(context.Context, policy.EvaluationInput) (policy.Decision, error) {
			return middlewarePolicyDecision(policy.ActionRedact), nil
		},
	}

	for name, engine := range tests {
		t.Run(name, func(t *testing.T) {
			r := newGinTest(TraceContext(), Recover(), attachEnvelopeFor("x"), Moderation(WithPolicyEngine(engine)))
			r.POST("/x", func(c *gin.Context) { c.Status(200) })

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
			if w.Code != 503 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestModerationAppliesInputRedactionBeforeDownstream(t *testing.T) {
	definition := &policy.Definition{
		Ref:  policy.PolicyRef{ID: "pii", Version: 2, Scope: policy.Scope{Kind: policy.ScopeAccount, ID: "a1"}},
		Name: "PII", InputEnabled: true, OutputMode: policy.OutputDisabled,
	}
	resolver := policyResolverFunc(func(context.Context, policy.Subject) (*policy.Definition, error) {
		return definition, nil
	})
	engine := policyEngineFunc(func(_ context.Context, input policy.EvaluationInput) (policy.Decision, error) {
		if len(input.Segments) != 1 || input.Segments[0].Target != "/messages/0/content" {
			t.Fatalf("segments=%+v", input.Segments)
		}
		decision := middlewarePolicyDecision(policy.ActionRedact)
		decision.Mutations[0].Target = "/messages/0/content"
		decision.Mutations[0].Replacement = []byte("card [MASKED]")

		return decision, nil
	})
	r := newGinTest(TraceContext(), Recover(), attachEnvelopeFor("gpt-4o"), func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Identity.AccountID = "a1"
		rc.Envelope.RawBytes = []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"card 4111"}]}`)
		c.Next()
	}, Moderation(WithPolicyEngine(engine), WithPolicyResolver(resolver)))
	r.POST("/x", func(c *gin.Context) {
		body := string(GetRequestContext(c).Envelope.RawBytes)
		if strings.Contains(body, "4111") || !strings.Contains(body, "[MASKED]") {
			t.Fatalf("downstream body=%s", body)
		}
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 || w.Header().Get(HeaderGatewayPolicyOutputMode) != string(policy.OutputDisabled) ||
		w.Header().Get(HeaderGatewayPolicyID) != "pii@2" {
		t.Fatalf("status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
	}
}

func TestModerationBoundPolicyWithoutEngineFailsClosed(t *testing.T) {
	resolver := policyResolverFunc(func(context.Context, policy.Subject) (*policy.Definition, error) {
		return &policy.Definition{
			Ref:  policy.PolicyRef{ID: "p", Version: 1, Scope: policy.Scope{Kind: policy.ScopeGlobal}},
			Name: "bound", InputEnabled: true, OutputMode: policy.OutputDisabled,
		}, nil
	})
	r := newGinTest(TraceContext(), Recover(), attachEnvelopeFor("x"), Moderation(WithPolicyResolver(resolver)))
	r.POST("/x", func(c *gin.Context) { c.Status(200) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 503 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
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
