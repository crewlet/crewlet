"""CounterpartyProfiler — LLM-backed observed-traits updater.

Runs inside the :class:`~crewlet.learning.reflect_engine.ReflectEngine`
alongside :class:`~crewlet.learning.persist_decider.PersistDecider`.
For each completed turn whose trigger had an identifiable sender, the
profiler feeds the inbound body + plan summary + review outcome to the
role's auxiliary model and asks it to emit a **JSON patch** of
newly-observed traits about the subject, or ``NOOP``.

The patch is merged into the existing profile via
:meth:`CounterpartyStore.upsert` (JSONB ``||`` merge — new keys add,
existing keys overwrite, untouched keys are retained).

Design parity with :class:`PersistDecider`:
- auxiliary LLM model, not the turn's primary model,
- short declarative-facts prompt,
- best-effort: failures are logged, never raised,
- LLM cooperation is welcome but the *trigger* for running is
  deterministic (every turn with an identifiable counterparty).
"""

from __future__ import annotations

import json
from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.phase_model import resolve_phase_provider
from crewlet.learning._aux_telemetry import complete_with_telemetry
from crewlet.learning.counterparty_store import CounterpartyStoreProtocol
from crewlet.learning.interaction import InboundInteraction
from crewlet.learning.persist_decider import _extract_json_object
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import LLMProvider, Message

logger = get_logger("learning.counterparty_profiler")

NOOP_SENTINEL = "NOOP"

_PROFILER_SYSTEM_PROMPT = """You are a counterparty-observation assistant.

Your job is to read ONE recent interaction between the observing
agent and a subject, and emit any newly-observed traits about the
SUBJECT (never about the observer, never about the task).

Rules:
1. If the interaction contains nothing observation-worthy about the
   subject, respond exactly with NOOP.
2. Emit only things you can point to evidence for in THIS
   interaction.  Do not invent; do not repeat what you already know.
3. Write **observations of the SUBJECT**, including their explicit
   preferences and directives.  Phrase them as facts about the
   subject, not commands to yourself.
   - "communication_style: terse, action-oriented" ✓
   - "preferred_greeting: 'hey sam'" ✓
   - "directives: ['expects 'hey sam' as the opening of every
     reply']" ✓
   - "Always be terse with them" ✗  (instruction-shaped)
   - "Always greet with 'hey sam'" ✗  (instruction-shaped)
4. **Capture explicit subject preferences when they're stated.**  If
   the subject says "always X" or "I want you to Y" or "call me Z",
   record that as a trait about THEM.  These are the highest-value
   observations — they're how the observing agent personalises future
   replies without re-asking.
5. Prefer short keys.  Typical ones:
   - communication_style
   - preferred_greeting
   - directives
   - interests
   - sensitivities
   - decisions
   - notes
6. Values may be strings or short lists.  Keep each under ~200
   chars.  Never put task-specific identifiers (ticket IDs, URLs)
   in the output.

Output format (strict):
- Either the literal string: NOOP
- Or a single JSON object mapping trait keys to values.  Example:
  {"preferred_greeting": "hey sam", "directives": ["expects 'hey sam' as opening"]}
- Never include prose before or after the JSON."""


class CounterpartyProfiler:
    """LLM-backed profile updater."""

    def __init__(
        self,
        *,
        llm_providers: dict[str, LLMProvider],
        store: CounterpartyStoreProtocol,
        budget_tokens: int = 3000,
        event_queue: Any = None,
        budget_manager: Any = None,
    ) -> None:
        self._llm_providers = llm_providers
        self._store = store
        self._budget_tokens = budget_tokens
        self._event_queue = event_queue
        self._budget_manager = budget_manager

    async def update_from_turn(
        self,
        *,
        role: Role,
        observer_handle: str,
        interaction: InboundInteraction,
        plan_summary: str,
        review_outcome: str,
        turn_id: str,
        observer_agent_id: str = "",
    ) -> int | None:
        """Update the profile for one turn's counterparty.

        Returns the count of trait keys patched (0 when the LLM emitted
        NOOP but ``interaction_count`` was still bumped, N>0 on a real
        observation), or ``None`` when no counterparty was identifiable
        or the store call failed.  ReflectEngine uses the return value
        to populate the lifecycle event the dashboard groups by trace.

        No-op when no counterparty can be identified (the
        :class:`InboundInteraction` has no resolvable sender).  On LLM
        failure or NOOP response, the row's ``interaction_count`` is
        still incremented via a no-trait upsert so observation cadence
        is tracked.
        """
        sender = interaction.sender
        if not sender.is_identifiable:
            return None

        subject_label = sender.label
        patch = await self._derive_patch(
            role=role,
            subject_name=subject_label,
            trigger_body=interaction.body,
            plan_summary=plan_summary,
            review_outcome=review_outcome,
            turn_id=turn_id,
            observer_agent_id=observer_agent_id,
        )

        try:
            profile = await self._store.upsert(
                observer_handle=observer_handle,
                subject_handle=sender.handle,
                subject_external_id=sender.external_id,
                subject_platform=sender.platform,
                subject_name=sender.display_name,
                traits_patch=patch,
                increment_interactions=True,
            )
        except Exception:
            logger.exception("counterparty_upsert_failed", turn_id=turn_id)
            return None
        logger.info(
            "counterparty_profile_updated",
            turn_id=turn_id,
            observer=observer_handle,
            subject_handle=sender.handle,
            subject_platform=sender.platform,
            traits_patched=len(patch),
            interaction_count=profile.interaction_count,
        )
        return len(patch)

    async def _derive_patch(
        self,
        *,
        role: Role,
        subject_name: str,
        trigger_body: str,
        plan_summary: str,
        review_outcome: str,
        turn_id: str,
        observer_agent_id: str = "",
    ) -> dict[str, Any]:
        try:
            provider_key, provider = resolve_phase_provider(
                role, "auxiliary", self._llm_providers
            )
        except Exception:
            logger.debug("counterparty_no_provider", turn_id=turn_id)
            return {}

        user_prompt = _build_user_prompt(
            subject_name=subject_name,
            trigger_body=trigger_body,
            plan_summary=plan_summary,
            review_outcome=review_outcome,
        )
        try:
            completion = await complete_with_telemetry(
                provider=provider,
                messages=[
                    Message(role="system", content=_PROFILER_SYSTEM_PROMPT),
                    Message(role="user", content=user_prompt),
                ],
                worker="counterparty_profiler",
                role_name=role.name,
                provider_key=provider_key,
                event_queue=self._event_queue,
                budget_manager=self._budget_manager,
                agent_id=observer_agent_id,
                turn_id=turn_id,
                temperature=0.2,
                max_tokens=(self._budget_tokens or None),
            )
        except Exception:
            logger.exception("counterparty_profiler_llm_failed", turn_id=turn_id)
            return {}

        text = (completion.content or "").strip()
        if not text:
            return {}
        if text.upper().startswith(NOOP_SENTINEL):
            return {}

        parsed = _extract_json_object(text)
        if parsed is None:
            logger.warning(
                "counterparty_profiler_unparseable",
                turn_id=turn_id,
                response=text[:200],
            )
            return {}
        return _sanitise_patch(parsed)


def _build_user_prompt(
    *,
    subject_name: str,
    trigger_body: str,
    plan_summary: str,
    review_outcome: str,
) -> str:
    return (
        f"Subject: {subject_name or '(unknown)'}\n\n"
        f"Inbound from subject:\n{trigger_body or '(no body)'}\n\n"
        f"Agent's plan for this turn: {plan_summary or '(no plan)'}\n"
        f"Turn outcome: {review_outcome}\n\n"
        "Emit NOOP, or a JSON object of newly-observed traits."
    )


_MAX_VALUE_LEN = 200
_MAX_DICT_DEPTH = 2


def _clean_str_value(value: Any) -> str:
    """Sanitise an LLM-emitted trait value before storage.

    Collapses whitespace runs (including newlines) into single spaces
    and truncates to ``_MAX_VALUE_LEN`` so the system prompt's
    ~200-char target is enforced server-side; without this guard a
    runaway aux-LLM emission could bloat downstream prompts or smuggle
    newlines that look like prompt structure.
    """
    text = str(value)
    cleaned = " ".join(text.split())
    if len(cleaned) > _MAX_VALUE_LEN:
        cleaned = cleaned[:_MAX_VALUE_LEN].rstrip()
    return cleaned


def _sanitise_nested(value: Any, depth: int = 0) -> Any:
    """Recursively apply trait-value sanitisation to a JSONB scalar
    or container.  Strings + list items get the standard whitespace +
    length guards; nested dicts recurse up to ``_MAX_DICT_DEPTH``
    levels with the same per-key + per-value caps.  Anything beyond
    the depth cap is dropped so a malicious / runaway LLM can't
    smuggle long multi-line strings inside an arbitrarily deep dict
    tree.
    """
    if value is None:
        return None
    if isinstance(value, str):
        cleaned = _clean_str_value(value)
        return cleaned or None
    if isinstance(value, bool):  # subclass of int — check first
        return value
    if isinstance(value, (int, float)):
        return value
    if isinstance(value, (list, tuple)):
        out_list: list[Any] = []
        for item in value:
            cleaned = _sanitise_nested(item, depth=depth)
            if cleaned not in (None, ""):
                out_list.append(cleaned)
        return out_list[:20] if out_list else None
    if isinstance(value, dict):
        if depth >= _MAX_DICT_DEPTH:
            return None
        out_dict: dict[str, Any] = {}
        for k, v in value.items():
            if not isinstance(k, str):
                continue
            ck = k.strip()
            if not ck or len(ck) > 64:
                continue
            cv = _sanitise_nested(v, depth=depth + 1)
            if cv is None or cv == "":
                continue
            out_dict[ck] = cv
            if len(out_dict) >= 16:
                break
        return out_dict or None
    return None


def _sanitise_patch(patch: dict[str, Any]) -> dict[str, Any]:
    """Drop obviously-unsafe keys/values from the LLM-emitted patch.

    - skips empty keys and empty values,
    - coerces non-scalar values to JSON-safe forms,
    - truncates string values + list items to ``_MAX_VALUE_LEN`` and
      collapses whitespace, so a runaway aux-LLM emission can't
      bloat Plan / lookup_colleague prompts or smuggle newlines that
      look like prompt structure,
    - caps key count to 16 to prevent pathological expansion.
    """
    if not isinstance(patch, dict):
        return {}
    out: dict[str, Any] = {}
    for key, value in patch.items():
        if not isinstance(key, str):
            continue
        clean_key = key.strip()
        if not clean_key or len(clean_key) > 64:
            continue
        if value is None or value == "":
            continue
        if isinstance(value, (list, tuple)):
            cleaned = [
                _clean_str_value(v) for v in value if v is not None and str(v).strip()
            ]
            cleaned = [v for v in cleaned if v]
            if not cleaned:
                continue
            out[clean_key] = cleaned[:20]
        elif isinstance(value, str):
            cleaned_str = _clean_str_value(value)
            if not cleaned_str:
                continue
            out[clean_key] = cleaned_str
        elif isinstance(value, (int, float, bool)):
            out[clean_key] = value
        elif isinstance(value, dict):
            # Nested dicts are rare from the LLM but the schema allows
            # them.  Recursively apply the same whitespace + length
            # caps so a malicious / runaway emission can't bypass the
            # scalar guards by wrapping long multi-line strings in a
            # dict envelope.
            sanitised = _sanitise_nested(value)
            if not sanitised:
                continue
            try:
                json.dumps(sanitised)
            except Exception:
                continue
            out[clean_key] = sanitised
        else:
            continue
        if len(out) >= 16:
            break
    return out
