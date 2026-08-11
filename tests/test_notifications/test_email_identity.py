"""Tests for email identity on Role, AgentInstance, and AgentPool."""

import pytest

from crewlet.agent.pool import AgentPool
from crewlet.org.models import Organization, OrgUnit, Role
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


class TestRoleEmail:
    def test_single_email(self):
        role = Role(name="Engineer", email="alice@test.com")
        assert role.email == "alice@test.com"

    def test_no_email_returns_empty(self):
        role = Role(name="Engineer")
        assert role.email == ""


class TestAgentPoolEmail:
    @pytest.mark.asyncio
    async def test_spawn_assigns_email(self):
        org = _make_org(
            Role(name="Engineer", email="alice@test.com"),
        )
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        agents = await pool.spawn_from_org(org)

        assert len(agents) == 1
        assert agents[0].email == "alice@test.com"

    @pytest.mark.asyncio
    async def test_spawn_assigns_individual_emails(self):
        org = _make_org(
            Role(name="Engineer A", email="eng1@test.com"),
            Role(name="Engineer B", email="eng2@test.com"),
        )
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        agents = await pool.spawn_from_org(org)

        assert len(agents) == 2
        emails = {a.email for a in agents}
        assert emails == {"eng1@test.com", "eng2@test.com"}

    @pytest.mark.asyncio
    async def test_get_by_email(self):
        org = _make_org(
            Role(name="Engineer", email="alice@test.com"),
            Role(name="Designer", email="bob@test.com"),
        )
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        await pool.spawn_from_org(org)

        agent = pool.get_by_email("alice@test.com")
        assert agent is not None
        assert agent.role_name == "Engineer"

        agent = pool.get_by_email("bob@test.com")
        assert agent is not None
        assert agent.role_name == "Designer"

    @pytest.mark.asyncio
    async def test_get_by_email_case_insensitive(self):
        org = _make_org(
            Role(name="Engineer", email="Alice@Test.Com"),
        )
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        await pool.spawn_from_org(org)

        assert pool.get_by_email("alice@test.com") is not None
        assert pool.get_by_email("ALICE@TEST.COM") is not None

    @pytest.mark.asyncio
    async def test_get_by_email_not_found(self):
        org = _make_org(
            Role(name="Engineer", email="alice@test.com"),
        )
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        await pool.spawn_from_org(org)

        assert pool.get_by_email("nobody@test.com") is None

    @pytest.mark.asyncio
    async def test_get_by_email_ignores_terminated(self):
        org = _make_org(
            Role(name="Engineer", email="alice@test.com"),
        )
        bus = MemoryEventQueue()
        await bus.start()
        pool = AgentPool(bus)
        agents = await pool.spawn_from_org(org)

        await pool.terminate(agents[0])
        assert pool.get_by_email("alice@test.com") is None
