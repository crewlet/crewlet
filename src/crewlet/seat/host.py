"""Which seats this node owns, and how it comes to own them.

A seat is an agent's chair in the org: its turns, its per-role MCP
children, and its inbox consumer all belong to whoever holds
``seat:{handle}``. This module is the placement half — claiming, holding,
and letting go. What a node *does* with an owned seat is wired in
:mod:`crewlet.engine`.

The placement policy is deliberately dumb, and the reasons are worth
stating because a cleverer one is a standing temptation:

- **Greedy claim up to a fair share.** Capacity is ``ceil(seats / live
  nodes)``, live nodes being the count of ``node:*`` presence leases. No
  membership service, no gossip, no coordinator — every node computes the
  same number from the same table and stops there. Two nodes racing for
  the last seat is resolved by the lease, not by the arithmetic.
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
"""

from __future__ import annotations

import asyncio
import contextlib
import math
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass, field
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


@dataclass(frozen=True, slots=True)
class SweepResult:
    """What one placement pass did, for logs, tests, and /health."""

    held: int
    capacity: int
    live_nodes: int
    claimed: tuple[str, ...] = ()
    lost: tuple[str, ...] = ()
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
    on_acquire: Callable[[str, Lease], Awaitable[None]] | None = None
    on_release: Callable[[str, Lease], Awaitable[None]] | None = None
    ttl_seconds: float = SEAT_LEASE_TTL_SECONDS
    heartbeat_seconds: float = SEAT_HEARTBEAT_INTERVAL_SECONDS
    sweep_seconds: float = SEAT_SWEEP_INTERVAL_SECONDS
    claim_limit: int = SEAT_CLAIM_LIMIT_PER_SWEEP
    protocol: int = PROTOCOL_VERSION

    _held: dict[str, _Held] = field(default_factory=dict, init=False)
    _seat_locks: dict[str, asyncio.Lock] = field(default_factory=dict, init=False)
    _node_lease: Lease | None = field(default=None, init=False)
    _tasks: list[asyncio.Task[Any]] = field(default_factory=list, init=False)
    _running: bool = field(default=False, init=False)
    _draining: bool = field(default=False, init=False)
    _last: SweepResult | None = field(default=None, init=False)

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

    def owns(self, handle: str) -> bool:
        return handle in self._held

    def epoch_for(self, handle: str) -> int | None:
        """The fencing token for a seat, or ``None`` if not owned.

        Thread this into every write made on the seat's behalf. A write
        without it is a write a zombie can also make.
        """
        held = self._held.get(handle)
        return held.lease.epoch if held is not None else None

    @property
    def held_handles(self) -> tuple[str, ...]:
        return tuple(sorted(self._held))

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
        logger.info("seat_host_draining", node=self.node_id, held=len(self._held))

    async def stop(self) -> None:
        if not self._running:
            return
        self._running = False
        for task in self._tasks:
            await cancel_and_wait(task)
        self._tasks.clear()
        await self.release_all()
        await self._release_node_presence()
        logger.info("seat_host_stopped", node=self.node_id)

    async def release_all(self) -> None:
        """Hand every seat back, one at a time.

        Releasing expires the row in place rather than deleting it, so
        the epoch stays monotonic and ``preferred`` survives — a peer can
        claim immediately, and a rolling deploy tends to bring the seat
        home afterwards.
        """
        for handle in list(self._held):
            await self.release(handle)

    async def release(self, handle: str) -> bool:
        async with self._lock_for(handle):
            return await self._release_locked(handle)

    async def _release_locked(self, handle: str) -> bool:
        held = self._held.pop(handle, None)
        if held is None:
            return False
        await self._notify_release(handle, held.lease)
        try:
            return await self.leases.release(
                held.lease.resource, owner=self.owner, epoch=held.lease.epoch
            )
        except LeaseError:
            # The seat is already torn down locally; the row simply
            # lapses on its own. Nothing here is worth failing a drain.
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
        now = asyncio.get_running_loop().time()
        for handle, held in list(self._held.items()):
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
                held.renewed_at = now
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
                logger.warning(
                    "seat_lease_lost",
                    seat=handle,
                    epoch=held.lease.epoch,
                    hint="a peer may already own this seat; dropping it locally",
                )
                await self._notify_release(handle, held.lease)
        await self._renew_node_presence()
        return tuple(lost)

    async def sweep(self) -> SweepResult:
        """One placement pass: claim up to a fair share, rate-limited."""
        seats = list(self.seats())
        capacity, live_nodes = await self._capacity(len(seats))

        # Seats this node holds that the org no longer has (a role was
        # decommissioned under a live apply) are released, not kept.
        for handle in list(self._held):
            if handle not in seats:
                logger.info("seat_released_role_gone", seat=handle)
                await self.release(handle)

        claimed: list[str] = []
        blocked: int | None = None
        if not self._draining:
            room = min(capacity - len(self._held), self.claim_limit)
            if room > 0:
                claimed, blocked = await self._claim_up_to(seats, room)

        result = SweepResult(
            held=len(self._held),
            capacity=capacity,
            live_nodes=live_nodes,
            claimed=tuple(claimed),
            lost=(),
            blocked_by_protocol=blocked,
        )
        self._last = result
        if claimed or blocked is not None:
            logger.info(
                "seat_sweep",
                node=self.node_id,
                held=result.held,
                capacity=capacity,
                live_nodes=live_nodes,
                claimed=list(claimed),
                blocked_by_protocol=blocked,
            )
        return result

    # ── internals ────────────────────────────────────────────────────

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
                lease = await self.leases.try_acquire(
                    seat_resource(handle),
                    owner=self.owner,
                    ttl_seconds=self.ttl_seconds,
                    # The STABLE node id, not the incarnation: the hint
                    # has to survive this process to be worth anything.
                    preferred=self.node_id,
                    protocol=self.protocol,
                )
                if lease is None:
                    continue
                self._held[handle] = _Held(
                    lease=lease,
                    handle=handle,
                    renewed_at=asyncio.get_running_loop().time(),
                )
                claimed.append(handle)
                logger.info("seat_claimed", seat=handle, epoch=lease.epoch)
                await self._notify_acquire(handle, lease)

        if claimed:
            return claimed, None
        # Nothing claimed. Distinguish "peers hold everything" (normal)
        # from "an older-protocol node is live and this build refuses to
        # claim beside it" (an upgrade that has stalled, and invisible
        # without this).
        return claimed, await self._protocol_block()

    async def _claim_order(self, seats: Sequence[str]) -> list[str]:
        """Unheld seats, this node's ``preferred`` ones first.

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
        """
        candidates = sorted(h for h in seats if h not in self._held)
        try:
            hinted = await self.leases.preferred_resources("seat:", self.node_id)
        except LeaseError:
            return candidates
        if not hinted:
            return candidates
        mine = [h for h in candidates if seat_resource(h) in hinted]
        rest = [h for h in candidates if seat_resource(h) not in hinted]
        return mine + rest

    async def _capacity(self, seat_count: int) -> tuple[int, int]:
        """``(fair share, live node count)``.

        Falls back to a share of *everything* when the node count cannot
        be read — the single-node case is the common one, and a node that
        cannot see the table is not helped by claiming nothing.
        """
        try:
            live_nodes = max(1, len(await self.leases.list_live("node:")))
        except LeaseError:
            logger.warning("seat_capacity_unavailable", node=self.node_id)
            live_nodes = 1
        return math.ceil(seat_count / live_nodes), live_nodes

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

        Uses ``try_acquire`` rather than ``renew`` because it is
        idempotent for a live self-held lease and re-establishes the row
        after a lapse, which is exactly the behaviour presence wants.
        """
        try:
            lease = await self.leases.try_acquire(
                node_resource(self.node_id),
                owner=self.owner,
                ttl_seconds=self.ttl_seconds,
                preferred=self.node_id,
                protocol=self.protocol,
            )
        except LeaseError:
            return
        if lease is not None:
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
            # runs it. Give it straight back so a peer can try.
            logger.exception("seat_acquire_hook_failed", seat=handle)
            # _release_locked, not release(): we are already inside this
            # seat's lock, and asyncio.Lock is not reentrant.
            await self._release_locked(handle)

    async def _notify_release(self, handle: str, lease: Lease) -> None:
        if self.on_release is None:
            return
        with contextlib.suppress(Exception):
            await self.on_release(handle, lease)
