"""Which seats this node owns, and how it comes to own them.

A seat is an agent's chair in the org: its turns, its per-role MCP
children, and its inbox consumer all belong to whoever holds
``seat:{handle}``. This module is the placement half — claiming, holding,
and letting go. What a node *does* with an owned seat is wired in
:mod:`crewlet.engine`.

The placement policy is deliberately dumb, and the reasons are worth
stating because a cleverer one is a standing temptation:

- **Greedy claim up to a fair share.** Capacity is ``ceil(seats / live
  nodes)``, live nodes being the ``node:*`` presence leases of nodes that
  actually run seats. No membership service, no gossip, no coordinator —
  every node computes the same number from the same table and stops
  there. Two nodes racing for the last seat is resolved by the lease, not
  by the arithmetic. With ``role.placement`` in play the share is
  computed per placement group and summed, because a single fleet-wide
  ratio strands pinned seats — see :mod:`crewlet.seat.placement`.
- **``preferred`` orders the attempt; it never gates it.** A seat whose
  hint names this node is tried first, so a rolling deploy tends to land
  seats back where their MCP children and caches are already warm. It is
  a *hint*: treating a matching hint as a reason to wait would strand
  every seat a dead node used to hold, since the hint outlives the node.
- **Claims are rate-limited per sweep.** The cost of a takeover is not
  the lease — measured at 4.9 ms to attach a consumer — it is spawning
  that seat's stdio MCP children. A node absorbing a dead peer's twenty
  seats at once would fork twenty subprocess trees in one tick.

Losing a seat is as important as gaining one, and the two failure modes
are not the same. ``renew`` returning ``False`` means the lease is
definitively gone (lapsed, moved, or superseded) and the node must drop
the seat immediately. A :class:`~crewlet.db.leases.LeaseError` means the
*store* could not be reached, which says nothing about ownership — the
row is untouched and still held — so the node keeps its seats and retries
until the TTL genuinely runs out. Conflating them would tear a healthy
node's whole company down over a two-second database blip, and no peer
could pick the seats up during it anyway.

Three properties are worth stating outright, because each replaced
something that looked reasonable and was not:

- **Admission is freshness, not membership.** :meth:`SeatHost.may_start`
  answers "may a new turn start on this seat?", and it answers it from
  how long ago the last successful renew was — not from whether the
  handle is in a local dict. That dict is refreshed on a heartbeat
  against a TTL three times longer, so a membership check can be a full
  TTL stale: exactly the window an ownership check exists to close. It is
  an optimization either way. Correctness comes from epoch-fenced writes,
  and the epoch ``may_start`` returns is that fence.
- **Release has two modes, because loss and drain are opposites.**
  Voluntary (:class:`ReleaseReason` ``DRAIN`` / ``ROLE_GONE``) still owns
  the seat and can take its time: quiesce, let the in-flight handler
  finish, detach, release. Fenced (``LEASE_LOST`` / ``ACQUIRE_FAILED`` /
  ``POSTURE``) has neither time nor exclusivity — a peer may already be
  running the seat — so in-flight work is abandoned and nothing is
  republished, because republishing hands the peer a second copy of work
  it is already doing.
- **Release fails closed.** A hook that cannot *prove* teardown finished
  raises :class:`SeatReleaseError`, and the lease is then kept and kept
  renewed rather than handed on. Releasing a lease while still consuming
  the seat is the one thing ownership exists to prevent; a lease held a
  little too long costs latency, a lease released too early costs
  correctness.
"""

from __future__ import annotations

import asyncio
import contextlib
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass, field, replace
from enum import StrEnum
from typing import Any

from crewlet._logging import get_logger
from crewlet._tasks import cancel_and_wait
from crewlet.db.leases import (
    PROTOCOL_VERSION,
    Lease,
    LeaseError,
    node_resource,
    seat_resource,
)
from crewlet.seat.placement import (
    ANYWHERE,
    NodeProfile,
    NodeRole,
    PlacementPlan,
    SeatPlacement,
    plan_placement,
)

#: What an operator loses when no live node performs a role. Absences
#: every one of them, which is why they need saying out loud.
_UNMANNED_HINTS: dict[NodeRole, str] = {
    NodeRole.INGRESS: (
        "no live node serves the HTTP API, so no webhook from any "
        "integration reaches this company and the dashboard is down. "
        "Give a node the 'ingress' role."
    ),
    NodeRole.SEATS: (
        "no live node runs agents, so every trigger queues up unread. "
        "Give a node the 'seats' role."
    ),
    NodeRole.WORKERS: (
        "no live node runs the company-wide duties, so nothing fires on "
        "a schedule, no sandbox run is ever collected, and the retention "
        "sweeps do not run. Give a node the 'workers' role."
    ),
}

#: ``on_release(handle, lease, reason)`` — positional, all three.
type ReleaseHook = Callable[[str, Lease, ReleaseReason], Awaitable[None]]

#: ``on_admission(handle, admitted)`` — see ``SeatHost.on_admission``.
type AdmissionHook = Callable[[str, bool], Awaitable[None]]

logger = get_logger("seat.host")

# How long a seat stays claimed without a heartbeat.
#
# Measured, and the measurement moved where the constraint lives. The
# broker imposes no floor: a cleanly-closed consumer releases its
# messages in ~9 ms and a successor attaches in ~5 ms, so a peer is
# productive the instant it claims. What bounds the TTL is heartbeat
# reliability — 45 s is three heartbeat intervals, which tolerates two
# consecutive missed renewals (a GC pause, a database blip, a scheduling
# hiccup) with a full interval left to recover in.
#
# Shorter would drop healthy nodes' seats on ordinary jitter, and each
# spurious handoff costs a real MCP respawn. Longer is time a dead node's
# seats sit dark, since nothing can claim them until the TTL runs out.
SEAT_LEASE_TTL_SECONDS = 45.0

# One third of the TTL — the standard lease ratio, for the standard
# reason: it is the largest interval that still leaves room for two
# failures.
SEAT_HEARTBEAT_INTERVAL_SECONDS = 15.0

# How often placement is re-evaluated.
#
# Distinct from the heartbeat because they answer different questions:
# the heartbeat keeps what this node has, the sweep looks for what it
# should take. Five seconds means a dead peer's seats are fully absorbed
# within ~TTL + a few sweeps, without polling the table hard enough to
# matter.
SEAT_SWEEP_INTERVAL_SECONDS = 5.0

# How many seats one sweep may newly claim.
#
# The limiter is MCP spawn, not the lease — see the module docstring.
# Four per five-second sweep absorbs twenty seats in ~25 s, which is
# comfortably inside the window where a dead peer's seats were going to
# be dark anyway, while never forking more than four subprocess trees in
# one tick.
SEAT_CLAIM_LIMIT_PER_SWEEP = 4

# How long a seat whose acquire pipeline failed is not re-attempted here.
#
# A failed acquire is almost never transient at the seat level — a bad
# MCP command, a credential the role's config resolves to nothing, a
# sandbox template that no longer exists. Retrying it on the next 5 s
# sweep spins: claim, spawn, fail, release, repeat, forever, at the cost
# of an MCP fork each time, and the seat is dark throughout either way.
#
# Backing off for a TTL gives a peer a clear run at it (its own claim
# order is unaffected) and, if every node fails the same way, reduces the
# noise from twelve attempts a minute to one. It is deliberately not
# permanent: the cause is often config, and config changes.
SEAT_ACQUIRE_BACKOFF_SECONDS = SEAT_LEASE_TTL_SECONDS

# How many seats one sweep may hand back to rebalance.
#
# The mirror of the claim limit, sized the same way and for the same
# reason: giving a seat up is an MCP teardown and, on the node that takes
# it, an MCP spawn. Two per sweep rather than four because a release is
# the more disruptive half — it interrupts a live agent at a turn
# boundary, while a claim only starts one — and because the fleet has no
# deadline to meet here. Nothing is dark during a rebalance; the seats
# are being served the whole time, just by the wrong node.
SEAT_RELEASE_LIMIT_PER_SWEEP = 2


class ReleaseReason(StrEnum):
    """Why a seat is being given up. The two families are opposites.

    **Voluntary** — this node still holds the lease and is choosing to
    let go. There is time: quiesce, let the in-flight handler finish
    under a bounded wait, then detach and release.

    **Fenced** — the lease is gone or must be treated as gone. There is
    NO time and no exclusivity: a peer may already be running this seat,
    so in-flight work is abandoned rather than finished, and nothing is
    republished — republishing hands the peer a second copy of work it is
    already doing.
    """

    #: Graceful shutdown / capacity rebalance. Voluntary.
    DRAIN = "drain"
    #: The role was decommissioned. Voluntary, but must not defer at all:
    #: the events are for a role that no longer exists.
    ROLE_GONE = "role_gone"
    #: This node stopped matching the seat's ``role.placement`` — the
    #: selector changed under a live apply, or this node's labels did.
    #: Voluntary: the lease is still held, so the in-flight turn finishes
    #: and an eligible peer picks the seat up. Distinct from ``DRAIN``
    #: only so the log says which of the two happened; a rebalance and a
    #: pin change look identical otherwise, and one of them is an
    #: operator action they will want to see land.
    PLACEMENT = "placement"
    #: ``renew`` returned False, or the TTL grace expired. Fenced.
    LEASE_LOST = "lease_lost"
    #: The acquire pipeline failed partway. Fenced — the seat was never
    #: fully established, so the hook must tolerate partial state.
    ACQUIRE_FAILED = "acquire_failed"
    #: Config posture went SHED or STUCK. Fenced: this node must stop
    #: serving the seat now, not when its turn happens to end.
    POSTURE = "posture"

    @property
    def fenced(self) -> bool:
        return self in (
            ReleaseReason.LEASE_LOST,
            ReleaseReason.ACQUIRE_FAILED,
            ReleaseReason.POSTURE,
        )


class SeatReleaseError(RuntimeError):
    """A release hook could not prove the seat was torn down.

    Raised by ``on_release`` when it cannot be sure this node has stopped
    consuming — a consumer that will not close, an MCP child that will
    not die. The host then **keeps the lease and keeps renewing it**
    rather than handing the seat to a peer while still serving it. See
    :meth:`SeatHost._release_locked`.
    """


@dataclass(frozen=True, slots=True)
class SweepResult:
    """What one placement pass did, for logs, tests, and /health."""

    held: int
    capacity: int
    live_nodes: int
    claimed: tuple[str, ...] = ()
    lost: tuple[str, ...] = ()
    unplaceable: tuple[str, ...] = ()
    """Seats whose ``role.placement`` matches no live seat-running node.

    Nothing this node can act on — a pin to a node that is down, a label
    nobody carries — but it is the one placement failure that is
    otherwise invisible: the seat is simply not served, and every node in
    the fleet reports a perfectly healthy sweep."""
    blocked_by_protocol: int | None = None
    """The fleet's protocol floor when an older-protocol peer is holding
    leases and this node is therefore refusing to claim. ``None`` when
    nothing is blocking. Without this a node stalled by the mixed-version
    gate is indistinguishable from one whose peers simply hold every
    seat — see :data:`~crewlet.db.leases.PROTOCOL_VERSION`."""

    @property
    def blocked(self) -> bool:
        return self.blocked_by_protocol is not None


@dataclass
class _Held:
    lease: Lease
    handle: str
    renewed_at: float
    """Monotonic time of the last SUCCESSFUL renew.

    The deadline that makes "keep the seat through a database blip"
    bounded rather than forever. Without it, a store that stays
    unreachable leaves this node holding a seat whose row lapsed long
    ago — and a peer that took it over is then running the same agent
    concurrently."""


@dataclass
class SeatHost:
    """Claims, holds and releases the seats this node runs.

    ``owner`` is a process *incarnation* (``{node_id}:{random}``); the
    stable ``node_id`` goes into the lease's ``preferred`` hint, where
    restart-stability is what you actually want. Passing the node id as
    the owner would let two processes sharing one id both believe they
    hold the same seat at the same epoch.
    """

    leases: Any
    owner: str
    node_id: str
    seats: Callable[[], Sequence[str]]
    """Current seat handles, read fresh each sweep — the org changes
    under a live config apply, and a snapshot taken at construction would
    keep claiming seats that no longer exist."""
    placement_of: Callable[[str], SeatPlacement] | None = None
    """Where a seat may run, read fresh alongside ``seats``. ``None``
    (and any handle it does not know) means anywhere, which is the whole
    company before anyone writes a ``role.placement``.

    Every seat's placement is needed, not just this node's: the fair
    share is computed per placement GROUP, so a node has to know how the
    seats it cannot run are distributed to know how many of the ones it
    can run are its own."""
    profile: NodeProfile | None = None
    """What this node is — its roles and labels. ``None`` means the
    all-roles, no-labels default, which is the single-process
    deployment.

    Advertised to peers on the presence lease every heartbeat rather
    than at config-activation time, so a label or role change lands
    within one TTL of the restart that made it."""
    on_acquire: Callable[[str, Lease], Awaitable[None]] | None = None
    on_release: ReleaseHook | None = None
    """Tear a seat down. Called as ``on_release(handle, lease, reason)``
    — three POSITIONAL arguments; a keyword-only ``reason`` raises
    ``TypeError`` inside the release, which is swallowed as "teardown
    could not be proven" and strands the seat. The reason decides whether
    in-flight work is finished or abandoned (see :class:`ReleaseReason`).

    Must be **idempotent and tolerant of partial state**: a failed
    acquire releases the same seat, so the hook can be handed a seat
    whose MCP children are half-spawned and whose consumer was never
    attached.

    Raise :class:`SeatReleaseError` when teardown cannot be *proven* —
    the host then keeps the lease rather than handing a seat to a peer
    while still serving it."""
    on_admission: AdmissionHook | None = None
    """Ownership of a HELD seat became unprovable, or provable again.

    Called ``on_admission(handle, admitted)`` when a renew starts or
    stops failing, and only for seats this node keeps — a seat it loses
    goes through ``on_release`` instead.

    It exists because the lease grace and the consumer are two different
    clocks. A store blip freezes ``renewed_at``, so :meth:`may_start`
    stops admitting turns within one heartbeat while the seat itself is
    correctly kept (the row is untouched; shedding on a blip would tear
    a healthy company down). The consumer, meanwhile, is still attached
    and still being handed deliveries, each of which is refused. Without
    a signal on the way back up, a delivery that arrived during the blip
    would leave the attachment stopped for good: the node owns the seat,
    is attached to it, and reads nothing from it ever again.

    Fires only on the EDGE, so a store that is down for an hour produces
    one call, not one per heartbeat."""
    ttl_seconds: float = SEAT_LEASE_TTL_SECONDS
    heartbeat_seconds: float = SEAT_HEARTBEAT_INTERVAL_SECONDS
    sweep_seconds: float = SEAT_SWEEP_INTERVAL_SECONDS
    claim_limit: int = SEAT_CLAIM_LIMIT_PER_SWEEP
    acquire_backoff_seconds: float = SEAT_ACQUIRE_BACKOFF_SECONDS
    release_limit: int = SEAT_RELEASE_LIMIT_PER_SWEEP
    protocol: int = PROTOCOL_VERSION

    _held: dict[str, _Held] = field(default_factory=dict, init=False)
    _seat_locks: dict[str, asyncio.Lock] = field(default_factory=dict, init=False)
    _acquire_backoff: dict[str, float] = field(default_factory=dict, init=False)
    """Monotonic deadline before which a seat is not re-claimed here.

    Negative stickiness: a seat whose acquire pipeline failed is skipped
    by this node's claim order until the deadline. Peers are unaffected —
    they may well succeed where this node did not."""
    _unproven_admission: set[str] = field(default_factory=set, init=False)
    """Held seats whose last renew FAILED, so admission is not provable.

    The edge-detector behind :attr:`on_admission` — membership changes
    at most once per outage, not once per heartbeat."""
    _undead: dict[str, _Held] = field(default_factory=dict, init=False)
    """Seats whose teardown could not be proven, kept OUT of ``_held``
    (so nothing new starts on them) but still renewed (so no peer takes a
    seat this process may still be serving). See
    :meth:`_release_locked`."""
    _node_lease: Lease | None = field(default=None, init=False)
    _tasks: list[asyncio.Task[Any]] = field(default_factory=list, init=False)
    _running: bool = field(default=False, init=False)
    _draining: bool = field(default=False, init=False)
    _last: SweepResult | None = field(default=None, init=False)
    _live_nodes: int = field(default=1, init=False)
    """Last successfully-read seat-running node count, for logs."""
    _unmanned_roles: set[NodeRole] = field(default_factory=set, init=False)
    """Roles no live node performs, as of the last read. The edge
    detector behind ``fleet_role_unmanned`` — see
    :meth:`_check_fleet_roles`."""
    _live_profiles: list[NodeProfile] = field(default_factory=list, init=False)
    """Last successfully-read fleet membership, reused when the store
    blips. Profiles rather than a count, because placement needs to know
    which peers are eligible for which seats, not merely how many there
    are."""

    def _lock_for(self, handle: str) -> asyncio.Lock:
        """One lock per seat, held across a WHOLE acquire or release.

        The heartbeat and the sweep are independent tasks with no
        ordering between them, and both hooks are long: an acquire
        attaches a consumer and spawns MCP children, a release tears them
        down. Without this, a heartbeat that detects a lost lease can
        interleave with a sweep that just re-claimed the same seat — and
        the release then tears down the consumer the claim just created,
        leaving a seat that is owned in the lease table and dead in this
        process, with nothing to notice.
        """
        lock = self._seat_locks.get(handle)
        if lock is None:
            lock = asyncio.Lock()
            self._seat_locks[handle] = lock
        return lock

    # ── introspection ────────────────────────────────────────────────

    def may_start(self, handle: str) -> int | None:
        """The epoch a new turn may start under, or ``None``.

        **Freshness, not membership.** "Do I hold this seat?" is a
        question about a local snapshot refreshed on a 15 s heartbeat
        against a 45 s TTL, so the honest answer can be a full TTL stale
        — precisely the window an ownership check exists to close, which
        means a membership check cannot meet its own exit criterion.

        What *is* provable: a successful renew at time ``t`` bought
        exclusivity through ``t + ttl``. So admission asks how long ago
        that renew was, and admits only inside one heartbeat interval.
        Every turn that starts is then certified owned for at least
        ``ttl - heartbeat`` (30 s at the shipped constants), and the
        first failed renew stops NEW turns immediately while the
        ``LeaseError`` grace still keeps the seat so in-flight work can
        finish.

        This is an optimization, not the safety property. Correctness
        comes from epoch-fenced writes: the returned epoch is the fencing
        token, and a write without it is a write a zombie can also make.
        """
        held = self._held.get(handle)
        if held is None:
            return None
        elapsed = asyncio.get_running_loop().time() - held.renewed_at
        if elapsed > self.heartbeat_seconds:
            logger.info(
                "seat_admission_stale",
                seat=handle,
                seconds_since_renew=round(elapsed, 1),
                heartbeat_seconds=self.heartbeat_seconds,
            )
            return None
        return held.lease.epoch

    def note_delivery_deferred(self, handle: str) -> None:
        """Record that this seat's consumer stopped for want of proof.

        Called by the caller that acted on a ``may_start`` refusal by
        deferring a delivery — which quiesces the attachment. Without
        this the seat is **permanently deaf on a perfectly healthy
        node**, and it takes no failure at all to get there:
        ``may_start`` refuses once the last renew is older than one
        heartbeat interval, and consecutive renews are exactly one
        heartbeat apart plus the duration of the pass, so every cycle
        has a real window where a healthy, renewing seat answers
        ``None``. A batch landing in that window defers, the queue
        quiesces, and nothing ever resumes it: ``_note_admission`` is
        edge-triggered on the set below, the deferral never entered it,
        so the next successful renew reports "still admitted", short-
        circuits, and never calls the resume hook.

        Deliberately NOT inside :meth:`may_start`. The in-turn fence
        calls that too, where a refusal abandons a turn and stops no
        consumer — marking there would leave the set claiming a seat is
        quiesced when it is still happily consuming, and the next real
        store outage would then short-circuit the call that stops it.
        The state means "the consumer is stopped pending proof", so only
        the path that stops one may set it.

        No hook fires here: the consumer is already stopping, which is
        what got us called. This records the edge so the *resume* can
        fire on the next successful renew.
        """
        if handle in self._held or handle in self._undead:
            self._unproven_admission.add(handle)

    def epoch_for(self, handle: str) -> int | None:
        """The fencing token for a seat, or ``None`` if not held here.

        Unlike :meth:`may_start` this does NOT check freshness: it is for
        fencing a write that belongs to work already under way, where
        refusing would abandon a turn mid-flight for no gain — the fence
        in the write itself is what makes the write safe.
        """
        held = self._held.get(handle)
        return held.lease.epoch if held is not None else None

    @property
    def held_handles(self) -> tuple[str, ...]:
        """Seats this node is serving. Excludes the undead by design —
        nothing new starts on a seat whose teardown was never proven."""
        return tuple(sorted(self._held))

    @property
    def unproven_handles(self) -> tuple[str, ...]:
        """Seats still leased because their teardown could not be proven.

        Operationally the most important number on this object: each one
        is a seat this process may still be consuming, holding a lease no
        peer can take. See :meth:`_release_locked`.
        """
        return tuple(sorted(self._undead))

    @property
    def last_sweep(self) -> SweepResult | None:
        return self._last

    # ── lifecycle ────────────────────────────────────────────────────

    async def start(self) -> None:
        if self._running:
            return
        self._running = True
        self._draining = False
        # Announce presence before the first sweep: capacity divides by
        # the live node count, and a node that has not registered itself
        # would compute a share that excludes itself.
        await self._renew_node_presence()
        await self.sweep()
        self._tasks = [
            asyncio.create_task(self._heartbeat_loop()),
            asyncio.create_task(self._sweep_loop()),
        ]
        logger.info(
            "seat_host_started",
            node=self.node_id,
            owner=self.owner,
            held=len(self._held),
        )

    async def begin_drain(self) -> None:
        """Stop claiming, but keep renewing what is already held.

        The first half of a graceful shutdown: readiness flips off, no
        new seats are taken, and the seats in hand keep their leases
        alive so their turns can finish. :meth:`release_all` is the
        second half.
        """
        self._draining = True
        # Stop counting toward the fleet size immediately. A draining node
        # that keeps its presence lease stays in every peer's divisor, so
        # they compute a share that reserves capacity for a node which
        # will never claim again — and this node's seats stay dark for
        # the whole drain plus the takeover ramp.
        await self._release_node_presence()
        logger.info("seat_host_draining", node=self.node_id, held=len(self._held))

    async def resume_claiming(self) -> None:
        """Undo :meth:`begin_drain`: claim again, and count again.

        The posture path needs this — a node that shed its seats on
        divergence and then converged must rejoin the fleet rather than
        sit out until it restarts.
        """
        if not self._draining:
            return
        self._draining = False
        await self._renew_node_presence()
        logger.info("seat_host_resumed", node=self.node_id)

    async def stop(self) -> None:
        if not self._running:
            return
        self._running = False
        for task in self._tasks:
            await cancel_and_wait(task)
        self._tasks.clear()
        await self.release_all()
        await self._release_node_presence()
        stranded = tuple(sorted(self._undead))
        if stranded:
            logger.error(
                "seat_host_stopped_with_unproven_seats",
                node=self.node_id,
                seats=list(stranded),
                hint=(
                    "these seats' teardown was never proven, so their "
                    "leases were held rather than released; they lapse at "
                    "the TTL and a peer picks them up then"
                ),
            )
        self._undead.clear()
        self._unproven_admission.clear()
        self._acquire_backoff.clear()
        self._seat_locks.clear()
        logger.info("seat_host_stopped", node=self.node_id)

    async def release_all(self, reason: ReleaseReason = ReleaseReason.DRAIN) -> None:
        """Hand every seat back — each one the moment IT goes idle.

        Concurrently, not one after another, and the difference is the
        whole point of a graceful drain. A voluntary release waits for
        that seat's in-flight turn under a bounded timeout; run in
        sequence, a node holding a dozen seats pays that timeout a dozen
        times over, and the eleventh seat stays dark for the whole
        procession even though it went idle first. Released together,
        each seat leaves as soon as its own turn finishes and the drain
        costs one timeout rather than N.

        Deliberately uncapped, unlike claiming. The claim-rate limit is
        sized by the cost of an MCP *spawn* on the node taking a seat on;
        letting go costs a teardown, and the peers picking these up are
        throttled by their own claim limits anyway — so the only thing a
        release cap would add is a longer drain.

        Failures are per seat: a teardown that cannot be proven keeps
        THAT seat's lease (see :meth:`_release_locked`) and says nothing
        about the others, so one stuck consumer must not strand eleven
        healthy seats.

        Releasing expires the row in place rather than deleting it, so
        the epoch stays monotonic and ``preferred`` survives — a peer can
        claim immediately, and a rolling deploy tends to bring the seat
        home afterwards.
        """
        handles = list(self._held)
        if not handles:
            return
        results = await asyncio.gather(
            *(self.release(handle, reason) for handle in handles),
            return_exceptions=True,
        )
        for handle, result in zip(handles, results, strict=True):
            if isinstance(result, BaseException):
                logger.error(
                    "seat_release_raised",
                    seat=handle,
                    reason=str(reason),
                    error=str(result),
                )

    async def release(
        self, handle: str, reason: ReleaseReason = ReleaseReason.DRAIN
    ) -> bool:
        async with self._lock_for(handle):
            return await self._release_locked(handle, reason)

    async def _release_locked(self, handle: str, reason: ReleaseReason) -> bool:
        """Tear the seat down, then give up the lease — in that order.

        **Fail-closed.** If the hook cannot prove teardown finished
        (:class:`SeatReleaseError`) the lease is NOT released: the seat
        moves to ``_undead``, where nothing new starts on it but the
        heartbeat keeps renewing it, so no peer can take a seat this
        process may still be consuming. That is the whole point of a
        lease — releasing one while still serving the seat hands a peer
        permission to run the agent concurrently, which is the single
        failure ownership exists to prevent.

        Undead seats are not held forever: the watchdog's remedy applies
        if they cannot be closed within the TTL (see :meth:`heartbeat`).
        """
        held = self._held.pop(handle, None)
        # Whatever happens next, this seat's admission is no longer a
        # live question here: the attachment goes with it, so a stale
        # flag would suppress the resume after a future re-claim.
        self._unproven_admission.discard(handle)
        if held is None:
            return False
        try:
            await self._notify_release(handle, held.lease, reason)
        except SeatReleaseError as exc:
            self._undead[handle] = held
            logger.error(
                "seat_release_unproven",
                seat=handle,
                epoch=held.lease.epoch,
                reason=str(reason),
                error=str(exc),
                hint=(
                    "keeping the lease and renewing it: this node may still "
                    "be consuming the seat, and releasing now would let a "
                    "peer run the same agent concurrently"
                ),
            )
            return False
        try:
            return await self.leases.release(
                held.lease.resource, owner=self.owner, epoch=held.lease.epoch
            )
        except LeaseError:
            # The seat IS torn down locally; the row simply lapses on its
            # own. Nothing here is worth failing a drain.
            logger.warning("seat_release_unavailable", seat=handle)
            return False

    # ── the loops ────────────────────────────────────────────────────

    async def _heartbeat_loop(self) -> None:
        while self._running:
            try:
                await asyncio.sleep(self.heartbeat_seconds)
                await self.heartbeat()
            except asyncio.CancelledError:
                raise
            except Exception:
                logger.exception("seat_heartbeat_tick_failed")

    async def _sweep_loop(self) -> None:
        while self._running:
            try:
                await asyncio.sleep(self.sweep_seconds)
                await self.sweep()
            except asyncio.CancelledError:
                raise
            except Exception:
                logger.exception("seat_sweep_tick_failed")

    async def heartbeat(self) -> tuple[str, ...]:
        """Renew every held lease. Returns the seats that were LOST.

        A lost seat is dropped immediately and locally: its lease is gone
        and a peer may already be running it, so anything this node does
        for it from here is a zombie's work. Fencing catches the writes;
        this is what stops the rest.
        """
        lost: list[str] = []
        loop = asyncio.get_running_loop()
        # The undead are renewed alongside the living: their teardown was
        # never proven, so a peer must not be able to claim them.
        for handle, held in [*self._held.items(), *self._undead.items()]:
            # Read the clock PER SEAT, not once before the loop. A single
            # pre-loop timestamp is assigned to every seat's renewed_at,
            # so with many seats and a slow store the later ones record a
            # renew earlier than it happened — narrowing their grace, and
            # their admission window, as the seat count grows.
            now = loop.time()
            try:
                alive = await self.leases.renew(
                    held.lease.resource,
                    owner=self.owner,
                    epoch=held.lease.epoch,
                    ttl_seconds=self.ttl_seconds,
                )
            except LeaseError:
                # Unknown is not lost — the row is untouched and still
                # held, so shedding on a blip would tear down a healthy
                # node while no peer could claim the seats anyway.
                #
                # But it is only "not lost" until the row's TTL runs out.
                # Past that the lease HAS lapsed, whatever this node can
                # or cannot see, and a peer may already be running the
                # agent. Keeping the seat on faith from there is how one
                # unreachable database turns into two nodes serving one
                # seat, so the grace is bounded by the same TTL the lease
                # was granted with.
                elapsed = now - held.renewed_at
                if elapsed < self.ttl_seconds:
                    logger.warning(
                        "seat_heartbeat_unavailable",
                        seat=handle,
                        seconds_since_renew=round(elapsed, 1),
                        ttl_seconds=self.ttl_seconds,
                    )
                    await self._note_admission(handle, admitted=False)
                    continue
                logger.error(
                    "seat_dropped_unrenewable",
                    seat=handle,
                    seconds_since_renew=round(elapsed, 1),
                    ttl_seconds=self.ttl_seconds,
                    hint=(
                        "the lease store has been unreachable for longer "
                        "than the TTL, so this lease has lapsed whether or "
                        "not we can see it; dropping the seat rather than "
                        "risk running an agent a peer now owns"
                    ),
                )
                alive = False
            if alive:
                async with self._lock_for(handle):
                    # Under the lock, and only if this is still the live
                    # record: a sweep may have re-claimed the seat at a
                    # new epoch while we awaited, and writing here would
                    # stamp an orphaned object while the live one keeps
                    # the older timestamp.
                    current = self._held.get(handle) or self._undead.get(handle)
                    if current is held:
                        held.renewed_at = now
                await self._note_admission(handle, admitted=True)
                continue
            if handle in self._undead:
                # An undead seat whose lease is genuinely gone: the row
                # has moved on and a peer owns it now. Stop renewing.
                self._undead.pop(handle, None)
                logger.error(
                    "seat_undead_lease_lost",
                    seat=handle,
                    epoch=held.lease.epoch,
                    hint=(
                        "teardown was never proven and the lease has now "
                        "moved to a peer; this process may still be "
                        "consuming the seat"
                    ),
                )
                continue
            async with self._lock_for(handle):
                # Re-check under the lock: a sweep may have re-claimed
                # this seat at a NEW epoch between the failed renew and
                # here, and tearing that down would kill a claim this
                # node legitimately holds.
                current = self._held.get(handle)
                if current is None or current.lease.epoch != held.lease.epoch:
                    continue
                lost.append(handle)
                self._held.pop(handle, None)
                self._unproven_admission.discard(handle)
                logger.warning(
                    "seat_lease_lost",
                    seat=handle,
                    epoch=held.lease.epoch,
                    hint="a peer may already own this seat; dropping it locally",
                )
                # Fenced: a peer may already be running this seat, so
                # in-flight work is abandoned rather than finished. There
                # is nothing to fail closed ON — the lease is already
                # gone — so an unproven teardown is logged, not retained.
                with contextlib.suppress(SeatReleaseError):
                    await self._notify_release(
                        handle, held.lease, ReleaseReason.LEASE_LOST
                    )
        if lost and self._last is not None:
            self._last = replace(self._last, lost=tuple(lost), held=len(self._held))
        await self._renew_node_presence()
        return tuple(lost)

    async def sweep(self) -> SweepResult:
        """One placement pass: converge on a fair share, rate-limited.

        Both directions. Claiming alone is only half of placement, and
        the half that does nothing for a fleet that grows: a node that
        booted alone holds every seat, and every peer that joins later
        computes a share it can never reach because the seats it should
        take are already held by a node with no reason to let go. Scaling
        out would then do nothing at all until something died.
        """
        seats = list(self.seats())
        plan, live_nodes = await self._plan(seats)
        capacity = plan.capacity
        eligible = set(plan.eligible)

        # Seats this node holds that the org no longer has (a role was
        # decommissioned under a live apply) are released, not kept.
        released: list[str] = []
        for handle in list(self._held):
            if handle not in seats:
                logger.info("seat_released_role_gone", seat=handle)
                # Only if the release was PROVEN. ``release`` returns
                # False when teardown could not be confirmed and the seat
                # went undead — still leased, still renewed, and reported
                # as released is exactly backwards. ``_shed_to_capacity``
                # has always checked; these two did not.
                if await self.release(handle, ReleaseReason.ROLE_GONE):
                    released.append(handle)
            elif handle not in eligible:
                # Still a seat, no longer OURS to run: the placement
                # selector changed under a live apply, or this node's
                # labels or roles did. Released before the capacity shed,
                # because an ineligible seat is not a question of how
                # many we hold — no amount of spare capacity makes it
                # ours again, and holding it means an eligible peer
                # cannot take it.
                logger.info(
                    "seat_released_not_placeable_here",
                    seat=handle,
                    node=self.node_id,
                    placement=self._placement(handle).describe(),
                )
                if await self.release(handle, ReleaseReason.PLACEMENT):
                    released.append(handle)

        if not self._draining:
            released.extend(await self._shed_to_capacity(capacity))

        claimed: list[str] = []
        blocked: int | None = None
        if not self._draining:
            # Undead seats count against capacity: this process may still
            # be serving them, so taking on more work would over-subscribe
            # a node that is already in trouble.
            room = min(capacity - len(self._held) - len(self._undead), self.claim_limit)
            if room > 0:
                claimed, blocked = await self._claim_up_to(plan.eligible, room)

        if plan.unplaceable:
            logger.warning(
                "seats_unplaceable",
                node=self.node_id,
                seats=list(plan.unplaceable),
                hint=(
                    "no live node that runs seats matches these seats' "
                    "role.placement, so nothing is serving them. Start a "
                    "node that matches, or widen the selector."
                ),
            )

        self._prune_seat_locks(seats)
        result = SweepResult(
            held=len(self._held),
            capacity=capacity,
            live_nodes=live_nodes,
            claimed=tuple(claimed),
            lost=tuple(released),
            unplaceable=plan.unplaceable,
            blocked_by_protocol=blocked,
        )
        self._last = result
        if claimed or released or blocked is not None:
            logger.info(
                "seat_sweep",
                node=self.node_id,
                held=result.held,
                capacity=capacity,
                live_nodes=live_nodes,
                claimed=list(claimed),
                released=list(released),
                blocked_by_protocol=blocked,
            )
        return result

    def _prune_seat_locks(self, seats: Sequence[str]) -> None:
        """Drop locks for seats this node neither holds nor could claim.

        One ``asyncio.Lock`` accumulated per handle ever seen, including
        every seat removed by a live config apply — unbounded growth in a
        long-lived process that reconfigures often.
        """
        keep = set(seats) | set(self._held) | set(self._undead)
        for handle in [h for h in self._seat_locks if h not in keep]:
            lock = self._seat_locks[handle]
            if not lock.locked():
                del self._seat_locks[handle]

    # ── internals ────────────────────────────────────────────────────

    async def _shed_to_capacity(self, capacity: int) -> list[str]:
        """Hand back seats held above this node's fair share.

        The give-back half of placement. It exists because the fair share
        is recomputed from a live node count: the moment a peer joins,
        every incumbent's share drops, and without this nothing ever acts
        on that — the new node computes a share it cannot reach, and the
        incumbent keeps serving seats it has already been told are not
        its own.

        **Voluntary**, so the seat is quiesced and its in-flight turn
        finishes before the consumer detaches. A rebalance costs at most
        one turn boundary of latency on each seat that moves; nothing is
        abandoned and nothing goes dark, because the seat is served
        continuously — first here, then there.

        **Convergent, not oscillating**, and the ceiling is what
        guarantees it: shares are ``ceil(seats / nodes)``, so they sum to
        at least the seat count, and a node that has shed down to its
        share has ``room == 0`` and cannot immediately re-claim what it
        just gave up. The excess moves once and stops.

        Rate-limited like claiming, and skipped entirely while draining —
        a draining node is releasing everything anyway, and counting its
        seats twice would only slow that down.
        """
        over = len(self._held) + len(self._undead) - capacity
        if over <= 0:
            return []
        # Sorted, and NOT ordered by the ``preferred`` hint the way
        # claiming is. The hint records the last node to claim a seat,
        # which for every seat this node holds is this node — so it
        # cannot discriminate among them, and ordering by it would only
        # look like it was doing something. A stable order is what
        # matters: it makes a shed reproducible, and two nodes
        # rebalancing at once hold disjoint sets, so they cannot collide.
        shed: list[str] = []
        for handle in sorted(self._held)[: min(over, self.release_limit)]:
            logger.info(
                "seat_released_over_capacity",
                seat=handle,
                held=len(self._held),
                capacity=capacity,
            )
            await self.release(handle, ReleaseReason.DRAIN)
            # A release whose teardown could not be proven keeps the
            # lease — the seat leaves ``_held`` but lands in ``_undead``,
            # so no peer can take it. Reporting that as shed would claim
            # the rebalance made room it did not make, and this node
            # would shed a second seat next sweep on top of one it is
            # still serving.
            if handle not in self._held and handle not in self._undead:
                shed.append(handle)
        return shed

    async def _claim_up_to(
        self, seats: Sequence[str], room: int
    ) -> tuple[list[str], int | None]:
        claimed: list[str] = []
        for handle in await self._claim_order(seats):
            if len(claimed) >= room:
                break
            async with self._lock_for(handle):
                if handle in self._held:
                    continue  # re-claimed under us while we waited
                try:
                    lease = await self.leases.try_acquire(
                        seat_resource(handle),
                        owner=self.owner,
                        ttl_seconds=self.ttl_seconds,
                        # The STABLE node id, not the incarnation: the
                        # hint has to survive this process to be worth
                        # anything.
                        preferred=self.node_id,
                        protocol=self.protocol,
                    )
                except LeaseError:
                    # The store is unreachable, which says nothing about
                    # who owns what. Stop claiming — never take a seat on
                    # unknown state — and report what this pass actually
                    # got. Distinct from ``None``, which is a real
                    # refusal by a peer and only skips THIS seat.
                    logger.warning(
                        "seat_claim_unavailable",
                        seat=handle,
                        claimed=len(claimed),
                    )
                    break
                if lease is None:
                    continue
                self._held[handle] = _Held(
                    lease=lease,
                    handle=handle,
                    renewed_at=asyncio.get_running_loop().time(),
                )
                logger.info("seat_claimed", seat=handle, epoch=lease.epoch)
                await self._notify_acquire(handle, lease)
                # Counted only once the seat is ESTABLISHED. The hook
                # gives a failed takeover straight back (bad MCP command,
                # a credential resolving to nothing), so appending before
                # it meant a seat nothing runs still burned one of this
                # pass's claim slots, still logged as claimed, and — the
                # part that misleads an operator — still made ``claimed``
                # non-empty, which suppresses the protocol-block probe
                # below. A stalled mixed-version upgrade then reported
                # ``blocked_by_protocol=None`` beside a claim that never
                # happened.
                if handle in self._held:
                    claimed.append(handle)

        if claimed:
            return claimed, None
        # Nothing claimed. Distinguish "peers hold everything" (normal)
        # from "an older-protocol node is live and this build refuses to
        # claim beside it" (an upgrade that has stalled, and invisible
        # without this).
        return claimed, await self._protocol_block()

    async def _claim_order(self, seats: Sequence[str]) -> list[str]:
        """Unheld seats this node may run, its ``preferred`` ones first.

        The caller passes the ELIGIBLE handles, already filtered by
        placement — eligibility is not a preference to be sorted, it is
        the difference between a seat this node may hold and one it may
        not.

        Stickiness, and only stickiness: a seat whose hint names this
        node is *tried* first so a restart or a rolling deploy tends to
        land it back where its MCP children were already spawned. A
        matching hint is never a claim precondition and a non-matching
        one is never a reason to skip — the hint outlives the node that
        set it, so gating on it would strand every seat a dead node used
        to hold.

        Sorted within each group so every node walks the list the same
        way. The lease decides races; a stable order stops two nodes
        colliding on the same seat over and over and making no progress.

        Seats this node recently failed to acquire are skipped until
        their backoff expires — negative stickiness, the mirror of the
        positive kind. Peers are unaffected.
        """
        now = asyncio.get_running_loop().time()
        self._acquire_backoff = {
            h: until for h, until in self._acquire_backoff.items() if until > now
        }
        candidates = sorted(
            h
            for h in seats
            if h not in self._held
            and h not in self._undead
            and h not in self._acquire_backoff
        )
        try:
            hinted = await self.leases.preferred_resources("seat:", self.node_id)
        except LeaseError:
            return candidates
        if not hinted:
            return candidates
        mine = [h for h in candidates if seat_resource(h) in hinted]
        rest = [h for h in candidates if seat_resource(h) not in hinted]
        return mine + rest

    def _profile(self) -> NodeProfile:
        return self.profile or NodeProfile(node_id=self.node_id)

    def _placement(self, handle: str) -> SeatPlacement:
        if self.placement_of is None:
            return ANYWHERE
        try:
            return self.placement_of(handle) or ANYWHERE
        except Exception:
            # A seat the org no longer describes, or a caller that
            # raised. "Anywhere" is the pre-placement answer and the only
            # one that cannot strand the seat.
            logger.exception("seat_placement_lookup_failed", seat=handle)
            return ANYWHERE

    async def _plan(self, seats: Sequence[str]) -> tuple[PlacementPlan, int]:
        """``(what this node may claim and how much, live node count)``.

        When the fleet cannot be read, the LAST known membership is
        reused rather than assuming a fleet of one. A partial store
        failure — a timeout on the scan while point writes still succeed
        — would otherwise turn every node in the fleet into "I should own
        everything" simultaneously: mutual exclusion still prevents
        double ownership, but the fleet degenerates to whoever sweeps
        first taking the claim limit every 5 s until it holds the lot,
        undoing the balance for no reason. Before the first successful
        read there is nothing to reuse, and a fleet of one — this node —
        is the honest assumption.
        """
        me = self._profile()
        try:
            live = [
                NodeProfile.from_meta(lease.resource.removeprefix("node:"), lease.meta)
                for lease in await self.leases.list_live("node:")
            ]
            self._live_profiles = live
        except LeaseError:
            live = self._live_profiles
            logger.warning(
                "seat_capacity_unavailable",
                node=self.node_id,
                assumed_live_nodes=max(1, len(live)),
            )
        plan = plan_placement(
            [(h, self._placement(h)) for h in seats], me=me, live=live
        )
        self._live_nodes = max(1, plan.seat_nodes)
        self._check_fleet_roles([*live, me])
        return plan, plan.seat_nodes

    def _check_fleet_roles(self, live: Sequence[NodeProfile]) -> None:
        """Say something when the fleet has nobody doing one of the jobs.

        ``node.roles`` subtracts a role from THIS node, never from the
        company — so a fleet can be configured, node by node, into a
        shape where a whole job is done by nobody, and no single node's
        config is wrong. The symptoms are all absences: no scheduled
        tasks ever fire, no retention sweep ever runs, no webhook is ever
        received. Nothing errors, so nothing surfaces.

        Edge-triggered. A fleet that is missing ``workers`` for an hour
        should say so once and again when it comes back, not 720 times.
        """
        by_role = {
            NodeRole.INGRESS: any(n.runs_ingress for n in live),
            NodeRole.SEATS: any(n.runs_seats for n in live),
            NodeRole.WORKERS: any(n.runs_workers for n in live),
        }
        unmanned = {role for role, manned in by_role.items() if not manned}
        for role in sorted(unmanned - self._unmanned_roles):
            logger.warning(
                "fleet_role_unmanned",
                node=self.node_id,
                role=str(role),
                live_nodes=len({n.node_id for n in live}),
                hint=_UNMANNED_HINTS[role],
            )
        for role in sorted(self._unmanned_roles - unmanned):
            logger.info("fleet_role_manned", node=self.node_id, role=str(role))
        self._unmanned_roles = unmanned

    async def _protocol_block(self) -> int | None:
        try:
            floor = await self.leases.fleet_protocol_floor()
        except LeaseError:
            return None
        if floor is not None and floor < self.protocol:
            logger.warning(
                "seat_claims_blocked_by_older_protocol",
                node=self.node_id,
                fleet_floor=floor,
                this_node=self.protocol,
                hint=(
                    "an older-protocol node still holds leases; this node "
                    "will claim nothing until it drains. Finish the rolling "
                    "upgrade — do NOT roll back across a protocol bump "
                    "without stopping the fleet first."
                ),
            )
            return floor
        return None

    async def _renew_node_presence(self) -> None:
        """Keep this node counted in the fleet size.

        **Not while draining.** ``begin_drain`` drops the presence lease
        precisely so peers stop reserving capacity for a node that will
        never claim again — and the heartbeat runs on regardless, so an
        ungated renew here put the row straight back within one
        interval. A node shedding on a config posture can stay drained
        for minutes: with 3 nodes and 10 seats, the two healthy peers
        each compute ``ceil(10/3) = 4`` and two of the drained node's
        seats are claimable by nobody for the whole window.

        ``resume_claiming`` is what re-establishes it, and it is the only
        thing that should.

        Uses ``try_acquire`` rather than ``renew`` because it is
        idempotent for a live self-held lease and re-establishes the row
        after a lapse, which is exactly the behaviour presence wants.

        ``gated=False``: presence is membership, not work. Running it
        through the mixed-version gate makes a newer-protocol node
        invisible during the exact rolling upgrade the gate exists for —
        its peers then divide the seats by a count that excludes it, and
        its own capacity excludes it too. Nothing logged the refusal
        either, because the protocol block is only reported on the seat
        path.
        """
        if self._draining:
            return
        try:
            lease = await self.leases.try_acquire(
                node_resource(self.node_id),
                owner=self.owner,
                ttl_seconds=self.ttl_seconds,
                preferred=self.node_id,
                protocol=self.protocol,
                gated=False,
                # Re-sent on EVERY renew, not written once at claim. The
                # profile describes the live process, so a node that
                # restarts with different roles or labels has to be able
                # to correct what its peers believe about it without
                # waiting for its presence lease to lapse first.
                meta=self._profile().to_meta(),
            )
        except LeaseError:
            return
        if lease is None:
            logger.warning(
                "node_presence_refused",
                node=self.node_id,
                hint=(
                    "another process holds this node id's presence lease; "
                    "two processes sharing one node_id will miscount the "
                    "fleet and each compute too small a share"
                ),
            )
            return
        self._node_lease = lease

    async def _release_node_presence(self) -> None:
        lease, self._node_lease = self._node_lease, None
        if lease is None:
            return
        with contextlib.suppress(LeaseError):
            await self.leases.release(
                lease.resource, owner=self.owner, epoch=lease.epoch
            )

    async def _notify_acquire(self, handle: str, lease: Lease) -> None:
        if self.on_acquire is None:
            return
        try:
            await self.on_acquire(handle, lease)
        except Exception:
            # A seat whose takeover pipeline failed must not stay
            # claimed: it would look owned to the fleet while nothing
            # runs it. Give it straight back so a peer can try — and back
            # off here, because retrying a config-shaped failure every
            # 5 s spins at the cost of an MCP fork each time.
            logger.exception("seat_acquire_hook_failed", seat=handle)
            self._acquire_backoff[handle] = (
                asyncio.get_running_loop().time() + self.acquire_backoff_seconds
            )
            # _release_locked, not release(): we are already inside this
            # seat's lock, and asyncio.Lock is not reentrant. The reason
            # tells the hook the seat was never fully established.
            await self._release_locked(handle, ReleaseReason.ACQUIRE_FAILED)

    async def _note_admission(self, handle: str, *, admitted: bool) -> None:
        """Report a change in whether this seat's ownership is provable.

        Edge-triggered: a store that is unreachable for an hour produces
        one call, not one per heartbeat, so the consumer is stopped once
        and resumed once. A hook failure is logged and swallowed — it
        cannot be allowed to abort the heartbeat, which is what keeps
        every OTHER seat on this node alive.
        """
        was_unproven = handle in self._unproven_admission
        if admitted:
            if not was_unproven:
                return
            self._unproven_admission.discard(handle)
        else:
            if was_unproven:
                return
            self._unproven_admission.add(handle)
        logger.info("seat_admission_changed", seat=handle, admitted=admitted)
        if self.on_admission is None:
            return
        try:
            await self.on_admission(handle, admitted)
        except Exception:
            logger.exception(
                "seat_admission_hook_failed", seat=handle, admitted=admitted
            )

    async def _notify_release(
        self, handle: str, lease: Lease, reason: ReleaseReason
    ) -> None:
        """Run the teardown hook, letting the caller see what happened.

        Exceptions are NOT swallowed. A release hook that fails leaves
        the seat's consumer and MCP children alive in this process while
        the lease goes to a peer — two live consumers on one inbox, the
        exact state ownership exists to prevent — and the old
        ``suppress(Exception)`` produced no evidence at all. A
        :class:`SeatReleaseError` is the caller's cue to fail closed;
        anything else is logged and re-raised as one, because an
        unexpected failure is no more proof of teardown than an expected
        one.
        """
        if self.on_release is None:
            return
        try:
            await self.on_release(handle, lease, reason)
        except SeatReleaseError:
            raise
        except Exception as exc:
            logger.exception(
                "seat_release_hook_failed", seat=handle, reason=str(reason)
            )
            raise SeatReleaseError(
                f"release hook for seat {handle!r} raised {exc!r}"
            ) from exc
