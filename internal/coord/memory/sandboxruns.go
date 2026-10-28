package memory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the detached sandbox runs ----------------------------------------- //

// SandboxRun reads one run's record.
func (f *Fleet) SandboxRun(_ context.Context, turnID string) (coord.Record, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.runs[turnID]
	if !ok {
		return coord.Record{}, false, nil
	}
	return copyRecord(record), true, nil
}

// SandboxRuns returns every record, by turn id.
func (f *Fleet) SandboxRuns(context.Context) ([]coord.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.Record, 0, len(f.runs))
	for _, key := range slices.Sorted(maps.Keys(f.runs)) {
		out = append(out, copyRecord(f.runs[key]))
	}
	return out, nil
}

// CreateSandboxRun writes a new record, ignoring a turn id that already
// exists.
func (f *Fleet) CreateSandboxRun(_ context.Context, turnID string, value []byte) (bool, error) {
	if turnID == "" {
		return false, errors.New("coord/memory: a sandbox run needs a turn id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.runs[turnID]; exists {
		return false, nil
	}
	f.putRunLocked(turnID, value)
	return true, nil
}

// MutateSandboxRun changes one run's record by a decision taken on the record
// as it stands.
//
// OPTIMISTIC, as the KV backend is, and never a decision taken under the
// twin's lock: the record is read under the lock, decided on WITHOUT it, and
// written under it again only where its version still holds — a write another
// writer beat is decided again on the record as it then stands. So a decision
// is asked again after a lost race here exactly as there, which is the case
// every caller's decision has to be right for, and one a twin that decided
// under its lock could never show a test; and a decision that reads or writes
// the store itself, which the contract allows, does not deadlock on it.
//
// NO PATIENCE: every race this twin loses is another writer's write landing
// under its lock, so the loop ends when the writers do. A ctx that ends
// between two attempts is answered as the KV backend answers it, the store
// unavailable for this write, because every attempt before it lost.
func (f *Fleet) MutateSandboxRun(ctx context.Context, turnID string,
	decide func(coord.Record) (coord.SandboxRunChange, error)) (coord.Record, bool, error) {

	if turnID == "" {
		return coord.Record{}, false, errors.New("coord/memory: a sandbox run needs a turn id")
	}
	if decide == nil {
		return coord.Record{}, false, errors.New("coord/memory: a change to a sandbox run needs a decision")
	}
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := ctx.Err(); err != nil {
				return coord.Record{}, false, fmt.Errorf("%w: change the sandbox run %s: %w",
					coord.ErrUnavailable, turnID, err)
			}
		}
		f.mu.Lock()
		read, ok := f.runs[turnID]
		f.mu.Unlock()
		if !ok {
			return coord.Record{}, false, nil
		}
		read = copyRecord(read)
		change, err := decide(copyRecord(read))
		if err != nil {
			return coord.Record{}, false, err
		}
		if err := change.Validate(); err != nil {
			return coord.Record{}, false, err
		}
		if !change.Delete && len(change.Put) == 0 {
			return read, false, nil
		}
		if written, won := f.applyRunChange(turnID, read, change); won {
			return written, true, nil
		}
	}
}

// applyRunChange writes change over turnID's record where the record still
// stands at read's version, reporting what it wrote — the stored record for a
// put, the removed one for a delete — and false where another writer moved
// the record first, or removed it.
func (f *Fleet) applyRunChange(turnID string, read coord.Record, change coord.SandboxRunChange) (coord.Record, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.runs[turnID]
	if !ok || current.Version != read.Version {
		return coord.Record{}, false
	}
	if change.Delete {
		delete(f.runs, turnID)
		f.notifyRunLocked(turnID, coord.SandboxRunUpdate{})
		return read, true
	}
	return copyRecord(f.putRunLocked(turnID, change.Put)), true
}

// WatchSandboxRun streams one run's record, its first state included.
//
// The snapshot and the registration happen under ONE lock, the ordering
// guarantee [Fleet.WatchSeatPauses] states: a write lands before the snapshot
// and is in it, or after the registration and is delivered as a change. This
// twin delivers every state, none passed over — inside what the contract
// allows, which only says a backend MAY pass one over.
func (f *Fleet) WatchSandboxRun(ctx context.Context, turnID string) (<-chan coord.SandboxRunUpdate, error) {
	if turnID == "" {
		return nil, errors.New("coord/memory: a sandbox run needs a turn id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	first := coord.SandboxRunUpdate{}
	if record, ok := f.runs[turnID]; ok {
		first = coord.SandboxRunUpdate{Found: true, Record: copyRecord(record)}
	}
	w := newWatcher(first)
	if f.runWatchers == nil {
		f.runWatchers = map[string]map[*watcher[coord.SandboxRunUpdate]]struct{}{}
	}
	if f.runWatchers[turnID] == nil {
		f.runWatchers[turnID] = map[*watcher[coord.SandboxRunUpdate]]struct{}{}
	}
	f.runWatchers[turnID][w] = struct{}{}
	return w.serve(ctx, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.runWatchers[turnID], w)
		if len(f.runWatchers[turnID]) == 0 {
			delete(f.runWatchers, turnID)
		}
	}), nil
}

// putRunLocked stores value as turnID's record at the next store-wide version
// and tells the run's watchers, answering the record stored.
//
// Versions start at 1, so a zero version names no record, and they are drawn
// from the counter every versioned write shares — see [Fleet.version] for the
// incarnation that would otherwise hand a deleted run's version to its
// successor.
func (f *Fleet) putRunLocked(turnID string, value []byte) coord.Record {
	f.version++
	stored := coord.Record{Key: turnID, Value: slices.Clone(value), Version: f.version}
	f.runs[turnID] = stored
	f.notifyRunLocked(turnID, coord.SandboxRunUpdate{Found: true, Record: stored})
	return stored
}

// notifyRunLocked hands u to every watch of turnID, each its own copy of the
// bytes, so one consumer that keeps them cannot see another's edits.
func (f *Fleet) notifyRunLocked(turnID string, u coord.SandboxRunUpdate) {
	for w := range f.runWatchers[turnID] {
		copied := u
		copied.Record = copyRecord(u.Record)
		w.push(copied)
	}
}

// copyRecord hands back a value whose bytes the caller cannot write through.
//
// A map of slices shares its backing arrays, so without this a caller that
// decoded a record, mutated the buffer and lost the CAS would have rewritten
// the store's own copy anyway — a write that never happened, visible.
func copyRecord(r coord.Record) coord.Record {
	r.Value = slices.Clone(r.Value)
	return r
}
