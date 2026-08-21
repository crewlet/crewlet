"""Tests for ``Engine.apply_config`` snapshot / rollback semantics."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parent.parent))
from conftest import make_engine  # noqa: E402
from crewlet.config import CompanyConfig  # noqa: E402
from crewlet.engine import ConfigApplyError  # noqa: E402


async def test_apply_failure_restores_active_config() -> None:
    """If a diff handler raises, ``_active_config`` rolls back."""
    engine = await make_engine(
        company=CompanyConfig(name="Initial"),
    )
    initial_cfg = engine._active_config
    assert initial_cfg is not None

    # Monkey-patch a handler to raise mid-apply.
    def _boom(self, old, new):
        raise RuntimeError("simulated subsystem failure")

    engine._apply_turn_engine_diff = _boom.__get__(engine, type(engine))

    new_cfg = CompanyConfig(name="Initial", turn_engine={"max_iterations": 99})
    with pytest.raises(ConfigApplyError) as exc_info:
        await engine.apply_config(new_cfg)

    assert exc_info.value.subsystem == "turn_engine"
    assert isinstance(exc_info.value.original, RuntimeError)
    # ``_active_config`` rolled back to the pre-apply value.
    assert engine._active_config is initial_cfg
    assert engine._active_config.name == "Initial"


async def test_apply_failure_restores_org_pointer() -> None:
    """A failed apply restores the prior org reference (no role drift)."""
    engine = await make_engine(
        company=CompanyConfig(
            name="Initial",
            roles=[{"name": "Founder"}],
        ),
    )
    initial_org = engine.org
    initial_role_count = len(engine.org.all_roles())

    # Monkey-patch a downstream handler to raise after org-diff runs.
    def _boom(self, old, new):
        raise RuntimeError("simulated providers failure")

    engine._apply_providers_diff = _boom.__get__(engine, type(engine))

    with pytest.raises(ConfigApplyError):
        await engine.apply_config(
            CompanyConfig(
                name="Initial",
                roles=[
                    {"name": "Founder"},
                    {"name": "Engineer"},
                ],
            )
        )

    # Org pointer + role count restored.
    assert engine.org is initial_org
    assert len(engine.org.all_roles()) == initial_role_count


async def test_apply_failure_restores_budget() -> None:
    """A failed apply restores the org token_budget cap."""
    engine = await make_engine(
        company=CompanyConfig(name="Initial", token_budget=100),
    )
    assert engine.budget_manager.org_budget.max_tokens == 100

    def _boom(self, old, new):
        raise RuntimeError("simulated scalars failure")

    engine._apply_scalars_diff = _boom.__get__(engine, type(engine))

    with pytest.raises(ConfigApplyError):
        await engine.apply_config(
            CompanyConfig(
                name="Initial", token_budget=500, integrations={"forge_app_id": "x"}
            )
        )

    assert engine.budget_manager.org_budget.max_tokens == 100


async def test_apply_failure_restores_per_seat_budgets() -> None:
    """A failed apply restores the per-ROLE caps, not just the org one.

    ``org`` applies before ``scalars``, so by the time the failure lands
    the new revision's per-seat caps are already in place.  Only the org
    cap used to be in the rollback snapshot, so the failed revision's
    per-role caps survived the rollback and governed spend until the
    next successful apply.
    """
    engine = await make_engine(
        company=CompanyConfig(
            name="Initial", roles=[{"name": "Engineer", "token_budget": 100}]
        ),
    )
    seat = engine.org.get_role("Engineer")
    agent_id = engine.org.agent_id_for(seat)
    assert engine.budget_manager.get_agent_budget(agent_id).max_tokens == 100

    def _boom(self, old, new):
        raise RuntimeError("simulated scalars failure")

    engine._apply_scalars_diff = _boom.__get__(engine, type(engine))

    with pytest.raises(ConfigApplyError):
        await engine.apply_config(
            CompanyConfig(
                name="Initial",
                roles=[{"name": "Engineer", "token_budget": 9000}],
                integrations={"forge_app_id": "x"},
            )
        )

    assert engine.budget_manager.get_agent_budget(agent_id).max_tokens == 100


async def test_apply_failure_restores_plane_config() -> None:
    """A failed apply that already rebuilt ``_plane_config`` (the
    ``integrations_plane`` branch runs before the failing transport
    swap) rolls the engine field back to the prior object.  The
    ``PlaneTransport`` in the restored ``live_transports`` dict carries
    its own resolved config, so the field restore is the whole Plane
    rollback."""
    plane_block = {
        "enabled": True,
        "url": "https://plane.example.com",
        "workspace": "acme",
        "webhook_secret": "wh-1",
    }
    engine = await make_engine(
        company=CompanyConfig(name="Initial", integrations={"plane": plane_block})
    )
    engine._tier_b_done = True
    original = engine._plane_config
    assert original is not None
    assert original.enabled is True

    async def _boom(self, transports):
        raise RuntimeError("simulated transport swap failure")

    engine._apply_notification_transports_live = _boom.__get__(engine, type(engine))

    changed = dict(plane_block, webhook_secret="wh-2")
    with pytest.raises(ConfigApplyError) as exc_info:
        await engine.apply_config(
            CompanyConfig(name="Initial", integrations={"plane": changed})
        )

    assert exc_info.value.subsystem == "restart_required"
    assert engine._plane_config is original
    assert engine._plane_config.webhook_secret == "wh-1"


async def test_apply_failure_reseeds_transport_routing_from_rolled_back_org() -> None:
    """``_apply_org_diff`` re-seeds the RUNNING transports' lead maps in
    place, which the rollback snapshot (transport *instances*, not their
    internal maps) cannot reach.  ``_rollback`` must therefore re-derive
    routing from the restored org — otherwise a failed apply leaves the
    live transport routing on the FAILED revision's map (and, with the
    always-push refresh, the map must also be able to SHRINK back)."""
    from unittest.mock import MagicMock

    from crewlet.notifications.transports.plane import PlaneTransport

    class _FakePlane(PlaneTransport):
        def __init__(self):  # type: ignore[no-untyped-def]
            self.name = "plane"
            self.lead_maps: list[dict[str, str]] = []

        def set_handle_registry(self, registry):  # type: ignore[no-untyped-def]
            pass

        def set_project_leads(self, mapping):  # type: ignore[no-untyped-def]
            self.lead_maps.append(dict(mapping))

        def set_notification_excluded_projects(self, keys):  # type: ignore[no-untyped-def]
            pass

    unit_eng = {
        "name": "Core",
        "type": "team",
        "lead": "Agent Lead",
        "roles": [{"name": "Agent Lead"}],
        "integrations": {"plane": {"project": "eng"}},
    }
    engine = await make_engine(company=CompanyConfig(name="Initial", units=[unit_eng]))
    engine._tier_b_done = True

    fake_plane = _FakePlane()
    fake_service = MagicMock()
    fake_service.transports = {"plane": fake_plane}
    engine.notification_service = fake_service

    # Seed the running transport from the pre-apply org.
    engine._reseed_notification_routing()
    assert fake_plane.lead_maps[-1] == {"ENG": "agent-lead"}

    # Fail a subsystem that runs AFTER the org diff.
    def _boom(self, old, new):
        raise RuntimeError("simulated turn_engine failure")

    engine._apply_turn_engine_diff = _boom.__get__(engine, type(engine))

    unit_plat = dict(unit_eng)
    unit_plat["integrations"] = {"plane": {"project": "plat"}}
    with pytest.raises(ConfigApplyError):
        await engine.apply_config(
            CompanyConfig(
                name="Initial",
                units=[unit_plat],
                turn_engine={"max_iterations": 7},
            )
        )

    # Mid-apply the org diff pushed the failed revision's map…
    assert {"PLAT": "agent-lead"} in fake_plane.lead_maps
    # …and rollback re-derived routing from the restored org.
    assert fake_plane.lead_maps[-1] == {"ENG": "agent-lead"}
    assert [u.plane_project for u in engine.org.all_units()] == ["eng"]


async def test_successful_apply_does_not_rollback() -> None:
    """When apply succeeds, snapshot is taken but never restored."""
    engine = await make_engine(
        company=CompanyConfig(name="Initial"),
    )
    await engine.apply_config(CompanyConfig(name="Initial", token_budget=500))
    assert engine.budget_manager.org_budget.max_tokens == 500


async def test_config_apply_error_carries_applied_before_failure() -> None:
    """The exception lists subsystems that succeeded before the crash."""
    engine = await make_engine(
        company=CompanyConfig(name="Initial", token_budget=10),
    )

    def _boom(self, old, new):
        raise RuntimeError("simulated turn_engine failure")

    engine._apply_turn_engine_diff = _boom.__get__(engine, type(engine))

    with pytest.raises(ConfigApplyError) as exc_info:
        # budgets diff runs first; turn_engine raises.
        await engine.apply_config(
            CompanyConfig(
                name="Initial",
                token_budget=99,
                turn_engine={"max_iterations": 7},
            )
        )

    assert "budgets" in exc_info.value.applied_before_failure
    assert exc_info.value.subsystem == "turn_engine"
