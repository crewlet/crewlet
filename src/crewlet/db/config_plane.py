"""The control plane: activation epochs, per-node apply status, and the
decision a lagging node makes about its own traffic.

Config used to reach processes over a competing-consumer subscription,
so exactly one applied a revision and the rest ran the old company
indefinitely. The replacement is boring on purpose: an append-only
activation log in PostgreSQL is the authoritative pointer, every node
polls it, and every node records what it managed to do.

The interesting part is not delivery, it is what a node does when it
finds itself behind — and the obvious answer is wrong.
"""

from __future__ import annotations

import random
import time
from dataclasses import dataclass
from enum import StrEnum
from typing import Any
from uuid import UUID

from crewlet._logging import get_logger

logger = get_logger("db.config_plane")

# How often a node re-reads the activation pointer.
#
# This is the authoritative delivery mechanism, so it bounds how long a
# node can serve a stale company after an activation it never saw. 15s
# against a config change an operator makes by hand is imperceptible,
# and the query is one indexed MAX() — cheaper than the event it
# replaces was to deliver.
RECONCILE_INTERVAL_SECONDS = 15.0

# Jitter applied to the interval, NOT to the apply itself.
#
# Deliberately narrow, and deliberately only here. Its whole job is to
# break lock-step after a synchronized fleet restart — a rolling deploy
# boots every pod within the same second, and without jitter they would
# poll (and then apply, and then restart their MCP children) in unison
# forever. Jittering the *apply* was considered and rejected: it delays
# convergence, which under the shed rule below means delaying the moment
# a node stops being anomalous.
RECONCILE_JITTER_FRACTION = 0.2

# How recently a peer must have reported for its status to count as
# evidence about the fleet RIGHT NOW.
#
# ``peer_health`` answers one question — "is there a healthy peer this
# work could go to?" — and the honest answer decays. ``record_apply``
# upserts on ``node_id``, so a node that is scaled in, redeployed or
# crashed leaves its last row behind forever, and nothing sweeps
# ``config_apply_status``: it is keyed by node, not by event, so it was
# never in the retention worker's list of short-horizon tables.
#
# Unbounded, that ghost row is not merely stale, it inverts a decision.
# A surviving node that cannot apply the current epoch counts the dead
# node's ``ok`` as a healthy peer, so ``decide_posture`` returns SHED —
# release every seat "so the work can go to a healthy peer" — when the
# truth is ISOLATED, where a node keeps serving the config it has. The
# company goes dark instead of degraded, which is the precise outcome
# the ISOLATED branch exists to prevent.
#
# Four reconcile intervals: a live node writes its status every tick, so
# four consecutive misses is already well past a blip and comfortably
# inside the lease TTL a dead node's presence takes to lapse.
PEER_STATUS_FRESH_SECONDS = RECONCILE_INTERVAL_SECONDS * 4

# How many consecutive reconcile ticks a node may be behind before its
# lag counts as anomalous rather than as ordinary propagation.
#
# A node that has simply not polled yet is not broken, and treating it as
# broken is what made the first draft of this design turn every
# successful rollout into a fleet-wide outage. Three ticks (~45s) is
# comfortably longer than a poll interval plus a normal apply, and short
# enough that a genuinely stuck node is out of rotation quickly.
LAG_GRACE_TICKS = 3

# How many times a node re-attempts an epoch that keeps failing.
#
# Without a bound, a revision that fails on one node only (a missing
# per-node env var, an MCP binary absent from that image) would re-apply
# every reconcile tick forever — restarting MCP children each time. After
# this many attempts the node stops trying and goes STUCK, which fails
# readiness so an operator sees it.
MAX_APPLY_ATTEMPTS = 3


class ApplyStatus(StrEnum):
    """What a node managed to do with an epoch."""

    OK = "ok"
    ERROR = "error"
    """Failed and rolled back cleanly. The node still serves the prior
    epoch — a legitimate degraded-but-correct state."""

    DEGRADED = "degraded"
    """Failed AFTER a restart-required subsystem was mutated. Rollback
    restores and restarts transports, but cannot respawn the per-role MCP
    children the failed revision already started — so this node's
    declared epoch is not the whole truth: it claims the prior config
    while its tool surface may be amputated. Never counts as converged."""


class Posture(StrEnum):
    """What a node should do about its own lag."""

    SERVE = "serve"
    """Converged, or not yet meaningfully behind. Take work."""

    WAIT = "wait"
    """Behind, but within grace and with no evidence anything is wrong.
    Keep serving — this is ordinary propagation, and shedding here is
    what turns a successful rollout into an outage."""

    SHED = "shed"
    """Confirmed anomalous: this node cannot apply an epoch its peers
    have. Stop taking new work so it routes to a healthy peer."""

    ISOLATED = "isolated"
    """Behind, past grace, and NO peer has applied this epoch either — so
    the revision is probably bad rather than this node. Keep serving the
    prior config (that is what rollback preserved) and raise divergence
    loudly. Shedding here would take the whole fleet down over one bad
    revision, which is precisely what the rollback path exists to avoid."""

    STUCK = "stuck"
    """Retries exhausted. Stop attempting, stop taking work, fail
    readiness. Needs an operator."""


@dataclass(frozen=True, slots=True)
class Activation:
    """One entry in the activation log."""

    epoch: int
    revision_id: UUID
    summary: str = ""


@dataclass(frozen=True, slots=True)
class FleetView:
    """What a node knows about the fleet when it decides its posture."""

    target_epoch: int
    applied_epoch: int
    ticks_behind: int
    attempts: int
    peers_ok: int
    peers_reported: int
    self_status: ApplyStatus | None = None


def decide_posture(view: FleetView) -> Posture:
    """Decide what a node does about its lag.

    The rule that matters, and the one the first draft got backwards:
    **the target is the store's activation pointer, but lag alone is not
    a reason to shed.** Every successful rollout produces lag — the first
    node to apply advances the pointer and every peer is behind until it
    polls. Shedding on that makes the fastest node the cause of a
    fleet-wide outage, and the faster it is, the longer the outage.

    So lag has to be *confirmed* before it means anything: either this
    node tried the epoch and failed, or it has been behind for longer
    than propagation could explain. Only then does peer health pick the
    action — and when no peer managed the epoch either, the honest
    conclusion is that the revision is bad, not this node, so it keeps
    serving what rollback preserved.
    """
    if view.applied_epoch >= view.target_epoch:
        return Posture.SERVE

    tried_and_failed = view.self_status in (ApplyStatus.ERROR, ApplyStatus.DEGRADED)
    confirmed = tried_and_failed or view.ticks_behind >= LAG_GRACE_TICKS
    if not confirmed:
        # Ordinary propagation. Never shed here.
        return Posture.WAIT

    if view.peers_ok > 0:
        # Peers have this epoch, so the work CAN go to them. Only here
        # is giving it up an improvement — and exhausted retries mean
        # this node is the anomaly rather than the revision, which is
        # exactly what STUCK reports.
        return Posture.STUCK if view.attempts >= MAX_APPLY_ATTEMPTS else Posture.SHED

    if view.peers_reported > 0 or tried_and_failed:
        # Everyone who tried, failed — and on a single-node deployment
        # "everyone" is this node. The revision is the problem, so keep
        # serving what rollback preserved.
        #
        # This outranks exhausted attempts, and used not to. Shedding
        # exists to move work to a HEALTHY PEER; with ``peers_ok == 0``
        # there is no healthy peer, so it is not shedding, it is
        # stopping. Every node in a fleet that cannot apply a revision
        # reaches this state at the same time, so the old ordering took
        # the whole company dark roughly 45 s after a bad activation —
        # the outcome this branch exists to prevent, and the one the
        # docstring above has always described. A lone node reached it
        # by a shorter path still: with no peer to report anything, its
        # own failure was the only evidence there ever would be.
        #
        # The retry bound is untouched: ``_apply_attempts`` gates
        # ``_converge_to`` directly, so a node still stops re-applying
        # (and restarting its MCP children) after MAX_APPLY_ATTEMPTS
        # whatever posture it reports.
        return Posture.ISOLATED

    # Behind for longer than propagation explains, but this node has not
    # attempted the epoch and no peer has reported one way or the other.
    # That is silence, not evidence — keep polling.
    return Posture.WAIT


def reconcile_delay(interval: float = RECONCILE_INTERVAL_SECONDS) -> float:
    """One jittered reconcile interval."""
    spread = interval * RECONCILE_JITTER_FRACTION
    return max(1.0, interval + random.uniform(-spread, spread))


# The append that moves the pointer.
#
# Exported rather than inlined because it has to run inside
# ``CompanyConfigStore``'s OWN activation transaction — the epoch and the
# ``is_active`` flip have to commit together or a crash between them
# leaves a fleet converged on a revision nobody asked for (or, worse, an
# activation no node ever notices). One statement, two callers, no way
# for them to drift.
ACTIVATION_INSERT_SQL = """
INSERT INTO config_activations (revision_id, summary)
VALUES ($1, $2)
RETURNING epoch
"""


class ConfigPlaneStore:
    """Activation log + per-node apply status."""

    def __init__(self, db: Any) -> None:
        self._db = db

    # -- activation log --------------------------------------------

    async def record_activation(
        self, revision_id: UUID | str, *, summary: str = ""
    ) -> int:
        """Append an activation and return its epoch.

        Called on EVERY activation, including re-activation of an
        unchanged revision — that gesture is how an operator asks a
        running fleet to pick up a rotated credential, and it has to move
        the pointer or nobody reconciles.

        The activation paths in :class:`~crewlet.db.company_config.CompanyConfigStore`
        run :data:`ACTIVATION_INSERT_SQL` directly on their own
        transaction rather than calling this; this entry point exists for
        callers that are not already inside one.
        """
        epoch = await self._db.fetchval(
            ACTIVATION_INSERT_SQL,
            UUID(str(revision_id)),
            summary,
        )
        logger.info(
            "config_activation_recorded", epoch=int(epoch), revision=str(revision_id)
        )
        return int(epoch)

    async def target(self) -> Activation | None:
        """The activation every node should converge on."""
        row = await self._db.fetchrow(
            """
            SELECT epoch, revision_id, summary
            FROM config_activations
            ORDER BY epoch DESC
            LIMIT 1
            """
        )
        if row is None:
            return None
        return Activation(
            epoch=int(row["epoch"]),
            revision_id=row["revision_id"],
            summary=str(row["summary"] or ""),
        )

    # -- per-node status -------------------------------------------

    async def record_apply(
        self,
        node_id: str,
        *,
        epoch: int,
        revision_id: UUID | str | None,
        status: ApplyStatus,
        error: str = "",
    ) -> None:
        await self._db.execute(
            """
            INSERT INTO config_apply_status
                (node_id, epoch, revision_id, status, error)
            VALUES ($1, $2, $3, $4, $5)
            ON CONFLICT (node_id) DO UPDATE
            SET epoch       = EXCLUDED.epoch,
                revision_id = EXCLUDED.revision_id,
                status      = EXCLUDED.status,
                error       = EXCLUDED.error,
                updated_at  = now()
            """,
            node_id,
            int(epoch),
            UUID(str(revision_id)) if revision_id else None,
            str(status),
            error[:2000],
        )

    async def peer_health(
        self, epoch: int, *, exclude_node: str = ""
    ) -> tuple[int, int]:
        """``(peers_ok, peers_reported)`` for ``epoch``, excluding this node.

        A peer that reported ``degraded`` counts as reported but NOT as
        ok: its rollback did not restore what it tore down, so it is not
        somewhere work can safely go.

        **Bounded on freshness**, because a row here is a peer's last
        word and not a liveness signal: ``record_apply`` upserts on
        ``node_id``, so a node that was scaled in or crashed leaves its
        ``ok`` behind forever. Counting it makes a diverged survivor
        shed its seats to a peer that no longer exists — see
        :data:`PEER_STATUS_FRESH_SECONDS`.
        """
        row = await self._db.fetchrow(
            """
            SELECT
                COUNT(*) FILTER (WHERE status = 'ok')  AS ok,
                COUNT(*)                               AS reported
            FROM config_apply_status
            WHERE epoch = $1
              AND node_id <> $2
              AND updated_at > now() - make_interval(secs => $3)
            """,
            int(epoch),
            exclude_node,
            float(PEER_STATUS_FRESH_SECONDS),
        )
        if row is None:
            return (0, 0)
        return (int(row["ok"] or 0), int(row["reported"] or 0))

    async def fleet(self) -> list[dict[str, Any]]:
        """Every node's last reported status — the operator view."""
        return await self._db.execute(
            """
            SELECT node_id, epoch, status, error, updated_at
            FROM config_apply_status
            ORDER BY node_id
            """
        )


class MemoryConfigPlaneStore:
    """In-memory twin. Single-process, and the default without a database."""

    def __init__(self) -> None:
        self._activations: list[Activation] = []
        self._status: dict[str, dict[str, Any]] = {}

    async def record_activation(
        self, revision_id: UUID | str, *, summary: str = ""
    ) -> int:
        epoch = len(self._activations) + 1
        self._activations.append(
            Activation(epoch=epoch, revision_id=UUID(str(revision_id)), summary=summary)
        )
        return epoch

    async def target(self) -> Activation | None:
        return self._activations[-1] if self._activations else None

    async def record_apply(
        self,
        node_id: str,
        *,
        epoch: int,
        revision_id: UUID | str | None,
        status: ApplyStatus,
        error: str = "",
    ) -> None:
        self._status[node_id] = {
            "node_id": node_id,
            "epoch": int(epoch),
            "status": str(status),
            "error": error,
            # Stamped so the twin can apply the same freshness bound the
            # SQL does. Without it the twin counts a dead node's `ok`
            # forever and certifies a SHED the real plane would call
            # ISOLATED — and the contract suite runs against both.
            "reported_at": time.monotonic(),
        }

    async def peer_health(
        self, epoch: int, *, exclude_node: str = ""
    ) -> tuple[int, int]:
        cutoff = time.monotonic() - PEER_STATUS_FRESH_SECONDS
        rows = [
            r
            for r in self._status.values()
            if r["epoch"] == int(epoch)
            and r["node_id"] != exclude_node
            and float(r.get("reported_at", 0.0)) > cutoff
        ]
        return (sum(1 for r in rows if r["status"] == "ok"), len(rows))

    async def fleet(self) -> list[dict[str, Any]]:
        # Every row, fresh or not: this is the operator view, and a node
        # that stopped reporting is exactly what it needs to show.
        return [
            {k: v for k, v in r.items() if k != "reported_at"}
            for r in sorted(self._status.values(), key=lambda r: r["node_id"])
        ]
