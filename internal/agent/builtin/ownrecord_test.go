package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			Seats:        func() []colleague.Seat { return builtin.Corpus(company, nil) },
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

// directory is the identity directory as a person verb reads it, over
// [ownRecordCompany]: `jane.doe` holds no seat — the finding's own case, a
// login that RESEMBLES the seat `jane` ("Jane Doe") — `j.bound` holds the seat
// `jane`, `dev.person` holds `dev`, and `leaver.person` is bound to a seat the
// chart has since removed. err makes every lookup unknown.
type directory struct {
	err   error
	asked []string
}

func (d *directory) HolderRecord(_ context.Context, login string) (string, error) {
	d.asked = append(d.asked, login)
	if d.err != nil {
		return "", d.err
	}
	switch login {
	case "jane.doe":
		return "jane.doe", nil
	case "j.bound":
		return "jane", nil
	case "dev.person":
		return "dev", nil
	case "leaver.person":
		return "", fmt.Errorf("%w: %s", iam.ErrHolderUnseated, login)
	}
	return "", fmt.Errorf("%w: %s", iam.ErrNoHolder, login)
}

// personVerb is one of the five verbs that name whose record they act on, as
// the surface a caller reaches it through: the three tools, and the two HTTP
// writes that reach the tools' writers.
type personVerb struct {
	name  string
	write bool
	// ownerOnly marks the two the table gives the record's owner alone —
	// marking somebody's inbox and pinning their views are gestures nobody
	// asked a lead to make.
	ownerOnly bool
	call      func(ctx context.Context, deps builtin.WorkDeps, name string) tools.Result
}

// personVerbs are the five.
func personVerbs() []personVerb {
	tool := func(verb string, args func(string) map[string]any) func(
		context.Context, builtin.WorkDeps, string) tools.Result {

		return func(ctx context.Context, deps builtin.WorkDeps, name string) tools.Result {
			for _, t := range builtin.OperatorTools(builtin.OperatorDeps{
				Work: deps, Authorize: deps.Authorize,
			}) {
				if t.Name() == verb {
					got, err := t.Call(ctx, args(name))
					if err != nil {
						panic(err)
					}
					return got
				}
			}
			panic(verb + " is not served")
		}
	}
	return []personVerb{
		{name: tracker.GetPersonTool, call: tool(tracker.GetPersonTool,
			func(n string) map[string]any { return map[string]any{"handle": n} })},
		{name: tracker.WorkInboxTool, call: tool(tracker.WorkInboxTool,
			func(n string) map[string]any { return map[string]any{"handle": n} })},
		{name: tracker.SetPrioritiesTool, write: true, call: tool(tracker.SetPrioritiesTool,
			func(n string) map[string]any {
				return map[string]any{"handle": n, "items": []any{"ENG-1"}}
			})},
		{name: "MarkInboxFor", write: true, ownerOnly: true,
			call: func(ctx context.Context, deps builtin.WorkDeps, n string) tools.Result {
				return builtin.MarkInboxFor(ctx, deps, n, map[string]any{})
			}},
		{name: "SetPinsFor", write: true, ownerOnly: true,
			call: func(ctx context.Context, deps builtin.WorkDeps, n string) tools.Result {
				return builtin.SetPinsFor(ctx, deps, n, map[string]any{})
			}},
	}
}

// personDeps is the operator surface's deps over [ownRecordCompany], the
// directory above and a person spy, decided by the real table over a chart
// where `lead` leads `dev`.
func personDeps(person *personSpy, dir *directory) builtin.WorkDeps {
	trk := newFakeTracker()
	company := ownRecordCompany()
	return builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Inbox: trk,
		PersonWriter: func(builtin.Actor) builtin.PersonWriter { return person },
		Seats:        func() []colleague.Seat { return builtin.Corpus(company, nil) },
		Actor:        builtin.PrincipalActor,
		Holders:      dir,
		Authorize:    builtin.Decide(handleChart{}),
	}
}

// whose is the record one call reached: the writer's handle for a write, and
// the handle the read answered under for a read.
func whose(t *testing.T, verb personVerb, person *personSpy, got tools.Result) string {
	t.Helper()
	if got.Failed {
		return ""
	}
	if verb.write {
		return person.handle
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("%s answered %q: %v", verb.name, got.Output, err)
	}
	handle, _ := answer["handle"].(string)
	return handle
}

// SOMEBODY ELSE'S LOGIN NAMES THEIR HOLDER'S RECORD, and a login is never a
// seat — on every verb that names whose record it acts on.
//
// Two defects, one cause: the name was never resolved to a record. An
// administrator naming the unbound `jane.doe` through `set_priorities`, or
// `PUT /work/people/jane.doe/priorities`, had the login sent through the fuzzy
// colleague resolver, which set the priorities of the seat `jane` ("Jane
// Doe"); and every other verb read or wrote a login LITERALLY, so a person the
// directory binds to a seat — `j.bound`, who holds `jane` — was read an empty
// inbox and written marks under the login, where nothing of hers reads them.
// And a login that is somebody's resolves before the table is asked, so a lead
// naming their report by login is admitted as the lead they are.
func TestSomebodyElsesLoginNamesTheirHoldersRecord(t *testing.T) {
	t.Parallel()
	admin := iam.Principal{Kind: iam.KindMachine, Login: "token:admin"}
	lead := iam.Principal{Kind: iam.KindPerson, Login: "lead.person", Seat: "lead"}
	for _, verb := range personVerbs() {
		for _, c := range []struct {
			name   string
			caller context.Context
			typed  string
			want   string // "" — refused
			// asLead marks a case admitted as the LEAD, which the
			// owner-only verbs refuse whatever the name resolves to.
			asLead bool
		}{
			{"an admin naming an unbound login", as(admin, iam.GrantFleetOperate,
				iam.GrantWorkWrite, iam.GrantStateRead), "jane.doe", "jane.doe", false},
			{"an admin naming a bound person's login", as(admin, iam.GrantFleetOperate,
				iam.GrantWorkWrite, iam.GrantStateRead), "j.bound", "jane", false},
			{"a lead naming their report's login", as(lead, iam.GrantWorkWrite,
				iam.GrantStateRead), "dev.person", "dev", true},
			{"a lead naming somebody they do not lead", as(lead, iam.GrantWorkWrite,
				iam.GrantStateRead), "j.bound", "", false},
		} {
			t.Run(verb.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				person := &personSpy{}
				got := verb.call(c.caller, personDeps(person, &directory{}), c.typed)
				want := c.want
				if verb.ownerOnly && c.asLead {
					want = ""
				}
				if reached := whose(t, verb, person, got); reached != want {
					t.Errorf("%s naming %q reached %q's record, want %q: %s",
						verb.name, c.typed, reached, want, got.Output)
				}
				if want == "" && !errors.Is(got.Cause, builtin.ErrRefused) {
					t.Errorf("the refusal carries %v, not the authority's own "+
						"answer: %s", got.Cause, got.Output)
				}
			})
		}
	}
}

// A BOUND PERSON NAMING THEMSELVES BY LOGIN REACHES THEIR OWN RECORD, and the
// directory is never asked about the caller's own names.
func TestYourOwnLoginIsYourOwnRecordOnEveryVerb(t *testing.T) {
	t.Parallel()
	me := iam.Principal{Kind: iam.KindPerson, Login: "j.bound", Seat: "jane"}
	for _, verb := range personVerbs() {
		for _, typed := range []string{"j.bound", "jane"} {
			t.Run(verb.name+"/"+typed, func(t *testing.T) {
				t.Parallel()
				person, dir := &personSpy{}, &directory{}
				got := verb.call(as(me, iam.GrantWorkWrite, iam.GrantStateRead),
					personDeps(person, dir), typed)
				if reached := whose(t, verb, person, got); reached != "jane" {
					t.Errorf("%s naming %q reached %q, want the caller's own "+
						"record `jane`: %s", verb.name, typed, reached, got.Output)
				}
				if len(dir.asked) != 0 {
					t.Errorf("%s asked the directory about the caller's own "+
						"name: %v", verb.name, dir.asked)
				}
			})
		}
	}
}

// A LOGIN THAT NAMES NOBODY IS NOT A ROSTER, and a directory that cannot say
// is not "nobody".
//
// A login nobody holds is decided on the name as typed FIRST — which nobody
// leads, so only the admin grant passes — and only then refused as naming no
// record: "nobody holds that" before "you may not" would tell a caller with no
// authority over anybody which logins exist. A directory this node cannot read
// is the undecidable arm, never a guess in either direction.
func TestALoginThatNamesNobodyIsNotARoster(t *testing.T) {
	t.Parallel()
	admin := as(iam.Principal{Kind: iam.KindMachine, Login: "token:admin"},
		iam.GrantFleetOperate, iam.GrantWorkWrite, iam.GrantStateRead)
	colleague := as(iam.Principal{Kind: iam.KindPerson, Login: "lead.person",
		Seat: "lead"}, iam.GrantWorkWrite, iam.GrantStateRead)
	for _, verb := range personVerbs() {
		for _, c := range []struct {
			name   string
			caller context.Context
			typed  string
			dir    *directory
			cause  error
		}{
			{"an admin naming a login nobody holds", admin, "ghost.person",
				&directory{}, iam.ErrNoHolder},
			{"an admin naming a holder whose seat is gone", admin, "leaver.person",
				&directory{}, iam.ErrHolderUnseated},
			{"a colleague naming a login nobody holds", colleague, "ghost.person",
				&directory{}, builtin.ErrRefused},
			{"a colleague naming a holder whose seat is gone", colleague,
				"leaver.person", &directory{}, builtin.ErrRefused},
			{"a directory this node cannot read", admin, "j.bound",
				&directory{err: errors.New("the identity applier is behind")},
				builtin.ErrUndecidable},
		} {
			t.Run(verb.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				person := &personSpy{}
				got := verb.call(c.caller, personDeps(person, c.dir), c.typed)
				if !got.Failed || person.handle != "" {
					t.Fatalf("%s naming %q reached a record: %s", verb.name,
						c.typed, got.Output)
				}
				if !errors.Is(got.Cause, c.cause) {
					t.Errorf("%s naming %q failed with %v, want %v: %s",
						verb.name, c.typed, got.Cause, c.cause, got.Output)
				}
				if errors.Is(got.Cause, builtin.ErrRefused) &&
					(errors.Is(got.Cause, iam.ErrNoHolder) ||
						errors.Is(got.Cause, iam.ErrHolderUnseated)) {

					t.Errorf("a refused caller was told whether %q is held", c.typed)
				}
			})
		}
	}
}

// SOMEBODY ELSE'S RECORD NAMED BY A SEAT IS WRITTEN EXACTLY, never looked up.
//
// [builtin.MarkInboxFor] and [builtin.SetPinsFor] are an HTTP route's writes,
// and the route's path is a record's name — so a name that is neither the
// caller's nor a login must be a seat the chart has EXACTLY. The fuzzy
// colleague resolver turned it into whichever seat it resembled; what they
// refuse instead is a name no record could be kept under.
func TestSomebodyElsesSeatIsWrittenExactly(t *testing.T) {
	t.Parallel()
	admin := as(iam.Principal{Kind: iam.KindMachine, Login: "token:admin"},
		iam.GrantFleetOperate)
	for _, verb := range personVerbs() {
		if !verb.ownerOnly {
			continue
		}
		for _, c := range []struct {
			handle  string
			written string
		}{
			{"dev", "dev"},
			// A NAME NO RECORD IS KEPT UNDER is refused rather than
			// resolved: prose, and a seat-shaped name the chart lacks.
			{"Jane Doe", ""},
			{"nobody", ""},
		} {
			person := &personSpy{}
			got := verb.call(admin, personDeps(person, &directory{}), c.handle)
			switch {
			case c.written == "" && (!got.Failed || person.handle != ""):
				t.Errorf("%s naming %q wrote %q's record: %s", verb.name,
					c.handle, person.handle, got.Output)
			case c.written != "" && got.Failed:
				t.Errorf("%s naming %q was refused: %s", verb.name, c.handle, got.Output)
			case person.handle != c.written:
				t.Errorf("%s naming %q wrote %q's record, want exactly the one "+
					"named", verb.name, c.handle, person.handle)
			}
		}
	}
}

// A CALLER WITH NO AUTHORITY LEARNS NOTHING FROM THE DIRECTORY — not even when
// this node cannot read it.
//
// The authority used to be decided AFTER the lookup, and the directory's
// answers differ by login: a login nobody holds was refused on the name as
// typed, while a held one this node could not place answered undecidable
// carrying the directory's own words — "j.bound is bound to jane". A caller
// who leads nobody read which logins exist, and whose seat each holds, off the
// difference. Decided first, every one of those is the same refusal, and the
// directory is never asked. A caller who leads somebody may still be admitted
// on the record, so for them the node honestly cannot say — in words that are
// not the directory's.
func TestACallerWithNoAuthorityLearnsNothingFromTheDirectory(t *testing.T) {
	t.Parallel()
	// `dev` leads nobody in [handleChart]; `lead` leads `dev`.
	stranger := as(iam.Principal{Kind: iam.KindPerson, Login: "dev.person",
		Seat: "dev"}, iam.GrantWorkWrite, iam.GrantStateRead)
	lead := as(iam.Principal{Kind: iam.KindPerson, Login: "lead.person",
		Seat: "lead"}, iam.GrantWorkWrite, iam.GrantStateRead)
	admin := as(iam.Principal{Kind: iam.KindMachine, Login: "token:admin"},
		iam.GrantFleetOperate, iam.GrantWorkWrite, iam.GrantStateRead)
	blind := func() *directory {
		return &directory{err: errors.New("engine: j.bound is bound to jane and " +
			"this node cannot say where that seat is now")}
	}
	for _, verb := range personVerbs() {
		t.Run(verb.name, func(t *testing.T) {
			t.Parallel()
			var answers []string
			for _, c := range []struct {
				typed string
				dir   *directory
			}{
				{"j.bound", &directory{}},
				{"ghost.person", &directory{}},
				{"leaver.person", &directory{}},
				{"j.bound", blind()},
				{"ghost.person", blind()},
			} {
				person := &personSpy{}
				got := verb.call(stranger, personDeps(person, c.dir), c.typed)
				if !got.Failed || person.handle != "" {
					t.Fatalf("naming %q reached a record: %s", c.typed, got.Output)
				}
				if !errors.Is(got.Cause, builtin.ErrRefused) {
					t.Errorf("naming %q failed with %v, want the refusal a "+
						"seat they do not lead gets: %s", c.typed, got.Cause, got.Output)
				}
				if len(c.dir.asked) != 0 {
					t.Errorf("naming %q asked the directory %v for a caller "+
						"it could never admit", c.typed, c.dir.asked)
				}
				answers = append(answers, got.Output)
			}
			for _, answer := range answers[1:] {
				if answer != answers[0] {
					t.Errorf("two logins were refused in different words, which "+
						"is the directory speaking:\n%s\n%s", answers[0], answer)
				}
			}

			// A LEAD MAY BE ADMITTED ON THE RECORD, so a node that cannot
			// read the directory cannot say — and the admin grant may be
			// told that too. Neither hears the directory's own words.
			for who, caller := range map[string]context.Context{
				"a lead": lead, "the admin grant": admin,
			} {
				got := verb.call(caller, personDeps(&personSpy{}, blind()), "j.bound")
				if verb.ownerOnly && who == "a lead" {
					// NO LEAD PATH: refused before the lookup, as above.
					if !errors.Is(got.Cause, builtin.ErrRefused) {
						t.Errorf("%s on an owner-only verb failed with %v", who, got.Cause)
					}
					continue
				}
				if !errors.Is(got.Cause, builtin.ErrUndecidable) {
					t.Errorf("%s, on a directory that cannot say, failed with %v: %s",
						who, got.Cause, got.Output)
				}
				if strings.Contains(got.Output, "bound to") ||
					strings.Contains(got.Output, "jane") {
					t.Errorf("%s was told the directory's own words: %s", who, got.Output)
				}
			}
		})
	}
}
