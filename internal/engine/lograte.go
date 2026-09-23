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

// HOW FAST EACH LOG GROWS, measured, so a ceiling can be held against its own
// replay window.
//
// # What the measurement is
//
// A log keeps every record younger than `min_age` — the age term never lets
// the trim past one — so its ceiling has to hold `min_age` of its own intake
// or it fills and refuses appends with every trim term satisfied. That is the
// comparison `log_ceiling_short` makes, and it needs the intake as a number.
// Nothing measured one: the framework knew each log's size and ceiling and
// never how fast the first approached the second.
//
// THE LOG IS ITS OWN MEASUREMENT. The records of the trailing day are all
// still in it — `min_age` is at least a day, a floor config refuses to go
// below — and every record carries the broker's own stored instant, so the
// day's intake is the records at or after now − 24 h, found by the same binary
// search the age term already runs ([ageFloorOf]), times the average size of
// what the log holds (its byte count over its record count). Nothing is
// sampled into memory and nothing is persisted: every node reading the same
// stream derives the same number, a node that has just restarted measures on
// its first tick, and there is no series of observations for a lease move or a
// restart to lose.
//
// # What it deliberately does not measure
//
// A COMPACTED log has no rate in this sense: it keeps one message per subject,
// so its size follows the number of subjects rather than the time it has been
// written for, and a burst that republishes every subject — a model change
// re-embedding the corpus — replaces what it held rather than adding to it.
// Holding its ceiling against `min_age` of that burst would page an operator
// for a log that is not growing. It measures nothing, which the reading treats
// as unmeasured.
//
// A LOG YOUNGER THAN THE DAY measures nothing either. Its first hours are the
// ones a company imports into, and a day extrapolated from an import is a
// ceiling alarm on day one for a log that settles an order of magnitude lower.
// A company's writing is also diurnal, so a rate taken over its working hours
// is several times the one a whole day averages to.
//
// # Why the average record size and not the exact bytes
//
// The broker answers a byte count for the whole stream and a stored instant per
// record, and nothing for a range. The exact figure would be a read of every
// record of the day — tens of thousands of round trips a tick on a busy log —
// against the handful the search costs. The average is over the records the log
// holds, a window of at least a day and usually `min_age`, so it describes the
// same mix of records the day was written in; what it can misstate is a day
// whose records are unusually large or small against the week before it, which
// is a fraction of the rate rather than an order of magnitude of it.

// logRateWindow is the span a log's intake is measured over.
//
// ONE DAY, because the rate the alarm needs is per day and the one honest way
// to measure a daily rate is over a whole day: a company writes to its logs in
// working hours, so any shorter window extrapolates a peak. It is also the
// floor config holds `min_age` to, which is what guarantees every record of
// the window is still in the log to be counted.
const logRateWindow = 24 * time.Hour

// rateLog is what measuring a log's intake reads, as narrowly as it reads it.
type rateLog interface {
	Stats(ctx context.Context) (jetstream.LogStats, error)
	At(ctx context.Context, seq uint64) (subject string, payload []byte,
		storedAt time.Time, ok bool, err error)
}

// measureRate is one log's intake over the trailing day, or nil when it has
// none this measurement can honestly state.
//
// THREE ANSWERS: a measured rate (zero included — an established log that
// took in nothing yesterday is the most benign reading there is), nil with no
// error (compacted, or younger than the window), and an error when the broker
// could not be read, which is not a log that took in nothing.
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

// bytesPerDayOf is the arithmetic, over anything that can be probed by
// sequence — separated from the broker for [ageFloorOf]'s reason: every case
// that matters is reachable in a table test with no broker at all.
func bytesPerDayOf(stats jetstream.LogStats, now time.Time,
	at func(seq uint64) (time.Time, bool, error)) (*uint64, error) {

	cutoff := now.Add(-logRateWindow)
	if stats.CreatedAt.IsZero() || stats.CreatedAt.After(cutoff) {
		// THE STREAM HAS NOT EXISTED FOR THE WHOLE WINDOW — a fresh
		// deployment, or a log re-anchored under a running fleet — so
		// the day this would measure is partly a day before it existed.
		return nil, nil
	}
	if stats.Messages == 0 || stats.FirstSeq > stats.LastSeq {
		// AN ESTABLISHED LOG HOLDING NOTHING took in nothing for a day:
		// anything it had taken in since the cutoff would still be in
		// it, because the trim keeps every record younger than a day.
		return statelog.PerDay(0), nil
	}
	from, err := ageFloorOf(stats.FirstSeq, stats.LastSeq, stats.Messages, cutoff, at)
	if err != nil {
		return nil, err
	}
	// FROM IS THE FIRST RECORD OF THE DAY, or one past the head when the
	// log took in nothing since the cutoff. A strict log is contiguous, so
	// the day is exactly the records from there to the head — clamped to
	// what the log holds, which only a record purged mid-search could make
	// smaller.
	count := min(stats.LastSeq+1-from, stats.Messages)
	bytes := math.Round(float64(count) * float64(stats.Bytes) / float64(stats.Messages))
	return statelog.PerDay(uint64(bytes)), nil
}

// measureRates records every strict log's intake, on the tick.
//
// ON THE TICK AND NEVER ON A REPORT, because the search is a handful of broker
// round trips per log and a report is assembled on every operator request and
// every dashboard poll — the same reason the vector coverage is cached. The
// tick is also "at boot and on every trim tick", which is when the design asks
// for the comparison: it runs at once when the loop starts.
//
// A LOG THIS TICK COULD NOT READ KEEPS ITS LAST MEASUREMENT, for the reason the
// applier's lag does: a figure left standing is a day's rate that was true a
// tick ago, where dropping it would clear a firing alarm on a broker blip and
// raise it again a quarter of an hour later.
func (r *retention) measureRates(ctx context.Context, now time.Time) {
	if r.state == nil {
		return
	}
	logs := make([]measuredLog, 0, len(r.state.order))
	for _, name := range r.state.order {
		running := r.state.domains[name]
		if running == nil || running.log == nil {
			continue
		}
		logs = append(logs, measuredLog{
			domain: name, log: running.log, replay: running.domain.Stream().Replay,
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
		if rate != nil && r.metrics != nil {
			r.metrics.Set(metrics.StatelogLogBytesPerDay, float64(*rate),
				metrics.Attrs{"domain": l.domain})
		}
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
