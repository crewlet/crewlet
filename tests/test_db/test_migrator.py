"""Tests for the SQL migrator: phase filtering, dimension deferral,
statement validation, and the advisory-locked transactional path."""

from __future__ import annotations

import re
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from typing import Any

import pytest

from crewlet.db.client import Database
from crewlet.db.migrator import (
    BOOTSTRAP_MIGRATIONS,
    COMPANY_CONFIG_MIGRATION,
    DIMENSION_MIGRATIONS,
    MIGRATIONS_DIR,
    MigrationError,
    migrate,
    pending_migrations,
)


class _RecordingDB:
    """Minimal SQLExecutor stand-in that records which migrations applied.

    The migrator issues raw ``db.execute`` calls; this captures the
    ``INSERT INTO schema_migrations`` versions to assert which files ran,
    plus every statement it was handed, and reports ``_already`` for the
    "already applied" SELECT (empty by default, so every file is pending).

    Not a :class:`~crewlet.db.client.Database`, so :func:`migrate` runs it
    on the unguarded path — the lock/transaction behaviour is exercised
    separately by :class:`_FakeDatabase`.
    """

    def __init__(self) -> None:
        self.applied: list[str] = []
        self.statements: list[str] = []
        self._already: set[str] = set()

    def mark_applied(self, versions: list[str]) -> None:
        """Persist a prior run's versions so the next call sees them."""
        self._already.update(versions)

    async def execute(self, sql: str, *args: Any) -> Any:
        if sql.strip().startswith("SELECT version"):
            return [{"version": v} for v in sorted(self._already)]
        self.statements.append(sql)
        if "INSERT INTO schema_migrations" in sql:
            self.applied.append(args[0])
            self._already.add(args[0])
        return None

    async def fetchrow(self, sql: str, *args: Any) -> Any:
        raise AssertionError("unused")

    async def fetchval(self, sql: str, *args: Any) -> Any:
        if "to_regclass" in sql:
            # The tracking table exists once anything has been applied.
            return "schema_migrations" if self._already else None
        raise AssertionError(f"unexpected fetchval: {sql}")


class _FakeDatabase(Database):
    """A :class:`Database` whose pool is faked, to observe lock/transaction use.

    Subclasses the real class because :func:`migrate` dispatches on
    ``isinstance(db, Database)`` to decide whether it may take the
    advisory lock and open transactions.
    """

    def __init__(self, *, fail_on: str | None = None) -> None:
        super().__init__()
        self.applied: list[str] = []
        self.locks: list[int] = []
        self.unlocks: list[int] = []
        self.transactions = 0
        self.lock_available = True
        self._already: set[str] = set()
        self._fail_on = fail_on

    async def execute(self, sql: str, *args: Any) -> Any:
        if sql.strip().startswith("SELECT version"):
            return [{"version": v} for v in sorted(self._already)]
        if "INSERT INTO schema_migrations" in sql:
            self.applied.append(args[0])
            self._already.add(args[0])
        return None

    async def fetchval(self, sql: str, *args: Any) -> Any:
        if "pg_try_advisory_lock" in sql:
            self.locks.append(args[0])
            return self.lock_available
        if "pg_advisory_unlock" in sql:
            self.unlocks.append(args[0])
            return True
        if "to_regclass" in sql:
            return "schema_migrations" if self._already else None
        if "pg_locks" in sql:
            return "4242"
        return None

    @asynccontextmanager
    async def acquire(self) -> AsyncIterator[Any]:
        yield self

    @asynccontextmanager
    async def transaction(self) -> AsyncIterator[Any]:
        self.transactions += 1
        if self._fail_on and self._fail_on not in self._already:
            raise RuntimeError("boom")
        yield self


async def test_migrate_only_restricts_to_named_migration() -> None:
    db = _RecordingDB()
    await migrate(db, only={COMPANY_CONFIG_MIGRATION})
    assert db.applied == [COMPANY_CONFIG_MIGRATION]


async def test_migrate_without_only_applies_all() -> None:
    db = _RecordingDB()
    await migrate(db, template_vars={"embedding_dimensions": "1536"})
    # More than just the company_config migration runs, and it's included.
    assert COMPANY_CONFIG_MIGRATION in db.applied
    assert len(db.applied) > 1


async def test_two_phase_migrate_skips_already_applied() -> None:
    """Phase 1 applies company_config; phase 2 applies the rest without
    re-running the already-recorded file."""
    db = _RecordingDB()
    await migrate(db, only={COMPANY_CONFIG_MIGRATION})

    # Phase 2 sees company_config as already applied.
    applied_so_far = set(db.applied)

    async def _execute(sql: str, *args: Any) -> Any:
        if sql.strip().startswith("SELECT version"):
            return [{"version": v} for v in applied_so_far]
        if "INSERT INTO schema_migrations" in sql:
            db.applied.append(args[0])
        return None

    db.execute = _execute  # type: ignore[assignment]
    await migrate(db, template_vars={"embedding_dimensions": "3072"})

    # company_config recorded exactly once across both phases.
    assert db.applied.count(COMPANY_CONFIG_MIGRATION) == 1


def test_secret_values_is_in_the_boot_phase_one_set():
    """``_connect_and_migrate_from_db`` must create ``secret_values`` in
    phase 1: the secret snapshot is installed there, before the first
    Tier B ``${VAR}`` is resolved, so the table has to exist by then."""
    import asyncio
    import inspect
    from unittest.mock import AsyncMock, MagicMock

    from crewlet.cli import _connect_and_migrate_from_db
    from crewlet.config import SecretsConfig

    # The constant must name a file that actually ships.
    from crewlet.db.migrator import (
        COMPANY_CONFIG_MIGRATION,
        MIGRATIONS_DIR,
        SECRET_VALUES_MIGRATION,
    )

    assert (MIGRATIONS_DIR / SECRET_VALUES_MIGRATION).exists()

    phase_one: list[set[str] | None] = []

    async def _connect(_dsn):
        db = AsyncMock()
        db.execute = AsyncMock(return_value=[])
        return db

    async def _migrate(_db, *, template_vars=None, only=None):
        if only is not None:
            phase_one.append(only)

    store = MagicMock()
    store.get_active = AsyncMock(return_value=None)

    import crewlet.db.client as client_mod
    import crewlet.db.company_config as cc_mod
    import crewlet.db.migrator as mig_mod

    orig_connect = client_mod.Database.connect
    orig_migrate = mig_mod.migrate
    orig_store = cc_mod.CompanyConfigStore
    client_mod.Database.connect = _connect
    mig_mod.migrate = _migrate
    cc_mod.CompanyConfigStore = lambda _db: store
    try:
        bootstrap = MagicMock()
        bootstrap.providers.database.dsn = "postgresql://x/y"
        bootstrap.secrets = SecretsConfig()
        asyncio.run(_connect_and_migrate_from_db(bootstrap))
    finally:
        client_mod.Database.connect = orig_connect
        mig_mod.migrate = orig_migrate
        cc_mod.CompanyConfigStore = orig_store

    assert phase_one == [set(BOOTSTRAP_MIGRATIONS)]
    assert {COMPANY_CONFIG_MIGRATION, SECRET_VALUES_MIGRATION} <= phase_one[0]
    assert inspect.iscoroutinefunction(_connect_and_migrate_from_db)


# ---------------------------------------------------------------------------
# Dimension deferral — the fix for the silent pgvector-width corruption
# ---------------------------------------------------------------------------


async def test_run_stops_at_the_first_dimension_migration_without_a_width() -> None:
    """No width means STOP, never guess.

    ``vector(N)`` is fixed at creation and the sequence is forward-only,
    so a guessed 1536 against a 3072-dim model makes every diary/episode
    insert raise forever — swallowed by ReflectEngine, so the learning
    subsystem just goes quiet.
    """
    db = _RecordingDB()
    await migrate(db)

    assert "001_initial.sql" in db.applied
    assert not (set(db.applied) & DIMENSION_MIGRATIONS)


async def test_deferral_stops_rather_than_skipping_ahead() -> None:
    """Forward-only ordering means later files may touch earlier objects:
    011 runs ``UPDATE episodes``, the table 002 creates. Skipping 002 and
    continuing would fail — or record a version whose object never existed.
    """
    db = _RecordingDB()
    await migrate(db)

    assert "011_drop_ask_colleague_outcome.sql" not in db.applied
    # Everything applied sorts before the first deferred migration.
    first_deferred = min(DIMENSION_MIGRATIONS)
    assert all(v < first_deferred for v in db.applied)


async def test_supplying_a_width_completes_the_deferred_tail() -> None:
    db = _RecordingDB()
    await migrate(db)
    deferred_run = list(db.applied)

    db.mark_applied(deferred_run)
    await migrate(db, template_vars={"embedding_dimensions": "3072"})

    assert set(DIMENSION_MIGRATIONS) <= set(db.applied)
    assert "011_drop_ask_colleague_outcome.sql" in db.applied
    # Nothing was applied twice across the two runs.
    assert len(db.applied) == len(set(db.applied))


async def test_width_is_substituted_into_the_vector_column() -> None:
    db = _RecordingDB()
    await migrate(db, template_vars={"embedding_dimensions": "3072"})
    assert any("vector(3072)" in sql for sql in db.statements)
    assert not any("$embedding_dimensions" in sql for sql in db.statements)


async def test_bootstrap_phase_is_dimension_independent() -> None:
    """The API applies only this phase, so it must never be blocked by —
    or contribute to — the pgvector width decision."""
    db = _RecordingDB()
    await migrate(db, only=set(BOOTSTRAP_MIGRATIONS))
    assert set(db.applied) == set(BOOTSTRAP_MIGRATIONS)


async def test_pending_migrations_reports_the_deferred_tail() -> None:
    db = _RecordingDB()
    applied = await migrate(db)
    db.mark_applied(applied)

    pending = await pending_migrations(db)
    assert pending == []  # nothing more is applicable without a width

    with_width = await pending_migrations(
        db, template_vars={"embedding_dimensions": "1536"}
    )
    assert set(DIMENSION_MIGRATIONS) <= set(with_width)


# ---------------------------------------------------------------------------
# Statement validation
# ---------------------------------------------------------------------------


def test_check_sql_rejects_dollar_quoting() -> None:
    from crewlet.db.migrator import _check_sql

    with pytest.raises(MigrationError, match="dollar-quoted"):
        _check_sql("099_x.sql", ["CREATE FUNCTION f() AS $$ SELECT 1; $$ LANGUAGE sql"])


def test_check_sql_rejects_an_unsubstituted_template_var() -> None:
    from crewlet.db.migrator import _check_sql

    with pytest.raises(MigrationError, match="unsubstituted"):
        _check_sql("099_x.sql", ["CREATE TABLE t (v vector($embedding_dimensions))"])


def test_shipped_migrations_pass_validation_at_a_known_width() -> None:
    """Every migration we ship must survive its own splitter."""
    from crewlet.db.migrator import _check_sql, _split_sql

    for path in sorted(MIGRATIONS_DIR.glob("*.sql")):
        sql = path.read_text().replace("$embedding_dimensions", "1536")
        _check_sql(path.name, _split_sql(sql))


# ---------------------------------------------------------------------------
# Advisory lock + per-file transactions
# ---------------------------------------------------------------------------


async def test_database_backed_run_takes_the_lock_and_wraps_each_file() -> None:
    """Three entry points call migrate() and the split-deployment docs say
    to start two of them together. The lock is what stops that being a race
    over non-idempotent DDL."""
    from crewlet.db.migrator import MIGRATION_LOCK_KEY

    db = _FakeDatabase()
    applied = await migrate(db, template_vars={"embedding_dimensions": "1536"})

    assert db.locks == [MIGRATION_LOCK_KEY]
    assert db.unlocks == [MIGRATION_LOCK_KEY]
    # One transaction per applied file, and every file was applied inside one.
    assert db.transactions == len(applied)


async def test_lock_is_released_when_a_migration_fails() -> None:
    """A failed migration must not wedge every future boot behind a lock
    this process still holds."""
    db = _FakeDatabase(fail_on="001_initial.sql")
    with pytest.raises(RuntimeError, match="boom"):
        await migrate(db, template_vars={"embedding_dimensions": "1536"})
    assert db.unlocks == db.locks


# ---------------------------------------------------------------------------
# The learning_health repair
# ---------------------------------------------------------------------------


def test_repair_migration_matches_the_canonical_view_definition() -> None:
    """018 exists to heal databases where racing migrators applied 005 and
    009 out of order. It only works if it carries 009's definition
    verbatim — so drift between the two is a build failure, not a
    production mystery."""

    def _view_sql(name: str) -> str:
        text = (MIGRATIONS_DIR / name).read_text()
        match = re.search(
            r"CREATE OR REPLACE VIEW learning_health AS.*?;", text, re.DOTALL
        )
        assert match, f"{name} has no learning_health view"
        return " ".join(match.group(0).split())

    assert _view_sql("020_learning_health_repair.sql") == _view_sql(
        "009_skill_curator.sql"
    )


def test_repair_migration_sorts_after_the_definitions_it_repairs() -> None:
    ordered = sorted(p.name for p in MIGRATIONS_DIR.glob("*.sql"))
    assert ordered.index("020_learning_health_repair.sql") > ordered.index(
        "009_skill_curator.sql"
    )


# ---------------------------------------------------------------------------
# The migrator is the only author of schema
# ---------------------------------------------------------------------------


def test_no_test_hand_builds_a_table_the_migrations_own() -> None:
    """A test that re-creates a migrated table is a schema mirror nothing
    keeps in step, and it fails in the worst direction: the copy goes
    stale, the tests that read it break, and the engine's real SQL —
    which was correct all along — is what looks broken.

    That is not hypothetical. ``tests/test_learning/test_episode_work_key``
    dropped ``episodes`` and built its own, and migration 033's new
    ``conversation_key`` column turned every test in it red while the
    store was right. Its teardown left the real table dropped, so
    anything later in the same session that touched it failed too.

    Against a real ``CREWLET_TEST_DSN`` the migrator has already built
    every table. Clean rows between tests; never the schema.
    """
    owned = set()
    for path in MIGRATIONS_DIR.glob("*.sql"):
        owned.update(
            re.findall(r"CREATE TABLE (?:IF NOT EXISTS )?([a-z_]+)", path.read_text())
        )
    assert "episodes" in owned, "the table-name scan found nothing to protect"

    root = MIGRATIONS_DIR.parents[3] / "tests"
    offenders: list[str] = []
    for path in sorted(root.rglob("*.py")):
        text = path.read_text()
        for statement in re.findall(
            r"(?:CREATE|DROP) TABLE (?:IF NOT EXISTS |IF EXISTS )?([a-z_]+)", text
        ):
            if statement in owned:
                offenders.append(f"{path.relative_to(root.parent)}: {statement}")
    assert not offenders, (
        "these tests build their own copy of a migrated table: " + ", ".join(offenders)
    )
