package statelogtest

import (
	"context"
	"database/sql"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// LocalReader is a domain's read authority over rows already applied, with no
// broker behind it, for a domain's tests of its own SQL that seed rows through
// the applier directly. They still read through [statelog.Reader], where the
// level is honoured, so they test the path production takes. It supplies only
// a waiter already at the position and a health that serves, over the
// domain's log ([statelog.Domain.Stream]). It has no read index, so a
// `linearizable` read refuses rather than degrading: with no quorum that is
// the honest answer, and barrier cases belong in internal/statelog against a
// real broker.
func LocalReader(domain statelog.Domain, db DB, at statelog.Position) (*statelog.Reader, error) {
	return LocalReaderBehind(domain, db, at, 0)
}

// LocalReaderBehind is [LocalReader] over a node that reports itself `lag`
// records behind the stream's end, and is healthy in every other sense. At
// lag zero every `max_lag_seconds` and `max_lag_seq` bound passes, so a domain
// reader that drops the caller's bound looks like one that carries it; a lag
// and a tighter bound make the domain's hop into [statelog.Query] observable.
func LocalReaderBehind(domain statelog.Domain, db DB, at statelog.Position,
	lag uint64) (*statelog.Reader, error) {

	return statelog.NewReader(statelog.ReaderDeps{
		Domain: domain,
		Spec:   domain.Stream(),
		Mode:   statelog.ModeNormal,
		DB:     db,
		Waiter: localWaiter{at: at},
		Health: func() (statelog.Health, error) {
			behind, first, floor := lag, uint64(1), uint64(0)
			return statelog.Health{
				Position: at, AppliedThrough: at.Seq, Drained: true,
				Floor:     statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()},
				Lag:       &behind,
				FirstSeq:  &first,
				TrimFloor: &floor,
			}, nil
		},
		// The shipped figure, so a rendered retry hint is the one an
		// operator would see.
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

func (w localWaiter) Reached() statelog.Position { return w.at }

func (w localWaiter) WaitReached(context.Context, statelog.Position) error { return nil }

func (w localWaiter) WaitApplied(context.Context, statelog.ScopeSet, statelog.Position) error {
	return nil
}

// LocalReaderOver is [LocalReader] over a node whose applied position moves:
// the waiter is the caller's own, so a read waits on it, and the health
// reports wherever it is. It exists for cases about the caller's floor: a
// reader handed a position past what the node applied must refuse `behind`,
// not serve older rows, and [LocalReader]'s waiter, which never blocks, cannot
// tell the two apart.
func LocalReaderOver(domain statelog.Domain, db DB, waiter statelog.Waiter) (*statelog.Reader, error) {
	return statelog.NewReader(statelog.ReaderDeps{
		Domain: domain,
		Spec:   domain.Stream(),
		Mode:   statelog.ModeNormal,
		DB:     db,
		Waiter: waiter,
		Health: func() (statelog.Health, error) {
			at := waiter.Reached()
			behind, first, floor := uint64(0), uint64(1), uint64(0)
			return statelog.Health{
				Position: at, AppliedThrough: at.Seq, Drained: true,
				Floor:     statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()},
				Lag:       &behind,
				FirstSeq:  &first,
				TrimFloor: &floor,
			}, nil
		},
		Drain: func() float64 { return 2000 },
	})
}
