"""Tests for GitHub username resolution via MCP and webhook routing registration."""

from __future__ import annotations

import json
from unittest.mock import AsyncMock, MagicMock

import pytest

from crewlet.config import (
    _resolve_github_login_via_mcp,
    register_github_accounts_from_org,
)

# --- helpers ---


def _mock_mcp_bridge(
    instance_responses: dict[str, list[dict]],
) -> MagicMock:
    """Create a mock MCP bridge with per-instance call_tool responses.

    ``instance_responses`` maps instance names to the content blocks
    that ``call_tool("get_me", {})`` should return.
    """
    bridge = MagicMock()
    clients: dict[str, AsyncMock] = {}
    for name, blocks in instance_responses.items():
        client = AsyncMock()
        client.call_tool = AsyncMock(return_value=blocks)
        clients[name] = client

    def get_client(name: str):
        return clients.get(name)

    bridge.get_client = MagicMock(side_effect=get_client)
    return bridge


def _mock_org(roles: list[dict]) -> MagicMock:
    """Create a mock org with roles that carry per-agent GitHub creds.

    GitHub creds live in ``role.mcp_env.github`` now (an Authorization
    header for the per-role Copilot MCP); the ``github`` key in each
    role dict is routed there.
    """
    mock_roles = []
    for r in roles:
        role = MagicMock()
        role.name = r["name"]
        gh = r.get("github")
        role.mcp_env = {"github": gh} if gh is not None else {}
        role.get_handle.return_value = r["handle"]
        mock_roles.append(role)

    org = MagicMock()
    org.all_roles.return_value = mock_roles
    return org


# --- _resolve_github_login_via_mcp ---


@pytest.mark.asyncio
async def test_resolve_via_mcp_returns_login() -> None:
    """JSON response with login is extracted."""
    response = {"login": "ali-swe", "id": 12345}
    bridge = _mock_mcp_bridge(
        {"github::Agent_SWE": [{"type": "text", "text": json.dumps(response)}]}
    )
    result = await _resolve_github_login_via_mcp(bridge, "github::Agent_SWE")
    assert result == "ali-swe"


@pytest.mark.asyncio
async def test_resolve_via_mcp_missing_client() -> None:
    """Missing MCP client returns empty string."""
    bridge = _mock_mcp_bridge({})
    result = await _resolve_github_login_via_mcp(bridge, "github::Missing")
    assert result == ""


@pytest.mark.asyncio
async def test_resolve_via_mcp_tool_error() -> None:
    """MCP tool error returns empty string."""
    bridge = MagicMock()
    client = AsyncMock()
    client.call_tool = AsyncMock(side_effect=RuntimeError("connection lost"))
    bridge.get_client = MagicMock(return_value=client)

    result = await _resolve_github_login_via_mcp(bridge, "github::Agent_SWE")
    assert result == ""


@pytest.mark.asyncio
async def test_resolve_via_mcp_no_login_in_response() -> None:
    """Response without login field returns empty string."""
    response = {"id": 12345, "name": "Some User"}
    bridge = _mock_mcp_bridge(
        {"github::Agent_SWE": [{"type": "text", "text": json.dumps(response)}]}
    )
    result = await _resolve_github_login_via_mcp(bridge, "github::Agent_SWE")
    assert result == ""


# --- register_github_accounts_from_org ---


@pytest.mark.asyncio
async def test_register_github_account_success() -> None:
    """GitHub login is resolved via MCP and registered as external ID."""
    response = {"login": "ali-swe", "id": 12345}
    bridge = _mock_mcp_bridge(
        {"github::Agent_SWE": [{"type": "text", "text": json.dumps(response)}]}
    )
    registry = MagicMock()
    org = _mock_org(
        [
            {
                "name": "Agent SWE",
                "handle": "agent-swe",
                "github": {"token": "fake_github_token"},
            }
        ]
    )

    count = await register_github_accounts_from_org(registry, org, mcp_bridge=bridge)
    assert count == 1
    registry.register_external_id.assert_called_once_with(
        "github", "ali-swe", "agent-swe"
    )


@pytest.mark.asyncio
async def test_register_github_skips_roles_without_github() -> None:
    """Roles without github config are skipped."""
    bridge = _mock_mcp_bridge({})
    registry = MagicMock()
    org = _mock_org(
        [
            {"name": "Agent PM", "handle": "agent-pm", "github": None},
        ]
    )

    count = await register_github_accounts_from_org(registry, org, mcp_bridge=bridge)
    assert count == 0
    registry.register_external_id.assert_not_called()


@pytest.mark.asyncio
async def test_register_github_skips_roles_with_empty_token() -> None:
    """Roles with a github block but empty token are skipped."""
    bridge = _mock_mcp_bridge({})
    registry = MagicMock()
    org = _mock_org(
        [
            {
                "name": "Agent SWE",
                "handle": "agent-swe",
                "github": {"token": ""},
            },
        ]
    )

    count = await register_github_accounts_from_org(registry, org, mcp_bridge=bridge)
    assert count == 0
    registry.register_external_id.assert_not_called()
    # Should not even attempt MCP call
    bridge.get_client.assert_not_called()


@pytest.mark.asyncio
async def test_register_github_handles_mcp_failure() -> None:
    """MCP failure is handled gracefully without crashing."""
    bridge = MagicMock()
    client = AsyncMock()
    client.call_tool = AsyncMock(side_effect=RuntimeError("server down"))
    bridge.get_client = MagicMock(return_value=client)

    registry = MagicMock()
    org = _mock_org(
        [
            {
                "name": "Agent SWE",
                "handle": "agent-swe",
                "github": {"token": "fake_github_token_bad"},
            }
        ]
    )

    count = await register_github_accounts_from_org(registry, org, mcp_bridge=bridge)
    assert count == 0
    registry.register_external_id.assert_not_called()


@pytest.mark.asyncio
async def test_register_github_shared_login_skipped() -> None:
    """When two roles share the same GitHub login, neither is registered."""
    response = {"login": "shared-user", "id": 999}
    bridge = _mock_mcp_bridge(
        {
            "github::Agent_SWE": [{"type": "text", "text": json.dumps(response)}],
            "github::Agent_CTO": [{"type": "text", "text": json.dumps(response)}],
        }
    )
    registry = MagicMock()
    org = _mock_org(
        [
            {
                "name": "Agent SWE",
                "handle": "agent-swe",
                "github": {"token": "fake_github_token_1"},
            },
            {
                "name": "Agent CTO",
                "handle": "agent-cto",
                "github": {"token": "fake_github_token_2"},
            },
        ]
    )

    count = await register_github_accounts_from_org(registry, org, mcp_bridge=bridge)
    assert count == 0
    registry.register_external_id.assert_not_called()
