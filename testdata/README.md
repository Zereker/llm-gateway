# testdata/

Repo-root shared test fixtures — the **llm-gateway-specific** scaffolding the
e2e suite needs. The bulk real-traffic corpora are *not* here: they come from
the opencassette Go module (see "Real cassette corpora" below). What remains is
`fieldmatrix/` — hand-shaped fixtures, endpoint manifests, and golden outputs
purpose-built for this gateway's tests. `internal/cassette.TestdataPath`
resolves an **absolute** path to this directory from `cassette.go`'s own source
location, so any package can call e.g. `cassette.TestdataPath("fieldmatrix",
"endpoints")` regardless of its nesting depth or the test runner's working
directory — no hand-counted `"../../..."` relative paths to get wrong.

```
testdata/
└── fieldmatrix/        curated/sanitized fixtures + manifests for the gateway's own e2e suite
    ├── endpoints/      per-vendor endpoint-seed manifests (vendor/protocol/model/auth/reply)
    ├── upstream/       sanitized derivative upstream responses for specific e2e scenarios
    └── golden/         hand-reviewed exact-match fixtures for internal/cassette/replay's TestGolden* tests
```

## Real cassette corpora — the opencassette Go module

The real recorded vendor traffic that used to live here (`vendor-cassettes/`)
now comes from the [`github.com/zereker/opencassette`](https://github.com/zereker/opencassette)
module, which embeds two corpora and exposes each as an `fs.FS`:

- `opencassette.Corpus()` — opencassette's **own** recordings against its
  scenario packs, covering vendors for which no public recorded traffic existed
  at all (Zhipu GLM, MiniMax, Moonshot Kimi, Volcano ARK) plus fresh AWS Bedrock
  (Anthropic wire) / Azure (OpenAI + Responses) / Google Gemini captures.
- `opencassette.Vendored()` — the **third-party** cassettes (langchain partner
  packages, simonw's `llm-*` plugins) recorded against live vendor APIs and
  published under Apache-2.0 / MIT, kept for their value as a real-world
  reference of each vendor's wire shape (Anthropic / Gemini / Cohere / OpenAI /
  Bedrock).

`internal/cassette` reads both via `LoadFS` / `LoadDirFS`, handling the two
cassette formats (pytest-recording's `interactions:` and langchain's parallel
`requests:`/`responses:` lists) plus transparent gunzip (`*.yaml.gz` and
gzipped bodies). Because the corpora are embedded in a versioned dependency, a
plain `go test` resolves them — no submodule, no checked-out tree. Recording new
traffic (a recorder, credential scrubbing, scenario packs) also lives in
opencassette, not here.

**Consumers:**
- `internal/cassette/replay` — replays every cassette in both corpora through
  the real translator/extractor code, with completeness enforcement (a file
  that stops being claimed is a hard failure). `TestReplayOpenCassetteCorpus`
  covers `Corpus()`; the per-vendor `TestReplay*` suites cover `Vendored()`.
- `internal/app/gateway`'s `TestE2E_MultiVendor_AllProtocols` and the
  `cmd/mockupstream` smoke test — a mock upstream replays a real captured body
  per manifest, so the full middleware chain runs on real data. `reply.kind`
  picks the source (see `endpoints/` below).

## `fieldmatrix/` — curated e2e fixtures (llm-gateway-specific)

Purpose-built for `internal/app/gateway`'s own e2e suite (`fieldmatrix_test.go`,
`fieldmatrix_multivendor_test.go`) — hand-shaped for specific scenarios, unlike
the raw/unmodified opencassette corpora:

- `*.json` — full-parameter **client request** bodies (every field a real
  upstream is known to accept), used to assert the gateway forwards fields it
  should and drops/rejects fields it can't translate.
- `upstream/*.json|*.sse` — **sanitized derivatives** of real captured vendor
  responses (see `upstream/README.md`), shaped for a specific e2e scenario (a
  truncated array, a redacted opaque blob) rather than kept byte-for-byte.

Rule of thumb: raw/unmodified real traffic belongs in the opencassette corpora;
anything hand-shaped for one specific test scenario goes in `fieldmatrix/`.

### `endpoints/` — per-vendor endpoint-seed manifests

One JSON file per upstream vendor (see `internal/cassette/vendorfixture` for the
loader and exact field shape): vendor / protocol / model / auth type / auth
payload / which upstream path to route to / which real response to reply with.
`reply.kind` selects the response source:

- `"opencassette"` — a cassette from `opencassette.Corpus()` (our own recordings).
- `"cassette"` — a cassette from `opencassette.Vendored()` (third-party corpus).
- `"fixture"` — a whole file from `fieldmatrix/upstream/` (a curated derivative).

**Consumers**, both reading the *same* files so there is exactly one place that
declares "these are the vendors this gateway supports end-to-end":
- `internal/app/gateway`'s `TestE2E_MultiVendor_AllProtocols` — in-process e2e.
- `scripts/seed-multivendor` — seeds a real MySQL instance for
  `scripts/e2e-smoke-multivendor.sh`'s real-binary (`cmd/gateway` +
  `cmd/mockupstream`) black-box run.

Adding a vendor to *both* is one new JSON file here — see any existing file for
the shape, and `cmd/mockupstream`'s doc comment for which `upstream_path` values
it actually serves.

### `golden/` — exact-match fixtures (a stricter companion to the replay suite)

`internal/cassette/replay`'s ordinary tests (`TestReplay*`) only assert "this is
a well-formed OpenAI response" — they'd miss a translator bug that swaps two
fields but keeps the shape valid (e.g. mapping `stopReason: tool_use` to
`finish_reason: "stop"` instead of `"tool_calls"`). The handful of `TestGolden*`
tests compare a translator's *exact* output against a fixture here, byte for
byte (after normalizing the random `id` / `created` timestamp every `openai_*`
translator generates, so the comparison isn't flaky by construction).

**These fixtures are not self-verifying.** A new one is only trustworthy after a
human reads the generated output next to the real cassette interaction it came
from and confirms the translation is actually correct — regenerating a fixture
from a buggy translator bakes the bug in as the new "expected" output. Workflow:

```sh
UPDATE_GOLDEN=1 go test ./internal/cassette/replay/... -run TestGolden -v
git diff testdata/fieldmatrix/golden/   # read it by hand before committing
```

Use this sparingly — on scenarios worth pinning down precisely (a tool call, an
extended-thinking turn) — not as a wholesale replacement for the shape-only
checks across the full corpora.
