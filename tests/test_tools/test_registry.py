"""Tests for the tool registry."""

from typing import Any

import pytest

from crewlet.tools.protocol import AgentContext, CheckContext, ToolResult
from crewlet.tools.registry import (
    BUILTIN_ORIGIN,
    CUSTOM_ORIGIN,
    SimpleTool,
    ToolRegistry,
    build_availability_set,
    extension_origin,
    mcp_origin,
)


def make_tool(name: str) -> SimpleTool:
    async def fn(params: dict[str, Any], context: AgentContext) -> ToolResult:
        return ToolResult(output=f"{name} executed")

    return SimpleTool(
        name=name,
        description=f"Test tool {name}",
        parameters={"type": "object"},
        fn=fn,
    )


def test_register_and_get():
    registry = ToolRegistry()
    tool = make_tool("test")
    registry.register(tool)
    assert registry.get("test") is not None
    assert registry.get("test").name == "test"


def test_get_nonexistent():
    registry = ToolRegistry()
    assert registry.get("nonexistent") is None


def test_list_tools():
    registry = ToolRegistry()
    registry.register(make_tool("a"))
    registry.register(make_tool("b"))
    tools = registry.list_tools()
    names = [t.name for t in tools]
    assert "a" in names
    assert "b" in names


def test_to_tool_defs():
    registry = ToolRegistry()
    registry.register(make_tool("test"))
    defs = registry.to_tool_defs()
    assert len(defs) == 1
    assert defs[0]["name"] == "test"
    assert "description" in defs[0]
    assert "parameters" in defs[0]


@pytest.mark.asyncio
async def test_simple_tool_execute():
    tool = make_tool("test")
    context = AgentContext(agent_id="a1", role="Engineer")
    result = await tool.execute({}, context)
    assert result.success
    assert result.output == "test executed"


# per-tool check_fn -------------------------------------------------------


def test_register_with_check_fn_attaches_check():
    """``register(tool, check_fn=...)`` stores the check_fn under the
    tool's name so ``check_fn_for`` can retrieve it later."""
    registry = ToolRegistry()

    def fn(_ctx):
        return True

    registry.register(make_tool("with_check"), check_fn=fn)
    assert registry.check_fn_for("with_check") is fn


def test_register_without_check_fn_returns_none():
    """A tool registered without a ``check_fn`` is treated as always
    available — ``check_fn_for`` returns ``None``."""
    registry = ToolRegistry()
    registry.register(make_tool("no_check"))
    assert registry.check_fn_for("no_check") is None


def test_register_check_attaches_after_registration():
    """``register_check`` lets infrastructure code (MCP wrapper,
    colleague-tool registrar) attach a ``check_fn`` to a tool that
    was registered earlier."""
    registry = ToolRegistry()
    registry.register(make_tool("colleague"))
    registry.register_check("colleague", lambda ctx: bool(ctx.mcp_env.get("slack")))
    fn = registry.check_fn_for("colleague")
    assert fn is not None
    assert fn(CheckContext(mcp_env={"slack": {"token": "x"}})) is True
    assert fn(CheckContext(mcp_env={})) is False


def test_build_availability_set_no_check_fns_means_all_available():
    """A registry with zero check_fns returns every name as
    available — the default-allow rule."""
    registry = ToolRegistry()
    registry.register(make_tool("a"))
    registry.register(make_tool("b"))
    ctx = CheckContext()
    result = build_availability_set(registry, ctx, ["a", "b"])
    assert result == {"a", "b"}


def test_build_availability_set_filters_unavailable():
    """A tool whose ``check_fn`` returns ``False`` is excluded; one
    that returns ``True`` is included; one with no check is always
    included."""
    registry = ToolRegistry()
    registry.register(make_tool("always"))
    registry.register(make_tool("yes"), check_fn=lambda _ctx: True)
    registry.register(make_tool("no"), check_fn=lambda _ctx: False)
    ctx = CheckContext()
    result = build_availability_set(registry, ctx, ["always", "yes", "no"])
    assert result == {"always", "yes"}


def test_build_availability_set_gates_on_sandbox_enabled():
    """A check_fn can gate a tool on ``CheckContext.sandbox_enabled`` —
    how the ``run_sandbox`` builtin is shown only to roles allowed to use
    the detached sandbox."""
    registry = ToolRegistry()
    registry.register(make_tool("run_sandbox"), check_fn=lambda c: c.sandbox_enabled)
    off = build_availability_set(registry, CheckContext(), ["run_sandbox"])
    assert off == set()
    on = build_availability_set(
        registry, CheckContext(sandbox_enabled=True), ["run_sandbox"]
    )
    assert on == {"run_sandbox"}


def test_build_availability_set_exception_treated_as_unavailable():
    """A ``check_fn`` that raises is fail-safe: the tool is treated
    as unavailable. The exception class is logged (verified by no
    propagation here) but exception args are not logged so secrets
    in error messages don't leak."""

    def _raises(_ctx: CheckContext) -> bool:
        raise RuntimeError("password=abc123 is invalid")

    registry = ToolRegistry()
    registry.register(make_tool("flaky"), check_fn=_raises)
    ctx = CheckContext()
    result = build_availability_set(registry, ctx, ["flaky"])
    assert result == set()


def test_build_availability_set_only_resolves_requested_names():
    """``tool_names`` filters which check_fns run. A registered tool
    not in the names list is not resolved (and its check_fn is not
    invoked, even if registered)."""
    calls: list[str] = []

    def _track(name: str):
        def _fn(_ctx: CheckContext) -> bool:
            calls.append(name)
            return True

        return _fn

    registry = ToolRegistry()
    registry.register(make_tool("a"), check_fn=_track("a"))
    registry.register(make_tool("b"), check_fn=_track("b"))
    build_availability_set(registry, CheckContext(), ["a"])
    assert calls == ["a"]


def test_unregister_removes_a_tool_and_its_metadata() -> None:
    """Registration used to be one-way: when a live config edit removed a
    shared MCP server, the engine stopped its client but left the tools
    in the registry, so they stayed in every later turn's catalogue and
    dispatched to a dead client forever."""
    from crewlet.tools.capabilities import ToolAnnotations

    registry = ToolRegistry()
    tool = make_tool("doomed")
    registry.register(
        tool,
        check_fn=lambda _ctx: True,
        annotations=ToolAnnotations(read_only_hint=True),
    )
    assert registry.get("doomed") is not None

    assert registry.unregister("doomed") is True

    assert registry.get("doomed") is None
    assert registry.annotations_for("doomed") is None
    assert registry.origin_for("doomed") == BUILTIN_ORIGIN  # the not-here answer
    assert "doomed" not in [t.name for t in registry.list_tools()]


def test_unregister_is_false_for_an_unknown_tool() -> None:
    assert ToolRegistry().unregister("never-existed") is False


# --- tool origins ---


def test_registration_without_an_origin_is_a_builtin() -> None:
    """The engine's own register_* helpers pass nothing, so the default
    has to be the engine's own answer."""
    registry = ToolRegistry()
    registry.register(make_tool("lookup_colleague"))
    assert registry.origin_for("lookup_colleague") == BUILTIN_ORIGIN


def test_an_origin_is_recorded_verbatim() -> None:
    registry = ToolRegistry()
    registry.register(make_tool("acme_ping"), origin=extension_origin("acme"))
    registry.register(make_tool("review_code"), origin=CUSTOM_ORIGIN)
    registry.register(make_tool("jira_get_issue"), origin=mcp_origin("atlassian"))

    assert registry.origin_for("acme_ping") == "extension:acme"
    assert registry.origin_for("review_code") == "custom"
    assert registry.origin_for("jira_get_issue") == "mcp:atlassian"


def test_a_scoped_view_stamps_every_registration() -> None:
    registry = ToolRegistry()
    scoped = registry.for_origin(extension_origin("acme"))

    scoped.register(make_tool("acme_ping"))
    scoped.register(make_tool("acme_pong"), check_fn=lambda _ctx: False)

    assert registry.origin_for("acme_ping") == "extension:acme"
    assert registry.origin_for("acme_pong") == "extension:acme"
    # The rest of the registry is the real one, not a copy of it.
    assert registry.get("acme_ping") is not None
    assert registry.check_fn_for("acme_pong") is not None


def test_a_scoped_view_passes_every_other_call_through() -> None:
    """Extensions are handed the view as ``ctx.tool_registry`` and use
    the whole registry surface through it — a view that intercepted only
    ``register`` and forwarded nothing else would break every extension
    that reads back what it registered."""
    registry = ToolRegistry()
    scoped = registry.for_origin(extension_origin("acme"))
    registry.register(make_tool("builtin_one"))

    assert scoped.get("builtin_one") is not None
    assert [t.name for t in scoped.list_tools()] == ["builtin_one"]
    assert scoped.origin_for("builtin_one") == BUILTIN_ORIGIN

    scoped.register(make_tool("acme_ping"))
    assert scoped.unregister("acme_ping") is True
    assert registry.get("acme_ping") is None
