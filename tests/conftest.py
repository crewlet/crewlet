"""Shared test fixtures for Crewlet."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from crewlet.config import (
    ApiAuthConfig,
    ApiConfig,
    BootstrapConfig,
    BootstrapProvidersConfig,
    CompanyConfig,
    DatabaseConfig,
    KnowledgeStoreConfig,
    QueueConfig,
)


def make_bootstrap(**overrides: Any) -> BootstrapConfig:
    """Build a minimal Tier A bootstrap config for tests.

    Defaults: memory knowledge backend, empty DSN, and auth explicitly
    DISABLED so test apps aren't gated by the bearer middleware.

    Explicit because serving the API now requires an auth decision:
    tokens, ``disabled``, or ``allow_anonymous_read``. Silence used to
    mean "open", which is how ``/events`` and ``/ws/stream`` ended up
    serving LLM transcripts to anyone who could reach the port.
    """
    return BootstrapConfig(
        providers=BootstrapProvidersConfig(
            queue=QueueConfig(),
            database=DatabaseConfig(),
            knowledge=KnowledgeStoreConfig(type="memory"),
        ),
        api=ApiConfig(auth=ApiAuthConfig(disabled=True)),
        debug=False,
        **overrides,
    )


def make_company(name: str = "Test Co", **overrides: Any) -> CompanyConfig:
    """Build a minimal Tier B CompanyConfig for tests."""
    return CompanyConfig(name=name, **overrides)


async def make_engine(
    *,
    bootstrap: BootstrapConfig | None = None,
    company: CompanyConfig | None = None,
    storage: Any = None,
    event_queue: Any = None,
    a2a_bus: Any = None,
    embeddings: Any = None,
    company_config_store: Any = None,
):
    """Build an engine and (optionally) apply a Tier B config.

    Pass ``company`` to seed the engine; omit to leave it in the
    unconfigured state.
    """
    from crewlet.engine import Engine

    engine = Engine.from_bootstrap(
        bootstrap or make_bootstrap(),
        storage=storage,
        event_queue=event_queue,
        a2a_bus=a2a_bus,
        embeddings=embeddings,
        company_config_store=company_config_store,
    )
    if company is not None:
        await engine.apply_config(company)
    return engine


async def make_engine_from_yaml(
    path: str | Path,
    *,
    bootstrap: BootstrapConfig | None = None,
    storage: Any = None,
    event_queue: Any = None,
    a2a_bus: Any = None,
    embeddings: Any = None,
):
    """Test-only helper: build an engine from a Tier B YAML fixture,
    for tests that bootstrap an engine from a YAML on disk.

    Production code uses ``Engine.from_bootstrap(bootstrap) +
    apply_config(company)`` sourced from the DB.
    """
    from crewlet.config import load_company_config

    return await make_engine(
        bootstrap=bootstrap,
        company=load_company_config(path),
        storage=storage,
        event_queue=event_queue,
        a2a_bus=a2a_bus,
        embeddings=embeddings,
    )


class StubAgent:
    """Duck-typed AgentInstance for registry stubs — only the fields
    the colleague resolvers read."""

    def __init__(self, handle: str, role_name: str = "", email: str = "") -> None:
        self.handle = handle
        self.role_name = role_name or handle
        self.email = email


class PartyRegistryStub:
    """Shared stand-in for ``HandleRegistry`` in resolver tests.

    Mirrors the production surface the colleague resolvers consume —
    the party-level API (``resolve_party`` / ``resolve_party_role_name``
    / ``resolve_party_external`` / ``human_seats`` / ``all_parties``)
    plus the agent-only methods.  One home so a registry contract
    change updates every consumer test at once.

    ``agents`` take :class:`StubAgent`; ``humans`` take real
    :class:`~crewlet.org.models.Role` seats; ``external`` maps
    ``{transport: {external_id: handle}}``.

    ``remote_agents`` are agent seats that exist in the org but are
    **not running in this process**.  They resolve like any other agent
    seat and are addressable (handle, derived id, inbox), they just
    carry no instance — the shape every seat has on every node that does
    not own it.  Consumers that must not confuse "not here" with "does
    not exist" are tested against these.  Pass a bare handle when only
    the handle matters, or a real :class:`~crewlet.org.models.Role` when
    the consumer reads the seat's own fields.
    """

    _ORG_NAME = "TestCo"

    def __init__(
        self,
        agents: list[Any] | None = None,
        external: dict[str, dict[str, str]] | None = None,
        humans: list[Any] | None = None,
        remote_agents: list[Any] | None = None,
    ) -> None:
        self._agents = {a.handle: a for a in (agents or [])}
        self._external = external or {}
        self._humans = {h.get_handle(): h for h in (humans or [])}
        self._remote = {
            (r if isinstance(r, str) else r.get_handle()): (
                None if isinstance(r, str) else r
            )
            for r in (remote_agents or [])
        }

    # -- party construction ------------------------------------------ #

    def _agent_party(self, agent: Any) -> Any:
        from crewlet.notifications.handle import ResolvedParty
        from crewlet.org.models import RoleKind

        return ResolvedParty(
            kind=RoleKind.AGENT,
            handle=agent.handle,
            name=agent.role_name,
            role=None,
            agent=agent,
        )

    def _remote_agent_party(self, handle: str) -> Any:
        from crewlet.notifications.handle import ResolvedParty
        from crewlet.org.models import RoleKind

        seat = self._remote.get(handle)
        return ResolvedParty(
            kind=RoleKind.AGENT,
            handle=handle,
            name=seat.name if seat is not None else handle,
            role=seat,
            agent=None,
            org_name=self._ORG_NAME,
        )

    def _human_party(self, seat: Any) -> Any:
        from crewlet.notifications.handle import ResolvedParty
        from crewlet.org.models import RoleKind

        return ResolvedParty(
            kind=RoleKind.HUMAN,
            handle=seat.get_handle(),
            name=seat.name,
            role=seat,
            agent=None,
        )

    # -- agent-only surface ------------------------------------------ #

    def resolve_handle(self, query: str) -> Any:
        return self._agents.get(query)

    def resolve_role(self, role_name: str) -> Any:
        for agent in self._agents.values():
            if agent.role_name == role_name:
                return agent
        return None

    def resolve_external_id(self, transport: str, ext_id: str) -> Any:
        handle = self._external.get(transport, {}).get(ext_id)
        return self._agents.get(handle) if handle else None

    def get_external_id(self, transport: str, handle: str) -> str:
        for ext_id, h in self._external.get(transport, {}).items():
            if h == handle:
                return ext_id
        return ""

    def all_transports(self) -> list[str]:
        return sorted(self._external.keys())

    def all_handles(self) -> list[str]:
        return list(self._agents.keys())

    # -- party surface ------------------------------------------------ #

    def resolve_party(self, query: str) -> Any:
        agent = self._agents.get(query)
        if agent is not None:
            return self._agent_party(agent)
        if query in self._remote:
            return self._remote_agent_party(query)
        seat = self._humans.get(query)
        if seat is not None:
            return self._human_party(seat)
        return None

    def resolve_party_role_name(self, role_name: str) -> Any:
        agent = self.resolve_role(role_name)
        if agent is not None:
            return self._agent_party(agent)
        for seat in self._humans.values():
            if seat.name == role_name:
                return self._human_party(seat)
        return None

    def resolve_party_external(self, transport: str, ext_id: str) -> Any:
        handle = self._external.get(transport, {}).get(ext_id)
        if not handle and transport == "slack":
            handle = self._external.get("slack_bot", {}).get(ext_id)
        return self.resolve_party(handle) if handle else None

    def human_seats(self) -> list[Any]:
        return list(self._humans.values())

    def all_parties(self) -> list[Any]:
        parties = [self._agent_party(a) for a in self._agents.values()]
        parties.extend(self._remote_agent_party(h) for h in self._remote)
        parties.extend(self._human_party(s) for s in self._humans.values())
        return parties
