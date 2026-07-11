# 05 — Recording, Metering & Billing

This document records the data the gateway needs to persist and the channels for it. The gateway needs to record three kinds of things:

1. **Content recording**: request / response content, for auditing, troubleshooting, replay, compliance.
2. **Usage metering**: token / audio / image and other resource consumption, sent to the billing platform.
3. **Request/response metrics**: latency, status, endpoint, retry chain, etc., for monitoring, scheduling, and troubleshooting.

These three kinds of data must not be merged into a single event. They differ in volume, sensitivity, reliability requirements, and consumers.

## 1. Three recording channels

| Channel | Content | Primary consumer | Reliability requirement |
|------|------|------------|------------|
| Content Log | Request body, response body, upstream request/response, optionally redacted/sampled | Auditing, troubleshooting, compliance, replay | Configurable; usually async, sampling allowed |
| Usage Event | `domain.Usage` + identity/model/endpoint/time meta | Billing platform, usage reports, TPM post-charge | High; failure must not block the response, but must have compensation/retry |
| Metrics / Trace | duration, status, error class, attempts, decision | Prometheus, logs, scheduling scores | High throughput, low cost; no large bodies recorded |

Design principles:

- Request / response content does not enter usage events.
- Usage events do not carry the full prompt / completion.
- Metrics do not carry large payloads, only labels and numeric values.
- All three channels are correlated via `request_id` / `trace_id`.

## 2. Content Log

Content recording is an optional capability; it should not be assumed by default that all deployments enable full body recording. Reasons:

- Request / response may contain sensitive data.
- The response stream may be very large.
- Streaming scenarios need to read and write concurrently, and must not block the main chain for the sake of recording.

Content recording should be based on `internal/invoker` hooks:

| Hook | Content recorded | Purpose |
|------|----------|------|
| `ClientRequestObserver` | Client's original request body | User-perspective auditing |
| `UpstreamRequestObserver` | Translated body sent to the upstream | Upstream protocol troubleshooting |
| `UpstreamChunkObserver` | Original upstream response chunk | Upstream replay / fixture |
| `ClientChunkObserver` | Response chunk actually received by the client | User-perspective auditing / reconciliation |
| `AttemptCompleteObserver` | Result of a single upstream attempt | Attempt-level troubleshooting |

Recommended recording shape:

```go
type ContentRecord struct {
    RequestID   string
    TraceID     string
    AccountID   string
    APIKeyID    string
    SubAccountID string
    Model       string
    Vendor      string
    EndpointID  string

    Direction   string // client_request | upstream_request | upstream_chunk | client_chunk
    Protocol    string
    Modality    string
    ContentType string

    Body        []byte // or object storage pointer
    BodySHA256  string
    Truncated   bool
    Redacted    bool
    CreatedAt   time.Time
}
```

Implementation constraints:

- If a hook needs to persist a chunk, it must copy the bytes; the slice received by the hook callback may be reused.
- The recorder must support an async buffer / backpressure strategy, and must not block the response stream indefinitely.
- The default backpressure strategy is drop-oldest, with a dropped counter recorded; when strong auditing is needed it can be configured to block or write an object storage pointer, but that must not be the default main-chain behavior.
- Must support toggles by account, model, endpoint, status code, and sampling rate.
- Must support max body size; truncate or write an object storage pointer beyond that.
- When compliance is required, redact before persisting; on redaction failure, choose drop or write-summary per configuration.

Output backend (driver) only supports `none` / `file`:

- `none`: fully disabled, zero overhead.
- `file`: JSONL append to a local file.

Deliberately **not** embedding Kafka / S3 / Loki / ES or other producers inside the gateway process. Content Log is by nature a logging/auditing channel, not a business event (as distinct from the Usage Event in §3); in a typical deployment it has multiple downstream consumers — archiving (S3 / OSS), search (Loki / ES), content-safety post-review (Kafka), training data feedback. Having the gateway handle multi-sink delivery would couple the availability of all those downstreams into the main chain.

The correct shape is to make `file` the sole exit, with a mature log collector such as fluent-bit / vector responsible for fan-out + retry + sink adaptation:

```text
gateway ──→ content.jsonl ──→ fluent-bit / vector ──┬─→ S3 / OSS         (archiving + training data)
                                                    ├─→ Loki / ES        (troubleshooting search / compliance)
                                                    ├─→ Kafka topic      (content-safety post-review pipeline)
                                                    └─→ ...
```

Rotation / compression / cleanup of the file itself is handled by an external logrotate or log collector (fluent-bit's tail input supports inode following), not inside the gateway process. Adding/adjusting sinks only requires changing the fluent-bit config; the gateway does not need a release.

Comparison with Usage Event:

| Dimension | Usage Event (§3-5) | Content Log (this section) |
|------|---------------------|---------------------|
| Nature | Business event, relied on for financial reconciliation | Log/audit |
| Backend | `file` / `kafka` / `async_kafka` | `file` (gateway's sole exit) |
| Downstream | Billing platform (single consumer) | Multiple sinks, fanned out by fluent-bit |
| Cost of loss | Severe (missed billing) | Sampling / dropping the oldest is tolerable |
| Schema evolution | `schema_version` + dual-write to switch topics | Evolves by JSONL field; consumers tolerate unknown fields |

## 3. Usage Event

Usage is the resource-consumption event consumed by the billing platform. Its source is the translator / usage extractor:

```go
type Usage struct {
    Input  int64
    Output int64
    Total  int64
    Truncated bool

    Raw        json.RawMessage
    Source     string // upstream | extracted | estimated
    Estimator  string // tiktoken | naive_chars | vendor_default
    Confidence string // exact | derived | approximate

    Meta UsageMeta
}
```

Field semantics:

- `Input` / `Output` / `Total` are the generic token fields the gateway extracts as best it can, used for basic statistics and TPM post-charge.
- `Truncated` indicates the response did not complete fully, e.g. the client disconnected or the streaming response stopped midway.
- `Raw` is the upstream's original usage object, forwarded as-is to the billing platform.
- `Source` / `Estimator` / `Confidence` identify the origin and trustworthiness of the usage, to avoid disguising an estimated value as genuine upstream data.
- Complex billing dimensions are parsed from `Raw` by the downstream billing platform via rules; the gateway does not maintain an enum of vendor-specific fields.

An extension key like `Details map[MetricKey]int64` is no longer defined in the gateway. The reason is that usage dimensions keep evolving per vendor; enumerating them in the gateway would make billing-rule changes depend on gateway releases. Downstreams can choose parsing rules based on `vendor` / `model` / `protocol` / the time the request occurred.

Usage source priority:

1. Upstream returns raw usage: fill `Raw`; generic fields can be extracted from Raw, `Source=upstream`, `Confidence=exact`.
2. Upstream has no standard usage, but the translator can reliably parse equivalent fields from the response: fill the generic fields, `Source=extracted`, `Confidence=derived`.
3. Upstream has no usage: fall back to a tokenizer or character-count estimate, `Source=estimated`, `Estimator=tiktoken` or `naive_chars`, `Confidence=approximate`.

tiktoken is only a fallback estimate:

- It cannot override genuine upstream usage.
- It cannot guarantee coverage of every vendor's tokenizer.
- Multimodal, tool calls, and reasoning tokens may be inaccurate.
- The estimate can be used for TPM post-charge and floor usage; the billing platform may decide by rule whether to trust it.

`naive_chars` means a rough estimate based on character count, with the specific divisor determined by configuration; do not hardcode an English-specific heuristic like `chars/4` into enum semantics.

M7 writes after the response forward completes:

```go
rc.Usage = fwd.Usage
```

M10 fills in the remaining meta after `c.Next()` and publishes the usage outbox. If a cross-model fallback occurred, the `Model` in the usage meta must be the model of the endpoint that actually succeeded, not the original request model.

Usage on exception paths:

- If the client disconnects mid-stream and partial token counts are already available, publish the accumulated usage before the cutoff, with `Truncated=true`, `Confidence=approximate`; if no counting is possible at all, do not construct the generic token fields, but a meta event with an error status may still be published.
- A failed attempt that switches to the next endpoint after an upstream 5xx / network error does not produce Usage; the eventually successful attempt produces Usage.
- If an error occurs after the response has already started, M10 still publishes the known Usage using `context.Background()` with a timeout, to avoid losing usage due to client disconnect.

## 4. Usage Meta

`UsageMeta` is used by the billing platform to correlate identity, model, routing, and the time the request occurred:

```go
type UsageMeta struct {
    AccountID         string
    Model             string
    Vendor            string
    EndpointID        string
    SubAccountID      string
    APIKeyID          string
    ServiceID         string
    ModelServiceID    int64       // pricing lookup fingerprint; same source as ServiceID's RoutedModelService
    ServiceUpdateTime time.Time   // snapshot of model_services.updated_at
    RequestID         string
    TraceID           string
    StartTime         time.Time
    EndTime           time.Time
    TTFTMs            int64
    TotalLatency      int64
}
```

Field origins:

| Meta field | Origin |
|-----------|------|
| `RequestID` | M1 `rc.RequestID` |
| `TraceID` | `TraceIDFromCtx(c.Request.Context())` |
| `AccountID` / `SubAccountID` / `APIKeyID` | M2 `rc.Identity` |
| `Model` / `ServiceID` / `ModelServiceID` / `ServiceUpdateTime` | M7 `rc.RoutedModelService`, equal to M5 `rc.ModelService` when there was no fallback |
| `Vendor` / `EndpointID` | M7 `rc.Endpoint` |
| `StartTime` | M1 `rc.StartTime` |
| `EndTime` / `TotalLatency` | M10 current time |

`TTFTMs` is not yet captured.

**On `ModelServiceID` / `ServiceUpdateTime`**: this is a pricing lookup fingerprint for the downstream billing aggregator. Once M5 obtains the ModelService, it already holds the ID + UpdatedAt on `rc.ModelService`; M10's `fillUsageMeta`, together with `Model` / `ServiceID`, copies them into Meta on a "routed takes priority" basis, ensuring that after a fallback all 4 fields describe the same model being billed. **The gateway side still does not query active pricing** (the principle in §6 is unchanged); it only passes through the two model_service fields as a stable pointer for downstream price lookups.

## 5. Usage Outbox

Current interface:

```go
type OutboxPublisher interface {
    Publish(c context.Context, evt *OutboxEvent) error
}

type OutboxEvent struct {
    Payload []byte
    Key     string
}
```

The Usage Event payload uses JSON, in the following recommended envelope shape:

```go
type UsageEvent struct {
    SchemaVersion string    `json:"schema_version"` // "usage.v1"
    EventID       string    `json:"event_id"`
    Usage         Usage     `json:"usage"`          // includes Meta; request_id / trace_id are inside Meta
    CreatedAt     time.Time `json:"created_at"`
}
```

The recommended default Kafka topic is `billing.usage.recorded.v1`. Topic naming follows the **domain.entity.event.version** convention, decoupled from the producing service's name — the topic describes "what business event this is" (billing usage has been recorded), not "who sent it". This lets downstream billing/reconciliation/quota services subscribe by business domain; if multiple services later produce usage events, they still use the same topic, avoiding fragmentation like `llm-gateway.usage` / `embedding-gateway.usage` / `image-gateway.usage`.

The partition key uses `AccountID`, so events for the same billing subject stay ordered as much as possible; when there is no AccountID it falls back to `Usage.Meta.RequestID`. `request_id` / `trace_id` are placed only inside `Usage.Meta` — not duplicated at the envelope top level, to eliminate the potential bug of dual writes going out of sync. `CreatedAt` is the time the event was enqueued into the outbox, not equivalent to when the request completed; timing analysis of the request should use `Usage.Meta.StartTime` / `Usage.Meta.EndTime`.

Schema evolution proceeds via backward-compatible branching on `schema_version`; fields are not removed within the same version. Breaking changes must be migrated explicitly: prefer switching to a new topic (`billing.usage.recorded.v2`) with a dual-write period and consumer cutover; if the same topic continues to be used, multiple schemas must be allowed to coexist, with consumers routing/parsing by `schema_version`. The `.v1` in the topic name is topic-level physical isolation, a separate mechanism from the envelope `schema_version`: a topic upgrade changes broker routing, a schema upgrade changes field parsing.

M10 publishes using `context.Background()` with a timeout, to avoid client disconnect preventing usage from being written out. Publish failure does not affect the business response.

Implementations:

- `file`: JSONL append, suitable only for local development / single-machine deployment.
- `kafka`: synchronous producer, no local copy (if the broker goes down it's simply lost; not recommended).
- `async_kafka`: buffer, retry, backoff, DLQ topic; short broker blips can be rescued, but it's still lost during a long outage.
- `file_and_kafka`: **recommended for production** — Transactional Outbox Pattern. file is the source of truth
  (sync commit), Kafka is an asynchronous broadcast (best-effort). The broker failure domain is orthogonal to the
  disk failure domain, so they won't fail simultaneously; when the broker is down, the file has already been
  committed, and an external replay tool reads the file to resend the missing events to
  Kafka (the consumer side deduplicates idempotently by `event_id`).

Failure semantics:

| Driver | Failure mode | Default behavior | Observability |
|--------|----------|----------|--------|
| `file` | Disk full / IO error | drop event + error log | `llm_gateway_usage_publish_total{backend="file",result="error"}` |
| `kafka` | broker / leader / network unavailable | retry until publish timeout, then drop event + error log on failure | `llm_gateway_usage_publish_total{backend="kafka",result="error"}` |
| `async_kafka` | buffer full | drop-oldest by default; can be configured to block, but must have a timeout | `llm_gateway_outbox_dropped_total{driver="async_kafka"}` / buffer depth |
| `async_kafka` | retries exhausted | write to DLQ topic; if DLQ fails, error log + metric | `llm_gateway_outbox_dlq_total` |
| `file_and_kafka` | broker / network unavailable | file already committed; Kafka retries asynchronously, then writes to DLQ (if configured) or just a metric after retries are exhausted; **no data loss** | `llm_gateway_outbox_kafka_publish_error_total` |
| `file_and_kafka` | disk full / IO error | **severe** — file commit fails; still attempts Kafka but returns an error; M10 counts `usage.publish.error` | `llm_gateway_outbox_file_error_total` |
| `file_and_kafka` | double failure | file error returned to M10; Kafka error swallowed into a metric | both metrics above increment simultaneously |

Why not reuse `async_kafka + DLQ` instead of `file_and_kafka`: the DLQ sits on the **same broker cluster** as the main topic, so if the broker is entirely down the DLQ can't be written either. `file_and_kafka` puts file and broker in different failure domains, so a broker failure doesn't lose data. Under dual-write, the DLQ degrades to a "single-message-level error fallback" (broker online, but the message itself has a problem — too large, invalid schema, ACL rejection, etc.), which is optional, not required.

Reliability requirements:

- Usage events are billing input, and must prioritize being compensable.
- The gateway must not block the user response because of a brief outbox failure.
- The `file` driver is only suitable for local development or temporary troubleshooting.
- Production must use `file_and_kafka`: file provides a durability fallback, Kafka provides low-latency broadcast; and monitor
  `outbox_file_error_total` (severe, disk issue), `outbox_kafka_publish_error_total` (data
  safe but needs replay), and Kafka consumer lag.

## 6. Pricing

In the target design, the gateway does not do pricing, nor does it need to query active price on the request path.

The gateway only produces the factual data needed for billing:

- account / API key / sub account.
- model / service id.
- vendor / endpoint.
- request_id / trace_id.
- request start time / end time.
- usage values.

Concrete price matching and amount calculation are done by the downstream billing platform. The billing platform matches the price version in effect at the time based on the request-occurrence time in the usage event (usually `StartTime`, combined with `EndTime` if needed).

Benefits of this approach:

- The gateway is unaware of complex pricing rules, avoiding a pricing-query dependency on the request path.
- Price changes, corrections, and recalculations all happen on the billing platform.
- The usage event is a factual record, not a settlement result in currency.

The gateway does not do:

- Hourly/daily bill aggregation.
- Account balance deduction.
- Online pricing-rule calculation.
- Active price lookup.
- Amount generation.

## 7. Metrics / Trace

Metrics record the runtime status of requests and responses, without recording large payloads.

M10 currently records:

- `llm_gateway_http_request_duration_seconds`
- scheduling decision trace

Recommended metric dimensions:

| Metric | Dimensions | Purpose |
|------|------|------|
| request duration | method, path, status, model, vendor, endpoint_id | SLA / latency |
| upstream duration | vendor, endpoint_id, model, result, error_class | scheduling score / troubleshooting |
| usage publish | result, backend | billing chain health |
| content log publish | result, backend, sampled | content-recording chain health |
| scheduler attempt | model, routed_model, vendor, endpoint_id, class, attempt_role | fallback / cooldown analysis |
| scheduling duration | model, attempts | total time for scheduling filter / pick / report |
| eligibility filter duration | model | eligibility filtering performance |
| policy cache hit | layer, result | quota policy cache hit rate |
| outbox publish latency | driver, result | outbox write latency |
| outbox buffer depth | driver | `async_kafka` buffer occupancy |
| ratelimit charge result | dimension, result | visibility into TPM post-charge failures |
| tpm overflow | layer, dimension | number of times TPM post-charge exceeds the configured cap |

When metrics are used for runtime scoring, only lightweight aggregated statistics are read, never the Content Log.

## 8. Relationship with rate limiting

Rate limiting does not depend on the Content Log.

RPM / RPS is reserved before the request; TPM is charged based on `Usage.Total` after usage is produced. If `Usage.Total` comes from a tiktoken estimate, the post-charge must still retain the `Source=estimated` / `Confidence=approximate` markers, so downstreams can identify it.

If the translator / extractor only obtains the upstream's raw usage but cannot reliably extract `Total`, the usage event is still published to the downstream billing platform, but this request does not undergo gateway-side TPM post-charge.

Therefore, usage capture and generic-field extraction coverage directly affect:

- Billing completeness.
- TPM post-charge accuracy.
- Usage report accuracy.

When adding a new protocol or translator, the original upstream usage must be preserved into `Raw` as much as possible. If the generic `Total` can be reliably extracted, fill `Input` / `Output` / `Total`; if it cannot, `Raw` should still be handed to downstream billing rules.

## 9. Recording policy

Different channels should have different defaults:

| Data | Default policy |
|------|----------|
| Usage Event | On by default |
| Metrics / Trace | On by default |
| Client request body | Off or sampled by default |
| Client response body | Off or sampled by default |
| Upstream request / response | Off by default, enabled only for troubleshooting |

Content-recording toggles should support:

- By account / API key.
- By model / endpoint / vendor.
- By error status.
- By sampling rate.
- By max body size.
- By field redaction rule.

## 10. Relationship with the repo cache

The Usage Event channel (gateway → downstream billing) and the SQL → gateway config propagation
(repo's in-process TTL LRU cache, [06 §8](./06-pluggable-infra.md#8-repo-cache-deployer-sql--gateway-data-propagation))
are **two independent channels** and must not be reused for each other:

| Dimension | Usage Event Outbox | Repo TTL cache |
|------|--------------------|---------------|
| Data direction | gateway → downstream billing | SQL → gateway |
| Trigger | M10 actively publishes after request completion | SQL direct lookup on cache miss |
| Transport | Kafka topic `billing.usage.recorded.v1` | in-process LRU; no cross-process channel |
| Reliability | DLQ + retry, failure does not block the response | TTL expiry falls back to source; MySQL failure returns 503 directly |
| Schema evolution | `schema_version` + switching to a new topic | repo struct evolution |

The repo cache is only a read-path optimization for **configuration data**; it does not carry usage / content. Writing usage to
the repo cache is a mismatch (usage is an event stream, not lookup data); conversely, routing SQL config changes through the usage
outbox is also wrong (the billing consumer should not see schema-type events).

## 11. Evolution rules

- When changing the strategy for passing through raw usage fields, update this document, the usage extractor / translator, and the downstream billing platform schema together.
- When changing usage meta fields, update the downstream billing pipeline schema together.
- Usage outbox publishing must preserve the semantics of "failure does not affect the business response"; if strongly consistent billing is required, compensate downstream rather than blocking M10.
- Content Log must not reuse the Usage Event schema; the two must evolve independently.
- Metric labels must not contain request / response body or high-cardinality fields.
- The gateway must not compute amounts on the request path; price matching is done downstream based on the time the request occurred.
- Do not reuse the Usage Event and repo cache channels for each other (§10).

## 12. Downstream consumers

§6 already states that the gateway does not do "bill aggregation / balance deduction / online price lookup / amount generation". These are the responsibility of the downstream
billing platform, and **out of scope for this repo** — this repo only produces Kafka `billing.usage.recorded.v1`
events; downstream consumers implement aggregation + price lookup + billing themselves.

This document defines what a usage event is, how the gateway produces it, the Outbox driver and reliability semantics, and the gateway-side
Pricing boundary (the "what the gateway does not do" list). Any change to the usage event schema must trigger a synchronized
evaluation by the downstream consumers.
