package main

import (
	"context"
	"net/http"
	"time"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/selector"
	"github.com/zereker/llm-gateway/pkg/selector/eligibility"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/invoker"
)

// =============================================================================
// Selector adapter: middleware.EndpointReader + selector.Scheduler → dispatch.Selector
// =============================================================================
//
// **职责**：
//   1. ListForModel 拉候选
//   2. eligibility.Filter（modality / adapter / protocol / translator 资格过滤）
//   3. 构造 selector.Candidate（EffectiveWeight = ep.Weight）
//   4. scheduler.Pick（filter chain → scorer → 内部 selector）
//
// **没 Report 方法**——Report 内化到 Invoker（在 Invoke 完成时由 invokerAdapter 调）。

type selectorAdapter struct {
	endpoints middleware.EndpointReader
	sched     selector.Scheduler
}

func newSelectorAdapter(endpoints middleware.EndpointReader, sched selector.Scheduler) *selectorAdapter {
	return &selectorAdapter{endpoints: endpoints, sched: sched}
}

func (s *selectorAdapter) Select(ctx context.Context, q dispatch.Query) (*domain.Endpoint, error) {
	raw, err := s.endpoints.ListForModel(ctx, q.Model, q.Identity.Group)
	if err != nil {
		return nil, err
	}
	elgRes := eligibility.Filter(raw, q.Envelope, adapterRegistryLookup{}, translatorRegistryLookup{})
	if len(elgRes.Eligible) == 0 {
		return nil, nil
	}
	cands := make([]selector.Candidate, len(elgRes.Eligible))
	for i, ep := range elgRes.Eligible {
		cands[i] = selector.Candidate{Endpoint: ep, EffectiveWeight: float64(ep.Weight)}
	}
	return s.sched.Pick(ctx, &selector.Request{
		Model:      q.Model,
		Group:      q.Identity.Group,
		Candidates: cands,
		ExcludeIDs: q.Exclude,
	})
}

// adapterRegistryLookup / translatorRegistryLookup：复制自 pkg/middleware/selector.go
// （PR3 时 selector 包 internalize 这两个 lookup，cmd 不再 hold）。

type adapterRegistryLookup struct{}

func (adapterRegistryLookup) Has(vendor string) bool { return adapter.Get(vendor) != nil }
func (adapterRegistryLookup) NativeProtocol(vendor string) domain.Protocol {
	f := adapter.Get(vendor)
	if f == nil {
		return domain.ProtoUnknown
	}
	return f.Metadata().NativeProtocol
}
func (adapterRegistryLookup) SupportedModalities(vendor string) []domain.Modality {
	f := adapter.Get(vendor)
	if f == nil {
		return nil
	}
	return f.Metadata().SupportedModalities
}

type translatorRegistryLookup struct{}

func (translatorRegistryLookup) Has(src, tgt domain.Protocol) bool {
	return translator.Find(src, tgt) != nil
}

// =============================================================================
// InvokerFactory adapter: invoker.Sender + ratelimit.Store + selector.Scheduler
//                       → dispatch.InvokerFactory + dispatch.Invoker + dispatch.Result
// =============================================================================
//
// **职责**：
//   - For(ep, env, body) → invokerAdapter（一次性句柄）
//   - Invoke(ctx) 内做：endpoint RPM/RPS reserve → sender.Send → scheduler.Report
//   - Result.StreamTo 内做：moderator wrap → sender.Forward → endpoint TPM 后扣
//   - Result.Close 兜底关 body
//
// **scheduler.Report 内化点**：所有 Invoke 返回前都已 Report 完（包括 reserve 失败
// 的 capacity 分类）。Dispatcher 不再调 scheduler.Report——这是"业务编排不知道
// cooldown"的体现。

type invokerFactoryAdapter struct {
	sender    *invoker.Sender
	sched     selector.Scheduler
	rateStore middleware.RateLimitStore // 可空：不传 = 跳过 endpoint quota
}

func newInvokerFactoryAdapter(sender *invoker.Sender, sched selector.Scheduler, rateStore middleware.RateLimitStore) *invokerFactoryAdapter {
	return &invokerFactoryAdapter{sender: sender, sched: sched, rateStore: rateStore}
}

func (f *invokerFactoryAdapter) For(ep *domain.Endpoint, env *domain.RequestEnvelope, body []byte) dispatch.Invoker {
	return &invokerAdapter{
		ep:        ep,
		env:       env,
		body:      body,
		sender:    f.sender,
		sched:     f.sched,
		rateStore: f.rateStore,
	}
}

type invokerAdapter struct {
	ep        *domain.Endpoint
	env       *domain.RequestEnvelope
	body      []byte
	sender    *invoker.Sender
	sched     selector.Scheduler
	rateStore middleware.RateLimitStore
}

func (i *invokerAdapter) Invoke(ctx context.Context) (dispatch.Result, error) {
	// 1) endpoint RPM/RPS reserve（前扣）
	if i.rateStore != nil {
		if buckets := selector.EndpointReserveBuckets(i.ep); len(buckets) > 0 {
			allowed, violated, rerr := i.rateStore.ReserveBatch(ctx, buckets)
			if rerr != nil {
				reason := "endpoint reserve: " + rerr.Error()
				v := dispatch.Verdict{Class: dispatch.ClassCapacity, Reason: reason}
				i.sched.Report(ctx, i.ep, selector.Result{Class: selector.ClassCapacity, Reason: reason})
				return &reserveFailedResult{ep: i.ep, verdict: v}, nil
			}
			if !allowed {
				key := ""
				if violated != nil {
					key = violated.Key
				}
				reason := "endpoint quota exhausted: " + key
				v := dispatch.Verdict{Class: dispatch.ClassCapacity, Reason: reason}
				i.sched.Report(ctx, i.ep, selector.Result{Class: selector.ClassCapacity, Reason: reason})
				return &reserveFailedResult{ep: i.ep, verdict: v}, nil
			}
		}
	}

	// 2) sender.Send
	outcome, _ := i.sender.Send(ctx, i.ep, i.env, i.body)

	// 3) scheduler.Report
	i.sched.Report(ctx, i.ep, outcome.ToScheduleResult())

	// 4) 转 dispatch.Verdict
	v := dispatch.Verdict{
		Class:    scheduleClassToDispatch(outcome.Class),
		HTTPCode: outcome.HTTPCode,
		Reason:   outcome.Reason,
		Latency:  outcome.Latency,
	}

	return &invocationResult{
		ep:         i.ep,
		verdict:    v,
		response:   outcome.Response,
		translator: outcome.Translator,
		sender:     i.sender,
		rateStore:  i.rateStore,
	}, nil
}

// reserveFailedResult — reserve 阶段失败，sender.Send 还没发生。
type reserveFailedResult struct {
	ep      *domain.Endpoint
	verdict dispatch.Verdict
}

func (r *reserveFailedResult) Verdict() dispatch.Verdict      { return r.verdict }
func (r *reserveFailedResult) Endpoint() *domain.Endpoint     { return r.ep }
func (r *reserveFailedResult) StreamTo(_ context.Context, _ http.ResponseWriter) dispatch.StreamReport {
	return dispatch.StreamReport{} // 不可能调到（Class != Success）
}
func (r *reserveFailedResult) Close() error { return nil }

// invocationResult — sender.Send 真实拿到响应的 Result。
//
// 资源生命周期：StreamTo 内部消费 response.Body 并 close；Close 兜底关
// body（StreamTo 之后 Close 是 no-op）。
type invocationResult struct {
	ep         *domain.Endpoint
	verdict    dispatch.Verdict
	response   *http.Response
	translator translator.Translator
	sender     *invoker.Sender
	rateStore  middleware.RateLimitStore
	consumed   bool
}

func (r *invocationResult) Verdict() dispatch.Verdict  { return r.verdict }
func (r *invocationResult) Endpoint() *domain.Endpoint { return r.ep }

func (r *invocationResult) StreamTo(ctx context.Context, w http.ResponseWriter) dispatch.StreamReport {
	if r.consumed || r.response == nil || r.translator == nil {
		return dispatch.StreamReport{}
	}
	r.consumed = true

	handler := middleware.WrapWithModerator(r.translator.NewResponseHandler(), ctx)
	fwd := r.sender.Forward(ctx, w, r.ep, r.response, handler)

	// endpoint TPM 后扣（docs/04 §10）
	chargeEndpointTPM(r.rateStore, r.ep, fwd.Usage)

	return dispatch.StreamReport{
		Usage:  fwd.Usage,
		Err:    fwd.FeedErr,
		TTFTMs: fwd.TTFTMs,
	}
}

func (r *invocationResult) Close() error {
	if r.consumed || r.response == nil {
		return nil
	}
	r.consumed = true
	return r.response.Body.Close()
}

// =============================================================================
// helpers
// =============================================================================

// scheduleClassToDispatch selector.ErrorClass → dispatch.Class（1:1 映射）。
func scheduleClassToDispatch(c selector.ErrorClass) dispatch.Class {
	switch c {
	case selector.ClassSuccess:
		return dispatch.ClassSuccess
	case selector.ClassTransient:
		return dispatch.ClassTransient
	case selector.ClassCapacity:
		return dispatch.ClassCapacity
	case selector.ClassPermanent:
		return dispatch.ClassPermanent
	case selector.ClassInvalid:
		return dispatch.ClassInvalid
	default:
		return dispatch.ClassUnknown
	}
}

// chargeEndpointTPM 选中 endpoint 响应成功后，把真实 usage.Total 写到 endpoint
// TPM bucket（docs/04 §10）。超限只记 metric；不阻塞响应。
//
// 用 background ctx（响应已完成，客户端 ctx 可能 cancel）。
func chargeEndpointTPM(store middleware.RateLimitStore, ep *domain.Endpoint, usage *domain.Usage) {
	if store == nil || ep == nil || usage == nil || usage.Total <= 0 {
		return
	}
	b := selector.EndpointTPMChargeBucket(ep, uint32(usage.Total))
	if b == nil {
		return
	}
	bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = store.ChargeBatch(bgCtx, []ratelimit.Bucket{*b})
}
