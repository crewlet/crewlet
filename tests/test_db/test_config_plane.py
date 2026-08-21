"""The posture decision, and the memory twin of the control-plane store.

``decide_posture`` is small enough to read and subtle enough to get
backwards — the first draft of this design did, and the tests below are
mostly a record of the ways it can be wrong.  The governing rule:

    the target is the store's activation pointer, but **lag alone is
    never a reason to shed**.

Every successful rollout produces lag.  The first node to apply advances
the pointer and every peer is behind until it polls, so a rule that
sheds on lag makes the fastest node the cause of a fleet-wide outage —
and the faster it is, the longer the outage lasts.
"""

from __future__ import annotations

import os
from typing import Any
from uuid import uuid4

import pytest

from crewlet.db.config_plane import (
    APPLY_STATUS_RETENTION_SECONDS,
    LAG_GRACE_TICKS,
    MAX_APPLY_ATTEMPTS,
    PEER_STATUS_FRESH_SECONDS,
    RECONCILE_INTERVAL_SECONDS,
    ApplyStatus,
    FleetView,
    MemoryConfigPlaneStore,
    Posture,
    decide_posture,
    reconcile_delay,
)

_TEST_DSN = os.environ.get("CREWLET_TEST_DSN", "")


def _view(**kwargs) -> FleetView:
    base = {
        "target_epoch": 5,
        "applied_epoch": 4,
        "ticks_behind": 0,
        "attempts": 0,
        "peers_ok": 0,
        "peers_reported": 0,
        "self_status": None,
    }
    base.update(kwargs)
    return FleetView(**base)  # type: ignore[arg-type]


# ── converged ────────────────────────────────────────────────────────


def test_converged_node_serves() -> None:
    assert decide_posture(_view(applied_epoch=5)) is Posture.SERVE


def test_a_node_ahead_of_the_pointer_serves() -> None:
    """Ahead can happen legitimately: this node converged on epoch 6 and
    is reading a pointer snapshot from before the write it just saw."""
    assert decide_posture(_view(applied_epoch=6)) is Posture.SERVE


# ── the rule the first draft got backwards ───────────────────────────


def test_fresh_lag_waits_and_never_sheds() -> None:
    """The single most important case.

    A node one tick behind a just-advanced pointer is not broken — it is
    a node that has not polled yet.  Shedding here is what turns every
    successful rollout into a fleet-wide outage.
    """
    assert decide_posture(_view(ticks_behind=1)) is Posture.WAIT


def test_lag_within_grace_waits_even_with_healthy_peers() -> None:
    """Healthy peers are not evidence that THIS node is broken.

    During a rollout the peers that already applied are exactly what
    makes lag look suspicious, so this is the case a naive
    "peers are ahead → step aside" rule gets wrong.
    """
    view = _view(ticks_behind=LAG_GRACE_TICKS - 1, peers_ok=3, peers_reported=3)
    assert decide_posture(view) is Posture.WAIT


def test_silence_is_not_evidence() -> None:
    """Past grace, but nobody has reported at all — not even us.

    No node has attempted the epoch, so there is nothing to conclude.
    Do not shed on silence.
    """
    view = _view(ticks_behind=LAG_GRACE_TICKS + 2)
    assert decide_posture(view) is Posture.WAIT


# ── confirmed lag ────────────────────────────────────────────────────


def test_confirmed_by_own_failure_with_healthy_peers_sheds() -> None:
    """This node tried and failed; peers have the epoch. Step aside."""
    view = _view(self_status=ApplyStatus.ERROR, peers_ok=2, peers_reported=2)
    assert decide_posture(view) is Posture.SHED


def test_confirmed_by_duration_with_healthy_peers_sheds() -> None:
    """No failure recorded, but the lag outlasted propagation."""
    view = _view(ticks_behind=LAG_GRACE_TICKS, peers_ok=1, peers_reported=1)
    assert decide_posture(view) is Posture.SHED


def test_degraded_self_confirms_lag_like_an_error() -> None:
    view = _view(self_status=ApplyStatus.DEGRADED, peers_ok=1, peers_reported=1)
    assert decide_posture(view) is Posture.SHED


def test_nobody_could_apply_it_is_isolated_not_shed() -> None:
    """Everyone who tried, failed — so the REVISION is the problem.

    Shedding here would take the whole fleet out over one bad revision,
    which is precisely what the rollback path exists to avoid.  The node
    keeps serving what rollback preserved and raises divergence instead.
    """
    view = _view(self_status=ApplyStatus.ERROR, peers_ok=0, peers_reported=3)
    assert decide_posture(view) is Posture.ISOLATED


def test_a_degraded_peer_does_not_count_as_healthy() -> None:
    """Degraded peers report, but are not somewhere work can go.

    ``peer_health`` counts them in ``reported`` and not in ``ok``, so a
    fleet where every peer went degraded resolves to ISOLATED rather
    than shedding onto a node whose tool surface is amputated.
    """
    view = _view(self_status=ApplyStatus.ERROR, peers_ok=0, peers_reported=2)
    assert decide_posture(view) is Posture.ISOLATED


# ── bounded retry ────────────────────────────────────────────────────


def test_exhausted_attempts_go_stuck_when_a_peer_can_take_the_work() -> None:
    """Exhaustion means THIS node is the anomaly — but only when the
    epoch demonstrably applies somewhere else.

    Retry is bounded either way: `_apply_attempts` gates `_converge_to`
    directly, so a node stops re-applying (and restarting its MCP
    children) after the ceiling whatever posture it reports. Posture
    decides who serves, not whether to retry.
    """
    view = _view(
        attempts=MAX_APPLY_ATTEMPTS,
        self_status=ApplyStatus.ERROR,
        peers_ok=2,
        peers_reported=3,
    )
    assert decide_posture(view) is Posture.STUCK


def test_isolated_outranks_exhausted_attempts() -> None:
    """The inversion that mattered, and the reason it did.

    Shedding exists to move work to a HEALTHY PEER. With `peers_ok == 0`
    there is no healthy peer, so shedding is not shedding — it is
    stopping. And every node in a fleet that cannot apply a revision
    reaches this state at the same time, so ranking STUCK above ISOLATED
    took the whole company dark roughly 45 s after a bad activation:
    all seats released, inbound paused, `/ready` 503, on a fleet whose
    previous config was serving perfectly well.

    That is precisely the outcome ISOLATED exists to prevent, and what
    `decide_posture`'s own docstring has always said it does — "when no
    peer managed the epoch either, the honest conclusion is that the
    revision is bad, not this node, so it keeps serving what rollback
    preserved."
    """
    view = _view(
        attempts=MAX_APPLY_ATTEMPTS,
        self_status=ApplyStatus.ERROR,
        peers_ok=0,
        peers_reported=4,
    )
    assert decide_posture(view) is Posture.ISOLATED


def test_exhaustion_alone_on_the_fleet_is_isolated_not_stuck() -> None:
    """The single-node deployment, which is the common one.

    No peer has reported, so there is nobody to shed to — but this node
    DID try and DID fail, and with no peers there will never be any
    other evidence. STUCK here fails readiness and stops admitting
    triggers, so one bad revision takes a single-node company entirely
    dark while the config it was already serving is still perfectly
    good. ISOLATED is the same conclusion the multi-node case reaches
    (the revision is the problem, not the node) by a shorter path.
    """
    view = _view(
        attempts=MAX_APPLY_ATTEMPTS,
        self_status=ApplyStatus.ERROR,
        peers_ok=0,
        peers_reported=0,
    )
    assert decide_posture(view) is Posture.ISOLATED


def test_silence_without_an_attempt_of_our_own_is_not_evidence() -> None:
    """Past grace, but this node never attempted the epoch and no peer
    reported either. Nothing has failed anywhere that we know of, so
    there is nothing to conclude — keep polling."""
    view = _view(
        ticks_behind=LAG_GRACE_TICKS + 5,
        attempts=MAX_APPLY_ATTEMPTS,
        self_status=None,
        peers_ok=0,
        peers_reported=0,
    )
    assert decide_posture(view) is Posture.WAIT


def test_converged_outranks_exhausted_attempts() -> None:
    """A node that finally succeeded is converged, whatever it cost."""
    view = _view(applied_epoch=5, attempts=MAX_APPLY_ATTEMPTS)
    assert decide_posture(view) is Posture.SERVE


# ── jitter ───────────────────────────────────────────────────────────


def test_reconcile_delay_stays_within_a_narrow_band() -> None:
    """Jitter exists only to break lock-step after a synchronized fleet
    restart — a rolling deploy boots every pod within the same second.
    It is deliberately narrow, and deliberately only on the poll."""
    delays = [reconcile_delay() for _ in range(200)]
    assert min(delays) >= RECONCILE_INTERVAL_SECONDS * 0.79
    assert max(delays) <= RECONCILE_INTERVAL_SECONDS * 1.21
    assert len(set(delays)) > 1  # actually jittered
    assert all(d >= 1.0 for d in delays)


# ── the store, on both backends ──────────────────────────────────────
#
# The Postgres store had NO test of any kind: the twin was exercised and
# the SQL half — the one every node in a real fleet actually reads to
# decide whether it serves or sheds — was never run by anything. So this
# section is parameterised, and the real backend runs whenever
# CREWLET_TEST_DSN is set (which CI now does).


class _RealPlane:
    """ConfigPlaneStore over a real database, one connection per test.

    Per-test because pytest-asyncio gives each test a fresh event loop
    and an asyncpg pool is bound to the loop that made it — the same
    shape tests/test_db/test_leases.py explains at length.
    """

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._db: Any = None
        self._store: Any = None

    async def _ready(self) -> Any:
        if self._store is None:
            from crewlet.db.client import Database
            from crewlet.db.config_plane import ConfigPlaneStore

            self._db = await Database.connect(self._dsn)
            await self._db.execute("DELETE FROM config_apply_status")
            await self._db.execute("DELETE FROM config_activations")
            self._store = ConfigPlaneStore(self._db)
        return self._store

    def __getattr__(self, name: str) -> Any:
        async def _call(*args: Any, **kwargs: Any) -> Any:
            store = await self._ready()
            return await getattr(store, name)(*args, **kwargs)

        return _call

    async def age_status(self, node_id: str, seconds: float) -> None:
        """Backdate a node's last report, so freshness can be tested."""
        await self._ready()
        await self._db.execute(
            "UPDATE config_apply_status "
            "SET updated_at = updated_at - make_interval(secs => $2) "
            "WHERE node_id = $1",
            node_id,
            float(seconds),
        )

    async def aclose(self) -> None:
        if self._db is not None:
            await self._db.close()
            self._db = None


class _MemoryPlane(MemoryConfigPlaneStore):
    """The twin, plus the same ageing hook the real backend exposes."""

    async def age_status(self, node_id: str, seconds: float) -> None:
        self._status[node_id]["reported_at"] -= seconds


@pytest.fixture(
    params=[
        pytest.param(_MemoryPlane, id="memory"),
        pytest.param(
            lambda: _RealPlane(_TEST_DSN),
            id="postgres-real",
            marks=[
                pytest.mark.integration,
                pytest.mark.skipif(
                    not _TEST_DSN,
                    reason="set CREWLET_TEST_DSN to run against real PG",
                ),
            ],
        ),
    ]
)
async def plane(request: pytest.FixtureRequest) -> Any:
    built = request.param()
    yield built
    if isinstance(built, _RealPlane):
        await built.aclose()


async def test_activation_epochs_are_monotonic(plane: Any) -> None:
    rev = uuid4()
    epochs = [await plane.record_activation(rev) for _ in range(3)]
    assert epochs == sorted(epochs)
    assert len(set(epochs)) == 3
    target = await plane.target()
    assert target is not None
    assert target.epoch == epochs[-1]


async def test_reactivating_the_same_revision_moves_the_pointer(plane: Any) -> None:
    """The gesture that picks up a rotated credential.

    Re-activating an unchanged revision is documented in
    ``docs/concepts/secret-store.md``; a pointer keyed on the revision id
    could not express it, which is why the log is append-only.
    """
    rev = uuid4()
    first = await plane.record_activation(rev)
    second = await plane.record_activation(rev)
    assert second > first


async def test_target_is_none_before_any_activation(plane: Any) -> None:
    assert await plane.target() is None


async def test_peer_health_excludes_self_and_other_epochs(plane: Any) -> None:
    rev = uuid4()
    await plane.record_apply("a", epoch=7, revision_id=rev, status=ApplyStatus.OK)
    await plane.record_apply("b", epoch=7, revision_id=rev, status=ApplyStatus.ERROR)
    await plane.record_apply("c", epoch=7, revision_id=rev, status=ApplyStatus.DEGRADED)
    await plane.record_apply("d", epoch=6, revision_id=rev, status=ApplyStatus.OK)

    ok, reported = await plane.peer_health(7, exclude_node="a")
    assert (ok, reported) == (0, 2)

    ok, reported = await plane.peer_health(7, exclude_node="b")
    assert (ok, reported) == (1, 2)


async def test_record_apply_is_last_write_wins_per_node(plane: Any) -> None:
    """One row per node: a node reports where it IS, not where it has been."""
    rev = uuid4()
    await plane.record_apply("a", epoch=1, revision_id=rev, status=ApplyStatus.ERROR)
    await plane.record_apply("a", epoch=2, revision_id=rev, status=ApplyStatus.OK)
    fleet = await plane.fleet()
    assert len(fleet) == 1
    assert fleet[0]["epoch"] == 2
    assert fleet[0]["status"] == "ok"


# ── peer health decays ───────────────────────────────────────────────


async def test_a_dead_peers_status_stops_counting_as_a_healthy_peer(
    plane: Any,
) -> None:
    """`peer_health` answers "is there a peer this work could go to?" —
    and that answer decays.

    `record_apply` upserts on `node_id`, so a node that is scaled in,
    redeployed or crashed leaves its last `ok` row behind forever.
    Counting that ghost inverts a decision: a surviving node that cannot
    apply the current epoch sees `peers_ok=1`, so `decide_posture`
    returns SHED — release every seat "so the work can go to a healthy
    peer" — when the truth is ISOLATED, where a node keeps serving the
    config it has. The company goes dark instead of degraded, which is
    exactly what ISOLATED exists to prevent.
    """
    await plane.record_apply("dead-node", epoch=12, revision_id=None, status="ok")
    assert await plane.peer_health(12, exclude_node="survivor") == (1, 1)

    # The node is gone; its row is not.
    await plane.age_status("dead-node", PEER_STATUS_FRESH_SECONDS * 2)
    assert await plane.peer_health(12, exclude_node="survivor") == (0, 0), (
        "a terminated node still counted as somewhere work could go"
    )


async def test_the_operator_view_still_shows_a_silent_node(plane: Any) -> None:
    """Freshness bounds the DECISION, not the display. A node that
    stopped reporting is precisely what the fleet view has to show."""
    await plane.record_apply("dead-node", epoch=12, revision_id=None, status="ok")
    await plane.age_status("dead-node", PEER_STATUS_FRESH_SECONDS * 2)
    rows = await plane.fleet()
    assert [r["node_id"] for r in rows] == ["dead-node"]
    assert "reported_at" not in rows[0], "internal bookkeeping leaked to the view"


# ── retention ────────────────────────────────────────────────────────


async def test_a_dead_nodes_row_is_eventually_collected(plane: Any) -> None:
    """Not expiry — the freshness bound above already stops a stale row
    affecting a decision within a minute. This is the growth: one row per
    node id that has ever run, which under pod names is one row per pod,
    on a table nothing swept because it is keyed by node rather than by
    event and so did not look short-horizon."""
    await plane.record_apply("dead-node", epoch=1, revision_id=None, status="ok")
    await plane.age_status("dead-node", APPLY_STATUS_RETENTION_SECONDS * 2)
    assert await plane.purge(APPLY_STATUS_RETENTION_SECONDS) == 1
    assert await plane.fleet() == []


async def test_a_live_nodes_row_survives_the_sweep(plane: Any) -> None:
    """A node reporting every tick must not be collected out from under
    itself — its row is what every peer's health check reads."""
    await plane.record_apply("live-node", epoch=1, revision_id=None, status="ok")
    assert await plane.purge(APPLY_STATUS_RETENTION_SECONDS) == 0
    assert len(await plane.fleet()) == 1
