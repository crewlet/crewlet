package chart_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A CREATE IS PUBLISHED AS A CREATE, and the apply holds it to that.
//
// An edge used to be a placement and nothing else, and a placement of an object
// the chart already holds is a MOVE — so a create that met an address something
// had taken since its decide moved that object under the creator's parent and
// cleared its lead, while the create itself landed as nothing. These cases hold
// the verb the batch now states on every edge ([chart.Edge.Op]) and the
// permanent reader that still applies a version-1 edge as the placement it was.

// withDeclines gives the harness's applier a recorder, so a case can read what
// it declined.
func withDeclines(t *testing.T, h *harness) *metrics.Recorder {
	t.Helper()
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("build a recorder: %v", err)
	}
	h.applier.WithMetrics(recorder)
	return recorder
}

// ledPlatform seeds engineering and product at the root and platform under
// engineering, led by sarah-chen.
func ledPlatform(h *harness) {
	h.t.Helper()
	h.must(create("op-eng", chart.KindUnit, "engineering", ""))
	h.must(create("op-product", chart.KindUnit, "product", ""))
	h.must(place("op-platform", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		Parent: "engineering", Lead: "sarah-chen", Op: chart.OpCreateUnit,
	}))
}

// placement reads where a unit sits and who leads it, in both places the lead
// is written.
func (h *harness) placement(key string) (parent, lead string, leads []string) {
	h.t.Helper()
	return h.one(`SELECT parent_key FROM chart_units WHERE key = ?`, key),
		h.one(`SELECT lead FROM chart_units WHERE key = ?`, key),
		h.column(`SELECT handle FROM chart_leads WHERE unit_key = ?`, key)
}

// A CREATE WHOSE ADDRESS IS HELD WHEN IT APPLIES IS DECLINED, AND THE OBJECT
// HOLDING IT STAYS WHERE IT IS.
//
// The create below was decided against a snapshot in which platform did not
// exist — a batch behind on the log, or an import, which decides nothing about
// which objects exist. Applied as a placement it would move platform under
// product and take sarah-chen's lead away, which is a reorganisation nobody
// asked for, written by a record that meant to add a team.
func TestACreateWhoseAddressIsHeldIsDeclinedAndMovesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	recorder := withDeclines(t, h)
	ledPlatform(h)

	h.must(place("op-stale-create", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		Parent: "product", Op: chart.OpCreateUnit,
	}))
	h.applier.Committed(t.Context())

	parent, lead, leads := h.placement("platform")
	if parent != "engineering" || lead != "sarah-chen" ||
		!slices.Equal(leads, []string{"sarah-chen"}) {
		t.Errorf("platform sits under %q led by %q (edges %v), want engineering "+
			"and sarah-chen — a create met an object on its address and moved "+
			"it", parent, lead, leads)
	}
	if got := declines(recorder); len(got) != 1 || got["create/present"] != 1 {
		t.Errorf("the declines are %v, want create/present once", got)
	}
	if got := h.count(`chart_history WHERE id = 'op-stale-create'`); got != 0 {
		t.Errorf("the declined create wrote %d history rows — a history row "+
			"saying the unit was created beside a decline saying it was not "+
			"is a record of something that never happened", got)
	}
}

// A VERSION-1 PLACEMENT STILL MOVES WHAT IT FINDS.
//
// The permanent reader: a version-1 edge states no verb, and what it meant when
// it was written is a placement that creates what is absent and moves what is
// present, as FULL POST-STATE — so its empty lead clears the lead. Nothing
// rewrites a record on the log, so this is what a replay of one must still do.
func TestAVersionOnePlacementStillMovesWhatItFinds(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ledPlatform(h)

	h.must(v1(place("op-old-writer",
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
			Parent: "product"},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "omar"},
			Parent: "product"})))

	parent, lead, leads := h.placement("platform")
	if parent != "product" || lead != "" || len(leads) != 0 {
		t.Errorf("platform sits under %q led by %q (edges %v), want product and "+
			"no lead — what a version-1 placement meant", parent, lead, leads)
	}
	if got := h.column(`SELECT handle FROM chart_seats`); !slices.Equal(got,
		[]string{"omar"}) {
		t.Errorf("the seats are %v, want [omar] — a version-1 placement "+
			"creates what is absent", got)
	}

	// AND A VERSION-2 FIELD ON A VERSION-1 EDGE MEANS NOTHING: the build it
	// was written for had nowhere to read a verb or a kind into, so it moved
	// platform back and left omar's kind alone — and so does a replay here.
	h.must(v1(place("op-odd-writer",
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
			Parent: "engineering", Op: chart.OpCreateUnit},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "omar"},
			Kind: chart.SeatHuman})))
	if parent, _, _ := h.placement("platform"); parent != "engineering" {
		t.Errorf("platform sits under %q, want engineering — a version-1 edge "+
			"stating a create was declined as one", parent)
	}
	if got := h.one(`SELECT kind FROM chart_seats WHERE handle = 'omar'`); got != "agent" {
		t.Errorf("omar is %q, want agent — a version-1 edge set a seat's kind", got)
	}
}

// A VERSION-1 RECORD LANDS WHERE ITS FIRST APPLY PUT IT.
//
// Version 2 added rules a creation is held to — the address's shape (a reserved
// word, a seat handle outside the grammar), an edge declined under a unit its
// own record failed to make, a history naming the first edge that LANDED — and
// asked of a version-1 record each one makes this build derive different rows
// from the same log than the build that applied it first: a node that applied a
// version-1 placement of `jane.doe` holds the seat, and a node replaying it here
// would not. One replicated estate, two rosters. So every rule version 2 added
// is keyed on the record's own version, and a version-1 record is read for
// ever as the placement, rekey or content write it was.
func TestAVersionOneRecordLandsWhereItsFirstApplyPutIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	recorder := withDeclines(t, h)

	// A PLACEMENT whose first edge meets a removed address — declined by
	// every version — and whose others name a reserved word, handles
	// outside the grammar and a unit filed under the declined one.
	h.must(create("op-gone", chart.KindUnit, "gone", ""))
	h.must(record(chart.TreeSubject(), chart.OpRemove, "op-remove",
		chart.RemovePayload{V: chart.GateRecordVersion, Reason: "reorg",
			Objects: []chart.ObjectRef{{Kind: chart.KindUnit, ID: "gone"}}},
		chart.BatchScope([]chart.ScopeTerm{{Kind: chart.TermUnit, ID: "gone"}})))
	h.must(v1(place("op-old-place",
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "gone"}},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "tree"}},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "orphans"},
			Parent: "gone"},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "jane.doe"}},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "ci:release"}})))
	if got := h.column(`SELECT key FROM chart_units ORDER BY key`); !slices.Equal(
		got, []string{"orphans", "tree"}) {
		t.Errorf("the units are %v, want [orphans tree] — what the version-1 "+
			"placement wrote when it was first applied", got)
	}
	if got := h.column(`SELECT handle FROM chart_seats ORDER BY handle`); !slices.Equal(
		got, []string{"ci:release", "jane.doe"}) {
		t.Errorf("the seats are %v, want [ci:release jane.doe]", got)
	}
	// ITS HISTORY NAMES ITS FIRST EDGE, which a version-1 apply wrote
	// whether or not that edge landed.
	if got := h.column(`SELECT object_id FROM chart_history
		WHERE id = 'op-old-place'`); !slices.Equal(got, []string{"gone"}) {
		t.Errorf("the placement's history names %v, want [gone]", got)
	}

	// A CONTENT RECORD creates its row on a reserved word, and a REKEY moves
	// a seat onto one.
	h.must(v1(seatRecord("op-old-content", "barrier", "", nil)))
	h.must(create("op-omar", chart.KindSeat, "omar", ""))
	h.must(seatRekey("op-old-rekey", "none", "omar"))
	if got := h.column(`SELECT handle FROM chart_seats ORDER BY handle`); !slices.Equal(
		got, []string{"barrier", "ci:release", "jane.doe", "none"}) {
		t.Errorf("the seats are %v, want [barrier ci:release jane.doe none]", got)
	}

	// WHAT A VERSION-1 RECORD WAS ALWAYS DECLINED FOR, it still is: the
	// removed address, once.
	h.applier.Committed(t.Context())
	if got := declines(recorder); len(got) != 1 || got["place/removed"] != 1 {
		t.Errorf("the declines are %v, want place/removed once", got)
	}

	// THE CONTROL: the same names at version 2 are declined, on every path.
	h.must(place("op-new-place",
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "root"}},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "ana.lopez"}}))
	h.must(create("op-lena", chart.KindSeat, "lena", ""))
	h.must(place("op-new-rename", renameEdge(chart.KindSeat, "lena", "root", "", "")))
	if got := h.column(`SELECT handle FROM chart_seats ORDER BY handle`); !slices.Equal(
		got, []string{"barrier", "ci:release", "jane.doe", "lena", "none"}) {
		t.Errorf("the seats are %v after the version-2 records — a version-2 "+
			"record gave an address its shape forbids", got)
	}
	if got := h.column(`SELECT key FROM chart_units ORDER BY key`); !slices.Equal(
		got, []string{"orphans", "tree"}) {
		t.Errorf("the units are %v after the version-2 placement", got)
	}
}

// A DECLINE IS COUNTED BY WHAT WAS DECLINED, WHICHEVER RULE DECLINED IT.
//
// A create_seat under a unit whose own create was declined is a declined
// CREATE, counted as one under the reason `parent` — it used to be counted as
// `create_seat`, a word the counter never declared, so a panel of the
// documented values saw the held-address creates and not these. And a set_kind
// on a seat a removal took is counted under `set_kind`, which is now declared.
func TestADeclineIsCountedUnderTheWordForWhatWasDeclined(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	recorder := withDeclines(t, h)
	ledPlatform(h)
	h.must(create("op-omar", chart.KindSeat, "omar", ""))
	h.must(record(chart.TreeSubject(), chart.OpRemove, "op-remove",
		chart.RemovePayload{V: chart.GateRecordVersion, Reason: "left",
			Objects: []chart.ObjectRef{{Kind: chart.KindSeat, ID: "omar"}}},
		chart.BatchScope([]chart.ScopeTerm{{Kind: chart.TermSeat, ID: "omar"}})))

	h.must(place("op-stale",
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
			Parent: "product", Op: chart.OpCreateUnit},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "sre"},
			Parent: "platform", Op: chart.OpCreateSeat, Kind: chart.SeatAgent},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "omar"},
			Op: chart.OpSetKind, Kind: chart.SeatHuman}))
	h.applier.Committed(t.Context())

	want := map[string]uint64{
		"create/present": 1, "create/parent": 1, "set_kind/removed": 1,
	}
	if got := declines(recorder); !maps.Equal(got, want) {
		t.Errorf("the declines are %v, want %v", got, want)
	}
	if got := h.column(`SELECT handle FROM chart_seats`); len(got) != 0 {
		t.Errorf("the seats are %v, want none — sre was filed under a unit "+
			"its record never made", got)
	}
}

// A VERB NOBODY DECLARED FAILS ITS RECORD, WHEREVER THE EDGE SITS.
//
// A writer publishing an operation this build never declared is a mistake the
// apply makes visible by failing, identically on every node. Checked edge by
// edge as each was applied, the same verb under a unit its record had failed to
// make was declined instead — counted under a word the counter does not have —
// while under any other parent it failed the record.
func TestAVerbNobodyDeclaredFailsItsRecordWhereverItsEdgeSits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ledPlatform(h)

	for name, parent := range map[string]string{
		"under a unit the record failed to make": "platform",
		"at the root":                            "",
	} {
		_, _, err := h.apply(place("op-"+name,
			chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
				Parent: "product", Op: chart.OpCreateUnit},
			chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "sre"},
				Parent: parent, Op: chart.OperationKind("promote")}))
		if err == nil || !strings.Contains(err.Error(), "promote") {
			t.Errorf("%s: an edge stating an undeclared verb applied (%v)", name, err)
		}
	}
}

// A REDELIVERED CREATE IS ALREADY DONE, NOT A COLLISION.
//
// The row a create makes carries its record's position, so the same record met
// again finds its own object on the address — which is the one held address a
// create does not decline.
func TestARedeliveredCreateIsNotDeclined(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	recorder := withDeclines(t, h)

	h.must(create("op-create", chart.KindUnit, "platform", ""))
	rows, gate, err := h.applyAt(create("op-create", chart.KindUnit, "platform", ""), h.seq)
	if err != nil || gate != "" {
		t.Fatalf("redeliver the create: gate %q, %v", gate, err)
	}
	h.applier.Committed(t.Context())

	if rows != 0 {
		t.Errorf("a redelivered create wrote %d rows, want none", rows)
	}
	if got := declines(recorder); len(got) != 0 {
		t.Errorf("a redelivered create was declined: %v — it met the object it "+
			"made itself", got)
	}
}

// A MOVE OR A LEAD CHANGE OF AN OBJECT THAT IS NOT THERE CREATES NOTHING.
//
// Its batch found the object, so an absent row at the apply is a removal the
// log ordered between — and writing the object back would put a unit into the
// chart that no create arbitrated, on an address the removal tombstoned.
func TestAMoveOfAnObjectThatIsGoneIsDeclined(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	recorder := withDeclines(t, h)
	h.must(create("op-create", chart.KindUnit, "platform", ""))
	h.must(record(chart.TreeSubject(), chart.OpRemove, "op-remove",
		chart.RemovePayload{V: chart.GateRecordVersion,
			Objects: []chart.ObjectRef{{Kind: chart.KindUnit, ID: "platform"}}},
		chart.BatchScope([]chart.ScopeTerm{{Kind: chart.TermUnit, ID: "platform"}})))

	h.must(place("op-late-move", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		Parent: "", Op: chart.OpMove,
	}))
	h.must(place("op-late-lead", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "design"},
		Lead:   "ana", Op: chart.OpSetLead,
	}))
	h.applier.Committed(t.Context())

	if got := h.count("chart_units"); got != 0 {
		t.Errorf("%d unit rows, want none — a move or a lead change created "+
			"the object it named", got)
	}
	want := map[string]uint64{"move/removed": 1, "set_lead/absent": 1}
	if got := declines(recorder); len(got) != len(want) ||
		got["move/removed"] != 1 || got["set_lead/absent"] != 1 {
		t.Errorf("the declines are %v, want %v", got, want)
	}
}

// A VERSION-2 CONTENT RECORD NEVER CREATES ITS OBJECT; A VERSION-1 ONE STILL
// DOES.
//
// Its decide found the row — a content write refuses an object the chart does
// not hold — so meeting none at the apply means a removal or a rename the log
// ordered between, and creating the object back would put a seat into the
// chart that no structural write arbitrated. A version-1 record is read as
// what it meant when it was written.
func TestAContentRecordCreatesNothingFromVersionTwo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	recorder := withDeclines(t, h)

	h.must(seatRecord("op-content", "omar", "", nil))
	h.applier.Committed(t.Context())
	if got := h.count("chart_seats"); got != 0 {
		t.Errorf("%d seat rows, want none — a version-2 content record created "+
			"its seat", got)
	}
	if got := declines(recorder); got["content/absent"] != 1 {
		t.Errorf("the declines are %v, want content/absent once", got)
	}

	h.must(v1(seatRecord("op-old-writer", "omar", "", nil)))
	if got := h.column(`SELECT handle FROM chart_seats`); !slices.Equal(got,
		[]string{"omar"}) {
		t.Errorf("the seats are %v, want [omar] — a version-1 content record "+
			"creates its row, as its writer meant", got)
	}
}
