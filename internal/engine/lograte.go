package engine

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// HOW FAST EACH LOG GROWS, measured so `log_ceiling_short` can hold a ceiling
// against `min_age` of its own intake: the trim never passes a record younger
// than `min_age`, so a smaller ceiling fills and refuses appends with every
// trim term satisfied.
//
// THE LOG IS ITS OWN MEASUREMENT: `min_age` is at least a day, so the trailing
// day is all still in it, found by [ageFloorOf]'s search on the broker's stored
// instants and priced at the log's average record size. Nothing is sampled or
// persisted, so every node derives the same number from its first tick, and a
// restart or a lease move loses no series.

// logRateWindow is the span a log's intake is measured over: ONE DAY, because
// a company writes in working hours, so a shorter window extrapolates a peak.
// It is config's floor for `min_age`, which keeps every record of the window in
// the log to be counted.
const logRateWindow = 24 * time.Hour

// logRateSettled is how old a log must be before its trailing day is
// measured: two windows, so the day measured lies wholly after the log's first,
// the one a company imports into. A day's age alone would measure the import
// day and page for a log that settles an order of magnitude lower.
const logRateSettled = 2 * logRateWindow

// rateLog is what measuring a log's intake reads, as narrowly as it reads it.
type rateLog interface {
	Stats(ctx context.Context) (jetstream.LogStats, error)
	At(ctx context.Context, seq uint64) (subject string, payload []byte,
		storedAt time.Time, ok bool, err error)
}

// measureRate is one log's intake over the trailing day: a rate (zero
// included), nil when none can honestly be stated (compacted, or in its first
// two days: [logRateSettled]), or an error when the broker could not be read,
// which is not a log that took in nothing.
//
// A COMPACTED log has no rate: it keeps one message per subject, so a burst
// republishing every subject (a model change re-embedding the corpus) replaces
// what it held, and holding it to `min_age` of that burst would page for a log
// that is not growing.
func measureRate(ctx context.Context, log rateLog, replay statelog.ReplayProtocol,
	now time.Time) (*uint64, error) {

	if replay != statelog.ReplayStrict {
		return nil, nil
	}
	stats, err := log.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the stream's state: %w", err)
	}
	return bytesPerDayOf(stats, now, func(seq uint64) (time.Time, bool, error) {
		_, _, storedAt, ok, err := log.At(ctx, seq)
		return storedAt, ok, err
	})
}

// bytesPerDayOf is the arithmetic over anything probed by sequence, apart
// from the broker for [ageFloorOf]'s reason: every case is a table test.
func bytesPerDayOf(stats jetstream.LogStats, now time.Time,
	at func(seq uint64) (time.Time, bool, error)) (*uint64, error) {

	cutoff := now.Add(-logRateWindow)
	if stats.CreatedAt.IsZero() || stats.CreatedAt.After(now.Add(-logRateSettled)) {
		// THE STREAM IS IN ITS FIRST TWO DAYS (a fresh deployment, or a log
		// re-anchored under a running fleet): the day measured would be, or
		// contain, its first.
		return nil, nil
	}
	if stats.Messages == 0 || stats.FirstSeq > stats.LastSeq {
		// AN ESTABLISHED LOG HOLDING NOTHING took in nothing for a day:
		// the trim keeps every record younger than a day.
		return statelog.PerDay(0), nil
	}
	from, err := ageFloorOf(stats.FirstSeq, stats.LastSeq, stats.Messages, cutoff, at)
	if err != nil {
		return nil, err
	}
	// FROM IS THE FIRST RECORD OF THE DAY, or one past the head. A strict
	// log is contiguous, so the day runs from there to the head, clamped
	// against a record purged mid-search. It is priced at the average
	// record size: the broker counts bytes only for the whole stream, and
	// reading the day's records would cost tens of thousands of round trips
	// a tick; the average misstates a rate by a fraction, not a magnitude.
	count := min(stats.LastSeq+1-from, stats.Messages)
	bytes := math.Round(float64(count) * float64(stats.Bytes) / float64(stats.Messages))
	return statelog.PerDay(uint64(bytes)), nil
}

// measureRates records every strict log's intake, on the tick and never on a
// report: the search is a handful of broker round trips per log, and a report
// is built on every operator request and dashboard poll. It runs at once when
// the loop starts, so a booted node has a rate before its first beat; the
// alarm heartbeat compares it every [statelog.AlarmInterval].
//
// A log this tick could not read KEEPS ITS LAST MEASUREMENT, as the applier's
// lag does: dropping it would clear a firing alarm on a broker blip. A log that
// MEASURES NOTHING reads as absent everywhere, its gauge withdrawn rather than
// left holding the last stream's rate, which the report would show as not
// measured.
func (r *retention) measureRates(ctx context.Context, now time.Time) {
	if r.state == nil {
		return
	}
	held := r.state.running()
	logs := make([]measuredLog, 0, len(held))
	for _, running := range held {
		if running == nil || running.log == nil {
			continue
		}
		logs = append(logs, measuredLog{
			domain: running.domain.Name(), log: running.log, replay: running.spec.Replay,
		})
	}
	r.recordRates(ctx, now, logs)
}

// measuredLog is one log a tick measures, named by its domain.
type measuredLog struct {
	domain string
	log    rateLog
	replay statelog.ReplayProtocol
}

// recordRates measures each log and replaces the tick's cache with what it
// found.
func (r *retention) recordRates(ctx context.Context, now time.Time, logs []measuredLog) {
	r.mu.Lock()
	previous := r.rates
	r.mu.Unlock()
	rates := make(map[string]*uint64, len(logs))
	for _, l := range logs {
		rate, err := measureRate(ctx, l.log, l.replay, now)
		if err != nil {
			rates[l.domain] = previous[l.domain]
			if ctx.Err() == nil {
				log.WarnContext(ctx, "statelog_rate_unmeasured", "domain", l.domain,
					"err", err, "detail", "log_ceiling_short compares against "+
						"the last measurement until this one succeeds")
			}
			continue
		}
		rates[l.domain] = rate
		if r.metrics == nil {
			continue
		}
		at := metrics.Attrs{"domain": l.domain}
		if rate == nil {
			r.metrics.Unset(metrics.StatelogLogBytesPerDay, at)
			continue
		}
		r.metrics.Set(metrics.StatelogLogBytesPerDay, float64(*rate), at)
	}
	r.mu.Lock()
	r.rates = rates
	r.mu.Unlock()
}

// rateOf is one log's latest measurement, nil where there is none.
func (r *retention) rateOf(domain string) *uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rates[domain]
}
