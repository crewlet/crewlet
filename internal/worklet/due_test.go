package worklet_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fileOn files one item on ana at `at`, in a status and at a priority.
func (h *flowHarness) fileOn(id string, status worklet.Status, priority worklet.Priority, at time.Time) {
	h.t.Helper()
	h.task(id, at, func(task *worklet.Task) {
		task.Assignee, task.Priority = "ana", priority
		task.Status, task.StatusGroup = status, status.Group()
	})
}

func (h *flowHarness) ready(handle string, now time.Time) worklet.ReadyWork {
	h.t.Helper()
	out, err := h.reader.ReadyWork(h.t.Context(), worklet.ReadyWorkQuery{
		Handle: handle, Level: statelog.ReadStale,
	}, now)
	if err != nil {
		h.t.Fatalf("ReadyWork(%s): %v", handle, err)
	}
	return out
}

// A DUE ITEM COMES RIGHT AFTER URGENT WORK (ADR-0162). An item in progress
// whose start passed with nothing of its seat's on it since is due: after
// urgent work, before every other item (started work of a higher priority
// included), with the wait's words and their writer. The seat's turn segment or
// own write on it since the start ends it; a turn before the start, or another
// seat's, does not.
//
// Mutation: order due items with started work, and the check waits behind
// higher-priority work; drop the start bound on the seat's writes, and the
// wait's own write ends it at once.
func TestADueItemComesRightAfterUrgentWork(t *testing.T) {
	t.Parallel()
	h := newFlowHarness(t, worklet.Posture{})
	created := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	start := created.Add(time.Hour)
	now := start.Add(2 * time.Hour)
	const reason = "the release's health check runs then"

	h.fileOn("fire", worklet.StatusTodo, worklet.PriorityUrgent, created)
	h.fileOn("started", worklet.StatusInProgress, worklet.PriorityHigh, created)
	h.fileOn("later", worklet.StatusTodo, worklet.PriorityNormal, created)
	for _, id := range []string{"check", "probe"} {
		h.fileOn(id, worklet.StatusInProgress, worklet.PriorityLow, created)
		h.record(worklet.TaskSubject(id), worklet.OpPatch, worklet.ChangeFields,
			worklet.TaskPatch{StartAt: &start, WaitReason: new(reason)}, "ana", created.Add(10*time.Minute))
	}
	// A TURN BEFORE THE START, AND ANOTHER SEAT'S SINCE, leave it due.
	h.turn("check", "ana", start.Add(-30*time.Minute))
	h.turn("check", "bo", start.Add(10*time.Minute))

	order := func(want []string, whys []types.PickupWhy) []worklet.ReadyItem {
		t.Helper()
		got := h.ready("ana", now)
		if ids := readyIDs(got); !slices.Equal(ids, want) {
			t.Fatalf("ready work = %v, want %v", ids, want)
		}
		for i, why := range whys {
			if got.Items[i].Why != why {
				t.Errorf("%s sits there because %q, want %q", got.Items[i].ID, got.Items[i].Why, why)
			}
		}
		return got.Items
	}
	items := order([]string{"fire", "check", "probe", "started", "later"},
		[]types.PickupWhy{types.PickupUrgent, types.PickupDue, types.PickupDue, types.PickupInProgress,
			types.PickupQueue})
	if w := items[1]; w.WaitReason != reason || w.WaitBy == nil ||
		*w.WaitBy != (provenance.Author{Kind: iam.ActorAgent, Seat: "ana"}) || w.Start == nil || !w.Start.Equal(start) {
		t.Errorf("the due item carries start %v, reason %q by %+v; want %s, %q by ana",
			w.Start, w.WaitReason, w.WaitBy, start, reason)
	}
	if items[3].WaitReason != "" || items[3].WaitBy != nil {
		t.Errorf("started work that is not due carries a wait: %+v", items[3])
	}

	// THE SEAT'S TURN ON IT SINCE THE START ends it: it is started work again.
	h.turn("check", "ana", start.Add(20*time.Minute))
	items = order([]string{"fire", "probe", "started", "check", "later"},
		[]types.PickupWhy{types.PickupUrgent, types.PickupDue, types.PickupInProgress,
			types.PickupInProgress, types.PickupQueue})
	if items[3].WaitReason != "" {
		t.Errorf("an item no longer due still carries its wait's words: %+v", items[3])
	}

	// SO DOES THE SEAT'S OWN WRITE ON IT SINCE.
	h.record(worklet.TaskSubject("probe"), worklet.OpPatch, worklet.ChangeFields,
		worklet.TaskPatch{Title: new("probe the health endpoint")}, "ana", start.Add(30*time.Minute))
	order([]string{"fire", "started", "check", "probe", "later"},
		[]types.PickupWhy{types.PickupUrgent, types.PickupInProgress, types.PickupInProgress,
			types.PickupInProgress, types.PickupQueue})
}
