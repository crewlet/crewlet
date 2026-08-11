"""PersistDecider — post-turn reflection that decides what (if anything)
to write to the agent's personal memory.

Runs inside the :class:`~crewlet.learning.reflect_engine.ReflectEngine`
after every terminal turn.  The decider is the **deterministic harness**
piece of the learning stack: it runs regardless of whether the LLM
invoked a memory tool during the turn, so durable facts don't fall on
the floor when the planner forgets.

What it persists is still an LLM decision (compressing a turn into a
useful cross-session fact requires judgement).  The LLM call is made
on the role's ``llm_auxiliary`` model (cheap/fast) and is tightly
scoped: one tool-less completion, a short classification prompt, and
a JSON object identifying the tier.

The classifier emits one of four tiers:

- ``NOOP``: nothing durable in this turn.  Most common.
- ``DOC``: a STANDING RULE the team / org should follow.  No write
  -- a ``DirectiveObserved`` lifecycle event fires instead so an
  operator (or the agent's nudged manager-handoff flow) can route
  it into the team's documentation.  This is how the system says
  "this isn't memory, it's policy".
- ``LONG``: a personal fact the agent should remember indefinitely
  (a stakeholder preference, a project convention).  Stored at AGENT
  scope without a TTL.
- ``SHORT``: a personal fact with a known expiry (operational
  context like vacations, sprint focus, incident state).  Stored at
  AGENT scope with ``metadata.ttl_until`` set.  Read paths filter
  out expired rows.

All memory writes land at AGENT scope.  Unit-scope and org-scope
writes are NOT a category PersistDecider produces -- those belong in
documentation or are the SkillPromotion subsystem's responsibility,
not personal memory.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import Any, Literal

from crewlet._logging import get_logger
from crewlet.agent.phase_model import resolve_phase_provider
from crewlet.learning._aux_telemetry import complete_with_telemetry
from crewlet.learning.diary import KIND_LONG as DIARY_LONG
from crewlet.learning.diary import KIND_SHORT as DIARY_SHORT
from crewlet.learning.diary import AgentDiary, DiaryKind
from crewlet.learning.interaction import InboundInteraction
from crewlet.learning.personal_memory import fetch_existing_memories
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import LLMProvider, Message

logger = get_logger("learning.persist_decider")

NOOP_SENTINEL = "NOOP"

ClassificationKind = Literal["NOOP", "DOC", "LONG", "SHORT"]

# Diary kind values written by the decider.  Re-exported under
# ``KIND_LONG`` / ``KIND_SHORT`` so call sites (tests, prompts) can
# import the kind constants from one place.
KIND_LONG = DIARY_LONG
KIND_SHORT = DIARY_SHORT

# Default and cap on SHORT memory TTLs.  The aux LLM proposes a
# duration in days; the decider clamps it to a sane band so a single
# misclassification can't pin a row in the prompt forever.
_DEFAULT_SHORT_TTL_DAYS = 30
_MAX_SHORT_TTL_DAYS = 180

# Cap on how many existing memory entries we render into the decider's
# user prompt.  The dedup signal saturates well before hitting the
# read-side digest's pool size; we cap lower to keep aux-LLM input
# bounded for agents whose memory pool grows large.
_DEDUP_POOL_LIMIT = 50

# Truncate any single existing-memory entry rendered in the dedup
# block to this many characters -- enough for the LLM to recognise
# overlap with a candidate fact, short enough that one bloated row
# can't crowd out the others.
_DEDUP_ENTRY_CHAR_CAP = 240


@dataclass(frozen=True)
class PersistResult:
    """The write half of one ``decide_and_persist`` call.

    Populated only when the decider actually persisted a row (i.e.
    the LONG or SHORT tiers).  ``classification`` is the raw
    metadata kind (``memory_long`` / ``memory_short``) so dashboards
    and lifecycle code can match it against what the read side reads.
    """

    doc_id: str
    scope: str
    classification: str
    ttl_until: str = ""


@dataclass(frozen=True)
class PersistOutcome:
    """Outcome of one ``decide_and_persist`` call -- always returned.

    ``kind`` is the four-tier classification (``NOOP`` / ``DOC`` /
    ``LONG`` / ``SHORT``) so post-turn telemetry can plot the full
    distribution per agent.  ``result`` carries write-side detail
    only for ``LONG`` and ``SHORT`` (the only tiers that persist a
    row); it is ``None`` for ``NOOP`` (intentional skip), ``DOC``
    (handled via the ``DirectiveObservation`` callback rather than
    a knowledge write), and any error path -- callers must treat
    ``result is None`` as "nothing was written" regardless of
    ``kind``.
    """

    kind: ClassificationKind
    result: PersistResult | None = None

    @property
    def persisted(self) -> bool:
        """``True`` iff a row was written at agent scope."""
        return self.result is not None


@dataclass(frozen=True)
class DirectiveObservation:
    """Emitted (not stored) when the classifier picks ``DOC``.

    Surfaces a would-have-been-doc-worthy observation so operators can
    audit which directives the agent saw without forcing the engine to
    write to documentation.  The in-turn manager-handoff path is handled
    by the agent reaching its lead over a colleague-surface tool (a chat
    mention / issue comment); this event is the post-turn audit trail.
    """

    content: str
    target_hint: str
    rationale: str


_DECIDER_SYSTEM_PROMPT = """You are a post-turn reflection classifier.

Read the turn summary and decide which of four categories the
turn's content falls into:

  NOOP  -- Nothing durable.  This is the common case.  Use it when
           in doubt.

  DOC   -- The turn surfaced a STANDING RULE that the team or org
           should follow (e.g. "always commit in semantic style",
           "review every backend PR before merge").  Things that
           apply beyond just the agent's personal interactions and
           belong in the team's documentation, not in personal
           memory.  Output DOC; we do NOT memorize these -- the
           agent will be nudged to hand off to a team lead who has
           authority to update the docs.

  LONG  -- A personal fact the agent should remember indefinitely.
           Stakeholder preferences, project conventions the agent
           personally encountered, durable observations:
             - "Stakeholder Sam prefers terse replies"
             - "The auth service uses JWT in the Authorization header"
             - "Sarah Chen reviews backend PRs in the morning"
           Stored at agent scope without an expiry.

  SHORT -- A personal fact with a known expiry: operational context
           that ages out:
             - "Sarah is OOO until 2026-05-08"
             - "Q2 launch freeze active until end of June"
             - "Production rollback in progress; hold non-critical
                deploys"
           When you choose SHORT, propose ``ttl_days`` -- how many
           days the fact stays useful.  Default 30 if you can't tell.
           Cap 180.

Writing-style rules (for LONG and SHORT):

1. **Declarative facts, not instructions.**
   - "Stakeholder X prefers weekly digests" ✓
   - "Always send weekly digests" ✗
   - "Deploy script fails when CI is green but Slack webhook is
     misconfigured" ✓
   - "Check the Slack webhook first" ✗

2. **Always attribute the fact to the named requester / subject when
   one is shown in the turn context.**  Crewlet is multi-party --
   a fact stored without "who" loses its meaning.
   - "User Sam (slack:U0TESTUSER1) prefers replies opened with
     'hey sam'" ✓
   - "User prefers replies opened with 'hey sam'" ✗

3. **Never persist task-specific details that won't apply next time**
   (a single ticket ID, one-off debug output, ephemeral state).

4. **Never persist anything the turn itself already wrote via a tool.**

5. **Deduplicate against existing memory.**  The user prompt may
   include an "Already in your memory" list of facts the agent
   already remembers.  Return ``NOOP`` when the candidate fact:
   - restates an existing entry (same fact, possibly paraphrased), or
   - is a thinner / less-specific version of an existing entry
     (e.g. "Sam is OOO this week" when memory already has
     "Sam is OOO Mon-Fri, route backend reviews to Maria"), or
   - is a more-specific instance already covered by a broader
     existing rule.
   Only emit a new ``LONG`` / ``SHORT`` when the candidate fact adds
   information the existing entries do not already express.  Dedup
   does not apply to ``DOC`` -- standing rules are independent of
   personal memory.

For DOC, write ``content`` as the standing rule itself (so the
manager-handoff hint can include it verbatim) and ``target_hint``
if you have a sense of where it should land (e.g. "Engineering /
commit conventions"), else empty.

Output format (strict, JSON only -- no prose before or after):

  {"kind": "NOOP"}

  {"kind": "DOC", "content": "<rule>", "target_hint": "<hint or empty>",
   "rationale": "<one short sentence>"}

  {"kind": "LONG", "content": "<declarative fact>"}

  {"kind": "SHORT", "content": "<declarative fact>",
   "ttl_days": <int>}
"""


class PersistDecider:
    """Aux-LLM-backed four-tier classifier for personal memory.

    Writes LONG / SHORT entries to agent-scope knowledge.  Emits
    ``DirectiveObserved`` callbacks (when a callback is wired) for
    DOC classifications.  NOOP returns ``None``.
    """

    def __init__(
        self,
        *,
        llm_providers: dict[str, LLMProvider],
        diary: AgentDiary,
        budget_tokens: int = 5000,
        event_queue: Any = None,
        on_doc_observed: Any = None,
    ) -> None:
        self._llm_providers = llm_providers
        self._diary = diary
        self._budget_tokens = budget_tokens
        self._event_queue = event_queue
        # Optional async callback ``async (DirectiveObservation) -> None``
        # invoked when the classifier picks ``DOC``.  Engine wires this
        # to publish a lifecycle event; tests can inject a recorder.
        self._on_doc_observed = on_doc_observed

    async def decide_and_persist(
        self,
        *,
        role: Role,
        agent_id: str,
        agent_handle: str,
        unit_id: str,
        org_id: str,
        turn_id: str,
        task_summary: str,
        plan_summary: str,
        tool_sequence: list[str],
        review_outcome: str,
        interactions: list[InboundInteraction] | None = None,
    ) -> PersistOutcome:
        """Run one decider pass.  Always returns a :class:`PersistOutcome`.

        ``outcome.kind`` is the four-tier classification
        (``NOOP`` / ``DOC`` / ``LONG`` / ``SHORT``).  ``outcome.result``
        is populated only for ``LONG`` and ``SHORT`` (the tiers that
        persist a row); ``DOC`` and ``NOOP`` -- and any error path --
        return ``result=None``.

        ``unit_id`` and ``org_id`` are accepted for backward
        compatibility with the call site but are not used -- all
        memory writes are agent-scope.
        """
        del unit_id, org_id  # unused in the four-tier model
        try:
            provider_key, provider = resolve_phase_provider(
                role, "auxiliary", self._llm_providers
            )
        except Exception:
            logger.debug("persist_decider_provider_unavailable", turn_id=turn_id)
            return PersistOutcome(kind="NOOP")
        # Dedup signal: pull the agent's currently-effective LONG/SHORT
        # memories so the classifier can reject restatements / thinner
        # versions of facts already known.  ``fetch_existing_memories``
        # swallows fetch failures and returns ``[]`` -- the decider
        # then runs without a dedup block, falling back to its
        # pre-dedup behaviour rather than skipping the whole turn.
        existing_memories = await fetch_existing_memories(
            diary=self._diary,
            agent_id=agent_id,
            candidate_pool_limit=_DEDUP_POOL_LIMIT,
        )
        user_prompt = _build_user_prompt(
            task_summary=task_summary,
            plan_summary=plan_summary,
            tool_sequence=tool_sequence,
            review_outcome=review_outcome,
            interactions=interactions,
            existing_memories=existing_memories,
        )
        try:
            completion = await complete_with_telemetry(
                provider=provider,
                messages=[
                    Message(role="system", content=_DECIDER_SYSTEM_PROMPT),
                    Message(role="user", content=user_prompt),
                ],
                worker="persist_decider",
                role_name=role.name,
                provider_key=provider_key,
                event_queue=self._event_queue,
                agent_id=agent_id,
                turn_id=turn_id,
                temperature=0.2,
                max_tokens=(self._budget_tokens or None),
            )
        except Exception:
            logger.exception("persist_decider_llm_failed", turn_id=turn_id)
            return PersistOutcome(kind="NOOP")
        text = (completion.content or "").strip()
        if not text:
            logger.debug("persist_decider_empty_response", turn_id=turn_id)
            return PersistOutcome(kind="NOOP")
        # Tolerate a bare "NOOP" sentinel some models produce instead
        # of the JSON contract.
        if text.upper().startswith(NOOP_SENTINEL):
            logger.debug("persist_decider_noop", turn_id=turn_id)
            return PersistOutcome(kind="NOOP")

        parsed = _extract_json_object(text)
        if parsed is None:
            logger.warning(
                "persist_decider_unparseable", turn_id=turn_id, response=text[:200]
            )
            return PersistOutcome(kind="NOOP")

        kind_raw = str(parsed.get("kind", "")).strip().upper()
        if kind_raw == "NOOP":
            logger.debug("persist_decider_noop", turn_id=turn_id)
            return PersistOutcome(kind="NOOP")
        content = str(parsed.get("content", "")).strip()
        if not content:
            logger.debug("persist_decider_empty_content", turn_id=turn_id)
            return PersistOutcome(kind="NOOP")

        if kind_raw == "DOC":
            await self._handle_doc(
                content=content,
                target_hint=str(parsed.get("target_hint", "")).strip(),
                rationale=str(parsed.get("rationale", "")).strip(),
                turn_id=turn_id,
                agent_handle=agent_handle,
            )
            return PersistOutcome(kind="DOC")

        if kind_raw == "LONG":
            result = await self._write_memory(
                content=content,
                kind=KIND_LONG,
                ttl_until=None,
                agent_id=agent_id,
                agent_handle=agent_handle,
                turn_id=turn_id,
                review_outcome=review_outcome,
            )
            return PersistOutcome(kind="LONG", result=result)

        if kind_raw == "SHORT":
            ttl_days = _coerce_ttl_days(parsed.get("ttl_days"))
            ttl_until = datetime.now(UTC) + timedelta(days=ttl_days)
            result = await self._write_memory(
                content=content,
                kind=KIND_SHORT,
                ttl_until=ttl_until,
                agent_id=agent_id,
                agent_handle=agent_handle,
                turn_id=turn_id,
                review_outcome=review_outcome,
            )
            return PersistOutcome(kind="SHORT", result=result)

        logger.warning("persist_decider_bad_kind", turn_id=turn_id, kind=kind_raw)
        return PersistOutcome(kind="NOOP")

    async def _handle_doc(
        self,
        *,
        content: str,
        target_hint: str,
        rationale: str,
        turn_id: str,
        agent_handle: str,
    ) -> None:
        """DOC classification -- no memory write; emit observation."""
        logger.info(
            "persist_decider_doc_observed",
            turn_id=turn_id,
            agent_handle=agent_handle,
            content_preview=content[:120],
            target_hint=target_hint,
        )
        if self._on_doc_observed is None:
            return
        try:
            await self._on_doc_observed(
                DirectiveObservation(
                    content=content,
                    target_hint=target_hint,
                    rationale=rationale,
                )
            )
        except Exception:
            logger.exception("persist_decider_doc_callback_failed", turn_id=turn_id)

    async def _write_memory(
        self,
        *,
        content: str,
        kind: DiaryKind,
        ttl_until: datetime | None,
        agent_id: str,
        agent_handle: str,
        turn_id: str,
        review_outcome: str,
    ) -> PersistResult | None:
        if not agent_id:
            logger.warning("persist_decider_no_agent_id", turn_id=turn_id)
            return None
        # The diary's structured columns absorb kind / ttl_until /
        # source / turn_id; remaining row metadata stays in the JSONB
        # blob (review_outcome, agent_handle).
        metadata: dict[str, Any] = {
            "review_outcome": review_outcome,
            "agent_handle": agent_handle,
        }
        try:
            doc_id = await self._diary.write(
                agent_id=agent_id,
                kind=kind,
                content=content,
                ttl_until=ttl_until,
                source="persist_decider",
                turn_id=turn_id,
                metadata=metadata,
            )
        except Exception:
            logger.exception("persist_decider_store_failed", turn_id=turn_id)
            return None
        ttl_str = ttl_until.isoformat() if ttl_until is not None else ""
        logger.info(
            "persist_decider_stored",
            turn_id=turn_id,
            doc_id=doc_id,
            kind=kind,
            agent_handle=agent_handle,
            ttl_until=ttl_str,
        )
        return PersistResult(
            doc_id=str(doc_id),
            scope="agent",
            classification=kind,
            ttl_until=ttl_str,
        )


def _coerce_ttl_days(value: Any) -> int:
    """Clamp the LLM-proposed TTL to the supported band."""
    try:
        days = int(value)
    except (TypeError, ValueError):
        return _DEFAULT_SHORT_TTL_DAYS
    if days < 1:
        return _DEFAULT_SHORT_TTL_DAYS
    return min(days, _MAX_SHORT_TTL_DAYS)


def _build_user_prompt(
    *,
    task_summary: str,
    plan_summary: str,
    tool_sequence: list[str],
    review_outcome: str,
    interactions: list[InboundInteraction] | None = None,
    existing_memories: list[dict[str, Any]] | None = None,
) -> str:
    tools_rendered = ", ".join(tool_sequence) if tool_sequence else "(none)"
    # One requester/message pair per interaction with an identifiable
    # sender — a coalesced trigger renders each constituent message so
    # the classifier sees who asked what, not just the last ping.
    sender_lines: list[str] = []
    for interaction in interactions or []:
        if not interaction.has_sender:
            continue
        described = interaction.sender.describe()
        if described:
            sender_lines.append(f"- Requester: {described}")
        if interaction.body:
            snippet = interaction.body.strip().replace("\n", " ")
            sender_lines.append(f'- Inbound message: "{snippet}"')
    sender_block = ("\n" + "\n".join(sender_lines) + "\n") if sender_lines else ""
    memory_block = _render_existing_memories_block(existing_memories or [])
    return (
        "Turn summary:\n"
        f"- Task: {task_summary or '(no description)'}\n"
        f"- Plan: {plan_summary or '(no plan)'}\n"
        f"- Tools called: {tools_rendered}\n"
        f"- Outcome: {review_outcome}"
        f"{sender_block}"
        f"{memory_block}"
        "\nClassify and emit the JSON object."
    )


def _render_existing_memories_block(memories: list[dict[str, Any]]) -> str:
    """Render the dedup context as an "Already in your memory" block.

    Returns the empty string when no memories are passed in so the
    prompt stays compact for fresh agents and for turns where the
    knowledge fetch failed (callers pass ``[]`` in both cases).

    Each entry's ``ttl_until`` is appended in parentheses for SHORT
    entries so the LLM can distinguish "agent already knows this and
    the row is still live" from "agent knew this until last week".
    """
    if not memories:
        return ""
    lines: list[str] = ["", "Already in your memory:"]
    for i, doc in enumerate(memories):
        content = (doc.get("content") or "").strip().replace("\n", " ")
        if not content:
            continue
        if len(content) > _DEDUP_ENTRY_CHAR_CAP:
            content = content[:_DEDUP_ENTRY_CHAR_CAP] + "..."
        meta = doc.get("metadata") or {}
        ttl = meta.get("ttl_until") if isinstance(meta, dict) else None
        if ttl:
            lines.append(f"{i}. {content} (until {ttl})")
        else:
            lines.append(f"{i}. {content}")
    if len(lines) == 2:
        # Every doc had empty content; emit nothing rather than a
        # bare header.
        return ""
    return "\n".join(lines) + "\n"


def _extract_json_object(text: str) -> dict[str, Any] | None:
    """Best-effort JSON-object extraction.

    Accepts the full response as JSON first; falls back to scanning
    for the first ``{...}`` block if the model wrapped its answer in
    prose (which the system prompt forbids but models occasionally do
    anyway).
    """
    candidates: list[str] = [text]
    start = text.find("{")
    end = text.rfind("}")
    if start != -1 and end != -1 and end > start:
        candidates.append(text[start : end + 1])
    for candidate in candidates:
        try:
            obj = json.loads(candidate)
        except Exception:
            continue
        if isinstance(obj, dict):
            return obj
    return None
