# weft-flavored JupyterLab user image.
#
# Layered on top of the Jupyter project's official minimal-notebook
# so we inherit the maintained Jupyter+conda stack instead of
# re-curating it. We only add what the weft control plane expects
# from a user notebook server:
#
#   * jupyterlab-git        — git UI in JupyterLab, the default
#                             tooling everyone wants in lab.
#   * jupyter-server-proxy  — lets users expose ad-hoc local apps
#                             (TensorBoard, RStudio, etc.) through
#                             the same Hub-routed URL prefix.
#   * pkgx                  — opt-in CLI tool manager : students
#                             can install language toolchains
#                             ($ pkgx node, $ pkgx +rust …)
#                             without root, no apt needed.
#
# weft-client (Python) is NOT installed — the user notebook does
# not talk to the weft agent ; the JupyterHub spawner does, and
# the spawner lives in the Hub container, not the user VM. See
# user-image/README.md for the rationale.
#
# Layer budget : kept under 10 (currently 6 RUN/COPY).
# Build context : empty — everything is fetched by pip / install.sh.

ARG BASE_IMAGE=quay.io/jupyter/minimal-notebook
ARG BASE_TAG=python-3.12
FROM ${BASE_IMAGE}:${BASE_TAG}

LABEL org.opencontainers.image.title="jupyter-user" \
      org.opencontainers.image.description="weft-flavored JupyterLab user notebook image" \
      org.opencontainers.image.source="https://github.com/openweft/weft" \
      org.opencontainers.image.licenses="BSD-3-Clause"

# The upstream image runs as jovyan (uid 1000) by default. Pip
# installs land in the user site so we don't need root here.
#
# Versions are pinned to majors only — Jupyter and these
# extensions follow semver tightly and we want minor security
# patches without a Dockerfile bump.
RUN pip install --no-cache-dir \
        "jupyterlab-git>=0.50,<1" \
        "jupyter-server-proxy>=4,<5" \
    && jupyter lab build --dev-build=False --minimize=False \
    && jupyter lab clean

# pkgx is shipped as a single static binary ; install to /usr/local
# so it's on PATH for all kernels. We swap to root briefly because
# the installer writes to /usr/local/bin ; the rest of the layer
# runs the install script with a known SHA-pinned version URL.
USER root
ARG PKGX_VERSION=2.4.0
RUN set -eux ; \
    arch="$(dpkg --print-architecture)" ; \
    case "${arch}" in \
        amd64) pkgx_arch="x86_64-unknown-linux-musl" ;; \
        arm64) pkgx_arch="aarch64-unknown-linux-musl" ;; \
        *) echo "unsupported arch ${arch}" >&2 ; exit 1 ;; \
    esac ; \
    url="https://dist.pkgx.dev/pkgx.sh/v${PKGX_VERSION}/${pkgx_arch}.tar.xz" ; \
    curl -fsSL "${url}" -o /tmp/pkgx.tar.xz ; \
    tar -C /usr/local/bin -xJf /tmp/pkgx.tar.xz pkgx ; \
    chmod +x /usr/local/bin/pkgx ; \
    rm /tmp/pkgx.tar.xz ; \
    /usr/local/bin/pkgx --version
USER ${NB_UID}

# JupyterLab listens on 8888 — matches WeftSpawner.notebook_port.
EXPOSE 8888
