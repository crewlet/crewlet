"""Durable state for a detached sandbox Execute job.

A sandbox-backed Execute does not finish in its kick-off turn: the turn
starts a background coding job and ends; the job's completion (or a
human's answer to a mid-run clarification) arrives later as a *fresh*
turn that rebuilds everything it needs from a persisted
``pending_sandbox_run`` row. This module owns that row: the
:class:`PendingSandboxRun` model, the :class:`PendingSandboxRunStore`
protocol, a Postgres-backed implementation, and an in-memory one for
tests / no-DB runs.

The state machine and the at-most-once tail guard
(``running``/``awaiting_clarification``/``reseed`` → ``resumed`` via an
atomic conditional flip) are documented in
``db/migrations/012_pending_sandbox_run.sql``.

The row is also the engine's record of the run's **sandbox**:
``sandbox_id`` non-empty means a box exists, ``paused_at`` non-NULL means
that box is currently paused. Those two fields are what let the pause
reaper reclaim a snapshot nothing else would ever free (see
``db/migrations/015_pending_sandbox_paused_at.sql``).
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any, Protocol

from crewlet._logging import get_logger
from crewlet.db._jsonb import decode_jsonb_dict, decode_jsonb_str_list
from crewlet.db.client import Database

logger = get_logger("sandbox.pending_store")

# Statuses that mean "the tail has not run yet" — the busy-running job, a
# parked clarification, and a parked clarification whose paused box was
# reaped past its ``pause_ttl`` are each claimable for resume exactly once.
# ``reseed`` belongs here because reaping the box does not end the run: the
# answer can still arrive, and the work re-seeds from the pushed branch.
_CLAIMABLE = ("running", "awaiting_clarification", "reseed")

# Statuses whose run is still waiting for a person's answer on its
# conversation — matched back by ``conversation_key``.
_AWAITING = ("awaiting_clarification", "reseed")

# Statuses that still own engine-side state (an agent, a box, or a pending
# tail) and so must survive a restart.  ``resumed`` is here so boot recovery
# can SEE a tail that died mid-flight with the previous engine: nothing else
# would ever look at that row again, and its paused box would leak forever.
_ACTIVE = ("running", "awaiting_clarification", "reseed", "resumed")


@dataclass
class PendingSandboxRun:
    """One detached sandbox job's durable state (keyed by kick-off turn)."""

    turn_id: str
    agent_handle: str
    agent_id: str = ""
    role: str = ""
    sandbox_id: str = ""
    coding_agent: str = ""
    command_id: str = ""
    status: str = "running"
    plan: dict[str, Any] = field(default_factory=dict)
    task_description: str = ""
    success_criteria: list[str] = field(default_factory=list)
    conversation_key: str = ""
    notification_metadata: dict[str, Any] = field(default_factory=dict)
    branch: str = ""
    session_id: str = ""
    question: str = ""
    audience: str = ""
    trace_id: str = ""
    span_id: str = ""
    budget_remaining: int = 0
    delegation_depth: int = 0
    delegation_chain: list[str] = field(default_factory=list)
    owner: str = ""
    """Process incarnation of the node that owns this run's seat.

    A run outlives the process that started it — the box is real and
    billed, the row is the durable state — and its seat can move between
    nodes while the coding agent is still working. Empty means unclaimed:
    what an in-flight run looks like the instant before its seat's new
    owner recovers it."""
    owner_epoch: int = 0
    """The seat lease's epoch at the moment of the claim: the fencing
    token. Every mutation on a live run carries ``WHERE owner_epoch =
    $mine``, so a node whose lease moved cannot write even if it has not
    noticed yet — the ownership check is an optimization, the fence is
    the guarantee."""
    pause_ttl_seconds: float = 0.0
    paused_at: datetime | None = None
    """When this run's sandbox was paused, or ``None`` if it isn't.

    Together with ``sandbox_id`` this is the engine's record of the box:
    ``sandbox_id`` non-empty means a box exists, ``paused_at`` set means it
    is currently paused. The pause reaper
    (:class:`~crewlet.sandbox.waiter.SandboxWaiter`) compares it against
    ``pause_ttl_seconds`` — E2B holds a paused box forever and bills for the
    snapshot, so nothing else would ever reclaim it."""
    # Suspended Execute-loop state: the serialized
    # conversation + surface bookkeeping needed to RESUME the tool-loop when
    # the sandbox run completes. Empty only when a crash landed between
    # launch and the suspend persist (the coordinator then fails the run
    # instead of resuming). Keys:
    #   messages: list[dict]        -- [Message.model_dump(...)], incl. the
    #                                  assistant turn with the dangling tool_use
    #   pending_tool_call_id: str   -- the run_sandbox call awaiting its result
    #   pending_tool_name: str
    #   active_tool_names: list[str] -- surface.names at suspend (replayed)
    #   loaded_skill_keys: list[str] -- skill-guard loaded set (replayed)
    #   iteration: int               -- turn.iteration at suspend (label cont.)
    #   input_tokens / output_tokens: int -- Execute partials (observability)
    execute_state: dict[str, Any] = field(default_factory=dict)
    # Transient (never persisted): the status this row held immediately
    # BEFORE ``claim_for_resume`` flipped it to ``resumed``.  A failed
    # resume dispatch reverts the claim to exactly this status so the
    # NAK'd trigger can re-claim on redelivery — inferring it from other
    # fields is unsound (a reused run keeps its old ``question``).
    claimed_from: str = ""


def _row_to_run(row: dict[str, Any]) -> PendingSandboxRun:
    return PendingSandboxRun(
        turn_id=row["turn_id"],
        agent_handle=row.get("agent_handle", ""),
        agent_id=row.get("agent_id", ""),
        role=row.get("role", ""),
        sandbox_id=row.get("sandbox_id", ""),
        coding_agent=row.get("coding_agent", ""),
        command_id=row.get("command_id", ""),
        status=row.get("status", "running"),
        owner=row.get("owner") or "",
        owner_epoch=int(row.get("owner_epoch") or 0),
        plan=decode_jsonb_dict(row.get("plan")),
        task_description=row.get("task_description", ""),
        success_criteria=decode_jsonb_str_list(row.get("success_criteria")),
        conversation_key=row.get("conversation_key", ""),
        notification_metadata=decode_jsonb_dict(row.get("notification_metadata")),
        branch=row.get("branch", ""),
        session_id=row.get("session_id", ""),
        question=row.get("question", ""),
        audience=row.get("audience", ""),
        trace_id=row.get("trace_id", ""),
        span_id=row.get("span_id", ""),
        budget_remaining=int(row.get("budget_remaining", 0) or 0),
        delegation_depth=int(row.get("delegation_depth", 0) or 0),
        delegation_chain=decode_jsonb_str_list(row.get("delegation_chain")),
        pause_ttl_seconds=float(row.get("pause_ttl_seconds", 0.0) or 0.0),
        paused_at=row.get("paused_at"),
        execute_state=decode_jsonb_dict(row.get("execute_state")),
    )


class PendingSandboxRunStore(Protocol):
    """Persistence surface for detached sandbox runs."""

    async def create(self, run: PendingSandboxRun) -> None:
        """Persist a new ``running`` row (idempotent on ``turn_id``)."""
        ...

    async def get(self, turn_id: str) -> PendingSandboxRun | None:
        """Load a run by kick-off turn id, or ``None``."""
        ...

    async def claim_for_resume(self, turn_id: str) -> PendingSandboxRun | None:
        """Atomically flip ``running``/``awaiting_clarification`` → ``resumed``.

        Returns the row iff THIS call won the flip (the at-most-once tail
        guard); ``None`` when already claimed / terminal / missing. The
        returned row carries the POST-flip status (``resumed``) — callers
        tell a completion from a clarification answer by whether the row's
        ``question`` is set, and a failed resume dispatch reverts the claim
        by the same signal (``running`` vs ``awaiting_clarification``).
        """
        ...

    async def mark_awaiting_clarification(
        self,
        turn_id: str,
        *,
        question: str,
        audience: str,
        conversation_key: str,
        branch: str = "",
        session_id: str = "",
    ) -> bool:
        """Flip ``running`` → ``awaiting_clarification`` (agent goes free)."""
        ...

    async def claim_ownership(self, turn_id: str, *, owner: str, epoch: int) -> bool:
        """Take a run for this node at ``epoch``; monotonic in the epoch."""
        ...

    async def set_status(
        self,
        turn_id: str,
        status: str,
        *,
        epoch: int | None = None,
        expect: str | None = None,
    ) -> bool:
        """Set a terminal / re-seed status (done | failed | reseed).

        ``epoch`` fences the write to the node that owns the run's seat;
        ``expect`` makes it conditional on the status the caller read.
        Both return ``False`` when the precondition refused.
        """
        ...

    async def attach_sandbox(
        self,
        turn_id: str,
        *,
        sandbox_id: str,
        command_id: str,
        session_id: str = "",
    ) -> None:
        """Point an existing row at a live box and flip it back to ``running``.

        The launch side of the box invariant, used whenever a run that
        already has a row starts another detached job: a follow-up
        ``run_sandbox`` reusing the paused box, **or** a re-seeded run whose
        box was reaped and which therefore just provisioned a fresh one. The
        row must pick up the new ``sandbox_id`` / ``command_id`` — a stale
        pair leaves the waiter polling a box that isn't running this job, so
        the completion never fires. Clears ``paused_at``: the box is live
        again.
        """
        ...

    async def mark_box_paused(self, turn_id: str) -> None:
        """Record that this run's sandbox is now paused (starts its TTL)."""
        ...

    async def release_box(self, turn_id: str) -> None:
        """Record that this run no longer has a sandbox (torn down / reaped).

        Clears ``sandbox_id`` and ``paused_at`` without touching ``status``,
        so the two concerns stay orthogonal: the reaper releases the box and
        *then* decides the run is ``reseed``, teardown releases it and *then*
        marks the run ``done``. Clearing ``sandbox_id`` is what makes the
        next ``run_sandbox`` provision a fresh box rather than trying to
        reattach to a dead id.
        """
        ...

    async def save_execute_state(self, turn_id: str, state: dict[str, Any]) -> None:
        """Persist the suspended Execute-loop state for later resume."""
        ...

    async def list_active(self) -> list[PendingSandboxRun]:
        """Rows still awaiting their tail (for the poll waiter)."""
        ...

    async def list_active_for_seat(self, agent_handle: str) -> list[PendingSandboxRun]:
        """Active rows for ONE seat, for its owner's recovery pass.

        Recovery is per-seat rather than fleet-wide: a node may only
        touch runs for seats it holds. A boot-time scan of every active
        row would have each node re-pausing, re-parking and reaping runs
        belonging to seats its peers own.
        """
        ...

    async def find_awaiting_by_conversation(
        self, conversation_key: str
    ) -> PendingSandboxRun | None:
        """The run awaiting an answer on ``conversation_key``.

        Matches both a run whose box is still paused
        (``awaiting_clarification``) and one whose box was already reaped
        past its pause TTL (``reseed``) — the answer resumes the work either
        way; only the resume text differs.
        """
        ...

    async def delete(self, turn_id: str) -> None:
        """Remove a run (after its tail fully completes)."""
        ...


class PostgresPendingSandboxRunStore:
    """Postgres-backed :class:`PendingSandboxRunStore`."""

    def __init__(self, db: Database) -> None:
        self._db = db

    async def create(self, run: PendingSandboxRun) -> None:
        await self._db.execute(
            """
            INSERT INTO pending_sandbox_run (
                turn_id, agent_handle, agent_id, role, sandbox_id,
                coding_agent, command_id, status, plan, task_description,
                success_criteria, conversation_key, notification_metadata,
                branch, session_id, question, audience, trace_id, span_id,
                budget_remaining, delegation_depth, delegation_chain,
                pause_ttl_seconds, execute_state
            )
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
                    $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)
            ON CONFLICT (turn_id) DO NOTHING
            """,
            run.turn_id,
            run.agent_handle,
            run.agent_id,
            run.role,
            run.sandbox_id,
            run.coding_agent,
            run.command_id,
            run.status,
            json.dumps(run.plan),
            run.task_description,
            json.dumps(run.success_criteria),
            run.conversation_key,
            json.dumps(run.notification_metadata),
            run.branch,
            run.session_id,
            run.question,
            run.audience,
            run.trace_id,
            run.span_id,
            int(run.budget_remaining),
            int(run.delegation_depth),
            json.dumps(run.delegation_chain),
            float(run.pause_ttl_seconds),
            json.dumps(run.execute_state),
        )

    async def get(self, turn_id: str) -> PendingSandboxRun | None:
        row = await self._db.fetchrow(
            "SELECT * FROM pending_sandbox_run WHERE turn_id = $1", turn_id
        )
        return _row_to_run(row) if row else None

    async def claim_for_resume(self, turn_id: str) -> PendingSandboxRun | None:
        # Atomic at-most-once flip: only one caller's UPDATE matches the
        # claimable status, so only one gets a RETURNING row. The CTE
        # snapshots the pre-flip status (RETURNING alone yields post-UPDATE
        # values) so a failed resume dispatch can revert the claim exactly.
        row = await self._db.fetchrow(
            """
            WITH prev AS (
                SELECT turn_id, status AS prev_status
                FROM pending_sandbox_run
                WHERE turn_id = $1
                  AND status IN ('running', 'awaiting_clarification', 'reseed')
                FOR UPDATE
            )
            UPDATE pending_sandbox_run p
            SET status = 'resumed', updated_at = now()
            FROM prev
            WHERE p.turn_id = prev.turn_id
            RETURNING p.*, prev.prev_status
            """,
            turn_id,
        )
        if not row:
            return None
        run = _row_to_run(row)
        run.claimed_from = str(row.get("prev_status", "") or "")
        return run

    async def mark_awaiting_clarification(
        self,
        turn_id: str,
        *,
        question: str,
        audience: str,
        conversation_key: str,
        branch: str = "",
        session_id: str = "",
    ) -> bool:
        # Parkable from ``running`` (the job asked mid-flight) OR
        # ``resumed`` (the ``ask`` was discovered when the completion turn
        # collected the result — the detached path).
        row = await self._db.fetchrow(
            """
            UPDATE pending_sandbox_run
            SET status = 'awaiting_clarification', question = $2,
                audience = $3, conversation_key = $4,
                branch = COALESCE(NULLIF($5, ''), branch),
                session_id = COALESCE(NULLIF($6, ''), session_id),
                updated_at = now()
            WHERE turn_id = $1 AND status IN ('running', 'resumed')
            RETURNING turn_id
            """,
            turn_id,
            question,
            audience,
            conversation_key,
            branch,
            session_id,
        )
        return row is not None

    async def claim_ownership(self, turn_id: str, *, owner: str, epoch: int) -> bool:
        """Take a run for this node at ``epoch``. Monotonic in the epoch.

        Refuses to move a run BACKWARDS: a node whose stale view still
        names it the owner cannot reclaim a run whose seat has since gone
        to a peer at a higher epoch. Adopting an unclaimed row (``owner``
        NULL, from before ownership existed) is allowed and is how
        existing runs migrate.
        """
        row = await self._db.fetchrow(
            """
            UPDATE pending_sandbox_run
            SET owner = $2, owner_epoch = $3, updated_at = now()
            WHERE turn_id = $1
              AND (owner_epoch IS NULL OR owner_epoch <= $3)
            RETURNING turn_id
            """,
            turn_id,
            owner,
            int(epoch),
        )
        return row is not None

    async def set_status(
        self,
        turn_id: str,
        status: str,
        *,
        epoch: int | None = None,
        expect: str | None = None,
    ) -> bool:
        """Move a run to ``status``; ``False`` when a precondition refused.

        Two independent preconditions, both optional, both compare-and-set:

        ``epoch`` is the caller's seat-lease epoch. It makes the write
        conditional on this node still owning the run, which is what
        stops a node that lost the seat from settling a run its
        successor is already resuming.

        ``expect`` is the status the caller believes the run is in. The
        pause reaper needs it: it decides to expire a paused box from a
        row it read seconds ago, and by then the clarification answer
        that un-pauses that run may already have arrived. Without the
        precondition it would stamp ``reseed`` over a run already back
        to ``running``.
        """
        if epoch is None and expect is None:
            await self._db.execute(
                "UPDATE pending_sandbox_run SET status = $2, updated_at = now() "
                "WHERE turn_id = $1",
                turn_id,
                status,
            )
            return True
        row = await self._db.fetchrow(
            """
            UPDATE pending_sandbox_run
            SET status = $2, updated_at = now()
            WHERE turn_id = $1
              AND ($3::bigint IS NULL OR owner_epoch IS NULL OR owner_epoch = $3)
              AND ($4::text IS NULL OR status = $4)
            RETURNING turn_id
            """,
            turn_id,
            status,
            None if epoch is None else int(epoch),
            expect,
        )
        if row is None:
            logger.warning(
                "pending_run_write_refused",
                turn_id=turn_id,
                status=status,
                epoch=epoch,
                expect=expect,
                hint=(
                    "the run moved under this caller — its seat went to "
                    "another node, or its status changed — so the write "
                    "was refused rather than racing whoever owns it now"
                ),
            )
        return row is not None

    async def attach_sandbox(
        self,
        turn_id: str,
        *,
        sandbox_id: str,
        command_id: str,
        session_id: str = "",
    ) -> None:
        await self._db.execute(
            """
            UPDATE pending_sandbox_run
            SET sandbox_id = $2, command_id = $3,
                session_id = COALESCE(NULLIF($4, ''), session_id),
                status = 'running', paused_at = NULL, updated_at = now()
            WHERE turn_id = $1
            """,
            turn_id,
            sandbox_id,
            command_id,
            session_id,
        )

    async def mark_box_paused(self, turn_id: str) -> None:
        await self._db.execute(
            "UPDATE pending_sandbox_run SET paused_at = now(), updated_at = now() "
            "WHERE turn_id = $1",
            turn_id,
        )

    async def release_box(self, turn_id: str) -> None:
        await self._db.execute(
            "UPDATE pending_sandbox_run "
            "SET sandbox_id = '', paused_at = NULL, updated_at = now() "
            "WHERE turn_id = $1",
            turn_id,
        )

    async def save_execute_state(self, turn_id: str, state: dict[str, Any]) -> None:
        await self._db.execute(
            "UPDATE pending_sandbox_run SET execute_state = $2, updated_at = now() "
            "WHERE turn_id = $1",
            turn_id,
            json.dumps(state),
        )

    async def list_active(self) -> list[PendingSandboxRun]:
        rows = await self._db.execute(
            "SELECT * FROM pending_sandbox_run "
            "WHERE status IN ('running', 'awaiting_clarification', 'reseed', "
            "'resumed') "
            "ORDER BY created_at ASC"
        )
        return [_row_to_run(r) for r in rows]

    async def list_active_for_seat(self, agent_handle: str) -> list[PendingSandboxRun]:
        rows = await self._db.execute(
            "SELECT * FROM pending_sandbox_run "
            "WHERE agent_handle = $1 "
            "AND status IN ('running', 'awaiting_clarification', 'reseed', "
            "'resumed') "
            "ORDER BY created_at ASC",
            agent_handle,
        )
        return [_row_to_run(r) for r in rows]

    async def find_awaiting_by_conversation(
        self, conversation_key: str
    ) -> PendingSandboxRun | None:
        if not conversation_key:
            return None
        row = await self._db.fetchrow(
            "SELECT * FROM pending_sandbox_run "
            "WHERE conversation_key = $1 "
            "AND status IN ('awaiting_clarification', 'reseed') "
            "ORDER BY updated_at DESC LIMIT 1",
            conversation_key,
        )
        return _row_to_run(row) if row else None

    async def delete(self, turn_id: str) -> None:
        await self._db.execute(
            "DELETE FROM pending_sandbox_run WHERE turn_id = $1", turn_id
        )


class MemoryPendingSandboxRunStore:
    """In-memory store for tests and single-process / no-DB runs.

    Process-local, so it loses the across-restart durability the
    Postgres store gives; the engine wires the Postgres store whenever a
    database is configured.
    """

    def __init__(self) -> None:
        self._runs: dict[str, PendingSandboxRun] = {}

    async def create(self, run: PendingSandboxRun) -> None:
        self._runs.setdefault(run.turn_id, run)

    async def get(self, turn_id: str) -> PendingSandboxRun | None:
        return self._runs.get(turn_id)

    async def claim_for_resume(self, turn_id: str) -> PendingSandboxRun | None:
        run = self._runs.get(turn_id)
        if run is None or run.status not in _CLAIMABLE:
            return None
        run.claimed_from = run.status
        run.status = "resumed"
        return run

    async def mark_awaiting_clarification(
        self,
        turn_id: str,
        *,
        question: str,
        audience: str,
        conversation_key: str,
        branch: str = "",
        session_id: str = "",
    ) -> bool:
        run = self._runs.get(turn_id)
        if run is None or run.status not in ("running", "resumed"):
            return False
        run.status = "awaiting_clarification"
        run.question = question
        run.audience = audience
        run.conversation_key = conversation_key
        if branch:
            run.branch = branch
        if session_id:
            run.session_id = session_id
        return True

    async def claim_ownership(self, turn_id: str, *, owner: str, epoch: int) -> bool:
        run = self._runs.get(turn_id)
        if run is None or run.owner_epoch > int(epoch):
            return False
        run.owner = owner
        run.owner_epoch = int(epoch)
        return True

    async def set_status(
        self,
        turn_id: str,
        status: str,
        *,
        epoch: int | None = None,
        expect: str | None = None,
    ) -> bool:
        run = self._runs.get(turn_id)
        if run is None:
            return False
        if epoch is not None and run.owner_epoch and run.owner_epoch != int(epoch):
            logger.warning(
                "pending_run_write_refused",
                turn_id=turn_id,
                status=status,
                epoch=epoch,
                owner_epoch=run.owner_epoch,
            )
            return False
        if expect is not None and run.status != expect:
            logger.warning(
                "pending_run_write_refused",
                turn_id=turn_id,
                status=status,
                expect=expect,
                actual=run.status,
            )
            return False
        run.status = status
        return True

    async def attach_sandbox(
        self,
        turn_id: str,
        *,
        sandbox_id: str,
        command_id: str,
        session_id: str = "",
    ) -> None:
        run = self._runs.get(turn_id)
        if run is None:
            return
        run.sandbox_id = sandbox_id
        run.command_id = command_id
        if session_id:
            run.session_id = session_id
        run.status = "running"
        run.paused_at = None

    async def mark_box_paused(self, turn_id: str) -> None:
        run = self._runs.get(turn_id)
        if run is not None:
            run.paused_at = datetime.now(UTC)

    async def release_box(self, turn_id: str) -> None:
        run = self._runs.get(turn_id)
        if run is not None:
            run.sandbox_id = ""
            run.paused_at = None

    async def save_execute_state(self, turn_id: str, state: dict[str, Any]) -> None:
        run = self._runs.get(turn_id)
        if run is not None:
            run.execute_state = dict(state)

    async def list_active(self) -> list[PendingSandboxRun]:
        return [r for r in self._runs.values() if r.status in _ACTIVE]

    async def list_active_for_seat(self, agent_handle: str) -> list[PendingSandboxRun]:
        return [
            r
            for r in self._runs.values()
            if r.status in _ACTIVE and r.agent_handle == agent_handle
        ]

    async def find_awaiting_by_conversation(
        self, conversation_key: str
    ) -> PendingSandboxRun | None:
        if not conversation_key:
            return None
        for run in self._runs.values():
            if run.status in _AWAITING and run.conversation_key == conversation_key:
                return run
        return None

    async def delete(self, turn_id: str) -> None:
        self._runs.pop(turn_id, None)


__all__ = [
    "MemoryPendingSandboxRunStore",
    "PendingSandboxRun",
    "PendingSandboxRunStore",
    "PostgresPendingSandboxRunStore",
]
