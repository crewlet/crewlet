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
	"github.com/crewlet/crewlet/internal/worklet"
)

// viewStore is the saved-view write side over a table of stored views: it
// answers the prior a save is decided on and records every save it is asked
// for, so a case can tell a refusal from a write that landed.
type viewStore struct {
	mu     sync.Mutex
	stored map[string]worklet.View
	saved  []worklet.View
	priors []worklet.ViewPrior
}

func (s *viewStore) ViewPrior(_ context.Context, id string) (worklet.ViewPrior, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held, ok := s.stored[id]
	if !ok {
		return worklet.ViewPrior{}, nil
	}
	return worklet.ViewPrior{Held: true, Owner: held.Owner, Container: held.Container}, nil
}

func (s *viewStore) WriteView(_ context.Context, _ string, view worklet.View,
	prior worklet.ViewPrior) (worklet.WriteResult, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, view)
	s.priors = append(s.priors, prior)
	return worklet.WriteResult{Result: statelog.Result{
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
// and a save naming no id is the caller's own new view, or a tool that refused
// every replacement would pass.
func TestASaveCannotTakeOverAViewItMayNotWrite(t *testing.T) {
	t.Parallel()
	eng := worklet.Container{Kind: worklet.ContainerProject, ID: "ENG"}
	stored := map[string]worklet.View{
		"v-shared": {ID: "v-shared", Container: eng, Name: "Board"},
		"v-bob":    {ID: "v-bob", Container: eng, Name: "Mine", Owner: "bob"},
	}
	mine := map[string]any{
		"container": "person:tester", "name": "Mine now", "type": "list",
		"personal": true,
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
		{"no id is the caller's own new view", "", chartRefuses, mine, true},
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
				if candidate.Name() == worklet.SaveWorkViewTool {
					tool = candidate
				}
			}
			if tool == nil {
				t.Fatal("save_work_view is not on the operator surface")
			}
			args := map[string]any{}
			if c.id != "" {
				args["id"] = c.id
			}
			for key, value := range c.args {
				args[key] = value
			}
			got, err := tool.Call(personHolding(iam.GrantWorklet), args)
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

// A SAVE NAMING AN ID NO VIEW HOLDS IS REFUSED, and writes nothing.
//
// The argument is "the view to replace", and a view's id is the one its
// create minted. A save that took any string for a missing view CREATED one
// under it — dots, spaces and a path were all a primary key and the subject
// its writes arbitrate on — so the only ids that reach the write are ones this
// engine minted. Refused as not found rather than as an authority refusal,
// because there is nothing the caller may not do: there is nothing there.
func TestASaveNamingNoViewIsRefused(t *testing.T) {
	t.Parallel()
	views := &viewStore{stored: map[string]worklet.View{}}
	var tool tools.Callable
	for _, candidate := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: newFakeTracker(), Writer: newFakeTracker().as,
			ViewWriter: func(builtin.Actor) builtin.ViewWriter { return views },
			Actor:      builtin.PrincipalActor,
		},
		Authorize: builtin.Decide(chartLeads),
	}) {
		if candidate.Name() == worklet.SaveWorkViewTool {
			tool = candidate
		}
	}
	for _, id := range []string{"v-made-up", "ENG/docs/My Notes.md", "a.b.c"} {
		got, err := tool.Call(personHolding(iam.GrantWorklet), map[string]any{
			"id": id, "container": "person:tester", "name": "Mine", "type": "list",
			"personal": true,
		})
		if err != nil {
			t.Fatalf("save_work_view %q: %v", id, err)
		}
		if !got.Failed || len(views.saved) != 0 {
			t.Fatalf("a save naming the missing view %q landed: %s", id, got.Output)
		}
		if class := tools.RefusalOf(got); class != tools.RefusalNotFound {
			t.Errorf("a save naming the missing view %q was refused as %v, want "+
				"not found: %s", id, class, got.Output)
		}
	}
}
