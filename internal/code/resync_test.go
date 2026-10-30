package code_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/statelog"
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
	l := code.NewResyncs(func() time.Time { return now })
	for i := range code.ResyncBurst {
		if r := l.Take("eng-dev", "run-a", 1); r != nil {
			t.Fatalf("re-sync %d of the burst refused: %+v", i+1, r)
		}
	}
	r := l.Take("eng-dev", "run-a", 1)
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

	other := l.Take("eng-dev", "run-b", 1)
	if other == nil || other.Scope != costlimit.ScopeShared ||
		!strings.Contains(code.NotRejudged(other, "this run's", "this seat's", now), "this seat's re-syncs") {
		t.Errorf("another run of the spent seat: %+v, want the seat's bucket refusing it, named so", other)
	}

	now = now.Add(refill)
	if r := l.Take("eng-dev", "run-a", 1); r != nil {
		t.Errorf("once the refill earned one, the run was refused: %+v", r)
	}
	if r := l.Take("eng-dev", "run-a", 1); r == nil {
		t.Error("the refill's one re-sync was taken twice")
	}
}
