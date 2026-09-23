package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/tools"
)

// A TURN SHOWS ITS AGENT ONLY THE PEOPLE WHO MAY BE REACHED — through the
// wiring a real turn goes through, from the live registry's reading to the
// prompt the model is sent.
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
			prov := &promptCapture{}
			e := leadEngine(t, prov, tc.standing)
			tick := events.New(types.TaskAssigned{
				TaskID: "t-1", RoleName: "Lead", Description: "plan the week",
			}, events.TraceContext{})
			_, _ = e.runTurn(t.Context(), Request{
				Handle: "lead", WorkKey: "wk-1", Events: []*events.Event{tick},
			})
			system := prov.system(t)
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
// registry built from the reading given.
func leadEngine(t *testing.T, prov llm.Provider, standing notify.Standing) *Engine {
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
	models, err := phase.NewRegistry([]phase.Entry{{Key: "only", Provider: prov}})
	if err != nil {
		t.Fatalf("phase.NewRegistry: %v", err)
	}
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	e := &Engine{backends: &Backends{Queue: q}}
	e.epoch.current.Store(&Company{
		Org: o, Models: models, Tools: tools.NewRegistry(),
		Config: &config.Company{Name: "Acme", TurnEngine: config.TurnEngine{
			MaxIterations: 1, DelegationDepthLimit: 3, MaxToolRounds: 1,
		}},
	})
	reg := notify.NewRegistry(o)
	reg.ReconcileHumanContacts(o, func(string) (string, bool) { return "", false }, standing)
	e.notify.registry = reg
	return e
}

// promptCapture records the system prompt of the first request and refuses
// it: the prompt is the whole of what a case reads, and a refused call ends
// the turn without a second round.
type promptCapture struct {
	mu       sync.Mutex
	requests []llm.Request
}

func (*promptCapture) Model() string { return "capture" }

func (p *promptCapture) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return nil, &llm.Error{Kind: llm.KindFatal, Provider: "test", Model: "capture"}
}

func (p *promptCapture) system(t *testing.T) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		t.Fatal("the turn never reached the model, so there is no prompt to read")
	}
	var b strings.Builder
	for _, m := range p.requests[0].Messages {
		if m.Role == "system" {
			b.WriteString(m.Content)
		}
	}
	if b.Len() == 0 {
		t.Fatal("the first request carries no system prompt")
	}
	return b.String()
}
