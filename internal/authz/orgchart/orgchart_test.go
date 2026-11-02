package orgchart_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// hierarchyDoc is a company that expresses the SAME authority two ways, which
// is the point: the CEO leads the SRE through `manages:`, and the CTO leads
// them by leading the division they sit in. A check that read one relation
// would refuse half the leads in any real company, and which half depends on
// how the founder wrote their chart.
const hierarchyDoc = `
name: Nimbus
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    manages: [sre]
  - name: CTO
    handle: cto
    llm: zulu
    project: TOOLING
units:
  - name: Engineering
    id: engineering
    lead: cto
    project: PLATFORM
    space: RUNBOOKS
    children:
      - name: Reliability
        id: reliability
        lead: sre-lead
        roles:
          # THE LEAD SITS IN THE UNIT IT LEADS, which is the usual shape and
          # the only one in which "does this seat lead itself" has a route to
          # answer yes: its own unit chain holds a unit whose effective lead
          # is itself. Without that seat the self case is answered by the
          # chart happening to have nowhere to look, which is not the guard.
          - name: SRE Lead
            handle: sre-lead
            llm: zulu
          - name: SRE
            handle: sre
            llm: zulu
  - name: Design
    id: design
    lead: designer
    roles:
      - name: Designer
        handle: designer
        llm: zulu
`

// treeOf parses the fixture into the organization the relations read.
func treeOf(t *testing.T) *org.Organization {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(hierarchyDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	return o
}

// BOTH RELATIONS A COMPANY CAN EXPRESS ARE READ, AND NOTHING ELSE IS.
func TestLeadsReadsTheManagesChainAndTheUnitLead(t *testing.T) {
	t.Parallel()
	chart := orgchart.Of(treeOf(t))
	for _, c := range []struct {
		name          string
		actor, target string
		want          bool
	}{
		{"the manages chain", "ceo", "sre", true},
		{"a unit lead one level up", "cto", "sre", true},
		{"and not the other way", "sre", "cto", false},
		{"nor a colleague", "sre", "ceo", false},
		{"a lead inside the unit it leads", "sre-lead", "sre", true},
		{"self is never a lead relation", "sre-lead", "sre-lead", false},
		{"a subject the chart does not hold", "ceo", "nobody", false},
		{"an actor the chart does not hold", "nobody", "sre", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := chart.Leads(t.Context(), c.actor, c.target)
			if err != nil || got != c.want {
				t.Errorf("Leads(%s, %s) = %v, %v; want %v", c.actor, c.target, got, err, c.want)
			}
		})
	}
}

// WHETHER A SEAT LEADS ANYBODY IS THE SAME RELATION, ASKED OF EVERY SEAT.
//
// It is what a caller naming somebody else's login is decided by before the
// identity directory is asked, so it must agree with Leads exactly — a seat it
// says leads nobody while Leads admits it on somebody is a lead refused their
// own report, and the reverse sends a stranger's lookup to the directory.
// Held against the definition over every seat of the fixture, the shortcut
// for a seat that manages nobody and leads no unit included.
func TestLeadsAnyoneAgreesWithLeadsOverEverySeat(t *testing.T) {
	t.Parallel()
	o := treeOf(t)
	chart := orgchart.Of(o)
	for _, c := range []struct {
		actor string
		want  bool
	}{
		{"ceo", true},      // manages the SRE
		{"cto", true},      // leads the division the SRE sits in
		{"sre-lead", true}, // leads the unit it sits in, with the SRE beside it
		{"sre", false},     // manages nobody and leads no unit
		{"designer", false},
		{"nobody", false}, // not a seat at all
		{"jane.doe", false},
		{"", false},
	} {
		if got, err := chart.LeadsAnyone(t.Context(), c.actor); err != nil || got != c.want {
			t.Errorf("LeadsAnyone(%q) = %v, %v; want %v", c.actor, got, err, c.want)
		}
	}
	for actor := range o.AllRoles() {
		want := false
		for subject := range o.AllRoles() {
			if led, _ := chart.Leads(t.Context(), actor.Handle(), subject.Handle()); led {
				want = true
			}
		}
		if got, _ := chart.LeadsAnyone(t.Context(), actor.Handle()); got != want {
			t.Errorf("LeadsAnyone(%q) = %v, and Leads admits it on some seat = %v",
				actor.Handle(), got, want)
		}
	}
}

// A PROJECT IS LED BY ITS UNIT'S LEAD, OR BY THE SEAT WHOSE OWN IT IS — and a
// page container the same way, over the other field.
func TestAProjectAndAContainerAreLedByTheirUnitsLead(t *testing.T) {
	t.Parallel()
	chart := orgchart.Of(treeOf(t))
	for _, c := range []struct {
		name       string
		ask        func(actor, key string) (bool, error)
		actor, key string
		want       bool
	}{
		{"the unit's lead", projectOf(t, chart), "cto", "PLATFORM", true},
		{"a member of it", projectOf(t, chart), "sre", "PLATFORM", false},
		{"a seat's own project", projectOf(t, chart), "cto", "TOOLING", true},
		{"somebody else's", projectOf(t, chart), "ceo", "TOOLING", false},
		{"a project nobody declares", projectOf(t, chart), "cto", "GHOST", false},
		{"keyed however it was typed", projectOf(t, chart), "cto", "platform", true},
		{"a container's unit lead", containerOf(t, chart), "cto", "RUNBOOKS", true},
		{"a project key is not a container", containerOf(t, chart), "cto", "PLATFORM", false},
	} {
		got, err := c.ask(c.actor, c.key)
		if err != nil || got != c.want {
			t.Errorf("%s: (%s, %s) = %v, %v; want %v", c.name, c.actor, c.key, got, err, c.want)
		}
	}
}

// A UNIT IS IN A LEAD'S SUBTREE WHEN THEY LEAD IT OR ANY UNIT ABOVE IT.
//
// The relation the org chart's own write is decided by: the CTO leads
// Engineering and therefore Reliability inside it, the SRE lead leads
// Reliability and not the division above it, and a lead of one tree leads
// nothing of a sibling's.
func TestLeadsUnitIsTheSubtree(t *testing.T) {
	t.Parallel()
	chart := orgchart.Of(treeOf(t))
	for _, c := range []struct {
		actor, unit string
		want        bool
	}{
		{"cto", "engineering", true},
		{"cto", "reliability", true},
		{"sre-lead", "reliability", true},
		{"sre-lead", "engineering", false},
		{"cto", "design", false},
		{"designer", "reliability", false},
		{"sre", "reliability", false},
		{"ceo", "reliability", false}, // manages the SRE, leads no unit
		{"cto", "ghost", false},
		{"cto", "", false},
	} {
		if got, err := chart.LeadsUnit(t.Context(), c.actor, c.unit); err != nil || got != c.want {
			t.Errorf("LeadsUnit(%s, %s) = %v, %v; want %v", c.actor, c.unit, got, err, c.want)
		}
	}
}

// A SEAT IS A MEMBER OF THE UNIT IT SITS IN AND OF EVERY UNIT ABOVE IT, AND
// LEADING A UNIT IS NOT SITTING IN IT.
//
// The relation a share with a team admits: the SRE sits in Reliability and so
// in Engineering above it; the CTO leads Engineering from the root and sits in
// neither, so a share with the division does not reach them through
// membership; a seat of a sibling tree is in nothing of this one. Mutation:
// answer from the lead relation and the CTO is admitted; compare only the
// seat's own unit and the SRE is refused the division.
func TestMemberOfIsTheSeatsOwnUnitChain(t *testing.T) {
	t.Parallel()
	chart := orgchart.Of(treeOf(t))
	for _, c := range []struct {
		actor, unit string
		want        bool
	}{
		{"sre", "reliability", true},
		{"sre", "engineering", true},
		{"sre-lead", "reliability", true},
		{"sre-lead", "engineering", true},
		{"designer", "design", true},
		{"designer", "engineering", false},
		{"sre", "design", false},
		{"cto", "engineering", false}, // leads it, and sits at the root
		{"ceo", "reliability", false}, // manages the SRE, sits in no unit
		{"sre", "ghost", false},
		{"sre", "", false},
		{"nobody", "reliability", false},
		{"", "reliability", false},
	} {
		if got, err := chart.MemberOf(t.Context(), c.actor, c.unit); err != nil || got != c.want {
			t.Errorf("MemberOf(%q, %q) = %v, %v; want %v", c.actor, c.unit, got, err, c.want)
		}
	}
}

// NO TREE IS UNKNOWN, NEVER "YOU LEAD NOTHING", on every relation.
func TestNoTreeIsUnknownRatherThanFalse(t *testing.T) {
	t.Parallel()
	chart := orgchart.Of(nil)
	ctx := t.Context()
	for name, ask := range map[string]func() (bool, error){
		"Leads":          func() (bool, error) { return chart.Leads(ctx, "cto", "sre") },
		"LeadsAnyone":    func() (bool, error) { return chart.LeadsAnyone(ctx, "cto") },
		"LeadsProject":   func() (bool, error) { return chart.LeadsProject(ctx, "cto", "PLATFORM") },
		"LeadsContainer": func() (bool, error) { return chart.LeadsContainer(ctx, "cto", "RUNBOOKS") },
		"LeadsUnit":      func() (bool, error) { return chart.LeadsUnit(ctx, "cto", "engineering") },
		"MemberOf":       func() (bool, error) { return chart.MemberOf(ctx, "sre", "engineering") },
		"SeatGranted": func() (bool, error) {
			return chart.SeatGranted(ctx, "sre", authz.SeatGrantHealth)
		},
	} {
		if _, err := ask(); !errors.Is(err, authz.ErrNoChart) {
			t.Errorf("%s with no tree: err = %v, want authz.ErrNoChart", name, err)
		}
	}
}

// A GRANT IS THE ROLE'S, AN AGENT'S ALONE, AND THE SEATS IT REACHES ARE THE
// SEATS IT ADMITS.
//
// The SRE's role carries the health grant and nobody else's does: the CEO who
// manages it and the unit lead above it hold nothing for leading it, a human
// seat whose document carries the field holds nothing (a person is decided by
// their own permissions), and a handle the tree does not hold and a grant
// this build does not know open nothing. GrantedSeats answers the same rule
// over the whole tree. Mutation: answer SeatGranted from the role without the
// agent check, or make GrantedSeats read a second field, and this goes red.
func TestAGrantIsAnAgentRolesAndReachesWhomItAdmits(t *testing.T) {
	t.Parallel()
	// UNVALIDATED, because the human seat's grant is an admission rule's
	// refusal (internal/config): a stored revision carrying one still runs,
	// and this is what the chart then answers for it.
	cfg, err := config.ParseCompanyDocument([]byte(strings.Replace(strings.Replace(hierarchyDoc,
		"          - name: SRE\n            handle: sre\n",
		"          - name: SRE\n            handle: sre\n            health_tools: true\n", 1),
		"roles:\n  - name: CEO\n",
		"roles:\n  - name: Founder\n    handle: founder\n    kind: human\n    health_tools: true\n"+
			"  - name: CEO\n", 1)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	chart := orgchart.Of(o)
	for _, c := range []struct {
		actor string
		grant authz.SeatGrant
		want  bool
	}{
		{"sre", authz.SeatGrantHealth, true},
		{"ceo", authz.SeatGrantHealth, false},
		{"sre-lead", authz.SeatGrantHealth, false},
		{"founder", authz.SeatGrantHealth, false},
		{"nobody", authz.SeatGrantHealth, false},
		{"sre", authz.SeatGrant("root"), false},
	} {
		if got, err := chart.SeatGranted(t.Context(), c.actor, c.grant); err != nil || got != c.want {
			t.Errorf("SeatGranted(%q, %q) = %v, %v; want %v", c.actor, c.grant, got, err, c.want)
		}
	}
	if got := orgchart.GrantedSeats(o, authz.SeatGrantHealth); !slices.Equal(got, []string{"sre"}) {
		t.Errorf("GrantedSeats = %v, want exactly the seat the grant admits, [sre]", got)
	}
	if got := orgchart.GrantedSeats(nil, authz.SeatGrantHealth); got != nil {
		t.Errorf("GrantedSeats over no tree = %v, want nobody", got)
	}
}

func projectOf(t *testing.T, chart authz.Chart) func(actor, key string) (bool, error) {
	return func(actor, key string) (bool, error) {
		return chart.LeadsProject(t.Context(), actor, key)
	}
}

func containerOf(t *testing.T, chart authz.Chart) func(actor, key string) (bool, error) {
	return func(actor, key string) (bool, error) {
		return chart.LeadsContainer(t.Context(), actor, key)
	}
}
