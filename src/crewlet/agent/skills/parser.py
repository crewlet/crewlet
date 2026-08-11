"""Markdown + YAML frontmatter parser for tool-skill files.

Reads files like those under ``examples/tool-skills/``::

    ---
    key: mcp:github
    trigger:
      mcp_server: github
    phases: [plan, execute]
    title: GitHub tools
    ---

    <markdown body...>

and produces a validated :class:`PromptSkill`. Used by the import CLIs
(creating knowledge-backend pages from local seeds) and by the Tool
Skills sync workers (parsing the YAML frontmatter each backend's codec
extracts from a page).
"""

from __future__ import annotations

import re
from typing import Any

import yaml

from crewlet.agent.skills.models import (
    MAX_SKILL_BODY_BYTES,
    PromptSkill,
    TriggerExpr,
)

_FRONTMATTER_RE = re.compile(
    r"\A---\s*\n(.*?\n)---\s*\n?(.*)\Z",
    re.DOTALL,
)

_REQUIRED_KEYS: frozenset[str] = frozenset(
    {"key", "trigger", "phases", "title", "summary"}
)


class SkillParseError(ValueError):
    """Raised when a skill file cannot be parsed or fails validation."""


def parse_skill_file(text: str) -> tuple[dict[str, Any], str]:
    """Split a markdown-with-frontmatter file into ``(frontmatter, body)``.

    Raises :class:`SkillParseError` on malformed YAML or missing frontmatter.
    """
    match = _FRONTMATTER_RE.match(text)
    if not match:
        raise SkillParseError("missing or malformed YAML frontmatter")
    raw_yaml = match.group(1)
    body = match.group(2).strip()
    try:
        frontmatter = yaml.safe_load(raw_yaml)
    except yaml.YAMLError as exc:
        raise SkillParseError(f"invalid YAML in frontmatter: {exc}") from exc
    if not isinstance(frontmatter, dict):
        raise SkillParseError("frontmatter must be a YAML mapping")
    return frontmatter, body


def build_skill(
    frontmatter: dict[str, Any],
    body: str,
    *,
    source_page_id: str = "",
    source_page_version: int = 0,
) -> PromptSkill:
    """Validate a parsed frontmatter + body into a :class:`PromptSkill`.

    Raises :class:`SkillParseError` on missing required keys, an
    over-cap body, or invalid trigger / phase values.
    """
    missing = _REQUIRED_KEYS - set(frontmatter)
    if missing:
        raise SkillParseError(f"frontmatter missing required keys: {sorted(missing)}")
    if len(body.encode("utf-8")) > MAX_SKILL_BODY_BYTES:
        raise SkillParseError(f"skill body exceeds {MAX_SKILL_BODY_BYTES} byte cap")
    # ``required`` is forwarded only when the frontmatter carries it —
    # the model's SKILL_REQUIRED_DEFAULT is the single source of the
    # default, so a future default change cannot silently diverge
    # between file-sourced and directly-constructed skills.
    optional: dict[str, Any] = {}
    if "required" in frontmatter:
        optional["required"] = frontmatter["required"]
    try:
        trigger = TriggerExpr.model_validate(frontmatter["trigger"])
        return PromptSkill(
            key=frontmatter["key"],
            trigger=trigger,
            phases=set(frontmatter["phases"]),
            title=frontmatter["title"],
            summary=str(frontmatter["summary"]).strip(),
            body=body,
            source_page_id=source_page_id,
            source_page_version=source_page_version,
            **optional,
        )
    except (ValueError, TypeError) as exc:
        raise SkillParseError(f"invalid skill definition: {exc}") from exc


def parse_skill(
    text: str,
    *,
    source_page_id: str = "",
    source_page_version: int = 0,
) -> PromptSkill:
    """Parse and validate a full skill file in one call."""
    frontmatter, body = parse_skill_file(text)
    return build_skill(
        frontmatter,
        body,
        source_page_id=source_page_id,
        source_page_version=source_page_version,
    )


__all__ = [
    "SkillParseError",
    "build_skill",
    "parse_skill",
    "parse_skill_file",
]
