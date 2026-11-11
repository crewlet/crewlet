package engine

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// A PICKUP IS WORKED FOR WHOM ITS ITEM IS FOR (ADR-0109, ADR-0112): the turn
// reads it off the pickup as an answering turn reads it off the ask, so the
// item a person assigned is held to that person's apps however it reaches the
// seat; a schedule's fire and a pickup of the seat's own work carry nobody.
//
// Mutation: read only the colleague hand-ons, and the pickup turn works with
// the seat's every app.
func TestAPickupIsWorkedForWhomItsItemIsFor(t *testing.T) {
	t.Parallel()
	pickup := func(w *types.WorkingFor) []*events.Event {
		return []*events.Event{events.New(types.WorkPickup{Agent: "a1", RoleName: "Engineer",
			Item: "t-1", Key: "ENG-1", Status: "todo", WorkingFor: w}, events.TraceContext{})}
	}
	person := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	token := &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
		Apps: []iam.Grant{iam.GrantWorklet}}
	for name, tc := range map[string]struct {
		evs  []*events.Event
		want *types.WorkingFor
	}{
		"an item a person assigned":     {pickup(person), person},
		"an item a credential assigned": {pickup(token), token},
		"the seat's own item":           {pickup(nil), nil},
		"a schedule's fire": {[]*events.Event{events.New(types.TaskAssigned{
			Schedule: "standup", Description: "Post the standup."}, events.TraceContext{})}, nil},
	} {
		if got := workingForIn(tc.evs); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: the turn works for %+v, want %+v", name, got, tc.want)
		}
	}
}

// A PICKUP IS A WAKE OF ITS OWN TYPE (ADR-0112): the ledger records it, its
// turn answers through a tool as an assignment's does, its brief is the ask,
// and it never merges with a schedule's fires.
func TestAPickupIsAWakeOfItsOwnType(t *testing.T) {
	t.Parallel()
	ev := events.New(types.WorkPickup{Agent: "a1", RoleName: "Engineer", Item: "t-1",
		Key: "ENG-1", Status: "todo", Why: types.PickupQueue, Ready: 1}, events.TraceContext{})
	if ev.Type != types.WorkPickupType {
		t.Fatalf("the pickup is a %q, want its own type", ev.Type)
	}
	if !inbox.Ledgered(ev.Type) {
		t.Error("the completion ledger does not record a pickup, so a redelivery runs it again")
	}
	// A TYPED ANSWER ON ITS OWN ITEM (ADR-0122), which an assignment's turn
	// does not owe; and a merge with another tool obligation keeps the item.
	owed := turn.ToolReply("")
	owed.Item = "t-1"
	if got := ReplyFor([]*events.Event{ev}); !reflect.DeepEqual(got, owed) {
		t.Errorf("a pickup's turn owes %+v, want a typed answer on t-1", got)
	}
	assigned := events.New(types.TaskAssigned{Agent: "a1", RoleName: "Engineer"}, events.TraceContext{})
	if got := ReplyFor([]*events.Event{assigned}); got.Item != "" {
		t.Errorf("an assignment's turn owes a typed answer on %q", got.Item)
	}
	for _, order := range [][]*events.Event{{assigned, ev}, {ev, assigned}} {
		if got := ReplyFor(order); !reflect.DeepEqual(got, owed) {
			t.Errorf("a pickup merged with an assignment owes %+v, want the typed answer on t-1", got)
		}
	}
	fire := events.New(types.TaskAssigned{Schedule: "standup"}, events.TraceContext{})
	if _, merged := mergeScheduledFires([]*events.Event{fire, ev}); merged {
		t.Error("a pickup merged into a schedule's fires")
	}
	if got := DescribeTrigger([]*events.Event{ev}, ""); got == "("+ev.Type+")" || got == "" {
		t.Errorf("the pickup's ask is %q, want its brief", got)
	}
}
