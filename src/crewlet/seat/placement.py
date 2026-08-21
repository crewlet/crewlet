"""What a node does, and where a seat may run.

The vocabulary two layers have to agree on: Tier A declares it
(``node.roles`` / ``node.labels``), Tier B constrains against it
(``role.placement``), and :mod:`crewlet.seat.host` decides with it. It
imports nothing from the rest of Crewlet so both can depend on it, for
the same reason :mod:`crewlet.queue.topics` and
:mod:`crewlet.env_refs` do: a producer and a consumer that disagree
about a name never raise — they just quietly do different things.

**The capacity math is the part that is easy to get wrong.** Placement
is not a filter you can bolt onto a fair share computed over the whole
fleet. Nine seats pinned to one node and one seat free, over three
nodes: the naive share is ``ceil(10/3) = 4``, so the pinned node claims
four of its nine and the other five are claimable by nobody — stranded
forever, with every node reporting a healthy sweep. The share has to be
computed per *placement group*, over the nodes eligible for that group,
and summed. See :func:`plan_placement`.
"""

from __future__ import annotations

import math
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any


class NodeRole(StrEnum):
    """What a process is willing to do for the company.

    A node declares a subset; the default is all three, which is the
    single-process deployment and the shape every existing config
    already has.
    """

    #: Terminate inbound traffic: the HTTP API, the dashboard, and the
    #: webhooks every integration posts to. A fleet with none of these
    #: still runs its agents and never hears from the outside world.
    INGRESS = "ingress"
    #: Run agents: claim seat leases, spawn the instances, consume the
    #: inboxes, execute turns.
    SEATS = "seats"
    #: Run the company-wide singleton duties behind ``worker:{duty}``
    #: leases — the scheduler tick, the retention sweep, the sandbox
    #: waiter, skill clustering and curation, seat-subscription
    #: creation. Exactly one node does each at a time; a fleet with none
    #: of these does none of them.
    WORKERS = "workers"


DEFAULT_NODE_ROLES: frozenset[NodeRole] = frozenset(NodeRole)
"""Every role. A node that declares nothing does everything, which is
what one process running a whole company has always done."""


def parse_roles(values: Sequence[str] | None) -> frozenset[NodeRole]:
    """Coerce a sequence of role names, defaulting to all of them.

    Unknown names raise — a typo'd role in a bootstrap file must not
    silently subtract a duty from the fleet.
    """
    if not values:
        return DEFAULT_NODE_ROLES
    return frozenset(NodeRole(str(v)) for v in values)


@dataclass(frozen=True, slots=True)
class SeatPlacement:
    """Where a seat may run. Empty means anywhere.

    ``node`` pins to one node id; ``labels`` requires every pair to be
    present and equal on the node. Both may be given, and then both must
    hold — the conditions are ANDed, so a placement can only ever narrow.
    """

    node: str = ""
    labels: Mapping[str, str] = field(default_factory=dict)

    @property
    def is_anywhere(self) -> bool:
        return not self.node and not self.labels

    def matches(self, node_id: str, labels: Mapping[str, str]) -> bool:
        if self.node and self.node != node_id:
            return False
        return all(labels.get(k) == v for k, v in self.labels.items())

    @property
    def key(self) -> tuple[str, tuple[tuple[str, str], ...]]:
        """Group key. Seats with the same placement share a fair share."""
        return (self.node, tuple(sorted(self.labels.items())))

    def describe(self) -> str:
        parts = []
        if self.node:
            parts.append(f"node={self.node}")
        if self.labels:
            parts.append(
                "labels=" + ",".join(f"{k}={v}" for k, v in sorted(self.labels.items()))
            )
        return " ".join(parts) or "anywhere"


ANYWHERE = SeatPlacement()


@dataclass(frozen=True, slots=True)
class NodeProfile:
    """A live node as its peers see it: what it does and how it is labelled.

    Carried on the node's own presence lease (``node:{id}``) so every
    peer can answer "who is eligible for this seat" without a membership
    service. A node that is *present* but does not run seats must not
    count in the seat denominator — it would shrink everyone's share and
    strand the difference.
    """

    node_id: str
    roles: frozenset[NodeRole] = DEFAULT_NODE_ROLES
    labels: Mapping[str, str] = field(default_factory=dict)

    @property
    def runs_seats(self) -> bool:
        return NodeRole.SEATS in self.roles

    @property
    def runs_workers(self) -> bool:
        return NodeRole.WORKERS in self.roles

    @property
    def runs_ingress(self) -> bool:
        return NodeRole.INGRESS in self.roles

    def to_meta(self) -> dict[str, Any]:
        """The lease ``meta`` payload for this node's presence row."""
        return {
            "roles": sorted(str(r) for r in self.roles),
            "labels": dict(self.labels),
        }

    @classmethod
    def from_meta(cls, node_id: str, meta: Mapping[str, Any] | None) -> NodeProfile:
        """Read a peer's profile back off its presence lease.

        Tolerant on purpose. A presence row written by an older build has
        no ``meta`` at all, and the honest reading of that is the old
        behaviour: a node that does everything and is labelled with
        nothing. Anything malformed reads the same way rather than
        raising — a peer's bad row must not take down the reader's sweep.
        """
        raw = dict(meta or {})
        roles_raw = raw.get("roles")
        try:
            roles = parse_roles(roles_raw if isinstance(roles_raw, list) else None)
        except ValueError:
            roles = DEFAULT_NODE_ROLES
        labels_raw = raw.get("labels")
        labels = (
            {str(k): str(v) for k, v in labels_raw.items()}
            if isinstance(labels_raw, dict)
            else {}
        )
        return cls(node_id=node_id, roles=roles, labels=labels)


@dataclass(frozen=True, slots=True)
class PlacementPlan:
    """What this node may claim, how much of it, and what nobody can."""

    capacity: int
    """This node's fair share, summed over the placement groups it is
    eligible for. Zero for a node that does not run seats."""
    eligible: tuple[str, ...]
    """Seat handles this node is allowed to hold, in the given order."""
    unplaceable: tuple[str, ...]
    """Seats no live node matches. Not an error the engine can fix — a
    pin to a node that is down, or to a label nobody carries — but it
    must be visible, because the seat is simply not being served and
    every node's sweep otherwise looks perfectly healthy."""
    seat_nodes: int
    """How many live nodes run seats at all. The denominator that used to
    be "every live node"."""


def plan_placement(
    seats: Sequence[tuple[str, SeatPlacement]],
    *,
    me: NodeProfile,
    live: Sequence[NodeProfile],
) -> PlacementPlan:
    """Work out this node's share of a placement-constrained company.

    ``me`` is always counted as live whether or not the presence read
    returned it: before the first successful renew there is no row yet,
    and a store blip must not make a node invisible to itself.

    Seats are grouped by placement, each group's share is
    ``ceil(|group| / |nodes eligible for it|)``, and this node's capacity
    is the sum over the groups it belongs to. With no placements
    anywhere that collapses to exactly one group and the old
    ``ceil(seats / nodes)`` — the unconstrained fleet is the degenerate
    case, not a separate path.

    The share is a *ceiling*, which is what makes the give-back in
    :meth:`~crewlet.seat.host.SeatHost._shed_to_capacity` settle instead
    of oscillating: the shares sum to at least the seat count, so a node
    that has shed to its share cannot immediately re-claim what it let
    go.
    """
    profiles = {n.node_id: n for n in live if n.node_id}
    profiles[me.node_id] = me
    seat_nodes = [n for n in profiles.values() if n.runs_seats]

    groups: dict[tuple[str, tuple[tuple[str, str], ...]], list[str]] = {}
    placements: dict[tuple[str, tuple[tuple[str, str], ...]], SeatPlacement] = {}
    order: list[str] = []
    for handle, placement in seats:
        groups.setdefault(placement.key, []).append(handle)
        placements.setdefault(placement.key, placement)
        order.append(handle)

    capacity = 0
    eligible: list[str] = []
    unplaceable: list[str] = []
    for key, handles in groups.items():
        placement = placements[key]
        matching = [
            n for n in seat_nodes if placement.matches(n.node_id, dict(n.labels))
        ]
        if not matching:
            unplaceable.extend(handles)
            continue
        if not me.runs_seats or not placement.matches(me.node_id, dict(me.labels)):
            continue
        capacity += math.ceil(len(handles) / len(matching))
        eligible.extend(handles)

    eligible_set = set(eligible)
    unplaceable_set = set(unplaceable)
    return PlacementPlan(
        capacity=capacity,
        eligible=tuple(h for h in order if h in eligible_set),
        unplaceable=tuple(h for h in order if h in unplaceable_set),
        seat_nodes=len(seat_nodes),
    )


__all__ = [
    "ANYWHERE",
    "DEFAULT_NODE_ROLES",
    "NodeProfile",
    "NodeRole",
    "PlacementPlan",
    "SeatPlacement",
    "parse_roles",
    "plan_placement",
]
