"""The posture gate: what a node that cannot apply the current config
does about the work it would otherwise take.

Three properties, each of which was a bug in an earlier draft:

1. **The gate sits at trigger admission, not at ``run_turn``.** A stale
   node still *consumes* inbound messages, and Slack HMAC verification
   happens consume-side against that node's cached signing secret —
   where a failure is a skip plus an ack.  So a node that missed a
   rotation does not merely lag; it silently eats its share of the
   fleet's inbound messages.
2. **Refusal is republish-then-ack, never NAK.** Three redeliveries at
   one second dead-letter a perfectly healthy event, and a shed can last
   minutes.
3. **The pause is reason-scoped.** The sandbox busy gate and the config
   shed hold the same topics; if either could release the other's hold,
   a converging node would un-gate a seat mid-sandbox, or a completing
   sandbox would un-gate a diverged node.
"""

from __future__ import annotations

import asyncio
import sys
from collections.abc import AsyncIterator
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent.parent))
from crewlet.db.config_plane import Posture
from crewlet.engine import Engine  # noqa: E402
from crewlet.org.models import Organization, OrgUnit, Role  # noqa: E402
from crewlet.providers.llm.protocol import (  # noqa: E402
    Completion,
    CompletionChunk,
    Message,
    ToolDef,
)
from crewlet.queue.memory import MemoryEventQueue  # noqa: E402
from crewlet.queue.topics import agent_inbox_group, agent_inbox_topic  # noqa: E402


class _MockLLM:
    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice: str | None = None,
    ) -> Completion:
        return Completion(content="done")

    async def stream(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
    ) -> AsyncIterator[CompletionChunk]:
        yield CompletionChunk(content="done")


async def _engine_with_queue():
    """A started engine with one real seat and a live turn engine.

    The turn engine matters: without an LLM provider the inbox handler
    parks every delivery on its *first* branch, which would make the
    posture tests below pass for the wrong reason.
    """
    queue = MemoryEventQueue()
    await queue.start()
    engine = Engine(
        organization=Organization(
            name="Acme",
            mission="ship",
            units=[
                OrgUnit(name="Core", type="team", lead="Dev", roles=[Role(name="Dev")])
            ],
        ),
        llm_providers={"default": _MockLLM()},
        event_queue=queue,
    )
    await engine.start()
    assert engine.turn_engine is not None
    return engine, queue


# ── admits_triggers ──────────────────────────────────────────────────


async def test_serve_and_wait_admit_work() -> None:
    """WAIT must keep serving — it is ordinary rollout propagation.

    This is the case that decides whether a successful rollout is
    invisible or a fleet-wide outage.
    """
    engine, queue = await _engine_with_queue()
    try:
        for posture in (Posture.SERVE, Posture.WAIT):
            engine._posture = posture
            assert engine.admits_triggers() is True
    finally:
        await engine.stop()
        await queue.stop()


async def test_isolated_admits_work() -> None:
    """ISOLATED means NO node applied the revision — the revision is the
    problem, not this node.  Refusing here would take the fleet down
    over one bad revision, which is what rollback exists to avoid."""
    engine, queue = await _engine_with_queue()
    try:
        engine._posture = Posture.ISOLATED
        assert engine.admits_triggers() is True
    finally:
        await engine.stop()
        await queue.stop()


async def test_shed_and_stuck_refuse_work() -> None:
    engine, queue = await _engine_with_queue()
    try:
        for posture in (Posture.SHED, Posture.STUCK):
            engine._posture = posture
            assert engine.admits_triggers() is False
    finally:
        await engine.stop()
        await queue.stop()


async def test_unset_posture_admits() -> None:
    """A node that has not decided anything yet is not shedding."""
    engine, queue = await _engine_with_queue()
    try:
        assert engine._posture is None
        assert engine.posture is Posture.SERVE
        assert engine.admits_triggers() is True
    finally:
        await engine.stop()
        await queue.stop()


# ── the pause is reason-scoped ───────────────────────────────────────


async def test_shedding_pauses_inboxes_under_the_config_reason() -> None:
    engine, queue = await _engine_with_queue()
    try:
        await engine._apply_posture(Posture.SHED)
        handles = [a.handle for a in engine.agent_pool.agents]
        assert handles
        for handle in handles:
            assert queue.pause_holds(
                agent_inbox_topic(handle), agent_inbox_group(handle)
            ) == {"config"}
        assert queue.pause_holds("crewlet.notifications.inbound", "notifications") == {
            "config"
        }
    finally:
        await engine.stop()
        await queue.stop()


async def test_converging_does_not_release_a_sandbox_hold() -> None:
    """The reason scoping, stated as the failure it prevents.

    A seat parked on a detached sandbox run has its inbox paused by the
    coordinator.  If the config shed and the sandbox gate shared one
    hold, a node converging back to SERVE would resume that seat's inbox
    mid-run — delivering messages to an agent whose turn is suspended.
    """
    engine, queue = await _engine_with_queue()
    try:
        handle = engine.agent_pool.agents[0].handle
        topic, group = agent_inbox_topic(handle), agent_inbox_group(handle)
        await queue.pause_topic(topic, group, reason="sandbox")

        await engine._apply_posture(Posture.SHED)
        assert queue.pause_holds(topic, group) == {"sandbox", "config"}

        await engine._apply_posture(Posture.SERVE)
        # Config released its hold; the sandbox's survives.
        assert queue.pause_holds(topic, group) == {"sandbox"}
    finally:
        await engine.stop()
        await queue.stop()


async def test_a_completing_sandbox_does_not_un_gate_a_diverged_node() -> None:
    """The mirror image: the sandbox releasing its own hold must not
    hand work to a node that cannot apply the current config."""
    engine, queue = await _engine_with_queue()
    try:
        handle = engine.agent_pool.agents[0].handle
        topic, group = agent_inbox_topic(handle), agent_inbox_group(handle)
        await queue.pause_topic(topic, group, reason="sandbox")
        await engine._apply_posture(Posture.SHED)

        await queue.resume_topic(topic, group, reason="sandbox")
        assert queue.pause_holds(topic, group) == {"config"}
    finally:
        await engine.stop()
        await queue.stop()


async def test_posture_changes_within_a_serving_band_do_not_churn() -> None:
    """SERVE→WAIT must not touch the queue at all.

    Pausing and resuming every inbox on each poll tick would restart
    consumers constantly during any rollout.
    """
    engine, queue = await _engine_with_queue()
    try:
        await engine._apply_posture(Posture.SERVE)
        await engine._apply_posture(Posture.WAIT)
        await engine._apply_posture(Posture.ISOLATED)
        handle = engine.agent_pool.agents[0].handle
        assert (
            queue.pause_holds(agent_inbox_topic(handle), agent_inbox_group(handle))
            == set()
        )
    finally:
        await engine.stop()
        await queue.stop()


# ── refusal is republish-then-ack ────────────────────────────────────


async def test_a_shed_inbox_delivery_is_requeued_not_dropped() -> None:
    """A delivery that arrives on a shedding node goes back on the topic.

    Not NAK'd (three redeliveries at 1s would dead-letter it), and above
    all not consumed-and-dropped — the copy has to survive for a node
    that can read it.  This branch catches deliveries already in flight
    when the pause landed, which is why both have to work.
    """
    from crewlet.events.types import ExternalNotification

    engine, queue = await _engine_with_queue()
    try:
        agent = engine.agent_pool.agents[0]
        topic = agent_inbox_topic(agent.handle)
        group = agent_inbox_group(agent.handle)
        handler = engine._make_agent_handler(agent)
        await engine._apply_posture(Posture.SHED)

        event = ExternalNotification(
            source="slack",
            recipient_handle=agent.handle,
            sender="U123",
            content="hello",
        )
        await handler([event])

        # Retained by the paused subscription rather than handled or lost.
        assert [e.id for e in queue.backlog(topic, group)] == [event.id]
    finally:
        await engine.stop()
        await queue.stop()


async def test_a_serving_node_handles_the_same_delivery() -> None:
    """The control: without the gate the very same partition is handled,
    so the test above is measuring the gate and not a broken fixture."""
    from crewlet.events.types import ExternalNotification

    engine, queue = await _engine_with_queue()
    try:
        agent = engine.agent_pool.agents[0]
        topic = agent_inbox_topic(agent.handle)
        group = agent_inbox_group(agent.handle)
        handled: list[object] = []
        engine._handle_notification = (  # type: ignore[method-assign]
            lambda _agent, event: handled.append(event) or _noop()
        )
        handler = engine._make_agent_handler(agent)

        event = ExternalNotification(
            source="slack",
            recipient_handle=agent.handle,
            sender="U123",
            content="hello",
        )
        await handler([event])

        assert handled == [event]
        assert queue.backlog(topic, group) == []
    finally:
        await engine.stop()
        await queue.stop()


async def _noop() -> None:
    return None


# ── the scheduler is a producer and gets gated too ───────────────────


async def test_scheduler_tick_is_skipped_while_shedding() -> None:
    """A schedule's fire identity is ORG-derived.

    A node that cannot apply the current epoch would fire the previous
    company's schedules — crons that were edited, seats that were
    deleted, schedules that were removed outright.  Unlike a delivery
    there is no queued copy to fall back on, so the tick skips whole.
    """
    from crewlet.schedule.scheduler import Scheduler

    admits = {"ok": False}
    fired: list[str] = []

    class _Store:
        async def try_claim(self, *args, **kwargs):
            fired.append("claimed")
            return True

    class _Queue:
        async def publish(self, topic, event):
            fired.append(topic)

    scheduler = Scheduler(
        event_queue=_Queue(),
        org_provider=lambda: (_ for _ in ()).throw(
            AssertionError("org must not be read while shedding")
        ),
        store=_Store(),
        admits=lambda: admits["ok"],
    )
    assert await scheduler.tick_once() == 0
    assert fired == []
    # And the window stays open, so the catchup pass evaluates it once
    # this node converges.
    assert scheduler._last_tick_utc is None


# ── the inbound notification path ────────────────────────────────────


async def test_inbound_notification_is_requeued_while_shedding() -> None:
    """Inbound routing is where a stale node does its most damage:
    HMAC verification is consume-side, and a failure is a skip + ack."""
    from crewlet.events.types import Event
    from crewlet.notifications.service import INBOUND_TOPIC, NotificationService

    republished: list[tuple[str, Event]] = []

    class _Queue:
        async def publish(self, topic, event):
            republished.append((topic, event))

        async def subscribe(self, *args, **kwargs): ...

    service = NotificationService(
        event_queue=_Queue(),  # type: ignore[arg-type]
        transports={},
        handle_registry=None,  # type: ignore[arg-type]
    )
    service._running = True
    service.set_admission_gate(lambda: False)

    event = Event(source="slack", type="raw_webhook", payload={"body": {}})
    await service._handle_inbound(event)

    assert republished == [(INBOUND_TOPIC, event)]


async def test_inbound_notification_without_a_gate_is_admitted() -> None:
    """A service built without an engine (tests, programmatic embeds)
    has no posture to consult and must not refuse everything."""
    from crewlet.events.types import Event
    from crewlet.notifications.service import NotificationService

    seen: list[Event] = []

    class _Queue:
        async def publish(self, topic, event): ...

    service = NotificationService(
        event_queue=_Queue(),  # type: ignore[arg-type]
        transports={},
        handle_registry=None,  # type: ignore[arg-type]
    )
    service._running = True

    async def _inner(event: Event) -> None:
        seen.append(event)

    service._handle_inbound_inner = _inner  # type: ignore[method-assign]
    event = Event(source="slack", type="raw_webhook", payload={"body": {}})
    await service._handle_inbound(event)
    assert seen == [event]


# ── readiness ────────────────────────────────────────────────────────


def test_readiness_fails_on_shed_and_stuck_only() -> None:
    """``wait`` and ``isolated`` stay in rotation on purpose — see the
    module docstring and ``decide_posture``."""
    from crewlet.api.streaming import build_readiness_envelope
    from tests.test_api.helpers import FakeNodeRuntime

    class _App:
        class state:  # noqa: N801
            configured = True
            node_id = "node-7"
            runtime = None

    for posture, expected in (
        ("serve", 200),
        ("wait", 200),
        ("isolated", 200),
        ("shed", 503),
        ("stuck", 503),
    ):
        _App.state.runtime = FakeNodeRuntime(posture=posture)  # type: ignore[assignment]
        body, status = build_readiness_envelope(_App)
        assert status == expected, posture
        assert body["posture"] == posture


def test_health_reports_posture_and_epoch() -> None:
    """``/ready`` is a bare 503 either way; ``posture`` on ``/health`` is
    the only place an operator can see WHY a node left rotation — and
    "draining" and "cannot apply epoch 41" call for opposite responses."""
    from crewlet.api.streaming import build_health_envelope
    from tests.test_api.helpers import FakeNodeRuntime

    class _App:
        class state:  # noqa: N801
            configured = True
            node_id = "node-7"
            runtime = FakeNodeRuntime(posture="shed", applied_epoch=40)

    body = build_health_envelope(_App)
    assert body["posture"] == "shed"
    assert body["applied_epoch"] == 40
    assert body["status"] == "shed"


def test_health_status_prefers_draining_over_posture() -> None:
    """A drain is the operator's own action and outranks a posture the
    node inferred about itself."""
    from crewlet.api.streaming import build_health_envelope
    from tests.test_api.helpers import FakeNodeRuntime

    class _App:
        class state:  # noqa: N801
            configured = True
            node_id = "node-7"
            runtime = FakeNodeRuntime(posture="shed", shutting_down=True)

    assert build_health_envelope(_App)["status"] == "shutting_down"


def test_unknown_posture_reads_as_serve() -> None:
    """An engine that cannot answer is not evidence of lag — the same
    rule ``decide_posture`` follows for silence."""
    from crewlet.api.runtime import EngineNodeRuntime

    class _Broken:
        @property
        def posture(self):
            raise RuntimeError("engine mid-spawn")

    assert EngineNodeRuntime(_Broken()).posture == "serve"
    assert EngineNodeRuntime(_Broken()).applied_epoch == 0


# ── a live apply does not move a turn already running ────────────────


async def test_a_live_apply_drains_the_seat_before_swapping_it() -> None:
    """The whole point of 4.E/4.F, end to end.

    A config apply that changes a role rebuilds that role's definition
    and can respawn the MCP children its tools dispatch to.  A turn
    already running keeps reading its own pinned view, and the apply
    waits for it rather than landing mid-round.
    """
    from crewlet.agent.definition import AgentDefinition

    engine, queue = await _engine_with_queue()
    try:
        turn_engine = engine.turn_engine
        assert turn_engine is not None
        agent = engine.agent_pool.agents[0]
        original = agent.live_definition

        # A turn is mid-flight on this seat.
        turn_engine._seat_enter(agent.role_name)
        released = asyncio.Event()

        async def _apply() -> None:
            new_org = Organization(
                name="Acme",
                mission="ship faster",
                units=[
                    OrgUnit(
                        name="Core",
                        type="team",
                        lead="Dev",
                        roles=[Role(name="Dev", goal="new goal")],
                    )
                ],
            )
            await engine._apply_org_diff(engine.org, new_org)
            released.set()

        applying = asyncio.create_task(_apply())
        # The apply is parked on the drain, so the seat's definition is
        # untouched while the turn runs.
        for _ in range(50):
            await asyncio.sleep(0)
        assert not released.is_set()
        assert agent.live_definition is original

        turn_engine._seat_exit(agent.role_name)
        await asyncio.wait_for(applying, timeout=5.0)
        assert isinstance(agent.live_definition, AgentDefinition)
        assert agent.live_definition is not original
    finally:
        await engine.stop()
        await queue.stop()


async def test_a_wedged_seat_does_not_block_the_apply_forever() -> None:
    """The drain is a cap, not a contract — an apply that never finishes
    is strictly worse than one turn seeing a mid-flight rewire."""
    engine, queue = await _engine_with_queue()
    try:
        turn_engine = engine.turn_engine
        assert turn_engine is not None
        agent = engine.agent_pool.agents[0]
        turn_engine._seat_enter(agent.role_name)  # never exits

        original = agent.live_definition
        new_org = Organization(
            name="Acme",
            mission="ship faster",
            units=[
                OrgUnit(
                    name="Core",
                    type="team",
                    lead="Dev",
                    roles=[Role(name="Dev", goal="new goal")],
                )
            ],
        )
        # Shorten the cap so the test does not sit out the real one.
        original_drain = turn_engine.drain_seat

        async def _short_drain(role_name: str, **_kwargs) -> bool:
            return await original_drain(role_name, timeout=0.05)

        turn_engine.drain_seat = _short_drain  # type: ignore[method-assign]
        await asyncio.wait_for(engine._apply_org_diff(engine.org, new_org), timeout=5.0)
        assert agent.live_definition is not original
    finally:
        await engine.stop()
        await queue.stop()
