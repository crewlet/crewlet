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
	return LocalReaderBehind(domain, db, at, 0)
}

// LocalReaderBehind is [LocalReader] over a node that reports itself `lag`
// records behind the stream's end.
//
// # Why the lag is a parameter and not always zero
//
// Because a caller's staleness bound cannot be exercised against a node that
// is caught up: `max_lag_seconds` and `max_lag_seq` are enforced by comparing
// them against this figure, so at lag zero every bound passes and a domain
// reader that dropped the caller's bound on the floor looks exactly like one
// that carried it.
//
// That is not hypothetical. `max_lag_seconds` was validated against the level
// and then never carried, so a tile polling every twenty seconds and declaring
// a twenty-second bound was served an answer of any age and rendered it live;
// `max_lag_seq` was fixed for one question and left in for nine more, because
// nothing on either side of the call could tell. A test that sets a lag and a
// tighter bound is what makes the last hop — the domain's query into
// [statelog.Query] — observable at all.
//
// The node is still CAUGHT UP in every other sense: the floor reads, the
// position is the one given, and nothing is deferred. The only thing this
// changes is how far behind the node says it is.
func LocalReaderBehind(domain statelog.Domain, db DB, at statelog.Position,
	lag uint64) (*statelog.Reader, error) {

	return statelog.NewReader(statelog.ReaderDeps{
		Domain: domain,
		DB:     db,
		Waiter: localWaiter{at: at},
		Health: func() statelog.Health {
			behind, first, floor := lag, uint64(1), uint64(0)
			return statelog.Health{
				Position: at, AppliedThrough: at.Seq, CaughtUp: lag == 0,
				Floor:     statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()},
				Lag:       &behind,
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
