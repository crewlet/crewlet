"""In-process fakes for the sandbox subsystem -- the unit-test substrate.

No real sandbox, no real coding-agent CLI, no network (per the project
testing rules). ``FakeSandboxProvider`` records the spec it was handed and
mints ``FakeSandbox`` handles backed by an in-memory filesystem;
``FakeCodingAgentRunner`` returns a scripted ``CodingAgentResult`` (and can
be scripted to "call ask").
"""

from __future__ import annotations

from crewlet._logging import get_logger
from crewlet.sandbox.protocol import (
    DEFAULT_SANDBOX_HOME,
    CodingAgentLLM,
    CodingAgentResult,
    ExecResult,
    RunHandle,
    RunLimits,
    Sandbox,
    SandboxSpec,
)

logger = get_logger("sandbox.fake")


class FakeSandbox:
    """In-memory :class:`~crewlet.sandbox.protocol.Sandbox` for tests."""

    def __init__(
        self, sandbox_id: str = "fake-sandbox", home: str = DEFAULT_SANDBOX_HOME
    ) -> None:
        self._id = sandbox_id
        self._home = home
        self._files: dict[str, bytes] = {}
        self.commands: list[str] = []
        # env passed to each exec/start_background call, index-aligned with
        # ``commands`` (setup commands run WITH the run env — recipes read
        # $CREWLET_* / their configured tokens at provisioning time).
        self.exec_envs: list[dict[str, str]] = []
        self.background: list[str] = []
        self.closed = False
        self.paused = False
        # Pids the in-box `kill -0` liveness probe should report as DEAD
        # (empty → every process reads as alive). Lets a test drive the
        # process-liveness completion path in DetachedFileRunner.poll.
        self.dead_pids: set[str] = set()
        # Every set_timeout(seconds) the waiter issued (keepalive assertions).
        self.timeout_refreshes: list[float] = []

    @property
    def id(self) -> str:
        return self._id

    @property
    def home(self) -> str:
        return self._home

    async def exec(
        self,
        cmd: str,
        *,
        env: dict[str, str] | None = None,
        cwd: str = "",
        timeout_s: float = 0.0,
    ) -> ExecResult:
        self.commands.append(cmd)
        self.exec_envs.append(dict(env or {}))
        # Emulate the `kill -0 <pid>` liveness probe used by the detached
        # runner's completion poll: exit 1 ("no such process") for a pid the
        # test marked dead, else exit 0 (alive).
        if cmd.startswith("kill -0 "):
            parts = cmd.split()
            pid = parts[2].strip("'\"") if len(parts) > 2 else ""
            if pid in self.dead_pids:
                return ExecResult(exit_code=1, stdout="", stderr="")
        return ExecResult(exit_code=0, stdout="", stderr="")

    async def start_background(
        self,
        cmd: str,
        *,
        env: dict[str, str] | None = None,
        cwd: str = "",
    ) -> str:
        self.commands.append(cmd)
        self.exec_envs.append(dict(env or {}))
        self.background.append(cmd)
        return f"fake-pid-{len(self.background)}"

    async def write_file(self, path: str, content: bytes | str) -> None:
        self._files[path] = content.encode() if isinstance(content, str) else content

    async def read_file(self, path: str) -> bytes:
        return self._files.get(path, b"")

    async def set_timeout(self, seconds: float) -> None:
        self.timeout_refreshes.append(seconds)

    async def pause(self) -> None:
        self.paused = True

    async def close(self) -> None:
        self.closed = True


class FakeSandboxProvider:
    """Deterministic in-process provider. Records every spec it is handed."""

    kind = "fake"

    def __init__(self) -> None:
        self.created: list[SandboxSpec] = []
        self.sandboxes: list[FakeSandbox] = []
        self._by_id: dict[str, FakeSandbox] = {}
        self._counter = 0

    async def create(self, spec: SandboxSpec) -> Sandbox:
        self._counter += 1
        self.created.append(spec)
        sandbox = FakeSandbox(sandbox_id=f"fake-sandbox-{self._counter}")
        self.sandboxes.append(sandbox)
        self._by_id[sandbox.id] = sandbox
        return sandbox

    async def connect(self, sandbox_id: str) -> Sandbox:
        """Reattach to an existing fake sandbox (auto-unpauses it)."""
        sandbox = self._by_id.get(sandbox_id)
        if sandbox is None:
            raise KeyError(f"no fake sandbox with id {sandbox_id!r}")
        sandbox.paused = False
        return sandbox

    async def kill(self, sandbox_id: str) -> None:
        """Terminate by id without resuming (an unknown id is a no-op).

        Mirrors the real provider: a killed box stays ``paused`` in whatever
        state it was — tests assert on ``closed``, and a reaper must never
        have to un-pause a box to reclaim it.
        """
        sandbox = self._by_id.get(sandbox_id)
        if sandbox is not None:
            sandbox.closed = True


class FakeCodingAgentRunner:
    """Returns a scripted :class:`CodingAgentResult`.

    Defaults to a successful run that "opened a PR". Pass ``result`` to
    script other outcomes (a clarification request, a failure, …). The
    detached triple (:meth:`start` / :meth:`poll` / :meth:`collect`) is
    scripted too: ``poll`` returns ``poll_done`` (default True, i.e. the
    job is already finished), and ``collect`` returns the same result.
    """

    def __init__(
        self,
        name: str = "claude-code",
        *,
        result: CodingAgentResult | None = None,
        poll_done: bool = True,
    ) -> None:
        self.name = name
        self._result = result or CodingAgentResult(
            text="(fake) implemented the change and opened a PR",
            success=True,
            input_tokens=100,
            output_tokens=50,
            delivered_refs=["https://github.com/acme/repo/pull/1"],
        )
        self.poll_done = poll_done
        self.runs: list[tuple[str, dict[str, str], RunLimits]] = []
        self.started: list[tuple[str, dict[str, str], RunLimits]] = []
        self.collected: list[Sandbox] = []
        self.installed: list[Sandbox] = []
        # Last scoped MCP surface + LLM descriptor the runner was handed
        # (assertions).
        self.mcp_servers: dict[str, dict] = {}
        self.llm: CodingAgentLLM | None = None

    async def install(self, sandbox: Sandbox) -> None:
        self.installed.append(sandbox)

    async def run(
        self,
        sandbox: Sandbox,
        *,
        brief: str,
        env: dict[str, str],
        limits: RunLimits,
        llm: CodingAgentLLM | None = None,
        mcp_servers: dict[str, dict] | None = None,
    ) -> CodingAgentResult:
        self.runs.append((brief, dict(env), limits))
        self.llm = llm
        self.mcp_servers = dict(mcp_servers or {})
        return self._result

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
        self.started.append((brief, dict(env), limits))
        self.llm = llm
        self.mcp_servers = dict(mcp_servers or {})
        return RunHandle(
            command_id=f"cmd-{len(self.started)}",
            pid=1000 + len(self.started),
            session_id=self._result.session_id,
        )

    async def poll(self, sandbox: Sandbox, handle: RunHandle) -> bool:
        return self.poll_done

    async def collect(self, sandbox: Sandbox, handle: RunHandle) -> CodingAgentResult:
        self.collected.append(sandbox)
        return self._result


__all__ = ["FakeCodingAgentRunner", "FakeSandbox", "FakeSandboxProvider"]
