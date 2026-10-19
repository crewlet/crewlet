package chatlet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// SOMEBODY ELSE'S WORDS COME DOWN ONLY BY A MODERATOR'S AUTHORITY, judged
// against the stored author.
//
// The store decides nothing about who is a moderator: the asking node's
// authority table does, and hands the verdict over as
// [chatlet.MessageAuthority.Moderate]. What the store decides is whether the
// verdict was NEEDED, from the author it holds, in the snapshot it writes from
// — so a caller cannot read the author, decide it was its own, and have the
// store take its word. Mutation: compare the actor with anything but the
// stored author, or ignore Moderate, and one of the three below goes wrong.
func TestSomebodyElsesWordsComeDownOnlyByAModeratorsAuthority(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	room := r.room(person("jane"), "launch").Channel.ID
	janes := r.say(person("jane"), room, "the key is hunter2", "k-1").Message.ID
	again := r.say(person("jane"), room, "and the other one is hunter3", "k-2").Message.ID

	// A MEMBER WHO IS NOT ITS AUTHOR, with no moderator's verdict.
	if _, err := r.store.Delete(t.Context(), keyed(person("bob"), requested("k-b")),
		room, janes, chatlet.MessageAuthority{}); !errors.Is(err, chatlet.ErrForbidden) {
		t.Fatalf("bob took jane's words down with no moderator's authority: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND deleted_at IS NULL`,
		janes); got != 1 {
		t.Fatal("a refused delete tombstoned the message anyway")
	}

	// ITS AUTHOR NEEDS NONE.
	if _, err := r.store.Delete(t.Context(), keyed(person("jane"), requested("k-j")),
		room, janes, chatlet.MessageAuthority{}); err != nil {
		t.Fatalf("jane could not take her own words back: %v", err)
	}
	// AND A MODERATOR'S VERDICT REACHES ANYBODY'S.
	if _, err := r.store.Delete(t.Context(), keyed(person("bob"), requested("k-m")),
		room, again, chatlet.MessageAuthority{Moderate: true}); err != nil {
		t.Fatalf("a moderator could not take jane's words down: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id IN (?, ?) AND `+
		`deleted_at IS NOT NULL`, janes, again); got != 2 {
		t.Fatalf("%d of the two admitted deletes tombstoned their message", got)
	}
}

// AN ARCHIVE IS ADMITTED BY THE ASKING NODE AND JUDGED AGAINST THE ROOM IT WAS
// DECIDED ON.
//
// Who may archive a room — its creator, its unit's lead, an admin — is the
// authority table's question, asked before the store is reached, and its
// answer arrives as [chatlet.ChannelAuthority]. The store holds two things the
// asking node cannot: that the patch was admitted at all, and that the room it
// writes still sits under the unit the lead decision was made for — a room
// re-homed between the ask and the write was decided about by the wrong lead.
// Mutation: drop either check in the patch's decide and its case below lands
// the archive.
func TestAnArchiveIsJudgedAgainstTheRoomItWasDecidedOn(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch").Channel.ID
	archived := true

	if _, err := r.store.PatchChannel(t.Context(), keyed(person("jane"), requested("k-1")),
		room, chatlet.ChannelPatch{Archived: &archived}, chatlet.ChannelAuthority{},
	); !errors.Is(err, chatlet.ErrForbidden) {
		t.Fatalf("an archive admitted for nobody answered %v, want ErrForbidden", err)
	}
	if _, err := r.store.PatchChannel(t.Context(), keyed(person("jane"), requested("k-2")),
		room, chatlet.ChannelPatch{Archived: &archived},
		chatlet.ChannelAuthority{Archive: true, ArchiveUnit: "engineering"},
	); !errors.Is(err, chatlet.ErrConflict) {
		t.Fatalf("an archive decided for a unit the room is not under answered "+
			"%v, want ErrConflict", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_channels WHERE id = ? AND `+
		`archived_at IS NULL`, room); got != 1 {
		t.Fatal("a refused archive closed the room anyway")
	}

	// A TOPIC NEEDS NO SUCH AUTHORITY: it is asked for only where the
	// patch archives or reopens.
	topic := "the release"
	if _, err := r.store.PatchChannel(t.Context(), keyed(person("jane"), requested("k-3")),
		room, chatlet.ChannelPatch{Topic: &topic}, chatlet.ChannelAuthority{}); err != nil {
		t.Fatalf("a topic change asked for an archive's authority: %v", err)
	}
	r.drain()
	if _, err := r.store.PatchChannel(t.Context(), keyed(person("jane"), requested("k-4")),
		room, chatlet.ChannelPatch{Archived: &archived}, chatlet.ChannelAuthority{Archive: true},
	); err != nil {
		t.Fatalf("an admitted archive: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_channels WHERE id = ? AND `+
		`archived_at IS NOT NULL`, room); got != 1 {
		t.Fatal("the admitted archive did not land")
	}
}

// A ROOM'S RETENTION IS ITS OWN ONCE-WRITE GESTURE, and never a seat's.
//
// A shorter horizon is a prune on a delay, so it is set by a person or a
// credential, on its own record, and moves exactly the one column: set, set
// again to the same value (which publishes nothing), and cleared back to the
// company's default. Mutation: admit a seat or the engine, write the column
// from anything but the record, or publish a record for an unchanged horizon,
// and a case below says so.
func TestARoomsRetentionIsItsOwnGestureAndNeverASeats(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch", "eng").Channel.ID
	days := 30

	for name, actor := range map[string]chatlet.Actor{
		"a seat":     seat("eng", "turn-1"),
		"the engine": {Handle: "retention", Kind: chatlet.AuthorSystem},
	} {
		if _, err := r.store.SetRetention(t.Context(), actor, room, &days); !errors.Is(err, chatlet.ErrForbidden) {
			t.Fatalf("%s set a room's retention: %v", name, err)
		}
	}
	if _, err := r.store.Prune(t.Context(), seat("eng", "turn-2"),
		statelog.NewOpID(time.Now(), ""), room, chatAt.Add(-time.Hour)); !errors.Is(err, chatlet.ErrForbidden) {
		t.Fatalf("a seat pruned a room: %v", err)
	}
	negative := -1
	if _, err := r.store.SetRetention(t.Context(), person("jane"), room, &negative); !errors.Is(err, chatlet.ErrInvalid) {
		t.Fatalf("a negative retention answered %v, want ErrInvalid", err)
	}

	if _, err := r.store.SetRetention(t.Context(), person("jane"), room, &days); err != nil {
		t.Fatalf("set the retention: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_channels WHERE id = ? AND retention_days = 30`,
		room); got != 1 {
		t.Fatal("the room's retention did not land")
	}
	end, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if _, err := r.store.SetRetention(t.Context(), person("jane"), room, &days); err != nil {
		t.Fatalf("set the same retention again: %v", err)
	}
	if again, err := r.log.End(t.Context()); err != nil || again != end {
		t.Fatalf("an unchanged horizon moved the log from %d to %d (%v)", end, again, err)
	}
	if _, err := r.store.SetRetention(t.Context(), person("jane"), room, nil); err != nil {
		t.Fatalf("clear the retention: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_channels WHERE id = ? AND retention_days IS NULL`,
		room); got != 1 {
		t.Fatal("clearing the retention left the override in place")
	}
}
