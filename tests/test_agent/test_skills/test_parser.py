"""Tests for ``crewlet.agent.skills.parser``."""

from __future__ import annotations

import textwrap
from pathlib import Path

import pytest

from crewlet.agent.skills.models import Phase
from crewlet.agent.skills.parser import (
    SkillParseError,
    build_skill,
    parse_skill,
    parse_skill_file,
)

EXAMPLES_DIR = Path(__file__).resolve().parents[3] / "examples" / "tool-skills"


def test_parses_every_bundled_example() -> None:
    files = sorted(EXAMPLES_DIR.glob("*.md"))
    assert files, "expected bundled example skills under examples/tool-skills/"
    for path in files:
        skill = parse_skill(path.read_text())
        assert skill.key
        assert skill.title
        assert skill.body
        assert skill.phases
        # ``required`` is a bool in every bundled example.  Most ship
        # enforced (the default — practices for the tools their trigger
        # names, read before use); a few are deliberately advisory
        # (``required: false``): ``code-runtime.md`` is Plan-phase
        # planning guidance that gates no specific tool, and
        # ``getting-unstuck.md`` / ``retrieval-research.md`` trigger on
        # the whole ``plane`` server, so enforcing them would gate every
        # Plane call — including pure reads — behind the load guard.
        assert isinstance(skill.required, bool), path.name
        advisory = {"code-runtime.md", "getting-unstuck.md", "retrieval-research.md"}
        if path.name not in advisory:
            assert skill.required is True, path.name


def test_parse_skill_file_extracts_frontmatter_and_body() -> None:
    text = textwrap.dedent(
        """\
        ---
        key: tool:x
        trigger:
          tool: x
        phases: [plan]
        title: X
        ---

        Body line one.

        Body line two.
        """
    )
    frontmatter, body = parse_skill_file(text)
    assert frontmatter == {
        "key": "tool:x",
        "trigger": {"tool": "x"},
        "phases": ["plan"],
        "title": "X",
    }
    assert body == "Body line one.\n\nBody line two."


def test_parse_skill_file_rejects_missing_frontmatter() -> None:
    with pytest.raises(SkillParseError, match="frontmatter"):
        parse_skill_file("just a body, no frontmatter\n")


def test_parse_skill_file_rejects_invalid_yaml() -> None:
    text = "---\nkey: [unbalanced\n---\nbody\n"
    with pytest.raises(SkillParseError, match="YAML"):
        parse_skill_file(text)


def test_parse_skill_file_rejects_non_mapping_frontmatter() -> None:
    text = "---\n- just\n- a\n- list\n---\nbody\n"
    with pytest.raises(SkillParseError, match="mapping"):
        parse_skill_file(text)


def test_build_skill_rejects_missing_required_keys() -> None:
    with pytest.raises(SkillParseError, match="required keys"):
        build_skill({"key": "x"}, "body")


def test_build_skill_rejects_invalid_trigger() -> None:
    with pytest.raises(SkillParseError, match="invalid skill definition"):
        build_skill(
            {
                "key": "tool:x",
                "trigger": {"mcp_server": "a", "tool": "b"},  # not exactly-one
                "phases": ["plan"],
                "title": "X",
                "summary": "summary",
            },
            "body",
        )


def test_build_skill_with_source_page_metadata() -> None:
    skill = build_skill(
        {
            "key": "tool:x",
            "trigger": {"tool": "x"},
            "phases": ["plan"],
            "title": "X",
            "summary": "Summary text.",
        },
        "body",
        source_page_id="12345",
        source_page_version=7,
    )
    assert skill.source_page_id == "12345"
    assert skill.source_page_version == 7
    assert skill.phases == {Phase.PLAN}
    assert skill.summary == "Summary text."


def test_build_skill_required_defaults_to_true_and_parses_false() -> None:
    base = {
        "key": "tool:x",
        "trigger": {"tool": "x"},
        "phases": ["plan"],
        "title": "X",
        "summary": "Summary text.",
    }
    assert build_skill(base, "body").required is True
    assert build_skill({**base, "required": True}, "body").required is True
    assert build_skill({**base, "required": False}, "body").required is False


def test_build_skill_rejects_non_boolean_required() -> None:
    with pytest.raises(SkillParseError, match="invalid skill definition"):
        build_skill(
            {
                "key": "tool:x",
                "trigger": {"tool": "x"},
                "phases": ["plan"],
                "title": "X",
                "summary": "Summary text.",
                "required": "banana",
            },
            "body",
        )


def test_parse_skill_reads_required_from_frontmatter() -> None:
    text = textwrap.dedent(
        """\
        ---
        key: mcp:github
        trigger:
          mcp_server: github
        phases: [plan, execute]
        required: true
        title: GitHub tools
        summary: How to GitHub.
        ---

        Body.
        """
    )
    assert parse_skill(text).required is True
