"""Tests for party-level resolution (agents ∪ human seats) in HandleRegistry."""

import pytest_asyncio

from crewlet.agent.pool import AgentPool
from crewlet.notifications.handle import (
    HandleRegistry,
    register_human_contacts_from_org,
)
from crewlet.org.models import Organization, OrgUnit, Role, RoleKind
from crewlet.queue.memory import MemoryEventQueue


def make_mixed_org() -> Organization:
    return Organization(
        name="TestCo",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Sarah Chen",
                roles=[
                    Role(
                        name="Sarah Chen",
                        kind="human",
                        email="sarah@acme.com",
                        contact={
                            "slack_user_id": "U0HUMAN",
                            "atlassian_account_id": "5b10ac8d-sarah",
                            "github_login": "sarahchen",
                            "plane_user_id": "AB12CD34-0000-0000-0000-000000-sarah",
                        },
                        availability="CET business hours",
                    ),
                    Role(name="Engineer", email="eng@test.com"),
                ],
            )
        ],
    )


@pytest_asyncio.fixture
async def registry():
    org = make_mixed_org()
    bus = MemoryEventQueue()
    await bus.start()
    pool = AgentPool(bus)
    await pool.spawn_from_org(org)
    yield HandleRegistry(pool, org_provider=lambda: org), org, pool
    await bus.stop()


async def test_human_seats_not_spawned(registry):
    _, _, pool = registry
    assert {a.role_name for a in pool.active_agents} == {"Engineer"}


async def test_resolve_party_agent(registry):
    reg, _, _ = registry
    party = reg.resolve_party("engineer")
    assert party is not None
    assert party.kind == RoleKind.AGENT
    assert not party.is_human
    assert party.agent is not None
    assert party.role is not None
    assert party.role.name == "Engineer"


async def test_resolve_party_human(registry):
    reg, _, _ = registry
    party = reg.resolve_party("sarah-chen")
    assert party is not None
    assert party.is_human
    assert party.agent is None
    assert party.role is not None
    assert party.role.availability == "CET business hours"


async def test_resolve_party_unknown(registry):
    reg, _, _ = registry
    assert reg.resolve_party("nobody") is None


async def test_agent_only_resolution_skips_humans(registry):
    reg, _, _ = registry
    # The inbox-routing surface must never resolve a human.
    assert reg.resolve_handle("sarah-chen") is None
    assert reg.resolve_role("Sarah Chen") is None


async def test_resolve_party_role_name(registry):
    reg, _, _ = registry
    party = reg.resolve_party_role_name("Sarah Chen")
    assert party is not None
    assert party.is_human
    party = reg.resolve_party_role_name("Engineer")
    assert party is not None
    assert party.kind == RoleKind.AGENT


async def test_resolve_party_email_human_real_address(registry):
    reg, _, _ = registry
    party = reg.resolve_party_email("Sarah@Acme.com")
    assert party is not None
    assert party.is_human


async def test_resolve_party_email_agent(registry):
    reg, _, _ = registry
    party = reg.resolve_party_email("eng@test.com")
    assert party is not None
    assert party.kind == RoleKind.AGENT


async def test_resolve_party_external_after_registration(registry):
    # External-ID resolution is index-only (it runs on the webhook
    # hot path) — registration is the engine-guaranteed step that
    # feeds it (boot + every org swap).
    reg, org, _ = registry
    register_human_contacts_from_org(reg, org)
    party = reg.resolve_party_external("slack", "U0HUMAN")
    assert party is not None and party.is_human
    party = reg.resolve_party_external("jira", "5b10ac8d-sarah")
    assert party is not None and party.is_human
    party = reg.resolve_party_external("confluence", "5b10ac8d-sarah")
    assert party is not None and party.is_human
    party = reg.resolve_party_external("github", "sarahchen")
    assert party is not None and party.is_human
    # Plane webhooks carry lowercase user UUIDs; the contact field is
    # normalized to lowercase at model construction so both sides match.
    party = reg.resolve_party_external("plane", "ab12cd34-0000-0000-0000-000000-sarah")
    assert party is not None and party.is_human
    assert reg.resolve_party_external("slack", "U_NOBODY") is None
    assert reg.resolve_party_external("smoke-signal", "U0HUMAN") is None


async def test_resolve_party_external_consults_slack_bot_namespace(registry):
    # Agent bot-user IDs live under "slack_bot" (auth.test); senders
    # in Slack must annotate for agents the same way they do for
    # humans.
    reg, _, _ = registry
    reg.register_external_id("slack_bot", "B0AGENT", "engineer")
    party = reg.resolve_party_external("slack", "B0AGENT")
    assert party is not None
    assert party.kind == RoleKind.AGENT
    assert party.handle == "engineer"


async def test_register_human_contacts_reconciles_contact_edit(registry):
    """A corrected slack_user_id unregisters the stale mapping."""
    reg, org, _ = registry
    register_human_contacts_from_org(reg, org)
    assert reg.resolve_party_external("slack", "U0HUMAN") is not None

    sarah = org.get_role("Sarah Chen")
    sarah.contact.slack_user_id = "U1FIXED"
    register_human_contacts_from_org(reg, org)

    assert reg.resolve_party_external("slack", "U0HUMAN") is None
    party = reg.resolve_party_external("slack", "U1FIXED")
    assert party is not None and party.is_human


async def test_register_human_contacts_reconciles_seat_removal():
    """A removed human seat's IDs stop resolving after the next swap."""
    org = make_mixed_org()
    bus = MemoryEventQueue()
    await bus.start()
    try:
        pool = AgentPool(bus)
        await pool.spawn_from_org(org)
        holder = {"org": org}
        reg = HandleRegistry(pool, org_provider=lambda: holder["org"])
        register_human_contacts_from_org(reg, holder["org"])
        assert reg.resolve_party_external("jira", "5b10ac8d-sarah") is not None

        holder["org"] = Organization(
            name="TestCo", roles=[Role(name="Engineer", email="eng@test.com")]
        )
        register_human_contacts_from_org(reg, holder["org"])
        assert reg.resolve_party_external("jira", "5b10ac8d-sarah") is None
    finally:
        await bus.stop()


async def test_register_human_contacts_reconcile_allows_moved_id(registry):
    """An ID that legitimately moves between human seats re-registers
    on the next reconcile instead of deadlocking on the conflict guard."""
    reg, org, _ = registry
    register_human_contacts_from_org(reg, org)

    sarah = org.get_role("Sarah Chen")
    sarah.contact.slack_user_id = ""
    replacement = Role(
        name="Pat Ops",
        kind="human",
        email="pat@acme.com",
        contact={"slack_user_id": "U0HUMAN"},
    )
    org.units[0].roles.append(replacement)
    register_human_contacts_from_org(reg, org)

    party = reg.resolve_party_external("slack", "U0HUMAN")
    assert party is not None
    assert party.handle == "pat-ops"


async def test_register_human_contacts(registry):
    reg, org, _ = registry
    count = register_human_contacts_from_org(reg, org)
    # slack + jira + confluence + github + plane
    assert count == 5
    assert reg.resolve_party_external("slack", "U0HUMAN").handle == "sarah-chen"
    # Agent-only external resolution must NOT return the human.
    assert reg.resolve_external_id("slack", "U0HUMAN") is None
    # Outbound lookup works (e.g. for <@…> mentions).
    assert reg.get_external_id("slack", "sarah-chen") == "U0HUMAN"


async def test_register_human_contacts_resolves_env_ref(registry, monkeypatch):
    """A ``${VAR}`` plane_user_id registers the resolved UUID (lowercased),
    never the raw reference text."""
    reg, org, _ = registry
    monkeypatch.setenv("PLANE_FOUNDER_USER_ID", "AB12CD34-0000-0000-0000-000000000042")
    sarah = org.get_role("Sarah Chen")
    sarah.contact.plane_user_id = "${PLANE_FOUNDER_USER_ID}"
    register_human_contacts_from_org(reg, org)

    party = reg.resolve_party_external("plane", "ab12cd34-0000-0000-0000-000000000042")
    assert party is not None and party.is_human
    assert party.handle == "sarah-chen"
    # The raw reference is never registered — under any casing.
    assert reg.resolve_party_external("plane", "${PLANE_FOUNDER_USER_ID}") is None
    assert reg.resolve_party_external("plane", "${plane_founder_user_id}") is None


async def test_register_human_contacts_skips_unresolved_env_ref(registry, monkeypatch):
    """An unresolved ``${VAR}`` identity is omitted — not registered
    verbatim; the seat's other identities still register, and the next
    reconcile picks the identity up once the variable is exported."""
    reg, org, _ = registry
    monkeypatch.delenv("PLANE_FOUNDER_USER_ID", raising=False)
    sarah = org.get_role("Sarah Chen")
    sarah.contact.plane_user_id = "${PLANE_FOUNDER_USER_ID}"
    count = register_human_contacts_from_org(reg, org)
    # slack + jira + confluence + github; the plane ref is omitted.
    assert count == 4
    assert reg.resolve_party_external("slack", "U0HUMAN") is not None
    assert reg.resolve_party_external("plane", "${PLANE_FOUNDER_USER_ID}") is None

    # Export the variable and reconcile again (engine start / org swap).
    monkeypatch.setenv("PLANE_FOUNDER_USER_ID", "ab12cd34-0000-0000-0000-000000000042")
    count = register_human_contacts_from_org(reg, org)
    assert count == 5
    party = reg.resolve_party_external("plane", "ab12cd34-0000-0000-0000-000000000042")
    assert party is not None and party.handle == "sarah-chen"


async def test_register_human_contacts_does_not_steal_agent_mapping(registry):
    reg, org, _ = registry
    # An agent already owns this Slack ID (e.g. its bot user id).
    reg.register_external_id("slack", "U0HUMAN", "engineer")
    register_human_contacts_from_org(reg, org)
    agent = reg.resolve_external_id("slack", "U0HUMAN")
    assert agent is not None
    assert agent.role_name == "Engineer"


async def test_registry_without_org_provider_has_no_humans():
    org = make_mixed_org()
    bus = MemoryEventQueue()
    await bus.start()
    try:
        pool = AgentPool(bus)
        await pool.spawn_from_org(org)
        reg = HandleRegistry(pool)  # pool-only construction (no human seats)
        assert reg.human_seats() == []
        assert reg.resolve_party("sarah-chen") is None
        party = reg.resolve_party("engineer")
        assert party is not None
        assert party.role is None  # no org reference to attach
    finally:
        await bus.stop()


async def test_org_provider_tracks_live_swap():
    org = make_mixed_org()
    holder = {"org": org}
    bus = MemoryEventQueue()
    await bus.start()
    try:
        pool = AgentPool(bus)
        await pool.spawn_from_org(org)
        reg = HandleRegistry(pool, org_provider=lambda: holder["org"])
        assert reg.resolve_party("sarah-chen") is not None

        # Hot reload swaps the org for one without the human seat.
        holder["org"] = Organization(
            name="TestCo", roles=[Role(name="Engineer", email="eng@test.com")]
        )
        assert reg.resolve_party("sarah-chen") is None
    finally:
        await bus.stop()
