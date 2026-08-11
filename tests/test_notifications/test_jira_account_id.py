"""Tests for Jira accountId resolution via MCP and webhook routing registration."""

from __future__ import annotations

import json
from unittest.mock import AsyncMock, MagicMock

from crewlet.config import (
    _resolve_jira_account_via_mcp,
    register_jira_accounts_from_org,
)

# --- _resolve_jira_account_via_mcp ---


def _mock_mcp_bridge(
    instance_responses: dict[str, list[dict]],
) -> MagicMock:
    """Create a mock MCP bridge with per-instance call_tool responses.

    ``instance_responses`` maps instance names to the content blocks
    that ``call_tool("jira_get_user_profile", ...)`` should return.
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


async def test_resolve_via_mcp_json_account_id() -> None:
    """JSON response with account_id (in user envelope) is extracted."""
    response = {
        "success": True,
        "user": {"account_id": "712020:abc-123", "display_name": "Alice"},
    }
    bridge = _mock_mcp_bridge(
        {"atlassian::Engineer": [{"type": "text", "text": json.dumps(response)}]}
    )
    result = await _resolve_jira_account_via_mcp(
        bridge, "atlassian::Engineer", jira_username="alice@example.com"
    )
    assert result == "712020:abc-123"


async def test_resolve_via_mcp_json_accountId_field() -> None:
    """JSON response with accountId (camelCase) is extracted."""
    response = {
        "success": True,
        "user": {"accountId": "712020:cloud-id", "display_name": "Alice"},
    }
    bridge = _mock_mcp_bridge(
        {"atlassian::Engineer": [{"type": "text", "text": json.dumps(response)}]}
    )
    result = await _resolve_jira_account_via_mcp(
        bridge, "atlassian::Engineer", jira_username="alice@example.com"
    )
    assert result == "712020:cloud-id"


async def test_resolve_via_mcp_key_fallback() -> None:
    """JSON response with key (Data Center) is used as fallback."""
    response = {
        "success": True,
        "user": {"display_name": "Alice", "name": "alice", "key": "alice-key"},
    }
    bridge = _mock_mcp_bridge(
        {"atlassian::Engineer": [{"type": "text", "text": json.dumps(response)}]}
    )
    result = await _resolve_jira_account_via_mcp(
        bridge, "atlassian::Engineer", jira_username="alice@example.com"
    )
    assert result == "alice-key"


async def test_resolve_via_mcp_name_fallback() -> None:
    """JSON response with only name is used as last fallback."""
    response = {
        "success": True,
        "user": {"display_name": "Alice", "name": "alice"},
    }
    bridge = _mock_mcp_bridge(
        {"atlassian::Engineer": [{"type": "text", "text": json.dumps(response)}]}
    )
    result = await _resolve_jira_account_via_mcp(
        bridge, "atlassian::Engineer", jira_username="alice@example.com"
    )
    assert result == "alice"


async def test_resolve_via_mcp_flat_json() -> None:
    """JSON response without user envelope is also handled."""
    profile = {"account_id": "712020:flat-id", "display_name": "Alice"}
    bridge = _mock_mcp_bridge(
        {"atlassian::Engineer": [{"type": "text", "text": json.dumps(profile)}]}
    )
    result = await _resolve_jira_account_via_mcp(
        bridge, "atlassian::Engineer", jira_username="alice@example.com"
    )
    assert result == "712020:flat-id"


async def test_resolve_via_mcp_no_client() -> None:
    """Missing MCP client returns empty string."""
    bridge = _mock_mcp_bridge({})
    result = await _resolve_jira_account_via_mcp(
        bridge, "atlassian::Missing", jira_username="alice@example.com"
    )
    assert result == ""


async def test_resolve_via_mcp_call_tool_error() -> None:
    """MCP call_tool exception returns empty string."""
    bridge = MagicMock()
    client = AsyncMock()
    client.call_tool = AsyncMock(side_effect=RuntimeError("MCP error"))
    bridge.get_client = MagicMock(return_value=client)
    result = await _resolve_jira_account_via_mcp(
        bridge, "atlassian::Engineer", jira_username="alice@example.com"
    )
    assert result == ""


async def test_resolve_via_mcp_empty_response() -> None:
    """Empty response returns empty string."""
    bridge = _mock_mcp_bridge({"atlassian::Engineer": [{"type": "text", "text": "{}"}]})
    result = await _resolve_jira_account_via_mcp(
        bridge, "atlassian::Engineer", jira_username="alice@example.com"
    )
    assert result == ""


async def test_resolve_via_mcp_no_username() -> None:
    """Missing jira_username returns empty string without calling MCP."""
    bridge = _mock_mcp_bridge(
        {
            "atlassian::Engineer": [
                {"type": "text", "text": json.dumps({"account_id": "x"})}
            ]
        }
    )
    result = await _resolve_jira_account_via_mcp(
        bridge, "atlassian::Engineer", jira_username=""
    )
    assert result == ""


async def test_resolve_prefers_account_id_over_name() -> None:
    """When both account_id and name are present, account_id wins."""
    response = {
        "success": True,
        "user": {"account_id": "712020:cloud-id", "name": "alice", "key": "alice-key"},
    }
    bridge = _mock_mcp_bridge(
        {"atlassian::Engineer": [{"type": "text", "text": json.dumps(response)}]}
    )
    result = await _resolve_jira_account_via_mcp(
        bridge, "atlassian::Engineer", jira_username="alice@example.com"
    )
    assert result == "712020:cloud-id"


# --- register_jira_accounts_from_org ---


def _make_role(
    name: str,
    handle: str,
    has_atlassian: bool = True,
    jira_username: str = "user@example.com",
) -> MagicMock:
    role = MagicMock()
    role.name = name
    role.get_handle.return_value = handle
    role.mcp_env = (
        {"atlassian": {"JIRA_PROJECT_KEY": "DEV", "JIRA_USERNAME": jira_username}}
        if has_atlassian
        else {}
    )
    return role


async def test_register_single_role() -> None:
    """Single role with atlassian env gets registered via MCP."""
    role = _make_role("Engineer", "engineer")
    org = MagicMock()
    org.all_roles.return_value = [role]
    handle_registry = MagicMock()

    profile = {"account_id": "712020:eng-account"}
    bridge = _mock_mcp_bridge(
        {"atlassian::Engineer": [{"type": "text", "text": json.dumps(profile)}]}
    )

    count = await register_jira_accounts_from_org(
        handle_registry, org, mcp_bridge=bridge
    )

    assert count == 1
    assert handle_registry.register_external_id.call_count == 2
    handle_registry.register_external_id.assert_any_call(
        "jira", "712020:eng-account", "engineer"
    )
    handle_registry.register_external_id.assert_any_call(
        "confluence", "712020:eng-account", "engineer"
    )


async def test_register_multiple_unique_accounts() -> None:
    """Multiple roles with distinct identities all get registered."""
    role1 = _make_role("Lead", "lead")
    role2 = _make_role("Engineer", "engineer")
    org = MagicMock()
    org.all_roles.return_value = [role1, role2]
    handle_registry = MagicMock()

    bridge = _mock_mcp_bridge(
        {
            "atlassian::Lead": [
                {"type": "text", "text": json.dumps({"account_id": "account:lead-111"})}
            ],
            "atlassian::Engineer": [
                {"type": "text", "text": json.dumps({"account_id": "account:eng-222"})}
            ],
        }
    )

    count = await register_jira_accounts_from_org(
        handle_registry, org, mcp_bridge=bridge
    )

    assert count == 2


async def test_shared_account_skips() -> None:
    """Multiple roles resolving to same accountId are skipped."""
    role1 = _make_role("Engineer", "engineer")
    role2 = _make_role("PM", "pm")
    org = MagicMock()
    org.all_roles.return_value = [role1, role2]
    handle_registry = MagicMock()

    shared_profile = json.dumps({"account_id": "712020:shared"})
    bridge = _mock_mcp_bridge(
        {
            "atlassian::Engineer": [{"type": "text", "text": shared_profile}],
            "atlassian::PM": [{"type": "text", "text": shared_profile}],
        }
    )

    count = await register_jira_accounts_from_org(
        handle_registry, org, mcp_bridge=bridge
    )

    assert count == 0
    handle_registry.register_external_id.assert_not_called()


async def test_no_atlassian_env_skipped() -> None:
    """Roles without atlassian mcp_env are skipped."""
    role = _make_role("Designer", "designer", has_atlassian=False)
    org = MagicMock()
    org.all_roles.return_value = [role]
    handle_registry = MagicMock()
    bridge = _mock_mcp_bridge({})

    count = await register_jira_accounts_from_org(
        handle_registry, org, mcp_bridge=bridge
    )

    assert count == 0


async def test_resolve_failure_skipped() -> None:
    """Failed MCP call → role skipped gracefully."""
    role = _make_role("Engineer", "engineer")
    org = MagicMock()
    org.all_roles.return_value = [role]
    handle_registry = MagicMock()

    # Bridge has no client for this instance → returns ""
    bridge = _mock_mcp_bridge({})

    count = await register_jira_accounts_from_org(
        handle_registry, org, mcp_bridge=bridge
    )

    assert count == 0
    handle_registry.register_external_id.assert_not_called()
