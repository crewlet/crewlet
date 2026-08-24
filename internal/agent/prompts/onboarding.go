package prompts

import "strings"

// OnboardingHeader is the one-time, pre-Plan setup contract.
//
// Backend-neutral by design: it points at the team's knowledge-base MCP
// server by CAPABILITY ("a page-search / get-page tool"), never by product,
// so the same words read correctly whether the company runs Confluence or
// Plane. Naming a product here would make the header wrong for half the
// installed base and right-looking for all of it.
const OnboardingHeader = "\n## ONBOARDING phase" +
	"\nThis is a one-time setup pass that runs before your normal work, " +
	"with its **own** budget. Do ONLY this now — not the task you were " +
	"triggered on (that runs next, in Plan).\n" +
	"Your knowledge-base search / read tools are MCP tools: call " +
	"`list_mcp_server_tools(server=...)` to find them (a page-search / " +
	"get-page tool on your team's knowledge-base server), then " +
	"`activate_tool(name=...)` to promote one into your `tools=[...]`. " +
	"`reflect_and_persist` and `mark_onboarded` are already active — " +
	"call them directly. Follow the steps below, then call " +
	"`mark_onboarded` to end the pass."

// OnboardingInput is what the onboarding pass renders beyond the seat.
type OnboardingInput struct {
	// Hint is the org-chain-derived instruction set: which pages to read,
	// what to persist, and then mark_onboarded.
	Hint string

	// ToolCatalogue is the slim discovery catalogue, so the agent can
	// locate its knowledge-base server.
	ToolCatalogue string
}

// BuildOnboarding renders the first-turn onboarding system prompt.
//
// Lightweight and dedicated: identity line, the onboarding contract, the
// discovery catalogue. No policies, no roster, no phase plumbing — onboarding
// is a fixed read → persist → mark workflow, and it runs on its own budget so
// that nothing it does can starve the Plan phase that follows.
func BuildOnboarding(seat Seat, in OnboardingInput) string {
	parts := []string{BuildIdentityLine(seat), OnboardingHeader}
	if in.Hint != "" {
		parts = append(parts, "\n## What to do", in.Hint)
	}
	if strings.TrimSpace(in.ToolCatalogue) != "" {
		parts = append(parts, "\n## Available tools", in.ToolCatalogue)
	}
	return strings.Join(parts, "\n")
}
