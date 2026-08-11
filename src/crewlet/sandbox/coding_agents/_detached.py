"""Shared detached-run plumbing for file-based coding-agent runners.

Both Claude Code and OpenCode run headless in an E2B sandbox and follow
the same detached shape: a background process redirects
its result to a file and drops a done-marker; ``poll`` checks the marker,
``collect`` reads + parses the file, and an ``ask.json`` overlay surfaces
a mid-run clarification. This base owns that file plumbing so each
runner only supplies its CLI command (``_build_command``) and output
parser (``_parse_result``).

Touches only the :class:`Sandbox` protocol, so the fake sandbox is a
faithful substitute and the runners are unit-testable without a real CLI.
"""

from __future__ import annotations

import abc
import json
import re
import shlex
from typing import Any

from crewlet._logging import get_logger
from crewlet.sandbox.coding_agents.ask import ASK_SHIM_PATH, build_ask_shim
from crewlet.sandbox.protocol import (
    CodingAgentLLM,
    CodingAgentResult,
    RunHandle,
    RunLimits,
    Sandbox,
)

logger = get_logger("sandbox.coding_agent")

# Side dir for the detached artifacts (out of the checkout so a git clean
# in the brief can't wipe them).
WORK_DIR = "/home/user/.crewlet"
RESULT_PATH = f"{WORK_DIR}/result.json"
ERR_PATH = f"{WORK_DIR}/err.log"
# The done-marker AND exit-code file. The marker is written by the shell
# AFTER the agent process returns, carrying its exit code. It must be
# NON-EMPTY: the Sandbox protocol's read_file returns b"" for both a missing
# and an empty file, so a zero-byte ``touch`` marker would read identically
# to "not yet created" and ``poll`` would never see it. The exit code is also
# surfaced on collect to explain a crash. NB: a coding agent can FINISH its
# work yet never exit (open file watcher / MCP subprocesses keep its event
# loop alive), so the shell never reaches this write — such a runner instead
# flushes a terminal marker into its streamed output, which ``poll`` detects
# via ``_result_done`` (see ``OpenCodeRunner._result_done``).
DONE_MARKER = f"{WORK_DIR}/done"
EXIT_PATH = f"{WORK_DIR}/exit_code"
ASK_PATH = f"{WORK_DIR}/ask.json"
MCP_CONFIG_PATH = f"{WORK_DIR}/mcp.json"
# The coding agent's authoritative final report. The brief instructs the agent
# to write its structured findings here before it stops (see
# ``findings_brief_instruction``). It is the result carrier of record: a run
# that finishes but never exits (OpenCode's known bug) can lose its streamed
# final message, and a tool-only stdout yields no parsed text at all — but the
# findings file survives both, so ``collect`` prefers it for ``result.text``
# and treats its presence as the success signal. Lives under ``WORK_DIR`` (out
# of the checkout) so a ``git clean`` in the brief can't wipe it.
FINDINGS_PATH = f"{WORK_DIR}/findings.md"

# github.com/<owner>/<repo>/pull/<n> — the PR the agent opened.
PR_RE = re.compile(r"https://github\.com/[\w.-]+/[\w.-]+/pull/\d+")

# Tail cap for the captured activity transcript (the coding agent's streamed
# stderr) carried on the result for dashboard observability. The tail keeps
# the most recent activity + the conclusion; the head (clone / install) is
# the least interesting if anything has to be dropped.
_MAX_TRANSCRIPT_CHARS = 100_000


async def _read_text(sandbox: Sandbox, path: str) -> str:
    raw = await sandbox.read_file(path)
    return raw.decode() if isinstance(raw, bytes | bytearray) else str(raw)


async def clear_run_artifacts(sandbox: Sandbox) -> None:
    """Remove the previous run's result / marker files before a reuse run.

    A reused box still carries the prior run's
    done-marker, result, findings and ask files. Without clearing them the
    completion poll would fire immediately on the stale marker and
    ``collect`` would read the old result. The checkout itself is
    deliberately left intact — that disk state is the continued context.
    """
    await sandbox.exec(
        f"rm -f {DONE_MARKER} {EXIT_PATH} {RESULT_PATH} {FINDINGS_PATH} {ASK_PATH}"
    )


def _tail(text: str, max_chars: int) -> str:
    """Keep the last ``max_chars`` of ``text`` with a truncation marker."""
    if len(text) <= max_chars:
        return text
    return "…[earlier output truncated]…\n" + text[-max_chars:]


class DetachedFileRunner(abc.ABC):
    """Base for file-based detached coding-agent runners.

    Subclasses set ``name`` and implement :meth:`_build_command` (the
    headless CLI invocation) and :meth:`_parse_result` (its output →
    :class:`CodingAgentResult`); this base owns the shared file plumbing.
    """

    name = "coding-agent"
    # Where this runner writes its scoped MCP config inside the sandbox.
    mcp_config_path = MCP_CONFIG_PATH

    @abc.abstractmethod
    def _build_command(
        self,
        brief: str,
        limits: RunLimits,
        *,
        llm: CodingAgentLLM | None = None,
        mcp_config_path: str = "",
    ) -> str:
        """The headless CLI invocation for this coding agent."""

    @abc.abstractmethod
    def _parse_result(self, stdout: str) -> CodingAgentResult:
        """Map this coding agent's output onto a :class:`CodingAgentResult`."""

    async def _write_config(
        self,
        sandbox: Sandbox,
        mcp_servers: dict[str, Any],
        llm: CodingAgentLLM | None = None,
    ) -> str:
        """Write the agent's config file; return the path the CLI is pointed at.

        Default is Claude Code's ``.mcp.json`` (``{"mcpServers": ...}``);
        Claude Code reaches its LLM via the sandbox env, so ``llm`` is
        unused here. Returns ``""`` when there are no servers to render.
        """
        if not mcp_servers:
            return ""
        await sandbox.write_file(
            self.mcp_config_path, json.dumps({"mcpServers": mcp_servers}, indent=2)
        )
        return self.mcp_config_path

    async def install(self, sandbox: Sandbox) -> None:
        # CLI ships in E2B's template; set up the artifact dir + the
        # ``crewlet-ask`` shim so the agent can signal a clarification.
        # Environment provisioning (git auth, registry creds, toolchains)
        # is NOT the runner's concern — the manager applies the launch's
        # setup steps after install (``crewlet.sandbox.setup``).
        await sandbox.exec(f"mkdir -p {WORK_DIR}")
        await sandbox.write_file(ASK_SHIM_PATH, build_ask_shim())
        await sandbox.exec(f"chmod +x {ASK_SHIM_PATH}")

    async def run(
        self,
        sandbox: Sandbox,
        *,
        brief: str,
        env: dict[str, str],
        limits: RunLimits,
        llm: CodingAgentLLM | None = None,
        mcp_servers: dict[str, Any] | None = None,
    ) -> CodingAgentResult:
        path = await self._write_config(sandbox, mcp_servers or {}, llm)
        cmd = self._build_command(
            brief,
            limits,
            llm=llm,
            mcp_config_path=path,
        )
        # Close stdin (a headless agent must never block on input) and bound
        # the run by the exec timeout (the inline analogue of the detached
        # `timeout` wrap below).
        res = await sandbox.exec(
            f"{cmd} < /dev/null", env=env, timeout_s=limits.timeout_s
        )
        result = self._parse_result(res.stdout)
        if res.stderr and res.stderr.strip():
            result.transcript = _tail(res.stderr.strip(), _MAX_TRANSCRIPT_CHARS)
        return await self._overlay_ask(sandbox, result)

    async def start(
        self,
        sandbox: Sandbox,
        *,
        brief: str,
        env: dict[str, str],
        limits: RunLimits,
        llm: CodingAgentLLM | None = None,
        mcp_servers: dict[str, Any] | None = None,
    ) -> RunHandle:
        path = await self._write_config(sandbox, mcp_servers or {}, llm)
        inner = self._build_command(
            brief,
            limits,
            llm=llm,
            mcp_config_path=path,
        )
        # Close stdin (a headless agent must never block on input). The job
        # runs UNCAPPED — we never force-stop it on a wall-clock timer, so a
        # legitimately long run is free to finish. On a clean exit the shell
        # writes the exit code into the exit-code file and the done-marker
        # (non-empty, see DONE_MARKER). When the agent finishes but never
        # exits, ``poll`` falls back to the non-empty result file, so the run
        # still completes (and the sandbox is torn down, killing the husk).
        script = (
            f"{inner} < /dev/null > {RESULT_PATH} 2> {ERR_PATH}; "
            f"code=$?; echo $code > {EXIT_PATH}; echo $code > {DONE_MARKER}"
        )
        pid = await sandbox.start_background(f"sh -lc {shlex.quote(script)}", env=env)
        logger.info("coding_agent_started", agent=self.name, pid=pid)
        return RunHandle(command_id=pid)

    def _result_done(self, stdout: str) -> bool:
        """Whether the agent's streamed output signals it has finished.

        Default: no — rely on the done-marker (the process exited cleanly).
        A coding agent that finishes its work but never exits (OpenCode's
        known bug) never lets the shell write the marker, so such a runner
        overrides this to detect a terminal event it flushed into its output
        stream before hanging (see ``OpenCodeRunner._result_done``).
        """
        return False

    async def poll(self, sandbox: Sandbox, handle: RunHandle) -> bool:
        # Three completion signals, so a finished / hung / dead job can't wedge
        # the run (there is NO run-time TTL to fall back on — the box is kept
        # alive while the job runs):
        #   (1) the done-marker — the wrapper returned (clean / crashed) and
        #       wrote the exit code;
        #   (2) a terminal signal in the streamed output (``_result_done``) for
        #       an agent that finishes its work but never exits (OpenCode's
        #       known hang) — the keepalive holds the box open so the terminal
        #       event lands and collect reads the streamed result;
        #   (3) process liveness — the detached wrapper PID is gone yet no
        #       marker was written, i.e. the whole job process-group died
        #       abnormally (SIGKILL / OOM took it before the tail `echo` ran).
        #       This is the proper way to detect a dead coding agent; without
        #       it the run would hang forever, and collect then surfaces the
        #       partial / error result. A still-alive wrapper (work in progress,
        #       or a finished-but-unexited agent) reports alive and we keep
        #       waiting — a genuinely hung-but-alive process is indistinguishable
        #       from a working one without a timer, which we deliberately do not
        #       impose.
        if await sandbox.read_file(DONE_MARKER):
            return True
        stdout = await _read_text(sandbox, RESULT_PATH)
        if stdout and self._result_done(stdout):
            return True
        if handle.command_id and not await self._process_alive(
            sandbox, handle.command_id
        ):
            logger.warning(
                "coding_agent_process_gone", agent=self.name, pid=handle.command_id
            )
            return True
        return False

    async def _process_alive(self, sandbox: Sandbox, pid: str) -> bool:
        """Whether the detached wrapper process is still running (``kill -0``).

        ``kill -0`` sends no signal — it only probes whether the PID exists
        (exit 0) or not (exit non-zero). The PID is the ``sh -lc`` wrapper
        started by :meth:`start`: while the coding agent runs (or has finished
        but not exited) the wrapper is alive, and once it exits it has already
        written the done-marker (caught above). So a *dead wrapper with no
        marker* means the process-group was killed outright — the run is over.
        """
        res = await sandbox.exec(f"kill -0 {shlex.quote(str(pid))} 2>/dev/null")
        return res.exit_code == 0

    async def collect(self, sandbox: Sandbox, handle: RunHandle) -> CodingAgentResult:
        stdout = await _read_text(sandbox, RESULT_PATH)
        result = self._parse_result(stdout)
        # Capture the coding agent's activity transcript for dashboard
        # observability (the OTLP-less surface). The parser may already have
        # built one from streamed events (OpenCode --format json); otherwise
        # fall back to the raw stderr (claude / plain runs). Read stderr once;
        # reuse for the crash detail.
        err = (await _read_text(sandbox, ERR_PATH)).strip()
        if result.transcript:
            result.transcript = _tail(result.transcript, _MAX_TRANSCRIPT_CHARS)
        elif err:
            result.transcript = _tail(err, _MAX_TRANSCRIPT_CHARS)

        code = (await _read_text(sandbox, EXIT_PATH)).strip()
        crashed = bool(code) and code != "0"

        # The findings file is the result carrier of record: the brief asks the
        # agent to write its structured report there before stopping. It
        # survives a finished-but-never-exited run whose streamed final message
        # was lost (and a tool-only stdout that parses to no text), so prefer
        # it for the result text and treat its presence as the success signal
        # unless the process itself crashed.
        findings = (await _read_text(sandbox, FINDINGS_PATH)).strip()
        if findings:
            result.text = findings
            if not result.delivered_refs:
                result.delivered_refs = PR_RE.findall(findings)
            if not crashed:
                result.success = True
                result.error = ""
        elif not result.success and not result.text.strip():
            # No findings file AND nothing parsed from the stream — the job
            # produced no report. Surface stderr and/or the non-zero exit code
            # so the completion reports a real failure instead of a silent
            # stall ("no assistant output" with only a tool-name transcript).
            detail = err
            if crashed:
                crash = f"coding agent exited with status {code}"
                detail = f"{crash}: {err}" if err else crash
            if detail:
                result.error = detail[:500]
        return await self._overlay_ask(sandbox, result)

    async def _overlay_ask(
        self, sandbox: Sandbox, result: CodingAgentResult
    ) -> CodingAgentResult:
        """If the ``crewlet-ask`` shim dropped a question, surface it."""
        blob = (await _read_text(sandbox, ASK_PATH)).strip()
        if not blob:
            return result
        try:
            ask = json.loads(blob)
        except (json.JSONDecodeError, ValueError):
            return result
        if isinstance(ask, dict) and ask.get("question"):
            result.needs_input = True
            result.question = str(ask.get("question", ""))
            result.ask_to = str(ask.get("to", "") or ask.get("ask_to", ""))
        return result


__all__ = [
    "ASK_PATH",
    "DONE_MARKER",
    "ERR_PATH",
    "EXIT_PATH",
    "FINDINGS_PATH",
    "MCP_CONFIG_PATH",
    "PR_RE",
    "RESULT_PATH",
    "WORK_DIR",
    "DetachedFileRunner",
    "clear_run_artifacts",
]
