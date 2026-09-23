package builtin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// ownRecordCompany holds a seat whose NAME is an unbound person's login spelt
// as prose — `jane` is "Jane Doe", and `jane.doe` is somebody the directory
// binds to no seat — so a name sent through the colleague resolver has a
// wrong seat to land on.
func ownRecordCompany() *org.Organization {
	return &org.Organization{Name: "Nimbus", Roles: []*org.Role{
		{Name: "Jane Doe", DeclaredHandle: "jane", Kind: org.KindHuman},
		{Name: "Staff Engineer", DeclaredHandle: "dev"},
		{Name: "Engineering Lead", DeclaredHandle: "lead"},
	}}
}

// ownRecordCallers are the three shapes of caller, and the name each one's
// own record is kept under.
var ownRecordCallers = []struct {
	name   string
	caller iam.Principal
	owner  string
}{
	{"a bound person", iam.Principal{Kind: iam.KindPerson,
		Login: "jane.doe", Seat: "jane"}, "jane"},
	{"an unbound person", iam.Principal{Kind: iam.KindPerson,
		Login: "jane.doe"}, "jane.doe"},
	{"an unbound token", iam.Principal{Kind: iam.KindMachine,
		Login: "token:ops"}, "token:ops"},
}

// as is a context carrying one of those callers with exactly these grants.
func as(caller iam.Principal, grants ...iam.Grant) context.Context {
	caller.ID, caller.Stage, caller.Grants = uuid.New(), iam.StageActive, grants
	caller.Colleague = iam.ColleagueWrite
	return iam.WithPrincipal(context.Background(), caller)
}

// ownRecordTools is the operator surface over the company above, a fake
// tracker, a person spy and a view store, decided by the real table over a
// chart where `lead` leads `dev` and nobody leads anybody else.
func ownRecordTools(t *testing.T, person *personSpy, views *viewStore) map[string]tools.Callable {
	t.Helper()
	trk := newFakeTracker()
	company := ownRecordCompany()
	out := map[string]tools.Callable{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as,
			PersonWriter: func(builtin.Actor) builtin.PersonWriter { return person },
			ViewWriter:   func(builtin.Actor) builtin.ViewWriter { return views },
			Seats:        func() []colleague.Seat { return builtin.Corpus(company) },
			Actor:        builtin.PrincipalActor,
		},
		Authorize: builtin.Decide(handleChart{}),
	}) {
		out[tool.Name()] = tool
	}
	return out
}

// YOUR OWN QUEUE IS NEVER LOOKED UP.
//
// `set_priorities` with no handle — or with the caller's own — sent the
// caller's own name through the fuzzy colleague resolver like anybody else's.
// An unbound `jane.doe` holding the admin grant resolved to the seat `jane`
// and set THAT seat's priorities; the same caller without the grant was
// refused their own queue; and the refusal listed the roster. The queue a
// caller names by omission or by either of their own names is the record
// every other write of theirs is kept under, for each of the three shapes of
// caller, with and without the admin grant.
func TestYourOwnQueueIsNeverLookedUp(t *testing.T) {
	t.Parallel()
	for _, c := range ownRecordCallers {
		for _, grants := range [][]iam.Grant{
			{iam.GrantWorkWrite},
			{iam.GrantWorkWrite, iam.GrantFleetOperate},
		} {
			for _, named := range []string{"", c.caller.Login, c.owner} {
				person := &personSpy{}
				surface := ownRecordTools(t, person, &viewStore{})
				got, err := surface[tracker.SetPrioritiesTool].Call(
					as(c.caller, grants...),
					map[string]any{"handle": named, "items": []any{"ENG-1"}})
				if err != nil {
					t.Fatalf("set_priorities: %v", err)
				}
				switch {
				case got.Failed:
					t.Errorf("%s holding %v, naming %q, was refused their own "+
						"queue: %s", c.name, grants, named, got.Output)
				case person.handle != c.owner:
					t.Errorf("%s holding %v, naming %q, wrote %q's queue — "+
						"their own is %q", c.name, grants, named,
						person.handle, c.owner)
				}
			}
		}
	}
}

// A NAME NOBODY ANSWERS TO IS REFUSED WITHOUT THE ROSTER, to a caller who may
// not read it.
//
// A miss listed every seat in the company, and an ambiguity the seats it
// matched, to whoever made the write — so a caller refused `lookup_colleague`
// read the chart one misspelling at a time. The names are in the refusal when
// the roster question would have been answered, and only then.
func TestAMissedNameListsTheRosterOnlyToAReader(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		grants []iam.Grant
		lists  bool
	}{
		{"a caller who may not read the roster",
			[]iam.Grant{iam.GrantWorkWrite, iam.GrantFleetOperate}, false},
		{"a caller who may", []iam.Grant{iam.GrantWorkWrite,
			iam.GrantFleetOperate, iam.GrantStateRead}, true},
	} {
		surface := ownRecordTools(t, &personSpy{}, &viewStore{})
		got, err := surface[tracker.SetPrioritiesTool].Call(
			as(ownRecordCallers[0].caller, c.grants...),
			map[string]any{"handle": "nobody-by-this-name", "items": []any{"ENG-1"}})
		if err != nil {
			t.Fatalf("set_priorities: %v", err)
		}
		if !got.Failed {
			t.Fatalf("%s: a name nobody answers to was accepted: %s", c.name, got.Output)
		}
		listed := strings.Contains(got.Output, "The seats are") ||
			strings.Contains(got.Output, "lead, ")
		if listed != c.lists {
			t.Errorf("%s: the refusal listed the roster = %v, want %v: %s",
				c.name, listed, c.lists, got.Output)
		}
	}
}

// A PERSONAL VIEW IS THE CALLER'S OWN RECORD.
//
// `save_work_view` took a free `owner` handle, so a bound person typing their
// login saved a view under a name no strip of theirs reads — and one who
// marked it protected was then refused their own next save, which the tracker
// checks against the name the writer acts under. `personal: true` is the only
// way to say it now, and the owner is the one name every other record of theirs
// is kept under.
func TestAPersonalViewIsTheCallersOwnRecord(t *testing.T) {
	t.Parallel()
	for _, c := range ownRecordCallers {
		views := &viewStore{stored: map[string]tracker.View{}}
		surface := ownRecordTools(t, &personSpy{}, views)
		got, err := surface[tracker.SaveWorkViewTool].Call(
			as(c.caller, iam.GrantStateRead, iam.GrantWorkWrite),
			map[string]any{
				"container": "workspace", "name": "Mine", "type": "list",
				"personal": true, "protected": true,
			})
		if err != nil {
			t.Fatalf("save_work_view: %v", err)
		}
		if got.Failed || len(views.saved) != 1 {
			t.Fatalf("%s was refused a personal view: %s", c.name, got.Output)
		}
		if owner := views.saved[0].Owner; owner != c.owner {
			t.Errorf("%s's personal view is %q's, want %q — the name their "+
				"strip is read under", c.name, owner, c.owner)
		}
	}

	// AND THE SCHEMA OFFERS NO NAME TO GIVE IT: a personal view is always
	// the caller's own, so there is no argument through which to name
	// somebody else's.
	for _, tool := range ownRecordTools(t, &personSpy{}, &viewStore{}) {
		if tool.Name() != tracker.SaveWorkViewTool {
			continue
		}
		properties, _ := tool.Parameters()["properties"].(map[string]any)
		if _, held := properties["owner"]; held {
			t.Error("save_work_view still declares a free `owner` handle")
		}
	}
}

// SOMEBODY ELSE'S RECORD IS WRITTEN EXACTLY AS IT WAS DECIDED.
//
// [builtin.MarkInboxFor] and [builtin.SetPinsFor] are handed a record the HTTP
// route has ALREADY decided on — the owner, or the admin path — and both sent
// it through the fuzzy colleague resolver afterwards, so an administrator
// unsticking the unbound `jane.doe`'s queue wrote into the seat `jane`'s: a
// write into a record nobody decided on. What they refuse instead is a name no
// record could be kept under.
func TestSomebodyElsesRecordIsWrittenExactlyAsDecided(t *testing.T) {
	t.Parallel()
	company := ownRecordCompany()
	deps := builtin.WorkDeps{
		Seats: func() []colleague.Seat { return builtin.Corpus(company) },
		Actor: builtin.PrincipalActor,
	}
	admin := as(iam.Principal{Kind: iam.KindMachine, Login: "token:admin"},
		iam.GrantFleetOperate)
	authority := tracker.PersonAuthority{Authorized: true}
	for _, write := range []struct {
		name string
		fn   func(context.Context, builtin.WorkDeps, string, map[string]any,
			tracker.PersonAuthority) tools.Result
	}{
		{"an inbox mark", builtin.MarkInboxFor},
		{"a pin set", builtin.SetPinsFor},
	} {
		for _, c := range []struct {
			handle  string
			written string
		}{
			{"jane.doe", "jane.doe"},
			{"token:ci", "token:ci"},
			{"dev", "dev"},
			// A NAME NO RECORD IS KEPT UNDER is refused rather than
			// resolved: prose, and a seat-shaped name the chart lacks.
			{"Jane Doe", ""},
			{"nobody", ""},
		} {
			person := &personSpy{}
			deps.PersonWriter = func(builtin.Actor) builtin.PersonWriter { return person }
			got := write.fn(admin, deps, c.handle, map[string]any{}, authority)
			switch {
			case c.written == "" && (!got.Failed || person.handle != ""):
				t.Errorf("%s decided on %q wrote %q's record: %s", write.name,
					c.handle, person.handle, got.Output)
			case c.written != "" && got.Failed:
				t.Errorf("%s decided on %q was refused: %s", write.name,
					c.handle, got.Output)
			case person.handle != c.written:
				t.Errorf("%s decided on %q wrote %q's record, want exactly "+
					"the one decided on", write.name, c.handle, person.handle)
			}
		}
	}
}
