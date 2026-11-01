package compact

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// A WEEKLY PLAN TAKES ONLY WHAT ARRIVED A GRACE AGO, EARLIEST FIRST: planned
// on the maintainer's own schedule, an expunge's inputs are the FILES whose
// rows' newest content arrived at or before the instant (code.PackRow.Arrived)
// — a file judged by the latest of its rows, so a part that arrived late keeps
// the whole file — earliest arrival first; and a file one of whose rows'
// arrival is not known (zero: a pack a cache adopted before a round served its
// row) is passed over, the side the grace keeps whole, however old its other
// rows are.
//
// Mutation: take a file whose arrival is not known, and the unknown file is
// planned; judge a file by its EARLIEST row, and the file with a late part is
// planned; order by id, and the inputs come out of arrival order.
func TestAWeeklyPlanTakesOnlyWhatArrivedAGraceAgo(t *testing.T) {
	t.Parallel()
	now := time.Date(2031, 3, 1, 9, 0, 0, 0, time.UTC)
	by := now.Add(-code.PruneGrace)
	row := func(id, checksum string, arrived time.Time) code.PackRow {
		return code.PackRow{PackID: id, Part: "pack", GitChecksum: checksum, Objects: 10, Size: 1 << 10,
			Arrived: arrived}
	}
	live := []code.PackRow{
		row("a-old", "c-old", by.Add(-48*time.Hour)),
		row("b-older", "c-older", by.Add(-72*time.Hour)),
		row("c-young", "c-young", by.Add(time.Hour)),
		row("d-unknown", "c-unknown", time.Time{}),
		row("e-unknown", "c-unknown", by.Add(-96*time.Hour)),
		row("f-split", "c-split", by.Add(-120*time.Hour)),
		row("g-split", "c-split", by.Add(time.Minute)),
		row("h-edge", "c-edge", by),
	}
	p := planExpunge(live, arrivedBy(by), 1<<40)
	var got []string
	for _, in := range p.inputs {
		got = append(got, in.checksum)
	}
	if want := []string{"c-older", "c-old", "c-edge"}; !slices.Equal(got, want) || !p.due {
		t.Fatalf("the weekly plan's inputs are %v (due %t), want %v: earliest first, and nothing young, "+
			"part young or of unknown arrival", got, p.due, want)
	}
}
