"""Tests for Plan-phase auto-injection of counterparty profiles."""

from __future__ import annotations

from datetime import UTC, datetime

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.plan import _render_counterparty_profile
from crewlet.agent.prompts import build_plan_prompt
from crewlet.learning.models import CounterpartyProfile
from crewlet.org.models import Organization, OrgUnit, Role


def _mk_definition() -> AgentDefinition:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    resolved = org.get_role("Engineer")
    assert resolved is not None
    return AgentDefinition(role=resolved, org=org)


def _mk_profile(**overrides) -> CounterpartyProfile:
    base = {
        "observer_handle": "eng",
        "subject_handle": "bob",
        "subject_external_id": "",
        "subject_platform": "",
        "subject_name": "Bob",
        "traits": {
            "communication_style": "terse",
            "interests": ["deploys", "CI"],
        },
        "first_seen_at": datetime(2026, 4, 20, 9, 0, tzinfo=UTC),
        "last_updated_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        "interaction_count": 3,
    }
    base.update(overrides)
    return CounterpartyProfile(**base)


def test_render_counterparty_profile_includes_traits_and_provenance() -> None:
    rendered = _render_counterparty_profile(_mk_profile())
    assert "Subject: Bob" in rendered
    assert "communication_style: terse" in rendered
    assert "interests: deploys, CI" in rendered
    assert "interactions: 3" in rendered
    assert "last updated:" in rendered


def test_render_handles_empty_traits_gracefully() -> None:
    rendered = _render_counterparty_profile(_mk_profile(traits={}))
    assert "Observed by you: (no traits yet)" in rendered


def test_render_for_external_subject_uses_platform_and_external_id() -> None:
    profile = _mk_profile(
        subject_handle="",
        subject_external_id="U12345",
        subject_platform="slack",
        subject_name="",
    )
    rendered = _render_counterparty_profile(profile)
    assert "Subject: slack:U12345" in rendered
    assert "Platform: slack" in rendered


def test_profile_lands_after_guidance_and_before_catalogue() -> None:
    profile_text = _render_counterparty_profile(_mk_profile())
    prompt = build_plan_prompt(
        _mk_definition(),
        tool_catalogue="query_episodes: Search.",
        available_tools=["query_episodes"],
        counterparty_profile=profile_text,
    )
    assert "## Known counterparty" in prompt
    assert profile_text in prompt
    # Guidance block is before the profile; profile is before catalogue.
    idx_guidance = prompt.find("query_episodes before starting")
    idx_profile = prompt.index("## Known counterparty")
    idx_catalogue = prompt.index("Available tools")
    assert idx_guidance < idx_profile < idx_catalogue


def test_empty_profile_text_does_not_add_section() -> None:
    prompt = build_plan_prompt(
        _mk_definition(), tool_catalogue="", counterparty_profile=""
    )
    assert "Known counterparty" not in prompt
