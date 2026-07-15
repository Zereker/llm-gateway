package adapters

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/dispatch"
	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/invoker"
	"github.com/zereker/llm-gateway/internal/moderation"
	"github.com/zereker/llm-gateway/internal/policy"
	"github.com/zereker/llm-gateway/internal/selector"
)

func TestClassMappings(t *testing.T) {
	cases := []struct {
		dispatch dispatch.Class
		selector selector.ErrorClass
		invoker  invoker.Class
	}{
		{dispatch.ClassSuccess, selector.ClassSuccess, invoker.ClassSuccess},
		{dispatch.ClassTransient, selector.ClassTransient, invoker.ClassTransient},
		{dispatch.ClassCapacity, selector.ClassCapacity, invoker.ClassCapacity},
		{dispatch.ClassPermanent, selector.ClassPermanent, invoker.ClassPermanent},
		{dispatch.ClassInvalid, selector.ClassInvalid, invoker.ClassInvalid},
		{dispatch.ClassUnknown, selector.ClassUnknown, invoker.ClassUnknown},
	}
	for _, tc := range cases {
		if got := dispatchClassToSelector(tc.dispatch); got != tc.selector {
			t.Errorf("dispatchClassToSelector(%v) = %v", tc.dispatch, got)
		}
		if got := invokerClassToDispatch(tc.invoker); got != tc.dispatch {
			t.Errorf("invokerClassToDispatch(%v) = %v", tc.invoker, got)
		}
	}
}

type closeTrackingBody struct {
	io.Reader
	closed int
	err    error
}

func (b *closeTrackingBody) Close() error {
	b.closed++
	return b.err
}

func TestInvokerResultAccessorsCloseAndEmptyStream(t *testing.T) {
	ep := &domain.Endpoint{ID: 7}
	verdict := dispatch.Verdict{Class: dispatch.ClassSuccess, HTTPCode: 200}
	bodyErr := errors.New("close failed")
	body := &closeTrackingBody{Reader: strings.NewReader("body"), err: bodyErr}
	result := &invokerResult{ep: ep, verdict: verdict, response: &http.Response{Body: body}}
	if result.Endpoint() != ep || result.Verdict() != verdict {
		t.Fatalf("endpoint=%+v verdict=%+v", result.Endpoint(), result.Verdict())
	}
	if err := result.Close(); !errors.Is(err, bodyErr) || body.closed != 1 {
		t.Fatalf("Close err=%v count=%d", err, body.closed)
	}
	if err := result.Close(); err != nil || body.closed != 1 {
		t.Fatalf("second Close err=%v count=%d", err, body.closed)
	}
	if report := result.StreamTo(context.Background(), nil); report != (dispatch.StreamReport{}) {
		t.Fatalf("consumed StreamTo=%+v", report)
	}

	for name, empty := range map[string]*invokerResult{
		"nil response": {},
		"nil handler":  {response: &http.Response{Body: io.NopCloser(strings.NewReader("body"))}},
	} {
		t.Run(name, func(t *testing.T) {
			if report := empty.StreamTo(context.Background(), nil); report != (dispatch.StreamReport{}) {
				t.Fatalf("report=%+v", report)
			}
		})
	}
	if err := (&invokerResult{}).Close(); err != nil {
		t.Fatalf("nil response Close=%v", err)
	}
}

func TestInvokerFactoryForAndStageMappings(t *testing.T) {
	factory := NewInvokerFactory(nil)
	ep := &domain.Endpoint{ID: 1}
	env := &domain.RequestEnvelope{RawBytes: []byte("body")}
	created := factory.For(ep, nil, env)
	impl, ok := created.(*invokerImpl)
	if !ok || impl.ep != ep || impl.env != env || impl.sender != nil {
		t.Fatalf("invoker=%+v", created)
	}
	if got := invokerStageToDispatch(invoker.StagePrepare); got != dispatch.StagePrepare {
		t.Fatalf("prepare stage=%v", got)
	}
	if got := invokerStageToDispatch(invoker.StageInvoke); got != dispatch.StageInvoke {
		t.Fatalf("invoke stage=%v", got)
	}

	closed := 0
	reader := readClose{Reader: strings.NewReader("decoded"), closeFn: func() error { closed++; return nil }}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "decoded" {
		t.Fatalf("read=%q err=%v", data, err)
	}
	if err := reader.Close(); err != nil || closed != 1 {
		t.Fatalf("close err=%v count=%d", err, closed)
	}
}

type schedulerStub struct {
	pickRequest *selector.Request
	pickResult  *domain.Endpoint
	pickErr     error
	reportEP    *domain.Endpoint
	report      selector.Result
	released    *domain.Endpoint
}

func (s *schedulerStub) Pick(_ context.Context, request *selector.Request) (*domain.Endpoint, error) {
	s.pickRequest = request
	return s.pickResult, s.pickErr
}

func (s *schedulerStub) Report(_ context.Context, ep *domain.Endpoint, result selector.Result) {
	s.reportEP = ep
	s.report = result
}

func (s *schedulerStub) Release(_ context.Context, ep *domain.Endpoint) { s.released = ep }

func TestPickerAdapterMapsPickReportAndRelease(t *testing.T) {
	selected := &domain.Endpoint{ID: 2}
	scheduler := &schedulerStub{pickResult: selected}
	adapter := NewSelector(scheduler)
	exclude := map[int64]struct{}{9: {}}
	eligible := []*domain.Endpoint{{ID: 1, Weight: 3}, selected}
	got, err := adapter.Pick(context.Background(), eligible, dispatch.PickQuery{
		Model: "model", Group: "group", SessionKey: "session", Exclude: exclude,
	})
	if err != nil || got != selected {
		t.Fatalf("picked=%+v err=%v", got, err)
	}
	request := scheduler.pickRequest
	if request == nil || request.Model != "model" || request.Group != "group" || request.SessionKey != "session" ||
		len(request.Candidates) != 2 || request.Candidates[0].Endpoint != eligible[0] ||
		request.Candidates[0].EffectiveWeight != 3 || request.ExcludeIDs[9] != struct{}{} {
		t.Fatalf("request=%+v", request)
	}

	verdict := dispatch.Verdict{
		Class: dispatch.ClassTransient, HTTPCode: 503, Reason: "upstream", Latency: time.Second, RetryAfter: 2 * time.Second,
	}
	adapter.Report(context.Background(), selected, verdict)
	if scheduler.reportEP != selected || scheduler.report.Class != selector.ClassTransient || scheduler.report.HTTPCode != 503 ||
		scheduler.report.Reason != "upstream" || scheduler.report.Latency != time.Second || scheduler.report.RetryAfter != 2*time.Second {
		t.Fatalf("report endpoint=%+v result=%+v", scheduler.reportEP, scheduler.report)
	}
	adapter.Release(context.Background(), selected)
	if scheduler.released != selected {
		t.Fatalf("released=%+v", scheduler.released)
	}
}

func TestPickerAdapterEmptyAndSchedulerError(t *testing.T) {
	scheduler := &schedulerStub{}
	adapter := NewSelector(scheduler)
	got, err := adapter.Pick(context.Background(), nil, dispatch.PickQuery{})
	if err != nil || got != nil || scheduler.pickRequest != nil {
		t.Fatalf("empty pick=%+v err=%v request=%+v", got, err, scheduler.pickRequest)
	}

	wantErr := errors.New("picker unavailable")
	scheduler.pickErr = wantErr
	got, err = adapter.Pick(context.Background(), []*domain.Endpoint{{ID: 1}}, dispatch.PickQuery{})
	if got != nil || !errors.Is(err, wantErr) {
		t.Fatalf("pick=%+v err=%v", got, err)
	}
}

func TestForwardResultToStreamReport(t *testing.T) {
	tests := []struct {
		name         string
		forward      invoker.ForwardResult
		class        dispatch.Class
		code         int
		reason       string
		localFailure bool
		prewrite     bool
	}{
		{name: "success", forward: invoker.ForwardResult{Committed: true}, prewrite: false},
		{
			name: "stream processing failure", forward: invoker.ForwardResult{FeedErr: errors.New("feed")},
			class: dispatch.ClassTransient, code: 503, reason: "response stream processing failed", prewrite: true,
		},
		{
			name: "policy enforcement failure", forward: invoker.ForwardResult{FeedErr: moderation.ErrPolicyEnforcement},
			class: dispatch.ClassTransient, code: 503, reason: "response policy enforcement failed", localFailure: true, prewrite: true,
		},
		{
			name: "policy denial", forward: invoker.ForwardResult{FeedErr: errors.Join(moderation.ErrPolicyEnforcement, policy.ErrDenied)},
			class: dispatch.ClassInvalid, code: 400, reason: "content rejected by response policy", localFailure: true, prewrite: true,
		},
		{
			name: "committed failure", forward: invoker.ForwardResult{FeedErr: moderation.ErrPolicyEnforcement, Committed: true},
			localFailure: true, prewrite: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := forwardResultToStreamReport(tc.forward)
			if report.LocalFailure != tc.localFailure || (report.Prewrite != nil) != tc.prewrite {
				t.Fatalf("report=%+v", report)
			}
			if tc.prewrite && (report.Prewrite.Class != tc.class || report.Prewrite.HTTPCode != tc.code || report.Prewrite.Reason != tc.reason) {
				t.Fatalf("prewrite=%+v", report.Prewrite)
			}
		})
	}
}
