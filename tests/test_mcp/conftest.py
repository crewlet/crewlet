"""Shared helpers for MCP tests.

Results and content blocks are **real** ``mcp.types`` objects and the SDK
client mock is **spec'd against the real ``mcp.Client``** — a migration
suite built on bare MagicMocks cannot catch SDK naming drift, because
auto-created attributes make any misspelling (``serverInfo``,
``isError``, …) silently "exist".  Real objects also make the
camelCase-dict conversion tests true round-trips: snake_case SDK fields
in, wire-format keys out.
"""

from unittest.mock import AsyncMock, MagicMock

from mcp import Client
from mcp.types import (
    BlobResourceContents,
    CallToolResult,
    ContentBlock,
    EmbeddedResource,
    ImageContent,
    ListToolsResult,
    ResourceLink,
    TextContent,
    TextResourceContents,
    Tool,
    ToolAnnotations,
)


def mock_tool(
    name: str,
    description: str = "",
    input_schema: dict | None = None,
    annotations: ToolAnnotations | None = None,
) -> Tool:
    """Create a real MCP SDK Tool."""
    return Tool(
        name=name,
        description=description,
        input_schema=input_schema or {"type": "object", "properties": {}},
        annotations=annotations,
    )


def mock_text_content(text: str) -> TextContent:
    return TextContent(text=text)


def mock_image_content(
    mime_type: str = "image/png",
    data: str = "base64data",
) -> ImageContent:
    return ImageContent(mime_type=mime_type, data=data)


def mock_other_content(block_type: str = "custom") -> MagicMock:
    """Create a stub content block with an arbitrary (unknown) type.

    This is the one helper that stays a mock: the union of real content
    block types can't represent a *future* block type, which is exactly
    the forward-compat path ``extract_content``'s catch-all handles.
    ``spec`` keeps only ``type`` present so the ``getattr(..., None)``
    probes stay honest.
    """
    block = MagicMock(spec=["type"])
    block.type = block_type
    return block


def mock_embedded_resource(
    text: str | None = None,
    uri: str = "file:///resource",
    mime_type: str = "text/plain",
    blob: str | None = None,
) -> EmbeddedResource:
    """Create a real EmbeddedResource (text or blob payload)."""
    if blob is not None:
        resource: TextResourceContents | BlobResourceContents = BlobResourceContents(
            uri=uri, mime_type=mime_type, blob=blob
        )
    else:
        resource = TextResourceContents(uri=uri, mime_type=mime_type, text=text or "")
    return EmbeddedResource(resource=resource)


def mock_resource_link(
    uri: str = "https://example/resource",
    name: str = "resource",
    mime_type: str = "text/plain",
) -> ResourceLink:
    return ResourceLink(uri=uri, name=name, mime_type=mime_type)


def mock_call_result(
    content_blocks: list[ContentBlock], is_error: bool = False
) -> CallToolResult:
    """Create a real CallToolResult.

    Tests exercising the unknown-content-type catch-all can't use this
    (the real union rejects unknown blocks) — they build a result stub
    inline instead.
    """
    return CallToolResult(content=content_blocks, is_error=is_error)


def mock_list_result(
    tools: list[Tool], next_cursor: str | None = None
) -> ListToolsResult:
    return ListToolsResult(tools=tools, next_cursor=next_cursor)


def mock_sdk_client(server_name: str | None = "test-server"):
    """Create a mock of the SDK's high-level ``mcp.Client`` (entered state).

    Spec'd against the real class so an attribute the 2.x Client does not
    have raises instead of auto-materializing.  ``server_name=None``
    models a modern (2026-era) server that does not identify itself —
    ``Client.server_info`` is None for those.
    """
    client = AsyncMock(spec=Client)
    if server_name is None:
        client.server_info = None
    else:
        client.server_info = MagicMock()
        client.server_info.name = server_name
    return client
