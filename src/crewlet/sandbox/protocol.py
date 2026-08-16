"""Protocols and data types for the sandboxed coding-agent runtime.

The sandbox subsystem (``crewlet.sandbox``) lets a sandbox-enabled role run
its Execute phase as a coding agent (Claude Code / OpenCode) inside an
isolated sandbox instead of the native LLM tool-loop. This module defines
the pluggable seams -- mirroring the ``LLMProvider`` protocol and the
``MCPToolBridge`` lifecycle -- so the engine stays provider-agnostic.

See ``docs/concepts/code-sandbox.md``.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Protocol, runtime_checkable


@dataclass
class ExecResult:
    """Outcome of one shell command run inside a :class:`Sandbox`."""

    exit_code: int
    stdout: str = ""
    stderr: str = ""


@dataclass
class RunLimits:
    """Resource / cost caps handed to a :class:`CodingAgentRunner`.

    ``0`` (the default) means "unset" for each field -- the runner falls
    back to the coding agent's own default or the sandbox spec's timeout.
    The token-budget cascade still bounds the run regardless.
    """

    max_turns: int = 0
    max_budget_usd: float = 0.0
    timeout_s: float = 0.0


@dataclass
class SandboxSpec:
    """The resolved, per-role inputs needed to mint and drive a sandbox.

    The repo to work in is **not** here -- it is task context the planner
    puts in the brief, and the coding agent clones it with
    the token its config injects. ``env`` carries the derived LLM creds +
    the generic agent-identity facts (``CREWLET_AGENT_*``) + the setup
    steps' env (``crewlet.sandbox.setup``) + ``role.sandbox.env`` (incl.
    any external-service tokens the config declares).

    ``timeout_s`` is **not** a run deadline: it is the box's initial TTL,
    which the waiter refreshes every tick (:meth:`Sandbox.set_timeout`) so a
    running job is never killed by the clock. It is effectively the
    orphan-reclaim grace — how long the box outlives an engine that stops
    heart-beating. ``pause_ttl_s`` governs a *paused* (blocked / reused) box:
    how long its snapshot is held before the
    :class:`~crewlet.sandbox.waiter.SandboxWaiter` reaps it (``0`` = never
    pause, always re-seed from git).

    There are deliberately **no CPU / memory / disk fields**: a sandbox's
    resources are a property of its *template*, fixed when the template is
    built (E2B's ``Template.build(cpu_count=…, memory_mb=…)``), and the
    sandbox-create API accepts no resource arguments. Sizing is therefore
    done by pointing ``template`` at a template built with the resources you
    want, not by a per-run spec field.
    """

    coding_agent: str = "claude-code"
    template: str = ""
    timeout_s: float = 900.0
    pause_ttl_s: float = 1800.0
    env: dict[str, str] = field(default_factory=dict)
    credential_files: dict[str, str] = field(default_factory=dict)
    """A ``cli-agent`` LLM provider's login: path-relative-to-the-box-home
    → absolute path on the **engine host**.

    Populated when the role's resolved sandbox provider is a
    :ref:`subscription CLI backend <cli-agent>`, so a local sandbox runs
    the coding agent against the very login ``crewlet llm login``
    established — no token minting, no API key.

    Each provider decides what to do with it, and they decide
    differently on purpose. ``local`` seeds the files into the box (and
    writes a refreshed one back). ``e2b`` **ignores it**: these files
    carry a refresh token whose rotation is shared fleet state, and
    pushing that onto a remote VM is a materially larger trust step than
    the scoped headless token ``build_sandbox_env`` already exports.
    """


@dataclass
class CodingAgentLLM:
    """The role's resolved LLM endpoint, for a coding agent that must
    self-configure its provider rather than read creds from the env.

    OpenCode resolves a bare ``<provider>/<model>`` against the provider's
    Models.dev catalog *and* the vendor's default endpoint — so a custom
    gateway plus an unlisted model id raises ``ProviderModelNotFoundError``
    and (when it does resolve) silently hits the wrong host. The runner
    uses this to declare a custom provider with an explicit ``base_url`` +
    the exact model, bypassing both. Claude Code reads its creds from the
    sandbox env (``ANTHROPIC_*``) and ignores this.

    The API key is **not** carried here: it rides the sandbox env
    (``OPENAI_API_KEY`` / ``ANTHROPIC_API_KEY`` from ``build_sandbox_env``)
    and the written config references it via ``{env:VAR}`` so the secret is
    never duplicated into the config payload.
    """

    model: str = ""
    """Raw model id (provider-agnostic, e.g. ``acme/model-x-large``)."""
    provider_type: str = ""
    """The model FAMILY the runner should address.

    For an API entry this is the ``providers.llm`` type (``anthropic`` |
    ``openai`` | ``openai-compatible``), which already names the family.
    For a subscription (``cli-agent``) entry every provider shares one
    type, so it carries the CLI profile's **vendor** instead — otherwise
    a Claude subscription's ``sonnet`` would be addressed as
    ``openai/sonnet``. See
    :func:`crewlet.agent.execute_sandbox._coding_agent_llm`."""
    base_url: str = ""
    """The endpoint; empty means the vendor default (no custom provider)."""


@dataclass
class RunHandle:
    """A handle to a *detached* coding job started in the background.

    Returned by :meth:`CodingAgentRunner.start` and persisted (its
    ``command_id`` / ``pid``) in the ``pending_sandbox_run`` row so a
    later turn — or a fresh engine after a restart — can reconnect to the
    still-running command and collect its result.
    """

    command_id: str = ""
    pid: int = 0
    session_id: str = ""


@dataclass
class CodingAgentResult:
    """Structured outcome of one coding-agent run inside a sandbox."""

    text: str = ""
    success: bool = False
    input_tokens: int = 0
    output_tokens: int = 0
    cost_usd: float = 0.0
    session_id: str = ""
    needs_input: bool = False
    """The agent called the ``ask`` tool and stopped."""
    question: str = ""
    """The clarifying question, when ``needs_input``."""
    ask_to: str = ""
    """Who should answer: ``requester`` | ``team`` | ``manager`` | a name."""
    delivered_refs: list[str] = field(default_factory=list)
    """Branch names / PR URLs the run produced."""
    changed_files: list[str] = field(default_factory=list)
    commands: list[str] = field(default_factory=list)
    error: str = ""
    transcript: str = ""
    """The coding agent's streamed activity log (tool calls, shell commands,
    todos) captured from its stderr — the observability surface for an agent
    (e.g. OpenCode) that emits no OTLP. Tail-capped; redacted at publish."""


#: Home directory of a sandbox that doesn't say otherwise — E2B's box
#: user. Every run artefact (result, done-marker, ask signal, findings)
#: lives under ``<home>/.crewlet``.
DEFAULT_SANDBOX_HOME = "/home/user"


@runtime_checkable
class Sandbox(Protocol):
    """A live, isolated execution environment for one Execute phase."""

    @property
    def id(self) -> str: ...

    @property
    def home(self) -> str:
        """Absolute path the run's artefacts live under.

        A remote box has one home per VM, so this was a module constant
        for as long as E2B was the only backend.  A *local* backend runs
        many boxes on one filesystem — sharing ``/home/user/.crewlet``
        between them would have every run reading its neighbour's
        done-marker and result. Making the home a property of the
        sandbox is what lets the same
        :class:`~crewlet.sandbox.coding_agents._detached.DetachedFileRunner`
        drive both backends unchanged.
        """
        ...

    async def exec(
        self,
        cmd: str,
        *,
        env: dict[str, str] | None = None,
        cwd: str = "",
        timeout_s: float = 0.0,
    ) -> ExecResult: ...

    async def start_background(
        self,
        cmd: str,
        *,
        env: dict[str, str] | None = None,
        cwd: str = "",
    ) -> str:
        """Start ``cmd`` as a detached background process; return its handle.

        Returns immediately with a process/command id (the ``pid`` E2B's
        ``commands.run(background=True)`` hands back). The detached coding
        job is launched this way so the kick-off turn can end; the
        job writes its result to a file the runner reads on ``collect``.
        """
        ...

    async def write_file(self, path: str, content: bytes | str) -> None: ...

    async def read_file(self, path: str) -> bytes: ...

    async def set_timeout(self, seconds: float) -> None:
        """Reset the box's wall-clock TTL to ``seconds`` from now (keepalive).

        E2B reclaims a box ``timeout`` seconds after the TTL was last set.
        The engine imposes **no run-time limit** on a coding job — it runs as
        long as it needs — so the :class:`~crewlet.sandbox.waiter.SandboxWaiter`
        calls this every poll tick to keep a *running* box alive. The box is
        thus bounded only by how long the engine can go *without* a heartbeat
        (an orphan-reclaim grace after an engine crash), never by a fixed run
        deadline. Completion is detected by tracking the job itself (done
        marker / terminal stream event / process liveness), not by a TTL.
        Providers without a settable TTL no-op.
        """
        ...

    async def pause(self) -> None:
        """Suspend (snapshot) the sandbox for later resume.

        Used to hold a sandbox blocked on a clarification with exact
        conversational continuity. Providers without snapshot support
        no-op; the engine then re-seeds from the git branch instead.
        """
        ...

    async def close(self) -> None: ...


@runtime_checkable
class SandboxProvider(Protocol):
    """Pluggable backend that mints :class:`Sandbox` handles.

    Mirrors ``LLMProvider``: configured under ``providers.sandbox`` and
    swapped wholesale on ``apply_config``.
    """

    kind: str

    async def create(self, spec: SandboxSpec) -> Sandbox: ...

    async def connect(self, sandbox_id: str) -> Sandbox:
        """Reconnect to an existing (live or paused) sandbox by id.

        The detached lifecycle relies on this: the
        completion turn — possibly in a fresh engine after a restart —
        reattaches to the sandbox that ran the background job to collect
        its result and tear it down. A paused sandbox auto-resumes on
        connect.
        """
        ...

    async def kill(self, sandbox_id: str) -> None:
        """Terminate a sandbox by id **without resuming it**.

        The teardown counterpart of :meth:`connect`, and the primitive the
        pause reaper needs: ``connect`` auto-resumes a paused box, so
        reclaiming a paused snapshot through it would boot the VM back up
        purely to kill it. Best-effort — a box that is already gone is not
        an error.
        """
        ...


@runtime_checkable
class CodingAgentRunner(Protocol):
    """Runs one coding agent inside a :class:`Sandbox` against a brief.

    Two execution shapes share one runner: the **inline** ``run`` (start
    → block → result, used by tests and short jobs) and the **detached**
    ``start`` / ``poll`` / ``collect`` triple the engine drives across
    turns. ``run`` is equivalent to ``start`` then
    ``collect`` with the same handle.
    """

    name: str

    async def install(self, sandbox: Sandbox) -> None: ...

    async def run(
        self,
        sandbox: Sandbox,
        *,
        brief: str,
        env: dict[str, str],
        limits: RunLimits,
        llm: CodingAgentLLM | None = None,
        mcp_servers: dict[str, dict] | None = None,
    ) -> CodingAgentResult: ...

    async def start(
        self,
        sandbox: Sandbox,
        *,
        brief: str,
        env: dict[str, str],
        limits: RunLimits,
        llm: CodingAgentLLM | None = None,
        mcp_servers: dict[str, dict] | None = None,
    ) -> RunHandle:
        """Start the coding agent as a background command; return a handle.

        Returns immediately — the job keeps running server-side. The
        handle's ``command_id`` / ``pid`` let a later ``poll`` / ``collect``
        reattach to it. ``llm`` carries the role's model + endpoint (the
        runner addresses + configures its provider from it); ``mcp_servers``
        is the scoped MCP surface — server-level scoping only, no per-tool
        allowlist.
        """
        ...

    async def poll(self, sandbox: Sandbox, handle: RunHandle) -> bool:
        """Return True iff the background command has finished."""
        ...

    async def collect(self, sandbox: Sandbox, handle: RunHandle) -> CodingAgentResult:
        """Collect the finished job's result + diff from the sandbox."""
        ...


__all__ = [
    "DEFAULT_SANDBOX_HOME",
    "CodingAgentLLM",
    "CodingAgentResult",
    "CodingAgentRunner",
    "ExecResult",
    "RunHandle",
    "RunLimits",
    "Sandbox",
    "SandboxProvider",
    "SandboxSpec",
]
