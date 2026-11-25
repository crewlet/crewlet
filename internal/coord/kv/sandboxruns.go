// A run's STANDING rides the message of its record: three headers its writer
// states ([standingHeader]) in the one conditional publish that writes the
// value ([settledBucket.update]), so the two land at one revision or not at
// all. [FleetStore.SandboxRunStandings] lists them over a pass of headers
// alone: across a leaf's link a record at its bound takes minutes, its
// standing milliseconds. A record whose last write stated none (an older
// build's) is listed without one, for its reader to judge on the value read
// whole, never skipped. Each header has a widest form, which [kvHeaderBytes]
// counts: the handle rides as its digest because a handle has no bound.
// ADR-0175 counts what the pass moves.

package kv

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
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

// runRecordKeys matches a run's RECORD key, one segment ([encodeKey]), and
// never a PART beside it, two ([runPartKey]), so a listing of records filters
// at the broker. Parts are swept off the marker pass ([FleetStore.sweepRunParts]).
const runRecordKeys = "*"

// runPartClasses is a class for each part a run's record may have beside it
// ([runPartKey]), held to a run's value: any turn's part under its name.
func runPartClasses() []keyClass {
	var out []keyClass
	for _, part := range coord.RunParts() {
		out = append(out, keyClass{keys: "*" + coord.KeySeparator + coord.DocumentKey(string(part)),
			class: runValue})
	}
	return out
}

// runPartKey is the key of part beside turnID's run.
func runPartKey(turnID string, part coord.RunPart) string {
	return coord.DocumentKey(turnID, string(part))
}

// longestRunKey is the length of the longest key a run's value is written
// under: its longest part's, under a 36-byte UUIDv7 turn id (internal/engine's
// newRunID). [Reach.valueCrossing] counts it beside the value, as
// [settledBucket.budget] counts every write's key.
var longestRunKey = func() int {
	turnID := uuid.Nil.String()
	longest := len(encodeKey(turnID))
	for _, part := range coord.RunParts() {
		longest = max(longest, len(runPartKey(turnID, part)))
	}
	return longest
}()

// partOf refuses a part [coord.RunParts] does not name, so a caller's typo
// is an error rather than a key nothing removes.
func partOf(turnID string, part coord.RunPart) error {
	switch {
	case turnID == "":
		return errors.New("coord/kv: a sandbox run's part needs a turn id")
	case !part.Valid():
		return fmt.Errorf("coord/kv: %q is not a part of a sandbox run", part)
	}
	return nil
}

// SandboxRuns returns every record, by turn id — the records alone.
func (f *FleetStore) SandboxRuns(ctx context.Context) ([]coord.Record, error) {
	// A short answer raises rather than returns: a dropped run would tell
	// the seat's new owner there is nothing to recover, abandoning a billed
	// box. RECORDS ONLY, filtered by the broker: the parts (a call log, a
	// sealed secret set) are kept out of the record, and this listing
	// never reads one.
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

// The headers a run's standing rides in ([coord.RunStanding]).
const (
	runSeatHeader   = "Crewlet-Run-Seat"
	runHandleHeader = "Crewlet-Run-Handle-Sha256"
	runEpochHeader  = "Crewlet-Run-Owner-Epoch"
)

// widestStanding is a standing at its widest on the wire: a seat and a digest
// are one width whatever they hold, and an epoch's widest is math.MinInt64.
var widestStanding = coord.StandingOf(uuid.Max, "", math.MinInt64)

// standingHeader is the header stating s, and none for nil.
func standingHeader(s *coord.RunStanding) nats.Header {
	if s == nil {
		return nil
	}
	return nats.Header{
		runSeatHeader:   {s.Seat.String()},
		runHandleHeader: {hex.EncodeToString(s.Handle[:])},
		runEpochHeader:  {strconv.FormatInt(s.OwnerEpoch, 10)},
	}
}

// standingOf is the standing h states: nil unless all three headers are there
// and parse. A header this build does not know is not read.
func standingOf(h nats.Header) *coord.RunStanding {
	seat, err := uuid.Parse(h.Get(runSeatHeader))
	if err != nil {
		return nil
	}
	digest, err := hex.DecodeString(h.Get(runHandleHeader))
	if err != nil || len(digest) != len(coord.RunStanding{}.Handle) {
		return nil
	}
	epoch, err := strconv.ParseInt(h.Get(runEpochHeader), 10, 64)
	if err != nil {
		return nil
	}
	s := coord.RunStanding{Seat: seat, OwnerEpoch: epoch}
	copy(s.Handle[:], digest)
	return &s
}

// SandboxRunStandings returns every record's standing as its last write stated
// it, by turn id: a certified pass of the records' headers alone
// ([listHeadersUnder]), a record the pass missed read whole from the leader.
func (f *FleetStore) SandboxRunStandings(ctx context.Context) ([]coord.StoodRun, error) {
	var out []coord.StoodRun
	err := listHeadersUnder(ctx, f.js, f.runs, runRecordKeys, "the sandbox runs' standing",
		func(kve jetstream.KeyValueEntry) error {
			turnID, ok := decodeKey(kve.Key())
			if !ok {
				return nil
			}
			out = append(out, coord.StoodRun{TurnID: turnID, Version: kve.Revision(),
				Standing: standingOf(headersOf(kve))})
			return nil
		})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b coord.StoodRun) int { return cmp.Compare(a.TurnID, b.TurnID) })
	return out, nil
}

// CreateSandboxRun writes a new record stating standing in its message,
// ignoring a turn id that already exists.
func (f *FleetStore) CreateSandboxRun(ctx context.Context, turnID string, value []byte,
	standing *coord.RunStanding) (bool, error) {

	if turnID == "" {
		return false, errors.New("coord/kv: a sandbox run needs a turn id")
	}
	_, err := createKey(ctx, f.js, f.runs, encodeKey(turnID), value, standing)
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
// as the stream LEADER holds it (package doc, "Every single-key read is the
// leader's"). An attempt is a read, the decision, and a write conditioned on
// the revision read; a lost race, or a record gone meanwhile, is decided again
// through [casRetry] within [fleetCASAttempts]. NOT COALESCED ON A LINE
// ([writeLines]): the decision may itself write the store, and would wait on
// its own line; twice [fleetWriters] attempts outlast the overlap of one box's
// calls. The decision and the caller get COPIES, so neither writes through the
// client's buffer. A delete PURGES (markers.go, "Every removal is a purge").
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
				// THEN ITS PARTS, never before: a delete decided again
				// must find them still there ([FleetStore.removeRunPart]).
				for _, part := range coord.RunParts() {
					f.removeRunPart(ctx, turnID, part)
				}
			}
		case len(change.Put) > 0:
			var revision uint64
			revision, err = f.updateRun(ctx, key, change.Put, read.Version, change.Standing)
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
			// ANOTHER WRITER LANDED, or the record went: decided again on
			// what is there now, never resent.
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

// updateRun writes a run's record at revision, stating standing in its message.
func (f *FleetStore) updateRun(ctx context.Context, key string, value []byte, revision uint64,
	standing *coord.RunStanding) (uint64, error) {

	h, ok := f.runs.(headedWriter)
	if !ok {
		return 0, fmt.Errorf("coord/kv: %s is not a bucket settled where it was opened, so "+
			"nothing writes a run's standing with its record", f.runs.Bucket())
	}
	return h.update(ctx, key, value, revision, standing)
}

// ValueCrossing is how long a write of a run's largest value takes to cross
// this node's link to the buckets at the slowest allowed pace, and zero in
// process ([Reach], [coord.SandboxRuns.ValueCrossing]).
func (f *FleetStore) ValueCrossing() time.Duration { return f.reach.valueCrossing() }

// ValueReadCrossing is how long the leader's reply to a read of a run's
// largest value takes to cross this node's link to the buckets at the slowest
// allowed pace, and zero in process ([Reach.readCrossing],
// [coord.SandboxRuns.ValueReadCrossing]).
func (f *FleetStore) ValueReadCrossing() time.Duration { return f.reach.readCrossing() }

// WatchSandboxRun streams one run's record over a watcher on its own KEY.
// Deletes are NOT ignored: a purge is how a finished run looks on the wire.
// The first state is [watchUnder]'s certified answer, for the reason
// [watchRecords] gives, so a live record is never answered absent; it may be
// a revision behind the leader, and the consumer then sends the newer one.
// Later states come in stream order, and the revision last delivered (a
// marker's included) filters out a copy already sent. The bucket keeps ONE
// revision per key, so an overwritten state is skipped: the watch carries the
// current state, which is what [coord.SandboxRuns] promises.
func (f *FleetStore) WatchSandboxRun(ctx context.Context, turnID string) (<-chan coord.SandboxRunUpdate, error) {
	if turnID == "" {
		return nil, errors.New("coord/kv: a sandbox run needs a turn id")
	}
	return f.watchRunKey(ctx, turnID, encodeKey(turnID), "the sandbox run "+turnID)
}

// WatchSandboxRunPart is [FleetStore.WatchSandboxRun]'s watch on the part's
// own key: no other key's write reaches it, and a marker on it is delivered as
// a part that is gone.
func (f *FleetStore) WatchSandboxRunPart(ctx context.Context, turnID string, part coord.RunPart,
) (<-chan coord.SandboxRunUpdate, error) {
	if err := partOf(turnID, part); err != nil {
		return nil, err
	}
	return f.watchRunKey(ctx, turnID, runPartKey(turnID, part),
		"the sandbox run "+turnID+"'s "+string(part))
}

// watchRunKey streams one key of turnID's run, record or part, as
// [FleetStore.WatchSandboxRun] describes. what names the key in an error.
func (f *FleetStore) watchRunKey(ctx context.Context, turnID, key, what string,
) (<-chan coord.SandboxRunUpdate, error) {
	current, w, err := watchUnder(ctx, f.js, f.runs, key, what)
	if err != nil {
		return nil, err
	}
	// held is the revision last delivered, a marker's included.
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
					// Closing means "stopped hearing": the consumer watches
					// again, never reading it as a delete.
					return
				}
				if kve == nil || kve.Key() != key || kve.Revision() <= held {
					// The client's end of initial values (the certified
					// answer already ended them), or NOT A CHANGE: a
					// revision already delivered or older.
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

// SandboxRunPart reads the part beside turnID's run from the stream LEADER
// ([FleetStore.get]): a lagging replica would hand a resume a short call log.
func (f *FleetStore) SandboxRunPart(ctx context.Context, turnID string, part coord.RunPart) (coord.Record, bool, error) {
	if err := partOf(turnID, part); err != nil {
		return coord.Record{}, false, err
	}
	entry, err := f.get(ctx, f.runs, runPartKey(turnID, part))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return coord.Record{}, false, nil
	}
	if err != nil {
		return coord.Record{}, false, unavailable("read the sandbox run's "+string(part), err)
	}
	return coord.Record{Key: turnID, Value: bytes.Clone(entry.Value()), Version: entry.Revision()}, true, nil
}

// MutateSandboxRunPart is [FleetStore.MutateSandboxRun]'s loop on the part's
// key. AN UPDATE needs nothing more: the part is removed after its record
// ([FleetStore.removeRunPart]), so an update lands first and goes with it, or
// loses its race. A CREATE then reads the record from the leader, closing the
// race of record removed, part removed, part created: a record still there
// will take the part with it; a record gone means the create is undone here,
// through the revision it wrote, so a later writer's part is untouched.
func (f *FleetStore) MutateSandboxRunPart(ctx context.Context, turnID string, part coord.RunPart,
	decide func(coord.Record, bool) ([]byte, error)) (coord.Record, bool, error) {

	if err := partOf(turnID, part); err != nil {
		return coord.Record{}, false, err
	}
	if decide == nil {
		return coord.Record{}, false, errors.New("coord/kv: a change to a sandbox run's part needs a decision")
	}
	key := runPartKey(turnID, part)
	what := "the sandbox run's " + string(part)
	var (
		result  coord.Record
		changed bool
	)
	settled, err := casRetry(ctx, &f.runTrips, "change "+what+" "+turnID, func() (bool, error) {
		result, changed = coord.Record{}, false
		read, found := coord.Record{Key: turnID}, true
		entry, err := f.get(ctx, f.runs, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			found = false
		case err != nil:
			return false, unavailable("read "+what, err)
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
			revision, werr := f.runs.Update(ctx, key, put, read.Version)
			switch {
			case werr == nil:
				result, changed = coord.Record{Key: turnID, Value: bytes.Clone(put), Version: revision}, true
				return true, nil
			case lostUpdateRace(werr):
				return false, nil
			default:
				return false, unavailable("write "+what, werr)
			}
		}
		revision, err := f.create(ctx, f.runs, key, put)
		switch {
		case errors.Is(err, jetstream.ErrKeyExists):
			// ANOTHER WRITER CREATED IT first: decided again on its part.
			return false, nil
		case err != nil:
			return false, unavailable("create "+what, err)
		}
		_, err = f.get(ctx, f.runs, encodeKey(turnID))
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// THE RUN ENDED UNDER THE CREATE: undone at the revision
			// written; a loss here is a successor's write, not ours.
			if perr := f.runs.Purge(ctx, key, jetstream.LastRevision(revision)); perr != nil &&
				!lostUpdateRace(perr) {
				return false, unavailable("remove the "+string(part)+" written beside a run that ended", perr)
			}
			return true, nil
		case err != nil:
			// The create stands; the sweep removes it if the run is gone.
			return false, unavailable("read the sandbox run under its new "+string(part), err)
		}
		result, changed = coord.Record{Key: turnID, Value: bytes.Clone(put), Version: revision}, true
		return true, nil
	})
	switch {
	case err != nil:
		return coord.Record{}, false, err
	case !settled:
		return coord.Record{}, false, contended("MutateSandboxRunPart", turnID)
	}
	return result, changed, nil
}

// removeRunPart removes the part beside a run whose record this node has just
// removed. ASKED FIRST, of the leader: purging an absent key would leave a
// marker every walk reads; a racing create removes itself
// ([FleetStore.MutateSandboxRunPart]). NEVER A PART BESIDE A RECORD: the turn
// may have relaunched, and taking its new log would drop its calls (no append
// creates one). So a part is purged only with no record beside it, THROUGH THE
// REVISION READ so a part written since is untouched, a loss retried within
// [casRetry]. A FAILURE IS LOGGED, NOT RETURNED: the record's removal landed,
// and an error would make a caller retry a decision with no record;
// [FleetStore.sweepRunParts] gets the part.
func (f *FleetStore) removeRunPart(ctx context.Context, turnID string, part coord.RunPart) {
	key := runPartKey(turnID, part)
	ctx = context.WithoutCancel(ctx)
	settled, err := casRetry(ctx, &f.runTrips, "remove the sandbox run's "+string(part)+" "+turnID, func() (bool, error) {
		entry, err := f.get(ctx, f.runs, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			return true, nil
		case err != nil:
			return false, err
		}
		_, err = f.get(ctx, f.runs, encodeKey(turnID))
		switch {
		case err == nil:
			// A NEW RUN OF THE TURN, whose part this is to manage.
			return true, nil
		case !errors.Is(err, jetstream.ErrKeyNotFound):
			return false, err
		}
		err = f.runs.Purge(ctx, key, jetstream.LastRevision(entry.Revision()))
		switch {
		case err == nil:
			return true, nil
		case lostUpdateRace(err):
			return false, nil
		default:
			return false, err
		}
	})
	if err == nil && !settled {
		err = contended("removeRunPart", turnID)
	}
	if err != nil {
		log.WarnContext(ctx, "coord_kv_sandbox_run_part_left", "turn_id", turnID, "part", string(part),
			"error", err.Error(),
			"detail", "the run's record is removed and this part of it is not; the marker sweep removes it")
	}
}

// sweepRunParts removes every part written before cutoff whose run has no
// record, off the runs bucket's marker pass, reporting how many messages went
// ([coord.Markers]). A candidate's record is confirmed absent from the LEADER,
// so a run recreated after the pass keeps its parts, and the part is purged
// THROUGH THE REVISION THE PASS SAW ([leaderPurge]), sparing one written since.
func (f *FleetStore) sweepRunParts(ctx context.Context, pass sweepPass, cutoff time.Time) (int64, error) {
	what := "the sandbox runs' parts"
	keys := make([]string, 0)
	for key, kve := range pass.keys {
		segments, ok := coord.DocumentSegments(key)
		if !ok || len(segments) != 2 || !coord.RunPart(segments[1]).Valid() ||
			kve.Operation() != jetstream.KeyValuePut || !kve.Created().Before(cutoff) {
			continue
		}
		if record, ok := pass.keys[encodeKey(segments[0])]; ok && record.Operation() == jetstream.KeyValuePut {
			continue
		}
		keys = append(keys, key)
	}
	// KEY ORDER, so a failure part-way names the same key on every retry.
	slices.Sort(keys)
	var swept int64
	for _, key := range keys {
		segments, _ := coord.DocumentSegments(key)
		_, err := f.get(ctx, f.runs, encodeKey(segments[0]))
		switch {
		case err == nil:
			continue
		case !errors.Is(err, jetstream.ErrKeyNotFound):
			return swept, unavailable("sweep "+what, fmt.Errorf("read the run of %q: %w", key, err))
		}
		n, err := pass.purge.through(ctx, key, pass.keys[key].Revision())
		if err != nil {
			return swept, unavailable("sweep "+what, fmt.Errorf("remove the part %q: %w", key, err))
		}
		swept += int64(n)
	}
	return swept, nil
}
