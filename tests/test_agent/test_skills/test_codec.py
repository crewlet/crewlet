"""Tests for the Confluence storage codec."""

from __future__ import annotations

from pathlib import Path

import pytest

from crewlet.agent.skills.confluence_codec import (
    CodecDecodeError,
    decode_page,
    encode_page,
)
from crewlet.agent.skills.parser import parse_skill_file

EXAMPLES_DIR = Path(__file__).resolve().parents[3] / "examples" / "tool-skills"


def test_encode_emits_leading_code_macro_with_yaml_frontmatter() -> None:
    storage = encode_page(
        {"key": "tool:x", "trigger": {"tool": "x"}},
        "Body line.",
    )
    # Confluence Cloud rejects the body with a 400 if ac:schema-version
    # is missing on the macro, so the encoder must include it.
    assert storage.startswith(
        '<ac:structured-macro ac:name="code" ac:schema-version="1">'
    )
    assert '<ac:parameter ac:name="language">yaml</ac:parameter>' in storage
    assert "<![CDATA[key: tool:x" in storage
    assert "Body line." in storage


def test_decode_extracts_frontmatter_and_body() -> None:
    storage = encode_page(
        {"key": "tool:x", "trigger": {"tool": "x"}, "phases": ["plan"]},
        "Run `query_knowledge` carefully.",
    )
    frontmatter, body = decode_page(storage)
    assert frontmatter == {
        "key": "tool:x",
        "trigger": {"tool": "x"},
        "phases": ["plan"],
    }
    assert "Run query_knowledge carefully." in body


def test_decode_raises_when_code_macro_missing() -> None:
    with pytest.raises(CodecDecodeError, match="leading"):
        decode_page("<p>just a body</p>")


def test_decode_raises_when_yaml_malformed() -> None:
    storage = (
        '<ac:structured-macro ac:name="code">'
        '<ac:parameter ac:name="language">yaml</ac:parameter>'
        "<ac:plain-text-body><![CDATA[key: [unbalanced]]></ac:plain-text-body>"
        "</ac:structured-macro>"
    )
    with pytest.raises(CodecDecodeError, match="YAML"):
        decode_page(storage)


def test_round_trip_preserves_frontmatter_for_every_bundled_example() -> None:
    """Every shipped example file survives parse → encode → decode with
    its frontmatter intact."""
    files = sorted(EXAMPLES_DIR.glob("*.md"))
    assert files, "expected bundled example skills under examples/tool-skills/"
    for path in files:
        frontmatter, body = parse_skill_file(path.read_text())
        storage = encode_page(frontmatter, body)
        decoded_fm, decoded_body = decode_page(storage)
        assert decoded_fm["key"] == frontmatter["key"]
        assert decoded_fm["trigger"] == frontmatter["trigger"]
        assert set(decoded_fm["phases"]) == set(frontmatter["phases"])
        assert decoded_fm["title"] == frontmatter["title"]
        assert decoded_fm.get("required") == frontmatter.get("required")
        # Body's plain-text form must at least contain something
        # recognisable from the source — exact fidelity is not a goal.
        assert decoded_body.strip()


def test_round_trip_preserves_required_flag() -> None:
    """The ``required`` safeguard marking survives the Confluence
    storage round-trip — an operator marking a skill required in the
    source file (or directly in the page's YAML box) gets enforcement
    after the next sync."""
    storage = encode_page(
        {
            "key": "mcp:github",
            "trigger": {"mcp_server": "github"},
            "phases": ["plan", "execute"],
            "required": True,
            "title": "GitHub tools",
            "summary": "How to GitHub.",
        },
        "Body.",
    )
    frontmatter, _ = decode_page(storage)
    assert frontmatter["required"] is True


def test_decode_converts_list_items_to_text_bullets() -> None:
    storage = encode_page(
        {"key": "tool:x", "trigger": {"tool": "x"}},
        "- first item\n- second item\n",
    )
    _, body = decode_page(storage)
    assert "- first item" in body
    assert "- second item" in body


def test_encode_uses_literal_block_style_for_multiline_strings() -> None:
    """PyYAML's default str representer single-quotes multi-line scalars
    with awkward folded lines.  The codec switches to literal ``|`` style
    so the Confluence-rendered YAML block reads cleanly and matches the
    source file's authoring style."""
    storage = encode_page(
        {
            "key": "skill:demo",
            "trigger": {"tool": "x"},
            "summary": "line one\nline two\nline three",
        },
        "body",
    )
    # Multi-line summary renders as a block literal, not single-quoted.
    assert "summary: |" in storage
    assert "  line one" in storage
    assert "'line one" not in storage  # not single-quoted


def test_encode_keeps_short_strings_unquoted() -> None:
    """Single-line scalars should still render plain — no unnecessary
    quoting clutter."""
    storage = encode_page(
        {"key": "tool:x", "trigger": {"tool": "x"}, "title": "Short title"},
        "body",
    )
    assert "title: Short title" in storage
    assert "'Short title'" not in storage


def test_decode_handles_inline_code_and_emphasis() -> None:
    storage = encode_page(
        {"key": "tool:x", "trigger": {"tool": "x"}},
        "Use `query_knowledge` **carefully**.",
    )
    _, body = decode_page(storage)
    assert "query_knowledge" in body
    assert "carefully" in body
