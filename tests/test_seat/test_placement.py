"""The placement arithmetic, which is the part that is easy to get wrong.

Placement is not a filter you bolt onto a fleet-wide fair share. The
share has to be computed per placement group, over the nodes eligible
for that group — and the test that proves it is
``test_a_pinned_majority_is_not_stranded_by_a_fleet_wide_ratio``: under
the naive ratio every node reports a healthy sweep while five seats are
served by nobody.
"""

from __future__ import annotations

import pytest

from crewlet.seat.placement import (
    DEFAULT_NODE_ROLES,
    NodeProfile,
    NodeRole,
    SeatPlacement,
    parse_roles,
    plan_placement,
)


def _node(node_id: str, *, roles: set[str] | None = None, **labels: str) -> NodeProfile:
    return NodeProfile(
        node_id=node_id,
        roles=parse_roles(sorted(roles)) if roles else DEFAULT_NODE_ROLES,
        labels=labels,
    )


ANY = SeatPlacement()


# ── matching ─────────────────────────────────────────────────────────


def test_an_empty_placement_matches_everything() -> None:
    assert ANY.is_anywhere
    assert ANY.matches("whatever", {})


def test_a_node_pin_is_exact() -> None:
    pin = SeatPlacement(node="sat-1")
    assert pin.matches("sat-1", {})
    assert not pin.matches("sat-10", {})
    assert not pin.matches("core-1", {})


def test_labels_must_all_match() -> None:
    sel = SeatPlacement(labels={"zone": "eu", "gpu": "true"})
    assert sel.matches("n", {"zone": "eu", "gpu": "true", "extra": "ok"})
    assert not sel.matches("n", {"zone": "eu"})
    assert not sel.matches("n", {"zone": "us", "gpu": "true"})


def test_a_node_pin_and_labels_are_anded() -> None:
    """Both conditions, so a placement can only ever narrow."""
    sel = SeatPlacement(node="sat-1", labels={"zone": "eu"})
    assert sel.matches("sat-1", {"zone": "eu"})
    assert not sel.matches("sat-1", {"zone": "us"})
    assert not sel.matches("sat-2", {"zone": "eu"})


# ── the share ────────────────────────────────────────────────────────


def test_no_placement_anywhere_is_the_old_arithmetic() -> None:
    """The unconstrained fleet is the degenerate case, not a second path."""
    seats = [(h, ANY) for h in ("a", "b", "c", "d", "e")]
    live = [_node("n1"), _node("n2"), _node("n3")]
    plan = plan_placement(seats, me=live[0], live=live)
    assert plan.capacity == 2  # ceil(5 / 3)
    assert plan.eligible == ("a", "b", "c", "d", "e")
    assert plan.unplaceable == ()


def test_a_pinned_majority_is_not_stranded_by_a_fleet_wide_ratio() -> None:
    """The reason the share is per-group.

    Nine seats pinned to one node and one free, over three nodes: a
    fleet-wide ``ceil(10/3) = 4`` lets the pinned node take four of its
    nine, and the other five are claimable by nobody — stranded forever,
    while every node in the fleet reports a perfectly healthy sweep.
    """
    pinned = SeatPlacement(node="big")
    seats = [(f"p{i}", pinned) for i in range(9)] + [("free", ANY)]
    live = [_node("big"), _node("n2"), _node("n3")]

    big = plan_placement(seats, me=live[0], live=live)
    # Nine pinned seats, one eligible node → all nine, plus its third of
    # the one free seat.
    assert big.capacity == 9 + 1
    assert set(big.eligible) == {f"p{i}" for i in range(9)} | {"free"}

    other = plan_placement(seats, me=live[1], live=live)
    assert other.capacity == 1
    assert other.eligible == ("free",)


def test_a_seat_no_live_node_matches_is_reported_not_widened() -> None:
    """The one placement failure that is otherwise invisible."""
    seats = [("a", ANY), ("gpu", SeatPlacement(labels={"gpu": "true"}))]
    live = [_node("n1"), _node("n2")]
    plan = plan_placement(seats, me=live[0], live=live)
    assert plan.unplaceable == ("gpu",)
    assert plan.eligible == ("a",)
    assert plan.capacity == 1


def test_a_node_that_does_not_run_seats_is_not_in_the_denominator() -> None:
    """An ingress-only node would otherwise shrink everyone's share and
    strand the difference — it is present, and it will never claim."""
    seats = [(h, ANY) for h in ("a", "b", "c", "d")]
    live = [_node("n1"), _node("n2"), _node("api", roles={"ingress"})]
    plan = plan_placement(seats, me=live[0], live=live)
    assert plan.seat_nodes == 2
    assert plan.capacity == 2  # ceil(4 / 2), not ceil(4 / 3)


def test_a_node_that_does_not_run_seats_claims_nothing() -> None:
    api = _node("api", roles={"ingress", "workers"})
    plan = plan_placement([("a", ANY)], me=api, live=[api, _node("n1")])
    assert plan.capacity == 0
    assert plan.eligible == ()


def test_this_node_counts_even_before_its_presence_row_lands() -> None:
    """First sweep of the first node, and every store blip after.

    A node that is invisible to itself computes a share out of a fleet it
    is not in — zero eligible nodes for every group, so it claims
    nothing and reports every seat unplaceable.
    """
    me = _node("n1")
    plan = plan_placement([("a", ANY), ("b", ANY)], me=me, live=[])
    assert plan.capacity == 2
    assert plan.unplaceable == ()
    assert plan.seat_nodes == 1


def test_a_stale_profile_for_this_node_does_not_win() -> None:
    """``me`` is authoritative about itself.

    Its own presence row was written by its previous incarnation and can
    describe roles or labels this process no longer has; believing the
    row over the process would make a node claim seats it is no longer
    configured for.
    """
    me = _node("n1", zone="us")
    stale = _node("n1", zone="eu")
    seats = [("eu-seat", SeatPlacement(labels={"zone": "eu"}))]
    plan = plan_placement(seats, me=me, live=[stale])
    assert plan.eligible == ()
    assert plan.unplaceable == ("eu-seat",)


def test_shares_sum_to_at_least_the_seat_count() -> None:
    """What makes the give-back settle instead of oscillating.

    A ceiling per group means a node that has shed down to its share has
    no room to immediately re-claim what it let go.
    """
    seats = [(f"s{i}", ANY) for i in range(7)]
    live = [_node(f"n{i}") for i in range(3)]
    total = sum(plan_placement(seats, me=n, live=live).capacity for n in live)
    assert total >= len(seats)


# ── the node profile on the wire ─────────────────────────────────────


def test_a_profile_round_trips_through_lease_meta() -> None:
    me = _node("n1", roles={"seats", "workers"}, zone="eu")
    back = NodeProfile.from_meta("n1", me.to_meta())
    assert back == me


def test_a_presence_row_with_no_meta_reads_as_the_old_behaviour() -> None:
    """A peer running a build that predates the column. "Does everything,
    labelled with nothing" is the only safe reading — the alternative is
    a node with no roles, which would drop a live peer out of the
    denominator and over-subscribe the rest of the fleet."""
    back = NodeProfile.from_meta("old", None)
    assert back.roles == DEFAULT_NODE_ROLES
    assert back.labels == {}
    assert back.runs_seats


def test_a_malformed_profile_reads_as_the_old_behaviour_too() -> None:
    """A peer's bad row must not raise inside the reader's sweep."""
    for junk in ({"roles": "seats"}, {"roles": ["nonsense"]}, {"labels": 7}):
        back = NodeProfile.from_meta("weird", junk)
        assert back.roles == DEFAULT_NODE_ROLES
        assert back.labels == {}


def test_parse_roles_rejects_a_typo() -> None:
    """A typo'd role in a bootstrap file must not silently subtract a
    duty from the fleet."""
    with pytest.raises(ValueError):
        parse_roles(["seat"])  # singular — the plausible typo
    assert parse_roles(["seats"]) == frozenset({NodeRole.SEATS})
    assert parse_roles([]) == DEFAULT_NODE_ROLES
    assert parse_roles(None) == DEFAULT_NODE_ROLES
