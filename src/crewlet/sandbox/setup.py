"""Declarative sandbox provisioning — the setup-step framework.

A coding agent's box needs environment wiring beyond the coding-agent CLI
itself: git auth, package-registry credentials, preinstalled toolchains,
whatever the org's tasks demand. Rather than hardcoding each concern into a
runner, provisioning is expressed as an ordered list of
:class:`SandboxSetupStep` — one declarative unit each contributing:

- ``files``: content written into the box (helper scripts, config files);
- ``commands``: shell commands run after the files land (chmod, ``git
  config``, ``apt-get`` — anything);
- ``env``: variables merged into the coding agent's run env (``${VAR}``
  references resolved with the rest of the sandbox env);
- ``brief``: a short paragraph TELLING the agent what its environment now
  provides, folded into the "## Your environment" block of its brief.

Steps come ENTIRELY from company config, applied in order (later env wins):

1. ``providers.sandbox.setup`` — engine-wide steps for every sandbox role;
2. ``role.sandbox.setup`` — per-role extras.

The engine ships NO steps of its own — git auth included. The recommended
git wiring (a github.com-scoped credential helper reading ``$GITHUB_TOKEN``
at git-runtime + SSH→HTTPS rewrites, so a headless ``git clone`` never dies
on ``could not read Username``) is a documented CONFIG RECIPE — see
``docs/concepts/code-sandbox.md`` and the ``git-auth`` step in
``examples/nimbus.company.yaml`` — expressed as an ordinary step with a
``brief`` telling the coding agent to use ``$GITHUB_TOKEN`` for GitHub
operations. What stays engine-side is only what static config cannot know,
expressed generically: the run env (``build_sandbox_env``) carries the LLM
creds and the agent's identity as ``CREWLET_AGENT_HANDLE`` /
``CREWLET_AGENT_EMAIL`` — never a tool-specific variable. External tokens
are declared in ``role.sandbox.env`` (e.g. ``GITHUB_TOKEN``), and setup
commands execute WITH the run env, so a recipe maps the generic facts into
tool shape itself (``git config --global user.name "$CREWLET_AGENT_HANDLE"``).

:func:`apply_setup` runs at box-acquisition time
(:meth:`~crewlet.sandbox.manager.SandboxManager.acquire`); a failed command
fails the acquisition (the manager tears the box down) — a half-provisioned
box must never receive a brief promising an environment it doesn't have.
Mechanism + hint together: the steps make the environment work no matter how
the agent reasons, and the brief stops it wasting rounds rediscovering that.
"""

from __future__ import annotations

from collections.abc import Sequence

from pydantic import BaseModel, ConfigDict, Field

from crewlet._logging import get_logger
from crewlet.redaction import redact_secrets
from crewlet.sandbox.protocol import Sandbox

#: Default budget for one setup command.
#:
#: Anchored to what provisioning actually is: a cold dependency install
#: (``apt-get install``, ``npm ci``, ``uv sync``) over a network is
#: typically one to five minutes, and a large monorepo clone or an image
#: pull can reach ten. Ten minutes covers those while still failing well
#: inside the box TTL (``default_timeout_seconds``, 900 s), so a wedged
#: command surfaces as a named setup failure rather than as a box the
#: provider reaps out from under the run.
DEFAULT_SETUP_TIMEOUT_SECONDS = 600.0

logger = get_logger("sandbox.setup")


class SandboxSetupError(RuntimeError):
    """A setup step's command failed — the box is not usable as promised."""


class SandboxSetupStep(BaseModel):
    """One declarative provisioning unit applied to a fresh sandbox.

    Config-loadable (``providers.sandbox.setup`` / ``role.sandbox.setup``);
    every provisioning concern — git auth included — goes through this one
    apply / env-merge / brief pipeline with no engine special cases.
    """

    model_config = ConfigDict(extra="forbid")

    name: str
    """Short identifier, used in logs and setup-failure errors."""
    files: dict[str, str] = Field(default_factory=dict)
    """Files written into the box (path → content) before ``commands`` run."""
    commands: list[str] = Field(default_factory=list)
    """Shell commands run in order after the files land. A non-zero exit
    fails the sandbox acquisition — no silent half-provisioning."""
    env: dict[str, str] = Field(default_factory=dict)
    """Env merged into the coding agent's run env. ``${VAR}`` references are
    resolved exactly ONCE, with the rest of the sandbox env at launch (like
    ``role.sandbox.env``)."""
    # Resolution semantics (``config.resolve_setup_step_content``): ``${VAR}``
    # in FILES and COMMANDS is resolved when the step is loaded (provider
    # steps at manager build, role steps at launch); ``env`` stays verbatim
    # until ``build_sandbox_env``'s single resolution; ``brief`` / ``name``
    # are never resolved. Plain ``$VAR`` (no braces) shell syntax is left
    # untouched everywhere.
    brief: str = ""
    """Environment-context paragraph for the coding agent's brief — what
    this step made true about the box (empty = nothing to tell)."""
    timeout_seconds: float = Field(default=DEFAULT_SETUP_TIMEOUT_SECONDS, gt=0)
    """How long each of this step's commands may run.

    Provisioning is not a control-plane call. Without its own budget
    these commands inherited the backend's control timeout — sized for
    a ``mkdir`` or a ``docker exec``, not for work — so any real
    provisioning step (a dependency install, a cold image pull, a large
    clone) was killed and failed the whole acquisition. Raise it for a
    step you know is slow; lower it for one that should be instant, so
    a hung command surfaces as a setup failure rather than eating the
    turn."""


def setup_env(steps: Sequence[SandboxSetupStep]) -> dict[str, str]:
    """Merge all steps' env contributions, in step order (later wins)."""
    merged: dict[str, str] = {}
    for step in steps:
        merged.update(step.env)
    return merged


async def apply_setup(
    sandbox: Sandbox,
    steps: Sequence[SandboxSetupStep],
    *,
    env: dict[str, str] | None = None,
) -> None:
    """Apply every step to a fresh box: write files, run commands, in order.

    ``env`` is the run env the coding agent will get — commands run WITH it
    so a recipe can reference the engine's identity facts
    (``$CREWLET_AGENT_HANDLE`` / ``$CREWLET_AGENT_EMAIL``) and its own
    configured tokens at provisioning time (e.g. the git-auth recipe's
    ``git config --global user.name "$CREWLET_AGENT_HANDLE"``).

    Raises :class:`SandboxSetupError` on the first failed command so the
    caller (the manager's acquire) tears the box down — the coding agent's
    brief promises this environment, so a partial application must never
    reach a run.
    """
    for step in steps:
        for path, content in step.files.items():
            await sandbox.write_file(path, content)
        for index, cmd in enumerate(step.commands):
            res = await sandbox.exec(cmd, env=env or {}, timeout_s=step.timeout_seconds)
            if res.exit_code != 0:
                # NOT the command text. `${VAR}` references in commands
                # are resolved before they get here (see
                # `config.resolve_setup_step_content`), so a recipe that
                # pipes a token into a login carries that token verbatim
                # in `cmd` — and this message is logged AND handed back
                # to the LLM. The step name plus the command's position
                # identifies it precisely, and the operator has the
                # config; stderr is redacted for the same reason a
                # coding-agent transcript is.
                detail = redact_secrets(res.stderr.strip())
                raise SandboxSetupError(
                    f"setup step {step.name!r} command #{index + 1} failed "
                    f"(exit {res.exit_code})" + (f" — {detail}" if detail else "")
                )
        logger.debug(
            "sandbox_setup_step_applied",
            step=step.name,
            files=len(step.files),
            commands=len(step.commands),
        )


def environment_brief(
    steps: Sequence[SandboxSetupStep],
    *,
    mcp_servers: Sequence[str] = (),
) -> str:
    """The "## Your environment" brief block for the coding agent.

    A generic sandbox intro, then each step's ``brief`` paragraph (what the
    configured provisioning made true about the box — this is where a
    config-authored git-auth step tells the agent to use ``$GITHUB_TOKEN``),
    then the connected MCP servers. The agent shouldn't have to rediscover —
    or fail against — what its environment already provides.
    """
    lines = [
        "\n## Your environment",
        "You are running autonomously inside an isolated sandbox with your "
        "own shell, filesystem, and a full developer toolchain (git, "
        "language runtimes, build tools). Work directly here — there is no "
        "other machine to set up.",
    ]
    lines.extend(step.brief for step in steps if step.brief)
    if mcp_servers:
        lines.append(
            "You also have these MCP tool servers connected: "
            + ", ".join(sorted(mcp_servers))
            + "."
        )
    return "\n".join(lines)


__all__ = [
    "SandboxSetupError",
    "SandboxSetupStep",
    "apply_setup",
    "environment_brief",
    "setup_env",
]
