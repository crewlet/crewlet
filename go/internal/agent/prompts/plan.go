package prompts

import (
	"slices"
	"strings"
)

// PlanHeader is the Plan-phase contract.
//
// Carried verbatim; nearly every paragraph is a production failure that has
// already happened once:
//
//   - the tools_needed rule: a plan that listed only research tools left
//     Execute with data and no way to deliver it;
//   - the originating-channel rule: a Jira-create task triggered from chat
//     had no reply tool, so when Execute hit an unresolvable assignee it had
//     no way to ask and the requester got silence;
//   - the full-arc rule: routed comment triggers planned recon only, and the
//     turn ended with the model writing "I don't have write tools" as text;
//   - the skip rule: `skip` on a direct @mention reads to a human exactly
//     like the message never arriving.
const PlanHeader = "\n## PLAN phase" +
	"\nYou decide *what* Execute should do. Output one `submit_plan` call " +
	"matching the ExecutionPlan schema." +
	"\n" +
	"\n**Your `tools=[...]` starts with the meta-tools (`submit_plan`, " +
	"`activate_tool`, `list_mcp_server_tools`, `load_tool_skill`).** " +
	"Your catalogue lists builtin tools by name and MCP servers by name " +
	"only. To use a builtin tool here for in-Plan recon, call " +
	"`activate_tool(name=...)` — it promotes the tool into your " +
	"`tools=[...]`, so its schema appears on the next message and you " +
	"can call it directly. To use an MCP tool, first call " +
	"`list_mcp_server_tools(server=...)` to see what the server offers, " +
	"then `activate_tool(name=...)` to promote the chosen tool. Reserve " +
	"activation for read-only recon (reading threads, issues, or docs; " +
	"colleague lookups); action / write tools belong in `submit_plan`'s " +
	"`tools_needed` for Execute to run under Review." +
	"\n" +
	"\n**Your Plan-phase tool results are not forwarded to Execute.** If " +
	"you fetch data here, Execute will have to re-fetch it — OR put the " +
	"finished content into `steps[].approach` so Execute sees it " +
	"verbatim and calls the named tool with it." +
	"\n" +
	"\n**`tools_needed` must list EVERY tool Execute will call — " +
	"including the final delivery tool.** If the task will end by " +
	"replying on the channel the trigger arrived on, include that " +
	"channel's post/reply tool. Creating, updating, or transitioning " +
	"something? Include the relevant write tool. A plan that only lists " +
	"research tools is broken: Execute will gather data and have no way " +
	"to deliver the result. Rule of thumb: if your `success_criteria` " +
	"mention 'post', 'reply', 'notify', 'create', 'update', 'send', " +
	"'review', 'act', 'take action', 'assign', 'transition', 'respond', " +
	"'decline', or 'acknowledge', the corresponding action tool MUST be " +
	"in `tools_needed`." +
	"\n" +
	"\n**Plan the full arc, not just recon.** For tasks that need both " +
	"fetching AND acting (routed event triggers — \"review and respond\", " +
	"\"investigate and decide\"), list recon AND the likely action tools " +
	"(the reply, status-change, and post tools for the systems in play) " +
	"in `tools_needed` so Execute acts in one pass.  Review always runs " +
	"after Execute (engine-enforced for any executable plan) and can " +
	"``self_iterate`` with broader tools when you couldn't yet predict " +
	"the action — but that's a two-pass turn.  Prefer one pass: cost of " +
	"an unused action tool is zero." +
	"\n" +
	"\n**Always include the originating channel's reply tool — even when " +
	"the task's primary action is on a different system.** Whichever " +
	"transport delivered the trigger is how you stay in touch with the " +
	"requester.  Execute may need to: (a) ask follow-up questions when " +
	"info is missing or ambiguous (e.g. a referenced item that doesn't " +
	"resolve, an assignee whose handle isn't a valid account id), (b) " +
	"confirm completion with a short status, (c) report partial " +
	"failures.  Without the reply tool the requester gets silence and " +
	"Execute hits a wall.  This is in addition to whatever action tools " +
	"the task itself requires." +
	"\n" +
	"\n**`decision=\"skip\"` means \"nobody was actually asking me to do " +
	"anything\" — not \"I'm ignoring a direct ping\".** Use `skip` for " +
	"informational triggers, passing references, and messages addressed " +
	"to someone else.  When you were directly asked / @mentioned / " +
	"assigned but are declining (out of scope, wrong owner, already " +
	"handled), do NOT use `skip`; emit `decision=\"plan\"` with one step " +
	"posting a brief decline via the originating channel's reply tool — " +
	"name the right owner or point at where the work lives.  Silence on " +
	"a direct ping looks like the message was lost; a one-liner closes " +
	"the loop." +
	"\n" +
	"\nMission, vision, policies, your role profile, your unit context, " +
	"and your team roster are already in this prompt -- no lookup " +
	"needed."

// planKnowledgeNote points the planner at the prefetched documentation.
//
// APPENDED ONLY WHEN THERE IS A BLOCK. It used to be part of the header and
// therefore unconditional, which made it a false statement on every turn the
// search was gated off, found nothing, or had no backend to run against — and
// a planner told the documentation is already here is a planner that does not
// go looking for it.
const planKnowledgeNote = "  Relevant team documentation was surfaced in the " +
	"`## Relevant knowledge` block below."

// PlanHeaderModelSplit is appended when Execute runs on a cheaper model than
// Plan, so the planner writes a plan that does not need planner-level
// judgement at each step.
const PlanHeaderModelSplit = "\nExecute runs on a cheaper model — make the plan rich enough that " +
	"it does not need planner-level judgement at each step."

// planSandboxSection is rendered only for sandbox-enabled roles.
//
// Gated because it is absent for very nearly every role: unconditional, it
// would be ~230 tokens of prose about a tool the seat does not have, on every
// Plan prompt in the company.
const planSandboxSection = "\n## Sandbox code work (the `run_sandbox` tool)" +
	"\nFor **real code work** — implement or modify code, run tests, " +
	"reproduce a bug, run a script — Execute has a `run_sandbox` tool: a " +
	"real computer with a shell and a git checkout where a coding agent " +
	"(Claude Code / OpenCode) works autonomously on a brief you give it " +
	"and returns a structured report." +
	"\n" +
	"\nPlan the **full arc** in one plan: list `run_sandbox` in " +
	"`tools_needed` **and** the tool you'll use to report / act on the " +
	"result (e.g. the originating channel's reply tool). The sandbox " +
	"runs detached — Execute pauses and resumes automatically with the " +
	"result, keeping full context — so after `run_sandbox` returns, " +
	"Execute reports the outcome (or opens / links the PR, files a " +
	"ticket, etc.) with those tools in the SAME turn. Don't split it " +
	"into a separate report turn." +
	"\n" +
	"\nKeep everything else native: reply on Slack, update a ticket, " +
	"read docs, answer a question — those need no sandbox."

// PlanInput is everything the Plan prompt renders beyond the seat itself.
//
// Every field is frozen at turn start and none of them varies as the Plan
// loop runs: that is what makes the prompt byte-identical on round 9 and
// round 1, which is what the provider's prefix cache is priced on. Work that
// accumulates DURING the phase belongs in [BuildPhaseUserMessage].
//
// The zero value renders the plain contract with no catalogue and no
// prefetch blocks, which is what the combined single-shot path and most tests
// want.
type PlanInput struct {
	// ToolCatalogue is the rendered slim catalogue: builtin tools by name
	// with a one-line description, MCP servers by name only.
	ToolCatalogue string

	// AvailableTools is the tool surface registered for this plan. It
	// scopes skill matching, and gates the onboarding hint — see
	// OnboardingHint.
	AvailableTools []string

	// ModelSplitEnabled appends [PlanHeaderModelSplit].
	ModelSplitEnabled bool

	// CounterpartyProfile is the observed-traits block for the turn's
	// triggering counterparty, pre-rendered and trimmed by the profiler.
	CounterpartyProfile string

	// SynthesizedSkills is the block of skills this agent has learned for
	// itself (distinct from the operator-authored tool skills in Skills).
	SynthesizedSkills string

	// EpisodeRecall is episodic recall, frozen at turn start rather than
	// re-queried mid-turn precisely to preserve prefix caching.
	EpisodeRecall string

	// OnboardingHint renders only when this agent has not yet completed
	// onboarding for its current org chain AND mark_onboarded is in
	// AvailableTools — instructing an agent to call a tool that is not
	// registered (reflection disabled) is worse than saying nothing.
	OnboardingHint string

	// PersonalMemory is the agent-scope diary block, already filtered for
	// relevance to this task. Empty when the filter is unavailable, fails,
	// or finds no candidates.
	PersonalMemory string

	// RelevantKnowledge is the team knowledge-base prefetch: pages found by
	// a query-time search built from the trigger, frozen at turn start.
	RelevantKnowledge string

	// Skills is the tool-skill registry. Nil keeps the prompt free of skill
	// scaffolding entirely.
	Skills SkillCatalogue
}

// BuildPlan renders the Plan-phase system prompt.
//
// The planner gets the whole static picture inline — identity, mission,
// policies, unit, roster — because an employee does not look up what their
// company does before every decision, and a knowledge round-trip per turn
// costs more than the tokens it saves.
func BuildPlan(seat Seat, in PlanInput) string {
	body := joinSections(
		BuildIdentitySection(seat),
		BuildOrgMissionVisionSection(seat),
		BuildRoleProfileSection(seat),
		BuildUnitContextSection(seat),
		BuildPoliciesSection(seat),
		BuildRosterSection(seat),
		BuildHumanColleaguesNote(seat),
	)

	header := PlanHeader
	if in.RelevantKnowledge != "" {
		header += planKnowledgeNote
	}
	if in.ModelSplitEnabled {
		header += PlanHeaderModelSplit
	}
	parts := []string{body, header}

	if seat.ok() && seat.Role.Sandbox != nil && seat.Role.Sandbox.Enabled {
		parts = append(parts, planSandboxSection)
	}

	// The onboarding hint gates on the tool as well as the marker: the
	// same rule the memory / skill blocks follow, so a prompt never tells
	// the model to call something that is not registered.
	if in.OnboardingHint != "" && slices.Contains(in.AvailableTools, "mark_onboarded") {
		parts = append(parts, "\n## First-turn onboarding", in.OnboardingHint)
	}
	if in.PersonalMemory != "" {
		parts = append(parts, "\n## Personal memory", in.PersonalMemory)
	}
	if in.SynthesizedSkills != "" {
		parts = append(parts, "\n## Synthesized skills you've learned", in.SynthesizedSkills)
	}
	if in.RelevantKnowledge != "" {
		parts = append(parts, "\n## Relevant knowledge", in.RelevantKnowledge)
	}
	if in.EpisodeRecall != "" {
		parts = append(parts, "\n## Similar prior work", in.EpisodeRecall)
	}
	if in.CounterpartyProfile != "" {
		parts = append(parts, "\n## Known counterparty", in.CounterpartyProfile)
	}

	// The tool-skills catalogue lands adjacent to the tool catalogue so the
	// planner reads "here is how to use these tools" immediately before
	// "here are the tools" — the two are conceptually one section.
	parts = injectSkillCatalogue(parts, in.Skills, PhasePlan, Surface{
		Tools:      in.AvailableTools,
		MCPServers: seat.mcpServers(),
	})
	if in.ToolCatalogue != "" {
		parts = append(parts, "\n## Available tools", in.ToolCatalogue)
	}
	return strings.Join(parts, "\n")
}
