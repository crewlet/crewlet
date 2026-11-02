package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/tracing"
)

// scheduledFire is one fire of a schedule as the scheduler publishes it: its
// own trace, the instant it came due, and the partition every fire of the
// schedule shares.
func scheduledFire(schedule, label string, due time.Time) *events.Event {
	ev := events.New(types.TaskAssigned{
		TaskID: "role:sre:" + schedule + ":" + label + ":sre", Agent: "a-1",
		RoleName: "SRE", Description: "check the services", Schedule: schedule,
		ScheduledAt: due, TimeoutSeconds: 1800,
	}, events.NewTrace())
	ev.Source = "scheduler"
	ev.Timestamp = due.Add(time.Second)
	notify.Stamp(ev, "schedule/role/a-1/"+schedule, "")
	return ev
}

// FIRES THAT WAITED TOGETHER RUN ONCE, AS THE LATEST.
//
// On a live company an SRE's five-hourly health check fired three times into
// an inbox held by a provider limit, and when the limit cleared the three
// fires ran as three turns within four minutes, each reporting the same thing.
// They reach the seat as one partition now, and the seat runs only the latest:
// told how many earlier fires it stands for, under the latest fire's own
// trace, with each earlier fire recorded as superseded under its own.
func TestFiresOfOneScheduleThatWaitedTogetherRunOnce(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	var ranUnder []string
	d.Turn = func(ctx context.Context, req engine.Request) (turn.Result, error) {
		ranUnder = append(ranUnder, tracing.TraceOf(ctx).TraceID)
		return r.run(ctx, req)
	}
	var seen []*events.Event
	d.Observe = func(_ context.Context, e *events.Event) { seen = append(seen, e) }

	fourth := scheduledFire("health-check", "20261030T1300", clock)
	fifth := scheduledFire("health-check", "20261030T1800", clock.Add(5*time.Hour))
	sixth := scheduledFire("health-check", "20261031T0800", clock.Add(19*time.Hour))

	// IN DELIVERY ORDER, oldest first, which is how a held inbox hands
	// them back.
	if got := d.Dispatch(context.Background(), "sre",
		[]*events.Event{fourth, fifth, sixth}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v (%s)", got.Outcome, got.Reason)
	}
	if len(r.reqs) != 1 {
		t.Fatalf("three waiting fires ran %d turns, want one", len(r.reqs))
	}
	if len(r.parked) != 0 {
		t.Fatalf("%d fires were requeued to run on their own", len(r.parked))
	}
	req := r.reqs[0]
	if len(req.Trigger) != 1 {
		t.Fatalf("the turn was asked about %d fires, want the latest alone", len(req.Trigger))
	}
	kept, ok := events.DataAs[*types.TaskAssigned](req.Trigger[0])
	if !ok {
		t.Fatalf("the trigger is %T, want the latest fire", req.Trigger[0].Data)
	}
	if !kept.ScheduledAt.Equal(clock.Add(19 * time.Hour)) {
		t.Errorf("the run is the fire due %s, want the latest", kept.ScheduledAt)
	}
	if len(kept.Missed) != 2 {
		t.Fatalf("the run stands for %d missed fires, want two", len(kept.Missed))
	}
	brief := kept.Brief()
	for _, want := range []string{
		"check the services",
		"stands for 2 earlier fires",
		clock.Format(time.RFC3339),
		"Do the work once",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief does not say %q:\n%s", want, brief)
		}
	}
	// EVERY CONSTITUENT IS THE TURN'S TO SETTLE, so a redelivery of any of
	// the three is dropped by the completion ledger once the run is worked.
	if len(req.Events) != 3 {
		t.Errorf("the dispatch carries %d constituents, want all three", len(req.Events))
	}
	if len(ranUnder) != 1 || ranUnder[0] != sixth.TraceID {
		t.Errorf("the run is under trace %v, want the latest fire's %s", ranUnder, sixth.TraceID)
	}

	superseded := map[string]string{}
	for _, e := range seen {
		if rec, ok := e.Data.(*types.ScheduledFireSuperseded); ok {
			superseded[rec.TaskID] = e.TraceID
			if rec.SupersededBy != kept.TaskID || rec.TargetHandle != "sre" {
				t.Errorf("superseded record = %+v", rec)
			}
		}
	}
	for _, fire := range []*events.Event{fourth, fifth} {
		task, _ := events.DataAs[*types.TaskAssigned](fire)
		if got, ok := superseded[task.TaskID]; !ok || got != fire.TraceID {
			t.Errorf("fire %s is recorded under trace %q (%v), want its own %s",
				task.TaskID, got, ok, fire.TraceID)
		}
	}
	if len(superseded) != 2 {
		t.Errorf("%d fires recorded as superseded, want the two earlier ones", len(superseded))
	}
}

// TWO SCHEDULES ARE NEVER ONE RUN. A partition holding fires of two schedules
// is a key-scheme bug, and it degrades to one run per fire rather than letting
// one schedule's work stand for another's.
func TestFiresOfTwoSchedulesAreNeverMerged(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	check := scheduledFire("health-check", "20261030T1300", clock)
	standup := scheduledFire("standup", "20261030T1300", clock.Add(time.Minute))
	if got := d.Dispatch(context.Background(), "sre",
		[]*events.Event{check, standup}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v (%s)", got.Outcome, got.Reason)
	}
	if len(r.reqs) != 1 || len(r.reqs[0].Trigger) != 1 || len(r.parked) != 1 {
		t.Fatalf("ran %d turn(s) and requeued %d partition(s); want the head run and "+
			"the other fire requeued to run on its own", len(r.reqs), len(r.parked))
	}
	ran, _ := events.DataAs[*types.TaskAssigned](r.reqs[0].Trigger[0])
	if len(ran.Missed) != 0 {
		t.Errorf("a fire of one schedule stands for another's: %+v", ran.Missed)
	}
}
