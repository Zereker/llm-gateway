[English](11-public-contracts.md) | [简体中文](11-public-contracts.zh-CN.md)

# Public contracts

This document defines the surfaces that operators, API clients, and downstream
consumers may depend on. Internal Go package names are not public contracts.

## HTTP API versions

The data plane exposes OpenAI-compatible operations under `/v1`:

- `POST /v1/chat/completions`
- `POST /v1/responses`
- `POST /v1/embeddings`
- `POST /v1/messages` for the Anthropic-compatible request shape

The control plane exposes management operations under `/api/v1`. Authentication
role names do not appear in the URL: `/api/v1` is the version boundary, while
Bearer authentication determines whether an actor is an admin or viewer. The
Console UI at `/` and operational probe at `/healthz` are not API resources.
Unversioned `/admin/*` management routes do not exist.

## Control-plane input contract

Mutation bodies must use `Content-Type: application/json` (parameters such as
`charset=utf-8` are allowed). A body:

- contains exactly one JSON value;
- is at most 1 MiB;
- uses only fields declared by the target request type;
- fails with `invalid_json` when malformed, oversized, trailing, or unknown.

Unsupported media types fail with HTTP 415 and `unsupported_media_type`.
Documented query fields are also strict: malformed identifiers, booleans, or
limits fail with `invalid_argument` rather than silently changing the query.
Audit list limits are restricted to 1 through 1000.

## Error envelopes

Data-plane errors use the OpenAI-compatible gateway envelope and include stable
machine code, class, request ID, and trace ID:

```json
{
  "error": {
    "code": "model_not_found",
    "message": "model not found: example",
    "class": "invalid",
    "request_id": "req_...",
    "trace_id": "..."
  }
}
```

Control-plane errors use one smaller stable envelope:

```json
{
  "error": {
    "code": "endpoint_invalid",
    "message": "endpoint failed validation",
    "details": {"reasons": ["invalid_url"]}
  }
}
```

Clients branch on `error.code`, never on the human-readable message. Structured
diagnostics belong under `error.details`; handlers must not add ad-hoc siblings
to the error object. Secrets and raw prompt content must never appear there.

## Configuration contract

Gateway and Console YAML decoders reject unknown fields. Removed or misspelled
keys therefore stop startup instead of being ignored. Environment variables are
limited to the documented secret/connection overrides; they do not redefine
behavioral policy.

The former empty `paths` gateway section is not part of the contract. Concrete
file destinations live with their owners, such as `usage_events.file.path` and
`content_log.file.path`.

## Schema, events, metrics, and reason codes

- MySQL schema history starts at `internal/infra/migrations/000001_base.sql`.
  Merged migration files are immutable; evolution adds a numbered file.
- Usage records carry `schema_version: usage.v1`. A breaking event change uses a
  new schema version and downstream topic/file contract.
- Prometheus names and bounded label sets are machine contracts checked against
  the shipped dashboard and alert rules.
- Routing reason codes are bounded, non-secret values. Header-selected concrete
  fallbacks use candidate source `fallback_header` and reason
  `fallback_accepted`; virtual-model policy decisions retain their policy reason
  even when a caller sends an ignored fallback header.

## Change policy

Before the first release, obsolete pre-release surfaces are removed rather than
kept as aliases. Starting with the first tagged release, a breaking HTTP or event
change requires a new version; schema changes require a new immutable migration.
Every public-contract change includes tests and matching English/Chinese docs.
