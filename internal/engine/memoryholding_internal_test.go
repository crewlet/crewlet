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

// notHeldAfter is a call that carried rows of the seat, and then met its
// holding changed under it, as a cycle partway through a seat whose release
// begins does.
func notHeldAfter(carried memsync.Carried) func(context.Context, func()) (memsync.Carried, error) {
	return func(context.Context, func()) (memsync.Carried, error) {
		return carried, fmt.Errorf("memsync: publish a seat: %w", memsync.ErrNotHeld)
	}
}

// A CYCLE SAYS NOTHING OF A SEAT THAT IS NOT ITS TO CARRY.
//
// A seat whose acquisition is still hydrating it is held by the host, so the
// cycle reaches it, and its syncer carries none of it: published, every row
// this node held for the seat went to the changelog as its own over the newer
// ones the hydration had yet to read (#511). A seat whose release began while
// the cycle was partway through it stops at its next row. Neither is a
// failure of the cycle's, and neither is a cut: the cycle says nothing of
// them, goes on to the next seat, and leaves where the next cycle starts
// alone.
//
// Mutation: read memsync.ErrNotHeld as a failure or a cut, in a seat's own
// pass or in the backlogs' turn, and this goes red.
func TestACycleSaysNothingOfASeatThatIsNotItsToCarry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		own     map[string]func(context.Context, func()) (memsync.Carried, error)
		backlog map[string]func(context.Context, func()) (memsync.Carried, error)
	}{{
		name: "a seat still being taken",
		own:  map[string]func(context.Context, func()) (memsync.Carried, error){"b": notHeldAfter(memsync.Carried{})},
		backlog: map[string]func(context.Context, func()) (memsync.Carried, error){
			"b": notHeldAfter(memsync.Carried{})},
	}, {
		name: "a seat whose release began partway through its own pass",
		own: map[string]func(context.Context, func()) (memsync.Carried, error){
			"b": notHeldAfter(memsync.Carried{Rows: 12, Marked: 12, StoppedIn: "agent_diary"})},
		backlog: map[string]func(context.Context, func()) (memsync.Carried, error){
			"b": notHeldAfter(memsync.Carried{})},
	}, {
		name: "a seat whose release began partway through its backlog",
		backlog: map[string]func(context.Context, func()) (memsync.Carried, error){
			"b": notHeldAfter(memsync.Carried{Rows: 40, Marked: 40, StoppedIn: "episodes"})},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &scriptedMemory{play: func(ctx context.Context, seat string, _ bool,
				runOut func(),
			) (memsync.Carried, error) {
				if play, ok := tc.own[seat]; ok {
					return play(ctx, runOut)
				}
				return carriedWhole, nil
			}, playBacklog: func(ctx context.Context, seat string, runOut func()) (memsync.Carried, error) {
				if play, ok := tc.backlog[seat]; ok {
					return play(ctx, runOut)
				}
				return movedMarks, nil
			}}
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

// THE RELEASE OF A SEAT WHOSE TAKING FAILED FLUSHES NOTHING AND SAYS NOTHING.
//
// A seat whose acquisition failed, its hydration among the steps that can, is
// given straight back, and its release flushes it. Its syncer began no
// holding, so the flush's own pass carries nothing (memsync.ErrNotHeld):
// carried, it put this node's own copies of the seat's profiles, skills and
// onboarding marker, which can be older than the changelog's, over them
// (#511's second comment). The flush calls no backlog after it, which would
// answer the same, and says nothing: it lost nothing that was this holding's
// to carry, and the failed taking said so already.
//
// Mutation: read memsync.ErrNotHeld in a flush as a failure, or call the
// backlog after it, and this goes red.
func TestTheReleaseOfASeatWhoseTakingFailedSaysNothing(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{
		play: func(ctx context.Context, _ string, _ bool, runOut func()) (memsync.Carried, error) {
			return notHeldAfter(memsync.Carried{})(ctx, runOut)
		},
		playBacklog: notHeldBacklog,
	}
	lines := m.flush(t, "a")
	if got := said(lines); len(got) > 0 {
		t.Errorf("the flush said %q, want nothing", strings.Join(got, "; "))
	}
	if want := []string{"own:a"}; !slices.Equal(m.order, want) {
		t.Errorf("the flush called %s, want %s alone: its backlog would answer the same",
			words.And(m.order), words.And(want))
	}
}

// notHeldBacklog is a backlog the seat's holding does not let be carried.
func notHeldBacklog(ctx context.Context, _ string, runOut func()) (memsync.Carried, error) {
	return notHeldAfter(memsync.Carried{})(ctx, runOut)
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
