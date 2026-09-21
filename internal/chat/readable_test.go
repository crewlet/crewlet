package chat_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The readable set a search is scoped by.
//
// WHAT THIS DEFENDS IS THE GAP BETWEEN THE RAIL AND [chat.Visible]. A rail is
// what somebody is IN; the visibility rule is wider, because a public room and
// a unit's room are readable by any seat of the company. A search driven off
// the rail would therefore answer nothing from the rooms a company does most
// of its talking in — and would do it silently, since an empty result reads as
// "nobody said that" rather than as "this search could not see the room".
//
// The other half is the one that must never widen: a private room somebody is
// not in may not appear here, because this slice IS the authorization the
// index is handed.

// A PUBLIC ROOM IS SEARCHABLE BY SOMEBODY WHO NEVER JOINED IT.
func TestTheReadableSetIsWiderThanTheRail(t *testing.T) {
	h := newReadHarness(t)
	// CAR IS IN NEITHER ROOM. The public one is still hers to read, and
	// the rail — which is membership — would hand back an empty list.
	h.apply(readCreateRoom("room-open", "open", chat.KindPublic, "ada"))
	h.apply(readCreateRoom("room-unit", "eng", chat.KindUnit, "ada"))
	h.apply(readCreateRoom("room-shut", "shut", chat.KindPrivate, "ada"))
	reader := h.reader()

	got, err := reader.Readable(t.Context(), "car", h.session())
	if err != nil {
		t.Fatalf("read the set car may search: %v", err)
	}
	for _, want := range []string{"room-open", "room-unit"} {
		if !slices.Contains(got, want) {
			t.Errorf("%q is missing from car's readable set %v — a public or "+
				"unit room is readable by any seat, joined or not, and a "+
				"search that cannot see it answers nothing rather than "+
				"saying it looked nowhere", want, got)
		}
	}
	if slices.Contains(got, "room-shut") {
		t.Errorf("car's readable set %v carries a private room she is not in "+
			"— this slice IS the authorization the index is handed, so a "+
			"room in it is a transcript disclosed", got)
	}

	// THE CONTROL: ada is in the private room, so she may search it.
	// Without this the assertion above is satisfied by a reader that
	// resolves nothing at all.
	ada, err := reader.Readable(t.Context(), "ada", h.session())
	if err != nil {
		t.Fatalf("read the set ada may search: %v", err)
	}
	if !slices.Contains(ada, "room-shut") {
		t.Errorf("ada's readable set %v leaves out the private room she is a "+
			"member of", ada)
	}
}

// AN ARCHIVED ROOM IS STILL SEARCHABLE.
//
// An archive closes a room to new messages, not to reading it, and a search is
// the read where a closed room is most of the point: what a team concluded
// last quarter is in the room they stopped using. The rail leaves archived
// rooms out because it is the list somebody works out of; this is not that
// list, and inheriting the rail's default here would quietly make the
// company's own history unfindable.
func TestAnArchivedRoomIsStillSearchable(t *testing.T) {
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-old", "old", chat.KindPublic, "ada"))
	archived := true
	h.apply(readRecord(chat.ChannelSubject("room-old"), chat.OpPatch,
		"op-archive-room-old", chat.ChannelPatch{
			V: chat.DocumentVersion, Archived: &archived,
		}, chat.ScopeSet{Subject: true}, "ada"))
	reader := h.reader()

	got, err := reader.Readable(t.Context(), "ada", h.session())
	if err != nil {
		t.Fatalf("read the set ada may search: %v", err)
	}
	if !slices.Contains(got, "room-old") {
		t.Errorf("an archived room is missing from the readable set %v — "+
			"archiving closes a room to new messages, not to reading it, "+
			"and a search is exactly where a closed room is wanted", got)
	}
}

// AN UNBOUND VIEWER IS REFUSED BY NAME, never answered with an empty set.
//
// The two are opposite facts and only one is safe: a viewer who may genuinely
// read nothing is a real state the caller renders as no hits, and a caller
// that LOST its viewer must not be handed the same answer.
func TestAReadableSetRefusesAnUnboundViewer(t *testing.T) {
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-open", "open", chat.KindPublic, "ada"))
	reader := h.reader()

	got, err := reader.Readable(t.Context(), "  ", h.session())
	if !errors.Is(err, chat.ErrForbidden) {
		t.Errorf("an empty viewer answered (%v, %v), want %v — an unbound "+
			"credential reads nothing in chat, and an empty slice would be "+
			"indistinguishable from a viewer with no rooms",
			got, err, chat.ErrForbidden)
	}
	if got != nil {
		t.Errorf("a refused readable set still carried %v", got)
	}
}

// AND A READ THAT NAMED NO FRESHNESS IS REFUSED.
//
// The level is the surface's to resolve — a seat reads linearizable, a
// dashboard poll stale — so defaulting it here would pick one of them for
// every caller, silently.
func TestAReadableSetRefusesAnAbsentFreshness(t *testing.T) {
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-open", "open", chat.KindPublic, "ada"))
	reader := h.reader()

	if _, err := reader.Readable(t.Context(), "ada",
		statelog.Freshness{}); err == nil {

		t.Error("a readable set with no freshness level was served — the " +
			"level is the surface's to resolve, and a default chosen here " +
			"would be chosen for all four surfaces at once")
	}
}
