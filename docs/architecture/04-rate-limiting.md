# 04 — Rate Limiting

This document records the design goals for rate limiting. Rate limiting is split into two categories:

1. **User-side quota**: account / API key / model dimensions, handled by M6.
2. **Endpoint quota**: the upstream endpoint's own capacity, handled by M7 after an endpoint is selected.

Rate-limit buckets only serve flow control, not billing ledgers. Billing is based on the usage outbox.

## 1. Design Principles

- M6 only handles user-side quota; it does not mix in endpoint quota.
- M7 only does endpoint quota reserve for the finally selected endpoint; it does not deduct from all endpoints during the candidate filter stage.
- User-side RPM / RPS are reserved before the request.
- TPM is not pre-deducted before the request, does not read `max_tokens`, and is only deducted after the response based on real usage; it is an after-the-fact counter and does not guarantee a token ceiling before the request starts.
- `X-RateLimit-*` headers are not returned; rate-limit state is not exposed as a client contract.
- Redis is the only Store in production; multi-replica gateways must share the counters.

## 2. M6 User-Side Rate Limiting

M6 sits after M5 ModelService and before M7 Schedule.

Flow:

```text
identity = rc.Identity
model = rc.ModelService.Model

rules = load account quota policy + api key quota policy
buckets = build user RPM/RPS buckets by additive policy
ReserveBatch(buckets)
if denied:
    return 429 + Retry-After

c.Next()

usage = rc.Usage  # M7 thin adapter writes back from dispatch.Outcome; nil means usage was not extracted
if usage != nil:
    build user TPM buckets by additive policy
    ChargeBatch(TPM buckets with cost = usage.Total)
```

RPM / RPS are pre-deducted: cost is fixed at 1, known before the request starts, used to block request surges.

**Pre-deduction is not refunded (an explicit trade-off)**: RPM/RPS reserve is a "funnel count" rather than a "reserved quota" — even if the request subsequently fails on the gateway side (dispatch 503 / all upstreams down / M8 moderation rejection), the 1 slot already reserved **is not rolled back**. Reasons: (1) the sliding window counter will naturally expire that count within the window length, so the over-count is bounded and self-healing; (2) a compensating refund would need to precisely pair reserve/refund on every failure path, across multiple layers of middleware + dispatch, including panic recover — getting the pairing wrong would instead cause double-refunds that blow through the rate limit; (3) the semantics of rate limiting are inherently "the rate of requests entering the system" rather than "the rate of successful responses" — a client that keeps hitting 503 consuming RPM quota is expected behavior, since it prevents that client from retry-flooding at no cost. Where billing needs to be based on success, the usage outbox is authoritative, not the rate-limit bucket. Endpoint-side RPM/RPS (§10) likewise are not refunded.

TPM is post-deducted: the real token count is only known after the request completes, so no pre-reserve is done. A TPM post-deduction failure does not change the current response. The user-side `ReserveBatch` by default does not read the TPM bucket, so exceeding the TPM limit itself does not reject subsequent requests; it is used for after-the-fact observation, reporting, and operational alerting. Businesses that need a hard token ceiling should configure stricter RPM/RPS or separately introduce an explicit TPM soft-check scheme.

## 3. Data Sources

Identity comes from M2:

```go
type UserIdentity struct {
    AccountID             string
    SubAccountID          string
    APIKeyID              string
    Group                 string
    ExternalUser          bool
    AccountQuotaPolicyID  *int64
    APIKeyQuotaPolicyID   *int64
}
```

The quota policy comes from the SQL table `quota_policies`, whose `rule_json` is parsed into:

```go
type PolicyRule struct {
    Default  *QuotaConfig           `json:"default,omitempty"`
    PerModel map[string]QuotaConfig `json:"per_model,omitempty"`
}
```

Example:

```json
{
  "default": {"rpm": 60, "tpm": 100000},
  "per_model": {
    "gpt-4o": {"rpm": 10, "tpm": 30000}
  }
}
```

## 4. Additive Semantics

`PolicyRule.PickRulesAdditive(model)` returns both:

- the default rule, with scope `*`
- the matched per-model rule, with scope being the current model

When both exist, both are consumed simultaneously. This way per-model acts as a sub-limit of default, avoiding a situation where "matching per-model bypasses the overall cap."

The account layer and the API key layer are also consumed simultaneously; the two layers of policy are independent of each other and apply additively. For pre-deducted buckets like RPM/RPS, if either exceeds the limit the whole batch is rejected; the TPM bucket is a post-deduction counter and does not participate in pre-request rejection.

## 5. Redis Store Interface

Target interface:

```go
type Store interface {
    ReserveBatch(ctx context.Context, buckets []Bucket) (allowed bool, violated *BucketViolation, err error)
    ChargeBatch(ctx context.Context, buckets []Bucket) ([]BucketChargeResult, error)
    SnapshotBatch(ctx context.Context, buckets []Bucket) ([]BucketState, error)
}
```

`ReserveBatch` is used for pre-request rate limiting; it is a multi-key atomic all-or-nothing operation — if any bucket exceeds its limit, the whole batch is not written, and the caller returns 429 or switches endpoints. The algorithm uses a sliding window counter, avoiding the 2x burst at fixed window boundaries.

`ChargeBatch` is used for post-response accounting and must write the actual usage; even if the write results in exceeding the limit, the already-completed response cannot be rejected. The return value can flag which buckets are already over the limit, for use in logging, metrics, and operational alerting.

`SnapshotBatch` is read-only, used for subsequent endpoint quota / observability scenarios to read current state, without any deduction side effects.

## 6. Bucket Naming

M6 user-side buckets:

```text
rl:quota:<layer>:<subject>:<scope>:<dim>
```

Field meanings:

- `layer`: `account` or `apikey`
- `subject`: the primary account pin or api_key_id
- `scope`: `*` or the actual model
- `dim`: `rpm`, `tpm`, `rps`

Example:

```text
rl:quota:account:default:*:rpm
rl:quota:account:default:gpt-4o:tpm
rl:quota:apikey:ak_alice:*:rps
```

M7 endpoint-side buckets:

```text
rl:endpoint:<endpoint_id>:<dim>
```

Endpoint quota is not exposed to clients; it is only used to protect upstream capacity.

## 7. TPM Post-Deduction

Estimating TPM from the request body is no longer used:

- `max_tokens` is not read.
- A global default output token count is not used.
- `input_chars / 4 + max_tokens` pre-deduction is not done.
- Requests are not rejected early because the estimate is too large.

After the M7 thin adapter writes `dispatch.Outcome.Usage` back to `rc.Usage`, M6's post-side writes the user-side TPM using the real value:

```text
usage = rc.Usage
cost = usage.Total
ChargeBatch(TPM buckets, cost)
```

If `usage == nil`, the TPM bucket is not deducted for this request. This case should be gradually reduced through usage extractor / translator coverage.

The trade-offs of TPM post-deduction:

- Pros: normal requests are not wrongly blocked due to over-estimation; the implementation is simpler; it does not depend on the client's `max_tokens`.
- Cons: under high concurrency it may exceed the TPM limit, and exceeding the limit does not automatically block subsequent requests; this is an explicit trade-off — billing is still based on the usage outbox.

If `ChargeBatch` finds that the write causes the TPM limit to be exceeded, it must record `llm_gateway_tpm_overflow_total{layer,dimension}`, for operations to observe how many times "post-deducted tokens have exceeded the configured limit."

## 7a. Redis Deployment Shape Limitations

The rate-limiting script is a **multi-key EVAL with no hash tag** (`rl:quota:account:*` and `rl:quota:apikey:*`
fall on different slots) — **incompatible with Redis Cluster**; switching to it would cause the very first batch of requests to hit CROSSSLOT errors,
and M6's fail-closed behavior would amplify this into a full-scale 503. Supported deployment shapes: single instance / master-replica + Sentinel /
a proxy layer that aggregates (e.g. Twemproxy won't work — only a proxy that supports EVAL across keys will). To actually move to Cluster
you'd need to first introduce a `{account}` hash tag into the bucket key and batch EVAL by subject — this is recorded as a known
evolution item; do not point at Cluster before doing so.

Also, the script uses the gateway's local clock for window boundaries (`ARGV = time.Now().Unix()`): clock skew between replicas
will cause over-admission bounded by ≤ skew/window — negligible for an NTP-synced fleet.

## 8. Redis Failure Behavior

Redis is a production dependency for rate limiting and cooldown; if it cannot be connected to at startup, it fails fast. Runtime failures are handled differently depending on the call site:

| Call Site | Default Behavior | Notes |
|--------|----------|------|
| M6 user-side `ReserveBatch` | fail-closed, returns 503 + `Retry-After` | when quota cannot be confirmed, do not admit, to avoid bypassing rate limiting |
| M6 user-side `ChargeBatch` | does not change the current response, records an error metric | the request has already completed, the response cannot be changed anymore |
| M7 endpoint `ReserveBatch` | the current endpoint is treated as unavailable, try the next endpoint; if all fail then 503 | avoids mistaking Redis jitter for upstream success |
| M7 endpoint `SnapshotBatch` | does not affect scheduling; the endpoint is treated as having no readable quota info | a read-only filter must not become a hard dependency |
| cooldown set/report | does not affect the current response, records an error metric | cooldown is a protection mechanism, not a prerequisite for a successful response |

An optional fail-open mode can only be an explicit configuration; it must emit a warn log and `llm_gateway_ratelimit_fail_open_total`, and is off by default in production.

## 9. Headers

`X-RateLimit-*` headers are not returned.

Reasons:

- The current quota is a stack of multiple buckets across account / API key / model, which is hard to accurately express with one set of headers.
- Endpoint quota is upstream capacity, not a user entitlement, and should not be exposed to clients.
- Under post-deducted TPM, it is not possible to accurately give token remaining before the request starts.

When rejecting a request, only the following are returned:

- HTTP 429
- `Retry-After`
- an error body containing the dimension that was exceeded and the bucket key, for troubleshooting

## 10. Endpoint Rate Limiting

Endpoint quota belongs to M7.

Target flow:

```text
candidates = list + eligibility filter + cooldown filter
optional: SnapshotBatch(endpoint buckets) for read-only filtering
ep = weighted/scored pick

ReserveBatch(endpoint RPM/RPS buckets for selected ep)
if denied:
    Scheduler.Report(ep, capacity)
    exclude ep
    pick next endpoint

call upstream

if usage != nil:
    ChargeBatch(endpoint TPM bucket, cost = usage.Total)
```

Key constraints:

- Do not reserve for all candidate endpoints during the filter stage.
- Only the endpoint that will finally be tried may deduct endpoint RPM/RPS.
- Endpoint TPM is also post-deducted based on real usage.
- Read-only filtering may only use `SnapshotBatch`.
- **Reserve release**: if an attempt fails *before the endpoint is ever contacted*
  (handler-lookup miss / call-construction failure), the dispatcher rolls back
  the reserve via `Store.ReleaseBatch` so a config gap doesn't silently throttle
  a healthy endpoint. A genuine upstream response — including a 429/5xx — keeps
  the reservation: we did send the endpoint a request, and self-throttling on
  its rejection is the intended behavior. `ReleaseBatch` decrements the current
  window (used only on fast-failing paths, so window-boundary drift is
  negligible and always in the caller's favor).

## 11. PolicyCache

`ratelimit.PolicyCache` wraps the `QuotaPolicyReader` defined by middleware:

- Default TTL of 30 seconds.
- Caches the pre-parsed `PolicyRule`.
- If the policy does not exist, returns `nil, nil`, meaning that layer is unlimited.
- After SQL changes a policy, propagation happens via passive TTL: the cached item naturally expires after 30s and is
  reloaded. The data plane does
  not have an active invalidation channel; changes to business tables do not need to take effect within seconds (see
  [06 §8](./06-pluggable-infra.md#8-repo-cache-deployer-sql--gateway-data-propagation) for details).

## 12. Evolution Rules

- When modifying the quota JSON schema, update the domain/ratelimit quota types, repo row/mapper, example configuration, and this document in sync.
- When adding a new rate-limit dimension, the bucket key rule, reserve cost rule, and rejection semantics must be defined.
- Do not introduce an in-memory Store as a production fallback; multi-replica gateways must share Redis counters.
- Endpoint quota must not produce deduction side effects during the candidate filter stage.
- RPM/RPS pre-deduction uses `ReserveBatch`; TPM post-deduction uses `ChargeBatch` — reserve semantics must not be used to swallow real usage.
- TPM does not do pre-deduction; any scheme that reintroduces TPM estimation must explain the risk of wrongly blocking requests and the rollback strategy.
