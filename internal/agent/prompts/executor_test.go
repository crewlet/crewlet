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

// THE EXECUTOR LISTS NO TOOL AND NO SERVER TO DISCOVER. Every tool its seat
// holds is on its surface from its launch, each one's definition reaching its
// harness over the bridge under the server it comes from, with what that
// server is for as the server's own MCP instructions; and its harness keeps a
// large server's tools out of the requests that do not need them itself. A
// section naming them would be the prompt paying for what each request
// already carries, and a section to discover them would send the model to
// tools no executor holds.
func TestExecutorPromptListsNoToolAndCarriesItsIdentity(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{
		AvailableTools: []string{"submit_work", "post_message", "create_issue"},
		MCPServers:     []string{"github"},
	}).Text
	contains(t, p, "## Your turn", "Engineer", "Acme")
	excludes(t, p, "## Tools to discover", "## Available tools", "MCP server `github`",
		"create_issue")
}

// THE ACT RULE. A turn that ended with a composed reply nobody was sent is
// the failure this sentence exists for, and it is carried verbatim from the
// Execute contract it replaces.
func TestExecutorPromptSaysWritingIsNotDoing(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{}).Text
	contains(t, p,
		"writing about an action does not perform it",
		"`submit_work` exactly once",
	)
}

// THE SEARCH SENTENCE is what stops an executor giving up when the tool it
// needs is not already in front of it: every tool its seat holds is its own,
// but its harness shows a large server's tools only once its model looks for
// them. ONE SENTENCE, naming no engine tool — how to look is the harness's own
// to teach (its tool search, Code Mode's `search()`), and the discovery pair
// the engine used to name is on no executor's surface.
func TestExecutorPromptSaysToSearchBeforeGivingUp(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{}).Text
	contains(t, p,
		"search your tools for it",
		"before reporting that you cannot act",
	)
	excludes(t, p, "list_mcp_server_tools", "activate_tool")
	// Nothing survives of the reconciliation the two-phase engine needed — no
	// declared tool list, no phantom warning about one — nor of the
	// API-era wording that described a vendor's tool channel this engine no
	// longer has.
	excludes(t, p, "tools_needed", "Heads-up", "tools=[", "next message")
}

// The originating-channel rule earned its place: a tracker-create task
// triggered from chat ended up with no way to reach the requester. The
// executor hit an unresolvable assignee, had no way to ask, and the requester
// got silence. The rule must state the WHY, or a model cannot generalise it
// to a novel case.
//
// And the work filed elsewhere is not the reply: a founder's direct message
// was once "answered" with a work item they never heard about.
func TestExecutorPromptRequiresStayingReachable(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{}).Text
	contains(t, p,
		"Stay reachable on the channel the trigger arrived on",
		"filing the ticket is the work, and the requester still has to be told",
		"Ask a follow-up",
		"confirm completion",
		"the requester gets silence",
	)
	excludes(t, p, "slack_conversations_add_message", "jira_add_comment")
}

// A direct ask that the seat declines still gets a reply — the decline IS the
// reply — and the header says where it goes. What `no_action` MEANS is the
// outcome field's to say, beside the field (runner's schema), and the decoder
// refuses it on a turn somebody is waiting on: a second copy here was the
// same rule written twice, for two texts to drift apart.
func TestExecutorPromptRequiresADeclineOnTheOriginatingChannel(t *testing.T) {
	t.Parallel()
	contains(t, ExecutorHeader,
		"decline in one line through the originating channel's reply tool",
		"naming the right owner")
	excludes(t, ExecutorHeader, "no_action")
}

// Production failure: routed comment triggers produced recon only, and the
// turn ended with the model writing "I don't have write tools" as text.
func TestExecutorPromptCallsOutTheFullArcRule(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{}).Text
	contains(t, p,
		"Finish the arc in this pass",
		// By capability, not by vendor tool name.
		"the reply, the comment, the status change, the create",
		"gathers data and stops has delivered nothing",
	)
	excludes(t, p, "`jira_add_comment`", "`jira_transition_issue`")
}

// ONE COPY OF EACH RULE. A fact about a submission field — that `deliveries`
// is checked against the engine's record, what an outcome means — is the
// field's own description, which reaches the model beside the field on every
// round; the header carries the rules about the TURN. And it says nothing the
// prompt already shows: the mission and the roster need no sentence saying
// they are present.
func TestTheExecutorContractLeavesFieldRulesToTheSchema(t *testing.T) {
	t.Parallel()
	excludes(t, ExecutorHeader,
		"`deliveries`", "no_action",
		"already in this prompt", "Mission, vision",
		"A reviewer runs after you")
}

func TestExecutorPromptInlinesFullPolicies(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{}).Text
	contains(t, p, "## Company policies", "Respect teammates.")

	// No truncation, no "..." suffix, however long the policy runs.
	long := "Use the wiki to document architecture decisions, runbooks, " +
		"and meeting notes — search it before creating new docs."
	o := acme()
	o.Policies = []string{long}
	contains(t, BuildExecutor(seatIn(o, "eng"), ExecutorInput{}).Text, long)
}

func TestExecutorPromptRostersOnlyForLeads(t *testing.T) {
	t.Parallel()
	contains(t, BuildExecutor(lead(), ExecutorInput{}).Text, "## Your Team", "Engineer")
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}).Text, "## Your Team")
}

// A lead's roster inlines each member's profile so the lead can reason about
// assignment without a knowledge fetch.
func TestExecutorPromptRosterInlinesMemberProfiles(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(lead(), ExecutorInput{}).Text
	contains(t, p, "## Your Team", "**Engineer**",
		"Goal: Ship quality code.", "Responsibilities: Write tests.")
}

func TestExecutorPromptOmitsTheLearnedSkillsScaffold(t *testing.T) {
	t.Parallel()
	// Synthesized skills arrive as a prefetch block, never as a section of
	// the static identity scaffold.
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}).Text, "## Your Skills")
}

func TestExecutorPromptRendersOrgAndRoleContextInline(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{}).Text
	contains(t, p,
		"## Company Context",
		"Build great things.", // mission
		"Be the best.",        // vision
		"## Your Responsibilities", "- Write tests.",
		"## Behavioral Guidelines", "- Be concise.",
		"## Your Unit (Eng Team)", "**Purpose:** Build the thing.", "- Ship v1.0.",
	)
	// It has all of it inline — and no knowledge-fetch pointer, nor a sentence
	// telling the model what it can already read.
	excludes(t, p, "no lookup needed")
}

func TestExecutorPromptDropsEmptySections(t *testing.T) {
	t.Parallel()
	o := acme()
	o.Mission, o.Vision = "", ""
	role := o.Role("eng")
	role.Backstory = ""
	role.Responsibilities = nil
	role.BehavioralGuidelines = nil
	p := BuildExecutor(seatIn(o, "eng"), ExecutorInput{}).Text
	// Empty bullet lists are visual noise; a section with nothing in it is
	// dropped whole.
	excludes(t, p, "## Company Context", "## Your Background",
		"## Your Responsibilities", "## Behavioral Guidelines")
}

func TestExecutorPromptPrefetchBlocks(t *testing.T) {
	t.Parallel()
	in := ExecutorInput{
		ThreadContext:       "- **Ana Ruiz (ana)**: staging redirects in a loop",
		PersonalMemory:      "- prefers short replies",
		SynthesizedSkills:   "- how to triage a sev-1",
		RelevantKnowledge:   "- **Incident Response Runbook**: Steps for a sev-1.",
		EpisodeRecall:       "- last month's latency spike",
		CounterpartyProfile: "Subject: U0TESTUSER1",
	}
	p := BuildExecutor(engineer(), in).Text
	// Ordered so the agent reads the conversation it was woken in, then
	// memory, then what it learned, then the team's docs, then prior work,
	// then who it is talking to. The thread leads because it is the
	// trigger's own context rather than something retrieved about it — and
	// it led in the prose before it led in the prompt, with another block
	// appended above it.
	//
	// EVERY BLOCK IS AN ANCHOR. A subset pins the blocks it names and says
	// nothing about one appended between two of them, which is exactly how
	// another block once came to render first while the comment beside it
	// and the docs both said the thread did.
	//
	// Anchored on the section break rather than the bare heading:
	// ExecutorHeader itself mentions `## Relevant knowledge` in prose, and
	// matching that would have this assertion pass on a prompt that renders
	// no block at all.
	order(t, p,
		"\n## The thread so far\n",
		"\n## Personal memory\n",
		"\n## Synthesized skills you've learned\n",
		"\n## Relevant knowledge\n",
		"\n## Similar prior work\n",
		"\n## Known counterparty\n",
	)
	// AND IT IS NOT CALLED `## Thread context`, which is what the chat
	// prompt titles the section it writes into the notification BODY — and
	// that lands in the USER message of this same conversation. Two sections
	// under one heading saying different things is a model reading whichever
	// it saw last.
	excludes(t, p, "## Thread context")
	// Each block is dropped whole when empty — no stub headings.
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}).Text,
		"\n## The thread so far\n",
		"\n## Personal memory\n", "\n## Synthesized skills you've learned\n",
		"\n## Relevant knowledge\n", "\n## Similar prior work\n", "\n## Known counterparty\n")
}

// THE CONTRACT IS THE SAME WHATEVER WAS PREFETCHED. It used to carry a note
// saying team documentation had been surfaced whenever the knowledge block was
// present — which was false whenever that block was the prefetch's own hint
// that the search found nothing, and redundant otherwise, since the block's
// heading says what it is. A contract that changes with the prefetch is also
// one sentence more that moves between two turns of one seat.
func TestTheContractIsTheSameWhateverWasPrefetched(t *testing.T) {
	t.Parallel()
	with := BuildExecutor(engineer(), ExecutorInput{RelevantKnowledge: "- **Runbook**: steps."})
	without := BuildExecutor(engineer(), ExecutorInput{})
	a, okA := slice(with, "your_turn")
	b, okB := slice(without, "your_turn")
	if !okA || !okB {
		t.Fatal("no your_turn section — this case compares nothing")
	}
	if a != b {
		t.Errorf("the contract changed with the knowledge block:\nwith:    %q\nwithout: %q", a, b)
	}
	excludes(t, with.Text, "was surfaced")
}

// Both identity renderings must phrase a top-level seat the same way. They
// diverged once — "none" in one, "None (top-level)" in the other — and an
// agent reading its own chart differently from its reviewer is a difference
// no phase assertion is looking for.
func TestIdentityLineAndSectionAgreeOnATopLevelSeat(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{{Name: "Engineer", Goal: "Ship."}}}
	o.Normalize()
	s := seatIn(o, "engineer")
	contains(t, BuildExecutor(s, ExecutorInput{}).Text, "None (top-level)")
	contains(t, BuildReview(s, ReviewInput{}).Text, "None (top-level)")
}

// THE REVIEW IS THE SMALLER PROMPT of one seat's turn, because it renders a
// one-line identity and its rules and never the executor's scaffold — the
// whole turn's budget counts the roster once on that premise.
//
// Measured with EVERY GATE ON in both, the way production sends them to a
// seat that holds every gated tool: the review's `a2a_ask`, and the
// executor's escalation and workers (referenceExecutor). An ungated review is the smallest one there is, and a
// comparison made on it held for a review that had grown every rule it can
// render.
func TestTheReviewPromptIsSmallerThanTheExecutors(t *testing.T) {
	t.Parallel()
	s := lead()
	exec := BuildExecutor(s, referenceExecutor(t)).Text
	review := BuildReview(s, ReviewInput{OffersA2A: true}).Text
	if len(review) >= len(exec) {
		t.Errorf("review prompt (%d) is not smaller than the executor's (%d)",
			len(review), len(exec))
	}
}

// The contracts steer by capability, never by a vendor tool name. The engine
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
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}).Text, forbiddenToolNames...)
	excludes(t, BuildReview(engineer(), ReviewInput{OffersA2A: true}).Text,
		forbiddenToolNames...)
}

// Config-derived identity (a unit's configured chat channel) is data and may
// appear; the engine-authored contracts must not name a product.
func TestPhaseContractHeadersNameNoSpecificPlatforms(t *testing.T) {
	t.Parallel()
	contains(t, ExecutorHeader, "originating channel's reply tool")
	// The review header with every gated rule rendered, so none hides a name.
	review := reviewHeader(ReviewInput{OffersA2A: true})
	for _, product := range []string{"Slack", "Jira", "Confluence", "Copilot", "GitHub"} {
		if strings.Contains(ExecutorHeader, product) {
			t.Errorf("ExecutorHeader names %q", product)
		}
		if strings.Contains(review, product) {
			t.Errorf("the review header names %q", product)
		}
	}
}
