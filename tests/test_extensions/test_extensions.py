"""Tests for the extension system."""

import pytest

from crewlet.engine import Engine
from crewlet.events.types import Event
from crewlet.extensions.loader import ExtensionManager
from crewlet.extensions.protocol import ExtensionContext
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.queue.memory import MemoryEventQueue
from crewlet.tools.protocol import ToolResult
from crewlet.tools.registry import SimpleTool


class TrackingExtension:
    """Test extension that tracks lifecycle calls."""

    def __init__(self) -> None:
        self.registered = False
        self.started = False
        self.stopped = False
        self.ctx: ExtensionContext | None = None

    @property
    def name(self) -> str:
        return "tracking"

    @property
    def version(self) -> str:
        return "1.0.0"

    async def on_register(self, ctx: ExtensionContext) -> None:
        self.registered = True
        self.ctx = ctx

    async def on_engine_start(self, ctx: ExtensionContext) -> None:
        self.started = True

    async def on_engine_stop(self, ctx: ExtensionContext) -> None:
        self.stopped = True


class ToolRegisteringExtension:
    """Extension that registers a custom tool."""

    @property
    def name(self) -> str:
        return "custom-tools"

    @property
    def version(self) -> str:
        return "1.0.0"

    async def on_register(self, ctx: ExtensionContext) -> None:
        async def hello_fn(params, context):
            return ToolResult(output="Hello from extension!")

        tool = SimpleTool(
            name="ext_hello",
            description="Extension hello tool",
            parameters={"type": "object"},
            fn=hello_fn,
        )
        ctx.tool_registry.register(tool)

    async def on_engine_start(self, ctx: ExtensionContext) -> None:
        pass

    async def on_engine_stop(self, ctx: ExtensionContext) -> None:
        pass


class MiddlewareExtension:
    """Extension that adds event middleware."""

    def __init__(self) -> None:
        self.events_seen: list[str] = []

    @property
    def name(self) -> str:
        return "middleware"

    @property
    def version(self) -> str:
        return "1.0.0"

    async def on_register(self, ctx: ExtensionContext) -> None:
        async def log_handler(event: Event) -> None:
            self.events_seen.append(event.type)

        # Subscribe to common event types for logging
        if ctx.event_queue is not None:
            for event_type in ("org_started", "org_stopped", "agent_spawned"):
                await ctx.event_queue.subscribe(
                    f"crewlet.events.{event_type}",
                    f"middleware-{event_type}",
                    log_handler,
                )

    async def on_engine_start(self, ctx: ExtensionContext) -> None:
        pass

    async def on_engine_stop(self, ctx: ExtensionContext) -> None:
        pass


class FailingExtension:
    """Extension whose on_register raises an error."""

    @property
    def name(self) -> str:
        return "failing"

    @property
    def version(self) -> str:
        return "1.0.0"

    async def on_register(self, ctx: ExtensionContext) -> None:
        raise RuntimeError("on_register failed")

    async def on_engine_start(self, ctx: ExtensionContext) -> None:
        pass

    async def on_engine_stop(self, ctx: ExtensionContext) -> None:
        pass


class FailingStartExtension:
    """Extension whose on_engine_start raises an error."""

    def __init__(self) -> None:
        self.registered = False

    @property
    def name(self) -> str:
        return "failing-start"

    @property
    def version(self) -> str:
        return "1.0.0"

    async def on_register(self, ctx: ExtensionContext) -> None:
        self.registered = True

    async def on_engine_start(self, ctx: ExtensionContext) -> None:
        raise RuntimeError("on_engine_start failed")

    async def on_engine_stop(self, ctx: ExtensionContext) -> None:
        pass


# --- ExtensionManager tests ---


@pytest.mark.asyncio
async def test_manager_register():
    ext = TrackingExtension()
    manager = ExtensionManager()
    ctx = ExtensionContext(event_queue=MemoryEventQueue())

    await manager.register(ext, ctx)
    assert ext.registered
    assert ext.ctx is ctx
    assert len(manager.extensions) == 1


@pytest.mark.asyncio
async def test_manager_start_stop():
    ext = TrackingExtension()
    manager = ExtensionManager()
    ctx = ExtensionContext(event_queue=MemoryEventQueue())

    await manager.register(ext, ctx)
    await manager.start_all(ctx)
    assert ext.started

    await manager.stop_all(ctx)
    assert ext.stopped


@pytest.mark.asyncio
async def test_manager_stop_reverse_order():
    order = []

    class Ext:
        def __init__(self, n):
            self._name = n

        @property
        def name(self):
            return self._name

        @property
        def version(self):
            return "1.0"

        async def on_register(self, ctx):
            pass

        async def on_engine_start(self, ctx):
            pass

        async def on_engine_stop(self, ctx):
            order.append(self._name)

    manager = ExtensionManager()
    ctx = ExtensionContext()

    await manager.register(Ext("first"), ctx)
    await manager.register(Ext("second"), ctx)
    await manager.stop_all(ctx)

    assert order == ["second", "first"]


@pytest.mark.asyncio
async def test_register_failure_removes_extension():
    """A failing on_register should remove the extension from the list."""
    manager = ExtensionManager()
    ctx = ExtensionContext()
    failing = FailingExtension()

    with pytest.raises(RuntimeError, match="on_register failed"):
        await manager.register(failing, ctx)

    assert len(manager.extensions) == 0


@pytest.mark.asyncio
async def test_start_failure_does_not_block_others():
    """A failing on_engine_start should not prevent other extensions."""
    manager = ExtensionManager()
    ctx = ExtensionContext()

    failing = FailingStartExtension()
    good = TrackingExtension()

    await manager.register(failing, ctx)
    await manager.register(good, ctx)
    await manager.start_all(ctx)

    # The good extension should still have started
    assert good.started


# --- Engine + Extension integration tests ---


def make_org() -> Organization:
    return Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Engineer",
                roles=[Role(name="Engineer")],
            )
        ],
    )


@pytest.mark.asyncio
async def test_engine_with_extension():
    ext = TrackingExtension()
    engine = Engine(organization=make_org(), extensions=[ext])

    await engine.start()
    assert ext.registered
    assert ext.started
    assert ext.ctx is not None
    assert ext.ctx.event_queue is engine.event_queue

    await engine.stop()
    assert ext.stopped


@pytest.mark.asyncio
async def test_the_context_carries_the_fleet_fields() -> None:
    """``node_id`` and ``claim_duty`` have to actually be wired.

    Both fail SILENTLY if the engine stops passing them. ``claim_duty``
    is documented as "``None`` means treat it as yes", which is right for
    a bare context in a test and exactly wrong for a real engine: every
    extension doing a company-wide job would quietly do it once per
    node, which is the thing the field exists to prevent. Nothing else
    would report it — that is a job done N times, not an error.
    """
    ext = TrackingExtension()
    engine = Engine(organization=make_org(), extensions=[ext])

    await engine.start()
    try:
        assert ext.ctx is not None
        assert ext.ctx.node_id, "an extension cannot tell which node it is on"
        assert ext.ctx.node_id == engine._node_id
        assert ext.ctx.claim_duty is not None, (
            "extensions were left with no way to ask for a singleton duty"
        )
        # And it answers. A single node is a fleet of one, so it wins.
        assert await ext.ctx.claim_duty("acme-digest") is True
    finally:
        await engine.stop()


@pytest.mark.asyncio
async def test_the_duty_is_claimed_per_call_not_held() -> None:
    """The doc tells extensions to ask again each tick, which is only
    sound if asking twice works — a claim that answered yes once and no
    forever after would push every extension into the
    hold-from-on_engine_start shape the doc warns against."""
    ext = TrackingExtension()
    engine = Engine(organization=make_org(), extensions=[ext])

    await engine.start()
    try:
        assert ext.ctx is not None
        for _ in range(3):
            assert await ext.ctx.claim_duty("acme-digest") is True
    finally:
        await engine.stop()


@pytest.mark.asyncio
async def test_engine_extension_registers_tool():
    ext = ToolRegisteringExtension()
    engine = Engine(organization=make_org(), extensions=[ext])

    await engine.start()
    tool = engine.tool_registry.get("ext_hello")
    assert tool is not None
    assert tool.name == "ext_hello"
    await engine.stop()


@pytest.mark.asyncio
async def test_engine_extension_adds_middleware():
    ext = MiddlewareExtension()
    engine = Engine(organization=make_org(), extensions=[ext])

    await engine.start()
    # Subscriber should have seen some events
    assert len(ext.events_seen) > 0
    assert "org_started" in ext.events_seen
    await engine.stop()


@pytest.mark.asyncio
async def test_engine_extensions_property():
    ext = TrackingExtension()
    engine = Engine(organization=make_org(), extensions=[ext])
    await engine.start()
    assert len(engine.extensions) == 1
    assert engine.extensions[0].name == "tracking"
    await engine.stop()


@pytest.mark.asyncio
async def test_extension_context_is_pydantic():
    """ExtensionContext should be a Pydantic BaseModel."""
    from pydantic import BaseModel

    assert issubclass(ExtensionContext, BaseModel)

    ctx = ExtensionContext(event_queue=MemoryEventQueue())
    data = ctx.model_dump()
    assert "event_queue" in data
