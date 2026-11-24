package memory

import (
	"bytes"
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the agents' grant requests ---------------------------------------- //

// GrantRequest returns one request, and whether there is one.
func (f *Fleet) GrantRequest(_ context.Context, id string) ([]byte, bool, error) {
	if id == "" {
		return nil, false, errors.New("coord/memory: a grant request is read by its id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.grantRequests[id]
	return bytes.Clone(value), ok, nil
}

// GrantRequests returns every request, by id.
func (f *Fleet) GrantRequests(_ context.Context) (map[string][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.grantRequests))
	for id, value := range f.grantRequests {
		out[id] = bytes.Clone(value)
	}
	return out, nil
}

// PutGrantRequest stores one request.
func (f *Fleet) PutGrantRequest(_ context.Context, id string, value []byte) error {
	if id == "" {
		return errors.New("coord/memory: a grant request needs its id")
	}
	if err := coord.HoldValue(coord.GrantRequestKey(id), len(value), coord.MaxGrantRequestBytes); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grantRequests[id] = bytes.Clone(value)
	return nil
}

// DeleteGrantRequest drops one request; deleting one that is not there is the
// outcome asked for.
func (f *Fleet) DeleteGrantRequest(_ context.Context, id string) error {
	if id == "" {
		return errors.New("coord/memory: a grant request needs its id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.grantRequests, id)
	return nil
}
