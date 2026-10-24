package coordtest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// The per-person chat read state, certified against both backends. A case's
// handle is a seat's: the record is kept under the seat a person acts as.
//
// WHAT THESE CASES ARE REALLY DEFENDING is the merge rule, because every
// property a caller depends on comes from it rather than from the store: a
// cursor that only moves forward, a flush that says nothing about a field
// leaving that field alone, and a record two tabs can write at once without
// either losing the other's work. A backend that stored the delta verbatim
// would pass a naive round-trip test and fail every one of these.

var chatReadCases = []fleetCase{
	{"a person who has read nothing is a zero state and not an error", func(h *fleetHarness) {
		state, err := h.f.ChatRead(h.ctx, "founder")
		if err != nil {
			h.t.Fatalf("ChatRead on a person with no record: %v — the absence "+
				"of a record is the ordinary condition of somebody who has not "+
				"opened chat, and a caller cannot tell it from an unreachable "+
				"store if it arrives as an error", err)
		}
		if len(state.Cursors) != 0 || len(state.Muted) != 0 || !state.DNDUntil.IsZero() {
			h.t.Fatalf("ChatRead on a person with no record answered %+v", state)
		}
	}},

	{"a cursor never moves backward", func(h *fleetHarness) {
		at := h.now()
		if _, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 500}, At: at,
		}); err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		// A SECOND TAB, further back. This is the case the forward rule
		// exists for: without it the badge flickers as two tabs disagree
		// about which is further along, and the loser is whichever flush
		// the network delivered second.
		got, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 200}, At: at.Add(time.Second),
		})
		if err != nil {
			h.t.Fatalf("AdvanceChatRead with an older cursor: %v", err)
		}
		if got.Cursors["eng"] != 500 {
			h.t.Fatalf("a cursor at 500 was moved back to %d by a later flush "+
				"carrying 200", got.Cursors["eng"])
		}
	}},

	{"a flush that names one room leaves every other alone", func(h *fleetHarness) {
		at := h.now()
		if _, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 10, "design": 20}, At: at,
		}); err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		got, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 30}, At: at.Add(time.Second),
		})
		if err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		if got.Cursors["eng"] != 30 || got.Cursors["design"] != 20 {
			h.t.Fatalf("a flush naming one room produced %v — a delta is what "+
				"CHANGED, so a room it does not name must be untouched",
				got.Cursors)
		}
	}},

	{"a mute names one room and leaves every other alone", func(h *fleetHarness) {
		at := h.now()
		if _, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Mute: []string{"noisy", "busy"}, At: at,
		}); err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		got, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 1}, At: at.Add(time.Second),
		})
		if err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		if !slices.Equal(got.Muted, []string{"busy", "noisy"}) {
			h.t.Fatalf("a cursor flush left the mutes as %v, want [busy noisy] — "+
				"a flush that names no room to mute or unmute changes none, and a "+
				"tab that never touches mutes would otherwise un-mute every room on "+
				"every flush", got.Muted)
		}
		// A ROOM AT A TIME, IN BOTH DIRECTIONS: muting one room adds it to
		// the set the record holds rather than replacing the set, and
		// unmuting one takes only it out. A merge that replaced would
		// answer [quiet] and then nothing.
		got, err = h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Mute: []string{"quiet"}, At: at.Add(2 * time.Second),
		})
		if err != nil {
			h.t.Fatalf("AdvanceChatRead muting one more room: %v", err)
		}
		if !slices.Equal(got.Muted, []string{"busy", "noisy", "quiet"}) {
			h.t.Fatalf("muting one room left %v, want it beside the two already muted",
				got.Muted)
		}
		got, err = h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Unmute: []string{"noisy", "never-muted"}, At: at.Add(3 * time.Second),
		})
		if err != nil {
			h.t.Fatalf("AdvanceChatRead unmuting: %v", err)
		}
		if !slices.Equal(got.Muted, []string{"busy", "quiet"}) {
			h.t.Fatalf("unmuting noisy (and a room never muted) left %v, want "+
				"[busy quiet]", got.Muted)
		}
		// AND THE LAST ONE UNMUTED LEAVES NOTHING, in the shape a person who
		// never muted anything has.
		got, err = h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Unmute: []string{"busy", "quiet"}, At: at.Add(4 * time.Second),
		})
		if err != nil {
			h.t.Fatalf("AdvanceChatRead unmuting the rest: %v", err)
		}
		if got.Muted != nil {
			h.t.Fatalf("unmuting every muted room left %#v, want no list at all",
				got.Muted)
		}
	}},

	{"two tabs muting two rooms at once both land", func(h *fleetHarness) {
		// THE RACE A LIST REPLACE LOSES: each tab writes its own toggle,
		// and a write that carried the whole list would carry its own
		// tab's copy of it — so whichever landed second erased the
		// first's room. Per room, the store's own retry re-merges the
		// loser's ONE room into the winner's record.
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, room := range []string{"left-tab", "right-tab"} {
			wg.Go(func() {
				_, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
					Mute: []string{room}, At: h.now(),
				})
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				h.t.Fatalf("a concurrent mute failed: %v", err)
			}
		}
		if got := h.chatRead("founder"); !slices.Equal(got.Muted, []string{"left-tab", "right-tab"}) {
			h.t.Fatalf("two tabs muting two rooms at once left %v, want both", got.Muted)
		}
	}},

	{"the mute cap refuses growth and never a flush that does not grow", func(h *fleetHarness) {
		at := h.now()
		full := make([]string, coord.MaxReadCursors)
		for i := range full {
			full[i] = "room-" + itoa(i)
		}
		if _, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Mute: full, At: at,
		}); err != nil {
			h.t.Fatalf("muting exactly the cap: %v", err)
		}
		// ONE MORE IS REFUSED, against the set the flush would LEAVE — the
		// flush itself names one room.
		_, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Mute: []string{"one-more"}, At: at.Add(time.Second),
		})
		if !errors.Is(err, coord.ErrInvalidReadState) {
			h.t.Fatalf("a mute past the cap answered %v, want coord.ErrInvalidReadState", err)
		}
		// BUT AT THE CAP, everything that does not grow the set lands:
		// re-muting a muted room, a cursor, and the unmute that makes room.
		for name, delta := range map[string]coord.ChatReadDelta{
			"a room already muted": {Mute: []string{"room-0"}},
			"a cursor":             {Cursors: map[string]int64{"eng": 3}},
			"a swap":               {Mute: []string{"one-more"}, Unmute: []string{"room-1"}},
		} {
			delta.At = at.Add(2 * time.Second)
			if _, err := h.f.AdvanceChatRead(h.ctx, "founder", delta); err != nil {
				h.t.Fatalf("%s at the cap was refused: %v — a flush that does not "+
					"grow the set must never be refused for its size", name, err)
			}
		}
		got := h.chatRead("founder")
		if len(got.Muted) != coord.MaxReadCursors || !slices.Contains(got.Muted, "one-more") ||
			slices.Contains(got.Muted, "room-1") {
			h.t.Fatalf("after a swap at the cap the record holds %d mutes (one-more: %v, "+
				"room-1: %v), want the cap with one-more in and room-1 out",
				len(got.Muted), slices.Contains(got.Muted, "one-more"),
				slices.Contains(got.Muted, "room-1"))
		}
	}},

	{"do not disturb is an instant and clearing it is a gesture", func(h *fleetHarness) {
		at := h.now()
		until := at.Add(time.Hour)
		if _, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			DNDUntil: &until, At: at,
		}); err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		state, err := h.f.ChatRead(h.ctx, "founder")
		if err != nil {
			h.t.Fatalf("ChatRead: %v", err)
		}
		if !state.DNDUntil.Equal(until) {
			h.t.Fatalf("do-not-disturb stored as %v, want %v", state.DNDUntil, until)
		}
		var zero time.Time
		cleared, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			DNDUntil: &zero, At: at.Add(time.Second),
		})
		if err != nil {
			h.t.Fatalf("AdvanceChatRead clearing do-not-disturb: %v", err)
		}
		if !cleared.DNDUntil.IsZero() {
			h.t.Fatalf("do-not-disturb could not be cleared: %v — an instant "+
				"nobody can zero is a person silenced by a toggle they forgot",
				cleared.DNDUntil)
		}
	}},

	{"the record is bounded and the stalest cursor is what goes", func(h *fleetHarness) {
		// ONE OVER THE CAP, with the stalest room deliberately the lowest
		// position rather than the first written: the eviction rule is
		// about the log order, not about insertion order.
		cursors := make(map[string]int64, coord.MaxReadCursors+1)
		cursors["stalest"] = 1
		for i := range coord.MaxReadCursors {
			cursors["room-"+itoa(i)] = int64(1000 + i)
		}
		got, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Cursors: cursors, At: h.now(),
		})
		if err != nil {
			h.t.Fatalf("AdvanceChatRead at the cap: %v", err)
		}
		if len(got.Cursors) != coord.MaxReadCursors {
			h.t.Fatalf("%d cursors survived a flush of %d against a cap of %d",
				len(got.Cursors), len(cursors), coord.MaxReadCursors)
		}
		if _, ok := got.Cursors["stalest"]; ok {
			h.t.Fatalf("the cursor furthest back in the log survived the cap; " +
				"the room somebody has read least recently is the one whose " +
				"stale badge costs least")
		}
	}},

	{"a flush the merge refuses is refused as the caller's and changes nothing", func(h *fleetHarness) {
		at := h.now()
		h.advanceChatRead("founder", "eng", 7)
		// THE SENTINEL, not a sentence: the caller has to tell a flush
		// refused for what it carries — refused the same way every time —
		// from a store worth asking again, and a backend that wrapped the
		// merge's refusal as unavailable would send a person retrying a
		// flush that can never land.
		muted := make([]string, coord.MaxReadCursors+1)
		for i := range muted {
			muted[i] = "room-" + itoa(i)
		}
		for name, delta := range map[string]coord.ChatReadDelta{
			"a mute past the cap":           {Mute: muted, At: at},
			"a cursor naming no room":       {Cursors: map[string]int64{"": 9}, At: at},
			"a mute naming no room":         {Mute: []string{""}, At: at},
			"an unmute naming no room":      {Unmute: []string{""}, At: at},
			"a room both muted and unmuted": {Mute: []string{"eng"}, Unmute: []string{"eng"}, At: at},
		} {
			_, err := h.f.AdvanceChatRead(h.ctx, "founder", delta)
			if !errors.Is(err, coord.ErrInvalidReadState) {
				h.t.Fatalf("%s answered %v, want coord.ErrInvalidReadState", name, err)
			}
			if errors.Is(err, coord.ErrUnavailable) {
				h.t.Fatalf("%s was refused as an unreachable store: %v", name, err)
			}
			// AND AS THE MERGE WORDED IT: the person who sent the flush
			// reads [coord.ReadStateSentence] of this, and a backend that
			// wrapped the refusal in a prefix of its own would put that
			// prefix in front of them.
			if said := coord.ReadStateSentence(err); strings.HasPrefix(said, "coord") {
				h.t.Fatalf("%s reads %q to the person who sent it — the backend "+
					"wrapped the merge's refusal rather than returning it", name, said)
			}
		}
		if got := h.chatRead("founder"); got.Cursors["eng"] != 7 || len(got.Muted) != 0 {
			h.t.Fatalf("a refused flush changed the record to %+v", got)
		}
	}},

	{"a record is forgotten only once its absence is as old as the bound", func(h *fleetHarness) {
		at := h.now()
		h.advanceChatRead("founder", "eng", 7)
		if got := h.chatReaders(); !slices.Equal(got, []coord.ChatReader{{Handle: "founder"}}) {
			h.t.Fatalf("ChatReaders answered %v, want the person who just read a "+
				"room with no absence — without this listing the reconcile "+
				"cannot find the records of people who left", got)
		}
		// NEVER FOUND MISSING: no bound forgets it. A reconcile that
		// forgot a reader whose seat it had not seen leave would empty the
		// bucket of everybody it lists.
		h.forgetChatRead("founder", at.Add(time.Hour), false)

		if exists, err := h.f.SetChatReadAbsence(h.ctx, "founder", at); err != nil || !exists {
			h.t.Fatalf("SetChatReadAbsence = (%v, %v), want a stamp on the record just written", exists, err)
		}
		got := h.chatReaders()
		if len(got) != 1 || !got[0].AbsentSince.Equal(at) {
			h.t.Fatalf("ChatReaders answered %v, want founder absent since %v", got, at)
		}
		if got[0].AbsentSince.Location() != time.UTC {
			h.t.Errorf("the absence stamp is in %v, want UTC", got[0].AbsentSince.Location())
		}
		// STAMPED LATER THAN THE BOUND: inside the grace, so kept.
		h.forgetChatRead("founder", at.Add(-time.Second), false)
		if state := h.chatRead("founder"); state.Cursors["eng"] != 7 {
			h.t.Fatalf("a record inside its grace was touched: %+v", state)
		}
		// AT THE BOUND: forgotten, once.
		h.forgetChatRead("founder", at, true)
		h.forgetChatRead("founder", at, false)
		if state := h.chatRead("founder"); len(state.Cursors) != 0 || !state.AbsentSince.IsZero() {
			h.t.Fatalf("a forgotten record still reads as %+v", state)
		}
		if got := h.chatReaders(); len(got) != 0 {
			h.t.Fatalf("a forgotten record is still listed: %v", got)
		}
	}},

	{"an absence keeps its first sighting, and a return clears it", func(h *fleetHarness) {
		at := h.now()
		h.advanceChatRead("founder", "eng", 7)
		for _, since := range []time.Time{at, at.Add(time.Hour)} {
			if _, err := h.f.SetChatReadAbsence(h.ctx, "founder", since); err != nil {
				h.t.Fatalf("SetChatReadAbsence(%v): %v", since, err)
			}
		}
		// THE FIRST SIGHTING STANDS. A sweep restamping every tick would
		// restart the grace every tick, and forget nobody.
		if got := h.chatReaders(); len(got) != 1 || !got[0].AbsentSince.Equal(at) {
			h.t.Fatalf("after a second sighting the stamp is %v, want the first, %v", got, at)
		}
		if _, err := h.f.SetChatReadAbsence(h.ctx, "founder", time.Time{}); err != nil {
			h.t.Fatalf("clearing the absence: %v", err)
		}
		if got := h.chatReaders(); len(got) != 1 || !got[0].AbsentSince.IsZero() {
			h.t.Fatalf("a cleared absence lists as %v", got)
		}
		// AND A CLEARED STAMP CANNOT BE FORGOTTEN ON, which is the race a
		// peer sweep finding the seat back is in with one forgetting it.
		h.forgetChatRead("founder", at.Add(2*time.Hour), false)
		if state := h.chatRead("founder"); state.Cursors["eng"] != 7 {
			h.t.Fatalf("a seat found back in the company lost its record: %+v", state)
		}
	}},

	{"an absence is stamped only on a record that exists", func(h *fleetHarness) {
		exists, err := h.f.SetChatReadAbsence(h.ctx, "nobody", h.now())
		if err != nil {
			h.t.Fatalf("SetChatReadAbsence on a person with no record: %v", err)
		}
		if exists {
			h.t.Fatal("SetChatReadAbsence reported a record for a person who never read a room")
		}
		if got := h.chatReaders(); len(got) != 0 {
			h.t.Fatalf("stamping an absence created a record: %v", got)
		}
	}},

	{"a flush neither sets nor clears an absence", func(h *fleetHarness) {
		at := h.now()
		h.advanceChatRead("founder", "eng", 7)
		if _, err := h.f.SetChatReadAbsence(h.ctx, "founder", at); err != nil {
			h.t.Fatalf("SetChatReadAbsence: %v", err)
		}
		// A FLUSH FROM A NODE STILL ON AN OLDER REVISION is evidence about
		// that node, not about the company the fleet is pointed at; the
		// reconcile reads the fleet's, and only it moves the stamp.
		flushed := h.advanceChatRead("founder", "eng", 9)
		if !flushed.AbsentSince.Equal(at) {
			h.t.Fatalf("a flush answered absence %v, want the stamp it found, %v",
				flushed.AbsentSince, at)
		}
		if got := h.chatReaders(); len(got) != 1 || !got[0].AbsentSince.Equal(at) {
			h.t.Fatalf("after a flush the listing reads %v, want founder absent since %v", got, at)
		}
	}},

	{"sixteen flushes for one person at once all land", func(h *fleetHarness) {
		// THE PROPERTY THE FORWARD MERGE AND THE RETRY EXIST FOR, and the
		// one nothing else here exercises: every case above writes one
		// flush at a time, so a backend whose lost race was an error —
		// or was never retried at all — passed every one of them. Each
		// flush names a room of its own, so a flush that lost and was
		// dropped is a cursor missing from the record.
		//
		// SIXTEEN AT ONCE: enough that flushes lose rounds to each other
		// on every backend that has rounds to lose, while each round lost
		// is another flush's write that landed, so the burst always ends.
		// That one flush outlasts more lost rounds than any fixed count
		// would allow is the KV backend's own case, which makes it lose
		// twenty in a row deterministically rather than by chance.
		const flushes = 16
		var wg sync.WaitGroup
		type answer struct {
			state coord.ChatReadState
			err   error
		}
		answers := make(chan answer, flushes)
		for i := range flushes {
			wg.Go(func() {
				state, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
					Cursors: map[string]int64{"room-" + itoa(i): int64(i + 1)},
					At:      h.now(),
				})
				answers <- answer{state, err}
			})
		}
		wg.Wait()
		close(answers)
		revisions := map[uint64]bool{}
		var newest coord.ChatReadState
		for a := range answers {
			if a.err != nil {
				h.t.Fatalf("a concurrent flush failed: %v — a lost compare-and-set "+
					"is the forward merge's to retry, never the caller's to see", a.err)
			}
			revisions[a.state.Revision] = true
			if a.state.Revision > newest.Revision {
				newest = a.state
			}
		}
		state, err := h.f.ChatRead(h.ctx, "founder")
		if err != nil {
			h.t.Fatalf("ChatRead: %v", err)
		}
		for i := range flushes {
			if got := state.Cursors["room-"+itoa(i)]; got != int64(i+1) {
				h.t.Errorf("room-%d reads %d after %d concurrent flushes, want %d: "+
					"a flush was lost", i, got, flushes, i+1)
			}
		}
		// AND EACH ANSWER IS ORDERED BY ITS REVISION, which is what a screen
		// handed these answers out of order keeps the newest by: sixteen
		// writes, sixteen revisions, and the highest is the record as it
		// stands — every cursor in it.
		if len(revisions) != flushes || revisions[0] {
			h.t.Errorf("%d concurrent flushes answered %d distinct revisions (zero "+
				"among them: %v), want one each", flushes, len(revisions), revisions[0])
		}
		if newest.Revision != state.Revision || !maps.Equal(newest.Cursors, state.Cursors) {
			h.t.Errorf("the highest revision answered was %d with %v, but the record "+
				"reads %d with %v: the newest answer is not the record",
				newest.Revision, newest.Cursors, state.Revision, state.Cursors)
		}
	}},

	{"every state of a record is stamped with the revision that orders it", func(h *fleetHarness) {
		// THE ORDER A SCREEN KEEPS THE NEWEST STATE BY, and the one fact
		// the record's own instant cannot give: two flushes stamped by two
		// nodes' clocks are in whatever order those clocks say.
		if got := h.chatRead("founder"); got.Revision != 0 {
			h.t.Fatalf("a person with no record reads at revision %d, want zero", got.Revision)
		}
		first := h.advanceChatRead("founder", "eng", 7)
		if first.Revision == 0 {
			h.t.Fatal("a flush answered no revision: a screen handed it beside an " +
				"older state cannot tell which is newer")
		}
		if got := h.chatRead("founder"); got.Revision != first.Revision {
			h.t.Fatalf("the record reads at revision %d, but the flush that wrote it "+
				"answered %d", got.Revision, first.Revision)
		}
		second := h.advanceChatRead("founder", "eng", 9)
		if second.Revision <= first.Revision {
			h.t.Fatalf("a later flush answered revision %d after %d — the order a "+
				"screen keeps the newest by ran backwards", second.Revision, first.Revision)
		}
		// A STAMP MOVES THE RECORD TOO, so a state read after it is newer
		// than every flush before it.
		at := h.now()
		if _, err := h.f.SetChatReadAbsence(h.ctx, "founder", at); err != nil {
			h.t.Fatalf("SetChatReadAbsence: %v", err)
		}
		stamped := h.chatRead("founder")
		if stamped.Revision <= second.Revision {
			h.t.Fatalf("the record reads at revision %d after an absence stamp, "+
				"not past the flush before it at %d", stamped.Revision, second.Revision)
		}
		// AND A RECORD FORGOTTEN AND WRITTEN AGAIN MOVES PAST THE OLD ONE:
		// a screen still holding the old record's newest state must not keep
		// it over the new record's first.
		h.forgetChatRead("founder", at, true)
		if got := h.chatRead("founder"); got.Revision != 0 {
			h.t.Fatalf("a forgotten record reads at revision %d, want zero", got.Revision)
		}
		again := h.advanceChatRead("founder", "eng", 1)
		if again.Revision <= stamped.Revision {
			h.t.Fatalf("a record written again after it was forgotten answered "+
				"revision %d, not past the old record's %d", again.Revision, stamped.Revision)
		}
	}},

	{"two people's records are separate", func(h *fleetHarness) {
		at := h.now()
		if _, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 5}, At: at,
		}); err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		if _, err := h.f.AdvanceChatRead(h.ctx, "designer", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 9}, At: at,
		}); err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		founder, err := h.f.ChatRead(h.ctx, "founder")
		if err != nil {
			h.t.Fatalf("ChatRead: %v", err)
		}
		if founder.Cursors["eng"] != 5 {
			h.t.Fatalf("one person's flush moved another's cursor to %d",
				founder.Cursors["eng"])
		}
	}},

	{"a handle is required rather than defaulted", func(h *fleetHarness) {
		if _, err := h.f.AdvanceChatRead(h.ctx, "", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 1}, At: h.now(),
		}); err == nil {
			h.t.Fatal("a flush with no handle was accepted; the record it wrote " +
				"belongs to nobody and no listing can attribute it")
		}
		if _, err := h.f.ChatRead(h.ctx, ""); err == nil {
			h.t.Fatal("a read with no handle was answered")
		}
		if _, err := h.f.SetChatReadAbsence(h.ctx, "", h.now()); err == nil {
			h.t.Fatal("an absence with no handle was stamped")
		}
		if _, err := h.f.ForgetChatRead(h.ctx, "", h.now()); err == nil {
			h.t.Fatal("a forget with no handle was answered")
		}
	}},

	{"a record the caller mutates does not reach the next reader", func(h *fleetHarness) {
		if _, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 5}, Mute: []string{"noisy"}, At: h.now(),
		}); err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		first, err := h.f.ChatRead(h.ctx, "founder")
		if err != nil {
			h.t.Fatalf("ChatRead: %v", err)
		}
		// A real backend hands back a decoded record that shares nothing
		// with its store. A twin that handed out its live map would let
		// one caller's edit reach another's read — a property only the
		// twin has, which is exactly what a shared suite is for.
		maps.Copy(first.Cursors, map[string]int64{"eng": 999})
		if len(first.Muted) > 0 {
			first.Muted[0] = "tampered"
		}
		second, err := h.f.ChatRead(h.ctx, "founder")
		if err != nil {
			h.t.Fatalf("ChatRead: %v", err)
		}
		if second.Cursors["eng"] != 5 || (len(second.Muted) > 0 && second.Muted[0] != "noisy") {
			h.t.Fatalf("a caller's edit of its own copy reached the store: %+v", second)
		}
	}},
}

// The watch the live channel is derived from: every node serving sockets
// pushes a seat's record to that seat's tabs from it, so it must say which
// records already existed and where they end, then deliver every flush, stamp
// and forget as the change it is, each with the revision a screen orders two
// states by.
var chatReadWatchCases = []fleetCase{
	{"a read-state watch sees what exists, then every flush, stamp and forget", func(h *fleetHarness) {
		before := h.advanceChatRead("founder", "eng", 7)

		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		updates, err := h.f.WatchChatReads(ctx)
		if err != nil {
			h.t.Fatalf("WatchChatReads: %v", err)
		}
		u := h.nextChatReadUpdate(updates)
		if u.Handle != "founder" || u.State == nil || u.Current ||
			u.State.Cursors["eng"] != 7 || u.State.Revision != before.Revision {
			h.t.Fatalf("first update = %s, want founder's record as the flush "+
				"before the watch left it, at revision %d", describeChatRead(u), before.Revision)
		}
		if u = h.nextChatReadUpdate(updates); !u.Current {
			h.t.Fatalf("second update = %s, want the marker ending the records "+
				"that existed — without it a consumer cannot tell \"nobody has "+
				"read anything\" from \"nothing has arrived yet\"", describeChatRead(u))
		}

		// A FLUSH AFTER THE MARKER IS A CHANGE, carrying the whole record
		// and the revision the flush itself answered, so the push and the
		// flush's own answer order alike on the screen that sees both.
		flushed := h.advanceChatRead("designer", "design", 3)
		u = h.nextChatReadUpdate(updates)
		if u.Handle != "designer" || u.State == nil || u.State.Cursors["design"] != 3 ||
			u.State.Revision != flushed.Revision {
			h.t.Fatalf("update after a flush = %s, want designer's record at "+
				"revision %d", describeChatRead(u), flushed.Revision)
		}

		// A STAMP MOVES THE RECORD, and is delivered as its new state.
		at := h.now()
		if _, err := h.f.SetChatReadAbsence(h.ctx, "founder", at); err != nil {
			h.t.Fatalf("SetChatReadAbsence: %v", err)
		}
		u = h.nextChatReadUpdate(updates)
		if u.Handle != "founder" || u.State == nil || !u.State.AbsentSince.Equal(at) ||
			u.State.Cursors["eng"] != 7 || u.State.Revision <= before.Revision {
			h.t.Fatalf("update after an absence stamp = %s, want founder's record "+
				"absent since %v, past revision %d", describeChatRead(u), at, before.Revision)
		}

		// A FORGET IS DELIVERED, as a seat with no record: a consumer that
		// missed it would keep pushing a state the store no longer holds.
		h.forgetChatRead("founder", at, true)
		u = h.nextChatReadUpdate(updates)
		if u.Handle != "founder" || u.State != nil || u.Current {
			h.t.Fatalf("update after a forget = %s, want founder with no record",
				describeChatRead(u))
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
				h.t.Fatal("a cancelled read-state watch never closed its channel")
			}
		}
	}},

	{"a state one watcher keeps does not reach another", func(h *fleetHarness) {
		// Two nodes' hubs hold what their watches delivered; a real backend
		// decodes each delivery afresh, and a twin that handed both the
		// same map would let one consumer's edit reach the other's push —
		// a property only the twin has, which is what a shared suite is for.
		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		first, err := h.f.WatchChatReads(ctx)
		if err != nil {
			h.t.Fatalf("WatchChatReads: %v", err)
		}
		second, err := h.f.WatchChatReads(ctx)
		if err != nil {
			h.t.Fatalf("WatchChatReads: %v", err)
		}
		for _, updates := range []<-chan coord.ChatReadUpdate{first, second} {
			if u := h.nextChatReadUpdate(updates); !u.Current {
				h.t.Fatalf("an empty store's first update = %s, want the marker",
					describeChatRead(u))
			}
		}
		if _, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 5}, Mute: []string{"noisy"}, At: h.now(),
		}); err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		kept := h.nextChatReadUpdate(first)
		if kept.State == nil {
			h.t.Fatalf("the first watch delivered %s, want founder's record", describeChatRead(kept))
		}
		kept.State.Cursors["eng"] = 999
		kept.State.Muted[0] = "tampered"
		other := h.nextChatReadUpdate(second)
		if other.State == nil || other.State.Cursors["eng"] != 5 || other.State.Muted[0] != "noisy" {
			h.t.Fatalf("one watcher's edit of its own copy reached another's: %s",
				describeChatRead(other))
		}
		if got := h.chatRead("founder"); got.Cursors["eng"] != 5 || got.Muted[0] != "noisy" {
			h.t.Fatalf("a watcher's edit of its own copy reached the store: %+v", got)
		}
	}},
}

// nextChatReadUpdate reads one update, failing the case if none arrives.
func (h *fleetHarness) nextChatReadUpdate(updates <-chan coord.ChatReadUpdate) coord.ChatReadUpdate {
	h.t.Helper()
	select {
	case u, ok := <-updates:
		if !ok {
			h.t.Fatal("the read-state watch closed while the store was healthy")
		}
		return u
	case <-time.After(stallBudget):
		h.t.Fatal("the read-state watch delivered nothing")
	}
	return coord.ChatReadUpdate{}
}

// describeChatRead renders an update for a failure message, its state's
// pointer followed.
func describeChatRead(u coord.ChatReadUpdate) string {
	if u.State == nil {
		return fmt.Sprintf("{handle:%q current:%v state:none}", u.Handle, u.Current)
	}
	return fmt.Sprintf("{handle:%q current:%v state:%+v}", u.Handle, u.Current, *u.State)
}

func (h *fleetHarness) advanceChatRead(handle, room string, cursor int64) coord.ChatReadState {
	h.t.Helper()
	state, err := h.f.AdvanceChatRead(h.ctx, handle, coord.ChatReadDelta{
		Cursors: map[string]int64{room: cursor}, At: h.now(),
	})
	if err != nil {
		h.t.Fatalf("AdvanceChatRead(%s): %v", handle, err)
	}
	return state
}

func (h *fleetHarness) chatRead(handle string) coord.ChatReadState {
	h.t.Helper()
	state, err := h.f.ChatRead(h.ctx, handle)
	if err != nil {
		h.t.Fatalf("ChatRead(%s): %v", handle, err)
	}
	return state
}

func (h *fleetHarness) chatReaders() []coord.ChatReader {
	h.t.Helper()
	readers, err := h.f.ChatReaders(h.ctx)
	if err != nil {
		h.t.Fatalf("ChatReaders: %v", err)
	}
	return readers
}

func (h *fleetHarness) forgetChatRead(handle string, absentBy time.Time, want bool) {
	h.t.Helper()
	forgot, err := h.f.ForgetChatRead(h.ctx, handle, absentBy)
	if err != nil {
		h.t.Fatalf("ForgetChatRead(%s, %v): %v", handle, absentBy, err)
	}
	if forgot != want {
		h.t.Fatalf("ForgetChatRead(%s, %v) = %v, want %v", handle, absentBy, forgot, want)
	}
}

// itoa is strconv.Itoa without the import, so this file's only dependencies
// are the ones its assertions are about.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
