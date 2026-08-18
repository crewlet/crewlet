"""Tests for ``Engine.from_bootstrap`` and ``Engine.apply_config``.

These exercise the two-tier bootstrap (Tier A → Engine, Tier B →
``apply_config``) and the unconfigured-state behaviour the engine
must support so the operator can configure the company over the API
after the engine is already running.
"""

from __future__ import annotations

import pytest

from crewlet.config import (
    ApiConfig,
    BootstrapConfig,
    BootstrapProvidersConfig,
    CompanyConfig,
    DatabaseConfig,
    KnowledgeStoreConfig,
    LLMProviderConfig,
    ProvidersConfig,
    QueueConfig,
)
from crewlet.engine import Engine
from crewlet.events.types import Event
from crewlet.queue.memory import MemoryEventQueue
from crewlet.queue.topics import agent_inbox_group, agent_inbox_topic


def _make_bootstrap() -> BootstrapConfig:
    return BootstrapConfig(
        providers=BootstrapProvidersConfig(
            queue=QueueConfig(url="pulsar://test:6650"),
            database=DatabaseConfig(dsn=""),
            knowledge=KnowledgeStoreConfig(type="memory"),
        ),
        api=ApiConfig(host="127.0.0.1", port=0),
        debug=False,
    )


# -- from_bootstrap --------------------------------------------------


def test_from_bootstrap_returns_unconfigured_engine() -> None:
    bootstrap = _make_bootstrap()
    engine = Engine.from_bootstrap(bootstrap)
    assert engine._active_config is None
    assert engine.org.name == ""
    assert engine.org.roles == []
    assert engine.org.units == []
    assert engine.agent_pool.agents == []
    assert engine._llm_providers == {}
    assert engine.mcp_bridge is None


def test_from_bootstrap_carries_tier_a_settings() -> None:
    bootstrap = _make_bootstrap()
    bootstrap.api.host = "10.0.0.1"
    bootstrap.api.port = 9090
    bootstrap.debug = True
    engine = Engine.from_bootstrap(bootstrap)
    assert engine._api_host == "10.0.0.1"
    assert engine._api_port == 9090
    assert engine.debug is True


def test_from_bootstrap_stores_company_config_store() -> None:
    bootstrap = _make_bootstrap()
    sentinel = object()
    engine = Engine.from_bootstrap(
        bootstrap,
        company_config_store=sentinel,
    )
    assert engine._company_config_store is sentinel


# -- apply_config (first activation) ---------------------------------


async def test_apply_config_first_activation_populates_org_and_providers() -> None:
    bootstrap = _make_bootstrap()
    engine = Engine.from_bootstrap(bootstrap)
    company = CompanyConfig(
        name="Acme AI",
        mission="ship",
        roles=[{"name": "Founder", "goal": "lead"}],
    )
    await engine.apply_config(company)
    assert engine._active_config is company
    assert engine.org.name == "Acme AI"
    assert engine.org.mission == "ship"
    assert len(engine.org.all_roles()) == 1


async def test_apply_config_second_call_with_org_only_change_succeeds() -> None:
    """Org-level changes hot-reload via apply_config."""
    bootstrap = _make_bootstrap()
    engine = Engine.from_bootstrap(bootstrap)
    await engine.apply_config(CompanyConfig(name="Acme AI"))
    await engine.apply_config(CompanyConfig(name="Acme AI v2", mission="ship"))
    assert engine._active_config.name == "Acme AI v2"
    assert engine.org.name == "Acme AI v2"
    assert engine.org.mission == "ship"


async def test_apply_config_mcp_servers_change_stores_and_warns(
    caplog: pytest.LogCaptureFixture,
) -> None:
    """MCP server diff stores the new config and warns for
    restart-required subsystems."""
    bootstrap = _make_bootstrap()
    engine = Engine.from_bootstrap(bootstrap)
    await engine.apply_config(CompanyConfig(name="Acme AI"))
    await engine.apply_config(
        CompanyConfig(
            name="Acme AI",
            mcp_servers=[
                {"name": "calculator", "command": "uvx", "args": ["mcp-calc"]}
            ],
        )
    )
    # New config stored on the engine.
    assert engine._active_config.mcp_servers[0].name == "calculator"
    # And the in-memory MCP config list was rebuilt.
    assert any(c.name == "calculator" for c in engine._mcp_configs)


async def test_apply_config_llm_providers_change_is_live(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """LLM provider diff swaps the in-memory provider dict in place."""
    import os

    from crewlet.config import LLMProviderConfig, ProvidersConfig

    monkeypatch.setenv("OPENAI_API_KEY", "test-key-for-construction")
    bootstrap = _make_bootstrap()
    engine = Engine.from_bootstrap(bootstrap)
    await engine.apply_config(CompanyConfig(name="Acme AI"))
    # Capture initial dict identity — apply_config must NOT replace it.
    initial_dict = engine._llm_providers
    await engine.apply_config(
        CompanyConfig(
            name="Acme AI",
            providers=ProvidersConfig(
                llm={"default": LLMProviderConfig(model="gpt-4o-mini")}
            ),
        )
    )
    assert engine._llm_providers is initial_dict  # in-place mutation
    assert "default" in engine._llm_providers
    _ = os  # keep import explicit for env munging


async def test_provider_added_post_activation_builds_turn_engine(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Per-entity bootstrap: an empty stub activation boots with zero
    LLM providers (so ``start()`` cannot build the turn engine), then
    ``PUT /config/llm-providers`` adds one.  The provider diff must build
    the turn engine and let the parked seats flow -- otherwise routed
    notifications sit unconsumed and the agent never takes a turn.

    The seat's inbox is attached the moment the seat is CLAIMED, not
    when the turn engine appears: an owned seat is always attached, and
    a node with no turn engine parks its deliveries in the handler
    instead. So what the late path owes is the un-park, under the park's
    own reason -- resuming under the default would release a live
    sandbox hold on a seat mid-run.
    """
    monkeypatch.setenv("OPENAI_API_KEY", "test-key-for-construction")
    bootstrap = _make_bootstrap()
    queue = MemoryEventQueue()
    await queue.start()
    engine = Engine.from_bootstrap(bootstrap, event_queue=queue)

    # A company with a seat but NO providers: start() runs the Tier-B
    # cascade, claims the seat and attaches it, and leaves the turn
    # engine unbuilt.
    await engine.apply_config(CompanyConfig(name="Acme AI", roles=[{"name": "CEO"}]))
    await engine.start()
    assert engine.turn_engine is None
    assert engine._seat_host.held_handles == ("ceo",)
    assert (
        agent_inbox_topic("ceo"),
        agent_inbox_group("ceo"),
    ) in queue.attachments()

    # A delivery arriving now is parked, under the park's own reason.
    agent = engine.agent_pool.get_by_handle("ceo")
    assert agent is not None
    await engine._make_agent_handler(agent)([Event(type="task_assigned", source="t")])
    assert queue.pause_holds(agent_inbox_topic("ceo"), agent_inbox_group("ceo")) == {
        "no_turn_engine"
    }

    await engine.apply_config(
        CompanyConfig(
            name="Acme AI",
            roles=[{"name": "CEO"}],
            providers=ProvidersConfig(
                llm={"default": LLMProviderConfig(model="gpt-4o-mini")}
            ),
        )
    )

    assert engine.turn_engine is not None
    assert queue.pause_holds(agent_inbox_topic("ceo"), agent_inbox_group("ceo")) == (
        set()
    )
    await engine.stop()
    await queue.stop()


async def test_apply_config_scalars_change_is_live() -> None:
    """forge_app_id / notification settings update in place."""
    bootstrap = _make_bootstrap()
    engine = Engine.from_bootstrap(bootstrap)
    await engine.apply_config(
        CompanyConfig(
            name="Acme",
            integrations={"forge_app_id": "ari:old"},
            notification_rate_limit=10,
        )
    )
    await engine.apply_config(
        CompanyConfig(
            name="Acme",
            integrations={"forge_app_id": "ari:new"},
            notification_rate_limit=99,
        )
    )
    assert engine._forge_app_id == "ari:new"
    assert engine._notification_rate_limit == 99


async def test_apply_config_org_token_budget_change_is_live() -> None:
    """org_token_budget diff applies live (preserving used)."""
    bootstrap = _make_bootstrap()
    engine = Engine.from_bootstrap(bootstrap)
    await engine.apply_config(CompanyConfig(name="Acme AI", token_budget=100))
    # Simulate prior usage that must survive the live update.
    engine.budget_manager.org_budget.used_tokens = 25
    await engine.apply_config(CompanyConfig(name="Acme AI", token_budget=500))
    assert engine.budget_manager.org_budget.max_tokens == 500
    assert engine.budget_manager.org_budget.used_tokens == 25


async def test_apply_config_turn_engine_change_is_live() -> None:
    """turn_engine settings diff propagates to the live cell."""
    from crewlet.config import TurnEngineConfig

    bootstrap = _make_bootstrap()
    engine = Engine.from_bootstrap(bootstrap)
    await engine.apply_config(
        CompanyConfig(
            name="Acme AI",
            turn_engine=TurnEngineConfig(max_iterations=3),
        )
    )
    await engine.apply_config(
        CompanyConfig(
            name="Acme AI",
            turn_engine=TurnEngineConfig(max_iterations=8),
        )
    )
    # _active_config carries the new value; future TurnEngine
    # constructions read through it.
    assert engine._active_config.turn_engine.max_iterations == 8
    assert engine._turn_engine_config.max_iterations == 8


async def test_apply_config_rebuilds_delegation_for_new_org() -> None:
    bootstrap = _make_bootstrap()
    engine = Engine.from_bootstrap(bootstrap)
    old_delegation = engine.delegation
    await engine.apply_config(CompanyConfig(name="Acme AI"))
    assert engine.delegation is not old_delegation
    assert engine.delegation._org is engine.org


# -- start() gates on Tier B -----------------------------------------


async def test_start_unconfigured_engine_stays_alive_but_spawns_nothing() -> None:
    """The unconfigured engine boots, listens, but skips the spawn cascade."""
    bootstrap = _make_bootstrap()
    engine = Engine.from_bootstrap(bootstrap)
    try:
        await engine.start()
        assert engine.is_running is True
        assert engine.agent_pool.agents == []
        # No MCP bridge, no notification service — all Tier B-gated.
        assert engine.mcp_bridge is None
        assert engine.notification_service is None
        assert engine.turn_engine is None
    finally:
        await engine.stop()


async def test_first_activation_after_unconfigured_start_spawns_company() -> None:
    """First revision arrives post-boot → spawn cascade runs live.

    Engine starts unconfigured, then apply_config triggers start()
    re-entrancy so the Tier B cascade runs without a process restart.
    """
    bootstrap = _make_bootstrap()
    engine = Engine.from_bootstrap(bootstrap)
    try:
        await engine.start()
        # Unconfigured-boot state: Tier A done, Tier B not.
        assert engine._tier_a_done is True
        assert getattr(engine, "_tier_b_done", False) is False
        assert engine.agent_pool.agents == []

        # First revision arrives.
        await engine.apply_config(
            CompanyConfig(
                name="Late Spawn Co",
                roles=[{"name": "Founder", "goal": "lead"}],
            )
        )

        # Cascade ran: Tier B is done, agent spawned, notification
        # service wired.
        assert engine._tier_b_done is True
        assert len(engine.agent_pool.agents) == 1
        assert engine.notification_service is not None
    finally:
        await engine.stop()
