"""``E2BSandboxProvider`` — real sandboxes on E2B.

Thin adapter from the :class:`Sandbox` / :class:`SandboxProvider`
protocols onto the open-source `e2b` async SDK. The engine layers the
brief, result parsing, budget caps, OTel injection, and the detached
lifecycle on top; E2B supplies the substrate (a real VM with the
coding-agent CLI preinstalled in its ``claude`` / ``opencode`` templates).

The ``e2b`` SDK is imported lazily inside ``create`` / ``connect`` so this
module (and the engine) import fine without the optional dependency; a
missing SDK surfaces as a clear error only when an E2B sandbox is actually
provisioned. ``domain`` (env ``E2B_DOMAIN``) is the cloud↔self-hosted
switch — unset → ``e2b.dev`` cloud; set → a self-hosted / local cluster.
"""

from __future__ import annotations

from typing import Any

from crewlet._logging import get_logger
from crewlet.sandbox.protocol import ExecResult, Sandbox, SandboxSpec

logger = get_logger("sandbox.e2b")

# E2B's prebuilt coding-agent templates, keyed by our coding_agent name.
_DEFAULT_TEMPLATE = {"claude-code": "claude", "opencode": "opencode"}


def _require_e2b() -> Any:
    """Import the ``e2b`` async SDK or raise an actionable error."""
    try:
        from e2b import AsyncSandbox
    except ImportError as exc:  # pragma: no cover - depends on optional dep
        raise RuntimeError(
            "providers.sandbox.type 'e2b' requires the 'e2b' package — "
            "install the sandbox extra (`pip install 'crewlet[sandbox]'` or, "
            "from a checkout, `uv sync --extra sandbox`). Use type 'fake' for "
            "local testing."
        ) from exc
    return AsyncSandbox


class E2BSandbox:
    """A live E2B sandbox behind the :class:`Sandbox` protocol."""

    def __init__(self, sbx: Any) -> None:
        self._sbx = sbx

    @property
    def id(self) -> str:
        return self._sbx.sandbox_id

    async def exec(
        self,
        cmd: str,
        *,
        env: dict[str, str] | None = None,
        cwd: str = "",
        timeout_s: float = 0.0,
    ) -> ExecResult:
        # The SDK RAISES CommandExitException on any non-zero exit; the
        # Sandbox protocol reports the exit code in the result instead
        # (callers depend on it: the `kill -0` liveness probe reads exit 1
        # as "process gone", and apply_setup turns a failed provisioning
        # command into SandboxSetupError). Map the exception back to an
        # ExecResult so non-zero exits are data, not control flow.
        from e2b import CommandExitException

        try:
            res = await self._sbx.commands.run(
                cmd,
                envs=env or {},
                cwd=cwd or None,
                timeout=int(timeout_s) or None,
            )
        except CommandExitException as exc:
            return ExecResult(
                exit_code=int(getattr(exc, "exit_code", 1) or 1),
                stdout=str(getattr(exc, "stdout", "") or ""),
                stderr=str(getattr(exc, "stderr", "") or ""),
            )
        return ExecResult(
            exit_code=int(getattr(res, "exit_code", 0) or 0),
            stdout=str(getattr(res, "stdout", "") or ""),
            stderr=str(getattr(res, "stderr", "") or ""),
        )

    async def start_background(
        self,
        cmd: str,
        *,
        env: dict[str, str] | None = None,
        cwd: str = "",
    ) -> str:
        handle = await self._sbx.commands.run(
            cmd, envs=env or {}, cwd=cwd or None, background=True
        )
        return str(getattr(handle, "pid", "") or "")

    async def write_file(self, path: str, content: bytes | str) -> None:
        await self._sbx.files.write(path, content)

    async def read_file(self, path: str) -> bytes:
        # Empty-on-missing: the detached runner polls for marker / result
        # files that don't exist until the job finishes.
        try:
            data = await self._sbx.files.read(path, format="bytes")
        except Exception:
            return b""
        if isinstance(data, str):
            return data.encode()
        return bytes(data)

    async def set_timeout(self, seconds: float) -> None:
        # Reset the box's kill timer to ``seconds`` from now. The waiter calls
        # this each tick to keep a running job's box alive (no run-time TTL).
        set_timeout = getattr(self._sbx, "set_timeout", None)
        if set_timeout is not None and seconds > 0:
            await set_timeout(int(seconds))

    async def pause(self) -> None:
        # E2B keeps a paused sandbox indefinitely (no provider-side TTL) and
        # bills for the snapshot, which is why reaping is the engine's job —
        # see SandboxWaiter's pause reaper. ``connect`` auto-resumes.
        #
        # ``pause`` is the current name; ``beta_pause`` is its deprecated
        # alias. Prefer the stable one and fall back, rather than probing
        # only for the deprecated name: when that alias is eventually
        # removed, a getattr probe silently turns pausing into a no-op and
        # every "paused" box keeps running (and billing) until its TTL.
        pause = getattr(self._sbx, "pause", None) or getattr(
            self._sbx, "beta_pause", None
        )
        if pause is None:  # pragma: no cover - provider without snapshots
            logger.warning("e2b_pause_unsupported", sandbox_id=self.id)
            return
        await pause()

    async def close(self) -> None:
        await self._sbx.kill()


class E2BSandboxProvider:
    """Mints :class:`E2BSandbox` handles on E2B cloud or a self-hosted cluster."""

    kind = "e2b"

    def __init__(
        self, *, api_key: str = "", domain: str = "", template: str = ""
    ) -> None:
        self._api_key = api_key
        self._domain = domain
        self._template = template

    def _template_for(self, spec: SandboxSpec) -> str:
        return (
            spec.template
            or self._template
            or _DEFAULT_TEMPLATE.get(spec.coding_agent, "base")
        )

    def _kwargs(self) -> dict[str, Any]:
        kw: dict[str, Any] = {}
        if self._api_key:
            kw["api_key"] = self._api_key
        if self._domain:
            kw["domain"] = self._domain
        return kw

    async def create(self, spec: SandboxSpec) -> Sandbox:
        async_sandbox = _require_e2b()
        template = self._template_for(spec)
        sbx = await async_sandbox.create(
            template=template,
            timeout=int(spec.timeout_s) or None,
            envs=dict(spec.env),
            **self._kwargs(),
        )
        logger.info(
            "e2b_sandbox_created",
            sandbox_id=sbx.sandbox_id,
            template=template,
            domain=self._domain or "e2b.dev",
        )
        return E2BSandbox(sbx)

    async def connect(self, sandbox_id: str) -> Sandbox:
        async_sandbox = _require_e2b()
        sbx = await async_sandbox.connect(sandbox_id, **self._kwargs())
        logger.debug("e2b_sandbox_connected", sandbox_id=sandbox_id)
        return E2BSandbox(sbx)

    async def kill(self, sandbox_id: str) -> None:
        # The static ``kill(sandbox_id)`` variant terminates by id over the
        # API — no ``connect`` first, so reclaiming a PAUSED box doesn't
        # resume it (which would boot the VM and bill runtime) just to kill
        # it. Returns False for an already-gone box; that is not an error.
        async_sandbox = _require_e2b()
        killed = await async_sandbox.kill(sandbox_id, **self._kwargs())
        logger.debug("e2b_sandbox_killed", sandbox_id=sandbox_id, killed=bool(killed))


__all__ = ["E2BSandbox", "E2BSandboxProvider"]
