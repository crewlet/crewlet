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
