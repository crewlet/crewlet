"""Tests for the SQL migrator's ``only`` filter (two-phase migrate)."""

from __future__ import annotations

from typing import Any

from crewlet.db.migrator import COMPANY_CONFIG_MIGRATION, migrate


class _RecordingDB:
    """Minimal Database stand-in that records which migrations applied.

    The migrator issues raw ``db.execute`` calls; this captures the
    ``INSERT INTO schema_migrations`` versions to assert which files ran,
    and returns ``[]`` for the "already applied" SELECT so every file is
    pending.
    """

    def __init__(self) -> None:
        self.applied: list[str] = []

    async def execute(self, sql: str, *args: Any) -> Any:
        if sql.strip().startswith("SELECT version"):
            return []
        if "INSERT INTO schema_migrations" in sql:
            self.applied.append(args[0])
        return None


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

    assert phase_one == [{COMPANY_CONFIG_MIGRATION, SECRET_VALUES_MIGRATION}]
    assert inspect.iscoroutinefunction(_connect_and_migrate_from_db)
