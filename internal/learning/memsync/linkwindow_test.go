package memsync

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/jsapi"
)

// A REPLAY PULL WAITS FOR ITS LINK'S WINDOW.
//
// Across a link a replay's pulls take from the ONE window the object store's
// transfers share (Config.Window). With an upload's piece out, a pull is not
// asked until the piece is given back. Its wait is a turn, not a silence: the
// replay asked its copy nothing, so waiting past every stall refuses no seat,
// and once let out it carries every row and leaves the window empty.
//
// Mutation: ask a replay's pull without taking it from the window, and the
// pull is asked beside the upload's piece.
func TestAReplayPullWaitsForItsLinksWindow(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		notesOnto(t, log, "n", 3, 512)
		copies := newFakeCopies(log)
		s := bubbleSyncer(t, db, copies, true)
		piece := jsapi.RequestBytes(128 << 10)
		if err := s.window.Take(t.Context(), piece); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		var carried int
		go func() {
			var err error
			carried, err = s.Hydrate(t.Context(), seat.Handle)
			done <- err
		}()
		synctest.Wait()
		if pulls := copies.pullsOf("agent_diary"); len(pulls) != 0 {
			t.Fatalf("the diary's replay asked %d pulls beside an upload's piece holding the link's window",
				len(pulls))
		}
		if _, waiting := s.window.Held(); waiting != 1 {
			t.Fatalf("%d takes wait behind the upload's piece, want the replay's pull", waiting)
		}
		// A TURN, NOT A SILENCE: past the longest stall any pull is given.
		time.Sleep(stallOf(s, pullBytesOf(s)) + time.Minute)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("the hydration ended while its pull waited its turn: %v", err)
		default:
		}

		s.window.Give(piece)
		if err := <-done; err != nil || carried != 3 {
			t.Fatalf("Hydrate = (%d, %v), want the 3 notes once the window let the pull out", carried, err)
		}
		if len(copies.pullsOf("agent_diary")) == 0 {
			t.Fatal("the hydration carried the diary and asked it no pull")
		}
		if held, waiting := s.window.Held(); held != 0 || waiting != 0 {
			t.Errorf("the hydration done, the window holds %d bytes with %d waiting: a pull kept its take",
				held, waiting)
		}
	})
}
