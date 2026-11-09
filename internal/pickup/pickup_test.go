package pickup_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/pickup"
	"github.com/crewlet/crewlet/internal/worklet"
)

var seat = uuid.MustParse("6a1f3c52-9d0e-4b7a-8c31-2e5f7d9a0b14")

func item(id string, status worklet.Status, moved uint64) worklet.ReadyItem {
	return worklet.ReadyItem{
		TaskRow: worklet.TaskRow{ID: id, Key: "WRK-" + id, Status: status,
			Updated: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)},
		Why: types.PickupQueue, Moved: moved,
	}
}

// A PICKUP IS MADE ONCE PER (seat, item, the last change somebody else made,
// status): the same key on every node and attempt, a new one when somebody
// else changes the item or its status moves, and the seat's own comment, which
// moves neither, is the same pickup.
func TestAPickupIsKeyedOnTheItemsVersionBySomebodyElse(t *testing.T) {
	t.Parallel()
	base := pickup.KeyOf(seat, item("a", worklet.StatusTodo, 40))
	if again := pickup.KeyOf(seat, item("a", worklet.StatusTodo, 40)); again.WakeID() != base.WakeID() ||
		again.WorkKey() != base.WorkKey() || again.ClaimKey() != base.ClaimKey() {
		t.Fatal("one pickup keyed twice gave two identities; a second node or a " +
			"restart would hand the seat the same item again")
	}
	for name, other := range map[string]pickup.Key{
		"somebody else changed it": pickup.KeyOf(seat, item("a", worklet.StatusTodo, 41)),
		"its status moved":         pickup.KeyOf(seat, item("a", worklet.StatusInProgress, 40)),
		"another item":             pickup.KeyOf(seat, item("b", worklet.StatusTodo, 40)),
		"another seat":             pickup.KeyOf(uuid.New(), item("a", worklet.StatusTodo, 40)),
	} {
		if other.WakeID() == base.WakeID() || other.WorkKey() == base.WorkKey() {
			t.Errorf("%s kept the pickup's identity, so the seat is never "+
				"handed the item again", name)
		}
	}

	// CHOOSE SKIPS EVERY PICKUP THE LEDGER HOLDS, in order.
	items := []worklet.ReadyItem{item("a", worklet.StatusInProgress, 40),
		item("b", worklet.StatusTodo, 7), item("c", worklet.StatusTodo, 9)}
	worked := map[string]bool{pickup.KeyOf(seat, items[0]).WorkKey(): true}
	got, key, ok := pickup.Choose(seat, items, worked)
	if !ok || got.ID != "b" || key != pickup.KeyOf(seat, items[1]) {
		t.Fatalf("Choose = %q, %v; want the first item the seat was not handed, b", got.ID, ok)
	}
	for _, it := range items {
		worked[pickup.KeyOf(seat, it).WorkKey()] = true
	}
	if _, _, ok := pickup.Choose(seat, items, worked); ok {
		t.Error("Choose handed an item every pickup of which was already worked")
	}
	if keys := pickup.WorkKeys(seat, items); len(keys) != 3 || keys[1] != pickup.KeyOf(seat, items[1]).WorkKey() {
		t.Errorf("WorkKeys = %v, want each item's key in order", keys)
	}
}

// THE EVENT IS A TASK ASSIGNMENT NAMING THE ITEM, filed under the item's
// conversation, with the derived id and the item's own instant, and its brief
// names the item and why.
func TestAPickupEventNamesItsItem(t *testing.T) {
	t.Parallel()
	it := item("a", worklet.StatusInProgress, 40)
	it.Why = types.PickupInProgress
	key := pickup.KeyOf(seat, it)
	ev := pickup.Event(seat, "engineer", it, key, 3, events.TraceContext{})
	if ev.ID != key.WakeID() || !ev.Timestamp.Equal(it.Updated) || ev.Source != pickup.Source {
		t.Fatalf("event id %s at %s from %q, want the derived id at the item's instant",
			ev.ID, ev.Timestamp, ev.Source)
	}
	if got := notify.ConversationIdentityOf(ev); got != worklet.ItemConversation("a") {
		t.Errorf("conversation = %q, want the item's own", got)
	}
	if got := notify.KeyOf(ev); got != worklet.ItemConversation("a") {
		t.Errorf("partition = %q, want the item's own", got)
	}
	assigned, ok := events.DataAs[*types.TaskAssigned](ev)
	if !ok || assigned.Pickup == nil || assigned.Pickup.Key != "WRK-a" || assigned.Pickup.Ready != 3 {
		t.Fatalf("payload = %+v, want a pickup of WRK-a among 3 ready", assigned)
	}
	origin := assigned.TriggerOrigin()
	if origin.Kind != types.OriginWorkItem || origin.ID != "WRK-a" || origin.Reason != types.PickupReason {
		t.Errorf("origin = %+v, want the work item under the pickup reason", origin)
	}
	brief := assigned.Brief()
	for _, want := range []string{"WRK-a", "in progress and nothing is working on it",
		"3 ready items", "get_work_item", "not be handed this item again"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
}
