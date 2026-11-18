package prompts

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/toolname"
)

// SubagentPreamble is the mandated runtime contract every sub-agent carries,
// appended after the parent's task prompt.
//
// The prohibitions are structural: a sub-agent that could spawn sub-agents
// has no depth bound, and one that could contact colleagues or write to a
// shared surface would put a worker's half-formed conclusions on a teammate's
// desk under the parent's name. The grant enforces all three
// ([github.com/crewlet/crewlet/internal/agent/subagent.Permit]); the sentence
// saves the worker a round finding that out.
const SubagentPreamble = "You are a short-lived worker. Do not delegate further. " +
	"Do not contact colleagues. You cannot write to a shared surface: no " +
	"posting, commenting or opening pull requests."

// subagentDiscoveryRule is how a worker reaches what it was not granted up
// front, rendered only beside a list of tools to discover: without one, it
// names a tool that can only refuse.
const subagentDiscoveryRule = "To use one of the tools to discover above, call " +
	"`" + toolname.Activate + "` (an MCP server's tools are named by " +
	"`" + toolname.ListMCPServerTools + "` first)."

// SubagentSubmitRule is how a worker ends when it has a submission tool.
//
// Only the tool's name: that everything else is discarded and a partial
// answer beats none is `submit_result`'s own description. A worker never told
// its answer is a call writes it as prose and costs a corrective round.
const SubagentSubmitRule = "End by calling `" + toolname.SubmitResult + "` with your answer."

// SubagentProseRule is how a worker ends when it has no submission tool.
//
// Reachable only when a granted tool already publishes the submission tool's
// name, a config collision the engine logs; naming a tool the worker lacks
// would cost it a round.
const SubagentProseRule = "Return a concise final answer as text when done. If " +
	"you cannot complete the task, return what you have with a brief note on why."

// SubagentLastRound is said to a worker before its last round, so one that has
// not finished answers with what it has rather than nothing: the submission
// form, and the prose form for a worker without the tool.
const (
	SubagentLastRound = "This is your last round. Call `" + toolname.SubmitResult +
		"` now with what you have, and say what is missing."
	SubagentLastRoundProse = "This is your last round. Answer now with what you have, " +
		"and say what is missing."
)

// SubagentInput is what a sub-agent prompt renders around the preamble.
type SubagentInput struct {
	// ParentSystemPrompt is the task-specific prompt the spawning agent
	// wrote. It leads, so the worker reads its task before its rules.
	ParentSystemPrompt string

	// AvailableTools is the parent-passed allowlist; it scopes skill
	// matching to tools this worker can actually call.
	AvailableTools []string

	// MCPServers is every MCP server the worker's grant holds a tool of,
	// sorted: what the catalogue's server triggers are matched against, as
	// the worker's guard matches them (see [Surface]).
	MCPServers []string

	// Discoverable is what the worker can reach but was not granted up
	// front, rendered by [github.com/crewlet/crewlet/internal/tools.Snapshot.Discoverable],
	// set only for a discovery-capable worker; it also decides whether the
	// rules tell the worker how to discover.
	Discoverable string

	// Submits says the worker's surface holds its submission tool, which
	// picks the ending rule ([SubagentSubmitRule] or [SubagentProseRule]).
	Submits bool

	// MaxTurns is the worker's round cap (its task's `max_turns`), said in
	// its rules; zero says none.
	MaxTurns int

	// Skills is the tool-skill registry; nil keeps the prompt free of skill
	// scaffolding.
	Skills SkillCatalogue
}

// BuildSubagent renders a sub-agent system prompt.
//
// Order: the parent's task prompt, the skill catalogue (tool guidance read in
// the task's context), the tools to discover, the rules. No identity or
// policies: a sub-agent is a short-lived worker, not a teammate; but it gets
// a regular agent's MCP scaffolding.
//
// The parent's task prompt is ONE outline section, headings and all: its
// "## Steps" are its author's. The rules have no heading in the text, so the
// outline names them: they are the only part the engine wrote.
func BuildSubagent(seat Seat, in SubagentInput) Prompt {
	b := NewBuilder("\n")
	b.Lead("task", "Task", in.ParentSystemPrompt)
	b.Heading("tool_skills", skillCatalogue(in.Skills, PhaseSubagent, Surface{
		Tools:      in.AvailableTools,
		MCPServers: in.MCPServers,
	})...)
	rules := SubagentPreamble
	if section := discoverableSection(in.Discoverable); section != nil {
		b.Heading(KeyDiscoverableTools, section...)
		rules += " " + subagentDiscoveryRule
	}
	ending := SubagentProseRule
	if in.Submits {
		ending = SubagentSubmitRule
	}
	if in.MaxTurns > 0 {
		ending = fmt.Sprintf("You have up to %d rounds of tool calls. %s", in.MaxTurns, ending)
	}
	b.Add("")
	// The fenced-text sentence rides the rules: a worker reads with the
	// parent's read tools and meets the same fences ([provenance.StandingInstruction]).
	b.Lead("worker_rules", "Worker rules", rules+" "+
		provenance.StandingInstruction+" "+ending)
	return b.Build()
}
