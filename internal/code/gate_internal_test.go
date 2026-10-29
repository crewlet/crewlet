package code

import (
	"encoding/json"
	"reflect"
	"testing"
)

// gatedRules are a repository's rules as a record carries them: main's with a
// gate, a release glob's without.
func gatedRules() []Protection {
	return []Protection{
		{Pattern: "refs/heads/main", DirectPush: DirectPersons, Gate: &MergeGate{
			MergeBy: MergeByMaintainers, Approvals: ApprovalsOne,
			ApprovalsUnless: []ChangeClass{ChangeClassCommentOnly},
			HumanPaths:      []HumanPath{{Glob: ".crewlet/**", Unless: []ChangeClass{ChangeClassWhitespaceOnly}}},
			Checks:          []string{"lint", "unit"},
			Landing:         Landing{Policy: LandingPreLand, Depth: 4, Batch: 4},
			OwnersApprove:   true, DeleteSource: DeleteSeats, AutoRevert: true, AgentsEditChecks: true,
		}},
		{Pattern: "refs/heads/release/*", DirectPush: DirectNone},
	}
}

// AN UNCHANGED GATE COMPARES EQUAL, however many times it is read. A gate is
// a pointer, so two decodes of one record's settings are two gates in memory,
// and every comparison that decides whether something changed — the
// reconcile's settings comparison and the decide's verdicts — compares them by
// value: by ==, a gated repository's unchanged declaration would be written
// again at every activation, and every round of a batch would form its
// verdicts again. And the comparison reads EVERY field of a gate, held to the
// type by reflection, so a field it skipped would be a change of that field
// every activation drops.
//
// Mutation: compare the rules with slices.Equal in RepoSettings.equal — two
// decodes of one record's settings compare unequal, red; drop Checks from
// MergeGate.Equal — the variant changing them compares equal, red.
func TestAnUnchangedGateComparesEqual(t *testing.T) {
	t.Parallel()
	settings := RepoSettings{
		ManagedBy: ManagedConfig, Kind: RepoProject, Owner: Owner{Kind: OwnerUnit, Ref: "ENG"},
		DefaultBranch: "main", Visibility: VisibilityCompany, Protections: gatedRules(),
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	var one, two RepoSettings
	if err := json.Unmarshal(raw, &one); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &two); err != nil {
		t.Fatal(err)
	}
	if one.Protections[0].Gate == two.Protections[0].Gate {
		t.Fatal("the premise: two decodes of one record share one gate")
	}
	if !one.equal(two) {
		t.Error("two decodes of one record's settings compare unequal: every activation would write them again")
	}

	// THE VERDICTS of a batch's earlier rounds are kept across a round that
	// reads the same rules again.
	var v verdicts
	if err := v.under(one.Protections); err != nil {
		t.Fatal(err)
	}
	formed := verdict{refusal: RefusalProtectedDirect}
	v.of[[2]int{0, 0}] = formed
	if err := v.under(two.Protections); err != nil {
		t.Fatal(err)
	}
	if got, kept := v.of[[2]int{0, 0}]; !kept || got != formed {
		t.Error("a round reading the same gated rules again dropped the verdicts the earlier rounds formed")
	}

	// EVERY FIELD OF A GATE TELLS TWO GATES APART.
	variants := map[string]func(*MergeGate){
		"MergeBy":         func(g *MergeGate) { g.MergeBy = MergeByPersons },
		"Approvals":       func(g *MergeGate) { g.Approvals = ApprovalsNone },
		"ApprovalsUnless": func(g *MergeGate) { g.ApprovalsUnless = nil },
		"HumanPaths": func(g *MergeGate) {
			g.HumanPaths = []HumanPath{{Glob: ".crewlet/**", Unless: []ChangeClass{ChangeClassCommentOnly}}}
		},
		"Checks":           func(g *MergeGate) { g.Checks = []string{"lint"} },
		"Landing":          func(g *MergeGate) { g.Landing.Batch = 2 },
		"OwnersApprove":    func(g *MergeGate) { g.OwnersApprove = false },
		"DeleteSource":     func(g *MergeGate) { g.DeleteSource = DeleteNone },
		"AutoRevert":       func(g *MergeGate) { g.AutoRevert = false },
		"AgentsEditChecks": func(g *MergeGate) { g.AgentsEditChecks = false },
	}
	fields := reflect.TypeFor[MergeGate]()
	for i := range fields.NumField() {
		if _, ok := variants[fields.Field(i).Name]; !ok {
			t.Errorf("MergeGate.%s has no case here — give it one, and Equal a clause comparing it, or a "+
				"declared change to it never reaches the log", fields.Field(i).Name)
		}
	}
	for name, change := range variants {
		var other RepoSettings
		if err := json.Unmarshal(raw, &other); err != nil {
			t.Fatal(err)
		}
		change(other.Protections[0].Gate)
		if one.equal(other) || other.equal(one) {
			t.Errorf("settings whose gates differ only in %s compare equal", name)
		}
	}

	// A RULE WITH A GATE IS NOT THE RULE WITHOUT ONE, and two rules with
	// none are one.
	bare := one.Protections[0]
	bare.Gate = nil
	if bare.Equal(one.Protections[0]) || one.Protections[0].Equal(bare) {
		t.Error("a rule with a gate and the same rule with none compare equal")
	}
	if !one.Protections[1].Equal(two.Protections[1]) {
		t.Error("two reads of a rule with no gate compare unequal")
	}
}
