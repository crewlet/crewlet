package schedule

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// refusingMailboxes is a node that cannot yet confirm the mailboxes of the
// seats it names.
type refusingMailboxes struct {
	mu    sync.Mutex
	seats map[uuid.UUID]bool
}

func refuseMailboxes(handles ...string) *refusingMailboxes {
	m := &refusingMailboxes{seats: map[uuid.UUID]bool{}}
	for _, h := range handles {
		id, _ := topics.SeatFromInbox(seatInbox(h))
		m.seats[id] = true
	}
	return m
}

func (m *refusingMailboxes) admit() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seats = map[uuid.UUID]bool{}
}

func (m *refusingMailboxes) ConfirmMailbox(_ context.Context, seat uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seats[seat] {
		return fmt.Errorf("the mailbox is not complete: %w", queue.ErrUnavailable)
	}
	return nil
}

// A FIRE WHOSE SEAT'S MAILBOX IS NOT CONFIRMED IS SENT ON A LATER TICK. It is
// not claimed, so no claim is spent on a fire that never reaches the seat, and
// the tick keeps its window open, so the next one finds it again; the
// colleague whose fire was sent is not sent it a second time.
func TestAFireWhoseMailboxIsNotConfirmedIsSentOnALaterTick(t *testing.T) {
	t.Parallel()
	boxes := refuseMailboxes("qa-dev")
	h := build(t, unitOrg(org.TargetEach), func(o *Options) { o.Mailboxes = boxes })
	h.seed(tickAt(8, 59, 30))

	if got := h.tick(tickAt(9, 0, 30)); got != 1 {
		t.Fatalf("Tick = %d fires, want 1: the colleague with a mailbox still runs", got)
	}
	requireTopics(t, h.q.inboxTopics(), seatInbox("qa-lead"))
	for _, row := range h.ledger.rows(t) {
		if row.TargetHandle == "qa-dev" {
			t.Fatalf("the deferred fire was claimed (%v): a claim spent on a fire that "+
				"never reached its seat is one no later tick sends", row)
		}
	}
	if got := h.lastTick(); !got.Equal(tickAt(8, 59, 30)) {
		t.Fatalf("the window moved to %v with a fire still deferred in it", got)
	}

	if got := h.tick(tickAt(9, 0, 40)); got != 0 {
		t.Fatalf("a tick while the mailbox is still refused fired %d", got)
	}
	boxes.admit()
	if got := h.tick(tickAt(9, 0, 50)); got != 1 {
		t.Fatalf("the tick after the mailbox was confirmed fired %d, want the deferred one", got)
	}
	requireTopics(t, h.q.inboxTopics(), seatInbox("qa-lead"), seatInbox("qa-dev"))
	if got := h.lastTick(); !got.Equal(tickAt(9, 0, 50)) {
		t.Fatalf("the window is at %v after every fire in it was sent, want it moved on", got)
	}
}

// FIRES THAT WAITED FOR A MAILBOX REACH THE SEAT TOGETHER, in the one partition
// every fire of a schedule shares, so the seat's dispatcher runs the latest and
// records the rest superseded (#559) instead of running each.
func TestFiresThatWaitedForAMailboxShareOnePartition(t *testing.T) {
	t.Parallel()
	boxes := refuseMailboxes("qa")
	h := build(t, roleOrg(org.Schedule{Name: "pulse", Cron: "* * * * *", Task: "check"}),
		func(o *Options) { o.Mailboxes = boxes })
	h.seed(tickAt(8, 59, 30))
	h.tick(tickAt(9, 0, 30))
	h.tick(tickAt(9, 1, 30))
	if got := len(h.q.inboxTasks()); got != 0 {
		t.Fatalf("%d fires reached a seat whose mailbox was refused", got)
	}
	boxes.admit()
	if got := h.tick(tickAt(9, 1, 40)); got != 2 {
		t.Fatalf("the tick after the mailbox was confirmed fired %d, want both waiting fires", got)
	}
	tasks := h.q.inboxTasks()
	if len(tasks) != 2 || notify.KeyOf(tasks[0].ev) != notify.KeyOf(tasks[1].ev) ||
		notify.KeyOf(tasks[0].ev) == "" {
		t.Fatalf("the waiting fires carry partitions %q and %q, want one shared partition",
			notify.KeyOf(tasks[0].ev), notify.KeyOf(tasks[1].ev))
	}
}

// A FIRE DEFERRED PAST THE CATCHUP CEILING IS RECORDED AS MISSED, under its own
// runner, never dropped without a row: a seat whose mailbox never came back
// leaves a ledger entry saying its fire was missed.
func TestAFireDeferredPastTheCatchupCeilingIsRecordedMissed(t *testing.T) {
	t.Parallel()
	boxes := refuseMailboxes("qa")
	h := build(t, roleOrg(), func(o *Options) {
		o.Mailboxes = boxes
		o.CatchupMin = time.Minute
		o.CatchupMax = 2 * time.Minute
	})
	h.seed(tickAt(8, 59, 30))
	h.tick(tickAt(9, 0, 30))
	if got := h.lastTick(); !got.Equal(tickAt(8, 59, 30)) {
		t.Fatalf("the window moved to %v with a fire still deferred in it", got)
	}
	h.tick(tickAt(9, 2, 0))
	if got := h.lastTick(); !got.Equal(tickAt(9, 2, 0)) {
		t.Fatalf("the window is held at %v past the catchup ceiling", got)
	}
	rows := h.ledger.rows(t)
	if len(rows) != 1 || rows[0].TargetHandle != "qa" || rows[0].Outcome != OutcomeSkippedCatchup {
		t.Fatalf("ledger rows = %v, want one missed-tick row for qa", rows)
	}
	if got := len(h.q.inboxTasks()); got != 0 {
		t.Fatalf("%d fires reached a seat whose mailbox was refused", got)
	}
}
