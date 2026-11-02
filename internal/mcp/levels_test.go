package mcp

import (
	"slices"
	"testing"
)

// A TOOL NOTHING SETS STARTS AT THE LEVEL ITS HINTS GIVE IT (the series'
// decision D1), and the entry's own levels win over the hints: a level named
// for the tool, else the entry's default, else allow for a tool asserted
// read-only and approve for every other, one that asserts nothing included.
// The hints read are the ones the operator's overrides leave, so the level
// and the risk a reader is shown are one reading.
//
// Mutation: start an unannotated tool at allow in [StartingLevel], or let the
// default lose to the hints in [Levels.Of], and a row goes red.
func TestAToolNothingSetsStartsAtTheLevelItsHintsGiveIt(t *testing.T) {
	t.Parallel()
	readOnly := Annotations{ReadOnly: Yes}
	for _, tc := range []struct {
		name   string
		levels Levels
		hints  Annotations
		want   Level
	}{
		{"a read-only tool nothing sets runs", Levels{}, readOnly, LevelAllow},
		{"an additive tool nothing sets asks", Levels{}, Annotations{ReadOnly: No, Destructive: No}, LevelApprove},
		{"a tool asserting nothing asks", Levels{}, Annotations{}, LevelApprove},
		{"the default wins over the hints", Levels{Default: LevelAllow}, Annotations{}, LevelAllow},
		{"a default of approve holds a read-only tool", Levels{Default: LevelApprove}, readOnly, LevelApprove},
		{"the tool's own level wins over the default", Levels{Default: LevelBlocked,
			Tools: map[string]Level{"t": LevelApprove}}, readOnly, LevelApprove},
	} {
		if got := tc.levels.Of("t", tc.hints); got != tc.want {
			t.Errorf("%s: Of = %q, want %q", tc.name, got, tc.want)
		}
	}
	if (Levels{}).Blocks("t") || !(Levels{Default: LevelBlocked}).Blocks("t") ||
		(Levels{Default: LevelBlocked, Tools: map[string]Level{"t": LevelApprove}}).Blocks("t") {
		t.Fatal("Blocks reads a level no entry set, or misses one it set")
	}
	if !slices.Equal(AllLevels, []Level{LevelAllow, LevelApprove, LevelBlocked}) || Level("ask").Valid() {
		t.Fatalf("the levels are %v", AllLevels)
	}
}

// A TOOL AT APPROVE IS FILED, offered and listed like any other, and carries
// its level on the definition every registry is handed, so the surface that
// decides a call holds it (internal/tools). A tool the hints start at approve
// and one the entry sets there read the same, and an override that marks a
// tool read-only lets it run.
//
// Mutation: leave the level off [definitionOf], and every filed tool reads
// no level.
func TestAToolAtApproveIsFiledAndCarriesItsLevel(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	spec := helperSpec(t, "tracker", "serve", map[string]string{
		helperToolsEnv: toolsJSON(
			[3]string{"list_issues", "List", `{"readOnlyHint":true}`},
			[3]string{"close_issue", "Close", ""},
			[3]string{"tag_issue", "Tag", ""},
			[3]string{"lookup", "Lookup", ""},
		),
	})
	spec.Levels = Levels{Tools: map[string]Level{"tag_issue": LevelApprove}}
	spec.AnnotationOverrides = map[string]Annotations{"lookup": {ReadOnly: Yes}}
	if _, err := b.Add(t.Context(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got := map[string]Level{}
	for _, tool := range b.ServerTools("tracker") {
		got[tool.Name()] = tool.Level()
	}
	want := map[string]Level{"list_issues": LevelAllow, "close_issue": LevelApprove,
		"tag_issue": LevelApprove, "lookup": LevelAllow}
	for name, level := range want {
		if got[name] != level {
			t.Errorf("%s is filed at %q, want %q (filed: %v)", name, got[name], level, got)
		}
	}
}
