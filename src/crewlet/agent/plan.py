"""Plan phase of the turn engine.

The Plan phase is an LLM call that decides what Execute should do.
Its output is an :class:`ExecutionPlan` artifact:

- free-text reasoning,
- an ordered list of :class:`Step`s,
- the ``tools_needed`` contract the executor's ToolSurface will be
  built from,
- success criteria for Review to judge against.

The planner's LLM surface starts narrow. It sees:

- a ``submit_plan`` structured-output tool that forces the plan to
  arrive as JSON matching ``ExecutionPlan``,
- a ``list_mcp_server_tools`` meta-tool that returns the tool listing
  for one MCP server on demand,
- an ``activate_tool`` meta-tool that promotes a builtin or MCP tool
  into the active ``tools=[...]`` for in-Plan recon (Slack thread
  read, Jira fetch, agent lookup, Confluence search, etc.) — the
  tool's schema reaches the LLM via the next round's
  ``tools=[...]``,
- a ``load_tool_skill`` meta-tool for fetching skill bodies on
  demand,
- a *slim* tool catalogue in its system prompt: builtin tool names
  + MCP server names only.

MCP tool names are not in the prompt — the planner discovers them
via ``list_mcp_server_tools(server)`` and either activates one for
in-Plan recon or names it in ``submit_plan.tools_needed`` for
Execute to run.
"""

from __future__ import annotations

import asyncio
import json
from typing import Any, Literal

from pydantic import BaseModel, Field

from crewlet._logging import get_logger
from crewlet.agent.extension import DeferredJudgeEvent, emit_deferred, maybe_extend
from crewlet.agent.iteration_log import render_iteration_ledger
from crewlet.agent.llm_loop import (
    publish_phase_completed,
    publish_phase_started,
    run_tool_loop,
)
from crewlet.agent.prompts import build_phase_user_message, build_plan_prompt
from crewlet.agent.skills.guard import skill_guard_for_turn
from crewlet.agent.skills.models import Phase as SkillPhase
from crewlet.agent.tool_discovery import (
    build_activate_tool,
    build_list_mcp_server_tools,
)
from crewlet.agent.turn_context import PlanPrefetch, TurnContext
from crewlet.events.types import (
    PlanPrefetchSummary,
    RelevantKnowledgeRefetched,
    describe_trigger,
)
from crewlet.learning.interaction import (
    interactions_require_recon,
    merge_interactions_by_sender,
    salient_task_text,
)
from crewlet.learning.onboarding import (
    build_onboarding_hint,
    compute_chain_hash,
    is_onboarded,
)
from crewlet.learning.personal_memory import fetch_personal_memory_block
from crewlet.learning.relevant_knowledge import fetch_relevant_knowledge_block
from crewlet.learning.summarize import summarize_episodes
from crewlet.providers.llm.protocol import LLMProvider, Message
from crewlet.queue.protocol import EventQueue
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry
from crewlet.tools.surface import ToolSurface, merge_registry_and_mcp

_EPISODE_RECALL_LIMIT = 3

# Names of the Plan-phase meta-tools — pure scaffolding for the Plan
# contract, carrying no business semantics.  ``submit_plan`` is the
# artifact handoff; ``activate_tool``, ``list_mcp_server_tools`` and
# ``load_tool_skill`` are in-Plan recon scaffolding.  Defined here as
# the source of truth so Review's prompt formatter, the engine's
# delivery-override check, and the prior-work ledger (consumers in
# ``review.py`` / ``turn.py`` / ``iteration_log.py``) stay in sync — any
# new meta-tool added to ``_build_meta_tools`` below must also land in
# this set, otherwise it leaks into Review's ``## What Plan did`` log,
# into the engine's ``called_tool_names`` union, and into the ledger as
# already-delivered work the next round is told not to repeat.
# ``list_mcp_server_tools`` was missing here until the ledger landed;
# ``turn.py`` had been unioning it into ``non_delivery_tools`` by hand,
# which covered the delivery gate but not the two prompt consumers.
PLAN_META_TOOL_NAMES: frozenset[str] = frozenset(
    {"submit_plan", "activate_tool", "list_mcp_server_tools", "load_tool_skill"}
)

# Rendered in place of the ``## Similar prior work`` block when the
# thin-trigger gate fires.  Mirrors the ``EMPTY_FILTER_HINT`` constants
# in :mod:`crewlet.learning.personal_memory` and
# :mod:`crewlet.learning.relevant_knowledge` so all three gated
# prefetches stay visible and self-explanatory instead of one of them
# vanishing silently.  Phrased conditionally ("if this task
# resembles ...") so it reads correctly even for an agent with no
# prior episodes -- unlike personal memory (a cheap diary list) and
# relevant knowledge (a cheap accessible-spaces check), the "does the
# agent have any candidates" probe for episodes IS the vector query
# the gate exists to skip, so we cannot cheaply suppress the hint for
# a fresh agent.
_EMPTY_RECALL_HINT = (
    "(episode recall was skipped -- the bare trigger is too thin to "
    "match past work against; if this task resembles something you "
    "have done before, call `query_episodes` with a focused query "
    "once you have done recon)"
)


def _single_line(text: str, *, limit: int) -> str:
    """Collapse whitespace into single spaces and truncate.

    Plan-prompt bullets store one entry per line; embedding raw
    LLM-generated strings (episode summaries, skill descriptions) can
    introduce newlines that turn one bullet into a multi-line block,
    breaking the prefix structure and the format cue the planner reads.
    """
    return " ".join((text or "").split())[:limit]


logger = get_logger("agent.plan")


class Step(BaseModel):
    """One ordered step in an :class:`ExecutionPlan`."""

    intent: str
    """What this step accomplishes."""

    approach: str = ""
    """How -- prose, not code. Helps the executor pick the right tool."""

    tools: list[str] = Field(default_factory=list)
    """Tools this step may call (subset of ``ExecutionPlan.tools_needed``)."""

    on_failure: Literal["retry", "skip"] = "retry"
    """What the executor should do if this step fails."""


class ExecutionPlan(BaseModel):
    """The artifact produced by the Plan phase.

    When the planner and executor share a model, the planner may
    also return a ``direct`` decision (no plan, straight to Execute);
    callers detect that as ``decision == "direct"``.
    """

    decision: Literal["plan", "direct", "skip"] = "plan"
    """Top-level decision made by the planner:

    - ``plan``:    run this plan through Execute, then Review.
    - ``direct``:  skip the separate Plan artifact; go straight to
                   Execute with the full registry as its surface.
    - ``skip``:    nobody was actually asking the agent to do
                   anything (informational trigger, passing
                   reference, addressee was someone else,
                   broadcast). The turn ends immediately with
                   ``reasoning`` as the final text -- nothing is
                   posted back to the requester.  Do NOT use
                   ``skip`` when the agent was directly asked /
                   @mentioned / assigned but is declining; that
                   case must be expressed as ``decision="plan"``
                   with a single step that posts a brief
                   explanation on the originating channel, so the
                   requester knows the message was received and
                   isn't left waiting in silence.

    To hand a task off to a colleague, use ``plan`` with a step that
    reaches them on the surface where the work lives (a Slack mention,
    Jira comment / reassignment, or ``a2a_ask``) -- the same tools any
    teammate would use. There is no dedicated delegate decision: the
    handoff is just Execute calling the colleague-surface tool.
    """

    reasoning: str = ""
    """Why this plan."""

    steps: list[Step] = Field(default_factory=list)
    """Ordered steps. Empty when ``decision`` is ``skip`` or
    ``direct``."""

    tools_needed: list[str] = Field(default_factory=list)
    """Tool names the executor will receive schemas for. The Execute
    ToolSurface is built from this list union with the always-on set.
    """

    success_criteria: list[str] = Field(default_factory=list)
    """What 'done' looks like for Review to judge against."""

    @property
    def is_direct(self) -> bool:
        return self.decision == "direct"

    def summary(self) -> str:
        """Prose summary of the plan for Execute / Review / Episode.

        Includes each step's ``intent`` AND ``approach`` so the
        planner can pre-compose content (e.g. the exact text to
        post to Slack) in ``approach`` and have Execute see it
        without re-fetching data the planner already gathered.

        For ``skip`` turns -- which short-circuit before Execute /
        Review -- this output is consumed only by the episode
        publisher.  Including ``reasoning`` here is what makes
        skipped episodes carry diagnostic value (audit, recall,
        compaction); without it the episode row is a tombstone with
        no usable signal beyond the trigger text.  Mirrors the
        ``direct`` branch below which already keeps ``reasoning``.
        """
        if self.decision == "skip":
            if self.reasoning:
                return f"(skip) {self.reasoning[:400]}"
            return "(skip: not addressed to me)"
        if not self.steps:
            # ``direct`` with no steps: fall back to ``reasoning`` so
            # Execute at least sees the planner's thinking.
            if self.decision == "direct":
                return (
                    self.reasoning[:400]
                    if self.reasoning
                    else "(direct: no explicit plan; executor improvises)"
                )
            return self.reasoning[:400]
        lines: list[str] = []
        for i, step in enumerate(self.steps):
            lines.append(f"{i + 1}. {step.intent}")
            if step.approach:
                lines.append(f"   {step.approach}")
        return "\n".join(lines)


# ---------------------------------------------------------------------------
# Plan-phase meta-tools
# ---------------------------------------------------------------------------


_SUBMIT_PLAN_DESCRIPTION = (
    "Submit the final ExecutionPlan. Call exactly once per "
    "turn. Decisions:\n"
    "- `plan`: Execute runs the steps, then Review. Name the "
    "  action tools Execute will call in `tools_needed`. "
    "  If you already know the exact content Execute should "
    "  produce (e.g. the Slack reply text), put it in the "
    "  step's `approach` — Execute sees it verbatim and "
    "  doesn't need to re-derive it.\n"
    "- `direct`: one-tool task with no multi-step plan. "
    "  Name the tool in `tools_needed` and put any "
    "  pre-composed content in a single step's `approach`.\n"
    "- `skip`: nobody was actually asking the agent to "
    "  act (informational trigger, passing reference, "
    "  addressee was someone else). The turn ends "
    "  silently with `reasoning` as the final output. "
    "  Do NOT use `skip` when you were directly asked / "
    "  @mentioned / assigned but are declining — that "
    "  case uses `plan` with one reply step (see the "
    "  PLAN-phase contract).\n\n"
    "To hand a task off to a colleague, use `plan` with a "
    "step that reaches them where the work lives — a chat "
    "mention, an issue comment / reassignment, or `a2a_ask` "
    "— and name that tool in `tools_needed`. There is no "
    "separate delegate decision.\n\n"
    "`tools_needed` is REQUIRED for `plan` and `direct`, "
    "and MUST list EVERY tool Execute will call — research "
    "AND the final delivery tool. If the task ends by "
    "replying on the channel it arrived on, include that "
    "channel's post/reply tool; if it creates, updates, or "
    "transitions something, include that write tool. A plan "
    "that only lists research tools will fail to deliver a "
    "response — Execute will compose text with no way to "
    "send it. If you don't know which tool yet, pick "
    "`plan` and figure it out there.\n\n"
    "Plan-phase tool results (from `activate_tool` + "
    "read-only calls) are NOT forwarded to Execute. When "
    "you fetch data to compose the answer, hand the answer "
    "off to Execute via `step.approach` — don't assume "
    "Execute can see what you saw.\n\n"
    "Review runs after Execute on every `plan` / "
    "`direct` plan (engine-enforced; not configurable "
    "from the plan).  Review catches missing-tool "
    "failures, stalls, and incomplete artifacts, and is "
    "the only path that can ``self_iterate`` when Execute "
    "falls short.  A `skip` decision never runs Execute "
    "and therefore never runs Review."
)


def _build_meta_tools(
    catalogue_tools: list[Any],
    role_mcp_tools: list[Any],
    surface_holder: list[Any],
    *,
    event_queue: Any = None,
    agent_id: str = "",
    agent_role: str = "",
    turn_id: str = "",
    iteration: int = 0,
    availability_filter: set[str] | None = None,
) -> list[SimpleTool]:
    """Return the Plan phase's meta-tools: activate_tool,
    list_mcp_server_tools, submit_plan, and load_tool_skill.

    ``surface_holder`` is a one-element list holding the Plan
    ``ToolSurface``.  It's populated by the caller right after the
    surface is constructed (``ToolSurface.for_plan`` requires
    ``meta_tools`` upfront -- chicken and egg).  ``activate_tool``
    reads it to call ``surface.activate(name)``, promoting the
    catalogue tool into ``tools=[...]`` so the next round can invoke
    it directly for in-Plan recon.

    ``catalogue_tools`` is the role's MERGED registry+MCP universe
    (used by ``activate_tool`` to detect "registered but
    availability-gated" so the LLM gets a precise diagnostic, and as
    the source of the shared ``load_tool_skill`` builtin so its
    description stays identical to the version Execute sees via
    ``always_on``).  ``role_mcp_tools`` is the role's MCP-only
    subset; bucketed by server inside ``list_mcp_server_tools``.

    ``availability_filter`` is forwarded to ``list_mcp_server_tools``
    so discovery and activation see a consistent set — without it the
    LLM could discover a gated tool then fail to activate it.

    ``event_queue`` / ``agent_id`` / ``agent_role`` / ``turn_id`` /
    ``iteration`` are forwarded to a ``phase.tool_activated`` event
    on every successful activation.
    """
    activate = build_activate_tool(
        catalogue_tools,
        surface_holder,
        phase="plan",
        event_queue=event_queue,
        agent_id=agent_id,
        agent_role=agent_role,
        turn_id=turn_id,
        iteration=iteration,
    )
    list_servers = build_list_mcp_server_tools(
        # The MERGED universe, not the per-role list: the catalogue
        # advertises every shared server too, and this is what the
        # agent is told to call for its tool names.
        catalogue_tools,
        availability_filter=availability_filter,
    )

    async def _submit_plan(params: dict[str, Any], context: AgentContext) -> ToolResult:
        # Validate against the Pydantic schema; the planner's output
        # is captured by the caller via messages (the tool returns a
        # confirmation so the LLM stops calling more tools).
        try:
            ExecutionPlan.model_validate(params)
        except Exception as exc:  # pragma: no cover - pydantic error path
            return ToolResult(success=False, error=f"Invalid plan: {exc}")
        return ToolResult(success=True, output="plan submitted")

    # Source ``load_tool_skill`` directly from the catalogue (registered
    # by ``register_builtin_tools``) so Plan's ``tools=[...]`` shows the
    # same description / parameters as the version Execute / Sub-agent
    # surface via the registry. Maintaining a second Plan-local
    # definition would diverge the descriptions on a single tool name
    # and break LLM provider prefix-cache reuse across phases.
    load_tool_skill_tool = next(
        (t for t in catalogue_tools if t.name == "load_tool_skill"), None
    )

    meta: list[SimpleTool] = [
        activate,
        list_servers,
        SimpleTool(
            name="submit_plan",
            description=_SUBMIT_PLAN_DESCRIPTION,
            parameters=ExecutionPlan.model_json_schema(),
            fn=_submit_plan,
        ),
    ]
    if load_tool_skill_tool is not None:
        meta.append(load_tool_skill_tool)
    return meta


# ---------------------------------------------------------------------------
# Parsing helpers
# ---------------------------------------------------------------------------


def parse_plan_from_messages(messages: list[Message]) -> ExecutionPlan:
    """Extract an :class:`ExecutionPlan` from the LLM trace.

    Prefers the last ``submit_plan`` tool call; falls back to parsing
    the final assistant text as JSON.  Returns a ``decision="skip"``
    placeholder plan if neither is present.

    Why ``skip`` and not ``direct``: when the planner LLM emits prose
    instead of calling ``submit_plan``, it has not articulated *what*
    Execute should do.  Falling through to ``direct`` runs Execute
    with the full registry but no plan, and the executor LLM (often a
    cheaper model) is then free to invent actions -- the production
    bug that motivated this default was a planner that emitted "I'll
    skip this message — it's addressed to another bot user" but
    forgot to call ``submit_plan(decision="skip")``; the fallback
    coerced to ``direct`` and Execute then posted a reply.  The agent
    plainly didn't intend to act.

    ``skip`` is the safe default: TurnEngine short-circuits before
    Execute, the turn ends with the placeholder reasoning as the
    final artifact, and no external side effects fire.  When the
    planner *did* mean to act, the bug is in the planner not following
    the contract -- not the right time to silently execute on its
    behalf.

    ``decision="skip"`` is exactly the shape ``TurnEngine`` needs to
    short-circuit before Execute (and therefore before Review), so
    the placeholder doesn't need to express "skip Review" any other
    way -- Review only runs after Execute and ``skip`` never reaches
    it.
    """
    for msg in reversed(messages):
        if msg.role == "assistant" and msg.tool_calls:
            for call in msg.tool_calls:
                if call.name == "submit_plan":
                    try:
                        return ExecutionPlan.model_validate(call.arguments)
                    except Exception:
                        logger.warning("plan_parse_failed", args=call.arguments)
    for msg in reversed(messages):
        if msg.role == "assistant" and msg.content:
            try:
                data = json.loads(msg.content)
                return ExecutionPlan.model_validate(data)
            except Exception:
                break
    logger.warning("plan_no_submission_fallback_to_skip")
    return ExecutionPlan(
        decision="skip",
        reasoning="(no plan submitted)",
    )


def _submit_plan_was_called(messages: list[Message]) -> bool:
    """True if any assistant message in ``messages`` contains a
    ``submit_plan`` tool call.  Used to decide whether the
    Plan-phase rescue (one constrained final call) needs to fire."""
    for msg in messages:
        if msg.role != "assistant":
            continue
        for call in msg.tool_calls or []:
            if call.name == "submit_plan":
                return True
    return False


_PLAN_RESCUE_DIRECTIVE = (
    "You've reached the planning round limit and haven't submitted a "
    "plan.  Submit one now based on what you've found so far.  Two "
    "acceptable shapes:\n\n"
    "1. **Surface a blocker via the originating channel.**  If your "
    "research left you without enough info to plan the work (missing "
    "fields, ambiguous identifiers, unresolved references), submit a "
    "plan whose single step is to reply on the originating channel "
    "(Slack / Jira / Confluence / GitHub depending on where the "
    "trigger came from) explaining what's blocking you and what you "
    "need from the requester.  Include the corresponding reply tool "
    "in `tools_needed`.\n\n"
    "2. **Hand off to Execute.**  If you have enough to proceed, "
    "submit a normal plan with the steps Execute should run.  "
    "Pre-compose any text content into `step.approach` so Execute "
    "doesn't have to re-derive it.\n\n"
    "Either way, your next call must be `submit_plan`.  No further "
    "research -- your tool surface for this rescue is constrained to "
    "`submit_plan` only."
)


async def _rescue_submit_plan(
    *,
    provider: LLMProvider,
    provider_key: str,
    base_messages: list[Message],
    surface: ToolSurface,
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    agent_context: AgentContext,
    turn: TurnContext,
    event_queue: EventQueue,
    budget_manager: Any,
    observability: Any,
    token_usage_repo: Any,
    validators: list[Any] | None,
) -> Any:
    """One-shot constrained rescue when Plan exhausted rounds without
    ``submit_plan``.  Reuses the same conversation history; surface
    narrowed to ``[submit_plan]`` only; appends a directive nudging
    the LLM to pick one of two clear paths.  Best-effort: failures
    return ``None`` and the caller falls through to the existing
    no-submission skip fallback in ``parse_plan_from_messages``."""
    submit_plan_tool: Any = None
    # ``ToolSurface._tools`` holds the meta-tools that were passed to
    # ``for_plan``; we narrow the rescue surface to the submit_plan
    # one only so the LLM has no choice but to call it.
    for tool in getattr(surface, "_tools", []):
        if getattr(tool, "name", "") == "submit_plan":
            submit_plan_tool = tool
            break
    if submit_plan_tool is None:
        logger.warning("plan_rescue_no_submit_plan_tool")
        return None
    # Thread the turn's availability_set through, same as the main
    # Plan surface: for_plan also populates the *catalogue*, and
    # without the filter the rescue catalogue would re-expose tools a
    # check_fn gated off -- the planner could then name one in
    # tools_needed and Execute would silently drop it.
    rescue_surface = ToolSurface.for_plan(
        registry,
        role_mcp_tools,
        meta_tools=[submit_plan_tool],
        availability_filter=turn.availability_set,
    )
    rescue_messages = list(base_messages) + [
        Message(role="user", content=_PLAN_RESCUE_DIRECTIVE)
    ]
    logger.info(
        "plan_rescue_invoked",
        agent=turn.agent.handle,
        turn_id=turn.turn_id,
        base_round_count=sum(
            1 for m in base_messages if m.role == "assistant" and m.tool_calls
        ),
    )
    try:
        rescue_loop = await run_tool_loop(
            provider=provider,
            messages=rescue_messages,
            surface=rescue_surface,
            context=agent_context,
            agent=turn.agent,
            # Two rounds: one for the LLM's expected submit_plan call,
            # one slack in case it emits prose first then the call.
            max_rounds=2,
            event_queue=event_queue,
            budget_manager=budget_manager,
            observability=observability,
            token_usage_repo=token_usage_repo,
            validators=validators,
            provider_key=provider_key,
            a2a_context=turn.a2a_context,
            terminate_after=["submit_plan"],
            # Force the LLM to call a tool this round. The
            # rescue surface only carries ``submit_plan`` so "required"
            # collapses to "you must call submit_plan". Without this
            # the LLM can ignore the directive and return prose,
            # leaving us back at the no-submission fallback.
            tool_choice="required",
            turn_id=turn.turn_id,
            iteration=turn.iteration,
            trigger=describe_trigger(turn.trigger_event),
        )
    except Exception:
        logger.exception("plan_rescue_failed")
        return None
    if not _submit_plan_was_called(rescue_loop.messages):
        # The rescue itself didn't submit -- caller's
        # parse_plan_from_messages will fall through to the existing
        # skip default.  Logged so operators can see the planner
        # ignored the rescue directive.
        logger.warning(
            "plan_rescue_did_not_submit",
            agent=turn.agent.handle,
            turn_id=turn.turn_id,
        )
    return rescue_loop


# ---------------------------------------------------------------------------
# Runner
# ---------------------------------------------------------------------------


async def run_plan_phase(
    *,
    turn: TurnContext,
    provider: LLMProvider,
    provider_key: str,
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    event_queue: EventQueue,
    agent_context: AgentContext,
    max_rounds: int = 16,
    model_split_enabled: bool = False,
    budget_manager: Any = None,
    observability: Any = None,
    token_usage_repo: Any = None,
    validators: list[Any] | None = None,
    llm_providers: dict[str, LLMProvider] | None = None,
    episode_recall_summarize: bool = True,
    episode_recall_summarize_max_tokens: int = 400,
    prompt_skill_registry: Any = None,
    judge_provider: LLMProvider | None = None,
    judge_provider_key: str = "",
    extension_enabled: bool = False,
    extension_ceiling: int = 0,
    extension_round_step: int = 8,
) -> ExecutionPlan:
    """Run the Plan phase and return the resulting :class:`ExecutionPlan`.

    Builds a Plan-phase :class:`~crewlet.tools.surface.ToolSurface`
    (catalogue in the prompt, meta-tools in ``tools=[...]``) and
    drives it through :func:`~crewlet.agent.llm_loop.run_tool_loop`.
    The returned plan is parsed from the last ``submit_plan`` tool
    call.

    ``llm_providers`` is the engine's full provider pool; it is passed
    unconditionally so the personal-memory and relevant-knowledge
    prefetches can always run their aux-LLM relevance filter.
    ``episode_recall_summarize`` toggles *only* whether episode-recall
    hits are summarised by the aux model -- it must not gate the other
    prefetches' filters (the bug this split fixes: forwarding
    ``llm_providers=None`` to disable summarisation silently disabled
    the memory and knowledge filters too).
    """
    trigger = describe_trigger(turn.trigger_event)
    await publish_phase_started(
        event_queue=event_queue,
        agent=turn.agent,
        turn_id=turn.turn_id,
        iteration=turn.iteration,
        phase="plan",
        trigger=trigger,
    )

    # Build the role's full activation universe via the same merge
    # helper the surface uses (MCP overrides global for name collisions).
    # Passing the merged list to ``_build_meta_tools`` keeps the
    # closure's diagnostic ``catalogue_map`` and the surface's
    # ``_catalogue_tools`` in sync — they must agree on which Tool
    # instance backs each name, or the LLM's error / success paths
    # reference different versions of the same name.
    catalogue_tools = merge_registry_and_mcp(registry, role_mcp_tools)
    # surface_holder is populated right after ToolSurface.for_plan so
    # the activate_tool closure can call surface.activate(name).
    surface_holder: list[Any] = [None]
    meta_tools = _build_meta_tools(
        catalogue_tools,
        role_mcp_tools,
        surface_holder=surface_holder,
        event_queue=event_queue,
        agent_id=turn.agent.id_str,
        agent_role=turn.agent.role_name,
        turn_id=turn.turn_id,
        iteration=turn.iteration,
        availability_filter=turn.availability_set,
    )
    surface = ToolSurface.for_plan(
        registry,
        role_mcp_tools,
        meta_tools=list(meta_tools),
        availability_filter=turn.availability_set,
    )
    surface_holder[0] = surface
    # Required-skill guard: required skills gate the tools their
    # trigger covers until ``load_tool_skill`` runs in this session.
    # The extension loop reuses this surface, so loads carry across
    # extensions; a self_iterate Plan builds a fresh surface + guard
    # because its LLM context starts over.
    surface.skill_guard = skill_guard_for_turn(
        registry=prompt_skill_registry,
        phase=SkillPhase.PLAN,
        surface=surface,
        turn=turn,
        event_queue=event_queue,
    )

    # Frozen-at-turn-start prefetches.  On the first Plan iteration the
    # three blocks are resolved in parallel and cached on the
    # TurnContext; subsequent self_iterate loops reuse the cached
    # strings so the system-prompt prefix stays byte-identical and
    # provider prefix caching holds.
    if turn.plan_prefetch is None:
        (
            counterparty,
            synthesized,
            recall,
            onboarding,
            personal_memory,
            relevant_knowledge,
        ) = await asyncio.gather(
            _fetch_counterparty_profile_text(turn=turn, agent_context=agent_context),
            _fetch_synthesized_skills_block(turn=turn, agent_context=agent_context),
            _fetch_episode_recall_block(
                turn=turn,
                agent_context=agent_context,
                llm_providers=llm_providers,
                summarize=episode_recall_summarize,
                summarize_max_tokens=episode_recall_summarize_max_tokens,
            ),
            _fetch_onboarding_hint_block(turn=turn, agent_context=agent_context),
            _fetch_personal_memory_block_wrapper(
                turn=turn,
                agent_context=agent_context,
                llm_providers=llm_providers,
            ),
            _fetch_relevant_knowledge_block(
                turn=turn,
                agent_context=agent_context,
                llm_providers=llm_providers,
            ),
        )
        turn.plan_prefetch = PlanPrefetch(
            counterparty_profile=counterparty,
            synthesized_skills=synthesized,
            episode_recall=recall,
            onboarding_hint=onboarding,
            personal_memory=personal_memory,
            relevant_knowledge=relevant_knowledge,
        )
        # Prefetch telemetry: emit once per turn (only on the first Plan
        # iteration -- the prefetch is frozen across self-iterations
        # so re-emitting on every iteration would over-count hits).
        await _emit_plan_prefetch_summary(
            turn=turn,
            event_queue=event_queue,
            counterparty=counterparty,
            synthesized=synthesized,
            recall=recall,
            onboarding=onboarding,
            personal_memory=personal_memory,
            relevant_knowledge=relevant_knowledge,
        )
    prefetch = turn.plan_prefetch

    system_prompt = build_plan_prompt(
        turn.agent.definition,
        tool_catalogue=surface.catalogue_text(),
        available_tools=list(surface.catalogue_names()),
        model_split_enabled=model_split_enabled,
        counterparty_profile=prefetch.counterparty_profile,
        synthesized_skills_block=prefetch.synthesized_skills,
        episode_recall_block=prefetch.episode_recall,
        onboarding_hint_block=prefetch.onboarding_hint,
        personal_memory_block=prefetch.personal_memory,
        relevant_knowledge_block=prefetch.relevant_knowledge,
        skill_registry=prompt_skill_registry,
    )
    messages = [
        Message(role="system", content=system_prompt),
        Message(
            role="user",
            # The prior-work ledger and the conversation history both
            # ride the USER message so the frozen system prefix above
            # stays byte-stable across self_iterate loops (provider
            # prefix caching); the ledger is empty on iteration 1, and
            # the history is frozen at turn start for the same reason.
            content=build_phase_user_message(
                task_description=turn.task_description,
                prior_work=render_iteration_ledger(
                    turn.iteration_history, skip_names=PLAN_META_TOOL_NAMES
                ),
                conversation_history=turn.conversation_history,
            ),
        ),
    ]

    loop = await run_tool_loop(
        provider=provider,
        messages=messages,
        surface=surface,
        context=agent_context,
        agent=turn.agent,
        max_rounds=max_rounds,
        event_queue=event_queue,
        budget_manager=budget_manager,
        observability=observability,
        token_usage_repo=token_usage_repo,
        validators=validators,
        provider_key=provider_key,
        a2a_context=turn.a2a_context,
        terminate_after=["submit_plan"],
        turn_id=turn.turn_id,
        iteration=turn.iteration,
        trigger=trigger,
    )

    turn.input_tokens += loop.input_tokens
    turn.output_tokens += loop.output_tokens
    turn.model_keys["plan"] = loop.model or provider_key

    # Extension judge: when the loop exhausted rounds without calling
    # ``submit_plan``, ask a cheap judge LLM whether the planner is
    # making progress.  If yes, grant more rounds (bounded by the
    # configured ceiling); if no, fall through to the existing rescue
    # path.  Extensions chain -- after each extended run we re-judge
    # if rounds were again exhausted, up to the ceiling.
    total_rounds = loop.rounds_used
    deferred_judge_events: list[DeferredJudgeEvent] = []
    if (
        extension_enabled
        and judge_provider is not None
        and loop.exhausted_rounds
        and not _submit_plan_was_called(loop.messages)
    ):
        while loop.exhausted_rounds and total_rounds < extension_ceiling:
            ext_loop, _decision = await maybe_extend(
                main_loop=loop,
                phase="plan",
                task_description=turn.task_description,
                plan_summary="",
                judge_provider=judge_provider,
                judge_provider_key=judge_provider_key,
                main_provider=provider,
                main_provider_key=provider_key,
                main_surface=surface,
                main_terminate_after=["submit_plan"],
                main_tool_choice=None,
                enabled=True,
                round_step=extension_round_step,
                rounds_remaining_under_ceiling=(extension_ceiling - total_rounds),
                context=agent_context,
                turn=turn,
                event_queue=event_queue,
                registry=registry,
                role_mcp_tools=role_mcp_tools,
                budget_manager=budget_manager,
                observability=observability,
                token_usage_repo=token_usage_repo,
                validators=validators,
                deferred_events=deferred_judge_events,
            )
            if ext_loop is None:
                break
            turn.input_tokens += ext_loop.input_tokens
            turn.output_tokens += ext_loop.output_tokens
            total_rounds += ext_loop.rounds_used
            # Fold extension counters into the main loop so the
            # per-phase event sees the full picture -- the extension
            # is conceptually part of the same Plan run.  Mirrors the
            # same merge in run_execute_phase.  ``loop.messages`` is
            # already the merged history (run_tool_loop mutates in
            # place across both calls).  ``plan_tool_sequence`` is
            # appended once below by the post-loop iterator (which
            # walks the merged ``loop.tool_executions``) -- don't
            # also append here or extension calls land in the
            # sequence twice.
            loop.tool_executions.extend(ext_loop.tool_executions)
            loop.input_tokens += ext_loop.input_tokens
            loop.output_tokens += ext_loop.output_tokens
            loop.rounds_used += ext_loop.rounds_used
            loop.exhausted_rounds = ext_loop.exhausted_rounds
            if ext_loop.model:
                loop.model = ext_loop.model
            if _submit_plan_was_called(loop.messages):
                break

    # Rescue: if the loop terminated without submit_plan (typically
    # because it hit ``max_rounds`` mid-research), run one more
    # constrained call with only submit_plan in the surface and a
    # directive offering two paths -- surface a blocker via the
    # originating channel, or hand off to Execute with what we have.
    # Without this rescue the no-submission fallback silently skips
    # the turn and the requester gets nothing -- exactly the case
    # that prompted this fix (planner researched a Jira Epic, found
    # the project, ran out of rounds before submitting).
    plan_messages = loop.messages
    rescue_fired = False
    rescue_loop = None
    if not _submit_plan_was_called(loop.messages):
        rescue_fired = True
        rescue_loop = await _rescue_submit_plan(
            provider=provider,
            provider_key=provider_key,
            base_messages=loop.messages,
            surface=surface,
            registry=registry,
            role_mcp_tools=role_mcp_tools,
            agent_context=agent_context,
            turn=turn,
            event_queue=event_queue,
            budget_manager=budget_manager,
            observability=observability,
            token_usage_repo=token_usage_repo,
            validators=validators,
        )
        if rescue_loop is not None:
            plan_messages = rescue_loop.messages
            turn.input_tokens += rescue_loop.input_tokens
            turn.output_tokens += rescue_loop.output_tokens

    # Record the Plan-phase tool calls on the turn context.  Plan-only
    # builtins (``reflect_and_persist`` etc.) never reach
    # ``TurnCompleted.tool_sequence`` (that is Execute-scoped), so the
    # ReflectEngine needs this trail to know the LLM already
    # self-persisted in-flight.  Accumulated so a ``self_iterate`` loop
    # back into Plan does not drop the earlier iteration's calls.
    # ``plan_tool_executions`` carries the full call records (name,
    # arguments, result, success) so Review's prompt can show what
    # the planner already did during recon — see ``run_review_phase``.
    for _loop in (loop, rescue_loop):
        if _loop is None:
            continue
        for exe in _loop.tool_executions or []:
            name = exe.get("name", "")
            if name:
                turn.plan_tool_sequence.append(name)
                # Shallow-copy so downstream readers (event publisher,
                # observability) and Review's prompt formatter never
                # observe each other's mutations.  The dict otherwise
                # comes straight from ``llm_loop`` and is shared by
                # reference with ``AgentPhaseCompleted.tool_executions``.
                turn.plan_tool_executions.append(dict(exe))

    plan = parse_plan_from_messages(plan_messages)
    # No-submission fallback that arrived on a real conversation: never go
    # silent. ``decision="skip"`` + ``"(no plan submitted)"`` means the planner
    # engaged but we couldn't extract a plan even after the rescue — an
    # engine/model failure, NOT the planner deciding the trigger wasn't for us.
    # Silently skipping would drop a request the requester is waiting on
    # (violating the triage contract for a direct ping). Convert it to a
    # one-step acknowledgment; Execute discovers the originating channel's reply
    # tool via its always-on discovery meta-tools.
    if (
        plan.decision == "skip"
        and plan.reasoning == "(no plan submitted)"
        and turn.notification_metadata
    ):
        logger.warning("plan_no_submission_acknowledge_fallback", turn_id=turn.turn_id)
        plan = ExecutionPlan(
            decision="plan",
            reasoning="(no plan submitted — acknowledging the requester)",
            steps=[
                Step(
                    intent="Acknowledge the request on the channel it arrived on",
                    approach=(
                        "Planning didn't converge this turn. Post a brief reply "
                        "on the conversation this request came from saying you "
                        "received it but need them to restate or clarify what "
                        "they want, then stop. If the reply tool isn't already "
                        "available, discover it with list_mcp_server_tools + "
                        "activate_tool."
                    ),
                )
            ],
            success_criteria=[
                "Posted a brief acknowledgment on the originating channel"
            ],
        )
    logger.info(
        "plan_phase_complete",
        decision=plan.decision,
        steps=len(plan.steps),
        tools_needed=plan.tools_needed,
    )
    # Plan phase: meta-tool schemas (``submit_plan``, ``activate_tool``,
    # ``load_tool_skill``) plus any catalogue tool the planner activated
    # this turn go into the LLM call's tools=[...]; the domain-tool
    # catalogue itself is plain text in the prompt.  Report each list
    # separately so dashboards can show which tools had schemas versus
    # which were listed by name only.
    await publish_phase_completed(
        event_queue=event_queue,
        agent=turn.agent,
        turn_id=turn.turn_id,
        conversation_key=turn.stored_conversation_key,
        iteration=turn.iteration,
        phase="plan",
        provider_key=provider_key,
        loop=loop,
        decision=plan.decision,
        notes=plan.reasoning[:500],
        # Sorted for byte-stable cross-phase comparison; Execute publishes
        # the same shape (sorted) so dashboards joining the two events
        # don't see spurious ordering diffs.
        tools_available=sorted(surface.names),
        tool_catalogue=sorted(surface.catalogue_names()),
        rescue_fired=rescue_fired,
        rescue_loop=rescue_loop,
        trigger=trigger,
    )
    # Flush any judge events that fired during this phase.  Deferred
    # so chronological dashboards see the host phase event first,
    # then its judge children.
    await emit_deferred(deferred_judge_events)
    return plan


def _count_relevant_knowledge_picks(block: str) -> int:
    """Count the doc bullets the relevant-knowledge aux filter rendered.

    Each pick renders as a ``- **<title>**`` Markdown bullet;
    ``EMPTY_FILTER_HINT`` is a single ``(no team documents ...)`` line
    with no ``- **`` marker.  So a non-zero count cleanly means "the
    filter selected at least one doc" and zero means "the filter
    rendered the hint only" (or the block was empty / gated).

    Shared by :func:`_emit_plan_prefetch_summary` (Plan-time prefetch)
    and :func:`_emit_relevant_knowledge_refetched` (post-Plan
    re-fetch) so both telemetry surfaces count picks identically.
    """
    text = block or ""
    return text.count("\n- **") + (1 if text.startswith("- **") else 0)


async def _emit_plan_prefetch_summary(
    *,
    turn: TurnContext,
    event_queue: EventQueue,
    counterparty: str,
    synthesized: str,
    recall: str,
    onboarding: str,
    personal_memory: str,
    relevant_knowledge: str,
) -> None:
    """Publish one ``PlanPrefetchSummary`` after the prefetches resolve.

    Prefetch-telemetry helper.  Records hit / miss + rendered-byte size for
    each block so operators can plot per-block hit rate per agent and
    catch silent regressions (e.g. a block stuck at 0% hit rate is
    almost always a config / data problem rather than a turn problem).

    Best-effort: a publish failure is logged once and swallowed so
    telemetry never breaks the turn.
    """
    if event_queue is None:
        return
    relevant_picks = _count_relevant_knowledge_picks(relevant_knowledge)
    # Whether the trigger was a bare pointer (a recon-required webhook).
    # When True, the personal-memory / relevant-knowledge / episode-
    # recall prefetches skipped their aux-LLM call by the thin-trigger
    # gate -- this field is what tells an operator "the block is empty
    # because it was *gated*", not because the filter ran and found
    # nothing.  Pure-logic check (notification metadata), no LLM call.
    trigger_requires_recon = interactions_require_recon(turn.trigger_interactions())
    try:
        await event_queue.publish(
            "crewlet.events.plan_prefetch_summary",
            PlanPrefetchSummary(
                source=turn.agent.handle,
                agent_id=str(turn.agent.id),
                agent_handle=turn.agent.handle,
                role=turn.agent.definition.role.name,
                turn_id=str(turn.turn_id),
                counterparty_hit=bool(counterparty),
                counterparty_bytes=len(counterparty or ""),
                synthesized_skills_hit=bool(synthesized),
                synthesized_skills_bytes=len(synthesized or ""),
                episode_recall_hit=bool(recall),
                episode_recall_bytes=len(recall or ""),
                onboarding_hint_hit=bool(onboarding),
                onboarding_hint_bytes=len(onboarding or ""),
                personal_memory_hit=bool(personal_memory),
                personal_memory_bytes=len(personal_memory or ""),
                relevant_knowledge_hit=bool(relevant_knowledge),
                relevant_knowledge_bytes=len(relevant_knowledge or ""),
                relevant_knowledge_selection_count=relevant_picks,
                trigger_requires_recon=trigger_requires_recon,
            ),
        )
    except Exception:
        logger.exception(
            "plan_prefetch_summary_publish_failed", turn_id=str(turn.turn_id)
        )


async def _fetch_counterparty_profile_text(
    *, turn: TurnContext, agent_context: AgentContext
) -> str:
    """Pre-render the known-counterparty block for the Plan prompt.

    Returns an empty string when (a) no ``counterparty_store`` is
    wired on the context (counterparty profiling disabled), (b) the trigger event
    lacks an identifiable sender (internal ``TaskAssigned`` and
    friends), or (c) no profile exists yet for any of its senders.
    A coalesced trigger renders one block per distinct sender with a
    stored profile.  Any fetch failure is logged and treated as "no
    profile".
    """
    store = getattr(agent_context, "counterparty_store", None)
    if store is None or turn.trigger_event is None:
        return ""
    try:
        senders = [
            i.sender for i in merge_interactions_by_sender(turn.trigger_interactions())
        ]
        profiles = await store.fetch_for_senders(turn.agent.handle, senders)
        if not profiles:
            return ""
        # One block per distinct sender — a coalesced multi-human trigger
        # surfaces every known participant, not just the latest pinger.
        return "\n\n".join(_render_counterparty_profile(p) for p in profiles)
    except Exception:
        # Rendering sits inside the guard too: a malformed profile (or a
        # store fake that violates the list contract) degrades to "no
        # block" instead of breaking the Plan prefetch.
        logger.exception("counterparty_prefetch_failed")
        return ""


def _render_counterparty_profile(profile: Any) -> str:
    """Render a ``CounterpartyProfile`` into the Plan prompt section.

    Prepends a ``Subject:`` / ``Platform:`` header (the planner needs
    to know who the observations are about, since this block is
    surfaced without conversational context) then delegates to
    :meth:`CounterpartyProfile.render_observed_traits` so the trait
    body matches what ``lookup_colleague`` emits.
    """
    lines: list[str] = [f"Subject: {profile.subject_label}"]
    if profile.subject_platform:
        lines.append(f"Platform: {profile.subject_platform}")
    lines.append(profile.render_observed_traits())
    return "\n".join(lines)


async def _fetch_episode_recall_block(
    *,
    turn: TurnContext,
    agent_context: AgentContext,
    llm_providers: dict[str, LLMProvider] | None = None,
    summarize: bool = True,
    summarize_max_tokens: int = 400,
) -> str:
    """Pre-render a short "Similar prior work" block from past turns.

    Satisfies the doc's *frozen-at-turn-start* promise (see
    ``docs/concepts/agent-learning.md`` EpisodicMemory section):
    resolve similar episodes once at Plan phase start and bake them
    into the system prompt so later ``query_episodes`` tool calls
    don't invalidate prefix caching.

    When ``summarize`` is set and ``llm_providers`` is supplied, the
    raw bullets are passed through
    :func:`~crewlet.learning.summarize.summarize_episodes` on the
    role's auxiliary model so the planner sees a compact rendering
    instead of raw data — matching the spec's "cheap-model summarises
    episode hits" rule.  Operators disable summarisation by setting
    ``learning.reflect.summarize_episodes=false``, which the
    TurnEngine forwards as ``summarize=False`` -- the provider pool is
    still passed (the sibling personal-memory / relevant-knowledge
    prefetches need it for their aux filters), only the summarisation
    call is skipped.  ``summarize_max_tokens`` mirrors the same config
    knob the runtime ``query_episodes`` builtin honours.  Any
    summarisation failure falls back to raw.

    Returns an empty string when the store is not wired, the agent
    has no handle, the task description is empty, or the fetch
    returns nothing.  Any failure silently omits the block.

    Returns :data:`_EMPTY_RECALL_HINT` when the trigger is a bare
    pointer (a webhook naming a thing-that-changed) -- same
    thin-trigger gate as the personal-memory and relevant-knowledge
    prefetches.  Vector-searching the agent's own past against a
    pointer is low-value, so the gate skips the query; the hint keeps
    the ``## Similar prior work`` block visible and points the planner
    at ``query_episodes`` once it has done recon and has a real query
    (the consolidated retrieval-research guidance block carries the
    same nudge -- see ``RETRIEVAL_RESEARCH_GUIDANCE`` in
    ``agent/prompts.py``).
    """
    store = getattr(agent_context, "episode_store", None)
    if store is None:
        return ""
    handle = turn.agent.handle
    interactions = turn.trigger_interactions()
    # Vector-search past turns against the salient inbound message, not
    # the enriched task description -- the notification builder's
    # triage scaffolding is identical on every turn and only adds noise
    # to the similarity query.
    task_description = salient_task_text(interactions, turn.task_description).strip()
    if not handle or not task_description:
        return ""
    if interactions_require_recon(interactions):
        logger.debug(
            "episode_recall_skipped_thin_trigger",
            agent_handle=handle,
            turn_id=turn.turn_id,
        )
        return _EMPTY_RECALL_HINT
    try:
        episodes = await store.query(
            agent_handle=handle,
            query_text=task_description,
            limit=_EPISODE_RECALL_LIMIT,
            outcome_filter="done",
        )
    except Exception:
        logger.exception("episode_recall_prefetch_failed")
        return ""
    if not episodes:
        return ""
    lines: list[str] = []
    for ep in episodes:
        tools = list(ep.tool_sequence or [])
        tool_line = ", ".join(tools[:8]) if tools else "(no tools)"
        # Compacted rows carry their signal in ``common_task_pattern``
        # (their per-turn ``task_summary`` / ``plan_summary`` are empty
        # by construction).  Branch on ``kind`` so a compacted hit
        # renders its aggregate pattern + count instead of an empty
        # "(no summary)" bullet.
        if ep.kind == "compacted":
            pattern = ep.common_task_pattern or ep.notable_patterns or "(pattern)"
            summary = f"{_single_line(pattern, limit=160)} [observed {ep.count}x]"
        else:
            raw_summary = ep.task_summary or ep.plan_summary or "(no summary)"
            summary = _single_line(raw_summary, limit=160)
        lines.append(f"- [{ep.review_outcome}] {summary} -- tools: {tool_line}")
    raw = "\n".join(lines)
    footer = (
        "Use `query_episodes` to dig deeper if the above hints "
        "aren't enough; otherwise reuse the approach."
    )
    if summarize and llm_providers:
        try:
            raw = await summarize_episodes(
                raw_payload=raw,
                role=turn.agent.definition.role,
                llm_providers=llm_providers,
                max_tokens=summarize_max_tokens,
                event_queue=getattr(agent_context, "event_queue", None),
                agent_id=agent_context.agent_id,
                turn_id=turn.turn_id,
            )
        except Exception:
            # summarize_episodes handles its own LLM failures, but any
            # unexpected raise here (e.g. OOM during role lookup) must
            # not cancel the sibling prefetches under ``asyncio.gather``.
            logger.exception("episode_recall_summarize_failed")
    return f"{raw}\n{footer}"


async def _fetch_onboarding_hint_block(
    *, turn: TurnContext, agent_context: AgentContext
) -> str:
    """Render the first-turn ``## First-turn onboarding`` block when
    the agent is *definitively* not marked onboarded for its current
    org chain.

    The engine does not distill Confluence pages itself -- it just
    nudges the agent to read its team's
    ``Onboarding`` Confluence pages, capture conventions via
    ``reflect_and_persist``, and call ``mark_onboarded``.  The marker
    suppresses this block on subsequent turns; chain-hash mismatch
    revives it.

    Returns an empty string when the dedicated pre-Plan pass already
    ran this turn (``turn.onboarding_ran``), the org isn't on the
    context, the process-local latch already confirmed the marker
    (``AgentInstance.onboarded_chain_hash`` — no DB read at all), the
    marker read returns ``True``, **or the read fails** (tri-state
    ``None`` — rendering the hint to a possibly already-marked agent
    would re-run onboarding inside Plan, so unknown state suppresses
    and the check retries next turn).
    """
    # The dedicated pre-Plan onboarding pass already handled onboarding this
    # turn (agent.onboarding_phase). Don't also render the hint into Plan — that
    # would let onboarding happen a second time inside Plan and re-spend the
    # Plan round budget, which the dedicated pass exists to prevent.
    if getattr(turn, "onboarding_ran", False):
        return ""
    organization = getattr(agent_context, "org", None)
    if organization is None:
        return ""
    role_name = getattr(agent_context, "role", "") or ""
    if not role_name:
        return ""
    try:
        role_obj = organization.get_role(role_name)
    except Exception:
        logger.exception(
            "onboarding_role_lookup_failed",
            agent_id=agent_context.agent_id,
            role=role_name,
        )
        return ""
    if role_obj is None:
        return ""
    marker_store = getattr(agent_context, "onboarding_marker_store", None)
    try:
        chain_hash = compute_chain_hash(role_obj, organization)
    except Exception:
        logger.exception(
            "onboarding_hint_chain_hash_failed",
            agent_id=agent_context.agent_id,
            role=role_name,
        )
        return ""
    # Process-local latch: this process already confirmed the agent is
    # onboarded for this chain — no hint, and no DB read that could flake.
    if turn.agent.onboarded_chain_hash == chain_hash:
        return ""
    try:
        already = await is_onboarded(
            marker_store=marker_store,
            agent_id=agent_context.agent_id,
            expected_chain_hash=chain_hash,
        )
    except Exception:
        logger.exception(
            "onboarding_hint_check_failed",
            agent_id=agent_context.agent_id,
            role=role_name,
        )
        return ""
    if already is True:
        turn.agent.onboarded_chain_hash = chain_hash
        return ""
    if already is None:
        # Unknown (lookup failed): render NO hint. Showing it to a possibly
        # already-onboarded agent would trigger a repeat onboarding inside
        # Plan — the exact bug the tri-state read exists to prevent.
        return ""
    try:
        return build_onboarding_hint(role_obj, organization)
    except Exception:
        logger.exception(
            "onboarding_hint_render_failed",
            agent_id=agent_context.agent_id,
            role=role_name,
        )
        return ""


async def _fetch_personal_memory_block_wrapper(
    *,
    turn: TurnContext,
    agent_context: AgentContext,
    llm_providers: dict[str, LLMProvider] | None,
) -> str:
    """Plan-phase wrapper around
    :func:`personal_memory.fetch_personal_memory_block`.

    Resolves the agent's :class:`Role` from the org so the aux-LLM
    filter can pick the role's auxiliary provider; passes the turn's
    task description and (when present) the canonical inbound
    interaction.  The interaction lets the filter compare the
    triggering sender against per-subject memories instead of only
    seeing raw user IDs in the task body.
    """
    organization = getattr(agent_context, "org", None)
    role_obj = None
    role_name = getattr(agent_context, "role", "") or ""
    if organization is not None and role_name:
        try:
            role_obj = organization.get_role(role_name)
        except Exception:
            role_obj = None
    interactions = turn.trigger_interactions()
    return await fetch_personal_memory_block(
        diary=getattr(agent_context, "agent_diary", None),
        agent_id=agent_context.agent_id,
        role=role_obj,
        task_description=salient_task_text(interactions, turn.task_description),
        interactions=interactions,
        llm_providers=llm_providers,
        event_queue=getattr(agent_context, "event_queue", None),
        budget_manager=getattr(agent_context, "budget_manager", None),
        turn_id=turn.turn_id,
        trigger_requires_recon=interactions_require_recon(interactions),
    )


async def _fetch_relevant_knowledge_block(
    *,
    turn: TurnContext,
    agent_context: AgentContext,
    llm_providers: dict[str, LLMProvider] | None,
    query_override: str = "",
) -> str:
    """Plan-phase wrapper around
    :func:`relevant_knowledge.fetch_relevant_knowledge_block`.

    Resolves the agent's :class:`Role` and reaches the
    ``KnowledgeSearcher`` via ``agent_context.knowledge_searcher``.
    Returns an empty string when either is unavailable; the renderer
    module handles the aux-LLM, search-scope, and empty-result
    cases.

    ``query_override`` re-points the fetch at a caller-supplied query
    string instead of ``turn.task_description`` and forces the
    thin-trigger gate OFF.  Used by
    :func:`fetch_post_plan_relevant_knowledge`: when the trigger was a
    bare pointer the Plan-time prefetch is gated, but once Plan has
    done its recon the plan summary IS a real query -- so the
    post-Plan path re-runs the fetch keyed on it, ungated.
    """
    organization = getattr(agent_context, "org", None)
    searcher = getattr(agent_context, "knowledge_searcher", None)
    if organization is None or searcher is None:
        return ""
    role_name = getattr(agent_context, "role", "") or ""
    role_obj = None
    if role_name:
        try:
            role_obj = organization.get_role(role_name)
        except Exception:
            role_obj = None
    if role_obj is None:
        return ""
    interactions = turn.trigger_interactions()
    # ``query_override`` (the post-Plan re-fetch) carries a real,
    # recon-informed query, so the thin-trigger gate must NOT fire --
    # gating it would defeat the whole point of the re-fetch.
    if query_override:
        task_description = query_override
        trigger_requires_recon = False
    else:
        # Filter against the salient inbound message, not the enriched
        # task description -- the notification builder's triage
        # scaffolding would otherwise crowd the real query out of the
        # filter prompt and the vector search.
        task_description = salient_task_text(interactions, turn.task_description)
        trigger_requires_recon = interactions_require_recon(interactions)
    return await fetch_relevant_knowledge_block(
        searcher=searcher,
        role=role_obj,
        org=organization,
        task_description=task_description,
        llm_providers=llm_providers,
        event_queue=getattr(agent_context, "event_queue", None),
        budget_manager=getattr(agent_context, "budget_manager", None),
        agent_id=agent_context.agent_id,
        turn_id=turn.turn_id,
        trigger_requires_recon=trigger_requires_recon,
    )


async def fetch_post_plan_relevant_knowledge(
    *,
    turn: TurnContext,
    agent_context: AgentContext,
    plan: ExecutionPlan,
    llm_providers: dict[str, LLMProvider] | None,
) -> str:
    """Re-fetch the relevant-knowledge block after Plan submits, keyed
    on the plan summary, for injection into the Execute prompt.

    The Plan-time ``## Relevant knowledge`` prefetch is gated off when
    the trigger is a bare pointer (a recon-required webhook):
    embedding-searching ``"PR #123"`` is noise.  But once Plan has
    done its recon the *plan summary* is a real, task-shaped query --
    so this runs the fetch the gate skipped, keyed on
    ``plan.summary()`` alone (the original task is boilerplate on a
    thin trigger and would only dilute the query), and hands the
    result to Execute.

    Returns an empty string unless ALL of:

    - the plan decision is ``plan`` or ``direct`` -- ``skip`` never
      reaches Execute, so there is no prompt to enrich;
    - the trigger actually required recon -- otherwise the Plan-time
      prefetch already ran against a real trigger and Execute has
      nothing missing to recover;
    - the plan summary is non-empty.

    When it does run, it emits a :class:`RelevantKnowledgeRefetched`
    telemetry event so the gated Plan prefetch and the recovered
    Execute block can be correlated in the dashboard.

    Boundary: this enriches only the Execute phase that *follows*
    Plan.  It does not retroactively cover tool calls the planner made
    *inside* the Plan phase -- those already ran without the block.

    Best-effort: any unexpected failure degrades to ``""`` (logged)
    rather than breaking the turn -- this is a prefetch, and the
    Plan-phase prefetch helpers all hold the same contract.
    """
    try:
        if plan.decision not in ("plan", "direct"):
            return ""
        if not interactions_require_recon(turn.trigger_interactions()):
            return ""
        plan_summary = (plan.summary() or "").strip()
        if not plan_summary:
            return ""
        block = await _fetch_relevant_knowledge_block(
            turn=turn,
            agent_context=agent_context,
            llm_providers=llm_providers,
            query_override=plan_summary,
        )
        await _emit_relevant_knowledge_refetched(
            turn=turn,
            event_queue=getattr(agent_context, "event_queue", None),
            plan_decision=plan.decision,
            block=block,
        )
        return block
    except Exception:
        # A prefetch must never break the turn -- degrade to "no
        # Relevant knowledge block in Execute".  ``_fetch_relevant_
        # knowledge_block`` and ``_emit_relevant_knowledge_refetched``
        # are each internally defensive; this guards the thin layer
        # around them (e.g. ``plan.summary()``) for parity with the
        # Plan-phase prefetch helpers.
        logger.exception(
            "post_plan_relevant_knowledge_failed", turn_id=str(turn.turn_id)
        )
        return ""


async def _emit_relevant_knowledge_refetched(
    *,
    turn: TurnContext,
    event_queue: EventQueue | None,
    plan_decision: str,
    block: str,
) -> None:
    """Publish one ``RelevantKnowledgeRefetched`` after the post-Plan
    re-fetch runs.

    Best-effort: a publish failure is logged once and swallowed so
    telemetry never breaks the turn.  Mirrors
    :func:`_emit_plan_prefetch_summary`.
    """
    if event_queue is None:
        return
    try:
        await event_queue.publish(
            "crewlet.events.relevant_knowledge_refetched",
            RelevantKnowledgeRefetched(
                source=turn.agent.handle,
                agent_id=str(turn.agent.id),
                agent_handle=turn.agent.handle,
                role=turn.agent.definition.role.name,
                turn_id=str(turn.turn_id),
                iteration=turn.iteration,
                plan_decision=plan_decision,
                block_bytes=len(block or ""),
                selection_count=_count_relevant_knowledge_picks(block or ""),
            ),
        )
    except Exception:
        logger.exception(
            "relevant_knowledge_refetched_publish_failed",
            turn_id=str(turn.turn_id),
        )


async def _fetch_synthesized_skills_block(
    *, turn: TurnContext, agent_context: AgentContext
) -> str:
    """Pre-render the "Synthesized skills you've learned" block.

    Lists the agent's own synthesized skills (agent-scope only --
    shared procedural drafts live in the team knowledge base under
    each unit's ``Auto-Drafted Skills`` parent and surface to agents
    via the ``## Relevant knowledge`` prefetch once published).
    Returns an empty string when no store is wired or the agent has
    nothing to show.  Any failure silently omits the block.
    """
    store = getattr(agent_context, "synthesized_skill_store", None)
    if store is None:
        return ""
    handle = turn.agent.handle
    if not handle:
        return ""
    try:
        # Hide curator-archived skills from the catalogue, but
        # keep stale skills visible (rendered with a marker below) so
        # the agent can still load one if it remembers the name.
        skills = await store.list_for_agent(
            handle, include_archived=False, include_stale=True
        )
    except Exception:
        logger.exception("synthesized_skills_prefetch_failed")
        return ""
    if not skills:
        return ""
    lines: list[str] = []
    for skill in skills:
        desc = _single_line(skill.description or "", limit=200) or "(no description)"
        prefix = "[stale] " if skill.state == "stale" else ""
        lines.append(f"- **{prefix}{skill.name}**: {desc}")
    lines.append(
        "Use the `use_skill` tool to load any of these on demand. "
        "Use `refine_skill` to update one with new observations."
    )
    return "\n".join(lines)


__all__ = [
    "ExecutionPlan",
    "Step",
    "fetch_post_plan_relevant_knowledge",
    "parse_plan_from_messages",
    "run_plan_phase",
]
