package memory

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the seats' box readings ------------------------------------------- //

// boxReadingKey is a reading's place in the twin.
type boxReadingKey struct {
	seat uuid.UUID
	cell string
}

// BoxReading is seat's reading of its box in cell, false where none is kept.
func (f *Fleet) BoxReading(_ context.Context, seat uuid.UUID, cell string) (coord.BoxReading, bool, error) {
	if seat == uuid.Nil || cell == "" {
		return coord.BoxReading{}, false, errors.New("coord/memory: a box reading is read per seat and cell")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.boxReadings[boxReadingKey{seat, cell}]
	return r, ok, nil
}

// ListBoxReadings is every seat's readings, by seat then cell.
func (f *Fleet) ListBoxReadings(_ context.Context) ([]coord.BoxReading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.BoxReading, 0, len(f.boxReadings))
	for _, r := range f.boxReadings {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b coord.BoxReading) int {
		return cmp.Or(cmp.Compare(a.Seat.String(), b.Seat.String()), cmp.Compare(a.Cell, b.Cell))
	})
	return out, nil
}

// PutBoxReading keeps r, replacing the seat's reading in its cell.
func (f *Fleet) PutBoxReading(_ context.Context, r coord.BoxReading) error {
	if err := r.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boxReadings[boxReadingKey{r.Seat, r.Cell}] = r
	return nil
}

// DeleteBoxReading drops seat's reading in cell; none kept is no error.
func (f *Fleet) DeleteBoxReading(_ context.Context, seat uuid.UUID, cell string) error {
	if seat == uuid.Nil || cell == "" {
		return errors.New("coord/memory: a box reading is dropped per seat and cell")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.boxReadings, boxReadingKey{seat, cell})
	return nil
}
