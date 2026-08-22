"""The conversation ledger: what this seat already did in THIS thread.

A notification turn is built from a blank ``TurnContext``.
Everything that spans turns today is either agent-scoped and
similarity-addressed (``episodes``, ``agent_diary``) or content-free
(``chat_thread_follows``, ``turn_completions``) — so the second comment
on a Jira issue starts a turn that cannot see what the first one
answered.  The engine's standing answer is re-recon: re-read the thread
with the role's chat tools, every turn, in Plan and again in Execute.

This table is the other half.  It records, once per completed turn, what
the seat *did* in one conversation — the plan it made and why, the write
calls that fired, the reply it sent, the reviewer's verdict — and the
turn engine renders the recent entries back into the next turn's user
message.  Episodes answer "have I done something like this before";
this answers "what did I already say here".

Three properties are load-bearing:

- **Keyed on the conversation, deduped on the work.**  The row key is
  ``conversation_key`` (:func:`crewlet.notifications.coalesce.conversation_key`
  — the Slack thread / Jira issue / GitHub PR identity that already
  partitions every seat inbox), and the dedupe key is the ``work_key``
  the completion ledger uses.  Never ``turn_id``: two nodes completing
  one trigger mint two turn ids, so a turn-keyed row would RECORD the
  duplicate instead of collapsing it.
- **Bounded at write time.**  A busy DM channel keys on the channel
  itself rather than a thread, so an unbounded ledger would grow without
  end; ``append`` trims to the newest ``max_entries`` in the same
  statement that inserts.
- **Both directions fail open.**  A read that cannot answer yields no
  history, which is exactly the pre-ledger prompt; a write that fails
  loses one entry of context.  Neither may stop a turn — this is an
  improvement on a blank context, not a gate in front of one.

Rows are plain JSONB payloads here; the entry's *shape* is owned by
:mod:`crewlet.agent.conversation_log`, the same way
``pending_sandbox_run.execute_state`` is a blob here and a conversation
in :mod:`crewlet.agent.execute`.  That keeps the database layer free of
agent imports.
"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any, Protocol

from crewlet._logging import get_logger
from crewlet.db._jsonb import decode_jsonb_dict

logger = get_logger("db.conversation_sessions")

# How long a conversation is remembered, by default.
#
# Thirty days, matching the event store's own ``EVENT_HISTORY_DAYS``: the
# engine's memory of a conversation and the telemetry that shows what it
# did there then forget on the same horizon, so an operator reading the
# dashboard never finds a rendered history whose turns have no records
# behind them.
#
# This is the one swept table whose retention is an operator's call
# rather than a mechanical floor — a company running quarter-long tickets
# has a real reason to keep more — so it is exposed as
# ``turn_engine.conversation_session.retention_days`` and this constant is
# only the default.
CONVERSATION_SESSION_RETENTION_SECONDS = 30 * 24 * 3600.0

# Entries kept per conversation, by default.
#
# The read path injects far fewer (``injected_max_entries``); the stored
# depth is larger so the dashboard can show a conversation's history
# beyond what any single prompt carried.  Twenty mirrors the inbox
# coalescer's ``max_batch``: the same order of magnitude of traffic that
# counts as one burst of a conversation.
CONVERSATION_SESSION_MAX_ENTRIES = 20


@dataclass(frozen=True, slots=True)
class SessionRow:
    """One recorded turn of one conversation."""

    turn_id: str = ""
    work_key: str = ""
    created_at: datetime | None = None
    entry: dict[str, Any] = field(default_factory=dict)


class ConversationSessionStore(Protocol):
    """Per-(seat, conversation) record of completed turns."""

    async def append(
        self,
        *,
        agent_handle: str,
        conversation_key: str,
        work_key: str,
        turn_id: str,
        entry: dict[str, Any],
        max_entries: int = CONVERSATION_SESSION_MAX_ENTRIES,
    ) -> bool:
        """Record one completed turn.  ``True`` when a row was written.

        First writer wins on ``work_key``: a peer recording a turn its
        twin already recorded is a no-op, so a redelivered trigger cannot
        double an entry.
        """
        ...

    async def recent(
        self,
        *,
        agent_handle: str,
        conversation_key: str,
        limit: int,
    ) -> list[SessionRow]:
        """The newest ``limit`` entries, oldest-first for rendering.

        Returns ``[]`` when the store cannot answer — see the module
        docstring: no history is the pre-ledger prompt, which is safe.
        """
        ...

    async def conversations(
        self, *, agent_handle: str, limit: int = 50
    ) -> list[dict[str, Any]]:
        """Conversations this seat has entries for, most recent first."""
        ...

    async def purge(self, older_than_seconds: float) -> int: ...


class PostgresConversationSessionStore:
    """PostgreSQL-backed :class:`ConversationSessionStore`."""

    def __init__(self, db: Any) -> None:
        self._db = db

    async def append(
        self,
        *,
        agent_handle: str,
        conversation_key: str,
        work_key: str,
        turn_id: str,
        entry: dict[str, Any],
        max_entries: int = CONVERSATION_SESSION_MAX_ENTRIES,
    ) -> bool:
        if not agent_handle or not conversation_key or not work_key:
            return False
        try:
            rows = await self._db.execute(
                """
                INSERT INTO conversation_sessions
                    (agent_handle, conversation_key, work_key, turn_id, entry)
                VALUES ($1, $2, $3, $4, $5::jsonb)
                ON CONFLICT (agent_handle, conversation_key, work_key)
                    DO NOTHING
                RETURNING work_key
                """,
                agent_handle,
                conversation_key,
                work_key,
                turn_id,
                json.dumps(entry),
            )
        except Exception:
            # Fail OPEN, and loudly. The turn is already complete and its
            # side effects shipped; all that is lost is one entry of
            # context for the NEXT turn, which then starts from the same
            # blank slate every turn started from before this table.
            logger.exception(
                "conversation_session_append_failed",
                agent=agent_handle,
                conversation=conversation_key,
            )
            return False
        if not rows:
            return False
        await self._trim(agent_handle, conversation_key, max_entries)
        return True

    async def _trim(self, agent_handle: str, key: str, max_entries: int) -> None:
        """Drop all but the newest ``max_entries`` rows of one conversation.

        Trimming on write rather than on a retention tick is what bounds
        a chat DM, whose conversation key is the whole channel rather
        than a thread and so never stops receiving entries.
        """
        keep = max(1, int(max_entries))
        try:
            await self._db.execute(
                """
                DELETE FROM conversation_sessions
                WHERE ctid IN (
                    SELECT ctid FROM conversation_sessions
                    WHERE agent_handle = $1 AND conversation_key = $2
                    ORDER BY created_at DESC
                    OFFSET $3
                )
                """,
                agent_handle,
                key,
                keep,
            )
        except Exception:
            # A ledger that grew one row past its cap is strictly better
            # than an exception on a completed turn's tail.
            logger.warning(
                "conversation_session_trim_failed",
                agent=agent_handle,
                conversation=key,
            )

    async def recent(
        self,
        *,
        agent_handle: str,
        conversation_key: str,
        limit: int,
    ) -> list[SessionRow]:
        if not agent_handle or not conversation_key or limit <= 0:
            return []
        try:
            rows = await self._db.execute(
                """
                SELECT turn_id, work_key, created_at, entry
                FROM conversation_sessions
                WHERE agent_handle = $1 AND conversation_key = $2
                ORDER BY created_at DESC
                LIMIT $3
                """,
                agent_handle,
                conversation_key,
                int(limit),
            )
        except Exception:
            # Fail OPEN: no history renders no block, which is the prompt
            # every turn had before this table existed.
            logger.warning(
                "conversation_sessions_unavailable",
                agent=agent_handle,
                conversation=conversation_key,
            )
            return []
        return [_row_to_session_row(row) for row in reversed(rows)]

    async def conversations(
        self, *, agent_handle: str, limit: int = 50
    ) -> list[dict[str, Any]]:
        if not agent_handle:
            return []
        try:
            rows = await self._db.execute(
                """
                SELECT conversation_key,
                       count(*)        AS entries,
                       max(created_at) AS last_at
                FROM conversation_sessions
                WHERE agent_handle = $1
                GROUP BY conversation_key
                ORDER BY last_at DESC
                LIMIT $2
                """,
                agent_handle,
                int(limit),
            )
        except Exception:
            logger.warning("conversation_sessions_unavailable", agent=agent_handle)
            return []
        return [
            {
                "conversation_key": str(row.get("conversation_key", "")),
                "entries": int(row.get("entries", 0) or 0),
                "last_at": row.get("last_at"),
            }
            for row in rows
        ]

    async def purge(self, older_than_seconds: float) -> int:
        rows = await self._db.execute(
            """
            DELETE FROM conversation_sessions
            WHERE created_at < now() - make_interval(secs => $1)
            RETURNING work_key
            """,
            float(older_than_seconds),
        )
        return len(rows)


def _row_to_session_row(row: dict[str, Any]) -> SessionRow:
    return SessionRow(
        turn_id=str(row.get("turn_id", "") or ""),
        work_key=str(row.get("work_key", "") or ""),
        created_at=row.get("created_at"),
        entry=decode_jsonb_dict(row.get("entry")),
    )


class MemoryConversationSessionStore:
    """In-memory :class:`ConversationSessionStore` twin.

    Process-local, so a seat that moves to another node arrives with no
    history — correct for a single node, and no more than that, which is
    the same honesty the other memory twins carry.  A fleet without a
    database already gets the loud ``seat_placement_is_process_local``
    warning at boot.
    """

    def __init__(self) -> None:
        self._rows: dict[tuple[str, str], list[tuple[float, SessionRow]]] = {}

    async def append(
        self,
        *,
        agent_handle: str,
        conversation_key: str,
        work_key: str,
        turn_id: str,
        entry: dict[str, Any],
        max_entries: int = CONVERSATION_SESSION_MAX_ENTRIES,
    ) -> bool:
        if not agent_handle or not conversation_key or not work_key:
            return False
        bucket = self._rows.setdefault((agent_handle, conversation_key), [])
        if any(row.work_key == work_key for _at, row in bucket):
            return False  # first writer wins, as ON CONFLICT DO NOTHING
        bucket.append(
            (
                time.monotonic(),
                SessionRow(
                    turn_id=turn_id,
                    work_key=work_key,
                    created_at=datetime.now(UTC),
                    entry=dict(entry),
                ),
            )
        )
        keep = max(1, int(max_entries))
        if len(bucket) > keep:
            del bucket[:-keep]
        return True

    async def recent(
        self,
        *,
        agent_handle: str,
        conversation_key: str,
        limit: int,
    ) -> list[SessionRow]:
        if limit <= 0:
            return []
        bucket = self._rows.get((agent_handle, conversation_key), [])
        return [row for _at, row in bucket[-int(limit) :]]

    async def conversations(
        self, *, agent_handle: str, limit: int = 50
    ) -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = []
        for (handle, key), bucket in self._rows.items():
            if handle != agent_handle or not bucket:
                continue
            out.append(
                {
                    "conversation_key": key,
                    "entries": len(bucket),
                    "last_at": bucket[-1][1].created_at,
                }
            )
        out.sort(key=lambda item: item["last_at"] or datetime.min, reverse=True)
        return out[: int(limit)]

    async def purge(self, older_than_seconds: float) -> int:
        cutoff = time.monotonic() - max(older_than_seconds, 0.0)
        dropped = 0
        for key, bucket in list(self._rows.items()):
            kept = [(at, row) for at, row in bucket if at >= cutoff]
            dropped += len(bucket) - len(kept)
            if kept:
                self._rows[key] = kept
            else:
                del self._rows[key]
        return dropped


__all__ = [
    "CONVERSATION_SESSION_MAX_ENTRIES",
    "CONVERSATION_SESSION_RETENTION_SECONDS",
    "ConversationSessionStore",
    "MemoryConversationSessionStore",
    "PostgresConversationSessionStore",
    "SessionRow",
]
