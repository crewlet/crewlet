package code

import (
	"reflect"
	"testing"
)

// EVERY CONTROL OF A PROTECTION IS JUDGED FOR LOOSENING. A seat may add a rule
// or tighten one and never loosen one (ADR-0058), so a field ADDED to
// Protection that loosenings skips is a control every unit's lead may switch
// off unseen — and a direct_push value added to the closed set, one it may
// switch to. So the cases are keyed by field name and held to the struct's own
// fields by reflection, and the direct_push cases to every pair of values the
// closed set holds: a new field, or a new value, fails this test until it is
// given a case, and with it its clause in loosenings.
//
// THE MERGE GATE is one control, judged whole: removed it opens, added where
// the rule had none it does not, and ALTERED in any field it opens too — fail
// closed — while the rule's own gate sent back, another pointer to an equal
// value, alters nothing.
//
// Mutations: drop allow_force's clause from loosenings — its case names no
// opening, red; judge direct_push by a rank (none below persons, a value this
// build does not know above both) and call only a rise a loosening — the
// unknown value made persons is none, red; compare the gates by pointer — the
// gate sent back as it stands is judged altered, red; drop the altered arm —
// a gate whose merge_by is lowered opens nothing, red.
func TestEveryProtectionControlIsJudged(t *testing.T) {
	t.Parallel()
	const pattern = "refs/heads/main"
	judged := func(from Protection, to *Protection) []string {
		return loosenings([]Protection{from}, ProtectChange{Pattern: pattern, Rule: to})
	}

	gate := func() *MergeGate {
		return &MergeGate{
			MergeBy: MergeByMaintainers, Approvals: ApprovalsOne, Checks: []string{"lint", "unit"},
			Landing: Landing{Policy: LandingPreLand, Depth: 4, Batch: 4}, DeleteSource: DeleteSeats,
		}
	}

	// EACH CONTROL, opened alone and closed back: one loosening, then none.
	cases := map[string]struct{ strict, open Protection }{
		"DirectPush": {Protection{Pattern: pattern, DirectPush: DirectNone},
			Protection{Pattern: pattern, DirectPush: DirectPersons}},
		"AllowForce": {Protection{Pattern: pattern, DirectPush: DirectPersons},
			Protection{Pattern: pattern, DirectPush: DirectPersons, AllowForce: true}},
		"AllowDelete": {Protection{Pattern: pattern, DirectPush: DirectPersons},
			Protection{Pattern: pattern, DirectPush: DirectPersons, AllowDelete: true}},
		"Gate": {Protection{Pattern: pattern, DirectPush: DirectPersons, Gate: gate()},
			Protection{Pattern: pattern, DirectPush: DirectPersons}},
	}
	ignored := map[string]bool{"Pattern": true}
	fields := reflect.TypeFor[Protection]()
	for i := range fields.NumField() {
		name := fields.Field(i).Name
		if _, ok := cases[name]; !ok && !ignored[name] {
			t.Errorf("Protection.%s has no case here — give it one, and loosenings a clause judging it, or "+
				"every unit's lead may loosen it", name)
		}
	}
	for name, c := range cases {
		if got := judged(c.strict, &c.open); len(got) != 1 {
			t.Errorf("a change opening %s alone is judged %q, want one loosening", name, got)
		}
		if got := judged(c.open, &c.strict); len(got) != 0 {
			t.Errorf("a change closing %s alone is judged %q, want no loosening", name, got)
		}
	}

	// EVERY PAIR OF direct_push VALUES, decided here: none admits nobody, and
	// persons a person.
	loosens := map[[2]DirectPush]bool{
		{DirectNone, DirectPersons}: true,
		{DirectPersons, DirectNone}: false,
	}
	for _, from := range DirectPushes() {
		for _, to := range DirectPushes() {
			if from == to {
				continue
			}
			want, ok := loosens[[2]DirectPush{from, to}]
			if !ok {
				t.Errorf("direct_push %s → %s is decided nowhere here — say whether it loosens, and "+
					"loosenings with it", from, to)
				continue
			}
			got := judged(Protection{Pattern: pattern, DirectPush: from}, &Protection{DirectPush: to})
			if (len(got) > 0) != want {
				t.Errorf("direct_push %s → %s is judged %q, want a loosening: %v", from, to, got, want)
			}
		}
	}
	// AND ONE THIS BUILD DOES NOT KNOW, either side, fails closed — but for
	// none, which admits nobody whatever it replaces.
	const unknown DirectPush = "maintainers"
	for _, tc := range []struct {
		from, to DirectPush
		loosens  bool
	}{
		{DirectNone, unknown, true},
		{DirectPersons, unknown, true},
		{unknown, DirectPersons, true},
		{unknown, DirectNone, false},
	} {
		got := judged(Protection{Pattern: pattern, DirectPush: tc.from}, &Protection{DirectPush: tc.to})
		if (len(got) > 0) != tc.loosens {
			t.Errorf("direct_push %s → %s is judged %q, want a loosening: %v", tc.from, tc.to, got, tc.loosens)
		}
	}

	// THE GATE, ALTERED IN ANY FIELD, opens — fail closed, whichever way the
	// field moved — and the rule's own gate sent back as it stands, another
	// pointer to an equal value, alters nothing.
	gated := Protection{Pattern: pattern, DirectPush: DirectPersons, Gate: gate()}
	if got := judged(gated, &Protection{DirectPush: DirectPersons, Gate: gate()}); len(got) != 0 {
		t.Errorf("the rule's own gate sent back as it stands is judged %q, want no loosening", got)
	}
	for name, alter := range map[string]func(*MergeGate){
		"lowering merge_by":      func(g *MergeGate) { g.MergeBy = MergeByWriters },
		"raising merge_by":       func(g *MergeGate) { g.MergeBy = MergeByPersons },
		"dropping a check":       func(g *MergeGate) { g.Checks = []string{"lint"} },
		"turning auto_revert on": func(g *MergeGate) { g.AutoRevert = true },
	} {
		altered := gate()
		alter(altered)
		if got := judged(gated, &Protection{DirectPush: DirectPersons, Gate: altered}); len(got) != 1 {
			t.Errorf("a gate altered by %s is judged %q, want one loosening", name, got)
		}
	}

	// A RULE REMOVED loosens; a pattern no rule holds loosens nothing, added
	// to or removed.
	if got := judged(cases["DirectPush"].strict, nil); len(got) != 1 {
		t.Errorf("a removed rule is judged %q, want one loosening", got)
	}
	for _, to := range []*Protection{nil, {DirectPush: DirectPersons, AllowForce: true, AllowDelete: true,
		Gate: gate()}} {
		if got := loosenings(nil, ProtectChange{Pattern: pattern, Rule: to}); got != nil {
			t.Errorf("a change of a pattern no rule holds is judged %q, want none", got)
		}
	}
}
