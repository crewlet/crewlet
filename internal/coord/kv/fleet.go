package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// The fleet-shared state on JetStream KV.
//
// # Why SIX buckets and not one
//
// The package doc records the constraint this whole file is shaped by: a
// bucket's TTL is its stream's MaxAge, and jetstream.KeyTTL is create-only —
// an Update CLEARS it, leaving the key immortal. So retention is a property
// of the BUCKET, and each of these has a genuinely different one:
//
//	rate       one window's width x a small factor — a window nobody writes
//	           to again must age out, and nothing may outlive its successor
//	claims     the dedupe window, minutes: long enough to cover a vendor's
//	           retries and an operator's replay, short enough that a
//	           deliberate re-send later is not swallowed
//	ledger     turn-completion retention, days: it has to outlast the
//	           redelivery horizon and the scheduler's catchup floor
//	cooldowns  the longest credential cooldown, an hour
//	status     peer freshness, a minute: a node that STOPS reporting must
//	           vanish from the fleet view, which the bucket does for free
//	config     none at all: the activation pointer is the fencing sequence,
//	           and a pointer that expired would restart the epoch
//
// Putting two of those in one bucket would give one of them the other's
// retention, and every such mistake is silent — a cooldown that expired in a
// second, a fleet view showing a node that died last week.
//
// # The activation epoch IS the pointer's revision
//
// JetStream assigns each key write a monotonic revision. Publishing the
// pointer therefore appends and flips in ONE write, which is the atomicity
// d-201 asks for: there is no instant where a node can read an epoch whose
// revision has not been published, and two nodes activating at once get two
// different revisions rather than racing over a counter this engine keeps.

const (
	rateSuffix      = "_rate"
	claimsSuffix    = "_claims"
	ledgerSuffix    = "_ledger"
	cooldownSuffix  = "_cooldowns"
	statusSuffix    = "_status"
	configSuffix    = "_config"
	activationKey   = "activation"
	fleetCASRetries = 16
)

// FleetConfig is what a [FleetStore] needs at construction. Every duration is
// a BUCKET's retention; see the file doc for why each is its own bucket.
type FleetConfig struct {
	// BucketPrefix names the six buckets. Empty means "crewlet", matching
	// the lease store — two companies on one NATS account are separated by
	// giving them different prefixes.
	BucketPrefix string

	// RateWindow is the valve's window width. The bucket keeps a few
	// multiples of it so a window that has just closed is still readable
	// while a straggler finishes writing to it.
	RateWindow time.Duration

	// ClaimTTL is how long a webhook delivery stays claimed.
	ClaimTTL time.Duration

	// LedgerRetention is how long a turn completion is remembered.
	LedgerRetention time.Duration

	// CooldownMax is the longest credential cooldown, and therefore the
	// bucket's age: a cooldown is stored as its own end instant, so the
	// bucket only has to outlive the longest one anybody sets.
	CooldownMax time.Duration

	// StatusFreshness is how long a node's apply status counts as current.
	StatusFreshness time.Duration

	// Replicas is the JetStream replica count for all six.
	Replicas int
}

// rateBucketFactor is how many windows the rate bucket keeps.
//
// Three: the CURRENT window, the one that just closed (a straggler may still
// be incrementing it), and one of slack so a broker under load does not reap
// a window a caller is about to read. Higher costs nothing but stream size
// on keys nobody reads; lower risks reaping a live window, which resets a
// seat's count mid-window and lets it emit its whole allowance again.
const rateBucketFactor = 3

func (c *FleetConfig) normalize() error {
	if c.BucketPrefix == "" {
		c.BucketPrefix = defaultBucketPrefix
	}
	if c.Replicas == 0 {
		c.Replicas = 1
	}
	required := []struct {
		name  string
		value time.Duration
	}{
		{"RateWindow", c.RateWindow}, {"ClaimTTL", c.ClaimTTL},
		{"LedgerRetention", c.LedgerRetention}, {"CooldownMax", c.CooldownMax},
		{"StatusFreshness", c.StatusFreshness},
	}
	for _, field := range required {
		switch {
		case field.value <= 0:
			return fmt.Errorf("coord/kv: FleetConfig.%s is required", field.name)
		case field.value < minBucketTTL:
			return fmt.Errorf("coord/kv: FleetConfig.%s %v is below the broker's %v floor "+
				"on a bucket TTL", field.name, field.value, minBucketTTL)
		}
	}
	if c.Replicas < 0 || c.Replicas > maxReplicas {
		return fmt.Errorf("coord/kv: FleetConfig.Replicas %d is outside 1..%d", c.Replicas, maxReplicas)
	}
	if !validBucketName.MatchString(c.BucketPrefix + rateSuffix) {
		return fmt.Errorf("coord/kv: FleetConfig.BucketPrefix %q is not a valid bucket name "+
			"(letters, digits, '-' and '_' only)", c.BucketPrefix)
	}
	return nil
}

// FleetStore is the JetStream KV [coord.Fleet].
type FleetStore struct {
	rate      jetstream.KeyValue
	claims    jetstream.KeyValue
	ledger    jetstream.KeyValue
	cooldowns jetstream.KeyValue
	status    jetstream.KeyValue
	config    jetstream.KeyValue

	rateWindow time.Duration
	freshness  time.Duration
}

var _ coord.Fleet = (*FleetStore)(nil)

// OpenFleet creates or adopts the six buckets and returns the backend.
//
// Idempotent and safe to call from every node at once, like [Open]: creating
// a bucket that already exists with the same shape is a no-op, and a changed
// retention is applied as a stream update.
func OpenFleet(ctx context.Context, nc *nats.Conn, cfg FleetConfig) (*FleetStore, error) {
	if nc == nil {
		return nil, errors.New("coord/kv: a NATS connection is required")
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: jetstream context: %w", err)
	}

	open := func(suffix, describe string, ttl time.Duration) (jetstream.KeyValue, error) {
		name := cfg.BucketPrefix + suffix
		bucket, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket: name, Description: describe, TTL: ttl, Replicas: cfg.Replicas,
		})
		if err != nil {
			return nil, fmt.Errorf("coord/kv: open %s: %w", name, err)
		}
		return bucket, nil
	}

	store := &FleetStore{rateWindow: cfg.RateWindow, freshness: cfg.StatusFreshness}
	for _, bucket := range []struct {
		into     *jetstream.KeyValue
		suffix   string
		describe string
		ttl      time.Duration
	}{
		{&store.rate, rateSuffix,
			"Crewlet notification-valve windows; the bucket TTL reaps a closed window",
			cfg.RateWindow * rateBucketFactor},
		{&store.claims, claimsSuffix,
			"Crewlet inbound-delivery claims; the bucket TTL is the dedupe window",
			cfg.ClaimTTL},
		{&store.ledger, ledgerSuffix,
			"Crewlet turn completions; the bucket TTL is the retention horizon",
			cfg.LedgerRetention},
		{&store.cooldowns, cooldownSuffix,
			"Crewlet credential cooldowns; each value carries its own end instant",
			cfg.CooldownMax},
		{&store.status, statusSuffix,
			"Crewlet per-node config-apply status; a node that stops reporting ages out",
			cfg.StatusFreshness},
		{&store.config, configSuffix,
			"Crewlet activation pointer; NO TTL — its revision IS the epoch", 0},
	} {
		got, err := open(bucket.suffix, bucket.describe, bucket.ttl)
		if err != nil {
			return nil, err
		}
		*bucket.into = got
	}

	log.Debug("coord_kv_fleet_open", "prefix", cfg.BucketPrefix,
		"rate_window", cfg.RateWindow, "claim_ttl", cfg.ClaimTTL,
		"ledger_retention", cfg.LedgerRetention, "cooldown_max", cfg.CooldownMax,
		"status_freshness", cfg.StatusFreshness)
	return store, nil
}

// ---- the rate valve ---------------------------------------------------- //

// rateRecord is one window's count.
type rateRecord struct {
	Count int `json:"count"`
}

// Allow increments a bucket's window and reports whether it stayed in limit.
//
// The WINDOW IS IN THE KEY, so a new window is a new record and the previous
// one simply ages out — there is nothing to reset and no sweep to run. The
// increment is a compare-and-swap on that key, which is what makes four nodes
// incrementing at once add up to four rather than one.
func (f *FleetStore) Allow(ctx context.Context, bucket string, limit int, window time.Duration, now time.Time) (bool, error) {
	if bucket == "" {
		return false, errors.New("coord/kv: a rate bucket needs a name")
	}
	if limit <= 0 || window <= 0 {
		return false, nil
	}
	if window > f.rateWindow*rateBucketFactor {
		// The bucket would reap the window before it closed, so the count
		// would restart mid-window and the valve would pass far more than
		// the operator asked for. Refused loudly rather than silently.
		return false, fmt.Errorf(
			"coord/kv: rate window %v exceeds what the bucket retains (%v) — "+
				"raise FleetConfig.RateWindow", window, f.rateWindow*rateBucketFactor)
	}
	key := encodeKey(bucket + "|" + strconv.FormatInt(now.Truncate(window).UnixNano(), 10))

	for range fleetCASRetries {
		entry, err := f.rate.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			// Create, not Put: the first caller in a window must be
			// distinguishable from a caller racing it, or two nodes both
			// write "1" and the window counts one.
			_, created := f.rate.Create(ctx, key, mustEncodeRate(1))
			switch {
			case created == nil:
				return true, nil
			case errors.Is(created, jetstream.ErrKeyExists):
				continue
			default:
				return false, unavailable("increment the rate window", created)
			}
		}
		if err != nil {
			return false, unavailable("read the rate window", err)
		}
		var record rateRecord
		if decode := json.Unmarshal(entry.Value(), &record); decode != nil {
			return false, unavailable("decode the rate window", decode)
		}
		if record.Count >= limit {
			return false, nil
		}
		_, err = f.rate.Update(ctx, key, mustEncodeRate(record.Count+1), entry.Revision())
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			continue
		default:
			return false, unavailable("increment the rate window", err)
		}
	}
	// Exhausting the retries means the bucket is HOT — every round had a
	// winner and it was somebody else. Reported as an error rather than
	// "allowed": the valve's whole purpose is a bucket under exactly this
	// much pressure, and answering true would open it at the worst moment.
	return false, contended("increment", bucket)
}

func mustEncodeRate(count int) []byte {
	// A two-field object with an int cannot fail to encode.
	raw, _ := json.Marshal(rateRecord{Count: count})
	return raw
}

// ---- the delivery claims ----------------------------------------------- //

// Claim records a key and reports whether this caller was first.
func (f *FleetStore) Claim(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error) {
	if key == "" {
		return false, errors.New("coord/kv: a claim needs a key")
	}
	if ttl <= 0 {
		return false, errors.New("coord/kv: a claim needs a positive ttl")
	}
	encoded := encodeKey(key)
	// Create is the whole mechanism: it fails when the key exists, so the
	// FIRST caller wins and every other gets ErrKeyExists. Expiry is the
	// bucket's, which means the server decides when a claim lapses and no
	// node compares its own clock to a peer's deadline.
	if _, err := f.claims.Create(ctx, encoded, []byte(now.UTC().Format(time.RFC3339Nano))); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return false, nil
		}
		return false, unavailable("claim the delivery", err)
	}
	return true, nil
}

// Release drops a claim.
func (f *FleetStore) Release(ctx context.Context, key string) error {
	if key == "" {
		return nil
	}
	// Purge, not Delete: Delete leaves a tombstone that Create still
	// refuses, so a released claim could never be re-claimed and a
	// deliberate replay would be swallowed for the bucket's whole age.
	if err := f.claims.Purge(ctx, encodeKey(key)); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("release the delivery claim", err)
	}
	return nil
}

// ---- the completion ledger --------------------------------------------- //

// ledgerRecord is one completed unit of work.
type ledgerRecord struct {
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// Worked returns the subset of keys already recorded under scope.
func (f *FleetStore) Worked(ctx context.Context, scope string, keys []string) (map[string]bool, error) {
	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		_, err := f.ledger.Get(ctx, ledgerKey(scope, key))
		switch {
		case err == nil:
			out[key] = true
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// Not worked. The ordinary answer.
		default:
			// RAISED, even though the contract says the caller fails
			// open: the decision to run the work anyway belongs to the
			// caller, and swallowing it here would take away the log
			// line that says why a turn ran twice.
			return nil, unavailable("read the completion ledger", err)
		}
	}
	return out, nil
}

// Record marks one key worked.
func (f *FleetStore) Record(ctx context.Context, scope, key, detail string, at time.Time) error {
	if scope == "" || key == "" {
		return errors.New("coord/kv: a ledger entry needs a scope and a key")
	}
	raw, err := json.Marshal(ledgerRecord{Detail: detail, At: at.UTC()})
	if err != nil {
		return fmt.Errorf("coord/kv: encode the ledger entry: %w", err)
	}
	// FIRST WRITER WINS, and losing is not a failure: two nodes completing
	// one trigger is the case the ledger exists to collapse.
	if _, err := f.ledger.Create(ctx, ledgerKey(scope, key), raw); err != nil &&
		!errors.Is(err, jetstream.ErrKeyExists) {
		return unavailable("record the completion", err)
	}
	return nil
}

func ledgerKey(scope, key string) string { return encodeKey(scope + "|" + key) }

// ---- the credential cooldowns ------------------------------------------ //

// Cool records a credential as unusable until an instant.
func (f *FleetStore) Cool(ctx context.Context, key string, until time.Time) error {
	if key == "" {
		return errors.New("coord/kv: a cooldown needs a key")
	}
	encoded := encodeKey(key)
	value := []byte(until.UTC().Format(time.RFC3339Nano))

	for range fleetCASRetries {
		entry, err := f.cooldowns.Get(ctx, encoded)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			_, created := f.cooldowns.Create(ctx, encoded, value)
			switch {
			case created == nil:
				return nil
			case errors.Is(created, jetstream.ErrKeyExists):
				continue
			default:
				return unavailable("record the cooldown", created)
			}
		}
		if err != nil {
			return unavailable("read the cooldown", err)
		}
		// The LONGER of the two survives: both nodes saw a real refusal,
		// and shortening a peer's cooldown sends this node straight back
		// at a credential the peer already knows is spent.
		if existing, parsed := time.Parse(time.RFC3339Nano, string(entry.Value())); parsed == nil &&
			existing.After(until) {
			return nil
		}
		_, err = f.cooldowns.Update(ctx, encoded, value, entry.Revision())
		switch {
		case err == nil:
			return nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			continue
		default:
			return unavailable("record the cooldown", err)
		}
	}
	return contended("cool", key)
}

// Since returns every cooldown that has not yet lapsed.
func (f *FleetStore) Since(ctx context.Context, now time.Time) (map[string]time.Time, error) {
	keys, err := f.cooldowns.ListKeys(ctx)
	if err != nil {
		return nil, unavailable("list the cooldowns", err)
	}
	out := map[string]time.Time{}
	for key := range keys.Keys() {
		entry, err := f.cooldowns.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, unavailable("read a cooldown", err)
		}
		until, err := time.Parse(time.RFC3339Nano, string(entry.Value()))
		if err != nil || !until.After(now) {
			continue
		}
		decoded, ok := decodeKey(key)
		if !ok {
			continue
		}
		out[decoded] = until
	}
	return out, nil
}

// ---- the config plane -------------------------------------------------- //

// activationRecord is the pointer's stored form. The EPOCH IS NOT IN IT: the
// key's revision is the epoch, so storing one too would give two answers that
// could disagree.
type activationRecord struct {
	RevisionID string    `json:"revision_id"`
	At         time.Time `json:"at"`
	Summary    string    `json:"summary,omitempty"`
}

// Activate publishes a new target revision.
func (f *FleetStore) Activate(ctx context.Context, revisionID, summary string, at time.Time) (coord.Activation, error) {
	if revisionID == "" {
		return coord.Activation{}, errors.New("coord/kv: an activation needs a revision id")
	}
	raw, err := json.Marshal(activationRecord{RevisionID: revisionID, At: at.UTC(), Summary: summary})
	if err != nil {
		return coord.Activation{}, fmt.Errorf("coord/kv: encode the activation: %w", err)
	}
	// ONE WRITE. Put returns the revision it assigned, and that revision
	// IS the epoch — so the append and the flip cannot come apart, and two
	// nodes activating at the same instant are handed two epochs by the
	// store rather than racing over a counter this engine keeps.
	revision, err := f.config.Put(ctx, activationKey, raw)
	if err != nil {
		return coord.Activation{}, unavailable("publish the activation", err)
	}
	return coord.Activation{
		Epoch:      int64(revision),
		RevisionID: revisionID,
		At:         at.UTC(),
		Summary:    summary,
	}, nil
}

// Target reads the pointer.
func (f *FleetStore) Target(ctx context.Context) (coord.Activation, bool, error) {
	entry, err := f.config.Get(ctx, activationKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return coord.Activation{}, false, nil
	}
	if err != nil {
		return coord.Activation{}, false, unavailable("read the activation pointer", err)
	}
	var record activationRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return coord.Activation{}, false, unavailable("decode the activation pointer", err)
	}
	return coord.Activation{
		Epoch:      int64(entry.Revision()),
		RevisionID: record.RevisionID,
		At:         record.At,
		Summary:    record.Summary,
	}, true, nil
}

// applyRecord is one node's status. The node id is the KEY, so a node cannot
// report on behalf of another by writing a different field.
type applyRecord struct {
	Epoch      int64     `json:"epoch"`
	RevisionID string    `json:"revision_id,omitempty"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// RecordApply publishes this node's status for an epoch.
func (f *FleetStore) RecordApply(ctx context.Context, status coord.NodeApply) error {
	if status.NodeID == "" {
		return errors.New("coord/kv: an apply status needs a node id")
	}
	detail := coord.TruncateApplyError(status.Error)
	raw, err := json.Marshal(applyRecord{
		Epoch: status.Epoch, RevisionID: status.RevisionID, Status: status.Status,
		Error: detail, UpdatedAt: status.UpdatedAt.UTC(),
	})
	if err != nil {
		return fmt.Errorf("coord/kv: encode the apply status: %w", err)
	}
	// Put, unconditionally: a node's own status is its to overwrite, and
	// every write refreshes the key's age — which is exactly what makes a
	// node that STOPS reporting age out of the fleet view.
	if _, err := f.status.Put(ctx, encodeKey(status.NodeID), raw); err != nil {
		return unavailable("publish the apply status", err)
	}
	return nil
}

// Fleet returns every node's last status, freshest first.
func (f *FleetStore) Fleet(ctx context.Context) ([]coord.NodeApply, error) {
	keys, err := f.status.ListKeys(ctx)
	if err != nil {
		return nil, unavailable("list the fleet status", err)
	}
	var out []coord.NodeApply
	for key := range keys.Keys() {
		entry, err := f.status.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, unavailable("read a node's apply status", err)
		}
		var record applyRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			continue
		}
		node, ok := decodeKey(key)
		if !ok {
			continue
		}
		out = append(out, coord.NodeApply{
			NodeID: node, Epoch: record.Epoch, RevisionID: record.RevisionID,
			Status: record.Status, Error: record.Error, UpdatedAt: record.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out, nil
}
