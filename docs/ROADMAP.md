# Product Evolution Roadmap

This roadmap evolves `llm-gateway` from a capable infrastructure component into
an adoptable, policy-aware LLM runtime gateway. It is intentionally organized by
user-visible outcomes rather than by package or provider count.

The roadmap is directional, not a compatibility promise. A milestone is complete
only when its acceptance gates pass; landing isolated backend types or APIs does
not count as delivery.

## Progress

| Work package | Status |
|---|---|
| M0.1 Product narrative | Complete |
| M0.2 One-command local demo | Complete |
| M0.3 Operational dashboard | Complete |
| M0.4 Reproducible performance proof | Complete |
| M1.1 Routing policy ADR and domain contract | Complete |
| M1.2 Rule-based virtual models | Complete |
| M1.3 Latency and cost objectives | Planned |
| M2 Governed prompt and response policy | Planned |
| M3 Enterprise identity and hierarchy | Deferred pending product need |

## Product position

`llm-gateway` is a policy-aware, OpenAI-compatible runtime gateway for enterprise
LLM traffic. It provides one runtime enforcement point for authentication,
routing, quota, governance, metering, audit, and observability across hosted and
self-hosted models.

It is not a prompt orchestration framework, an inference server, or an in-process
billing engine.

## Existing baseline

The following capabilities already exist and must be evolved instead of rebuilt:

- `internal/dispatch` owns routing execution, retry, endpoint fallback, and
  explicit cross-model fallback.
- `internal/selector` provides weighted/P2C selection, cooldown, inflight state,
  and success/latency-aware runtime scoring.
- `internal/moderation` provides input/output guard composition and response
  stream decoration.
- `cmd/console` is a separate control-plane API with a Web UI, admin/viewer roles,
  and write audit.
- the data plane emits usage facts and metrics; downstream systems retain
  ownership of aggregation and billing.

These boundaries remain architectural constraints throughout the roadmap.

## Evolution principles

1. **Adoption before breadth.** Do not add another provider unless it closes a
   demonstrated compatibility gap. Improve setup, proof, and operability first.
2. **One routing engine.** Extend policy resolution around `internal/dispatch`;
   do not create a parallel routing execution package.
3. **Explain every policy decision.** Model changes, fallback, blocking, and
   redaction must produce a stable reason code suitable for audit and metrics.
4. **Keep hot-path data small.** Billing aggregation and analytics remain outside
   the gateway. Routing may consume a dedicated compact cost profile, not the
   downstream invoice model.
5. **Streaming semantics are explicit.** Every output policy declares whether it
   is strict, buffered, or best-effort for streaming responses.
6. **Add tenancy only with an authorization model.** A new hierarchy must define
   ownership, isolation, and migration from `account`; it must not introduce a
   second name for the same entity.

## Milestone 0 — Adoptable product surface

Goal: a new evaluator can understand, run, observe, and benchmark the project
without learning its internal package graph first.

### M0.1 — Product narrative

- Rewrite the first screen of both READMEs around the product outcome.
- Add a compact architecture diagram showing application, gateway policy stages,
  and upstream providers.
- Move the package-by-package data-plane description behind the quick start or
  into architecture documentation.
- Show the Console and an example routing decision.
- Clearly label current capabilities versus roadmap capabilities.

Acceptance gates:

- a reader can identify the target user, problem, and differentiator before the
  first installation command;
- no README claim depends on an unimplemented roadmap feature;
- English and Chinese first-screen content remain structurally aligned.

### M0.2 — One-command local demo

- Add a Quickstart Compose profile containing MySQL, Redis, gateway, console,
  mock upstream, gateway-owned schema migration, and idempotent seed.
- Give the demo stable, non-secret development credentials.
- Provide one command that reaches a ready state without manual SQL.
- Print the Console URL, API key, and a copyable request after startup.
- Keep the current infra-only Compose workflow for contributors.

Acceptance gates:

- a clean checkout reaches a successful chat request and a usable Console with
  one documented command;
- a second run is idempotent;
- startup readiness is healthcheck-driven rather than sleep-driven;
- CI executes the same demo smoke path.

### M0.3 — Operational dashboard

- Ship an importable Grafana dashboard instead of a panel wish list.
- Cover request rate, success/error class, p50/p95/p99 latency, TTFT, endpoint
  health/cooldown, rate-limit rejection, and usage outbox failures.
- Add Prometheus and Grafana to an optional observability demo profile.
- Document label cardinality and the absence of in-gateway cost aggregation.

Acceptance gates:

- the checked-in dashboard imports without manual edits into the documented
  Grafana version;
- panels populate from demo traffic;
- PromQL expressions are checked against metric names in CI or tests.

### M0.4 — Reproducible performance proof

- Add non-streaming and streaming load scenarios using one pinned load tool.
- Measure direct mock-upstream versus gateway to isolate gateway overhead.
- Report throughput, first-token overhead, end-to-end latency, allocations or
  memory, CPU, error rate, and active streams.
- Add slow-client, client-disconnect, and upstream-mid-stream-failure scenarios.
- Check in the harness and environment metadata with every published result.

Acceptance gates:

- another contributor can reproduce the report from documented commands;
- results distinguish gateway overhead from configured upstream delay;
- performance work has a baseline and a regression threshold.

## Milestone 1 — Explainable policy routing

Goal: callers request a stable virtual model while the platform selects an
allowed concrete model and endpoint according to explicit policy.

### M1.1 — Routing policy ADR and domain contract

- Define `virtual model`, `route policy`, `candidate model`, `constraint`, and
  `routing decision` precisely.
- Decide account/project policy precedence and default-deny/default-allow rules.
- Define stable decision reason codes and the audit/metric representation.
- Define compatibility behavior for the existing
  `X-Gateway-Fallback-Models` header.
- Keep endpoint execution in `internal/dispatch`; model-chain resolution happens
  before dispatch receives its input.

Acceptance gates:

- the ADR includes failure behavior, cache consistency, rollback, and migration;
- the domain contract can represent the existing routing behavior without loss;
- no implementation begins until the hot-path query and cache shape are clear.

### M1.2 — Rule-based virtual models

Status: complete.

- Persist virtual-model policies and candidate order/weight.
- Resolve a virtual model to a validated model chain under the caller's account
  permissions.
- Support deterministic constraints first: region, modality/capability, maximum
  attempts, and allow/deny model lists.
- Expose CRUD, validation, and dry-run evaluation through the Console API.
- Record requested model, routed model, policy version, and reason code.

Acceptance gates:

- existing concrete-model requests behave unchanged;
- virtual-model decisions are deterministic under fixed inputs;
- invalid or unsubscribed candidates never enter dispatch;
- policy updates follow the documented cache consistency contract.

### M1.3 — Latency and cost objectives

Status: complete.

- Reuse selector runtime statistics for observed health and latency.
- Introduce a compact versioned routing cost profile, separate from billing
  pricing and invoice calculation.
- Add bounded latency/cost scoring with exploration safeguards.
- Provide dry-run comparison and per-decision score explanation.

Acceptance gates:

- a routing decision can be reconstructed from its policy version and inputs;
- missing/stale telemetry has an explicit neutral fallback;
- cost-aware routing does not add downstream billing calls to the request path;
- tests cover constraint conflicts and all-candidates-ineligible behavior.

Semantic/quality-based routing is deliberately deferred until rule-based routing
has production traces, evaluation data, and an agreed quality signal.

## Milestone 2 — Pluggable policy enforcement

Goal: make the gateway a vendor-neutral policy enforcement point, not a
content-safety product. Detection logic, enterprise DLP, and moderation APIs
remain replaceable engines behind one stable contract.

### M2.1 — Policy decision contract

Status: complete.

- Evolve the current error-only moderation result into explicit actions:
  `allow`, `deny`, and `redact`.
- Define stable rule IDs, policy versions, reason codes, and safe audit metadata.
- Define policy binding and precedence for global/account/project/API-key scopes.
- Preserve the guard-chain extension point and avoid coupling policy evaluation
  to Gin or to a vendor protocol.
- Define engine failure and unsupported-redaction behavior as fail-closed.

Acceptance gates:

- policy evaluation never logs matched secrets or raw sensitive content;
- every mutation is visible in audit metadata without storing the sensitive
  before/after value;
- existing moderation configuration has a documented compatibility path.

### M2.2 — Request enforcement and mutation execution

- Add structured text extraction for supported request protocols as an adapter,
  not as part of the engine contract.
- Apply engine-provided mutations without making the gateway own detection.
- Rebuild the upstream request safely after redaction while preserving envelope
  routing fields and protocol translation behavior.
- Add policy binding persistence, Console CRUD, and simulation with synthetic
  engine decisions.

Acceptance gates:

- original sensitive values do not reach upstream, logs, traces, or audit;
- redaction works across supported content-block shapes;
- malformed or unsupported structures have a documented fail-open/fail-closed
  policy.

### M2.3 — Response enforcement modes

- Define `strict-buffered` and `best-effort-streaming` output modes.
- Add decoded-text accumulation across SSE frames for best-effort scanning.
- Make strict mode buffer before client delivery and document its latency/memory
  cost.
- Keep provider moderation and enterprise DLP as adapters; reference engines
  are examples, not a gateway-owned safety platform.

Acceptance gates:

- API responses and Console policy configuration expose the selected guarantee;
- tests cover a sensitive term split across transport chunks and SSE frames;
- a post-first-byte violation has explicit truncation and audit semantics.

## Milestone 3 — Enterprise identity and hierarchy

Goal: add organization/project self-service only after the deployment model and
authorization boundaries are proven.

### M3.1 — Identity and tenancy ADR

- Decide whether current `account` maps to organization, tenant, or billing
  account and document the migration.
- Define project/application ownership, quota inheritance, model subscriptions,
  API-key ownership, and BYOK boundaries.
- Define tenant-scoped roles before adding new tables.

### M3.2 — OIDC and scoped RBAC

- Add OIDC login for the Console while retaining a break-glass operator path.
- Enforce resource-scoped authorization in Store/repository operations, not only
  in UI routing.
- Include actor subject, organization/project scope, and policy version in audit.

Acceptance gates:

- cross-tenant access tests cover every management resource;
- authorization cannot be bypassed by calling the Admin API directly;
- audit identifies both human actor and affected resource scope.

## Delivery order

Work proceeds in this order:

1. M0.1 product narrative
2. M0.2 one-command demo
3. M0.3 dashboard and M0.4 benchmark harness
4. M1.1 routing ADR
5. M1.2 rule-based virtual models
6. M2.1 governance contract

Milestone 3 is not started until a concrete multi-tenant product requirement
exists. M1.3 and M2.2/M2.3 can proceed after their respective contracts and may
be prioritized using production feedback.

## Definition of done for roadmap work

Every roadmap change must include:

- compatibility and migration notes where persistent data or API behavior changes;
- unit tests plus an integration or smoke assertion for its user-visible path;
- metrics, reason codes, and safe audit behavior for policy decisions;
- English and Chinese user documentation for public behavior;
- no unexplained degradation in the published performance baseline.
