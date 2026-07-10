# 03 — Endpoint Scheduling

This document records the M7 endpoint scheduling boundary. The scheduling layer's goal is not to build a generic policy framework, but to reliably deliver a request to one qualified endpoint:

1. Only attempt endpoints that can actually serve the current request.
2. Within the same model, select an endpoint by cooldown / endpoint quota / weight.
3. On failure, move to the next endpoint.
4. Cross-model fallback only runs when explicitly declared via a caller header.

## 1. Simplified boundaries

| Package | Responsibility |
|----|----|
| `pkg/middleware/model_service.go` (M5) | Parses `X-Gateway-Fallback-Models`, walks catalog + subscription per model, writes the validated sequence to `rc.ModelChain` |
| `pkg/middleware/schedule.go` (M7) | **thin adapter**: maps RC ↔ `dispatch.Input/Outcome`; content log enrichment; overall metrics. **Makes no scheduling decisions** |
| `pkg/dispatch` | **Sole owner of scheduling execution order**: Dispatcher.Dispatch / step main loop; 4 ports (CandidateSource / Selector / InvokerFactory / EndpointQuota) + 3 policies (AttemptCap / RetryPolicy / FallbackPolicy) + internal `filterEligible` helper |
| `pkg/dispatch/adapters/` | Bridges primitive packages into dispatch ports: selector → Selector / invoker → InvokerFactory / ratelimit → EndpointQuota |
| `pkg/selector` | Selection primitives: filter / score / pick over a batch of candidates. **Holds no repo, knows nothing about protocol / handler / fallback** |
| `pkg/invoker` | Takes a Handler and runs `PrepareCall + HTTP Do + response forwarding + error classification` (**does no protocol lookup**—dispatch has already obtained the Handler via `protocol.Lookup`) |
| `pkg/protocol` | Facade: `Handler = Factory + Translator + Quirks`; consumers only see `Handler / Lookup` |
| `pkg/ratelimit` | bucket / store primitives; `dispatch/adapters.EndpointQuotaAdapter` wires it into `dispatch.EndpointQuota` |
| `pkg/repo` | SQL endpoint reader + TTL LRU cached wrapper |

**Key boundary**: execution order (candidate fetch / eligibility / selection / pre-charge / invoke / report /
retry / fallback / post-charge) belongs to `dispatch.Dispatcher`, **not** middleware. M7
is always a thin adapter—it maps `*domain.RequestContext` into `dispatch.Input`,
calls `dispatcher.Dispatch(ctx, w, input)`, then maps `dispatch.Outcome` back onto RC (writing
`RoutedModelService` / `Usage` / `Error` / `SchedulingDecision`) + the HTTP response.

**Why split this way**:
- The scheduling sequence is an external contract that must not change (fallback chain / retry / quota / streaming are core gateway contracts)
  — putting it in an independent package with 4 ports lets unit tests avoid gin / RC and instead run against fake ports
- Middleware cares about request lifecycle + RC fields; once timing is extracted, schedule.go shrinks to ~165 lines
- Cross-model fallback lives in dispatch's outer reducer (the `Switch` action); M5 prepares
  `rc.ModelChain`, and dispatch consumes it in order; the selector knows nothing about the fallback concept

M5 has already prepared `rc.ModelChain = [primary, fb1, fb2, ...]` (a validated `*ModelService` sequence), which `dispatch.Dispatcher` consumes directly without any further catalog/subscription calls. Fallbacks that can't be found are already dropped at the M5 stage.

Actual execution flow (`pkg/dispatch/dispatcher.go`):

```text
# M7 middleware (thin adapter):
input := dispatch.Input{
    Envelope: rc.Envelope, Identity: rc.Identity,
    ModelChain: rc.ModelChain, Handlers: rc.Handlers, ...
}
outcome := dispatcher.Dispatch(ctx, w, input)
rc.RoutedModelService = outcome.RoutedModel
rc.SchedulingDecision = outcome.Decision
rc.Usage = outcome.Usage
// when outcome.Result != Streamed, write the error by HTTPCode / Class / Reason

# dispatch.Dispatcher.Dispatch (outer reducer):
state := newState(input, AttemptCap.Resolve(input))
for {
    switch action := step(ctx, w, state).(type) {
    case Continue: continue                       # reselect within the same model
    case Switch:   state.SetModel(action.Next)    # FallbackPolicy switches to the next model
    case Stream:   return state.Outcome()         # already StreamTo + ChargeUsage
    case Abort:    state.SetAbort(action); return state.Outcome()
    }
}

# dispatch.Dispatcher.step (a single attempt):
if state.Exhausted() → Abort{NoEndpoint, 503}
candidates := CandidateSource.ListForModel(ctx, model, group)
eligible   := filterEligible(candidates, env, handlers)         # pure function
if len(eligible) == 0 → FallbackPolicy.OnExhausted(state)
ep := Selector.Pick(ctx, eligible, query)                       # selector primitives
if denied := EndpointQuota.Reserve(ctx, ep) → Verdict + RetryPolicy.Decide
handler := state.Handlers().Get(ep, srcProto)                   # protocol.Lookup
res := InvokerFactory.For(ep, handler, env).Invoke(ctx)         # HTTP Do
state.Record(ep, verdict); Selector.Report(ctx, ep, verdict)
action := RetryPolicy.Decide(state, verdict)
if Stream → res.StreamTo(w); state.ApplyStream(rep); EndpointQuota.ChargeUsage(...)
return action
```

Cross-model fallback cannot bypass model visibility: M5 runs the full catalog + subscription validation for every fallback model before it can enter `rc.ModelChain`. If a fallback doesn't exist / isn't subscribed / hits a transient dependency error, it is **silently dropped** (the request is not blocked; primary has already been validated and the request continues).

## 2. Endpoint data

`domain.Endpoint` is a pure domain type; the repo is only responsible for converting SQL rows into domain objects.

Illustrative target fields:

```go
type Endpoint struct {
    ID      int64
    Name    string
    Vendor  string
    Model   string
    Group   string
    Weight  uint32
    Enabled bool

    Protocol Protocol          // upstream protocol for the endpoint (endpoint-level since v0.6)

    Auth         EndpointAuth
    Routing      EndpointRouting
    Quota        EndpointQuota
    Capabilities EndpointCapabilities // includes the Modalities subfield (v0.7)
    Quirks       json.RawMessage      // body / header tuning DSL, pkg/protocol/quirks
}
```

Candidate queries match enabled, non-soft-deleted endpoints by `(model, group)` and return them sorted by descending weight. Endpoints form a global pool with no account_id; primary-account visibility is handled in the M5 subscription stage.

`EndpointReader` shares its source of truth with M5's `ModelCatalog`; the production implementation follows
[06 §8 repo caching](./06-pluggable-infra.md#8-repo-cache-deployer-sql--gateway-data-propagation):
after a SQL change to the `endpoints` table, the gateway repo's in-process TTL LRU (30s by default)
naturally expires, and the next miss goes straight to SQL to fetch the new value.
`CachedEndpointReader` maintains both a list cache
(keyed by `"model\x00group"`) and an id cache; see [06 §8.2](./06-pluggable-infra.md#82-applicable-tables-and-default-parameters) for parameters.

`EndpointCapabilities.SelfHosted` determines `FormSelfHosted`; it is not inferred from the vendor name.

Endpoint field constraints:

- `Protocol` (core column, **required**): the upstream protocol this endpoint uses, e.g. `openai` /
  `anthropic` / `gemini` / `responses`. A zero value (ProtoUnknown) makes `DefaultLookup.Get`
  return nil → dropped by eligibility.
- `Capabilities.Modalities` (JSON column subfield, **explicit declaration recommended**, may be empty): the
  modality whitelist this endpoint actually serves, e.g. `["chat"]` / `["embedding", "rerank"]`.
  - Non-empty: narrows the vendor ceiling; eligibility requires **both this field and the vendor's
    `SupportedModalities`** to include the current request's modality (intersection; prevents a deployer from
    widening the vendor's actual capability)
  - Empty: for backward compatibility with old data / cases where declaration isn't wanted, eligibility falls back
    to the vendor's `SupportedModalities`

## 3. Candidate eligibility filtering

Candidate eligibility filtering must complete before entering `pkg/selector`. Rules:

1. If `protocol.Lookup.Get(ep, env.SourceProtocol)` returns nil → no Handler is available (missing
   vendor Factory, missing translator, or `ep.Protocol == ProtoUnknown`) → dropped.
2. Unsupported modality → dropped. The semantics are **narrow, never widen**: when both the endpoint's
   `Capabilities.Modalities` and the vendor's `Handler.Capabilities().SupportedModalities` are non-empty, **both must
   include** the current modality; if only one side is non-empty, check that side; if both are empty, there is no
   modality restriction.
   This guards against a deployer misconfiguration that would widen the vendor's actual capability.

These are not upstream failures, must not be reported to `Scheduler.Report`, and must not trigger cooldown. Otherwise an endpoint that simply "doesn't support the current request" gets mislabeled as a bad endpoint.

Eligibility filtering is a hard precondition of the dispatcher's driver loop. Missing Factory, protocol mismatch, missing translator, and modality mismatch are all cases of "lacking the capability to serve," and must not enter retry/cooldown.

Implemented in `pkg/dispatch/eligibility.go` (an internal dispatch helper, not a standalone package), as a pure
function; it takes `*domain.RequestEnvelope`, candidate endpoints, and `protocol.Lookup` as input and outputs
eligible endpoints. The dispatcher calls it inside `step()`, rather than inlining complex conditionals.

## 4. The selector only does in-batch selection

`pkg/selector` provides selection primitives—running filter / scorer / picker over a batch of candidates and
outputting one endpoint. It is **completely unaware** of dispatch / protocol / handler / fallback.

Interface shape (`pkg/selector/types.go`):

```go
type Scheduler interface {
    Pick(ctx context.Context, req *Request) (*domain.Endpoint, error)
    Report(ctx context.Context, ep *domain.Endpoint, result Result)
}

type Candidate struct {
    Endpoint        *domain.Endpoint
    EffectiveWeight float64
}

type Request struct {
    Model      string
    Group      string
    Candidates []Candidate
    ExcludeIDs map[int64]struct{}
    PrefixKey  []byte
}
```

`Request` carries no `LoadFallback` / `FallbackModels` / `attempts` state—these are all internal state of
`dispatch.Dispatcher` (the `attempts` / `excluded` / `modelChain` / `decisions` fields in the `state` struct).

Dispatch uses `pkg/dispatch/adapters/SelectorAdapter` to bridge `selector.Scheduler` into `dispatch.Selector` (which
accepts eligible endpoints + a PickQuery). The selector always receives a candidate list that is already eligible,
and only runs its own filter chain → scorer → picker.

`Pick` is stateless: it takes candidates + an exclusion set as input and outputs one endpoint. `Report` only feeds
failures back to cooldown / stats, and does not decide the next control-flow step (control flow belongs to
`dispatch.RetryPolicy.Decide` and `dispatch.FallbackPolicy.OnExhausted`).

Two pickers are available, selected via `selector.picker` in `gateway.yaml`:

- **`weighted_random`** (default) — picks 1 by an `EffectiveWeight` probability distribution. Always selects
  based on `Candidate.EffectiveWeight`, never reading `Endpoint.Weight` directly.
- **`p2c`** (power-of-two-choices) — samples two distinct candidates by `EffectiveWeight`, then takes the one
  with fewer pending calls (tracked by `selector.Inflight`: the scheduler increments on Pick and decrements on
  the matching Report, so the counter covers the window up to the upstream's response headers — exactly where
  an overloaded upstream queues). Ties go to the higher `EffectiveWeight`. This composes with Runtime Scoring:
  scoring shifts the sampling probability, P2C breaks the tie by live load. The counter is per-process; each
  replica balances its own view (the standard P2C deployment shape).

## 5. Retry model

Two layers are enough, maintained by `dispatch.Dispatcher`:

- **Switch endpoint within the same model** (dispatcher's inner `Continue` action): when a call fails with a
  retryable error, state adds the endpoint to `excluded`, and the next `step` naturally won't Pick it again.
- **Cross-model fallback** (dispatcher's outer `Switch` action): only when the request carries
  `X-Gateway-Fallback-Models` does `rc.ModelChain` have length > 1; `FallbackPolicy.OnExhausted`
  returns `Switch{Next: next model}` once the current model's candidates are exhausted, and the outer reducer
  switches model and continues to the next round of step.

Both layers are decided by `dispatch.RetryPolicy.Decide` / `dispatch.FallbackPolicy.OnExhausted`;
control flow is never scattered across middleware or the selector.

- `cap` (max attempts): decided by `AttemptCap.Resolve(input)`; the default implementation
  `HeaderAttemptCap` accepts a `X-Gateway-Max-Attempts` header override (**can only make the default stricter**).
- `excluded`: maintained by state, accumulating across models.
- `decisions`: maintained by state; each `Record(ep, verdict)` appends an entry; at the terminal state,
  `finalize()` writes it to `outcome.Decision` (filled even with 0 attempts—see the `dispatch.Outcome.Decision`
  contract for details).

L1 same-endpoint retry is unnecessary by default. Network jitter can be absorbed by other endpoints within the same
model; if same-endpoint retry does turn out to be needed in the future, it should be added back as an explicit
`RetryPolicy` implementation, not implicitly enabled internally.

`ClassInvalid` means the request itself is invalid (e.g. translator request conversion failed / quirks compile
failed); `DefaultRetry` returns `Abort{400}` directly, without retrying other endpoints or entering fallback models.

### Cross-model fallback

Compatibility between models is never guaranteed. Tool calling, structured output, context length, vision input,
reasoning parameters, and response style can all differ; the gateway cannot reliably judge whether a fallback model
meets business expectations.

Therefore cross-model fallback can only be explicitly given by the caller in the request:

```http
X-Gateway-Fallback-Models: gpt-4o-mini,deepseek-v3
```

The gateway only tries endpoints for these models in the declared order; it never performs automatic model
substitution, nor implicit downgrading based on some default chain. When this header is absent, the gateway only
switches endpoints within the current request's model, even if other models have available endpoints.

Header parsing + validation is entirely done in **M5 (`pkg/middleware/model_service.go`)**, and the result is
written to `rc.ModelChain`. M7 no longer reads the header or calls catalog/subscription. Rules:

- Deduplicate while preserving first-occurrence order; entries matching primary's name are also dropped.
- An empty model is simply ignored.
- A cap on the number of fallback models, defaulting to 3 (`middleware.MaxFallbackModels`).
- Every fallback model goes through catalog + subscription validation; if any check fails (not found / not
  subscribed / dependency error) → that fallback is **silently dropped**, without blocking the primary request.
- Failures validating primary itself still follow the original behavior and abort (404 / 403 / 503)—a fallback
  parsing failure cannot "rescue" an already-invalid primary.
- `rc.ModelChain[0] == rc.ModelService`, with length ≥ 1.
- `SchedulingDecision.Attempt` must record the model corresponding to that attempt; `AttemptRole` is assigned by
  position in the chain (`[0]` → `primary`, the rest → `fallback`).

## 6. Error classification

Dispatch uses `dispatch.Class` (the outer Verdict field), while the selector internally uses the semantically
equivalent `selector.ErrorClass`; the two are mapped bidirectionally in `pkg/dispatch/adapters/` (the
dispatch→selector direction is in `adapters/selector.go`, and the selector→dispatch direction is in
`adapters/invoker.go`'s `selectorClassToDispatch`), keeping dispatch independent of selector types and the selector
independent of dispatch types:

| Category | Meaning | Keep retrying? |
|------|------|--------------|
| `success` | HTTP 2xx and success at the protocol layer | No |
| `transient` | 5xx, network errors, timeout, DNS, etc. | Yes |
| `capacity` | 429 or overloaded | Yes |
| `permanent` | Upstream 401/403/config error from the selected endpoint | Yes, switch endpoint |
| `invalid` | Client-side 4xx or translation failure | No |
| `unknown` | Cannot be classified | Yes |

`pkg/invoker` converts HTTP / network / Handler `Classifier` results into this classification, and the dispatcher
feeds it back to `Scheduler.Report`.

An unregistered vendor Factory, `ep.Protocol == ProtoUnknown`, or an unregistered translator should all be dropped
at the candidate eligibility filtering stage (see §3), and must not be reported as an upstream `permanent` failure.

## 7. Filter chain

Currently retained filters:

- `cooldown`: excludes endpoints with recent short-term failures.
- `limit_read`: excludes endpoints that are over endpoint quota.
- `weighted_random`: picks an endpoint by weight.

`prefix_cache` / `busy` are self-hosted optimizations; their implementations may be kept, but they must not add to
the cost of understanding the main flow. They must be optional filters, placed after eligibility filtering.

`limit_read` may only do read-only filtering based on `SnapshotBatch`. Endpoint RPM/RPS reservation must happen
after the dispatcher has picked an endpoint (`EndpointQuota.Reserve`), not at the filter stage.

## 8. Runtime Scoring (opt-in, disabled by default)

**Implementation status**: shipped (`pkg/selector` DefaultScorer + EndpointStatsStore); `scoring.enabled` is off by
default. When enabled, `Scheduler.Report` writes EMA stats, and `Pick` adjusts `EffectiveWeight` using
success/latency factors. The stats store has two drivers: `inmemory` (per-replica, independent) and `redis`
(shared across replicas, consistent scoring)—see the `scoring:` section in 07-configuration. What follows are the
design principles.

Currently, default scheduling uses only the static `endpoint.weight`. This is simple and controllable, but it does
not factor runtime quality into selection:

- latency: recent-window average latency / p95 / EMA.
- success rate: recent-window success rate, and 5xx / 429 / timeout ratios.
- cost: cost multiplier across vendors/endpoints for the same model.

This should be added as soft scoring, not a hard filter. Hard filters decide "can this be selected", scoring
decides "which one is preferred".

Target flow:

```text
eligible candidates
  -> hard filters: cooldown / quota / busy-threshold
  -> scoring: latency / success_rate / cost adjust the effective weight
  -> weighted pick by effective_weight
```

Scoring should not be modeled as an ordinary `Filter`, because `Filter`'s semantics are "input candidates, output
candidates," which cannot express "adjust weight without eliminating." The target abstraction can be its own type:

```go
type Scorer interface {
    Score(ctx context.Context, candidates []Candidate, req Request) []Candidate
}
```

Once `Scorer` is introduced, `WeightedRandom` should select based on `Candidate.EffectiveWeight`, not
`Endpoint.Weight` directly.

Suggested first-version formula:

```text
effective_weight =
  base_weight
  * success_factor
  * latency_factor
  * cost_factor
```

Constraints:

- `base_weight` comes from the endpoint row's weight column, preserving manual operational control.
- `success_factor` / `latency_factor` use a sliding window or EMA, avoiding excessive influence from a single
  data point.
- `cost_factor` uses the cost weight from endpoint configuration or an offline-distributed multiplier; it must not
  look up prices or compute costs live on the scheduling hot path.
- Each factor has an upper/lower bound, e.g. `[0.1, 2.0]`, preventing any single metric from blowing up the weight.
- New endpoints lacking runtime data get a neutral factor of `1.0`, and retain a small amount of exploration traffic.

The source of runtime data should be an independent `EndpointStatsStore`, written asynchronously by
`Scheduler.Report` or by tracing/metrics. `Pick` only reads a lightweight snapshot, and does not perform slow
queries or complex aggregation.

`EndpointStatsStore` and Metrics are not the same layer: Metrics / Trace are observability output and keep rich
labels; `EndpointStatsStore` is the scheduler's internal read model, storing only per-endpoint aggregated EMA /
sliding-window summaries. `Scheduler.Report` can write to both metrics and the stats store, but `Pick` may only read
the stats store's lightweight snapshot.

### Endpoint quota

Endpoint quota must not be a candidate filter with side effects. The candidate stage may at most use
`SnapshotBatch` for read-only filtering; the actual deduction only happens after the dispatcher has picked an
endpoint (`EndpointQuota.Reserve` pre-charge / `ChargeUsage` post-charge).

After selection:

- Endpoint RPM/RPS uses `ReserveBatch` as a pre-charge; if over limit, it is reported as `capacity`, and that
  endpoint is excluded before selection continues.
- Endpoint TPM uses `ChargeBatch` as a post-charge once usage is produced; the post-charge does not affect the
  current response.

## 9. Cooldown

The gateway currently wires up a Redis cooldown manager, with durations coming from `scheduler.cooldown`
configuration:

- transient
- capacity
- permanent
- invalid
- unknown

`Scheduler.Report` best-effort marks cooldown for retryable failures; a failure to mark does not block the request.

Eligibility filtering failures never enter cooldown.

**Reset-aware TTL**: when the failed upstream response carries its own recovery hint, the cooldown TTL follows
the upstream instead of the static per-class duration. `pkg/invoker` parses, in priority order: `Retry-After`
(delay-seconds or HTTP-date), OpenAI-style `x-ratelimit-reset-requests` / `x-ratelimit-reset-tokens` (Go
durations; both buckets must clear, so the max of the pair wins), and Anthropic-style
`anthropic-ratelimit-*-reset` (RFC 3339, same max rule). The hint travels through
`invoker.Outcome.RetryAfter → dispatch.Verdict.RetryAfter → selector.Result.RetryAfter` into
`CooldownManager.Mark`, where it is clamped to `[1s, 10m]` — the floor absorbs sub-second churn, the cap stops
a pathological "retry in 24h" from poisoning the endpoint. A class whose configured duration is 0 stays opted
out; the hint never re-enables it.

A cooldown can also end early — see probe-gated recovery in §10.

## 10. Health Probing

`pkg/health.Prober` actively probes **self-hosted** endpoints (`capabilities.self_hosted=true` with
`capabilities.health_probe_endpoint` set) on a fixed interval (`health.enabled`, default off; see docs/07).
Vendor API endpoints are never probed — a probe there would burn quota against a third party.

Constraints (unchanged from the original design):

- Probe results cannot substitute for eligibility filtering; endpoints that don't support the protocol/modality
  must still be dropped.
- Probes only affect endpoint health signals; they never directly change business configuration.
- Probe results are written into `EndpointStatsStore` via the same path as `Scheduler.Report`, as one input to
  `success_factor` / `latency_factor`.
- The main request path never blocks on probing; passive cooldown remains the authoritative failure signal.

**Probe-gated recovery** (`health.recover_cooldown`, default off): with it enabled, the prober snapshots which
targets are in cooldown before each round; a **successful** probe of a cooling endpoint calls
`CooldownManager.Clear`, releasing it back into rotation before the TTL expires. Recovery is thus confirmed by
a probe instead of spending a user request to "test the waters" after the TTL runs out. This is strictly
release-only: a failed probe never creates or extends a cooldown (a probe failure and a business-call failure
are not the same signal — the probe URL may be down while inference still serves, and vice versa). Each early
release emits `llm_gateway_health_recover_total{endpoint_id}`.

## 11. SchedulingDecision

`dispatch.Outcome.Decision` is always produced by `state.finalize()` (filled even with 0 attempts—see the
Outcome.Decision contract for details):

```go
type Attempt struct {
    Index       int
    Model       string
    EndpointID  string
    AttemptRole string // primary | fallback
    Outcome     AttemptOutcome
    LatencyMs   int64
    ErrorClass  string
}
```

`AttemptRole` indicates whether the model for this attempt is the original request model (`primary`) or a fallback
model from `X-Gateway-Fallback-Models`; it is the same information source used by traces, the metric
`attempt_role` label (see [08-observability §3](./08-observability.md#3-metrics)), and alerting analysis.

Outcome derivation:

- success -> `success`
- last failure -> `fail`
- intermediate failure -> `fallback`

Attempts after a cross-model fallback continue to append to the same decision, with `Model` + `EndpointID` +
`AttemptRole` making the routing target of each attempt explicit.

## 12. Evolution rules

- `pkg/selector` only handles a single batch of candidates; it is not responsible for loading fallback models from the repo.
- Cross-model fallback comes only from `X-Gateway-Fallback-Models`; header parsing + catalog/subscription validation happens in M5, `dispatch.Dispatcher` consumes `rc.ModelChain` directly, and middleware only passes it through.
- When adding endpoint native protocol / modality configuration, add the candidate eligibility filter first, before letting requests enter retry/cooldown.
- Protocol incompatibility must never be classified as an upstream failure; doing so amplifies useless retries and pollutes cooldown.
- New filters must be registered by name in `cmd/gateway buildSchedulerFilters`, and kept optional.
- Runtime scoring may only adjust the effective weight; it must not reintroduce a per-request state machine.
