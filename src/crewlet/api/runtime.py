"""The one seam between the API and a co-located engine.

The API is written against exactly one wiring: it learns the company from
the active config revision (``attach_config_refresh``) and learns what is
happening from the broadcast event stream (``subscribe_stream``).  That
holds whether it runs inside the engine process or on its own host, and
keeping it true is the whole point — the embedded and standalone paths
used to be two different implementations of the same surface, and all
three bugs that shipped in that surface came from the divergence:

- the standalone API was constructed without an OTLP receiver, so every
  in-sandbox trace 503'd;
- the embedded API was fed by ``add_publish_listener``, which sees only
  its own process's publishes;
- ``crewlet run api`` applied the pgvector migrations at a guessed width.

A few facts genuinely cannot be learned either way, because they are
properties of a live engine object in this process: how many turns are in
flight, whether a drain is under way, and what the *actual* per-role MCP
tool surface is (the config payload names servers, not the tools they
turned out to expose).  Those come through :class:`NodeRuntime` — one
optional object, registered by the embedded path and absent otherwise —
rather than through a handful of parameters that would drift apart again.
"""

from __future__ import annotations

from typing import Any, Protocol, runtime_checkable


@runtime_checkable
class NodeRuntime(Protocol):
    """Live in-process facts only a co-located engine can answer.

    Every method must be cheap and non-raising: they are called from
    ``/health`` and from the shared health tick that fans out to every
    connected dashboard.
    """

    @property
    def in_flight(self) -> int:
        """Turns currently executing in this process."""
        ...

    @property
    def shutting_down(self) -> bool:
        """Whether a graceful drain is under way."""
        ...

    @property
    def started_at(self) -> str:
        """When the co-located engine started, ISO-8601, or ``""``.

        Deliberately separate from the API process's own ``started_at``:
        split across two processes they are two clocks, and one merged
        "uptime" would be the two-different-windows error in a new
        place.
        """
        ...

    @property
    def posture(self) -> str:
        """This node's config posture — see :mod:`crewlet.db.config_plane`.

        ``"serve"`` on a converged node.  Anything else is the node
        telling a load balancer, and an operator, what it has concluded
        about its own lag.
        """
        ...

    @property
    def applied_epoch(self) -> int:
        """The activation epoch this node has actually applied."""
        ...

    def tools_data(self) -> list[dict[str, Any]]:
        """The live tool surface: builtins + per-role MCP tools.

        Richer than what the config payload can reveal, because MCP tools
        are discovered from the running servers.  Returning ``[]`` is
        valid — the caller falls back to the payload-derived view.
        """
        ...

    def seats(self) -> dict[str, Any]:
        """Which seats this node owns, and how it arrived at that.

        The first question about any fleet, and until this existed the
        only way to answer it was to read three processes' logs at DEBUG.
        Shape::

            {"held": ["ceo", "eng"], "capacity": 3, "live_nodes": 2,
             "last_claimed": [...], "last_lost": [...],
             "unproven": [...], "blocked_by_protocol": 1 | None}

        ``unproven`` is the one to alert on: each entry is a seat whose
        teardown could not be proven, so this node holds a lease no peer
        can take while possibly still consuming the seat.
        ``blocked_by_protocol`` names the fleet's protocol floor when a
        rolling upgrade has stalled — without it, a node refusing to
        claim looks identical to one whose peers hold every seat.

        ``{}`` when this node does no placement at all.
        """
        ...


class EngineNodeRuntime:
    """:class:`NodeRuntime` over a live :class:`~crewlet.engine.Engine`.

    Registered by the embedded API path. Every accessor is defensive: the
    engine may still be mid-spawn when the first health tick or config
    refresh lands, and a dashboard poll must never take the process down.
    """

    __slots__ = ("_engine",)

    def __init__(self, engine: Any) -> None:
        self._engine = engine

    @property
    def engine(self) -> Any:
        """The wrapped engine (for callers that legitimately need it)."""
        return self._engine

    @property
    def in_flight(self) -> int:
        try:
            return int(self._engine.in_flight_count)
        except Exception:
            return 0

    @property
    def shutting_down(self) -> bool:
        try:
            return bool(self._engine.shutting_down or not self._engine.is_running)
        except Exception:
            return False

    @property
    def started_at(self) -> str:
        return str(getattr(self._engine, "started_at", "") or "")

    @property
    def posture(self) -> str:
        try:
            return str(self._engine.posture)
        except Exception:
            # Unknown lag is not evidence of lag — the same rule
            # ``decide_posture`` follows.  Reporting "serve" here keeps a
            # transient read failure from pulling a healthy node out of
            # rotation.
            return "serve"

    @property
    def applied_epoch(self) -> int:
        try:
            return int(self._engine._applied_epoch)
        except Exception:
            return 0

    def tools_data(self) -> list[dict[str, Any]]:
        try:
            return list(self._engine._build_tools_data())
        except Exception:
            # The spawn cascade runs after the first config refresh, so
            # an early call is expected rather than exceptional.
            return []

    def seats(self) -> dict[str, Any]:
        try:
            host = self._engine._seat_host
            if host is None:
                return {}
            last = host.last_sweep
            return {
                "held": list(host.held_handles),
                "unproven": list(host.unproven_handles),
                "capacity": int(last.capacity) if last is not None else 0,
                "live_nodes": int(last.live_nodes) if last is not None else 0,
                "last_claimed": list(last.claimed) if last is not None else [],
                "last_lost": list(last.lost) if last is not None else [],
                "blocked_by_protocol": (
                    last.blocked_by_protocol if last is not None else None
                ),
                "draining": bool(host._draining),
            }
        except Exception:
            return {}
