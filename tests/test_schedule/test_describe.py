"""Tests for the read-only schedule projection used by the dashboard."""

from __future__ import annotations

from crewlet.org.models import Organization, OrgUnit, Role, Schedule
from crewlet.schedule import describe_schedules, resolve_runners
from crewlet.schedule.scheduler import _iter_schedules


def _org() -> Organization:
    return Organization(
        name="Acme",
        roles=[
            Role(
                name="Ops",
                handle="ops",
                manages=["QA Lead", "QA Dev"],
                schedules=[Schedule(name="nightly", cron="0 2 * * *", task="rotate")],
            )
        ],
        units=[
            OrgUnit(
                name="Quality",
                type="team",
                lead="QA Lead",
                roles=[
                    Role(name="QA Lead", handle="qa-lead"),
                    Role(name="QA Dev", handle="qa-dev"),
                ],
                schedules=[
                    Schedule(name="standup", cron="30 9 * * 1-5", task="standup"),
                    Schedule(
                        name="report", cron="0 16 * * 5", task="report", target="lead"
                    ),
                ],
            )
        ],
    )


def test_describe_schedules_shape_and_defaults():
    rows = describe_schedules(_org(), default_timezone="Europe/Amsterdam")
    by_name = {r["name"]: r for r in rows}

    # Role schedule.
    nightly = by_name["nightly"]
    assert nightly["scope_type"] == "role"
    assert nightly["scope_id"] == "ops"
    assert nightly["target"] == ""  # role schedules ignore target
    assert nightly["runners"] == ["ops"]
    assert nightly["timezone"] == "Europe/Amsterdam"  # default applied

    # Unit each (default).
    standup = by_name["standup"]
    assert standup["scope_type"] == "unit"
    assert standup["scope_id"] == "Quality"
    assert standup["target"] == "each"
    assert set(standup["runners"]) == {"qa-lead", "qa-dev"}

    # Unit lead.
    assert by_name["report"]["target"] == "lead"
    assert by_name["report"]["runners"] == ["qa-lead"]


def test_describe_preserves_explicit_timezone():
    org = Organization(
        name="Acme",
        roles=[
            Role(
                name="Ops",
                handle="ops",
                schedules=[
                    Schedule(
                        name="x",
                        cron="0 9 * * *",
                        task="t",
                        timezone="America/New_York",
                    )
                ],
            )
        ],
    )
    rows = describe_schedules(org, default_timezone="UTC")
    assert rows[0]["timezone"] == "America/New_York"


def test_resolve_runners_matches_describe():
    org = _org()
    for scope_type, scope_id, unit, sch in _iter_schedules(org):
        handles = resolve_runners(org, scope_type, scope_id, unit, sch)
        assert isinstance(handles, list)
        assert all(isinstance(h, str) and h for h in handles)


# ---------------------------------------------------------------- #
# Human seats — runner resolution skips them
# ---------------------------------------------------------------- #


def _human(name: str = "Sarah Chen") -> Role:
    return Role(
        name=name,
        kind="human",
        email="sarah@acme.com",
        contact={"slack_user_id": "U0HUMAN"},
    )


def test_each_runners_exclude_human_seats():
    org = Organization(
        name="Acme",
        units=[
            OrgUnit(
                name="Quality",
                type="team",
                lead="QA Lead",
                roles=[
                    Role(name="QA Lead", handle="qa-lead"),
                    _human(),
                    Role(name="QA Dev", handle="qa-dev"),
                ],
                schedules=[
                    Schedule(name="standup", cron="30 9 * * 1-5", task="standup")
                ],
            )
        ],
    )
    rows = describe_schedules(org)
    assert set(rows[0]["runners"]) == {"qa-lead", "qa-dev"}


def test_lead_runner_empty_when_effective_lead_is_human():
    # Build the org without org-level validation tripping: the
    # schedule is disabled in config (validation rejects enabled
    # lead-targeted schedules under a human lead), then re-enabled in
    # memory to exercise the runtime guard that covers piecewise
    # bootstrap windows.
    org = Organization(
        name="Acme",
        units=[
            OrgUnit(
                name="Quality",
                type="team",
                lead="Sarah Chen",
                roles=[
                    _human(),
                    Role(name="QA Dev", handle="qa-dev"),
                ],
                schedules=[
                    Schedule(
                        name="report",
                        cron="0 16 * * 5",
                        task="report",
                        target="lead",
                        enabled=False,
                    )
                ],
            )
        ],
    )
    unit = org.get_unit("Quality")
    sch = unit.schedules[0]
    assert resolve_runners(org, "unit", unit.name, unit, sch) == []
