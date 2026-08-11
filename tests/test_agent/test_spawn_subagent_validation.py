"""Input-validation regression tests for spawn_subagent and
colleague-handle resolution.

- ``spawn_subagent`` tool coerces ``max_turns`` to ``int`` and
  rejects non-integer values with a clear error.
- ``_resolve_colleague_handle`` returns strings, not
  ``AgentInstance`` objects.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.turn_context import TurnContext
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion
from crewlet.tools.colleague import register_colleague_tools
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry
from crewlet.tools.spawn_subagent_tool import register_spawn_subagent_tool


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


def _mk_agent() -> AgentInstance:
    role = Role(name="Engineer", handle="alice")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    agent = AgentInstance(definition=defn, handle="alice", email="a@acme.com")
    agent.activate()
    return agent


# ---------------------------------------------------------------------------
# spawn_subagent_tool max_turns coercion  (review comment on line 104)
# ---------------------------------------------------------------------------


async def test_spawn_subagent_tool_rejects_non_integer_max_turns():
    registry = ToolRegistry()
    register_spawn_subagent_tool(registry)
    tool = registry.get("spawn_subagent")
    assert tool is not None

    # Attach the turn_context + spawn_subagent_config so the tool
    # doesn't short-circuit on missing state -- we want to reach the
    # coercion check.
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = AgentContext(
        agent_id=agent.id_str,
        agent_handle=agent.handle,
        role=agent.role_name,
        event_queue=_QueueStub(),
    )
    ctx.__dict__["turn_context"] = turn
    ctx.__dict__["spawn_subagent_config"] = {
        "llm_providers": {"default": None},
        "tool_registry": registry,
        "role_mcp_tools": [],
        "parent_tool_names": [],
        "max_turns_cap": 20,
        "timeout_seconds": 120.0,
        "budget_fraction": 0.2,
        "budget_manager": None,
        "observability": None,
        "token_usage_repo": None,
        "validators": None,
    }

    # Non-parseable string.
    result = await tool.execute(
        {
            "task_prompt": "x",
            "system_prompt": "y",
            "tool_names": [],
            "max_turns": "lots",
        },
        ctx,
    )
    assert result.success is False
    assert "max_turns must be a positive integer" in (result.error or "")


async def test_spawn_subagent_tool_rejects_zero_max_turns():
    registry = ToolRegistry()
    register_spawn_subagent_tool(registry)
    tool = registry.get("spawn_subagent")
    assert tool is not None
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = AgentContext(
        agent_id=agent.id_str,
        agent_handle=agent.handle,
        role=agent.role_name,
        event_queue=_QueueStub(),
    )
    ctx.__dict__["turn_context"] = turn
    ctx.__dict__["spawn_subagent_config"] = {
        "llm_providers": {"default": None},
        "tool_registry": registry,
        "role_mcp_tools": [],
        "parent_tool_names": [],
        "max_turns_cap": 20,
        "timeout_seconds": 120.0,
        "budget_fraction": 0.2,
        "budget_manager": None,
        "observability": None,
        "token_usage_repo": None,
        "validators": None,
    }
    result = await tool.execute(
        {
            "task_prompt": "x",
            "system_prompt": "y",
            "tool_names": [],
            "max_turns": 0,
        },
        ctx,
    )
    assert result.success is False
    assert "max_turns must be >= 1" in (result.error or "")


async def test_spawn_subagent_tool_accepts_string_integer():
    """A stringified integer like ``"5"`` should be accepted (LLMs
    sometimes stringify numeric JSON values)."""
    registry = ToolRegistry()
    register_spawn_subagent_tool(registry)
    tool = registry.get("spawn_subagent")
    assert tool is not None
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = AgentContext(
        agent_id=agent.id_str,
        agent_handle=agent.handle,
        role=agent.role_name,
        event_queue=_QueueStub(),
    )
    ctx.__dict__["turn_context"] = turn

    class _Provider:
        model = "sub"

        async def complete(self, messages, tools=None, **_):
            return Completion(content="done", tool_calls=[])

    ctx.__dict__["spawn_subagent_config"] = {
        "llm_providers": {"default": _Provider()},
        "tool_registry": registry,
        "role_mcp_tools": [],
        "parent_tool_names": [],
        "max_turns_cap": 20,
        "timeout_seconds": 120.0,
        "budget_fraction": 0.2,
        "budget_manager": None,
        "observability": None,
        "token_usage_repo": None,
        "validators": None,
    }
    result = await tool.execute(
        {
            "task_prompt": "x",
            "system_prompt": "y",
            "tool_names": [],
            "max_turns": "5",
        },
        ctx,
    )
    assert result.success is True


async def test_spawn_subagent_tool_max_turns_coercion_edge_cases():
    """``max_turns``
    must not silently truncate fractional floats (``3.9 -> 3``) and
    must not accept ``bool`` (which is an ``int`` subclass).  Whole
    floats and digit-only strings should still pass.
    """
    registry = ToolRegistry()
    register_spawn_subagent_tool(registry)
    tool = registry.get("spawn_subagent")
    assert tool is not None
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)

    class _Provider:
        model = "sub"

        async def complete(self, messages, tools=None, **_):
            return Completion(content="done", tool_calls=[])

    def _fresh_ctx() -> AgentContext:
        ctx = AgentContext(
            agent_id=agent.id_str,
            agent_handle=agent.handle,
            role=agent.role_name,
            event_queue=_QueueStub(),
        )
        ctx.__dict__["turn_context"] = turn
        ctx.__dict__["spawn_subagent_config"] = {
            "llm_providers": {"default": _Provider()},
            "tool_registry": registry,
            "role_mcp_tools": [],
            "parent_tool_names": [],
            "max_turns_cap": 20,
            "timeout_seconds": 120.0,
            "budget_fraction": 0.2,
            "budget_manager": None,
            "observability": None,
            "token_usage_repo": None,
            "validators": None,
        }
        return ctx

    # Fractional float -> rejected, no silent truncation.
    for bad in (3.9, 0.5, -1.5):
        result = await tool.execute(
            {
                "task_prompt": "x",
                "system_prompt": "y",
                "tool_names": [],
                "max_turns": bad,
            },
            _fresh_ctx(),
        )
        assert result.success is False, f"float {bad!r} should be rejected"
        assert "integer" in (result.error or "").lower()

    # bool -> rejected (would silently map to 1 / 0).
    for bad in (True, False):
        result = await tool.execute(
            {
                "task_prompt": "x",
                "system_prompt": "y",
                "tool_names": [],
                "max_turns": bad,
            },
            _fresh_ctx(),
        )
        assert result.success is False, f"bool {bad!r} should be rejected"

    # Whole-number float -> accepted.
    result = await tool.execute(
        {
            "task_prompt": "x",
            "system_prompt": "y",
            "tool_names": [],
            "max_turns": 4.0,
        },
        _fresh_ctx(),
    )
    assert result.success is True, "whole-number float 4.0 should be accepted"

    # Signed digit string negative -> rejected via range check, not
    # parse-error; ensures the sign-accepting lstrip doesn't let a
    # negative sneak past the ``< 1`` guard.
    result = await tool.execute(
        {
            "task_prompt": "x",
            "system_prompt": "y",
            "tool_names": [],
            "max_turns": "-2",
        },
        _fresh_ctx(),
    )
    assert result.success is False
    assert "max_turns must be >= 1" in (result.error or "")


# ---------------------------------------------------------------------------
# resolve_colleague_party / _resolve_colleague_handle string semantics
# (review comment on colleague.py:85)
# ---------------------------------------------------------------------------

from tests.conftest import PartyRegistryStub, StubAgent  # noqa: E402


def test_resolve_colleague_handle_returns_string_not_agent_instance():
    """``_resolve_colleague_handle`` must return a string.  Returning
    whatever ``registry.resolve_handle`` returns -- an AgentInstance --
    would stringify to something like ``<AgentInstance object at
    0x...>`` when later used as ``to_handle`` or in a guard check.
    """
    from crewlet.tools.builtin import _resolve_colleague_handle

    ctx = AgentContext(
        agent_id="a",
        agent_handle="alice",
        role="Engineer",
        event_queue=_QueueStub(),
        handle_registry=PartyRegistryStub(
            agents=[StubAgent("sarah-chen", "Sarah Chen")]
        ),
    )
    # Slack-style handle with underscores -> normalized to hyphens by
    # the resolver's tier-1 fallback.  The returned value must be a
    # plain string.
    resolved = _resolve_colleague_handle("sarah_chen", ctx)
    assert resolved == "sarah-chen"
    assert isinstance(resolved, str)


async def test_a2a_ask_passes_raw_target_when_unresolvable():
    """When nothing matches, a2a_ask falls back to the raw query so
    the A2AService-side guard owns the error."""
    sent_targets: list[str] = []

    class _A2AStub2:
        async def request_channel(self, requester, target, **kwargs):
            sent_targets.append(target)
            return "a2a-1"

        async def send(self, channel_id, sender, content, sender_role=""):
            return None

    from crewlet.tools.registry import ToolRegistry as _TR

    r = _TR()
    register_colleague_tools(r)
    tool = r.get("a2a_ask")
    ctx = AgentContext(
        agent_id="a",
        agent_handle="alice",
        role="Engineer",
        event_queue=_QueueStub(),
        a2a_service=_A2AStub2(),
        handle_registry=PartyRegistryStub(agents=[]),
    )
    result = await tool.execute({"role_urn": "anything", "brief": "hi"}, ctx)
    assert result.success
    assert sent_targets == ["anything"]
