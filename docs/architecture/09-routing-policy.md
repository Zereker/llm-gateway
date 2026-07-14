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
Eligible candidates are ordered by descending weight with configuration order
as a deterministic tie-breaker. `max_attempts` can only tighten the gateway's
global attempt cap.

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
The full decision is trace/audit metadata. The metric
`llm_gateway_routing_decisions_total` uses only `outcome`, `reason`, and
`scope_kind`; policy/account/model identifiers are not metric labels. Usage
metadata records requested and routed models plus policy ID/version/reason.

## Console API

- `GET /admin/routing-policies` lists all versions.
- `POST /admin/routing-policies` validates and publishes a new active version.
- `DELETE /admin/routing-policies/:policyID` disables the active version.
- `POST /admin/routing-policies/dry-run` evaluates synthetic account, region,
  modality, and requested-model input without dispatching upstream traffic.

Writes are covered by the Console's existing admin role and write audit.
