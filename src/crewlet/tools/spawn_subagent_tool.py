"""The ``spawn_subagent`` LLM-callable tool.

``spawn_subagent`` is a tool the Execute-phase LLM can call to run a
short-lived bespoke worker (the ``ToolSurface.for_subagent`` denylist
explicitly filters its name so a sub-agent can never spawn its own).
This module wraps the :func:`crewlet.agent.subagent.spawn_subagent`
Python function as a registered tool.

The tool receives everything it needs (parent TurnContext, LLM
provider map, registry, budgets, timeouts) via the ``AgentContext``
attached by :class:`~crewlet.agent.turn.TurnEngine`. The tool reads
``context.__dict__["spawn_subagent_config"]`` -- a dict the engine
populates once per turn -- so the registered tool instance itself is
stateless and can be shared across roles.
"""

from __future__ import annotations

from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.phase_model import resolve_phase_provider
from crewlet.agent.subagent import SubagentTask, spawn_subagent_batch
from crewlet.agent.subagent import spawn_subagent as _spawn
from crewlet.agent.turn_context import TurnContext
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry

# Keys the TurnEngine attaches to ``context.spawn_subagent_config``.
# Validated up-front so a malformed config produces a clean tool
# failure instead of a ``KeyError`` that aborts the parent turn.
_REQUIRED_CFG_KEYS: frozenset[str] = frozenset({"llm_providers", "tool_registry"})

logger = get_logger("tools.spawn_subagent")


_PARAMS = {
    "type": "object",
    "properties": {
        "task_prompt": {
            "type": "string",
            "description": (
                "The concrete task the sub-agent should complete. "
                "Seeded as its first user message. For a single "
                "sub-agent. Mutually exclusive with ``tasks``."
            ),
        },
        "system_prompt": {
            "type": "string",
            "description": (
                "Task-specific system prompt for the sub-agent. The "
                "runtime appends the mandatory preamble (no further "
                "sub-agents, no colleague contact, return a concise "
                "final answer) automatically."
            ),
        },
        "tool_names": {
            "type": "array",
            "items": {"type": "string"},
            "description": (
                "Names of tools the sub-agent(s) may call. Must be a "
                "subset of your own tool list; ``spawn_subagent``, "
                "``a2a_ask``, and outbound write tools to shared "
                "surfaces (Slack post, Jira comment / update, "
                "Confluence comment, GitHub Copilot review request) "
                "are rejected regardless. The SAME allowlist applies "
                "to all children in a batched call."
            ),
        },
        "max_turns": {
            "type": "integer",
            "description": (
                "Optional cap on tool-call rounds for a single "
                "sub-agent. Capped at the runtime's configured "
                "maximum; asking for more is silently clamped. "
                "Ignored when ``tasks`` is provided -- use the "
                "per-task ``max_turns`` instead."
            ),
        },
        "tasks": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "task_prompt": {"type": "string"},
                    "system_prompt": {"type": "string"},
                    "max_turns": {"type": "integer"},
                },
                "required": ["task_prompt", "system_prompt"],
            },
            "description": (
                "Fan out N sub-agents in parallel. Use for "
                "independent workstreams that can run concurrently "
                "(parallel research, batch reads, multi-source "
                "summarisation). Each entry needs its own "
                "``task_prompt`` and ``system_prompt``; the batch "
                "shares one ``tool_names`` allowlist. The whole "
                "batch shares one budget slice -- greedy children "
                "can starve siblings. Results are returned in input "
                "order. Mutually exclusive with the single-shape "
                "fields above (``task_prompt`` / ``system_prompt`` / "
                "``max_turns``)."
            ),
        },
    },
}


def _validate_batched_tasks(raw: Any) -> tuple[list[SubagentTask] | None, str | None]:
    """Parse the ``tasks: list[dict]`` field into structured
    :class:`SubagentTask` objects. Returns ``(tasks, None)`` on
    success or ``(None, error)`` on the first malformed entry.
    """
    if not isinstance(raw, list):
        return None, f"tasks must be a list; got {type(raw).__name__}"
    if not raw:
        return None, "tasks must not be empty"
    parsed: list[SubagentTask] = []
    for i, entry in enumerate(raw):
        if not isinstance(entry, dict):
            return None, f"tasks[{i}] must be an object; got {type(entry).__name__}"
        tp = entry.get("task_prompt")
        sp = entry.get("system_prompt")
        if not isinstance(tp, str) or not tp:
            return None, f"tasks[{i}].task_prompt must be a non-empty string"
        if not isinstance(sp, str) or not sp:
            return None, f"tasks[{i}].system_prompt must be a non-empty string"
        mt_raw = entry.get("max_turns")
        mt: int | None
        if mt_raw is None:
            mt = None
        elif isinstance(mt_raw, bool):
            return None, f"tasks[{i}].max_turns must be a positive integer"
        elif isinstance(mt_raw, int):
            mt = mt_raw
        elif (isinstance(mt_raw, float) and mt_raw.is_integer()) or (
            isinstance(mt_raw, str) and mt_raw.lstrip("-").isdigit()
        ):
            mt = int(mt_raw)
        else:
            return None, f"tasks[{i}].max_turns must be a positive integer"
        if mt is not None and mt < 1:
            return None, f"tasks[{i}].max_turns must be >= 1, got {mt}"
        parsed.append(SubagentTask(task_prompt=tp, system_prompt=sp, max_turns=mt))
    return parsed, None


async def _spawn_subagent_tool(
    params: dict[str, Any], context: AgentContext
) -> ToolResult:
    """Run an ephemeral sub-agent with the LLM-provided parameters.

    The parent TurnContext + engine-scoped providers/config are read
    from attributes the TurnEngine attaches to ``context`` before
    running the Execute phase.  When those attributes are absent
    (e.g. the tool is invoked outside a turn for some reason) the
    call is rejected with a clear error.
    """
    parent_turn = getattr(context, "turn_context", None)
    cfg = getattr(context, "spawn_subagent_config", None)
    # Validate the engine-attached state before doing anything that
    # could throw.  ``llm_loop.execute_tool`` does NOT catch tool
    # exceptions, so an accidental ``None`` / wrong-shape
    # ``turn_context`` (from an extension that monkey-patches
    # ``context``, say) would otherwise propagate out and abort the
    # entire parent turn rather than producing a clean tool failure.
    if not isinstance(parent_turn, TurnContext):
        return ToolResult(
            success=False,
            error=(
                "spawn_subagent requires an active turn context; "
                "it can only be called from within a TurnEngine turn."
            ),
        )
    if not isinstance(cfg, dict):
        return ToolResult(
            success=False,
            error=(
                "spawn_subagent engine config missing or malformed; "
                "this tool can only be invoked from a TurnEngine turn."
            ),
        )
    missing_keys = _REQUIRED_CFG_KEYS - cfg.keys()
    if missing_keys:
        return ToolResult(
            success=False,
            error=(
                "spawn_subagent engine config missing required keys: "
                f"{sorted(missing_keys)}"
            ),
        )

    # Branch on single vs batched shape.  Mutual exclusion is
    # enforced explicitly in Python (oneOf-style JSON schemas confuse
    # LLM tool-call serialisers; they sometimes emit a hybrid both-
    # branches payload).
    raw_tasks = params.get("tasks")
    has_single = bool(params.get("task_prompt")) or bool(params.get("system_prompt"))
    has_batch = raw_tasks is not None
    if has_single and has_batch:
        return ToolResult(
            success=False,
            error=(
                "spawn_subagent: pass either ``task_prompt`` + "
                "``system_prompt`` (single) OR ``tasks`` (batched), "
                "not both"
            ),
        )

    task_prompt = params.get("task_prompt", "")
    system_prompt = params.get("system_prompt", "")
    if not has_batch and (not task_prompt or not system_prompt):
        return ToolResult(
            success=False,
            error=(
                "task_prompt and system_prompt are required for a "
                "single-shape call; or pass ``tasks: [...]`` for "
                "a batched call"
            ),
        )
    # Only fall back to an empty allowlist when ``tool_names`` is
    # absent / ``None`` -- a ``or []`` default would silently swallow
    # other falsy values like ``""`` or ``0`` and bypass the type
    # check below, turning malformed LLM output into "spawn a tool-
    # less sub-agent" which is almost never what the caller meant.
    tool_names = params.get("tool_names")
    if tool_names is None:
        tool_names = []
    if not isinstance(tool_names, list):
        return ToolResult(
            success=False,
            error=f"tool_names must be a list; got {type(tool_names).__name__}",
        )
    # Guard against malformed LLM output: non-string entries (dicts,
    # numbers, nested lists) would later blow up the ``frozenset[str]``
    # membership check in ``ToolSurface.for_subagent`` with a
    # ``TypeError`` for unhashable items.
    if not all(isinstance(name, str) for name in tool_names):
        bad = [n for n in tool_names if not isinstance(n, str)]
        return ToolResult(
            success=False,
            error=f"tool_names entries must be strings; got {bad!r}",
        )

    # Coerce max_turns to int when present.  LLM-generated tool
    # arguments can arrive as strings / floats; accept ints and
    # digit-only strings but reject non-integer floats (``3.9``
    # would silently truncate to ``3`` and mis-cap the sub-agent).
    raw_max_turns = params.get("max_turns")
    max_turns_request: int | None
    if raw_max_turns is None:
        max_turns_request = None
    else:
        if isinstance(raw_max_turns, bool):
            # ``bool`` is a subclass of ``int``; reject explicitly so
            # ``True`` / ``False`` don't silently become ``1`` / ``0``.
            return ToolResult(
                success=False,
                error=f"max_turns must be a positive integer; got {raw_max_turns!r}",
            )
        if isinstance(raw_max_turns, float):
            if not raw_max_turns.is_integer():
                return ToolResult(
                    success=False,
                    error=(
                        f"max_turns must be a positive integer "
                        f"(not a fractional float); got {raw_max_turns!r}"
                    ),
                )
            max_turns_request = int(raw_max_turns)
        elif isinstance(raw_max_turns, int):
            max_turns_request = raw_max_turns
        elif isinstance(raw_max_turns, str) and raw_max_turns.lstrip("-").isdigit():
            # Digit-only string (optionally signed).  ``int()`` below
            # handles both ``"5"`` and ``"-5"``; the range check
            # right after rejects non-positive values.
            max_turns_request = int(raw_max_turns)
        else:
            return ToolResult(
                success=False,
                error=f"max_turns must be a positive integer; got {raw_max_turns!r}",
            )
        if max_turns_request < 1:
            return ToolResult(
                success=False,
                error=f"max_turns must be >= 1, got {max_turns_request}",
            )

    providers: dict[str, Any] = cfg["llm_providers"]
    provider_key, provider = resolve_phase_provider(
        parent_turn.agent.definition.role, "subagent", providers
    )

    registry: ToolRegistry = cfg["tool_registry"]
    role_mcp_tools = cfg.get("role_mcp_tools", [])
    parent_tool_names: list[str] = cfg.get("parent_tool_names", [])

    # Batched-shape dispatch.
    if has_batch:
        parsed_tasks, parse_err = _validate_batched_tasks(raw_tasks)
        if parse_err is not None:
            return ToolResult(success=False, error=parse_err)
        max_parallel = int(cfg.get("subagent_max_parallel", 3))
        # Defence in depth: the config validator clamps this to 1..10
        # but defaulting here keeps test paths sound.
        if max_parallel < 1:
            max_parallel = 1
        if max_parallel > 10:
            max_parallel = 10
        results = await spawn_subagent_batch(
            parent_turn=parent_turn,
            tasks=parsed_tasks or [],
            tool_names=tool_names,
            parent_tool_names=parent_tool_names,
            provider=provider,
            provider_key=provider_key,
            registry=registry,
            role_mcp_tools=role_mcp_tools,
            agent_context=context,
            event_queue=context.event_queue,
            max_turns_cap=cfg.get("max_turns_cap", 20),
            timeout_seconds=cfg.get("timeout_seconds", 120.0),
            batch_timeout_seconds=cfg.get("subagent_batch_timeout_seconds", 120.0),
            max_parallel=max_parallel,
            min_per_child_tokens=int(cfg.get("subagent_min_per_child_tokens", 500)),
            budget_fraction=cfg.get("budget_fraction", 0.2),
            budget_manager=cfg.get("budget_manager"),
            observability=cfg.get("observability"),
            token_usage_repo=cfg.get("token_usage_repo"),
            validators=cfg.get("validators"),
            prompt_skill_registry=cfg.get("prompt_skill_registry"),
        )
        # Render results as a JSON-shaped tool output. The planner /
        # executor can parse this; the rendered text is also stable
        # enough to embed in trace events as-is.
        import json as _json

        rendered = _json.dumps(
            {
                "results": [
                    {
                        "index": i,
                        "text": r.text,
                        "turns_used": r.turns_used,
                        "tokens_used": r.tokens_used,
                        "rejected_tools": r.rejected_tools,
                        "timed_out": r.timed_out,
                        "error": r.error,
                        "model": r.model,
                    }
                    for i, r in enumerate(results)
                ]
            }
        )
        # The tool itself is "successful" -- per-child errors are
        # carried inside the JSON payload so the planner can pick out
        # which sub-tasks succeeded and which need a retry. This
        # mirrors how ``asyncio.gather(return_exceptions=True)``
        # works.
        return ToolResult(success=True, output=rendered)

    result = await _spawn(
        parent_turn=parent_turn,
        task_prompt=task_prompt,
        system_prompt=system_prompt,
        tool_names=tool_names,
        parent_tool_names=parent_tool_names,
        provider=provider,
        provider_key=provider_key,
        registry=registry,
        role_mcp_tools=role_mcp_tools,
        agent_context=context,
        event_queue=context.event_queue,
        max_turns_cap=cfg.get("max_turns_cap", 20),
        timeout_seconds=cfg.get("timeout_seconds", 120.0),
        max_turns_request=max_turns_request,
        budget_fraction=cfg.get("budget_fraction", 0.2),
        budget_manager=cfg.get("budget_manager"),
        observability=cfg.get("observability"),
        token_usage_repo=cfg.get("token_usage_repo"),
        validators=cfg.get("validators"),
        prompt_skill_registry=cfg.get("prompt_skill_registry"),
    )

    if result.timed_out:
        return ToolResult(
            success=False,
            error=f"sub-agent timed out after {cfg.get('timeout_seconds', 120.0)}s",
        )
    if result.rejected_tools:
        logger.info(
            "spawn_subagent_tool_rejected_some_tools",
            rejected=result.rejected_tools,
        )
    return ToolResult(success=True, output=result.text)


def register_spawn_subagent_tool(registry: ToolRegistry) -> None:
    """Register the ``spawn_subagent`` tool with ``registry``.

    Called by ``TurnEngine`` during engine startup alongside
    :func:`crewlet.tools.builtin.register_builtin_tools` and
    :func:`crewlet.tools.colleague.register_colleague_tools`.
    """
    registry.register(
        SimpleTool(
            name="spawn_subagent",
            description=(
                "Spawn an ephemeral sub-agent to do a narrowly-scoped "
                "task on your behalf (for example: web research, code "
                "execution, large-doc summarisation). The sub-agent "
                "runs with ONLY the tools you name in ``tool_names`` "
                "(subset of your own tools, minus spawn_subagent "
                "itself and all colleague-surface tools), returns a "
                "concise text answer, and cannot itself spawn further "
                "sub-agents or contact colleagues. Use this when the "
                "task is a bounded capability call rather than work "
                "that belongs with a colleague."
            ),
            parameters=_PARAMS,
            fn=_spawn_subagent_tool,
        )
    )


__all__ = ["register_spawn_subagent_tool"]
