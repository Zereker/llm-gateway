[English](CHANGELOG.md) | [简体中文](CHANGELOG.zh-CN.md)

# Changelog

This file records user-visible changes. The project follows Semantic Versioning
starting with the first tagged release.

## [Unreleased]

## [0.1.0] - 2026-07-15

Initial public release.

### Added

- OpenAI-compatible Chat Completions, Responses, and Embeddings APIs, plus an
  Anthropic-compatible Messages API.
- Multi-provider upstream support with protocol translation, endpoint-level
  quirks, capability filtering, weighted/P2C selection, cooldown, retry, and
  explicit cross-model fallback.
- Rule-based virtual models with deterministic constraints, latency/cost
  objectives, dry-run evaluation, and explainable routing decisions.
- API-key authentication, account subscriptions, layered quota, pluggable
  policy enforcement, request redaction, explicit streaming response modes,
  safe audit metadata, and content logging.
- A separate versioned Console control plane under `/api/v1`, including Web UI,
  admin/viewer roles, endpoint/key/policy/pricing management, and write audit.
- MySQL schema lifecycle, Redis-backed runtime state, file/Kafka usage events,
  Prometheus metrics, OTel/slog tracing, Helm deployment assets, a one-command
  Quickstart, and a reproducible performance benchmark.
- Checksummed release archives, versioned Gateway/Console container images, and
  embedded `-version` build metadata.

### Compatibility boundary

- `v0.1.0` establishes the first public HTTP, configuration, schema, usage-event,
  metric, and reason-code contracts described in
  [public contracts](architecture/11-public-contracts.md).
- Pre-release databases must be recreated before adopting `v0.1.0`. Starting
  with this release, merged migration files are immutable and later schema
  evolution adds new numbered migrations.
