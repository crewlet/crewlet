package chatlet_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
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
	made := r.unitRoom("engineering", "engineering", "bob")

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
				made.Channel.ID, members("jane"), nil)
			return err
		},
	} {
		if err := gesture(); !errors.Is(err, chatlet.ErrForbidden) {
			t.Errorf("%s on a unit room answered %v, want %v — a write the next "+
				"apply undoes is worse than one that is refused, because "+
				"nothing reports it", name, err, chatlet.ErrForbidden)
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
	made := r.unitRoom("engineering", "engineering", "bob")

	if _, _, err := r.store.SetUnitMembers(t.Context(), orgChart, made.Channel.ID,
		"engineering", members("jane"), chartActivation); err != nil {
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

	_, _, err := r.store.SetUnitMembers(t.Context(), orgChart, made.Channel.ID,
		"engineering", members("jane"), chartActivation)
	if !errors.Is(err, chatlet.ErrForbidden) {
		t.Errorf("the chart's writer took a %s room: %v", chatlet.KindPublic, err)
	}
}

// AND NOBODY BUT THE CHART MAKES ONE.
//
// A unit's room has one membership writer, and it maintains only the rooms
// of units that DECLARE a channel. A unit room a person, an operator or a
// seat made was one every membership gesture above refuses and no reconcile
// keeps — its founding membership frozen for the life of the deployment, under
// a kind that promises the chart maintains it. So the create is refused
// before anything is published, and the same name is still free for the
// chart to take.
func TestOnlyTheChartMakesAUnitsRoom(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	for _, actor := range []chatlet.Actor{person("jane"), viaToken("jane"),
		seat("agent-cto", "run-1")} {
		_, err := r.store.CreateChannel(t.Context(), actor, chatlet.NewChannel{
			Name: "engineering", Kind: chatlet.KindUnit, Unit: "engineering",
		})
		if !errors.Is(err, chatlet.ErrForbidden) {
			t.Errorf("an actor of kind %s made a unit's room: %v", actor.Kind, err)
		}
	}
	end, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if end != 0 {
		t.Fatalf("a refused unit room published %d record(s)", end)
	}
	if made := r.unitRoom("engineering", "engineering"); made.Channel.Kind != chatlet.KindUnit {
		t.Fatalf("the chart's own create made a %s room", made.Channel.Kind)
	}
}
