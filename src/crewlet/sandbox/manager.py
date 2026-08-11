"""``SandboxManager`` -- the engine-held sandbox lifecycle.

Unlike ``MCPToolBridge`` (per-role *persistent* server instances),
sandboxes are **per-turn ephemeral**. The manager holds the configured
:class:`SandboxProvider` and the available :class:`CodingAgentRunner`
instances, resolves an effective :class:`SandboxSpec` for a role, and
mints + installs a sandbox on demand. Teardown is the caller's
responsibility (``async with`` / explicit ``sandbox.close()``), so a turn
that crashes still reclaims the sandbox.
"""

from __future__ import annotations

from collections.abc import Sequence

from crewlet._logging import get_logger
from crewlet.sandbox.protocol import (
    CodingAgentRunner,
    Sandbox,
    SandboxProvider,
    SandboxSpec,
)
from crewlet.sandbox.setup import SandboxSetupError, SandboxSetupStep, apply_setup

logger = get_logger("sandbox.manager")


class SandboxConfigError(ValueError):
    """``providers.sandbox`` is misconfigured (unknown / unavailable type)."""


class SandboxManager:
    """Resolves specs and acquires sandboxes for sandbox-enabled roles."""

    def __init__(
        self,
        *,
        provider: SandboxProvider,
        runners: dict[str, CodingAgentRunner],
        default_coding_agent: str = "claude-code",
        default_template: str = "",
        default_timeout_s: float = 900.0,
        default_pause_ttl_s: float = 1800.0,
        default_setup: Sequence[SandboxSetupStep] = (),
    ) -> None:
        self._provider = provider
        self._runners = dict(runners)
        self._default_coding_agent = default_coding_agent
        self._default_template = default_template
        self._default_timeout_s = default_timeout_s
        self._default_pause_ttl_s = default_pause_ttl_s
        self._default_setup = list(default_setup)

    @property
    def provider(self) -> SandboxProvider:
        """The configured sandbox provider (for reconnect-by-id)."""
        return self._provider

    @property
    def provider_kind(self) -> str:
        return self._provider.kind

    @property
    def default_coding_agent(self) -> str:
        """Provider-level default coding agent
        (``providers.sandbox.default_coding_agent``). The effective agent
        when a role leaves ``role.sandbox.coding_agent`` empty (inherit)."""
        return self._default_coding_agent

    @property
    def default_setup(self) -> list[SandboxSetupStep]:
        """Engine-wide setup steps (``providers.sandbox.setup``) applied to
        every sandbox before the role's own ``role.sandbox.setup`` extras.
        The engine ships NO steps of its own — git auth included — so these
        two config lists are the ONLY provisioning a box receives."""
        return list(self._default_setup)

    @property
    def default_timeout_s(self) -> float:
        """The sandbox box's TTL / keepalive window
        (``providers.sandbox.default_timeout_seconds``).

        This is **not** a run-time limit: a coding job is never force-stopped
        on a timer, and the :class:`~crewlet.sandbox.waiter.SandboxWaiter`
        refreshes a running box's TTL to this value every tick so the clock
        never kills it. It is effectively the orphan-reclaim grace — how long
        a box outlives an engine that stops heart-beating before E2B reaps
        it."""
        return self._default_timeout_s

    def runner_for(self, coding_agent: str) -> CodingAgentRunner:
        """Return the runner for ``coding_agent`` or raise ``KeyError``."""
        runner = self._runners.get(coding_agent)
        if runner is None:
            raise KeyError(
                f"no coding-agent runner registered for {coding_agent!r} "
                f"(have: {sorted(self._runners)})"
            )
        return runner

    def build_spec(
        self,
        *,
        coding_agent: str = "",
        template: str = "",
        timeout_s: float = 0.0,
        pause_ttl_s: float = -1.0,
        env: dict[str, str] | None = None,
    ) -> SandboxSpec:
        """Overlay per-role inputs onto the provider defaults.

        ``timeout_s == 0`` and ``pause_ttl_s < 0`` mean "use the provider
        default"; an explicit ``pause_ttl_s == 0`` disables pausing
        (always re-seed from git).

        The spec carries no CPU / memory / disk: sandbox resources are baked
        into the *template* at build time and the create API takes no
        resource arguments, so sizing is done via ``template``.
        """
        return SandboxSpec(
            coding_agent=coding_agent or self._default_coding_agent,
            template=template or self._default_template,
            timeout_s=timeout_s or self._default_timeout_s,
            pause_ttl_s=(self._default_pause_ttl_s if pause_ttl_s < 0 else pause_ttl_s),
            env=dict(env or {}),
        )

    async def acquire(
        self,
        spec: SandboxSpec,
        *,
        setup: Sequence[SandboxSetupStep] = (),
    ) -> tuple[Sandbox, CodingAgentRunner]:
        """Provision a sandbox for ``spec``, install the coding agent, and
        apply the launch's setup steps (``crewlet.sandbox.setup``).

        Returns ``(sandbox, runner)``. On install *or* setup failure the
        sandbox is torn down before the exception propagates — a
        half-provisioned box must never reach a run whose brief promises
        the full environment.
        """
        runner = self.runner_for(spec.coding_agent)
        sandbox = await self._provider.create(spec)
        try:
            await runner.install(sandbox)
            # Setup commands run WITH the run env (spec.env) so recipes can
            # reference the engine's identity facts ($CREWLET_AGENT_*) and
            # their own configured tokens at provisioning time.
            await apply_setup(sandbox, setup, env=spec.env)
        except SandboxSetupError:
            # A failed SETUP step is an operator config problem (a broken
            # role/provider setup command), not a coding-agent install
            # problem — log it distinctly so the operator debugs the right
            # subsystem.
            logger.exception("sandbox_setup_failed", sandbox_id=sandbox.id)
            await sandbox.close()
            raise
        except Exception:
            logger.exception("sandbox_install_failed", sandbox_id=sandbox.id)
            await sandbox.close()
            raise
        logger.debug(
            "sandbox_acquired",
            sandbox_id=sandbox.id,
            coding_agent=spec.coding_agent,
            setup_steps=[s.name for s in setup],
        )
        return sandbox, runner

    async def reconnect(
        self, sandbox_id: str, coding_agent: str
    ) -> tuple[Sandbox, CodingAgentRunner]:
        """Reattach to an existing (paused) sandbox for reuse.

        Used when a follow-up ``run_sandbox`` call in a resumed Execute loop
        reuses the same box + checkout: ``connect`` auto-resumes a paused box.
        No re-``install`` and no setup-step re-apply — the box already
        carries the ask shim, work dir, and the launch's provisioning.
        """
        runner = self.runner_for(coding_agent)
        sandbox = await self._provider.connect(sandbox_id)
        logger.debug(
            "sandbox_reconnected", sandbox_id=sandbox_id, coding_agent=coding_agent
        )
        return sandbox, runner


__all__ = ["SandboxConfigError", "SandboxManager"]
