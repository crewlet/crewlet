package prompts

import (
	"slices"
	"strings"
	"testing"
)

// -- Onboarding ----------------------------------------------------------

// The one-time pass points at the team's knowledge-base tools by capability,
// not by product and not by tool name: on Doclet they are
// first-party tools to activate, on Confluence an MCP server's, and the same
// header has to read correctly on either. It once said they were always MCP
// tools, which on the default backend sent every seat to list a server for a
// tool it could have activated by name.
//
// AND IT DOES NOT SAY HOW THE PASS ENDS: the hint's last step names
// `mark_onboarded`, and that tool's description says it ends the pass. Both
// used to say so here too.
func TestOnboardingHeaderIsBackendNeutral(t *testing.T) {
	t.Parallel()
	contains(t, OnboardingHeader,
		"knowledge-base tools",
		"`activate_tool` makes one callable",
		"`list_mcp_server_tools`")
	lowered := strings.ToLower(OnboardingHeader)
	excludes(t, lowered, "confluence", "atlassian", "jira",
		"are mcp tools", "search_knowledge", "get_page", "tools=[", "mark_onboarded")
}

func TestOnboardingPromptRendersHintAndTheToolsToDiscover(t *testing.T) {
	t.Parallel()
	p := BuildOnboarding(engineer(), OnboardingInput{
		Hint:         "Read the 'Onboarding' pages on your chain.",
		Discoverable: "- search_knowledge: Search team pages.",
	}).Text
	contains(t, p, "ONBOARDING phase", "## What to do",
		"Read the 'Onboarding' pages on your chain.",
		"## Tools to discover", "search_knowledge: Search team pages.")

	// A whitespace-only list is nothing to discover: it would leave a
	// heading over nothing.
	bare := BuildOnboarding(engineer(), OnboardingInput{Discoverable: "  \n"}).Text
	excludes(t, bare, "## What to do", "## Tools to discover")
}

// -- Sub-agent -----------------------------------------------------------

func TestSubagentPromptAppendsTheMandatedPreamble(t *testing.T) {
	t.Parallel()
	parent := "You are a web research worker. Return a concise summary."
	p := BuildSubagent(engineer(), SubagentInput{ParentSystemPrompt: parent}).Text
	// Parent task framing first, runtime contract last.
	order(t, p, parent, SubagentPreamble)
}

// The two prohibitions are structural: a worker that could delegate has no
// depth bound, and one that could contact colleagues would put a short-lived
// worker's half-formed conclusions on a teammate's desk under the parent's
// name.
func TestSubagentPreambleStatesTheInvariants(t *testing.T) {
	t.Parallel()
	contains(t, SubagentPreamble,
		"Do not delegate further",
		"Do not contact colleagues")
}

// A WORKER IS TOLD HOW TO DISCOVER ONLY BESIDE SOMETHING TO DISCOVER. A worker
// with no discovery tools, or with nothing left to reach, told to call
// activate_tool is told to call a tool that can only refuse it — and the
// preamble used to say so to every worker, frozen ones included.
func TestAWorkerIsToldToDiscoverOnlyWhenItCan(t *testing.T) {
	t.Parallel()
	with := BuildSubagent(engineer(), SubagentInput{
		ParentSystemPrompt: "p", Discoverable: "- MCP server `research`",
	}).Text
	contains(t, with, subagentDiscoveryRule)
	order(t, with, "## Tools to discover", subagentDiscoveryRule)

	without := BuildSubagent(engineer(), SubagentInput{ParentSystemPrompt: "p"}).Text
	excludes(t, without, "activate_tool", "list_mcp_server_tools", "## Tools to discover")
	contains(t, without, SubagentPreamble)
}

// HOW A WORKER ENDS depends on whether it HAS a submission tool, and the two
// rules must not be interchangeable: telling a worker to call submit_result
// when its name collided with a granted tool costs it a round to discover
// that, and telling one that has the tool to answer in prose loses the
// structured answer the parent asked for.
func TestAWorkerIsToldHowToEndTheWayItActuallyCan(t *testing.T) {
	t.Parallel()
	withTool := BuildSubagent(engineer(), SubagentInput{
		ParentSystemPrompt: "p", Submits: true,
	}).Text
	contains(t, withTool, "submit_result")
	if strings.Contains(withTool, SubagentProseRule) {
		t.Error("a worker with a submission tool was told to answer in prose")
	}

	withoutTool := BuildSubagent(engineer(), SubagentInput{ParentSystemPrompt: "p"}).Text
	contains(t, withoutTool, "concise final answer")
	if strings.Contains(withoutTool, "submit_result") {
		t.Error("a worker with no submission tool was told to call it")
	}
}

func TestSubagentPromptOrdersParentThenSkillsThenDiscoverableThenPreamble(t *testing.T) {
	t.Parallel()
	cat := &fakeCatalogue{skills: []fakeSkill{{
		key: "mcp:github", mcpServer: "github", summary: "GITHUB-SUMMARY",
		body: "LONG GITHUB BODY", phases: []Phase{PhaseSubagent},
	}}}
	p := BuildSubagent(engineer(), SubagentInput{
		ParentSystemPrompt: "PARENT-PROMPT-MARKER",
		Discoverable:       "- foo: Does foo.",
		Skills:             cat,
	}).Text
	order(t, p, "PARENT-PROMPT-MARKER", "GITHUB-SUMMARY",
		"## Tools to discover", SubagentPreamble)
	// No identity section and no policies: a sub-agent is a worker, not a
	// teammate.
	excludes(t, p, "## Your Identity", "Company policies", "LONG GITHUB BODY")
}

// -- Phase user message --------------------------------------------------

// The common single-pass turn keeps its exact pre-ledger shape, so nothing
// shifts for the turns that never self_iterate — which is most of them.
func TestPhaseUserMessageWithoutLedgersIsJustTheTask(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ task, want string }{
		{"do the thing", "## Task\ndo the thing"},
		{"", "## Task\n(no description)"},
	} {
		if got := BuildPhaseUserMessage(UserMessage{TaskDescription: tc.task}).Text; got != tc.want {
			t.Errorf("BuildPhaseUserMessage(%q).Text = %q, want %q", tc.task, got, tc.want)
		}
	}
}

func TestPhaseUserMessageReadsChronologically(t *testing.T) {
	t.Parallel()
	msg := BuildPhaseUserMessage(UserMessage{
		TaskDescription:     "THE-ASK",
		PriorWork:           "PRIOR-WORK",
		ConversationHistory: "CONVERSATION-HISTORY",
	}).Text
	// Earlier turns of this conversation, then the ask, then earlier rounds
	// of THIS turn — which happened after the ask arrived. The newest thing
	// said sits nearest the model's answer.
	order(t, msg, "CONVERSATION-HISTORY", "THE-ASK", "PRIOR-WORK")
	if !strings.HasPrefix(msg, ConversationHistoryHeader) {
		t.Error("conversation history must lead the message")
	}
}

// A NOTE THE TURN HAS READ IS IN THE BRIEF, after everything the turn already
// had, each its own section. A run is a fresh session with nowhere else to
// read it, so without this a turn sent back after a person corrected it
// launches its next run on the task as it was before the correction.
func TestPhaseUserMessageCarriesTheNotesTheTurnHasRead(t *testing.T) {
	t.Parallel()
	first := SteerMessage("founder", "post to #staging, not #general")
	second := SteerMessage("founder", "and tag the release")
	msg := BuildPhaseUserMessage(UserMessage{
		TaskDescription: "THE-ASK",
		PriorWork:       "PRIOR-WORK",
		Steered:         []string{first, second},
	})
	order(t, msg.Text, "THE-ASK", "PRIOR-WORK", "post to #staging", "tag the release")
	if !msg.Valid() {
		t.Fatalf("the outline does not tile the brief: %+v", msg.Sections)
	}
	var keys []string
	for _, s := range msg.Sections {
		keys = append(keys, s.Key)
	}
	if want := []string{"task", "prior_work", "steered_1", "steered_2"}; !slices.Equal(keys, want) {
		t.Errorf("sections = %v, want %v", keys, want)
	}
	if none := BuildPhaseUserMessage(UserMessage{TaskDescription: "THE-ASK"}).Text; strings.Contains(none, "A note from") {
		t.Errorf("a brief with no note read carries one:\n%s", none)
	}
}

// A NOTE READS THE SAME WHEREVER IT LANDS. It reaches a model at the opening of
// a run of the engine's loop, in a coding run's brief, and inside a coding run
// where its CLI places a hook's context — so it names who sent it and through
// what, says it arrived while the work was under way and is the later word, and
// says nothing about where it arrived, which would be false in every place but
// one. The note itself is last and verbatim.
func TestASteerMessageIsFactualWhereverItLands(t *testing.T) {
	t.Parallel()
	msg := SteerMessage("ada", "  post to #staging, not #general  ")
	contains(t, msg,
		"## A note from ada, sent while this turn was running",
		"ada sent this through Crewlet while the work was under way",
		"the note is the later word",
		"What has already been done stays unless the note says otherwise.")
	if !strings.HasSuffix(msg, "\n\npost to #staging, not #general") {
		t.Errorf("the note is not last and verbatim:\n%s", msg)
	}
	for _, where := range []string{"between rounds", "round", "tool result", "hook"} {
		if strings.Contains(strings.ToLower(msg), where) {
			t.Errorf("the message says where it arrived (%q), which is false in every "+
				"place but one:\n%s", where, msg)
		}
	}
	anonymous := SteerMessage(" ", "x")
	contains(t, anonymous, "## A note from a person who runs this company",
		"\nA person who runs this company sent this through Crewlet")
}

func TestPhaseUserMessageCarriesTheLedgerRules(t *testing.T) {
	t.Parallel()
	withPrior := BuildPhaseUserMessage(UserMessage{
		TaskDescription: "post the summary",
		PriorWork:       "### Iteration 1\nExecute called:\n- post_message(...) → success",
	}).Text
	if !strings.HasPrefix(withPrior, "## Task\npost the summary") {
		t.Error("the ask must lead when there is no conversation history")
	}
	// The rule that actually prevents the double-post.
	contains(t, withPrior, "Already done earlier in this turn", "### Iteration 1", "ALREADY RAN")

	withHistory := BuildPhaseUserMessage(UserMessage{
		TaskDescription:     "any update?",
		ConversationHistory: "### 2026-08-20T09:30\nYou replied: shipped it",
	}).Text
	contains(t, withHistory,
		"You replied: shipped it",
		// The block is worse than absent if the model reads it as a script
		// to re-run rather than as what it already said.
		"Do not repeat a reply you already gave",
		// Across turns this warning is stronger than within one: a read
		// from last Tuesday is stale by construction.
		"may be stale", "history, not the current state", "the task wins")
	if !strings.HasSuffix(strings.TrimRight(withHistory, "\n"), "## Task\nany update?") {
		t.Error("the ask must sit last when there is no prior work")
	}
}
