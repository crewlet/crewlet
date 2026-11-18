package kv

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP restarts -------------------------------------------------------- //

// One record per server, and one per seat whose instance a person restarted,
// a positions-register class with no age (internal/coord's mcprestart.go says
// why), over [entryClass] keyed by the restart's entry.

var restartClass = entryClass[coord.MCPRestart]{
	name:   "the MCP restarts",
	filter: coord.DocumentFilter(coord.MCPRestartClass),
	key:    coord.MCPRestartKey,
	entry:  coord.MCPRestartEntryOf,
	stored: func(r coord.MCPRestart, entry string, version uint64) coord.MCPRestart {
		// The entry it is filed under, whatever its body says; a key
		// [coord.MCPRestartEntryOf] accepted names a server and maybe a seat.
		r.Server, r.Seat, _ = coord.MCPRestartTarget(entry)
		r.At, r.Version = r.At.UTC(), version
		return r
	},
}

// MCPRestart reads one restart.
func (f *FleetStore) MCPRestart(ctx context.Context, entry string) (coord.MCPRestart, bool, error) {
	return restartClass.read(ctx, f, entry)
}

// ListMCPRestarts returns every restart, ordered by entry.
func (f *FleetStore) ListMCPRestarts(ctx context.Context) ([]coord.MCPRestart, error) {
	return restartClass.list(ctx, f)
}

// PutMCPRestart records a restart, keeping the later stamp: a read, then a
// create or an update at the version read, retried on a lost race.
//
// AT MOST [fleetCASAttempts] TIMES, not the person-paced [maxRetryRounds]:
// every node that restarts the server records it, so its contenders are
// bounded by the fleet's head count.
func (f *FleetStore) PutMCPRestart(ctx context.Context, r coord.MCPRestart) (coord.MCPRestart, error) {
	if err := r.Validate(); err != nil {
		return coord.MCPRestart{}, err
	}
	entry := r.Entry()
	var stored coord.MCPRestart
	settled, err := casRetry(ctx, &f.positionTrips, "record the MCP restart of "+entry, func() (bool, error) {
		held, found, err := restartClass.read(ctx, f, entry)
		if err != nil {
			return false, err
		}
		if found && !r.At.After(held.At) {
			stored = held
			return true, nil
		}
		var wrote bool
		if found {
			stored, wrote, err = restartClass.update(ctx, f, entry, r, held.Version)
		} else {
			stored, wrote, err = restartClass.create(ctx, f, entry, r)
		}
		return wrote, err
	})
	switch {
	case err != nil:
		return coord.MCPRestart{}, err
	case !settled:
		return coord.MCPRestart{}, contended("PutMCPRestart", entry)
	}
	return stored, nil
}

// DeleteMCPRestart removes a restart at a version.
func (f *FleetStore) DeleteMCPRestart(ctx context.Context, entry string, version uint64) (bool, error) {
	return restartClass.remove(ctx, f, entry, version)
}

// WatchMCPRestarts streams every restart, the marker, then every change.
func (f *FleetStore) WatchMCPRestarts(ctx context.Context) (<-chan coord.MCPRestartUpdate, error) {
	return watchEntries(ctx, f, restartClass,
		func(v *coord.MCPRestart, entry string, current bool) coord.MCPRestartUpdate {
			return coord.MCPRestartUpdate{Restart: v, Entry: entry, Current: current}
		})
}
