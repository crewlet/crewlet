"""Unit tests for ``accessible_spaces`` / ``accessible_projects``.

Read scope for the query-time knowledge search is an org-wide list and
nothing else: ``Organization.confluence_spaces`` for the Confluence
backend, ``Organization.plane_projects`` for the Plane backend.  Both
sets are role-independent: a unit's / role's ``confluence_space`` /
``plane_project`` *identity* (routing + write home) deliberately does
not narrow reads.  Tests cover the empty case, normalisation, and that
per-team identity is ignored.
"""

from __future__ import annotations

from crewlet.knowledge.accessibility import accessible_projects, accessible_spaces
from crewlet.org.models import Organization, OrgUnit, Role

# ---------------------------------------------------------------------------
# Empty / no-config cases
# ---------------------------------------------------------------------------


def test_empty_org_returns_empty_set() -> None:
    org = Organization(name="Empty", roles=[Role(name="Founder")])
    assert accessible_spaces(org) == set()


def test_no_confluence_spaces_returns_empty_set() -> None:
    """Units/roles with their own space identity, but no org-wide list,
    still yield an empty read scope (→ unscoped/ACL-bound downstream)."""
    dev = Role(name="Dev", confluence_space="BACKEND")
    org = Organization(
        name="Acme",
        units=[OrgUnit(name="Backend", confluence_space="BACKEND", roles=[dev])],
        roles=[Role(name="CTO", confluence_space="EXEC")],
    )
    assert accessible_spaces(org) == set()


# ---------------------------------------------------------------------------
# Org-wide list is the read scope
# ---------------------------------------------------------------------------


def test_org_spaces_are_the_read_scope() -> None:
    org = Organization(name="Acme", confluence_spaces=["HANDBOOK", "GENERAL"])
    assert accessible_spaces(org) == {"HANDBOOK", "GENERAL"}


def test_read_scope_is_role_and_unit_independent() -> None:
    """The key decoupling guarantee: a unit's / role's ``confluence_space``
    identity is NOT added to the read scope -- only the org-wide list is."""
    dev = Role(name="Dev", confluence_space="BACKEND")
    cto = Role(name="CTO", confluence_space="EXEC")
    org = Organization(
        name="Acme",
        confluence_spaces=["HANDBOOK"],
        roles=[cto],
        units=[OrgUnit(name="Engineering", confluence_space="ENG", roles=[dev])],
    )
    # BACKEND / EXEC / ENG identities are ignored -- only HANDBOOK scopes reads.
    assert accessible_spaces(org) == {"HANDBOOK"}


# ---------------------------------------------------------------------------
# Normalisation
# ---------------------------------------------------------------------------


def test_lowercase_keys_normalised_to_uppercase() -> None:
    org = Organization(name="Acme", confluence_spaces=["handbook", "Eng"])
    assert accessible_spaces(org) == {"HANDBOOK", "ENG"}


def test_blank_space_keys_filtered_out() -> None:
    org = Organization(name="Acme", confluence_spaces=["", "  ", "HANDBOOK"])
    assert accessible_spaces(org) == {"HANDBOOK"}


def test_duplicate_keys_deduped() -> None:
    org = Organization(name="Acme", confluence_spaces=["ENG", "eng", " ENG "])
    assert accessible_spaces(org) == {"ENG"}


# ---------------------------------------------------------------------------
# accessible_projects -- the Plane analog, identical contract
# ---------------------------------------------------------------------------


def test_projects_empty_org_returns_empty_set() -> None:
    org = Organization(name="Empty", roles=[Role(name="Founder")])
    assert accessible_projects(org) == set()


def test_projects_identity_alone_yields_empty_read_scope() -> None:
    """Units/roles with their own project identity, but no org-wide
    list, still yield an empty read scope (→ unscoped / membership-bound
    downstream)."""
    dev = Role(name="Dev", plane_project="BACKEND")
    org = Organization(
        name="Acme",
        units=[OrgUnit(name="Backend", plane_project="BACKEND", roles=[dev])],
        roles=[Role(name="CTO", plane_project="EXEC")],
    )
    assert accessible_projects(org) == set()


def test_org_projects_are_the_read_scope() -> None:
    org = Organization(name="Acme", plane_projects=["HANDBOOK", "GENERAL"])
    assert accessible_projects(org) == {"HANDBOOK", "GENERAL"}


def test_projects_read_scope_is_role_and_unit_independent() -> None:
    """A unit's / role's ``plane_project`` identity is NOT added to the
    read scope -- only the org-wide list is."""
    dev = Role(name="Dev", plane_project="BACKEND")
    cto = Role(name="CTO", plane_project="EXEC")
    org = Organization(
        name="Acme",
        plane_projects=["HANDBOOK"],
        roles=[cto],
        units=[OrgUnit(name="Engineering", plane_project="ENG", roles=[dev])],
    )
    assert accessible_projects(org) == {"HANDBOOK"}


def test_projects_normalised_uppercased_blank_filtered_deduped() -> None:
    org = Organization(
        name="Acme",
        plane_projects=["handbook", "", "  ", "Eng", "eng", " ENG "],
    )
    assert accessible_projects(org) == {"HANDBOOK", "ENG"}
