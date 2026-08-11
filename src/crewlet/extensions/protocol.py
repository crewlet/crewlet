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
