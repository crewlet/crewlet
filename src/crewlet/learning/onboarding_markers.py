"""OnboardingMarkerStore -- per-agent onboarding bookkeeping.

One row per agent in ``agent_onboarding_markers``, keyed by
``agent_id`` so ``is_onboarded`` answers with a single indexed
equality lookup.  Re-onboarding (org chain change) UPSERTs the
existing row in place.

Operations are best-effort -- onboarding state is a UI nicety
(suppressing the per-turn "read these Onboarding pages" hint), not
a correctness invariant.  All failures log rather than propagating,
but a read failure is reported as **unknown** (``None``), never as
"not onboarded": collapsing a transient lookup failure into ``False``
would re-fire a full onboarding pass for an agent that had already
marked itself onboarded.
"""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from crewlet._logging import get_logger
from crewlet.db.client import Database

logger = get_logger("learning.onboarding_markers")


class OnboardingMarkerStore:
    """Postgres-backed marker store keyed by ``agent_id``.

    One row per agent: a fresh ``mark`` upserts the existing row so
    the table never carries stale markers.  ``is_onboarded`` returns
    ``True`` only when the stored ``chain_hash`` matches the caller's
    expected hash; a hash mismatch (org chain changed) reads as
    "not onboarded" so the hint re-fires for the new structure.  A
    lookup *failure* returns ``None`` (unknown) — callers must not
    treat unknown as unmarked, or a transient DB error re-runs a whole
    onboarding pass.
    """

    def __init__(self, db: Database) -> None:
        self._db = db

    async def is_onboarded(self, *, agent_id: str, chain_hash: str) -> bool | None:
        """Whether ``agent_id`` has a marker matching ``chain_hash``.

        Tri-state: ``True`` (marker matches), ``False`` (definitively no
        matching marker), ``None`` (lookup failed — state unknown).
        """
        if not agent_id or not chain_hash:
            return False
        try:
            rows = await self._db.execute(
                """
                SELECT chain_hash
                FROM agent_onboarding_markers
                WHERE agent_id = $1
                """,
                agent_id,
            )
        except Exception:
            logger.exception("onboarding_marker_lookup_failed", agent_id=agent_id)
            return None
        if not rows:
            return False
        return str(rows[0].get("chain_hash") or "") == chain_hash

    async def mark(
        self,
        *,
        agent_id: str,
        chain_hash: str,
        agent_handle: str = "",
        role: str = "",
        summary: str = "",
    ) -> None:
        """UPSERT a marker for ``agent_id``.

        Re-onboarding (a chain-hash change) overwrites the existing
        row in place, so ``is_onboarded`` immediately reflects the new
        state without a separate cleanup pass.  Blank ``agent_id`` /
        ``chain_hash`` is rejected so callers with incomplete context
        (missing role, missing chain) don't pollute the table with
        empty-key rows.  Marking also clears any pass lease — the pass
        is over.
        """
        if not agent_id:
            raise ValueError("agent_id is required")
        if not chain_hash:
            raise ValueError("chain_hash is required")
        await self._db.execute(
            """
            INSERT INTO agent_onboarding_markers (
                agent_id, chain_hash, agent_handle, role, summary
            )
            VALUES ($1, $2, $3, $4, $5)
            ON CONFLICT (agent_id) DO UPDATE
            SET chain_hash        = EXCLUDED.chain_hash,
                agent_handle      = EXCLUDED.agent_handle,
                role              = EXCLUDED.role,
                summary           = EXCLUDED.summary,
                in_progress_until = NULL,
                updated_at        = now()
            """,
            agent_id,
            chain_hash,
            agent_handle,
            role,
            summary,
        )
        logger.debug(
            "onboarding_marker_stored",
            agent_id=agent_id,
            agent_handle=agent_handle,
            role=role,
        )

    async def try_claim_pass(self, *, agent_id: str, ttl_seconds: float) -> bool:
        """Take the cross-process single-flight lease for an onboarding pass.

        The in-memory latch + lock serialize onboarding within ONE process,
        but agent inboxes are Shared subscriptions — during a rolling
        restart two engines can each run a turn for the same un-onboarded
        agent.  Whoever wins this upsert runs the pass; the loser skips
        (the winner's ``mark`` suppresses every later pass).  The lease
        rides the marker row itself: a claim for a never-marked agent
        inserts the row with an empty ``chain_hash``, which
        :meth:`is_onboarded` correctly reads as "not onboarded".  The TTL
        bounds a crashed claimant — a stale lease expires and the next
        turn re-claims.  A claim FAILURE (DB error) returns ``False``:
        never run a possibly-duplicate pass on unknown state.
        """
        if not agent_id:
            return False
        try:
            row = await self._db.fetchrow(
                """
                INSERT INTO agent_onboarding_markers (
                    agent_id, chain_hash, in_progress_until
                )
                VALUES ($1, '', now() + make_interval(secs => $2))
                ON CONFLICT (agent_id) DO UPDATE
                SET in_progress_until = now() + make_interval(secs => $2),
                    updated_at        = now()
                WHERE agent_onboarding_markers.in_progress_until IS NULL
                   OR agent_onboarding_markers.in_progress_until < now()
                RETURNING agent_id
                """,
                agent_id,
                float(ttl_seconds),
            )
        except Exception:
            logger.exception("onboarding_claim_failed", agent_id=agent_id)
            return False
        return row is not None

    async def release_claim(self, agent_id: str) -> None:
        """Clear the pass lease (pass ended without marking).

        Best-effort: a failed release just leaves the lease to its TTL.
        """
        if not agent_id:
            return
        try:
            await self._db.execute(
                """
                UPDATE agent_onboarding_markers
                SET in_progress_until = NULL, updated_at = now()
                WHERE agent_id = $1
                """,
                agent_id,
            )
        except Exception:
            logger.exception("onboarding_claim_release_failed", agent_id=agent_id)

    async def get(self, agent_id: str) -> dict[str, Any] | None:
        """Return the raw marker row for diagnostics (or ``None``)."""
        if not agent_id:
            return None
        try:
            rows = await self._db.execute(
                """
                SELECT agent_id, chain_hash, agent_handle, role, summary,
                       created_at, updated_at
                FROM agent_onboarding_markers
                WHERE agent_id = $1
                """,
                agent_id,
            )
        except Exception:
            logger.exception("onboarding_marker_get_failed", agent_id=agent_id)
            return None
        if not rows:
            return None
        row = dict(rows[0])
        for key in ("created_at", "updated_at"):
            if isinstance(row.get(key), datetime) and row[key].tzinfo is None:
                row[key] = row[key].replace(tzinfo=UTC)
        return row


__all__ = ["OnboardingMarkerStore"]
