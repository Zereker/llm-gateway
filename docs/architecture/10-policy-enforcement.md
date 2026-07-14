# Pluggable policy enforcement

The gateway is a policy enforcement point. It does not own a complete content
safety, regulatory knowledge, or enterprise DLP product. Engines decide;
gateway middleware enforces the returned decision.

```text
request/response bytes + trusted identity
                  |
                  v
             policy.Engine
                  |
        allow / deny / redact
                  |
                  v
       gateway enforcement + safe audit
```

## Contract boundary

`internal/policy.Engine` receives a vendor-neutral stage, trusted subject,
model/modality metadata, and runtime-only content. It returns a versioned
decision with a stable rule ID and reason code. Content bytes, mutation
replacement bytes, and internal causes are excluded from JSON and audit.

`allow` continues the pipeline. `deny` returns the gateway's bounded
`content_rejected` error without exposing a new engine's internal reason.
`redact` carries explicit mutations; until M2.2 installs a safe mutation
executor, it fails closed rather than pretending content was changed.
Engine errors and malformed decisions also fail closed.

Every valid input and output decision produces an `AuditRecord`. Mutation
audit contains only ID, kind, and structural target—never matched text or the
replacement value.

M8 owns these audit events. It collects decisions while downstream handlers
run and flushes them through the shared `AuditTracer` after its `c.Next()`
returns; M10 does not carry or interpret policy state.

## Binding precedence

Bindings use trusted identity resolved by authentication. Caller headers cannot
select policy scope. Precedence is:

```text
API key > project > account > global
```

The highest enabled immutable version wins within one scope. Project scope is
part of the contract but remains inactive in product configuration until a
trusted project identity and scoped RBAC exist.

## Compatibility

The existing `moderation.driver`, denylist, OpenAI moderator, `Moderator`
interface, guard chain, and streaming wrapper remain supported. M8 adapts the
legacy error-only interface to `policy.Engine` decisions through
`moderation.LegacyEngine`. Legacy client error text is retained for backward
compatibility, but it is never copied into safe audit metadata.

New integrations should implement `policy.Engine` and use
`middleware.WithPolicyEngine`. The existing router exposes this port directly;
when supplied, it takes precedence over the legacy moderator.

## Streaming boundary

M2.1 changes the decision contract, not the transport guarantee. The current
output adapter still evaluates translated output chunks. Strict buffered and
best-effort decoded streaming guarantees remain M2.3 work and must not be
inferred from the presence of an engine.
