package chart_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
)

// A STRUCTURAL RENAME AT THE APPLY: one record, renames first, then every
// placement on the rows they leave.

// renameEdge is a version-2 rename of one object, with the post-state its
// batch left it in.
func renameEdge(kind chart.ObjectKind, from, to, parent, lead string) chart.Edge {
	return chart.Edge{Object: chart.ObjectRef{Kind: kind, ID: to},
		Parent: parent, Lead: lead, Op: chart.OpRename, From: from}
}

// A BATCH THAT RENAMES A TEAM AND MOVES ONE OF ITS MEMBERS OUT LANDS BOTH.
//
// The rename's cascade moves every member onto the new key, and the member's
// own edge then places it where the batch left it — on a row the same record
// has already written. A placement that deferred to anything written at its own
// position would leave the member where the cascade put it.
func TestARenamedTeamsMemberMovedInTheSameBatchLandsWhereTheBatchLeftIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(create("op-platform", chart.KindUnit, "platform", ""))
	h.must(create("op-product", chart.KindUnit, "product", ""))
	h.must(create("op-bob", chart.KindSeat, "bob", "platform"))
	h.must(create("op-ana", chart.KindSeat, "ana", "platform"))

	h.must(place("op-batch",
		renameEdge(chart.KindUnit, "platform", "infra", "", ""),
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "bob"},
			Parent: "product", Op: chart.OpMove}))

	if got := h.one(`SELECT unit_key FROM chart_seats WHERE handle = 'bob'`); got != "product" {
		t.Errorf("bob sits in %q, want product — the rename's cascade won over "+
			"the edge the batch placed him with", got)
	}
	if got := h.seat("bob").UnitKey; got != "product" {
		t.Errorf("bob's document sits in %q, want product", got)
	}
	if got := h.one(`SELECT unit_key FROM chart_seats WHERE handle = 'ana'`); got != "infra" {
		t.Errorf("ana sits in %q, want infra — she followed the team by cascade", got)
	}
}

// TWO RENAMES IN ONE RECORD COMPOSE ON A ROW BOTH REACH.
//
// A seat's rename moves the units it leads, a unit's rename moves its
// children, and a unit that is both led by the one and a child of the other is
// written by both cascades at one position. Each moves one reference and
// leaves the other as it found it, so the second must not skip the row because
// the first already wrote it.
func TestTwoRenamesInOneRecordComposeOnARowBothReach(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(create("op-eng", chart.KindUnit, "engineering", ""))
	h.must(create("op-sarah", chart.KindSeat, "sarah-chen", ""))
	h.must(place("op-platform", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		Parent: "engineering", Lead: "sarah-chen", Op: chart.OpCreateUnit}))

	h.must(place("op-batch",
		renameEdge(chart.KindSeat, "sarah-chen", "sarah-okonkwo", "", ""),
		renameEdge(chart.KindUnit, "engineering", "eng", "", "")))

	parent := h.one(`SELECT parent_key FROM chart_units WHERE key = 'platform'`)
	lead := h.one(`SELECT lead FROM chart_units WHERE key = 'platform'`)
	if parent != "eng" || lead != "sarah-okonkwo" {
		t.Errorf("platform sits under %q led by %q, want eng and sarah-okonkwo",
			parent, lead)
	}
	if got := h.unit("platform"); got.ParentKey != "eng" || got.Lead != "sarah-okonkwo" {
		t.Errorf("platform's document sits under %q led by %q", got.ParentKey, got.Lead)
	}
}

// A RENAME A RECORD FROM ANOTHER SUBJECT BEAT IS DECLINED, AND SO IS WHAT THE
// BATCH PLACED UNDER THE ADDRESS IT MEANT TO TAKE.
//
// A structural rename contends with every structural write, but not with a
// version-1 claim on the address's own subject from a build mid-upgrade. When
// such a claim took the address first, the rename is declined — and a seat the
// same batch moved into the renamed team is declined with it, rather than being
// filed under whatever unit now answers to that address.
func TestADeclinedRenameDeclinesWhatWasPlacedUnderItsAddress(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	recorder := withDeclines(t, h)
	h.must(create("op-platform", chart.KindUnit, "platform", ""))
	h.must(create("op-design", chart.KindUnit, "design", ""))
	h.must(create("op-bob", chart.KindSeat, "bob", ""))

	// AN OLDER BUILD'S CLAIM, ordered first on the log.
	h.must(v1(record(chart.RekeySubject("infra"), chart.OpRekey, "op-claim",
		chart.RekeyPayload{V: chart.DocumentVersion, Key: "infra",
			FormerKey: "design",
			Object:    chart.ObjectRef{Kind: chart.KindUnit, ID: "infra"}},
		chart.BatchScope([]chart.ScopeTerm{{Kind: chart.TermUnit, ID: "infra"}}))))

	h.must(place("op-batch",
		renameEdge(chart.KindUnit, "platform", "infra", "", ""),
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "bob"},
			Parent: "infra", Op: chart.OpMove}))
	h.applier.Committed(t.Context())

	if got := h.column(`SELECT key FROM chart_units ORDER BY key`); !slices.Equal(got,
		[]string{"infra", "platform"}) {
		t.Errorf("the units are %v, want [infra platform] — the claim's unit "+
			"and the one whose rename was declined", got)
	}
	if got := h.one(`SELECT unit_key FROM chart_seats WHERE handle = 'bob'`); got != "" {
		t.Errorf("bob sits in %q, want the org root where he was — the team "+
			"his batch moved him into is not the unit on that address", got)
	}
	want := map[string]uint64{"rename/taken": 1, "move/parent": 1}
	if got := declines(recorder); len(got) != len(want) ||
		got["rename/taken"] != 1 || got["move/parent"] != 1 {
		t.Errorf("the declines are %v, want %v", got, want)
	}
}

// A REDELIVERED RENAME IS ALREADY DONE.
//
// The object is on the new address, stamped at this record's own position,
// which is the one absence of the old address a rename does not decline.
func TestARedeliveredRenameIsNotDeclined(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	recorder := withDeclines(t, h)
	h.must(create("op-platform", chart.KindUnit, "platform", ""))

	rename := place("op-rename", renameEdge(chart.KindUnit, "platform", "infra", "", ""))
	h.must(rename)
	if _, gate, err := h.applyAt(rename, h.seq); err != nil || gate != "" {
		t.Fatalf("redeliver the rename: gate %q, %v", gate, err)
	}
	h.applier.Committed(t.Context())

	if got := declines(recorder); len(got) != 0 {
		t.Errorf("a redelivered rename was declined: %v", got)
	}
	if got := h.unit("infra"); !slices.Equal(got.FormerKeys, []string{"platform"}) {
		t.Errorf("infra's former keys are %v, want [platform] once", got.FormerKeys)
	}
}
