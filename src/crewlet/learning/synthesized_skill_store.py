"""SynthesizedSkillStore — per-agent auto-drafted skill persistence.

The table holds one scope only: agent-scope rows keyed by
``(agent_handle, name)``.  The cross-agent promotion pass does not
write rows here -- it drafts a knowledge-base page under the unit's
``Auto-Drafted Skills`` parent (see
:class:`~crewlet.learning.skill_synthesizer.PromotionSynthesizer`,
writing through the active backend's ``PromotionPageWriter``), which
a unit lead reviews and publishes through the backend's normal
tools.  Once published the page reaches all unit members through
the active backend's query-time search behind the ``## Relevant
knowledge`` prefetch.

The history table (``synthesized_skill_versions``) keeps the
update/rollback semantics: every edit to a live row archives the
prior state to history, then updates in place and bumps ``version``.
The history is pruned to the most recent ``max_versions_kept`` per
skill after each update.

Lookup is exact-key.  ``fetch`` returns the agent's own row.
``list_for_agent`` returns every row owned by one agent;
``list_for_agent_handles`` returns rows owned by a set of handles
(used by the scheduler's promotion pass to assemble per-unit
candidate sets).
"""

from __future__ import annotations

import asyncio
import json
import random
from datetime import UTC, datetime
from typing import Any, Protocol
from uuid import UUID

from crewlet._logging import get_logger
from crewlet.db._jsonb import (
    decode_jsonb_dict,
    decode_jsonb_str_list,
    decode_jsonb_uuid_list,
)
from crewlet.db.client import Database
from crewlet.learning.models import (
    RefinementKind,
    SynthesizedSkill,
    SynthesizedSkillVersion,
)

logger = get_logger("learning.synthesized_skill_store")

# Sentinel for ``transition_state``'s optional optimistic-concurrency
# guard: ``_NO_GUARD`` means "transition unconditionally"; any other
# value (including ``None``) means "only transition if the row's
# ``last_used_at`` still equals this snapshot". ``None`` is a real,
# distinct guard value (a never-used skill), so it can't double as
# "no guard" -- hence the sentinel.
_NO_GUARD: Any = object()

# Bound on ``update``'s optimistic-concurrency retry loop.  A guard
# miss means another writer bumped the version between our read and
# write; we re-read and retry.  Concurrent refines of the *same* skill
# are rare (it needs the LLM-facing refine_skill tool to overlap the
# post-turn auto-refiner), so a handful of attempts is ample headroom.
_UPDATE_MAX_ATTEMPTS = 5


class SynthesizedSkillStoreProtocol(Protocol):
    """Protocol satisfied by both the real store and test fakes.

    Skills are agent-scope only; cross-agent promotion drafts go to
    the team knowledge base (Confluence or Plane) rather than into a
    unit-scope table row, so this surface is a single-scope keyed
    store.
    """

    async def insert(self, skill: SynthesizedSkill) -> SynthesizedSkill: ...

    async def fetch(
        self,
        *,
        agent_handle: str,
        name: str,
        include_archived: bool = False,
    ) -> SynthesizedSkill | None: ...

    async def list_for_agent(
        self,
        agent_handle: str,
        *,
        include_archived: bool = False,
        include_stale: bool = True,
    ) -> list[SynthesizedSkill]: ...

    async def count_for_agent(self, agent_handle: str) -> int: ...

    async def existing_tool_sequences(self, agent_handle: str) -> list[list[str]]: ...

    async def list_for_agent_handles(
        self, agent_handles: list[str]
    ) -> list[SynthesizedSkill]: ...

    """Fetch all skills owned by any of ``agent_handles``.

    Used by the scheduler's cross-agent promotion pass to assemble
    the per-unit candidate set.
    """

    async def update(
        self,
        skill: SynthesizedSkill,
        *,
        refinement_kind: RefinementKind,
        refinement_note: str = "",
        max_versions_kept: int = 10,
    ) -> SynthesizedSkill: ...

    async def list_versions(
        self, skill_id: UUID, limit: int = 20
    ) -> list[SynthesizedSkillVersion]: ...

    async def rollback_to_version(
        self, *, version_id: UUID, max_versions_kept: int = 10
    ) -> SynthesizedSkill | None: ...

    async def mark_used(
        self,
        skill_id: UUID,
        *,
        event_queue: Any = None,
        skill_name: str = "",
        agent_handle: str = "",
    ) -> None: ...

    async def transition_state(
        self,
        skill_id: UUID,
        *,
        new_state: str,
        reason: str = "",
        guard_last_used_at: Any = _NO_GUARD,
    ) -> bool: ...

    async def set_pinned(self, skill_id: UUID, *, pinned: bool) -> bool: ...

    async def list_curator_candidates(
        self, agent_handle: str = ""
    ) -> list[SynthesizedSkill]: ...


class SynthesizedSkillStore:
    """Postgres-backed synthesized-skill store."""

    def __init__(self, db: Database) -> None:
        self._db = db

    # ------------------------------------------------------------------
    # Insert + fetch + list (scope-aware)
    # ------------------------------------------------------------------

    async def insert(self, skill: SynthesizedSkill) -> SynthesizedSkill:
        """Insert a new synthesized skill.

        Raises on name collision (the ``(agent_handle, name)`` unique
        index enforces it).  Callers are expected to pre-check
        collisions; a violation here is a programming error, not a
        concurrency race -- the synthesizer doesn't synthesise in
        parallel for the same agent.
        """
        await self._db.execute(
            """
            INSERT INTO synthesized_skills (
                id, agent_handle, name,
                description, content, frontmatter, tool_sequence,
                source_episode_ids, version
            )
            VALUES (
                $1::uuid, $2, $3,
                $4, $5, $6::jsonb, $7::jsonb,
                $8::jsonb, $9
            )
            """,
            str(skill.id),
            skill.agent_handle,
            skill.name,
            skill.description,
            skill.content,
            *_encode_skill_jsonb(skill),
            int(skill.version),
        )
        logger.info(
            "synthesized_skill_stored",
            agent_handle=skill.agent_handle,
            name=skill.name,
            tool_count=len(skill.tool_sequence or []),
            sources=len(skill.source_episode_ids or []),
        )
        return skill

    async def fetch(
        self,
        *,
        agent_handle: str,
        name: str,
        include_archived: bool = False,
    ) -> SynthesizedSkill | None:
        """Return ``agent_handle``'s skill named ``name``, or ``None``.

        Curator semantics: by default ``state='archived'`` rows are returned with
        their state set so callers can decide what to do. ``_use_skill``
        inspects the state and refuses archived skills; operators with
        admin paths pass ``include_archived=True`` to bypass the filter.
        Here we always return the row (state inspection is the
        caller's responsibility) but expose the flag for symmetry with
        :meth:`list_for_agent`.
        """
        if not name or not agent_handle:
            return None
        rows = await self._db.execute(
            """
            SELECT id, agent_handle, name,
                   description, content, frontmatter, tool_sequence,
                   source_episode_ids,
                   version, created_at, updated_at,
                   use_count, last_used_at,
                   state, pinned, stale_at, archived_at
            FROM synthesized_skills
            WHERE agent_handle = $1
              AND name = $2
            """,
            agent_handle,
            name,
        )
        if not rows:
            return None
        skill = _row_to_skill(rows[0])
        if not include_archived and skill.state == "archived":
            return None
        return skill

    async def list_for_agent(
        self,
        agent_handle: str,
        *,
        include_archived: bool = False,
        include_stale: bool = True,
    ) -> list[SynthesizedSkill]:
        """Return ``agent_handle``'s skills (newest first).

        Curator semantics: by default hides ``state='archived'`` skills but keeps
        ``stale`` ones visible -- the Plan-phase prefetch renders
        stale rows with a marker so the agent knows they're aging.
        Pass ``include_archived=True`` for admin paths that want to
        see everything.
        """
        if not agent_handle:
            return []
        # Build the state filter dynamically so the SQL stays simple
        # and there's no risk of mis-using a runtime list-IN clause.
        excluded_states: list[str] = []
        if not include_archived:
            excluded_states.append("archived")
        if not include_stale:
            excluded_states.append("stale")
        if excluded_states:
            placeholders = ", ".join(f"${i + 2}" for i in range(len(excluded_states)))
            sql = f"""
                SELECT id, agent_handle, name,
                       description, content, frontmatter, tool_sequence,
                       source_episode_ids,
                       version, created_at, updated_at,
                       use_count, last_used_at,
                       state, pinned, stale_at, archived_at
                FROM synthesized_skills
                WHERE agent_handle = $1
                  AND state NOT IN ({placeholders})
                ORDER BY created_at DESC
            """
            rows = await self._db.execute(sql, agent_handle, *excluded_states)
        else:
            rows = await self._db.execute(
                """
                SELECT id, agent_handle, name,
                       description, content, frontmatter, tool_sequence,
                       source_episode_ids,
                       version, created_at, updated_at,
                       use_count, last_used_at,
                       state, pinned, stale_at, archived_at
                FROM synthesized_skills
                WHERE agent_handle = $1
                ORDER BY created_at DESC
                """,
                agent_handle,
            )
        return [_row_to_skill(row) for row in rows]

    async def count_for_agent(self, agent_handle: str) -> int:
        """Count this agent's skills for the per-agent cap check."""
        if not agent_handle:
            return 0
        rows = await self._db.execute(
            """
            SELECT COUNT(*)::int AS n
            FROM synthesized_skills
            WHERE agent_handle = $1
            """,
            agent_handle,
        )
        if not rows:
            return 0
        return int(rows[0].get("n") or 0)

    async def existing_tool_sequences(self, agent_handle: str) -> list[list[str]]:
        """Tool sequences of this agent's existing skills."""
        if not agent_handle:
            return []
        rows = await self._db.execute(
            """
            SELECT tool_sequence
            FROM synthesized_skills
            WHERE agent_handle = $1
            """,
            agent_handle,
        )
        return [decode_jsonb_str_list(row.get("tool_sequence")) for row in rows]

    # ------------------------------------------------------------------
    # Cross-agent listing (for the scheduler's promotion pass)
    # ------------------------------------------------------------------

    async def list_for_agent_handles(
        self, agent_handles: list[str]
    ) -> list[SynthesizedSkill]:
        """Every skill owned by any of ``agent_handles``.

        Used by the scheduler's promotion pass to assemble a per-unit
        candidate set for cross-agent clustering.
        """
        if not agent_handles:
            return []
        # asyncpg supports list binding via ``= ANY($N)``.  Selects the
        # full column set (incl. curator state) so callers never see a
        # SynthesizedSkill with a defaulted-and-wrong ``state``.
        rows = await self._db.execute(
            """
            SELECT id, agent_handle, name,
                   description, content, frontmatter, tool_sequence,
                   source_episode_ids,
                   version, created_at, updated_at,
                   use_count, last_used_at,
                   state, pinned, stale_at, archived_at
            FROM synthesized_skills
            WHERE agent_handle = ANY($1::text[])
            ORDER BY created_at DESC
            """,
            list(agent_handles),
        )
        logger.debug(
            "listed_for_agent_handles",
            members=len(agent_handles),
            returned=len(rows),
        )
        return [_row_to_skill(row) for row in rows]

    # ------------------------------------------------------------------
    # Versioned update + rollback
    # ------------------------------------------------------------------

    async def update(
        self,
        skill: SynthesizedSkill,
        *,
        refinement_kind: RefinementKind,
        refinement_note: str = "",
        max_versions_kept: int = 10,
    ) -> SynthesizedSkill:
        """Update the live row in place, archiving the prior state.

        ``skill`` is the *proposed* new state — pass the mutated
        ``SynthesizedSkill`` with the updated content / description /
        frontmatter / tool_sequence; this method bumps ``version``,
        refreshes ``updated_at``, and returns the persisted row.

        **Concurrency-safe via an optimistic version guard.** The
        LLM-facing ``refine_skill`` builtin can overlap the post-turn
        auto-refiner for the same agent, so two ``update`` calls *can*
        race the same skill.  The write is a guarded
        ``UPDATE ... WHERE version = <observed>``: on a guard miss
        (another writer bumped the row first) it re-reads and retries.
        The version sequence stays gap-free, every superseded state is
        archived, and last-writer-wins on content -- a superseded
        body is never lost, only demoted to history (recoverable via
        ``rollback_to_version``).
        """
        for attempt in range(_UPDATE_MAX_ATTEMPTS):
            current = await self._fetch_by_id(skill.id)
            if current is None:
                raise ValueError(
                    f"cannot update unknown synthesized skill id={skill.id}"
                )
            new_version = current.version + 1
            # Guarded UPDATE: fires only if the row still has the
            # version we just read.  ``RETURNING id`` lets the
            # ``fetch``-backed client report whether a row matched.
            won = await self._db.execute(
                """
                UPDATE synthesized_skills
                SET description = $2,
                    content = $3,
                    frontmatter = $4::jsonb,
                    tool_sequence = $5::jsonb,
                    source_episode_ids = $6::jsonb,
                    version = $7,
                    updated_at = now()
                WHERE id = $1::uuid
                  AND version = $8
                RETURNING id
                """,
                str(skill.id),
                skill.description,
                skill.content,
                *_encode_skill_jsonb(skill),
                int(new_version),
                int(current.version),
            )
            if not won:
                # A concurrent writer bumped the version between our
                # read and write.  Re-read and retry -- nothing was
                # archived yet, so there is no spurious history row.
                logger.warning(
                    "synthesized_skill_update_raced",
                    skill_id=str(skill.id),
                    observed_version=current.version,
                    attempt=attempt,
                )
                continue
            # We won the race.  Archive the state we just replaced --
            # ``current`` is exactly that state because the guard
            # matched ``version = current.version``.
            await self._insert_version(
                current,
                refinement_kind=refinement_kind,
                refinement_note=refinement_note,
            )
            await self._prune_versions(skill.id, keep=max_versions_kept)
            logger.info(
                "synthesized_skill_updated",
                skill_id=str(skill.id),
                refinement_kind=refinement_kind,
                version=new_version,
            )
            updated = await self._fetch_by_id(skill.id)
            if updated is None:  # pragma: no cover - defensive
                raise RuntimeError("update lost the row")
            return updated
        raise RuntimeError(
            f"synthesized skill {skill.id} update lost "
            f"{_UPDATE_MAX_ATTEMPTS} consecutive concurrency races"
        )

    async def list_versions(
        self, skill_id: UUID, limit: int = 20
    ) -> list[SynthesizedSkillVersion]:
        rows = await self._db.execute(
            """
            SELECT id, skill_id, agent_handle,
                   name, description, content, frontmatter,
                   tool_sequence, source_episode_ids, version,
                   refinement_kind, refinement_note, archived_at
            FROM synthesized_skill_versions
            WHERE skill_id = $1::uuid
            ORDER BY archived_at DESC
            LIMIT $2
            """,
            str(skill_id),
            int(limit),
        )
        return [_row_to_version(row) for row in rows]

    async def rollback_to_version(
        self, *, version_id: UUID, max_versions_kept: int = 10
    ) -> SynthesizedSkill | None:
        """Rewrite the live row from an archived version.

        Treated as a forward-step: the current state is archived
        (``refinement_kind="rollback"``) and a new ``version = prev + 1``
        is written containing the archived content.  Nothing is lost
        -- a rollback followed by another rollback works.
        """
        rows = await self._db.execute(
            """
            SELECT id, skill_id, agent_handle,
                   name, description, content, frontmatter,
                   tool_sequence, source_episode_ids, version,
                   refinement_kind, refinement_note, archived_at
            FROM synthesized_skill_versions
            WHERE id = $1::uuid
            """,
            str(version_id),
        )
        if not rows:
            return None
        archived = _row_to_version(rows[0])
        current = await self._fetch_by_id(archived.skill_id)
        if current is None:
            return None
        target = current.model_copy(
            update={
                "description": archived.description,
                "content": archived.content,
                "frontmatter": dict(archived.frontmatter),
                "tool_sequence": list(archived.tool_sequence),
                "source_episode_ids": list(archived.source_episode_ids),
            }
        )
        return await self.update(
            target,
            refinement_kind="rollback",
            refinement_note=f"rolled back to version {archived.version}",
            max_versions_kept=max_versions_kept,
        )

    # ------------------------------------------------------------------
    # Internal
    # ------------------------------------------------------------------

    async def _fetch_by_id(self, skill_id: UUID) -> SynthesizedSkill | None:
        # Must select the full column set -- including the curator-state
        # columns -- so the SynthesizedSkill this returns carries the
        # row's *real* state/pinned/timestamps.  Omitting them made
        # ``_row_to_skill`` default state="active"/pinned=False, so
        # ``update`` and ``rollback_to_version`` returned objects that
        # silently misreported a pinned or archived skill.
        rows = await self._db.execute(
            """
            SELECT id, agent_handle, name,
                   description, content, frontmatter, tool_sequence,
                   source_episode_ids, version, created_at, updated_at,
                   use_count, last_used_at,
                   state, pinned, stale_at, archived_at
            FROM synthesized_skills
            WHERE id = $1::uuid
            """,
            str(skill_id),
        )
        if not rows:
            return None
        return _row_to_skill(rows[0])

    async def _insert_version(
        self,
        skill: SynthesizedSkill,
        *,
        refinement_kind: RefinementKind,
        refinement_note: str,
    ) -> None:
        await self._db.execute(
            """
            INSERT INTO synthesized_skill_versions (
                skill_id, agent_handle, name,
                description, content, frontmatter, tool_sequence,
                source_episode_ids,
                version, refinement_kind, refinement_note
            )
            VALUES (
                $1::uuid, $2, $3,
                $4, $5, $6::jsonb, $7::jsonb,
                $8::jsonb, $9, $10, $11
            )
            """,
            str(skill.id),
            skill.agent_handle,
            skill.name,
            skill.description,
            skill.content,
            *_encode_skill_jsonb(skill),
            int(skill.version),
            refinement_kind,
            refinement_note,
        )

    async def mark_used(
        self,
        skill_id: UUID,
        *,
        event_queue: Any = None,
        skill_name: str = "",
        agent_handle: str = "",
    ) -> None:
        """Bump ``use_count`` and stamp ``last_used_at`` for one skill.

        Called by ``_use_skill`` after a successful synthesized-skill
        load.  Best-effort: a UPDATE failure is logged but never
        surfaced to the tool path -- skill loading must not depend on
        telemetry success.

        Hardening: retries once with random jitter on the first
        failure. A persistent failure publishes
        :class:`SkillTelemetryWriteFailed` so operators can detect a
        degraded ``learning_health`` view before the curator wrongly
        archives a skill whose ``last_used_at`` failed to refresh.

        Idempotent in the loose sense: each call increments by exactly
        one.  Concurrent calls for the same skill on different turns
        race naturally on the database row; both bumps land.

        **Revival.** Using a skill again revives it: if the row
        was ``stale`` the same UPDATE flips it back to ``active`` and
        clears ``stale_at``.  Doing it here -- in the atomic use-count
        bump -- means a freshly-used skill is visible to the next Plan
        prefetch immediately, not only after the curator's next
        interval tick.  ``archived`` rows are never reached: ``_use_skill``
        refuses to load them, so ``mark_used`` is never called for one;
        the ``CASE`` leaves any non-``stale`` state untouched regardless.
        """
        sql = """
            UPDATE synthesized_skills
            SET use_count = use_count + 1,
                last_used_at = now(),
                state = CASE WHEN state = 'stale' THEN 'active' ELSE state END,
                stale_at = CASE WHEN state = 'stale' THEN NULL ELSE stale_at END
            WHERE id = $1::uuid
        """
        last_exc: Exception | None = None
        for attempt in range(2):
            try:
                await self._db.execute(sql, str(skill_id))
                return
            except Exception as exc:
                last_exc = exc
                if attempt == 0:
                    logger.warning(
                        "synthesized_skill_mark_used_retry",
                        skill_id=str(skill_id),
                        error_class=type(exc).__name__,
                    )
                    await asyncio.sleep(random.uniform(0.05, 0.15))
                else:
                    logger.exception(
                        "synthesized_skill_mark_used_failed",
                        skill_id=str(skill_id),
                    )
        # Persistent failure -- publish the operator-facing telemetry
        # event. Best-effort: a queue absence / publish failure must
        # not crash the calling code path.
        if event_queue is not None and last_exc is not None:
            try:
                from crewlet.events.types import SkillTelemetryWriteFailed

                await event_queue.publish(
                    "crewlet.events.skill_telemetry_write_failed",
                    SkillTelemetryWriteFailed(
                        source=agent_handle or "",
                        skill_id=str(skill_id),
                        skill_name=skill_name,
                        agent_handle=agent_handle,
                        kind="mark_used",
                        error=str(last_exc)[:300],
                    ),
                )
            except Exception:
                logger.exception("skill_telemetry_event_publish_failed")

    async def transition_state(
        self,
        skill_id: UUID,
        *,
        new_state: str,
        reason: str = "",
        guard_last_used_at: Any = _NO_GUARD,
    ) -> bool:
        """Move a skill to a new state and stamp the matching timestamp.

        ``new_state`` must be one of ``active`` / ``stale`` /
        ``archived``. Returns ``True`` only when a row was actually
        updated.

        Sets ``stale_at`` when transitioning to stale, ``archived_at``
        when transitioning to archived. Clears both when going back to
        active. No version row archived -- transitions are
        out-of-band metadata, not skill-content changes, and
        ``synthesized_skill_versions`` rows are reserved for refinement
        edits.

        **Optimistic-concurrency guard.** When
        ``guard_last_used_at`` is supplied (the curator passes the
        ``last_used_at`` value it snapshotted via
        ``list_curator_candidates``), the UPDATE only fires if the
        row's ``last_used_at`` *still* equals that snapshot --
        ``IS NOT DISTINCT FROM`` so a ``NULL`` snapshot (never-used
        skill) is matched correctly. If a concurrent ``mark_used``
        bumped ``last_used_at`` between the curator's SELECT and this
        UPDATE, zero rows match and the method returns ``False``: the
        curator then counts it as raced and skips the event, leaving
        the just-used skill in its current state. This closes the
        read-then-archive race against the Plan-phase prefetch /
        mid-turn ``use_skill`` without needing a separate "safety
        window" heuristic.

        Omit ``guard_last_used_at`` (the default ``_NO_GUARD``) for
        the revival transition (``stale → active``) -- reviving a
        recently-used skill is the safe direction and should never
        be blocked.

        Callers (the ``SkillCuratorWorker``) handle event
        publication separately so they can include ``last_used_at`` /
        ``prior_state`` context that the store doesn't have at the
        UPDATE point.
        """
        if new_state not in {"active", "stale", "archived"}:
            raise ValueError(f"invalid state: {new_state}")
        if new_state == "stale":
            updates = "state = $2, stale_at = now(), archived_at = NULL"
        elif new_state == "archived":
            updates = "state = $2, archived_at = now()"
        else:
            updates = "state = $2, stale_at = NULL, archived_at = NULL"
        sql = f"UPDATE synthesized_skills SET {updates} WHERE id = $1::uuid"
        params: list[Any] = [str(skill_id), new_state]
        if guard_last_used_at is not _NO_GUARD:
            # ``IS NOT DISTINCT FROM`` so a NULL snapshot (never-used
            # skill) matches a still-NULL row, while any concurrent
            # ``mark_used`` bump fails the guard.
            sql += " AND last_used_at IS NOT DISTINCT FROM $3"
            params.append(guard_last_used_at)
        # ``RETURNING id`` turns the UPDATE into a row-returning query
        # so ``Database.execute`` (which is ``fetch``-backed) can
        # report whether a row actually matched.
        sql += " RETURNING id"
        try:
            rows = await self._db.execute(sql, *params)
        except Exception:
            logger.exception(
                "synthesized_skill_transition_state_failed",
                skill_id=str(skill_id),
                new_state=new_state,
            )
            return False
        if not rows:
            # Guard failed: the row was used since the curator's
            # snapshot. Not an error -- expected under the race.
            logger.debug(
                "synthesized_skill_transition_raced",
                skill_id=str(skill_id),
                new_state=new_state,
            )
            return False
        logger.info(
            "synthesized_skill_state_transition",
            skill_id=str(skill_id),
            new_state=new_state,
            reason=reason,
        )
        return True

    async def set_pinned(self, skill_id: UUID, *, pinned: bool) -> bool:
        """Toggle the ``pinned`` flag. Pinned skills are exempt from
        every curator auto-transition.

        Returns True when the row was updated.
        """
        try:
            await self._db.execute(
                "UPDATE synthesized_skills SET pinned = $2 WHERE id = $1::uuid",
                str(skill_id),
                bool(pinned),
            )
            return True
        except Exception:
            logger.exception(
                "synthesized_skill_set_pinned_failed",
                skill_id=str(skill_id),
            )
            return False

    async def list_curator_candidates(
        self, agent_handle: str = ""
    ) -> list[SynthesizedSkill]:
        """Return non-pinned, non-archived skills the curator should
        evaluate. When ``agent_handle`` is empty, returns rows for
        every agent."""
        sql = """
            SELECT id, agent_handle, name,
                   description, content, frontmatter, tool_sequence,
                   source_episode_ids,
                   version, created_at, updated_at,
                   use_count, last_used_at,
                   state, pinned, stale_at, archived_at
            FROM synthesized_skills
            WHERE pinned = FALSE
              AND state <> 'archived'
        """
        if agent_handle:
            sql += " AND agent_handle = $1"
            rows = await self._db.execute(sql, agent_handle)
        else:
            rows = await self._db.execute(sql)
        return [_row_to_skill(row) for row in rows]

    async def _prune_versions(self, skill_id: UUID, *, keep: int) -> None:
        if keep <= 0:
            return
        # ``version DESC`` is the tiebreaker: two version rows can share
        # an ``archived_at`` (concurrent refines in the same instant),
        # and ``version`` is the monotonic, meaningful order -- so the
        # ``keep`` newest are deterministically the highest-versioned.
        await self._db.execute(
            """
            DELETE FROM synthesized_skill_versions
            WHERE id IN (
                SELECT id
                FROM synthesized_skill_versions
                WHERE skill_id = $1::uuid
                ORDER BY archived_at DESC, version DESC
                OFFSET $2
            )
            """,
            str(skill_id),
            int(keep),
        )


# ----------------------------------------------------------------------
# Row ↔ model helpers
# ----------------------------------------------------------------------


def _row_to_skill(row: dict[str, Any]) -> SynthesizedSkill:
    frontmatter = decode_jsonb_dict(row.get("frontmatter"))
    tool_sequence = decode_jsonb_str_list(row.get("tool_sequence"))
    source_ids = decode_jsonb_uuid_list(row.get("source_episode_ids"))
    created_at = row.get("created_at") or datetime.now(UTC)
    updated_at = row.get("updated_at") or datetime.now(UTC)
    return SynthesizedSkill(
        id=row["id"],
        agent_handle=row.get("agent_handle") or "",
        name=row["name"],
        description=row["description"],
        content=row["content"],
        frontmatter=frontmatter,
        tool_sequence=tool_sequence,
        source_episode_ids=source_ids,
        version=int(row.get("version") or 1),
        created_at=created_at,
        updated_at=updated_at,
        use_count=int(row.get("use_count") or 0),
        last_used_at=row.get("last_used_at"),
        state=row.get("state") or "active",
        pinned=bool(row.get("pinned") or False),
        stale_at=row.get("stale_at"),
        archived_at=row.get("archived_at"),
    )


def _row_to_version(row: dict[str, Any]) -> SynthesizedSkillVersion:
    frontmatter = decode_jsonb_dict(row.get("frontmatter"))
    tool_sequence = decode_jsonb_str_list(row.get("tool_sequence"))
    source_ids = decode_jsonb_uuid_list(row.get("source_episode_ids"))
    archived_at = row.get("archived_at") or datetime.now(UTC)
    # refinement_kind is validated via the Literal type on the model.
    return SynthesizedSkillVersion(
        id=row["id"],
        skill_id=row["skill_id"],
        agent_handle=row.get("agent_handle") or "",
        name=row["name"],
        description=row["description"],
        content=row["content"],
        frontmatter=frontmatter,
        tool_sequence=tool_sequence,
        source_episode_ids=source_ids,
        version=int(row.get("version") or 1),
        refinement_kind=row["refinement_kind"],
        refinement_note=row.get("refinement_note") or "",
        archived_at=archived_at,
    )


def _encode_skill_jsonb(skill: SynthesizedSkill) -> tuple[str, str, str]:
    """Encode the three JSONB columns shared by every skill-write path.

    Returns ``(frontmatter, tool_sequence, source_episode_ids)`` JSON
    strings in the column order used by ``INSERT synthesized_skills``,
    ``UPDATE synthesized_skills``, and ``INSERT synthesized_skill_versions``.
    Keeps the encoding behaviour -- ``None`` -> empty, UUIDs ->
    stringified -- in one place.
    """
    return (
        json.dumps(skill.frontmatter or {}),
        json.dumps(list(skill.tool_sequence or [])),
        json.dumps([str(x) for x in (skill.source_episode_ids or [])]),
    )
