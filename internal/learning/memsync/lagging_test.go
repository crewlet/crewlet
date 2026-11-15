package memsync

import (
	"testing"
	"testing/synctest"
	"time"
)

// A REPLAY SERVED BY A COPY THAT IS BEHIND IS NEVER A HYDRATION THAT IS SHORT.
//
// On a fleet a replay's consumer is placed on a random member of the stream,
// and a follower applies a row a moment after the leader acknowledged it. The
// rows a lagging copy has not applied are the seat's NEWEST — the ones its
// last owner wrote just before placement moved it — and a replay that counted
// what its copy held, delivered it, and reported success served the seat with
// amnesia. A table's replay is done only once it has received a barrier at or
// past the one its hydration published before it, and then its own end
// barrier: each table's first copy here lacks the table's row and the
// barrier past it. One that catches up hands both over; one that never does is
// placed again, and where every copy stays behind for a minute the seat is
// refused, naming the barrier it never received, rather than admitted having
// forgotten what it did last. Played on a bubble's clock over the model
// changelog (copyfake_test.go), where a copy behind is staged rather than
// raced, because the window is the gap between an acknowledgement and a
// follower's apply, and a case that waits for it to open passes with the fix
// removed.
//
// Mutation: take the copy's word for the end, ending a table's replay once
// its copy says it holds nothing more whether or not the barrier has arrived,
// and both cases come back as a success short of each table's row.
func TestAReplayServedByACopyThatIsBehindIsNeverAShortHydration(t *testing.T) {
	t.Parallel()
	owner := openStore(t)
	seedMemory(t, owner)
	published := publishedBy(t, owner)
	if len(published) != len(tables) {
		t.Fatalf("published %d rows, want %d", len(published), len(tables))
	}
	for _, tc := range []struct {
		name      string
		catchesUp bool
	}{
		{"a copy that catches up hands over the rest", true},
		{"a copy that never catches up fails the hydration", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			next := openStore(t)
			synctest.Test(t, func(t *testing.T) {
				log := newFakeChangelog()
				copies := newFakeCopies(log)
				for i, seq := range onto(log, published) {
					row, err := decode(published[i].data)
					if err != nil {
						t.Fatal(err)
					}
					// EACH TABLE'S COPY LACKS ITS ROW AND THE BARRIER PAST IT.
					script := copyScript{behind: true, holdsTo: seq - 1}
					if tc.catchesUp {
						script.catchUpAfter = 500 * time.Millisecond
					}
					copies.scripts[row.Table] = []copyScript{script}
				}
				carried, err := bubbleSyncer(t, next, copies, false).Hydrate(t.Context(), seat.Handle)
				if tc.catchesUp {
					if err != nil || carried != len(tables) {
						t.Fatalf("Hydrate = (%d, %v), want all %d rows: the copy caught up, so "+
							"the rows it was missing are there to be read", carried, err, len(tables))
					}
					return
				}
				if err == nil {
					t.Fatalf("Hydrate reported success with %d of %d rows: the seat would serve "+
						"having forgotten the newest thing it learned", carried, len(tables))
				}
				if !says(err, "it reads to never arrived", "its copy held nothing for it") {
					t.Errorf("the hydration was refused for another reason than the barrier its "+
						"copies never delivered: %v", err)
				}
			})
		})
	}
}
