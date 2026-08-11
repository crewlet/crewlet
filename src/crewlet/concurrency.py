"""Concurrency controls — rate limiting and token budgets."""

from __future__ import annotations

import asyncio
import time

from pydantic import BaseModel

from crewlet._logging import get_logger

logger = get_logger("concurrency")


class RateLimiter:
    """Token bucket rate limiter for LLM API calls.

    Limits the number of calls within a time window.
    """

    def __init__(
        self,
        max_calls: int = 60,
        window_seconds: float = 60.0,
    ) -> None:
        self._max_calls = max_calls
        self._window = window_seconds
        self._calls: list[float] = []
        self._lock = asyncio.Lock()

    async def acquire(self) -> None:
        """Wait until a call is allowed under the rate limit."""
        while True:
            wait_time = 0.0
            async with self._lock:
                now = time.monotonic()
                # Remove expired entries
                self._calls = [t for t in self._calls if now - t < self._window]

                logger.debug(
                    "rate_limiter_acquiring",
                    used=len(self._calls),
                    max=self._max_calls,
                )

                if len(self._calls) < self._max_calls:
                    self._calls.append(now)
                    return

                # Calculate wait time but release the lock before sleeping
                wait_time = self._window - (now - self._calls[0])

            if wait_time > 0:
                logger.debug("rate_limiter_throttled", wait_seconds=round(wait_time, 1))
                await asyncio.sleep(wait_time)

    @property
    def available(self) -> int:
        """Number of calls available right now (approximate, not lock-protected)."""
        now = time.monotonic()
        active = sum(1 for t in list(self._calls) if now - t < self._window)
        return max(0, self._max_calls - active)


class TokenBudget(BaseModel):
    """Token budget tracking for agents or the org."""

    max_tokens: int = 0  # 0 = unlimited
    used_tokens: int = 0

    @property
    def remaining(self) -> int:
        if self.max_tokens == 0:
            return -1  # unlimited
        return max(0, self.max_tokens - self.used_tokens)

    @property
    def is_exhausted(self) -> bool:
        if self.max_tokens == 0:
            return False
        return self.used_tokens >= self.max_tokens

    def consume(self, tokens: int) -> bool:
        """Consume tokens from the budget.

        Returns True if tokens were available, False if budget exhausted.
        """
        logger.debug(
            "token_budget_consuming",
            tokens=tokens,
            used=self.used_tokens,
            max=self.max_tokens,
        )
        if self.max_tokens > 0 and self.used_tokens + tokens > self.max_tokens:
            return False
        self.used_tokens += tokens
        return True

    def reset(self) -> None:
        self.used_tokens = 0


class ConcurrencyController:
    """Manages concurrency limits for agent execution.

    Controls how many agents can be in the Working state
    simultaneously, with per-role limits.
    """

    def __init__(
        self,
        max_concurrent: int = 10,
        per_role_limits: dict[str, int] | None = None,
    ) -> None:
        self._max_concurrent = max_concurrent
        self._per_role_limits = per_role_limits or {}
        self._semaphore = asyncio.Semaphore(max_concurrent)
        self._role_semaphores: dict[str, asyncio.Semaphore] = {}

        for role, limit in self._per_role_limits.items():
            self._role_semaphores[role] = asyncio.Semaphore(limit)

    async def acquire(self, role: str = "") -> None:
        """Acquire a concurrency slot for the given role."""
        logger.debug("concurrency_acquiring", role=role)
        await self._semaphore.acquire()

        if role and role in self._role_semaphores:
            try:
                await self._role_semaphores[role].acquire()
            except BaseException:
                # Must release the global semaphore on any failure,
                # including CancelledError (a BaseException).
                self._semaphore.release()
                raise
        logger.debug("concurrency_acquired", role=role)

    def release(self, role: str = "") -> None:
        """Release a concurrency slot."""
        logger.debug("concurrency_released", role=role)
        self._semaphore.release()

        if role and role in self._role_semaphores:
            self._role_semaphores[role].release()

    @property
    def max_concurrent(self) -> int:
        return self._max_concurrent


class BudgetManager:
    """Manages token budgets for the org and per-agent."""

    def __init__(
        self,
        org_budget: int = 0,
        agent_budgets: dict[str, int] | None = None,
    ) -> None:
        self.org_budget = TokenBudget(max_tokens=org_budget)
        self._agent_budgets: dict[str, TokenBudget] = {}
        self._lock = asyncio.Lock()
        for agent_id, budget in (agent_budgets or {}).items():
            self._agent_budgets[agent_id] = TokenBudget(max_tokens=budget)

    def set_agent_budget(self, agent_id: str, max_tokens: int) -> None:
        self._agent_budgets[agent_id] = TokenBudget(max_tokens=max_tokens)

    def update_org_budget(self, max_tokens: int) -> None:
        """Update the org cap in place, preserving ``used_tokens``."""
        self.org_budget.max_tokens = max_tokens

    def update_agent_budget(self, agent_id: str, max_tokens: int) -> None:
        """Update a per-agent cap in place, preserving ``used_tokens``.

        Creates a fresh ``TokenBudget`` only when the agent had no
        prior budget — existing usage history survives the rewire.
        """
        existing = self._agent_budgets.get(agent_id)
        if existing is None:
            self._agent_budgets[agent_id] = TokenBudget(max_tokens=max_tokens)
        else:
            existing.max_tokens = max_tokens

    def drop_agent_budget(self, agent_id: str) -> None:
        """Remove a per-agent cap (e.g. when a role is deleted)."""
        self._agent_budgets.pop(agent_id, None)

    async def consume(self, agent_id: str, tokens: int) -> bool:
        """Consume tokens from both agent and org budgets.

        Returns True if tokens were available, False if any budget exhausted.
        """
        logger.debug("budget_consuming", tokens=tokens, agent_id=agent_id)
        async with self._lock:
            # Check org budget first
            if not self.org_budget.consume(tokens):
                logger.warning("budget_exhausted_org")
                return False

            # Check agent budget
            agent_budget = self._agent_budgets.get(agent_id)
            if agent_budget is not None and not agent_budget.consume(tokens):
                # Roll back org consumption (atomic under lock)
                logger.warning("budget_exhausted_agent", agent_id=agent_id)
                logger.debug(
                    "budget_rollback",
                    tokens=tokens,
                )
                self.org_budget.used_tokens -= tokens
                return False

            return True

    def get_agent_budget(self, agent_id: str) -> TokenBudget | None:
        return self._agent_budgets.get(agent_id)
