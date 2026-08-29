package prompts

import (
	"strings"
	"testing"
)

// -- Onboarding ----------------------------------------------------------

// The one-time pass points at the team's knowledge-base MCP server by
// capability, not by product: the same header has to read correctly whatever
// knowledge base a company runs.
func TestOnboardingHeaderIsBackendNeutral(t *testing.T) {
	t.Parallel()
	contains(t, OnboardingHeader,
		"knowledge-base search / read tools",
		"a page-search / get-page tool on your team's knowledge-base server")
	lowered := strings.ToLower(OnboardingHeader)
	excludes(t, lowered, "confluence", "atlassian", "jira")
}

func TestOnboardingPromptRendersHintAndCatalogue(t *testing.T) {
	t.Parallel()
	p := BuildOnboarding(engineer(), OnboardingInput{
		Hint:          "Read the 'Onboarding' pages on your chain.",
		ToolCatalogue: "- knowledge_search: Search team pages.",
	})
	contains(t, p, "ONBOARDING phase", "## What to do",
		"Read the 'Onboarding' pages on your chain.",
		"## Available tools", "knowledge_search: Search team pages.")

	// A whitespace-only catalogue is no catalogue: it would leave a heading
	// over nothing.
	bare := BuildOnboarding(engineer(), OnboardingInput{ToolCatalogue: "  \n"})
	excludes(t, bare, "## What to do", "## Available tools")
}

// -- Sub-agent -----------------------------------------------------------

func TestSubagentPromptAppendsTheMandatedPreamble(t *testing.T) {
	t.Parallel()
	parent := "You are a web research worker. Return a concise summary."
	p := BuildSubagent(engineer(), SubagentInput{ParentSystemPrompt: parent})
	// Parent task framing first, runtime contract last.
	order(t, p, parent, SubagentPreamble)
}

// The two prohibitions are structural: a sub-agent that could spawn
// sub-agents has no depth bound, and one that could contact colleagues would
// put a short-lived worker's half-formed conclusions on a teammate's desk
// under the parent's name.
func TestSubagentPreambleStatesTheInvariants(t *testing.T) {
	t.Parallel()
	contains(t, SubagentPreamble,
		"Do not spawn further sub-agents",
		"Do not contact colleagues",
		"concise final answer")
}

func TestSubagentPromptOrdersParentThenSkillsThenCatalogueThenPreamble(t *testing.T) {
	t.Parallel()
	cat := &fakeCatalogue{skills: []fakeSkill{{
		key: "mcp:github", mcpServer: "github", summary: "GITHUB-SUMMARY",
		body: "LONG GITHUB BODY", phases: []Phase{PhaseSubagent},
	}}}
	p := BuildSubagent(engineer(), SubagentInput{
		ParentSystemPrompt: "PARENT-PROMPT-MARKER",
		ToolCatalogue:      "- foo: Does foo.",
		Skills:             cat,
	})
	order(t, p, "PARENT-PROMPT-MARKER", "GITHUB-SUMMARY",
		"## Available tools", SubagentPreamble)
	// No identity section and no policies: a sub-agent is a worker, not a
	// teammate.
	excludes(t, p, "# Your Identity", "Company policies", "LONG GITHUB BODY")
}

// -- Phase user message --------------------------------------------------

// The common single-pass turn keeps its exact pre-ledger shape, so nothing
// shifts for the turns that never self_iterate — which is most of them.
func TestPhaseUserMessageWithoutLedgersIsJustTheTask(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ task, want string }{
		{"do the thing", "Task:\ndo the thing"},
		{"", "Task:\n(no description)"},
	} {
		if got := BuildPhaseUserMessage(UserMessage{TaskDescription: tc.task}); got != tc.want {
			t.Errorf("BuildPhaseUserMessage(%q) = %q, want %q", tc.task, got, tc.want)
		}
	}
}

func TestPhaseUserMessageReadsChronologically(t *testing.T) {
	t.Parallel()
	msg := BuildPhaseUserMessage(UserMessage{
		TaskDescription:     "THE-ASK",
		PriorWork:           "PRIOR-WORK",
		ConversationHistory: "CONVERSATION-HISTORY",
	})
	// Earlier turns of this conversation, then the ask, then earlier rounds
	// of THIS turn — which happened after the ask arrived. The newest thing
	// said sits nearest the model's answer.
	order(t, msg, "CONVERSATION-HISTORY", "THE-ASK", "PRIOR-WORK")
	if !strings.HasPrefix(msg, ConversationHistoryHeader) {
		t.Error("conversation history must lead the message")
	}
}

func TestPhaseUserMessageCarriesTheLedgerRules(t *testing.T) {
	t.Parallel()
	withPrior := BuildPhaseUserMessage(UserMessage{
		TaskDescription: "post the summary",
		PriorWork:       "### Iteration 1\nExecute called:\n- post_message(...) → success",
	})
	if !strings.HasPrefix(withPrior, "Task:\npost the summary") {
		t.Error("the ask must lead when there is no conversation history")
	}
	// The rule that actually prevents the double-post.
	contains(t, withPrior, "Already done earlier in this turn", "### Iteration 1", "ALREADY RAN")

	withHistory := BuildPhaseUserMessage(UserMessage{
		TaskDescription:     "any update?",
		ConversationHistory: "### 2026-08-20T09:30\nYou replied: shipped it",
	})
	contains(t, withHistory,
		"You replied: shipped it",
		// The block is worse than absent if the model reads it as a script
		// to re-run rather than as what it already said.
		"Do not repeat a reply you already gave",
		// Across turns this warning is stronger than within one: a read
		// from last Tuesday is stale by construction.
		"may be stale", "moved on")
	if !strings.HasSuffix(strings.TrimRight(withHistory, "\n"), "Task:\nany update?") {
		t.Error("the ask must sit last when there is no prior work")
	}
}
