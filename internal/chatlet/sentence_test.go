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
	archived := true
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
			"message " + said.Message.ID, "only its author may edit it"},
		{"a room that takes nothing more", closed, chatlet.ErrArchived,
			room.Channel.ID + " was archived", "reopen it before posting"},
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
		"a conflict":              fmt.Errorf("%w: room kept moving", chatlet.ErrConflict),
		"anything else":           errors.New("disk I/O"),
	} {
		if got := chatlet.Sentence(err); got != err.Error() {
			t.Errorf("%s: Sentence = %q, want the whole message %q", name, got, err)
		}
	}
}
