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

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/period"
)

// A BUCKET IS A STREAM, so provisioning a replicated one has the hazards
// [internal/queue/jetstream] documents for streams, fixed the same way: both
// sides take their budgets, retry cadence and "still forming" predicate from
// [internal/jsprovision], so neither drifts from the other.
//
// The deadline: nats.go gives a context with no deadline a five-second API
// timeout, too short for a create waiting on a slow metadata group (symptom:
// `open crewlet_rate: context deadline exceeded`, naming neither).
//
// The placement retry: `No suitable peers` is transient during formation and
// the one error worth waiting out; retrying any other (a bad TTL, a replica
// conflict, auth) turns a config mistake into a long hang.

// positionsSuffix is the per-node state-log position bucket.
const positionsSuffix = "_statelog_positions"

// openBucket creates one bucket when it is absent and OBSERVES it when it is
// present, waiting out a cluster that is still forming.
//
// Not CreateOrUpdate: every node opens every bucket at boot, and its update
// half makes each racing loser a WRITE against a metadata group still
// electing, which never returns. Create-else-observe (as
// [internal/queue/jetstream] does for a stream) makes "bucket exists" the
// other node's win, and a bucket's TTL a deployment-wide fact nobody re-asserts.
//
// ctx is the boot's; the per-create deadline is derived inside, since the
// read-back runs once it has expired ([jsprovision.Settle]). The facts
// returned are what is IN FORCE, read back on every path ([observeBucket]).
func openBucket(ctx context.Context, js jetstream.JetStream,
	clustered bool, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, bucketFacts, error) {

	// A BREADCRUMB: a boot opens every bucket in a row and logs nothing
	// between them, so a node hung here would not say which bucket. It spans
	// the lookup and the create, and stops where the create ends, so it never
	// names a step the member is not on. On ctx, because the two halves carry
	// separate deadlines and a watcher on one would stop when it gave up.
	stop := jsprovision.WhenSlow(ctx, func(after time.Duration) {
		log.WarnContext(ctx, "coord_kv_bucket_slow", "bucket", cfg.Bucket,
			"replicas", cfg.Replicas, "waited", after,
			"detail", "this bucket is still being provisioned — looked up, "+
				"and created if it was absent; on a fleet that is a metadata "+
				"group that has not settled, and the next line from this node "+
				"says whether it got past it")
	})

	// THE LOOKUP IS SIZED AS A READ: its ceiling is
	// [jsprovision.LookupBudget], so an unanswered lookup leaves the create
	// its budget, and each request goes through [jsprovision.Read], re-asked
	// a [jsprovision.ReadTerm] later rather than a write's
	// [jsprovision.AskTerm]. On a fleet booting together a bucket another
	// member just asked for is in flight with no leader, and every member but
	// its leader drops a lookup of it: at a write's term each drop costs
	// sixteen seconds a bucket, at a read's two.
	bucket, err := jsprovision.LookupBucket(ctx, js, cfg.Bucket)
	switch {
	case err == nil:
		stop()
		// ON ctx: a slow lookup may have spent its own deadline, and
		// observeBucket owns its own term.
		return observeBucket(ctx, js, bucket, cfg)
	case errors.Is(err, jetstream.ErrBucketNotFound):
		// TOLD it is absent. Create it below.
	case jsprovision.Unanswered(ctx, err):
		// TOLD NOTHING, not the same fact as "not there"
		// ([jsprovision.Unanswered]): the create below answers it either way.
		log.WarnContext(ctx, "coord_kv_bucket_lookup_unanswered",
			"bucket", cfg.Bucket, "error", err.Error(),
			"detail", "the broker did not say whether this bucket exists, so "+
				"the create below decides it: absent and it is made, present "+
				"and it comes back as a peer's win and its replicas are read")
	default:
		stop()
		return nil, bucketFacts{}, err
	}
	// WithTimeout only shortens, so a tighter caller deadline — the sequence
	// ceiling [OpenFleet] applies — still bounds each create.
	createCtx, cancel := context.WithTimeout(ctx,
		jsprovision.Clustered(clustered).Budget())
	defer cancel()
	// REASSIGNED, not shadowed: bucket is nil on every path reaching here,
	// and it is one object.
	var createErr error
	bucket, createErr = createKeyValue(createCtx, js, clustered, cfg)
	stop()
	if createErr == nil {
		// THIS NODE MADE IT, and it is still READ BACK: the create returns
		// what was sent, not what is in force (a server clamp, a peer's
		// resize since the lookup).
		return observeBucket(ctx, js, bucket, cfg)
	}
	if jsprovision.NoApplicableLimit(createErr) {
		// NO LIMIT APPLIES AT ALL: the account's tiered limits carry none
		// for this replica class, so there is no number to make smaller,
		// and the detail names the class to give one. Asked before Refused,
		// which it is one of: read back, it reported a bucket "not there",
		// the sentence most likely read as corruption.
		return nil, bucketFacts{}, fmt.Errorf("%w%s", createErr,
			jsprovision.NoApplicableLimitDetail(cfg.Replicas))
	}
	if jsprovision.Refused(createErr) {
		// REFUSED ON THE REQUEST ALONE ([jsprovision.Refused]): nothing was
		// placed, so a read-back would only wrap it as "not there". An
		// unplaceable create ([jsprovision.Unplaceable]) was already retried
		// by createKeyValue; an out-of-capacity one
		// ([jsprovision.OutOfCapacity]) was asked once, since waiting frees
		// nothing. The room is never this bucket's own (it declares no
		// ceiling, MaxBytes -1) but what the limit's other reservations
		// hold, the state logs' above all. Standalone,
		// `stream.store_max_bytes` can refuse it; clustered, peer selection
		// skips storage for an object with no ceiling
		// (server/jetstream_cluster.go, selectPeerGroup), so a refusal for
		// room is another member's disk and stays Unplaceable.
		return nil, bucketFacts{}, createErr
	}
	// A PEER MAY HAVE WON THE RACE, shown as [jetstream.ErrBucketExists] or
	// as a TIMEOUT: two members create at once and the server holds the
	// loser past its deadline while the group settles. So the question is
	// re-asked while it answers not-found, since a peer's create reaches
	// this member only on its next metadata update ([jsprovision.Settle]).
	// On ctx, not createCtx, which just expired.
	bucket, err = jsprovision.Settle(ctx,
		func(ctx context.Context) (jetstream.KeyValue, error) { return js.KeyValue(ctx, cfg.Bucket) })
	if err != nil {
		// The create's error is the cause; the read-back's only confirms it.
		return nil, bucketFacts{}, fmt.Errorf("%w (and it is not there: %w)", createErr, err)
	}
	// A PEER MADE IT, so its replica count is in force and is checked.
	return observeBucket(ctx, js, bucket, cfg)
}

// observeBucket reads back what is IN FORCE on a bucket: the replica count,
// which it REFUSES to run short of, and the age, which the caller reports
// ([reportRetention]).
//
// Refused rather than resized (observeStream's rule in
// [internal/queue/jetstream]): nothing is applied to a bucket that exists.
// Replication alone is refused over because a shortfall is invisible until a
// node is lost — leases, epochs and SECRETS on one disk while every node
// reports itself configured. Equal or higher passes. It reads at one replica
// too, where the adopted age still needs checking, through [readBucket] on the
// boot's ctx ([jsprovision.Settle]): the per-create deadline may be spent.
func observeBucket(ctx context.Context, js jetstream.JetStream, bucket jetstream.KeyValue,
	cfg jetstream.KeyValueConfig) (jetstream.KeyValue, bucketFacts, error) {

	facts, err := readBucket(ctx, js, bucket)
	if err != nil {
		return nil, bucketFacts{}, err
	}
	if want := max(cfg.Replicas, 1); facts.replicas < want {
		return nil, bucketFacts{}, fmt.Errorf(
			"coord/kv: the running bucket %q is replicated %dx and this node is "+
				"configured for %dx: it holds leases, fencing epochs and this "+
				"company's secrets on fewer copies than stream.replicas promises, "+
				"and losing one node loses them. Nothing is applied to a bucket "+
				"that already exists, so this is an operator gesture: align "+
				"stream.replicas across the fleet, or resize this bucket's stream "+
				"deliberately (a bucket IS a stream: nats stream update --replicas)",
			cfg.Bucket, facts.replicas, want)
	}
	return bucket, facts, nil
}

// reportRetention warns when the age in force on a bucket is not the one this
// node asked for.
//
// Reported, never applied: a bucket that exists is ADOPTED ([openBucket]), and
// rewriting it would let the last node to boot decide. The remedy is an
// operator's: align the config and delete the bucket while the fleet is down.
// The duty bucket reports nothing here: its age is a ceiling raised and never
// lowered ([openDuties]), so above coord.MaxDutyTTL is healthy. A line is
// worth it because each mismatch is otherwise silent — a ledger at a peer's
// shorter age forgets completions a redelivery can still reach.
func reportRetention(ctx context.Context, name string, want time.Duration, facts bucketFacts) {
	if facts.age == want {
		return
	}
	log.WarnContext(ctx, "bucket_retention_mismatch", "bucket", name,
		"in_force", facts.age, "this_node", want,
		"detail", "a peer created this bucket with a different age and a booting "+
			"node adopts a bucket rather than rewriting it, so every record here "+
			"expires on the age in force",
		"remedy", "make the retention agree across the fleet, or delete the bucket "+
			"while the fleet is down so the next boot re-creates it")
}

// createKeyValue makes the bucket, waiting out a cluster that has not yet seen
// enough members to place it. The loop wraps the create alone, as
// createStream's does: an unplaceable create placed nothing, so re-running the
// lookup would re-ask a question whose answer cannot have changed.
func createKeyValue(ctx context.Context, js jetstream.JetStream, clustered bool,
	cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {

	var bucket jetstream.KeyValue
	err := jsprovision.Place(ctx, jsprovision.Clustered(clustered).AskTerm(), func(ctx context.Context) error {
		var e error
		bucket, e = js.CreateKeyValue(ctx, cfg)
		return e
	}, func() {
		log.InfoContext(ctx, "coord_kv_bucket_awaiting_peers",
			"bucket", cfg.Bucket, "replicas", cfg.Replicas,
			"detail", "the cluster has not yet seen enough members to place "+
				"this bucket; retrying until the provisioning deadline")
	})
	if err != nil {
		return nil, err
	}
	return bucket, nil
}

// The fleet-shared state on JetStream KV.
//
// # Why TWENTY-FOUR buckets and not one
//
// A bucket's TTL is its stream's MaxAge and jetstream.KeyTTL is create-only
// (package doc, "Per-key TTL cannot be used for this"), so retention belongs
// to the BUCKET, and each of these has its own:
//
//	rate       a few windows' width: a closed window ages out, a live one never
//	windows    three days, for windows up to coord.MaxCounterWindow: rate's age
//	           would reap a day's count mid-window, and the valve's per-second
//	           records would pile up at this one

//	claims     the dedupe window, minutes: covers an app's retries and a
//	           replay, never a deliberate re-send later
//	ledger     days: outlasts the redelivery horizon and the catchup floor
//	status     a minute: a node that stops reporting leaves the fleet view
//	nodeStatus the same minute: rows rewritten every reconcile interval, so
//	           only a dead node's age out, with no sweep (coord)
//	config     none: the pointer is the fencing sequence (an expired one would
//	           restart the epoch); an unnamed body goes by the sweep (markers.go)
//	budgets    thirty-two days, the longest calendar window and a day; the age
//	           is not the reset, which the charge's roll makes (ADR-0019)
//	channels   none: an age cannot tell an open channel from a closed one, and
//	           would reap an ask still waiting for its answer

//	follows    ninety days, a thread's last-activity horizon: every re-assert
//	           rewrites it, and an early expiry costs one missed reply
//	chatReads  none: a read cursor never meaningfully expires, and an aged-out
//	           one marks a room unread. Apart from positions for its LOAD: the
//	           busiest record the fleet writes stays off the trim's raft group
//	           and leader (coord's chatread.go)
//	fires      the catchup ceiling, days: an earlier expiry runs a fire twice
//	rebases    a month, the operation ledger's: a rebase is inherited only
//	           within the mint horizon, so it goes when none can inherit it;
//	           sooner, a retry mints anew and writes again
//	runs       none: a run parked on an answer waits days, and its record is
//	           the only thing that knows a billed box exists

//	secrets    none: an expiring credential is an outage on a timer
//	integrations
//	           none, status and findings alike: an expired status
//	           re-provisions a surface the loop had already agreed with
//	atlasSources
//	           none: an expired standing reads as a source never read, so the
//	           collect duty would read everything at once
//	atlasRefreshes
//	           none: an age would drop an ask the collect duty had not reached
//	atlasCode  none: an expired one reads as a repository never scanned
//	mailboxes  none: an age cannot tell a present seat from a removed one, and
//	           would forget a mailbox still retaining mail

//	objects    none: an expired record lets a node record another backend,
//	           splitting the company's files between two stores
//	custody    thirty-two days, a day past the event log's retention: it must
//	           outlive every row it can be asked about
//	uploads    thirty hours, the longest hold a reservation or a seat's save
//	           ledger entry keeps, so what lapsed goes with no sweep
//	positions  none: an expired position reads as a node that applied NOTHING,
//	           which pins the trim or lets it delete what that node still needs
//
// Putting two of those in one bucket would give one of them the other's
// retention, and every such mistake is silent.

// # The activation epoch IS the pointer's revision
//
// JetStream assigns each key write a monotonic revision, so publishing the
// pointer appends and flips in ONE write: no node can read an epoch whose
// revision is unpublished, and two nodes activating at once get two
// revisions rather than racing over a counter this engine keeps.

const (
	rateSuffix         = "_rate"
	windowsSuffix      = "_counter_windows"
	claimsSuffix       = "_claims"
	ledgerSuffix       = "_ledger"
	statusSuffix       = "_status"
	configSuffix       = "_config"
	budgetSuffix       = "_token_windows"
	channelSuffix      = "_channels"
	followsSuffix      = "_follows"
	chatReadsSuffix    = "_chat_reads"
	firesSuffix        = "_fires"
	rebasesSuffix      = "_rebases"
	runsSuffix         = "_sandbox_runs"
	secretsSuffix      = "_secrets"
	integrationsSuffix = "_integrations"
	atlasSourcesSuffix = "_atlas_sources"
	atlasCodeSuffix    = "_atlas_code_scans"
	atlasRefreshSuffix = "_atlas_source_refreshes"
	mailboxesSuffix    = "_mailboxes"
	objectsSuffix      = "_objects"
	custodySuffix      = "_custody"
	uploadsSuffix      = "_uploads"
	nodeStatusSuffix   = "_node_status"
	// activationKey holds the pointer, which names the current revision's
	// sealed body under [coord.BodyClass] — see [coord.ActivationRecord].
	activationKey = "activation"
)

// FleetConfig is what a [FleetStore] needs at construction. Every duration is
// a BUCKET's retention; see "Why TWENTY-FOUR buckets" for why each has its own.
type FleetConfig struct {
	// BucketPrefix names every bucket. Empty means "crewlet", matching the
	// lease store; two companies on one NATS account take different prefixes.
	BucketPrefix string

	// MaxRateWindow is the widest window a rate caller counts over
	// ([coord.MaxRateWindow] in every deployment).
	MaxRateWindow time.Duration

	// MaxClaimTTL is the longest claim the claims bucket holds, and so its
	// age ([coord.MaxClaimTTL] in every deployment). Each claim lapses at the
	// deadline its record carries; the age only reaps the lapsed.
	MaxClaimTTL time.Duration

	// LedgerRetention is how long a turn completion is remembered.
	LedgerRetention time.Duration

	// FireRetention is how long a scheduled fire stays claimed: sized from
	// the catchup ceiling like LedgerRetention, but not the same number.
	FireRetention time.Duration

	// FollowRetention is how long a chat thread-follow survives with no
	// activity; every re-assert rewrites the record, so the age is that stamp.
	FollowRetention time.Duration

	// RebaseRetention is how long a unit of work's recorded rebase is kept
	// — see [coord.RebaseRetention], which is what every deployment passes.
	RebaseRetention time.Duration

	// BudgetRetention is how long a token counter outlives its last write: past
	// the longest calendar window, or a month's early charge is forgotten while
	// the month still counts it ([coord.BudgetRetention]).
	BudgetRetention time.Duration

	// StatusFreshness is how long a node's apply status, and its status
	// record, count as current: the age of both buckets.
	StatusFreshness time.Duration

	// CustodyRetention is how long the record of which data node keeps a
	// stateless node's batch is kept — see [coord.CustodyRetention].
	CustodyRetention time.Duration

	// UploadRetention is how long an upload reservation's records outlive
	// their last write. Zero takes [coord.UploadRetention]: shorter than the
	// longest hold forgets a hold on bytes still in the object store.
	UploadRetention time.Duration

	// Replicas is the JetStream replica count for every bucket.
	Replicas int

	// Clustered is whether this node's broker has PEERS, which the
	// provisioning budgets branch on ([jsprovision.Clustered]). Not inferred
	// from Replicas: a member with peers at one replica still waits on the
	// metadata group.
	Clustered bool

	// Reach is how this node's connection reaches the buckets, in process or
	// across a link, which decides how long a write's bytes are given to
	// cross ([Reach]). Required.
	Reach Reach
}

// rateBucketFactor is how many windows the rate bucket keeps: the current one,
// the one just closed (a straggler may still increment it) and one of slack.
// Lower risks reaping a live window, which resets a seat's count and lets it
// emit its whole allowance again.
const rateBucketFactor = 3

func (c *FleetConfig) normalize() error {
	if err := checkReach("FleetConfig", c.Reach); err != nil {
		return err
	}
	if c.BucketPrefix == "" {
		c.BucketPrefix = defaultBucketPrefix
	}
	if c.Replicas == 0 {
		c.Replicas = 1
	}
	if c.UploadRetention == 0 {
		c.UploadRetention = coord.UploadRetention
	}
	required := []struct {
		name  string
		value time.Duration
	}{
		{"MaxRateWindow", c.MaxRateWindow}, {"MaxClaimTTL", c.MaxClaimTTL},
		{"LedgerRetention", c.LedgerRetention}, {"FireRetention", c.FireRetention},
		{"FollowRetention", c.FollowRetention},
		{"RebaseRetention", c.RebaseRetention},
		{"BudgetRetention", c.BudgetRetention},
		{"StatusFreshness", c.StatusFreshness},
		{"CustodyRetention", c.CustodyRetention},
		{"UploadRetention", c.UploadRetention},
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
	windows      jetstream.KeyValue
	claims       jetstream.KeyValue
	ledger       jetstream.KeyValue
	status       jetstream.KeyValue
	config       jetstream.KeyValue
	budgets      jetstream.KeyValue
	channels     jetstream.KeyValue
	follows      jetstream.KeyValue
	chatReads    jetstream.KeyValue
	secrets      jetstream.KeyValue
	fires        jetstream.KeyValue
	rebases      jetstream.KeyValue
	runs         jetstream.KeyValue
	integrations jetstream.KeyValue
	atlasSources jetstream.KeyValue
	atlasCode    jetstream.KeyValue
	atlasRefresh jetstream.KeyValue
	mailboxes    jetstream.KeyValue
	objects      jetstream.KeyValue
	custody      jetstream.KeyValue
	uploads      jetstream.KeyValue
	nodeStatus   jetstream.KeyValue

	// positions is the register the ageless key classes share (log
	// positions, trim holds, backup points, published floors, capacity
	// operations, admissions, maintenance acks, seat pauses): none may ever
	// expire, since each decides what somebody ELSE may delete, publish or
	// run, and every listing over it filters by class. Chat read state stays
	// out, in chatReads, so the busiest record never shares this register's
	// raft group and leader with the trim (coord's chatread.go).
	positions jetstream.KeyValue

	// js is held for what a bucket handle cannot do: read the stream's key
	// index and a key as its LEADER holds it (package doc, "A pass cannot
	// tell that it is complete, so it is certified"), and purge a marker
	// through its own revision (markers.go).
	js jetstream.JetStream

	// reach ([FleetConfig.Reach]) paces every bucket's writes and a caller's
	// budget for a run's value ([FleetStore.ValueCrossing]).
	reach Reach

	// now is the clock an activation's flip is bounded by ([flipBy]): the
	// wall clock, which a case replaces to name the instant exactly.
	now func() time.Time

	// ageless is every bucket the broker never ages, whose removal markers
	// stay until [FleetStore.SweepMarkers]. DERIVED from each bucket's
	// retention, so a new ageless bucket is swept without being listed.
	ageless []jetstream.KeyValue

	maxRateWindow time.Duration
	freshness     time.Duration

	// maxClaim is the longest claim [FleetStore.Claim] accepts: the claims
	// bucket's age IN FORCE, never this node's config. Zero is a bucket with
	// no age, which reaps nothing and so caps nothing.
	maxClaim time.Duration

	// The read-modify-write records, each COALESCED PER KEY ([writeLines]):
	// one compare-and-swap decides every write this node has in flight on a
	// key, so its contenders are nodes, not calls (casline.go).
	tallies  writeLines[coord.Tally]
	rates    writeLines[rateRecord]
	windowW  writeLines[rateRecord]
	channelW writeLines[coord.Channel]

	// Node-wide round-trip estimates ([roundTrip]) of the buckets whose
	// compare-and-swap loops are not coalesced ([casRetry]); positionTrips is
	// the positions register's (feed marks, MCP checks and restarts).
	claimTrips, custodyTrips, objectTrips, followTrips, runTrips, chatReadTrips roundTrip
	positionTrips, nodeStatusTrips                                              roundTrip
}

var _ coord.Fleet = (*FleetStore)(nil)

// OpenFleet creates or adopts every bucket and returns the backend.
//
// Idempotent and safe to call from every node at once, like [Open]: an
// existing bucket is ADOPTED, never rewritten ([openBucket]).
//
// The whole sequence runs under one [jsprovision.SequenceBudget]: each bucket
// takes its own provisioning budget, so without a ceiling a wedged cluster is
// rediscovered once per bucket and the bound is their product.
func OpenFleet(ctx context.Context, js jetstream.JetStream, cfg FleetConfig) (*FleetStore, error) {
	if js == nil {
		return nil, errNoJetStream
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx,
		jsprovision.Clustered(cfg.Clustered).SequenceBudget())
	defer cancel()

	open := func(suffix, describe string, ttl time.Duration, h holding) (jetstream.KeyValue, bucketFacts, error) {
		name := cfg.BucketPrefix + suffix
		bucket, facts, err := openBucket(ctx, js, cfg.Clustered, jetstream.KeyValueConfig{
			Bucket: name, Description: describe, TTL: ttl, Replicas: cfg.Replicas,
		})
		if err != nil {
			return nil, bucketFacts{}, fmt.Errorf("coord/kv: open %s: %w", name, err)
		}
		// EVERY BUCKET reports its retention ([reportRetention]); the
		// leases bucket reports its own in [Open], where the age is also
		// arithmetic.
		reportRetention(ctx, name, ttl, facts)
		// EVERY CONDITIONAL WRITE TO IT IS DECIDED BY THE LEADER — see
		// settle.go for the third answer a compare-and-set can get.
		return settled(js, bucket, cfg.Reach, h), facts, nil
	}

	store := &FleetStore{
		js: js, reach: cfg.Reach, now: time.Now,
		maxRateWindow: cfg.MaxRateWindow, freshness: cfg.StatusFreshness,
	}
	store.tallies.codec = codec[coord.Tally]{
		decode: decodeTally, encode: encodeTally,
		reading: "read the token counter", decoding: "decode the token counter",
	}
	store.rates.codec = codec[rateRecord]{
		decode: decodeRate, encode: encodeRate,
		reading: "read the rate window", decoding: "decode the rate window",
	}
	store.windowW.codec = store.rates.codec
	store.channelW.codec = codec[coord.Channel]{
		decode: decodeChannelRecord, encode: encodeChannel,
		reading: "read the channel", decoding: "decode the channel",
	}
	var claimFacts, rateFacts bucketFacts
	for _, bucket := range []struct {
		into     *jetstream.KeyValue
		suffix   string
		describe string
		ttl      time.Duration
		// holding is the bucket's key classes ([holding]).
		holding holding
	}{
		{&store.rate, rateSuffix,
			"Crewlet notification-valve windows; the bucket TTL reaps a closed window",
			cfg.MaxRateWindow * rateBucketFactor, counterHolding},
		{&store.windows, windowsSuffix,
			"Crewlet counter windows up to a day wide; the bucket TTL reaps a closed window",
			coord.MaxCounterWindow * rateBucketFactor, counterHolding},
		{&store.claims, claimsSuffix,
			"Crewlet inbound-delivery claims; each record carries its own deadline, and the " +
				"bucket age is the longest claim's",
			cfg.MaxClaimTTL, claimHolding},
		{&store.ledger, ledgerSuffix,
			"Crewlet turn completions; the bucket TTL is the retention horizon",
			cfg.LedgerRetention, ledgerHolding},
		{&store.status, statusSuffix,
			"Crewlet per-node config-apply status; a node that stops reporting ages out",
			cfg.StatusFreshness, applyStatusHolding},
		{&store.nodeStatus, nodeStatusSuffix,
			"Crewlet per-node status records (MCP, models, runners, infra); a node that stops writing ages out",
			cfg.StatusFreshness, nodeStatusHolding},
		{&store.config, configSuffix,
			"Crewlet activation pointer and the bodies it names; NO TTL — the pointer's revision IS the epoch", 0, configHolding},
		{&store.budgets, budgetSuffix,
			"Crewlet token counters, a slot per calendar window; the bucket TTL outlasts the longest window",
			cfg.BudgetRetention, counterHolding},
		{&store.channels, channelSuffix,
			"Crewlet agent-to-agent channels; NO TTL — an open ask must outlive any clock", 0, chatterHolding},
		{&store.follows, followsSuffix,
			"Crewlet chat thread-follows; the bucket TTL is the last-activity horizon",
			cfg.FollowRetention, chatterHolding},
		{&store.chatReads, chatReadsSuffix,
			"Crewlet per-person chat read state; NO TTL — an expired cursor marks a room unread",
			0, chatReadsHolding},
		{&store.fires, firesSuffix,
			"Crewlet scheduled-fire claims; the bucket TTL outlasts the catchup ceiling",
			cfg.FireRetention, claimHolding},
		{&store.rebases, rebasesSuffix,
			"Crewlet operation-id rebases; the bucket TTL outlasts the horizon a rebase is inherited within",
			cfg.RebaseRetention, claimHolding},
		{&store.runs, runsSuffix,
			"Crewlet detached sandbox runs; NO TTL — a parked run's box outlives any clock", 0, runsHolding},
		{&store.secrets, secretsSuffix,
			"Crewlet sealed credentials; NO TTL: an expiring secret is an outage on a timer", 0, secretsHolding},
		{&store.integrations, integrationsSuffix,
			"Crewlet integration reconcile status; NO TTL, standing state rather than a short horizon", 0,
			integrationsHolding},
		{&store.atlasSources, atlasSourcesSuffix,
			"Crewlet atlas source standings; NO TTL, an expired one reads as a source never read", 0, atlasSourcesHolding},
		{&store.atlasCode, atlasCodeSuffix,
			"Crewlet atlas code scan standings; NO TTL, an expired one reads as a repository never scanned", 0, atlasSmallHolding},
		{&store.atlasRefresh, atlasRefreshSuffix,
			"Crewlet atlas source refresh asks; NO TTL, the collect duty drops one once it is honoured", 0, atlasSmallHolding},
		{&store.mailboxes, mailboxesSuffix,
			"Crewlet seat mailbox registry; NO TTL, a record's age cannot tell a present seat from a removed one", 0, chatterHolding},
		{&store.objects, objectsSuffix,
			"Crewlet object store backend; NO TTL — an expired record lets a node record another backend", 0, objectsHolding},
		{&store.custody, custodySuffix,
			"Crewlet stateless-node event custody; the bucket TTL outlasts the event log's retention",
			cfg.CustodyRetention, claimHolding},
		{&store.uploads, uploadsSuffix,
			"Crewlet artifact upload reservations and seat save ledgers; the bucket TTL outlasts the longest hold either keeps",
			cfg.UploadRetention, uploadsHolding},
		{&store.positions, positionsSuffix,
			"Crewlet per-node state-log positions; NO TTL — an expired position reads as a node that applied nothing", 0, positionsHolding},
	} {
		got, facts, err := open(bucket.suffix, bucket.describe, bucket.ttl, bucket.holding)
		if err != nil {
			return nil, err
		}
		*bucket.into = got
		switch bucket.into {
		case &store.claims:
			claimFacts = facts
		case &store.rate:
			rateFacts = facts
		}
		if bucket.ttl == 0 {
			store.ageless = append(store.ageless, got)
		}
	}

	// THE CLAIMS BUCKET'S AGE IS A CEILING: a claim longer than the age IN
	// FORCE (read back by openBucket) would be reaped live and its delivery
	// replayed, so Claim refuses one ([coord.ErrTTLTooLong]).
	store.maxClaim = claimFacts.age
	// So is the rate bucket's: one created with a shorter age keeps it, so a
	// wider window is refused rather than reaped mid-window.
	store.maxRateWindow = min(cfg.MaxRateWindow, rateFacts.age/rateBucketFactor)

	log.DebugContext(ctx, "coord_kv_fleet_open", "prefix", cfg.BucketPrefix,
		"max_rate_window", cfg.MaxRateWindow, "max_claim_ttl", store.maxClaim,
		"ledger_retention", cfg.LedgerRetention,
		"budget_retention", cfg.BudgetRetention,
		"status_freshness", cfg.StatusFreshness)
	return store, nil
}

// each walks a whole bucket through [eachEntry]; the method exists so a call
// site reads as a walk rather than connection plumbing.
func (f *FleetStore) each(ctx context.Context, kv jetstream.KeyValue,
	visit func(jetstream.KeyValueEntry) error) error {

	return eachEntry(ctx, f.js, kv, visit)
}

// get reads one key as its stream LEADER holds it ([getLatest]); kv.go says
// why never the bucket handle's direct get.
func (f *FleetStore) get(ctx context.Context, kv jetstream.KeyValue,
	key string) (jetstream.KeyValueEntry, error) {

	return getLatest(ctx, f.js, kv, key)
}

// create writes a key that must not hold a live value ([createKey]);
// markers.go says why not the bucket handle's own create.
func (f *FleetStore) create(ctx context.Context, kv jetstream.KeyValue, key string, value []byte) (uint64, error) {
	return createKey(ctx, f.js, kv, key, value, nil)
}

// eachUnder is [each] narrowed to the keys matching one filter, `what` naming
// the listing a failure could not complete ([eachEntryUnder]).
func (f *FleetStore) eachUnder(ctx context.Context, kv jetstream.KeyValue, keys, what string,
	visit func(jetstream.KeyValueEntry) error) error {

	return eachEntryUnder(ctx, f.js, kv, keys, what, visit)
}

// ---- the rate valve ---------------------------------------------------- //

// rateRecord is one window's count.
type rateRecord struct {
	Count int `json:"count"`
}

// Allow increments a bucket's window and reports whether it stayed in limit.
//
// The WINDOW IS IN THE KEY, so a new window is a new record and the old one
// ages out, with nothing to reset or sweep. The increment is a compare-and-swap,
// so concurrent nodes add up, and this node's increments of one window are
// COALESCED into one contender ([writeLines]).
//
// CONTENTION IS NEVER AN ERROR: every write here adds one, so each lost
// attempt is another node's increment, and a write reads the window full
// after at most limit losses. Hence limit+1 attempts where that exceeds the
// shared bound ([writeLines.writeWithin]): a fixed count below the limit
// answers "could not say", which the notification path fails open on.
func (f *FleetStore) Allow(ctx context.Context, bucket string, limit int, window time.Duration, now time.Time) (bool, error) {
	if bucket == "" {
		return false, errors.New("coord/kv: a rate bucket needs a name")
	}
	if limit <= 0 || window <= 0 {
		return false, nil
	}
	// THE BUCKET WHOSE AGE OUTLASTS THE WINDOW (see "Why TWENTY-FOUR buckets"):
	// one reaped before it closed restarts its count mid-window, so a longer
	// window is refused loudly.
	kv, lines := f.rate, &f.rates
	switch {
	case window <= f.maxRateWindow:
	case window <= coord.MaxCounterWindow:
		kv, lines = f.windows, &f.windowW
	default:
		return false, fmt.Errorf("coord/kv: a counter window of %v is longer than the %v the counter "+
			"windows bucket keeps (coord.MaxCounterWindow)", window, coord.MaxCounterWindow)
	}
	key := counterKey(bucket, window, now)

	// A CREATE where the window has no record, so two first callers cannot
	// both write "1"; a lost create is a lost compare-and-swap like any other.
	var allowed bool
	_, err := lines.writeWithin(ctx, kv, f.js, key, bucket, "increment the rate window", limit+1,
		func(stored rateRecord, _ bool) (rateRecord, bool) {
			allowed = stored.Count < limit
			if !allowed {
				return stored, false
			}
			return rateRecord{Count: stored.Count + 1}, true
		})
	if err != nil {
		// Unreachable, or past every attempt (only a record aged out from
		// under a caller reaches that). Unknown, never "allowed": what an
		// error means is the caller's (the notification path fails open).
		return false, err
	}
	return allowed, nil
}

// counterKey is the record of the window of width window holding now, in the
// counter named bucket: the name, the width and the start, so one name counted
// at two widths keeps two counts.
func counterKey(bucket string, window time.Duration, now time.Time) string {
	return encodeKey(bucket + "|" + strconv.FormatInt(int64(window), 10) + "|" +
		strconv.FormatInt(now.Truncate(window).UnixNano(), 10))
}

func encodeRate(r rateRecord) ([]byte, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: encode the rate window: %w", err)
	}
	return raw, nil
}

func decodeRate(raw []byte) (rateRecord, error) {
	var record rateRecord
	err := json.Unmarshal(raw, &record)
	return record, err
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
	// REFUSED, never clamped: the bucket keeps a record for its age and no
	// longer, so a longer claim would lapse early and let through the very
	// duplicate it was sized to stop.
	if f.maxClaim > 0 && ttl > f.maxClaim {
		return false, fmt.Errorf("coord/kv: a %v claim exceeds the claims bucket's age %v: %w",
			ttl, f.maxClaim, coord.ErrTTLTooLong)
	}
	encoded := encodeKey(key)
	value := mustEncodeClaim(claimRecord{At: now.UTC(), Until: now.Add(ttl).UTC()})
	// A ROUND REPEATS ONLY WHEN THE KEY MOVED between this caller's create and
	// its read or take-over (reaped, released, or taken over by another
	// receiver), so rounds stay far below the shared bound ([fleetCASAttempts]).
	var first bool
	settled, err := casRetry(ctx, &f.claimTrips, "claim the delivery", func() (bool, error) {
		// Create fails when the key exists, so the FIRST caller wins.
		_, err := f.create(ctx, f.claims, encoded, value)
		if err == nil {
			first = true
			return true, nil
		}
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return false, unavailable("claim the delivery", err)
		}
		// A KEY THAT EXISTS IS NOT YET A CLAIM THAT HOLDS: the bucket's age is
		// the longest claim's, so each lapses at the deadline its record carries.
		entry, err := f.get(ctx, f.claims, encoded)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// Reaped or released since the create lost: ask again.
			return false, nil
		case err != nil:
			return false, unavailable("read the delivery claim", err)
		}
		if decodeClaim(entry.Value()).Until.After(now) {
			return true, nil
		}
		// LAPSED, or naming no deadline (the contract fails open): taken over
		// at the revision just read, so two callers cannot both win it.
		_, err = f.claims.Update(ctx, encoded, value, entry.Revision())
		switch {
		case err == nil:
			first = true
			return true, nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			return false, nil
		default:
			return false, unavailable("take over the lapsed delivery claim", err)
		}
	})
	switch {
	case err != nil:
		return false, err
	case !settled:
		// Every round was won by somebody else: unknown, not "claimed" (the
		// contract fails open on an error).
		return false, contended("claim", key)
	}
	return first, nil
}

// claimRecord is one delivery claim: when it was taken and when it lapses.
type claimRecord struct {
	At    time.Time `json:"at"`
	Until time.Time `json:"until"`
}

// decodeClaim reads a claim record: the zero record, which has lapsed, for a
// value that names no deadline.
func decodeClaim(raw []byte) claimRecord {
	var record claimRecord
	if json.Unmarshal(raw, &record) != nil {
		return claimRecord{}
	}
	return record
}

func mustEncodeClaim(record claimRecord) []byte {
	// Two times cannot fail to encode.
	raw, _ := json.Marshal(record)
	return raw
}

// Release drops a claim.
func (f *FleetStore) Release(ctx context.Context, key string) error {
	if key == "" {
		return nil
	}
	// Purged: the marker ages out with the bucket and createKey writes over
	// it, so a released claim is re-claimable at once.
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
		_, err := f.get(ctx, f.ledger, ledgerKey(scope, key))
		switch {
		case err == nil:
			out[key] = true
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// Not worked. The ordinary answer.
		default:
			// RAISED though the caller fails open: running anyway is its
			// decision, and its log line says why a turn ran twice.
			return nil, unavailable("read the completion ledger", err)
		}
	}
	return out, nil
}

// WorkedAt returns the keys already recorded under scope, each with the
// instant its first record stated.
func (f *FleetStore) WorkedAt(ctx context.Context, scope string, keys []string) (map[string]time.Time, error) {
	out := make(map[string]time.Time, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		entry, err := f.get(ctx, f.ledger, ledgerKey(scope, key))
		switch {
		case err == nil:
			// A RECORD THAT DOES NOT DECODE IS STILL WORKED, at no
			// instant anybody can state.
			var record ledgerRecord
			_ = json.Unmarshal(entry.Value(), &record)
			out[key] = record.At
		case errors.Is(err, jetstream.ErrKeyNotFound):
		default:
			return nil, unavailable("read the completion ledger", err)
		}
	}
	return out, nil
}

// WorkedBy returns the keys already recorded under scope, each with the detail
// its first record stated.
func (f *FleetStore) WorkedBy(ctx context.Context, scope string, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		entry, err := f.get(ctx, f.ledger, ledgerKey(scope, key))
		switch {
		case err == nil:
			// A RECORD THAT DOES NOT DECODE IS STILL WORKED, by nobody it names.
			var record ledgerRecord
			_ = json.Unmarshal(entry.Value(), &record)
			out[key] = record.Detail
		case errors.Is(err, jetstream.ErrKeyNotFound):
		default:
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
	if err := coord.CheckLedgerDetail(detail); err != nil {
		return err
	}
	raw, err := json.Marshal(ledgerRecord{Detail: detail, At: at.UTC()})
	if err != nil {
		return fmt.Errorf("coord/kv: encode the ledger entry: %w", err)
	}
	// FIRST WRITER WINS, and losing is not a failure: two nodes completing
	// one trigger is the case the ledger exists to collapse.
	if _, err := f.create(ctx, f.ledger, ledgerKey(scope, key), raw); err != nil &&
		!errors.Is(err, jetstream.ErrKeyExists) {
		return unavailable("record the completion", err)
	}
	return nil
}

func ledgerKey(scope, key string) string { return encodeKey(scope + "|" + key) }

// ---- the token counters ------------------------------------------------ //

// tallyRecord is one scope's counter as the bucket stores it: a slot per
// period, keyed by the period's own spelling, and when the counter last moved.
//
// KEYED BY PERIOD, so a period this build does not know is skipped rather than
// misplaced. A build DROPS fields it does not know when it rewrites the record,
// so an added field must be one whose loss is harmless, as RefusedAt's is: a
// dashboard stamp the next refusal writes again.
type tallyRecord struct {
	Slots map[period.Period]slotRecord `json:"slots"`
	At    time.Time                    `json:"at,omitzero"`
}

type slotRecord struct {
	Label     string    `json:"label"`
	Used      int       `json:"used"`
	RefusedAt time.Time `json:"refused_at,omitzero"`
}

func encodeTally(t coord.Tally) ([]byte, error) {
	record := tallyRecord{Slots: make(map[period.Period]slotRecord, len(t.Slots)), At: t.At}
	for i, p := range period.Periods {
		if s := t.Slots[i]; s.Label != "" {
			record.Slots[p] = slotRecord{Label: s.Label, Used: s.Used, RefusedAt: s.RefusedAt}
		}
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: encode the token counter: %w", err)
	}
	return raw, nil
}

func decodeTally(raw []byte) (coord.Tally, error) {
	var record tallyRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return coord.Tally{}, err
	}
	t := coord.Tally{At: record.At}
	for i, p := range period.Periods {
		if s, ok := record.Slots[p]; ok {
			t.Slots[i] = coord.Slot{Label: s.Label, Used: s.Used, RefusedAt: s.RefusedAt}
		}
	}
	return t, nil
}

// Charge records a round in the org's counter and the seat's, in every
// window, and judges whether it fitted. See [coord.Budgets.Charge].
//
// No transaction, so the order is BUILT: org, then seat, each one CAS that
// rolls, counts and stamps refusals in one write, never taken back.
//
// The org's write is on the caller's context, since nothing precedes it (a
// caller that must count regardless passes one that outlives it, as the
// engine's meter does). The rest, the charge half done, runs on one that
// outlives the caller's, so only a write that itself fails leaves the partial
// ([coord.SeatUncountedError]). It cannot hang: a deadline-free request is
// bounded by the API timeout, and only lost races retry ([fleetCASAttempts]).
func (f *FleetStore) Charge(ctx context.Context, req coord.ChargeRequest) (coord.Spend, error) {
	if req.Tokens <= 0 {
		// Not an error and not a charge: refusing would stop a company over
		// a backend that omits usage.
		return coord.Spend{OK: true}, nil
	}
	if err := req.Validate(); err != nil {
		return coord.Spend{}, fmt.Errorf("coord/kv: %w", err)
	}

	org, refusing, err := f.count(ctx, coord.OrgScope, req.Tokens, req.Windows, req.OrgCaps, false)
	if err != nil {
		return coord.Spend{}, err
	}
	// The company's write has landed; see the doc for this context.
	landed := context.WithoutCancel(ctx)
	if len(refusing) > 0 {
		// THE COMPANY REFUSED; the round is the seat's all the same, counted
		// with no verdict so its stamps stay (coord.WindowUsage.RefusedAt).
		// A failed seat write is LOGGED, never returned or unwound: the
		// refusal is decided, an error would report an outage for a company
		// out of budget, and the company's record is true.
		refused := org.Refusal("org", refusing, req.OrgCaps, req.Windows)
		refused.Org = org.Usage(coord.OrgScope, req.Windows)
		seat, _, seatErr := f.count(landed, req.Seat, req.Tokens, req.Windows, nil, false)
		if seatErr != nil {
			log.WarnContext(ctx, "coord_kv_budget_spend_uncounted", "scope", req.Seat,
				"tokens", req.Tokens, "error", seatErr,
				"detail", "the company refused this round and counted it; the seat's own "+
					"counter understates its spend by the round")
			return refused, nil
		}
		refused.Agent = seat.Usage(req.Seat, req.Windows)
		return refused, nil
	}

	seat, refusing, err := f.count(landed, req.Seat, req.Tokens, req.Windows, req.SeatCaps, true)
	if err != nil {
		// THE COMPANY KEEPS THE ROUND: it was billed before it was charged,
		// so the company's record is true. The seat's verdict is unknown, so
		// the round stops, with an error naming the partial
		// (coord.SeatUncountedError) rather than a charge that recorded nothing.
		log.WarnContext(ctx, "coord_kv_budget_spend_uncounted", "scope", req.Seat,
			"tokens", req.Tokens, "error", err,
			"detail", "the company counted this round and the seat's write failed; the "+
				"seat's own counter understates its spend by the round")
		return coord.Spend{}, &coord.SeatUncountedError{Seat: req.Seat, Tokens: req.Tokens, Err: err}
	}
	if len(refusing) > 0 {
		// Refused by the seat. The company's half and its stamps stay (the
		// refusal is not the company's to clear), and both counters travel
		// with the refusal (coord.Spend.Org).
		refused := seat.Refusal("agent", refusing, req.SeatCaps, req.Windows)
		refused.Org, refused.Agent = org.Usage(coord.OrgScope, req.Windows), seat.Usage(req.Seat, req.Windows)
		return refused, nil
	}
	// ADMITTED, so the company had room too. Its write kept its stamps, since
	// it ran before the seat was judged; the seat's own write cleared its.
	if org.Refused() {
		f.clearRefusal(ctx, coord.OrgScope, org)
	}
	return coord.Spend{
		OK:    true,
		Org:   org.ClearAll().Usage(coord.OrgScope, req.Windows),
		Agent: seat.Usage(req.Seat, req.Windows),
	}, nil
}

// PostCharge adds spend that already happened to the org's counter and the
// seat's, refusing nothing. See [coord.Budgets.PostCharge].
//
// [FleetStore.Charge]'s two writes, context and partial, with no ceiling
// judged and no refusal stamp touched.
func (f *FleetStore) PostCharge(ctx context.Context, seat string, tokens int, windows coord.Windows) (coord.Spend, error) {
	if tokens <= 0 {
		return coord.Spend{OK: true}, nil
	}
	if seat == "" {
		return coord.Spend{}, errors.New("coord/kv: a charge needs a seat scope")
	}
	if err := windows.Validate(); err != nil {
		return coord.Spend{}, fmt.Errorf("coord/kv: %w", err)
	}
	org, _, err := f.count(ctx, coord.OrgScope, tokens, windows, nil, false)
	if err != nil {
		return coord.Spend{}, err
	}
	agent, _, err := f.count(context.WithoutCancel(ctx), seat, tokens, windows, nil, false)
	if err != nil {
		// THE COMPANY KEEPS IT, for Charge's reason; a caller that retries
		// finishes the seat's half alone (PostChargeSeat).
		log.WarnContext(ctx, "coord_kv_budget_spend_uncounted", "scope", seat,
			"tokens", tokens, "error", err,
			"detail", "the company counted this post-charge and the seat's write failed; "+
				"the seat's own counter understates its spend by it")
		return coord.Spend{}, &coord.SeatUncountedError{Seat: seat, Tokens: tokens, Err: err}
	}
	return coord.Spend{
		OK: true, Org: org.Usage(coord.OrgScope, windows), Agent: agent.Usage(seat, windows),
	}, nil
}

// PostChargeSeat adds spend that already happened to one seat's counter alone,
// refusing nothing. See [coord.Budgets.PostChargeSeat].
//
// ONE write, so it can leave no partial of its own, and a count with no caps
// carries the seat's refusal stamps through untouched.
func (f *FleetStore) PostChargeSeat(ctx context.Context, seat string, tokens int, windows coord.Windows) (coord.Usage, error) {
	if tokens <= 0 {
		return coord.Usage{}, nil
	}
	if seat == "" {
		return coord.Usage{}, errors.New("coord/kv: a charge needs a seat scope")
	}
	if err := windows.Validate(); err != nil {
		return coord.Usage{}, fmt.Errorf("coord/kv: %w", err)
	}
	agent, _, err := f.count(ctx, seat, tokens, windows, nil, false)
	if err != nil {
		return coord.Usage{}, err
	}
	return agent.Usage(seat, windows), nil
}

// PostChargeOrg adds spend that already happened to the company's counter
// alone, refusing nothing. See [coord.Budgets.PostChargeOrg].
//
// ONE write, so it can leave no partial, and a count with no caps carries the
// company's refusal stamps through untouched.
func (f *FleetStore) PostChargeOrg(ctx context.Context, tokens int, windows coord.Windows) (coord.Usage, error) {
	if tokens <= 0 {
		return coord.Usage{}, nil
	}
	if err := windows.Validate(); err != nil {
		return coord.Usage{}, fmt.Errorf("coord/kv: %w", err)
	}
	org, _, err := f.count(ctx, coord.OrgScope, tokens, windows, nil, false)
	if err != nil {
		return coord.Usage{}, err
	}
	return org.Usage(coord.OrgScope, windows), nil
}

// Refuse stamps every full window of one scope with a refusal the gate made
// without a charge, counting nothing. See [coord.Budgets.Refuse].
//
// ONE compare-and-swap, judged on the counter rolled as it stands NOW, never
// as the caller saw it, so a rolled-over window or one with room is not
// stamped and a concurrent stamp is raced at the broker, not overwritten. A
// counter with nothing to stamp is not written, so a refusal never creates a
// record or persists a roll. On the caller's context: one write, nothing half
// done (a caller that must record regardless passes one that outlives it).
func (f *FleetStore) Refuse(ctx context.Context, scope string, caps coord.Caps, windows coord.Windows) (coord.Usage, error) {
	if scope == "" {
		return coord.Usage{}, errors.New("coord/kv: a refusal needs a scope")
	}
	if err := windows.Validate(); err != nil {
		return coord.Usage{}, fmt.Errorf("coord/kv: %w", err)
	}
	if err := caps.Validate(); err != nil {
		return coord.Usage{}, fmt.Errorf("coord/kv: %w", err)
	}
	tally, err := f.casTally(ctx, scope, "record the budget refusal",
		func(stored coord.Tally, _ bool) (coord.Tally, bool) {
			stamped, full := stored.Refuse(caps, windows, time.Now().UTC())
			return stamped, len(full) > 0
		})
	if err != nil {
		return coord.Usage{}, err
	}
	return tally.Usage(scope, windows), nil
}

// clearRefusal drops the refusals an admitted charge found on a scope.
//
// ONLY THE STAMPS IT SAW: a newer stamp from another refused caller is still
// true. A failure is logged, not returned: the stamp is what a dashboard
// reads, not what the gate decides with. On a context that OUTLIVES the
// caller's, like the seat's write ([FleetStore.Charge]): the charge is a fact
// by now, and a clear lost to a cancelled caller leaves the scope showing a
// refusal it has just admitted past.
func (f *FleetStore) clearRefusal(ctx context.Context, scope string, seen coord.Tally) {
	_, err := f.casTally(context.WithoutCancel(ctx), scope, "clear the budget refusal",
		func(stored coord.Tally, found bool) (coord.Tally, bool) {
			// Gone means aged out, which cleared it; unchanged means
			// already cleared or stamped again since.
			cleared := stored.Clear(seen)
			return cleared, found && cleared != stored
		})
	if err != nil {
		log.WarnContext(ctx, "coord_kv_budget_refusal_not_cleared", "scope", scope, "error", err,
			"detail", "the charge was admitted; the live meter keeps showing the old "+
				"refusal until the scope's next admitted charge clears it")
	}
}

// count is one scope's share of a charge under a compare-and-swap
// ([coord.Tally.Count]), answering the counter it wrote and the periods that
// refused. Nil caps judge nothing. clearOnAdmit clears the stamps of a scope
// that had room, which only the LAST scope a charge judges may do, inside its
// own write, since only it knows the whole charge was admitted
// ([FleetStore.Charge]).
func (f *FleetStore) count(ctx context.Context, scope string, tokens int, windows coord.Windows,
	caps coord.Caps, clearOnAdmit bool) (coord.Tally, []period.Period, error) {

	var refusing []period.Period
	tally, err := f.casTally(ctx, scope, "charge the token counter",
		func(stored coord.Tally, _ bool) (coord.Tally, bool) {
			var counted coord.Tally
			counted, refusing = stored.Count(tokens, caps, windows, time.Now().UTC())
			if clearOnAdmit && len(refusing) == 0 {
				counted = counted.ClearAll()
			}
			return counted, true
		})
	if err != nil {
		return coord.Tally{}, nil, err
	}
	return tally, refusing, nil
}

// casTally is one read-modify-write of a scope's counter under a
// compare-and-swap, retried when a peer wrote in between. change gets the
// stored counter (zero and false when absent) and answers what to write and
// whether; casTally answers what change last answered, written or not.
//
// COALESCED PER SCOPE ([writeLines]) because no key is hotter: every round,
// auxiliary call and executor launch of every seat writes the org's counter.
// A node's charges are decided in arrival order by one write, each as if
// alone, so the contenders are nodes, within [fleetCASAttempts]. Exhausted
// attempts are an ERROR, never a refusal: the round fails rather than telling
// an agent it is out of budget (fail closed).
func (f *FleetStore) casTally(ctx context.Context, scope, verb string,
	change func(stored coord.Tally, found bool) (coord.Tally, bool)) (coord.Tally, error) {

	return f.tallies.write(ctx, f.budgets, f.js, encodeKey(scope), scope, verb, change)
}

// tally reads one scope's counter rolled to windows, which is what it would
// be judged on by the charge that wrote next. A scope never charged is the
// zero Tally rolled, every window unspent.
func (f *FleetStore) tally(ctx context.Context, scope string, windows coord.Windows) (coord.Tally, error) {
	entry, err := f.get(ctx, f.budgets, encodeKey(scope))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return coord.Tally{}.Roll(windows), nil
	}
	if err != nil {
		return coord.Tally{}, unavailable("read the token counter", err)
	}
	stored, err := decodeTally(entry.Value())
	if err != nil {
		return coord.Tally{}, unavailable("decode the token counter", err)
	}
	return stored.Roll(windows), nil
}

// Used reports one scope's counter against the given windows.
func (f *FleetStore) Used(ctx context.Context, scope string, windows coord.Windows) (coord.Usage, error) {
	if scope == "" {
		return coord.Usage{}, errors.New("coord/kv: a budget scope is required")
	}
	if err := windows.Validate(); err != nil {
		return coord.Usage{}, fmt.Errorf("coord/kv: %w", err)
	}
	entry, err := f.get(ctx, f.budgets, encodeKey(scope))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return coord.Unspent(scope, windows), nil
	}
	if err != nil {
		return coord.Usage{}, unavailable("read the token counter", err)
	}
	stored, err := decodeTally(entry.Value())
	if err != nil {
		return coord.Usage{}, unavailable("decode the token counter", err)
	}
	return stored.Usage(scope, windows), nil
}

// Usage returns every counter against the given windows, org first then seats
// by scope.
func (f *FleetStore) Usage(ctx context.Context, windows coord.Windows) ([]coord.Usage, error) {
	if err := windows.Validate(); err != nil {
		return nil, fmt.Errorf("coord/kv: %w", err)
	}
	var out []coord.Usage
	err := f.each(ctx, f.budgets, func(kve jetstream.KeyValueEntry) error {
		stored, err := decodeTally(kve.Value())
		if err != nil {
			return unavailable("decode the token counter", err)
		}
		scope, ok := decodeKey(kve.Key())
		if !ok {
			// A key this backend did not write: skipped, as the lease
			// listing does, since an invented scope name is worse than none.
			return nil
		}
		out = append(out, stored.Usage(scope, windows))
		return nil
	})
	if err != nil {
		return nil, err
	}
	coord.SortUsage(out)
	return out, nil
}

// ---- the config plane -------------------------------------------------- //

// Activate publishes a new target revision: its body under its digest, then
// the pointer naming it ([coord.Plane.Activate]).
func (f *FleetStore) Activate(ctx context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	if req.RevisionID == "" {
		return coord.Activation{}, errors.New("coord/kv: an activation needs a revision id")
	}
	if err := req.Origin.Check(); err != nil {
		return coord.Activation{}, fmt.Errorf("coord/kv: activate %s: %w", req.RevisionID, err)
	}
	if req.Expect != "" && req.ExpectAbsent {
		return coord.Activation{}, errors.New("coord/kv: an activation cannot " +
			"expect a revision and no revision at once")
	}
	if req.ExpectEpoch != 0 && req.Expect == "" {
		return coord.Activation{}, errors.New("coord/kv: an activation cannot " +
			"expect an epoch without the revision it names")
	}
	payload, err := coord.CanonicalPayload(req.Payload)
	if err != nil {
		return coord.Activation{}, fmt.Errorf("coord/kv: %w", err)
	}
	if err = coord.HoldValue("the activation's body", len(payload), coord.MaxValueBytes); err != nil {
		return coord.Activation{}, err
	}
	size, err := coord.ActivationBytes(req)
	if err != nil {
		return coord.Activation{}, fmt.Errorf("coord/kv: %w", err)
	}
	if err = coord.HoldValue("the activation pointer", size, coord.MaxPointerBytes); err != nil {
		return coord.Activation{}, err
	}
	seq, previous, err := f.expectedSeq(ctx, req)
	if err != nil {
		return coord.Activation{}, err
	}
	// THE BODY FIRST, ALWAYS WRITTEN even where its key exists: its new
	// revision is what fences the sweep ([FleetStore.sweepBodies]), and the
	// flip below must land within half of coord.BodyRetention of it. The
	// instant is taken BEFORE the write, so the broker's stamp is no earlier.
	body := coord.BodyOf(payload)
	wrote := f.now()
	if _, err = f.config.Put(ctx, bodyKey(body), payload); err != nil {
		if errors.Is(err, coord.ErrValueTooLarge) {
			return coord.Activation{}, err
		}
		return coord.Activation{}, unavailable("publish the activation's body", err)
	}
	flipCtx, cancel := context.WithDeadline(ctx, wrote.Add(flipBy))
	defer cancel()
	// THE REVISION THE STORE ASSIGNS IS THE EPOCH, so concurrent activations
	// get two epochs. An Update carrying the sequence read above when the
	// caller named what it replaces, so a concurrent edit fails rather than
	// being lost behind a 201. Even the unconditional write compares-and-sets,
	// since its instant is decided against the pointer it replaces
	// ([coord.ActivationAt]).
	revision, at, err := f.flip(flipCtx, req, body, seq, previous)
	if err != nil {
		return coord.Activation{}, err
	}
	return coord.Activation{
		Epoch:      int64(revision),
		RevisionID: req.RevisionID,
		At:         at,
		Summary:    req.Summary,
		Origin:     req.Origin.UTC(),
		Restored:   req.Restore,
		Body:       body,
	}, nil
}

// flipBy is how long after its body's write begins a flip may still land: half
// of [coord.BodyRetention], so the sweep, which removes only a body written
// longer ago than that and read before the pointer, never removes one a flip
// in flight names, with the other half left for the clock skew between this
// node and the broker that stamps the body.
const flipBy = coord.BodyRetention / 2

// bodyKey is the key body is stored under in the config bucket, beside
// [activationKey]: `body.<sha256>` ([coord.BodyClass]), one key one set of
// bytes.
func bodyKey(body coord.BodyRef) string { return coord.DocumentKey(coord.BodyClass, body.SHA256) }

// pointerBytes is the pointer's stored form for req ([coord.ActivationValue])
// at the instant at, which is decided against the pointer the write replaces
// ([coord.ActivationAt]) and so is known only inside the write.
func pointerBytes(req coord.ActivationRequest, body coord.BodyRef, at time.Time) ([]byte, error) {
	raw, err := coord.ActivationValue(req, body, at)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: %w", err)
	}
	return raw, nil
}

// expectedSeq resolves the caller's expectation to the KV sequence to
// compare-and-set against and the instant of the activation it names, or
// reports the race. Zero means unconditional
// ([coord.ActivationRequest.Expect]) or create-only; an unconditional write
// reads the pointer in [FleetStore.flip], since it re-reads on every loss.
func (f *FleetStore) expectedSeq(ctx context.Context, req coord.ActivationRequest) (uint64, time.Time, error) {
	if req.Expect == "" && !req.ExpectAbsent {
		return 0, time.Time{}, nil
	}
	record, seq, held, err := f.pointer(ctx)
	switch {
	case err != nil:
		return 0, time.Time{}, err
	case !held && req.ExpectAbsent:
		// Exactly what a create-only write is waiting to see.
		return 0, time.Time{}, nil
	case !held:
		// WHAT THE WRITE WAS BUILT ON IS GONE: the pointer was deleted or
		// purged since the caller read it, so the write edits a company
		// the fleet no longer runs. See [coord.ActivationRequest.Expect].
		return 0, time.Time{}, fmt.Errorf("%w: expected %s, the fleet has no activation",
			coord.ErrActivationRaced, req.Expect)
	case req.ExpectAbsent:
		return 0, time.Time{}, fmt.Errorf("%w: expected no activation, the fleet is on %s",
			coord.ErrActivationRaced, record.RevisionID)
	case record.RevisionID != req.Expect:
		return 0, time.Time{}, fmt.Errorf("%w: expected %s, the fleet is on %s",
			coord.ErrActivationRaced, req.Expect, record.RevisionID)
	case req.ExpectEpoch != 0 && int64(seq) != req.ExpectEpoch:
		// THE EPOCH IS THE SEQUENCE: compared against what the caller read,
		// since a re-activation of the same revision moves this fresh read.
		return 0, time.Time{}, fmt.Errorf("%w: expected %s at epoch %d, the fleet "+
			"re-activated it at epoch %d", coord.ErrActivationRaced, req.Expect,
			req.ExpectEpoch, seq)
	}
	return seq, record.At, nil
}

// pointer reads the activation pointer: its record, the KV sequence to
// compare-and-set against, and whether there is one at all.
func (f *FleetStore) pointer(ctx context.Context) (coord.ActivationRecord, uint64, bool, error) {
	entry, err := f.get(ctx, f.config, activationKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return coord.ActivationRecord{}, 0, false, nil
	}
	if err != nil {
		return coord.ActivationRecord{}, 0, false, unavailable("read the activation to compare against", err)
	}
	var record coord.ActivationRecord
	if err = json.Unmarshal(entry.Value(), &record); err != nil {
		return coord.ActivationRecord{}, 0, false, fmt.Errorf("coord/kv: decode the activation: %w", err)
	}
	return record, entry.Revision(), true, nil
}

// flip writes the pointer, conditionally when there was an expectation, and
// reports the revision and the instant it was published at. A create-only
// write is a Create: the store's "only if absent" makes two nodes claiming the
// company resolve to one winner. previous is the instant of the pointer seq
// names ([FleetStore.expectedSeq]).
func (f *FleetStore) flip(ctx context.Context, req coord.ActivationRequest,
	body coord.BodyRef, seq uint64, previous time.Time) (uint64, time.Time, error) {

	switch {
	case req.ExpectAbsent:
		at := coord.ActivationAt(req.At, time.Time{})
		raw, err := pointerBytes(req, body, at)
		if err != nil {
			return 0, time.Time{}, err
		}
		revision, err := f.create(ctx, f.config, activationKey, raw)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				return 0, time.Time{}, fmt.Errorf("%w: an activation was published "+
					"while this write was being prepared", coord.ErrActivationRaced)
			}
			return 0, time.Time{}, unavailable("publish the activation", err)
		}
		return revision, at, nil
	case req.Expect == "":
		return f.assert(ctx, req, body)
	}
	at := coord.ActivationAt(req.At, previous)
	raw, err := pointerBytes(req, body, at)
	if err != nil {
		return 0, time.Time{}, err
	}
	revision, err := f.config.Update(ctx, activationKey, raw, seq)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) || isWrongLastSequence(err) {
			// SOMEBODY WROTE BETWEEN: the race, not an unavailable store,
			// since the caller's fix is to re-read and rebuild.
			return 0, time.Time{}, fmt.Errorf("%w: %s was replaced while this "+
				"write was being prepared", coord.ErrActivationRaced, req.Expect)
		}
		return 0, time.Time{}, unavailable("publish the activation", err)
	}
	return revision, at, nil
}

// assert is the UNCONDITIONAL write: whatever the pointer names, it ends up
// naming req, at an instant later than the one it replaces.
//
// A compare-and-set anyway, since the instant is decided against the pointer
// replaced ([coord.ActivationAt]); losing is no refusal, so it re-reads and
// retries. Bounded by the context, not a count: every lost round is another
// writer's landed write, and its one caller (`crewlet run -import-company`)
// and every other writer write once per read, so it always progresses.
func (f *FleetStore) assert(ctx context.Context, req coord.ActivationRequest,
	body coord.BodyRef) (uint64, time.Time, error) {
	for {
		record, seq, held, err := f.pointer(ctx)
		if err != nil {
			return 0, time.Time{}, err
		}
		var previous time.Time
		if held {
			previous = record.At
		}
		at := coord.ActivationAt(req.At, previous)
		raw, err := pointerBytes(req, body, at)
		if err != nil {
			return 0, time.Time{}, err
		}
		var revision uint64
		if held {
			revision, err = f.config.Update(ctx, activationKey, raw, seq)
		} else {
			revision, err = f.create(ctx, f.config, activationKey, raw)
		}
		switch {
		case err == nil:
			return revision, at, nil
		case errors.Is(err, jetstream.ErrKeyExists) || isWrongLastSequence(err):
			if ctx.Err() != nil {
				return 0, time.Time{}, unavailable("publish the activation", ctx.Err())
			}
			continue
		default:
			return 0, time.Time{}, unavailable("publish the activation", err)
		}
	}
}

// isWrongLastSequence reports a DECIDED compare-and-set refusal: 10071, the
// leader's answer on either topology once it has compared revisions.
//
// Not 10164, the leader saying another write to the subject is still in
// flight, which decides nothing (it may be this caller's own); every handle
// here is [settled], which waits it out (settle.go). Matched on the code, never
// the message a client release may reword: a refusal read as an outage
// answers 503 where it should answer 409.
func isWrongLastSequence(err error) bool {
	var api *jetstream.APIError
	return errors.As(err, &api) && api.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
}

// Body reads the body a names, given the time its recorded size takes to
// arrive ([getSized]), and holds it to the length and digest a names.
func (f *FleetStore) Body(ctx context.Context, a coord.Activation) (json.RawMessage, bool, error) {
	if !a.Body.Named() {
		return nil, false, fmt.Errorf("coord/kv: read the body of %s: %w", a.RevisionID, coord.ErrBodyless)
	}
	if err := a.Body.Check(); err != nil {
		return nil, false, fmt.Errorf("coord/kv: read the body of %s: %w", a.RevisionID, err)
	}
	key := bodyKey(a.Body)
	entry, err := getSized(ctx, f.js, f.config, key, a.Body.Bytes)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		// SWEPT: the pointer moved on, and the caller reads it again.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, unavailable("read the activation's body", err)
	}
	if !a.Body.Describes(entry.Value()) {
		return nil, false, fmt.Errorf("coord/kv: %s/%s holds %d bytes that are not the %d the "+
			"activation of %s names: %w", f.config.Bucket(), key, len(entry.Value()), a.Body.Bytes,
			a.RevisionID, coord.ErrBodyCorrupt)
	}
	return entry.Value(), true, nil
}

// BodyCrossing is how long a's body takes to cross this node's connection at
// the slowest pace its reach allows: a read's reply of the body and the most
// header a write carries, none in process. The recorded size is held to what
// a key holds, so a pointer naming another size stretches no grace past it
// and shortens none below nothing ([coord.BodyRef.Check]).
func (f *FleetStore) BodyCrossing(a coord.Activation) time.Duration {
	size := min(max(a.Body.Bytes, 0), coord.MaxValueBytes)
	return jsapi.Crossing(size+kvHeaderBytes, f.reach.pace())
}

// Target reads the pointer: a small record ([coord.MaxPointerBytes]), read at
// the read term. A pointer written before bodies had keys answers with no
// body named, which [FleetStore.Body] refuses: a node holding the revision
// keeps running it.
func (f *FleetStore) Target(ctx context.Context) (coord.Activation, bool, error) {
	entry, err := f.get(ctx, f.config, activationKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return coord.Activation{}, false, nil
	}
	if err != nil {
		return coord.Activation{}, false, unavailable("read the activation pointer", err)
	}
	var record coord.ActivationRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return coord.Activation{}, false, unavailable("decode the activation pointer", err)
	}
	return coord.Activation{
		Epoch:      int64(entry.Revision()),
		RevisionID: record.RevisionID,
		At:         record.At,
		Summary:    record.Summary,
		Origin:     record.Origin(),
		Restored:   record.Restored,
		Body:       record.Body,
	}, true, nil
}

// applyRecord is one node's status. The node id is the KEY, so a node cannot
// report on behalf of another by writing a different field.
type applyRecord struct {
	Epoch      int64     `json:"epoch"`
	RevisionID string    `json:"revision_id,omitempty"`
	Applied    int64     `json:"applied_epoch,omitempty"`
	Serving    string    `json:"serving_revision_id,omitempty"`
	Boots      string    `json:"boots_revision_id,omitempty"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// RecordApply publishes this node's status for an epoch.
func (f *FleetStore) RecordApply(ctx context.Context, status coord.NodeApply) error {
	if status.NodeID == "" {
		return errors.New("coord/kv: an apply status needs a node id")
	}
	if err := status.Check(); err != nil {
		return err
	}
	detail := coord.TruncateApplyError(status.Error)
	raw, err := json.Marshal(applyRecord{
		Epoch: status.Epoch, RevisionID: status.RevisionID, Applied: status.Applied,
		Serving: status.Serving, Boots: status.Boots, Status: status.Status,
		Error: detail, UpdatedAt: status.UpdatedAt.UTC(),
	})
	if err != nil {
		return fmt.Errorf("coord/kv: encode the apply status: %w", err)
	}
	// Put, unconditionally: a node's status is its own, and every write
	// refreshes the key's age, so a node that STOPS reporting ages out.
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
		// raised: failing over one row would blank the whole fleet view,
		// where dropping it leaves only the bad node unreported.
		if err := json.Unmarshal(kve.Value(), &record); err != nil {
			return nil
		}
		node, ok := decodeKey(kve.Key())
		if !ok {
			return nil
		}
		out = append(out, coord.NodeApply{
			NodeID: node, Epoch: record.Epoch, RevisionID: record.RevisionID,
			Applied: record.Applied, Serving: record.Serving, Boots: record.Boots,
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

// One credential's record under its name, the bucket's one key class
// ([coord.MaxSecretRecordBytes], [secretsHolding]), stored as
// [coord.EncodeSecret] writes it on both backends.

// Secret reads one sealed value.
func (f *FleetStore) Secret(ctx context.Context, name string) (coord.SecretRecord, bool, error) {
	if name == "" {
		return coord.SecretRecord{}, false, errors.New("coord/kv: a secret needs a name")
	}
	entry, err := f.get(ctx, f.secrets, encodeKey(name))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.SecretRecord{}, false, nil
	case err != nil:
		// RAISED: "no such credential" renders as an empty ${VAR}, an auth
		// failure blamed on the vendor, which an unreadable store must never say.
		return coord.SecretRecord{}, false, unavailable("read the secret", err)
	}
	rec, ok := coord.DecodeSecret(entry.Value())
	if !ok {
		return coord.SecretRecord{}, false, fmt.Errorf(
			"coord/kv: the stored secret %q is not decodable", name)
	}
	rec.Version = entry.Revision()
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
		rec, ok := coord.DecodeSecret(kve.Value())
		if !ok {
			return fmt.Errorf("coord/kv: a stored secret is not decodable")
		}
		rec.Version = kve.Revision()
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
	raw, err := coord.EncodeSecret(rec)
	if err != nil {
		return err
	}
	if _, err := f.secrets.Put(ctx, encodeKey(rec.Name), raw); err != nil {
		return unavailable("write the secret", err)
	}
	return nil
}

// CreateSecret writes a sealed value only where none is stored.
//
// THROUGH [createKey], the one create this package makes, so a purged row is
// absent — a create is never refused over the history of a key somebody
// destroyed — and a live one is somebody else's, answered false and left
// untouched.
func (f *FleetStore) CreateSecret(ctx context.Context, rec coord.SecretRecord) (bool, error) {
	raw, err := coord.EncodeSecret(rec)
	if err != nil {
		return false, err
	}
	_, err = f.create(ctx, f.secrets, encodeKey(rec.Name), raw)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyExists):
		return false, nil
	default:
		return false, unavailable("create the secret", err)
	}
}

// UpdateSecret replaces a sealed value only while it is still at version.
//
// A ZERO VERSION writes nothing: the client reads an expected revision of zero
// as "never written", a create that could resurrect a deleted row.
func (f *FleetStore) UpdateSecret(ctx context.Context, rec coord.SecretRecord, version uint64) (bool, error) {
	raw, err := coord.EncodeSecret(rec)
	if err != nil {
		return false, err
	}
	if version == 0 {
		return false, nil
	}
	_, err = f.secrets.Update(ctx, encodeKey(rec.Name), raw, version)
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	default:
		return false, unavailable("update the secret", err)
	}
}

// DeleteSecret removes a value, reporting whether it was there.
//
// PURGED (markers.go, "Every removal is a purge"): it removes every revision,
// and a credential's history is what you least want kept.
func (f *FleetStore) DeleteSecret(ctx context.Context, name string) (bool, error) {
	if name == "" {
		return false, errors.New("coord/kv: a secret needs a name")
	}
	key := encodeKey(name)
	if _, err := f.get(ctx, f.secrets, key); errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	} else if err != nil {
		return false, unavailable("read the secret before deleting it", err)
	}
	if err := f.secrets.Purge(ctx, key); err != nil {
		return false, unavailable("delete the secret", err)
	}
	return true, nil
}

// ---- the agent-to-agent channels --------------------------------------- //

// channelRecord is one authorization row on the wire. ClosedAt is a POINTER so
// "open" is an absent value, never a zero instant a decoder could misread —
// the one field where a wrong reading changes whether an answer is delivered.
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
	ch, err := decodeChannelRecord(raw)
	if err != nil {
		return coord.Channel{}, unavailable("decode the channel", err)
	}
	ch.ID = id
	return ch, nil
}

// decodeChannelRecord is a channel's record with no id — the id is its key —
// and its decoder's own error, for a caller that names the failure itself.
func decodeChannelRecord(raw []byte) (coord.Channel, error) {
	var record channelRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return coord.Channel{}, err
	}
	ch := coord.Channel{
		Requester: record.Requester, Target: record.Target,
		Messages: record.Messages, OpenedAt: record.OpenedAt, LastAt: record.LastAt,
	}
	if record.ClosedAt != nil {
		ch.ClosedAt = *record.ClosedAt
	}
	return ch, nil
}

// OpenChannel records a new channel, ignoring an id that already exists.
//
// Create, not Put: the id is minted per ask, so ErrKeyExists is a retried
// publish, and overwriting would reset a channel already carrying an answer.
func (f *FleetStore) OpenChannel(ctx context.Context, ch coord.Channel) error {
	if ch.ID == "" {
		return errors.New("coord/kv: a channel needs an id")
	}
	if err := ch.Check(); err != nil {
		return err
	}
	raw, err := encodeChannel(ch)
	if err != nil {
		return err
	}
	_, err = f.create(ctx, f.channels, encodeKey(ch.ID), raw)
	switch {
	case err == nil, errors.Is(err, jetstream.ErrKeyExists):
		return nil
	default:
		return unavailable("open the channel", err)
	}
}

// Channel reads one record.
func (f *FleetStore) Channel(ctx context.Context, id string) (coord.Channel, bool, error) {
	entry, err := f.get(ctx, f.channels, encodeKey(id))
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
// Not a blind Put: the requester's node counts the ask while the target's
// counts the answer, and a lost update under-reports the traffic the cap
// exists to catch. Coalesced per key ([writeLines]).
func (f *FleetStore) mutateChannel(ctx context.Context, what, id string, apply func(coord.Channel) coord.Channel) (coord.Channel, bool, error) {
	var found bool
	ch, err := f.channelW.write(ctx, f.channels, f.js, encodeKey(id), id, what+" the channel",
		func(stored coord.Channel, present bool) (coord.Channel, bool) {
			// A channel that is not there is not opened by a count or a
			// close: it is answered as absent.
			if found = present; !present {
				return coord.Channel{}, false
			}
			stored.ID = id
			return apply(stored), true
		})
	if err != nil || !found {
		return coord.Channel{}, false, err
	}
	return ch, true, nil
}

// OpenChannels returns every channel still open, by id.
func (f *FleetStore) OpenChannels(ctx context.Context) ([]coord.Channel, error) {
	return f.listChannels(ctx, coord.Channel.Open)
}

// AllChannels returns every channel this store still holds, by id.
func (f *FleetStore) AllChannels(ctx context.Context) ([]coord.Channel, error) {
	return f.listChannels(ctx, func(coord.Channel) bool { return true })
}

// listChannels walks the bucket and keeps what matches.
//
// ONE WALK FOR BOTH LISTINGS, so the decode, the key check and the ordering
// cannot drift between them.
func (f *FleetStore) listChannels(ctx context.Context, keep func(coord.Channel) bool) (
	[]coord.Channel, error) {

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
		if keep(ch) {
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
// PURGED, the standing rule for a bucket no clock ages — see markers.go, "Every
// removal is a purge", and [FleetStore.SweepMarkers] for what removes the
// marker it leaves.
func (f *FleetStore) PurgeChannels(ctx context.Context, cutoff time.Time) (int64, error) {
	// DECIDED FIRST, PURGED AFTER: the sweep must not write to the bucket its
	// listing is still delivered from. Each candidate keeps its revision.
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
		// Predicated on the revision read: a channel changed since is not
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

// ClaimFire records one runner's claim on a minute, reporting whether this
// call wrote it.
//
// Create, so the first writer wins and every later tick — this node's own
// re-evaluated minute, or a peer that has just picked up the scheduler duty —
// reads false and does not dispatch.
func (f *FleetStore) ClaimFire(ctx context.Context, minute, runner string, at time.Time) (bool, error) {
	if minute == "" {
		return false, errors.New("coord/kv: a fire claim needs a minute")
	}
	raw, err := json.Marshal(fireRecord{At: at.UTC()})
	if err != nil {
		return false, fmt.Errorf("coord/kv: encode the fire claim: %w", err)
	}
	_, err = f.create(ctx, f.fires, fireKey(minute, runner), raw)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyExists):
		return false, nil
	default:
		// RAISED, never "somebody else has it": the caller skips the tick
		// on an error, while a lost race would mark the fire handled.
		return false, unavailable("claim the fire", err)
	}
}

// FireMinuteClaimed reports whether any runner, or none, holds the minute.
//
// ONE FILTERED LISTING of the minute's own keys ([fireKey]), so the question
// costs what that minute holds, never every claim of the week.
func (f *FleetStore) FireMinuteClaimed(ctx context.Context, minute string) (bool, error) {
	if minute == "" {
		// Refused: an empty minute filters nothing, and "nothing matched"
		// writes a skip into the schedule's history.
		return false, errors.New("coord/kv: a fire minute read needs a minute")
	}
	held := false
	err := f.eachUnder(ctx, f.fires, coord.DocumentFilter(minute), "the fire claims",
		func(jetstream.KeyValueEntry) error {
			held = true
			return nil
		})
	if err != nil {
		return false, err
	}
	return held, nil
}

// fireKey is one runner's claim on a minute: the minute, then the runner, each
// its own key segment. The minute leads so a minute's claims share a
// filterable prefix ([FleetStore.FireMinuteClaimed]). The runner segment
// carries a one-byte mark because a catchup skip names no runner and the
// grammar refuses an empty segment ([coord.DocumentKey]); the skip's segment
// is the mark alone, so the mapping stays injective.
func fireKey(minute, runner string) string {
	return coord.DocumentKey(minute, fireRunnerMark+runner)
}

// fireRunnerMark prefixes the runner segment of a fire claim ([fireKey]). A
// literal byte of the key grammar, so it reads as itself in the bucket.
const fireRunnerMark = "_"

// fireRecord is one claim on the wire. The instant is diagnostic, for an
// operator reading the bucket, and nothing branches on it.
type fireRecord struct {
	At time.Time `json:"at"`
}

// ---- the custody claims ----------------------------------------------- //

// custodyRecord is one batch's keeper on the wire, as a claim writes it. The
// instant is diagnostic, and nothing branches on it.
type custodyRecord struct {
	Node string    `json:"node"`
	At   time.Time `json:"at"`
}

// custodyKeeper is what a claim READS of a record: the keeper alone. Decoding
// [custodyRecord] whole would fail on a field nothing reads (an `at` a later
// build retyped) and answer [coord.ErrCustodyUndecidable], which must mean
// the KEEPER cannot be read.
type custodyKeeper struct {
	Node string `json:"node"`
}

// ClaimCustody records node as the keeper of batch unless one is recorded,
// answering the keeper.
//
// A refused create is read back FROM THE LEADER ([FleetStore.get]): a replica
// behind the create would say nobody keeps the batch, and the caller would
// keep a second copy. A key gone between refusal and read (aged out, a month)
// is created again within [casRetry]'s bound. A record nothing can read as a
// keeper is [coord.ErrCustodyUndecidable] ([FleetStore.undecidable]).
func (f *FleetStore) ClaimCustody(ctx context.Context, batch, node string) (string, error) {
	if batch == "" || node == "" {
		return "", errors.New("coord/kv: a custody claim needs a batch and a node")
	}
	if err := coord.CheckCustodyNode(node); err != nil {
		return "", err
	}
	raw, encodeErr := json.Marshal(custodyRecord{Node: node, At: time.Now().UTC()})
	if encodeErr != nil {
		return "", fmt.Errorf("coord/kv: encode the custody claim: %w", encodeErr)
	}
	key := encodeKey(batch)
	var keeper string
	settled, err := casRetry(ctx, &f.custodyTrips, "claim custody of batch "+batch, func() (bool, error) {
		_, err := f.create(ctx, f.custody, key, raw)
		switch {
		case err == nil:
			keeper = node
			return true, nil
		case errors.Is(err, errUndecodableHeaders):
			// The create reads the newest message back to tell a marker
			// from a value, and its headers say neither.
			return false, f.undecidable(batch, err)
		case !errors.Is(err, jetstream.ErrKeyExists):
			return false, unavailable("claim custody of batch "+batch, err)
		}
		entry, read := f.get(ctx, f.custody, key)
		switch {
		case errors.Is(read, jetstream.ErrKeyNotFound):
			// GONE between the refusal and the read (only the month-long
			// age removes it): not settled, so the loop creates it again.
			return false, nil
		case errors.Is(read, errUndecodableHeaders):
			return false, f.undecidable(batch, read)
		case read != nil:
			return false, unavailable("read the keeper of batch "+batch, read)
		}
		var held custodyKeeper
		if decode := json.Unmarshal(entry.Value(), &held); decode != nil {
			return false, f.undecidable(batch, decode)
		}
		if held.Node == "" {
			return false, f.undecidable(batch, errors.New("it names no node"))
		}
		keeper = held.Node
		return true, nil
	})
	switch {
	case err != nil:
		return "", err
	case !settled:
		return "", contended("ClaimCustody", batch)
	}
	return keeper, nil
}

// undecidable is the answer for a batch whose keeper record the leader holds
// and nothing can read as a keeper: undecodable headers, an undecodable value,
// or no node named.
//
// RAISED, since guessing loses the batch or keeps it twice, and DEFINITE,
// never [coord.ErrUnavailable]: the leader will answer the same until the
// record goes, so waiting would wait out the bucket's month. It names the
// bucket and the key ([coord.DocumentKey] of the batch id) because removing
// the record is the one remedy short of that month.
func (f *FleetStore) undecidable(batch string, why error) error {
	return fmt.Errorf("%w: batch %s (key %s in bucket %s): %w", coord.ErrCustodyUndecidable,
		batch, encodeKey(batch), f.custody.Bucket(), why)
}

// ---- the rebases -------------------------------------------------------- //

// rebaseRecord is one recorded rebase on the wire.
type rebaseRecord struct {
	At time.Time `json:"at"`
}

// Rebase reads the instant recorded for a seed and the revision it was read
// at, (zero, 0) where nothing is recorded. From the leader ([FleetStore.get]):
// a lagging replica's "not found" would send this attempt to mint anew.
func (f *FleetStore) Rebase(ctx context.Context, seed string) (time.Time, uint64, error) {
	if seed == "" {
		return time.Time{}, 0, errors.New("coord/kv: a rebase needs a seed")
	}
	entry, err := f.get(ctx, f.rebases, encodeKey(seed))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return time.Time{}, 0, nil
	}
	if err != nil {
		return time.Time{}, 0, unavailable("read the rebase", err)
	}
	var rec rebaseRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		// Not "nothing recorded": absent, the caller would mint anew over an
		// instant an earlier attempt wrote under.
		return time.Time{}, 0, fmt.Errorf("coord/kv: the rebase recorded for %q "+
			"does not decode: %w", seed, err)
	}
	return rec.At.UTC(), entry.Revision(), nil
}

// RecordRebase writes a seed's instant, conditional on the revision read: 0
// creates it only where nothing is recorded.
func (f *FleetStore) RecordRebase(ctx context.Context, seed string, at time.Time, version uint64) (bool, error) {
	if seed == "" {
		return false, errors.New("coord/kv: a rebase needs a seed")
	}
	value, err := json.Marshal(rebaseRecord{At: at.UTC()})
	if err != nil {
		return false, fmt.Errorf("coord/kv: encode the rebase: %w", err)
	}
	if version == 0 {
		_, err = f.create(ctx, f.rebases, encodeKey(seed), value)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, jetstream.ErrKeyExists):
			return false, nil
		default:
			return false, unavailable("record the rebase", err)
		}
	}
	_, err = f.rebases.Update(ctx, encodeKey(seed), value, version)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound):
		// A LOST RACE, not a fault (moved by another attempt, or aged out):
		// the caller reads again.
		return false, nil
	default:
		return false, unavailable("record the rebase", err)
	}
}

// ---- the integration reconcile status ---------------------------------- //

// IntegrationStatuses returns every recorded status, keyed by surface: a
// pass over the status class alone, never a findings record.
func (f *FleetStore) IntegrationStatuses(ctx context.Context) (map[string][]byte, error) {
	return f.integrationClass(ctx, coord.IntegrationStatusClass, "the integration statuses")
}

// IntegrationFindings returns every recorded findings record, keyed by
// surface.
func (f *FleetStore) IntegrationFindings(ctx context.Context) (map[string][]byte, error) {
	return f.integrationClass(ctx, coord.IntegrationFindingsClass, "the integration findings")
}

// integrationClass is every value of one of the bucket's classes, keyed by
// surface, waiting the class's quiet ([integrationsHolding]).
func (f *FleetStore) integrationClass(ctx context.Context, class, what string) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := f.eachUnder(ctx, f.integrations, coord.DocumentFilter(class), what,
		func(kve jetstream.KeyValueEntry) error {
			kind, ok := coord.IntegrationKind(kve.Key(), class)
			if !ok {
				// A key this backend did not write names no surface.
				return nil
			}
			// COPIED: the entry's buffer belongs to the client.
			out[kind] = bytes.Clone(kve.Value())
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// IntegrationStatus returns one surface's status, false where none.
func (f *FleetStore) IntegrationStatus(ctx context.Context, kind string) ([]byte, bool, error) {
	return f.integrationValue(ctx, coord.IntegrationStatusKey(kind), kind, "status")
}

// IntegrationFindingsOf returns one surface's findings record, false where
// none.
func (f *FleetStore) IntegrationFindingsOf(ctx context.Context, kind string) ([]byte, bool, error) {
	return f.integrationValue(ctx, coord.IntegrationFindingsKey(kind), kind, "findings")
}

// integrationValue is one key's value, read by its leader in the time its
// class gives ([integrationsHolding]).
func (f *FleetStore) integrationValue(ctx context.Context, key, kind, what string) ([]byte, bool, error) {
	if kind == "" {
		return nil, false, errors.New("coord/kv: an integration's " + what + " is read per surface")
	}
	entry, err := f.get(ctx, f.integrations, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, unavailable("read the "+kind+" integration "+what, err)
	}
	return bytes.Clone(entry.Value()), true, nil
}

// PutIntegrationStatus records one surface's status.
func (f *FleetStore) PutIntegrationStatus(ctx context.Context, kind string, value []byte) error {
	if kind == "" {
		return errors.New("coord/kv: an integration status needs a surface name")
	}
	if _, err := f.integrations.Put(ctx, coord.IntegrationStatusKey(kind), value); err != nil {
		return unavailable("record the integration status", err)
	}
	return nil
}

// RecordIntegrationPass records a pass's findings, or purges them where it
// found none, and then its status: in that order, so a status read after its
// write names a findings record already there. Both are held to their
// classes' bounds before either is written, so a refused status leaves no
// findings it would have named.
func (f *FleetStore) RecordIntegrationPass(ctx context.Context, kind string, status, findings []byte) error {
	if kind == "" {
		return errors.New("coord/kv: an integration status needs a surface name")
	}
	key := coord.IntegrationFindingsKey(kind)
	if b, ok := f.integrations.(settledBucket); ok {
		if err := errors.Join(b.carriedBack(key, findings),
			b.carriedBack(coord.IntegrationStatusKey(kind), status)); err != nil {
			return err
		}
	}
	if findings == nil {
		err := f.integrations.Purge(ctx, key)
		if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
			return unavailable("drop the integration findings", err)
		}
	} else if _, err := f.integrations.Put(ctx, key, findings); err != nil {
		return unavailable("record the integration findings", err)
	}
	return f.PutIntegrationStatus(ctx, kind, status)
}

// DeleteIntegrationStatus drops a surface's status and then its findings,
// so no status names findings already gone. PURGED, the standing rule for a
// bucket no clock ages: see markers.go, "Every removal is a purge", and
// [FleetStore.SweepMarkers] for what removes the marker it leaves.
func (f *FleetStore) DeleteIntegrationStatus(ctx context.Context, kind string) error {
	for _, key := range []string{coord.IntegrationStatusKey(kind), coord.IntegrationFindingsKey(kind)} {
		err := f.integrations.Purge(ctx, key)
		if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
			return unavailable("delete the integration status", err)
		}
	}
	return nil
}
