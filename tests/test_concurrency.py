"""Tests for concurrency controls and token budgets."""

import asyncio

import pytest

from crewlet.concurrency import (
    BudgetManager,
    ConcurrencyController,
    RateLimiter,
    TokenBudget,
)

# --- TokenBudget tests ---


def test_token_budget_unlimited():
    budget = TokenBudget(max_tokens=0)
    assert not budget.is_exhausted
    assert budget.remaining == -1
    assert budget.consume(1000) is True


def test_token_budget_limited():
    budget = TokenBudget(max_tokens=100)
    assert budget.consume(60) is True
    assert budget.remaining == 40
    assert budget.consume(50) is False  # Would exceed
    assert budget.remaining == 40


def test_token_budget_exhausted():
    budget = TokenBudget(max_tokens=100)
    budget.consume(100)
    assert budget.is_exhausted
    assert budget.remaining == 0


def test_token_budget_reset():
    budget = TokenBudget(max_tokens=100)
    budget.consume(100)
    budget.reset()
    assert budget.used_tokens == 0
    assert budget.remaining == 100


# --- RateLimiter tests ---


@pytest.mark.asyncio
async def test_rate_limiter_allows_under_limit():
    limiter = RateLimiter(max_calls=5, window_seconds=1.0)
    for _ in range(5):
        await limiter.acquire()
    assert limiter.available == 0


@pytest.mark.asyncio
async def test_rate_limiter_available():
    limiter = RateLimiter(max_calls=10, window_seconds=1.0)
    assert limiter.available == 10
    await limiter.acquire()
    assert limiter.available == 9


@pytest.mark.asyncio
async def test_rate_limiter_blocks_when_exhausted():
    """When the limit is hit, acquire should block until a slot opens."""
    limiter = RateLimiter(max_calls=2, window_seconds=0.1)
    await limiter.acquire()
    await limiter.acquire()
    assert limiter.available == 0

    # This acquire should block briefly then succeed after the window
    await asyncio.wait_for(limiter.acquire(), timeout=1.0)
    # Should have completed (window expired)


# --- ConcurrencyController tests ---


@pytest.mark.asyncio
async def test_concurrency_acquire_release():
    ctrl = ConcurrencyController(max_concurrent=2)
    await ctrl.acquire()
    await ctrl.acquire()
    # Both slots taken — release one
    ctrl.release()
    await ctrl.acquire()  # Should succeed
    ctrl.release()
    ctrl.release()


@pytest.mark.asyncio
async def test_concurrency_per_role():
    ctrl = ConcurrencyController(
        max_concurrent=10,
        per_role_limits={"Engineer": 2},
    )
    await ctrl.acquire("Engineer")
    await ctrl.acquire("Engineer")
    ctrl.release("Engineer")
    await ctrl.acquire("Engineer")  # Should succeed
    ctrl.release("Engineer")
    ctrl.release("Engineer")


@pytest.mark.asyncio
async def test_concurrency_max():
    assert ConcurrencyController(max_concurrent=5).max_concurrent == 5


@pytest.mark.asyncio
async def test_concurrency_role_cancel_releases_global():
    """If role semaphore acquire is cancelled, global slot is released."""
    ctrl = ConcurrencyController(
        max_concurrent=5,
        per_role_limits={"Engineer": 1},
    )

    # Fill the role semaphore
    await ctrl.acquire("Engineer")

    # Try to acquire another slot for the same role, but cancel it
    async def acquire_and_cancel():
        await ctrl.acquire("Engineer")

    task = asyncio.create_task(acquire_and_cancel())
    await asyncio.sleep(0.01)  # Let it block on role semaphore
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task

    # Global semaphore should still have 4 slots (only 1 actually held)
    # Try to acquire 4 more global-only slots
    for _ in range(4):
        await ctrl.acquire()
    ctrl.release()
    ctrl.release()
    ctrl.release()
    ctrl.release()
    ctrl.release("Engineer")


# --- BudgetManager tests ---


@pytest.mark.asyncio
async def test_budget_manager_consume():
    mgr = BudgetManager(org_budget=1000)
    assert await mgr.consume("a1", 500) is True
    assert mgr.org_budget.used_tokens == 500


@pytest.mark.asyncio
async def test_budget_manager_org_exhausted():
    mgr = BudgetManager(org_budget=100)
    assert await mgr.consume("a1", 60) is True
    assert await mgr.consume("a1", 60) is False  # Exceeds org budget


@pytest.mark.asyncio
async def test_budget_manager_agent_exhausted():
    mgr = BudgetManager(org_budget=10000, agent_budgets={"a1": 100})
    assert await mgr.consume("a1", 60) is True
    assert await mgr.consume("a1", 60) is False  # Exceeds agent budget
    # Org budget should be rolled back
    assert mgr.org_budget.used_tokens == 60


def test_budget_manager_set_agent():
    mgr = BudgetManager(org_budget=10000)
    mgr.set_agent_budget("a1", 500)
    assert mgr.get_agent_budget("a1") is not None
    assert mgr.get_agent_budget("a1").max_tokens == 500


@pytest.mark.asyncio
async def test_budget_manager_unlimited_org():
    mgr = BudgetManager(org_budget=0)
    assert await mgr.consume("a1", 999999) is True
