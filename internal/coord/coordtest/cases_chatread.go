package coordtest

import (
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// The per-person chat read state, certified against both backends.
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

	{"an absent mute list leaves the stored mutes alone", func(h *fleetHarness) {
		at := h.now()
		if _, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Muted: []string{"noisy"}, At: at,
		}); err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		got, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 1}, At: at.Add(time.Second),
		})
		if err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		if !slices.Contains(got.Muted, "noisy") {
			h.t.Fatalf("a cursor flush cleared the mute list (%v) — nil means "+
				"unchanged, and a tab that never touches mutes would otherwise "+
				"un-mute every room on every flush", got.Muted)
		}
		// AND AN EMPTY NON-NIL LIST IS HOW A MUTE IS LIFTED, which is the
		// distinction the pointer-free field cannot make without it.
		cleared, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Muted: []string{}, At: at.Add(2 * time.Second),
		})
		if err != nil {
			h.t.Fatalf("AdvanceChatRead clearing the mutes: %v", err)
		}
		if len(cleared.Muted) != 0 {
			h.t.Fatalf("an empty mute list left %v stored, so nothing can be "+
				"un-muted", cleared.Muted)
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

	{"a record is forgotten only by decision, and the listing finds it", func(h *fleetHarness) {
		if _, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 7}, At: h.now(),
		}); err != nil {
			h.t.Fatalf("AdvanceChatRead: %v", err)
		}
		readers, err := h.f.ChatReaders(h.ctx)
		if err != nil {
			h.t.Fatalf("ChatReaders: %v", err)
		}
		if !slices.Contains(readers, "founder") {
			h.t.Fatalf("ChatReaders answered %v and does not name the person "+
				"who just read a room — without this listing the reconcile "+
				"cannot find the records of people who left", readers)
		}
		gone, err := h.f.ForgetChatRead(h.ctx, "founder")
		if err != nil {
			h.t.Fatalf("ForgetChatRead: %v", err)
		}
		if !gone {
			h.t.Fatal("ForgetChatRead reported nothing to forget for a person " +
				"whose record was just written")
		}
		again, err := h.f.ForgetChatRead(h.ctx, "founder")
		if err != nil {
			h.t.Fatalf("ForgetChatRead twice: %v", err)
		}
		if again {
			h.t.Fatal("ForgetChatRead reported a second removal of one record")
		}
		state, err := h.f.ChatRead(h.ctx, "founder")
		if err != nil || len(state.Cursors) != 0 {
			h.t.Fatalf("a forgotten record still reads as %+v (err %v)", state, err)
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
	}},

	{"a record the caller mutates does not reach the next reader", func(h *fleetHarness) {
		if _, err := h.f.AdvanceChatRead(h.ctx, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{"eng": 5}, Muted: []string{"noisy"}, At: h.now(),
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
