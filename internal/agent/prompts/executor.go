package prompts

import (
	"slices"
	"strings"
)

// ExecutorHeader is the executor's contract: decide and act in one pass, then
// account for it.
//
// Carried verbatim from the two headers it replaces, because nearly every
// paragraph is a production failure that has already happened once:
//
//   - "writing about an action does not perform it" — a turn that ended with
//     a composed reply nobody was sent;
//   - the discovery paragraph — an actor that gave up when the tool it needed
//     was not already on its surface;
//   - the originating-channel rule — a Jira-create task triggered from chat
//     hit an unresolvable assignee and the requester got silence;
//   - the full-arc rule — routed comment triggers produced recon only, and
//     the turn ended with the model writing "I don't have write tools";
//   - the no_action rule — the old `skip`, which on a direct @mention reads
//     to a human exactly like the message never arriving.
//
// What is NOT carried is everything the two-phase split needed and one loop
// does not: naming tools in advance against a catalogue the namer was never
// shown, and the reconciliation of those names with reality. The executor
// discovers what it needs when it turns out to need it.
const ExecutorHeader = "\n## Your turn" +
	"\nYou decide what to do about the task below and you do it, in this " +
	"one pass. Use tool calls for every action — writing about an action " +
	"does not perform it. End by calling `submit_work` exactly once, " +
	"reporting what you actually did." +
	"\n" +
	"\n**Your `tools=[...]` starts with your first-party tools and the " +
	"discovery meta-tools (`activate_tool`, `list_mcp_server_tools`).** " +
	"Everything your team's MCP servers publish is reachable but not yet " +
	"loaded: the catalogue below lists those servers by NAME only. Call " +
	"`list_mcp_server_tools(server=...)` to see what a server offers, " +
	"then `activate_tool(name=...)` to promote the one you want — its " +
	"schema appears on the next message and you call it directly. " +
	"Prefer discovery over giving up: only report that you cannot act if " +
	"discovery fails or the tool genuinely does not exist." +
	"\n" +
	"\n**Finish the arc in this pass.** Recon and action belong to the " +
	"same turn: read the thread, decide, and then call the tool that " +
	"delivers — the reply, the comment, the status change, the create. A " +
	"turn that gathers data and stops has delivered nothing. A reviewer " +
	"runs after you and can send the turn back for another round, but " +
	"that is a second pass over work one pass should have finished." +
	"\n" +
	"\n**Stay reachable on the channel the trigger arrived on — even when " +
	"the work itself is on a different system.** Whichever transport " +
	"delivered this task is how you stay in touch with the requester. You " +
	"may need to: (a) ask a follow-up when something is missing or " +
	"ambiguous (a referenced item that doesn't resolve, an assignee whose " +
	"handle isn't a valid account id), (b) confirm completion with a " +
	"short status, (c) report a partial failure. Without a reply the " +
	"requester gets silence." +
	"\n" +
	"\n**`outcome=\"no_action\"` means \"nobody was actually asking me to " +
	"do anything\" — not \"I'm ignoring a direct ping\".** Use it for " +
	"informational triggers, passing references, and messages addressed " +
	"to someone else. When you were directly asked / @mentioned / " +
	"assigned but are declining (out of scope, wrong owner, already " +
	"handled), do NOT use `no_action`: post a brief decline via the " +
	"originating channel's reply tool — name the right owner or point at " +
	"where the work lives — and report that as `delivered`. Silence on a " +
	"direct ping looks like the message was lost; a one-liner closes the " +
	"loop." +
	"\n" +
	"\n**`deliveries` must name calls you actually made.** The engine " +
	"checks them against its own log of this turn, and a claim it cannot " +
	"find comes straight back to you." +
	"\n" +
	"\nMission, vision, policies, your role profile, your unit context, " +
	"and your team roster are already in this prompt -- no lookup " +
	"needed."

// executorKnowledgeNote points the executor at the prefetched documentation.
//
// APPENDED ONLY WHEN THERE IS A BLOCK. It used to be part of the header and
// therefore unconditional, which made it a false statement on every turn the
// search was gated off, found nothing, or had no backend to run against — and
// an agent told the documentation is already here is one that does not go
// looking for it.
const executorKnowledgeNote = "  Relevant team documentation was surfaced in the " +
	"`## Relevant knowledge` block below."

// executorSandboxSection is rendered only for sandbox-enabled roles.
//
// Gated because it is absent for very nearly every role: unconditional, it
// would be ~200 tokens of prose about a tool the seat does not have, on every
// executor prompt in the company.
const executorSandboxSection = "\n## Sandbox code work (the `run_sandbox` tool)" +
	"\nFor **real code work** — implement or modify code, run tests, " +
	"reproduce a bug, run a script — you have a `run_sandbox` tool: a " +
	"real computer with a shell and a git checkout where a coding agent " +
	"works autonomously on a brief you give it and returns a structured " +
	"report." +
	"\n" +
	"\nThe sandbox runs detached: this turn pauses and resumes " +
	"automatically with the result, keeping full context. So after " +
	"`run_sandbox` returns, report the outcome — or open / link the PR, " +
	"file a ticket, reply on the channel — in the SAME turn. Don't leave " +
	"it for a separate report turn." +
	"\n" +
	"\nKeep everything else native: reply on Slack, update a ticket, " +
	"read docs, answer a question — those need no sandbox."

// ExecutorInput is everything the executor prompt renders beyond the seat
// itself.
//
// Every field is frozen at turn start and none of them varies as the loop
// runs: that is what makes the prompt byte-identical on round 9 and round 1,
// which is what the provider's prefix cache is priced on. Work that
// accumulates DURING the turn belongs in [BuildPhaseUserMessage].
//
// The zero value renders the plain contract with no catalogue and no
// prefetch blocks, which is what most tests want.
type ExecutorInput struct {
	// ToolCatalogue is the rendered slim catalogue: builtin tools by name
	// with a one-line description, MCP servers by name only.
	ToolCatalogue string

	// AvailableTools is the tool surface registered for this turn. It
	// scopes skill matching, and gates the onboarding hint — see
	// OnboardingHint.
	AvailableTools []string

	// Workers is the rendered `## Your workers` block: one line per
	// delegate template this seat may name, with what it is for and what
	// it returns.
	//
	// GATED on `delegate` actually being on the surface, below: a seat
	// told about workers it cannot reach spends a round discovering the
	// tool does not exist, and reads the refusal as a permission decision
	// nobody made.
	Workers string

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

// workersPreamble frames the list that follows it.
//
// Two sentences, and both are load-bearing. The first says a worker is a
// PARALLEL leaf rather than a colleague, which is the confusion that sends an
// executor to delegate something it should have asked a teammate. The second
// says the answer comes back in this turn, which is what stops it treating a
// delegation as fire-and-forget and ending the turn before reading anything.
const workersPreamble = "Short-lived workers you can hand narrowly-scoped work " +
	"to with `delegate`. Each one runs its own tool loop with a slice of your " +
	"tools, cannot write anywhere, and reports back to you inside this turn — " +
	"so you still finish the job yourself. They are not colleagues: work that " +
	"belongs to another seat goes to that seat."

// BuildExecutor renders the executor's system prompt.
//
// The executor gets the whole static picture inline — identity, mission,
// policies, unit, roster — because an employee does not look up what their
// company does before every decision, and a knowledge round-trip per turn
// costs more than the tokens it saves.
//
// It is one prompt where there were two. The planner used to get the identity
// scaffold and the actor got a one-line identity plus a plan, which meant the
// frame that decided WHAT to do was thrown away before anything was done: the
// actor met an unforeseen choice with no policy, no roster and no mission to
// make it against.
func BuildExecutor(seat Seat, in ExecutorInput) string {
	body := joinSections(
		BuildIdentitySection(seat),
		BuildOrgMissionVisionSection(seat),
		BuildRoleProfileSection(seat),
		BuildUnitContextSection(seat),
		BuildPoliciesSection(seat),
		BuildRosterSection(seat),
		BuildHumanColleaguesNote(seat),
	)

	header := ExecutorHeader
	if in.RelevantKnowledge != "" {
		header += executorKnowledgeNote
	}
	parts := []string{body, header}

	if seat.ok() && seat.Role.Sandbox != nil && seat.Role.Sandbox.Enabled {
		parts = append(parts, executorSandboxSection)
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
	// BEFORE the tool sections, because choosing a worker is a decision
	// about how to do the work rather than about which tool to call, and
	// the executor reads this prompt top-down: what it is, what it knows,
	// who it can hand work to, then what it can call itself.
	if in.Workers != "" && slices.Contains(in.AvailableTools, "delegate") {
		parts = append(parts, "\n## Your workers", workersPreamble, in.Workers)
	}

	// The tool-skills catalogue lands adjacent to the tool catalogue so the
	// executor reads "here is how to use these tools" immediately before
	// "here are the tools" — the two are conceptually one section.
	parts = injectSkillCatalogue(parts, in.Skills, PhaseExecute, Surface{
		Tools:      in.AvailableTools,
		MCPServers: seat.mcpServers(),
	})
	if in.ToolCatalogue != "" {
		parts = append(parts, "\n## Available tools", in.ToolCatalogue)
	}
	return strings.Join(parts, "\n")
}
