package code_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A SEAT'S RE-SYNCS ARE THE CENSUS'S SEAT TERM: a run (or a turn) on its seat
// takes ResyncBurst at once, the next is refused until the refill — a day over
// statelog.LinearizableReadsPerSeatDay — has earned one, and another run of
// the same seat is refused by the seat's own bucket; the refusal's sentence
// names whose bucket it was and the second it holds a re-sync again, never
// one before.
//
// Mutations: the rate read as a day's 125 a second — the refusal's wait is
// under a second — red; the seat's bucket named as the run's — red.
func TestASeatsResyncsAreTheCensussSeatTerm(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	now := start
	l := code.NewResyncs(func() time.Time { return now }, nil)
	for i := range code.ResyncBurst {
		if r := l.Box(code.RunResyncs("eng-dev", "run-a")); r != nil {
			t.Fatalf("re-sync %d of the burst refused: %+v", i+1, r)
		}
	}
	r := l.Box(code.RunResyncs("eng-dev", "run-a"))
	refill := 24 * time.Hour / time.Duration(statelog.LinearizableReadsPerSeatDay)
	if r == nil || r.Scope != costlimit.ScopePrincipal || r.RetryAfter < refill-time.Second || r.RetryAfter > refill {
		t.Fatalf("past the burst: %+v, want the run's bucket refusing for about %s", r, refill)
	}
	said := code.NotRejudged(r, "this run's", "this seat's", now)
	back := code.ResyncsBackAt(r, now)
	if !strings.Contains(said, "this run's re-syncs are spent until "+back.Format(time.RFC3339)) ||
		back.Before(now.Add(r.RetryAfter)) {
		t.Errorf("the sentence %q names %s, want the second at or after %s", said, back, now.Add(r.RetryAfter))
	}

	other := l.Box(code.RunResyncs("eng-dev", "run-b"))
	if other == nil || other.Scope != costlimit.ScopeShared ||
		!strings.Contains(code.NotRejudged(other, "this run's", "this seat's", now), "this seat's re-syncs") {
		t.Errorf("another run of the spent seat: %+v, want the seat's bucket refusing it, named so", other)
	}

	now = now.Add(refill)
	if r := l.Box(code.RunResyncs("eng-dev", "run-a")); r != nil {
		t.Errorf("once the refill earned one, the run was refused: %+v", r)
	}
	if r := l.Box(code.RunResyncs("eng-dev", "run-a")); r == nil {
		t.Error("the refill's one re-sync was taken twice")
	}
}

// A SEAT HAS ONE TERM ON A NODE, HOWEVER ITS WORK REACHES THE LOG'S HEAD: its
// boxes' re-syncs and its turns' tools' draw on one seat bucket of the node's
// one Resyncs, so a seat whose box spent it finds its turn refused by the
// SEAT's bucket — while a turn's own bucket stays its own, even one spelled as
// the run's launch id, and another seat is untouched. A box's draws, a coding
// run's and a check's, are counted crewlet.code.box.resyncs by outcome; a
// turn's are not a box's.
//
// Mutations: Turn drawing on a limiter of its own — the turn is served past
// its seat's spent term — red; RunResyncs and Turn keying the bare id — the
// turn named as the launch finds the RUN's bucket spent — red; Box uncounted
// — red.
func TestASeatsBoxesAndToolsShareItsTerm(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	l := code.NewResyncs(func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) }, rec)
	for i := range code.ResyncBurst {
		if r := l.Box(code.RunResyncs("eng-dev", "same-id")); r != nil {
			t.Fatalf("re-sync %d of the box's burst refused: %+v", i+1, r)
		}
	}
	if r := l.Turn("eng-dev", "same-id"); r == nil || r.Scope != costlimit.ScopeShared {
		t.Errorf("a turn of the seat whose box spent its term on the node: %+v, want the seat's bucket refusing "+
			"it, and never the run's", r)
	}
	if r := l.Turn("eng-ops", "same-id"); r != nil {
		t.Errorf("another seat's turn was refused: %+v", r)
	}

	key := code.CheckKey{RepoID: "repo", OID: strings.Repeat("ab", 20), Target: "refs/heads/main", Name: "unit",
		Attempt: 1}
	for range code.ResyncBurst {
		_ = l.Box(key.Resyncs())
	}
	if r := l.Box(key.Resyncs()); r == nil {
		t.Errorf("the check's re-sync past its burst of %d was taken", code.ResyncBurst)
	}
	counts := map[string]uint64{}
	for _, s := range rec.Read() {
		if s.Name == metrics.CodeBoxResyncs {
			counts[s.Attrs["outcome"]] += s.Total
		}
	}
	if counts["taken"] != 2*code.ResyncBurst || counts["spent"] != 1 {
		t.Errorf("%s = %v, want taken %d (the run's and the check's bursts) and spent 1 (the check's "+
			"one past its burst; a turn is no box)", metrics.CodeBoxResyncs, counts, 2*code.ResyncBurst)
	}
}
