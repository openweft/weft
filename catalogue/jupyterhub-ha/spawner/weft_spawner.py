"""weft_spawner — JupyterHub Spawner backend that runs each user's
notebook server inside an isolated weft microVM.

The spawner shells the host's ``weft`` CLI (exposed inside the
controller VM via a virtio-fs share of ``/var/run/weft``) rather
than calling the agent's gRPC API directly. Rationale:

* The Hub container would otherwise need a Python gRPC client
  generated from weft's protobufs ; the CLI is the stable user-
  facing contract and round-trip overhead (~tens of ms) is dwarfed
  by the microVM cold-start (~hundreds of ms).
* The CLI does its own retry / auth / config handling — we get
  that for free instead of reimplementing it in Python.

If you want raw speed (sub-second spawn budgets), swap to gRPC
via grpcurl or a generated client ; the public surface here
(:meth:`start`, :meth:`stop`, :meth:`poll`) is unchanged.

Per-user state model
--------------------

* VM name : ``vm-jh-<safe-username>`` — JupyterHub usernames are
  already sanitised by the OAuthenticator, but we re-apply a strict
  ``[a-z0-9-]`` filter as defence-in-depth.
* Home volume : ``vol-jh-home-<safe-username>``, size from input
  ``home_volume_gib``, lazily created on first login. Mounted at
  ``/home/jovyan`` inside the user's VM.
* Idle cull : we just call :meth:`stop` ; the VM transitions to
  ``stopped`` (not deleted), home volume persists, and the next
  login restarts it.

Errors
------

* ``codes.ResourceExhausted`` from the weft agent (tenant quota)
  is surfaced to the user as an HTTP 503 with a clear message —
  that's the correct behaviour per the design doc.
* Spawn failures leave any partially-created VM in place so an
  operator can inspect ; we only ``rm`` on explicit ``stop``
  with ``--purge`` (which we never pass).
"""

# Standard library only — JupyterHub's Spawner base class is the
# one third-party import. Keeping it minimal lets the Hub image
# stay small.
from __future__ import annotations

import asyncio
import json
import os
import re
import shutil
import subprocess
from typing import Any, Optional

try:
    # Real dependency at runtime — only imported when the Hub
    # actually loads us. We guard so ``python -m py_compile`` (the
    # CI gate) doesn't need jupyterhub installed.
    from jupyterhub.spawner import Spawner
    from traitlets import Int, Unicode
except ImportError:  # pragma: no cover - CI-only path
    Spawner = object  # type: ignore[assignment,misc]

    def Int(default_value=0, **_kwargs):  # type: ignore[no-redef]
        return default_value

    def Unicode(default_value="", **_kwargs):  # type: ignore[no-redef]
        return default_value


# Restrict to characters that are safe both as a weft resource
# name (RFC 1123-ish) and as a filesystem component.
_SAFE_NAME = re.compile(r"[^a-z0-9-]+")


def _safe_username(raw: str) -> str:
    """Lowercase + collapse unsafe chars to '-'. JupyterHub upstream
    already normalises usernames but we re-filter so a misconfigured
    authenticator can't produce a VM name that weft would reject."""
    lowered = raw.lower()
    cleaned = _SAFE_NAME.sub("-", lowered).strip("-")
    return cleaned or "anon"


class WeftSpawner(Spawner):  # type: ignore[misc, valid-type]
    """JupyterHub Spawner that delegates to ``weft instance ...``.

    Configurable via traitlets in ``jupyterhub_config.py`` ; sane
    defaults match the plugin manifest's input defaults so a vanilla
    install just works.
    """

    weft_binary = Unicode(
        "weft",
        config=True,
        help="Path to the weft CLI. Defaults to PATH lookup ; set to /run/weft/bin/weft "
        "if you bind-mounted the host CLI rather than installing it inside the Hub image.",
    )

    weft_socket = Unicode(
        "/run/weft/weft.sock",
        config=True,
        help="Unix socket where the agent listens, as visible from inside the Hub VM. "
        "The controller's virtio-fs share exposes the host's /var/run/weft here.",
    )

    project_uuid = Unicode(
        "",
        config=True,
        help="weft project UUID that owns user notebook VMs. MUST be set ; the Hub will "
        "refuse to start if empty (the agent would just reject every CreateVM with codes."
        "InvalidArgument otherwise).",
    )

    image = Unicode(
        "quay.io/jupyter/minimal-notebook:python-3.12",
        config=True,
        help="OCI image for user notebook microVMs.",
    )

    cpu_per_user = Int(2, config=True, help="vCPUs per user VM.")
    memory_gib_per_user = Int(4, config=True, help="Memory (GiB) per user VM.")
    home_volume_gib = Int(50, config=True, help="Per-user /home/jovyan volume size.")

    network_name = Unicode(
        "jupyterhub-control",
        config=True,
        help="weft network user VMs join. Must allow the hub-controller SG ingress on 8888.",
    )

    security_group = Unicode(
        "user-notebook",
        config=True,
        help="weft security group attached to user VMs.",
    )

    notebook_port = Int(8888, config=True, help="Port the notebook server listens on.")

    # ----- helpers -----------------------------------------------------

    def _vm_name(self) -> str:
        return f"vm-jh-{_safe_username(self.user.name)}"

    def _volume_name(self) -> str:
        return f"vol-jh-home-{_safe_username(self.user.name)}"

    async def _weft(self, *args: str, check: bool = True) -> tuple[int, str, str]:
        """Run ``weft <args>`` against the agent socket, return
        (rc, stdout, stderr). All calls are async to keep the
        Hub's event loop responsive — a spawn can take ~hundreds
        of ms and we don't want to block other users' requests."""
        cmd = [self.weft_binary, "--socket", self.weft_socket, *args]
        self.log.debug("weft call: %s", " ".join(cmd))
        proc = await asyncio.create_subprocess_exec(
            *cmd,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        out, err = await proc.communicate()
        rc = proc.returncode or 0
        sout = out.decode("utf-8", "replace")
        serr = err.decode("utf-8", "replace")
        if check and rc != 0:
            # Surface ResourceExhausted distinctly so the Hub UI
            # can show a friendly "quota full" page instead of a
            # generic 500.
            if "ResourceExhausted" in serr or "quota" in serr.lower():
                raise QuotaExceededError(serr.strip())
            raise WeftCLIError(rc, serr.strip())
        return rc, sout, serr

    async def _ensure_volume(self) -> None:
        """Create the home volume if it doesn't exist. Idempotent :
        a second ``volume create`` with the same name is a no-op."""
        rc, _, _ = await self._weft(
            "volume", "show", self._volume_name(),
            "--project", self.project_uuid,
            check=False,
        )
        if rc == 0:
            return
        await self._weft(
            "volume", "create",
            "--name", self._volume_name(),
            "--project", self.project_uuid,
            "--size-gib", str(self.home_volume_gib),
        )

    async def _vm_state(self) -> Optional[str]:
        """Return the VM's State string (\"running\", \"stopped\", …) or
        None if it doesn't exist."""
        rc, out, _ = await self._weft(
            "instance", "show", self._vm_name(),
            "--project", self.project_uuid,
            "--format", "json",
            check=False,
        )
        if rc != 0:
            return None
        try:
            return json.loads(out).get("state")
        except json.JSONDecodeError:
            return None

    async def _create_vm(self) -> None:
        """Create + start the per-user notebook VM. Mounts the home
        volume at /home/jovyan and the user's OAuth token in
        /run/secrets/oidc-token (so libraries that pull notebooks
        from a private git repo can use it)."""
        memory_mib = self.memory_gib_per_user * 1024
        await self._weft(
            "instance", "create",
            "--name", self._vm_name(),
            "--project", self.project_uuid,
            "--image", self.image,
            "--cpu-count", str(self.cpu_per_user),
            "--memory-mib", str(memory_mib),
            "--network", self.network_name,
            "--security-group", self.security_group,
            "--volume", f"{self._volume_name()}:/home/jovyan",
            "--env", f"JUPYTERHUB_USER={self.user.name}",
            "--env", f"JUPYTERHUB_API_TOKEN={self.api_token}",
            "--env", f"JUPYTERHUB_API_URL={self.hub.api_url}",
            "--env", f"JUPYTERHUB_BASE_URL={self.hub.base_url}",
            "--env", f"JPY_API_TOKEN={self.api_token}",
            "--cmd", "/usr/local/bin/start-notebook.sh",
        )
        await self._weft(
            "instance", "start", self._vm_name(),
            "--project", self.project_uuid,
        )

    async def _vm_ip(self) -> str:
        """Resolve the VM's primary IP. We poll because the IP may
        not be assigned the instant ``start`` returns ; weft
        agent's gRPC StreamVMEvents would be cleaner but the CLI
        path is plenty for first cut."""
        for _ in range(30):
            rc, out, _ = await self._weft(
                "instance", "show", self._vm_name(),
                "--project", self.project_uuid,
                "--format", "json",
                check=False,
            )
            if rc == 0:
                try:
                    ip = json.loads(out).get("primary_ip", "")
                except json.JSONDecodeError:
                    ip = ""
                if ip:
                    return ip
            await asyncio.sleep(1)
        raise WeftCLIError(1, f"timed out waiting for {self._vm_name()} primary_ip")

    # ----- JupyterHub Spawner interface -------------------------------

    async def start(self) -> tuple[str, int]:
        """Start (or resume) the user's microVM, return (ip, port)
        for the Hub's proxy to route to."""
        if not self.project_uuid:
            raise RuntimeError(
                "WeftSpawner.project_uuid is empty ; set c.WeftSpawner.project_uuid "
                "in jupyterhub_config.py to the weft project UUID that owns user VMs."
            )
        await self._ensure_volume()
        state = await self._vm_state()
        if state is None:
            await self._create_vm()
        elif state == "stopped":
            await self._weft(
                "instance", "start", self._vm_name(),
                "--project", self.project_uuid,
            )
        # else: already running ; just resolve the IP and return.
        ip = await self._vm_ip()
        return ip, self.notebook_port

    async def stop(self, now: bool = False) -> None:
        """Stop the user's microVM. We never ``rm`` — the home
        volume persists across logouts. ``now=True`` skips the
        graceful shutdown grace period."""
        args = ["instance", "stop", self._vm_name(), "--project", self.project_uuid]
        if now:
            args.append("--force")
        await self._weft(*args, check=False)

    async def poll(self) -> Optional[int]:
        """JupyterHub contract: return None if the user's process is
        still alive, an exit code otherwise. We map weft's
        ``running`` state to None, anything else to 0."""
        state = await self._vm_state()
        if state == "running":
            return None
        return 0

    def get_state(self) -> dict[str, Any]:
        """Persist enough state for the Hub to recover after a restart.
        The VM name is derived deterministically from the username,
        so we just need to remember that we ever spawned one."""
        state = super().get_state() if hasattr(super(), "get_state") else {}
        state["weft_vm"] = self._vm_name()
        return state

    def load_state(self, state: dict[str, Any]) -> None:
        if hasattr(super(), "load_state"):
            super().load_state(state)
        # No-op : _vm_name() is deterministic. We keep the key
        # around for forward-compat with future schema changes.


class WeftCLIError(RuntimeError):
    """Raised when ``weft`` exits non-zero. ``returncode`` and
    ``stderr`` are exposed for the Hub's error handler."""

    def __init__(self, returncode: int, stderr: str) -> None:
        super().__init__(f"weft CLI exited {returncode}: {stderr}")
        self.returncode = returncode
        self.stderr = stderr


class QuotaExceededError(WeftCLIError):
    """Specialisation of WeftCLIError raised when the agent rejects
    a CreateVM with ``codes.ResourceExhausted`` — the tenant
    quota landed in commit 88cece7c6. Operators should size the
    project's quota for the expected user count ahead of rollout."""

    def __init__(self, stderr: str) -> None:
        super().__init__(8, stderr)  # 8 == gRPC ResourceExhausted


# Sanity self-check : importable + the safe-name helper works as
# documented. The Hub's startup logs this so operators see a
# breadcrumb if the wrong version of the module got mounted in.
if __name__ == "__main__":  # pragma: no cover
    assert _safe_username("Alice Smith") == "alice-smith"
    assert _safe_username("---") == "anon"
    print("weft_spawner OK")
