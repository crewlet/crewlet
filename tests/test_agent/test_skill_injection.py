"""End-to-end skill-catalogue contract across the four per-phase builders.

Locks in the skill phase-scoping rules (see
``docs/concepts/tool-skills.md``) AND the
two-tier consumption model (catalogue summary inline; body via
``load_tool_skill`` on demand).

- **Plan** catalogues skills triggered by the full tool catalogue.
- **Execute** catalogues skills triggered by ``plan.tools_needed``.
- **Review** catalogues only MCP-server-triggered skills (no tool surface).
- **Sub-agent** catalogues skills triggered by the parent allowlist.

A separate contract from ``test_prompts.py`` so a future refactor of
the prompt-builder internals cannot silently drop this behavior.
"""

from __future__ import annotations

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.prompts import (
    build_execute_prompt,
    build_plan_prompt,
    build_review_prompt,
    build_subagent_prompt,
)
from crewlet.agent.skills import (
    Phase,
    PromptSkill,
    PromptSkillRegistry,
    TriggerExpr,
)
from crewlet.org.models import Organization, Role


def _engineer_def() -> AgentDefinition:
    role = Role(
        name="Engineer",
        handle="eng",
        goal="ship",
        mcp_env={"github": {}, "atlassian": {}},
    )
    org = Organization(name="Acme", roles=[role])
    resolved = org.get_role("Engineer")
    assert resolved is not None
    return AgentDefinition(role=resolved, org=org)


def _registry_with_all_skills() -> PromptSkillRegistry:
    """Three skills, each scoped to different phases / triggers.

    All explicitly advisory (``required=False``) — these tests lock in
    the phase-scoping / cataloguing contract, not enforcement; the
    required-marker contract has its own section below.
    """
    reg = PromptSkillRegistry()
    reg.seed(
        [
            # Plan-only, tool-triggered.
            PromptSkill(
                key="tool:reflect_and_persist",
                trigger=TriggerExpr(tool="reflect_and_persist"),
                phases={Phase.PLAN},
                title="Persisting durable facts",
                summary="PERSIST-SUMMARY",
                body="PERSIST-BODY",
                required=False,
            ),
            # All four phases, mcp_server-triggered.
            PromptSkill(
                key="mcp:github",
                trigger=TriggerExpr(mcp_server="github"),
                phases={
                    Phase.PLAN,
                    Phase.EXECUTE,
                    Phase.REVIEW,
                    Phase.SUBAGENT,
                },
                title="GitHub tools",
                summary="GITHUB-SUMMARY",
                body="GITHUB-BODY",
                required=False,
            ),
            # any_of of two MCP servers; Plan + Execute + Review.
            PromptSkill(
                key="skill:platform_mentions",
                trigger=TriggerExpr(
                    any_of=[
                        TriggerExpr(mcp_server="atlassian"),
                        TriggerExpr(mcp_server="slack"),
                    ]
                ),
                phases={Phase.PLAN, Phase.EXECUTE, Phase.REVIEW},
                title="Mentioning teammates",
                summary="MENTIONS-SUMMARY",
                body="MENTIONS-BODY",
                required=False,
            ),
        ]
    )
    return reg


# ---------------------------------------------------------------------------
# Bodies never inline at prompt-build time
# ---------------------------------------------------------------------------


def test_no_phase_inlines_skill_bodies_in_the_system_prompt() -> None:
    """Skill bodies are loaded on demand via ``load_tool_skill``.
    They never appear in the prompt-build output."""
    reg = _registry_with_all_skills()
    d = _engineer_def()
    pl = build_plan_prompt(
        d,
        tool_catalogue="",
        available_tools=["reflect_and_persist"],
        skill_registry=reg,
    )
    ex = build_execute_prompt(
        d,
        plan_summary="…",
        available_tools=["reflect_and_persist"],
        skill_registry=reg,
    )
    rv = build_review_prompt(d, skill_registry=reg)
    sa = build_subagent_prompt(
        d,
        parent_system_prompt="parent",
        available_tools=[],
        skill_registry=reg,
    )
    for prompt in (pl, ex, rv, sa):
        assert "PERSIST-BODY" not in prompt
        assert "GITHUB-BODY" not in prompt
        assert "MENTIONS-BODY" not in prompt


# ---------------------------------------------------------------------------
# Plan phase
# ---------------------------------------------------------------------------


def test_plan_catalogue_emits_summary_for_every_triggered_skill() -> None:
    reg = _registry_with_all_skills()
    p = build_plan_prompt(
        _engineer_def(),
        tool_catalogue="- reflect_and_persist: …",
        available_tools=["reflect_and_persist"],
        skill_registry=reg,
    )
    assert "PERSIST-SUMMARY" in p
    assert "GITHUB-SUMMARY" in p
    assert "MENTIONS-SUMMARY" in p


def test_plan_catalogue_drops_skill_whose_trigger_is_missing() -> None:
    """``tool:reflect_and_persist`` is not in the catalogue → skipped."""
    reg = _registry_with_all_skills()
    p = build_plan_prompt(
        _engineer_def(),
        tool_catalogue="",
        available_tools=[],
        skill_registry=reg,
    )
    assert "PERSIST-SUMMARY" not in p
    # MCP-server triggers still fire.
    assert "GITHUB-SUMMARY" in p
    assert "MENTIONS-SUMMARY" in p


def test_plan_catalogue_orders_summaries_by_key() -> None:
    """Deterministic alphabetical order for prefix-cache stability."""
    reg = _registry_with_all_skills()
    p = build_plan_prompt(
        _engineer_def(),
        tool_catalogue="",
        available_tools=["reflect_and_persist"],
        skill_registry=reg,
    )
    # mcp:github < skill:platform_mentions < tool:reflect_and_persist
    assert (
        p.index("GITHUB-SUMMARY")
        < p.index("MENTIONS-SUMMARY")
        < p.index("PERSIST-SUMMARY")
    )


# ---------------------------------------------------------------------------
# Execute phase — narrower than Plan: only plan.tools_needed surface
# ---------------------------------------------------------------------------


def test_execute_catalogue_drops_tool_skills_not_in_planned_surface() -> None:
    """The tool-keyed PERSIST skill is Plan-only AND its tool is in
    Execute's allowed list; but Execute phase isn't in its ``phases``
    set, so it should be omitted from the Execute catalogue."""
    reg = _registry_with_all_skills()
    p = build_execute_prompt(
        _engineer_def(),
        plan_summary="…",
        available_tools=["reflect_and_persist"],
        skill_registry=reg,
    )
    assert "PERSIST-SUMMARY" not in p
    assert "GITHUB-SUMMARY" in p  # mcp:github includes Execute phase


def test_execute_catalogue_includes_mcp_skill_independent_of_tools() -> None:
    """A mcp_server-triggered skill fires based on the role's
    ``mcp_env``, not the per-phase tool surface — Execute catalogue
    includes it even when ``available_tools`` is empty."""
    reg = _registry_with_all_skills()
    p = build_execute_prompt(
        _engineer_def(),
        plan_summary="…",
        available_tools=[],
        skill_registry=reg,
    )
    assert "GITHUB-SUMMARY" in p


# ---------------------------------------------------------------------------
# Review phase — no domain tools, MCP-server skills still fire
# ---------------------------------------------------------------------------


def test_review_catalogue_includes_mcp_skill_for_role_with_matching_server() -> None:
    """MCP-keyed skills with the Review phase in their ``phases`` set
    still appear in the Review catalogue (operator-scoped guidance),
    even though Review has no domain-tool surface."""
    reg = _registry_with_all_skills()
    p = build_review_prompt(_engineer_def(), skill_registry=reg)
    assert "GITHUB-SUMMARY" in p
    assert "MENTIONS-SUMMARY" in p


def test_review_catalogue_excludes_tool_keyed_skills() -> None:
    """Review's ``available_tools`` is empty → tool-keyed skills can't match."""
    reg = _registry_with_all_skills()
    p = build_review_prompt(_engineer_def(), skill_registry=reg)
    assert "PERSIST-SUMMARY" not in p


# ---------------------------------------------------------------------------
# Sub-agent phase — parent-passed allowlist
# ---------------------------------------------------------------------------


def test_subagent_catalogue_filters_by_parent_allowlist_and_phases() -> None:
    reg = _registry_with_all_skills()
    p = build_subagent_prompt(
        _engineer_def(),
        parent_system_prompt="You are a worker.",
        available_tools=[],
        skill_registry=reg,
    )
    # mcp:github has SUBAGENT in phases and fires on mcp_env → catalogued.
    assert "GITHUB-SUMMARY" in p
    # platform_mentions and reflect_and_persist don't include SUBAGENT.
    assert "MENTIONS-SUMMARY" not in p
    assert "PERSIST-SUMMARY" not in p


def test_subagent_keeps_parent_prompt_above_catalogue_above_preamble() -> None:
    """Order matters: parent task framing first, then the catalogue
    (so its contextualises the tool allowlist), then the runtime preamble."""
    from crewlet.agent.prompts import SUBAGENT_PREAMBLE

    reg = _registry_with_all_skills()
    p = build_subagent_prompt(
        _engineer_def(),
        parent_system_prompt="PARENT-PROMPT-MARKER",
        available_tools=[],
        skill_registry=reg,
    )
    assert "PARENT-PROMPT-MARKER" in p
    assert "GITHUB-SUMMARY" in p
    assert SUBAGENT_PREAMBLE in p
    assert (
        p.index("PARENT-PROMPT-MARKER")
        < p.index("GITHUB-SUMMARY")
        < p.index(SUBAGENT_PREAMBLE)
    )


# ---------------------------------------------------------------------------
# Cross-phase consistency
# ---------------------------------------------------------------------------


def test_no_phase_promises_engine_auto_injection() -> None:
    """The engine never auto-injects skill bodies — all loads are
    LLM-driven via ``load_tool_skill``.  Each per-phase catalogue
    header must point the LLM at the explicit-load mechanism and
    must NOT claim any kind of automatic body injection (which would
    encourage the model to skip the explicit call and rely on
    guidance that never arrives)."""
    reg = _registry_with_all_skills()
    d = _engineer_def()
    pl = build_plan_prompt(
        d,
        tool_catalogue="",
        available_tools=["reflect_and_persist"],
        skill_registry=reg,
    )
    ex = build_execute_prompt(
        d,
        plan_summary="…",
        available_tools=["reflect_and_persist"],
        skill_registry=reg,
    )
    rv = build_review_prompt(d, skill_registry=reg)
    sa = build_subagent_prompt(
        d,
        parent_system_prompt="P",
        available_tools=[],
        skill_registry=reg,
    )
    for prompt in (pl, ex, rv, sa):
        assert "## Tool skills" in prompt
        assert "load_tool_skill" in prompt
        assert "auto-inject" not in prompt
        assert "auto_load_with" not in prompt
        assert "auto-load" not in prompt


def test_catalogue_collapses_multiline_summary_to_single_bullet() -> None:
    """Operators routinely author summaries as YAML ``|`` literal
    blocks across multiple lines; the catalogue renderer must collapse
    that to one line so the bullet format stays intact."""
    reg = PromptSkillRegistry()
    reg.seed(
        [
            PromptSkill(
                key="skill:multiline",
                trigger=TriggerExpr(tool="anything"),
                phases={Phase.PLAN},
                title="Multi-line Summary Test",
                summary="First sentence here.\nSecond sentence here.\n",
                body="…",
            ),
        ]
    )
    p = build_plan_prompt(
        _engineer_def(),
        tool_catalogue="",
        available_tools=["anything"],
        skill_registry=reg,
    )
    # The whole summary lives on one line — no embedded newline.
    line = next(
        line for line in p.splitlines() if line.startswith("- `skill:multiline`")
    )
    assert "First sentence here. Second sentence here." in line
    # And there's no orphan "Second sentence" line on its own.
    assert "\nSecond sentence here." not in p


def test_empty_registry_produces_no_skill_scaffolding_in_any_phase() -> None:
    """Engine ships zero default skills — without operator-seeded
    Confluence pages, the registry stays empty and the agent runs on
    tool catalogue alone."""
    empty = PromptSkillRegistry()
    d = _engineer_def()
    pl = build_plan_prompt(
        d,
        tool_catalogue="- x: y",
        available_tools=["reflect_and_persist"],
        skill_registry=empty,
    )
    ex = build_execute_prompt(d, plan_summary="…", skill_registry=empty)
    rv = build_review_prompt(d, skill_registry=empty)
    sa = build_subagent_prompt(d, parent_system_prompt="P", skill_registry=empty)
    for prompt in (pl, ex, rv, sa):
        assert "## Tool skills" not in prompt
        assert "GITHUB-SUMMARY" not in prompt
        assert "MENTIONS-SUMMARY" not in prompt
        assert "PERSIST-SUMMARY" not in prompt


# ---------------------------------------------------------------------------
# Required-skill marker (load-before-use guard)
# ---------------------------------------------------------------------------


def _registry_with_required_github() -> PromptSkillRegistry:
    reg = PromptSkillRegistry()
    reg.seed(
        [
            # ``required`` omitted — relies on the enforced-by-default
            # contract, so this fixture doubles as the default's test.
            PromptSkill(
                key="mcp:github",
                trigger=TriggerExpr(mcp_server="github"),
                phases={Phase.PLAN, Phase.EXECUTE, Phase.REVIEW, Phase.SUBAGENT},
                title="GitHub tools",
                summary="GITHUB-SUMMARY",
                body="GITHUB-BODY",
            ),
            PromptSkill(
                key="skill:advice",
                trigger=TriggerExpr(mcp_server="atlassian"),
                phases={Phase.PLAN, Phase.EXECUTE},
                title="Advice",
                summary="ADVICE-SUMMARY",
                body="ADVICE-BODY",
                required=False,
            ),
        ]
    )
    return reg


def test_required_skill_carries_marker_and_note_in_enforced_phases() -> None:
    """Plan / Execute / Sub-agent show the per-entry marker AND the
    enforcement note, but only on the required entry — the advisory
    skill renders unmarked."""
    reg = _registry_with_required_github()
    d = _engineer_def()
    prompts = (
        build_plan_prompt(d, tool_catalogue="", available_tools=[], skill_registry=reg),
        build_execute_prompt(d, plan_summary="…", skill_registry=reg),
        build_subagent_prompt(d, parent_system_prompt="P", skill_registry=reg),
    )
    for prompt in prompts:
        assert "- `mcp:github` (required — load before use) — GITHUB-SUMMARY" in prompt
        assert "engine rejects calls" in prompt  # the enforcement note
    # The advisory entry never gets the marker (sub-agent drops it by
    # phase scoping; check the two phases that catalogue it).
    for prompt in prompts[:2]:
        assert "- `skill:advice` — ADVICE-SUMMARY" in prompt


def test_required_marker_suppressed_in_review() -> None:
    """Review has no domain tools and no ``load_tool_skill`` — nothing
    is enforced there, so the marker and note must not render (they
    would point the reviewer at a tool it doesn't have)."""
    reg = _registry_with_required_github()
    rv = build_review_prompt(_engineer_def(), skill_registry=reg)
    assert "- `mcp:github` — GITHUB-SUMMARY" in rv
    assert "(required — load before use)" not in rv
    assert "engine rejects calls" not in rv


def test_no_required_note_when_no_catalogued_skill_is_required() -> None:
    reg = _registry_with_all_skills()  # all advisory
    p = build_plan_prompt(
        _engineer_def(),
        tool_catalogue="",
        available_tools=["reflect_and_persist"],
        skill_registry=reg,
    )
    assert "## Tool skills" in p
    assert "(required — load before use)" not in p
    assert "engine rejects calls" not in p
