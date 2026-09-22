package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// hierarchyDoc is a company that expresses the SAME authority two ways, which
// is the point: the CEO leads the SRE through `manages:`, and the CTO leads
// them by leading the division they sit in. A check that read one relation
// would refuse half the leads in any real company, and which half depends on
// how the founder wrote their chart.
const hierarchyDoc = `
name: Nimbus
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
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
`

// chartOf parses the fixture into the org the walks read.
func chartOf(t *testing.T) *config.Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(hierarchyDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

// BOTH RELATIONS A COMPANY CAN EXPRESS ARE READ, AND NOTHING ELSE IS.
//
// PURE OVER VALUES, deliberately: with the walk inside [ChartAuthority.Leads]
// it could only be exercised through a running engine against whatever
// hierarchy that fixture happened to have — and measured, deleting both loops
// left this package's suite green. A rule exercised only through a live
// engine is a rule nobody re-measures.
func TestTheChartWalkReadsTheManagesChainAndTheUnitLead(t *testing.T) {
	t.Parallel()
	o, err := chartOf(t).Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
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
			if got := leadsInChart(o, c.actor, c.target); got != c.want {
				t.Errorf("leads(%s, %s) = %v, want %v", c.actor, c.target, got, c.want)
			}
		})
	}
}

// AND A PROJECT IS LED BY ITS UNIT'S LEAD, OR BY THE SEAT WHOSE OWN IT IS.
func TestTheChartWalkReadsAProjectsLead(t *testing.T) {
	t.Parallel()
	o, err := chartOf(t).Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	for _, c := range []struct {
		name           string
		actor, project string
		want           bool
	}{
		{"the unit's lead", "cto", "PLATFORM", true},
		{"a member of it", "sre", "PLATFORM", false},
		{"a seat's own project", "cto", "TOOLING", true},
		{"somebody else's", "ceo", "TOOLING", false},
		{"a project nobody declares", "cto", "GHOST", false},
		{"keyed however it was typed", "cto", "platform", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := leadsProjectInChart(o, c.actor, c.project); got != c.want {
				t.Errorf("leadsProject(%s, %s) = %v, want %v",
					c.actor, c.project, got, c.want)
			}
		})
	}
}
