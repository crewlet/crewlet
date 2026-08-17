"""Built-in tools available to agents."""

from __future__ import annotations

import difflib
import re
import unicodedata
from dataclasses import dataclass
from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import SkillUsed
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry

logger = get_logger("tools.builtin")

# Methods that count as definitive matches — the registry returned the
# agent for an exact handle / external ID / role-name lookup, so we
# should never disambiguate or surface a "fuzzy match" hint.
_EXACT_METHODS = frozenset({"exact_handle", "exact_role", "external_id"})

# difflib ratio cutoff below which we don't even consider a candidate.
# 0.6 catches single-character typos and minor variations without
# flooding the list with unrelated names.
_FUZZY_CUTOFF = 0.6

# Minimum query length before tier 3 (substring) runs. Below this, a
# 1-2 char query like ``"a"`` would surface every agent whose name
# happens to contain those characters; require at least 3 chars so
# the query carries enough signal to be a deliberate partial name.
_MIN_SUBSTRING_QUERY_LEN = 3

# Minimum query length before tier 4 (difflib fuzzy) runs. Ratios on
# 1-3 char strings are unreliable — ratio('ceo','veo') is 0.67 from
# a single shared character, well above the cutoff. Require 4+ chars
# so a typo has enough signal for the score to mean something.
_MIN_FUZZY_QUERY_LEN = 4

# How many candidates ``_lookup_colleague`` will render in a disambiguation
# list before truncating with a "...and N more" hint.
_LOOKUP_DISPLAY_LIMIT = 10

# Whitespace + underscore + ASCII hyphen-minus + Unicode dashes
# (figure dash U+2012, hyphen U+2010, non-breaking hyphen U+2011,
# en/em/horizontal-bar dashes U+2013..U+2015, minus sign U+2212).
# Keeps role names with em-dashes pasted from Confluence normalising
# the same as ones with ASCII hyphens.
_NORMALIZE_RE = re.compile(r"[\s_\-‐-―−]+")

# Length cap for query / role-name strings interpolated into LLM-
# visible output and structured logs. Prevents pathological long
# strings or smuggled newlines from breaking line-structured renders
# and bounds the size of audit log payloads.
_DISPLAY_MAX_LEN = 80


def _safe_for_display(value: str, *, max_len: int = _DISPLAY_MAX_LEN) -> str:
    """Collapse internal whitespace and truncate for safe interpolation.

    Any run of whitespace (including embedded ``\\n`` / ``\\t``)
    collapses to a single space so untrusted input can't break out of
    a line-oriented format or splice phantom rows into a candidate
    list. Truncated with a single-char ellipsis when over ``max_len``.
    """
    if not value:
        return value
    collapsed = " ".join(value.split())
    if len(collapsed) > max_len:
        return collapsed[: max_len - 1] + "…"
    return collapsed


def _join_surfaces(names: list[str]) -> str:
    """Render transport names as an English list: "a", "a or b", "a, b or c"."""
    labels = [name.replace("_", " ") for name in names]
    if len(labels) == 1:
        return labels[0]
    return f"{', '.join(labels[:-1])} or {labels[-1]}"


@dataclass(frozen=True)
class _Candidate:
    """One ranked candidate produced by ``_resolve_colleague_candidates``."""

    handle: str
    role_name: str
    method: str
    # difflib ratio for ``fuzzy`` matches; 1.0 for everything else.
    # Carried forward so the disambiguation surface can show the LLM
    # a confidence number and ordering can be score-aware.
    score: float = 1.0
    # Seat kind — "agent" or "human".  Drives the identity block and
    # the disambiguation row marker.
    kind: str = "agent"
    # The resolved party itself, so render paths and kind consumers
    # never re-resolve (each re-resolution is another registry/org
    # round).  ``None`` only for degenerate stub registries.
    party: Any = None


def _normalize_identifier(value: str) -> str:
    """Unicode-fold and collapse an identifier for cross-style matching.

    NFKD-decompose + casefold + strip combining marks so Turkish
    ``İK`` folds to ``ik`` (not ``i̇k`` with combining dot), German
    ``ß`` folds to ``ss``, and full-width Latin folds to ASCII.
    Turkish dotless ``ı`` (U+0131) isn't decomposed by NFKD and
    doesn't casefold to ASCII ``i``, so we map it explicitly — lets
    an ASCII query ``yazilim`` reach a role named ``Yazılım``.
    Then collapse runs of whitespace, ASCII / Unicode dashes, and
    underscores to a single space — ``Agent CEO`` / ``agent-ceo`` /
    ``agent_ceo`` / ``agent—ceo`` all normalise to ``agent ceo``.
    """
    if not value:
        return ""
    decomposed = unicodedata.normalize("NFKD", value).casefold()
    stripped = "".join(c for c in decomposed if not unicodedata.combining(c))
    stripped = stripped.replace("ı", "i")
    return _NORMALIZE_RE.sub(" ", stripped).strip()


def _has_word_aligned_substring(haystack: str, needle: str) -> bool:
    """Return True iff ``needle`` appears in ``haystack`` starting at a
    word boundary (string start or after whitespace).

    Used by tier 3 to reject mid-word fragments: query ``"log"`` does
    not match ``"blog editor"`` (``log`` starts mid-``blog``), but
    query ``"ceo"`` matches ``"agent ceo"`` (after space) and query
    ``"engineer"`` matches ``"engineering lead"`` (start of string).
    """
    idx = 0
    while True:
        pos = haystack.find(needle, idx)
        if pos == -1:
            return False
        if pos == 0 or haystack[pos - 1] == " ":
            return True
        idx = pos + 1


def _substring_or_token_match(q_norm: str, q_tokens: list[str], name_norm: str) -> bool:
    """Tier-3 match test against a single normalised name.

    Forward direction (``q_norm`` appears in ``name_norm`` at a word
    boundary) catches typed-partial matches: query ``"ceo"`` against
    name ``"agent ceo"``. The word-boundary guard rejects mid-word
    fragments so query ``"log"`` does NOT match ``"blog editor"``.

    Reverse direction (``name_norm`` appears as a contiguous run of
    whole tokens in ``q_norm``) catches surrounding-context matches:
    query ``"senior engineer"`` against role name ``"engineer"``.
    """
    if not name_norm:
        return False
    if _has_word_aligned_substring(name_norm, q_norm):
        return True
    name_tokens = name_norm.split()
    if not name_tokens or len(name_tokens) > len(q_tokens):
        return False
    span = len(name_tokens)
    for i in range(len(q_tokens) - span + 1):
        if q_tokens[i : i + span] == name_tokens:
            return True
    return False


@dataclass(frozen=True)
class _CorpusEntry:
    """One seat in the lookup corpus, with pre-normalized match keys."""

    party: Any
    h_norm: str
    n_norm: str

    @property
    def handle(self) -> str:
        return self.party.handle


def _colleague_corpus(registry: Any) -> list[_CorpusEntry]:
    """Every addressable seat, from the registry's party enumeration.

    The party mapping (who counts as a seat, how handles/names derive)
    is the registry's job — ``all_parties()`` — so tier 2-4 matching
    can never disagree with tier-1 ``resolve_party`` results.  Only
    the lookup-specific normalization lives here.
    """
    return [
        _CorpusEntry(
            party=party,
            h_norm=_normalize_identifier(party.handle),
            n_norm=_normalize_identifier(party.name),
        )
        for party in registry.all_parties()
    ]


def _resolve_colleague_candidates(
    query: str, context: AgentContext
) -> list[_Candidate]:
    """Return all ranked candidate matches for ``query``.

    The corpus covers every addressable seat — live agents AND human
    seats.  Tiers, in priority order — earlier tiers short-circuit
    later ones:
      1. Exact handle / external ID / role name (case-sensitive).
      2. Case-insensitive normalised match on handle or role name.
      3. Substring / whole-token match on handle or role name.
         Skipped when ``len(q_norm) < _MIN_SUBSTRING_QUERY_LEN``.
      4. ``difflib`` fuzzy ratio above :data:`_FUZZY_CUTOFF`.
         Skipped when ``len(q_norm) < _MIN_FUZZY_QUERY_LEN``.

    Returns an empty list when the registry is missing or no tier
    matched. The list is deduplicated by handle. Tier-3 results sort
    alphabetically by handle; tier-4 by descending score then handle.
    Returns ALL matches without internal truncation; callers slice
    for display.
    """
    registry = context.handle_registry
    if registry is None:
        return []

    candidates: list[_Candidate] = []
    seen: set[str] = set()

    def _add(party: Any, method: str, *, score: float = 1.0) -> None:
        if party is None or not party.handle or party.handle in seen:
            return
        candidates.append(
            _Candidate(
                handle=party.handle,
                role_name=party.name,
                method=method,
                score=score,
                kind=party.kind.value,
                party=party,
            )
        )
        seen.add(party.handle)

    # Tier 1 — exact matches via the registry's own indexes.
    _add(registry.resolve_party(query), "exact_handle")

    # Slack-style underscores → canonical hyphenated handle.
    normalised_handle = query.replace("_", "-")
    if normalised_handle != query:
        _add(registry.resolve_party(normalised_handle), "exact_handle")

    for transport in registry.all_transports():
        _add(registry.resolve_party_external(transport, query), "external_id")

    _add(registry.resolve_party_role_name(query), "exact_role")

    if candidates:
        # Sort tier-1 candidates alphabetically by handle for stable
        # disambiguation ordering across registries / pool insertion
        # orders; tier-3 already sorts the same way below.
        candidates.sort(key=lambda c: c.handle)
        logger.debug(
            "lookup_colleague_tier1_resolved",
            query=_safe_for_display(query),
            count=len(candidates),
            handles=[c.handle for c in candidates],
        )
        return candidates

    # Tiers 2-4 work over the full seat corpus (agents ∪ humans).
    q_norm = _normalize_identifier(query)
    if not q_norm:
        return []

    corpus = _colleague_corpus(registry)

    # Tier 2 — case-insensitive normalised equality. A hit here is
    # strong enough to short-circuit substring / fuzzy: it means the
    # query is the role name or handle modulo case + separators.
    for entry in corpus:
        if entry.h_norm == q_norm or entry.n_norm == q_norm:
            _add(entry.party, "case_insensitive")
    if candidates:
        candidates.sort(key=lambda c: c.handle)
        logger.debug(
            "lookup_colleague_tier2_resolved",
            query=_safe_for_display(query),
            count=len(candidates),
            handles=[c.handle for c in candidates],
        )
        return candidates

    # Tier 3 — substring / whole-token match.
    if len(q_norm) >= _MIN_SUBSTRING_QUERY_LEN:
        q_tokens = q_norm.split()
        tier3_hits = [
            entry
            for entry in corpus
            if _substring_or_token_match(q_norm, q_tokens, entry.h_norm)
            or _substring_or_token_match(q_norm, q_tokens, entry.n_norm)
        ]
        # Sort alphabetically by handle so the disambiguation list is
        # stable across pool insertion orders.
        tier3_hits.sort(key=lambda e: e.handle)
        for entry in tier3_hits:
            _add(entry.party, "substring")
        if candidates:
            logger.debug(
                "lookup_colleague_tier3_resolved",
                query=_safe_for_display(query),
                count=len(candidates),
                handles=[c.handle for c in candidates],
            )

    # Tier 4 — difflib fuzzy ratio. Only consult when earlier tiers
    # yielded nothing AND the query is long enough for ratios to mean
    # something (3-char ratios cross the cutoff on a single shared
    # character).
    if not candidates and len(q_norm) >= _MIN_FUZZY_QUERY_LEN:
        scored: list[tuple[float, _CorpusEntry]] = []
        for entry in corpus:
            h_ratio = (
                difflib.SequenceMatcher(None, q_norm, entry.h_norm).ratio()
                if entry.h_norm
                else 0.0
            )
            n_ratio = (
                difflib.SequenceMatcher(None, q_norm, entry.n_norm).ratio()
                if entry.n_norm
                else 0.0
            )
            score = max(h_ratio, n_ratio)
            if score >= _FUZZY_CUTOFF:
                scored.append((score, entry))
        # Descending score; ascending handle as deterministic tiebreaker.
        scored.sort(key=lambda s: (-s[0], s[1].handle))
        for score, entry in scored:
            _add(entry.party, "fuzzy", score=score)
        if candidates:
            logger.debug(
                "lookup_colleague_tier4_resolved",
                query=_safe_for_display(query),
                count=len(candidates),
                handles=[c.handle for c in candidates],
            )

    return candidates


_METHOD_LABELS: dict[str, str] = {
    "case_insensitive": "case / format match",
    "substring": "partial-name match",
    "fuzzy": "fuzzy match",
}


def resolve_colleague_party(query: str, context: AgentContext) -> Any:
    """Resolve ``query`` to a single unambiguous party, or ``None``.

    Returns the matched candidate's :class:`ResolvedParty` iff exactly
    one candidate was found.  The candidate list is deduped by handle,
    so any length > 1 means distinct seats matched and we cannot
    safely pick one -- return ``None`` so callers fall back to the raw
    query rather than silently routing to the wrong colleague. This
    covers both ambiguous fuzzy queries AND tier-1 collisions where
    e.g. a Slack external ID coincides with another seat's handle.

    The one resolver for colleague-routing consumers (``a2a_ask`` and
    the ``lookup_colleague`` builtin) — they read ``party.kind`` and
    ``party.role`` off the result instead of re-resolving.

    Logs at ``info``:
    - ``lookup_colleague_inferred_handle`` on a non-exact single match
      so operators can audit which colleague-routing calls were
      inferred.
    - ``lookup_colleague_skipped_ambiguous`` when the resolver had
      matches but couldn't pick one, so 'why didn't this A2A request
      route?' doesn't require log-absence detective work.
    """
    candidates = _resolve_colleague_candidates(query, context)
    safe_query = _safe_for_display(query)
    if len(candidates) != 1:
        if candidates:
            logger.info(
                "lookup_colleague_skipped_ambiguous",
                query=safe_query,
                count=len(candidates),
                handles=[c.handle for c in candidates[:_LOOKUP_DISPLAY_LIMIT]],
            )
        return None
    only = candidates[0]
    if only.method not in _EXACT_METHODS:
        logger.info(
            "lookup_colleague_inferred_handle",
            query=safe_query,
            handle=only.handle,
            method=only.method,
            score=only.score,
        )
    return only.party


def _resolve_colleague_handle(query: str, context: AgentContext) -> str | None:
    """Resolve ``query`` to a single canonical handle when unambiguous.

    Thin string view over :func:`resolve_colleague_party` for callers
    that only need the handle.
    """
    party = resolve_colleague_party(query, context)
    return party.handle if party is not None else None


async def _lookup_colleague(
    params: dict[str, Any], context: AgentContext
) -> ToolResult:
    """Look up a colleague's canonical handle and cross-platform identities.

    Resolves agent seats AND human seats; human results carry contact
    identities, availability, and an interaction note so the caller
    immediately knows the counterparty replies asynchronously.
    """
    query = params.get("query", "").strip()
    if not query:
        return ToolResult(success=False, error="query is required")

    # Distinguish 'engine misconfigured' from 'no match' so operators
    # diagnosing 'No colleague found' don't chase the wrong root cause.
    if context.handle_registry is None:
        return ToolResult(
            success=False,
            error="Handle registry is not configured for this engine.",
        )

    # Sanitised query for any LLM-visible interpolation: collapses
    # internal whitespace (so smuggled \n can't splice phantom rows
    # or break out of the format) and truncates pathological inputs.
    safe_query = _safe_for_display(query)

    candidates = _resolve_colleague_candidates(query, context)
    if not candidates:
        return ToolResult(
            success=False,
            error=f"No colleague found matching '{safe_query}'",
        )

    # Any time more than one distinct seat matched -- including the
    # tier-1 collision case where e.g. a Slack external ID happens to
    # equal another seat's handle -- surface the candidate list so
    # the LLM can pick rather than silently rendering only the first.
    # The list is deduped by handle, so len > 1 always means distinct
    # seats. Each row carries its match method so the LLM knows
    # whether the candidate matched by handle, external ID, role,
    # substring, etc. — without this hint a tier-1 collision (handle
    # 'alice' + Slack ID 'alice' → bob) would loop the LLM when it
    # retries with the same handle.
    if len(candidates) > 1:
        display = candidates[:_LOOKUP_DISPLAY_LIMIT]
        hidden = len(candidates) - len(display)
        lines = [f"Multiple colleagues matched '{safe_query}':"]
        for c in display:
            role_safe = _safe_for_display(c.role_name)
            method_hint = _METHOD_LABELS.get(c.method, c.method.replace("_", " "))
            kind_hint = ", human" if c.kind == "human" else ""
            lines.append(f"  - {c.handle} ({role_safe}{kind_hint}) [via {method_hint}]")
        if hidden:
            lines.append(f"  ...and {hidden} more — narrow your query to see them.")
        lines.append("")
        lines.append(
            "Call lookup_colleague again with one of these handles (or "
            "a more specific identifier) for the full identity block."
        )
        logger.debug(
            "lookup_colleague_disambiguation_surfaced",
            query=safe_query,
            total=len(candidates),
            hidden=hidden,
            handles=[c.handle for c in display],
        )
        return ToolResult(output="\n".join(lines))

    match = candidates[0]
    handle = match.handle
    registry = context.handle_registry

    lines: list[str] = []
    if match.method not in _EXACT_METHODS:
        method_label = _METHOD_LABELS.get(match.method, match.method.replace("_", " "))
        score_suffix = f", score {match.score:.2f}" if match.method == "fuzzy" else ""
        lines.append(
            f"# Inferred match: '{safe_query}' → handle '{handle}' "
            f"({method_label}{score_suffix})"
        )
        lines.append("")
    lines.append(f"handle: {handle}")
    lines.append(f"kind: {match.kind}")

    if match.kind == "human":
        # The candidate carries its resolved party — no re-resolution.
        seat = getattr(match.party, "role", None)
        surfaces: list[str] = []
        if seat is not None:
            lines.append(f"name: {_safe_for_display(seat.name)}")
            # What the person owns — the routing context.  Longer cap
            # than the default: these rows are why the caller looked
            # the person up, and 80 chars truncates real backstories.
            if seat.goal:
                lines.append(f"goal: {_safe_for_display(seat.goal, max_len=200)}")
            if seat.backstory:
                lines.append(
                    f"background: {_safe_for_display(seat.backstory, max_len=200)}"
                )
            if seat.responsibilities:
                lines.append(
                    "responsibilities: "
                    + _safe_for_display("; ".join(seat.responsibilities), max_len=200)
                )
            if seat.email:
                lines.append(f"email: {_safe_for_display(seat.email)}")
            if seat.contact is not None:
                for transport, external_id in seat.contact.resolved_identities():
                    lines.append(f"{transport}_id: {external_id}")
                    surfaces.append(transport)
            if seat.availability:
                lines.append(f"availability: {_safe_for_display(seat.availability)}")
        lines.append("")
        # Name the surfaces THIS seat actually has rather than a fixed
        # stack: an org on Mattermost + Plane has neither Slack nor
        # Confluence, and naming a tool the agent cannot call sends it
        # hunting for one.  The ``<transport>_id:`` rows above are the
        # same list, so the two can never disagree.
        where = (
            f"on {_join_surfaces(surfaces)}"
            if surfaces
            else "on whichever surface the work lives"
        )
        lines.append(
            f"Human teammate — not on the A2A bus. Mention or message them "
            f"{where}, include the context they need, and end your turn; "
            "they reply asynchronously and their reply will re-trigger you."
        )
    else:
        agent = registry.resolve_handle(handle)
        if agent is not None:
            lines.append(f"role: {_safe_for_display(agent.role_name)}")
            if agent.email:
                lines.append(f"email: {_safe_for_display(agent.email)}")

        for transport in registry.all_transports():
            ext_id = registry.get_external_id(transport, handle)
            if ext_id:
                lines.append(f"{transport}_id: {ext_id}")

    # Counterparty profile: if the calling agent has one on this
    # subject, inline the observed traits so the planner can adapt tone
    # without a separate round-trip.  Best-effort — failures are logged
    # and the lookup still returns the identity block.  Human seats
    # accrue profiles too (they're keyed by handle).
    profile_block = await _render_observed_profile(
        context=context, subject_handle=handle
    )
    if profile_block:
        lines.append("")
        lines.append(profile_block)

    return ToolResult(output="\n".join(lines))


async def _render_observed_profile(
    *, context: AgentContext, subject_handle: str
) -> str:
    """Return an "Observed by you:" block for ``subject_handle``, or ''."""
    store = getattr(context, "counterparty_store", None)
    observer = context.agent_handle or ""
    if store is None or not observer or not subject_handle:
        return ""
    if subject_handle == observer:
        # Looking up yourself — no profile to surface.
        return ""
    try:
        profile = await store.fetch(
            observer_handle=observer, subject_handle=subject_handle
        )
    except Exception:
        logger.exception(
            "lookup_colleague_profile_fetch_failed", subject=subject_handle
        )
        return ""
    if profile is None:
        return ""
    return profile.render_observed_traits()


async def _emit_skill_used(
    context: AgentContext,
    *,
    skill_name: str,
    skill_id: str,
    source: str,
    file_loaded: str = "",
) -> None:
    """Best-effort SkillUsed publish.

    Telemetry must never break a skill load -- a publish failure is
    logged once and swallowed.  Skipped silently when no event_queue
    is wired (test mode).
    """
    if context.event_queue is None:
        return
    try:
        await context.event_queue.publish(
            "crewlet.events.skill_used",
            SkillUsed(
                source=context.role,
                agent_id=context.agent_id,
                agent_handle=context.agent_handle,
                role=context.role,
                skill_name=skill_name,
                skill_id=skill_id,
                source_kind=source,
                file_loaded=file_loaded,
            ),
        )
    except Exception:
        logger.exception("skill_used_publish_failed", skill_name=skill_name)


async def _use_skill(params: dict[str, Any], context: AgentContext) -> ToolResult:
    """Load one of the agent's own synthesized skills.

    Surfaced in the Plan prompt's ``## Synthesized skills you've
    learned`` block.  Team-published procedural knowledge is a
    different surface -- see the ``## Relevant knowledge`` Plan-prompt
    block and the role's knowledge-base search tool for that.

    A successful load emits a ``SkillUsed`` event so operators can
    answer the Berlot-Attwell question: do agents actually use the
    skills they're induced?
    """
    skill_name = params.get("skill_name", "").strip()

    if not skill_name:
        return ToolResult(success=False, error="skill_name is required")

    synthesized_store = getattr(context, "synthesized_skill_store", None)
    if synthesized_store is None or not context.agent_handle:
        return ToolResult(
            success=False,
            error=(
                "Synthesized-skill store unavailable for this agent. "
                "For team-published procedural docs, use your "
                "knowledge-base search tool."
            ),
        )

    try:
        synthesized = await synthesized_store.fetch(
            agent_handle=context.agent_handle,
            name=skill_name,
        )
    except Exception as exc:
        logger.exception("synthesized_skill_fetch_failed", name=skill_name)
        return ToolResult(success=False, error=f"skill lookup failed: {exc}")

    if synthesized is None:
        return ToolResult(
            success=False,
            error=(
                f"No synthesized skill named '{skill_name}' for this "
                "agent.  For team-published procedural docs, use your "
                "knowledge-base search tool."
            ),
        )

    # Curator lifecycle — refuse archived skills.  ``fetch`` already filters by
    # default, so reaching this branch means an operator passed
    # ``include_archived=True`` (admin path) or a future caller asks
    # for the row directly.  Either way, the loadable surface should
    # reject archived skills.
    if synthesized.state == "archived":
        return ToolResult(
            success=False,
            error=(
                f"Synthesized skill '{skill_name}' has been archived "
                "by the curator. Ask an operator to restore it "
                "(set state='active') if you still need it."
            ),
        )

    logger.debug(
        "synthesized_skill_loaded",
        agent_handle=context.agent_handle,
        name=skill_name,
        state=synthesized.state,
    )
    try:
        await synthesized_store.mark_used(
            synthesized.id,
            event_queue=context.event_queue,
            skill_name=skill_name,
            agent_handle=context.agent_handle,
        )
    except Exception:
        logger.exception("synthesized_skill_mark_used_failed", name=skill_name)
    await _emit_skill_used(
        context,
        skill_name=skill_name,
        skill_id=str(synthesized.id),
        source="synthesized",
    )
    # Curator lifecycle — surface a "stale" prefix so the agent knows the curator
    # has flagged this skill as aging.  The Plan-phase prefetch also
    # shows the marker; surfacing it again at load time keeps the
    # signal consistent.
    body = synthesized.render_skill_md()
    if synthesized.state == "stale":
        body = (
            "<!-- This skill is marked stale by the curator -- "
            "the curator will archive it if it stays unused. "
            "If it's still useful, mention so to your manager. -->\n\n" + body
        )
    return ToolResult(output=body)


async def _load_tool_skill(params: dict[str, Any], context: AgentContext) -> ToolResult:
    """Return the full body of a tool skill by exact ``key``.

    The Plan / Execute / sub-agent prompts list every triggered skill
    as a one-line summary; this tool fetches the rich body when the
    LLM decides it needs the detail (workflow examples, mention markup,
    handoff conventions, etc.) for a tool it's about to use.
    """
    key = str(params.get("key", "")).strip()
    if not key:
        return ToolResult(success=False, error="`key` is required.")
    registry = getattr(context, "prompt_skill_registry", None)
    if registry is None:
        return ToolResult(
            success=False,
            error="Tool-skill registry is not configured for this engine.",
        )
    skill = registry.get(key)
    if skill is None:
        available = ", ".join(registry.keys()) or "(none)"
        return ToolResult(
            success=False,
            error=(
                f"No tool skill registered with key '{key}'. "
                f"Available keys: {available}"
            ),
        )
    # Render ${var} references so operator-defined facts (e.g. tenant
    # base URLs) reach the LLM substituted, not as literal placeholders.
    title = registry.render(skill.title)
    body = registry.render(skill.body)
    return ToolResult(output=f"# {title}\n\n{body}")


def register_builtin_tools(registry: ToolRegistry) -> None:
    """Register all built-in tools with the registry.

    Task-management tools (create_task, update_task, assign_task,
    list_tasks, delegate) are not registered — agents interact with
    the external PM tool (Jira, Linear, etc.) via MCP tools.
    Manager handoffs when stuck use the colleague-surface tools
    (slack / jira / confluence / a2a).
    """
    tools = [
        SimpleTool(
            name="lookup_colleague",
            description=(
                "Look up a colleague — AI agent or human teammate — by "
                "any identifier: handle, role name, Slack user ID, "
                "Jira account ID, or Confluence account ID. Returns "
                "the canonical handle, the seat kind (agent | human), "
                "and all known cross-platform identities (including "
                "Atlassian account IDs for Confluence mentions); human "
                "results add what they own (goal, background, "
                "responsibilities), availability, and how to reach "
                "them (Slack/Jira mention, asynchronous replies — "
                "never a2a_ask). Matching is case-insensitive and "
                "falls back to substring / fuzzy match, so partial "
                "names like 'ceo' find role 'Agent CEO'; when more "
                "than one colleague matches a fuzzy query, the tool "
                "returns the candidate list instead of guessing. Use "
                "this before a2a_ask or Confluence @mentions."
            ),
            parameters={
                "type": "object",
                "properties": {
                    "query": {
                        "type": "string",
                        "description": (
                            "Any colleague identifier: handle, role "
                            "name (or part of one), Slack user ID, "
                            "Jira account ID, or Confluence account ID"
                        ),
                    },
                },
                "required": ["query"],
            },
            fn=_lookup_colleague,
        ),
        SimpleTool(
            name="use_skill",
            description=(
                "Load one of your synthesized skills (listed in the "
                "`## Synthesized skills you've learned` Plan-prompt "
                "block). For team-published procedural docs, use your "
                "knowledge-base search tool -- those live in the team "
                "knowledge base, not the synthesized-skill store."
            ),
            parameters={
                "type": "object",
                "properties": {
                    "skill_name": {
                        "type": "string",
                        "description": (
                            "Exact name of one of your synthesized "
                            "skills (as shown in the Plan-prompt block)"
                        ),
                    },
                },
                "required": ["skill_name"],
            },
            fn=_use_skill,
        ),
        SimpleTool(
            name="load_tool_skill",
            description=(
                "Load the full body of a tool skill by exact `key` "
                "(as listed in the `## Tool skills` catalogue). Returns "
                "the rich guidance (workflow examples, mention markup, "
                "handoff conventions, etc.) for a specific tool or MCP "
                "server. Call this whenever the catalogue summary is "
                "not enough — e.g. before writing a platform-specific "
                "mention or invoking a tool with subtle workflow "
                "constraints."
            ),
            parameters={
                "type": "object",
                "properties": {
                    "key": {
                        "type": "string",
                        "description": (
                            "Exact skill key from the catalogue, e.g. "
                            "'mcp:github' or 'tool:reflect_and_persist'."
                        ),
                    },
                },
                "required": ["key"],
            },
            fn=_load_tool_skill,
        ),
    ]

    logger.info("builtin_tools_registering")
    for tool in tools:
        logger.debug("builtin_tool_registering", name=tool.name)
        registry.register(tool)
    logger.info("builtin_tools_registered", count=len(tools))
