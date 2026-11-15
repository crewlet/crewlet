package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/learning/memsync"
	"github.com/crewlet/crewlet/internal/words"
)

// A SEAT'S MEMORY IS PUBLISHED ONLY WHILE THIS NODE HOLDS IT (#511).
//
// The memory syncer carries a seat only under its holding on this node: the
// cycle while it is live, from the moment the seat's hydration here completed,
// and the release's flush once the release has turned it to releasing. A call
// the holding does not let carry the seat answers memsync.ErrNotHeld. The
// cases below hold the engine's half: what a cycle and a flush say of such a
// seat, and the order a release turns, flushes and forgets it in. The
// syncer's half, what each call carries, is memsync's (holding_test.go).

// passScript plays one call of a seat's, handed the bound and runOut, which
// spends it.
type passScript func(ctx context.Context, runOut func()) (memsync.Carried, error)

// passScripts plays a seat's own pass, or its backlog, by its handle.
type passScripts map[string]passScript

// notHeldAfter is a call that carried rows of the seat, and then met its
// holding changed under it, as a cycle partway through a seat whose release
// begins does.
func notHeldAfter(carried memsync.Carried) passScript {
	return func(context.Context, func()) (memsync.Carried, error) {
		return carried, fmt.Errorf("memsync: publish a seat: %w", memsync.ErrNotHeld)
	}
}

// notHeldAsItRunsOut is notHeldAfter on a call that returns as the bound runs
// out: it spends the bound before it answers.
func notHeldAsItRunsOut(carried memsync.Carried) passScript {
	return func(ctx context.Context, runOut func()) (memsync.Carried, error) {
		runOut()
		return notHeldAfter(carried)(ctx, runOut)
	}
}

// scriptedBy is a memory syncer whose seats' own passes and backlogs play own
// and backlog, and every other call of theirs carries all it holds.
func scriptedBy(own, backlog passScripts) *scriptedMemory {
	return &scriptedMemory{play: func(ctx context.Context, seat string, _ bool,
		runOut func(),
	) (memsync.Carried, error) {
		if play, ok := own[seat]; ok {
			return play(ctx, runOut)
		}
		return carriedWhole, nil
	}, playBacklog: func(ctx context.Context, seat string, runOut func()) (memsync.Carried, error) {
		if play, ok := backlog[seat]; ok {
			return play(ctx, runOut)
		}
		return movedMarks, nil
	}}
}

// A CYCLE SAYS NOTHING OF A SEAT THAT IS NOT ITS TO CARRY.
//
// A seat whose acquisition is still hydrating it is held by the host, so the
// cycle reaches it, and its syncer carries none of it: published, every row
// this node held for the seat went to the changelog as its own over the newer
// ones the hydration had yet to read (#511). A seat whose release began while
// the cycle was partway through it stops at its next row. Neither is a
// failure of the cycle's: the cycle says nothing of them, goes on to the next
// seat, and leaves where the next cycle starts alone.
//
// Mutation: read memsync.ErrNotHeld as a failure, in a seat's own pass or in
// the backlogs' turn, and this goes red.
func TestACycleSaysNothingOfASeatThatIsNotItsToCarry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		own, backlog passScripts
	}{{
		name:    "a seat still being taken",
		own:     passScripts{"b": notHeldAfter(memsync.Carried{})},
		backlog: passScripts{"b": notHeldAfter(memsync.Carried{})},
	}, {
		name: "a seat whose release began partway through its own pass",
		own: passScripts{
			"b": notHeldAfter(memsync.Carried{Rows: 12, Marked: 12, StoppedIn: "agent_diary"})},
		backlog: passScripts{"b": notHeldAfter(memsync.Carried{})},
	}, {
		name: "a seat whose release began partway through its backlog",
		backlog: passScripts{
			"b": notHeldAfter(memsync.Carried{Rows: 40, Marked: 40, StoppedIn: "episodes"})},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := scriptedBy(tc.own, tc.backlog)
			loop := &memorySync{}
			report := m.cycle(t, loop, "a", "b", "c")
			if got := said(report.lines()); len(got) > 0 {
				t.Errorf("the cycle said %q, want nothing: a seat its syncer does not hold is "+
					"no failure and no cut", strings.Join(got, "; "))
			}
			if report.ranOut || len(report.failed) > 0 {
				t.Errorf("the cycle reported ranOut=%v and %d failed calls, want neither",
					report.ranOut, len(report.failed))
			}
			want := []string{"own:a", "own:b", "own:c", "backlog:a", "backlog:b", "backlog:c"}
			if !slices.Equal(m.order, want) {
				t.Errorf("the cycle called %s, want %s: every turn, the seat it does not hold "+
					"having had its own", words.And(m.order), words.And(want))
			}
			if loop.resume != "" || loop.backlogResume != "" {
				t.Errorf("the next cycle starts at %q and its backlogs' turn at %q, want both at "+
					"the first seat: this cycle had every turn", loop.resume, loop.backlogResume)
			}
		})
	}
}

// A SEAT NOT THE CYCLE'S TO CARRY IS NO CUT WHEN THE BOUND RUNS OUT IN IT.
//
// A seat whose release begins as the cycle's bound runs out returns from its
// call with the bound spent, answering memsync.ErrNotHeld. The bound ran out,
// so the cycle stops; but not in a call of a seat it carries, so it ran out
// between two calls: it names no seat, and the next cycle, or the next
// backlogs' turn, starts at the first call it did not make. Read as a cut of
// that seat's, the next cycle would start at a seat that is no longer held
// here, and the cycle would say the seat's memory was not carried.
//
// Mutation: read memsync.ErrNotHeld as a cut, in a seat's own pass or in the
// backlogs' turn, and this goes red.
func TestASeatNotTheCyclesToCarryIsNoCutWhenTheBoundRunsOutInIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		own, backlog         passScripts
		want                 string
		resume, backlogStart string
	}{{
		name: "in its own pass",
		own: passScripts{"b": notHeldAsItRunsOut(memsync.Carried{Rows: 12, Marked: 12,
			StoppedIn: "agent_diary"})},
		want:   "INFO memory_sync_ran_out pass=own next=c not_reached=c,(backlogs)",
		resume: "c",
	}, {
		name: "in its backlog",
		backlog: passScripts{"b": notHeldAsItRunsOut(memsync.Carried{Rows: 40, Marked: 40,
			StoppedIn: "episodes"})},
		want: "INFO memory_sync_ran_out pass=backlog next= not_reached= " +
			"backlogs_next=c backlogs_not_reached=c",
		backlogStart: "c",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := scriptedBy(tc.own, tc.backlog)
			loop := &memorySync{}
			report := m.cycle(t, loop, "a", "b", "c")
			if got, want := said(report.lines()), []string{tc.want}; !slices.Equal(got, want) {
				t.Errorf("the cycle said %q, want %q: it ran out between two calls, in none "+
					"of a seat it carries", got, want)
			}
			if len(report.failed) > 0 {
				t.Errorf("the cycle reported %d failed calls, want none", len(report.failed))
			}
			if loop.resume != tc.resume || loop.backlogResume != tc.backlogStart {
				t.Errorf("the next cycle starts at %q and its backlogs' turn at %q, want %q and "+
					"%q: at the first call this cycle did not make", loop.resume,
					loop.backlogResume, tc.resume, tc.backlogStart)
			}
		})
	}
}

// THE RELEASE OF A SEAT WITH NO HOLDING TO TURN FLUSHES NOTHING.
//
// A seat whose acquisition failed is given straight back, and released. When
// its hydration was the step that failed, or never ran, its syncer has no
// holding of it to turn (memsync.Syncer.Releasing answers false), and its
// store can hold copies of the seat's profiles, skills and onboarding marker
// older than the changelog's: flushed, they went over the changelog's (#511's
// second comment). The release flushes nothing, and forgets the seat all the
// same.
//
// Mutation: flush whatever Releasing answers, or forget the seat only when it
// flushes, and this goes red.
func TestTheReleaseOfASeatWithNoHoldingToTurnFlushesNothing(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: completes, unheld: map[string]bool{"a": true}}
	releaseSeatMemory(t.Context(), m, "a")
	if want := []string{"releasing:a", "forget:a"}; !slices.Equal(m.order, want) {
		t.Errorf("the release called %s, want %s: no flush of a seat this node has no "+
			"holding of", words.And(m.order), words.And(want))
	}
}

// A FLUSH WHOSE HOLDING ENDS UNDER IT WARNS.
//
// A release flushes only a holding it turned to releasing, so a flush that
// meets memsync.ErrNotHeld met that holding ended under it, which nothing
// should do: what the flush did not carry is lost to the next holder, as a
// refusal's is, and it warns as one does. Silent on it, as a cycle is, a
// release that flushed under a holding it had not turned would lose every
// seat's last writes without a line. An own pass that met it calls no backlog
// after it, which would answer the same.
//
// Mutation: say nothing of memsync.ErrNotHeld in a flush, in its own pass or
// in its backlog, or call the backlog after an own pass that met it, and this
// goes red.
func TestAFlushWhoseHoldingEndsUnderItWarns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		own, backlog passScripts
		want, called []string
	}{{
		name: "in its own pass",
		own: passScripts{"a": notHeldAfter(memsync.Carried{Rows: 12, Marked: 12,
			StoppedIn: "agent_diary"})},
		want:   []string{"WARN seat_memory_not_flushed seat=a pass=own"},
		called: []string{"own:a"},
	}, {
		name: "in its backlog",
		backlog: passScripts{"a": notHeldAfter(memsync.Carried{Rows: 40, Marked: 40,
			StoppedIn: "episodes"})},
		want:   []string{"WARN seat_memory_not_flushed seat=a pass=backlog"},
		called: []string{"own:a", "backlog:a"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := scriptedBy(tc.own, tc.backlog)
			if got := said(m.flush(t, "a")); !slices.Equal(got, tc.want) {
				t.Errorf("the flush said %q, want %q", got, tc.want)
			}
			if !slices.Equal(m.order, tc.called) {
				t.Errorf("the flush called %s, want %s", words.And(m.order),
					words.And(tc.called))
			}
		})
	}
}

// A RELEASE TURNS THE SEAT TO RELEASING BEFORE ITS FLUSH, AND FORGETS IT
// AFTER.
//
// From the moment a seat's release begins its memory is the flush's to carry,
// and the cycle's no more: a cycle partway through the seat stops at its next
// row, and one that reaches it later carries nothing of it. So the release
// turns the seat's holding to releasing before the flush's first call, which
// carries only under that holding, and forgets the seat once the flush has
// returned. Flushed first, the flush would carry nothing, and a cycle would go
// on publishing the seat beside it, its older copies of the seat's whole
// tables landing after the flush's (#511's first comment).
//
// Mutation: flush before turning the holding, skip the turn, or forget the
// seat before the flush, and this goes red.
func TestAReleaseTurnsTheSeatToReleasingBeforeItsFlushAndForgetsItAfter(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: completes}
	releaseSeatMemory(t.Context(), m, "a")
	want := []string{"releasing:a", "own:a", "backlog:a", "forget:a"}
	if !slices.Equal(m.order, want) {
		t.Errorf("the release called %s, want %s", words.And(m.order), words.And(want))
	}
}
