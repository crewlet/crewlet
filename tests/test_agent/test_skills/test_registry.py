"""Tests for ``crewlet.agent.skills.registry``."""

from __future__ import annotations

from pathlib import Path

import pytest

from crewlet.agent.skills.models import (
    Phase,
    PromptSkill,
    TriggerContext,
    TriggerExpr,
)
from crewlet.agent.skills.registry import PromptSkillRegistry

_REPO_ROOT = Path(__file__).resolve().parents[3]


def _skill(
    key: str,
    *,
    trigger: TriggerExpr | None = None,
    phases: set[Phase] | None = None,
    required: bool = False,
) -> PromptSkill:
    return PromptSkill(
        key=key,
        trigger=trigger or TriggerExpr(tool=key),
        phases=phases or {Phase.PLAN},
        title=key,
        summary=f"summary of {key}",
        body=f"body of {key}",
        required=required,
    )


def test_render_substitutes_installed_variables() -> None:
    reg = PromptSkillRegistry()
    reg.set_variables({"confluence_base_url": "https://acme.atlassian.net/wiki"})
    assert (
        reg.render("see ${confluence_base_url}/spaces/{space_key}")
        == "see https://acme.atlassian.net/wiki/spaces/{space_key}"
    )


def test_render_is_identity_without_variables() -> None:
    reg = PromptSkillRegistry()
    text = "${confluence_base_url}/x $$ {page_id}"
    assert reg.render(text) == text


def test_set_variables_swaps_atomically_and_copies() -> None:
    reg = PromptSkillRegistry()
    src = {"jira_base_url": "https://acme.atlassian.net"}
    reg.set_variables(src)
    # Mutating the source dict after install must not affect the registry.
    src["jira_base_url"] = "https://evil.example"
    assert reg.render("${jira_base_url}") == "https://acme.atlassian.net"


class _RecordingLogger:
    """Drop-in structlog stand-in that records every call.

    Monkeypatched over the module logger instead of
    ``structlog.testing.capture_logs`` because the engine configures
    structlog with ``cache_logger_on_first_use=True`` — once any earlier
    test has bound the module's lazy logger proxy, ``capture_logs`` can
    no longer intercept it and the assertion becomes test-order
    dependent.
    """

    def __init__(self) -> None:
        self.calls: list[tuple[str, str, dict]] = []

    def __getattr__(self, level: str):
        def _log(event: str, **kw) -> None:
            self.calls.append((level, event, kw))

        return _log

    def unresolved(self) -> list[tuple[str, str, str]]:
        return [
            (kw["skill_key"], kw["variable"], kw["field"])
            for _, event, kw in self.calls
            if event == "skill_variable_unresolved"
        ]


def test_unresolved_variable_reference_warns_on_registration(monkeypatch) -> None:
    """A skill referencing a ${var} missing from the map must produce a
    structured warning — the literal ${name} otherwise only ever shows
    up inside an LLM prompt, which no operator reads."""
    from crewlet.agent.skills import registry as registry_module

    skill = PromptSkill(
        key="tool:links",
        trigger=TriggerExpr(tool="post"),
        title="Links",
        summary="share via ${confluence_base_url}",
        body="also ${jira_base_url} and known ${present}",
    )
    reg = PromptSkillRegistry()
    reg.set_variables({"present": "yes"})

    rec = _RecordingLogger()
    monkeypatch.setattr(registry_module, "logger", rec)

    reg.seed([skill])
    assert rec.unresolved() == [
        ("tool:links", "confluence_base_url", "summary"),
        ("tool:links", "jira_base_url", "body"),
    ]

    # Installing a map that covers the references clears the warnings...
    rec.calls.clear()
    reg.set_variables(
        {
            "present": "yes",
            "confluence_base_url": "https://x.atlassian.net/wiki",
            "jira_base_url": "https://x.atlassian.net",
        }
    )
    assert rec.unresolved() == []

    # ...and a config push that DROPS a referenced variable re-warns for
    # already-registered skills (not just on the skill's next edit).
    rec.calls.clear()
    reg.set_variables({"present": "yes"})
    assert ("tool:links", "confluence_base_url", "summary") in rec.unresolved()


@pytest.mark.asyncio
async def test_unresolved_variable_reference_warns_on_upsert(monkeypatch) -> None:
    from crewlet.agent.skills import registry as registry_module

    reg = PromptSkillRegistry()
    skill = PromptSkill(
        key="tool:links",
        trigger=TriggerExpr(tool="post"),
        title="uses ${nowhere}",
        summary="s",
        body="b",
    )
    rec = _RecordingLogger()
    monkeypatch.setattr(registry_module, "logger", rec)
    await reg.upsert(skill)
    assert ("tool:links", "nowhere", "title") in rec.unresolved()


def test_bundled_platform_mentions_renders_with_example_variables(
    monkeypatch,
) -> None:
    """End-to-end regression against the REAL shipped skill + example config:
    the bundled platform_mentions body must render clickable workspace links
    and keep the agent-facing single-brace placeholders.  Pins the contract
    so a future switch to str.format() (which eats ``{project-uuid}``) can't
    silently break it.  The example's ``plane_base_url`` is the
    ``${PLANE_URL}`` env reference (one var points the whole config at an
    instance), so the env supplies the address here exactly as the
    bootstrap-written ``.env.plane`` does in real runs."""
    from crewlet.agent.skills.parser import parse_skill
    from crewlet.config import load_company_config
    from crewlet.engine_builders import resolve_skill_variables

    monkeypatch.setenv("PLANE_URL", "https://plane.nimbus.example")
    skill = parse_skill(
        (_REPO_ROOT / "examples/tool-skills/platform-mentions.md").read_text()
    )
    cfg = load_company_config(str(_REPO_ROOT / "examples/nimbus.company.yaml"))
    reg = PromptSkillRegistry()
    reg.seed([skill])
    reg.set_variables(resolve_skill_variables(cfg))

    body = reg.render(skill.body)
    assert (
        "https://plane.nimbus.example/nimbus/projects/{project-uuid}"
        "/issues/{work-item-uuid}" in body
    )
    assert (
        "https://plane.nimbus.example/nimbus/projects/{project-uuid}"
        "/pages/{page-id}" in body
    )
    assert "${plane_base_url}" not in body
    assert "${plane_workspace_slug}" not in body
    # Agent-facing single-brace placeholders survive for the LLM to fill.
    assert "{project-uuid}" in body
    assert "{work-item-uuid}" in body and "{page-id}" in body


@pytest.mark.asyncio
async def test_upsert_then_skills_for_returns_matching() -> None:
    reg = PromptSkillRegistry()
    await reg.upsert(_skill("tool:a"))
    await reg.upsert(_skill("tool:b"))

    out = reg.skills_for(
        phase=Phase.PLAN, available_tools={"tool:a"}, mcp_servers=set()
    )
    assert [s.key for s in out] == ["tool:a"]


@pytest.mark.asyncio
async def test_skills_for_filters_by_phase() -> None:
    reg = PromptSkillRegistry()
    await reg.upsert(_skill("tool:plan_only", phases={Phase.PLAN}))
    await reg.upsert(_skill("tool:exec_only", phases={Phase.EXECUTE}))
    plan = reg.skills_for(
        phase=Phase.PLAN,
        available_tools={"tool:plan_only", "tool:exec_only"},
        mcp_servers=set(),
    )
    execute = reg.skills_for(
        phase=Phase.EXECUTE,
        available_tools={"tool:plan_only", "tool:exec_only"},
        mcp_servers=set(),
    )
    assert [s.key for s in plan] == ["tool:plan_only"]
    assert [s.key for s in execute] == ["tool:exec_only"]


@pytest.mark.asyncio
async def test_skills_for_sorted_by_key_for_prefix_cache_stability() -> None:
    reg = PromptSkillRegistry()
    # Insert in reverse-alpha order — the registry must still return alpha.
    for k in ("tool:z", "tool:m", "tool:a"):
        await reg.upsert(_skill(k))
    out = reg.skills_for(
        phase=Phase.PLAN,
        available_tools={"tool:z", "tool:m", "tool:a"},
        mcp_servers=set(),
    )
    assert [s.key for s in out] == ["tool:a", "tool:m", "tool:z"]


@pytest.mark.asyncio
async def test_evict_removes_skill() -> None:
    reg = PromptSkillRegistry()
    await reg.upsert(_skill("tool:a"))
    assert "tool:a" in reg
    assert await reg.evict("tool:a") is True
    assert "tool:a" not in reg
    assert (
        reg.skills_for(phase=Phase.PLAN, available_tools={"tool:a"}, mcp_servers=set())
        == []
    )


@pytest.mark.asyncio
async def test_evict_missing_returns_false() -> None:
    reg = PromptSkillRegistry()
    assert await reg.evict("tool:nope") is False


@pytest.mark.asyncio
async def test_evict_by_page_id_removes_matching_skill() -> None:
    reg = PromptSkillRegistry()
    skill = _skill("tool:a").model_copy(update={"source_page_id": "P-1"})
    await reg.upsert(skill)
    await reg.upsert(_skill("tool:b").model_copy(update={"source_page_id": "P-2"}))
    assert await reg.evict_by_page_id("P-1") is True
    assert "tool:a" not in reg
    assert "tool:b" in reg


@pytest.mark.asyncio
async def test_evict_by_page_id_miss_returns_false() -> None:
    reg = PromptSkillRegistry()
    await reg.upsert(_skill("tool:a").model_copy(update={"source_page_id": "P-1"}))
    assert await reg.evict_by_page_id("P-404") is False
    assert await reg.evict_by_page_id("") is False
    assert "tool:a" in reg
    # Skills constructed without a source page (empty id) never match
    # an empty-string lookup.
    await reg.upsert(_skill("tool:pageless"))
    assert await reg.evict_by_page_id("") is False
    assert "tool:pageless" in reg


@pytest.mark.asyncio
async def test_evict_by_page_id_evicts_at_most_one() -> None:
    # Page ids are unique in practice — and ``upsert`` now enforces it
    # (see the rename tests below) — so an aliased state can only be
    # constructed via ``seed``; even then, a single eviction call
    # removes exactly one entry.
    reg = PromptSkillRegistry()
    reg.seed(
        [
            _skill("tool:a").model_copy(update={"source_page_id": "P-1"}),
            _skill("tool:b").model_copy(update={"source_page_id": "P-1"}),
        ]
    )
    assert await reg.evict_by_page_id("P-1") is True
    assert len(reg) == 1


def test_seed_replaces_wholesale() -> None:
    """``seed`` installs the walked state atomically AND drops entries
    the walk didn't produce — a re-walk after a page deletion (or a
    backend cut-over) must not keep serving stale skills."""
    reg = PromptSkillRegistry()
    reg.seed([_skill("tool:old"), _skill("tool:kept")])
    assert sorted(reg.keys()) == ["tool:kept", "tool:old"]
    reg.seed([_skill("tool:kept"), _skill("tool:new")])
    assert sorted(reg.keys()) == ["tool:kept", "tool:new"]
    reg.seed([])
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_upsert_replaces_existing() -> None:
    reg = PromptSkillRegistry()
    await reg.upsert(_skill("tool:a"))
    skill_v2 = PromptSkill(
        key="tool:a",
        trigger=TriggerExpr(tool="tool:a"),
        title="A v2",
        summary="A v2 summary",
        body="body v2",
    )
    await reg.upsert(skill_v2)
    assert len(reg) == 1
    out = reg.skills_for(
        phase=Phase.PLAN, available_tools={"tool:a"}, mcp_servers=set()
    )
    assert out[0].title == "A v2"
    assert out[0].body == "body v2"


@pytest.mark.asyncio
async def test_upsert_evicts_stale_key_for_same_page() -> None:
    """A backend page is one skill: renaming a page's ``key`` in the
    UI upserts under the new key AND evicts the old-key entry sourced
    from the same page — no ghost serving the stale trigger/body."""
    reg = PromptSkillRegistry()
    await reg.upsert(_skill("tool:old").model_copy(update={"source_page_id": "P-1"}))
    await reg.upsert(_skill("tool:new").model_copy(update={"source_page_id": "P-1"}))
    assert reg.keys() == ["tool:new"]


@pytest.mark.asyncio
async def test_upsert_rename_leaves_other_pages_untouched() -> None:
    reg = PromptSkillRegistry()
    await reg.upsert(_skill("tool:other").model_copy(update={"source_page_id": "P-9"}))
    await reg.upsert(_skill("tool:old").model_copy(update={"source_page_id": "P-1"}))
    await reg.upsert(_skill("tool:new").model_copy(update={"source_page_id": "P-1"}))
    assert reg.keys() == ["tool:new", "tool:other"]


@pytest.mark.asyncio
async def test_upsert_without_page_id_never_evicts() -> None:
    """Skills with no source page (empty id) must not match each other
    — the rename eviction keys on a NON-EMPTY shared page id."""
    reg = PromptSkillRegistry()
    await reg.upsert(_skill("tool:a"))
    await reg.upsert(_skill("tool:b"))
    assert reg.keys() == ["tool:a", "tool:b"]


@pytest.mark.asyncio
async def test_skills_for_no_trigger_match_returns_empty() -> None:
    reg = PromptSkillRegistry()
    await reg.upsert(_skill("mcp:github", trigger=TriggerExpr(mcp_server="github")))
    out = reg.skills_for(
        phase=Phase.PLAN,
        available_tools=set(),
        mcp_servers={"atlassian"},
    )
    assert out == []


@pytest.mark.asyncio
async def test_get_returns_skill_by_exact_key_or_none() -> None:
    reg = PromptSkillRegistry()
    await reg.upsert(_skill("mcp:github"))
    assert reg.get("mcp:github") is not None
    assert reg.get("mcp:slack") is None


def test_required_skills_for_tool_filters_required_phase_and_coverage() -> None:
    reg = PromptSkillRegistry()
    reg.seed(
        [
            # Required, covers the tool, right phase → gates.
            _skill(
                "mcp:github",
                trigger=TriggerExpr(mcp_server="github"),
                phases={Phase.EXECUTE},
                required=True,
            ),
            # Advisory twin — never gates.
            _skill(
                "mcp:github-advice",
                trigger=TriggerExpr(mcp_server="github"),
                phases={Phase.EXECUTE},
            ),
            # Required but Plan-scoped — not returned for Execute.
            _skill(
                "mcp:github-plan",
                trigger=TriggerExpr(mcp_server="github"),
                phases={Phase.PLAN},
                required=True,
            ),
            # Required but about a different server — no coverage.
            _skill(
                "mcp:slack",
                trigger=TriggerExpr(mcp_server="slack"),
                phases={Phase.EXECUTE},
                required=True,
            ),
        ]
    )
    ctx = TriggerContext(
        tools=frozenset({"create_pull_request"}),
        mcp_servers=frozenset({"github", "slack"}),
    )
    out = reg.required_skills_for_tool(
        phase=Phase.EXECUTE,
        tool_name="create_pull_request",
        server_name="github",
        ctx=ctx,
    )
    assert [s.key for s in out] == ["mcp:github"]


def test_required_skills_for_tool_needs_full_trigger_match() -> None:
    """An ``all_of`` skill whose full trigger doesn't match the session
    surface does not gate, even though one leaf covers the tool."""
    reg = PromptSkillRegistry()
    reg.seed(
        [
            _skill(
                "skill:cross",
                trigger=TriggerExpr(
                    all_of=[
                        TriggerExpr(mcp_server="atlassian"),
                        TriggerExpr(mcp_server="slack"),
                    ]
                ),
                phases={Phase.EXECUTE},
                required=True,
            )
        ]
    )
    partial = TriggerContext(mcp_servers=frozenset({"atlassian"}))
    full = TriggerContext(mcp_servers=frozenset({"atlassian", "slack"}))
    assert (
        reg.required_skills_for_tool(
            phase=Phase.EXECUTE,
            tool_name="jira_add_comment",
            server_name="atlassian",
            ctx=partial,
        )
        == []
    )
    assert [
        s.key
        for s in reg.required_skills_for_tool(
            phase=Phase.EXECUTE,
            tool_name="jira_add_comment",
            server_name="atlassian",
            ctx=full,
        )
    ] == ["skill:cross"]


def test_required_skills_for_tool_sorted_by_key() -> None:
    reg = PromptSkillRegistry()
    reg.seed(
        [
            _skill(
                key,
                trigger=TriggerExpr(tool="deploy"),
                phases={Phase.EXECUTE},
                required=True,
            )
            for key in ("tool:z", "tool:a", "tool:m")
        ]
    )
    out = reg.required_skills_for_tool(
        phase=Phase.EXECUTE,
        tool_name="deploy",
        server_name="",
        ctx=TriggerContext(tools=frozenset({"deploy"})),
    )
    assert [s.key for s in out] == ["tool:a", "tool:m", "tool:z"]
