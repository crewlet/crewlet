"""Sandbox launch / collect plumbing for the ``run_sandbox`` tool.

Code work runs via the ``run_sandbox`` Execute tool (see
:mod:`crewlet.tools.run_sandbox_tool`): a coding agent (Claude Code /
OpenCode) works in an isolated sandbox with its own shell + git checkout.
This module owns the shared, engine-side plumbing the tool + coordinator
use:

- :func:`launch_sandbox_run` — provision (or reuse) a box, start the coding
  agent detached, persist the ``pending_sandbox_run`` row, emit
  :class:`SandboxRunStarted`. The tool returns ``ToolResult(suspend=True)``
  and the Execute loop suspends.
- :func:`collect_sandbox_run` — on completion, reattach by ``sandbox_id``,
  collect the result, and either pause the box (reuse) or tear it down.
- :func:`teardown_sandbox_run` — close a reused box when the Execute phase
  is done with it.
- credential / spec / brief / MCP / OTel assembly (``_prepare_launch``).

The coding agent calls the LLM itself, so token accounting is **post-hoc**
(cap + post-account; :func:`account_sandbox_tokens`).
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.llm_loop import (
    LoopResult,
    publish_phase_completed,
)
from crewlet.agent.phase_model import resolve_phase_provider
from crewlet.agent.plan import ExecutionPlan
from crewlet.agent.turn_context import TurnContext
from crewlet.config import (
    LLMProviderConfig,
    resolve_setup_step_content,
)
from crewlet.events.types import SandboxRunStarted
from crewlet.providers.llm.protocol import LLMProvider, Message
from crewlet.queue.protocol import EventQueue
from crewlet.queue.topics import agent_control_topic
from crewlet.redaction import redact_secrets
from crewlet.sandbox.coding_agents._detached import clear_run_artifacts
from crewlet.sandbox.coding_agents.ask import ask_brief_instruction
from crewlet.sandbox.credentials import build_sandbox_env, cli_credential_files
from crewlet.sandbox.manager import SandboxManager
from crewlet.sandbox.mcp_render import resolve_sandbox_mcp
from crewlet.sandbox.pending_store import PendingSandboxRun, PendingSandboxRunStore
from crewlet.sandbox.protocol import (
    CodingAgentLLM,
    CodingAgentResult,
    RunHandle,
    RunLimits,
    SandboxSpec,
)
from crewlet.sandbox.setup import (
    SandboxSetupStep,
    environment_brief,
    setup_env,
)

logger = get_logger("agent.execute_sandbox")

# Coarse tokens-per-round estimate used to translate a budget fraction
# into the coding agent's ``--max-turns`` self-limit.
_TOKENS_PER_ROUND = 2000


def compose_brief(plan: ExecutionPlan, task_description: str) -> str:
    """Fallback brief from the plan, when the run_sandbox tool gave none.

    The ``run_sandbox`` tool normally passes the executor's own ``brief``
    (``brief_override``); this assembles a brief from the plan summary +
    acceptance criteria + task as a fallback when that is empty.
    """
    lines: list[str] = []
    summary = plan.summary()
    if summary:
        lines.append(summary)
    if plan.success_criteria:
        lines.append("\nAcceptance criteria:")
        lines.extend(f"- {c}" for c in plan.success_criteria)
    if task_description:
        lines.append(f"\nTask:\n{task_description}")
    return "\n".join(lines).strip() or task_description


def _team_context(turn: TurnContext) -> tuple[list[str], str]:
    """Best-effort (roster handles, manager handle) for the ask brief.

    Returns the agent's unit teammates and the unit lead so the coding
    agent can name a real person in ``crewlet-ask --to``. Defensive: any
    missing org structure yields empty values (the instruction still
    works without names).
    """
    org = turn.org
    role_name = turn.agent.role_name
    try:
        for unit in getattr(org, "units", []) or []:
            roles = getattr(unit, "roles", []) or []
            if role_name not in {getattr(r, "name", "") for r in roles}:
                continue
            roster = [
                getattr(r, "handle", "") or getattr(r, "name", "")
                for r in roles
                if getattr(r, "name", "") != role_name
            ]
            roster = [h for h in roster if h]
            lead_name = getattr(unit, "lead", "")
            manager = ""
            if lead_name and lead_name != role_name:
                for r in roles:
                    if getattr(r, "name", "") == lead_name:
                        manager = getattr(r, "handle", "") or lead_name
                        break
            return roster, manager
    except Exception:
        logger.debug("team_context_failed", role=role_name)
    return [], ""


def _otel_env(
    turn: TurnContext,
    otel_receiver: Any = None,
    ttl_seconds: float = 900.0,
    *,
    coding_agent: str = "claude-code",
) -> dict[str, str]:
    """Non-secret OTel env for the in-sandbox agent.

    Injects the endpoint + exporter flags + per-turn resource attributes
    and a ``TRACEPARENT`` so the coding agent's telemetry nests in the
    turn's ``agent.turn.execute.sandbox`` span. The OTLP **ingest token**
    (``OTEL_EXPORTER_OTLP_HEADERS``) is deliberately NOT injected.

    When an engine-fronted ``otel_receiver`` is wired, the endpoint is the
    receiver URL carrying a per-run, trace-scoped, expiring token (the
    receiver adds upstream auth outside the sandbox). Otherwise it falls
    back to a directly-configured ``OTEL_EXPORTER_OTLP_ENDPOINT`` (a
    co-located tokenless collector). Returns ``{}`` when neither is set.

    The ``CLAUDE_CODE_*`` telemetry toggles are emitted **only for Claude
    Code** — it is the coding agent that natively exports OTLP. OpenCode
    does not emit OTLP today, so it gets only the standard ``OTEL_*`` vars
    (harmless, future-proof); its in-sandbox work shows up via the engine-
    side run-lifecycle events (the dashboard's Running-sandboxes panel + the
    detached Execute phase event), not per-LLM-call traces.
    """
    from crewlet.telemetry import current_span_id, current_trace_id

    tid, sid = current_trace_id(), current_span_id()
    if otel_receiver is not None:
        endpoint, _token = otel_receiver.endpoint_for(
            tid or turn.turn_id, ttl_seconds=ttl_seconds
        )
    else:
        endpoint = os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
    if not endpoint:
        return {}

    env = {
        "OTEL_METRICS_EXPORTER": "otlp",
        "OTEL_LOGS_EXPORTER": "otlp",
        "OTEL_TRACES_EXPORTER": "otlp",
        "OTEL_EXPORTER_OTLP_ENDPOINT": endpoint,
        "OTEL_RESOURCE_ATTRIBUTES": (
            f"service.name=crewlet-sandbox,"
            f"crewlet.turn_id={turn.turn_id},"
            f"crewlet.agent_handle={turn.agent.handle}"
        ),
    }
    if coding_agent == "claude-code":
        env["CLAUDE_CODE_ENABLE_TELEMETRY"] = "1"
        env["CLAUDE_CODE_ENHANCED_TELEMETRY_BETA"] = "1"
    protocol = os.environ.get("OTEL_EXPORTER_OTLP_PROTOCOL", "")
    if protocol:
        env["OTEL_EXPORTER_OTLP_PROTOCOL"] = protocol
    if tid and sid:
        env["TRACEPARENT"] = f"00-{tid}-{sid}-01"
    return env


def _remaining_budget(budget_manager: Any, agent_id: str) -> int | None:
    """Return the agent's remaining token budget, or ``None`` when uncapped."""
    if budget_manager is None:
        return None
    try:
        agent_budget = budget_manager.get_agent_budget(agent_id)
    except Exception:
        return None
    if agent_budget is None or getattr(agent_budget, "max_tokens", 0) <= 0:
        return None
    return max(0, agent_budget.max_tokens - agent_budget.used_tokens)


@dataclass
class _Launch:
    """Everything resolved up-front to mint and drive one sandbox run."""

    provider_key: str
    coding_agent: str
    llm: CodingAgentLLM
    env: dict[str, str]
    spec: SandboxSpec
    brief: str
    limits: RunLimits
    remaining: int | None
    mcp_servers: dict[str, dict]
    setup_steps: list[SandboxSetupStep]


def _coding_agent_llm(llm_config: LLMProviderConfig) -> CodingAgentLLM:
    """The role's resolved LLM as a coding-agent descriptor.

    Carries the raw model id + model family + endpoint. The runner uses it
    to address the model and (OpenCode) declare a custom provider; the key
    rides the sandbox env, so it is deliberately absent here. The model is
    NOT an env var the agent reads — passed explicitly, or the agent falls
    back to its own built-in default (e.g. OpenCode → ``gpt-5.*``).

    A ``cli-agent`` provider reports its CLI's **vendor** as the family:
    every subscription entry has the same ``type``, so passing the type
    through would tell OpenCode to address a Claude subscription's
    ``sonnet`` as ``openai/sonnet``.
    """
    provider_type = llm_config.type
    if provider_type == "cli-agent" and llm_config.cli is not None:
        provider_type = llm_config.cli.profile().vendor
    return CodingAgentLLM(
        model=(llm_config.model or "").strip(),
        provider_type=provider_type,
        base_url=llm_config.base_url,
    )


def _prepare_launch(
    *,
    turn: TurnContext,
    plan: ExecutionPlan,
    manager: SandboxManager,
    llm_providers: dict[str, LLMProvider],
    llm_provider_configs: dict[str, LLMProviderConfig],
    budget_manager: Any,
    sandbox_budget_fraction: float,
    mcp_server_configs: Any = None,
    otel_receiver: Any = None,
    brief_override: str = "",
) -> _Launch:
    """Resolve provider creds, sandbox spec, brief, run limits, MCP surface.

    ``brief_override`` is the concrete task supplied by the ``run_sandbox``
    tool (the executor's words for the coding agent). When empty the brief
    is composed from the plan. The ask + findings addenda are always
    appended.
    """
    role = turn.agent.definition.role
    sandbox_cfg = role.sandbox
    assert sandbox_cfg is not None, "sandbox launch requires role.sandbox"
    # An empty role-level coding_agent inherits the provider default
    # (``providers.sandbox.default_coding_agent``); a role value overrides
    # it. Resolve once here so the creds (build_sandbox_env), the launch
    # spec, and the run record all agree on the effective agent.
    coding_agent = sandbox_cfg.coding_agent or manager.default_coding_agent

    provider_key, _provider = resolve_phase_provider(role, "sandbox", llm_providers)
    llm_config = llm_provider_configs.get(provider_key) or LLMProviderConfig()
    # There is NO run-time limit on a coding job: the detached job is never
    # force-stopped on a timer, and the box's TTL is refreshed every waiter
    # tick (keepalive) so the clock never kills a running job. ``box_ttl_s`` is
    # therefore just the box's INITIAL TTL / orphan-reclaim grace (how long it
    # outlives an engine that stops heart-beating), not a run deadline.
    # Completion is detected by tracking the job (done marker / terminal stream
    # event / process liveness), never by a wall-clock kill.
    box_ttl_s = manager.default_timeout_s
    # The OTel token outlives the initial TTL by a buffer so late-flushed
    # telemetry still validates (the engine-owned span bounds the real
    # correlation).
    otel_ttl = box_ttl_s + 300.0
    # Sandbox provisioning comes ENTIRELY from config (setup-step framework,
    # ``crewlet.sandbox.setup``): ``manager.default_setup`` carries the
    # engine-wide ``providers.sandbox.setup`` steps (the documented git-auth
    # recipe lives there when the deployment wants it — the engine ships no
    # steps of its own); the role's own ``role.sandbox.setup`` appends here.
    # The manager applies files+commands at acquire (with this run env, so
    # recipes can reference $CREWLET_AGENT_* / their configured tokens); env
    # and brief thread through below. The engine's env contribution is
    # tool-agnostic only: LLM creds + the agent's identity — external-tool
    # tokens (GITHUB_TOKEN, …) are declared in role.sandbox.env / step env.
    agent = turn.agent
    agent_handle = agent.handle or agent.role_name or "crewlet-agent"
    # Role-level steps get the same ``${VAR}`` treatment as provider-level
    # ones (resolved when the manager was built): files + commands resolved
    # here, env left verbatim so it resolves exactly ONCE in
    # ``build_sandbox_env`` below — same semantics as ``role.sandbox.env``.
    role_steps = [resolve_setup_step_content(s) for s in sandbox_cfg.setup]
    setup_steps: list[SandboxSetupStep] = [
        *manager.default_setup,
        *role_steps,
    ]
    env = build_sandbox_env(
        coding_agent=coding_agent,
        llm_config=llm_config,
        role_sandbox_env=dict(sandbox_cfg.env),
        agent_handle=agent_handle,
        agent_email=agent.email or f"{agent_handle}@crewlet.local",
        setup_env=setup_env(setup_steps),
        otel_env=_otel_env(turn, otel_receiver, otel_ttl, coding_agent=coding_agent),
    )

    remaining = _remaining_budget(budget_manager, turn.agent.id_str)
    # ``timeout_s`` here bounds only the INLINE run() path's exec wait (tests /
    # short jobs); the detached start() path the engine actually uses runs
    # uncapped, so this never caps a real coding job.
    limits = RunLimits(timeout_s=box_ttl_s)
    if remaining is not None:
        # Derive a coarse round cap from the budget fraction; the runner
        # also enforces the coding agent's own --max-* caps (D/F).
        limits.max_turns = max(
            1, int(remaining * sandbox_budget_fraction / _TOKENS_PER_ROUND)
        )

    # Scope the in-sandbox MCP surface to the role's servers. Empty
    # when the role didn't opt any servers in or the engine didn't thread its
    # server configs (tests / no-MCP deployments). Scoping is server-level
    # only — the agent gets every tool those servers expose. Resolved before
    # the brief so the environment block can name the connected servers.
    mcp_servers: dict[str, dict] = {}
    mcp_cfg = sandbox_cfg.mcp
    if mcp_server_configs and mcp_cfg.servers:
        mcp_servers = resolve_sandbox_mcp(
            list(mcp_server_configs),
            role.mcp_env,
            mcp_cfg.servers,
        )

    brief = brief_override.strip() or compose_brief(plan, turn.task_description or "")
    # Tell the agent about its runtime (each setup step's brief: git is
    # pre-authed, where the token is; plus which MCP servers it has) so it
    # doesn't fail trying to clone, then how to ask a person when it gets
    # blocked — the roster + manager give it real names to target.
    roster, manager_handle = _team_context(turn)
    # The report-file instruction is NOT appended here: its path depends on
    # the sandbox the run lands in (``RunPaths.findings``), which does not
    # exist yet. ``DetachedFileRunner`` appends it at start/run time.
    brief = (
        brief
        + "\n"
        + environment_brief(setup_steps, mcp_servers=list(mcp_servers))
        + "\n"
        + ask_brief_instruction(roster, manager_handle)
    )

    # The box's INITIAL TTL is the keepalive window; the waiter refreshes it
    # every tick so a running job is never killed by the clock (no run-time
    # TTL). ``pause_ttl_s`` separately governs a paused (blocked / reused) box.
    spec = manager.build_spec(
        coding_agent=coding_agent,
        timeout_s=box_ttl_s,
        pause_ttl_s=sandbox_cfg.pause_ttl_seconds,
        env=env,
        # A subscription CLI login, for a backend that can use the files
        # (local). E2B ignores these and rides the headless token
        # build_sandbox_env already exported.
        credential_files=cli_credential_files(provider_key, llm_config),
    )
    return _Launch(
        provider_key=provider_key,
        coding_agent=coding_agent,
        llm=_coding_agent_llm(llm_config),
        env=env,
        spec=spec,
        brief=brief,
        limits=limits,
        remaining=remaining,
        mcp_servers=mcp_servers,
        setup_steps=setup_steps,
    )


async def account_sandbox_tokens(
    *,
    turn: TurnContext,
    result: CodingAgentResult,
    provider_key: str,
    budget_manager: Any,
    token_usage_repo: Any,
) -> int:
    """Post-account a coding-agent run's tokens.

    Folds usage into the turn totals, the budget cascade, and the durable
    ledger. Telemetry failures never abort the turn. Returns the total.
    """
    tokens = result.input_tokens + result.output_tokens
    turn.input_tokens += result.input_tokens
    turn.output_tokens += result.output_tokens
    turn.model_keys["execute"] = provider_key
    if budget_manager is not None and tokens:
        try:
            # ``record_spend``, not ``consume``: the sandbox has already
            # burned these tokens. ``consume`` REFUSES a charge that
            # would exceed the cap and increments nothing, and the
            # result was being discarded here -- so a run that busted
            # the budget was neither counted nor blocked, and the meter
            # read low exactly when the cap was binding.
            if await budget_manager.record_spend(turn.agent.id_str, tokens):
                logger.warning(
                    "sandbox_spend_over_budget",
                    agent_id=turn.agent.id_str,
                    tokens=tokens,
                )
        except Exception:
            logger.exception("sandbox_budget_consume_failed")
    if token_usage_repo is not None and tokens and turn.agent.handle:
        try:
            await token_usage_repo.increment(turn.agent.handle, tokens)
        except Exception:
            logger.exception("sandbox_token_usage_increment_failed")
    return tokens


def _sandbox_phase_response(result: CodingAgentResult) -> str:
    """Dashboard-facing text for a sandbox Execute phase.

    The coding agent's final summary plus its captured activity transcript
    (tool calls, shell commands, todos) — the closest thing to a trace for an
    agent (e.g. OpenCode) that emits no OTLP, rendered by the dashboard's
    collapsible phase view. Secrets are redacted here, at the boundary where
    the transcript first crosses into a published event.
    """
    parts: list[str] = []
    if result.text:
        parts.append(result.text)
    if result.transcript:
        parts.append("--- Coding-agent transcript ---\n" + result.transcript)
    if result.error and not result.success:
        parts.append("--- Error ---\n" + result.error)
    return redact_secrets("\n\n".join(parts).strip())


async def publish_sandbox_completion_phase(
    *,
    event_queue: EventQueue,
    agent: Any,
    run: PendingSandboxRun,
    result: CodingAgentResult,
) -> None:
    """Publish a detached run's real Execute phase at completion.

    The kick-off only published a "launched" placeholder; this emits the
    finished phase — carrying the coding agent's transcript + summary —
    attributed to the **original kick-off turn**, so the dashboard shows what
    the sandbox actually did under that turn. Telemetry only: failures are
    swallowed by ``publish_phase_completed``.
    """
    if agent is None:
        return
    loop = LoopResult(
        text=_sandbox_phase_response(result),
        input_tokens=result.input_tokens,
        output_tokens=result.output_tokens,
        model=run.coding_agent,
        messages=[Message(role="system", content=run.task_description or "")],
    )
    notes = f"backend=sandbox coding_agent={run.coding_agent} detached(completed)"
    if result.delivered_refs:
        notes += f" delivered={','.join(result.delivered_refs)}"
    if not result.success:
        notes += " failed"
    await publish_phase_completed(
        event_queue=event_queue,
        agent=agent,
        turn_id=run.turn_id,
        iteration=1,
        phase="execute",
        provider_key=run.coding_agent,
        loop=loop,
        decision="done" if result.success else "",
        notes=notes,
        tools_available=[run.coding_agent],
        tag_span=False,
    )


@dataclass
class SandboxLaunchResult:
    """Outcome of :func:`launch_sandbox_run`."""

    sandbox_id: str = ""
    coding_agent: str = ""
    provider_key: str = ""
    brief: str = ""
    skipped: bool = False
    skip_reason: str = ""


async def launch_sandbox_run(
    *,
    turn: TurnContext,
    plan: ExecutionPlan,
    manager: SandboxManager,
    llm_providers: dict[str, LLMProvider],
    llm_provider_configs: dict[str, LLMProviderConfig],
    event_queue: EventQueue,
    pending_store: PendingSandboxRunStore,
    budget_manager: Any = None,
    conversation_key: str = "",
    mcp_server_configs: Any = None,
    otel_receiver: Any = None,
    sandbox_budget_fraction: float = 0.5,
    sandbox_min_budget_tokens: int = 2000,
    brief_override: str = "",
    reuse_sandbox_id: str = "",
) -> SandboxLaunchResult:
    """Provision a sandbox, start the coding agent detached, persist its run.

    The shared launch core: the ``run_sandbox`` tool passes
    ``brief_override`` (the executor's task); an empty override composes
    the brief from the plan. Runs ``_prepare_launch`` → budget-floor
    check → acquire → ``runner.start`` → persist the ``pending_sandbox_run``
    row → publish :class:`SandboxRunStarted`, and returns the ids the
    caller needs. The sandbox is **not** torn down here; it runs server-side
    until completion (collect / resume).

    ``reuse_sandbox_id`` (a follow-up ``run_sandbox`` call in a resumed
    Execute loop) reattaches to that paused box + checkout instead of
    provisioning a fresh one: the prior run's result artifacts are cleared,
    the existing pending row is re-pointed at the job and flipped back to
    ``running``, and the coding agent starts again on the same disk state.
    Empty means provision a fresh box — either the first call of the turn,
    or a re-seed after the pause reaper reclaimed the paused box, in which
    case the existing row is re-pointed at the NEW box.
    """
    launch = _prepare_launch(
        turn=turn,
        plan=plan,
        manager=manager,
        llm_providers=llm_providers,
        llm_provider_configs=llm_provider_configs,
        budget_manager=budget_manager,
        sandbox_budget_fraction=sandbox_budget_fraction,
        mcp_server_configs=mcp_server_configs,
        otel_receiver=otel_receiver,
        brief_override=brief_override,
    )

    if launch.remaining is not None and launch.remaining < sandbox_min_budget_tokens:
        logger.warning(
            "sandbox_budget_floor",
            agent=turn.agent.handle,
            remaining=launch.remaining,
            floor=sandbox_min_budget_tokens,
        )
        return SandboxLaunchResult(
            coding_agent=launch.coding_agent,
            skipped=True,
            skip_reason="remaining token budget below the floor",
        )

    if reuse_sandbox_id:
        sandbox, runner = await manager.reconnect(reuse_sandbox_id, launch.coding_agent)
        await clear_run_artifacts(sandbox)
    else:
        sandbox, runner = await manager.acquire(launch.spec, setup=launch.setup_steps)
    # Start the background job. On a start failure, tear down a box WE just
    # provisioned (never a reused one — its earlier work must survive), then
    # re-raise.
    try:
        handle = await runner.start(
            sandbox,
            brief=launch.brief,
            env=launch.env,
            limits=launch.limits,
            llm=launch.llm,
            mcp_servers=launch.mcp_servers,
        )
    except Exception:
        logger.exception("sandbox_start_failed", sandbox_id=sandbox.id)
        if not reuse_sandbox_id:
            await sandbox.close()
        raise

    # Point the run's row at the box that is now executing it. A row already
    # exists whenever this turn launched before — a follow-up ``run_sandbox``
    # reusing the paused box, or one re-seeding after the reaper reclaimed it
    # — and it must pick up THIS job's sandbox + command id, not keep the
    # previous pair, or the waiter polls the wrong box and the completion
    # never fires. The flip back to ``running`` also re-arms the at-most-once
    # claim gate for the next completion.
    if await pending_store.get(turn.turn_id) is not None:
        await pending_store.attach_sandbox(
            turn.turn_id,
            sandbox_id=sandbox.id,
            command_id=handle.command_id,
            session_id=handle.session_id,
        )
    else:
        run = PendingSandboxRun(
            turn_id=turn.turn_id,
            agent_handle=turn.agent.handle,
            agent_id=turn.agent.id_str,
            role=turn.agent.role_name,
            sandbox_id=sandbox.id,
            coding_agent=launch.coding_agent,
            command_id=handle.command_id,
            status="running",
            plan=plan.model_dump(mode="json"),
            task_description=turn.task_description or "",
            success_criteria=list(plan.success_criteria),
            conversation_key=conversation_key,
            notification_metadata=dict(turn.notification_metadata or {}),
            session_id=handle.session_id,
            trace_id=turn.trigger_event.trace_id if turn.trigger_event else "",
            span_id=turn.trigger_event.span_id if turn.trigger_event else "",
            budget_remaining=launch.remaining or 0,
            delegation_depth=turn.delegation_depth,
            delegation_chain=list(turn.delegation_chain),
            pause_ttl_seconds=launch.spec.pause_ttl_s,
        )
        await pending_store.create(run)

    started = SandboxRunStarted(
        source=turn.agent.role_name,
        agent_id=turn.agent.id_str,
        agent_handle=turn.agent.handle,
        role=turn.agent.role_name,
        turn_id=turn.turn_id,
        sandbox_id=sandbox.id,
        coding_agent=launch.coding_agent,
        conversation_key=conversation_key,
        task_id=turn.task_id,
        task=(launch.brief or turn.task_description or "").strip()[:160],
    )
    # Announced under ``crewlet.events.*`` for the dashboard's broadcast
    # stream; ACTED ON via the seat's own control topic, so the busy gate
    # is applied by the node that owns the seat and nobody else.
    await event_queue.publish("crewlet.events.sandbox_run_started", started)
    control = agent_control_topic(turn.agent.handle)
    if control:
        await event_queue.publish(control, started)

    logger.info(
        "sandbox_run_started",
        agent=turn.agent.handle,
        turn_id=turn.turn_id,
        sandbox_id=sandbox.id,
        coding_agent=launch.coding_agent,
        command_id=handle.command_id,
    )
    return SandboxLaunchResult(
        sandbox_id=sandbox.id,
        coding_agent=launch.coding_agent,
        provider_key=launch.provider_key,
        brief=launch.brief,
    )


async def collect_sandbox_run(
    *,
    run: PendingSandboxRun,
    manager: SandboxManager,
    pending_store: PendingSandboxRunStore,
    teardown: bool = True,
) -> CodingAgentResult:
    """Reconnect to a detached run's sandbox and collect its result.

    The completion side of the detached lifecycle: reattach by
    ``sandbox_id`` (auto-resumes a paused box), collect the coding-agent
    result + diff. With ``teardown=True`` (single-shot collection, no reuse)
    the box is then closed. With ``teardown=False`` (the engine's
    sandbox-as-a-tool path) it is **paused** instead, so a follow-up
    ``run_sandbox`` call in the
    resumed Execute loop can reuse the same checkout + coding-agent session;
    the box is torn down when the Execute phase ends
    (:func:`teardown_sandbox_run`).

    Either way the run's row is updated to match — a paused box starts its
    TTL (:meth:`~PendingSandboxRunStore.mark_box_paused`), a closed one is
    released — so the pause reaper and the next launch both read the truth
    rather than a stale sandbox id. Token accounting is the caller's job via
    :func:`account_sandbox_tokens`.
    """
    runner = manager.runner_for(run.coding_agent)
    sandbox = await manager.provider.connect(run.sandbox_id)
    handle = RunHandle(command_id=run.command_id, session_id=run.session_id)
    try:
        result = await runner.collect(sandbox, handle)
    finally:
        if teardown:
            await sandbox.close()
            await pending_store.release_box(run.turn_id)
            # Keep the caller's in-memory row in step with the store. The
            # memory store hands back the SAME object (so it is already
            # updated) while Postgres hands back a detached copy — without
            # this the two backends disagree about whether a box still
            # exists, and only the tested one would be right.
            run.sandbox_id, run.paused_at = "", None
        else:
            # Keep the box for reuse across run_sandbox calls in this turn;
            # pausing snapshots it so an idle gap doesn't burn its TTL. This
            # pause is short — the resume dispatches straight after — and is
            # settled by that tail, so it is NOT what ``pause_ttl`` governs;
            # that knob bounds the open-ended wait a *blocked* run parks
            # in, which the coordinator decides on the needs_input path.
            try:
                await sandbox.pause()
            except Exception:
                # Record the pause anyway: the box still EXISTS, so the row
                # must keep pointing at it or nothing can ever reclaim it. A
                # box that failed to pause is simply still running and dies
                # on its own TTL, which a later kill tolerates.
                logger.exception("sandbox_pause_failed", sandbox_id=run.sandbox_id)
            await pending_store.mark_box_paused(run.turn_id)
            run.paused_at = datetime.now(UTC)
    logger.info(
        "sandbox_run_collected",
        agent=run.agent_handle,
        turn_id=run.turn_id,
        sandbox_id=run.sandbox_id,
        success=result.success,
        delivered=bool(result.delivered_refs),
        teardown=teardown,
    )
    return result


async def teardown_sandbox_run(
    *,
    run: PendingSandboxRun,
    manager: SandboxManager,
    pending_store: PendingSandboxRunStore,
) -> None:
    """Close a reusable sandbox once the Execute phase is done with it.

    Kills the box **by id** — never via ``connect``, which auto-resumes a
    paused sandbox and would boot the VM back up purely to shut it down —
    then releases it on the run's row so nothing tries to reattach to a dead
    id. Best-effort: a box that is already gone (reaped, TTL-reclaimed) is
    fine, and a run that has no box is a no-op.
    """
    if not run.sandbox_id:
        return
    try:
        await manager.provider.kill(run.sandbox_id)
    except Exception:
        logger.warning("sandbox_teardown_failed", sandbox_id=run.sandbox_id)
    else:
        logger.info("sandbox_torn_down", turn_id=run.turn_id, sandbox_id=run.sandbox_id)
    await pending_store.release_box(run.turn_id)
    run.sandbox_id, run.paused_at = "", None


__all__ = [
    "SandboxLaunchResult",
    "account_sandbox_tokens",
    "collect_sandbox_run",
    "compose_brief",
    "launch_sandbox_run",
    "publish_sandbox_completion_phase",
    "teardown_sandbox_run",
]
