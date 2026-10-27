// Package steersourcetest is the ONE contract suite every [runner.SteerSource]
// runs: the first segment's box ([steer.Box], certified from internal/agent/
// steer) and the record of the coding run a resumed segment reads
// (internal/engine's rowSource, certified from there).
//
// The runner reads a turn's notes through the source and nothing else, so what
// the two must agree on is written once: a waiting note is taken once, by one
// take, however many race for it; Waiting reports a note without taking it,
// and answers without waiting on anything — the tool loop asks it under its
// dispatcher's lock; and a brief's notes handed back after a launch that
// failed are never lost — taken again by the next reader, or, where the launch
// had already recorded them read by a job that never ran, recorded expired
// exactly once. What a source does beyond that — the box's bounds, the
// record's seals and fences — stays in its own package's suite.
package steersourcetest

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/events/types"
)

// Source is one source under the suite, and how a case reaches around it.
type Source struct {
	runner.SteerSource

	// File puts a person's note where the source's notes wait — into the box,
	// onto the run's record — failing the case where it is not accepted.
	File func(t *testing.T, note steer.Note)

	// Expired is the ids of every note the source has recorded expired.
	Expired func() []string

	// LaunchWrote, where the source has a record, plays the launch that took
	// notes for its brief writing them read by the job it opened — before it
	// failed. Nil for a source with no record to write them on.
	LaunchWrote func(t *testing.T, notes []steer.Note)

	// Stall, where the source reads a store, holds every read of it until the
	// function it answers is called. Nil for a source that reads none.
	Stall func() (release func())
}

// Run runs every case against a fresh source from newSource.
func Run(t *testing.T, newSource func(t *testing.T) Source) {
	t.Helper()
	for _, c := range []struct {
		name string
		fn   func(*testing.T, Source)
	}{
		{"ATakeHandsEachWaitingNoteOnce", testATakeHandsEachWaitingNoteOnce},
		{"RacingTakesHandEachNoteToOneOfThem", testRacingTakesHandEachNoteToOneOfThem},
		{"WaitingReportsANoteWithoutTakingIt", testWaitingReportsANoteWithoutTakingIt},
		{"WaitingNeverWaitsOnTheStore", testWaitingNeverWaitsOnTheStore},
		{"ABriefHandedBackIsReadAgain", testABriefHandedBackIsReadAgain},
		{"ABriefTheFailedLaunchReadIsExpiredOnce", testABriefTheFailedLaunchReadIsExpiredOnce},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.fn(t, newSource(t))
		})
	}
}

// at is where a take reads its notes in every case: the reviewer's first round.
var at = steer.Reading{Phase: types.PhaseReview, Iteration: 1, Round: 1, Boundary: types.SteerBoundaryRound}

func note(id string) steer.Note {
	return steer.Note{ID: id, Text: "note " + id, By: "founder"}
}

func ids(notes []steer.Note) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.ID)
	}
	return out
}

// eventually waits up to ten seconds for cond: a source that follows a record
// sees a write a moment after it lands.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10s for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testATakeHandsEachWaitingNoteOnce(t *testing.T, s Source) {
	ctx := t.Context()
	s.File(t, note("n-1"))
	s.File(t, note("n-2"))
	if got := ids(s.Take(ctx, at)); !slices.Equal(got, []string{"n-1", "n-2"}) {
		t.Fatalf("the take handed %v, want both waiting notes, oldest first", got)
	}
	if again := s.Take(ctx, at); len(again) != 0 {
		t.Errorf("a second take handed %v again", ids(again))
	}
}

func testRacingTakesHandEachNoteToOneOfThem(t *testing.T, s Source) {
	ctx := t.Context()
	want := []string{"n-1", "n-2", "n-3", "n-4", "n-5"}
	for _, id := range want {
		s.File(t, note(id))
	}
	const takers = 8
	var (
		mu    sync.Mutex
		taken []string
		wg    sync.WaitGroup
	)
	start := make(chan struct{})
	for range takers {
		wg.Go(func() {
			<-start
			got := ids(s.Take(ctx, at))
			mu.Lock()
			taken = append(taken, got...)
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	slices.Sort(taken)
	if !slices.Equal(taken, want) {
		t.Errorf("racing takes handed %v, want each note once: %v", taken, want)
	}
}

func testWaitingReportsANoteWithoutTakingIt(t *testing.T, s Source) {
	if s.Waiting() {
		t.Fatal("a source with nothing filed reports a note waiting")
	}
	s.File(t, note("n-1"))
	eventually(t, "Waiting to report the filed note", s.Waiting)
	if !s.Waiting() {
		t.Error("asking Waiting took the note it reported")
	}
	if got := ids(s.Take(t.Context(), at)); !slices.Equal(got, []string{"n-1"}) {
		t.Fatalf("the take after Waiting handed %v, want the note it reported", got)
	}
	eventually(t, "Waiting to report the taken note gone", func() bool { return !s.Waiting() })
}

func testWaitingNeverWaitsOnTheStore(t *testing.T, s Source) {
	s.File(t, note("n-1"))
	eventually(t, "Waiting to report the filed note", s.Waiting)
	if s.Stall == nil {
		return
	}
	release := s.Stall()
	defer release()
	answered := make(chan bool, 1)
	go func() { answered <- s.Waiting() }()
	select {
	case waiting := <-answered:
		if !waiting {
			t.Error("Waiting lost the note while its store was held")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Waiting waited on a store read: the tool loop asks it under its dispatcher's lock")
	}
}

func testABriefHandedBackIsReadAgain(t *testing.T, s Source) {
	ctx := t.Context()
	s.File(t, note("n-1"))
	brief := s.Brief(ctx)
	if got := ids(brief); !slices.Equal(got, []string{"n-1"}) {
		t.Fatalf("the brief carried %v, want the waiting note", got)
	}
	// THE LAUNCH FAILS BEFORE IT WROTE ANYTHING, and its notes come back.
	s.Return(ctx, brief)
	if got := ids(s.Take(ctx, at)); !slices.Equal(got, []string{"n-1"}) {
		t.Errorf("after the failed launch the next reader took %v, want the returned note", got)
	}
	if expired := s.Expired(); len(expired) != 0 {
		t.Errorf("a note the next reader took was recorded expired too: %v", expired)
	}
}

func testABriefTheFailedLaunchReadIsExpiredOnce(t *testing.T, s Source) {
	if s.LaunchWrote == nil {
		return
	}
	ctx := t.Context()
	s.File(t, note("n-1"))
	brief := s.Brief(ctx)
	if got := ids(brief); !slices.Equal(got, []string{"n-1"}) {
		t.Fatalf("the brief carried %v, want the waiting note", got)
	}
	// THE LAUNCH WROTE THEM READ BY ITS JOB, then failed: the job never ran.
	s.LaunchWrote(t, brief)
	s.Return(ctx, brief)
	eventually(t, "the returned note to be recorded expired", func() bool { return len(s.Expired()) > 0 })
	time.Sleep(100 * time.Millisecond)
	if expired := s.Expired(); !slices.Equal(expired, []string{"n-1"}) {
		t.Errorf("recorded expired %v, want the note once", expired)
	}
	if again := s.Take(ctx, at); len(again) != 0 {
		t.Errorf("a note recorded expired was taken again: %v", ids(again))
	}
}
