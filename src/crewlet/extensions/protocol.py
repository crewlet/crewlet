"""Extension protocol — interface for engine extensions."""

from __future__ import annotations

from typing import Any, Protocol, runtime_checkable

from pydantic import BaseModel, ConfigDict


class ExtensionContext(BaseModel):
    """Context provided to extensions during lifecycle hooks.

    Gives extensions access to engine subsystems without
    requiring a direct Engine import (avoids circular deps).

    Uses Any types because the subsystems are defined in separate
    modules and we need to avoid circular imports. Extensions
    should cast/access attributes as needed.
    """

    model_config = ConfigDict(arbitrary_types_allowed=True)

    event_queue: Any = None
    agent_pool: Any = None
    execution_tracker: Any = None
    tool_registry: Any = None
    role_mcp_tools: Any = None
    storage: Any = None
    notification_service: Any = None
    org: Any = None
    observability: Any = None
    debug: bool = False

    node_id: str = ""
    """Which process this is. Stable across restarts, and the same value
    the logs, ``/health`` and the lease table use."""

    claim_duty: Any = None
    """``await ctx.claim_duty("my-extension")`` → may this node do a
    company-wide job right now?

    Extensions run on **every** node — ``on_engine_start`` fires in each
    process — so an extension that polls an API, writes a daily digest,
    or reconciles an external system does it N times unless it asks. This
    is the same primitive the engine's own singleton duties use, under
    the same name, and it takes an arbitrary duty string so two
    extensions never collide.

    **Claim per tick, never once at start.** A claim is a short lease, so
    holding one from ``on_engine_start`` means the node that happened to
    boot first owns the job for the life of the process — including after
    it stops being able to do it. Ask again each time you are about to
    act; a node that dies mid-duty hands it back by lapsing, with no
    handoff protocol.

    ``None`` when the engine did not wire one (a bare
    ``ExtensionContext`` in a test). Treat that as "yes": a fleet of one
    is what an unwired engine is.

    See ``docs/guides/extensions.md`` and
    ``docs/concepts/seat-ownership.md#singleton-duties``."""


@runtime_checkable
class Extension(Protocol):
    """Protocol for engine extensions."""

    @property
    def name(self) -> str: ...

    @property
    def version(self) -> str: ...

    async def on_register(self, ctx: ExtensionContext) -> None:
        """Called when extension is registered with the engine.

        Use to register event handlers, middleware, tools, etc.
        """
        ...

    async def on_engine_start(self, ctx: ExtensionContext) -> None:
        """Called after the engine has fully started."""
        ...

    async def on_engine_stop(self, ctx: ExtensionContext) -> None:
        """Called before the engine shuts down."""
        ...
