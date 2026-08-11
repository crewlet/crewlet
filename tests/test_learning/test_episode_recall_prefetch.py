"""Tests for the frozen-at-turn-start episode prefetch in Plan.

Covers the Gap-4 closure: the Plan phase pre-fetches a small number
of similar past turns once at turn start and bakes the summary into
the system prompt, preserving prompt prefix-caching on later
``query_episodes`` tool calls.
"""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any
from uuid import uuid4

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.plan import _EMPTY_RECALL_HINT, _fetch_episode_recall_block
from crewlet.agent.prompts import build_plan_prompt
from crewlet.agent.turn_context import TurnContext
from crewlet.learning.models import Episode
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.tools.protocol import AgentContext


class _EpisodeStoreStub:
    def __init__(self, episodes: list[Episode] | None = None) -> None:
        self._episodes = episodes or []
        self.queries: list[dict[str, Any]] = []

    async def query(
        self,
        *,
        agent_handle: str,
        query_text: str,
        limit: int = 5,
        outcome_filter: str | None = None,
    ) -> list[Episode]:
        self.queries.append(
            {
                "agent_handle": agent_handle,
                "query_text": query_text,
                "limit": limit,
                "outcome_filter": outcome_filter,
            }
        )
        return list(self._episodes)


def _mk_episode(
    *, task_summary: str = "ship the release", outcome: str = "done"
) -> Episode:
    return Episode(
        agent_handle="eng",
        agent_role="Engineer",
        task_id="",
        turn_id=uuid4(),
        started_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        ended_at=datetime(2026, 4, 23, 9, 5, tzinfo=UTC),
        plan_summary="Plan",
        task_summary=task_summary,
        tool_sequence=["query_knowledge", "slack_message"],
        skills_used=[],
        review_outcome=outcome,  # type: ignore[arg-type]
        duration_ms=60_000,
    )


def _mk_turn_and_ctx(
    *,
    episode_store: Any | None,
    task_description: str = "Help with the release",
) -> tuple[TurnContext, AgentContext]:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    resolved = org.get_role("Engineer")
    assert resolved is not None
    defn = AgentDefinition(role=resolved, org=org)
    agent = type(
        "_A",
        (),
        {
            "handle": "eng",
            "role_name": "Engineer",
            "definition": defn,
            "id_str": "eng-id",
        },
    )()
    turn = TurnContext(agent=agent, org=org, task_description=task_description)
    ctx = AgentContext(
        agent_id="eng-id",
        agent_handle="eng",
        role="Engineer",
        episode_store=episode_store,
    )
    return turn, ctx


# ----------------------------------------------------------------------
# Helper behaviour
# ----------------------------------------------------------------------


async def test_prefetch_renders_hits_as_bullets() -> None:
    store = _EpisodeStoreStub(
        [
            _mk_episode(task_summary="ship release v2"),
            _mk_episode(task_summary="ship release v1", outcome="done"),
        ]
    )
    turn, ctx = _mk_turn_and_ctx(episode_store=store)
    block = await _fetch_episode_recall_block(turn=turn, agent_context=ctx)
    assert "[done] ship release v2" in block
    assert "[done] ship release v1" in block
    assert "query_knowledge" in block  # tool list rendered
    # Query was run with outcome_filter=done and scoped to the agent.
    assert store.queries[0]["outcome_filter"] == "done"
    assert store.queries[0]["agent_handle"] == "eng"
    assert store.queries[0]["limit"] == 3


async def test_prefetch_renders_compacted_episode_by_pattern() -> None:
    """A compacted hit has empty task_summary/plan_summary by
    construction -- its signal is common_task_pattern. The bullet must
    render that + the observed count, not a useless '(no summary)'."""
    compacted = Episode(
        agent_handle="eng",
        agent_role="Engineer",
        task_id="",
        turn_id=uuid4(),
        started_at=datetime(2026, 4, 1, 9, 0, tzinfo=UTC),
        ended_at=datetime(2026, 4, 20, 9, 0, tzinfo=UTC),
        plan_summary="",
        task_summary="",
        tool_sequence=["query_knowledge"],
        skills_used=[],
        review_outcome="done",
        duration_ms=600_000,
        kind="compacted",
        count=7,
        common_task_pattern="triage and reply to inbound release questions",
    )
    store = _EpisodeStoreStub([compacted])
    turn, ctx = _mk_turn_and_ctx(episode_store=store)
    block = await _fetch_episode_recall_block(turn=turn, agent_context=ctx)
    assert "triage and reply to inbound release questions" in block
    assert "[observed 7x]" in block
    assert "(no summary)" not in block


async def test_prefetch_empty_when_no_store_wired() -> None:
    turn, ctx = _mk_turn_and_ctx(episode_store=None)
    assert await _fetch_episode_recall_block(turn=turn, agent_context=ctx) == ""


async def test_prefetch_empty_when_no_task_description() -> None:
    store = _EpisodeStoreStub([_mk_episode()])
    turn, ctx = _mk_turn_and_ctx(episode_store=store, task_description="")
    assert await _fetch_episode_recall_block(turn=turn, agent_context=ctx) == ""
    assert store.queries == []


async def test_prefetch_empty_when_store_returns_nothing() -> None:
    store = _EpisodeStoreStub([])
    turn, ctx = _mk_turn_and_ctx(episode_store=store)
    assert await _fetch_episode_recall_block(turn=turn, agent_context=ctx) == ""


async def test_prefetch_swallows_store_exceptions() -> None:
    class _BoomStore:
        async def query(self, **kwargs):
            raise RuntimeError("db down")

    turn, ctx = _mk_turn_and_ctx(episode_store=_BoomStore())
    assert await _fetch_episode_recall_block(turn=turn, agent_context=ctx) == ""


async def test_prefetch_skipped_on_thin_recon_required_trigger() -> None:
    """Same thin-trigger gate as the personal-memory / relevant-
    knowledge prefetches: a bare pointer (a recon-required webhook)
    skips the episode vector query entirely.  Instead of vanishing
    silently the block renders ``_EMPTY_RECALL_HINT`` -- so the planner
    sees *why* it is empty and is pointed at ``query_episodes`` after
    recon, consistent with the personal-memory / relevant-knowledge
    EMPTY_FILTER_HINTs."""
    from crewlet.events.types import ExternalNotification

    store = _EpisodeStoreStub([_mk_episode(task_summary="ship release v2")])
    turn, ctx = _mk_turn_and_ctx(episode_store=store)
    # A Jira webhook -> context_requires_recon=True -> InboundInteraction
    # .requires_recon=True.
    turn.trigger_event = ExternalNotification(
        notification_source="jira",
        sender="Manager",
        body=turn.task_description,
        metadata={"issue_key": "POC-528"},
        context_requires_recon=True,
    )
    block = await _fetch_episode_recall_block(turn=turn, agent_context=ctx)
    assert block == _EMPTY_RECALL_HINT
    assert "query_episodes" in block
    # The vector query never ran -- the gate fired before the hint.
    assert store.queries == []


def test_thin_trigger_hint_keeps_similar_prior_work_section_visible() -> None:
    """The gate-path hint is a non-empty block, so ``build_plan_prompt``
    still renders the ``## Similar prior work`` section -- the block
    stays visible instead of vanishing, which is the inconsistency this
    fixes (personal memory / relevant knowledge already render a hint
    on their gate paths)."""
    prompt = build_plan_prompt(
        _defn(), tool_catalogue="", episode_recall_block=_EMPTY_RECALL_HINT
    )
    assert "## Similar prior work" in prompt
    assert "query_episodes" in prompt


async def test_prefetch_runs_on_self_contained_trigger() -> None:
    """A trigger that is *not* recon-required (e.g. a top-level Slack
    message, ``context_requires_recon=False``) keeps the normal
    episode-recall path."""
    from crewlet.events.types import ExternalNotification

    store = _EpisodeStoreStub([_mk_episode(task_summary="ship release v2")])
    turn, ctx = _mk_turn_and_ctx(episode_store=store)
    turn.trigger_event = ExternalNotification(
        notification_source="slack",
        sender="U123",
        body=turn.task_description,
        metadata={"slack_user_id": "U123"},
        context_requires_recon=False,
    )
    block = await _fetch_episode_recall_block(turn=turn, agent_context=ctx)
    assert "ship release v2" in block
    assert len(store.queries) == 1  # vector query ran


async def test_prefetch_queries_against_salient_body_not_enriched_task() -> None:
    """The episode vector query must key on the raw inbound message,
    not the notification builder's triage scaffolding -- otherwise
    every Slack turn matches against the same boilerplate and the
    real question never reaches the similarity search."""
    from crewlet.events.types import ExternalNotification

    store = _EpisodeStoreStub([_mk_episode(task_summary="ship release v2")])
    turn, ctx = _mk_turn_and_ctx(
        episode_store=store, task_description="ENRICHED TASK DESCRIPTION (fallback)"
    )
    turn.trigger_event = ExternalNotification(
        notification_source="slack",
        sender="U0TESTUSER1",
        body=(
            "A Slack message was posted by **U0TESTUSER1**.\n## Triage — decide "
            "BEFORE replying\n" + ("scaffolding " * 200)
        ),
        salient_body="who can review the backend fixes this week?",
        metadata={"slack_user_id": "U0TESTUSER1"},
        context_requires_recon=False,
    )
    await _fetch_episode_recall_block(turn=turn, agent_context=ctx)
    assert store.queries[0]["query_text"] == (
        "who can review the backend fixes this week?"
    )


async def test_prefetch_normalises_newlines_in_episode_summaries() -> None:
    """A multi-line task_summary would otherwise turn one bullet into a
    multi-line block and break the Plan-prompt's line-per-entry shape."""
    store = _EpisodeStoreStub(
        [_mk_episode(task_summary="ship release\nstep two\n  step three")]
    )
    turn, ctx = _mk_turn_and_ctx(episode_store=store)
    block = await _fetch_episode_recall_block(turn=turn, agent_context=ctx)
    bullet_lines = [ln for ln in block.splitlines() if ln.startswith("- [")]
    assert len(bullet_lines) == 1
    assert "\n" not in bullet_lines[0]
    assert "ship release step two step three" in bullet_lines[0]


async def test_prefetch_summarises_via_auxiliary_model() -> None:
    """When ``llm_providers`` is supplied the raw bullets pass through
    ``summarize_episodes`` on the role's auxiliary model.  Matches the
    spec's "cheap-model summarises episode hits" rule."""
    from crewlet.providers.llm.protocol import Completion

    class _ScriptedProvider:
        def __init__(self) -> None:
            self.calls: list[list[Any]] = []
            self.model = "aux"

        async def complete(self, messages, **kwargs):
            self.calls.append(list(messages))
            return Completion(content="- [done] compact summary line")

    provider = _ScriptedProvider()
    store = _EpisodeStoreStub([_mk_episode(task_summary="ship release v1")])
    turn, ctx = _mk_turn_and_ctx(episode_store=store)

    block = await _fetch_episode_recall_block(
        turn=turn,
        agent_context=ctx,
        llm_providers={"default": provider},  # type: ignore[dict-item]
    )

    # The aux model was invoked and its output is baked into the block.
    assert len(provider.calls) == 1
    assert "compact summary line" in block
    # Footer still appended so the planner knows it can drill deeper.
    assert "query_episodes" in block


async def test_prefetch_skips_summarization_when_summarize_false() -> None:
    """``summarize=False`` skips the aux summarisation call even when
    the provider pool is supplied.  Operators toggling
    ``learning.reflect.summarize_episodes=false`` get raw bullets --
    the pool is still passed (the sibling personal-memory /
    relevant-knowledge prefetches need it for their aux filters), only
    the episode summarisation is gated off."""
    from crewlet.providers.llm.protocol import Completion

    class _ScriptedProvider:
        def __init__(self) -> None:
            self.calls: list[list[Any]] = []
            self.model = "aux"

        async def complete(self, messages, **kwargs):
            self.calls.append(list(messages))
            return Completion(content="- [done] compact summary line")

    provider = _ScriptedProvider()
    store = _EpisodeStoreStub([_mk_episode(task_summary="ship release v1")])
    turn, ctx = _mk_turn_and_ctx(episode_store=store)

    block = await _fetch_episode_recall_block(
        turn=turn,
        agent_context=ctx,
        llm_providers={"default": provider},  # type: ignore[dict-item]
        summarize=False,
    )

    # The provider pool was passed but the summariser was NOT called.
    assert provider.calls == []
    # Raw bullets are surfaced instead of a summary.
    assert "ship release v1" in block
    assert "query_episodes" in block  # footer still appended


# ----------------------------------------------------------------------
# Prompt integration
# ----------------------------------------------------------------------


def _defn() -> AgentDefinition:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    resolved = org.get_role("Engineer")
    assert resolved is not None
    return AgentDefinition(role=resolved, org=org)


def test_prompt_shows_similar_prior_work_when_block_non_empty() -> None:
    block = "- [done] ship release v1 -- tools: slack_message"
    prompt = build_plan_prompt(
        _defn(),
        tool_catalogue="",
        episode_recall_block=block,
    )
    assert "## Similar prior work" in prompt
    assert "ship release v1" in prompt


def test_prompt_omits_section_when_block_empty() -> None:
    prompt = build_plan_prompt(_defn(), tool_catalogue="", episode_recall_block="")
    assert "Similar prior work" not in prompt


def test_prompt_section_order_recall_before_counterparty() -> None:
    prompt = build_plan_prompt(
        _defn(),
        tool_catalogue="",
        episode_recall_block="- [done] x",
        synthesized_skills_block="- **y**: z",
        counterparty_profile="Subject: bob",
    )
    # Synthesized skills → recall → counterparty → catalogue ordering.
    assert prompt.index("Synthesized skills") < prompt.index("Similar prior work")
    assert prompt.index("Similar prior work") < prompt.index("Known counterparty")
