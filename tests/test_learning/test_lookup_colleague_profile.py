"""Tests for the ``lookup_colleague`` counterparty-profile inline."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from crewlet.learning.models import CounterpartyProfile
from crewlet.tools.builtin import register_builtin_tools
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry
from tests.conftest import PartyRegistryStub, StubAgent


def _registry_stub() -> PartyRegistryStub:
    """Resolves 'bob' to a stub agent (handle + Slack external ID)."""
    return PartyRegistryStub(
        agents=[StubAgent("bob", "Engineer", "bob@acme.com")],
        external={"slack": {"U_bob": "bob"}},
    )


class _StoreStub:
    def __init__(self, profile: CounterpartyProfile | None) -> None:
        self._profile = profile
        self.fetch_calls: list[dict[str, Any]] = []

    async def upsert(self, **kwargs):  # pragma: no cover - unused
        raise NotImplementedError

    async def fetch(self, **kwargs) -> CounterpartyProfile | None:
        self.fetch_calls.append(kwargs)
        return self._profile

    async def fetch_for_senders(self, *args, **kwargs):  # pragma: no cover
        return None


def _mk_profile() -> CounterpartyProfile:
    return CounterpartyProfile(
        observer_handle="alice",
        subject_handle="bob",
        subject_external_id="",
        subject_platform="",
        subject_name="Bob",
        traits={
            "communication_style": "terse",
            "interests": ["deploys", "CI"],
        },
        first_seen_at=datetime(2026, 4, 20, 9, 0, tzinfo=UTC),
        last_updated_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        interaction_count=4,
    )


def _ctx(*, store: Any = None) -> AgentContext:
    return AgentContext(
        agent_id="alice-id",
        agent_handle="alice",
        role="Engineer",
        current_task_id="",
        counterparty_store=store,
        handle_registry=_registry_stub(),
    )


async def test_lookup_colleague_inlines_profile_when_present() -> None:
    registry = ToolRegistry()
    register_builtin_tools(registry)
    tool = registry.get("lookup_colleague")
    assert tool is not None

    store = _StoreStub(_mk_profile())
    result = await tool.execute({"query": "bob"}, _ctx(store=store))

    assert result.success
    assert "handle: bob" in result.output
    assert "Observed by you:" in result.output
    assert "communication_style: terse" in result.output
    assert "interests: deploys, CI" in result.output
    assert "interactions: 4" in result.output
    assert store.fetch_calls[0]["observer_handle"] == "alice"
    assert store.fetch_calls[0]["subject_handle"] == "bob"


async def test_lookup_colleague_omits_profile_when_none() -> None:
    registry = ToolRegistry()
    register_builtin_tools(registry)
    tool = registry.get("lookup_colleague")
    assert tool is not None

    store = _StoreStub(None)
    result = await tool.execute({"query": "bob"}, _ctx(store=store))

    assert result.success
    assert "handle: bob" in result.output
    assert "Observed by you:" not in result.output


async def test_lookup_colleague_omits_profile_when_store_unset() -> None:
    registry = ToolRegistry()
    register_builtin_tools(registry)
    tool = registry.get("lookup_colleague")
    assert tool is not None

    result = await tool.execute({"query": "bob"}, _ctx(store=None))

    assert result.success
    assert "Observed by you:" not in result.output


async def test_lookup_colleague_skips_profile_on_self_lookup() -> None:
    """Looking yourself up should never surface your own profile."""
    registry = ToolRegistry()
    register_builtin_tools(registry)
    tool = registry.get("lookup_colleague")
    assert tool is not None

    # A registry that resolves 'alice' to the caller.
    self_registry = PartyRegistryStub(
        agents=[StubAgent("alice", "Engineer", "alice@acme.com")]
    )

    ctx = AgentContext(
        agent_id="alice-id",
        agent_handle="alice",
        role="Engineer",
        current_task_id="",
        counterparty_store=_StoreStub(_mk_profile()),
        handle_registry=self_registry,
    )

    result = await tool.execute({"query": "alice"}, ctx)
    assert result.success
    assert "Observed by you:" not in result.output


async def test_lookup_colleague_swallows_store_exceptions() -> None:
    class _BoomStore:
        async def fetch(self, **kwargs):
            raise RuntimeError("db down")

        async def upsert(self, **kwargs):  # pragma: no cover
            raise NotImplementedError

        async def fetch_for_senders(self, *a, **k):  # pragma: no cover
            return None

    registry = ToolRegistry()
    register_builtin_tools(registry)
    tool = registry.get("lookup_colleague")
    assert tool is not None

    result = await tool.execute({"query": "bob"}, _ctx(store=_BoomStore()))
    # The identity block still returns even when profile fetch fails.
    assert result.success
    assert "handle: bob" in result.output
    assert "Observed by you:" not in result.output
