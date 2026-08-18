"""Org-derived seat identity.

An agent seat's runtime id is *derived* from the org, never looked up in
a process: ``derive_agent_id`` is a ``uuid5`` over ``(org name, handle)``.
That is what lets any node in a fleet address a seat it is not running —
the routing paths in ``crewlet.events.subscriptions`` and the scheduler
invert this derivation on every event.
"""

from __future__ import annotations

from crewlet.db.agents import derive_agent_id
from crewlet.org.models import Organization, OrgUnit, Role


def _org() -> Organization:
    return Organization(
        name="TestCo",
        units=[
            OrgUnit(
                name="eng",
                type="team",
                lead="Tech Lead",
                roles=[
                    Role(name="Tech Lead", goal="lead", manages=["Developer"]),
                    Role(name="Developer", goal="code"),
                    Role(
                        name="Sarah Chen",
                        kind="human",
                        email="sarah@example.com",
                        contact={"slack_user_id": "U0HUMAN"},
                    ),
                ],
            )
        ],
        roles=[Role(name="Founder Bot", goal="oversee")],
    )


def test_the_id_has_no_process_local_input():
    org = _org()
    dev = org.get_role("Developer")
    assert dev is not None
    assert org.agent_id_for(dev) == str(derive_agent_id("TestCo", "developer"))


def test_two_orgs_with_the_same_seat_name_do_not_collide():
    """The org name is in the digest, so two companies sharing a Postgres
    can both have a ``developer``."""
    a = Organization(name="Acme", roles=[Role(name="Developer", goal="code")])
    b = Organization(name="Globex", roles=[Role(name="Developer", goal="code")])
    assert a.agent_id_for(a.all_roles()[0]) != b.agent_id_for(b.all_roles()[0])


def test_a_human_seat_has_no_agent_id():
    """Human seats are addressable but never spawned."""
    org = _org()
    sarah = org.get_role("Sarah Chen")
    assert sarah is not None
    assert org.agent_id_for(sarah) == ""


def test_an_unnamed_org_yields_no_id():
    """Without an org name the digest input is incomplete; returning ``""``
    beats deriving an id that a differently-named org would not match."""
    org = Organization(name="", roles=[Role(name="Developer", goal="code")])
    assert org.agent_id_for(org.all_roles()[0]) == ""


def test_seat_lookup_by_handle_spans_units_and_root():
    org = _org()
    assert org.agent_seat_by_handle("developer") is org.get_role("Developer")
    assert org.agent_seat_by_handle("founder-bot") is org.get_role("Founder Bot")
    assert org.agent_seat_by_handle("nobody") is None
    assert org.agent_seat_by_handle("") is None


def test_seat_lookup_by_handle_never_returns_a_human():
    """Handles are shared across kinds, but this lookup is the *agent*
    one — its callers publish to an inbox a human does not have."""
    org = _org()
    assert org.get_role("Sarah Chen") is not None
    assert org.agent_seat_by_handle("sarah-chen") is None


def test_id_lookup_inverts_the_derivation():
    org = _org()
    for role in org.all_roles():
        if role.is_human:
            continue
        assert org.agent_seat_by_id(org.agent_id_for(role)) is role


def test_id_lookup_rejects_unknown_and_empty():
    org = _org()
    assert org.agent_seat_by_id("") is None
    assert org.agent_seat_by_id("not-an-id") is None
    assert org.agent_seat_by_id(str(derive_agent_id("Globex", "developer"))) is None


def test_an_explicit_handle_beats_the_slugified_name():
    """``get_handle`` is the canonical identity, so the derivation and
    both lookups must agree with it."""
    org = Organization(
        name="TestCo",
        roles=[Role(name="Senior Backend Engineer", handle="sbe", goal="build")],
    )
    role = org.all_roles()[0]
    assert org.agent_id_for(role) == str(derive_agent_id("TestCo", "sbe"))
    assert org.agent_seat_by_handle("sbe") is role
    assert org.agent_seat_by_handle("senior-backend-engineer") is None
    assert org.agent_seat_by_id(org.agent_id_for(role)) is role
