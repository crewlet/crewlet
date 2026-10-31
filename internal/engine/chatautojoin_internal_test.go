package engine

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
)

// autoJoinDesk is the chat store as the auto-join pass writes through it,
// recording every room it was asked about and with which seats.
type autoJoinDesk struct {
	rooms   []string
	listErr error
	failOn  string
	asked   []string
	seats   [][]string
	actors  []chatlet.Actor
}

func (d *autoJoinDesk) AutoJoinRooms(context.Context) ([]string, error) {
	return d.rooms, d.listErr
}

func (d *autoJoinDesk) AutoJoin(_ context.Context, actor chatlet.Actor, channelID string,
	seats []string) (chatlet.Written, []string, error) {

	d.asked = append(d.asked, channelID)
	d.seats = append(d.seats, seats)
	d.actors = append(d.actors, actor)
	if channelID == d.failOn {
		return chatlet.Written{}, nil, errors.New("the broker is away")
	}
	return chatlet.Written{}, []string{"qa"}, nil
}

// EVERY AUTO-JOIN ROOM IS HANDED THE CHART'S WHOLE SEAT LIST, AS THE ENGINE:
// the room decides who of them is new, so the pass hands over everybody, and
// one room it could not bring into line costs none of the others theirs.
func TestEveryAutoJoinRoomIsHandedTheChartAsTheEngine(t *testing.T) {
	t.Parallel()
	desk := &autoJoinDesk{rooms: []string{"general", "random", "launch"}, failOn: "random"}
	seats := []string{"ceo", "jane", "qa"}
	applyAutoJoin(t.Context(), desk, seats)
	if !slices.Equal(desk.asked, desk.rooms) {
		t.Fatalf("the pass asked about %v, want every auto-join room %v: a "+
			"room that failed must not stop the rooms after it", desk.asked, desk.rooms)
	}
	for i, got := range desk.seats {
		if !slices.Equal(got, seats) {
			t.Errorf("room %s was handed %v, want the chart's whole list", desk.asked[i], got)
		}
		if a := desk.actors[i]; a.Kind != chatlet.AuthorSystem || a.Handle != chatAutoJoinActor {
			t.Errorf("room %s was written as %+v, want the engine narrating", desk.asked[i], a)
		}
	}

	quiet := &autoJoinDesk{listErr: errors.New("the estate is away")}
	applyAutoJoin(t.Context(), quiet, seats)
	if len(quiet.asked) != 0 {
		t.Errorf("a pass that could not list the rooms wrote into %v", quiet.asked)
	}
}
