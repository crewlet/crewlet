package statelogtest

import (
	"context"
	"database/sql"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// LocalReader is a domain's read authority over rows that are already applied,
// with no broker behind it.
//
// # What it is for, and what it deliberately is not
//
// A domain's own tests are about its SQL — which rows a filter selects, what a
// projection renders — and they seed those rows by running records through the
// applier directly, with no stream and no runner. They still have to go
// through [statelog.Reader], because that is where the level is honoured and a
// reader that bypassed it in tests would be testing a path production does not
// take.
//
// So this supplies the two seams a local read needs and nothing else: a waiter
// already at the position, and a health that serves. It has NO read index, so
// a `linearizable` read through it refuses rather than silently degrading —
// which is the honest answer for a harness with no quorum to commit against,
// and it is what stops this helper being mistaken for a way to test the
// barrier. Those cases belong in internal/statelog, against a real broker.
func LocalReader(domain statelog.Domain, db DB, at statelog.Position) (*statelog.Reader, error) {
	return statelog.NewReader(statelog.ReaderDeps{
		Domain: domain,
		DB:     db,
		Waiter: localWaiter{at: at},
		Health: func() statelog.Health {
			lag, first, floor := uint64(0), uint64(1), uint64(0)
			return statelog.Health{
				Position: at, AppliedThrough: at.Seq, CaughtUp: true,
				Floor:     statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()},
				Lag:       &lag,
				FirstSeq:  &first,
				TrimFloor: &floor,
			}
		},
		// THE SHIPPED FIGURE, so a retry hint a case renders is the one an
		// operator would see rather than a number invented here.
		Drain: func() float64 { return 2000 },
	})
}

// DB is the read seam [statelog.Reader] takes, restated here so a caller need
// not spell the anonymous interface out.
type DB interface {
	Read(context.Context, func(*sql.Tx) error) error
}

// localWaiter is already at the position and never blocks.
type localWaiter struct{ at statelog.Position }

func (w localWaiter) Committed() statelog.Position { return w.at }

func (w localWaiter) WaitCommitted(context.Context, statelog.Position) error { return nil }

func (w localWaiter) WaitApplied(context.Context, statelog.ScopeSet, statelog.Position) error {
	return nil
}
