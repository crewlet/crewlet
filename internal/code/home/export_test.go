package home

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// SetBackstop sets l's backstop — its passes' interval and a turn's bound —
// so a suite proves a pass catches what a nudge missed without waiting a
// minute for it. Before Start.
func SetBackstop(l *Loop, d time.Duration) { l.backstop = d }

// SetTicks paces l's passes by ticks in place of a ticker of its backstop, so
// a suite begins each paced pass — and with it each repository's new share of
// a pass's rechecks — exactly when it sends one. Before Start.
func SetTicks(l *Loop, ticks <-chan time.Time) { l.ticks = ticks }

// SetConcurrency sets how many turns l runs at once, so BenchmarkHomeLoop
// measures the values [Concurrency] is chosen among. Before Start.
func SetConcurrency(l *Loop, n int) { l.turnsAtOnce = n }

// SetMergesPerPass sets how many merges of its moved targets one repository's
// share of a pass holds on l, so a suite proves the budget with a few changes
// rather than [MergeabilityPerPass] of them. Before Start.
func SetMergesPerPass(l *Loop, n int) { l.changes.rechecks.perPass = n }

// BeginStop is the first half of l's Stop and nothing more: stopped set
// under the loop's mutex and its watches taken, its life NOT yet ended — the
// window between Stop letting the mutex go and ending the life, which a case
// cannot otherwise reach, held open so the case proves what a watch that
// begins inside it does. The case's own Stop ends the rest.
func BeginStop(l *Loop) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopped = true
	l.watches = make([]Watching, len(nudgeFilters))
}

// CheckWork is l's check work's turn of repoID, its rows read at floor: what
// a nudge or a pass schedules for the checks worker, run on the case's own
// goroutine.
func CheckWork(ctx context.Context, l *Loop, repoID string, floor statelog.Position) error {
	return l.checks.Work(ctx, repoID, floor)
}

// QueueWork is l's merge queue's turn of repoID, its rows read at floor: what
// a nudge or a pass schedules for the queue worker, run on the case's own
// goroutine.
func QueueWork(ctx context.Context, l *Loop, repoID string, floor statelog.Position) error {
	return l.merges.Work(ctx, repoID, floor)
}

// MergeChange is l's merge step for one due change of repoID, read at the
// listing's position at — its ancestry, then its merge recorded through the
// loop's estate — answering whether it landed the change: what
// BenchmarkMergeability times, one merge a run.
func MergeChange(ctx context.Context, l *Loop, repoID string, due code.ChangeDue,
	at statelog.Position) (bool, error) {

	return l.changes.merge(ctx, repoID, due, at)
}

// MergeRateTargets is how many targets l's queue worker holds a merge rate
// for, measured or timing a train: what a suite reads to prove a target this
// node no longer turns is forgotten rather than kept for the node's life.
func MergeRateTargets(l *Loop) int {
	l.merges.rate.mu.Lock()
	defer l.merges.rate.mu.Unlock()
	return len(l.merges.rate.targets)
}
