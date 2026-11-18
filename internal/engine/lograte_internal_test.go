package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// rateNow is the instant every rate case is measured at.
var rateNow = time.Date(2031, 4, 9, 12, 0, 0, 0, time.UTC)

// hourlyLog is a strict log of `records` records one hour apart, the newest
// stored `newestAgo` before rateNow, each a kilobyte on the broker.
func hourlyLog(records uint64, newestAgo time.Duration) (jetstream.LogStats, *probe) {
	stored := map[uint64]time.Time{}
	for seq := uint64(1); seq <= records; seq++ {
		stored[seq] = rateNow.Add(-newestAgo - time.Duration(records-seq)*time.Hour)
	}
	return jetstream.LogStats{
		FirstSeq: 1, LastSeq: records, Messages: records, Bytes: records * 1024,
		MaxBytes: 1 << 30, CreatedAt: rateNow.Add(-30 * 24 * time.Hour),
	}, &probe{stored: stored}
}

// THE DAY'S INTAKE IS THE DAY'S RECORDS AT THE LOG'S OWN RECORD SIZE, counted
// out of the log itself: the records at or after the cutoff, the one stored
// exactly a day ago included. One more or one fewer is a rate off by an hour's
// writing on every log.
func TestADaysIntakeIsTheRecordsOfTheDayAtTheLogsOwnRecordSize(t *testing.T) {
	t.Parallel()
	stats, p := hourlyLog(100, 0)
	got, err := bytesPerDayOf(stats, rateNow, p.at)
	if err != nil {
		t.Fatalf("bytesPerDayOf: %v", err)
	}
	// Records stored at now−24h … now: twenty-five of them, the one exactly
	// a day old included, at the log's own kilobyte each.
	if got == nil || *got != 25*1024 {
		t.Fatalf("measured %v, want %d — twenty-five hourly records of a "+
			"kilobyte", deref(got), 25*1024)
	}
	// AND BY BISECTION: a year-five log is a quarter of a million records,
	// measured on every node every tick.
	if len(p.asked) > 10 {
		t.Errorf("the measurement probed %d records of a hundred", len(p.asked))
	}
}

// A LOG THAT TOOK IN NOTHING MEASURES ZERO — a real value, not an absent one —
// whether its newest record is older than the day or the trim has emptied it:
// a record written in the last day would still be in the log.
func TestALogThatTookInNothingMeasuresZero(t *testing.T) {
	t.Parallel()
	stats, p := hourlyLog(100, 3*24*time.Hour)
	got, err := bytesPerDayOf(stats, rateNow, p.at)
	if err != nil || got == nil || *got != 0 {
		t.Errorf("a log idle for three days measured %v (err %v), want a "+
			"measured zero", deref(got), err)
	}

	empty := jetstream.LogStats{
		FirstSeq: 101, LastSeq: 100, Messages: 0, MaxBytes: 1 << 30,
		CreatedAt: rateNow.Add(-30 * 24 * time.Hour),
	}
	got, err = bytesPerDayOf(empty, rateNow, func(uint64) (time.Time, bool, error) {
		t.Fatal("an empty log was probed")
		return time.Time{}, false, nil
	})
	if err != nil || got == nil || *got != 0 {
		t.Errorf("an emptied log measured %v (err %v), want a measured zero",
			deref(got), err)
	}
}

// A LOG IN ITS FIRST TWO DAYS MEASURES NOTHING, which the alarm is silent on,
// and the day it does measure never contains the import day it was created on.
// A log a day and ten minutes old, ten thousand kilobyte records imported in
// its first two hours and a hundred written since, would otherwise measure about
// ninety times its steady intake and fire log_ceiling_short on every node. The
// control is the same company a day later, measuring its steady intake.
func TestALogInItsFirstTwoDaysMeasuresNothing(t *testing.T) {
	t.Parallel()
	stats, p := importedLog(rateNow.Add(-(24*time.Hour + 10*time.Minute)))
	got, err := bytesPerDayOf(stats, rateNow, p.at)
	if err != nil || got != nil {
		t.Errorf("a log a day and ten minutes old, its import inside the day "+
			"behind it, measured %v (err %v), want nothing", deref(got), err)
	}

	// THE CONTROL: the same shape two days on, measured over a day that
	// holds none of the import.
	stats, p = importedLog(rateNow.Add(-72 * time.Hour))
	got, err = bytesPerDayOf(stats, rateNow, p.at)
	if err != nil || got == nil {
		t.Fatalf("a settled log measured %v (err %v)", deref(got), err)
	}
	if *got > 200*1024 {
		t.Errorf("a settled log measured %d bytes a day, want its steady intake "+
			"of about a hundred kilobyte records rather than the import", *got)
	}

	// AND A STREAM THAT DID NOT SAY WHEN IT WAS CREATED is not one that
	// is old enough.
	stats.CreatedAt = time.Time{}
	if got, _ := bytesPerDayOf(stats, rateNow, p.at); got != nil {
		t.Errorf("a stream with no creation instant measured %v", *got)
	}
}

// importedLog is a strict log created at created: ten thousand kilobyte
// records imported over its first two hours, then a hundred a day, evenly, up
// to rateNow.
func importedLog(created time.Time) (jetstream.LogStats, *probe) {
	stored := map[uint64]time.Time{}
	seq := uint64(0)
	for i := range 10_000 {
		seq++
		stored[seq] = created.Add(time.Duration(i) * (2 * time.Hour) / 10_000)
	}
	steady := created.Add(2 * time.Hour)
	for at := steady; at.Before(rateNow); at = at.Add(24 * time.Hour / 100) {
		seq++
		stored[seq] = at
	}
	return jetstream.LogStats{
		FirstSeq: 1, LastSeq: seq, Messages: seq, Bytes: seq * 1024,
		MaxBytes: 1 << 30, CreatedAt: created,
	}, &probe{stored: stored}
}

// A GAUGE FOLLOWS A MEASUREMENT THAT BECAME UNKNOWN: a log re-anchored under a
// running fleet starts its first two days again, so the report shows it
// unmeasured and the gauge is withdrawn rather than giving a second answer. A
// log that merely could not be read keeps its last measurement everywhere.
func TestAGaugeFollowsAMeasurementThatBecameUnknown(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	r := &retention{metrics: rec}
	stats, p := hourlyLog(100, 0)
	iam := &fakeRateLog{stats: stats, p: p}
	logs := []measuredLog{{domain: "iam", log: iam, replay: statelog.ReplayStrict}}
	r.recordRates(t.Context(), rateNow, logs)
	if got := gaugeValue(t, rec, "iam"); got != 25*1024 {
		t.Fatalf("precondition: the gauge reads %v", got)
	}

	iam.stats.CreatedAt = rateNow.Add(-time.Hour)
	r.recordRates(t.Context(), rateNow, logs)
	if got := r.rateOf("iam"); got != nil {
		t.Fatalf("a re-anchored log still holds a rate of %d", *got)
	}
	for _, s := range rec.Read() {
		if s.Name == metrics.StatelogLogBytesPerDay && s.Attrs["domain"] == "iam" {
			t.Errorf("the gauge still reads %v for a log the report shows as "+
				"unmeasured", s.Value)
		}
	}
}

// AND THE COVERAGE GAUGE, on the same terms: a scan that failed leaves the
// report with no coverage, so it leaves the collector with none either.
func TestTheCoverageGaugeFollowsAScanThatFailed(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	var fail error
	r := &retention{metrics: rec, coverage: func(context.Context) (float64, bool, error) {
		return 0.97, true, fail
	}}
	r.measureCoverage(t.Context())
	fail = errors.New("the corpus could not be scanned")
	r.measureCoverage(t.Context())
	if r.semanticCoverage() != nil {
		t.Fatal("a failed scan left a coverage in the report")
	}
	for _, s := range rec.Read() {
		if s.Name == metrics.TrackerVectorCoverage {
			t.Errorf("the coverage gauge still reads %v after a scan that failed",
				s.Value)
		}
	}
}

// AN UNREADABLE LOG IS NOT AN IDLE ONE: a zero taken from a failed probe would
// be a measured "nothing written" on a broker that could not be asked.
func TestAnUnreadableLogIsAnErrorNotAZero(t *testing.T) {
	t.Parallel()
	stats, _ := hourlyLog(100, 0)
	want := errors.New("broker unreachable")
	got, err := bytesPerDayOf(stats, rateNow, (&probe{fail: want}).at)
	if !errors.Is(err, want) || got != nil {
		t.Errorf("a failed probe gave %v, %v — want the probe's own failure",
			deref(got), err)
	}
}

// fakeRateLog is a log for [measureRate]: a stats answer and a probe.
type fakeRateLog struct {
	stats    jetstream.LogStats
	statsErr error
	p        *probe
	read     int
}

func (f *fakeRateLog) Stats(context.Context) (jetstream.LogStats, error) {
	f.read++
	return f.stats, f.statsErr
}

func (f *fakeRateLog) At(_ context.Context, seq uint64) (string, []byte, time.Time, bool, error) {
	stored, ok, err := f.p.at(seq)
	return "", nil, stored, ok, err
}

// A COMPACTED LOG HAS NO RATE IN THIS SENSE, and is not asked for one: a
// model change re-embedding the corpus replaces what it holds rather than
// adding to it, so holding its ceiling to min_age of that would page for a log
// that is not growing ([measureRate]).
func TestACompactedLogIsNeverMeasured(t *testing.T) {
	t.Parallel()
	stats, p := hourlyLog(100, 0)
	l := &fakeRateLog{stats: stats, p: p}
	got, err := measureRate(t.Context(), l, statelog.ReplayCompacted, rateNow)
	if err != nil || got != nil {
		t.Errorf("a compacted log measured %v (err %v), want nothing", deref(got), err)
	}
	if l.read != 0 {
		t.Errorf("a compacted log's stream was read %d time(s) for a rate it "+
			"cannot have", l.read)
	}
	// AND THE CONTROL: the same records on a strict log measure.
	if got, err := measureRate(t.Context(), l, statelog.ReplayStrict, rateNow); err != nil || got == nil {
		t.Errorf("a strict log measured %v (err %v)", deref(got), err)
	}
}

// A TICK THAT CANNOT READ A LOG KEEPS WHAT THE LAST ONE MEASURED, and publishes
// what it did measure: dropping the figure on a broker blip would clear a firing
// `log_ceiling_short` and raise it again a tick later. A node that has not yet
// ticked holds nothing, which the alarm is silent on.
func TestATickThatCannotReadALogKeepsItsLastMeasurement(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	r := &retention{metrics: rec}
	if got := r.rateOf("iam"); got != nil {
		t.Fatalf("a node that has never ticked holds a rate of %d", *got)
	}

	stats, p := hourlyLog(100, 0)
	iam := &fakeRateLog{stats: stats, p: p}
	vectors := &fakeRateLog{stats: stats, p: p}
	logs := []measuredLog{
		{domain: "iam", log: iam, replay: statelog.ReplayStrict},
		{domain: "vectors", log: vectors, replay: statelog.ReplayCompacted},
	}
	r.recordRates(t.Context(), rateNow, logs)
	if got := r.rateOf("iam"); got == nil || *got != 25*1024 {
		t.Fatalf("the first tick measured %v", deref(got))
	}
	if got := r.rateOf("vectors"); got != nil {
		t.Errorf("a compacted log holds a rate of %d", *got)
	}
	if got := gaugeValue(t, rec, "iam"); got != 25*1024 {
		t.Errorf("the gauge reads %v, want the measurement", got)
	}

	iam.statsErr = errors.New("broker unreachable")
	r.recordRates(t.Context(), rateNow.Add(statelog.TrimInterval), logs)
	if got := r.rateOf("iam"); got == nil || *got != 25*1024 {
		t.Errorf("a tick that could not read the log left %v, want the last "+
			"measurement standing", deref(got))
	}
}

func gaugeValue(t *testing.T, rec *metrics.Recorder, domain string) float64 {
	t.Helper()
	for _, s := range rec.Read() {
		if s.Name == metrics.StatelogLogBytesPerDay && s.Attrs["domain"] == domain {
			return s.Value
		}
	}
	t.Fatalf("no %s series for %s", metrics.StatelogLogBytesPerDay, domain)
	return -1
}

// deref renders a measurement for a failure message.
func deref(v *uint64) any {
	if v == nil {
		return "nothing"
	}
	return *v
}
