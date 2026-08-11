"""Tests for the ``lookup_colleague`` fuzzy resolver.

Identity / counterparty-profile inlining for ``lookup_colleague`` is
covered in ``tests/test_learning/test_lookup_colleague_profile.py``; this
file focuses on the multi-tier matching that turns partial / typo'd
identifiers into a single canonical handle (or a disambiguation list).
"""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.tools.builtin import (
    _resolve_colleague_candidates,
    _resolve_colleague_handle,
    register_builtin_tools,
)
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry
from tests.conftest import PartyRegistryStub, StubAgent

_Agent = StubAgent
_Registry = PartyRegistryStub


def _ctx(registry: _Registry, *, agent_handle: str = "alice") -> AgentContext:
    return AgentContext(
        agent_id="alice-id",
        agent_handle=agent_handle,
        role="Engineer",
        current_task_id="",
        handle_registry=registry,
    )


# -- _resolve_colleague_candidates ----------------------------------------------


def test_exact_handle_short_circuits_fuzzy_tiers() -> None:
    reg = _Registry(
        [
            _Agent("agent-ceo", "Agent CEO"),
            _Agent("agent-cto", "Agent CTO"),
        ]
    )
    cands = _resolve_colleague_candidates("agent-ceo", _ctx(reg))
    assert [c.handle for c in cands] == ["agent-ceo"]
    assert cands[0].method == "exact_handle"


def test_exact_role_name_matches_case_sensitive() -> None:
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    cands = _resolve_colleague_candidates("Agent CEO", _ctx(reg))
    assert [c.handle for c in cands] == ["agent-ceo"]
    assert cands[0].method == "exact_role"


def test_external_id_lookup() -> None:
    reg = _Registry(
        [_Agent("agent-ceo", "Agent CEO")],
        external={"slack": {"U123": "agent-ceo"}},
    )
    cands = _resolve_colleague_candidates("U123", _ctx(reg))
    assert [c.handle for c in cands] == ["agent-ceo"]
    assert cands[0].method == "external_id"


def test_underscore_handle_normalised_to_hyphenated() -> None:
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    cands = _resolve_colleague_candidates("agent_ceo", _ctx(reg))
    assert [c.handle for c in cands] == ["agent-ceo"]
    assert cands[0].method == "exact_handle"


def test_case_insensitive_role_name_match() -> None:
    """``ceo`` query, role ``CEO`` (no other matches) -> tier 2."""
    reg = _Registry([_Agent("ceo-handle", "CEO")])
    cands = _resolve_colleague_candidates("ceo", _ctx(reg))
    assert [c.handle for c in cands] == ["ceo-handle"]
    assert cands[0].method == "case_insensitive"


def test_substring_match_finds_partial_role_name() -> None:
    """The motivating bug: 'ceo' should resolve 'Agent CEO'."""
    reg = _Registry(
        [
            _Agent("agent-ceo", "Agent CEO"),
            _Agent("eng-lead", "Engineering Lead"),
        ]
    )
    cands = _resolve_colleague_candidates("ceo", _ctx(reg))
    assert [c.handle for c in cands] == ["agent-ceo"]
    assert cands[0].method == "substring"


def test_substring_match_returns_multiple_when_ambiguous() -> None:
    reg = _Registry(
        [
            _Agent("agent-ceo", "Agent CEO"),
            _Agent("vp-ceo-ops", "VP CEO Operations"),
            _Agent("eng-lead", "Engineering Lead"),
        ]
    )
    cands = _resolve_colleague_candidates("ceo", _ctx(reg))
    assert {c.handle for c in cands} == {"agent-ceo", "vp-ceo-ops"}
    assert all(c.method == "substring" for c in cands)


def test_fuzzy_match_handles_typos() -> None:
    """One-character typo: 'enginer' -> 'engineer'."""
    reg = _Registry([_Agent("engineer", "Senior Engineer")])
    cands = _resolve_colleague_candidates("enginer", _ctx(reg))
    assert [c.handle for c in cands] == ["engineer"]
    assert cands[0].method == "fuzzy"


def test_fuzzy_match_skipped_when_substring_matches() -> None:
    """When substring matches, we don't pollute the list with fuzzy hits."""
    reg = _Registry(
        [
            _Agent("agent-ceo", "Agent CEO"),
            _Agent("agent-cfo", "Agent CFO"),  # close to 'ceo' fuzzy
        ]
    )
    cands = _resolve_colleague_candidates("ceo", _ctx(reg))
    # CFO would be a fuzzy hit but substring already matched, so we
    # don't fall through to tier 4.
    assert [c.handle for c in cands] == ["agent-ceo"]
    assert cands[0].method == "substring"


def test_no_match_returns_empty_list() -> None:
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    cands = _resolve_colleague_candidates("totally unrelated nonsense", _ctx(reg))
    assert cands == []


def test_registry_missing_returns_empty_list() -> None:
    ctx = AgentContext(
        agent_id="alice-id",
        agent_handle="alice",
        role="Engineer",
        current_task_id="",
        handle_registry=None,
    )
    assert _resolve_colleague_candidates("ceo", ctx) == []


def test_candidates_deduplicated_by_handle() -> None:
    """Slack ID and exact handle pointing at the same agent → one row."""
    reg = _Registry(
        [_Agent("agent-ceo", "Agent CEO")],
        external={"slack": {"agent-ceo": "agent-ceo"}},
    )
    # Query "agent-ceo" hits exact_handle AND external_id, but only
    # one candidate should land in the output.
    cands = _resolve_colleague_candidates("agent-ceo", _ctx(reg))
    assert [c.handle for c in cands] == ["agent-ceo"]


# -- _resolve_colleague_handle (unambiguous-single-handle path) ------------------


def test_resolve_colleague_handle_exact_match() -> None:
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    assert _resolve_colleague_handle("agent-ceo", _ctx(reg)) == "agent-ceo"


def test_resolve_colleague_handle_single_fuzzy_match() -> None:
    """Sole substring/fuzzy hit is treated as unambiguous."""
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    assert _resolve_colleague_handle("ceo", _ctx(reg)) == "agent-ceo"


def test_resolve_colleague_handle_ambiguous_returns_none() -> None:
    """Multiple fuzzy candidates -> None, so colleague tools don't guess."""
    reg = _Registry(
        [
            _Agent("agent-ceo", "Agent CEO"),
            _Agent("vp-ceo-ops", "VP CEO Operations"),
        ]
    )
    assert _resolve_colleague_handle("ceo", _ctx(reg)) is None


def test_resolve_colleague_handle_no_match_returns_none() -> None:
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    assert _resolve_colleague_handle("nonsense xyz", _ctx(reg)) is None


# -- lookup_colleague tool surface ----------------------------------------------


def _registry_and_tool() -> tuple[ToolRegistry, Any]:
    r = ToolRegistry()
    register_builtin_tools(r)
    return r, r.get("lookup_colleague")


@pytest.mark.asyncio
async def test_lookup_colleague_substring_returns_single_profile() -> None:
    _, tool = _registry_and_tool()
    reg = _Registry([_Agent("agent-ceo", "Agent CEO", email="ceo@acme.com")])
    result = await tool.execute({"query": "ceo"}, _ctx(reg))
    assert result.success
    assert "Inferred match" in result.output
    assert "handle: agent-ceo" in result.output
    assert "role: Agent CEO" in result.output
    assert "email: ceo@acme.com" in result.output


@pytest.mark.asyncio
async def test_lookup_colleague_exact_match_omits_inferred_hint() -> None:
    _, tool = _registry_and_tool()
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    result = await tool.execute({"query": "agent-ceo"}, _ctx(reg))
    assert result.success
    assert "Inferred match" not in result.output
    assert "handle: agent-ceo" in result.output


@pytest.mark.asyncio
async def test_lookup_colleague_ambiguous_returns_candidate_list() -> None:
    _, tool = _registry_and_tool()
    reg = _Registry(
        [
            _Agent("agent-ceo", "Agent CEO"),
            _Agent("vp-ceo-ops", "VP CEO Operations"),
        ]
    )
    result = await tool.execute({"query": "ceo"}, _ctx(reg))
    assert result.success
    assert "Multiple colleagues matched 'ceo'" in result.output
    assert "agent-ceo (Agent CEO)" in result.output
    assert "vp-ceo-ops (VP CEO Operations)" in result.output
    # Not a single profile — neither identity block should appear.
    assert "handle: agent-ceo" not in result.output


@pytest.mark.asyncio
async def test_lookup_colleague_no_match_returns_error() -> None:
    _, tool = _registry_and_tool()
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    result = await tool.execute({"query": "nonsense xyz"}, _ctx(reg))
    assert not result.success
    assert "No colleague found" in (result.error or "")


@pytest.mark.asyncio
async def test_lookup_colleague_empty_query_is_rejected() -> None:
    _, tool = _registry_and_tool()
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    result = await tool.execute({"query": "   "}, _ctx(reg))
    assert not result.success
    assert "query is required" in (result.error or "")


# -- Tier-1 collision: distinct agents on different exact paths -------------


def test_exact_collision_returns_all_distinct_agents() -> None:
    """Handle 'alice' (agent X) and Slack ID 'alice' (agent Y) — both surface."""
    reg = _Registry(
        [_Agent("alice", "Alice Smith"), _Agent("bob", "Bob Jones")],
        external={"slack": {"alice": "bob"}},
    )
    cands = _resolve_colleague_candidates("alice", _ctx(reg))
    assert {c.handle for c in cands} == {"alice", "bob"}


def test_resolve_colleague_handle_returns_none_on_exact_collision() -> None:
    """Tier-1 collision must return None so colleague tools don't silent-route."""
    reg = _Registry(
        [_Agent("alice", "Alice Smith"), _Agent("bob", "Bob Jones")],
        external={"slack": {"alice": "bob"}},
    )
    assert _resolve_colleague_handle("alice", _ctx(reg)) is None


@pytest.mark.asyncio
async def test_lookup_colleague_disambiguates_exact_collision() -> None:
    """Distinct exact-match candidates render as a disambiguation list,
    not as the first candidate's identity block."""
    _, tool = _registry_and_tool()
    reg = _Registry(
        [_Agent("alice", "Alice Smith"), _Agent("bob", "Bob Jones")],
        external={"slack": {"alice": "bob"}},
    )
    result = await tool.execute({"query": "alice"}, _ctx(reg))
    assert result.success
    assert "Multiple colleagues matched 'alice'" in result.output
    assert "alice (Alice Smith)" in result.output
    assert "bob (Bob Jones)" in result.output
    # Must NOT render the single-profile block (which would drop bob).
    assert "handle: alice\n" not in result.output


# -- Tier 2 short-circuits tier 3 -------------------------------------------


def test_tier_2_case_insensitive_short_circuits_tier_3() -> None:
    """A normalised-exact role-name match must beat weaker substring hits.

    Agent A's role normalises to exactly the query; agent B contains the
    query as a substring. Without the short-circuit, both would be
    returned and ``_resolve_colleague_handle`` would treat the pair as
    ambiguous, returning None.
    """
    reg = _Registry(
        [
            _Agent("ceo-handle", "CEO"),
            _Agent("marketing-ceo-deputy", "Marketing CEO Deputy"),
        ]
    )
    cands = _resolve_colleague_candidates("ceo", _ctx(reg))
    assert [c.handle for c in cands] == ["ceo-handle"]
    assert cands[0].method == "case_insensitive"
    assert _resolve_colleague_handle("ceo", _ctx(reg)) == "ceo-handle"


# -- Tier 3 substring guards: short queries + reverse-direction -------------


def test_short_handle_does_not_match_inside_unrelated_word() -> None:
    """Handle 'ml' must NOT match query 'html-form' (reverse-direction
    bug: 'ml' is a substring of 'html'). Token-boundary check rejects."""
    reg = _Registry([_Agent("ml", "ML Lead")])
    assert _resolve_colleague_candidates("html-form", _ctx(reg)) == []


def test_short_role_does_not_match_inside_unrelated_word() -> None:
    """Role 'ML' must not match query 'html-form' for the same reason."""
    reg = _Registry([_Agent("ml-lead-handle", "ML")])
    assert _resolve_colleague_candidates("html-form", _ctx(reg)) == []


def test_role_name_token_match_still_works() -> None:
    """Reverse-direction match should still fire on whole-token boundaries:
    query 'senior engineer' against role 'Engineer' resolves."""
    reg = _Registry([_Agent("engineer", "Engineer")])
    cands = _resolve_colleague_candidates("senior engineer", _ctx(reg))
    assert [c.handle for c in cands] == ["engineer"]
    assert cands[0].method == "substring"


def test_one_char_query_does_not_surface_random_agents() -> None:
    """Below _MIN_SUBSTRING_QUERY_LEN tier 3 is skipped — 1-char queries
    don't pollute the disambiguation list with every agent containing 'a'."""
    reg = _Registry(
        [
            _Agent("alice", "Alice"),
            _Agent("agent-bob", "Agent Bob"),
            _Agent("agent-carol", "Agent Carol"),
        ]
    )
    cands = _resolve_colleague_candidates("a", _ctx(reg))
    assert [c.method for c in cands if c.method == "substring"] == []


def test_two_char_query_does_not_trigger_substring_tier() -> None:
    """2-char queries also fall under the min-length guard."""
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    cands = _resolve_colleague_candidates("ce", _ctx(reg))
    # Tier 4 fuzzy ratio("ce", "agent ceo") = 4/11 ≈ 0.36 < 0.6 — no match.
    assert [c.method for c in cands if c.method == "substring"] == []


def test_two_char_query_still_resolves_exact_handle() -> None:
    """The min-length guard only affects tier 3; exact tier-1 matches
    still resolve regardless of length."""
    reg = _Registry([_Agent("ml", "ML Lead")])
    cands = _resolve_colleague_candidates("ml", _ctx(reg))
    assert [c.handle for c in cands] == ["ml"]
    assert cands[0].method == "exact_handle"


# -- Tier-4 fuzzy length floor ----------------------------------------------


def test_short_query_skips_fuzzy_tier() -> None:
    """3-char queries that miss tier 1-3 don't fall to fuzzy — ratios
    on short strings are too noisy (ratio('ceo','veo')=0.67)."""
    reg = _Registry([_Agent("veo-handle", "Veo Operations")])
    cands = _resolve_colleague_candidates("ceo", _ctx(reg))
    assert cands == []


def test_fuzzy_tier_runs_for_4_plus_char_queries() -> None:
    reg = _Registry([_Agent("engineer", "Engineer")])
    cands = _resolve_colleague_candidates("engneer", _ctx(reg))
    assert [c.handle for c in cands] == ["engineer"]
    assert cands[0].method == "fuzzy"
    assert 0.6 <= cands[0].score <= 1.0


# -- Deterministic ordering -------------------------------------------------


def test_substring_candidates_sorted_alphabetically() -> None:
    """Tier-3 list is stable across registry / pool insertion orders."""
    reg = _Registry(
        [
            _Agent("zeta-ceo-ops", "Zeta CEO Ops"),
            _Agent("alpha-ceo-ops", "Alpha CEO Ops"),
            _Agent("mid-ceo-ops", "Mid CEO Ops"),
        ]
    )
    cands = _resolve_colleague_candidates("ceo", _ctx(reg))
    assert [c.handle for c in cands] == [
        "alpha-ceo-ops",
        "mid-ceo-ops",
        "zeta-ceo-ops",
    ]


def test_fuzzy_candidates_sorted_by_score_then_handle() -> None:
    reg = _Registry(
        [
            _Agent("zenginer", "Z Engineer"),  # weaker fuzzy
            _Agent("engineer", "Engineer"),  # strongest fuzzy
        ]
    )
    cands = _resolve_colleague_candidates("engneer", _ctx(reg))
    # Best score first.
    assert [c.handle for c in cands][0] == "engineer"


# -- Unicode-aware normalization --------------------------------------------


def test_em_dash_in_role_name_normalises_to_space() -> None:
    """Confluence-pasted em-dashes fold the same as ASCII hyphens."""
    reg = _Registry([_Agent("vp-eng", "VP—Engineering")])
    cands = _resolve_colleague_candidates("vp engineering", _ctx(reg))
    assert [c.handle for c in cands] == ["vp-eng"]
    assert cands[0].method == "case_insensitive"


def test_casefold_handles_german_eszett() -> None:
    """``ß`` casefolds to ``ss`` so users can type either."""
    reg = _Registry([_Agent("strasse-lead", "Straße Lead")])
    cands = _resolve_colleague_candidates("strasse lead", _ctx(reg))
    assert [c.handle for c in cands] == ["strasse-lead"]


def test_nfkd_strips_combining_marks_in_role_name() -> None:
    """Turkish dotted İ + combining dot folds to plain 'i'."""
    reg = _Registry([_Agent("ik-lead", "İK Lead")])
    cands = _resolve_colleague_candidates("ik lead", _ctx(reg))
    assert [c.handle for c in cands] == ["ik-lead"]


# -- Disambiguation UX ------------------------------------------------------


@pytest.mark.asyncio
async def test_lookup_colleague_truncates_long_candidate_list_with_marker() -> None:
    """When more than _LOOKUP_DISPLAY_LIMIT match, the LLM sees how many
    are hidden so it knows to narrow the query."""
    _, tool = _registry_and_tool()
    # 12 agents whose role_name contains 'ceo'.
    reg = _Registry([_Agent(f"agent-{i:02d}-ceo", f"Agent {i} CEO") for i in range(12)])
    result = await tool.execute({"query": "ceo"}, _ctx(reg))
    assert result.success
    assert "Multiple colleagues matched 'ceo'" in result.output
    assert "...and 2 more" in result.output
    # Exactly 10 candidate lines rendered (capped).
    candidate_lines = [
        line for line in result.output.splitlines() if line.startswith("  - ")
    ]
    assert len(candidate_lines) == 10


@pytest.mark.asyncio
async def test_lookup_colleague_no_marker_when_at_or_below_limit() -> None:
    """Multi-match below the display limit renders the full list with
    no truncation marker. Also pin the disambiguation contract so a
    regression to success=False doesn't pass this test vacuously."""
    _, tool = _registry_and_tool()
    reg = _Registry([_Agent(f"agent-{i:02d}-ceo", f"Agent {i} CEO") for i in range(3)])
    result = await tool.execute({"query": "ceo"}, _ctx(reg))
    assert result.success
    assert "Multiple colleagues matched 'ceo'" in result.output
    assert "more — narrow your query" not in result.output
    candidate_lines = [
        line for line in result.output.splitlines() if line.startswith("  - ")
    ]
    assert len(candidate_lines) == 3


@pytest.mark.asyncio
async def test_lookup_colleague_fuzzy_hint_includes_score() -> None:
    _, tool = _registry_and_tool()
    reg = _Registry([_Agent("engineer", "Engineer")])
    result = await tool.execute({"query": "engneer"}, _ctx(reg))
    assert "Inferred match" in result.output
    assert "fuzzy match" in result.output
    assert "score 0." in result.output  # e.g. "score 0.93"


@pytest.mark.asyncio
async def test_lookup_colleague_no_score_in_hint_for_non_fuzzy_inferred_match() -> None:
    """Substring matches show the method label but no score field."""
    _, tool = _registry_and_tool()
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    result = await tool.execute({"query": "ceo"}, _ctx(reg))
    assert "Inferred match" in result.output
    assert "partial-name match" in result.output
    assert "score" not in result.output


@pytest.mark.asyncio
async def test_lookup_colleague_case_insensitive_label_is_not_fuzzy() -> None:
    """Case-difference matches are not 'Fuzzy' — the user typed the
    literal role name modulo case. Label should reflect that."""
    _, tool = _registry_and_tool()
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    result = await tool.execute({"query": "agent ceo"}, _ctx(reg))
    assert result.success
    assert "Inferred match" in result.output
    assert "case / format match" in result.output
    # The literal word "Fuzzy" must not appear for non-fuzzy methods.
    assert "Fuzzy" not in result.output


# -- Disambiguation: per-candidate method hint ------------------------------


@pytest.mark.asyncio
async def test_disambiguation_list_shows_method_per_candidate() -> None:
    """Tier-1 collision (handle vs Slack external_id pointing at a
    different agent) renders both with their match method so the LLM
    can narrow with 'the Slack one' rather than looping on the same
    handle."""
    _, tool = _registry_and_tool()
    reg = _Registry(
        [_Agent("alice", "Alice Smith"), _Agent("bob", "Bob Jones")],
        external={"slack": {"alice": "bob"}},
    )
    result = await tool.execute({"query": "alice"}, _ctx(reg))
    assert result.success
    assert "[via exact handle]" in result.output
    assert "[via external id]" in result.output


# -- Distinguished error when registry is unwired ---------------------------


@pytest.mark.asyncio
async def test_lookup_colleague_distinguishes_missing_registry_from_no_match() -> None:
    _, tool = _registry_and_tool()
    ctx_no_reg = AgentContext(
        agent_id="alice-id",
        agent_handle="alice",
        role="Engineer",
        current_task_id="",
        handle_registry=None,
    )
    result = await tool.execute({"query": "ceo"}, ctx_no_reg)
    assert not result.success
    assert "Handle registry is not configured" in (result.error or "")


# -- Tier-3 forward direction: word-boundary guard --------------------------


def test_forward_substring_rejects_mid_word_fragment() -> None:
    """Query 'log' must NOT match role 'Blog Editor' — 'log' is a
    mid-word fragment of 'blog'."""
    reg = _Registry([_Agent("blog-editor", "Blog Editor")])
    cands = _resolve_colleague_candidates("log", _ctx(reg))
    assert cands == []


def test_forward_substring_accepts_word_aligned_match() -> None:
    """Query 'ceo' matches 'agent ceo' (after space) — word boundary."""
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    cands = _resolve_colleague_candidates("ceo", _ctx(reg))
    assert [c.handle for c in cands] == ["agent-ceo"]
    assert cands[0].method == "substring"


def test_forward_substring_accepts_prefix_within_token() -> None:
    """Query 'engineer' matches role 'Engineering Lead' — 'engineer'
    starts at the beginning of token 'engineering' (word boundary)."""
    reg = _Registry([_Agent("eng-lead", "Engineering Lead")])
    cands = _resolve_colleague_candidates("engineer", _ctx(reg))
    assert [c.handle for c in cands] == ["eng-lead"]
    assert cands[0].method == "substring"


def test_forward_substring_rejects_short_topical_fragments() -> None:
    """'form' must not match 'Platform Engineer' — 'form' is mid-word
    inside 'platform'."""
    reg = _Registry([_Agent("plat-eng", "Platform Engineer")])
    cands = _resolve_colleague_candidates("form", _ctx(reg))
    assert cands == []


# -- Output / log safety ----------------------------------------------------


@pytest.mark.asyncio
async def test_query_with_newlines_collapsed_in_error_message() -> None:
    """Untrusted query with embedded newlines must not break out of
    the ``error`` string the LLM sees — even on the no-match path
    where the query is echoed back verbatim."""
    _, tool = _registry_and_tool()
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    nasty = "totally\nunrelated\n  - smuggled-bullet (Smuggled Role)"
    result = await tool.execute({"query": nasty}, _ctx(reg))
    assert not result.success
    # The error must be a single line (no embedded newlines).
    assert "\n" not in (result.error or "")


@pytest.mark.asyncio
async def test_role_name_newline_does_not_splice_phantom_rows() -> None:
    """Role names with embedded newlines must not break the candidate
    list formatting — exactly one bullet per registered agent, even
    if its role_name contains \\n + ``-`` sequences."""
    _, tool = _registry_and_tool()
    reg = _Registry(
        [
            _Agent("a-ceo", "Agent\nCEO\n  - smuggled-row (Smuggled)"),
            _Agent("b-ceo", "B CEO"),
        ]
    )
    result = await tool.execute({"query": "ceo"}, _ctx(reg))
    assert result.success
    candidate_lines = [
        line for line in result.output.splitlines() if line.startswith("  - ")
    ]
    assert len(candidate_lines) == 2
    # Each bullet starts with one of the registered handles, not a
    # smuggled identifier.
    assert candidate_lines[0].startswith("  - a-ceo")
    assert candidate_lines[1].startswith("  - b-ceo")


@pytest.mark.asyncio
async def test_long_query_is_truncated_in_error_message() -> None:
    _, tool = _registry_and_tool()
    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    huge = "x" * 10_000
    result = await tool.execute({"query": huge}, _ctx(reg))
    assert not result.success
    # Error contains the truncation ellipsis, not the full 10k chars.
    assert len(result.error or "") < 200
    assert "…" in (result.error or "")


# -- Turkish dotless ı normalisation ----------------------------------------


def test_dotless_turkish_i_folds_to_ascii_i() -> None:
    """Role 'Yazılım' (Turkish, with U+0131 dotless ı) is reachable
    from an ASCII query 'yazilim'."""
    reg = _Registry([_Agent("yazilim-lead", "Yazılım Lead")])
    cands = _resolve_colleague_candidates("yazilim lead", _ctx(reg))
    assert [c.handle for c in cands] == ["yazilim-lead"]
    assert cands[0].method == "case_insensitive"


# -- Tier-2 multi-hit -------------------------------------------------------


def test_tier_2_multi_hit_returns_all_distinct_agents() -> None:
    """Two agents whose normalised role / handle equals the query —
    both are returned; ``_resolve_colleague_handle`` returns None."""
    reg = _Registry(
        [
            _Agent("agent-ceo", "CEO"),
            _Agent("other-ceo", "CEO"),  # same role, different handle
        ]
    )
    cands = _resolve_colleague_candidates("ceo", _ctx(reg))
    assert {c.handle for c in cands} == {"agent-ceo", "other-ceo"}
    assert all(c.method == "case_insensitive" for c in cands)
    assert _resolve_colleague_handle("ceo", _ctx(reg)) is None


# -- Deterministic tier-1 ordering ------------------------------------------


def test_tier1_collisions_sorted_alphabetically_by_handle() -> None:
    """Tier-1 collision display order must be deterministic across
    pool insertion orders — alphabetical by handle, matching tier 3."""
    reg = _Registry(
        [_Agent("zeta", "Zeta"), _Agent("alpha", "Alpha")],
        external={"slack": {"alpha": "zeta"}},  # 'alpha' Slack ID → zeta
    )
    cands = _resolve_colleague_candidates("alpha", _ctx(reg))
    assert [c.handle for c in cands] == ["alpha", "zeta"]


# -- Ambiguity audit log ---------------------------------------------------


def test_resolve_colleague_handle_logs_skipped_on_ambiguity(monkeypatch) -> None:
    """When the resolver had matches but returned None (ambiguous),
    an info-level event fires so operators can trace skipped routes."""
    from crewlet.tools import builtin as builtin_mod

    calls: list[tuple[str, dict[str, Any]]] = []

    class _LogSpy:
        def info(self, event: str, **kwargs: Any) -> None:
            calls.append((event, kwargs))

        def debug(self, event: str, **kwargs: Any) -> None:
            calls.append((event, kwargs))

    monkeypatch.setattr(builtin_mod, "logger", _LogSpy())

    reg = _Registry(
        [_Agent("agent-ceo", "Agent CEO"), _Agent("vp-ceo-ops", "VP CEO Ops")]
    )
    result = _resolve_colleague_handle("ceo", _ctx(reg))
    assert result is None
    events = [(e, k) for e, k in calls if e == "lookup_colleague_skipped_ambiguous"]
    assert len(events) == 1
    _, kwargs = events[0]
    assert kwargs["count"] == 2
    assert set(kwargs["handles"]) == {"agent-ceo", "vp-ceo-ops"}


def test_resolve_colleague_handle_logs_inferred_on_single_match(monkeypatch) -> None:
    """Single-match non-exact resolution still emits the audit info log."""
    from crewlet.tools import builtin as builtin_mod

    calls: list[tuple[str, dict[str, Any]]] = []

    class _LogSpy:
        def info(self, event: str, **kwargs: Any) -> None:
            calls.append((event, kwargs))

        def debug(self, event: str, **kwargs: Any) -> None:
            calls.append((event, kwargs))

    monkeypatch.setattr(builtin_mod, "logger", _LogSpy())

    reg = _Registry([_Agent("agent-ceo", "Agent CEO")])
    result = _resolve_colleague_handle("ceo", _ctx(reg))
    assert result == "agent-ceo"
    events = [(e, k) for e, k in calls if e == "lookup_colleague_inferred_handle"]
    assert len(events) == 1
    _, kwargs = events[0]
    assert kwargs["handle"] == "agent-ceo"
    assert kwargs["method"] == "substring"


# -- human seats -------------------------------------------------------------


def _human_seat(**overrides: Any) -> Any:
    from crewlet.org.models import Role

    data: dict[str, Any] = {
        "name": "Sarah Chen",
        "kind": "human",
        "email": "sarah@acme.com",
        "contact": {
            "slack_user_id": "U0HUMAN",
            "atlassian_account_id": "5b10-sarah",
            "github_login": "sarahchen",
        },
        "availability": "CET business hours; replies within ~4h",
        "goal": "Keep the team unblocked",
        "backstory": "20 years in infrastructure",
        "responsibilities": ["Approvals", "Vendor decisions"],
    }
    data.update(overrides)
    return Role(**data)


def test_human_seat_resolves_by_handle() -> None:
    reg = _Registry([_Agent("engineer", "Engineer")], humans=[_human_seat()])
    cands = _resolve_colleague_candidates("sarah-chen", _ctx(reg))
    assert [c.handle for c in cands] == ["sarah-chen"]
    assert cands[0].kind == "human"
    assert cands[0].method == "exact_handle"


def test_human_seat_resolves_by_role_name() -> None:
    reg = _Registry([], humans=[_human_seat()])
    cands = _resolve_colleague_candidates("Sarah Chen", _ctx(reg))
    assert [c.handle for c in cands] == ["sarah-chen"]
    assert cands[0].method == "exact_role"


def test_human_seat_resolves_by_substring() -> None:
    reg = _Registry([_Agent("engineer", "Engineer")], humans=[_human_seat()])
    cands = _resolve_colleague_candidates("sarah", _ctx(reg))
    assert [c.handle for c in cands] == ["sarah-chen"]
    assert cands[0].kind == "human"
    assert cands[0].method == "substring"


def test_human_seat_resolves_by_fuzzy() -> None:
    reg = _Registry([], humans=[_human_seat()])
    cands = _resolve_colleague_candidates("sara chen", _ctx(reg))
    assert [c.handle for c in cands] == ["sarah-chen"]
    assert cands[0].method == "fuzzy"


@pytest.mark.asyncio
async def test_lookup_human_identity_block() -> None:
    _, tool = _registry_and_tool()
    reg = _Registry([], humans=[_human_seat()])
    result = await tool.execute({"query": "sarah-chen"}, _ctx(reg))
    assert result.success
    out = result.output
    assert "kind: human" in out
    assert "name: Sarah Chen" in out
    assert "goal: Keep the team unblocked" in out
    assert "background: 20 years in infrastructure" in out
    assert "responsibilities: Approvals; Vendor decisions" in out
    assert "email: sarah@acme.com" in out
    assert "slack_id: U0HUMAN" in out
    assert "jira_id: 5b10-sarah" in out
    assert "confluence_id: 5b10-sarah" in out
    assert "github_id: sarahchen" in out
    assert "availability: CET business hours" in out
    assert "Human teammate" in out
    assert "asynchronously" in out


@pytest.mark.asyncio
async def test_lookup_agent_seat_identity_block_carries_kind() -> None:
    _, tool = _registry_and_tool()
    reg = _Registry([_Agent("engineer", "Engineer", email="e@acme.com")])
    result = await tool.execute({"query": "engineer"}, _ctx(reg))
    assert result.success
    assert "kind: agent" in result.output
    assert "Human teammate" not in result.output


@pytest.mark.asyncio
async def test_disambiguation_marks_human_rows() -> None:
    _, tool = _registry_and_tool()
    reg = _Registry(
        [_Agent("sarah-bot", "Sarah Bot")],
        humans=[_human_seat()],
    )
    result = await tool.execute({"query": "sarah"}, _ctx(reg))
    assert result.success
    assert "Multiple colleagues matched" in result.output
    assert "sarah-chen (Sarah Chen, human)" in result.output
    assert "sarah-bot (Sarah Bot)" in result.output
