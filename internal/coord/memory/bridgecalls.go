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
// Keyed by LAUNCH, with each launch's own numbering beside its records, which
// is the shape the KV backend has: one counter key and one record key per call
// under the launch. A purge removes both together, so the next append to a
// purged launch starts again at 1 on either backend.

// bridgeLaunch is one launch's log.
type bridgeLaunch struct {
	// next is the last Seq allocated. Allocation and write happen under
	// the fleet's one mutex, so no number is ever taken and left unused
	// here — which the contract allows and the KV backend can do.
	next  uint64
	calls map[uint64][]byte
}

// AppendBridgeCall files one call under its launch.
func (f *Fleet) AppendBridgeCall(_ context.Context, turnID, launchID string, value []byte) (uint64, error) {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return 0, err
	}
	if len(value) > coord.MaxBridgeCallBytes {
		// THE KV BACKEND'S CEILING, stated by the contract so that this
		// twin refuses what the real broker refuses rather than storing a
		// record production never could.
		return 0, fmt.Errorf("coord/memory: a bridged call of %d bytes is past the %d-byte "+
			"ceiling one record may hold (coord.MaxBridgeCallBytes): %w",
			len(value), coord.MaxBridgeCallBytes, coord.ErrTooLarge)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bridge == nil {
		f.bridge = map[coord.BridgeLaunch]*bridgeLaunch{}
	}
	key := coord.BridgeLaunch{TurnID: turnID, LaunchID: launchID}
	launch := f.bridge[key]
	if launch == nil {
		launch = &bridgeLaunch{calls: map[uint64][]byte{}}
		f.bridge[key] = launch
	}
	launch.next++
	launch.calls[launch.next] = slices.Clone(value)
	return launch.next, nil
}

// BridgeCalls returns every call of one launch, in Seq order.
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
		out = append(out, bridgeRecord(turnID, launchID, seq, launch.calls[seq]))
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
func (f *Fleet) BridgeLaunches(context.Context) ([]coord.BridgeLaunch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Collect(maps.Keys(f.bridge))
	slices.SortFunc(out, func(a, b coord.BridgeLaunch) int {
		return cmp.Or(cmp.Compare(a.TurnID, b.TurnID), cmp.Compare(a.LaunchID, b.LaunchID))
	})
	return out, nil
}

// PurgeBridgeCalls removes every record of one launch, and its numbering.
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

func validBridgeLaunch(turnID, launchID string) error {
	if turnID == "" || launchID == "" {
		return errors.New("coord/memory: a bridged call needs a turn id and a launch id")
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
