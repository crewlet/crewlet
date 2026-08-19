"""Concurrency controls — rate limiting and token budgets."""

from __future__ import annotations

import asyncio
import time
from collections.abc import Callable
from datetime import UTC, datetime
from uuid import uuid4

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
    """Token budget tracking for agents or the org.

    ``used_tokens`` accumulates for the lifetime of the object, which is
    the lifetime of the engine process -- there is no window and no
    decay.  Anything that renders it has to say so, and must never be
    compared against a figure covering a different span.
    """

    max_tokens: int = 0  # 0 = unlimited
    used_tokens: int = 0
    refused_at: str = ""
    """When this budget last refused a charge, ISO-8601, or ``""``.

    This -- not ``used_tokens >= max_tokens`` -- is what "the cap is
    biting" means.  ``consume`` refuses a charge that would exceed and
    increments nothing, so a budget with a 100k cap being charged in 3k
    rounds stalls at ~99k and can never reach its own maximum: every
    ratio-based test of exhaustion is false forever while the agent is
    in fact completely blocked.
    """

    @property
    def remaining(self) -> int:
        if self.max_tokens == 0:
            return -1  # unlimited
        return max(0, self.max_tokens - self.used_tokens)

    @property
    def is_exhausted(self) -> bool:
        """Whether this budget is refusing charges.

        Keyed on the last refusal rather than on ``used >= max``.  A
        partial charge is never made (see ``consume``), so the counter
        stops short of the cap by the size of the round that could not
        fit -- ``used >= max`` is reachable only when a charge lands
        exactly on the cap, which is to say essentially never.
        """
        if self.max_tokens == 0:
            return False
        return bool(self.refused_at) or self.used_tokens >= self.max_tokens

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
            self.refused_at = datetime.now(UTC).isoformat()
            return False
        self.used_tokens += tokens
        return True

    def record(self, tokens: int) -> bool:
        """Record tokens that were ALREADY spent, whatever the cap says.

        The counterpart to :meth:`consume`, for spend a refusal cannot
        undo -- a detached sandbox run has already burned its tokens by
        the time the engine accounts for them.  Refusing such a charge
        does not un-spend anything; it only makes the meter read low,
        and it reads low precisely when the cap is binding, which is the
        moment the figure matters most.

        Returns whether this recording put the budget over its cap.
        """
        self.used_tokens += tokens
        over = self.max_tokens > 0 and self.used_tokens > self.max_tokens
        if over and not self.refused_at:
            self.refused_at = datetime.now(UTC).isoformat()
        return over

    def reset(self) -> None:
        self.used_tokens = 0
        self.refused_at = ""


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
    """Manages token budgets for the org and per-agent.

    Keyed by ``AgentInstance.id_str``.  The counters live only here, in
    memory, for the lifetime of this object -- which is the lifetime of
    the engine process.  ``meter_id`` names that lifetime so a consumer
    can tell a restarted meter (every counter legitimately back at zero)
    from a meter that dropped, and never carries a value across the two.
    """

    def __init__(
        self,
        org_budget: int = 0,
        agent_budgets: dict[str, int] | None = None,
        *,
        on_change: Callable[[str], None] | None = None,
    ) -> None:
        self.org_budget = TokenBudget(max_tokens=org_budget)
        self._agent_budgets: dict[str, TokenBudget] = {}
        self._lock = asyncio.Lock()
        # Identity of this meter's run.  ``used_tokens`` is only
        # comparable within one ``meter_id``.
        self.meter_id: str = str(uuid4())
        # Fired with the agent id whose figures moved ("" for an
        # org-only change).  Synchronous and must not await: it runs
        # under ``self._lock`` on the engine's hot path, so the only
        # correct implementation sets a flag for someone else to read.
        self._on_change = on_change
        for agent_id, budget in (agent_budgets or {}).items():
            self._agent_budgets[agent_id] = TokenBudget(max_tokens=budget)

    def set_on_change(self, on_change: Callable[[str], None] | None) -> None:
        """Attach (or detach) the change hook after construction."""
        self._on_change = on_change

    def _changed(self, agent_id: str) -> None:
        if self._on_change is None:
            return
        try:
            self._on_change(agent_id)
        except Exception as exc:  # pragma: no cover - defensive
            # Reporting must never break accounting.
            logger.warning("budget_on_change_failed", error=str(exc))

    def set_agent_budget(self, agent_id: str, max_tokens: int) -> None:
        self._agent_budgets[agent_id] = TokenBudget(max_tokens=max_tokens)
        self._changed(agent_id)

    def update_org_budget(self, max_tokens: int) -> None:
        """Update the org cap in place, preserving ``used_tokens``."""
        self.org_budget.max_tokens = max_tokens
        self._changed("")

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
        self._changed(agent_id)

    def drop_agent_budget(self, agent_id: str) -> None:
        """Remove a per-agent cap (e.g. when a role is deleted)."""
        self._agent_budgets.pop(agent_id, None)
        self._changed(agent_id)

    async def consume(self, agent_id: str, tokens: int) -> bool:
        """Consume tokens from both agent and org budgets.

        Returns True if tokens were available, False if any budget exhausted.
        """
        logger.debug("budget_consuming", tokens=tokens, agent_id=agent_id)
        async with self._lock:
            # Check org budget first
            if not self.org_budget.consume(tokens):
                logger.warning("budget_exhausted_org")
                self._changed(agent_id)
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
                self._changed(agent_id)
                return False

            self._changed(agent_id)
            return True

    async def record_spend(self, agent_id: str, tokens: int) -> bool:
        """Account tokens that were already spent.

        Used where the charge is post-hoc and a refusal would change
        nothing -- a collected sandbox run.  Both budgets are moved
        unconditionally; the return says whether that put either over
        its cap, which the caller should surface rather than swallow.
        """
        async with self._lock:
            over_org = self.org_budget.record(tokens)
            agent_budget = self._agent_budgets.get(agent_id)
            over_agent = agent_budget.record(tokens) if agent_budget else False
            self._changed(agent_id)
        return over_org or over_agent

    def get_agent_budget(self, agent_id: str) -> TokenBudget | None:
        return self._agent_budgets.get(agent_id)

    def report(self) -> dict[str, object]:
        """Snapshot every counter, in the shape the dashboard consumes.

        ``capped`` distinguishes "this seat has no per-agent budget at
        all" (the engine seeds one only for a non-zero
        ``Role.token_budget``) from "its cap is zero", which are
        different facts and would otherwise both render as an empty bar.

        The org figure is reported alongside the per-agent ones and is
        deliberately NOT their sum: ``consume`` charges the org for every
        agent, including the ones with no per-agent budget, so neither
        number can be derived from the other in either direction.
        """
        return {
            "meter_id": self.meter_id,
            "org_used_tokens": self.org_budget.used_tokens,
            "org_max_tokens": self.org_budget.max_tokens,
            "org_refused_at": self.org_budget.refused_at,
            "agents": {
                agent_id: {
                    "used_tokens": budget.used_tokens,
                    "max_tokens": budget.max_tokens,
                    "refused_at": budget.refused_at,
                }
                for agent_id, budget in self._agent_budgets.items()
            },
        }
