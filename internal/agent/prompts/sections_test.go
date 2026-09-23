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
	contains(t, BuildExecutor(s, ExecutorInput{}), "**Reports to:** Sarah Chen (human)")
	contains(t, BuildIdentityLine(s), "Sarah Chen (human)")
}

func TestHumanColleaguesNoteAppearsOnlyInMixedOrgs(t *testing.T) {
	t.Parallel()
	mixed := BuildExecutor(seatIn(mixedAcme(), "eng"), ExecutorInput{})
	contains(t, mixed, "## Human colleagues", "NOT on A2A", "asynchronously")

	// A pure-agent company's prompts are unchanged by the feature existing.
	excludes(t, BuildExecutor(engineer(), ExecutorInput{}), "## Human colleagues")
}

// A human member's roster block renders their external identities, their
// availability, and how to hand them work — there is no engine task
// assignment that reaches a person.
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
	p := BuildExecutor(seatIn(o, "lead"), ExecutorInput{})

	contains(t, p, "**Sarah Chen** (sarah-chen) — **human teammate**")
	// Identities render generically, labelled by transport. The shared
	// Atlassian id covers both Jira and Confluence and renders ONCE, under
	// the first transport that claims it — twice would read as two
	// different accounts.
	contains(t, p, "Slack ID: U0HUMAN", "Jira ID: 5b10-s")
	excludes(t, p, "Confluence ID: 5b10-s")
	contains(t, p, "Availability: CET business hours",
		"hand work over in the PM tool")
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

	excludes(t, BuildExecutor(seat, ExecutorInput{}), "${SARAH_SLACK_ID}", "Slack ID:")

	seat.Env = func(name string) (string, bool) {
		return "U0RESOLVED", name == "SARAH_SLACK_ID"
	}
	contains(t, BuildExecutor(seat, ExecutorInput{}), "Slack ID: U0RESOLVED")
}

// A seat missing its chart renders the phase contract and no identity,
// rather than taking the turn down: a prompt without an identity line is
// degraded, a panicking prompt builder is a dead turn.
func TestZeroSeatRendersTheContractWithoutPanicking(t *testing.T) {
	t.Parallel()
	var s Seat
	contains(t, BuildExecutor(s, ExecutorInput{}), "## Your turn")
	contains(t, BuildReview(s, ReviewInput{}), "## REVIEW phase")
	contains(t, BuildOnboarding(s, OnboardingInput{}), "## ONBOARDING phase")
	excludes(t, BuildExecutor(s, ExecutorInput{}), "## Your Identity")
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

// A PERSON WITH NO CHAT ACCOUNT HAS NOBODY TO @-MENTION.
//
// A human seat can be in the chart with no contact identity at all — somebody
// who signs in to the dashboard and never joined the company's chat — and a
// roster that told an agent to mention them would produce a message addressed
// to a handle resolving to nobody, which reads to everyone else as work handed
// over.
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
	p := BuildExecutor(seatIn(o, "lead"), ExecutorInput{})

	contains(t, p, "**Jane Founder** (jane-founder) — **human teammate**")
	excludes(t, p, "Crewlet ID:", "crewlet")
	contains(t, p, "nobody to @-mention")
	excludes(t, p, "@-mention them on their team's chat")

	// A colleague WITH a reachable account keeps the mention instruction.
	o.Units[0].Roles[1].Contact.SlackUserID = "U0FOUNDER"
	o.Normalize()
	with := BuildExecutor(seatIn(o, "lead"), ExecutorInput{})
	contains(t, with, "Slack ID: U0FOUNDER", "@-mention them on their team's chat")
	excludes(t, with, "nobody to @-mention")
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
		"executor":   BuildExecutor(seat, ExecutorInput{}),
		"review":     BuildReview(seat, ReviewInput{}),
		"onboarding": BuildOnboarding(seat, OnboardingInput{}),
		"subagent":   BuildSubagent(seat, SubagentInput{}),
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
	})
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
