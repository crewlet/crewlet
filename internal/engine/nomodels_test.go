package engine_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// noModelsDoc is an org chart written before its credentials exist: valid,
// and configuring no providers.llm at all. It is what a company created from
// the dashboard is until somebody adds a provider.
const noModelsDoc = `
name: Acme
roles:
  - name: CEO
    handle: ceo
`

// THE EPOCH BUILDS, AND IT CANNOT THINK. Validation accepts a company with no
// models, so a build that refused it made every node refuse a revision its own
// dry run had called valid. What the epoch cannot do is build a runner, and the
// refusal names the seat and the one field that fixes it.
func TestACompanyWithNoModelsBuildsAnEpochThatCannotTakeATurn(t *testing.T) {
	t.Parallel()
	c, err := engine.NewCompany(parsedCompany(t, noModelsDoc))
	if err != nil {
		t.Fatalf("a company with no models was refused at build: %v", err)
	}
	if c.Models != nil {
		t.Errorf("Models = %v, want no registry for a company that configures none", c.Models)
	}
	if got := c.Seats(); len(got) != 1 || got[0].Handle != "ceo" {
		t.Errorf("seats = %v, want the one agent seat placed like any other", got)
	}

	_, err = c.RunnerFor("ceo", nil, engine.RunnerInput{Task: "post it"})
	if !errors.Is(err, phase.ErrNoProviders) {
		t.Fatalf("RunnerFor = %v, want ErrNoProviders", err)
	}
	for _, want := range []string{"ceo", "providers.llm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %s", err, want)
		}
	}
}

// THE REAL APPLY PATH TAKES IT. The defect this pins: `PUT /config` and its
// dry run accepted a company with no providers.llm, and the reconcile tick on
// every node then refused it with "phase: no LLM providers configured", so a
// company created from the dashboard was stored, activated and run by nobody.
func TestACompanyWithNoModelsIsApplied(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	epoch := p.activate(t.Context(), t, noModelsDoc)

	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("the apply refused a company with no models: %v", err)
	}
	if p.recon.Applied() != epoch {
		t.Fatalf("applied = %d, want %d", p.recon.Applied(), epoch)
	}
	row := p.fleetRow(t)
	if configplane.ApplyStatus(row.Status) != configplane.StatusOK || row.Error != "" {
		t.Errorf("fleet row = %q %q, want ok with no error", row.Status, row.Error)
	}
	if p.engine.Company().Models != nil {
		t.Error("the applied epoch carries a model registry for a company with none")
	}
	// THE SEATS ARE THE CHART'S and the apply does not touch them, so what
	// this checks is that the company still HAS them: an apply that
	// published an epoch composed with no view would leave a node serving a
	// company of nobody, which is the failure a model-less revision is
	// most likely to be mistaken for.
	if got := p.seats(t); len(got) == 0 {
		t.Error("the applied epoch carries no seats at all — a company with " +
			"no models still has its chart, and a node serving nobody looks " +
			"exactly like this case's own subject")
	}
}

// NOTHING IS LOST WHILE THERE IS NO MODEL, AND NOTHING WAITS AFTER ONE ARRIVES.
//
// A delivery to a seat of a company with no models must not run a turn (there
// is nothing to run it on), must not be consumed (the work would be lost), and
// must not spin (a requeue with no pause loops at broker speed). So the inbox
// is paused and the delivery parked. The half this pins as well is the one
// that was missing: the apply that brings a provider lifts that pause, and the
// parked delivery runs. Without it the seat stayed deaf until a restart.
func TestACompanyWithNoModelsHoldsItsWorkUntilAProviderArrives(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var ran [][]string
	d := &engine.Dispatcher{
		Turn: func(_ context.Context, req engine.Request) (turn.Result, error) {
			ids := make([]string, 0, len(req.Events))
			for _, ev := range req.Events {
				ids = append(ids, ev.ID.String())
			}
			mu.Lock()
			ran = append(ran, ids)
			mu.Unlock()
			return turn.Result{}, nil
		},
	}
	turns := func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(ran)
	}
	e := newEngine(t, engine.Options{Company: parsedCompany(t, noModelsDoc), Dispatch: d})
	p := planeFor(t, e)
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(e.Node().Host().Held(), "ceo")
	})

	holds, ok := e.Backends().Queue.(interface {
		PauseHolds(topic, group string) []string
	})
	if !ok {
		t.Fatalf("the queue %T reports no pause holds", e.Backends().Queue)
	}
	paused := func() bool {
		return slices.Contains(holds.PauseHolds(
			topics.AgentInbox("ceo"), topics.AgentInboxGroup("ceo")), "no_turn_engine")
	}

	held := ev("external_notification")
	if err := e.Backends().Queue.Publish(t.Context(), topics.AgentInbox("ceo"), held); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitFor(t, "the seat's inbox to be paused", paused)
	if got := turns(); len(got) != 0 {
		t.Fatalf("a seat with no model ran %d turns: %v", len(got), got)
	}

	p.activate(t.Context(), t, companyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("adding a provider: %v", err)
	}
	waitFor(t, "the held delivery to run", func() bool { return len(turns()) > 0 })
	if got := turns(); !slices.Contains(got[0], held.ID.String()) {
		t.Errorf("the first turn ran %v, want the delivery held while there was no model", got[0])
	}
	if paused() {
		t.Error("the inbox is still paused after a provider arrived")
	}
}
