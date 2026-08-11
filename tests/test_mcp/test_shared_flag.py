"""Tests for MCPServerConfig.shared flag in engine startup."""

from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from pydantic import ValidationError

from crewlet.config import MCPServerConfig
from crewlet.engine import Engine
from crewlet.org.models import Organization, OrgUnit, Role


def test_mcp_server_config_rejects_empty_name():
    """The MCP server name is the source of truth for several things:
    the MCP client connection key, the per-role instance prefix in
    ``mcp_instance_name``, and (via ``MCPToolWrapper.server_name``)
    the prompt-rendered server name. An empty string would silently
    propagate through all three; reject it at config-load time.
    """
    with pytest.raises(ValidationError):
        MCPServerConfig(name="", command="uvx", args=["mcp-foo"])


def test_mcp_server_config_parses_annotation_overrides():
    """``tool_annotations`` (operator config for servers that
    under-advertise MCP hints) parses into ToolAnnotations keyed by bare
    tool name."""
    cfg = MCPServerConfig(
        name="linear",
        command="uvx",
        args=["mcp-linear"],
        tool_annotations={
            "linear_create_comment": {"read_only": False, "open_world": True},
            "linear_get_issue": {"readOnlyHint": True},
        },
    )
    overrides = cfg.annotation_overrides()
    assert overrides["linear_create_comment"].read_only is False
    assert overrides["linear_create_comment"].open_world is True
    # camelCase MCP keys parse too.
    assert overrides["linear_get_issue"].read_only is True


def test_mcp_server_config_no_annotation_overrides_by_default():
    cfg = MCPServerConfig(name="calc", command="uvx", args=["mcp-calc"])
    assert cfg.annotation_overrides() == {}


def _make_org(roles: list[Role] | None = None) -> Organization:
    if roles is None:
        roles = [
            Role(
                name="Engineer",
                mcp_env={
                    "atlassian": {"JIRA_API_TOKEN": "eng-tok"},
                },
            ),
        ]
    return Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead=roles[0].name,
                roles=roles,
            ),
        ],
    )


def _make_mock_tool(name: str = "jira_search") -> MagicMock:
    tool = MagicMock()
    tool.name = name
    return tool


@pytest.mark.asyncio
async def test_shared_false_skips_global_launch():
    """shared=false servers are not launched globally."""
    org = _make_org()
    engine = Engine(organization=org)
    engine._mcp_configs = [
        MCPServerConfig(
            name="atlassian",
            shared=False,
            command="uvx",
            args=["mcp-atlassian"],
            env={"JIRA_URL": "https://co.atlassian.net"},
        ),
    ]

    mock_tool = _make_mock_tool()

    with patch.object(Engine, "_start_mcp_servers", wraps=engine._start_mcp_servers):
        from crewlet.mcp.bridge import MCPToolBridge

        mock_bridge = MagicMock(spec=MCPToolBridge)
        mock_bridge.add_server = AsyncMock(return_value=[mock_tool])

        engine.mcp_bridge = mock_bridge
        # Patch MCPToolBridge() constructor to return our mock
        with patch("crewlet.engine.MCPToolBridge", return_value=mock_bridge):
            await engine._start_mcp_servers()

        # Global add_server should NOT have been called with
        # the bare "atlassian" name
        for call in mock_bridge.add_server.call_args_list:
            assert (
                call.kwargs.get("name", call.args[0] if call.args else "")
                != "atlassian"
            ), "shared=false server should not be launched globally"

        # Per-role instance SHOULD have been launched
        per_role_names = [
            call.kwargs.get("name", "")
            for call in mock_bridge.add_server.call_args_list
        ]
        assert any("atlassian::Engineer" in n for n in per_role_names), (
            f"Expected per-role instance, got: {per_role_names}"
        )


@pytest.mark.asyncio
async def test_shared_true_launches_global():
    """shared=true (default) servers are launched globally."""
    org = _make_org(
        roles=[Role(name="Engineer")],  # no mcp_env
    )
    engine = Engine(organization=org)
    engine._mcp_configs = [
        MCPServerConfig(
            name="calculator",
            command="uvx",
            args=["mcp-calculator"],
            # shared=True is the default
        ),
    ]

    mock_tool = _make_mock_tool(name="calculate")

    from crewlet.mcp.bridge import MCPToolBridge

    mock_bridge = MagicMock(spec=MCPToolBridge)
    mock_bridge.add_server = AsyncMock(return_value=[mock_tool])

    with patch("crewlet.engine.MCPToolBridge", return_value=mock_bridge):
        await engine._start_mcp_servers()

    # Global launch should have been called
    global_names = [
        call.kwargs.get("name", "") for call in mock_bridge.add_server.call_args_list
    ]
    assert "calculator" in global_names


@pytest.mark.asyncio
async def test_shared_false_http_spawns_per_role():
    """shared=false + http transport now spawns a per-role instance —
    the role's ``mcp_env`` overrides become HTTP headers (this is how
    the remote GitHub Copilot MCP gets a per-agent token)."""
    org = _make_org(
        roles=[
            Role(
                name="Engineer",
                mcp_env={"github": {"Authorization": "Bearer eng-tok"}},
            ),
        ],
    )
    engine = Engine(organization=org)
    engine._mcp_configs = [
        MCPServerConfig(
            name="github",
            shared=False,
            transport="http",
            url="https://api.githubcopilot.com/mcp/",
        ),
    ]

    from crewlet.mcp.bridge import MCPToolBridge

    mock_bridge = MagicMock(spec=MCPToolBridge)
    mock_bridge.add_server = AsyncMock(return_value=[])
    mock_bridge.add_http_server = AsyncMock(return_value=[_make_mock_tool("get_me")])

    with patch("crewlet.engine.MCPToolBridge", return_value=mock_bridge):
        await engine._start_mcp_servers()

    # Not launched globally (no bare "github" instance).
    global_http = [
        c.kwargs.get("name", "") for c in mock_bridge.add_http_server.call_args_list
    ]
    assert "github" not in global_http
    # A single per-role http instance launched, with the role's mcp_env
    # applied as an Authorization header.
    assert len(mock_bridge.add_http_server.call_args_list) == 1
    call = mock_bridge.add_http_server.call_args_list[0]
    assert call.kwargs["name"] == "github::Engineer"
    assert call.kwargs["url"] == "https://api.githubcopilot.com/mcp/"
    assert call.kwargs["headers"]["Authorization"] == "Bearer eng-tok"
    # The stdio path is untouched for an http template.
    mock_bridge.add_server.assert_not_called()
