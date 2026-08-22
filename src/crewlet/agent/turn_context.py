"""TurnContext — per-turn state threaded through the Plan/Execute/Review
phases and into any sub-agent or colleague-handoff spawned from the turn.

The ``TurnContext`` owns everything that must stay consistent across the
three phases of a single turn:

- identifiers for tracing (``turn_id``, ``parent_turn_id``, OTel ids),
- the triggering event (used for OTel context restoration, exactly once),
- delegation bookkeeping (``delegation_depth``, ``delegation_chain``),
- per-turn and per-phase budgets,
- resolved per-phase LLM provider keys (for stable trace attributes).

It is intentionally a plain dataclass -- serializable so the engine can
be ported to a state-machine library later without redesigning state.
"""

from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from typing import Any, Literal

from crewlet.agent.instance import AgentInstance
from crewlet.agent.iteration_log import IterationRecord
from crewlet.events.types import Event
from crewlet.learning.interaction import InboundInteraction

Phase = Literal["plan", "execute", "review", "subagent"]


@dataclass(frozen=True)
class PlanPrefetch:
    """Frozen-at-turn-start Plan-prompt blocks.

    Resolved once on the first Plan iteration and reused on every
    subsequent ``self_iterate`` loop.  Keeping the three blocks
    immutable makes the system-prompt prefix byte-stable across
    iterations, which is what the LLM providers' prefix-cache keys
    on.

    Fields are required — an empty string means "resolved, nothing to
    show" while ``turn.plan_prefetch is None`` means "not yet
    resolved".  Separating those two states lets callers distinguish
    the first Plan iteration from subsequent ones.
    """

    counterparty_profile: str
    synthesized_skills: str
    episode_recall: str
    onboarding_hint: str = ""
    personal_memory: str = ""
    relevant_knowledge: str = ""


@dataclass
class PhaseBudget:
    """Token budget allocated to one phase of one turn.

    ``remaining`` starts at ``limit`` and is decremented as tokens are
    consumed. A ``limit`` of 0 means "no per-phase cap" (the agent's
    global budget still applies).
    """

    limit: int = 0
    used: int = 0

    @property
    def remaining(self) -> int:
        if self.limit <= 0:
            return 0  # 0 means unlimited; caller checks ``limit``
        return max(0, self.limit - self.used)

    @property
    def unlimited(self) -> bool:
        return self.limit <= 0


@dataclass
class TurnContext:
    """State threaded through a single agent turn.

    One ``TurnContext`` per turn. Sub-agent spawns produce a child
    context via :meth:`child_for_subagent`. Colleague handoffs produce
    an event that carries ``delegation_depth + 1`` and the updated
    ``delegation_chain``; the recipient's turn builds its own
    ``TurnContext`` from that event (see ``TurnEngine.run_turn``).
    """

    agent: AgentInstance
    org: Any
    trigger_event: Event | None = None

    task_id: str = ""
    task_description: str = ""
    """The ask, as it arrived.  Never mutated during the turn: a
    ``self_iterate`` round records what happened in
    ``iteration_history`` instead of appending review notes here.  That
    keeps every consumer reading the user's actual request — the
    knowledge-search query builders, the sandbox brief, the extension
    judge, and the ``Episode`` / ``TurnCompleted`` publishers all read
    this field directly.
    """

    turn_id: str = field(default_factory=lambda: str(uuid.uuid4()))
    parent_turn_id: str = ""

    delegation_depth: int = 0
    delegation_chain: list[str] = field(default_factory=list)

    notification_metadata: dict[str, Any] | None = None
    a2a_context: dict[str, Any] | None = None

    conversation_key: str = ""
    """Which conversation this turn belongs to — the Slack thread, Jira
    issue, GitHub PR (``{source}:{local}``, see
    :func:`crewlet.notifications.coalesce.conversation_key`).

    Derived once by the engine at dispatch and carried here so every
    consumer reads ONE derivation: the per-conversation session ledger,
    the ``run_sandbox`` clarification round-trip, the episode row, and
    the turn's telemetry.  Empty, or ``event:``-prefixed, when the
    trigger has no conversation a later message could reproduce (a
    scheduled fire, a task assignment, an A2A wake).
    """

    # Per-phase model keys populated lazily by the engine once
    # ``phase_model.resolve_phase_provider`` runs.
    model_keys: dict[str, str] = field(default_factory=dict)

    # Per-phase budgets assembled from the agent's session budget and
    # the turn engine's phase-split policy.
    phase_budgets: dict[str, PhaseBudget] = field(default_factory=dict)

    # Iteration counter for Plan -> Execute -> Review -> self_iterate
    # cycles within a single turn.
    iteration: int = 0

    # Offset applied to the iteration LABEL (events / dashboard grouping)
    # when this turn continues under an earlier turn's id — the detached
    # sandbox completion runs under the kick-off ``turn_id`` and must not
    # reuse iteration numbers the kick-off already emitted.  ``0`` (the
    # default) is the normal fresh-turn case (labels start at 1).  Only the
    # label is offset; the loop control and round cap are unchanged.
    start_iteration: int = 0

    # Set when this turn RESUMES a suspended Execute tool-loop:
    # an ``ExecuteResumeState`` carrying the persisted conversation +
    # the sandbox result. On the first iteration the engine skips Plan and
    # resumes Execute with it; ``None`` is the normal fresh-turn path. Typed
    # Any to avoid importing agent.execute into this dataclass header.
    resume_state: Any = None

    # Running totals across all phases of this turn.
    input_tokens: int = 0
    output_tokens: int = 0
    subagent_count: int = 0
    subagent_tokens: int = 0

    # Wall-clock start (UTC) — captured by TurnEngine so end-of-turn
    # events can report duration.  Typed as Any to avoid importing
    # ``datetime`` into this dataclass header; runtime callers pass
    # ``datetime.datetime`` objects.
    started_at: Any = None

    # Last Plan and Execute artifacts produced during this turn.
    # Stashed here (rather than returned up the call stack) so the
    # turn-completion publisher in the ``finally`` block can build
    # the ``TurnCompleted`` event without restructuring the engine.
    # Typed as Any to avoid circular imports with agent.plan /
    # agent.execute.
    last_plan: Any = None
    last_execute_result: Any = None

    # The ReviewOutcome of the round that ENDED the turn.  Stashed for
    # the same reason as the two above: ``_drive_phases`` returns only
    # ``(final_artifact, decision)``, and a ``done`` round appends no
    # ``IterationRecord`` — so without this the reviewer's own prose
    # about what landed never reaches the turn-completion frame, which
    # is exactly what the conversation ledger wants to record.
    last_review: Any = None

    # Prior turns of this conversation, rendered once at turn start and
    # frozen for the whole turn (the ``plan_prefetch`` rule: a block that
    # changed between iterations would invalidate the provider prompt
    # cache on every self_iterate loop).  Empty when the ledger is off,
    # when the trigger has no reproducible conversation, or on the first
    # turn of one.  See :mod:`crewlet.agent.conversation_log`.
    conversation_history: str = ""

    # Set once the dedicated first-turn onboarding pass has run this turn
    # (see agent.onboarding_phase). Suppresses the Plan-prompt onboarding hint
    # for the rest of the turn so onboarding can't also happen inside Plan and
    # re-spend the Plan round budget — the whole point of the dedicated pass.
    onboarding_ran: bool = False

    # Names of every tool called during the Plan phase(s) of this turn,
    # accumulated across self_iterate loops.  The Execute-phase tool
    # trace lands in ``last_execute_result.tool_executions``; Plan-phase
    # tool calls have no other home, so the turn-completion publisher
    # reads this to populate ``TurnCompleted.plan_tool_sequence``.  The
    # ReflectEngine needs it to know whether the LLM already invoked the
    # Plan-only ``reflect_and_persist`` builtin in-flight.
    plan_tool_sequence: list[str] = field(default_factory=list)

    # Full Plan-phase tool-call records (name, arguments, result,
    # success) for the CURRENT iteration only.  Mirrors what
    # ``ExecuteResult.tool_executions`` carries for the Execute phase
    # and shares its iteration-local scope.  Reset by ``TurnEngine``
    # at the top of each ``self_iterate`` loop so Review's prompt and
    # the engine's delivery-override check see only this iteration's
    # Plan calls — iter-1 Plan posts shouldn't satisfy iter-2's
    # delivery, and the reviewer shouldn't try to dedupe across
    # iterations from a flat list.  Cumulative cross-iteration names
    # live in ``plan_tool_sequence`` (ReflectEngine's view).
    plan_tool_executions: list[dict[str, Any]] = field(default_factory=list)

    # Frozen-at-turn-start prefetches for the Plan prompt.  Resolved
    # once on the first Plan iteration and reused on self_iterate
    # loops so the system-prompt prefix stays byte-identical and
    # provider prefix caching holds, even when ``task_description``
    # gains review notes between iterations.
    plan_prefetch: PlanPrefetch | None = None

    # Closed snapshots of every Plan -> Execute -> Review round that has
    # already finished this turn, appended just before each
    # ``self_iterate`` loops back.  Each phase rebuilds its LLM
    # conversation from scratch every iteration, so without this the
    # next Plan round starts blind and re-plans work (and external side
    # effects) that already happened.  Rendered into the Plan and
    # Execute user messages and into Review's evidence sections; see
    # :mod:`crewlet.agent.iteration_log`.
    iteration_history: list[IterationRecord] = field(default_factory=list)

    # Tool names MCP annotations positively mark read-only, among those
    # actually called this turn — accumulated across iterations by the
    # phase driver, which is the only frame where the role's MCP
    # wrappers are in scope to resolve them.  The conversation ledger is
    # built after that frame returns and marks its ``(read)`` lines from
    # here; the per-iteration ``IterationRecord`` carries its own copy.
    read_only_names: tuple[str, ...] = ()

    # The engine-detected guard breach that ended this turn, as
    # ``{"kind": ..., "detail": ...}``, or ``None``.  A stall abort and an
    # exhausted iteration cap end a turn by RETURNING ``decision="failed"``
    # rather than by raising, so this is the only way the turn-completed
    # record can name the cause — otherwise it reports a turn that failed
    # for no stated reason while the reason sits on a separate event the
    # LLM-history view never reads.
    guard_breach: dict[str, str] | None = None

    # Per-turn availability set built once by TurnEngine from the role's
    # ``mcp_env`` + registry ``check_fn`` calls.  Passed to each
    # ToolSurface factory as ``availability_filter`` so check_fns are
    # not re-invoked across phases.  ``None`` = filtering disabled
    # (test path; behaves as "everything available").
    availability_set: set[str] | None = None

    # Hard wall-clock cap (seconds) for the whole turn.  Set for
    # scheduler-originated turns so a runaway scheduled run can't
    # monopolise the runner; ``None`` = uncapped (the normal path).  The
    # TurnEngine wraps phase execution in ``asyncio.wait_for`` when set
    # and terminates the turn as ``failed`` on breach.
    deadline_seconds: float | None = None

    # Memo for ``trigger_interactions()`` — derived state, never set by
    # callers.
    _trigger_interactions: list[InboundInteraction] | None = field(
        default=None, init=False, repr=False
    )

    @property
    def stored_conversation_key(self) -> str:
        """:attr:`conversation_key`, or ``""`` when it identifies nothing.

        The raw key falls back to ``event:{id}`` for any trigger with no
        derivable conversation — a scheduled fire, a task assignment, an
        A2A wake.  That value is unique per emission, so no later
        message can reproduce it: storing it would key a row nothing can
        look up, and every read of one would miss.  Every consumer that
        PERSISTS or indexes the key (the session ledger, the episode
        column, the turn's telemetry tags) wants that filtered answer,
        so the rule lives here rather than in each of them.
        """
        key = self.conversation_key or ""
        return "" if key.startswith("event:") else key

    def trigger_interactions(self) -> list[InboundInteraction]:
        """Canonical interactions derived from ``trigger_event``, memoized.

        ``trigger_event`` is set once at construction and never
        reassigned, so the derivation (per-constituent metadata
        extraction + model construction — one per message on a
        coalesced trigger) runs once per turn instead of once per
        prefetch helper.  Plan-phase prefetches, ``refresh_memory``,
        and the turn-completion publisher all read through here.
        """
        if self._trigger_interactions is None:
            self._trigger_interactions = InboundInteraction.list_from_trigger_event(
                self.trigger_event
            )
        return self._trigger_interactions
