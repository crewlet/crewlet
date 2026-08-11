"""Telemetry coverage for skill-use tracking and prefetch summaries.

Two surfaces under test:

1. ``SynthesizedSkillStore.mark_used`` -- bumps ``use_count`` and
   stamps ``last_used_at`` so the ``learning_health`` view can
   answer whether synthesized skills are actually being used.
2. ``_use_skill`` -- on each successful load, calls ``mark_used``
   and emits a ``SkillUsed`` event (only the synthesized-skill
   path exists).
3. ``PlanPrefetchSummary`` -- one event per turn after the Plan-
   phase prefetches resolve, recording per-block hit + bytes.
"""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any
from uuid import UUID, uuid4

from crewlet.events.types import PlanPrefetchSummary, SkillUsed
from crewlet.learning.interaction import InboundInteraction
from crewlet.learning.models import SynthesizedSkill
from crewlet.learning.synthesized_skill_store import SynthesizedSkillStore
from crewlet.tools.builtin import _emit_skill_used, _use_skill
from crewlet.tools.protocol import AgentContext

# ---------------------------------------------------------------------------
# Stubs
# ---------------------------------------------------------------------------


class _DBStub:
    def __init__(self, fetch_rows: list[dict[str, Any]] | None = None) -> None:
        self.executed: list[tuple[str, tuple[Any, ...]]] = []
        self._fetch_rows = fetch_rows or []

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        self.executed.append((query, args))
        return list(self._fetch_rows)


class _EventQueueStub:
    def __init__(self) -> None:
        self.published: list[tuple[str, Any]] = []

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


class _BoomEventQueue:
    """Raises on publish; used to verify telemetry never breaks the host."""

    async def publish(self, topic: str, event: Any) -> None:
        raise RuntimeError("queue down")


class _SynthesizedStoreStub:
    """Minimal store stub for ``_use_skill`` synthesized-path tests."""

    def __init__(self, skill: SynthesizedSkill | None) -> None:
        self._skill = skill
        self.fetched: list[dict[str, Any]] = []
        self.marks_used: list[UUID] = []

    async def fetch(
        self,
        *,
        agent_handle: str,
        name: str,
        include_archived: bool = False,
    ) -> SynthesizedSkill | None:
        self.fetched.append({"agent_handle": agent_handle, "name": name})
        return self._skill

    async def mark_used(
        self,
        skill_id: UUID,
        *,
        event_queue: Any = None,
        skill_name: str = "",
        agent_handle: str = "",
    ) -> None:
        self.marks_used.append(skill_id)


def _mk_synthesized(name: str = "close-the-loop") -> SynthesizedSkill:
    return SynthesizedSkill(
        agent_handle="alice",
        name=name,
        description="Close the loop after a ship.",
        content="## Close the loop\n1. Ship.\n2. Post update.",
        frontmatter={},
        tool_sequence=["slack_message", "jira_comment"],
        source_episode_ids=[],
        version=1,
        created_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        updated_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
    )


# ---------------------------------------------------------------------------
# Store: mark_used + parsing
# ---------------------------------------------------------------------------


async def test_mark_used_issues_one_update() -> None:
    db = _DBStub()
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    skill_id = uuid4()
    await store.mark_used(skill_id)
    assert len(db.executed) == 1
    sql, args = db.executed[0]
    # Idempotently increments + stamps last_used_at.
    assert "UPDATE synthesized_skills" in sql
    assert "use_count + 1" in sql
    assert "last_used_at" in sql
    assert args == (str(skill_id),)


async def test_mark_used_revives_a_stale_skill() -> None:
    """Using a skill again revives it: the same atomic use-count bump
    flips a ``stale`` row back to ``active`` and clears ``stale_at``,
    so a freshly-used skill is visible to the next Plan prefetch
    immediately -- not only after the curator's next interval tick."""
    db = _DBStub()
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    await store.mark_used(uuid4())
    sql, _ = db.executed[0]
    # The state flip is part of the same UPDATE, gated on stale.
    assert "state = CASE WHEN state = 'stale' THEN 'active'" in sql
    assert "stale_at = CASE WHEN state = 'stale' THEN NULL" in sql


async def test_mark_used_swallows_db_failure() -> None:
    """Telemetry must never break a skill load -- a DB failure inside
    ``mark_used`` is logged and silently absorbed."""

    class _BoomDB:
        async def execute(self, *args: Any, **kwargs: Any) -> Any:
            raise RuntimeError("connection reset")

    store = SynthesizedSkillStore(_BoomDB())  # type: ignore[arg-type]
    # Should not raise.
    await store.mark_used(uuid4())


async def test_use_count_and_last_used_at_parsed_from_row() -> None:
    """``_row_to_skill`` populates the new telemetry fields; absent
    columns default to ``0`` / ``None`` so older snapshots still parse."""
    import json as _json

    row = {
        "id": uuid4(),
        "agent_handle": "alice",
        "name": "close-the-loop",
        "description": "d",
        "content": "b",
        "frontmatter": _json.dumps({}),
        "tool_sequence": _json.dumps([]),
        "source_episode_ids": _json.dumps([]),
        "version": 1,
        "created_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        "updated_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        "use_count": 7,
        "last_used_at": datetime(2026, 4, 30, 14, 0, tzinfo=UTC),
    }
    db = _DBStub(fetch_rows=[row])
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    fetched = await store.fetch(agent_handle="alice", name="close-the-loop")
    assert fetched is not None
    assert fetched.use_count == 7
    assert fetched.last_used_at == datetime(2026, 4, 30, 14, 0, tzinfo=UTC)


async def test_missing_telemetry_columns_default_to_zero_none() -> None:
    """Until the migration runs, rows lack use_count + last_used_at;
    parsing must not blow up -- defaults take over."""
    import json as _json

    row = {
        "id": uuid4(),
        "agent_handle": "alice",
        "name": "n",
        "description": "d",
        "content": "b",
        "frontmatter": _json.dumps({}),
        "tool_sequence": _json.dumps([]),
        "source_episode_ids": _json.dumps([]),
        "version": 1,
        "created_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        "updated_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
    }
    db = _DBStub(fetch_rows=[row])
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    fetched = await store.fetch(agent_handle="alice", name="n")
    assert fetched is not None
    assert fetched.use_count == 0
    assert fetched.last_used_at is None


# ---------------------------------------------------------------------------
# _use_skill: synthesized path bumps + emits
# ---------------------------------------------------------------------------


async def test_synthesized_load_marks_used_and_emits_skill_used() -> None:
    skill = _mk_synthesized()
    store = _SynthesizedStoreStub(skill)
    queue = _EventQueueStub()
    context = AgentContext(
        agent_id="alice-id",
        agent_handle="alice",
        role="Engineer",
        synthesized_skill_store=store,
        event_queue=queue,
    )
    result = await _use_skill({"skill_name": "close-the-loop"}, context)
    assert result.success
    # mark_used fired with the synthesized skill's UUID.
    assert store.marks_used == [skill.id]
    # SkillUsed event published with source_kind="synthesized".
    topics = [t for t, _ in queue.published]
    assert "crewlet.events.skill_used" in topics
    event = next(e for t, e in queue.published if t == "crewlet.events.skill_used")
    assert isinstance(event, SkillUsed)
    assert event.skill_name == "close-the-loop"
    assert event.skill_id == str(skill.id)
    assert event.source_kind == "synthesized"
    assert event.agent_handle == "alice"
    assert event.role == "Engineer"


async def test_synthesized_mark_used_failure_does_not_break_load() -> None:
    """If mark_used raises, the user still gets the skill body --
    telemetry is best-effort."""

    class _BoomMarkStore(_SynthesizedStoreStub):
        async def mark_used(
            self,
            skill_id: UUID,
            *,
            event_queue: Any = None,
            skill_name: str = "",
            agent_handle: str = "",
        ) -> None:
            raise RuntimeError("db down")

    skill = _mk_synthesized()
    store = _BoomMarkStore(skill)
    queue = _EventQueueStub()
    context = AgentContext(
        agent_id="alice-id",
        agent_handle="alice",
        role="Engineer",
        synthesized_skill_store=store,
        event_queue=queue,
    )
    result = await _use_skill({"skill_name": "close-the-loop"}, context)
    assert result.success
    # Event still fires despite the bookkeeping failure.
    assert any(t == "crewlet.events.skill_used" for t, _ in queue.published)


async def test_emit_skill_used_swallows_publish_failure() -> None:
    """A publish failure inside the helper is logged once and never
    surfaced to the tool path."""
    context = AgentContext(
        agent_id="x",
        agent_handle="x",
        role="X",
        event_queue=_BoomEventQueue(),
    )
    # Should not raise.
    await _emit_skill_used(context, skill_name="x", skill_id="", source="synthesized")


async def test_no_event_queue_emits_nothing() -> None:
    """Test mode (no event_queue wired) is a silent no-op."""
    skill = _mk_synthesized()
    store = _SynthesizedStoreStub(skill)
    context = AgentContext(
        agent_id="alice-id",
        agent_handle="alice",
        role="Engineer",
        synthesized_skill_store=store,
    )
    result = await _use_skill({"skill_name": "close-the-loop"}, context)
    assert result.success
    # mark_used still fires (it's not gated on event_queue).
    assert store.marks_used == [skill.id]


# ---------------------------------------------------------------------------
# PlanPrefetchSummary -- pure event-shape sanity
# ---------------------------------------------------------------------------


async def test_emit_plan_prefetch_summary_publishes_with_byte_counts() -> None:
    """The helper called from ``run_plan_phase`` emits one summary event
    per turn with hit / bytes per block."""
    from types import SimpleNamespace

    from crewlet.agent.plan import _emit_plan_prefetch_summary

    queue = _EventQueueStub()
    turn = SimpleNamespace(
        agent=SimpleNamespace(
            handle="alice",
            id=uuid4(),
            definition=SimpleNamespace(role=SimpleNamespace(name="Engineer")),
        ),
        turn_id=uuid4(),
        trigger_event=None,
        trigger_interactions=lambda: [],
    )
    await _emit_plan_prefetch_summary(
        turn=turn,  # type: ignore[arg-type]
        event_queue=queue,  # type: ignore[arg-type]
        counterparty="Subject: Bob\nObserved by you: prefers terse",
        synthesized="",  # miss
        recall="## Similar prior work\n- did the thing once",
        onboarding="",  # miss
        personal_memory="- Stakeholder X prefers digests",
        relevant_knowledge="- **Code Review Checklist**: ...",
    )
    events = [
        e for t, e in queue.published if t == "crewlet.events.plan_prefetch_summary"
    ]
    assert len(events) == 1
    ev = events[0]
    assert isinstance(ev, PlanPrefetchSummary)
    assert ev.counterparty_hit is True
    assert ev.counterparty_bytes > 0
    assert ev.synthesized_skills_hit is False
    assert ev.synthesized_skills_bytes == 0
    assert ev.episode_recall_hit is True
    assert ev.episode_recall_bytes > 0
    assert ev.onboarding_hint_hit is False
    assert ev.personal_memory_hit is True
    assert ev.personal_memory_bytes > 0
    assert ev.relevant_knowledge_hit is True
    assert ev.relevant_knowledge_bytes > 0
    # One bullet rendered (one ``- **`` marker) → selection_count=1.
    assert ev.relevant_knowledge_selection_count == 1
    assert ev.agent_handle == "alice"
    assert ev.role == "Engineer"
    # No trigger_event on the stub → not a recon-required trigger.
    assert ev.trigger_requires_recon is False


async def test_emit_plan_prefetch_summary_records_thin_trigger_gate() -> None:
    """When the trigger is a recon-required webhook, the emitted
    summary carries ``trigger_requires_recon=True`` -- the field that
    tells an operator the prefetch blocks are empty because they were
    *gated*, not because the filters ran and found nothing."""
    from types import SimpleNamespace

    from crewlet.agent.plan import _emit_plan_prefetch_summary
    from crewlet.events.types import ExternalNotification

    queue = _EventQueueStub()
    turn = SimpleNamespace(
        agent=SimpleNamespace(
            handle="alice",
            id=uuid4(),
            definition=SimpleNamespace(role=SimpleNamespace(name="Engineer")),
        ),
        turn_id=uuid4(),
        trigger_event=ExternalNotification(
            notification_source="jira",
            sender="Manager",
            body="A Jira event has been routed to you.",
            metadata={"issue_key": "POC-528"},
            context_requires_recon=True,
        ),
    )
    turn.trigger_interactions = lambda: InboundInteraction.list_from_trigger_event(
        turn.trigger_event
    )
    await _emit_plan_prefetch_summary(
        turn=turn,  # type: ignore[arg-type]
        event_queue=queue,  # type: ignore[arg-type]
        counterparty="",
        synthesized="",
        recall="",
        onboarding="",
        personal_memory="",
        relevant_knowledge="",
    )
    events = [
        e for t, e in queue.published if t == "crewlet.events.plan_prefetch_summary"
    ]
    ev = events[0]
    assert ev.trigger_requires_recon is True
    # The event summary surfaces the gate so the trace view reads it.
    assert "thin trigger" in ev.summary


async def test_emit_plan_prefetch_summary_distinguishes_hint_from_picks() -> None:
    """``relevant_knowledge_hit=True`` covers both "filter picked docs"
    and "filter rendered EMPTY_FILTER_HINT".
    ``relevant_knowledge_selection_count`` is the field that separates
    the two cases."""
    from types import SimpleNamespace

    from crewlet.agent.plan import _emit_plan_prefetch_summary
    from crewlet.learning.relevant_knowledge import EMPTY_FILTER_HINT

    queue = _EventQueueStub()
    turn = SimpleNamespace(
        agent=SimpleNamespace(
            handle="alice",
            id=uuid4(),
            definition=SimpleNamespace(role=SimpleNamespace(name="Engineer")),
        ),
        turn_id=uuid4(),
        trigger_event=None,
        trigger_interactions=lambda: [],
    )
    # The real empty-filter hint -- no ``- **`` markers in the text.
    hint_text = EMPTY_FILTER_HINT
    await _emit_plan_prefetch_summary(
        turn=turn,  # type: ignore[arg-type]
        event_queue=queue,  # type: ignore[arg-type]
        counterparty="",
        synthesized="",
        recall="",
        onboarding="",
        personal_memory="",
        relevant_knowledge=hint_text,
    )
    events = [
        e for t, e in queue.published if t == "crewlet.events.plan_prefetch_summary"
    ]
    ev = events[0]
    assert ev.relevant_knowledge_hit is True  # block rendered
    assert ev.relevant_knowledge_selection_count == 0  # but with no real picks


async def test_emit_plan_prefetch_summary_swallows_publish_failure() -> None:
    from types import SimpleNamespace

    from crewlet.agent.plan import _emit_plan_prefetch_summary

    turn = SimpleNamespace(
        agent=SimpleNamespace(
            handle="alice",
            id=uuid4(),
            definition=SimpleNamespace(role=SimpleNamespace(name="Engineer")),
        ),
        turn_id=uuid4(),
        trigger_event=None,
        trigger_interactions=lambda: [],
    )
    # Should not raise.
    await _emit_plan_prefetch_summary(
        turn=turn,  # type: ignore[arg-type]
        event_queue=_BoomEventQueue(),  # type: ignore[arg-type]
        counterparty="x",
        synthesized="",
        recall="",
        onboarding="",
        personal_memory="",
        relevant_knowledge="",
    )


def test_prefetch_summary_summary_string_counts_hits() -> None:
    event = PlanPrefetchSummary(
        source="alice",
        agent_id="alice-id",
        agent_handle="alice",
        role="Engineer",
        turn_id="t1",
        counterparty_hit=True,
        counterparty_bytes=120,
        synthesized_skills_hit=False,
        synthesized_skills_bytes=0,
        episode_recall_hit=True,
        episode_recall_bytes=300,
        onboarding_hint_hit=False,
        onboarding_hint_bytes=0,
        personal_memory_hit=True,
        personal_memory_bytes=500,
        relevant_knowledge_hit=False,
        relevant_knowledge_bytes=0,
    )
    assert "3/6 hits" in event.summary


def test_prefetch_summary_zero_hits_renders() -> None:
    event = PlanPrefetchSummary(
        source="alice",
        turn_id="t2",
    )
    assert "0/6 hits" in event.summary
