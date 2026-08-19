"""Cross-process resource ownership: TTL leases with a fencing token.

The primitive every multi-node duty is built on.  A node claims a
resource (``seat:{handle}``, ``worker:{duty}``), renews it on a heartbeat,
and loses it by crashing or releasing.  See ``SCALING.md`` for how seat
ownership uses it, and ``migrations/017_leases.sql`` for why it is a lease
rather than an advisory lock.

Three rules carry the correctness of everything above:

1. **Every write on behalf of a resource is fenced by the epoch.**
   ``try_acquire`` returns the epoch; callers thread it into
   ``WHERE owner_epoch = $epoch`` predicates.  A zombie's late write
   bounces instead of corrupting state.  The epoch is therefore
   **monotonic for the lifetime of the resource** — releasing a lease
   expires the row in place rather than deleting it, because a deleted
   row would restart the counter at 1 and hand the next owner a token
   the previous one is still using.
2. **A lapsed lease cannot be renewed, only re-acquired** — and
   re-acquiring bumps the epoch even for the same owner.  During the gap
   the owner's in-flight work was unprotected, so it must be fenced
   against its own past self.
3. **``owner`` identifies a process incarnation, not a machine.** A live
   lease is renewable by its own owner string, so two processes sharing
   one would both hold the resource at the same epoch — and the default
   node id is the shared constant ``node-0``.  Callers pass
   :func:`~crewlet.config.resolve_node_incarnation` (``{node_id}:{random}``),
   which is unique per process; the *stable* node id goes in
   ``preferred``, where restart-stability is what you actually want.

Both backends (:class:`LeaseStore` over PostgreSQL, :class:`MemoryLeaseStore`
for tests) implement :class:`LeaseBackend` with identical semantics, and
the same contract suite runs against both.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import Any, Protocol

from crewlet._logging import get_logger
from crewlet.db.client import SQLExecutor

logger = get_logger("db.leases")

# The seat-host protocol this build speaks.
#
# A rolling upgrade puts a vN and a vN+1 node on the same lease table and
# the same topics at the same time. That is fine as long as both agree on
# what *holding a lease means* — and catastrophic when they do not: two
# nodes that disagree about whether a seat's inbox consumer is
# owner-only, or about whether a turn claim fences a resume, will each be
# individually correct and jointly wrong.
#
# So the rule is asymmetric, and deliberately so: **a node refuses to
# claim anything while a live lease is held at a lower protocol.** The
# older nodes keep working (they cannot know about a check that postdates
# them); the newer ones wait, visibly, until the last old lease lapses or
# is released. A rolling deploy converges because that is exactly what a
# rolling deploy does — drain the old, then the new take over.
#
# Two consequences worth stating plainly:
#
# - **Schema evolution here is additive-only.** A column the older build
#   does not select is invisible to it; a column it *requires* is a
#   crash. Add, never rename or repurpose.
# - **A downgrade across a protocol bump needs a full drain.** An older
#   build has no protocol check at all, so it will happily take over a
#   newer node's expired leases. Nothing in the table can stop it; the
#   only protection is to stop the whole fleet before rolling back.
#
# Bump this when the meaning of holding a lease changes — not when
# something merely gains a field.
#
# v2: holding a seat lease now means "consult and settle
# ``turn_completions`` for this seat's triggers". A v1 node cannot: it
# takes a seat over, never reads the completion row, and re-runs a turn
# whose outbound effects already shipped — silent duplication, and
# invisible, because a v1 node logs nothing about a table it does not
# know exists. That is exactly the failure the ledger was built to
# remove, so the two builds must not hold seats at the same time.
PROTOCOL_VERSION = 2


class LeaseError(RuntimeError):
    """The lease store could not be reached or answered.

    Deliberately distinct from a ``False`` return.  ``False`` means the
    lease is definitively **not held** — lapsed, moved, or advanced — and
    a caller must shed the work it covered.  A database blip means
    nothing about ownership: the row is untouched and still held, so
    treating it as loss would tear down a healthy node's seats for the
    remainder of the TTL while no peer can claim them.
    """


def seat_resource(handle: str) -> str:
    """Lease resource name for an agent seat."""
    return f"seat:{handle}"


def worker_resource(duty: str) -> str:
    """Lease resource name for a per-company singleton duty."""
    return f"worker:{duty}"


def node_resource(node_id: str) -> str:
    """Lease resource name for a node's own presence.

    Every node holds one, renewed on the same heartbeat as its seats.
    Counting the live ones is how a node learns the fleet size it has to
    divide the seats by — there is no membership service, and inferring
    the count from *seat* ownership cannot work: a fleet where nobody has
    claimed anything yet would read as zero nodes, and every node would
    then believe it should take every seat.
    """
    return f"node:{node_id}"


def is_seat_resource(resource: str) -> bool:
    return resource.startswith("seat:")


def is_node_resource(resource: str) -> bool:
    return resource.startswith("node:")


@dataclass(frozen=True, slots=True)
class Lease:
    """A held lease. ``epoch`` is the fencing token — thread it into writes."""

    resource: str
    owner: str
    epoch: int
    expires_at: datetime
    """Timezone-aware UTC deadline. Typed, and identical in both backends:
    a heartbeat computes its next tick from this, and a float-vs-datetime
    divergence between the memory twin and PostgreSQL would pass every
    test and raise ``TypeError`` on the first production tick."""
    preferred: str = ""
    protocol: int = 1


class LeaseBackend(Protocol):
    """The lease surface. Implemented by PostgreSQL and memory backends."""

    async def try_acquire(
        self,
        resource: str,
        *,
        owner: str,
        ttl_seconds: float,
        preferred: str = "",
        protocol: int = 1,
        gated: bool = True,
    ) -> Lease | None: ...

    async def renew(
        self, resource: str, *, owner: str, epoch: int, ttl_seconds: float
    ) -> bool: ...

    async def release(self, resource: str, *, owner: str, epoch: int) -> bool: ...

    async def get(self, resource: str) -> Lease | None: ...

    async def list_owned(self, owner: str) -> list[Lease]: ...

    async def list_live(self, prefix: str) -> list[Lease]: ...

    async def preferred_resources(self, prefix: str, node_id: str) -> set[str]: ...

    async def fleet_protocol_floor(self) -> int | None: ...


def _validate(resource: str, owner: str, ttl_seconds: float | None = None) -> None:
    if not resource:
        raise ValueError("resource is required")
    if not owner:
        raise ValueError("owner is required")
    if ttl_seconds is not None and ttl_seconds <= 0:
        raise ValueError("ttl_seconds must be positive")


def _row_to_lease(row: dict[str, Any]) -> Lease:
    return Lease(
        resource=str(row["resource"]),
        owner=str(row["owner"]),
        epoch=int(row["epoch"]),
        expires_at=row["expires_at"],
        preferred=str(row.get("preferred") or ""),
        protocol=int(row.get("protocol") or 1),
    )


class LeaseStore:
    """PostgreSQL-backed :class:`LeaseBackend`."""

    def __init__(self, db: SQLExecutor) -> None:
        self._db = db

    async def try_acquire(
        self,
        resource: str,
        *,
        owner: str,
        ttl_seconds: float,
        preferred: str = "",
        protocol: int = 1,
        gated: bool = True,
    ) -> Lease | None:
        """Claim ``resource`` for ``owner``, or return ``None`` if held by another.

        Succeeds when the resource is unclaimed, its lease has expired, or
        ``owner`` already holds it (in which case this doubles as a renew).
        The epoch increments on every ownership change AND on a same-owner
        re-acquire after expiry — see the module docstring for why the
        latter matters.

        A DB error returns ``None``: never proceed as owner on unknown
        state.  This mirrors ``OnboardingMarkerStore.try_claim_pass``,
        whose fail-closed behaviour this generalises.

        Refuses outright — same ``None`` — while ANY live lease is held
        at a lower ``protocol``.  The guard is part of the same statement
        rather than a read-then-claim pair, because a read-then-claim
        loses the race it exists to prevent: an older node can take a
        lease in the window between the two. Ask
        :meth:`fleet_protocol_floor` once per claim sweep to tell a
        protocol refusal apart from a peer simply holding the seat.

        ``gated=False`` skips that refusal, and exactly one caller needs
        it: **node presence**. Presence is membership, not work. A
        newer-protocol node that cannot register itself during the
        rolling upgrade the gate exists for is invisible in
        ``list_live("node:")`` — its peers then divide the seats by a
        count that excludes it and each take a larger share, while its
        own capacity also excludes itself. It stays asymmetric: the
        refusal only ever looks for leases at a LOWER protocol, so a
        presence row written at the newer protocol blocks nobody.
        """
        _validate(resource, owner, ttl_seconds)
        try:
            row = await self._db.fetchrow(
                """
                INSERT INTO leases (
                    resource, owner, epoch, expires_at, preferred, protocol
                )
                SELECT $1, $2, 1, now() + make_interval(secs => $3), $4, $5
                WHERE NOT $6 OR NOT EXISTS (
                    SELECT 1 FROM leases
                    WHERE expires_at > now() AND protocol < $5
                )
                ON CONFLICT (resource) DO UPDATE
                SET owner      = EXCLUDED.owner,
                    epoch      = CASE
                        WHEN leases.owner = EXCLUDED.owner
                         AND leases.expires_at > now()
                        THEN leases.epoch
                        ELSE leases.epoch + 1
                    END,
                    expires_at = EXCLUDED.expires_at,
                    preferred  = COALESCE(
                        NULLIF(EXCLUDED.preferred, ''), leases.preferred
                    ),
                    protocol   = EXCLUDED.protocol,
                    updated_at = now()
                WHERE (
                        leases.expires_at <= now()
                     OR leases.owner = EXCLUDED.owner
                      )
                  AND (
                        NOT $6
                     OR NOT EXISTS (
                            SELECT 1 FROM leases older
                            WHERE older.expires_at > now()
                              AND older.protocol < EXCLUDED.protocol
                        )
                      )
                RETURNING resource, owner, epoch, expires_at, preferred, protocol
                """,
                resource,
                owner,
                float(ttl_seconds),
                preferred,
                int(protocol),
                bool(gated),
            )
        except Exception:
            logger.exception("lease_acquire_failed", resource=resource, owner=owner)
            return None
        if row is None:
            return None
        lease = _row_to_lease(row)
        logger.debug(
            "lease_acquired",
            resource=resource,
            owner=owner,
            epoch=lease.epoch,
        )
        return lease

    async def renew(
        self, resource: str, *, owner: str, epoch: int, ttl_seconds: float
    ) -> bool:
        """Extend a lease this owner still holds at this epoch.

        Returns ``False`` only when the lease is definitively **not held**
        — lapsed, moved, or advanced.  The caller must then stop the work
        the lease covered.  A lapsed lease is deliberately NOT renewable:
        re-acquiring is the only way back, and that mints a new epoch
        which fences the gap.

        Raises :class:`LeaseError` when the store could not be reached.
        That is emphatically not the same answer: the row is untouched
        and still held, so reporting loss would make a healthy node shed
        every seat it owns over a two-second connection blip — and no
        peer could pick them up until the TTL ran out anyway.  A
        heartbeat should retry; it has until ``expires_at`` to succeed.
        """
        _validate(resource, owner, ttl_seconds)
        try:
            row = await self._db.fetchrow(
                """
                UPDATE leases
                SET expires_at = now() + make_interval(secs => $4),
                    updated_at = now()
                WHERE resource = $1
                  AND owner = $2
                  AND epoch = $3
                  AND expires_at > now()
                RETURNING resource
                """,
                resource,
                owner,
                int(epoch),
                float(ttl_seconds),
            )
        except Exception as exc:
            logger.warning(
                "lease_renew_unavailable",
                resource=resource,
                owner=owner,
                error=str(exc),
            )
            raise LeaseError(f"could not renew lease {resource!r}: {exc}") from exc
        return row is not None

    async def release(self, resource: str, *, owner: str, epoch: int) -> bool:
        """Release a lease, predicated on owner AND epoch.

        The predicate is one of two things this method has to get right.
        An unqualified release lets a departing owner clear its
        *successor's* live lease — the defect this generalises away from
        ``OnboardingMarkerStore.release_claim``.

        The other is that release **expires the row in place instead of
        deleting it**.  Deleting would destroy the epoch counter, so the
        next acquirer would start again at 1 — and a zombie coroutine
        from the released tenure, still fencing its writes with epoch 1,
        would sail straight through the new owner's predicate.  The
        counter has to be monotonic for the lifetime of the resource, not
        the lifetime of a row.  Expiring also keeps ``preferred``, so a
        rolling deploy can bring the resource back to the node whose
        caches and MCP children are already warm.
        """
        _validate(resource, owner)
        try:
            row = await self._db.fetchrow(
                """
                UPDATE leases
                SET expires_at = now(), owner = '', updated_at = now()
                WHERE resource = $1 AND owner = $2 AND epoch = $3
                RETURNING resource
                """,
                resource,
                owner,
                int(epoch),
            )
        except Exception as exc:
            logger.warning(
                "lease_release_unavailable",
                resource=resource,
                owner=owner,
                error=str(exc),
            )
            raise LeaseError(f"could not release lease {resource!r}: {exc}") from exc
        released = row is not None
        if released:
            logger.debug("lease_released", resource=resource, owner=owner, epoch=epoch)
        return released

    async def get(self, resource: str) -> Lease | None:
        """Return the lease row, live or lapsed, or ``None`` if never claimed.

        Raises :class:`LeaseError` if the store is unreachable — same
        contract as :meth:`renew` and :meth:`release`. ``None`` means
        "no such lease", and a caller must be able to trust that.
        """
        try:
            row = await self._db.fetchrow(
                """
                SELECT resource, owner, epoch, expires_at, preferred, protocol
                FROM leases WHERE resource = $1
                """,
                resource,
            )
        except Exception as exc:
            raise LeaseError(f"could not read lease {resource!r}: {exc}") from exc
        return _row_to_lease(row) if row else None

    async def list_owned(self, owner: str) -> list[Lease]:
        """Every LIVE lease held by ``owner`` (lapsed ones are not owned).

        Raises :class:`LeaseError` if the store is unreachable: an empty
        list means "this owner holds nothing", which a drain or takeover
        pass would act on.
        """
        try:
            rows = await self._db.execute(
                """
                SELECT resource, owner, epoch, expires_at, preferred, protocol
                FROM leases
                WHERE owner = $1 AND expires_at > now()
                ORDER BY resource
                """,
                owner,
            )
        except Exception as exc:
            raise LeaseError(f"could not list leases for {owner!r}: {exc}") from exc
        return [_row_to_lease(r) for r in rows]

    async def list_live(self, prefix: str) -> list[Lease]:
        """Every LIVE lease whose resource starts with ``prefix``.

        The fleet-membership read: ``list_live("node:")`` is how a node
        learns how many peers it is dividing the seats between. Lapsed
        rows are excluded, so a node that died stops counting toward
        capacity as soon as its lease runs out — which is what lets the
        survivors widen their share instead of leaving seats dark.

        Raises :class:`LeaseError` when unreachable: an empty list means
        "nobody holds anything matching", and a capacity calculation
        would act on that.
        """
        try:
            rows = await self._db.execute(
                """
                SELECT resource, owner, epoch, expires_at, preferred, protocol
                FROM leases
                WHERE resource LIKE $1 || '%' AND expires_at > now()
                ORDER BY resource
                """,
                prefix,
            )
        except Exception as exc:
            raise LeaseError(
                f"could not list live leases for {prefix!r}: {exc}"
            ) from exc
        return [_row_to_lease(r) for r in rows]

    async def preferred_resources(self, prefix: str, node_id: str) -> set[str]:
        """Resources whose ``preferred`` hint names ``node_id``.

        Deliberately includes LAPSED rows — that is the whole point. The
        hint survives release and expiry so a node coming back from a
        restart can find the seats whose MCP children and caches it had
        warm. A live-only read would return nothing in exactly the case
        the hint exists for.

        One indexed scan per sweep, not one lookup per seat — see the
        ``leases_preferred_idx`` partial index (migration 024). Without
        it this is a sequential scan of every lease row on every sweep of
        every node, and lease rows never shrink: ``release`` expires them
        in place so the epoch stays monotonic.
        """
        try:
            rows = await self._db.execute(
                """
                SELECT resource FROM leases
                WHERE resource LIKE $1 || '%' AND preferred = $2
                """,
                prefix,
                node_id,
            )
        except Exception as exc:
            raise LeaseError(f"could not read placement hints: {exc}") from exc
        return {str(r["resource"]) for r in rows}

    async def fleet_protocol_floor(self) -> int | None:
        """Lowest ``protocol`` among LIVE leases, or ``None`` if none are held.

        The observability half of the mixed-version gate: ``try_acquire``
        enforces it silently (it can only answer yes or no), so a node
        that claims nothing during a botched upgrade would otherwise look
        identical to one whose peers simply hold every seat. Called once
        per claim sweep, not per resource.

        Raises :class:`LeaseError` when unreachable — an unknown floor
        must not read as "nothing older is out there".
        """
        try:
            value = await self._db.fetchval(
                "SELECT MIN(protocol) FROM leases WHERE expires_at > now()"
            )
        except Exception as exc:
            raise LeaseError(f"could not read fleet protocol floor: {exc}") from exc
        return int(value) if value is not None else None


class MemoryLeaseStore:
    """In-memory :class:`LeaseBackend` twin for tests and single-node runs.

    A semantic twin, not a stub: same epoch rules, same expiry rules, same
    fail-closed edges.  The contract suite runs against this and
    :class:`LeaseStore` alike, so a divergence is a failing test rather
    than a production-only surprise.
    """

    def __init__(self) -> None:
        self._rows: dict[str, dict[str, Any]] = {}

    def _now(self) -> datetime:
        return datetime.now(UTC)

    async def try_acquire(
        self,
        resource: str,
        *,
        owner: str,
        ttl_seconds: float,
        preferred: str = "",
        protocol: int = 1,
        gated: bool = True,
    ) -> Lease | None:
        _validate(resource, owner, ttl_seconds)
        now = self._now()
        # Mixed-version gate — the twin of the ``NOT EXISTS`` guard in
        # LeaseStore.try_acquire. Refuse while any live lease is held at
        # an older protocol, unless the caller opted out (node presence:
        # membership is not work — see LeaseStore.try_acquire).
        if gated and any(
            r["expires_at"] > now and int(r.get("protocol") or 1) < int(protocol)
            for r in self._rows.values()
        ):
            return None
        row = self._rows.get(resource)
        if row is None:
            epoch = 1
            kept_preferred = preferred
        else:
            live = row["expires_at"] > now
            mine = row["owner"] == owner
            if live and not mine:
                return None  # somebody else holds it
            # Unbroken same-owner hold keeps the epoch; anything else
            # (takeover, or re-acquire after our own lease lapsed) mints
            # a new one so the gap is fenced.
            epoch = int(row["epoch"]) if (live and mine) else int(row["epoch"]) + 1
            kept_preferred = preferred or str(row.get("preferred") or "")
        self._rows[resource] = {
            "resource": resource,
            "owner": owner,
            "epoch": epoch,
            "expires_at": now + timedelta(seconds=float(ttl_seconds)),
            "preferred": kept_preferred,
            "protocol": int(protocol),
        }
        return _row_to_lease(self._rows[resource])

    async def renew(
        self, resource: str, *, owner: str, epoch: int, ttl_seconds: float
    ) -> bool:
        _validate(resource, owner, ttl_seconds)
        row = self._rows.get(resource)
        if row is None:
            return False
        if (
            row["owner"] != owner
            or int(row["epoch"]) != int(epoch)
            or row["expires_at"] <= self._now()
        ):
            return False
        row["expires_at"] = self._now() + timedelta(seconds=float(ttl_seconds))
        return True

    async def release(self, resource: str, *, owner: str, epoch: int) -> bool:
        # Expire in place, never delete — the epoch must stay monotonic
        # for the resource's lifetime. See LeaseStore.release.
        _validate(resource, owner)
        row = self._rows.get(resource)
        if row is None or row["owner"] != owner or int(row["epoch"]) != int(epoch):
            return False
        row["owner"] = ""
        row["expires_at"] = self._now()
        return True

    async def get(self, resource: str) -> Lease | None:
        row = self._rows.get(resource)
        return _row_to_lease(row) if row else None

    async def list_owned(self, owner: str) -> list[Lease]:
        now = self._now()
        return [
            _row_to_lease(r)
            for r in sorted(self._rows.values(), key=lambda r: str(r["resource"]))
            if r["owner"] == owner and r["expires_at"] > now
        ]

    async def list_live(self, prefix: str) -> list[Lease]:
        now = self._now()
        return [
            _row_to_lease(r)
            for r in sorted(self._rows.values(), key=lambda r: str(r["resource"]))
            if str(r["resource"]).startswith(prefix) and r["expires_at"] > now
        ]

    async def preferred_resources(self, prefix: str, node_id: str) -> set[str]:
        # Lapsed rows included on purpose — see LeaseStore.
        return {
            str(r["resource"])
            for r in self._rows.values()
            if str(r["resource"]).startswith(prefix)
            and str(r.get("preferred") or "") == node_id
        }

    async def fleet_protocol_floor(self) -> int | None:
        now = self._now()
        live = [
            int(r.get("protocol") or 1)
            for r in self._rows.values()
            if r["expires_at"] > now
        ]
        return min(live) if live else None
