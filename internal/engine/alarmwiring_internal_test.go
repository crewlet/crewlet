package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE TWO ALARMS THE IDENTITY DOMAIN BROUGHT FIRE THROUGH THE WIRING A RUNNING
// NODE HAS — not through helpers a case calls directly.
//
// Each has three links that a helper-level test cannot see: the retention loop
// the engine builds has to hold the watch or the measurement, the evaluation it
// runs has to take them, and the report every surface reads has to carry them
// to the table. With any one of those gone, both alarms were dead on a real
// node while every case beside them passed.

// A BINDING TO AN AGENT'S SEAT FIRES `iam_binding_dangling` — through the real
// directory, the real chart view and the real alarm tracker.
func TestTheDanglingBindingAlarmFiresOnARunningNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := bootDirectoryNode(t, nil)
	r := quietRetention(t, e)
	if r.bindings == nil {
		t.Fatal("a node running the identity domain armed no binding watch")
	}

	// A MACHINE BOUND TO THE CEO'S SEAT, which is an agent's: a settled
	// residue, refused 403 on every request it makes.
	bot := uuid.Must(uuid.NewV7()).String()
	writer := e.IAMWriter()
	if _, err := writer.Enrol(ctx, iamdomain.Enrolment{
		PersonID: bot, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "ci:bot", OpID: "op-enrol-bot", Reason: "a pipeline",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if _, err := writer.Claim(ctx, iamdomain.KindSeat, "ceo", bot, "op-bind-bot"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	awaitBinding(t, e, bot)

	t0 := time.Now().UTC()
	r.now = func() time.Time { return t0 }
	r.beat(ctx)
	if alarmed(r.Report(ctx), statelog.KindBindingDangling) {
		t.Fatal("a residue seen once fired before it outlived the stall grace")
	}
	r.now = func() time.Time { return t0.Add(statelog.StallGrace + statelog.AlarmInterval) }
	r.beat(ctx)
	if !alarmed(r.Report(ctx), statelog.KindBindingDangling) {
		t.Fatal("a binding to an agent's seat, a beat past the stall grace, " +
			"raised no iam_binding_dangling")
	}
	if got := alarmGauge(t, e, statelog.KindBindingDangling); got != 1 {
		t.Errorf("the alarm gauge for %s reads %v, want 1",
			statelog.KindBindingDangling, got)
	}
}

// A LOG WHOSE CEILING CANNOT HOLD ITS REPLAY WINDOW FIRES `log_ceiling_short`
// — and the evaluation measures every strict log's intake on the way.
//
// The intake cannot be made real here: a stream is created now, and the
// measurement waits out a log's first two days by design. So the clock is put
// three days ahead — which makes every strict log MEASURED (at nothing, the
// day behind the clock holding none of its records), proving the evaluation
// took the measurement and the report carries it — and then one log's intake
// is the figure a busy company would have. The ceiling and the replay window
// are the node's own.
func TestTheCeilingAlarmFiresOnARunningNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := bootDirectoryNode(t, nil)
	r := quietRetention(t, e)

	r.now = func() time.Time { return time.Now().UTC().Add(72 * time.Hour) }
	r.evaluate(ctx)
	report := r.Report(ctx)
	strict := 0
	for _, d := range report.Domains {
		if d.Replay != statelog.ReplayStrict {
			continue
		}
		strict++
		if d.BytesPerDay == nil {
			t.Errorf("the %s log was not measured by the evaluation", d.Domain)
		}
	}
	if strict == 0 {
		t.Fatal("the report carries no strict log at all")
	}
	if alarmed(report, statelog.KindLogCeilingShort) {
		t.Fatal("logs that took in nothing fired log_ceiling_short")
	}

	// A BUSY COMPANY'S INTAKE on the tracker's log: a tebibyte a day, which
	// no ceiling this node sized holds for the replay window.
	r.mu.Lock()
	r.rates["tracker"] = statelog.PerDay(1 << 40)
	r.mu.Unlock()
	r.beat(ctx)
	var fired statelog.Alarm
	for _, a := range r.Report(ctx).Alarms {
		if a.Kind == statelog.KindLogCeilingShort {
			fired = a
		}
	}
	if fired.Kind == "" {
		t.Fatal("a tebibyte a day on a log sized for gibibytes raised no " +
			"log_ceiling_short")
	}
	if !strings.HasPrefix(fired.Detail, "tracker: ") {
		t.Errorf("the alarm reads %q and does not name the log", fired.Detail)
	}
	if got := alarmGauge(t, e, statelog.KindLogCeilingShort); got != 1 {
		t.Errorf("the alarm gauge for %s reads %v, want 1",
			statelog.KindLogCeilingShort, got)
	}
}

// THE TABLE IS EVALUATED ON THE HEARTBEAT, not on the trim's quarter-hour.
//
// Every alarm there fires at a threshold of a minute or more, and an evaluation
// paced by the trim silently raised each of them to fifteen minutes: the
// binding watch this node runs is observed on every beat, so a second
// observation arriving within one [statelog.AlarmInterval] of the first is the
// heartbeat running.
func TestTheAlarmTableIsEvaluatedOnTheHeartbeat(t *testing.T) {
	t.Parallel()
	e := bootDirectoryNode(t, nil)
	r := e.retention
	if r == nil || r.bindings == nil {
		t.Fatal("this node runs no retention loop or no binding watch")
	}
	first := awaitObservation(t, r, time.Time{}, statelog.AlarmInterval+10*time.Second)
	second := awaitObservation(t, r, first, statelog.AlarmInterval+10*time.Second)
	if gap := second.Sub(first); gap > statelog.AlarmInterval+5*time.Second {
		t.Errorf("two evaluations were %s apart, want one heartbeat", gap)
	}
}

// quietRetention is the node's own retention loop with its ticking stopped, so
// a case can drive the same evaluations on its own clock without racing them.
func quietRetention(t *testing.T, e *Engine) *retention {
	t.Helper()
	r := e.retention
	if r == nil {
		t.Fatal("the node started no retention loop")
	}
	r.stop()
	r.done.Wait()
	if r.alarms == nil {
		t.Fatal("the node's retention loop keeps no alarm tracker")
	}
	return r
}

// awaitBinding waits for this node's identity applier to hold a person's
// binding.
func awaitBinding(t *testing.T, e *Engine, person string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		bindings, err := e.IAM().SeatBindings(t.Context())
		if err == nil {
			for _, b := range bindings {
				if b.Person == person && b.Seat != "" {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the binding never applied (%v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitObservation waits for the binding watch's latest observation to move
// past after, and answers it.
func awaitObservation(t *testing.T, r *retention, after time.Time,
	within time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		r.bindings.mu.Lock()
		at := r.bindings.at
		r.bindings.mu.Unlock()
		if at.After(after) {
			return at
		}
		if time.Now().After(deadline) {
			t.Fatalf("no observation after %s within %s", after, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// alarmed reports whether a report carries an alarm of a kind.
func alarmed(report statelog.Report, kind statelog.Kind) bool {
	for _, a := range report.Alarms {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

// alarmGauge is the process recorder's alarm gauge for one kind.
func alarmGauge(t *testing.T, e *Engine, kind statelog.Kind) float64 {
	t.Helper()
	if e.metrics == nil {
		t.Fatal("the node has no recorder")
	}
	for _, s := range e.metrics.Read() {
		if s.Name == metrics.AlarmActive && s.Attrs["kind"] == string(kind) {
			return s.Value
		}
	}
	t.Fatalf("no %s series for %s", metrics.AlarmActive, kind)
	return -1
}
