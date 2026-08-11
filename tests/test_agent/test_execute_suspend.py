"""Execute-phase suspend/resume for the run_sandbox tool.

When the executor calls a suspending tool, run_execute_phase persists the
conversation + surface state and returns status="detached"; a later call with
``resume_from`` continues the saved conversation with the sandbox result
spliced in.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.execute import ExecuteResumeState, run_execute_phase
from crewlet.agent.instance import AgentInstance
from crewlet.agent.plan import ExecutionPlan
from crewlet.agent.turn_context import TurnContext
from crewlet.providers.llm.protocol import Completion, Message, ToolCall
from crewlet.sandbox.pending_store import (
    MemoryPendingSandboxRunStore,
    PendingSandboxRun,
)
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


async def _suspending(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
    return ToolResult(
        success=True,
        output="launched",
        suspend=True,
        suspend_payload={"sandbox_id": "sbx-1", "turn_id": "t"},
    )


def _mk_agent() -> AgentInstance:
    from crewlet.org.models import Organization, OrgUnit, Role

    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    return AgentInstance(definition=defn, handle="eng", email="e@acme.com")


def _registry() -> ToolRegistry:
    r = ToolRegistry()
    r.register(
        SimpleTool(
            name="run_sandbox",
            description="run code in a sandbox",
            parameters={"type": "object"},
            fn=_suspending,
        )
    )
    return r


def _ctx(queue) -> AgentContext:
    return AgentContext(
        agent_id="a", agent_handle="eng", role="Engineer", event_queue=queue
    )


async def test_execute_suspends_and_persists_state() -> None:
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="fix CI")
    plan = ExecutionPlan(tools_needed=["run_sandbox"])
    queue = _QueueStub()
    store = MemoryPendingSandboxRunStore()
    # The run_sandbox tool would have created this row before suspending.
    await store.create(PendingSandboxRun(turn_id=turn.turn_id, agent_handle="eng"))

    provider = _ProviderStub(
        completions=[
            Completion(
                content="kicking off the sandbox",
                tool_calls=[ToolCall(id="c1", name="run_sandbox", arguments={})],
            ),
        ]
    )
    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=provider,
        provider_key="stub",
        registry=_registry(),
        role_mcp_tools=[],
        always_on=[],
        event_queue=queue,
        agent_context=_ctx(queue),
        pending_store=store,
    )

    # The turn ends detached; the loop never asked for a second completion.
    assert result.status == "detached"
    assert result.sandbox_id == "sbx-1"
    assert result.backend == "sandbox"
    assert provider.calls == 1
    # The conversation + surface state are persisted for resume.
    row = await store.get(turn.turn_id)
    assert row is not None
    st = row.execute_state
    assert st["pending_tool_call_id"] == "c1"
    assert st["pending_tool_name"] == "run_sandbox"
    assert "run_sandbox" in st["active_tool_names"]
    assert st["messages"]
    # The pending call has no tool reply in the persisted conversation.
    tool_msgs = [m for m in st["messages"] if m.get("role") == "tool"]
    assert all(m.get("tool_call_id") != "c1" for m in tool_msgs)
    # A suspended Execute phase event was published.
    assert any(e.type == "agent_phase_completed" for _, e in queue.published)


async def test_execute_resumes_with_result_and_finishes() -> None:
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="fix CI")
    plan = ExecutionPlan(tools_needed=["run_sandbox"])
    queue = _QueueStub()

    seen: dict[str, Any] = {}

    class _Capture:
        model = "cap"

        async def complete(
            self,
            messages,
            tools=None,
            temperature=0.7,
            max_tokens=None,
            tool_choice=None,
        ):
            seen["messages"] = list(messages)
            return Completion(content="Reported the findings on Slack.", tool_calls=[])

    resume = ExecuteResumeState(
        plan=plan,
        result_content="CI fails: pytest-cov missing from dev deps",
        messages=[
            Message(role="system", content="sys").model_dump(mode="json"),
            Message(role="user", content="Task: fix CI").model_dump(mode="json"),
            Message(
                role="assistant",
                content="",
                tool_calls=[ToolCall(id="c1", name="run_sandbox", arguments={})],
            ).model_dump(mode="json"),
        ],
        pending_tool_call_id="c1",
        pending_tool_name="run_sandbox",
        active_tool_names=["run_sandbox"],
    )

    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=_Capture(),
        provider_key="cap",
        registry=_registry(),
        role_mcp_tools=[],
        always_on=[],
        event_queue=queue,
        agent_context=_ctx(queue),
        resume_from=resume,
    )

    # The resumed loop continued and finished normally.
    assert result.status == "done"
    assert result.text == "Reported the findings on Slack."
    # The sandbox result was spliced in as the pending call's reply, so the
    # model saw it.
    tool_msgs = [m for m in seen["messages"] if m.role == "tool"]
    assert any(m.tool_call_id == "c1" and "pytest-cov" in m.content for m in tool_msgs)


async def test_suspend_persists_tool_executions_for_review() -> None:
    # The suspending run_sandbox call must be recorded in the persisted
    # execute_state so the resumed phase can replay it into Review's evidence.
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="fix CI")
    plan = ExecutionPlan(tools_needed=["run_sandbox"])
    queue = _QueueStub()
    store = MemoryPendingSandboxRunStore()
    await store.create(PendingSandboxRun(turn_id=turn.turn_id, agent_handle="eng"))
    provider = _ProviderStub(
        completions=[
            Completion(
                content="kicking off",
                tool_calls=[ToolCall(id="c1", name="run_sandbox", arguments={})],
            ),
        ]
    )

    await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=provider,
        provider_key="stub",
        registry=_registry(),
        role_mcp_tools=[],
        always_on=[],
        event_queue=queue,
        agent_context=_ctx(queue),
        pending_store=store,
    )

    row = await store.get(turn.turn_id)
    assert row is not None
    execs = row.execute_state["tool_executions"]
    assert any(e.get("name") == "run_sandbox" for e in execs)


async def test_resume_replays_prior_tool_executions_into_result() -> None:
    # Review's evidence is built from execute_result.tool_executions. On resume,
    # the run_sandbox call lives in the SUSPENDED portion; without replaying it
    # the resumed result would only show post-resume calls and Review would judge
    # the sandbox-delegated work "fabricated" and loop the turn. The resumed
    # result must carry the prior executions PLUS the new ones, in order.
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="fix CI")
    plan = ExecutionPlan(tools_needed=["run_sandbox"])
    queue = _QueueStub()

    class _Capture:
        model = "cap"

        async def complete(
            self,
            messages,
            tools=None,
            temperature=0.7,
            max_tokens=None,
            tool_choice=None,
        ):
            # Finish with a plain text wrap-up (no further tool calls).
            return Completion(content="Posted findings.", tool_calls=[])

    resume = ExecuteResumeState(
        plan=plan,
        result_content="CI fails: pytest-cov missing",
        messages=[
            Message(role="system", content="sys").model_dump(mode="json"),
            Message(
                role="assistant",
                content="",
                tool_calls=[ToolCall(id="c1", name="run_sandbox", arguments={})],
            ).model_dump(mode="json"),
        ],
        pending_tool_call_id="c1",
        pending_tool_name="run_sandbox",
        active_tool_names=["run_sandbox"],
        prior_tool_executions=[
            {
                "name": "run_sandbox",
                "arguments": "{}",
                "result": "(detached work launched; awaiting result)",
                "success": True,
            }
        ],
    )

    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=_Capture(),
        provider_key="cap",
        registry=_registry(),
        role_mcp_tools=[],
        always_on=[],
        event_queue=queue,
        agent_context=_ctx(queue),
        resume_from=resume,
    )

    assert result.status == "done"
    names = [e.get("name") for e in result.tool_executions]
    # The run_sandbox call (from the suspended portion) is visible to Review,
    # and it comes first (chronological order preserved).
    assert "run_sandbox" in names
    assert names[0] == "run_sandbox"


async def test_resume_published_event_omits_prior_segment() -> None:
    # The RESUMED phase event shows ONLY the post-resume segment — never a
    # replay of the pre-suspend reasoning/tools (those are already their own
    # published checkpoint). Without this the dashboard's resume row repeats the
    # kickoff row "from start to end". Review still sees the full history via
    # result.tool_executions (covered above).
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="fix CI")
    plan = ExecutionPlan(tools_needed=["run_sandbox"])
    queue = _QueueStub()

    class _Capture:
        model = "cap"

        async def complete(
            self,
            messages,
            tools=None,
            temperature=0.7,
            max_tokens=None,
            tool_choice=None,
        ):
            return Completion(content="NEW-WRAPUP posted to Slack.", tool_calls=[])

    resume = ExecuteResumeState(
        plan=plan,
        result_content="CI fails: pytest-cov missing",
        messages=[
            Message(role="system", content="sys").model_dump(mode="json"),
            Message(role="user", content="Task: fix CI").model_dump(mode="json"),
            Message(
                role="assistant",
                content="PRIOR-REASONING about the sandbox",
                tool_calls=[ToolCall(id="c1", name="run_sandbox", arguments={})],
            ).model_dump(mode="json"),
        ],
        pending_tool_call_id="c1",
        pending_tool_name="run_sandbox",
        active_tool_names=["run_sandbox"],
        prior_tool_executions=[
            {
                "name": "run_sandbox",
                "arguments": "{}",
                "result": "(detached work launched; awaiting result)",
                "success": True,
            }
        ],
    )

    result = await run_execute_phase(
        turn=turn,
        plan=plan,
        provider=_Capture(),
        provider_key="cap",
        registry=_registry(),
        role_mcp_tools=[],
        always_on=[],
        event_queue=queue,
        agent_context=_ctx(queue),
        resume_from=resume,
    )

    # Review still sees the full chain (the prior run_sandbox call, first).
    assert [e.get("name") for e in result.tool_executions][0] == "run_sandbox"

    # The PUBLISHED phase event carries only the post-resume segment.
    phase_events = [
        e
        for _, e in queue.published
        if getattr(e, "type", "") == "agent_phase_completed"
    ]
    assert phase_events
    ev = phase_events[-1]
    assert "NEW-WRAPUP" in ev.response
    assert "PRIOR-REASONING" not in ev.response  # no replay of the kickoff segment
    # The post-resume segment made no tool calls, so the event's tool log is
    # empty — the suspended run_sandbox is NOT replayed into it.
    assert all(t.get("name") != "run_sandbox" for t in ev.tool_executions)
