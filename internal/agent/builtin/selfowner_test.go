package builtin_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// recordSpy records whose record each person write landed on.
type recordSpy struct {
	mu      sync.Mutex
	written []string
}

func (s *recordSpy) wrote(handle string) (tracker.WriteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, handle)
	return tracker.WriteResult{Result: statelog.Result{
		Outcome: statelog.OutcomeApplied,
	}}, nil
}

func (s *recordSpy) WriteInbox(_ context.Context, _, handle string,
	_, _, _ []tracker.InboxEntry, _ []tracker.Reason, _ tracker.Position,
	_ tracker.PersonAuthority) (tracker.WriteResult, error) {

	return s.wrote(handle)
}

func (s *recordSpy) WritePins(_ context.Context, _, handle string, _ []string,
	_ []tracker.Favorite, _ tracker.PersonAuthority) (tracker.WriteResult, error) {

	return s.wrote(handle)
}

func (s *recordSpy) WritePriorities(_ context.Context, _, handle string, _ []string,
	_ tracker.PersonAuthority) (tracker.WriteResult, error) {

	return s.wrote(handle)
}

// A VIEW STRIP IS ITS READER'S, AND NOBODY ELSE'S IS REACHABLE.
//
// `list_work_views` took a free `viewer` argument, so any caller — a seat, or
// the operator's assistant — rendered another person's personal views and
// pins by naming them. The strip is the caller's own now, under the SAME name
// their pins and personal views are written with, so it shows exactly what
// they arranged; a `viewer` sent anyway names nothing the tool reads.
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
			caller.Grants = []iam.Grant{iam.GrantStateRead}
			trk := newFakeTracker()
			var listed bool
			for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
				Work: builtin.WorkDeps{
					Reader: trk, Writer: trk.as, Actor: builtin.PrincipalActor,
				},
				Authorize: builtin.Decide(chartRefuses),
			}) {
				if tool.Name() != tracker.ListWorkViewsTool {
					continue
				}
				if _, declared := tool.Parameters()["properties"].(map[string]any)["viewer"]; declared {
					t.Error("list_work_views still declares a `viewer`, so a " +
						"caller can still name whose strip to render")
				}
				res, err := tool.Call(iam.WithPrincipal(context.Background(), caller),
					map[string]any{"container": "project:ENG", "viewer": "bob"})
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
// the CALLER's own record, and the tool then reads or writes the record under
// the actor's handle. Those have to be one value, or the call is authorised
// against one person's record and lands on another's — and they were two
// functions that happened to agree, one reading the seat before the login and
// the other a machine's login and never its seat.
//
// Three callers: a person bound to a seat (both are the SEAT), a Tier A token
// whose id is spelled like a seat in this company (both are its whole LOGIN,
// never the seat's handle), and a machine credential carrying a seat, which is
// the one the two copies disagreed on. Each is checked by the owner the
// authority was actually ASKED about against the handle the record was read
// or written under.
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
			caller.Grants = []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite}
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
				switch tool.Name() {
				case tracker.MarkInboxTool, tracker.SetPinsTool, tracker.MyWorkTool:
				default:
					continue
				}
				res, err := tool.Call(ctx, map[string]any{})
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
