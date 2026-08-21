"""Simple forward-only SQL migrator.

Reads SQL files from the migrations directory in order, tracks applied
migrations in a ``schema_migrations`` table, and applies pending ones.

Three properties make it safe to run from more than one process:

- **One advisory lock guards the whole run** (see
  :func:`migrate`).  Concurrent callers serialize instead of racing
  non-idempotent DDL.
- **Each file applies inside one transaction**, together with its
  ``schema_migrations`` row — so a file is either fully applied and
  recorded, or neither.
- **The run stops rather than skips** when it reaches a migration it
  cannot apply yet (see :data:`DIMENSION_MIGRATIONS`).  Forward-only
  ordering means a later file may depend on an earlier one
  (``011_drop_ask_colleague_outcome.sql`` updates the ``episodes`` table
  that ``002`` creates), so skipping ahead would fail or, worse, record a
  version whose object never existed.
"""

from __future__ import annotations

from pathlib import Path

from crewlet._logging import get_logger
from crewlet.db.client import Database, SQLExecutor

logger = get_logger("db.migrator")

MIGRATIONS_DIR = Path(__file__).parent / "migrations"

# Advisory-lock key for the migration run.  An arbitrary but FIXED
# int64 — every process must pick the same number for the lock to mean
# anything.  Derived once from "crewlet.migrate" and pinned here rather
# than hashed at runtime, because Python's string hash is randomized per
# process and would hand each caller a different lock.
MIGRATION_LOCK_KEY = 0x63726577_6C65746D  # 'crew' 'letm'

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

# Cross-process resource ownership.  Joins the bootstrap phase because a
# lease is infrastructure, not company data: it has nothing to do with
# the embedding width, and gating it behind one would leave the table
# absent on exactly the databases (no active revision yet) where two
# processes are most likely to be racing each other.  Self-contained DDL.
LEASES_MIGRATION = "019_leases.sql"

# The bootstrap phase: the self-contained tables a process needs before
# it can read config, resolve a secret, or claim a resource.  None is
# dimension-dependent, so this phase is always applicable — which is what
# lets a process bring up an unconfigured database without ever touching
# the pgvector migrations.
BOOTSTRAP_MIGRATIONS = frozenset(
    {COMPANY_CONFIG_MIGRATION, SECRET_VALUES_MIGRATION, LEASES_MIGRATION}
)

# Migrations whose DDL bakes in the embedding width via the
# ``$embedding_dimensions`` template variable.  ``vector(N)`` columns are
# fixed at creation and the sequence is forward-only, so applying these
# at a guessed width is irreversible: every later diary/episode insert
# raises against the mismatched column, forever.  They therefore apply
# ONLY when the caller supplies a real width, and the run STOPS at the
# first of them otherwise.
DIMENSION_MIGRATIONS = frozenset({"002_episodes.sql", "007_agent_diary.sql"})

DIMENSION_VAR = "embedding_dimensions"


class MigrationError(RuntimeError):
    """A migration file could not be applied."""


def _split_sql(sql: str) -> list[str]:
    """Split a SQL file into individual statements.

    Strips line comments and splits on ``;``, returning only non-empty
    statements.  This ensures each statement can be executed individually
    via the extended query protocol (required by asyncpg).

    The splitter is deliberately naive, so it validates its own
    assumptions rather than mis-parsing silently: see :func:`_check_sql`.
    """
    lines = [ln for ln in sql.splitlines() if not ln.strip().startswith("--")]
    clean = "\n".join(lines)
    return [s.strip() for s in clean.split(";") if s.strip()]


def _check_sql(version: str, statements: list[str]) -> None:
    """Reject SQL the naive splitter would silently mangle.

    Called AFTER template substitution, so any surviving ``$`` means one
    of two things the splitter cannot handle:

    - **Dollar-quoting** (``$$ … $$``, a ``DO`` block or ``CREATE
      FUNCTION`` body).  Splitting on ``;`` cuts straight through it.
    - **An unsubstituted template variable**, i.e. a caller that forgot a
      key — which would otherwise reach PostgreSQL as a syntax error far
      from its cause, or worse, as a valid-looking positional parameter.

    Failing loudly here costs a future author one clear error; failing
    silently costs a corrupted schema with a recorded version row that
    makes it unreachable by any re-run.
    """
    for stmt in statements:
        if "$" not in stmt:
            continue
        detail = (
            "dollar-quoted block ($$) — the ';' splitter cannot parse it"
            if "$$" in stmt
            else "unsubstituted template variable"
        )
        raise MigrationError(
            f"migration {version} contains a {detail}. "
            f"Extend _split_sql (or pass the missing template var) "
            f"before shipping this migration."
        )


async def _applied_versions(db: SQLExecutor) -> set[str]:
    rows = await db.execute("SELECT version FROM schema_migrations ORDER BY version")
    return {row["version"] for row in rows}


async def _tracking_table_exists(db: SQLExecutor) -> bool:
    """Whether ``schema_migrations`` exists, without creating it."""
    try:
        return bool(await db.fetchval("SELECT to_regclass('schema_migrations')"))
    except Exception:
        return False


async def _ensure_tracking_table(db: SQLExecutor) -> None:
    await db.execute(
        """
        CREATE TABLE IF NOT EXISTS schema_migrations (
            version TEXT PRIMARY KEY,
            applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
        )
        """
    )


async def _apply_one(
    db: SQLExecutor,
    migration_file: Path,
    replacements: dict[str, str],
) -> None:
    version = migration_file.name
    sql = migration_file.read_text()
    for key, value in replacements.items():
        sql = sql.replace(f"${key}", value)
    statements = _split_sql(sql)
    _check_sql(version, statements)
    for stmt in statements:
        await db.execute(stmt)
    await db.execute(
        "INSERT INTO schema_migrations (version) VALUES ($1)",
        version,
    )


async def migrate(
    db: SQLExecutor,
    *,
    template_vars: dict[str, str] | None = None,
    only: set[str] | None = None,
) -> list[str]:
    """Apply all pending migrations and return the versions applied.

    Creates the ``schema_migrations`` tracking table if it doesn't exist,
    then applies each ``.sql`` file that hasn't been recorded yet.

    ``template_vars`` is an optional dict of ``{placeholder: value}``
    pairs.  Occurrences of ``$key`` in SQL are replaced with the
    corresponding value before execution (e.g.
    ``{"embedding_dimensions": "1536"}``).  Omitting
    ``embedding_dimensions`` is legitimate and meaningful: the run then
    STOPS when it reaches the first migration that needs it, leaving that
    file and everything after it pending for a later call that knows the
    width.  It is never guessed — see :data:`DIMENSION_MIGRATIONS`.

    ``only`` restricts the run to migration files whose name is in the
    set, regardless of their sort position.  Used for the bootstrap phase
    (``only=BOOTSTRAP_MIGRATIONS``) that creates just ``company_config``
    and ``secret_values``; a subsequent unrestricted call applies the rest
    (skipping the already-recorded files).  Inter-migration ordering is
    unaffected — both are standalone.

    When ``db`` is a :class:`~crewlet.db.client.Database`, the whole run
    is serialized behind an advisory lock and each file applies inside its
    own transaction.  Any other executor (a held connection, a test
    double) runs unguarded — the caller has taken responsibility for
    isolation.
    """
    if isinstance(db, Database):
        async with db.advisory_lock(MIGRATION_LOCK_KEY):
            return await _migrate_locked(db, template_vars, only, transactional=True)
    return await _migrate_locked(db, template_vars, only, transactional=False)


async def _migrate_locked(
    db: SQLExecutor,
    template_vars: dict[str, str] | None,
    only: set[str] | None,
    *,
    transactional: bool,
) -> list[str]:
    replacements = template_vars or {}
    have_dimensions = DIMENSION_VAR in replacements

    await _ensure_tracking_table(db)
    applied = await _applied_versions(db)

    done: list[str] = []
    for migration_file in sorted(MIGRATIONS_DIR.glob("*.sql")):
        version = migration_file.name
        if only is not None and version not in only:
            continue
        if version in applied:
            logger.debug("migration_already_applied", version=version)
            continue

        if version in DIMENSION_MIGRATIONS and not have_dimensions:
            # Stop, do not skip: later migrations may touch the objects
            # this one creates (011 updates 002's `episodes` table).
            logger.info(
                "migration_run_deferred",
                version=version,
                reason="embedding width unknown until a company config is active",
            )
            break

        logger.info("applying_migration", version=version)
        if transactional:
            async with db.transaction() as tx:  # type: ignore[attr-defined]
                await _apply_one(tx, migration_file, replacements)
        else:
            await _apply_one(db, migration_file, replacements)
        done.append(version)
        logger.info("migration_applied", version=version)

    return done


async def pending_migrations(
    db: SQLExecutor, *, template_vars: dict[str, str] | None = None
) -> list[str]:
    """Versions that would apply on the next unrestricted :func:`migrate`.

    Read-only, unlike :func:`migrate` — it creates nothing, so it is safe
    to call from a readiness probe or ``crewlet migrate --check`` while
    another process is mid-migration.  (Creating the tracking table here
    would not merely contradict "applies nothing": PostgreSQL's ``CREATE
    TABLE IF NOT EXISTS`` is not concurrency-safe, so racing a booting
    engine would fail on a ``pg_type_typname_nsp_index`` uniqueness
    violation naming nothing an operator would recognise.)
    """
    if not await _tracking_table_exists(db):
        applied: set[str] = set()
    else:
        applied = await _applied_versions(db)
    have_dimensions = DIMENSION_VAR in (template_vars or {})
    out: list[str] = []
    for migration_file in sorted(MIGRATIONS_DIR.glob("*.sql")):
        version = migration_file.name
        if version in applied:
            continue
        if version in DIMENSION_MIGRATIONS and not have_dimensions:
            break
        out.append(version)
    return out
