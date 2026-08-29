package memory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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
	budgets   map[string]coord.Usage
	channels  map[string]coord.Channel
	fires     map[string]time.Time
	runs      map[string]coord.Record
	secrets   map[string]coord.SecretRecord

	epoch   int64
	target  coord.Activation
	set     bool
	payload []byte
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
		budgets:   map[string]coord.Usage{},
		channels:  map[string]coord.Channel{},
		fires:     map[string]time.Time{},
		runs:      map[string]coord.Record{},
		secrets:   map[string]coord.SecretRecord{},
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
func (f *Fleet) Activate(_ context.Context, revisionID, summary string, payload []byte, at time.Time) (coord.Activation, error) {
	if revisionID == "" {
		return coord.Activation{}, errors.New("coord/memory: an activation needs a revision id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	// The payload lands before the pointer, matching the KV's two writes.
	// The twin gets both under one mutex, so the window cannot open here —
	// which is the point of holding it to the same order anyway: a reader
	// of this file should find the same invariant stated in both places.
	f.payload = slices.Clone(payload)
	f.epoch++
	f.target = coord.Activation{
		Epoch: f.epoch, RevisionID: revisionID, At: at.UTC(), Summary: summary,
	}
	f.set = true
	return f.target, nil
}

// Payload returns the current revision's sealed payload.
func (f *Fleet) Payload(_ context.Context, revisionID string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.set || f.target.RevisionID != revisionID || f.payload == nil {
		return nil, false, nil
	}
	return slices.Clone(f.payload), true, nil
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

// ---- the token counters ------------------------------------------------ //

// Charge checks and increments the org's counter and the seat's.
//
// The twin holds ONE mutex for the whole call, so the compensation the KV
// backend needs never runs here. That is not a shortcut around the contract —
// the observable behaviour is identical, and the suite asserts the behaviour
// — it is what a single process can honestly offer: there is no second writer
// to race, so building a compensation nothing could ever exercise would be a
// path with no test that could reach it.
func (f *Fleet) Charge(_ context.Context, agentScope string, tokens, orgLimit, agentLimit int) (coord.Spend, error) {
	if tokens <= 0 {
		return coord.Spend{OK: true}, nil
	}
	if agentScope == "" {
		return coord.Spend{}, errors.New("coord/memory: a charge needs a seat scope")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	// ORG FIRST for the REFUSAL, whichever order the writes go in: "the
	// company is out" is the fact an operator has to see when both scopes
	// are out of room.
	for _, scope := range []struct {
		name, key string
		limit     int
	}{{"org", coord.OrgScope, orgLimit}, {"agent", agentScope, agentLimit}} {
		used := f.budgets[scope.key].Used
		if scope.limit > 0 && used+tokens > scope.limit {
			return coord.Spend{
				RefusedScope: scope.name, RefusedUsed: used, RefusedLimit: scope.limit,
			}, nil
		}
	}
	orgUsed := f.charge(coord.OrgScope, tokens)
	agentUsed := f.charge(agentScope, tokens)
	return coord.Spend{OK: true, OrgUsed: orgUsed, AgentUsed: agentUsed}, nil
}

// charge applies one scope's delta under the held lock.
func (f *Fleet) charge(scope string, delta int) int {
	row := f.budgets[scope]
	row.Scope = scope
	row.Used = max(row.Used+delta, 0)
	row.UpdatedAt = time.Now().UTC()
	f.budgets[scope] = row
	return row.Used
}

// Used reports one scope's spend.
func (f *Fleet) Used(_ context.Context, scope string) (int, error) {
	if scope == "" {
		return 0, errors.New("coord/memory: a budget scope is required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.budgets[scope].Used, nil
}

// Usage returns every counter, org first then seats by scope.
func (f *Fleet) Usage(_ context.Context) ([]coord.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.Usage, 0, len(f.budgets))
	for _, row := range f.budgets {
		out = append(out, row)
	}
	coord.SortUsage(out)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// Reset zeroes one scope, or every scope when given "".
func (f *Fleet) Reset(_ context.Context, scope string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if scope != "" {
		if _, ok := f.budgets[scope]; !ok {
			return 0, nil
		}
		delete(f.budgets, scope)
		return 1, nil
	}
	cleared := len(f.budgets)
	clear(f.budgets)
	return cleared, nil
}

// ---- the agent-to-agent channels --------------------------------------- //

// OpenChannel records a new channel, ignoring an id that already exists.
//
// Ignoring rather than overwriting: the id is minted per ask, so a collision
// means a retried publish of ONE ask, and overwriting would reset the message
// counter and replace the participants of a channel already carrying an
// answer.
func (f *Fleet) OpenChannel(_ context.Context, ch coord.Channel) error {
	if ch.ID == "" {
		return errors.New("coord/memory: a channel needs an id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.channels[ch.ID]; exists {
		return nil
	}
	f.channels[ch.ID] = normalizeChannel(ch)
	return nil
}

// Channel reads one record.
func (f *Fleet) Channel(_ context.Context, id string) (coord.Channel, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[id]
	// A VALUE. coord.Channel holds no reference types, so the copy is
	// complete — a field that later does will break this, which is why the
	// suite asserts it rather than trusting the shape.
	return ch, ok, nil
}

// CloseChannel ends a channel, leaving an already-closed one untouched.
func (f *Fleet) CloseChannel(_ context.Context, id string, at time.Time) (coord.Channel, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[id]
	if !ok {
		return coord.Channel{}, false, nil
	}
	if ch.Open() {
		ch.ClosedAt = at.UTC()
		ch.LastAt = at.UTC()
		f.channels[id] = ch
	}
	return ch, true, nil
}

// CountChannelMessage records one message against a channel's own budget.
func (f *Fleet) CountChannelMessage(_ context.Context, id string, at time.Time) (coord.Channel, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[id]
	if !ok {
		return coord.Channel{}, false, nil
	}
	ch.Messages++
	ch.LastAt = at.UTC()
	f.channels[id] = ch
	return ch, true, nil
}

// OpenChannels returns every channel still open, by id.
func (f *Fleet) OpenChannels(context.Context) ([]coord.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []coord.Channel
	for _, id := range slices.Sorted(maps.Keys(f.channels)) {
		if ch := f.channels[id]; ch.Open() {
			out = append(out, ch)
		}
	}
	return out, nil
}

// PurgeChannels deletes channels closed before the cutoff.
func (f *Fleet) PurgeChannels(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, ch := range f.channels {
		if !ch.Open() && ch.ClosedAt.Before(cutoff) {
			delete(f.channels, id)
			n++
		}
	}
	return n, nil
}

// normalizeChannel puts every stamp in UTC.
//
// The KV backend gets this for free — JSON round-trips a time.Time through
// RFC 3339 and back in UTC — so the twin has to do it explicitly or the two
// backends disagree about a stamp a caller handed in with a zone.
func normalizeChannel(ch coord.Channel) coord.Channel {
	ch.OpenedAt = ch.OpenedAt.UTC()
	ch.LastAt = ch.LastAt.UTC()
	if !ch.ClosedAt.IsZero() {
		ch.ClosedAt = ch.ClosedAt.UTC()
	}
	return ch
}

// ---- the scheduled-fire claims ----------------------------------------- //

// ClaimFire records one fire identity, reporting whether this call wrote it.
func (f *Fleet) ClaimFire(_ context.Context, key string, at time.Time) (bool, error) {
	if key == "" {
		return false, errors.New("coord/memory: a fire claim needs a key")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, taken := f.fires[key]; taken {
		return false, nil
	}
	f.fires[key] = at.UTC()
	return true, nil
}

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
	// Versions start at 1 so a zero version is always a lost race, which
	// is what a caller that forgot to read one deserves.
	f.runs[turnID] = coord.Record{Key: turnID, Value: slices.Clone(value), Version: 1}
	return true, nil
}

// UpdateSandboxRun writes at a version, reporting whether that version held.
func (f *Fleet) UpdateSandboxRun(_ context.Context, turnID string, value []byte, version uint64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.runs[turnID]
	if !ok || record.Version != version {
		return false, nil
	}
	f.runs[turnID] = coord.Record{Key: turnID, Value: slices.Clone(value), Version: version + 1}
	return true, nil
}

// DeleteSandboxRun removes a record at a version.
func (f *Fleet) DeleteSandboxRun(_ context.Context, turnID string, version uint64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.runs[turnID]
	if !ok || record.Version != version {
		return false, nil
	}
	delete(f.runs, turnID)
	return true, nil
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

// ---- the sealed credentials -------------------------------------------- //
//
// The twin stores what it is given, exactly as the KV does: the value is an
// envelope the Tier A keyring produced before it arrived, and neither backend
// can open one. A twin that "helpfully" held plaintext would certify a
// contract the real store does not implement.

// Secret reads one sealed value.
func (f *Fleet) Secret(_ context.Context, name string) (coord.SecretRecord, bool, error) {
	if name == "" {
		return coord.SecretRecord{}, false, errors.New("coord/memory: a secret needs a name")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.secrets[name]
	return rec, ok, nil
}

// SecretValues returns every sealed value, by name.
func (f *Fleet) SecretValues(_ context.Context) ([]coord.SecretRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.SecretRecord, 0, len(f.secrets))
	for _, rec := range f.secrets {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// PutSecret writes a sealed value, replacing any prior one.
func (f *Fleet) PutSecret(_ context.Context, rec coord.SecretRecord) error {
	switch {
	case rec.Name == "":
		return errors.New("coord/memory: a secret needs a name")
	case rec.Value == "":
		return fmt.Errorf("coord/memory: secret %q has no sealed value", rec.Name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec.UpdatedAt = rec.UpdatedAt.UTC()
	f.secrets[rec.Name] = rec
	return nil
}

// DeleteSecret removes a value, reporting whether it was there.
func (f *Fleet) DeleteSecret(_ context.Context, name string) (bool, error) {
	if name == "" {
		return false, errors.New("coord/memory: a secret needs a name")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.secrets[name]; !ok {
		return false, nil
	}
	delete(f.secrets, name)
	return true, nil
}
