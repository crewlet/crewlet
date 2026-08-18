"""Tests for the SandboxCoordinator: the engine-side
detached lifecycle — busy-on-start, completion → continuation turn (under
the kick-off turn_id), at-most-once claim, clarification re-park, and
restart recovery."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import pytest

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance, AgentState
from crewlet.events.types import SandboxRunCompleted, SandboxRunStarted
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.queue.memory import MemoryEventQueue
from crewlet.queue.topics import (
    agent_control_topic,
    agent_inbox_group,
    agent_inbox_topic,
)
from crewlet.sandbox import FakeCodingAgentRunner, FakeSandboxProvider, SandboxManager
from crewlet.sandbox.coordinator import (
    SandboxCoordinator,
    SandboxResumeUnavailable,
)
from crewlet.sandbox.pending_store import (
    MemoryPendingSandboxRunStore,
    PendingSandboxRun,
)
from crewlet.sandbox.protocol import CodingAgentResult


@dataclass
class _TurnEngineStub:
    calls: list[dict[str, Any]] = field(default_factory=list)

    async def run_turn(self, agent, **kwargs) -> str:
        self.calls.append({"agent": agent, **kwargs})
        return "ok"


@dataclass
class _BudgetManager:
    consumed: list[tuple[str, int]] = field(default_factory=list)

    async def consume(self, agent_id: str, tokens: int) -> bool:
        self.consumed.append((agent_id, tokens))
        return True


def _mk_agent(handle: str = "eng") -> AgentInstance:
    role = Role(name="Engineer", handle=handle)
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    agent = AgentInstance(definition=defn, handle=handle, email="e@acme.com")
    agent.activate()
    return agent


def _mk_manager(result: CodingAgentResult | None = None):
    runner = FakeCodingAgentRunner(name="claude-code", result=result)
    provider = FakeSandboxProvider()
    return SandboxManager(provider=provider, runners={"claude-code": runner}), provider


def _coordinator(
    *,
    queue,
    store,
    manager,
    agent,
    turn_engine,
    budget_manager=None,
) -> SandboxCoordinator:
    return SandboxCoordinator(
        event_queue=queue,
        pending_store=store,
        manager=manager,
        get_agent=lambda h: agent if h == agent.handle else None,
        get_turn_engine=lambda: turn_engine,
        get_org=lambda: agent.definition.org,
        budget_manager=budget_manager,
    )


def _run(**over: Any) -> PendingSandboxRun:
    base: dict[str, Any] = {
        "turn_id": "t-1",
        "agent_handle": "eng",
        "agent_id": "aid",
        "sandbox_id": "sbx-1",
        "coding_agent": "claude-code",
        "command_id": "cmd-1",
        "success_criteria": ["PR opened"],
        "task_description": "Build it",
        "conversation_key": "slack:C1:9",
        # A resolved pause TTL, as launch_sandbox_run records from the spec.
        # The dataclass default (0.0) means "never pause" — the opt-out — so
        # a run that should exercise the PAUSED path has to say so.
        "pause_ttl_seconds": 1800.0,
        # Every run is a run_sandbox suspend: a completion resumes the
        # Execute loop from this state.
        "execute_state": {
            "messages": [{"role": "system", "content": "EXECUTE phase"}],
            "pending_tool_call_id": "c1",
            "pending_tool_name": "run_sandbox",
            "active_tool_names": [],
            "loaded_skill_keys": [],
            "iteration": 1,
        },
    }
    base.update(over)
    return PendingSandboxRun(**base)


# --- busy on start --------------------------------------------------------


async def test_on_run_started_pauses_inbox_and_leaves_working_turn_alone() -> None:
    # The busy transition is OWNED by the suspending turn's finally; a
    # WORKING agent here is either that turn (about to transition itself) or
    # an UNRELATED later turn — flipping it from the coordinator would
    # hijack that turn's state and produce overlapping turns.
    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    agent.start_working("t-1")  # WORKING (mid kick-off turn)
    manager, _ = _mk_manager()
    coord = _coordinator(
        queue=queue,
        store=MemoryPendingSandboxRunStore(),
        manager=manager,
        agent=agent,
        turn_engine=_TurnEngineStub(),
    )
    await coord.on_run_started(
        SandboxRunStarted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1")
    )
    assert agent.state == AgentState.WORKING  # untouched — the turn owns it
    assert queue.pause_holds(agent_inbox_topic("eng"), agent_inbox_group("eng"))
    await queue.stop()


async def test_on_run_started_recovers_idle_agent_to_busy() -> None:
    # Post-restart recovery: a fresh instance is IDLE (the kick-off turn's
    # own transition never ran in this process) — the coordinator re-enters
    # the busy state for it.
    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()  # IDLE
    manager, _ = _mk_manager()
    coord = _coordinator(
        queue=queue,
        store=MemoryPendingSandboxRunStore(),
        manager=manager,
        agent=agent,
        turn_engine=_TurnEngineStub(),
    )
    await coord.on_run_started(
        SandboxRunStarted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1")
    )
    assert agent.state == AgentState.AWAITING_SANDBOX
    assert queue.pause_holds(agent_inbox_topic("eng"), agent_inbox_group("eng"))
    await queue.stop()


# --- completion → continuation turn --------------------------------------


async def test_on_run_completed_resumes_execute_when_state_present() -> None:
    # A run with persisted execute_state RESUMES the
    # Execute loop (under the kick-off turn_id) instead of dispatching a fresh
    # completion turn.
    from crewlet.agent.execute import ExecuteResumeState

    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    store = MemoryPendingSandboxRunStore()
    await store.create(
        _run(
            execute_state={
                "messages": [{"role": "system", "content": "EXECUTE phase"}],
                "pending_tool_call_id": "c1",
                "pending_tool_name": "run_sandbox",
                "active_tool_names": ["search"],
                "loaded_skill_keys": [],
                "iteration": 1,
            }
        )
    )
    manager, provider = _mk_manager(
        CodingAgentResult(text="opened a PR", success=True, delivered_refs=["pr-1"])
    )
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    te = _TurnEngineStub()
    coord = _coordinator(
        queue=queue, store=store, manager=manager, agent=agent, turn_engine=te
    )

    await coord.on_run_completed(
        SandboxRunCompleted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1")
    )

    assert len(te.calls) == 1
    call = te.calls[0]
    # Continues the SAME turn at the offset iteration …
    assert call["turn_id"] == "t-1"
    assert call["start_iteration"] == 1
    # … resuming the loop with the result spliced in.
    rs = call["resume_state"]
    assert isinstance(rs, ExecuteResumeState)
    assert rs.pending_tool_call_id == "c1"
    assert "opened a PR" in rs.result_content
    assert "do NOT redo it" in rs.result_content  # report-don't-redo framing
    await queue.stop()


async def test_completion_keeps_agent_busy_until_resume_dispatch() -> None:
    # The agent must stay AWAITING_SANDBOX (and its inbox paused) all the
    # way through collect — freeing it up front let a queued event steal
    # the WORKING slot before the resume turn dispatched.
    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    agent.await_sandbox(task_id="t-1")
    # The coordinator's OWN hold reason — pause holds are reason-scoped
    # so the sandbox gate, the config shed and the no-turn-engine park
    # cannot release each other's.
    await queue.pause_topic(
        agent_inbox_topic("eng"), agent_inbox_group("eng"), reason="sandbox"
    )
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    manager, provider = _mk_manager(CodingAgentResult(text="done", success=True))
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]

    observed: list[str] = []

    @dataclass
    class _ObservingTurnEngine:
        async def run_turn(self, agent_arg, **kwargs) -> str:
            # By dispatch time the agent has been freed (start_working can
            # take the slot) …
            observed.append(str(agent.state))
            return "ok"

    # Wrap collect to observe the agent's state mid-collection.
    orig_collect = manager.runner_for("claude-code").collect

    async def _observing_collect(sandbox, handle):
        observed.append(f"collect:{agent.state}")
        return await orig_collect(sandbox, handle)

    manager.runner_for("claude-code").collect = _observing_collect  # type: ignore[method-assign]

    coord = _coordinator(
        queue=queue,
        store=store,
        manager=manager,
        agent=agent,
        turn_engine=_ObservingTurnEngine(),
    )
    await coord.on_run_completed(
        SandboxRunCompleted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1")
    )

    # Busy while collecting; freed by dispatch time.
    assert observed[0] == f"collect:{AgentState.AWAITING_SANDBOX}"
    assert observed[1] == str(AgentState.IDLE)
    # Settled (no re-suspend): the box is done and the inbox resumed.
    assert not queue.pause_holds(agent_inbox_topic("eng"), agent_inbox_group("eng"))
    await queue.stop()


async def test_resume_dispatch_failure_reverts_claim_for_retry() -> None:
    # A failed resume dispatch must UN-CLAIM the row (back to its pre-flip
    # status) and re-raise — otherwise the NAK'd completion redelivers,
    # claim_for_resume returns None, and the suspended loop is lost forever
    # with the row stranded in 'resumed'.
    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    agent.await_sandbox(task_id="t-1")
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    manager, provider = _mk_manager(CodingAgentResult(text="done", success=True))
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]

    class _BoomTurnEngine:
        async def run_turn(self, agent_arg, **kwargs) -> str:
            raise RuntimeError("dispatch exploded")

    coord = _coordinator(
        queue=queue,
        store=store,
        manager=manager,
        agent=agent,
        turn_engine=_BoomTurnEngine(),
    )
    with pytest.raises(RuntimeError, match="dispatch exploded"):
        await coord.on_run_completed(
            SandboxRunCompleted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1")
        )

    # The claim was reverted to its pre-flip status, so a redelivery of the
    # completion event can claim + retry the resume.
    row = await store.get("t-1")
    assert row is not None and row.status == "running"
    retried = await store.claim_for_resume("t-1")
    assert retried is not None
    await queue.stop()


async def test_tool_path_tears_down_box_when_execute_finishes() -> None:
    # The box is paused on collect (kept for reuse),
    # then torn down once the resumed Execute finishes without calling
    # run_sandbox again (the stub turn engine doesn't re-suspend).
    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    store = MemoryPendingSandboxRunStore()
    await store.create(
        _run(
            execute_state={"messages": [], "pending_tool_call_id": "c1", "iteration": 1}
        )
    )
    manager, provider = _mk_manager(
        CodingAgentResult(text="done", success=True, delivered_refs=["pr-9"])
    )
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    te = _TurnEngineStub()
    coord = _coordinator(
        queue=queue, store=store, manager=manager, agent=agent, turn_engine=te
    )

    await coord.on_run_completed(
        SandboxRunCompleted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1")
    )

    # Resumed once …
    assert len(te.calls) == 1
    # … and since it didn't re-suspend, the box was torn down + row terminal.
    assert provider.sandboxes[0].closed is True
    row = await store.get("t-1")
    assert row is not None and row.status == "done"
    await queue.stop()


async def test_on_run_completed_is_at_most_once() -> None:
    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    manager, provider = _mk_manager()
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    te = _TurnEngineStub()
    coord = _coordinator(
        queue=queue, store=store, manager=manager, agent=agent, turn_engine=te
    )
    evt = SandboxRunCompleted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1")
    await coord.on_run_completed(evt)
    await coord.on_run_completed(evt)  # duplicate completion — second loses
    assert len(te.calls) == 1
    await queue.stop()


async def test_on_run_completed_clarification_parks_and_posts_question() -> None:
    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    manager, provider = _mk_manager(
        CodingAgentResult(
            text="need info",
            success=False,
            needs_input=True,
            question="Which framework?",
            ask_to="team",
        )
    )
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    te = _TurnEngineStub()
    coord = _coordinator(
        queue=queue, store=store, manager=manager, agent=agent, turn_engine=te
    )

    await coord.on_run_completed(
        SandboxRunCompleted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1")
    )

    # The run is parked for the answer …
    row = await store.get("t-1")
    assert row is not None
    assert row.status == "awaiting_clarification"
    assert row.question == "Which framework?"
    assert row.audience == "team"
    # … a SandboxClarificationRequested was emitted …
    assert any(e.type == "sandbox_clarification_requested" for e in queue.history)
    # … and a turn was dispatched to POST the question (engine identity),
    # not the completion turn.
    assert len(te.calls) == 1
    posted = te.calls[0]["task_description"]
    assert "Which framework?" in posted
    assert "Post this question" in posted or "Post only that question" in posted
    # The box is held PAUSED for the answer — that wait is what pause_ttl
    # bounds — and the row records when, so the reaper can expire it.
    assert provider.sandboxes[0].paused is True
    assert provider.sandboxes[0].closed is False
    assert row.sandbox_id == "sbx-1"
    assert row.paused_at is not None
    await queue.stop()


async def test_clarification_with_pause_ttl_zero_parks_straight_into_reseed() -> None:
    # ``pause_ttl_seconds: 0`` = never pause, always re-seed. The box is torn
    # down the moment the run blocks (zero snapshot cost) and the run parks
    # directly in `reseed` — still matched by conversation_key, so the answer
    # still resumes it, just from the pushed branch.
    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    store = MemoryPendingSandboxRunStore()
    await store.create(_run(pause_ttl_seconds=0.0))
    manager, provider = _mk_manager(
        CodingAgentResult(
            text="need info",
            success=False,
            needs_input=True,
            question="Which framework?",
            ask_to="team",
        )
    )
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    coord = _coordinator(
        queue=queue,
        store=store,
        manager=manager,
        agent=agent,
        turn_engine=_TurnEngineStub(),
    )

    await coord.on_run_completed(
        SandboxRunCompleted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1")
    )

    # Torn down as soon as it blocked — and killed by id, so it was never
    # resumed on the way out.
    assert provider.sandboxes[0].closed is True
    row = await store.get("t-1")
    assert row is not None
    assert row.status == "reseed"
    assert row.question == "Which framework?"
    assert row.sandbox_id == ""
    assert await store.find_awaiting_by_conversation("slack:C1:9") is not None
    await queue.stop()


async def test_answer_to_a_reaped_run_resumes_with_a_reseed_brief() -> None:
    # After the reaper takes the paused box, the answer still resumes the
    # SAME suspended Execute loop — but the spliced reply must not promise a
    # machine that no longer exists. It has to send the executor back to the
    # pushed branch on a fresh box.
    from crewlet.events.types import ExternalNotification
    from crewlet.notifications.coalesce import conversation_key

    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    store = MemoryPendingSandboxRunStore()
    answer = ExternalNotification(body="Use Postgres", subject="re")
    conv_key = conversation_key(answer)
    await store.create(_run(conversation_key=conv_key, branch="wip/db"))
    await store.mark_awaiting_clarification(
        "t-1", question="Which DB?", audience="requester", conversation_key=conv_key
    )
    # …then the reaper expired the pause.
    await store.release_box("t-1")
    await store.set_status("t-1", "reseed")
    manager, provider = _mk_manager()
    te = _TurnEngineStub()
    coord = _coordinator(
        queue=queue, store=store, manager=manager, agent=agent, turn_engine=te
    )

    assert await coord.try_resume_from_answer(agent, answer) is True

    assert len(te.calls) == 1
    resume = te.calls[0]["resume_state"]
    text = resume.result_content
    assert "Use Postgres" in text
    assert "wip/db" in text
    assert "reclaimed" in text and "FRESH machine" in text
    # It must NOT claim the old checkout is still there.
    assert "still up with that work-in-progress" not in text
    await queue.stop()


async def test_try_resume_from_answer_resumes_execute_loop() -> None:
    # A clarification answer RESUMES the SAME
    # suspended Execute loop the run_sandbox call left parked (the human's
    # answer spliced in as that call's reply), not a fresh re-investigation
    # turn. The executor continues with full context.
    from crewlet.agent.execute import ExecuteResumeState
    from crewlet.events.types import ExternalNotification
    from crewlet.notifications.coalesce import conversation_key

    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    store = MemoryPendingSandboxRunStore()
    # The inbound reply that answers the question; its conversation_key is
    # what the parked run must be keyed on.
    answer = ExternalNotification(body="Use Postgres", subject="re")
    conv_key = conversation_key(answer)

    await store.create(_run(conversation_key=conv_key, branch="wip/x"))
    await store.mark_awaiting_clarification(
        "t-1", question="Which DB?", audience="requester", conversation_key=conv_key
    )
    manager, provider = _mk_manager()
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    te = _TurnEngineStub()
    coord = _coordinator(
        queue=queue, store=store, manager=manager, agent=agent, turn_engine=te
    )

    handled = await coord.try_resume_from_answer(agent, answer)

    assert handled is True
    assert len(te.calls) == 1
    call = te.calls[0]
    # Continues the SAME turn (under the kick-off turn_id) by resuming Execute …
    assert call["turn_id"] == "t-1"
    assert call["start_iteration"] == 1
    rs = call["resume_state"]
    assert isinstance(rs, ExecuteResumeState)
    assert rs.pending_tool_call_id == "c1"
    # … with the answer (and the question it was for + the WIP branch) spliced
    # in as the run_sandbox call's reply, telling it to call run_sandbox again.
    assert "Use Postgres" in rs.result_content
    assert "Which DB?" in rs.result_content
    assert "wip/x" in rs.result_content
    assert "run_sandbox again" in rs.result_content
    # The stub didn't re-suspend, so the box was torn down + the row terminal.
    assert provider.sandboxes[0].closed is True
    row = await store.get("t-1")
    assert row is not None and row.status == "done"
    # A non-matching conversation is left alone.
    unrelated = ExternalNotification(body="x")
    assert await coord.try_resume_from_answer(agent, unrelated) is False
    await queue.stop()


async def test_try_resume_from_answer_reuses_box_when_run_sandbox_recalled() -> None:
    # When the resumed executor calls run_sandbox AGAIN (the stub flips the row
    # back to "running" to simulate a re-suspend), the paused box is left up for
    # the next completion instead of being torn down.
    from crewlet.events.types import ExternalNotification
    from crewlet.notifications.coalesce import conversation_key

    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    store = MemoryPendingSandboxRunStore()
    answer = ExternalNotification(body="Use Postgres", subject="re")
    conv_key = conversation_key(answer)
    await store.create(_run(conversation_key=conv_key))
    await store.mark_awaiting_clarification(
        "t-1", question="Which DB?", audience="requester", conversation_key=conv_key
    )
    manager, provider = _mk_manager()
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]

    # A turn engine that re-suspends by flipping the row back to "running"
    # (what launch_sandbox_run does for a reused box).
    @dataclass
    class _ResuspendingEngine:
        store: Any
        calls: list[dict[str, Any]] = field(default_factory=list)

        async def run_turn(self, agent, **kwargs) -> str:
            self.calls.append({"agent": agent, **kwargs})
            await self.store.set_status("t-1", "running")
            return "ok"

    te = _ResuspendingEngine(store=store)
    coord = _coordinator(
        queue=queue, store=store, manager=manager, agent=agent, turn_engine=te
    )

    await coord.try_resume_from_answer(agent, answer)

    assert len(te.calls) == 1
    # The box stays up (reused) and the row stays "running" for the next round.
    assert provider.sandboxes[0].closed is False
    row = await store.get("t-1")
    assert row is not None and row.status == "running"
    await queue.stop()


# --- restart recovery -----------------------------------------------------


async def test_recover_repauses_running_agents() -> None:
    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()  # fresh IDLE agent after restart
    store = MemoryPendingSandboxRunStore()
    await store.create(_run(turn_id="t-run", status="running"))
    await store.create(_run(turn_id="t-clar"))
    await store.mark_awaiting_clarification(
        "t-clar", question="Q", audience="team", conversation_key="k"
    )
    manager, _ = _mk_manager()
    coord = _coordinator(
        queue=queue,
        store=store,
        manager=manager,
        agent=agent,
        turn_engine=_TurnEngineStub(),
    )

    await coord.recover_seat("eng", owner="node-a:1", epoch=1)

    # The running job re-pauses the agent's inbox and re-enters busy.
    assert queue.pause_holds(agent_inbox_topic("eng"), agent_inbox_group("eng"))
    assert agent.state == AgentState.AWAITING_SANDBOX
    await queue.stop()


async def test_recover_reaps_a_tail_abandoned_by_a_dead_engine() -> None:
    # A `resumed` row at boot means the previous engine died between claiming
    # a completion and settling it. The at-most-once claim already flipped, so
    # the redelivered completion is refused and nothing will ever look at the
    # row again — its paused box would sit there billing forever. Boot is the
    # one moment the row is unambiguously abandoned, so reap it here.
    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    store = MemoryPendingSandboxRunStore()
    await store.create(_run(turn_id="t-dead"))
    await store.set_status("t-dead", "resumed")
    manager, provider = _mk_manager()
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    coord = _coordinator(
        queue=queue,
        store=store,
        manager=manager,
        agent=agent,
        turn_engine=_TurnEngineStub(),
    )

    await coord.recover_seat("eng", owner="node-a:1", epoch=1)

    assert provider.sandboxes[0].closed is True
    row = await store.get("t-dead")
    assert row is not None and row.status == "failed" and row.sandbox_id == ""
    # An abandoned tail must not re-park the agent — nothing is coming.
    assert agent.state == AgentState.IDLE
    await queue.stop()


async def test_attach_seat_wires_the_seats_control_topic() -> None:
    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    manager, provider = _mk_manager()
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    te = _TurnEngineStub()
    coord = _coordinator(
        queue=queue, store=store, manager=manager, agent=agent, turn_engine=te
    )
    await coord.attach_seat("eng")

    # Publishing a completion onto the SEAT's control topic drives the
    # flow. Routing falls out of who subscribed: only the node that
    # attached this seat has the suspended Execute conversation.
    await queue.publish(
        agent_control_topic("eng"),
        SandboxRunCompleted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1"),
    )
    assert len(te.calls) == 1
    # And a started event on its topic re-enters the busy state for an IDLE
    # agent (post-restart recovery — a WORKING one is owned by its turn).
    assert agent.state == AgentState.IDLE
    await queue.publish(
        agent_control_topic("eng"),
        SandboxRunStarted(agent_handle="eng", turn_id="t-2", sandbox_id="sbx-2"),
    )
    assert agent.state == AgentState.AWAITING_SANDBOX
    await queue.stop()


@pytest.mark.parametrize("missing", ["agent", "engine"])
async def test_a_completion_this_node_cannot_resume_is_refused(missing: str) -> None:
    """It must RAISE, not settle.

    The suspended Execute conversation — the agent's whole in-progress
    turn — lives in the pending row and can only be resumed where the
    seat's instance is. Returning quietly let the caller fall through to
    settling the run ``done`` and tearing the box down, discarding it.
    Raising reverts the claim so the completion redelivers to a node
    that can actually resume it.
    """
    queue = MemoryEventQueue()
    await queue.start()
    agent = _mk_agent()
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    manager, provider = _mk_manager()
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    te = None if missing == "engine" else _TurnEngineStub()
    coord = SandboxCoordinator(
        event_queue=queue,
        pending_store=store,
        manager=manager,
        get_agent=lambda h: None if missing == "agent" else agent,
        get_turn_engine=lambda: te,
        get_org=lambda: agent.definition.org,
    )
    with pytest.raises(SandboxResumeUnavailable):
        await coord.on_run_completed(
            SandboxRunCompleted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1")
        )
    if isinstance(te, _TurnEngineStub):
        assert te.calls == []
    # The row is back where the claim found it, so the redelivered
    # completion can win the flip again on the owning node.
    run = await store.get("t-1")
    assert run is not None and run.status == "running"
    await queue.stop()


# ---------------------------------------------------------------------------
# owner-routed control
# ---------------------------------------------------------------------------
#
# A detached sandbox run outlives the process that started it, and the
# seat it belongs to can move while the coding agent is still working.
# The suspended Execute conversation lives in the pending row and can
# only be resumed where the seat's instance is — so a completion has to
# reach that node, and only that node.


async def test_a_completion_reaches_the_seats_owner_and_nobody_else() -> None:
    """Routing falls out of who subscribed.

    Under one fleet-wide group the broker hands a completion to whichever
    node wins it, which is a non-owner (N-1)/N of the time — and that
    node has no suspended conversation to resume.
    """
    queue = MemoryEventQueue()
    await queue.start()
    owner_saw: list[str] = []
    peer_saw: list[str] = []

    async def _owner(event) -> None:
        owner_saw.append(event.turn_id)

    async def _peer(event) -> None:
        peer_saw.append(event.turn_id)

    # The owner attaches this seat; a peer attaches a DIFFERENT one.
    await queue.subscribe(agent_control_topic("eng"), "agent-eng-control", _owner)
    await queue.subscribe(agent_control_topic("ops"), "agent-ops-control", _peer)

    await queue.publish(
        agent_control_topic("eng"),
        SandboxRunCompleted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1"),
    )

    assert owner_saw == ["t-1"]
    assert peer_saw == []
    await queue.stop()


async def test_a_completion_for_an_unowned_seat_is_held() -> None:
    """A run outlives its node, so "nobody is attached right now" is a
    normal state — and losing the completion would strand a real, billed
    box with a suspended turn behind it."""
    queue = MemoryEventQueue()
    await queue.start()
    await queue.ensure_subscription(agent_control_topic("eng"), "agent-eng-control")

    await queue.publish(
        agent_control_topic("eng"),
        SandboxRunCompleted(agent_handle="eng", turn_id="t-1", sandbox_id="sbx-1"),
    )
    assert len(queue.backlog(agent_control_topic("eng"), "agent-eng-control")) == 1

    seen: list[str] = []

    async def _new_owner(event) -> None:
        seen.append(event.turn_id)

    await queue.subscribe(agent_control_topic("eng"), "agent-eng-control", _new_owner)
    assert seen == ["t-1"]
    await queue.stop()


async def test_detaching_a_seat_leaves_its_control_subscription() -> None:
    queue = MemoryEventQueue()
    await queue.start()
    store = MemoryPendingSandboxRunStore()
    manager, _provider = _mk_manager()
    coord = _coordinator(
        queue=queue,
        store=store,
        manager=manager,
        agent=_mk_agent(),
        turn_engine=_TurnEngineStub(),
    )

    await coord.attach_seat("eng")
    assert (agent_control_topic("eng"), "agent-eng-control") in queue.attachments()

    await coord.detach_seat("eng")
    assert (agent_control_topic("eng"), "agent-eng-control") not in (
        queue.attachments()
    )
    # Non-destructive: a completion published now is still held.
    await queue.publish(
        agent_control_topic("eng"),
        SandboxRunCompleted(agent_handle="eng", turn_id="t-9", sandbox_id="sbx-9"),
    )
    assert len(queue.backlog(agent_control_topic("eng"), "agent-eng-control")) == 1
    await queue.stop()


async def test_recover_seat_only_touches_its_own_seats_runs() -> None:
    """Per-seat, not fleet-wide.

    A boot-time scan of every active row had each node re-pausing,
    re-parking and reaping runs belonging to seats its peers own — and a
    seat claimed LATER (a takeover, not a boot) was never recovered at
    all.
    """
    queue = MemoryEventQueue()
    await queue.start()
    store = MemoryPendingSandboxRunStore()
    await store.create(_run(turn_id="mine", agent_handle="eng"))
    await store.create(_run(turn_id="theirs", agent_handle="ops"))
    manager, _provider = _mk_manager()
    agent = _mk_agent()
    coord = _coordinator(
        queue=queue,
        store=store,
        manager=manager,
        agent=agent,
        turn_engine=_TurnEngineStub(),
    )

    await coord.recover_seat("eng", owner="node-a:1", epoch=7)

    assert queue.pause_holds(agent_inbox_topic("eng"), agent_inbox_group("eng")) == {
        "sandbox"
    }
    assert queue.pause_holds(agent_inbox_topic("ops"), agent_inbox_group("ops")) == (
        set()
    )
    mine = await store.get("mine")
    theirs = await store.get("theirs")
    assert mine is not None and mine.owner == "node-a:1" and mine.owner_epoch == 7
    assert theirs is not None and theirs.owner == ""
    await queue.stop()


async def test_a_stale_owner_cannot_settle_a_run_its_successor_holds() -> None:
    """The fence. The ownership check is an optimization — it reads a
    snapshot that can be a lease TTL stale — so correctness has to come
    from the write itself."""
    store = MemoryPendingSandboxRunStore()
    await store.create(_run(turn_id="t-1"))

    assert await store.claim_ownership("t-1", owner="node-a:1", epoch=1) is True
    # The seat moves: the successor claims at a higher epoch.
    assert await store.claim_ownership("t-1", owner="node-b:1", epoch=2) is True

    # The old owner, not yet aware, tries to settle it.
    assert await store.set_status("t-1", "done", epoch=1) is False
    run = await store.get("t-1")
    assert run is not None and run.status == "running"

    # The current owner can.
    assert await store.set_status("t-1", "done", epoch=2) is True


async def test_ownership_never_moves_backwards() -> None:
    store = MemoryPendingSandboxRunStore()
    await store.create(_run(turn_id="t-1"))
    await store.claim_ownership("t-1", owner="node-b:1", epoch=5)

    assert await store.claim_ownership("t-1", owner="node-a:1", epoch=2) is False
    run = await store.get("t-1")
    assert run is not None and run.owner == "node-b:1"
