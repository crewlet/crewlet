"""Tool Skills — modular tool-guidance prompt fragments.

The runtime building blocks:

- :class:`PromptSkill` — one prompt fragment + trigger, sourced from a
  page in the dedicated Tool Skills container of the active knowledge
  backend (a Confluence space or a Plane project).
- :class:`PromptSkillRegistry` — in-memory store the prompt builders
  consult once per phase.
- :func:`evaluate_trigger` — pure boolean check over the active tool
  surface + role MCP servers.
- :class:`SkillGuard` — per-session load-before-use enforcement for
  skills marked ``required: true`` (see ``guard.py``).

The engine ships no skill prose; an empty registry produces zero
injected scaffolding (just the tool catalogue). See
``docs/concepts/tool-skills.md`` for the full design.
"""

from __future__ import annotations

from crewlet.agent.skills.guard import (
    GUARD_EXEMPT_TOOLS,
    SkillGuard,
    build_skill_guard,
    skill_guard_for_turn,
)
from crewlet.agent.skills.models import (
    MAX_SKILL_BODY_BYTES,
    MAX_SKILL_SUMMARY_BYTES,
    SKILL_REQUIRED_DEFAULT,
    Phase,
    PromptSkill,
    TriggerContext,
    TriggerExpr,
    evaluate_trigger,
    trigger_covers_tool,
)
from crewlet.agent.skills.parser import (
    SkillParseError,
    build_skill,
    parse_skill,
    parse_skill_file,
)
from crewlet.agent.skills.plane_sync import PlaneSkillSyncWorker
from crewlet.agent.skills.registry import PromptSkillRegistry
from crewlet.agent.skills.sync import ToolSkillSyncWorker

__all__ = [
    "GUARD_EXEMPT_TOOLS",
    "MAX_SKILL_BODY_BYTES",
    "MAX_SKILL_SUMMARY_BYTES",
    "SKILL_REQUIRED_DEFAULT",
    "Phase",
    "PlaneSkillSyncWorker",
    "PromptSkill",
    "PromptSkillRegistry",
    "SkillGuard",
    "SkillParseError",
    "ToolSkillSyncWorker",
    "TriggerContext",
    "TriggerExpr",
    "build_skill",
    "build_skill_guard",
    "evaluate_trigger",
    "parse_skill",
    "parse_skill_file",
    "skill_guard_for_turn",
    "trigger_covers_tool",
]
