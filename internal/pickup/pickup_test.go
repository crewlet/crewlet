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

// at is the instant a key is read at where the case is not about time.
var at = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

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
	base := pickup.KeyOf(seat, item("a", worklet.StatusTodo, 40), at)
	if again := pickup.KeyOf(seat, item("a", worklet.StatusTodo, 40), at); again.WakeID() != base.WakeID() ||
		again.WorkKey() != base.WorkKey() || again.ClaimKey() != base.ClaimKey() {
		t.Fatal("one pickup keyed twice gave two identities; a second node or a " +
			"restart would hand the seat the same item again")
	}
	started := item("a", worklet.StatusInProgress, 40)
	progressed := started
	progressed.Progressed = 52
	// THE SEAT'S OWN WAIT moves no other part: its date coming is the change.
	waited := item("a", worklet.StatusTodo, 40)
	until := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	waited.Start = &until
	for name, other := range map[string]pickup.Key{
		"the date it waited until came": pickup.KeyOf(seat, waited, at),
		"somebody else changed it":      pickup.KeyOf(seat, item("a", worklet.StatusTodo, 41), at),
		"its status moved":              pickup.KeyOf(seat, started, at),
		"the seat made progress on it":  pickup.KeyOf(seat, progressed, at),
		"another item":                  pickup.KeyOf(seat, item("b", worklet.StatusTodo, 40), at),
		"another seat":                  pickup.KeyOf(uuid.New(), item("a", worklet.StatusTodo, 40), at),
	} {
		if other.WakeID() == base.WakeID() || other.WorkKey() == base.WorkKey() {
			t.Errorf("%s kept the pickup's identity, so the seat is never "+
				"handed the item again", name)
		}
	}
	if pickup.KeyOf(seat, progressed, at) == pickup.KeyOf(seat, started, at) {
		t.Error("the seat's progress on started work left its key where it was")
	}
	// AN ITEM WITH NO START keeps the identity older builds derive, so a
	// rolling upgrade hands no such item again.
	older := uuid.NewSHA1(uuid.MustParse("7d0c6f2e-4b1a-5e39-8c27-91f3a6b2d4e8"),
		[]byte(seat.String()+"\x00a\x0040\x00todo\x000"))
	if base.WakeID() != older {
		t.Errorf("an item with no start is keyed %s, want %s as older builds key it", base.WakeID(), older)
	}
}

// A QUIET ITEM IS RECHECKED ONCE A DAY (ADR-0122): an item the seat left
// without a typed answer keeps the key it was handed under until a whole day
// has passed since it went quiet, takes a new key then and keeps it through
// that day, and takes another the next; an item a typed answer holds keeps its
// key however long it waits; and an item with no recheck due keeps the identity
// older builds derive.
//
// Mutation: leave the recheck out of the key's text, and every recheck is the
// pickup the ledger already refuses, so a prose wait is final again.
func TestAQuietItemIsRecheckedOnceADay(t *testing.T) {
	t.Parallel()
	quietSince := time.Date(2026, 11, 8, 21, 17, 0, 0, time.UTC)
	quiet := item("a", worklet.StatusInProgress, 40)
	quiet.Quiet = quietSince
	keyAt := func(after time.Duration) pickup.Key { return pickup.KeyOf(seat, quiet, quietSince.Add(after)) }
	handed := keyAt(time.Minute)
	if handed.Recheck != 0 || handed.WakeID() != keyAt(pickup.RecheckAfter-time.Second).WakeID() {
		t.Fatal("the key moved before a day had passed since the item went quiet")
	}
	first := keyAt(pickup.RecheckAfter)
	if first.WakeID() == handed.WakeID() || first.Recheck != quietSince.Add(pickup.RecheckAfter).Unix() {
		t.Fatalf("a day after the item went quiet its key is %+v, want a recheck due then", first)
	}
	if keyAt(2*pickup.RecheckAfter-time.Second).WakeID() != first.WakeID() {
		t.Error("the first recheck's key moved before the second day was out")
	}
	if second := keyAt(2 * pickup.RecheckAfter); second.WakeID() == first.WakeID() ||
		second.WakeID() == handed.WakeID() {
		t.Error("two days after the item went quiet its key is one it already had")
	}

	held := item("a", worklet.StatusInProgress, 40)
	if pickup.KeyOf(seat, held, quietSince.Add(30*pickup.RecheckAfter)).WakeID() != handed.WakeID() {
		t.Error("an item a typed answer holds came back for a recheck")
	}
	older := uuid.NewSHA1(uuid.MustParse("7d0c6f2e-4b1a-5e39-8c27-91f3a6b2d4e8"),
		[]byte(seat.String()+"\x00a\x0040\x00in_progress\x000"))
	if handed.WakeID() != older {
		t.Errorf("an item with no recheck due is keyed %s, want %s as older builds key it", handed.WakeID(), older)
	}
}

// A RECHECK'S BRIEF SAYS WHY IT CAME BACK and since when, and every brief names
// the five typed answers and that a comment alone is none (ADR-0122).
func TestARecheckSaysWhyItCameBack(t *testing.T) {
	t.Parallel()
	it := item("a", worklet.StatusInProgress, 40)
	it.Quiet = time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	ev := pickup.Event(seat, "engineer", it, pickup.KeyOf(seat, it, it.Quiet.Add(pickup.RecheckAfter)), 1,
		false, events.TraceContext{})
	assigned, _ := events.DataAs[*types.WorkPickup](ev)
	if assigned.Quiet == nil || !assigned.Quiet.Equal(it.Quiet) {
		t.Fatalf("a recheck carries quiet %v, want %s", assigned.Quiet, it.Quiet)
	}
	brief := assigned.Brief()
	for _, want := range []string{"comes back to you", "2026-11-08T09:00:00Z", "A comment alone is not an answer",
		"`ask`", "`waiting_on`", "`wait_until` and `wait_reason`", "`assignee`", "status"} {
		if !strings.Contains(brief, want) {
			t.Errorf("a recheck's brief lacks %q:\n%s", want, brief)
		}
	}
	first := pickup.Event(seat, "engineer", it, pickup.KeyOf(seat, it, it.Quiet.Add(time.Hour)), 1, false,
		events.TraceContext{})
	if handed, _ := events.DataAs[*types.WorkPickup](first); handed.Quiet != nil ||
		strings.Contains(handed.Brief(), "It comes back to you because") {
		t.Errorf("a first hand-over reads as a recheck: %+v", handed)
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
	key := pickup.KeyOf(seat, it, at)
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
		"3 ready items", "get_work_item", "not handed this item again",
		"`ask`", "`waiting_on`", "`wait_until` and `wait_reason`", "reassign it"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
	// EVERY ANSWER IS A CHANGE TO THE ITEM: a wait said in a comment is none.
	if strings.Contains(brief, "say why") {
		t.Errorf("the brief still offers a comment as a wait:\n%s", brief)
	}
}

// A COUNT PAST WHAT THE PICKUP READ IS A FLOOR, and the brief says so.
func TestABriefPastTheReadCountSaysAtLeast(t *testing.T) {
	t.Parallel()
	it := item("a", worklet.StatusTodo, 40)
	ev := pickup.Event(seat, "engineer", it, pickup.KeyOf(seat, it, at), 400, true, events.TraceContext{})
	assigned, _ := events.DataAs[*types.WorkPickup](ev)
	if brief := assigned.Brief(); !strings.Contains(brief, "at least 400 ready items") {
		t.Errorf("a capped count reads as exact:\n%s", brief)
	}
}
