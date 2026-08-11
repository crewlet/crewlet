"""The ``run_sandbox`` LLM-callable tool.

Makes the sandbox a **tool the Execute-phase LLM calls**, rather than a
whole-phase backend that replaces Execute. The executor
calls ``run_sandbox`` with a concrete code task; the engine provisions a
sandbox, starts the coding agent (Claude Code / OpenCode) detached, and
**suspends** the Execute tool-loop (the tool returns
``ToolResult(suspend=True)``). When the coding agent finishes, the engine
resumes the loop with the agent's findings spliced in as this call's
result, and the executor continues with full context — reporting, fixing,
or calling the sandbox again — using its normal tools.

The tool is stateless: everything it needs (the parent TurnContext, the
sandbox manager, the pending-run store, provider configs, budgets) is read
from ``context.__dict__["sandbox_config"]``, a dict the TurnEngine
populates once per turn — mirroring ``spawn_subagent_config``.
"""

from __future__ import annotations

from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.execute_sandbox import launch_sandbox_run
from crewlet.agent.plan import ExecutionPlan
from crewlet.agent.turn_context import TurnContext
from crewlet.notifications.coalesce import conversation_key
from crewlet.tools.protocol import AgentContext, CheckContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry

logger = get_logger("tools.run_sandbox")

_PARAMS = {
    "type": "object",
    "properties": {
        "brief": {
            "type": "string",
            "description": (
                "The concrete code task for the sandbox coding agent: name "
                "the repository and the exact change / investigation (e.g. "
                "'Clone github.com/acme/api, run the test suite, and report "
                "which tests fail and why' or 'Fix the failing CI in "
                "acme/api and open a PR'). The coding agent has its own "
                "shell, git checkout, and tools; it ends by writing a "
                "structured report that is handed back to you to continue "
                "the task. You are NOT done after calling this — when it "
                "returns you still report to / act for the requester."
            ),
        },
    },
    "required": ["brief"],
}


async def _run_sandbox_tool(
    params: dict[str, Any], context: AgentContext
) -> ToolResult:
    """Launch a detached sandbox coding run and suspend the Execute loop.

    Reads the engine-attached ``sandbox_config`` + ``turn_context``;
    rejects cleanly when invoked outside a properly-wired TurnEngine turn
    (e.g. an extension monkey-patched ``context``) so the parent turn sees
    a tool failure rather than an exception.
    """
    cfg = getattr(context, "sandbox_config", None)
    turn = getattr(context, "turn_context", None)
    if not isinstance(turn, TurnContext) or not isinstance(cfg, dict):
        return ToolResult(
            success=False,
            error=(
                "run_sandbox requires an active turn context; it can only be "
                "called from within a TurnEngine Execute phase."
            ),
        )
    manager = cfg.get("manager")
    pending_store = cfg.get("pending_store")
    if manager is None or pending_store is None:
        return ToolResult(
            success=False,
            error="the sandbox backend is not configured for this engine.",
        )
    brief = str(params.get("brief", "")).strip()
    if not brief:
        return ToolResult(
            success=False,
            error="run_sandbox requires a non-empty `brief` describing the code task.",
        )

    # The plan produced earlier this turn carries success_criteria / context
    # for the launch + the persisted run; fall back to a minimal plan.
    plan = (
        turn.last_plan if isinstance(turn.last_plan, ExecutionPlan) else ExecutionPlan()
    )
    conv_key = conversation_key(turn.trigger_event) if turn.trigger_event else ""

    # Reuse the same box + checkout across run_sandbox calls in this turn:
    # an existing pending row with a sandbox_id is the
    # paused box from an earlier call — reattach to it instead of provisioning
    # a fresh one, so the coding agent continues on the same disk state. An
    # EMPTY sandbox_id on an existing row means that box is gone (reaped past
    # its pause TTL, or never paused under ``pause_ttl_seconds: 0``), so this
    # call provisions a fresh one and the work re-seeds from the pushed branch.
    existing = await pending_store.get(turn.turn_id)
    reuse_id = existing.sandbox_id if (existing and existing.sandbox_id) else ""

    res = await launch_sandbox_run(
        turn=turn,
        plan=plan,
        manager=manager,
        llm_providers=cfg.get("llm_providers", {}),
        llm_provider_configs=cfg.get("llm_provider_configs", {}),
        event_queue=context.event_queue,
        pending_store=pending_store,
        budget_manager=cfg.get("budget_manager"),
        conversation_key=conv_key,
        mcp_server_configs=cfg.get("mcp_server_configs"),
        otel_receiver=cfg.get("otel_receiver"),
        sandbox_budget_fraction=cfg.get("sandbox_budget_fraction", 0.5),
        sandbox_min_budget_tokens=cfg.get("sandbox_min_budget_tokens", 2000),
        brief_override=brief,
        reuse_sandbox_id=reuse_id,
    )
    if res.skipped:
        # Not enough budget to run the sandbox — surface it as a normal
        # (non-suspending) result so the executor can decide what to do.
        return ToolResult(
            success=False,
            error=f"sandbox not started: {res.skip_reason}.",
        )

    logger.info(
        "run_sandbox_suspended",
        turn_id=turn.turn_id,
        sandbox_id=res.sandbox_id,
        coding_agent=res.coding_agent,
    )
    # Suspend the Execute loop: the engine persists the conversation, ends
    # the turn, and resumes with the coding agent's findings as this call's
    # result once the detached run completes.
    return ToolResult(
        success=True,
        output="(sandbox coding job launched; awaiting its result)",
        suspend=True,
        suspend_payload={"turn_id": turn.turn_id, "sandbox_id": res.sandbox_id},
    )


def register_run_sandbox_tool(registry: ToolRegistry) -> None:
    """Register the ``run_sandbox`` tool, gated on ``sandbox_enabled``.

    Called by the engine alongside the other builtin registrations. The
    ``check_fn`` hides the tool from roles that can't run the sandbox (no
    ``role.sandbox`` enabled, or no sandbox manager wired).
    """
    registry.register(
        SimpleTool(
            name="run_sandbox",
            description=(
                "Run real code work in a sandbox: a fresh computer with a "
                "shell and a git checkout where a coding agent works "
                "autonomously on the `brief` you give it (implement or "
                "modify code, run tests, reproduce a bug, run a script) and "
                "returns a structured report. Use this for any task that "
                "needs a shell or a checkout. It runs detached — your turn "
                "pauses and resumes automatically with the result, so you "
                "keep full context. You are NOT finished when it returns: "
                "report the outcome to / act for the requester with your "
                "other tools (e.g. reply on the originating channel)."
            ),
            parameters=_PARAMS,
            fn=_run_sandbox_tool,
        ),
        check_fn=lambda c: bool(isinstance(c, CheckContext) and c.sandbox_enabled),
    )


__all__ = ["register_run_sandbox_tool"]
