"""Tests for the Plan phase."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import pytest

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.plan import (
    ExecutionPlan,
    Step,
    parse_plan_from_messages,
    run_plan_phase,
)
from crewlet.agent.turn_context import TurnContext
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, Message, ToolCall
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry

# ---------------------------------------------------------------------------
# Stubs
# ---------------------------------------------------------------------------


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


@dataclass
class _ProviderStub:
    completions: list[Completion]
    calls: int = 0
    model: str = "plan-stub"

    async def complete(
        self, messages, tools=None, temperature=0.7, max_tokens=None, tool_choice=None
    ):
        idx = self.calls
        self.calls += 1
        if idx >= len(self.completions):
            return Completion(content="done", tool_calls=[])
        return self.completions[idx]


def _mk_org() -> Organization:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    return Organization(name="Acme", policies=["Be kind."], units=[unit])


def _mk_agent() -> AgentInstance:
    org = _mk_org()
    role = org.get_role("Engineer")
    assert role is not None
    defn = AgentDefinition(role=role, org=org)
    agent = AgentInstance(definition=defn, handle="eng", email="eng@acme.com")
    return agent


def _mk_turn(task: str = "Do a thing") -> TurnContext:
    agent = _mk_agent()
    ctx = TurnContext(agent=agent, org=agent.definition.org, task_description=task)
    return ctx


async def _noop(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
    return ToolResult(success=True, output="")


@pytest.fixture
def registry() -> ToolRegistry:
    r = ToolRegistry()
    r.register(
        SimpleTool(
            name="search",
            description="Search something.",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    r.register(
        SimpleTool(
            name="write_file",
            description="Write a file.",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    return r


# ---------------------------------------------------------------------------
# ExecutionPlan model
# ---------------------------------------------------------------------------


def test_execution_plan_defaults():
    plan = ExecutionPlan()
    assert plan.decision == "plan"
    assert plan.steps == []
    assert plan.tools_needed == []


def test_execution_plan_summary_for_direct_without_steps_falls_back_to_reasoning():
    """``direct`` with no steps surfaces the planner's reasoning text
    (falling back to a placeholder only when reasoning is empty)."""
    plan = ExecutionPlan(decision="direct", reasoning="Simple task.")
    assert "Simple task." in plan.summary()

    empty = ExecutionPlan(decision="direct", reasoning="")
    assert "direct" in empty.summary()


def test_execution_plan_summary_for_skip_includes_reasoning():
    """``skip`` turns short-circuit before Execute / Review, so the
    summary is consumed only by the episode publisher.  Including
    ``reasoning`` is what makes skipped episodes carry diagnostic
    value (audit, recall, compaction); without it the episode row
    is a tombstone with no usable signal beyond the trigger text."""
    plan = ExecutionPlan(
        decision="skip",
        reasoning="The trigger is addressed to <@U0TESTUSER3>, not me.",
    )
    summary = plan.summary()
    assert summary.startswith("(skip)")
    assert "addressed to <@U0TESTUSER3>" in summary


def test_execution_plan_summary_for_skip_falls_back_when_reasoning_empty():
    """Tombstone fallback when the planner skipped without reasoning
    (e.g., the parse_plan_from_messages no-submission default).  The
    label encodes the narrowed skip semantic: skip means
    'nobody was asking me', not 'no action required'."""
    plan = ExecutionPlan(decision="skip", reasoning="")
    assert plan.summary() == "(skip: not addressed to me)"


def test_execution_plan_summary_with_steps():
    plan = ExecutionPlan(
        steps=[Step(intent="Look it up"), Step(intent="Write it down")],
    )
    summary = plan.summary()
    assert "1. Look it up" in summary
    assert "2. Write it down" in summary


def test_execution_plan_summary_includes_step_approach():
    """``step.approach`` (pre-composed content from the planner) must
    flow through to Execute's plan summary, not be silently dropped."""
    plan = ExecutionPlan(
        steps=[
            Step(
                intent="Reply in Slack",
                approach="Post: 'Here are the POC tickets: POC-440, POC-29'",
            ),
        ],
        tools_needed=["slack_conversations_add_message"],
    )
    summary = plan.summary()
    assert "1. Reply in Slack" in summary
    assert "Here are the POC tickets" in summary


def test_execution_plan_summary_direct_with_steps_shows_approach():
    """When ``direct`` carries a single step with composed content,
    Execute must see that content — not a placeholder."""
    plan = ExecutionPlan(
        decision="direct",
        steps=[
            Step(
                intent="Post Slack reply",
                approach="Call slack_conversations_add_message with text='hi!'",
            ),
        ],
        tools_needed=["slack_conversations_add_message"],
    )
    summary = plan.summary()
    assert "Post Slack reply" in summary
    assert "text='hi!'" in summary
    assert "executor improvises" not in summary


# ---------------------------------------------------------------------------
# parse_plan_from_messages
# ---------------------------------------------------------------------------


def test_parse_plan_prefers_submit_plan_tool_call():
    msgs = [
        Message(role="user", content="x"),
        Message(
            role="assistant",
            content="",
            tool_calls=[
                ToolCall(
                    id="c1",
                    name="submit_plan",
                    arguments={
                        "decision": "plan",
                        "reasoning": "Use the search tool.",
                        "steps": [{"intent": "search"}],
                        "tools_needed": ["search"],
                        "success_criteria": ["found"],
                    },
                )
            ],
        ),
    ]
    plan = parse_plan_from_messages(msgs)
    assert plan.decision == "plan"
    assert plan.tools_needed == ["search"]
    assert len(plan.steps) == 1


def test_parse_plan_falls_back_to_json_text():
    msgs = [
        Message(
            role="assistant",
            content='{"decision":"direct","reasoning":"trivial"}',
        ),
    ]
    plan = parse_plan_from_messages(msgs)
    assert plan.decision == "direct"


def test_parse_plan_returns_skip_placeholder_when_absent():
    """When the planner LLM emits prose instead of calling
    ``submit_plan`` (and the prose isn't a parseable plan JSON),
    the placeholder must default to ``skip`` -- not ``direct``.

    A planner that says "I'll skip this message" in plain text but
    forgets to call ``submit_plan(decision="skip")`` must not fall
    through to ``direct`` -- Execute would then run with the full
    registry and post a reply the agent plainly didn't intend to
    send.  The safe default is to do nothing when intent is unclear;
    ``skip`` short-circuits before Execute.
    """
    msgs = [Message(role="assistant", content="no plan here")]
    plan = parse_plan_from_messages(msgs)
    assert plan.decision == "skip"
    assert "(no plan submitted)" in plan.reasoning


def test_parse_plan_fallback_is_skip():
    """Regression: when the LLM skips ``submit_plan`` and emits no
    JSON, the placeholder plan must be ``decision="skip"`` so
    TurnEngine short-circuits before Execute (and therefore before
    Review).  Anything else risks Review running on a turn with no
    success criteria, hallucinating them from the agent's role
    description, deciding ``self_iterate``, and looping the turn --
    re-firing external side effects (Slack posts, Jira comments)
    that already happened.
    """
    plan = parse_plan_from_messages([Message(role="assistant", content="nope")])
    assert plan.decision == "skip"
    # Also when there are no messages at all.
    plan2 = parse_plan_from_messages([])
    assert plan2.decision == "skip"


def test_parse_plan_ignores_extra_needs_review_field():
    """Review is mandatory on every ``plan`` / ``direct`` decision;
    ``needs_review`` is not part of the submit_plan contract.  A
    planner that sets ``needs_review`` in the submit_plan arguments
    anyway must not break validation -- Pydantic's default
    ``extra='ignore'`` drops the unknown field silently, and the plan
    parses normally instead of falling through to the skip
    placeholder."""
    msgs = [
        Message(role="user", content="x"),
        Message(
            role="assistant",
            content="",
            tool_calls=[
                ToolCall(
                    id="c1",
                    name="submit_plan",
                    arguments={
                        "decision": "plan",
                        "reasoning": "Read.",
                        "steps": [{"intent": "fetch"}],
                        "tools_needed": ["jira_get_issue"],
                        "success_criteria": ["Read"],
                        "needs_review": False,  # extra field -- ignored
                    },
                )
            ],
        ),
    ]
    plan = parse_plan_from_messages(msgs)
    assert plan.decision == "plan"
    assert plan.tools_needed == ["jira_get_issue"]
    # The unknown field doesn't appear on the model.
    assert not hasattr(plan, "needs_review")


# ---------------------------------------------------------------------------
# run_plan_phase — end-to-end
# ---------------------------------------------------------------------------


async def test_run_plan_phase_produces_execution_plan(registry):
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="c1",
                        name="submit_plan",
                        arguments={
                            "decision": "plan",
                            "reasoning": "Because of X.",
                            "steps": [{"intent": "search for it", "tools": ["search"]}],
                            "tools_needed": ["search"],
                            "success_criteria": ["found the answer"],
                        },
                    )
                ],
            ),
            Completion(content="plan submitted.", tool_calls=[]),
        ]
    )
    turn = _mk_turn("Find out X")
    ctx = AgentContext(
        agent_id=turn.agent.id_str,
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )
    plan = await run_plan_phase(
        turn=turn,
        provider=provider,
        provider_key="plan-stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
    )
    assert isinstance(plan, ExecutionPlan)
    assert plan.decision == "plan"
    assert plan.tools_needed == ["search"]
    assert turn.model_keys["plan"] == "plan-stub"


async def test_run_plan_phase_rescue_fires_when_rounds_exhausted(registry):
    """Regression guard: a planner that researches across multiple
    rounds and hits ``max_rounds`` before submitting must not fall
    through to the
    silent-skip default -- requester gets no response.  The rescue
    runs one more constrained call (submit_plan only + a directive
    offering two paths) so the planner articulates something before
    the turn ends.

    Production trace: Plan researched a missing Jira Epic over many
    rounds (jira_search, jira_get_all_projects), found the project
    was POC, ran out of rounds before submitting.  User saw silence.
    """
    # First 16 completions: research-shaped tool calls that don't
    # submit_plan (mimics the LLM doing real research and burning
    # all max_rounds).  We use a generic tool here just to fill
    # rounds; what matters is no submit_plan in any of them.
    research_completions = [
        Completion(
            content=f"<think>research round {i}</think>",
            tool_calls=[
                ToolCall(
                    id=f"r{i}",
                    name="search",
                    arguments={"query": f"q{i}"},
                )
            ],
        )
        for i in range(1, 17)  # rounds 1..16
    ]
    # The rescue then submits a plan that asks the requester to
    # clarify -- the "surface a blocker" path.
    rescue_completion = Completion(
        content="",
        tool_calls=[
            ToolCall(
                id="rescue",
                name="submit_plan",
                arguments={
                    "decision": "plan",
                    "reasoning": (
                        "Couldn't resolve Jira Epic name; asking the "
                        "requester for clarification."
                    ),
                    "steps": [
                        {
                            "intent": "Ask Sam for the project key",
                            "approach": (
                                "hey sam - quick one: which Jira project "
                                "should this go in? Couldn't find an Epic "
                                "matching 'Q2 Auth Refactor'."
                            ),
                            "tools": ["slack_conversations_add_message"],
                        }
                    ],
                    "tools_needed": ["slack_conversations_add_message"],
                    "success_criteria": ["Asked the requester via Slack"],
                },
            )
        ],
    )
    provider = _ProviderStub(completions=research_completions + [rescue_completion])
    turn = _mk_turn("Open a Jira ticket: ...")
    ctx = AgentContext(
        agent_id=turn.agent.id_str,
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )
    plan = await run_plan_phase(
        turn=turn,
        provider=provider,
        provider_key="plan-stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=16,  # explicit so it matches the production default
    )
    # The rescue produced a real plan with the originating-channel
    # reply tool -- not the silent-skip fallback.
    assert plan.decision == "plan"
    assert "slack_conversations_add_message" in plan.tools_needed
    assert "hey sam" in plan.steps[0].approach.lower()


async def test_no_submission_acknowledges_when_trigger_has_reply_destination(registry):
    # Planning never yields a submit_plan (even the rescue's forced retries
    # fail), but the request arrived on a real conversation. The turn must NOT
    # silently skip — it falls back to a one-step acknowledgment so a direct
    # ping is never met with silence.
    provider = _ProviderStub(completions=[])  # always prose, never submit_plan
    turn = _mk_turn("investigate the CI failure")
    turn.notification_metadata = {"channel_id": "C1", "thread_ts": "1.2"}
    ctx = AgentContext(
        agent_id=turn.agent.id_str,
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )
    plan = await run_plan_phase(
        turn=turn,
        provider=provider,
        provider_key="plan-stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=2,
    )
    assert plan.decision == "plan"
    assert len(plan.steps) == 1
    assert "no plan submitted" in plan.reasoning


async def test_no_submission_skips_when_no_reply_destination(registry):
    # No originating conversation (e.g. an internal trigger) → keep the silent
    # skip; there's nowhere to acknowledge to.
    provider = _ProviderStub(completions=[])
    turn = _mk_turn("internal task")  # no notification_metadata
    ctx = AgentContext(
        agent_id=turn.agent.id_str,
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )
    plan = await run_plan_phase(
        turn=turn,
        provider=provider,
        provider_key="plan-stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=2,
    )
    assert plan.decision == "skip"
    assert plan.reasoning == "(no plan submitted)"


async def test_run_plan_phase_no_rescue_when_submit_plan_already_called(registry):
    """Sanity: when the original loop already called submit_plan, the
    rescue must NOT fire -- it'd be wasted LLM cycles and could
    overwrite a perfectly good plan."""
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="ok",
                        name="submit_plan",
                        arguments={
                            "decision": "plan",
                            "reasoning": "x",
                            "steps": [{"intent": "do", "tools": ["search"]}],
                            "tools_needed": ["search"],
                        },
                    )
                ],
            ),
            Completion(content="plan submitted.", tool_calls=[]),
            # Nothing more should be requested.  If the rescue
            # incorrectly fired, the provider would raise on the
            # missing third completion.
        ]
    )
    turn = _mk_turn("Do something")
    ctx = AgentContext(
        agent_id=turn.agent.id_str,
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )
    plan = await run_plan_phase(
        turn=turn,
        provider=provider,
        provider_key="plan-stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
    )
    assert plan.decision == "plan"
    # The original loop terminated on submit_plan after one provider
    # call (terminate_after=["submit_plan"]).  No rescue call slipped
    # in -- a rescue would have driven the count to 2+.
    assert provider.calls == 1


async def test_run_plan_phase_populates_plan_tool_executions(registry):
    """``turn.plan_tool_executions`` must carry the full call records
    (name + arguments + result + success) for every Plan-phase tool
    call, not just the names that ``plan_tool_sequence`` collects.
    Review reads this list to render ``## What Plan did`` so it can
    judge against the cumulative state of the world — without it the
    reviewer would demand Execute re-do work the planner already
    delivered during recon.
    """
    provider = _ProviderStub(
        completions=[
            # Round 1: planner does in-Plan recon by promoting ``search``.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="r1",
                        name="activate_tool",
                        arguments={"name": "search"},
                    )
                ],
            ),
            # Round 2: actually call the promoted tool.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="r2",
                        name="search",
                        arguments={"query": "anything"},
                    )
                ],
            ),
            # Round 3: submit_plan.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="r3",
                        name="submit_plan",
                        arguments={
                            "decision": "plan",
                            "reasoning": "x",
                            "steps": [{"intent": "do", "tools": ["search"]}],
                            "tools_needed": ["search"],
                            "success_criteria": ["done"],
                        },
                    )
                ],
            ),
        ]
    )
    turn = _mk_turn("research then plan")
    ctx = AgentContext(
        agent_id=turn.agent.id_str,
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )
    await run_plan_phase(
        turn=turn,
        provider=provider,
        provider_key="plan-stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=5,
    )
    # plan_tool_sequence still gets the names (consumed by ReflectEngine).
    assert "activate_tool" in turn.plan_tool_sequence
    assert "search" in turn.plan_tool_sequence
    assert "submit_plan" in turn.plan_tool_sequence
    # plan_tool_executions carries the full records in the same order.
    names = [exe.get("name") for exe in turn.plan_tool_executions]
    assert names == ["activate_tool", "search", "submit_plan"]
    # Each entry must carry the call shape Review's formatter expects.
    search_exe = next(e for e in turn.plan_tool_executions if e.get("name") == "search")
    assert "arguments" in search_exe
    assert "success" in search_exe


async def test_run_plan_phase_emits_agent_phase_completed(registry):
    """Regression (dashboard visibility): each Plan invocation must
    publish one ``AgentPhaseCompleted`` event carrying the system
    prompt, user prompt, response, tool executions, tokens, and the
    structured plan decision so dashboards can render per-phase
    detail.  Without this event the dashboard had no per-phase row
    and couldn't show "what did the planner see / say / decide".
    """
    from crewlet.events.types import AgentPhaseCompleted

    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="c1",
                        name="submit_plan",
                        arguments={
                            "decision": "direct",
                            "reasoning": "trivial",
                            "steps": [],
                            "tools_needed": [],
                            "success_criteria": [],
                        },
                    )
                ],
                input_tokens=100,
                output_tokens=20,
                tokens_used=120,
            ),
        ]
    )
    turn = _mk_turn("answer the slack message")
    ctx = AgentContext(
        agent_id=turn.agent.id_str,
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )
    await run_plan_phase(
        turn=turn,
        provider=provider,
        provider_key="plan-stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
    )
    phase_events = [
        e for _, e in ctx.event_queue.published if isinstance(e, AgentPhaseCompleted)
    ]
    assert len(phase_events) == 1
    ev = phase_events[0]
    assert ev.phase == "plan"
    assert ev.turn_id == turn.turn_id
    assert ev.decision == "direct"
    assert ev.provider_key == "plan-stub"
    assert ev.input_tokens == 100
    assert ev.output_tokens == 20
    assert ev.total_tokens == 120
    # System prompt captured (non-empty) so the dashboard can show
    # what the phase was told to do.
    assert ev.system_prompt != ""
    # User prompt carries the task description.
    assert "answer the slack message" in ev.user_prompt
    # Plan phase: only meta-tool schemas go in tools=[...]; the
    # domain-tool catalogue goes in the prompt as text.  Dashboards
    # use the split to clarify that Plan does NOT ship all schemas.
    assert "submit_plan" in ev.tools_available
    assert "activate_tool" in ev.tools_available
    assert "search" not in ev.tools_available
    assert "write_file" not in ev.tools_available
    # Catalogue (prompt-text) domain tools (fixture registers search
    # + write_file).
    assert "search" in ev.tool_catalogue
    assert "write_file" in ev.tool_catalogue
    assert "submit_plan" not in ev.tool_catalogue


async def test_plan_rescue_merges_into_phase_event_telemetry(registry):
    """When the main Plan loop exhausts its
    rounds without ``submit_plan`` and the rescue recovers it, the
    rescue loop is a *continuation* of the phase -- its tool calls
    and tokens must be merged into the single ``AgentPhaseCompleted``
    event. Otherwise the rescue's ``submit_plan`` call would be
    invisible in per-phase telemetry while still counted at the turn
    level.
    """
    from crewlet.events.types import AgentPhaseCompleted

    provider = _ProviderStub(
        completions=[
            # Main loop round 1: a non-submit tool call (mimics the
            # planner researching). Keeps the loop going.
            Completion(
                content="<think>researching</think>",
                tool_calls=[
                    ToolCall(
                        id="r1",
                        name="activate_tool",
                        arguments={"name": "search"},
                    )
                ],
                input_tokens=200,
                output_tokens=40,
                tokens_used=240,
            ),
            # Main loop round 2: another non-submit call -> exhausts
            # ``max_rounds=2`` without ever calling submit_plan.
            Completion(
                content="<think>still researching</think>",
                tool_calls=[
                    ToolCall(
                        id="r2",
                        name="activate_tool",
                        arguments={"name": "write_file"},
                    )
                ],
                input_tokens=210,
                output_tokens=45,
                tokens_used=255,
            ),
            # Rescue loop: the forced submit_plan call.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="rescue",
                        name="submit_plan",
                        arguments={
                            "decision": "direct",
                            "reasoning": "rescued plan",
                            "steps": [],
                            "tools_needed": [],
                            "success_criteria": [],
                        },
                    )
                ],
                input_tokens=90,
                output_tokens=15,
                tokens_used=105,
            ),
        ]
    )
    turn = _mk_turn("research then plan")
    ctx = AgentContext(
        agent_id=turn.agent.id_str,
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )
    plan = await run_plan_phase(
        turn=turn,
        provider=provider,
        provider_key="plan-stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
        max_rounds=2,  # tight cap -> main loop exhausts
    )
    # The rescue produced a usable plan.
    assert plan.decision == "direct"

    phase_events = [
        e for _, e in ctx.event_queue.published if isinstance(e, AgentPhaseCompleted)
    ]
    assert len(phase_events) == 1
    ev = phase_events[0]
    assert ev.rescue_fired is True
    # exhausted_rounds keeps the *main* loop's signal -- paired with
    # rescue_fired it reads "main loop exhausted, rescue recovered".
    assert ev.exhausted_rounds is True
    # The rescue's submit_plan call is present in the merged
    # tool_executions, not lost behind the main-loop-only view.
    assert any(x.get("name") == "submit_plan" for x in ev.tool_executions)
    # Tokens are summed across main loop (240 + 255) + rescue (105).
    assert ev.input_tokens == 200 + 210 + 90
    assert ev.output_tokens == 40 + 45 + 15
    assert ev.total_tokens == ev.input_tokens + ev.output_tokens
    # rounds_used reflects main (2) + rescue (>=1).
    assert ev.rounds_used >= 3
    # decision came from the rescue's submit_plan.
    assert ev.decision == "direct"
    # The turn-level counters also include the rescue (these were
    # already correct pre-fix; assert the event now matches).
    assert turn.input_tokens == ev.input_tokens
    assert turn.output_tokens == ev.output_tokens


async def test_activate_tool_promotes_catalogue_tool(registry):
    """``activate_tool`` promotes the catalogue tool into ``tools=[...]``
    so subsequent Plan rounds can invoke it directly (in-Plan recon).
    The schema reaches the LLM via the next round's SDK ``tools=[...]``
    payload, not via the tool-call response body -- so the response is
    a terse confirmation, not the schema itself.  This is the activation
    contract the planner relies on -- without it the planner can never
    call a catalogue tool from Plan, only name it for Execute.
    """
    from crewlet.agent.llm_loop import execute_tool
    from crewlet.agent.plan import _build_meta_tools
    from crewlet.tools.surface import ToolSurface

    surface_holder: list[Any] = [None]
    meta_tools = _build_meta_tools(
        registry.list_tools(), [], surface_holder=surface_holder
    )
    surface = ToolSurface.for_plan(
        registry, role_mcp_tools=[], meta_tools=list(meta_tools)
    )
    surface_holder[0] = surface
    activate_tool = next(t for t in meta_tools if t.name == "activate_tool")
    ctx = AgentContext(
        agent_id="a",
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )

    # Before activation, the tool sits in the catalogue, not the active surface.
    assert surface.has("search") is False
    assert "search" in surface.catalogue_names()

    # activate_tool promotes the tool; response is a terse confirmation.
    result = await activate_tool.execute({"name": "search"}, ctx)
    assert result.success
    # Schema is delivered via tools=[...] on the next round, not the
    # response body.  Asserting the body does NOT carry the JSON
    # schema payload (no parameters block, no "type": "object") pins
    # the no-redundant-schema contract -- the tool name and a "schema
    # will appear" hint in prose are fine.
    assert '"parameters"' not in result.output
    assert '"type": "object"' not in result.output
    assert "search" in result.output
    assert "active" in result.output.lower()

    # Surface now contains the tool -- the next round can invoke it.
    assert surface.has("search") is True
    assert "search" in surface.names

    # A direct call now succeeds: dispatch resolves to the activated tool.
    direct = await execute_tool("search", {"q": "x"}, ctx, surface=surface)
    assert direct.success is True


async def test_activate_tool_idempotent_after_activation(registry):
    """Re-activating an already-active tool returns a terse nudge --
    avoids burning a round on a thrashing LLM that re-activates a tool
    it already has.
    """
    from crewlet.agent.plan import _build_meta_tools
    from crewlet.tools.surface import ToolSurface

    surface_holder: list[Any] = [None]
    meta_tools = _build_meta_tools(
        registry.list_tools(), [], surface_holder=surface_holder
    )
    surface = ToolSurface.for_plan(
        registry, role_mcp_tools=[], meta_tools=list(meta_tools)
    )
    surface_holder[0] = surface
    activate_tool = next(t for t in meta_tools if t.name == "activate_tool")
    ctx = AgentContext(
        agent_id="a",
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )

    first = await activate_tool.execute({"name": "search"}, ctx)
    assert first.success
    assert "active" in first.output.lower()

    second = await activate_tool.execute({"name": "search"}, ctx)
    assert second.success
    # Distinct already-active nudge on the second call.
    assert "ALREADY active" in second.output


async def test_activate_tool_rejects_unknown_name(registry):
    """An unknown tool name returns a clear error -- no surface mutation."""
    from crewlet.agent.plan import _build_meta_tools
    from crewlet.tools.surface import ToolSurface

    surface_holder: list[Any] = [None]
    meta_tools = _build_meta_tools(
        registry.list_tools(), [], surface_holder=surface_holder
    )
    surface = ToolSurface.for_plan(
        registry, role_mcp_tools=[], meta_tools=list(meta_tools)
    )
    surface_holder[0] = surface
    activate_tool = next(t for t in meta_tools if t.name == "activate_tool")
    ctx = AgentContext(
        agent_id="a",
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )

    result = await activate_tool.execute({"name": "nonsense"}, ctx)
    assert result.success is False
    assert "nonsense" in (result.error or "")


async def test_activate_tool_filtered_out_returns_clear_error(registry):
    """Regression: when a tool is in the role's registry universe but
    filtered out of the Plan surface by ``availability_filter`` (a
    runtime check_fn returned False), the closure must NOT claim
    activation succeeded.  Before this guard the closure's local
    ``catalogue_map`` (built from the unfiltered registry) accepted the
    name, ``surface.activate`` silently returned False, and the LLM
    was told the tool was 'now active' -- only to hit Unknown-tool on
    the next round, a thrashing attractor.
    """
    from crewlet.agent.plan import _build_meta_tools
    from crewlet.tools.surface import ToolSurface

    surface_holder: list[Any] = [None]
    meta_tools = _build_meta_tools(
        registry.list_tools(), [], surface_holder=surface_holder
    )
    # ``search`` is registered but excluded from the surface's filter.
    surface = ToolSurface.for_plan(
        registry,
        role_mcp_tools=[],
        meta_tools=list(meta_tools),
        availability_filter={"write_file"},
    )
    surface_holder[0] = surface
    activate_tool = next(t for t in meta_tools if t.name == "activate_tool")
    ctx = AgentContext(
        agent_id="a",
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )

    result = await activate_tool.execute({"name": "search"}, ctx)
    assert result.success is False
    # Error must name the tool AND say it's gated, not "not registered" --
    # the LLM needs to know it shouldn't keep retrying activation.
    assert "search" in (result.error or "")
    assert "availability" in (result.error or "").lower()
    # And the surface must not have been mutated by the failed activation.
    assert surface.has("search") is False


async def test_activate_tool_non_string_name_returns_clean_error(registry):
    """A hallucinated non-string ``name`` (list/dict) would crash
    ``catalogue_map.get`` with ``TypeError: unhashable type`` and
    propagate out of the tool loop.  The closure must reject the
    payload with a clean error instead.
    """
    from crewlet.agent.plan import _build_meta_tools
    from crewlet.tools.surface import ToolSurface

    surface_holder: list[Any] = [None]
    meta_tools = _build_meta_tools(
        registry.list_tools(), [], surface_holder=surface_holder
    )
    surface = ToolSurface.for_plan(
        registry, role_mcp_tools=[], meta_tools=list(meta_tools)
    )
    surface_holder[0] = surface
    activate_tool = next(t for t in meta_tools if t.name == "activate_tool")
    ctx = AgentContext(
        agent_id="a",
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )

    for hallucination in ([{"foo": 1}], {"key": "val"}, ["search"], 42):
        result = await activate_tool.execute({"name": hallucination}, ctx)
        assert result.success is False
        assert "non-empty string" in (result.error or "")


async def test_plan_surface_exposes_activate_tool_meta(registry):
    """Plan meta-tools include activate_tool."""
    captured_tools: list[str] = []

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
                captured_tools.append(td.name)
            return Completion(content='{"decision":"direct"}', tool_calls=[])

    turn = _mk_turn()
    ctx = AgentContext(
        agent_id=turn.agent.id_str,
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )
    await run_plan_phase(
        turn=turn,
        provider=_Recorder(),
        provider_key="rec",
        registry=registry,
        role_mcp_tools=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
    )
    assert "activate_tool" in captured_tools
    assert "submit_plan" in captured_tools
    # Domain tools are in the catalogue (prompt) -- NOT in tools=[...]
    assert "search" not in captured_tools
    assert "write_file" not in captured_tools


async def test_plan_system_prompt_includes_catalogue(registry):
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
            return Completion(content='{"decision":"direct"}', tool_calls=[])

    turn = _mk_turn()
    ctx = AgentContext(
        agent_id=turn.agent.id_str,
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )
    await run_plan_phase(
        turn=turn,
        provider=_Recorder(),
        provider_key="rec",
        registry=registry,
        role_mcp_tools=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
    )
    assert captured_system
    assert "Available tools" in captured_system[0]
    assert "search:" in captured_system[0]
    assert "write_file:" in captured_system[0]


async def test_plan_rescue_forces_submit_plan_via_tool_choice(registry):
    """The rescue passes ``tool_choice="required"``
    so the LLM is FORCED to call the only tool in the narrowed
    surface (``submit_plan``). Without this the LLM can ignore the
    directive and return prose, leaving the rescue ineffective.
    """
    captured_tool_choices: list[str | None] = []

    @dataclass
    class _ChoiceCapturingStub:
        completions: list[Completion]
        calls: int = 0
        model: str = "plan-stub"

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

    # Main loop: LLM ignores submit_plan, just emits prose.  Rescue
    # must therefore fire.  Rescue's call must use
    # ``tool_choice="required"``.
    main_completion = Completion(content="I'm thinking about it...", tool_calls=[])
    rescue_completion = Completion(
        content="",
        tool_calls=[
            ToolCall(
                id="rescue",
                name="submit_plan",
                arguments={
                    "decision": "skip",
                    "reasoning": "Couldn't formulate a plan",
                    "steps": [],
                    "tools_needed": [],
                    "success_criteria": [],
                },
            )
        ],
    )
    provider = _ChoiceCapturingStub(completions=[main_completion, rescue_completion])

    turn = _mk_turn("do something")
    ctx = AgentContext(
        agent_id=turn.agent.id_str,
        agent_handle="eng",
        role="Engineer",
        event_queue=_QueueStub(),
    )
    await run_plan_phase(
        turn=turn,
        provider=provider,
        provider_key="plan-stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=ctx.event_queue,
        agent_context=ctx,
    )
    # Two calls: the main loop (tool_choice=auto -- default) and the
    # rescue (tool_choice="required" -- new). The "auto" comes from
    # the existing ``run_tool_loop`` default when tool_defs is non-
    # empty.
    assert len(captured_tool_choices) >= 2
    # Last call is the rescue.
    assert captured_tool_choices[-1] == "required"
    # First call is the regular planner -- not forced.
    assert captured_tool_choices[0] == "auto"
