"""Tests for tool capability classification.

These cover the primitive the engine uses to reason about tools
*without naming them* — annotations parsed from the MCP spec hints and
the ``writes_to_shared_surface`` classifier the sub-agent guard relies
on.
"""

from __future__ import annotations

from crewlet.tools.capabilities import (
    ToolAnnotations,
    resolve_annotations,
    writes_to_shared_surface,
)


def test_from_mcp_parses_camelcase_hints():
    """MCP advertises camelCase hints (``readOnlyHint`` …)."""
    ann = ToolAnnotations.from_mcp(
        {
            "title": "Create PR",
            "readOnlyHint": False,
            "destructiveHint": False,
            "idempotentHint": True,
            "openWorldHint": True,
        }
    )
    assert ann.title == "Create PR"
    assert ann.read_only is False
    assert ann.idempotent is True
    assert ann.open_world is True


def test_from_mcp_accepts_snake_case_for_config():
    """Operator config uses snake_case keys; both parse."""
    ann = ToolAnnotations.from_mcp({"read_only": True})
    assert ann.read_only is True
    assert ann.open_world is None  # unspecified stays unknown, not False


def test_from_mcp_none_is_all_unknown():
    ann = ToolAnnotations.from_mcp(None)
    assert ann.read_only is None
    assert ann.destructive is None
    assert ann.open_world is None


def test_merge_lets_override_win_only_for_set_fields():
    base = ToolAnnotations(read_only=False, open_world=True)
    override = ToolAnnotations(read_only=True)  # only read_only set
    merged = base.merge(override)
    assert merged.read_only is True  # override won
    assert merged.open_world is True  # base preserved (override left it None)


def test_merge_none_override_is_noop():
    base = ToolAnnotations(read_only=False)
    assert base.merge(None) is base


def test_writes_to_shared_surface_true_for_open_world_write():
    assert writes_to_shared_surface(ToolAnnotations(read_only=False, open_world=True))


def test_writes_to_shared_surface_open_world_defaults_true_when_unspecified():
    """A non-read-only tool with no open-world hint is still treated as
    an external write (MCP's open-world default is true)."""
    assert writes_to_shared_surface(ToolAnnotations(read_only=False))


def test_writes_to_shared_surface_false_for_read_only():
    assert not writes_to_shared_surface(ToolAnnotations(read_only=True))


def test_writes_to_shared_surface_destructive_always_true():
    assert writes_to_shared_surface(ToolAnnotations(destructive=True))


def test_writes_to_shared_surface_false_when_local_write():
    """A write that is explicitly NOT open-world (a local-only effect,
    e.g. the personal diary) is not a shared-surface write."""
    assert not writes_to_shared_surface(
        ToolAnnotations(read_only=False, open_world=False)
    )


def test_writes_to_shared_surface_false_when_unknown():
    """All-unknown annotations are never auto-classified as a write —
    we don't block what we cannot classify."""
    assert not writes_to_shared_surface(ToolAnnotations())


def test_resolve_prefers_tool_carried_annotations():
    class _Tool:
        name = "post"
        annotations = ToolAnnotations(read_only=False, open_world=True)

    # Lookup is ignored when the tool carries its own annotations.
    resolved = resolve_annotations(
        _Tool(), lambda _name: ToolAnnotations(read_only=True)
    )
    assert resolved.read_only is False


def test_resolve_falls_back_to_lookup_for_builtins():
    class _Builtin:
        name = "reflect_and_persist"

    table = {"reflect_and_persist": ToolAnnotations(read_only=False, open_world=False)}
    resolved = resolve_annotations(_Builtin(), table.get)
    assert resolved.open_world is False


def test_resolve_unknown_returns_empty():
    class _Bare:
        name = "mystery"

    resolved = resolve_annotations(_Bare(), lambda _name: None)
    assert resolved == ToolAnnotations()
