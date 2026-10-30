package prompts

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/provenance"
)

// ExecutorHeader is the executor's contract: decide and act in one pass, then
// account for it.
//
// It states only what the model cannot learn from anything else it is sent,
// and each rule once. Every sentence left is a production failure that has
// already happened:
//
//   - "writing about an action does not perform it" — a turn that ended with
//     a composed reply nobody was sent. It stays here, not only in
//     `submit_work`'s description, because in text mode the tool protocol is
//     the engine's own envelope and no CLI harness can teach it;
//   - the discovery sentence — an actor that gave up when the tool it needed
//     was not already on its surface;
//   - the full-arc rule — routed comment triggers produced recon only, and
//     the turn ended with the model writing "I don't have write tools";
//   - the originating-channel rule — a tracker-create task triggered from
//     chat hit an unresolvable assignee and the requester got silence; and a
//     founder's direct message was "answered" with a work item they never
//     heard about.
//
// What is NOT here is everything another channel already carries. What an
// outcome means and that `deliveries` are checked is the submission schema's
// to say (runner/schema.go), beside the fields it describes, and the decoder
// enforces both; how discovery works is the two discovery tools' own
// descriptions; the mission, policies and roster need no sentence saying they
// are present, because they are. A second copy of a rule is not emphasis: it
// is two texts that drift, and the model reads whichever it saw last.
const ExecutorHeader = "\n## Your turn" +
	"\nDecide what to do about the task below and do it, in this one pass. " +
	"Use tool calls for every action — writing about an action does not " +
	"perform it. End by calling `submit_work` exactly once, reporting what " +
	"you actually did." +
	"\n" +
	"\n**Finish the arc in this pass:** read, decide, then call the tool " +
	"that delivers — the reply, the comment, the status change, the create. " +
	"A turn that gathers data and stops has delivered nothing. If a tool you " +
	"need is not among your tools, discover it (`list_mcp_server_tools`, then " +
	"`activate_tool`) before reporting that you cannot act." +
	"\n" +
	"\n**Stay reachable on the channel the trigger arrived on**, even when " +
	"the work lives on another system: filing the ticket is the work, and " +
	"the requester still has to be told. Ask a follow-up when something is " +
	"missing, confirm completion, report a partial failure, or decline in " +
	"one line through the originating channel's reply tool, naming the right " +
	"owner. Without a reply the requester gets silence."

// untrustedTextHeading titles the standing sentence about fenced text.
const untrustedTextHeading = "\n## Text somebody else wrote"

// KeyUntrustedText is the outline key of that section.
const KeyUntrustedText = "untrusted_text"

// executorSandboxSection is rendered only where the executor is offered
// run_sandbox ([ExecutorInput.OffersSandbox]).
//
// Gated because it is absent for very nearly every role: unconditional, it
// would be prose about a tool the seat does not have, on every executor prompt
// in the company. What the tool does and takes is its own description; what
// this adds is the one fact about the TURN — it pauses for the run and resumes
// with its report — which is what stops a seat ending the turn to "report
// later" and leaving the requester waiting on a run nobody follows up.
const executorSandboxSection = "\n## Sandbox code work" +
	"\nReal code work — changing code, running tests, reproducing a bug — " +
	"goes to `run_sandbox`. This turn pauses while the run works and resumes " +
	"with its report as the call's result, so act on it in the SAME turn: " +
	"report the outcome, link the pull request, update the ticket."

// KeyCodeHistory is the outline key of the guidance on answering questions
// about the company's own code.
const KeyCodeHistory = "code_history"

// CodeHistoryTools are the code tools [codeHistorySection]'s claims rest on:
// what a file says, what changed, who last changed a line, how a file's
// history went and whether a commit reached a branch, read by the commit ids a
// ref resolves to — and a change's diff, its findings and what changed since a
// review, read by the change. The section renders only where the executor
// holds EVERY one of them, so a prompt never sends a seat to a tool its
// surface does not carry; they are registered together where a company hosts
// code, so a seat holds all of them or none. Exported for the test in
// internal/agent/builtin that holds them to the code tools that package
// registers, which this package cannot import.
func CodeHistoryTools() []string {
	return []string{"code_resolve", "code_read", "code_diff", "code_compare", "code_landed_in", "code_log",
		"code_blame", "code_get_change"}
}

// codeHistorySection is the one rule about reading the company's code that no
// tool's description can carry: that such a question is not one for a clone
// at all. Each code tool's description says what it answers and how to call
// it — `code_resolve`'s that a branch is resolved once and its commit id
// passed on — so none of that is repeated here; what is left is the choice a
// seat makes BEFORE it picks a tool, between a server-side read by commit id,
// which costs milliseconds, and a clone in a sandbox or the shell a coding CLI
// holds, which costs minutes and the company's git nodes their transfer lane.
//
// The tools by their family's name rather than one by one, so the sentence
// stays true as later reads join the family — but for the one read a review
// is done with, named because the choice it settles is the same one: a
// reviewer that re-reads a change by checking it out and walking its history
// in a box spends the box's minutes on a question the server answers by the
// change, the revisions its rows record, and the findings placed in them.
const codeHistorySection = "\n## Reading the company's code" +
	"\nWhat a file says, what changed, who last changed a line, how a file's " +
	"history went, and whether a commit reached a branch are read on the " +
	"server with the `code_*` tools, by commit id — never from a clone, which " +
	"costs minutes for one file. A change's diff, its findings and what changed " +
	"since your review are read with `code_get_change`."

// codeHistoryBoxSentence closes [codeHistorySection] where the executor is
// also offered run_sandbox ([ExecutorInput.OffersSandbox]), which then takes
// `repos` — the code tools and `repos` are offered together, where this node
// serves the code host — because a repository a coding run is given is cloned
// BLOBLESS: a `git log -p` or a `git blame` in the box fetches each file's
// contents over the network as it walks, so the history question a seat might
// hand the box is the one the server answers fastest: history in a box is
// for building, not for answering questions. Said only there: to a seat with
// no sandbox it is prose about a box it never gets.
const codeHistoryBoxSentence = " A repository `run_sandbox` clones is blobless — each " +
	"file's contents fetched as git needs them — so its history is for building, not for answering questions."

// KeyCodeWrites is the outline key of the guidance on changing the company's
// own code.
const KeyCodeWrites = "code_writes"

// CodeWriteTools are the code tools [codeWritesSection]'s claims rest on: a
// small change committed on the server, and a scratch repository of the
// seat's own. The section renders only where the executor holds EVERY one of
// them; the code host's writes are registered together, where a company hosts
// code and its writes are served, so a seat holds all of them or none.
// Exported for the test in internal/agent/builtin that holds them to the code
// writes that package registers, which this package cannot import.
func CodeWriteTools() []string {
	return []string{"code_commit_files", "code_create_repo"}
}

// codeWritesSection is the one rule about CHANGING the company's code that no
// tool's description can carry, for [codeHistorySection]'s reason: the choice
// a seat makes before it picks a tool — between a change the code host builds
// from a call, which costs a push and no checkout, and a checkout a coding run
// or the seat's own shell builds and tests in, which costs minutes. How each
// write is called (`expected_head`, an edit's exact match, a conflict's
// stages) is its description's, and is not repeated here; nor is the scratch
// repository's expiry, which code_create_repo's description says.
const codeWritesSection = "\n## Changing the company's code" +
	"\nA small change to a repository — a document, a configuration, a fix of a " +
	"few lines — is committed on the server with `code_commit_files`; anything to " +
	"build or test belongs in a checkout. Your experiments go in a scratch " +
	"repository, `<your handle>.<name>`."

// ExecutorInput is everything the executor prompt renders beyond the seat
// itself.
//
// Every field is frozen at turn start and none of them varies as the loop
// runs: that is what makes the prompt byte-identical on round 9 and round 1,
// which is what the provider's prefix cache is priced on. Work that
// accumulates DURING the turn belongs in [BuildPhaseUserMessage].
//
// The zero value renders the plain contract with nothing to discover and no
// prefetch blocks, which is what most tests want.
type ExecutorInput struct {
	// Discoverable is what the executor can reach but does not hold yet,
	// rendered by [github.com/crewlet/crewlet/internal/tools.Snapshot.Discoverable]:
	// on the executor, which holds every first-party tool from its first
	// round, that is its MCP servers by name. Empty renders no section.
	Discoverable string

	// AvailableTools is every name on the surface built for this phase,
	// the tools the phase adds to it included (`submit_work`, the
	// discovery pair, `delegate`) — never the registry snapshot it was
	// built from, which holds none of those. It scopes skill matching,
	// and gates every section that tells the model to call a tool
	// (escalation, workers).
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

	// PersonalMemory is the agent-scope diary block, already filtered for
	// relevance to this task. Empty when the filter is unavailable, fails,
	// or finds no candidates.
	PersonalMemory string

	// RelevantKnowledge is the team knowledge-base prefetch: pages found by
	// a query-time search built from the trigger, frozen at turn start.
	RelevantKnowledge string

	// ThreadContext is the chat thread this turn was woken in, read at
	// turn start and handed over rather than left for the agent to fetch.
	// Empty for every trigger that is not a chat thread reply.
	ThreadContext string

	// Skills is the tool-skill registry. Nil keeps the prompt free of skill
	// scaffolding entirely.
	Skills SkillCatalogue

	// OffersSandbox is whether run_sandbox is on the surface this executor
	// is offered, which is what the sandbox section teaches.
	//
	// THE CALLER'S ANSWER, not the role's gate read here: the gate is not
	// what says a seat holds the tool. A seat whose executor runs in agent
	// mode is never offered it — it already holds a shell — whatever its
	// gate says, and the section rendered off the gate told that seat's
	// coding CLI about a tool its bridge never lists, on every turn.
	OffersSandbox bool
}

// BuildExecutor renders the executor's system prompt, and its outline.
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
//
// Every block is its own section of the outline, under a key that does not
// move between prompts. The prefetched blocks are keyed on the
// [github.com/crewlet/crewlet/internal/events/types.PrefetchSummary] fields
// that measured each one (`thread_context`, `personal_memory`, …), so a reader
// can put a block beside what the prefetch recorded about it.
func BuildExecutor(seat Seat, in ExecutorInput) Prompt {
	// ONE counter for BOTH renderings of this seat's direct reports — the
	// identity line's name list and the roster's profiles. Planned once,
	// here, because the two sections are two views of one team and a cap
	// each would let them disagree about who is on it while the prompt paid
	// for both budgets. See [planRoster].
	reports := planRoster(seat, newRosterAllowance(rosterAllowanceTokens))

	b := NewBuilder("\n")
	identity := []struct {
		key   string
		lines []string
	}{
		{"identity", BuildIdentitySection(seat, reports)},
		{"company_context", BuildOrgMissionVisionSection(seat)},
		{"background", buildBackgroundSection(seat)},
		{"responsibilities", buildResponsibilitiesSection(seat)},
		{"behavioral_guidelines", buildGuidelinesSection(seat)},
		{"unit", BuildUnitContextSection(seat)},
		{"policies", BuildPoliciesSection(seat)},
		{"team", BuildRosterSection(seat, reports)},
		{"human_colleagues", BuildHumanColleaguesNote(seat)},
	}
	empty := true
	for _, section := range identity {
		b.Heading(section.key, section.lines...)
		empty = empty && len(section.lines) == 0
	}
	if empty {
		// THE EMPTY BODY STILL TAKES ITS PLACE. The prompt has always been
		// the identity body and the contract joined by a newline, so a seat
		// that renders no identity at all opens on a blank line — kept,
		// because the outline never changes a byte of the text.
		b.Add("")
	}

	b.Heading("your_turn", ExecutorHeader)
	// RIGHT AFTER THE CONTRACT, because it is part of how a turn ends:
	// the header says finish the arc, and this says the one case where
	// finishing means stopping on a branch until somebody answers.
	b.Heading("escalation", BuildEscalationSection(in.AvailableTools)...)
	// AND HOW TO READ WHAT OTHER PEOPLE WROTE, before any of it: the thread
	// below, every page, comment and file a tool answers, and anything an
	// MCP tool hands back is somebody else's words, and the fences round
	// them mean something only if the seat was told what. UNCONDITIONAL and
	// constant ([provenance.StandingInstruction]): the turn's token lives in
	// the fences, so this section is the same bytes on every turn of every
	// seat and costs the prompt cache nothing.
	b.Heading(KeyUntrustedText, untrustedTextHeading, provenance.StandingInstruction)

	if in.OffersSandbox {
		b.Heading("sandbox", executorSandboxSection)
	}

	// FIRST of the prefetched blocks, because it is the trigger's own
	// context rather than something retrieved about it, and the executor
	// reads this prompt top-down: what the conversation is before what the
	// seat remembers about conversations like it.
	//
	// "The thread so far" rather than "Thread context", which is what the
	// chat prompt titles the section it writes into the notification BODY
	// — and that lands in the USER message of this same conversation. Two
	// sections under one heading saying different things is a model reading
	// whichever it saw last.
	if in.ThreadContext != "" {
		b.Heading(KeyThreadContext, "\n## The thread so far", in.ThreadContext)
	}
	if in.PersonalMemory != "" {
		b.Heading(KeyPersonalMemory, "\n## Personal memory", in.PersonalMemory)
	}
	if in.SynthesizedSkills != "" {
		b.Heading(KeySynthesizedSkills, "\n## Synthesized skills you've learned", in.SynthesizedSkills)
	}
	if in.RelevantKnowledge != "" {
		b.Heading(KeyRelevantKnowledge, "\n## Relevant knowledge", in.RelevantKnowledge)
	}
	if in.EpisodeRecall != "" {
		b.Heading(KeyEpisodeRecall, "\n## Similar prior work", in.EpisodeRecall)
	}
	if in.CounterpartyProfile != "" {
		b.Heading(KeyCounterparty, "\n## Known counterparty", in.CounterpartyProfile)
	}
	// BEFORE the tool sections, because choosing a worker is a decision
	// about how to do the work rather than about which tool to call, and
	// the executor reads this prompt top-down: what it is, what it knows,
	// who it can hand work to, then what it can call itself.
	//
	// THE LIST ALONE: what a worker is, what it may not do and that it
	// answers inside this turn is `delegate`'s own description, which
	// reaches the model with the tool on every round.
	if in.Workers != "" && slices.Contains(in.AvailableTools, "delegate") {
		b.Heading("workers", "\n## Your workers", in.Workers)
	}
	// AFTER THE WORKERS AND BEFORE THE TOOL SKILLS, because it is a choice
	// of how to answer — on the server, not in a clone — made before the
	// how-to of any one tool. Gated on every tool its claims rest on.
	if holdsAll(in.AvailableTools, CodeHistoryTools()) {
		section := []string{codeHistorySection}
		if in.OffersSandbox {
			section = append(section, codeHistoryBoxSentence)
		}
		b.Heading(KeyCodeHistory, section...)
	}
	// AND HOW TO CHANGE IT, right after: the same choice made for a write.
	if holdsAll(in.AvailableTools, CodeWriteTools()) {
		b.Heading(KeyCodeWrites, codeWritesSection)
	}

	// The tool-skills catalogue lands adjacent to the tools to discover so
	// the executor reads "here is how to use these tools" immediately before
	// "here is what else you can reach" — the two are conceptually one
	// section.
	b.Heading("tool_skills", skillCatalogue(in.Skills, PhaseExecute, Surface{
		Tools:      in.AvailableTools,
		MCPServers: seat.mcpServers(),
	})...)
	b.Heading(KeyDiscoverableTools, discoverableSection(in.Discoverable)...)
	return b.Build()
}

// holdsAll reports whether surface holds every one of names.
func holdsAll(surface, names []string) bool {
	for _, name := range names {
		if !slices.Contains(surface, name) {
			return false
		}
	}
	return true
}

// KeyDiscoverableTools is the outline key of the tools a phase can reach but
// does not hold yet.
const KeyDiscoverableTools = "discoverable_tools"

// discoverableSection is the heading and the list of what a phase can still
// discover, or nil when there is nothing — a phase that holds everything it
// can reach has no section, rather than a heading over "(none)".
//
// NOT "Available tools", which is what it was titled while it listed every
// tool the phase already held. That is the heading a text-mode prompt gives
// the JSON definitions of the tools the phase DOES hold, in the same
// conversation, and two sections under one heading saying different things
// is a model reading whichever it saw last.
func discoverableSection(list string) []string {
	if strings.TrimSpace(list) == "" {
		return nil
	}
	return []string{"\n## Tools to discover", list}
}

// The keys of the executor's PREFETCHED blocks: the prefixes of the
// [github.com/crewlet/crewlet/internal/events/types.PrefetchSummary] fields
// that measure each block (`thread_context_hit`, `thread_context_bytes`, …).
// One name for one block across the two records, so a reader joins them
// without a table — and a test reads the summary's own tags and holds these to
// them, rather than a second list here drifting from it.
const (
	KeyThreadContext     = "thread_context"
	KeyPersonalMemory    = "personal_memory"
	KeySynthesizedSkills = "synthesized_skills"
	KeyRelevantKnowledge = "relevant_knowledge"
	KeyEpisodeRecall     = "episode_recall"
	KeyCounterparty      = "counterparty"
)
