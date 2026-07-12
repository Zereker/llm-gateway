# testdata/

Repo-root shared test fixtures — anything used by more than one package (or
by both a unit-test layer and the integration-test layer) lives here, not
nested inside whichever package happened to consume it first. See
`internal/cassette.TestdataPath` for how a test file finds this directory:
it resolves an **absolute** path from `internal/cassette/cassette.go`'s own
source location, so any package can call `cassette.TestdataPath("vendor-cassettes", "...")`
regardless of its own nesting depth or the test runner's working directory —
no hand-counted `"../../..."` relative paths to get wrong or silently break
when a file moves.

```
testdata/
├── vendor-cassettes/   real, raw, third-party-licensed VCR cassettes (reference corpus)
└── fieldmatrix/        curated/sanitized fixtures for the gateway's own e2e suite
```

## `vendor-cassettes/` — real upstream traffic, unmodified

Real request/response pairs recorded by third-party open-source projects
against actual vendor APIs (Anthropic / Gemini / Cohere / OpenAI), vendored
in as-recorded (only auth headers scrubbed, by the recording tool itself,
not by us). See `vendor-cassettes/README.md` for full provenance/license
detail per source.

**Consumers:**
- `internal/cassette` — the loader (`Load`/`LoadDir`) that parses both
  on-disk cassette formats found across these sources.
- `internal/cassette/replay` — unit-ish tests that replay every interaction
  in every file through the real translator/extractor code
  (`internal/translator/openai_anthropic` etc.), asserting no case is
  silently unaccounted for (see that package's `zzz_completeness_test.go`).
- `internal/app/gateway`'s `TestE2E_MultiVendor_AllProtocols` — pulls
  specific real response bodies from here as the canned reply for a mock
  upstream, so the *integration* test exercises real vendor data too, one
  layer above the translator.

**When you need real data for a new unit test**: check here first before
writing a synthetic literal. If the exact shape you need isn't covered yet,
see that directory's README for how to add a new source.

## `fieldmatrix/` — curated e2e fixtures

Two kinds of file, both purpose-built for `internal/app/gateway`'s own e2e
suite (`fieldmatrix_test.go`, `fieldmatrix_multivendor_test.go`):
- `*.json` — full-parameter **client request** bodies (every field a real
  upstream is known to accept), used to assert the gateway forwards fields
  it should and drops/rejects fields it can't translate.
- `upstream/*.json|*.sse` — **sanitized derivatives** of real captured
  vendor responses (see `upstream/README.md`), shaped for a specific e2e
  scenario (a truncated array, a redacted opaque blob) rather than kept
  byte-for-byte like `vendor-cassettes/`.

If you're not sure whether a new fixture belongs here or in
`vendor-cassettes/`: raw/unmodified real traffic goes in `vendor-cassettes/`;
anything hand-shaped for one specific test scenario goes in `fieldmatrix/`.
