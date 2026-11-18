package prompts

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/toolname"
)

// ExecutorHeader is the executor's contract: decide and act in one pass, then
// account for it. Each sentence answers a failure seen (package doc, "Prompt
// text is behaviour"). "Writing about an action does not perform it": a
// composed reply nobody was sent, since the harness teaches that its final
// text is the answer. The search sentence: a harness shows a large server's
// tools only once its model looks, and an actor gave up on a tool not in
// front of it. The full arc: comment triggers ended in recon and "I don't have
// write tools". The originating channel: a requester got silence, or a work
// item they never heard of. What an outcome means is the submission schema's
// (runner/schema.go).
const ExecutorHeader = "\n## Your turn" +
	"\nDecide what to do about the task below and do it, in this one pass. " +
	"Use tool calls for every action — writing about an action does not " +
	"perform it. End by calling `" + toolname.SubmitWork + "` exactly once, reporting what " +
	"you actually did, unless you park on a person's answer as your standing " +
	"instructions describe." +
	"\n" +
	"\n**Finish the arc in this pass:** read, decide, then call the tool " +
	"that delivers — the reply, the comment, the status change, the create. " +
	"A turn that gathers data and stops has delivered nothing. If a tool you " +
	"need is not among the tools you see, search your tools for it before " +
	"reporting that you cannot act: every tool your seat holds is yours." +
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

// KeyCodeHistory is the outline key of the guidance on answering questions
// about the company's own code.
const KeyCodeHistory = "code_history"

// CodeHistoryTools are the code tools [codeHistorySection]'s claims rest on.
// The section renders only where the executor holds every one, so a prompt
// never sends a seat to a tool its surface lacks; they are registered
// together, so a seat holds all or none. Exported for
// internal/agent/builtin's test holding them to the tools it registers, which
// this package cannot import.
func CodeHistoryTools() []string {
	return []string{toolname.CodeResolve, toolname.CodeRead, toolname.CodeDiff, toolname.CodeCompare,
		toolname.CodeLandedIn, toolname.CodeLog, toolname.CodeBlame, toolname.CodeGetChange}
}

// codeHistorySection is the one rule no tool's description can carry: the
// choice made before picking a tool, between a server-side read by commit id
// (milliseconds) and a clone in the box (minutes, and the Gitlet nodes'
// transfer lane). How each tool is called is its description's. The family is
// named as `code_*` so the sentence stays true as reads join it; the change
// read is named because a reviewer checking a change out faces the same
// choice.
const codeHistorySection = "\n## Reading the company's code" +
	"\nWhat a file says, what changed, who last changed a line, how a file's " +
	"history went, and whether a commit reached a branch are read on the " +
	"server with the `code_*` tools, by commit id — never from a clone, which " +
	"costs minutes for one file. A change's diff, its findings and what changed " +
	"since your review are read with `" + toolname.CodeGetChange + "`."

// KeyCodeSearch is the outline key of the guidance on searching the company's
// code.
const KeyCodeSearch = "code_searching"

// CodeSearchTools are the code tools [codeSearchSection]'s claim rests on,
// gated and exported as [CodeHistoryTools] are.
func CodeSearchTools() []string { return []string{toolname.CodeSearch} }

// codeSearchSection is the choice no tool's description reaches a seat early
// enough to make: one search across its repositories rather than walking trees
// with code_read, a round and context per file. How a search is written is
// the tool's description; "every repository" would be false of a `search:
// grep` one no repo: names.
const codeSearchSection = "\n## Searching the company's code" +
	"\nWhere something is defined, used or mentioned is found with `" + toolname.CodeSearch + "` " +
	"across the repositories you can read — search before reading whole trees."

// KeyCodeWrites is the outline key of the guidance on changing the company's
// own code.
const KeyCodeWrites = "code_writes"

// CodeWriteTools are the code tools [codeWritesSection]'s claims rest on,
// gated and exported as [CodeHistoryTools] are. A seat whose code tier writes
// nothing holds neither (internal/agent/builtin's CodeWritesWithheld), so it
// is told nothing about changing a repository.
func CodeWriteTools() []string {
	return []string{toolname.CodeCommitFiles, toolname.CodeCreateRepo}
}

// codeWritesSection is [codeHistorySection]'s choice made for a write: a change
// Gitlet builds from a call (a push, no checkout) or a checkout built and
// tested in the box (minutes). How each write is called, and a scratch
// repository's expiry, are the tools' descriptions'.
const codeWritesSection = "\n## Changing the company's code" +
	"\nA small change to a repository — a document, a configuration, a fix of a " +
	"few lines — is committed on the server with `" + toolname.CodeCommitFiles + "`; anything to " +
	"build or test belongs in a checkout. Your experiments go in a scratch " +
	"repository, `<your handle>.<name>`."

// KeyCodeMerging is the outline key of the guidance on merging a change.
const KeyCodeMerging = "code_merging"

// CodeMergeTools are the code tools [codeMergingSection]'s claims rest on,
// gated and exported as [CodeHistoryTools] are.
func CodeMergeTools() []string { return []string{toolname.CodeMergeChange} }

// codeMergingSection is what a seat does while a change waits: nothing, since
// the engine wakes it (`ready_to_merge` once the last precondition is met,
// `merged` once a queue lands it), so polling or holding the turn open spends
// rounds on what a wake delivers. Preconditions are the tool's answer.
const codeMergingSection = "\n## Merging a change" +
	"\nA change merges with `" + toolname.CodeMergeChange + "` once it is ready. A merge that " +
	"is not ready, or is queued for its checks, is not waited on: end the turn, " +
	"and a `ready_to_merge` or `merged` wake brings you back."

// ExecutorInput is everything the executor prompt renders beyond the seat
// itself, frozen at turn start (package doc, "Byte-stability across rounds").
// The zero value renders the plain contract with no prefetch blocks.
//
// No tool is listed: the executor holds every tool from launch, each reaching
// its harness over the bridge with its server's MCP instructions
// (internal/api/mcpbridge, servers.go).
type ExecutorInput struct {
	// AvailableTools is every name on the phase's surface, the tools the
	// phase adds included (`submit_work`, `delegate`), never the registry
	// snapshot, which lacks those. It gates every section naming a tool.
	AvailableTools []string

	// Workers is the rendered `## Your workers` block, one line per delegate
	// template this seat may name. Gated on `delegate` being on the surface:
	// otherwise a seat spends a round finding the tool missing and reads the
	// refusal as a permission decision nobody made.
	Workers string

	// CounterpartyProfile is the observed-traits block for the turn's
	// triggering counterparty, pre-rendered and trimmed by the profiler.
	CounterpartyProfile string

	// OpenAsks is the seat's own open questions to the people who triggered
	// the turn, each with the call that withdraws it.
	OpenAsks string

	// Systems is the entries of the company's atlas the turn's ask names,
	// fenced as text somebody else wrote.
	Systems string

	// SynthesizedSkills is the skills this agent learned for itself, not the
	// operator's tool skills, which its harness lists (internal/agent/runner,
	// handedskills.go).
	SynthesizedSkills string

	// EpisodeRecall is episodic recall, frozen at turn start to preserve
	// prefix caching.
	EpisodeRecall string

	// PersonalMemory is the agent-scope diary block, already filtered for
	// relevance to this task. Empty when the filter is unavailable, fails,
	// or finds no candidates.
	PersonalMemory string

	// RelevantKnowledge is the team knowledge-base prefetch: pages found by
	// a query-time search built from the trigger, frozen at turn start.
	RelevantKnowledge string

	// ThreadContext is the chat thread this turn was woken in, read at turn
	// start; empty for a trigger that is not a chat thread reply.
	ThreadContext string

	// Access is the rendered "## Your access" section (internal/capability's
	// YourAccess), current on every turn, so it outranks what an earlier turn
	// of a continued session said. Empty where no integration is connected.
	Access string
}

// BuildExecutor renders the executor's system prompt, and its outline.
//
// The whole static picture (identity, mission, policies, unit, roster) is
// inline: a knowledge round-trip per turn costs more than the tokens it saves,
// and an actor without it meets an unforeseen choice with nothing to decide
// it against. Each block is an outline section under a stable key; prefetched
// blocks are keyed as PrefetchSummary measures them (the Key consts below).
func BuildExecutor(seat Seat, in ExecutorInput) Prompt {
	// One plan for both views of the direct reports (identity line and
	// roster), so they cannot disagree about who is on the team. See
	// [planRoster].
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
		// After the policies (what it may do): what it can reach.
		{KeyAccess, accessSection(in.Access)},
		{"team", BuildRosterSection(seat, reports)},
		{"human_colleagues", BuildHumanColleaguesNote(seat)},
	}
	empty := true
	for _, section := range identity {
		b.Heading(section.key, section.lines...)
		empty = empty && len(section.lines) == 0
	}
	if empty {
		// The empty body still takes its place: a seat with no identity
		// opens on a blank line, since the outline never changes the text.
		b.Add("")
	}

	b.Heading("your_turn", ExecutorHeader)
	// After the contract: the one case where finishing the arc means stopping
	// until somebody answers.
	b.Heading("escalation", BuildEscalationSection(in.AvailableTools)...)
	// How to read fenced text, before any of it. Unconditional and constant
	// ([provenance.StandingInstruction]): the token lives in the fences, so
	// these bytes never vary and cost the prompt cache nothing.
	b.Heading(KeyUntrustedText, untrustedTextHeading, provenance.StandingInstruction)

	// First of the prefetched blocks: the trigger's own context before what
	// is retrieved about it. Not "Thread context", which the chat trigger's
	// body in the user message already uses: two sections under one heading
	// leave the model reading whichever it saw last.
	if in.ThreadContext != "" {
		b.Heading(KeyThreadContext, "\n## The thread so far", in.ThreadContext)
	}
	// After the thread: what the seat already asked the same people.
	if in.OpenAsks != "" {
		b.Heading(KeyOpenAsks, "\n## Your open questions to them", in.OpenAsks)
	}
	// Before memory: the systems the work names, checked before probing.
	if in.Systems != "" {
		b.Heading(KeySystems, "\n## Systems in this work", in.Systems)
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
	// Before the code sections: choosing a worker is how to do the work, not
	// which tool to call. The list alone; what a worker is, is `delegate`'s
	// description.
	if in.Workers != "" && slices.Contains(in.AvailableTools, toolname.Delegate) {
		b.Heading("workers", "\n## Your workers", in.Workers)
	}
	// Each code section is gated on every tool its claims rest on: reading,
	// then finding what to read, then writing, then merging.
	if holdsAll(in.AvailableTools, CodeHistoryTools()) {
		b.Heading(KeyCodeHistory, codeHistorySection)
	}
	if holdsAll(in.AvailableTools, CodeSearchTools()) {
		b.Heading(KeyCodeSearch, codeSearchSection)
	}
	if holdsAll(in.AvailableTools, CodeWriteTools()) {
		b.Heading(KeyCodeWrites, codeWritesSection)
	}
	if holdsAll(in.AvailableTools, CodeMergeTools()) {
		b.Heading(KeyCodeMerging, codeMergingSection)
	}

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

// KeyAccess is the outline key of the "## Your access" section.
const KeyAccess = "access"

// accessSection is the rendered section as the builder's lines, or nil when
// there is none.
func accessSection(access string) []string {
	if strings.TrimSpace(access) == "" {
		return nil
	}
	return []string{"\n" + access}
}

// KeyDiscoverableTools is the outline key of the tools a phase can reach but
// does not hold yet.
const KeyDiscoverableTools = "discoverable_tools"

// discoverableSection is the heading and list of what a phase can still
// discover, or nil when there is nothing (no heading over "(none)").
//
// Not "Available tools": that tells the model it may call what it must first
// discover, a call the session refuses.
func discoverableSection(list string) []string {
	if strings.TrimSpace(list) == "" {
		return nil
	}
	return []string{"\n## Tools to discover", list}
}

// The keys of the executor's prefetched blocks: the prefixes of the
// [github.com/crewlet/crewlet/internal/events/types.PrefetchSummary] fields
// measuring each (`thread_context_hit`, …), so a reader joins the two records
// without a table. A test holds these to the summary's tags.
const (
	KeyThreadContext     = "thread_context"
	KeyPersonalMemory    = "personal_memory"
	KeySynthesizedSkills = "synthesized_skills"
	KeyRelevantKnowledge = "relevant_knowledge"
	KeyEpisodeRecall     = "episode_recall"
	KeyCounterparty      = "counterparty"
	KeyOpenAsks          = "open_asks"
	KeySystems           = "systems"
)
