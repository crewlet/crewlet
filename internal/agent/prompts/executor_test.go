package prompts

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// contains fails unless every want appears in got, naming the first that
// does not. The prompt itself is not printed: it is thousands of characters
// and the missing phrase is the finding.
func contains(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("prompt is missing %q", w)
		}
	}
}

func excludes(t *testing.T, got string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(got, u) {
			t.Errorf("prompt contains %q and must not", u)
		}
	}
}

// order fails unless the phrases appear in the given order.
func order(t *testing.T, got string, phrases ...string) {
	t.Helper()
	prev := -1
	for _, p := range phrases {
		i := strings.Index(got, p)
		if i < 0 {
			t.Fatalf("prompt is missing %q", p)
			return
		}
		if i <= prev {
			t.Errorf("%q appears out of order", p)
		}
		prev = i
	}
}

func TestExecutorPromptCarriesCatalogueAndIdentity(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{
		ToolCatalogue: "- foo: Does foo.\n- bar: Does bar.",
	})
	contains(t, p, "## Your turn", "## Available tools", "- foo: Does foo.", "Engineer", "Acme")
}

// THE ACT RULE. A turn that ended with a composed reply nobody was sent is
// the failure this sentence exists for, and it is carried verbatim from the
// Execute contract it replaces.
func TestExecutorPromptSaysWritingIsNotDoing(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{})
	contains(t, p,
		"writing about an action does not perform it",
		"`submit_work` exactly once",
	)
}

// Discovery is what stops an executor giving up when the tool it needs is not
// already on its surface. It replaces the planner's tools_needed guess
// entirely: nothing names tools in advance any more, so the prompt has to
// teach the two-step lookup rather than mention it.
func TestExecutorPromptTeachesDiscovery(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{})
	contains(t, p,
		"`list_mcp_server_tools(server=...)`",
		"`activate_tool(name=...)`",
		"Prefer discovery over giving up",
		// Server names only, which is WHY discovery is needed at all.
		"lists those servers by NAME only",
	)
	// And nothing survives of the reconciliation the two-phase engine
	// needed: no declared tool list, and no phantom warning about one.
	excludes(t, p, "tools_needed", "Heads-up")
}

// The originating-channel rule earned its place: a tracker-create task
// triggered from chat ended up with no way to reach the requester. The
// executor hit an unresolvable assignee, had no way to ask, and the requester
// got silence. The rule must state the WHY, or a model cannot generalise it
// to a novel case.
func TestExecutorPromptRequiresStayingReachable(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{})
	contains(t, p,
		"Stay reachable on the channel the trigger arrived on",
		"ask a follow-up",
		"confirm completion",
		"the requester gets silence",
	)
	excludes(t, p, "slack_conversations_add_message", "jira_add_comment")
}

// no_action means "nobody was asking me", not "I was asked and I am ignoring
// it". Without this rule a turn silently declines a direct ping and the
// requester gets nothing back.
func TestExecutorPromptRequiresVerboseDeclineForDirectAsk(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{})
	contains(t, p,
		`means "nobody was actually asking me to do anything"`,
		"do NOT use `no_action`",
		"@mentioned",
		"originating channel's reply tool",
		"closes the loop",
	)
}

// Production failure: routed comment triggers produced recon only, and the
// turn ended with the model writing "I don't have write tools" as text.
func TestExecutorPromptCallsOutTheFullArcRule(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{})
	contains(t, p,
		"Finish the arc in this pass",
		// By capability, not by vendor tool name.
		"the reply, the comment, the status change, the create",
		"gathers data and stops has delivered nothing",
		// A second pass exists but is not free, which is what stops the
		// model treating recon-then-stop as the normal shape.
		"a second pass over work one pass should have finished",
	)
	excludes(t, p, "`jira_add_comment`", "`jira_transition_issue`")
}

// The engine checks delivery claims against its own record, and a model that
// does not know that names the tool it MEANT to call.
func TestExecutorPromptSaysDeliveriesAreChecked(t *testing.T) {
	t.Parallel()
	contains(t, BuildExecutor(engineer(), ExecutorInput{}),
		"`deliveries` must name calls you actually made",
		"checks them against its own log")
}

func TestExecutorPromptInlinesFullPolicies(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{})
	contains(t, p, "## Company policies", "Respect teammates.")

	// No truncation, no "..." suffix, however long the policy runs.
	long := "Use the wiki to document architecture decisions, runbooks, " +
		"and meeting notes — search it before creating new docs."
	o := acme()
	o.Policies = []string{long}
	contains(t, BuildExecutor(seatIn(o, "Engineer"), ExecutorInput{}), long)
}

func TestExecutorPromptRostersOnlyForLeads(t *testing.T) {
	t.Parallel()
	contains(t, BuildExecutor(lead(), ExecutorInput{}), "## Your Team", "Engineer")
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}), "## Your Team")
}

// A lead's roster inlines each member's profile so the lead can reason about
// assignment without a knowledge fetch.
func TestExecutorPromptRosterInlinesMemberProfiles(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(lead(), ExecutorInput{})
	contains(t, p, "## Your Team", "**Engineer**",
		"Goal: Ship quality code.", "Responsibilities: Write tests.")
}

func TestExecutorPromptSandboxSectionIsGatedOnTheRole(t *testing.T) {
	t.Parallel()
	// Absent for the ~all roles that are not sandbox-enabled, so it never
	// bloats the common prompt.
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}), "Sandbox code work", "run_sandbox")

	o := acme()
	o.Role("Engineer").Sandbox = &org.RoleSandbox{Enabled: true, CodingAgent: "claude-code"}
	p := BuildExecutor(seatIn(o, "Engineer"), ExecutorInput{})
	// The run is detached and the SAME turn resumes with the result, which
	// is what stops the model ending the turn to "report later".
	contains(t, p, "run_sandbox", "resumes", "SAME turn")
	// And it no longer talks about planning the arc in a tools_needed list
	// that does not exist.
	excludes(t, p, "tools_needed")
}

func TestExecutorPromptOmitsTheLearnedSkillsScaffold(t *testing.T) {
	t.Parallel()
	// Synthesized skills arrive as a prefetch block, never as a section of
	// the static identity scaffold.
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}), "## Your Skills")
}

func TestExecutorPromptRendersOrgAndRoleContextInline(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{})
	contains(t, p,
		"## Company Context",
		"Build great things.", // mission
		"Be the best.",        // vision
		"## Your Responsibilities", "- Write tests.",
		"## Behavioral Guidelines", "- Be concise.",
		"## Your Unit (Eng Team)", "**Purpose:** Build the thing.", "- Ship v1.0.",
		// It has all of it inline — no knowledge-fetch pointer.
		"Mission, vision, policies",
	)
}

func TestExecutorPromptDropsEmptySections(t *testing.T) {
	t.Parallel()
	o := acme()
	o.Mission, o.Vision = "", ""
	role := o.Role("Engineer")
	role.Backstory = ""
	role.Responsibilities = nil
	role.BehavioralGuidelines = nil
	p := BuildExecutor(seatIn(o, "Engineer"), ExecutorInput{})
	// Empty bullet lists are visual noise; a section with nothing in it is
	// dropped whole.
	excludes(t, p, "## Company Context", "## Your Background",
		"## Your Responsibilities", "## Behavioral Guidelines")
}

func TestExecutorPromptPrefetchBlocks(t *testing.T) {
	t.Parallel()
	in := ExecutorInput{
		PersonalMemory:      "- prefers short replies",
		SynthesizedSkills:   "- how to triage a sev-1",
		RelevantKnowledge:   "- **Incident Response Runbook**: Steps for a sev-1.",
		EpisodeRecall:       "- last month's latency spike",
		CounterpartyProfile: "Subject: U0TESTUSER1",
	}
	p := BuildExecutor(engineer(), in)
	// Ordered so the agent reads memory, then what it learned, then the
	// team's docs, then prior work, then who it is talking to.
	//
	// Anchored on the section break rather than the bare heading:
	// ExecutorHeader itself mentions `## Relevant knowledge` in prose, and
	// matching that would have this assertion pass on a prompt that renders
	// no block at all.
	order(t, p,
		"\n## Personal memory\n",
		"\n## Synthesized skills you've learned\n",
		"\n## Relevant knowledge\n",
		"\n## Similar prior work\n",
		"\n## Known counterparty\n",
	)
	// Each block is dropped whole when empty — no stub headings.
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}),
		"\n## Personal memory\n", "\n## Synthesized skills you've learned\n",
		"\n## Relevant knowledge\n", "\n## Similar prior work\n", "\n## Known counterparty\n")
}

// The knowledge note is a STATEMENT about the prompt, so it must only appear
// when the block it points at does. Unconditional, it was a lie on every turn
// the search was gated off — and an agent told the documentation is already
// here does not go looking for it.
func TestTheKnowledgeNoteOnlyAppearsWithABlock(t *testing.T) {
	t.Parallel()
	contains(t, BuildExecutor(engineer(), ExecutorInput{
		RelevantKnowledge: "- **Runbook**: steps.",
	}), "Relevant team documentation was surfaced")
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}),
		"Relevant team documentation was surfaced")
}

// The onboarding hint gates on the tool as well as the marker: telling an
// agent to call mark_onboarded when reflection is disabled and the builtin is
// not registered sends it hunting for a tool that does not exist.
func TestExecutorPromptOnboardingHintGatesOnTheTool(t *testing.T) {
	t.Parallel()
	hint := "Read the 'Onboarding' pages on your chain."
	withTool := BuildExecutor(engineer(), ExecutorInput{
		OnboardingHint: hint, AvailableTools: []string{"mark_onboarded"},
	})
	contains(t, withTool, "## First-turn onboarding", hint)

	excludes(t, BuildExecutor(engineer(), ExecutorInput{OnboardingHint: hint}),
		"## First-turn onboarding")
	excludes(t, BuildExecutor(engineer(), ExecutorInput{AvailableTools: []string{"mark_onboarded"}}),
		"## First-turn onboarding")
}

// Both identity renderings must phrase a top-level seat the same way. They
// diverged once — "none" in one, "None (top-level)" in the other — and an
// agent reading its own chart differently from its reviewer is a difference
// no phase assertion is looking for.
func TestIdentityLineAndSectionAgreeOnATopLevelSeat(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{{Name: "Engineer", Goal: "Ship."}}}
	o.Normalize()
	s := seatIn(o, "Engineer")
	contains(t, BuildExecutor(s, ExecutorInput{}), "None (top-level)")
	contains(t, BuildReview(s, ReviewInput{}), "None (top-level)")
}

func TestTheReviewPromptIsSmallerThanTheExecutors(t *testing.T) {
	t.Parallel()
	s := lead()
	exec := BuildExecutor(s, ExecutorInput{ToolCatalogue: "- foo: Does foo."})
	if review := BuildReview(s, ReviewInput{}); len(review) >= len(exec) {
		t.Errorf("review prompt (%d) is not smaller than the executor's (%d)",
			len(review), len(exec))
	}
}

// The contracts steer by capability, never by a vendor tool name — the engine
// must not re-couple itself to one tool stack.
var forbiddenToolNames = []string{
	"slack_conversations_add_message",
	"slack_conversations_postMessage",
	"jira_add_comment",
	"jira_transition_issue",
	"jira_update_issue",
	"jira_create_issue",
	"confluence_add_footer_comment",
	"confluence_search",
	"confluence_get_page",
	"request_copilot_review",
	"create_pull_request_with_copilot",
}

func TestPhaseContractsNameNoVendorTools(t *testing.T) {
	t.Parallel()
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}), forbiddenToolNames...)
	excludes(t, BuildReview(engineer(), ReviewInput{}), forbiddenToolNames...)
}

// Config-derived identity (a unit's configured chat channel) is data and may
// appear; the engine-authored contracts must not name a product.
func TestPhaseContractHeadersNameNoSpecificPlatforms(t *testing.T) {
	t.Parallel()
	contains(t, ExecutorHeader, "originating channel's reply tool")
	for _, product := range []string{"Slack", "Jira", "Confluence", "Copilot", "GitHub"} {
		if strings.Contains(ExecutorHeader, product) {
			t.Errorf("ExecutorHeader names %q", product)
		}
		if strings.Contains(ReviewHeader, product) {
			t.Errorf("ReviewHeader names %q", product)
		}
	}
}
