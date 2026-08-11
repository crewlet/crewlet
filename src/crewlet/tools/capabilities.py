"""Tool capability classification derived from MCP tool annotations.

The engine occasionally needs to reason about *what a tool does* — for
example, whether a sub-agent may call it without leaking the parent
agent's identity onto a human-visible surface.  A hardcoded list of
tool names (``slack_conversations_postMessage``, ``jira_add_comment``,
…) would couple the engine to one specific tool stack and silently
fail for any other (Linear, Teams, GitLab…).

Instead we derive behaviour from the **MCP tool annotations** every
compliant MCP server can advertise — the ``readOnlyHint`` /
``destructiveHint`` / ``openWorldHint`` / ``idempotentHint`` / ``title``
hints from the MCP spec.  Built-in (first-party) tools carry the same
annotations declared in code.  The classification functions here turn
those hints into the yes/no questions the engine asks, so the engine
never has to know a single concrete tool name.

See ``docs/concepts/tool-capabilities.md``.
"""

from __future__ import annotations

from collections.abc import Callable
from typing import Any

from pydantic import BaseModel


class ToolAnnotations(BaseModel):
    """Behavioural hints about a tool, mirroring the MCP spec.

    Every field is **tri-state**: ``True`` / ``False`` / ``None``
    (unknown — the server did not advertise the hint).  ``None`` must
    never be coerced to ``False``: "unknown" and "explicitly safe" are
    different, and the classifiers below depend on the distinction.
    """

    title: str | None = None
    read_only: bool | None = None
    """``readOnlyHint`` — the tool does not modify any state."""
    destructive: bool | None = None
    """``destructiveHint`` — the tool may perform irreversible updates."""
    idempotent: bool | None = None
    """``idempotentHint`` — repeat calls have no additional effect."""
    open_world: bool | None = None
    """``openWorldHint`` — the tool interacts with entities outside the
    local system (the network, external services, shared surfaces a
    human can see).  MCP's spec default is ``True`` when unspecified."""

    @classmethod
    def from_mcp(cls, raw: Any) -> ToolAnnotations:
        """Build from an MCP ``annotations`` payload (dict or model).

        Accepts the camelCase keys the MCP spec uses (``readOnlyHint``
        etc.) as well as a plain ``ToolAnnotations``-shaped dict (snake
        case), so operator config and on-wire data both parse.  Unknown
        shape or ``None`` → an all-unknown instance.
        """
        if raw is None:
            return cls()
        if isinstance(raw, ToolAnnotations):
            return raw
        if isinstance(raw, dict):
            data: dict[str, Any] = raw
        elif hasattr(raw, "model_dump"):
            data = raw.model_dump()
        else:
            return cls()

        def pick(*keys: str) -> Any:
            for key in keys:
                if key in data and data[key] is not None:
                    return data[key]
            return None

        return cls(
            title=pick("title"),
            read_only=pick("readOnlyHint", "read_only"),
            destructive=pick("destructiveHint", "destructive"),
            idempotent=pick("idempotentHint", "idempotent"),
            open_world=pick("openWorldHint", "open_world"),
        )

    def merge(self, override: ToolAnnotations | None) -> ToolAnnotations:
        """Return a copy with any *set* fields from ``override`` applied.

        Lets operator-supplied config annotations win over what a server
        advertised — the escape hatch for MCP servers that under-annotate
        their tools."""
        if override is None:
            return self
        merged = self.model_dump()
        for key, value in override.model_dump().items():
            if value is not None:
                merged[key] = value
        return ToolAnnotations(**merged)


def writes_to_shared_surface(ann: ToolAnnotations) -> bool:
    """True when a tool writes to a surface outside the local engine.

    This is the question the sub-agent guard asks: a sub-agent must not
    post to a channel / comment on an issue / open a pull request (etc.)
    under the parent agent's identity.  Such a tool is one that is **not
    read-only** and is **open-world** (touches external entities), or is
    explicitly **destructive**.

    Tri-state logic, deliberately conservative about *unknown*:

    - ``read_only is True``   → never a shared write (a pure read).
    - ``destructive is True``  → always treated as a write.
    - ``read_only is False`` and ``open_world`` not explicitly ``False``
      → a write to the outside world.
    - everything else (unknown) → ``False``.  We do not block what we
      cannot classify: the parent's explicit allowlist already curates
      the sub-agent surface, and operators can annotate via config for
      servers that don't advertise hints.
    """
    if ann.read_only is True:
        return False
    if ann.destructive is True:
        return True
    return ann.read_only is False and ann.open_world is not False


AnnotationsLookup = Callable[[str], "ToolAnnotations | None"]


def resolve_annotations(
    tool: Any, lookup: AnnotationsLookup | None = None
) -> ToolAnnotations:
    """Resolve a tool's annotations from the tool itself or a side-table.

    MCP-bridged tools carry their annotations directly (the
    ``annotations`` property on ``MCPToolWrapper``).  First-party
    builtins register theirs in the :class:`~crewlet.tools.registry.ToolRegistry`
    side-table; pass ``registry.annotations_for`` as ``lookup`` to
    consult it.  Returns an all-unknown instance when neither source has
    anything.
    """
    direct = getattr(tool, "annotations", None)
    if isinstance(direct, ToolAnnotations):
        return direct
    if lookup is not None:
        found = lookup(getattr(tool, "name", ""))
        if found is not None:
            return found
    return ToolAnnotations()
