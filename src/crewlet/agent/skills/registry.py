"""In-memory registry of tool skills, sourced from the knowledge backend.

Engine-wide singleton, populated at boot by the active backend's sync
worker (:class:`ToolSkillSyncWorker` for Confluence,
:class:`~crewlet.agent.skills.plane_sync.PlaneSkillSyncWorker` for
Plane) and refreshed by webhook events. Prompt builders consult it once
per phase to fetch the skills matching the active tool surface.
"""

from __future__ import annotations

import asyncio
from collections.abc import Mapping

from crewlet._logging import get_logger
from crewlet.agent.skills.models import (
    Phase,
    PromptSkill,
    TriggerContext,
    evaluate_trigger,
    find_variable_references,
    substitute_variables,
    trigger_covers_tool,
)

logger = get_logger("agent.skills.registry")


class PromptSkillRegistry:
    """Async-safe in-memory store of :class:`PromptSkill` instances.

    Mutations (``upsert`` / ``evict``) are serialized behind an
    ``asyncio.Lock`` because the webhook handler can race with the
    boot-time populate during the brief overlap. Reads (``skills_for``)
    don't take the lock — dict reads are atomic in CPython and the
    prompt-build path accepts eventually-consistent reads (a skill
    upserted mid-build either lands in this prompt or the next; both
    are correct).
    """

    def __init__(self) -> None:
        self._skills: dict[str, PromptSkill] = {}
        self._variables: dict[str, str] = {}
        self._lock = asyncio.Lock()

    def set_variables(self, variables: Mapping[str, str]) -> None:
        """Install the operator-defined ``${var}`` substitution map.

        Called by the engine on every ``apply_config`` (boot + live
        reload) with the resolved ``skill_variables``.  Stored as a fresh
        dict so the assignment is a single atomic reference swap — a
        lock-free :meth:`render` running concurrently sees either the old
        or the new map whole, never a half-updated one (mirrors the
        lock-free read policy on :meth:`skills_for`).

        Re-checks every already-registered skill against the new map so
        a variable dropped by a config push surfaces immediately, not on
        the skill's next edit.
        """
        self._variables = dict(variables)
        for skill in self._skills.values():
            self._warn_unresolved(skill)

    def _warn_unresolved(self, skill: PromptSkill) -> None:
        """Warn when ``skill`` references a ``${var}`` not in the map.

        Substitution deliberately leaves unknown references as literal
        ``${name}`` text (visible, debuggable) — but the only place that
        literal shows up is inside an LLM prompt, which no operator
        reads.  This turns the silent prompt defect into a structured
        log line at skill-registration / config-apply time.
        """
        for field_name, text in (
            ("title", skill.title),
            ("summary", skill.summary),
            ("body", skill.body),
        ):
            for name in sorted(find_variable_references(text) - set(self._variables)):
                logger.warning(
                    "skill_variable_unresolved",
                    skill_key=skill.key,
                    variable=name,
                    field=field_name,
                )

    def render(self, text: str) -> str:
        """Substitute ``${var}`` references in skill text from the
        installed variable map.

        Every site that renders a skill's ``summary`` / ``body`` /
        ``title`` to the LLM routes through here so substitution is
        applied uniformly.  See
        :func:`~crewlet.agent.skills.models.substitute_variables` for the
        (braced-identifier-only) substitution rules.
        """
        return substitute_variables(text, self._variables)

    async def upsert(self, skill: PromptSkill) -> None:
        """Insert or replace ``skill`` by ``key``.

        A backend page is one skill: when the same ``source_page_id``
        already backs an entry under a DIFFERENT key (the operator
        edited the page's ``key`` in the UI), that stale entry is
        evicted under the same lock before the insert — otherwise the
        rename leaves a ghost skill serving the old trigger and body
        until the next full walk.  Enforced here, once, so both sync
        workers' webhook paths get it for free.
        """
        stale_key: str | None = None
        async with self._lock:
            if skill.source_page_id:
                stale_key = next(
                    (
                        key
                        for key, existing in self._skills.items()
                        if existing.source_page_id == skill.source_page_id
                        and key != skill.key
                    ),
                    None,
                )
                if stale_key is not None:
                    del self._skills[stale_key]
            self._skills[skill.key] = skill
        if stale_key is not None:
            logger.info(
                "skill_evicted",
                key=stale_key,
                source_page_id=skill.source_page_id,
                reason="key_renamed",
                new_key=skill.key,
            )
        logger.info(
            "skill_upserted",
            key=skill.key,
            source_page_id=skill.source_page_id,
            source_page_version=skill.source_page_version,
        )
        self._warn_unresolved(skill)

    def seed(self, skills: list[PromptSkill] | tuple[PromptSkill, ...]) -> None:
        """Atomically REPLACE the registry contents with ``skills``.

        Used by the full populate (boot walk and live-refresh re-walk)
        and by tests that need a registry pre-loaded without an event
        loop.  Replace — not upsert — semantics are load-bearing: a
        re-walk must drop pages deleted while the engine was down, and
        a live backend switch must drop the old backend's skills.  The
        new state is built off to the side and installed with a single
        reference assignment, so lock-free readers (``skills_for``) and
        seed itself never observe a half-updated map; a webhook upsert
        racing the walk lands on whichever dict its lock acquisition
        saw, and the next event or walk reconciles — the same
        eventual-consistency contract as every other read here.
        """
        fresh = {skill.key: skill for skill in skills}
        self._skills = fresh
        for skill in fresh.values():
            self._warn_unresolved(skill)

    async def evict(self, key: str) -> bool:
        """Drop the skill at ``key``. Returns True if something was removed."""
        async with self._lock:
            removed = self._skills.pop(key, None) is not None
        if removed:
            logger.info("skill_evicted", key=key)
        return removed

    async def evict_by_page_id(self, page_id: str) -> bool:
        """Drop the skill sourced from backend page ``page_id``.

        The webhook eviction path for both sync workers: a page-delete
        (or a page edited into a non-skill) names only the page id, and
        skills are keyed by their declared ``key`` — so this is a small
        linear scan over ``source_page_id`` (the registry is bounded to
        a few dozen entries).  Evicts at most one skill (page ids are
        unique) and returns whether it did; a page id the registry
        never held is a harmless no-op.
        """
        if not page_id:
            return False
        async with self._lock:
            removed_key = next(
                (
                    key
                    for key, skill in self._skills.items()
                    if skill.source_page_id == page_id
                ),
                None,
            )
            if removed_key is None:
                return False
            del self._skills[removed_key]
        logger.info("skill_evicted", key=removed_key, source_page_id=page_id)
        return True

    def skills_for(
        self,
        *,
        phase: Phase,
        available_tools: set[str] | frozenset[str],
        mcp_servers: set[str] | frozenset[str],
    ) -> list[PromptSkill]:
        """Return skills triggered by ``available_tools`` / ``mcp_servers``
        and scoped to ``phase``, sorted by ``key`` for prefix-cache stability.
        """
        ctx = TriggerContext(
            tools=frozenset(available_tools),
            mcp_servers=frozenset(mcp_servers),
        )
        matched = [
            skill
            for skill in self._skills.values()
            if phase in skill.phases and evaluate_trigger(skill.trigger, ctx)
        ]
        matched.sort(key=lambda s: s.key)
        return matched

    def required_skills_for_tool(
        self,
        *,
        phase: Phase,
        tool_name: str,
        server_name: str,
        ctx: TriggerContext,
    ) -> list[PromptSkill]:
        """Required skills that gate a specific tool call.

        A skill gates the call when it is :attr:`PromptSkill.required`,
        declares ``phase`` in its ``phases``, its full trigger matches
        the session's surface (``ctx`` — the same TriggerContext the
        phase's prompt catalogue was built from), AND the trigger
        actually covers ``tool_name`` / ``server_name`` (see
        :func:`~crewlet.agent.skills.models.trigger_covers_tool`).

        Sorted by ``key`` so the guard's block message lists skills
        deterministically. Reads are lock-free like :meth:`skills_for`
        — a skill upserted mid-turn starts gating on the next call,
        which is the behaviour operators expect from a webhook-driven
        registry.
        """
        matched = [
            skill
            for skill in self._skills.values()
            if skill.required
            and phase in skill.phases
            and evaluate_trigger(skill.trigger, ctx)
            and trigger_covers_tool(
                skill.trigger, tool_name=tool_name, server_name=server_name
            )
        ]
        matched.sort(key=lambda s: s.key)
        return matched

    def get(self, key: str) -> PromptSkill | None:
        """Look up a skill by exact key. Used by the load_tool_skill builtin."""
        return self._skills.get(key)

    def keys(self) -> list[str]:
        """Snapshot of registered skill keys, sorted."""
        return sorted(self._skills.keys())

    def __len__(self) -> int:
        return len(self._skills)

    def __contains__(self, key: str) -> bool:
        return key in self._skills


__all__ = ["PromptSkillRegistry"]
