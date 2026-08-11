"""Typed queries against the ``company_config`` table — versioned Tier B."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Any
from uuid import UUID, uuid4

from crewlet._logging import get_logger
from crewlet.db._jsonb import decode_jsonb_dict
from crewlet.db.client import Database

logger = get_logger("db.company_config")


@dataclass
class CompanyConfigRevision:
    """One row in the ``company_config`` table — an immutable revision."""

    revision_id: UUID
    parent_revision_id: UUID | None
    created_at: datetime
    created_by: str
    source: str
    summary: str
    payload: dict[str, Any]
    is_active: bool
    activated_at: datetime | None


def _row_to_revision(row: dict[str, Any]) -> CompanyConfigRevision:
    return CompanyConfigRevision(
        revision_id=row["revision_id"],
        parent_revision_id=row.get("parent_revision_id"),
        created_at=row["created_at"],
        created_by=row["created_by"],
        source=row["source"],
        summary=row["summary"],
        payload=decode_jsonb_dict(row["payload"]),
        is_active=row["is_active"],
        activated_at=row.get("activated_at"),
    )


class CompanyConfigStore:
    """Persist and query versioned company-config revisions.

    Single-tenant: at most one row has ``is_active = TRUE`` at any
    time.  Zero rows is the unconfigured state — the engine boots, the
    API serves ``/config/*``, and the first ``insert_active`` populates
    the company.
    """

    def __init__(self, db: Database) -> None:
        self._db = db

    async def insert_active(
        self,
        payload: dict[str, Any],
        *,
        created_by: str,
        source: str,
        summary: str,
        parent_revision_id: UUID | None = None,
    ) -> UUID:
        """Insert a new revision and mark it active.

        Deactivates any prior active row inside the same transaction so
        the partial unique index ``company_config_one_active`` never
        sees two ``TRUE`` rows simultaneously.
        """
        import json

        revision_id = uuid4()
        pool = self._db._require_pool()
        async with pool.acquire() as conn, conn.transaction():
            await conn.execute(
                "UPDATE company_config SET is_active = FALSE WHERE is_active",
            )
            await conn.execute(
                """
                    INSERT INTO company_config
                        (revision_id, parent_revision_id, created_by,
                         source, summary, payload, is_active, activated_at)
                    VALUES ($1, $2, $3, $4, $5, $6::jsonb, TRUE, now())
                    """,
                revision_id,
                parent_revision_id,
                created_by,
                source,
                summary,
                json.dumps(payload),
            )
        logger.info(
            "company_config_revision_activated",
            revision_id=str(revision_id),
            source=source,
            created_by=created_by,
        )
        return revision_id

    async def get_active(self) -> CompanyConfigRevision | None:
        """Return the currently-active revision, or ``None``."""
        row = await self._db.fetchrow(
            "SELECT * FROM company_config WHERE is_active LIMIT 1",
        )
        return _row_to_revision(row) if row is not None else None

    async def get_revision(self, revision_id: UUID) -> CompanyConfigRevision | None:
        """Return a revision by id, or ``None``."""
        row = await self._db.fetchrow(
            "SELECT * FROM company_config WHERE revision_id = $1",
            revision_id,
        )
        return _row_to_revision(row) if row is not None else None

    async def list_revisions(
        self,
        *,
        limit: int = 50,
        offset: int = 0,
    ) -> list[CompanyConfigRevision]:
        """Return revisions ordered by ``created_at`` descending."""
        rows = await self._db.execute(
            """
            SELECT * FROM company_config
            ORDER BY created_at DESC
            LIMIT $1 OFFSET $2
            """,
            limit,
            offset,
        )
        return [_row_to_revision(row) for row in rows]

    async def has_any(self) -> bool:
        """Return ``True`` iff at least one active revision exists."""
        value = await self._db.fetchval(
            "SELECT 1 FROM company_config WHERE is_active LIMIT 1",
        )
        return value is not None

    async def activate(self, revision_id: UUID) -> None:
        """Mark a specific existing revision as the active one.

        Used by the revert path: an operator picks a historical
        revision and the dispatcher re-activates its payload.  The
        deactivate-then-activate pair runs in one transaction so the
        partial unique index never sees two ``TRUE`` rows.
        """
        pool = self._db._require_pool()
        async with pool.acquire() as conn, conn.transaction():
            await conn.execute(
                "UPDATE company_config SET is_active = FALSE WHERE is_active",
            )
            await conn.execute(
                """
                    UPDATE company_config
                    SET is_active = TRUE, activated_at = now()
                    WHERE revision_id = $1
                    """,
                revision_id,
            )
        logger.info(
            "company_config_revision_reactivated",
            revision_id=str(revision_id),
        )
