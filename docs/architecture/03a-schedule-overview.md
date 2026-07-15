[English](03a-schedule-overview.md) | [简体中文](03a-schedule-overview.zh-CN.md)

# 03a — Schedule Quick Reference / Onboarding Companion

This is the beginner's-eye view of [03-endpoint-scheduling.md](./03-endpoint-scheduling.md): first walk through
the whole schedule picture (data flow, each package's responsibility, key data structures, wiring points),
then go back to 03 for the design rationale behind each rule.

> 03 explains **why**, this doc explains **what / where**. For changes to main-path code, 03 still governs.

## 0. TL;DR

The scheduling execution timing is owned by **`dispatch.Dispatcher`**; `middleware/schedule.go` is just a
thin adapter — it maps gin / RC into `dispatch.Input`, runs `Dispatch()`, then maps
`dispatch.Outcome` back into RC + HTTP. Below is the actual execution flow:

```text
middleware/schedule.go (M7 thin adapter):
  envelope/identity/modelChain/handlers → dispatch.Input
  outcome := dispatcher.Dispatch(ctx, w, input)
  outcome → rc.{RoutedModelService, Usage, Error, SchedulingDecision} + HTTP

dispatch.Dispatcher.Dispatch (internal/dispatch/dispatcher.go):
  state := newState(input, AttemptCap.Resolve(input))
  for {
      action := step(ctx, w, state)
      switch action {
      case Continue: pick another one on the same model
      case Switch:   switch to the next model (triggered by FallbackPolicy)
      case Stream:   stream already written, return Outcome
      case Abort:    terminate, return Outcome
      }
  }

dispatch.Dispatcher.step (a single attempt):
  if Exhausted → Abort(NoEndpoint, 503)
  candidates := CandidateSource.ListForModel(model, group)
  eligible   := filterEligible(candidates, env, handlers)   # internal/dispatch/eligibility.go
  if no eligible → FallbackPolicy.OnExhausted(state)
  ep := Selector.Pick(eligible, query)                       # internal/selector → adapters.SelectorAdapter
  if denied := EndpointQuota.Reserve(ep) → Verdict + RetryPolicy.Decide
  handler := state.Handlers().Get(ep, srcProto)              # protocol.Lookup
  res := InvokerFactory.For(ep, handler, env).Invoke(ctx)    # internal/invoker → adapters.InvokerFactoryAdapter
  Selector.Report(ep, verdict)
  switch RetryPolicy.Decide(verdict):
  case Stream:   res.StreamTo(w) + EndpointQuota.ChargeUsage  # deducted after TPM
  case Continue / Switch / Abort: return action
```

Three-layer division of responsibility:

| Who | Responsible for |
|----|------|
| `internal/middleware/schedule.go` | RC ↔ dispatch.Input/Outcome mapping; content log enrichment; metric `scheduling_duration_seconds`; **does not make scheduling decisions** |
| `dispatch.Dispatcher` (`internal/dispatch`) | The **sole** owner of scheduling execution timing: candidates → eligibility filter → selection → pre-deduction → invocation → reporting → retry/fallback → post-deduction |
| `internal/selector` | Runs the filter chain → scorer → picker over a batch of candidates, outputs 1 endpoint. **Stateless**, unaware of protocol / handler / repo / fallback |
| `internal/invoker` | Takes a Handler and runs `PrepareCall + HTTP Do + response forward + error classification` (does not do protocol lookup — dispatch has already obtained the Handler via `protocol.Lookup`) |
| `internal/protocol` | facade: Handler = Factory + Translator + Quirks; consumers only see Handler / Lookup |
| `internal/ratelimit` | bucket / store primitives; `dispatch/adapters.EndpointQuotaAdapter` wires it into `dispatch.EndpointQuota` |
| `internal/dispatch/adapters/` | Bridges the 4 primitive packages above into dispatch's 4 ports (CandidateSource / Selector / InvokerFactory / EndpointQuota), decoupling composition logic from the primitives |

`internal/selector` does not hold a repo, and doesn't know fallback models exist. Cross-model fallback is
business semantics, and stays in the outer loop of `dispatch.Dispatcher` (FallbackPolicy triggers the Switch action).

## 1. Overview of package / file responsibilities

```
internal/middleware/schedule.go     M7 thin adapter (gin.HandlerFunc Schedule()):
                               RC → dispatch.Input → dispatcher.Dispatch → RC

internal/dispatch/                  owner of scheduling execution timing
    dispatcher.go              Dispatcher.Dispatch / step main loop
    eligibility.go             pure function filterEligible: takes candidates + envelope + protocol.Lookup
                               outputs eligible endpoints; semantics in §2
    state.go                   per-request state; finalize always produces a SchedulingDecision
    action.go / verdict.go     Continue / Switch / Stream / Abort + Verdict types
    ports.go                   4 port interfaces (CandidateSource / Selector / InvokerFactory / EndpointQuota)
    policy.go / fallback_chain.go / retry_default.go
                               default implementations of AttemptCap / RetryPolicy / FallbackPolicy
    cap_header.go              X-Gateway-Max-Attempts header parsing
    adapters/                  bridges primitive packages into dispatch ports
        selector.go            selector.Scheduler → dispatch.Selector
        invoker.go             invoker.Sender   → dispatch.InvokerFactory
        quota.go               ratelimit.Store  → dispatch.EndpointQuota

internal/selector/                  selection primitives, **unaware** of protocol / handler / repo
    types.go                   Candidate / Request / Result / ErrorClass / Scheduler interfaces
    scheduler.go               defaultScheduler: Pick (filter→scorer→picker) + Report
    filter.go                  Filter interface + runChain
    cooldown.go                CooldownManager + RedisCooldownManager + CooldownFilter
    limit_filter.go            LimitReadFilter (SnapshotBatch, read-only)
    busy.go / prefix_cache.go  self-hosted optimization filters
    weighted.go                Picker interface + WeightedRandomPicker
    scorer.go                  Scorer + EndpointStatsStore + DefaultScorer

internal/invoker/                   HTTP invocation + forward stream, does not do protocol lookup
internal/ratelimit/                 Store / Bucket / endpoint bucket helpers
internal/protocol/                  Handler facade + Factory/Session + quirks

internal/app/gateway/          composition root
    app.go                     wires primitives together into dispatch.Dispatcher
    dispatch.go                buildDispatcher: dispatch.New(WithCandidates / WithSelector /
                               WithInvokerFactory / WithQuota / WithCap / WithRetry /
                               WithFallback / WithTracer)
```

## 2. Semantic boundaries of the three filtering layers (**the point most easily confused in schedule**)

| Layer | Who | Semantics | Consequence of failure |
|----|----|------|----------|
| **Eligibility** | `internal/dispatch/eligibility.go` (an internal helper of dispatch, not a standalone package) | Can it handle this at all: protocol.Lookup can't find a Handler / modality unsupported | Excluded, **does not enter cooldown, not counted as an upstream failure** |
| **Hard Filter** | `internal/selector.Filter` (cooldown / limit_read / busy / prefix_cache) | Should it be picked right now: in cooldown / quota exhausted / too busy / prefix affinity | Not chosen this Pick; does not directly terminate the request |
| **Soft Scoring** | `internal/selector.Scorer` | Who is preferred: adjusts `EffectiveWeight` based on success/latency/cost | Only adjusts weight, **never eliminates** a candidate |
| **Selector** | `internal/selector.Selector` (default weighted_random) | Picks 1 from the filtered candidates by `EffectiveWeight` | All-zero → nil → inner break |

**Core principle** (03 §3): capability issues (missing vendor Factory / translator / ep.Protocol unknown)
must be excluded at the Eligibility stage — they must never reach `Scheduler.Report`, otherwise
"unsupported" gets mislabeled as "bad ep", triggering cooldown and polluting subsequent selection.

## 3. Key data structures (types.go)

```go
type Candidate struct {
    Endpoint        *domain.Endpoint
    EffectiveWeight float64           // = ep.Weight when static; adjusted when Scorer is enabled
}

type Request struct {
    Model      string                  // the model for the current round (primary or a fallback)
    Group      string                  // rc.Identity.Group
    Candidates []Candidate             // candidates after eligibility
    ExcludeIDs map[int64]struct{}      // eps already tried in this request
    PrefixKey  []byte                  // used only by PrefixCacheFilter
}

type Result struct {
    Class    ErrorClass                // determines cooldown TTL + whether to retry
    HTTPCode int
    Reason   string
    Latency  time.Duration
}

type Scheduler interface {
    Pick(ctx, *Request) (*domain.Endpoint, error)
    Report(ctx, *domain.Endpoint, Result)
}
```

**Request deliberately does not carry** `attempts` / `fallbackModels` / `LoadFallback` — these are all
the responsibility of the outer reducer of dispatch.Dispatcher; the scheduler only looks at a single batch of candidates.

## 4. ErrorClass six-way classification quick reference

| Class | Trigger scenario | IsRetryable | Cooldown |
|-------|----------|-------------|----------|
| `success` | HTTP 2xx + protocol-layer success | false | no cooldown |
| `transient` | 5xx / network / timeout / DNS | true | TTL per config |
| `capacity` | upstream 429 / overloaded / local reserve exceeded | true | TTL per config |
| `permanent` | upstream 401 / 403 / config error | true (switch ep) | TTL per config |
| `invalid` | client 4xx (other than 401/403/429) / translator conversion failure | **false** | no cooldown |
| `unknown` | cannot be classified | true | **no cooldown** (to prevent a classification bug from being amplified into a blanket cooldown) |

Note two special points:

- When `invalid` is hit, `dispatch.DefaultRetry.Decide` returns `Abort{HTTPCode: 400}` directly,
  and after the outer reducer receives it, `state.SetAbort` + exits the loop — it **neither** switches ep
  **nor** switches fallback model (`PrepareError{Phase:PhaseTranslate|PhaseQuirks}` also takes this path).
- `unknown` is retryable, but `Scheduler.Report` special-cases it to **not write a cooldown**
  (to avoid a classification blind spot polluting cooldown).

## 5. Retry model (two layers, complementary)

```
Inner layer (same model, switch endpoint):
  failure + retryable → excluded[ep.ID] = struct{}{} → continue Pick
  attempts count toward totalAttempts, bounded by attemptsCap
  (attemptsCap = min(cfg.MaxAttempts, X-Gateway-Max-Attempts), default 3)

Outer layer (cross-model fallback):
  switches only when the request carries X-Gateway-Fallback-Models
  cap MaxFallbackModels = 3 (dedup, order preserved)
  each fallback model must go through M5 again (catalog + subscription)
  totalAttempts accumulates across all models, never resets
```

**Key point**: same-endpoint retry (L1 retry) is no longer done — network jitter is now absorbed by
"same model, switch ep". If needed again in the future, it must be added back as explicit config,
not implicitly turned on inside schedule.

## 6. Cooldown flow

```
Scheduler.Report(ep, result) [scheduler.go:107]
  ├─ Stats.Record(ep.ID, result)          # writes to EndpointStatsStore (Scorer input)
  └─ if result.Class.IsRetryable()
        && result.Class != ClassUnknown   # unknown does not cool down
        && Cooldown != nil
        → Cooldown.Mark(ep.ID, class)     # Redis SET cd:endpoint:<id> <class> EX <ttl>
                                           # best-effort: errors only logged, never block
```

**From Redis's perspective**:

```
key:   cd:endpoint:<id>
value: ErrorClass string (for diagnostics)
TTL:   configured per class (CooldownDurations.Get)
```

A later Mark **directly overwrites** the TTL — continued failure = continued cooldown (as expected).

CooldownFilter does a batch query via MGET; **fail-open on Redis error** (keeps all candidates), to avoid
Redis jitter turning into a 503 storm.

## 7. Endpoint Quota (strictly layered apart from M6 user-side quota)

| Timing | Operation | Bucket Key |
|------|------|------------|
| After eligibility / inside the Pick filter chain | `LimitReadFilter` uses `SnapshotBatch` **read-only** to exclude already-exhausted ones | `rl:endpoint:<id>:rpm`, `...:rps` |
| After Pick selects an ep / before Invoke | `dispatch.Dispatcher.step` calls `EndpointQuota.Reserve(ctx, ep)` (`adapters/EndpointQuotaAdapter` wraps `ratelimit.Store.ReserveBatch` for RPM/RPS pre-deduction); over limit → `QuotaVerdict` becomes `Verdict{Class: ClassCapacity}` → `RetryPolicy.Decide` generally returns `Continue` to switch ep | same as above |
| After StreamTo completes (response already finished) | after the dispatcher gets `StreamReport.Usage`, it calls `EndpointQuota.ChargeUsage(ctx, ep, usage)` (`adapters/EndpointQuotaAdapter` wraps `ratelimit.Store.ChargeBatch`, `cost = usage.Total`, fire-and-forget) | `rl:endpoint:<id>:tpm` |

**Why pre-deduction isn't done at the filter stage**: the filter's input is a "candidate set" — if reserve
happened there, candidates that weren't even selected would also get their quota deducted, significantly
amplifying errors. So the filter can **only ever do a read-only SnapshotBatch**;
Reserve only happens after the dispatcher's Pick.

**TPM must be deducted after the fact**: because at request time `usage.Total` is unknown (must wait for the
stream to finish). Only after the dispatcher gets `StreamReport.Usage` does it know the real token usage,
and only then does it call `ChargeUsage`. When TPM goes over limit, only a metric is emitted; it doesn't
block **this** response (it has already been streamed out); the next request is the one that gets blocked
by `LimitReadFilter`.

## 8. Runtime Scoring (optional layer)

Off by default (`cfg.Scoring.Enabled = false`); once enabled, the pipeline becomes:

```
filter chain → Scorer.Score(candidates) → Selector.Select
                     ↑
       EndpointStatsStore.Snapshot(ep.ID)
                     ↑
       Scheduler.Report → Stats.Record (writes every time)
```

`DefaultScorer` formula:

```
effective_weight = base_weight * success_factor * latency_factor
success_factor    = clamp(stats.SuccessRate,                [0.1, 2.0])
latency_factor    = clamp(latencyBaselineMs / stats.LatencyMs, [0.1, 2.0])
SampleCount < minSamples (default 5) → neutral factor 1.0 (preserve exploration)
```

`InMemoryStatsStore` uses EMA (default decay=0.2); under a multi-replica deployment each instance
accumulates independently — if cross-replica consistency is needed, swap the store for a Redis-backed
implementation; the interface stays the same.

## 9. Header quick reference

| Header | Meaning | Parsing rule |
|--------|------|----------|
| `X-Gateway-Fallback-Models` | cross-model fallback list (comma-separated) | dedup, order preserved, empty ignored, truncated beyond `MaxFallbackModels=3` |
| `X-Gateway-Max-Attempts` | client requests a tighter attempts cap | only takes effect when < the cfg default, **cannot** widen it |

**The client can only make the default stricter** — this is a config principle, to prevent a malicious
request from blowing up the gateway's attempts.

## 10. Where SchedulingDecision gets written

```go
rc.SchedulingDecision = &domain.SchedulingDecision{
    Model:       rc.ModelService.Model,   // the original requested model
    RoutedModel: routedModelOf(rc),       // the model actually hit (including fallback)
    UserGroup:   rc.Identity.Group,
    Attempts:    []domain.Attempt{...},    // one entry per Pick + Send
    DurationMs:  ...,
}
```

Each `Attempt`:

```go
type Attempt struct {
    Index       int        // 1, 2, 3 ... accumulates across models
    Model       string     // the model used for this attempt
    EndpointID  string
    AttemptRole string     // "primary" | "fallback"
    LatencyMs   int64
    ErrorClass  string
    Outcome     string     // success | fallback | fail
}
```

Outcome has three states derived: success = `success`; an intermediate failure = `fallback`;
the final failure = `fail`.

## 11. Where metrics get written

| Metric | Labels | Write location |
|--------|------|---------|
| `scheduling_duration_seconds` | model, attempts | at the M7 thin adapter's defer, when it ends |
| `invoker_attempts_total` | model, routed_model, vendor, endpoint_id, attempt_role, result, error_class | after every Invoker.Invoke (dispatch adapter) |
| `rate_limit_decisions_total` | scope="endpoint", dimension, result="violated" | when EndpointQuota.Reserve goes over limit |
| `rate_limit_charge_total` | dimension="tpm", result | at EndpointQuota.ChargeUsage |
| `tpm_overflow_total` | layer="endpoint", dimension="tpm" | when endpoint TPM post-deduction overflows |
| `rate_limit_fail_open_total` | scope="endpoint", dimension="any" | when LimitReadFilter fails open on a Redis error |
| `llm_gateway_repo_cache_total` | table, result | repo TTL LRU cache hit/miss/error |

Dispatch also has internal OTel spans (`dispatch.request` / `dispatch.attempt`), with attrs including
model / endpoint.id / vendor / verdict.{stage,class,http_code,reason} / dispatch.outcome
/ dispatch.routed_model / dispatch.attempts — see [08 §4](./08-observability.md#4-tracing) for details.

The complete metric contract is in [08-observability.md §3](./08-observability.md#3-metrics).

## 12. Wiring points (`internal/app/gateway/app.go` + `dispatch.go`)

The actual wiring happens in two layers: first assemble the primitives for selector / invoker / ratelimit
individually, then feed them into `buildDispatcher` to compose the `dispatch.Dispatcher`, and finally
inject the Dispatcher (**not** the selector / sender) into the M7 middleware.

```go
// === main.go prepares primitives ===

// 1. Cooldown manager
cooldown := selector.NewRedisCooldownManager(rdb, selector.CooldownDurations{...})

// 2. Filter chain (in the order of cfg.Selector.Filters)
filters := buildSchedulerFilters(cfg.Selector.Filters, rateStore, cooldown)

// 3. Scorer + Stats (optional)
stats, scorer := buildScoring(cfg.Scoring)

// 4. Scheduler primitives (selector.Scheduler interface; pure in-batch Pick + Report)
sched := selector.New(selector.Config{
    Filters: filters, Picker: selector.NewWeightedRandomPicker(),
    Cooldown: cooldown, Scorer: scorer, Stats: stats,
})

// 5. Sender primitives (invoker.Sender; pure HTTP Do + forward)
sender := invoker.New(senderOpts...)

// === dispatch_wiring.go composes the primitives into a Dispatcher ===

dispatcher := buildDispatcher(
    adaptEndpoints(endpointReader),  // CandidateSource bridge for repo.EndpointReader
    sched,                            // Selector bridge (internal/dispatch/adapters/SelectorAdapter)
    sender,                           // InvokerFactory bridge (adapters/InvokerFactoryAdapter)
    rateStore,                        // EndpointQuota bridge (adapters/EndpointQuotaAdapter; ratelimit.Store)
    cfg.Selector.MaxAttempts,         // AttemptCap.Default
    dispatchTracer,                   // OTel tracer, spans dispatch.request / dispatch.attempt
)

// inside buildDispatcher:
//   dispatch.New(
//       dispatch.WithCandidates(candidates),
//       dispatch.WithSelector(adapters.NewSelector(sched)),
//       dispatch.WithInvokerFactory(adapters.NewInvokerFactory(sender)),
//       dispatch.WithQuota(adapters.NewEndpointQuota(rateStore)),
//       dispatch.WithCap(dispatch.HeaderAttemptCap{Default: maxAttempts}),
//       dispatch.WithRetry(dispatch.DefaultRetry{}),
//       dispatch.WithFallback(dispatch.ModelChainFallback{}),
//       dispatch.WithTracer(tracer),
//   )

// === injected into router.Deps; M7 middleware only sees the Dispatcher, not selector / sender ===
Dispatcher: dispatcher,
```

`buildSchedulerFilters` maps the string names from yaml to Filter instances:

| Name | Implementation |
|------|------|
| `cooldown` | `NewCooldownFilter(cd)` |
| `limit_read` | `NewLimitReadFilter(rateStore)` |
| `prefix_cache` | `NewPrefixCacheFilter(0)` (vnodes=64) |
| `busy` | `NewBusyFilter(0)` (threshold=0.85) |
| `weighted_random` | ignored (it's already the Selector, configured separately) |
| anything else | **panic** (fail-fast to expose config errors) |

## 13. Config YAML quick reference

```yaml
selector:
  max_attempts: 3
  filters:                  # order-sensitive
    - cooldown              # cheapest filter, put it first
    - limit_read            # endpoint quota read-only filter
    # - busy                # optional: self-hosted load threshold
    # - prefix_cache        # optional: pick either this or weighted_random
  cooldown:
    transient: 30s
    capacity:  10s
    permanent: 5m
    invalid:   0s           # no cooldown (semantics in §4)
    unknown:   0s           # no cooldown (semantics in §4)

scoring:
  enabled: false            # off by default; enabling switches to runtime scoring
  ema_decay: 0.2
  min_samples: 5
  latency_baseline: 200ms
```

In cooldown durations, 0 = no cooldown; a deployer setting invalid / unknown to 0 is the recommended default.

## 14. Evolution rules (aligned with 03 §12 / abridged)

1. Cross-model fallback can only come from a client header, never implicitly demoted by the gateway's default path.
2. When adding new endpoint Protocol / Capabilities.Modalities config, extend eligibility first, and only then let the request fall into retry/cooldown.
3. To add a new Filter: implement `selector.Filter` → register the name in `cmd/gateway/buildSchedulerFilters` → add a yaml field.
4. To add a new Scorer / Stats implementation: the interface is in `internal/selector/scorer.go`; when cross-replica consistency is needed, swap InMemoryStatsStore for a Redis implementation, interface unchanged.
5. `internal/selector` never holds a repo dependency; anything that needs to query SQL belongs in a dispatch port adapter or cmd wiring.
6. Runtime Scoring can only adjust `EffectiveWeight`; it cannot eliminate candidates, let alone introduce a per-request state machine.
7. **Don't push responsibilities back into middleware**: candidate fetching, eligibility, retry/fallback decisions, quota
   reserve/charge all live in `dispatch.Dispatcher`. The M7 middleware is always a thin adapter,
   doing only RC ↔ dispatch.Input/Outcome mapping + content log enrichment + the overall metric.

## 15. Suggested reading order for the code

The fastest onboarding path is to read in this order:

1. [03-endpoint-scheduling.md](./03-endpoint-scheduling.md) §1 (flow diagram) → §3 (eligibility) → §6 (error classification)
2. `internal/middleware/schedule.go` `Schedule()` (thin adapter, ~165 lines, look at the RC ↔ Input/Outcome mapping)
3. `internal/dispatch/dispatcher.go` `Dispatch` / `step` (main scheduling-timing loop, ~250 lines)
4. `internal/dispatch/ports.go` (4 port interfaces ~150 lines; understand dispatch's seams)
5. `internal/dispatch/eligibility.go` (pure function ~70 lines)
6. `internal/dispatch/adapters/` (3 files ~200 lines: bridging selector / invoker / ratelimit into ports)
7. `internal/selector/types.go` (Candidate / Request / Result and other data structures)
8. `internal/selector/scheduler.go` `defaultScheduler.Pick / Report` (~50 lines of substantive logic)
9. Each Filter (cooldown / limit_filter / busy / prefix_cache), as needed
10. To understand runtime scoring, go back to `internal/selector/scorer.go`

After this pass, all the control flow + data flow of the schedule module will be in your head.
