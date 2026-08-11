"""Tests for runtime org mutations and streaming API."""

import pytest

from crewlet.engine import Engine
from crewlet.org.models import Organization, OrgUnit, Role


def make_org() -> Organization:
    return Organization(
        name="Test Corp",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Tech Lead",
                roles=[
                    Role(
                        name="Tech Lead",
                        manages=["Engineer A", "Engineer B"],
                    ),
                    Role(name="Engineer A"),
                    Role(name="Engineer B"),
                ],
            )
        ],
    )


# --- Runtime mutation tests ---


@pytest.mark.asyncio
async def test_reassign():
    engine = Engine(organization=make_org())
    await engine.start()

    engineers = engine.agent_pool.get_all_for_role("Engineer A")
    target = engineers[0]

    result = await engine.reassign(target.id_str, "Tech Lead")
    assert result is True
    assert target.role_name == "Tech Lead"

    await engine.stop()


@pytest.mark.asyncio
async def test_reassign_nonexistent_role():
    engine = Engine(organization=make_org())
    await engine.start()

    engineers = engine.agent_pool.get_all_for_role("Engineer A")
    result = await engine.reassign(engineers[0].id_str, "Nonexistent")
    assert result is False

    await engine.stop()


@pytest.mark.asyncio
async def test_reassign_emits_event():
    """Reassigning should emit an agent_reassigned event."""
    engine = Engine(organization=make_org())
    await engine.start()

    engineers = engine.agent_pool.get_all_for_role("Engineer A")
    target = engineers[0]

    await engine.reassign(target.id_str, "Tech Lead")

    events = [e for e in engine.event_queue.history if e.type == "agent_reassigned"]
    assert len(events) == 1
    assert events[0].old_role == "Engineer A"
    assert events[0].new_role == "Tech Lead"

    await engine.stop()


def make_two_team_org() -> Organization:
    """Org with two teams under the same department, for manager-transfer tests."""
    return Organization(
        name="Test Corp",
        units=[
            OrgUnit(
                name="Eng",
                type="department",
                children=[
                    OrgUnit(
                        name="Core",
                        type="team",
                        lead="Tech Lead",
                        roles=[
                            Role(
                                name="Tech Lead",
                                manages=["Engineer A", "Engineer B"],
                            ),
                            Role(name="Engineer A"),
                            Role(name="Engineer B"),
                        ],
                    ),
                    OrgUnit(
                        name="Platform",
                        type="team",
                        lead="Platform Lead",
                        roles=[
                            Role(name="Platform Lead", manages=["SRE"]),
                            Role(name="SRE"),
                        ],
                    ),
                ],
            )
        ],
    )


@pytest.mark.asyncio
async def test_reassign_new_manager_only():
    """Move an agent under a different manager without changing its role."""
    engine = Engine(organization=make_two_team_org())
    await engine.start()

    engineers = engine.agent_pool.get_all_for_role("Engineer A")
    target = engineers[0]

    result = await engine.reassign(target.id_str, new_manager="Platform Lead")
    assert result is True
    # Role stays the same
    assert target.role_name == "Engineer A"

    # Engineer A moved to Platform Lead, Engineer B still under Tech Lead
    tech_lead = engine.org.get_role("Tech Lead")
    platform_lead = engine.org.get_role("Platform Lead")
    assert tech_lead is not None
    assert platform_lead is not None
    assert "Engineer B" in tech_lead.manages
    assert "Engineer A" in platform_lead.manages

    await engine.stop()


@pytest.mark.asyncio
async def test_reassign_new_manager_last_agent():
    """Moving the last agent of a role removes it from old manager."""
    org = Organization(
        name="Test Corp",
        units=[
            OrgUnit(
                name="Eng",
                type="department",
                children=[
                    OrgUnit(
                        name="Core",
                        type="team",
                        lead="Lead",
                        roles=[
                            Role(name="Lead", manages=["Dev"]),
                            Role(name="Dev"),
                        ],
                    ),
                    OrgUnit(
                        name="Platform",
                        type="team",
                        lead="Platform Lead",
                        roles=[
                            Role(name="Platform Lead"),
                        ],
                    ),
                ],
            )
        ],
    )
    engine = Engine(organization=org)
    await engine.start()

    devs = engine.agent_pool.get_all_for_role("Dev")
    assert len(devs) == 1

    result = await engine.reassign(devs[0].id_str, new_manager="Platform Lead")
    assert result is True

    # No siblings remain, so Dev is removed from Lead's manages
    lead = engine.org.get_role("Lead")
    platform_lead = engine.org.get_role("Platform Lead")
    assert lead is not None
    assert platform_lead is not None
    assert "Dev" not in lead.manages
    assert "Dev" in platform_lead.manages

    await engine.stop()


@pytest.mark.asyncio
async def test_reassign_new_manager_emits_event():
    """Manager-only reassign should emit event with manager info."""
    engine = Engine(organization=make_two_team_org())
    await engine.start()

    engineers = engine.agent_pool.get_all_for_role("Engineer A")
    target = engineers[0]

    await engine.reassign(target.id_str, new_manager="Platform Lead")

    events = [e for e in engine.event_queue.history if e.type == "agent_reassigned"]
    assert len(events) == 1
    assert events[0].old_role == "Engineer A"
    assert events[0].new_role == "Engineer A"
    assert events[0].old_manager == "Tech Lead"
    assert events[0].new_manager == "Platform Lead"

    await engine.stop()


@pytest.mark.asyncio
async def test_reassign_role_and_manager():
    """Change both role and manager simultaneously."""
    engine = Engine(organization=make_two_team_org())
    await engine.start()

    engineers = engine.agent_pool.get_all_for_role("Engineer A")
    target = engineers[0]

    result = await engine.reassign(
        target.id_str, new_role="SRE", new_manager="Platform Lead"
    )
    assert result is True
    assert target.role_name == "SRE"

    # Engineer B stays in Tech Lead's manages (sibling agent remains)
    tech_lead = engine.org.get_role("Tech Lead")
    assert tech_lead is not None
    assert "Engineer B" in tech_lead.manages

    # SRE should now be under Platform Lead
    platform_lead = engine.org.get_role("Platform Lead")
    assert platform_lead is not None
    assert "SRE" in platform_lead.manages

    await engine.stop()


@pytest.mark.asyncio
async def test_reassign_invalid_manager():
    """Reassigning to a nonexistent manager should fail."""
    engine = Engine(organization=make_two_team_org())
    await engine.start()

    engineers = engine.agent_pool.get_all_for_role("Engineer A")
    result = await engine.reassign(engineers[0].id_str, new_manager="Ghost")
    assert result is False

    await engine.stop()


@pytest.mark.asyncio
async def test_reassign_no_args():
    """Calling reassign with neither new_role nor new_manager should fail."""
    engine = Engine(organization=make_two_team_org())
    await engine.start()

    engineers = engine.agent_pool.get_all_for_role("Engineer A")
    result = await engine.reassign(engineers[0].id_str)
    assert result is False

    await engine.stop()


# --- stop() ordering tests ---


@pytest.mark.asyncio
async def test_stop_terminates_agents_before_org_stopped():
    """OrgStopped should be emitted after agents are terminated."""
    engine = Engine(organization=make_org())
    await engine.start()
    await engine.stop()

    # After stop, all agents should be terminated
    for agent in engine.agent_pool.agents:
        assert agent.state.value == "terminated"

    # Event ordering: agent_terminated events before org_stopped
    types = [e.type for e in engine.event_queue.history]
    if "agent_terminated" in types and "org_stopped" in types:
        last_terminated = max(i for i, t in enumerate(types) if t == "agent_terminated")
        org_stopped = types.index("org_stopped")
        assert last_terminated < org_stopped


# --- ExecutionTracker tests ---


def test_engine_creates_execution_tracker():
    """Engine always creates an ExecutionTracker."""
    engine = Engine(organization=make_org())
    assert engine.execution_tracker is not None


@pytest.mark.asyncio
async def test_reassign_to_human_seat_rejected():
    """A live agent instance must never be reattached to a human seat
    — kind flips go through apply_config, not reassign."""
    org = Organization(
        name="Test Corp",
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
                        contact={"slack_user_id": "U0HUMAN"},
                    ),
                    Role(name="Engineer A"),
                ],
            )
        ],
    )
    engine = Engine(organization=org)
    await engine.start()
    try:
        target = engine.agent_pool.get_all_for_role("Engineer A")[0]
        result = await engine.reassign(target.id_str, "Sarah Chen")
        assert result is False
        assert target.role_name == "Engineer A"
    finally:
        await engine.stop()


@pytest.mark.asyncio
async def test_kind_flip_to_agent_triggers_external_handle_refresh(monkeypatch):
    """A human → agent seat flip spawns from the kept set, not
    `added` — the Jira/GitHub identity refresh must cover it anyway."""
    old_org = Organization(
        name="Test Corp",
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
                        contact={"slack_user_id": "U0HUMAN"},
                    ),
                    Role(name="Engineer A"),
                ],
            )
        ],
    )
    new_org = Organization(
        name="Test Corp",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Sarah Chen",
                roles=[
                    Role(name="Sarah Chen"),  # now an agent seat
                    Role(name="Engineer A"),
                ],
            )
        ],
    )
    engine = Engine(organization=old_org)
    await engine.start()
    try:
        refreshed: list[set[str] | None] = []

        async def record(only_roles=None):
            refreshed.append(only_roles)

        monkeypatch.setattr(engine, "_refresh_role_external_handles", record)
        engine._tier_b_done = True

        await engine._apply_org_diff(old_org, new_org)

        assert engine.agent_pool.get_by_handle("sarah-chen") is not None
        assert refreshed and "Sarah Chen" in (refreshed[0] or set())
    finally:
        await engine.stop()
