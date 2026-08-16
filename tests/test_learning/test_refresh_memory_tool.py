"""Unit tests for the ``refresh_memory`` Plan-phase builtin.

Covers:
- Happy path: tool calls fetch_personal_memory_block with enriched
  task description and returns rendered output.
- Idempotency: repeat calls with the same context_hint return cached
  output without firing a fresh LLM call and without consuming cap.
- Per-turn cap: distinct hints exceeding the cap return an error.
- Per-turn isolation: state for turn A doesn't leak into turn B.
- Failure modes: missing knowledge, missing agent_id, missing
  context_hint, missing turn_context, role lookup returning None,
  fetch raising.
- Sender propagation: trigger_event surfaces as a ``Current sender:``
  line in the filter prompt.
"""

from __future__ import annotations

from typing import Any

from crewlet.events.types import ExternalNotification
from crewlet.learning.personal_memory import EMPTY_FILTER_HINT
from crewlet.learning.tools import (
    DEFAULT_MAX_REFRESHES,
    _hint_hash,
    register_refresh_memory_tool,
)
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import Completion
from crewlet.tools.protocol import AgentContext, Tool
from crewlet.tools.registry import ToolRegistry


def _tool_or_raise(registry: ToolRegistry, name: str) -> Tool:
    """Helper: lookup a tool and assert it exists.  ``ToolRegistry.get``
    returns ``Optional[Tool]``; tests want a hard assertion."""
    tool = registry.get(name)
    assert tool is not None, f"tool {name!r} not registered"
    return tool


class _ScriptedProvider:
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
        self.calls.append({"messages": messages})
        if self._completions:
            return self._completions.pop(0)
        return Completion(content="[]")


class _DiaryStub:
    """Stub matching :class:`~crewlet.learning.diary.AgentDiary`'s
    read surface for the refresh-tool flow.  ``refresh_memory`` is
    read-only; we don't need ``write`` here.
    """

    def __init__(self, docs: list[dict[str, Any]] | None = None) -> None:
        self.docs = list(docs or [])

    async def list_for_agent(self, agent_id: str, **kwargs) -> list[dict[str, Any]]:
        return list(self.docs)

    async def mark_retrieved(self, doc_id: str) -> None:
        # No-op; refresh tests don't assert on telemetry bumps.
        return None

    async def mark_retrieved_many(self, doc_ids: list[str]) -> None:
        # No-op; refresh tests don't assert on telemetry bumps.
        return None


def _doc(content: str, kind: str = "diary_long") -> dict[str, Any]:
    return {
        "id": "row",
        "agent_id": "agent-1",
        "content": content,
        "metadata": {"kind": kind},
    }


def _role() -> Role:
    return Role(name="Engineer", handle="eng", llm="cheap", llm_auxiliary="cheap")


class _TurnCtxStub:
    """Minimal stand-in for ``TurnContext``.

    The real type pulls in AgentInstance + Org which are heavy to
    fixture; the tool only reads ``turn_id``, ``task_description``,
    and ``trigger_interactions()``.
    """

    def __init__(
        self,
        *,
        turn_id: str = "t1",
        task_description: str = "respond to slack",
        trigger_event: Any = None,
    ) -> None:
        self.turn_id = turn_id
        self.task_description = task_description
        self.trigger_event = trigger_event

    def trigger_interactions(self) -> list[Any]:
        from crewlet.learning.interaction import InboundInteraction

        return InboundInteraction.list_from_trigger_event(self.trigger_event)


def _make_ctx(
    *,
    turn_ctx: _TurnCtxStub | None = None,
    diary: Any = None,
    agent_id: str = "agent-1",
    agent_handle: str = "alice",
) -> AgentContext:
    ctx = AgentContext(
        agent_id=agent_id,
        agent_handle=agent_handle,
        role="Engineer",
    )
    ctx.agent_diary = diary  # type: ignore[assignment]
    if turn_ctx is not None:
        ctx.__dict__["turn_context"] = turn_ctx
    return ctx


def _setup(
    completions: list[Completion],
    *,
    docs: list[dict[str, Any]] | None = None,
    cap: int = DEFAULT_MAX_REFRESHES,
    role_lookup_returns_none: bool = False,
) -> tuple[ToolRegistry, _DiaryStub, _ScriptedProvider]:
    registry = ToolRegistry()
    knowledge = _DiaryStub(docs)
    provider = _ScriptedProvider(completions)

    def _lookup(handle: str) -> Role | None:
        return None if role_lookup_returns_none else _role()

    register_refresh_memory_tool(
        registry,
        knowledge,  # type: ignore[arg-type]
        llm_providers={"cheap": provider},
        role_lookup=_lookup,
        max_refreshes_per_turn=cap,
    )
    return registry, knowledge, provider


# ---------------------------------------------------------------------
# Happy path
# ---------------------------------------------------------------------


async def test_refresh_memory_runs_filter_and_returns_rendered_block() -> None:
    """The tool re-runs the filter with enriched context and returns
    the rendered ``## Personal memory`` body as its tool output."""
    registry, _, provider = _setup(
        [Completion(content="[0]")],
        docs=[_doc("User Sam is OOO; route backend reviews to Maria")],
    )
    tool = _tool_or_raise(registry, "refresh_memory")
    turn_ctx = _TurnCtxStub(task_description="respond to a Slack message")
    ctx = _make_ctx(turn_ctx=turn_ctx, diary=_DiaryStub())
    # Override knowledge via context so the tool has somewhere to read.
    ctx.agent_diary = _DiaryStub(  # type: ignore[assignment]
        [_doc("User Sam is OOO; route backend reviews to Maria")]
    )

    result = await tool.execute(
        {"context_hint": "this is about a backend PR review"}, ctx
    )

    assert result.success is True
    assert "Sam is OOO" in result.output
    # Filter call inspected the enriched task description.
    assert len(provider.calls) == 1
    user_msg = provider.calls[0]["messages"][1].content
    assert "respond to a Slack message" in user_msg
    assert "this is about a backend PR review" in user_msg


async def test_refresh_memory_renders_no_match_message_when_filter_returns_empty() -> (
    None
):
    """When the filter returns ``[]`` the tool returns a friendly 'no
    matches' string instead of an empty payload, so the planner can
    distinguish 'tool ran, nothing matched' from 'tool failed'."""
    registry, _, provider = _setup(
        [Completion(content="[]")],
        docs=[_doc("User Sam is OOO; route backend reviews to Maria")],
    )
    tool = _tool_or_raise(registry, "refresh_memory")
    ctx = _make_ctx(turn_ctx=_TurnCtxStub())
    ctx.agent_diary = _DiaryStub(  # type: ignore[assignment]
        [_doc("User Sam is OOO; route backend reviews to Maria")]
    )

    result = await tool.execute({"context_hint": "unrelated topic"}, ctx)
    assert result.success is True
    # The fetcher renders the ``EMPTY_FILTER_HINT`` since candidates
    # exist but the filter surfaced none; the tool returns it verbatim.
    assert result.output == EMPTY_FILTER_HINT


# ---------------------------------------------------------------------
# Idempotency
# ---------------------------------------------------------------------


async def test_repeat_call_with_same_hint_is_cached_and_does_not_recall_llm() -> None:
    """Idempotency: same hint within the same turn returns the cached
    output without firing the LLM again or consuming cap."""
    registry, _, provider = _setup(
        [Completion(content="[0]")],
        docs=[_doc("Memory A")],
    )
    tool = _tool_or_raise(registry, "refresh_memory")
    ctx = _make_ctx(turn_ctx=_TurnCtxStub(turn_id="t-cache"))
    ctx.agent_diary = _DiaryStub([_doc("Memory A")])  # type: ignore[assignment]

    first = await tool.execute({"context_hint": "topic X"}, ctx)
    second = await tool.execute({"context_hint": "topic X"}, ctx)

    assert first.output == second.output
    # Only one LLM call -- second was served from cache.
    assert len(provider.calls) == 1


async def test_hint_normalisation_makes_trivial_differences_cache_hits() -> None:
    """Whitespace + case normalisation in the cache key so a planner
    that re-types the hint slightly differently still hits the cache."""
    assert _hint_hash("  Hello World  ") == _hint_hash("hello world")
    assert _hint_hash("Hello   World") == _hint_hash("hello world")


# ---------------------------------------------------------------------
# Per-turn cap
# ---------------------------------------------------------------------


async def test_distinct_hints_exceeding_cap_return_error() -> None:
    """Three distinct hints with cap=2 → third call rejected with an
    error message naming the cap, so the planner stops trying."""
    registry, _, provider = _setup(
        [Completion(content="[0]"), Completion(content="[0]")],
        docs=[_doc("Memory A")],
        cap=2,
    )
    tool = _tool_or_raise(registry, "refresh_memory")
    ctx = _make_ctx(turn_ctx=_TurnCtxStub(turn_id="t-cap"))
    ctx.agent_diary = _DiaryStub([_doc("Memory A")])  # type: ignore[assignment]

    r1 = await tool.execute({"context_hint": "first topic"}, ctx)
    r2 = await tool.execute({"context_hint": "second topic"}, ctx)
    r3 = await tool.execute({"context_hint": "third topic"}, ctx)

    assert r1.success and r2.success
    assert r3.success is False
    assert "cap reached" in (r3.error or "").lower()
    # Cap message must include the number so the planner knows the
    # rule, not just that it was rejected.
    assert "2" in (r3.error or "")
    # And the cap-rejected call did NOT consume an LLM call.
    assert len(provider.calls) == 2


async def test_cached_hits_do_not_count_against_cap() -> None:
    """Cap counts DISTINCT hints, not call count -- repeat hits are
    free.  Important: planner shouldn't fear re-reading the same
    refreshed context."""
    registry, _, _ = _setup(
        [Completion(content="[0]")],
        docs=[_doc("Memory A")],
        cap=1,
    )
    tool = _tool_or_raise(registry, "refresh_memory")
    ctx = _make_ctx(turn_ctx=_TurnCtxStub(turn_id="t-cap-cache"))
    ctx.agent_diary = _DiaryStub([_doc("Memory A")])  # type: ignore[assignment]

    r1 = await tool.execute({"context_hint": "the topic"}, ctx)
    r2 = await tool.execute({"context_hint": "the topic"}, ctx)
    r3 = await tool.execute({"context_hint": "the topic"}, ctx)
    assert r1.success and r2.success and r3.success


# ---------------------------------------------------------------------
# Per-turn isolation
# ---------------------------------------------------------------------


async def test_separate_turns_get_separate_state() -> None:
    """State for turn A must not leak into turn B -- otherwise a
    long-running engine accumulates phantom cap usage across turns."""
    registry, _, provider = _setup(
        [Completion(content="[0]"), Completion(content="[0]")],
        docs=[_doc("Memory A")],
        cap=1,
    )
    tool = _tool_or_raise(registry, "refresh_memory")

    ctx_a = _make_ctx(turn_ctx=_TurnCtxStub(turn_id="turn-a"))
    ctx_b = _make_ctx(turn_ctx=_TurnCtxStub(turn_id="turn-b"))

    r_a = await tool.execute({"context_hint": "topic"}, ctx_a)
    r_b = await tool.execute({"context_hint": "topic"}, ctx_b)
    assert r_a.success and r_b.success
    # Both turns ran the LLM independently; turn B was not blocked by
    # turn A's cap consumption.
    assert len(provider.calls) == 2


# ---------------------------------------------------------------------
# Sender propagation
# ---------------------------------------------------------------------


async def test_trigger_event_surfaces_as_current_sender_in_filter_prompt() -> None:
    """Refresh path keeps the per-subject discipline: when the trigger
    event names a sender, the filter prompt's ``Current sender:`` line
    must appear so per-subject memories don't leak across people."""
    registry, _, provider = _setup(
        [Completion(content="[]")],
        docs=[_doc("User Sam prefers terse replies")],
    )
    tool = _tool_or_raise(registry, "refresh_memory")
    trigger = ExternalNotification(
        source="test",
        notification_source="slack",
        sender="Miles",
        body="yes",
        metadata={"slack_user_id": "U0TESTUSER2"},
    )
    ctx = _make_ctx(
        turn_ctx=_TurnCtxStub(turn_id="t-sender", trigger_event=trigger),
    )
    ctx.agent_diary = _DiaryStub(  # type: ignore[assignment]
        [_doc("User Sam prefers terse replies")]
    )

    await tool.execute({"context_hint": "thread is about a PR review"}, ctx)
    user_msg = provider.calls[0]["messages"][1].content
    assert "Current sender:" in user_msg
    assert "Miles" in user_msg


# ---------------------------------------------------------------------
# Failure paths
# ---------------------------------------------------------------------


async def test_missing_context_hint_is_required() -> None:
    registry, _, provider = _setup([], cap=3)
    tool = _tool_or_raise(registry, "refresh_memory")
    ctx = _make_ctx(turn_ctx=_TurnCtxStub())
    ctx.agent_diary = _DiaryStub()  # type: ignore[assignment]

    result = await tool.execute({"context_hint": "   "}, ctx)
    assert result.success is False
    assert "context_hint" in (result.error or "")
    # And the LLM was never called for an invalid request.
    assert provider.calls == []


async def test_missing_turn_context_returns_error() -> None:
    """Without a TurnContext attached we can't scope per-turn state or
    fetch the original task description -- both essential."""
    registry, _, _ = _setup([])
    tool = _tool_or_raise(registry, "refresh_memory")
    # No turn_context attached.
    ctx = _make_ctx(turn_ctx=None, diary=_DiaryStub())

    result = await tool.execute({"context_hint": "topic"}, ctx)
    assert result.success is False
    assert "turn context" in (result.error or "").lower()


async def test_role_lookup_failure_returns_error() -> None:
    """Without a role we can't resolve the aux provider -- error
    surfaces with a hint rather than silently going through the
    'no aux provider' fall-through path."""
    registry, _, _ = _setup([], role_lookup_returns_none=True)
    tool = _tool_or_raise(registry, "refresh_memory")
    ctx = _make_ctx(turn_ctx=_TurnCtxStub())
    ctx.agent_diary = _DiaryStub()  # type: ignore[assignment]

    result = await tool.execute({"context_hint": "topic"}, ctx)
    assert result.success is False
    assert "role" in (result.error or "").lower()


async def test_missing_diary_returns_error() -> None:
    registry = ToolRegistry()
    register_refresh_memory_tool(
        registry,
        None,  # type: ignore[arg-type]
        llm_providers={"cheap": _ScriptedProvider([])},
        role_lookup=lambda h: _role(),
    )
    tool = _tool_or_raise(registry, "refresh_memory")
    ctx = _make_ctx(turn_ctx=_TurnCtxStub(), diary=None)
    result = await tool.execute({"context_hint": "topic"}, ctx)
    assert result.success is False
    assert "diary" in (result.error or "").lower()


async def test_silent_filter_failure_returns_no_match_message() -> None:
    """Transient LLM exceptions inside the filter are swallowed by
    ``personal_memory`` (its contract is "filter failure -> empty
    string"); the tool reports a friendly no-match message rather
    than a tool-level error.

    Note: silent failures DO consume cap (the tool can't distinguish
    them from intentional empty selections).  That's accepted -- the
    common case of a real outage will surface as repeated empty
    refreshes that the planner can interpret.
    """

    class _Boom:
        model = "boom"

        async def complete(self, *args, **kwargs):
            raise RuntimeError("transient outage")

    registry = ToolRegistry()
    knowledge = _DiaryStub([_doc("Memory")])
    register_refresh_memory_tool(
        registry,
        knowledge,  # type: ignore[arg-type]
        llm_providers={"cheap": _Boom()},  # type: ignore[dict-item]
        role_lookup=lambda h: _role(),
        max_refreshes_per_turn=2,
    )
    tool = _tool_or_raise(registry, "refresh_memory")
    ctx = _make_ctx(turn_ctx=_TurnCtxStub(turn_id="t-boom"))
    ctx.agent_diary = _DiaryStub([_doc("Memory")])  # type: ignore[assignment]

    result = await tool.execute({"context_hint": "first hint"}, ctx)
    # Tool succeeds (no tool-level error); output indicates no matches
    # rather than exposing the underlying outage.
    assert result.success is True
    assert "no relevant" in result.output.lower()


# ---------------------------------------------------------------------
# Tool description / metadata
# ---------------------------------------------------------------------


def test_tool_description_mentions_when_to_use_and_cap() -> None:
    """The description is the only signal the planner reads when
    deciding whether to call the tool.  It must encode the
    recon-based trigger (call after meaningful context-gathering,
    even when the initial block had entries), name the parameter,
    and surface the per-turn cap so the planner doesn't burn it."""
    registry, _, _ = _setup([], cap=5)
    tool = _tool_or_raise(registry, "refresh_memory")
    desc = tool.description.lower()
    # New framing: refresh is the standard after-recon pattern, not
    # an empty-block escape hatch.  The description must call that
    # out so the planner doesn't gate refresh on emptiness.
    assert "recon" in desc
    assert "even if" in desc and "entries" in desc
    assert "turn start" in desc
    assert "context_hint" in tool.description
    assert "5" in tool.description  # configured cap surfaces in prose


def test_tool_parameter_schema_marks_context_hint_required() -> None:
    registry, _, _ = _setup([])
    tool = _tool_or_raise(registry, "refresh_memory")
    schema = tool.parameters
    assert schema["properties"]["context_hint"]["type"] == "string"
    assert "context_hint" in schema["required"]


def test_default_cap_is_three() -> None:
    """Sanity: the documented default is 3 (matches
    :class:`PersonalMemoryConfig.max_refreshes_per_turn`)."""
    assert DEFAULT_MAX_REFRESHES == 3
