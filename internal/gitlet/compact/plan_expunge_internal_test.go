package compact

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A WEEKLY SCAN WEIGHS ONLY WHAT ARRIVED A GRACE AGO, EARLIEST FIRST: on the
// maintainer's own schedule, the files an expunge may take — and so the files
// its scan weighs — are those whose rows' newest content arrived at or before
// the instant (gitlet.PackRow.Arrived), a file judged by the latest of its rows,
// so a part that arrived late keeps the whole file, earliest arrival first;
// and a file one of whose rows' arrival is not known (zero: a pack a cache
// adopted before a round served its row) is passed over, the side the grace
// keeps whole, however old its other rows are.
//
// Mutation: take a file whose arrival is not known, and the unknown file is
// weighed; judge a file by its EARLIEST row, and the file with a late part is;
// order by id, and the files come out of arrival order.
func TestAWeeklyScanWeighsOnlyWhatArrivedAGraceAgo(t *testing.T) {
	t.Parallel()
	now := time.Date(2031, 3, 1, 9, 0, 0, 0, time.UTC)
	by := now.Add(-gitlet.PruneGrace)
	got := checksums(scheduled(scheduleRows(by), by))
	if want := []string{"c-older", "c-old", "c-edge"}; !slices.Equal(got, want) {
		t.Fatalf("the weekly scan weighs %v, want %v: earliest first, and nothing young, part young or of "+
			"unknown arrival", got, want)
	}
}

// A WEEKLY PLAN TAKES ONLY WHAT FREES A BYTE, DENSEST FIRST: of the files the
// schedule may take, a cycle's inputs are those its scan found holding bytes
// no tip reaches — a file that frees nothing is never rewritten, however old
// — the one freeing the most for each byte it rewrites first, earliest arrival
// breaking a tie; nothing young, part young or of unknown arrival is taken
// whatever it would free.
//
// Mutation: take a file that frees nothing, and the old reachable file is
// planned; order by arrival alone, and the densest file is not first; drop
// the arrival rule, and the young file freeing the most is planned.
func TestAWeeklyPlanTakesOnlyWhatFreesAByteDensestFirst(t *testing.T) {
	t.Parallel()
	now := time.Date(2031, 3, 1, 9, 0, 0, 0, time.UTC)
	by := now.Add(-gitlet.PruneGrace)
	freed := map[string]int64{
		"c-older": 0,      // old, every object reached: frees nothing
		"c-old":   1 << 8, // a quarter of its kibibyte
		"c-edge":  1 << 9, // half of its kibibyte: the densest
		"c-young": 1 << 10, "c-unknown": 1 << 10, "c-split": 1 << 10,
	}
	p := planExpunge(scheduleRows(by), arrivedBy(by, freed), 1<<40)
	if got, want := checksums(p.inputs), []string{"c-edge", "c-old"}; !slices.Equal(got, want) || !p.due {
		t.Fatalf("the weekly plan's inputs are %v (due %t), want %v: only what frees a byte, densest first, "+
			"and nothing young, part young or of unknown arrival", got, p.due, want)
	}
	if p := planExpunge(scheduleRows(by), arrivedBy(by, map[string]int64{"c-older": 0}), 1<<40); p.due {
		t.Fatalf("a weekly plan over files that free nothing takes %v", checksums(p.inputs))
	}
}

// scheduleRows is a repository's live rows around the instant by: two old
// files, a young one, one with a row of unknown arrival, one with a late part
// and one arrived exactly at by — each of a kibibyte.
func scheduleRows(by time.Time) []gitlet.PackRow {
	row := func(id, checksum string, arrived time.Time) gitlet.PackRow {
		return gitlet.PackRow{PackID: id, Part: "pack", GitChecksum: checksum, Objects: 10, Size: 1 << 10,
			Arrived: arrived}
	}
	return []gitlet.PackRow{
		row("a-old", "c-old", by.Add(-48*time.Hour)),
		row("b-older", "c-older", by.Add(-72*time.Hour)),
		row("c-young", "c-young", by.Add(time.Hour)),
		row("d-unknown", "c-unknown", time.Time{}),
		row("e-unknown", "c-unknown", by.Add(-96*time.Hour)),
		row("f-split", "c-split", by.Add(-120*time.Hour)),
		row("g-split", "c-split", by.Add(time.Minute)),
		row("h-edge", "c-edge", by),
	}
}

// checksums is the files' checksums, in their order.
func checksums(files []pack) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.checksum
	}
	return out
}

// AN EXPUNGE ON THE SCHEDULE IS DUE PAST A QUARTER AND AT THE LEAST: its
// unreachable bytes must be MORE than 1/gitlet.ExpungeShare of the live bytes —
// exactly a quarter is not — and AT LEAST the least an expunge frees — one
// byte under it is not, whatever its share — and every compactor the engine
// builds holds gitlet.ExpungeMinBytes as that least.
//
// Mutation: drop `unreachable >= least` from expungeDue, and the case under
// the least is due; judge the share with >=, and exactly a quarter is; leave
// the production bounds' least at zero, and the last check fails.
func TestAnExpungeIsDuePastAQuarterAndAtTheLeast(t *testing.T) {
	t.Parallel()
	const least = 1 << 20
	for name, tc := range map[string]struct {
		unreachable, live int64
		due               bool
	}{
		"a byte under the least, half unreachable": {least - 1, 2 * least, false},
		"at the least, half unreachable":           {least, 2 * least, true},
		"at the least, exactly a quarter":          {least, gitlet.ExpungeShare * least, false},
		"at the least, a byte past a quarter":      {least, gitlet.ExpungeShare*least - 1, true},
		"past the least, under a quarter":          {2 * least, gitlet.ExpungeShare*2*least + 1, false},
	} {
		if got := expungeDue(tc.unreachable, tc.live, least); got != tc.due {
			t.Errorf("%s: expungeDue(%d, %d, %d) = %t, want %t", name, tc.unreachable, tc.live, least, got, tc.due)
		}
	}
	if production.expungeMin != gitlet.ExpungeMinBytes {
		t.Errorf("the production bounds hold %d as the least an expunge frees, want gitlet.ExpungeMinBytes (%d)",
			production.expungeMin, gitlet.ExpungeMinBytes)
	}
}
