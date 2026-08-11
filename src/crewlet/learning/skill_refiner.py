"""SkillRefiner — post-turn auto-refinement of synthesized skills.

Runs as a fourth worker inside the
:class:`~crewlet.learning.reflect_engine.ReflectEngine`, dispatched
after SkillSynthesizer.  For every turn whose ``skills_used``
contains at least one synthesized skill, the refiner picks the most-
recently-used match and asks the auxiliary model whether to append a
short observation to it:

* ``review_outcome="done"``  → *"Observed in practice: …"* note
* anything else              → *"Counter-example: …"* note

The LLM may emit ``NOOP`` for turns that are too ordinary to warrant
an annotation.  Every refinement bumps the skill's version; the prior
state is archived into ``synthesized_skill_versions`` by the store.

Scope:
* Agent-scope skills only.  Shared procedural knowledge lives as
  knowledge-base pages (Confluence or Plane) that a unit lead
  reviews and edits there, drafted by the cross-agent promotion
  pass.
* At most one refinement per turn.
* The refiner never deletes content; it appends bullets (bounded by
  ``max_body_chars``) and rejects the patch when the skill is already
  at the cap.
"""

from __future__ import annotations

from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.phase_model import resolve_phase_provider
from crewlet.learning._aux_telemetry import complete_with_telemetry
from crewlet.learning.models import SynthesizedSkill
from crewlet.learning.persist_decider import _extract_json_object
from crewlet.learning.synthesized_skill_store import SynthesizedSkillStoreProtocol
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import LLMProvider, Message

logger = get_logger("learning.skill_refiner")

NOOP_SENTINEL = "NOOP"

_REFINER_SYSTEM_PROMPT = """You are a skill-refinement assistant.

You will be given one synthesized skill the agent just used in a
turn, plus the turn outcome.  Your job is to decide whether ONE
short observation should be appended to the skill's body.

Rules:
1. Most turns don't warrant a refinement.  If the turn didn't teach
   anything the existing body doesn't already capture, respond
   exactly with NOOP.
2. When you do refine, emit a single bullet.  Mark success
   observations as ``Observed in practice:`` and failures as
   ``Counter-example:``.
3. Never edit the existing body; you can only append.  Never put
   task-specific identifiers (ticket IDs, URLs, one-off data) in the
   bullet.
4. Keep the bullet under ~300 characters, one sentence or two at
   most.

Output format (strict):
- Either the literal string: NOOP
- Or a single JSON object: {"note": "short observation bullet"}
- Never include prose before or after the JSON."""


class SkillRefiner:
    """LLM-backed auto-refiner that bumps version + archives history."""

    def __init__(
        self,
        *,
        llm_providers: dict[str, LLMProvider],
        store: SynthesizedSkillStoreProtocol,
        budget_tokens: int = 3000,
        max_body_chars: int = 20000,
        max_versions_kept: int = 10,
        auto_refine_on_success: bool = True,
        auto_refine_on_failure: bool = True,
        event_queue: Any = None,
    ) -> None:
        self._llm_providers = llm_providers
        self._store = store
        self._budget_tokens = budget_tokens
        self._max_body_chars = max_body_chars
        self._max_versions_kept = max_versions_kept
        self._auto_refine_on_success = auto_refine_on_success
        self._auto_refine_on_failure = auto_refine_on_failure
        self._event_queue = event_queue

    async def refine_from_turn(
        self,
        *,
        role: Role,
        agent_handle: str,
        skills_used: list[str],
        review_outcome: str,
        task_summary: str,
        plan_summary: str,
        turn_id: str,
        agent_id: str = "",
    ) -> SynthesizedSkill | None:
        """Refine one synthesized skill that was used this turn.

        Returns the updated :class:`SynthesizedSkill` or ``None`` when
        no refinement happened (no matching skill, disabled by config,
        NOOP, LLM failure, body cap reached, etc.).
        """
        if not agent_handle or not skills_used:
            return None
        is_success = review_outcome == "done"
        if is_success and not self._auto_refine_on_success:
            return None
        if not is_success and not self._auto_refine_on_failure:
            return None

        target = await self._pick_target(
            agent_handle=agent_handle,
            skills_used=skills_used,
        )
        if target is None:
            return None

        if len(target.content) >= self._max_body_chars:
            logger.info(
                "skill_refinement_body_cap_reached",
                turn_id=turn_id,
                name=target.name,
                size=len(target.content),
            )
            return None

        note = await self._derive_note(
            role=role,
            skill=target,
            review_outcome=review_outcome,
            task_summary=task_summary,
            plan_summary=plan_summary,
            turn_id=turn_id,
            agent_id=agent_id,
        )
        if note is None:
            return None

        kind = "observed_in_practice" if is_success else "counter_example"
        prefix = "- Observed in practice: " if is_success else "- Counter-example: "
        appended = (target.content.rstrip() + "\n\n" + prefix + note).strip()
        appended = appended[: self._max_body_chars]
        updated = target.model_copy(update={"content": appended})
        try:
            persisted = await self._store.update(
                updated,
                refinement_kind=kind,  # type: ignore[arg-type]
                refinement_note=note[:400],
                max_versions_kept=self._max_versions_kept,
            )
        except Exception:
            logger.exception(
                "skill_refinement_update_failed",
                turn_id=turn_id,
                name=target.name,
            )
            return None
        logger.info(
            "skill_refined",
            turn_id=turn_id,
            name=target.name,
            kind=kind,
            version=persisted.version,
        )
        return persisted

    async def _pick_target(
        self,
        *,
        agent_handle: str,
        skills_used: list[str],
    ) -> SynthesizedSkill | None:
        """Pick the most-recently-used synthesized skill owned by ``agent_handle``.

        Iterates ``skills_used`` in *reverse* order (LLMs tend to call
        skills near the end of a plan); returns the first name that
        resolves to one of this agent's rows.  The table holds
        agent-scope rows only, so there is nothing else to skip.
        """
        # Walk in reverse so the last-called skill wins on ties.
        for name in reversed(skills_used):
            if not name:
                continue
            try:
                candidate = await self._store.fetch(
                    agent_handle=agent_handle, name=name
                )
            except Exception:
                logger.exception("skill_refiner_fetch_failed", name=name)
                continue
            if candidate is None:
                continue
            return candidate
        return None

    async def _derive_note(
        self,
        *,
        role: Role,
        skill: SynthesizedSkill,
        review_outcome: str,
        task_summary: str,
        plan_summary: str,
        turn_id: str,
        agent_id: str = "",
    ) -> str | None:
        try:
            provider_key, provider = resolve_phase_provider(
                role, "auxiliary", self._llm_providers
            )
        except Exception:
            logger.debug("skill_refiner_no_provider", turn_id=turn_id)
            return None

        user_prompt = _build_user_prompt(
            skill=skill,
            review_outcome=review_outcome,
            task_summary=task_summary,
            plan_summary=plan_summary,
        )
        try:
            completion = await complete_with_telemetry(
                provider=provider,
                messages=[
                    Message(role="system", content=_REFINER_SYSTEM_PROMPT),
                    Message(role="user", content=user_prompt),
                ],
                worker="skill_refiner",
                role_name=role.name,
                provider_key=provider_key,
                event_queue=self._event_queue,
                agent_id=agent_id,
                turn_id=turn_id,
                temperature=0.2,
                max_tokens=(self._budget_tokens or None),
            )
        except Exception:
            logger.exception("skill_refiner_llm_failed", turn_id=turn_id)
            return None

        text = (completion.content or "").strip()
        if not text:
            return None
        if text.upper().startswith(NOOP_SENTINEL):
            logger.debug("skill_refinement_noop", turn_id=turn_id, name=skill.name)
            return None

        parsed = _extract_json_object(text)
        if not isinstance(parsed, dict):
            logger.warning(
                "skill_refiner_unparseable",
                turn_id=turn_id,
                response=text[:200],
            )
            return None
        note = str(parsed.get("note", "")).strip()
        if not note:
            return None
        if len(note) > 400:
            note = note[:400].rstrip() + "…"
        return note


def _build_user_prompt(
    *,
    skill: SynthesizedSkill,
    review_outcome: str,
    task_summary: str,
    plan_summary: str,
) -> str:
    return (
        f"Skill: {skill.name}\n"
        f"Description: {skill.description}\n"
        f"Current body (verbatim, do not repeat in your note):\n"
        f"{skill.content}\n\n"
        f"Turn summary:\n"
        f"- Task: {task_summary or '(no description)'}\n"
        f"- Plan: {plan_summary or '(no plan)'}\n"
        f"- Outcome: {review_outcome}\n\n"
        "Decide: NOOP, or emit the JSON object."
    )


__all__ = [
    "NOOP_SENTINEL",
    "SkillRefiner",
]
