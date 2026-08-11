"""Tests for ClaudeCodeRunner: command construction, result
parsing, and the detached start/poll/collect against the fake sandbox."""

from __future__ import annotations

import json
from typing import Any

from crewlet.sandbox.coding_agents._detached import ERR_PATH, EXIT_PATH
from crewlet.sandbox.coding_agents.claude_code import (
    ASK_PATH,
    DONE_MARKER,
    RESULT_PATH,
    ClaudeCodeRunner,
    build_claude_command,
    parse_claude_result,
)
from crewlet.sandbox.fake import FakeSandbox
from crewlet.sandbox.protocol import CodingAgentLLM, ExecResult, RunHandle, RunLimits

# --- build_claude_command -------------------------------------------------


def test_build_command_minimal() -> None:
    cmd = build_claude_command("do the thing")
    assert cmd.startswith("claude -p 'do the thing'")
    assert "--output-format json" in cmd
    assert "--permission-mode bypassPermissions" in cmd
    # No caps / mcp when unset.
    assert "--max-turns" not in cmd
    assert "--mcp-config" not in cmd


def test_build_command_with_caps_session_and_mcp() -> None:
    cmd = build_claude_command(
        "fix bug",
        model="claude-opus-4-8",
        limits=RunLimits(max_turns=12, max_budget_usd=3.5),
        session_id="sess-1",
        mcp_config_path="/work/.mcp.json",
    )
    assert "--model claude-opus-4-8" in cmd
    assert "--max-turns 12" in cmd
    assert "--max-budget-usd 3.5" in cmd
    assert "--resume sess-1" in cmd
    assert "--mcp-config /work/.mcp.json --strict-mcp-config" in cmd
    # No per-tool allowlist: bypassPermissions grants all tools on the scoped
    # servers (the mcp.allow feature was dropped).
    assert "--allowedTools" not in cmd


def test_build_command_threads_runtime_model() -> None:
    # The model resolved from the role's provider at run time reaches --model.
    # Claude Code takes the bare id; its endpoint/key ride the env.
    cmd = ClaudeCodeRunner()._build_command(
        "fix", RunLimits(), llm=CodingAgentLLM(model="claude-x")
    )
    assert "--model claude-x" in cmd


def test_build_command_quotes_dangerous_brief() -> None:
    cmd = build_claude_command("rm -rf / ; echo $(whoami)")
    # The whole brief is a single shell-quoted argument.
    assert "'rm -rf / ; echo $(whoami)'" in cmd


# --- parse_claude_result --------------------------------------------------


def test_parse_success_with_usage_and_pr() -> None:
    out = json.dumps(
        {
            "type": "result",
            "subtype": "success",
            "is_error": False,
            "result": "Opened https://github.com/acme/repo/pull/12 with the fix.",
            "session_id": "sess-9",
            "usage": {"input_tokens": 1200, "output_tokens": 340},
            "total_cost_usd": 0.21,
        }
    )
    r = parse_claude_result(out)
    assert r.success is True
    assert r.input_tokens == 1200
    assert r.output_tokens == 340
    assert r.cost_usd == 0.21
    assert r.session_id == "sess-9"
    assert r.delivered_refs == ["https://github.com/acme/repo/pull/12"]


def test_parse_error_result() -> None:
    out = json.dumps(
        {"subtype": "error_max_turns", "is_error": True, "result": "ran out"}
    )
    r = parse_claude_result(out)
    assert r.success is False
    assert "ran out" in r.error


def test_parse_empty_and_garbage() -> None:
    assert parse_claude_result("").success is False
    assert parse_claude_result("not json at all").success is False


def test_parse_banner_then_json() -> None:
    out = "Starting Claude Code...\n" + json.dumps(
        {"subtype": "success", "result": "done", "usage": {}}
    )
    r = parse_claude_result(out)
    assert r.success is True
    assert r.text == "done"


# --- runner against the fake sandbox -------------------------------------


class _ScriptedSandbox(FakeSandbox):
    """Fake sandbox whose ``exec`` returns scripted stdout (for inline run)."""

    def __init__(self, stdout: str) -> None:
        super().__init__()
        self._stdout = stdout

    async def exec(self, cmd: str, **kw: Any) -> ExecResult:
        self.commands.append(cmd)
        return ExecResult(exit_code=0, stdout=self._stdout, stderr="")


async def test_install_makes_artifact_dir() -> None:
    # Install is coding-agent plumbing only (work dir + ask shim);
    # environment provisioning (git auth etc.) is the manager-applied
    # setup-step pipeline (see test_setup.py / test_execute_sandbox.py).
    sb = FakeSandbox()
    await ClaudeCodeRunner().install(sb)
    assert any("mkdir -p" in c for c in sb.commands)
    assert all("git config" not in c for c in sb.commands)


async def test_inline_run_parses_exec_stdout() -> None:
    out = json.dumps(
        {"subtype": "success", "result": "shipped", "usage": {"input_tokens": 5}}
    )
    sb = _ScriptedSandbox(out)
    r = await ClaudeCodeRunner().run(
        sb, brief="b", env={"ANTHROPIC_API_KEY": "k"}, limits=RunLimits()
    )
    assert r.success is True
    assert r.text == "shipped"
    assert r.input_tokens == 5
    # the claude command was executed
    assert any(c.startswith("claude -p") for c in sb.commands)


async def test_start_backgrounds_a_redirecting_script() -> None:
    sb = FakeSandbox()
    handle = await ClaudeCodeRunner().start(
        sb, brief="implement", env={"K": "v"}, limits=RunLimits(max_turns=3)
    )
    assert handle.command_id == "fake-pid-1"
    script = sb.background[0]
    # "implement" is a shell-safe word so shlex.quote leaves it bare.
    assert "claude -p implement" in script
    assert f"> {RESULT_PATH}" in script
    # The done-marker carries the exit code (written, not `touch`ed) so the
    # completion poll can see a finished/crashed job — a zero-byte marker
    # reads identically to "missing" through read_file.
    assert f"> {DONE_MARKER}" in script
    assert "touch " not in script
    assert f"> {EXIT_PATH}" in script
    # stdin is closed so a headless agent never blocks on input.
    assert "< /dev/null" in script
    # The job runs UNCAPPED — never force-stopped on a wall-clock timer.
    assert "timeout " not in script
    assert script.startswith("sh -lc ")


async def test_start_does_not_wall_clock_cap_the_agent() -> None:
    # Even with a runtime budget set, the detached agent is NOT wrapped in
    # `timeout`: a legitimately long run must be free to finish. Completion
    # is detected by the result file / done-marker, not a kill.
    sb = FakeSandbox()
    await ClaudeCodeRunner().start(
        sb, brief="implement", env={}, limits=RunLimits(timeout_s=600)
    )
    script = sb.background[0]
    assert "timeout " not in script
    assert "claude -p implement" in script


async def test_poll_reads_done_marker() -> None:
    sb = FakeSandbox()
    runner = ClaudeCodeRunner()
    assert await runner.poll(sb, RunHandle(command_id="p")) is False
    # An empty marker (what a bare `touch` produces) must NOT read as
    # done — read_file returns b"" for both missing and empty files.
    await sb.write_file(DONE_MARKER, "")
    assert await runner.poll(sb, RunHandle(command_id="p")) is False
    await sb.write_file(DONE_MARKER, "0")
    assert await runner.poll(sb, RunHandle(command_id="p")) is True


async def test_poll_is_marker_only_for_claude() -> None:
    # Claude Code exits cleanly, so its completion signal is the done-marker.
    # A non-empty result file alone must NOT complete the run (the default
    # _result_done is False) — only OpenCode, which never exits, reads a
    # terminal event out of its streamed output.
    sb = FakeSandbox()
    runner = ClaudeCodeRunner()
    await sb.write_file(RESULT_PATH, '{"result": "done"}')
    assert await runner.poll(sb, RunHandle(command_id="p")) is False
    await sb.write_file(DONE_MARKER, "0")
    assert await runner.poll(sb, RunHandle(command_id="p")) is True


async def test_poll_fires_when_process_gone_without_marker() -> None:
    # No run-time TTL: a dead wrapper process (SIGKILL / OOM took the group
    # before the tail `echo` wrote the marker) is detected by the `kill -0`
    # liveness probe, so the run completes and collect can surface the failure
    # instead of hanging forever.
    sb = FakeSandbox()
    runner = ClaudeCodeRunner()
    # Wrapper alive (the fake's default) + no marker → still running.
    assert await runner.poll(sb, RunHandle(command_id="123")) is False
    # Wrapper PID now reads as dead → poll detects the abnormal death.
    sb.dead_pids.add("123")
    assert await runner.poll(sb, RunHandle(command_id="123")) is True


async def test_poll_without_command_id_skips_liveness_probe() -> None:
    # No PID to probe (edge) → liveness is skipped, never wrongly fires.
    sb = FakeSandbox()
    assert await ClaudeCodeRunner().poll(sb, RunHandle()) is False


async def test_collect_captures_activity_transcript() -> None:
    # The coding agent's streamed stderr (tool calls, shell commands, todos)
    # is captured onto the result for dashboard observability — even on a
    # successful run that emits no OTLP.
    sb = FakeSandbox()
    await sb.write_file(
        RESULT_PATH, json.dumps({"subtype": "success", "result": "done"})
    )
    await sb.write_file(ERR_PATH, "$ git clone ...\n→ Read pyproject.toml\n[✓] tests")
    r = await ClaudeCodeRunner().collect(sb, RunHandle(command_id="p"))
    assert r.success is True
    assert "git clone" in r.transcript
    assert "[✓] tests" in r.transcript


async def test_collect_surfaces_exit_code_on_crash() -> None:
    # The coding agent crashed before writing a result file; collect must
    # surface the non-zero exit code (so the completion turn reports a real
    # failure) instead of a silent "empty output".
    sb = FakeSandbox()
    await sb.write_file(EXIT_PATH, "1")
    r = await ClaudeCodeRunner().collect(sb, RunHandle(command_id="p"))
    assert r.success is False
    assert "exited with status 1" in r.error


async def test_collect_reads_and_parses_result_file() -> None:
    sb = FakeSandbox()
    await sb.write_file(
        RESULT_PATH,
        json.dumps(
            {
                "subtype": "success",
                "result": "PR https://github.com/acme/repo/pull/4",
                "usage": {"input_tokens": 10, "output_tokens": 4},
            }
        ),
    )
    r = await ClaudeCodeRunner().collect(sb, RunHandle(command_id="p"))
    assert r.success is True
    assert r.delivered_refs == ["https://github.com/acme/repo/pull/4"]
    assert r.output_tokens == 4


async def test_collect_overlays_ask_into_needs_input() -> None:
    sb = FakeSandbox()
    await sb.write_file(
        RESULT_PATH, json.dumps({"subtype": "success", "result": "blocked"})
    )
    await sb.write_file(ASK_PATH, json.dumps({"question": "Which DB?", "to": "team"}))
    r = await ClaudeCodeRunner().collect(sb, RunHandle(command_id="p"))
    assert r.needs_input is True
    assert r.question == "Which DB?"
    assert r.ask_to == "team"
