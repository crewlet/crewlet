package memory

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// Fleet is the in-process twin of the fleet-shared state.
//
// A SEMANTIC twin, held to the same suite as the KV backend, which means it
// has to model the things a naive map would not:
//
//   - Expiry against a stored deadline, so a claim that has lapsed is gone
//     whether or not anything has swept.
//   - A monotonic activation epoch that survives every write, because the
//     epoch is a fencing token: a counter that restarted would hand a node a
//     number an older revision already used.
//   - A rate window keyed by its own start instant, so two callers in the
//     same window share a counter and the next window starts at zero without
//     anything having to delete the old one.
type Fleet struct {
	mu sync.Mutex

	windows   map[windowKey]int
	claims    map[string]time.Time
	worked    map[string]workedEntry
	cooldowns map[string]time.Time
	applies   map[string]coord.NodeApply

	epoch  int64
	target coord.Activation
	set    bool
}

type windowKey struct {
	bucket string
	start  int64
}

type workedEntry struct {
	at     time.Time
	detail string
}

var _ coord.Fleet = (*Fleet)(nil)

// NewFleet returns an empty twin.
func NewFleet() *Fleet {
	return &Fleet{
		windows:   map[windowKey]int{},
		claims:    map[string]time.Time{},
		worked:    map[string]workedEntry{},
		cooldowns: map[string]time.Time{},
		applies:   map[string]coord.NodeApply{},
	}
}

// Allow increments a bucket's window and reports whether it stayed in limit.
func (f *Fleet) Allow(_ context.Context, bucket string, limit int, window time.Duration, now time.Time) (bool, error) {
	if bucket == "" {
		return false, errors.New("coord/memory: a rate bucket needs a name")
	}
	if limit <= 0 || window <= 0 {
		// A limit of zero means "allow nothing", and a caller reaching
		// here with one has already decided; answering false is the only
		// reading of it that is not an invitation to divide by zero.
		return false, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	key := windowKey{bucket: bucket, start: now.Truncate(window).UnixNano()}
	// Older windows are dropped as they are passed rather than swept: the
	// twin runs inside one test process, and a sweep would be state the KV
	// backend does not have — the server expires those keys.
	for existing := range f.windows {
		if existing.bucket == bucket && existing.start < key.start {
			delete(f.windows, existing)
		}
	}
	if f.windows[key] >= limit {
		return false, nil
	}
	f.windows[key]++
	return true, nil
}

// Claim records a key, reporting whether this caller was first.
func (f *Fleet) Claim(_ context.Context, key string, ttl time.Duration, now time.Time) (bool, error) {
	if key == "" {
		return false, errors.New("coord/memory: a claim needs a key")
	}
	if ttl <= 0 {
		return false, errors.New("coord/memory: a claim needs a positive ttl")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if until, held := f.claims[key]; held && until.After(now) {
		return false, nil
	}
	// A LAPSED claim is re-claimable, which is what makes a deliberate
	// replay work: the record is not a tombstone, it is a window.
	f.claims[key] = now.Add(ttl)
	return true, nil
}

// Release drops a claim.
func (f *Fleet) Release(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claims, key)
	return nil
}

// Worked returns the subset of keys already recorded under scope.
func (f *Fleet) Worked(_ context.Context, scope string, keys []string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		if _, done := f.worked[scope+"\x00"+key]; done {
			out[key] = true
		}
	}
	return out, nil
}

// Record marks one key worked.
func (f *Fleet) Record(_ context.Context, scope, key, detail string, at time.Time) error {
	if scope == "" || key == "" {
		return errors.New("coord/memory: a ledger entry needs a scope and a key")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	// FIRST WRITER WINS. A second Record for one key is not an error and
	// does not overwrite: two nodes completing one trigger is exactly the
	// case the ledger exists to collapse, and the first one's detail is
	// the one that describes the turn that actually ran.
	id := scope + "\x00" + key
	if _, done := f.worked[id]; done {
		return nil
	}
	f.worked[id] = workedEntry{at: at, detail: detail}
	return nil
}

// Cool records a credential as unusable until an instant.
func (f *Fleet) Cool(_ context.Context, key string, until time.Time) error {
	if key == "" {
		return errors.New("coord/memory: a cooldown needs a key")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	// The LONGER of the two survives: two nodes cooling the same key have
	// each seen a real refusal, and shortening a peer's cooldown would send
	// this node back at a credential the peer already knows is spent.
	if existing, ok := f.cooldowns[key]; ok && existing.After(until) {
		return nil
	}
	f.cooldowns[key] = until
	return nil
}

// Since returns every cooldown that has not yet lapsed.
func (f *Fleet) Since(_ context.Context, now time.Time) (map[string]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := map[string]time.Time{}
	for key, until := range f.cooldowns {
		if until.After(now) {
			out[key] = until
			continue
		}
		delete(f.cooldowns, key)
	}
	return out, nil
}

// Activate publishes a new target revision.
func (f *Fleet) Activate(_ context.Context, revisionID, summary string, at time.Time) (coord.Activation, error) {
	if revisionID == "" {
		return coord.Activation{}, errors.New("coord/memory: an activation needs a revision id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	f.epoch++
	f.target = coord.Activation{
		Epoch: f.epoch, RevisionID: revisionID, At: at.UTC(), Summary: summary,
	}
	f.set = true
	return f.target, nil
}

// Target reads the pointer.
func (f *Fleet) Target(context.Context) (coord.Activation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.set {
		return coord.Activation{}, false, nil
	}
	return f.target, true, nil
}

// RecordApply publishes this node's status for an epoch.
func (f *Fleet) RecordApply(_ context.Context, status coord.NodeApply) error {
	if status.NodeID == "" {
		return errors.New("coord/memory: an apply status needs a node id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	status.UpdatedAt = status.UpdatedAt.UTC()
	status.Error = coord.TruncateApplyError(status.Error)
	f.applies[status.NodeID] = status
	return nil
}

// Fleet returns every node's last status, freshest first.
func (f *Fleet) Fleet(context.Context) ([]coord.NodeApply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]coord.NodeApply, 0, len(f.applies))
	for _, status := range f.applies {
		out = append(out, status)
	}
	// Freshest first, and the node id breaks a tie, so two nodes that
	// reported in the same millisecond order the same way on every read.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out, nil
}

// ExpireApplies drops apply statuses older than the cutoff.
//
// The twin's stand-in for the KV bucket's own MaxAge, which the server
// enforces. Exported so the contract suite can drive the same freshness
// behaviour against both.
func (f *Fleet) ExpireApplies(cutoff time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for node, status := range f.applies {
		if status.UpdatedAt.Before(cutoff) {
			delete(f.applies, node)
		}
	}
}
