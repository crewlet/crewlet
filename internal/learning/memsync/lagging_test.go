package memsync

import (
	"testing"
	"testing/synctest"
	"time"
)

// A REPLAY SERVED BY A COPY THAT IS BEHIND IS NEVER A HYDRATION THAT IS SHORT.
//
// A follower applies a row a moment after the leader acknowledged it, so the
// rows a lagging copy lacks are the seat's NEWEST. A table's replay ends only
// on a barrier at or past its hydration's, then its own end barrier. Each
// table's first copy here lacks its row and the barrier past it: one that
// catches up hands both over; where every copy stays behind for a minute the
// seat is refused, naming the barrier. Staged on the model changelog
// (copyfake_test.go): a case waiting for the real window passes unfixed.
//
// Mutation: end a table's replay once its copy says it holds nothing more,
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
