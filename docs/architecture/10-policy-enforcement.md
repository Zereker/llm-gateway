[English](10-policy-enforcement.md) | [简体中文](10-policy-enforcement.zh-CN.md)

# Pluggable policy enforcement

The gateway is a vendor-neutral enforcement point, not a content-safety or DLP
product. A replaceable `policy.Engine` decides; M8 applies the decision to the
exact client-facing request or translated response.

```text
trusted identity + runtime-only text
                 |
                 v
            policy.Engine
                 |
        allow / deny / redact
                 |
                 v
       M8 enforcement + safe audit
```

## Decision contract

`internal/policy.Engine` receives the stage, trusted subject, model/modality,
runtime-only content, extracted text segments, and the selected persisted
policy reference. It returns `allow`, `deny`, or `redact` with a policy version,
stable rule ID, and reason code. Engine errors, malformed decisions, invalid
mutations, unsupported structures, and buffer overflow all fail closed.

Content bytes, replacements, and internal causes are excluded from JSON and
`AuditRecord`. Mutation audit contains only ID, kind, and RFC 6901 target.
Explicit engines never expose `Decision.Cause` to clients; the legacy adapter
alone preserves the old input-moderation error message for compatibility.

M8 owns the audit in its onion return phase. It collects input and output
records while downstream runs, then emits `policy_decision` after `c.Next()`.
Each record distinguishes the decision action from enforcement result:
`allowed`, `denied`, `applied`, or `failed`. M10 does not relay policy state.

## Binding and persistence

Immutable definitions live in `policy_definitions`; `policy_bindings` selects
one definition for a trusted scope. Resolution is one query with precedence:

```text
API key > project > account > global
```

Definitions are complete snapshots and are not merged. Positive and negative
lookups use bounded TTL caches; Console writes publish a cache-bus invalidation.
Project is present in the domain contract but Console rejects project bindings
until authentication and RBAC produce a trusted project identity.

When there is no binding, existing moderation configuration behaves as before.
An active binding that requires input or output enforcement but has no engine
configured fails closed. Responses expose the effective configuration:

- `X-Gateway-Policy-ID: <policy-id>@<version>`
- `X-Gateway-Policy-Output-Mode: disabled|strict_buffered|best_effort_streaming`

## Request extraction and redaction

`JSONDocumentAdapter` extracts only known user/content text nodes from OpenAI
Chat, Responses, Anthropic, Gemini, and embedding-shaped JSON. It does not
extract routing fields, tool definitions, image URLs, or arbitrary metadata.
Segments carry RFC 6901 JSON Pointers such as `/messages/0/content` or
`/input/0/content/0/text`.

For `redact`, every target must be one of the extracted UTF-8 text nodes.
Unknown, duplicate, non-text, routing, or malformed targets reject the whole
mutation set. The adapter rebuilds a fresh JSON document atomically, preserves
routing and protocol fields, replaces both the envelope bytes and request body,
and only then permits translation/upstream dispatch. Therefore a partial
mutation is never forwarded.

The Console simulation endpoint accepts a synthetic decision and body. It
executes this same adapter without calling a detector or upstream model:

- `GET/POST /admin/policies`
- `DELETE /admin/policies/:policyID`
- `GET/POST /admin/policy-bindings`
- `DELETE /admin/policy-bindings/:scopeKind?scope_id=...`
- `POST /admin/policies/simulate`

## Response modes

`disabled` skips output evaluation. The other modes have deliberately different
guarantees:

| Mode | Delivery | Guarantee and failure behavior |
|---|---|---|
| `strict_buffered` | Buffers the complete translated client response before committing headers | Full response is evaluated once; allow/redact is released atomically, deny/engine error/invalid mutation/buffer overflow returns a gateway error before first byte |
| `best_effort_streaming` | Delivers each accepted frame immediately | Decoded text is accumulated across SSE frames, so terms split across frames can be detected; a later violation truncates the stream because prior bytes cannot be recalled; streaming redaction fails closed |

Strict mode defaults to a 4 MiB per-response cap, is configurable per
definition, and rejects values above 64 MiB. Best-effort decoding retains a
bounded 64 KiB rolling text window. Strict mode adds response-size memory and full-response latency. Strict
redaction requires the buffered client output to be a supported JSON document;
an entire SSE transcript is not a single JSON document and therefore fails
closed for redaction.

The invoker delays upstream response headers/status until the first accepted
client byte. Thus strict-mode failures remain normal JSON errors. After a
best-effort stream commits, errors are recorded as truncated usage and cannot
trigger retry or fallback.

## Compatibility and extension

The denylist, OpenAI moderator, `Moderator`, guard chain, and stream decorator
remain supported through `moderation.LegacyEngine`. A new integration should
implement `policy.Engine` and inject it with `middleware.WithPolicyEngine` (or
`router.Deps.PolicyEngine`) while leaving detection and vendor credentials
outside the gateway's policy domain.
