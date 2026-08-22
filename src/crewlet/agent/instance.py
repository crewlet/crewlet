"""AgentInstance — runtime state for a single agent."""

from __future__ import annotations

import asyncio
from enum import StrEnum
from uuid import UUID, uuid4

from crewlet._logging import get_logger
from crewlet.agent.definition import AgentDefinition

logger = get_logger("agent.instance")


class AgentState(StrEnum):
    CREATED = "created"
    IDLE = "idle"
    WORKING = "working"
    AWAITING_SANDBOX = "awaiting_sandbox"
    """Busy on a detached sandbox coding job.

    The kick-off turn started a background job and ended; the agent
    stays in this busy sub-state — its inbox paused — until the job
    completes and a fresh completion turn runs.  A *clarification* wait
    is NOT this state: there the agent goes back to ``IDLE`` (free)
    because a human reply can take minutes or days."""
    TERMINATED = "terminated"


class AgentInstance:
    """A single running agent instance.

    Each instance has a unique ID, a definition (from Role), and a
    current state.  Cross-turn context is NOT held here: it lives in
    the durable stores (``agent_diary``, ``episodes``, and the
    per-conversation ``conversation_sessions`` ledger), because a seat
    moves between nodes and anything process-local is lost with it.
    """

    def __init__(
        self,
        definition: AgentDefinition,
        email: str = "",
        handle: str = "",
        *,
        id: UUID | None = None,
    ) -> None:
        # ``id`` is injected by ``AgentPool`` from the persistent
        # ``agents`` table when a database is configured (see
        # ``crewlet.db.agents.resolve_agent_id``).  Falling back to a
        # fresh ``uuid4()`` keeps the in-memory / test path working
        # unchanged.
        self.id: UUID = id if id is not None else uuid4()
        # Read through the ``definition`` property below, never directly:
        # a live config apply reassigns this mid-turn.
        self._definition = definition
        self.email = email
        self.handle = handle
        self.state = AgentState.CREATED
        self.current_task_id: str | None = None
        self.input_tokens: int = 0
        self.output_tokens: int = 0
        # Process-local onboarding latch: the org chain hash this agent is
        # KNOWN to be onboarded for (confirmed marker read, or a successful
        # ``mark_onboarded`` this process).  The durable record is the
        # ``agent_onboarding_markers`` row; this latch is the in-memory
        # truth that stops a transient marker-read failure from re-running
        # a whole onboarding pass.  Keyed by chain hash so a live org
        # restructure (new hash) still re-fires onboarding by design.
        self.onboarded_chain_hash: str = ""
        # Single-flight guard for the onboarding pass.  Turns for one agent
        # are normally serialized by ``start_working``, but the sandbox
        # busy-state transitions (``await_sandbox`` / ``resume_from_sandbox``)
        # can free the agent while an earlier turn is still mid-flight, so two
        # turns CAN overlap — this lock makes the second turn wait at the
        # onboarding gate and then skip via the latch instead of running a
        # concurrent duplicate pass.
        self.onboarding_lock = asyncio.Lock()
        # State-change signal: every transition swaps + sets this event so a
        # turn parked on a busy agent (``TurnEngine.run_turn`` WAITS for the
        # agent instead of erroring — a busy agent is normal queuing, and
        # erroring would NAK the trigger toward the dead-letter
        # topic) wakes exactly when the state moves.  Waiters must capture
        # the CURRENT event and await it; the swap-then-set pattern makes
        # each transition a one-shot broadcast with no lost wakeups.
        self._state_changed: asyncio.Event = asyncio.Event()
        # The asyncio task currently running this agent's turn (set by
        # ``start_working``).  Lets the inbox handler detect the
        # memory-backend re-entrancy case — an event published to this
        # agent's own inbox FROM its running turn dispatches inline inside
        # the same task, and waiting there would deadlock on ourselves.
        self.working_task: asyncio.Task[object] | None = None
        logger.debug(
            "agent_created",
            agent_id=str(self.id),
            agent_name=self.role_name,
            email=self.email,
            handle=self.handle,
        )

    @property
    def definition(self) -> AgentDefinition:
        """This agent's definition — pinned for the duration of a turn.

        A live config apply reassigns the definition on the running
        instance, and the definition (role prompt, model, tool list) is
        read from roughly twenty places across a turn's phases.  Without
        the pin a turn could plan against one role and execute as
        another.  Outside a turn — and inside a turn belonging to a
        different seat — this is the live value, unchanged.

        See :mod:`crewlet.agent.turn_pin`.
        """
        from crewlet.agent.turn_pin import current_pin

        pin = current_pin()
        if pin is not None and pin.agent_id == str(self.id):
            return pin.definition
        return self._definition

    @definition.setter
    def definition(self, value: AgentDefinition) -> None:
        self._definition = value

    @property
    def live_definition(self) -> AgentDefinition:
        """The current definition, ignoring any pin.

        For the config-apply path itself, which has to diff against what
        is actually installed rather than what some turn is reading.
        """
        return self._definition

    @property
    def total_tokens(self) -> int:
        return self.input_tokens + self.output_tokens

    @property
    def role_name(self) -> str:
        return self.definition.role_name

    @property
    def id_str(self) -> str:
        return str(self.id)

    def _notify_state_change(self) -> None:
        """Wake every coroutine parked on this agent's state (one-shot).

        Swap-then-set: waiters captured the previous event, so setting it
        releases them all, and the fresh event is ready for the next wait.
        """
        released, self._state_changed = self._state_changed, asyncio.Event()
        released.set()

    async def wait_for_state_change(self) -> None:
        """Block until the next state transition of this agent.

        Callers re-check the condition they care about after waking (the
        transition may not be the one they wanted) — the standard pattern
        is ``while not agent.start_working(...): await
        agent.wait_for_state_change()``.
        """
        await self._state_changed.wait()

    def activate(self) -> None:
        """Transition from CREATED to IDLE."""
        if self.state == AgentState.CREATED:
            logger.info(
                "state_transition",
                agent_id=str(self.id),
                agent_name=self.role_name,
                from_state=AgentState.CREATED,
                to_state=AgentState.IDLE,
            )
            self.state = AgentState.IDLE
            self._notify_state_change()

    def start_working(self, task_id: str) -> bool:
        """Transition to WORKING state. Returns True if transition succeeded."""
        if self.state == AgentState.IDLE:
            logger.info(
                "state_transition",
                agent_id=str(self.id),
                agent_name=self.role_name,
                from_state=AgentState.IDLE,
                to_state=AgentState.WORKING,
                task_id=task_id,
            )
            self.state = AgentState.WORKING
            self.current_task_id = task_id
            try:
                self.working_task = asyncio.current_task()
            except RuntimeError:  # no running loop (sync test construction)
                self.working_task = None
            self._notify_state_change()
            return True
        logger.debug(
            "start_working_failed",
            agent_id=str(self.id),
            agent_name=self.role_name,
            current_state=self.state,
            expected_state=AgentState.IDLE,
        )
        return False

    def finish_working(self) -> bool:
        """Return to IDLE after completing work.

        Returns True if transition succeeded.
        """
        if self.state == AgentState.WORKING:
            logger.info(
                "state_transition",
                agent_id=str(self.id),
                agent_name=self.role_name,
                from_state=AgentState.WORKING,
                to_state=AgentState.IDLE,
            )
            self.state = AgentState.IDLE
            self.current_task_id = None
            self.working_task = None
            self._notify_state_change()
            return True
        logger.debug(
            "finish_working_failed",
            agent_id=str(self.id),
            agent_name=self.role_name,
            current_state=self.state,
            expected_state=AgentState.WORKING,
        )
        return False

    def await_sandbox(self, task_id: str = "") -> bool:
        """Transition to AWAITING_SANDBOX (detached kick-off / recovery).

        Called when a turn's Execute kicked off a detached sandbox job.
        Accepts ``WORKING`` (the kick-off turn is still in its finally) OR
        ``IDLE`` (the turn already finished, or this is a post-restart
        recovery re-attach) — both converge here because the kick-off
        event and the turn's ``finish_working`` race with no fixed order.
        Keeps / sets ``current_task_id`` so the busy agent shows its job.
        Returns True if the transition happened.
        """
        if self.state in (AgentState.WORKING, AgentState.IDLE):
            logger.info(
                "state_transition",
                agent_id=str(self.id),
                agent_name=self.role_name,
                from_state=self.state,
                to_state=AgentState.AWAITING_SANDBOX,
                task_id=task_id or self.current_task_id or "",
            )
            self.state = AgentState.AWAITING_SANDBOX
            if task_id:
                self.current_task_id = task_id
            self.working_task = None
            self._notify_state_change()
            return True
        logger.debug(
            "await_sandbox_failed",
            agent_id=str(self.id),
            agent_name=self.role_name,
            current_state=self.state,
        )
        return False

    def resume_from_sandbox(self) -> bool:
        """Transition AWAITING_SANDBOX -> IDLE (job completed / freed).

        The completion turn then re-enters via ``start_working``.  Also
        used to free the agent when a kick-off ended in a clarification
        wait.  Returns True if the transition succeeded.
        """
        if self.state == AgentState.AWAITING_SANDBOX:
            logger.info(
                "state_transition",
                agent_id=str(self.id),
                agent_name=self.role_name,
                from_state=AgentState.AWAITING_SANDBOX,
                to_state=AgentState.IDLE,
            )
            self.state = AgentState.IDLE
            self.current_task_id = None
            self._notify_state_change()
            return True
        logger.debug(
            "resume_from_sandbox_failed",
            agent_id=str(self.id),
            agent_name=self.role_name,
            current_state=self.state,
            expected_state=AgentState.AWAITING_SANDBOX,
        )
        return False

    def terminate(self) -> None:
        """Terminate this agent."""
        logger.info(
            "agent_terminated",
            agent_id=str(self.id),
            agent_name=self.role_name,
            previous_state=self.state,
        )
        self.state = AgentState.TERMINATED
        self.current_task_id = None
        self.working_task = None
        # Wake any turn parked on this agent so it can observe TERMINATED
        # and fail fast instead of waiting forever.
        self._notify_state_change()

    @property
    def is_available(self) -> bool:
        """Whether this agent can accept new work."""
        return self.state == AgentState.IDLE
