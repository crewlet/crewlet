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

func TestPlanPromptCarriesCatalogueAndIdentity(t *testing.T) {
	t.Parallel()
	p := BuildPlan(engineer(), PlanInput{ToolCatalogue: "- foo: Does foo.\n- bar: Does bar."})
	contains(t, p, "PLAN phase", "## Available tools", "- foo: Does foo.", "Engineer", "Acme")
}

// The originating-channel rule earned its place: a tracker-create task
// triggered from chat ended up with no reply tool in tools_needed. Execute
// hit an unresolvable assignee, had no way to ask, and the requester got
// silence. The rule must state the WHY, or a planner cannot generalise it to
// a novel case.
func TestPlanPromptRequiresOriginatingChannelReplyTool(t *testing.T) {
	t.Parallel()
	p := BuildPlan(engineer(), PlanInput{})
	contains(t, p,
		"originating channel's reply tool",
		"ask follow-up questions",
		"confirm completion",
		// Additive to the action tool, not a replacement for it.
		"in addition to",
		// Described by capability, never by a vendor tool name.
		"post/reply tool",
	)
	excludes(t, p, "slack_conversations_add_message", "jira_add_comment")
}

// skip means "nobody was asking me", not "I was asked and I am ignoring it".
// Without this rule a planner silently skips a direct ping and the requester
// gets nothing back.
func TestPlanPromptRequiresVerboseDeclineForDirectAsk(t *testing.T) {
	t.Parallel()
	p := BuildPlan(engineer(), PlanInput{})
	contains(t, p,
		`means "nobody was actually asking me to do anything"`,
		"do NOT use `skip`",
		"@mentioned",
		`decision="plan"`,
		"originating channel's reply tool",
		"closes the loop",
	)
}

// Production failure: routed comment triggers had the planner list recon
// tools only, Execute wrote "I don't have write tools available" as text, and
// the turn ended having delivered nothing.
func TestPlanPromptCallsOutTheFullArcRule(t *testing.T) {
	t.Parallel()
	p := BuildPlan(engineer(), PlanInput{})
	contains(t, p,
		"Plan the full arc",
		// By capability, not by vendor tool name.
		"reply, status-change, and post tools",
		// Review is not optional, so there is no token saving in opting out.
		"engine-enforced",
		"self_iterate",
		// The verb list must cover the routed-event vocabulary that the
		// original write-action verbs missed.
		"'review'", "'respond'",
	)
	excludes(t, p, "`jira_add_comment`", "`jira_transition_issue`")
}

func TestPlanPromptInlinesFullPolicies(t *testing.T) {
	t.Parallel()
	p := BuildPlan(engineer(), PlanInput{})
	contains(t, p, "## Company policies", "Respect teammates.")

	// No truncation, no "..." suffix, however long the policy runs.
	long := "Use the wiki to document architecture decisions, runbooks, " +
		"and meeting notes — search it before creating new docs."
	o := acme()
	o.Policies = []string{long}
	contains(t, BuildPlan(seatIn(o, "Engineer"), PlanInput{}), long)
}

func TestPlanPromptRostersOnlyForLeads(t *testing.T) {
	t.Parallel()
	contains(t, BuildPlan(lead(), PlanInput{}), "## Your Team", "Engineer")
	excludes(t, BuildPlan(engineer(), PlanInput{}), "## Your Team")
}

// A lead's roster inlines each member's profile so the lead can reason about
// assignment without a knowledge fetch.
func TestPlanPromptRosterInlinesMemberProfiles(t *testing.T) {
	t.Parallel()
	p := BuildPlan(lead(), PlanInput{})
	contains(t, p, "## Your Team", "**Engineer**",
		"Goal: Ship quality code.", "Responsibilities: Write tests.")
}

func TestPlanPromptModelSplitAddsRichPlanHint(t *testing.T) {
	t.Parallel()
	excludes(t, BuildPlan(engineer(), PlanInput{}), "cheaper model")
	contains(t, BuildPlan(engineer(), PlanInput{ModelSplitEnabled: true}), "cheaper model")
}

func TestPlanPromptSandboxSectionIsGatedOnTheRole(t *testing.T) {
	t.Parallel()
	// Absent for the ~all roles that are not sandbox-enabled, so it never
	// bloats the common Plan prompt.
	excludes(t, BuildPlan(engineer(), PlanInput{}), "Sandbox code work", "run_sandbox")

	o := acme()
	o.Role("Engineer").Sandbox = &org.RoleSandbox{Enabled: true, CodingAgent: "claude-code"}
	p := BuildPlan(seatIn(o, "Engineer"), PlanInput{})
	// Code work is the run_sandbox tool: plan it in tools_needed together
	// with the tool that reports the result, and the executor continues the
	// same turn once it returns.
	contains(t, p, "run_sandbox", "tools_needed", "resumes automatically")
}

func TestPlanPromptOmitsTheLearnedSkillsScaffold(t *testing.T) {
	t.Parallel()
	// Synthesized skills arrive as a Plan-phase prefetch block, never as a
	// section of the static identity scaffold.
	excludes(t, BuildPlan(engineer(), PlanInput{}), "## Your Skills")
}

func TestPlanPromptRendersOrgAndRoleContextInline(t *testing.T) {
	t.Parallel()
	p := BuildPlan(engineer(), PlanInput{})
	contains(t, p,
		"## Company Context",
		"Build great things.", // mission
		"Be the best.",        // vision
		"## Your Responsibilities", "- Write tests.",
		"## Behavioral Guidelines", "- Be concise.",
		"## Your Unit (Eng Team)", "**Purpose:** Build the thing.", "- Ship v1.0.",
		// The planner has all of it inline — no knowledge-fetch pointer.
		"Mission, vision, policies",
	)
}

func TestPlanPromptDropsEmptySections(t *testing.T) {
	t.Parallel()
	o := acme()
	o.Mission, o.Vision = "", ""
	role := o.Role("Engineer")
	role.Backstory = ""
	role.Responsibilities = nil
	role.BehavioralGuidelines = nil
	p := BuildPlan(seatIn(o, "Engineer"), PlanInput{})
	// Empty bullet lists are visual noise; a section with nothing in it is
	// dropped whole.
	excludes(t, p, "## Company Context", "## Your Background",
		"## Your Responsibilities", "## Behavioral Guidelines")
}

func TestPlanPromptPrefetchBlocks(t *testing.T) {
	t.Parallel()
	in := PlanInput{
		PersonalMemory:      "- prefers short replies",
		SynthesizedSkills:   "- how to triage a sev-1",
		RelevantKnowledge:   "- **Incident Response Runbook**: Steps for a sev-1.",
		EpisodeRecall:       "- last month's latency spike",
		CounterpartyProfile: "Subject: U0TESTUSER1",
	}
	p := BuildPlan(engineer(), in)
	// Ordered so the planner reads memory, then what it learned, then the
	// team's docs, then prior work, then who it is talking to.
	//
	// Anchored on the section break rather than the bare heading: PlanHeader
	// itself mentions `## Relevant knowledge` in prose, and matching that
	// would have this assertion pass on a prompt that renders no block at
	// all.
	order(t, p,
		"\n## Personal memory\n",
		"\n## Synthesized skills you've learned\n",
		"\n## Relevant knowledge\n",
		"\n## Similar prior work\n",
		"\n## Known counterparty\n",
	)
	// Each block is dropped whole when empty — no stub headings.
	excludes(t, BuildPlan(engineer(), PlanInput{}),
		"\n## Personal memory\n", "\n## Synthesized skills you've learned\n",
		"\n## Relevant knowledge\n", "\n## Similar prior work\n", "\n## Known counterparty\n")
}

// The onboarding hint gates on the tool as well as the marker: telling an
// agent to call mark_onboarded when reflection is disabled and the builtin is
// not registered sends it hunting for a tool that does not exist.
func TestPlanPromptOnboardingHintGatesOnTheTool(t *testing.T) {
	t.Parallel()
	hint := "Read the 'Onboarding' pages on your chain."
	withTool := BuildPlan(engineer(), PlanInput{
		OnboardingHint: hint, AvailableTools: []string{"mark_onboarded"},
	})
	contains(t, withTool, "## First-turn onboarding", hint)

	excludes(t, BuildPlan(engineer(), PlanInput{OnboardingHint: hint}),
		"## First-turn onboarding")
	excludes(t, BuildPlan(engineer(), PlanInput{AvailableTools: []string{"mark_onboarded"}}),
		"## First-turn onboarding")
}

// The Plan contract steers by capability, never by a vendor tool name — the
// engine must not re-couple itself to one tool stack.
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
	excludes(t, BuildPlan(engineer(), PlanInput{}), forbiddenToolNames...)
	excludes(t, BuildReview(engineer(), ReviewInput{}), forbiddenToolNames...)
}

// Config-derived identity (a unit's configured chat channel) is data and may
// appear; the engine-authored contracts must not name a product.
func TestPhaseContractHeadersNameNoSpecificPlatforms(t *testing.T) {
	t.Parallel()
	contains(t, PlanHeader, "originating channel's reply tool", "post/reply tool")
	for _, product := range []string{"Slack", "Jira", "Confluence", "Copilot", "GitHub"} {
		if strings.Contains(PlanHeader, product) {
			t.Errorf("PlanHeader names %q", product)
		}
		if strings.Contains(ReviewHeader, product) {
			t.Errorf("ReviewHeader names %q", product)
		}
	}
}
