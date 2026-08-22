"""Tool registry for managing available agent tools."""

from __future__ import annotations

from typing import Any

from crewlet._logging import get_logger
from crewlet.tools.capabilities import ToolAnnotations
from crewlet.tools.protocol import AgentContext, CheckContext, CheckFn, Tool, ToolResult

logger = get_logger("tools.registry")

# THE tool-origin grammar -- who put a tool in front of the agents.
# Recorded at registration because it cannot be recovered afterwards: a
# tool an extension registers is structurally identical to an engine
# builtin, so the dashboard reported both as "builtin" and an operator
# could not tell what the engine ships from what an extension added --
# a tool missing because its extension failed to load read as a missing
# builtin. The strings are the ``source`` field of ``GET /tools``, which
# the dashboard groups on, so producers and consumers derive them here.
BUILTIN_ORIGIN = "builtin"
"""Origin of the tools the engine itself ships."""

CUSTOM_ORIGIN = "custom"
"""Origin of tools handed to ``Engine(tools=[...])`` by an embedding
application -- not shipped by the engine, and not from an extension."""

EXTENSION_ORIGIN_PREFIX = "extension:"
MCP_ORIGIN_PREFIX = "mcp:"


def extension_origin(extension_name: str) -> str:
    """Origin string for a tool an extension registered."""
    return f"{EXTENSION_ORIGIN_PREFIX}{extension_name}"


def mcp_origin(server_name: str) -> str:
    """Origin string for a tool served by an MCP server."""
    return f"{MCP_ORIGIN_PREFIX}{server_name}"


class SimpleTool:
    """A simple tool implementation wrapping an async callable."""

    def __init__(
        self,
        name: str,
        description: str,
        parameters: dict[str, Any],
        fn: Any,
    ) -> None:
        self._name = name
        self._description = description
        self._parameters = parameters
        self._fn = fn

    @property
    def name(self) -> str:
        return self._name

    @property
    def description(self) -> str:
        return self._description

    @property
    def parameters(self) -> dict[str, Any]:
        return self._parameters

    async def execute(
        self, params: dict[str, Any], context: AgentContext
    ) -> ToolResult:
        return await self._fn(params, context)


class ToolRegistry:
    """Registry for managing agent tools.

    Tools are registered globally and filtered per-role based on
    the role's allowed tools list.

    Per-tool availability is tracked in a *separate* dict
    (``_check_fns``) keyed by tool name. This deliberately avoids
    extending the :class:`Tool` Protocol -- adding an attribute to a
    ``@runtime_checkable`` Protocol is backwards-incompatible with
    every existing ``Tool`` implementation that doesn't set it.  Tool
    behavioural annotations (:class:`ToolAnnotations`) for first-party
    builtins are tracked the same way (``_annotations``); MCP tools
    carry their own annotations on the wrapper instead.  So is each
    tool's ``origin`` (``_origins``) -- who registered it.
    """

    def __init__(self) -> None:
        self._tools: dict[str, Tool] = {}
        self._check_fns: dict[str, CheckFn] = {}
        self._annotations: dict[str, ToolAnnotations] = {}
        self._origins: dict[str, str] = {}

    def register(
        self,
        tool: Tool,
        *,
        check_fn: CheckFn | None = None,
        annotations: ToolAnnotations | None = None,
        origin: str = BUILTIN_ORIGIN,
    ) -> None:
        """Register a tool, optionally with an availability check.

        ``check_fn`` is called per turn (with results cached) to
        decide whether the tool should appear in the catalogue /
        be loadable. ``None`` (the default) means "always available".
        See :func:`build_availability_set`.

        ``annotations`` declares the tool's behaviour (read-only,
        open-world, …) for first-party builtins, the same way MCP
        servers advertise hints for their tools.  Read via
        :meth:`annotations_for` /
        :func:`crewlet.tools.capabilities.resolve_annotations`.

        ``origin`` records who is registering — see the tool-origin
        grammar at the top of this module.  It defaults to
        :data:`BUILTIN_ORIGIN` because the engine's own registration
        helpers are the ones that pass nothing; every other registrant
        (extensions via :meth:`for_origin`, MCP servers, the embedding
        application) states its own.
        """
        self._tools[tool.name] = tool
        if check_fn is not None:
            self._check_fns[tool.name] = check_fn
        if annotations is not None:
            self._annotations[tool.name] = annotations
        self._origins[tool.name] = origin
        logger.info("tool_registered", name=tool.name, origin=origin)

    def for_origin(self, origin: str) -> OriginScopedRegistry:
        """Return a view of this registry that stamps ``origin`` on
        everything registered through it."""
        return OriginScopedRegistry(self, origin)

    def unregister(self, tool_name: str) -> bool:
        """Remove a tool and its check/annotation entries. ``True`` if present.

        Needed because registration was one-way: when a live config edit
        removed a shared MCP server, the engine stopped the server's
        client but left its tools in the registry. They stayed in every
        later turn's catalogue, and calling one dispatched to a stopped
        client — a soft ``success=False`` the model burns rounds
        retrying, with nothing in the logs to explain why a tool the
        catalogue advertises never works.
        """
        existed = self._tools.pop(tool_name, None) is not None
        self._check_fns.pop(tool_name, None)
        self._annotations.pop(tool_name, None)
        self._origins.pop(tool_name, None)
        if existed:
            logger.info("tool_unregistered", name=tool_name)
        return existed

    def unregister_origin(self, origin: str) -> list[str]:
        """Drop every tool registered under ``origin``. Returns their names.

        The counterpart to the origin being recorded at registration.
        Without it, dropping an extension dropped only the extension
        object: ``ExtensionManager.unregister`` calls ``on_engine_stop``
        and removes it from its own list, and an extension whose stop
        hook does nothing (the common case — most have nothing to stop)
        left its tools in the shared registry for ever. A live config
        edit that removes an extension therefore kept advertising tools
        backed by a disposed object, and calling one dispatched into it.
        """
        doomed = [name for name, its in self._origins.items() if its == origin]
        for name in doomed:
            self.unregister(name)
        if doomed:
            logger.info(
                "tools_unregistered_by_origin", origin=origin, count=len(doomed)
            )
        return doomed

    def annotations_for(self, tool_name: str) -> ToolAnnotations | None:
        """Return the registered :class:`ToolAnnotations` for a builtin,
        or ``None`` when none were declared (treated as all-unknown)."""
        return self._annotations.get(tool_name)

    def origin_for(self, tool_name: str) -> str:
        """Return who registered ``tool_name`` (the tool-origin grammar
        at the top of this module).

        Every registered tool has one — :meth:`register` writes it
        unconditionally — so the :data:`BUILTIN_ORIGIN` default is
        reached only for a name that is not registered at all."""
        return self._origins.get(tool_name, BUILTIN_ORIGIN)

    def register_check(self, tool_name: str, check_fn: CheckFn) -> None:
        """Attach an availability ``check_fn`` to an already-registered
        tool. Useful for MCP wrappers and colleague tools, which are
        registered by infrastructure code that does not have access
        to the check at construction time.
        """
        self._check_fns[tool_name] = check_fn

    def check_fn_for(self, tool_name: str) -> CheckFn | None:
        """Return the registered ``check_fn`` for ``tool_name`` or
        ``None`` (treated as "always available")."""
        return self._check_fns.get(tool_name)

    def get(self, name: str) -> Tool | None:
        """Get a tool by name."""
        tool = self._tools.get(name)
        if tool is None:
            logger.debug("tool_lookup_miss", name=name)
        return tool

    def list_tools(self) -> list[Tool]:
        """List all registered tools."""
        return list(self._tools.values())

    def to_tool_defs(self) -> list[dict[str, Any]]:
        """Convert tools to LLM-compatible tool definitions.

        Returns a list of dicts with name, description, and parameters
        suitable for passing to an LLM provider.
        """
        tools = self.list_tools()
        defs = [
            {
                "name": t.name,
                "description": t.description,
                "parameters": t.parameters,
            }
            for t in tools
        ]
        logger.debug("tool_defs_generated", count=len(defs))
        return defs


class OriginScopedRegistry:
    """A :class:`ToolRegistry` view that stamps one fixed origin on
    every tool registered through it.

    This is what an extension is handed as ``ctx.tool_registry``: the
    registration call is the only moment anything knows who is
    registering, so a per-registrant view is what makes the origin
    recordable at all.  Nothing else on the registry is intercepted —
    every other attribute passes through to the real registry.

    The scope cannot be overridden per call.  A view whose origin an
    extension could pass around it would let a third-party tool claim
    ``builtin``, which is the exact confusion the origin exists to end.
    """

    def __init__(self, registry: ToolRegistry, origin: str) -> None:
        self._registry = registry
        self._origin = origin

    @property
    def origin(self) -> str:
        return self._origin

    def register(
        self,
        tool: Tool,
        *,
        check_fn: CheckFn | None = None,
        annotations: ToolAnnotations | None = None,
    ) -> None:
        """Register a tool under this view's origin."""
        self._registry.register(
            tool,
            check_fn=check_fn,
            annotations=annotations,
            origin=self._origin,
        )

    def for_origin(self, origin: str) -> OriginScopedRegistry:
        """Re-scoping a scoped view is a no-op: it keeps this origin.

        Without this, ``__getattr__`` forwarded ``for_origin`` to the
        real registry, so an extension holding its own scoped view could
        call ``ctx.tool_registry.for_origin("builtin").register(tool)``
        and stamp its tool as one the engine ships — the exact confusion
        the origin exists to end. Returning ``self`` refuses the change
        rather than raising, because an extension asking for its own
        origin is not doing anything wrong and should not be punished
        for it.
        """
        if origin != self._origin:
            logger.warning(
                "tool_origin_rescope_refused",
                requested=origin,
                origin=self._origin,
            )
        return self

    def __getattr__(self, name: str) -> Any:
        # The underlying registry stays reachable through this — the
        # origin is a convention enforced at the seam an extension is
        # HANDED, not a sandbox. An extension that goes looking for
        # `_registry` can still reach it; nothing here is a security
        # boundary, and the loader is not a place to pretend otherwise.
        return getattr(self._registry, name)


def build_availability_set(
    registry: ToolRegistry, ctx: CheckContext, tool_names: list[str]
) -> set[str]:
    """Resolve which of ``tool_names`` are currently available.

    For each name:

    - If no ``check_fn`` is registered → available.
    - If a ``check_fn`` is registered and returns truthy → available.
    - If a ``check_fn`` is registered and returns falsy → not available.
    - If a ``check_fn`` raises → not available (fail-safe), warning
      logged with the tool name and exception class only (exception
      args may carry credentials).

    Returns a set of available tool names.

    Called once per turn; caller caches the result on ``TurnContext``
    so the same ``check_fn`` is not invoked across multiple phases.
    """
    available: set[str] = set()
    for name in tool_names:
        fn = registry.check_fn_for(name)
        if fn is None:
            available.add(name)
            continue
        try:
            ok = bool(fn(ctx))
        except Exception as exc:
            logger.warning(
                "tool_check_fn_raised",
                tool=name,
                error_class=type(exc).__name__,
            )
            continue
        if ok:
            available.add(name)
        else:
            logger.debug("tool_check_fn_unavailable", tool=name)
    return available
