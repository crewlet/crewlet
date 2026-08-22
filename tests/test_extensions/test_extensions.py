"""Tests for the extension system."""

import pytest

from crewlet.engine import Engine
from crewlet.events.types import Event
from crewlet.extensions.loader import ExtensionManager
from crewlet.extensions.protocol import ExtensionContext
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.queue.memory import MemoryEventQueue
from crewlet.tools.protocol import ToolResult
from crewlet.tools.registry import BUILTIN_ORIGIN, SimpleTool, ToolRegistry


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


class LateToolExtension:
    """Extension that registers its tool from ``on_engine_start``.

    A perfectly legal place to do it — a tool whose construction needs a
    started subsystem cannot be built during ``on_register`` — and a
    second hook the origin has to survive.
    """

    @property
    def name(self) -> str:
        return "late-tools"

    @property
    def version(self) -> str:
        return "1.0.0"

    async def on_register(self, ctx: ExtensionContext) -> None:
        pass

    async def on_engine_start(self, ctx: ExtensionContext) -> None:
        async def late_fn(params, context):
            return ToolResult(output="late")

        ctx.tool_registry.register(
            SimpleTool(
                name="ext_late",
                description="Registered at engine start",
                parameters={"type": "object"},
                fn=late_fn,
            )
        )

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
async def test_an_extensions_tool_reports_the_extension_as_its_origin():
    """The whole point of the origin: an extension's tool is
    structurally identical to a builtin, so before it was recorded the
    dashboard called both "builtin" — and a tool missing because its
    extension failed to load read as a missing builtin."""
    ext = ToolRegisteringExtension()
    engine = Engine(organization=make_org(), extensions=[ext])

    await engine.start()
    try:
        assert engine.tool_registry.origin_for("ext_hello") == "extension:custom-tools"
        # And the engine's own tools are still the engine's own.
        assert engine.tool_registry.origin_for("lookup_colleague") == BUILTIN_ORIGIN

        by_name = {t["name"]: t for t in engine._build_tools_data()}
        assert by_name["ext_hello"]["source"] == "extension:custom-tools"
        assert by_name["lookup_colleague"]["source"] == BUILTIN_ORIGIN
    finally:
        await engine.stop()


@pytest.mark.asyncio
async def test_a_tool_registered_at_engine_start_carries_the_origin_too():
    ext = LateToolExtension()
    engine = Engine(organization=make_org(), extensions=[ext])

    await engine.start()
    try:
        assert engine.tool_registry.get("ext_late") is not None
        assert engine.tool_registry.origin_for("ext_late") == "extension:late-tools"
    finally:
        await engine.stop()


@pytest.mark.asyncio
async def test_a_tool_passed_to_the_engine_is_not_a_builtin():
    """``Engine(tools=[...])`` is the embedding application's surface,
    not the engine's — reporting it as ``builtin`` is the same lie in
    different clothes."""

    async def fn(params, context):
        return ToolResult(output="ok")

    tool = SimpleTool(
        name="review_code",
        description="Run static analysis",
        parameters={"type": "object"},
        fn=fn,
    )
    engine = Engine(organization=make_org(), tools=[tool])
    assert engine.tool_registry.origin_for("review_code") == "custom"


@pytest.mark.asyncio
async def test_the_scoped_registry_keeps_the_rest_of_the_context():
    """Each extension gets its own context object so its registrations
    can be stamped. Everything else on it must still be the shared
    engine state — a copy that dropped a subsystem would break the
    extension silently, at the first attribute it read."""
    ext = ToolRegisteringExtension()
    engine = Engine(organization=make_org(), extensions=[ext])
    ctx = engine._build_extension_context()

    from crewlet.extensions.loader import _scoped

    scoped = _scoped(ctx, ext)
    assert scoped is not ctx
    assert scoped.tool_registry is not ctx.tool_registry
    assert scoped.event_queue is ctx.event_queue
    assert scoped.org is ctx.org
    assert scoped.claim_duty is ctx.claim_duty
    assert scoped.node_id == ctx.node_id


@pytest.mark.asyncio
async def test_a_context_without_a_registry_is_left_alone():
    """A bare ``ExtensionContext`` (every test that builds one) carries
    no registry, and scoping must not manufacture one."""
    from crewlet.extensions.loader import _scoped

    ctx = ExtensionContext(event_queue=MemoryEventQueue())
    ext = ToolRegisteringExtension()
    assert _scoped(ctx, ext) is ctx


@pytest.mark.asyncio
async def test_two_extensions_do_not_share_an_origin():
    class Second(ToolRegisteringExtension):
        @property
        def name(self) -> str:
            return "second"

        async def on_register(self, ctx: ExtensionContext) -> None:
            async def fn(params, context):
                return ToolResult(output="second")

            ctx.tool_registry.register(
                SimpleTool(
                    name="second_tool",
                    description="Second extension tool",
                    parameters={"type": "object"},
                    fn=fn,
                )
            )

    registry = ToolRegistry()
    manager = ExtensionManager()
    ctx = ExtensionContext(tool_registry=registry)

    await manager.register(ToolRegisteringExtension(), ctx)
    await manager.register(Second(), ctx)

    assert registry.origin_for("ext_hello") == "extension:custom-tools"
    assert registry.origin_for("second_tool") == "extension:second"


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


@pytest.mark.asyncio
async def test_unregistering_an_extension_takes_its_tools_with_it():
    """Dropping the object was never enough.

    ``unregister`` calls ``on_engine_stop`` and removes the extension
    from its own list — and most extensions have nothing to stop, so they
    do not implement the hook. Everything they registered therefore
    stayed in the shared registry, advertised in every later catalogue
    and dispatching into a disposed object. A live config edit that
    removed an extension left exactly that behind.
    """
    from crewlet.tools.registry import (
        BUILTIN_ORIGIN,
        SimpleTool,
        ToolRegistry,
        extension_origin,
    )

    async def _fn(params, context):
        return ToolResult(output="ok")

    def _tool(name: str) -> SimpleTool:
        return SimpleTool(
            name=name, description="d", parameters={"type": "object"}, fn=_fn
        )

    class SilentExtension:
        """The common case: nothing to stop, so no stop hook."""

        name = "acme"
        version = "1.0"

        async def on_register(self, ctx):
            ctx.tool_registry.register(_tool("acme_deploy"))

        async def on_engine_start(self, ctx):
            return None

        async def on_engine_stop(self, ctx):
            return None

    registry = ToolRegistry()
    registry.register(_tool("builtin_one"), origin=BUILTIN_ORIGIN)
    ctx = ExtensionContext(event_queue=MemoryEventQueue(), tool_registry=registry)
    manager = ExtensionManager()
    ext = SilentExtension()

    await manager.register(ext, ctx)
    assert registry.origin_for("acme_deploy") == extension_origin("acme")

    await manager.unregister(ext, ctx)

    names = {t.name for t in registry.list_tools()}
    assert "acme_deploy" not in names, "the extension's tool outlived the extension"
    assert "builtin_one" in names, "the sweep took a tool it did not own"
