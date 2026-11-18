package gitlet_test

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

	"github.com/crewlet/crewlet/internal/gitlet"
)

// aGate is a merge gate in the one form a record carries it: every field set,
// every list sorted.
func aGate() gitlet.MergeGate {
	return gitlet.MergeGate{
		MergeBy: gitlet.MergeByMaintainers, Approvals: gitlet.ApprovalsOne,
		ApprovalsUnless: []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly, gitlet.ChangeClassWhitespaceOnly},
		HumanPaths: []gitlet.HumanPath{
			{Glob: ".crewlet/**"},
			{Glob: "deploy/**", Unless: []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly}},
		},
		Checks:        []string{"lint", "unit"},
		Landing:       gitlet.Landing{Depth: 4, Batch: 4},
		OwnersApprove: true, DeleteSource: gitlet.DeleteSeats,
	}
}

// gatedSettings are the suite's settings with main's rule carrying gate.
func gatedSettings(gate gitlet.MergeGate) gitlet.RepoSettings {
	s := suiteSettings()
	s.Protections = []gitlet.Protection{{PushRule: gitlet.PushRule{Pattern: "refs/heads/main",
		DirectPush: gitlet.DirectPersons}, Gate: &gate}}
	return s
}

// gatesOf is the merge gates of the repository name's rules, as a person's
// read of its detail that asks for them answers them — failing the case where
// a gate names no rule the detail's row answers, or the listing answers other
// rules than the detail.
func (n *node) gatesOf(name string) []gitlet.RuleGate {
	n.t.Helper()
	detail, err := n.reader.Read(n.t.Context(), gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRepo, Repo: name,
		Gates: true})
	if err != nil {
		n.t.Fatalf("read %s's detail: %v", name, err)
	}
	listing, err := n.reader.Read(n.t.Context(), gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRepos})
	if err != nil {
		n.t.Fatalf("list the repositories: %v", err)
	}
	for _, r := range listing.Repos {
		if r.Name == name && !slices.Equal(r.Protections, detail.Repo.Protections) {
			n.t.Fatalf("%s is listed with the rules %+v and its detail answers %+v", name, r.Protections,
				detail.Repo.Protections)
		}
	}
	for _, g := range detail.Repo.Gates {
		if !slices.ContainsFunc(detail.Repo.Protections, func(p gitlet.PushRule) bool { return p.Pattern == g.Pattern }) {
			n.t.Fatalf("%s's detail answers a gate for %s, which none of its rules %+v is", name, g.Pattern,
				detail.Repo.Protections)
		}
	}
	return detail.Repo.Gates
}

// A GATE RIDES ITS SETTINGS RECORD: a create writes it, a settings change
// replaces or removes it, a restore carries it back as declared then; a
// detail read asking for gates answers it, and a node replaying the records
// from zero holds byte-identical rows.
//
// Mutation: drop the gate column from insertProtections' write — the created
// repository's rule answers no gate, red.
func TestAGateRidesItsSettingsRecord(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	gate := aGate()
	id := a.ensure("web", gatedSettings(gate), t1).RepoID
	want := func(stage string, gate *gitlet.MergeGate) {
		t.Helper()
		gates := a.gatesOf("web")
		switch {
		case gate == nil && len(gates) != 0:
			t.Fatalf("%s: the repository answers the gates %+v, want none", stage, gates)
		case gate == nil:
		case len(gates) != 1 || gates[0].Pattern != "refs/heads/main":
			t.Fatalf("%s: the repository answers the gates %+v, want main's one", stage, gates)
		case !gates[0].Gate.Equal(*gate):
			t.Fatalf("%s: main's rule answers the gate %+v, want %+v", stage, gates[0].Gate, *gate)
		}
	}
	want("created", &gate)

	changed := aGate()
	changed.MergeBy, changed.Checks = gitlet.MergeByPersons, []string{"e2e", "lint", "unit"}
	a.ensure("web", gatedSettings(changed), t1)
	want("after a settings change", &changed)

	a.ensure("web", suiteSettings(), t1)
	want("after the gate's removal", nil)

	a.deleteAt(id, harnessAt.Add(-3*time.Hour))
	a.ensure("web", gatedSettings(gate), t3)
	want("restored", &gate)

	b.drain()
	if got, want := tableRows(t, b.db, "code_protections"), tableRows(t, a.db, "code_protections"); !slices.Equal(got,
		want) {
		t.Errorf("code_protections replayed on another node holds\n\t%v\nwant\n\t%v", got, want)
	}
}

// THE GATES ARE ANSWERED ONLY TO A READ THAT ASKS FOR THEM, never on a
// repository's row: every read answering the row (a name's resolution, a
// snapshot page, a refs resolution, a listing, a detail) answers each rule as
// gitlet.PushRule, sparing them up to half a mebibyte of JSON. A detail read
// that asks (one repository's, or a refs listing's first page) answers every
// gate beside its rule's pattern, one that does not answers none, and every
// other kind refuses the ask, naming it, a refs listing's later pages among
// them.
//
// Mutations: answer the gates on every detail (Reader.repo's ask dropped) — a
// resolution answers them, red; drop gates from the checks_due listing's
// refused fields — its read takes the ask, red.
func TestOnlyAReadThatAsksIsAnsweredTheGates(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	gate := aGate()
	s := gatedSettings(gate)
	s.Protections = append(s.Protections, gitlet.Protection{PushRule: gitlet.PushRule{Pattern: "refs/heads/release/*",
		DirectPush: gitlet.DirectNone, AllowDelete: true}})
	id := n.ensure("web", s, t1).RepoID
	push := []gitlet.PushRule{s.Protections[0].PushRule, s.Protections[1].PushRule}

	resolved, err := n.reader.Resolve(t.Context(), gitlet.ResolveQuery{Name: "web", Fresh: stale})
	if err != nil || !slices.Equal(resolved.Protections, push) {
		t.Errorf("the resolution of web = (%+v, %v), want its rules %+v", resolved.Protections, err, push)
	}
	page, err := n.reader.Snapshot(t.Context(), gitlet.StateQuery{RepoID: id, Fresh: stale})
	if err != nil || !slices.Equal(page.Repo.Protections, push) {
		t.Errorf("a snapshot of web = (%+v, %v), want its rules %+v", page.Repo.Protections, err, push)
	}

	read := func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		q.Asker = asker
		return n.reader.Read(t.Context(), q)
	}
	for _, q := range []gitlet.ReadQuery{{Kind: gitlet.ReadRepo, Repo: "web"}, {Kind: gitlet.ReadRefs, Repo: "web"}} {
		plain, err := read(q)
		switch {
		case err != nil || plain.Repo == nil:
			t.Fatalf("a %s read of web = (%+v, %v), want its detail", q.Kind, plain.Repo, err)
		case !slices.Equal(plain.Repo.Protections, push):
			t.Errorf("a %s read of web answers the rules %+v, want %+v", q.Kind, plain.Repo.Protections, push)
		case plain.Repo.Gates != nil:
			t.Errorf("a %s read of web that asks for no gates answers %+v", q.Kind, plain.Repo.Gates)
		}
		q.Gates = true
		asked, err := read(q)
		switch {
		case err != nil || asked.Repo == nil:
			t.Fatalf("a %s read of web asking for its gates = (%+v, %v), want its detail", q.Kind, asked.Repo, err)
		case len(asked.Repo.Gates) != 1 || asked.Repo.Gates[0].Pattern != "refs/heads/main" ||
			!asked.Repo.Gates[0].Gate.Equal(gate):
			t.Errorf("a %s read of web asking for its gates answers %+v, want main's %+v", q.Kind,
				asked.Repo.Gates, gate)
		}
	}

	asks := everyReadAsked(id)
	for _, kind := range gitlet.ReadKinds() {
		q, named := asks[kind]
		switch {
		case kind == gitlet.ReadRepo:
			continue
		case !named:
			t.Errorf("a %s read is never asked for gates here: name it, so its refusal is held", kind)
			continue
		case kind == gitlet.ReadRefs:
			// A LATER PAGE, which carries no detail.
			q.Cursor = "refs/heads/main"
		}
		q.Kind = kind
		if _, err := n.reader.Read(t.Context(), q); errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("a %s read that asks for no gates = %v, want no field refused", kind, err)
		}
		q.Gates = true
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, gitlet.ErrInvalid) ||
			!strings.Contains(err.Error(), "gates") {
			t.Errorf("a %s read asking for gates = %v, want ErrInvalid naming gates", kind, err)
		}
	}
}

// A LIFECYCLE CHANGE KEEPS EVERY GATE IT DOES NOT TOUCH. A settings change,
// archive, unarchive and protect request each write the settings whole,
// formed in the decide from the rules the snapshot reads whole beside the row
// (which carries no gate), so each leaves main's gate as it was.
//
// Mutation: form the decide's rules from the row's push rules, each with no
// gate — the first change drops main's gate, red.
func TestALifecycleChangeKeepsEveryGate(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	id := n.made(lcPerson, "gated", unitOwned("eng"))
	gate := aGate()
	n.lands(lcGate(lcPerson, id, "refs/heads/main", persons, gateTo(gate)))
	described := "the gated one"
	for _, req := range []gitlet.LifecycleRequest{
		lcSettings(lcPerson, id, gitlet.RepoChange{Description: &described}),
		lcOn(gitlet.LifecycleArchive, lcPerson, id),
		lcOn(gitlet.LifecycleUnarchive, lcPerson, id),
		lcProtect(lcPerson, id, "refs/heads/release/*", persons),
	} {
		n.lands(req)
		if gates := n.gatesOf("gated"); len(gates) != 1 || gates[0].Pattern != "refs/heads/main" ||
			!gates[0].Gate.Equal(gate) {
			t.Fatalf("after a %s, the gates read %+v, want main's %+v", req.Kind, gates, gate)
		}
	}
}

// sourcesOf is who set each of the repository name's rules, by pattern, as a
// read of its detail asking for its gates answers them — failing the case
// where a source names no rule of the detail's row, or a rule has none.
func (n *node) sourcesOf(name string) map[string]gitlet.RuleSource {
	n.t.Helper()
	detail, err := n.reader.Read(n.t.Context(), gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRepo, Repo: name,
		Gates: true})
	if err != nil {
		n.t.Fatalf("read %s's detail: %v", name, err)
	}
	out := map[string]gitlet.RuleSource{}
	for _, s := range detail.Repo.RuleSources {
		out[s.Pattern] = s
	}
	if len(out) != len(detail.Repo.Protections) {
		n.t.Fatalf("%s answers the sources %+v for the rules %+v, want one a rule", name,
			detail.Repo.RuleSources, detail.Repo.Protections)
	}
	for _, p := range detail.Repo.Protections {
		if _, ok := out[p.Pattern]; !ok {
			n.t.Fatalf("%s's rule %s answers no source among %+v", name, p.Pattern, detail.Repo.RuleSources)
		}
	}
	return out
}

// A RULE NAMES WHO LAST CHANGED IT, AND WHEN: a protect request sources its
// rule to the asker (a person's seat, an agent's handle) at its record's
// instant; a record carries every rule it does not change with its source,
// and gating main moves only main's; a declared rule names nobody, through a
// re-activation too. A node replaying the records holds the same sources.
// Both nodes stamp each record a minute past harnessAt per log place, so two
// rules' order is their records' log order, with no wall clock between.
//
// Mutation: stamp every rule a record writes with the record's own source
// (rulesSourced's comparison dropped) — the release protection moves main's
// source to the agent, red; stamp a rule with the applier's own now rather
// than its record's instant — every rule set at one instant, red.
func TestARuleNamesWhoLastChangedItAndWhen(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	stamped := func(seq uint64, _ time.Time) time.Time { return harnessAt.Add(time.Duration(seq) * time.Minute) }
	a.storedAt, b.storedAt = stamped, stamped
	a.chart = testChart()
	set := func(stage, pattern string, got map[string]gitlet.RuleSource, by string, kind gitlet.ActorKind) {
		t.Helper()
		switch s := got[pattern]; {
		case s.SetBy != by || s.SetByKind != kind:
			t.Fatalf("%s: %s reads set by %q (%s), want %q (%s)", stage, pattern, s.SetBy, s.SetByKind, by, kind)
		case s.SetAt.IsZero():
			t.Fatalf("%s: %s reads set at no instant", stage, pattern)
		}
	}

	id := a.made(lcPerson, "gated", unitOwned("eng"))
	a.lands(lcProtect(lcPerson, id, "refs/heads/main", persons))
	first := a.sourcesOf("gated")
	set("a person protected main", "refs/heads/main", first, "founder", gitlet.ActorOperator)

	a.lands(lcProtect(agent("cto"), id, "refs/heads/release/*", persons))
	second := a.sourcesOf("gated")
	set("an agent protected release/*", "refs/heads/release/*", second, "cto", gitlet.ActorAgent)
	if second["refs/heads/main"] != first["refs/heads/main"] {
		t.Fatalf("protecting release/* moved main's source from %+v to %+v", first["refs/heads/main"],
			second["refs/heads/main"])
	}
	//fixedwait:stated both are their records' instants, which the case's clock stamped by their places in the log
	if !second["refs/heads/release/*"].SetAt.After(first["refs/heads/main"].SetAt) {
		t.Errorf("release/* reads set at %s, not after main's %s", second["refs/heads/release/*"].SetAt,
			first["refs/heads/main"].SetAt)
	}

	described := "the gated one"
	a.lands(lcSettings(lcPerson, id, gitlet.RepoChange{Description: &described}))
	if third := a.sourcesOf("gated"); !maps.Equal(third, second) {
		t.Fatalf("a description change moved the sources %+v to %+v", second, third)
	}

	a.lands(lcGate(lcPerson, id, "refs/heads/main", nil, gateTo(aGate())))
	gated := a.sourcesOf("gated")
	set("a person gated main", "refs/heads/main", gated, "founder", gitlet.ActorOperator)
	switch {
	//fixedwait:stated both are their records' instants, which the case's clock stamped by their places in the log
	case !gated["refs/heads/main"].SetAt.After(second["refs/heads/release/*"].SetAt):
		t.Errorf("main's gate reads set at %s, not after the release protection's %s",
			gated["refs/heads/main"].SetAt, second["refs/heads/release/*"].SetAt)
	case gated["refs/heads/release/*"] != second["refs/heads/release/*"]:
		t.Errorf("gating main moved release/*'s source from %+v to %+v", second["refs/heads/release/*"],
			gated["refs/heads/release/*"])
	}

	a.ensure("web", gatedSettings(aGate()), t1)
	declared := a.sourcesOf("web")
	set("declared", "refs/heads/main", declared, "", gitlet.ActorConfig)
	redescribed := gatedSettings(aGate())
	redescribed.Description = "the web app"
	a.ensure("web", redescribed, t1)
	if again := a.sourcesOf("web"); !maps.Equal(again, declared) {
		t.Fatalf("a re-activation that changed the description moved the sources %+v to %+v", declared, again)
	}

	b.drain()
	if got, want := tableRows(t, b.db, "code_protections"), tableRows(t, a.db, "code_protections"); !slices.Equal(got,
		want) {
		t.Errorf("code_protections replayed on another node holds\n\t%v\nwant\n\t%v", got, want)
	}
}

// NO ROW OF A REPOSITORY CARRIES A MERGE GATE, by its type: gitlet.RepoRef,
// gitlet.RepoSnapshot, gitlet.RefAnswer and gitlet.PushRule reach no
// gitlet.MergeGate, so no read of them decodes, ships or keeps one, and no
// consumer can take a gate nobody read for a rule with none.
//
// Mutation: type RepoRef.Protections as []Protection — the row reaches a
// merge gate, red.
func TestNoRowOfARepositoryCarriesAMergeGate(t *testing.T) {
	t.Parallel()
	gate := reflect.TypeFor[gitlet.MergeGate]()
	for _, row := range []reflect.Type{reflect.TypeFor[gitlet.RepoRef](), reflect.TypeFor[gitlet.RepoSnapshot](),
		reflect.TypeFor[gitlet.RefAnswer](), reflect.TypeFor[gitlet.PushRule]()} {
		if at, found := reachesType(row, gate, map[reflect.Type]bool{}); found {
			t.Errorf("%s reaches a merge gate at %s%s", row, row.Name(), at)
		}
	}
}

// reachesType is where a value of type from holds a value of type target —
// the path of fields to it — and whether it does at all, through pointers,
// slices, arrays, maps and struct fields, each type walked once.
func reachesType(from, target reflect.Type, seen map[reflect.Type]bool) (string, bool) {
	if from == target {
		return "", true
	}
	if seen[from] {
		return "", false
	}
	seen[from] = true
	switch from.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return reachesType(from.Elem(), target, seen)
	case reflect.Map:
		if at, found := reachesType(from.Key(), target, seen); found {
			return at, true
		}
		return reachesType(from.Elem(), target, seen)
	case reflect.Struct:
		for i := range from.NumField() {
			f := from.Field(i)
			if at, found := reachesType(f.Type, target, seen); found {
				return "." + f.Name + at, true
			}
		}
	}
	return "", false
}

// A TAG RULE TAKES NO GATE: a tag never moves, so no merge reaches one, and a
// gate on a tag rule is refused at the rule's gate — while the same gate on a
// branch rule beside it is recorded.
func TestATagRuleTakesNoGate(t *testing.T) {
	t.Parallel()
	gate := aGate()
	s := suiteSettings()
	s.Protections = []gitlet.Protection{
		{PushRule: gitlet.PushRule{Pattern: "refs/heads/main", DirectPush: gitlet.DirectPersons}, Gate: &gate},
		{PushRule: gitlet.PushRule{Pattern: "refs/tags/v*", DirectPush: gitlet.DirectPersons}},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("a gate on a branch rule beside a tag rule is refused: %v", err)
	}
	s.Protections[1].Gate = &gate
	err := s.Validate()
	if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "protections[1].gate:") ||
		!strings.Contains(err.Error(), "tag never moves") {
		t.Fatalf("a gate on a tag rule: Validate = %v, want ErrInvalid at protections[1].gate naming why", err)
	}
}

// NO_CODE_CHANGE IS NO CLASS A GATE EXEMPTS BY: it is a revision's class, not
// a path's, so it is refused in approvals_unless and a human path's unless,
// each where it is named, while both path classes are taken in both.
func TestNoCodeChangeIsNoUnlessClass(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		set   func(*gitlet.MergeGate, []gitlet.ChangeClass)
		field string
	}{
		"approvals_unless": {func(g *gitlet.MergeGate, c []gitlet.ChangeClass) { g.ApprovalsUnless = c },
			"protections[0].gate.approvals_unless[0]"},
		"a human path's unless": {func(g *gitlet.MergeGate, c []gitlet.ChangeClass) { g.HumanPaths[0].Unless = c },
			"protections[0].gate.human_paths[0].unless[0]"},
	} {
		gate := aGate()
		tc.set(&gate, slices.Sorted(slices.Values(gitlet.PathClasses())))
		if err := gatedSettings(gate).Validate(); err != nil {
			t.Errorf("%s: every path class is refused: %v", name, err)
		}
		tc.set(&gate, []gitlet.ChangeClass{gitlet.ChangeClassNoCodeChange})
		err := gatedSettings(gate).Validate()
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), tc.field+":") ||
			!strings.Contains(err.Error(), "already carries") {
			t.Errorf("%s: no_code_change is answered %v, want ErrInvalid at %s naming why it exempts nothing",
				name, err, tc.field)
		}
	}
}

// A GATE IS CANONICAL: CanonicalGate forms its one order from any order,
// leaving its input as it was, so one gate declared in two orders encodes to
// the same bytes; a gate out of order, or naming a value twice, is refused
// where it first breaks the order, since one declaration must form one record
// on every node.
func TestAGateIsCanonical(t *testing.T) {
	t.Parallel()
	want := aGate()
	shuffled := aGate()
	slices.Reverse(shuffled.ApprovalsUnless)
	slices.Reverse(shuffled.HumanPaths)
	shuffled.HumanPaths[0].Unless = []gitlet.ChangeClass{gitlet.ChangeClassWhitespaceOnly, gitlet.ChangeClassCommentOnly}
	slices.Reverse(shuffled.Checks)
	given, err := json.Marshal(shuffled)
	if err != nil {
		t.Fatal(err)
	}

	got := gitlet.CanonicalGate(shuffled)
	want.HumanPaths[1].Unless = gitlet.PathClasses() // both, sorted
	slices.Sort(want.HumanPaths[1].Unless)
	if !got.Equal(want) {
		t.Fatalf("CanonicalGate formed %+v, want %+v", got, want)
	}
	if after, _ := json.Marshal(shuffled); string(after) != string(given) {
		t.Errorf("CanonicalGate changed the gate it was given: %s became %s", given, after)
	}
	if err := gatedSettings(got).Validate(); err != nil {
		t.Errorf("the canonical gate is refused: %v", err)
	}
	again := gitlet.CanonicalGate(gitlet.CanonicalGate(shuffled))
	one, _ := json.Marshal(got)
	two, _ := json.Marshal(again)
	if string(one) != string(two) {
		t.Errorf("one gate in two orders encodes as %s and %s", one, two)
	}

	for name, tc := range map[string]struct {
		mutate func(*gitlet.MergeGate)
		field  string
	}{
		"its exempting classes out of order": {func(g *gitlet.MergeGate) { slices.Reverse(g.ApprovalsUnless) },
			"approvals_unless[1]"},
		"its human paths out of order": {func(g *gitlet.MergeGate) { slices.Reverse(g.HumanPaths) },
			"human_paths[1].glob"},
		"a human path's classes out of order": {func(g *gitlet.MergeGate) {
			g.HumanPaths[0].Unless = []gitlet.ChangeClass{gitlet.ChangeClassWhitespaceOnly, gitlet.ChangeClassCommentOnly}
		}, "human_paths[0].unless[1]"},
		"its checks out of order": {func(g *gitlet.MergeGate) { slices.Reverse(g.Checks) }, "checks[1]"},
		"a check twice":           {func(g *gitlet.MergeGate) { g.Checks = []string{"lint", "lint"} }, "checks[1]"},
		"a human path twice": {func(g *gitlet.MergeGate) {
			g.HumanPaths = []gitlet.HumanPath{{Glob: "deploy/**"}, {Glob: "deploy/**"}}
		}, "human_paths[1].glob"},
		"a class twice": {func(g *gitlet.MergeGate) {
			g.ApprovalsUnless = []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly, gitlet.ChangeClassCommentOnly}
		}, "approvals_unless[1]"},
	} {
		gate := aGate()
		tc.mutate(&gate)
		err := gatedSettings(gate).Validate()
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "protections[0].gate."+tc.field+":") {
			t.Errorf("%s: Validate = %v, want ErrInvalid at protections[0].gate.%s", name, err, tc.field)
		}
	}
}

// A GATE IS HELD TO ITS FORM, each breach refused at its field: every enum
// known and none unset (a record carries every field resolved), a landing in
// bounds, every glob one gitref reads, every check a name, each list in its
// bound, and the whole within MaxGateBytes of JSON.
func TestAGateIsHeldToItsForm(t *testing.T) {
	t.Parallel()
	many := func(n int, item func(int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = item(i)
		}
		return out
	}
	for name, tc := range map[string]struct {
		mutate func(*gitlet.MergeGate)
		field  string
	}{
		"no merge_by":            {func(g *gitlet.MergeGate) { g.MergeBy = "" }, "merge_by"},
		"an unknown merge_by":    {func(g *gitlet.MergeGate) { g.MergeBy = "admins" }, "merge_by"},
		"no approvals":           {func(g *gitlet.MergeGate) { g.Approvals = "" }, "approvals"},
		"two approvals":          {func(g *gitlet.MergeGate) { g.Approvals = "two" }, "approvals"},
		"a depth of none":        {func(g *gitlet.MergeGate) { g.Landing.Depth = 0 }, "landing.depth"},
		"a depth past its bound": {func(g *gitlet.MergeGate) { g.Landing.Depth = gitlet.MaxQueueDepth + 1 }, "landing.depth"},
		"a batch of none":        {func(g *gitlet.MergeGate) { g.Landing.Batch = 0 }, "landing.batch"},
		"a batch past its bound": {func(g *gitlet.MergeGate) { g.Landing.Batch = gitlet.MaxQueueBatch + 1 }, "landing.batch"},
		"no delete_source":       {func(g *gitlet.MergeGate) { g.DeleteSource = "" }, "delete_source"},
		"an unknown class": {func(g *gitlet.MergeGate) {
			g.ApprovalsUnless = []gitlet.ChangeClass{"docs_only"}
		}, "approvals_unless[0]"},
		"a human path that is no glob": {func(g *gitlet.MergeGate) {
			g.HumanPaths = []gitlet.HumanPath{{Glob: "/deploy/**"}}
		}, "human_paths[0].glob"},
		"too many human paths": {func(g *gitlet.MergeGate) {
			g.HumanPaths = nil
			for _, glob := range many(gitlet.MaxHumanPaths+1, func(i int) string { return fmt.Sprintf("d%02d/**", i) }) {
				g.HumanPaths = append(g.HumanPaths, gitlet.HumanPath{Glob: glob})
			}
		}, "human_paths"},
		"a check that is no name": {func(g *gitlet.MergeGate) { g.Checks = []string{"Unit Tests"} }, "checks[0]"},
		"a check named past its bound": {func(g *gitlet.MergeGate) {
			g.Checks = []string{strings.Repeat("a", 65)}
		}, "checks[0]"},
		"too many checks": {func(g *gitlet.MergeGate) {
			g.Checks = many(gitlet.MaxRequiredChecks+1, func(i int) string { return fmt.Sprintf("c%02d", i) })
		}, "checks"},
	} {
		gate := aGate()
		tc.mutate(&gate)
		err := gatedSettings(gate).Validate()
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "protections[0].gate."+tc.field+":") {
			t.Errorf("%s: Validate = %v, want ErrInvalid at protections[0].gate.%s", name, err, tc.field)
		}
	}

	// THE WHOLE, past MaxGateBytes, is refused at the gate: every check at
	// its longest name and human paths of the globs JSON spells longest,
	// which no single field breaks.
	wide := widestGate(t)
	if err := gatedSettings(wide).Validate(); err != nil {
		t.Fatalf("the widest gate within its bytes is refused: %v", err)
	}
	wide.HumanPaths = append(wide.HumanPaths, gitlet.HumanPath{Glob: "zz/**"})
	if b, _ := json.Marshal(wide); len(b) <= gitlet.MaxGateBytes {
		t.Fatalf("the premise: a gate of %d bytes is not past the bound", len(b))
	}
	err := gatedSettings(wide).Validate()
	if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "protections[0].gate: ") ||
		!strings.Contains(err.Error(), fmt.Sprint(gitlet.MaxGateBytes)) {
		t.Errorf("a gate past MaxGateBytes: Validate = %v, want ErrInvalid at protections[0].gate naming %d", err,
			gitlet.MaxGateBytes)
	}
}

// A GATE REQUIRES EXACTLY THE NAMES A CHECKS FILE GIVES: both are held to
// config.ValidCodeCheckName, so no rule requires a check no repository can
// define, which would shut its gate for good, and every defined check can be
// required.
//
// Mutation: hold the checks file to a rule of its own that takes an upper-case
// letter, or the gate to one that takes a `~`, and the names they part on go
// red.
func TestAGateRequiresExactlyTheNamesAChecksFileGives(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"unit", "a", "0", "go1.27-race_x", "unit.", strings.Repeat("a", gitlet.MaxCheckNameBytes),
		"", "Unit", ".unit", "-unit", "_unit", "unit tests", "unit/fast", "unit~1", "unit:fast", "ünit",
		strings.Repeat("a", gitlet.MaxCheckNameBytes+1),
	} {
		file := fmt.Sprintf("checks:\n  - name: %q\n    run: make\n    timeout_seconds: 60\n", name)
		_, fileErr := gitlet.ParseChecks([]byte(file))
		gate := aGate()
		gate.Checks = []string{name}
		gateErr := gatedSettings(gate).Validate()
		if (fileErr == nil) != (gateErr == nil) {
			t.Errorf("%q: a checks file defining it is refused with %v, and a gate requiring it with %v", name,
				fileErr, gateErr)
		}
	}
}

// widestGate is a gate as wide as a record may carry one, a few bytes short of
// MaxGateBytes at most: every check at its longest name, then human paths of
// the globs JSON spells longest (each & six bytes) with both classes, as many
// as fit, and a last one of plain bytes that brings it to the bound.
func widestGate(t testing.TB) gitlet.MergeGate {
	t.Helper()
	g := aGate()
	g.Checks = nil
	for i := range gitlet.MaxRequiredChecks {
		g.Checks = append(g.Checks, fmt.Sprintf("%02d", i)+strings.Repeat("x", 62))
	}
	weigh := func() int {
		b, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	both := slices.Sorted(slices.Values(gitlet.PathClasses()))
	g.HumanPaths = nil
	for i := 0; i < gitlet.MaxHumanPaths-1; i++ {
		g.HumanPaths = append(g.HumanPaths, gitlet.HumanPath{Glob: fmt.Sprintf("%02d", i) + strings.Repeat("&", 126),
			Unless: both})
		if weigh() > gitlet.MaxGateBytes {
			g.HumanPaths = g.HumanPaths[:i]
			break
		}
	}
	// THE LAST, `,{"glob":"NN…"}`: fourteen bytes and its padding.
	if pad := gitlet.MaxGateBytes - weigh() - 14; pad >= 0 {
		g.HumanPaths = append(g.HumanPaths, gitlet.HumanPath{
			Glob: fmt.Sprintf("%02d", len(g.HumanPaths)) + strings.Repeat("x", min(pad, 126))})
	}
	if n := weigh(); n > gitlet.MaxGateBytes || n < gitlet.MaxGateBytes-16 {
		t.Fatalf("the widest gate weighs %d bytes, want just short of %d", n, gitlet.MaxGateBytes)
	}
	return g
}

// MERGE AUTHORITY RANKS writers BELOW maintainers BELOW persons, and the
// stricter of two is what a merge is held to where both hold — a value this
// build does not know ranking past every one it does, so an authority it
// cannot read never reads as looser than one it can.
func TestMergeByRanksItsAuthorities(t *testing.T) {
	t.Parallel()
	all := gitlet.MergeBys()
	if !slices.Equal(all, []gitlet.MergeBy{gitlet.MergeByWriters, gitlet.MergeByMaintainers, gitlet.MergeByPersons}) {
		t.Fatalf("MergeBys is %v, want writers, maintainers, persons — loosest first", all)
	}
	for i, m := range all {
		if m.Rank() != i {
			t.Errorf("%s ranks %d, want %d", m, m.Rank(), i)
		}
		for _, o := range all {
			want := m
			if o.Rank() > m.Rank() {
				want = o
			}
			if got := m.Stricter(o); got != want {
				t.Errorf("%s.Stricter(%s) = %s, want %s", m, o, got, want)
			}
		}
	}
	unknown := gitlet.MergeBy("nobody")
	if unknown.Valid() || unknown.Rank() <= gitlet.MergeByPersons.Rank() {
		t.Errorf("an unknown authority ranks %d, want past persons' %d", unknown.Rank(), gitlet.MergeByPersons.Rank())
	}
	if got := gitlet.MergeByPersons.Stricter(unknown); got != unknown {
		t.Errorf("persons.Stricter(an unknown authority) = %s, want the unknown one, never the looser", got)
	}
}
