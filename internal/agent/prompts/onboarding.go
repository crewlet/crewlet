package prompts

import "strings"

// OnboardingHeader is the one-time setup contract, run on a seat's first turn
// before the executor.
//
// ONE HEADER FOR BOTH KNOWLEDGE BASES, so it names each reader by what it is
// rather than by product. On the engine's own knowledge base the search and
// the page read are this engine's `search_knowledge` and `get_page`, in the
// catalogue like every other first-party tool; on a vendor wiki they are that
// wiki's own MCP server's tools, whose names only the server knows. The pass
// starts with neither active — its surface is the two tools below plus the
// discovery pair — so the header says how each is reached. Naming a vendor
// here would make the header wrong for anyone on another one and
// right-looking for all of them.
const OnboardingHeader = "\n## ONBOARDING phase" +
	"\nThis is a one-time setup pass that runs before your normal work, " +
	"with its **own** budget. Do ONLY this now — not the task you were " +
	"triggered on (that runs next).\n" +
	"Read your team's pages with your knowledge-base search and page-read " +
	"tools, and activate them before calling them with " +
	"`activate_tool(name=...)`. Where your company's knowledge base is the " +
	"engine's own, they are `search_knowledge` and `get_page` in your tool " +
	"catalogue; where it is a vendor wiki, they are that wiki's MCP server's " +
	"page-search and get-page tools — call " +
	"`list_mcp_server_tools(server=...)` on your team's knowledge-base server " +
	"to find their names. " +
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
// that nothing it does can starve the executor that follows.
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
