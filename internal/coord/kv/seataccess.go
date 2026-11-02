package kv

import (
	"bytes"
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the seat access ledger -------------------------------------------- //

// A record is filed in the positions register under its own class, by
// integration and seat handle: see [coord.SeatAccess] for why that bucket,
// which is the one with no age.

// SeatAccessRecords returns every record for one integration, by handle.
func (f *FleetStore) SeatAccessRecords(ctx context.Context, kind string) (map[string][]byte, error) {
	if kind == "" {
		return nil, errors.New("coord/kv: the seat access ledger is read per integration")
	}
	out := map[string][]byte{}
	err := f.eachUnder(ctx, f.positions, coord.SeatAccessFilter(kind),
		"the "+kind+" seat access ledger", func(kve jetstream.KeyValueEntry) error {
			handle, ok := coord.SeatAccessHandle(kve.Key(), kind)
			if !ok {
				// A key this backend did not write names no seat.
				return nil
			}
			// COPIED: the entry's buffer belongs to the client.
			out[handle] = bytes.Clone(kve.Value())
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PutSeatAccessRecord records one seat's record at one integration.
func (f *FleetStore) PutSeatAccessRecord(ctx context.Context, kind, handle string, value []byte) error {
	if kind == "" || handle == "" {
		return errors.New("coord/kv: a seat access record needs its integration and its seat")
	}
	if _, err := f.positions.Put(ctx, coord.SeatAccessKey(kind, handle), value); err != nil {
		return unavailable("record "+handle+"'s "+kind+" access", err)
	}
	return nil
}

// DeleteSeatAccessRecord drops one seat's record.
//
// PURGED, the standing rule for a bucket no clock ages: see markers.go.
func (f *FleetStore) DeleteSeatAccessRecord(ctx context.Context, kind, handle string) error {
	if kind == "" || handle == "" {
		return errors.New("coord/kv: a seat access record needs its integration and its seat")
	}
	err := f.positions.Purge(ctx, coord.SeatAccessKey(kind, handle))
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("delete "+handle+"'s "+kind+" access", err)
	}
	return nil
}
