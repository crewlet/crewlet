"""Tests for per-phase prompt builders."""

from __future__ import annotations

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.prompts import (
    ONBOARDING_HEADER,
    PLAN_HEADER,
    REVIEW_HEADER,
    SUBAGENT_PREAMBLE,
    build_execute_prompt,
    build_onboarding_prompt,
    build_phase_user_message,
    build_plan_prompt,
    build_review_prompt,
    build_subagent_prompt,
)
from crewlet.agent.skills import (
    Phase,
    PromptSkill,
    PromptSkillRegistry,
    TriggerExpr,
)
from crewlet.org.models import Organization, OrgUnit, Role


def _registry(skills: list[PromptSkill]) -> PromptSkillRegistry:
    """Build a registry pre-loaded with ``skills`` (sync helper for tests)."""
    reg = PromptSkillRegistry()
    reg.seed(skills)
    return reg


def _org(with_policies: bool = True) -> Organization:
    lead = Role(
        name="Engineering Lead",
        handle="lead",
        goal="Lead the engineering team.",
        responsibilities=["Guide the team."],
        manages=["Engineer"],
    )
    engineer = Role(
        name="Engineer",
        handle="eng",
        goal="Ship quality code.",
        responsibilities=["Write tests."],
        behavioral_guidelines=["Be concise."],
        mcp_env={
            "atlassian": {"token": "x"},
            "github": {"Authorization": "Bearer x"},
        },
    )
    unit = OrgUnit(
        name="Eng Team",
        type="team",
        purpose="Build the thing.",
        lead="Engineering Lead",
        roles=[lead, engineer],
        goals=["Ship v1.0."],
        slack_channel="C_ENG",
    )
    policies = ["Respect teammates.", "No secrets in code."] if with_policies else []
    return Organization(
        name="Acme",
        mission="Build great things.",
        vision="Be the best.",
        policies=policies,
        units=[unit],
    )


def _def(role_name: str = "Engineer") -> AgentDefinition:
    org = _org()
    role = org.get_role(role_name)
    assert role is not None
    return AgentDefinition(role=role, org=org)


# -- Plan prompt ----------------------------------------------------------


def test_plan_prompt_includes_catalogue_and_identity():
    p = build_plan_prompt(_def(), tool_catalogue="- foo: Does foo.\n- bar: Does bar.")
    assert "PLAN phase" in p
    assert "## Available tools" in p
    assert "- foo: Does foo." in p
    assert "Engineer" in p
    assert "Acme" in p


def test_plan_prompt_requires_originating_channel_reply_tool():
    """Production bug: a Jira-create task triggered from Slack ended
    up with `tools_needed` missing `slack_conversations_add_message`.
    Execute hit unresolved info (Epic name not found, assignee handle
    not a Jira ID) and had no way to ask the requester.  Plan-phase
    prompt must call out that the originating-channel reply tool is
    always needed, regardless of where the primary action lands."""
    p = build_plan_prompt(_def(), tool_catalogue="")
    # The dedicated source-channel rule, not just the generic
    # "include the final delivery tool" rule.
    assert "originating channel's reply tool" in p
    # Why it matters -- the prompt has to give the planner a reason
    # so it generalises to novel cases.  The (a/b/c) framing maps to
    # concrete failure modes the planner can recognize.
    assert "ask follow-up questions" in p
    assert "confirm completion" in p
    # And it must be clear this is ADDITIVE to the action tool, not
    # a replacement -- otherwise the planner reads "originating
    # channel" as redirecting the action tool itself.
    assert "in addition to" in p
    # The delivery-tool guidance must describe the tool by capability
    # ("post/reply tool" for the channel the trigger arrived on), NOT
    # by a hardcoded integration tool name -- the engine stays
    # decoupled from any specific tool stack and lets the LLM pick the
    # right tool from its (MCP-sourced) catalogue.
    assert "post/reply tool" in p
    assert "slack_conversations_add_message" not in p
    assert "jira_add_comment" not in p


def test_plan_prompt_requires_verbose_decline_for_direct_ask():
    """``decision="skip"`` is for "nobody was asking me",
    not "I was asked but I'm ignoring it".  When the agent was
    directly asked / @mentioned / assigned but is declining, the
    plan must instead post a brief explanation via the originating
    channel.  Without this rule the planner silently skips direct
    pings and the requester gets nothing back."""
    p = build_plan_prompt(_def(), tool_catalogue="")
    # The anchor sentence -- one short, greppable summary of the
    # rule so the planner can refer to it.
    assert 'means "nobody was actually asking me to do anything"' in p
    # The negative: spell out the WRONG use of ``skip`` so the
    # planner doesn't fall through to it on a direct ask.
    assert "do NOT use `skip`" in p
    assert "@mentioned" in p
    # The positive: what to do instead -- a ``plan`` with a one-step
    # explanation on the originating channel.  This connects to the
    # earlier "originating channel's reply tool" rule.
    assert 'decision="plan"' in p
    assert "originating channel's reply tool" in p
    # The justification -- so the planner generalises rather than
    # reading the rule as a magic phrase to pattern-match.
    assert "closes the loop" in p


def test_submit_plan_tool_description_states_review_is_mandatory():
    """Review is mandatory on every ``plan`` / ``direct`` decision;
    there is no ``needs_review`` opt-out.  The tool description has
    to explain that Review runs unconditionally on executable plans
    -- otherwise a planner may try to set ``needs_review=False``
    (Pydantic silently ignores it) and assume Review was
    skipped."""
    from crewlet.agent.plan import _build_meta_tools

    meta_tools = _build_meta_tools(
        catalogue_tools=[], role_mcp_tools=[], surface_holder=[None]
    )
    submit_plan = next(t for t in meta_tools if t.name == "submit_plan")
    desc = submit_plan.description
    # Must explain that Review is engine-enforced for plan / direct.
    assert "engine-enforced" in desc
    # And that the field is not in the planner's hands -- "not
    # configurable from the plan" tells the planner not to look for
    # a knob that doesn't exist.
    assert "not configurable from the plan" in desc


def test_plan_prompt_calls_out_plan_the_full_arc_rule():
    """LEAD-1 production failure: routed Jira ``comment_created``
    triggers had both the PM and the CTO plan recon-only
    (`tools_needed: ["jira_get_issue"]`) with `needs_review: false`.
    Execute fetched, the LLM wrote "I don't have Jira write tools
    available" in plain text, and the turn ended with nothing
    delivered.  The Plan-prompt rule (paired with the engine-side
    ``_force_review_for_executable_plan`` guard) tells the planner
    to list action tools upfront so Execute can act in one pass,
    rather than relying on Review→self_iterate as a slow round-trip."""
    p = build_plan_prompt(_def(), tool_catalogue="")
    # The section title -- greppable in agent traces, gives the
    # planner a single phrase to anchor against.
    assert "Plan the full arc" in p
    # The action-tool guidance is described by capability (reply /
    # status-change / post tools "for the systems in play"), not by
    # hardcoded integration tool names -- the planner maps the concept
    # onto the actual tools in its catalogue.
    assert "reply, status-change, and post tools" in p
    assert "`jira_add_comment`" not in p
    assert "`jira_transition_issue`" not in p
    # The engine guarantee -- so the planner understands Review is
    # not optional and there's no token saving in trying to opt out.
    assert "engine-enforced" in p
    # The escape valve -- Review can self_iterate when the planner
    # genuinely couldn't predict the action.
    assert "self_iterate" in p
    # The extended action-verb rule of thumb has to mention the
    # routed-event verbs the LEAD-1 trigger actually used (`review`,
    # `respond`) -- the original list only had the write-action
    # verbs (`post`, `reply`, `notify`, `create`, `update`, `send`),
    # which didn't fire because the routed-event task body used
    # different vocabulary.
    assert "'review'" in p
    assert "'respond'" in p


def test_plan_prompt_includes_full_policies() -> None:
    """Full policy text is inlined in the Plan prompt.  The bullet
    list is verbatim -- no digest, no knowledge pointer."""
    p = build_plan_prompt(_def(), tool_catalogue="")
    assert "## Company policies" in p
    assert "Respect teammates." in p


def test_plan_prompt_renders_long_policies_in_full() -> None:
    """Policies are inlined in full -- no truncation, no ``...``-suffix."""
    org = _org(with_policies=False)
    long_policy = (
        "Use Confluence to document architecture decisions, runbooks, "
        "and meeting notes — search Confluence before creating new docs."
    )
    org.policies = [long_policy]
    role = org.get_role("Engineer")
    assert role is not None
    d = AgentDefinition(role=role, org=org)
    p = build_plan_prompt(d, tool_catalogue="")
    assert long_policy in p


def test_plan_prompt_omits_your_skills_section():
    """No ``## Your Skills`` block
    appears in the Plan prompt.  Synthesized skills the agent has
    learned are surfaced in a separate Plan-phase prefetch block,
    not the static identity scaffold."""
    p = build_plan_prompt(_def("Engineer"), tool_catalogue="")
    assert "## Your Skills" not in p


def test_plan_prompt_roster_for_leads():
    p = build_plan_prompt(_def("Engineering Lead"), tool_catalogue="")
    assert "## Your Team" in p
    assert "Engineer" in p


def test_plan_prompt_no_roster_for_non_leads():
    p = build_plan_prompt(_def("Engineer"), tool_catalogue="")
    assert "## Your Team" not in p


def test_plan_prompt_model_split_adds_rich_plan_hint():
    plain = build_plan_prompt(_def(), tool_catalogue="")
    split = build_plan_prompt(_def(), tool_catalogue="", model_split_enabled=True)
    assert "cheaper model" not in plain
    assert "cheaper model" in split


def _sandbox_def() -> AgentDefinition:
    """An ``Engineer`` definition whose role is sandbox-enabled."""
    from crewlet.org.models import RoleSandboxConfig

    org = _org()
    role = org.get_role("Engineer")
    assert role is not None
    role.sandbox = RoleSandboxConfig(enabled=True, coding_agent="claude-code")
    return AgentDefinition(role=role, org=org)


def test_plan_prompt_sandbox_section_gated_off_by_default():
    """The sandbox Plan section must be absent for the ~all roles that
    aren't sandbox-enabled, so it never bloats the common Plan prompt."""
    p = build_plan_prompt(_def(), tool_catalogue="")
    assert "Sandbox Execute" not in p
    assert "execution_backend" not in p


def test_plan_prompt_sandbox_section_rendered_when_enabled():
    p = build_plan_prompt(_sandbox_def(), tool_catalogue="")
    # Code work is the run_sandbox tool: plan it in tools_needed + the
    # report/act tool, and the executor continues the same turn with the
    # result (no separate report turn).
    assert "run_sandbox" in p
    assert "tools_needed" in p
    assert "resumes automatically" in p or "resume" in p


def test_plan_prompt_catalogues_registry_skill_when_trigger_matches():
    """Plan emits a one-line catalogue entry (key + summary) when a
    skill's trigger matches; bodies are NOT inlined — the LLM loads
    them on demand via ``load_tool_skill``."""
    skill = PromptSkill(
        key="tool:refresh_memory",
        trigger=TriggerExpr(tool="refresh_memory"),
        phases={Phase.PLAN},
        title="Re-querying personal memory",
        summary="Re-filter personal memory after recon changed the picture.",
        body="LONG BODY THAT SHOULD NOT APPEAR IN PROMPT",
    )
    reg = _registry([skill])
    without = build_plan_prompt(
        _def("Engineer"),
        tool_catalogue="",
        available_tools=[],
        skill_registry=reg,
    )
    assert "tool:refresh_memory" not in without
    with_tool = build_plan_prompt(
        _def("Engineer"),
        tool_catalogue="",
        available_tools=["refresh_memory"],
        skill_registry=reg,
    )
    # Catalogue parent header + the skill's summary line; not its body.
    assert "## Tool skills" in with_tool
    assert "tool:refresh_memory" in with_tool
    assert "Re-filter personal memory after recon" in with_tool
    assert "LONG BODY THAT SHOULD NOT APPEAR IN PROMPT" not in with_tool


def test_plan_prompt_catalogue_substitutes_skill_variables():
    """A ${var} in a skill summary is rendered with the registry's
    variables when the catalogue line is emitted into the prompt."""
    skill = PromptSkill(
        key="tool:refresh_memory",
        trigger=TriggerExpr(tool="refresh_memory"),
        phases={Phase.PLAN},
        title="t",
        summary="base is ${confluence_base_url}",
        body="b",
    )
    reg = _registry([skill])
    reg.set_variables({"confluence_base_url": "https://acme.atlassian.net/wiki"})
    p = build_plan_prompt(
        _def("Engineer"),
        tool_catalogue="",
        available_tools=["refresh_memory"],
        skill_registry=reg,
    )
    assert "base is https://acme.atlassian.net/wiki" in p
    assert "${confluence_base_url}" not in p


def test_plan_prompt_skill_catalogue_lands_immediately_before_tool_catalogue():
    """The Tool skills catalogue is conceptually one section with the
    Available tools catalogue — they must render adjacent so the LLM
    reads "how to use these" directly above "here are the tool names"."""
    skill = PromptSkill(
        key="tool:x",
        trigger=TriggerExpr(tool="x"),
        title="X tool",
        summary="Tight summary of X.",
        body="LONG BODY OF X",
    )
    reg = _registry([skill])
    p = build_plan_prompt(
        _def("Engineer"),
        tool_catalogue="- x: does x",
        available_tools=["x"],
        skill_registry=reg,
    )
    assert "## Tool skills" in p
    assert "## Available tools" in p
    assert p.index("## Tool skills") < p.index("## Available tools")
    # The whole "## Personal memory" / prefetch run lands BEFORE the
    # catalogue so the LLM gets task context, then tool guidance,
    # then the tool names — see the LLM-perspective review notes.
    # We just check that no other section sits between Tool skills
    # and Available tools.
    between = p[p.index("## Tool skills") : p.index("## Available tools")]
    assert between.count("##") == 1  # only the Tool skills heading


def test_plan_prompt_no_skill_prose_without_registry():
    """The engine ships no skill prose. An empty registry (or None) plus
    a role with every conceivable tool / MCP server produces zero
    injected skill scaffolding — only the catalogue and core sections."""
    p = build_plan_prompt(
        _def("Engineer"),
        tool_catalogue="",
        available_tools=[
            "reflect_and_persist",
            "refine_skill",
            "query_knowledge",
            "refresh_memory",
            "slack_conversations_postMessage",
        ],
        skill_registry=None,
    )
    # Neither the catalogue header nor any hardcoded skill-section
    # headings appear.
    assert "## Tool skills" not in p
    assert "## Persisting durable facts" not in p
    assert "## Skill refinement" not in p
    assert "## Re-querying memory & knowledge after recon" not in p
    assert "## Sharing observed directives" not in p
    assert "## GitHub tools" not in p
    assert "## Mentioning teammates" not in p


def test_plan_prompt_renders_org_and_role_context_inline() -> None:
    """Mission, vision, behavioral guidelines, responsibilities all
    render inline in the Plan prompt rather than going through a
    knowledge fetch."""
    p = build_plan_prompt(_def("Engineer"), tool_catalogue="")
    assert "Build great things." in p  # mission
    assert "Be the best." in p  # vision
    assert "Be concise." in p  # behavioral guideline
    assert "Write tests." in p  # responsibility
    # The planner has the full context inline — no knowledge-fetch
    # pointer block.
    assert "Mission, vision, policies" in p


# -- inline-context section builders --------------------------------------


def test_plan_prompt_includes_company_context_section() -> None:
    """Mission + vision render under a ``## Company Context`` heading."""
    p = build_plan_prompt(_def("Engineer"), tool_catalogue="")
    assert "## Company Context" in p
    assert "Build great things." in p
    assert "Be the best." in p


def test_plan_prompt_omits_company_context_when_empty() -> None:
    """When the org has no mission/vision, the section is dropped."""
    org = _org()
    org.mission = ""
    org.vision = ""
    role = org.get_role("Engineer")
    assert role is not None
    d = AgentDefinition(role=role, org=org)
    p = build_plan_prompt(d, tool_catalogue="")
    assert "## Company Context" not in p


def test_plan_prompt_includes_role_profile_sections() -> None:
    """Backstory / responsibilities / behavioral guidelines each get
    their own section."""
    p = build_plan_prompt(_def("Engineer"), tool_catalogue="")
    assert "## Your Responsibilities" in p
    assert "- Write tests." in p
    assert "## Behavioral Guidelines" in p
    assert "- Be concise." in p


def test_plan_prompt_omits_role_profile_when_role_is_minimal() -> None:
    """A role with no backstory / responsibilities / guidelines gets
    no profile section -- empty bullet lists are visual noise."""
    org = _org()
    role = org.get_role("Engineer")
    assert role is not None
    role.backstory = ""
    role.responsibilities = []
    role.behavioral_guidelines = []
    d = AgentDefinition(role=role, org=org)
    p = build_plan_prompt(d, tool_catalogue="")
    assert "## Your Background" not in p
    assert "## Your Responsibilities" not in p
    assert "## Behavioral Guidelines" not in p


def test_plan_prompt_includes_unit_context_section() -> None:
    """Containing unit's purpose + goals render under
    ``## Your Unit (<unit name>)``."""
    p = build_plan_prompt(_def("Engineer"), tool_catalogue="")
    assert "## Your Unit (Eng Team)" in p
    assert "**Purpose:** Build the thing." in p
    assert "- Ship v1.0." in p


def test_plan_prompt_catalogues_mcp_skill_when_server_present() -> None:
    """A ``mcp:github`` skill in the registry shows up as a catalogue
    entry when the role's ``mcp_env`` carries the matching server."""
    github_skill = PromptSkill(
        key="mcp:github",
        trigger=TriggerExpr(mcp_server="github"),
        phases={Phase.PLAN},
        title="GitHub tools",
        summary="GitHub MCP tools incl. Copilot delegation.",
        body="LONG GITHUB BODY",
    )
    reg = _registry([github_skill])
    org = _org()
    role = org.get_role("Engineer")
    assert role is not None
    role.mcp_env = {**role.mcp_env, "github": {}}
    d = AgentDefinition(role=role, org=org)
    p = build_plan_prompt(d, tool_catalogue="", skill_registry=reg)
    assert "mcp:github" in p
    assert "GitHub MCP tools incl. Copilot delegation." in p
    assert "LONG GITHUB BODY" not in p  # body loads on demand, not eagerly


def test_plan_prompt_does_not_catalogue_mcp_skill_when_server_absent() -> None:
    """Same registry, role without the matching MCP server → no entry."""
    github_skill = PromptSkill(
        key="mcp:github",
        trigger=TriggerExpr(mcp_server="github"),
        phases={Phase.PLAN},
        title="GitHub tools",
        summary="GitHub MCP tools.",
        body="body",
    )
    reg = _registry([github_skill])
    # Engineering Lead has no github in mcp_env.
    p = build_plan_prompt(
        _def("Engineering Lead"), tool_catalogue="", skill_registry=reg
    )
    assert "mcp:github" not in p


def _mentions_skill(phases: set[Phase]) -> PromptSkill:
    return PromptSkill(
        key="skill:platform_mentions",
        trigger=TriggerExpr(
            any_of=[
                TriggerExpr(mcp_server="atlassian"),
                TriggerExpr(mcp_server="slack"),
            ]
        ),
        phases=phases,
        title="Mentioning teammates",
        summary="Per-platform mention markup for Jira / Confluence / Slack.",
        body="Jira: [~accountid:<jira_id>] — Confluence: ri:user account-id markup.",
    )


def test_plan_prompt_catalogues_mention_skill_for_atlassian_role() -> None:
    reg = _registry([_mentions_skill({Phase.PLAN})])
    p = build_plan_prompt(_def("Engineer"), tool_catalogue="", skill_registry=reg)
    assert "skill:platform_mentions" in p
    assert "Per-platform mention markup" in p
    # Body not inlined — operator loads via load_tool_skill.
    assert "[~accountid:" not in p


def test_execute_prompt_catalogues_mention_skill_for_atlassian_role() -> None:
    """Execute sees the skill as a catalogue entry; the body loads
    on demand via the always-on ``load_tool_skill`` builtin."""
    reg = _registry([_mentions_skill({Phase.PLAN, Phase.EXECUTE})])
    p = build_execute_prompt(
        _def("Engineer"),
        plan_summary="…",
        skill_registry=reg,
    )
    assert "skill:platform_mentions" in p
    assert "Per-platform mention markup" in p
    assert "[~accountid:" not in p  # body not inlined


def test_review_prompt_catalogues_mention_skill_for_atlassian_role() -> None:
    """Review-phase tool skills are operator-scoped: a skill that lists
    ``review`` in its phases surfaces in the Review prompt keyed on the
    role's MCP servers, even though Review has no domain-tool surface."""
    reg = _registry([_mentions_skill({Phase.PLAN, Phase.EXECUTE, Phase.REVIEW})])
    p = build_review_prompt(_def("Engineer"), skill_registry=reg)
    assert "skill:platform_mentions" in p
    assert "Per-platform mention markup" in p
    assert "[~accountid:" not in p  # body not inlined


def test_plan_prompt_team_roster_includes_member_profiles_for_lead() -> None:
    """A unit lead's roster inlines
    each member's backstory + goal + responsibilities + jira username
    so the lead can reason about task assignment without a separate
    knowledge fetch."""
    p = build_plan_prompt(_def("Engineering Lead"), tool_catalogue="")
    assert "## Your Team" in p
    assert "**Engineer**" in p
    # Engineer's profile detail surfaces under the roster bullet.
    assert "Goal: Ship quality code." in p
    assert "Responsibilities: Write tests." in p


# -- Execute prompt -------------------------------------------------------


def test_execute_prompt_has_no_catalogue():
    """Executor must NOT see the tool catalogue."""
    p = build_execute_prompt(_def())
    assert "## Available tools" not in p
    assert "EXECUTE phase" in p


def test_execute_prompt_has_no_roster():
    """Executor must NOT see the team roster."""
    p = build_execute_prompt(_def("Engineering Lead"))
    assert "## Your Team" not in p


def test_execute_prompt_includes_plan_summary_if_given():
    p = build_execute_prompt(_def(), plan_summary="1. Do X\n2. Do Y")
    assert "Do X" in p
    assert "Do Y" in p


def test_execute_prompt_forwards_counterparty_profile_when_given():
    """When the planner picks ``decision='direct'`` with abstract
    reasoning ('use the counterparty's preferred greeting format')
    the executor would otherwise have no way to know what 'preferred
    greeting' actually means.  Forwarding the same Plan-prefetch
    counterparty block to Execute fixes the personalization gap."""
    profile_block = (
        "Subject: U0TESTUSER1\n"
        "Platform: slack\n"
        "Observed by you:\n"
        "  - preferred_greeting: hey sam\n"
        "  - directives: expects 'hey sam' as opening of every reply\n"
    )
    p = build_execute_prompt(
        _def(),
        plan_summary="Reply with the counterparty's preferred greeting.",
        counterparty_profile=profile_block,
    )
    assert "## Known counterparty" in p
    assert "hey sam" in p


def test_execute_prompt_omits_counterparty_section_when_empty():
    """An empty counterparty block (the common case — most turns have
    no identifiable subject) must not leave a stub heading behind."""
    p = build_execute_prompt(_def(), plan_summary="do thing")
    assert "## Known counterparty" not in p


def test_execute_prompt_includes_relevant_knowledge_block_when_given():
    """The post-Plan relevant-knowledge re-fetch (thin-trigger turns
    only) is injected into the Execute prompt as a ``## Relevant
    knowledge`` section so the executor sees the docs the Plan-phase
    prefetch was gated out of surfacing."""
    block = "- **Incident Response Runbook**: Steps for a sev-1."
    p = build_execute_prompt(
        _def(),
        plan_summary="Investigate the latency spike.",
        relevant_knowledge_block=block,
    )
    assert "## Relevant knowledge" in p
    assert "Incident Response Runbook" in p


def test_execute_prompt_omits_relevant_knowledge_section_when_empty():
    """Empty block (every non-thin-trigger turn — the common case) must
    not leave a stub heading behind."""
    p = build_execute_prompt(_def(), plan_summary="do thing")
    assert "## Relevant knowledge" not in p


def test_execute_prompt_relevant_knowledge_lands_between_plan_and_counterparty():
    """Section order: Plan → Relevant knowledge → Known counterparty,
    matching the Plan-prompt ordering."""
    p = build_execute_prompt(
        _def(),
        plan_summary="do the thing",
        relevant_knowledge_block="- **Doc A**: x",
        counterparty_profile="Subject: bob",
    )
    assert p.index("## Plan") < p.index("## Relevant knowledge")
    assert p.index("## Relevant knowledge") < p.index("## Known counterparty")


def test_execute_prompt_drops_policies_and_company_context():
    """Policies and mission/vision should NOT be in Execute — the plan's
    success_criteria carries policy-driven constraints forward from
    Plan."""
    p = build_execute_prompt(_def())
    assert "Company Policies" not in p
    assert "Company policies" not in p
    assert "Respect teammates." not in p
    assert "Build great things." not in p  # mission


def test_execute_prompt_uses_one_line_identity():
    p = build_execute_prompt(_def("Engineer"))
    assert "Engineer" in p
    assert "Acme" in p
    # The full identity block header lives in Plan only.
    assert "# Your Identity" not in p


def test_identity_line_reports_to_phrasing_matches_identity_section():
    """Execute/Review identity line must use the same 'None (top-level)'
    phrasing as the Plan-phase identity section for root-level roles,
    not a bare 'none'."""
    # Root-level Engineer (no manager).
    org = Organization(name="Acme", roles=[Role(name="Engineer", goal="Ship.")])
    role = org.get_role("Engineer")
    assert role is not None
    d = AgentDefinition(role=role, org=org)
    plan_p = build_plan_prompt(d, tool_catalogue="")
    exec_p = build_execute_prompt(d)
    assert "None (top-level)" in plan_p
    assert "None (top-level)" in exec_p


# -- Review prompt -------------------------------------------------------


def test_review_prompt_describes_decision_enum():
    p = build_review_prompt(_def())
    assert "done" in p
    assert "self_iterate" in p
    # ask_colleague was removed — Review only decides done | self_iterate.
    assert "ask_colleague" not in p


def test_review_prompt_covers_missing_tool_and_blocked_rules():
    """Production failures the rules prevent:

    1. Missing-tool: Execute narrates "I don't have Jira access" but
       the tool IS in the catalogue; it just wasn't in ``tools_needed``.
       Right answer is ``self_iterate``: Plan re-lists ``tools_needed``
       with the missing tool and Execute gets it next pass.

    2. Blocked / needs-a-colleague: the turn can't finish without a
       manager or peer.  There is no handoff decision -- the reviewer
       chooses ``self_iterate`` and Plan adds an outreach step so
       Execute reaches the colleague with its own tools.
    """
    p = build_review_prompt(_def())
    # Missing-tool rule names the failure pattern and the right
    # decision -- so the reviewer has a single phrase to anchor
    # against.
    assert "Missing-tool rule" in p
    assert "re-list `tools_needed`" in p
    # And the specific narration shape Execute tends to emit when
    # it lacks a tool -- gives the reviewer something concrete to
    # match against in ``## What Execute produced`` (phrased by
    # capability, not by any one integration's name).
    assert "I don't have access to the tool needed to deliver this" in p
    # Blocked / needs-a-colleague rule routes escalation through
    # self_iterate + an Execute-phase outreach step (no handoff decision).
    assert "Blocked / needs-a-colleague rule" in p
    assert "self_iterate" in p


def test_submit_review_tool_description_covers_missing_tool_and_handoff():
    """Companion to the review-prompt header rules: the submit_review
    tool description is the *other* place the reviewer reads policy
    before calling, so the same failure modes need to be spelled out
    there too."""
    from crewlet.agent.review import _build_review_meta_tools

    review_tools = _build_review_meta_tools()
    submit_review = next(t for t in review_tools if t.name == "submit_review")
    desc = submit_review.description
    # Missing-tool guidance: pick self_iterate when Execute mentions
    # a tool that just wasn't requested in tools_needed.
    assert "lacks a tool" in desc
    assert "self_iterate" in desc
    # Blocked / needs-a-colleague guidance: still self_iterate, with an
    # outreach step Execute runs with its own tools.
    assert "colleague or manager" in desc


def test_review_prompt_has_no_catalogue():
    p = build_review_prompt(_def())
    assert "## Available tools" not in p


def test_review_prompt_includes_summaries_if_given():
    p = build_review_prompt(
        _def(),
        plan_summary="PLAN:1,2,3",
        execute_summary="EXECUTED:x",
    )
    assert "PLAN:1,2,3" in p
    assert "EXECUTED:x" in p


def test_review_prompt_includes_execute_tool_log():
    """Review must see Execute's actual tool-call log so it can judge
    against evidence, not Execute's text narration."""
    p = build_review_prompt(
        _def(),
        plan_summary="PLAN",
        execute_summary="I posted the reply",
        execute_tool_log="- slack_conversations_add_message(...) → success",
    )
    # Anchored on the section break so the assertion matches the
    # rendered block, not the in-text references inside REVIEW_HEADER.
    assert "\n## What Execute did\n" in p
    assert "slack_conversations_add_message" in p
    # Header still teaches the reviewer to weight evidence over narration.
    assert "are the evidence" in p


def test_review_prompt_renders_none_when_no_tool_log():
    """Both tool-log sections are always rendered (REVIEW_HEADER points
    at them as the primary evidence). Empty / missing logs show
    ``(none)`` so Review sees the affirmative "no action taken" signal,
    and the ordering (Plan before Execute) is preserved so the reviewer
    reads a chronological timeline even when both phases were idle."""
    p = build_review_prompt(_def(), plan_summary="P", execute_summary="E")
    # Header still references the sections as evidence.
    assert "are the evidence" in p
    # Both section headings render with explicit empty markers.
    plan_idx = p.index("\n## What Plan did\n(none)")
    execute_idx = p.index("\n## What Execute did\n(none)")
    # Plan precedes Execute in the (none) case too — the timeline order
    # must hold uniformly, not just when the logs have entries.
    assert plan_idx < execute_idx


def test_review_prompt_includes_plan_tool_log():
    """Review must see what Plan already did during recon -- otherwise
    it can't tell whether a side effect listed in ``tools_needed`` was
    already delivered by the planner and would incorrectly demand
    Execute repeat it.  Production failure: planner posted the Slack
    reply during recon, Execute didn't repeat, Review self-iterated
    despite the work being done."""
    p = build_review_prompt(
        _def(),
        plan_summary="PLAN",
        execute_summary="E",
        execute_tool_log="(none)",
        plan_tool_log="- slack_conversations_add_message(...) → success",
    )
    # Section heading + content.  Anchored on the surrounding newlines
    # so we match the section break, not the in-text reference inside
    # the REVIEW_HEADER prose.
    plan_section_idx = p.index("\n## What Plan did\n")
    execute_section_idx = p.index("\n## What Execute did\n")
    assert "slack_conversations_add_message" in p
    # Plan section renders BEFORE Execute section so the reviewer
    # reads top-to-bottom as a chronological timeline.
    assert plan_section_idx < execute_section_idx
    # Header teaches the reviewer that Plan-phase calls count as
    # delivered.
    assert "already-delivered" in p


def test_review_prompt_tool_delivery_rule_spans_both_logs():
    """The tool-delivery rule must accept either log as evidence of
    delivery -- not just Execute's -- otherwise the rule contradicts
    the new ``## What Plan did`` section."""
    p = build_review_prompt(_def())
    assert "Tool-delivery rule" in p
    assert "EITHER log" in p


def test_review_prompt_failed_calls_do_not_count_as_delivery():
    """REVIEW_HEADER must explicitly tell the reviewer that
    ``→ error`` calls are NOT delivery — otherwise a failed Plan-phase
    Slack post (5xx, channel-not-found, permission-denied) gets read
    as ``Plan delivered`` and the reviewer picks ``done`` against a
    side effect that never landed."""
    p = build_review_prompt(_def())
    assert "→ error" in p
    assert "do not" in p.lower() or "do NOT" in p


def test_review_prompt_duplicate_delivery_rule_present():
    """REVIEW_HEADER must teach the reviewer to self_iterate when both
    phases delivered the same side effect — without it, the new "Plan
    calls count as already-delivered" guidance invites the reviewer to
    bless a turn that posted the same Slack message twice."""
    p = build_review_prompt(_def())
    assert "Duplicate-delivery rule" in p
    assert "self_iterate" in p


def test_review_prompt_drops_policies_and_company_context():
    p = build_review_prompt(_def())
    assert "Company Policies" not in p
    assert "Company policies" not in p
    assert "Respect teammates." not in p
    assert "Build great things." not in p  # mission
    assert "# Your Identity" not in p


# -- Onboarding prompt ---------------------------------------------------


def test_onboarding_header_is_backend_neutral():
    """The one-time onboarding pass points at the team's knowledge-base
    MCP server by capability, not by product -- the same header must
    read correctly for Confluence orgs and Plane orgs alike."""
    assert "knowledge-base search / read tools" in ONBOARDING_HEADER
    assert (
        "a page-search / get-page tool on your team's knowledge-base server"
    ) in ONBOARDING_HEADER
    lowered = ONBOARDING_HEADER.lower()
    assert "confluence" not in lowered
    assert "atlassian" not in lowered
    assert "plane" not in lowered


def test_onboarding_prompt_includes_header_hint_and_catalogue():
    p = build_onboarding_prompt(
        _def(),
        onboarding_hint="Read the 'Onboarding' pages on your chain.",
        tool_catalogue="- knowledge_search: Search team pages.",
    )
    assert "ONBOARDING phase" in p
    assert "## What to do" in p
    assert "Read the 'Onboarding' pages on your chain." in p
    assert "## Available tools" in p
    assert "knowledge_search: Search team pages." in p


def test_onboarding_prompt_omits_empty_sections():
    p = build_onboarding_prompt(_def(), onboarding_hint="")
    assert "## What to do" not in p
    assert "## Available tools" not in p


# -- Sub-agent prompt ----------------------------------------------------


def test_subagent_prompt_appends_mandated_preamble():
    parent = "You are a web research worker. Return a concise summary."
    p = build_subagent_prompt(_def(), parent_system_prompt=parent)
    assert parent in p
    assert SUBAGENT_PREAMBLE in p
    # Order: parent-provided first, preamble appended.
    assert p.index(parent) < p.index(SUBAGENT_PREAMBLE)


def test_subagent_preamble_content():
    """The preamble must state the required sub-agent invariants exactly."""
    assert "Do not spawn further sub-agents" in SUBAGENT_PREAMBLE
    assert "Do not contact colleagues" in SUBAGENT_PREAMBLE
    assert "concise final answer" in SUBAGENT_PREAMBLE


# -- Size budget ---------------------------------------------------------


def test_plan_prompt_with_big_catalogue_stays_under_token_budget():
    """Reference role: 5 skills, 3 MCP servers, roster, catalogue.

    Token budget: < 2400 tokens for the Plan phase under a 100-tool
    catalogue.  The budget covers the identity scaffold, the
    recon-then-action rule, the verbose-decline rule, and the
    slim-catalogue + ``list_mcp_server_tools`` discovery flow header;
    the ~50 tokens of discovery prose replaces the typical 1500-token
    MCP-tool-name listing in real workloads — a net win as soon as one
    MCP server with >5 tools is wired.
    We approximate tokens as chars/4 (conservative) so the test is
    stable across tokenisers.
    """
    # 100 tools x ~60 chars each = ~6k chars of catalogue = ~1500 tokens
    big_catalogue = "\n".join(
        f"- tool_{i}: Short description of tool number {i}." for i in range(100)
    )
    p = build_plan_prompt(_def("Engineering Lead"), tool_catalogue=big_catalogue)
    approx_tokens = len(p) // 4
    assert approx_tokens < 2400, f"Plan prompt too large: ~{approx_tokens} tokens"


def test_execute_prompt_is_tiny():
    """Execute body (no plan summary) should be <300 tokens — the
    prompt is identity + 4-line contract and nothing else."""
    p = build_execute_prompt(_def("Engineering Lead"))
    approx_tokens = len(p) // 4
    assert approx_tokens < 300, f"Execute prompt too large: ~{approx_tokens} tokens"


def test_review_prompt_is_small():
    """Review body (no summaries) stays <600 tokens — identity +
    decision enum + tool-delivery / sandbox / missing-tool / blocked /
    duplicate-delivery rules and the `completed_work` instruction.  The
    original budget was 300 (identity + 6-line decision enum); the
    rules added ~290 tokens to prevent real production failures (silent
    half-finished turns when Plan didn't list the right tools; the
    sandbox rule, which stops Review from looping a turn forever by
    mis-reading a `run_sandbox`-delegated investigation as "fabricated";
    and the cross-iteration duplicate rule, which stops a second
    `self_iterate` pass re-firing a side effect that already landed).
    The trade is worth it -- each rule maps to a turn-ending bug we have
    actually observed.

    Raised 560 -> 600 when the duplicate rule had to be keyed on target
    and content rather than tool name: keyed on the name alone it fired
    on the in-thread follow-up ``PRIOR_WORK_HEADER`` explicitly asks
    for, so every corrected turn looped to ``max_iterations`` and
    terminated ``failed``.  Headroom is ~13 tokens on purpose: the next
    addition should have to justify itself here, not slip in."""
    p = build_review_prompt(_def("Engineering Lead"))
    approx_tokens = len(p) // 4
    assert approx_tokens < 600, f"Review prompt too large: ~{approx_tokens} tokens"


def test_execute_prompt_smaller_than_plan_prompt():
    d = _def("Engineering Lead")
    plan = build_plan_prompt(d, tool_catalogue="- foo: Does foo.")
    execute = build_execute_prompt(d)
    assert len(execute) < len(plan)


def test_review_prompt_smaller_than_plan_prompt():
    d = _def("Engineering Lead")
    plan = build_plan_prompt(d, tool_catalogue="- foo: Does foo.")
    review = build_review_prompt(d)
    assert len(review) < len(plan)


# ---------------------------------------------------------------------------
# Human seats in prompts
# ---------------------------------------------------------------------------


def _mixed_org() -> Organization:
    sarah = Role(
        name="Sarah Chen",
        kind="human",
        email="sarah@acme.com",
        contact={"slack_user_id": "U0HUMAN", "atlassian_account_id": "5b10-s"},
        availability="CET business hours; replies within ~4h",
        backstory="20 years in infrastructure.",
        manages=["Engineer"],
    )
    engineer = Role(name="Engineer", handle="eng", goal="Ship quality code.")
    unit = OrgUnit(
        name="Eng Team",
        type="team",
        lead="Sarah Chen",
        roles=[sarah, engineer],
    )
    return Organization(name="Acme", units=[unit])


def test_identity_section_marks_human_manager():
    org = _mixed_org()
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    prompt = build_plan_prompt(defn, tool_catalogue="(none)")
    assert "**Reports to:** Sarah Chen (human)" in prompt


def test_plan_prompt_includes_human_colleagues_note_in_mixed_org():
    org = _mixed_org()
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    prompt = build_plan_prompt(defn, tool_catalogue="(none)")
    assert "## Human colleagues" in prompt
    assert "NOT on the A2A bus" in prompt
    assert "asynchronously" in prompt


def test_plan_prompt_omits_human_note_in_pure_agent_org():
    org = _org()
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    prompt = build_plan_prompt(defn, tool_catalogue="(none)")
    assert "## Human colleagues" not in prompt


def test_roster_renders_human_member_block():
    org = Organization(
        name="Acme",
        units=[
            OrgUnit(
                name="Eng Team",
                type="team",
                lead="Lead",
                roles=[
                    Role(name="Lead", handle="lead"),
                    Role(
                        name="Sarah Chen",
                        kind="human",
                        email="sarah@acme.com",
                        contact={
                            "slack_user_id": "U0HUMAN",
                            "atlassian_account_id": "5b10-s",
                        },
                        availability="CET business hours",
                    ),
                    Role(name="Engineer", handle="eng"),
                ],
            )
        ],
    )
    defn = AgentDefinition(role=org.get_role("Lead"), org=org)
    prompt = build_plan_prompt(defn, tool_catalogue="(none)")
    assert "**Sarah Chen** (sarah-chen) — **human teammate**" in prompt
    # Contact identities render generically from
    # HumanContact.resolved_identities() (labelled by transport), not
    # from hardcoded field names.  The shared Atlassian id renders once
    # under its first transport (jira).
    assert "Slack ID: U0HUMAN" in prompt
    assert "Jira ID: 5b10-s" in prompt
    assert "Availability: CET business hours" in prompt
    assert "hand work over in the PM tool" in prompt
    # Agent members keep the regular rendering.
    assert "**Engineer** (eng)" in prompt


def test_identity_line_marks_human_manager():
    from crewlet.agent.definition import build_identity_line

    org = _mixed_org()
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    line = build_identity_line(defn)
    assert "Sarah Chen (human)" in line


def test_definition_system_prompt_marks_humans():
    org = _mixed_org()
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    prompt = defn.system_prompt
    assert "**Reports to:** Sarah Chen (human)" in prompt
    assert "## Human colleagues" in prompt


def test_review_prompt_acknowledges_async_colleague_replies():
    """Review's blocked/needs-a-colleague rule tells the reviewer the
    colleague (human or agent) replies asynchronously and that
    re-triggers the agent — so it should self_iterate and let Execute
    do the outreach rather than wait."""
    org = _mixed_org()
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    prompt = build_review_prompt(defn, plan_summary="p", execute_summary="e")
    assert "reply asynchronously" in prompt


# ---------------------------------------------------------------------------
# Decoupling guard
# ---------------------------------------------------------------------------

# Concrete integration tool names that must never be hardcoded into the
# phase contracts.  The engine must describe delivery / colleague /
# knowledge tools by capability and let the LLM pick the real tool from
# its (MCP-sourced) catalogue — so none of these may appear in a core
# prompt.  Guards against a regression that re-couples the engine to one
# tool stack.
_FORBIDDEN_TOOL_NAMES = (
    "slack_conversations_add_message",
    "slack_conversations_postMessage",
    "jira_add_comment",
    "jira_transition_issue",
    "jira_update_issue",
    "jira_create_issue",
    "confluence_add_footer_comment",
    "confluence_search",
    "confluence_get_page",
    "request_copilot_review",
    "create_pull_request_with_copilot",
)


def test_plan_prompt_names_no_hardcoded_integration_tools():
    """The Plan contract steers by capability, never by a vendor tool
    name."""
    p = build_plan_prompt(_def(), tool_catalogue="")
    for name in _FORBIDDEN_TOOL_NAMES:
        assert name not in p, f"Plan prompt hardcodes {name!r}"


def test_review_prompt_names_no_hardcoded_integration_tools():
    p = build_review_prompt(_def())
    for name in _FORBIDDEN_TOOL_NAMES:
        assert name not in p, f"Review prompt hardcodes {name!r}"


def test_execute_prompt_warns_about_phantom_tools():
    """When the planner named tools that don't resolve in Execute's
    catalogue (wrong MCP-tool-name guesses), Execute is told explicitly
    so it discovers the real tool instead of stopping at a text reply."""
    p = build_execute_prompt(
        _def(),
        plan_summary="reply to the greeting",
        phantom_tools=["slack_conversations_postMessage"],
    )
    assert "Heads-up" in p
    assert "slack_conversations_postMessage" in p
    assert "list_mcp_server_tools" in p
    assert "does not deliver" in p


def test_execute_prompt_no_phantom_note_when_all_resolve():
    p = build_execute_prompt(_def(), plan_summary="x")
    assert "Heads-up" not in p


def test_phase_contract_headers_name_no_specific_platforms():
    """The engine-authored phase contracts (the PLAN_HEADER /
    REVIEW_HEADER constants) should not lean on specific product names
    (Slack/Jira/Confluence/Copilot) for their delivery-tool examples —
    those are deployment choices, not engine knowledge.  (Config-derived
    identity like a unit's configured chat channel is data, not contract
    prose, so we check the constants, not the assembled prompt.)"""
    # The header still teaches the *concept* of an originating-channel
    # reply tool…
    assert "originating channel's reply tool" in PLAN_HEADER
    assert "post/reply tool" in PLAN_HEADER
    # …without naming a specific chat / tracker / wiki / code product.
    for product in ("Slack", "Jira", "Confluence", "Copilot", "GitHub"):
        assert product not in PLAN_HEADER, f"PLAN_HEADER names {product!r}"
        assert product not in REVIEW_HEADER, f"REVIEW_HEADER names {product!r}"


def test_review_prompt_omits_earlier_iterations_by_default():
    """First iteration of a turn: no section over an empty ledger."""
    p = build_review_prompt(_def(), plan_summary="P", execute_summary="E")
    assert "\n## Earlier iterations (already delivered)\n" not in p


def test_review_prompt_renders_earlier_iterations_before_this_round():
    """Both per-phase tool logs reset each iteration (so the delivery
    gate can't read iter-1 calls as iter-2 delivery), which left the
    reviewer blind to a repeat across iterations. This section restores
    that view, and lands first so the whole turn reads as one timeline.
    """
    p = build_review_prompt(
        _def(),
        plan_summary="PLAN",
        execute_summary="E",
        execute_tool_log="- slack_post(...) → success",
        earlier_iterations=(
            "### Iteration 1\nExecute called:\n- slack_post(...) → success"
        ),
    )
    earlier_idx = p.index("\n## Earlier iterations (already delivered)\n")
    plan_idx = p.index("\n## What Plan did\n")
    assert earlier_idx < plan_idx
    assert "### Iteration 1" in p


def test_review_header_teaches_cross_iteration_duplicate_rule():
    """A repeat of an already-delivered call is a duplicate too — the
    rule is worthless if it only sees inside one iteration."""
    assert "Earlier iterations" in REVIEW_HEADER
    assert "completed_work" in REVIEW_HEADER


def test_phase_user_message_without_prior_work_is_unchanged():
    """The common single-pass turn must keep its exact pre-ledger shape
    so nothing shifts for turns that never self_iterate."""
    assert (
        build_phase_user_message(task_description="do the thing")
        == "Task:\ndo the thing"
    )
    assert build_phase_user_message(task_description="") == "Task:\n(no description)"


def test_phase_user_message_appends_prior_work_block():
    msg = build_phase_user_message(
        task_description="post the summary",
        prior_work="### Iteration 1\nExecute called:\n- slack_post(...) → success",
    )
    assert msg.startswith("Task:\npost the summary")
    assert "Already done earlier in this turn" in msg
    assert "### Iteration 1" in msg
    # The rule that actually prevents the double-post.
    assert "ALREADY RAN" in msg
