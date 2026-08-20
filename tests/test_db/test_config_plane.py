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

from uuid import uuid4

from crewlet.db.config_plane import (
    LAG_GRACE_TICKS,
    MAX_APPLY_ATTEMPTS,
    RECONCILE_INTERVAL_SECONDS,
    ApplyStatus,
    FleetView,
    MemoryConfigPlaneStore,
    Posture,
    decide_posture,
    reconcile_delay,
)


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


def test_exhausted_attempts_go_stuck() -> None:
    """Retry is bounded, and exhaustion outranks every other signal.

    Without a bound, a revision that fails on one node only (a missing
    per-node env var, an MCP binary absent from that image) re-applies
    every reconcile tick forever — restarting MCP children each time.
    """
    view = _view(attempts=MAX_APPLY_ATTEMPTS, peers_ok=0, peers_reported=0)
    assert decide_posture(view) is Posture.STUCK


def test_stuck_outranks_isolated() -> None:
    view = _view(
        attempts=MAX_APPLY_ATTEMPTS,
        self_status=ApplyStatus.ERROR,
        peers_ok=0,
        peers_reported=4,
    )
    assert decide_posture(view) is Posture.STUCK


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


# ── the memory twin ──────────────────────────────────────────────────


async def test_activation_epochs_are_monotonic() -> None:
    plane = MemoryConfigPlaneStore()
    rev = uuid4()
    epochs = [await plane.record_activation(rev) for _ in range(3)]
    assert epochs == sorted(epochs)
    assert len(set(epochs)) == 3
    target = await plane.target()
    assert target is not None
    assert target.epoch == epochs[-1]


async def test_reactivating_the_same_revision_moves_the_pointer() -> None:
    """The gesture that picks up a rotated credential.

    Re-activating an unchanged revision is documented in
    ``docs/concepts/secret-store.md``; a pointer keyed on the revision id
    could not express it, which is why the log is append-only.
    """
    plane = MemoryConfigPlaneStore()
    rev = uuid4()
    first = await plane.record_activation(rev)
    second = await plane.record_activation(rev)
    assert second > first


async def test_target_is_none_before_any_activation() -> None:
    assert await MemoryConfigPlaneStore().target() is None


async def test_peer_health_excludes_self_and_other_epochs() -> None:
    plane = MemoryConfigPlaneStore()
    rev = uuid4()
    await plane.record_apply("a", epoch=7, revision_id=rev, status=ApplyStatus.OK)
    await plane.record_apply("b", epoch=7, revision_id=rev, status=ApplyStatus.ERROR)
    await plane.record_apply("c", epoch=7, revision_id=rev, status=ApplyStatus.DEGRADED)
    await plane.record_apply("d", epoch=6, revision_id=rev, status=ApplyStatus.OK)

    ok, reported = await plane.peer_health(7, exclude_node="a")
    assert (ok, reported) == (0, 2)

    ok, reported = await plane.peer_health(7, exclude_node="b")
    assert (ok, reported) == (1, 2)


async def test_record_apply_is_last_write_wins_per_node() -> None:
    """One row per node: a node reports where it IS, not where it has been."""
    plane = MemoryConfigPlaneStore()
    rev = uuid4()
    await plane.record_apply("a", epoch=1, revision_id=rev, status=ApplyStatus.ERROR)
    await plane.record_apply("a", epoch=2, revision_id=rev, status=ApplyStatus.OK)
    fleet = await plane.fleet()
    assert len(fleet) == 1
    assert fleet[0]["epoch"] == 2
    assert fleet[0]["status"] == "ok"


# ── peer health decays ───────────────────────────────────────────────


async def test_a_dead_peers_status_stops_counting_as_a_healthy_peer() -> None:
    """`peer_health` answers "is there a peer this work could go to?" —
    and that answer decays.

    `record_apply` upserts on `node_id`, so a node that is scaled in,
    redeployed or crashed leaves its last `ok` row behind forever;
    nothing sweeps `config_apply_status` because it is keyed by node,
    not by event. Counting that ghost inverts a decision: a surviving
    node that cannot apply the current epoch sees `peers_ok=1`, so
    `decide_posture` returns SHED — release every seat "so the work can
    go to a healthy peer" — when the truth is ISOLATED, where a node
    keeps serving the config it has. The company goes dark instead of
    degraded, which is exactly what ISOLATED exists to prevent.
    """
    from crewlet.db.config_plane import (
        PEER_STATUS_FRESH_SECONDS,
        MemoryConfigPlaneStore,
    )

    plane = MemoryConfigPlaneStore()
    await plane.record_apply("dead-node", epoch=12, revision_id=None, status="ok")
    assert await plane.peer_health(12, exclude_node="survivor") == (1, 1)

    # The node is gone; its row is not.
    plane._status["dead-node"]["reported_at"] -= PEER_STATUS_FRESH_SECONDS * 2
    assert await plane.peer_health(12, exclude_node="survivor") == (0, 0), (
        "a terminated node still counted as somewhere work could go"
    )


async def test_the_operator_view_still_shows_a_silent_node() -> None:
    """Freshness bounds the DECISION, not the display. A node that
    stopped reporting is precisely what the fleet view has to show."""
    from crewlet.db.config_plane import (
        PEER_STATUS_FRESH_SECONDS,
        MemoryConfigPlaneStore,
    )

    plane = MemoryConfigPlaneStore()
    await plane.record_apply("dead-node", epoch=12, revision_id=None, status="ok")
    plane._status["dead-node"]["reported_at"] -= PEER_STATUS_FRESH_SECONDS * 2
    rows = await plane.fleet()
    assert [r["node_id"] for r in rows] == ["dead-node"]
    assert "reported_at" not in rows[0], "internal bookkeeping leaked to the view"
