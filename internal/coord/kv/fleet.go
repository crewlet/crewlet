package kv

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// The fleet-shared state on JetStream KV.
//
// # Why ELEVEN buckets and not one
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
	rateSuffix     = "_rate"
	claimsSuffix   = "_claims"
	ledgerSuffix   = "_ledger"
	cooldownSuffix = "_cooldowns"
	statusSuffix   = "_status"
	configSuffix   = "_config"
	budgetSuffix   = "_budgets"
	channelSuffix  = "_channels"
	firesSuffix    = "_fires"
	runsSuffix     = "_sandbox_runs"
	secretsSuffix  = "_secrets"
	activationKey  = "activation"
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
	// BucketPrefix names the eleven buckets. Empty means "crewlet", matching
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

	// Replicas is the JetStream replica count for all eleven.
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
	rate      jetstream.KeyValue
	claims    jetstream.KeyValue
	ledger    jetstream.KeyValue
	cooldowns jetstream.KeyValue
	status    jetstream.KeyValue
	config    jetstream.KeyValue
	budgets   jetstream.KeyValue
	channels  jetstream.KeyValue
	secrets   jetstream.KeyValue
	fires     jetstream.KeyValue
	runs      jetstream.KeyValue

	// The document families. Ageless, and each its own bucket — see
	// documents.go for why the split is by family rather than by class.
	work      jetstream.KeyValue
	pages     jetstream.KeyValue
	kbVectors jetstream.KeyValue

	rateWindow time.Duration
	freshness  time.Duration
}

var _ coord.Fleet = (*FleetStore)(nil)

// OpenFleet creates or adopts the fourteen buckets and returns the backend.
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
		{&store.work, workSuffix,
			"Crewlet work items, comments and changes; NO TTL — an item is the company's own record", 0},
		{&store.pages, pagesSuffix,
			"Crewlet knowledge-base pages and revisions; NO TTL — a page is the company's own record", 0},
		{&store.kbVectors, kbVectorsSuffix,
			"Crewlet knowledge embeddings; NO TTL — derived, and dropped wholesale when the width changes", 0},
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

// ---- the token counters ------------------------------------------------ //

// budgetRecord is one scope's spend.
type budgetRecord struct {
	Used int       `json:"used"`
	At   time.Time `json:"at"`
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
	// before anything is written — org first, matching the order below, and
	// so a seat whose own cap is smaller than the charge never costs the
	// org a bump and an unwind.
	for _, scope := range []struct {
		name, key string
		limit     int
	}{{"org", coord.OrgScope, orgLimit}, {"agent", agentScope, agentLimit}} {
		if scope.limit > 0 && tokens > scope.limit {
			used, err := f.Used(ctx, scope.key)
			if err != nil {
				return coord.Spend{}, err
			}
			return coord.Spend{RefusedScope: scope.name, RefusedUsed: used, RefusedLimit: scope.limit}, nil
		}
	}

	orgUsed, fits, err := f.bump(ctx, coord.OrgScope, tokens, orgLimit)
	if err != nil {
		return coord.Spend{}, err
	}
	if !fits {
		return coord.Spend{RefusedScope: "org", RefusedUsed: orgUsed, RefusedLimit: orgLimit}, nil
	}

	agentUsed, fits, err := f.bump(ctx, agentScope, tokens, agentLimit)
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
		return coord.Spend{RefusedScope: "agent", RefusedUsed: agentUsed, RefusedLimit: agentLimit}, nil
	}
	return coord.Spend{OK: true, OrgUsed: orgUsed, AgentUsed: agentUsed}, nil
}

// bump applies one scope's delta under a compare-and-swap, reporting the
// resulting usage and whether it fit.
//
// A negative delta is a compensation and is never refused: it is undoing a
// charge this caller already made, so a limit has nothing to say about it.
func (f *FleetStore) bump(ctx context.Context, scope string, delta, limit int) (int, bool, error) {
	key := encodeKey(scope)
	for range fleetCASRetries {
		entry, err := f.budgets.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			if limit > 0 && delta > limit {
				return 0, false, nil
			}
			raw, encoded := encodeBudget(max(delta, 0))
			if encoded != nil {
				return 0, false, encoded
			}
			_, created := f.budgets.Create(ctx, key, raw)
			switch {
			case created == nil:
				return max(delta, 0), true, nil
			case errors.Is(created, jetstream.ErrKeyExists):
				continue
			default:
				return 0, false, unavailable("charge the budget", created)
			}
		}
		if err != nil {
			return 0, false, unavailable("read the budget", err)
		}
		var record budgetRecord
		if decode := json.Unmarshal(entry.Value(), &record); decode != nil {
			return 0, false, unavailable("decode the budget", decode)
		}
		// Floored at zero: a compensation for a charge whose own write
		// was already reaped (or reset by an operator mid-turn) must not
		// leave a counter that reads as credit.
		next := max(record.Used+delta, 0)
		if limit > 0 && next > limit {
			return record.Used, false, nil
		}
		raw, encoded := encodeBudget(next)
		if encoded != nil {
			return 0, false, encoded
		}
		_, err = f.budgets.Update(ctx, key, raw, entry.Revision())
		switch {
		case err == nil:
			return next, true, nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			continue
		default:
			return 0, false, unavailable("charge the budget", err)
		}
	}
	// Exhausting the retries is reported as an ERROR, never as a refusal:
	// the caller fails the round rather than telling an agent it is out of
	// budget, which is the fail-closed direction the contract requires.
	return 0, false, contended("charge", scope)
}

func encodeBudget(used int) ([]byte, error) {
	raw, err := json.Marshal(budgetRecord{Used: used, At: time.Now().UTC()})
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
	keys, err := f.budgets.ListKeys(ctx)
	if err != nil {
		return nil, unavailable("list the budgets", err)
	}
	var out []coord.Usage
	for key := range keys.Keys() {
		entry, err := f.budgets.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, unavailable("read the budget", err)
		}
		var record budgetRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			return nil, unavailable("decode the budget", err)
		}
		scope, ok := decodeKey(key)
		if !ok {
			// A key this backend did not write. Skipped rather than
			// guessed at, matching the lease listing: an invented
			// scope name in the operator's budget view is worse than
			// a missing one.
			continue
		}
		out = append(out, coord.Usage{Scope: scope, Used: record.Used, UpdatedAt: record.At})
	}
	coord.SortUsage(out)
	return out, nil
}

// Reset zeroes one scope, or every scope when given "".
//
// PURGE, not delete: a tombstone would be returned by a later ListKeys as a
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
	keys, err := f.budgets.ListKeys(ctx)
	if err != nil {
		return 0, unavailable("list the budgets", err)
	}
	cleared := 0
	for key := range keys.Keys() {
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
// MATCHED ON THE MESSAGE, which is not something to do lightly: the client
// surfaces this as an API error whose typed form is not exported, so there is
// nothing else to match on. Getting it wrong is not silent — a refusal read as
// an outage answers 503 instead of 409, which an operator sees immediately —
// and the conformance suite exercises the real store rather than trusting it.
func isWrongLastSequence(err error) bool {
	var api *jetstream.APIError
	if errors.As(err, &api) && api.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "wrong last sequence")
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
	keys, err := f.secrets.ListKeys(ctx)
	if err != nil {
		return nil, unavailable("list the secrets", err)
	}
	var out []coord.SecretRecord
	for key := range keys.Keys() {
		if _, ok := decodeKey(key); !ok {
			continue
		}
		entry, err := f.secrets.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			// RAISED for the same reason Secret raises, and more sharply:
			// this listing IS the engine's boot snapshot, so a value
			// silently dropped here becomes an empty ${VAR} everywhere at
			// once.
			return nil, unavailable("read a secret", err)
		}
		rec, ok := decodeSecret(entry.Value())
		if !ok {
			return nil, fmt.Errorf("coord/kv: a stored secret is not decodable")
		}
		out = append(out, rec)
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
	keys, err := f.channels.ListKeys(ctx)
	if err != nil {
		return nil, unavailable("list the channels", err)
	}
	var out []coord.Channel
	for key := range keys.Keys() {
		id, ok := decodeKey(key)
		if !ok {
			continue
		}
		entry, err := f.channels.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, unavailable("read a channel", err)
		}
		ch, err := decodeChannel(id, entry.Value())
		if err != nil {
			return nil, err
		}
		if ch.Open() {
			out = append(out, ch)
		}
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
	keys, err := f.channels.ListKeys(ctx)
	if err != nil {
		return 0, unavailable("list the channels", err)
	}
	var n int64
	for key := range keys.Keys() {
		id, ok := decodeKey(key)
		if !ok {
			continue
		}
		entry, err := f.channels.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return n, unavailable("read a channel", err)
		}
		ch, err := decodeChannel(id, entry.Value())
		if err != nil {
			return n, err
		}
		if ch.Open() || !ch.ClosedAt.Before(cutoff) {
			continue
		}
		// Predicated on the revision we read: a channel somebody
		// re-opened or counted between the read and the delete is not
		// the one this sweep decided to drop.
		if err := f.channels.Purge(ctx, key, jetstream.LastRevision(entry.Revision())); err != nil {
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
	keys, err := f.runs.ListKeys(ctx)
	if err != nil {
		return nil, unavailable("list the sandbox runs", err)
	}
	var out []coord.Record
	for key := range keys.Keys() {
		turnID, ok := decodeKey(key)
		if !ok {
			continue
		}
		entry, err := f.runs.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			// RAISED, not skipped. A listing that quietly dropped a run
			// tells the seat's new owner there is nothing to recover,
			// which abandons a billed box — the exact failure this
			// bucket exists to end.
			return nil, unavailable("read a sandbox run", err)
		}
		out = append(out, coord.Record{
			Key: turnID, Value: entry.Value(), Version: entry.Revision(),
		})
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
