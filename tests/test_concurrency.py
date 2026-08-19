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


# --- Exhaustion is a state, not a ratio -------------------------------


def test_a_refused_charge_marks_the_budget_exhausted():
    """``used >= max`` is essentially never true, so it cannot be the test.

    ``consume`` refuses a charge that would exceed the cap and
    increments nothing -- there is no partial charge.  A budget with a
    100-token cap charged in 60s therefore stalls at 60 and can never
    reach its own maximum, while being completely blocked.  Every
    ratio-based check of exhaustion is false forever.
    """
    budget = TokenBudget(max_tokens=100)
    assert budget.consume(60) is True
    assert budget.consume(60) is False
    assert budget.used_tokens == 60, "a refused charge incremented the counter"
    assert budget.used_tokens < budget.max_tokens
    assert budget.refused_at, "the refusal was not recorded"
    assert budget.is_exhausted is True


def test_an_unlimited_budget_is_never_exhausted():
    budget = TokenBudget(max_tokens=0)
    assert budget.consume(10**9) is True
    assert budget.is_exhausted is False


def test_reset_clears_the_refusal_too():
    budget = TokenBudget(max_tokens=10)
    budget.consume(50)
    assert budget.is_exhausted is True
    budget.reset()
    assert budget.is_exhausted is False
    assert budget.refused_at == ""


# --- Already-spent tokens ---------------------------------------------


def test_record_counts_spend_that_a_refusal_cannot_undo():
    budget = TokenBudget(max_tokens=100)
    assert budget.record(150) is True, "going over the cap was not reported"
    assert budget.used_tokens == 150, "already-spent tokens were dropped"
    assert budget.is_exhausted is True


@pytest.mark.asyncio
async def test_record_spend_moves_both_budgets():
    """A sandbox run's tokens are spent before the engine sees them.

    ``consume`` would refuse the charge and count nothing, leaving the
    meter reading low exactly when the cap is binding -- and, since the
    caller cannot un-spend them, not blocking anything either.
    """
    mgr = BudgetManager(org_budget=1000, agent_budgets={"a1": 100})
    assert await mgr.record_spend("a1", 400) is True
    assert mgr.org_budget.used_tokens == 400
    assert mgr.get_agent_budget("a1").used_tokens == 400
    assert mgr.get_agent_budget("a1").is_exhausted is True


@pytest.mark.asyncio
async def test_record_spend_for_an_uncapped_agent_still_charges_the_org():
    mgr = BudgetManager(org_budget=1000)
    assert await mgr.record_spend("nobody", 250) is False
    assert mgr.org_budget.used_tokens == 250


# --- The meter's identity and change hook ------------------------------


def test_each_manager_has_its_own_meter_identity():
    """A restart must be distinguishable from a drop.

    The counters are zeroed only by process death, so a consumer that
    merged or maximised across meters would pin a phantom high-water
    mark that no later report could clear.
    """
    assert BudgetManager().meter_id != BudgetManager().meter_id


@pytest.mark.asyncio
async def test_the_change_hook_fires_on_both_the_accepted_and_refused_path():
    """A refusal is the moment the cap bites and the most worth showing."""
    seen: list[str] = []
    mgr = BudgetManager(org_budget=100, on_change=seen.append)
    await mgr.consume("a1", 60)
    await mgr.consume("a1", 60)  # refused
    assert seen == ["a1", "a1"]


@pytest.mark.asyncio
async def test_the_change_hook_fires_on_a_cap_edit():
    """A live cap change must reach a screen without waiting for a token."""
    seen: list[str] = []
    mgr = BudgetManager(on_change=seen.append)
    mgr.set_agent_budget("a1", 10)
    mgr.update_agent_budget("a1", 20)
    mgr.update_org_budget(30)
    mgr.drop_agent_budget("a1")
    assert seen == ["a1", "a1", "", "a1"]


@pytest.mark.asyncio
async def test_a_raising_hook_never_breaks_accounting():
    def boom(_agent_id: str) -> None:
        raise RuntimeError("reporting is down")

    mgr = BudgetManager(org_budget=100, on_change=boom)
    assert await mgr.consume("a1", 10) is True
    assert mgr.org_budget.used_tokens == 10


@pytest.mark.asyncio
async def test_report_lists_only_metered_seats():
    """Absence means "no cap and no meter", not "a cap of zero"."""
    mgr = BudgetManager(org_budget=500, agent_budgets={"a1": 100})
    await mgr.consume("a1", 30)
    await mgr.consume("uncapped", 70)
    report = mgr.report()
    assert set(report["agents"]) == {"a1"}
    assert report["agents"]["a1"]["used_tokens"] == 30
    # The org figure is NOT the sum of the per-agent ones: it also
    # carries every uncapped seat's spend.
    assert report["org_used_tokens"] == 100
    assert report["meter_id"] == mgr.meter_id
