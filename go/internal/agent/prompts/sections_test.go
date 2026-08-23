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
	contains(t, BuildPlan(s, PlanInput{}), "**Reports to:** Sarah Chen (human)")
	contains(t, BuildIdentityLine(s), "Sarah Chen (human)")
}

func TestHumanColleaguesNoteAppearsOnlyInMixedOrgs(t *testing.T) {
	t.Parallel()
	mixed := BuildPlan(seatIn(mixedAcme(), "Engineer"), PlanInput{})
	contains(t, mixed, "## Human colleagues", "NOT on A2A", "asynchronously")

	// A pure-agent company's prompts are unchanged by the feature existing.
	excludes(t, BuildPlan(engineer(), PlanInput{}), "## Human colleagues")
}

// A human member's roster block renders their external identities, their
// availability, and how to hand them work — there is no engine task
// assignment that reaches a person.
func TestRosterRendersHumanMemberBlock(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Units: []*org.OrgUnit{{
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
	p := BuildPlan(seatIn(o, "Lead"), PlanInput{})

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
		Units: []*org.OrgUnit{{
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

	excludes(t, BuildPlan(seat, PlanInput{}), "${SARAH_SLACK_ID}", "Slack ID:")

	seat.Env = func(name string) (string, bool) {
		return "U0RESOLVED", name == "SARAH_SLACK_ID"
	}
	contains(t, BuildPlan(seat, PlanInput{}), "Slack ID: U0RESOLVED")
}

// A seat missing its chart renders the phase contract and no identity,
// rather than taking the turn down: a prompt without an identity line is
// degraded, a panicking prompt builder is a dead turn.
func TestZeroSeatRendersTheContractWithoutPanicking(t *testing.T) {
	t.Parallel()
	var s Seat
	contains(t, BuildPlan(s, PlanInput{}), "## PLAN phase")
	contains(t, BuildExecute(s, ExecuteInput{}), "## EXECUTE phase")
	contains(t, BuildReview(s, ReviewInput{}), "## REVIEW phase")
	contains(t, BuildOnboarding(s, OnboardingInput{}), "## ONBOARDING phase")
	excludes(t, BuildPlan(s, PlanInput{}), "# Your Identity")
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
