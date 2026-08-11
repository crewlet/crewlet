"""Tool-annotation overrides flow from ``mcp_servers`` entries into the
launched MCP servers.

The sub-agent write-guard derives behaviour from MCP annotations.  Since
every tool server — including the Jira/Confluence ``atlassian`` server,
Slack, and GitHub — is a generic ``mcp_servers`` entry, these tests
pin that an operator override placed on such an entry reaches the
``MCPServerConfig`` the engine actually launches, so an under-annotating
server is correctable without engine changes.
"""

from __future__ import annotations

from crewlet.config import CompanyConfig, parse_mcp_servers


def _cfg() -> CompanyConfig:
    return CompanyConfig(
        name="T",
        mcp_servers=[
            {
                "name": "atlassian",
                "shared": False,
                "command": "uvx",
                "args": ["mcp-atlassian"],
                "tool_annotations": {
                    "jira_add_comment": {"read_only": False, "open_world": True},
                    "confluence_create_page": {
                        "read_only": False,
                        "open_world": True,
                    },
                },
            },
            {
                "name": "slack",
                "shared": False,
                "command": "npx",
                "args": ["slack-mcp-server"],
                "tool_prefix": "slack_",
                "tool_annotations": {
                    "conversations_add_message": {
                        "read_only": False,
                        "open_world": True,
                    }
                },
            },
            {
                "name": "github",
                "transport": "http",
                "shared": False,
                "url": "https://api.githubcopilot.com/mcp/",
                "tool_annotations": {"create_pull_request": {"destructive": True}},
            },
        ],
    )


def test_atlassian_entry_carries_overrides():
    """A single ``atlassian`` entry carries both Jira and Confluence
    overrides (the two products share one mcp-atlassian server)."""
    cfg = _cfg()
    atlassian = next(
        c for c in parse_mcp_servers(cfg.mcp_servers) if c.name == "atlassian"
    )
    overrides = atlassian.annotation_overrides()
    assert set(overrides) == {"jira_add_comment", "confluence_create_page"}
    assert overrides["jira_add_comment"].read_only is False
    assert overrides["confluence_create_page"].open_world is True


def test_slack_entry_carries_overrides():
    cfg = _cfg()
    slack = next(c for c in parse_mcp_servers(cfg.mcp_servers) if c.name == "slack")
    overrides = slack.annotation_overrides()
    assert overrides["conversations_add_message"].read_only is False


def test_github_http_entry_carries_overrides():
    """The GitHub Copilot MCP is a ``shared: false`` http entry; its
    overrides still reach the launched server."""
    cfg = _cfg()
    github = next(c for c in parse_mcp_servers(cfg.mcp_servers) if c.name == "github")
    assert github.transport == "http"
    assert github.annotation_overrides()["create_pull_request"].destructive is True


def test_no_overrides_when_unset():
    """Default (no tool_annotations) → empty overrides, unchanged
    behaviour for the common case where the server self-annotates."""
    cfg = CompanyConfig(
        name="T",
        mcp_servers=[
            {
                "name": "atlassian",
                "shared": False,
                "command": "uvx",
                "args": ["mcp-atlassian"],
            }
        ],
    )
    atlassian = next(
        c for c in parse_mcp_servers(cfg.mcp_servers) if c.name == "atlassian"
    )
    assert atlassian.annotation_overrides() == {}
