"""Tests for the Plane ``description_html`` codec.

The wire format is a leading ``<pre><code class="language-yaml">``
block (frontmatter, HTML-escaped) + rendered markdown body.  Decode is
TOLERANT of Plane's live editor re-serialising the block (attribute
variants, dropped language class, leading whitespace) — the fork's
sanitizer keeps ``pre``/``code`` with their ``class`` attributes
intact, so the block itself always survives.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from crewlet.agent.skills.parser import parse_skill_file
from crewlet.plane.plane_codec import (
    CodecDecodeError,
    decode_page,
    encode_doc,
    encode_page,
)

EXAMPLES_DIR = Path(__file__).resolve().parents[2] / "examples" / "tool-skills"


def test_encode_emits_leading_yaml_code_block() -> None:
    html = encode_page({"key": "tool:x", "trigger": {"tool": "x"}}, "Body line.")
    assert html.startswith('<pre><code class="language-yaml">')
    assert "key: tool:x" in html
    assert "</code></pre>" in html
    assert "Body line." in html


def test_encode_escapes_html_special_chars_in_frontmatter() -> None:
    html = encode_page(
        {"key": "tool:x", "trigger": {"tool": "x"}, "summary": "a < b & c"},
        "body",
    )
    # The raw ``<`` / ``&`` must not appear inside the code block — they
    # would confuse both the HTML layer and the closing-tag scan.
    yaml_block = html.split("</code></pre>")[0]
    assert "a &lt; b &amp; c" in yaml_block
    assert "a < b & c" not in yaml_block


def test_round_trip_preserves_frontmatter_values() -> None:
    frontmatter = {
        "key": "skill:demo",
        "trigger": {"tool": "x", "mcp_server": "plane"},
        "phases": ["plan", "execute"],
        "required": True,
        "title": "Colons: and #hashes — unicode ☕",
        "summary": "line one\nline two <with> &special; chars\nline three",
    }
    decoded_fm, decoded_body = decode_page(encode_page(frontmatter, "The body."))
    assert decoded_fm == frontmatter
    assert "The body." in decoded_body


def test_round_trip_preserves_frontmatter_for_every_bundled_example() -> None:
    """Every shipped example file survives parse → encode → decode with
    its frontmatter intact (mirror of the Confluence codec test)."""
    files = sorted(EXAMPLES_DIR.glob("*.md"))
    assert files, "expected bundled example skills under examples/tool-skills/"
    for path in files:
        frontmatter, body = parse_skill_file(path.read_text())
        html = encode_page(frontmatter, body)
        decoded_fm, decoded_body = decode_page(html)
        assert decoded_fm["key"] == frontmatter["key"]
        assert decoded_fm["trigger"] == frontmatter["trigger"]
        assert set(decoded_fm["phases"]) == set(frontmatter["phases"])
        assert decoded_fm["title"] == frontmatter["title"]
        assert decoded_fm.get("required") == frontmatter.get("required")
        assert decoded_body.strip()


def test_decode_tolerates_editor_rewritten_attributes() -> None:
    """Plane's live editor may re-serialise the block with different
    attributes — the decode must still find the frontmatter."""
    html = (
        '<pre class="code-block" data-x="1">'
        '<code spellcheck="false" class="language-yaml">'
        "key: tool:x\ntrigger:\n  tool: x\n"
        "</code></pre><p>Body.</p>"
    )
    fm, body = decode_page(html)
    assert fm["key"] == "tool:x"
    assert "Body." in body


def test_decode_tolerates_missing_language_class() -> None:
    html = "<pre><code>key: tool:x\ntrigger:\n  tool: x\n</code></pre><p>B.</p>"
    fm, _body = decode_page(html)
    assert fm == {"key": "tool:x", "trigger": {"tool": "x"}}


def test_decode_tolerates_leading_whitespace_and_newlines() -> None:
    html = '\n\n  <pre><code class="language-yaml">key: k\ntrigger: {}\n</code></pre>'
    fm, _body = decode_page(html)
    assert fm["key"] == "k"


def test_decode_missing_block_raises() -> None:
    with pytest.raises(CodecDecodeError, match="leading"):
        decode_page("<p>just a body</p>")
    with pytest.raises(CodecDecodeError, match="leading"):
        decode_page("")


def test_decode_block_not_at_document_start_raises() -> None:
    # A code block further down the body is content, not metadata.
    with pytest.raises(CodecDecodeError, match="leading"):
        decode_page("<p>intro</p><pre><code>key: k\n</code></pre>")


def test_decode_non_mapping_yaml_raises() -> None:
    with pytest.raises(CodecDecodeError, match="mapping"):
        decode_page("<pre><code>- a\n- b\n</code></pre>")


def test_decode_malformed_yaml_raises() -> None:
    with pytest.raises(CodecDecodeError, match="YAML"):
        decode_page("<pre><code>key: [unbalanced\n</code></pre>")


def test_decode_flattens_body_lists_and_inline_markup() -> None:
    html = encode_page(
        {"key": "tool:x", "trigger": {"tool": "x"}},
        "- first item\n- second item\n\nUse `query_knowledge` **carefully**.",
    )
    _, body = decode_page(html)
    assert "- first item" in body
    assert "- second item" in body
    assert "query_knowledge" in body
    assert "carefully" in body


def test_encode_doc_is_clean_prose_without_code_block() -> None:
    html = encode_doc("## Section\n\nProse body.\n")
    assert "<pre>" not in html
    assert "<h2>Section</h2>" in html
    assert "Prose body." in html
