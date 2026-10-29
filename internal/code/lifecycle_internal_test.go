package code

import (
	"reflect"
	"slices"
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
// THE MERGE GATE: removed it opens, added where the rule had none it does
// not, and changed it opens what MergeGate.LoosenedBy names — field by field,
// each held by TestEveryGateFieldIsJudged — so the rule's own gate sent back,
// another pointer to an equal value, opens nothing, a gate tightened opens
// nothing, and one loosened in two fields opens those two alone.
//
// Mutations: drop allow_force's clause from loosenings — its case names no
// opening, red; judge direct_push by a rank (none below persons, a value this
// build does not know above both) and call only a rise a loosening — the
// unknown value made persons is none, red; drop the gate's removal arm — the
// gate removed opens nothing, red; drop the gate's field arm — the gate
// lowered in two fields opens nothing, red.
func TestEveryProtectionControlIsJudged(t *testing.T) {
	t.Parallel()
	const pattern = "refs/heads/main"
	// judged is a change of the one rule from to the rule to — removed where
	// to is nil — as the decide judges it: the rules it read, and the rules
	// the change leaves.
	judged := func(from Protection, to *Protection) []string {
		return loosenings([]Protection{from}, onlyRule(pattern, to), pattern)
	}

	gate := func() *MergeGate {
		return &MergeGate{
			MergeBy: MergeByMaintainers, Approvals: ApprovalsOne, Checks: []string{"lint", "unit"},
			Landing: Landing{Policy: LandingPreLand, Depth: 4, Batch: 4}, DeleteSource: DeleteSeats,
		}
	}

	// EACH CONTROL, opened alone and closed back: one loosening, then none.
	cases := map[string]struct{ strict, open Protection }{
		"DirectPush": {Protection{PushRule: PushRule{Pattern: pattern, DirectPush: DirectNone}},
			Protection{PushRule: PushRule{Pattern: pattern, DirectPush: DirectPersons}}},
		"AllowForce": {Protection{PushRule: PushRule{Pattern: pattern, DirectPush: DirectPersons}},
			Protection{PushRule: PushRule{Pattern: pattern, DirectPush: DirectPersons, AllowForce: true}}},
		"AllowDelete": {Protection{PushRule: PushRule{Pattern: pattern, DirectPush: DirectPersons}},
			Protection{PushRule: PushRule{Pattern: pattern, DirectPush: DirectPersons, AllowDelete: true}}},
		"Gate": {Protection{PushRule: PushRule{Pattern: pattern, DirectPush: DirectPersons}, Gate: gate()},
			Protection{PushRule: PushRule{Pattern: pattern, DirectPush: DirectPersons}}},
	}
	// EVERY FIELD A RULE HOLDS, its push rule's included: the embedded
	// PushRule's own fields are the rule's, and a control added to either is
	// one more field here.
	ignored := map[string]bool{"Pattern": true}
	for _, field := range reflect.VisibleFields(reflect.TypeFor[Protection]()) {
		if field.Anonymous {
			continue
		}
		if _, ok := cases[field.Name]; !ok && !ignored[field.Name] {
			t.Errorf("Protection.%s has no case here — give it one, and loosenings a clause judging it, or "+
				"every unit's lead may loosen it", field.Name)
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
			got := judged(Protection{PushRule: PushRule{Pattern: pattern, DirectPush: from}},
				&Protection{PushRule: PushRule{DirectPush: to}})
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
		got := judged(Protection{PushRule: PushRule{Pattern: pattern, DirectPush: tc.from}},
			&Protection{PushRule: PushRule{DirectPush: tc.to}})
		if (len(got) > 0) != tc.loosens {
			t.Errorf("direct_push %s → %s is judged %q, want a loosening: %v", tc.from, tc.to, got, tc.loosens)
		}
	}

	// THE GATE, FIELD BY FIELD: the rule's own gate sent back as it stands,
	// another pointer to an equal value, opens nothing; raised, nothing; and
	// lowered in two fields, those two alone, as LoosenedBy names them.
	gated := Protection{PushRule: PushRule{Pattern: pattern, DirectPush: DirectPersons}, Gate: gate()}
	if got := judged(gated, &Protection{PushRule: PushRule{DirectPush: DirectPersons}, Gate: gate()}); len(got) != 0 {
		t.Errorf("the rule's own gate sent back as it stands is judged %q, want no loosening", got)
	}
	raised := gate()
	raised.MergeBy, raised.Checks = MergeByPersons, []string{"e2e", "lint", "unit"}
	if got := judged(gated, &Protection{PushRule: PushRule{DirectPush: DirectPersons}, Gate: raised}); len(got) != 0 {
		t.Errorf("a gate raised is judged %q, want no loosening", got)
	}
	lowered := gate()
	lowered.MergeBy, lowered.Checks = MergeByWriters, []string{"lint"}
	got := judged(gated, &Protection{PushRule: PushRule{DirectPush: DirectPersons}, Gate: lowered})
	if want := lowered.LoosenedBy(*gate()); len(want) != 2 || !slices.Equal(got, want) {
		t.Errorf("a gate lowered in merge_by and checks is judged %q, want LoosenedBy's two, %q", got, want)
	}

	// A RULE REMOVED loosens; a pattern no rule holds loosens nothing, added
	// to or removed.
	if got := judged(cases["DirectPush"].strict, nil); len(got) != 1 {
		t.Errorf("a removed rule is judged %q, want one loosening", got)
	}
	for _, to := range []*Protection{nil, {PushRule: PushRule{DirectPush: DirectPersons, AllowForce: true,
		AllowDelete: true}, Gate: gate()}} {
		if got := loosenings(nil, onlyRule(pattern, to), pattern); got != nil {
			t.Errorf("a change of a pattern no rule holds is judged %q, want none", got)
		}
	}
}

// onlyRule is the rules a change of pattern to rule leaves where pattern's was
// the one rule: rule alone, or none where it is nil.
func onlyRule(pattern string, rule *Protection) []Protection {
	if rule == nil {
		return nil
	}
	r := *rule
	r.Pattern = pattern
	return []Protection{r}
}
