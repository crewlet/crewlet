"""Markdown ↔ Confluence storage XHTML round-trip for tool-skill pages.

The wire format for a skill page is a leading Confluence ``code`` macro
holding the YAML frontmatter (so operators can edit binding metadata
in Confluence's editor) followed by the markdown body rendered to
Confluence storage XHTML (so the rest of the page renders as proper
prose, lists, code blocks, etc.).

On import (local file → Confluence): :func:`encode_page` builds the
storage XHTML.  On sync (Confluence → in-memory skill):
:func:`decode_page` extracts the frontmatter and converts the body
XHTML back to plain text the LLM can consume.  The round-trip is
intentionally lossy on body formatting (bullets and headings flatten
to text-with-newlines) because skill prose is read by an LLM, not a
human, and preserving exact markdown shape across the storage layer
isn't worth a structured-conversion library.

The backend-neutral pieces — markdown rendering, HTML → text
flattening, the operator-friendly YAML dump — live in
:mod:`crewlet.knowledge.markdown_docs`, shared with the Plane codec
(:mod:`crewlet.plane.plane_codec`).  This module owns only the
Confluence macro framing.
"""

from __future__ import annotations

import re
from typing import Any

import yaml

from crewlet.knowledge.markdown_docs import (
    dump_frontmatter_yaml,
    html_to_text,
    render_markdown,
)

_CODE_MACRO_RE = re.compile(
    r"<ac:structured-macro[^>]*ac:name=\"code\"[^>]*>"
    r".*?"
    r"<ac:plain-text-body>\s*<!\[CDATA\[(?P<body>.*?)\]\]>\s*</ac:plain-text-body>"
    r"\s*</ac:structured-macro>",
    re.DOTALL,
)
"""Captures the verbatim CDATA content of the leading code macro.

Confluence's storage format wraps the macro body in
``<![CDATA[...]]>`` so YAML colons / brackets / unicode survive
unchanged.
"""


def encode_page(frontmatter: dict[str, Any], body_markdown: str) -> str:
    """Build the Confluence storage XHTML for a skill page.

    Layout:

    .. code-block:: text

        <ac:structured-macro ac:name="code">
          <ac:parameter ac:name="language">yaml</ac:parameter>
          <ac:plain-text-body><![CDATA[<frontmatter yaml>]]></ac:plain-text-body>
        </ac:structured-macro>
        <rendered markdown body as Confluence storage XHTML>
    """
    frontmatter_yaml = dump_frontmatter_yaml(frontmatter)
    rendered_body = render_markdown(body_markdown)
    # ``ac:schema-version="1"`` is required by Confluence Cloud's
    # storage-format parser on macros; Cloud rejects the body with a
    # bare 400 otherwise.
    return (
        '<ac:structured-macro ac:name="code" ac:schema-version="1">'
        '<ac:parameter ac:name="language">yaml</ac:parameter>'
        f"<ac:plain-text-body><![CDATA[{frontmatter_yaml}]]></ac:plain-text-body>"
        "</ac:structured-macro>"
        f"\n{rendered_body}"
    )


class CodecDecodeError(ValueError):
    """Raised when a Confluence page body can't be parsed as a skill."""


def decode_page(storage_xhtml: str) -> tuple[dict[str, Any], str]:
    """Inverse of :func:`encode_page`.

    Returns ``(frontmatter_dict, body_plaintext)``. The body is plain
    text with newlines preserved for paragraphs / list items; markdown
    formatting is not reconstructed (the LLM doesn't need it).

    Raises :class:`CodecDecodeError` if the leading code macro is
    missing or the YAML inside it is malformed.
    """
    match = _CODE_MACRO_RE.search(storage_xhtml)
    if not match:
        raise CodecDecodeError(
            "skill page is missing the leading <ac:structured-macro "
            'name="code"> block that carries the YAML frontmatter'
        )
    try:
        frontmatter = yaml.safe_load(match.group("body"))
    except yaml.YAMLError as exc:
        raise CodecDecodeError(f"invalid YAML in frontmatter: {exc}") from exc
    if not isinstance(frontmatter, dict):
        raise CodecDecodeError("frontmatter must be a YAML mapping")

    remainder = storage_xhtml[match.end() :]
    body_text = html_to_text(remainder)
    return frontmatter, body_text


__all__ = ["CodecDecodeError", "decode_page", "encode_page"]
