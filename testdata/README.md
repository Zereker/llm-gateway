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
    ├── endpoints/      per-vendor endpoint-seed manifests (vendor/protocol/model/auth/reply)
    └── golden/         hand-reviewed exact-match fixtures for internal/cassette/replay's TestGolden* tests
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

### `endpoints/` — per-vendor endpoint-seed manifests

One JSON file per upstream vendor (see `internal/cassette/vendorfixture` for
the loader and exact field shape): vendor / protocol / model / auth type /
auth payload / which upstream path to route to / which real response
(`vendor-cassettes/` or `fieldmatrix/upstream/`) to reply with.

**Consumers**, both reading the *same* files so there is exactly one place
that declares "these are the vendors this gateway supports end-to-end":
- `internal/app/gateway`'s `TestE2E_MultiVendor_AllProtocols` — in-process e2e.
- `scripts/seed-multivendor` — seeds a real MySQL instance for
  `scripts/e2e-smoke-multivendor.sh`'s real-binary (`cmd/gateway` +
  `cmd/mockupstream`) black-box run.

Adding a vendor to *both* is one new JSON file here — see any existing file
for the shape, and `cmd/mockupstream`'s doc comment for which
`upstream_path` values it actually serves.

### `golden/` — exact-match fixtures (a stricter companion to the replay suite)

`internal/cassette/replay`'s ordinary tests (`TestReplay*`) only assert
"this is a well-formed OpenAI response" — they'd miss a translator bug that
swaps two fields but keeps the shape valid (e.g. mapping `stopReason:
tool_use` to `finish_reason: "stop"` instead of `"tool_calls"` — a real
regression a coverage number can't tell you is caught). The handful of
`TestGolden*` tests in that package compare a translator's *exact* output
against a fixture here, byte for byte (after normalizing the two fields
every `openai_*` translator generates itself — a random `id`, a `created`
timestamp — so the comparison isn't flaky by construction).

**These fixtures are not self-verifying.** A new one is only trustworthy
after a human reads the generated output next to the real cassette
interaction it came from and confirms the translation is actually correct —
regenerating a fixture from a translator that has a bug bakes the bug in as
the new "expected" output. Workflow:

```sh
UPDATE_GOLDEN=1 go test ./internal/cassette/replay/... -run TestGolden -v
git diff testdata/fieldmatrix/golden/   # read it by hand before committing
```

Use this sparingly — on scenarios worth pinning down precisely (a tool call,
an extended-thinking turn) — not as a wholesale replacement for the
shape-only checks across the full real-cassette corpus, which stay useful
for "did this crash or produce garbage" on everything else.
