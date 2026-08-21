"""Tests for MCP bridge — tool wrapping and multi-server management."""

import pytest

from crewlet.mcp.bridge import (
    MCPToolBridge,
    MCPToolWrapper,
    _render_block,
    mcp_instance_name,
)
from crewlet.tools.protocol import AgentContext


class FakeMCPClient:
    """Fake MCP client for testing the bridge without subprocesses."""

    def __init__(
        self,
        name: str,
        tools: list[dict] | None = None,
        content: list[dict] | None = None,
    ) -> None:
        self.name = name
        self._tools = tools or []
        self._content = content
        self._calls: list[tuple[str, dict]] = []
        self.started = False
        self.stopped = False

    async def start(self) -> None:
        self.started = True

    async def stop(self) -> None:
        self.stopped = True

    async def list_tools(self) -> list[dict]:
        return self._tools

    async def call_tool(self, tool_name: str, arguments: dict) -> list[dict]:
        self._calls.append((tool_name, arguments))
        if self._content is not None:
            return self._content
        return [{"type": "text", "text": f"Result of {tool_name}"}]


@pytest.fixture
def fake_client() -> FakeMCPClient:
    return FakeMCPClient(
        name="test-server",
        tools=[
            {
                "name": "search",
                "description": "Search for items",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "query": {"type": "string"},
                    },
                },
            },
            {
                "name": "create",
                "description": "Create an item",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "title": {"type": "string"},
                    },
                },
            },
        ],
    )


@pytest.fixture
def ctx() -> AgentContext:
    return AgentContext(agent_id="agent-1", role="Engineer")


# --- MCPToolWrapper ---


@pytest.mark.asyncio
async def test_tool_wrapper_properties(
    fake_client: FakeMCPClient,
):
    tool_def = {
        "name": "search",
        "description": "Search things",
        "inputSchema": {
            "type": "object",
            "properties": {"q": {"type": "string"}},
        },
    }
    wrapper = MCPToolWrapper(fake_client, tool_def)
    assert wrapper.name == "search"
    assert wrapper.description == "Search things"
    assert wrapper.parameters["type"] == "object"


@pytest.mark.asyncio
async def test_tool_wrapper_with_prefix(
    fake_client: FakeMCPClient,
):
    tool_def = {"name": "search", "description": "Search"}
    wrapper = MCPToolWrapper(fake_client, tool_def, name_prefix="jira_")
    assert wrapper.name == "jira_search"


def test_tool_wrapper_server_name_shared_no_prefix():
    """Shared MCP servers (no per-role namespacing) expose their bare
    name via ``server_name``."""
    client = FakeMCPClient(name="github")
    wrapper = MCPToolWrapper(client, {"name": "create_pr", "description": ""})
    assert wrapper.server_name == "github"


def test_tool_wrapper_server_name_strips_role_suffix():
    """Per-role MCP instances are constructed with names of the form
    ``"{server}::{Role_Name}"`` (see ``mcp_instance_name``). The
    ``server_name`` property strips the role suffix so the LLM sees
    the bare server name (the form it appears in YAML ``mcp_env``
    keys, the prompt's `### MCP servers` block, and the
    ``list_mcp_server_tools`` argument).
    """
    client = FakeMCPClient(name=mcp_instance_name("github", "Engineering"))
    assert client.name == "github::Engineering"
    wrapper = MCPToolWrapper(client, {"name": "create_pr", "description": ""})
    assert wrapper.server_name == "github"


def test_tool_wrapper_captures_mcp_annotations():
    """MCP behavioural hints survive the wrap so the engine can derive
    tool behaviour from the server (not a hardcoded name list)."""
    wrapper = MCPToolWrapper(
        FakeMCPClient(name="github"),
        {
            "name": "create_pr",
            "description": "Open a PR",
            "annotations": {"readOnlyHint": False, "openWorldHint": True},
        },
    )
    assert wrapper.annotations.read_only is False
    assert wrapper.annotations.open_world is True


def test_tool_wrapper_missing_annotations_is_all_unknown():
    wrapper = MCPToolWrapper(
        FakeMCPClient(name="github"),
        {"name": "search", "description": "Search"},
    )
    assert wrapper.annotations.read_only is None
    assert wrapper.annotations.open_world is None


def test_tool_wrapper_config_override_wins():
    """An operator override (keyed by bare tool name) layers over the
    server-advertised hint."""
    from crewlet.tools.capabilities import ToolAnnotations

    wrapper = MCPToolWrapper(
        FakeMCPClient(name="weird"),
        {
            "name": "weird_write",
            "description": "x",
            "annotations": {"readOnlyHint": True},  # server says read-only…
        },
        annotation_overrides={
            "weird_write": ToolAnnotations(read_only=False, open_world=True)
        },
    )
    # …but the operator knows better.
    assert wrapper.annotations.read_only is False
    assert wrapper.annotations.open_world is True


def test_tool_wrapper_override_matches_prefixed_name():
    """An override keyed by the **prefixed** catalogue name applies too —
    the engine prefixes Slack tools (`slack_`), and an operator who keys
    the override by the name they see in the catalogue must not have it
    silently no-op."""
    from crewlet.tools.capabilities import ToolAnnotations

    wrapper = MCPToolWrapper(
        FakeMCPClient(name="slack"),
        {"name": "conversations_add_message", "description": "x"},
        name_prefix="slack_",
        annotation_overrides={
            "slack_conversations_add_message": ToolAnnotations(
                read_only=False, open_world=True
            )
        },
    )
    assert wrapper.name == "slack_conversations_add_message"
    assert wrapper.annotations.read_only is False
    assert wrapper.annotations.open_world is True


@pytest.mark.asyncio
async def test_register_threads_annotation_overrides():
    bridge = MCPToolBridge()
    from crewlet.tools.capabilities import ToolAnnotations

    client = FakeMCPClient(
        name="linear",
        tools=[{"name": "linear_create_comment", "description": "Comment"}],
    )
    wrapped = await bridge._register(
        client,
        tool_prefix="",
        annotation_overrides={
            "linear_create_comment": ToolAnnotations(read_only=False, open_world=True)
        },
    )
    assert wrapped[0].annotations.read_only is False


@pytest.mark.asyncio
async def test_tool_wrapper_execute(fake_client: FakeMCPClient, ctx: AgentContext):
    tool_def = {"name": "search", "description": "Search"}
    wrapper = MCPToolWrapper(fake_client, tool_def)
    result = await wrapper.execute({"query": "test"}, ctx)
    assert result.success is True
    assert "Result of search" in result.output
    assert fake_client._calls == [("search", {"query": "test"})]


def test_render_block_text():
    assert _render_block({"type": "text", "text": "hi"}) == "hi"


def test_render_block_embedded_text_resource_surfaces_body():
    """An embedded text resource renders its body — the model must see
    the file contents, not an opaque ``{'type': 'resource'}`` string
    from a ``str(block)`` fallback."""
    rendered = _render_block(
        {
            "type": "resource",
            "uri": "file:///app/main.py",
            "mimeType": "text/x-python",
            "text": "print('hello')\n",
        }
    )
    assert rendered == "print('hello')\n"


def test_render_block_blob_resource_describes():
    rendered = _render_block(
        {
            "type": "resource",
            "uri": "file:///app/logo.png",
            "mimeType": "image/png",
            "blob": True,
        }
    )
    assert rendered == "[resource: file:///app/logo.png (image/png)]"


def test_render_block_resource_link_describes():
    rendered = _render_block(
        {
            "type": "resource_link",
            "uri": "https://example/file.txt",
            "name": "file.txt",
            "mimeType": "text/plain",
        }
    )
    assert rendered == "[resource_link: https://example/file.txt file.txt (text/plain)]"


def test_render_block_image_describes():
    rendered = _render_block({"type": "image", "mimeType": "image/png"})
    assert rendered == "[image: image/png]"


def test_render_block_audio_describes():
    rendered = _render_block({"type": "audio", "mimeType": "audio/wav"})
    assert rendered == "[audio: audio/wav]"


@pytest.mark.asyncio
async def test_tool_wrapper_execute_renders_embedded_resource(ctx: AgentContext):
    """End-to-end: a tool returning an embedded text resource (GitHub's
    ``get_file_contents`` shape) surfaces the file body to the agent."""
    client = FakeMCPClient(
        name="github",
        content=[
            {
                "type": "resource",
                "uri": "file:///README.md",
                "mimeType": "text/markdown",
                "text": "# Title\n\nbody text",
            }
        ],
    )
    wrapper = MCPToolWrapper(client, {"name": "get_file_contents", "description": ""})
    result = await wrapper.execute({"path": "README.md"}, ctx)
    assert result.success is True
    assert result.output == "# Title\n\nbody text"


@pytest.mark.asyncio
async def test_tool_wrapper_execute_error(
    ctx: AgentContext,
):
    class ErrorClient:
        name = "broken"

        async def call_tool(self, name: str, args: dict) -> list[dict]:
            msg = "Connection refused"
            raise RuntimeError(msg)

    tool_def = {"name": "fail_tool", "description": "Fails"}
    wrapper = MCPToolWrapper(ErrorClient(), tool_def)
    result = await wrapper.execute({}, ctx)
    assert result.success is False
    # Upstream error must land in `error`, not `output` — the LLM loop
    # reads `error` on failure and would otherwise show the agent the
    # generic "Tool execution failed" string instead of the real reason.
    assert result.error is not None
    assert "Connection refused" in result.error
    assert "broken/fail_tool" in result.error


# --- MCPToolBridge ---


@pytest.mark.asyncio
async def test_bridge_register_discovers_and_wraps(
    fake_client: FakeMCPClient,
):
    bridge = MCPToolBridge()
    wrapped = await bridge._register(fake_client, tool_prefix="")
    assert len(wrapped) == 2
    assert {t.name for t in wrapped} == {"search", "create"}

    # All tools accessible via get_all_tools
    assert len(bridge.get_all_tools()) == 2

    # Look up by name
    assert bridge.get_tool("search") is not None
    assert bridge.get_tool("nonexistent") is None


@pytest.mark.asyncio
async def test_bridge_discover_with_prefix(
    fake_client: FakeMCPClient,
):
    bridge = MCPToolBridge()
    wrapped = await bridge._register(fake_client, tool_prefix="jira_")
    assert {t.name for t in wrapped} == {
        "jira_search",
        "jira_create",
    }


@pytest.mark.asyncio
async def test_bridge_get_server_tools(
    fake_client: FakeMCPClient,
):
    bridge = MCPToolBridge()
    await bridge._register(fake_client, "")

    server_tools = bridge.get_server_tools("test-server")
    assert len(server_tools) == 2

    other_tools = bridge.get_server_tools("other-server")
    assert len(other_tools) == 0


@pytest.mark.asyncio
async def test_bridge_stop_all(fake_client: FakeMCPClient):
    bridge = MCPToolBridge()
    bridge._clients["test"] = fake_client
    await bridge.stop_all()
    assert fake_client.stopped is True
    assert len(bridge._clients) == 0
    assert len(bridge._tools) == 0


@pytest.mark.asyncio
async def test_bridge_stop_server(fake_client: FakeMCPClient):
    bridge = MCPToolBridge()
    bridge._clients["test-server"] = fake_client
    await bridge._register(fake_client, "")
    assert len(bridge._tools) == 2

    await bridge.stop_server("test-server")
    assert fake_client.stopped is True
    assert "test-server" not in bridge._clients
    assert len(bridge._tools) == 0


@pytest.mark.asyncio
async def test_bridge_get_client(fake_client: FakeMCPClient):
    bridge = MCPToolBridge()
    bridge._clients["test"] = fake_client
    assert bridge.get_client("test") is fake_client
    assert bridge.get_client("missing") is None


@pytest.mark.asyncio
async def test_bridge_has_client(fake_client: FakeMCPClient):
    bridge = MCPToolBridge()
    bridge._clients["test"] = fake_client
    assert bridge.has_client("test") is True
    assert bridge.has_client("missing") is False


def test_mcp_instance_name():
    assert mcp_instance_name("atlassian", "Engineer") == "atlassian::Engineer"
    assert mcp_instance_name("slack", "Senior Dev") == "slack::Senior_Dev"
    assert mcp_instance_name("jira", "PM") == "jira::PM"


@pytest.mark.asyncio
async def test_bridge_restart_http_server(monkeypatch, fake_client: FakeMCPClient):
    """``restart_http_server`` stops the prior client and reconnects via
    the HTTP path with the new url/headers — ``restart_server`` would
    have relaunched it as a stdio subprocess (empty command)."""
    bridge = MCPToolBridge()
    bridge._clients["remote"] = fake_client
    await bridge._register(fake_client, "")

    created: dict[str, object] = {}

    class _FakeHttp:
        def __init__(self, name, url, headers=None, **timeouts):
            created["name"], created["url"], created["headers"] = name, url, headers
            created.update(timeouts)
            self.name = name

        async def start(self) -> None:
            pass

        async def list_tools(self) -> list[dict]:
            return [{"name": "remote_tool", "description": "d", "inputSchema": {}}]

    monkeypatch.setattr("crewlet.mcp.bridge.MCPHttpClient", _FakeHttp)

    wrapped = await bridge.restart_http_server(
        name="remote",
        url="https://new.example/mcp",
        headers={"Authorization": "Bearer x"},
        tool_prefix="r_",
    )

    assert fake_client.stopped is True
    assert created["url"] == "https://new.example/mcp"
    assert created["headers"] == {"Authorization": "Bearer x"}
    assert [w.name for w in wrapped] == ["r_remote_tool"]


# ── a server that never speaks ─────────────────────────────────────


async def test_a_server_that_fails_discovery_is_not_registered():
    """Registering before discovery leaves a live process with no tools.

    ``has_client`` then answers yes for a server that serves nothing, so
    a live config edit sees it as healthy and the engine's own restart
    never fires — the process sits there until shutdown.
    """

    class _Boom(FakeMCPClient):
        async def list_tools(self) -> list[dict]:
            raise RuntimeError("server closed the pipe")

    bridge = MCPToolBridge()
    client = _Boom("jira")

    with pytest.raises(RuntimeError, match="closed the pipe"):
        await bridge._register(client, tool_prefix="")

    assert bridge.has_client("jira") is False
    assert client.stopped is True, "the subprocess must not be left running"


async def test_discovery_answers_to_the_startup_deadline():
    """A connected-but-mute server must fail, not hang.

    The engine starts MCP servers on the seat-acquisition path, so an
    await that never returns holds up every seat behind it — and its
    caller's ``except`` is dead code while it does.
    """
    from crewlet.mcp.client import MCPClient

    client = MCPClient(name="mute", command="true", startup_timeout_seconds=0.05)
    # Stand in for a connected session that never answers tools/list.
    client._initialized = True
    client._client = object()

    import crewlet.mcp.client as client_mod

    async def _never(_session):
        import asyncio

        await asyncio.Event().wait()

    original = client_mod.list_tool_dicts
    client_mod.list_tool_dicts = _never
    try:
        with pytest.raises(TimeoutError, match="tool discovery"):
            await client.list_tools()
    finally:
        client_mod.list_tool_dicts = original


async def test_timeouts_reach_the_client_from_the_server_config():
    """The per-server overrides are the point — a local checkout and a
    cold ``uvx`` fetch do not want the same deadline."""
    created: dict = {}

    class _Recording(FakeMCPClient):
        def __init__(self, name, command=None, args=None, env=None, **timeouts):
            super().__init__(name, tools=[])
            created.update(timeouts)

    import crewlet.mcp.bridge as bridge_mod

    original = bridge_mod.MCPClient
    bridge_mod.MCPClient = _Recording
    try:
        await MCPToolBridge().add_server(
            name="calc",
            command="uvx",
            startup_timeout_seconds=7.0,
            request_timeout_seconds=11.0,
        )
    finally:
        bridge_mod.MCPClient = original

    assert created == {
        "startup_timeout_seconds": 7.0,
        "request_timeout_seconds": 11.0,
    }


async def test_stop_all_does_not_strand_servers_behind_a_slow_one():
    """Shutdown budgets the whole step, not each server.

    Stopped in sequence, one server that will not die consumed the
    entire budget and every server after it in the dict was never
    stopped at all — its subprocess outlived the engine. They are
    independent processes, so the slowest one should bound the step,
    not the first slow one.
    """
    import asyncio

    order: list[str] = []

    class _Slow(FakeMCPClient):
        async def stop(self) -> None:
            await asyncio.sleep(0.2)
            order.append(self.name)
            self.stopped = True

    bridge = MCPToolBridge()
    # Five servers that each take 0.2 s: 1.0 s in sequence, 0.2 s
    # together. The engine's shutdown budget is per STEP, so the
    # sequential shape blew it and left the tail running.
    clients = [_Slow(f"server-{i}") for i in range(5)]
    for client in clients:
        bridge._clients[client.name] = client

    await asyncio.wait_for(bridge.stop_all(), timeout=0.6)

    assert all(c.stopped for c in clients), [c.name for c in clients if not c.stopped]
    assert bridge.has_client("server-4") is False


async def test_stop_all_drops_its_index_even_when_a_server_raises():
    """A restarted bridge must not believe a failed stop left it live."""

    class _Boom(FakeMCPClient):
        async def stop(self) -> None:
            raise RuntimeError("will not die")

    bridge = MCPToolBridge()
    bridge._clients["boom"] = _Boom("boom")
    bridge._clients["ok"] = FakeMCPClient("ok")

    await bridge.stop_all()

    assert bridge.has_client("boom") is False
    assert bridge.has_client("ok") is False
