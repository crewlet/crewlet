package code

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// strictGate is a gate holding a merge to something in every field a
// loosening can lower: what each case below opens one field of.
func strictGate() MergeGate {
	return MergeGate{
		MergeBy: MergeByMaintainers, Approvals: ApprovalsOne,
		ApprovalsUnless: []ChangeClass{ChangeClassCommentOnly},
		HumanPaths:      []HumanPath{{Glob: "deploy/**", Unless: []ChangeClass{ChangeClassCommentOnly}}},
		Checks:          []string{"lint", "unit"},
		Landing:         Landing{Depth: 4, Batch: 4},
		OwnersApprove:   true, DeleteSource: DeleteSeats,
	}
}

// gateFieldNames is every field of a gate by its Go name — Landing's own
// fields as Landing.<name> — beside the name a record, a request and a
// refusal spell it by (its JSON key, landing.<key> below Landing), in the
// struct's order.
func gateFieldNames(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(ty reflect.Type, goPrefix, jsonPrefix string)
	walk = func(ty reflect.Type, goPrefix, jsonPrefix string) {
		for i := range ty.NumField() {
			f := ty.Field(i)
			key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, goPrefix+f.Name+".", jsonPrefix+key+".")
				continue
			}
			out[goPrefix+f.Name] = jsonPrefix + key
		}
	}
	walk(reflect.TypeFor[MergeGate](), "", "")
	return out
}

// EVERY FIELD OF A MERGE GATE IS JUDGED FOR LOOSENING, or named neutral. A
// seat may tighten a gate and never loosen one (ADR-0058), so a field ADDED to
// MergeGate that LoosenedBy skips is one every unit's lead may switch off
// unseen — and a value added to a ranked field's closed set, one it may lower
// to. So the cases are keyed by field — Landing's own fields among them — and
// held to the struct's fields by reflection: each opened alone is ONE
// loosening, whose phrase names the field as a request spells it, and closed
// back is none, while a neutral field moved either way is none. Each ranked
// field's every pair of known values is decided here, so a value added to its
// closed set fails until it is; and a value this build does not know fails
// closed on either side. A human path is kept only by its own glob with an
// unless naming nothing more.
//
// Mutations: treat agents_edit_checks turned on as neutral — its case opens
// nothing, red; rank merge_by only among the values this build knows — the
// unknown value it moves to is no lowering, red; keep a human path by its glob
// alone — the widened unless opens nothing, red.
func TestEveryGateFieldIsJudged(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		open    func(*MergeGate)
		neutral bool
	}{
		"MergeBy":   {open: func(g *MergeGate) { g.MergeBy = MergeByWriters }},
		"Approvals": {open: func(g *MergeGate) { g.Approvals = ApprovalsNone }},
		"ApprovalsUnless": {open: func(g *MergeGate) {
			g.ApprovalsUnless = []ChangeClass{ChangeClassCommentOnly, ChangeClassWhitespaceOnly}
		}},
		"HumanPaths":       {open: func(g *MergeGate) { g.HumanPaths = nil }},
		"Checks":           {open: func(g *MergeGate) { g.Checks = []string{"lint"} }},
		"Landing.Depth":    {open: func(g *MergeGate) { g.Landing.Depth = 1 }, neutral: true},
		"Landing.Batch":    {open: func(g *MergeGate) { g.Landing.Batch = MaxQueueBatch }, neutral: true},
		"OwnersApprove":    {open: func(g *MergeGate) { g.OwnersApprove = false }},
		"DeleteSource":     {open: func(g *MergeGate) { g.DeleteSource = DeleteNone }, neutral: true},
		"AgentsEditChecks": {open: func(g *MergeGate) { g.AgentsEditChecks = true }},
	}
	fields := gateFieldNames(t)
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if _, ok := cases[name]; !ok {
			t.Errorf("MergeGate.%s has no case here — give it one, and LoosenedBy a clause judging it or a "+
				"reason it is neutral, or every unit's lead may loosen it", name)
		}
	}
	for name, c := range cases {
		spelled, ok := fields[name]
		if !ok {
			t.Errorf("the case %s names no field of MergeGate", name)
			continue
		}
		strict, open := strictGate(), strictGate()
		c.open(&open)
		if open.Equal(strict) {
			t.Errorf("the case %s changes nothing of the gate", name)
		}
		opened, closed := open.LoosenedBy(strict), strict.LoosenedBy(open)
		switch {
		case c.neutral && (len(opened) != 0 || len(closed) != 0):
			t.Errorf("%s is neutral, and moved it is judged %q one way and %q the other", spelled, opened, closed)
		case c.neutral:
		case len(opened) != 1 || !strings.Contains(opened[0], spelled):
			t.Errorf("%s opened alone is judged %q, want one loosening naming it", spelled, opened)
		case len(closed) != 0:
			t.Errorf("%s closed back alone is judged %q, want none", spelled, closed)
		}
	}

	// A HUMAN PATH IS KEPT ONLY BY ITS OWN GLOB, its unless naming nothing
	// the old one did not: widened it opens, narrowed it does not, and a
	// broader glob in its place is it dropped.
	strict := strictGate()
	for name, tc := range map[string]struct {
		paths []HumanPath
		opens int
	}{
		"its unless widened": {[]HumanPath{{Glob: "deploy/**", Unless: []ChangeClass{ChangeClassCommentOnly,
			ChangeClassWhitespaceOnly}}}, 1},
		"its unless narrowed": {[]HumanPath{{Glob: "deploy/**"}}, 0},
		"a broader glob in its place": {[]HumanPath{{Glob: "**",
			Unless: []ChangeClass{ChangeClassCommentOnly}}}, 1},
		"another added beside it": {[]HumanPath{{Glob: ".crewlet/**"}, strict.HumanPaths[0]}, 0},
	} {
		g := strictGate()
		g.HumanPaths = tc.paths
		if got := g.LoosenedBy(strict); len(got) != tc.opens ||
			(tc.opens > 0 && !strings.Contains(got[0], "human_paths")) {
			t.Errorf("a human path with %s is judged %q, want %d loosening(s) naming human_paths", name, got,
				tc.opens)
		}
	}

	// EVERY PAIR OF A RANKED FIELD'S VALUES, decided here — each the loosest
	// first: writers < maintainers < persons, none < one.
	type ranked struct {
		field  string
		values []string
		set    func(*MergeGate, string)
		looser map[[2]string]bool
	}
	for _, r := range []ranked{
		{"merge_by", spelledAll(MergeBys()), func(g *MergeGate, v string) { g.MergeBy = MergeBy(v) },
			map[[2]string]bool{
				{"writers", "maintainers"}: false, {"writers", "persons"}: false,
				{"maintainers", "writers"}: true, {"maintainers", "persons"}: false,
				{"persons", "writers"}: true, {"persons", "maintainers"}: true,
			}},
		{"approvals", spelledAll(ApprovalCounts()), func(g *MergeGate, v string) { g.Approvals = Approvals(v) },
			map[[2]string]bool{{"none", "one"}: false, {"one", "none"}: true}},
	} {
		for _, from := range r.values {
			for _, to := range r.values {
				if from == to {
					continue
				}
				want, ok := r.looser[[2]string{from, to}]
				if !ok {
					t.Errorf("%s %s → %s is decided nowhere here — say whether it loosens, and LoosenedBy "+
						"with it", r.field, from, to)
					continue
				}
				old, next := strictGate(), strictGate()
				r.set(&old, from)
				r.set(&next, to)
				if got := next.LoosenedBy(old); (len(got) == 1) != want || len(got) > 1 {
					t.Errorf("%s %s → %s is judged %q, want a loosening: %v", r.field, from, to, got, want)
				}
			}
		}
		// AND A VALUE THIS BUILD DOES NOT KNOW, on either side, fails closed:
		// a newer build's value may hold a merge to more than any this one
		// knows, so a move away from one is a lowering, and a move to one is
		// judged a loosening.
		const unknown = "unheard_of"
		for _, known := range r.values {
			old, next := strictGate(), strictGate()
			r.set(&old, unknown)
			r.set(&next, known)
			if got := next.LoosenedBy(old); len(got) != 1 {
				t.Errorf("%s %s → %s is judged %q, want a loosening", r.field, unknown, known, got)
			}
			if got := old.LoosenedBy(next); len(got) != 1 {
				t.Errorf("%s %s → %s is judged %q, want a loosening", r.field, known, unknown, got)
			}
		}
	}
}

// spelledAll is a closed set's values as strings.
func spelledAll[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

// A GATE CHANGE NAMES EVERY FIELD OF A GATE, by the gate's own spelling, and
// sets exactly the one it names. A field added to MergeGate that GateChange
// cannot name is one no tool can set — a gate a declaration can state, a
// person's assistant could never make — and GateChangeFields is what a tool
// takes, so the three are held to one another: every MergeGate field has a
// GateChange field of its Go name and JSON key (Landing's through
// LandingChange), GateChange has those and its removal alone, and
// GateChangeFields lists exactly their keys in MergeGate's order. Then a
// change of each field, decoded from its JSON as a tool's call is, changes
// that field of a gate and leaves every other as it was.
//
// Mutation: leave agents_edit_checks out of applyTo — its change leaves the
// gate as it was, red.
func TestAGateChangeNamesEveryGateField(t *testing.T) {
	t.Parallel()
	for _, pair := range []struct{ whole, change reflect.Type }{
		{reflect.TypeFor[MergeGate](), reflect.TypeFor[GateChange]()},
		{reflect.TypeFor[Landing](), reflect.TypeFor[LandingChange]()},
	} {
		var keys []string
		for i := range pair.whole.NumField() {
			f := pair.whole.Field(i)
			c, ok := pair.change.FieldByName(f.Name)
			switch {
			case !ok:
				t.Errorf("%s.%s has no field in %s: no request could set it", pair.whole.Name(), f.Name,
					pair.change.Name())
				continue
			case jsonKey(c) != jsonKey(f):
				t.Errorf("%s.%s is spelled %q, and %s.%s %q", pair.whole.Name(), f.Name, jsonKey(f),
					pair.change.Name(), c.Name, jsonKey(c))
			}
			keys = append(keys, jsonKey(f))
		}
		for i := range pair.change.NumField() {
			if f := pair.change.Field(i); f.Name != "Remove" {
				if _, ok := pair.whole.FieldByName(f.Name); !ok {
					t.Errorf("%s.%s is no field of %s", pair.change.Name(), f.Name, pair.whole.Name())
				}
			}
		}
		if pair.whole == reflect.TypeFor[MergeGate]() && !slices.Equal(GateChangeFields(), keys) {
			t.Errorf("GateChangeFields is %v, want MergeGate's keys in its order, %v", GateChangeFields(), keys)
		}
	}

	// EACH FIELD CHANGED ALONE, decoded as a tool's argument is.
	values := map[string]string{
		"merge_by": `"persons"`, "approvals": `"none"`, "approvals_unless": `["whitespace_only"]`,
		"human_paths": `[{"glob":"web/**"}]`, "checks": `["e2e","unit"]`, "landing": `{"depth":2}`,
		"owners_approve": `false`, "delete_source": `"none"`, "agents_edit_checks": `true`,
	}
	if got := slices.Sorted(maps.Keys(values)); !slices.Equal(got, slices.Sorted(slices.Values(GateChangeFields()))) {
		t.Fatalf("the cases name %v, want every field a change names, %v", got, GateChangeFields())
	}
	base := strictGate()
	before := asObject(t, base)
	for field, value := range values {
		dec := json.NewDecoder(bytes.NewReader([]byte(`{"` + field + `":` + value + `}`)))
		dec.DisallowUnknownFields()
		var change GateChange
		if err := dec.Decode(&change); err != nil {
			t.Errorf("a change of %s does not decode: %v", field, err)
			continue
		}
		if named := change.named(); !slices.Equal(named, []string{field}) {
			t.Errorf("a change of %s names %v", field, named)
		}
		after := asObject(t, change.applyTo(base))
		for key := range maps.Keys(before) {
			if changed := !reflect.DeepEqual(before[key], after[key]); changed != (key == field) {
				t.Errorf("a change of %s leaves %s %v, from %v", field, key, after[key], before[key])
			}
		}
	}
}

// jsonKey is a field's JSON key.
func jsonKey(f reflect.StructField) string {
	key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return key
}

// asObject is g as its JSON object, every key present.
func asObject(t *testing.T, g MergeGate) map[string]any {
	t.Helper()
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	// THE OMITTED KEYS TOO, so a field a change empties is a key that moved.
	for _, key := range GateChangeFields() {
		if _, ok := out[key]; !ok {
			out[key] = nil
		}
	}
	return out
}
