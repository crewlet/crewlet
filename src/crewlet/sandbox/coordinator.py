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

import contextlib
from collections.abc import Callable
from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.execute_sandbox import (
    collect_sandbox_run,
    publish_sandbox_completion_phase,
    teardown_sandbox_run,
)
from crewlet.agent.instance import AgentInstance, AgentState
from crewlet.events.types import (
    Event,
    SandboxClarificationRequested,
    SandboxRunCompleted,
    SandboxRunStarted,
)
from crewlet.notifications.coalesce import conversation_key
from crewlet.queue.protocol import EventQueue
from crewlet.queue.topics import (
    agent_control_group,
    agent_control_topic,
    agent_inbox_group,
    agent_inbox_topic,
)
from crewlet.redaction import redact_secrets
from crewlet.sandbox.manager import SandboxManager
from crewlet.sandbox.pending_store import PendingSandboxRun, PendingSandboxRunStore
from crewlet.sandbox.protocol import CodingAgentResult

logger = get_logger("sandbox.coordinator")


class SandboxResumeUnavailable(RuntimeError):
    """This node cannot resume the run the completion refers to.

    Not a failure of the run — a failure of ROUTING. The suspended
    Execute conversation lives in the pending row and can only be
    resumed where the seat's instance is, so the completion has to go
    back and reach that node instead of being settled here.
    """


# Where the sandbox lifecycle is ANNOUNCED, for observability. These stay
# under ``crewlet.events.*`` because that is what the dashboard's
# broadcast stream watches — moving them would blank the
# Running-sandboxes panel. Nothing ACTS on them.
STARTED_TOPIC = "crewlet.events.sandbox_run_started"
COMPLETIONS_TOPIC = "crewlet.events.sandbox_run_completed"

# Where the sandbox lifecycle is ACTED ON: one control topic per seat,
# attached and detached alongside that seat's inbox.
#
# Routing then falls out of who subscribed, exactly as it does for the
# inbox, rather than from any "which node" computation. A single
# fleet-wide group could not work: it delivers a completion to whichever
# node wins it, which is a non-owner (N-1)/N of the time, and that node
# has no suspended Execute conversation to resume.
#
# It cannot ride the inbox either. While a seat is AWAITING_SANDBOX its
# inbox is PAUSED — that is the busy gate — so a completion arriving
# there would queue behind the very pause it exists to lift.
_GROUP = "sandbox-coordinator"

# The reason this subsystem holds a seat's inbox paused.
#
# Its OWN reason, not the default. Pause holds are reason-scoped
# precisely so one subsystem cannot un-gate another's, and three
# independent things gate an agent inbox: this busy gate, the
# config-divergence shed, and the no-turn-engine park. Sharing the
# default with the park meant the late turn-engine build's blanket
# resume released a live sandbox hold — delivering messages to an agent
# whose turn is suspended mid-run.
_SANDBOX_PAUSE_REASON = "sandbox"


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

    async def attach_seat(self, handle: str) -> None:
        """Consume one seat's sandbox control topic.

        Called from the seat acquire hook, so the node that owns the seat
        is the node that resumes its runs — and only that node.
        """
        if not handle:
            return
        await self._event_queue.subscribe(
            agent_control_topic(handle),
            agent_control_group(handle),
            self._on_control_event,
        )
        logger.info("sandbox_control_attached", seat=handle)

    async def detach_seat(self, handle: str) -> None:
        """Stop consuming a seat's control topic. Non-destructive.

        The subscription and any undelivered completion survive for the
        seat's next owner — a completion published while the seat is
        between owners must not be lost, and the box it refers to is
        still real.
        """
        if not handle:
            return
        with contextlib.suppress(Exception):
            await self._event_queue.detach(
                agent_control_topic(handle), agent_control_group(handle)
            )
        logger.info("sandbox_control_detached", seat=handle)

    async def ensure_seat_subscription(self, handle: str) -> None:
        """Make a seat's control subscription exist, owner or not.

        Same invariant as the inbox: a completion published while nobody
        is attached has to be held, not dropped. A detached run outlives
        its node, so this window is not hypothetical.
        """
        if not handle:
            return
        await self._event_queue.ensure_subscription(
            agent_control_topic(handle), agent_control_group(handle)
        )

    async def delete_seat_subscription(self, handle: str) -> None:
        """Destructive teardown, for a decommissioned seat."""
        if not handle:
            return
        with contextlib.suppress(Exception):
            await self._event_queue.delete_subscription(
                agent_control_topic(handle), agent_control_group(handle)
            )

    # -- event adapters ----------------------------------------------------

    async def _on_control_event(self, event: Event) -> None:
        if isinstance(event, SandboxRunStarted):
            await self.on_run_started(event)
        elif isinstance(event, SandboxRunCompleted):
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
        await self._event_queue.pause_topic(
            agent_inbox_topic(handle),
            agent_inbox_group(handle),
            reason=_SANDBOX_PAUSE_REASON,
        )
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
            # The job is over even though collection failed — free the
            # agent and let its inbox flow again, whatever the cleanup
            # below manages. Both of those are DB and network calls that
            # can fail on their own, and neither failing is a reason to
            # leave the seat parked on a run that is finished.
            try:
                await self._pending_store.set_status(
                    run.turn_id, "failed", epoch=run.owner_epoch or None
                )
                await teardown_sandbox_run(
                    run=run, manager=self._manager, pending_store=self._pending_store
                )
            finally:
                self._free_agent(agent)
                await self._resume_inbox(run.agent_handle)
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
            await self._resume_inbox(run.agent_handle)
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

    async def _resume_inbox(self, handle: str) -> None:
        """Release this subsystem's pause hold on a seat's inbox.

        Never raises, and never skipped on a path that is done with the
        box. The hold is reason-scoped, so nothing else in the engine can
        release it: a seat that keeps one is owned, attached and deaf,
        and stays that way until the process restarts — the run's row is
        already terminal, so neither the waiter nor a seat handoff comes
        back for it.

        Which is why every caller releases it from a ``finally``. The
        release used to be the last statement of a happy path, so a
        teardown that raised — one E2B call failing to kill a box, which
        is a network call like any other — took the seat out of service
        permanently as a side effect of failing to free a sandbox.
        """
        try:
            await self._event_queue.resume_topic(
                agent_inbox_topic(handle),
                agent_inbox_group(handle),
                reason=_SANDBOX_PAUSE_REASON,
            )
        except Exception:
            logger.exception("sandbox_inbox_resume_failed", agent_handle=handle)

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
            await self._pending_store.set_status(
                run.turn_id, "reseed", epoch=run.owner_epoch or None
            )
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
                # Already-spent tokens: see the same note in
                # ``agent/execute_sandbox.py``. A refusal cannot un-spend
                # a collected run, so recording it is the only way the
                # meter stays true when the cap is binding.
                if await self._budget_manager.record_spend(run.agent_id, tokens):
                    logger.warning(
                        "sandbox_spend_over_budget",
                        agent_id=run.agent_id,
                        turn_id=run.turn_id,
                        tokens=tokens,
                    )
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
            try:
                await self._pending_store.set_status(
                    run.turn_id, "failed", epoch=run.owner_epoch or None
                )
                await teardown_sandbox_run(
                    run=run, manager=self._manager, pending_store=self._pending_store
                )
            finally:
                self._free_agent(agent)
                await self._resume_inbox(run.agent_handle)
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
            await self._pending_store.set_status(
                run.turn_id, revert_to, epoch=run.owner_epoch or None
            )
            raise
        # From here the hold is released on EVERY exit but one: the box is
        # either finished with the seat or a new run owns it, and any
        # other outcome — a store read that fails, a teardown that
        # raises — is a reason to let the seat work, not to park it
        # forever.
        keep_paused = False
        try:
            latest = await self._pending_store.get(run.turn_id)
            if latest is not None and latest.status == "running":
                # The resumed executor called run_sandbox AGAIN: a new
                # detached job owns the box and the suspending turn
                # re-parked the agent — leave the inbox paused for the
                # next completion. The ONE deliberate exit.
                keep_paused = True
                logger.info("sandbox_reused_in_turn", turn_id=run.turn_id)
                return
            # Tear down the box the RESUMED row points at: a re-seeded run
            # provisioned a fresh one, so ``run``'s (possibly reaped) id is
            # stale.
            await teardown_sandbox_run(
                run=latest or run,
                manager=self._manager,
                pending_store=self._pending_store,
            )
            await self._pending_store.set_status(
                run.turn_id, "done", epoch=run.owner_epoch or None
            )
        finally:
            if not keep_paused:
                await self._resume_inbox(run.agent_handle)

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
            # RAISE, never return. Returning let the caller fall through
            # to settling the run `done` and tearing the box down — with
            # the suspended Execute conversation, the agent's whole
            # in-progress turn, discarded. Raising reverts the claim so
            # the completion redelivers to a node that can actually
            # resume it.
            #
            # The per-seat control topic makes this rare rather than
            # routine (a completion reaches its seat's owner by
            # construction), but "rare" is not "never": a lease can move
            # between the publish and the receive.
            raise SandboxResumeUnavailable(
                f"cannot resume run {run.turn_id!r} for seat "
                f"{run.agent_handle!r}: "
                f"turn_engine={'yes' if turn_engine is not None else 'no'} "
                f"agent={'yes' if agent is not None else 'no'}"
            )
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

    async def recover_seat(self, handle: str, *, owner: str, epoch: int) -> None:
        """Re-attach to a seat's still-active runs as this node claims it.

        Per-seat, and inside the acquire hook, because a fleet-wide boot
        scan is wrong twice over: every node would re-pause, re-park and
        reap runs belonging to seats its peers own, and a node that
        claims a seat later — a takeover, not a boot — would never
        recover it at all.

        Running jobs re-pause this seat's inbox and re-enter the busy
        state; the poll waiter then drives them to completion.
        Clarification / re-seed runs are left for their answer — their
        boxes are paused, and the waiter's reaper expires them on TTL.

        A ``resumed`` row means the engine that owned this seat died
        between claiming a completion and settling it. Nothing will ever
        pick it up — the at-most-once claim already flipped, so a
        redelivered completion is refused — and its box sits paused.
        Reaping it is safe HERE and only here: taking the seat's lease is
        what proves no live process holds the row. A boot-time scan
        proved nothing of the sort, because a peer could be mid-resume on
        a seat this node never owned.
        """
        active = await self._pending_store.list_active_for_seat(handle)
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
            await self._pending_store.claim_ownership(
                run.turn_id, owner=owner, epoch=epoch
            )
            await self._event_queue.pause_topic(
                agent_inbox_topic(run.agent_handle),
                agent_inbox_group(run.agent_handle),
                reason=_SANDBOX_PAUSE_REASON,
            )
            agent = self._get_agent(run.agent_handle)
            if agent is not None:
                agent.await_sandbox(task_id=run.turn_id)
            recovered += 1
        logger.info(
            "sandbox_seat_recovered",
            seat=handle,
            epoch=epoch,
            running=recovered,
            abandoned=abandoned,
            active=len(active),
        )

    async def release_seat(self, handle: str) -> None:
        """Stop tracking a seat's runs; the row is the durable state.

        Nothing is torn down: a detached run belongs to the
        ``pending_sandbox_run`` row, not to this process, and the seat's
        next owner recovers it through :meth:`recover_seat`. Reaping the
        box here would destroy work the successor is about to resume.
        """
        logger.debug("sandbox_seat_released", seat=handle)

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


__all__ = [
    "COMPLETIONS_TOPIC",
    "STARTED_TOPIC",
    "SandboxCoordinator",
    "SandboxResumeUnavailable",
]
