package chart_test

import (
	"slices"
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
