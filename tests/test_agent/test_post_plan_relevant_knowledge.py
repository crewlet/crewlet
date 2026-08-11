"""Tests for the post-Plan relevant-knowledge re-fetch.

On a thin-trigger turn the Plan-phase ``## Relevant knowledge``
prefetch is gated off -- searching a bare pointer is noise.
:func:`crewlet.agent.plan.fetch_post_plan_relevant_knowledge` recovers
the gap: once Plan has done its recon it re-runs the fetch keyed on the
plan summary and hands the result to the Execute phase, emitting a
``RelevantKnowledgeRefetched`` telemetry event.

Covers:

- fires only on a thin trigger + ``plan`` / ``direct`` decision + a
  non-empty plan summary;
- the aux query-generation call is fed the plan summary, not the
  (boilerplate) task description;
- forces the thin-trigger gate OFF via ``query_override``;
- emits ``RelevantKnowledgeRefetched`` describing the produced block;
- short-circuits (no search, no event) for non-thin triggers and
  the ``skip`` decision.
"""

from __future__ import annotations

from types import SimpleNamespace
from typing import Any
from uuid import uuid4

import pytest

from crewlet.agent.plan import (
    ExecutionPlan,
    Step,
    _count_relevant_knowledge_picks,
    _fetch_personal_memory_block_wrapper,
    _fetch_relevant_knowledge_block,
    fetch_post_plan_relevant_knowledge,
)
from crewlet.events.types import ExternalNotification, RelevantKnowledgeRefetched
from crewlet.knowledge.protocol import KnowledgeHit
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion

# ---------------------------------------------------------------------------
# Stubs
# ---------------------------------------------------------------------------


class _EventQueueStub:
    def __init__(self) -> None:
        self.published: list[tuple[str, Any]] = []

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


class _ScriptedProvider:
    """LLM stub returning queued :class:`Completion` objects.

    Used as the auxiliary model: the re-fetch makes one call to turn
    the plan summary into a knowledge-base search query.
    """

    def __init__(self, completions: list[Completion]) -> None:
        self._completions = list(completions)
        self.calls: list[dict[str, Any]] = []
        self.model = "scripted"

    async def complete(
        self,
        messages,
        tools=None,
        temperature=0.7,
        max_tokens=None,
        tool_choice=None,
    ):
        self.calls.append({"messages": messages})
        if self._completions:
            return self._completions.pop(0)
        return Completion(content="")


class _SearcherStub:
    """Protocol-shaped stand-in for a ``KnowledgeSearcher`` backend.

    Records every ``search`` call so tests can assert the aux-generated
    query reached it.
    """

    def __init__(self, results: list[KnowledgeHit]) -> None:
        self.results = list(results)
        self.calls: list[dict[str, Any]] = []

    def can_search(self, *, role: Any, org: Any) -> bool:
        return True

    async def search(
        self,
        *,
        query: str,
        role: Any,
        org: Any,
        limit: int = 8,
        exclude_ancestors: list[str] | None = None,
    ) -> list[KnowledgeHit]:
        self.calls.append({"query": query, "role": role, "org": org})
        return list(self.results)


def _hit(*, title: str, snippet: str) -> KnowledgeHit:
    return KnowledgeHit(
        title=title,
        url=f"https://wiki.example/{title}",
        container="ENG",
        page_id=title,
        snippet=snippet,
    )


def _user_prompt(call: dict[str, Any]) -> str:
    """The last user-role message content from a recorded LLM call."""
    for m in reversed(call["messages"]):
        if m.role == "user":
            return m.content
    return ""


def _role() -> Role:
    return Role(
        name="Engineer",
        handle="eng",
        llm="cheap",
        llm_auxiliary="cheap",
    )


def _org_with_role(role: Role) -> Organization:
    # Read scope is the org-wide list now; the unit's space identity no
    # longer narrows reads.
    return Organization(
        name="Acme",
        confluence_spaces=["ENG"],
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                lead=role.name,
                roles=[role],
                confluence_space="ENG",
            )
        ],
    )


def _mk_turn(*, trigger_event: Any, task_description: str = "task boilerplate") -> Any:
    from crewlet.learning.interaction import InboundInteraction

    return SimpleNamespace(
        agent=SimpleNamespace(
            handle="eng",
            id=uuid4(),
            definition=SimpleNamespace(role=SimpleNamespace(name="Engineer")),
        ),
        turn_id=uuid4(),
        iteration=1,
        task_description=task_description,
        trigger_event=trigger_event,
        trigger_interactions=lambda: InboundInteraction.list_from_trigger_event(
            trigger_event
        ),
    )


def _mk_ctx(*, org: Organization, searcher: Any, queue: Any) -> Any:
    return SimpleNamespace(
        org=org,
        knowledge_searcher=searcher,
        role="Engineer",
        agent_id="eng-id",
        event_queue=queue,
    )


def _thin_trigger() -> ExternalNotification:
    """A Jira webhook -- ``context_requires_recon=True`` -> the
    InboundInteraction is recon-required (a bare pointer)."""
    return ExternalNotification(
        notification_source="jira",
        sender="Manager",
        body="A Jira event has been routed to you.",
        metadata={"issue_key": "POC-528"},
        context_requires_recon=True,
    )


def _rich_trigger() -> ExternalNotification:
    """A self-contained Slack message -- not recon-required."""
    return ExternalNotification(
        notification_source="slack",
        sender="U123",
        body="full self-contained question about the latency dashboard",
        metadata={"slack_user_id": "U123"},
        context_requires_recon=False,
    )


# ---------------------------------------------------------------------------
# _count_relevant_knowledge_picks
# ---------------------------------------------------------------------------


def test_count_picks_counts_bullet_markers() -> None:
    block = "- **Doc A**: x\n- **Doc B**: y\nfooter line"
    assert _count_relevant_knowledge_picks(block) == 2


def test_count_picks_zero_for_hint_and_empty() -> None:
    assert _count_relevant_knowledge_picks("") == 0
    assert _count_relevant_knowledge_picks("(no team documents matched ...)") == 0


# ---------------------------------------------------------------------------
# fetch_post_plan_relevant_knowledge -- active path
# ---------------------------------------------------------------------------


async def test_refetch_fires_on_thin_trigger_plan_decision() -> None:
    role = _role()
    org = _org_with_role(role)
    searcher = _SearcherStub([_hit(title="Incident Runbook", snippet="Sev-1 steps.")])
    queue = _EventQueueStub()
    provider = _ScriptedProvider([Completion(content="incident runbook")])
    turn = _mk_turn(trigger_event=_thin_trigger())
    ctx = _mk_ctx(org=org, searcher=searcher, queue=queue)
    plan = ExecutionPlan(
        decision="plan",
        steps=[Step(intent="Investigate the latency spike on the orders service")],
    )
    out = await fetch_post_plan_relevant_knowledge(
        turn=turn, agent_context=ctx, plan=plan, llm_providers={"cheap": provider}
    )
    assert "Incident Runbook" in out
    # The aux query-generation call ran (the gate was forced off) ...
    assert len(provider.calls) == 1
    # ... and the generated query reached the searcher.
    assert len(searcher.calls) == 1


async def test_refetch_keys_query_on_plan_summary_not_task() -> None:
    """The aux query-generation call must be fed the plan summary --
    the recon-informed text -- not the thin task description."""
    role = _role()
    org = _org_with_role(role)
    searcher = _SearcherStub([_hit(title="Doc A", snippet="...")])
    queue = _EventQueueStub()
    provider = _ScriptedProvider([Completion(content="payments regression")])
    turn = _mk_turn(
        trigger_event=_thin_trigger(),
        task_description="A Jira event has been routed to you.",
    )
    ctx = _mk_ctx(org=org, searcher=searcher, queue=queue)
    plan = ExecutionPlan(
        decision="plan",
        steps=[Step(intent="Triage the POC-528 payments regression")],
    )
    await fetch_post_plan_relevant_knowledge(
        turn=turn, agent_context=ctx, plan=plan, llm_providers={"cheap": provider}
    )
    aux_prompt = _user_prompt(provider.calls[0])
    assert "payments regression" in aux_prompt
    assert "A Jira event has been routed" not in aux_prompt


async def test_refetch_emits_relevant_knowledge_refetched_event() -> None:
    role = _role()
    org = _org_with_role(role)
    searcher = _SearcherStub([_hit(title="Doc A", snippet="First sentence.")])
    queue = _EventQueueStub()
    provider = _ScriptedProvider([Completion(content="ticket triage")])
    turn = _mk_turn(trigger_event=_thin_trigger())
    turn.iteration = 2
    ctx = _mk_ctx(org=org, searcher=searcher, queue=queue)
    plan = ExecutionPlan(
        decision="direct", reasoning="Handle the POC-528 ticket directly."
    )
    out = await fetch_post_plan_relevant_knowledge(
        turn=turn, agent_context=ctx, plan=plan, llm_providers={"cheap": provider}
    )
    events = [
        e
        for t, e in queue.published
        if t == "crewlet.events.relevant_knowledge_refetched"
    ]
    assert len(events) == 1
    ev = events[0]
    assert isinstance(ev, RelevantKnowledgeRefetched)
    assert ev.plan_decision == "direct"
    assert ev.iteration == 2
    assert ev.selection_count == 1
    assert ev.block_bytes == len(out)
    assert ev.turn_id == str(turn.turn_id)
    assert ev.agent_handle == "eng"


async def test_refetch_emits_event_even_when_search_finds_nothing() -> None:
    """The event fires whenever the re-fetch path is *taken* -- even an
    empty result is useful telemetry ("we tried, found nothing")."""
    role = _role()
    org = _org_with_role(role)
    searcher = _SearcherStub([])  # search returns nothing -> EMPTY_FILTER_HINT
    queue = _EventQueueStub()
    provider = _ScriptedProvider([Completion(content="query")])
    turn = _mk_turn(trigger_event=_thin_trigger())
    ctx = _mk_ctx(org=org, searcher=searcher, queue=queue)
    plan = ExecutionPlan(decision="plan", steps=[Step(intent="do the thing")])
    await fetch_post_plan_relevant_knowledge(
        turn=turn, agent_context=ctx, plan=plan, llm_providers={"cheap": provider}
    )
    events = [
        e
        for t, e in queue.published
        if t == "crewlet.events.relevant_knowledge_refetched"
    ]
    assert len(events) == 1
    assert events[0].selection_count == 0  # hint rendered, no real picks


async def test_refetch_swallows_unexpected_failure(monkeypatch) -> None:
    """A prefetch must never break the turn: an unexpected raise inside
    the re-fetch degrades to "" (logged), not a propagated exception."""

    async def _boom(**kwargs: Any) -> str:
        raise RuntimeError("search exploded")

    monkeypatch.setattr("crewlet.agent.plan._fetch_relevant_knowledge_block", _boom)
    role = _role()
    org = _org_with_role(role)
    queue = _EventQueueStub()
    turn = _mk_turn(trigger_event=_thin_trigger())
    ctx = _mk_ctx(org=org, searcher=_SearcherStub([]), queue=queue)
    plan = ExecutionPlan(decision="plan", steps=[Step(intent="do the thing")])
    out = await fetch_post_plan_relevant_knowledge(
        turn=turn, agent_context=ctx, plan=plan, llm_providers=None
    )
    assert out == ""


# ---------------------------------------------------------------------------
# fetch_post_plan_relevant_knowledge -- short-circuits
# ---------------------------------------------------------------------------


async def test_refetch_skipped_on_non_thin_trigger() -> None:
    """A self-contained trigger means the Plan-time prefetch already
    ran against the real trigger -- nothing for Execute to recover."""
    role = _role()
    org = _org_with_role(role)
    searcher = _SearcherStub([_hit(title="Doc A", snippet="...")])
    queue = _EventQueueStub()
    provider = _ScriptedProvider([Completion(content="query")])
    turn = _mk_turn(trigger_event=_rich_trigger())
    ctx = _mk_ctx(org=org, searcher=searcher, queue=queue)
    plan = ExecutionPlan(decision="plan", steps=[Step(intent="do the thing")])
    out = await fetch_post_plan_relevant_knowledge(
        turn=turn, agent_context=ctx, plan=plan, llm_providers={"cheap": provider}
    )
    assert out == ""
    assert searcher.calls == []
    assert provider.calls == []
    assert queue.published == []


async def test_refetch_skipped_when_no_trigger_event() -> None:
    role = _role()
    org = _org_with_role(role)
    searcher = _SearcherStub([_hit(title="Doc A", snippet="...")])
    queue = _EventQueueStub()
    turn = _mk_turn(trigger_event=None)
    ctx = _mk_ctx(org=org, searcher=searcher, queue=queue)
    plan = ExecutionPlan(decision="plan", steps=[Step(intent="do the thing")])
    out = await fetch_post_plan_relevant_knowledge(
        turn=turn,
        agent_context=ctx,
        plan=plan,
        llm_providers={"cheap": _ScriptedProvider([])},
    )
    assert out == ""
    assert searcher.calls == []
    assert queue.published == []


@pytest.mark.parametrize("decision", ["skip"])
async def test_refetch_skipped_for_non_execute_decisions(decision: str) -> None:
    """``skip`` never reaches Execute, so there is no prompt to enrich
    -- the re-fetch short-circuits before any work."""
    role = _role()
    org = _org_with_role(role)
    searcher = _SearcherStub([_hit(title="Doc A", snippet="...")])
    queue = _EventQueueStub()
    turn = _mk_turn(trigger_event=_thin_trigger())
    ctx = _mk_ctx(org=org, searcher=searcher, queue=queue)
    plan = ExecutionPlan(decision=decision, reasoning="not acting")
    out = await fetch_post_plan_relevant_knowledge(
        turn=turn,
        agent_context=ctx,
        plan=plan,
        llm_providers={"cheap": _ScriptedProvider([])},
    )
    assert out == ""
    assert searcher.calls == []
    assert queue.published == []


async def test_refetch_skipped_when_plan_summary_blank() -> None:
    """A ``plan`` decision with no steps and no reasoning renders an
    empty ``summary()`` -- there is no real query to re-fetch against."""
    role = _role()
    org = _org_with_role(role)
    searcher = _SearcherStub([_hit(title="Doc A", snippet="...")])
    queue = _EventQueueStub()
    turn = _mk_turn(trigger_event=_thin_trigger())
    ctx = _mk_ctx(org=org, searcher=searcher, queue=queue)
    plan = ExecutionPlan(decision="plan", reasoning="")  # summary() -> ""
    assert plan.summary() == ""
    out = await fetch_post_plan_relevant_knowledge(
        turn=turn,
        agent_context=ctx,
        plan=plan,
        llm_providers={"cheap": _ScriptedProvider([])},
    )
    assert out == ""
    assert searcher.calls == []
    assert queue.published == []


# ---------------------------------------------------------------------------
# _fetch_relevant_knowledge_block -- query_override
# ---------------------------------------------------------------------------


async def test_query_override_forces_thin_trigger_gate_off() -> None:
    """``query_override`` is the post-Plan path: even though the turn's
    trigger is thin (the Plan-time gate fired), the override carries a
    real recon-informed query, so the fetch must run -- not gate."""
    role = _role()
    org = _org_with_role(role)
    searcher = _SearcherStub([_hit(title="Doc A", snippet="body")])
    queue = _EventQueueStub()
    provider = _ScriptedProvider([Completion(content="payments regression")])
    turn = _mk_turn(trigger_event=_thin_trigger())
    ctx = _mk_ctx(org=org, searcher=searcher, queue=queue)
    out = await _fetch_relevant_knowledge_block(
        turn=turn,
        agent_context=ctx,
        llm_providers={"cheap": provider},
        query_override="triage the POC-528 payments regression",
    )
    assert "Doc A" in out
    # The fetch ran: the aux call fired and the searcher was queried.
    assert len(provider.calls) == 1
    assert len(searcher.calls) == 1
    assert "POC-528 payments regression" in _user_prompt(provider.calls[0])


async def test_no_query_override_keeps_thin_trigger_gate_on() -> None:
    """Without ``query_override`` the wrapper still honours the
    thin-trigger gate (the Plan-prefetch path) -- the fetch is gated and
    renders the EMPTY_FILTER_HINT instead of searching."""
    role = _role()
    org = _org_with_role(role)
    searcher = _SearcherStub([_hit(title="Doc A", snippet="body")])
    queue = _EventQueueStub()
    provider = _ScriptedProvider([Completion(content="query")])
    turn = _mk_turn(trigger_event=_thin_trigger())
    ctx = _mk_ctx(org=org, searcher=searcher, queue=queue)
    out = await _fetch_relevant_knowledge_block(
        turn=turn, agent_context=ctx, llm_providers={"cheap": provider}
    )
    # Gate fired: the EMPTY_FILTER_HINT text, no search, no aux call.
    # The hint points at the knowledge base by capability, not by a
    # hardcoded tool name.
    assert "knowledge base" in out
    assert "confluence_search" not in out
    assert searcher.calls == []
    assert provider.calls == []


# ---------------------------------------------------------------------------
# RelevantKnowledgeRefetched event shape
# ---------------------------------------------------------------------------


def test_relevant_knowledge_refetched_event_summary() -> None:
    ev = RelevantKnowledgeRefetched(
        source="eng", role="Engineer", selection_count=3, block_bytes=210
    )
    assert ev.type == "relevant_knowledge_refetched"
    assert "re-fetched relevant knowledge" in ev.summary
    assert "3 doc(s)" in ev.summary


# ---------------------------------------------------------------------------
# Prefetch wrappers key on the salient inbound message
#
# Notification-triggered turns carry a task_description that front-loads
# ~1.5k chars of triage scaffolding before the actual message.  A
# query keyed on a prefix of that string never sees the message -- the
# bug where a stored memory that perfectly answered a Slack question
# went unused because the filter only saw boilerplate.
# ---------------------------------------------------------------------------


def _notification_with_scaffolding(salient: str) -> ExternalNotification:
    """A non-recon Slack notification whose ``body`` is enriched
    boilerplate and whose ``salient_body`` is the raw message."""
    return ExternalNotification(
        notification_source="slack",
        sender="U1",
        body=(
            "A Slack message was posted by **U1**.\n## Triage — decide BEFORE "
            "replying\n" + ("scaffolding " * 200)
        ),
        salient_body=salient,
        metadata={"slack_user_id": "U1"},
        context_requires_recon=False,
    )


async def test_relevant_knowledge_wrapper_filters_against_salient_body() -> None:
    """``_fetch_relevant_knowledge_block`` (no override) keys the aux
    query-generation call on the raw inbound message, not the enriched
    task description."""
    role = _role()
    org = _org_with_role(role)
    searcher = _SearcherStub([_hit(title="Doc A", snippet="body")])
    queue = _EventQueueStub()
    provider = _ScriptedProvider([Completion(content="gpu scheduling")])
    turn = _mk_turn(
        trigger_event=_notification_with_scaffolding(
            "how do we handle GPU scheduling?"
        ),
        task_description="ENRICHED TASK DESCRIPTION (fallback only)",
    )
    ctx = _mk_ctx(org=org, searcher=searcher, queue=queue)
    await _fetch_relevant_knowledge_block(
        turn=turn, agent_context=ctx, llm_providers={"cheap": provider}
    )
    assert "how do we handle GPU scheduling?" in _user_prompt(provider.calls[0])


async def test_personal_memory_wrapper_filters_against_salient_body(
    monkeypatch,
) -> None:
    """``_fetch_personal_memory_block_wrapper`` passes the raw inbound
    message to ``fetch_personal_memory_block`` -- this is the exact path
    from the trace where the triage scaffolding pushed the real
    question past the filter's prompt-truncation window, so a memory
    that answered the question went unused."""
    captured: dict[str, str] = {}

    async def _fake_fetch(**kwargs: Any) -> str:
        captured["task_description"] = kwargs["task_description"]
        return ""

    monkeypatch.setattr("crewlet.agent.plan.fetch_personal_memory_block", _fake_fetch)
    role = _role()
    org = _org_with_role(role)
    queue = _EventQueueStub()
    turn = _mk_turn(
        trigger_event=_notification_with_scaffolding(
            "who can review the backend fixes this week?"
        ),
        task_description="ENRICHED TASK DESCRIPTION (fallback only)",
    )
    ctx = _mk_ctx(org=org, searcher=_SearcherStub([]), queue=queue)
    await _fetch_personal_memory_block_wrapper(
        turn=turn, agent_context=ctx, llm_providers=None
    )
    assert captured["task_description"] == (
        "who can review the backend fixes this week?"
    )
