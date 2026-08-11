"""Tests for the crewlet package top-level exports."""

from crewlet import Tool
from crewlet.tools.protocol import Tool as ProtocolTool


def test_tool_exported():
    """Tool protocol is accessible from the top-level crewlet package."""
    assert Tool is ProtocolTool


def test_tool_in_all():
    """Tool appears in __all__."""
    import crewlet

    assert "Tool" in crewlet.__all__
