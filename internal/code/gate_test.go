package code_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// aGate is a merge gate in the one form a record carries it: every field set,
// every list sorted.
func aGate() code.MergeGate {
	return code.MergeGate{
		MergeBy: code.MergeByMaintainers, Approvals: code.ApprovalsOne,
		ApprovalsUnless: []code.ChangeClass{code.ChangeClassCommentOnly, code.ChangeClassWhitespaceOnly},
		HumanPaths: []code.HumanPath{
			{Glob: ".crewlet/**"},
			{Glob: "deploy/**", Unless: []code.ChangeClass{code.ChangeClassCommentOnly}},
		},
		Checks:        []string{"lint", "unit"},
		Landing:       code.Landing{Policy: code.LandingPreLand, Depth: 4, Batch: 4},
		OwnersApprove: true, DeleteSource: code.DeleteSeats,
	}
}

// gatedSettings are the suite's settings with main's rule carrying gate.
func gatedSettings(gate code.MergeGate) code.RepoSettings {
	s := suiteSettings()
	s.Protections = []code.Protection{{PushRule: code.PushRule{Pattern: "refs/heads/main",
		DirectPush: code.DirectPersons}, Gate: &gate}}
	return s
}

// gatesOf is the merge gates of the repository name's rules, as a person's
// read of its detail that asks for them answers them — failing the case where
// a gate names no rule the detail's row answers, or the listing answers other
// rules than the detail.
func (n *node) gatesOf(name string) []code.RuleGate {
	n.t.Helper()
	detail, err := n.reader.Read(n.t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRepo, Repo: name,
		Gates: true})
	if err != nil {
		n.t.Fatalf("read %s's detail: %v", name, err)
	}
	listing, err := n.reader.Read(n.t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRepos})
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
		if !slices.ContainsFunc(detail.Repo.Protections, func(p code.PushRule) bool { return p.Pattern == g.Pattern }) {
			n.t.Fatalf("%s's detail answers a gate for %s, which none of its rules %+v is", name, g.Pattern,
				detail.Repo.Protections)
		}
	}
	return detail.Repo.Gates
}

// A GATE RIDES ITS SETTINGS RECORD: a create writes a rule's merge gate, a
// settings change replaces it — a field changed, the gate removed — and a
// restore carries it back as the declaration names it then; a read of the
// repository's detail asking for its gates answers it, and a node that
// applies the same records from zero holds byte-identical rows.
//
// Mutation: drop the gate column from insertProtections' write — the created
// repository's rule answers no gate, red.
func TestAGateRidesItsSettingsRecord(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	gate := aGate()
	id := a.ensure("web", gatedSettings(gate), t1).RepoID
	want := func(stage string, gate *code.MergeGate) {
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
	changed.MergeBy, changed.Checks = code.MergeByPersons, []string{"e2e", "lint", "unit"}
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

// THE GATES ARE ANSWERED ONLY TO A READ THAT ASKS FOR THEM, and never on a
// repository's row. Every read answering the row — the resolution of its name
// every git request is admitted by, a page of the snapshot every git node
// syncs from and keeps in its state file, a resolution of its refs, a listing,
// its detail — answers each rule as a push is judged by it, with no gate
// (code.PushRule, which has none to leave out): a repository's gates are up to
// half a mebibyte of JSON that none of them reads. A read of its detail that
// asks for them — one repository's, or a refs listing's first page, which
// carries the detail — answers every gate beside its rule's pattern; one that
// does not answers none. Every other kind refuses the ask, naming it: a refs
// listing's later pages, which carry no detail, among them.
//
// Mutations: answer the gates on every detail (Reader.repo's ask dropped) — a
// resolution answers them, red; drop gates from the checks_due listing's
// refused fields — its read takes the ask, red.
func TestOnlyAReadThatAsksIsAnsweredTheGates(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	gate := aGate()
	s := gatedSettings(gate)
	s.Protections = append(s.Protections, code.Protection{PushRule: code.PushRule{Pattern: "refs/heads/release/*",
		DirectPush: code.DirectNone, AllowDelete: true}})
	id := n.ensure("web", s, t1).RepoID
	push := []code.PushRule{s.Protections[0].PushRule, s.Protections[1].PushRule}

	resolved, err := n.reader.Resolve(t.Context(), code.ResolveQuery{Name: "web", Fresh: stale})
	if err != nil || !slices.Equal(resolved.Protections, push) {
		t.Errorf("the resolution of web = (%+v, %v), want its rules %+v", resolved.Protections, err, push)
	}
	page, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Fresh: stale})
	if err != nil || !slices.Equal(page.Repo.Protections, push) {
		t.Errorf("a snapshot of web = (%+v, %v), want its rules %+v", page.Repo.Protections, err, push)
	}

	read := func(q code.ReadQuery) (code.ReadAnswer, error) {
		q.Asker = asker
		return n.reader.Read(t.Context(), q)
	}
	for _, q := range []code.ReadQuery{{Kind: code.ReadRepo, Repo: "web"}, {Kind: code.ReadRefs, Repo: "web"}} {
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
	for _, kind := range code.ReadKinds() {
		q, named := asks[kind]
		switch {
		case kind == code.ReadRepo:
			continue
		case !named:
			t.Errorf("a %s read is never asked for gates here: name it, so its refusal is held", kind)
			continue
		case kind == code.ReadRefs:
			// A LATER PAGE, which carries no detail.
			q.Cursor = "refs/heads/main"
		}
		q.Kind = kind
		if _, err := n.reader.Read(t.Context(), q); errors.Is(err, code.ErrInvalid) {
			t.Errorf("a %s read that asks for no gates = %v, want no field refused", kind, err)
		}
		q.Gates = true
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, code.ErrInvalid) ||
			!strings.Contains(err.Error(), "gates") {
			t.Errorf("a %s read asking for gates = %v, want ErrInvalid naming gates", kind, err)
		}
	}
}

// A LIFECYCLE CHANGE KEEPS EVERY GATE IT DOES NOT TOUCH. A tool-made
// repository's settings change, archive, unarchive and protect request each
// write its settings WHOLE, formed in the decide from its rules as the
// snapshot holds them — read whole beside the repository's row, which carries
// each rule as a push is judged by it and no gate — so a description changed,
// the repository archived and brought back, or another pattern protected
// leaves main's gate as it was.
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
	for _, req := range []code.LifecycleRequest{
		lcSettings(lcPerson, id, code.RepoChange{Description: &described}),
		lcOn(code.LifecycleArchive, lcPerson, id),
		lcOn(code.LifecycleUnarchive, lcPerson, id),
		lcProtect(lcPerson, id, "refs/heads/release/*", persons),
	} {
		n.lands(req)
		if gates := n.gatesOf("gated"); len(gates) != 1 || gates[0].Pattern != "refs/heads/main" ||
			!gates[0].Gate.Equal(gate) {
			t.Fatalf("after a %s, the gates read %+v, want main's %+v", req.Kind, gates, gate)
		}
	}
}

// NO ROW OF A REPOSITORY CARRIES A MERGE GATE, by its type: the row every git
// request's admission and every resolution of a ref reads (code.RepoRef), each
// page of the snapshot a git node syncs by and its cache's state file keeps
// (code.RepoSnapshot), and each rule a push is judged by (code.PushRule) reach
// no code.MergeGate anywhere — so no read of them decodes, ships or keeps one,
// and no consumer of them can take a gate nobody read for a rule with none.
//
// Mutation: type RepoRef.Protections as []Protection — the row reaches a
// merge gate, red.
func TestNoRowOfARepositoryCarriesAMergeGate(t *testing.T) {
	t.Parallel()
	gate := reflect.TypeFor[code.MergeGate]()
	for _, row := range []reflect.Type{reflect.TypeFor[code.RepoRef](), reflect.TypeFor[code.RepoSnapshot](),
		reflect.TypeFor[code.RefAnswer](), reflect.TypeFor[code.PushRule]()} {
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
	s.Protections = []code.Protection{
		{PushRule: code.PushRule{Pattern: "refs/heads/main", DirectPush: code.DirectPersons}, Gate: &gate},
		{PushRule: code.PushRule{Pattern: "refs/tags/v*", DirectPush: code.DirectPersons}},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("a gate on a branch rule beside a tag rule is refused: %v", err)
	}
	s.Protections[1].Gate = &gate
	err := s.Validate()
	if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "protections[1].gate:") ||
		!strings.Contains(err.Error(), "tag never moves") {
		t.Fatalf("a gate on a tag rule: Validate = %v, want ErrInvalid at protections[1].gate naming why", err)
	}
}

// NO_CODE_CHANGE IS NO CLASS A GATE EXEMPTS BY: it is a revision's class,
// never a path's, and an approval already carries across a revision that
// changed nothing — so it is refused in approvals_unless and in a human
// path's unless, each at the place it is named, and the two path classes are
// taken in both.
func TestNoCodeChangeIsNoUnlessClass(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		set   func(*code.MergeGate, []code.ChangeClass)
		field string
	}{
		"approvals_unless": {func(g *code.MergeGate, c []code.ChangeClass) { g.ApprovalsUnless = c },
			"protections[0].gate.approvals_unless[0]"},
		"a human path's unless": {func(g *code.MergeGate, c []code.ChangeClass) { g.HumanPaths[0].Unless = c },
			"protections[0].gate.human_paths[0].unless[0]"},
	} {
		gate := aGate()
		tc.set(&gate, slices.Sorted(slices.Values(code.PathClasses())))
		if err := gatedSettings(gate).Validate(); err != nil {
			t.Errorf("%s: every path class is refused: %v", name, err)
		}
		tc.set(&gate, []code.ChangeClass{code.ChangeClassNoCodeChange})
		err := gatedSettings(gate).Validate()
		if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), tc.field+":") ||
			!strings.Contains(err.Error(), "already carries") {
			t.Errorf("%s: no_code_change is answered %v, want ErrInvalid at %s naming why it exempts nothing",
				name, err, tc.field)
		}
	}
}

// A GATE IS CANONICAL: CanonicalGate forms its one order — every list sorted,
// each human path's classes too — from any order, leaving what it was given as
// it was, so two declarations of one gate in two orders are one gate and
// encode to the same bytes; and a gate in any other order, or naming a value
// twice, is refused at the first place it breaks the order, since one
// declaration must form one record on every node.
func TestAGateIsCanonical(t *testing.T) {
	t.Parallel()
	want := aGate()
	shuffled := aGate()
	slices.Reverse(shuffled.ApprovalsUnless)
	slices.Reverse(shuffled.HumanPaths)
	shuffled.HumanPaths[0].Unless = []code.ChangeClass{code.ChangeClassWhitespaceOnly, code.ChangeClassCommentOnly}
	slices.Reverse(shuffled.Checks)
	given, err := json.Marshal(shuffled)
	if err != nil {
		t.Fatal(err)
	}

	got := code.CanonicalGate(shuffled)
	want.HumanPaths[1].Unless = code.PathClasses() // both, sorted
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
	again := code.CanonicalGate(code.CanonicalGate(shuffled))
	one, _ := json.Marshal(got)
	two, _ := json.Marshal(again)
	if string(one) != string(two) {
		t.Errorf("one gate in two orders encodes as %s and %s", one, two)
	}

	for name, tc := range map[string]struct {
		mutate func(*code.MergeGate)
		field  string
	}{
		"its exempting classes out of order": {func(g *code.MergeGate) { slices.Reverse(g.ApprovalsUnless) },
			"approvals_unless[1]"},
		"its human paths out of order": {func(g *code.MergeGate) { slices.Reverse(g.HumanPaths) },
			"human_paths[1].glob"},
		"a human path's classes out of order": {func(g *code.MergeGate) {
			g.HumanPaths[0].Unless = []code.ChangeClass{code.ChangeClassWhitespaceOnly, code.ChangeClassCommentOnly}
		}, "human_paths[0].unless[1]"},
		"its checks out of order": {func(g *code.MergeGate) { slices.Reverse(g.Checks) }, "checks[1]"},
		"a check twice":           {func(g *code.MergeGate) { g.Checks = []string{"lint", "lint"} }, "checks[1]"},
		"a human path twice": {func(g *code.MergeGate) {
			g.HumanPaths = []code.HumanPath{{Glob: "deploy/**"}, {Glob: "deploy/**"}}
		}, "human_paths[1].glob"},
		"a class twice": {func(g *code.MergeGate) {
			g.ApprovalsUnless = []code.ChangeClass{code.ChangeClassCommentOnly, code.ChangeClassCommentOnly}
		}, "approvals_unless[1]"},
	} {
		gate := aGate()
		tc.mutate(&gate)
		err := gatedSettings(gate).Validate()
		if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "protections[0].gate."+tc.field+":") {
			t.Errorf("%s: Validate = %v, want ErrInvalid at protections[0].gate.%s", name, err, tc.field)
		}
	}
}

// A GATE IS HELD TO ITS FORM: every enum one this build knows — none left
// unset, since a record carries every field its writer resolved — a landing
// inside its bounds, every glob gitref reads, every check a name, each list
// inside its bound, and the whole within MaxGateBytes of JSON — each refused
// at the field it is about.
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
		mutate func(*code.MergeGate)
		field  string
	}{
		"no merge_by":            {func(g *code.MergeGate) { g.MergeBy = "" }, "merge_by"},
		"an unknown merge_by":    {func(g *code.MergeGate) { g.MergeBy = "admins" }, "merge_by"},
		"no approvals":           {func(g *code.MergeGate) { g.Approvals = "" }, "approvals"},
		"two approvals":          {func(g *code.MergeGate) { g.Approvals = "two" }, "approvals"},
		"an unknown landing":     {func(g *code.MergeGate) { g.Landing.Policy = "on_green" }, "landing.policy"},
		"a depth of none":        {func(g *code.MergeGate) { g.Landing.Depth = 0 }, "landing.depth"},
		"a depth past its bound": {func(g *code.MergeGate) { g.Landing.Depth = code.MaxQueueDepth + 1 }, "landing.depth"},
		"a batch of none":        {func(g *code.MergeGate) { g.Landing.Batch = 0 }, "landing.batch"},
		"a batch past its bound": {func(g *code.MergeGate) { g.Landing.Batch = code.MaxQueueBatch + 1 }, "landing.batch"},
		"no delete_source":       {func(g *code.MergeGate) { g.DeleteSource = "" }, "delete_source"},
		"an unknown class": {func(g *code.MergeGate) {
			g.ApprovalsUnless = []code.ChangeClass{"docs_only"}
		}, "approvals_unless[0]"},
		"a human path that is no glob": {func(g *code.MergeGate) {
			g.HumanPaths = []code.HumanPath{{Glob: "/deploy/**"}}
		}, "human_paths[0].glob"},
		"too many human paths": {func(g *code.MergeGate) {
			g.HumanPaths = nil
			for _, glob := range many(code.MaxHumanPaths+1, func(i int) string { return fmt.Sprintf("d%02d/**", i) }) {
				g.HumanPaths = append(g.HumanPaths, code.HumanPath{Glob: glob})
			}
		}, "human_paths"},
		"a check that is no name": {func(g *code.MergeGate) { g.Checks = []string{"Unit Tests"} }, "checks[0]"},
		"a check named past its bound": {func(g *code.MergeGate) {
			g.Checks = []string{strings.Repeat("a", 65)}
		}, "checks[0]"},
		"too many checks": {func(g *code.MergeGate) {
			g.Checks = many(code.MaxRequiredChecks+1, func(i int) string { return fmt.Sprintf("c%02d", i) })
		}, "checks"},
	} {
		gate := aGate()
		tc.mutate(&gate)
		err := gatedSettings(gate).Validate()
		if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "protections[0].gate."+tc.field+":") {
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
	wide.HumanPaths = append(wide.HumanPaths, code.HumanPath{Glob: "zz/**"})
	if b, _ := json.Marshal(wide); len(b) <= code.MaxGateBytes {
		t.Fatalf("the premise: a gate of %d bytes is not past the bound", len(b))
	}
	err := gatedSettings(wide).Validate()
	if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "protections[0].gate: ") ||
		!strings.Contains(err.Error(), fmt.Sprint(code.MaxGateBytes)) {
		t.Errorf("a gate past MaxGateBytes: Validate = %v, want ErrInvalid at protections[0].gate naming %d", err,
			code.MaxGateBytes)
	}
}

// A GATE REQUIRES EXACTLY THE NAMES A CHECKS FILE GIVES: one rule names a
// check wherever a name is written (config.ValidCodeCheckName) — a check's own
// in a repository's .crewlet/checks.yaml and the checks a protection rule
// requires — so a rule can never require a check no repository can define,
// which would keep its gate shut for good, and every check a file defines a
// rule can require.
//
// Mutation: hold the checks file to a rule of its own that takes an upper-case
// letter, or the gate to one that takes a `~`, and the names they part on go
// red.
func TestAGateRequiresExactlyTheNamesAChecksFileGives(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"unit", "a", "0", "go1.27-race_x", "unit.", strings.Repeat("a", code.MaxCheckNameBytes),
		"", "Unit", ".unit", "-unit", "_unit", "unit tests", "unit/fast", "unit~1", "unit:fast", "ünit",
		strings.Repeat("a", code.MaxCheckNameBytes+1),
	} {
		file := fmt.Sprintf("checks:\n  - name: %q\n    run: make\n    timeout_seconds: 60\n", name)
		_, fileErr := code.ParseChecks([]byte(file))
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
func widestGate(t testing.TB) code.MergeGate {
	t.Helper()
	g := aGate()
	g.Checks = nil
	for i := range code.MaxRequiredChecks {
		g.Checks = append(g.Checks, fmt.Sprintf("%02d", i)+strings.Repeat("x", 62))
	}
	weigh := func() int {
		b, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	both := slices.Sorted(slices.Values(code.PathClasses()))
	g.HumanPaths = nil
	for i := 0; i < code.MaxHumanPaths-1; i++ {
		g.HumanPaths = append(g.HumanPaths, code.HumanPath{Glob: fmt.Sprintf("%02d", i) + strings.Repeat("&", 126),
			Unless: both})
		if weigh() > code.MaxGateBytes {
			g.HumanPaths = g.HumanPaths[:i]
			break
		}
	}
	// THE LAST, `,{"glob":"NN…"}`: fourteen bytes and its padding.
	if pad := code.MaxGateBytes - weigh() - 14; pad >= 0 {
		g.HumanPaths = append(g.HumanPaths, code.HumanPath{
			Glob: fmt.Sprintf("%02d", len(g.HumanPaths)) + strings.Repeat("x", min(pad, 126))})
	}
	if n := weigh(); n > code.MaxGateBytes || n < code.MaxGateBytes-16 {
		t.Fatalf("the widest gate weighs %d bytes, want just short of %d", n, code.MaxGateBytes)
	}
	return g
}

// MERGE AUTHORITY RANKS writers BELOW maintainers BELOW persons, and the
// stricter of two is what a merge is held to where both hold — a value this
// build does not know ranking past every one it does, so an authority it
// cannot read never reads as looser than one it can.
func TestMergeByRanksItsAuthorities(t *testing.T) {
	t.Parallel()
	all := code.MergeBys()
	if !slices.Equal(all, []code.MergeBy{code.MergeByWriters, code.MergeByMaintainers, code.MergeByPersons}) {
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
	unknown := code.MergeBy("nobody")
	if unknown.Valid() || unknown.Rank() <= code.MergeByPersons.Rank() {
		t.Errorf("an unknown authority ranks %d, want past persons' %d", unknown.Rank(), code.MergeByPersons.Rank())
	}
	if got := code.MergeByPersons.Stricter(unknown); got != unknown {
		t.Errorf("persons.Stricter(an unknown authority) = %s, want the unknown one, never the looser", got)
	}
}
