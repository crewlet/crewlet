package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// withheldCompany is a company of two agent seats: one whose role carries the
// health grant and the code tier every code write needs, and one whose role
// carries neither.
const withheldCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: SRE
    handle: sre
    llm: zulu
    health_tools: true
    code: {access: full_access}
  - name: Dev
    handle: dev
    llm: zulu
`

// AN AGENT IS COUNTED THE BUILTINS ITS TURN IS SHOWN. WithheldBuiltins, asked
// as the API asks it (a chart and a registry snapshot, not the epoch), leaves
// out exactly what the same seat's executor is not handed, so a turn's
// surfaces and Settings › MCP & tools' per-agent count cannot part. One seat
// carries the health grant and full_access and the other neither, so they are
// withheld different builtins. mark_onboarded is left out: the one builtin a
// phase scopes (the runner's phaseScoped), offered to onboarding and never to
// the executor shownTools reads.
//
// Mutation: move the code-tier layer into withheldFrom alone and dev is not
// shown code_commit_files that WithheldBuiltins counts; a layer added to
// WithheldBuiltins alone is red the other way.
func TestAnAgentIsCountedTheBuiltinsItsTurnIsShown(t *testing.T) {
	t.Parallel()
	c, err := NewCompany(parseCompany(t, withheldCompany))
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	e := &Engine{}
	e.epoch.current.Store(c)
	requesters, chart := turnRequesters{engine: e}, orgchart.Of(c.Org)
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Authorize:  builtin.Decide(chart),
		Requesters: requesters,
		Health: builtin.HealthDeps{Leases: coordmem.New(), Self: "node-a", Requesters: requesters, Chart: chart,
			Status: idleGitletNodes{}, Code: builtin.CodeDeps{Code: unreadCode{}, Requesters: requesters, Chart: chart}},
		Code: builtin.CodeDeps{Code: unreadCode{}, Writer: unwrittenCode{}, Holder: unwrittenCode{},
			Requesters: requesters, Chart: chart},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot := reg.Snapshot()
	withheld := func(handle string) []string {
		return WithheldBuiltins(t.Context(), c.Org, c.Org.AgentSeatByHandle(handle), snapshot)
	}
	if dev := withheld("dev"); !slices.Contains(dev, builtin.FleetHealthTool) ||
		!slices.Contains(dev, builtin.CodeCommitFilesTool) {
		t.Fatalf("the premise: dev is withheld %v, want fleet_health and code_commit_files among them", dev)
	}
	if sre := withheld("sre"); slices.Contains(sre, builtin.FleetHealthTool) ||
		slices.Contains(sre, builtin.CodeCommitFilesTool) {
		t.Fatalf("the premise: sre is withheld %v, want neither fleet_health nor code_commit_files", sre)
	}

	for _, handle := range []string{"sre", "dev"} {
		shown, held := shownTools(t, c, handle, reg), withheld(handle)
		for _, entry := range snapshot.Entries() {
			name := entry.Name()
			if entry.Origin != tools.OriginBuiltin || name == toolname.MarkOnboarded {
				continue
			}
			if offered := !slices.Contains(held, name); offered != slices.Contains(shown, name) {
				t.Errorf("%s: %s is counted offered %t, and its executor is shown it %t", handle, name, offered,
					slices.Contains(shown, name))
			}
		}
	}
}
