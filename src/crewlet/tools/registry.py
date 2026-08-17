"""Tool registry for managing available agent tools."""

from __future__ import annotations

from typing import Any

from crewlet._logging import get_logger
from crewlet.tools.capabilities import ToolAnnotations
from crewlet.tools.protocol import AgentContext, CheckContext, CheckFn, Tool, ToolResult

logger = get_logger("tools.registry")


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
    carry their own annotations on the wrapper instead.
    """

    def __init__(self) -> None:
        self._tools: dict[str, Tool] = {}
        self._check_fns: dict[str, CheckFn] = {}
        self._annotations: dict[str, ToolAnnotations] = {}

    def register(
        self,
        tool: Tool,
        *,
        check_fn: CheckFn | None = None,
        annotations: ToolAnnotations | None = None,
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
        """
        self._tools[tool.name] = tool
        if check_fn is not None:
            self._check_fns[tool.name] = check_fn
        if annotations is not None:
            self._annotations[tool.name] = annotations
        logger.info("tool_registered", name=tool.name)

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
        if existed:
            logger.info("tool_unregistered", name=tool_name)
        return existed

    def annotations_for(self, tool_name: str) -> ToolAnnotations | None:
        """Return the registered :class:`ToolAnnotations` for a builtin,
        or ``None`` when none were declared (treated as all-unknown)."""
        return self._annotations.get(tool_name)

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
