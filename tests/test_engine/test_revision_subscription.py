"""End-to-end test for the engine's Pulsar subscription to
``crewlet.config.revision_activated``.

Wires a real :class:`MemoryEventQueue`, a stubbed
:class:`CompanyConfigStore`, and an unconfigured engine, then
publishes a ``ConfigRevisionActivated`` event and asserts the engine
fetches the payload, runs ``apply_config``, and publishes
``ConfigRevisionApplied(status=ok)``.
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


def _store_with_revision(payload: dict) -> MagicMock:
    rev_id = uuid4()
    rev = CompanyConfigRevision(
        revision_id=rev_id,
        parent_revision_id=None,
        created_at=datetime.now(UTC),
        created_by="cli",
        source="cli",
        summary="seed",
        payload=payload,
        is_active=True,
        activated_at=datetime.now(UTC),
    )
    s = MagicMock()
    s.get_revision = AsyncMock(return_value=rev)
    s.get_active = AsyncMock(return_value=rev)
    return s, rev_id


async def test_engine_applies_revision_published_over_pulsar() -> None:
    queue = MemoryEventQueue()
    store, rev_id = _store_with_revision({"name": "Acme via Pulsar", "mission": "test"})

    engine = Engine.from_bootstrap(
        _bootstrap(),
        event_queue=queue,
        company_config_store=store,
    )

    # Capture revision_applied events for assertion.
    applied: list[ConfigRevisionApplied] = []

    async def _capture(event: ConfigRevisionApplied) -> None:
        applied.append(event)

    await queue.start()
    await queue.subscribe(
        topic="crewlet.config.revision_applied",
        group="test-collector",
        handler=_capture,
    )

    try:
        await engine.start()
        # Engine is in unconfigured state — _active_config is None.
        assert engine._active_config is None

        # Publish the activated event the API would have published.
        await queue.publish(
            "crewlet.config.revision_activated",
            ConfigRevisionActivated(
                source="test",
                revision_id=str(rev_id),
                revision_summary="seed",
                created_by="cli",
            ),
        )

        # Memory queue dispatches synchronously inside an asyncio.Task;
        # yield to the loop until the handler runs.
        for _ in range(20):
            await asyncio.sleep(0)
            if engine._active_config is not None:
                break

        assert engine._active_config is not None
        assert engine._active_config.name == "Acme via Pulsar"
        assert engine.org.name == "Acme via Pulsar"

        # And the engine published ConfigRevisionApplied(ok).
        for _ in range(20):
            await asyncio.sleep(0)
            if applied:
                break
        assert any(e.status == "ok" and e.revision_id == str(rev_id) for e in applied)
    finally:
        await engine.stop()
        await queue.stop()


async def test_engine_publishes_error_when_apply_fails() -> None:
    queue = MemoryEventQueue()
    store = MagicMock()
    # get_revision returns a payload that fails to validate as CompanyConfig.
    store.get_revision = AsyncMock(
        return_value=CompanyConfigRevision(
            revision_id=uuid4(),
            parent_revision_id=None,
            created_at=datetime.now(UTC),
            created_by="x",
            source="x",
            summary="x",
            payload={"missing_required_name_field": True},
            is_active=True,
            activated_at=datetime.now(UTC),
        )
    )

    engine = Engine.from_bootstrap(
        _bootstrap(),
        event_queue=queue,
        company_config_store=store,
    )

    applied: list[ConfigRevisionApplied] = []

    async def _capture(event: ConfigRevisionApplied) -> None:
        applied.append(event)

    await queue.start()
    await queue.subscribe(
        topic="crewlet.config.revision_applied",
        group="test-collector",
        handler=_capture,
    )

    try:
        await engine.start()
        await queue.publish(
            "crewlet.config.revision_activated",
            ConfigRevisionActivated(
                source="test",
                revision_id=str(uuid4()),
                revision_summary="bad",
                created_by="x",
            ),
        )

        for _ in range(20):
            await asyncio.sleep(0)
            if applied:
                break
        assert applied
        assert applied[0].status == "error"
        assert applied[0].error
    finally:
        await engine.stop()
        await queue.stop()


async def test_engine_skips_subscription_without_store() -> None:
    """No CompanyConfigStore → subscription is skipped; no error."""
    queue = MemoryEventQueue()
    engine = Engine.from_bootstrap(
        _bootstrap(),
        event_queue=queue,
        company_config_store=None,
    )
    try:
        await engine.start()
        # Successful start with no subscription is the assertion.
        assert engine._active_config is None
    finally:
        await engine.stop()
        await queue.stop()
