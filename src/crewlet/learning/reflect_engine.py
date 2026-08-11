"""ReflectEngine — the deterministic post-turn reflection dispatcher.

Subscribes to ``crewlet.events.turn_completed`` on the EventQueue and
runs, for each emitted :class:`~crewlet.events.types.TurnCompleted`:

1. Skip if the turn already called ``reflect_and_persist`` (the LLM
   handled persistence in-flight; nothing left to do).
2. Dispatch to :class:`~crewlet.learning.persist_decider.PersistDecider`
   which decides whether to write a durable fact to the agent's
   diary.

Failures are logged and swallowed — reflection must never break the
engine.  The other learning workers (skill synthesis, counterparty
profiling) plug into this same dispatcher.
"""

from __future__ import annotations

import asyncio
from collections import OrderedDict
from typing import Any

from opentelemetry import trace

from crewlet._logging import get_logger
from crewlet.concurrency import BudgetManager, ConcurrencyController
from crewlet.events.types import (
    CounterpartyProfileUpdated,
    Event,
    PersistDeciderCompleted,
    ReflectionCompleted,
    SkillRefined,
    SkillSynthesized,
    TurnCompleted,
)
from crewlet.learning.counterparty_profiler import CounterpartyProfiler
from crewlet.learning.episode_store import EpisodeStoreProtocol
from crewlet.learning.interaction import merge_interactions_by_sender
from crewlet.learning.persist_decider import PersistDecider
from crewlet.learning.skill_refiner import SkillRefiner
from crewlet.learning.skill_scheduler import (
    SkillClusteringScheduler,
    collect_existing_skill_names,
)
from crewlet.learning.skill_synthesizer import SkillSynthesizer
from crewlet.learning.tools import was_reflect_and_persist_called
from crewlet.org.models import Organization, Role
from crewlet.providers.llm.protocol import LLMProvider
from crewlet.queue.protocol import EventQueue
from crewlet.telemetry import restore_context, set_span_error, tracer

logger = get_logger("learning.reflect_engine")

TURN_COMPLETED_TOPIC = "crewlet.events.turn_completed"
CONSUMER_GROUP = "reflect-engine"

# Post-turn reflection runs on outcomes that represent a *settled*
# turn state.  ``self_iterate`` is a mid-turn state the engine will
# reattempt; persisting durable facts or drafting skills from it risks
# reinforcing incomplete work.  ``done`` and ``failed`` are terminal —
# the turn ran its course and the engine has a stable trace to learn
# from.
_REFLECT_OUTCOMES = frozenset({"done", "failed"})

# Bound on the in-memory dedup set of recently-processed turn ids.
# 1024 turns covers well over an hour of traffic on a busy org; beyond
# that a redelivered event is extremely unlikely to still be in-flight.
_MAX_SEEN_TURN_IDS = 1024


class ReflectEngine:
    """Dispatch post-turn reflection workers.

    Wired into :meth:`crewlet.engine.Engine.start` after the EpisodeStore.
    Owns its own subscription; does not share a consumer group with any
    other subsystem so it can be enabled/disabled independently.
    """

    def __init__(
        self,
        *,
        event_queue: EventQueue,
        llm_providers: dict[str, LLMProvider],
        organization: Organization,
        persist_decider_enabled: bool = True,
        persist_budget_tokens: int = 5000,
        diary: Any = None,
        counterparty_profiler: CounterpartyProfiler | None = None,
        skill_synthesizer: SkillSynthesizer | None = None,
        skill_scheduler: SkillClusteringScheduler | None = None,
        skill_refiner: SkillRefiner | None = None,
        episode_store: EpisodeStoreProtocol | None = None,
        single_turn_min_tool_calls: int = 5,
        concurrency: ConcurrencyController | None = None,
        budget_manager: BudgetManager | None = None,
    ) -> None:
        self._event_queue = event_queue
        self._llm_providers = llm_providers
        self._org = organization
        self._persist_decider_enabled = persist_decider_enabled
        self._persist_decider: PersistDecider | None = None
        # PersistDecider writes durable facts to the AgentDiary.
        # Without a wired diary the decider can't persist -- skip
        # construction so the post-turn dispatch sees
        # ``self._persist_decider is None`` and short-circuits.
        if persist_decider_enabled and diary is not None:
            self._persist_decider = PersistDecider(
                llm_providers=llm_providers,
                diary=diary,
                budget_tokens=persist_budget_tokens,
                event_queue=event_queue,
            )
        self._counterparty_profiler = counterparty_profiler
        self._skill_synthesizer = skill_synthesizer
        self._skill_scheduler = skill_scheduler
        self._skill_refiner = skill_refiner
        self._episode_store = episode_store
        self._single_turn_min_tool_calls = max(1, int(single_turn_min_tool_calls))
        self._concurrency = concurrency
        self._budget_manager = budget_manager
        self._processed_turn_ids: OrderedDict[str, None] = OrderedDict()
        self._running = False
        self._subscribed = False

    async def start(self) -> None:
        if self._running:
            return
        self._running = True
        if not self._subscribed:
            await self._event_queue.subscribe(
                TURN_COMPLETED_TOPIC, CONSUMER_GROUP, self._handle
            )
            self._subscribed = True
        if self._skill_scheduler is not None:
            try:
                await self._skill_scheduler.start()
            except Exception:
                logger.exception("skill_scheduler_start_failed")
        logger.info(
            "reflect_engine_started",
            persist_decider=self._persist_decider_enabled,
            counterparty_profiler=self._counterparty_profiler is not None,
            skill_synthesizer=self._skill_synthesizer is not None,
            skill_scheduler=self._skill_scheduler is not None,
        )

    async def stop(self) -> None:
        if not self._running:
            return
        self._running = False
        if self._skill_scheduler is not None:
            try:
                await self._skill_scheduler.stop()
            except Exception:
                logger.exception("skill_scheduler_stop_failed")
        logger.info("reflect_engine_stopped")

    async def _handle(self, event: Event) -> None:
        """Entry point for each TurnCompleted arrival."""
        if not self._running:
            return
        if not isinstance(event, TurnCompleted):
            # Defensive: the subscription is typed by topic, but event
            # payloads may deserialise to plain ``Event`` in some
            # backends.  Attempt duck-typed access for robustness.
            return await self._handle_duck(event)
        await self._dispatch_with_trace(event)

    async def _handle_duck(self, event: Any) -> None:
        """Duck-typed fallback when the event arrives as a base Event."""
        if getattr(event, "type", "") != "turn_completed":
            return
        # Rebuild a TurnCompleted from the payload-ish fields that
        # queue backends forward.
        try:
            reconstructed = TurnCompleted.model_validate(
                event.model_dump(mode="python")
            )
        except Exception:
            logger.debug("reflection_skipped_unparseable_event")
            return
        await self._dispatch_with_trace(reconstructed)

    async def _dispatch_with_trace(self, event: TurnCompleted) -> None:
        """Wrap ``_reflect`` in a ``learning.reflect`` span.

        Restores the trace context captured by the original ``agent.turn``
        span (publisher set ``trace_id`` / ``span_id`` on the event via
        the ``Event`` base) so the dashboard nests reflection workers
        under the same trace as the turn that produced them.  The
        per-worker spans inside ``_reflect`` then attach as grandchildren.
        """
        otel_context = restore_context(event.trace_id, event.span_id)
        with tracer.start_as_current_span(
            "learning.reflect",
            context=otel_context,
            attributes={
                "learning.turn_id": event.turn_id,
                "learning.agent_id": event.agent_id,
                "learning.agent_handle": event.agent_handle,
                "learning.role": event.role,
                "learning.review_outcome": event.review_outcome,
                "learning.tool_count": len(event.tool_sequence or []),
                "learning.skills_used": len(event.skills_used or []),
            },
        ):
            try:
                await self._reflect(event)
            except Exception as exc:
                set_span_error(exc)
                logger.exception("reflection_failed", turn_id=event.turn_id)

    async def _reflect(self, event: TurnCompleted) -> None:
        span = trace.get_current_span()

        # No-op fast path when nothing is wired — saves the dedup,
        # budget, and concurrency work on every incoming event.
        workers = (
            self._persist_decider,
            self._counterparty_profiler,
            self._skill_synthesizer,
            self._skill_refiner,
        )
        if all(w is None for w in workers):
            span.set_attribute("learning.skip_reason", "no_workers")
            return

        role = self._resolve_role(event)
        if role is None:
            span.set_attribute("learning.skip_reason", "no_role")
            logger.debug(
                "reflection_skipped_no_role",
                turn_id=event.turn_id,
                role_name=event.role,
            )
            return

        # Per-role opt-out.  ``Role.learning_enabled`` defaults to None
        # (inherit the system-level flag); setting it to False lets
        # operators silence reflection for noisy or sensitive roles
        # without disabling the whole subsystem.
        if role.learning_enabled is False:
            span.set_attribute("learning.skip_reason", "role_disabled")
            logger.debug(
                "reflection_skipped_role_disabled",
                turn_id=event.turn_id,
                role_name=role.name,
            )
            return

        # Idempotency — queue backends (Pulsar, the in-memory
        # queue under test) may redeliver the same TurnCompleted event
        # on transient failure.  A bounded OrderedDict of recent turn
        # ids lets the engine short-circuit duplicates without needing
        # any DB round-trip.
        #
        # The mark is set *now* (before the gates) so a concurrent
        # redelivery of the same event is blocked immediately -- but it
        # is *cleared* on the transient early-return paths below
        # (budget exhausted, concurrency-acquire failure) via
        # ``_release_turn_id`` so a genuine redelivery after a transient
        # skip still gets retried.  Deterministic skips (``no_action``)
        # keep the mark: re-deriving them on a redelivery is wasted work
        # with no new outcome.
        if event.turn_id in self._processed_turn_ids:
            span.set_attribute("learning.skip_reason", "duplicate")
            logger.debug("reflection_skipped_duplicate", turn_id=event.turn_id)
            return
        self._processed_turn_ids[event.turn_id] = None
        if len(self._processed_turn_ids) > _MAX_SEEN_TURN_IDS:
            self._processed_turn_ids.popitem(last=False)

        # Gate — agent didn't actually engage.  Two routes here, both
        # produce phantom-directive learning if not blocked:
        #
        #   (a) plan_decision == "skip" -- planner explicitly opted out
        #       (e.g. recognised the trigger was for someone else and
        #       called submit_plan(decision="skip")).
        #   (b) plan_decision != "skip" but tool_sequence is empty and
        #       outcome is "done" -- the planner *intended* to skip
        #       but failed to call submit_plan, so the system coerced
        #       to decision="direct" and Execute ran with the full
        #       registry but emitted nothing actionable.  Same outcome
        #       from a learning perspective: the agent processed
        #       nothing externally observable.
        #
        # Either way, persisting facts read off the trigger body would
        # teach the agent directives it never received -- e.g. "User
        # Sam prefers replies opened with 'hey sam'" landing in
        # agent-pm's memory because Sam was @-mentioning agent-ceo
        # in a shared channel and agent-pm just observed the trigger.
        #
        # Note: this fires BEFORE the terminal-outcome / budget /
        # concurrency gates because all four learning workers depend
        # on the agent having actually engaged with the trigger.  A
        # narrow ``plan_decision == "skip"`` check is a special case
        # of the broader "no engagement" condition.
        no_action = event.plan_decision == "skip" or (
            not event.tool_sequence and event.review_outcome == "done"
        )
        if no_action:
            reason = (
                "plan_decision_skip"
                if event.plan_decision == "skip"
                else "no_tools_executed"
            )
            span.set_attribute("learning.skip_reason", reason)
            logger.info(
                "reflection_skipped_no_engagement",
                turn_id=event.turn_id,
                agent_handle=event.agent_handle,
                reason=reason,
                plan_decision=event.plan_decision,
                tool_count=len(event.tool_sequence),
                review_outcome=event.review_outcome,
            )
            await self._publish_lifecycle(
                ReflectionCompleted(
                    source=role.name,
                    agent_id=event.agent_id,
                    agent_handle=event.agent_handle,
                    role=event.role,
                    turn_id=event.turn_id,
                    workers_run=0,
                    review_outcome=event.review_outcome,
                )
            )
            return

        # Gate 1 — terminal-outcome filter.  ``self_iterate`` is a
        # mid-turn state; persisting / synthesising from it risks
        # reinforcing incomplete work.  PersistDecider +
        # SkillSynthesizer + SkillRefiner all skip these outcomes.  The
        # CounterpartyProfiler is the exception — it observes the
        # counterparty regardless of what the agent decided to do next.
        is_terminal = event.review_outcome in _REFLECT_OUTCOMES
        span.set_attribute("learning.is_terminal", is_terminal)

        # Gate 2 — budget pre-flight.  If the agent or the org is already
        # exhausted, every aux-LLM call would fail or silently burn
        # cycles.  Skip the whole pass -- and release the idempotency
        # mark: a budget can be topped up, so a later redelivery of this
        # event should get a real reflection pass rather than being
        # dropped as a duplicate.
        if self._is_budget_exhausted(event.agent_id):
            span.set_attribute("learning.skip_reason", "budget_exhausted")
            logger.info(
                "reflection_skipped_budget_exhausted",
                turn_id=event.turn_id,
                agent_id=event.agent_id,
            )
            self._release_turn_id(event.turn_id)
            return

        unit_id = self._resolve_unit_id(role)
        org_id = self._org.name or ""

        # Gate 3 — concurrency slot.  One slot per role, held across all
        # worker dispatch, so a burst of ``TurnCompleted`` events does not
        # fan out unbounded aux-LLM traffic.  The clustering scheduler
        # acquires its own per-agent slots on the scheduled path; this is
        # the inline equivalent.
        acquired_role: str | None = None
        if self._concurrency is not None:
            try:
                await self._concurrency.acquire(role.name)
                acquired_role = role.name
            except Exception as exc:
                span.set_attribute("learning.skip_reason", "concurrency_acquire_failed")
                set_span_error(exc)
                logger.exception("reflect_acquire_failed", turn_id=event.turn_id)
                # Transient: release the idempotency mark so a
                # redelivery retries instead of being dropped.
                self._release_turn_id(event.turn_id)
                return
        # Tracks how many workers entered their span block; reported on
        # the trailing ``ReflectionCompleted`` event for telemetry.
        workers_run = 0
        try:
            # Worker 1: PersistDecider (skipped on non-terminal outcomes
            # and when the LLM already invoked ``reflect_and_persist``
            # during the turn).
            if self._persist_decider is not None:
                workers_run += 1
                with tracer.start_as_current_span(
                    "learning.persist_decider",
                    attributes={
                        "learning.worker": "persist_decider",
                        "learning.turn_id": event.turn_id,
                    },
                ) as worker_span:
                    if not is_terminal:
                        worker_span.set_attribute("learning.outcome", "skipped")
                        worker_span.set_attribute(
                            "learning.skip_reason", "non_terminal"
                        )
                        logger.debug(
                            "reflection_skipped_non_terminal",
                            turn_id=event.turn_id,
                            outcome=event.review_outcome,
                        )
                    # ``reflect_and_persist`` is a Plan-phase-only builtin,
                    # so it lands in ``plan_tool_sequence`` -- never in the
                    # Execute-scoped ``tool_sequence``.  Check both so the
                    # dedup still holds if the tool's registration phase
                    # ever changes.
                    elif was_reflect_and_persist_called(
                        [*event.plan_tool_sequence, *event.tool_sequence]
                    ):
                        worker_span.set_attribute("learning.outcome", "skipped")
                        worker_span.set_attribute(
                            "learning.skip_reason", "self_persisted"
                        )
                        logger.debug(
                            "reflection_skipped_self_persisted", turn_id=event.turn_id
                        )
                    else:
                        try:
                            outcome = await self._persist_decider.decide_and_persist(
                                role=role,
                                agent_id=event.agent_id,
                                agent_handle=event.agent_handle,
                                unit_id=unit_id,
                                org_id=org_id,
                                turn_id=event.turn_id,
                                task_summary=event.task_summary,
                                plan_summary=event.plan_summary,
                                tool_sequence=list(event.tool_sequence),
                                review_outcome=event.review_outcome,
                                interactions=list(event.interactions),
                            )
                            worker_span.set_attribute(
                                "learning.outcome",
                                "done" if outcome.persisted else outcome.kind.lower(),
                            )
                            worker_span.set_attribute(
                                "learning.classification", outcome.kind
                            )
                            if outcome.result is not None:
                                worker_span.set_attribute(
                                    "learning.doc_id", outcome.result.doc_id
                                )
                                worker_span.set_attribute(
                                    "learning.scope", outcome.result.scope
                                )
                            logger.info(
                                "persist_decider_completed",
                                turn_id=event.turn_id,
                                persisted=outcome.persisted,
                                classification=outcome.kind,
                                doc_id=(
                                    outcome.result.doc_id if outcome.result else ""
                                ),
                            )
                            await self._publish_lifecycle(
                                PersistDeciderCompleted(
                                    source=role.name,
                                    agent_id=event.agent_id,
                                    agent_handle=event.agent_handle,
                                    role=event.role,
                                    turn_id=event.turn_id,
                                    persisted=outcome.persisted,
                                    classification=outcome.kind,
                                    doc_id=(
                                        outcome.result.doc_id if outcome.result else ""
                                    ),
                                    scope=(
                                        outcome.result.scope if outcome.result else ""
                                    ),
                                    ttl_until=(
                                        outcome.result.ttl_until
                                        if outcome.result
                                        else ""
                                    ),
                                    review_outcome=event.review_outcome,
                                )
                            )
                        except Exception as exc:
                            worker_span.set_attribute("learning.outcome", "failed")
                            set_span_error(exc)
                            logger.exception(
                                "persist_decider_dispatch_failed",
                                turn_id=event.turn_id,
                            )

            # Worker 2: CounterpartyProfiler (runs whenever the turn has
            # identifiable trigger senders, regardless of outcome —
            # observing the counterparty is valuable even on mid-turn
            # handoffs).  One pass per DISTINCT sender: a coalesced
            # trigger merging four messages from one human is one
            # counterparty; a multi-human thread is genuinely several.
            # The aux-LLM cost is demand-proportional to the people
            # actually involved — and the passes run CONCURRENTLY:
            # each is an independent aux-LLM call + a distinct-row
            # upsert, and this reflection pass holds the per-role
            # concurrency slot while blocking the engine's single
            # turn-completion consumer, so a multi-human batch must
            # cost the max of its passes, not the sum.
            if self._counterparty_profiler is not None:
                sender_interactions = merge_interactions_by_sender(event.interactions)
                workers_run += len(sender_interactions)
                if sender_interactions:
                    await asyncio.gather(
                        *(
                            self._profile_counterparty(
                                role=role, event=event, interaction=interaction
                            )
                            for interaction in sender_interactions
                        )
                    )

            # Worker 3: SkillSynthesizer (single-turn path).  Fires when
            # the turn finished ``done`` and used >= min_tool_calls
            # tools.  Clustered synthesis is the scheduler's job and
            # runs separately.
            if (
                self._skill_synthesizer is not None
                and self._episode_store is not None
                and event.review_outcome == "done"
                and len(event.tool_sequence) >= self._single_turn_min_tool_calls
            ):
                workers_run += 1
                with tracer.start_as_current_span(
                    "learning.skill_synthesizer",
                    attributes={
                        "learning.worker": "skill_synthesizer",
                        "learning.turn_id": event.turn_id,
                        "learning.trigger": "single_turn",
                    },
                ) as worker_span:
                    try:
                        episode = await self._episode_store.fetch_by_turn_id(
                            event.turn_id
                        )
                    except Exception as exc:
                        episode = None
                        set_span_error(exc)
                        logger.exception(
                            "skill_synthesis_episode_fetch_failed",
                            turn_id=event.turn_id,
                        )
                    if episode is None:
                        worker_span.set_attribute("learning.outcome", "skipped")
                        worker_span.set_attribute(
                            "learning.skip_reason", "episode_unavailable"
                        )
                    else:
                        existing = collect_existing_skill_names()
                        try:
                            skill = await self._skill_synthesizer.synthesize(
                                role=role,
                                agent_handle=event.agent_handle,
                                source_episodes=[episode],
                                existing_skill_names=existing,
                                trigger="single_turn",
                                agent_id=event.agent_id,
                            )
                            worker_span.set_attribute(
                                "learning.outcome", "done" if skill else "noop"
                            )
                            if skill is not None:
                                worker_span.set_attribute(
                                    "learning.skill_name", skill.name
                                )
                                await self._publish_lifecycle(
                                    SkillSynthesized(
                                        source=role.name,
                                        agent_id=event.agent_id,
                                        agent_handle=event.agent_handle,
                                        role=event.role,
                                        turn_id=event.turn_id,
                                        skill_name=skill.name,
                                        skill_id=str(skill.id),
                                        trigger="single_turn",
                                        cluster_size=1,
                                        tool_count=len(skill.tool_sequence or []),
                                    )
                                )
                        except Exception as exc:
                            worker_span.set_attribute("learning.outcome", "failed")
                            set_span_error(exc)
                            logger.exception(
                                "skill_synthesizer_dispatch_failed",
                                turn_id=event.turn_id,
                            )

            # Worker 4: SkillRefiner.  Fires when this turn
            # used any synthesized skill AND reached a terminal outcome
            # — we don't want to annotate a skill based on a mid-turn
            # retry state.
            if self._skill_refiner is not None and is_terminal and event.skills_used:
                workers_run += 1
                with tracer.start_as_current_span(
                    "learning.skill_refiner",
                    attributes={
                        "learning.worker": "skill_refiner",
                        "learning.turn_id": event.turn_id,
                        "learning.skills_used": len(event.skills_used),
                    },
                ) as worker_span:
                    try:
                        refined = await self._skill_refiner.refine_from_turn(
                            role=role,
                            agent_handle=event.agent_handle,
                            skills_used=list(event.skills_used),
                            review_outcome=event.review_outcome,
                            task_summary=event.task_summary,
                            plan_summary=event.plan_summary,
                            turn_id=event.turn_id,
                            agent_id=event.agent_id,
                        )
                        worker_span.set_attribute(
                            "learning.outcome", "done" if refined else "noop"
                        )
                        if refined is not None:
                            worker_span.set_attribute(
                                "learning.skill_name", refined.name
                            )
                            worker_span.set_attribute(
                                "learning.skill_version", refined.version
                            )
                            kind = (
                                "observed_in_practice"
                                if event.review_outcome == "done"
                                else "counter_example"
                            )
                            await self._publish_lifecycle(
                                SkillRefined(
                                    source=role.name,
                                    agent_id=event.agent_id,
                                    agent_handle=event.agent_handle,
                                    role=event.role,
                                    turn_id=event.turn_id,
                                    skill_name=refined.name,
                                    skill_id=str(refined.id),
                                    skill_version=refined.version,
                                    refinement_kind=kind,
                                )
                            )
                    except Exception as exc:
                        worker_span.set_attribute("learning.outcome", "failed")
                        set_span_error(exc)
                        logger.exception(
                            "skill_refiner_dispatch_failed", turn_id=event.turn_id
                        )

            # Trailing sentinel — flips agent state back to "idle" in the
            # dashboard's state derivation now that the aux phase events
            # emitted by workers above are no longer the latest event for
            # this role.  Always emitted on a real reflection pass (i.e.
            # past all early-return gates) so it pairs reliably with the
            # ``agent_phase_completed phase=auxiliary`` events workers
            # may have produced.
            span.set_attribute("learning.workers_run", workers_run)
            await self._publish_lifecycle(
                ReflectionCompleted(
                    source=role.name,
                    agent_id=event.agent_id,
                    agent_handle=event.agent_handle,
                    role=event.role,
                    turn_id=event.turn_id,
                    workers_run=workers_run,
                    review_outcome=event.review_outcome,
                )
            )
        finally:
            if acquired_role is not None:
                try:
                    self._concurrency.release(acquired_role)
                except Exception:
                    logger.exception("reflect_release_failed", turn_id=event.turn_id)

    async def _profile_counterparty(
        self, *, role: Role, event: TurnCompleted, interaction: Any
    ) -> None:
        """One CounterpartyProfiler pass for one distinct sender.

        Runs under its own OTel span; exceptions are swallowed (logged
        + span-marked) so one sender's failure never breaks the others
        gathered alongside it or the rest of the reflection pass.
        """
        assert self._counterparty_profiler is not None
        with tracer.start_as_current_span(
            "learning.counterparty_profiler",
            attributes={
                "learning.worker": "counterparty_profiler",
                "learning.turn_id": event.turn_id,
                "learning.subject_handle": interaction.sender.handle or "",
                "learning.subject_platform": interaction.sender.platform or "",
            },
        ) as worker_span:
            try:
                traits_patched = await self._counterparty_profiler.update_from_turn(
                    role=role,
                    observer_handle=event.agent_handle,
                    interaction=interaction,
                    plan_summary=event.plan_summary,
                    review_outcome=event.review_outcome,
                    turn_id=event.turn_id,
                    observer_agent_id=event.agent_id,
                )
                worker_span.set_attribute("learning.outcome", "done")
                if traits_patched is not None:
                    worker_span.set_attribute("learning.traits_patched", traits_patched)
                    await self._publish_lifecycle(
                        CounterpartyProfileUpdated(
                            source=role.name,
                            observer_handle=event.agent_handle,
                            role=event.role,
                            turn_id=event.turn_id,
                            subject_handle=interaction.sender.handle,
                            subject_external_id=interaction.sender.external_id,
                            subject_platform=interaction.sender.platform,
                            subject_name=interaction.sender.display_name,
                            traits_patched=traits_patched,
                        )
                    )
            except Exception as exc:
                worker_span.set_attribute("learning.outcome", "failed")
                set_span_error(exc)
                logger.exception(
                    "counterparty_profiler_dispatch_failed",
                    turn_id=event.turn_id,
                )

    async def _publish_lifecycle(self, event: Event) -> None:
        """Publish a learning lifecycle event on the engine's queue.

        Best-effort: a queue failure must not break the worker that
        emitted the event.  Trace context is auto-captured by the
        ``Event`` base from the active OTel span (the surrounding
        ``learning.<worker>`` span), so the dashboard's trace-grouped
        view nests these events under the same card as the parent
        ``agent.turn``.
        """
        try:
            await self._event_queue.publish(f"crewlet.events.{event.type}", event)
        except Exception:
            logger.exception("learning_event_publish_failed", event_type=event.type)

    def _release_turn_id(self, turn_id: str) -> None:
        """Undo the idempotency mark for ``turn_id``.

        Called on the transient early-return paths (budget exhausted,
        concurrency-acquire failure) so a genuine redelivery of the
        event is reprocessed rather than silently dropped as a
        duplicate.  Deterministic skips deliberately do *not* call
        this -- re-deriving them yields no new outcome.
        """
        self._processed_turn_ids.pop(turn_id, None)

    def _is_budget_exhausted(self, agent_id: str) -> bool:
        """Cheap pre-flight check against the shared BudgetManager.

        Returns ``True`` when either the org-wide budget or the
        per-agent budget is exhausted.  Missing budget manager = not
        exhausted (the caller is in in-memory / test mode and
        BudgetManager integration is optional).
        """
        if self._budget_manager is None:
            return False
        try:
            if self._budget_manager.org_budget.is_exhausted:
                return True
            agent_budget = self._budget_manager.get_agent_budget(agent_id)
            if agent_budget is not None and agent_budget.is_exhausted:
                return True
        except Exception:
            logger.debug("reflect_budget_check_failed")
            return False
        return False

    def _resolve_role(self, event: TurnCompleted) -> Role | None:
        if not event.role:
            return None
        try:
            return self._org.get_role(event.role)
        except Exception:
            return None

    def _resolve_unit_id(self, role: Role) -> str:
        """Return the innermost unit name the role belongs to.

        Mirrors the write-side default used by
        :meth:`AgentContext.scope_id_for_store` — innermost unit, or
        empty when the role is root-level.
        """
        for unit in self._org.all_units():
            if any(r.name == role.name for r in unit.roles):
                return unit.name
        return ""
