"""Tests for the token-spend aggregation helper and ``/tokens/breakdown``.

The aggregator (:func:`crewlet.api.tokens.aggregate_phase_events`) is
covered in isolation so the maths is reviewable without Starlette
plumbing.  The endpoint test then verifies the wiring end-to-end via a
``MemoryEventStore`` seeded with real ``agent_phase_completed`` rows.
"""

from __future__ import annotations

import asyncio
from datetime import UTC, datetime

import pytest
from starlette.testclient import TestClient

from crewlet.api.app import create_app
from crewlet.api.tokens import aggregate_phase_events
from crewlet.timescaledb.memory import MemoryEventStore

# --------------------------------------------------------------------------- #
# Aggregator unit tests
# --------------------------------------------------------------------------- #


def _phase_event(
    *,
    phase: str,
    role: str = "PM",
    agent_id: str = "a-1",
    turn_id: str = "turn-1",
    model: str = "claude-sonnet-4-6",
    worker: str = "",
    tokens_in: int = 100,
    tokens_out: int = 50,
    timestamp: str = "2026-05-16T10:00:00+00:00",
) -> dict:
    return {
        "event_id": f"{phase}-{turn_id}",
        "timestamp": timestamp,
        "agent_id": agent_id,
        "agent_role": role,
        "phase": phase,
        "host_phase": "",
        "worker": worker,
        "model": model,
        "turn_id": turn_id,
        "iteration": 0,
        "input_tokens": tokens_in,
        "output_tokens": tokens_out,
        "total_tokens": tokens_in + tokens_out,
        "tool_executions": [],
    }


class TestAggregator:
    def test_empty_input_produces_zero_totals(self) -> None:
        result = aggregate_phase_events([])
        assert result["totals"] == {
            "input_tokens": 0,
            "output_tokens": 0,
            "total_tokens": 0,
            "calls": 0,
        }
        assert result["by_phase"] == []
        assert result["by_model"] == []
        assert result["by_worker"] == []
        assert result["by_agent"] == []
        assert result["by_turn"] == []
        # No events → no watermark, so the dashboard folds every live
        # phase event onto this empty baseline.
        assert result["aggregated_through"] == ""

    def test_aggregated_through_is_latest_event_timestamp(self) -> None:
        """The watermark lets the dashboard fold live phase events onto the
        fetched baseline without double-counting the ones it already saw."""
        events = [
            _phase_event(phase="plan", timestamp="2026-05-16T10:00:00+00:00"),
            _phase_event(
                phase="execute",
                turn_id="t-1",
                timestamp="2026-05-16T10:00:30+00:00",
            ),
            _phase_event(
                phase="review",
                turn_id="t-1",
                timestamp="2026-05-16T10:00:15+00:00",
            ),
        ]
        result = aggregate_phase_events(events)
        assert result["aggregated_through"] == "2026-05-16T10:00:30+00:00"

    def test_totals_sum_across_all_events(self) -> None:
        events = [
            _phase_event(phase="plan", tokens_in=100, tokens_out=50),
            _phase_event(
                phase="execute",
                turn_id="turn-1",
                tokens_in=200,
                tokens_out=80,
            ),
        ]
        result = aggregate_phase_events(events)
        assert result["totals"]["input_tokens"] == 300
        assert result["totals"]["output_tokens"] == 130
        assert result["totals"]["total_tokens"] == 430
        assert result["totals"]["calls"] == 2

    def test_by_phase_groups_and_sorts_by_spend(self) -> None:
        """Phases ordered by total_tokens descending so the chart is readable."""
        events = [
            _phase_event(phase="plan", tokens_in=10, tokens_out=5),
            _phase_event(phase="execute", turn_id="t-1", tokens_in=200, tokens_out=80),
            _phase_event(phase="execute", turn_id="t-2", tokens_in=50, tokens_out=20),
        ]
        result = aggregate_phase_events(events)
        phases = result["by_phase"]
        assert phases[0]["phase"] == "execute"
        assert phases[0]["total_tokens"] == 350
        assert phases[0]["calls"] == 2
        assert phases[1]["phase"] == "plan"
        assert phases[1]["total_tokens"] == 15

    def test_by_worker_only_counts_auxiliary_phase(self) -> None:
        """``worker`` is meaningful only when ``phase == 'auxiliary'``.

        Other phases leave ``worker`` blank; the rollup must not create a
        spurious blank-worker row.
        """
        events = [
            _phase_event(phase="plan", worker="", tokens_in=10, tokens_out=5),
            _phase_event(
                phase="auxiliary",
                worker="persist_decider",
                turn_id="t-1",
                tokens_in=20,
                tokens_out=4,
            ),
            _phase_event(
                phase="auxiliary",
                worker="counterparty_profiler",
                turn_id="t-2",
                tokens_in=30,
                tokens_out=6,
            ),
        ]
        result = aggregate_phase_events(events)
        workers = {w["worker"]: w for w in result["by_worker"]}
        assert set(workers) == {"persist_decider", "counterparty_profiler"}
        assert workers["counterparty_profiler"]["total_tokens"] == 36

    def test_by_agent_carries_per_phase_split_and_handle(self) -> None:
        events = [
            _phase_event(phase="plan", role="PM", tokens_in=10, tokens_out=5),
            _phase_event(
                phase="execute",
                role="PM",
                turn_id="t-1",
                tokens_in=100,
                tokens_out=40,
            ),
            _phase_event(
                phase="plan",
                role="Dev",
                agent_id="a-2",
                turn_id="t-2",
                tokens_in=20,
                tokens_out=10,
            ),
        ]
        result = aggregate_phase_events(
            events, role_handle_map={"PM": "pm", "Dev": "dev"}
        )
        agents = {a["role"]: a for a in result["by_agent"]}
        assert agents["PM"]["handle"] == "pm"
        assert agents["PM"]["total_tokens"] == 155
        assert agents["PM"]["by_phase"]["plan"]["total_tokens"] == 15
        assert agents["PM"]["by_phase"]["execute"]["total_tokens"] == 140
        assert agents["Dev"]["handle"] == "dev"
        assert agents["Dev"]["total_tokens"] == 30
        # Sorted by spend descending.
        assert result["by_agent"][0]["role"] == "PM"

    def test_by_turn_tracks_window_and_phase_split(self) -> None:
        events = [
            _phase_event(
                phase="plan",
                turn_id="t-1",
                tokens_in=10,
                tokens_out=5,
                timestamp="2026-05-16T10:00:00+00:00",
            ),
            _phase_event(
                phase="execute",
                turn_id="t-1",
                tokens_in=100,
                tokens_out=40,
                timestamp="2026-05-16T10:00:15+00:00",
            ),
            _phase_event(
                phase="review",
                turn_id="t-1",
                tokens_in=20,
                tokens_out=8,
                timestamp="2026-05-16T10:00:30+00:00",
            ),
            _phase_event(
                phase="plan",
                turn_id="t-2",
                tokens_in=12,
                tokens_out=4,
                timestamp="2026-05-16T11:00:00+00:00",
            ),
        ]
        result = aggregate_phase_events(events)
        turns = {t["turn_id"]: t for t in result["by_turn"]}
        assert turns["t-1"]["started_at"] == "2026-05-16T10:00:00+00:00"
        assert turns["t-1"]["ended_at"] == "2026-05-16T10:00:30+00:00"
        assert turns["t-1"]["total_tokens"] == 183
        assert turns["t-1"]["by_phase"]["plan"]["total_tokens"] == 15
        assert turns["t-1"]["by_phase"]["execute"]["total_tokens"] == 140
        assert turns["t-1"]["by_phase"]["review"]["total_tokens"] == 28
        # Newest first.
        assert result["by_turn"][0]["turn_id"] == "t-2"

    def test_recent_turns_limit_caps_the_per_turn_list(self) -> None:
        events = [
            _phase_event(
                phase="plan",
                turn_id=f"t-{i}",
                timestamp=f"2026-05-16T10:{i:02d}:00+00:00",
            )
            for i in range(20)
        ]
        result = aggregate_phase_events(events, recent_turns_limit=5)
        assert len(result["by_turn"]) == 5
        # Newest 5 turns kept (t-19 .. t-15).
        assert result["by_turn"][0]["turn_id"] == "t-19"
        assert result["by_turn"][-1]["turn_id"] == "t-15"


# --------------------------------------------------------------------------- #
# Endpoint integration
# --------------------------------------------------------------------------- #


class _MockEventQueue:
    async def publish(self, *_args, **_kwargs) -> None:
        pass

    async def subscribe(self, *_args, **_kwargs) -> None:
        pass

    async def start(self) -> None:
        pass

    async def stop(self) -> None:
        pass


@pytest.fixture
def event_store() -> MemoryEventStore:
    store = MemoryEventStore()
    loop = asyncio.new_event_loop()
    loop.run_until_complete(store.start())
    loop.close()
    return store


@pytest.fixture
def client(event_store: MemoryEventStore) -> TestClient:
    app = create_app(
        event_queue=_MockEventQueue(),
        event_store=event_store,
        agent_roles=[
            {"id": "a-1", "role": "PM", "handle": "pm"},
            {"id": "a-2", "role": "Dev", "handle": "dev"},
        ],
    )
    return TestClient(app)


def _seed_phase_event(
    store: MemoryEventStore,
    *,
    event_id: str,
    role: str,
    agent_id: str,
    phase: str,
    model: str = "m",
    turn_id: str = "t-1",
    tokens_in: int = 10,
    tokens_out: int = 5,
) -> None:
    asyncio.run(
        store.write_event(
            event_id=event_id,
            event_type="agent_phase_completed",
            source=f"agent-{role.lower()}",
            timestamp=datetime.now(UTC),
            payload={
                "phase": phase,
                "model": model,
                "turn_id": turn_id,
                "iteration": 0,
                "input_tokens": tokens_in,
                "output_tokens": tokens_out,
                "total_tokens": tokens_in + tokens_out,
            },
            tags={"agent_id": agent_id, "agent_role": role},
        )
    )


class TestTokensBreakdownEndpoint:
    def test_breakdown_without_store_returns_empty_skeleton(self) -> None:
        app = create_app(
            event_queue=_MockEventQueue(),
            agent_roles=[{"id": "a-1", "role": "PM", "handle": "pm"}],
        )
        client = TestClient(app)
        resp = client.get("/tokens/breakdown")
        assert resp.status_code == 200
        body = resp.json()
        assert body["totals"]["total_tokens"] == 0
        assert body["by_phase"] == []
        assert body["by_agent"] == []
        assert body["aggregated_through"] == ""

    def test_breakdown_aggregates_phase_events(
        self, client: TestClient, event_store: MemoryEventStore
    ) -> None:
        _seed_phase_event(
            event_store,
            event_id="p1",
            role="PM",
            agent_id="rt-pm",
            phase="plan",
            tokens_in=10,
            tokens_out=5,
        )
        _seed_phase_event(
            event_store,
            event_id="p2",
            role="PM",
            agent_id="rt-pm",
            phase="execute",
            tokens_in=100,
            tokens_out=40,
        )
        _seed_phase_event(
            event_store,
            event_id="p3",
            role="Dev",
            agent_id="rt-dev",
            phase="plan",
            turn_id="t-2",
            tokens_in=20,
            tokens_out=8,
        )

        resp = client.get("/tokens/breakdown")
        assert resp.status_code == 200
        body = resp.json()
        assert body["totals"]["total_tokens"] == 183
        assert body["totals"]["calls"] == 3
        # The watermark is populated from real phase rows so the dashboard
        # can fold subsequent live events onto this baseline.
        assert body["aggregated_through"]
        phases = {p["phase"]: p for p in body["by_phase"]}
        assert phases["plan"]["total_tokens"] == 43
        assert phases["execute"]["total_tokens"] == 140
        agents = {a["role"]: a for a in body["by_agent"]}
        assert agents["PM"]["handle"] == "pm"
        assert agents["PM"]["by_phase"]["execute"]["total_tokens"] == 140

    def test_breakdown_filters_by_agent_role(
        self, client: TestClient, event_store: MemoryEventStore
    ) -> None:
        _seed_phase_event(
            event_store,
            event_id="p1",
            role="PM",
            agent_id="rt-pm",
            phase="plan",
            tokens_in=10,
            tokens_out=5,
        )
        _seed_phase_event(
            event_store,
            event_id="p2",
            role="Dev",
            agent_id="rt-dev",
            phase="plan",
            turn_id="t-2",
            tokens_in=20,
            tokens_out=8,
        )
        resp = client.get("/tokens/breakdown?agent_role=PM")
        assert resp.status_code == 200
        body = resp.json()
        assert body["agent_role"] == "PM"
        assert body["totals"]["total_tokens"] == 15
        agents = {a["role"]: a for a in body["by_agent"]}
        assert set(agents) == {"PM"}

    def test_breakdown_clamps_since_days(self, client: TestClient) -> None:
        """``since_days`` is bounded to [1, 30] to keep queries cheap."""
        resp = client.get("/tokens/breakdown?since_days=500")
        assert resp.status_code == 200
        assert resp.json()["since_days"] == 30
        resp = client.get("/tokens/breakdown?since_days=-5")
        assert resp.status_code == 200
        assert resp.json()["since_days"] == 1
