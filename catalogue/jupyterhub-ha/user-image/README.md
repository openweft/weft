# weft jupyter-user image

The default OCI image for user notebook microVMs spawned by the
[jupyterhub-ha catalogue plugin](../../docs/catalogue/jupyterhub-ha.md).

Published as `ghcr.io/openweft/jupyter-user:<tag>` (multi-arch
linux/amd64 + linux/arm64, cosign-signed) by the
`.github/workflows/jupyter-user-image.yml` workflow. Until the
first `v0.1.0` release is published the catalogue plugin's
manifest still defaults to `quay.io/jupyter/minimal-notebook` ;
the flip is tracked under "Post-v0.1.0 follow-ups" in the doc.

## What's in the image

| Layer    | Contents                                                                 |
|----------|--------------------------------------------------------------------------|
| upstream | `quay.io/jupyter/minimal-notebook:python-3.12` — JupyterLab + Python    |
| weft     | `jupyterlab-git`, `jupyter-server-proxy`                                  |
| weft     | `pkgx` static binary at `/usr/local/bin/pkgx`                            |

That's it — six layers including the FROM, well under the
10-layer budget. No team-specific scientific stacks (PyTorch,
CUDA, R, …) ; bake those in a downstream image instead.

## What's NOT in the image

- **`weft-client` (Python).** There is no published Python
  weft-client package today — the [`weft-client`](https://github.com/openweft/weft-client)
  repo is the Go client used by the agent. User notebooks have
  no reason to talk to the weft control plane directly ; the
  spawner does that on their behalf. **Gap to track : if a
  future story needs a Python control-plane SDK, publish one
  to PyPI first, then add it to this Dockerfile.**
- **Cloud-vendor SDKs (boto3, google-cloud-…)**. Pull them in
  your downstream image if you need them ; we don't want every
  user dragging ~150 MB of unused dependencies.

## How to override

Set `--input image=…` when installing the plugin :

```bash
weft plugin install catalogue/jupyterhub-ha \
     --input image=ghcr.io/example-team/jupyter-user-pytorch:2025.10 \
     ...
```

Or post-install, via the env var on each controller VM :

```bash
weft instance env set vm-jupyterhub-controller-1 \
     JUPYTER_USER_IMAGE=ghcr.io/example-team/jupyter-user-pytorch:2025.10
```

## How to build locally

From the repo root :

```bash
docker buildx build \
    --platform linux/amd64,linux/arm64 \
    --tag ghcr.io/openweft/jupyter-user:dev \
    catalogue/jupyterhub-ha/user-image
```

Smoke-test single-arch :

```bash
docker run --rm -p 8888:8888 \
    ghcr.io/openweft/jupyter-user:dev \
    start-notebook.sh --NotebookApp.token=''
# open http://localhost:8888
```

## How this differs from upstream

- Adds `jupyterlab-git` + `jupyter-server-proxy` — most weft
  deployments want both, so we don't make every operator
  re-derive the image.
- Adds `pkgx` so notebooks can install ad-hoc toolchains
  without root (`pkgx node`, `pkgx go`, …) — matches the rest
  of the weft tooling convention.
- License/labels declare BSD-3-Clause + the openweft/weft
  repo as the source.

Anything outside that delta should stay upstream.
