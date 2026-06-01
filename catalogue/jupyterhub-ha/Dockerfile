# JupyterHub controller image for the weft jupyterhub-ha plugin.
#
# Layers :
#   1. upstream jupyterhub/jupyterhub base (Python + Hub installed)
#   2. OAuthenticator + idle-culler + cockroach driver
#   3. our custom spawner module dropped at /opt/weft-spawner/
#   4. rendered jupyterhub_config.py at /etc/jupyterhub/
#
# Build :   make image  (or: docker build -t openweft/jupyterhub-ha:0.1.0 .)
# Publish : pushed by the openweft/jupyterhub-ha repo's CI on a
#           tagged release ; see docs/catalogue/jupyterhub-ha.md.

# Pin a specific Hub minor — JupyterHub follows semver loosely
# and Spawner API changes have happened at minor bumps before.
FROM jupyterhub/jupyterhub:5.2

# Python deps :
#   * oauthenticator       — OIDC login flow
#   * jupyterhub-idle-culler — calls /hub/api/users/.../servers to stop idle servers
#   * sqlalchemy-cockroachdb — CockroachDB dialect (no-op when db_backend=sqlite-nfs)
#
# Pinning is loose on purpose — JupyterHub is the one component
# whose minor bumps actually matter ; the rest are stable.
RUN pip install --no-cache-dir \
        "oauthenticator==17.*" \
        "jupyterhub-idle-culler==1.*" \
        "sqlalchemy-cockroachdb==2.*"

# Custom spawner. Mounted at /opt/weft-spawner so jupyterhub_config.py
# can prepend it to sys.path without polluting the system site-
# packages — keeps upgrades clean.
COPY spawner/weft_spawner.py /opt/weft-spawner/weft_spawner.py

# Hub config. The deployer's config_file{} block can override
# this with a fresh render, but the image ships a sensible default
# so `docker run` works for smoke-testing without weft involvement.
COPY jupyterhub_config.py /etc/jupyterhub/jupyterhub_config.py

# Where the agent's unix socket lands once the virtio-fs share
# from the host mounts. We create the directory empty so the
# share has a mountpoint ; the actual socket file appears at
# boot when the share comes online.
RUN mkdir -p /run/weft

# Hub HTTP + hub-internal API.
EXPOSE 8000 8081

# The base image's ENTRYPOINT already runs jupyterhub ; we just
# point it at the config we just baked in.
CMD ["jupyterhub", "-f", "/etc/jupyterhub/jupyterhub_config.py"]
