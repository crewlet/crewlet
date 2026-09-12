package builtin_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN OPERATOR WRITING SOMEBODY'S PRIORITIES CARRIES THE AUTHORITY TO DO IT.
//
// This is the wiring that decided whether the `prioritised` wake could fire at
// all. `set_priorities` is registered on the operator MCP alone, and an
// operator's actor is an API TOKEN's name — never a handle in the org chart —
// so the lead lookup can never match it. With the tool sending only `Lead`,
// the writer refused every cross-person write through the only surface that
// exists, and omitting the handle silently wrote a person record for the
// token instead of for a person.
func TestAnOperatorCarriesTheAuthorityToWriteAPersonsPriorities(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind       tracker.AuthorKind
		wantPerson bool
	}{
		"an operator token": {tracker.AuthorOperator, true},
		"a human":           {tracker.AuthorHuman, true},
		// A SEAT CARRIES NEITHER unless it genuinely leads the handle,
		// which the lead seam answers separately.
		"a seat": {tracker.AuthorAgent, false},
	} {
		t.Run(name, func(t *testing.T) {
			person := &personSpy{}
			reg := tools.NewRegistry()
			for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
				Work: builtin.WorkDeps{
					Reader:       newFakeTracker(),
					Writer:       newFakeTracker().as,
					PersonWriter: func(builtin.Actor) builtin.PersonWriter { return person },
					Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
						return builtin.Actor{Handle: "ops", Kind: tc.kind}, nil
					},
				},
			}) {
				if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
					t.Fatalf("register %s: %v", tool.Name(), err)
				}
			}
			got := callWork(t, reg, tracker.SetPrioritiesTool, map[string]any{
				"handle": "alice", "items": []any{"ENG-1"},
			})
			if got.Failed {
				t.Fatalf("set_priorities failed: %q", got.Output)
			}
			if person.authority.Person != tc.wantPerson {
				t.Errorf("authority.Person = %v, want %v — %s writing "+
					"somebody else's queue reaches the writer's gate with "+
					"this and nothing else",
					person.authority.Person, tc.wantPerson, name)
			}
		})
	}
}

// A PRIORITY ENTRY GIVEN AS A KEY IS RESOLVED TO AN ID.
//
// The list is stored as ids and read back by joining on them, and this tool's
// own description invites a key. An unresolved `ENG-1` failed in three places
// at once and reported nothing anywhere: the entry vanished from `my_work`, it
// vanished from `preset=priorities`, and the wake that tells the person their
// queue changed was silently suppressed — while the call answered
// `outcome: applied`.
func TestAPriorityGivenAsAKeyIsResolved(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	person := &personSpy{}
	reg := operatorRegistry(t, trk, person)

	got := callWork(t, reg, tracker.SetPrioritiesTool, map[string]any{
		"handle": "alice", "items": []any{"ENG-1"},
	})
	if got.Failed {
		t.Fatalf("set_priorities failed: %q", got.Output)
	}
	if len(person.priorities) != 1 || person.priorities[0] != "i1" {
		t.Fatalf("the writer was given %v, want the task's id — a key never "+
			"joins, so the entry is stored and then invisible everywhere",
			person.priorities)
	}
}

// AND ONE THAT NAMES NOTHING IS REFUSED rather than stored: a queue entry
// pointing at no task is one the person never sees and nobody is told about.
func TestAPriorityNamingNoTaskIsRefused(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	person := &personSpy{}
	reg := operatorRegistry(t, trk, person)

	got := callWork(t, reg, tracker.SetPrioritiesTool, map[string]any{
		"handle": "alice", "items": []any{"ENG-404"},
	})
	if !got.Failed {
		t.Fatalf("a priority naming no task was stored: %q", got.Output)
	}
	if len(person.priorities) != 0 {
		t.Fatalf("the writer was still called with %v", person.priorities)
	}
}

// operatorRegistry is the operator surface over a fake tracker and a person
// spy, which is the only surface set_priorities is registered on.
func operatorRegistry(t *testing.T, trk *fakeTracker, person builtin.PersonWriter) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader:       trk,
			Writer:       trk.as,
			PersonWriter: func(builtin.Actor) builtin.PersonWriter { return person },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator}, nil
			},
		},
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

type personSpy struct {
	handle     string
	priorities []string
	authority  tracker.PersonAuthority
}

func (p *personSpy) WritePriorities(_ context.Context, _, handle string,
	priorities []string, authority tracker.PersonAuthority) (
	tracker.WriteResult, error) {

	p.handle, p.priorities, p.authority = handle, priorities, authority
	return tracker.WriteResult{
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 20},
	}, nil
}

func (p *personSpy) WritePins(_ context.Context, _, _ string, _ []string,
	_ []tracker.Favorite) (
	tracker.WriteResult, error) {

	return tracker.WriteResult{Outcome: statelog.OutcomeApplied}, nil
}

func (p *personSpy) WriteInbox(_ context.Context, _, _ string,
	_, _, _ []tracker.InboxEntry, _ []tracker.Reason, _ tracker.Position) (
	tracker.WriteResult, error) {

	return tracker.WriteResult{Outcome: statelog.OutcomeApplied}, nil
}
