package chatlet_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
)

// A WHOLE MEMBERSHIP STATED AGAINST A READ IS REFUSED WHEN THE ROOM MOVED.
//
// A screen that turns one agent's following on restates everybody else as it
// last read them. Before the expectation, a member who left the private room
// a moment earlier was written straight back in by that toggle, and so could
// read the room again although nobody had added them. The room's revision
// cannot guard it, since it moves with every message, so the store compares
// the membership itself inside the write's own snapshot.
//
// Mutation: drop the expectation check in SetMembers, compare handles alone
// (ignoring follow_all), or refuse a set the room already holds, and a step
// here goes red.
func TestAMembershipStatedAgainstAReadIsRefusedWhenTheRoomMoved(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "kim", "bob"))
	made, err := r.store.CreateChannel(t.Context(), person("jane"), chatlet.NewChannel{
		Name: "deal-room", Kind: chatlet.KindPrivate, Members: members("kim", "eng"),
	})
	if err != nil {
		t.Fatalf("create the private room: %v", err)
	}
	r.drain()
	room := made.Channel.ID
	read := members("eng", "jane", "kim")
	following := func(handle string) int {
		return r.count(`SELECT COUNT(*) FROM chat_members
			WHERE channel_id = ? AND handle = ? AND follow_all = 1`, room, handle)
	}
	inRoom := func(handle string) bool {
		return r.count(`SELECT COUNT(*) FROM chat_members
			WHERE channel_id = ? AND handle = ?`, room, handle) == 1
	}
	toggled := []chatlet.Member{{Handle: "eng", FollowAll: true}, {Handle: "jane"}, {Handle: "kim"}}

	// KIM LEAVES while jane's pane still shows her.
	if _, err := r.store.Leave(t.Context(), person("kim"), room); err != nil {
		t.Fatalf("kim leaves: %v", err)
	}
	r.drain()

	_, err = r.store.SetMembers(t.Context(), person("jane"), room, toggled, &read)
	if !errors.Is(err, chatlet.ErrConflict) {
		t.Fatalf("a toggle built from a read kim has since left answered %v, want %v",
			err, chatlet.ErrConflict)
	}
	r.drain()
	if inRoom("kim") {
		t.Fatal("the refused toggle put kim back in a private room she left")
	}
	if following("eng") != 0 {
		t.Fatal("the refused toggle changed eng's following")
	}

	// AGAINST THE ROOM AS IT STANDS, the same gesture lands.
	now := members("eng", "jane")
	if _, err := r.store.SetMembers(t.Context(), person("jane"), room,
		[]chatlet.Member{{Handle: "eng", FollowAll: true}, {Handle: "jane"}}, &now); err != nil {
		t.Fatalf("the toggle against the current membership: %v", err)
	}
	r.drain()
	if following("eng") != 1 || inRoom("kim") {
		t.Fatalf("eng follows %d, kim in the room %v: want eng following and kim out",
			following("eng"), inRoom("kim"))
	}

	// A SET THE ROOM ALREADY HOLDS is the no-op every membership write
	// answers, whatever was expected: a retry of a change that landed is told
	// it landed, not refused.
	if _, err := r.store.SetMembers(t.Context(), person("jane"), room,
		[]chatlet.Member{{Handle: "eng", FollowAll: true}, {Handle: "jane"}}, &now); err != nil {
		t.Fatalf("a retry of the landed toggle answered %v, want the no-op", err)
	}

	// FOLLOWING IS PART OF WHAT WAS READ: a change to it since is a moved room,
	// or one person's toggle would quietly undo another's.
	stale := members("eng", "jane")
	if _, err := r.store.SetMembers(t.Context(), person("jane"), room,
		[]chatlet.Member{{Handle: "eng"}, {Handle: "jane"}, {Handle: "bob"}}, &stale); !errors.Is(err,
		chatlet.ErrConflict) {
		t.Fatalf("a set built before eng's following changed answered %v, want %v",
			err, chatlet.ErrConflict)
	}

	// NO EXPECTATION is the caller that states the whole set from its own
	// knowledge, and it is taken as it always was.
	if _, err := r.store.SetMembers(t.Context(), person("jane"), room,
		members("eng", "jane", "bob"), nil); err != nil {
		t.Fatalf("a set with no expectation: %v", err)
	}
	r.drain()
	if !inRoom("bob") || following("eng") != 0 {
		t.Fatalf("bob in the room %v, eng follows %d: want the stated set", inRoom("bob"),
			following("eng"))
	}
}
