"""Tracing-coverage tests for the ``learning.reflect`` span hierarchy.

Verifies that the ReflectEngine emits one ``learning.reflect`` span
per dispatched event, with one child span per fired worker carrying
``learning.outcome`` and worker-specific identifiers.  The dashboard
groups reflection cost / latency by these spans, so a regression here
breaks observability silently.
"""

from __future__ import annotations

from collections.abc import Iterator
from typing import Any

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import ReadableSpan, TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
    InMemorySpanExporter,
)

from crewlet.events.types import TurnCompleted
from crewlet.knowledge.protocol import AUTO_DRAFT_TITLE_PREFIX
from crewlet.learning.reflect_engine import ReflectEngine
from crewlet.org.models import Organization, OrgUnit, Role

# ----------------------------------------------------------------------
# Fixtures + stubs
# ----------------------------------------------------------------------


@pytest.fixture
def otel_exporter() -> Iterator[InMemorySpanExporter]:
    provider = trace.get_tracer_provider()
    assert isinstance(provider, TracerProvider)
    exporter = InMemorySpanExporter()
    processor = SimpleSpanProcessor(exporter)
    provider.add_span_processor(processor)
    try:
        yield exporter
    finally:
        processor.shutdown()
        multi = provider._active_span_processor  # type: ignore[attr-defined]
        current = list(getattr(multi, "_span_processors", ()))
        if processor in current:
            current.remove(processor)
            multi._span_processors = tuple(current)  # type: ignore[attr-defined]


def _find_span(spans: list[ReadableSpan], name: str) -> ReadableSpan | None:
    for s in spans:
        if s.name == name:
            return s
    return None


class _QueueStub:
    def __init__(self) -> None:
        self._handlers: list[tuple[str, Any]] = []

    async def subscribe(self, topic: str, group: str, handler) -> None:
        self._handlers.append((topic, handler))

    async def publish(self, topic: str, event: Any) -> None:  # pragma: no cover
        pass

    async def deliver(self, event: Any) -> None:
        for _topic, handler in self._handlers:
            await handler(event)


class _RecordingDecider:
    def __init__(self, doc_id: str | None = "doc-1") -> None:
        self._doc_id = doc_id

    async def decide_and_persist(self, **kwargs):
        from crewlet.learning.persist_decider import PersistOutcome, PersistResult

        if self._doc_id is None:
            return PersistOutcome(kind="NOOP")
        return PersistOutcome(
            kind="LONG",
            result=PersistResult(
                doc_id=self._doc_id, scope="agent", classification="memory_long"
            ),
        )


class _RecordingProfiler:
    async def update_from_turn(self, **kwargs) -> None:
        return None


class _RecordingRefiner:
    def __init__(self, refined: Any = None) -> None:
        self._refined = refined

    async def refine_from_turn(self, **kwargs):
        return self._refined


def _mk_org() -> Organization:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    return Organization(name="Acme", units=[unit])


def _mk_event(
    *,
    outcome: str = "done",
    skills_used: list[str] | None = None,
    trigger_sender_handle: str = "",
) -> TurnCompleted:
    from crewlet.learning.interaction import CanonicalIdentity, InboundInteraction

    interactions: list[InboundInteraction] = []
    if trigger_sender_handle:
        interactions = [
            InboundInteraction(
                sender=CanonicalIdentity(
                    handle=trigger_sender_handle,
                    platform="a2a",
                    display_name=trigger_sender_handle,
                ),
                body="",
            )
        ]
    return TurnCompleted(
        source="Engineer",
        agent_id="alice-id",
        agent_handle="eng",
        role="Engineer",
        turn_id="t1",
        task_id="task-1",
        task_summary="Do X",
        plan_summary="Plan: do X",
        tool_sequence=["search"],
        skills_used=skills_used or [],
        review_outcome=outcome,
        iterations=1,
        interactions=interactions,
    )


async def _boot(
    *,
    decider: Any = None,
    profiler: Any = None,
    refiner: Any = None,
) -> tuple[ReflectEngine, _QueueStub]:
    queue = _QueueStub()
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
    )
    await engine.start()
    if decider is not None:
        engine._persist_decider = decider  # type: ignore[assignment]
    else:
        engine._persist_decider = None  # type: ignore[assignment]
    engine._counterparty_profiler = profiler  # type: ignore[assignment]
    engine._skill_refiner = refiner  # type: ignore[assignment]
    return engine, queue


# ----------------------------------------------------------------------
# Outer ``learning.reflect`` span
# ----------------------------------------------------------------------


async def test_reflect_span_records_event_attributes(otel_exporter) -> None:
    _, queue = await _boot(decider=_RecordingDecider())
    await queue.deliver(_mk_event())
    spans = otel_exporter.get_finished_spans()
    reflect = _find_span(spans, "learning.reflect")
    assert reflect is not None
    attrs = dict(reflect.attributes or {})
    assert attrs["learning.turn_id"] == "t1"
    assert attrs["learning.agent_handle"] == "eng"
    assert attrs["learning.role"] == "Engineer"
    assert attrs["learning.review_outcome"] == "done"
    assert attrs["learning.is_terminal"] is True


async def test_reflect_span_records_skip_reason_when_no_workers(otel_exporter) -> None:
    _, queue = await _boot()  # all workers None
    await queue.deliver(_mk_event())
    spans = otel_exporter.get_finished_spans()
    reflect = _find_span(spans, "learning.reflect")
    assert reflect is not None
    assert (reflect.attributes or {}).get("learning.skip_reason") == "no_workers"
    # No worker children when nothing fires.
    assert _find_span(spans, "learning.persist_decider") is None


async def test_reflect_span_records_duplicate_skip(otel_exporter) -> None:
    _, queue = await _boot(decider=_RecordingDecider())
    event = _mk_event()
    await queue.deliver(event)
    otel_exporter.clear()
    await queue.deliver(event)  # redelivery
    spans = otel_exporter.get_finished_spans()
    reflect = _find_span(spans, "learning.reflect")
    assert reflect is not None
    assert (reflect.attributes or {}).get("learning.skip_reason") == "duplicate"


# ----------------------------------------------------------------------
# Per-worker child spans
# ----------------------------------------------------------------------


async def test_persist_decider_span_records_doc_id_on_write(otel_exporter) -> None:
    _, queue = await _boot(decider=_RecordingDecider(doc_id="doc-42"))
    await queue.deliver(_mk_event())
    spans = otel_exporter.get_finished_spans()
    decider_span = _find_span(spans, "learning.persist_decider")
    assert decider_span is not None
    attrs = dict(decider_span.attributes or {})
    assert attrs["learning.worker"] == "persist_decider"
    assert attrs["learning.outcome"] == "done"
    assert attrs["learning.doc_id"] == "doc-42"


async def test_persist_decider_span_records_noop_on_empty_doc(otel_exporter) -> None:
    _, queue = await _boot(decider=_RecordingDecider(doc_id=None))
    await queue.deliver(_mk_event())
    spans = otel_exporter.get_finished_spans()
    decider_span = _find_span(spans, "learning.persist_decider")
    assert decider_span is not None
    assert (decider_span.attributes or {}).get("learning.outcome") == "noop"
    assert "learning.doc_id" not in (decider_span.attributes or {})


async def test_persist_decider_span_records_skip_on_non_terminal(otel_exporter) -> None:
    _, queue = await _boot(decider=_RecordingDecider())
    await queue.deliver(_mk_event(outcome="self_iterate"))
    spans = otel_exporter.get_finished_spans()
    decider_span = _find_span(spans, "learning.persist_decider")
    assert decider_span is not None
    attrs = dict(decider_span.attributes or {})
    assert attrs["learning.outcome"] == "skipped"
    assert attrs["learning.skip_reason"] == "non_terminal"


async def test_counterparty_profiler_span_records_subject(otel_exporter) -> None:
    _, queue = await _boot(profiler=_RecordingProfiler())
    await queue.deliver(_mk_event(trigger_sender_handle="bob"))
    spans = otel_exporter.get_finished_spans()
    profiler_span = _find_span(spans, "learning.counterparty_profiler")
    assert profiler_span is not None
    attrs = dict(profiler_span.attributes or {})
    assert attrs["learning.outcome"] == "done"
    assert attrs["learning.subject_handle"] == "bob"


async def test_counterparty_profiler_span_absent_when_no_sender(otel_exporter) -> None:
    _, queue = await _boot(profiler=_RecordingProfiler())
    await queue.deliver(_mk_event())  # no trigger_sender_handle
    spans = otel_exporter.get_finished_spans()
    assert _find_span(spans, "learning.counterparty_profiler") is None


async def test_skill_refiner_span_records_skill_metadata(otel_exporter) -> None:
    from uuid import uuid4

    class _MockSkill:
        id = uuid4()
        name = "close-the-loop"
        version = 3

    _, queue = await _boot(refiner=_RecordingRefiner(refined=_MockSkill()))
    await queue.deliver(_mk_event(skills_used=["close-the-loop"]))
    spans = otel_exporter.get_finished_spans()
    refiner_span = _find_span(spans, "learning.skill_refiner")
    assert refiner_span is not None
    attrs = dict(refiner_span.attributes or {})
    assert attrs["learning.outcome"] == "done"
    assert attrs["learning.skill_name"] == "close-the-loop"
    assert attrs["learning.skill_version"] == 3


async def test_skill_refiner_span_records_noop_when_refiner_returns_none(
    otel_exporter,
) -> None:
    _, queue = await _boot(refiner=_RecordingRefiner(refined=None))
    await queue.deliver(_mk_event(skills_used=["close-the-loop"]))
    spans = otel_exporter.get_finished_spans()
    refiner_span = _find_span(spans, "learning.skill_refiner")
    assert refiner_span is not None
    assert (refiner_span.attributes or {}).get("learning.outcome") == "noop"


async def test_worker_span_records_failed_outcome_on_exception(otel_exporter) -> None:
    class _BoomDecider:
        async def decide_and_persist(self, **kwargs):
            raise RuntimeError("boom")

    _, queue = await _boot(decider=_BoomDecider())
    await queue.deliver(_mk_event())
    spans = otel_exporter.get_finished_spans()
    decider_span = _find_span(spans, "learning.persist_decider")
    assert decider_span is not None
    assert (decider_span.attributes or {}).get("learning.outcome") == "failed"


# ----------------------------------------------------------------------
# learning.summarize_episodes (Plan-prefetch aux call)
# ----------------------------------------------------------------------


async def test_summarize_episodes_span_records_no_provider(otel_exporter) -> None:
    """When ``llm_providers`` is empty the resolver raises and the
    helper short-circuits to raw payload — surface that as a skip on
    the span so the dashboard can show summarisation coverage."""
    from crewlet.learning.summarize import summarize_episodes

    role = Role(name="Engineer", handle="eng")
    out = await summarize_episodes(raw_payload="raw", role=role, llm_providers={})
    assert out == "raw"
    spans = otel_exporter.get_finished_spans()
    span = _find_span(spans, "learning.summarize_episodes")
    assert span is not None
    attrs = dict(span.attributes or {})
    assert attrs["learning.outcome"] == "skipped"
    assert attrs["learning.skip_reason"] == "provider_unavailable"


async def test_summarize_episodes_span_records_done_with_output_chars(
    otel_exporter,
) -> None:
    from crewlet.learning.summarize import summarize_episodes
    from crewlet.providers.llm.protocol import Completion

    class _Provider:
        model = "aux"

        async def complete(self, messages, **_):
            return Completion(content="- [done] compact bullet")

    role = Role(name="Engineer", handle="eng", llm="default", llm_auxiliary="default")
    out = await summarize_episodes(
        raw_payload="raw", role=role, llm_providers={"default": _Provider()}
    )
    assert "compact bullet" in out
    spans = otel_exporter.get_finished_spans()
    span = _find_span(spans, "learning.summarize_episodes")
    assert span is not None
    attrs = dict(span.attributes or {})
    assert attrs["learning.outcome"] == "done"
    assert attrs["learning.output_chars"] == len(out.strip())
    assert attrs["learning.payload_chars"] == 3
    assert attrs["learning.provider_key"] == "default"


async def test_summarize_episodes_span_records_failed_on_provider_exception(
    otel_exporter,
) -> None:
    from crewlet.learning.summarize import summarize_episodes

    class _BoomProvider:
        model = "aux"

        async def complete(self, messages, **_):
            raise RuntimeError("rate limit")

    role = Role(name="Engineer", handle="eng", llm="default", llm_auxiliary="default")
    out = await summarize_episodes(
        raw_payload="raw", role=role, llm_providers={"default": _BoomProvider()}
    )
    assert out == "raw"  # falls back to raw on failure
    spans = otel_exporter.get_finished_spans()
    span = _find_span(spans, "learning.summarize_episodes")
    assert span is not None
    assert (span.attributes or {}).get("learning.outcome") == "failed"


# ----------------------------------------------------------------------
# learning.skill_scheduler.tick + learning.skill_promotion
# ----------------------------------------------------------------------


async def test_scheduler_tick_span_records_counts(otel_exporter) -> None:
    from crewlet.learning.skill_scheduler import SkillClusteringScheduler

    class _AgentPool:
        agents = []  # no agents → tick returns 0, but the span still fires

    sched = SkillClusteringScheduler(
        synthesizer=object(),  # type: ignore[arg-type]
        episode_store=object(),  # type: ignore[arg-type]
        agent_pool=_AgentPool(),
        organization=_mk_org(),
    )
    made = await sched.tick_once()
    assert made == 0
    spans = otel_exporter.get_finished_spans()
    tick_span = _find_span(spans, "learning.skill_scheduler.tick")
    assert tick_span is not None
    attrs = dict(tick_span.attributes or {})
    assert attrs["learning.worker"] == "skill_scheduler"
    assert attrs["learning.agent_count"] == 0
    assert attrs["learning.agent_synthesised"] == 0
    assert attrs["learning.unit_promoted"] == 0
    assert attrs["learning.outcome"] == "done"


# ----------------------------------------------------------------------
# Lifecycle events (the dashboard's per-turn view consumes these)
# ----------------------------------------------------------------------


async def test_persist_decider_completed_event_published_on_write() -> None:
    """The dashboard groups events by ``trace_id`` for the per-turn view;
    PersistDeciderCompleted must publish to the queue with the doc_id +
    scope so it shows up under the parent turn's card."""
    from crewlet.events.types import PersistDeciderCompleted

    queue = _QueueStub()
    published: list[Any] = []

    async def _publish(topic: str, event: Any) -> None:
        published.append(event)

    queue.publish = _publish  # type: ignore[method-assign]

    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
    )
    await engine.start()
    engine._persist_decider = _RecordingDecider(doc_id="doc-42")  # type: ignore[assignment]
    engine._counterparty_profiler = None  # type: ignore[assignment]
    engine._skill_refiner = None  # type: ignore[assignment]

    await queue.deliver(_mk_event())

    decider_events = [e for e in published if isinstance(e, PersistDeciderCompleted)]
    assert len(decider_events) == 1
    ev = decider_events[0]
    assert ev.persisted is True
    assert ev.doc_id == "doc-42"
    assert ev.scope == "agent"
    assert ev.classification == "LONG"
    assert ev.ttl_until == ""  # LONG entries never expire
    assert ev.turn_id == "t1"
    assert ev.review_outcome == "done"


async def test_persist_decider_completed_event_published_on_noop() -> None:
    """NOOP paths still publish so the dashboard can show 'reflected, did
    not persist' alongside successful persists."""
    from crewlet.events.types import PersistDeciderCompleted

    queue = _QueueStub()
    published: list[Any] = []

    async def _publish(topic: str, event: Any) -> None:
        published.append(event)

    queue.publish = _publish  # type: ignore[method-assign]

    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
    )
    await engine.start()
    engine._persist_decider = _RecordingDecider(doc_id=None)  # type: ignore[assignment]
    engine._counterparty_profiler = None  # type: ignore[assignment]
    engine._skill_refiner = None  # type: ignore[assignment]

    await queue.deliver(_mk_event())

    decider_events = [e for e in published if isinstance(e, PersistDeciderCompleted)]
    assert len(decider_events) == 1
    ev = decider_events[0]
    assert ev.persisted is False
    assert ev.doc_id == ""
    assert ev.classification == "NOOP"
    assert ev.ttl_until == ""


async def test_skill_promotion_span_records_done_with_skill_name(
    otel_exporter,
) -> None:
    """The promotion pass emits one ``learning.skill_promotion`` span per
    promoted cluster.  The synthesizer returns a ``PromotionResult``
    pointing at a knowledge-base page;
    the span carries the LLM-picked name + the ``page_id`` attr.
    """
    from datetime import UTC, datetime
    from uuid import uuid4

    from crewlet.learning.models import SynthesizedSkill
    from crewlet.learning.skill_scheduler import SkillClusteringScheduler
    from crewlet.learning.skill_synthesizer import PromotionResult

    def _mk_skill(*, agent_handle: str, name: str) -> SynthesizedSkill:
        return SynthesizedSkill(
            id=uuid4(),
            agent_handle=agent_handle,
            name=name,
            description=f"{name} by {agent_handle}",
            content="body",
            tool_sequence=["x", "y", "z"],
            created_at=datetime(2026, 4, 23, tzinfo=UTC),
            updated_at=datetime(2026, 4, 23, tzinfo=UTC),
        )

    class _AgentStub:
        def __init__(self, handle: str, role_name: str) -> None:
            self.handle = handle
            self.role_name = role_name
            self.definition = type("_D", (), {"role": Role(name=role_name)})()

    class _AgentPool:
        agents = [
            _AgentStub("alice", "Alice"),
            _AgentStub("bob", "Bob"),
            _AgentStub("cara", "Cara"),
        ]

    org = Organization(
        name="Acme",
        units=[
            OrgUnit(
                name="Eng",
                type="team",
                lead="Alice",
                roles=[
                    Role(name="Alice", handle="alice"),
                    Role(name="Bob", handle="bob"),
                    Role(name="Cara", handle="cara"),
                ],
            )
        ],
    )

    class _Store:
        async def list_for_agent_handles(self, handles):
            return [_mk_skill(agent_handle=h, name=f"{h}-ship") for h in handles]

    class _PromoSynth:
        async def synthesize_promotion(self, **_kw):
            return PromotionResult(
                skill_name="ship-routine",
                page_id="PAGE-1",
                page_title=f"{AUTO_DRAFT_TITLE_PREFIX}ship-routine",
                container_key="ENG",
            )

    sched = SkillClusteringScheduler(
        synthesizer=object(),  # type: ignore[arg-type]
        episode_store=object(),  # type: ignore[arg-type]
        agent_pool=_AgentPool(),
        organization=org,
        promotion_synthesizer=_PromoSynth(),  # type: ignore[arg-type]
        synthesized_skill_store=_Store(),  # type: ignore[arg-type]
        promotion_enabled=True,
        promotion_min_sibling_count=3,
        promotion_jaccard_threshold=0.5,
    )
    made = await sched.tick_once()
    assert made == 1
    spans = otel_exporter.get_finished_spans()
    promo_span = _find_span(spans, "learning.skill_promotion")
    assert promo_span is not None
    attrs = dict(promo_span.attributes or {})
    assert attrs["learning.outcome"] == "done"
    assert attrs["learning.skill_name"] == "ship-routine"
    assert attrs["learning.unit_id"] == "Eng"
    assert attrs["learning.page_id"] == "PAGE-1"
    assert attrs["learning.distinct_agents"] == 3
