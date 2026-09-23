package builtin_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// viewStore is the saved-view write side over a table of stored views: it
// answers the prior a save is decided on and records every save it is asked
// for, so a case can tell a refusal from a write that landed.
type viewStore struct {
	mu     sync.Mutex
	stored map[string]tracker.View
	saved  []tracker.View
	priors []tracker.ViewPrior
}

func (s *viewStore) ViewPrior(_ context.Context, id string) (tracker.ViewPrior, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held, ok := s.stored[id]
	if !ok {
		return tracker.ViewPrior{}, nil
	}
	return tracker.ViewPrior{Held: true, Owner: held.Owner, Container: held.Container}, nil
}

func (s *viewStore) WriteView(_ context.Context, _ string, view tracker.View,
	prior tracker.ViewPrior) (tracker.WriteResult, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, view)
	s.priors = append(s.priors, prior)
	return tracker.WriteResult{Result: statelog.Result{
		Outcome: statelog.OutcomeApplied,
	}}, nil
}

// A SAVE IS DECIDED ON THE VIEW IT REPLACES AS WELL AS THE ONE IT WRITES.
//
// `save_work_view` replaces a view whole, by id, and its gate decided only on
// the arguments — so anybody who may keep a view of their own could name any
// id and overwrite it with one: a project's SHARED tab taken over by somebody
// who does not lead the project, or a colleague's personal view rewritten as
// the caller's. The stored view is a row, and the tool asks the same action on
// its owner and container once it has read it — and hands the write the prior
// it decided on, so the write refuses a view that changed in between.
//
// Both refusals, and the controls: the project's lead replaces the shared tab,
// and a fresh id is the caller's own view, or a tool that refused every
// replacement would pass.
func TestASaveCannotTakeOverAViewItMayNotWrite(t *testing.T) {
	t.Parallel()
	eng := tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"}
	stored := map[string]tracker.View{
		"v-shared": {ID: "v-shared", Container: eng, Name: "Board"},
		"v-bob":    {ID: "v-bob", Container: eng, Name: "Mine", Owner: "bob"},
	}
	mine := map[string]any{
		"container": "person:tester", "name": "Mine now", "type": "list",
		"owner": "tester",
	}
	for _, c := range []struct {
		name    string
		id      string
		chart   authz.Chart
		args    map[string]any
		allowed bool
	}{
		{"a non-lead taking a project's shared tab", "v-shared", chartRefuses, mine, false},
		{"a colleague taking somebody's personal view", "v-bob", chartLeads, mine, false},
		{"the project's lead replacing its shared tab", "v-shared", chartLeads,
			map[string]any{"container": "project:ENG", "name": "Board v2", "type": "board"},
			true},
		{"a fresh id is the caller's own view", "v-new", chartRefuses, mine, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			views := &viewStore{stored: stored}
			var tool tools.Callable
			for _, candidate := range builtin.OperatorTools(builtin.OperatorDeps{
				Work: builtin.WorkDeps{
					Reader: newFakeTracker(), Writer: newFakeTracker().as,
					ViewWriter: func(builtin.Actor) builtin.ViewWriter { return views },
					Actor:      builtin.PrincipalActor,
				},
				Authorize: builtin.Decide(c.chart),
			}) {
				if candidate.Name() == tracker.SaveWorkViewTool {
					tool = candidate
				}
			}
			if tool == nil {
				t.Fatal("save_work_view is not on the operator surface")
			}
			args := map[string]any{"id": c.id}
			for key, value := range c.args {
				args[key] = value
			}
			got, err := tool.Call(personHolding(iam.GrantStateRead, iam.GrantWorkWrite), args)
			if err != nil {
				t.Fatalf("save_work_view: %v", err)
			}
			switch {
			case c.allowed && (got.Failed || len(views.saved) != 1):
				t.Fatalf("refused: %s", got.Output)
			case !c.allowed && (!got.Failed || len(views.saved) != 0):
				t.Fatalf("the save landed on a view the caller may not write: %s",
					got.Output)
			case !c.allowed && !errors.Is(got.Cause, builtin.ErrRefused):
				t.Fatalf("the refusal carries %v, not the authority's own answer",
					got.Cause)
			}
			// AND THE WRITE IS HANDED WHAT THE DECISION WAS TAKEN ON,
			// which is what lets it refuse a view that moved between.
			if c.allowed {
				want, held := stored[c.id]
				if prior := views.priors[0]; prior.Held != held ||
					prior.Owner != want.Owner || prior.Container != want.Container {
					t.Errorf("the save was handed the prior %+v, want the "+
						"stored view it replaces", prior)
				}
			}
		})
	}
}
