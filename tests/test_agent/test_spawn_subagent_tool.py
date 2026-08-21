"""Tests for the ``spawn_subagent`` LLM-callable tool."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.turn import TurnEngine
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, ToolCall
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry
from crewlet.tools.spawn_subagent_tool import register_spawn_subagent_tool


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


async def _noop(params, ctx):
    return ToolResult(success=True, output="noop")


def _mk_registry() -> ToolRegistry:
    r = ToolRegistry()
    r.register(
        SimpleTool(
            name="web_search",
            description="search",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    r.register(
        SimpleTool(
            name="load_tool_skill",
            description="q",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    register_spawn_subagent_tool(r)
    return r


def _mk_agent() -> AgentInstance:
    role = Role(name="Engineer", handle="alice")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    agent = AgentInstance(definition=defn, handle="alice", email="a@acme.com")
    agent.activate()
    return agent


def _plan_submission(tools_needed: list[str]) -> Completion:
    return Completion(
        content="",
        tool_calls=[
            ToolCall(
                id="cp",
                name="submit_plan",
                arguments={
                    "decision": "plan",
                    "reasoning": "delegate research",
                    "steps": [{"intent": "spawn"}],
                    "tools_needed": tools_needed,
                    "success_criteria": [],
                },
            )
        ],
    )


def _review_done(final: str) -> Completion:
    return Completion(
        content="",
        tool_calls=[
            ToolCall(
                id="cr",
                name="submit_review",
                arguments={"decision": "done", "final_artifact": final},
            )
        ],
    )


class _ScriptedProvider:
    """Routes by phase and by sub-agent system prompt substring.

    The sub-agent's system prompt contains SUBAGENT_PREAMBLE; routes
    there to return a distinct sub-agent completion.
    """

    model = "s"

    def __init__(
        self,
        *,
        plan: list[Completion],
        execute: list[Completion],
        review: list[Completion],
        subagent: list[Completion],
    ) -> None:
        self._plan = list(plan)
        self._execute = list(execute)
        self._review = list(review)
        self._subagent = list(subagent)

    async def complete(self, messages, tools=None, **_):
        sys_text = next((m.content for m in messages if m.role == "system"), "")
        if "short-lived sub-agent" in sys_text:
            return (
                self._subagent.pop(0)
                if self._subagent
                else Completion(content="sub-done")
            )
        if "PLAN phase" in sys_text:
            return self._plan.pop(0) if self._plan else Completion(content="plan")
        if "EXECUTE phase" in sys_text:
            return self._execute.pop(0) if self._execute else Completion(content="x")
        if "REVIEW phase" in sys_text:
            return self._review.pop(0) if self._review else Completion(content="r")
        return Completion(content="?")


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


async def test_execute_phase_llm_can_spawn_subagent_via_tool():
    agent = _mk_agent()
    provider = _ScriptedProvider(
        plan=[_plan_submission(["spawn_subagent", "web_search"])],
        execute=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="sp",
                        name="spawn_subagent",
                        arguments={
                            "task_prompt": "Find 3 sources on X",
                            "system_prompt": "You are a researcher.",
                            "tool_names": ["web_search"],
                        },
                    )
                ],
            ),
            Completion(content="here is the summary", tool_calls=[]),
        ],
        review=[_review_done("final summary")],
        subagent=[
            Completion(
                content="Research finished. 3 sources: A, B, C.",
                tool_calls=[],
            )
        ],
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    result = await engine.run_turn(
        agent, task_description="research X", org=agent.definition.org
    )
    assert result == "final summary"
    # The parent turn should have counted one sub-agent invocation.
    from crewlet.events.types import AgentTurnCompleted

    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed
    assert completed[0].subagent_count == 1
    assert completed[0].subagent_tokens >= 0


async def test_subagent_cannot_be_granted_tools_the_parent_didnt_have():
    """``parent_tool_names`` in
    the engine-provided ``spawn_subagent_config`` must reflect the
    parent's Execute ``ToolSurface`` (``plan.tools_needed ∪
    always_on``), NOT the full tool registry.  Otherwise the
    "requested tools must be a subset of the parent's tools"
    invariant in ``ToolSurface.for_subagent`` is defeated: a parent
    LLM could request tool names that exist in the registry but were
    not in its own Execute surface, granting the sub-agent broader
    capabilities than the parent itself had.
    """
    agent = _mk_agent()
    registry = _mk_registry()
    # Parent's plan names only "web_search"; registry also has
    # "load_tool_skill" as always-on.  The sub-agent must therefore
    # NOT be grantable the tool "secret_tool" even though it is
    # registered globally.
    registry.register(
        SimpleTool(
            name="secret_tool",
            description="s",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    captured_parent_tool_names: list[list[str]] = []

    async def _capture_spawn(params, ctx):
        captured_parent_tool_names.append(
            list(ctx.__dict__["spawn_subagent_config"]["parent_tool_names"])
        )
        return ToolResult(success=True, output="captured")

    # Override the spawn_subagent tool with a capturing stub so we
    # can inspect the config the engine attaches before any real
    # spawn happens.
    registry._tools["spawn_subagent"] = SimpleTool(  # type: ignore[attr-defined]
        name="spawn_subagent",
        description="stub",
        parameters={"type": "object"},
        fn=_capture_spawn,
    )

    provider = _ScriptedProvider(
        plan=[_plan_submission(["spawn_subagent", "web_search"])],
        execute=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="sp",
                        name="spawn_subagent",
                        arguments={
                            "task_prompt": "x",
                            "system_prompt": "y",
                            # Attempt to grant a tool that the parent
                            # never had access to in its Execute surface.
                            "tool_names": ["secret_tool"],
                        },
                    )
                ],
            ),
            Completion(content="back", tool_calls=[]),
        ],
        review=[_review_done("ok")],
        subagent=[],
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=registry,
        event_queue=queue,
    )
    await engine.run_turn(agent, task_description="x", org=agent.definition.org)

    assert captured_parent_tool_names, "spawn_subagent never called"
    # The parent's Execute surface is plan.tools_needed(["web_search",
    # "spawn_subagent"]) ∪ always_on(["load_tool_skill"]).  Critically,
    # "secret_tool" must NOT be in this list even though it is in the
    # registry.
    names = captured_parent_tool_names[0]
    assert "secret_tool" not in names, f"parent_tool_names leaked the registry: {names}"
    assert "web_search" in names
    assert "load_tool_skill" in names


async def test_spawn_subagent_tool_rejects_non_string_tool_names():
    """``tool_names`` with
    non-string elements must be rejected before reaching
    ``ToolSurface.for_subagent``, whose ``frozenset[str]`` membership
    check would otherwise raise ``TypeError`` on unhashable items.
    """
    from crewlet.agent.turn_context import TurnContext

    registry = _mk_registry()
    tool = registry.get("spawn_subagent")
    assert tool is not None
    agent = _mk_agent()
    ctx = AgentContext(
        agent_id=agent.id_str,
        agent_handle=agent.handle,
        role=agent.role_name,
        event_queue=_QueueStub(),
    )
    # Real TurnContext + minimum-viable spawn_subagent_config so the
    # earlier "active turn context" / "malformed config" guards
    # don't short-circuit the call before the type check we're
    # testing.
    ctx.__dict__["turn_context"] = TurnContext(agent=agent, org=agent.definition.org)
    ctx.__dict__["spawn_subagent_config"] = {
        "llm_providers": {},
        "tool_registry": registry,
    }
    for bad in (
        [{"nested": "dict"}],
        [["list"]],
        [123],
        ["ok", 42],
    ):
        result = await tool.execute(
            {"task_prompt": "t", "system_prompt": "s", "tool_names": bad}, ctx
        )
        assert not result.success, f"should reject tool_names={bad!r}"
        assert "strings" in (result.error or "")


async def test_spawn_subagent_tool_rejects_wrong_typed_turn_context():
    """The engine attaches
    ``turn_context`` via ``ctx.__dict__``; an extension that
    monkey-patches the context with a non-TurnContext object must
    not crash the parent turn.  ``llm_loop.execute_tool`` does not
    catch tool exceptions, so the guard has to return a clean
    ToolResult rather than let an ``AttributeError`` propagate.
    """
    registry = _mk_registry()
    tool = registry.get("spawn_subagent")
    assert tool is not None
    ctx = AgentContext(
        agent_id="x", agent_handle="x", role="x", event_queue=_QueueStub()
    )
    ctx.__dict__["turn_context"] = object()  # bogus, not a TurnContext
    ctx.__dict__["spawn_subagent_config"] = {
        "llm_providers": {},
        "tool_registry": registry,
    }
    result = await tool.execute(
        {"task_prompt": "t", "system_prompt": "s", "tool_names": []}, ctx
    )
    assert not result.success
    assert "active turn context" in (result.error or "")


async def test_spawn_subagent_tool_rejects_malformed_config():
    """A malformed
    ``spawn_subagent_config`` (missing required keys, not a dict)
    returns a clean ToolResult failure instead of raising
    ``KeyError`` / ``AttributeError`` out of the tool loop.
    """
    from crewlet.agent.turn_context import TurnContext

    registry = _mk_registry()
    tool = registry.get("spawn_subagent")
    assert tool is not None
    agent = _mk_agent()
    ctx = AgentContext(
        agent_id=agent.id_str,
        agent_handle=agent.handle,
        role=agent.role_name,
        event_queue=_QueueStub(),
    )
    ctx.__dict__["turn_context"] = TurnContext(agent=agent, org=agent.definition.org)

    # Not a dict.
    ctx.__dict__["spawn_subagent_config"] = "oops"
    result = await tool.execute(
        {"task_prompt": "t", "system_prompt": "s", "tool_names": []}, ctx
    )
    assert not result.success
    assert "malformed" in (result.error or "")

    # Missing required keys.
    ctx.__dict__["spawn_subagent_config"] = {"tool_registry": registry}
    result = await tool.execute(
        {"task_prompt": "t", "system_prompt": "s", "tool_names": []}, ctx
    )
    assert not result.success
    assert "missing required keys" in (result.error or "")
    assert "llm_providers" in (result.error or "")


async def test_spawn_subagent_tool_rejects_falsy_non_list_tool_names():
    """A falsy-but-non-None value
    like ``""`` or ``0`` must NOT silently become an empty allowlist
    -- that would mask malformed LLM output as a legitimate "spawn a
    tool-less sub-agent" request.  Only ``None`` / absent key should
    default to ``[]``.
    """
    from crewlet.agent.turn_context import TurnContext

    registry = _mk_registry()
    tool = registry.get("spawn_subagent")
    assert tool is not None
    agent = _mk_agent()
    ctx = AgentContext(
        agent_id=agent.id_str,
        agent_handle=agent.handle,
        role=agent.role_name,
        event_queue=_QueueStub(),
    )
    ctx.__dict__["turn_context"] = TurnContext(agent=agent, org=agent.definition.org)
    ctx.__dict__["spawn_subagent_config"] = {
        "llm_providers": {},
        "tool_registry": registry,
    }
    for bad in ("", 0, 0.0, {}):
        result = await tool.execute(
            {
                "task_prompt": "t",
                "system_prompt": "s",
                "tool_names": bad,
            },
            ctx,
        )
        assert not result.success, f"should reject tool_names={bad!r}"
        assert "tool_names must be a list" in (result.error or "")


async def test_spawn_subagent_tool_rejects_without_turn_context():
    """Calling the tool directly (not from a turn) must fail cleanly."""
    registry = _mk_registry()
    tool = registry.get("spawn_subagent")
    assert tool is not None
    ctx = AgentContext(
        agent_id="x", agent_handle="x", role="x", event_queue=_QueueStub()
    )
    result = await tool.execute(
        {"task_prompt": "t", "system_prompt": "s", "tool_names": []}, ctx
    )
    assert not result.success
    assert "active turn context" in (result.error or "")


async def test_spawn_subagent_tool_rejects_denylisted_tool_names():
    """The subagent ToolSurface drops ``spawn_subagent`` and colleague
    tools from what the sub-agent actually sees.  Here we confirm the
    tool still returns success (sub-agent ran) -- the denylist filter
    is silent by design, with rejected names in a log line.
    """
    agent = _mk_agent()
    provider = _ScriptedProvider(
        plan=[_plan_submission(["spawn_subagent"])],
        execute=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="sp",
                        name="spawn_subagent",
                        arguments={
                            "task_prompt": "x",
                            "system_prompt": "y",
                            # Parent has these; sub-agent denylist rejects
                            # spawn_subagent; allowed list ends up empty.
                            "tool_names": ["spawn_subagent"],
                        },
                    )
                ],
            ),
            Completion(content="back", tool_calls=[]),
        ],
        review=[_review_done("ok")],
        subagent=[Completion(content="done", tool_calls=[])],
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    await engine.run_turn(agent, task_description="x", org=agent.definition.org)
    # The sub-agent ran and returned.
    from crewlet.events.types import AgentTurnCompleted

    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed and completed[0].subagent_count == 1


# batched spawn_subagent -----------------------------------------------


async def test_spawn_subagent_tool_accepts_batched_tasks():
    """A ``tasks: [...]`` payload spawns N children concurrently.
    The tool result is JSON encoding the per-child outcomes.
    """
    import json

    agent = _mk_agent()
    provider = _ScriptedProvider(
        plan=[_plan_submission(["spawn_subagent", "web_search"])],
        execute=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="sp",
                        name="spawn_subagent",
                        arguments={
                            "tool_names": ["web_search"],
                            "tasks": [
                                {
                                    "task_prompt": "Search A",
                                    "system_prompt": "Researcher 1",
                                },
                                {
                                    "task_prompt": "Search B",
                                    "system_prompt": "Researcher 2",
                                },
                                {
                                    "task_prompt": "Search C",
                                    "system_prompt": "Researcher 3",
                                },
                            ],
                        },
                    )
                ],
            ),
            Completion(content="batch done", tool_calls=[]),
        ],
        review=[_review_done("final")],
        subagent=[
            Completion(content="A finding", tool_calls=[]),
            Completion(content="B finding", tool_calls=[]),
            Completion(content="C finding", tool_calls=[]),
        ],
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    await engine.run_turn(
        agent, task_description="research three things", org=agent.definition.org
    )

    # The Execute tool result should be the rendered JSON with three
    # entries in input order.
    from crewlet.events.types import AgentPhaseCompleted, SubagentBatched

    exec_events = [
        e
        for _, e in queue.published
        if isinstance(e, AgentPhaseCompleted) and e.phase == "execute"
    ]
    assert exec_events
    tool_results = [
        x for x in exec_events[-1].tool_executions if x.get("name") == "spawn_subagent"
    ]
    assert tool_results
    rendered = json.loads(tool_results[0]["result"])
    assert "results" in rendered
    assert len(rendered["results"]) == 3
    assert [r["index"] for r in rendered["results"]] == [0, 1, 2]
    # Texts arrive in input order.
    texts = [r["text"] for r in rendered["results"]]
    assert texts == ["A finding", "B finding", "C finding"]

    # A SubagentBatched event was published with the right totals.
    batched_events = [e for _, e in queue.published if isinstance(e, SubagentBatched)]
    assert batched_events
    assert batched_events[-1].task_count == 3
    assert batched_events[-1].successes == 3
    assert batched_events[-1].failures == 0


async def test_spawn_subagent_tool_rejects_hybrid_shape():
    """Both ``task_prompt`` (single) AND ``tasks`` (batched) → rejected
    with a clear error before any sub-agent runs."""
    agent = _mk_agent()
    provider = _ScriptedProvider(
        plan=[_plan_submission(["spawn_subagent", "web_search"])],
        execute=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="bad",
                        name="spawn_subagent",
                        arguments={
                            "task_prompt": "x",
                            "system_prompt": "y",
                            "tasks": [
                                {"task_prompt": "a", "system_prompt": "b"},
                            ],
                            "tool_names": ["web_search"],
                        },
                    )
                ],
            ),
            Completion(content="ok", tool_calls=[]),
        ],
        review=[_review_done("ok")],
        subagent=[],
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    await engine.run_turn(agent, task_description="x", org=agent.definition.org)
    # No sub-agent ran -- the tool rejected the call up front.
    from crewlet.events.types import AgentTurnCompleted

    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed and completed[0].subagent_count == 0


async def test_spawn_subagent_tool_rejects_empty_tasks_list():
    """``tasks: []`` is an explicit empty-batch error."""
    agent = _mk_agent()
    provider = _ScriptedProvider(
        plan=[_plan_submission(["spawn_subagent", "web_search"])],
        execute=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="empty",
                        name="spawn_subagent",
                        arguments={
                            "tasks": [],
                            "tool_names": ["web_search"],
                        },
                    )
                ],
            ),
            Completion(content="ok", tool_calls=[]),
        ],
        review=[_review_done("ok")],
        subagent=[],
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    await engine.run_turn(agent, task_description="x", org=agent.definition.org)
    from crewlet.events.types import AgentTurnCompleted

    completed = [e for _, e in queue.published if isinstance(e, AgentTurnCompleted)]
    assert completed and completed[0].subagent_count == 0


async def test_spawn_subagent_batch_isolates_per_child_failure():
    """When one child raises during gather, the sibling children still
    return real results; the failing child's slot carries an error
    string.

    Tests :func:`spawn_subagent_batch` directly (skipping the tool
    schema layer) so we can plug in a sub-agent provider that raises
    on a specific call without needing to route by message content.
    """
    import json

    from crewlet.agent.subagent import SubagentTask, spawn_subagent_batch
    from crewlet.agent.turn_context import TurnContext

    agent = _mk_agent()
    parent_turn = TurnContext(agent=agent, org=agent.definition.org)

    raising_calls = {"count": 0}

    class _RaisingProvider:
        model = "rp"

        async def complete(self, messages, tools=None, **_):
            raising_calls["count"] += 1
            if raising_calls["count"] == 2:
                raise RuntimeError("synthetic child failure")
            return Completion(content=f"child output {raising_calls['count']}")

    queue = _QueueStub()
    ctx = AgentContext(
        agent_id="a",
        agent_handle="eng",
        role="Engineer",
        event_queue=queue,
    )

    registry = _mk_registry()
    results = await spawn_subagent_batch(
        parent_turn=parent_turn,
        tasks=[
            SubagentTask(task_prompt="T1", system_prompt="S1"),
            SubagentTask(task_prompt="T2", system_prompt="S2"),
            SubagentTask(task_prompt="T3", system_prompt="S3"),
        ],
        tool_names=["web_search"],
        parent_tool_names=["web_search"],
        provider=_RaisingProvider(),
        provider_key="rp",
        registry=registry,
        role_mcp_tools=[],
        agent_context=ctx,
        event_queue=queue,
        max_parallel=1,  # deterministic serial execution for ordering
    )

    assert len(results) == 3
    successful = [r for r in results if not r.error]
    failed = [r for r in results if r.error]
    assert len(successful) == 2
    assert len(failed) == 1
    assert "synthetic" in failed[0].error
    # Sanity: the JSON payload would render the same fields the tool
    # wrapper exposes.
    payload = [{"text": r.text, "error": r.error} for r in results]
    assert json.dumps(payload)  # serialisable

    # SubagentBatched published.
    from crewlet.events.types import SubagentBatched

    batched = [e for _, e in queue.published if isinstance(e, SubagentBatched)]
    assert batched and batched[-1].successes == 2 and batched[-1].failures == 1


async def test_a_batch_timeout_keeps_the_children_that_already_finished():
    """The deadline cancels the ``gather``, which discards the return
    value of every child — including the ones that had already FINISHED.

    Reporting those as timed-out with empty text throws away work that
    completed and was paid for: the tokens are spent either way, and the
    planner is told a child produced nothing when it produced an answer.
    A batch where one task is quick and one is pathological is the common
    shape of this, not an exotic one.
    """
    import asyncio

    from crewlet.agent.subagent import SubagentTask, spawn_subagent_batch
    from crewlet.agent.turn_context import TurnContext

    agent = _mk_agent()
    parent_turn = TurnContext(agent=agent, org=agent.definition.org)

    class _OneSlowProvider:
        """Answers the first task at once and hangs on the second."""

        model = "mixed"

        def __init__(self) -> None:
            self._seen = 0

        async def complete(self, messages, tools=None, **_):
            self._seen += 1
            if self._seen > 1:
                await asyncio.sleep(5.0)
            return Completion(content="quick answer")

    queue = _QueueStub()
    ctx = AgentContext(
        agent_id="a",
        agent_handle="eng",
        role="Engineer",
        event_queue=queue,
    )

    results = await spawn_subagent_batch(
        parent_turn=parent_turn,
        tasks=[
            SubagentTask(task_prompt="quick", system_prompt="S1"),
            SubagentTask(task_prompt="slow", system_prompt="S2"),
        ],
        tool_names=["web_search"],
        parent_tool_names=["web_search"],
        provider=_OneSlowProvider(),
        provider_key="mixed",
        registry=_mk_registry(),
        role_mcp_tools=[],
        agent_context=ctx,
        event_queue=queue,
        max_parallel=1,  # serial, so the first genuinely settles first
        batch_timeout_seconds=0.3,
    )

    assert len(results) == 2
    assert results[0].text == "quick answer", (
        "a child that finished before the deadline was reported as a timeout "
        "with its answer discarded"
    )
    assert not results[0].timed_out
    assert results[1].timed_out


async def test_spawn_subagent_batch_aggregate_timeout_returns_timeout_results():
    """A pathological batch exceeding ``batch_timeout_seconds`` returns
    a TimeoutError synthetic result per task -- siblings don't carry
    forward past the wall-clock cap."""
    import asyncio

    from crewlet.agent.subagent import SubagentTask, spawn_subagent_batch
    from crewlet.agent.turn_context import TurnContext

    agent = _mk_agent()
    parent_turn = TurnContext(agent=agent, org=agent.definition.org)

    class _SlowProvider:
        model = "slow"

        async def complete(self, messages, tools=None, **_):
            await asyncio.sleep(5.0)
            return Completion(content="never-reached")

    queue = _QueueStub()
    ctx = AgentContext(
        agent_id="a",
        agent_handle="eng",
        role="Engineer",
        event_queue=queue,
    )
    registry = _mk_registry()

    results = await spawn_subagent_batch(
        parent_turn=parent_turn,
        tasks=[
            SubagentTask(task_prompt="T1", system_prompt="S1"),
            SubagentTask(task_prompt="T2", system_prompt="S2"),
        ],
        tool_names=["web_search"],
        parent_tool_names=["web_search"],
        provider=_SlowProvider(),
        provider_key="slow",
        registry=registry,
        role_mcp_tools=[],
        agent_context=ctx,
        event_queue=queue,
        max_parallel=2,
        batch_timeout_seconds=0.1,
    )
    assert len(results) == 2
    assert all(r.timed_out for r in results)
    assert all("0.1" in r.error or "exceeded" in r.error for r in results)
