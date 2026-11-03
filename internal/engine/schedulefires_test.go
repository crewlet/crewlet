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
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/tracing"
)

// scheduledFire is one fire of a schedule as the scheduler publishes it: its
// own trace, the instant it came due, and the schedule's conversation — the
// partition every fire of the schedule shares and the identity its turn is
// filed under — stamped as both keys, as the scheduler stamps them.
func scheduledFire(name, label string, due time.Time) *events.Event {
	return fireOf("a-1", "sre", name, label, due)
}

// fireOf is one fire of the role schedule name of the seat whose agent id is
// agent and whose handle is handle, due at due, as the scheduler publishes it
// ([scheduledFire]).
func fireOf(agent, handle, name, label string, due time.Time) *events.Event {
	ev := events.New(types.TaskAssigned{
		TaskID: "role:" + handle + ":" + name + ":" + label + ":" + handle, Agent: agent,
		RoleName: "SRE", Description: "check the services", Schedule: name,
		ScopeType: types.ScheduleScopeRole, ScopeID: agent,
		ScheduledAt: due, TimeoutSeconds: 1800,
	}, events.NewTrace())
	ev.Source = "scheduler"
	ev.Timestamp = due.Add(time.Second)
	key := schedule.FirePartition(schedule.Entry{Scope: types.ScheduleScopeRole, ScopeID: agent,
		Schedule: org.Schedule{Name: name}})
	notify.Stamp(ev, key, key)
	return ev
}

// FIRES THAT WAITED TOGETHER RUN ONCE, AS THE LATEST, ON THE SCHEDULE'S ONE
// CONVERSATION.
//
// On a live company an SRE's five-hourly health check fired three times into
// an inbox held by a provider limit, and when the limit cleared the three
// fires ran as three turns within four minutes, each reporting the same thing.
// They reach the seat as one partition now, and the seat runs only the latest:
// told how many earlier fires it stands for, under the latest fire's own
// trace, with each earlier fire recorded as superseded under its own. The run
// serves the schedule's conversation, the one every fire carries, so it reads
// the head the schedule's last run left once for all three, and the merged
// fire carries it on.
//
// Mutation: run each waiting fire as its own turn, take the run's conversation
// off anything but the fires' identity, or stamp the merged fire with none,
// and a check goes red.
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
	want := notify.ConversationIdentityOfAll([]*events.Event{sixth})
	if !notify.Scheduled(want) || req.ConversationKey != want {
		t.Errorf("the run serves conversation %q, want the schedule's %q", req.ConversationKey, want)
	}
	if got := notify.ConversationIdentityOfAll(req.Trigger); got != want {
		t.Errorf("the merged fire carries conversation %q, want the schedule's %q", got, want)
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
			// FILED UNDER THE SCHEDULE'S WHOLE IDENTITY, which is what
			// the seat's node publishes it under in its own usage day:
			// a name alone could be either of two units' standups.
			if rec.ScopeType != types.ScheduleScopeRole || rec.ScopeID != "a-1" ||
				rec.ScheduleName != "health-check" {
				t.Errorf("the superseded record names the schedule %s/%s/%s, want role/a-1/health-check",
					rec.ScopeType, rec.ScopeID, rec.ScheduleName)
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

// A FIRE IS NEVER TAKEN AS THE ANSWER TO THE LAST FIRE.
//
// Every fire of a schedule carries the schedule's conversation, the very one
// the last fire's run parked under when its job asked a question — so offered
// as a reply, the next fire would be recorded as that question's answer, posted
// after it was asked, and resume the run with the schedule's own task text in
// place of a person's word. A schedule's conversation is nobody's to reply
// into, so the fire is never offered: it runs as the turn it is, and the run
// waits on for its answer. The parked run and the fire are the coordinator's
// and the dispatcher's own, wired as a node wires them.
//
// Mutation: drop the notify.Derived gate from the dispatcher's offer, or let
// Derived read a schedule's key as one somebody could reply into, and the fire
// is recorded as the parked run's answer.
func TestAFireIsNeverTakenAsTheAnswerToTheLastFire(t *testing.T) {
	t.Parallel()
	order := &sequence{}
	s := newBusySeat(t, func(*busySeat) sandbox.Resumer { return &orderedResumer{order: order} })
	ctx := t.Context()
	agent := fixtureSeat("swe").String()
	last := fireOf(agent, "swe", "health-check", "20261030T1300", time.Now().UTC().Add(-time.Hour))
	key := notify.ConversationIdentityOfAll([]*events.Event{last})
	if _, err := s.coordinator.Launch(ctx, s.manager, sandbox.LaunchRequest{
		Turn: sandbox.TurnRef{TurnID: "f1", AgentID: agent, AgentHandle: "swe", Role: "SWE",
			PartitionKey: key, ConversationKey: key},
		Brief: "check the services", Task: "check the services",
		Spec: sandbox.Spec{Harness: "claude-code", PauseTTLSec: 1800},
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := s.store.MarkSuspended(ctx, "f1", sandbox.Suspension{State: []byte(`{"messages":[]}`)}); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
	parked := s.parks(t, "f1", "which dashboard is the real one?")
	if _, awaits := s.coordinator.SeatRuns("swe"); parked.ConversationKey != key || !awaits {
		t.Fatalf("the premise: the last fire's run parked under %q (a question open: %v), want its "+
			"schedule's %q", parked.ConversationKey, awaits, key)
	}

	next := fireOf(agent, "swe", "health-check", "20261030T1800", time.Now().UTC())
	if err := s.q.Publish(ctx, fixtureInbox("swe"), next); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	eventually(t, "the next fire to be handled", func() bool {
		return len(s.ordinary.ran()) == 1 || mustRun(t, s.store, "f1").Answer != nil
	})
	settle()
	if run := mustRun(t, s.store, "f1"); run.Status != sandbox.StatusAwaiting || run.Answer != nil {
		t.Fatalf("the last fire's run is %q with the answer %+v, want it still waiting on its question",
			run.Status, run.Answer)
	}
	if ran := s.ordinary.ran(); len(ran) != 1 {
		t.Fatalf("the next fire ran %d turns, want one of its own", len(ran))
	}
	if resumed := order.list(); len(resumed) != 0 {
		t.Fatalf("the seat resumed %v on the next fire, want nothing resumed", resumed)
	}
}
