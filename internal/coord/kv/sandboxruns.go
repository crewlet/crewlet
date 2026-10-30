package kv

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the detached sandbox runs ----------------------------------------- //

// SandboxRun reads one run's record.
func (f *FleetStore) SandboxRun(ctx context.Context, turnID string) (coord.Record, bool, error) {
	entry, err := f.get(ctx, f.runs, encodeKey(turnID))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return coord.Record{}, false, nil
	}
	if err != nil {
		return coord.Record{}, false, unavailable("read the sandbox run", err)
	}
	return coord.Record{Key: turnID, Value: entry.Value(), Version: entry.Revision()}, true, nil
}

// SandboxRuns returns every record, by turn id.
func (f *FleetStore) SandboxRuns(ctx context.Context) ([]coord.Record, error) {
	// A listing that quietly dropped a run would tell the seat's new owner
	// there is nothing to recover, which abandons a billed box — the exact
	// failure this bucket exists to end. eachEntry raises on a short answer
	// rather than returning one, which is what makes that true.
	var out []coord.Record
	err := f.each(ctx, f.runs, func(kve jetstream.KeyValueEntry) error {
		turnID, ok := decodeKey(kve.Key())
		if !ok {
			return nil
		}
		out = append(out, coord.Record{
			Key: turnID, Value: bytes.Clone(kve.Value()), Version: kve.Revision(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b coord.Record) int { return cmp.Compare(a.Key, b.Key) })
	return out, nil
}

// CreateSandboxRun writes a new record, ignoring a turn id that already
// exists.
func (f *FleetStore) CreateSandboxRun(ctx context.Context, turnID string, value []byte) (bool, error) {
	if turnID == "" {
		return false, errors.New("coord/kv: a sandbox run needs a turn id")
	}
	_, err := f.create(ctx, f.runs, encodeKey(turnID), value)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyExists):
		return false, nil
	default:
		return false, unavailable("create the sandbox run", err)
	}
}

// MutateSandboxRun changes one run's record by a decision taken on the record
// as the stream LEADER holds it.
//
// ONE ATTEMPT is a leader read, the decision, and a write conditioned on the
// revision read — an Update for a Put, a Purge bound to that revision for a
// Delete — and an attempt another writer beat is a LOST RACE, decided again
// through [FleetStore.retryRaces] for the store's patience. A record gone
// between the read and the write is one too: the next attempt reads it absent
// and decides nothing. The read is the leader's for the reason every
// single-key read here is (kv.go): a replica behind the newest write would hand
// the decision a revision it can never win at, and every attempt would lose
// until the replica caught up.
//
// The decision is handed a COPY of the bytes read, and the record answered is
// another, so neither a decision nor its caller can write through the client's
// buffer into what the next attempt reads.
//
// PURGED, the standing rule for a bucket no clock ages — see markers.go, "Every
// removal is a purge", and [FleetStore.SweepMarkers] for what removes the
// marker it leaves.
func (f *FleetStore) MutateSandboxRun(ctx context.Context, turnID string,
	decide func(coord.Record) (coord.SandboxRunChange, error)) (coord.Record, bool, error) {

	if turnID == "" {
		return coord.Record{}, false, errors.New("coord/kv: a sandbox run needs a turn id")
	}
	if decide == nil {
		return coord.Record{}, false, errors.New("coord/kv: a change to a sandbox run needs a decision")
	}
	key := encodeKey(turnID)
	var (
		result  coord.Record
		changed bool
	)
	err := f.retryRaces(ctx, "MutateSandboxRun", turnID, func() (bool, error) {
		result, changed = coord.Record{}, false
		entry, err := f.get(ctx, f.runs, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// NO RECORD, NO DECISION: a run that is gone is the same
			// non-answer as a change nobody wanted.
			return false, nil
		case err != nil:
			return false, unavailable("read the sandbox run", err)
		}
		read := coord.Record{Key: turnID, Value: bytes.Clone(entry.Value()), Version: entry.Revision()}
		change, err := decide(coord.Record{Key: read.Key, Value: bytes.Clone(read.Value), Version: read.Version})
		if err != nil {
			return false, err
		}
		if invalid := change.Validate(); invalid != nil {
			return false, invalid
		}
		switch {
		case change.Delete:
			err = f.runs.Purge(ctx, key, jetstream.LastRevision(read.Version))
			if err == nil {
				result, changed = read, true
			}
		case len(change.Put) > 0:
			var revision uint64
			revision, err = f.runs.Update(ctx, key, change.Put, read.Version)
			if err == nil {
				result = coord.Record{Key: turnID, Value: bytes.Clone(change.Put), Version: revision}
				changed = true
			}
		default:
			result = read
		}
		switch {
		case err == nil:
			return false, nil
		case lostUpdateRace(err):
			// ANOTHER WRITER LANDED between the read and the write, or the
			// record went: decided again on what is there now, never sent
			// again as it was.
			return true, nil
		default:
			return false, unavailable("write the sandbox run", err)
		}
	})
	if err != nil {
		return coord.Record{}, false, err
	}
	return result, changed, nil
}

// WatchSandboxRun streams one run's record, its first state certified.
//
// A KV watcher over the run's own KEY, so the broker sends this record and no
// other run's. Deletes are NOT ignored: a purge is what a finished run looks
// like on the wire, and a watch that dropped it would hold a run that is gone.
//
// # The first state is a certified answer, and the call waits for it
//
// [watchUnder]'s, certified against the bucket's key index exactly as a
// listing is, for the reason [watchRecords] gives: the client's own end of the
// initial values ends them on a guess, and never at all for a consumer whose
// pending messages were removed before it reached them. A record live
// throughout is therefore never answered absent. The pass is a replica's copy
// (walk.go), so a record it delivered can be a revision behind the leader; the
// consumer then delivers the newer one, as the next update.
//
// # A change after it is the consumer's, and filtered by revision
//
// Every later state comes off the same consumer, in the order the stream holds
// them, so a state can be late but never out of order — and the revision the
// watch last delivered keeps the consumer's own late copy of one it already
// sent from being sent again, a marker's revision included. The bucket keeps
// ONE revision per key, so a state overwritten before the consumer reached it
// is gone from the stream and is passed over: what the watch carries is the
// record's current state, which is what [coord.SandboxRuns] promises.
func (f *FleetStore) WatchSandboxRun(ctx context.Context, turnID string) (<-chan coord.SandboxRunUpdate, error) {
	if turnID == "" {
		return nil, errors.New("coord/kv: a sandbox run needs a turn id")
	}
	key := encodeKey(turnID)
	current, w, err := watchUnder(ctx, f.js, f.runs, key, "the sandbox run "+turnID)
	if err != nil {
		return nil, err
	}
	// held is the revision of the state last delivered — a marker's too,
	// since a delete the answer saw is a revision of the key like any other.
	var held uint64
	first := coord.SandboxRunUpdate{}
	if kve, ok := current[key]; ok {
		held = kve.Revision()
		first = sandboxRunState(turnID, kve)
	}

	out := make(chan coord.SandboxRunUpdate)
	go func() {
		defer close(out)
		defer func() { _ = w.Stop() }()
		send := func(u coord.SandboxRunUpdate) bool {
			select {
			case out <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !send(first) {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case kve, ok := <-w.Updates():
				if !ok {
					// The client ended the watch. Closing is the contract's
					// word for "stopped hearing", which the consumer answers
					// by watching again — never by reading it as a delete.
					return
				}
				if kve == nil || kve.Key() != key || kve.Revision() <= held {
					// The client's end of the initial values, which the
					// certified answer has already ended; or NOT A CHANGE —
					// a revision the watch already delivered, or an older
					// one the consumer reached after the leader's read.
					continue
				}
				held = kve.Revision()
				if !send(sandboxRunState(turnID, kve)) {
					return
				}
			}
		}
	}()
	return out, nil
}

// sandboxRunState is the state one revision of a run's key stands for: the
// record it holds, or none for a delete or purge marker.
func sandboxRunState(turnID string, kve jetstream.KeyValueEntry) coord.SandboxRunUpdate {
	if kve.Operation() != jetstream.KeyValuePut {
		return coord.SandboxRunUpdate{}
	}
	return coord.SandboxRunUpdate{Found: true, Record: coord.Record{
		Key: turnID, Value: bytes.Clone(kve.Value()), Version: kve.Revision(),
	}}
}
