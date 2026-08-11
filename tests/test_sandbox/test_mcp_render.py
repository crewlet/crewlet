"""Tests for the in-sandbox MCP scoping: render the role's
servers + creds into per-agent launch specs, and the runners writing them."""

from __future__ import annotations

import json

from crewlet.config import MCPServerConfig
from crewlet.sandbox.coding_agents._detached import MCP_CONFIG_PATH
from crewlet.sandbox.coding_agents.claude_code import ClaudeCodeRunner
from crewlet.sandbox.coding_agents.opencode import OPENCODE_CONFIG_PATH, OpenCodeRunner
from crewlet.sandbox.fake import FakeSandbox
from crewlet.sandbox.mcp_render import resolve_sandbox_mcp, to_launch_spec
from crewlet.sandbox.protocol import RunLimits


def test_to_launch_spec_http_merges_role_headers(
    monkeypatch,
) -> None:
    monkeypatch.setenv("GH_PAT", "ghp_xyz")
    cfg = MCPServerConfig(name="github", transport="http", url="https://gh/mcp")
    spec = to_launch_spec(cfg, {"Authorization": "Bearer ${GH_PAT}"})
    assert spec["type"] == "http"
    assert spec["url"] == "https://gh/mcp"
    # role creds become headers and ${ENV} resolves
    assert spec["headers"]["Authorization"] == "Bearer ghp_xyz"


def test_to_launch_spec_stdio_merges_role_env(monkeypatch) -> None:
    monkeypatch.setenv("JIRA_TOKEN", "jt-1")
    cfg = MCPServerConfig(
        name="atlassian", command="uvx", args=["mcp-atlassian"], env={"JIRA_URL": "u"}
    )
    spec = to_launch_spec(cfg, {"JIRA_API_TOKEN": "${JIRA_TOKEN}"})
    assert spec["command"] == "uvx"
    assert spec["args"] == ["mcp-atlassian"]
    assert spec["env"]["JIRA_URL"] == "u"
    assert spec["env"]["JIRA_API_TOKEN"] == "jt-1"


def test_resolve_scopes_to_named_servers(monkeypatch) -> None:
    monkeypatch.setenv("GH_PAT", "g")
    configs = [
        MCPServerConfig(name="github", transport="http", url="https://gh"),
        MCPServerConfig(name="atlassian", command="uvx", args=["mcp-atlassian"]),
        MCPServerConfig(name="secret", command="nope"),
    ]
    role_mcp_env = {"github": {"Authorization": "Bearer ${GH_PAT}"}}
    specs = resolve_sandbox_mcp(
        configs,
        role_mcp_env,
        servers=["github", "atlassian", "unknown"],
    )
    # only the role's scoped + engine-known servers (server-level scoping only;
    # no per-tool allowlist)
    assert set(specs) == {"github", "atlassian"}
    assert "secret" not in specs
    assert specs["github"]["headers"]["Authorization"] == "Bearer g"


async def test_claude_runner_writes_mcp_json_and_flags() -> None:
    sb = FakeSandbox()
    runner = ClaudeCodeRunner()
    servers = {"github": {"type": "http", "url": "https://gh"}}
    await runner.start(
        sb,
        brief="implement",
        env={},
        limits=RunLimits(),
        mcp_servers=servers,
    )
    # .mcp.json written with the scoped servers
    written = json.loads((await sb.read_file(MCP_CONFIG_PATH)).decode())
    assert written["mcpServers"] == servers
    # the background script wires the flags (the outer `sh -lc` re-quotes,
    # so check content rather than the exact inner quoting)
    script = sb.background[0]
    assert f"--mcp-config {MCP_CONFIG_PATH}" in script
    assert "--strict-mcp-config" in script
    # No per-tool allowlist — bypassPermissions grants all tools on the
    # scoped servers (the mcp.allow feature was dropped).
    assert "--allowedTools" not in script


async def test_claude_runner_no_mcp_no_flags() -> None:
    sb = FakeSandbox()
    await ClaudeCodeRunner().start(sb, brief="x", env={}, limits=RunLimits())
    assert (await sb.read_file(MCP_CONFIG_PATH)) == b""
    assert "--mcp-config" not in sb.background[0]


async def test_opencode_runner_writes_translated_config() -> None:
    sb = FakeSandbox()
    servers = {
        "github": {"type": "http", "url": "https://gh", "headers": {"A": "x"}},
        "atlassian": {"command": "uvx", "args": ["mcp-atlassian"], "env": {"K": "v"}},
    }
    await OpenCodeRunner().start(
        sb, brief="x", env={}, limits=RunLimits(), mcp_servers=servers
    )
    cfg = json.loads((await sb.read_file(OPENCODE_CONFIG_PATH)).decode())
    assert cfg["mcp"]["github"]["type"] == "remote"
    assert cfg["mcp"]["github"]["url"] == "https://gh"
    assert cfg["mcp"]["atlassian"]["type"] == "local"
    assert cfg["mcp"]["atlassian"]["command"] == ["uvx", "mcp-atlassian"]
    assert cfg["mcp"]["atlassian"]["environment"] == {"K": "v"}
    # OpenCode auto-loads its config — no --mcp-config on the CLI
    assert "--mcp-config" not in sb.background[0]
