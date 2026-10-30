package prompts

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// The (human) marker is load-bearing: it is how an agent knows this
// colleague replies asynchronously over an external surface rather than
// running a turn.
func TestHumanSeatsAreMarkedInBothIdentityRenderings(t *testing.T) {
	t.Parallel()
	s := seatIn(mixedAcme(), "eng")
	contains(t, BuildExecutor(s, ExecutorInput{}).Text, "**Reports to:** Sarah Chen (human)")
	contains(t, BuildIdentityLine(s), "Sarah Chen (human)")
}

func TestHumanColleaguesNoteAppearsOnlyInMixedOrgs(t *testing.T) {
	t.Parallel()
	mixed := BuildExecutor(seatIn(mixedAcme(), "eng"), ExecutorInput{}).Text
	contains(t, mixed, "## Human colleagues", "never over A2A", "asynchronously",
		// Both markers, because the identity line and the roster mark a
		// person differently, and a note naming one would leave the other
		// unexplained.
		"(human)", "**human teammate**")

	// A pure-agent company's prompts are unchanged by the feature existing.
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}).Text, "## Human colleagues")
}

// A human member's roster block renders their external identities and their
// availability. How a reachable person is handed work is the `## Human
// colleagues` note's to say, ONCE: a lead of a mostly human team used to pay
// for it per profile, out of the allowance that buys profiles.
func TestRosterRendersHumanMemberBlock(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Units: []*org.Unit{{
			Name: "Eng Team",
			Type: org.UnitTypeTeam,
			Lead: "lead",
			Roles: []*org.Role{
				{Name: "Lead", DeclaredHandle: "lead"},
				{
					Name: "Sarah Chen",
					Kind: org.KindHuman,
					Contact: &org.HumanContact{
						SlackUserID:        "U0HUMAN",
						AtlassianAccountID: "5b10-s",
					},
					Availability: "CET business hours",
				},
				{Name: "Engineer", DeclaredHandle: "eng"},
			},
		}},
	}
	o.Name = "Acme"
	o.Normalize()
	p := BuildExecutor(seatIn(o, "lead"), ExecutorInput{}).Text

	contains(t, p, "**Sarah Chen** (sarah-chen) — **human teammate**")
	// Identities render generically, labelled by transport. The shared
	// Atlassian id covers both Jira and Confluence and renders ONCE, under
	// the first transport that claims it — twice would read as two
	// different accounts.
	contains(t, p, "Slack ID: U0HUMAN", "Jira ID: 5b10-s")
	excludes(t, p, "Confluence ID: 5b10-s")
	contains(t, p, "Availability: CET business hours")
	if got := strings.Count(p, "PM tool"); got != 1 {
		t.Errorf("how to hand a person work is said %d times, want once — in the note", got)
	}
	// Agent members keep the plain rendering.
	contains(t, p, "**Engineer** (eng)")
}

// An unresolved ${VAR} is omitted, never rendered verbatim: a literal
// "${SLACK_ID}" in a roster is a mention that can never match an account, and
// the failure surfaces as a person who mysteriously never gets pinged.
func TestRosterOmitsUnresolvedContactReferences(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name: "Acme",
		Units: []*org.Unit{{
			Name: "Eng Team",
			Type: org.UnitTypeTeam,
			Lead: "lead",
			Roles: []*org.Role{
				{Name: "Lead", DeclaredHandle: "lead"},
				{
					Name:    "Sarah Chen",
					Kind:    org.KindHuman,
					Contact: &org.HumanContact{SlackUserID: "${SARAH_SLACK_ID}"},
				},
			},
		}},
	}
	o.Normalize()
	seat := seatIn(o, "lead")

	excludes(t, BuildExecutor(seat, ExecutorInput{}).Text, "${SARAH_SLACK_ID}", "Slack ID:")

	seat.Env = func(name string) (string, bool) {
		return "U0RESOLVED", name == "SARAH_SLACK_ID"
	}
	contains(t, BuildExecutor(seat, ExecutorInput{}).Text, "Slack ID: U0RESOLVED")
}

// A seat missing its chart renders the phase contract and no identity,
// rather than taking the turn down: a prompt without an identity line is
// degraded, a panicking prompt builder is a dead turn.
func TestZeroSeatRendersTheContractWithoutPanicking(t *testing.T) {
	t.Parallel()
	var s Seat
	contains(t, BuildExecutor(s, ExecutorInput{}).Text, "## Your turn")
	contains(t, BuildReview(s, ReviewInput{}).Text, "## REVIEW phase")
	contains(t, BuildOnboarding(s, OnboardingInput{}).Text, "## ONBOARDING phase")
	excludes(t, BuildExecutor(s, ExecutorInput{}).Text, "## Your Identity")
}

func TestCapitalizeMatchesPythonSemantics(t *testing.T) {
	t.Parallel()
	// Upper the first rune, lower the rest — which is what renders
	// "Github ID:" rather than "GitHub ID:". Carried as-is because the
	// roster line is prompt text.
	for in, want := range map[string]string{
		"slack":  "Slack",
		"github": "Github",
		"gitlab": "Gitlab",
		"jira":   "Jira",
		"":       "",
	} {
		if got := capitalize(in); got != want {
			t.Errorf("capitalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// A PERSON WITH NO CHAT ACCOUNT HAS NOBODY TO @-MENTION — ON A VENDOR'S CHAT.
//
// A human seat can be in the chart with no contact identity at all — somebody
// who signs in to the dashboard and never joined the company's Slack — and a
// roster that told an agent to mention them would produce a message addressed
// to a handle resolving to nobody, which reads to everyone else as work handed
// over. The company below is VALIDATED first, because a roster case over a
// chart validation refuses would be certifying a state no company can be in.
// The engine's own chat is the other case, below.
func TestRosterOffersNoMentionForAPersonWithNoChatAccount(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name: "Acme",
		Units: []*org.Unit{{
			Name: "Eng Team",
			Type: org.UnitTypeTeam,
			Lead: "lead",
			Roles: []*org.Role{
				{Name: "Lead", DeclaredHandle: "lead"},
				{
					Name:    "Jane Founder",
					Kind:    org.KindHuman,
					Contact: &org.HumanContact{},
				},
			},
		}},
	}
	o.Normalize()
	if err := o.Validate(); err != nil {
		t.Fatalf("a chart holding a person with no chat account is refused: %v", err)
	}
	p := BuildExecutor(seatIn(o, "lead"), ExecutorInput{}).Text

	contains(t, p, "**Jane Founder** (jane-founder) — **human teammate**")
	excludes(t, p, "Crewlet ID:", "crewlet")
	contains(t, p, "nobody to @-mention", "assign their work to them in the PM tool")

	// A colleague WITH a reachable account is mentioned as the note says,
	// and the profile says nothing that would contradict it.
	o.Units[0].Roles[1].Contact.SlackUserID = "U0FOUNDER"
	o.Normalize()
	with := BuildExecutor(seatIn(o, "lead"), ExecutorInput{}).Text
	contains(t, with, "Slack ID: U0FOUNDER", "Reach them with an @-mention")
	excludes(t, with, "nobody to @-mention")
}

// ON THE ENGINE'S OWN CHAT, A PERSON ANSWERS TO THEIR SEAT'S HANDLE.
//
// Chatlet reads a handle: `@jane-founder` in a room reaches the person
// holding that seat, and `send_dm` opens a conversation with them, with no
// vendor account behind either. So the person with no contact identity above
// is somebody to address there — and a roster that decided reachability from
// contact identities alone told their lead there was nobody to @-mention, so
// the lead routed everything through the PM tool and never asked them anything
// in the room where they read. A person who also has a code-host account keeps
// it listed beside the handle. The profile names the handle and nothing more:
// how a person is reached and handed work is the `## Human colleagues` note's,
// once, and on Chatlet the note names the handle and `send_dm` too.
func TestRosterOffersAPersonsHandleOnChatlet(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name: "Acme",
		Units: []*org.Unit{{
			Name: "Eng Team",
			Type: org.UnitTypeTeam,
			Lead: "lead",
			Roles: []*org.Role{
				{Name: "Lead", DeclaredHandle: "lead"},
				{
					Name:    "Jane Founder",
					Kind:    org.KindHuman,
					Contact: &org.HumanContact{},
				},
			},
		}},
	}
	o.Normalize()
	if err := o.Validate(); err != nil {
		t.Fatalf("a chart holding a person with no chat account is refused: %v", err)
	}
	seat := seatIn(o, "lead")
	seat.Chatlet = true
	p := BuildExecutor(seat, ExecutorInput{}).Text

	contains(t, p, "**Jane Founder** (jane-founder) — **human teammate**",
		"  - Chat: @jane-founder", "mention a person by the @handle their profile gives",
		"`send_dm`")
	excludes(t, p, "nobody to @-mention", "Working with them")

	// AND A CODE-HOST ACCOUNT IS STILL LISTED, beside the handle.
	o.Units[0].Roles[1].Contact.GitHubLogin = "jane"
	o.Normalize()
	seat = seatIn(o, "lead")
	seat.Chatlet = true
	with := BuildExecutor(seat, ExecutorInput{}).Text
	contains(t, with, "Github ID: jane", "  - Chat: @jane-founder")
	excludes(t, with, "nobody to @-mention", "Working with them")
}

// -- Document structure ---------------------------------------------------

// heading is one ATX heading a builder emitted: its level and its words.
type heading struct {
	level int
	text  string
}

// headingsIn reads the ATX headings out of a rendered prompt, skipping
// anything inside a fenced block — a tool catalogue carries shell samples,
// and `# install deps` is a comment rather than a section.
func headingsIn(prompt string) []heading {
	var out []heading
	fence := ""
	for line := range strings.SplitSeq(prompt, "\n") {
		if fence != "" {
			if strings.HasPrefix(line, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fence = line[:3]
			continue
		}
		hashes := len(line) - len(strings.TrimLeft(line, "#"))
		if hashes == 0 || hashes > 6 || !strings.HasPrefix(line[hashes:], " ") {
			continue
		}
		out = append(out, heading{level: hashes, text: strings.TrimSpace(line[hashes:])})
	}
	return out
}

// EVERY SECTION OF A PHASE PROMPT IS A SIBLING.
//
// These documents are flat: the company's policies are not part of the
// agent's identity, and the tool catalogue is not part of the turn contract —
// each is the next thing the model reads. A single `#` over a run of `##`
// says the opposite, and says it to every reader of the document: the model,
// anything that summarises one, and the dashboard's prompt outline, which
// draws the structure the builders wrote.
//
// It is asserted rather than remembered because nothing else would notice.
// The executor's prompt carried that `#` for its whole life and no assertion
// in this package was looking at the level of anything.
func TestEveryHeadingAPromptBuilderEmitsIsASibling(t *testing.T) {
	t.Parallel()
	seat := lead()
	// Zero inputs, so every heading counted below is one a builder in this
	// package wrote — not one a caller passed in a catalogue or a skill body.
	for name, prompt := range map[string]string{
		"executor":   BuildExecutor(seat, ExecutorInput{}).Text,
		"review":     BuildReview(seat, ReviewInput{}).Text,
		"onboarding": BuildOnboarding(seat, OnboardingInput{}).Text,
		"subagent":   BuildSubagent(seat, SubagentInput{}).Text,
	} {
		found := headingsIn(prompt)
		// Guard the guard: a builder that emitted no heading at all would
		// satisfy "every heading is level 2" without meaning anything.
		if name != "subagent" && len(found) == 0 {
			t.Errorf("%s: no headings at all — this case would pass on an empty prompt", name)
		}
		for _, h := range found {
			if h.level != 2 {
				t.Errorf("%s: %q is an h%d among siblings at h2 — it claims the "+
					"sections after it are part of it", name, h.text, h.level)
			}
		}
	}
}

// THE ASK IS A SECTION OF THE MESSAGE, NOT A LINE INSIDE THE BLOCK ABOVE IT.
//
// It was "Task:", a bare label, and the conversation ledger above it is a
// heading — so with history present the newest thing anybody said was filed
// under "Earlier in this conversation", whose own text calls it "the task
// below". Every block of this message is a peer, and this is what says so.
func TestTheUserMessagesBlocksAreAllPeers(t *testing.T) {
	t.Parallel()
	msg := BuildPhaseUserMessage(UserMessage{
		TaskDescription:     "THE-ASK",
		PriorWork:           "PRIOR-WORK",
		ConversationHistory: "CONVERSATION-HISTORY",
	}).Text
	var levels []int
	var texts []string
	for _, h := range headingsIn(msg) {
		levels = append(levels, h.level)
		texts = append(texts, h.text)
	}
	if !slices.Contains(texts, "Task") {
		t.Fatalf("the ask has no heading of its own: %q", texts)
	}
	for i, level := range levels {
		if level != 2 {
			t.Errorf("%q is an h%d — the message's blocks are peers", texts[i], level)
		}
	}
}

// A SEAT WHOSE CONTACT IDENTITIES THE DIRECTORY WITHHOLDS RENDERS NONE.
//
// The party registry already resolves a suspended person's Slack message to an
// outside party; a roster that still printed their Slack id would have a lead's
// agent DMing somebody the company has off-boarded. The seat stays — work for it
// waits for whoever holds it next — and the prompt does not say WHY: a
// person's standing is not an agent's business. The control is the same
// roster with nothing withheld.
func TestRosterRendersNoIdentityForAWithheldSeat(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name: "Acme",
		Units: []*org.Unit{{
			Name: "Eng Team",
			Type: org.UnitTypeTeam,
			Lead: "lead",
			Roles: []*org.Role{
				{Name: "Lead", DeclaredHandle: "lead"},
				{
					Name: "Sarah Chen", Kind: org.KindHuman,
					Contact:      &org.HumanContact{SlackUserID: "U0HUMAN"},
					Availability: "CET business hours",
				},
			},
		}},
	}
	o.Normalize()
	seat := seatIn(o, "lead")

	contains(t, BuildExecutor(seat, ExecutorInput{}).Text, "Slack ID: U0HUMAN")

	seat.Withheld = func(handle string) bool { return handle == "sarah-chen" }
	p := BuildExecutor(seat, ExecutorInput{}).Text
	contains(t, p, "**Sarah Chen** (sarah-chen) — **human teammate**",
		"cannot be reached on any chat or code-host account",
		"in the PM tool, assigned to the seat",
		"Availability: CET business hours")
	excludes(t, p, "U0HUMAN", "Slack ID:", "nobody to @-mention",
		"suspended", "removed", "retired")
}

// A SEAT THAT CAN ASK IS TOLD WHEN TO ASK FOR A DECISION, AND TO STOP.
//
// The ask is a structured `decision` because the person answers by choosing
// and the asker is woken with the choice — and the shape of one (options, a
// recommendation, the evidence, the role) is the `decision` argument's own
// schema, which reaches the model with the tool: a list of it here was the
// same contract twice. What only this section says is when, and that asking
// ends the branch: a seat that asked and carried on has decided for the
// person, or is waiting inside a turn that cannot receive the reply. Gated on
// the tool it names, like every other instruction to call something: a seat
// without it is told nothing.
func TestTheExecutorIsToldToAskAndStopOnTheBranch(t *testing.T) {
	t.Parallel()
	asking := BuildExecutor(engineer(), ExecutorInput{
		AvailableTools: []string{"comment_on_work_item"},
	}).Text
	contains(t, asking,
		"## When a decision is not yours to make",
		"an `ask` and a `decision`",
		"`comment_on_work_item`", "`create_work_item`",
		"END THIS TURN BLOCKED ON THAT BRANCH",
		"Their answer wakes you with the option")
	excludes(t, asking, "Two to four options", "`approver`", "`contributor`")
	// AFTER THE CONTRACT, which it qualifies, and before the tools.
	if strings.Index(asking, "## When a decision") < strings.Index(asking, "## Your turn") {
		t.Error("the escalation guidance renders before the turn contract it qualifies")
	}
	excludes(t, BuildExecutor(engineer(), ExecutorInput{
		AvailableTools: []string{"post_message"},
	}).Text, "## When a decision is not yours to make")
}
