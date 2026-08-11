"""Config-layer tests: schedule parsing + validation."""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from crewlet.config import CompanyConfig, SchedulingConfig, config_to_organization
from crewlet.org.models import OrgUnit, Role, Schedule


def test_config_maps_role_and_unit_schedules():
    raw = {
        "name": "Acme",
        "roles": [
            {
                "name": "Ops",
                "manages": ["QA Engineer"],
                "schedules": [
                    {"name": "nightly", "cron": "0 2 * * *", "task": "rotate logs"}
                ],
            }
        ],
        "units": [
            {
                "name": "Quality",
                "type": "team",
                "lead": "QA Engineer",
                "roles": [{"name": "QA Engineer"}],
                "schedules": [
                    {
                        "name": "standup",
                        "cron": "30 9 * * 1-5",
                        "timezone": "Europe/Amsterdam",
                        "task": "post your standup",
                    }
                ],
            }
        ],
    }
    org = config_to_organization(CompanyConfig.model_validate(raw))

    ops = org.get_role("Ops")
    assert ops is not None
    assert [s.name for s in ops.schedules] == ["nightly"]

    unit = org.get_unit("Quality")
    assert unit is not None
    assert len(unit.schedules) == 1
    sch = unit.schedules[0]
    assert sch.name == "standup"
    assert sch.timezone == "Europe/Amsterdam"
    assert sch.target == "each"  # default


def test_invalid_cron_rejected():
    with pytest.raises(ValidationError):
        Schedule(name="bad", cron="0 9 * *", task="x")  # 4 fields


def test_invalid_timezone_rejected():
    with pytest.raises(ValidationError):
        Schedule(name="bad", cron="0 9 * * *", task="x", timezone="Mars/Phobos")


def test_zero_timeout_rejected():
    with pytest.raises(ValidationError):
        Schedule(name="bad", cron="0 9 * * *", task="x", timeout_seconds=0)


def test_extra_key_rejected():
    with pytest.raises(ValidationError):
        Schedule(name="bad", cron="0 9 * * *", task="x", evry="typo")


def test_unit_target_must_resolve():
    sch = Schedule(name="s", cron="0 9 * * *", task="x", target="Nonexistent")
    with pytest.raises(ValidationError):
        OrgUnit(
            name="Quality",
            type="team",
            lead="QA",
            roles=[Role(name="QA")],
            schedules=[sch],
        )


def test_each_on_roles_less_unit_rejected():
    # target='each' fans out to DIRECT roles only; on a pure parent unit
    # (no direct roles) it could never fire, so it must fail at load.
    with pytest.raises(ValidationError):
        OrgUnit(
            name="Engineering",
            type="department",
            children=[
                OrgUnit(
                    name="Backend", type="team", lead="Dev", roles=[Role(name="Dev")]
                )
            ],
            schedules=[Schedule(name="s", cron="0 9 * * *", task="t")],  # target=each
        )


def test_unit_target_specific_role_rejected():
    # A specific-role target is not a unit-schedule mode (only
    # 'each' / 'lead'); a role schedule is the home for a per-person task.
    sch = Schedule(name="s", cron="0 9 * * *", task="x", target="QA")
    with pytest.raises(ValidationError):
        OrgUnit(
            name="Quality",
            type="team",
            lead="QA",
            roles=[Role(name="QA")],
            schedules=[sch],
        )


def test_duplicate_schedule_names_rejected():
    a = Schedule(name="dup", cron="0 9 * * *", task="x")
    b = Schedule(name="dup", cron="0 10 * * *", task="y")
    with pytest.raises(ValidationError):
        Role(name="Ops", schedules=[a, b])
    with pytest.raises(ValidationError):
        OrgUnit(
            name="U",
            type="team",
            lead="Ops",
            roles=[Role(name="Ops")],
            schedules=[a, b],
        )


def test_scheduling_config_validation():
    # Valid defaults.
    cfg = SchedulingConfig()
    assert cfg.enabled and cfg.tick_seconds == 10

    with pytest.raises(ValidationError):
        SchedulingConfig(tick_seconds=0)
    with pytest.raises(ValidationError):
        SchedulingConfig(catchup_min_seconds=200, catchup_max_seconds=100)
    with pytest.raises(ValidationError):
        SchedulingConfig(default_timezone="Nowhere/Nope")
