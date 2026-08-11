"""Tests for per-role and per-team MCP env overrides."""

from crewlet.config import (
    _parse_role,
    _parse_unit,
    parse_mcp_servers,
    resolve_env_vars,
)
from crewlet.org.models import Role

# --- Role mcp_env ---


def test_role_mcp_env_default():
    role = Role(name="Engineer")
    assert role.mcp_env == {}


def test_role_mcp_env_from_yaml():
    role = _parse_role(
        {
            "name": "Product Manager",
            "mcp_env": {
                "atlassian": {
                    "JIRA_API_TOKEN": "pm-token-123",
                },
            },
        }
    )
    assert "atlassian" in role.mcp_env
    assert role.mcp_env["atlassian"]["JIRA_API_TOKEN"] == ("pm-token-123")


# --- Team mcp_env inheritance ---


def test_team_mcp_env_inherited_by_roles():
    """Team mcp_env is merged into each role's mcp_env."""
    team = _parse_unit(
        {
            "name": "Backend",
            "lead": "Tech Lead",
            "mcp_env": {
                "atlassian": {
                    "JIRA_PROJECT_KEY": "BACK",
                    "JIRA_URL": "https://co.atlassian.net",
                },
            },
            "roles": [
                {
                    "name": "Tech Lead",
                    "mcp_env": {
                        "atlassian": {
                            "JIRA_API_TOKEN": "lead-token",
                        },
                    },
                },
                {
                    "name": "Engineer",
                    "mcp_env": {
                        "atlassian": {
                            "JIRA_API_TOKEN": "eng-token",
                        },
                    },
                },
            ],
        }
    )

    # Both roles should inherit team's JIRA_PROJECT_KEY
    lead = team.get_role("Tech Lead")
    assert lead is not None
    assert lead.mcp_env["atlassian"]["JIRA_PROJECT_KEY"] == ("BACK")
    assert lead.mcp_env["atlassian"]["JIRA_API_TOKEN"] == ("lead-token")

    eng = team.get_role("Engineer")
    assert eng is not None
    assert eng.mcp_env["atlassian"]["JIRA_PROJECT_KEY"] == ("BACK")
    assert eng.mcp_env["atlassian"]["JIRA_API_TOKEN"] == ("eng-token")


def test_role_overrides_team_mcp_env():
    """Role-level env vars override team-level ones."""
    team = _parse_unit(
        {
            "name": "Platform",
            "lead": "SRE",
            "mcp_env": {
                "atlassian": {
                    "JIRA_PROJECT_KEY": "PLAT",
                    "JIRA_URL": "https://co.atlassian.net",
                },
            },
            "roles": [
                {
                    "name": "SRE",
                    "mcp_env": {
                        "atlassian": {
                            # SRE overrides project key
                            "JIRA_PROJECT_KEY": "SRE",
                            "JIRA_API_TOKEN": "sre-token",
                        },
                    },
                },
            ],
        }
    )

    sre = team.get_role("SRE")
    assert sre is not None
    # Role override wins
    assert sre.mcp_env["atlassian"]["JIRA_PROJECT_KEY"] == "SRE"
    # Team value inherited
    assert sre.mcp_env["atlassian"]["JIRA_URL"] == ("https://co.atlassian.net")
    # Role-only value
    assert sre.mcp_env["atlassian"]["JIRA_API_TOKEN"] == ("sre-token")


def test_team_without_mcp_env_leaves_roles_unchanged():
    team = _parse_unit(
        {
            "name": "NoEnv",
            "lead": "Dev",
            "roles": [
                {
                    "name": "Dev",
                    "mcp_env": {
                        "slack": {"BOT_TOKEN": "xoxb-123"},
                    },
                },
            ],
        }
    )
    dev = team.get_role("Dev")
    assert dev is not None
    assert dev.mcp_env == {"slack": {"BOT_TOKEN": "xoxb-123"}}


# --- MCPServerConfig ---


def test_parse_mcp_servers():
    configs = parse_mcp_servers(
        [
            {
                "name": "atlassian",
                "command": "uvx",
                "args": ["mcp-atlassian"],
                "env": {"JIRA_URL": "https://co.atlassian.net"},
            },
            {
                "name": "slack",
                "command": "slack-mcp-server",
                "env": {"SLACK_MCP_XOXB_TOKEN": "xoxb-test"},
            },
        ]
    )
    assert len(configs) == 2
    assert configs[0].name == "atlassian"
    assert configs[0].transport == "stdio"
    assert configs[0].command == "uvx"
    assert configs[0].args == ["mcp-atlassian"]
    assert configs[0].shared is True  # default

    assert configs[1].name == "slack"
    assert configs[1].transport == "stdio"
    assert configs[1].command == "slack-mcp-server"
    assert configs[1].shared is True  # default


def test_parse_mcp_servers_shared_false():
    """Servers with shared=false are templates for per-role instances."""
    configs = parse_mcp_servers(
        [
            {
                "name": "atlassian",
                "shared": False,
                "command": "uvx",
                "args": ["mcp-atlassian"],
                "env": {"JIRA_URL": "https://co.atlassian.net"},
            },
            {
                "name": "calculator",
                "command": "uvx",
                "args": ["mcp-calculator"],
            },
        ]
    )
    assert len(configs) == 2
    assert configs[0].name == "atlassian"
    assert configs[0].shared is False
    assert configs[1].name == "calculator"
    assert configs[1].shared is True


# --- Env var resolution ---


def test_resolve_env_vars(monkeypatch):
    monkeypatch.setenv("MY_SECRET", "s3cret")
    resolved = resolve_env_vars(
        {
            "TOKEN": "${MY_SECRET}",
            "PLAIN": "hello",
            "MISSING": "${DOES_NOT_EXIST}",
        }
    )
    assert resolved["TOKEN"] == "s3cret"
    assert resolved["PLAIN"] == "hello"
    assert resolved["MISSING"] == ""


def test_resolve_env_vars_embedded_substring(monkeypatch):
    """``${VAR}`` resolves anywhere in a value, not just whole-value —
    needed for per-role HTTP ``Authorization`` headers like
    ``"Bearer ${TOKEN}"`` (the GitHub Copilot MCP per-agent token)."""
    monkeypatch.setenv("GH_TOKEN", "ghp_abc")
    monkeypatch.delenv("GH_MISSING", raising=False)
    resolved = resolve_env_vars(
        {
            "Authorization": "Bearer ${GH_TOKEN}",
            "Two": "${GH_TOKEN}-${GH_TOKEN}",
            "Missing": "Bearer ${GH_MISSING}",
        }
    )
    assert resolved["Authorization"] == "Bearer ghp_abc"
    assert resolved["Two"] == "ghp_abc-ghp_abc"
    assert resolved["Missing"] == "Bearer "


# --- AgentDefinition mcp_env ---


def test_agent_definition_mcp_env():
    from crewlet.agent.definition import AgentDefinition
    from crewlet.org.models import Organization

    role = Role(
        name="PM",
        mcp_env={
            "atlassian": {"JIRA_API_TOKEN": "pm-token"},
        },
    )
    org = Organization(name="Test")
    defn = AgentDefinition(role=role, org=org)
    assert defn.mcp_env == {
        "atlassian": {"JIRA_API_TOKEN": "pm-token"},
    }
