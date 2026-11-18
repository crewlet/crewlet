package memory

import (
	"bytes"
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the advisor analyst's records -------------------------------------- //

// AdvisorRecord is one record and its revision, false where none.
func (f *Fleet) AdvisorRecord(_ context.Context, name string) ([]byte, uint64, bool, error) {
	if name == "" {
		return nil, 0, false, errors.New("coord/memory: an advisor record is read by its name")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.advisorRecords[name]
	return bytes.Clone(r.Value), r.Revision, ok, nil
}

// CreateAdvisorRecord writes a first record, false where one exists.
func (f *Fleet) CreateAdvisorRecord(_ context.Context, name string, value []byte) (bool, error) {
	if name == "" {
		return false, errors.New("coord/memory: an advisor record names itself")
	}
	if err := coord.HoldValue(coord.AdvisorRecordKey(name), len(value)); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.advisorRecords[name]; ok {
		return false, nil
	}
	f.advisorRevision++
	f.advisorRecords[name] = coord.AtlasWake{Value: bytes.Clone(value), Revision: f.advisorRevision}
	return true, nil
}

// UpdateAdvisorRecord replaces the record read at revision, false where it moved.
func (f *Fleet) UpdateAdvisorRecord(_ context.Context, name string, value []byte, revision uint64) (bool, error) {
	if name == "" || revision == 0 {
		return false, errors.New("coord/memory: an advisor record is updated at the revision it was read at")
	}
	if err := coord.HoldValue(coord.AdvisorRecordKey(name), len(value)); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.advisorRecords[name]
	if !ok || r.Revision != revision {
		return false, nil
	}
	f.advisorRevision++
	f.advisorRecords[name] = coord.AtlasWake{Value: bytes.Clone(value), Revision: f.advisorRevision}
	return true, nil
}
