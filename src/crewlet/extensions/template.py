"""Community extension template — copy this to create your own extension.

To create a custom extension:

1. Copy this file and rename the class.
2. Implement the lifecycle hooks you need.
3. Pass your extension to Engine(extensions=[YourExtension()]).

Example:
    class MetricsExtension:
        @property
        def name(self) -> str:
            return "metrics"

        @property
        def version(self) -> str:
            return "1.0.0"

        async def on_register(self, ctx):
            await ctx.event_queue.subscribe(
                "crewlet.events.task_completed",
                "metrics",
                self._on_task_done,
            )

        async def on_engine_start(self, ctx):
            print(f"Metrics tracking started")

        async def on_engine_stop(self, ctx):
            print(f"Metrics tracking stopped")

        async def _on_task_done(self, event):
            print(f"Task completed: {event.task_id}")
"""

from __future__ import annotations

from crewlet.extensions.protocol import ExtensionContext


class TemplateExtension:
    """Template extension — copy and customize for your needs.

    Available subsystems via ExtensionContext:
        ctx.event_queue       — publish/subscribe events
        ctx.agent_pool      — query and manage agents
        ctx.execution_tracker — track agent ↔ issue mappings
        ctx.tool_registry   — register custom tools
        ctx.storage         — persist arbitrary data
    """

    @property
    def name(self) -> str:
        return "template"

    @property
    def version(self) -> str:
        return "0.1.0"

    async def on_register(self, ctx: ExtensionContext) -> None:
        """Called when the extension is registered.

        Use this to:
        - Subscribe to events
        - Register custom tools
        """

    async def on_engine_start(self, ctx: ExtensionContext) -> None:
        """Called after the engine has fully started.

        Use this to:
        - Start background tasks
        - Initialize external connections
        - Log startup metrics
        """

    async def on_engine_stop(self, ctx: ExtensionContext) -> None:
        """Called before the engine shuts down.

        Use this to:
        - Clean up resources
        - Flush buffers
        - Close connections
        """
