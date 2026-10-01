package coordtest

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- seat pauses -------------------------------------------------------- //

func (h *fleetHarness) pauseOf(handle string) (coord.SeatPause, bool) {
	h.t.Helper()
	p, found, err := h.f.SeatPause(h.ctx, handle)
	if err != nil {
		h.t.Fatalf("SeatPause(%s): %v", handle, err)
	}
	return p, found
}

func (h *fleetHarness) pause(handle, by string, stop bool) (coord.SeatPause, bool) {
	h.t.Helper()
	p, created, err := h.f.CreateSeatPause(h.ctx, coord.SeatPause{
		Handle: handle, By: by, Reason: "looping on the same ticket",
		StopRunning: stop, At: h.now(),
	})
	if err != nil {
		h.t.Fatalf("CreateSeatPause(%s): %v", handle, err)
	}
	return p, created
}

// nextPauseUpdate reads one update, failing the case if none arrives.
func (h *fleetHarness) nextPauseUpdate(updates <-chan coord.SeatPauseUpdate) coord.SeatPauseUpdate {
	h.t.Helper()
	select {
	case u, ok := <-updates:
		if !ok {
			h.t.Fatal("the pause watch closed while the store was healthy")
		}
		return u
	case <-time.After(stallBudget):
		h.t.Fatal("the pause watch delivered nothing")
	}
	return coord.SeatPauseUpdate{}
}

var seatPauseCases = []fleetCase{{
	// THE LOST UPDATE this record exists to prevent: two people act on one
	// seat at once, and exactly one of them may win each change. A pause is
	// a create, so a second pause is told the seat was already paused and
	// changes nothing; an amendment and a resume are conditioned on the
	// version their writer read, so the one that read an older record loses
	// rather than overwriting what somebody else just did.
	name: "a seat pause is compare and set",
	fn: func(h *fleetHarness) {
		first, created := h.pause("swe", "jane-token", false)
		if !created {
			h.t.Fatal("the first pause of a free seat lost")
		}
		if first.Version == 0 {
			h.t.Fatal("the pause carries no version, so no resume can be conditioned on it")
		}
		if _, again := h.pause("swe", "omar-token", true); again {
			h.t.Fatal("a second pause of a paused seat reported itself as new: " +
				"two people's pauses would both announce, and the second overwrote the first")
		}
		read, found := h.pauseOf("swe")
		if !found || read.By != "jane-token" || read.StopRunning {
			h.t.Fatalf("pause = %+v (found %v), want jane's, untouched by the second", read, found)
		}
		if read.Version != first.Version || !read.At.Equal(h.now()) || read.Handle != "swe" {
			h.t.Errorf("read back %+v, want what the create stored (%+v)", read, first)
		}

		// An amendment at the version read wins and moves the version.
		stop := read
		stop.StopRunning = true
		amended, ok, err := h.f.UpdateSeatPause(h.ctx, stop)
		if err != nil || !ok {
			h.t.Fatalf("UpdateSeatPause at the version just read = (%v, %v)", ok, err)
		}
		if amended.Version == first.Version {
			h.t.Fatal("the version did not move, so a resume that read the old pause would win")
		}
		// A resume that read the FIRST version lost to that amendment.
		if gone, derr := h.f.DeleteSeatPause(h.ctx, "swe", first.Version); derr != nil || gone {
			h.t.Fatalf("a resume at a superseded version = (%v, %v), want a lost race", gone, derr)
		}
		if _, found := h.pauseOf("swe"); !found {
			h.t.Fatal("a stale resume lifted a pause somebody had just amended")
		}
		// No version is never a win either way.
		if gone, _ := h.f.DeleteSeatPause(h.ctx, "swe", 0); gone {
			h.t.Fatal("a resume carrying no version lifted the pause")
		}
		if _, ok, _ := h.f.UpdateSeatPause(h.ctx, coord.SeatPause{
			Handle: "qa", By: "jane-token", At: h.now(),
		}); ok {
			h.t.Fatal("an update carrying no version created a pause")
		}
		if gone, derr := h.f.DeleteSeatPause(h.ctx, "swe", amended.Version); derr != nil || !gone {
			h.t.Fatalf("a resume at the current version = (%v, %v), want it lifted", gone, derr)
		}
		if _, found := h.pauseOf("swe"); found {
			h.t.Fatal("a resumed seat still reads as paused")
		}
		// And a resumed seat can be paused again — a purge marker must not
		// make the next create lose.
		if _, created := h.pause("swe", "omar-token", false); !created {
			h.t.Fatal("a seat resumed once could not be paused again")
		}
		all, err := h.f.ListSeatPauses(h.ctx)
		if err != nil || len(all) != 1 || all[0].By != "omar-token" {
			h.t.Fatalf("ListSeatPauses = %+v, %v; want omar's one pause", all, err)
		}
	},
}, {
	// A pause missing who took it is one nobody can be asked about, and one
	// missing its seat is filed under nothing.
	name: "a seat pause names its seat, who took it and when",
	fn: func(h *fleetHarness) {
		for _, bad := range []coord.SeatPause{
			{By: "jane-token", At: h.now()},
			{Handle: "swe", At: h.now()},
			{Handle: "swe", By: "jane-token"},
		} {
			if _, created, err := h.f.CreateSeatPause(h.ctx, bad); err == nil || created {
				h.t.Errorf("CreateSeatPause(%+v) = (%v, %v), want a refusal", bad, created, err)
			}
		}
		if all, _ := h.f.ListSeatPauses(h.ctx); len(all) != 0 {
			h.t.Errorf("a refused pause left %+v behind", all)
		}
	},
}, {
	// What every node's cache is built from. The watch must say which records
	// already existed and where they end — "no seat is paused" is otherwise
	// indistinguishable from "nothing has arrived yet" — and then deliver a
	// pause and its lift as the two changes they are, a lift included:
	// missing the purge is a seat every node keeps paused after its resume.
	name: "a watch sees a pause and its clear",
	fn: func(h *fleetHarness) {
		h.pause("ops", "jane-token", false)

		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		updates, err := h.f.WatchSeatPauses(ctx)
		if err != nil {
			h.t.Fatalf("WatchSeatPauses: %v", err)
		}
		u := h.nextPauseUpdate(updates)
		if u.Handle != "ops" || u.Pause == nil || u.Pause.By != "jane-token" || u.Current {
			h.t.Fatalf("first update = %+v, want the pause that already existed", u)
		}
		if u = h.nextPauseUpdate(updates); !u.Current {
			h.t.Fatalf("second update = %+v, want the marker ending the current records", u)
		}

		paused, _ := h.pause("swe", "omar-token", true)
		u = h.nextPauseUpdate(updates)
		if u.Handle != "swe" || u.Pause == nil || !u.Pause.StopRunning || u.Pause.Version != paused.Version {
			h.t.Fatalf("update after a pause = %+v, want swe's pause as stored", u)
		}
		if gone, err := h.f.DeleteSeatPause(h.ctx, "swe", paused.Version); err != nil || !gone {
			h.t.Fatalf("DeleteSeatPause = (%v, %v)", gone, err)
		}
		u = h.nextPauseUpdate(updates)
		if u.Handle != "swe" || u.Pause != nil || u.Current {
			h.t.Fatalf("update after a resume = %+v, want swe's pause cleared", u)
		}

		// Ending the watch closes the channel: a consumer can tell a
		// watch that stopped from one that is merely quiet.
		cancel()
		deadline := time.After(stallBudget)
		for {
			select {
			case _, ok := <-updates:
				if !ok {
					return
				}
			case <-deadline:
				h.t.Fatal("a cancelled watch never closed its channel")
			}
		}
	},
}}
