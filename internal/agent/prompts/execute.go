package prompts

import "strings"

// ExecuteHeader is the Execute-phase contract.
//
// Two sentences of it carry the phase: "writing about an action does not
// execute it" (the failure mode where a turn ends with a composed reply
// nobody was sent), and the discovery paragraph, which is what stops the
// executor giving up when the planner named a tool it cannot see.
const ExecuteHeader = "\n## EXECUTE phase" +
	"\nRun the plan below. Use tool calls for every action — writing " +
	"about an action does not execute it." +
	"\n" +
	"\n**Discovery is available.** Your `tools=[...]` starts with the " +
	"plan-named tools, always-on builtins (`load_tool_skill`), and " +
	"discovery meta-tools (`activate_tool`, `list_mcp_server_tools`). If " +
	"you find mid-run that you need a tool the planner didn't list, call " +
	"`list_mcp_server_tools(server=...)` to discover MCP tool names and " +
	"`activate_tool(name=...)` to promote any catalogue tool (builtin or " +
	"MCP) into your `tools=[...]`. The activated tool's schema appears " +
	"on the next message; call it directly. Prefer activation over " +
	"giving up: only stop and report in plain text if discovery fails or " +
	"the tool genuinely does not exist."

// ExecuteInput is everything the Execute prompt renders beyond the seat.
//
// Frozen at turn start, like [PlanInput], and for the same reason: Execute
// loops, and a system prompt that moved between rounds would miss the prefix
// cache on every one of them. The zero value renders identity + contract,
// which is the whole prompt for a turn with no plan summary.
type ExecuteInput struct {
	// PlanSummary is the plan Execute is running.
	PlanSummary string

	// CounterpartyProfile is forwarded from the Plan-phase prefetch so the
	// executor has the requester's observed traits even when the plan
	// describes the action abstractly ("use the counterparty's preferred
	// greeting") without baking the literal into the plan.
	CounterpartyProfile string

	// RelevantKnowledge is the post-Plan knowledge re-fetch. Non-empty only
	// when the turn's trigger was a bare pointer: the Plan-phase prefetch
	// is gated off there (searching on a thin pointer returns noise), so
	// the block is re-fetched keyed on the plan summary and handed here.
	RelevantKnowledge string

	// AvailableTools is the executor's STARTING surface (the plan's
	// tools_needed plus the always-on builtins). It scopes skill matching:
	// only skills for a tool the executor will actually call are offered.
	AvailableTools []string

	// ToolCatalogue is the same slim catalogue Plan sees — builtin names
	// plus MCP server names — so the executor knows what discovery surface
	// exists. Empty when the surface was built without one.
	ToolCatalogue string

	// PhantomTools are names the planner put in tools_needed that do NOT
	// resolve in Execute's catalogue — almost always wrong guesses at an
	// MCP tool's name, since the planner sees server names only. Naming
	// them explicitly is what stops the executor assuming the tool exists,
	// failing to call it, and settling for a text reply that delivers
	// nothing.
	PhantomTools []string

	// Skills is the tool-skill registry; nil keeps the prompt free of skill
	// scaffolding.
	Skills SkillCatalogue
}

// BuildExecute renders the Execute-phase system prompt.
//
// Deliberately thin: no policies (Plan already decided the action surface and
// carried policy-driven constraints into success_criteria), no roster, no org
// context. Identity, the contract, the plan, and the tools.
func BuildExecute(seat Seat, in ExecuteInput) string {
	parts := []string{BuildIdentityLine(seat), ExecuteHeader}

	if len(in.PhantomTools) > 0 {
		quoted := make([]string, 0, len(in.PhantomTools))
		for _, t := range in.PhantomTools {
			quoted = append(quoted, "`"+t+"`")
		}
		parts = append(parts, "\n**Heads-up — some tools your plan named are NOT in your "+
			"catalogue and were almost certainly wrong guesses at an MCP "+
			"tool's name: "+
			strings.Join(quoted, ", ")+
			".** Do not assume they exist and do not stop at writing a "+
			"text reply. Call `list_mcp_server_tools(server=...)` to find "+
			"the real tool on the relevant server, then "+
			"`activate_tool(name=...)` and call it — composing the reply "+
			"as text does not deliver it.")
	}
	if in.PlanSummary != "" {
		parts = append(parts, "\n## Plan", in.PlanSummary)
	}
	// The skill catalogue lands AFTER the plan so the executor reads the
	// task framing first, then tool guidance in the context of what it is
	// about to do.
	parts = injectSkillCatalogue(parts, in.Skills, PhaseExecute, Surface{
		Tools:      in.AvailableTools,
		MCPServers: seat.mcpServers(),
	})
	if in.RelevantKnowledge != "" {
		parts = append(parts, "\n## Relevant knowledge", in.RelevantKnowledge)
	}
	if in.CounterpartyProfile != "" {
		parts = append(parts, "\n## Known counterparty", in.CounterpartyProfile)
	}
	if in.ToolCatalogue != "" {
		parts = append(parts, "\n## Available tools", in.ToolCatalogue)
	}
	return strings.Join(parts, "\n")
}
