package prompts

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// contains fails unless every want appears in got, naming the first that does
// not; the prompt is not printed, since the missing phrase is the finding.
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
// holds reaches its harness over the bridge from launch, with each server's
// MCP instructions: a section naming them pays twice for what every request
// carries, and one to discover them sends the model to tools no executor holds.
func TestExecutorPromptListsNoToolAndCarriesItsIdentity(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{
		AvailableTools: []string{"submit_work", "post_message", "create_issue"},
	}).Text
	contains(t, p, "## Your turn", "Engineer", "Acme")
	excludes(t, p, "## Tools to discover", "## Available tools", "MCP server `github`",
		"create_issue")
}

// THE ACT RULE: the failure it prevents is a turn ending on a composed reply
// nobody was sent.
func TestExecutorPromptSaysWritingIsNotDoing(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{}).Text
	contains(t, p,
		"writing about an action does not perform it",
		"`submit_work` exactly once",
	)
}

// THE SEARCH SENTENCE stops an executor giving up on a tool its harness shows
// only once the model looks for it. ONE SENTENCE, naming no engine tool: how to
// look is the harness's to teach, and the discovery pair is on no executor's
// surface.
func TestExecutorPromptSaysToSearchBeforeGivingUp(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{}).Text
	contains(t, p,
		"search your tools for it",
		"before reporting that you cannot act",
	)
	excludes(t, p, "list_mcp_server_tools", "activate_tool")
	// No declared tool list, no warning about one, no vendor tool channel.
	excludes(t, p, "tools_needed", "Heads-up", "tools=[", "next message")
}

// The originating-channel rule prevents a requester's silence when the work
// stalls or lands elsewhere (a work item filed is not the reply). It must state
// the WHY, or a model cannot generalise it to a novel case.
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

// A declined direct ask still gets a reply (the decline IS the reply), and the
// header says where it goes. What `no_action` means is the outcome field's to
// say (runner's schema), never a second copy here.
func TestExecutorPromptRequiresADeclineOnTheOriginatingChannel(t *testing.T) {
	t.Parallel()
	contains(t, ExecutorHeader,
		"decline in one line through the originating channel's reply tool",
		"naming the right owner")
	excludes(t, ExecutorHeader, "no_action")
}

// The failure it prevents: a turn that gathers data, then ends saying in text
// that it has no write tools.
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

// ONE COPY OF EACH RULE. A submission field's facts are its own description,
// beside it on every round; the header carries the TURN's rules, and no
// sentence saying the mission or roster is present.
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

// A lead's roster inlines each member's profile, so assignment needs no fetch.
func TestExecutorPromptRosterInlinesMemberProfiles(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(lead(), ExecutorInput{}).Text
	contains(t, p, "## Your Team", "**Engineer**",
		"Goal: Ship quality code.", "Responsibilities: Write tests.")
}

// THE CODE GUIDANCE NEEDS THE CODE TOOLS: rendered only for a seat holding
// every code tool it rests on (registered together), so no prompt sends a seat
// to tools it lacks or bills a company hosting no code for it.
//
// Mutation: the gate dropped (rendered unconditionally), or read as "any one
// of them" → red.
func TestTheCodeHistoryGuidanceNeedsACodeTool(t *testing.T) {
	t.Parallel()
	const heading = "## Reading the company's code"
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}).Text, heading, "code_*")
	excludes(t, BuildExecutor(engineer(), ExecutorInput{
		AvailableTools: []string{"submit_work", "delegate"}}).Text, heading)

	held := append([]string{"submit_work"}, CodeHistoryTools()...)
	p := BuildExecutor(engineer(), ExecutorInput{AvailableTools: held})
	contains(t, p.Text, heading, "`code_*` tools", "by commit id", "never from a clone",
		"what changed since your review are read with `code_get_change`")
	if _, ok := slice(p, KeyCodeHistory); !ok {
		t.Errorf("the guidance is not its own %q section of the outline", KeyCodeHistory)
	}
	for _, missing := range CodeHistoryTools() {
		var some []string
		for _, name := range held {
			if name != missing {
				some = append(some, name)
			}
		}
		excludes(t, BuildExecutor(engineer(), ExecutorInput{AvailableTools: some}).Text, heading)
	}
}

// THE CODE WRITES GUIDANCE NEEDS THE CODE WRITES: rendered only for a seat
// holding every Gitlet write it rests on (registered together), so no seat is
// sent to commit with a tool it lacks; a read-only seat learns only to read.
//
// Mutation: the gate dropped (rendered unconditionally), or read as "any one
// of them" → red.
func TestTheCodeWritesGuidanceNeedsTheCodeWrites(t *testing.T) {
	t.Parallel()
	const heading = "## Changing the company's code"
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}).Text, heading, "code_commit_files")
	reads := append([]string{"submit_work"}, CodeHistoryTools()...)
	excludes(t, BuildExecutor(engineer(), ExecutorInput{AvailableTools: reads}).Text, heading)

	held := append(slices.Clone(reads), CodeWriteTools()...)
	p := BuildExecutor(engineer(), ExecutorInput{AvailableTools: held})
	contains(t, p.Text, heading, "`code_commit_files`", "belongs in a checkout", "`<your handle>.<name>`")
	if _, ok := slice(p, KeyCodeWrites); !ok {
		t.Errorf("the guidance is not its own %q section of the outline", KeyCodeWrites)
	}
	for _, missing := range CodeWriteTools() {
		var some []string
		for _, name := range held {
			if name != missing {
				some = append(some, name)
			}
		}
		excludes(t, BuildExecutor(engineer(), ExecutorInput{AvailableTools: some}).Text, heading)
	}
}

func TestExecutorPromptOmitsTheLearnedSkillsScaffold(t *testing.T) {
	t.Parallel()
	// Synthesized skills arrive as a prefetch block, never in the static
	// scaffold.
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
	// No sentence telling the model what it can already read.
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
	// A section with nothing in it is dropped whole.
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
	// The thread leads, being the trigger's own context rather than
	// something retrieved about it. EVERY BLOCK IS AN ANCHOR: a subset says
	// nothing of a block appended between two it names. Anchored on the
	// section break, since ExecutorHeader mentions `## Relevant knowledge` in
	// prose and the bare heading would match a prompt rendering no block.
	order(t, p,
		"\n## The thread so far\n",
		"\n## Personal memory\n",
		"\n## Synthesized skills you've learned\n",
		"\n## Relevant knowledge\n",
		"\n## Similar prior work\n",
		"\n## Known counterparty\n",
	)
	// NOT `## Thread context`: the chat prompt titles its section in the
	// user message so, and two sections under one heading leave the model
	// reading whichever it saw last.
	excludes(t, p, "## Thread context")
	// Each block is dropped whole when empty.
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}).Text,
		"\n## The thread so far\n",
		"\n## Personal memory\n", "\n## Synthesized skills you've learned\n",
		"\n## Relevant knowledge\n", "\n## Similar prior work\n", "\n## Known counterparty\n")
}

// THE CONTRACT IS THE SAME WHATEVER WAS PREFETCHED: the block's heading says
// what it is, and a note about it would be false when the search found nothing
// and would move between two turns of one seat.
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

// Both identity renderings phrase a top-level seat the same way, so an agent
// never reads its own chart differently from its reviewer.
func TestIdentityLineAndSectionAgreeOnATopLevelSeat(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{{Name: "Engineer", Goal: "Ship."}}}
	o.Normalize()
	s := seatIn(o, "engineer")
	contains(t, BuildExecutor(s, ExecutorInput{}).Text, "None (top-level)")
	contains(t, BuildReview(s, ReviewInput{}).Text, "None (top-level)")
}

// THE REVIEW IS THE SMALLER PROMPT of one seat's turn (a one-line identity and
// its rules, never the executor's scaffold); the turn's budget counts the
// roster once on that premise. Measured with EVERY GATE ON in both (the
// review's `a2a_ask`, referenceExecutor's), since an ungated review is the
// smallest there is and would prove nothing.
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

// forbiddenToolNames are vendor tool names: the contracts steer by capability,
// never coupling the engine to one tool stack.
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
