package kv

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// A BUCKET IS A STREAM, and provisioning a replicated one has the two hazards
// [internal/queue/jetstream] documents for the stream side. This is the same
// fix, because it is the same call underneath.
//
// # The deadline
//
// nats.go applies a FIVE-SECOND default API timeout to a context with no
// deadline of its own. That is right for an ordinary request and wrong for the
// one that creates a replicated stream: the caller has already waited for the
// metadata group precisely because that group is slow, and then gives the call
// depending on it five seconds. On a fleet booting together the symptom is a
// node that fails with `open crewlet_rate: context deadline exceeded` — a
// message that names neither the deadline that expired nor the cluster it was
// waiting for.
//
// # The placement retry
//
// `No suitable peers` means the metadata leader has not yet seen enough members
// to place a replicated bucket. It is transient and self-clearing during
// formation, and it is the one condition worth waiting out — every other error
// (a bad TTL, a conflicting replica count, an auth failure) clears by nobody
// waiting, so retrying it would turn a config mistake into a two-minute hang
// with the same message at the end.
const (
	// positionsSuffix is the per-node state-log position bucket.
	positionsSuffix = "_statelog_positions"
	// bucketProvisionTimeout bounds one bucket create. The same size as
	// the stream side's, and for the same reason: a replicated create is
	// a raft round trip plus file-store setup, fast on a quiet cluster and
	// seconds under load, so thirty is well past any healthy case and
	// still fails a genuinely wedged one rather than hanging a boot.
	bucketProvisionTimeout = 30 * time.Second

	// bucketPlacementRetry is how often a forming cluster is re-asked.
	bucketPlacementRetry = 250 * time.Millisecond

	// jsErrCodeNoPeers is JetStream's "no suitable peers for placement".
	// A NUMBER rather than a string match, so it survives the server
	// rewording the message.
	jsErrCodeNoPeers jetstream.ErrorCode = 10005
)

// openBucket creates one bucket when it is absent and OBSERVES it when it is
// present, waiting out a cluster that is still forming.
//
// # Why not CreateOrUpdate
//
// Every node of a fleet opens every bucket at boot, so on a fleet starting
// together N nodes issue the same call for the same bucket at the same moment.
// `CreateOrUpdate` makes each of those a WRITE — the update half runs whenever
// the bucket exists — so the losers of the create race go on to rewrite a
// configuration they agree with, against a metadata group that is still
// electing. Measured on three members: the request never returns, and the boot
// fails after its whole deadline with `context deadline exceeded` naming a
// bucket rather than a cluster.
//
// Create-else-observe is the shape [internal/queue/jetstream] already uses for
// a stream, and the reasoning transfers whole: the create races, the loser
// gets "bucket exists", and that is not an error but the other node having
// won. A booting peer then READS what the winner made instead of writing over
// it — which is also the honest ownership rule, since a bucket's TTL is a
// deployment-wide fact and not something each node should re-assert.
func openBucket(ctx context.Context, js jetstream.JetStream,
	cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {

	// WithTimeout only ever shortens against the parent, so a caller that
	// already set a tighter deadline keeps it.
	ctx, cancel := context.WithTimeout(ctx, bucketProvisionTimeout)
	defer cancel()

	for {
		bucket, err := createOrObserveBucket(ctx, js, cfg)
		if err == nil || !unplaceableBucket(err) {
			return bucket, err
		}
		select {
		case <-ctx.Done():
			// THE ORIGINAL ERROR, not the context's: "no suitable
			// peers" says what is wrong and "deadline exceeded"
			// does not.
			return nil, err
		case <-time.After(bucketPlacementRetry):
		}
	}
}

// createOrObserveBucket is one attempt at that.
func createOrObserveBucket(ctx context.Context, js jetstream.JetStream,
	cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {

	switch bucket, err := js.KeyValue(ctx, cfg.Bucket); {
	case err == nil:
		return bucket, nil
	case !errors.Is(err, jetstream.ErrBucketNotFound):
		return nil, err
	}
	bucket, createErr := js.CreateKeyValue(ctx, cfg)
	if createErr == nil {
		return bucket, nil
	}
	if unplaceableBucket(createErr) {
		// STILL FORMING, which is the caller's loop to wait out rather
		// than a race to read back.
		return nil, createErr
	}
	// A PEER MAY HAVE WON THE RACE between the read above and this
	// create, and it says so in two shapes.
	//
	// The tidy one is [jetstream.ErrBucketExists]. The other is a
	// TIMEOUT, and it is the one a fleet booting together produces: two
	// members create the same bucket in the same instant, the server
	// commits one and holds the other while the metadata group settles,
	// and the held request outlives the deadline. The bucket is there —
	// the loser never heard so, and reporting that as a failure is a node
	// refusing to boot because a peer beat it.
	//
	// So the question is re-asked rather than assumed. The read gets its
	// OWN context, because the one above may be the deadline that just
	// expired.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bucketReadBack)
	defer cancel()
	bucket, err := js.KeyValue(readCtx, cfg.Bucket)
	if err != nil {
		// THE CREATE'S ERROR IS WHAT IS REPORTED, with the read-back's
		// beside it: the first says what went wrong and the second only
		// confirms the create really did fail.
		return nil, fmt.Errorf("%w (and it is not there: %w)", createErr, err)
	}
	return bucket, nil
}

// bucketReadBack bounds the one read that asks whether a peer won the race.
// Short for [streamReadBack]'s reason: an ordinary metadata read against a
// group that has just proven it works.
const bucketReadBack = 5 * time.Second

// unplaceableBucket reports the transient "the cluster is still forming" error.
func unplaceableBucket(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jsErrCodeNoPeers
}

// The fleet-shared state on JetStream KV.
//
// # Why THIRTEEN buckets and not one
//
// The package doc records the constraint this whole file is shaped by: a
// bucket's TTL is its stream's MaxAge, and jetstream.KeyTTL is create-only —
// an Update CLEARS it, leaving the key immortal. So retention is a property
// of the BUCKET, and each of these has a genuinely different one:
//
//	rate       one window's width x a small factor — a window nobody writes
//	           to again must age out, and nothing may outlive its successor
//	claims     the dedupe window, minutes: long enough to cover a
//	           third-party app's retries and an operator's replay, short
//	           enough that a deliberate re-send later is not swallowed
//	ledger     turn-completion retention, days: it has to outlast the
//	           redelivery horizon and the scheduler's catchup floor
//	cooldowns  the longest credential cooldown, an hour
//	status     peer freshness, a minute: a node that STOPS reporting must
//	           vanish from the fleet view, which the bucket does for free
//	config     none at all: the activation pointer is the fencing sequence,
//	           and a pointer that expired would restart the epoch
//	budgets    none at all either, for the opposite reason: a token cap is
//	           a ceiling for the life of a deployment, and a counter that
//	           rolled over would re-arm a company somebody had stopped
//	channels   none at all, for a third reason: a bucket age cannot tell an
//	           OPEN channel from a closed one, so it would reap the
//	           authorization record of an ask still waiting for its answer
//	fires      the scheduler's catchup ceiling, days: a claim that expired
//	           inside the window a tick can still evaluate lets that fire
//	           run a second time
//	runs       none at all, a sharper version of the channels case: a run
//	           parked on a person's answer waits DAYS, and its record is
//	           the only thing that knows a billed box exists
//	secrets    none at all, and this is the one where an age would be
//	           actively dangerous: a credential is not short-horizon state,
//	           and a bucket that expired one would de-authenticate a
//	           company on a timer nobody set
//	integrations
//	           none at all, for the budget counter's reason: an
//	           integration's reconcile status is standing state, and one
//	           that expired would make a converged surface read as one
//	           nobody has looked at, sending the loop to re-provision
//	           against a third-party app it had already agreed with
//	positions  none at all, and this is the one where an age would be
//	           worst: a node's position is what the trim reads to decide
//	           what every other node may delete, and a key that expired
//	           would read as a node that has applied NOTHING — which
//	           either pins the trim for ever or, read the other way,
//	           lets it delete records that node still needs
//
// Putting two of those in one bucket would give one of them the other's
// retention, and every such mistake is silent — a cooldown that expired in a
// second, a fleet view showing a node that died last week.
//
// # The activation epoch IS the pointer's revision
//
// JetStream assigns each key write a monotonic revision. Publishing the
// pointer therefore appends and flips in ONE write, which is the atomicity
// the contract asks for: there is no instant where a node can read an epoch whose
// revision has not been published, and two nodes activating at once get two
// different revisions rather than racing over a counter this engine keeps.

const (
	rateSuffix         = "_rate"
	claimsSuffix       = "_claims"
	ledgerSuffix       = "_ledger"
	cooldownSuffix     = "_cooldowns"
	statusSuffix       = "_status"
	configSuffix       = "_config"
	budgetSuffix       = "_budgets"
	channelSuffix      = "_channels"
	firesSuffix        = "_fires"
	runsSuffix         = "_sandbox_runs"
	secretsSuffix      = "_secrets"
	integrationsSuffix = "_integrations"
	activationKey      = "activation"
	// payloadKey holds the CURRENT revision's sealed body, in the same
	// bucket as the pointer and for the same reason: neither may expire,
	// and a payload in a bucket the pointer is not in could age out from
	// under the epoch it belongs to.
	payloadKey      = "revision_payload"
	fleetCASRetries = 16
)

// FleetConfig is what a [FleetStore] needs at construction. Every duration is
// a BUCKET's retention; see the file doc for why each is its own bucket.
type FleetConfig struct {
	// BucketPrefix names every bucket. Empty means "crewlet", matching
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

	// FireRetention is how long a scheduled fire stays claimed. Sized from
	// the same fact as LedgerRetention — the scheduler's catchup ceiling —
	// and kept a separate knob because they are not one number.
	FireRetention time.Duration

	// CooldownMax is the longest credential cooldown, and therefore the
	// bucket's age: a cooldown is stored as its own end instant, so the
	// bucket only has to outlive the longest one anybody sets.
	CooldownMax time.Duration

	// StatusFreshness is how long a node's apply status counts as current.
	StatusFreshness time.Duration

	// Replicas is the JetStream replica count for every bucket.
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
		{"LedgerRetention", c.LedgerRetention}, {"FireRetention", c.FireRetention},
		{"CooldownMax", c.CooldownMax},
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
	rate         jetstream.KeyValue
	claims       jetstream.KeyValue
	ledger       jetstream.KeyValue
	cooldowns    jetstream.KeyValue
	status       jetstream.KeyValue
	config       jetstream.KeyValue
	budgets      jetstream.KeyValue
	channels     jetstream.KeyValue
	secrets      jetstream.KeyValue
	fires        jetstream.KeyValue
	runs         jetstream.KeyValue
	integrations jetstream.KeyValue

	// positions is the register every ageless key class the fleet still
	// composes shares: a node's log positions, a trim hold, a backup point,
	// a domain's published floor, a capacity operation, a node's admission
	// and its maintenance acknowledgement. They are together because none
	// of them may ever expire — each is read to decide what somebody ELSE
	// may delete or publish — and every listing over the bucket filters by
	// class, which is the load-bearing half of sharing it. The document
	// families that once had buckets beside this one left with the last
	// projector; only their key grammar outlived them, in coord/keys.go.
	positions jetstream.KeyValue

	// js is the JetStream context, held so a feed can create the durable
	// consumer a bucket's own KeyValue handle cannot: a watch is
	// ephemeral by construction, and a feed's position has to be the
	// FLEET's rather than this process's.
	js jetstream.JetStream

	// bucketPrefix names the buckets, so a feed can address the stream
	// behind one by its conventional name.
	bucketPrefix string

	rateWindow time.Duration
	freshness  time.Duration
}

var _ coord.Fleet = (*FleetStore)(nil)

// OpenFleet creates or adopts every bucket and returns the backend.
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
		bucket, err := openBucket(ctx, js, jetstream.KeyValueConfig{
			Bucket: name, Description: describe, TTL: ttl, Replicas: cfg.Replicas,
		})
		if err != nil {
			return nil, fmt.Errorf("coord/kv: open %s: %w", name, err)
		}
		return bucket, nil
	}

	store := &FleetStore{
		js: js, bucketPrefix: cfg.BucketPrefix,
		rateWindow: cfg.RateWindow, freshness: cfg.StatusFreshness,
	}
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
		{&store.budgets, budgetSuffix,
			"Crewlet token counters; NO TTL — a cap is a ceiling for the deployment's life", 0},
		{&store.channels, channelSuffix,
			"Crewlet agent-to-agent channels; NO TTL — an open ask must outlive any clock", 0},
		{&store.fires, firesSuffix,
			"Crewlet scheduled-fire claims; the bucket TTL outlasts the catchup ceiling",
			cfg.FireRetention},
		{&store.runs, runsSuffix,
			"Crewlet detached sandbox runs; NO TTL — a parked run's box outlives any clock", 0},
		{&store.secrets, secretsSuffix,
			"Crewlet sealed credentials; NO TTL — an expiring secret is an outage on a timer", 0},
		{&store.integrations, integrationsSuffix,
			"Crewlet integration reconcile status; NO TTL, standing state rather than a short horizon", 0},
		{&store.positions, positionsSuffix,
			"Crewlet per-node state-log positions; NO TTL — an expired position reads as a node that applied nothing", 0},
	} {
		got, err := open(bucket.suffix, bucket.describe, bucket.ttl)
		if err != nil {
			return nil, err
		}
		*bucket.into = got
	}

	log.DebugContext(ctx, "coord_kv_fleet_open", "prefix", cfg.BucketPrefix,
		"rate_window", cfg.RateWindow, "claim_ttl", cfg.ClaimTTL,
		"ledger_retention", cfg.LedgerRetention, "cooldown_max", cfg.CooldownMax,
		"status_freshness", cfg.StatusFreshness)
	return store, nil
}

// each walks a whole bucket — see [eachEntry], which is the one implementation
// and which both backends reach through a method of their own only so that a
// call site reads as a walk rather than as connection plumbing.
func (f *FleetStore) each(ctx context.Context, kv jetstream.KeyValue,
	visit func(jetstream.KeyValueEntry) error) error {

	return eachEntry(ctx, kv, visit)
}

// eachUnder is [each] narrowed to the keys matching one filter, with `what`
// naming the listing a failure could not complete — see [eachEntryUnder],
// which is where both are explained.
func (f *FleetStore) eachUnder(ctx context.Context, kv jetstream.KeyValue, keys, what string,
	visit func(jetstream.KeyValueEntry) error) error {

	return eachEntryUnder(ctx, kv, keys, what, visit)
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
	out := map[string]time.Time{}
	err := f.each(ctx, f.cooldowns, func(kve jetstream.KeyValueEntry) error {
		until, err := time.Parse(time.RFC3339Nano, string(kve.Value()))
		//nolint:nilerr // An unreadable cooldown row is SKIPPED, not raised:
		// this listing answers "which credentials are benched", and one
		// undecodable row must not bench the whole pool by failing the read.
		// The conservative direction here is to treat the row as absent —
		// a key that is not benched is simply tried, and a real failure
		// benches it again.
		if err != nil || !until.After(now) {
			return nil
		}
		decoded, ok := decodeKey(kve.Key())
		if !ok {
			return nil
		}
		out[decoded] = until
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- the token counters ------------------------------------------------ //

// budgetRecord is one scope's spend.
//
// RefusedAt is omitted when zero, so a record no refusal has touched encodes
// exactly as it did before the field existed, and a build that predates it
// reads a stamped record by ignoring the key.
type budgetRecord struct {
	Used      int       `json:"used"`
	At        time.Time `json:"at"`
	RefusedAt time.Time `json:"refused_at,omitzero"`
}

// Charge checks and increments the org's counter and the seat's.
//
// Two keys and no transaction, so the all-or-nothing property is BUILT: the
// org is charged first and compensated if the seat then refuses. See
// [coord.Budgets.Charge] for why that order and not the reverse.
func (f *FleetStore) Charge(ctx context.Context, agentScope string, tokens, orgLimit, agentLimit int) (coord.Spend, error) {
	if tokens <= 0 {
		// Not an error and not a charge. A phase whose provider reported
		// nothing still ran, and refusing it would stop a company over a
		// backend that omits usage.
		return coord.Spend{OK: true}, nil
	}
	if agentScope == "" {
		return coord.Spend{}, errors.New("coord/kv: a charge needs a seat scope")
	}

	// A charge larger than a whole cap can never fit, so it is screened
	// before anything is written, and a seat whose own cap is smaller than
	// the charge never costs the org a bump and an unwind.
	if orgLimit > 0 && tokens > orgLimit {
		used, err := f.Used(ctx, coord.OrgScope)
		if err != nil {
			return coord.Spend{}, err
		}
		return f.refuse(ctx, coord.OrgScope, "org", used, orgLimit), nil
	}
	if agentLimit > 0 && tokens > agentLimit {
		// ORG FIRST even here, which is why the screen reads the org's
		// counter before naming the seat. Testing each cap alone reported
		// the seat for a charge the company had no room for either, and
		// the contract's ordering rule exists for exactly that case: an
		// operator who raised this seat's ceiling would still be refused.
		if orgLimit > 0 {
			orgUsed, err := f.Used(ctx, coord.OrgScope)
			if err != nil {
				return coord.Spend{}, err
			}
			if orgUsed+tokens > orgLimit {
				return f.refuse(ctx, coord.OrgScope, "org", orgUsed, orgLimit), nil
			}
		}
		used, err := f.Used(ctx, agentScope)
		if err != nil {
			return coord.Spend{}, err
		}
		return f.refuse(ctx, agentScope, "agent", used, agentLimit), nil
	}

	org, fits, err := f.bump(ctx, coord.OrgScope, tokens, orgLimit)
	if err != nil {
		return coord.Spend{}, err
	}
	if !fits {
		return f.refuse(ctx, coord.OrgScope, "org", org.Used, orgLimit), nil
	}

	agent, fits, err := f.bump(ctx, agentScope, tokens, agentLimit)
	switch {
	case err != nil, !fits:
		// COMPENSATE, which is what a single SQL transaction used to do
		// for free: charging the company for a turn that never ran lets
		// it exhaust its budget on work it did not do.
		if _, _, undo := f.bump(ctx, coord.OrgScope, -tokens, 0); undo != nil {
			// Logged rather than returned: the caller's answer is
			// already decided, and a compensation that failed leaves
			// the org over-stated, which trips the cap EARLY. That is
			// the safe direction, and it is worth a line saying so
			// rather than a drift nobody can later explain.
			log.ErrorContext(ctx, "coord_kv_budget_compensation_failed", "scope", coord.OrgScope,
				"tokens", tokens, "error", undo,
				"detail", "the org counter is over-stated by this charge and will refuse "+
					"early; clear it with `crewlet budgets reset`")
		}
		if err != nil {
			return coord.Spend{}, err
		}
		return f.refuse(ctx, agentScope, "agent", agent.Used, agentLimit), nil
	}
	// ADMITTED, so each scope that carried a refusal has just had room for
	// a charge. The counter writes above deliberately kept the stamp: the
	// org is written before the seat is tested, and clearing it there would
	// let a charge that was refused overall erase the company's refusal.
	for _, scope := range []struct {
		key  string
		seen time.Time
	}{{coord.OrgScope, org.RefusedAt}, {agentScope, agent.RefusedAt}} {
		if !scope.seen.IsZero() {
			f.clearRefusal(ctx, scope.key, scope.seen)
		}
	}
	return coord.Spend{OK: true, OrgUsed: org.Used, AgentUsed: agent.Used}, nil
}

// refuse stamps a refusal on the scope that made it and answers with it.
//
// A stamp that cannot be written is LOGGED and the refusal still stands. The
// decision was taken from a counter this call read, so it is true whether or
// not the stamp lands; turning it into an error would report an outage for a
// company that is simply out of budget, and those send an operator to
// different places. What the failure costs is one dashboard not saying
// "refusing charges" until the next refusal writes.
func (f *FleetStore) refuse(ctx context.Context, scope, name string, used, limit int) coord.Spend {
	if err := f.stampRefusal(ctx, scope); err != nil {
		log.WarnContext(ctx, "coord_kv_budget_refusal_not_recorded", "scope", scope, "error", err,
			"detail", "the charge was still refused; the live meter will not show "+
				"this refusal until the scope refuses another charge")
	}
	return coord.Spend{RefusedScope: name, RefusedUsed: used, RefusedLimit: limit}
}

// stampRefusal records now as the scope's last refusal, under a
// compare-and-swap that leaves its spend untouched.
//
// A scope with no record yet gets one at zero spend: a seat refused on its
// first charge has refused a charge, and that is worth listing.
func (f *FleetStore) stampRefusal(ctx context.Context, scope string) error {
	key := encodeKey(scope)
	for range fleetCASRetries {
		now := time.Now().UTC()
		entry, err := f.budgets.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			raw, encoded := encodeBudget(budgetRecord{RefusedAt: now})
			if encoded != nil {
				return encoded
			}
			_, created := f.budgets.Create(ctx, key, raw)
			switch {
			case created == nil:
				return nil
			case errors.Is(created, jetstream.ErrKeyExists):
				continue
			default:
				return unavailable("record the budget refusal", created)
			}
		}
		if err != nil {
			return unavailable("read the budget", err)
		}
		var record budgetRecord
		if decode := json.Unmarshal(entry.Value(), &record); decode != nil {
			return unavailable("decode the budget", decode)
		}
		record.RefusedAt = now
		raw, encoded := encodeBudget(record)
		if encoded != nil {
			return encoded
		}
		_, err = f.budgets.Update(ctx, key, raw, entry.Revision())
		switch {
		case err == nil:
			return nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			continue
		default:
			return unavailable("record the budget refusal", err)
		}
	}
	return contended("refuse", scope)
}

// clearRefusal drops the refusal an admitted charge found on the scope.
//
// ONLY THE STAMP IT SAW. Between the charge's write and this one another
// caller may have been refused and stamped a newer instant, and that refusal
// is still true; clearing it would hide a scope that is refusing right now.
// A failure is logged for the reason [FleetStore.refuse] gives: the charge
// already happened, and the stamp is what a dashboard reads, not what the gate
// decides with.
func (f *FleetStore) clearRefusal(ctx context.Context, scope string, seen time.Time) {
	key := encodeKey(scope)
	for range fleetCASRetries {
		entry, err := f.budgets.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			// Reset by an operator in between, which cleared it.
			return
		}
		if err != nil {
			f.logUncleared(ctx, scope, unavailable("read the budget", err))
			return
		}
		var record budgetRecord
		if decode := json.Unmarshal(entry.Value(), &record); decode != nil {
			f.logUncleared(ctx, scope, unavailable("decode the budget", decode))
			return
		}
		if !record.RefusedAt.Equal(seen) {
			// Already cleared, or stamped again since.
			return
		}
		record.RefusedAt = time.Time{}
		raw, encoded := encodeBudget(record)
		if encoded != nil {
			f.logUncleared(ctx, scope, encoded)
			return
		}
		_, err = f.budgets.Update(ctx, key, raw, entry.Revision())
		switch {
		case err == nil:
			return
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			continue
		default:
			f.logUncleared(ctx, scope, unavailable("clear the budget refusal", err))
			return
		}
	}
	f.logUncleared(ctx, scope, contended("clear refusal", scope))
}

func (f *FleetStore) logUncleared(ctx context.Context, scope string, err error) {
	log.WarnContext(ctx, "coord_kv_budget_refusal_not_cleared", "scope", scope, "error", err,
		"detail", "the charge was admitted; the live meter keeps showing the old "+
			"refusal until the scope's next admitted charge clears it")
}

// bump applies one scope's delta under a compare-and-swap, reporting the
// record it wrote and whether the delta fit.
//
// A negative delta is a compensation and is never refused: it is undoing a
// charge this caller already made, so a limit has nothing to say about it.
//
// The record's refusal stamp is CARRIED through, never cleared here: see
// [FleetStore.Charge] for who clears it and why this cannot.
func (f *FleetStore) bump(ctx context.Context, scope string, delta, limit int) (budgetRecord, bool, error) {
	key := encodeKey(scope)
	for range fleetCASRetries {
		entry, err := f.budgets.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			if limit > 0 && delta > limit {
				return budgetRecord{}, false, nil
			}
			record := budgetRecord{Used: max(delta, 0), At: time.Now().UTC()}
			raw, encoded := encodeBudget(record)
			if encoded != nil {
				return budgetRecord{}, false, encoded
			}
			_, created := f.budgets.Create(ctx, key, raw)
			switch {
			case created == nil:
				return record, true, nil
			case errors.Is(created, jetstream.ErrKeyExists):
				continue
			default:
				return budgetRecord{}, false, unavailable("charge the budget", created)
			}
		}
		if err != nil {
			return budgetRecord{}, false, unavailable("read the budget", err)
		}
		var record budgetRecord
		if decode := json.Unmarshal(entry.Value(), &record); decode != nil {
			return budgetRecord{}, false, unavailable("decode the budget", decode)
		}
		// Floored at zero: a compensation for a charge whose own write
		// was already reaped (or reset by an operator mid-turn) must not
		// leave a counter that reads as credit.
		next := max(record.Used+delta, 0)
		if limit > 0 && next > limit {
			return record, false, nil
		}
		record.Used, record.At = next, time.Now().UTC()
		raw, encoded := encodeBudget(record)
		if encoded != nil {
			return budgetRecord{}, false, encoded
		}
		_, err = f.budgets.Update(ctx, key, raw, entry.Revision())
		switch {
		case err == nil:
			return record, true, nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			continue
		default:
			return budgetRecord{}, false, unavailable("charge the budget", err)
		}
	}
	// Exhausting the retries is reported as an ERROR, never as a refusal:
	// the caller fails the round rather than telling an agent it is out of
	// budget, which is the fail-closed direction the contract requires.
	return budgetRecord{}, false, contended("charge", scope)
}

func encodeBudget(record budgetRecord) ([]byte, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: encode the budget: %w", err)
	}
	return raw, nil
}

// Used reports one scope's spend.
func (f *FleetStore) Used(ctx context.Context, scope string) (int, error) {
	if scope == "" {
		return 0, errors.New("coord/kv: a budget scope is required")
	}
	entry, err := f.budgets.Get(ctx, encodeKey(scope))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, unavailable("read the budget", err)
	}
	var record budgetRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return 0, unavailable("decode the budget", err)
	}
	return record.Used, nil
}

// Usage returns every counter, org first then seats by scope.
func (f *FleetStore) Usage(ctx context.Context) ([]coord.Usage, error) {
	var out []coord.Usage
	err := f.each(ctx, f.budgets, func(kve jetstream.KeyValueEntry) error {
		var record budgetRecord
		if err := json.Unmarshal(kve.Value(), &record); err != nil {
			return unavailable("decode the budget", err)
		}
		scope, ok := decodeKey(kve.Key())
		if !ok {
			// A key this backend did not write. Skipped rather than
			// guessed at, matching the lease listing: an invented
			// scope name in the operator's budget view is worse than
			// a missing one.
			return nil
		}
		out = append(out, coord.Usage{
			Scope: scope, Used: record.Used, UpdatedAt: record.At, RefusedAt: record.RefusedAt,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	coord.SortUsage(out)
	return out, nil
}

// Reset zeroes one scope, or every scope when given "".
//
// PURGE, not delete: a tombstone would be returned by a later listing as a
// key with no value, so an operator who cleared a counter would still see the
// scope in `crewlet budgets`.
func (f *FleetStore) Reset(ctx context.Context, scope string) (int, error) {
	if scope != "" {
		if _, err := f.Used(ctx, scope); err != nil {
			return 0, err
		}
		if err := f.budgets.Purge(ctx, encodeKey(scope)); err != nil {
			return 0, unavailable("reset the budget", err)
		}
		return 1, nil
	}
	// COLLECTED FIRST, PURGED AFTER. The walk holds a live subscription to
	// this very bucket, and purging inside it would have the sweep writing
	// the records its own listing is still delivering. The set is one key
	// per counted scope, so holding it costs nothing worth the hazard.
	var keys []string
	if err := f.each(ctx, f.budgets, func(kve jetstream.KeyValueEntry) error {
		keys = append(keys, kve.Key())
		return nil
	}); err != nil {
		return 0, err
	}
	cleared := 0
	for _, key := range keys {
		if err := f.budgets.Purge(ctx, key); err != nil {
			return cleared, unavailable("reset the budget", err)
		}
		cleared++
	}
	return cleared, nil
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
func (f *FleetStore) Activate(ctx context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	if req.RevisionID == "" {
		return coord.Activation{}, errors.New("coord/kv: an activation needs a revision id")
	}
	// THE EXPECTATION IS RESOLVED FIRST, before anything is written: a
	// caller that has already lost the race must not leave a payload
	// behind for a revision the fleet will never point at.
	seq, err := f.expectedSeq(ctx, req.Expect)
	if err != nil {
		return coord.Activation{}, err
	}
	raw, err := json.Marshal(activationRecord{
		RevisionID: req.RevisionID, At: req.At.UTC(), Summary: req.Summary,
	})
	if err != nil {
		return coord.Activation{}, fmt.Errorf("coord/kv: encode the activation: %w", err)
	}
	// THE PAYLOAD FIRST. A crash here leaves a body nothing points at,
	// which the next activation replaces; the other order points the fleet
	// at bytes no node can read.
	body, err := json.Marshal(payloadRecord{RevisionID: req.RevisionID, Payload: req.Payload})
	if err != nil {
		return coord.Activation{}, fmt.Errorf("coord/kv: encode the revision payload: %w", err)
	}
	if _, put := f.config.Put(ctx, payloadKey, body); put != nil {
		return coord.Activation{}, unavailable("publish the revision payload", put)
	}
	// ONE WRITE for the flip. The store returns the revision it assigned,
	// and that revision IS the epoch — so the append and the flip cannot
	// come apart, and two nodes activating at the same instant are handed
	// two epochs by the store rather than racing over a counter this engine
	// keeps. Writing the payload into the same bucket moves the sequence
	// too, which is harmless: the epoch has to be monotonic and unique,
	// never dense.
	//
	// UPDATE RATHER THAN PUT when the caller said what it was replacing.
	// Update carries the sequence read above, so anything that wrote in
	// between makes this fail rather than overwrite — which is the only
	// thing standing between two operators editing at once and one of them
	// losing their change with a 201 in hand.
	revision, err := f.flip(ctx, req.Expect, seq, raw)
	if err != nil {
		return coord.Activation{}, err
	}
	return coord.Activation{
		Epoch:      int64(revision),
		RevisionID: req.RevisionID,
		At:         req.At.UTC(),
		Summary:    req.Summary,
	}, nil
}

// expectedSeq resolves the caller's expectation to the KV sequence to
// compare-and-set against, or reports the race.
//
// Zero means unconditional — see [coord.ActivationRequest.Expect].
func (f *FleetStore) expectedSeq(ctx context.Context, expect string) (uint64, error) {
	if expect == "" {
		return 0, nil
	}
	entry, err := f.config.Get(ctx, activationKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		// NOTHING TO HAVE RACED WITH. A node seeded from a file holds a
		// locally-active revision before it has published anything, and
		// treating that as a race would refuse every config write on it
		// until it did. See [coord.ActivationRequest.Expect].
		return 0, nil
	}
	if err != nil {
		return 0, unavailable("read the activation to compare against", err)
	}
	var record activationRecord
	if err = json.Unmarshal(entry.Value(), &record); err != nil {
		return 0, fmt.Errorf("coord/kv: decode the activation: %w", err)
	}
	if record.RevisionID != expect {
		return 0, fmt.Errorf("%w: expected %s, the fleet is on %s",
			coord.ErrActivationRaced, expect, record.RevisionID)
	}
	return entry.Revision(), nil
}

// flip writes the pointer, conditionally when there was an expectation.
func (f *FleetStore) flip(ctx context.Context, expect string, seq uint64, raw []byte) (uint64, error) {
	if expect == "" {
		revision, err := f.config.Put(ctx, activationKey, raw)
		if err != nil {
			return 0, unavailable("publish the activation", err)
		}
		return revision, nil
	}
	revision, err := f.config.Update(ctx, activationKey, raw, seq)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) || isWrongLastSequence(err) {
			// SOMEBODY WROTE BETWEEN the read and this. Reported as the
			// race rather than as an unavailable store, because the
			// caller's fix is to re-read and rebuild rather than retry.
			return 0, fmt.Errorf("%w: %s was replaced while this write was "+
				"being prepared", coord.ErrActivationRaced, expect)
		}
		return 0, unavailable("publish the activation", err)
	}
	return revision, nil
}

// isWrongLastSequence reports a compare-and-set refusal.
//
// TWO CODES, and the second is not a fallback: the server answers 10071 on a
// solo stream and 10164 on a REPLICATED one, for the same refusal. So a fleet
// — the only topology where a compare-and-set race is common — was matching
// on neither code and reaching the substring test underneath, which is a
// message this client is free to reword in any release.
//
// The message test is gone with it. A refusal read as an outage answers 503
// where it should answer 409, and the shape of that bug is a conflict an
// operator retries for ever because the engine called it an unavailable store.
func isWrongLastSequence(err error) bool {
	var api *jetstream.APIError
	if !errors.As(err, &api) {
		return false
	}
	return api.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence ||
		api.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequenceConstant
}

// payloadRecord is the current revision's sealed body on the wire. The
// revision id travels WITH it so a reader can tell "the payload for the epoch
// I am converging on" from "a payload a newer activation has already
// replaced" — the two are one key, and only the id separates them.
type payloadRecord struct {
	RevisionID string `json:"revision_id"`
	Payload    []byte `json:"payload"`
}

// Payload returns the current revision's sealed payload.
func (f *FleetStore) Payload(ctx context.Context, revisionID string) ([]byte, bool, error) {
	if revisionID == "" {
		return nil, false, errors.New("coord/kv: a payload read needs a revision id")
	}
	entry, err := f.config.Get(ctx, payloadKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, unavailable("read the revision payload", err)
	}
	var record payloadRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, false, unavailable("decode the revision payload", err)
	}
	if record.RevisionID != revisionID {
		// A newer activation has replaced it. Reported as absent rather
		// than as the wrong body: a node that applied whatever happened
		// to be there would converge on a revision the fleet is not
		// pointed at, and say it succeeded.
		return nil, false, nil
	}
	return record.Payload, true, nil
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
	var out []coord.NodeApply
	err := f.each(ctx, f.status, func(kve jetstream.KeyValueEntry) error {
		var record applyRecord
		//nolint:nilerr // An undecodable status row is SKIPPED rather than
		// raised, because this read is what reports the fleet's apply
		// progress: failing it over one row would blank the whole view,
		// where dropping the row shows every node that IS readable and
		// leaves the bad one looking as it does — unreported.
		if err := json.Unmarshal(kve.Value(), &record); err != nil {
			return nil
		}
		node, ok := decodeKey(kve.Key())
		if !ok {
			return nil
		}
		out = append(out, coord.NodeApply{
			NodeID: node, Epoch: record.Epoch, RevisionID: record.RevisionID,
			Status: record.Status, Error: record.Error, UpdatedAt: record.UpdatedAt,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b coord.NodeApply) int {
		// NEWEST FIRST, so the negated compare; the node id breaks a tie
		// ascending so one instant's statuses have a stable order.
		return cmp.Or(b.UpdatedAt.Compare(a.UpdatedAt), cmp.Compare(a.NodeID, b.NodeID))
	})
	return out, nil
}

// ---- the sealed credentials -------------------------------------------- //

// secretRecord is one credential's wire form in the bucket.
//
// The VALUE IS ALREADY AN ENVELOPE when it arrives — sealed by the Tier A
// keyring, which lives on each node's disk and never reaches this store — so
// nothing here can open it and the key id beside it is the only thing a
// rotation sweep needs to read.
type secretRecord struct {
	Name      string    `json:"name"`
	Value     string    `json:"value"`
	KeyID     string    `json:"key_id"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	Source    string    `json:"source,omitempty"`
}

// Secret reads one sealed value.
func (f *FleetStore) Secret(ctx context.Context, name string) (coord.SecretRecord, bool, error) {
	if name == "" {
		return coord.SecretRecord{}, false, errors.New("coord/kv: a secret needs a name")
	}
	entry, err := f.secrets.Get(ctx, encodeKey(name))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.SecretRecord{}, false, nil
	case err != nil:
		// RAISED. "No such credential" renders downstream as an unset
		// ${VAR}, which is an empty string handed to a provider, which is
		// an auth failure blamed on the vendor. An unreadable store must
		// never be able to say it.
		return coord.SecretRecord{}, false, unavailable("read the secret", err)
	}
	rec, ok := decodeSecret(entry.Value())
	if !ok {
		return coord.SecretRecord{}, false, fmt.Errorf(
			"coord/kv: the stored secret %q is not decodable", name)
	}
	return rec, true, nil
}

// SecretValues returns every sealed value.
func (f *FleetStore) SecretValues(ctx context.Context) ([]coord.SecretRecord, error) {
	var out []coord.SecretRecord
	// RAISED rather than skipped, for the same reason Secret raises and more
	// sharply: this listing IS the engine's boot snapshot, so a value
	// silently dropped here becomes an empty ${VAR} everywhere at once.
	err := f.each(ctx, f.secrets, func(kve jetstream.KeyValueEntry) error {
		if _, ok := decodeKey(kve.Key()); !ok {
			return nil
		}
		rec, ok := decodeSecret(kve.Value())
		if !ok {
			return fmt.Errorf("coord/kv: a stored secret is not decodable")
		}
		out = append(out, rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b coord.SecretRecord) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// PutSecret writes a sealed value, replacing any prior one.
//
// A PLAIN PUT, not a compare-and-swap: see [coord.Secrets] for why rotation
// wants last-write-wins rather than a lost race one operator has to retry.
func (f *FleetStore) PutSecret(ctx context.Context, rec coord.SecretRecord) error {
	switch {
	case rec.Name == "":
		return errors.New("coord/kv: a secret needs a name")
	case rec.Value == "":
		// An empty envelope is not an empty secret — it is a caller that
		// forgot to seal. Storing it would resolve as an empty ${VAR} on
		// every node, which is the failure this bucket exists to prevent.
		return fmt.Errorf("coord/kv: secret %q has no sealed value", rec.Name)
	}
	raw, err := json.Marshal(secretRecord{
		Name: rec.Name, Value: rec.Value, KeyID: rec.KeyID,
		UpdatedAt: rec.UpdatedAt.UTC(), UpdatedBy: rec.UpdatedBy, Source: rec.Source,
	})
	if err != nil {
		return fmt.Errorf("coord/kv: encode the secret: %w", err)
	}
	if _, err := f.secrets.Put(ctx, encodeKey(rec.Name), raw); err != nil {
		return unavailable("write the secret", err)
	}
	return nil
}

// DeleteSecret removes a value, reporting whether it was there.
//
// PURGED rather than deleted, like every other record here that must not come
// back: a KV delete leaves a tombstone the history keeps, and for a credential
// the history is the thing you least want kept.
func (f *FleetStore) DeleteSecret(ctx context.Context, name string) (bool, error) {
	if name == "" {
		return false, errors.New("coord/kv: a secret needs a name")
	}
	key := encodeKey(name)
	if _, err := f.secrets.Get(ctx, key); errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	} else if err != nil {
		return false, unavailable("read the secret before deleting it", err)
	}
	if err := f.secrets.Purge(ctx, key); err != nil {
		return false, unavailable("delete the secret", err)
	}
	return true, nil
}

// decodeSecret reads one stored record.
func decodeSecret(raw []byte) (coord.SecretRecord, bool) {
	var rec secretRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Name == "" {
		return coord.SecretRecord{}, false
	}
	return coord.SecretRecord{
		Name: rec.Name, Value: rec.Value, KeyID: rec.KeyID,
		UpdatedAt: rec.UpdatedAt, UpdatedBy: rec.UpdatedBy, Source: rec.Source,
	}, true
}

// ---- the agent-to-agent channels --------------------------------------- //

// channelRecord is one authorization row on the wire.
//
// The stamps travel as time.Time through JSON, which is RFC 3339 and
// therefore round-trips in UTC. ClosedAt is a POINTER so "open" is the
// absence of a value rather than a zero instant a decoder could confuse with
// the epoch — the one field where a wrong reading changes whether an answer
// is delivered.
type channelRecord struct {
	Requester string     `json:"requester"`
	Target    string     `json:"target"`
	Messages  int        `json:"messages"`
	OpenedAt  time.Time  `json:"opened_at"`
	LastAt    time.Time  `json:"last_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
}

func encodeChannel(ch coord.Channel) ([]byte, error) {
	record := channelRecord{
		Requester: ch.Requester, Target: ch.Target, Messages: ch.Messages,
		OpenedAt: ch.OpenedAt.UTC(), LastAt: ch.LastAt.UTC(),
	}
	if !ch.ClosedAt.IsZero() {
		closed := ch.ClosedAt.UTC()
		record.ClosedAt = &closed
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: encode the channel: %w", err)
	}
	return raw, nil
}

func decodeChannel(id string, raw []byte) (coord.Channel, error) {
	var record channelRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return coord.Channel{}, unavailable("decode the channel", err)
	}
	ch := coord.Channel{
		ID: id, Requester: record.Requester, Target: record.Target,
		Messages: record.Messages, OpenedAt: record.OpenedAt, LastAt: record.LastAt,
	}
	if record.ClosedAt != nil {
		ch.ClosedAt = *record.ClosedAt
	}
	return ch, nil
}

// OpenChannel records a new channel, ignoring an id that already exists.
//
// Create, not Put: the id is minted per ask, so ErrKeyExists means a retried
// publish of ONE ask. Overwriting would reset the counter and replace the
// participants of a channel that is already carrying an answer.
func (f *FleetStore) OpenChannel(ctx context.Context, ch coord.Channel) error {
	if ch.ID == "" {
		return errors.New("coord/kv: a channel needs an id")
	}
	raw, err := encodeChannel(ch)
	if err != nil {
		return err
	}
	_, err = f.channels.Create(ctx, encodeKey(ch.ID), raw)
	switch {
	case err == nil, errors.Is(err, jetstream.ErrKeyExists):
		return nil
	default:
		return unavailable("open the channel", err)
	}
}

// Channel reads one record.
func (f *FleetStore) Channel(ctx context.Context, id string) (coord.Channel, bool, error) {
	entry, err := f.channels.Get(ctx, encodeKey(id))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return coord.Channel{}, false, nil
	}
	if err != nil {
		return coord.Channel{}, false, unavailable("read the channel", err)
	}
	ch, err := decodeChannel(id, entry.Value())
	if err != nil {
		return coord.Channel{}, false, err
	}
	return ch, true, nil
}

// CloseChannel ends a channel, leaving an already-closed one untouched.
func (f *FleetStore) CloseChannel(ctx context.Context, id string, at time.Time) (coord.Channel, bool, error) {
	return f.mutateChannel(ctx, "close", id, func(ch coord.Channel) coord.Channel {
		if ch.Open() {
			ch.ClosedAt = at.UTC()
			ch.LastAt = at.UTC()
		}
		return ch
	})
}

// CountChannelMessage records one message against a channel's own budget.
func (f *FleetStore) CountChannelMessage(ctx context.Context, id string, at time.Time) (coord.Channel, bool, error) {
	return f.mutateChannel(ctx, "count a message on", id, func(ch coord.Channel) coord.Channel {
		ch.Messages++
		ch.LastAt = at.UTC()
		return ch
	})
}

// mutateChannel applies a read-modify-write under a compare-and-swap.
//
// A CAS rather than a blind Put, because the two writers are real: the
// requester's node counts the ask while the target's node counts the answer,
// and a lost update there is a message count that under-reports the very
// traffic the cap on it exists to catch.
func (f *FleetStore) mutateChannel(ctx context.Context, what, id string, apply func(coord.Channel) coord.Channel) (coord.Channel, bool, error) {
	key := encodeKey(id)
	for range fleetCASRetries {
		entry, err := f.channels.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return coord.Channel{}, false, nil
		}
		if err != nil {
			return coord.Channel{}, false, unavailable("read the channel", err)
		}
		ch, err := decodeChannel(id, entry.Value())
		if err != nil {
			return coord.Channel{}, false, err
		}
		next := apply(ch)
		raw, err := encodeChannel(next)
		if err != nil {
			return coord.Channel{}, false, err
		}
		_, err = f.channels.Update(ctx, key, raw, entry.Revision())
		switch {
		case err == nil:
			return next, true, nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			continue
		default:
			return coord.Channel{}, false, unavailable(what+" the channel", err)
		}
	}
	return coord.Channel{}, false, contended(what, id)
}

// OpenChannels returns every channel still open, by id.
func (f *FleetStore) OpenChannels(ctx context.Context) ([]coord.Channel, error) {
	var out []coord.Channel
	err := f.each(ctx, f.channels, func(kve jetstream.KeyValueEntry) error {
		id, ok := decodeKey(kve.Key())
		if !ok {
			return nil
		}
		ch, err := decodeChannel(id, kve.Value())
		if err != nil {
			return err
		}
		if ch.Open() {
			out = append(out, ch)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b coord.Channel) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// PurgeChannels deletes channels closed before the cutoff.
//
// Purge rather than Delete, so the key's history goes with it: a Delete
// leaves a tombstone revision, and a bucket with no TTL keeps every one of
// them for the life of the deployment.
func (f *FleetStore) PurgeChannels(ctx context.Context, cutoff time.Time) (int64, error) {
	// DECIDED FIRST, PURGED AFTER — the sweep must not write to the bucket
	// its own listing is still being delivered from. Each candidate carries
	// the revision it was decided on, so the predicate below is unchanged.
	type doomed struct {
		key      string
		revision uint64
	}
	var candidates []doomed
	err := f.each(ctx, f.channels, func(kve jetstream.KeyValueEntry) error {
		id, ok := decodeKey(kve.Key())
		if !ok {
			return nil
		}
		ch, err := decodeChannel(id, kve.Value())
		if err != nil {
			return err
		}
		if ch.Open() || !ch.ClosedAt.Before(cutoff) {
			return nil
		}
		candidates = append(candidates, doomed{key: kve.Key(), revision: kve.Revision()})
		return nil
	})
	if err != nil {
		return 0, err
	}
	var n int64
	for _, c := range candidates {
		// Predicated on the revision we read: a channel somebody
		// re-opened or counted between the read and the delete is not
		// the one this sweep decided to drop.
		if err := f.channels.Purge(ctx, c.key, jetstream.LastRevision(c.revision)); err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return n, unavailable("purge a channel", err)
		}
		n++
	}
	return n, nil
}

// ---- the scheduled-fire claims ----------------------------------------- //

// ClaimFire records one fire identity, reporting whether this call wrote it.
//
// Create, so the first writer wins and every later tick — this node's own
// re-evaluated minute, or a peer that has just picked up the scheduler duty —
// reads false and does not dispatch.
func (f *FleetStore) ClaimFire(ctx context.Context, key string, at time.Time) (bool, error) {
	if key == "" {
		return false, errors.New("coord/kv: a fire claim needs a key")
	}
	raw, err := json.Marshal(fireRecord{At: at.UTC()})
	if err != nil {
		return false, fmt.Errorf("coord/kv: encode the fire claim: %w", err)
	}
	_, err = f.fires.Create(ctx, encodeKey(key), raw)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyExists):
		return false, nil
	default:
		// RAISED, never reported as "somebody else has it". The caller
		// fails closed on an error and skips the tick; reporting a lost
		// race instead would tell it the fire was handled, and the next
		// tick would skip it too.
		return false, unavailable("claim the fire", err)
	}
}

// fireRecord is one claim on the wire. The instant is diagnostic — an operator
// asking "when was the 09:00 standup claimed, and by a tick or a catchup pass"
// reads it out of the bucket — and nothing branches on it.
type fireRecord struct {
	At time.Time `json:"at"`
}

// ---- the detached sandbox runs ----------------------------------------- //

// SandboxRun reads one run's record.
func (f *FleetStore) SandboxRun(ctx context.Context, turnID string) (coord.Record, bool, error) {
	entry, err := f.runs.Get(ctx, encodeKey(turnID))
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
	_, err := f.runs.Create(ctx, encodeKey(turnID), value)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyExists):
		return false, nil
	default:
		return false, unavailable("create the sandbox run", err)
	}
}

// UpdateSandboxRun writes at a version, reporting whether that version held.
func (f *FleetStore) UpdateSandboxRun(ctx context.Context, turnID string, value []byte, version uint64) (bool, error) {
	_, err := f.runs.Update(ctx, encodeKey(turnID), value, version)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound):
		// A LOST RACE, not a fault: the caller re-reads and re-decides,
		// because the condition it evaluated may no longer hold. A
		// deleted key lands here too — the run finished under it.
		return false, nil
	default:
		return false, unavailable("update the sandbox run", err)
	}
}

// DeleteSandboxRun removes a record at a version.
//
// Purge rather than Delete, so the key's history goes with it: a Delete leaves
// a tombstone revision, and a bucket with no TTL keeps every one of them for
// the life of the deployment.
func (f *FleetStore) DeleteSandboxRun(ctx context.Context, turnID string, version uint64) (bool, error) {
	err := f.runs.Purge(ctx, encodeKey(turnID), jetstream.LastRevision(version))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound):
		return false, nil
	default:
		return false, unavailable("delete the sandbox run", err)
	}
}

// ---- the integration reconcile status ---------------------------------- //

// IntegrationStatuses returns every recorded status, keyed by surface.
func (f *FleetStore) IntegrationStatuses(ctx context.Context) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := f.each(ctx, f.integrations, func(kve jetstream.KeyValueEntry) error {
		kind, ok := decodeKey(kve.Key())
		if !ok {
			// A key this backend did not write. Skipped rather than
			// guessed at, exactly as the fleet listing does: inventing a
			// surface name would put a row nothing reconciles into an
			// operator's status page.
			return nil
		}
		// COPIED. The entry's buffer belongs to the client and the caller
		// keeps this past the walk.
		out[kind] = bytes.Clone(kve.Value())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PutIntegrationStatus records one surface's status.
func (f *FleetStore) PutIntegrationStatus(ctx context.Context, kind string, value []byte) error {
	if kind == "" {
		return errors.New("coord/kv: an integration status needs a surface name")
	}
	if _, err := f.integrations.Put(ctx, encodeKey(kind), value); err != nil {
		return unavailable("record the integration status", err)
	}
	return nil
}

// DeleteIntegrationStatus drops a surface's status.
//
// Purge rather than Delete, matching the sandbox runs above: a Delete leaves a
// tombstone revision, and a bucket with no TTL keeps every one of them for the
// life of the deployment. A surface an operator adds and removes a few times
// while wiring a company would otherwise accumulate history nothing reads.
func (f *FleetStore) DeleteIntegrationStatus(ctx context.Context, kind string) error {
	err := f.integrations.Purge(ctx, encodeKey(kind))
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("delete the integration status", err)
	}
	return nil
}
