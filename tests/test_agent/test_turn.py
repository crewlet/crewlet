"""Tests for the TurnEngine orchestrator."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.execute import ExecuteResumeState
from crewlet.agent.instance import AgentInstance
from crewlet.agent.plan import ExecutionPlan
from crewlet.agent.turn import TurnEngine
from crewlet.events.types import (
    AgentPhaseCompleted,
    AgentTurnCompleted,
    Event,
    TaskCompleted,
    TaskFailed,
    TaskStarted,
    TurnGuardBreach,
)
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

    async def subscribe(self, topic, group, handler):
        pass


class _PhaseScriptedProvider:
    """Provider that returns different canned sequences per phase.

    Routes by looking for "PLAN phase" / "EXECUTE phase" / "REVIEW phase"
    strings in the system prompt.
    """

    def __init__(
        self,
        *,
        plan: list[Completion],
        execute: list[Completion],
        review: list[Completion],
        model: str = "phased",
    ) -> None:
        self._plan = list(plan)
        self._execute = list(execute)
        self._review = list(review)
        self.model = model

    async def complete(
        self, messages, tools=None, temperature=0.7, max_tokens=None, tool_choice=None
    ):
        sys_text = ""
        for m in messages:
            if m.role == "system":
                sys_text = m.content
                break
        if "PLAN phase" in sys_text:
            return self._plan.pop(0) if self._plan else Completion(content="plan done")
        if "EXECUTE phase" in sys_text:
            return (
                self._execute.pop(0)
                if self._execute
                else Completion(content="exec done")
            )
        if "REVIEW phase" in sys_text:
            return (
                self._review.pop(0)
                if self._review
                else Completion(content="review done")
            )
        return Completion(content="?", tool_calls=[])


async def _noop(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
    return ToolResult(success=True, output="noop")


def _mk_org(roles_llm: dict[str, str] | None = None) -> Organization:
    kwargs = {"name": "Engineer", "handle": "eng"}
    if roles_llm:
        kwargs.update(roles_llm)
    role = Role(**kwargs)
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    return Organization(name="Acme", units=[unit])


def _mk_agent(role_kwargs: dict[str, Any] | None = None) -> AgentInstance:
    org = _mk_org(role_kwargs)
    role = org.get_role("Engineer")
    assert role is not None
    defn = AgentDefinition(role=role, org=org)
    agent = AgentInstance(definition=defn, handle="eng", email="e@acme.com")
    agent.activate()  # CREATED -> IDLE so start_working(task_id) succeeds
    return agent


def _mk_registry() -> ToolRegistry:
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
            description="query",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    return r


def _plan_submission(
    tools_needed: list[str] | None = None,
    decision: str = "plan",
) -> list[Completion]:
    """One plan round. ``terminate_after=["submit_plan"]`` in the plan
    runner stops the loop right after this call succeeds, so no
    explicit terminator completion is needed.
    """
    return [
        Completion(
            content="",
            tool_calls=[
                ToolCall(
                    id="cp",
                    name="submit_plan",
                    arguments={
                        "decision": decision,
                        "reasoning": "Because.",
                        "steps": [{"intent": "search"}] if decision == "plan" else [],
                        "tools_needed": tools_needed or ["search"],
                        "success_criteria": ["done"],
                    },
                )
            ],
        ),
    ]


def _review_submission(decision: str, **kwargs) -> list[Completion]:
    args = {"decision": decision, "notes": ""}
    args.update(kwargs)
    return [
        Completion(
            content="",
            tool_calls=[ToolCall(id="cr", name="submit_review", arguments=args)],
        ),
    ]


def _flat(*chunks: list[Completion]) -> list[Completion]:
    out: list[Completion] = []
    for c in chunks:
        out.extend(c)
    return out


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


async def test_turn_engine_drives_all_three_phases():
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["search"]),
        # Execute must actually call ``search`` so the delivery-safety
        # net stays out of the way; we want this test exercising the
        # well-formed Plan → Execute → Review pipeline.
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="I searched and found X.", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="Final: X"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    result = await engine.run_turn(
        agent,
        task_id="t1",
        task_description="Find X",
        org=agent.definition.org,
    )
    assert result == "Final: X"
    # Task events published.
    event_types = [e.type for _, e in queue.published]
    assert "task_started" in event_types
    assert "task_completed" in event_types
    assert "agent_turn_completed" in event_types


async def test_turn_engine_shares_role_mcp_tools_dict_by_reference():
    """The TurnEngine must hold the SAME ``role_mcp_tools`` dict the
    engine passed -- not a fresh copy.  Per-entity bootstrap builds
    the TurnEngine when the first LLM provider arrives, BEFORE any
    roles exist, so the dict is empty at construction time.  A naive
    ``role_mcp_tools or {}`` swaps in a brand-new dict for the empty
    case (empty dict is falsy), and per-role MCP tools added later by
    the engine's in-place mutation would never reach the running
    TurnEngine -- the agent would see ``list_mcp_server_tools`` return
    ``(none)`` despite the servers having spawned.
    """
    shared: dict[str, list[Any]] = {}
    engine = TurnEngine(
        llm_providers={
            "default": _PhaseScriptedProvider(plan=[], execute=[], review=[])
        },
        tool_registry=_mk_registry(),
        event_queue=_QueueStub(),
        role_mcp_tools=shared,
    )
    # Same object, not a copy.
    assert engine._role_mcp_tools is shared
    # In-place mutation by the engine is visible to the TurnEngine.
    shared["Agent CTO"] = ["slack_tool", "jira_tool"]
    assert engine._role_mcp_tools["Agent CTO"] == ["slack_tool", "jira_tool"]


async def test_turn_engine_defaults_role_mcp_tools_to_empty_dict():
    """When no ``role_mcp_tools`` is passed (``None``), the TurnEngine
    still gets a usable empty dict rather than ``None``."""
    engine = TurnEngine(
        llm_providers={
            "default": _PhaseScriptedProvider(plan=[], execute=[], review=[])
        },
        tool_registry=_mk_registry(),
        event_queue=_QueueStub(),
    )
    assert engine._role_mcp_tools == {}


async def test_turn_engine_runs_review_on_every_plan_decision():
    """Review is mandatory on every ``plan`` decision -- the
    historical ``needs_review=False`` opt-out has been removed
    (failure mode: silent half-finished turns when the planner
    mis-judged a recon task as a mechanical one-shot, e.g. the
    LEAD-1 / agent-ceo production trace).  This test pins the
    invariant: Plan → Execute → Review always runs to completion
    on a ``plan`` decision, and Review's ``final_artifact`` is
    what propagates.
    """
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["search"]),
        # Execute calls the planned tool and emits a final text.
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="hello to slack", tool_calls=[]),
        ],
        # Review owns the final artifact.
        review=_review_submission("done", final_artifact="reviewed: hello to slack"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    result = await engine.run_turn(
        agent,
        task_description="reply on Slack",
        org=agent.definition.org,
    )
    assert result == "reviewed: hello to slack"
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed
    assert completed[0].decision == "done"
    assert completed[0].iterations == 1
    # ``review_model`` is populated because Review actually ran -- the
    # signal a dashboard would use to count which turns paid for Review.
    assert completed[0].review_model != ""


async def test_turn_engine_forces_review_on_direct_when_delivery_missing():
    """Safety net for ``direct`` plans: ``decision="direct"`` skips
    Review by default (no explicit success criteria to judge against
    -- letting Review run invites hallucinated criteria → self_iterate
    → re-firing of external side effects).  BUT when the direct plan
    listed action tools in ``tools_needed`` AND Execute called NONE
    of them, the LLM almost certainly wrote a draft as text without
    delivering it.  The engine forces Review in that case so the
    miss gets caught.  On the next iteration the agent calls the tool
    properly and the turn ends with ``done``.

    ``ExecutionPlan`` carries no ``needs_review`` opt-out, so this
    test covers the only path into the ``skip_review`` branch:
    ``decision="direct"`` with action tools and no delivery.
    """
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_flat(
            _plan_submission(["search"], decision="direct"),
            _plan_submission(["search"], decision="direct"),
        ),
        execute=[
            # Iter 1 — wrote a draft as text without calling search.
            Completion(content="I would reply but can't", tool_calls=[]),
            # Iter 2 — actually call search this time.
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="now actually delivered", tool_calls=[]),
        ],
        # Iter 1's Review fires (forced by the no-delivery safety
        # net) and naively says "done" — the engine override flips
        # it to self_iterate.  Iter 2's Review doesn't fire (the
        # tool was called → ``skip_review`` honoured for direct).
        review=_review_submission("done", final_artifact="review-caught-it"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    result = await engine.run_turn(
        agent, task_description="reply", org=agent.definition.org
    )
    # Iter 2's Execute artifact is the final answer because the
    # delivery tool actually ran.
    assert result == "now actually delivered"
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed
    # Two iterations proves both safety-net layers engaged: forced
    # Review on iter 1, decision overridden to self_iterate, then
    # iter 2 actually called the tool.
    assert completed[0].iterations == 2


async def test_turn_engine_skips_review_when_decision_is_direct():
    """``decision="direct"`` skips Review by design: a direct plan
    has no explicit success criteria for Review to judge against,
    and letting Review run invites it to hallucinate criteria from
    the agent's role description, decide ``self_iterate``, and loop
    the turn -- re-firing external side effects (Slack posts, Jira
    comments) that already happened.  ``direct`` is the only branch
    that still skips Review now that the ``needs_review`` opt-out
    has been removed.
    """
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["search"], decision="direct"),
        # Execute calls the planned tool then emits the final text —
        # well-formed direct path, no delivery miss.
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="reply sent", tool_calls=[]),
        ],
        review=[],
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    result = await engine.run_turn(
        agent,
        task_description="reply on Slack",
        org=agent.definition.org,
    )
    assert result == "reply sent"
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed and completed[0].decision == "done"
    assert completed[0].iterations == 1
    assert completed[0].review_model == ""


async def test_turn_engine_counts_plan_phase_delivery_when_judging_done():
    """The delivery safety net flips Review's "done" to
    ``self_iterate`` when the action tools listed in ``tools_needed``
    weren't actually called — but it must accept calls from BOTH
    Plan and Execute as delivery.  Production failure: the planner
    posted the Slack reply during in-Plan recon, Execute didn't
    repeat (correctly avoiding a duplicate), Review judged ``done``
    against the cumulative state — and the engine override flipped
    to ``self_iterate`` because it only looked at Execute's tool log.
    The loop then re-fired the same Slack post.
    """
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=[
            # Plan promotes ``search`` and then calls it during recon
            # — this is the side effect Execute should NOT repeat.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="p1",
                        name="activate_tool",
                        arguments={"name": "search"},
                    )
                ],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="p2", name="search", arguments={"q": "x"})],
            ),
            # Now submit the plan declaring ``search`` as the needed
            # action tool.  Execute's surface will include it, but
            # Execute correctly chooses not to re-fire.
            *_plan_submission(["search"], decision="plan"),
        ],
        # Execute writes a wrap-up without calling ``search`` again.
        execute=[Completion(content="already done by planner", tool_calls=[])],
        # Review judges ``done`` against the cumulative state.
        review=_review_submission("done", final_artifact="confirmed"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    result = await engine.run_turn(
        agent, task_description="reply", org=agent.definition.org
    )
    # The override did NOT flip ``done`` → ``self_iterate`` because
    # Plan-phase delivery counted.  Single iteration; final artifact
    # is Review's, not a re-run.
    assert result == "confirmed"
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed and completed[0].decision == "done"
    assert completed[0].iterations == 1


async def test_turn_engine_safety_net_for_direct_plan_uses_execute_only_delivery():
    """For ``decision="direct"`` plans, the planner committed to
    Execute doing the work in one shot — Plan-phase delivery does NOT
    count toward the ``skip_review`` safety net.  Otherwise a planner
    that called a read-only recon tool listed in ``tools_needed``
    (e.g. ``search`` for lookup before reply) would satisfy
    ``delivered`` by name-overlap even when Execute genuinely skipped
    the write.

    Production failure pattern: ``tools_needed=["search","slack_post"]``,
    Plan calls ``search`` during recon, Execute writes a draft reply
    as text without ``slack_post``, the union-based check would say
    ``delivered=True``, Review skipped, silent no-op turn.
    """
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=[
            # Plan promotes ``search`` and calls it during recon
            # (the read-only lookup), then submits a DIRECT plan.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="p1",
                        name="activate_tool",
                        arguments={"name": "search"},
                    )
                ],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="p2", name="search", arguments={"q": "x"})],
            ),
            *_plan_submission(["search"], decision="direct"),
        ],
        # Execute writes a draft as text without calling ``search``.
        execute=[Completion(content="I would reply but didn't", tool_calls=[])],
        # When the safety net forces Review, Review judges done.
        # Override then catches via union delivered=True (Plan
        # called search) — but THIS test verifies the safety net
        # uses Execute-only, so Review WAS forced (not skipped).
        review=_review_submission("done", final_artifact="caught"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    await engine.run_turn(agent, task_description="reply", org=agent.definition.org)
    # If the safety net had used the union (the pre-fix behavior),
    # Plan's ``search`` call would have satisfied delivery, Review
    # would have been skipped, no review event would fire.  With
    # the Execute-only fix Review IS forced — a review_model is set.
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed
    assert completed[0].review_model != ""


async def test_turn_engine_does_not_count_failed_plan_calls_as_delivery():
    """A Plan-phase tool call that returned ``success=False`` (Slack
    5xx, Jira permission denied, etc.) must NOT count toward
    delivery.  Otherwise a failed Plan post followed by Execute
    writing text-only produces a silent no-op turn — Review is
    skipped (or its ``done`` decision stands) because the engine
    treats the failed call as delivery on name-overlap alone.
    """
    from crewlet.tools.protocol import ToolResult

    agent = _mk_agent()

    # Custom registry where ``search`` fails — emulates a Plan-phase
    # write that returned an error (success=False reaches the loop's
    # tool_executions and lands in turn.plan_tool_executions).
    async def _failing(params: dict, ctx) -> ToolResult:
        return ToolResult(success=False, error="permission denied")

    registry = _mk_registry()
    # Replace the existing search tool with a failing one.
    registry._tools.pop("search", None)
    registry.register(
        SimpleTool(
            name="search",
            description="search",
            parameters={"type": "object"},
            fn=_failing,
        )
    )

    provider = _PhaseScriptedProvider(
        plan=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="p1",
                        name="activate_tool",
                        arguments={"name": "search"},
                    )
                ],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="p2", name="search", arguments={"q": "x"})],
            ),
            *_plan_submission(["search"], decision="plan"),
        ],
        # Execute writes a reply without calling search.
        execute=[Completion(content="text-only reply", tool_calls=[])],
        # Review (LLM) hallucinates ``done``.
        review=_review_submission("done", final_artifact="should-be-flipped"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=registry,
        event_queue=queue,
    )
    # Failed Plan call + no Execute delivery + Review says done →
    # override MUST flip to self_iterate.  If success filtering is
    # missing, delivered=True (the failed call name matches
    # tools_needed) and the override silently passes the bad
    # ``done`` — exactly the silent-failure mode the override exists
    # to catch.  With multiple iterations and the same failure each
    # time, we'd loop; capping iterations at 1 lets us assert the
    # ``done`` decision was overridden.
    engine._settings.set(
        engine._settings.get().model_copy(update={"max_iterations": 1})
    )
    await engine.run_turn(agent, task_description="reply", org=agent.definition.org)
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed
    # Single iteration capped; the override flipped done → self_iterate;
    # the iteration cap then terminates the turn as failed.
    assert completed[0].decision != "done"


async def test_turn_engine_resets_plan_tool_executions_per_iteration():
    """``turn.plan_tool_executions`` must be scoped to the current
    iteration.  Otherwise iter-N's delivery check sees stale Plan
    calls from iter-(N-1) and treats them as fresh delivery —
    causing a genuine iter-N delivery gap to be silently shipped.

    Scenario: iter-1 Plan calls ``search``, Review self_iterates
    (unrelated reason). Iter-2: Plan does NOT call search; Execute
    doesn't either; Review says ``done``. The override MUST catch
    the gap (Execute produced text without delivering) — it would
    miss it if turn.plan_tool_executions still held iter-1's call.
    """
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=[
            # Iter-1 Plan: activate + call search + submit_plan.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="p1a",
                        name="activate_tool",
                        arguments={"name": "search"},
                    )
                ],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="p1b", name="search", arguments={"q": "x"})],
            ),
            *_plan_submission(["search"], decision="plan"),
            # Iter-2 Plan: no recon, just submit.
            *_plan_submission(["search"], decision="plan"),
        ],
        execute=[
            # Iter-1: text-only (Review will say self_iterate
            # because of the unrelated note).
            Completion(content="iter-1 draft", tool_calls=[]),
            # Iter-2: still text-only — the genuine delivery gap.
            Completion(content="iter-2 draft", tool_calls=[]),
        ],
        review=[
            *_review_submission("self_iterate", notes="add more detail"),
            *_review_submission("done", final_artifact="should-be-flipped"),
        ],
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    # Cap iterations so the override-flipped second pass terminates
    # the turn rather than looping forever on the same failure.
    engine._settings.set(
        engine._settings.get().model_copy(update={"max_iterations": 2})
    )
    await engine.run_turn(agent, task_description="reply", org=agent.definition.org)
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed
    # If the reset works, iter-2's plan_tool_executions is empty,
    # Execute called nothing, delivered=False → override fires.
    # The final decision is NOT "done" (overridden to self_iterate
    # → loop exhausts iterations).  If the reset is missing, iter-1's
    # search call would still be present in iter-2's check,
    # delivered=True, override doesn't fire, decision stays "done".
    assert completed[0].decision != "done"
    assert completed[0].iterations == 2


async def test_turn_engine_delivery_gate_ignores_phantom_tools_needed():
    """A ``tools_needed`` entry naming a tool that does NOT exist in the
    role's catalogue must not drive the delivery gate.

    The planner only sees MCP *server* names and is expected to guess
    MCP tool names in ``tools_needed``; Execute recovers by discovering
    and activating the real tool.  Production failure: the planner named
    ``confluence_add_footer_comment`` (the name the prompts teach) but
    the deployed mcp-atlassian exposes ``confluence_add_comment``.
    Execute activated + called the real tool and Review judged ``done``,
    yet the exact-match delivery gate saw no call to the phantom name,
    flipped ``done`` → ``self_iterate``, and the re-plan double-posted
    the comment.  With the catalogue filter the phantom name is ignored,
    the override stays out, and the turn completes in one pass with the
    side effect fired exactly once.
    """
    agent = _mk_agent()

    posted: list[dict[str, Any]] = []

    async def _record(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
        posted.append(params)
        return ToolResult(success=True, output="comment added")

    # The REAL tool Execute discovers is a server-backed MCP tool (it
    # carries ``server_name``), exactly as ``MCPToolWrapper`` exposes it.
    class _McpTool(SimpleTool):
        @property
        def server_name(self) -> str:
            return "atlassian"

    registry = _mk_registry()
    registry.register(
        _McpTool(
            name="confluence_add_comment",
            description="Add a comment to a Confluence page.",
            parameters={"type": "object"},
            fn=_record,
        )
    )

    provider = _PhaseScriptedProvider(
        # Planner guesses the wrong (non-existent) tool name.
        plan=_plan_submission(["confluence_add_footer_comment"], decision="plan"),
        execute=[
            # Execute discovers + activates the REAL tool, then calls it.
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="e1",
                        name="activate_tool",
                        arguments={"name": "confluence_add_comment"},
                    )
                ],
            ),
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="e2",
                        name="confluence_add_comment",
                        arguments={"page_id": "1", "body": "ack"},
                    )
                ],
            ),
            Completion(content="acknowledgement posted", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="ack posted"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=registry,
        event_queue=queue,
    )
    # Cap at one iteration: if the (pre-fix) override fired it would
    # flip ``done`` → ``self_iterate`` and the cap would terminate the
    # turn as non-done — so a passing ``done`` assertion proves the
    # override stayed out rather than being masked by a later pass.
    engine._settings.set(
        engine._settings.get().model_copy(update={"max_iterations": 1})
    )
    result = await engine.run_turn(
        agent, task_description="ack the page", org=agent.definition.org
    )

    assert result == "ack posted"
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed and completed[0].decision == "done"
    assert completed[0].iterations == 1
    # The real delivery tool fired exactly once — no duplicate side effect.
    assert len(posted) == 1


async def test_turn_phantom_only_plan_with_no_delivery_is_not_silently_done():
    """Regression: a plan whose only delivery tool is a wrong guess and
    that calls NOTHING must not complete silently as ``done``.

    Reported failure: an agent asked to reply "hi" planned
    ``decision="direct"`` with
    ``tools_needed=["slack_conversations_postMessage"]`` (a guess — the
    deployed Slack server exposes ``slack_conversations_add_message``).
    Execute composed the reply as *text* and called no tool.  Without
    the phantom-tool check, the gate would read "no action expected"
    and the turn would finish as ``done`` — the reply never reaching
    Slack.  The gate keys intent off the raw ``tools_needed`` (phantom
    included) and forces Review / ``self_iterate`` when nothing
    substantive was delivered.
    """
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        # Planner names a phantom Slack tool and commits to a direct reply.
        plan=_plan_submission(["slack_conversations_postMessage"], decision="direct"),
        # Execute composes the reply as text and calls NO tool.
        execute=[Completion(content="Hi Sam!", tool_calls=[])],
        # Review is forced to run (skip_review flipped); its "done" is
        # overridden to self_iterate because nothing was delivered.
        review=_review_submission("done", final_artifact="Hi Sam!"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    engine._settings.set(
        engine._settings.get().model_copy(update={"max_iterations": 1})
    )
    await engine.run_turn(agent, task_description="<@BOT> hi", org=agent.definition.org)

    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed
    # Must NOT silently complete as a delivered "done": the phantom-aware
    # gate forces Review, whose "done" is overridden to self_iterate;
    # capped at one iteration the turn ends non-done.
    assert completed[0].decision != "done"


async def test_turn_phantom_plan_builtin_call_does_not_mask_non_delivery():
    """A builtin call must not satisfy the phantom-guess delivery
    fallback.

    The planner named a phantom Slack tool and Execute called a
    *builtin* (here ``search`` — a non-server-backed registry tool, a
    stand-in for ``lookup_colleague`` recon) but never delivered. A
    builtin is not a delivery to a shared surface, so the turn must NOT
    read as delivered — otherwise a recon call would mask the same
    non-delivery the phantom-aware gate exists to catch.
    """
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["slack_conversations_postMessage"], decision="direct"),
        execute=[
            # Calls a builtin-style tool (no server_name), then text.
            Completion(
                content="",
                tool_calls=[ToolCall(id="e1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="Hi Sam!", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="Hi Sam!"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    engine._settings.set(
        engine._settings.get().model_copy(update={"max_iterations": 1})
    )
    await engine.run_turn(agent, task_description="<@BOT> hi", org=agent.definition.org)

    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed
    # The builtin call did not count as delivery → forced Review → not done.
    assert completed[0].decision != "done"


async def test_turn_phantom_plan_known_read_does_not_mask_non_delivery():
    """A server-backed *read* tool (annotated ``read_only``) called during
    a phantom-guess turn must not count as a delivery — even when its
    annotations live on the tool instance (a shared MCP wrapper) rather
    than the registry side-table."""
    from crewlet.tools.capabilities import ToolAnnotations

    agent = _mk_agent()

    class _ReadTool(SimpleTool):
        @property
        def server_name(self) -> str:
            return "atlassian"

        @property
        def annotations(self) -> ToolAnnotations:
            return ToolAnnotations(read_only=True)

    registry = _mk_registry()
    registry.register(
        _ReadTool(
            name="jira_get_issue",
            description="Read a Jira issue.",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["slack_conversations_postMessage"], decision="direct"),
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="e1", name="jira_get_issue", arguments={})],
            ),
            Completion(content="Hi!", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="Hi!"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=registry,
        event_queue=queue,
    )
    engine._settings.set(
        engine._settings.get().model_copy(update={"max_iterations": 1})
    )
    await engine.run_turn(agent, task_description="<@BOT> hi", org=agent.definition.org)
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed
    # A read didn't deliver → forced Review → not silently done.
    assert completed[0].decision != "done"


async def test_turn_emits_phase_completed_events_per_phase():
    """Regression (dashboard visibility): a full Plan -> Execute ->
    Review turn must publish exactly one ``AgentPhaseCompleted``
    per phase, in order, so dashboards can draw a per-phase timeline.

    The per-phase record is what carries the system prompt, tool
    executions, and structured decision -- without it, "what did Plan
    decide / what did Execute run / what did Review say" would be
    invisible.
    """
    from crewlet.events.types import AgentPhaseCompleted

    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["search"], decision="plan"),
        # Execute must actually call ``search`` (the tool the Plan
        # listed in ``tools_needed``) — otherwise the engine's
        # delivery-safety net flips Review's "done" to
        # ``self_iterate`` and the turn loops, making this test
        # double-fire the per-phase events.  Calling the tool
        # exercises the well-formed Plan → Execute → Review path
        # this test is for.
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="execute output", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="reviewed"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    await engine.run_turn(agent, task_description="real work", org=agent.definition.org)

    phase_events = [e for _, e in queue.published if isinstance(e, AgentPhaseCompleted)]
    assert [e.phase for e in phase_events] == ["plan", "execute", "review"]
    # Each event carries the turn id + iteration + system prompt so
    # the dashboard can group by turn and show per-phase context.
    for ev in phase_events:
        assert ev.turn_id
        assert ev.iteration == 1
        assert ev.system_prompt != ""
    # Plan carries the plan decision, Review carries the review
    # decision, Execute has none.
    assert phase_events[0].decision == "plan"
    assert phase_events[1].decision == ""
    assert phase_events[2].decision == "done"


async def test_turn_continues_under_given_turn_id_with_iteration_offset():
    """A follow-on turn (the detached sandbox completion) can run UNDER an
    earlier turn's id with its iteration labels offset past the kick-off's,
    so its phases group with — and order after — the originating turn in the
    dashboard instead of appearing as a disconnected new turn.
    """
    from crewlet.events.types import AgentPhaseCompleted

    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["search"], decision="plan"),
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="execute output", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="reviewed"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    await engine.run_turn(
        agent,
        task_description="report the finished job",
        org=agent.definition.org,
        turn_id="kickoff-turn",
        start_iteration=1,  # kick-off already emitted iteration 1
    )

    phase_events = [e for _, e in queue.published if isinstance(e, AgentPhaseCompleted)]
    assert phase_events
    for ev in phase_events:
        # Same turn id as the kick-off → one dashboard group.
        assert ev.turn_id == "kickoff-turn"
        # Iteration label offset past the kick-off's iteration 1.
        assert ev.iteration == 2


async def test_turn_resumes_suspended_execute_skipping_plan():
    """A resume turn skips Plan and continues the
    suspended Execute loop with the sandbox result spliced in, then runs
    Review — all under the kick-off turn_id at the offset iteration.
    """
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        # Plan must NOT be consumed on a resume turn.
        plan=[],
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="e1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="reported the findings", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="reviewed"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    resume = ExecuteResumeState(
        plan=ExecutionPlan(
            tools_needed=["search"], decision="plan", success_criteria=["done"]
        ),
        result_content="findings from the sandbox: pytest-cov missing",
        messages=[
            Message(role="system", content="EXECUTE phase").model_dump(mode="json"),
            Message(role="user", content="Task: fix CI").model_dump(mode="json"),
            Message(
                role="assistant",
                content="",
                tool_calls=[ToolCall(id="c1", name="run_sandbox", arguments={})],
            ).model_dump(mode="json"),
        ],
        pending_tool_call_id="c1",
        pending_tool_name="run_sandbox",
        active_tool_names=["search"],
    )
    result = await engine.run_turn(
        agent,
        task_id="kick",
        task_description="fix CI",
        org=agent.definition.org,
        turn_id="kick",
        start_iteration=1,
        resume_state=resume,
    )

    assert result == "reviewed"
    phase_events = [e for _, e in queue.published if isinstance(e, AgentPhaseCompleted)]
    phases = [e.phase for e in phase_events]
    # Plan was skipped; only Execute + Review ran, under the kick-off id.
    assert "plan" not in phases
    assert "execute" in phases and "review" in phases
    for ev in phase_events:
        assert ev.turn_id == "kick"
        assert ev.iteration == 2  # offset past the kick-off's iteration 1


async def test_turn_progress_events_carry_matching_turn_coordinates():
    """Live-stream correlation (dashboard agent page): the per-round
    ``AgentTurnProgress`` events a turn emits must carry the same
    ``turn_id`` / ``phase`` / ``iteration`` coordinates as the
    ``AgentPhaseCompleted`` events, so an in-flight round can be
    placed inside the right turn/phase grouping before its phase
    record exists.
    """
    from crewlet.events.types import AgentPhaseCompleted, AgentTurnProgress

    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["search"], decision="plan"),
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="execute output", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="reviewed"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    await engine.run_turn(agent, task_description="real work", org=agent.definition.org)

    phase_events = [e for _, e in queue.published if isinstance(e, AgentPhaseCompleted)]
    progress = [e for _, e in queue.published if isinstance(e, AgentTurnProgress)]
    assert phase_events and progress
    turn_id = phase_events[0].turn_id
    for ev in progress:
        assert ev.turn_id == turn_id
        assert ev.iteration == 1
        assert ev.phase in ("plan", "execute", "review")
        assert ev.role == agent.role_name
    # Execute's tool round produced at least one progress event tagged
    # with the execute phase.
    assert any(ev.phase == "execute" for ev in progress)


async def test_turn_emits_phase_started_event_before_each_phase():
    """``AgentPhaseStarted`` fires at the top of each Plan / Execute /
    Review phase, paired with the matching ``AgentPhaseCompleted``.

    This is the live-state signal the dashboard reads to show
    ``current_phase`` on a ``state=working`` agent -- without it the
    dashboard would have to wait for ``AgentPhaseCompleted`` (which
    fires AFTER the LLM call returns) and the badge would only flip
    to the just-finished phase, never the currently-running one.
    """
    from crewlet.events.types import AgentPhaseCompleted, AgentPhaseStarted

    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["search"], decision="plan"),
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="execute output", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="reviewed"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    await engine.run_turn(agent, task_description="real work", org=agent.definition.org)

    started = [e for _, e in queue.published if isinstance(e, AgentPhaseStarted)]
    completed = [e for _, e in queue.published if isinstance(e, AgentPhaseCompleted)]
    assert [e.phase for e in started] == ["plan", "execute", "review"]

    # Each AgentPhaseStarted must precede its matching Completed in
    # the publish stream, otherwise the dashboard would briefly show
    # the *previous* phase as current.
    publish_order = [
        e.type for _, e in queue.published if e.type.startswith("agent_phase_")
    ]
    assert publish_order == [
        "agent_phase_started",
        "agent_phase_completed",  # plan
        "agent_phase_started",
        "agent_phase_completed",  # execute
        "agent_phase_started",
        "agent_phase_completed",  # review
    ]

    # Each started event carries the same turn_id and iteration as
    # its completed counterpart -- the projection joins on these.
    for s, c in zip(started, completed, strict=True):
        assert s.turn_id == c.turn_id
        assert s.iteration == c.iteration
        assert s.agent_id == agent.id_str


async def test_turn_engine_publishes_task_started_before_completed():
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(),
        execute=[Completion(content="x")],
        review=_review_submission("done", final_artifact="a"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    await engine.run_turn(agent, task_description="x", org=agent.definition.org)
    started = [
        i for i, (_, e) in enumerate(queue.published) if isinstance(e, TaskStarted)
    ]
    completed = [
        i for i, (_, e) in enumerate(queue.published) if isinstance(e, TaskCompleted)
    ]
    assert started and completed
    assert started[0] < completed[0]


async def test_turn_engine_self_iterate_loops_then_done():
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_flat(_plan_submission(["search"]), _plan_submission(["search"])),
        execute=[
            # Iteration 1 — call ``search`` then emit a draft.
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="draft v1"),
            # Iteration 2 — call ``search`` again then emit final.
            Completion(
                content="",
                tool_calls=[ToolCall(id="c2", name="search", arguments={"q": "x"})],
            ),
            Completion(content="draft v2"),
        ],
        review=_flat(
            _review_submission("self_iterate", notes="Add more detail."),
            _review_submission("done", final_artifact="final draft"),
        ),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
        max_iterations=3,
    )
    result = await engine.run_turn(
        agent, task_description="Write X", org=agent.definition.org
    )
    assert result == "final draft"
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed
    assert completed[0].iterations == 2


async def test_turn_engine_stall_aborts_as_failed():
    """Two self_iterate rounds with identical artifact hash → the
    stall guard publishes ``turn.guard_breach(kind=stall)`` and the
    turn terminates as ``failed``."""
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_flat(_plan_submission(), _plan_submission(), _plan_submission()),
        execute=[
            Completion(content="same text"),
            Completion(content="same text"),
            Completion(content="same text"),
        ],
        review=_flat(
            _review_submission("self_iterate", notes="bad"),
            _review_submission("self_iterate", notes="still bad"),
            _review_submission("self_iterate", notes="still bad"),
        ),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
        max_iterations=5,
    )
    result = await engine.run_turn(
        agent, task_description="x", org=agent.definition.org
    )
    # The artifact passes through; the `(escalated)` suffix is gone.
    assert "(escalated)" not in result
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed[0].decision == "failed"
    breaches = [
        e
        for _, e in queue.published
        if isinstance(e, TurnGuardBreach) and e.kind == "stall"
    ]
    assert breaches, "stall guard should publish a turn.guard_breach"


async def test_turn_engine_respects_delegation_depth_cap():
    agent = _mk_agent()
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={
            "default": _PhaseScriptedProvider(plan=[], execute=[], review=[])
        },
        tool_registry=_mk_registry(),
        event_queue=queue,
        delegation_depth_limit=2,
    )
    # Synthesise a trigger event that already sits at the limit.
    event = Event(
        type="task_assigned",
        source="a",
        payload={"task_id": "t", "task_description": "x"},
    )
    event.__dict__["delegation_depth"] = 2  # simulate inbound depth field
    result = await engine.run_turn(agent, event=event, org=agent.definition.org)
    assert "failed" in result
    # The TaskFailed emitted on
    # the depth-cap path must carry ``agent_id`` so per-agent
    # metrics/diagnostics that key off it (e.g. ObservabilityManager's
    # ``turns_failed``) don't silently drop the failure.
    failed = [e for _, e in queue.published if isinstance(e, TaskFailed)]
    assert failed, "depth-cap path should publish TaskFailed"
    assert failed[0].agent_id == agent.id_str
    # And AgentTurnCompleted is still emitted so downstream consumers
    # see a consistent turn-ended signal on this early-exit path.
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed and completed[0].decision == "failed"


async def test_turn_engine_max_iter_publishes_guard_breach_and_fails():
    """When the Plan/Execute/Review loop exhausts ``max_iterations``
    without ``done``, the engine publishes a
    ``turn.guard_breach(kind=max_iter)`` and terminates the turn as
    ``failed``."""
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_flat(_plan_submission(), _plan_submission()),
        execute=[
            Completion(content="round-1"),
            Completion(content="round-2"),
        ],
        review=_flat(
            _review_submission("self_iterate", notes="not yet"),
            _review_submission("self_iterate", notes="still not"),
        ),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
        max_iterations=2,
    )
    await engine.run_turn(agent, task_description="x", org=agent.definition.org)
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed and completed[-1].decision == "failed"
    breaches = [
        e
        for _, e in queue.published
        if isinstance(e, TurnGuardBreach) and e.kind == "max_iter"
    ]
    assert breaches, "max-iter path should publish a turn.guard_breach"


async def test_turn_engine_publishes_llm_unavailable_on_chain_exhaustion():
    """When ``FallbackLLMProvider`` raises ``LLMChainExhausted`` (chain
    fully exhausted), the turn publishes ``LLMUnavailable`` and
    terminates as ``failed`` without crashing the handler."""
    from crewlet.events.types import LLMUnavailable
    from crewlet.providers.errors import (
        AllCredentialsExhausted,
        ProviderErrorKind,
    )
    from crewlet.providers.fallback import LLMChainExhausted

    class _AlwaysFailingProvider:
        async def complete(self, *args, **kwargs):
            raise LLMChainExhausted(
                chain=["primary", "secondary"],
                last_exc=AllCredentialsExhausted(
                    provider_key="secondary",
                    kind_hint=ProviderErrorKind.RATE_LIMIT,
                ),
                last_error_kind="rate_limit",
            )

        async def stream(self, *args, **kwargs):
            return
            yield  # pragma: no cover - unreachable

    agent = _mk_agent()
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": _AlwaysFailingProvider()},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    # Should NOT raise — the chain-exhausted exception is caught and
    # converted into a clean ``failed`` turn so the handler keeps
    # serving other agents.
    result = await engine.run_turn(
        agent, task_description="x", org=agent.definition.org
    )
    assert result == "(no output)"
    unavailable = [e for _, e in queue.published if isinstance(e, LLMUnavailable)]
    assert unavailable, "should publish LLMUnavailable on chain exhaustion"
    assert unavailable[0].provider_chain == ["primary", "secondary"]
    assert unavailable[0].attempt_count == 2
    assert unavailable[0].last_error_kind == "rate_limit"
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed and completed[-1].decision == "failed"


async def test_lifecycle_events_carry_delegation_bookkeeping():
    """Every event the TurnEngine
    emits during a delegated turn must carry the trigger event's
    ``delegation_depth`` / ``parent_turn_id`` / ``delegation_chain``
    so dashboards can correlate the full chain of responsibility.
    If ``TaskStarted`` / ``TaskCompleted`` / ``TaskFailed`` /
    ``AgentTurnCompleted`` / ``TurnGuardBreach`` relied on the
    ``Event`` base-class defaults (depth=0, empty chain), delegated
    turns would look indistinguishable from top-level ones.
    """
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(),
        execute=[Completion(content="x")],
        review=_review_submission("done", final_artifact="a"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    # Synthesise a wake event a delegated turn would receive.
    event = Event(
        type="a2a_message",
        source="alice",
        payload={"task_id": "t-child", "task_description": "handle this"},
    )
    event.__dict__["delegation_depth"] = 2
    event.__dict__["parent_turn_id"] = "turn-parent-42"
    event.__dict__["delegation_chain"] = ["ceo", "alice"]

    await engine.run_turn(agent, event=event, org=agent.definition.org)

    # Every lifecycle event the engine emits must have inherited the
    # trigger event's delegation context.
    for ev_cls in (TaskStarted, TaskCompleted, AgentTurnCompleted):
        events = [e for _, e in queue.published if isinstance(e, ev_cls)]
        assert events, f"{ev_cls.__name__} not emitted"
        for ev in events:
            assert ev.delegation_depth == 2, (
                f"{ev_cls.__name__}.delegation_depth not propagated"
            )
            assert ev.parent_turn_id == "turn-parent-42", (
                f"{ev_cls.__name__}.parent_turn_id not propagated"
            )
            assert ev.delegation_chain == ["ceo", "alice"], (
                f"{ev_cls.__name__}.delegation_chain not propagated"
            )


async def test_turn_engine_per_phase_model_split():
    """model_keys should reflect each phase's resolved provider key."""
    agent = _mk_agent(
        {
            "llm_plan": "plan-model",
            "llm_execute": "exec-model",
            "llm_review": "review-model",
        }
    )

    providers = {
        "plan-model": _PhaseScriptedProvider(
            plan=_plan_submission(), execute=[], review=[], model="plan-m"
        ),
        "exec-model": _PhaseScriptedProvider(
            plan=[],
            # Plan defaults ``tools_needed=["search"]`` — call it so the
            # delivery-safety net stays out of the way and this test
            # exercises the per-phase model split it actually cares about.
            execute=[
                Completion(
                    content="",
                    tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
                ),
                Completion(content="ran"),
            ],
            review=[],
            model="exec-m",
        ),
        "review-model": _PhaseScriptedProvider(
            plan=[],
            execute=[],
            review=_review_submission("done", final_artifact="r"),
            model="review-m",
        ),
    }
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers=providers,
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    result = await engine.run_turn(
        agent, task_description="x", org=agent.definition.org
    )
    assert result == "r"
    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed
    ev = completed[0]
    assert ev.plan_model == "plan-m"
    assert ev.execute_model == "exec-m"
    assert ev.review_model == "review-m"


async def test_model_split_flag_detects_single_phase_override():
    """``_model_split_enabled``
    must return True when *any* phase resolves to a different provider
    key than the others, including the common case of overriding just
    ``llm_execute`` while leaving ``llm_plan`` / ``llm_review`` unset.

    The previous implementation only compared the per-phase role
    fields, missed the fallback to ``role.llm`` / ``"default"``, and
    returned False for that setup -- leaving the Plan prompt without
    the "executor runs on a cheaper / different model" hint.
    """
    # Role overrides ONLY llm_execute; plan/review fall back to
    # role.llm (unset) -> "default".  Resolved keys should be
    # {"default", "cheap-exec"} -> split=True.
    agent = _mk_agent({"llm_execute": "cheap-exec"})
    providers = {
        "default": _PhaseScriptedProvider(plan=[], execute=[], review=[]),
        "cheap-exec": _PhaseScriptedProvider(plan=[], execute=[], review=[]),
    }
    engine = TurnEngine(
        llm_providers=providers,
        tool_registry=_mk_registry(),
        event_queue=_QueueStub(),
    )
    role = agent.definition.role
    assert engine._model_split_enabled(role) is True, (
        "single-phase override must trigger the split-model hint"
    )

    # Control: all phases resolve to the same provider -> False.
    agent_same = _mk_agent()
    engine_same = TurnEngine(
        llm_providers={"default": providers["default"]},
        tool_registry=_mk_registry(),
        event_queue=_QueueStub(),
    )
    assert engine_same._model_split_enabled(agent_same.definition.role) is False


async def test_turn_engine_registers_custom_validator():
    """Extensions use register_result_validator; it must be preserved."""
    engine = TurnEngine(
        llm_providers={
            "default": _PhaseScriptedProvider(plan=[], execute=[], review=[])
        },
        tool_registry=_mk_registry(),
        event_queue=_QueueStub(),
    )

    class _V:
        name = "v"

        def validate(self, output: str) -> str:
            return output

    engine.register_result_validator(_V())
    assert any(v.name == "v" for v in engine._result_validators)


# per-tool check_fn integration -----------------------------------------


async def test_turn_engine_hides_tools_with_failing_check_fn():
    """End-to-end: a registry that registers ``search`` with a
    ``check_fn`` returning ``False`` must result in the planner's
    Plan catalogue not containing ``search`` (and the Execute surface
    rejecting it if the plan named it anyway)."""
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(decision="direct"),
        execute=[Completion(content="ok", tool_calls=[])],
        review=_review_submission("done", final_artifact="ok"),
    )

    registry = ToolRegistry()
    registry.register(
        SimpleTool(
            name="search",
            description="Search.",
            parameters={"type": "object"},
            fn=_noop,
        ),
        check_fn=lambda _ctx: False,  # ← never available this turn
    )
    registry.register(
        SimpleTool(
            name="query_knowledge",
            description="Query knowledge.",
            parameters={"type": "object"},
            fn=_noop,
        ),
    )

    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=registry,
        event_queue=queue,
    )
    await engine.run_turn(
        agent,
        task_id="t-checkfn",
        task_description="anything",
        org=agent.definition.org,
    )

    plan_events = [
        e
        for _, e in queue.published
        if getattr(e, "type", "") == "agent_phase_completed"
        and getattr(e, "phase", "") == "plan"
    ]
    assert plan_events, "expected a plan phase event"
    last_plan = plan_events[-1]
    # ``tool_catalogue`` is the list shown in the Plan prompt.  The
    # filtered ``search`` tool must not appear; the always-on
    # ``query_knowledge`` does.
    assert "search" not in last_plan.tool_catalogue
    assert "query_knowledge" in last_plan.tool_catalogue


async def test_turn_engine_falls_through_provider_chain_on_pool_exhausted():
    """Fallback-chain integration: a role configured with
    ``llm_plan: ["primary", "fallback"]`` where ``primary`` raises
    :class:`AllCredentialsExhausted` successfully
    completes its Plan phase against ``fallback``."""
    from crewlet.events.types import ProviderFallback
    from crewlet.providers.errors import AllCredentialsExhausted, ProviderErrorKind

    class _ExhaustedProvider:
        model = "primary"

        async def complete(self, *_args, **_kwargs):
            raise AllCredentialsExhausted(
                provider_key="primary",
                kind_hint=ProviderErrorKind.RATE_LIMIT,
            )

        async def stream(self, *_args, **_kwargs):
            raise NotImplementedError

    fallback = _PhaseScriptedProvider(
        plan=_plan_submission(["search"]),
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="searched", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="ok"),
    )

    agent = _mk_agent({"llm_plan": ["primary", "fallback"]})

    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={
            "primary": _ExhaustedProvider(),
            "fallback": fallback,
            # ``default`` is required because non-Plan phases resolve
            # via the role's default ``llm`` -- without a default the
            # test would fail on Execute / Review provider lookup.
            "default": fallback,
        },
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    result = await engine.run_turn(
        agent, task_id="t-fb", task_description="x", org=agent.definition.org
    )
    assert result == "ok"

    # A ProviderFallback event was published with the expected hop.
    # The wrapper's on_fallback fires via asyncio.create_task, so the
    # event arrives at some point during the turn; assert the
    # ``from``/``to`` keys and the error kind.
    fallback_events = [e for _, e in queue.published if isinstance(e, ProviderFallback)]
    assert fallback_events
    assert any(
        e.phase == "plan"
        and e.from_provider_key == "primary"
        and e.error_kind == "pool_exhausted"
        for e in fallback_events
    )


async def test_turn_engine_check_fn_invoked_once_per_turn():
    """Across the four phases of one turn, a tool's ``check_fn`` is
    invoked exactly once -- the result is cached on
    ``TurnContext.availability_set`` and reused by every phase
    factory."""
    invocations: list[int] = []

    def _counting(_ctx):
        invocations.append(1)
        return True

    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["search"]),
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="ok", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="done"),
    )

    registry = ToolRegistry()
    registry.register(
        SimpleTool(
            name="search",
            description="Search.",
            parameters={"type": "object"},
            fn=_noop,
        ),
        check_fn=_counting,
    )
    registry.register(
        SimpleTool(
            name="query_knowledge",
            description="Query knowledge.",
            parameters={"type": "object"},
            fn=_noop,
        ),
    )

    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=registry,
        event_queue=_QueueStub(),
    )
    await engine.run_turn(
        agent,
        task_id="t-once",
        task_description="x",
        org=agent.definition.org,
    )

    # Plan + Execute both consult availability for ``search`` but the
    # check_fn runs once at the top of ``_drive_phases``.
    assert len(invocations) == 1


async def test_batched_subagent_config_reaches_agent_context() -> None:
    """The batched-spawn knobs (subagent_max_parallel,
    subagent_batch_timeout_seconds, subagent_min_per_child_tokens) must
    land in the engine-provided ``spawn_subagent_config`` dict.  Knobs
    defined on TurnEngine but never placed in the dict would make the
    tool silently fall back to hardcoded defaults, ignoring the YAML
    config."""
    from crewlet.agent.turn_context import TurnContext

    agent = _mk_agent()
    engine = TurnEngine(
        llm_providers={"default": object()},  # type: ignore[dict-item]
        tool_registry=_mk_registry(),
        event_queue=_QueueStub(),
        subagent_max_parallel=7,
        subagent_batch_timeout_seconds=45.0,
        subagent_min_per_child_tokens=250,
    )
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = engine._build_agent_context(turn)
    cfg = ctx.__dict__["spawn_subagent_config"]
    assert cfg["subagent_max_parallel"] == 7
    assert cfg["subagent_batch_timeout_seconds"] == 45.0
    assert cfg["subagent_min_per_child_tokens"] == 250


async def test_set_knowledge_searcher_swaps_and_clears_live() -> None:
    """The TurnEngine captures the knowledge searcher at construction,
    so the engine's live-refresh path re-points it via the setter.
    Both directions must flow into the next turn's ``AgentContext``:
    swap (transport rebuilt / backend cut-over) and removal (the
    integration was deleted -- the prefetch must disable, not keep
    searching a stopped transport)."""
    from crewlet.agent.turn_context import TurnContext

    original = object()
    agent = _mk_agent()
    engine = TurnEngine(
        llm_providers={"default": object()},  # type: ignore[dict-item]
        tool_registry=_mk_registry(),
        event_queue=_QueueStub(),
        knowledge_searcher=original,
    )
    turn = TurnContext(agent=agent, org=agent.definition.org)
    assert engine._build_agent_context(turn).knowledge_searcher is original

    replacement = object()
    engine.set_knowledge_searcher(replacement)
    assert engine._build_agent_context(turn).knowledge_searcher is replacement

    engine.set_knowledge_searcher(None)
    assert engine._build_agent_context(turn).knowledge_searcher is None


# ---------------------------------------------------------------------------
# Graceful-shutdown gate (Engine.stop() drain)
# ---------------------------------------------------------------------------


async def test_run_turn_refused_after_begin_shutdown() -> None:
    """Once ``begin_shutdown`` fires, a new turn is refused before any
    state changes: the agent stays IDLE and the trigger can be NAK'd
    back to the broker for the next boot."""
    import pytest

    from crewlet.agent.instance import AgentState
    from crewlet.agent.turn import ShutdownDraining

    agent = _mk_agent()
    engine = TurnEngine(
        llm_providers={
            "default": _PhaseScriptedProvider(plan=[], execute=[], review=[])
        },
        tool_registry=_mk_registry(),
        event_queue=_QueueStub(),
    )
    engine.begin_shutdown()
    assert engine.shutting_down

    with pytest.raises(ShutdownDraining):
        await engine.run_turn(
            agent,
            task_id="t-refused",
            task_description="too late",
            org=agent.definition.org,
        )

    assert agent.state == AgentState.IDLE
    assert agent.current_task_id is None


async def test_turn_parked_at_concurrency_gate_aborts_on_shutdown() -> None:
    """A turn waiting for a ``ConcurrencyController`` slot when the
    drain begins must NOT start a fresh Plan/Execute/Review run during
    shutdown -- it rolls the agent back to IDLE and raises
    ``ShutdownDraining`` so the message is redelivered next boot."""
    import asyncio

    import pytest

    from crewlet.agent.instance import AgentState
    from crewlet.agent.turn import ShutdownDraining
    from crewlet.concurrency import ConcurrencyController

    concurrency = ConcurrencyController(max_concurrent=1)
    # Occupy the only slot so the turn parks at the gate.
    await concurrency.acquire("blocker")

    agent = _mk_agent()
    engine = TurnEngine(
        llm_providers={
            "default": _PhaseScriptedProvider(plan=[], execute=[], review=[])
        },
        tool_registry=_mk_registry(),
        event_queue=_QueueStub(),
        concurrency=concurrency,
    )

    turn_task = asyncio.create_task(
        engine.run_turn(
            agent,
            task_id="t-parked",
            task_description="parked behind the semaphore",
            org=agent.definition.org,
        )
    )
    # Let the turn reach the gate (start_working done, acquire pending).
    await asyncio.sleep(0.05)
    assert agent.state == AgentState.WORKING
    assert not turn_task.done()

    engine.begin_shutdown()

    with pytest.raises(ShutdownDraining):
        await asyncio.wait_for(turn_task, timeout=2.0)

    # Rolled back: no slot leaked, agent back to IDLE.
    assert agent.state == AgentState.IDLE
    assert agent.current_task_id is None
    concurrency.release("blocker")
    # The global slot must be free again -- a leak would hang here.
    await asyncio.wait_for(concurrency.acquire("post"), timeout=1.0)
    concurrency.release("post")


async def test_running_turn_finishes_despite_begin_shutdown() -> None:
    """``begin_shutdown`` must not interrupt a turn that is already
    past the concurrency gate -- the whole point of the drain is to
    let in-flight LLM rounds finish."""
    import asyncio

    from crewlet.concurrency import ConcurrencyController

    entered = asyncio.Event()
    release = asyncio.Event()

    async def _slow_tool(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
        entered.set()
        await release.wait()
        return ToolResult(success=True, output="slow done")

    registry = _mk_registry()
    registry.register(
        SimpleTool(
            name="slow_tool",
            description="Blocks until released.",
            parameters={"type": "object"},
            fn=_slow_tool,
        )
    )

    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["slow_tool"]),
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="slow_tool", arguments={})],
            ),
            Completion(content="finished the slow work", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="Final: slow"),
    )
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=registry,
        event_queue=_QueueStub(),
        concurrency=ConcurrencyController(max_concurrent=2),
    )

    turn_task = asyncio.create_task(
        engine.run_turn(
            agent,
            task_id="t-running",
            task_description="long round",
            org=agent.definition.org,
        )
    )
    await asyncio.wait_for(entered.wait(), timeout=2.0)

    # Drain begins mid-Execute; the running turn must be unaffected.
    engine.begin_shutdown()
    release.set()

    result = await asyncio.wait_for(turn_task, timeout=5.0)
    assert result == "Final: slow"
