"""Tests for the ask shim + MCP scoping helpers."""

from __future__ import annotations

import json

from crewlet.sandbox.coding_agents.ask import (
    ASK_OUTPUT_PATH,
    ASK_SHIM_PATH,
    ask_brief_instruction,
    build_ask_shim,
    findings_brief_instruction,
    mcp_config_json,
    render_mcp_config,
)
from crewlet.sandbox.coding_agents.claude_code import ASK_PATH, ClaudeCodeRunner
from crewlet.sandbox.fake import FakeSandbox


def test_shim_writes_to_the_path_the_runner_reads() -> None:
    # The shim's output path must equal where collect looks for ask.json.
    assert ASK_OUTPUT_PATH == ASK_PATH
    shim = build_ask_shim()
    assert "argparse" in shim
    assert "--to" in shim
    assert ASK_OUTPUT_PATH in shim


def test_ask_brief_instruction_names_audiences_and_people() -> None:
    text = ask_brief_instruction(roster=["alice", "bob"], manager="carol")
    assert "crewlet-ask" in text
    assert "requester" in text and "team" in text and "manager" in text
    assert "alice, bob" in text
    assert "carol" in text


def test_ask_brief_instruction_works_without_names() -> None:
    text = ask_brief_instruction()
    assert "crewlet-ask" in text
    assert "Teammates you can name" not in text


def test_findings_brief_instruction_names_the_report_file() -> None:
    from crewlet.sandbox.coding_agents._detached import FINDINGS_PATH

    text = findings_brief_instruction(FINDINGS_PATH)
    # Frames the agent as a step in a larger turn, not the last word …
    assert "NOT the last step" in text
    # … and points it at the exact file the runner reads back on collect.
    assert FINDINGS_PATH in text


def test_render_mcp_config_scopes_to_named_servers() -> None:
    specs = {
        "github": {"type": "http", "url": "https://gh", "headers": {"A": "x"}},
        "slack": {"command": "slack-mcp", "args": []},
        "secret": {"command": "nope"},
    }
    cfg = render_mcp_config(specs, servers=["github", "slack"])
    assert set(cfg["mcpServers"]) == {"github", "slack"}
    assert "secret" not in cfg["mcpServers"]
    # round-trips as JSON for writing into the sandbox
    assert json.loads(mcp_config_json(specs, ["github"]))["mcpServers"]["github"]["url"]


async def test_install_writes_ask_shim() -> None:
    sb = FakeSandbox()
    await ClaudeCodeRunner().install(sb)
    # the shim file was written and made executable
    written = await sb.read_file(ASK_SHIM_PATH)
    assert b"argparse" in written
    assert any(f"chmod +x {ASK_SHIM_PATH}" in c for c in sb.commands)
