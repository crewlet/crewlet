"""The embedded and standalone APIs must be ONE wiring, not two.

All three bugs this phase retires came from those paths being different
implementations of the same surface: the standalone process built no OTLP
receiver (503 on every in-sandbox trace), the embedded one was fed by
``add_publish_listener`` (blind to any peer's events), and ``crewlet run
api`` applied the pgvector migrations at a guessed width.

These are the guards that stop the fork growing back.
"""

from __future__ import annotations

import inspect
from typing import Any

import pytest
from starlette.testclient import TestClient

from crewlet.api.app import create_app
from crewlet.api.runtime import EngineNodeRuntime, NodeRuntime
from crewlet.api.streaming import build_health_envelope, build_readiness_envelope
from tests.test_api.helpers import FakeNodeRuntime


class _Queue:
    def __init__(self) -> None:
        self.published: list[Any] = []
        self.streams: list[tuple[str, Any]] = []
        self.listeners: list[Any] = []

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))

    async def subscribe_stream(self, pattern: str, handler: Any) -> Any:
        self.streams.append((pattern, handler))

        async def _unsub() -> None:
            return None

        return _unsub

    def add_publish_listener(self, handler: Any) -> None:
        self.listeners.append(handler)


# --- the factory takes no boot-time snapshots ------------------------------


@pytest.mark.parametrize(
    "forbidden",
    ["agent_roles", "org_data", "schedules_data", "tools_data", "engine"],
)
def test_create_app_has_no_embedded_only_parameters(forbidden: str) -> None:
    """Every one of these was a snapshot the engine passed and the
    standalone process derived. Re-adding any of them — even "just for
    compatibility" — recreates the divergence, so it fails here first."""
    assert forbidden not in inspect.signature(create_app).parameters


def test_config_derived_state_starts_empty() -> None:
    """Nothing is seeded at construction: ``attach_config_refresh`` is the
    only thing that fills these, on both paths."""
    app = create_app(_Queue())
    assert app.state.agent_roles == []
    assert app.state.org_data == {}
    assert app.state.schedules_data == []
    assert app.state.tools_data == []


# --- the runtime seam ------------------------------------------------------


def test_engine_adapter_satisfies_the_protocol_structurally() -> None:
    class _Engine:
        in_flight_count = 3
        shutting_down = False
        is_running = True

        def _build_tools_data(self) -> list[dict[str, Any]]:
            return [{"name": "t"}]

    runtime = EngineNodeRuntime(_Engine())
    assert isinstance(runtime, NodeRuntime)
    assert runtime.in_flight == 3
    assert runtime.shutting_down is False
    assert runtime.tools_data() == [{"name": "t"}]


def test_engine_adapter_is_defensive_mid_spawn() -> None:
    """The first health tick can land before the spawn cascade finishes. A
    dashboard poll must never take the process down."""

    class _Broken:
        @property
        def in_flight_count(self) -> int:
            raise RuntimeError("not ready")

        @property
        def shutting_down(self) -> bool:
            raise RuntimeError("not ready")

        def _build_tools_data(self) -> list[dict[str, Any]]:
            raise RuntimeError("not ready")

    runtime = EngineNodeRuntime(_Broken())
    assert runtime.in_flight == 0
    assert runtime.shutting_down is False
    assert runtime.tools_data() == []


# --- /health and /ready ----------------------------------------------------


def test_health_omits_runtime_fields_without_a_runtime() -> None:
    app = create_app(_Queue())
    body = build_health_envelope(app)
    assert body["status"] == "ok"
    assert "in_flight" not in body
    assert "shutting_down" not in body


def test_health_reports_runtime_fields_when_registered() -> None:
    app = create_app(_Queue(), runtime=FakeNodeRuntime(in_flight=2))
    body = build_health_envelope(app)
    assert body["in_flight"] == 2
    assert body["shutting_down"] is False


def test_health_names_the_answering_node() -> None:
    """Behind a load balancer this is the only way a caller can tell which
    process replied."""
    app = create_app(_Queue())
    assert build_health_envelope(app)["node"]


def test_health_stays_200_through_a_drain_but_ready_goes_503() -> None:
    """The split exists for exactly this: an orchestrator watching liveness
    must not SIGKILL a node that is finishing multi-minute turns, while the
    load balancer must stop sending it new work immediately."""
    app = create_app(_Queue(), runtime=FakeNodeRuntime(shutting_down=True))
    app.state.configured = True

    with TestClient(app) as client:
        assert client.get("/health").status_code == 200
        assert client.get("/health").json()["status"] == "shutting_down"

        ready = client.get("/ready")
        assert ready.status_code == 503
        assert ready.json()["draining"] is True


def test_ready_is_503_before_the_first_revision_applies() -> None:
    """An unconfigured node cannot verify a webhook signature, so it should
    not be in rotation to receive one."""
    app = create_app(_Queue())
    app.state.configured = False
    body, status = build_readiness_envelope(app)
    assert status == 503
    assert body["ready"] is False
    assert body["configured"] is False


def test_ready_is_200_once_configured_and_not_draining() -> None:
    app = create_app(_Queue(), runtime=FakeNodeRuntime())
    app.state.configured = True
    body, status = build_readiness_envelope(app)
    assert status == 200
    assert body["ready"] is True


def test_health_reports_which_seats_this_node_serves() -> None:
    """The first question about any fleet.

    Until this shipped the only way to answer "who is running the CEO
    right now?" was to read three processes' logs at DEBUG.
    """
    app = create_app(
        _Queue(),
        runtime=FakeNodeRuntime(
            seats={
                "held": ["ceo", "eng"],
                "unproven": [],
                "capacity": 2,
                "live_nodes": 3,
                "last_claimed": ["eng"],
                "last_lost": [],
                "blocked_by_protocol": None,
                "draining": False,
            }
        ),
    )
    body = build_health_envelope(app)
    assert body["seats"]["held"] == ["ceo", "eng"]
    assert body["seats"]["live_nodes"] == 3
    assert body["seats"]["last_claimed"] == ["eng"]


def test_health_surfaces_a_stalled_rolling_upgrade() -> None:
    """A node refusing to claim under the mixed-version gate looks
    identical to one whose peers hold every seat — unless it says so."""
    app = create_app(
        _Queue(),
        runtime=FakeNodeRuntime(seats={"held": [], "blocked_by_protocol": 1}),
    )
    assert build_health_envelope(app)["seats"]["blocked_by_protocol"] == 1


def test_health_surfaces_seats_whose_teardown_was_never_proven() -> None:
    """The one to alert on: each entry is a seat this node may still be
    consuming while holding a lease no peer can take."""
    app = create_app(_Queue(), runtime=FakeNodeRuntime(seats={"unproven": ["ceo"]}))
    assert build_health_envelope(app)["seats"]["unproven"] == ["ceo"]


def test_health_omits_seats_on_a_node_that_does_no_placement() -> None:
    app = create_app(_Queue(), runtime=FakeNodeRuntime())
    assert "seats" not in build_health_envelope(app)
