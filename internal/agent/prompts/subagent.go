package prompts

import "strings"

// SubagentPreamble is the mandated runtime contract every sub-agent carries,
// appended after the parent's task prompt.
//
// The two prohibitions are structural, not stylistic: a sub-agent that could
// spawn sub-agents has no depth bound, and one that could contact colleagues
// would put a short-lived worker's half-formed conclusions on a teammate's
// desk under the parent's name.
const SubagentPreamble = "You are a short-lived worker. Do not delegate further. " +
	"Do not contact colleagues. You can discover and activate more tools " +
	"yourself: call `list_mcp_server_tools(server=...)` to see what an " +
	"MCP server offers, then `activate_tool(name=...)` to promote a tool " +
	"into your `tools=[...]` so you can call it on the next round. Only " +
	"read-only tools are available this way — you cannot post to " +
	"channels, comment on issues, open PRs, or otherwise write to a " +
	"shared surface."

// SubagentSubmitRule is how a worker ends when it has a submission tool.
//
// Explicit about what is DISCARDED, because that is the part a model gets
// wrong: it writes the answer as prose, calls the tool with a pointer to what
// it just said, and the parent reads a field saying "see above" about a
// conversation it cannot see.
const SubagentSubmitRule = "When you are done, call `submit_result` with your " +
	"answer and stop. That submission is the ONLY thing passed back — your " +
	"tool calls, your reasoning and anything you wrote as prose are not. If " +
	"you could not finish, submit what you do have and say why in the fields " +
	"the schema gives you: a partial answer is worth far more than none."

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

	// ToolCatalogue is the slim catalogue, present only when the worker is
	// discovery-capable.
	ToolCatalogue string

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
// read in the context of the task), then the catalogue, then the preamble.
// No identity section and no policies — a sub-agent is a short-lived worker,
// not a teammate — but it gets the same MCP scaffolding a regular agent has
// when calling and discovering those tools.
func BuildSubagent(seat Seat, in SubagentInput) string {
	parts := []string{in.ParentSystemPrompt}
	parts = injectSkillCatalogue(parts, in.Skills, PhaseSubagent, Surface{
		Tools:      in.AvailableTools,
		MCPServers: seat.mcpServers(),
	})
	if strings.TrimSpace(in.ToolCatalogue) != "" {
		parts = append(parts, "", "## Available tools", in.ToolCatalogue)
	}
	ending := SubagentProseRule
	if in.Submits {
		ending = SubagentSubmitRule
	}
	parts = append(parts, "", SubagentPreamble+" "+ending)
	return strings.Join(parts, "\n")
}
