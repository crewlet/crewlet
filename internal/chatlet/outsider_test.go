package chatlet_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
)

// A PRIVATE ROOM IS NOT THERE TO SOMEBODY OUTSIDE IT, on every write that takes
// its membership rule, in exactly the words a room with no such id gets — the
// reader's answer, which the writer gave as "#deal-room is for its members only,
// and bob isn't one".
//
// That told anybody holding a private room's id what it was called and that it
// existed, and a direct conversation's id is derived from the two people in it,
// so it told anybody holding two handles whether those two talk — which the
// reader refuses to say. A membership change of a direct conversation said so
// first of all, refusing "a direct conversation is the people in it" before it
// asked whether the caller was one of them.
//
// Mutation: answer an outsider with a forbidden naming the room again, or
// refuse a direct conversation's membership change before reachable, and a row
// goes red.
func TestAPrivateRoomIsNotThereToSomebodyOutsideIt(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "kim", "bob"))
	private, err := r.store.CreateChannel(t.Context(), person("jane"), chatlet.NewChannel{
		Name: "deal-room", Kind: chatlet.KindPrivate, Members: members("kim")})
	if err != nil {
		t.Fatalf("create the private room: %v", err)
	}
	r.drain()
	said := r.say(person("jane"), private.Channel.ID, "the terms", "k-1")
	direct, _, err := r.store.OpenDirect(t.Context(), keyed(person("jane"), requested("k-2")),
		[]string{"kim"})
	if err != nil {
		t.Fatalf("open jane and kim's conversation: %v", err)
	}
	r.drain()
	// THE CONTROL: a conversation nobody opened, whose id bob derives the
	// same way.
	nobody := chatlet.DirectChannelID([]string{"jane", "lee"}).String()
	topic := "anything"

	bob := person("bob")
	gestures := map[string]func(room, message string) error{
		"a post": func(room, _ string) error {
			_, err := r.store.Post(t.Context(), keyed(bob, requested("k-3")), room,
				chatlet.NewMessage{Body: "hello?"})
			return err
		},
		"a join": func(room, _ string) error {
			_, err := r.store.Join(t.Context(), bob, room)
			return err
		},
		"a membership": func(room, _ string) error {
			_, err := r.store.SetMembers(t.Context(), bob, room, members("bob", "jane"))
			return err
		},
		"a topic": func(room, _ string) error {
			_, err := r.store.PatchChannel(t.Context(), bob, room,
				chatlet.ChannelPatch{Topic: &topic}, chatlet.ChannelAuthority{})
			return err
		},
		"a reaction": func(room, message string) error {
			_, err := r.store.React(t.Context(), bob, room, message, ":eyes:")
			return err
		},
	}
	for name, gesture := range gestures {
		absent := gesture(nobody, said.Message.ID)
		if !errors.Is(absent, chatlet.ErrNotFound) {
			t.Fatalf("%s into a room nobody opened answered %v: the control is not "+
				"the reader's answer", name, absent)
		}
		for room, id := range map[string]string{"jane's private room": private.Channel.ID,
			"jane and kim's conversation": direct.Channel.ID} {
			got := gesture(id, said.Message.ID)
			if !errors.Is(got, chatlet.ErrNotFound) || errors.Is(got, chatlet.ErrForbidden) {
				t.Errorf("%s by bob into %s answered %v, want the not-found a room "+
					"nobody opened gets", name, room, got)
				continue
			}
			if chatlet.Sentence(got) != chatlet.Sentence(absent) ||
				strings.Contains(chatlet.Sentence(got), "deal-room") {
				t.Errorf("%s by bob into %s reads %q, and into a room nobody opened %q",
					name, room, chatlet.Sentence(got), chatlet.Sentence(absent))
			}
		}
	}
}

// A SEAT ALREADY IN A ROOM THAT JOINS IT IS TOLD IT IS IN IT, whatever the
// room's kind: a member re-joining their own private room was refused "only
// someone already in it can add you", and a seat the org chart put in its
// unit's room that the chart decides who is in it. Somebody outside a unit's
// room is still told that.
//
// Mutation: ask the kinds before the membership in Join and both members' rows
// go red.
func TestJoiningARoomYouAreInChangesNothing(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "kim", "bob"))
	private, err := r.store.CreateChannel(t.Context(), person("jane"), chatlet.NewChannel{
		Name: "deal-room", Kind: chatlet.KindPrivate, Members: members("kim")})
	if err != nil {
		t.Fatalf("create the private room: %v", err)
	}
	unit := r.unitRoom("platform", "eng", "kim")
	r.drain()
	for name, c := range map[string]struct {
		who  string
		room string
	}{
		"a member of a private room": {"kim", private.Channel.ID},
		"its creator":                {"jane", private.Channel.ID},
		"a member of a unit's room":  {"kim", unit.Channel.ID},
	} {
		if _, err := r.store.Join(t.Context(), person(c.who), c.room); err != nil {
			t.Errorf("%s joining it again: %v, want nothing to change", name, err)
		}
	}
	_, err = r.store.Join(t.Context(), person("bob"), unit.Channel.ID)
	if !errors.Is(err, chatlet.ErrForbidden) ||
		!strings.Contains(chatlet.Sentence(err), "the org chart decides who's in it") {
		t.Errorf("somebody outside a unit's room joining it: %v", err)
	}
}
