package kv

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP restarts -------------------------------------------------------- //

// One record per server, and one per seat a person restarted one instance of,
// filed in the positions register under a class of their own: see
// internal/coord's mcprestart.go for why that bucket, which has no age. The
// model retries' verbs (modelrecords.go), over the restart's entry.

var restartClass = entryClass[coord.MCPRestart]{
	name:   "the MCP restarts",
	filter: coord.DocumentFilter(coord.MCPRestartClass),
	key:    coord.MCPRestartKey,
	entry:  coord.MCPRestartEntryOf,
	stored: func(r coord.MCPRestart, entry string, version uint64) coord.MCPRestart {
		// A RECORD DESCRIBES THE ENTRY IT IS FILED UNDER, whatever its body
		// says: the entry was read off a key [coord.MCPRestartEntryOf]
		// accepted, so it names a server and, for one instance, a seat.
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
// create or an update at the version read, retried on a lost race, as
// [FleetStore.PutModelRetry] is.
func (f *FleetStore) PutMCPRestart(ctx context.Context, r coord.MCPRestart) (coord.MCPRestart, error) {
	if err := r.Validate(); err != nil {
		return coord.MCPRestart{}, err
	}
	entry := r.Entry()
	for range maxRetryRounds {
		held, found, err := restartClass.read(ctx, f, entry)
		if err != nil {
			return coord.MCPRestart{}, err
		}
		if found && !r.At.After(held.At) {
			return held, nil
		}
		var wrote bool
		var stored coord.MCPRestart
		if found {
			stored, wrote, err = restartClass.update(ctx, f, entry, r, held.Version)
		} else {
			stored, wrote, err = restartClass.create(ctx, f, entry, r)
		}
		if err != nil {
			return coord.MCPRestart{}, err
		}
		if wrote {
			return stored, nil
		}
	}
	return coord.MCPRestart{}, contended("PutMCPRestart", entry)
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
