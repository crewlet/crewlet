"""GET /fleet — every node, what it runs, and what it holds.

``/health`` answers about the process that served it, which is the right
answer to "is this node alive" and the wrong one to every question an
operator actually has behind a load balancer: which node is running that
agent, has the config reached everybody, is a seat being served at all.
Refreshing the page hits a different node and tells a different story.

The lease table already knows. Node presence, seat ownership and the
worker duties are all rows in it, written by whoever holds them and
readable by anyone — so this view is a read, not a fan-out of health
probes to peers that may be mid-drain. The per-node config epoch comes
from the control plane's apply status for the same reason.

Its own limit: leases say who *holds* a seat, not what that node is doing
with it right now. In-flight counts stay on ``/health``, which is the
only place that can honestly answer them.
"""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet._logging import get_logger

logger = get_logger("api.routes.fleet")


class FleetUnavailable(Exception):
    """The lease store could not be read at all.

    Distinct from the degraded no-database answer: without a database
    there is genuinely no fleet, and saying so is the truth. A lease
    store that is configured and failing is an outage, and every surface
    has to report it as one rather than as an empty fleet — an empty
    node list reads as "nothing is running", which is the opposite of
    what an unreadable lease table means.
    """


def _database(app: Any) -> Any:
    from crewlet.db.client import Database

    db = getattr(app.state, "database", None)
    return db if isinstance(db, Database) else None


def _seconds_left(expires_at: Any, now: datetime) -> float:
    try:
        return max(0.0, (expires_at - now).total_seconds())
    except Exception:
        return 0.0


async def get_fleet(request: Request) -> JSONResponse:
    """GET /fleet — nodes, their roles and labels, and their seats."""
    try:
        return JSONResponse(await fleet_payload(request.app))
    except FleetUnavailable:
        return JSONResponse({"error": "lease store unavailable"}, status_code=503)


async def fleet_payload(app: Any) -> dict[str, Any]:
    """The fleet, as both surfaces answer it.

    One function behind the REST route and the websocket query, for the
    reason every other read in ``api/queries.py`` shares one: two
    implementations of "what is the fleet" can disagree, and the moment
    they do, which answer an operator gets depends on which transport
    their browser happened to use.

    Degrades rather than fails. Without a database there is no lease
    table and therefore no fleet — the answer is this one node, which is
    exactly what a single-process deployment is, and saying so beats a
    500 that reads like a broken endpoint.
    """
    from crewlet.config import DEFAULT_NODE_ID
    from crewlet.seat.placement import NodeProfile

    db = _database(app)
    this_node = str(getattr(app.state, "node_id", "") or DEFAULT_NODE_ID)

    if db is None:
        return {
            "nodes": [],
            "seats": [],
            "duties": [],
            "unplaceable": [],
            "unmanned_roles": [],
            "this_node": this_node,
            "degraded": "no database configured — leases are process-local",
        }

    from crewlet.db.leases import LeaseError, LeaseStore

    leases = LeaseStore(db)
    now = datetime.now(UTC)
    try:
        node_rows = await leases.list_live("node:")
        seat_rows = await leases.list_live("seat:")
        duty_rows = await leases.list_live("worker:")
    except LeaseError as exc:
        logger.warning("fleet_read_failed", error=str(exc))
        raise FleetUnavailable(str(exc)) from exc

    # owner is an incarnation (``{node_id}:{random}``); the node id is
    # what an operator recognises, and what everything else is keyed on.
    def _node_of(owner: str) -> str:
        return owner.split(":", 1)[0] if owner else ""

    seats: list[dict[str, Any]] = []
    seats_by_node: dict[str, int] = {}
    for lease in seat_rows:
        handle = lease.resource.removeprefix("seat:")
        node = _node_of(lease.owner)
        seats_by_node[node] = seats_by_node.get(node, 0) + 1
        seats.append(
            {
                "handle": handle,
                "node": node,
                "owner": lease.owner,
                "epoch": lease.epoch,
                "expires_in": _seconds_left(lease.expires_at, now),
            }
        )
    seats.sort(key=lambda s: str(s["handle"]))

    applied = await _apply_status(db)
    nodes: list[dict[str, Any]] = []
    for lease in node_rows:
        node_id = lease.resource.removeprefix("node:")
        profile = NodeProfile.from_meta(node_id, lease.meta)
        status = applied.get(node_id, {})
        nodes.append(
            {
                "id": node_id,
                "roles": sorted(str(r) for r in profile.roles),
                "labels": dict(profile.labels),
                "owner": lease.owner,
                "protocol": lease.protocol,
                "seats": seats_by_node.get(node_id, 0),
                "expires_in": _seconds_left(lease.expires_at, now),
                "config_epoch": status.get("epoch", 0),
                "config_status": status.get("status", ""),
                "config_error": status.get("error", ""),
                # When this node last reported an apply outcome. Fetched
                # and then dropped before: a node that stopped reporting
                # is exactly the one an operator is looking for, and
                # without this its stale row is indistinguishable from a
                # node that reported the same epoch a second ago.
                "config_reported_at": status.get("reported_at", ""),
            }
        )
    nodes.sort(key=lambda n: str(n["id"]))

    duties = sorted(
        (
            {
                "duty": lease.resource.removeprefix("worker:"),
                "node": _node_of(lease.owner),
                "expires_in": _seconds_left(lease.expires_at, now),
            }
            for lease in duty_rows
        ),
        key=lambda d: str(d["duty"]),
    )

    return {
        "nodes": nodes,
        "seats": seats,
        "duties": duties,
        "unplaceable": _unplaceable(app, nodes, seats),
        "unmanned_roles": _unmanned_roles(nodes),
        "this_node": this_node,
        # What the fleet is converging ON, so a lagging node reads as
        # "3 epochs behind 41" rather than as a number with nothing to
        # compare it to.
        "target_epoch": await _target_epoch(db),
    }


async def _target_epoch(db: Any) -> int:
    """The newest activation epoch — the one every node is heading for."""
    try:
        from crewlet.db.config_plane import ConfigPlaneStore

        return int(await ConfigPlaneStore(db).target() or 0)
    except Exception:
        logger.exception("fleet_target_epoch_failed")
        return 0


async def _apply_status(db: Any) -> dict[str, dict[str, Any]]:
    """Per-node config apply state, keyed by node id.

    A node whose epoch lags is not an error by itself — every successful
    rollout produces lag — so this reports the numbers and leaves the
    reading to whoever is looking. See ``docs/concepts/control-plane.md``.
    """
    try:
        from crewlet.db.config_plane import ConfigPlaneStore

        rows = await ConfigPlaneStore(db).fleet()
    except Exception:
        logger.exception("fleet_apply_status_failed")
        return {}
    return {
        str(row["node_id"]): {
            "epoch": int(row["epoch"] or 0),
            "status": str(row["status"] or ""),
            "error": str(row["error"] or ""),
            "reported_at": _iso(row.get("updated_at")),
        }
        for row in rows
    }


def _iso(value: Any) -> str:
    try:
        return value.isoformat() if value is not None else ""
    except Exception:
        return ""


def _unmanned_roles(nodes: list[dict[str, Any]]) -> list[str]:
    """Roles no live node performs — the shape no node's config is wrong for."""
    from crewlet.seat.placement import NodeRole

    manned = {role for node in nodes for role in node.get("roles", [])}
    return sorted(str(r) for r in NodeRole if str(r) not in manned)


def _unplaceable(
    app: Any, nodes: list[dict[str, Any]], seats: list[dict[str, Any]]
) -> list[dict[str, str]]:
    """Seats in the org that no live node is eligible to run.

    Computed here rather than reported by a node, because the node that
    reports it is the one that noticed — and when the pinned node is the
    one that died, the survivors are exactly who should be able to say
    so.

    Read off ``agent_roles``, the same projection ``GET /agents`` uses,
    so the handle derivation and the human-seat exclusion are shared. A
    second walk over the payload would have to repeat both, and a seat
    whose handle it derived differently would show here as unplaceable
    forever.
    """
    from crewlet.seat.placement import NodeProfile, SeatPlacement, parse_roles

    profiles = [
        NodeProfile(
            node_id=str(n["id"]),
            roles=parse_roles(list(n.get("roles") or [])),
            labels=dict(n.get("labels") or {}),
        )
        for n in nodes
    ]
    seat_nodes = [p for p in profiles if p.runs_seats]
    held = {str(s["handle"]) for s in seats}

    out: list[dict[str, str]] = []
    for role in getattr(app.state, "agent_roles", None) or []:
        handle = str(role.get("handle") or "")
        raw = role.get("placement") or {}
        if not handle or handle in held or not raw:
            continue
        placement = SeatPlacement(
            node=str(raw.get("node") or ""),
            labels={str(k): str(v) for k, v in (raw.get("labels") or {}).items()},
        )
        if any(placement.matches(p.node_id, dict(p.labels)) for p in seat_nodes):
            continue
        out.append({"handle": handle, "placement": placement.describe()})
    return out
