"""Simple forward-only SQL migrator.

Reads SQL files from the migrations directory in order, tracks applied
migrations in a ``schema_migrations`` table, and applies pending ones.
"""

from __future__ import annotations

from pathlib import Path
from typing import TYPE_CHECKING

from crewlet._logging import get_logger

if TYPE_CHECKING:
    from crewlet.db.client import Database

logger = get_logger("db.migrator")

MIGRATIONS_DIR = Path(__file__).parent / "migrations"

# The Tier B ``company_config`` table.  Applied on its own as the first
# phase of a two-phase migrate so the engine can read the active
# revision's embedding dimensions BEFORE the pgvector migrations
# (``002_episodes`` / ``007_agent_diary``) create their fixed-width
# vector columns.  See ``cli._connect_and_migrate_from_db``.
COMPANY_CONFIG_MIGRATION = "010_company_config.sql"

# The encrypted secret store.  Joins phase 1 because the boot snapshot is
# installed there — every ``${VAR}`` resolved afterwards (providers,
# transports, per-role MCP env) reads through it, including the ones the
# *rest* of the migration run would need if it ever grew a templated
# credential.  Self-contained DDL, so restricting the phase to these two
# files is safe.
SECRET_VALUES_MIGRATION = "016_secret_values.sql"


def _split_sql(sql: str) -> list[str]:
    """Split a SQL file into individual statements.

    Strips line comments and splits on ``;``, returning only non-empty
    statements.  This ensures each statement can be executed individually
    via the extended query protocol (required by asyncpg).
    """
    lines = [ln for ln in sql.splitlines() if not ln.strip().startswith("--")]
    clean = "\n".join(lines)
    return [s.strip() for s in clean.split(";") if s.strip()]


async def migrate(
    db: Database,
    *,
    template_vars: dict[str, str] | None = None,
    only: set[str] | None = None,
) -> None:
    """Apply all pending migrations.

    Creates the ``schema_migrations`` tracking table if it doesn't exist,
    then applies each ``.sql`` file that hasn't been recorded yet.

    ``template_vars`` is an optional dict of ``{placeholder: value}``
    pairs.  Occurrences of ``$key`` in SQL are replaced with the
    corresponding value before execution (e.g.
    ``{"embedding_dimensions": "1536"}``).

    ``only`` restricts the run to migration files whose name is in the
    set, regardless of their sort position.  Used for the first phase
    of the two-phase migrate (``only={COMPANY_CONFIG_MIGRATION}``) that
    bootstraps just the ``company_config`` table; a subsequent
    unrestricted call applies the rest (skipping the already-recorded
    files).  Inter-migration ordering is unaffected — ``company_config``
    is standalone (it references only itself).
    """
    replacements = template_vars or {}
    await db.execute(
        """
        CREATE TABLE IF NOT EXISTS schema_migrations (
            version TEXT PRIMARY KEY,
            applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
        )
        """
    )

    applied_rows = await db.execute(
        "SELECT version FROM schema_migrations ORDER BY version"
    )
    applied = {row["version"] for row in applied_rows}

    migration_files = sorted(MIGRATIONS_DIR.glob("*.sql"))

    for migration_file in migration_files:
        version = migration_file.name
        if only is not None and version not in only:
            continue
        if version in applied:
            logger.debug("migration_already_applied", version=version)
            continue

        logger.info("applying_migration", version=version)
        sql = migration_file.read_text()
        for key, value in replacements.items():
            sql = sql.replace(f"${key}", value)
        for stmt in _split_sql(sql):
            await db.execute(stmt)
        await db.execute(
            "INSERT INTO schema_migrations (version) VALUES ($1)",
            version,
        )
        logger.info("migration_applied", version=version)
