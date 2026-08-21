"""End-to-end tests for the engine's config **control plane**.

Config activation used to reach the engine over a durable
competing-consumer subscription (group ``engine-config``) whose handler
did the apply itself.  With N engine processes that meant exactly one
applied any given revision and the rest ran the previous company
indefinitely, while the dashboard reported success because the one node
that applied it published ``ok``.

What replaced it, and what these tests pin down:

* an append-only activation log is the authoritative pointer;
* every node polls it and converges (:meth:`Engine._reconcile_config_once`);
* every node records what it managed to do, so partial apply is visible;
* the ``revision_activated`` event survives only as a **nudge** that
  shortens the poll — never as the thing that carries the work.
"""

from __future__ import annotations

import asyncio
from datetime import UTC, datetime
from unittest.mock import AsyncMock, MagicMock
from uuid import uuid4

from crewlet.config import (
    ApiConfig,
    BootstrapConfig,
    BootstrapProvidersConfig,
    DatabaseConfig,
    KnowledgeStoreConfig,
    QueueConfig,
)
from crewlet.db.company_config import CompanyConfigRevision
from crewlet.db.config_plane import ApplyStatus, Posture
from crewlet.engine import Engine
from crewlet.events.types import ConfigRevisionActivated, ConfigRevisionApplied
from crewlet.queue.memory import MemoryEventQueue


def _bootstrap() -> BootstrapConfig:
    return BootstrapConfig(
        providers=BootstrapProvidersConfig(
            queue=QueueConfig(),
            database=DatabaseConfig(),
            knowledge=KnowledgeStoreConfig(type="memory"),
        ),
        api=ApiConfig(),
    )


def _revision(payload: dict) -> CompanyConfigRevision:
    return CompanyConfigRevision(
        revision_id=uuid4(),
        parent_revision_id=None,
        created_at=datetime.now(UTC),
        created_by="cli",
        source="cli",
        summary="seed",
        payload=payload,
        is_active=True,
        activated_at=datetime.now(UTC),
    )


def _store_with_revision(payload: dict) -> tuple[MagicMock, CompanyConfigRevision]:
    rev = _revision(payload)
    store = MagicMock()
    store.get_revision = AsyncMock(return_value=rev)
    store.get_active = AsyncMock(return_value=rev)
    return store, rev


async def _collect_applied(queue: MemoryEventQueue) -> list[ConfigRevisionApplied]:
    applied: list[ConfigRevisionApplied] = []

    async def _capture(event: ConfigRevisionApplied) -> None:
        applied.append(event)

    await queue.subscribe(
        topic="crewlet.config.revision_applied",
        group="test-collector",
        handler=_capture,
    )
    return applied


async def test_engine_converges_on_the_activation_pointer() -> None:
    queue = MemoryEventQueue()
    store, rev = _store_with_revision({"name": "Acme via reconcile", "mission": "test"})

    engine = Engine.from_bootstrap(
        _bootstrap(), event_queue=queue, company_config_store=store
    )
    await queue.start()
    applied = await _collect_applied(queue)

    try:
        await engine.start()
        assert engine._active_config is None  # unconfigured

        plane = engine._config_plane()
        epoch = await plane.record_activation(rev.revision_id)
        assert await engine._reconcile_config_once() is Posture.SERVE

        assert engine._active_config is not None
        assert engine._active_config.name == "Acme via reconcile"
        assert engine.org.name == "Acme via reconcile"
        assert engine._applied_epoch == epoch

        for _ in range(20):
            await asyncio.sleep(0)
            if applied:
                break
        assert any(
            e.status == "ok" and e.revision_id == str(rev.revision_id) for e in applied
        )
    finally:
        await engine.stop()
        await queue.stop()


async def test_converged_node_does_not_re_apply_on_the_next_tick() -> None:
    """A tick at the current epoch must not re-apply.

    Re-applying restarts every MCP child, so a reconcile loop that
    re-converged each tick would tear down the tool surface every 15
    seconds forever.
    """
    queue = MemoryEventQueue()
    store, rev = _store_with_revision({"name": "Steady", "mission": "test"})
    engine = Engine.from_bootstrap(
        _bootstrap(), event_queue=queue, company_config_store=store
    )
    await queue.start()
    try:
        await engine.start()
        plane = engine._config_plane()
        await plane.record_activation(rev.revision_id)
        await engine._reconcile_config_once()
        assert store.get_revision.await_count == 1

        await engine._reconcile_config_once()
        await engine._reconcile_config_once()
        assert store.get_revision.await_count == 1
    finally:
        await engine.stop()
        await queue.stop()


async def test_reactivating_the_same_revision_converges_again() -> None:
    """The rotation gesture must move the pointer and re-converge.

    Re-activating an unchanged revision is the documented way to make a
    running engine pick up a rotated credential.  Keyed on the revision
    id, a reconcile could never express it — hence the epoch log.
    """
    queue = MemoryEventQueue()
    store, rev = _store_with_revision({"name": "Rotator", "mission": "test"})
    engine = Engine.from_bootstrap(
        _bootstrap(), event_queue=queue, company_config_store=store
    )
    await queue.start()
    try:
        await engine.start()
        plane = engine._config_plane()
        await plane.record_activation(rev.revision_id)
        await engine._reconcile_config_once()
        first_epoch = engine._applied_epoch

        second = await plane.record_activation(rev.revision_id)
        await engine._reconcile_config_once()
        assert second > first_epoch
        assert engine._applied_epoch == second
        assert store.get_revision.await_count == 2
    finally:
        await engine.stop()
        await queue.stop()


async def test_engine_publishes_error_when_apply_fails() -> None:
    queue = MemoryEventQueue()
    store = MagicMock()
    # A payload that fails to validate as CompanyConfig.
    bad = _revision({"missing_required_name_field": True})
    store.get_revision = AsyncMock(return_value=bad)
    store.get_active = AsyncMock(return_value=None)

    engine = Engine.from_bootstrap(
        _bootstrap(), event_queue=queue, company_config_store=store
    )
    await queue.start()
    applied = await _collect_applied(queue)

    try:
        await engine.start()
        plane = engine._config_plane()
        epoch = await plane.record_activation(bad.revision_id)
        await engine._reconcile_config_once()

        for _ in range(20):
            await asyncio.sleep(0)
            if applied:
                break
        assert applied
        assert applied[0].status == "error"
        assert applied[0].error

        # And the failure is recorded for the fleet, keyed to the epoch
        # that failed — that row is what lets a peer tell "behind because
        # propagation takes a moment" from "behind because it cannot
        # apply this".
        ok, reported = await plane.peer_health(epoch, exclude_node="other")
        assert (ok, reported) == (0, 1)
        assert engine._apply_status is ApplyStatus.ERROR
        assert engine._applied_epoch == 0
    finally:
        await engine.stop()
        await queue.stop()


async def test_a_failing_epoch_is_retried_a_bounded_number_of_times() -> None:
    """Retry must be bounded, and the node must keep serving.

    Two independent things, and only the first is about the bound.
    Without a bound, a revision that fails on one node only (a per-node
    env var, an MCP binary missing from that image) re-applies every
    reconcile tick forever — restarting MCP children each time.

    The posture is the second. This engine is alone: no peer has
    reported, so there is nowhere for its work to go, and the config it
    was already serving is untouched by the failed apply. Reporting
    STUCK here would stop trigger admission and fail readiness over a
    revision that nothing else in the fleet even saw — a single-node
    company taken dark by a bad activation, which is exactly what
    ISOLATED exists to prevent.
    """
    queue = MemoryEventQueue()
    store = MagicMock()
    bad = _revision({"missing_required_name_field": True})
    store.get_revision = AsyncMock(return_value=bad)
    store.get_active = AsyncMock(return_value=None)

    engine = Engine.from_bootstrap(
        _bootstrap(), event_queue=queue, company_config_store=store
    )
    await queue.start()
    try:
        await engine.start()
        plane = engine._config_plane()
        await plane.record_activation(bad.revision_id)

        for _ in range(3):
            await engine._reconcile_config_once()
        assert store.get_revision.await_count == 3

        # Attempts exhausted: stop trying, and say so — while still
        # serving the config this node has.
        assert await engine._reconcile_config_once() is Posture.ISOLATED
        assert store.get_revision.await_count == 3
        assert engine.admits_triggers() is True
    finally:
        await engine.stop()
        await queue.stop()


async def test_activation_event_only_wakes_the_poll() -> None:
    """The event shortens the wait; the pointer decides the work.

    Two halves, both load-bearing.  An event that fires with the pointer
    unmoved applies nothing — so a spurious or replayed nudge cannot
    make a node re-apply.  And an event that fires after the pointer
    moved converges *immediately* rather than up to a poll interval
    later, which is the only reason the nudge exists.
    """
    queue = MemoryEventQueue()
    store, rev = _store_with_revision({"name": "Nudged", "mission": "test"})
    engine = Engine.from_bootstrap(
        _bootstrap(), event_queue=queue, company_config_store=store
    )
    await queue.start()

    async def _publish_activation() -> None:
        await queue.publish(
            "crewlet.config.revision_activated",
            ConfigRevisionActivated(
                source="test",
                revision_id=str(rev.revision_id),
                revision_summary="seed",
                created_by="cli",
            ),
        )
        for _ in range(50):
            await asyncio.sleep(0)

    try:
        await engine.start()

        # Nudge with the pointer unmoved: woken, nothing applied.
        await _publish_activation()
        assert engine._active_config is None
        assert store.get_revision.await_count == 0

        # Pointer moves, then the nudge — converged well inside one
        # poll interval (which is at least 12s).
        plane = engine._config_plane()
        epoch = await plane.record_activation(rev.revision_id)
        await _publish_activation()
        assert engine._applied_epoch == epoch
        assert engine.org.name == "Nudged"
    finally:
        await engine.stop()
        await queue.stop()


async def test_activation_subscription_is_broadcast_not_a_consumer_group() -> None:
    """The nudge must reach EVERY node, so it cannot be a durable group.

    Under ``engine-config`` exactly one process ever saw an activation.
    A regression here is invisible in a single-process test run, so the
    subscription shape is asserted directly.
    """
    queue = MemoryEventQueue()
    store, _rev = _store_with_revision({"name": "Fanout", "mission": "test"})
    engine = Engine.from_bootstrap(
        _bootstrap(), event_queue=queue, company_config_store=store
    )
    groups: list[tuple[str, str]] = []
    original_subscribe = queue.subscribe

    async def _record(topic: str, group: str, handler, **kwargs):
        groups.append((topic, group))
        return await original_subscribe(topic, group, handler, **kwargs)

    queue.subscribe = _record  # type: ignore[assignment]
    await queue.start()
    try:
        await engine.start()
        assert not any(t == "crewlet.config.revision_activated" for t, _ in groups)
        assert engine._nudge_unsubscribe is not None
    finally:
        await engine.stop()
        await queue.stop()


async def test_engine_skips_subscription_without_store() -> None:
    """No CompanyConfigStore → no nudge subscription; no error."""
    queue = MemoryEventQueue()
    engine = Engine.from_bootstrap(
        _bootstrap(), event_queue=queue, company_config_store=None
    )
    try:
        await engine.start()
        assert engine._active_config is None
        assert engine._nudge_unsubscribe is None
        # And a reconcile tick on a store-less engine is a no-op, not a
        # crash — the loop runs on every node regardless.
        assert await engine._reconcile_config_once() is Posture.SERVE
    finally:
        await engine.stop()
        await queue.stop()
