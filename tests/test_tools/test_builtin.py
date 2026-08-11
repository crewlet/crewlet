"""Tests for built-in tools."""

import pytest
import pytest_asyncio

from crewlet.queue.memory import MemoryEventQueue
from crewlet.tools.builtin import register_builtin_tools
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry


@pytest.fixture
def registry():
    r = ToolRegistry()
    register_builtin_tools(r)
    return r


@pytest_asyncio.fixture
async def bus():
    q = MemoryEventQueue()
    await q.start()
    yield q
    await q.stop()


@pytest.fixture
def context(bus: MemoryEventQueue):
    return AgentContext(
        agent_id="agent-1",
        role="Engineer",
        current_task_id="task-99",
        event_queue=bus,
    )


def test_all_builtin_tools_registered(registry: ToolRegistry):
    """Task-management tools are not registered (external PM tool handles them).

    Note: ``a2a_ask`` is the single colleague-surface tool; it is
    registered separately via
    :func:`crewlet.tools.colleague.register_colleague_tools` at engine
    startup.
    """
    expected = [
        "lookup_colleague",
        "use_skill",
        "load_tool_skill",
    ]
    for name in expected:
        assert registry.get(name) is not None, f"Missing tool: {name}"

    # ``store_knowledge`` / ``query_knowledge`` / ``read_doc`` are not
    # on the agent
    # surface.  Shared knowledge lives in Confluence, reached via the
    # ``confluence_search`` / ``confluence_get_page`` MCP tools;
    # personal facts go through ``reflect_and_persist``.
    for name in ("store_knowledge", "query_knowledge", "read_doc"):
        assert registry.get(name) is None, f"Should not be registered: {name}"

    # Channel-based A2A tools must not be registered.
    for name in ("request_a2a_channel", "send_a2a_message", "close_a2a_channel"):
        assert registry.get(name) is None, (
            f"Channel-based A2A tool should not be registered: {name}"
        )

    # Task management tools should NOT be registered
    for name in ("create_task", "update_task", "delegate", "assign_task", "list_tasks"):
        assert registry.get(name) is None, f"Should not be registered: {name}"
