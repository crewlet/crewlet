"""Tests for the dashboard live-stream service + WebSocket endpoint."""

from __future__ import annotations

import asyncio
import json
from collections.abc import Awaitable, Callable

import pytest
from starlette.testclient import TestClient

from crewlet.api.app import create_app
from crewlet.api.streaming import StreamService, serialize_event
from crewlet.events.types import (
    AgentPhaseCompleted,
    AgentPhaseStarted,
    AgentTurnProgress,
    Event,
    TaskCompleted,
    TaskStarted,
)
from crewlet.timescaledb.memory import MemoryEventStore


class _MockQueue:
    def __init__(self) -> None:
        self.published: list[tuple[str, Event]] = []

    async def publish(self, topic: str, event: Event) -> None:
        self.published.append((topic, event))

    async def subscribe(
        self, topic: str, group: str, handler: Callable[[Event], Awaitable[None]]
    ) -> None:
        pass

    async def subscribe_stream(
        self,
        topic_pattern: str,
        handler: Callable[[str, Event], Awaitable[None]],
    ) -> Callable[[], Awaitable[None]]:
        async def _noop_unsubscribe() -> None:
            return None

        return _noop_unsubscribe

    def add_publish_listener(
        self, listener: Callable[[str, Event], Awaitable[None]]
    ) -> None:
        pass

    async def start(self) -> None:
        pass

    async def stop(self) -> None:
        pass


AGENT_ROLES = [
    {"id": "a-1", "name": "Lead", "role": "Lead", "goal": "Lead", "handle": "lead"},
    {"id": "a-2", "name": "Dev", "role": "Dev", "goal": "Code", "handle": "dev"},
]


@pytest.fixture
def store() -> MemoryEventStore:
    store = MemoryEventStore()
    loop = asyncio.new_event_loop()
    loop.run_until_complete(store.start())
    loop.close()
    return store


@pytest.fixture
def stream() -> StreamService:
    return StreamService()


@pytest.fixture
def app(store: MemoryEventStore, stream: StreamService):
    return create_app(
        event_queue=_MockQueue(),
        event_store=store,
        agent_roles=AGENT_ROLES,
        org_data={"name": "Acme"},
        tools_data=[{"name": "t1", "description": "tool", "source": "builtin"}],
        stream=stream,
    )


# ---------------------------------------------------------------------------
# serialize_event
# ---------------------------------------------------------------------------


class TestSerializeEvent:
    def test_includes_category_and_payload(self) -> None:
        evt = AgentPhaseCompleted(
            agent_id="a1", role="Lead", phase="plan", model="gpt-4o", total_tokens=42
        )
        env = serialize_event("crewlet.events.agent_phase_completed", evt)
        assert env["type"] == "agent_phase_completed"
        assert env["category"] == "system"
        assert "Lead plan" in env["summary"]
        assert env["payload"]["phase"] == "plan"
        assert env["payload"]["total_tokens"] == 42
        assert env["topic"] == "crewlet.events.agent_phase_completed"

    def test_task_routing_keys_present(self) -> None:
        env = serialize_event(
            "crewlet.events.task_started", TaskStarted(task_id="t1", agent_id="a1")
        )
        assert env["payload"]["agent_id"] == "a1"
        assert env["payload"]["task_id"] == "t1"
        assert env["category"] == "task"


# ---------------------------------------------------------------------------
# StreamService unit tests
# ---------------------------------------------------------------------------


class TestStreamService:
    async def test_register_and_fan_out(self, stream: StreamService) -> None:
        a = stream.register()
        b = stream.register()
        assert stream.client_count == 2

        await stream.ingest(
            "crewlet.events.task_started",
            TaskStarted(task_id="t1", agent_id="a1", source="engine"),
        )

        msg_a = json.loads(a.queue.get_nowait())
        msg_b = json.loads(b.queue.get_nowait())
        assert msg_a["kind"] == "event"
        assert msg_a["data"]["type"] == "task_started"
        assert msg_b["data"]["type"] == "task_started"

    async def test_unregister_removes_client(self, stream: StreamService) -> None:
        client = stream.register()
        stream.unregister(client)
        assert stream.client_count == 0
        await stream.ingest("crewlet.events.task_started", TaskStarted(task_id="t1"))
        assert client.queue.empty()

    async def test_slow_client_drops_oldest(self) -> None:
        stream = StreamService(client_queue_size=2)
        client = stream.register()
        for i in range(5):
            await stream.ingest(
                "crewlet.events.task_started", TaskStarted(task_id=f"t{i}")
            )
        assert client.queue.qsize() == 2
        assert client.drops == 3

    async def test_ingest_filters_non_event_topics(self, stream: StreamService) -> None:
        """Internal inbox / notification topics never reach dashboards."""
        client = stream.register()
        await stream.ingest(
            "crewlet.notifications.inbound", Event(type="raw_webhook", source="jira")
        )
        assert client.queue.empty()
        await stream.ingest(
            "crewlet.events.task_started", TaskStarted(task_id="t1", source="engine")
        )
        assert client.queue.qsize() == 1

    async def test_ingest_updates_projection_without_clients(
        self, stream: StreamService
    ) -> None:
        """The projection stays current even when nobody is connected, so
        the next snapshot reflects the latest state."""
        await stream.ingest(
            "crewlet.events.task_started",
            TaskStarted(task_id="t1", agent_id="rt-1", source="Lead"),
        )
        # TaskStarted carries ``agent_id`` but no ``role``; state still
        # applies for events that carry a role (phase events do).
        await stream.ingest(
            "crewlet.events.agent_phase_started",
            AgentPhaseStarted(agent_id="rt-1", role="Lead", phase="plan"),
        )
        overlay = stream.live.agent_overlay("Lead")
        assert overlay is not None
        assert overlay["state"] == "working"
        assert overlay["current_phase"] == "plan"


# ---------------------------------------------------------------------------
# Snapshot route + REST
# ---------------------------------------------------------------------------


class TestStreamSnapshot:
    def test_snapshot_returns_agents_events_tools_org(self, app) -> None:
        with TestClient(app) as client:
            body = client.get("/stream/snapshot").json()
            assert {
                "health",
                "agents",
                "events",
                "sandboxes",
                "tools",
                "org",
            } <= body.keys()
            assert body["sandboxes"] == []
            assert body["org"] == {"name": "Acme"}
            assert len(body["agents"]) == 2
            assert body["tools"][0]["name"] == "t1"

    def test_snapshot_health_includes_runtime_metrics(self, store) -> None:
        class _Eng:
            is_running = True
            shutting_down = False
            in_flight_count = 3

        app = create_app(
            event_queue=_MockQueue(),
            event_store=store,
            agent_roles=AGENT_ROLES,
            engine=_Eng(),
        )
        with TestClient(app) as client:
            body = client.get("/stream/snapshot").json()
            assert body["health"]["in_flight"] == 3
            assert body["health"]["shutting_down"] is False

    def test_snapshot_carries_in_flight_live_call(
        self, app, stream: StreamService
    ) -> None:
        """An in-flight LLM call must be
        present in the snapshot so a reconnecting/refreshing tab
        re-renders the live row instead of losing it.

        ``agent_turn_progress`` is stream-only (never persisted), so the
        only way a fresh snapshot can carry it is via the in-memory
        projection.
        """
        stream.live.apply_event(
            serialize_event(
                "crewlet.events.agent_phase_started",
                AgentPhaseStarted(
                    agent_id="rt-1",
                    role="Lead",
                    turn_id="tn-1",
                    iteration=0,
                    phase="execute",
                ),
            )
        )
        stream.live.apply_event(
            serialize_event(
                "crewlet.events.agent_turn_progress",
                AgentTurnProgress(
                    agent_id="rt-1",
                    role="Lead",
                    turn_id="tn-1",
                    phase="execute",
                    iteration=0,
                    model="claude-sonnet-4-6",
                    response="streaming answer…",
                    round_num=1,
                ),
            )
        )
        with TestClient(app) as client:
            body = client.get("/stream/snapshot").json()
            lead = next(a for a in body["agents"] if a["role"] == "Lead")
            assert lead["state"] == "working"
            assert lead["current_phase"] == "execute"
            assert lead["live_call"] is not None
            assert lead["live_call"]["response"] == "streaming answer…"
            assert lead["live_call"]["rounds"] == 2

        # Once the phase completes the live row is replaced by history.
        stream.live.apply_event(
            serialize_event(
                "crewlet.events.agent_phase_completed",
                AgentPhaseCompleted(
                    agent_id="rt-1",
                    role="Lead",
                    turn_id="tn-1",
                    iteration=0,
                    phase="execute",
                ),
            )
        )
        with TestClient(app) as client:
            body = client.get("/stream/snapshot").json()
            lead = next(a for a in body["agents"] if a["role"] == "Lead")
            assert lead["live_call"] is None


# ---------------------------------------------------------------------------
# WebSocket
# ---------------------------------------------------------------------------


class TestStreamWebSocket:
    def test_handshake_pushes_snapshot(self, app) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            env = json.loads(ws.receive_text())
            assert env["kind"] == "snapshot"
            assert env["data"]["org"] == {"name": "Acme"}
            assert len(env["data"]["agents"]) == 2

    def test_client_registered_before_snapshot_is_built(
        self, app, stream: StreamService
    ) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()  # snapshot envelope
            assert stream.client_count == 1

    def test_event_broadcast_reaches_client(self, app, stream: StreamService) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()  # drain snapshot
            client.portal.call(
                stream.ingest,
                "crewlet.events.task_started",
                TaskStarted(task_id="t-77", agent_id="a-1", source="engine"),
            )
            env = json.loads(ws.receive_text())
            assert env["kind"] == "event"
            assert env["data"]["type"] == "task_started"
            assert env["data"]["payload"]["task_id"] == "t-77"

    def test_ping_returns_pong(self, app) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()  # snapshot
            ws.send_text(json.dumps({"kind": "ping"}))
            env = json.loads(ws.receive_text())
            assert env["kind"] == "pong"

    def test_works_with_default_stream(self, store) -> None:
        """create_app always wires a stream, so the WS handshake always
        serves a snapshot even when the caller passes none explicitly."""
        app = create_app(
            event_queue=_MockQueue(),
            event_store=store,
            agent_roles=AGENT_ROLES,
        )
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            env = json.loads(ws.receive_text())
            assert env["kind"] == "snapshot"


# ---------------------------------------------------------------------------
# Webhook surfacing
# ---------------------------------------------------------------------------


class TestWebhookBroadcast:
    def test_jira_webhook_writes_to_store(self, app, store: MemoryEventStore) -> None:
        with TestClient(app) as client:
            response = client.post(
                "/webhooks/jira",
                json={
                    "webhookEvent": "jira:issue_created",
                    "issue": {"key": "ENG-1", "fields": {"summary": "Bug"}},
                    "user": {"displayName": "Alice"},
                },
            )
            assert response.status_code == 200
        events = [e for e in store._events if e["category"] == "webhook"]
        assert len(events) == 1
        assert events[0]["type"] == "webhook:jira:issue_created"

    def test_log_event_broadcasts_and_buffers(self, app, stream: StreamService) -> None:
        """``_log_event`` fans the webhook envelope out to live clients
        AND records it in the projection's activity buffer."""
        from crewlet.api.routes import _log_event

        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()  # snapshot

            class _FakeRequest:
                def __init__(self, app):
                    self.app = app

            ws.portal.call(
                _log_event,
                _FakeRequest(app),
                "webhook:jira:issue_created",
                "jira",
                {"key": "ENG-1"},
                "Jira issue_created ENG-1",
            )
            env = json.loads(ws.receive_text())
            assert env["kind"] == "event"
            assert env["data"]["category"] == "webhook"
            assert env["data"]["type"] == "webhook:jira:issue_created"
        # The webhook also lands in the activity buffer.
        assert any(
            e["type"] == "webhook:jira:issue_created"
            for e in stream.live.recent_events()
        )


# ---------------------------------------------------------------------------
# Wiring: standalone-API path uses subscribe_stream → stream.ingest
# ---------------------------------------------------------------------------


class TestSubscribeStreamWiring:
    async def test_subscribe_stream_into_service(self) -> None:
        from crewlet.queue.memory import MemoryEventQueue

        queue = MemoryEventQueue()
        await queue.start()
        stream = StreamService()
        client = stream.register()

        unsubscribe = await queue.subscribe_stream("crewlet.events.>", stream.ingest)
        try:
            await queue.publish(
                "crewlet.events.task_started",
                TaskStarted(task_id="t-1", agent_id="a-1"),
            )
            env = json.loads(client.queue.get_nowait())
            assert env["kind"] == "event"
            assert env["data"]["payload"]["task_id"] == "t-1"
            await queue.publish(
                "crewlet.notifications.inbound", Event(type="raw_webhook")
            )
            assert client.queue.empty()
        finally:
            await unsubscribe()
            await queue.stop()


# ---------------------------------------------------------------------------
# Agent-state routing-key sanity (events must carry what the projection indexes)
# ---------------------------------------------------------------------------


class TestAgentStateEventsCarryRoutingKeys:
    def test_task_completed_serialization(self) -> None:
        env = serialize_event(
            "crewlet.events.task_completed",
            TaskCompleted(task_id="t1", agent_id="a1", result="done"),
        )
        assert env["payload"]["agent_id"] == "a1"
        assert env["category"] == "task"

    def test_agent_phase_started_serialization(self) -> None:
        env = serialize_event(
            "crewlet.events.agent_phase_started",
            AgentPhaseStarted(
                agent_id="a1", role="Lead", turn_id="turn-1", iteration=2, phase="plan"
            ),
        )
        assert env["payload"]["phase"] == "plan"
        assert env["payload"]["iteration"] == 2
        assert env["category"] == "system"
