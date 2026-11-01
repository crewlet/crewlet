package home

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// SetBackstop sets l's backstop — its passes' interval and a turn's bound —
// and the window its moved targets' merges are counted in, so a suite proves a
// pass catches what a nudge missed, and a pass's merges keep to their share,
// without waiting a minute for either. Before Start.
func SetBackstop(l *Loop, d time.Duration) {
	l.backstop = d
	l.changes.rechecks.window = d
}

// SetMergesPerPass sets how many merges of its moved targets one repository's
// window holds on l, so a suite proves the budget with a few changes rather
// than [MergeabilityPerPass] of them. Before Start.
func SetMergesPerPass(l *Loop, n int) { l.changes.rechecks.perPass = n }

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
