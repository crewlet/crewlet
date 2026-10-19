package objectives_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// beaconAt is a beacon over a recording meter on a test clock.
func beaconAt(t *testing.T) (*objectives.Beacon, *objectives.Meter, *recorded, *clock) {
	t.Helper()
	c := newClock()
	rec := newRecorded()
	m := objectives.NewMeter(rec, c.now)
	b, err := objectives.NewBeacon(m, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return b, m, rec, c
}

// report is a plausible report of frame by viewer, minted a minute before now.
func report(c *clock, viewer, frame string, outcome objectives.ReadyOutcome, elapsed time.Duration) objectives.ReadyReport {
	return objectives.ReadyReport{Viewer: viewer, Frame: frame, Minted: c.now().Add(-time.Minute),
		Expires: c.now().Add(59 * time.Minute), Outcome: outcome, Elapsed: elapsed}
}

// AN ADMITTED REPORT IS ONE EVENT: the histogram by outcome and one
// artifact_ready event, good or bad by the bound.
func TestAReadyReportIsOneEvent(t *testing.T) {
	t.Parallel()
	b, _, rec, c := beaconAt(t)
	if err := b.Accept(report(c, "jane", "f1", objectives.ReadyOK, 800*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := b.Accept(report(c, "jane", "f2", objectives.ReadyOK, 4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := b.Accept(report(c, "jane", "f3", objectives.ReadyFailed, time.Second)); err != nil {
		t.Fatal(err)
	}
	ready := rec.observed[key(metrics.ArtifactsReadyDuration, metrics.Attrs{"outcome": "ready"})]
	failed := rec.observed[key(metrics.ArtifactsReadyDuration, metrics.Attrs{"outcome": "failed"})]
	if len(ready) != 2 || len(failed) != 1 || ready[0] != 800*time.Millisecond {
		t.Errorf("ready %v failed %v", ready, failed)
	}
	good := rec.adds[key(metrics.ObjectivesEvents, metrics.Attrs{"objective": "artifact_ready", "outcome": "good"})]
	bad := rec.adds[key(metrics.ObjectivesEvents, metrics.Attrs{"objective": "artifact_ready", "outcome": "bad"})]
	if good != 1 || bad != 2 {
		t.Errorf("good %d bad %d: a slow ready and a failure are both bad, want 1 and 2", good, bad)
	}
}

// EACH FRAME REPORTS ONCE, so one page cannot be counted twice.
func TestEachFrameReportsOnce(t *testing.T) {
	t.Parallel()
	b, _, rec, c := beaconAt(t)
	if err := b.Accept(report(c, "jane", "f1", objectives.ReadyOK, time.Second)); err != nil {
		t.Fatal(err)
	}
	err := b.Accept(report(c, "jane", "f1", objectives.ReadyFailed, time.Second))
	if !errors.Is(err, objectives.ErrReported) {
		t.Errorf("a second report of one frame answered %v, want ErrReported", err)
	}
	if n := len(rec.observed[key(metrics.ArtifactsReadyDuration, metrics.Attrs{"outcome": "failed"})]); n != 0 {
		t.Errorf("the refused report was counted %d times", n)
	}
}

// A REPORT THAT COULD NOT HAVE BEEN MEASURED IS REFUSED and counted nowhere.
func TestAnImplausibleReportIsRefused(t *testing.T) {
	t.Parallel()
	b, _, rec, c := beaconAt(t)
	ended := report(c, "jane", "f5", objectives.ReadyOK, time.Second)
	ended.Expires = c.now()
	for name, r := range map[string]objectives.ReadyReport{
		"longer than the frame has existed": report(c, "jane", "f1", objectives.ReadyOK, 2*time.Minute),
		"a negative time":                   report(c, "jane", "f2", objectives.ReadyOK, -time.Second),
		"an outcome outside the set":        report(c, "jane", "f3", "rendered", time.Second),
		"no viewer":                         report(c, " ", "f4", objectives.ReadyOK, time.Second),
		"a ticket that has ended":           ended,
	} {
		if err := b.Accept(r); !errors.Is(err, objectives.ErrImplausible) {
			t.Errorf("%s: %v, want ErrImplausible", name, err)
		}
	}
	if len(rec.observed) != 0 || len(rec.adds) != 0 {
		t.Errorf("refused reports were counted: %v %v", rec.observed, rec.adds)
	}
	// A refused report does not spend the frame: its real report is admitted.
	if err := b.Accept(report(c, "jane", "f1", objectives.ReadyOK, time.Second)); err != nil {
		t.Errorf("the frame's plausible report after an implausible one: %v", err)
	}
}

// ONE CREDENTIAL IS HELD TO ITS RATE, and another is not charged for it.
func TestOneCredentialIsHeldToItsRate(t *testing.T) {
	t.Parallel()
	b, _, _, c := beaconAt(t)
	for i := range objectives.ReadyReportsBurst {
		if err := b.Accept(report(c, "jane", fmt.Sprintf("f%d", i), objectives.ReadyOK, time.Second)); err != nil {
			t.Fatalf("report %d inside the burst: %v", i, err)
		}
	}
	err := b.Accept(report(c, "jane", "past", objectives.ReadyOK, time.Second))
	var throttled *objectives.Throttled
	if !errors.As(err, &throttled) || throttled.RetryAfter <= 0 || throttled.RetryAfter > time.Second {
		t.Fatalf("the report past the burst: %v, want throttled for at most a second", err)
	}
	if err := b.Accept(report(c, "bob", "bob1", objectives.ReadyOK, time.Second)); err != nil {
		t.Errorf("another credential was charged for jane's reports: %v", err)
	}
	c.advance(time.Second)
	if err := b.Accept(report(c, "jane", "past", objectives.ReadyOK, time.Second)); err != nil {
		t.Errorf("a second later the bucket holds one again: %v", err)
	}
}

// THE BEACON NEEDS A METER.
func TestTheBeaconNeedsAMeter(t *testing.T) {
	t.Parallel()
	if _, err := objectives.NewBeacon(nil, nil); err == nil {
		t.Error("a beacon with no meter was built")
	}
}
