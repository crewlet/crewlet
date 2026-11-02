package usage_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/store"
)

// A FIRE ITS SEAT DID NOT RUN READS AS SKIPPED, NOT AS A DISPATCH WITH NO TURN.
//
// The scheduler did send both fires, and its ledger says so. The seat's node
// ran only the later one and recorded the earlier as superseded, under that
// fire's own trace; a history that showed both as fired told a reader the
// health check ran twice, which is the very thing that stopped happening.
func TestASupersededFireIsReportedAsSkipped(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	local := func(h, m int) time.Time { return time.Date(2026, 9, 23, h, m, 0, 0, santiago) }
	ledger := sqlledger.New(db.SQL())
	for _, f := range []struct{ label, trace string }{
		{"20260923T0800", "trace-check-8"},
		{"20260923T1300", "trace-check-13"},
	} {
		if _, err := ledger.Claim(t.Context(), schedule.Run{
			FireKey: schedule.FireKey{Scope: "role", ScopeID: "Sre",
				ScheduleName: "health-check", FireLabel: f.label, TargetHandle: "sre"},
			ScheduledAt: local(8, 0), FiredAt: map[string]time.Time{
				"20260923T0800": local(8, 0), "20260923T1300": local(13, 0)}[f.label],
			Outcome: schedule.OutcomeFired, TraceID: f.trace,
		}); err != nil {
			t.Fatalf("record a fire: %v", err)
		}
	}
	body, err := json.Marshal(types.ScheduledFireSuperseded{
		ScheduleName: "health-check", TargetHandle: "sre",
		TaskID:       "role:sre:health-check:20260923T0800:sre",
		SupersededBy: "role:sre:health-check:20260923T1300:sre",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Events().Append(t.Context(), store.EventRecord{
		ID: "ev-superseded", Type: types.ScheduledFireSuperseded{}.EventType(),
		Source: "engine.scheduler", Category: "task", Time: local(13, 5),
		TraceID: "trace-check-8", Payload: body,
	}); err != nil {
		t.Fatalf("append the superseded record: %v", err)
	}

	p, _ := newPublisher(t, "node-a", db, db, local(23, 50))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	outcomes := map[string]string{}
	for _, row := range dump(t, db)["usage_schedule_runs"] {
		for _, trace := range []string{"trace-check-8", "trace-check-13"} {
			if contains(row, "trace_id="+trace) {
				outcomes[trace] = row
			}
		}
	}
	if row := outcomes["trace-check-8"]; !contains(row, "outcome="+types.ScheduleOutcomeSuperseded) {
		t.Errorf("the superseded fire reads %q, want outcome %s", row, types.ScheduleOutcomeSuperseded)
	}
	if row := outcomes["trace-check-13"]; !contains(row, "outcome=fired") {
		t.Errorf("the fire that ran reads %q, want outcome fired", row)
	}
}
