"""Unit tests for :class:`PersistDecider`.

All tests use a scripted mock LLM provider + a stub diary so no
external services are required.
"""

from __future__ import annotations

import json
from datetime import datetime
from typing import Any

from crewlet.learning.persist_decider import (
    _DECIDER_SYSTEM_PROMPT,
    KIND_LONG,
    KIND_SHORT,
    NOOP_SENTINEL,
    DirectiveObservation,
    PersistDecider,
    _build_user_prompt,
    _extract_json_object,
)
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import Completion


class _ScriptedProvider:
    """Returns canned completions in order; records sent messages."""

    def __init__(self, completions: list[Completion]) -> None:
        self._completions = list(completions)
        self.calls: list[dict[str, Any]] = []
        self.model = "scripted"

    async def complete(
        self,
        messages,
        tools=None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice=None,
    ):
        self.calls.append(
            {"messages": messages, "temperature": temperature, "max_tokens": max_tokens}
        )
        if self._completions:
            return self._completions.pop(0)
        return Completion(content="NOOP")


class _DiaryStub:
    """Stand-in for :class:`~crewlet.learning.diary.AgentDiary`.

    Records every write call and returns canned existing-memories
    rows from ``list_for_agent``.  Mirrors the diary's signature so
    PersistDecider can call through unmodified.
    """

    def __init__(
        self,
        existing_docs: list[dict[str, Any]] | None = None,
        *,
        list_raises: Exception | None = None,
    ) -> None:
        self.writes: list[dict[str, Any]] = []
        self._existing_docs = list(existing_docs or [])
        self._list_raises = list_raises
        self.list_calls: list[tuple[str, bool]] = []

    async def write(
        self,
        *,
        agent_id: str,
        kind: str,
        content: str,
        ttl_until=None,
        source: str = "",
        turn_id: str = "",
        metadata: dict[str, Any] | None = None,
    ) -> str:
        self.writes.append(
            {
                "agent_id": agent_id,
                "kind": kind,
                "content": content,
                "ttl_until": ttl_until,
                "source": source,
                "turn_id": turn_id,
                "metadata": metadata or {},
            }
        )
        return "doc-1"

    async def list_for_agent(
        self,
        agent_id: str,
        *,
        limit: int = 100,
        include_expired: bool = False,
    ) -> list[dict[str, Any]]:
        # Dedup pre-flight: PersistDecider always queries
        # ``list_for_agent`` for the agent's existing LONG/SHORT
        # diary entries before classifying.  Default returns nothing
        # so tests written before dedup landed keep working unchanged.
        self.list_calls.append((agent_id, include_expired))
        if self._list_raises is not None:
            # Mirrors the real diary's "swallow DB failures, return []"
            # contract -- the decider's robustness depends on the
            # diary absorbing transient backend failures rather than
            # propagating them up.
            return []
        from datetime import UTC
        from datetime import datetime as _dt

        out: list[dict[str, Any]] = []
        now = _dt.now(UTC)
        for doc in self._existing_docs:
            if not include_expired:
                meta = doc.get("metadata") or {}
                ttl = meta.get("ttl_until") if isinstance(meta, dict) else None
                if ttl:
                    try:
                        expiry = _dt.fromisoformat(ttl)
                    except ValueError:
                        expiry = None
                    if expiry is not None and expiry < now:
                        continue
            out.append(doc)
        return out


def _mk_role(
    *, llm: str = "default", llm_auxiliary: str | None = None, name: str = "Engineer"
) -> Role:
    return Role(name=name, handle="eng", llm=llm, llm_auxiliary=llm_auxiliary)


def _decider(
    completions: list[Completion],
    *,
    budget: int = 1000,
    on_doc_observed: Any = None,
    existing_docs: list[dict[str, Any]] | None = None,
    list_raises: Exception | None = None,
) -> tuple[PersistDecider, _ScriptedProvider, _DiaryStub]:
    provider = _ScriptedProvider(completions)
    diary = _DiaryStub(existing_docs=existing_docs, list_raises=list_raises)
    decider = PersistDecider(
        llm_providers={"default": provider, "cheap": provider},
        diary=diary,  # type: ignore[arg-type]
        budget_tokens=budget,
        on_doc_observed=on_doc_observed,
    )
    return decider, provider, diary


async def test_noop_response_results_in_no_write() -> None:
    decider, provider, diary = _decider([Completion(content="NOOP")])
    result = await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t1",
        task_summary="a thing",
        plan_summary="do the thing",
        tool_sequence=[],
        review_outcome="done",
    )
    assert result.kind == "NOOP"
    assert result.result is None
    assert not result.persisted
    assert diary.writes == []
    assert len(provider.calls) == 1
    # System prompt enforces the declarative-facts rule + names the
    # four classification tiers.
    system_message = provider.calls[0]["messages"][0]
    assert system_message.role == "system"
    assert "Declarative facts, not instructions" in system_message.content
    assert NOOP_SENTINEL in _DECIDER_SYSTEM_PROMPT
    assert "DOC" in _DECIDER_SYSTEM_PROMPT
    assert "LONG" in _DECIDER_SYSTEM_PROMPT
    assert "SHORT" in _DECIDER_SYSTEM_PROMPT


async def test_long_classification_writes_at_agent_scope_with_metadata() -> None:
    decider, _, diary = _decider(
        [
            Completion(
                content=json.dumps(
                    {
                        "kind": "LONG",
                        "content": "User prefers weekly digests.",
                    }
                )
            )
        ]
    )
    result = await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t1",
        task_summary="Help with reporting",
        plan_summary="Summarise",
        tool_sequence=["search"],
        review_outcome="done",
    )
    assert result.kind == "LONG"
    assert result.persisted
    assert result.result is not None
    assert result.result.doc_id == "doc-1"
    assert result.result.scope == "agent"
    assert result.result.classification == KIND_LONG
    assert result.result.ttl_until == ""  # LONG entries never expire
    assert len(diary.writes) == 1
    write = diary.writes[0]
    assert write["agent_id"] == "alice-id"
    assert write["content"] == "User prefers weekly digests."
    assert write["kind"] == KIND_LONG
    assert write["source"] == "persist_decider"
    assert write["turn_id"] == "t1"
    assert write["metadata"]["review_outcome"] == "done"
    assert write["ttl_until"] is None


async def test_short_classification_writes_with_ttl_until() -> None:
    decider, _, diary = _decider(
        [
            Completion(
                content=json.dumps(
                    {
                        "kind": "SHORT",
                        "content": "Sarah is OOO until Friday.",
                        "ttl_days": 7,
                    }
                )
            )
        ]
    )
    result = await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t2",
        task_summary="",
        plan_summary="",
        tool_sequence=[],
        review_outcome="done",
    )
    assert result.kind == "SHORT"
    assert result.persisted
    assert result.result is not None
    assert result.result.classification == KIND_SHORT
    assert result.result.ttl_until  # ISO timestamp in the future
    write = diary.writes[0]
    assert write["kind"] == KIND_SHORT
    assert write["ttl_until"] is not None
    # Roughly 7 days out, leave slack for clock granularity.
    expiry = write["ttl_until"]
    delta = expiry - datetime.now(expiry.tzinfo)
    assert 6 <= delta.days <= 7


async def test_short_with_invalid_ttl_uses_default() -> None:
    decider, _, diary = _decider(
        [
            Completion(
                content=json.dumps(
                    {
                        "kind": "SHORT",
                        "content": "Op context.",
                        "ttl_days": "not-a-number",
                    }
                )
            )
        ]
    )
    await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t3",
        task_summary="",
        plan_summary="",
        tool_sequence=[],
        review_outcome="done",
    )
    expiry = diary.writes[0]["ttl_until"]
    delta = expiry - datetime.now(expiry.tzinfo)
    # Default is 30 days.
    assert 29 <= delta.days <= 30


async def test_short_with_excessive_ttl_clamped_to_max() -> None:
    decider, _, diary = _decider(
        [
            Completion(
                content=json.dumps(
                    {
                        "kind": "SHORT",
                        "content": "Op context.",
                        "ttl_days": 9999,
                    }
                )
            )
        ]
    )
    await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t4",
        task_summary="",
        plan_summary="",
        tool_sequence=[],
        review_outcome="done",
    )
    expiry = diary.writes[0]["ttl_until"]
    delta = expiry - datetime.now(expiry.tzinfo)
    # Capped at 180 days.
    assert 179 <= delta.days <= 180


async def test_doc_classification_does_not_write_but_emits_observation() -> None:
    observed: list[DirectiveObservation] = []

    async def _record(obs: DirectiveObservation) -> None:
        observed.append(obs)

    decider, _, diary = _decider(
        [
            Completion(
                content=json.dumps(
                    {
                        "kind": "DOC",
                        "content": "Use semantic commit messages.",
                        "target_hint": "Engineering / commit conventions",
                        "rationale": "Imperative team-wide rule",
                    }
                )
            )
        ],
        on_doc_observed=_record,
    )
    result = await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t5",
        task_summary="",
        plan_summary="",
        tool_sequence=[],
        review_outcome="done",
    )
    assert result.kind == "DOC"  # observed but not persisted
    assert result.result is None
    assert not result.persisted
    assert diary.writes == []  # belongs in docs, not memory
    assert len(observed) == 1
    obs = observed[0]
    assert obs.content == "Use semantic commit messages."
    assert obs.target_hint == "Engineering / commit conventions"
    assert "Imperative" in obs.rationale


async def test_org_scope_rejected_no_write() -> None:
    decider, _, diary = _decider(
        [Completion(content=json.dumps({"scope": "org", "content": "Anything."}))]
    )
    result = await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t3",
        task_summary="",
        plan_summary="",
        tool_sequence=[],
        review_outcome="done",
    )
    assert not result.persisted
    assert result.kind == "NOOP"
    assert diary.writes == []


async def test_unparseable_response_results_in_no_write() -> None:
    decider, _, diary = _decider(
        [Completion(content="I would say maybe do something like...")]
    )
    result = await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t4",
        task_summary="",
        plan_summary="",
        tool_sequence=[],
        review_outcome="done",
    )
    assert not result.persisted
    assert result.kind == "NOOP"
    assert diary.writes == []


async def test_llm_exception_swallowed_no_write() -> None:
    class _Boom:
        model = "boom"

        async def complete(self, *args, **kwargs):
            raise RuntimeError("network down")

    diary = _DiaryStub()
    decider = PersistDecider(
        llm_providers={"default": _Boom()},
        diary=diary,  # type: ignore[arg-type]
        budget_tokens=1000,
    )
    result = await decider.decide_and_persist(
        role=_mk_role(),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t5",
        task_summary="",
        plan_summary="",
        tool_sequence=[],
        review_outcome="done",
    )
    assert not result.persisted
    assert result.kind == "NOOP"
    assert diary.writes == []


def test_extract_json_object_handles_prose_wrapping() -> None:
    text = 'Sure thing! {"scope": "agent", "content": "foo"} done.'
    obj = _extract_json_object(text)
    assert obj == {"scope": "agent", "content": "foo"}


def test_extract_json_object_returns_none_for_non_object() -> None:
    assert _extract_json_object("just some text") is None
    assert _extract_json_object("") is None


async def test_empty_content_results_in_no_write() -> None:
    decider, _, diary = _decider(
        [Completion(content=json.dumps({"scope": "agent", "content": ""}))]
    )
    result = await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t6",
        task_summary="",
        plan_summary="",
        tool_sequence=[],
        review_outcome="done",
    )
    assert not result.persisted
    assert result.kind == "NOOP"
    assert diary.writes == []


async def test_budget_tokens_forwarded_to_provider() -> None:
    decider, provider, _ = _decider([Completion(content="NOOP")], budget=1234)
    await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t7",
        task_summary="",
        plan_summary="",
        tool_sequence=[],
        review_outcome="done",
    )
    assert provider.calls[0]["max_tokens"] == 1234


def test_decider_system_prompt_requires_attribution() -> None:
    """The system prompt must steer the LLM toward attributing facts
    to the named requester — without this, persisted facts lose
    meaning in a multi-party org."""
    assert "Always attribute" in _DECIDER_SYSTEM_PROMPT
    assert "multi-party" in _DECIDER_SYSTEM_PROMPT


def test_user_prompt_renders_requester_block_when_sender_known() -> None:
    from crewlet.learning.interaction import CanonicalIdentity, InboundInteraction

    rendered = _build_user_prompt(
        task_summary="reply to slack",
        plan_summary="reply",
        tool_sequence=["slack_message"],
        review_outcome="done",
        interactions=[
            InboundInteraction(
                sender=CanonicalIdentity(
                    handle="sam",
                    external_id="U0TESTUSER1",
                    platform="slack",
                    display_name="Sam",
                ),
                body="please always greet me with hey sam",
            )
        ],
    )
    assert "- Requester: Sam (slack:U0TESTUSER1)" in rendered
    assert "always greet me with hey sam" in rendered


def test_user_prompt_renders_each_coalesced_message() -> None:
    """A coalesced trigger renders one requester/message pair per
    constituent so the classifier sees who asked what — not just the
    last ping."""
    from crewlet.learning.interaction import CanonicalIdentity, InboundInteraction

    rendered = _build_user_prompt(
        task_summary="reply to slack thread",
        plan_summary="reply",
        tool_sequence=["slack_message"],
        review_outcome="done",
        interactions=[
            InboundInteraction(
                sender=CanonicalIdentity(
                    external_id="U-A", platform="slack", display_name="Alice"
                ),
                body="please use the staging cluster",
            ),
            InboundInteraction(
                sender=CanonicalIdentity(
                    external_id="U-B", platform="slack", display_name="Bob"
                ),
                body="and tag me on the rollout ticket",
            ),
        ],
    )
    assert "- Requester: Alice (slack:U-A)" in rendered
    assert "- Requester: Bob (slack:U-B)" in rendered
    assert "staging cluster" in rendered
    assert "rollout ticket" in rendered


def test_user_prompt_omits_sender_block_when_no_sender() -> None:
    """Internal triggers (TaskAssigned etc.) have no sender — the
    prompt must remain compact so the decider doesn't see a stray
    'Requester: (unknown)' line."""
    rendered = _build_user_prompt(
        task_summary="run periodic check",
        plan_summary="check",
        tool_sequence=[],
        review_outcome="done",
    )
    assert "Requester" not in rendered
    assert "Inbound message" not in rendered


# ---------------------------------------------------------------------
# Dedup against existing personal memory
# ---------------------------------------------------------------------


def _memory_doc(
    content: str,
    *,
    kind: str = KIND_SHORT,
    ttl_until: str | None = None,
) -> dict[str, Any]:
    """Construct a doc dict shaped like an ``AgentDiary`` memory row."""
    meta: dict[str, Any] = {"kind": kind}
    if ttl_until is not None:
        meta["ttl_until"] = ttl_until
    return {"id": "existing", "content": content, "metadata": meta}


def test_user_prompt_renders_existing_memory_block_when_present() -> None:
    """The dedup signal must reach the LLM as a numbered list with a
    stable header, so the system-prompt rule can refer to it by name."""
    rendered = _build_user_prompt(
        task_summary="route reviews",
        plan_summary="ask Maria",
        tool_sequence=["query_knowledge"],
        review_outcome="done",
        existing_memories=[
            _memory_doc(
                "User Sam is OOO Mon-Fri this week, route backend reviews to Maria",
                kind=KIND_SHORT,
                ttl_until="2026-05-09T00:00:00+00:00",
            ),
            _memory_doc("Stakeholder Sam prefers terse replies", kind=KIND_LONG),
        ],
    )
    assert "Already in your memory:" in rendered
    assert "0. User Sam is OOO Mon-Fri" in rendered
    assert "(until 2026-05-09T00:00:00+00:00)" in rendered  # SHORT carries TTL
    assert "1. Stakeholder Sam prefers terse replies" in rendered
    # Block sits before the trailing classify directive.
    memory_idx = rendered.index("Already in your memory:")
    classify_idx = rendered.index("Classify and emit")
    assert memory_idx < classify_idx


def test_user_prompt_omits_memory_block_when_no_existing_memories() -> None:
    """Fresh agents (or fetch failures returning ``[]``) must not see
    a bare header — the prompt should look identical to the pre-dedup
    shape so the LLM's classification behaviour doesn't shift on
    empty input."""
    rendered = _build_user_prompt(
        task_summary="something",
        plan_summary="plan",
        tool_sequence=[],
        review_outcome="done",
        existing_memories=[],
    )
    assert "Already in your memory" not in rendered


def test_user_prompt_truncates_overlong_memory_entry() -> None:
    """A pathologically long entry must not crowd out other memories
    in the dedup block."""
    long_content = "x" * 5000
    rendered = _build_user_prompt(
        task_summary="t",
        plan_summary="p",
        tool_sequence=[],
        review_outcome="done",
        existing_memories=[_memory_doc(long_content, kind=KIND_LONG)],
    )
    assert "..." in rendered
    # Truncated to the documented cap; far smaller than 5000 chars.
    assert len(rendered) < 1500


def test_user_prompt_skips_memory_with_empty_content() -> None:
    """An entry whose content is empty / whitespace must not render
    as a bare ``2. `` line — and if every entry is empty, the block
    should disappear entirely."""
    rendered = _build_user_prompt(
        task_summary="t",
        plan_summary="p",
        tool_sequence=[],
        review_outcome="done",
        existing_memories=[
            _memory_doc("", kind=KIND_LONG),
            _memory_doc("   ", kind=KIND_LONG),
        ],
    )
    assert "Already in your memory" not in rendered


def test_decider_system_prompt_includes_dedup_rule() -> None:
    """The dedup rule is what makes the new block useful — without it,
    the LLM has the data but no instruction to act on it."""
    assert "Deduplicate against existing memory" in _DECIDER_SYSTEM_PROMPT
    assert "restates an existing entry" in _DECIDER_SYSTEM_PROMPT
    # Dedup is scoped to LONG/SHORT — DOC must remain independent.
    assert "does not apply to ``DOC``" in _DECIDER_SYSTEM_PROMPT


async def test_decider_loads_existing_memories_into_user_prompt() -> None:
    """End-to-end: the decider must list existing AGENT-scope memories
    and inject them into the user prompt before classifying."""
    from datetime import UTC, datetime, timedelta

    future_ttl = (datetime.now(UTC) + timedelta(days=7)).isoformat()
    existing = [
        _memory_doc(
            "User Sam is OOO Mon-Fri this week, route backend reviews to Maria",
            kind=KIND_SHORT,
            ttl_until=future_ttl,
        )
    ]
    decider, provider, diary = _decider(
        [Completion(content="NOOP")], existing_docs=existing
    )
    await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t-dedup-1",
        task_summary="route a backend review",
        plan_summary="Since Sam is OOO this week, ask Maria",
        tool_sequence=["query_knowledge"],
        review_outcome="done",
    )
    # The decider asked the diary for the agent's existing
    # memories — scoped by agent_id, expired entries excluded.
    assert diary.list_calls == [("alice-id", False)]
    # And those memories made it into the user prompt verbatim.
    user_message = provider.calls[0]["messages"][1]
    assert user_message.role == "user"
    assert "Already in your memory:" in user_message.content
    assert "User Sam is OOO Mon-Fri" in user_message.content


async def test_decider_returns_noop_when_llm_dedups_against_existing() -> None:
    """The agent already has
    the OOO fact; the decider's LLM, seeing it in the dedup block,
    must be free to return NOOP and not double-write."""
    existing = [
        _memory_doc(
            "User Sam is OOO Mon-Fri this week, route backend reviews to Maria",
            kind=KIND_SHORT,
            ttl_until="2026-05-09T00:00:00+00:00",
        )
    ]
    # Simulate a well-behaved LLM that recognises the duplicate.
    decider, _, diary = _decider([Completion(content="NOOP")], existing_docs=existing)
    result = await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t-dedup-2",
        task_summary="route a backend review",
        plan_summary="Since Sam is OOO this week, ask Maria",
        tool_sequence=[],
        review_outcome="done",
    )
    assert not result.persisted
    assert result.kind == "NOOP"
    assert diary.writes == []  # no duplicate row created


async def test_decider_writes_when_no_overlap_with_existing_memory() -> None:
    """Existing memories about Sam's OOO must not block writes of
    unrelated facts — dedup is per-fact, not a blanket veto."""
    existing = [
        _memory_doc(
            "User Sam is OOO this week",
            kind=KIND_SHORT,
            ttl_until="2026-05-09T00:00:00+00:00",
        )
    ]
    decider, _, diary = _decider(
        [
            Completion(
                content=json.dumps(
                    {
                        "kind": "LONG",
                        "content": "Stakeholder Miles prefers terse replies.",
                    }
                )
            )
        ],
        existing_docs=existing,
    )
    result = await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t-dedup-3",
        task_summary="reply to Miles",
        plan_summary="reply terse",
        tool_sequence=["slack_message"],
        review_outcome="done",
    )
    assert result.persisted
    assert result.kind == "LONG"
    assert result.result is not None
    assert result.result.classification == KIND_LONG
    assert len(diary.writes) == 1
    assert diary.writes[0]["content"] == "Stakeholder Miles prefers terse replies."


async def test_decider_falls_back_when_existing_memory_fetch_fails() -> None:
    """A broken knowledge backend on the read side must not break the
    write side — the decider should classify with no dedup block
    rather than skip the turn entirely."""
    decider, provider, diary = _decider(
        [
            Completion(
                content=json.dumps(
                    {
                        "kind": "LONG",
                        "content": "User prefers weekly digests.",
                    }
                )
            )
        ],
        list_raises=RuntimeError("pgvector down"),
    )
    result = await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t-dedup-4",
        task_summary="t",
        plan_summary="p",
        tool_sequence=[],
        review_outcome="done",
    )
    assert result.persisted
    assert result.kind == "LONG"
    assert len(diary.writes) == 1
    user_message = provider.calls[0]["messages"][1]
    # No dedup block — the prompt looks like the pre-dedup shape so the
    # LLM still classifies normally.
    assert "Already in your memory" not in user_message.content


async def test_decider_skips_expired_short_memories_in_dedup_block() -> None:
    """Expired SHORT entries are stale — they must not block fresh
    versions of the same fact from being persisted."""
    existing = [
        _memory_doc(
            "User Sam is OOO this week",
            kind=KIND_SHORT,
            # Set well in the past so ``_select_memory_candidates``
            # filters it out before it reaches the dedup block.
            ttl_until="2020-01-01T00:00:00+00:00",
        )
    ]
    decider, provider, _ = _decider(
        [Completion(content="NOOP")], existing_docs=existing
    )
    await decider.decide_and_persist(
        role=_mk_role(llm_auxiliary="cheap"),
        agent_id="alice-id",
        agent_handle="alice",
        unit_id="Eng",
        org_id="Acme",
        turn_id="t-dedup-5",
        task_summary="t",
        plan_summary="p",
        tool_sequence=[],
        review_outcome="done",
    )
    user_message = provider.calls[0]["messages"][1]
    # Expired entry filtered out → no dedup block at all (since it
    # was the only existing memory).
    assert "Already in your memory" not in user_message.content
