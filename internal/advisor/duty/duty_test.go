package duty_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
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
	// reports is every week's report written, tally what Tally answers, and
	// order what was written, a run or a report, in the order it was.
	reports map[string]advisor.Report
	tally   advisor.Tally
	order   []string
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
	l.order = append(l.order, "run")
	return nil
}

func (l *fakeLog) Report(_ context.Context, week string) (*advisor.Report, error) {
	r, ok := l.reports[week]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func (l *fakeLog) Tally(context.Context, time.Time, time.Time) (advisor.Tally, error) {
	return l.tally, nil
}

func (l *fakeLog) WriteReport(_ context.Context, r advisor.Report) error {
	if l.reports == nil {
		l.reports = map[string]advisor.Report{}
	}
	l.reports[r.Week] = r
	l.order = append(l.order, "report:"+r.Week)
	return nil
}

// fakeReader observes what it holds, behind a signature a test moves.
type fakeReader struct {
	observed  []advisor.Observation
	signature string
	reads     int
	metrics   []advisor.Metric
	weeks     []advisor.Span
}

func (r *fakeReader) Observe(context.Context, time.Time) (duty.Observed, error) {
	r.reads++
	return duty.Observed{Observations: r.observed}, nil
}

func (r *fakeReader) Signature(context.Context) (string, error) { return r.signature, nil }

// Week answers the metrics it holds, and records the period it was asked for.
func (r *fakeReader) Week(_ context.Context, from, to time.Time) ([]advisor.Metric, []string, error) {
	r.weeks = append(r.weeks, advisor.Span{From: from, To: to})
	return r.metrics, []string{}, nil
}

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

// openFinding is an open finding of severity on seat, opened at opened.
func openFinding(t *testing.T, id string, severity advisor.Severity, seat string, opened time.Time) advisor.Stored {
	t.Helper()
	f := advisor.Finding{Class: advisor.ClassRoundCap, Severity: severity, Source: advisor.SourceEvidence,
		Title: "Runs ended on their cap.", Item: "2 runs", Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: seat},
		Fix: advisor.Fix{Kind: advisor.FixNone}}
	o := advisor.Observe(f, advisor.Span{From: opened.Add(-advisor.Window), To: opened}, "")
	return advisor.Stored{ID: id, State: advisor.StateOpen, OpenedAt: opened, Observation: o}
}

// THE WEEKLY REPORT COUNTS WHAT THE WEEK OPENED, RESOLVED AND APPLIED: on the
// Monday after it, after that hour's model pass, once; with the week's numbers
// beside the week before's and the open findings that cost the most, the
// most severe first. Mutation: write it before the hour, write it again on
// the next tick, or drop the week before's numbers, and a check fails.
func TestTheWeeklyReportCountsWhatTheWeekOpenedResolvedAndApplied(t *testing.T) {
	t.Parallel()
	monday := time.Date(2030, 1, 14, 0, 0, 0, 0, time.UTC)
	l := &fakeLog{tally: advisor.Tally{Opened: 4, Resolved: 2, Applied: 1, Dismissed: 1},
		reports: map[string]advisor.Report{"2030-W01": {Week: "2030-W01",
			Metrics: []advisor.Metric{{Key: "turns_nothing", Value: 25}}}}}
	for i := range 6 {
		l.live = append(l.live, openFinding(t, "costs-"+strconv.Itoa(i), advisor.SeverityCosts, "dev",
			monday.Add(-time.Duration(6-i)*time.Hour)))
	}
	l.live = append(l.live, openFinding(t, "blocks", advisor.SeverityBlocks, "qa", monday.Add(-time.Hour)))
	r := &fakeReader{signature: "r1", metrics: []advisor.Metric{{Key: "turns_nothing", Label: "Turns that committed nothing",
		Value: 40, Unit: advisor.UnitTurns}}}
	c := &clock{now: monday.Add(5*time.Hour + 59*time.Minute)}
	d := newDuty(l, r, &fakeModel{}, c)
	step(t, d)
	if _, written := l.reports["2030-W02"]; written {
		t.Fatal("the report was written before its hour")
	}
	c.now = monday.Add(6*time.Hour + time.Minute)
	step(t, d)
	report, written := l.reports["2030-W02"]
	if !written {
		t.Fatalf("no report at its hour; wrote %v", l.order)
	}
	if last := l.order[len(l.order)-2:]; last[0] != "run" || last[1] != "report:2030-W02" {
		t.Errorf("wrote %v, want the hour's run before the report", l.order)
	}
	if !report.From.Equal(monday.AddDate(0, 0, -7)) || !report.To.Equal(monday) ||
		len(r.weeks) != 1 || !r.weeks[0].From.Equal(report.From) || !r.weeks[0].To.Equal(report.To) {
		t.Errorf("the report covers %s to %s, read %+v; want the week before Monday", report.From, report.To, r.weeks)
	}
	if report.Tally != l.tally {
		t.Errorf("the report counts %+v, want %+v", report.Tally, l.tally)
	}
	if m := report.Metrics; len(m) != 1 || m[0].Value != 40 || m[0].Was == nil || *m[0].Was != 25 {
		t.Errorf("the metrics are %+v, want 40 beside last week's 25", m)
	}
	var top []string
	for _, f := range report.Top {
		top = append(top, f.ID)
	}
	if want := []string{"blocks", "costs-0", "costs-1", "costs-2", "costs-3"}; strings.Join(top, ",") != strings.Join(want, ",") {
		t.Errorf("the top is %v, want %v", top, want)
	}
	writes := len(l.order)
	c.now = monday.Add(7 * time.Hour)
	step(t, d)
	again := newDuty(l, r, nil, c)
	step(t, again)
	if n := strings.Count(strings.Join(l.order[writes:], " "), "report:"); n != 0 {
		t.Errorf("the week's report was written %d more times", n)
	}
}
