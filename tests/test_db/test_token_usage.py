"""Tests for TokenUsageRepository using a mocked Database."""

from __future__ import annotations

from unittest.mock import AsyncMock

import pytest

from crewlet.db.token_usage import TokenUsageRepository


@pytest.fixture
def mock_db() -> AsyncMock:
    db = AsyncMock()
    db.execute = AsyncMock(return_value=[])
    db.fetchrow = AsyncMock(return_value=None)
    db.fetchval = AsyncMock(return_value=None)
    return db


@pytest.fixture
def repo(mock_db: AsyncMock) -> TokenUsageRepository:
    return TokenUsageRepository(mock_db)


# -- get -------------------------------------------------------------------


async def test_get_unknown_handle_returns_zero(
    repo: TokenUsageRepository,
) -> None:
    result = await repo.get("agent-unknown")
    assert result == 0


async def test_get_returns_tokens_used(
    repo: TokenUsageRepository, mock_db: AsyncMock
) -> None:
    mock_db.fetchval.return_value = 1500
    result = await repo.get("agent-eng-1")
    assert result == 1500
    call_args = mock_db.fetchval.call_args
    assert "token_usage" in call_args[0][0]
    assert call_args[0][1] == "agent-eng-1"


# -- increment -------------------------------------------------------------


async def test_increment_calls_execute(
    repo: TokenUsageRepository, mock_db: AsyncMock
) -> None:
    await repo.increment("agent-eng-1", 500)
    mock_db.execute.assert_called_once()
    call_args = mock_db.execute.call_args
    query = call_args[0][0]
    assert "token_usage" in query
    assert "ON CONFLICT" in query
    assert call_args[0][1] == "agent-eng-1"
    assert call_args[0][2] == 500


async def test_increment_passes_correct_tokens(
    repo: TokenUsageRepository, mock_db: AsyncMock
) -> None:
    await repo.increment("agent-lead", 42)
    call_args = mock_db.execute.call_args
    assert call_args[0][1] == "agent-lead"
    assert call_args[0][2] == 42


# -- get_all ---------------------------------------------------------------


async def test_get_all_empty(repo: TokenUsageRepository) -> None:
    result = await repo.get_all()
    assert result == {}


async def test_get_all_returns_dict(
    repo: TokenUsageRepository, mock_db: AsyncMock
) -> None:
    mock_db.execute.return_value = [
        {"agent_handle": "agent-eng-1", "tokens_used": 1000},
        {"agent_handle": "agent-lead", "tokens_used": 2500},
    ]
    result = await repo.get_all()
    assert result == {"agent-eng-1": 1000, "agent-lead": 2500}


# -- roundtrip (mock level) -----------------------------------------------


async def test_increment_then_get(
    repo: TokenUsageRepository, mock_db: AsyncMock
) -> None:
    """Simulate increment followed by get via mock."""
    await repo.increment("agent-eng-1", 500)

    mock_db.fetchval.return_value = 500
    result = await repo.get("agent-eng-1")
    assert result == 500
