async def test_write_event_is_idempotent_on_event_id() -> None:
    """The queue's deferral/requeue paths REPUBLISH events (same id) and
    rely on event-store idempotency — the PG repository has
    ``ON CONFLICT DO NOTHING``; the memory leg must match, or every
    deferral cycle appends a duplicate row and evicts genuine events
    from the capped ring."""
    from crewlet.timescaledb.memory import MemoryEventStore

    store = MemoryEventStore(max_events=10)
    await store.start()
    for _ in range(3):
        await store.write_event(
            event_id="evt-1", event_type="external_notification", source="t"
        )
    events = await store.list_events()
    assert len(events) == 1

    # Eviction frees the id for a (theoretical) rewrite — the seen-set
    # must not grow unboundedly past the ring.
    for i in range(15):
        await store.write_event(event_id=f"evt-fill-{i}", event_type="x", source="t")
    assert len(await store.list_events(limit=100)) == 10
