package builtin_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// recordSpy records whose record each person write landed on.
type recordSpy struct {
	mu      sync.Mutex
	written []string
}

func (s *recordSpy) wrote(handle string) (worklet.WriteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, handle)
	return worklet.WriteResult{Result: statelog.Result{
		Outcome: statelog.OutcomeApplied,
	}}, nil
}

func (s *recordSpy) MarkInbox(_ context.Context, _, handle string, _ statelog.Position,
	_ inbox.Gesture[worklet.Reason], _ worklet.PersonAuthority) (worklet.WriteResult, error) {

	return s.wrote(handle)
}

func (s *recordSpy) WritePins(_ context.Context, _, handle string,
	_ worklet.PinGesture, _ worklet.PersonAuthority) (worklet.WriteResult, error) {

	return s.wrote(handle)
}

func (s *recordSpy) WritePriorities(_ context.Context, _, handle string, _ []string,
	_ *uint64, _ worklet.PersonAuthority) (worklet.WriteResult, error) {

	return s.wrote(handle)
}

// A VIEW STRIP IS ITS READER'S, AND NOBODY ELSE'S IS REACHABLE: it is rendered
// under the SAME name the caller's pins and personal views are written with,
// and a `viewer` sent anyway is REFUSED by name rather than dropped, so a
// caller who meant somebody else's strip is not shown their own as if it were.
func TestAViewStripIsTheCallersOwn(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		caller iam.Principal
		want   string
	}{
		{"a person bound to a seat", iam.Principal{
			Login: "ana.silva", Seat: "ana", Kind: iam.KindPerson,
		}, "ana"},
		{"an unbound token", iam.Principal{
			Login: "token:ops", Kind: iam.KindMachine,
		}, "token:ops"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			caller := c.caller
			caller.ID, caller.Stage = uuid.New(), iam.StageActive
			caller.Grants = []iam.Grant{iam.GrantWorklet}
			trk := newFakeTracker()
			var listed bool
			for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
				Work: builtin.WorkDeps{
					Reader: trk, Writer: trk.as, Actor: builtin.PrincipalActor,
				},
				Authorize: builtin.Decide(chartRefuses),
			}) {
				if tool.Name() != worklet.ListWorkViewsTool {
					continue
				}
				if _, declared := tool.Parameters()["properties"].(map[string]any)["viewer"]; declared {
					t.Error("list_work_views still declares a `viewer`, so a " +
						"caller can still name whose strip to render")
				}
				ctx := iam.WithPrincipal(context.Background(), caller)
				named, err := tool.Call(ctx,
					map[string]any{"container": "project:ENG", "viewer": "bob"})
				if err != nil {
					t.Fatalf("list_work_views: %v", err)
				}
				if !named.Failed || !errors.Is(named.Cause, builtin.ErrUndeclaredArgument) ||
					!strings.Contains(named.Output, `"viewer"`) {
					t.Fatalf("list_work_views naming bob's strip answered %q "+
						"(cause %v), want the argument refused by name",
						named.Output, named.Cause)
				}
				if got := trk.viewQuery.Viewer; got != "" {
					t.Fatalf("the refused call rendered %q's strip", got)
				}
				res, err := tool.Call(ctx, map[string]any{"container": "project:ENG"})
				if err != nil {
					t.Fatalf("list_work_views: %v", err)
				}
				if res.Failed {
					t.Fatalf("list_work_views refused: %s", res.Output)
				}
				listed = true
			}
			if !listed {
				t.Fatal("list_work_views is not on the operator surface")
			}
			if got := trk.viewQuery.Viewer; got != c.want {
				t.Errorf("the strip rendered is %q's, want the caller's own %q", got, c.want)
			}
		})
	}
}

// A PERSONAL VERB IS DECIDED ON THE RECORD IT WRITES.
//
// `my_work`, `mark_inbox` and `set_pins` name nobody: the gate decides them on
// the CALLER's own record and the tool reads or writes under the actor's
// handle, which must be one value, or a call authorised on one record lands on
// another. Three callers: a person bound to a seat (the SEAT), a Tier A token
// spelled like a seat (its whole LOGIN, never the seat's handle), and a machine
// credential carrying a seat (its LOGIN). Each compares the owner the authority
// was ASKED about with the handle the record was read or written under.
func TestAPersonalVerbIsDecidedOnTheRecordItWrites(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		caller iam.Principal
		want   string
	}{
		{"a person bound to a seat", iam.Principal{
			Login: "ana.silva", Seat: "ana", Kind: iam.KindPerson,
		}, "ana"},
		{"a token spelled like a seat", iam.Principal{
			Login: "token:ana", Kind: iam.KindMachine,
		}, "token:ana"},
		{"a machine carrying a seat", iam.Principal{
			Login: "token:ci", Seat: "ana", Kind: iam.KindMachine,
		}, "token:ci"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			caller := c.caller
			caller.ID, caller.Stage = uuid.New(), iam.StageActive
			caller.Grants = []iam.Grant{iam.GrantWorklet}
			ctx := iam.WithPrincipal(context.Background(), caller)

			var mu sync.Mutex
			var asked []string
			decide := builtin.Decide(chartRefuses)
			spy := &recordSpy{}
			trk := newFakeTracker()
			for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
				Work: builtin.WorkDeps{
					Reader: trk, Writer: trk.as,
					PersonWriter: func(builtin.Actor) builtin.PersonWriter { return spy },
					Actor:        builtin.PrincipalActor,
				},
				Authorize: func(ctx context.Context, action authz.Action,
					object authz.Object) error {

					mu.Lock()
					asked = append(asked, object.Owner)
					mu.Unlock()
					return decide(ctx, action, object)
				},
			}) {
				// ONE GESTURE EACH, the smallest that is a change: a
				// mark naming nothing is refused before it is decided.
				args := map[string]any{}
				switch tool.Name() {
				case worklet.MarkInboxTool:
					args["read"] = []any{"r-1"}
				case worklet.SetPinsTool:
					args["views"] = map[string]any{"add": []any{"v-1"}}
				case worklet.MyWorkTool:
				default:
					continue
				}
				res, err := tool.Call(ctx, args)
				if err != nil {
					t.Fatalf("%s: %v", tool.Name(), err)
				}
				if res.Failed {
					t.Fatalf("%s refused the caller their own record: %s",
						tool.Name(), res.Output)
				}
			}
			if len(asked) != 3 {
				t.Fatalf("the authority was asked %d times, want once per verb", len(asked))
			}
			for _, owner := range asked {
				if owner != c.want {
					t.Errorf("the authority was asked about %q's record, want %q",
						owner, c.want)
				}
			}
			written := append(spy.written, trk.myWorkQuery.Handle)
			for _, handle := range written {
				if handle != c.want {
					t.Errorf("the record read or written is %q's, want %q — the "+
						"call was decided on one record and landed on another",
						handle, c.want)
				}
			}
		})
	}
}
