package chatlet_test

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The names of the rooms a viewer may read, which is how a reader tells a room
// written in prose (`#e2e`) from a colour or an issue's number spelled the same
// way.
//
// WHAT THIS DEFENDS IS THE SET: every named room [chatlet.Visible] admits, the
// public and unit rooms a viewer never joined included, and nothing else. A
// name the answer leaves out is a room drawn as a colour; a private room it
// adds is a room disclosed to somebody outside it.

// roomNamed is one room of a names read as its case states it.
type roomNamed struct {
	id, name string
	kind     chatlet.Kind
	archived bool
}

func namedOf(names chatlet.RoomNames) []roomNamed {
	out := make([]roomNamed, 0, len(names.Rooms))
	for _, r := range names.Rooms {
		out = append(out, roomNamed{id: r.ID, name: r.Name, kind: r.Kind, archived: r.Archived})
	}
	return out
}

// EVERY NAMED ROOM A VIEWER MAY READ, AND NO OTHER.
//
// Car is in none of the open rooms and is in one private room. She may read
// the public and the unit room without being in them, the private room she is
// in, and the archived room, since an archive closes a room to new messages and
// not to reading it; she may not read the private room she is outside, and a
// direct conversation has no name for prose to write. The live rooms come in
// name order and the archived one after them. Mutation: drop the name filter
// and the direct conversation is answered with an empty name; widen the
// predicate past [chatlet.Visible] and the private room car is outside appears.
func TestTheRoomNamesAreEveryNamedRoomAViewerMayRead(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-launch", "launch", chatlet.KindPublic, "ada"))
	h.apply(readCreateRoom("room-eng", "eng", chatlet.KindUnit, "ada"))
	h.apply(readCreateRoom("room-board", "board", chatlet.KindPrivate, "bob"))
	h.apply(readCreateRoom("room-ops", "ops", chatlet.KindPrivate, "car", "ada"))
	direct := chatlet.DirectChannelID([]string{"ada", "car"}).String()
	h.apply(readRecord(chatlet.ChannelSubject(direct), chatlet.OpCreate, "op-open-direct",
		chatlet.ChannelCreate{
			V: chatlet.DocumentVersion, ChannelID: direct, Kind: chatlet.KindDM,
			Members:   []chatlet.Member{{Handle: "ada"}, {Handle: "car"}},
			CreatedBy: "ada", CreatedByKind: chatlet.AuthorHuman,
		}, chatlet.ScopeSet{Subject: true}, "ada"))
	h.apply(readCreateRoom("room-old", "e2e", chatlet.KindPublic, "ada"))
	archived := true
	h.apply(readRecord(chatlet.ChannelSubject("room-old"), chatlet.OpPatch,
		"op-archive-room-old", chatlet.ChannelPatch{
			V: chatlet.DocumentVersion, Archived: &archived,
		}, chatlet.ScopeSet{Subject: true}, "ada"))
	reader := h.reader()

	names, err := reader.RoomNames(t.Context(), "car", h.session())
	if err != nil {
		t.Fatalf("read the names of the rooms car may read: %v", err)
	}
	want := []roomNamed{
		{id: "room-eng", name: "eng", kind: chatlet.KindUnit},
		{id: "room-launch", name: "launch", kind: chatlet.KindPublic},
		{id: "room-ops", name: "ops", kind: chatlet.KindPrivate},
		{id: "room-old", name: "e2e", kind: chatlet.KindPublic, archived: true},
	}
	if got := namedOf(names); !slices.Equal(got, want) {
		t.Errorf("car may read the rooms %+v, and the names read answered %+v: every "+
			"named room she may read, joined or not and archived or not, and no "+
			"other, the live ones first", want, got)
	}
	if names.Truncated || names.Level != statelog.ReadSession || names.Position.Seq == 0 {
		t.Errorf("the names read answered truncated=%t at %s, %s, want the whole set "+
			"served at the level asked with the position it was read at",
			names.Truncated, names.Level, names.Position)
	}

	// THE CONTROL: bob is in the board, so it is his to name.
	bob, err := reader.RoomNames(t.Context(), "bob", h.session())
	if err != nil {
		t.Fatalf("read the names of the rooms bob may read: %v", err)
	}
	if !slices.ContainsFunc(bob.Rooms, func(r chatlet.RoomName) bool { return r.ID == "room-board" }) {
		t.Errorf("bob's names %+v leave out the private room he is in", bob.Rooms)
	}
}

// THE NAMES STOP AT THE CAP AND SAY SO, counting only the rooms a viewer may
// read.
//
// Every live room a company can hold fits, so a company at the live-room cap is
// answered whole; an archived room past it is left out and the answer says
// rooms were left out, so a reader knows a name it does not find may still be a
// room. A room this build cannot classify is no room the viewer may read, so it
// takes no place under the cap. Mutation: count the rows before [chatlet.Visible]
// decides, and the unclassified room marks the whole set truncated.
func TestTheRoomNamesStopAtTheCapAndSaySo(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	insert := func(id, kind string, archivedAt any) {
		t.Helper()
		if err := h.db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `
				INSERT INTO chat_channels
					(id, name, name_norm, kind, private, topic, purpose, unit,
					 retention_days, message_seq, created_at, created_by,
					 created_by_kind, archived_at, version, scoped_through, document)
				VALUES (?, ?, ?, ?, 0, '', '', '', NULL, 0, 1, 'ada',
				        'human', ?, 1, 0, X'')`,
				id, id, id, kind, archivedAt)
			return err
		}); err != nil {
			t.Fatalf("insert the room %s: %v", id, err)
		}
	}
	// A ROOM A NEWER PEER WROTE, sorting first.
	insert("aaa-newer", "broadcast", nil)
	for i := range chatlet.MaxRoomNames {
		insert(fmt.Sprintf("live-%04d", i), "public", nil)
	}
	h.apply(readCreateRoom("room-seed", "zz-seed", chatlet.KindPrivate, "bob"))
	reader := h.reader()

	whole, err := reader.RoomNames(t.Context(), "car", h.session())
	if err != nil {
		t.Fatalf("read car's room names: %v", err)
	}
	if len(whole.Rooms) != chatlet.MaxRoomNames || whole.Truncated {
		t.Fatalf("a company holding %d live rooms car may read was answered %d of "+
			"them, truncated=%t: every live room fits, and a room this build "+
			"cannot classify takes no place under the cap",
			chatlet.MaxRoomNames, len(whole.Rooms), whole.Truncated)
	}

	insert("zz-archived", "public", int64(2))
	cut, err := reader.RoomNames(t.Context(), "car", h.session())
	if err != nil {
		t.Fatalf("read car's room names past the cap: %v", err)
	}
	if len(cut.Rooms) != chatlet.MaxRoomNames || !cut.Truncated {
		t.Fatalf("past the cap the names read answered %d rooms, truncated=%t; "+
			"want %d and a truncated set, said rather than left to a count",
			len(cut.Rooms), cut.Truncated, chatlet.MaxRoomNames)
	}
	if slices.ContainsFunc(cut.Rooms, func(r chatlet.RoomName) bool { return r.ID == "zz-archived" }) {
		t.Error("the archived room was answered in place of a live one: the live " +
			"rooms come first, and every one of them fits")
	}
}

// A NAMES READ FOR NOBODY, OR AT NO LEVEL, IS REFUSED.
//
// An empty viewer is a credential acting as no seat, which reads nothing in
// chat, and an empty set would be indistinguishable from a viewer with no rooms;
// the level is the surface's to resolve.
func TestTheRoomNamesRefuseNobodyAndAnAbsentLevel(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.apply(readCreateRoom("room-launch", "launch", chatlet.KindPublic, "ada"))
	reader := h.reader()

	if got, err := reader.RoomNames(t.Context(), " ", h.session()); !errors.Is(
		err, chatlet.ErrForbidden) || got.Rooms != nil {

		t.Errorf("an empty viewer's names read answered (%+v, %v), want a refusal "+
			"carrying no room", got.Rooms, err)
	}
	if _, err := reader.RoomNames(t.Context(), "ada", statelog.Freshness{}); err == nil {
		t.Error("a names read naming no read level was served")
	}
}
