package prompts

import (
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

func TestExecutePromptIsIdentityPlusContract(t *testing.T) {
	t.Parallel()
	p := BuildExecute(lead(), ExecuteInput{})
	contains(t, p, "EXECUTE phase", "You are **Engineering Lead** at **Acme**")
	// No catalogue unless one is passed, and never a roster: the executor
	// runs a plan, it does not decide who does the work.
	excludes(t, p, "## Available tools", "## Your Team")
	// Policies and mission/vision stay in Plan — the plan's success_criteria
	// is what carries policy-driven constraints forward.
	excludes(t, p, "Company Policies", "Company policies",
		"Respect teammates.", "Build great things.")
	// One-line identity, not the full identity block.
	excludes(t, p, "# Your Identity")
}

func TestExecutePromptRendersPlanAndPrefetchBlocks(t *testing.T) {
	t.Parallel()
	p := BuildExecute(engineer(), ExecuteInput{
		PlanSummary:         "1. Do X\n2. Do Y",
		RelevantKnowledge:   "- **Incident Response Runbook**: Steps for a sev-1.",
		CounterpartyProfile: "Subject: U0TESTUSER1\nObserved by you:\n  - preferred_greeting: hey sam\n",
		ToolCatalogue:       "- foo: Does foo.",
	})
	contains(t, p, "Do X", "Do Y", "Incident Response Runbook", "hey sam")
	// Section order matches the Plan prompt's, so an executor reading both
	// finds the same things in the same places.
	order(t, p, "\n## Plan\n", "\n## Relevant knowledge\n",
		"\n## Known counterparty\n", "\n## Available tools\n")

	// Each block is dropped whole when absent — the common case for both.
	bare := BuildExecute(engineer(), ExecuteInput{PlanSummary: "do thing"})
	excludes(t, bare, "\n## Relevant knowledge\n", "\n## Known counterparty\n")
}

// The counterparty block is forwarded from the Plan prefetch because a plan
// may describe the action abstractly ("use the counterparty's preferred
// greeting format") without baking the literal greeting into the plan.
func TestExecutePromptForwardsCounterpartyProfile(t *testing.T) {
	t.Parallel()
	p := BuildExecute(engineer(), ExecuteInput{
		PlanSummary:         "Reply with the counterparty's preferred greeting.",
		CounterpartyProfile: "Observed by you:\n  - preferred_greeting: hey sam\n",
	})
	contains(t, p, "## Known counterparty", "hey sam")
}

// A planner cannot see MCP tool names, only server names, so tools_needed
// regularly carries a guess that resolves to nothing. Named explicitly, the
// executor goes and discovers the real tool; unnamed, it assumes the tool
// exists, fails to call it, and settles for a text reply that delivers
// nothing.
func TestExecutePromptWarnsAboutPhantomTools(t *testing.T) {
	t.Parallel()
	p := BuildExecute(engineer(), ExecuteInput{
		PlanSummary:  "reply to the greeting",
		PhantomTools: []string{"slack_conversations_postMessage", "jira_add_comment"},
	})
	contains(t, p, "Heads-up",
		"`slack_conversations_postMessage`, `jira_add_comment`",
		"list_mcp_server_tools", "does not deliver")

	excludes(t, BuildExecute(engineer(), ExecuteInput{PlanSummary: "x"}), "Heads-up")
}

// Both identity renderings must phrase a top-level seat the same way. They
// diverged once — "none" in one, "None (top-level)" in the other — and an
// executor reading its own chart differently from its planner is a
// difference no phase assertion is looking for.
func TestIdentityLineAndSectionAgreeOnATopLevelSeat(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{{Name: "Engineer", Goal: "Ship."}}}
	o.Normalize()
	s := seatIn(o, "Engineer")
	contains(t, BuildPlan(s, PlanInput{}), "None (top-level)")
	contains(t, BuildExecute(s, ExecuteInput{}), "None (top-level)")
	contains(t, BuildReview(s, ReviewInput{}), "None (top-level)")
}

func TestExecuteAndReviewPromptsAreSmallerThanPlan(t *testing.T) {
	t.Parallel()
	s := lead()
	plan := BuildPlan(s, PlanInput{ToolCatalogue: "- foo: Does foo."})
	for name, got := range map[string]string{
		"execute": BuildExecute(s, ExecuteInput{}),
		"review":  BuildReview(s, ReviewInput{}),
	} {
		if len(got) >= len(plan) {
			t.Errorf("%s prompt (%d) is not smaller than plan (%d)", name, len(got), len(plan))
		}
	}
}
