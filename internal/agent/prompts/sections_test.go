package prompts

import (
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// The (human) marker is load-bearing: it is how an agent knows this
// colleague replies asynchronously over an external surface rather than
// running a turn.
func TestHumanSeatsAreMarkedInBothIdentityRenderings(t *testing.T) {
	t.Parallel()
	s := seatIn(mixedAcme(), "Engineer")
	contains(t, BuildExecutor(s, ExecutorInput{}), "**Reports to:** Sarah Chen (human)")
	contains(t, BuildIdentityLine(s), "Sarah Chen (human)")
}

func TestHumanColleaguesNoteAppearsOnlyInMixedOrgs(t *testing.T) {
	t.Parallel()
	mixed := BuildExecutor(seatIn(mixedAcme(), "Engineer"), ExecutorInput{})
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
			Lead: "Lead",
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
	p := BuildExecutor(seatIn(o, "Lead"), ExecutorInput{})

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
			Lead: "Lead",
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
	seat := seatIn(o, "Lead")

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
	excludes(t, BuildExecutor(s, ExecutorInput{}), "# Your Identity")
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

// A PERSON WHO HOLDS ONLY AN OPERATOR CREDENTIAL HAS NOBODY TO @-MENTION.
// Their id identifies them on the engine's own surface, where nothing is ever
// sent — so a roster that listed it and then told an agent to mention them
// would produce a message addressed to a handle resolving to nobody, which
// reads to everyone else as work handed over.
func TestRosterDoesNotOfferAnOperatorIDAsAnAddress(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name: "Acme",
		Units: []*org.Unit{{
			Name: "Eng Team",
			Type: org.UnitTypeTeam,
			Lead: "Lead",
			Roles: []*org.Role{
				{Name: "Lead", DeclaredHandle: "lead"},
				{
					Name:    "Jane Founder",
					Kind:    org.KindHuman,
					Contact: &org.HumanContact{CrewletOperatorID: "founder"},
				},
			},
		}},
	}
	o.Normalize()
	p := BuildExecutor(seatIn(o, "Lead"), ExecutorInput{})

	contains(t, p, "**Jane Founder** (jane-founder) — **human teammate**")
	excludes(t, p, "Crewlet ID:", "crewlet")
	contains(t, p, "nobody to @-mention")
	excludes(t, p, "@-mention them on their team's chat")

	// A colleague WITH a reachable account keeps the mention instruction.
	o.Units[0].Roles[1].Contact.SlackUserID = "U0FOUNDER"
	o.Normalize()
	with := BuildExecutor(seatIn(o, "Lead"), ExecutorInput{})
	contains(t, with, "Slack ID: U0FOUNDER", "@-mention them on their team's chat")
	excludes(t, with, "nobody to @-mention")
}
