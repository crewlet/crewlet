"""Structural tests for the shipped authoring skills under ``skills/``.

These are founder-facing assets an AI assistant loads to author a
company config — distinct from ``examples/tool-skills/``, which are the
knowledge-base-sourced fragments the *engine's own agents* load at
runtime (see ``docs/concepts/tool-skills.md``).

A skill that points at a moved doc or a renamed CLI flag is worse than
no skill: the assistant follows it confidently and produces a broken
config. These tests fail the build instead.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest
import yaml

from crewlet.cli import _build_parser, build_config_schema

_REPO_ROOT = Path(__file__).resolve().parents[2]
_SKILLS_DIR = _REPO_ROOT / "skills"

_SKILL_FILES = sorted(_SKILLS_DIR.glob("*/SKILL.md"))

#: ``](../relative/path)`` markdown links — banned in a skill, which
#: must work when fetched away from the repository.
_REL_LINK_RE = re.compile(r"\]\((\.\.?/[^)#\s]+)")

#: Absolute links into the documentation site.  Both page links and the
#: two JSON Schema URLs match; the suffix is what tells them apart.
_DOCS_LINK_RE = re.compile(r"https://docs\.crewlet\.ai(/[^)>\s]*)")

#: Fenced ``bash`` blocks, whose ``crewlet …`` lines must be real
#: commands — a founder pastes these verbatim.
_BASH_BLOCK_RE = re.compile(r"```bash\n(.*?)```", re.DOTALL)


def test_skills_directory_is_not_empty() -> None:
    assert _SKILL_FILES, "expected at least one skills/*/SKILL.md"


@pytest.mark.parametrize("skill", _SKILL_FILES, ids=lambda p: p.parent.name)
class TestSkillFile:
    def test_frontmatter_declares_name_and_description(self, skill: Path) -> None:
        """The name/description frontmatter is what an agent runtime
        matches on to decide whether to load the skill at all."""
        text = skill.read_text()
        assert text.startswith("---\n"), f"{skill} has no frontmatter"
        _, raw, _ = text.split("---", 2)
        meta = yaml.safe_load(raw)
        assert meta["name"] == skill.parent.name
        assert meta["description"].strip()

    def test_has_no_repo_relative_links(self, skill: Path) -> None:
        """A skill is meant to be *fetched* — installed into
        ``~/.claude/skills``, pasted into another assistant, or read from
        a URL by a founder who installed Crewlet with ``pip`` and has no
        checkout.  A ``../../docs/…`` link resolves in none of those
        cases, so every reference must be an absolute URL.
        """
        relative = _REL_LINK_RE.findall(skill.read_text())
        assert relative == [], (
            f"{skill} uses repo-relative links that break when the file "
            f"is fetched standalone: {relative}"
        )

    def test_documentation_links_carry_the_trailing_slash(self, skill: Path) -> None:
        """docs.crewlet.ai is built with Astro's ``trailingSlash:
        'always'``, so every page there is a directory and
        ``/concepts/tool-skills`` is a 308 to ``/concepts/tool-skills/``.

        The slashless spelling resolves, which is why seven of them sat
        here unnoticed.  It is still the wrong URL to hand out.  This
        file is read by an assistant that fetches exactly what it is
        given, so each one costs a redirect per fetch, and the file is
        published — the docs site serves ``skills/`` alongside the pages
        themselves — so a crawler following these links files each URL
        under "page with redirect" rather than as the page it reaches.

        Schema URLs are exempt: ``/schema/company.schema.json`` names a
        file, not a page, and a trailing slash there would be wrong.
        """
        unslashed = [
            path
            for path in _DOCS_LINK_RE.findall(skill.read_text())
            if not path.endswith("/") and not Path(path).suffix
        ]
        assert unslashed == [], (
            f"{skill} links documentation pages without the trailing "
            f"slash, so each one redirects: {unslashed}"
        )

    def test_referenced_schema_urls_match_the_generated_ids(self, skill: Path) -> None:
        """The schema URLs a skill tells an assistant to fetch must be
        the ones ``crewlet schema`` stamps as ``$id`` — otherwise a
        binary-free assistant is pointed at a document that may not be
        the one this version of the code accepts.
        """
        text = skill.read_text()
        for tier in ("company", "bootstrap"):
            if f"{tier}.schema.json" not in text:
                continue
            assert build_config_schema(tier)["$id"] in text, (
                f"{skill} references the {tier} schema at a URL other "
                f"than its canonical $id {build_config_schema(tier)['$id']}"
            )

    def test_referenced_cli_commands_exist(self, skill: Path) -> None:
        """Every ``crewlet <subcommand>`` shown in a bash block must be a
        real subcommand, so a founder pasting it does not hit
        'invalid choice'."""
        parser = _build_parser()
        known = {
            name
            for action in parser._subparsers._group_actions  # type: ignore[union-attr]
            for name in action.choices
        }
        referenced = {
            line.split()[1]
            for block in _BASH_BLOCK_RE.findall(skill.read_text())
            for line in block.splitlines()
            if line.strip().startswith("crewlet ") and len(line.split()) > 1
        }
        unknown = {cmd for cmd in referenced if cmd not in known}
        assert unknown == set(), f"{skill} references unknown commands: {unknown}"
