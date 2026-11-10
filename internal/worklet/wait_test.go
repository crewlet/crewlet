package worklet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN ITEM WAITS UNTIL ITS DATE AND SAYS WHY: the seat holding it is not handed
// it before the date and is after it, the reason is on the item's read, and a
// start moved or cleared without a reason takes the old one off; a reason needs
// the start it waits until, in one line.
func TestAnItemWaitsUntilItsDateAndSaysWhy(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileFor(t, r, "w", "app", worklet.StatusTodo, worklet.PriorityLow)
	app := r.writer.As("app", worklet.AuthorAgent, worklet.Provenance{})
	until, reason := wednesday.Add(48*time.Hour), "the vendor's window opens then"
	patch := func(op string, as *worklet.Writer, p worklet.TaskPatch) error {
		t.Helper()
		_, err := as.UpdateTask(t.Context(), op, "w", "ENG", worklet.NoIfMatch, p, worklet.ChangeFields, nil)
		r.drain()
		return err
	}
	readyAt := func(now time.Time) *worklet.ReadyItem {
		t.Helper()
		out, err := r.reader.ReadyWork(t.Context(), worklet.ReadyWorkQuery{Handle: "app",
			Level: statelog.ReadStale}, now)
		if err != nil {
			t.Fatalf("ready work at %s: %v", now, err)
		}
		for i := range out.Items {
			if out.Items[i].ID == "w" {
				return &out.Items[i]
			}
		}
		return nil
	}
	held := func() worklet.Task {
		t.Helper()
		detail, err := r.reader.Task(t.Context(), "w", worklet.DetailWants{},
			statelog.Freshness{Level: statelog.ReadSession})
		if err != nil {
			t.Fatalf("read w: %v", err)
		}
		return detail.Task
	}

	if err := patch("op-wait", app, worklet.TaskPatch{StartAt: &until, WaitReason: &reason}); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got := held().WaitReason; got != reason {
		t.Errorf("the item reads its wait's reason as %q, want %q", got, reason)
	}
	if readyAt(until.Add(-time.Minute)) != nil {
		t.Error("the item is ready before the date it waits until")
	}
	if item := readyAt(until); item == nil || item.Start == nil || !item.Start.Equal(until) {
		t.Errorf("at its date the item is ready as %+v, want it with the start it waited until", item)
	}

	for name, refused := range map[string]worklet.TaskPatch{
		"a reason with no start": {WaitReason: &reason},
		"a reason of two lines":  {StartAt: &until, WaitReason: new("the window\nopens then")},
	} {
		if err := patch("op-"+name, app, refused); !errors.Is(err, worklet.ErrInvalid) {
			t.Errorf("%s: %v, want it refused as invalid", name, err)
		}
	}

	later := until.Add(24 * time.Hour)
	if err := patch("op-move", r.writer, worklet.TaskPatch{StartAt: &later}); err != nil {
		t.Fatalf("move the start: %v", err)
	}
	if got := held(); got.WaitReason != "" || got.StartAt == nil || !got.StartAt.Equal(later) {
		t.Errorf("a start moved with no reason left %q beside %v, want no reason", got.WaitReason, got.StartAt)
	}

	// A WAIT ENDED BY ITS START'S CLEAR takes its reason with it: the clear is
	// the patch's flag, with no StartAt for the writer to see. Mutation: the
	// writer clearing a reason only beside a StartAt leaves this one standing
	// with no date to wait until.
	again := later.Add(24 * time.Hour)
	if err := patch("op-wait-again", app, worklet.TaskPatch{StartAt: &again, WaitReason: &reason}); err != nil {
		t.Fatalf("wait again: %v", err)
	}
	if err := patch("op-end", r.writer, worklet.TaskPatch{ClearStart: true}); err != nil {
		t.Fatalf("end the wait: %v", err)
	}
	if got := held(); got.WaitReason != "" || got.StartAt != nil {
		t.Errorf("a cleared start left %q beside %v, want neither", got.WaitReason, got.StartAt)
	}
}
