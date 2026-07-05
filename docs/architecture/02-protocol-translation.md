# 02 — Protocol Translation

This document records the abstraction and composition of protocol translation: client
protocol in, upstream protocol out (pre-call), upstream response back, client protocol
out (post-call). The two phases are wrapped under the `pkg/protocol.Handler` facade;
internally it is still composed of two independent sub-abstractions — `pkg/protocol`
(vendor HTTP layer) + `pkg/translator` (body shape layer) — but consumers only see the
Handler.

Core principles:

- Protocol ownership is an **endpoint-level** property (`Endpoint.Protocol`), not a
  vendor-level one.
- Handler is an end-to-end processor for the (endpoint, sourceProtocol) tuple; it is
  **dynamically composed** per request, not statically registered into a matrix in
  init().
- We do not aim to fill out an arbitrary `source × target` protocol matrix — an
  unregistered combination is simply treated as unsupported; eligibility filtering
  removes that endpoint, and the request either falls back or returns 503.

## 1. Abstraction relationships

```text
┌──────────────────────────────────────────────────────────────────┐
│ pkg/protocol.Handler  (facade, consumers only see this)           │
│                                                                  │
│   ┌──────────────────────────┐  ┌────────────────────────────┐   │
│   │ pkg/protocol.Factory      │  │ pkg/translator.Translator  │   │
│   │ (vendor HTTP layer)      │  │ (body shape conversion +    │   │
│   │  - Metadata              │  │  usage)                     │   │
│   │  - NewSession            │  │  - Source / Target          │   │
│   │  - Session.BuildRequest  │  │  - TranslateRequest          │   │
│   │                          │  │  - NewResponseHandler       │   │
│   └──────────────────────────┘  └────────────────────────────┘   │
└──────────────────────────────────────────────────────────────────┘
                 ▲
                 │ Combine(ad, tr) → Handler
                 │
        DefaultLookup.Get(ep, srcProto) composes dynamically at request time
```

## 2. End-to-end request pipeline

```text
Client request
  ↓
M3 Envelope: writes rc.Envelope (RawBytes / SourceProtocol / Modality)
            + rc.Handlers = protocol.DefaultLookup{}
  ↓
M5 ModelService: resolves model + fallback chain
  ↓
M7 Schedule → dispatch.Dispatcher.Dispatch(ctx, w, rc):
  loop {
    ep := Selector.Select(query)                                    // StageSelect
    handler := rc.Handlers.Get(ep, env.SourceProtocol)               // dynamically composed Handler
    if handler == nil { record StagePrepare; retry / fallback }
    
    invocation := InvokerFactory.For(ep, env, body, handler)
    res := invocation.Invoke(ctx)
      └─ reserve quota                                              // StageReserve
      └─ handler.PrepareCall(ep, srcBody) → Call{Request, UpstreamBody}  // StagePrepare
      └─ client.Do(req)                                             // StageInvoke
    
    if success: res.StreamTo(ctx, w)
      └─ handler.NewResponseStream().Feed/Flush — translates back to client protocol chunk-by-chunk
  }
```

## 3. `domain.Endpoint.Protocol`

**Required field**. When the deployer creates an endpoint (SQL INSERT), it must
explicitly declare which protocol the upstream of that endpoint speaks
(`openai` / `anthropic` / `gemini` / `responses` / ...); if missing or
`ProtoUnknown`, `DefaultLookup.Get` returns nil and eligibility removes that endpoint.

```go
type Endpoint struct {
    ...
    Vendor   string             // openai|anthropic|gemini|ark|... — vendor adapter selection
    Protocol domain.Protocol    // openai|anthropic|gemini|responses|... — protocol ownership
    ...
}
```

**Why protocol is endpoint-level rather than vendor-level**: the same vendor can host
multiple endpoints of different protocols at the same time. Example:

| vendor | endpoint.Protocol | translator needed |
|---|---|---|
| anthropic | anthropic | (Anthropic → Anthropic) identity |
| anthropic | openai     | (OpenAI → Anthropic) cross |
| openai | openai | (OpenAI → OpenAI) identity |
| openai | responses | (OpenAI → Responses) — n/a, actually the reverse |
| openai | anthropic | (Anthropic → Anthropic, only when the vendor runs an Anthropic-compatible API) |

The vendor adapter no longer declares `NativeProtocol` — it only knows HTTP-layer
details (auth header / URL / TLS); protocol ownership is left to the endpoint.

## 4. `pkg/protocol.Handler` — facade

```go
type Handler interface {
    Capabilities() Capabilities

    // pre-call: translate srcBody + wrap in vendor HTTP envelope
    PrepareCall(ctx, ep, srcBody) (*Call, error)

    // post-call: translate the response back to the client chunk-by-chunk
    NewResponseStream() ResponseStream
}

type Call struct {
    Request      *http.Request  // HTTP request ready to send to upstream
    UpstreamBody []byte         // translated byte copy (for audit/hook)
}

type Capabilities struct {
    SourceProtocol      domain.Protocol   // = translator.Source()
    UpstreamProtocol    domain.Protocol   // = translator.Target() == ep.Protocol
    SupportedModalities []domain.Modality // = protocol.Metadata().SupportedModalities
}
```

**Capabilities carries no Vendor** — Vendor is a property of the endpoint, not of the
Handler; the Handler is a dynamic (adapter, translator) composition, and it only
touches the specific endpoint once passed as the `PrepareCall` argument.

## 4a. Quirks — endpoint-level request tweaks (`pkg/protocol/quirks`)

`translator` is only responsible for shape conversion from "client protocol → upstream
protocol"; within the same upstream protocol, different vendors / models can still have
subtle differences. **All quirks are deployment knowledge, stored in the
`endpoints.quirks` JSON column, configured directly via SQL by the deployer** — no
vendor rule is registered in code via init().

Two typical categories of differences:

**Body fields**
- OpenAI o1/o3/o4 reasoning models: `max_tokens` → `max_completion_tokens`; strip
  `temperature` / `top_p` / `presence_penalty` / `frequency_penalty`
- DeepSeek `deepseek-reasoner`: similar restrictions
- Anthropic Claude 3.7+ extended_thinking: insert a `thinking` block + force
  `temperature=1`
- vLLM / Ollama: strip certain OpenAI-specific fields

**Header fields**
- Different vendors use different trace-id header names (`X-Request-Id` /
  `X-Ark-Request-Id` / `x-ds-request-id`, etc.) — the gateway uniformly uses
  `X-Request-Id`, and the deployer configures a rename so the upstream receives the
  header name it recognizes
- Vendor-private headers (e.g. `X-API-Version`) are hard-configured on the endpoint

Inserted between the translator and the adapter — body and header both run through in
one pass before being handed to the adapter for final assembly:

```text
client body
  → translator.TranslateRequest          (client protocol → upstream protocol shape)
  → ep.Quirks.RewriteBody  + RewriteHeader  ← 4a (both segments run in one pass)
  → protocol.Session.BuildRequest(body, headers)   (HTTP envelope + merge quirks headers)
  → upstream
```

**Adapter merge rule**: after copying quirks headers into req.Header, the adapter then
writes its own protocol-required headers (Auth / Content-Type / vendor version header,
etc.) — **last write wins**. This means:
- The deployer can add arbitrary vendor-private headers (X-Vendor-Tag, etc.)
- If the deployer mistakenly overwrites Authorization with something else, it won't
  break the request (the adapter overwrites it back as a safety net)

**DSL** (stored in the `endpoints.quirks` JSON column):

```json
{
  "body": {
    "rename":      {"max_tokens": "max_completion_tokens"},
    "strip":       ["temperature", "top_p"],
    "set":         {"reasoning_effort": "high"},
    "set_default": {"max_completion_tokens": 4096}
  },
  "headers": {
    "rename":      {"X-Request-Id": "X-Ark-Trace-Id"},
    "strip":       ["X-Internal-Debug"],
    "set":         {"X-Custom-Tag": "prod"},
    "set_default": {"User-Agent": "llm-gateway/1.0"}
  }
}
```

The application order within the body / headers sub-sections is fixed:
`rename → strip → set → set_default` (make room first, then clean up, then override,
then finally fill defaults).

Interface (`pkg/protocol/quirks/quirks.go`):

```go
type Rewriter interface {
    RewriteBody(body []byte) ([]byte, error)
    RewriteHeader(h http.Header)
}

// Compiles endpoint.Quirks JSON → Rewriter; strict mode (typo'd fields error out immediately).
func CompileJSON(specJSON []byte) (Rewriter, error)
```

**combine.go caching**: the same spec literal (`string(ep.Quirks)`) is compiled only
once, and the resulting Rewriter is shared across requests; different endpoints with
identical quirks configuration also share it.

**NULL column / empty JSON / `{}`** → no-op Rewriter, zero overhead.

**Handling deployer misconfiguration**: if the spec JSON fails to parse (or has an
unknown field typo), requests to that endpoint return a `PhaseQuirks` PrepareError
(`dispatch.ClassInvalid`), and the dispatcher aborts directly without retrying. A
misconfigured endpoint will always error, so pinning it to a metric / log is enough to
locate it.

## 5. PrepareCall failure classification

```go
type PreparePhase int
const (
    PhaseTranslate PreparePhase = iota  // translator.TranslateRequest failed
    PhaseQuirks                         // quirks.Rewrite failed (vendor / model-level body tweak)
    PhaseBuild                          // adapter session BuildRequest / NewSession failed
)

type PrepareError struct {
    Phase PreparePhase
    Err   error
}
```

- **PhaseTranslate**: `srcBody` does not match the `SourceProtocol` schema →
  `dispatch.ClassInvalid` → the caller should abort with 400 directly (switching
  endpoints for the same request would fail the same way)
- **PhaseQuirks**: vendor / model-level body Rewriter failed (see §4a for details) →
  `dispatch.ClassInvalid` → the caller should abort directly (retrying the same
  request would fail the same way)
- **PhaseBuild**: vendor HTTP construction failed (rare; usually an invalid endpoint
  configuration such as an unparseable URL) → `dispatch.ClassPermanent`

`invoker.Sender.Send` uses `errors.As(*PrepareError)` to route to different
`Outcome.Class` and return values; the wiring layer marks both cases with
`Verdict.Stage = StagePrepare`, so Policy can distinguish them from an "upstream call
failure."

## 6. Lookup: dynamic composition

```go
type Lookup interface {
    Get(ep *domain.Endpoint, srcProto domain.Protocol) Handler
}

type DefaultLookup struct{}

func (DefaultLookup) Get(ep *Endpoint, src Protocol) Handler {
    if ep == nil || ep.Protocol == ProtoUnknown {
        return nil
    }
    ad := protocol.LookupFactory(ep.Vendor)
    if ad == nil {
        return nil
    }
    // direct route preferred; on miss, fall back via pivot (OpenAI) composition, see §6a
    tr := translator.FindVia(src, ep.Protocol, ProtoOpenAI)
    if tr == nil {
        return nil
    }
    return Combine(ad, tr)   // result goes into the package-level handlerCache (vendor|src|tgt)
}
```

**Request-level injection**: M3 Envelope fills `rc.Handlers` with the default
`DefaultLookup{}`; in multi-tenant / canary scenarios, subsequent middleware (e.g. M2
Auth) can override `rc.Handlers` per tenant with a custom Lookup implementation
(restricting available vendors / a custom translator chain).

The dispatcher / invoker / eligibility all go through `dispatch.HandlersFrom(rc)` to
get the typed Lookup, and never consume the adapter / translator registry directly.

## 6a. Fallback on missing pairs: pivot composition (governing the Cartesian product)

Of the three growth axes of the protocol pair matrix, **the vendor axis has already
been collapsed** (protocol belongs to the endpoint level + OpenAI-compatible aliases
share a Factory + quirks absorb vendor differences — adding a new vendor is O(1) and
does not enter the matrix). What remains, client protocol × upstream protocol, is a
slow-growing axis, but it still grows multiplicatively as more protocols are onboarded.
The governance strategy has two layers:

**Layer 1: direct translator (high fidelity, preferred)** — for every (src, tgt) pair
that has real traffic, hand-write a `pkg/translator/<src>_<tgt>/` that fully maps the
protocol-specific fields (thinking blocks / cache_control / tool schema, etc.).

**Layer 2: pivot composition (fallback, potentially lossy)** — `translator.FindVia(src,
tgt, pivot)` attempts `Compose(Find(src, pivot), Find(pivot, tgt))` when the direct
route misses:

```text
Request direction: src body → front(src→openai) → openai body → back(openai→tgt) → tgt body
Response direction: tgt chunks → back.handler(tgt→openai) → openai body → front.handler(openai→src) → src body
```

- The pivot is fixed to the **OpenAI protocol** (the de facto industry lingua franca;
  every existing cross-protocol pair already has it on one end, so when onboarding a
  new protocol, writing its conversion pair with OpenAI first maximizes composed
  coverage automatically)
- **Direct routes always take precedence over composition**: `FindVia` checks the
  direct route first; once a direct implementation is added for a popular pair, it
  automatically takes over, transparently to the caller
- Usage extraction preferentially takes the upstream-side handler (closest to the real
  response; what the client side sees is the secondhand pivot bytes)
- Composed handlers log `slog.Warn` on creation (handlerCache guarantees only one warn
  per (vendor, src, tgt)) — **lossy**: fields the pivot cannot express are lost across
  the double hop
- If either leg is missing → returns nil → eligibility removes it as usual, same
  behavior as before

**Current coverage**: 7 direct pairs; composition automatically fills in
anthropic→gemini, responses→anthropic, and responses→gemini (7/12 → 10/12); the
remaining two *→responses pairs cannot be composed because there is no
`openai→responses` translator at all — a direct implementation should be added under
Layer 1 once real demand appears.

**Evolution discipline**:
- Frequent composition warns for an (src, tgt) pair = a signal to add a direct
  translator
- **Do not** jump to building a canonical IR (global intermediate representation) just
  because composition is available — the project already removed
  `Envelope.Canonical` in v0.5 and won't go back down that path: a full-fidelity IR
  double hop is fully lossy + adds streaming complexity, which is worse than the
  two-layer structure of "direct high fidelity + composition fallback"

## 7. `pkg/protocol` — vendor HTTP layer (internal detail of the facade)

```go
type Metadata struct {
    Vendor              string            // openai|anthropic|gemini|ark|...
    SupportedModalities []domain.Modality // chat|embedding|image|...
}

type Factory interface {
    Metadata() Metadata
    NewSession(ctx, ep, env) (Session, error)
}

type Session interface {
    BuildRequest(body []byte) (*http.Request, error) // body = bytes already translated by translator
    Close() error
}

type Classifier interface {  // optional
    Classify(status int, body []byte) *domain.AdapterError
}
```

**The adapter no longer declares NativeProtocol** — v0.5 put it on Metadata as the
vendor default protocol, v0.6 removed it, and protocol ownership moved to
`Endpoint.Protocol`.

`Classifier` implementations automatically surface through the `protocol.Handler`
interface: when a vendor adapter implements Classifier, the Handler produced by
`Combine(ad, tr)` automatically satisfies `protocol.Classifier`, and the invoker
type-asserts and calls it on non-2xx HTTP responses.

Vendor sub-packages:

- `pkg/protocol/openai/` — vendor=openai + alias=ark
- `pkg/protocol/anthropic/`
- `pkg/protocol/gemini/`

Each vendor sub-package's `init()` only calls
`protocol.RegisterFactory("<vendor>", Factory{})`; the Handler is dynamically
synthesized by `DefaultLookup` at request time, not registered into a matrix in
init().

## 8. `pkg/translator` — body shape layer (internal detail of the facade)

```go
type Translator interface {
    Source() domain.Protocol // client protocol accepted
    Target() domain.Protocol // upstream protocol translated to (matches Endpoint.Protocol)

    TranslateRequest(srcBody []byte) ([]byte, error)
    NewResponseHandler() ResponseHandler
}

type ResponseHandler interface {
    Feed(chunk []byte) (clientBytes []byte, err error)
    Flush() (clientBytes []byte, usage *domain.Usage, err error)
}
```

**Registration**: each translator sub-package's `init()` calls
`translator.Register(...)`; the global table is fully populated at startup;
`translator.Find(src, tgt)` queries it at runtime. `DefaultLookup.Get` uses this to
dynamically obtain the translator.

Built-in translators:

| src → tgt | package | purpose |
|---|---|---|
| OpenAI → OpenAI | `translator/identity` | identity passthrough (injects stream_options.include_usage) |
| Anthropic → Anthropic | `translator/identity` | identity passthrough |
| Responses → Responses | `translator/identity` | identity passthrough |
| OpenAI → Anthropic | `translator/openai_anthropic` | client OpenAI SDK → Anthropic upstream |
| Anthropic → OpenAI | `translator/anthropic_openai` | client Anthropic SDK → OpenAI upstream |
| OpenAI → Gemini | `translator/openai_gemini` | client OpenAI SDK → Gemini upstream |
| Responses → OpenAI | `translator/responses_openai` | Responses entry point wired to a Chat Completions endpoint |

**We do not require every combination to be filled in**. Priority:

1. Same-protocol identity: when the client protocol matches `ep.Protocol`, pass through
   as much as possible.
2. Cross-protocol combinations with clear, established business need: those listed in
   the table above.
3. Unregistered combinations → `DefaultLookup.Get` returns nil → eligibility removes it
   → that endpoint does not participate in this request.

## 9. eligibility filtering

The internal helper `pkg/dispatch.filterEligible` filters endpoint candidates using a
single `protocol.Lookup` argument:

```go
for ep in candidates {
    h := handlers.Get(ep, env.SourceProtocol)
    if h == nil:
        removed (handler_missing)
    if !endpointSupportsModality(ep.Capabilities.Modalities, h.Capabilities().SupportedModalities, env.Modality):
        removed (modality_unsupported)
    eligible
}
```

`Capabilities.Modalities` on the endpoint is an endpoint-level allowlist that can only
narrow the vendor's `SupportedModalities`, never widen it; for the exact intersection
semantics see [03 §3](./03-endpoint-scheduling.md#3-candidate-eligibility-filtering).

The old v0.5 shape did two lookups (vendor / translator) plus a match check; as of
v0.6+ this has been merged into a single Handler lookup.

## 10. invoker flow

```go
func (s *Sender) Send(ctx, ep, env, srcBody, handler) (Outcome, error) {
    fire hook(ClientRequest)
    
    if handler == nil {
        return Outcome{Stage: StagePrepare, Class: ClassPermanent}
    }
    
    call, err := handler.PrepareCall(ctx, ep, srcBody)
    if err != nil {
        return Outcome{Stage: StagePrepare, Class: <PhaseTranslate→Invalid | PhaseBuild→Permanent>}
    }
    
    fire hook(UpstreamRequest, call.UpstreamBody)
    
    resp := client.Do(call.Request)
    class := classifyHTTPStatus(resp.StatusCode)
    if h, ok := handler.(protocol.Classifier); ok {
        class = h.Classify(resp.StatusCode, peekBody(resp))  // refine
    }
    
    return Outcome{Stage: StageInvoke, Response: resp, Handler: handler, Class: class}
}

func (s *Sender) Forward(ctx, w, ep, resp, stream protocol.ResponseStream) ForwardResult {
    for chunk := resp.Body.Read():
        out := stream.Feed(chunk)
        w.Write(out); flush
    final := stream.Flush()
    w.Write(final)
}
```

`Outcome` no longer carries a `Translator` field; it has been replaced with `Handler`,
and the caller uses `outcome.Handler.NewResponseStream()` to obtain the ResponseStream
to pass to Forward.

## 11. dispatch.Verdict.Stage

```go
type Stage int
const (
    StageInvoke   Stage = iota // HTTP call (default)
    StageSelect               // endpoint selection failed
    StagePrepare              // protocol translation / HTTP construction failed
    StageReserve              // ratelimit pre-deduction failed
)
```

Policy.Decide can make finer-grained decisions based on Stage — for example, a
StagePrepare failure means `ep.Protocol` doesn't match srcProto; there's no point
retrying the same endpoint, so it can Switch directly to the next model or Abort.

## 12. Steps for adding a new vendor / endpoint

1. Implement `protocol.Factory` and `protocol.Session` in `pkg/protocol/<vendor>/`.
2. Call `protocol.RegisterFactory("<vendor>", Factory{})` in `init()`.
3. If the protocol the client will use doesn't match the vendor's upstream protocol,
   and `pkg/translator/<src>_<dst>/` isn't registered yet — add a new translator
   implementation and register it in `init()`.
4. Add a blank import in `cmd/gateway/main.go`:
   - `_ "github.com/zereker/llm-gateway/pkg/protocol/<vendor>"`
   - `_ "github.com/zereker/llm-gateway/pkg/translator/<pair>"` (identity is already
     imported by default)
5. Rebuild and restart the gateway process.
6. Deployer creates the endpoint via SQL INSERT: `vendor` must match the registered
   name; `protocol` is required and declares which protocol the upstream of that
   endpoint speaks.

## 13. Evolution rules

- Protocol ownership is always endpoint-level; do not restore NativeProtocol on the
  vendor adapter.
- Do not statically register a (vendor, srcProto) Handler matrix in init() — keep
  runtime dynamic composition, so that overriding rc.Handlers can affect all paths.
- Multiple protocol capabilities on the same vendor → multiple endpoint rows, each
  with its own Protocol set.
- Do not add a translator just for "matrix completeness" — only add one when there is
  a real business need and no endpoint runs the native protocol.
- Do not push protocol translation logic back into the adapter — the adapter is always
  only responsible for the HTTP layer.
- New translators must have test coverage for request translation, response handler,
  usage extraction, and error paths.
- Do not restore a global canonical request unless there is a clear consumer and a
  field-fidelity strategy.
