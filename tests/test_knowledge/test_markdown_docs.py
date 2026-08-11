"""Tests for the backend-neutral markdown-document helpers.

These cover the shared behaviour both publishers rely on — frontmatter
splitting, H1 title extraction, skill-vs-doc classification, ``.md``
collection, markdown rendering, HTML flattening, and the
operator-friendly frontmatter YAML dump.  Backend suites
(``tests/test_confluence``, ``tests/test_plane``) keep only their wire
formats.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from crewlet.knowledge.markdown_docs import (
    DocParseError,
    classify,
    collect_md_files,
    dump_frontmatter_yaml,
    extract_title,
    html_to_text,
    parse_doc_file,
    render_markdown,
    split_optional_frontmatter,
)

# ---------------------------------------------------------------------------
# split_optional_frontmatter
# ---------------------------------------------------------------------------


def test_split_optional_frontmatter_tolerates_absence() -> None:
    # A frontmatter-less file → empty mapping + the text unchanged.
    text = "# Title\n\nbody"
    assert split_optional_frontmatter(text) == ({}, text)
    # A present block → (mapping, remaining body).
    fm, body = split_optional_frontmatter("---\ntitle: T\n---\n\n# H1\n\nbody")
    assert fm == {"title": "T"}
    assert body == "# H1\n\nbody"


def test_split_optional_frontmatter_empty_block_is_empty_mapping() -> None:
    fm, body = split_optional_frontmatter("---\n\n---\nbody\n")
    assert fm == {}
    assert body == "body\n"


def test_split_optional_frontmatter_malformed_yaml_raises() -> None:
    with pytest.raises(DocParseError, match="YAML"):
        split_optional_frontmatter("---\nkey: [unbalanced\n---\nbody\n")


def test_split_optional_frontmatter_non_mapping_raises() -> None:
    with pytest.raises(DocParseError, match="mapping"):
        split_optional_frontmatter("---\n- a\n- b\n---\nbody\n")


# ---------------------------------------------------------------------------
# extract_title / parse_doc_file
# ---------------------------------------------------------------------------


def test_extract_title_from_leading_h1_strips_it() -> None:
    title, body = extract_title({}, "# My Title\n\nBody text.\n")
    assert title == "My Title"
    assert body == "Body text.\n"


def test_extract_title_frontmatter_override_still_strips_h1() -> None:
    title, body = extract_title({"title": "Override"}, "# Ignored\n\nBody.\n")
    assert title == "Override"
    assert "Ignored" not in body


def test_extract_title_tolerates_leading_blank_lines() -> None:
    title, _body = extract_title({}, "\n\n# After Blanks\nBody.\n")
    assert title == "After Blanks"


def test_extract_title_ignores_mid_body_heading() -> None:
    # A ``# `` further down is real content, not the title.
    with pytest.raises(DocParseError, match="no title"):
        extract_title({}, "prose first\n\n# Not A Title\n")


def test_extract_title_missing_raises() -> None:
    with pytest.raises(DocParseError, match="no title"):
        extract_title({}, "just prose, no heading\n")


def test_parse_doc_file_composes_split_and_title() -> None:
    fm, title, body = parse_doc_file(
        "---\nlabels: [x]\n---\n\n# Doc Title\n\nBody line.\n"
    )
    assert fm == {"labels": ["x"]}
    assert title == "Doc Title"
    assert body == "Body line.\n"


# ---------------------------------------------------------------------------
# classify
# ---------------------------------------------------------------------------


def test_classify_routes_by_trigger() -> None:
    # trigger present → skill; everything else → doc (the container
    # comes from the directory, never from frontmatter).
    assert classify({"trigger": {"tool": "x"}}) == "skill"
    assert classify({"title": "T"}) == "doc"
    assert classify({}) == "doc"


# ---------------------------------------------------------------------------
# collect_md_files
# ---------------------------------------------------------------------------


def test_collect_md_files_walks_recursively_and_dedupes(tmp_path: Path) -> None:
    (tmp_path / "a").mkdir()
    (tmp_path / "a" / "one.md").write_text("# One\n")
    (tmp_path / "a" / "b").mkdir()
    (tmp_path / "a" / "b" / "two.md").write_text("# Two\n")
    (tmp_path / "a" / "ignore.txt").write_text("not markdown")
    single = tmp_path / "a" / "one.md"

    # Directory + an overlapping explicit file → deduped, sorted.
    out = collect_md_files([tmp_path, single])
    assert out == sorted(
        [
            (tmp_path / "a" / "one.md").resolve(),
            (tmp_path / "a" / "b" / "two.md").resolve(),
        ]
    )


def test_collect_md_files_single_file_and_nonexistent(tmp_path: Path) -> None:
    md = tmp_path / "solo.md"
    md.write_text("# Solo\n")
    assert collect_md_files([md]) == [md.resolve()]
    assert collect_md_files([tmp_path / "nope"]) == []
    assert collect_md_files([tmp_path / "solo.txt"]) == []


# ---------------------------------------------------------------------------
# render_markdown / html_to_text
# ---------------------------------------------------------------------------


def test_render_markdown_produces_xhtml_with_extensions() -> None:
    html = render_markdown("## Head\n\npara\n\n```py\ncode()\n```\n")
    assert "<h2>Head</h2>" in html
    assert "<p>para</p>" in html
    assert "<code" in html  # fenced_code extension active


def test_render_markdown_tables_extension() -> None:
    html = render_markdown("| a | b |\n|---|---|\n| 1 | 2 |\n")
    assert "<table>" in html


def test_html_to_text_flattens_paragraphs_and_lists() -> None:
    text = html_to_text("<p>Intro.</p><ul><li>first</li><li>second</li></ul>")
    assert "Intro." in text
    assert "- first" in text
    assert "- second" in text


def test_html_to_text_headings_and_inline_markup() -> None:
    text = html_to_text("<h2>Head</h2><p>Use <code>tool</code> <b>now</b>.</p>")
    assert "Head" in text
    assert "Use tool now." in text
    assert "<" not in text


def test_html_to_text_strips_unknown_wrappers_to_content() -> None:
    text = html_to_text("<ac:whatever>inner text</ac:whatever><br/>next")
    assert "inner text" in text
    assert "next" in text


# ---------------------------------------------------------------------------
# dump_frontmatter_yaml
# ---------------------------------------------------------------------------


def test_dump_uses_literal_block_style_for_multiline_strings() -> None:
    out = dump_frontmatter_yaml(
        {"key": "skill:demo", "summary": "line one\nline two\nline three"}
    )
    assert "summary: |" in out
    assert "  line one" in out
    assert "'line one" not in out  # not single-quoted


def test_dump_keeps_short_strings_unquoted_and_key_order() -> None:
    out = dump_frontmatter_yaml({"zeta": "z", "alpha": "Short title"})
    assert "alpha: Short title" in out
    assert "'Short title'" not in out
    # sort_keys=False — authored order preserved.
    assert out.index("zeta") < out.index("alpha")


def test_dump_passes_unicode_through_and_strips_trailing_whitespace() -> None:
    out = dump_frontmatter_yaml({"title": "café ☕"})
    assert "café ☕" in out
    assert out == out.rstrip()
