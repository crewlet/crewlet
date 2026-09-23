package memory

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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

	windows      map[windowKey]int
	claims       map[string]time.Time
	worked       map[string]workedEntry
	cooldowns    map[string]time.Time
	applies      map[string]coord.NodeApply
	budgets      map[string]coord.Tally
	channels     map[string]coord.Channel
	follows      map[string]followEntry
	fires        map[string]time.Time
	runs         map[string]coord.Record
	secrets      map[string]coord.SecretRecord
	integrations map[string][]byte
	mailboxes    map[string]coord.MailboxRecord
	positions    map[string]coord.NodePositions
	holds        map[string]coord.TrimHold
	floors       map[string]coord.TrimFloor
	backups      map[string]coord.BackupPoint
	maintenance  map[string]coord.MaintenanceOperation
	admissions   map[string]coord.Admission
	maintAcks    map[string]coord.MaintenanceAck
	maintRev     uint64

	// version is the one counter every versioned write draws from, sandbox
	// runs and mailbox records alike. Store-wide rather than per record, as
	// a KV revision is, so a record deleted and created again never hands
	// back a version an older incarnation of it already used: a caller still
	// holding that version must lose, not win against a record it never
	// read. A per-record counter restarting at 1 was exactly that bug.
	version uint64

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
		windows:      map[windowKey]int{},
		claims:       map[string]time.Time{},
		worked:       map[string]workedEntry{},
		cooldowns:    map[string]time.Time{},
		applies:      map[string]coord.NodeApply{},
		budgets:      map[string]coord.Tally{},
		channels:     map[string]coord.Channel{},
		follows:      map[string]followEntry{},
		fires:        map[string]time.Time{},
		runs:         map[string]coord.Record{},
		secrets:      map[string]coord.SecretRecord{},
		integrations: map[string][]byte{},
		mailboxes:    map[string]coord.MailboxRecord{},
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
func (f *Fleet) Activate(_ context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	if req.RevisionID == "" {
		return coord.Activation{}, errors.New("coord/memory: an activation needs a revision id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	// THE EXPECTATION FIRST, and before the payload write, so a caller
	// that has already lost the race leaves nothing behind — the same
	// ordering the KV holds, for the same reason.
	//
	// The mutex makes the compare and the set one step here, which is
	// exactly what the KV buys with a sequence: the twin has to REFUSE the
	// same writes, not merely be safe by construction, or a suite that
	// passes against it proves nothing about the real store.
	// An unset pointer is NOT a race — see the KV's own note and
	// [coord.ActivationRequest.Expect].
	if req.Expect != "" && req.ExpectAbsent {
		return coord.Activation{}, errors.New("coord/memory: an activation cannot " +
			"expect a revision and no revision at once")
	}
	if req.Expect != "" && f.set && f.target.RevisionID != req.Expect {
		return coord.Activation{}, fmt.Errorf(
			"%w: expected %s, the fleet is on %s",
			coord.ErrActivationRaced, req.Expect, f.target.RevisionID)
	}
	// CREATE-ONLY, and the comparison an expectation cannot make: this
	// write was built on nothing, so any pointer at all is a race with it.
	if req.ExpectAbsent && f.set {
		return coord.Activation{}, fmt.Errorf(
			"%w: expected no activation, the fleet is on %s",
			coord.ErrActivationRaced, f.target.RevisionID)
	}

	// The payload lands before the pointer, matching the KV's two writes.
	// The twin gets both under one mutex, so the window cannot open here —
	// which is the point of holding it to the same order anyway: a reader
	// of this file should find the same invariant stated in both places.
	f.payload = slices.Clone(req.Payload)
	f.epoch++
	f.target = coord.Activation{
		Epoch: f.epoch, RevisionID: req.RevisionID,
		At: req.At.UTC(), Summary: req.Summary,
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
	slices.SortFunc(out, func(a, b coord.NodeApply) int {
		// NEWEST FIRST, so the negated compare; the node id breaks a tie
		// ascending so one instant's statuses have a stable order.
		return cmp.Or(b.UpdatedAt.Compare(a.UpdatedAt), cmp.Compare(a.NodeID, b.NodeID))
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

// Charge checks and increments the org's counter and the seat's, in every
// window.
//
// The twin holds ONE mutex for the whole call, so the compensation the KV
// backend needs never runs here. That is not a shortcut around the contract —
// the observable behaviour is identical, and the suite asserts the behaviour
// — it is what a single process can honestly offer: there is no second writer
// to race, so building a compensation nothing could ever exercise would be a
// path with no test that could reach it. The arithmetic is [coord.Tally]'s,
// the same the KV backend runs.
func (f *Fleet) Charge(_ context.Context, req coord.ChargeRequest) (coord.Spend, error) {
	if req.Tokens <= 0 {
		return coord.Spend{OK: true}, nil
	}
	if err := req.Validate(); err != nil {
		return coord.Spend{}, fmt.Errorf("coord/memory: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	// ORG FIRST for the REFUSAL, whichever order the writes go in: "the
	// company is out" is the fact an operator has to see when both scopes
	// are out of room.
	now := time.Now().UTC()
	org := f.budgets[coord.OrgScope].Roll(req.Windows)
	seat := f.budgets[req.Seat].Roll(req.Windows)
	for _, scope := range []struct {
		name, key string
		tally     coord.Tally
		caps      coord.Caps
	}{{"org", coord.OrgScope, org, req.OrgCaps}, {"agent", req.Seat, seat, req.SeatCaps}} {
		if refusing := scope.tally.Refusing(req.Tokens, scope.caps); len(refusing) > 0 {
			// The refusal is recorded on the windows of the scope that
			// made it, and on no other: see coord.WindowUsage.RefusedAt.
			f.budgets[scope.key] = scope.tally.Stamp(refusing, scope.tally, now)
			return scope.tally.Refusal(scope.name, refusing, scope.caps, req.Windows), nil
		}
	}
	// ADMITTED, which is also what clears both scopes' refusals: each has
	// just had room in every window.
	org, seat = org.Add(req.Tokens, now).ClearAll(), seat.Add(req.Tokens, now).ClearAll()
	f.budgets[coord.OrgScope], f.budgets[req.Seat] = org, seat
	return coord.Spend{
		OK: true, Org: org.Usage(coord.OrgScope, req.Windows), Agent: seat.Usage(req.Seat, req.Windows),
	}, nil
}

// PostCharge adds spend that already happened to both counters, refusing
// nothing and leaving both refusal stamps as they were. See
// [coord.Budgets.PostCharge].
func (f *Fleet) PostCharge(_ context.Context, seat string, tokens int, windows coord.Windows) (coord.Spend, error) {
	if tokens <= 0 {
		return coord.Spend{OK: true}, nil
	}
	if seat == "" {
		return coord.Spend{}, errors.New("coord/memory: a charge needs a seat scope")
	}
	if err := windows.Validate(); err != nil {
		return coord.Spend{}, fmt.Errorf("coord/memory: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	org := f.budgets[coord.OrgScope].Roll(windows).Add(tokens, now)
	agent := f.budgets[seat].Roll(windows).Add(tokens, now)
	f.budgets[coord.OrgScope], f.budgets[seat] = org, agent
	return coord.Spend{
		OK: true, Org: org.Usage(coord.OrgScope, windows), Agent: agent.Usage(seat, windows),
	}, nil
}

// Used reports one scope's counter against the given windows.
func (f *Fleet) Used(_ context.Context, scope string, windows coord.Windows) (coord.Usage, error) {
	if scope == "" {
		return coord.Usage{}, errors.New("coord/memory: a budget scope is required")
	}
	if err := windows.Validate(); err != nil {
		return coord.Usage{}, fmt.Errorf("coord/memory: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.budgets[scope].Usage(scope, windows), nil
}

// Usage returns every counter against the given windows, org first then seats
// by scope.
//
// Nothing here ages a counter out: the twin lives for one process, a far
// shorter span than [coord.BudgetRetention], which is the KV bucket's age and
// no part of what a caller can observe of a counter it is still charging.
func (f *Fleet) Usage(_ context.Context, windows coord.Windows) ([]coord.Usage, error) {
	if err := windows.Validate(); err != nil {
		return nil, fmt.Errorf("coord/memory: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.Usage, 0, len(f.budgets))
	for scope, tally := range f.budgets {
		out = append(out, tally.Usage(scope, windows))
	}
	coord.SortUsage(out)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// RetireLifetimeCounters has nothing to retire: the twin lives and dies with
// its process, so no earlier build's counters can exist in it.
func (f *Fleet) RetireLifetimeCounters(context.Context) (bool, error) {
	return false, nil
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
	return f.listChannels(coord.Channel.Open), nil
}

// AllChannels returns every channel this store still holds, by id.
func (f *Fleet) AllChannels(context.Context) ([]coord.Channel, error) {
	return f.listChannels(func(coord.Channel) bool { return true }), nil
}

// listChannels is the one walk both listings take — see the KV store's own
// note: two near-identical loops are how one of them stops matching.
func (f *Fleet) listChannels(keep func(coord.Channel) bool) []coord.Channel {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []coord.Channel
	for _, id := range slices.Sorted(maps.Keys(f.channels)) {
		if ch := f.channels[id]; keep(ch) {
			out = append(out, ch)
		}
	}
	return out
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
	f.version++
	f.runs[turnID] = coord.Record{Key: turnID, Value: slices.Clone(value), Version: f.version}
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
	f.version++
	f.runs[turnID] = coord.Record{Key: turnID, Value: slices.Clone(value), Version: f.version}
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
	slices.SortFunc(out, func(a, b coord.SecretRecord) int { return cmp.Compare(a.Name, b.Name) })
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

// ---- the integration reconcile status ---------------------------------- //

// IntegrationStatuses returns every recorded status, keyed by surface.
func (f *Fleet) IntegrationStatuses(context.Context) (map[string][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.integrations))
	for kind, value := range f.integrations {
		// CLONED on the way out, as on the way in. A caller handed the
		// stored slice could mutate what the next reader sees, which the
		// KV backend makes impossible and a twin has to model rather than
		// merely usually get away with.
		out[kind] = bytes.Clone(value)
	}
	return out, nil
}

// PutIntegrationStatus records one surface's status.
func (f *Fleet) PutIntegrationStatus(_ context.Context, kind string, value []byte) error {
	if kind == "" {
		return errors.New("coord/memory: an integration status needs a surface name")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.integrations[kind] = bytes.Clone(value)
	return nil
}

// DeleteIntegrationStatus drops a surface's status. Deleting one that is not
// there is the outcome asked for rather than an error, matching the KV
// backend's purge of an absent key.
func (f *Fleet) DeleteIntegrationStatus(_ context.Context, kind string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.integrations, kind)
	return nil
}

// ---- the chat thread-follows ------------------------------------------- //

// followEntry is one follow in the twin.
//
// The instant is kept although nothing here reads it, so the twin holds the
// same record the real backend does: a test that asserted on a field the twin
// silently dropped would pass against a backend that never stored it.
type followEntry struct {
	reason string
	at     time.Time
}

// followKey composes one follow's key, through the SAME grammar the real
// backend uses — so a segment containing a separator collides in both or in
// neither, and the conformance suite's case for it means something.
func followKey(backend, handle, channel, thread string) string {
	return coord.DocumentKey("follow", backend, handle, channel, thread)
}

// Follow records or refreshes a seat's follow on a thread.
func (f *Fleet) Follow(_ context.Context, backend, handle, channel, thread, reason string, at time.Time) error {
	if backend == "" || handle == "" || thread == "" {
		return errors.New("coord/memory: a follow needs a backend, a handle and a thread")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.follows[followKey(backend, handle, channel, thread)] = followEntry{reason: reason, at: at.UTC()}
	return nil
}

// Following reports why a seat follows a thread, and whether it does.
func (f *Fleet) Following(_ context.Context, backend, handle, channel, thread string) (string, bool, error) {
	if backend == "" || handle == "" || thread == "" {
		return "", false, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.follows[followKey(backend, handle, channel, thread)]
	if !ok {
		return "", false, nil
	}
	return entry.reason, true, nil
}

// FollowIfAbsent records a follow only where none exists, reporting whether
// this call created it.
//
// ONE MUTEX ACROSS THE CHECK AND THE WRITE, which is what the KV backend buys
// with Create — see [coord.Follows.FollowIfAbsent] for what depends on it.
func (f *Fleet) FollowIfAbsent(_ context.Context, backend, handle, channel, thread, reason string, at time.Time) (bool, error) {
	if backend == "" || handle == "" || thread == "" {
		return false, errors.New("coord/memory: a follow needs a backend, a handle and a thread")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := followKey(backend, handle, channel, thread)
	if _, held := f.follows[key]; held {
		return false, nil
	}
	f.follows[key] = followEntry{reason: reason, at: at.UTC()}
	return true, nil
}

// Unfollow drops a follow, reporting whether one was there.
func (f *Fleet) Unfollow(_ context.Context, backend, handle, channel, thread string) (bool, error) {
	if backend == "" || handle == "" || thread == "" {
		return false, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := followKey(backend, handle, channel, thread)
	if _, ok := f.follows[key]; !ok {
		return false, nil
	}
	delete(f.follows, key)
	return true, nil
}
