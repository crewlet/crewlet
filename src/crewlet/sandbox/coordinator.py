"""Engine-side sandbox-as-a-tool lifecycle.

The :class:`SandboxCoordinator` is the engine's hands on the detached
``run_sandbox`` flow, kept out of ``engine.py`` so the moving parts stay
cohesive and unit-testable:

- **suspend → busy.** The suspending turn ITSELF flips its agent
  WORKING → AWAITING_SANDBOX in its ``finally`` (the state never passes
  through IDLE, so no queued event can slip a turn in between); the
  :class:`SandboxRunStarted` event pauses the agent's inbox and re-enters
  the busy state after a restart (the only case the coordinator transitions
  the agent itself).
- **completion → resume.** A :class:`SandboxRunCompleted` event (from the
  completion poll, :class:`~crewlet.sandbox.waiter.SandboxWaiter`) is
  claimed at-most-once, the result is collected + the box paused for reuse,
  tokens are post-accounted, and the suspended Execute loop is RESUMED with
  the result spliced in — the SAME turn continues with full context. The
  agent stays busy and its inbox stays paused until immediately before the
  resume dispatch; a failed dispatch reverts the claim so the retry can
  re-claim. When the resumed Execute is done with the box (no further
  run_sandbox call) the box is torn down and the inbox resumes.
- **restart recovery.** On boot, still-active runs re-pause their agents'
  inboxes so a restart doesn't orphan a running job, and a run left mid-tail
  by the previous engine (``resumed``, unreachable through the at-most-once
  claim) has its sandbox reaped instead of leaking.

The completion/started signals ride engine-wide topics (NOT the agent
inbox), so the paused inbox never blocks the wake-up.
"""

from __future__ import annotations

from collections.abc import Callable
from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.execute_sandbox import (
    collect_sandbox_run,
    publish_sandbox_completion_phase,
    teardown_sandbox_run,
)
from crewlet.agent.instance import AgentInstance, AgentState
from crewlet.agent.llm_loop import redact_secrets
from crewlet.events.types import (
    Event,
    SandboxClarificationRequested,
    SandboxRunCompleted,
    SandboxRunStarted,
)
from crewlet.notifications.coalesce import conversation_key
from crewlet.queue.protocol import EventQueue
from crewlet.sandbox.manager import SandboxManager
from crewlet.sandbox.pending_store import PendingSandboxRun, PendingSandboxRunStore
from crewlet.sandbox.protocol import CodingAgentResult

logger = get_logger("sandbox.coordinator")

STARTED_TOPIC = "crewlet.events.sandbox_run_started"
COMPLETIONS_TOPIC = "crewlet.events.sandbox_run_completed"
_GROUP = "sandbox-coordinator"


def inbox_topic(handle: str) -> str:
    """The per-agent inbox topic paused while a sandbox job runs."""
    return f"crewlet.agent.{handle}.inbox"


class SandboxCoordinator:
    """Drives the engine-side detached sandbox lifecycle."""

    def __init__(
        self,
        *,
        event_queue: EventQueue,
        pending_store: PendingSandboxRunStore,
        manager: SandboxManager,
        get_agent: Callable[[str], AgentInstance | None],
        get_turn_engine: Callable[[], Any],
        get_org: Callable[[], Any],
        budget_manager: Any = None,
        token_usage_repo: Any = None,
    ) -> None:
        self._event_queue = event_queue
        self._pending_store = pending_store
        self._manager = manager
        self._get_agent = get_agent
        self._get_turn_engine = get_turn_engine
        self._get_org = get_org
        self._budget_manager = budget_manager
        self._token_usage_repo = token_usage_repo

    def set_manager(self, manager: SandboxManager) -> None:
        """Swap the sandbox manager (live-reload of ``providers.sandbox``)."""
        self._manager = manager

    async def subscribe(self) -> None:
        """Subscribe to the engine-wide started / completed control topics."""
        await self._event_queue.subscribe(STARTED_TOPIC, _GROUP, self._on_started_event)
        await self._event_queue.subscribe(
            COMPLETIONS_TOPIC, _GROUP, self._on_completed_event
        )
        logger.info("sandbox_coordinator_subscribed")

    # -- event adapters ----------------------------------------------------

    async def _on_started_event(self, event: Event) -> None:
        if isinstance(event, SandboxRunStarted):
            await self.on_run_started(event)

    async def _on_completed_event(self, event: Event) -> None:
        if isinstance(event, SandboxRunCompleted):
            await self.on_run_completed(event)

    # -- kick-off → busy ---------------------------------------------------

    async def on_run_started(self, event: SandboxRunStarted) -> None:
        """Pause the agent's inbox; pick up post-restart busy recovery.

        The busy transition itself is OWNED by the suspending turn: its
        ``finally`` moves the agent WORKING → AWAITING_SANDBOX synchronously
        at suspend, so the state never passes through IDLE and no queued
        event can slip a turn in between.  This handler therefore only (a)
        pauses the inbox topic and (b) re-enters the busy state for an
        agent found IDLE — which can only mean a fresh instance after an
        engine restart (the kick-off turn's transition never ran in this
        process).  An agent found WORKING is left ALONE: that is either the
        kick-off turn still in flight (its finally will transition) or an
        unrelated later turn — flipping it from here would hijack that
        turn's state and let ``resume_from_sandbox`` free the agent while
        the turn is still mid-phase, producing overlapping turns.
        """
        handle = event.agent_handle
        if not handle:
            return
        await self._event_queue.pause_topic(inbox_topic(handle))
        agent = self._get_agent(handle)
        if agent is not None and agent.state == AgentState.IDLE:
            agent.await_sandbox(task_id=event.task_id)
        logger.info(
            "sandbox_agent_busy",
            agent=handle,
            turn_id=event.turn_id,
            sandbox_id=event.sandbox_id,
        )

    # -- completion → resume the Execute loop -----------------------------

    async def on_run_completed(self, event: SandboxRunCompleted) -> None:
        """Claim the run, collect, account, then resume the Execute loop.

        Ordering matters: the agent stays AWAITING_SANDBOX and its inbox
        stays paused all the way through collect / account / publish, and is
        freed only at the last moment before the resume turn dispatches
        (inside :meth:`_resume_execute_and_settle`).  Freeing it up front
        would open a race where a queued inbox event grabs the
        agent first, the resume ``run_turn`` fails, the completion event
        NAKs, and the redelivery finds the claim already flipped: the
        suspended Execute loop would be permanently lost.
        """
        run = await self._pending_store.claim_for_resume(event.turn_id)
        if run is None:
            # Already claimed by a duplicate signal (successive poll ticks
            # both firing before this claim landed, or an at-least-once
            # queue redelivery), or terminal.
            logger.info("sandbox_completion_already_claimed", turn_id=event.turn_id)
            return

        agent = self._get_agent(run.agent_handle)

        # The box is PAUSED on collect (kept for reuse across run_sandbox
        # calls in this turn); it is torn down when the Execute phase is done.
        try:
            result = await collect_sandbox_run(
                run=run,
                manager=self._manager,
                pending_store=self._pending_store,
                teardown=False,
            )
        except Exception:
            logger.exception("sandbox_collect_failed", turn_id=run.turn_id)
            await self._pending_store.set_status(run.turn_id, "failed")
            await teardown_sandbox_run(
                run=run, manager=self._manager, pending_store=self._pending_store
            )
            # The job is over even though collection failed — free the
            # agent and let its inbox flow again.
            self._free_agent(agent)
            await self._event_queue.resume_topic(inbox_topic(run.agent_handle))
            return

        await self._account(run, result)

        # Publish the real Execute phase (with the coding agent's transcript)
        # under the kick-off turn so the dashboard shows what the sandbox
        # actually did — the observability surface for an agent that emits no
        # OTLP. Telemetry only; never aborts the completion.
        try:
            await publish_sandbox_completion_phase(
                event_queue=self._event_queue,
                agent=agent,
                run=run,
                result=result,
            )
        except Exception:
            logger.exception("sandbox_transcript_publish_failed", turn_id=run.turn_id)

        if result.needs_input:
            # A clarification wait frees the agent (a human reply can take
            # days) and lets its inbox flow so the answer can arrive.
            self._free_agent(agent)
            await self._event_queue.resume_topic(inbox_topic(run.agent_handle))
            await self._handle_clarification(agent, run, result, event)
            return

        # Resume the suspended Execute loop with the coding agent's findings
        # spliced in as the run_sandbox call's reply, so the SAME turn continues
        # with full context, then settle the box.
        await self._resume_execute_and_settle(
            agent=agent,
            run=run,
            result_content=_resume_result_text(result),
            result_success=result.success,
            event=event,
        )

    @staticmethod
    def _free_agent(agent: AgentInstance | None) -> None:
        """AWAITING_SANDBOX → IDLE, tolerating an agent already free."""
        if agent is not None and agent.state == AgentState.AWAITING_SANDBOX:
            agent.resume_from_sandbox()

    async def _handle_clarification(
        self,
        agent: AgentInstance | None,
        run: PendingSandboxRun,
        result: CodingAgentResult,
        event: SandboxRunCompleted,
    ) -> None:
        """Park the run + post the question on the engine's audited path.

        The sandbox never posts itself: the engine dispatches a short turn
        for the agent to ask the resolved audience on the **originating
        conversation**, so the answer arrives back on the same key and the
        next inbound there resumes the work.

        This is the wait ``pause_ttl`` exists for, and the only place the
        knob applies: every other pause in the lifecycle is settled by the
        tail that made it, but this one is open-ended — a person can take
        days, and the paused box burns snapshot storage the whole time. The
        box stays paused, its TTL now ticking for the
        :class:`~crewlet.sandbox.waiter.SandboxWaiter`'s reaper — unless the
        deployment set ``pause_ttl_seconds: 0``, which is "never hold a
        blocked box": tear it down now and park straight into ``reseed`` for
        zero snapshot cost. The answer resumes the work either way; only the
        starting point differs (live checkout vs the pushed git branch).
        """
        await self._pending_store.mark_awaiting_clarification(
            run.turn_id,
            question=result.question,
            audience=result.ask_to,
            conversation_key=run.conversation_key,
        )
        if run.pause_ttl_seconds == 0:
            await teardown_sandbox_run(
                run=run, manager=self._manager, pending_store=self._pending_store
            )
            await self._pending_store.set_status(run.turn_id, "reseed")
        await self._event_queue.publish(
            "crewlet.events.sandbox_clarification_requested",
            SandboxClarificationRequested(
                source=run.role,
                agent_id=run.agent_id,
                agent_handle=run.agent_handle,
                role=run.role,
                turn_id=run.turn_id,
                sandbox_id=run.sandbox_id,
                question=result.question,
                audience=result.ask_to,
                conversation_key=run.conversation_key,
                trace_id=run.trace_id,
                span_id=run.span_id,
            ),
        )
        logger.info(
            "sandbox_run_awaiting_clarification",
            turn_id=run.turn_id,
            audience=result.ask_to,
        )
        # Post the question via a normal turn (engine identity, audited).
        turn_engine = self._get_turn_engine()
        if turn_engine is not None and agent is not None:
            await turn_engine.run_turn(
                agent,
                task_id=run.turn_id,
                task_description=_post_question_prompt(run, result),
                org=self._get_org(),
                notification_metadata=run.notification_metadata or None,
                event=event,
            )

    async def try_resume_from_answer(self, agent: AgentInstance, event: Event) -> bool:
        """If ``event`` answers a parked clarification, resume the work.

        The clarification parked the SAME suspended Execute loop the
        ``run_sandbox`` call left waiting (its ``execute_state`` is preserved and
        the box stays paused). Splice the human's answer in as that call's reply
        and resume the loop — the executor continues with full context (it calls
        ``run_sandbox`` again, reusing the paused checkout) rather than starting
        a fresh turn that re-plans and re-investigates from scratch.

        Returns True when it claimed + resumed (the caller then skips normal
        handling), False otherwise. v1 disambiguation: the next inbound on the
        question's conversation while a clarification is pending is the answer.
        """
        conv_key = conversation_key(event)
        run = await self._pending_store.find_awaiting_by_conversation(conv_key)
        if run is None:
            return False
        claimed = await self._pending_store.claim_for_resume(run.turn_id)
        if claimed is None:
            # Another inbound already claimed it; treat as handled so we
            # don't also run it as an unrelated message.
            return True
        answer = _answer_text(event)
        logger.info(
            "sandbox_clarification_answered",
            turn_id=claimed.turn_id,
            conversation_key=conv_key,
        )
        # Resume as the run's OWNER (it holds the conversation, sandbox, and
        # creds), not whichever agent the answer happened to route to.
        owner = self._get_agent(claimed.agent_handle) or agent
        await self._resume_execute_and_settle(
            agent=owner,
            run=claimed,
            result_content=_clarification_resume_text(claimed, answer),
            result_success=True,
            event=event,
        )
        return True

    async def _account(self, run: PendingSandboxRun, result: CodingAgentResult) -> None:
        """Post-account a collected run's tokens (budget cascade + ledger)."""
        tokens = result.input_tokens + result.output_tokens
        if self._budget_manager is not None and tokens and run.agent_id:
            try:
                await self._budget_manager.consume(run.agent_id, tokens)
            except Exception:
                logger.exception("sandbox_budget_consume_failed", turn_id=run.turn_id)
        if self._token_usage_repo is not None and tokens and run.agent_handle:
            try:
                await self._token_usage_repo.increment(run.agent_handle, tokens)
            except Exception:
                logger.exception("sandbox_token_increment_failed", turn_id=run.turn_id)

    async def _resume_execute_and_settle(
        self,
        *,
        agent: AgentInstance | None,
        run: PendingSandboxRun,
        result_content: str,
        result_success: bool,
        event: Event,
    ) -> None:
        """Resume the suspended Execute loop, then settle the paused box.

        Shared by the completion path (coding-agent findings) and the
        clarification-answer path (the human's reply): ``result_content`` is
        spliced into the loop as the pending ``run_sandbox`` call's reply and the
        SAME turn continues with full context. After the resumed Execute returns:
        if the executor called ``run_sandbox`` AGAIN the row was flipped back to
        ``running`` (the paused box is reused) — leave it for the next
        completion; otherwise the Execute phase is done with the box, so tear it
        down and mark the run ``done``.
        """
        if not run.execute_state:
            # No suspended conversation to resume (e.g. a crash between launch
            # and the suspend persist). Can't continue the turn — fail + reap,
            # free the agent, and let its inbox flow again.
            logger.warning("sandbox_resume_no_execute_state", turn_id=run.turn_id)
            await self._pending_store.set_status(run.turn_id, "failed")
            await teardown_sandbox_run(
                run=run, manager=self._manager, pending_store=self._pending_store
            )
            self._free_agent(agent)
            await self._event_queue.resume_topic(inbox_topic(run.agent_handle))
            return
        # Free the agent only NOW, immediately before the resume dispatch, so
        # no queued event can take the WORKING slot first (the inbox is still
        # paused; ``run_turn`` additionally WAITS on a busy agent rather than
        # erroring, so even an in-flight interloper only delays the resume).
        self._free_agent(agent)
        try:
            await self._dispatch_resume_execute(
                agent=agent,
                run=run,
                result_content=result_content,
                result_success=result_success,
                event=event,
            )
        except Exception:
            # UN-CLAIM so the retry can win the flip again: without this the
            # NAK'd completion event redelivers, ``claim_for_resume`` returns
            # None, and the suspended Execute loop is permanently lost with
            # the row stranded in 'resumed'. Revert to the exact pre-claim
            # status the claim snapshotted.
            revert_to = run.claimed_from or "running"
            logger.exception(
                "sandbox_resume_dispatch_failed",
                turn_id=run.turn_id,
                revert_to=revert_to,
            )
            await self._pending_store.set_status(run.turn_id, revert_to)
            raise
        latest = await self._pending_store.get(run.turn_id)
        if latest is not None and latest.status == "running":
            # The resumed executor called run_sandbox AGAIN: a new detached
            # job owns the box and the suspending turn re-parked the agent —
            # leave the inbox paused for the next completion.
            logger.info("sandbox_reused_in_turn", turn_id=run.turn_id)
        else:
            # Tear down the box the RESUMED row points at: a re-seeded run
            # provisioned a fresh one, so ``run``'s (possibly reaped) id is
            # stale.
            await teardown_sandbox_run(
                run=latest or run,
                manager=self._manager,
                pending_store=self._pending_store,
            )
            await self._pending_store.set_status(run.turn_id, "done")
            await self._event_queue.resume_topic(inbox_topic(run.agent_handle))

    async def _dispatch_resume_execute(
        self,
        *,
        agent: AgentInstance | None,
        run: PendingSandboxRun,
        result_content: str,
        result_success: bool,
        event: Event,
    ) -> None:
        """Resume the suspended Execute loop with ``result_content``.

        The run_sandbox tool suspended the Execute tool-loop and persisted the
        conversation in ``run.execute_state``. We rebuild an
        :class:`ExecuteResumeState` — the saved conversation + ``result_content``
        as the pending call's reply — and continue the SAME turn (under
        ``run.turn_id``, iteration offset past the kick-off) so the executor
        reports / acts with full context using its own tools. ``run``'s stored
        ``notification_metadata`` is threaded back so the resumed executor's
        reply tools route to the originating conversation.
        """
        from crewlet.agent.execute import ExecuteResumeState
        from crewlet.agent.plan import ExecutionPlan

        turn_engine = self._get_turn_engine()
        if turn_engine is None or agent is None:
            logger.warning(
                "sandbox_resume_skipped",
                turn_id=run.turn_id,
                have_engine=turn_engine is not None,
                have_agent=agent is not None,
            )
            return
        st = run.execute_state or {}
        try:
            plan = (
                ExecutionPlan.model_validate(run.plan) if run.plan else ExecutionPlan()
            )
        except Exception:
            logger.warning("sandbox_resume_plan_decode_failed", turn_id=run.turn_id)
            plan = ExecutionPlan()
        resume = ExecuteResumeState(
            plan=plan,
            result_content=result_content,
            messages=list(st.get("messages", [])),
            pending_tool_call_id=str(st.get("pending_tool_call_id", "")),
            pending_tool_name=str(st.get("pending_tool_name", "") or "run_sandbox"),
            active_tool_names=list(st.get("active_tool_names", [])),
            loaded_skill_keys=list(st.get("loaded_skill_keys", [])),
            result_success=result_success,
            prior_tool_executions=list(st.get("tool_executions", [])),
            iteration_history=list(st.get("iteration_history", [])),
        )
        await turn_engine.run_turn(
            agent,
            task_id=run.turn_id,
            task_description=run.task_description,
            org=self._get_org(),
            notification_metadata=run.notification_metadata or None,
            event=event,
            turn_id=run.turn_id,
            start_iteration=int(st.get("iteration", 1) or 1),
            resume_state=resume,
        )

    # -- restart recovery --------------------------------------------------

    async def recover(self) -> None:
        """Re-attach to still-active runs on boot so a restart never orphans.

        Running jobs re-pause their agents' inboxes and re-enter the busy
        state; the poll waiter then drives them to completion.
        Clarification / re-seed runs are left for their answer — their boxes
        are paused, and the waiter's reaper expires them on TTL.

        A ``resumed`` row can only mean the previous engine died between
        claiming a completion and settling it. Nothing will ever pick it up
        again — the at-most-once claim already flipped, so the redelivered
        completion is refused — and its box is sitting paused. Reap it here:
        this is the one moment the row is unambiguously abandoned (no live
        process holds it), so it is safe to do what no TTL would.
        """
        active = await self._pending_store.list_active()
        if not active:
            return
        recovered = 0
        abandoned = 0
        for run in active:
            if run.status == "resumed":
                await self._reap_abandoned_tail(run)
                abandoned += 1
                continue
            if run.status != "running":
                continue
            await self._event_queue.pause_topic(inbox_topic(run.agent_handle))
            agent = self._get_agent(run.agent_handle)
            if agent is not None:
                agent.await_sandbox(task_id=run.turn_id)
            recovered += 1
        logger.info(
            "sandbox_runs_recovered",
            running=recovered,
            abandoned=abandoned,
            active=len(active),
        )

    async def _reap_abandoned_tail(self, run: PendingSandboxRun) -> None:
        """Tear down a ``resumed`` run left behind by a dead engine."""
        logger.warning(
            "sandbox_abandoned_tail_reaped",
            turn_id=run.turn_id,
            agent=run.agent_handle,
            sandbox_id=run.sandbox_id,
        )
        await teardown_sandbox_run(
            run=run, manager=self._manager, pending_store=self._pending_store
        )
        await self._pending_store.set_status(run.turn_id, "failed")


def _resume_result_text(result: CodingAgentResult) -> str:
    """The run_sandbox tool's reply spliced into the resumed Execute loop.

    Carries the coding agent's findings (reported text + delivered refs, or the
    error) framed so the executor continues the task — reports / acts on the
    originating channel. Secret-redacted: it becomes a tool message published
    as the resumed Execute phase.
    """
    status = "succeeded" if result.success else "did NOT fully succeed"
    lines = [f"The sandbox coding run {status}."]
    if result.delivered_refs:
        lines.append("Delivered: " + ", ".join(result.delivered_refs))
    if result.text:
        lines.append("\n" + result.text)
    elif result.error:
        lines.append("\nError: " + result.error)
    lines.append(
        "\nThe sandbox did the code work above — do NOT redo it. Continue the "
        "task: report the outcome to the requester on the channel it came from "
        "(on success share the result / PR; on failure explain what blocked it "
        "and what's needed), and take any remaining action with your tools."
    )
    return redact_secrets("\n".join(lines))


_AUDIENCE_PHRASE = {
    "requester": "the person who asked for this task",
    "team": "your team",
    "manager": "your manager",
}


def _post_question_prompt(run: PendingSandboxRun, result: CodingAgentResult) -> str:
    """Seed text for the turn that posts a sandbox clarification."""
    who = _AUDIENCE_PHRASE.get(result.ask_to, result.ask_to or "the requester")
    return (
        "A sandboxed coding job you started for this task is blocked and needs "
        f"a person's answer. Post this question to {who} on the conversation "
        "this task came from, and ask them to reply there so the work can "
        f"resume:\n\n{result.question}\n\nPost only that question via the "
        "originating channel's reply tool. Do not do anything else."
    )


def _clarification_resume_text(run: PendingSandboxRun, answer: str) -> str:
    """The run_sandbox reply spliced into the Execute loop when a parked
    clarification is answered.

    The earlier ``run_sandbox`` call paused because the coding agent needed a
    person's decision before it could finish. This hands the answer back to the
    executor (as that call's result) and tells it to continue the coding work
    by calling ``run_sandbox`` again with the answer incorporated.

    What it must NOT do is promise a box that no longer exists. A run whose
    ``sandbox_id`` is empty had its sandbox reclaimed — reaped past
    ``pause_ttl``, or torn down the moment it blocked under
    ``pause_ttl_seconds: 0`` — so the next call provisions a fresh box: git is
    the durable state, and the brief has to say so or the coding agent starts
    by looking for a working tree that is gone. Secret-redacted: it becomes a
    tool message published as the resumed Execute phase.
    """
    lines = [
        "The sandbox coding run paused to ask a person a question before it "
        "could finish."
    ]
    if run.question:
        lines.append(f"\nQuestion it asked: {run.question}")
    lines.append(f"Their answer: {answer}")
    if run.branch:
        lines.append(f"\nWork-in-progress is on git branch: {run.branch}")
    if run.sandbox_id:
        lines.append(
            "\nThe sandbox is still up with that work-in-progress. Continue the "
            "task: call run_sandbox again with a brief that incorporates this "
            "answer so the coding agent finishes the work (it reuses the same "
            "checkout). When it's done, report the outcome to the requester on "
            "the channel this came from."
        )
    else:
        branch = (
            f"check out the existing branch `{run.branch}`"
            if run.branch
            else "check out the work-in-progress branch it pushed earlier"
        )
        lines.append(
            "\nThat sandbox has since been reclaimed, so the next run starts on "
            "a FRESH machine — the earlier working tree and the coding agent's "
            "memory of it are gone, but the pushed branch has the work. "
            "Continue the task: call run_sandbox again with a brief that tells "
            f"it to {branch}, re-read the code there, and finish the work with "
            "this answer incorporated. When it's done, report the outcome to "
            "the requester on the channel this came from."
        )
    return redact_secrets("\n".join(lines))


def _answer_text(event: Event) -> str:
    """Best-effort plain answer text from an inbound notification event."""
    for attr in ("salient_body", "body"):
        val = getattr(event, attr, "")
        if val:
            return str(val)
    return str(event.payload.get("task_description", "") or "")


__all__ = ["COMPLETIONS_TOPIC", "STARTED_TOPIC", "SandboxCoordinator", "inbox_topic"]
