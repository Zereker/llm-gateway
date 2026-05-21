package dispatch

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// =============================================================================
// fakeSelector / fakeInvokerFactory / fakeResult — 通用 test doubles
// =============================================================================

// fakeSelector 顺序消费 responses；out-of-range → panic（防止测试漏配 fixture）。
type fakeSelector struct {
	responses []selResp
	calls     int
}

type selResp struct {
	ep  *domain.Endpoint
	err error
}

func newFakeSelector(rs ...selResp) *fakeSelector {
	return &fakeSelector{responses: rs}
}

func (f *fakeSelector) Select(_ context.Context, _ Query) (*domain.Endpoint, error) {
	if f.calls >= len(f.responses) {
		panic("fakeSelector: out of responses")
	}
	r := f.responses[f.calls]
	f.calls++
	return r.ep, r.err
}

// fakeInvokerFactory 顺序消费 results。
type fakeInvokerFactory struct {
	results []*fakeResult
	calls   int
}

func newFakeInvokerFactory(results ...*fakeResult) *fakeInvokerFactory {
	return &fakeInvokerFactory{results: results}
}

func (f *fakeInvokerFactory) For(ep *domain.Endpoint, _ *domain.RequestEnvelope, _ []byte, _ protocol.Handler) Invoker {
	if f.calls >= len(f.results) {
		panic("fakeInvokerFactory: out of results")
	}
	r := f.results[f.calls]
	r.ep = ep
	f.calls++
	return &fakeInvoker{res: r, invokeErr: r.invokeErr}
}

type fakeInvoker struct {
	res       *fakeResult
	invokeErr error
}

func (f *fakeInvoker) Invoke(_ context.Context) (Result, error) {
	if f.invokeErr != nil {
		return nil, f.invokeErr
	}
	return f.res, nil
}

// fakeResult 实现 dispatch.Result。
type fakeResult struct {
	ep        *domain.Endpoint
	verdict   Verdict
	streamRep StreamReport
	invokeErr error // 让 fakeInvoker.Invoke 直接返 err（不返 fakeResult）
	streamed  bool
	closed    bool
}

func (r *fakeResult) Verdict() Verdict             { return r.verdict }
func (r *fakeResult) Endpoint() *domain.Endpoint   { return r.ep }
func (r *fakeResult) StreamTo(_ context.Context, _ http.ResponseWriter) StreamReport {
	r.streamed = true
	return r.streamRep
}
func (r *fakeResult) Close() error {
	r.closed = true
	return nil
}

// =============================================================================
// 工厂函数：常见 verdict / endpoint / rc 构造
// =============================================================================

func successResult(usage *domain.Usage, ttftMs int64) *fakeResult {
	return &fakeResult{
		verdict:   Verdict{Class: ClassSuccess, HTTPCode: 200, Latency: 10 * time.Millisecond},
		streamRep: StreamReport{Usage: usage, TTFTMs: ttftMs},
	}
}

func transientResult() *fakeResult {
	return &fakeResult{
		verdict: Verdict{Class: ClassTransient, HTTPCode: 502, Reason: "upstream 502", Latency: 5 * time.Millisecond},
	}
}

func invalidResult() *fakeResult {
	return &fakeResult{
		verdict: Verdict{Class: ClassInvalid, HTTPCode: 400, Reason: "bad request body", Latency: 1 * time.Millisecond},
	}
}

func permanentResult() *fakeResult {
	return &fakeResult{
		verdict: Verdict{Class: ClassPermanent, HTTPCode: 401, Reason: "bad key", Latency: 1 * time.Millisecond},
	}
}

func newTestEP(id int64) *domain.Endpoint {
	return &domain.Endpoint{
		ID:      id,
		Name:    "ep-" + itoa(id),
		Vendor:  "openai",
		Model:   "gpt-4",
		Enabled: true,
		Weight:  100,
	}
}

// newTestInput 构造 dispatch.Input；handlers 永远返回 stubHandlerOK，
// fakeInvokerFactory 不会真调它（只看 verdict），dispatcher 在 invoke 前能拿到 non-nil handler。
func newTestInput(models ...string) Input {
	in := Input{
		Identity: domain.UserIdentity{AccountID: "acc-1", Group: "free", APIKeyID: "ak-1"},
		Envelope: &domain.RequestEnvelope{RawBytes: []byte(`{}`)},
		Handlers: stubHandlers{},
	}
	in.ModelChain = make([]*domain.ModelService, len(models))
	for i, m := range models {
		in.ModelChain[i] = &domain.ModelService{ID: int64(i + 1), Model: m}
	}
	return in
}

// stubHandlers 永远返回 stubHandlerOK；fakeInvokerFactory.For 拿到后忽略不调用。
type stubHandlers struct{}

func (stubHandlers) Get(_ *domain.Endpoint, _ domain.Protocol) protocol.Handler {
	return stubHandlerOK{}
}

type stubHandlerOK struct{}

func (stubHandlerOK) Capabilities() protocol.Capabilities { return protocol.Capabilities{} }
func (stubHandlerOK) PrepareCall(_ context.Context, _ *domain.Endpoint, _ []byte) (*protocol.Call, error) {
	return nil, nil
}
func (stubHandlerOK) NewResponseStream() protocol.ResponseStream { return nil }

// =============================================================================
// sentinel
// =============================================================================

var errFakeDep = errors.New("fake dependency failure")

// itoa 避免 import strconv 在 test helper 里。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
