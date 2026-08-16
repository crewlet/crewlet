"""Tests for OpenCodeRunner: command + parse + detached run."""

from __future__ import annotations

import json

from crewlet.sandbox.coding_agents._detached import (
    DONE_MARKER,
    EXIT_PATH,
    FINDINGS_PATH,
    RESULT_PATH,
)
from crewlet.sandbox.coding_agents.opencode import (
    OPENCODE_CONFIG_PATH,
    OPENCODE_PROVIDER_ID,
    OpenCodeRunner,
    build_opencode_command,
    opencode_model_arg,
    opencode_stream_done,
    parse_opencode_result,
    to_opencode_provider,
)
from crewlet.sandbox.fake import FakeSandbox
from crewlet.sandbox.protocol import CodingAgentLLM, RunHandle, RunLimits


def test_build_command_minimal() -> None:
    # Always --format json: streamed, per-line-flushed events so the result
    # and the terminal idle event survive OpenCode's never-exits hang.
    assert build_opencode_command("do it") == "opencode run 'do it' --format json"


def test_build_command_with_model() -> None:
    cmd = build_opencode_command("fix", model="anthropic/claude-opus-4-8")
    assert cmd == "opencode run fix --model anthropic/claude-opus-4-8 --format json"


def test_model_arg_custom_provider_when_base_url_set() -> None:
    # A custom gateway → address the model under our custom provider, NOT
    # the built-in `openai` (whose catalog 404s the id and hits api.openai).
    llm = CodingAgentLLM(
        model="acme/model-x-large",
        provider_type="openai-compatible",
        base_url="https://llm.example.com/v1/",
    )
    assert opencode_model_arg(llm) == f"{OPENCODE_PROVIDER_ID}/acme/model-x-large"


def test_model_arg_builtin_family_when_no_base_url() -> None:
    # No custom endpoint → the built-in provider family is fine.
    oc = CodingAgentLLM(model="gpt-x", provider_type="openai")
    assert opencode_model_arg(oc) == "openai/gpt-x"
    an = CodingAgentLLM(model="claude-x", provider_type="anthropic")
    assert opencode_model_arg(an) == "anthropic/claude-x"
    assert opencode_model_arg(None) == ""
    assert opencode_model_arg(CodingAgentLLM(model="")) == ""


def test_to_opencode_provider_block() -> None:
    llm = CodingAgentLLM(
        model="acme/model-x-large",
        provider_type="openai-compatible",
        base_url="https://api.x/v1",
    )
    block = to_opencode_provider(llm)[OPENCODE_PROVIDER_ID]
    assert block["npm"] == "@ai-sdk/openai-compatible"
    assert block["options"]["baseURL"] == "https://api.x/v1"
    # The key is referenced from the env, never inlined.
    assert block["options"]["apiKey"] == "{env:OPENAI_API_KEY}"
    assert "acme/model-x-large" in block["models"]
    # Anthropic-type → the anthropic SDK + key env.
    an = to_opencode_provider(
        CodingAgentLLM(
            model="claude-x", provider_type="anthropic", base_url="https://a"
        )
    )[OPENCODE_PROVIDER_ID]
    assert an["npm"] == "@ai-sdk/anthropic"
    assert an["options"]["apiKey"] == "{env:ANTHROPIC_API_KEY}"


def test_build_command_threads_runtime_model() -> None:
    # The model resolved from the role's provider at run time reaches the
    # CLI as --model; without it OpenCode uses its own built-in default.
    cmd = OpenCodeRunner()._build_command(
        "fix",
        RunLimits(),
        llm=CodingAgentLLM(
            model="acme/model-x-large",
            provider_type="openai-compatible",
            base_url="https://api.x/v1",
        ),
    )
    assert f"--model {OPENCODE_PROVIDER_ID}/acme/model-x-large" in cmd
    assert "--format json" in cmd


def _events(*objs: dict) -> str:
    return "\n".join(json.dumps(o) for o in objs)


def test_parse_json_events_extracts_text_transcript_and_refs() -> None:
    stdout = _events(
        {"type": "step_start", "sessionID": "s"},
        {"type": "tool_use", "sessionID": "s", "part": {"tool": "bash"}},
        {
            "type": "text",
            "sessionID": "s",
            "part": {
                "type": "text",
                "text": "Opened https://github.com/acme/repo/pull/9",
                "time": {"end": 1},
            },
        },
        {"type": "session.status", "properties": {"status": {"type": "idle"}}},
    )
    r = parse_opencode_result(stdout)
    assert r.success is True
    assert "Opened" in r.text
    assert r.delivered_refs == ["https://github.com/acme/repo/pull/9"]
    assert "[tool] bash" in r.transcript
    assert "Opened" in r.transcript
    # No fabricated token/cost.
    assert r.input_tokens == 0 and r.cost_usd == 0.0


def test_transcript_enriched_with_command_and_tool_error() -> None:
    # A bare "[tool] bash" list can't explain a failed run. The transcript
    # surfaces the bash command and any tool-level error, so a failure is
    # diagnosable on the dashboard.
    stdout = _events(
        {
            "type": "tool_use",
            "part": {
                "tool": "bash",
                "state": {"status": "completed", "input": {"command": "pytest -q"}},
            },
        },
        {
            "type": "tool_use",
            "part": {
                "tool": "bash",
                "state": {
                    "status": "error",
                    "input": {"command": "pip install -e .[dev]"},
                    "error": "ERROR: No matching distribution found for foo",
                },
            },
        },
        {"type": "step_finish", "part": {"reason": "stop"}},
    )
    r = parse_opencode_result(stdout)
    assert "[tool] bash: pytest -q" in r.transcript
    assert "pip install -e .[dev]" in r.transcript
    assert "error: ERROR: No matching distribution" in r.transcript


def test_transcript_falls_back_to_bare_name_without_input() -> None:
    # Older / sparse events with only the tool name still render.
    stdout = _events(
        {"type": "tool_use", "part": {"tool": "read"}},
        {"type": "step_finish", "part": {"reason": "stop"}},
    )
    r = parse_opencode_result(stdout)
    assert "[tool] read" in r.transcript


def test_parse_error_event_is_failure() -> None:
    stdout = _events(
        {"type": "step_start", "sessionID": "s"},
        {"type": "error", "sessionID": "s", "error": "model exploded"},
        {"type": "session.status", "properties": {"status": {"type": "idle"}}},
    )
    r = parse_opencode_result(stdout)
    assert r.success is False
    assert "model exploded" in r.error


def test_stream_done_detects_terminal_events() -> None:
    # Mid-run: an intermediate step_finish carries reason "tool-calls" → not
    # done.
    mid = _events(
        {"type": "text", "part": {"text": "working"}},
        {"type": "tool_use", "part": {"tool": "bash"}},
        {"type": "step_finish", "part": {"reason": "tool-calls"}},
    )
    assert opencode_stream_done(mid) is False
    # The terminal step_finish (reason "stop") → done. This is what
    # `opencode run --format json` actually emits (v1.17.x) when the assistant
    # produced its final message and stopped — even though it then hangs.
    stop = mid + "\n" + json.dumps({"type": "step_finish", "part": {"reason": "stop"}})
    assert opencode_stream_done(stop) is True
    # A session.status idle event (newer builds) also ends the run.
    idle = (
        mid
        + "\n"
        + json.dumps(
            {"type": "session.status", "properties": {"status": {"type": "idle"}}}
        )
    )
    assert opencode_stream_done(idle) is True
    # An error event also ends the run.
    err = mid + "\n" + json.dumps({"type": "error", "error": "boom"})
    assert opencode_stream_done(err) is True


async def test_poll_detects_terminal_step_without_marker() -> None:
    # OpenCode hangs (no done-marker), but the terminal step_finish(stop) in
    # the streamed result.json completes the run.
    sb = FakeSandbox()
    runner = OpenCodeRunner()
    await sb.write_file(
        RESULT_PATH,
        _events(
            {"type": "text", "part": {"text": "hi"}},
            {"type": "step_finish", "part": {"reason": "tool-calls"}},
        ),
    )
    assert await runner.poll(sb, RunHandle(command_id="p")) is False
    await sb.write_file(
        RESULT_PATH,
        _events(
            {"type": "text", "part": {"text": "hi"}},
            {"type": "step_finish", "part": {"reason": "stop"}},
        ),
    )
    assert await runner.poll(sb, RunHandle(command_id="p")) is True


async def test_poll_fires_on_dead_process_with_non_terminal_stream() -> None:
    # The exact stuck case the user hit: OpenCode's last streamed event is a
    # step_finish(reason="tool-calls") (e.g. a tool that got rejected) with no
    # terminal stop/idle/error, so _result_done is False, and a never-exited
    # process means no done-marker. With no run-time TTL, the run would hang
    # forever — process liveness completes it once the wrapper is gone.
    sb = FakeSandbox()
    runner = OpenCodeRunner()
    await sb.write_file(
        RESULT_PATH,
        _events(
            {"type": "text", "part": {"text": "trying"}},
            {"type": "step_finish", "part": {"reason": "tool-calls"}},
        ),
    )
    assert await runner.poll(sb, RunHandle(command_id="pid9")) is False
    sb.dead_pids.add("pid9")
    assert await runner.poll(sb, RunHandle(command_id="pid9")) is True


async def test_start_writes_custom_provider_config() -> None:
    # The custom-endpoint model lands in opencode.json as a custom provider
    # so OpenCode reaches the role's gateway instead of api.openai.com.
    sb = FakeSandbox()
    llm = CodingAgentLLM(
        model="acme/model-x-large",
        provider_type="openai-compatible",
        base_url="https://api.x/v1",
    )
    await OpenCodeRunner().start(sb, brief="go", env={}, limits=RunLimits(), llm=llm)
    cfg = json.loads((await sb.read_file(OPENCODE_CONFIG_PATH)).decode())
    provider = cfg["provider"][OPENCODE_PROVIDER_ID]
    assert provider["options"]["baseURL"] == "https://api.x/v1"
    assert "acme/model-x-large" in provider["models"]
    assert f"--model {OPENCODE_PROVIDER_ID}/acme/model-x-large" in sb.background[0]


def test_parse_takes_text_and_finds_pr_leaves_tokens_zero() -> None:
    out = "Worked on it.\nOpened https://github.com/acme/repo/pull/8"
    r = parse_opencode_result(out)
    assert r.success is True
    assert "Worked on it" in r.text
    assert r.delivered_refs == ["https://github.com/acme/repo/pull/8"]
    # OpenCode has no stable token/cost envelope — honest zeros.
    assert r.input_tokens == 0
    assert r.output_tokens == 0
    assert r.cost_usd == 0.0


def test_parse_empty_is_failure() -> None:
    assert parse_opencode_result("").success is False


async def test_runner_name_and_detached_collect() -> None:
    runner = OpenCodeRunner()
    assert runner.name == "opencode"
    sb = FakeSandbox()
    # the shared base installs the ask shim + work dir (git auth is a
    # manager-applied setup step, not runner install plumbing)
    await runner.install(sb)
    assert any("mkdir -p" in c for c in sb.commands)

    # detached: start backgrounds an opencode-run script
    await runner.start(sb, brief="implement", env={}, limits=RunLimits())
    assert any("opencode run " in c and "implement" in c for c in sb.background)
    # The done-marker is written (carries the exit code), not `touch`ed —
    # a zero-byte marker is invisible to the completion poll.
    assert any(f"> {DONE_MARKER}" in c for c in sb.background)
    assert all("touch " not in c for c in sb.background)

    # collect parses the result file the job would have written
    await sb.write_file(RESULT_PATH, "Done. https://github.com/a/b/pull/2")
    r = await runner.collect(sb, RunHandle(command_id="p"))
    assert r.success is True
    assert r.delivered_refs == ["https://github.com/a/b/pull/2"]


async def test_write_config_always_disables_share_and_autoupdate() -> None:
    # Headless hygiene is written even with no provider/MCP: a session-share
    # connection can keep OpenCode alive past the run (and would exfil the
    # session to OpenCode's cloud); self-update mid-job is unwanted.
    sb = FakeSandbox()
    await OpenCodeRunner().start(sb, brief="x", env={}, limits=RunLimits())
    cfg = json.loads((await sb.read_file(OPENCODE_CONFIG_PATH)).decode())
    assert cfg["share"] == "disabled"
    assert cfg["autoupdate"] is False


async def test_write_config_grants_blanket_permission() -> None:
    # The OpenCode equivalent of Claude Code's bypassPermissions: a blanket
    # "allow" so headless `opencode run` never auto-rejects a permission it
    # can't prompt for — notably `external_directory` for a checkout that
    # lives outside OpenCode's cwd (the repo cloned under /tmp).
    sb = FakeSandbox()
    await OpenCodeRunner().start(sb, brief="x", env={}, limits=RunLimits())
    cfg = json.loads((await sb.read_file(OPENCODE_CONFIG_PATH)).decode())
    assert cfg["permission"] == "allow"


async def test_collect_surfaces_exit_code_when_opencode_dies() -> None:
    # OpenCode crashed (e.g. ProviderModelNotFoundError) before writing a
    # result file. collect surfaces the non-zero exit so the run doesn't
    # stall silently — the very symptom that motivated the marker fix.
    sb = FakeSandbox()
    await sb.write_file(EXIT_PATH, "1")
    r = await OpenCodeRunner().collect(sb, RunHandle(command_id="p"))
    assert r.success is False
    assert "exited with status 1" in r.error


async def test_runner_ask_overlay_shared_with_base() -> None:
    from crewlet.sandbox.coding_agents._detached import ASK_PATH

    sb = FakeSandbox()
    await sb.write_file(RESULT_PATH, "blocked")
    await sb.write_file(ASK_PATH, json.dumps({"question": "Which?", "to": "manager"}))
    r = await OpenCodeRunner().collect(sb, RunHandle(command_id="p"))
    assert r.needs_input is True
    assert r.ask_to == "manager"


async def test_collect_findings_file_rescues_tool_only_run() -> None:
    # The real failure: OpenCode streamed only tool_use events and never a
    # terminal text event, so parse_opencode_result yields success=False,
    # text="", error="no assistant output" — the engine had nothing to report.
    # The findings file the agent was told to write is the authoritative
    # result: collect prefers it and the run reports as a success.
    sb = FakeSandbox()
    await sb.write_file(
        RESULT_PATH,
        _events(
            {"type": "tool_use", "part": {"tool": "bash"}},
            {"type": "tool_use", "part": {"tool": "read"}},
            {"type": "step_finish", "part": {"reason": "stop"}},
        ),
    )
    # Sanity: the stream alone is a "no assistant output" failure.
    assert parse_opencode_result(await _read(sb, RESULT_PATH)).success is False
    await sb.write_file(
        FINDINGS_PATH,
        "Outcome: blocked. CI fails because `pytest-cov` is missing from the "
        "dev deps. PR: https://github.com/acme/repo/pull/7",
    )
    r = await OpenCodeRunner().collect(sb, RunHandle(command_id="p"))
    assert r.success is True
    assert "pytest-cov" in r.text
    assert r.delivered_refs == ["https://github.com/acme/repo/pull/7"]


async def test_collect_findings_does_not_mask_a_crash() -> None:
    # A non-zero exit means the process itself crashed; a stale/partial
    # findings file must not flip that to success.
    sb = FakeSandbox()
    await sb.write_file(RESULT_PATH, "")
    await sb.write_file(EXIT_PATH, "1")
    await sb.write_file(FINDINGS_PATH, "partial notes before the crash")
    r = await OpenCodeRunner().collect(sb, RunHandle(command_id="p"))
    assert r.success is False
    # The findings text is still surfaced (better than nothing to report).
    assert "partial notes" in r.text


async def _read(sb: FakeSandbox, path: str) -> str:
    raw = await sb.read_file(path)
    return raw.decode() if isinstance(raw, bytes | bytearray) else str(raw)
