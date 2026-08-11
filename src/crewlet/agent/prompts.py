"""Per-phase system prompt builders for the turn engine.

Each phase gets only what it needs.  Org-config context (mission /
vision / policies / role profile / unit context / team roster)
renders directly into the Plan-phase prompt from the in-memory
:class:`~crewlet.org.models.Organization` -- no DB seed step, no
per-turn knowledge round-trip.  Static org config is in the prompt;
agent-written diary memory and team knowledge-base docs are surfaced
by the ``## Personal memory`` / ``## Relevant knowledge`` prefetches.

- **Plan** — identity + role profile + unit context + org
  mission/vision + full policies + skills + roster (with team
  member profiles for leads) + tool-skills catalogue (per-tool
  how-to, sourced from the team knowledge base, not hardcoded) +
  tool catalogue + compact plan-phase contract.
- **Execute** — one-line identity + plan summary + 3-line execute
  contract. No policies (Plan already decided the action surface),
  no roster, no catalogue.
- **Review** — one-line identity + plan summary + execute artifact
  + decision enum. Same trim-down as Execute.
- **Sub-agent** — parent-provided task prompt + mandated runtime
  preamble (never spawn further sub-agents, never contact
  colleagues, return a concise final answer).
"""

from __future__ import annotations

from crewlet.agent.definition import (
    AgentDefinition,
    build_human_colleagues_note,
    build_identity_line,
    build_identity_section,
    build_org_mission_vision_section,
    build_policies_section,
    build_role_profile_section,
    build_roster_section,
    build_unit_context_section,
)
from crewlet.agent.skills import Phase, PromptSkillRegistry

SUBAGENT_PREAMBLE = (
    "You are a short-lived sub-agent. Do not spawn further sub-agents. "
    "Do not contact colleagues. "
    "You can discover and activate more tools yourself: call "
    "`list_mcp_server_tools(server=...)` to see what an MCP server "
    "offers, then `activate_tool(name=...)` to promote a tool into your "
    "`tools=[...]` so you can call it on the next round. Only read-only "
    "tools are available this way — you cannot post to channels, comment "
    "on issues, open PRs, or otherwise write to a shared surface. "
    "Return a concise final answer as text when done. If you cannot "
    "complete the task, return what you have with a brief note on why."
)


def _join(sections: list[list[str]]) -> str:
    """Join section-lists into a single string, skipping empty sections."""
    out: list[str] = []
    for section in sections:
        if section:
            out.extend(section)
    return "\n".join(out)


# ---------------------------------------------------------------------------
# Plan phase
# ---------------------------------------------------------------------------


PLAN_HEADER = (
    "\n## PLAN phase"
    "\nYou decide *what* Execute should do. Output one `submit_plan` "
    "call matching the ExecutionPlan schema."
    "\n\n**Your `tools=[...]` starts with the meta-tools "
    "(`submit_plan`, `activate_tool`, `list_mcp_server_tools`, "
    "`load_tool_skill`).** "
    "Your catalogue lists builtin tools by name and MCP servers by "
    "name only. To use a builtin tool here for in-Plan recon, call "
    "`activate_tool(name=...)` — it promotes the tool into your "
    "`tools=[...]`, so its schema appears on the next message and "
    "you can call it directly. To use an MCP tool, first call "
    "`list_mcp_server_tools(server=...)` to see what the server "
    "offers, then `activate_tool(name=...)` to promote the chosen "
    "tool. "
    "Reserve activation for read-only recon (reading threads, issues, "
    "or docs; colleague lookups); action / write tools belong in "
    "`submit_plan`'s `tools_needed` for Execute to run under Review."
    "\n\n**Your Plan-phase tool results are not forwarded to "
    "Execute.** If you fetch data here, Execute will have to "
    "re-fetch it — OR put the finished content into "
    "`steps[].approach` so Execute sees it verbatim and calls the "
    "named tool with it."
    "\n\n**`tools_needed` must list EVERY tool Execute will call — "
    "including the final delivery tool.** If the task will end by "
    "replying on the channel the trigger arrived on, include that "
    "channel's post/reply tool. Creating, updating, or transitioning "
    "something? Include the relevant write tool. A plan "
    "that only lists research tools is broken: Execute will gather "
    "data and have no way to deliver the result. Rule of thumb: if "
    "your `success_criteria` mention 'post', 'reply', 'notify', "
    "'create', 'update', 'send', 'review', 'act', 'take action', "
    "'assign', 'transition', 'respond', 'decline', or "
    "'acknowledge', the corresponding action tool MUST be in "
    "`tools_needed`."
    "\n\n**Plan the full arc, not just recon.** For tasks that need "
    'both fetching AND acting (routed event triggers — "review and '
    'respond", "investigate and decide"), list recon AND the likely '
    "action tools (the reply, status-change, and post tools for the "
    "systems in play) in `tools_needed` so "
    "Execute acts in one pass.  Review always runs after Execute "
    "(engine-enforced for any executable plan) and can "
    "``self_iterate`` with broader tools when you couldn't yet "
    "predict the action — but that's a two-pass turn.  Prefer one "
    "pass: cost of an unused action tool is zero."
    "\n\n**Always include the originating channel's reply tool — "
    "even when the task's primary action is on a different system.** "
    "Whichever transport delivered the trigger is how you stay in "
    "touch with the "
    "requester.  Execute may need to: (a) ask follow-up questions "
    "when info is missing or ambiguous (e.g. a referenced item that "
    "doesn't resolve, an assignee whose handle isn't a valid account "
    "id), (b) confirm completion with a short status, (c) report "
    "partial failures.  Without the reply tool the requester gets "
    "silence and Execute hits a wall.  This is in addition to whatever "
    "action tools the task itself requires."
    '\n\n**`decision="skip"` means "nobody was actually asking me '
    'to do anything" — not "I\'m ignoring a direct ping".** Use '
    "`skip` for informational triggers, passing references, and "
    "messages addressed to someone else.  When you were directly "
    "asked / @mentioned / assigned but are declining (out of scope, "
    "wrong owner, already handled), do NOT use `skip`; emit "
    '`decision="plan"` with one step posting a brief decline via '
    "the originating channel's reply tool — name the right owner "
    "or point at where the work lives.  Silence on a direct ping "
    "looks like the message was lost; a one-liner closes the loop."
    "\n\nMission, vision, policies, your role profile, your unit "
    "context, and your team roster are already in this prompt -- no "
    "lookup needed.  Relevant team documentation was surfaced in the "
    "Plan prompt's `## Relevant knowledge` block."
)

PLAN_HEADER_MODEL_SPLIT = (
    "\nExecute runs on a cheaper model — make the plan rich enough "
    "that it does not need planner-level judgement at each step."
)

# Gated section: rendered only for sandbox-enabled roles.
# Absent for ~all roles so it never bloats the common Plan prompt.
PLAN_SANDBOX_SECTION = (
    "\n## Sandbox code work (the `run_sandbox` tool)"
    "\nFor **real code work** — implement or modify code, run tests, "
    "reproduce a bug, run a script — Execute has a `run_sandbox` tool: a "
    "real computer with a shell and a git checkout where a coding agent "
    "(Claude Code / OpenCode) works autonomously on a brief you give it and "
    "returns a structured report."
    "\n\nPlan the **full arc** in one plan: list `run_sandbox` in "
    "`tools_needed` **and** the tool you'll use to report / act on the "
    "result (e.g. the originating channel's reply tool). The sandbox runs "
    "detached — Execute pauses and resumes automatically with the result, "
    "keeping full context — so after `run_sandbox` returns, Execute reports "
    "the outcome (or opens / links the PR, files a ticket, etc.) with those "
    "tools in the SAME turn. Don't split it into a separate report turn."
    "\n\nKeep everything else native: reply on Slack, update a ticket, read "
    "docs, answer a question — those need no sandbox."
)


# Tool-specific guidance is not hardcoded here.  Skills are sourced
# from a dedicated knowledge-base container (a Confluence space or a
# Plane project) and loaded into the engine's
# in-memory ``PromptSkillRegistry`` at boot and via webhook.  Each
# per-phase builder consults the registry once and appends matching
# skill bodies after its identity / context / contract sections; see
# ``_inject_skills`` and ``docs/concepts/tool-skills.md``.


_SKILL_CATALOGUE_HEADER = (
    "\n## Tool skills"
    "\nGuidance on how to use specific tools / MCP servers.  Each entry "
    "below is one line summarising a skill; call `load_tool_skill(key)` "
    "to fetch the rich body (workflow examples, mention markup, "
    "handoff conventions) when the summary is not enough."
)

_SKILL_CATALOGUE_REQUIRED_NOTE = (
    "\nEntries marked `(required — load before use)` are enforced: the "
    "engine rejects calls to the tools they cover until you have "
    "loaded the skill with `load_tool_skill(key)` in the current "
    "session.  Load them before your first call to those tools."
)

_REQUIRED_MARKER = " (required — load before use)"


def _inject_skill_catalogue(
    parts: list[str],
    *,
    skill_registry: PromptSkillRegistry | None,
    phase: Phase,
    available_tools: set[str] | frozenset[str],
    mcp_servers: set[str] | frozenset[str],
) -> None:
    """Append a one-line-per-skill catalogue for the active surface.

    Bodies are NOT inlined here — the LLM sees ``key — summary`` lines
    and decides whether to load the full body via ``load_tool_skill``,
    which is always available in Plan, Execute, and Sub-agent.

    Required skills (the ``required: true`` default; advisory skills
    opt out with ``required: false``) carry a visible
    ``(required — load before use)`` marker and an enforcement note
    after the header, so the LLM learns the contract up front instead
    of via a blocked tool call (the guard's error message is the
    recovery path, not the discovery path).  Review is the exception:
    it has no domain tools and no ``load_tool_skill``, so nothing is
    enforced there and the marker would point at a tool the reviewer
    doesn't have — required skills render unmarked in Review.

    Deterministic key-sorted order keeps the prompt prefix byte-stable
    across turns for prefix caching.
    """
    if skill_registry is None:
        return
    skills = skill_registry.skills_for(
        phase=phase,
        available_tools=available_tools,
        mcp_servers=mcp_servers,
    )
    if not skills:
        return
    enforceable_phase = phase is not Phase.REVIEW
    parts.append(_SKILL_CATALOGUE_HEADER)
    if enforceable_phase and any(skill.required for skill in skills):
        parts.append(_SKILL_CATALOGUE_REQUIRED_NOTE)
    for skill in skills:
        # Collapse whitespace in the summary so a multi-line YAML
        # literal (``summary: |\n  …\n  …``) still renders as one
        # catalogue bullet — otherwise the embedded newline breaks
        # the one-skill-per-line catalogue format.  Render ${var}
        # references first so operator-defined facts (e.g. tenant URLs)
        # appear substituted in the catalogue.
        summary_line = " ".join(skill_registry.render(skill.summary).split())
        marker = _REQUIRED_MARKER if skill.required and enforceable_phase else ""
        parts.append(f"- `{skill.key}`{marker} — {summary_line}")


def build_plan_prompt(
    definition: AgentDefinition,
    *,
    tool_catalogue: str,
    available_tools: list[str] | None = None,
    model_split_enabled: bool = False,
    counterparty_profile: str = "",
    synthesized_skills_block: str = "",
    episode_recall_block: str = "",
    onboarding_hint_block: str = "",
    personal_memory_block: str = "",
    relevant_knowledge_block: str = "",
    skill_registry: PromptSkillRegistry | None = None,
) -> str:
    """Plan-phase system prompt.

    Includes the tool catalogue (name + one-line description per
    tool) so the planner can name tools in
    ``ExecutionPlan.tools_needed``. All other role/org context
    (backstory, behavioral guidelines, mission, team goals) renders
    into this prompt directly from the in-memory Organization.
    ``model_split_enabled=True`` adds a line
    telling the planner to write a richer plan because the executor
    runs on a cheaper model.

    ``available_tools`` is the set of tool names currently registered
    for this plan (typically ``ToolSurface.catalogue_names()``) and
    drives ``skill_registry`` trigger matching alongside the role's
    MCP-server set.

    ``skill_registry`` is the engine-wide
    :class:`~crewlet.agent.skills.PromptSkillRegistry`. When
    non-``None``, every skill whose trigger fires for the current
    tool / MCP surface and whose phases include ``plan`` is appended
    after the header.  ``None`` (the default) keeps the prompt free of
    skill scaffolding -- used by tests and the combined single-shot
    prompt path.

    ``counterparty_profile`` is a pre-rendered block about the turn's
    triggering counterparty.  When non-empty it is injected
    after the guidance blocks so the planner sees observed traits
    before the tool catalogue.
    """
    sections = [
        build_identity_section(definition),
        build_org_mission_vision_section(definition),
        build_role_profile_section(definition),
        build_unit_context_section(definition),
        build_policies_section(definition),
        build_roster_section(definition),
        build_human_colleagues_note(definition),
    ]
    body = _join(sections)
    header = PLAN_HEADER
    if model_split_enabled:
        header = header + PLAN_HEADER_MODEL_SPLIT
    parts = [body, header]
    # Sandbox Execute option — only for roles gated on, so the prose
    # never bloats the common Plan prompt.
    role_sandbox = definition.role.sandbox
    if role_sandbox is not None and role_sandbox.enabled:
        parts.append(PLAN_SANDBOX_SECTION)
    available = set(available_tools or [])
    if onboarding_hint_block and "mark_onboarded" in available:
        # Appears only when this agent has not yet completed
        # onboarding for its current org chain AND the
        # ``mark_onboarded`` builtin is registered (i.e. reflection is
        # enabled).  Gating on tool availability matches the
        # EPISODIC_GUIDANCE / PERSIST_GUIDANCE pattern above and
        # avoids instructing the agent to call a tool that doesn't
        # exist when reflection is disabled.
        parts.append("\n## First-turn onboarding")
        parts.append(onboarding_hint_block)
    if personal_memory_block:
        # Agent-scope personal memory (LONG + non-expired
        # SHORT entries written by PersistDecider / reflect_and_persist).
        # Filtered for relevance to the current task by the aux-LLM
        # in ``personal_memory.fetch_personal_memory_block``.  Empty
        # string when the filter is unavailable / fails / finds no
        # candidates; a short hint when the filter ran but selected
        # nothing while candidates existed (nudges the planner toward
        # ``refresh_memory`` after gathering context).
        parts.append("\n## Personal memory")
        parts.append(personal_memory_block)
    if synthesized_skills_block:
        parts.append("\n## Synthesized skills you've learned")
        parts.append(synthesized_skills_block)
    if relevant_knowledge_block:
        # Team knowledge-base pages from the agent's accessible
        # containers, found by a query-time search the aux-LLM builds
        # from the trigger.  Frozen at turn start; full page bodies
        # open via the knowledge-base search / get-page tools.
        parts.append("\n## Relevant knowledge")
        parts.append(relevant_knowledge_block)
    if episode_recall_block:
        # Frozen-at-turn-start episodic recall (see
        # docs/concepts/agent-learning.md).  Resolved once here, not
        # re-queried mid-turn, to preserve prompt prefix-caching.
        parts.append("\n## Similar prior work")
        parts.append(episode_recall_block)
    if counterparty_profile:
        parts.append("\n## Known counterparty")
        parts.append(counterparty_profile)
    # Tool skills catalogue lands adjacent to the tool catalogue so the
    # planner reads "here is how to use these tools" right before "here
    # are the tools" — the two are conceptually one section.
    _inject_skill_catalogue(
        parts,
        skill_registry=skill_registry,
        phase=Phase.PLAN,
        available_tools=available,
        mcp_servers=set(definition.role.mcp_env.keys()),
    )
    if tool_catalogue:
        parts.append("\n## Available tools")
        parts.append(tool_catalogue)
    return "\n".join(parts)


# ---------------------------------------------------------------------------
# Execute phase
# ---------------------------------------------------------------------------


EXECUTE_HEADER = (
    "\n## EXECUTE phase"
    "\nRun the plan below. Use tool calls for every action — "
    "writing about an action does not execute it."
    "\n\n**Discovery is available.** Your `tools=[...]` starts with "
    "the plan-named tools, always-on builtins (`load_tool_skill`), "
    "and discovery meta-tools (`activate_tool`, "
    "`list_mcp_server_tools`). If you find mid-run that you need a "
    "tool the planner didn't list, call "
    "`list_mcp_server_tools(server=...)` to discover MCP tool names "
    "and `activate_tool(name=...)` to promote any catalogue tool "
    "(builtin or MCP) into your `tools=[...]`. The activated tool's "
    "schema appears on the next message; call it directly. "
    "Prefer activation over giving up: only stop and report in plain "
    "text if discovery fails or the tool genuinely does not exist."
)


def build_execute_prompt(
    definition: AgentDefinition,
    *,
    plan_summary: str = "",
    counterparty_profile: str = "",
    relevant_knowledge_block: str = "",
    available_tools: list[str] | None = None,
    tool_catalogue: str = "",
    phantom_tools: list[str] | None = None,
    skill_registry: PromptSkillRegistry | None = None,
) -> str:
    """Execute-phase system prompt.

    Plan named the tools Execute starts with; Execute can additionally
    discover and activate more via the ``activate_tool`` /
    ``list_mcp_server_tools`` meta-tools. The prompt carries the
    same slim catalogue Plan sees — builtin tools by name plus MCP
    server names — so the executor knows what discovery surface is
    available.

    ``available_tools`` is the executor's *starting* tool surface
    (typically ``plan.tools_needed ∪ executor_always_on``).  Used to
    scope skill injection: only skills whose trigger matches a tool
    the executor will actually call are injected.  The previous
    monolithic Atlassian-mention hint is now a registry-driven
    skill that triggers on the matching MCP server(s).

    ``tool_catalogue`` is the rendered slim catalogue from
    ``ToolSurface.catalogue_text()`` (builtin names + MCP server
    names). Empty when the surface was built with
    ``expose_catalogue=False`` (e.g. the Execute grace path).

    ``counterparty_profile`` is forwarded from the Plan-phase
    prefetch (frozen at turn start) so the executor has the
    requester's observed traits even when the planner's
    ``plan_summary`` only describes the action abstractly (e.g.
    ``"use the counterparty's preferred greeting format"`` without
    baking the literal greeting into the plan).  Cost is bounded
    by ``CounterpartyProfiler``'s trim policy — typically <200
    tokens.

    ``relevant_knowledge_block`` is the post-Plan relevant-knowledge
    re-fetch (see
    :func:`crewlet.agent.plan.fetch_post_plan_relevant_knowledge`).
    It is non-empty only when the turn's trigger was a bare pointer:
    the Plan-phase ``## Relevant knowledge`` prefetch was gated off
    (embedding-searching a thin pointer is noise), so once Plan has
    done recon the block is re-fetched keyed on the plan summary and
    handed to Execute here.  Empty on every non-thin-trigger turn.

    ``phantom_tools`` are names the planner put in ``tools_needed``
    that do NOT resolve in Execute's catalogue — almost always wrong
    guesses at an MCP tool's name (the planner can't see MCP tool
    names, only server names).  When present, Execute is told
    explicitly so it discovers the real tool via the meta-tools
    instead of assuming the named tool exists and stopping at a text
    reply (the failure mode that lets a "reply" turn never actually
    post).
    """
    parts = [build_identity_line(definition), EXECUTE_HEADER]
    if phantom_tools:
        parts.append(
            "\n**Heads-up — some tools your plan named are NOT in your "
            "catalogue and were almost certainly wrong guesses at an MCP "
            "tool's name: "
            + ", ".join(f"`{t}`" for t in phantom_tools)
            + ".** Do not assume they exist and do not stop at writing a "
            "text reply. Call `list_mcp_server_tools(server=...)` to find "
            "the real tool on the relevant server, then "
            "`activate_tool(name=...)` and call it — composing the reply "
            "as text does not deliver it."
        )
    if plan_summary:
        parts.append("\n## Plan")
        parts.append(plan_summary)
    # Skill catalogue lands AFTER the plan so the executor reads the
    # task framing first, then the tool guidance in the context of what
    # it is actually going to do.  The executor calls
    # ``load_tool_skill`` (always-on in Execute) to fetch any body it
    # needs.
    _inject_skill_catalogue(
        parts,
        skill_registry=skill_registry,
        phase=Phase.EXECUTE,
        available_tools=set(available_tools or []),
        mcp_servers=set(definition.role.mcp_env.keys()),
    )
    if relevant_knowledge_block:
        parts.append("\n## Relevant knowledge")
        parts.append(relevant_knowledge_block)
    if counterparty_profile:
        parts.append("\n## Known counterparty")
        parts.append(counterparty_profile)
    if tool_catalogue:
        parts.append("\n## Available tools")
        parts.append(tool_catalogue)
    return "\n".join(parts)


# ---------------------------------------------------------------------------
# Review phase
# ---------------------------------------------------------------------------


REVIEW_HEADER = (
    "\n## REVIEW phase"
    "\nJudge the artifact Execute produced. Submit exactly one "
    "`submit_review` call with one decision:"
    "\n- **done** — meets the success criteria; return the artifact."
    "\n- **self_iterate** — incomplete or wrong; loop back to Plan with notes."
    "\n\n`## What Plan did` + `## What Execute did` are the evidence — "
    "each line shows the call and its success/error outcome — not the "
    "narration in `## What Execute produced`. Successful Plan calls "
    "count as already-delivered (don't make Execute repeat them); "
    "failed calls (`→ error`) do not."
    "\n**Tool-delivery rule:** if a `tools_needed` action tool was "
    "NOT successfully called in EITHER log, choose `self_iterate`."
    "\n**Sandbox rule:** a successful `run_sandbox` call IS the code work — "
    "a coding agent cloned the repo and ran the shell / tests inside an "
    "isolated sandbox; its report is in `## What Execute produced`. The "
    "absence of `git`/`shell`/`pytest`/`file` calls is NOT fabrication — "
    "that work happens inside the sandbox, never as tool calls here. Judge "
    "the report on its merits."
    "\n**Duplicate-delivery rule:** if both phases successfully called "
    "the same delivery tool, the side effect fired twice — choose "
    "`self_iterate`."
    "\n**Missing-tool rule:** if Execute narrates that it lacks a "
    "tool (e.g. \"I don't have access to the tool needed to deliver "
    'this"), choose `self_iterate` '
    "— Plan can re-list `tools_needed` and Execute gets the tool "
    "next pass."
    "\n**Blocked / needs-a-colleague rule:** if the turn can't finish "
    "without a manager or peer — a capability gap needing someone "
    "else's identity / credentials, or a decision above the agent's "
    "authority — choose `self_iterate` and say so in `notes`. Plan "
    "then adds an outreach step and Execute reaches the colleague "
    "directly with its own colleague-surface tools — a chat mention, "
    "an issue comment, a doc comment, or `a2a_ask` — wherever the "
    "work lives. They reply asynchronously "
    "and that re-triggers the agent. Never leave a direct request "
    "unanswered."
)


def build_review_prompt(
    definition: AgentDefinition,
    *,
    plan_summary: str = "",
    execute_summary: str = "",
    execute_tool_log: str = "",
    plan_tool_log: str = "",
    skill_registry: PromptSkillRegistry | None = None,
) -> str:
    """Review-phase system prompt.

    Reviewer sees only the decision enum (via ``submit_review``),
    plus the plan, Execute's text artifact, and the actual tool-call
    logs from BOTH Plan and Execute so it can judge against evidence
    rather than self-narration.  The Plan-phase log matters because
    the planner can — and frequently does — fire side-effecting calls
    (Slack posts, Jira updates) during recon; without that log Review
    would treat those actions as not-yet-done and demand Execute
    repeat them.

    No domain tools, no catalogue, no policies / roster / backstory —
    all correctness constraints that matter at Review time are
    encoded in the plan's ``success_criteria``.

    Review-phase tool skills are operator-scoped: a skill that lists
    the Review phase in its ``phases`` set surfaces here, keyed on the
    role's MCP servers, even though Review has no domain-tool surface —
    so guidance an operator wants the reviewer to weigh stays
    available.  Review itself posts nothing externally; it only writes
    ``notes`` back to Plan and returns ``final_artifact``.
    """
    parts = [build_identity_line(definition), REVIEW_HEADER]
    if plan_summary:
        parts.append("\n## Plan")
        parts.append(plan_summary)
    _inject_skill_catalogue(
        parts,
        skill_registry=skill_registry,
        phase=Phase.REVIEW,
        available_tools=set(),
        mcp_servers=set(definition.role.mcp_env.keys()),
    )
    # Always include both tool-log sections — REVIEW_HEADER references
    # them as the primary evidence to judge against, so leaving them
    # out would make the header instructions point at nothing.  Plan
    # comes first because chronologically the planner ran first; the
    # reviewer reads top-to-bottom as a timeline.
    parts.append("\n## What Plan did")
    parts.append(plan_tool_log or "(none)")
    parts.append("\n## What Execute did")
    parts.append(execute_tool_log or "(none)")
    if execute_summary:
        parts.append("\n## What Execute produced")
        parts.append(execute_summary)
    return "\n".join(parts)


# ---------------------------------------------------------------------------
# Sub-agent phase
# ---------------------------------------------------------------------------


ONBOARDING_HEADER = (
    "\n## ONBOARDING phase"
    "\nThis is a one-time setup pass that runs before your normal work, with "
    "its **own** budget. Do ONLY this now — not the task you were triggered "
    "on (that runs next, in Plan).\n"
    "Your knowledge-base search / read tools are MCP tools: call "
    "`list_mcp_server_tools(server=...)` to find them (a page-search / "
    "get-page tool on your team's knowledge-base server), then "
    "`activate_tool(name=...)` to promote one into your `tools=[...]`. "
    "`reflect_and_persist` and `mark_onboarded` are already active — call them "
    "directly. Follow the steps below, then call `mark_onboarded` to end the "
    "pass."
)


def build_onboarding_prompt(
    definition: AgentDefinition,
    *,
    onboarding_hint: str,
    tool_catalogue: str = "",
) -> str:
    """First-turn onboarding-phase system prompt.

    A lightweight, dedicated prompt for the pre-Plan onboarding pass: identity
    line + the onboarding instructions (which pages to read, persist
    conventions, then ``mark_onboarded``) + the slim discovery catalogue so the
    agent can locate its knowledge-base tools. No policies / roster / phase
    plumbing — onboarding is a fixed read → persist → mark workflow.
    """
    parts = [build_identity_line(definition), ONBOARDING_HEADER]
    if onboarding_hint:
        parts.append("\n## What to do")
        parts.append(onboarding_hint)
    if tool_catalogue.strip():
        parts.append("\n## Available tools")
        parts.append(tool_catalogue)
    return "\n".join(parts)


def build_subagent_prompt(
    definition: AgentDefinition,
    *,
    parent_system_prompt: str,
    available_tools: list[str] | None = None,
    tool_catalogue: str = "",
    skill_registry: PromptSkillRegistry | None = None,
) -> str:
    """Sub-agent system prompt.

    Structure: parent-provided task-specific prompt, then any
    sub-agent-phase skills matching the parent-passed tool allowlist,
    then (when the sub-agent is discovery-capable) the slim tool
    catalogue, then the mandated runtime preamble.  No identity section,
    no policies -- sub-agents are short-lived workers, not teammates --
    but they benefit from the same MCP-server / tool scaffolding regular
    agents get when calling and discovering those tools.
    """
    parts = [parent_system_prompt]
    _inject_skill_catalogue(
        parts,
        skill_registry=skill_registry,
        phase=Phase.SUBAGENT,
        available_tools=set(available_tools or []),
        mcp_servers=set(definition.role.mcp_env.keys()),
    )
    if tool_catalogue.strip():
        parts.append("")
        parts.append("## Available tools")
        parts.append(tool_catalogue)
    parts.append("")
    parts.append(SUBAGENT_PREAMBLE)
    return "\n".join(parts)


__all__ = [
    "EXECUTE_HEADER",
    "ONBOARDING_HEADER",
    "PLAN_HEADER",
    "PLAN_HEADER_MODEL_SPLIT",
    "REVIEW_HEADER",
    "SUBAGENT_PREAMBLE",
    "build_execute_prompt",
    "build_onboarding_prompt",
    "build_plan_prompt",
    "build_review_prompt",
    "build_subagent_prompt",
]
