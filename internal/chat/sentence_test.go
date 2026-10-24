package chat_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
)

// A REFUSAL A PERSON IS SHOWN READS AS AN INSTRUCTION, never as a package
// path.
//
// The operator's act transport prints a refused argument and a forbidden
// gesture as the tool wrote them, and every chat write a person makes from the
// dashboard goes through it — so "chat: invalid: body: …" reached a person
// whose only question was what to change. Each refusal here is the store's
// own, made the way a caller meets it, so a sentence that drifts from the
// errors the store really builds is caught rather than one built by hand.
func TestARefusalReadsAsAPersonsSentence(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	room := r.room(person("jane"), "launch")
	said := r.say(person("jane"), room.Channel.ID, "we ship on friday", "k-1")

	_, forbidden := r.store.Edit(t.Context(), person("bob"), room.Channel.ID,
		said.Message.ID, chat.EditMessage{Body: "we ship on monday"})
	_, takenBack := r.store.Delete(t.Context(), person("bob"), room.Channel.ID,
		said.Message.ID, chat.MessageAuthority{})
	private, err := r.store.CreateChannel(t.Context(), person("jane"), chat.NewChannel{
		Name: "deal-room", Kind: chat.KindPrivate})
	if err != nil {
		t.Fatalf("create the private room: %v", err)
	}
	r.drain()
	_, shut := r.store.Join(t.Context(), person("bob"), private.Channel.ID)
	_, outside := r.store.Post(t.Context(), keyed(person("bob"), requested("k-4")),
		private.Channel.ID, chat.NewMessage{Body: "can I see this?"})
	_, nowhere := r.store.Post(t.Context(), keyed(person("jane"), requested("k-3")),
		"0193a7c2-0000-7000-8000-00000000c0de", chat.NewMessage{Body: "hello?"})
	archived := true
	_, notArchiver := r.store.PatchChannel(t.Context(), person("bob"), room.Channel.ID,
		chat.ChannelPatch{Archived: &archived}, chat.ChannelAuthority{})
	unit := r.unitRoom("platform", "eng", "jane")
	_, moved := r.store.PatchChannel(t.Context(), person("jane"), unit.Channel.ID,
		chat.ChannelPatch{Archived: &archived}, chat.ChannelAuthority{Archive: true,
			ArchiveUnit: "design"})
	if _, err := r.store.PatchChannel(t.Context(), person("jane"), room.Channel.ID,
		chat.ChannelPatch{Archived: &archived}, chat.ChannelAuthority{Archive: true}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	r.drain()
	_, closed := r.store.Post(t.Context(), keyed(person("jane"), requested("k-2")), room.Channel.ID,
		chat.NewMessage{Body: "anybody there?"})

	for _, c := range []struct {
		name string
		err  error
		mark error
		// opens is how the person's sentence begins, and says is a phrase
		// of the rule it states.
		opens, says string
	}{
		{"a field refused", chat.ChannelPatch{}.Validate(), chat.ErrInvalid,
			"`patch`: ", "a patch sets no field"},
		{"a gesture not this actor's", forbidden, chat.ErrForbidden,
			"this message was written by jane", "only its author can edit it"},
		{"a room that takes nothing more", closed, chat.ErrArchived,
			"this room is archived", "the lead of its team or of a team above it, or " +
				"an admin can reopen it"},
		{"somebody else's words taken back", takenBack, chat.ErrForbidden,
			"this message was written by jane", "Only its author can take it back"},
		{"a private room joined from outside it", shut, chat.ErrNotFound,
			"there's no such room", "no such room"},
		{"a private room posted into from outside it", outside, chat.ErrNotFound,
			"there's no such room", "no such room"},
		{"a room that is not there", nowhere, chat.ErrNotFound,
			"there's no such room", "no such room"},
		// WHO MAY ARCHIVE is said in full, the lead of a team above
		// included, and in the words a post into an archived room says it.
		{"an archive not this actor's", notArchiver, chat.ErrForbidden,
			"archiving #launch", "Only whoever created it, the lead of its team " +
				"or of a team above it, or an admin can do that"},
		// AND A ROOM THAT MOVED TEAMS is said without the unit's key,
		// which a write minted and nobody typed.
		{"a room that moved teams", moved, chat.ErrConflict,
			"#platform moved to another team", "Read it again"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !errors.Is(c.err, c.mark) {
				t.Fatalf("the store answered %v, not the refusal this case is about", c.err)
			}
			got := chat.Sentence(c.err)
			if !strings.HasPrefix(got, c.opens) || !strings.Contains(got, c.says) {
				t.Errorf("Sentence(%q) = %q, want it to open %q and say %q",
					c.err, got, c.opens, c.says)
			}
			if strings.Contains(got, "chat:") {
				t.Errorf("the person's sentence carries the package's prefix: %q", got)
			}
			// AND NO ID NOBODY CHOSE: a room is named as a person knows it,
			// and a message as the one the gesture was about.
			for _, id := range []string{room.Channel.ID, private.Channel.ID, said.Message.ID} {
				if strings.Contains(got, id) {
					t.Errorf("the person's sentence names %s by its id: %q", id, got)
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
			if !strings.HasPrefix(c.err.Error(), "chat: ") {
				t.Errorf("the error's own message lost its prefix: %q", c.err)
			}
		})
	}

	// A REFUSAL A CALLER WRAPPED IS ITS WHOLE MESSAGE: the context it was
	// given is never dropped to tidy a prefix away, and an error this
	// package never wrote as a refusal is said as it stands.
	for name, err := range map[string]error{
		"a wrapped field refusal": fmt.Errorf("open #launch: %w", chat.ChannelPatch{}.Validate()),
		"a wrapped forbidden":     fmt.Errorf("edit: %w", forbidden),
		"anything else":           errors.New("disk I/O"),
	} {
		if got := chat.Sentence(err); got != err.Error() {
			t.Errorf("%s: Sentence = %q, want the whole message %q", name, got, err)
		}
	}
	// A NAME ANOTHER ROOM HOLDS is said by the name the creator typed: the
	// subject the create lost on is a digest of it, which the sentence
	// printed.
	_, taken := r.store.CreateChannel(t.Context(), person("bob"), chat.NewChannel{
		Name: "Launch", Kind: chat.KindPublic})
	if got, want := chat.Sentence(taken), "#launch is already another room's name, "+
		"so pick another one (or open #launch if you're in it)"; !errors.Is(taken, chat.ErrNameTaken) ||
		got != want {
		t.Errorf("a taken name: %v reads %q, want %q", taken, got, want)
	}
	// AND ANY OTHER ERROR THIS PACKAGE WROTE loses the "chat:" it opens with
	// and keeps every word after it: a tool printed it whole, so a model read
	// "chat:" in front of what it was told.
	open := fmt.Errorf("%w", chat.ErrRoomExists)
	if got, want := chat.Sentence(open), "that conversation is already open"; got != want {
		t.Errorf("a conversation already open: Sentence = %q, want %q", got, want)
	}
}
