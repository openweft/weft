# Reproducible builds

The openweft release pipelines (`weft`, `weft-proxy`, `weft-webui`,
`weft-network`) are configured for bit-for-bit reproducibility: two
independent CI runs of the same tag produce byte-identical tarballs
and OCI layers. Page documents the guarantee, the verifier workflow,
and what's **not** reproducible (chase those with `cosign-verify.md`).

## What is pinned

All four release workflows export `SOURCE_DATE_EPOCH` from the build
commit's authored-date:

```
SOURCE_DATE_EPOCH=$(git log -1 --format=%ct ${GITHUB_SHA})
```

This is the canonical reproducible-builds.org convention: the epoch
is derived from the commit itself (not the wall-clock at build time),
so two CI runs of the same tag see the same epoch.

For `weft` and `weft-proxy` (Go binaries built at the workflow level):

- `go build -trimpath` strips the absolute build path from file/line
  info embedded in the binary.
- `go build -buildvcs=false` suppresses VCS revision/dirty/timestamp
  stamped into `.note.go.buildinfo` (Go 1.18+ default-embeds these).
- `-ldflags "-buildid="` zeroes the Go internal build ID (otherwise
  derived from action-graph hashes that vary with module-cache mtimes).
- `-ldflags "-s -w"` strips DWARF + symbol tables.
- Tarballs use `tar --sort=name --mtime=@${SOURCE_DATE_EPOCH}
  --owner=0 --group=0 --numeric-owner` + `gzip -n` so neither the tar
  entry metadata nor the gzip header timestamp varies.

For `weft-webui` and `weft-network` (Docker multi-stage builds):

- The release workflow passes `SOURCE_DATE_EPOCH` as both a buildkit
  env var and a `--build-arg`. Buildkit ≥ 0.11 honours the env var
  natively, rewriting layer mtimes and the image-config `created`
  field to the epoch.
- `provenance: false` + `sbom: false` on `docker/build-push-action`
  disables buildkit's auto-attached attestations (those embed wall
  clock and break the digest). We attach our own SBOM + SLSA
  provenance in dedicated steps where the timestamp is expected to
  vary (see "Not reproducible" below).
- The Dockerfile build stages **should** consume `ARG
  SOURCE_DATE_EPOCH` to drive `go build -trimpath -buildvcs=false
  -ldflags=-buildid=...` for their own go invocations. The release
  workflow can only pass the arg ; the Dockerfile is the source of
  truth for the in-image build flags and is out of scope of this
  page. If a future Dockerfile bump drops the flags the verifier
  will catch the regression.

## Verifying a release

Each repo ships a `verify-release.yml` workflow, dispatched manually:

```
gh workflow run -R openweft/weft verify-release.yml -f tag=v0.1.0
gh workflow run -R openweft/weft-proxy verify-release.yml -f tag=v0.1.0
gh workflow run -R openweft/weft-webui verify-release.yml -f tag=v0.1.0
gh workflow run -R openweft/weft-network verify-release.yml -f tag=v0.1.0
```

For each `(os, arch)` in the release matrix the verifier:

1. Checks out the tag.
2. Builds the artifact twice, each pass in an isolated sandbox
   (separate container for go builds ; separate buildx instance and
   OCI tarball output for Docker builds).
3. `sha256sum`s both artifacts and asserts they're identical.
4. Posts the two hashes to the run's step-summary ; on mismatch it
   exits non-zero and surfaces a per-blob diff so the operator can
   chase the offending layer/file.

The verifier never publishes or signs ; it only reads. It runs only
on `workflow_dispatch` so it cannot fire on a `push` event (per
`feedback_no_autopublish_dev`).

## Not reproducible (by design)

The following artifacts attached to a release have timestamps or
non-deterministic identifiers and are deliberately **not** covered by
the sha256 verifier:

- **Syft SBOM JSON** (`*.spdx.json`). Syft stamps
  `creationInfo.created` to wall-clock at scan time. Workaround if
  you need a reproducible SBOM: `syft scan ... --source-date-epoch
  $SOURCE_DATE_EPOCH` (Syft ≥ 1.10), which rewrites
  `creationInfo.created` to the epoch. The release workflows have
  not been updated to pass the flag yet ; verifying the SBOM still
  relies on the cosign attestation (binds SBOM bytes to image digest
  cryptographically), not on sha256 equality.
- **Sigstore signatures** (`*.sig`, `*.cert`,
  `*.spdx.intoto.jsonl`). Each sigstore signing event is recorded in
  the Rekor transparency log with a unique entry index and an
  ephemeral OIDC-derived certificate. Two runs of the same tag
  produce different signatures over the same bytes — that is
  Sigstore working as designed (non-repudiable, append-only log).
  Verify these with `cosign verify-blob` / `cosign verify` against
  the certificate identity, not with sha256.
- **SLSA provenance** (`*.intoto.jsonl` from
  `slsa-github-generator`). Embeds the workflow run ID and start/end
  timestamps. Verify with `slsa-verifier` against the expected
  builder identity, not with sha256.
- **GHCR multi-arch index**. The `imagetools create` step at the end
  of the release flow assembles the per-arch manifests into a single
  index. The index digest is content-addressed but the
  `org.opencontainers.image.created` annotation buildx attaches at
  index-assembly time may differ between runs ; the per-arch
  manifest digests it points to are reproducible.

## Reading the verifier output

A successful verifier run emits a `::notice::` per matrix leg and the
step summary lists matching sha256s, e.g.:

| pass | sha256 |
|------|--------|
| 1 | `c0ffee...` |
| 2 | `c0ffee...` |

A failing run emits `::error::` and a diff of per-file/per-blob
sha256s so you can tell whether the divergence was in the binary
itself, the tar header, the gzip header, or (for OCI builds) the
image config vs a specific layer.

## Reproducing locally

The verifier mirrors what a downstream consumer would run:

```
# Go binary
git clone --branch v0.1.0 https://github.com/openweft/weft && cd weft
export SOURCE_DATE_EPOCH=$(git log -1 --format=%ct HEAD)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -trimpath -buildvcs=false \
  -ldflags "-s -w -buildid= -X main.version=v0.1.0" \
  -o weft ./cmd/weft
tar --sort=name --mtime=@${SOURCE_DATE_EPOCH} \
    --owner=0 --group=0 --numeric-owner \
    -cf - weft | gzip -n -9 > weft.tar.gz
sha256sum weft.tar.gz
```

Compare the hash against `SHA256SUMS` on the GitHub release. If they
match you've verified the upstream tarball was built from the source
you just compiled.
