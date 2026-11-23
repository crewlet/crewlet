package advisordomain_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
)

// observing is a duty's reading of a company that observes what it holds.
type observing struct{ observed []advisor.Observation }

func (o observing) Observe(context.Context, time.Time) (duty.Observed, error) {
	return duty.Observed{Observations: o.observed, Unread: []string{}}, nil
}

func (observing) Signature(context.Context) (string, error) { return "r1", nil }

func (observing) Week(context.Context, time.Time, time.Time) ([]advisor.Metric, []string, error) {
	return []advisor.Metric{{Key: "turns", Label: "Turns", Value: 12, Unit: advisor.UnitTurns}}, []string{}, nil
}

// Holds holds every premise: no finding here states one.
func (observing) Holds(context.Context, advisor.Premise) (bool, error) { return true, nil }

// THE DUTY'S RECORDS ARE ONES THE LOG KEEPS: one tick of the duty, written
// through the domain's own writer, stores a finding about the whole company
// (whose subject is the only one of its kind, so it has no id) beside a seat's,
// and records the run with its start and its end, and on a Monday morning the
// week before's report. Mutation: refuse an empty subject id, or record a run
// with no end, and the log keeps neither.
func TestTheDutysRecordsAreOnesTheLogKeeps(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	window := advisor.Span{From: writerClock.Add(-advisor.Window), To: writerClock}
	company := advisor.Finding{Class: advisor.ClassPickupStillWaiting, Severity: advisor.SeverityNote,
		Actor: advisor.ActorEngine, Source: advisor.SourceEvidence, Area: advisor.AreaWork,
		Subject: advisor.Subject{Kind: advisor.SubjectCompany}, Title: "Agents picked up work and moved none of it.",
		Item: "4 of 10 pickups", Fix: advisor.Fix{Kind: advisor.FixNone, Options: []advisor.Option{}}}
	seat := company
	seat.Class, seat.Subject, seat.Item = advisor.ClassNoopShare, advisor.Subject{Kind: advisor.SubjectSeat, Seat: "dev"}, "8 of 20 turns"
	d := &duty.Duty{Log: duty.LogOf(n.reader(), n.writer, n.now),
		Read: observing{observed: []advisor.Observation{advisor.Observe(company, window, "r1"),
			advisor.Observe(seat, window, "r1")}},
		NewID: uuid.NewString, Now: n.now, Zone: func() *time.Location { return time.UTC }}
	if err := d.Step(t.Context()); err != nil {
		t.Fatalf("the tick failed: %v", err)
	}
	r := n.reader()
	live, err := r.NonClosed(t.Context(), stale)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, row := range live {
		kinds[row.SubjectKind] = row.SubjectID
	}
	if len(live) != 2 || kinds["company"] != "" || kinds["seat"] != "dev" {
		t.Errorf("the log keeps %+v, want the company's finding and the seat's", kinds)
	}
	run, found, err := r.LatestRun(t.Context(), stale)
	if err != nil || !found || run.Status != advisordomain.RunDone ||
		!run.StartedAt.Equal(writerClock) || !run.EndedAt.Equal(writerClock) {
		t.Errorf("the latest run is %+v, found %v (%v), want the tick's run recorded done, begun and ended at the writer's clock", run, found, err)
	}
	// A MONDAY MORNING'S TICK writes the week before's report, which the log keeps.
	week, _, _, _ := advisor.ReportWeek(writerClock, time.UTC)
	if _, written, err := r.Report(t.Context(), week, stale); err != nil || !written {
		t.Errorf("the report of %s: written %v (%v), want it kept", week, written, err)
	}
}

// A DISMISSAL REOPENED RETURNS ON THE NEXT RUN: a person's reopen lifts the
// dismissal's hold, so the duty's next run opens the condition that still
// holds as a new episode.
func TestADismissalReopenedReturnsOnTheNextRun(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	window := advisor.Span{From: writerClock.Add(-advisor.Window), To: writerClock}
	f := advisor.Finding{Class: advisor.ClassNoopShare, Severity: advisor.SeverityNote, Actor: advisor.ActorEngine,
		Source: advisor.SourceEvidence, Area: advisor.AreaWork, Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "dev"},
		Item: "8 of 20 turns", Fix: advisor.Fix{Kind: advisor.FixNone, Options: []advisor.Option{}}}
	d := &duty.Duty{Log: duty.LogOf(n.reader(), n.writer, n.now),
		Read:  observing{observed: []advisor.Observation{advisor.Observe(f, window, "r1")}},
		NewID: uuid.NewString, Now: n.now, Zone: func() *time.Location { return time.UTC }}
	if err := d.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	live, err := n.reader().NonClosed(t.Context(), stale)
	if err != nil || len(live) != 1 {
		t.Fatalf("live %+v (%v), want the finding open", live, err)
	}
	first := live[0]
	n.move(personCall("ada"), first.Key, dismissal(first.ID, first.Severity, time.Time{}))
	if _, err := n.writer.Release(t.Context(), personCall("ada"), first.Key, n.key(first.Key).Version); err != nil {
		t.Fatalf("release: %v", err)
	}
	n.advance(duty.RulePassEvery)
	if err := d.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	live, err = n.reader().NonClosed(t.Context(), stale)
	if err != nil || len(live) != 1 || live[0].ID == first.ID || live[0].SeenBefore != 1 {
		t.Fatalf("live after the reopen %+v (%v), want a new episode that was seen before", live, err)
	}
}

// A RUN WHOSE HOLDER STOPPED IS CLOSED ON THE NEXT TICK, through the log: the
// holder's tick records it interrupted from its first line to its last, and a
// record of it written again (an earlier attempt that landed unanswered, or
// the stopped holder's own) is taken as written. Mutation: answer the second
// record's refusal, and the duty keeps it to write again on every tick.
func TestARunWhoseHolderStoppedIsClosedOnTheNextTick(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	orphan := uuid.NewString()
	for i := range 2 {
		if _, err := n.writer.Progress(t.Context(), engineCall(), advisordomain.RunProgress{Run: orphan,
			At: writerClock.Add(-48*time.Hour + time.Duration(i)*time.Minute), Text: "reading capacity"}); err != nil {
			t.Fatal(err)
		}
	}
	log := duty.LogOf(n.reader(), n.writer, n.now)
	d := &duty.Duty{Log: log, Read: observing{}, NewID: uuid.NewString, Now: n.now,
		Zone: func() *time.Location { return time.UTC }}
	if err := d.Step(t.Context()); err != nil {
		t.Fatalf("the tick failed: %v", err)
	}
	run, found, err := n.reader().Run(t.Context(), orphan, stale)
	if err != nil || !found || run.Status != advisordomain.RunInterrupted || run.Kind != advisordomain.RunKindDeep ||
		!run.StartedAt.Equal(writerClock.Add(-48*time.Hour)) || !run.EndedAt.Equal(writerClock.Add(-48*time.Hour+time.Minute)) {
		t.Fatalf("the orphan = %+v, found %v (%v), want it recorded interrupted over its lines", run, found, err)
	}
	if err := log.RecordRun(t.Context(), duty.Run{ID: orphan, Trigger: duty.TriggerSchedule, StartedAt: writerClock,
		EndedAt: writerClock, Status: duty.RunDone, Kind: advisordomain.RunKindDeep}); err != nil {
		t.Errorf("a second record of a recorded run answered %v, want it taken as written", err)
	}
}
