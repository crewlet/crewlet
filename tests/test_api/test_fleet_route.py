"""``GET /fleet`` — the view ``/health`` cannot give.

``/health`` answers about the node that served it. Behind a load
balancer that means refreshing the page tells a different story, and
none of them answers the questions an operator actually has: which node
is running that agent, has the config reached everybody, is a seat being
served at all.

The lease table already holds all three, written by whoever holds them
and readable by anyone — so this is a read, not a fan-out of probes to
peers that may be mid-drain.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from typing import Any

import pytest
from starlette.testclient import TestClient

from crewlet.seat.placement import NodeProfile, parse_roles
from tests.test_api.helpers import create_app


class _MockQueue:
    async def publish(self, topic: str, event: Any) -> None: ...


class _FakeDB:
    """Answers the three lease reads and the apply-status read.

    Deliberately not a ``MemoryLeaseStore``: the route builds a
    ``LeaseStore`` over whatever ``app.state.database`` is, and going
    through the real store is what keeps this test honest about the SQL
    it issues.
    """

    def __init__(self, rows: list[dict[str, Any]], status: list[dict[str, Any]]):
        self.rows = rows
        self.status = status

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        q = " ".join(query.split())
        if "FROM config_apply_status" in q:
            return list(self.status)
        prefix = args[0] if args else ""
        return [r for r in self.rows if str(r["resource"]).startswith(prefix)]

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        return None

    async def fetchval(self, query: str, *args: Any) -> Any:
        return None


def _lease(resource: str, owner: str, **kw: Any) -> dict[str, Any]:
    return {
        "resource": resource,
        "owner": owner,
        "epoch": kw.get("epoch", 1),
        "expires_at": datetime.now(UTC) + timedelta(seconds=kw.get("ttl", 45)),
        "preferred": kw.get("preferred", ""),
        "protocol": kw.get("protocol", 3),
        "meta": kw.get("meta", {}),
    }


def _client(rows: list[dict[str, Any]], **kw: Any) -> TestClient:
    from crewlet.db.client import Database

    db = _FakeDB(rows, kw.pop("status", []))
    # The route type-checks for a Database; the fake answers its query
    # surface, so borrow the class rather than the connection pool.
    db.__class__ = type("_FakeDatabase", (Database,), dict(_FakeDB.__dict__))
    node_id = kw.pop("node_id", "node-a")
    app = create_app(_MockQueue(), database=db, **kw)
    app.state.node_id = node_id
    return TestClient(app)


@pytest.fixture
def fleet_rows() -> list[dict[str, Any]]:
    return [
        _lease(
            "node:core-1",
            "core-1:aaa",
            meta=NodeProfile(node_id="core-1").to_meta(),
        ),
        _lease(
            "node:sat-eu",
            "sat-eu:bbb",
            meta=NodeProfile(
                node_id="sat-eu",
                roles=parse_roles(["seats"]),
                labels={"zone": "eu"},
            ).to_meta(),
        ),
        _lease("seat:ceo", "core-1:aaa", epoch=4),
        _lease("seat:support", "sat-eu:bbb", epoch=2),
        _lease("worker:maintenance", "core-1:aaa"),
    ]


def test_it_reports_every_node_with_its_roles_and_labels(fleet_rows) -> None:
    body = _client(fleet_rows).get("/fleet").json()
    nodes = {n["id"]: n for n in body["nodes"]}
    assert set(nodes) == {"core-1", "sat-eu"}
    assert nodes["core-1"]["roles"] == ["ingress", "seats", "workers"]
    assert nodes["sat-eu"]["roles"] == ["seats"]
    assert nodes["sat-eu"]["labels"] == {"zone": "eu"}


def test_seat_ownership_is_attributed_to_the_node_not_the_incarnation(
    fleet_rows,
) -> None:
    """``owner`` is ``{node_id}:{random}``, minted per process. An
    operator recognises the node id; everything else is keyed on it."""
    body = _client(fleet_rows).get("/fleet").json()
    seats = {s["handle"]: s for s in body["seats"]}
    assert seats["ceo"]["node"] == "core-1"
    assert seats["support"]["node"] == "sat-eu"
    assert seats["ceo"]["epoch"] == 4
    nodes = {n["id"]: n for n in body["nodes"]}
    assert nodes["core-1"]["seats"] == 1
    assert nodes["sat-eu"]["seats"] == 1


def test_duties_name_the_node_holding_them(fleet_rows) -> None:
    body = _client(fleet_rows).get("/fleet").json()
    assert body["duties"] == [
        {"duty": "maintenance", "node": "core-1", "expires_in": pytest.approx(45, 1)}
    ]


def test_a_role_nobody_runs_is_reported(fleet_rows) -> None:
    """The shape no single node's config is wrong for. Every symptom is
    an absence, so it has to be said out loud somewhere."""
    seats_only = [r for r in fleet_rows if r["resource"] != "node:core-1"]
    body = _client(seats_only).get("/fleet").json()
    assert body["unmanned_roles"] == ["ingress", "workers"]


def test_a_seat_no_live_node_matches_is_reported(fleet_rows) -> None:
    """The pinned node died, or the label was a typo. Either way the
    seat is not being served and every node's own sweep looks fine."""
    roles = [
        {"handle": "support", "placement": {"labels": {"zone": "eu"}}},
        {"handle": "gpu-eng", "placement": {"labels": {"gpu": "true"}}},
        {"handle": "ceo", "placement": {}},
    ]
    body = _client(fleet_rows, agent_roles=roles).get("/fleet").json()
    # `support` is held, `ceo` is unpinned; only the GPU seat has nowhere
    # to go.
    assert body["unplaceable"] == [
        {"handle": "gpu-eng", "placement": "labels=gpu=true"}
    ]


def test_the_answering_node_names_itself(fleet_rows) -> None:
    """So a reader can tell "this is the node I reached" from "this is
    the node running that agent" — the distinction /health cannot make."""
    body = _client(fleet_rows, node_id="node-a").get("/fleet").json()
    assert body["this_node"] == "node-a"


def test_config_epoch_and_status_come_from_the_control_plane(fleet_rows) -> None:
    status = [
        {"node_id": "core-1", "epoch": 7, "status": "ok", "error": ""},
        {"node_id": "sat-eu", "epoch": 6, "status": "degraded", "error": "mcp gone"},
    ]
    body = _client(fleet_rows, status=status).get("/fleet").json()
    nodes = {n["id"]: n for n in body["nodes"]}
    assert nodes["core-1"]["config_epoch"] == 7
    assert nodes["sat-eu"]["config_status"] == "degraded"
    assert nodes["sat-eu"]["config_error"] == "mcp gone"


def test_without_a_database_it_says_so_rather_than_failing() -> None:
    """A single-process deployment has no lease table and no fleet. A
    500 there reads like a broken endpoint rather than an accurate
    answer."""
    app = create_app(_MockQueue())
    body = TestClient(app).get("/fleet").json()
    assert body["nodes"] == []
    assert "no database" in body["degraded"]
