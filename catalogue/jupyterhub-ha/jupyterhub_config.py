"""JupyterHub config for the weft jupyterhub-ha catalogue plugin.

This file is mounted into each controller VM at
``/etc/jupyterhub/jupyterhub_config.py``. Token substitution
($-prefixed) is done by the weft deployer (see
``infra/configfile.go``) before the file lands on the guest ;
runtime env vars are read directly at Hub startup.

The four substitution points operators care about:

  * ``$OIDC_ISSUER``       — plugin input ``oidc_issuer``
  * ``$OIDC_CLIENT_ID``    — plugin input ``oidc_client_id``
  * ``$OIDC_CLIENT_SECRET``— plugin input ``oidc_client_secret``
  * ``$DOMAIN``            — plugin input ``domain``

The remaining knobs (image, cpu/mem, idle_minutes, db_backend,
project_uuid, …) are read from environment variables the
deployer sets when launching the Hub container — that way a
single rendered config file is reused across all 3 controllers
and only the env differs between them.
"""

# ---------------------------------------------------------------
# Custom spawner registration
# ---------------------------------------------------------------
#
# The Dockerfile copies catalogue/jupyterhub-ha/spawner/ to
# /opt/weft-spawner/ and adds it to PYTHONPATH. We import by
# string so this config file stays parseable without the spawner
# package installed (the CI gate uses py_compile, which doesn't
# follow imports).

import os
import sys

# Inject the spawner path before resolving the class name.
sys.path.insert(0, "/opt/weft-spawner")

c = get_config()  # type: ignore[name-defined]  # provided by JupyterHub at runtime
c.JupyterHub.spawner_class = "weft_spawner.WeftSpawner"


# ---------------------------------------------------------------
# Spawner — per-user microVM knobs
# ---------------------------------------------------------------

c.WeftSpawner.weft_binary = os.environ.get("WEFT_BINARY", "weft")
c.WeftSpawner.weft_socket = os.environ.get("WEFT_SOCKET", "/run/weft/weft.sock")
c.WeftSpawner.project_uuid = os.environ.get("WEFT_PROJECT_UUID", "")
c.WeftSpawner.image = os.environ.get(
    "JUPYTER_USER_IMAGE",
    "quay.io/jupyter/minimal-notebook:python-3.12",
)
c.WeftSpawner.cpu_per_user = int(os.environ.get("CPU_PER_USER", "2"))
c.WeftSpawner.memory_gib_per_user = int(os.environ.get("MEMORY_GIB_PER_USER", "4"))
c.WeftSpawner.home_volume_gib = int(os.environ.get("HOME_VOLUME_GIB", "50"))
c.WeftSpawner.network_name = os.environ.get("USER_VM_NETWORK", "jupyterhub-control")
c.WeftSpawner.security_group = os.environ.get("USER_VM_SG", "user-notebook")

# Spawn timeout: cold-pull of the user image can easily run a
# minute on first launch. After the image is cached locally on
# every host, ~10 s is realistic.
c.Spawner.start_timeout = 300
c.Spawner.http_timeout = 60


# ---------------------------------------------------------------
# Idle culler — stop (not destroy) VMs after N minutes of idle
# ---------------------------------------------------------------
#
# JupyterHub ships a `jupyterhub-idle-culler` service that calls
# the Hub API to delete idle servers. Our spawner's `stop()`
# preserves the home volume, so a "culled" VM is just a stopped
# VM ; the next login restarts it with all user state intact.

_idle_minutes = int(os.environ.get("IDLE_MINUTES", "60"))
c.JupyterHub.services = [
    {
        "name": "idle-culler",
        "admin": True,
        "command": [
            sys.executable,
            "-m",
            "jupyterhub_idle_culler",
            f"--timeout={_idle_minutes * 60}",
            # Cull every 5 min ; cheap because Spawner.poll() is
            # one CLI call per user.
            "--cull-every=300",
            # IMPORTANT : do NOT pass --remove-named-servers.
            # We want stop, not delete — the home volume must
            # persist across cull cycles.
        ],
    },
]


# ---------------------------------------------------------------
# OIDC authenticator — same issuer weft itself trusts
# ---------------------------------------------------------------

c.JupyterHub.authenticator_class = "oauthenticator.generic.GenericOAuthenticator"

_issuer = os.environ.get("OIDC_ISSUER", "$OIDC_ISSUER")
_client_id = os.environ.get("OIDC_CLIENT_ID", "$OIDC_CLIENT_ID")
_client_secret = os.environ.get("OIDC_CLIENT_SECRET", "$OIDC_CLIENT_SECRET")
_domain = os.environ.get("DOMAIN", "$DOMAIN")

c.GenericOAuthenticator.client_id = _client_id
c.GenericOAuthenticator.client_secret = _client_secret
c.GenericOAuthenticator.oauth_callback_url = f"https://{_domain}/hub/oauth_callback"
c.GenericOAuthenticator.authorize_url = f"{_issuer}/auth"
c.GenericOAuthenticator.token_url = f"{_issuer}/token"
c.GenericOAuthenticator.userdata_url = f"{_issuer}/userinfo"
c.GenericOAuthenticator.username_claim = "preferred_username"
c.GenericOAuthenticator.scope = ["openid", "email", "groups", "profile"]
c.GenericOAuthenticator.claim_groups_key = "groups"

# Group-based access : the operator passes the project + admin
# group names via env. Convention (per docs/operations/rbac.md)
# is `weft:project:<uuid>` for users and `weft:admin` for the
# cluster admin group.
_admin_group = os.environ.get("ADMIN_GROUP", "weft:admin")
_user_group = os.environ.get("USER_GROUP", "")  # falls back to allow-any-authenticated below

c.GenericOAuthenticator.admin_groups = {_admin_group}
if _user_group:
    c.GenericOAuthenticator.allowed_groups = {_user_group}
# else : every authenticated user passes the OIDC check. The
# operator can still restrict per-Hub access via the project's
# OAuth client configuration on the issuer side.


# ---------------------------------------------------------------
# DB backend — Cockroach (default) or SQLite-on-NFS (small deploys)
# ---------------------------------------------------------------

_db_backend = os.environ.get("DB_BACKEND", "cockroach")

if _db_backend == "cockroach":
    # cockroachdb:// is parsed by SQLAlchemy via the
    # `sqlalchemy-cockroachdb` dialect. JupyterHub's schema is
    # small (users, servers, tokens, oauth state) so a single
    # 3-node Cockroach cluster handles thousands of users.
    _db_user = os.environ.get("DB_USER", "jupyterhub")
    _db_password = os.environ.get("DB_PASSWORD", "")
    _db_hosts = os.environ.get(
        "DB_HOSTS",
        "10.255.42.20:26257,10.255.42.21:26257,10.255.42.22:26257",
    )
    _db_name = os.environ.get("DB_NAME", "jupyterhub")
    c.JupyterHub.db_url = (
        f"cockroachdb://{_db_user}:{_db_password}@{_db_hosts}/{_db_name}"
        "?sslmode=verify-full"
    )
elif _db_backend == "sqlite-nfs":
    # Single shared NFS mount at /var/lib/jupyterhub. WAL mode
    # tolerates concurrent readers ; we still keep only ONE
    # controller writing — JupyterHub picks a leader via the
    # Caddy sticky-session cookie + a small file lock.
    c.JupyterHub.db_url = (
        "sqlite:////var/lib/jupyterhub/jupyterhub.sqlite"
        "?check_same_thread=False"
    )
else:
    raise ValueError(
        f"unknown DB_BACKEND {_db_backend!r} ; want cockroach | sqlite-nfs"
    )


# ---------------------------------------------------------------
# Network / proxy
# ---------------------------------------------------------------

# The Hub listens on 8000 ; Caddy fronts it (see plugin.hcl
# proxy_route "hub"). We bind to all interfaces so the proxy
# can reach us from any DC.
c.JupyterHub.bind_url = "http://0.0.0.0:8000"
c.JupyterHub.hub_bind_url = "http://0.0.0.0:8081"
# hub_connect_ip is the IP user notebooks dial back to ; in our
# topology that's the controller's static_ip in the
# jupyterhub-control network.
c.JupyterHub.hub_connect_ip = os.environ.get("HUB_CONNECT_IP", "")


# ---------------------------------------------------------------
# Logging
# ---------------------------------------------------------------

c.JupyterHub.log_level = os.environ.get("LOG_LEVEL", "INFO")
# Structured JSON to stdout so weft's log shipper just consumes it.
c.Application.log_format = (
    '{"ts":"%(asctime)s","level":"%(levelname)s","logger":"%(name)s","msg":"%(message)s"}'
)
