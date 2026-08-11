"""Humans are reached through the agent's own colleague-surface tools.

The engine has no special human-handoff path: an agent reaches a human
seat exactly as it reaches anyone — by calling its own Slack / Jira /
Confluence tools with an @-mention during Execute. The one engine-level
guard that remains is ``a2a_ask`` refusing a human target: humans are
not on the A2A bus (there is no inbox behind the topic), so the tool
returns actionable guidance to use an external surface instead.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.agent.pool import AgentPool
from crewlet.notifications.handle import HandleRegistry
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.tools.colleague import register_colleague_tools
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


def _mk_org() -> Organization:
    return Organization(
        name="Acme",
        units=[
            OrgUnit(
                name="Eng",
                type="team",
                lead="Sarah Chen",
                roles=[
                    Role(
                        name="Sarah Chen",
                        kind="human",
                        email="sarah@acme.com",
                        contact={"slack_user_id": "U0HUMAN"},
                    ),
                    Role(name="Engineer", handle="alice"),
                ],
            )
        ],
    )


def _mk_handle_registry(org: Organization, queue: _QueueStub) -> HandleRegistry:
    pool = AgentPool(queue)  # type: ignore[arg-type]
    return HandleRegistry(pool, org_provider=lambda: org)


async def test_a2a_ask_tool_rejects_human_target():
    """``a2a_ask`` aimed at a human seat fails with guidance to reach
    them on an external surface — humans aren't on the bus, so the
    channel would wait forever."""
    org = _mk_org()
    queue = _QueueStub()
    registry = ToolRegistry()
    register_colleague_tools(registry)
    tool = registry.get("a2a_ask")
    assert tool is not None

    class _A2AService:
        async def request_channel(self, *a, **k):
            raise AssertionError("must not open a channel to a human")

    ctx = AgentContext(
        agent_id="x",
        agent_handle="alice",
        role="Engineer",
        org=org,
        a2a_service=_A2AService(),
        handle_registry=_mk_handle_registry(org, queue),
    )
    result = await tool.execute({"role_urn": "sarah-chen", "brief": "quick sync?"}, ctx)
    assert not result.success
    assert "human teammate" in result.error
    assert "Slack" in result.error
