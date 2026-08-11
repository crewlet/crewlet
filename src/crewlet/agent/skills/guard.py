"""Required-skill guard — load-before-use enforcement for tool skills.

A tool skill's body carries practices an agent should read *before*
touching the tools the skill's trigger covers (workflow constraints,
mention markup, delegation conventions) — and LLMs routinely skip the
``load_tool_skill`` call and go straight for the tool.  This module
closes that gap in code, not in prompts.  Skills are enforced by
default (``required: true``); operators mark orientation / hint-grade
content ``required: false`` to keep it advisory.

One :class:`SkillGuard` is attached to the phase's ``ToolSurface``
(``surface.skill_guard``) by each phase runner that carries domain
tools — Plan, Execute, and Sub-agent.  The shared dispatch gate
(:func:`crewlet.agent.llm_loop.execute_tool`) consults it on every
tool call:

- a call to a tool covered by a required-but-unloaded skill is
  rejected with an instructive error naming the exact
  ``load_tool_skill(key=...)`` call to make, and a
  ``phase.tool_skill_blocked`` event is published for operators;
- a successful ``load_tool_skill`` call records the key, unlocking
  the covered tools for the rest of the session.

**Session scope.**  "Loaded" is tracked per LLM session — one phase's
message history — not per turn.  Plan, Execute, and each sub-agent run
on separate message histories, so a body loaded during Plan is not in
the Execute LLM's context; each session must load the skill itself.
Round-cap extension loops continue the same message history *and* the
same surface object, so the loaded set carries across extensions.
``self_iterate`` starts fresh phase sessions and therefore fresh
guards.

The guard never fires for sessions that cannot recover: it refuses to
arm when ``load_tool_skill`` is absent from the surface
(:func:`build_skill_guard` returns ``None``), and engine plumbing
tools (the unlock itself, discovery meta-tools, phase-contract
submitters) are exempt so a misauthored trigger can't brick a phase.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.skills.models import Phase, PromptSkill, TriggerContext
from crewlet.agent.skills.registry import PromptSkillRegistry
from crewlet.events.types import ToolSkillGuardBlocked
from crewlet.tools.protocol import ToolResult

logger = get_logger("agent.skills.guard")

GUARD_EXEMPT_TOOLS: frozenset[str] = frozenset(
    {
        # The unlock itself — gating it would deadlock the session.
        "load_tool_skill",
        # Discovery meta-tools: the LLM may need them to even reach the
        # guarded tool (list server tools, activate). Blocking them
        # adds rounds without protecting anything.
        "activate_tool",
        "list_mcp_server_tools",
        # Phase-contract submitters: blocking these bricks the phase
        # (the rescue paths force them with tool_choice="required").
        "submit_plan",
        "submit_review",
    }
)
"""Tools the guard never blocks, regardless of skill triggers."""


@dataclass
class SkillGuard:
    """Per-session load-before-use enforcement state.

    Built once per phase session by :func:`build_skill_guard` and
    attached to the phase's ``ToolSurface``.  ``trigger_ctx`` is the
    same :class:`TriggerContext` the phase's prompt catalogue was
    built from, so the guard gates exactly the skills the LLM was
    shown (catalogue and enforcement can't disagree).

    Event metadata (``event_queue`` / ``agent_id`` / ... ) is optional
    — guards built in tests without a queue simply skip publishing.
    """

    registry: PromptSkillRegistry
    phase: Phase
    trigger_ctx: TriggerContext
    event_queue: Any = None
    agent_id: str = ""
    agent_role: str = ""
    turn_id: str = ""
    iteration: int = 0
    loaded_keys: set[str] = field(default_factory=set)

    def missing_for(self, tool: Any) -> list[PromptSkill]:
        """Required skills covering ``tool`` not yet loaded this session."""
        name = getattr(tool, "name", "")
        if not name or name in GUARD_EXEMPT_TOOLS:
            return []
        skills = self.registry.required_skills_for_tool(
            phase=self.phase,
            tool_name=name,
            server_name=getattr(tool, "server_name", "") or "",
            ctx=self.trigger_ctx,
        )
        return [s for s in skills if s.key not in self.loaded_keys]

    async def check_tool(self, tool: Any) -> ToolResult | None:
        """Gate one tool call.

        Returns ``None`` when the call may proceed, or a failed
        :class:`ToolResult` telling the LLM which skill(s) to load
        first.  Publishes a ``phase.tool_skill_blocked`` event on every
        block so operators can see agents skipping required practices.
        """
        missing = self.missing_for(tool)
        if not missing:
            return None
        name = getattr(tool, "name", "")
        keys = [s.key for s in missing]
        logger.info(
            "tool_skill_guard_blocked",
            tool_name=name,
            skill_keys=keys,
            phase=str(self.phase),
            agent_id=self.agent_id,
            turn_id=self.turn_id,
        )
        await self._publish_blocked(tool_name=name, skill_keys=keys)
        return ToolResult(
            success=False, error=_block_message(name, missing, self.registry)
        )

    def observe(self, name: str, arguments: dict[str, Any], success: bool) -> None:
        """Record a completed tool call.

        Only successful ``load_tool_skill`` calls matter: the loaded
        key unlocks every tool that skill covers for the rest of this
        session.
        """
        if name != "load_tool_skill" or not success:
            return
        key = str(arguments.get("key", "")).strip()
        if not key:
            return
        self.loaded_keys.add(key)
        logger.debug(
            "tool_skill_guard_recorded_load",
            key=key,
            phase=str(self.phase),
            agent_id=self.agent_id,
        )

    async def _publish_blocked(self, *, tool_name: str, skill_keys: list[str]) -> None:
        if self.event_queue is None:
            return
        event = ToolSkillGuardBlocked(
            source=self.agent_role,
            agent_id=self.agent_id,
            role=self.agent_role,
            phase=str(self.phase),
            tool_name=tool_name,
            skill_keys=list(skill_keys),
            turn_id=self.turn_id,
            iteration=self.iteration,
        )
        try:
            await self.event_queue.publish(f"crewlet.events.{event.type}", event)
        except Exception:
            logger.exception(
                "tool_skill_blocked_publish_failed",
                tool_name=tool_name,
                skill_keys=skill_keys,
            )


def _block_message(
    tool_name: str,
    missing: list[PromptSkill],
    registry: PromptSkillRegistry,
) -> str:
    """Instructive rejection the LLM can act on in the next round.

    Summaries are rendered through the registry so any ``${var}``
    references resolve here too — this message goes straight to the LLM.
    """
    loads = "; then ".join(f"`load_tool_skill(key='{s.key}')`" for s in missing)
    listing = "\n".join(
        f"- '{s.key}': {' '.join(registry.render(s.summary).split())}" for s in missing
    )
    plural = "skills" if len(missing) > 1 else "skill"
    return (
        f"Tool '{tool_name}' is gated behind required tool {plural} you "
        f"have not loaded in this session:\n{listing}\n\n"
        f"Call {loads} to read the required practice(s), then retry "
        f"'{tool_name}'. Loading is needed once per session; after that "
        "the tool works normally."
    )


def build_skill_guard(
    *,
    registry: Any,
    phase: Phase,
    surface: Any,
    mcp_servers: set[str],
    event_queue: Any = None,
    agent_id: str = "",
    agent_role: str = "",
    turn_id: str = "",
    iteration: int = 0,
) -> SkillGuard | None:
    """Build the guard for one phase session, or ``None`` when it
    cannot or should not arm.

    - ``registry is None`` — tool skills not configured for this
      engine; nothing to enforce.
    - ``load_tool_skill`` not in ``surface`` — the session has no way
      to satisfy the guard; arming it would soft-lock the LLM in a
      block-retry loop. (Plan carries it as a meta-tool, Execute as an
      always-on, Sub-agent surfaces always include it — so this is a
      defensive rail for non-standard surfaces, not a normal path.)

    The trigger context mirrors what the phase's prompt catalogue was
    built from: the surface's full catalogue (activation universe) for
    Plan / Execute, falling back to the active tool list for Sub-agent
    surfaces (which carry no catalogue), plus the role's MCP servers.
    """
    if registry is None:
        return None
    if not surface.has("load_tool_skill"):
        logger.warning(
            "skill_guard_disabled_no_loader",
            phase=str(phase),
            agent_id=agent_id,
            turn_id=turn_id,
        )
        return None
    tools = set(surface.catalogue_names()) or set(surface.names)
    return SkillGuard(
        registry=registry,
        phase=phase,
        trigger_ctx=TriggerContext(
            tools=frozenset(tools),
            mcp_servers=frozenset(mcp_servers),
        ),
        event_queue=event_queue,
        agent_id=agent_id,
        agent_role=agent_role,
        turn_id=turn_id,
        iteration=iteration,
    )


def skill_guard_for_turn(
    *,
    registry: Any,
    phase: Phase,
    surface: Any,
    turn: Any,
    event_queue: Any = None,
) -> SkillGuard | None:
    """:func:`build_skill_guard` with the per-turn fields derived from a
    :class:`~crewlet.agent.turn_context.TurnContext`.

    The three phase runners (Plan / Execute / Sub-agent) all derive the
    same five arguments from their turn context — the role's MCP-server
    set and the agent / turn identity for the blocked event.  Deriving
    them here keeps the three call sites to one expression and removes
    the risk of the derivations drifting apart.  ``turn`` is typed
    ``Any`` to avoid coupling this module to the turn-context import
    graph; it duck-types ``TurnContext``.
    """
    return build_skill_guard(
        registry=registry,
        phase=phase,
        surface=surface,
        mcp_servers=set(getattr(turn.agent.definition.role, "mcp_env", {}) or {}),
        event_queue=event_queue,
        agent_id=turn.agent.id_str,
        agent_role=turn.agent.role_name,
        turn_id=turn.turn_id,
        iteration=turn.iteration,
    )


__all__ = [
    "GUARD_EXEMPT_TOOLS",
    "SkillGuard",
    "build_skill_guard",
    "skill_guard_for_turn",
]
