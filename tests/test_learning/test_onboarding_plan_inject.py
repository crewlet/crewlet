"""Plan-prompt integration tests for the agent-driven onboarding hint.

Covers two paths the unit tests in ``test_onboarding.py`` don't:

1. The full prompt-build flow: when an agent has no onboarding marker
   AND ``mark_onboarded`` is available, the ``## First-turn onboarding``
   block appears in the rendered Plan prompt.
2. The tool-availability gate: when ``mark_onboarded`` is *not* in
   the agent's available-tools surface (e.g. reflection disabled), the
   block is suppressed even if the fetcher returned hint content.
   Mirrors the EPISODIC_GUIDANCE / PERSIST_GUIDANCE gating pattern.
"""

from __future__ import annotations

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.prompts import build_plan_prompt
from crewlet.org.models import Organization, OrgUnit, Role


def _build_org() -> Organization:
    backend = OrgUnit(
        name="Backend",
        type="team",
        lead="Backend Lead",
        roles=[Role(name="Backend Lead"), Role(name="Backend Engineer")],
    )
    engineering = OrgUnit(
        name="Engineering",
        type="department",
        lead="VP Engineering",
        roles=[Role(name="VP Engineering")],
        children=[backend],
    )
    return Organization(name="Acme", units=[engineering])


def _definition(role_name: str = "Backend Engineer") -> AgentDefinition:
    org = _build_org()
    role = org.get_role(role_name)
    assert role is not None
    return AgentDefinition(role=role, org=org)


def test_plan_prompt_includes_hint_when_tool_available() -> None:
    rendered = build_plan_prompt(
        _definition(),
        tool_catalogue="",
        available_tools=["mark_onboarded", "confluence_search"],
        onboarding_hint_block="Read these pages: A, B, C.",
    )
    assert "## First-turn onboarding" in rendered
    assert "Read these pages: A, B, C." in rendered


def test_plan_prompt_suppresses_hint_when_tool_missing() -> None:
    rendered = build_plan_prompt(
        _definition(),
        tool_catalogue="",
        available_tools=[],  # mark_onboarded absent -> reflection disabled
        onboarding_hint_block="Read these pages: A, B, C.",
    )
    assert "## First-turn onboarding" not in rendered
    assert "Read these pages: A, B, C." not in rendered


def test_plan_prompt_suppresses_hint_when_block_empty() -> None:
    """Empty hint string suppresses the block even when the tool is
    available (i.e. agent is already onboarded)."""
    rendered = build_plan_prompt(
        _definition(),
        tool_catalogue="",
        available_tools=["mark_onboarded"],
        onboarding_hint_block="",
    )
    assert "## First-turn onboarding" not in rendered
