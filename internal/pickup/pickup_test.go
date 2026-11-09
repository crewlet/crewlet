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
			Updated: time.Date(2026, 11, 8, 9, 30, 0, 0, time.UTC)},
		Why: types.PickupQueue, Moved: moved,
		MovedAt: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC),
	}
}

// A PICKUP IS MADE ONCE PER KEY: the same identity on every node and attempt,
// a new one when somebody else changes the item, its status moves or the seat
// makes progress on started work, and none for the seat's own comment.
func TestAPickupIsKeyedOnTheItemsVersionBySomebodyElse(t *testing.T) {
	t.Parallel()
	base := pickup.KeyOf(seat, item("a", worklet.StatusTodo, 40))
	if again := pickup.KeyOf(seat, item("a", worklet.StatusTodo, 40)); again.WakeID() != base.WakeID() ||
		again.WorkKey() != base.WorkKey() || again.ClaimKey() != base.ClaimKey() {
		t.Fatal("one pickup keyed twice gave two identities; a second node or a " +
			"restart would hand the seat the same item again")
	}
	started := item("a", worklet.StatusInProgress, 40)
	progressed := started
	progressed.Progressed = 52
	for name, other := range map[string]pickup.Key{
		"somebody else changed it":     pickup.KeyOf(seat, item("a", worklet.StatusTodo, 41)),
		"its status moved":             pickup.KeyOf(seat, started),
		"the seat made progress on it": pickup.KeyOf(seat, progressed),
		"another item":                 pickup.KeyOf(seat, item("b", worklet.StatusTodo, 40)),
		"another seat":                 pickup.KeyOf(uuid.New(), item("a", worklet.StatusTodo, 40)),
	} {
		if other.WakeID() == base.WakeID() || other.WorkKey() == base.WorkKey() {
			t.Errorf("%s kept the pickup's identity, so the seat is never "+
				"handed the item again", name)
		}
	}
	if pickup.KeyOf(seat, progressed) == pickup.KeyOf(seat, started) {
		t.Error("the seat's progress on started work left its key where it was")
	}
}

// THE EVENT IS A TASK ASSIGNMENT NAMING THE ITEM and whom it is for, under the
// derived id and the instant of the change its key names, filed under the
// item's conversation, and its brief names the item and why.
func TestAPickupEventNamesItsItem(t *testing.T) {
	t.Parallel()
	it := item("a", worklet.StatusInProgress, 40)
	it.Why = types.PickupInProgress
	it.For = &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	key := pickup.KeyOf(seat, it)
	ev := pickup.Event(seat, "engineer", it, key, 3, false, events.TraceContext{})
	if ev.ID != key.WakeID() || !ev.Timestamp.Equal(it.MovedAt) || ev.Source != pickup.Source {
		t.Fatalf("event id %s at %s from %q, want the derived id at the instant of "+
			"the change its key names, not the item's last update", ev.ID, ev.Timestamp, ev.Source)
	}
	if got := notify.ConversationIdentityOf(ev); got != worklet.ItemConversation("a") {
		t.Errorf("conversation = %q, want the item's own", got)
	}
	if got := notify.KeyOf(ev); got != worklet.ItemConversation("a") {
		t.Errorf("partition = %q, want the item's own", got)
	}
	assigned, ok := events.DataAs[*types.WorkPickup](ev)
	if !ok || ev.Type != types.WorkPickupType || assigned.Key != "WRK-a" || assigned.Ready != 3 {
		t.Fatalf("%s payload = %+v, want a pickup of WRK-a among 3 ready", ev.Type, assigned)
	}
	if w := assigned.WorkingFor; w == nil || w.Seat != "ada" {
		t.Errorf("the pickup is worked for %+v, want ada, whom the item is for", w)
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

// A COUNT PAST WHAT THE PICKUP READ IS A FLOOR, and the brief says so.
func TestABriefPastTheReadCountSaysAtLeast(t *testing.T) {
	t.Parallel()
	it := item("a", worklet.StatusTodo, 40)
	ev := pickup.Event(seat, "engineer", it, pickup.KeyOf(seat, it), 400, true, events.TraceContext{})
	assigned, _ := events.DataAs[*types.WorkPickup](ev)
	if brief := assigned.Brief(); !strings.Contains(brief, "at least 400 ready items") {
		t.Errorf("a capped count reads as exact:\n%s", brief)
	}
}
