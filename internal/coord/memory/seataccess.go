package memory

import (
	"bytes"
	"context"
	"errors"
)

// ---- the seat access ledger -------------------------------------------- //

// SeatAccessRecords returns every record for one integration, by handle.
func (f *Fleet) SeatAccessRecords(_ context.Context, kind string) (map[string][]byte, error) {
	if kind == "" {
		return nil, errors.New("coord/memory: the seat access ledger is read per integration")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]byte{}
	for handle, value := range f.seatAccess[kind] {
		// CLONED on the way out, as on the way in, as the KV backend's
		// copy makes it.
		out[handle] = bytes.Clone(value)
	}
	return out, nil
}

// PutSeatAccessRecord records one seat's record at one integration.
func (f *Fleet) PutSeatAccessRecord(_ context.Context, kind, handle string, value []byte) error {
	if kind == "" || handle == "" {
		return errors.New("coord/memory: a seat access record needs its integration and its seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seatAccess[kind] == nil {
		f.seatAccess[kind] = map[string][]byte{}
	}
	f.seatAccess[kind][handle] = bytes.Clone(value)
	return nil
}

// DeleteSeatAccessRecord drops one seat's record; deleting one that is not
// there is the outcome asked for.
func (f *Fleet) DeleteSeatAccessRecord(_ context.Context, kind, handle string) error {
	if kind == "" || handle == "" {
		return errors.New("coord/memory: a seat access record needs its integration and its seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.seatAccess[kind], handle)
	return nil
}
