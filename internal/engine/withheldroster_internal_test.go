package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/tools"
)

// A TURN SHOWS ITS AGENT ONLY THE PEOPLE WHO MAY BE REACHED — through the
// wiring a real turn goes through, from the live registry's reading to the
// standing instructions its executor's run is launched with.
//
// The party registry withholds a suspended person's contact identities, so an
// inbound message from them resolves to an outside party. A lead's prompt that
// still printed their Slack id would hand its agent the one address the
// company stopped routing. The control is the same turn over a registry that
// withholds nobody.
func TestATurnShowsItsAgentOnlyThePeopleWhoMayBeReached(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		standing notify.Standing
		want     string
		refused  string
	}{
		{"nobody withheld", notify.Standing{}, "Slack ID: U0SARAH", "cannot be reached"},
		{"the holder suspended", notify.StandingOf([]notify.Holder{
			{Seat: "sarah-chen", Stage: iam.StageSuspended},
		}), "cannot be reached", "U0SARAH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, rig := leadEngine(t, tc.standing)
			tick := events.New(types.TaskAssigned{
				TaskID: "t-1", RoleName: "Lead", Description: "plan the week",
			}, events.TraceContext{})
			// THE INSTANT THE WORK BEGAN, as every dispatch hands a turn:
			// a unit of work stated with none began at the zero instant,
			// past every ledger, and is refused before it reaches a model.
			_, _ = e.runTurn(t.Context(), Request{
				Handle: "lead", WorkKey: "wk-1", WorkSince: time.Now().UTC(),
				Events: []*events.Event{tick},
			})
			started := rig.started()
			if len(started) != 1 {
				t.Fatalf("%d runs launched, want the lead's executor: there is no prompt to read",
					len(started))
			}
			system := started[0].System
			if !strings.Contains(system, tc.want) {
				t.Errorf("the lead's prompt does not say %q", tc.want)
			}
			if strings.Contains(system, tc.refused) {
				t.Errorf("the lead's prompt says %q", tc.refused)
			}
		})
	}
}

// leadEngine is an engine running one agent lead with a human report, over a
// registry built from the reading given, and the runtime its executor
// launches in.
func leadEngine(t *testing.T, standing notify.Standing) (*Engine, *runRig) {
	t.Helper()
	lead := &org.Role{Name: "Lead", DeclaredHandle: "lead", LLM: org.ProviderKeys{"only"}}
	o := &org.Organization{Name: "Acme", Units: []*org.Unit{{
		Name: "Eng", Type: org.UnitTypeTeam, Lead: "lead",
		Roles: []*org.Role{lead, {
			Name: "Sarah Chen", Kind: org.KindHuman,
			Contact: &org.HumanContact{SlackUserID: "U0SARAH"},
		}},
	}}}
	o.Normalize()
	models := runnable(t, []phase.Entry{{Key: "only", Provider: refusingProvider{}}}, lead)
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	e := withSessions(t, &Engine{backends: &Backends{Queue: q, Fleet: coordmem.NewFleet()}})
	e.epoch.current.Store(&Company{
		Org: o, Models: models, Tools: tools.NewRegistry(),
		Config: &config.Company{Name: "Acme", TurnEngine: config.TurnEngine{
			MaxIterations: 1, DelegationDepthLimit: 3,
		}},
	})
	reg := notify.NewRegistry(o, nil)
	reg.ReconcileHumanContacts(o, func(string) (string, bool) { return "", false }, standing)
	e.notify.registry = reg
	return e, rigRuns(t, e, nil)
}
