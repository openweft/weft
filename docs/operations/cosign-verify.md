# Verifying signed weft artifacts (Sigstore keyless)

Every weft release tag (`v<X.Y.Z>`) produces Sigstore-signed artifacts. The
identity that signed them is the GitHub Actions runner OIDC, **not** a
long-lived public key — there is no `cosign.pub` to fetch. Verification
is done by pinning the OIDC issuer and the certificate subject (the
workflow path) to the values below.

## TL;DR — two one-liners

Tarball (downloaded from the GitHub Release page, with its `.sig` + `.cert`
sidecars next to it):

```sh
cosign verify-blob \
  --certificate      weft-linux-amd64_v0.1.0.tar.gz.cert \
  --signature        weft-linux-amd64_v0.1.0.tar.gz.sig \
  --certificate-identity-regexp '^https://github\.com/openweft/weft/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' \
  --certificate-oidc-issuer     'https://token.actions.githubusercontent.com' \
  weft-linux-amd64_v0.1.0.tar.gz
```

OCI image (multi-arch index — pulls the signature object from ghcr.io
itself, nothing to download by hand):

```sh
cosign verify ghcr.io/openweft/weft:v0.1.0 \
  --certificate-identity-regexp '^https://github\.com/openweft/weft/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' \
  --certificate-oidc-issuer     'https://token.actions.githubusercontent.com'
```

Both commands exit 0 only if the signature chains to the Sigstore Fulcio
root **and** the embedded certificate's SAN matches both the issuer and
the workflow subject. Anything else — wrong repo, wrong workflow, expired
cert, tampered blob — fails closed.

## What's signed

| Artifact | Verify with | What this proves |
|---|---|---|
| `weft-<os>-<arch>_v<X.Y.Z>.tar.gz` | `cosign verify-blob` + `.sig` + `.cert` | The tarball bytes were produced by the `release.yml` workflow on the `openweft/weft` repo at tag `v<X.Y.Z>`. |
| `ghcr.io/openweft/weft:v<X.Y.Z>-<arch>` | `cosign verify` | The per-arch image manifest was signed by-digest right after `docker/build-push-action` pushed it — the signature stays bound to the content even if the tag is later moved. |
| `ghcr.io/openweft/weft:v<X.Y.Z>` | `cosign verify` | The multi-arch index digest (resolved from `buildx imagetools inspect`) was signed by the same workflow. Pulling `:v<X.Y.Z>` and verifying succeeds for every platform under the index. |

`SHA256SUMS` (also attached to the Release) is **not** signed on its own;
each tarball has its own `.sig` + `.cert` pair. See *Trust policy* below
for why you should still match SHA256SUMS in addition to the signature.

## The identity contract

The Sigstore certificate baked into every `.cert` (and into every OCI
signature object) carries two values you pin against:

- **Issuer (`--certificate-oidc-issuer`)**:
  `https://token.actions.githubusercontent.com`
- **Subject (`--certificate-identity-regexp`)**:
  `https://github.com/openweft/weft/.github/workflows/release.yml@refs/tags/v<X.Y.Z>`

The regexp form (`v[0-9]+\.[0-9]+\.[0-9]+`) lets a single verifier accept
any tagged release without changing the policy each version. If you want
to pin to one specific release, swap `--certificate-identity-regexp` for
`--certificate-identity` and inline the full URL with the literal tag.

**Why pin both, not just "any GitHub Actions identity"** — Sigstore's
Fulcio will issue a code-signing cert to anyone who can prove a GitHub
Actions OIDC claim. Without an identity pin, an attacker who runs a
workflow named `release.yml` in *their own* repo can produce signatures
that chain to the same Fulcio root. The issuer + subject pair is what
makes the signature say "this came from the openweft/weft repo's release
workflow at this tag", not just "some GitHub Action signed something".

> **Status of v0.1.0** — at the time this doc was written, tag `v0.1.0`
> was pushed but the `release` workflow had not yet been dispatched, so
> the GitHub Release and `ghcr.io/openweft/weft:v0.1.0` are not yet
> populated. The identity strings above are what the workflow at commit
> `3b7593594` *will* emit; once the run completes, the regexp matches
> as-is.

## Tarball verification — step by step

From <https://github.com/openweft/weft/releases/tag/v0.1.0>, download the
tarball for your platform plus its `.sig` + `.cert` sidecars and the
`SHA256SUMS` file into one directory, then:

```sh
# 1. provenance
cosign verify-blob \
  --certificate      weft-linux-amd64_v0.1.0.tar.gz.cert \
  --signature        weft-linux-amd64_v0.1.0.tar.gz.sig \
  --certificate-identity-regexp '^https://github\.com/openweft/weft/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' \
  --certificate-oidc-issuer     'https://token.actions.githubusercontent.com' \
  weft-linux-amd64_v0.1.0.tar.gz
# 2. bit integrity
sha256sum -c --ignore-missing SHA256SUMS
# 3. install
tar -xzf weft-linux-amd64_v0.1.0.tar.gz && install -m 0755 weft /usr/local/bin/weft
```

`cosign verify-blob` prints `Verified OK` and exits 0 on success.

## OCI image verification

The image signature lives in the registry alongside the manifest (Sigstore
`.sig` tag derived from the manifest digest), so there is nothing to
download by hand:

```sh
cosign verify ghcr.io/openweft/weft:v0.1.0 \
  --certificate-identity-regexp '^https://github\.com/openweft/weft/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' \
  --certificate-oidc-issuer     'https://token.actions.githubusercontent.com'
```

Because the workflow signs **by digest** right after push (and again on
the multi-arch index), verification stays valid even if `:v0.1.0` or
`:latest` are later re-pointed. For maximum strictness, resolve the
digest yourself first and verify the pinned form:

```sh
digest=$(crane digest ghcr.io/openweft/weft:v0.1.0)
cosign verify "ghcr.io/openweft/weft@${digest}" \
  --certificate-identity-regexp '^https://github\.com/openweft/weft/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' \
  --certificate-oidc-issuer     'https://token.actions.githubusercontent.com'
```

## In downstream CI pipelines

If your own GitHub Actions pipeline consumes `weft` (e.g. to render a
manifest with `weft up --plan` against your `cluster.hcl`), gate the
download on a signature check:

```yaml
- uses: sigstore/cosign-installer@v3
- name: verify weft image
  run: |
    cosign verify ghcr.io/openweft/weft:v0.1.0 \
      --certificate-identity-regexp '^https://github\.com/openweft/weft/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' \
      --certificate-oidc-issuer     'https://token.actions.githubusercontent.com'
```

For Kubernetes admission, the same identity pair plugs straight into a
[policy-controller](https://docs.sigstore.dev/policy-controller/overview/)
`ClusterImagePolicy` — match `ghcr.io/openweft/weft**` against the
keyless identity above.

## Trust policy — what a strict consumer should do

Require **both** of:

1. **Provenance** — `cosign verify` / `cosign verify-blob` against the
   pinned identity above passes. This proves the artifact was produced
   by *this repo's release workflow* at a *real version tag*.
2. **Bit integrity** — for tarballs, `sha256sum -c SHA256SUMS` matches.
   For images, you resolved a `@sha256:...` digest yourself and pinned
   to it in your deployment manifests.

The two checks are complementary: Sigstore proves *who* produced the
bytes, SHA256 proves the bytes you have are the ones you decided to
trust. Skipping the SHA pin on images means a future `:v0.1.0` retag
could redirect you to a *differently-signed-but-still-valid* artifact;
skipping the cosign check means a SHA you copy-pasted off a phishing
page silently becomes ground truth.

## What this doesn't cover (yet)

- **SBOM** — weft does not yet publish a CycloneDX or SPDX SBOM
  alongside releases. Follow-up: extend `release.yml` to run
  `syft packages` against the build output and attach (and sign) the
  result. Until then, `go version -m` on the verified binary is the
  closest stand-in.
- **Reproducible builds** — `release.yml` builds with
  `go build -trimpath -ldflags "-s -w -X main.version=…"` (most of the
  way there), but we make no public reproducibility claim — no
  rebuilder, no second-party attestations. Two independent rebuilds of
  the same tag with the `go.mod`-pinned toolchain *should* match
  byte-for-byte; a formal claim is a follow-up.
