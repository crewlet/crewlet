package chatlet_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
)

// A REFUSAL READS AS AN INSTRUCTION, never as a package path.
//
// The chat tools tell a refused argument and a forbidden gesture in these
// words, to a model that has to change the call — so "chatlet: invalid: body:
// …" handed it a package path where its only question was what to change.
// Each refusal here is the store's own, made the way a caller meets it, so a
// sentence that drifts from the errors the store really builds is caught
// rather than one built by hand.
func TestARefusalReadsAsAnInstruction(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	room := r.room(person("jane"), "launch")
	said := r.say(person("jane"), room.Channel.ID, "we ship on friday", "k-1")

	_, forbidden := r.store.Edit(t.Context(), person("bob"), room.Channel.ID,
		said.Message.ID, chatlet.EditMessage{Body: "we ship on monday"})
	_, takenBack := r.store.Delete(t.Context(), person("bob"), room.Channel.ID,
		said.Message.ID, chatlet.MessageAuthority{})
	private, err := r.store.CreateChannel(t.Context(), person("jane"), chatlet.NewChannel{
		Name: "deal-room", Kind: chatlet.KindPrivate})
	if err != nil {
		t.Fatalf("create the private room: %v", err)
	}
	r.drain()
	_, shut := r.store.Join(t.Context(), person("bob"), private.Channel.ID)
	_, outside := r.store.Post(t.Context(), keyed(person("bob"), requested("k-4")),
		private.Channel.ID, chatlet.NewMessage{Body: "can I see this?"})
	_, nowhere := r.store.Post(t.Context(), keyed(person("jane"), requested("k-3")),
		"0193a7c2-0000-7000-8000-00000000c0de", chatlet.NewMessage{Body: "hello?"})
	archived := true
	_, notArchiver := r.store.PatchChannel(t.Context(), person("bob"), room.Channel.ID,
		chatlet.ChannelPatch{Archived: &archived}, chatlet.ChannelAuthority{})
	unit := r.unitRoom("platform", "eng", "jane")
	_, moved := r.store.PatchChannel(t.Context(), person("jane"), unit.Channel.ID,
		chatlet.ChannelPatch{Archived: &archived}, chatlet.ChannelAuthority{Archive: true,
			ArchiveUnit: "design"})
	if _, err := r.store.PatchChannel(t.Context(), person("jane"), room.Channel.ID,
		chatlet.ChannelPatch{Archived: &archived}, chatlet.ChannelAuthority{Archive: true}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	r.drain()
	_, closed := r.store.Post(t.Context(), keyed(person("jane"), requested("k-2")), room.Channel.ID,
		chatlet.NewMessage{Body: "anybody there?"})

	for _, c := range []struct {
		name string
		err  error
		mark error
		// opens is how the sentence begins, and says is a phrase
		// of the rule it states.
		opens, says string
	}{
		{"a field refused", chatlet.ChannelPatch{}.Validate(), chatlet.ErrInvalid,
			"`patch`: ", "a patch sets no field"},
		{"a gesture not this actor's", forbidden, chatlet.ErrForbidden,
			"this message was written by jane", "only its author can edit it"},
		{"a room that takes nothing more", closed, chatlet.ErrArchived,
			"this room is archived", "the lead of its team or of a team above it, or " +
				"an admin can reopen it"},
		{"somebody else's words taken back", takenBack, chatlet.ErrForbidden,
			"this message was written by jane", "Only its author can take it back"},
		{"a private room joined from outside it", shut, chatlet.ErrNotFound,
			"there's no such room", "no such room"},
		{"a private room posted into from outside it", outside, chatlet.ErrNotFound,
			"there's no such room", "no such room"},
		{"a room that is not there", nowhere, chatlet.ErrNotFound,
			"there's no such room", "no such room"},
		// WHO MAY ARCHIVE is said in full, the lead of a team above
		// included, and in the words a post into an archived room says it.
		{"an archive not this actor's", notArchiver, chatlet.ErrForbidden,
			"archiving #launch", "Only whoever created it, the lead of its team " +
				"or of a team above it, or an admin can do that"},
		// AND A ROOM THAT MOVED TEAMS is said without the unit's key,
		// which a write minted and nobody typed.
		{"a room that moved teams", moved, chatlet.ErrConflict,
			"#platform moved to another team", "Read it again"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !errors.Is(c.err, c.mark) {
				t.Fatalf("the store answered %v, not the refusal this case is about", c.err)
			}
			got := chatlet.Sentence(c.err)
			if !strings.HasPrefix(got, c.opens) || !strings.Contains(got, c.says) {
				t.Errorf("Sentence(%q) = %q, want it to open %q and say %q",
					c.err, got, c.opens, c.says)
			}
			if strings.Contains(got, "chatlet:") {
				t.Errorf("the sentence carries the package's prefix: %q", got)
			}
			// AND NO ID NOBODY CHOSE: a room is named as a person knows it,
			// and a message as the one the gesture was about.
			for _, id := range []string{room.Channel.ID, private.Channel.ID, said.Message.ID} {
				if strings.Contains(got, id) {
					t.Errorf("the sentence names %s by its id: %q", id, got)
				}
			}
			// AND NOT A PRIVATE ROOM'S NAME TO SOMEBODY OUTSIDE IT, who
			// held only its id: what the room is called is part of what
			// they may not read.
			if strings.Contains(got, "deal-room") {
				t.Errorf("a refusal to bob names jane's private room: %q", got)
			}
			// AND THE ERROR ITSELF IS UNCHANGED: a log line and every
			// errors.Is still read the Go error.
			if !strings.HasPrefix(c.err.Error(), "chatlet: ") {
				t.Errorf("the error's own message lost its prefix: %q", c.err)
			}
		})
	}

	// A REFUSAL A CALLER WRAPPED IS ITS WHOLE MESSAGE: the context it was
	// given is never dropped to tidy a prefix away, and an error this
	// package never wrote as a refusal is said as it stands.
	for name, err := range map[string]error{
		"a wrapped field refusal": fmt.Errorf("open #launch: %w", chatlet.ChannelPatch{}.Validate()),
		"a wrapped forbidden":     fmt.Errorf("edit: %w", forbidden),
		"anything else":           errors.New("disk I/O"),
	} {
		if got := chatlet.Sentence(err); got != err.Error() {
			t.Errorf("%s: Sentence = %q, want the whole message %q", name, got, err)
		}
	}
	// A NAME ANOTHER ROOM HOLDS is said by the name the creator typed: the
	// subject the create lost on is a digest of it, which the sentence
	// printed.
	_, taken := r.store.CreateChannel(t.Context(), person("bob"), chatlet.NewChannel{
		Name: "Launch", Kind: chatlet.KindPublic})
	if got, want := chatlet.Sentence(taken), "#launch is already another room's name, "+
		"so pick another one (or open #launch if you're in it)"; !errors.Is(taken, chatlet.ErrNameTaken) ||
		got != want {
		t.Errorf("a taken name: %v reads %q, want %q", taken, got, want)
	}
	// AND ANY OTHER ERROR THIS PACKAGE WROTE loses the "chatlet:" it opens with
	// and keeps every word after it: a tool printed it whole, so a model read
	// "chatlet:" in front of what it was told.
	open := fmt.Errorf("%w", chatlet.ErrRoomExists)
	if got, want := chatlet.Sentence(open), "that conversation is already open"; got != want {
		t.Errorf("a conversation already open: Sentence = %q, want %q", got, want)
	}
}
