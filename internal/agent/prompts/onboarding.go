package prompts

import "github.com/crewlet/crewlet/internal/toolname"

// OnboardingHeader is the one-time setup contract, run on a seat's first turn
// before the executor.
//
// It says what the pass is and how it reaches its pages, nothing about which
// pages or how it ends: those are the hint's ([OnboardingInput.Hint]), which
// ends on `mark_onboarded`, whose description says it ends the pass.
//
// BACKEND-NEUTRAL ("your knowledge-base tools"), never by product or tool
// name: on Doclet they are first-party tools to activate by name, on
// Confluence an MCP server's; calling them MCP tools sent Doclet seats to list
// a server needlessly.
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
// No policies, roster or phase plumbing: onboarding is a fixed read → persist
// → mark workflow on its own budget, so it cannot starve the executor after it.
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
// as every phase's record carries one for each prompt.
func BuildOnboardingUserMessage() Prompt {
	b := NewBuilder("")
	b.Lead("instruction", "Instruction", OnboardingUserMessage)
	return b.Build()
}
