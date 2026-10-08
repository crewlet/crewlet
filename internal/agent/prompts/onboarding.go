package prompts

import "strings"

// OnboardingHeader is the one-time setup contract, run on a seat's first turn
// before the executor.
//
// Backend-neutral by design: it points at the team's knowledge-base MCP
// server by CAPABILITY ("a page-search / get-page tool"), never by product,
// so the same words read correctly whatever knowledge base the company runs.
// Naming a product here would make the header wrong for anyone on another
// one and right-looking for all of them.
const OnboardingHeader = "\n## ONBOARDING phase" +
	"\nThis is a one-time setup pass that runs before your normal work, " +
	"with its **own** budget. Do ONLY this now — not the task you were " +
	"triggered on (that runs next).\n" +
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
// that nothing it does can starve the executor that follows.
//
// It returns the prompt's outline beside the text — see [Prompt].
func BuildOnboarding(seat Seat, in OnboardingInput) Prompt {
	b := NewBuilder("\n")
	b.Lead("identity", "Identity", BuildIdentityLine(seat))
	b.Heading("onboarding_phase", OnboardingHeader)
	if in.Hint != "" {
		b.Heading("what_to_do", "\n## What to do", in.Hint)
	}
	if strings.TrimSpace(in.ToolCatalogue) != "" {
		b.Heading("available_tools", "\n## Available tools", in.ToolCatalogue)
	}
	return b.Build()
}

// OnboardingUserMessage is the onboarding pass's whole user message: the pass
// is a fixed workflow its system prompt describes, so the ask is one line.
const OnboardingUserMessage = "Complete your onboarding now."

// BuildOnboardingUserMessage renders [OnboardingUserMessage] with its outline,
// so the pass's record carries a map for each of its two prompts like every
// other phase's.
func BuildOnboardingUserMessage() Prompt {
	b := NewBuilder("")
	b.Lead("instruction", "Instruction", OnboardingUserMessage)
	return b.Build()
}
