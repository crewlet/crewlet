"""SkillSynthesizer — auto-draft reusable skills from past successful turns.

Two entry points, same underlying LLM + store path:

* **Single-turn** (phase-4 inline trigger): fires from the
  :class:`~crewlet.learning.reflect_engine.ReflectEngine` when a
  completed turn used >= ``min_tool_calls`` tools and finished with
  ``review_outcome=done``.  The turn alone seeds the new skill.
* **Clustered** (phase-4 scheduled trigger): fires from the
  :class:`~crewlet.learning.skill_scheduler.SkillClusteringScheduler`
  when >=``cluster_min_size`` recent successful turns share a similar
  tool sequence (Jaccard >= ``cluster_jaccard_threshold``).  The
  cluster seeds the new skill.

Both paths converge in :meth:`SkillSynthesizer.synthesize`, which:
1. enforces the per-agent skill cap,
2. calls the role's auxiliary model for a structured JSON proposal,
3. rejects name collisions against the agent's own synthesized-skill
   table (the only shared name space to guard against),
4. rejects tool-sequence near-duplicates against existing synthesized
   skills (Jaccard),
5. writes one row to ``synthesized_skills``.
"""

from __future__ import annotations

import re
from collections.abc import Iterable
from datetime import UTC, datetime
from typing import Any, NamedTuple, Protocol
from uuid import uuid4

from crewlet._logging import get_logger
from crewlet.agent.phase_model import resolve_phase_provider
from crewlet.knowledge.protocol import AUTO_DRAFT_TITLE_PREFIX, AUTO_DRAFTED_PARENT
from crewlet.learning._aux_telemetry import complete_with_telemetry
from crewlet.learning.models import Episode, SynthesizedSkill
from crewlet.learning.persist_decider import _extract_json_object
from crewlet.learning.synthesized_skill_store import SynthesizedSkillStoreProtocol
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import LLMProvider, Message

logger = get_logger("learning.skill_synthesizer")

NOOP_SENTINEL = "NOOP"
_NAME_PATTERN = re.compile(r"^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$")

_SYNTHESIZER_SYSTEM_PROMPT = """You are a skill-drafting assistant.

You will be given 1-N recent successful turns the agent completed.
Your job is to decide whether a reusable procedural skill can be
distilled from them, and if so emit one.

Rules:
1. If the turns are too task-specific, too divergent, or too trivial
   to warrant a reusable skill, respond exactly with NOOP.
2. When drafting, write a **procedure the agent can follow next time**,
   not a narrative of what already happened.  No task-specific IDs,
   URLs, ticket numbers, or one-off data.
3. Prefer tight steps with clear decision points.  Name the exact
   tools the agent should use (without arguments).
4. Never name a skill that collides with one already listed.

Output format (strict):
- Either the literal string: NOOP
- Or a single JSON object with exactly these keys:
    {
      "name": "kebab-case-skill-name",
      "description": "One sentence, under 200 chars.",
      "body": "Markdown body: short intro + numbered steps."
    }
- ``name`` must match ^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$ (lowercase
  alphanumeric + hyphens, 2-64 chars).
- Never include prose before or after the JSON."""


class SkillSynthesizer:
    """LLM-backed skill drafter."""

    def __init__(
        self,
        *,
        llm_providers: dict[str, LLMProvider],
        store: SynthesizedSkillStoreProtocol,
        budget_tokens: int = 4000,
        max_skills_per_agent: int = 50,
        duplicate_jaccard_threshold: float = 0.7,
        event_queue: Any = None,
        episode_store: Any = None,
    ) -> None:
        self._llm_providers = llm_providers
        self._store = store
        self._budget_tokens = budget_tokens
        self._max_skills_per_agent = max_skills_per_agent
        self._duplicate_jaccard_threshold = duplicate_jaccard_threshold
        self._event_queue = event_queue
        # Optional reference to the episode store -- used to stamp
        # ``consolidated_into_skill_id`` on source episodes after a
        # successful skill insert.  When absent (in-memory tests, or
        # engines that haven't wired episodic memory), consolidation
        # tagging is silently skipped and the lifecycle worker keeps
        # the source episodes in the regular pruning track.
        self._episode_store = episode_store

    async def synthesize(
        self,
        *,
        role: Role,
        agent_handle: str,
        source_episodes: list[Episode],
        existing_skill_names: Iterable[str] = (),
        trigger: str = "single_turn",
        agent_id: str = "",
    ) -> SynthesizedSkill | None:
        """Draft one skill from ``source_episodes``.

        Returns the persisted :class:`SynthesizedSkill` on success or
        ``None`` when the attempt was skipped (cap reached, LLM NOOP,
        parse failure, collision, near-duplicate, etc.).  Never raises
        on routine failures — the caller (ReflectEngine or scheduler)
        is already best-effort.
        """
        if not agent_handle:
            return None
        if not source_episodes:
            return None
        try:
            count = await self._store.count_for_agent(agent_handle)
        except Exception:
            logger.exception("skill_count_lookup_failed", agent_handle=agent_handle)
            return None
        if count >= self._max_skills_per_agent:
            logger.info(
                "skill_cap_reached",
                agent_handle=agent_handle,
                count=count,
                cap=self._max_skills_per_agent,
                trigger=trigger,
            )
            return None

        # Representative tool sequence: for single-turn, the turn's own;
        # for a cluster, the shortest (tends to be the most reusable core).
        representative = _representative_tool_sequence(source_episodes)
        if not representative:
            return None

        # Reject if a similar skill already exists.
        try:
            existing_sequences = await self._store.existing_tool_sequences(agent_handle)
        except Exception:
            logger.exception("skill_existing_sequences_failed")
            existing_sequences = []
        for seq in existing_sequences:
            if jaccard(representative, seq) >= self._duplicate_jaccard_threshold:
                logger.info(
                    "skill_synthesis_duplicate",
                    agent_handle=agent_handle,
                    trigger=trigger,
                )
                return None

        # The agent's own existing skill names are the real reserved
        # space.  Fetch them here -- the drafting LLM needs to *see*
        # them to pick a distinguishable name, and they back the
        # post-draft collision check.  ``existing_skill_names`` from the
        # caller is merged in as a supplementary seam (today the
        # ReflectEngine / scheduler pass nothing).  ``include_archived``
        # so a name occupied by an archived row still reserves it (the
        # ``(agent_handle, name)`` unique index covers archived rows).
        try:
            own_skills = await self._store.list_for_agent(
                agent_handle, include_archived=True
            )
            own_names = [s.name for s in own_skills]
        except Exception:
            logger.exception("skill_existing_names_failed", agent_handle=agent_handle)
            own_names = []
        all_existing_names = [*own_names, *existing_skill_names]

        proposal = await self._draft_proposal(
            role=role,
            source_episodes=source_episodes,
            existing_skill_names=all_existing_names,
            trigger=trigger,
            agent_id=agent_id,
            # ``Episode.turn_id`` is a UUID; downstream telemetry
            # events declare ``turn_id: str``, so stringify here.
            turn_id=str(source_episodes[0].turn_id) if source_episodes else "",
        )
        if proposal is None:
            return None

        name, description, content = proposal
        # Name-collision check after the draft (existing names are the
        # authoritative "reserved" set); case-insensitive.
        reserved = {n.lower() for n in all_existing_names}
        if name.lower() in reserved:
            logger.info(
                "skill_synthesis_name_collision",
                agent_handle=agent_handle,
                name=name,
                trigger=trigger,
            )
            return None
        try:
            existing_row = await self._store.fetch(agent_handle=agent_handle, name=name)
            if existing_row is not None:
                logger.info(
                    "skill_synthesis_name_exists",
                    agent_handle=agent_handle,
                    name=name,
                    trigger=trigger,
                )
                return None
        except Exception:
            logger.exception("skill_fetch_failed", name=name)
            return None

        source_ids = [ep.id for ep in source_episodes]
        now = datetime.now(UTC)
        skill = SynthesizedSkill(
            id=uuid4(),
            agent_handle=agent_handle,
            name=name,
            description=description,
            content=content,
            frontmatter={},
            tool_sequence=list(representative),
            source_episode_ids=source_ids,
            version=1,
            created_at=now,
            updated_at=now,
        )
        try:
            stored = await self._store.insert(skill)
        except Exception:
            logger.exception(
                "skill_synthesis_insert_failed",
                agent_handle=agent_handle,
                name=name,
            )
            return None
        # Stamp ``consolidated_into_skill_id`` on the source episodes so
        # the EpisodeLifecycleWorker can drop them after grace -- the
        # skill now carries the learning forward.  Best-effort; the
        # worker will skip rows it can't see anyway.
        await self._mark_consolidated(source_ids, stored.id)
        logger.info(
            "skill_synthesised",
            agent_handle=agent_handle,
            name=name,
            trigger=trigger,
            sources=len(source_ids),
            tool_count=len(representative),
        )
        return stored

    async def _mark_consolidated(self, episode_ids: list[Any], skill_id: Any) -> None:
        """Best-effort consolidation tagging.  Episode store may not
        implement ``mark_consolidated`` (in-memory test stubs); swallow
        the AttributeError + log all other failures."""
        if not episode_ids:
            return
        episode_store = getattr(self, "_episode_store", None)
        if episode_store is None:
            return
        method = getattr(episode_store, "mark_consolidated", None)
        if method is None:
            return
        try:
            await method(
                episode_ids=[str(x) for x in episode_ids],
                skill_id=str(skill_id),
            )
        except Exception:
            logger.exception(
                "skill_synthesis_mark_consolidated_failed",
                skill_id=str(skill_id),
            )

    async def _draft_proposal(
        self,
        *,
        role: Role,
        source_episodes: list[Episode],
        existing_skill_names: list[str],
        trigger: str,
        agent_id: str = "",
        turn_id: str = "",
    ) -> tuple[str, str, str] | None:
        try:
            provider_key, provider = resolve_phase_provider(
                role, "auxiliary", self._llm_providers
            )
        except Exception:
            logger.debug("skill_synthesis_no_provider")
            return None

        user_prompt = _build_user_prompt(
            source_episodes=source_episodes,
            existing_skill_names=existing_skill_names,
            trigger=trigger,
        )
        try:
            completion = await complete_with_telemetry(
                provider=provider,
                messages=[
                    Message(role="system", content=_SYNTHESIZER_SYSTEM_PROMPT),
                    Message(role="user", content=user_prompt),
                ],
                worker="skill_synthesizer",
                role_name=role.name,
                provider_key=provider_key,
                event_queue=self._event_queue,
                agent_id=agent_id,
                turn_id=turn_id,
                temperature=0.2,
                max_tokens=(self._budget_tokens or None),
            )
        except Exception:
            logger.exception("skill_synthesis_llm_failed")
            return None

        text = (completion.content or "").strip()
        if not text:
            return None
        if text.upper().startswith(NOOP_SENTINEL):
            return None

        parsed = _extract_json_object(text)
        if parsed is None:
            logger.warning("skill_synthesis_unparseable", response=text[:200])
            return None

        name = str(parsed.get("name", "")).strip().lower()
        description = str(parsed.get("description", "")).strip()
        body = str(parsed.get("body", "")).strip()
        if not _NAME_PATTERN.match(name):
            logger.warning("skill_synthesis_bad_name", name=name)
            return None
        if not description or len(description) > 400:
            logger.warning(
                "skill_synthesis_bad_description",
                length=len(description),
            )
            return None
        if not body or len(body) > 20000:
            logger.warning("skill_synthesis_bad_body", length=len(body))
            return None
        return name, description, body


# --------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------


def jaccard(a: list[str], b: list[str]) -> float:
    """Set-based Jaccard similarity on tool-name multisets.

    Returns 0.0 when either side is empty.  Simple and good enough
    for phase-4 duplicate detection — sequences that share most of
    their tool names are treated as near-duplicates regardless of
    order.
    """
    if not a or not b:
        return 0.0
    set_a = set(a)
    set_b = set(b)
    union = set_a | set_b
    if not union:
        return 0.0
    return len(set_a & set_b) / len(union)


def _representative_tool_sequence(
    episodes: list[Episode],
) -> list[str]:
    """Pick one tool sequence that represents the cluster.

    Heuristic: the shortest sequence (tends to be the "core" that all
    cluster members share) — with ties broken by the most-recent
    ``ended_at``.  Empty sequences are skipped.
    """
    candidates = [ep for ep in episodes if ep.tool_sequence]
    if not candidates:
        return []
    candidates.sort(key=lambda ep: (len(ep.tool_sequence), -ep.ended_at.timestamp()))
    return list(candidates[0].tool_sequence)


def _build_user_prompt(
    *,
    source_episodes: list[Episode],
    existing_skill_names: list[str],
    trigger: str,
) -> str:
    blocks: list[str] = []
    for i, ep in enumerate(source_episodes):
        blocks.append(
            "\n".join(
                [
                    f"Turn {i + 1}:",
                    f"  Task: {ep.task_summary or '(no description)'}",
                    f"  Plan: {ep.plan_summary or '(no plan)'}",
                    f"  Tools: {', '.join(ep.tool_sequence) or '(none)'}",
                    f"  Outcome: {ep.review_outcome}",
                ]
            )
        )
    existing_block = (
        "Existing skills (names must not collide):\n  "
        + "\n  ".join(f"- {n}" for n in existing_skill_names)
        if existing_skill_names
        else "Existing skills: (none)"
    )
    return (
        f"Trigger: {trigger}\n\n"
        f"{existing_block}\n\n"
        f"Source turns:\n\n"
        + "\n\n".join(blocks)
        + "\n\nDecide: NOOP, or emit the JSON object."
    )


_PROMOTION_SYSTEM_PROMPT = """You are a skill-promotion assistant.

You will be given 2+ similar synthesized skills that different
agents in the same unit independently drafted from their own
successful turns.  Your job is to distill ONE unit-wide skill that
captures the shared procedure without the agent-specific noise.

Rules:
1. If the sibling skills diverge too much to merge cleanly, respond
   exactly with NOOP.
2. Write a procedure any agent in the unit could follow, in their
   own role's voice.  Drop agent-specific preferences, internal
   aliases, and one-off examples.  Keep the steps and the tool
   names that appear across multiple siblings.
3. Pick a fresh kebab-case name that does not collide with any
   existing unit-scope skill listed below.
4. Keep the description under 200 chars.

Output format (strict):
- Either the literal string: NOOP
- Or a single JSON object with exactly these keys:
    {
      "name": "kebab-case-skill-name",
      "description": "One sentence, under 200 chars.",
      "body": "Markdown body: short intro + numbered steps."
    }
- ``name`` must match ^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$.
- Never include prose before or after the JSON."""


# --------------------------------------------------------------------
# Promotion (unit-scope) synthesis
# --------------------------------------------------------------------


class PromotionPageWriter(Protocol):
    """Backend seam the promotion pass writes draft pages through.

    Defined here (consumer-owned) so the synthesizer stays
    backend-agnostic; the implementations live with their backends —
    :class:`~crewlet.confluence.promotion.ConfluencePromotionWriter`
    wraps the ``ConfluenceTransport`` and
    :class:`~crewlet.plane.promotion.PlanePromotionWriter` wraps the
    ``PlaneTransport``.  The engine constructs exactly one, selected by
    the active knowledge backend; without one the promotion pass is a
    soft no-op.
    """

    backend: str
    """``"confluence"`` | ``"plane"`` — logging only."""

    def resolve_unit_container(self, org: Any, unit_id: str) -> str:
        """The unit's write / skill-promotion home; ``""`` = soft-skip.

        Confluence reads ``OrgUnit.confluence_space``, Plane reads
        ``OrgUnit.plane_project`` — both through ``resolve_env_scalar``
        (Tier B stores ``${VAR}`` references verbatim).
        """
        ...

    def missing_container_hint(self) -> str:
        """Operator remediation for the ``promotion_no_container`` log."""
        ...

    async def create_draft_page(
        self,
        *,
        container: str,
        parent_title: str,
        title: str,
        name: str,
        body_markdown: str,
    ) -> str | None:
        """Create the draft page under ``parent_title`` in ``container``.

        ``name`` is the LLM-picked kebab-case skill name — the draft's
        STABLE identity, distinct from the display ``title`` (which
        carries the ``[Auto-draft] `` prefix).  Writers use it for
        cross-tick dedup, because the scheduler re-clusters the same
        persisted rows every tick and the writer is the only dedup
        layer: Confluence relies on its 4xx on a duplicate title;
        Plane stamps ``external_id="draft:<name>"`` (the fork 409s on
        the duplicate pair) and returns the existing page instead of
        creating another.

        Best-effort: returns the page id (new, or existing on a dedup
        hit), or ``None`` on any failure (all paths logged — the
        scheduler retries on the next tick if the cluster still meets
        the threshold).  A missing parent degrades per backend rather
        than failing the draft.
        """
        ...


class PromotionResult(NamedTuple):
    """Outcome of one successful promotion draft.

    Returned by :meth:`PromotionSynthesizer.synthesize_promotion` so
    the scheduler can build a complete ``SkillPromoted`` event without
    re-deriving the LLM-picked name or target container.
    """

    skill_name: str
    """Kebab-case name the LLM picked for the auto-draft.  Embedded in
    the page title and used for dedup against ``existing_skill_names``.
    """
    page_id: str
    """Backend page id of the newly created draft."""
    page_title: str
    """Full page title under the ``Auto-Drafted Skills`` parent."""
    container_key: str
    """Knowledge-base container the draft landed in -- the unit's
    configured ``integrations.confluence.space`` /
    ``integrations.plane.project``."""


class PromotionSynthesizer:
    """Cross-agent skill promotion — drafts knowledge-base pages.

    When the scheduler observes >=N siblings in the same unit
    converging on a similar pattern, the cluster gets distilled into
    a single skill proposal and posted to the team knowledge base as
    a draft under the unit's ``Auto-Drafted Skills`` parent, through
    the active backend's :class:`PromotionPageWriter`.  A unit lead
    reviews the draft with normal knowledge-base tools (move it out
    of ``Auto-Drafted Skills`` to publish, edit, or delete/archive).

    Once published, the page is searchable like any other knowledge
    doc -- agents reach it through the ``## Relevant knowledge``
    prefetch and the backend's search tools.  This keeps shared
    procedural artifacts in one place (the knowledge base) instead of
    duplicating them in the engine database.

    The auxiliary-LLM drafting machinery is shared with
    :class:`SkillSynthesizer` -- only the write target sits behind
    the page-writer seam.
    """

    AUTO_DRAFTED_PARENT = AUTO_DRAFTED_PARENT
    """Parent page under which the writer posts every draft.
    Operators can move pages out of this parent (the publish action)
    to surface drafts in the Plan-phase ``## Relevant knowledge``
    prefetch; the promotion pass only writes here.  Single-sourced
    from :mod:`crewlet.knowledge.protocol` (the search exclusion reads
    the same constant).  Configurable per-org via the constructor
    argument."""

    def __init__(
        self,
        *,
        llm_providers: dict[str, LLMProvider],
        page_writer: PromotionPageWriter,
        org: Any,
        budget_tokens: int = 4000,
        event_queue: Any = None,
        auto_drafted_parent: str = "",
    ) -> None:
        self._llm_providers = llm_providers
        self._page_writer = page_writer
        self._org = org
        self._budget_tokens = budget_tokens
        self._event_queue = event_queue
        self._parent_title = auto_drafted_parent or self.AUTO_DRAFTED_PARENT

    async def synthesize_promotion(
        self,
        *,
        role: Role,
        unit_id: str,
        source_skills: list[SynthesizedSkill],
        existing_skill_names: Iterable[str] = (),
    ) -> PromotionResult | None:
        """Draft one knowledge-base page from ``source_skills``.

        Returns a :class:`PromotionResult` carrying the LLM-picked
        name, new page id, page title, and target container on
        success.  Returns ``None`` when:

        * ``unit_id`` is empty or there are no source skills,
        * the auxiliary LLM emits NOOP,
        * the proposed name collides with a reserved name,
        * the unit's knowledge container can't be resolved,
        * the page write fails (logged, swallowed -- the next
          scheduler tick will try again with a possibly-fresh
          cluster).
        """
        if not unit_id or not source_skills:
            return None

        representative = _shortest_tool_sequence(
            [s.tool_sequence for s in source_skills]
        )
        if not representative:
            return None

        container = self._page_writer.resolve_unit_container(self._org, unit_id)
        if not container:
            logger.info(
                "promotion_no_container",
                unit_id=unit_id,
                backend=self._page_writer.backend,
                hint=self._page_writer.missing_container_hint(),
            )
            return None

        proposal = await self._draft_proposal(
            role=role,
            source_skills=source_skills,
            existing_skill_names=list(existing_skill_names),
        )
        if proposal is None:
            return None
        name, description, content = proposal

        reserved = {n.lower() for n in existing_skill_names}
        if name.lower() in reserved:
            logger.info("promotion_name_collision", unit_id=unit_id, name=name)
            return None

        page_body = _render_draft_page_markdown(
            description=description,
            content=content,
            tool_sequence=list(representative),
            source_skills=source_skills,
        )
        page_title = f"{AUTO_DRAFT_TITLE_PREFIX}{name}"
        page_id = await self._page_writer.create_draft_page(
            container=container,
            parent_title=self._parent_title,
            title=page_title,
            name=name,
            body_markdown=page_body,
        )
        if page_id is None:
            return None

        logger.info(
            "skill_promoted_draft",
            unit_id=unit_id,
            name=name,
            siblings=len(source_skills),
            tool_count=len(representative),
            backend=self._page_writer.backend,
            container=container,
            page_id=page_id,
        )
        return PromotionResult(
            skill_name=name,
            page_id=page_id,
            page_title=page_title,
            container_key=container,
        )

    async def _draft_proposal(
        self,
        *,
        role: Role,
        source_skills: list[SynthesizedSkill],
        existing_skill_names: list[str],
    ) -> tuple[str, str, str] | None:
        try:
            provider_key, provider = resolve_phase_provider(
                role, "auxiliary", self._llm_providers
            )
        except Exception:
            logger.debug("promotion_no_provider")
            return None
        user_prompt = _build_promotion_user_prompt(
            source_skills=source_skills,
            existing_skill_names=existing_skill_names,
        )
        try:
            completion = await complete_with_telemetry(
                provider=provider,
                messages=[
                    Message(role="system", content=_PROMOTION_SYSTEM_PROMPT),
                    Message(role="user", content=user_prompt),
                ],
                worker="promotion_synthesizer",
                role_name=role.name,
                provider_key=provider_key,
                event_queue=self._event_queue,
                temperature=0.2,
                max_tokens=(self._budget_tokens or None),
            )
        except Exception:
            logger.exception("promotion_llm_failed")
            return None
        text = (completion.content or "").strip()
        if not text:
            return None
        if text.upper().startswith(NOOP_SENTINEL):
            return None
        parsed = _extract_json_object(text)
        if parsed is None:
            logger.warning("promotion_unparseable", response=text[:200])
            return None
        name = str(parsed.get("name", "")).strip().lower()
        description = str(parsed.get("description", "")).strip()
        body = str(parsed.get("body", "")).strip()
        if not _NAME_PATTERN.match(name):
            logger.warning("promotion_bad_name", name=name)
            return None
        if not description or len(description) > 400:
            return None
        if not body or len(body) > 20000:
            return None
        return name, description, body


def _shortest_tool_sequence(sequences: list[list[str]]) -> list[str]:
    """Return the shortest non-empty sequence in ``sequences`` or []."""
    non_empty = [list(seq) for seq in sequences if seq]
    if not non_empty:
        return []
    non_empty.sort(key=len)
    return non_empty[0]


def _build_promotion_user_prompt(
    *,
    source_skills: list[SynthesizedSkill],
    existing_skill_names: list[str],
) -> str:
    blocks: list[str] = []
    for i, s in enumerate(source_skills):
        blocks.append(
            "\n".join(
                [
                    f"Sibling {i + 1} (by agent {s.agent_handle}):",
                    f"  Name: {s.name}",
                    f"  Description: {s.description}",
                    f"  Tools: {', '.join(s.tool_sequence) or '(none)'}",
                    f"  Body:\n{s.content}",
                ]
            )
        )
    existing_block = (
        "Existing skill names in this space (must not collide):\n  "
        + "\n  ".join(f"- {n}" for n in sorted(set(existing_skill_names)))
        if existing_skill_names
        else "Existing skill names in this space: (none)"
    )
    return (
        "Trigger: promotion\n\n"
        f"{existing_block}\n\n"
        "Sibling skills:\n\n"
        + "\n\n".join(blocks)
        + "\n\nDecide: NOOP, or emit the JSON object."
    )


def resolve_unit_container_attr(org: Any, unit_id: str, attr: str) -> str:
    """Walk the org tree to find ``unit_id`` and read one of its
    knowledge-container *identities* (``OrgUnit.confluence_space`` /
    ``OrgUnit.plane_project``), resolving ``${VAR}`` references (Tier B
    stores them verbatim).

    This is the unit's write / skill-promotion home.  Returns ``""``
    when the unit isn't found or has no container configured -- the
    promotion path treats that as a soft-skip (the unit isn't operating
    with that backend; nothing to draft).  Shared by both
    :class:`PromotionPageWriter` implementations so the walk-and-resolve
    logic stays single-sourced.
    """
    if org is None or not unit_id:
        return ""
    try:
        unit = org.get_unit(unit_id)
    except Exception:
        return ""
    if unit is None:
        return ""
    try:
        from crewlet.config import resolve_env_scalar

        return resolve_env_scalar(getattr(unit, attr, "") or "")
    except Exception:
        return getattr(unit, attr, "") or ""


def _render_draft_page_markdown(
    *,
    description: str,
    content: str,
    tool_sequence: list[str],
    source_skills: list[SynthesizedSkill],
) -> str:
    """Build the markdown body for a draft page (backend-neutral).

    Layout:

    * bold description paragraph (so the page title's brevity doesn't
      lose context),
    * the LLM-drafted body content verbatim (already markdown),
    * the common tool sequence as a bullet list,
    * a provenance block listing the contributing agents + skill ids
      so a reviewing lead can walk back to the originals.

    The page is intentionally simple -- plain markdown, no
    backend-specific macros -- so each backend's writer renders it to
    its native page format (Confluence storage XHTML / Plane
    ``description_html``) and a reviewing human can quickly read it,
    edit it inline, and publish via the normal UI.
    """
    lines: list[str] = []
    if description:
        lines.append(f"**{description}**")
    if content:
        lines.append(content)
    if tool_sequence:
        lines.append("**Common tool sequence:**")
        lines.append("\n".join(f"- `{tool}`" for tool in tool_sequence))
    lines.append("---")
    provenance = ["**Provenance (auto-drafted):**"]
    provenance.append(
        "\n".join(
            f"- {s.agent_handle} -- `{s.name}` (skill id: {s.id})"
            for s in source_skills
        )
    )
    lines.extend(provenance)
    return "\n\n".join(lines)


__all__ = [
    "NOOP_SENTINEL",
    "PromotionPageWriter",
    "PromotionResult",
    "PromotionSynthesizer",
    "SkillSynthesizer",
    "jaccard",
    "resolve_unit_container_attr",
]
