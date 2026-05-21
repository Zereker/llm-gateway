package protocol_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// =============================================================================
// fakes
// =============================================================================

type fakeAdapter struct {
	meta         adapter.Metadata
	sessionErr   error
	buildErr     error
	classifyImpl func(int, []byte) *domain.AdapterError
}

func (f *fakeAdapter) Metadata() adapter.Metadata { return f.meta }

func (f *fakeAdapter) NewSession(_ context.Context, _ *domain.Endpoint, env *domain.RequestEnvelope) (adapter.Session, error) {
	if f.sessionErr != nil {
		return nil, f.sessionErr
	}
	return &fakeSession{buildErr: f.buildErr, gotEnv: env}, nil
}

// Classify makes fakeAdapter satisfy adapter.Classifier *iff* classifyImpl is set；
// 通过 wrapper 类型避免无意 satisfy（让 TestCombine_Classify_NonClassifier 拿到一个"不实现 Classifier"的 fakeAdapter）。
func newClassifierFakeAdapter(meta adapter.Metadata, classifyImpl func(int, []byte) *domain.AdapterError) *classifierFakeAdapter {
	return &classifierFakeAdapter{fakeAdapter: fakeAdapter{meta: meta, classifyImpl: classifyImpl}}
}

type classifierFakeAdapter struct{ fakeAdapter }

func (c *classifierFakeAdapter) Classify(status int, body []byte) *domain.AdapterError {
	if c.classifyImpl == nil {
		return nil
	}
	return c.classifyImpl(status, body)
}

type fakeSession struct {
	buildErr error
	gotEnv   *domain.RequestEnvelope
	closed   bool
}

func (s *fakeSession) BuildRequest(body []byte) (*http.Request, error) {
	if s.buildErr != nil {
		return nil, s.buildErr
	}
	return http.NewRequest("POST", "http://upstream.test/v1/chat", strings.NewReader(string(body)))
}

func (s *fakeSession) Close() error {
	s.closed = true
	return nil
}

type fakeTranslator struct {
	src, tgt      domain.Protocol
	translateErr  error
	upstreamBody  []byte // 自定义翻译后的字节；不设则原样返
	respHandlerFn func() translator.ResponseHandler
}

func (t *fakeTranslator) Source() domain.Protocol { return t.src }
func (t *fakeTranslator) Target() domain.Protocol { return t.tgt }
func (t *fakeTranslator) TranslateRequest(srcBody []byte) ([]byte, error) {
	if t.translateErr != nil {
		return nil, t.translateErr
	}
	if t.upstreamBody != nil {
		return t.upstreamBody, nil
	}
	return srcBody, nil
}
func (t *fakeTranslator) NewResponseHandler() translator.ResponseHandler {
	if t.respHandlerFn != nil {
		return t.respHandlerFn()
	}
	return &fakeRespHandler{}
}

type fakeRespHandler struct {
	feeds      [][]byte
	feedOut    []byte // Feed 返回的字节（默认 = 输入透传）
	feedErr    error
	flushOut   []byte
	flushUsage *domain.Usage
	flushErr   error
}

func (h *fakeRespHandler) Feed(chunk []byte) ([]byte, error) {
	h.feeds = append(h.feeds, append([]byte(nil), chunk...))
	if h.feedErr != nil {
		return nil, h.feedErr
	}
	if h.feedOut != nil {
		return h.feedOut, nil
	}
	return chunk, nil
}

func (h *fakeRespHandler) Flush() ([]byte, *domain.Usage, error) {
	return h.flushOut, h.flushUsage, h.flushErr
}

// =============================================================================
// Combine
// =============================================================================

func TestCombine_HappyPath_BuildsCallWithBothPhases(t *testing.T) {
	upBody := []byte(`{"upstream":"yes"}`)
	tr := &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoAnthropic, upstreamBody: upBody}
	ad := &fakeAdapter{meta: adapter.Metadata{Vendor: "anthropic", SupportedModalities: []domain.Modality{domain.ModalityChat}}}

	h := protocol.Combine(ad, tr)
	if h == nil {
		t.Fatal("Combine returned nil")
	}

	caps := h.Capabilities()
	if caps.SourceProtocol != domain.ProtoOpenAI {
		t.Errorf("SourceProtocol = %v, want OpenAI", caps.SourceProtocol)
	}
	if caps.UpstreamProtocol != domain.ProtoAnthropic {
		t.Errorf("UpstreamProtocol = %v, want Anthropic", caps.UpstreamProtocol)
	}
	if len(caps.SupportedModalities) != 1 || caps.SupportedModalities[0] != domain.ModalityChat {
		t.Errorf("SupportedModalities = %v", caps.SupportedModalities)
	}

	ep := &domain.Endpoint{ID: 1, Vendor: "anthropic", Protocol: domain.ProtoAnthropic}
	call, err := h.PrepareCall(context.Background(), ep, []byte(`{"client":"req"}`))
	if err != nil {
		t.Fatalf("PrepareCall err = %v", err)
	}
	if call == nil || call.Request == nil {
		t.Fatal("Call.Request nil")
	}
	if string(call.UpstreamBody) != string(upBody) {
		t.Errorf("UpstreamBody = %q, want %q", call.UpstreamBody, upBody)
	}
	gotBody, _ := io.ReadAll(call.Request.Body)
	if string(gotBody) != string(upBody) {
		t.Errorf("Request.Body = %q, want %q", gotBody, upBody)
	}
	if call.Request.URL.String() != "http://upstream.test/v1/chat" {
		t.Errorf("Request.URL = %s", call.Request.URL)
	}
}

func TestCombine_TranslateError_ReturnsPrepareErrorPhaseTranslate(t *testing.T) {
	tr := &fakeTranslator{
		src: domain.ProtoOpenAI, tgt: domain.ProtoAnthropic,
		translateErr: errors.New("bad json"),
	}
	ad := &fakeAdapter{meta: adapter.Metadata{Vendor: "anthropic"}}
	h := protocol.Combine(ad, tr)

	_, err := h.PrepareCall(context.Background(), &domain.Endpoint{Vendor: "anthropic"}, []byte("{"))
	if err == nil {
		t.Fatal("want error")
	}
	var pe *protocol.PrepareError
	if !errors.As(err, &pe) {
		t.Fatalf("want PrepareError, got %T", err)
	}
	if pe.Phase != protocol.PhaseTranslate {
		t.Errorf("Phase = %v, want PhaseTranslate", pe.Phase)
	}
	if !errors.Is(pe, pe.Err) || pe.Err.Error() != "bad json" {
		t.Errorf("inner err lost: %v", pe.Err)
	}
}

func TestCombine_NewSessionError_ReturnsPrepareErrorPhaseBuild(t *testing.T) {
	tr := &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI}
	ad := &fakeAdapter{meta: adapter.Metadata{Vendor: "openai"}, sessionErr: errors.New("session init failed")}
	h := protocol.Combine(ad, tr)

	_, err := h.PrepareCall(context.Background(), &domain.Endpoint{Vendor: "openai"}, []byte("{}"))
	var pe *protocol.PrepareError
	if !errors.As(err, &pe) || pe.Phase != protocol.PhaseBuild {
		t.Fatalf("want PrepareError{Phase:Build}, got %v", err)
	}
}

func TestCombine_BuildRequestError_ReturnsPrepareErrorPhaseBuild(t *testing.T) {
	tr := &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI}
	ad := &fakeAdapter{meta: adapter.Metadata{Vendor: "openai"}, buildErr: errors.New("bad routing url")}
	h := protocol.Combine(ad, tr)

	_, err := h.PrepareCall(context.Background(), &domain.Endpoint{Vendor: "openai"}, []byte("{}"))
	var pe *protocol.PrepareError
	if !errors.As(err, &pe) || pe.Phase != protocol.PhaseBuild {
		t.Fatalf("want PrepareError{Phase:Build}, got %v", err)
	}
}

func TestCombine_PassesEnvelopeToSession(t *testing.T) {
	tr := &fakeTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoAnthropic}
	// 包一层观察 NewSession 收到的 envelope
	var gotEnv *domain.RequestEnvelope
	ad := &observingAdapter{
		meta: adapter.Metadata{Vendor: "anthropic"},
		onNewSession: func(env *domain.RequestEnvelope) {
			gotEnv = env
		},
	}
	h := protocol.Combine(ad, tr)

	src := []byte(`{"orig":"body"}`)
	if _, err := h.PrepareCall(context.Background(), &domain.Endpoint{Vendor: "anthropic"}, src); err != nil {
		t.Fatalf("PrepareCall: %v", err)
	}
	if gotEnv == nil {
		t.Fatal("NewSession not called")
	}
	if gotEnv.SourceProtocol != domain.ProtoAnthropic {
		t.Errorf("envelope.SourceProtocol = %v, want Anthropic (from translator.Source)", gotEnv.SourceProtocol)
	}
	if string(gotEnv.RawBytes) != string(src) {
		t.Errorf("envelope.RawBytes = %q, want orig srcBody", gotEnv.RawBytes)
	}
}

// observingAdapter 观察 NewSession 收到的 envelope 参数。
type observingAdapter struct {
	meta         adapter.Metadata
	onNewSession func(env *domain.RequestEnvelope)
}

func (a *observingAdapter) Metadata() adapter.Metadata { return a.meta }
func (a *observingAdapter) NewSession(_ context.Context, _ *domain.Endpoint, env *domain.RequestEnvelope) (adapter.Session, error) {
	if a.onNewSession != nil {
		a.onNewSession(env)
	}
	return &fakeSession{}, nil
}

func TestCombine_Classify_PassthroughToAdapter(t *testing.T) {
	want := &domain.AdapterError{Class: domain.ErrPermanent, Code: domain.ErrCodeUpstreamError}
	ad := newClassifierFakeAdapter(
		adapter.Metadata{Vendor: "openai"},
		func(_ int, _ []byte) *domain.AdapterError { return want },
	)
	tr := &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI}
	h := protocol.Combine(ad, tr)

	cls, ok := h.(protocol.Classifier)
	if !ok {
		t.Fatal("combined handler does not satisfy protocol.Classifier")
	}
	got := cls.Classify(500, []byte(`{}`))
	if got != want {
		t.Errorf("Classify got %+v, want %+v", got, want)
	}
}

func TestCombine_Classify_NonClassifierAdapter_ReturnsNil(t *testing.T) {
	// fakeAdapter (no Classify method) → Handler.Classify 返回 nil
	ad := &fakeAdapter{meta: adapter.Metadata{Vendor: "openai"}}
	tr := &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI}
	h := protocol.Combine(ad, tr)

	cls, ok := h.(protocol.Classifier)
	if !ok {
		t.Fatal("combined handler should satisfy protocol.Classifier interface (even if pass-through)")
	}
	if got := cls.Classify(500, []byte(`{}`)); got != nil {
		t.Errorf("Classify = %+v, want nil (adapter not Classifier)", got)
	}
}

func TestCombine_PanicsOnNilAdapterOrTranslator(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want panic on nil adapter")
		}
	}()
	protocol.Combine(nil, &fakeTranslator{})
}

func TestCombine_PanicsOnNilTranslator(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want panic on nil translator")
		}
	}()
	protocol.Combine(&fakeAdapter{}, nil)
}

// =============================================================================
// ResponseStream
// =============================================================================

func TestCombine_NewResponseStream_ForwardsFeedAndFlush(t *testing.T) {
	innerOut := []byte("translated chunk")
	innerUsage := &domain.Usage{Input: 10, Output: 20, Total: 30}
	inner := &fakeRespHandler{feedOut: innerOut, flushOut: []byte("flushed"), flushUsage: innerUsage}

	tr := &fakeTranslator{
		src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI,
		respHandlerFn: func() translator.ResponseHandler { return inner },
	}
	ad := &fakeAdapter{meta: adapter.Metadata{Vendor: "openai"}}
	h := protocol.Combine(ad, tr)

	stream := h.NewResponseStream()
	out, err := stream.Feed([]byte("up chunk"))
	if err != nil {
		t.Fatalf("Feed err = %v", err)
	}
	if string(out) != string(innerOut) {
		t.Errorf("Feed out = %q, want %q", out, innerOut)
	}
	if len(inner.feeds) != 1 || string(inner.feeds[0]) != "up chunk" {
		t.Errorf("inner feeds = %v", inner.feeds)
	}

	flushed, usage, err := stream.Flush()
	if err != nil {
		t.Fatalf("Flush err = %v", err)
	}
	if string(flushed) != "flushed" {
		t.Errorf("Flush out = %q", flushed)
	}
	if usage != innerUsage {
		t.Errorf("Flush usage = %+v, want %+v", usage, innerUsage)
	}
}

// =============================================================================
// DefaultLookup
// =============================================================================
//
// DefaultLookup 走全局 adapter + translator registry——测试需要 reset + 注册。

func TestDefaultLookup_Get_Composes_AdapterPlusTranslator(t *testing.T) {
	resetGlobalRegistries(t)

	ad := &fakeAdapter{meta: adapter.Metadata{Vendor: "myv"}}
	adapter.Register("myv", ad)
	translator.Register(&fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoAnthropic})

	ep := &domain.Endpoint{Vendor: "myv", Protocol: domain.ProtoAnthropic}
	h := protocol.DefaultLookup{}.Get(ep, domain.ProtoOpenAI)
	if h == nil {
		t.Fatal("DefaultLookup.Get returned nil")
	}
	caps := h.Capabilities()
	if caps.SourceProtocol != domain.ProtoOpenAI || caps.UpstreamProtocol != domain.ProtoAnthropic {
		t.Errorf("Capabilities mismatch: src=%v tgt=%v", caps.SourceProtocol, caps.UpstreamProtocol)
	}
}

func TestDefaultLookup_Get_NilEndpoint_ReturnsNil(t *testing.T) {
	if h := (protocol.DefaultLookup{}).Get(nil, domain.ProtoOpenAI); h != nil {
		t.Errorf("nil endpoint should yield nil handler; got %v", h)
	}
}

func TestDefaultLookup_Get_ProtoUnknown_ReturnsNil(t *testing.T) {
	resetGlobalRegistries(t)
	adapter.Register("myv", &fakeAdapter{meta: adapter.Metadata{Vendor: "myv"}})

	ep := &domain.Endpoint{Vendor: "myv"} // Protocol 零值 = ProtoUnknown
	if h := (protocol.DefaultLookup{}).Get(ep, domain.ProtoOpenAI); h != nil {
		t.Errorf("ProtoUnknown ep should yield nil handler; got %v", h)
	}
}

func TestDefaultLookup_Get_NoAdapter_ReturnsNil(t *testing.T) {
	resetGlobalRegistries(t)
	translator.Register(&fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})

	ep := &domain.Endpoint{Vendor: "missing", Protocol: domain.ProtoOpenAI}
	if h := (protocol.DefaultLookup{}).Get(ep, domain.ProtoOpenAI); h != nil {
		t.Errorf("missing adapter should yield nil handler; got %v", h)
	}
}

func TestDefaultLookup_Get_NoTranslator_ReturnsNil(t *testing.T) {
	resetGlobalRegistries(t)
	adapter.Register("myv", &fakeAdapter{meta: adapter.Metadata{Vendor: "myv"}})
	// 注册一个 (Anthropic → Anthropic) translator，但 caller 找 (OpenAI → Anthropic)
	translator.Register(&fakeTranslator{src: domain.ProtoAnthropic, tgt: domain.ProtoAnthropic})

	ep := &domain.Endpoint{Vendor: "myv", Protocol: domain.ProtoAnthropic}
	if h := (protocol.DefaultLookup{}).Get(ep, domain.ProtoOpenAI); h != nil {
		t.Errorf("missing translator should yield nil handler; got %v", h)
	}
}

// =============================================================================
// PrepareError
// =============================================================================

func TestPrepareError_ErrorMessageAndUnwrap(t *testing.T) {
	inner := errors.New("inner detail")
	pe := protocol.NewPrepareError(protocol.PhaseTranslate, inner)
	if !strings.Contains(pe.Error(), "translate") || !strings.Contains(pe.Error(), "inner detail") {
		t.Errorf("Error() = %q, want to contain phase + detail", pe.Error())
	}
	if !errors.Is(pe, inner) {
		t.Errorf("errors.Is(pe, inner) = false; want true via Unwrap")
	}
}

func TestPrepareError_PhaseString(t *testing.T) {
	cases := []struct {
		phase protocol.PreparePhase
		want  string
	}{
		{protocol.PhaseTranslate, "translate"},
		{protocol.PhaseBuild, "build"},
		{protocol.PreparePhase(99), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.phase.String(); got != tc.want {
				t.Errorf("phase=%d got=%q want=%q", tc.phase, got, tc.want)
			}
		})
	}
}

func TestIsPrepareError(t *testing.T) {
	plain := errors.New("not a prepare error")
	if protocol.IsPrepareError(plain) {
		t.Error("plain error should not satisfy IsPrepareError")
	}
	pe := protocol.NewPrepareError(protocol.PhaseBuild, errors.New("x"))
	if !protocol.IsPrepareError(pe) {
		t.Error("PrepareError should satisfy IsPrepareError")
	}
	// 嵌套也应该被识别
	wrapped := errors.Join(errors.New("layer1"), pe)
	if !protocol.IsPrepareError(wrapped) {
		t.Error("wrapped PrepareError should satisfy IsPrepareError")
	}
}

// =============================================================================
// helpers
// =============================================================================

// resetGlobalRegistries 清空两个全局 registry（adapter + translator）；
// 测试 setup + cleanup 用。
func resetGlobalRegistries(t *testing.T) {
	t.Helper()
	adapter.Reset()
	translator.Reset()
	t.Cleanup(func() {
		adapter.Reset()
		translator.Reset()
	})
}
