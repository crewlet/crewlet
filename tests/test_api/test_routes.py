"""Tests for the standalone crewlet-api REST routes.

Uses mock EventQueue — no Engine dependency.
"""

from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import json
from collections.abc import Awaitable, Callable
from datetime import UTC, datetime
from typing import Any
from unittest.mock import AsyncMock, MagicMock

import pytest
from starlette.testclient import TestClient

from crewlet.api.app import create_app
from crewlet.config import (
    ApiAuthConfig,
    ApiAuthTokenConfig,
    ApiConfig,
    BootstrapConfig,
)
from crewlet.events.types import Event
from crewlet.timescaledb.memory import MemoryEventStore

# ---------------------------------------------------------------------------
# Mock infrastructure
# ---------------------------------------------------------------------------


class MockEventQueue:
    """Records published events."""

    def __init__(self) -> None:
        self.published: list[tuple[str, Event]] = []

    async def publish(self, topic: str, event: Event) -> None:
        self.published.append((topic, event))

    async def subscribe(
        self,
        topic: str,
        group: str,
        handler: Callable[[Event], Awaitable[None]],
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
        self,
        listener: Callable[[str, Event], Awaitable[None]],
    ) -> None:
        pass

    async def start(self) -> None:
        pass

    async def stop(self) -> None:
        pass


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

AGENT_ROLES = [
    {"id": "a-1", "name": "Lead", "role": "Lead", "goal": "Lead", "handle": "lead"},
    {"id": "a-2", "name": "Dev A", "role": "Dev A", "goal": "Code", "handle": "dev-a"},
    {"id": "a-3", "name": "Dev B", "role": "Dev B", "goal": "Code", "handle": "dev-b"},
]


@pytest.fixture
def event_queue() -> MockEventQueue:
    return MockEventQueue()


@pytest.fixture
def event_store() -> MemoryEventStore:
    store = MemoryEventStore()
    loop = asyncio.new_event_loop()
    loop.run_until_complete(store.start())
    loop.close()
    return store


@pytest.fixture
def app(event_queue: MockEventQueue, event_store: MemoryEventStore):
    return create_app(
        event_queue=event_queue,
        event_store=event_store,
        agent_roles=AGENT_ROLES,
    )


@pytest.fixture
def app_no_store(event_queue: MockEventQueue):
    """App without an event store (fallback behaviour)."""
    return create_app(
        event_queue=event_queue,
        agent_roles=AGENT_ROLES,
    )


@pytest.fixture
def client(app) -> TestClient:
    return TestClient(app)


@pytest.fixture
def client_no_store(app_no_store) -> TestClient:
    return TestClient(app_no_store)


# ---------------------------------------------------------------------------
# Dashboard & static
# ---------------------------------------------------------------------------


class TestDashboard:
    def test_root_redirects_to_dashboard(self, client: TestClient):
        resp = client.get("/", follow_redirects=False)
        assert resp.status_code == 302
        assert resp.headers["location"] == "/dashboard"

    def test_dashboard_returns_html(self, client: TestClient):
        resp = client.get("/dashboard")
        assert resp.status_code == 200
        assert "text/html" in resp.headers["content-type"]
        assert "Crewlet Dashboard" in resp.text
        # The shell loads the modular ES-module app + stylesheets.
        assert "/static/dashboard/js/app.js" in resp.text
        assert "/static/dashboard/styles/tokens.css" in resp.text

    def test_dashboard_ships_afk_state_and_quip(self, client: TestClient):
        """The dashboard ships a CSS branch and a per-cause quip helper
        for the ``afk`` state.  These live in served
        ES-module / stylesheet assets."""
        state_js = client.get("/static/dashboard/js/state.js")
        assert state_js.status_code == 200
        assert "text/javascript" in state_js.headers["content-type"]
        body = state_js.text
        # The quip + badge helpers recognise the failure causes.
        assert "afkQuip" in body
        assert "stateBadgeClass" in body
        # Generic fallback (the safety net) + a cause-specific quip.
        assert "stepped out for coffee" in body
        assert "brain unplugged" in body
        # The AFK chip styling ships in the component stylesheet.
        css = client.get("/static/dashboard/styles/components.css")
        assert css.status_code == 200
        assert ".badge.afk" in css.text
        assert ".afk-quip" in css.text

    def test_dashboard_module_served_as_javascript(self, client: TestClient):
        resp = client.get("/static/dashboard/js/store.js")
        assert resp.status_code == 200
        assert "text/javascript" in resp.headers["content-type"]

    def test_dashboard_ships_both_theme_token_sets(self, client: TestClient):
        """Every colour is defined once, in the token layer, for BOTH
        themes -- a theme is only a different set of values there.  Each
        categorical hue ships a mark step and an ``-ink`` text step; a
        label wearing the mark step would not clear contrast."""
        css = client.get("/static/dashboard/styles/tokens.css")
        assert css.status_code == 200
        body = css.text
        assert ':root[data-theme="light"]' in body
        assert ':root[data-theme="dark"]' in body
        hues = ("green", "blue", "amber", "purple", "cyan", "orange", "pink", "brown")
        for hue in hues:
            assert f"--{hue}:" in body, hue
            assert f"--{hue}-ink:" in body, hue
        # The reserved status hue, kept out of the categorical set.
        assert "--red:" in body and "--red-ink:" in body
        # Surface / glass / elevation primitives the panel recipe reads.
        for token in ("--bg-card:", "--glass:", "--panel-shadow:", "--lift-shadow:"):
            assert token in body, token

    def test_dashboard_panel_recipe_is_shared(self, client: TestClient):
        """Every surface is the same object -- one rule grants the fill,
        border, radius and elevation, so a view cannot drift its own
        panel styling."""
        css = client.get("/static/dashboard/styles/components.css")
        assert css.status_code == 200
        body = css.text
        recipe = body.split("var(--panel-shadow)")[0]
        surfaces = (".card", ".list", ".stat", ".tool-card", ".turn")
        for selector in (*surfaces, ".mem-card"):
            assert f"{selector},\n" in recipe or f"{selector} {{" in recipe, selector
        # The recipe owns the fill, and it uses ``--bg-card`` NEAT: the
        # token is itself a warm alpha over the ground in the dark theme,
        # so diluting it a second time leaves a panel under the threshold
        # where its own edge reads at all.
        assert "background: var(--bg-card);" in recipe
        assert "color-mix(in srgb, var(--bg-card)" not in recipe
        views = client.get("/static/dashboard/styles/views.css").text
        assert "color-mix(in srgb, var(--bg-card)" not in views

    def test_dashboard_hidden_attribute_is_honoured(self, client: TestClient):
        """`[hidden]` loses to any author `display`, so chrome that toggles
        via the attribute (the sidebar's in-flight footer) needs the rule
        re-asserted -- without it an empty pill is always on screen."""
        css = client.get("/static/dashboard/styles/base.css")
        assert css.status_code == 200
        assert "[hidden]" in css.text
        assert "display: none !important" in css.text

    def test_dashboard_phase_hues_have_one_source_of_truth(self, client: TestClient):
        """Phase colour is derived from a single map, and callers pick the
        mark step or the text step explicitly -- a second hardcoded copy is
        what let the agent x stage heatmap freeze at one theme's values."""
        state_js = client.get("/static/dashboard/js/state.js").text
        assert "PHASE_HUE" in state_js
        assert "export function phaseColor" in state_js
        assert "export function phaseInk" in state_js
        # No parallel hardcoded RGB table for the heatmap.
        assert "PHASE_RGB" not in state_js
        tokens_js = client.get("/static/dashboard/js/views/tokens.js").text
        assert "color-mix" in tokens_js

    def test_dashboard_nav_only_ships_backed_views(self, client: TestClient):
        """Every sidebar entry resolves to a view that reads a real
        endpoint -- a nav item leading to an empty screen is worse than no
        nav item, so the roster and the router are kept in step."""
        app_js = client.get("/static/dashboard/js/app.js").text
        router_js = client.get("/static/dashboard/js/router.js").text
        for view in (
            "company",
            "people",
            "org",
            "audit",
            "agents",
            "events",
            "tokens",
            "tools",
            "schedules",
            "config",
        ):
            assert f'"{view}"' in app_js, view
            assert f'"{view}"' in router_js, view
        # Screens with nothing behind them stay out of the nav entirely.
        for absent in ("billing", "wallet"):
            assert f'name: "{absent}"' not in app_js, absent

    def test_dashboard_flattens_the_org_tree_once(self, client: TestClient):
        """Views consume seats, not the raw /org payload, so unit-lead and
        `mcp_env` inheritance are resolved in exactly one place."""
        org_js = client.get("/static/dashboard/js/org.js")
        assert org_js.status_code == 200
        body = org_js.text
        assert "export function flattenSeats" in body
        assert "export function flattenUnits" in body
        # Lead inheritance: a child unit with no `lead` takes its parent's.
        assert "unit.lead || inheritedLead" in body
        for view in ("company.js", "people.js", "agents.js", "dashboard.js"):
            v = client.get(f"/static/dashboard/js/views/{view}").text
            assert "flattenSeats" in v or "flattenUnits" in v, view

    def test_dashboard_seat_status_is_derived_not_invented(self, client: TestClient):
        """A seat card's status line comes from live state only. An agent
        with nothing in flight has to say so rather than be given something
        plausible to be doing."""
        state_js = client.get("/static/dashboard/js/state.js").text
        assert "export function statusLine" in state_js
        assert "nothing in the inbox" in state_js
        assert "not running" in state_js
        # A human seat is marked as one, never given a runtime.
        assert "not run by the engine" in state_js
        # Integration chips come from wired MCP servers / contact identities.
        assert "export function seatIntegrations" in state_js
        assert "export function contactIntegrations" in state_js

    def test_dashboard_formats_future_timestamps(self, client: TestClient):
        """`relTime` measures elapsed time, so a scheduled run in the
        future clamped to "just now".  Upcoming instants go through
        `untilTime` instead."""
        fmt = client.get("/static/dashboard/js/format.js").text
        assert "export function untilTime" in fmt
        assert "due now" in fmt
        schedules = client.get("/static/dashboard/js/views/schedules.js").text
        assert "untilTime" in schedules
        # The old string-mangling ("in " + relTime(...) minus " ago") is gone.
        assert '.replace(" ago"' not in schedules

    def test_static_asset_etag_revalidation(self, client: TestClient):
        """Assets carry a content ETag + no-cache, so a redeploy is picked
        up on the next request while unchanged files get a cheap 304 (no
        stale-module window — the cause of 'I still see the old dashboard')."""
        r1 = client.get("/static/dashboard/js/llm.js")
        assert r1.status_code == 200
        etag = r1.headers.get("etag")
        assert etag
        assert "no-cache" in r1.headers.get("cache-control", "")
        r2 = client.get("/static/dashboard/js/llm.js", headers={"If-None-Match": etag})
        assert r2.status_code == 304

    def test_static_file_serves_icon(self, client: TestClient):
        resp = client.get("/static/crewlet-icon.png")
        assert resp.status_code == 200
        assert resp.headers["content-type"] == "image/png"

    def test_static_file_not_found(self, client: TestClient):
        resp = client.get("/static/nonexistent.png")
        assert resp.status_code == 404

    def test_static_file_rejects_path_traversal(self, client: TestClient):
        resp = client.get("/static/../app.py")
        # Starlette normalises the path; either way the app module is
        # never served as a static asset.
        assert resp.status_code in (403, 404)


# ---------------------------------------------------------------------------
# Health
# ---------------------------------------------------------------------------


class TestHealthEndpoint:
    def test_health(self, client: TestClient):
        resp = client.get("/health")
        assert resp.status_code == 200
        assert resp.json()["status"] == "ok"

    def test_health_omits_runtime_metrics_without_engine(
        self, client: TestClient
    ) -> None:
        """Standalone API process has no engine, so /health omits
        ``in_flight`` and ``shutting_down`` rather than synthesizing
        misleading defaults."""
        resp = client.get("/health")
        body = resp.json()
        assert "in_flight" not in body
        assert "shutting_down" not in body

    def test_health_includes_runtime_metrics_with_engine(
        self, event_queue: MockEventQueue
    ) -> None:
        """Embedded API exposes the engine's in-flight handler count
        so the dashboard's drain pill has a real-time signal during
        a graceful shutdown."""

        class _StubEngine:
            is_running = True
            shutting_down = False
            in_flight_count = 7

        app = create_app(
            event_queue=event_queue,
            agent_roles=AGENT_ROLES,
            engine=_StubEngine(),
        )
        with TestClient(app) as client:
            resp = client.get("/health")
            body = resp.json()
            assert body["in_flight"] == 7
            assert body["shutting_down"] is False

    def test_health_shutting_down_during_drain(
        self, event_queue: MockEventQueue
    ) -> None:
        """The drain is visible WHILE it happens: ``Engine.shutting_down``
        flips at the start of ``stop()``, long before ``is_running``
        flips at the end — the dashboard's footer pill depends on it."""

        class _DrainingEngine:
            is_running = True  # teardown not finished yet
            shutting_down = True  # but the drain has begun
            in_flight_count = 3

        app = create_app(
            event_queue=event_queue,
            agent_roles=AGENT_ROLES,
            engine=_DrainingEngine(),
        )
        with TestClient(app) as client:
            body = client.get("/health").json()
            assert body["in_flight"] == 3
            assert body["shutting_down"] is True
            assert body["status"] == "shutting_down"

    def test_health_shutting_down_flag_set_when_engine_stopped(
        self, event_queue: MockEventQueue
    ) -> None:
        class _StoppedEngine:
            is_running = False
            shutting_down = True
            in_flight_count = 2

        app = create_app(
            event_queue=event_queue,
            agent_roles=AGENT_ROLES,
            engine=_StoppedEngine(),
        )
        with TestClient(app) as client:
            body = client.get("/health").json()
            assert body["in_flight"] == 2
            assert body["shutting_down"] is True


# ---------------------------------------------------------------------------
# Agents
# ---------------------------------------------------------------------------


class TestAgentEndpoints:
    def test_list_agents_without_store(self, client_no_store: TestClient):
        resp = client_no_store.get("/agents")
        assert resp.status_code == 200
        agents = resp.json()
        assert len(agents) == 3
        names = {a["name"] for a in agents}
        assert names == {"Lead", "Dev A", "Dev B"}

    def test_list_agents_with_store(self, client: TestClient):
        resp = client.get("/agents")
        assert resp.status_code == 200
        agents = resp.json()
        assert len(agents) == 3

    def test_get_agent_not_found(self, client: TestClient):
        resp = client.get("/agents/nonexistent")
        assert resp.status_code == 404

    def test_get_agent_without_store(self, client_no_store: TestClient):
        resp = client_no_store.get("/agents/a-1")
        assert resp.status_code == 200
        assert resp.json()["name"] == "Lead"

    def test_cmd_api_event_store_aggregates_tokens(self) -> None:
        """``cmd_api`` must wrap the
        persistent leg in a ``CompositeEventStore`` so ``/agents`` returns
        non-zero token totals.

        The per-store ``get_agent_states`` leaves token counters at
        zero — aggregation is done at the composite layer via
        ``list_token_usage_events``.  Wiring
        ``BufferedEventStore(TimescaleDBEventStore(...))`` directly would
        make the standalone API dashboard report zero tokens for every
        agent.  This test calls the real ``_build_api_event_store``
        helper to gate the wiring end-to-end.
        """
        from datetime import UTC, datetime

        from crewlet.cli import _build_api_event_store

        async def _seed() -> object:
            # db=None → helper returns a CompositeEventStore with two
            # MemoryEventStore legs (the "no persistent store" fallback).
            # This still exercises the composite wrapping that the
            # TimescaleDB path also relies on.  Writing through the
            # top-level store exercises whatever write path the helper
            # actually produces, so this test fails cleanly if the
            # helper stops returning a composite.
            store = _build_api_event_store(None)
            await store.start()
            now = datetime.now(UTC)
            await store.write_event(
                event_id="spawn",
                event_type="agent_spawned",
                source="lead",
                timestamp=now,
                tags={"agent_id": "runtime-1", "agent_role": "Lead"},
            )
            await store.write_event(
                event_id="turn",
                event_type="agent_turn_completed",
                source="lead",
                timestamp=now,
                payload={
                    "input_tokens": 200,
                    "output_tokens": 100,
                    "total_tokens": 300,
                },
                tags={"agent_id": "runtime-1", "agent_role": "Lead"},
            )
            return store

        loop = asyncio.new_event_loop()
        try:
            store = loop.run_until_complete(_seed())
        finally:
            loop.close()

        app = create_app(
            event_queue=MockEventQueue(),
            event_store=store,
            agent_roles=AGENT_ROLES,
        )
        # The ``with`` block runs the app lifespan, which hydrates the
        # live-state projection from the store — ``/agents`` reads token
        # totals from that projection, aggregated by the composite via
        # ``list_token_usage_events``.
        with TestClient(app) as client:
            resp = client.get("/agents")
            assert resp.status_code == 200
            agents = {a["role"]: a for a in resp.json()}
            lead = agents["Lead"]
            # The composite aggregates via list_token_usage_events —
            # never 0/0/0.
            assert lead["input_tokens"] == 200, lead
            assert lead["output_tokens"] == 100, lead
            assert lead["total_tokens"] == 300, lead


# ---------------------------------------------------------------------------
# Agent memory endpoint
# ---------------------------------------------------------------------------


class TestAgentMemoryEndpoint:
    """``GET /agents/{id}/memory`` aggregates the four learning stores.

    Tests use the in-memory knowledge provider for personal memories
    and skip the SQL-backed sections (episodes, counterparty profiles,
    synthesized skills) by leaving ``database=None`` — those should
    return empty lists, not raise.
    """

    def test_memory_404_for_unknown_agent(self, client: TestClient):
        resp = client.get("/agents/missing/memory")
        assert resp.status_code == 404

    def test_memory_empty_when_no_knowledge(self, client: TestClient):
        """Without a knowledge system, all four sections return empty."""
        resp = client.get("/agents/a-1/memory")
        assert resp.status_code == 200
        body = resp.json()
        assert body["agent_id"] == "a-1"
        assert body["handle"] == "lead"
        assert body["role"] == "Lead"
        assert body["personal_memories"] == {"long": [], "short": []}
        assert body["episodes"] == []
        assert body["counterparty_profiles"] == []
        assert body["synthesized_skills"] == []

    def test_memory_returns_personal_memories(
        self, event_queue: MockEventQueue, event_store: MemoryEventStore
    ):
        """``agent_diary`` rows at this agent's id are split into long /
        short buckets.  TTL-expired SHORT entries are retained but
        flagged ``expired=true``.

        The
        endpoint queries ``agent_diary`` directly via a
        ``Database``-typed handle, so the test stubs the database
        with an asyncpg-shaped dummy that returns the desired rows.
        """
        from datetime import UTC, datetime, timedelta
        from uuid import uuid4

        from crewlet.db.client import Database

        future = (datetime.now(UTC) + timedelta(days=7)).isoformat()
        past = (datetime.now(UTC) - timedelta(days=1)).isoformat()

        diary_rows = [
            {
                "id": uuid4(),
                "agent_id": "a-1",
                "kind": "diary_long",
                "content": "Stakeholder prefers concise replies",
                "ttl_until": None,
                "source": "reflect_and_persist",
                "turn_id": "",
                "metadata": "{}",
                "retrieval_count": 0,
                "last_retrieved_at": None,
                "created_at": datetime.now(UTC),
            },
            {
                "id": uuid4(),
                "agent_id": "a-1",
                "kind": "diary_short",
                "content": "Sprint freeze in effect this week",
                "ttl_until": datetime.fromisoformat(future),
                "source": "reflect_and_persist",
                "turn_id": "",
                "metadata": "{}",
                "retrieval_count": 0,
                "last_retrieved_at": None,
                "created_at": datetime.now(UTC),
            },
            {
                "id": uuid4(),
                "agent_id": "a-1",
                "kind": "diary_short",
                "content": "Old reminder past its TTL",
                "ttl_until": datetime.fromisoformat(past),
                "source": "reflect_and_persist",
                "turn_id": "",
                "metadata": "{}",
                "retrieval_count": 0,
                "last_retrieved_at": None,
                "created_at": datetime.now(UTC),
            },
        ]

        class _StubDB(Database):
            def __init__(self) -> None:
                pass

            async def execute(self, query: str, *args, **kwargs):
                return diary_rows

        app = create_app(
            event_queue=event_queue,
            event_store=event_store,
            agent_roles=AGENT_ROLES,
            database=_StubDB(),
        )
        client = TestClient(app)

        resp = client.get("/agents/a-1/memory")
        assert resp.status_code == 200
        body = resp.json()
        long_mem = body["personal_memories"]["long"]
        short_mem = body["personal_memories"]["short"]
        assert len(long_mem) == 1
        assert long_mem[0]["content"] == "Stakeholder prefers concise replies"
        assert len(short_mem) == 2
        contents = {(e["content"], e["expired"]) for e in short_mem}
        assert ("Sprint freeze in effect this week", False) in contents
        assert ("Old reminder past its TTL", True) in contents


class TestToolsEndpoint:
    def test_list_tools_empty_by_default(self, client: TestClient):
        """Without tools_data the endpoint returns an empty list."""
        resp = client.get("/tools")
        assert resp.status_code == 200
        assert resp.json() == []

    def test_list_tools_returns_tools_data(self, event_queue: MockEventQueue):
        """When tools_data is provided, GET /tools returns it."""
        tools = [
            {
                "name": "send_message",
                "description": "Send a message",
                "source": "builtin",
                "roles": ["Lead", "Dev A"],
            },
            {
                "name": "jira_create_issue",
                "description": "Create a Jira issue",
                "source": "mcp:atlassian",
                "roles": ["Lead"],
            },
        ]
        app = create_app(
            event_queue=event_queue,
            agent_roles=AGENT_ROLES,
            tools_data=tools,
        )
        tc = TestClient(app)
        resp = tc.get("/tools")
        assert resp.status_code == 200
        data = resp.json()
        assert len(data) == 2
        assert data[0]["name"] == "send_message"
        assert data[0]["roles"] == ["Lead", "Dev A"]
        assert data[1]["source"] == "mcp:atlassian"


# ---------------------------------------------------------------------------
# Events
# ---------------------------------------------------------------------------


class TestEventEndpoints:
    def test_list_events_empty(self, client: TestClient):
        resp = client.get("/events")
        assert resp.status_code == 200
        assert resp.json() == []

    def test_list_events_without_store(self, client_no_store: TestClient):
        resp = client_no_store.get("/events")
        assert resp.status_code == 200
        assert resp.json() == []

    def test_get_event_without_store(self, client_no_store: TestClient):
        resp = client_no_store.get("/events/some-id")
        assert resp.status_code == 503

    def test_get_event_not_found(self, client: TestClient):
        resp = client.get("/events/nonexistent")
        assert resp.status_code == 404

    def test_list_events_with_trace_filter(
        self, client: TestClient, event_store: MemoryEventStore
    ):
        from datetime import UTC, datetime

        loop = asyncio.new_event_loop()
        loop.run_until_complete(
            event_store.write_event(
                event_id="e1",
                event_type="task_created",
                source="pm",
                timestamp=datetime.now(UTC),
                trace_id="trace-abc",
            )
        )
        loop.run_until_complete(
            event_store.write_event(
                event_id="e2",
                event_type="task_started",
                source="dev",
                timestamp=datetime.now(UTC),
                trace_id="trace-xyz",
            )
        )
        loop.close()

        resp = client.get("/events?trace_id=trace-abc")
        assert resp.status_code == 200
        events = resp.json()
        assert len(events) == 1
        assert events[0]["trace_id"] == "trace-abc"

    def test_list_trace_endpoint(
        self, client: TestClient, event_store: MemoryEventStore
    ):
        from datetime import UTC, datetime

        loop = asyncio.new_event_loop()
        loop.run_until_complete(
            event_store.write_event(
                event_id="e1",
                event_type="task_created",
                source="pm",
                timestamp=datetime.now(UTC),
                trace_id="trace-t1",
                span_id="span-1",
            )
        )
        loop.run_until_complete(
            event_store.write_event(
                event_id="e2",
                event_type="task_started",
                source="dev",
                timestamp=datetime.now(UTC),
                trace_id="trace-t1",
                span_id="span-2",
                parent_span_id="span-1",
            )
        )
        loop.close()

        resp = client.get("/events/trace/trace-t1")
        assert resp.status_code == 200
        events = resp.json()
        assert len(events) == 2
        assert events[0]["id"] == "e1"
        assert events[1]["parent_span_id"] == "span-1"

    def test_list_trace_without_store(self, client_no_store: TestClient):
        resp = client_no_store.get("/events/trace/some-trace")
        assert resp.status_code == 503


# ---------------------------------------------------------------------------
# Webhooks (EventQueue)
# ---------------------------------------------------------------------------


class TestJiraWebhook:
    def test_jira_webhook_publishes(
        self, client: TestClient, event_queue: MockEventQueue
    ):
        resp = client.post(
            "/webhooks/jira",
            json={"webhookEvent": "jira:issue_updated", "issue": {"key": "PROJ-1"}},
        )
        assert resp.status_code == 200

        assert len(event_queue.published) == 1
        topic, event = event_queue.published[0]
        assert topic == "crewlet.notifications.inbound"
        assert event.type == "raw_webhook"
        assert event.source == "jira"
        assert "body" in event.payload
        assert "body_raw_b64" in event.payload
        assert event.payload["body"]["issue"]["key"] == "PROJ-1"

    def test_jira_webhook_invalid_json(self, client: TestClient):
        resp = client.post(
            "/webhooks/jira",
            content=b"not json",
            headers={"content-type": "application/json"},
        )
        assert resp.status_code == 400


class TestSlackWebhook:
    def test_slack_webhook_publishes(
        self, client: TestClient, event_queue: MockEventQueue
    ):
        resp = client.post(
            "/webhooks/slack/my-bot",
            json={"type": "event_callback", "event": {"type": "message"}},
        )
        assert resp.status_code == 200

        assert len(event_queue.published) == 1
        topic, event = event_queue.published[0]
        assert topic == "crewlet.notifications.inbound"
        assert event.type == "raw_webhook"
        assert event.source == "slack"
        assert event.payload["handle"] == "my-bot"
        assert "body_raw_b64" in event.payload

    def test_slack_url_verification(self, client: TestClient):
        resp = client.post(
            "/webhooks/slack/my-bot",
            json={"type": "url_verification", "challenge": "abc123"},
        )
        assert resp.status_code == 200
        assert resp.json()["challenge"] == "abc123"


class TestGithubWebhook:
    """GitHub webhook tests — all require a configured secret."""

    SECRET = "test-webhook-secret"

    @pytest.fixture
    def github_app(self, event_queue: MockEventQueue, event_store: MemoryEventStore):
        return create_app(
            event_queue=event_queue,
            event_store=event_store,
            agent_roles=AGENT_ROLES,
            github_webhook_secret=self.SECRET,
        )

    @pytest.fixture
    def github_client(self, github_app) -> TestClient:
        return TestClient(github_app, raise_server_exceptions=False)

    def _sign(self, body: bytes) -> str:
        return (
            "sha256=" + hmac.new(self.SECRET.encode(), body, hashlib.sha256).hexdigest()
        )

    def test_github_webhook_publishes(
        self, github_client: TestClient, event_queue: MockEventQueue
    ):
        body = json.dumps({"action": "opened", "pull_request": {"number": 1}}).encode()
        resp = github_client.post(
            "/webhooks/github",
            content=body,
            headers={
                "content-type": "application/json",
                "x-hub-signature-256": self._sign(body),
                "x-github-event": "pull_request",
            },
        )
        assert resp.status_code == 200

        assert len(event_queue.published) == 1
        topic, event = event_queue.published[0]
        assert topic == "crewlet.notifications.inbound"
        assert event.type == "raw_webhook"
        assert event.source == "github"
        assert "body_raw_b64" in event.payload

    def test_github_webhook_invalid_json(self, github_client: TestClient):
        body = b"not json"
        resp = github_client.post(
            "/webhooks/github",
            content=body,
            headers={
                "content-type": "application/json",
                "x-hub-signature-256": self._sign(body),
            },
        )
        assert resp.status_code == 400

    def test_missing_signature_rejected(self, github_client: TestClient):
        resp = github_client.post(
            "/webhooks/github",
            json={"action": "opened"},
        )
        assert resp.status_code == 401

    def test_wrong_signature_rejected(self, github_client: TestClient):
        resp = github_client.post(
            "/webhooks/github",
            content=b'{"action": "opened"}',
            headers={
                "content-type": "application/json",
                "x-hub-signature-256": "sha256=bad",
            },
        )
        assert resp.status_code == 401

    def test_mismatched_body_signature_rejected(self, github_client: TestClient):
        """Signature valid for a different body is rejected (replay)."""
        sig_for_other_body = self._sign(b'{"action": "closed"}')
        resp = github_client.post(
            "/webhooks/github",
            content=b'{"action": "opened"}',
            headers={
                "content-type": "application/json",
                "x-hub-signature-256": sig_for_other_body,
            },
        )
        assert resp.status_code == 401


class TestGitlabWebhook:
    """GitLab webhook tests — signing-token (19.1+ Standard-Webhooks) only."""

    SIGNING = "whsec_" + base64.b64encode(b"k" * 32).decode()

    @pytest.fixture
    def signing_client(
        self, event_queue: MockEventQueue, event_store: MemoryEventStore
    ) -> TestClient:
        app = create_app(
            event_queue=event_queue,
            event_store=event_store,
            agent_roles=AGENT_ROLES,
            gitlab_signing_secret=self.SIGNING,
        )
        return TestClient(app, raise_server_exceptions=False)

    def _sig_headers(self, body: bytes) -> dict[str, str]:
        wid = "msg_1"
        wts = str(int(datetime.now(UTC).timestamp()))
        key = base64.b64decode(self.SIGNING[len("whsec_") :])
        signed = f"{wid}.{wts}.".encode() + body
        sig = base64.b64encode(hmac.new(key, signed, hashlib.sha256).digest()).decode()
        return {
            "content-type": "application/json",
            "webhook-id": wid,
            "webhook-timestamp": wts,
            "webhook-signature": f"v1,{sig}",
            "x-gitlab-event": "Merge Request Hook",
        }

    def test_signing_token_accepts(
        self, signing_client: TestClient, event_queue: MockEventQueue
    ):
        body = json.dumps({"object_kind": "merge_request"}).encode()
        resp = signing_client.post(
            "/webhooks/gitlab", content=body, headers=self._sig_headers(body)
        )
        assert resp.status_code == 200
        assert len(event_queue.published) == 1
        _, event = event_queue.published[0]
        assert event.source == "gitlab"

    def test_missing_signature_rejected(self, signing_client: TestClient):
        resp = signing_client.post("/webhooks/gitlab", json={"object_kind": "issue"})
        assert resp.status_code == 401

    def test_signing_token_tampered_body_rejected(self, signing_client: TestClient):
        headers = self._sig_headers(b'{"object_kind": "merge_request"}')
        resp = signing_client.post(
            "/webhooks/gitlab", content=b'{"object_kind": "issue"}', headers=headers
        )
        assert resp.status_code == 401

    def test_signing_token_stale_timestamp_rejected(self, signing_client: TestClient):
        body = b'{"object_kind": "merge_request"}'
        headers = self._sig_headers(body)
        headers["webhook-timestamp"] = "1000000000"  # far in the past
        resp = signing_client.post("/webhooks/gitlab", content=body, headers=headers)
        assert resp.status_code == 401

    def test_no_secret_configured_500(
        self, event_queue: MockEventQueue, event_store: MemoryEventStore
    ):
        app = create_app(
            event_queue=event_queue,
            event_store=event_store,
            agent_roles=AGENT_ROLES,
        )
        client = TestClient(app, raise_server_exceptions=False)
        resp = client.post("/webhooks/gitlab", json={"object_kind": "issue"})
        assert resp.status_code == 500

    def test_no_secret_configured_returns_500(
        self,
        client: TestClient,
    ):
        """When no secret is configured, the endpoint rejects with 500."""
        resp = client.post(
            "/webhooks/github",
            json={"action": "opened"},
        )
        assert resp.status_code == 500


class TestPlaneWebhook:
    """Plane webhook tests — HMAC-SHA256 hexdigest of the raw body,
    carried in ``X-Plane-Signature`` (CE webhook scheme)."""

    SECRET = "plane-webhook-secret"

    @pytest.fixture
    def plane_app(self, event_queue: MockEventQueue, event_store: MemoryEventStore):
        return create_app(
            event_queue=event_queue,
            event_store=event_store,
            agent_roles=AGENT_ROLES,
            plane_webhook_secret=self.SECRET,
        )

    @pytest.fixture
    def plane_client(self, plane_app) -> TestClient:
        return TestClient(plane_app, raise_server_exceptions=False)

    def _sign(self, body: bytes) -> str:
        return hmac.new(self.SECRET.encode(), body, hashlib.sha256).hexdigest()

    def _headers(self, body: bytes) -> dict[str, str]:
        return {
            "content-type": "application/json",
            "x-plane-signature": self._sign(body),
            "x-plane-event": "issue",
        }

    def test_valid_signature_publishes(
        self, plane_client: TestClient, event_queue: MockEventQueue
    ):
        body = json.dumps(
            {
                "event": "issue",
                "action": "created",
                "workspace_slug": "acme",
                "data": {"id": "9f8e7d6c", "name": "Fix login flow"},
            }
        ).encode()
        resp = plane_client.post(
            "/webhooks/plane", content=body, headers=self._headers(body)
        )
        assert resp.status_code == 200
        assert resp.json() == {"status": "ok"}

        assert len(event_queue.published) == 1
        topic, event = event_queue.published[0]
        assert topic == "crewlet.notifications.inbound"
        assert event.type == "raw_webhook"
        assert event.source == "plane"
        assert event.payload["body"]["data"]["name"] == "Fix login flow"
        assert "headers" in event.payload
        assert base64.b64decode(event.payload["body_raw_b64"]) == body

    def test_tampered_body_401(self, plane_client: TestClient):
        headers = self._headers(b'{"event": "issue", "action": "created"}')
        resp = plane_client.post(
            "/webhooks/plane",
            content=b'{"event": "issue", "action": "deleted"}',
            headers=headers,
        )
        assert resp.status_code == 401

    def test_missing_signature_401(self, plane_client: TestClient):
        resp = plane_client.post(
            "/webhooks/plane", json={"event": "issue", "action": "created"}
        )
        assert resp.status_code == 401

    def test_no_secret_configured_500(
        self, event_queue: MockEventQueue, event_store: MemoryEventStore
    ):
        app = create_app(
            event_queue=event_queue,
            event_store=event_store,
            agent_roles=AGENT_ROLES,
        )
        client = TestClient(app, raise_server_exceptions=False)
        resp = client.post("/webhooks/plane", json={"event": "issue"})
        assert resp.status_code == 500
        assert resp.json() == {"error": "webhook verification not configured"}

    def test_invalid_json_400(self, plane_client: TestClient):
        """A valid signature over non-JSON bytes still 400s."""
        body = b"not json"
        resp = plane_client.post(
            "/webhooks/plane",
            content=body,
            headers={
                "content-type": "application/json",
                "x-plane-signature": self._sign(body),
            },
        )
        assert resp.status_code == 400

    def test_non_ascii_signature_401(self, plane_client: TestClient):
        """A signature header with non-ASCII bytes must 401, not 500.

        Starlette decodes raw header bytes with latin-1, and
        ``hmac.compare_digest`` raises ``TypeError`` on non-ASCII
        ``str`` operands — the shape prefilter must reject the header
        before the comparison ever runs."""
        body = b'{"event": "issue", "action": "created"}'
        resp = plane_client.post(
            "/webhooks/plane",
            content=body,
            headers={
                b"content-type": b"application/json",
                b"x-plane-signature": b"\xff" * 64,
            },
        )
        assert resp.status_code == 401
        assert resp.json() == {"error": "invalid signature"}

    def test_non_hex_signature_401(self, plane_client: TestClient):
        """Garbage that is not a 64-char hexdigest fails the shape
        prefilter and 401s — wrong alphabet, wrong length, or both."""
        body = b'{"event": "issue", "action": "created"}'
        for bad in ("z" * 64, "deadbeef", self._sign(body) + "00", "sha256=abc"):
            resp = plane_client.post(
                "/webhooks/plane",
                content=body,
                headers={
                    "content-type": "application/json",
                    "x-plane-signature": bad,
                },
            )
            assert resp.status_code == 401, f"signature {bad!r} did not 401"

    def test_non_dict_json_body_400(self, plane_client: TestClient):
        """A correctly signed body whose JSON parses to a list (not an
        object) is a 400, not an ``AttributeError`` 500."""
        body = json.dumps([{"event": "issue"}]).encode()
        resp = plane_client.post(
            "/webhooks/plane", content=body, headers=self._headers(body)
        )
        assert resp.status_code == 400
        assert resp.json() == {"error": "invalid JSON"}

    def test_unconfigured_drop_200(
        self, event_queue: MockEventQueue, event_store: MemoryEventStore
    ):
        """A verified delivery to an unconfigured engine is dropped with
        a 200 AFTER signature verification, protecting Plane's five-retry
        auto-disable counter without accepting forgeries.

        ``create_app`` without a ``company_config_store`` defaults
        ``configured`` to True, so the test flips it explicitly."""
        app = create_app(
            event_queue=event_queue,
            event_store=event_store,
            agent_roles=AGENT_ROLES,
            plane_webhook_secret=self.SECRET,
        )
        app.state.configured = False
        client = TestClient(app, raise_server_exceptions=False)
        body = json.dumps({"event": "issue", "action": "created"}).encode()
        resp = client.post("/webhooks/plane", content=body, headers=self._headers(body))
        assert resp.status_code == 200
        assert resp.json() == {"status": "dropped", "reason": "unconfigured"}
        assert event_queue.published == []

        # A forgery is still rejected even while unconfigured.
        resp = client.post(
            "/webhooks/plane",
            content=body,
            headers={
                "content-type": "application/json",
                "x-plane-signature": "bad",
            },
        )
        assert resp.status_code == 401

    def test_summary_builder(self):
        from crewlet.api.routes.webhooks import _build_plane_summary

        assert (
            _build_plane_summary(
                {
                    "event": "issue",
                    "action": "created",
                    "data": {"name": "Fix login flow"},
                    "activity": {"actor": {"display_name": "Priya"}},
                }
            )
            == 'Plane Priya issue created "Fix login flow"'
        )
        # Comment event: no data.name, actor as a bare UUID string.
        assert (
            _build_plane_summary(
                {
                    "event": "issue_comment",
                    "action": "created",
                    "data": {"id": "c-1"},
                    "activity": {"actor": "9f8e7d6c"},
                }
            )
            == "Plane 9f8e7d6c issue_comment created"
        )
        assert _build_plane_summary({}) == "Plane"

    def test_event_type_carries_action(
        self, plane_client: TestClient, event_store: MemoryEventStore
    ):
        """The dashboard event type is ``webhook:{event}.{action}`` so
        create / update / delete stay distinguishable in the feed."""
        body = json.dumps({"event": "issue", "action": "updated"}).encode()
        resp = plane_client.post(
            "/webhooks/plane", content=body, headers=self._headers(body)
        )
        assert resp.status_code == 200
        loop = asyncio.new_event_loop()
        events = loop.run_until_complete(event_store.list_events())
        loop.close()
        assert len(events) == 1
        assert events[0]["type"] == "webhook:issue.updated"


class TestForgeWebhook:
    APP_ID = "ari:cloud:ecosystem::app/test-app"
    AUTH = {"authorization": "Bearer valid-fit-token"}

    @pytest.fixture
    def forge_app(self, event_queue: MockEventQueue):
        return create_app(
            event_queue=event_queue,
            forge_app_id=self.APP_ID,
        )

    @pytest.fixture
    def forge_client(self, forge_app) -> TestClient:
        return TestClient(forge_app)

    @pytest.fixture(autouse=True)
    def _mock_fit(self, monkeypatch):
        """Mock FIT verification — accept 'valid-fit-token', reject others."""

        async def _verify(token, app_id):
            if token == "valid-fit-token":
                return {"aud": app_id, "iss": "forge/invocation-token"}
            return None

        monkeypatch.setattr("crewlet.api.forge_fit.verify_fit", _verify)

    def test_forge_confluence_event_publishes(
        self, forge_client: TestClient, event_queue: MockEventQueue
    ):
        """Confluence events have data in 'content' (page/comment object)."""
        resp = forge_client.post(
            "/webhooks/forge",
            json={
                "eventType": "avi:confluence:created:page",
                "atlassianId": "user-789",
                "content": {
                    "id": "12345",
                    "type": "page",
                    "title": "Test Page",
                    "space": {"key": "ENG", "name": "Engineering"},
                },
                "eventCreatedDate": "2026-04-06T12:00:00Z",
                "selfGenerated": False,
            },
            headers=self.AUTH,
        )
        assert resp.status_code == 200

        assert len(event_queue.published) == 1
        topic, event = event_queue.published[0]
        assert topic == "crewlet.notifications.inbound"
        assert event.type == "raw_webhook"
        assert event.source == "confluence"
        assert event.payload["forge_atlassian_id"] == "user-789"
        # Transformed: content becomes page, legacy event injected
        assert event.payload["body"]["event"] == "page_created"
        assert event.payload["body"]["page"]["id"] == "12345"
        assert event.payload["body"]["userAccountId"] == "user-789"

    def test_forge_jira_event_publishes(
        self, forge_client: TestClient, event_queue: MockEventQueue
    ):
        """Jira events have issue/changelog at top level (not in content)."""
        resp = forge_client.post(
            "/webhooks/forge",
            json={
                "eventType": "avi:jira:created:issue",
                "atlassianId": "user-123",
                "issue": {"key": "PROJ-1", "fields": {"summary": "Test"}},
                "selfGenerated": False,
            },
            headers=self.AUTH,
        )
        assert resp.status_code == 200

        assert len(event_queue.published) == 1
        _, event = event_queue.published[0]
        assert event.source == "jira"
        assert event.payload["body"]["webhookEvent"] == "jira:issue_created"
        assert event.payload["body"]["issue"]["key"] == "PROJ-1"

    def test_forge_unknown_event_ignored(self, forge_client: TestClient):
        resp = forge_client.post(
            "/webhooks/forge",
            json={"eventType": "avi:unknown:thing", "atlassianId": ""},
            headers=self.AUTH,
        )
        assert resp.status_code == 200
        assert resp.json()["status"] == "ignored"

    def test_forge_self_generated_skipped(self, forge_client: TestClient):
        resp = forge_client.post(
            "/webhooks/forge",
            json={
                "eventType": "avi:confluence:created:page",
                "atlassianId": "agent-id",
                "content": {},
                "selfGenerated": True,
            },
            headers=self.AUTH,
        )
        assert resp.status_code == 200
        assert resp.json()["reason"] == "selfGenerated"

    def test_forge_invalid_json(self, forge_client: TestClient):
        resp = forge_client.post(
            "/webhooks/forge",
            content=b"not json",
            headers={"content-type": "application/json", **self.AUTH},
        )
        assert resp.status_code == 400

    def test_forge_headers_redacted(
        self, forge_client: TestClient, event_queue: MockEventQueue
    ):
        """Sensitive headers should be redacted in the published event."""
        resp = forge_client.post(
            "/webhooks/forge",
            json={
                "eventType": "avi:confluence:updated:page",
                "atlassianId": "u1",
                "content": {"id": "1", "type": "page", "title": "X"},
                "selfGenerated": False,
            },
            headers=self.AUTH,
        )
        assert resp.status_code == 200
        _, event = event_queue.published[0]
        assert event.payload["headers"].get("authorization") == "REDACTED"

    def test_forge_invalid_token_rejected(self, forge_client: TestClient):
        """Invalid FIT token rejected."""
        resp = forge_client.post(
            "/webhooks/forge",
            json={"event": "avi:confluence:created:page", "context": {}},
            headers={"authorization": "Bearer bad-token"},
        )
        assert resp.status_code == 401

    def test_forge_missing_auth_rejected(self, forge_client: TestClient):
        """Missing Authorization header rejected."""
        resp = forge_client.post(
            "/webhooks/forge",
            json={"event": "avi:confluence:created:page", "context": {}},
        )
        assert resp.status_code == 401

    def test_forge_no_app_id_configured_rejects(self, client: TestClient):
        """When no forge_app_id configured, endpoint rejects with 500."""
        resp = client.post(
            "/webhooks/forge",
            json={"event": "avi:confluence:created:page", "context": {}},
        )
        assert resp.status_code == 500


class TestClientDisconnect:
    """Senders that hang up before the body is read (e.g. Forge aborting
    a slow delivery) must produce a structured warning + unsent 204, not
    an unhandled ``ClientDisconnect`` traceback."""

    APP_ID = "ari:cloud:ecosystem::app/test-app"

    @staticmethod
    def _scope(
        path: str, headers: list[tuple[bytes, bytes]] | None = None
    ) -> dict[str, Any]:
        return {
            "type": "http",
            "asgi": {"version": "3.0", "spec_version": "2.3"},
            "http_version": "1.1",
            "method": "POST",
            "scheme": "http",
            "path": path,
            "raw_path": path.encode(),
            "query_string": b"",
            "root_path": "",
            "headers": headers or [],
            "client": ("203.0.113.10", 4711),
            "server": ("testserver", 80),
        }

    @staticmethod
    async def _post_disconnected(app: Any, scope: dict[str, Any]) -> list[dict]:
        """Drive the ASGI app with a client that hung up immediately."""
        sent: list[dict] = []

        async def receive() -> dict[str, Any]:
            return {"type": "http.disconnect"}

        async def send(message: dict[str, Any]) -> None:
            sent.append(message)

        await app(scope, receive, send)
        return sent

    async def test_forge_disconnect_logs_instead_of_raising(
        self, event_queue: MockEventQueue
    ):
        app = create_app(event_queue=event_queue, forge_app_id=self.APP_ID)
        sent = await self._post_disconnected(
            app,
            self._scope(
                "/webhooks/forge",
                headers=[(b"authorization", b"Bearer valid-fit-token")],
            ),
        )
        assert sent[0]["type"] == "http.response.start"
        assert sent[0]["status"] == 204
        assert event_queue.published == []

    async def test_forge_body_read_precedes_fit_verification(
        self, event_queue: MockEventQueue, monkeypatch
    ):
        """An aborted delivery must die at the body read, before the
        (potentially slow, JWKS-fetching) FIT verification runs."""
        calls: list[str] = []

        async def _verify(token: str, app_id: str) -> dict[str, Any]:
            calls.append(token)
            return {"aud": app_id}

        monkeypatch.setattr("crewlet.api.forge_fit.verify_fit", _verify)
        app = create_app(event_queue=event_queue, forge_app_id=self.APP_ID)
        sent = await self._post_disconnected(
            app,
            self._scope(
                "/webhooks/forge",
                headers=[(b"authorization", b"Bearer valid-fit-token")],
            ),
        )
        assert sent[0]["status"] == 204
        assert calls == []

    async def test_disconnect_handled_under_auth_middleware(
        self, event_queue: MockEventQueue
    ):
        """Production mounts ApiAuthMiddleware (a BaseHTTPMiddleware);
        the disconnect must be handled inside it, not re-raised
        through ``call_next``."""
        store = MagicMock()
        store.get_active = AsyncMock(return_value=None)
        bootstrap = BootstrapConfig(
            api=ApiConfig(
                auth=ApiAuthConfig(tokens=[ApiAuthTokenConfig(id="t", token="s")])
            )
        )
        app = create_app(
            event_queue=event_queue,
            forge_app_id=self.APP_ID,
            bootstrap=bootstrap,
            company_config_store=store,
        )
        sent = await self._post_disconnected(
            app,
            self._scope(
                "/webhooks/forge",
                headers=[(b"authorization", b"Bearer valid-fit-token")],
            ),
        )
        assert sent[0]["type"] == "http.response.start"
        assert sent[0]["status"] == 204

    async def test_jira_disconnect_logs_instead_of_raising(
        self, event_queue: MockEventQueue
    ):
        """The handler is app-level — it covers every body-reading route."""
        app = create_app(event_queue=event_queue)
        sent = await self._post_disconnected(app, self._scope("/webhooks/jira"))
        assert sent[0]["status"] == 204
        assert event_queue.published == []


# ---------------------------------------------------------------------------
# Webhook events are persisted to event store
# ---------------------------------------------------------------------------


class TestWebhookPersistence:
    def test_jira_webhook_persists_event(
        self, client: TestClient, event_store: MemoryEventStore
    ):
        client.post(
            "/webhooks/jira",
            json={"webhookEvent": "jira:issue_created"},
        )
        loop = asyncio.new_event_loop()
        events = loop.run_until_complete(event_store.list_events())
        loop.close()
        assert len(events) == 1
        assert events[0]["category"] == "webhook"

    def test_github_webhook_persists_event(
        self, event_queue: MockEventQueue, event_store: MemoryEventStore
    ):
        secret = "persist-test-secret"
        app = create_app(
            event_queue=event_queue,
            event_store=event_store,
            agent_roles=AGENT_ROLES,
            github_webhook_secret=secret,
        )
        gh_client = TestClient(app, raise_server_exceptions=False)
        body = json.dumps({"action": "opened"}).encode()
        sig = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
        gh_client.post(
            "/webhooks/github",
            content=body,
            headers={
                "content-type": "application/json",
                "x-hub-signature-256": sig,
                "x-github-event": "push",
            },
        )
        loop = asyncio.new_event_loop()
        events = loop.run_until_complete(event_store.list_events())
        loop.close()
        assert len(events) == 1
        assert "webhook:push" in events[0]["type"]


# ---------------------------------------------------------------------------
# Engine command handler dispatch (unit test, no HTTP)
# ---------------------------------------------------------------------------


def _make_engine():
    """Create a minimal Engine for command handler tests."""
    from crewlet import Engine, Organization, OrgUnit, Role

    org = Organization(
        name="Test Co",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Lead",
                roles=[Role(name="Lead", goal="Lead")],
            )
        ],
    )
    return Engine(organization=org)


class TestBuildToolsData:
    """Test Engine._build_tools_data produces correct tool metadata."""

    def test_builtin_tools_tagged_for_all_roles(self):
        engine = _make_engine()
        tools_data = engine._build_tools_data()
        assert len(tools_data) > 0
        # All builtin tools should have source "builtin"
        for tool in tools_data:
            assert tool["source"] == "builtin"
            assert "Lead" in tool["roles"]
            assert tool["name"]
            assert tool["description"]

    def test_role_mcp_tools_included(self):
        from crewlet import Engine, Organization, OrgUnit, Role
        from crewlet.tools.protocol import AgentContext, ToolResult
        from crewlet.tools.registry import SimpleTool

        org = Organization(
            name="Test Co",
            units=[
                OrgUnit(
                    name="Core",
                    type="team",
                    lead="Lead",
                    roles=[
                        Role(name="Lead", goal="Lead"),
                        Role(name="Dev", goal="Code"),
                    ],
                )
            ],
        )

        async def _noop(params: dict, ctx: AgentContext) -> ToolResult:
            return ToolResult(output="ok")

        role_tool = SimpleTool(
            name="special_mcp_tool",
            description="A role-specific tool",
            parameters={"type": "object", "properties": {}},
            fn=_noop,
        )
        engine = Engine(organization=org)
        engine._role_mcp_tools["Lead"] = [role_tool]

        tools_data = engine._build_tools_data()
        names = {t["name"] for t in tools_data}
        assert "special_mcp_tool" in names

        special = next(t for t in tools_data if t["name"] == "special_mcp_tool")
        assert "Lead" in special["roles"]
        assert "Dev" not in special["roles"]
