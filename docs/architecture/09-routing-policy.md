# Explainable virtual-model routing

Virtual-model routing is a model-chain resolution stage before dispatch. A
caller requests a stable name such as `fast-chat`; the gateway resolves one
immutable policy snapshot, evaluates deterministic constraints, and supplies
an ordered concrete `ModelService` chain to the existing dispatcher.

## Runtime boundary

```text
request model
     |
     +-- concrete catalog hit --> subscription --> existing fallback header
     |
     `-- catalog miss --> effective routing policy --> constraints/subscriptions
                                                  |
                                                  `--> dispatch model chain
```

`internal/routingpolicy` never selects endpoints or invokes upstreams.
`internal/dispatch` remains the only retry/fallback execution loop, and
`internal/selector` still chooses endpoints for one concrete model at a time.
Before each attempt, dispatch rewrites the envelope's top-level `model` to the
current concrete candidate; the caller-facing virtual name is retained in the
model-routing decision and usage metadata.

## Policy shape

Policies are immutable versions stored in `routing_policies.rule_json`:

```json
{
  "max_attempts": 2,
  "constraints": {
    "regions": ["cn-north"],
    "modalities": ["chat"],
    "allow_models": ["gpt-4o-mini", "local-llama"],
    "deny_models": []
  },
  "objectives": {
    "latency_weight": 2,
    "cost_weight": 1,
    "target_latency_ms": 500,
    "target_cost_microusd": 500,
    "estimated_input_tokens": 1000,
    "estimated_output_tokens": 500,
    "min_telemetry_samples": 5,
    "telemetry_max_age_seconds": 300,
    "exploration_permille": 50
  },
  "candidates": [
    {"model": "gpt-4o-mini", "weight": 100},
    {"model": "local-llama", "weight": 50, "regions": ["cn-north"]}
  ]
}
```

The resolver applies project, account, then global precedence in one SQL
query. Project policies are rejected by the Console until authentication and
RBAC provide a trusted project identity. Scope policies are complete snapshots
and are not merged.

Deny wins over allow. Policy-wide and candidate-specific region/modality rules
are intersected. Catalog existence and account subscription are mandatory.
Without objectives, eligible candidates are ordered by descending weight with
configuration order as a deterministic tie-breaker. `max_attempts` can only
tighten the gateway's global attempt cap.

## Latency and cost objectives

Hard constraints, catalog existence, and account subscription always run
before optimization. Objective scoring cannot make an ineligible candidate
eligible. For each eligible candidate the resolver computes bounded scores:

```text
latency_score = clamp(target_latency / observed_latency, 0, 1) * success_rate
cost_score    = clamp(target_cost / estimated_cost, 0, 1)
total_score   = weighted_mean(latency_score, cost_score)
```

The estimated cost uses the policy's fixed input/output token assumptions and
the active `routing_cost_profiles` snapshot. These immutable operator-cost
profiles are intentionally separate from `pricing_versions`: routing never
calls a billing service or evaluates customer pricing, discounts, or invoices.
The data plane reads profiles through the same bounded TTL cache pattern as
policies.

Latency and success signals reuse `selector.EndpointStatsStore`; the routing
layer projects the existing per-endpoint EMA snapshots for the candidate model
and request group. It does not create a parallel metrics pipeline. Fewer than
`min_telemetry_samples` is `missing_neutral`; telemetry older than
`telemetry_max_age_seconds` is `stale_neutral`. Missing cost profiles are also
`missing_neutral`. Every neutral signal scores `0.5` and is explicit in the
decision rather than silently treated as zero or best-in-class.
Collecting those EMA snapshots is controlled by `scoring.enabled`; when it is
off, latency objectives correctly report `missing_neutral` while cost scoring
continues to work. Multi-replica deployments should use `scoring.driver: redis`
so both endpoint selection and model routing see the same snapshots.

Exploration is deterministic. When the request ID hashes into
`exploration_permille`, one non-leading eligible candidate is promoted. The
hash includes policy ID/version, account, and request ID, so a decision is
reproducible from recorded inputs. Exploration never includes rejected
candidates. Candidate weight and configured order remain deterministic
tie-breakers after objective score.

## Compatibility and failure

- Concrete models and model aliases keep their existing behavior and do not
  depend on routing-policy storage.
- `X-Gateway-Fallback-Models` remains active for concrete requests.
- The header is ignored for virtual requests and cannot widen policy.
- Missing virtual policy returns `virtual_model_policy_not_found`.
- An empty eligible set returns `no_eligible_candidate`.
- Storage or malformed-policy failures are fail-closed dependency failures.

## Consistency and observability

The data plane caches complete effective snapshots for 30 seconds and negative
lookups for 5 seconds. Console writes publish a routing-policy invalidation;
global policy changes intentionally purge the small compiled cache because
they may affect every account key. TTL is the fallback when Redis pub/sub is
unavailable.

Every request records a bounded `ModelRoutingDecision`: requested model,
outcome/reason, policy ID/version/scope, and accepted/rejected candidates.
Objective decisions additionally record signal source, observed latency and
success, sample timestamp/count, cost-profile ID/version, estimated cost,
component scores, total score, and whether exploration selected the model.
The full decision is trace/audit metadata. The metric
`llm_gateway_routing_decisions_total` uses only `outcome`, `reason`, and
`scope_kind`; policy/account/model identifiers are not metric labels. Usage
metadata records requested and routed models plus policy ID/version/reason.

## Console API

- `GET /admin/routing-policies` lists all versions.
- `POST /admin/routing-policies` validates and publishes a new active version.
- `DELETE /admin/routing-policies/:policyID` disables the active version.
- `POST /admin/routing-policies/dry-run` evaluates synthetic account, region,
  modality, requested-model, decision key, and optional telemetry snapshots
  without dispatching upstream traffic. The response compares every candidate
  with its complete score explanation.
- `GET /admin/routing-costs` lists routing-only cost-profile versions.
- `POST /admin/routing-costs` publishes a new immutable active cost version.

Writes are covered by the Console's existing admin role and write audit.
