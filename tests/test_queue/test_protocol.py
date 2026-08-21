"""Tests for EventQueue using in-memory implementation."""

from __future__ import annotations

import asyncio

import pytest

from crewlet.events.types import Event
from crewlet.queue.memory import MemoryEventQueue

# ---------------------------------------------------------------------------
# EventQueue tests
# ---------------------------------------------------------------------------


class TestMemoryEventQueue:
    """Tests for MemoryEventQueue (EventQueue protocol)."""

    async def test_publish_subscribe(self) -> None:
        """Published events are delivered to subscribers."""
        q = MemoryEventQueue()
        await q.start()
        received: list[Event] = []

        async def handler(event: Event) -> None:
            received.append(event)

        await q.subscribe("topic.a", "grp1", handler)
        await q.start()

        event = Event(type="test_event", source="test")
        await q.publish("topic.a", event)

        # Give dispatcher time to process.
        await asyncio.sleep(0.15)
        await q.stop()

        assert len(received) == 1
        assert received[0].id == event.id
        assert received[0].type == "test_event"

    async def test_pause_topic_buffers_then_resume_flushes(self) -> None:
        """pause_topic holds a topic's delivery (no drop); resume flushes."""
        q = MemoryEventQueue()
        await q.start()
        got_a: list[str] = []
        got_b: list[str] = []

        async def handler_a(event: Event) -> None:
            got_a.append(event.type)

        async def handler_b(event: Event) -> None:
            got_b.append(event.type)

        await q.subscribe("topic.a", "grp", handler_a)
        await q.subscribe("topic.b", "grp", handler_b)

        await q.pause_topic("topic.a", "grp")
        await q.publish("topic.a", Event(type="a1"))
        await q.publish("topic.a", Event(type="a2"))
        # A different topic is unaffected while topic.a is paused.
        await q.publish("topic.b", Event(type="b1"))
        await asyncio.sleep(0.05)
        assert got_a == []  # buffered, not delivered, not dropped
        assert got_b == ["b1"]

        await q.resume_topic("topic.a", "grp")
        await asyncio.sleep(0.05)
        # Flushed in publish order.
        assert got_a == ["a1", "a2"]
        await q.stop()

    async def test_detach_stops_delivery_for_group(self) -> None:
        """detach(topic, group) tears down exactly that pair's consumers."""
        q = MemoryEventQueue()
        await q.start()
        got_a: list[str] = []
        got_other: list[str] = []

        async def handler_a(event: Event) -> None:
            got_a.append(event.type)

        async def handler_other(event: Event) -> None:
            got_other.append(event.type)

        await q.subscribe("topic.a", "grp", handler_a)
        await q.subscribe("topic.a", "other-grp", handler_other)
        await q.publish("topic.a", Event(type="e1"))
        assert got_a == ["e1"] and got_other == ["e1"]

        assert await q.detach("topic.a", "grp") is True
        await q.publish("topic.a", Event(type="e2"))
        # The detached pair stops receiving; the other group is untouched.
        assert got_a == ["e1"]
        assert got_other == ["e1", "e2"]
        # e2 is RETAINED for whoever attaches next — that is the whole
        # point of detach being non-destructive.
        assert [e.type for e in q.backlog("topic.a", "grp")] == ["e2"]

        # Re-attaching replays it: a seat that comes back finds its mail.
        await q.subscribe("topic.a", "grp", handler_a)
        assert got_a == ["e1", "e2"]

        # Idempotent, and returns whether an attachment existed.
        assert await q.detach("topic.a", "grp") is True
        assert await q.detach("topic.a", "grp") is False
        assert await q.detach("nope", "grp") is False
        await q.stop()

    async def test_detach_removes_batch_subscription(self) -> None:
        from crewlet.queue.protocol import BatchOptions

        q = MemoryEventQueue()
        await q.start()
        batches: list[list[Event]] = []

        async def handler(events: list[Event]) -> None:
            batches.append(events)

        await q.subscribe_batch(
            "topic.b",
            "grp",
            handler,
            batch_key=lambda e: "k",
            options=BatchOptions(linger_seconds=0, max_batch=10),
        )
        await q.publish("topic.b", Event(type="e1"))
        assert len(batches) == 1

        await q.detach("topic.b", "grp")
        await q.publish("topic.b", Event(type="e2"))
        assert len(batches) == 1  # no further delivery
        # Retained, not dropped.
        assert [e.type for e in q.backlog("topic.b", "grp")] == ["e2"]
        await q.stop()

    async def test_delete_subscription_discards_retained_mail(self) -> None:
        """The destructive half: what a decommissioned role needs.

        Its inbox must not accumulate undeliverable events forever, and
        deleting must not require a local consumer — role removal cannot
        depend on which node happened to run the seat.
        """
        q = MemoryEventQueue()
        await q.start()
        assert await q.ensure_subscription("topic.gone", "grp") is True
        await q.publish("topic.gone", Event(type="e1"))
        assert len(q.backlog("topic.gone", "grp")) == 1

        assert await q.delete_subscription("topic.gone", "grp") is True
        assert q.backlog("topic.gone", "grp") == []
        # Publishing into a deleted subscription retains nothing.
        await q.publish("topic.gone", Event(type="e2"))
        assert q.backlog("topic.gone", "grp") == []
        # Already gone is the desired end state, not an error.
        assert await q.delete_subscription("topic.gone", "grp") is False
        await q.stop()

    async def test_an_unowned_subscription_holds_its_mail(self) -> None:
        """The property seat ownership rests on.

        A subscription with nothing attached retains what is published
        to it and replays it on attach. Without this, a seat between
        owners loses every event published in the gap.
        """
        q = MemoryEventQueue()
        await q.start()
        assert await q.ensure_subscription("crewlet.agent.alice.inbox", "agent-alice")
        for n in range(3):
            await q.publish("crewlet.agent.alice.inbox", Event(type=f"e{n}"))
        assert len(q.backlog("crewlet.agent.alice.inbox", "agent-alice")) == 3

        got: list[str] = []

        async def handler(event: Event) -> None:
            got.append(event.type)

        await q.subscribe("crewlet.agent.alice.inbox", "agent-alice", handler)
        assert got == ["e0", "e1", "e2"]  # in order
        await q.stop()

    async def test_publishing_to_no_subscription_retains_nothing(self) -> None:
        """Which is exactly why ensure_subscription exists."""
        q = MemoryEventQueue()
        await q.start()
        await q.publish("nobody.listening", Event(type="lost"))
        assert q.backlog("nobody.listening", "grp") == []
        await q.stop()

    async def test_defer_delivery_leaves_the_event_and_stops_consuming(self) -> None:
        """The third handler outcome, and the one a lost seat needs.

        Neither claims the work (ack) nor spends the message's
        dead-letter budget (NAK).
        """
        from crewlet.queue.protocol import DeferDelivery

        q = MemoryEventQueue()
        await q.start()
        seen: list[str] = []

        async def handler(event: Event) -> None:
            seen.append(event.type)
            raise DeferDelivery("lease moved")

        await q.subscribe("topic.d", "grp", handler)
        await q.publish("topic.d", Event(type="e1"))
        await q.publish("topic.d", Event(type="e2"))

        # Handled once, then the subscription stopped taking work.
        assert seen == ["e1"]
        # Both events are still there, in order, with no redelivery
        # accrued against them.
        assert [e.type for e in q.backlog("topic.d", "grp")] == ["e1", "e2"]
        assert q.dead_letters("topic.d", "grp") == []
        await q.stop()

    async def test_a_deferral_stops_the_rest_of_a_batch(self) -> None:
        """The twin must model the broker, not something kinder.

        ``PulsarEventQueue._process_batch`` checks ``sub.quiescing`` at
        the top of EVERY partition, so a seat that lost its lease stops
        dispatching mid-batch. The memory twin's loop had no such check:
        after one partition deferred it went on invoking the handler for
        partitions 2..N on a seat this node had just been told it does
        not own — and each deferral pushed its partition to the FRONT of
        the backlog, so the queue came back in reverse partition order.
        That is the exact reordering ``DeferDelivery`` exists to
        prevent.

        ``tests/test_fleet`` runs against this twin as its "the same
        suite passes on the twin" criterion, so a twin that is more
        forgiving than the broker certifies guarantees production does
        not have.
        """
        from crewlet.queue.protocol import BatchOptions, DeferDelivery

        q = MemoryEventQueue()
        await q.start()
        seen: list[str] = []

        async def handler(events: list[Event]) -> None:
            seen.append(str(events[0].payload["k"]))
            raise DeferDelivery("seat is not owned here")

        await q.subscribe_batch(
            "topic.b",
            "grp",
            handler,
            batch_key=lambda e: str(e.payload["k"]),
            options=BatchOptions(),
        )
        # Paused while publishing so all three land in ONE batch as three
        # partitions. Publishing to a live subscription drains inline per
        # event, and the multi-partition loop this covers never runs.
        await q.pause_topic("topic.b", "grp", reason="test")
        for key in ("a", "b", "c"):
            await q.publish("topic.b", Event(type="x", payload={"k": key}))
        await q.resume_topic("topic.b", "grp", reason="test")

        assert seen == ["a"], (
            f"the deferral did not stop the batch — handled {seen} on a "
            f"seat this node was told it does not own"
        )
        # Everything is still queued, and still in partition order.
        backlog = [str(e.payload["k"]) for e in q.backlog("topic.b", "grp")]
        assert backlog == ["a", "b", "c"], (
            f"the backlog came back as {backlog}; deferring partition by "
            f"partition reversed the conversation"
        )
        await q.stop()

    async def test_a_publish_during_the_batch_does_not_move_the_splice(
        self,
    ) -> None:
        """The restore point is found by identity, not by arithmetic.

        The obvious way to place the undispatched partitions is a length
        delta — how much longer is the deque than before the loop. That
        assumes it only grew at the FRONT, and ``publish`` appends at the
        tail: a handler that awaits lets one land mid-loop, the delta
        over-counts, and the splice lands inside the pre-existing tail,
        reordering the very partitions the guard protects.
        """
        from crewlet.queue.protocol import BatchOptions, DeferDelivery

        q = MemoryEventQueue()
        await q.start()
        seen: list[str] = []

        async def handler(events: list[Event]) -> None:
            seen.append(str(events[0].payload["k"]))
            # A publish lands while this batch is mid-flight, so the
            # deque grows at the TAIL under the loop. Appended directly
            # rather than through ``publish``: publishing to the topic
            # being consumed dispatches inline in this same task, which
            # would recurse and measure something else entirely. What
            # matters here is only that the tail grew.
            q._subs[("topic.c", "grp")].events.append(
                Event(type="x", payload={"k": "late"})
            )
            raise DeferDelivery("seat is not owned here")

        await q.subscribe_batch(
            "topic.c",
            "grp",
            handler,
            batch_key=lambda e: str(e.payload["k"]),
            options=BatchOptions(),
        )
        await q.pause_topic("topic.c", "grp", reason="test")
        for key in ("a", "b", "c"):
            await q.publish("topic.c", Event(type="x", payload={"k": key}))
        await q.resume_topic("topic.c", "grp", reason="test")

        assert seen == ["a"]
        backlog = [str(e.payload["k"]) for e in q.backlog("topic.c", "grp")]
        assert backlog[:3] == ["a", "b", "c"], (
            f"the undispatched partitions were spliced into the wrong place: {backlog}"
        )
        await q.stop()

    async def test_re_attaching_clears_a_stale_quiesce(self) -> None:
        """A quiesce that outlives its consumer strands the seat forever.

        `detach` clears the flag — but an in-flight handler abandoned by
        that detach can still raise `DeferDelivery` afterwards, and
        `_invoke` puts the key straight back. From there nothing is
        deliverable on that `(topic, group)` and there is no consumer
        left to un-quiesce it, so the seat's next owner attaches to a
        subscription that never hands it anything.

        Pulsar quiesces per attachment, so the twin would have been the
        only one to strand it — and `tests/test_fleet` runs against the
        twin.
        """
        q = MemoryEventQueue()
        await q.start()
        seen: list[str] = []

        async def handler(event: Event) -> None:
            seen.append(event.type)

        # The stale flag, exactly as a late deferral leaves it.
        q._quiescing.add(("topic.q", "grp"))
        await q.subscribe("topic.q", "grp", handler)
        await q.publish("topic.q", Event(type="e1"))

        assert seen == ["e1"], (
            "a stale quiesce survived the re-attach, so the new owner "
            "gets nothing and the seat is dark for the whole fleet"
        )
        await q.stop()

    async def test_resume_topic_unpaused_is_noop(self) -> None:
        q = MemoryEventQueue()
        await q.start()
        # Resuming a subscription that was never paused must not error.
        await q.resume_topic("never.paused", "grp")
        await q.stop()

    async def test_consumer_group_competing(self) -> None:
        """Only one subscriber in a consumer group receives each event."""
        q = MemoryEventQueue()
        await q.start()
        counts: dict[str, int] = {"a": 0, "b": 0}

        async def handler_a(event: Event) -> None:
            counts["a"] += 1

        async def handler_b(event: Event) -> None:
            counts["b"] += 1

        await q.subscribe("topic.x", "grp", handler_a)
        await q.subscribe("topic.x", "grp", handler_b)
        await q.start()

        await q.publish("topic.x", Event(type="t"))
        await asyncio.sleep(0.15)
        await q.stop()

        # Exactly one handler should have been called.
        assert counts["a"] + counts["b"] == 1

    async def test_multiple_groups_receive_same_event(self) -> None:
        """Different groups each get a copy of the event."""
        q = MemoryEventQueue()
        await q.start()
        group1: list[Event] = []
        group2: list[Event] = []

        async def h1(event: Event) -> None:
            group1.append(event)

        async def h2(event: Event) -> None:
            group2.append(event)

        await q.start()
        await q.subscribe("topic.y", "g1", h1)
        await q.subscribe("topic.y", "g2", h2)

        await q.publish("topic.y", Event(type="t"))
        await asyncio.sleep(0.15)
        await q.stop()

        assert len(group1) == 1
        assert len(group2) == 1

    async def test_handler_failure_triggers_redelivery(self) -> None:
        """When a handler raises, the event is retried."""
        q = MemoryEventQueue(max_redeliveries=3)
        attempts: list[int] = []
        call_count = 0

        async def flaky_handler(event: Event) -> None:
            nonlocal call_count
            call_count += 1
            attempts.append(call_count)
            if call_count < 3:
                raise RuntimeError("transient failure")

        await q.start()
        await q.subscribe("topic.flaky", "grp", flaky_handler)

        await q.publish("topic.flaky", Event(type="t"))
        await asyncio.sleep(0.15)
        await q.stop()

        # Handler was called 3 times (2 failures + 1 success).
        assert len(attempts) == 3

    async def test_multiple_subscribers_different_topics(self) -> None:
        """Subscribers only receive events for their topic."""
        q = MemoryEventQueue()
        await q.start()
        results_a: list[Event] = []
        results_b: list[Event] = []

        async def ha(event: Event) -> None:
            results_a.append(event)

        async def hb(event: Event) -> None:
            results_b.append(event)

        await q.subscribe("topic.a", "g", ha)
        await q.subscribe("topic.b", "g", hb)
        await q.start()

        await q.publish("topic.a", Event(type="a"))
        await q.publish("topic.b", Event(type="b"))
        await asyncio.sleep(0.15)
        await q.stop()

        assert len(results_a) == 1
        assert results_a[0].type == "a"
        assert len(results_b) == 1
        assert results_b[0].type == "b"

    async def test_start_stop_lifecycle(self) -> None:
        """Start/stop can be called, publish before start raises."""
        q = MemoryEventQueue()

        with pytest.raises(RuntimeError, match="not started"):
            await q.publish("t", Event(type="t"))

        await q.start()
        await q.publish("t", Event(type="t"))  # should not raise
        await q.stop()

        with pytest.raises(RuntimeError, match="not started"):
            await q.publish("t", Event(type="t"))

    async def test_idempotent_start_stop(self) -> None:
        """Calling start/stop multiple times is safe."""
        q = MemoryEventQueue()
        await q.start()
        await q.start()  # idempotent
        await q.stop()
        await q.stop()  # idempotent

    async def test_members_of_a_group_compete(self) -> None:
        """Each event goes to exactly ONE member, and the group shares
        the load round-robin.

        Delivering always to the first-registered member made the
        double-attach split-brain invisible: two nodes consuming one
        seat looked like one, so a test asserting "exactly one delivery"
        passed while a real Shared subscription split the traffic and
        ran two interleaved turn streams.
        """
        q = MemoryEventQueue()
        await q.start()
        called_by: list[str] = []

        async def a(event: Event) -> None:
            called_by.append("a")

        async def b(event: Event) -> None:
            called_by.append("b")

        await q.subscribe("topic", "grp", a)
        await q.subscribe("topic", "grp", b)
        for _ in range(4):
            await q.publish("topic", Event(type="t"))
        await q.stop()

        assert called_by == ["a", "b", "a", "b"]

    async def test_redelivery_rotates_across_members(self) -> None:
        """On failure, redelivery tries the next member in the group."""
        q = MemoryEventQueue(max_redeliveries=3)
        called_by: list[str] = []

        async def always_fail(event: Event) -> None:
            called_by.append("a")
            raise RuntimeError("fail")

        async def succeeds(event: Event) -> None:
            called_by.append("b")

        # Two members in the same group: "a" always fails, "b" succeeds.
        await q.start()
        await q.subscribe("topic", "grp", always_fail)
        await q.subscribe("topic", "grp", succeeds)

        await q.publish("topic", Event(type="t"))
        await asyncio.sleep(0.15)
        await q.stop()

        # First delivery -> member[0] (a, fails); the redelivery advances
        # the round-robin cursor to member[1] (b, ok).
        assert called_by == ["a", "b"]

    async def test_exhausted_redeliveries_dead_letter_the_event(self) -> None:
        """A poison message is PRESERVED, not destroyed.

        ``max_redeliveries`` counts redeliveries *after* the first
        delivery — N+1 total attempts — and the exhausted message moves
        to ``dlq-{topic}-{group}``, exactly as the Pulsar dead-letter
        policy does. Counting total attempts and then dropping made
        every retry-count assertion off by one and every
        "poison message is recoverable" claim false.
        """
        q = MemoryEventQueue(max_redeliveries=2)
        attempts = 0

        async def always_fail(event: Event) -> None:
            nonlocal attempts
            attempts += 1
            raise RuntimeError("permanent failure")

        await q.start()
        await q.subscribe("topic", "grp", always_fail)

        await q.publish("topic", Event(type="t"))
        await asyncio.sleep(0.15)

        assert attempts == 3  # first delivery + 2 redeliveries
        assert q.backlog("topic", "grp") == []
        assert [e.type for e in q.dead_letters("topic", "grp")] == ["t"]
        await q.stop()

    async def test_subscribe_after_start(self) -> None:
        """Subscriptions added after start() still receive events."""
        q = MemoryEventQueue()
        await q.start()
        received: list[Event] = []

        await q.start()

        async def handler(event: Event) -> None:
            received.append(event)

        await q.subscribe("topic.late", "grp", handler)
        await q.publish("topic.late", Event(type="late"))
        await asyncio.sleep(0.15)
        await q.stop()

        assert len(received) == 1
        assert received[0].type == "late"

    async def test_pause_delivery_blocks_dispatch_but_accepts_publish(self) -> None:
        """After pause, publishes return but handlers don't fire.

        This is the graceful-shutdown contract: no new turns start
        (handlers are paused) while in-flight ones complete and emit
        their terminal events (publish still works).
        """
        q = MemoryEventQueue()
        await q.start()
        received: list[Event] = []

        async def handler(event: Event) -> None:
            received.append(event)

        await q.subscribe("topic.p", "grp", handler)

        # Pre-pause publish gets dispatched.
        await q.publish("topic.p", Event(type="before_pause"))
        assert [e.type for e in received] == ["before_pause"]

        await q.pause_delivery()

        # Publishes after pause succeed but are NOT dispatched to handlers.
        # Events still land in history for cross-restart durability semantics.
        await q.publish("topic.p", Event(type="after_pause"))
        await asyncio.sleep(0.05)
        assert [e.type for e in received] == ["before_pause"]
        assert any(e.type == "after_pause" for e in q.history)

        await q.stop()

    async def test_wait_for_handlers_no_op_when_idle(self) -> None:
        """Draining an idle queue returns 0 immediately."""
        q = MemoryEventQueue()
        await q.start()
        remaining = await q.wait_for_handlers(timeout=0.1)
        assert remaining == 0
        await q.stop()

    async def test_pause_is_idempotent(self) -> None:
        """Calling pause_delivery twice is safe."""
        q = MemoryEventQueue()
        await q.start()
        await q.pause_delivery()
        await q.pause_delivery()
        await q.stop()

    async def test_stop_clears_pause(self) -> None:
        """A fresh start() after stop() resumes normal delivery."""
        q = MemoryEventQueue()
        await q.start()
        await q.pause_delivery()
        await q.stop()

        received: list[Event] = []

        async def handler(event: Event) -> None:
            received.append(event)

        await q.start()
        await q.subscribe("topic.r", "grp", handler)
        await q.publish("topic.r", Event(type="resumed"))
        await asyncio.sleep(0.05)
        await q.stop()

        assert [e.type for e in received] == ["resumed"]


class TestMemoryEventQueueStream:
    """Broadcast-style ``subscribe_stream`` covers WebSocket fan-out."""

    async def test_stream_broadcasts_to_every_subscriber(self) -> None:
        q = MemoryEventQueue()
        await q.start()
        received_a: list[tuple[str, Event]] = []
        received_b: list[tuple[str, Event]] = []

        async def on_a(topic: str, event: Event) -> None:
            received_a.append((topic, event))

        async def on_b(topic: str, event: Event) -> None:
            received_b.append((topic, event))

        await q.subscribe_stream("crewlet.events.>", on_a)
        await q.subscribe_stream("crewlet.events.>", on_b)
        await q.publish("crewlet.events.task_created", Event(type="task_created"))
        await q.stop()

        assert len(received_a) == 1
        assert len(received_b) == 1
        assert received_a[0][0] == "crewlet.events.task_created"
        assert received_a[0][1].type == "task_created"

    async def test_stream_filters_by_topic_pattern(self) -> None:
        q = MemoryEventQueue()
        await q.start()
        received: list[str] = []

        async def on_event(topic: str, _event: Event) -> None:
            received.append(topic)

        await q.subscribe_stream("crewlet.events.>", on_event)
        await q.publish("crewlet.events.task_created", Event(type="task_created"))
        await q.publish("crewlet.notifications.inbound", Event(type="raw_webhook"))
        await q.stop()

        assert received == ["crewlet.events.task_created"]

    async def test_stream_unsubscribe_stops_dispatch(self) -> None:
        q = MemoryEventQueue()
        await q.start()
        calls: list[str] = []

        async def on_event(topic: str, _event: Event) -> None:
            calls.append(topic)

        unsubscribe = await q.subscribe_stream("crewlet.events.>", on_event)
        await q.publish("crewlet.events.task_created", Event(type="task_created"))
        await unsubscribe()
        await q.publish("crewlet.events.task_failed", Event(type="task_failed"))
        await q.stop()

        assert calls == ["crewlet.events.task_created"]

    async def test_stream_handler_failure_does_not_break_publish(self) -> None:
        q = MemoryEventQueue()
        await q.start()
        delivered: list[Event] = []

        async def boom(_topic: str, _event: Event) -> None:
            raise RuntimeError("stream client crashed")

        async def healthy(_topic: str, event: Event) -> None:
            delivered.append(event)

        await q.subscribe_stream("crewlet.events.>", boom)
        await q.subscribe_stream("crewlet.events.>", healthy)
        await q.publish("crewlet.events.task_created", Event(type="task_created"))
        await q.stop()

        # The healthy subscriber still received the event and the
        # publish call itself completed without raising.
        assert len(delivered) == 1


class TestMemoryEventQueueBatch:
    """Batched, key-partitioned delivery (``subscribe_batch``)."""

    @staticmethod
    def _key(event: Event) -> str:
        return event.payload.get("conv", f"event:{event.id}")

    async def test_zero_linger_dispatches_inline_single_event_batches(self) -> None:
        """``linger_seconds=0`` keeps publishes synchronous: each event
        arrives as its own one-element batch, deterministically, before
        ``publish`` returns -- existing engine tests rely on this."""
        from crewlet.queue.protocol import BatchOptions

        q = MemoryEventQueue()
        await q.start()
        batches: list[list[Event]] = []

        async def handler(events: list[Event]) -> None:
            batches.append(events)

        await q.subscribe_batch(
            "t", "g", handler, batch_key=self._key, options=BatchOptions()
        )
        await q.publish("t", Event(type="a", payload={"conv": "c1"}))
        await q.publish("t", Event(type="b", payload={"conv": "c1"}))
        await q.stop()

        assert [len(b) for b in batches] == [1, 1]

    async def test_linger_coalesces_same_key_events_into_one_batch(self) -> None:
        from crewlet.queue.protocol import BatchOptions

        q = MemoryEventQueue()
        await q.start()
        batches: list[list[Event]] = []

        async def handler(events: list[Event]) -> None:
            batches.append(events)

        await q.subscribe_batch(
            "t",
            "g",
            handler,
            batch_key=self._key,
            options=BatchOptions(linger_seconds=0.05),
        )
        e1 = Event(type="a", payload={"conv": "c1"})
        e2 = Event(type="b", payload={"conv": "c1"})
        await q.publish("t", e1)
        await q.publish("t", e2)
        await asyncio.sleep(0.15)
        await q.stop()

        assert len(batches) == 1
        assert [e.id for e in batches[0]] == [e1.id, e2.id]

    async def test_linger_partitions_by_key_preserving_arrival_order(self) -> None:
        from crewlet.queue.protocol import BatchOptions

        q = MemoryEventQueue()
        await q.start()
        batches: list[list[Event]] = []

        async def handler(events: list[Event]) -> None:
            batches.append(events)

        await q.subscribe_batch(
            "t",
            "g",
            handler,
            batch_key=self._key,
            options=BatchOptions(linger_seconds=0.05),
        )
        await q.publish("t", Event(type="a", payload={"conv": "c1"}))
        await q.publish("t", Event(type="b", payload={"conv": "c2"}))
        await q.publish("t", Event(type="c", payload={"conv": "c1"}))
        await asyncio.sleep(0.15)
        await q.stop()

        # Two partitions, dispatched oldest-constituent-first (here the
        # same as first-arrival — auto-now timestamps follow publish
        # order); the c1 partition keeps both its events in publish
        # order.
        assert [[e.type for e in b] for b in batches] == [["a", "c"], ["b"]]

    async def test_failing_partition_redelivers_only_itself(self) -> None:
        """A handler failure on one conversation must not replay or
        block the other conversation from the same flush."""
        from crewlet.queue.protocol import BatchOptions

        q = MemoryEventQueue(max_redeliveries=2)
        await q.start()
        attempts: dict[str, int] = {"c1": 0, "c2": 0}

        async def handler(events: list[Event]) -> None:
            conv = events[0].payload["conv"]
            attempts[conv] += 1
            if conv == "c1":
                raise RuntimeError("boom")

        await q.subscribe_batch(
            "t",
            "g",
            handler,
            batch_key=self._key,
            options=BatchOptions(linger_seconds=0.05),
        )
        await q.publish("t", Event(type="a", payload={"conv": "c1"}))
        await q.publish("t", Event(type="b", payload={"conv": "c2"}))
        await asyncio.sleep(0.2)
        await q.stop()

        assert attempts["c1"] == 3  # first delivery + 2 redeliveries
        assert attempts["c2"] == 1  # unaffected by c1's failure
        assert len(q.dead_letters("t", "g")) == 1

    async def test_max_batch_chunks_oversized_buffers(self) -> None:
        from crewlet.queue.protocol import BatchOptions

        q = MemoryEventQueue()
        await q.start()
        batches: list[list[Event]] = []

        async def handler(events: list[Event]) -> None:
            batches.append(events)

        await q.subscribe_batch(
            "t",
            "g",
            handler,
            batch_key=self._key,
            options=BatchOptions(linger_seconds=0.05, max_batch=2),
        )
        for i in range(5):
            await q.publish("t", Event(type=f"e{i}", payload={"conv": "c1"}))
        await asyncio.sleep(0.2)
        await q.stop()

        assert [len(b) for b in batches] == [2, 2, 1]

    async def test_batch_key_failure_falls_back_to_unique_key(self) -> None:
        """Key derivation must never block delivery -- a raising
        ``batch_key`` degrades to per-event partitions."""
        from crewlet.queue.protocol import BatchOptions

        q = MemoryEventQueue()
        await q.start()
        batches: list[list[Event]] = []

        def bad_key(_event: Event) -> str:
            raise ValueError("no key for you")

        async def handler(events: list[Event]) -> None:
            batches.append(events)

        await q.subscribe_batch(
            "t",
            "g",
            handler,
            batch_key=bad_key,
            options=BatchOptions(linger_seconds=0.05),
        )
        await q.publish("t", Event(type="a"))
        await q.publish("t", Event(type="b"))
        await asyncio.sleep(0.15)
        await q.stop()

        assert [len(b) for b in batches] == [1, 1]

    async def test_live_options_mutation_takes_effect_next_cycle(self) -> None:
        """Mutating the shared ``BatchOptions`` (live config reload)
        changes behaviour without re-subscribing."""
        from crewlet.queue.protocol import BatchOptions

        q = MemoryEventQueue()
        await q.start()
        batches: list[list[Event]] = []

        async def handler(events: list[Event]) -> None:
            batches.append(events)

        options = BatchOptions(linger_seconds=0.0)
        await q.subscribe_batch("t", "g", handler, batch_key=self._key, options=options)
        await q.publish("t", Event(type="a", payload={"conv": "c1"}))
        assert [len(b) for b in batches] == [1]  # inline while linger=0

        options.linger_seconds = 0.05
        await q.publish("t", Event(type="b", payload={"conv": "c1"}))
        await q.publish("t", Event(type="c", payload={"conv": "c1"}))
        await asyncio.sleep(0.15)
        await q.stop()

        assert [len(b) for b in batches] == [1, 2]

    async def test_stop_cancels_pending_lingered_flush(self) -> None:
        from crewlet.queue.protocol import BatchOptions

        q = MemoryEventQueue()
        await q.start()
        batches: list[list[Event]] = []

        async def handler(events: list[Event]) -> None:
            batches.append(events)

        await q.subscribe_batch(
            "t",
            "g",
            handler,
            batch_key=self._key,
            options=BatchOptions(linger_seconds=5.0),
        )
        await q.publish("t", Event(type="a", payload={"conv": "c1"}))
        await q.stop()
        await asyncio.sleep(0.05)

        assert batches == []
        # RETAINED, not dropped: the linger buffer IS the backlog, and a
        # subscription's mail outlives every attachment.
        assert [e.type for e in q.backlog("t", "g")] == ["a"]

    async def test_pause_during_linger_retains_pending(self) -> None:
        """Lingered events caught by ``pause_delivery`` are RETAINED.

        The Pulsar backend leaves them on the broker for the next
        engine subscription; the twin leaves them in the backlog, which
        is the same statement. Dropping them meant a graceful drain
        silently destroyed whatever was mid-window."""
        from crewlet.queue.protocol import BatchOptions

        q = MemoryEventQueue()
        await q.start()
        batches: list[list[Event]] = []

        async def handler(events: list[Event]) -> None:
            batches.append(events)

        await q.subscribe_batch(
            "t",
            "g",
            handler,
            batch_key=self._key,
            options=BatchOptions(linger_seconds=0.05),
        )
        await q.publish("t", Event(type="a", payload={"conv": "c1"}))
        await q.pause_delivery()
        await asyncio.sleep(0.15)

        assert batches == []
        assert [e.type for e in q.backlog("t", "g")] == ["a"]
        await q.stop()


class TestBatchOptionsAndOrdering:
    def test_effective_linger_clamps_to_ceiling(self) -> None:
        """The 60s ceiling is enforced where the value is consumed —
        programmatic Engine construction bypasses config validation, and
        an unbounded linger would re-open the ack-window overlap the
        dispatch budget exists to close."""
        from crewlet.queue.protocol import MAX_LINGER_SECONDS, BatchOptions

        assert BatchOptions(linger_seconds=300.0).effective_linger_seconds == (
            MAX_LINGER_SECONDS
        )
        assert BatchOptions(linger_seconds=-5).effective_linger_seconds == 0.0
        assert BatchOptions(linger_seconds=10).effective_linger_seconds == 10.0

    def test_order_partitions_oldest_first(self) -> None:
        """Dispatch order follows the oldest constituent event — a
        deferred (older) conversation wins priority over fresh arrivals,
        so steady inflow on a hot conversation cannot starve it."""
        from datetime import UTC, datetime, timedelta

        from crewlet.queue.protocol import (
            order_partitions_oldest_first,
            partition_by_key,
        )

        t0 = datetime(2026, 6, 10, 12, 0, 0, tzinfo=UTC)
        hot_new = Event(
            type="a", payload={"conv": "hot"}, timestamp=t0 + timedelta(seconds=60)
        )
        quiet_old = Event(type="b", payload={"conv": "quiet"}, timestamp=t0)
        # Receive order: hot first (it sits earlier in the topic).
        parts = partition_by_key([hot_new, quiet_old], lambda e: e.payload["conv"])
        ordered = order_partitions_oldest_first(parts)
        assert [key for key, _ in ordered] == ["quiet", "hot"]

    def test_order_partitions_tie_keeps_arrival_order(self) -> None:
        from datetime import UTC, datetime

        from crewlet.queue.protocol import (
            order_partitions_oldest_first,
            partition_by_key,
        )

        t0 = datetime(2026, 6, 10, 12, 0, 0, tzinfo=UTC)
        first = Event(type="a", payload={"conv": "c1"}, timestamp=t0)
        second = Event(type="b", payload={"conv": "c2"}, timestamp=t0)
        ordered = order_partitions_oldest_first(
            partition_by_key([first, second], lambda e: e.payload["conv"])
        )
        assert [key for key, _ in ordered] == ["c1", "c2"]

    def test_order_partitions_falls_back_on_incomparable_timestamps(self) -> None:
        """Ordering is a fairness policy — incomparable timestamps (a
        malformed naive datetime) degrade to arrival order rather than
        blocking delivery."""
        from datetime import UTC, datetime

        from crewlet.queue.protocol import (
            order_partitions_oldest_first,
            partition_by_key,
        )

        aware = Event(
            type="a",
            payload={"conv": "c1"},
            timestamp=datetime(2026, 6, 10, 12, 0, 0, tzinfo=UTC),
        )
        naive = Event(
            type="b",
            payload={"conv": "c2"},
            timestamp=datetime(2026, 6, 10, 12, 0, 0),
        )
        parts = partition_by_key([aware, naive], lambda e: e.payload["conv"])
        assert order_partitions_oldest_first(parts) == parts


class TestMemoryEventQueueFleet:
    """One broker, several clients — the twin's model of a fleet.

    A single :class:`MemoryEventQueue` is both a broker and a client, and
    for one process that is invisible. For two it inverts the property
    seat ownership rests on, so these assert the seam: subscriptions and
    mail are shared, everything about an *attachment* is not.
    """

    async def test_a_clients_detach_leaves_its_peers_attached(self) -> None:
        broker = MemoryEventQueue()
        a, b = broker.client(), broker.client()
        await a.start()
        await b.start()
        got_a: list[str] = []
        got_b: list[str] = []

        async def handler_a(event: Event) -> None:
            got_a.append(event.type)

        async def handler_b(event: Event) -> None:
            got_b.append(event.type)

        await a.subscribe("seat.inbox", "grp", handler_a)
        await b.subscribe("seat.inbox", "grp", handler_b)
        assert a.attachments() == [("seat.inbox", "grp")]
        assert b.attachments() == [("seat.inbox", "grp")]

        await a.detach("seat.inbox", "grp")
        assert a.attachments() == []
        assert b.attachments() == [("seat.inbox", "grp")], (
            "one node's detach tore down its peer's consumer"
        )

        await a.publish("seat.inbox", Event(type="after", source="t"))
        assert got_b == ["after"] and got_a == []

    async def test_a_clients_pause_does_not_gate_its_peers(self) -> None:
        """A hold describes one node's attachment.

        Gating the subscription instead would let one node's sandbox
        pause — or one node's shutdown — stop a peer from serving the
        seat it owns.
        """
        broker = MemoryEventQueue()
        a, b = broker.client(), broker.client()
        await a.start()
        await b.start()
        got: list[str] = []

        async def handler(event: Event) -> None:
            got.append(event.type)

        await a.subscribe("seat.inbox", "grp", lambda e: _never())
        await b.subscribe("seat.inbox", "grp", handler)
        await a.pause_topic("seat.inbox", "grp", reason="sandbox")

        assert a.pause_holds("seat.inbox", "grp") == {"sandbox"}
        assert b.pause_holds("seat.inbox", "grp") == set()

        for _ in range(4):
            await b.publish("seat.inbox", Event(type="work", source="t"))
        assert got == ["work"] * 4, "a peer's hold stole the whole round robin"

    async def test_stopping_one_client_leaves_the_broker_and_its_peers(self) -> None:
        broker = MemoryEventQueue()
        a, b = broker.client(), broker.client()
        await a.start()
        await b.start()
        got: list[str] = []

        async def handler(event: Event) -> None:
            got.append(event.type)

        await b.subscribe("seat.inbox", "grp", handler)
        await a.stop()

        await b.publish("seat.inbox", Event(type="still-serving", source="t"))
        assert got == ["still-serving"]

    async def test_unquiesce_resumes_and_delivers_what_was_held(self) -> None:
        """Quiesce is not always followed by a detach.

        A node whose lease store blipped keeps its seats and its
        consumers; only the proof of ownership lapsed. Without an
        inverse it would come back healthy and never read from them
        again.
        """
        q = MemoryEventQueue()
        await q.start()
        got: list[str] = []

        async def handler(event: Event) -> None:
            got.append(event.type)

        await q.subscribe("seat.inbox", "grp", handler)
        assert await q.quiesce("seat.inbox", "grp") is True

        await q.publish("seat.inbox", Event(type="held", source="t"))
        assert got == []
        assert [e.type for e in q.backlog("seat.inbox", "grp")] == ["held"]

        assert await q.unquiesce("seat.inbox", "grp") is True
        assert got == ["held"]
        assert await q.unquiesce("seat.inbox", "grp") is False

    async def test_unquiesce_leaves_pause_holds_alone(self) -> None:
        """A seat resuming from a stale-renew window may still be paused
        for a running sandbox; lifting that would deliver into a
        suspended turn."""
        q = MemoryEventQueue()
        await q.start()
        got: list[str] = []

        async def handler(event: Event) -> None:
            got.append(event.type)

        await q.subscribe("seat.inbox", "grp", handler)
        await q.pause_topic("seat.inbox", "grp", reason="sandbox")
        await q.quiesce("seat.inbox", "grp")

        await q.publish("seat.inbox", Event(type="held", source="t"))
        await q.unquiesce("seat.inbox", "grp")

        assert q.pause_holds("seat.inbox", "grp") == {"sandbox"}
        assert got == []


async def _never() -> None:
    """A handler that should never be reached in the pause test."""
    raise AssertionError("delivered to a paused attachment")
