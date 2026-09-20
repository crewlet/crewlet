package builtin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A HANDLE NOBODY HAS IS REFUSED, AT EVERY ARGUMENT THAT TAKES ONE.
//
// An unknown handle fails silently and permanently: it is stored, it rides the
// routing snapshot, it becomes a candidate — and Route drops it against the
// live roster with no error, no per-candidate log and no metric. The write
// answers `outcome: applied` and the person it named never hears anything, so
// a misspelling is indistinguishable from a colleague who is simply quiet.
//
// The table is every live site rather than the one the finding named, because
// the hole was systemic, and the assignee is the one that matters most.
func TestAHandleNobodyHasIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		tool string
		args map[string]any
	}{
		"a create's assignee": {tracker.CreateWorkItemTool, map[string]any{
			"title": "A task", "assignee": "nobody-here",
		}},
		"an update's assignee": {tracker.UpdateWorkItemTool, map[string]any{
			"item": "ENG-1", "assignee": "nobody-here",
		}},
		"a priority list's owner": {tracker.SetPrioritiesTool, map[string]any{
			"handle": "nobody-here", "items": []any{"ENG-1"},
		}},
		"a project's default assignee": {tracker.WriteProjectTool, map[string]any{
			"project": "ENG", "default_assignee": "nobody-here",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			reg := rosterRegistry(t, newFakeTracker())
			got := callWork(t, reg, tc.tool, tc.args)
			if !got.Failed {
				t.Fatalf("%s accepted a handle nobody has: %q", tc.tool, got.Output)
			}
			// THE REFUSAL NAMES THE WAY OUT, because the caller is
			// usually a model with one round left.
			if !strings.Contains(got.Output, builtin.LookupColleagueTool) &&
				!strings.Contains(got.Output, "alice") {

				t.Errorf("the refusal names neither the lookup nor the "+
					"seats: %q", got.Output)
			}
		})
	}
}

// AND A HANDLE THE COMPANY HAS IS ACCEPTED, or the table above passes for the
// empty reason that every call fails.
func TestAKnownHandleIsAccepted(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := rosterRegistry(t, trk)
	got := callWork(t, reg, tracker.CreateWorkItemTool, map[string]any{
		"title": "A task", "assignee": "alice", "project": "ENG",
	})
	if got.Failed {
		t.Fatalf("a known handle was refused: %q", got.Output)
	}
	if len(trk.created) != 1 || trk.created[0].Assignee != "alice" {
		t.Fatalf("the task was filed with assignee %q", trk.created[0].Assignee)
	}
}

// A HANDLE IS CANONICALISED, not merely checked: a caller that typed a role's
// NAME gets the handle rather than a refusal it cannot act on.
func TestAHandleIsResolvedToItsCanonicalForm(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := rosterRegistry(t, trk)
	got := callWork(t, reg, tracker.CreateWorkItemTool, map[string]any{
		"title": "A task", "assignee": "Alice Engineer", "project": "ENG",
	})
	if got.Failed {
		t.Fatalf("a role name was refused: %q", got.Output)
	}
	if trk.created[0].Assignee != "alice" {
		t.Errorf("filed under %q, want the canonical handle — a task watched "+
			"under one spelling and assigned under another is one whose "+
			"assignee is not following it", trk.created[0].Assignee)
	}
}

// A SURFACE WITH NO ROSTER ADMITS EVERYTHING, which is the honest degradation:
// a build with no chart loaded cannot tell a typo from a colleague, and
// refusing every handle there would be worse than the hole this closes.
func TestNoRosterAdmitsEveryHandle(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as,
			DefaultProject: func(string) string { return "ENG" },
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := callWork(t, reg, tracker.CreateWorkItemTool, map[string]any{
		"title": "A task", "assignee": "anybody",
	}); got.Failed {
		t.Fatalf("a build with no chart refused a handle: %q", got.Output)
	}
}

// rosterRegistry is the operator surface over a fake tracker and a two-seat
// company, which is what the handle checks resolve against.
func rosterRegistry(t *testing.T, trk *fakeTracker) *tools.Registry {
	t.Helper()
	seats := []colleague.Seat{
		{Handle: "alice", Name: "Alice Engineer", Kind: "agent"},
		{Handle: "bob", Name: "Bob Designer", Kind: "agent"},
	}
	reg := tools.NewRegistry()
	person := &personSpy{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as,
			PersonWriter:   func(builtin.Actor) builtin.PersonWriter { return person },
			ProjectWriter:  func(builtin.Actor) builtin.ProjectWriter { return trk },
			Seats:          func() []colleague.Seat { return seats },
			DefaultProject: func(string) string { return "ENG" },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator}, nil
			},
		},
		LeadsProject: leadAlways,
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}
