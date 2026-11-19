package duty_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
)

var start = time.Date(2030, 1, 7, 9, 0, 0, 0, time.UTC)

// fakeLog keeps what the duty records, and answers LastDone from its runs.
type fakeLog struct {
	live      []advisor.Stored
	requested []duty.Request
	recorded  []advisor.Transition
	runs      []duty.Run
	expired   int
}

func (l *fakeLog) Live(context.Context) ([]advisor.Stored, error)      { return l.live, nil }
func (l *fakeLog) Dismissed(context.Context) ([]advisor.Stored, error) { return nil, nil }
func (l *fakeLog) Requested(context.Context) ([]duty.Request, error)   { return l.requested, nil }
func (l *fakeLog) Expire(context.Context, time.Time) error             { l.expired++; return nil }

func (l *fakeLog) LastDone(context.Context) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	for _, r := range l.runs {
		for _, s := range r.Scopes {
			if s.Status == duty.ScopeDone {
				out[s.Scope] = r.StartedAt
			}
		}
	}
	return out, nil
}

func (l *fakeLog) Record(_ context.Context, t advisor.Transition, _ time.Time) error {
	l.recorded = append(l.recorded, t)
	return nil
}

func (l *fakeLog) RecordRun(_ context.Context, r duty.Run) error {
	l.runs = append(l.runs, r)
	l.requested = nil
	return nil
}

// fakeReader observes what it holds, behind a signature a test moves.
type fakeReader struct {
	observed  []advisor.Observation
	signature string
	reads     int
}

func (r *fakeReader) Observe(context.Context, time.Time) (duty.Observed, error) {
	r.reads++
	return duty.Observed{Observations: r.observed}, nil
}

func (r *fakeReader) Signature(context.Context) (string, error) { return r.signature, nil }

// fakeModel counts its passes.
type fakeModel struct{ passes int }

func (m *fakeModel) Assigned() bool { return true }
func (m *fakeModel) Run(context.Context, time.Time, []advisor.Stored) (duty.ModelRun, error) {
	m.passes++
	return duty.ModelRun{Concluded: true, Summary: map[string]int{"listed": 0}}, nil
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newDuty(l *fakeLog, r *fakeReader, m *fakeModel, c *clock) *duty.Duty {
	n := 0
	d := &duty.Duty{Log: l, Read: r, Now: c.Now, Zone: func() *time.Location { return time.UTC },
		NewID: func() string { n++; return "run-" + strconv.Itoa(n) }}
	if m != nil {
		d.Model = m
	}
	return d
}

func step(t *testing.T, d *duty.Duty) {
	t.Helper()
	if err := d.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// THE RULE PASS RUNS HOURLY AND AT ONCE ON A CHANGE TO THE SETUP, and not on
// every tick in between.
func TestEachClassRunsOnItsCadence(t *testing.T) {
	t.Parallel()
	l, r, c := &fakeLog{}, &fakeReader{signature: "r1"}, &clock{now: start}
	d := newDuty(l, r, nil, c)
	step(t, d)
	if r.reads != 1 || len(l.runs) != 1 || l.runs[0].Trigger != duty.TriggerSchedule {
		t.Fatalf("first tick: %d reads, runs %+v; want one scheduled run", r.reads, l.runs)
	}
	c.now = start.Add(10 * time.Minute)
	step(t, d)
	if r.reads != 1 {
		t.Fatalf("a tick ten minutes on read again (%d reads)", r.reads)
	}
	c.now = start.Add(duty.RulePassEvery)
	step(t, d)
	if r.reads != 2 {
		t.Fatalf("an hour on: %d reads, want 2", r.reads)
	}
}

// A CONFIG ACTIVATION RUNS THE SETUP CLASSES NOW: a setup that changed is read
// on the next tick, not at the hour.
func TestAConfigActivationRunsTheSetupClassesNow(t *testing.T) {
	t.Parallel()
	l, r, c := &fakeLog{}, &fakeReader{signature: "r1"}, &clock{now: start}
	d := newDuty(l, r, nil, c)
	step(t, d)
	c.now, r.signature = start.Add(5*time.Minute), "r2"
	step(t, d)
	if r.reads != 2 || l.runs[len(l.runs)-1].Trigger != duty.TriggerEvent {
		t.Fatalf("%d reads, last run %+v; want a run on the change", r.reads, l.runs[len(l.runs)-1])
	}
}

// A PRESS ON ANY NODE IS RUN BY THE HOLDER, everything at once, under the
// press's own id; a node that does not hold the lease runs nothing.
func TestARunNowIsRunByTheHolderUnderItsId(t *testing.T) {
	t.Parallel()
	l, r, m, c := &fakeLog{}, &fakeReader{signature: "r1"}, &fakeModel{}, &clock{now: start}
	d := newDuty(l, r, m, c)
	d.Claim = func(context.Context) (bool, error) { return false, nil }
	l.requested = []duty.Request{{Run: "pressed", By: "owner"}}
	step(t, d)
	if r.reads != 0 || len(l.runs) != 0 {
		t.Fatalf("a node without the lease ran: %d reads, %d runs", r.reads, len(l.runs))
	}
	d.Claim = func(context.Context) (bool, error) { return true, nil }
	step(t, d)
	if len(l.runs) != 1 || l.runs[0].ID != "pressed" || l.runs[0].Trigger != duty.TriggerPerson || m.passes != 1 {
		t.Fatalf("runs %+v, model passes %d; want the press run with its model pass", l.runs, m.passes)
	}
}

// THE MODEL PASS RUNS ONCE A DAY AT ITS HOUR on the company clock.
func TestTheModelPassRunsOnceADayAtItsHour(t *testing.T) {
	t.Parallel()
	l, r, m, c := &fakeLog{}, &fakeReader{signature: "r1"}, &fakeModel{}, &clock{now: start}
	d := newDuty(l, r, m, c)
	step(t, d)
	if m.passes != 1 {
		t.Fatalf("the first tick after the hour ran %d passes, want 1", m.passes)
	}
	for _, at := range []time.Duration{time.Hour, 12 * time.Hour, 20 * time.Hour} {
		c.now = start.Add(at)
		step(t, d)
	}
	if m.passes != 1 {
		t.Fatalf("%d passes before the next day's hour, want 1", m.passes)
	}
	c.now = start.Add(21*time.Hour + time.Minute)
	step(t, d)
	if m.passes != 2 {
		t.Fatalf("%d passes after the next day's hour, want 2", m.passes)
	}
}

// A CONDITION THAT ENDED IS RESOLVED WITHOUT ANYBODY ASKING: the duty's next
// run closes a stored finding its detectors no longer observe.
func TestAConditionThatEndedIsResolvedWithoutAnybodyAsking(t *testing.T) {
	t.Parallel()
	f := advisor.Finding{Class: advisor.ClassRoundCap, Severity: advisor.SeverityCosts, Source: advisor.SourceEvidence,
		Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "dev"}, Fix: advisor.Fix{Kind: advisor.FixNone}}
	o := advisor.Observe(f, advisor.Span{From: start.Add(-advisor.Window), To: start}, "")
	body, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	l := &fakeLog{live: []advisor.Stored{{ID: "e1", Version: 3, State: advisor.StateOpen, Observation: o, Body: body}}}
	d := newDuty(l, &fakeReader{signature: "r1"}, nil, &clock{now: start})
	step(t, d)
	if len(l.recorded) != 1 || l.recorded[0].Step != advisor.StepClose || l.recorded[0].Reason != advisor.ReasonConditionCleared {
		t.Fatalf("recorded %+v, want the finding resolved", l.recorded)
	}
	if got := l.runs[0].Count(advisor.StepClose); got != 1 {
		t.Errorf("the run counts %d closes, want 1", got)
	}
}
