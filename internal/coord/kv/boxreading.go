package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the seats' box readings ------------------------------------------- //

// BoxReading is seat's reading of its box in cell, false where none is kept.
func (f *FleetStore) BoxReading(ctx context.Context, seat uuid.UUID, cell string) (coord.BoxReading, bool, error) {
	if seat == uuid.Nil || cell == "" {
		return coord.BoxReading{}, false, errors.New("coord/kv: a box reading is read per seat and cell")
	}
	entry, err := f.get(ctx, f.positions, coord.BoxReadingKey(seat, cell))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.BoxReading{}, false, nil
	case err != nil:
		return coord.BoxReading{}, false, unavailable("read the box reading of "+seat.String(), err)
	}
	var r coord.BoxReading
	if err := json.Unmarshal(entry.Value(), &r); err != nil {
		return coord.BoxReading{}, false, fmt.Errorf("coord/kv: decode the box reading %s: %w", entry.Key(), err)
	}
	return r, true, nil
}

// ListBoxReadings is every seat's readings.
func (f *FleetStore) ListBoxReadings(ctx context.Context) ([]coord.BoxReading, error) {
	var out []coord.BoxReading
	err := f.eachUnder(ctx, f.positions, coord.BoxReadingsFilter(), "the box readings",
		func(kve jetstream.KeyValueEntry) error {
			var r coord.BoxReading
			if err := json.Unmarshal(kve.Value(), &r); err != nil {
				return fmt.Errorf("coord/kv: decode the box reading %s: %w", kve.Key(), err)
			}
			out = append(out, r)
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PutBoxReading keeps r, replacing the seat's reading in its cell.
func (f *FleetStore) PutBoxReading(ctx context.Context, r coord.BoxReading) error {
	if err := r.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("coord/kv: encode a box reading: %w", err)
	}
	if _, err := f.positions.Put(ctx, coord.BoxReadingKey(r.Seat, r.Cell), raw); err != nil {
		return unavailable("keep the box reading of "+r.Seat.String(), err)
	}
	return nil
}

// DeleteBoxReading drops seat's reading in cell; none kept is no error.
//
// PURGED, the standing rule for a bucket no clock ages: see markers.go.
func (f *FleetStore) DeleteBoxReading(ctx context.Context, seat uuid.UUID, cell string) error {
	if seat == uuid.Nil || cell == "" {
		return errors.New("coord/kv: a box reading is dropped per seat and cell")
	}
	err := f.positions.Purge(ctx, coord.BoxReadingKey(seat, cell))
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("drop the box reading of "+seat.String(), err)
	}
	return nil
}
