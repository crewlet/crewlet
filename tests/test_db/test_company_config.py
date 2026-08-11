"""Tests for CompanyConfigStore using a mocked Database."""

from __future__ import annotations

import json
from datetime import UTC, datetime
from unittest.mock import AsyncMock, MagicMock
from uuid import UUID, uuid4

import pytest

from crewlet.db.company_config import CompanyConfigRevision, CompanyConfigStore


def _make_async_ctx(value):
    """Build an async context manager that yields `value`."""
    ctx = MagicMock()
    ctx.__aenter__ = AsyncMock(return_value=value)
    ctx.__aexit__ = AsyncMock(return_value=None)
    return ctx


@pytest.fixture
def mock_conn() -> AsyncMock:
    conn = AsyncMock()
    conn.execute = AsyncMock(return_value=None)
    conn.transaction = MagicMock(return_value=_make_async_ctx(None))
    return conn


@pytest.fixture
def mock_pool(mock_conn: AsyncMock) -> MagicMock:
    pool = MagicMock()
    pool.acquire = MagicMock(return_value=_make_async_ctx(mock_conn))
    return pool


@pytest.fixture
def mock_db(mock_pool: MagicMock) -> AsyncMock:
    db = AsyncMock()
    db.execute = AsyncMock(return_value=[])
    db.fetchrow = AsyncMock(return_value=None)
    db.fetchval = AsyncMock(return_value=None)
    db._require_pool = MagicMock(return_value=mock_pool)
    return db


@pytest.fixture
def store(mock_db: AsyncMock) -> CompanyConfigStore:
    return CompanyConfigStore(mock_db)


# -- insert_active ----------------------------------------------------


async def test_insert_active_runs_deactivate_then_insert_in_transaction(
    store: CompanyConfigStore, mock_conn: AsyncMock
) -> None:
    payload = {"name": "Test Co", "mission": "test"}
    revision_id = await store.insert_active(
        payload,
        created_by="alice",
        source="api",
        summary="initial",
    )

    assert isinstance(revision_id, UUID)
    # Both UPDATE-deactivate and INSERT ran on the same connection.
    assert mock_conn.execute.call_count == 2
    deactivate_call = mock_conn.execute.call_args_list[0]
    insert_call = mock_conn.execute.call_args_list[1]
    assert "UPDATE company_config SET is_active = FALSE" in deactivate_call[0][0]
    assert "INSERT INTO company_config" in insert_call[0][0]
    # Args order: revision_id, parent, created_by, source, summary, payload-json
    assert insert_call[0][1] == revision_id
    assert insert_call[0][2] is None  # parent_revision_id
    assert insert_call[0][3] == "alice"
    assert insert_call[0][4] == "api"
    assert insert_call[0][5] == "initial"
    assert json.loads(insert_call[0][6]) == payload


async def test_insert_active_with_parent_revision_id(
    store: CompanyConfigStore, mock_conn: AsyncMock
) -> None:
    parent = uuid4()
    await store.insert_active(
        {"name": "X"},
        created_by="bob",
        source="cli",
        summary="next",
        parent_revision_id=parent,
    )
    insert_call = mock_conn.execute.call_args_list[1]
    assert insert_call[0][2] == parent


# -- get_active --------------------------------------------------------


async def test_get_active_none_when_unconfigured(
    store: CompanyConfigStore,
) -> None:
    result = await store.get_active()
    assert result is None


async def test_get_active_returns_revision(
    store: CompanyConfigStore, mock_db: AsyncMock
) -> None:
    rev_id = uuid4()
    now = datetime.now(UTC)
    mock_db.fetchrow.return_value = {
        "revision_id": rev_id,
        "parent_revision_id": None,
        "created_at": now,
        "created_by": "alice",
        "source": "api",
        "summary": "initial",
        "payload": {"name": "Test"},
        "is_active": True,
        "activated_at": now,
    }
    rev = await store.get_active()
    assert rev is not None
    assert isinstance(rev, CompanyConfigRevision)
    assert rev.revision_id == rev_id
    assert rev.is_active is True
    assert rev.payload == {"name": "Test"}


async def test_get_active_decodes_jsonb_string_payload(
    store: CompanyConfigStore, mock_db: AsyncMock
) -> None:
    rev_id = uuid4()
    now = datetime.now(UTC)
    mock_db.fetchrow.return_value = {
        "revision_id": rev_id,
        "parent_revision_id": None,
        "created_at": now,
        "created_by": "cli",
        "source": "cli",
        "summary": "x",
        "payload": '{"name": "FromString"}',  # asyncpg may return raw JSON
        "is_active": True,
        "activated_at": now,
    }
    rev = await store.get_active()
    assert rev is not None
    assert rev.payload == {"name": "FromString"}


# -- get_revision -----------------------------------------------------


async def test_get_revision_not_found(
    store: CompanyConfigStore, mock_db: AsyncMock
) -> None:
    rev = await store.get_revision(uuid4())
    assert rev is None
    call_args = mock_db.fetchrow.call_args
    assert "WHERE revision_id = $1" in call_args[0][0]


# -- list_revisions ---------------------------------------------------


async def test_list_revisions_empty(store: CompanyConfigStore) -> None:
    result = await store.list_revisions()
    assert result == []


async def test_list_revisions_orders_by_created_at_desc(
    store: CompanyConfigStore, mock_db: AsyncMock
) -> None:
    now = datetime.now(UTC)
    mock_db.execute.return_value = [
        {
            "revision_id": uuid4(),
            "parent_revision_id": None,
            "created_at": now,
            "created_by": "alice",
            "source": "api",
            "summary": f"r{i}",
            "payload": {},
            "is_active": i == 0,
            "activated_at": now if i == 0 else None,
        }
        for i in range(3)
    ]
    result = await store.list_revisions(limit=10, offset=0)
    assert len(result) == 3
    # Query carries the ORDER BY clause.
    query = mock_db.execute.call_args[0][0]
    assert "ORDER BY created_at DESC" in query
    assert mock_db.execute.call_args[0][1] == 10
    assert mock_db.execute.call_args[0][2] == 0


# -- has_any ---------------------------------------------------------


async def test_has_any_false_when_unconfigured(
    store: CompanyConfigStore,
) -> None:
    assert await store.has_any() is False


async def test_has_any_true_when_active_present(
    store: CompanyConfigStore, mock_db: AsyncMock
) -> None:
    mock_db.fetchval.return_value = 1
    assert await store.has_any() is True


# -- activate (revert path) ------------------------------------------


async def test_activate_runs_deactivate_then_activate_in_transaction(
    store: CompanyConfigStore, mock_conn: AsyncMock
) -> None:
    target = uuid4()
    await store.activate(target)
    assert mock_conn.execute.call_count == 2
    deactivate_call = mock_conn.execute.call_args_list[0]
    reactivate_call = mock_conn.execute.call_args_list[1]
    assert "is_active = FALSE" in deactivate_call[0][0]
    assert "is_active = TRUE" in reactivate_call[0][0]
    assert reactivate_call[0][1] == target
