package prompts

import "github.com/crewlet/crewlet/internal/toolname"

// OnboardingHeader is the one-time setup contract, run on a seat's first turn
// before the executor.
//
// It says what the pass is and how it reaches its pages, and nothing about
// which pages or how it ends: the steps are the hint's ([OnboardingInput.Hint],
// under its own heading), which ends on `mark_onboarded`, and that tool's own
// description says it ends the pass. Both used to say so here as well — two
// copies of one instruction, one of them a sentence before the other.
//
// BACKEND-NEUTRAL, by capability ("your knowledge-base tools"), never by
// product or by tool name: on Doclet they are first-party tools
// (`search_knowledge`, `get_page`) listed among the tools to discover, and on
// Confluence the page reads are an MCP server's. It said once that they were
// always MCP tools, which on the default backend sent every seat to list a
// server for a tool it could have activated by name.
const OnboardingHeader = "\n## ONBOARDING phase" +
	"\nThis is a one-time setup pass with its **own** budget. Do ONLY the " +
	"steps below now — not the task you were triggered on, which runs next. " +
	"Your knowledge-base tools are among the tools to discover below: " +
	"`" + toolname.Activate + "` makes one callable (an MCP server's are " +
	"named by `" + toolname.ListMCPServerTools + "` first)."

// OnboardingInput is what the onboarding pass renders beyond the seat.
type OnboardingInput struct {
	// Hint is the org-chain-derived instruction set: which pages to read,
	// what to persist, and then mark_onboarded.
	Hint string

	// Discoverable is what the pass can reach but does not hold yet,
	// rendered by [github.com/crewlet/crewlet/internal/tools.Snapshot.Discoverable]
	// — its knowledge-base tools among them, which is how it finds them.
	Discoverable string
}

// BuildOnboarding renders the first-turn onboarding system prompt.
//
// Lightweight and dedicated: identity line, the onboarding contract, the tools
// to discover. No policies, no roster, no phase plumbing — onboarding
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
	b.Heading(KeyDiscoverableTools, discoverableSection(in.Discoverable)...)
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
