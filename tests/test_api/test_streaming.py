"""Tests for the dashboard live-stream service + WebSocket endpoint."""

from __future__ import annotations

import asyncio
import hashlib
import hmac
import json
from collections.abc import Awaitable, Callable

import pytest
from starlette.testclient import TestClient

from crewlet.api import streaming
from crewlet.api.streaming import (
    StreamService,
    build_health_envelope,
    serialize_event,
)
from crewlet.events.types import (
    AgentPhaseCompleted,
    AgentPhaseStarted,
    AgentTurnProgress,
    Event,
    TaskCompleted,
    TaskStarted,
)
from crewlet.timescaledb.memory import MemoryEventStore
from tests.test_api.helpers import create_app

_JIRA_SECRET = "jira-webhook-secret"


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
        # The Atlassian routes verify at the edge, so a suite about what
        # a webhook does downstream has to get past that first.
        jira_webhook_secret=_JIRA_SECRET,
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
            started_at = "2026-04-01T12:00:00+00:00"

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
            assert body["health"]["engine"] is True
            assert body["health"]["engine_started_at"] == "2026-04-01T12:00:00+00:00"

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
        body = json.dumps(
            {
                "webhookEvent": "jira:issue_created",
                "issue": {"key": "ENG-1", "fields": {"summary": "Bug"}},
                "user": {"displayName": "Alice"},
            }
        ).encode()
        signature = (
            "sha256="
            + hmac.new(_JIRA_SECRET.encode(), body, hashlib.sha256).hexdigest()
        )
        with TestClient(app) as client:
            response = client.post(
                "/webhooks/jira",
                content=body,
                headers={
                    "content-type": "application/json",
                    "x-hub-signature": signature,
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


# ---------------------------------------------------------------------------
# Derived pushes — the dashboard mirrors the projection, it does not rebuild it
# ---------------------------------------------------------------------------


class TestDerivedPushes:
    """The socket carries the *result* of applying an event.

    Every tab used to re-derive agent state, the sandbox set and the
    spend rollup from the raw event stream. Three copies of the server's
    own logic, three ways to drift. These pin the pushes that replaced
    them.
    """

    def _drain(self, ws, kind: str, limit: int = 8) -> dict:
        """Read envelopes until one of ``kind`` arrives."""
        for _ in range(limit):
            env = json.loads(ws.receive_text())
            if env["kind"] == kind:
                return env
        raise AssertionError(f"no {kind!r} envelope in {limit} frames")

    def test_phase_start_pushes_the_changed_agent(self, app, stream) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()  # snapshot
            client.portal.call(
                stream.ingest,
                "crewlet.events.agent_phase_started",
                AgentPhaseStarted(
                    agent_id="rt-1",
                    role="Lead",
                    turn_id="t1",
                    iteration=1,
                    phase="plan",
                ),
            )
            env = self._drain(ws, "agents")
            assert [row["role"] for row in env["data"]] == ["Lead"]
            assert env["data"][0]["state"] == "working"
            assert env["data"][0]["live_call"]["phase"] == "plan"

    def test_only_the_agent_that_moved_is_pushed(self, app, stream) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()
            client.portal.call(
                stream.ingest,
                "crewlet.events.task_started",
                TaskStarted(task_id="t-1", agent_id="rt-2", source="Dev", role="Dev"),
            )
            env = self._drain(ws, "agents")
            assert [row["role"] for row in env["data"]] == ["Dev"]
            assert env["data"][0]["current_task"] == "t-1"

    def test_a_completed_phase_pushes_the_spend_rollup(self, app, stream) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()
            client.portal.call(
                stream.ingest,
                "crewlet.events.agent_phase_completed",
                AgentPhaseCompleted(
                    agent_id="rt-1",
                    role="Lead",
                    turn_id="t1",
                    iteration=1,
                    phase="plan",
                    model="gpt-x",
                    input_tokens=10,
                    output_tokens=5,
                    total_tokens=15,
                ),
            )
            env = self._drain(ws, "tokens")
            assert env["data"]["totals"]["total_tokens"] == 15
            assert env["data"]["by_phase"][0]["phase"] == "plan"

    def test_the_snapshot_carries_spend_and_schedules(self, app) -> None:
        """A dashboard opens on one frame and issues no HTTP request."""
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            data = json.loads(ws.receive_text())["data"]
            for section in (
                "health",
                "agents",
                "events",
                "sandboxes",
                "org",
                "tools",
                "tokens",
                "schedules",
            ):
                assert section in data, f"snapshot is missing {section!r}"
            assert "totals" in data["tokens"]


# ---------------------------------------------------------------------------
# Query channel
# ---------------------------------------------------------------------------


class TestHealthTickSurvivesAFailure:
    """The service has exactly ONE background timer, started once.

    Nothing restarts it, so an exception escaping its body does not skip
    a tick — it ends health pushes and spend rollups for the life of the
    process. And the socket stays open throughout, so a dashboard keeps
    rendering the last health it received and goes on looking healthy:
    exactly the lie the health surface exists to prevent.
    """

    async def test_a_failing_rollup_does_not_kill_the_loop(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.setattr(streaming, "_TOKENS_PUSH_INTERVAL_SECONDS", 0.001)
        service = StreamService(health_interval=0.0)
        service.register()  # a client, so the flush path is taken
        service._tokens_dirty = True
        calls: list[int] = []

        def _explodes() -> dict:
            calls.append(1)
            raise RuntimeError("aggregation is broken")

        service.token_rollup = _explodes  # type: ignore[method-assign]
        service.start_health_tick(lambda: {"status": "ok"})
        try:
            for _ in range(200):
                await asyncio.sleep(0.005)
                if len(calls) >= 2:
                    break
            assert len(calls) >= 2, (
                "the tick died on the first failure and never ran again"
            )
            task = service._health_task
            assert task is not None and not task.done()
        finally:
            await service.stop_health_tick()

    async def test_a_failing_health_fn_does_not_kill_the_loop(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.setattr(streaming, "_TOKENS_PUSH_INTERVAL_SECONDS", 0.001)
        service = StreamService(health_interval=0.0)
        calls: list[int] = []

        def _explodes() -> dict:
            calls.append(1)
            raise RuntimeError("health is broken")

        service.start_health_tick(_explodes)
        try:
            for _ in range(200):
                await asyncio.sleep(0.005)
                if len(calls) >= 2:
                    break
            assert len(calls) >= 2
            task = service._health_task
            assert task is not None and not task.done()
        finally:
            await service.stop_health_tick()

    async def test_a_failed_flush_is_retried_rather_than_consumed(self) -> None:
        """The dirty flag is cleared only once the rollup is built. Clearing
        it first would eat the burst it failed on, leaving the panel stale
        until some later phase happened to complete."""
        service = StreamService()
        service.register()
        service._tokens_dirty = True

        def _explodes() -> dict:
            raise RuntimeError("aggregation is broken")

        service.token_rollup = _explodes  # type: ignore[method-assign]
        with pytest.raises(RuntimeError):
            service._flush_tokens()
        assert service._tokens_dirty is True


class TestStreamQueries:
    """Request/response over the same socket as the pushes."""

    def _query(self, ws, what: str, params: dict | None = None, **extra) -> dict:
        ws.send_text(
            json.dumps(
                {
                    "kind": "query",
                    "id": 7,
                    "what": what,
                    "params": params or {},
                    **extra,
                }
            )
        )
        for _ in range(8):
            env = json.loads(ws.receive_text())
            if env["kind"] in ("result", "error"):
                return env
        raise AssertionError("no reply to the query")

    def test_agent_query_answers_with_the_detail_payload(self, app) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()
            env = self._query(ws, "agent", {"id": "a-1"})
            assert env["kind"] == "result"
            assert env["id"] == 7
            assert env["what"] == "agent"
            assert env["data"]["role"] == "Lead"

    def test_a_missing_agent_answers_not_found(self, app) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()
            env = self._query(ws, "agent", {"id": "nope"})
            assert env["kind"] == "error"
            assert env["error"] == "not_found"

    def test_an_unknown_query_is_rejected(self, app) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()
            env = self._query(ws, "wat")
            assert env["kind"] == "error"
            assert env["error"] == "unknown_query"

    def test_schedules_query_answers(self, app) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()
            env = self._query(ws, "schedules")
            assert env["kind"] == "result"
            assert "schedules" in env["data"]
            assert "recent_runs" in env["data"]

    def test_tokens_query_answers_for_another_window(self, app) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()
            env = self._query(ws, "tokens", {"since_days": 30})
            assert env["kind"] == "result"
            assert env["data"]["since_days"] == 30

    def test_stream_query_answers_facts_about_this_socket(self, app) -> None:
        """Per-socket facts cannot ride the shared health tick.

        ``_fan_out`` encodes ONE JSON string and hands the same string to
        every client -- that is the design -- so a per-client field there
        would force one encode per client per tick.  They are answered on
        request instead, which is also when an operator wants them.
        """
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()
            env = self._query(ws, "stream")
            assert env["kind"] == "result"
            data = env["data"]
            assert data["dropped"] == 0
            assert data["clients"] == 1
            assert data["capacity"] > 0
            # ``drops`` resets on every reconnect, so the counter ships
            # with the window it accumulated over or it cannot be read.
            assert data["connected_at"]

    def test_config_query_requires_an_operator_token(self, app) -> None:
        """The socket enforces exactly what the /config middleware does."""
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()
            env = self._query(ws, "config")
            assert env["kind"] == "error"
            assert env["error"] == "unauthorized"

    def test_a_malformed_frame_does_not_drop_the_socket(self, app) -> None:
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()
            ws.send_text("not json at all")
            ws.send_text(json.dumps(["not", "an", "object"]))
            ws.send_text(json.dumps({"kind": "ping"}))
            env = json.loads(ws.receive_text())
            assert env["kind"] == "pong"

    def test_a_query_does_not_block_the_live_feed(self, app, stream) -> None:
        """Queries run concurrently with the push stream.

        An agent's LLM history is a store scan; serialising it ahead of
        the feed would stall every tab's live rows behind one read.
        """
        with TestClient(app) as client, client.websocket_connect("/ws/stream") as ws:
            ws.receive_text()
            ws.send_text(
                json.dumps(
                    {"kind": "query", "id": 1, "what": "agent", "params": {"id": "a-1"}}
                )
            )
            client.portal.call(
                stream.ingest,
                "crewlet.events.task_started",
                TaskStarted(task_id="t-9", agent_id="rt-1", source="Lead"),
            )
            kinds = set()
            for _ in range(8):
                kinds.add(json.loads(ws.receive_text())["kind"])
                if {"result", "event"} <= kinds:
                    break
            assert {"result", "event"} <= kinds


class TestTaskEventAttribution:
    """A task event has to name the seat it belongs to.

    The task lifecycle events carried the role only in ``source``, which
    neither the live projection nor the event store's ``agent_role`` tag
    reads. The consequence was quiet and wrong: an agent's current task
    never reached a dashboard, and a seat stayed "working" after its turn
    ended because the ``task_completed`` that should have cleared it was
    dropped on the floor.
    """

    def test_task_started_sets_the_current_task(self, stream) -> None:
        stream.live.apply_event(
            serialize_event(
                "crewlet.events.task_started",
                TaskStarted(task_id="T-1", agent_id="rt-1", source="Lead", role="Lead"),
            )
        )
        overlay = stream.live.agent_overlay("Lead")
        assert overlay is not None
        assert overlay["state"] == "working"
        assert overlay["current_task"] == "T-1"

    def test_task_completed_returns_the_seat_to_idle(self, stream) -> None:
        for event in (
            TaskStarted(task_id="T-1", agent_id="rt-1", source="Lead", role="Lead"),
            TaskCompleted(task_id="T-1", agent_id="rt-1", source="Lead", role="Lead"),
        ):
            stream.live.apply_event(
                serialize_event(f"crewlet.events.{event.type}", event)
            )
        overlay = stream.live.agent_overlay("Lead")
        assert overlay["state"] == "idle"
        assert overlay["current_task"] is None

    def test_the_event_store_tags_the_role(self, stream) -> None:
        """The same field feeds the store's ``agent_role`` tag."""
        from crewlet.timescaledb.writer import EventStoreWriter

        tags = EventStoreWriter._extract_tags(
            TaskStarted(task_id="T-1", agent_id="rt-1", source="Lead", role="Lead")
        )
        assert tags["agent_role"] == "Lead"


# ---------------------------------------------------------------------------
# health envelope
# ---------------------------------------------------------------------------


class TestHealthEnvelope:
    """The one function backing ``GET /health``, the snapshot and the tick.

    Every field here was a field the dashboard could not have rendered
    before, and the omission was not neutral: an engine with no active
    company revision -- one accepting and discarding every inbound
    webhook -- reported ``status: "ok"`` and looked, on every screen,
    exactly like a correctly configured engine with nothing to do.
    """

    def _app(self, store, **kwargs):
        return create_app(
            event_queue=_MockQueue(),
            event_store=store,
            agent_roles=AGENT_ROLES,
            **kwargs,
        )

    def test_an_unconfigured_engine_says_so(self, store) -> None:
        app = self._app(store)
        with TestClient(app):
            # What ``attach_config_refresh`` leaves behind when no
            # revision is active. An app built without a Tier B config
            # store is ``configured`` by construction, so the flag is
            # set here rather than a whole config store stood up to
            # arrive at the same one boolean.
            app.state.configured = False
            body = build_health_envelope(app)
        assert body["configured"] is False
        assert body["status"] == "unconfigured"

    def test_a_configured_engine_is_ok(self, store) -> None:
        app = self._app(store)
        with TestClient(app):
            app.state.configured = True
            body = build_health_envelope(app)
        assert body["status"] == "ok"

    def test_a_draining_engine_outranks_unconfigured(self, store) -> None:
        """Precedence is ``shutting_down > unconfigured > ok``.

        A draining engine is draining first, whatever else is true of
        it: the reader's next action is to wait, not to import a config.
        """

        class _Eng:
            is_running = True
            shutting_down = True
            in_flight_count = 1
            started_at = ""

        app = self._app(store, engine=_Eng())
        with TestClient(app):
            app.state.configured = False
            body = build_health_envelope(app)
        assert body["configured"] is False
        assert body["status"] == "shutting_down"

    def test_no_engine_is_stated_rather_than_implied(self, store) -> None:
        """The standalone API cannot answer ``in_flight`` at all.

        Without the ``engine`` flag a client cannot tell "nothing is
        running" from "this process cannot know", so it renders a
        confident zero for both.
        """
        app = self._app(store)
        with TestClient(app):
            body = build_health_envelope(app)
        assert body["engine"] is False
        assert "in_flight" not in body

    def test_a_store_with_no_database_is_memory_not_durable(self, store) -> None:
        """ "A store is wired" is not "history survives a restart".

        With no database the CLI still wraps in-memory legs in a
        ``CompositeEventStore``, so the presence check an operator would
        naturally make answers yes while every event is one process
        death from gone.
        """
        app = self._app(store)
        with TestClient(app):
            assert build_health_envelope(app)["event_store"] == "memory"

    def test_no_store_at_all_is_its_own_answer(self) -> None:
        app = create_app(event_queue=_MockQueue(), agent_roles=AGENT_ROLES)
        with TestClient(app):
            assert build_health_envelope(app)["event_store"] == "none"

    def test_the_queue_backend_is_read_off_the_protocol(self, store) -> None:
        """Not sniffed from the class name, which lies once wrapped."""
        from crewlet.queue.memory import MemoryEventQueue

        app = create_app(
            event_queue=MemoryEventQueue(),
            event_store=store,
            agent_roles=AGENT_ROLES,
        )
        with TestClient(app):
            assert build_health_envelope(app)["queue"] == "memory"

    def test_feed_hydrated_is_false_when_the_store_could_not_be_read(self) -> None:
        """A projection that failed to seed must not claim it seeded.

        Every hydration leg swallows its own store error -- that is what
        best-effort means for the projection -- so watching for an
        exception cannot tell a seeded feed from an empty one.
        """

        class _BrokenStore:
            async def get_agent_states(self, roles):
                raise RuntimeError("no")

            async def list_token_usage_events(self, since_days):
                raise RuntimeError("no")

            async def list_phase_token_events(self, since_days):
                raise RuntimeError("no")

            async def list_events(self, **kwargs):
                raise RuntimeError("no")

        app = create_app(
            event_queue=_MockQueue(),
            event_store=_BrokenStore(),
            agent_roles=AGENT_ROLES,
        )
        with TestClient(app):
            assert build_health_envelope(app)["feed_hydrated"] is False

    def test_feed_hydrated_is_true_after_a_clean_seed(self, store) -> None:
        app = self._app(store)
        with TestClient(app):
            assert build_health_envelope(app)["feed_hydrated"] is True

    def test_the_health_route_and_the_push_answer_identically(self, store) -> None:
        """One builder, three surfaces.

        The REST route, the handshake snapshot and the five-second tick
        must never be able to disagree about whether the engine is
        healthy -- that is exactly the class of bug a second
        implementation introduces.
        """
        app = self._app(store)
        with TestClient(app) as client:
            route = client.get("/health").json()
            snapshot = client.get("/stream/snapshot").json()["health"]
        assert route == snapshot
