package prompts

import "strings"

// SubagentPreamble is the mandated runtime contract every sub-agent carries,
// appended after the parent's task prompt.
//
// The two prohibitions are structural, not stylistic: a sub-agent that could
// spawn sub-agents has no depth bound, and one that could contact colleagues
// would put a short-lived worker's half-formed conclusions on a teammate's
// desk under the parent's name.
const SubagentPreamble = "You are a short-lived sub-agent. Do not spawn further sub-agents. " +
	"Do not contact colleagues. You can discover and activate more tools " +
	"yourself: call `list_mcp_server_tools(server=...)` to see what an " +
	"MCP server offers, then `activate_tool(name=...)` to promote a tool " +
	"into your `tools=[...]` so you can call it on the next round. Only " +
	"read-only tools are available this way — you cannot post to " +
	"channels, comment on issues, open PRs, or otherwise write to a " +
	"shared surface. Return a concise final answer as text when done. If " +
	"you cannot complete the task, return what you have with a brief " +
	"note on why."

// SubagentInput is what a sub-agent prompt renders around the preamble.
type SubagentInput struct {
	// ParentSystemPrompt is the task-specific prompt the spawning agent
	// wrote. It leads, so the worker reads its task before its rules.
	ParentSystemPrompt string

	// AvailableTools is the parent-passed allowlist; it scopes skill
	// matching to tools this worker can actually call.
	AvailableTools []string

	// ToolCatalogue is the slim catalogue, present only when the sub-agent
	// is discovery-capable.
	ToolCatalogue string

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
	parts = append(parts, "", SubagentPreamble)
	return strings.Join(parts, "\n")
}
