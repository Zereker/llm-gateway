package invoker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/selector"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// =============================================================================
// fakes
// =============================================================================

// testSender 把 Send 调用所需的 protocol.Handler 提前固化，避免每个测试都
// 重复写 5 参数。handler 可以是 protocol.Combine 出来的真组合，也可以是 nil
// （测试"handler 缺失"路径）。
type testSender struct {
	*Sender
	handler protocol.Handler
}

func (ts *testSender) Send(ctx context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope, body []byte) (Outcome, error) {
	return ts.Sender.Send(ctx, ep, env, body, ts.handler)
}


type fakeFactory struct {
	meta       protocol.Metadata
	classifier protocol.Classifier // optional
	buildErr   error
	sessionErr error
}

func (f *fakeFactory) Metadata() protocol.Metadata { return f.meta }

func (f *fakeFactory) NewSession(_ context.Context, ep *domain.Endpoint, _ *domain.RequestEnvelope) (protocol.Session, error) {
	if f.sessionErr != nil {
		return nil, f.sessionErr
	}
	return &fakeSession{buildErr: f.buildErr, ep: ep}, nil
}

// Classify：只在显式装了 classifier 时按它返；否则不实现 Classifier interface。
func (f *fakeFactory) Classify(httpStatus int, body []byte) *domain.AdapterError {
	if f.classifier == nil {
		return nil
	}
	return f.classifier.Classify(httpStatus, body)
}

type fakeSession struct {
	buildErr error
	ep       *domain.Endpoint
}

func (s *fakeSession) BuildRequest(body []byte, _ http.Header) (*http.Request, error) {
	if s.buildErr != nil {
		return nil, s.buildErr
	}
	return http.NewRequest("POST", s.ep.Routing.URL, strings.NewReader(string(body)))
}

func (s *fakeSession) Close() error { return nil }

type fakeTranslator struct {
	src, tgt     domain.Protocol
	translateErr error
}

func (t *fakeTranslator) Source() domain.Protocol { return t.src }
func (t *fakeTranslator) Target() domain.Protocol { return t.tgt }
func (t *fakeTranslator) TranslateRequest(srcBody []byte) ([]byte, error) {
	if t.translateErr != nil {
		return nil, t.translateErr
	}
	return srcBody, nil
}
func (t *fakeTranslator) NewResponseHandler() translator.ResponseHandler {
	return &fakeRespHandler{}
}

type fakeRespHandler struct {
	collected []byte
}

func (h *fakeRespHandler) Feed(chunk []byte) ([]byte, error) {
	h.collected = append(h.collected, chunk...)
	return chunk, nil
}
func (h *fakeRespHandler) Flush() ([]byte, *domain.Usage, error) {
	return nil, &domain.Usage{Input: 1, Output: 2}, nil
}

// stubClassifier 强制返回 ErrPermanent。
type stubClassifier struct{}

func (stubClassifier) Classify(_ int, _ []byte) *domain.AdapterError {
	return &domain.AdapterError{Class: domain.ErrPermanent}
}

// =============================================================================
// helpers
// =============================================================================

func newEnv() *domain.RequestEnvelope {
	return &domain.RequestEnvelope{
		RawBytes:       []byte(`{"model":"x"}`),
		Model:          "x",
		SourceProtocol: domain.ProtoOpenAI,
	}
}

func registerOpenAITranslator(t *testing.T, tr translator.Translator) {
	t.Helper()
	translator.Reset()
	translator.Register(tr)
	t.Cleanup(translator.Reset)
}

// newSender 构造 testSender。target 指明 factory HTTP 层 produces 的协议，
// 用于 translator.Find(OpenAI, target) 查找 translator。target == ProtoUnknown
// 或 factory == nil → handler = nil（测试 Send 的"无 handler"分支）。
func newSender(t *testing.T, factory protocol.Factory, target domain.Protocol, opts ...Option) *testSender {
	t.Helper()
	var h protocol.Handler
	if factory != nil && target != domain.ProtoUnknown {
		if tr := translator.Find(domain.ProtoOpenAI, target); tr != nil {
			h = protocol.Combine(factory, tr)
		}
	}
	return &testSender{Sender: New(opts...), handler: h}
}

// =============================================================================
// Send 用例
// =============================================================================

func TestSend_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	registerOpenAITranslator(t, &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})

	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, domain.ProtoOpenAI)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}
	ep.Routing.URL = srv.URL

	out, err := sender.Send(context.Background(), ep, newEnv(), []byte(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !out.Success() {
		t.Fatalf("not success: %+v", out)
	}
	if out.HTTPCode != 200 {
		t.Fatalf("code = %d", out.HTTPCode)
	}
	if out.Handler == nil {
		t.Fatalf("Handler missing on success")
	}
	_ = out.Response.Body.Close()
}

func TestSend_NoFactory(t *testing.T) {
	registerOpenAITranslator(t, &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})

	sender := newSender(t, nil, domain.ProtoUnknown)
	ep := &domain.Endpoint{ID: 1, Vendor: "noone"}

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err != nil || out.Class != selector.ClassPermanent {
		t.Fatalf("want Permanent / nil err; got class=%v err=%v", out.Class, err)
	}
	if out.Response != nil {
		t.Fatalf("Response must be nil on failure")
	}
}

func TestSend_NoTranslator(t *testing.T) {
	translator.Reset()
	t.Cleanup(translator.Reset)

	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, domain.ProtoAnthropic)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err != nil || out.Class != selector.ClassPermanent {
		t.Fatalf("want Permanent / nil err; got class=%v err=%v", out.Class, err)
	}
	// v0.6 融合后：translator 缺失 → composeHandler 返 nil → Send 报 "no handler"
	if !strings.Contains(out.Reason, "no handler") {
		t.Fatalf("reason = %q", out.Reason)
	}
}

func TestSend_TranslateRequestError(t *testing.T) {
	registerOpenAITranslator(t, &fakeTranslator{
		src:          domain.ProtoOpenAI,
		tgt:          domain.ProtoOpenAI,
		translateErr: fmt.Errorf("bad json"),
	})
	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, domain.ProtoOpenAI)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err == nil {
		t.Fatalf("want non-nil err to flag invalid request")
	}
	if out.Class != selector.ClassInvalid {
		t.Fatalf("want Invalid; got %v", out.Class)
	}
}

func TestSend_5xxClassifiedTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"err":"oops"}`))
	}))
	defer srv.Close()

	registerOpenAITranslator(t, &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})
	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, domain.ProtoOpenAI)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}
	ep.Routing.URL = srv.URL

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Class != selector.ClassTransient {
		t.Fatalf("want Transient; got %v", out.Class)
	}
	if out.Response != nil {
		t.Fatalf("Response must be nil on failure")
	}
	if out.HTTPCode != 500 {
		t.Fatalf("code = %d", out.HTTPCode)
	}
}

func TestSend_ClassifierRefinesTo429AsCapacity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"err":"insufficient_quota"}`))
	}))
	defer srv.Close()

	registerOpenAITranslator(t, &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})
	sender := newSender(t, &fakeFactory{
		meta:       protocol.Metadata{Vendor: "fakev"},
		classifier: stubClassifier{},
	}, domain.ProtoOpenAI)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}
	ep.Routing.URL = srv.URL

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Class != selector.ClassPermanent {
		t.Fatalf("want Permanent (classifier override); got %v", out.Class)
	}
}

func TestSend_NetworkError(t *testing.T) {
	registerOpenAITranslator(t, &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})
	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, domain.ProtoOpenAI)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}
	ep.Routing.URL = "http://127.0.0.1:1"

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err != nil {
		t.Fatalf("err should be nil for transport-level fail; got %v", err)
	}
	if out.Class != selector.ClassTransient {
		t.Fatalf("want Transient; got %v", out.Class)
	}
}

// 注入 HTTPDoer 测试：不实际起 server，只验依赖注入路径
type stubDoer struct {
	resp *http.Response
	err  error
	gotN int
}

func (s *stubDoer) Do(*http.Request) (*http.Response, error) {
	s.gotN++
	return s.resp, s.err
}

func TestSend_WithCustomHTTPDoer(t *testing.T) {
	registerOpenAITranslator(t, &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})

	doer := &stubDoer{
		resp: &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
	}
	sender := newSender(t,
		&fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}},
		domain.ProtoOpenAI,
		WithHTTPClient(doer),
	)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}
	ep.Routing.URL = "http://stub" // 不会真发，doer 接管

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !out.Success() {
		t.Fatalf("not success: %+v", out)
	}
	if doer.gotN != 1 {
		t.Fatalf("doer called %d times", doer.gotN)
	}
	_ = out.Response.Body.Close()
}

// =============================================================================
// Forward 用例
// =============================================================================

func TestForward_StreamsBodyToWriter(t *testing.T) {
	registerOpenAITranslator(t, &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})
	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, domain.ProtoOpenAI)

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("hello world")),
	}
	w := httptest.NewRecorder()
	var stream protocol.ResponseStream = &fakeRespHandler{}

	res := sender.Forward(context.Background(), w, &domain.Endpoint{ID: 99}, resp, stream)

	if res.FeedErr != nil {
		t.Fatalf("FeedErr = %v", res.FeedErr)
	}
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Body.String(); got != "hello world" {
		t.Fatalf("body = %q", got)
	}
	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type lost")
	}
	if res.Usage == nil || res.Usage.Input != 1 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestForward_StripsContentLength(t *testing.T) {
	registerOpenAITranslator(t, &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})
	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, domain.ProtoOpenAI)

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Length": []string{"42"}, "X-Custom": []string{"v"}},
		Body:       io.NopCloser(strings.NewReader("x")),
	}
	w := httptest.NewRecorder()
	var stream protocol.ResponseStream = &fakeRespHandler{}

	_ = sender.Forward(context.Background(), w, &domain.Endpoint{ID: 99}, resp, stream)

	if w.Header().Get("Content-Length") != "" {
		t.Fatalf("Content-Length should be stripped")
	}
	if w.Header().Get("X-Custom") != "v" {
		t.Fatalf("X-Custom lost")
	}
}

// =============================================================================
// 内部 helper 单元
// =============================================================================

func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		code int
		want selector.ErrorClass
	}{
		{200, selector.ClassSuccess},
		{299, selector.ClassSuccess},
		{401, selector.ClassPermanent},
		{403, selector.ClassPermanent},
		{429, selector.ClassCapacity},
		{500, selector.ClassTransient},
		{503, selector.ClassTransient},
		{400, selector.ClassInvalid},
		{404, selector.ClassInvalid},
		{100, selector.ClassUnknown},
	}
	for _, tc := range cases {
		if got := classifyHTTPStatus(tc.code); got != tc.want {
			t.Errorf("code=%d: got %v want %v", tc.code, got, tc.want)
		}
	}
}

// =============================================================================
// Hook 用例
// =============================================================================

// recordingHook 同时实现 5 个 Observer 接口，记录全部回调供断言。
type recordingHook struct {
	clientReq    [][]byte
	upstreamReq  [][]byte
	upstreamChk  [][]byte
	clientChk    [][]byte
	completes    []Outcome
	completedEPs []int64
}

func (h *recordingHook) OnClientRequest(_ context.Context, _ *domain.Endpoint, body []byte) {
	h.clientReq = append(h.clientReq, append([]byte(nil), body...))
}
func (h *recordingHook) OnUpstreamRequest(_ context.Context, _ *domain.Endpoint, body []byte) {
	h.upstreamReq = append(h.upstreamReq, append([]byte(nil), body...))
}
func (h *recordingHook) OnUpstreamChunk(_ context.Context, _ *domain.Endpoint, chunk []byte) {
	h.upstreamChk = append(h.upstreamChk, append([]byte(nil), chunk...))
}
func (h *recordingHook) OnClientChunk(_ context.Context, _ *domain.Endpoint, chunk []byte) {
	h.clientChk = append(h.clientChk, append([]byte(nil), chunk...))
}
func (h *recordingHook) OnAttemptComplete(_ context.Context, ep *domain.Endpoint, out Outcome) {
	h.completes = append(h.completes, out)
	h.completedEPs = append(h.completedEPs, ep.ID)
}

func TestHooks_FiredOnSuccessPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("hello stream"))
	}))
	defer srv.Close()

	registerOpenAITranslator(t, &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})

	hook := &recordingHook{}
	sender := newSender(t,
		&fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}},
		domain.ProtoOpenAI,
		WithHooks(hook),
	)
	ep := &domain.Endpoint{ID: 7, Vendor: "fakev"}
	ep.Routing.URL = srv.URL

	clientBody := []byte(`{"model":"x","msg":"原始客户端 body"}`)
	out, err := sender.Send(context.Background(), ep, newEnv(), clientBody)
	if err != nil || !out.Success() {
		t.Fatalf("Send failed: out=%+v err=%v", out, err)
	}

	// ClientRequest：在 Send 一开始 fire；body 是 caller 传进来的字节
	if len(hook.clientReq) != 1 || string(hook.clientReq[0]) != string(clientBody) {
		t.Fatalf("ClientRequest got %v", hook.clientReq)
	}
	// UpstreamRequest：identity translator 下与 ClientRequest 同
	if len(hook.upstreamReq) != 1 || string(hook.upstreamReq[0]) != string(clientBody) {
		t.Fatalf("UpstreamRequest got %v", hook.upstreamReq)
	}

	// 走 Forward 拿响应；之后再断言 chunk 类 hook
	w := httptest.NewRecorder()
	stream := out.Handler.NewResponseStream()
	res := sender.Forward(context.Background(), w, ep, out.Response, stream)
	if res.FeedErr != nil {
		t.Fatalf("FeedErr = %v", res.FeedErr)
	}

	if len(hook.upstreamChk) == 0 {
		t.Fatalf("UpstreamChunk never fired")
	}
	if len(hook.clientChk) == 0 {
		t.Fatalf("ClientChunk never fired")
	}
	// identity 路径下两侧 chunk 应该一致
	upJoined := joinChunks(hook.upstreamChk)
	clJoined := joinChunks(hook.clientChk)
	if upJoined != clJoined {
		t.Fatalf("identity translator: upstream=%q client=%q", upJoined, clJoined)
	}
	if upJoined != "hello stream" {
		t.Fatalf("chunks reassembled = %q", upJoined)
	}

	// AttemptComplete 触发一次（success）
	if len(hook.completes) != 1 || hook.completes[0].Class != selector.ClassSuccess {
		t.Fatalf("AttemptComplete got %v", hook.completes)
	}
	if hook.completedEPs[0] != 7 {
		t.Fatalf("completed ep id = %d", hook.completedEPs[0])
	}
}

func TestHooks_AttemptCompleteFiredOnFailure(t *testing.T) {
	registerOpenAITranslator(t, &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})

	hook := &recordingHook{}
	// 让 factory 返 nil 触发 Permanent 失败路径
	sender := newSender(t, nil, domain.ProtoUnknown, WithHooks(hook))
	ep := &domain.Endpoint{ID: 8, Vendor: "missing"}

	out, _ := sender.Send(context.Background(), ep, newEnv(), []byte("body"))

	// ClientRequest：早于 factory 查询，仍触发
	if len(hook.clientReq) != 1 {
		t.Fatalf("ClientRequest should fire even when factory missing; got %d", len(hook.clientReq))
	}
	// UpstreamRequest：factory 没找到走不到，不应触发
	if len(hook.upstreamReq) != 0 {
		t.Fatalf("UpstreamRequest must not fire when factory missing; got %d", len(hook.upstreamReq))
	}
	// AttemptComplete：失败路径必须触发，且 outcome 是 Permanent
	if len(hook.completes) != 1 || hook.completes[0].Class != selector.ClassPermanent {
		t.Fatalf("AttemptComplete on failure: %v", hook.completes)
	}
	if out.Class != selector.ClassPermanent {
		t.Fatalf("expected Permanent outcome")
	}
}

// 只实现部分接口的 Hook —— 验证 type-assert 分桶是否按需触发。
type onlyClientReqHook struct{ count int }

func (h *onlyClientReqHook) OnClientRequest(_ context.Context, _ *domain.Endpoint, _ []byte) {
	h.count++
}

func TestHooks_PartialInterfaceIsAllowed(t *testing.T) {
	registerOpenAITranslator(t, &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})

	partial := &onlyClientReqHook{}
	// factory 缺失走 Permanent 路径
	sender := newSender(t, nil, domain.ProtoUnknown, WithHooks(partial))
	ep := &domain.Endpoint{ID: 9}
	_, _ = sender.Send(context.Background(), ep, newEnv(), []byte("x"))

	if partial.count != 1 {
		t.Fatalf("only OnClientRequest should fire once; got %d", partial.count)
	}
}

func TestHooks_MultipleHooksFireInOrder(t *testing.T) {
	registerOpenAITranslator(t, &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI})

	var order []string
	mk := func(name string) Hook {
		return clientReqHookFunc(func(_ context.Context, _ *domain.Endpoint, _ []byte) {
			order = append(order, name)
		})
	}

	sender := newSender(t, nil, domain.ProtoUnknown, WithHooks(mk("a"), mk("b"), mk("c")))
	_, _ = sender.Send(context.Background(), &domain.Endpoint{ID: 1}, newEnv(), []byte("x"))

	if got := strings.Join(order, ","); got != "a,b,c" {
		t.Fatalf("hook order = %q want a,b,c", got)
	}
}

// clientReqHookFunc adapter：让 closure 直接当作 ClientRequestObserver。
type clientReqHookFunc func(ctx context.Context, ep *domain.Endpoint, body []byte)

func (f clientReqHookFunc) OnClientRequest(ctx context.Context, ep *domain.Endpoint, body []byte) {
	f(ctx, ep, body)
}

func joinChunks(chunks [][]byte) string {
	var sb strings.Builder
	for _, c := range chunks {
		sb.Write(c)
	}
	return sb.String()
}

func TestPeekBodyForClassify_PreservesBody(t *testing.T) {
	full := []byte(`{"err":"capacity_exceeded","retry_after":30}`)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(string(full)))}
	peeked := peekBodyForClassify(resp)
	if string(peeked) != string(full) {
		t.Fatalf("peeked = %q", string(peeked))
	}
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != string(full) {
		t.Fatalf("body after peek = %q want full = %q", string(rest), string(full))
	}
}
