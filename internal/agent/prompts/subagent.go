package prompts

import (
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/toolname"
)

// SubagentPreamble is the mandated runtime contract every sub-agent carries,
// appended after the parent's task prompt.
//
// The prohibitions are structural, not stylistic: a sub-agent that could
// spawn sub-agents has no depth bound, and one that could contact colleagues
// or write to a shared surface would put a short-lived worker's half-formed
// conclusions on a teammate's desk under the parent's name. The grant enforces
// all three ([github.com/crewlet/crewlet/internal/agent/subagent.Permit]); the
// sentence is what keeps a worker from spending a round finding that out.
const SubagentPreamble = "You are a short-lived worker. Do not delegate further. " +
	"Do not contact colleagues. You cannot write to a shared surface: no " +
	"posting, commenting or opening pull requests."

// subagentDiscoveryRule is how a worker reaches what it was not granted up
// front, rendered only beside a list of tools to discover: a worker with
// nothing to discover — or no discovery tools at all — told to discover is
// told to call a tool that can only refuse it.
const subagentDiscoveryRule = "To use one of the tools to discover above, call " +
	"`" + toolname.Activate + "` (an MCP server's tools are named by " +
	"`" + toolname.ListMCPServerTools + "` first)."

// SubagentSubmitRule is how a worker ends when it has a submission tool.
//
// ONE SENTENCE NAMING THE TOOL, and nothing about what it carries: that
// everything else the worker wrote is discarded, and that a partial answer
// beats none, is `submit_result`'s own description, which reaches the worker
// with the tool. The name stays here because a worker that never learns its
// answer is a call writes it as prose, and costs a corrective round to be told.
const SubagentSubmitRule = "End by calling `" + toolname.SubmitResult + "` with your answer."

// SubagentProseRule is how a worker ends when it has no submission tool.
//
// Only reachable when a granted tool already publishes the submission tool's
// name, which is a config collision the engine logs — but the worker still
// has to know how to answer, and a prompt telling it to call a tool it does
// not have would cost it a round to discover that.
const SubagentProseRule = "Return a concise final answer as text when done. If " +
	"you cannot complete the task, return what you have with a brief note on why."

// SubagentInput is what a sub-agent prompt renders around the preamble.
type SubagentInput struct {
	// ParentSystemPrompt is the task-specific prompt the spawning agent
	// wrote. It leads, so the worker reads its task before its rules.
	ParentSystemPrompt string

	// AvailableTools is the parent-passed allowlist; it scopes skill
	// matching to tools this worker can actually call.
	AvailableTools []string

	// Discoverable is what the worker can reach but was not granted up
	// front, rendered by [github.com/crewlet/crewlet/internal/tools.Snapshot.Discoverable]
	// — present only when the worker is discovery-capable. It also decides
	// whether the rules tell the worker how to discover.
	Discoverable string

	// Submits says the worker has its submission tool, which decides which
	// of the two ending rules the preamble carries. It is a fact about the
	// SURFACE rather than a preference: telling a worker to call a tool it
	// does not have costs it a round to find out.
	Submits bool

	// Skills is the tool-skill registry; nil keeps the prompt free of skill
	// scaffolding.
	Skills SkillCatalogue
}

// BuildSubagent renders a sub-agent system prompt.
//
// Order is parent task prompt, then the skill catalogue (so tool guidance is
// read in the context of the task), then the tools to discover, then the
// preamble.
// No identity section and no policies — a sub-agent is a short-lived worker,
// not a teammate — but it gets the same MCP scaffolding a regular agent has
// when calling and discovering those tools.
//
// The parent's task prompt is ONE section of the outline, headings and all:
// it is a model's or a template author's document, and its "## Steps" are its
// own. The mandated rules have no heading in the text, so the outline names
// them — they are the part of a worker's prompt nobody but the engine wrote.
func BuildSubagent(seat Seat, in SubagentInput) Prompt {
	b := NewBuilder("\n")
	b.Lead("task", "Task", in.ParentSystemPrompt)
	b.Heading("tool_skills", skillCatalogue(in.Skills, PhaseSubagent, Surface{
		Tools:      in.AvailableTools,
		MCPServers: seat.mcpServers(),
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
	b.Add("")
	// THE STANDING SENTENCE ABOUT FENCED TEXT rides the rules, because a
	// worker reads pages, items and files with the parent's read tools and
	// meets the same fences the seat does; see [provenance.StandingInstruction].
	b.Lead("worker_rules", "Worker rules", rules+" "+
		provenance.StandingInstruction+" "+ending)
	return b.Build()
}
