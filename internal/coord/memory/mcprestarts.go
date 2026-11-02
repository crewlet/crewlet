package memory

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP restarts -------------------------------------------------------- //

// The model retries' class machinery (modelrecords.go), keyed by a restart's
// entry: one record per server, and one per seat a person restarted one
// instance of.

func newRestartRecords() entryRecords[coord.MCPRestart, coord.MCPRestartUpdate] {
	return entryRecords[coord.MCPRestart, coord.MCPRestartUpdate]{
		stamp: func(v coord.MCPRestart, version uint64) coord.MCPRestart {
			v.At, v.Version = v.At.UTC(), version
			return v
		},
		update: func(v *coord.MCPRestart, entry string, current bool) coord.MCPRestartUpdate {
			return coord.MCPRestartUpdate{Restart: v, Entry: entry, Current: current}
		},
	}
}

var errNoRestartEntry = errors.New("coord/memory: an MCP restart needs its entry: the server, and the seat for one instance")

// MCPRestart reads one restart.
func (f *Fleet) MCPRestart(_ context.Context, entry string) (coord.MCPRestart, bool, error) {
	if _, _, ok := coord.MCPRestartTarget(entry); !ok {
		return coord.MCPRestart{}, false, errNoRestartEntry
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.restarts.records[entry]
	return r, ok, nil
}

// ListMCPRestarts returns every restart, ordered by entry.
func (f *Fleet) ListMCPRestarts(context.Context) ([]coord.MCPRestart, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restarts.sortedLocked(), nil
}

// PutMCPRestart records a restart, keeping the later stamp.
func (f *Fleet) PutMCPRestart(_ context.Context, r coord.MCPRestart) (coord.MCPRestart, error) {
	if err := r.Validate(); err != nil {
		return coord.MCPRestart{}, err
	}
	entry := r.Entry()
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, ok := f.restarts.records[entry]; ok && !r.At.After(held.At) {
		return held, nil
	}
	return f.restarts.storeLocked(f, entry, r), nil
}

// DeleteMCPRestart removes a restart at a version.
func (f *Fleet) DeleteMCPRestart(_ context.Context, entry string, version uint64) (bool, error) {
	if _, _, ok := coord.MCPRestartTarget(entry); !ok {
		return false, errNoRestartEntry
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.restarts.records[entry]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	f.restarts.removeLocked(entry)
	return true, nil
}

// WatchMCPRestarts streams every restart, the marker, then every change.
func (f *Fleet) WatchMCPRestarts(ctx context.Context) (<-chan coord.MCPRestartUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restarts.watchLocked(ctx, f), nil
}
