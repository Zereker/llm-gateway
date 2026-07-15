[English](RELEASE.md) | [简体中文](RELEASE.zh-CN.md)

# Release process

The first planned tag is `v0.1.0`. Merging release-readiness changes does not
publish anything; a maintainer creates the tag only after every gate below is
complete.

## Before tagging

1. Confirm `main` is clean, protected CI is green, and `codecov/project` plus
   `codecov/patch` pass.
2. Review `SECURITY.md`, public contracts, supported platforms, dependency
   versions, and the upgrade boundary.
3. Set the release number in `deploy/helm/llm-gateway/Chart.yaml`: Chart
   `version` uses `0.1.0`, while `appVersion` uses the published image tag
   `v0.1.0`.
4. Move user-visible changes from `Unreleased` to the dated release section in
   both changelogs.
5. Run the release-equivalent checks:

   ```sh
   go vet ./...
   go test ./internal/...
   make release-snapshot VERSION=v0.1.0
   (cd dist && sha256sum --check SHA256SUMS)
   ```

6. Verify both locally built commands report the expected version with
   `make build VERSION=v0.1.0` and `bin/* -version`.
7. Confirm the production Helm chart renders only with explicit Secret input,
   and the Quickstart, multi-vendor smoke test, benchmark, and production image
   checks passed in CI.

## Tag and publish

Create an annotated, signed tag from the reviewed `main` commit and push only
that tag:

```sh
git switch main
git pull --ff-only
git tag -s v0.1.0 -m "llm-gateway v0.1.0"
git push origin v0.1.0
```

The `Release` workflow then:

- verifies the tag, Chart, and changelog versions agree;
- runs source tests and builds both commands for every supported platform;
- publishes archives, `SHA256SUMS`, and the packaged Helm chart to GitHub;
- publishes separate multi-architecture Gateway and Console images to GHCR;
- embeds the tag, commit, and build date in every binary and image.

## After publishing

1. Download one archive from GitHub and verify it against `SHA256SUMS`.
2. Run both commands with `-version`.
3. Pull both images by immutable tag, verify they run as UID/GID `65532:65532`,
   and exercise `/healthz` with a synthetic deployment.
4. Install the packaged Chart in a disposable namespace and verify readiness,
   one authenticated request, Console access, metrics, and clean shutdown.
5. Mark the release as non-draft only when these checks pass, then announce the
   compatibility and migration boundary.

Never move or reuse a published tag. If publishing is wrong, document the
problem, create the next patch version, and keep existing artifacts available
for audit. Never edit a migration that shipped in a tag.
