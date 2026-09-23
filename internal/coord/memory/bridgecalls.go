package memory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
)

// The bridged-call log, in memory.
//
// Keyed by LAUNCH, with each launch's own numbering beside its records and the
// parts filed under them, which is the shape the KV backend has: one counter
// key, one record key per call and one key per part, all under the launch. A
// purge removes the launch's entry and so all three together, so the next
// append to a purged launch starts again at 1 on either backend, and no part
// outlives the purge of its call.

// bridgeLaunch is one launch's log.
type bridgeLaunch struct {
	// next is the last Seq handed out. A number can be taken and left
	// unused — reserved for a call whose record was then never filed — as
	// the contract allows and the KV backend does.
	next  uint64
	calls map[uint64][]byte

	// parts are the part records filed under each call's number, by part.
	// Held apart from calls because a part is not a call: nothing that
	// lists or counts the calls reaches this map.
	parts map[uint64]map[int][]byte
}

// launch returns a launch's entry, creating it: a write to a purged launch
// brings its entry back, as a write lands on the KV backend whatever was
// purged before it.
func (f *Fleet) launch(turnID, launchID string) *bridgeLaunch {
	if f.bridge == nil {
		f.bridge = map[coord.BridgeLaunch]*bridgeLaunch{}
	}
	key := coord.BridgeLaunch{TurnID: turnID, LaunchID: launchID}
	launch := f.bridge[key]
	if launch == nil {
		launch = &bridgeLaunch{calls: map[uint64][]byte{}, parts: map[uint64]map[int][]byte{}}
		f.bridge[key] = launch
	}
	return launch
}

// AppendBridgeCall files one call under its launch.
//
// A number that already holds a record is passed over rather than overwritten,
// as the KV backend passes it over: a record filed at a number this launch's
// counter had not reached yet — by a create after a purge reset the counter —
// is somebody's call, and the append takes the next number instead.
func (f *Fleet) AppendBridgeCall(_ context.Context, turnID, launchID string, value []byte) (uint64, error) {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return 0, err
	}
	if err := withinCeiling("a bridged call", value); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	launch := f.launch(turnID, launchID)
	for {
		launch.next++
		if _, taken := launch.calls[launch.next]; taken {
			continue
		}
		launch.calls[launch.next] = slices.Clone(value)
		return launch.next, nil
	}
}

// ReserveBridgeCall takes the launch's next number and files nothing at it.
func (f *Fleet) ReserveBridgeCall(_ context.Context, turnID, launchID string) (uint64, error) {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	launch := f.launch(turnID, launchID)
	launch.next++
	return launch.next, nil
}

// CreateBridgeCall files a call's record at a number ReserveBridgeCall took.
func (f *Fleet) CreateBridgeCall(_ context.Context, turnID, launchID string, seq uint64, value []byte) (bool, error) {
	if err := validBridgeAddress(turnID, launchID, seq, 1); err != nil {
		return false, err
	}
	if err := withinCeiling("a bridged call", value); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	launch := f.launch(turnID, launchID)
	if _, taken := launch.calls[seq]; taken {
		return false, nil
	}
	launch.calls[seq] = slices.Clone(value)
	return true, nil
}

// CreateBridgeCallPart files one part of a call's whole under the call.
func (f *Fleet) CreateBridgeCallPart(_ context.Context, turnID, launchID string, seq uint64, part int, value []byte) (bool, error) {
	if err := validBridgeAddress(turnID, launchID, seq, part); err != nil {
		return false, err
	}
	if err := withinCeiling("a part of a bridged call", value); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	launch := f.launch(turnID, launchID)
	parts := launch.parts[seq]
	if parts == nil {
		parts = map[int][]byte{}
		launch.parts[seq] = parts
	}
	if _, taken := parts[part]; taken {
		return false, nil
	}
	parts[part] = slices.Clone(value)
	return true, nil
}

// BridgeCalls returns every call of one launch, in Seq order, with its parts.
func (f *Fleet) BridgeCalls(_ context.Context, turnID, launchID string) ([]coord.BridgeCallRecord, error) {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	launch := f.bridge[coord.BridgeLaunch{TurnID: turnID, LaunchID: launchID}]
	if launch == nil {
		return []coord.BridgeCallRecord{}, nil
	}
	out := make([]coord.BridgeCallRecord, 0, len(launch.calls))
	for _, seq := range slices.Sorted(maps.Keys(launch.calls)) {
		record := bridgeRecord(turnID, launchID, seq, launch.calls[seq])
		for _, part := range slices.Sorted(maps.Keys(launch.parts[seq])) {
			record.Parts = append(record.Parts, coord.BridgeCallPart{
				Part: part, Value: slices.Clone(launch.parts[seq][part]),
			})
		}
		out = append(out, record)
	}
	return out, nil
}

// BridgeCallPage returns one page of one launch's calls.
func (f *Fleet) BridgeCallPage(_ context.Context, q coord.BridgeCallQuery) (coord.BridgeCallPage, error) {
	if err := validBridgeQuery(q); err != nil {
		return coord.BridgeCallPage{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	launch := f.bridge[coord.BridgeLaunch{TurnID: q.TurnID, LaunchID: q.LaunchID}]
	if launch == nil {
		return coord.BridgeCallPage{Calls: []coord.BridgeCallRecord{}}, nil
	}
	seqs := slices.Sorted(maps.Keys(launch.calls))
	page := coord.BridgeCallPage{Calls: []coord.BridgeCallRecord{}, Total: len(seqs)}
	// What the cursor admits, in the order the page takes it: oldest first,
	// or newest first for the end of the log.
	admitted := slices.DeleteFunc(seqs, func(seq uint64) bool { return seq <= q.After })
	if q.Last {
		slices.Reverse(admitted)
	}
	weight := 0
	for _, seq := range admitted {
		value := launch.calls[seq]
		if len(page.Calls) == q.Limit || (len(page.Calls) > 0 && weight+len(value) > q.MaxBytes) {
			// A call past the page is the evidence that there is more.
			page.More = true
			break
		}
		weight += len(value)
		page.Calls = append(page.Calls, bridgeRecord(q.TurnID, q.LaunchID, seq, value))
	}
	if q.Last {
		// Taken newest first; handed back in the order the run made them.
		slices.Reverse(page.Calls)
	}
	return page, nil
}

// BridgeLaunches returns every launch that holds any key in the log.
//
// Every entry in the map, whatever it holds: an entry exists only once
// something was written under its launch, and a counter, a record or a part
// alone is each a key the KV backend lists the launch for.
func (f *Fleet) BridgeLaunches(context.Context) ([]coord.BridgeLaunch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Collect(maps.Keys(f.bridge))
	slices.SortFunc(out, func(a, b coord.BridgeLaunch) int {
		return cmp.Or(cmp.Compare(a.TurnID, b.TurnID), cmp.Compare(a.LaunchID, b.LaunchID))
	})
	return out, nil
}

// PurgeBridgeCalls removes every record of one launch, the parts under them,
// and its numbering.
func (f *Fleet) PurgeBridgeCalls(_ context.Context, turnID, launchID string) error {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.bridge, coord.BridgeLaunch{TurnID: turnID, LaunchID: launchID})
	return nil
}

// bridgeRecord hands back a record whose bytes the caller cannot write
// through, for the reason [copyRecord] gives.
func bridgeRecord(turnID, launchID string, seq uint64, value []byte) coord.BridgeCallRecord {
	return coord.BridgeCallRecord{
		TurnID: turnID, LaunchID: launchID, Seq: seq, Value: slices.Clone(value),
	}
}

// withinCeiling refuses a value the KV backend's broker would refuse.
//
// THE KV BACKEND'S CEILING, stated by the contract so that this twin refuses
// what the real broker refuses rather than storing a record production never
// could.
func withinCeiling(what string, value []byte) error {
	if len(value) > coord.MaxBridgeCallBytes {
		return fmt.Errorf("coord/memory: %s of %d bytes is past the %d-byte ceiling one "+
			"record may hold (coord.MaxBridgeCallBytes): %w",
			what, len(value), coord.MaxBridgeCallBytes, coord.ErrTooLarge)
	}
	return nil
}

func validBridgeLaunch(turnID, launchID string) error {
	if turnID == "" || launchID == "" {
		return errors.New("coord/memory: a bridged call needs a turn id and a launch id")
	}
	return nil
}

// validBridgeAddress refuses an address nothing numbers: a call is numbered
// from 1 and so is each of its parts.
func validBridgeAddress(turnID, launchID string, seq uint64, part int) error {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return err
	}
	if seq == 0 || part < 1 {
		return fmt.Errorf("coord/memory: a bridged call is numbered from 1 and so is each "+
			"part of it, got call %d part %d", seq, part)
	}
	return nil
}

func validBridgeQuery(q coord.BridgeCallQuery) error {
	if err := validBridgeLaunch(q.TurnID, q.LaunchID); err != nil {
		return err
	}
	if q.Limit < 1 || q.MaxBytes < 1 {
		return fmt.Errorf("coord/memory: a page of bridged calls needs a limit and a byte "+
			"budget of at least one, got %d and %d", q.Limit, q.MaxBytes)
	}
	return nil
}
