"""Tests for the ``run_sandbox`` Execute tool.

The tool launches a detached sandbox coding run and suspends the Execute
loop; the engine resumes the loop with the result later.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.plan import ExecutionPlan
from crewlet.agent.turn_context import TurnContext
from crewlet.config import LLMProviderConfig
from crewlet.org.models import Organization, OrgUnit, Role, RoleSandboxConfig
from crewlet.sandbox import FakeCodingAgentRunner, FakeSandboxProvider, SandboxManager
from crewlet.sandbox.pending_store import MemoryPendingSandboxRunStore
from crewlet.tools.protocol import AgentContext, CheckContext
from crewlet.tools.registry import ToolRegistry
from crewlet.tools.run_sandbox_tool import _run_sandbox_tool, register_run_sandbox_tool


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


class _P:
    model = "claude"


def _mk_agent(sandbox: RoleSandboxConfig | None = None) -> AgentInstance:
    role = Role(
        name="Engineer",
        handle="eng",
        llm="default",
        llm_execute="exec-prov",
        sandbox=sandbox or RoleSandboxConfig(enabled=True, coding_agent="claude-code"),
        mcp_env={"github": {"Authorization": "Bearer ghp_x"}},
    )
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    return AgentInstance(definition=defn, handle="eng", email="e@acme.com")


def _mk_manager(runner: FakeCodingAgentRunner | None = None) -> SandboxManager:
    runner = runner or FakeCodingAgentRunner(name="claude-code")
    return SandboxManager(provider=FakeSandboxProvider(), runners={runner.name: runner})


def _ctx(turn: TurnContext, queue, manager, store) -> AgentContext:
    ctx = AgentContext(
        agent_id=turn.agent.id_str,
        agent_handle=turn.agent.handle,
        role=turn.agent.role_name,
        event_queue=queue,
    )
    ctx.__dict__["turn_context"] = turn
    ctx.__dict__["sandbox_config"] = {
        "manager": manager,
        "pending_store": store,
        "llm_providers": {"default": _P(), "exec-prov": _P()},
        "llm_provider_configs": {
            "exec-prov": LLMProviderConfig(type="anthropic", api_keys=["sk-ant"])
        },
        "budget_manager": None,
        "mcp_server_configs": None,
        "otel_receiver": None,
        "sandbox_budget_fraction": 0.5,
        "sandbox_min_budget_tokens": 2000,
    }
    return ctx


async def test_run_sandbox_launches_and_suspends() -> None:
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="fix CI")
    turn.last_plan = ExecutionPlan(success_criteria=["tests pass"])
    queue = _QueueStub()
    runner = FakeCodingAgentRunner(name="claude-code")
    manager = _mk_manager(runner)
    store = MemoryPendingSandboxRunStore()

    result = await _run_sandbox_tool(
        {"brief": "Clone acme/api and run the tests"},
        _ctx(turn, queue, manager, store),
    )

    # The loop suspends: success + suspend flag + correlation payload.
    assert result.suspend is True
    assert result.success is True
    assert result.suspend_payload is not None
    assert result.suspend_payload["turn_id"] == turn.turn_id
    assert result.suspend_payload["sandbox_id"].startswith("fake-sandbox")
    # A pending run row was persisted for resume.
    row = await store.get(turn.turn_id)
    assert row is not None
    assert row.status == "running"
    assert row.coding_agent == "claude-code"
    # The executor's brief reached the coding agent.
    assert "Clone acme/api" in runner.started[0][0]
    # SandboxRunStarted was published (the coordinator's busy gate).
    assert any(e.type == "sandbox_run_started" for _, e in queue.published)
    # The box is NOT torn down — it runs detached.
    assert manager._provider.sandboxes[0].closed is False


async def test_run_sandbox_reuses_existing_box() -> None:
    # A second run_sandbox call in the same turn reuses the same box +
    # checkout instead of provisioning a fresh one.
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    queue = _QueueStub()
    manager = _mk_manager()
    store = MemoryPendingSandboxRunStore()
    ctx = _ctx(turn, queue, manager, store)

    first = await _run_sandbox_tool({"brief": "clone and inspect"}, ctx)
    second = await _run_sandbox_tool({"brief": "now fix it"}, ctx)

    assert first.suspend is True and second.suspend is True
    # Same box across both calls — only one was provisioned.
    assert len(manager._provider.created) == 1
    assert first.suspend_payload["sandbox_id"] == second.suspend_payload["sandbox_id"]


async def test_run_sandbox_requires_brief() -> None:
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    result = await _run_sandbox_tool(
        {"brief": "   "},
        _ctx(turn, _QueueStub(), _mk_manager(), MemoryPendingSandboxRunStore()),
    )
    assert result.success is False
    assert result.suspend is False
    assert "brief" in (result.error or "")


async def test_run_sandbox_rejected_without_turn_context() -> None:
    # Invoked outside a wired turn → clean failure, never an exception.
    ctx = AgentContext(agent_id="a", role="Engineer", event_queue=_QueueStub())
    result = await _run_sandbox_tool({"brief": "do X"}, ctx)
    assert result.success is False
    assert result.suspend is False


async def test_run_sandbox_rejected_when_backend_unconfigured() -> None:
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    ctx = AgentContext(agent_id="a", role="Engineer", event_queue=_QueueStub())
    ctx.__dict__["turn_context"] = turn
    ctx.__dict__["sandbox_config"] = {"manager": None, "pending_store": None}
    result = await _run_sandbox_tool({"brief": "do X"}, ctx)
    assert result.success is False
    assert "not configured" in (result.error or "")


def test_register_gates_on_sandbox_enabled() -> None:
    reg = ToolRegistry()
    register_run_sandbox_tool(reg)
    assert reg.get("run_sandbox") is not None
    check = reg.check_fn_for("run_sandbox")
    assert check is not None
    assert check(CheckContext(sandbox_enabled=True)) is True
    assert check(CheckContext(sandbox_enabled=False)) is False
