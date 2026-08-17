"""Shared token-budget usage: atomic spend across processes.

A budget splits into a cap and a counter, and only the counter is shared
state. The cap comes from the active config revision, so every process
derives the same number without coordinating. The counter answers "has
the company spent its 500k yet", which stops being true the moment it
lives in one process's memory: with N processes an org cap of 500k
becomes N x 500k, silently.

The spend is therefore a single conditional UPDATE per scope, with the
cap carried in the WHERE clause — check and increment in one statement,
so no peer can slip between them. Org and agent scopes move inside one
transaction, because a turn that is refused at the seat level must not
still have charged the org.

Both backends implement :class:`BudgetUsageStore` with identical
semantics and share one contract suite.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Protocol

from crewlet._logging import get_logger
from crewlet.db.client import Database

logger = get_logger("db.budgets")

ORG_SCOPE = "org"


def agent_scope(agent_id: str) -> str:
    """Usage scope for one seat."""
    return f"agent:{agent_id}"


@dataclass(frozen=True, slots=True)
class SpendOutcome:
    """The result of an attempted spend.

    ``rejected_scope`` names which budget refused (``"org"`` or
    ``"agent"``) so the caller can publish a ``BudgetExhausted`` that says
    which one, without a second read that a peer could have changed
    underneath it.
    """

    ok: bool
    org_used: int = 0
    agent_used: int = 0
    rejected_scope: str = ""
    rejected_used: int = 0
    rejected_limit: int = 0


class BudgetUsageStore(Protocol):
    """Atomic usage accounting for the org and one agent."""

    async def spend(
        self,
        *,
        agent_id: str,
        tokens: int,
        org_limit: int,
        agent_limit: int | None,
    ) -> SpendOutcome: ...

    async def usage(self, scope: str) -> int: ...

    async def reset(self, scope: str = "") -> int: ...


class _Refused(Exception):
    """Internal: unwinds the transaction when the second scope refuses."""

    def __init__(self, scope: str, used: int, limit: int) -> None:
        self.scope = scope
        self.used = used
        self.limit = limit


_BUMP_SQL = """
    INSERT INTO token_budget_usage (scope, used_tokens)
    VALUES ($1, $2)
    ON CONFLICT (scope) DO UPDATE
    SET used_tokens = token_budget_usage.used_tokens + $2,
        updated_at  = now()
    WHERE $3 = 0
       OR token_budget_usage.used_tokens + $2 <= $3
    RETURNING used_tokens
"""


class PostgresBudgetUsageStore:
    """PostgreSQL-backed :class:`BudgetUsageStore`."""

    def __init__(self, db: Database) -> None:
        self._db = db

    async def spend(
        self,
        *,
        agent_id: str,
        tokens: int,
        org_limit: int,
        agent_limit: int | None,
    ) -> SpendOutcome:
        if tokens <= 0:
            return SpendOutcome(ok=True)

        # A spend larger than a whole cap can never fit, and the INSERT
        # branch of the upsert (a scope with no row yet) has no existing
        # value to test against — so screen it here, before writing.
        for scope_name, limit in (("org", org_limit), ("agent", agent_limit)):
            if limit and tokens > limit:
                current = await self.usage(
                    ORG_SCOPE if scope_name == "org" else agent_scope(agent_id)
                )
                return SpendOutcome(
                    ok=False,
                    rejected_scope=scope_name,
                    rejected_used=current,
                    rejected_limit=limit,
                )

        try:
            async with self._db.transaction() as tx:
                org_row = await tx.fetchrow(
                    _BUMP_SQL, ORG_SCOPE, int(tokens), int(org_limit or 0)
                )
                if org_row is None:
                    raise _Refused(
                        "org", await self._usage_in(tx, ORG_SCOPE), int(org_limit or 0)
                    )
                org_used = int(org_row["used_tokens"])

                scope = agent_scope(agent_id)
                agent_row = await tx.fetchrow(
                    _BUMP_SQL, scope, int(tokens), int(agent_limit or 0)
                )
                if agent_row is None:
                    # Unwinds the org bump: the seat was refused, so the
                    # org must not be charged for a turn that never ran.
                    raise _Refused(
                        "agent", await self._usage_in(tx, scope), int(agent_limit or 0)
                    )
                return SpendOutcome(
                    ok=True,
                    org_used=org_used,
                    agent_used=int(agent_row["used_tokens"]),
                )
        except _Refused as refused:
            return SpendOutcome(
                ok=False,
                rejected_scope=refused.scope,
                rejected_used=refused.used,
                rejected_limit=refused.limit,
            )

    @staticmethod
    async def _usage_in(tx: Any, scope: str) -> int:
        row = await tx.fetchrow(
            "SELECT used_tokens FROM token_budget_usage WHERE scope = $1", scope
        )
        return int(row["used_tokens"]) if row else 0

    async def usage(self, scope: str) -> int:
        row = await self._db.fetchrow(
            "SELECT used_tokens FROM token_budget_usage WHERE scope = $1", scope
        )
        return int(row["used_tokens"]) if row else 0

    async def reset(self, scope: str = "") -> int:
        """Zero one scope, or every scope when ``scope`` is empty.

        Returns the number of scopes reset.
        """
        if scope:
            rows = await self._db.execute(
                "DELETE FROM token_budget_usage WHERE scope = $1 RETURNING scope",
                scope,
            )
        else:
            rows = await self._db.execute(
                "DELETE FROM token_budget_usage RETURNING scope"
            )
        return len(rows)


class MemoryBudgetUsageStore:
    """In-memory :class:`BudgetUsageStore` twin.

    Correct for a single process, which is what it is for. It does NOT
    make a multi-process deployment safe, and the engine refuses to use
    it as the authoritative counter when a database is configured.
    """

    def __init__(self) -> None:
        self._used: dict[str, int] = {}

    async def spend(
        self,
        *,
        agent_id: str,
        tokens: int,
        org_limit: int,
        agent_limit: int | None,
    ) -> SpendOutcome:
        if tokens <= 0:
            return SpendOutcome(ok=True)

        scope = agent_scope(agent_id)
        org_used = self._used.get(ORG_SCOPE, 0)
        seat_used = self._used.get(scope, 0)

        if org_limit and org_used + tokens > org_limit:
            return SpendOutcome(
                ok=False,
                rejected_scope="org",
                rejected_used=org_used,
                rejected_limit=org_limit,
            )
        if agent_limit and seat_used + tokens > agent_limit:
            return SpendOutcome(
                ok=False,
                rejected_scope="agent",
                rejected_used=seat_used,
                rejected_limit=agent_limit,
            )

        self._used[ORG_SCOPE] = org_used + tokens
        self._used[scope] = seat_used + tokens
        return SpendOutcome(
            ok=True,
            org_used=self._used[ORG_SCOPE],
            agent_used=self._used[scope],
        )

    async def usage(self, scope: str) -> int:
        return self._used.get(scope, 0)

    async def reset(self, scope: str = "") -> int:
        if scope:
            return 1 if self._used.pop(scope, None) is not None else 0
        count = len(self._used)
        self._used.clear()
        return count
