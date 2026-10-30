package prompts

import (
	"slices"
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

func TestExecutorPromptCarriesTheToolsToDiscoverAndIdentity(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{
		Discoverable: "- MCP server `github`\n- MCP server `atlassian`",
	}).Text
	contains(t, p, "## Your turn", "\n## Tools to discover\n- MCP server `github`",
		"Engineer", "Acme")
	// NOT "Available tools": that is the heading a text-mode prompt gives the
	// JSON definitions of the tools the seat DOES hold, in the same
	// conversation, and two sections under one heading saying different
	// things is a model reading whichever it saw last.
	excludes(t, p, "## Available tools")
	// Nothing to discover is no section at all, not a heading over nothing.
	excludes(t, BuildExecutor(engineer(), ExecutorInput{Discoverable: " \n"}).Text,
		"## Tools to discover")
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

// Discovery is what stops an executor giving up when the tool it needs is not
// already on its surface. ONE SENTENCE: the two tools' own descriptions say
// how each works, and what the header adds is the turn's rule — discover
// before reporting that you cannot act.
func TestExecutorPromptTeachesDiscovery(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{}).Text
	contains(t, p,
		"`list_mcp_server_tools`, then `activate_tool`",
		"before reporting that you cannot act",
	)
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

// THE SANDBOX SECTION FOLLOWS THE TOOL, NOT THE ROLE'S GATE.
//
// It used to be rendered for every seat whose gate was enabled, which an
// agent-mode executor's seat may be — and that seat is never offered
// run_sandbox, so its coding CLI was taught a tool its bridge never lists,
// every turn. The caller says whether the tool is offered; the gate alone
// renders nothing.
func TestExecutorPromptSandboxSectionIsGatedOnTheOfferedTool(t *testing.T) {
	t.Parallel()
	// Absent for the ~all seats that are not offered the tool, so it never
	// bloats the common prompt.
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}).Text, "Sandbox code work", "run_sandbox")

	o := acme()
	o.Role("eng").Sandbox = &org.RoleSandbox{Enabled: true, CodingAgent: "claude-code"}
	excludes(t, BuildExecutor(seatIn(o, "eng"), ExecutorInput{}).Text, "Sandbox code work", "run_sandbox")

	p := BuildExecutor(seatIn(o, "eng"), ExecutorInput{OffersSandbox: true}).Text
	// The run is detached and the SAME turn resumes with the result, which
	// is what stops the model ending the turn to "report later".
	contains(t, p, "run_sandbox", "resumes", "SAME turn")
	// And it no longer talks about planning the arc in a tools_needed list
	// that does not exist.
	excludes(t, p, "tools_needed")
}

// THE CODE GUIDANCE NEEDS THE CODE TOOLS: it is rendered for a seat that holds
// every code tool its claims rest on — a company hosting code registers them
// together — and for no other, so no prompt sends a seat to tools its surface
// does not carry, nor bills a company that hosts no code for the sentence on
// every turn.
//
// Mutation: the gate dropped (rendered unconditionally), or read as "any one
// of them" → red.
func TestTheCodeHistoryGuidanceNeedsACodeTool(t *testing.T) {
	t.Parallel()
	const heading = "## Reading the company's code"
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}).Text, heading, "code_*")
	excludes(t, BuildExecutor(engineer(), ExecutorInput{
		AvailableTools: []string{"submit_work", "run_sandbox"}}).Text, heading)

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

// A BOX'S CLONE IS BLOBLESS, AND THE CODE GUIDANCE SAYS SO where the seat
// holds both halves of the choice it closes: the code tools, and run_sandbox,
// whose `repos` is offered with them. Inside the code_history section, so the
// outline keeps it with the rule it qualifies; absent where either is missing
// — to a seat with no sandbox it is prose about a box it never gets, and with
// no code tools there is no section for it to close.
//
// Mutations: the OffersSandbox gate dropped → the no-sandbox prompt says it,
// red; the sentence opened as a section of its own, or put in the sandbox
// section → the code_history slice lacks it, red.
func TestTheCodeHistoryGuidanceSaysABoxCloneIsBlobless(t *testing.T) {
	t.Parallel()
	held := append([]string{"submit_work", "run_sandbox"}, CodeHistoryTools()...)
	p := BuildExecutor(engineer(), ExecutorInput{AvailableTools: held, OffersSandbox: true})
	section, ok := slice(p, KeyCodeHistory)
	if !ok {
		t.Fatalf("no %q section rendered", KeyCodeHistory)
	}
	contains(t, section, "`run_sandbox` clones is blobless",
		"its history is for building, not for answering questions")

	excludes(t, BuildExecutor(engineer(), ExecutorInput{AvailableTools: held}).Text, "blobless")
	excludes(t, BuildExecutor(engineer(), ExecutorInput{
		AvailableTools: []string{"submit_work", "run_sandbox"}, OffersSandbox: true}).Text, "blobless")
}

// THE CODE WRITES GUIDANCE NEEDS THE CODE WRITES: it is rendered for a seat
// that holds every write its claims rest on — the code host's writes are
// registered together — and for no other, so no prompt sends a seat to commit
// with a tool its surface does not carry; a seat that only reads code is told
// how to read it and nothing about changing it.
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
// seat that holds every gated tool: the review's sandbox rule and `a2a_ask`,
// and the executor's escalation, workers, sandbox and tools to discover
// (referenceExecutor). An ungated review is the smallest one there is, and a
// comparison made on it held for a review that had grown every rule it can
// render.
func TestTheReviewPromptIsSmallerThanTheExecutors(t *testing.T) {
	t.Parallel()
	s := lead()
	exec := BuildExecutor(s, referenceExecutor(t)).Text
	review := BuildReview(s, ReviewInput{OffersSandbox: true, OffersA2A: true}).Text
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
	excludes(t, BuildReview(engineer(), ReviewInput{OffersSandbox: true, OffersA2A: true}).Text,
		forbiddenToolNames...)
}

// Config-derived identity (a unit's configured chat channel) is data and may
// appear; the engine-authored contracts must not name a product.
func TestPhaseContractHeadersNameNoSpecificPlatforms(t *testing.T) {
	t.Parallel()
	contains(t, ExecutorHeader, "originating channel's reply tool")
	// The review header with every gated rule rendered, so none hides a name.
	review := reviewHeader(ReviewInput{OffersSandbox: true, OffersA2A: true})
	for _, product := range []string{"Slack", "Jira", "Confluence", "Copilot", "GitHub"} {
		if strings.Contains(ExecutorHeader, product) {
			t.Errorf("ExecutorHeader names %q", product)
		}
		if strings.Contains(review, product) {
			t.Errorf("the review header names %q", product)
		}
	}
}
