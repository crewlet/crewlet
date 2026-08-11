"""Tests for the ReflectEngine gates.

Covers the ``_reflect`` pre-flight logic:

* Terminal-outcome filter: PersistDecider + SkillRefiner skip
  ``self_iterate``; the counterparty profiler runs regardless.
* Budget pre-flight: when the shared :class:`BudgetManager` reports
  the agent or org exhausted, the whole reflection pass short-
  circuits before any worker fires.
* Concurrency slot: the engine acquires a per-role slot on the
  shared :class:`ConcurrencyController` around worker dispatch and
  releases it on success and on failure.
"""

from __future__ import annotations

from typing import Any

from crewlet.concurrency import BudgetManager, ConcurrencyController
from crewlet.events.types import TurnCompleted
from crewlet.learning.reflect_engine import (
    CONSUMER_GROUP,
    TURN_COMPLETED_TOPIC,
    ReflectEngine,
)
from crewlet.org.models import Organization, OrgUnit, Role


class _QueueStub:
    def __init__(self) -> None:
        self.subscriptions: list[tuple[str, str]] = []
        self._handlers: list[tuple[str, Any]] = []

    async def subscribe(self, topic: str, group: str, handler) -> None:
        self.subscriptions.append((topic, group))
        self._handlers.append((topic, handler))

    async def publish(self, topic: str, event: Any) -> None:  # pragma: no cover
        pass

    async def deliver(self, event: Any) -> None:
        for topic, handler in self._handlers:
            if topic == TURN_COMPLETED_TOPIC:
                await handler(event)


class _RecordingDecider:
    def __init__(self) -> None:
        self.calls: list[dict[str, Any]] = []

    async def decide_and_persist(self, **kwargs) -> str | None:
        self.calls.append(kwargs)
        return "doc-1"


class _RecordingProfiler:
    def __init__(self) -> None:
        self.calls: list[dict[str, Any]] = []

    async def update_from_turn(self, **kwargs) -> None:
        self.calls.append(kwargs)


class _RecordingRefiner:
    def __init__(self) -> None:
        self.calls: list[dict[str, Any]] = []

    async def refine_from_turn(self, **kwargs):
        self.calls.append(kwargs)
        return None


def _mk_org() -> Organization:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    return Organization(name="Acme", units=[unit])


def _mk_event(
    *,
    outcome: str = "done",
    agent_id: str = "alice-id",
    trigger_sender_handle: str = "",
    skills_used: list[str] | None = None,
    plan_decision: str = "",
    tool_sequence: list[str] | None = None,
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
        agent_id=agent_id,
        agent_handle="eng",
        role="Engineer",
        turn_id="t1",
        task_id="task-1",
        task_summary="Do X",
        plan_summary="Plan: do X",
        tool_sequence=tool_sequence if tool_sequence is not None else ["search"],
        skills_used=skills_used or [],
        review_outcome=outcome,
        iterations=1,
        plan_decision=plan_decision,
        interactions=interactions,
    )


async def _boot_engine(
    *,
    concurrency: Any = None,
    budget_manager: Any = None,
) -> tuple[
    ReflectEngine, _QueueStub, _RecordingDecider, _RecordingProfiler, _RecordingRefiner
]:
    queue = _QueueStub()
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
        concurrency=concurrency,
        budget_manager=budget_manager,
    )
    await engine.start()
    decider = _RecordingDecider()
    profiler = _RecordingProfiler()
    refiner = _RecordingRefiner()
    engine._persist_decider = decider  # type: ignore[assignment]
    engine._counterparty_profiler = profiler  # type: ignore[assignment]
    engine._skill_refiner = refiner  # type: ignore[assignment]
    return engine, queue, decider, profiler, refiner


# ----------------------------------------------------------------------
# Gate 1 — terminal-outcome filter
# ----------------------------------------------------------------------


async def test_persist_decider_skips_self_iterate() -> None:
    _, queue, decider, _, _ = await _boot_engine()
    await queue.deliver(_mk_event(outcome="self_iterate"))
    assert decider.calls == []


async def test_persist_decider_runs_on_done() -> None:
    _, queue, decider, _, _ = await _boot_engine()
    await queue.deliver(_mk_event(outcome="done"))
    assert len(decider.calls) == 1


async def test_persist_decider_runs_on_failed() -> None:
    _, queue, decider, _, _ = await _boot_engine()
    await queue.deliver(_mk_event(outcome="failed"))
    assert len(decider.calls) == 1


async def test_skill_refiner_skips_non_terminal_outcomes() -> None:
    _, queue, _, _, refiner = await _boot_engine()
    await queue.deliver(
        _mk_event(outcome="self_iterate", skills_used=["close-the-loop"])
    )
    assert refiner.calls == []


async def test_skill_refiner_runs_on_terminal_outcomes() -> None:
    _, queue, _, _, refiner = await _boot_engine()
    await queue.deliver(_mk_event(outcome="done", skills_used=["close-the-loop"]))
    assert len(refiner.calls) == 1


async def test_counterparty_profiler_runs_on_non_terminal() -> None:
    """The profiler observes the counterparty regardless of outcome
    — unlike the decider / refiner, handoffs and retries are still
    useful signal about the subject's communication style.
    """
    _, queue, _, profiler, _ = await _boot_engine()
    await queue.deliver(_mk_event(outcome="self_iterate", trigger_sender_handle="bob"))
    assert len(profiler.calls) == 1


# ----------------------------------------------------------------------
# Gate 2 — budget pre-flight
# ----------------------------------------------------------------------


async def test_reflection_skipped_when_org_budget_exhausted() -> None:
    budget = BudgetManager(org_budget=100)
    assert await budget.consume("alice-id", 100) is True  # drains to 0
    assert budget.org_budget.is_exhausted

    _, queue, decider, profiler, refiner = await _boot_engine(budget_manager=budget)
    await queue.deliver(
        _mk_event(skills_used=["close-the-loop"], trigger_sender_handle="bob")
    )
    # Every worker skipped.
    assert decider.calls == []
    assert profiler.calls == []
    assert refiner.calls == []


async def test_reflection_skipped_when_agent_budget_exhausted() -> None:
    budget = BudgetManager(org_budget=10_000)
    budget.set_agent_budget("alice-id", 100)
    assert await budget.consume("alice-id", 100) is True
    agent_budget = budget.get_agent_budget("alice-id")
    assert agent_budget is not None
    assert agent_budget.is_exhausted

    _, queue, decider, profiler, refiner = await _boot_engine(budget_manager=budget)
    await queue.deliver(
        _mk_event(
            agent_id="alice-id",
            skills_used=["close-the-loop"],
            trigger_sender_handle="bob",
        )
    )
    assert decider.calls == []
    assert profiler.calls == []
    assert refiner.calls == []


async def test_reflection_runs_when_budget_has_headroom() -> None:
    budget = BudgetManager(org_budget=10_000)
    budget.set_agent_budget("alice-id", 5_000)
    _, queue, decider, _, _ = await _boot_engine(budget_manager=budget)
    await queue.deliver(_mk_event())
    assert len(decider.calls) == 1


async def test_reflection_no_budget_manager_treated_as_unlimited() -> None:
    _, queue, decider, _, _ = await _boot_engine(budget_manager=None)
    await queue.deliver(_mk_event())
    assert len(decider.calls) == 1


# ----------------------------------------------------------------------
# Gate 3 — concurrency slot
# ----------------------------------------------------------------------


async def _all_slots_free(concurrency: ConcurrencyController) -> bool:
    """All ``max_concurrent`` slots can be acquired back-to-back."""
    for _ in range(concurrency.max_concurrent):
        await concurrency.acquire()
    for _ in range(concurrency.max_concurrent):
        concurrency.release()
    return True


async def test_concurrency_slot_acquired_and_released_on_success() -> None:
    concurrency = ConcurrencyController(max_concurrent=2)
    _, queue, decider, _, _ = await _boot_engine(concurrency=concurrency)

    await queue.deliver(_mk_event())

    assert len(decider.calls) == 1
    # After reflection returns, all slots are back in the pool.
    assert await _all_slots_free(concurrency)


async def test_concurrency_slot_released_on_worker_exception() -> None:
    concurrency = ConcurrencyController(max_concurrent=2)
    _, queue, _, _, _ = await _boot_engine(concurrency=concurrency)

    class _BoomDecider:
        async def decide_and_persist(self, **kwargs):
            raise RuntimeError("boom")

    # Replace the decider with one that raises.
    engine_task_handlers = queue._handlers
    engine = engine_task_handlers[0][1].__self__  # type: ignore[attr-defined]
    engine._persist_decider = _BoomDecider()  # type: ignore[assignment]

    await queue.deliver(_mk_event())
    # Even with a worker failure the slot is released.
    assert await _all_slots_free(concurrency)


async def test_reflection_returns_cleanly_when_acquire_fails() -> None:
    class _BoomConcurrency:
        async def acquire(self, role: str = ""):
            raise RuntimeError("no slots")

        def release(self, role: str = ""):  # pragma: no cover
            pass

        @property
        def available(self) -> int:  # pragma: no cover
            return 0

    _, queue, decider, _, _ = await _boot_engine(concurrency=_BoomConcurrency())
    # Must not raise, and no workers should run.
    await queue.deliver(_mk_event())
    assert decider.calls == []


async def test_concurrency_no_slot_wiring_works_without_controller() -> None:
    _, queue, decider, _, _ = await _boot_engine(concurrency=None)
    await queue.deliver(_mk_event())
    assert len(decider.calls) == 1


# ----------------------------------------------------------------------
# Gate 4 — per-role learning_enabled flag
# ----------------------------------------------------------------------


async def test_role_learning_disabled_skips_all_workers() -> None:
    queue = _QueueStub()
    org = _mk_org()
    role = org.get_role("Engineer")
    assert role is not None
    role.learning_enabled = False
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=org,
    )
    await engine.start()
    decider = _RecordingDecider()
    profiler = _RecordingProfiler()
    refiner = _RecordingRefiner()
    engine._persist_decider = decider  # type: ignore[assignment]
    engine._counterparty_profiler = profiler  # type: ignore[assignment]
    engine._skill_refiner = refiner  # type: ignore[assignment]

    await queue.deliver(
        _mk_event(skills_used=["close-the-loop"], trigger_sender_handle="bob")
    )

    assert decider.calls == []
    assert profiler.calls == []
    assert refiner.calls == []


async def test_role_learning_enabled_none_inherits_system_level() -> None:
    # Default learning_enabled=None → reflection runs (system-level
    # controls are handled at engine-construction time, not here).
    _, queue, decider, _, _ = await _boot_engine()
    await queue.deliver(_mk_event())
    assert len(decider.calls) == 1


# ----------------------------------------------------------------------
# Gate 5 — redelivery idempotency
# ----------------------------------------------------------------------


async def test_duplicate_turn_id_delivery_processed_once() -> None:
    _, queue, decider, _, _ = await _boot_engine()
    event = _mk_event()
    await queue.deliver(event)
    await queue.deliver(event)  # redelivery
    await queue.deliver(event)  # still a duplicate
    assert len(decider.calls) == 1


# ----------------------------------------------------------------------
# Constants are importable
# ----------------------------------------------------------------------


def test_constants_exported() -> None:
    assert TURN_COMPLETED_TOPIC == "crewlet.events.turn_completed"
    assert CONSUMER_GROUP == "reflect-engine"


# ----------------------------------------------------------------------
# Gate — agent didn't engage
# ----------------------------------------------------------------------
# Two routes produce phantom learning if not blocked:
#   (a) plan_decision == "skip" -- planner explicitly opted out.
#   (b) plan_decision != "skip" but tool_sequence is empty and outcome
#       is "done" -- planner failed to call submit_plan, system coerced
#       to decision="direct", Execute ran with full registry but emitted
#       nothing actionable.
# Both produce the same false-learning outcome and both are gated here.


async def test_plan_decision_skip_short_circuits_all_workers() -> None:
    """Route (a): planner explicitly returned ``decision="skip"``.

    The phantom-learning scenario this blocks: agent-pm receives a
    Slack DM addressed to agent-ceo (``<@U0TESTUSER3> hi, I want you to
    always answer me with hey sam first``); the planner correctly
    returns ``decision="skip"`` with no tool calls; without the gate,
    the post-turn aux LLM would still classify LONG and store "User Sam
    prefers replies opened with 'hey sam'" into agent-pm's personal
    memory.
    """
    _, queue, decider, profiler, refiner = await _boot_engine()
    await queue.deliver(
        _mk_event(
            plan_decision="skip",
            outcome="done",
            tool_sequence=[],
            trigger_sender_handle="sam",
            skills_used=["close-the-loop"],
        )
    )
    assert decider.calls == []
    assert profiler.calls == []
    assert refiner.calls == []


async def test_no_tools_executed_short_circuits_all_workers() -> None:
    """Route (b): planner intended to skip but didn't call submit_plan,
    so the system coerced to ``decision="direct"`` and Execute ran with
    the full registry but produced no tool calls.  Same phantom-learning
    bug as route (a) -- the agent didn't actually engage with the
    trigger, so learning from it would store directives meant for
    someone else.

    The scenario: agent-cto receives the same Slack DM addressed
    to agent-ceo; the planner emits text saying "I'll skip this
    message" but never calls submit_plan; the system falls back to
    decision="direct"; Execute also says skip, no tools called;
    without the gate, PersistDecider would store "User Sam prefers
    replies opened with 'hey sam'" into agent-cto's memory.
    """
    _, queue, decider, profiler, refiner = await _boot_engine()
    await queue.deliver(
        _mk_event(
            plan_decision="direct",
            outcome="done",
            tool_sequence=[],  # no tool calls -> no engagement
            trigger_sender_handle="sam",
            skills_used=["close-the-loop"],
        )
    )
    assert decider.calls == []
    assert profiler.calls == []
    assert refiner.calls == []


async def test_done_turn_with_tools_called_runs_normally() -> None:
    """Sanity: the broader gate must NOT block real engaged turns.
    A ``done`` turn that called even one tool is genuine work and
    should produce learning."""
    _, queue, decider, _, _ = await _boot_engine()
    await queue.deliver(
        _mk_event(plan_decision="plan", outcome="done", tool_sequence=["search"])
    )
    assert len(decider.calls) == 1


async def test_empty_plan_decision_with_tools_does_not_short_circuit() -> None:
    """Backward-compat: events from older publishers (no plan_decision
    field) still fall through to the regular gates as long as the
    agent actually called tools."""
    _, queue, decider, _, _ = await _boot_engine()
    await queue.deliver(
        _mk_event(plan_decision="", outcome="done", tool_sequence=["search"])
    )
    assert len(decider.calls) == 1
