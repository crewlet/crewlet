package kv

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

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

// The bucket's two kinds of key: a run's RECORD is its turn id, one segment
// ([encodeKey]), and the LOG beside it is the turn id and [runLogSegment], two
// ([runLogKey]). A subject token path, so the broker tells them apart: a
// listing of records filters on [runRecordKeys] and never moves a log, and the
// sweep of logs left behind filters on [runLogKeys] and never moves a record.
const (
	runLogSegment = "log"
	runRecordKeys = "*"
)

// runLogKeys matches every log in the bucket, whatever its run.
var runLogKeys = "*" + coord.KeySeparator + coord.DocumentKey(runLogSegment)

// runLogKey is the key of the log beside turnID's run.
func runLogKey(turnID string) string { return coord.DocumentKey(turnID, runLogSegment) }

// SandboxRuns returns every record, by turn id — the records alone.
func (f *FleetStore) SandboxRuns(ctx context.Context) ([]coord.Record, error) {
	// A listing that quietly dropped a run would tell the seat's new owner
	// there is nothing to recover, which abandons a billed box — the exact
	// failure this bucket exists to end. eachEntryUnder raises on a short
	// answer rather than returning one, which is what makes that true.
	//
	// RECORDS ONLY, filtered by the broker: the logs beside them hold up to
	// two hundred calls each with their arguments and output, and this
	// listing — the busy check, the recovery pass, the pause reaper — never
	// reads one.
	var out []coord.Record
	err := f.eachUnder(ctx, f.runs, runRecordKeys, "the sandbox runs", func(kve jetstream.KeyValueEntry) error {
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
// through [casRetry], within the bound every fleet key shares
// ([fleetCASAttempts]). A record gone
// between the read and the write is one too: the next attempt reads it absent
// and decides nothing. The read is the leader's for the reason every
// single-key read here is (kv.go): a replica behind the newest write would hand
// the decision a revision it can never win at, and every attempt would lose
// until the replica caught up.
//
// NOT COALESCED ON A LINE ([writeLines]), though the record has many
// writers — every tool call its box makes over the bridge, from as many as
// twenty subagents at once, beside its seat's own turns, a peer recovering
// it, a person's answer and a person's stop — because the decision may itself
// read or write the store ([coord.SandboxRuns]), and a line runs every
// write's decision on the goroutine leading its batch, where a decision that
// wrote the same record would wait on its own line. Each call is its own loop
// instead, and the bound still holds them: twice [fleetWriters] attempts,
// each waiting a window of round trips after a loss, outlast the overlap of
// the calls one run's box can make at once.
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
	settled, err := casRetry(ctx, &f.runTrips, "change the sandbox run "+turnID, func() (bool, error) {
		result, changed = coord.Record{}, false
		entry, err := f.get(ctx, f.runs, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// NO RECORD, NO DECISION: a run that is gone is the same
			// non-answer as a change nobody wanted.
			return true, nil
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
				// THEN ITS LOG, never before: a delete decided again
				// on a record reopened under it must find the log
				// still there. See [FleetStore.removeRunLog].
				f.removeRunLog(ctx, turnID)
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
			return true, nil
		case lostUpdateRace(err):
			// ANOTHER WRITER LANDED between the read and the write, or the
			// record went: decided again on what is there now, never sent
			// again as it was.
			return false, nil
		default:
			return false, unavailable("write the sandbox run", err)
		}
	})
	switch {
	case err != nil:
		return coord.Record{}, false, err
	case !settled:
		return coord.Record{}, false, contended("MutateSandboxRun", turnID)
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

// SandboxRunLog reads the log beside turnID's run as the stream LEADER holds it
// ([FleetStore.get]): a replica behind the quorum would hand a resume a log
// short of the calls appended a moment ago, and the resumed turn would judge
// the run on it.
func (f *FleetStore) SandboxRunLog(ctx context.Context, turnID string) (coord.Record, bool, error) {
	if turnID == "" {
		return coord.Record{}, false, errors.New("coord/kv: a sandbox run's log needs a turn id")
	}
	entry, err := f.get(ctx, f.runs, runLogKey(turnID))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return coord.Record{}, false, nil
	}
	if err != nil {
		return coord.Record{}, false, unavailable("read the sandbox run's log", err)
	}
	return coord.Record{Key: turnID, Value: bytes.Clone(entry.Value()), Version: entry.Revision()}, true, nil
}

// MutateSandboxRunLog changes the log beside turnID's run by a decision taken
// on the log as the stream LEADER holds it — [FleetStore.MutateSandboxRun]'s
// loop, on the log's key and within the same bound.
//
// AN UPDATE needs nothing more: the record's removal is followed by the
// log's ([FleetStore.removeRunLog]), so an update lands before that and is
// removed with it, or after it and loses its race, and is decided again on a
// log that is gone.
//
// A CREATE ASKS FOR THE RECORD ONCE IT HAS LANDED, from the leader, and
// removes what it wrote where the record is gone. That read is what closes the
// one race an update cannot lose: the record removed, then its (absent) log,
// then this create. The record read after the create either finds it — and
// the record's removal, and so the log's, comes after the create and takes it
// — or finds it gone, and the create is undone here, through the revision it
// wrote, so a log a later writer put there is untouched.
func (f *FleetStore) MutateSandboxRunLog(ctx context.Context, turnID string,
	decide func(coord.Record, bool) ([]byte, error)) (coord.Record, bool, error) {

	if turnID == "" {
		return coord.Record{}, false, errors.New("coord/kv: a sandbox run's log needs a turn id")
	}
	if decide == nil {
		return coord.Record{}, false, errors.New("coord/kv: a change to a sandbox run's log needs a decision")
	}
	key := runLogKey(turnID)
	var (
		result  coord.Record
		changed bool
	)
	settled, err := casRetry(ctx, &f.runTrips, "change the sandbox run's log "+turnID, func() (bool, error) {
		result, changed = coord.Record{}, false
		read, found := coord.Record{Key: turnID}, true
		entry, err := f.get(ctx, f.runs, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			found = false
		case err != nil:
			return false, unavailable("read the sandbox run's log", err)
		default:
			read = coord.Record{Key: turnID, Value: bytes.Clone(entry.Value()), Version: entry.Revision()}
		}
		put, err := decide(coord.Record{Key: read.Key, Value: bytes.Clone(read.Value), Version: read.Version}, found)
		if err != nil {
			return false, err
		}
		if len(put) == 0 {
			result = read
			return true, nil
		}
		if found {
			revision, err := f.runs.Update(ctx, key, put, read.Version)
			switch {
			case err == nil:
				result, changed = coord.Record{Key: turnID, Value: bytes.Clone(put), Version: revision}, true
				return true, nil
			case lostUpdateRace(err):
				return false, nil
			default:
				return false, unavailable("write the sandbox run's log", err)
			}
		}
		revision, err := f.create(ctx, f.runs, key, put)
		switch {
		case errors.Is(err, jetstream.ErrKeyExists):
			// ANOTHER WRITER CREATED IT first: decided again on its log.
			return false, nil
		case err != nil:
			return false, unavailable("create the sandbox run's log", err)
		}
		_, err = f.get(ctx, f.runs, encodeKey(turnID))
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// THE RUN ENDED UNDER THE CREATE: undone, through the
			// revision written. A loss here is a later write, which is
			// the record's own successor's and not this one's to remove.
			if err := f.runs.Purge(ctx, key, jetstream.LastRevision(revision)); err != nil &&
				!lostUpdateRace(err) {
				return false, unavailable("remove the log written beside a run that ended", err)
			}
			return true, nil
		case err != nil:
			// The create stands: the record is read again by the sweep,
			// which removes the log if the run is in fact gone.
			return false, unavailable("read the sandbox run under its new log", err)
		}
		result, changed = coord.Record{Key: turnID, Value: bytes.Clone(put), Version: revision}, true
		return true, nil
	})
	switch {
	case err != nil:
		return coord.Record{}, false, err
	case !settled:
		return coord.Record{}, false, contended("MutateSandboxRunLog", turnID)
	}
	return result, changed, nil
}

// removeRunLog removes the log beside a run whose record this node has just
// removed, whatever the log holds.
//
// ASKED FIRST, of the leader: a run that never opened a job has no log, and a
// purge of a key holding nothing leaves a marker behind for every such run,
// which the marker sweep and every walk of the bucket would then read. Asking
// first loses nothing to a create racing it: a create that lands after this
// read finds the record already gone and removes itself
// ([FleetStore.MutateSandboxRunLog]).
//
// A FAILURE IS LOGGED, NOT RETURNED: the record's removal has landed, and an
// error would tell its caller the decision did not — a caller that retried
// would find no record and decide nothing. The log is left to the sweep
// ([FleetStore.sweepRunLogs]), which removes a log whose run has no record.
func (f *FleetStore) removeRunLog(ctx context.Context, turnID string) {
	key := runLogKey(turnID)
	ctx = context.WithoutCancel(ctx)
	_, err := f.get(ctx, f.runs, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return
	}
	if err == nil {
		err = f.runs.Purge(ctx, key)
	}
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		log.WarnContext(ctx, "coord_kv_sandbox_run_log_left", "turn_id", turnID, "error", err.Error(),
			"detail", "the run's record is removed and its log is not; the marker sweep removes it")
	}
}

// sweepRunLogs removes every log written before cutoff whose run has no
// record, reporting how many messages went — see [coord.Markers].
//
// THE LOGS FIRST, OFF A WALK FILTERED TO THEM, THEN EACH ONE'S RECORD from the
// leader, so a record written under the turn again after its log was found
// keeps it; and each log is purged THROUGH THE REVISION THE WALK SAW
// ([leaderPurge]), so a log written again since is untouched.
func (f *FleetStore) sweepRunLogs(ctx context.Context, cutoff time.Time) (int64, error) {
	what := "the sandbox runs' logs"
	purge, err := newLeaderPurge(f.js, f.runs)
	if err != nil {
		return 0, fmt.Errorf("coord/kv: sweep %s: %w", what, err)
	}
	pass, err := watchWalk(ctx, f.runs, runLogKeys, what)
	if err != nil {
		return 0, err
	}
	keys := make([]string, 0, len(pass))
	for key, kve := range pass {
		if kve.Operation() == jetstream.KeyValuePut && kve.Created().Before(cutoff) {
			keys = append(keys, key)
		}
	}
	// KEY ORDER, as the marker sweep's, so a failure part-way names the same
	// key on every retry.
	slices.Sort(keys)
	var swept int64
	for _, key := range keys {
		segments, ok := coord.DocumentSegments(key)
		if !ok || len(segments) != 2 {
			continue
		}
		_, err := f.get(ctx, f.runs, encodeKey(segments[0]))
		switch {
		case err == nil:
			continue
		case !errors.Is(err, jetstream.ErrKeyNotFound):
			return swept, unavailable("sweep "+what, fmt.Errorf("read the run of %q: %w", key, err))
		}
		n, err := purge.through(ctx, key, pass[key].Revision())
		if err != nil {
			return swept, unavailable("sweep "+what, fmt.Errorf("remove the log %q: %w", key, err))
		}
		swept += int64(n)
	}
	return swept, nil
}
