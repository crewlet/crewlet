"""Tests for Execute, Review, and spawn_subagent."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import pytest

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.execute import run_execute_phase
from crewlet.agent.instance import AgentInstance
from crewlet.agent.plan import ExecutionPlan
from crewlet.agent.review import ReviewOutcome, run_review_phase
from crewlet.agent.subagent import (
    SubagentBudgetExceeded,
    _FractionalBudgetManager,
    spawn_subagent,
)
from crewlet.agent.turn_context import TurnContext
from crewlet.events.types import ExecuteMissingTool
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, Message, ToolCall
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


@dataclass
class _ProviderStub:
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
# Execute phase
# ---------------------------------------------------------------------------


async def test_execute_uses_only_plan_tools_plus_always_on(registry):
    captured_tool_names: list[str] = []

    class _Recorder:
        model = "rec"

        async def complete(
            self,
            messages,
            tools=None,
            temperature=0.7,
            max_tokens=None,
            tool_choice=None,
        ):
            for td in tools or []:
                captured_tool_names.append(td.name)
            return Completion(content="ok", tool_calls=[])

    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = _ctx()
    await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=_Recorder(),
        provider_key="rec",
        registry=registry,
        role_mcp_tools=[],
        always_on=["query_knowledge"],
        event_queue=ctx.event_queue,
        agent_context=ctx,
    )
    # Execute always also carries the discovery meta-tools
    # so it can fill gaps the planner missed mid-run. The plan-named
    # set is still present; the surface is plan-named ∪ always-on ∪
    # meta-tools.
    assert set(captured_tool_names) == {
        "search",
        "query_knowledge",
        "activate_tool",
        "list_mcp_server_tools",
    }


async def test_execute_emits_missing_tool_event_when_llm_asks_for_unknown(registry):
    """LLM asks for a tool not in the surface -> 'execute.missing_tool'."""
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="not_in_plan", arguments={})],
            ),
            Completion(content="giving up", tool_calls=[]),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = _ctx()
    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        always_on=["query_knowledge"],
        event_queue=ctx.event_queue,
        agent_context=ctx,
    )
    assert "not_in_plan" in result.missing_tools
    missing_events = [
        e for _, e in ctx.event_queue.published if isinstance(e, ExecuteMissingTool)
    ]
    assert missing_events
    assert missing_events[0].tool_name == "not_in_plan"
    assert missing_events[0].plan_tools == ["search"]


async def test_execute_dedupes_repeated_missing_tool_calls(registry):
    """An LLM that loops and calls
    the same unknown tool N times should produce ONE missing_tools
    entry and ONE ``ExecuteMissingTool`` event, not N (event-store
    spam + noisy Review prompts).
    """
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id=f"c{i}", name="ghost_tool", arguments={})],
            )
            for i in range(4)
        ]
        + [Completion(content="giving up", tool_calls=[])],
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = _ctx()
    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        always_on=["query_knowledge"],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=5,
    )
    # De-duplicated list -- one entry per unique missing name.
    assert result.missing_tools.count("ghost_tool") == 1
    missing_events = [
        e for _, e in ctx.event_queue.published if isinstance(e, ExecuteMissingTool)
    ]
    assert len(missing_events) == 1
    assert missing_events[0].tool_name == "ghost_tool"


async def test_execute_preserves_first_seen_order_across_missing_tools(registry):
    """Multiple distinct missing tools are reported once each, in
    first-seen order.
    """
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="tool_a", arguments={})],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="c2", name="tool_b", arguments={})],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="c3", name="tool_a", arguments={})],
            ),
            Completion(content="ok", tool_calls=[]),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = _ctx()
    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        always_on=["query_knowledge"],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=5,
    )
    assert result.missing_tools == ["tool_a", "tool_b"]


async def test_execute_activate_then_call_succeeds_and_emits_event(registry):
    """Happy path: Execute's LLM discovers a tool the planner
    didn't list, calls ``activate_tool`` to promote it, then calls the
    activated tool successfully on the next round. The mid-run
    activation must NOT register as a missing-tool failure, and it
    must publish ``phase.tool_activated`` so the dashboard can see
    the planner under-specified the plan.
    """
    from crewlet.events.types import PhaseToolActivated

    provider = _ProviderStub(
        completions=[
            # Round 1: LLM activates ``query_knowledge`` (not in plan).
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="c1",
                        name="activate_tool",
                        arguments={"name": "query_knowledge"},
                    )
                ],
            ),
            # Round 2: LLM calls the newly-activated tool.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="c2",
                        name="query_knowledge",
                        arguments={"q": "x"},
                    )
                ],
            ),
            # Round 3: LLM finishes with plain text.
            Completion(content="all done", tool_calls=[]),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = _ctx()
    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        always_on=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=10,
    )

    # No missing-tool events — the activated tool was successfully
    # called, so the post-loop surface includes it.
    assert result.missing_tools == []
    missing_events = [
        e for _, e in ctx.event_queue.published if isinstance(e, ExecuteMissingTool)
    ]
    assert missing_events == []

    # Activation telemetry fires once for the discovered tool.
    activated = [
        e for _, e in ctx.event_queue.published if isinstance(e, PhaseToolActivated)
    ]
    assert len(activated) == 1
    assert activated[0].phase == "execute"
    assert activated[0].tool_name == "query_knowledge"

    # ``query_knowledge`` shows up in the tool-execution trace, alongside
    # the ``activate_tool`` meta-call.
    names = [exe["name"] for exe in result.tool_executions]
    assert names == ["activate_tool", "query_knowledge"]
    # The actual tool call succeeded (not Unknown-tool).
    assert all(exe["success"] for exe in result.tool_executions)
    assert result.text == "all done"


async def test_execute_mid_run_activation_propagates_to_parent_tool_names(registry):
    """Tools the executor activates mid-run must extend the
    sub-agent allowlist that the engine snapshotted before Execute
    started. Without propagation, a parent that activates X then
    spawns a sub-agent with tool_names=[X] would be rejected by
    for_subagent ('not in parent_set') — the parent's live surface
    and the sub-agent's parent allowlist disagree.
    """
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="c1",
                        name="activate_tool",
                        arguments={"name": "query_knowledge"},
                    )
                ],
            ),
            Completion(content="ok", tool_calls=[]),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = _ctx()
    # Mirror the engine's pre-Execute snapshot — see turn.py:_drive_phases.
    ctx.__dict__["spawn_subagent_config"] = {"parent_tool_names": ["search"]}
    await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        always_on=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
    )
    parent_names = ctx.__dict__["spawn_subagent_config"]["parent_tool_names"]
    # Pre-Execute snapshot entries preserved + mid-run activation appended.
    assert "search" in parent_names
    assert "query_knowledge" in parent_names


async def test_execute_activate_unknown_still_fires_missing_tool(registry):
    """When the LLM calls a name that's not in the role's catalogue at
    all (typo / hallucination), ``activate_tool`` rejects it and the
    subsequent direct call still produces an ``execute.missing_tool``
    event — Plan incompleteness is distinct from typos but both need
    visibility.
    """
    provider = _ProviderStub(
        completions=[
            # The LLM bypasses activation and just calls the unknown name.
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="ghost_tool", arguments={})],
            ),
            Completion(content="giving up", tool_calls=[]),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = _ctx()
    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        always_on=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
    )
    assert "ghost_tool" in result.missing_tools
    missing = [
        e for _, e in ctx.event_queue.published if isinstance(e, ExecuteMissingTool)
    ]
    assert any(e.tool_name == "ghost_tool" for e in missing)


# ---------------------------------------------------------------------------
# Review phase
# ---------------------------------------------------------------------------


async def test_execute_grace_fires_when_loop_exhausts_rounds(registry):
    """When Execute hits its ``max_rounds`` cap without a clean stop,
    a no-tools grace call fires asking for a plain-text summary. The
    summary becomes the ExecuteResult.text so Review has a clean
    artifact to evaluate."""
    provider = _ProviderStub(
        completions=[
            # Main loop: LLM keeps requesting ``search`` -> never finishes.
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="c2", name="search", arguments={"q": "y"})],
            ),
            # Grace call: the surface has no tools, so the LLM returns
            # a plain-text summary.
            Completion(
                content="Summary of work: searched twice, partial results.",
                tool_calls=[],
            ),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = _ctx()

    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        always_on=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=2,  # tight cap to force exhaustion
    )
    # Loop did exhaust; the grace call's summary became the artifact.
    assert result.exhausted_rounds is True
    assert "Summary of work" in result.text
    # Three LLM calls: two main rounds + one grace. The provider stub
    # falls back to a default ``Completion(content="done")`` past index 2
    # so we assert at least 3 were issued, not exactly 3.
    assert provider.calls >= 3


async def test_execute_grace_phase_event_carries_rescue_fired_flag(registry):
    """The ``AgentPhaseCompleted`` event for an exhausted Execute
    phase sets ``rescue_fired=True`` so dashboards can count
    grace-recovered phases separately."""
    captured: list[Any] = []

    class _CapturingQueue:
        async def publish(self, topic, event):
            captured.append((topic, event))

    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="wrap-up summary", tool_calls=[]),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=["search"])
    ctx = AgentContext(
        agent_id="a",
        agent_handle="eng",
        role="Engineer",
        event_queue=_CapturingQueue(),
    )
    await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        always_on=[],
        event_queue=_CapturingQueue(),
        agent_context=ctx,
        max_rounds=1,  # one round, never finishes
    )
    execute_events = [e for (_t, e) in captured if getattr(e, "phase", "") == "execute"]
    assert execute_events
    ev = execute_events[-1]
    assert ev.rescue_fired is True
    # The grace loop is merged into the phase
    # event. The main loop ran 1 round (the ``search`` tool call); the
    # grace call adds another -- ``rounds_used`` reflects both.
    assert ev.rounds_used >= 2
    # The phase event's response is the grace call's wrap-up summary,
    # not the empty main-loop tail.
    assert "wrap-up summary" in ev.response


async def test_execute_does_not_fire_grace_when_loop_terminates_cleanly(registry):
    """A clean Execute run (LLM emits a message with no tool calls)
    does NOT fire the grace call -- the existing text is already a
    valid artifact."""
    provider = _ProviderStub(
        completions=[Completion(content="I finished the task.", tool_calls=[])]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(tools_needed=[])
    ctx = _ctx()
    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        always_on=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=5,
    )
    assert result.exhausted_rounds is False
    assert result.text == "I finished the task."
    # Exactly one LLM call: the main one.
    assert provider.calls == 1


async def test_review_parses_submit_review_tool_call(registry):
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="c1",
                        name="submit_review",
                        arguments={
                            "decision": "done",
                            "notes": "All good.",
                            "final_artifact": "Here is the answer.",
                        },
                    )
                ],
            ),
            Completion(content="reviewed", tool_calls=[]),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    plan = ExecutionPlan()
    from crewlet.agent.execute import ExecuteResult

    exe = ExecuteResult(text="Here is the answer.")
    ctx = _ctx()
    outcome = await run_review_phase(
        turn=turn,
        plan=plan,
        execute_result=exe,
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
    )
    assert isinstance(outcome, ReviewOutcome)
    assert outcome.decision == "done"
    assert outcome.final_artifact == "Here is the answer."
    assert turn.model_keys["review"] == "stub"


async def test_review_defaults_to_done_when_no_tool_call(registry):
    """When neither the main review call nor the rescue produce a
    ``submit_review`` invocation, ``parse_review_from_messages``
    falls through to ``ReviewOutcome(decision="done", notes="(no
    explicit review)")``."""
    provider = _ProviderStub(
        completions=[Completion(content="looks fine to me", tool_calls=[])]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    from crewlet.agent.execute import ExecuteResult

    outcome = await run_review_phase(
        turn=turn,
        plan=ExecutionPlan(),
        execute_result=ExecuteResult(text="x"),
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=_QueueStub(),
        agent_context=_ctx(),
    )
    assert outcome.decision == "done"


async def test_review_rescue_recovers_submit_review(registry):
    """Main call ran clean without ``submit_review`` → rescue fires
    and the rescue call's ``submit_review`` produces the final
    ``ReviewOutcome``."""
    provider = _ProviderStub(
        completions=[
            # First completion: no tool calls -> triggers rescue.
            Completion(content="hm", tool_calls=[]),
            # Rescue completion: emits submit_review.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="r1",
                        name="submit_review",
                        arguments={
                            "decision": "self_iterate",
                            "notes": "context unclear",
                        },
                    )
                ],
            ),
            # Rescue follow-up: noop final completion.
            Completion(content="ok", tool_calls=[]),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    from crewlet.agent.execute import ExecuteResult

    outcome = await run_review_phase(
        turn=turn,
        plan=ExecutionPlan(),
        execute_result=ExecuteResult(text="x"),
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=_QueueStub(),
        agent_context=_ctx(),
    )
    assert outcome.decision == "self_iterate"
    assert "context unclear" in outcome.notes
    # Two LLM calls: main + rescue. The third completion is the
    # stub's safety default if the rescue tool-loop polls again.
    assert provider.calls >= 2


async def test_review_rescue_forces_submit_review_via_tool_choice(registry):
    """The Review rescue passes
    ``tool_choice="required"`` so the LLM must call the only tool in
    the narrowed surface (``submit_review``). Without this the LLM
    can ignore the directive and return prose, leaving the rescue
    ineffective."""
    captured_tool_choices: list[str | None] = []

    @dataclass
    class _CapturingProvider:
        completions: list[Completion]
        calls: int = 0
        model: str = "review-stub"

        async def complete(
            self,
            messages,
            tools=None,
            temperature=0.7,
            max_tokens=None,
            tool_choice=None,
        ):
            captured_tool_choices.append(tool_choice)
            idx = self.calls
            self.calls += 1
            if idx >= len(self.completions):
                return Completion(content="done", tool_calls=[])
            return self.completions[idx]

    provider = _CapturingProvider(
        completions=[
            # Main loop: prose, no submit_review.
            Completion(content="I'm pondering...", tool_calls=[]),
            # Rescue: forced to call submit_review.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="r",
                        name="submit_review",
                        arguments={"decision": "done"},
                    )
                ],
            ),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    from crewlet.agent.execute import ExecuteResult

    await run_review_phase(
        turn=turn,
        plan=ExecutionPlan(),
        execute_result=ExecuteResult(text="x"),
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=_QueueStub(),
        agent_context=_ctx(),
    )
    # First call is the regular review (auto); last is the rescue
    # (required).
    assert len(captured_tool_choices) >= 2
    assert captured_tool_choices[0] == "auto"
    assert captured_tool_choices[-1] == "required"


async def test_review_rescue_phase_event_carries_rescue_fired_flag(registry):
    """Telemetry: the ``AgentPhaseCompleted`` event must set
    ``rescue_fired=True`` when the rescue ran, so dashboards can
    count Review rescues separately from successful first-pass
    reviews."""
    captured: list[Any] = []

    class _CapturingQueue:
        async def publish(self, topic, event):
            captured.append((topic, event))

    provider = _ProviderStub(
        completions=[
            Completion(content="no submit", tool_calls=[]),
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="r1",
                        name="submit_review",
                        arguments={"decision": "done"},
                    )
                ],
            ),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    from crewlet.agent.execute import ExecuteResult

    ctx = AgentContext(
        agent_id="a", agent_handle="eng", role="Engineer", event_queue=_CapturingQueue()
    )
    await run_review_phase(
        turn=turn,
        plan=ExecutionPlan(),
        execute_result=ExecuteResult(text="x"),
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=_CapturingQueue(),
        agent_context=ctx,
    )
    review_events = [e for (_t, e) in captured if getattr(e, "phase", "") == "review"]
    # The rescue is internal — only the parent phase event is published.
    assert review_events, "expected a review phase event"
    assert review_events[-1].rescue_fired is True


async def test_review_rescue_merges_into_phase_event_telemetry(registry):
    """The rescue loop is a continuation of
    the phase, so its tool executions + tokens must be merged into
    the single ``AgentPhaseCompleted`` event. Without the merge the
    event would show only the exhausted main loop and the rescue's
    ``submit_review`` call would be invisible in per-phase
    telemetry (while still counted at the turn level)."""
    captured: list[Any] = []

    class _CapturingQueue:
        async def publish(self, topic, event):
            captured.append((topic, event))

    # Main loop: prose, no submit_review (one round, no tool calls).
    # Rescue loop: the forced submit_review call.
    provider = _ProviderStub(
        completions=[
            Completion(content="thinking out loud", tool_calls=[]),
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="r1",
                        name="submit_review",
                        arguments={
                            "decision": "self_iterate",
                            "notes": "stuck",
                        },
                    )
                ],
            ),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    from crewlet.agent.execute import ExecuteResult

    ctx = AgentContext(
        agent_id="a", agent_handle="eng", role="Engineer", event_queue=_CapturingQueue()
    )
    await run_review_phase(
        turn=turn,
        plan=ExecutionPlan(),
        execute_result=ExecuteResult(text="x"),
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=_CapturingQueue(),
        agent_context=ctx,
    )
    review_events = [e for (_t, e) in captured if getattr(e, "phase", "") == "review"]
    assert review_events
    ev = review_events[-1]
    # The rescue's submit_review tool call is present in the phase
    # event's tool_executions -- not lost behind the main-loop view.
    assert any(x.get("name") == "submit_review" for x in ev.tool_executions)
    # rounds_used reflects main loop + rescue loop, not just main.
    assert ev.rounds_used >= 2
    # The event's decision came from the rescue's submit_review.
    assert ev.decision == "self_iterate"
    assert ev.rescue_fired is True


# --- _format_execute_tool_log ---
#
# Production data shape from ``llm_loop.run_tool_loop``:
#   { "name": str,
#     "arguments": str,           # already json.dumps()-serialized
#     "result": str,              # error message string when success=False
#     "success": bool }
# Tests use that exact shape.


def test_format_execute_tool_log_empty_returns_explicit_none():
    """An empty tool log must say ``(none)`` so Review sees an
    affirmative "no action taken" signal — not just a missing
    section the reviewer might overlook."""
    from crewlet.agent.review import _format_execute_tool_log

    assert _format_execute_tool_log([]) == "(none)"


def test_format_execute_tool_log_renders_success_calls():
    """Successful tool calls render as
    ``- name(args) → success`` so Review can confirm Execute
    actually delivered."""
    from crewlet.agent.review import _format_execute_tool_log

    log = _format_execute_tool_log(
        [
            {
                "name": "slack_conversations_add_message",
                "arguments": '{"channel_id": "C123", "text": "hi"}',
                "result": "MsgID,...",
                "success": True,
            }
        ]
    )
    assert "slack_conversations_add_message" in log
    assert "C123" in log
    assert "→ success" in log


def test_format_execute_tool_log_marks_errors():
    """A tool result with ``success=False`` renders as ``→ error: ...``
    so Review can decide self_iterate."""
    from crewlet.agent.review import _format_execute_tool_log

    log = _format_execute_tool_log(
        [
            {
                "name": "jira_create_issue",
                "arguments": '{"project_key": "POC"}',
                "result": "permission denied",
                "success": False,
            }
        ]
    )
    assert "→ error: permission denied" in log


def test_format_execute_tool_log_flags_unknown_tool_strings():
    """The shared tool loop returns ``Unknown tool: 'x'`` for tools
    not in Execute's surface — surface that as an error in the log
    even when ``success`` isn't explicitly False."""
    from crewlet.agent.review import _format_execute_tool_log

    log = _format_execute_tool_log(
        [
            {
                "name": "slack_conversations_add_message",
                "arguments": "{}",
                "result": "Unknown tool: 'slack_conversations_add_message'.",
            }
        ]
    )
    assert "→ error" in log
    assert "Unknown tool" in log


def test_format_execute_tool_log_renders_full_arguments():
    """Argument blobs render in full -- Review must see exactly what
    each call did, so the tool log is never truncated."""
    from crewlet.agent.review import _format_execute_tool_log

    big_args = '{"text": "' + ("x" * 1000) + '"}'
    log = _format_execute_tool_log(
        [
            {
                "name": "slack_conversations_add_message",
                "arguments": big_args,
                "success": True,
            }
        ]
    )
    assert big_args in log
    assert "..." not in log


def test_format_execute_tool_log_renders_full_error_text():
    """Error results render in full too, not clipped."""
    from crewlet.agent.review import _format_execute_tool_log

    big_err = "boom " * 200
    log = _format_execute_tool_log(
        [{"name": "t", "arguments": "{}", "result": big_err, "success": False}]
    )
    assert big_err.strip() in log
    assert "..." not in log


# --- _format_plan_tool_log ---


def test_format_plan_tool_log_drops_plan_meta_tools():
    """``submit_plan`` / ``activate_tool`` / ``load_tool_skill`` are
    pure Plan-phase scaffolding — they don't tell Review anything
    about what work happened in the world.  Filter them out so the
    Plan tool log stays signal-dense."""
    from crewlet.agent.review import _format_plan_tool_log

    log = _format_plan_tool_log(
        [
            {"name": "activate_tool", "arguments": "{}", "success": True},
            {
                "name": "slack_conversations_add_message",
                "arguments": '{"channel_id": "C1"}',
                "success": True,
            },
            {"name": "load_tool_skill", "arguments": "{}", "success": True},
            {"name": "submit_plan", "arguments": "{}", "success": True},
        ]
    )
    assert "slack_conversations_add_message" in log
    assert "activate_tool" not in log
    assert "load_tool_skill" not in log
    assert "submit_plan" not in log


def test_format_plan_tool_log_empty_after_filter_returns_none():
    """An all-meta Plan log filters down to nothing — Review needs to
    see ``(none)`` so the section signals "Plan did no externally-
    visible work" rather than an absent block."""
    from crewlet.agent.review import _format_plan_tool_log

    assert (
        _format_plan_tool_log(
            [
                {"name": "activate_tool", "arguments": "{}", "success": True},
                {"name": "submit_plan", "arguments": "{}", "success": True},
            ]
        )
        == "(none)"
    )


def test_format_plan_tool_log_preserves_non_meta_calls():
    """Read-only recon (lookups, fetches) stays in the log — it's
    useful context for Review even though it isn't a side effect."""
    from crewlet.agent.review import _format_plan_tool_log

    log = _format_plan_tool_log(
        [
            {
                "name": "lookup_colleague",
                "arguments": '{"query": "CTO"}',
                "success": True,
            },
            {
                "name": "jira_get_issue",
                "arguments": '{"issue_key": "LEAD-1"}',
                "success": True,
            },
        ]
    )
    assert "lookup_colleague" in log
    assert "jira_get_issue" in log
    assert "LEAD-1" in log


def test_format_plan_tool_log_renders_failed_calls_with_error_marker():
    """A Plan-phase call that returned ``success=False`` must render
    with the ``→ error: ...`` outcome — otherwise Review can't tell
    a failed Slack post (5xx) from a successful one and would treat
    it as "already-delivered" per REVIEW_HEADER's Plan-log rule."""
    from crewlet.agent.review import _format_plan_tool_log

    log = _format_plan_tool_log(
        [
            {
                "name": "slack_conversations_add_message",
                "arguments": '{"channel_id": "C1"}',
                "result": "channel_not_found",
                "success": False,
            }
        ]
    )
    assert "slack_conversations_add_message" in log
    assert "→ error: channel_not_found" in log


def test_format_plan_tool_log_uses_shared_meta_tool_constant():
    """``_format_plan_tool_log`` filters via
    ``crewlet.agent.plan.PLAN_META_TOOL_NAMES`` so the engine-side
    delivery-override check (``turn.py``) and the prompt formatter
    (``review.py``) stay in sync.  Regression guard: hardcoding a
    second copy of the set in ``review.py`` would silently drift if
    a future change adds a 4th meta-tool in ``plan.py``."""
    from crewlet.agent.plan import PLAN_META_TOOL_NAMES

    # The three meta-tools the Plan phase constructs in
    # ``_build_meta_tools`` must all be members of the shared set.
    assert "submit_plan" in PLAN_META_TOOL_NAMES
    assert "activate_tool" in PLAN_META_TOOL_NAMES
    assert "load_tool_skill" in PLAN_META_TOOL_NAMES


# ---------------------------------------------------------------------------
# Sub-agent
# ---------------------------------------------------------------------------


async def test_subagent_rejects_tools_not_in_parent(registry):
    provider = _ProviderStub(completions=[Completion(content="research done")])
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = _ctx()
    result = await spawn_subagent(
        parent_turn=turn,
        task_prompt="Do research",
        system_prompt="You are a researcher.",
        tool_names=["search", "query_knowledge"],  # only search in parent
        parent_tool_names=["search"],
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        agent_context=ctx,
        event_queue=ctx.event_queue,
    )
    assert "query_knowledge" in result.rejected_tools
    assert "search" not in result.rejected_tools


async def test_subagent_rejects_denied_tools_even_if_parent_has_them(registry):
    registry.register(
        SimpleTool(
            name="a2a_ask",
            description="a2a",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    registry.register(
        SimpleTool(
            name="spawn_subagent",
            description="spawn",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    provider = _ProviderStub(completions=[Completion(content="ok")])
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = _ctx()
    result = await spawn_subagent(
        parent_turn=turn,
        task_prompt="x",
        system_prompt="y",
        tool_names=["a2a_ask", "spawn_subagent", "search"],
        parent_tool_names=["a2a_ask", "spawn_subagent", "search"],
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        agent_context=ctx,
        event_queue=ctx.event_queue,
    )
    assert "a2a_ask" in result.rejected_tools
    assert "spawn_subagent" in result.rejected_tools
    assert "search" not in result.rejected_tools


async def test_subagent_max_turns_clamped_to_cap(registry):
    """Parent can request less, not more, than the cap."""
    provider = _ProviderStub(completions=[Completion(content="fast finish")])
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = _ctx()
    result = await spawn_subagent(
        parent_turn=turn,
        task_prompt="x",
        system_prompt="y",
        tool_names=[],
        parent_tool_names=[],
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        agent_context=ctx,
        event_queue=ctx.event_queue,
        max_turns_cap=3,
        max_turns_request=100,
    )
    # Completed in 1 round; no way to exceed cap even when parent asked 100.
    assert result.turns_used <= 3


async def test_subagent_uses_fresh_context(registry):
    """Parent messages must not leak into the sub-agent's message list."""
    captured_messages: list[list[Message]] = []

    class _Recorder:
        model = "rec"

        async def complete(
            self,
            messages,
            tools=None,
            temperature=0.7,
            max_tokens=None,
            tool_choice=None,
        ):
            captured_messages.append(list(messages))
            return Completion(content="ok", tool_calls=[])

    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = _ctx()
    await spawn_subagent(
        parent_turn=turn,
        task_prompt="Find 10 sources.",
        system_prompt="You are a researcher.",
        tool_names=[],
        parent_tool_names=[],
        provider=_Recorder(),
        provider_key="rec",
        registry=registry,
        role_mcp_tools=[],
        agent_context=ctx,
        event_queue=ctx.event_queue,
    )
    assert captured_messages
    msgs = captured_messages[0]
    # Exactly a system + user message, nothing inherited.
    assert len(msgs) == 2
    assert msgs[0].role == "system"
    assert msgs[1].role == "user"
    assert msgs[1].content == "Find 10 sources."


async def test_subagent_system_prompt_has_mandated_preamble(registry):
    captured_system: list[str] = []

    class _Recorder:
        model = "rec"

        async def complete(
            self,
            messages,
            tools=None,
            temperature=0.7,
            max_tokens=None,
            tool_choice=None,
        ):
            for m in messages:
                if m.role == "system":
                    captured_system.append(m.content)
            return Completion(content="ok", tool_calls=[])

    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = _ctx()
    await spawn_subagent(
        parent_turn=turn,
        task_prompt="task",
        system_prompt="You are a researcher.",
        tool_names=[],
        parent_tool_names=[],
        provider=_Recorder(),
        provider_key="rec",
        registry=registry,
        role_mcp_tools=[],
        agent_context=ctx,
        event_queue=ctx.event_queue,
    )
    assert captured_system
    sysm = captured_system[0]
    assert "researcher" in sysm
    assert "Do not spawn further sub-agents" in sysm
    assert "Do not contact colleagues" in sysm


async def test_subagent_updates_parent_turn_counters(registry):
    """subagent_count and subagent_tokens on the parent grow."""
    provider = _ProviderStub(
        completions=[
            Completion(
                content="ok",
                tool_calls=[],
                input_tokens=10,
                output_tokens=5,
                tokens_used=15,
            )
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = _ctx()
    await spawn_subagent(
        parent_turn=turn,
        task_prompt="x",
        system_prompt="y",
        tool_names=[],
        parent_tool_names=[],
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        agent_context=ctx,
        event_queue=ctx.event_queue,
    )
    assert turn.subagent_count == 1
    assert turn.subagent_tokens == 15


# ---------------------------------------------------------------------------
# Partial-usage reporting on budget / timeout
# ---------------------------------------------------------------------------


async def test_subagent_reports_partial_tokens_on_budget_exhausted(registry):
    """Regression: when the fractional budget cap trips mid-run, the
    returned SubagentResult must report the tokens actually consumed
    (from the _FractionalBudgetManager's internal counter), not zero.
    """

    class _Budget:
        # Minimal duck-typed BudgetManager: pretend the parent agent
        # has a big remaining budget so the cap is picked from our
        # fraction (10 tokens).
        def __init__(self) -> None:
            self.org_budget = None

        def get_agent_budget(self, agent_id: str):
            class _B:
                max_tokens = 100
                used_tokens = 0

            return _B()

        async def spend(self, agent_id: str, tokens: int):
            from crewlet.db.budgets import SpendOutcome

            return SpendOutcome(ok=True)

    # Patch the cap directly: we wrap _FractionalBudgetManager with
    # max_subagent_tokens=10 so a round reporting 25 tokens trips it
    # after the first round (which reported 8 tokens) has committed.
    class _TwoRoundProvider:
        model = "sub"
        calls = 0

        async def complete(self, messages, tools=None, **_):
            self.calls += 1
            if self.calls == 1:
                return Completion(
                    content="",
                    tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
                    input_tokens=5,
                    output_tokens=3,
                    tokens_used=8,
                )
            return Completion(
                content="done",
                tool_calls=[],
                input_tokens=10,
                output_tokens=15,
                tokens_used=25,
            )

    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = _ctx()

    # Deterministic cap regardless of parent-budget math.
    inner = _Budget()

    # Monkey-patch _subagent_budget_cap to return our small cap so
    # spawn_subagent picks up a 10-token ceiling deterministically.
    from crewlet.agent import subagent as subagent_mod

    original_cap = subagent_mod._subagent_budget_cap
    subagent_mod._subagent_budget_cap = lambda **_: 10
    try:
        result = await spawn_subagent(
            parent_turn=turn,
            task_prompt="x",
            system_prompt="y",
            tool_names=["search"],
            parent_tool_names=["search"],
            provider=_TwoRoundProvider(),
            provider_key="sub",
            registry=registry,
            role_mcp_tools=[],
            agent_context=ctx,
            event_queue=ctx.event_queue,
            budget_manager=inner,
        )
    finally:
        subagent_mod._subagent_budget_cap = original_cap

    # The wrapper itself isn't returned; both turns_used and
    # tokens_used must be strictly > 0 (never 0) because round 1
    # completed (8 tokens, 1 round) before round 2 tripped the cap.
    assert result.timed_out is False
    assert result.text == "(sub-agent budget exhausted)"
    assert result.tokens_used > 0, "should report real tokens consumed, not 0"
    assert result.turns_used >= 1, "should report rounds completed, not 0"
    # Parent turn counters are updated with the real partial tokens.
    assert turn.subagent_count == 1
    assert turn.subagent_tokens == result.tokens_used


async def test_fractional_budget_manager_is_atomic_under_concurrency():
    """``_FractionalBudgetManager`` is shared by every
    child of a batched spawn, and those children run concurrently under
    asyncio.gather.  ``consume`` straddles an await -- without the lock,
    two children both pass the cap check against the same snapshot and
    overshoot.  The reservation must happen atomically before the await
    so the second child correctly sees the cap is reached."""
    import asyncio

    class _SlowInner:
        org_budget = None

        def get_agent_budget(self, agent_id: str):  # pragma: no cover - unused
            return None

        async def spend(self, agent_id: str, tokens: int):
            # Yield control mid-spend -- this is the await the
            # wrapper straddles; a non-atomic wrapper would let the
            # other child slip through here.
            from crewlet.db.budgets import SpendOutcome

            await asyncio.sleep(0)
            return SpendOutcome(ok=True)

    wrapper = _FractionalBudgetManager(_SlowInner(), max_subagent_tokens=1000)

    async def _child() -> str:
        try:
            await wrapper.consume("agent", 600)
            return "ok"
        except SubagentBudgetExceeded:
            return "rejected"

    results = await asyncio.gather(_child(), _child())
    # 600 + 600 = 1200 > cap 1000: exactly one child wins, the other is
    # rejected.  A non-atomic wrapper would let both through.
    assert sorted(results) == ["ok", "rejected"]
    assert wrapper._subagent_used == 600
