package code_test

import (
	"encoding/json"
	"errors"
	"fmt"
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
	s.Protections = []code.Protection{{Pattern: "refs/heads/main", DirectPush: code.DirectPersons, Gate: &gate}}
	return s
}

// gatesOf is every rule of the repository name with its gate, as a person's
// read of its detail answers them, and as the listing does — failing the case
// where the two answer different rules.
func (n *node) gatesOf(name string) []code.Protection {
	n.t.Helper()
	detail, err := n.reader.Read(n.t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRepo, Repo: name})
	if err != nil {
		n.t.Fatalf("read %s's detail: %v", name, err)
	}
	listing, err := n.reader.Read(n.t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRepos})
	if err != nil {
		n.t.Fatalf("list the repositories: %v", err)
	}
	for _, r := range listing.Repos {
		if r.Name == name && !slices.EqualFunc(r.Protections, detail.Repo.Protections, code.Protection.Equal) {
			n.t.Fatalf("%s is listed with the rules %+v and its detail answers %+v", name, r.Protections,
				detail.Repo.Protections)
		}
	}
	return detail.Repo.Protections
}

// A GATE RIDES ITS SETTINGS RECORD: a create writes a rule's merge gate, a
// settings change replaces it — a field changed, the gate removed — and a
// restore carries it back as the declaration names it then; every read of the
// repository answers it, the detail and the listing alike, and a node that
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
		rules := a.gatesOf("web")
		switch {
		case len(rules) != 1:
			t.Fatalf("%s: the repository answers %d rules, want main's one", stage, len(rules))
		case gate == nil && rules[0].Gate != nil:
			t.Fatalf("%s: main's rule answers the gate %+v, want none", stage, *rules[0].Gate)
		case gate == nil:
		case rules[0].Gate == nil:
			t.Fatalf("%s: main's rule answers no gate, want %+v", stage, *gate)
		case !rules[0].Gate.Equal(*gate):
			t.Fatalf("%s: main's rule answers the gate %+v, want %+v", stage, *rules[0].Gate, *gate)
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

// A TAG RULE TAKES NO GATE: a tag never moves, so no merge reaches one, and a
// gate on a tag rule is refused at the rule's gate — while the same gate on a
// branch rule beside it is recorded.
func TestATagRuleTakesNoGate(t *testing.T) {
	t.Parallel()
	gate := aGate()
	s := suiteSettings()
	s.Protections = []code.Protection{
		{Pattern: "refs/heads/main", DirectPush: code.DirectPersons, Gate: &gate},
		{Pattern: "refs/tags/v*", DirectPush: code.DirectPersons},
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
