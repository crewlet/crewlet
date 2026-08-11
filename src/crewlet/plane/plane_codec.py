"""Markdown ↔ Plane ``description_html`` round-trip for engine pages.

The wire format for a Tool Skill page on Plane is a leading
``<pre><code class="language-yaml">`` block holding the YAML frontmatter
(rendered — and editable — as a code block in Plane's tiptap editor)
followed by the markdown body rendered to HTML.  Knowledge docs are
clean prose with no YAML block (:func:`encode_doc`).

On import (local file → Plane): :func:`encode_page` builds the
``description_html``.  On sync (Plane → in-memory skill):
:func:`decode_page` extracts the frontmatter and flattens the body HTML
to plain text the LLM can consume — deliberately lossy, exactly like
the Confluence codec (:mod:`crewlet.agent.skills.confluence_codec`).

Three fork facts make this wire format safe — pinned from the fork's
``preview`` tag, and the whole reason the codec works:

1. **The sanitizer keeps the block intact.**  The fork's pages API
   sanitizes inbound
   ``description_html`` with the same validator the internal app uses
   (``plane/utils/content_validator.py``): ``ALLOWED_TAGS =
   nh3.ALLOWED_TAGS | CUSTOM_TAGS`` with ``class`` allowed globally
   plus ``pre: {language}`` and ``code: {language, spellcheck}``
   attributes (10 MB cap) — so ``<pre><code class="language-yaml">``
   survives sanitization unchanged.
2. **The live editor regenerates from the HTML.**  Every content
   write through the fork's pages API sets ``description_binary =
   None``, so the Yjs collaboration
   service rebuilds its binary document from the HTML we posted — the
   YAML block round-trips through Plane's editor as a real code block
   (the editor may re-serialise tag attributes, which is why
   :func:`decode_page` is tolerant of attribute variants).
3. **Search sees the current content.**  ``Page.save()`` re-derives
   ``description_stripped`` from ``description_html`` (see the fork's
   ``touch_page`` docstring), so the page search indexes exactly what
   was written.
"""

from __future__ import annotations

import html as htmllib
import re
from typing import Any

import yaml

from crewlet.knowledge.markdown_docs import (
    dump_frontmatter_yaml,
    html_to_text,
    render_markdown,
)

_LEADING_CODE_BLOCK_RE = re.compile(
    r"\A\s*<pre\b[^>]*>\s*<code\b[^>]*>(?P<yaml>.*?)</code>\s*</pre>",
    re.DOTALL | re.IGNORECASE,
)
"""Matches the leading ``<pre><code …>`` block carrying the frontmatter.

TOLERANT of editor re-serialisation: leading whitespace/newlines are
allowed, and both tags accept arbitrary attributes (``[^>]*``) because
Plane's live editor may rewrite or drop the ``language-yaml`` class when
a page is saved from the UI.  Anchored at document start (``\\A``) so a
code block further down the body is never mistaken for metadata.
"""


class CodecDecodeError(ValueError):
    """Raised when a Plane page body can't be parsed as a skill."""


def encode_page(frontmatter: dict[str, Any], body_markdown: str) -> str:
    """Build the Plane ``description_html`` for a skill page.

    Layout:

    .. code-block:: text

        <pre><code class="language-yaml">
        <frontmatter yaml, HTML-escaped>
        </code></pre>
        <rendered markdown body as HTML>

    The YAML text is HTML-escaped so ``<`` / ``&`` / quotes inside
    frontmatter values survive the HTML layer verbatim (the decode path
    unescapes symmetrically).
    """
    frontmatter_yaml = dump_frontmatter_yaml(frontmatter)
    return (
        '<pre><code class="language-yaml">\n'
        + htmllib.escape(frontmatter_yaml)
        + "\n</code></pre>\n"
        + render_markdown(body_markdown)
    )


def decode_page(description_html: str) -> tuple[dict[str, Any], str]:
    """Inverse of :func:`encode_page`.

    Returns ``(frontmatter_dict, body_plaintext)``.  The body is plain
    text with newlines preserved for paragraphs / list items; markdown
    formatting is not reconstructed (the LLM doesn't need it).

    Raises :class:`CodecDecodeError` if the leading code block is
    missing (i.e. the page is not a skill page — the project home page,
    a stray operator page) or the YAML inside it is malformed.
    """
    match = _LEADING_CODE_BLOCK_RE.match(description_html or "")
    if not match:
        raise CodecDecodeError(
            "skill page is missing the leading <pre><code> block that "
            "carries the YAML frontmatter"
        )
    try:
        frontmatter = yaml.safe_load(htmllib.unescape(match.group("yaml")))
    except yaml.YAMLError as exc:
        raise CodecDecodeError(f"invalid YAML in frontmatter: {exc}") from exc
    if not isinstance(frontmatter, dict):
        raise CodecDecodeError("frontmatter must be a YAML mapping")

    remainder = description_html[match.end() :]
    body_text = html_to_text(remainder)
    return frontmatter, body_text


def encode_doc(body_markdown: str) -> str:
    """Render a knowledge-doc body to Plane ``description_html``.

    Clean prose — no YAML code block.  Uses the same markdown renderer
    the skill codec uses so doc pages and skill bodies convert
    identically (and the Confluence encoders share it too — see
    :func:`crewlet.knowledge.markdown_docs.render_markdown`).
    """
    return render_markdown(body_markdown)


__all__ = ["CodecDecodeError", "decode_page", "encode_doc", "encode_page"]
