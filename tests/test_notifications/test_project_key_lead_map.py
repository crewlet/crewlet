"""Tests for build_project_key_lead_map — project key to team lead mapping."""

from __future__ import annotations

import logging

from crewlet.config import build_project_key_lead_map
from crewlet.org.models import Organization, OrgUnit, Role

# Warnings are asserted through ``caplog`` (the stdlib record stream), not
# ``capfd``: ``crewlet._logging`` routes structlog through stdlib logging,
# so nothing reaches a file descriptor until ``configure_logging()``
# installs the stderr handler.  Reading file descriptors here only worked
# when some *other* test had already called it — running this module alone
# failed.


def _make_org(
    teams: list[tuple[str, str, str]],
) -> Organization:
    """Build an Organization from a list of (team_name, lead_name, project_key).

    All teams go into a single unit. Each team gets one role
    (the lead). The project key is set as the unit's ``jira_project``
    integration identity.
    """
    org_units = []
    for team_name, lead_name, project_key in teams:
        role = Role(name=lead_name, responsibilities=[])
        org_units.append(
            OrgUnit(
                name=team_name,
                type="team",
                lead=lead_name,
                roles=[role],
                jira_project=project_key,
            )
        )
    return Organization(name="TestCo", units=org_units)


def test_single_team_single_key() -> None:
    org = _make_org([("Backend", "Tech Lead", "BACK")])
    result = build_project_key_lead_map(org)
    assert result == {"BACK": "tech-lead"}


def test_multiple_teams_unique_keys() -> None:
    org = _make_org(
        [
            ("Backend", "Tech Lead", "BACK"),
            ("Frontend", "UI Lead", "FRONT"),
        ]
    )
    result = build_project_key_lead_map(org)
    assert result == {"BACK": "tech-lead", "FRONT": "ui-lead"}


def test_team_without_project_key_skipped() -> None:
    org = _make_org(
        [
            ("Backend", "Tech Lead", "BACK"),
            ("Design", "Designer", ""),  # no project key
        ]
    )
    result = build_project_key_lead_map(org)
    assert result == {"BACK": "tech-lead"}


def test_duplicate_key_warns_and_picks_first(caplog) -> None:
    """Same key resolving to DIFFERENT leads warns, uses first lead."""
    org = _make_org(
        [
            ("Backend", "Tech Lead", "SHARED"),
            ("Frontend", "UI Lead", "SHARED"),
        ]
    )
    with caplog.at_level(logging.WARNING, logger="crewlet.config"):
        result = build_project_key_lead_map(org)

    # First team's lead wins
    assert result == {"SHARED": "tech-lead"}

    # Warning was logged with structured event name.  ``caplog``, not
    # ``capfd``: structlog routes through stdlib logging here, which
    # pytest's logging plugin intercepts before it reaches the fds.
    assert "jira_project_key_shared" in caplog.text


def test_duplicate_key_same_lead_stays_silent(caplog) -> None:
    """Several units sharing one project under ONE lead is the documented
    Jira-parity pattern — first-wins, and NO warning (there is no routing
    ambiguity when every source resolves to the same handle)."""
    lead = Role(name="Tech Lead", responsibilities=[])
    child = OrgUnit(
        name="Backend Infra",
        type="team",
        jira_project="SHARED",  # no own lead — inherits Tech Lead
    )
    parent = OrgUnit(
        name="Backend",
        type="team",
        lead="Tech Lead",
        roles=[lead],
        jira_project="SHARED",
        children=[child],
    )
    org = Organization(name="TestCo", units=[parent])

    result = build_project_key_lead_map(org)

    assert result == {"SHARED": "tech-lead"}
    assert "jira_project_key_shared" not in caplog.text


def test_empty_org() -> None:
    org = Organization(name="Empty", units=[])
    result = build_project_key_lead_map(org)
    assert result == {}


def test_multiple_units() -> None:
    """Units across the structure are all scanned."""
    role1 = Role(name="Lead A", responsibilities=[])
    role2 = Role(name="Lead B", responsibilities=[])
    unit1 = OrgUnit(
        name="Team A",
        type="team",
        lead="Lead A",
        roles=[role1],
        jira_project="ALPHA",
    )
    unit2 = OrgUnit(
        name="Team B",
        type="team",
        lead="Lead B",
        roles=[role2],
        jira_project="BETA",
    )
    org = Organization(
        name="Multi",
        units=[
            OrgUnit(name="Dept1", type="department", children=[unit1]),
            OrgUnit(name="Dept2", type="department", children=[unit2]),
        ],
    )
    result = build_project_key_lead_map(org)
    assert result == {"ALPHA": "lead-a", "BETA": "lead-b"}


def test_env_var_resolution() -> None:
    """${VAR} references in JIRA_PROJECT_KEY are resolved."""
    import os

    os.environ["TEST_PROJECT_KEY"] = "RESOLVED"
    try:
        role = Role(name="Lead", responsibilities=[])
        unit = OrgUnit(
            name="Team",
            type="team",
            lead="Lead",
            roles=[role],
            jira_project="${TEST_PROJECT_KEY}",
        )
        org = Organization(
            name="Test",
            units=[unit],
        )
        result = build_project_key_lead_map(org)
        assert result == {"RESOLVED": "lead"}
    finally:
        del os.environ["TEST_PROJECT_KEY"]


def test_mixed_case_key_normalised_to_upper() -> None:
    """JIRA_PROJECT_KEY values are uppercased so webhook lookup is case-insensitive."""
    org = _make_org([("Executives", "CEO", "PoC")])
    result = build_project_key_lead_map(org)
    assert result == {"POC": "ceo"}


# --- Root-level role tests ---


def test_root_level_role_project_key() -> None:
    """Root-level role with JIRA_PROJECT_KEY is included in the map."""
    org = Organization(
        name="Test",
        roles=[
            Role(
                name="CEO",
                jira_project="EXEC",
            ),
        ],
    )
    result = build_project_key_lead_map(org)
    assert result == {"EXEC": "ceo"}


def test_root_level_role_and_unit_keys_coexist() -> None:
    """Root-level and unit-level project keys both appear in the map."""
    org = Organization(
        name="Test",
        roles=[
            Role(
                name="CTO",
                jira_project="INFRA",
            ),
        ],
        units=[
            OrgUnit(
                name="Backend",
                type="team",
                lead="Tech Lead",
                roles=[Role(name="Tech Lead")],
                jira_project="BACK",
            ),
        ],
    )
    result = build_project_key_lead_map(org)
    assert result == {"INFRA": "cto", "BACK": "tech-lead"}


def test_root_level_role_no_atlassian_env_skipped() -> None:
    """Root-level role without atlassian mcp_env is silently skipped."""
    org = Organization(
        name="Test",
        roles=[Role(name="CEO")],
    )
    result = build_project_key_lead_map(org)
    assert result == {}


def test_root_level_role_shared_key_with_unit(caplog) -> None:
    """Root-level role sharing a project key with a unit triggers warning."""
    org = Organization(
        name="Test",
        roles=[
            Role(
                name="CTO",
                jira_project="SHARED",
            ),
        ],
        units=[
            OrgUnit(
                name="Platform",
                type="team",
                lead="Dev",
                roles=[Role(name="Dev")],
                jira_project="SHARED",
            ),
        ],
    )
    with caplog.at_level(logging.WARNING, logger="crewlet.config"):
        result = build_project_key_lead_map(org)
    # Unit comes first (scanned before root roles), so unit lead wins
    assert result == {"SHARED": "dev"}
    assert "jira_project_key_shared" in caplog.text
