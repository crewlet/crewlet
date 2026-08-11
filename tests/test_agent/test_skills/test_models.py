"""Tests for ``crewlet.agent.skills.models``."""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from crewlet.agent.skills.models import (
    MAX_SKILL_BODY_BYTES,
    SKILL_VARIABLE_KEY_PATTERN,
    Phase,
    PromptSkill,
    TriggerContext,
    TriggerExpr,
    evaluate_trigger,
    substitute_variables,
    trigger_covers_tool,
)

# -- substitute_variables --------------------------------------------------

_VARS = {
    "confluence_base_url": "https://acme.atlassian.net/wiki",
    "jira_base_url": "https://acme.atlassian.net",
}


def test_substitute_replaces_known_braced_vars() -> None:
    out = substitute_variables(
        "page: ${confluence_base_url}/spaces/X issue: ${jira_base_url}/browse/Y",
        _VARS,
    )
    assert out == (
        "page: https://acme.atlassian.net/wiki/spaces/X "
        "issue: https://acme.atlassian.net/browse/Y"
    )


def test_substitute_leaves_single_brace_placeholders_untouched() -> None:
    """The agent-facing {page_id}/{space_key}/{ISSUE-KEY} placeholders must
    survive verbatim — they are not substitution targets."""
    text = "${confluence_base_url}/spaces/{space_key}/pages/{page_id} {ISSUE-KEY}"
    out = substitute_variables(text, _VARS)
    assert "{space_key}" in out
    assert "{page_id}" in out
    assert "{ISSUE-KEY}" in out
    assert out.startswith("https://acme.atlassian.net/wiki/spaces/{space_key}")


def test_substitute_is_identity_on_bare_dollar_and_double_dollar() -> None:
    """Bare $name, $$ (shell PID / escapes), and currency $ must NOT be
    touched — string.Template would corrupt all three."""
    # A variable named like a word that also appears after a bare $.
    vars_ = {"jira_base_url": "https://acme.atlassian.net"}
    samples = [
        "echo $jira_base_url",  # bare $ — not substituted
        "pid is $$ and price is $$50",  # $$ not collapsed
        "regex ^foo$ and $HOME/bin",  # bare $ words
        "no markers here at all",
    ]
    for s in samples:
        assert substitute_variables(s, vars_) == s


def test_substitute_leaves_unknown_braced_var_literal() -> None:
    out = substitute_variables("a ${unknown} b ${jira_base_url}", _VARS)
    assert out == "a ${unknown} b https://acme.atlassian.net"


def test_substitute_empty_map_is_identity() -> None:
    text = "${jira_base_url}/browse/X $$ {page_id}"
    assert substitute_variables(text, {}) == text


def test_substitute_is_idempotent() -> None:
    text = "${jira_base_url}/x $$5 {page_id}"
    once = substitute_variables(text, _VARS)
    assert substitute_variables(once, _VARS) == once


def test_skill_variable_key_pattern() -> None:
    assert SKILL_VARIABLE_KEY_PATTERN.match("confluence_base_url")
    assert SKILL_VARIABLE_KEY_PATTERN.match("_x9")
    assert not SKILL_VARIABLE_KEY_PATTERN.match("base-url")
    assert not SKILL_VARIABLE_KEY_PATTERN.match("a.b")
    assert not SKILL_VARIABLE_KEY_PATTERN.match("9x")
    assert not SKILL_VARIABLE_KEY_PATTERN.match("../etc")


def test_collect_leaves_walks_nested_triggers() -> None:
    from crewlet.agent.skills.models import (
        collect_server_leaves,
        collect_tool_leaves,
    )

    expr = TriggerExpr(
        any_of=[
            TriggerExpr(tool="slack_post"),
            TriggerExpr(
                all_of=[
                    TriggerExpr(mcp_server="atlassian"),
                    TriggerExpr(tool="jira_add_comment"),
                ]
            ),
        ]
    )
    assert collect_tool_leaves(expr) == {"slack_post", "jira_add_comment"}
    assert collect_server_leaves(expr) == {"atlassian"}


def test_classify_trigger_liveness_flags_drift_on_partially_live_skill() -> None:
    """One leaf matches a real tool, another matches nothing anywhere →
    the dangling leaf is almost certainly a renamed tool (drift)."""
    from crewlet.agent.skills.models import classify_trigger_liveness

    expr = TriggerExpr(
        any_of=[
            TriggerExpr(tool="confluence_create_page"),  # exists
            TriggerExpr(tool="slack_conversations_add_message"),  # renamed away
        ]
    )
    dangling, live = classify_trigger_liveness(
        expr,
        known_tools={"confluence_create_page", "other"},
        known_servers=set(),
    )
    assert dangling == ["slack_conversations_add_message"]
    assert live is True


def test_classify_trigger_liveness_inert_skill_for_undeployed_stack() -> None:
    """No leaf matches any tool or configured server → plausibly a skill
    for a stack this org doesn't run, not drift."""
    from crewlet.agent.skills.models import classify_trigger_liveness

    expr = TriggerExpr(
        any_of=[TriggerExpr(tool="gh_create_pr"), TriggerExpr(mcp_server="github")]
    )
    dangling, live = classify_trigger_liveness(
        expr, known_tools={"jira_add_comment"}, known_servers={"atlassian"}
    )
    assert dangling == ["gh_create_pr"]
    assert live is False


def test_classify_trigger_liveness_server_leaf_makes_skill_live() -> None:
    """A configured-server leaf marks the skill live even when its tool
    leaves all dangle — the server IS deployed, so a missing exact tool
    name there is drift, not absence."""
    from crewlet.agent.skills.models import classify_trigger_liveness

    expr = TriggerExpr(
        any_of=[TriggerExpr(tool="slack_postMsg"), TriggerExpr(mcp_server="slack")]
    )
    dangling, live = classify_trigger_liveness(
        expr, known_tools=set(), known_servers={"slack"}
    )
    assert dangling == ["slack_postMsg"]
    assert live is True


def test_classify_trigger_liveness_clean_when_all_leaves_match() -> None:
    from crewlet.agent.skills.models import classify_trigger_liveness

    expr = TriggerExpr(tool="jira_add_comment")
    dangling, live = classify_trigger_liveness(
        expr, known_tools={"jira_add_comment"}, known_servers=set()
    )
    assert dangling == []
    assert live is True


def test_find_variable_references_matches_substitution_grammar() -> None:
    """The reference finder must see exactly what substitute_variables
    would substitute — nothing more (bare $, $$, {brace}) and nothing
    less (every ${identifier})."""
    from crewlet.agent.skills.models import find_variable_references

    text = (
        "${confluence_base_url}/spaces/{space_key} and ${jira_base_url} "
        "but not $bare nor $$ nor {page_id} nor ${not-an-ident}"
    )
    assert find_variable_references(text) == {
        "confluence_base_url",
        "jira_base_url",
    }
    assert find_variable_references("plain text") == set()


def test_trigger_expr_requires_exactly_one_field() -> None:
    TriggerExpr(mcp_server="github")
    TriggerExpr(tool="reflect_and_persist")
    TriggerExpr(any_of=[TriggerExpr(tool="a")])
    TriggerExpr(all_of=[TriggerExpr(tool="a")])

    with pytest.raises(ValidationError):
        TriggerExpr()
    with pytest.raises(ValidationError):
        TriggerExpr(mcp_server="github", tool="reflect_and_persist")
    with pytest.raises(ValidationError):
        TriggerExpr(mcp_server="github", any_of=[TriggerExpr(tool="a")])


def test_evaluate_trigger_mcp_server() -> None:
    expr = TriggerExpr(mcp_server="github")
    assert evaluate_trigger(expr, TriggerContext(mcp_servers=frozenset({"github"})))
    assert not evaluate_trigger(expr, TriggerContext(mcp_servers=frozenset({"slack"})))
    assert not evaluate_trigger(expr, TriggerContext())


def test_evaluate_trigger_tool() -> None:
    expr = TriggerExpr(tool="reflect_and_persist")
    assert evaluate_trigger(
        expr, TriggerContext(tools=frozenset({"reflect_and_persist"}))
    )
    assert not evaluate_trigger(expr, TriggerContext(tools=frozenset({"other"})))


def test_evaluate_trigger_any_of_short_circuits_to_true() -> None:
    expr = TriggerExpr(
        any_of=[TriggerExpr(tool="missing"), TriggerExpr(mcp_server="github")]
    )
    assert evaluate_trigger(expr, TriggerContext(mcp_servers=frozenset({"github"})))
    assert not evaluate_trigger(expr, TriggerContext())


def test_evaluate_trigger_all_of_requires_every_subexpr() -> None:
    expr = TriggerExpr(all_of=[TriggerExpr(tool="a"), TriggerExpr(mcp_server="github")])
    ctx_both = TriggerContext(tools=frozenset({"a"}), mcp_servers=frozenset({"github"}))
    ctx_partial = TriggerContext(tools=frozenset({"a"}))
    assert evaluate_trigger(expr, ctx_both)
    assert not evaluate_trigger(expr, ctx_partial)


def test_evaluate_trigger_nested() -> None:
    expr = TriggerExpr(
        any_of=[
            TriggerExpr(all_of=[TriggerExpr(tool="a"), TriggerExpr(tool="b")]),
            TriggerExpr(mcp_server="slack"),
        ]
    )
    ctx_ab = TriggerContext(tools=frozenset({"a", "b"}))
    ctx_slack = TriggerContext(mcp_servers=frozenset({"slack"}))
    ctx_just_a = TriggerContext(tools=frozenset({"a"}))
    assert evaluate_trigger(expr, ctx_ab)
    assert evaluate_trigger(expr, ctx_slack)
    assert not evaluate_trigger(expr, ctx_just_a)


def test_prompt_skill_defaults_to_plan_phase() -> None:
    skill = PromptSkill(
        key="tool:x",
        trigger=TriggerExpr(tool="x"),
        title="X",
        summary="One-line summary.",
        body="body",
    )
    assert skill.phases == {Phase.PLAN}


def test_prompt_skill_body_length_cap_rejects_oversize_body() -> None:
    body = "x" * (MAX_SKILL_BODY_BYTES + 1)
    with pytest.raises(ValidationError):
        PromptSkill(
            key="tool:x",
            trigger=TriggerExpr(tool="x"),
            title="X",
            summary="summary",
            body=body,
        )


def test_prompt_skill_body_length_cap_uses_utf8_bytes() -> None:
    # Three-byte UTF-8 chars push the byte count past the cap before the
    # char count would.
    cap_chars = MAX_SKILL_BODY_BYTES // 3
    body = "字" * (cap_chars + 1)
    with pytest.raises(ValidationError):
        PromptSkill(
            key="tool:x",
            trigger=TriggerExpr(tool="x"),
            title="X",
            summary="summary",
            body=body,
        )


def test_prompt_skill_summary_length_cap_rejects_long_summary() -> None:
    from crewlet.agent.skills.models import MAX_SKILL_SUMMARY_BYTES

    with pytest.raises(ValidationError):
        PromptSkill(
            key="tool:x",
            trigger=TriggerExpr(tool="x"),
            title="X",
            summary="x" * (MAX_SKILL_SUMMARY_BYTES + 1),
            body="body",
        )


def test_prompt_skill_requires_summary() -> None:
    with pytest.raises(ValidationError):
        PromptSkill(
            key="tool:x",
            trigger=TriggerExpr(tool="x"),
            title="X",
            body="body",
        )


def test_prompt_skill_required_defaults_to_true() -> None:
    """Skills are enforced by default — a skill is practices for the
    tools its trigger names; advisory content opts out explicitly."""
    skill = PromptSkill(
        key="tool:x",
        trigger=TriggerExpr(tool="x"),
        title="X",
        summary="summary",
        body="body",
    )
    assert skill.required is True
    assert (
        PromptSkill(
            key="tool:x",
            trigger=TriggerExpr(tool="x"),
            title="X",
            summary="summary",
            body="body",
            required=False,
        ).required
        is False
    )


def test_trigger_covers_tool_leaf_tool() -> None:
    expr = TriggerExpr(tool="jira_create_issue")
    assert trigger_covers_tool(expr, tool_name="jira_create_issue")
    assert not trigger_covers_tool(expr, tool_name="jira_get_issue")


def test_trigger_covers_tool_leaf_mcp_server() -> None:
    expr = TriggerExpr(mcp_server="github")
    assert trigger_covers_tool(
        expr, tool_name="create_pull_request", server_name="github"
    )
    assert not trigger_covers_tool(
        expr, tool_name="jira_add_comment", server_name="atlassian"
    )
    # Builtins have no server — an mcp trigger never covers them.
    assert not trigger_covers_tool(expr, tool_name="lookup_colleague", server_name="")


def test_trigger_covers_tool_composites_use_union_semantics() -> None:
    """Both ``any_of`` and ``all_of`` cover the union of their leaves'
    tools — an ``all_of`` skill is about each of its surfaces once the
    full trigger matches (which :func:`evaluate_trigger` checks
    separately)."""
    any_expr = TriggerExpr(
        any_of=[TriggerExpr(tool="a"), TriggerExpr(mcp_server="slack")]
    )
    assert trigger_covers_tool(any_expr, tool_name="a")
    assert trigger_covers_tool(any_expr, tool_name="post", server_name="slack")
    assert not trigger_covers_tool(any_expr, tool_name="b")

    all_expr = TriggerExpr(
        all_of=[TriggerExpr(mcp_server="atlassian"), TriggerExpr(mcp_server="slack")]
    )
    assert trigger_covers_tool(all_expr, tool_name="jira_x", server_name="atlassian")
    assert trigger_covers_tool(all_expr, tool_name="post", server_name="slack")
    assert not trigger_covers_tool(all_expr, tool_name="pr", server_name="github")
