"""Tests for Plan-phase auto-injection of synthesized skills."""

from __future__ import annotations

from datetime import UTC, datetime
from uuid import uuid4

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.prompts import build_plan_prompt
from crewlet.learning.models import SynthesizedSkill
from crewlet.org.models import Organization, OrgUnit, Role


def _mk_definition() -> AgentDefinition:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    resolved = org.get_role("Engineer")
    assert resolved is not None
    return AgentDefinition(role=resolved, org=org)


def _mk_skill(name: str, description: str = "d") -> SynthesizedSkill:
    return SynthesizedSkill(
        id=uuid4(),
        agent_handle="eng",
        name=name,
        description=description,
        content="body",
        created_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        updated_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
    )


def test_synthesized_skills_block_appears_when_non_empty() -> None:
    block = "- **close-the-loop**: Close the loop after ship."
    prompt = build_plan_prompt(
        _mk_definition(),
        tool_catalogue="query_episodes: Search.",
        available_tools=["query_episodes"],
        synthesized_skills_block=block,
    )
    assert "## Synthesized skills you've learned" in prompt
    assert "close-the-loop" in prompt


def test_synthesized_skills_block_absent_when_empty() -> None:
    prompt = build_plan_prompt(
        _mk_definition(),
        tool_catalogue="",
        synthesized_skills_block="",
    )
    assert "Synthesized skills" not in prompt


def test_synthesized_skills_block_lands_before_tool_catalogue() -> None:
    prompt = build_plan_prompt(
        _mk_definition(),
        tool_catalogue="query_episodes: Search.",
        available_tools=["query_episodes"],
        synthesized_skills_block="- **x**: y",
    )
    assert prompt.index("## Synthesized skills") < prompt.index("Available tools")


async def test_plan_prefetch_helper_returns_empty_when_no_store() -> None:
    from crewlet.agent.plan import _fetch_synthesized_skills_block
    from crewlet.agent.turn_context import TurnContext
    from crewlet.tools.protocol import AgentContext

    defn = _mk_definition()
    agent = type(
        "_A",
        (),
        {"handle": "eng", "role_name": "Engineer", "definition": defn, "id_str": "id"},
    )()
    turn = TurnContext(agent=agent, org=defn.org)
    ctx = AgentContext(agent_id="id", agent_handle="eng", role="Engineer")
    out = await _fetch_synthesized_skills_block(turn=turn, agent_context=ctx)
    assert out == ""


async def test_plan_prefetch_helper_renders_skills_from_store() -> None:
    from crewlet.agent.plan import _fetch_synthesized_skills_block
    from crewlet.agent.turn_context import TurnContext
    from crewlet.tools.protocol import AgentContext

    class _Store:
        async def list_for_agent(
            self,
            handle: str,
            *,
            unit_id: str = "",
            include_archived: bool = False,
            include_stale: bool = True,
        ):
            return [
                _mk_skill("close-the-loop", "Close the loop after ship."),
                _mk_skill("escalation-workflow", "Run the escalation playbook."),
            ]

    defn = _mk_definition()
    agent = type(
        "_A",
        (),
        {"handle": "eng", "role_name": "Engineer", "definition": defn, "id_str": "id"},
    )()
    turn = TurnContext(agent=agent, org=defn.org)
    ctx = AgentContext(
        agent_id="id",
        agent_handle="eng",
        role="Engineer",
        synthesized_skill_store=_Store(),
    )
    out = await _fetch_synthesized_skills_block(turn=turn, agent_context=ctx)
    assert "close-the-loop" in out
    assert "escalation-workflow" in out
    assert "use_skill" in out  # nudge line


async def test_plan_prefetch_helper_normalises_skill_descriptions() -> None:
    """LLM-generated descriptions can carry newlines / runaway whitespace.
    The bullet list assumes one entry per line; verify normalisation."""
    from crewlet.agent.plan import _fetch_synthesized_skills_block
    from crewlet.agent.turn_context import TurnContext
    from crewlet.tools.protocol import AgentContext

    class _Store:
        async def list_for_agent(
            self,
            handle: str,
            *,
            unit_id: str = "",
            include_archived: bool = False,
            include_stale: bool = True,
        ):
            return [_mk_skill("close-the-loop", "step one\nstep two\n\nstep three")]

    defn = _mk_definition()
    agent = type(
        "_A",
        (),
        {"handle": "eng", "role_name": "Engineer", "definition": defn, "id_str": "id"},
    )()
    turn = TurnContext(agent=agent, org=defn.org)
    ctx = AgentContext(
        agent_id="id",
        agent_handle="eng",
        role="Engineer",
        synthesized_skill_store=_Store(),
    )
    out = await _fetch_synthesized_skills_block(turn=turn, agent_context=ctx)
    bullet = next(
        ln for ln in out.splitlines() if ln.startswith("- **close-the-loop**")
    )
    assert "\n" not in bullet
    assert "step one step two step three" in bullet


async def test_plan_prefetch_helper_swallows_store_exception() -> None:
    from crewlet.agent.plan import _fetch_synthesized_skills_block
    from crewlet.agent.turn_context import TurnContext
    from crewlet.tools.protocol import AgentContext

    class _BoomStore:
        async def list_for_agent(
            self,
            handle: str,
            *,
            include_archived: bool = False,
            include_stale: bool = True,
        ):
            raise RuntimeError("db down")

    defn = _mk_definition()
    agent = type(
        "_A",
        (),
        {"handle": "eng", "role_name": "Engineer", "definition": defn, "id_str": "id"},
    )()
    turn = TurnContext(agent=agent, org=defn.org)
    ctx = AgentContext(
        agent_id="id",
        agent_handle="eng",
        role="Engineer",
        synthesized_skill_store=_BoomStore(),
    )
    out = await _fetch_synthesized_skills_block(turn=turn, agent_context=ctx)
    assert out == ""
