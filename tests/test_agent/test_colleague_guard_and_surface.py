"""Tests for the ``a2a_ask`` colleague-surface builtin.

There are no per-integration colleague-surface wrappers
(slack_message / jira_assign / etc.); agents reach colleagues through
the upstream MCP tools (``slack_conversations_postMessage``,
``jira_update_issue``, etc.) called directly during Execute.
``a2a_ask`` is the only colleague-
surface builtin.  Runaway / circular delegation is bounded
by the always-on delegation-depth cap (checked at the top of every
turn); ``a2a_ask`` forwards the caller's chain so the recipient's turn
inherits the accumulated depth.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.tools.colleague import register_colleague_tools
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry

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
        self,
        channel_id: str,
        sender: str,
        content: str,
        sender_role: str = "",
    ) -> None:
        self.sent.append((channel_id, sender, content))


class _FakeTurn:
    """Minimal stand-in for TurnContext."""

    def __init__(self, chain: list[str]) -> None:
        self.delegation_chain = list(chain)
        self.delegation_depth = len(chain)
        self.turn_id = "t-1"


def _mk_ctx(*, chain: list[str], a2a: _A2AStub | None = None) -> AgentContext:
    ctx = AgentContext(
        agent_id="id-a",
        agent_handle="alice",
        role="Engineer",
        current_task_id="t",
        event_queue=_QueueStub(),
        a2a_service=a2a,
    )
    ctx.__dict__["turn_context"] = _FakeTurn(chain)
    return ctx


# ---------------------------------------------------------------------------
# a2a_ask
#
# ``a2a_ask`` opens a channel and forwards the caller's delegation
# chain so the recipient's turn inherits the accumulated depth; the
# always-on delegation-depth cap (checked at the top of every turn)
# bounds runaway / circular chains.
# ---------------------------------------------------------------------------


async def test_a2a_ask_proceeds_when_target_not_in_chain():
    registry = ToolRegistry()
    register_colleague_tools(registry)
    tool = registry.get("a2a_ask")
    assert tool is not None

    a2a = _A2AStub()
    ctx = _mk_ctx(chain=["alice"], a2a=a2a)
    result = await tool.execute({"role_urn": "bob", "brief": "hello"}, ctx)
    assert result.success is True
    assert a2a.channels  # channel was opened


async def test_a2a_ask_without_turn_context_defaults_to_empty_chain():
    """A fresh AgentContext with no turn_context attached has an
    implicit empty chain; the call must not be rejected for that
    reason."""
    registry = ToolRegistry()
    register_colleague_tools(registry)
    tool = registry.get("a2a_ask")
    assert tool is not None

    a2a = _A2AStub()
    ctx = AgentContext(
        agent_id="a",
        agent_handle="alice",
        role="Engineer",
        event_queue=_QueueStub(),
        a2a_service=a2a,
    )
    result = await tool.execute({"role_urn": "bob", "brief": "hi"}, ctx)
    assert result.success is True
