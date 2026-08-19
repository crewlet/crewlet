"""Test-only wrapper that seeds the state production derives from config.

``crewlet.api.app.create_app`` deliberately takes no ``agent_roles`` /
``org_data`` / ``schedules_data`` / ``tools_data`` / ``engine`` arguments:
every one of those is derived from the active config revision by
``attach_config_refresh``, on the embedded path exactly as on the
standalone one.  Letting a caller inject a boot-time snapshot instead is
what made the two deployments the same surface with two implementations.

Tests still need to put a known company in front of a route without
standing up a config store, so they seed ``app.state`` directly — the
same fields, set the same way the refresh path sets them, just from a
literal.  Keeping that in one test helper (rather than as optional
production parameters) means the production factory has exactly one way
to be wired.
"""

from __future__ import annotations

from typing import Any

from crewlet.api.app import create_app as _create_app


class FakeNodeRuntime:
    """A :class:`~crewlet.api.runtime.NodeRuntime` for tests.

    Structural, not a subclass — that is the point of the protocol.
    """

    def __init__(
        self,
        *,
        in_flight: int = 0,
        shutting_down: bool = False,
        tools: list[dict[str, Any]] | None = None,
        posture: str = "serve",
        applied_epoch: int = 0,
        seats: dict[str, Any] | None = None,
    ) -> None:
        self._in_flight = in_flight
        self._shutting_down = shutting_down
        self._tools = tools or []
        self._posture = posture
        self._applied_epoch = applied_epoch
        self._seats = seats or {}

    def seats(self) -> dict[str, Any]:
        return dict(self._seats)

    @property
    def in_flight(self) -> int:
        return self._in_flight

    @property
    def shutting_down(self) -> bool:
        return self._shutting_down

    @property
    def posture(self) -> str:
        return self._posture

    @property
    def applied_epoch(self) -> int:
        return self._applied_epoch

    def tools_data(self) -> list[dict[str, Any]]:
        return list(self._tools)


def create_app(
    event_queue: Any,
    *,
    agent_roles: list[dict[str, Any]] | None = None,
    org_data: dict[str, Any] | None = None,
    schedules_data: list[dict[str, Any]] | None = None,
    tools_data: list[dict[str, Any]] | None = None,
    engine: Any = None,
    **kwargs: Any,
) -> Any:
    """Build an app and seed the config-derived state fields.

    ``engine`` is accepted for the tests that assert engine-backed
    ``/health`` fields; it is wrapped in the real
    :class:`~crewlet.api.runtime.EngineNodeRuntime` adapter so those
    tests exercise the production seam rather than a stand-in.
    """
    runtime = kwargs.pop("runtime", None)
    if runtime is None and engine is not None:
        from crewlet.api.runtime import EngineNodeRuntime

        runtime = EngineNodeRuntime(engine)

    app = _create_app(event_queue, runtime=runtime, **kwargs)

    if agent_roles is not None:
        app.state.agent_roles = agent_roles
    if org_data is not None:
        app.state.org_data = org_data
    if schedules_data is not None:
        app.state.schedules_data = schedules_data
    if tools_data is not None:
        app.state.tools_data = tools_data
    return app
