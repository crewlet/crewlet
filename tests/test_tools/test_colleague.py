"""Tests for the colleague-surface ``a2a_ask`` builtin.

Pre-cleanup this module also covered thin MCP-forwarding wrappers
(``slack_message``, ``jira_assign``, ``jira_comment``,
``confluence_comment``, ``confluence_mention``,
``github_request_review``) plus a ``DelegationEdge`` event nothing
read.  Both layers are gone -- agents call upstream MCP tools
(``slack_conversations_postMessage``, ``jira_update_issue``, etc.)
directly during Execute.  Only ``a2a_ask`` survives as a colleague-
surface builtin because it's a pure engine path with no MCP equivalent.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.tools.colleague import register_colleague_tools
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry
from tests.conftest import PartyRegistryStub, StubAgent

# ---------------------------------------------------------------------------
# Stubs
# ---------------------------------------------------------------------------


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


@dataclass
class _A2AStub:
    channels: list[str] = field(default_factory=list)
    sent: list[tuple[str, str, str]] = field(default_factory=list)

    async def request_channel(self, requester: str, target: str, **kwargs: Any) -> str:
        cid = f"a2a-{len(self.channels)}"
        self.channels.append(cid)
        return cid

    async def send(
        self, channel_id: str, sender: str, content: str, sender_role: str = ""
    ) -> None:
        self.sent.append((channel_id, sender, content))


def _mk_ctx(*, a2a: _A2AStub | None = None) -> AgentContext:
    queue = _QueueStub()
    return AgentContext(
        agent_id="id-a",
        agent_handle="alice",
        role="Engineer",
        current_task_id="task-42",
        event_queue=queue,
        a2a_service=a2a,
        handle_registry=PartyRegistryStub(agents=[StubAgent("bob")]),
    )


def _registry() -> ToolRegistry:
    r = ToolRegistry()
    register_colleague_tools(r)
    return r


# ---------------------------------------------------------------------------
# Registration
# ---------------------------------------------------------------------------


def test_only_a2a_ask_is_registered():
    """The colleague layer registers exactly one tool.  There are no
    per-integration Slack/Jira/Confluence/GitHub wrappers -- agents
    reach those surfaces through the upstream MCP tools directly."""
    names = {t.name for t in _registry().list_tools()}
    assert "a2a_ask" in names
    for removed in (
        "slack_message",
        "jira_comment",
        "jira_assign",
        "confluence_comment",
        "confluence_mention",
        "github_request_review",
    ):
        assert removed not in names


def test_a2a_ask_description_narrows_use():
    """The a2a_ask description must explicitly steer the LLM away
    from using it for things a human would want to see."""
    tool = _registry().get("a2a_ask")
    assert tool is not None
    desc = tool.description
    assert "tight-loop" in desc or "tight loop" in desc
    # Points at the colleague-surface tools as the alternatives, by
    # capability rather than by any hardcoded tool name (so the steer
    # holds for any tool stack).
    assert "colleague-surface tools" in desc
    lowered = desc.lower()
    assert "issue comment" in lowered
    assert "chat" in lowered
    # And it must NOT name a specific integration tool.
    assert "slack_conversations_postMessage" not in desc
    assert "jira_add_comment" not in desc


# ---------------------------------------------------------------------------
# a2a_ask
# ---------------------------------------------------------------------------


async def test_a2a_ask_opens_channel_and_posts_brief():
    a2a = _A2AStub()
    ctx = _mk_ctx(a2a=a2a)
    tool = _registry().get("a2a_ask")
    assert tool is not None
    result = await tool.execute(
        {"role_urn": "bob", "brief": "please look at PR 42"}, ctx
    )
    assert result.success
    assert "Opened A2A channel" in result.output
    assert a2a.channels  # channel created
    assert a2a.sent  # brief delivered


async def test_a2a_ask_requires_role_urn_and_brief():
    ctx = _mk_ctx(a2a=_A2AStub())
    tool = _registry().get("a2a_ask")
    assert tool is not None
    result = await tool.execute({"role_urn": "bob"}, ctx)
    assert not result.success
    assert "brief" in (result.error or "")


async def test_a2a_ask_handles_missing_service():
    ctx = _mk_ctx(a2a=None)
    tool = _registry().get("a2a_ask")
    assert tool is not None
    result = await tool.execute({"role_urn": "bob", "brief": "hi"}, ctx)
    assert not result.success
    assert "A2A service" in (result.error or "")


async def test_a2a_ask_surfaces_service_refusal():
    """A ValueError from request_channel (target not a live agent)
    becomes a tool failure, not an exception or a fake success."""

    class _RefusingA2A:
        async def request_channel(self, requester, target, **kwargs):
            raise ValueError(f"a2a target '{target}' is not a live agent")

        async def send(self, *a, **k):  # pragma: no cover
            raise AssertionError("send must not be reached")

    queue = _QueueStub()
    ctx = AgentContext(
        agent_id="id-a",
        agent_handle="alice",
        role="Engineer",
        event_queue=queue,
        a2a_service=_RefusingA2A(),
        handle_registry=PartyRegistryStub(agents=[]),
    )
    tool = _registry().get("a2a_ask")
    result = await tool.execute({"role_urn": "human", "brief": "hi"}, ctx)
    assert not result.success
    assert "not a live agent" in (result.error or "")
