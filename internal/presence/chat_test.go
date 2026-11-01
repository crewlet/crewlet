package presence

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// A CHAT ROOM'S TYPING LAPSES AND AN ARTIFACT'S EDITING DOES NOT. A socket
// typing in a chat room is shown editing for as long as its tab says so again
// inside the lapse, and back to viewing once nothing has: a lid closed mid-word
// says nothing, and a typing line must not stand under it. An artifact's editor
// is editing for as long as its tab has said nothing else.
//
// Mutation: never arm the lapse, and the typing member stands; arm it in an
// artifact's room, and the editor is put back to viewing; skip the re-arm on a
// repeated frame, and a person still typing blinks off.
func TestAChatRoomsTypingLapsesAndAnArtifactsEditingDoesNot(t *testing.T) {
	t.Parallel()
	a := startNode(t, memory.NewBroker(), "node-a", newClock())
	room := ChatRoom(uuid.New())
	session, _, err := joined(a.hub, room, dana)
	if err != nil {
		t.Fatal(err)
	}
	typing := Focus{Thread: "m-1"}
	if err := a.hub.Update(room, session, Editing, typing); err != nil {
		t.Fatal(err)
	}
	a.seen.waitFor(t, "node-a", room, "dana:editing")

	// RE-ASSERTED INSIDE THE LAPSE, it stands past the lapse of the first,
	// and never blinks: a repeat fans nothing out, so any push meanwhile is
	// the line going down and coming back.
	before := a.seen.pushCount()
	//
	// NOT A DIVISOR OF THE LAPSE, so a lapse that was never re-armed runs out
	// between two repeats, where the fan-out shows it, rather than in the
	// same instant as one, where the repeat would hide it.
	for range 6 {
		time.Sleep(testLapse * 3 / 10)
		if err := a.hub.Update(room, session, Editing, typing); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.seen.members(room); !slices.Equal(got, []string{"dana:editing"}) {
		t.Fatalf("a person still typing is shown %v", got)
	}
	if pushes := a.seen.pushCount() - before; pushes != 0 {
		t.Fatalf("the room fanned out %d time(s) while the person kept typing: the "+
			"line blinked off under them", pushes)
	}
	// AND WITH NOTHING SAID, it lapses back to viewing.
	a.seen.waitFor(t, "node-a", room, "dana:viewing")

	editor, _, err := joined(a.hub, room1, erin)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.hub.Update(room1, editor, Editing, Focus{Collection: "rows"}); err != nil {
		t.Fatal(err)
	}
	a.seen.waitFor(t, "node-a", room1, "erin:editing")
	time.Sleep(2 * testLapse)
	if got := a.seen.members(room1); !slices.Equal(got, []string{"erin:editing"}) {
		t.Fatalf("an artifact's editor lapsed to %v", got)
	}
}

// A SEAT ANSWERING IN A CHAT ROOM IS SHOWN PER THREAD, WITH ITS LINE, UNTIL IT
// IS WITHDRAWN. A seat answering in two threads of one room — a turn parked
// behind its executor and the seat's next turn — is two members, a peer's
// sockets see each one's thread, status line and turn as the seat's node set
// them, and withdrawing one takes it down at once and leaves the other.
//
// Mutation: key a chat room's agents by seat alone, and the second thread
// replaces the first; drop Withdraw's delete, and the first line stands until
// it lingers out.
func TestASeatAnsweringInAChatRoomIsShownPerThreadUntilWithdrawn(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	api, seats := startNode(t, broker, "node-api", c), startNode(t, broker, "node-seats", c)
	room := ChatRoom(uuid.New())
	if _, _, err := joined(api.hub, room, dana); err != nil {
		t.Fatal(err)
	}
	waitListening(t, api.hub, room)

	seats.hub.Touch(room, Agent{Seat: "swe", Turn: "run-1", Activity: Editing,
		Focus: Focus{Thread: "m-1"}, Status: "is reading the thread"})
	seats.hub.Touch(room, Agent{Seat: "swe", Turn: "run-2", Activity: Editing,
		Focus: Focus{Thread: "m-2"}, Status: "is drafting a reply"})
	api.seen.waitFor(t, "node-api", room, "dana:viewing", "swe:editing", "swe:editing")
	lines := func() map[string]string {
		api.seen.mu.Lock()
		defer api.seen.mu.Unlock()
		out := map[string]string{}
		for _, m := range api.seen.rooms[room] {
			if m.Kind == iam.ActorAgent {
				out[m.Focus.Thread] = m.Turn + " " + m.Status
			}
		}
		return out
	}
	if got := lines(); got["m-1"] != "run-1 is reading the thread" ||
		got["m-2"] != "run-2 is drafting a reply" {
		t.Fatalf("the room shows the seat's threads as %v", got)
	}

	seats.hub.Withdraw(room, "swe", Focus{Thread: "m-1"})
	api.seen.waitFor(t, "node-api", room, "dana:viewing", "swe:editing")
	if got := lines(); len(got) != 1 || got["m-2"] == "" {
		t.Fatalf("withdrawing one thread left %v", got)
	}
}

// A CHAT ROOM'S FOCUS NAMES A THREAD AND NOTHING ELSE, an artifact's names no
// thread, and only an agent's member carries a status line, bounded: each kind
// reads the fields that mean something in it, and a frame naming any other is
// refused where it enters rather than shown to the room.
func TestEachKindOfRoomReadsItsOwnFocus(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind  RoomKind
		focus Focus
		ok    bool
	}{
		"a chat thread":                   {RoomChat, Focus{Thread: "m-1"}, true},
		"a chat room itself":              {RoomChat, Focus{}, true},
		"a chat focus naming a document":  {RoomChat, Focus{Thread: "m-1", Doc: "r1"}, false},
		"a chat thread past its bound":    {RoomChat, Focus{Thread: strings.Repeat("m", maxThreadBytes+1)}, false},
		"an artifact's cell":              {RoomArtifact, Focus{Collection: "rows", Doc: "r1"}, true},
		"an artifact focus naming thread": {RoomArtifact, Focus{Thread: "m-1"}, false},
	} {
		if err := CheckFocus(tc.kind, tc.focus); (err == nil) != tc.ok {
			t.Errorf("%s: CheckFocus = %v, want accepted %v", name, err, tc.ok)
		}
	}
	agent := Member{Session: "s-1", Seat: "swe", Kind: iam.ActorAgent, Activity: Editing,
		Status: "is thinking"}
	if err := checkMember(RoomChat, agent); err != nil {
		t.Errorf("an agent's status line was refused: %v", err)
	}
	person := agent
	person.Kind = iam.ActorHuman
	if checkMember(RoomChat, person) == nil {
		t.Error("a person's member naming a status line was accepted")
	}
	long := agent
	long.Status = strings.Repeat("s", maxStatusBytes+1)
	if checkMember(RoomChat, long) == nil {
		t.Error("a status line past its bound was accepted")
	}
}
