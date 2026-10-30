package seataccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
)

// CoordLedger is the [Ledger] on the fleet's coordination store, the one
// implementation (see the package doc for who has to agree on it).
type CoordLedger struct{ store coord.SeatAccess }

var _ Ledger = (*CoordLedger)(nil)

// NewCoordLedger wraps the fleet's seat access records. It refuses a nil
// backend rather than wrapping one, for integration.NewCoordStore's reason: a
// nil interface inside a non-nil ledger is a nil dereference on the first
// sync, inside a detached goroutine.
func NewCoordLedger(store coord.SeatAccess) (*CoordLedger, error) {
	if store == nil {
		return nil, errors.New("seataccess: NewCoordLedger needs the fleet's coordination backend")
	}
	return &CoordLedger{store: store}, nil
}

// Records implements [Ledger].
//
// A RECORD THIS BUILD CANNOT DECODE IS RAISED, not skipped. A skipped record
// is an account the sync no longer knows it created, which is the orphan the
// ledger exists to prevent; refusing the sync until the record is readable
// keeps every other account where it is.
func (l *CoordLedger) Records(ctx context.Context, kind integration.Kind) (map[string]Record, error) {
	raw, err := l.store.SeatAccessRecords(ctx, string(kind))
	if err != nil {
		return nil, fmt.Errorf("seataccess: read the %s ledger: %w", kind, err)
	}
	out := make(map[string]Record, len(raw))
	for handle, value := range raw {
		var rec Record
		if err := json.Unmarshal(value, &rec); err != nil {
			return nil, fmt.Errorf("seataccess: %s's %s ledger record is unreadable: %w", handle, kind, err)
		}
		// THE KEY IS AUTHORITATIVE, as it is for an integration status.
		rec.Kind, rec.Handle = kind, handle
		out[handle] = rec
	}
	return out, nil
}

// Put implements [Ledger].
func (l *CoordLedger) Put(ctx context.Context, rec Record) error {
	if rec.Kind == "" || rec.Handle == "" {
		return errors.New("seataccess: a ledger record needs its integration and its seat")
	}
	value, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("seataccess: encode %s's %s record: %w", rec.Handle, rec.Kind, err)
	}
	if err := l.store.PutSeatAccessRecord(ctx, string(rec.Kind), rec.Handle, value); err != nil {
		return fmt.Errorf("seataccess: record %s's %s account: %w", rec.Handle, rec.Kind, err)
	}
	return nil
}

// Drop implements [Ledger].
func (l *CoordLedger) Drop(ctx context.Context, kind integration.Kind, handle string) error {
	if err := l.store.DeleteSeatAccessRecord(ctx, string(kind), handle); err != nil {
		return fmt.Errorf("seataccess: drop %s's %s record: %w", handle, kind, err)
	}
	return nil
}
