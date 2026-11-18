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
	mu      sync.Mutex
	stored  map[string]worklet.View
	saved   []worklet.View
	deleted []string
	priors  []worklet.ViewPrior
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

func (s *viewStore) DeleteView(_ context.Context, _, id string,
	prior worklet.ViewPrior) (worklet.WriteResult, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, id)
	s.priors = append(s.priors, prior)
	return worklet.WriteResult{Result: statelog.Result{
		Outcome: statelog.OutcomeApplied,
	}}, nil
}

// A SAVE IS DECIDED ON THE VIEW IT REPLACES AS WELL AS THE ONE IT WRITES.
//
// `save_work_view` replaces a view whole, by id, so a gate on the arguments
// alone would let anybody overwrite a project's SHARED tab or a colleague's
// personal view. The tool asks the same action on the stored row's owner and
// container, and hands the write that prior, so a view that changed in
// between is refused. Controls, so a tool refusing every replacement fails:
// the project's lead replaces the shared tab, and a save naming no id is the
// caller's own new view.
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
// The argument is "the view to replace", so only an id a create minted
// reaches the write; any other string would become a primary key and the
// subject writes arbitrate on. Not found, not an authority refusal: there is
// nothing there to be refused.
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

// A DELETE IS DECIDED ON THE STORED VIEW, as the save that would replace it is.
//
// `delete_work_view` names only an id, so the view it takes away is a row the
// gate cannot form an object from. The tool reads it and asks the delete's
// action of its owner and container: a non-lead is refused a project's shared
// tab and a colleague somebody's personal view, the project's lead deletes the
// shared tab, and an id no view holds is not found, never a write.
func TestADeleteIsDecidedOnTheViewItTakesAway(t *testing.T) {
	t.Parallel()
	eng := worklet.Container{Kind: worklet.ContainerProject, ID: "ENG"}
	stored := map[string]worklet.View{
		"v-shared": {ID: "v-shared", Container: eng, Name: "Board"},
		"v-bob":    {ID: "v-bob", Container: eng, Name: "Mine", Owner: "bob"},
	}
	for _, c := range []struct {
		name    string
		id      string
		chart   authz.Chart
		allowed bool
		refusal tools.Refusal
	}{
		{"a non-lead deleting a project's shared tab", "v-shared", chartRefuses, false, ""},
		{"a colleague deleting somebody's personal view", "v-bob", chartLeads, false, ""},
		{"the project's lead deleting its shared tab", "v-shared", chartLeads, true, ""},
		{"an id no view holds", "v-made-up", chartLeads, false, tools.RefusalNotFound},
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
				if candidate.Name() == worklet.DeleteWorkViewTool {
					tool = candidate
				}
			}
			if tool == nil {
				t.Fatal("delete_work_view is not on the operator surface")
			}
			got, err := tool.Call(personHolding(iam.GrantWorklet), map[string]any{"id": c.id})
			if err != nil {
				t.Fatalf("delete_work_view: %v", err)
			}
			switch {
			case c.allowed && (got.Failed || len(views.deleted) != 1):
				t.Fatalf("refused: %s", got.Output)
			case !c.allowed && (!got.Failed || len(views.deleted) != 0):
				t.Fatalf("the delete landed on a view the caller may not delete: %s", got.Output)
			case c.refusal != "" && tools.RefusalOf(got) != c.refusal:
				t.Fatalf("refused as %v, want %v: %s", tools.RefusalOf(got), c.refusal, got.Output)
			case !c.allowed && c.refusal == "" && !errors.Is(got.Cause, builtin.ErrRefused):
				t.Fatalf("the refusal carries %v, not the authority's own answer", got.Cause)
			}
			if c.allowed {
				want := stored[c.id]
				if prior := views.priors[0]; !prior.Held || prior.Owner != want.Owner ||
					prior.Container != want.Container {
					t.Errorf("the delete was handed the prior %+v, want the stored view", prior)
				}
			}
		})
	}
}
