"""AgentDefinition — built from a Role, defines what an agent does."""

from __future__ import annotations

from typing import TYPE_CHECKING

from crewlet.org.models import Organization, Role

if TYPE_CHECKING:
    from crewlet.agent.skills import PromptSkillRegistry


class AgentDefinition:
    """Defines an agent's behavior, built from an org Role.

    Each Role maps 1:1 to an AgentDefinition and a single AgentInstance.
    For unit lead agents (designated via ``OrgUnit.lead``), the system
    prompt includes a lead banner and a roster so the LLM can reason
    about task assignment.
    """

    def __init__(
        self,
        role: Role,
        org: Organization,
    ) -> None:
        self.role = role
        self.org = org
        self._system_prompt: str | None = None

    @property
    def role_name(self) -> str:
        return self.role.name

    @property
    def llm_provider_key(self) -> str:
        return self.role.llm

    @property
    def mcp_env(self) -> dict[str, dict[str, str]]:
        """Per-role MCP server env overrides (credentials, config).

        When a role specifies ``mcp_env``, a dedicated MCP server
        instance is launched with these env vars merged on top of
        the base server config. This gives each role its own
        identity (token, email) in external tools like Jira/Slack.
        """
        return self.role.mcp_env

    @property
    def email(self) -> str:
        return self.role.email

    @property
    def system_prompt(self) -> str:
        """Generate a monolithic system prompt that composes every
        section builder (identity, reporting relationships, skills,
        guidelines, tool-use policy, etc.) into one string.

        The main runtime path (:class:`~crewlet.agent.turn.TurnEngine`)
        does **not** call this: each phase assembles its own narrower
        prompt via the builders in :mod:`crewlet.agent.prompts`.  This
        property is retained for external callers and introspection
        tests -- notably the skill-injection tests, role-update
        verification in ``tests/test_engine.py``, and anything that
        just wants the full composed text for logging / debugging.
        Use the per-phase builders in new code.
        """
        if self._system_prompt is not None:
            return self._system_prompt

        from crewlet.org.hierarchy import (
            get_manager,
            get_reports,
            get_unit_for_role,
        )

        manager = get_manager(self.role, self.org)
        reports = get_reports(self.role, self.org)
        unit = get_unit_for_role(self.role, self.org)

        manager_str = _seat_label(manager) if manager else "None (top-level)"
        reports_str = ", ".join(_seat_label(r) for r in reports) if reports else "None"
        is_lead = bool(reports)

        # ── Section 1: Company Context ──
        # Employees know what their company does without having to look
        # it up.  This anchors every decision in organizational purpose.
        parts: list[str] = [f"# {self.org.name}"]
        if self.org.mission:
            parts.append(f"**Mission:** {self.org.mission}")
        if self.org.vision:
            parts.append(f"**Vision:** {self.org.vision}")

        # ── Section 2: Professional Identity ──
        # Role clarity is the strongest predictor of employee
        # effectiveness — employees with high role clarity are 53% more
        # efficient.  We declare identity exhaustively: name, company,
        # team, reporting line, backstory, and goal.
        parts.append("\n# Your Identity")
        parts.append(f"You are **{self.role.name}** at **{self.org.name}**.")
        if unit:
            parts.append(f"You belong to the **{unit.name}** {unit.type}.")
            if unit.purpose:
                parts.append(f"Your {unit.type}'s purpose: {unit.purpose}")
            if unit.goals:
                parts.append(f"\nYour {unit.type}'s goals:")
                for goal in unit.goals:
                    parts.append(f"- {goal}")
        if self.role.backstory:
            parts.append(self.role.backstory)
        if self.role.goal:
            parts.append(f"\n**Your goal:** {self.role.goal}")

        parts.append(f"\n**Reports to:** {manager_str}")
        parts.append(f"**Direct reports:** {reports_str}")

        # Slack channel — if the unit has one, tell the agent.
        if unit and unit.slack_channel:
            parts.append(
                f"\n**Team Slack channel:** {unit.slack_channel}\n"
                "Use this channel for team discussions, questions, "
                "and decisions."
            )

        # ── Section 3: Responsibilities & Guidelines ──
        if self.role.responsibilities:
            parts.append("\n## Your Responsibilities")
            for r in self.role.responsibilities:
                parts.append(f"- {r}")

        if self.role.behavioral_guidelines:
            parts.append("\n## Behavioral Guidelines")
            for g in self.role.behavioral_guidelines:
                parts.append(f"- {g}")

        # ── Section 4: Company Policies ──
        # Policies are company-wide norms the founder defines.
        # They belong in the prompt — employees don't RAG-search
        # for basic company rules before every action.
        if self.org.policies:
            parts.append("\n## Company Policies")
            for policy in self.org.policies:
                parts.append(f"- {policy}")

        # ── Section 5: Event Triage ──
        # Knowledge workers use urgency/relevance frameworks to process
        # incoming events. This is platform-level cognitive scaffolding
        # — how the engine expects agents to evaluate work, not
        # company-specific culture.
        parts.append(
            "\n## How You Process Incoming Events"
            "\nWhen you receive a notification or message, evaluate "
            "it before acting:"
            "\n1. **Relevance:** Is this in my domain? Am I the "
            "right person for this?"
            "\n2. **Urgency:** Is someone blocked? Is there a "
            "deadline? Is this time-sensitive?"
            "\n3. **Action needed:** Do I need to act, or just be "
            "aware? If you were not actually asked (FYI, passing "
            "reference, addressee was someone else), skip silently. "
            "If you were asked but are declining (out of scope, "
            "wrong person, already handled), post a brief reply on "
            "the originating channel — silence on a direct ask "
            "looks like the message was lost."
            "\n4. **Level:** Should I handle this myself, delegate "
            "to a report, or ask my manager for help?"
            "\nDo not process events by arrival order — process "
            "by priority relative to your role."
        )

        # ── Section 6: Tool Usage ──
        parts.append(
            "\n## Tool Usage"
            "\nYou MUST use tool calls to perform actions. "
            "When you decide to take an action, you must call the "
            "appropriate tool. Writing about an action in your "
            "response does NOT execute it.\n"
            "For example, to reply to a message, you must "
            "call the appropriate reply tool for that channel. "
            'Saying "I replied" or '
            '"I sent a message" without executing a tool call '
            "does absolutely nothing.\n"
            "Act naturally in the manner of your role. Respond to "
            "messages the way a real person in your position would."
        )

        # ── Section 8: Knowledge System ──
        parts.append(
            "\n## Knowledge System"
            "\nRelevant team documentation is automatically surfaced "
            "with each task based on its content. If you need "
            "additional context — such as team goals to prioritize "
            "work, or a teammate's skills before delegating — search "
            "your team knowledge base for the relevant page."
            "\n\n### Delegation & Follow-up"
            "\nWhenever you hand off work or wait for external "
            "input — whether posting a question in chat, "
            "delegating coding work, creating a tracker "
            "ticket, or requesting a decision from your manager "
            "— **immediately** call ``reflect_and_persist`` "
            "(``ttl_days=30``) to capture:"
            "\n1. What you delegated or asked"
            "\n2. Where the request lives (the channel + thread, "
            "repo + PR, or issue key)"
            "\n3. What you need from the response and what you "
            "will do next"
            "\n\nWhen you later receive a notification (a thread "
            "reply, a PR review, an issue status "
            "change) that you don't immediately recognize, your "
            "``## Personal memory`` Plan-prompt block surfaces the "
            "matching diary entry automatically -- you don't need "
            "to query knowledge to look it up."
        )

        # Personal-memory guidance -- encourage agents to capture
        # useful context for their future selves via ``reflect_and_persist``.
        # Team-shared rules belong in docs (Confluence) where other
        # agents find them; this surface is private observations only.
        parts.append(
            "\nUse ``reflect_and_persist`` to capture durable facts "
            "for your own future reference -- declarative facts, "
            "not instructions to yourself. Pass ``ttl_days`` for "
            "ephemeral context (vacations, sprint focus, "
            "delegations awaiting reply); omit it for facts that "
            "should persist indefinitely."
            "\n\nFor team-shared procedural knowledge (runbooks, "
            "ADRs, conventions), edit the relevant page in your team "
            "knowledge base rather than capturing it locally -- that "
            "is where humans and other agents look for them."
        )

        # ── Section 9: Team Roster (leads only) ──
        if is_lead:
            parts.append("\n## Your Team")
            for report in reports:
                member_line = f"- {report.name}"
                if report.is_human:
                    member_line += f" ({report.get_handle()}) — human teammate"
                elif report.handle:
                    member_line += f" ({report.handle})"
                parts.append(member_line)
            parts.append(
                "\nTo learn about the specific skills, background, "
                "external usernames, and responsibilities of your team "
                "members before assigning tasks, search your team "
                "knowledge base for their profiles."
            )

        # ── Section 10: Human colleagues (mixed orgs only) ──
        parts.extend(build_human_colleagues_note(self))

        # Tool / MCP-server-specific guidance (GitHub delegation,
        # Atlassian/Slack mention markup, etc.) is sourced from the
        # registry-driven Tool Skills system, not hardcoded here.  The
        # property path leaves these slots empty; callers that want
        # skill scaffolding in the combined single-shot prompt should use
        # :meth:`build_system_prompt_with_skills` and pass a registry.

        self._system_prompt = "\n".join(parts)
        return self._system_prompt

    def build_system_prompt_with_skills(
        self,
        skill_registry: PromptSkillRegistry | None,
    ) -> str:
        """Combined single-shot prompt with optional tool-skill injection.

        Returns the same prose as :attr:`system_prompt`, plus any
        skills whose trigger fires against the role's MCP servers and
        whose ``phases`` include Plan.  ``skill_registry=None`` is
        equivalent to the bare property (no skill prose).

        This method exists for introspection / external callers that
        want the combined prompt to remain consistent with the
        Plan-phase prompt the live engine builds.  The main runtime
        path (:class:`~crewlet.agent.turn.TurnEngine`) does not call
        either of these; each phase builds its own prompt via the
        per-phase builders.
        """
        from crewlet.agent.skills import Phase as _Phase

        base = self.system_prompt
        if skill_registry is None:
            return base
        skills = skill_registry.skills_for(
            phase=_Phase.PLAN,
            available_tools=set(),
            mcp_servers=set(self.role.mcp_env.keys()),
        )
        if not skills:
            return base
        parts: list[str] = [base]
        for skill in skills:
            # Render ${var} references so this introspection path stays
            # consistent with the live per-phase prompts.
            parts.append(f"\n## {skill_registry.render(skill.title)}")
            parts.append(skill_registry.render(skill.body))
        return "\n".join(parts)


# ---------------------------------------------------------------------------
# Shared section builders
#
# The per-phase prompt builders in ``crewlet.agent.prompts`` reuse these
# section helpers so a single source of truth drives both the combined
# single-shot prompt (above) and the Plan / Execute / Review prompts.
# Each helper returns a list of lines (joined with ``\n`` by the caller)
# or an empty list when the section shouldn't appear.
# ---------------------------------------------------------------------------


def build_identity_section(definition: AgentDefinition) -> list[str]:
    """Professional identity for the Plan phase.

    Plan needs enough to decide *what* to do: role, company, unit,
    goal, manager, and direct reports (for delegation decisions).
    The long-form backstory / guidelines / team-goals render in the
    role-profile and unit-context sections of the Plan prompt.
    """
    from crewlet.org.hierarchy import (
        get_manager,
        get_reports,
        get_unit_for_role,
    )

    role = definition.role
    org = definition.org
    manager = get_manager(role, org)
    reports = get_reports(role, org)
    unit = get_unit_for_role(role, org)

    parts: list[str] = ["# Your Identity"]
    parts.append(f"You are **{role.name}** at **{org.name}**.")
    if unit:
        parts.append(f"You belong to the **{unit.name}** {unit.type}.")
    if role.goal:
        parts.append(f"**Your goal:** {role.goal}")
    manager_str = _seat_label(manager) if manager else "None (top-level)"
    reports_str = ", ".join(_seat_label(r) for r in reports) if reports else "None"
    parts.append(f"**Reports to:** {manager_str}")
    parts.append(f"**Direct reports:** {reports_str}")
    if unit and unit.slack_channel:
        parts.append(f"**Team Slack channel:** {unit.slack_channel}")
    return parts


def _seat_label(role: Role) -> str:
    """Render a colleague reference, marking human seats.

    The marker is load-bearing: it is how an agent knows its manager
    or report replies asynchronously over external surfaces rather
    than running turns (see ``build_human_colleagues_note``).
    """
    return f"{role.name} (human)" if role.is_human else role.name


def build_identity_line(definition: AgentDefinition) -> str:
    """Ultra-compact one-line identity for Execute/Review phases.

    Those phases already have the plan (and Execute's artifact, for
    Review) in the user message, so we don't need the full reporting
    line / unit context. A single sentence is enough to ground the
    LLM in "who is writing this".
    """
    from crewlet.org.hierarchy import get_manager

    role = definition.role
    org = definition.org
    manager = get_manager(role, org)
    manager_str = _seat_label(manager) if manager else "None (top-level)"
    return f"You are **{role.name}** at **{org.name}** (reports to: {manager_str})."


def build_roster_section(definition: AgentDefinition) -> list[str]:
    """Team roster.

    For non-leads (no direct reports): empty.

    For leads: names + handles + a compact per-member profile
    (backstory + goal + responsibilities) so the lead can reason about
    who to assign work to without a separate knowledge fetch.  Human
    members render with their external contact identities, availability,
    and how to hand them work (PM-tool assignment + mention — no engine
    task assignment).  Contact identities are rendered generically from
    the seat's
    :meth:`~crewlet.org.models.HumanContact.resolved_identities`
    enumerator (``${VAR}`` references env-resolved, unresolved ones
    omitted — never shown verbatim), so the renderer is not tied to
    any particular platform.
    Rendered directly from the in-memory
    :class:`~crewlet.org.models.Organization`.
    """
    from crewlet.org.hierarchy import get_reports

    reports = get_reports(definition.role, definition.org)
    if not reports:
        return []
    parts = ["\n## Your Team"]
    for report in reports:
        head = f"- **{report.name}**"
        if report.is_human:
            head += f" ({report.get_handle()}) — **human teammate**"
        elif report.handle:
            head += f" ({report.handle})"
        parts.append(head)
        if report.backstory:
            parts.append(f"  - Background: {report.backstory}")
        if report.goal:
            parts.append(f"  - Goal: {report.goal}")
        if report.responsibilities:
            parts.append("  - Responsibilities: " + "; ".join(report.responsibilities))
        if report.is_human:
            if report.contact is not None:
                seen_ids: set[str] = set()
                for transport, external_id in report.contact.resolved_identities():
                    # resolved_identities() can map several transports to one id
                    # (e.g. issue-tracker + wiki sharing one account id);
                    # show each id once, labelled by its first transport.
                    if external_id in seen_ids:
                        continue
                    seen_ids.add(external_id)
                    parts.append(f"  - {transport.capitalize()} ID: {external_id}")
            if report.availability:
                parts.append(f"  - Availability: {report.availability}")
            parts.append(
                "  - Working with them: hand work over in the PM tool "
                "and @-mention them on their team's chat or issue "
                "tracker; they reply asynchronously — don't expect an "
                "engine turn from them."
            )
    return parts


def build_human_colleagues_note(definition: AgentDefinition) -> list[str]:
    """Working-with-humans contract — present only in mixed orgs.

    A platform-level note, not role prose: it tells every agent how
    human seats behave (external surfaces, asynchronous replies, no
    A2A, no engine turns) so handoffs and mentions are shaped
    correctly on the first attempt.  Empty when the org has no human
    seats, keeping pure-agent prompts unchanged.
    """
    org = definition.org
    if org is None or not any(r.is_human for r in org.all_roles()):
        return []
    return [
        "\n## Human colleagues",
        "Some seats in this org are held by human teammates — marked "
        "**(human)** in your identity and roster, and "
        "`kind: human` in `lookup_colleague` results. When you need one "
        "of them:",
        "- Reach them where humans read: an @-mention on the team's "
        "chat, or a comment on the issue / doc where the work lives. "
        "They are NOT on the A2A bus.",
        "- They reply asynchronously (think hours, not seconds). Put "
        "the full context they need into your message, then finish "
        "your turn — their reply re-triggers you. Never wait or poll.",
        "- Hand them work through the PM tool (assign + mention), not "
        "through engine task assignment.",
    ]


def build_org_mission_vision_section(definition: AgentDefinition) -> list[str]:
    """Org-wide mission + vision (when set).

    Rendered directly from :class:`~crewlet.org.models.Organization`.
    Both fields are short by convention (1-2 sentences each); inlining
    is a token-cheap way to keep them in front of the planner without
    a knowledge round-trip.
    """
    org = definition.org
    if not (org.mission or org.vision):
        return []
    parts = ["\n## Company Context"]
    if org.mission:
        parts.append(f"**Mission:** {org.mission}")
    if org.vision:
        parts.append(f"**Vision:** {org.vision}")
    return parts


def build_policies_section(definition: AgentDefinition) -> list[str]:
    """Full company-policy text inline.

    Policies are short by convention -- a couple of sentences
    each -- so the full text is inlined rather than truncated
    one-liners, keeping the planner's context complete.

    Returns ``[]`` when the org has no policies configured.
    """
    if not definition.org.policies:
        return []
    parts = ["\n## Company policies"]
    for policy in definition.org.policies:
        parts.append(f"- {policy.strip()}")
    return parts


def build_role_profile_section(definition: AgentDefinition) -> list[str]:
    """Agent's own backstory + responsibilities + behavioral guidelines.

    Rendered directly from the in-memory
    :class:`~crewlet.org.models.Role`.  ``goal``
    is already covered by :func:`build_identity_section` so it
    isn't repeated here.
    """
    role = definition.role
    parts: list[str] = []
    if role.backstory:
        parts.append("\n## Your Background")
        parts.append(role.backstory)
    if role.responsibilities:
        parts.append("\n## Your Responsibilities")
        for item in role.responsibilities:
            parts.append(f"- {item}")
    if role.behavioral_guidelines:
        parts.append("\n## Behavioral Guidelines")
        for item in role.behavioral_guidelines:
            parts.append(f"- {item}")
    return parts


def build_unit_context_section(definition: AgentDefinition) -> list[str]:
    """Containing unit's purpose + goals, rendered inline.

    Returns ``[]`` for root-level roles (no containing unit) or
    when the unit has neither a ``purpose`` nor any ``goals``.
    """
    from crewlet.org.hierarchy import get_unit_for_role

    unit = get_unit_for_role(definition.role, definition.org)
    if unit is None:
        return []
    if not unit.purpose and not unit.goals:
        return []
    parts = [f"\n## Your Unit ({unit.name})"]
    if unit.purpose:
        parts.append(f"**Purpose:** {unit.purpose}")
    if unit.goals:
        parts.append("**Goals:**")
        for goal in unit.goals:
            parts.append(f"- {goal}")
    return parts


# NOTE: tool-specific guidance (GitHub delegation, Atlassian/Slack
# mention markup) is sourced from the Tool Skills registry, not built
# here.  Operators seed the content via ``crewlet confluence import``
# (bundled examples in ``examples/tool-skills/github.md`` and
# ``platform-mentions.md``); the runtime fetches via
# :class:`~crewlet.agent.skills.PromptSkillRegistry`.
