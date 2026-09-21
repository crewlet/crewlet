package chat_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
)

// A UNIT'S ROOM IS THE ORG CHART'S, AND THE THREE GESTURES AGREE ABOUT IT.
//
// # Why all three, in one case
//
// Because they were not agreeing, and the disagreement was invisible. Leave
// refused a unit room — "leaving it would be undone by the next apply, with
// nothing to say so" — while Join refused only private rooms and SetMembers
// had no kind gate at all. So a seat could let itself into a team room and
// could not leave again, and anybody could rewrite the whole membership.
//
// None of it mattered while nothing created unit rooms. The moment the chart
// reconcile exists, every one of those writes is silently undone by the next
// apply, which is the exact failure Leave's refusal was written to prevent.
// Asserted together because the property is that they AGREE: a case per
// gesture passes while two of them drift apart.
func TestAUnitsRoomRefusesEveryMembershipGestureButTheCharts(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made, err := r.store.CreateChannel(t.Context(), person("jane"), chat.NewChannel{
		Name: "engineering", Kind: chat.KindUnit, Unit: "engineering",
		Members: members("bob"),
	})
	if err != nil {
		t.Fatalf("create the unit room: %v", err)
	}
	r.drain()

	for name, gesture := range map[string]func() error{
		"join": func() error {
			_, err := r.store.Join(t.Context(), person("jane"), made.Channel.ID)
			return err
		},
		"leave": func() error {
			_, err := r.store.Leave(t.Context(), person("bob"), made.Channel.ID)
			return err
		},
		"set the members": func() error {
			_, err := r.store.SetMembers(t.Context(), person("jane"),
				made.Channel.ID, members("jane"))
			return err
		},
	} {
		if err := gesture(); !errors.Is(err, chat.ErrForbidden) {
			t.Errorf("%s on a unit room answered %v, want %v — a write the next "+
				"apply undoes is worse than one that is refused, because "+
				"nothing reports it", name, err, chat.ErrForbidden)
		}
	}
}

// AND THE CHART'S OWN WRITER IS THE ONE THAT GETS THROUGH.
//
// The refusals above are only coherent if something can still maintain the
// room — otherwise a unit's membership could never change at all, which is a
// different bug with the same three passing assertions.
func TestTheChartsOwnWriterMaintainsAUnitsRoom(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made, err := r.store.CreateChannel(t.Context(), person("jane"), chat.NewChannel{
		Name: "engineering", Kind: chat.KindUnit, Unit: "engineering",
		Members: members("bob"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r.drain()

	engine := chat.Actor{Handle: "org-chart", Kind: chat.AuthorSystem}
	if _, err := r.store.SetUnitMembers(t.Context(), engine, made.Channel.ID,
		members("jane")); err != nil {
		t.Fatalf("the chart could not maintain its own room: %v", err)
	}
	r.drain()
}

// AND IT REFUSES A ROOM THAT IS NOT A UNIT'S.
//
// The whole point of the separate method is that it is the one writer allowed
// past the refusals above. Pointed at somebody else's room it would be a way
// around them, so it declines rather than widening its own grant.
func TestTheChartsWriterWillNotTakeAnotherRoom(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	made := r.room(person("jane"), "watercooler")

	engine := chat.Actor{Handle: "org-chart", Kind: chat.AuthorSystem}
	_, err := r.store.SetUnitMembers(t.Context(), engine, made.Channel.ID,
		members("jane"))
	if !errors.Is(err, chat.ErrForbidden) {
		t.Errorf("the chart's writer took a %s room: %v", chat.KindPublic, err)
	}
}
