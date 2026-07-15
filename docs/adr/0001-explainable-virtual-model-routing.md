[English](0001-explainable-virtual-model-routing.md) | [简体中文](0001-explainable-virtual-model-routing.zh-CN.md)

# 0001. Resolve virtual-model policy before dispatch

* **Status**: Accepted
* **Date**: 2026-07-14
* **Author**: Zereker

## Context

The gateway currently resolves the requested concrete model and the optional
`X-Gateway-Fallback-Models` list in `internal/middleware/model_service.go`.
`internal/dispatch` then executes that validated model chain and
`internal/selector` chooses an endpoint for each model. This separation already
keeps catalog authorization out of endpoint scheduling.

Milestone M1 introduces stable virtual model names such as `fast-chat`. A policy
may map one virtual name to several concrete models, but it must not create a
second execution engine or let an untrusted request bypass subscriptions. Every
decision also needs a stable explanation that can be audited without recording
prompt content or producing unbounded metric labels.

The current authenticated identity has an account but no trusted project. The
roadmap reserves project hierarchy for a later identity/RBAC decision, so route
policy must not manufacture project tenancy from request headers.

## Definitions

- **Concrete model**: a name present in the global `model_services` catalog and
  executed by dispatch, for example `gpt-4o`.
- **Virtual model**: a stable caller-facing name owned by routing policy. It is
  never sent to an upstream endpoint.
- **Route policy**: an immutable versioned rule set that maps one virtual model
  to ordered/weighted concrete candidates and deterministic constraints.
- **Candidate model**: a concrete catalog model evaluated for subscription,
  allow/deny rules, region, modality, and configured limits.
- **Constraint**: a deterministic eligibility rule. A constraint removes a
  candidate; it does not choose or invoke an endpoint.
- **Routing decision**: the immutable explanation produced by policy resolution,
  including requested name, policy version, candidate evaluations, ordered
  eligible chain, outcome, and stable reason codes.

The machine-readable contract is `internal/domain/routing_policy.go`.

## Options Considered

### Option A: Put model optimization inside selector

- **How**: extend selector candidates to contain endpoints from several models
  and let its scorer choose both model and endpoint.
- **Pros**: one scoring pass; direct access to endpoint runtime statistics.
- **Cons**: mixes authorization and product policy with endpoint health, makes
  fallback semantics ambiguous, and duplicates catalog/subscription checks in a
  component that currently only selects endpoints.

### Option B: Add a separate routing execution engine

- **How**: virtual-model routing owns its own retry, fallback, and invocation
  loop, delegating only some requests to dispatch.
- **Pros**: maximum implementation freedom.
- **Cons**: creates two retry/streaming implementations, two attempt traces, and
  eventually inconsistent failure behavior.

### Option C: Resolve an explainable model chain before dispatch

- **How**: a policy resolver validates the requested name and produces an
  ordered concrete model chain plus a decision record. Existing dispatch and
  selector execute that chain unchanged.
- **Pros**: preserves current boundaries, keeps decisions reconstructable, and
  allows policy evolution without touching streaming execution.
- **Cons**: resolution and endpoint selection are two stages; latency/health
  optimization later needs a small read-only statistics projection.

## Decision

We choose **Option C**.

### Resolution and execution boundary

Model-chain resolution happens before `dispatch.Input` is built. It may read the
catalog, subscriptions, the effective route policy, and a compact routing
telemetry projection. It returns:

1. a `domain.ModelRoutingDecision` containing all accepted and rejected
   candidates with stable reason codes; and
2. the validated `[]*domain.ModelService` chain represented by the decision's
   eligible candidates.

`internal/dispatch` remains the only owner of endpoint loading, endpoint
selection, retry, cross-model fallback execution, streaming, and attempt
accounting. `internal/selector` remains an endpoint selector. Neither component
queries route-policy storage.

### Policy scope and precedence

Policy scopes are `global`, `account`, and reserved `project`. Effective-policy
selection uses the most specific complete policy: project, then account, then
global. Policies are not merged across scopes, because partial merging makes a
decision difficult to reconstruct and rollback.

Project policy is inactive until authentication produces a trusted Project ID
and its ownership/RBAC model is accepted. A header or request-body field must
not establish project scope.

Concrete-model requests are default-allow subject to the existing catalog and
subscription checks. Virtual-model requests are default-deny: without an active
effective policy they fail with `virtual_model_policy_not_found`. Account
subscription is always a hard constraint and cannot be widened by any policy.

### Legacy fallback header

For a concrete-model request, `X-Gateway-Fallback-Models` retains its current
ordered, de-duplicated, maximum-three behavior. Each accepted fallback is
recorded with `legacy_fallback_accepted`; missing or unauthorized candidates may
be recorded as rejected evaluations but never enter dispatch.

For a virtual-model request, the policy is the complete authority. The legacy
header is ignored and recorded as `legacy_fallback_ignored`; it cannot append a
model or widen policy. This prevents a caller-controlled header from bypassing
allow/deny constraints.

### Deterministic constraints and attempts

M1.2 initially supports region, modality/capability, allow/deny model lists, and
maximum attempts. Deny wins over allow. An explicit allow list excludes models
not present in it. Catalog existence and account subscription are evaluated
before a candidate enters the model chain.

Policy maximum attempts is an upper bound, not a replacement for the gateway's
operational attempt cap. The effective cap is the minimum positive value from
both sources. Candidate weight only influences ordering/selection among already
eligible candidates; it never overrides a constraint.

### Failure behavior

- concrete catalog miss or subscription denial keeps existing HTTP behavior;
- virtual policy not found/disabled is a client-visible rejection;
- all candidates ineligible is a rejection with `no_eligible_candidate`;
- policy storage unavailable is fail-closed with
  `routing_policy_unavailable` and HTTP 503;
- malformed active policy is fail-closed with `routing_policy_invalid`, is
  surfaced in health/operations, and never falls through to a less-specific
  policy;
- concrete requests do not depend on route-policy storage and continue during a
  policy-store outage.

No rejected candidate, free-form policy message, prompt, credential, or raw
request content is included in a metric label.

### Audit, trace, usage, and metrics

The full decision record is safe structured metadata: requested model, virtual
flag, outcome, reason, policy ID/version/scope, candidate model IDs/names,
eligibility, source, order, and bounded reason codes. It contains no prompts or
secrets.

Traces and audit may attach the full structured decision. Usage events record
requested model, actually routed model, policy ID/version, and terminal reason.
Metrics use only bounded labels such as `outcome`, `reason`, and `scope_kind`;
policy IDs, account IDs, project IDs, and model names are not metric labels.

### Hot-path query and cache shape

The persistence query returns one immutable effective-policy snapshot with all
candidates and constraints; candidate rows are not loaded one by one. The
resolver cache key is `(account_id, trusted_project_id, requested_model)` and
the cached value contains:

- positive or negative resolution;
- immutable policy ID/version/scope;
- compiled deterministic constraints and ordered candidates;
- load/expiry timestamps.

The first miss performs one effective-policy query. Catalog and subscription
lookups use their existing bounded caches; M1.2 may batch them but must not issue
per-endpoint queries. Positive and negative entries have bounded TTLs. A policy
write publishes an invalidation carrying policy identity and version; consumers
must not replace a newer cached version with an older event. TTL remains the
recovery path when pub/sub is unavailable.

### Consistency, rollback, and migration

Policy versions are immutable. Activating an update atomically changes the
effective version. In-flight requests keep their resolved snapshot; new
requests observe the new version after invalidation or TTL expiry. A rollback
re-publishes prior content as a new monotonically increasing version, preserving
an unambiguous audit timeline.

Migration is additive:

1. add policy storage/API/cache while concrete resolution remains the default;
2. enable virtual names only when an active policy exists;
3. add decision metadata to traces, usage, and Console dry-run output;
4. later add project scope only with trusted identity and RBAC.

Rollback disables virtual-policy resolution. Concrete requests and the legacy
fallback header continue on their existing path; policy tables may remain
unused without affecting dispatch.

## Consequences

### Positive

- callers can use stable virtual names without changing dispatch or streaming;
- every model-chain decision is reconstructable from policy version and inputs;
- authorization remains fail-closed and cannot be widened by request headers;
- concrete-model compatibility and policy-store fault isolation are explicit;
- the cache contract is defined before persistence implementation begins.

### Negative / Trade-offs

- policy resolution adds a bounded cache/storage stage before dispatch;
- project policies cannot ship until identity and RBAC provide trusted scope;
- non-merged scope policies repeat configuration across levels;
- latency/cost objectives need an explicit compact telemetry projection in
  M1.3 rather than directly coupling to selector internals.
