package tracker

import "testing"

// THE COMPATIBILITY RUNG ANSWERS ONLY IN THIS BUILD'S OWN VOCABULARY.
//
// [fallbackKind] is reached for one record class: one an older build wrote
// QUIETLY, before [MutationRecord.Kind] existed, which a rolling upgrade makes
// ordinary traffic for as long as one takes. Nothing this build publishes
// reaches it, which is exactly why it needs a test of its own — a round-trip
// case would exercise the record's stated kind and leave this untouched, and
// the bug it is being fixed for would survive the suite.
//
// The bug: it ended at `ChangeKind(op)` — the OPERATION, cast. Four of the
// nine [OpKind]s are not [ChangeKind]s, and three are NEAR-MISSES of one:
// `tombstone` against `removed`, `restore` against `restored`, `purge` against
// `purged`. A value one letter from the right one is the worst possible
// answer here, because it reads correct in a database dump and matches no
// filter anybody will ever write.
//
// IN-PACKAGE, because the function is unexported and exporting it to test it
// would widen a surface for a reason that is not a caller's.
func TestTheFallbackKindIsAlwaysAKindThisBuildKnows(t *testing.T) {
	t.Parallel()
	// EVERY OPERATION, from the declared list rather than a literal one,
	// so an op added later is covered without anybody remembering to.
	for _, op := range OpKinds {
		for _, applied := range []map[string]Delta{
			nil,
			{},
			{"status": {}},
			{"assignee": {}},
			{"title": {}},
			{"something_this_build_does_not_rank": {}},
		} {
			got := fallbackKind(applied, op)
			if !got.Valid() {
				t.Errorf("fallbackKind(%v, %s) = %q, which is not a change "+
					"kind — a history row filed under it is one no reader "+
					"can ever select", applied, op, got)
			}
		}
	}
}

// AND THE THREE NEAR-MISSES MAP TO THE WORD THE FILTER USES.
//
// Asserted by name rather than left to the validity walk above, because a
// fallback that answered `fields` for all three would pass that walk while
// filing every removal, restore and purge under the wrong word.
func TestTheFallbackKindTranslatesTheOperationsThatHaveAKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		op   OpKind
		want ChangeKind
	}{
		{OpCreate, ChangeCreated},
		{OpTombstone, ChangeRemoved},
		{OpRestore, ChangeRestored},
		{OpPurge, ChangePurged},
	} {
		// WITH DELTAS PRESENT, so the translation beats the field
		// precedence rather than merely running before it: a removal
		// whose patch also moved the status is still a removal.
		for _, applied := range []map[string]Delta{nil, {"status": {}}} {
			if got := fallbackKind(applied, tc.op); got != tc.want {
				t.Errorf("fallbackKind(%v, %s) = %q, want %q — the operation's "+
					"own word is one letter from this and matches no filter",
					applied, tc.op, got, tc.want)
			}
		}
	}
	// A PATCH IS DECIDED BY WHAT MOVED, and falls to `fields` rather than
	// to the operation when nothing in the precedence did.
	if got := fallbackKind(map[string]Delta{"status": {}}, OpPatch); got != ChangeStatus {
		t.Errorf("a patch that moved the status fell back to %q", got)
	}
	if got := fallbackKind(map[string]Delta{"nothing_ranked": {}}, OpPatch); got != ChangeFields {
		t.Errorf("a patch that moved nothing ranked fell back to %q, want "+
			"fields — `patch` is the operation and not a change kind", got)
	}
}
