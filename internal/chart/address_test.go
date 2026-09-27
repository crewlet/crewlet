package chart_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// forbiddenHandles are the names no seat may be given on any path: the reserved
// words, and three that are LOGINS rather than handles — a person's, a
// machine's, and the one a Tier A token acts under.
var forbiddenHandles = []string{
	"root", "tree", "barrier", "none", "jane.doe", "ci:release", "token:ops",
}

// EVERY PATH THAT GIVES A SEAT AN ADDRESS REFUSES THE SAME NAMES.
//
// A batch's create checked the reserved words and nothing else did, and no
// path on the chart's side asked the seat-handle grammar at all: `tree`,
// `barrier`, `root` and Datadog's `none` landed through a rename, a content
// write and an import, and `jane.doe`, `ci:release` and `token:ops` became
// seats — names every lookup sends to the identity directory rather than to
// the chart, so a seat nobody could ever reach by name. There is one rule now
// and every path asks it; this walks each path with each name.
func TestEveryPathThatGivesAnAddressRefusesTheSameNames(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))

	for _, name := range forbiddenHandles {
		// A STRUCTURAL CREATE.
		_, err := r.writer.WriteBatch(t.Context(), "op-create-"+name,
			chart.Batch{Operations: []chart.Operation{
				op(chart.OpCreateSeat, chart.KindSeat, name, ""),
			}})
		if !errors.Is(err, chart.ErrRefused) {
			t.Errorf("a batch created the seat %q: %v", name, err)
		}
		// A RENAME.
		_, err = r.publishRename("op-rename-"+name, chart.KindSeat, "sarah-chen", name)
		if !errors.Is(err, chart.ErrRefused) {
			t.Errorf("a rename took the handle %q: %v", name, err)
		}
		// A CONTENT WRITE ON AN ADDRESS NOTHING HOLDS.
		_, err = r.writer.WriteSeat(t.Context(), "op-content-"+name,
			chart.SeatContent{Handle: name, Kind: chart.SeatAgent, Name: "Nobody"})
		if !errors.Is(err, chart.ErrRefused) {
			t.Errorf("a content write created the seat %q: %v", name, err)
		}
	}
	// AN IMPORT, which decides nothing at its decide: the apply declines.
	edges := make([]chart.Edge, 0, len(forbiddenHandles))
	for _, name := range forbiddenHandles {
		edges = append(edges, chart.Edge{
			Object: chart.ObjectRef{Kind: chart.KindSeat, ID: name}})
	}
	r.mustImport("op-import", "rev-forbidden", edges...)

	if got := r.column(`SELECT handle FROM chart_seats ORDER BY handle`); !slices.Equal(
		got, []string{"sarah-chen"}) {
		t.Errorf("the seats are %v, want only [sarah-chen]", got)
	}
}

// A UNIT IS HELD TO THE RESERVED WORDS AND TO ITS OWN KEY RULE, and `none` is
// a SEAT's reserved word only.
//
// The control for the case above: a unit's key is minted from a display name
// and is looser than a handle, and Datadog's `none` means nobody only where a
// seat is named. A rule that refused both kinds the same way would refuse a
// team a company may legitimately call that.
func TestAUnitIsHeldToItsOwnReservedWords(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)

	for _, name := range []string{"root", "tree", "barrier"} {
		_, err := r.writer.WriteBatch(t.Context(), "op-unit-"+name,
			chart.Batch{Operations: []chart.Operation{
				op(chart.OpCreateUnit, chart.KindUnit, name, ""),
			}})
		if !errors.Is(err, chart.ErrRefused) {
			t.Errorf("a batch created the unit %q: %v", name, err)
		}
	}
	r.batch("op-none", op(chart.OpCreateUnit, chart.KindUnit, "none", ""))
	if got := r.column(`SELECT key FROM chart_units`); !slices.Equal(got, []string{"none"}) {
		t.Errorf("the units are %v, want [none]", got)
	}
}

// A DECLINE IS COUNTED ONCE, WHEN ITS BATCH COMMITS, BY WHAT AND WHY.
//
// A declined change is one its writer was told had landed, so the counter is
// the fleet's only aggregate of them. It is counted in the post-commit half
// and deduplicated by record and object, because the store re-runs an apply
// body that failed transiently and a count a disk error doubles is one nobody
// believes.
func TestADeclineIsCountedOnceAtCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("build a recorder: %v", err)
	}
	h.applier.WithMetrics(recorder)

	importing := record(chart.TreeSubject(), chart.OpImport, "op-import",
		chart.ImportPayload{V: chart.DocumentVersion, Revision: "rev-1",
			Edges: []chart.Edge{
				{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "jane.doe"}},
				{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "tree"}},
				{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"}},
			}},
		chart.BatchScope([]chart.ScopeTerm{{Kind: chart.TermRoot}}))
	h.must(importing)
	// NOTHING IS COUNTED INSIDE THE APPLY: an attempt may still roll back.
	if got := declines(recorder); len(got) != 0 {
		t.Fatalf("declines were counted before the commit: %v", got)
	}
	h.applier.Committed(t.Context())

	got := declines(recorder)
	want := map[string]uint64{"place/shape": 1, "place/reserved": 1}
	if len(got) != len(want) || got["place/shape"] != 1 || got["place/reserved"] != 1 {
		t.Errorf("the declines counted are %v, want %v", got, want)
	}
	if rows := h.column(`SELECT handle FROM chart_seats`); !slices.Equal(rows,
		[]string{"sarah-chen"}) {
		t.Errorf("the seats are %v, want [sarah-chen]", rows)
	}
	// AND A SECOND COMMIT COUNTS NOTHING MORE: the set was drained.
	h.applier.Committed(t.Context())
	if again := declines(recorder); again["place/shape"] != 1 {
		t.Errorf("a second commit counted the same decline again: %v", again)
	}
}

// declines reads the decline counter back, keyed op/reason.
func declines(r *metrics.Recorder) map[string]uint64 {
	out := map[string]uint64{}
	for _, s := range r.Read() {
		if s.Name == metrics.ChartApplyDeclined {
			out[s.Attrs["op"]+"/"+s.Attrs["reason"]] += s.Total
		}
	}
	return out
}
