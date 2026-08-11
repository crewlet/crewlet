"""Shared MCP SDK result conversion — content blocks and tool listings.

The MCP SDK returns typed Pydantic objects; Crewlet's bridge layer works
with plain dicts so that the rest of the codebase stays decoupled from the
SDK's type hierarchy.  The dict shapes deliberately keep the MCP *wire
format* keys (camelCase ``inputSchema`` / ``mimeType`` / ``readOnlyHint``)
even though the 2.x SDK renamed the Python attributes to snake_case —
that wire shape is Crewlet's internal contract (consumed by
``mcp/bridge.py``, ``tools/capabilities.py``, and config annotation
overrides), and it matches what the MCP spec itself serializes.
"""

from __future__ import annotations

from typing import Any


def extract_content(
    blocks: list[Any],
) -> list[dict[str, Any]]:
    """Convert MCP SDK content blocks to plain dicts.

    The MCP SDK returns typed Pydantic objects (``TextContent``,
    ``ImageContent``, ``EmbeddedResource``, ``ResourceLink``, …).

    ``EmbeddedResource`` carries the actual payload on ``block.resource``
    (a ``TextResourceContents`` with ``.text`` or a ``BlobResourceContents``
    with ``.blob``).  Earlier this fell into the catch-all that kept only
    ``{"type": "resource"}`` and silently dropped the body — so a tool like
    GitHub's ``get_file_contents``, which returns the file as an embedded
    text resource, reached the model as the opaque string
    ``{'type': 'resource'}`` and left it blind to the contents.  We now
    surface the embedded text (and describe binary blobs / links).
    """
    content: list[dict[str, Any]] = []
    for block in blocks:
        btype = getattr(block, "type", "") or "unknown"
        if btype == "text":
            content.append({"type": "text", "text": block.text})
        elif btype in ("image", "audio"):
            content.append(
                {
                    "type": btype,
                    "data": getattr(block, "data", ""),
                    "mimeType": getattr(block, "mime_type", "unknown"),
                }
            )
        elif btype == "resource":
            res = getattr(block, "resource", None)
            entry: dict[str, Any] = {"type": "resource"}
            uri = getattr(res, "uri", "")
            if uri:
                entry["uri"] = str(uri)
            mime = getattr(res, "mime_type", "")
            if mime:
                entry["mimeType"] = mime
            text = getattr(res, "text", None)
            if text is not None:
                entry["text"] = text
            elif getattr(res, "blob", None) is not None:
                entry["blob"] = True
            content.append(entry)
        elif btype == "resource_link":
            entry = {"type": "resource_link"}
            uri = getattr(block, "uri", "")
            if uri:
                entry["uri"] = str(uri)
            name = getattr(block, "name", "")
            if name:
                entry["name"] = name
            mime = getattr(block, "mime_type", "")
            if mime:
                entry["mimeType"] = mime
            content.append(entry)
        else:
            content.append({"type": btype})
    return content


#: Ceiling on ``tools/list`` pages per listing.  Real servers expose at
#: most a few hundred tools and never split a listing this finely, so a
#: walk this long means the server's pagination is broken (e.g. it
#: ignores the ``cursor`` request param and re-serves page one forever).
_MAX_TOOL_PAGES = 100


async def list_tool_dicts(client: Any) -> list[dict[str, Any]]:
    """Fetch **every** tool from an ``mcp.Client``, following pagination.

    Earlier the wrappers took a single ``tools/list`` page and stopped, so
    a server that paginates its listing silently lost every tool past page
    one.  This walks ``next_cursor`` until the server reports the listing
    complete.

    The walk is defensive about broken pagination — the cursor is an
    opaque token minted by the (third-party) server, and trusting it
    blindly turns a server bug into an engine hang:

    - a falsy cursor (``None`` per spec, ``""`` from sloppy serializers)
      ends the listing;
    - a cursor the walk has already sent (echo / cycle) raises;
    - more than ``_MAX_TOOL_PAGES`` pages raises.

    Raising — rather than silently truncating — hands the failure to the
    per-server startup error handling, which reports the server as broken
    instead of registering a partial or duplicated tool set.

    Annotations are dumped ``by_alias=True`` so the dict carries the
    spec's camelCase hint keys (``readOnlyHint`` / ``destructiveHint`` /
    ``openWorldHint`` / …) that ``tools/capabilities.py`` and config
    annotation overrides are written against.
    """
    tools: list[dict[str, Any]] = []
    cursor: str | None = None
    seen_cursors: set[str] = set()
    for _ in range(_MAX_TOOL_PAGES):
        result = await client.list_tools(cursor=cursor)
        for t in result.tools:
            tools.append(
                {
                    "name": t.name,
                    "description": (t.description or f"MCP tool: {t.name}"),
                    "inputSchema": t.input_schema
                    or {"type": "object", "properties": {}},
                    "annotations": (
                        t.annotations.model_dump(by_alias=True)
                        if getattr(t, "annotations", None) is not None
                        else None
                    ),
                }
            )
        cursor = result.next_cursor
        if not cursor:
            return tools
        if cursor in seen_cursors:
            msg = (
                "MCP server pagination is broken: tools/list repeated "
                f"cursor {cursor!r}"
            )
            raise RuntimeError(msg)
        seen_cursors.add(cursor)
    msg = (
        f"MCP server pagination is broken: tools/list exceeded {_MAX_TOOL_PAGES} pages"
    )
    raise RuntimeError(msg)
