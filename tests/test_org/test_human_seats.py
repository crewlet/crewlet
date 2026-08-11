"""Tests for human seats in the org chart (Role.kind == "human")."""

import pytest

from crewlet.org.hierarchy import get_manager, get_reports
from crewlet.org.models import (
    HumanContact,
    Organization,
    OrgUnit,
    Role,
    RoleKind,
    Schedule,
)


def make_human(**overrides) -> Role:
    """A minimal valid human seat."""
    data = {
        "name": "Sarah Chen",
        "kind": "human",
        "email": "sarah@acme.com",
        "contact": {"slack_user_id": "U0HUMAN"},
    }
    data.update(overrides)
    return Role(**data)


# ---------------------------------------------------------------- #
# Role-level validation
# ---------------------------------------------------------------- #


def test_default_kind_is_agent():
    role = Role(name="Engineer")
    assert role.kind == RoleKind.AGENT
    assert not role.is_human


def test_human_seat_minimal_valid():
    human = make_human()
    assert human.kind == RoleKind.HUMAN
    assert human.is_human
    assert human.get_handle() == "sarah-chen"


def test_human_seat_with_contact_only_is_reachable():
    human = make_human(email="", contact={"slack_user_id": "U0123"})
    assert human.contact is not None
    assert not human.contact.is_empty()


def test_human_seat_requires_contact_identity():
    # A human seat needs at least one contact ID so agents can mention
    # and reach them — an email alone is not a reach channel.
    with pytest.raises(ValueError, match="contact"):
        Role(name="Sarah", kind="human", email="s@x.com")
    # An empty contact block does not count.
    with pytest.raises(ValueError, match="contact"):
        Role(name="Sarah", kind="human", contact={})


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("llm", "gpt-4o"),
        ("llm", ["a", "b"]),
        ("llm_plan", "gpt-4o"),
        ("llm_execute", "gpt-4o"),
        ("llm_review", "gpt-4o"),
        ("llm_subagent", "gpt-4o"),
        ("llm_auxiliary", "gpt-4o"),
        ("llm_judge", "gpt-4o"),
        ("llm_sandbox", "sb-prov"),
        ("sandbox", {"enabled": True}),
        ("token_budget", 1000),
        ("learning_enabled", True),
        ("slack", {"bot_token": "xoxb-1"}),
        ("mcp_env", {"atlassian": {"JIRA_USERNAME": "s"}}),
        ("behavioral_guidelines", ["Always reply fast"]),
    ],
)
def test_human_seat_forbids_agent_only_fields(field, value):
    with pytest.raises(ValueError, match=f"agent-only fields.*{field}"):
        make_human(**{field: value})


def test_human_seat_forbids_schedules():
    schedule = {"name": "standup", "cron": "0 9 * * *", "task": "Post standup"}
    with pytest.raises(ValueError, match="agent-only fields.*schedules"):
        make_human(schedules=[schedule])


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("contact", {"slack_user_id": "U1"}),
        ("availability", "9-5 CET"),
    ],
)
def test_agent_seat_forbids_human_only_fields(field, value):
    with pytest.raises(ValueError, match="human-only fields"):
        Role(name="Engineer", **{field: value})


def test_human_contact_is_empty():
    assert HumanContact().is_empty()
    assert not HumanContact(github_login="sarah").is_empty()


def test_human_seat_keeps_descriptive_fields():
    human = make_human(
        goal="Keep the team unblocked",
        backstory="20 years in infra",
        responsibilities=["Approvals", "Vendor calls"],
        availability="CET business hours; replies within ~4h",
        manages=["Dev"],
    )
    assert human.goal
    assert human.backstory
    assert human.responsibilities
    assert human.availability
    assert human.manages == ["Dev"]


# ---------------------------------------------------------------- #
# Organization-level validation
# ---------------------------------------------------------------- #


def test_unit_each_schedule_requires_direct_agent_role():
    schedule = Schedule(name="standup", cron="0 9 * * *", task="standup")
    with pytest.raises(ValueError, match="no direct agent roles"):
        OrgUnit(name="Team", roles=[make_human()], schedules=[schedule])
    # Mixed unit is fine — fan-out simply skips the human.
    unit = OrgUnit(
        name="Team",
        roles=[make_human(), Role(name="Dev")],
        schedules=[Schedule(name="standup", cron="0 9 * * *", task="standup")],
    )
    assert unit.schedules


def test_lead_schedule_with_direct_human_lead_raises():
    with pytest.raises(ValueError, match="human"):
        Organization(
            name="T",
            units=[
                OrgUnit(
                    name="Team",
                    lead="Sarah Chen",
                    roles=[make_human(), Role(name="Dev")],
                    schedules=[
                        Schedule(
                            name="report",
                            cron="0 17 * * 5",
                            task="weekly report",
                            target="lead",
                        )
                    ],
                )
            ],
        )


def test_lead_schedule_with_inherited_human_lead_raises():
    with pytest.raises(ValueError, match="human"):
        Organization(
            name="T",
            units=[
                OrgUnit(
                    name="Dept",
                    lead="Sarah Chen",
                    roles=[make_human()],
                    children=[
                        OrgUnit(
                            name="Team",  # inherits the human lead
                            roles=[Role(name="Dev")],
                            schedules=[
                                Schedule(
                                    name="report",
                                    cron="0 17 * * 5",
                                    task="weekly report",
                                    target="lead",
                                )
                            ],
                        )
                    ],
                )
            ],
        )


def test_disabled_lead_schedule_with_human_lead_allowed():
    org = Organization(
        name="T",
        units=[
            OrgUnit(
                name="Team",
                lead="Sarah Chen",
                roles=[make_human(), Role(name="Dev")],
                schedules=[
                    Schedule(
                        name="report",
                        cron="0 17 * * 5",
                        task="weekly report",
                        target="lead",
                        enabled=False,
                    )
                ],
            )
        ],
    )
    assert org.get_unit("Team") is not None


# ---------------------------------------------------------------- #
# Hierarchy integration
# ---------------------------------------------------------------- #


def make_mixed_org() -> Organization:
    return Organization(
        name="Acme",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Sarah Chen",
                roles=[
                    make_human(),
                    Role(name="Dev A"),
                    Role(name="Dev B"),
                ],
            )
        ],
    )


def test_human_lead_auto_manages_agent_members():
    org = make_mixed_org()
    sarah = org.get_role("Sarah Chen")
    assert sarah is not None
    assert set(sarah.manages) == {"Dev A", "Dev B"}


def test_get_manager_returns_human_lead():
    org = make_mixed_org()
    dev = org.get_role("Dev A")
    assert dev is not None
    manager = get_manager(dev, org)
    assert manager is not None
    assert manager.name == "Sarah Chen"
    assert manager.is_human


def test_get_reports_includes_human_member():
    org = Organization(
        name="Acme",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Lead",
                roles=[
                    Role(name="Lead"),
                    make_human(),
                    Role(name="Dev"),
                ],
            )
        ],
    )
    lead = org.get_role("Lead")
    assert lead is not None
    reports = get_reports(lead, org)
    assert {r.name for r in reports} == {"Sarah Chen", "Dev"}
    kinds = {r.name: r.kind for r in reports}
    assert kinds["Sarah Chen"] == RoleKind.HUMAN
    assert kinds["Dev"] == RoleKind.AGENT


def test_to_api_dict_includes_kind():
    org = make_mixed_org()
    unit_dict = org.to_api_dict()["units"][0]
    by_name = {r["name"]: r for r in unit_dict["roles"]}
    assert by_name["Sarah Chen"]["kind"] == "human"
    assert by_name["Dev A"]["kind"] == "agent"


# ---------------------------------------------------------------- #
# Handle hygiene (format, uniqueness, contact normalization)
# ---------------------------------------------------------------- #


def test_explicit_handle_format_validated():
    # Would crash register_external_id at engine start otherwise.
    with pytest.raises(ValueError, match="must match"):
        Role(name="Sarah", handle="Sarah_Chen")
    with pytest.raises(ValueError, match="must match"):
        make_human(handle="Sarah_Chen")
    # Conforming explicit handles pass.
    assert Role(name="Sarah", handle="sarah-2").get_handle() == "sarah-2"


def test_duplicate_handles_rejected_org_wide():
    with pytest.raises(ValueError, match="duplicate handle"):
        Organization(
            name="T",
            roles=[
                Role(name="Sarah", handle="sarah"),
                make_human(name="Sarah Ops", handle="sarah"),
            ],
        )
    # Agent/agent collisions are equally fatal (shared inbox topic).
    with pytest.raises(ValueError, match="duplicate handle"):
        Organization(
            name="T",
            roles=[Role(name="Dev", handle="dev"), Role(name="Dev Two", handle="dev")],
        )


def test_github_login_normalized_to_lowercase():
    human = make_human(contact={"github_login": "JaneDoe"})
    assert human.contact.github_login == "janedoe"


def test_contact_identities_enumeration():
    human = make_human(
        contact={
            "slack_user_id": "U1",
            "atlassian_account_id": "abc",
            "github_login": "jane",
        }
    )
    assert human.contact.resolved_identities() == [
        ("slack", "U1"),
        ("jira", "abc"),
        ("confluence", "abc"),
        ("github", "jane"),
    ]


def test_env_ref_contact_values_survive_validation_unmangled():
    # A ${VAR} reference is an env indirection, not a literal identity:
    # lowercasing it would break (case-sensitive) resolution forever.
    human = make_human(
        contact={
            "github_login": "${GH_FOUNDER_LOGIN}",
            "gitlab_username": "${GL_FOUNDER_USERNAME}",
            "plane_user_id": "${PLANE_FOUNDER_USER_ID}",
        }
    )
    assert human.contact.github_login == "${GH_FOUNDER_LOGIN}"
    assert human.contact.gitlab_username == "${GL_FOUNDER_USERNAME}"
    assert human.contact.plane_user_id == "${PLANE_FOUNDER_USER_ID}"
    # A declared reference counts as an identity for validation.
    assert not human.contact.is_empty()


def test_resolved_identities_resolves_env_ref_then_normalizes(monkeypatch):
    monkeypatch.setenv("PLANE_FOUNDER_USER_ID", "AB12CD34-0000-0000-0000-000000000001")
    human = make_human(
        contact={
            "slack_user_id": "U1",
            "plane_user_id": "${PLANE_FOUNDER_USER_ID}",
        }
    )
    # Resolution happens at consumption time, then lowercasing —
    # webhook payloads carry lowercase Plane UUIDs.
    assert human.contact.resolved_identities() == [
        ("slack", "U1"),
        ("plane", "ab12cd34-0000-0000-0000-000000000001"),
    ]


def test_resolved_identities_omits_unresolved_env_ref(monkeypatch):
    monkeypatch.delenv("PLANE_FOUNDER_USER_ID", raising=False)
    human = make_human(
        contact={
            "slack_user_id": "U1",
            "plane_user_id": "${PLANE_FOUNDER_USER_ID}",
        }
    )
    # Never emit the raw ${VAR} text — no webhook payload or platform
    # lookup could ever match it.  The other identities still resolve.
    assert human.contact.resolved_identities() == [("slack", "U1")]


def test_contact_values_whitespace_stripped(monkeypatch):
    # Whitespace is never part of an identity.  A padded literal is
    # stripped (then lowercased where applicable), and a padded ${VAR}
    # reference is stripped down to the whole-value form so it still
    # resolves — instead of being silently case-mangled into
    # " ${gh_founder_login} " and dropped at resolution time.
    monkeypatch.setenv("GH_FOUNDER_LOGIN", "JaneDoe")
    human = make_human(
        contact={
            "slack_user_id": "  U1  ",
            "github_login": "  ${GH_FOUNDER_LOGIN}  ",
        }
    )
    assert human.contact.slack_user_id == "U1"
    assert human.contact.github_login == "${GH_FOUNDER_LOGIN}"
    assert human.contact.resolved_identities() == [
        ("slack", "U1"),
        ("github", "janedoe"),
    ]


def test_contact_embedded_env_ref_rejected():
    # A value that merely CONTAINS ${...} is neither a literal ID nor a
    # whole-value reference — half-substituting it would silently
    # register a truncated/mangled identity, so validation rejects it
    # loudly, naming the field.
    with pytest.raises(ValueError, match="github_login"):
        make_human(contact={"github_login": "acme-${GH_SUFFIX}"})
    # Same for the non-case-normalized fields.
    with pytest.raises(ValueError, match="slack_user_id"):
        make_human(contact={"slack_user_id": "U${SLACK_SUFFIX}"})
    # Two adjacent whole-value references are still an embedded form.
    with pytest.raises(ValueError, match="plane_user_id"):
        make_human(contact={"plane_user_id": "${A}${B}"})
