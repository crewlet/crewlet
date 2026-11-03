package usage_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// A FIRE ITS SEAT DID NOT RUN READS AS SKIPPED, NOT AS A DISPATCH WITH NO TURN,
// wherever the seat ran and whenever it came back.
//
// The scheduler did send both fires, and its ledger says so. The seat's node
// ran only the later one and recorded the earlier as superseded, under that
// fire's own trace; a history that showed both as fired told a reader the
// health check ran twice, which is the very thing that stopped happening.
//
// THE THREE PLACES THE TWO HALVES MEET. On one node; with the seat on another
// data node than the scheduler, whose event log the scheduler's node cannot
// read; and with the seat back three days after the fires, when the fires' own
// day is no longer one the publisher derives. Read from the scheduler's node
// alone, the last two showed the fire as fired for good.
//
// Mutation: drop the EXISTS from usage's fireOutcomeSQL, or derive the
// supersede on the scheduler's node again, and a case names the fire that
// reads as fired.
func TestAFireItsSeatDidNotRunReadsAsSkipped(t *testing.T) {
	t.Parallel()
	fired := func(h int) time.Time { return time.Date(2026, 9, 23, h, 0, 0, 0, santiago) }
	for _, c := range []struct {
		name string
		// apart puts the seat on a second data node.
		apart bool
		// back is when the seat's node recorded the supersede.
		back time.Time
	}{
		{"on one node", false, fired(13).Add(5 * time.Minute)},
		{"the seat on another node", true, fired(13).Add(5 * time.Minute)},
		{"the seat back three days later", false, fired(13).AddDate(0, 0, 3)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			estate := openStore(t)
			scheduler, seat := estate, estate
			if c.apart {
				seat = openStore(t)
			}
			ledger := sqlledger.New(scheduler.SQL())
			for _, f := range []struct {
				hour  int
				trace string
			}{{8, "trace-check-8"}, {13, "trace-check-13"}} {
				if _, err := ledger.Claim(t.Context(), schedule.Run{
					FireKey: schedule.FireKey{Scope: types.ScheduleScopeRole, ScopeID: "Sre",
						ScheduleName: "health-check", FireLabel: fired(f.hour).Format("20060102T1504"),
						TargetHandle: "sre"},
					ScheduledAt: fired(f.hour), FiredAt: fired(f.hour),
					Outcome: schedule.OutcomeFired, TraceID: f.trace,
				}); err != nil {
					t.Fatalf("record a fire: %v", err)
				}
			}
			body, err := json.Marshal(types.ScheduledFireSuperseded{
				ScopeType: types.ScheduleScopeRole, ScopeID: "Sre",
				ScheduleName: "health-check", TargetHandle: "sre",
				TaskID:       "role:sre:health-check:20260923T0800:sre",
				SupersededBy: "role:sre:health-check:20260923T1300:sre",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := seat.Events().Append(t.Context(), store.EventRecord{
				ID: "ev-superseded", Type: types.ScheduledFireSuperseded{}.EventType(),
				Source: "engine.scheduler", Category: "task", Time: c.back,
				TraceID: "trace-check-8", Payload: body,
			}); err != nil {
				t.Fatalf("append the superseded record: %v", err)
			}

			// EACH NODE PUBLISHES ITS OWN DAY, the scheduler's on the
			// fires' and the seat's on the day it came back, into the
			// estate every data node applies.
			p, _ := newPublisher(t, "node-a", scheduler, estate, fired(23))
			if err := p.Flush(t.Context()); err != nil {
				t.Fatalf("flush the scheduler's day: %v", err)
			}
			node := "node-a"
			if c.apart {
				node = "node-b"
			}
			p, _ = newPublisher(t, node, seat, estate, c.back.Add(time.Hour))
			if err := p.Flush(t.Context()); err != nil {
				t.Fatalf("flush the seat's day: %v", err)
			}

			for _, q := range []usage.ScheduleRunsQuery{
				{Limit: 10},
				{ScopeType: "role", ScopeID: "Sre", Name: "health-check", Limit: 10},
			} {
				runs, _, err := usage.ScheduleRuns(t.Context(), estate.Replicated(), q)
				if err != nil {
					t.Fatalf("ScheduleRuns: %v", err)
				}
				outcomes := map[string]string{}
				for _, run := range runs {
					outcomes[run.TraceID] = run.Outcome
				}
				if got := outcomes["trace-check-8"]; got != types.ScheduleOutcomeSuperseded {
					t.Errorf("%+v: the superseded fire reads %q, want %s", q, got,
						types.ScheduleOutcomeSuperseded)
				}
				if got := outcomes["trace-check-13"]; got != scheduleFired {
					t.Errorf("%+v: the fire that ran reads %q, want %s", q, got, scheduleFired)
				}
			}
			// AND A READER ASKING FOR RUNS (the company feed) IS NOT
			// HANDED THE FIRE NOBODY RAN.
			runs, _, err := usage.ScheduleRuns(t.Context(), estate.Replicated(),
				usage.ScheduleRunsQuery{Outcome: scheduleFired, Limit: 10})
			if err != nil {
				t.Fatalf("ScheduleRuns: %v", err)
			}
			if len(runs) != 1 || runs[0].TraceID != "trace-check-13" {
				t.Errorf("the runs are %+v, want the 13:00 fire alone", runs)
			}
		})
	}
}

// scheduleFired is the dispatch ledger's word for a fire it sent, which the
// history's own spelling of it is held to by every case that reads one back.
const scheduleFired = string(schedule.OutcomeFired)

// A FIRE'S TICK REACHES THE HISTORY: when it was due and the label the ledger
// keys it by, which on a catch-up is not when it ran.
//
// Mutation: drop either from the publisher's fold, the apply or the read, and
// the run comes back without it.
func TestAFiresTickReachesTheHistory(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	due := time.Date(2026, 9, 23, 9, 0, 0, 0, santiago)
	ran := due.Add(40 * time.Minute) // a catch-up after a restart
	if _, err := sqlledger.New(db.SQL()).Claim(t.Context(), schedule.Run{
		FireKey: schedule.FireKey{Scope: types.ScheduleScopeRole, ScopeID: "Pm",
			ScheduleName: "standup", FireLabel: "20260923T0900", TargetHandle: "pm"},
		ScheduledAt: due, FiredAt: ran, Outcome: schedule.OutcomeFired, TraceID: "trace-standup",
	}); err != nil {
		t.Fatalf("record a fire: %v", err)
	}
	p, _ := newPublisher(t, "node-a", db, db, ran.Add(time.Hour))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	runs, _, err := usage.ScheduleRuns(t.Context(), db.Replicated(),
		usage.ScheduleRunsQuery{ScopeType: "role", ScopeID: "Pm", Name: "standup", Limit: 1})
	if err != nil {
		t.Fatalf("ScheduleRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d runs, want the one fire", len(runs))
	}
	if !runs[0].ScheduledAt.Equal(due) || runs[0].FireLabel != "20260923T0900" ||
		!runs[0].FiredAt.Equal(ran) {
		t.Errorf("the fire reads due %v (%q), ran %v; want due %v (20260923T0900), ran %v",
			runs[0].ScheduledAt, runs[0].FireLabel, runs[0].FiredAt, due, ran)
	}
}
