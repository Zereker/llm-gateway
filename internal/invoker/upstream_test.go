package invoker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
	"github.com/zereker/llm-gateway/internal/translator"
)

// =============================================================================
// fakes
// =============================================================================

// testSender fixes the protocol.Handler needed for a Send call up front, so
// each test doesn't have to repeat all 5 parameters. handler can be a real
// composition produced by protocol.Combine, or nil (to test the "missing
// handler" path).
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

// Classify: returns via the classifier only when one is explicitly set;
// otherwise it does not implement the Classifier interface.
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

// stubClassifier always returns ErrPermanent.
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

// newSender constructs a testSender. tr is the translator available to this
// call; target is the protocol the factory's HTTP layer produces, looked up as
// (OpenAI, target) in an isolated registry built from tr. target == ProtoUnknown,
// factory == nil, or no matching translator → handler = nil (to test Send's "no
// handler" branch).
func newSender(t *testing.T, factory protocol.Factory, tr translator.Translator, target domain.Protocol, opts ...Option) *testSender {
	t.Helper()
	var h protocol.Handler
	if factory != nil && target != domain.ProtoUnknown && tr != nil {
		if found := translator.NewRegistry(tr).Find(domain.ProtoOpenAI, target); found != nil {
			h = protocol.Combine(factory, found)
		}
	}
	return &testSender{Sender: New(opts...), handler: h}
}

// openAIIdentityTranslator is the translator used by most Send tests: OpenAI in,
// OpenAI out (identity).
func openAIIdentityTranslator() translator.Translator {
	return &fakeTranslator{src: domain.ProtoOpenAI, tgt: domain.ProtoOpenAI}
}

// =============================================================================
// Send test cases
// =============================================================================

func TestSend_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, openAIIdentityTranslator(), domain.ProtoOpenAI)
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
	sender := newSender(t, nil, nil, domain.ProtoUnknown)
	ep := &domain.Endpoint{ID: 1, Vendor: "noone"}

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err != nil || out.Class != ClassPermanent {
		t.Fatalf("want Permanent / nil err; got class=%v err=%v", out.Class, err)
	}
	if out.Response != nil {
		t.Fatalf("Response must be nil on failure")
	}
}

func TestSend_NoTranslator(t *testing.T) {
	// registry only has (OpenAI → OpenAI); the endpoint speaks Anthropic, so no
	// (OpenAI → Anthropic) translator is reachable → nil handler.
	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, openAIIdentityTranslator(), domain.ProtoAnthropic)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err != nil || out.Class != ClassPermanent {
		t.Fatalf("want Permanent / nil err; got class=%v err=%v", out.Class, err)
	}
	// After the v0.6 merge: missing translator → composeHandler returns nil → Send reports "no handler"
	if !strings.Contains(out.Reason, "no handler") {
		t.Fatalf("reason = %q", out.Reason)
	}
}

func TestSend_TranslateRequestError(t *testing.T) {
	tr := &fakeTranslator{
		src:          domain.ProtoOpenAI,
		tgt:          domain.ProtoOpenAI,
		translateErr: fmt.Errorf("bad json"),
	}
	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, tr, domain.ProtoOpenAI)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err == nil {
		t.Fatalf("want non-nil err to flag invalid request")
	}
	if out.Class != ClassInvalid {
		t.Fatalf("want Invalid; got %v", out.Class)
	}
}

func TestSend_5xxClassifiedTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"err":"oops"}`))
	}))
	defer srv.Close()

	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, openAIIdentityTranslator(), domain.ProtoOpenAI)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}
	ep.Routing.URL = srv.URL

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Class != ClassTransient {
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

	sender := newSender(t, &fakeFactory{
		meta:       protocol.Metadata{Vendor: "fakev"},
		classifier: stubClassifier{},
	}, openAIIdentityTranslator(), domain.ProtoOpenAI)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}
	ep.Routing.URL = srv.URL

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Class != ClassPermanent {
		t.Fatalf("want Permanent (classifier override); got %v", out.Class)
	}
}

func TestSend_NetworkError(t *testing.T) {
	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, openAIIdentityTranslator(), domain.ProtoOpenAI)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}
	ep.Routing.URL = "http://127.0.0.1:1"

	out, err := sender.Send(context.Background(), ep, newEnv(), nil)
	if err != nil {
		t.Fatalf("err should be nil for transport-level fail; got %v", err)
	}
	if out.Class != ClassTransient {
		t.Fatalf("want Transient; got %v", out.Class)
	}
}

// Test injecting HTTPDoer: doesn't actually start a server, only verifies
// the dependency-injection path.
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
	doer := &stubDoer{
		resp: &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
	}
	sender := newSender(t,
		&fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}},
		openAIIdentityTranslator(),
		domain.ProtoOpenAI,
		WithHTTPClient(doer),
	)
	ep := &domain.Endpoint{ID: 1, Vendor: "fakev"}
	ep.Routing.URL = "http://stub" // never actually sent, the doer takes over

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
// Forward test cases
// =============================================================================

func TestForward_StreamsBodyToWriter(t *testing.T) {
	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, openAIIdentityTranslator(), domain.ProtoOpenAI)

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
	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, openAIIdentityTranslator(), domain.ProtoOpenAI)

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
// Internal helper units
// =============================================================================

func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		code int
		want Class
	}{
		{200, ClassSuccess},
		{299, ClassSuccess},
		{401, ClassPermanent},
		{403, ClassPermanent},
		{429, ClassCapacity},
		{500, ClassTransient},
		{503, ClassTransient},
		{400, ClassInvalid},
		{404, ClassInvalid},
		{100, ClassUnknown},
	}
	for _, tc := range cases {
		if got := classifyHTTPStatus(tc.code); got != tc.want {
			t.Errorf("code=%d: got %v want %v", tc.code, got, tc.want)
		}
	}
}

// =============================================================================
// Hook test cases
// =============================================================================

// recordingHook implements all 5 Observer interfaces at once, recording
// every callback for assertions.
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

	hook := &recordingHook{}
	sender := newSender(t,
		&fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}},
		openAIIdentityTranslator(),
		domain.ProtoOpenAI,
		WithHooks(hook),
	)
	ep := &domain.Endpoint{ID: 7, Vendor: "fakev"}
	ep.Routing.URL = srv.URL

	clientBody := []byte(`{"model":"x","msg":"original client body"}`)
	out, err := sender.Send(context.Background(), ep, newEnv(), clientBody)
	if err != nil || !out.Success() {
		t.Fatalf("Send failed: out=%+v err=%v", out, err)
	}

	// ClientRequest: fires at the very start of Send; body is the bytes the caller passed in
	if len(hook.clientReq) != 1 || string(hook.clientReq[0]) != string(clientBody) {
		t.Fatalf("ClientRequest got %v", hook.clientReq)
	}
	// UpstreamRequest: identical to ClientRequest under the identity translator
	if len(hook.upstreamReq) != 1 || string(hook.upstreamReq[0]) != string(clientBody) {
		t.Fatalf("UpstreamRequest got %v", hook.upstreamReq)
	}

	// Go through Forward to get the response; then assert the chunk-related hooks
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
	// under the identity path, chunks on both sides should match
	upJoined := joinChunks(hook.upstreamChk)
	clJoined := joinChunks(hook.clientChk)
	if upJoined != clJoined {
		t.Fatalf("identity translator: upstream=%q client=%q", upJoined, clJoined)
	}
	if upJoined != "hello stream" {
		t.Fatalf("chunks reassembled = %q", upJoined)
	}

	// AttemptComplete fires once (success)
	if len(hook.completes) != 1 || hook.completes[0].Class != ClassSuccess {
		t.Fatalf("AttemptComplete got %v", hook.completes)
	}
	if hook.completedEPs[0] != 7 {
		t.Fatalf("completed ep id = %d", hook.completedEPs[0])
	}
}

func TestHooks_AttemptCompleteFiredOnFailure(t *testing.T) {
	hook := &recordingHook{}
	// let the factory return nil to trigger the Permanent failure path
	sender := newSender(t, nil, nil, domain.ProtoUnknown, WithHooks(hook))
	ep := &domain.Endpoint{ID: 8, Vendor: "missing"}

	out, _ := sender.Send(context.Background(), ep, newEnv(), []byte("body"))

	// ClientRequest: precedes the factory lookup, still fires
	if len(hook.clientReq) != 1 {
		t.Fatalf("ClientRequest should fire even when factory missing; got %d", len(hook.clientReq))
	}
	// UpstreamRequest: unreachable since the factory wasn't found, must not fire
	if len(hook.upstreamReq) != 0 {
		t.Fatalf("UpstreamRequest must not fire when factory missing; got %d", len(hook.upstreamReq))
	}
	// AttemptComplete: must fire on the failure path, and the outcome must be Permanent
	if len(hook.completes) != 1 || hook.completes[0].Class != ClassPermanent {
		t.Fatalf("AttemptComplete on failure: %v", hook.completes)
	}
	if out.Class != ClassPermanent {
		t.Fatalf("expected Permanent outcome")
	}
}

// A Hook implementing only part of the interface -- verifies that
// type-assert bucketing fires only as needed.
type onlyClientReqHook struct{ count int }

func (h *onlyClientReqHook) OnClientRequest(_ context.Context, _ *domain.Endpoint, _ []byte) {
	h.count++
}

func TestHooks_PartialInterfaceIsAllowed(t *testing.T) {
	partial := &onlyClientReqHook{}
	// missing factory takes the Permanent path
	sender := newSender(t, nil, nil, domain.ProtoUnknown, WithHooks(partial))
	ep := &domain.Endpoint{ID: 9}
	_, _ = sender.Send(context.Background(), ep, newEnv(), []byte("x"))

	if partial.count != 1 {
		t.Fatalf("only OnClientRequest should fire once; got %d", partial.count)
	}
}

func TestHooks_MultipleHooksFireInOrder(t *testing.T) {
	var order []string
	mk := func(name string) Hook {
		return clientReqHookFunc(func(_ context.Context, _ *domain.Endpoint, _ []byte) {
			order = append(order, name)
		})
	}

	sender := newSender(t, nil, nil, domain.ProtoUnknown, WithHooks(mk("a"), mk("b"), mk("c")))
	_, _ = sender.Send(context.Background(), &domain.Endpoint{ID: 1}, newEnv(), []byte("x"))

	if got := strings.Join(order, ","); got != "a,b,c" {
		t.Fatalf("hook order = %q want a,b,c", got)
	}
}

// clientReqHookFunc adapter: lets a closure act directly as a ClientRequestObserver.
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

// errAfterReader returns its payload, then fails the next Read — simulating
// an upstream that drops the connection mid-stream.
type errAfterReader struct {
	payload io.Reader
	err     error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	n, err := r.payload.Read(p)
	if n > 0 {
		return n, nil
	}
	if err == io.EOF {
		return 0, r.err
	}
	return n, err
}

// docs/05 §3: a stream cut off mid-way must publish its accumulated usage
// with Truncated=true and Confidence downgraded to approximate.
func TestForward_InterruptedStreamMarksUsageTruncated(t *testing.T) {
	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, openAIIdentityTranslator(), domain.ProtoOpenAI)

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       io.NopCloser(&errAfterReader{payload: strings.NewReader("partial"), err: errors.New("upstream RST")}),
	}
	w := httptest.NewRecorder()
	var stream protocol.ResponseStream = &fakeRespHandler{}

	res := sender.Forward(context.Background(), w, &domain.Endpoint{ID: 99}, resp, stream)

	if res.FeedErr == nil {
		t.Fatalf("want FeedErr on interrupted stream")
	}
	if res.Usage == nil {
		t.Fatalf("usage should still be published on interruption")
	}
	if !res.Usage.Truncated {
		t.Fatalf("Truncated not set on interrupted stream")
	}
	if res.Usage.Confidence != domain.UsageConfidenceApproximate {
		t.Fatalf("Confidence = %q, want approximate", res.Usage.Confidence)
	}
}

// A cleanly completed stream must NOT be marked truncated.
func TestForward_CleanStreamNotTruncated(t *testing.T) {
	sender := newSender(t, &fakeFactory{meta: protocol.Metadata{Vendor: "fakev"}}, openAIIdentityTranslator(), domain.ProtoOpenAI)

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("hello")),
	}
	w := httptest.NewRecorder()
	var stream protocol.ResponseStream = &fakeRespHandler{}

	res := sender.Forward(context.Background(), w, &domain.Endpoint{ID: 99}, resp, stream)

	if res.FeedErr != nil {
		t.Fatalf("FeedErr = %v", res.FeedErr)
	}
	if res.Usage.Truncated {
		t.Fatalf("clean stream must not be truncated")
	}
}
