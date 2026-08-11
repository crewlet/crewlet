"""CounterpartyStore — per-(observer, subject) profile persistence.

Upserts trait patches into a dedicated ``counterparty_profiles`` table
(migration ``003_counterparty_profiles.sql``).  Update semantics: JSONB
merge via ``traits || $patch`` so LLM-emitted patches add or overwrite
only the keys they touch; keys not in the patch are retained.

Lookup is exact-key, never vector: profiles are fetched by either
resolved ``subject_handle`` or the ``(subject_platform,
subject_external_id)`` pair.
"""

from __future__ import annotations

import asyncio
import json
from datetime import UTC, datetime
from typing import Any, Protocol

from crewlet._logging import get_logger
from crewlet.db._jsonb import decode_jsonb_dict
from crewlet.db.client import Database
from crewlet.learning.interaction import CanonicalIdentity
from crewlet.learning.models import CounterpartyProfile

logger = get_logger("learning.counterparty_store")


class CounterpartyStoreProtocol(Protocol):
    """Protocol satisfied by both the real store and test fakes."""

    async def upsert(
        self,
        *,
        observer_handle: str,
        subject_handle: str = "",
        subject_external_id: str = "",
        subject_platform: str = "",
        subject_name: str = "",
        traits_patch: dict[str, Any] | None = None,
        increment_interactions: bool = True,
    ) -> CounterpartyProfile: ...

    async def fetch(
        self,
        *,
        observer_handle: str,
        subject_handle: str = "",
        subject_external_id: str = "",
        subject_platform: str = "",
    ) -> CounterpartyProfile | None: ...

    async def fetch_for_senders(
        self, observer_handle: str, senders: list[CanonicalIdentity]
    ) -> list[CounterpartyProfile]: ...


class CounterpartyStore:
    """Postgres-backed counterparty-profile store."""

    def __init__(self, db: Database) -> None:
        self._db = db

    async def upsert(
        self,
        *,
        observer_handle: str,
        subject_handle: str = "",
        subject_external_id: str = "",
        subject_platform: str = "",
        subject_name: str = "",
        traits_patch: dict[str, Any] | None = None,
        increment_interactions: bool = True,
    ) -> CounterpartyProfile:
        """Merge ``traits_patch`` into the profile row.

        Never overwrites ``first_seen_at`` on subsequent updates.
        Returns the row's state post-merge.
        """
        if not observer_handle:
            raise ValueError("observer_handle is required")
        if not subject_handle and not subject_external_id:
            raise ValueError("subject_handle or subject_external_id is required")

        patch = traits_patch or {}
        increment = 1 if increment_interactions else 0
        # ``last_corroborated_at`` only advances when traits actually
        # changed (patch non-empty).  ``last_updated_at`` continues to
        # capture interaction cadence (bumped on every upsert).  The
        # split lets Plan-phase prefetch demote stale traits without
        # losing visibility into how often the observer talks to this
        # subject.
        traits_changed = 1 if patch else 0
        rows = await self._db.execute(
            """
            INSERT INTO counterparty_profiles (
                observer_handle, subject_handle, subject_external_id,
                subject_platform, subject_name, traits, interaction_count
            )
            VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
            ON CONFLICT (
                observer_handle, subject_handle, subject_external_id,
                subject_platform
            )
            DO UPDATE SET
                traits = counterparty_profiles.traits || EXCLUDED.traits,
                subject_name = CASE
                    WHEN EXCLUDED.subject_name <> ''
                    THEN EXCLUDED.subject_name
                    ELSE counterparty_profiles.subject_name
                END,
                last_updated_at = now(),
                last_corroborated_at = CASE
                    WHEN $9::int = 1 THEN now()
                    ELSE counterparty_profiles.last_corroborated_at
                END,
                interaction_count = counterparty_profiles.interaction_count + $8
            RETURNING observer_handle, subject_handle, subject_external_id,
                      subject_platform, subject_name, traits,
                      first_seen_at, last_updated_at, last_corroborated_at,
                      interaction_count
            """,
            observer_handle,
            subject_handle,
            subject_external_id,
            subject_platform,
            subject_name,
            json.dumps(patch),
            increment,
            increment,
            traits_changed,
        )
        if not rows:
            raise RuntimeError("counterparty upsert returned no row")
        profile = _row_to_profile(rows[0])
        logger.info(
            "counterparty_upserted",
            observer=observer_handle,
            subject_handle=subject_handle or "",
            subject_external_id=subject_external_id or "",
            subject_platform=subject_platform or "",
            traits_patched=len(patch),
            interaction_count=profile.interaction_count,
        )
        return profile

    async def fetch(
        self,
        *,
        observer_handle: str,
        subject_handle: str = "",
        subject_external_id: str = "",
        subject_platform: str = "",
    ) -> CounterpartyProfile | None:
        """Look up one profile row by composite key.

        Pass either ``subject_handle`` (for resolved agents) or
        ``subject_external_id`` + ``subject_platform`` (for external
        humans).  Returns ``None`` when no row exists.
        """
        if not observer_handle:
            return None
        if not subject_handle and not subject_external_id:
            return None

        rows = await self._db.execute(
            """
            SELECT observer_handle, subject_handle, subject_external_id,
                   subject_platform, subject_name, traits,
                   first_seen_at, last_updated_at, last_corroborated_at,
                   interaction_count
            FROM counterparty_profiles
            WHERE observer_handle = $1
              AND subject_handle = $2
              AND subject_external_id = $3
              AND subject_platform = $4
            """,
            observer_handle,
            subject_handle,
            subject_external_id,
            subject_platform,
        )
        if not rows:
            return None
        return _row_to_profile(rows[0])

    async def fetch_for_senders(
        self, observer_handle: str, senders: list[CanonicalIdentity]
    ) -> list[CounterpartyProfile]:
        """Fetch the stored profiles for the given sender identities.

        One profile per sender that has a stored row — callers pass the
        distinct identifiable senders of a trigger (the turn path
        derives them once via ``TurnContext.trigger_interactions()`` +
        ``merge_interactions_by_sender``; the store stays free of
        event-shape parsing).  Per-sender fetches are independent
        single-row SELECTs, so they run concurrently; result order
        follows the input order.  Empty input → empty result."""
        if not senders:
            return []
        fetched = await asyncio.gather(
            *(
                self.fetch(
                    observer_handle=observer_handle,
                    subject_handle=sender.handle,
                    subject_external_id=sender.external_id,
                    subject_platform=sender.platform,
                )
                for sender in senders
            )
        )
        return [profile for profile in fetched if profile is not None]


def _row_to_profile(row: dict[str, Any]) -> CounterpartyProfile:
    first_seen_at = row.get("first_seen_at") or datetime.now(UTC)
    last_updated_at = row.get("last_updated_at") or datetime.now(UTC)
    last_corroborated_at = row.get("last_corroborated_at")
    return CounterpartyProfile(
        observer_handle=row["observer_handle"],
        subject_handle=row.get("subject_handle") or "",
        subject_external_id=row.get("subject_external_id") or "",
        subject_platform=row.get("subject_platform") or "",
        subject_name=row.get("subject_name") or "",
        traits=decode_jsonb_dict(row.get("traits")),
        first_seen_at=first_seen_at,
        last_updated_at=last_updated_at,
        last_corroborated_at=last_corroborated_at,
        interaction_count=int(row.get("interaction_count") or 0),
    )
