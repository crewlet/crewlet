"""Tests for HandleRegistry and handle-based identity."""

import pytest
import pytest_asyncio

from crewlet.agent.pool import AgentPool
from crewlet.notifications.handle import HandleRegistry
from crewlet.org.models import Organization, OrgUnit, Role, slugify
from crewlet.queue.memory import MemoryEventQueue


def _make_org(*roles: Role) -> Organization:
    return Organization(
        name="TestCo",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead=roles[0].name if roles else "",
                roles=list(roles),
            )
        ],
    )


class TestSlugify:
    def test_simple(self):
        assert slugify("Engineer") == "engineer"

    def test_spaces(self):
        assert slugify("Senior Engineer") == "senior-engineer"

    def test_special_chars(self):
        assert slugify("QA/Test Lead") == "qa-test-lead"

    def test_trailing_special(self):
        assert slugify("  Engineer  ") == "engineer"


class TestRoleHandle:
    def test_explicit_handle(self):
        role = Role(name="Senior Engineer", handle="sr-eng")
        assert role.get_handle() == "sr-eng"

    def test_auto_handle_from_name(self):
        role = Role(name="Senior Engineer")
        assert role.get_handle() == "senior-engineer"

    def test_single_handle_from_name(self):
        role = Role(name="Engineer")
        assert role.get_handle() == "engineer"


class TestHandleRegistry:
    @pytest_asyncio.fixture
    async def pool(self):
        org = _make_org(
            Role(name="Engineer", email="alice@test.com"),
            Role(name="Designer", email="bob@test.com"),
        )
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        await pool.spawn_from_org(org)
        yield pool
        await bus.stop()

    @pytest.mark.asyncio
    async def test_resolve_handle(self, pool):
        registry = HandleRegistry(pool)
        agent = registry.resolve_handle("engineer")
        assert agent is not None
        assert agent.role_name == "Engineer"

    @pytest.mark.asyncio
    async def test_resolve_handle_not_found(self, pool):
        registry = HandleRegistry(pool)
        assert registry.resolve_handle("nobody") is None

    @pytest.mark.asyncio
    async def test_resolve_email_plus_address(self, pool):
        registry = HandleRegistry(pool)
        agent = registry.resolve_email_address("notif+engineer@company.com")
        assert agent is not None
        assert agent.role_name == "Engineer"

    @pytest.mark.asyncio
    async def test_resolve_email_fallback_to_direct(self, pool):
        registry = HandleRegistry(pool)
        agent = registry.resolve_email_address("alice@test.com")
        assert agent is not None
        assert agent.role_name == "Engineer"

    @pytest.mark.asyncio
    async def test_parse_plus_address(self):
        assert (
            HandleRegistry.parse_plus_address("notif+engineer@company.com")
            == "engineer"
        )
        assert HandleRegistry.parse_plus_address("alice@company.com") == ""
        assert HandleRegistry.parse_plus_address("") == ""
        # Case normalization
        assert (
            HandleRegistry.parse_plus_address("notif+Engineer@company.com")
            == "engineer"
        )
        assert (
            HandleRegistry.parse_plus_address("NOTIF+TECH-LEAD@COMPANY.COM")
            == "tech-lead"
        )

    @pytest.mark.asyncio
    async def test_agent_email(self, pool):
        registry = HandleRegistry(pool)
        email = registry.agent_email("engineer", "company.com")
        assert email == "notif+engineer@company.com"

    @pytest.mark.asyncio
    async def test_agent_email_custom_prefix(self, pool):
        registry = HandleRegistry(pool)
        email = registry.agent_email("engineer", "company.com", prefix="crew")
        assert email == "crew+engineer@company.com"

    @pytest.mark.asyncio
    async def test_external_id_mapping(self, pool):
        registry = HandleRegistry(pool)
        registry.register_external_id("slack", "U_BOT_123", "engineer")

        agent = registry.resolve_external_id("slack", "U_BOT_123")
        assert agent is not None
        assert agent.role_name == "Engineer"

        ext_id = registry.get_external_id("slack", "engineer")
        assert ext_id == "U_BOT_123"

    @pytest.mark.asyncio
    async def test_external_id_not_found(self, pool):
        registry = HandleRegistry(pool)
        assert registry.resolve_external_id("slack", "unknown") is None

    @pytest.mark.asyncio
    async def test_all_handles(self, pool):
        registry = HandleRegistry(pool)
        handles = registry.all_handles()
        assert set(handles) == {"engineer", "designer"}


class TestAgentPoolHandle:
    @pytest.mark.asyncio
    async def test_spawn_assigns_handle(self):
        org = _make_org(
            Role(name="Senior Engineer"),
        )
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        agents = await pool.spawn_from_org(org)
        assert agents[0].handle == "senior-engineer"

    @pytest.mark.asyncio
    async def test_spawn_individual_handles(self):
        org = _make_org(
            Role(name="Engineer A"),
            Role(name="Engineer B"),
            Role(name="Engineer C"),
        )
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        agents = await pool.spawn_from_org(org)
        handles = {a.handle for a in agents}
        assert handles == {
            "engineer-a",
            "engineer-b",
            "engineer-c",
        }

    @pytest.mark.asyncio
    async def test_get_by_handle(self):
        org = _make_org(
            Role(name="Engineer"),
            Role(name="Designer"),
        )
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        await pool.spawn_from_org(org)

        agent = pool.get_by_handle("engineer")
        assert agent is not None
        assert agent.role_name == "Engineer"

    @pytest.mark.asyncio
    async def test_get_by_handle_ignores_terminated(self):
        org = _make_org(Role(name="Engineer"))
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        agents = await pool.spawn_from_org(org)
        await pool.terminate(agents[0])
        assert pool.get_by_handle("engineer") is None


class TestHandleValidation:
    def test_valid_handles(self):
        assert HandleRegistry.validate_handle("engineer") is True
        assert HandleRegistry.validate_handle("senior-engineer") is True
        assert HandleRegistry.validate_handle("eng-0") is True
        assert HandleRegistry.validate_handle("a") is True

    def test_invalid_handles(self):
        assert HandleRegistry.validate_handle("") is False
        assert HandleRegistry.validate_handle("-engineer") is False
        assert HandleRegistry.validate_handle("eng/test") is False
        assert HandleRegistry.validate_handle("eng\x00test") is False
        assert HandleRegistry.validate_handle("Engineer") is False
        assert HandleRegistry.validate_handle("eng test") is False

    @pytest.mark.asyncio
    async def test_register_external_id_rejects_invalid(self):
        org = _make_org(Role(name="Engineer"))
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        await pool.spawn_from_org(org)
        registry = HandleRegistry(pool)
        with pytest.raises(ValueError, match="Invalid handle"):
            registry.register_external_id("slack", "U_BOT", "../path-escape")

    @pytest.mark.asyncio
    async def test_agent_email_rejects_invalid(self):
        org = _make_org(Role(name="Engineer"))
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        await pool.spawn_from_org(org)
        registry = HandleRegistry(pool)
        with pytest.raises(ValueError, match="Invalid handle"):
            registry.agent_email("bad handle!", "test.com")


class TestHandleRegistryIndexInvalidation:
    @pytest.mark.asyncio
    async def test_index_detects_agent_replacement(self):
        """Index rebuilds when an agent is replaced (same count).

        ``AgentInstance.id`` is deterministic per ``(org, handle)``, so
        a replacement keeps the same id (restart-equivalent reuse).
        The index still has to rebuild though -- the *object* changes,
        and the original instance is now terminated -- otherwise
        ``resolve_handle`` would hand back a TERMINATED reference.
        """
        org = _make_org(
            Role(name="Engineer", email="alice@test.com"),
        )
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        await pool.spawn_from_org(org)
        registry = HandleRegistry(pool)

        # Initial lookup works
        agent1 = registry.resolve_handle("engineer")
        assert agent1 is not None
        agent1_id = agent1.id_str

        # Simulate failure + replacement (pool.handle_failure)
        await pool.handle_failure(agent1, "task-1", "test error")

        # Resolves to the live replacement instance, not the terminated
        # original.  Identity (``agent.id``) stays stable across the
        # replacement -- that's the whole point of the deterministic
        # derivation -- but the object is fresh and active.
        agent2 = registry.resolve_handle("engineer")
        assert agent2 is not None
        assert agent2 is not agent1
        assert agent2.id_str == agent1_id
        assert agent2.state.value == "idle"


class TestOrgDerivedResolution:
    """Resolution must not depend on where an agent happens to be running.

    Every routing decision a node makes — which inbox a notification goes
    to, which seat a task belongs to, who an agent's colleagues are —
    needs only a handle and an agent id, and both are derivable from the
    org that every process has. Resolving through the live
    ``AgentInstance`` instead made delivery depend on local execution,
    which is correct only while every agent runs in every process.

    These tests describe a seat that exists in the org and is NOT
    running here.
    """

    def _registry_with_unspawned_seat(self) -> HandleRegistry:
        bus = MemoryEventQueue()
        pool = AgentPool(bus)  # deliberately empty — nothing runs here
        org = _make_org(
            Role(name="Engineer", goal="build"),
            Role(name="Designer", goal="design"),
        )
        return HandleRegistry(pool, org_provider=lambda: org)

    def test_an_unspawned_seat_still_resolves(self):
        party = self._registry_with_unspawned_seat().resolve_party("engineer")
        assert party is not None
        assert party.handle == "engineer"
        assert party.is_human is False

    def test_an_unspawned_seat_is_addressable_but_not_runnable(self):
        """The distinction the whole change turns on. ``agent is None``
        must not be read as "no such party" — for an agent seat it means
        "not running here", and routing does not care."""
        party = self._registry_with_unspawned_seat().resolve_party("engineer")
        assert party is not None
        assert party.is_local is False
        assert party.agent is None
        assert party.inbox_topic == "crewlet.agent.engineer.inbox"

    def test_the_agent_id_is_derived_not_looked_up(self):
        """Deterministic from (org name, handle), so every process
        computes the same id for a seat none of them may be running."""
        from crewlet.db.agents import derive_agent_id

        party = self._registry_with_unspawned_seat().resolve_party("engineer")
        assert party is not None
        assert party.agent_id == derive_agent_id("TestCo", "engineer")

    async def test_a_live_instance_is_attached_when_present(self):
        """The local case must keep working, and must use the instance's
        own id rather than re-deriving it."""
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        org = _make_org(Role(name="Engineer", goal="build"))
        registry = HandleRegistry(pool, org_provider=lambda: org)

        (agent,) = await pool.spawn_from_org(org)
        party = registry.resolve_party("engineer")
        assert party is not None
        assert party.is_local is True
        assert party.agent is agent
        assert party.agent_id == agent.id

    def test_role_name_resolution_covers_unspawned_seats(self):
        party = self._registry_with_unspawned_seat().resolve_party_role_name("Engineer")
        assert party is not None and party.handle == "engineer"

    def test_plus_addressed_email_resolves_an_unspawned_seat(self):
        registry = self._registry_with_unspawned_seat()
        party = registry.resolve_party_email("notif+engineer@example.com")
        assert party is not None and party.handle == "engineer"

    def test_all_parties_enumerates_the_ORG_not_the_pool(self):
        """An agent's view of its own company must not shrink to whatever
        is running locally.

        This is what the colleague surface builds its fuzzy-match corpus
        from: a missing colleague does not merely fail to match, it lets
        the match land on the wrong one.
        """
        registry = self._registry_with_unspawned_seat()
        handles = {p.handle for p in registry.all_parties()}
        assert handles == {"engineer", "designer"}

    async def test_all_parties_attaches_live_instances_where_present(self):
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        org = _make_org(Role(name="Engineer", goal="build"))
        # The org the REGISTRY sees has a second seat the pool never
        # spawned — the shape a fleet member is in for a peer's seat.
        wider = _make_org(
            Role(name="Engineer", goal="build"),
            Role(name="Designer", goal="design"),
        )
        registry = HandleRegistry(pool, org_provider=lambda: wider)
        await pool.spawn_from_org(org)

        by_handle = {p.handle: p for p in registry.all_parties()}
        assert by_handle["engineer"].is_local is True
        assert by_handle["designer"].is_local is False
        # Both are addressable regardless.
        assert by_handle["designer"].inbox_topic
        assert by_handle["engineer"].inbox_topic

    async def test_human_seats_are_still_distinguishable(self):
        """``agent is None`` now has two causes, so the kind — not the
        instance — is what tells a human from an unspawned agent."""
        bus = MemoryEventQueue()
        pool = AgentPool(bus)
        org = _make_org(
            Role(name="Engineer", goal="build"),
            Role(
                name="Founder",
                kind="human",
                email="founder@example.com",
                contact={"slack_user_id": "U0FOUNDER"},
            ),
        )
        registry = HandleRegistry(pool, org_provider=lambda: org)

        human = registry.resolve_party("founder")
        agent = registry.resolve_party("engineer")
        assert human is not None and agent is not None
        assert human.is_human is True
        assert agent.is_human is False
        assert human.agent is None and agent.agent is None
        # A human seat has no inbox and no agent id; an unspawned agent
        # seat has both.
        assert human.inbox_topic == "" and human.agent_id is None
        assert agent.inbox_topic and agent.agent_id is not None

    def test_agent_id_resolution_covers_unspawned_seats(self):
        """The reverse derivation, which is what the internal routing
        paths use: they carry an agent id, not a handle."""
        registry = self._registry_with_unspawned_seat()
        from crewlet.db.agents import derive_agent_id

        wanted = str(derive_agent_id("TestCo", "designer"))
        party = registry.resolve_party_agent_id(wanted)
        assert party is not None
        assert party.handle == "designer"
        assert party.is_local is False

    async def test_agent_id_resolution_prefers_a_live_instance(self):
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        org = _make_org(Role(name="Engineer", goal="build"))
        registry = HandleRegistry(pool, org_provider=lambda: org)
        (agent,) = await pool.spawn_from_org(org)

        party = registry.resolve_party_agent_id(agent.id_str)
        assert party is not None and party.agent is agent

    def test_agent_id_resolution_rejects_unknown_and_empty(self):
        registry = self._registry_with_unspawned_seat()
        assert registry.resolve_party_agent_id("") is None
        assert registry.resolve_party_agent_id("not-an-id") is None

    def test_an_unknown_handle_still_resolves_to_nothing(self):
        assert self._registry_with_unspawned_seat().resolve_party("nobody") is None

    async def test_pool_only_construction_still_works(self):
        """No org reference at all (tests, programmatic embeds) falls back
        to the pool rather than reporting an empty company."""
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        org = _make_org(Role(name="Engineer", goal="build"))
        registry = HandleRegistry(pool)  # no org_provider
        await pool.spawn_from_org(org)

        assert [p.handle for p in registry.all_parties()] == ["engineer"]
