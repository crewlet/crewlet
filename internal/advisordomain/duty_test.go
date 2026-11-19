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

// THE DUTY'S RECORDS ARE ONES THE LOG KEEPS: one tick of the duty, written
// through the domain's own writer, stores a finding about the whole company
// (whose subject is the only one of its kind, so it has no id) beside a seat's,
// and records the run with its start and its end. Mutation: refuse an empty
// subject id, or record a run with no end, and the log keeps neither.
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
	if err != nil || !found || run.Status != advisordomain.RunDone || run.EndedAt.Before(run.StartedAt) {
		t.Errorf("the latest run is %+v, found %v (%v), want the tick's run recorded done", run, found, err)
	}
}
