package kv

import (
	"bytes"
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the advisor analyst's records -------------------------------------- //

// AdvisorRecord is one record and its revision, false where none.
func (f *FleetStore) AdvisorRecord(ctx context.Context, name string) ([]byte, uint64, bool, error) {
	if name == "" {
		return nil, 0, false, errors.New("coord/kv: an advisor record is read by its name")
	}
	kve, err := f.get(ctx, f.positions, coord.AdvisorRecordKey(name))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, 0, false, nil
	case err != nil:
		return nil, 0, false, unavailable("read the advisor's "+name, err)
	}
	return bytes.Clone(kve.Value()), kve.Revision(), true, nil
}

// CreateAdvisorRecord writes a first record, false where one exists.
func (f *FleetStore) CreateAdvisorRecord(ctx context.Context, name string, value []byte) (bool, error) {
	if name == "" {
		return false, errors.New("coord/kv: an advisor record names itself")
	}
	_, err := f.create(ctx, f.positions, coord.AdvisorRecordKey(name), value)
	switch {
	case errors.Is(err, jetstream.ErrKeyExists):
		return false, nil
	case err != nil:
		return false, unavailable("create the advisor's "+name, err)
	}
	return true, nil
}

// UpdateAdvisorRecord replaces the record read at revision, false where it moved.
func (f *FleetStore) UpdateAdvisorRecord(ctx context.Context, name string, value []byte, revision uint64) (bool, error) {
	if name == "" || revision == 0 {
		return false, errors.New("coord/kv: an advisor record is updated at the revision it was read at")
	}
	_, err := f.positions.Update(ctx, coord.AdvisorRecordKey(name), value, revision)
	switch {
	case err != nil && lostUpdateRace(err):
		return false, nil
	case err != nil:
		return false, unavailable("update the advisor's "+name, err)
	}
	return true, nil
}
