"""Tests for the round-cap extension judge (`crewlet.agent.extension`)."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import pytest

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.execute import run_execute_phase
from crewlet.agent.extension import (
    ExtensionDecision,
    judge_extension,
    maybe_extend,
)
from crewlet.agent.instance import AgentInstance
from crewlet.agent.llm_loop import LoopResult
from crewlet.agent.plan import ExecutionPlan, run_plan_phase
from crewlet.agent.turn_context import TurnContext
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, Message, ToolCall
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry
from crewlet.tools.surface import ToolSurface

# ---------------------------------------------------------------------------
# Test scaffolding (mirrors test_execute_review_subagent.py)
# ---------------------------------------------------------------------------


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


@dataclass
class _ProviderStub:
    """Plays a fixed list of Completions in order; defaults to ``"done"``."""

    completions: list[Completion]
    calls: int = 0
    model: str = "stub"

    async def complete(
        self, messages, tools=None, temperature=0.7, max_tokens=None, tool_choice=None
    ):
        idx = self.calls
        self.calls += 1
        if idx >= len(self.completions):
            return Completion(content="done", tool_calls=[])
        return self.completions[idx]


async def _noop(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
    return ToolResult(success=True, output="done")


def _mk_agent() -> AgentInstance:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    return AgentInstance(definition=defn, handle="eng", email="e@acme.com")


def _ctx() -> AgentContext:
    return AgentContext(
        agent_id="a", agent_handle="eng", role="Engineer", event_queue=_QueueStub()
    )


def _extend_decision_call(rounds: int, reason: str = "progress") -> Completion:
    """Build a Completion that calls submit_extension_decision(extend)."""
    return Completion(
        content="",
        tool_calls=[
            ToolCall(
                id="judge1",
                name="submit_extension_decision",
                arguments={
                    "decision": "extend",
                    "additional_rounds": rounds,
                    "reason": reason,
                },
            )
        ],
    )


def _rescue_decision_call(reason: str = "thrashing") -> Completion:
    return Completion(
        content="",
        tool_calls=[
            ToolCall(
                id="judge1",
                name="submit_extension_decision",
                arguments={"decision": "rescue", "reason": reason},
            )
        ],
    )


@pytest.fixture
def registry() -> ToolRegistry:
    r = ToolRegistry()
    r.register(
        SimpleTool(
            name="search",
            description="Search.",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    r.register(
        SimpleTool(
            name="query_knowledge",
            description="Query knowledge.",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    return r


# ---------------------------------------------------------------------------
# judge_extension — direct calls
# ---------------------------------------------------------------------------


async def test_judge_extension_parses_extend_decision(registry):
    provider = _ProviderStub(completions=[_extend_decision_call(5, "real progress")])
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="t")
    ctx = _ctx()

    decision = await judge_extension(
        phase="execute",
        task_description="t",
        plan_summary="step 1",
        executions=[
            {"name": "search", "arguments": "{}", "result": "ok", "success": True}
        ],
        last_assistant_text="about to do X",
        rounds_used=20,
        max_step=8,
        rounds_remaining_under_ceiling=20,
        provider=provider,
        provider_key="stub",
        context=ctx,
        turn=turn,
        event_queue=ctx.event_queue,
        registry=registry,
        role_mcp_tools=[],
    )

    assert decision.decision == "extend"
    assert decision.additional_rounds == 5
    assert decision.reason == "real progress"


async def test_judge_extension_parses_rescue_decision(registry):
    provider = _ProviderStub(completions=[_rescue_decision_call("looping")])
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="t")
    ctx = _ctx()

    decision = await judge_extension(
        phase="plan",
        task_description="t",
        plan_summary="",
        executions=[],
        last_assistant_text="",
        rounds_used=16,
        max_step=8,
        rounds_remaining_under_ceiling=16,
        provider=provider,
        provider_key="stub",
        context=ctx,
        turn=turn,
        event_queue=ctx.event_queue,
        registry=registry,
        role_mcp_tools=[],
    )

    assert decision.decision == "rescue"
    assert decision.reason == "looping"


async def test_judge_extension_provider_error_falls_back_to_rescue(registry):
    """A provider exception during the judge call must map to a rescue
    decision -- the host phase must not be blocked on the judge."""

    class _BadProvider:
        model = "bad"

        async def complete(self, **_):
            raise RuntimeError("boom")

    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="t")
    ctx = _ctx()

    decision = await judge_extension(
        phase="execute",
        task_description="t",
        plan_summary="",
        executions=[],
        last_assistant_text="",
        rounds_used=20,
        max_step=8,
        rounds_remaining_under_ceiling=20,
        provider=_BadProvider(),
        provider_key="bad",
        context=ctx,
        turn=turn,
        event_queue=ctx.event_queue,
        registry=registry,
        role_mcp_tools=[],
    )

    assert decision.decision == "rescue"
    assert decision.reason == "judge_call_failed"


async def test_judge_extension_unparseable_args_falls_back_to_rescue(registry):
    """A judge that returns garbage in the tool args must still map to a
    safe rescue rather than thrashing the host phase."""
    bad_completion = Completion(
        content="",
        tool_calls=[
            ToolCall(
                id="j",
                name="submit_extension_decision",
                arguments={"decision": "magic-wand"},  # invalid enum
            )
        ],
    )
    provider = _ProviderStub(completions=[bad_completion])
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="t")
    ctx = _ctx()

    decision = await judge_extension(
        phase="execute",
        task_description="t",
        plan_summary="",
        executions=[],
        last_assistant_text="",
        rounds_used=20,
        max_step=8,
        rounds_remaining_under_ceiling=20,
        provider=provider,
        provider_key="stub",
        context=ctx,
        turn=turn,
        event_queue=ctx.event_queue,
        registry=registry,
        role_mcp_tools=[],
    )

    assert decision.decision == "rescue"
    assert decision.reason == "parse_failed"


# ---------------------------------------------------------------------------
# maybe_extend — gating + execution
# ---------------------------------------------------------------------------


def _exhausted_loop() -> LoopResult:
    """Build a LoopResult that looks like an exhausted main-phase loop."""
    return LoopResult(
        text="last assistant text",
        input_tokens=100,
        output_tokens=50,
        tool_executions=[
            {"name": "search", "arguments": "{}", "result": "ok", "success": True}
        ],
        rounds_used=20,
        exhausted_rounds=True,
        model="main",
        messages=[
            Message(role="system", content="sys"),
            Message(role="user", content="usr"),
            Message(role="assistant", content="last assistant text", tool_calls=[]),
        ],
    )


async def test_maybe_extend_disabled_returns_rescue_without_judge(registry):
    main_loop = _exhausted_loop()
    judge_provider = _ProviderStub(completions=[])  # should never be called
    main_provider = _ProviderStub(completions=[])
    surface = ToolSurface.for_execute(
        registry, [], tools_needed=["search"], always_on=[]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="t")
    ctx = _ctx()

    ext_loop, decision = await maybe_extend(
        main_loop=main_loop,
        phase="execute",
        task_description="t",
        plan_summary="",
        judge_provider=judge_provider,
        judge_provider_key="judge",
        main_provider=main_provider,
        main_provider_key="main",
        main_surface=surface,
        main_terminate_after=None,
        main_tool_choice=None,
        enabled=False,
        round_step=8,
        rounds_remaining_under_ceiling=20,
        context=ctx,
        turn=turn,
        event_queue=ctx.event_queue,
        registry=registry,
        role_mcp_tools=[],
    )

    assert ext_loop is None
    assert decision.decision == "rescue"
    assert decision.reason == "extension_disabled"
    assert judge_provider.calls == 0


async def test_maybe_extend_at_ceiling_returns_rescue_without_judge(registry):
    main_loop = _exhausted_loop()
    judge_provider = _ProviderStub(completions=[])
    main_provider = _ProviderStub(completions=[])
    surface = ToolSurface.for_execute(
        registry, [], tools_needed=["search"], always_on=[]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="t")
    ctx = _ctx()

    ext_loop, decision = await maybe_extend(
        main_loop=main_loop,
        phase="execute",
        task_description="t",
        plan_summary="",
        judge_provider=judge_provider,
        judge_provider_key="judge",
        main_provider=main_provider,
        main_provider_key="main",
        main_surface=surface,
        main_terminate_after=None,
        main_tool_choice=None,
        enabled=True,
        round_step=8,
        rounds_remaining_under_ceiling=0,  # already at ceiling
        context=ctx,
        turn=turn,
        event_queue=ctx.event_queue,
        registry=registry,
        role_mcp_tools=[],
    )

    assert ext_loop is None
    assert decision.decision == "rescue"
    assert decision.reason == "ceiling_reached"
    assert judge_provider.calls == 0


async def test_maybe_extend_judge_says_rescue_returns_no_extension(registry):
    main_loop = _exhausted_loop()
    judge_provider = _ProviderStub(completions=[_rescue_decision_call("stuck")])
    main_provider = _ProviderStub(completions=[])
    surface = ToolSurface.for_execute(
        registry, [], tools_needed=["search"], always_on=[]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="t")
    ctx = _ctx()

    ext_loop, decision = await maybe_extend(
        main_loop=main_loop,
        phase="execute",
        task_description="t",
        plan_summary="",
        judge_provider=judge_provider,
        judge_provider_key="judge",
        main_provider=main_provider,
        main_provider_key="main",
        main_surface=surface,
        main_terminate_after=None,
        main_tool_choice=None,
        enabled=True,
        round_step=8,
        rounds_remaining_under_ceiling=20,
        context=ctx,
        turn=turn,
        event_queue=ctx.event_queue,
        registry=registry,
        role_mcp_tools=[],
    )

    assert ext_loop is None
    assert decision.decision == "rescue"
    # The main provider must not have been called -- no extension.
    assert main_provider.calls == 0


async def test_maybe_extend_judge_says_extend_runs_continuation(registry):
    main_loop = _exhausted_loop()
    judge_provider = _ProviderStub(completions=[_extend_decision_call(4, "progress")])
    # Main provider returns a final assistant message (no tool calls) so
    # the extension loop terminates cleanly within the granted rounds.
    main_provider = _ProviderStub(
        completions=[Completion(content="done!", tool_calls=[])]
    )
    surface = ToolSurface.for_execute(
        registry, [], tools_needed=["search"], always_on=[]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="t")
    ctx = _ctx()

    pre_msg_count = len(main_loop.messages)
    ext_loop, decision = await maybe_extend(
        main_loop=main_loop,
        phase="execute",
        task_description="t",
        plan_summary="",
        judge_provider=judge_provider,
        judge_provider_key="judge",
        main_provider=main_provider,
        main_provider_key="main",
        main_surface=surface,
        main_terminate_after=None,
        main_tool_choice=None,
        enabled=True,
        round_step=8,
        rounds_remaining_under_ceiling=20,
        context=ctx,
        turn=turn,
        event_queue=ctx.event_queue,
        registry=registry,
        role_mcp_tools=[],
    )

    assert ext_loop is not None
    assert decision.decision == "extend"
    assert decision.additional_rounds == 4
    # Nudge user message + extension's terminal assistant message landed.
    assert len(main_loop.messages) > pre_msg_count
    # Extension is the same list object (run_tool_loop mutates in place).
    assert ext_loop.messages is main_loop.messages
    # Granted rounds bounded by round_step + judge value.
    assert main_provider.calls == 1


async def test_maybe_extend_clamps_judge_requested_rounds(registry):
    main_loop = _exhausted_loop()
    # Judge asks for 100; round_step caps at 8.
    judge_provider = _ProviderStub(completions=[_extend_decision_call(100, "ok")])

    granted_rounds: list[int] = []
    completions_remaining = [Completion(content="done!", tool_calls=[])] * 20

    class _RecordingProvider:
        model = "main"
        calls = 0

        async def complete(
            self,
            messages,
            tools=None,
            temperature=0.7,
            max_tokens=None,
            tool_choice=None,
        ):
            self.calls += 1
            # The Anthropic provider chooses max_rounds via for-loop; here we just
            # return a clean terminal assistant message.
            return completions_remaining.pop(0)

    main_provider = _RecordingProvider()
    surface = ToolSurface.for_execute(
        registry, [], tools_needed=["search"], always_on=[]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="t")
    ctx = _ctx()

    ext_loop, decision = await maybe_extend(
        main_loop=main_loop,
        phase="execute",
        task_description="t",
        plan_summary="",
        judge_provider=judge_provider,
        judge_provider_key="judge",
        main_provider=main_provider,
        main_provider_key="main",
        main_surface=surface,
        main_terminate_after=None,
        main_tool_choice=None,
        enabled=True,
        round_step=8,
        rounds_remaining_under_ceiling=4,  # tighter than round_step
        context=ctx,
        turn=turn,
        event_queue=ctx.event_queue,
        registry=registry,
        role_mcp_tools=[],
    )

    assert ext_loop is not None
    # Granted was clamped to the smaller of (judge_request=100, round_step=8,
    # rounds_remaining_under_ceiling=4) = 4.  ext_loop.rounds_used must
    # therefore be <= 4.
    assert ext_loop.rounds_used <= 4
    granted_rounds.append(ext_loop.rounds_used)


# ---------------------------------------------------------------------------
# Integration: run_execute_phase wires extensions correctly
# ---------------------------------------------------------------------------


async def test_run_execute_phase_extends_when_judge_says_extend(registry):
    """End-to-end: Execute exhausts at 1 round, judge grants 2 more, the
    extended run terminates cleanly without the grace call firing.
    """
    # Round 1 of the main Execute loop: a tool call (forces exhaustion
    # because max_rounds=1).
    main_executions = [
        Completion(
            content="",
            tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
        ),
        # Extension round 1: terminal assistant message -- no more tools.
        Completion(content="finished after extension", tool_calls=[]),
    ]
    main_provider = _ProviderStub(completions=main_executions)
    judge_provider = _ProviderStub(completions=[_extend_decision_call(2, "ok")])

    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = _ctx()

    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=main_provider,
        provider_key="main",
        registry=registry,
        role_mcp_tools=[],
        always_on=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=1,
        judge_provider=judge_provider,
        judge_provider_key="judge",
        extension_enabled=True,
        extension_ceiling=10,
        extension_round_step=8,
    )

    # The extension finished cleanly -- result is not exhausted.
    assert result.exhausted_rounds is False
    # Extension's text replaced the partial main-loop text.
    assert "extension" in result.text


async def test_run_execute_phase_skips_extension_when_disabled(registry):
    main_provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            # Grace wrap-up (no tools).
            Completion(content="wrap-up", tool_calls=[]),
        ]
    )
    judge_provider = _ProviderStub(completions=[_extend_decision_call(8)])

    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = _ctx()

    await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=main_provider,
        provider_key="main",
        registry=registry,
        role_mcp_tools=[],
        always_on=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=1,
        judge_provider=judge_provider,
        judge_provider_key="judge",
        extension_enabled=False,  # disabled
        extension_ceiling=10,
        extension_round_step=8,
    )

    # Judge must not have been called when disabled.
    assert judge_provider.calls == 0


async def test_run_execute_phase_skips_extension_when_no_judge_provider(registry):
    """No judge provider configured -> behaves exactly like pre-extension."""
    main_provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="wrap-up", tool_calls=[]),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = _ctx()

    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=main_provider,
        provider_key="main",
        registry=registry,
        role_mcp_tools=[],
        always_on=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=1,
        extension_enabled=True,  # enabled, but no judge_provider
        extension_ceiling=10,
        extension_round_step=8,
    )

    # Falls through to grace; main loop exhausted.
    assert result.exhausted_rounds is True
    assert "wrap-up" in result.text


async def test_run_execute_phase_stops_at_ceiling_then_grace_fires(registry):
    """The extension respects the ceiling: after granting up to the cap,
    the grace call still fires when the extension itself exhausts."""
    main_provider = _ProviderStub(
        completions=[
            # Main loop: 1 tool call, exhausts.
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            # Extension: 1 tool call, exhausts at granted=1.
            Completion(
                content="",
                tool_calls=[ToolCall(id="c2", name="search", arguments={"q": "y"})],
            ),
            # Grace wrap-up.
            Completion(content="wrap-up", tool_calls=[]),
        ]
    )
    # Judge would extend forever, but the ceiling is 2 total rounds.
    judge_provider = _ProviderStub(completions=[_extend_decision_call(5, "go")])

    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = _ctx()

    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=main_provider,
        provider_key="main",
        registry=registry,
        role_mcp_tools=[],
        always_on=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=1,
        judge_provider=judge_provider,
        judge_provider_key="judge",
        extension_enabled=True,
        extension_ceiling=2,  # main(1) + extension(1) hits ceiling
        extension_round_step=1,  # one round at a time
    )

    # Ceiling reached -> still exhausted -> grace fired -> wrap-up is the text.
    assert result.exhausted_rounds is True
    assert "wrap-up" in result.text
    # Judge called once (the second exhaustion would have called it
    # again, but the ceiling check skipped that call).
    assert judge_provider.calls == 1


# ---------------------------------------------------------------------------
# ExtensionDecision model
# ---------------------------------------------------------------------------


def test_extension_decision_defaults_to_rescue():
    """Defaults match the conservative fallback used for parse failures."""
    d = ExtensionDecision()
    assert d.decision == "rescue"
    assert d.additional_rounds == 0
    assert d.reason == ""


# ---------------------------------------------------------------------------
# Event ordering — judges deferred so they emit AFTER the host phase
# ---------------------------------------------------------------------------


async def test_judge_event_deferred_emits_after_host_phase_event(registry):
    """When a judge fires during Execute, its ``AgentPhaseCompleted``
    event must be published AFTER the host execute phase's own event.

    Without the deferral chronological dashboards render judges before
    the phase they belong to, which is the bug reported in
    https://github.com/crewlet/crewlet/pull/506 review.
    """
    captured: list[tuple[str, Any]] = []

    class _Queue:
        async def publish(self, topic: str, event: Any) -> None:
            captured.append((topic, event))

    queue = _Queue()
    main_provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="finished after extension", tool_calls=[]),
        ]
    )
    judge_provider = _ProviderStub(completions=[_extend_decision_call(2, "ok")])

    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = AgentContext(
        agent_id="a", agent_handle="eng", role="Engineer", event_queue=queue
    )

    await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=main_provider,
        provider_key="main",
        registry=registry,
        role_mcp_tools=[],
        always_on=[],
        event_queue=queue,
        agent_context=ctx,
        max_rounds=1,
        judge_provider=judge_provider,
        judge_provider_key="judge",
        extension_enabled=True,
        extension_ceiling=10,
        extension_round_step=8,
    )

    phase_events = [
        e for (_t, e) in captured if getattr(e, "type", "") == "agent_phase_completed"
    ]
    # Expected order: execute (host) first, then judge (deferred).
    phases_in_order = [e.phase for e in phase_events]
    assert phases_in_order == ["execute", "judge"], (
        f"Expected ['execute', 'judge'], got {phases_in_order}"
    )
    # And the judge event carries the host-phase link for dashboard grouping.
    judge_event = phase_events[-1]
    assert judge_event.host_phase == "execute"
    assert judge_event.host_iteration == turn.iteration


async def test_tag_phase_span_writes_attributes_on_current_span():
    """``tag_phase_span`` is the single sink that writes phase attrs
    to the OTel span.  Verify it sets the expected keys.

    The extension judge calls this eagerly (inside its own span
    context) so the deferred publish doesn't re-tag a different span
    later.  The deferred publish opts out via ``tag_span=False`` on
    ``publish_phase_completed``.
    """
    from crewlet.agent.llm_loop import tag_phase_span

    set_attrs: dict[str, Any] = {}

    class _FakeSpan:
        def is_recording(self) -> bool:
            return True

        def set_attribute(self, key: str, value: Any) -> None:
            set_attrs[key] = value

    # Patch only the get_current_span seen by tag_phase_span -- it
    # imports lazily inside the function, so we patch the module attr.
    from opentelemetry import trace as _ot

    original = _ot.get_current_span
    _ot.get_current_span = lambda: _FakeSpan()  # type: ignore[assignment]
    try:
        tag_phase_span(
            phase="judge",
            iteration=2,
            turn_id="turn-1",
            rounds_used=3,
            exhausted_rounds=False,
            rescue_fired=False,
            tool_executions_count=4,
            decision="extend",
            model="haiku",
            input_tokens=120,
            output_tokens=80,
        )
    finally:
        _ot.get_current_span = original  # type: ignore[assignment]

    assert set_attrs["phase.name"] == "judge"
    assert set_attrs["phase.iteration"] == 2
    assert set_attrs["phase.rounds_used"] == 3
    assert set_attrs["phase.decision"] == "extend"
    assert set_attrs["gen_ai.request.model"] == "haiku"
    assert set_attrs["gen_ai.usage.input_tokens"] == 120
    assert set_attrs["gen_ai.usage.output_tokens"] == 80


async def test_run_plan_phase_extension_does_not_double_count_tool_sequence(registry):
    """``turn.plan_tool_sequence`` must contain each tool call exactly
    once -- the post-loop iterator walks ``loop.tool_executions`` (which
    we now MERGE in-place with extensions), so an inline per-extension
    append would double-count every extension call.

    Appending ext_loop.tool_executions
    twice -- once inline in the extension loop, then again via the
    merged ``loop.tool_executions`` post-loop iterator -- would land
    each extension tool call in ``plan_tool_sequence`` twice.
    Downstream consumers that filter the sequence by membership still
    worked, but consumers that counted occurrences (or rendered the
    sequence) saw bogus duplicates.
    """
    captured: list[tuple[str, Any]] = []

    class _Queue:
        async def publish(self, topic: str, event: Any) -> None:
            captured.append((topic, event))

    queue = _Queue()
    main_provider = _ProviderStub(
        completions=[
            # Main loop: one activate_tool call -- exhausts at max_rounds=1.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="c1",
                        name="activate_tool",
                        arguments={"name": "search"},
                    )
                ],
            ),
            # Extension: submit_plan, terminates cleanly.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="c2",
                        name="submit_plan",
                        arguments={
                            "decision": "plan",
                            "reasoning": "ok",
                            "steps": [{"intent": "go", "tools": ["search"]}],
                            "tools_needed": ["search"],
                            "success_criteria": [],
                        },
                    )
                ],
            ),
        ]
    )
    judge_provider = _ProviderStub(completions=[_extend_decision_call(4, "ok")])
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    ctx = AgentContext(
        agent_id="a", agent_handle="eng", role="Engineer", event_queue=queue
    )

    await run_plan_phase(
        turn=turn,
        provider=main_provider,
        provider_key="plan-stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=ctx,
        max_rounds=1,
        judge_provider=judge_provider,
        judge_provider_key="judge",
        extension_enabled=True,
        extension_ceiling=10,
        extension_round_step=8,
    )

    # Each call should appear exactly once.
    assert turn.plan_tool_sequence.count("activate_tool") == 1
    assert turn.plan_tool_sequence.count("submit_plan") == 1


async def test_run_plan_phase_extension_merges_into_main_loop(registry):
    """Plan phase must also merge extensions into the main loop (same
    contract as Execute).  Reassigning
    ``loop = ext_loop`` would leave the published plan phase event
    carrying only the LAST extension's data, hiding the main loop's
    tool calls.
    """
    captured: list[tuple[str, Any]] = []

    class _Queue:
        async def publish(self, topic: str, event: Any) -> None:
            captured.append((topic, event))

    queue = _Queue()
    main_provider = _ProviderStub(
        completions=[
            # Main loop: activate a tool (any tool call works to make
            # the loop exhaust without ``submit_plan``).
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="c1",
                        name="activate_tool",
                        arguments={"name": "search"},
                    )
                ],
            ),
            # Extension round: submit_plan (terminates the loop cleanly).
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="c2",
                        name="submit_plan",
                        arguments={
                            "decision": "plan",
                            "reasoning": "ok",
                            "steps": [{"intent": "go", "tools": ["search"]}],
                            "tools_needed": ["search"],
                            "success_criteria": [],
                        },
                    )
                ],
            ),
        ]
    )
    judge_provider = _ProviderStub(completions=[_extend_decision_call(4, "ok")])
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    ctx = AgentContext(
        agent_id="a", agent_handle="eng", role="Engineer", event_queue=queue
    )

    await run_plan_phase(
        turn=turn,
        provider=main_provider,
        provider_key="plan-stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=ctx,
        max_rounds=1,  # force exhaustion after one tool call
        judge_provider=judge_provider,
        judge_provider_key="judge",
        extension_enabled=True,
        extension_ceiling=10,
        extension_round_step=8,
    )

    plan_events = [
        e
        for (_t, e) in captured
        if getattr(e, "type", "") == "agent_phase_completed" and e.phase == "plan"
    ]
    assert plan_events, "Expected one plan phase event"
    ev = plan_events[-1]
    names = [t.get("name") for t in ev.tool_executions]
    # BOTH the main loop's activate_tool AND the extension's
    # submit_plan must appear -- the merge preserves the full Plan
    # picture for dashboards.
    assert "activate_tool" in names
    assert "submit_plan" in names


async def test_run_execute_phase_extension_merges_into_main_loop(registry):
    """When an extension fires, the Execute phase event must reflect
    BOTH the main loop's and the extension's tool calls / tokens --
    the extension is conceptually part of the same Execute run.

    Pins that Execute extends-in-place rather than reassigning
    ``loop = ext_loop``, which would lose the main loop's data.
    """
    captured: list[tuple[str, Any]] = []

    class _Queue:
        async def publish(self, topic: str, event: Any) -> None:
            captured.append((topic, event))

    queue = _Queue()
    main_provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="c2", name="search", arguments={"q": "y"})],
            ),
            Completion(content="done", tool_calls=[]),
        ]
    )
    judge_provider = _ProviderStub(completions=[_extend_decision_call(4, "ok")])
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = AgentContext(
        agent_id="a", agent_handle="eng", role="Engineer", event_queue=queue
    )

    await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=main_provider,
        provider_key="main",
        registry=registry,
        role_mcp_tools=[],
        always_on=[],
        event_queue=queue,
        agent_context=ctx,
        max_rounds=1,
        judge_provider=judge_provider,
        judge_provider_key="judge",
        extension_enabled=True,
        extension_ceiling=10,
        extension_round_step=8,
    )

    execute_events = [
        e
        for (_t, e) in captured
        if getattr(e, "type", "") == "agent_phase_completed" and e.phase == "execute"
    ]
    assert execute_events, "Expected one execute phase event"
    ev = execute_events[-1]
    # Both the main loop's `search` (c1) AND the extension's `search`
    # (c2) must appear in the phase event's tool log.
    names_and_args = [(t.get("name"), t.get("arguments")) for t in ev.tool_executions]
    assert ("search", '{"q": "x"}') in names_and_args
    assert ("search", '{"q": "y"}') in names_and_args
    # And rounds_used reflects main(1) + extension(2: tool + terminal).
    assert ev.rounds_used >= 2
