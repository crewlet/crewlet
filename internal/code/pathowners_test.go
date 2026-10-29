package code_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

// pathOwner is owner of kind and ref owning paths.
func pathOwner(kind code.OwnerKind, ref string, paths ...string) code.PathOwner {
	return code.PathOwner{Owner: code.Owner{Kind: kind, Ref: ref}, Paths: paths}
}

// ownersOf is the path owners of the repository name, as a person's read of
// its detail answers them.
func (n *node) ownersOf(name string) []code.PathOwner {
	n.t.Helper()
	answer, err := n.reader.Read(n.t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRepo, Repo: name})
	if err != nil {
		n.t.Fatalf("read %s's detail: %v", name, err)
	}
	return answer.Repo.PathOwners
}

// lcOwners is a settings change of the repository id's path owners.
func lcOwners(actor code.Principal, id string, add, remove []code.PathOwner) code.LifecycleRequest {
	return lcSettings(actor, id, code.RepoChange{PathOwners: &code.PathOwnersChange{Add: add, Remove: remove}})
}

// PATH OWNERS RIDE EVERY RECORD THAT CARRIES A REPOSITORY'S SETTINGS: a create
// writes them, a settings change replaces them whole, a delete keeps them and a
// restore carries them back as the declaration names them then — and the
// repository's purge removes them. A node that applies the same records from
// zero holds byte-identical rows, and the detail answers them in their one
// form.
//
// Mutation: leave the path owners out of the restore's apply — the restored
// repository answers the owners its delete left, red; drop clearPathOwnersSQL
// from the final purge — the purged repository's row stays, red.
func TestPathOwnersRideEveryRecordThatCarriesTheSettings(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	declared := func(owners ...code.PathOwner) code.RepoSettings {
		s := suiteSettings()
		s.PathOwners = code.CanonicalPathOwners(owners)
		return s
	}
	two := declared(pathOwner(code.OwnerUnit, "ENG", "web/**", "docs/*.md"),
		pathOwner(code.OwnerSeat, "dev", "Makefile"))
	id := a.ensure("web", two, t1).RepoID
	want := []code.PathOwner{pathOwner(code.OwnerSeat, "dev", "Makefile"),
		pathOwner(code.OwnerUnit, "ENG", "docs/*.md", "web/**")}
	if got := a.ownersOf("web"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the created repository's path owners are %+v, want %+v — by kind, then reference, each "+
			"one's globs sorted", got, want)
	}

	one := declared(pathOwner(code.OwnerUnit, "ENG", "web/**"))
	a.ensure("web", one, t1)
	if got := a.ownersOf("web"); !reflect.DeepEqual(got, one.PathOwners) {
		t.Fatalf("after a settings change the path owners are %+v, want %+v", got, one.PathOwners)
	}

	a.deleteAt(id, harnessAt.Add(-3*time.Hour))
	if n := a.count(`SELECT COUNT(*) FROM code_path_owners WHERE repo_id = ?`, id); n != 1 {
		t.Fatalf("the deleted repository holds %d path owners' rows, want its one kept until its purge", n)
	}
	a.ensure("web", two, t3)
	if got := a.ownersOf("web"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the restored repository's path owners are %+v, want the restore's %+v", got, want)
	}

	b.drain()
	if got, want := tableRows(t, b.db, "code_path_owners"), tableRows(t, a.db, "code_path_owners"); !slices.Equal(got, want) {
		t.Errorf("code_path_owners replayed on another node holds\n\t%v\nwant\n\t%v", got, want)
	}

	a.storedAt = func(uint64, time.Time) time.Time { return harnessAt.Add(-2 * time.Hour) }
	if deleted, err := a.store.DeleteRepo(t.Context(), a.activate(harnessAt.Add(-30*time.Minute)), id); err != nil ||
		!deleted {
		t.Fatalf("DeleteRepo = %v, %v", deleted, err)
	}
	a.storedAt = nil
	a.purgeNow(id)
	if n := a.count(`SELECT COUNT(*) FROM code_path_owners WHERE repo_id = ?`, id); n != 0 {
		t.Fatalf("the purged repository leaves %d path owners' rows, want none", n)
	}
}

// A PATH OWNER CHANGE IS RESOLVED INSIDE THE DECIDE, glob by glob: a request
// adds globs to an owner or removes them, and the decide applies it to the
// owners its own snapshot holds — so two requests giving one owner two globs,
// in either order, leave both, where a caller that sent the whole list it had
// read would drop the other's. An owner left with no glob is no path owner;
// a glob it holds already is no change and writes nothing. An owner the chart
// does not hold is refused for every actor, whom a review could not be asked
// of — and taken off by a removal however the chart changed, so a unit the
// chart dropped never sticks. A scratch repository takes none, a seat below
// maintain changes none, and a request both adding and removing one glob of
// one owner is no request.
//
// Mutation: apply the change to an empty list rather than the rows the decide
// read — the second owner's globs drop the first's, red.
func TestAPathOwnerChangeIsResolvedInsideTheDecide(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	cto := agent("cto")
	id := n.made(lcPerson, "owned", unitOwned("eng"))
	ownersOf := func() []code.PathOwner {
		t.Helper()
		return n.ownersOf("owned")
	}

	n.lands(lcOwners(cto, id, []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "infra/**")}, nil))
	n.lands(lcOwners(lcPerson, id, []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "deploy/**"),
		pathOwner(code.OwnerSeat, "dev", "Makefile")}, nil))
	want := []code.PathOwner{pathOwner(code.OwnerSeat, "dev", "Makefile"),
		pathOwner(code.OwnerUnit, "plat", "deploy/**", "infra/**")}
	if got := ownersOf(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after two requests giving plat a glob each, the owners are %+v, want %+v", got, want)
	}

	// A GLOB ALREADY HELD, ADDED AGAIN, AND ONE NOT HELD, REMOVED, WRITE
	// NOTHING.
	end := n.end()
	for _, req := range []code.LifecycleRequest{
		lcOwners(lcPerson, id, []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "infra/**")}, nil),
		lcOwners(lcPerson, id, nil, []code.PathOwner{pathOwner(code.OwnerUnit, "sales", "web/**")}),
	} {
		if got := n.life(req); got.Refusal != "" {
			t.Fatalf("a request that changes no path owner: %+v, want it answered from the row", got)
		}
	}
	if got := n.end(); got != end {
		t.Fatalf("requests that changed no path owner wrote %d records", got-end)
	}

	n.lands(lcOwners(lcPerson, id, nil, []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "infra/**"),
		pathOwner(code.OwnerSeat, "dev", "Makefile")}))
	want = []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "deploy/**")}
	if got := ownersOf(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after removing infra/** from plat and dev's one glob, the owners are %+v, want %+v", got, want)
	}

	for _, tc := range []struct {
		who  string
		req  code.LifecycleRequest
		says string
	}{
		{"a person naming a unit the chart lacks", lcOwners(lcPerson, id,
			[]code.PathOwner{pathOwner(code.OwnerUnit, "gone", "web/**")}, nil), "unit gone is no unit of the chart"},
		{"the lead naming a seat the chart lacks", lcOwners(cto, id,
			[]code.PathOwner{pathOwner(code.OwnerSeat, "ghost", "web/**")}, nil), "seat ghost is no seat of the chart"},
		{"a member of the unit", lcOwners(agent("dev"), id,
			[]code.PathOwner{pathOwner(code.OwnerSeat, "dev", "web/**")}, nil), "may not settings owned"},
		{"an outsider the change names", lcOwners(agent("vp-sales"), id,
			[]code.PathOwner{pathOwner(code.OwnerSeat, "vp-sales", "web/**")}, nil), "may not settings owned"},
		{"a person on a scratch repository", lcOwners(lcPerson, n.made(agent("dev"), "dev.mine", seatOwned("dev")),
			[]code.PathOwner{pathOwner(code.OwnerSeat, "dev", "web/**")}, nil), "takes no path owners"},
	} {
		end := n.end()
		got := n.life(tc.req)
		if got.Refusal != code.LifecycleNotPermitted || !strings.Contains(got.Detail, tc.says) {
			t.Errorf("%s: %+v, want not_permitted saying %q", tc.who, got, tc.says)
		}
		if n.end() != end {
			t.Errorf("%s: the refusal wrote %d records", tc.who, n.end()-end)
		}
	}

	// A UNIT THE CHART DROPPED IS TAKEN OFF ALL THE SAME.
	n.lands(lcOwners(lcPerson, id, []code.PathOwner{pathOwner(code.OwnerUnit, "sales", "sales/**")}, nil))
	dropped := testChart()
	dropped.units = slices.DeleteFunc(dropped.units, func(u chartUnit) bool { return u.id == "sales" })
	n.chart = dropped
	n.lands(lcOwners(lcPerson, id, nil, []code.PathOwner{pathOwner(code.OwnerUnit, "sales", "sales/**")}))
	if got := ownersOf(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after taking off a unit the chart dropped, the owners are %+v, want %+v", got, want)
	}

	// NO REQUEST AT ALL: nothing to change, a glob both ways, an owner twice,
	// a glob gitref refuses.
	for name, change := range map[string]code.PathOwnersChange{
		"nothing": {},
		"a glob both added and removed": {Add: []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "a/**")},
			Remove: []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "a/**")}},
		"an owner twice": {Add: []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "a/**"),
			pathOwner(code.OwnerUnit, "plat", "b/**")}},
		"a glob gitref refuses": {Add: []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "/a")}},
		"an owner of no kind":   {Add: []code.PathOwner{pathOwner("", "plat", "a/**")}},
		"an entry of no glob":   {Remove: []code.PathOwner{pathOwner(code.OwnerUnit, "plat")}},
	} {
		req := lcSettings(lcPerson, id, code.RepoChange{PathOwners: &change})
		if _, err := n.store.Lifecycle(t.Context(), req); !errors.Is(err, code.ErrInvalid) ||
			!strings.Contains(err.Error(), "path_owners") {
			t.Errorf("%s: %v, want ErrInvalid naming path_owners", name, err)
		}
	}
}

// A PATH OWNER GRANTS NOTHING: naming every unit and every seat of the chart a
// path owner of a repository changes no seat's standing on it — what each
// reads it as, a `visibility: unit` repository's outsider still refused, and
// whether each may change it — because a path owner is whom a review is asked
// of, and a seat's standing is its grant, read off the chart and its tier
// (ADR-0058). The detail that answers the owners answers the same access
// beside them.
//
// Mutation: answer a path owner's detail at maintain (Reader.repo reading the
// owners it serves) — dev, plat-lead and vp-sales answer maintain, red.
func TestAPathOwnerGrantsNothing(t *testing.T) {
	t.Parallel()
	n, _ := unitRepos(t)
	// EVERY AGENT SEAT: a human seat holds its standing as a person does, never
	// as a seat reading for itself.
	var seats []string
	for _, h := range slices.Sorted(maps.Keys(n.chart.(chart).seats)) {
		if !n.chart.(chart).seats[h].Human {
			seats = append(seats, h)
		}
	}
	// standing is what each seat may do with name: the access its detail
	// answers, or the refusal it is answered instead.
	standing := func(name string) map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, h := range seats {
			answer, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: agent(h), Kind: code.ReadRepo, Repo: name})
			switch {
			case errors.Is(err, code.ErrNotPermitted):
				out[h] = "not_permitted"
			case err != nil:
				t.Fatalf("%s's read of %s: %v", h, name, err)
			default:
				out[h] = answer.Repo.Access
			}
		}
		return out
	}
	everyone := code.CanonicalPathOwners(func() []code.PathOwner {
		var out []code.PathOwner
		for _, h := range seats {
			out = append(out, pathOwner(code.OwnerSeat, h, h+"/**"))
		}
		for _, u := range n.chart.(chart).units {
			out = append(out, pathOwner(code.OwnerUnit, cmpOr(u.id, u.name), "units/"+u.name+"/**"))
		}
		return out
	}())
	for _, name := range []string{"plat-private", "platform", "sales-private"} {
		before := standing(name)
		s := suiteSettings()
		s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: map[string]string{"plat-private": "Platform",
			"platform": "Platform", "sales-private": "Sales"}[name]}
		s.Visibility = code.VisibilityCompany
		if strings.HasSuffix(name, "-private") {
			s.Visibility = code.VisibilityUnit
		}
		s.PathOwners = everyone
		n.ensure(name, s, t1)
		if got := n.ownersOf(name); !reflect.DeepEqual(got, everyone) {
			t.Fatalf("the premise: %s's path owners are %+v, want every unit and seat of the chart", name, got)
		}
		if after := standing(name); !reflect.DeepEqual(after, before) {
			t.Errorf("%s with every unit and seat its path owner reads as %v, and as %v without them: a path "+
				"owner grants nothing", name, after, before)
		}
	}
}

// THE LARGEST SETTINGS FIT WHERE THEY TRAVEL. Every path glob of every owner
// at gitref's bound, each byte one JSON spells longest (`&`, six bytes), beside
// every protection rule's pattern and the description spelled the same way,
// and every rule's merge gate at MaxGateBytes, is the widest a repository's
// settings can be — and they must fit the code log's record whole, since a
// create, a settings change and a restore each carry them: a declaration past
// it would be refused at every activation. And the detail that answers them
// must fit the room a full page of refs beside it leaves in the estate's
// reply, which a refs listing's first page carries both in
// ([code.MaxPageBytes]).
func TestTheLargestSettingsFitTheirRecordAndTheDetail(t *testing.T) {
	t.Parallel()
	wide := func(n int) string { return strings.Repeat("&", n) }
	var owners []code.PathOwner
	for i := range code.MaxPathOwners {
		ref := fmt.Sprintf("%064d", i) // a unit's key and a seat's handle are at most 64 bytes
		var paths []string
		for k := range code.MaxPathOwnerPaths {
			p := fmt.Sprintf("%02d", k) + wide(gitref.MaxPathGlobBytes-2)
			if _, err := gitref.ParsePathGlob(p); err != nil {
				t.Fatalf("the widest glob %q is no glob: %v", p, err)
			}
			paths = append(paths, p)
		}
		owners = append(owners, pathOwner(code.OwnerUnit, ref, paths...))
	}
	s := suiteSettings()
	s.Description = wide(code.MaxDescriptionBytes)
	s.Protections = nil
	gate := widestGate(t)
	for i := range code.MaxProtectionsPerRepo {
		head := fmt.Sprintf("refs/heads/%02d", i)
		s.Protections = append(s.Protections, code.Protection{Pattern: head + wide(gitref.MaxRefNameBytes-len(head)),
			AllowForce: true, AllowDelete: true, DirectPush: code.DirectPersons, Gate: &gate})
	}
	s.PathOwners = code.CanonicalPathOwners(owners)
	if err := s.Validate(); err != nil {
		t.Fatalf("the widest settings are refused: %v", err)
	}

	const id = "0190f3c4-0000-7000-8000-000000000001"
	name := strings.Repeat("a", gitref.MaxRepoNameBytes)
	for _, tc := range []struct {
		record  string
		subject code.Subject
		op      code.OpKind
		payload any
	}{
		{"a create", code.NameSubject(name), code.OpCreate, code.CreateRepo{RepoID: id, Name: name, Settings: s}},
		{"a settings change", code.RepoSubject(id), code.OpSettings, code.SettingsRecord{RepoID: id, Settings: s}},
		{"a restore", code.RepoSubject(id), code.OpRestore, code.LifecycleRecord{RepoID: id, ConfigEpoch: 1,
			Settings: &s}},
	} {
		body, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatal(err)
		}
		record, err := code.Encode(code.MutationRecord{
			RecordEnvelope: code.RecordEnvelope{OpID: statelog.NewOpID(time.Now(), "code.widest"),
				Subject: tc.subject, Op: tc.op, CreatedAt: harnessAt,
				Scope: code.Scope{code.NamePath(code.NameToken(name)), code.RepoPath(id)}},
			Mutation: body, Actor: "node-a", ActorKind: code.ActorConfig,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(record) > code.CodeLogMaxRecordBytes {
			t.Errorf("%s of the widest settings is %d bytes, past the %d one record of the code log may hold",
				tc.record, len(record), code.CodeLogMaxRecordBytes)
		}
	}

	detail, err := json.Marshal(code.RepoDetail{RepoSummary: code.RepoSummary{RepoRef: code.RepoRef{
		ID: id, Name: name, Description: s.Description, Protections: s.Protections}}, PathOwners: s.PathOwners})
	if err != nil {
		t.Fatal(err)
	}
	// THE REST OF A REFS ANSWER'S FIRST PAGE beside its page of refs and the
	// detail — its position, its cursor, the estate's envelope — is under a
	// kilobyte; sixty-four leave it room past any field a later answer adds.
	const answerRoom = 64 << 10
	if room := queue.MaxPayloadBytes - code.MaxPageBytes - answerRoom; len(detail) > room {
		t.Errorf("the widest detail is %d bytes, past the %d a full page of refs leaves it in the estate's "+
			"reply", len(detail), room)
	}
}

// A REPOSITORY'S PATH OWNERS ARE HELD TO THEIR ONE FORM, which every record
// carries them in: at most MaxPathOwners owners, each a unit or a seat naming
// itself, once, in kind-then-reference order, with 1 to MaxPathOwnerPaths
// globs gitref reads, sorted and each once — none on a scratch repository.
// CanonicalPathOwners forms it from any order, leaving what it was given as it
// was.
//
// Mutation: compare owners by reference alone — the seat and the unit of one
// token read as one owner named twice, red.
func TestPathOwnersAreHeldToTheirOneForm(t *testing.T) {
	t.Parallel()
	with := func(owners ...code.PathOwner) code.RepoSettings {
		s := suiteSettings()
		s.PathOwners = owners
		return s
	}
	unit := func(ref string, paths ...string) code.PathOwner { return pathOwner(code.OwnerUnit, ref, paths...) }
	seat := func(ref string, paths ...string) code.PathOwner { return pathOwner(code.OwnerSeat, ref, paths...) }
	many := func(n int, each func(i int) code.PathOwner) []code.PathOwner {
		out := make([]code.PathOwner, n)
		for i := range out {
			out[i] = each(i)
		}
		return out
	}
	globs := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("d%02d/**", i)
		}
		return out
	}
	scratch := with(seat("dev", "x/**"))
	scratch.ManagedBy, scratch.Kind = code.ManagedTools, code.RepoScratch
	scratch.Owner, scratch.Protections = code.Owner{Kind: code.OwnerSeat, Ref: "dev"}, nil
	for name, tc := range map[string]struct {
		settings code.RepoSettings
		says     string // "" for settings the domain records
	}{
		"none":                           {with(), ""},
		"a seat and a unit of one token": {with(seat("web", "a/**"), unit("web", "b/**")), ""},
		"every bound at once": {with(many(code.MaxPathOwners, func(i int) code.PathOwner {
			return unit(fmt.Sprintf("u%02d", i), globs(code.MaxPathOwnerPaths)...)
		})...), ""},
		"owners out of order":   {with(unit("b", "x/**"), unit("a", "x/**")), "is not after"},
		"a unit before a seat":  {with(unit("a", "x/**"), seat("a", "x/**")), "is not after"},
		"an owner named twice":  {with(unit("a", "x/**"), unit("a", "y/**")), "is not after"},
		"globs out of order":    {with(unit("a", "y/**", "x/**")), "is not after"},
		"a glob named twice":    {with(unit("a", "x/**", "x/**")), "is not after"},
		"an owner of no glob":   {with(unit("a")), "at least one glob"},
		"an owner of no kind":   {with(pathOwner("", "a", "x/**")), "owner.kind"},
		"an owner of no ref":    {with(unit(" ", "x/**")), "owner.ref"},
		"a glob gitref refuses": {with(unit("a", "x/")), "is not a path glob"},
		"an owner past its globs": {with(unit("a", globs(code.MaxPathOwnerPaths+1)...)),
			fmt.Sprintf("past the cap of %d", code.MaxPathOwnerPaths)},
		"owners past their bound": {with(many(code.MaxPathOwners+1, func(i int) code.PathOwner {
			return unit(fmt.Sprintf("u%02d", i), "x/**")
		})...), fmt.Sprintf("past the cap of %d", code.MaxPathOwners)},
		"a scratch repository's": {scratch, "takes no path owners"},
	} {
		err := tc.settings.Validate()
		switch {
		case tc.says == "" && err != nil:
			t.Errorf("%s: refused: %v", name, err)
		case tc.says != "" && (!errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), tc.says)):
			t.Errorf("%s: %v, want ErrInvalid saying %q", name, err, tc.says)
		}
	}

	given := []code.PathOwner{unit("b", "z/**", "a/**"), seat("b", "m/**"), unit("a", "k/**")}
	kept := []code.PathOwner{unit("b", "z/**", "a/**"), seat("b", "m/**"), unit("a", "k/**")}
	got := code.CanonicalPathOwners(given)
	want := []code.PathOwner{seat("b", "m/**"), unit("a", "k/**"), unit("b", "a/**", "z/**")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CanonicalPathOwners = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(given, kept) {
		t.Errorf("CanonicalPathOwners rewrote what it was given: %+v", given)
	}
	if err := with(got...).Validate(); err != nil {
		t.Errorf("the canonical form is refused: %v", err)
	}
}
