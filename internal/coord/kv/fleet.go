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
	"github.com/crewlet/crewlet/internal/jsprovision"
)

// A BUCKET IS A STREAM, and provisioning a replicated one has the two hazards
// [internal/queue/jetstream] documents for the stream side. This is the same
// fix, because it is the same call underneath — and both sides now take the
// budgets, the retry cadence and the "still forming" predicate from
// [internal/jsprovision], so neither can drift away from the other.
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

// positionsSuffix is the per-node state-log position bucket.
const positionsSuffix = "_statelog_positions"

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
//
// ctx IS THE BOOT'S, and the per-create deadline is derived below rather than
// taken from the caller, because the read-back at the end runs precisely when
// that deadline has expired — see [jsprovision.Settle].
func openBucket(ctx context.Context, js jetstream.JetStream,
	clustered bool, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, bucketFacts, error) {

	// A BREADCRUMB, because without one this is the silent step. A boot
	// opens eighteen of these in a row and logs nothing between them, so a
	// node that hung here emitted nothing at all until its budget expired —
	// and the log could not say which bucket it was on.
	//
	// IT SPANS THE LOOKUP AND THE CREATE, AND SAYS SO — and it stops where
	// the create ends rather than where this function does.
	//
	// Both halves were wrong in the same way, at opposite ends. Armed
	// before the lookup while saying "created", it reported a create that
	// had not been attempted for a bucket that already existed; left armed
	// across the replica observation after it, it reported one that had
	// already finished. Either way it names a step the member is not on,
	// which is the single thing this line exists to get right.
	//
	// ON ctx rather than on either term below, because the two halves it
	// spans now carry separate deadlines and a watcher armed on one of them
	// would stop reporting the moment that half gave up.
	stop := jsprovision.WhenSlow(ctx, func(after time.Duration) {
		log.WarnContext(ctx, "coord_kv_bucket_slow", "bucket", cfg.Bucket,
			"replicas", cfg.Replicas, "waited", after,
			"detail", "this bucket is still being provisioned — looked up, "+
				"and created if it was absent; on a fleet that is a metadata "+
				"group that has not settled, and the next line from this node "+
				"says whether it got past it")
	})

	// THE LOOKUP IS SIZED AS A READ, not as the create it precedes — see
	// [jsprovision.LookupBudget]. Sharing the create's term meant a lookup
	// nobody answered spent the whole clustered budget and left none of it
	// for the create that would have settled the question.
	lookupCtx, cancelLookup := context.WithTimeout(ctx, jsprovision.LookupBudget)
	var bucket jetstream.KeyValue
	err := jsprovision.Ask(lookupCtx, jsprovision.Clustered(clustered).AskTerm(),
		func(ctx context.Context) error {
			var e error
			bucket, e = js.KeyValue(ctx, cfg.Bucket)
			return e
		}, nil)
	cancelLookup()
	switch {
	case err == nil:
		stop()
		// ON ctx, NOT lookupCtx: a slow lookup can leave its own
		// deadline spent, and the observation would then fail on a
		// bucket it had just found. observeBucket owns its own term.
		return observeBucket(ctx, bucket, cfg)
	case errors.Is(err, jetstream.ErrBucketNotFound):
		// TOLD it is absent. Create it below.
	case jsprovision.Unanswered(ctx, err):
		// TOLD NOTHING — the third value, and not the same fact as
		// "not there". See [jsprovision.Unanswered]: the create below
		// answers it either way, so a boot no longer fails on a
		// question the broker never got round to.
		log.WarnContext(ctx, "coord_kv_bucket_lookup_unanswered",
			"bucket", cfg.Bucket, "error", err.Error(),
			"detail", "the broker did not say whether this bucket exists, so "+
				"the create below decides it: absent and it is made, present "+
				"and it comes back as a peer's win and its replicas are read")
	default:
		stop()
		return nil, bucketFacts{}, err
	}
	// WithTimeout only ever shortens against the parent, so a caller that
	// already set a tighter deadline keeps it — which is also what makes
	// the sequence ceiling [OpenFleet] applies effective: each create here
	// takes the lesser of its own budget and what is left of that one.
	createCtx, cancel := context.WithTimeout(ctx,
		jsprovision.Clustered(clustered).Budget())
	defer cancel()
	// REASSIGNED rather than shadowed: the lookup above left bucket nil on
	// every path that reaches here, and a second name would make the two
	// halves read as different objects.
	var createErr error
	bucket, createErr = createKeyValue(createCtx, js, clustered, cfg)
	stop()
	if createErr == nil {
		// THIS NODE MADE IT, at what it asked for — and it is READ BACK
		// anyway, because the create's own return says what was sent
		// rather than what is in force. One metadata read against a
		// group that has just answered, and the alternative is a
		// retention nobody ever compared on the one path where it
		// could still be wrong (a server ceiling, a clamp, a bucket
		// whose stream a peer had resized between the lookup and now).
		return observeBucket(ctx, bucket, cfg)
	}
	if jsprovision.OutOfCapacity(createErr) {
		// THE BROKER HAS NO ROOM. Nothing frees capacity by being
		// waited for, so this was attempted ONCE — [jsprovision.Place]
		// re-asks only [jsprovision.Unplaceable] — and it is terminal.
		// That is the whole difference between this arm and the one
		// below, and the reason they are two rather than one condition:
		// they are told apart by whether the create is retried, and a
		// merged arm could only ever be exercised as a pair.
		//
		// IT IS NOT A CEILING OF THIS BUCKET'S OWN. A bucket declares
		// none — nats.go sends MaxBytes -1 for a [jetstream.KeyValueConfig]
		// that sets none, and checkBytesLimits reads a negative as one
		// byte — so what is spent is what the limit's OTHER reservations
		// already hold, the state logs' ceilings above all, and what an
		// operator changes is theirs or the limit's rather than this
		// bucket's.
		//
		// WHICH LIMIT REFUSED DEPENDS ON THE TOPOLOGY, and for a BUCKET
		// it is never the one a clustered stream gets. A standalone
		// create is checked against the account's limit and the
		// server's (checkAllLimits, checkServer=true), so
		// `stream.store_max_bytes` can refuse it. A clustered create
		// runs the account half alone (jsClusteredStreamLimitsCheck →
		// checkAccountLimits, checkServer=false), and the place a
		// clustered STREAM's server limit surfaces instead — the
		// metadata leader's peer selection — skips its storage check
		// entirely for an object with no ceiling: the test is
		// `maxBytes > 0 && maxBytes > available`
		// (server/jetstream_cluster.go, selectPeerGroup). So on a fleet
		// this is the ACCOUNT limit's refusal alone, under the two
		// codes [jsprovision.OutOfCapacity] names. A clustered member
		// that cannot place the bucket for want of room is not this
		// arm and not this fact: that refusal is about another
		// member's disk, which is why it stays [jsprovision.Unplaceable]
		// below and is waited out rather than reported.
		//
		// NOT READ BACK: nothing was placed. Without this arm the
		// refusal fell through to the read-back below and came back as
		// a bucket that is "not there", which is the one reading that
		// sends an operator to the wrong subsystem.
		return nil, bucketFacts{}, createErr
	}
	if jsprovision.NoApplicableLimit(createErr) {
		// NO LIMIT APPLIES TO THIS BUCKET AT ALL, which is not the arm
		// above wearing another code: that one is a ceiling that does
		// not fit inside a limit, and this is an account that states no
		// limit to fit into. A bucket declares no ceiling of its own,
		// so it is the clearest case of the two being different — there
		// is no number here to make smaller.
		//
		// TERMINAL AND NOT READ BACK for the same reason as the arm
		// above, and it matters more here: unclassified, this refusal
		// fell through and reported a bucket that is "not there", which
		// of everything on a boot path is the sentence most likely to
		// be read as corruption.
		return nil, bucketFacts{}, fmt.Errorf("%w%s", createErr,
			jsprovision.NoApplicableLimitDetail(cfg.Replicas))
	}
	if jsprovision.Unplaceable(createErr) {
		// STILL FORMING, and it stayed that way for the whole budget,
		// which createKeyValue has already waited out — re-asking every
		// [jsprovision.PlacementRetry] until the create's deadline. A
		// cluster still gathering members is the one condition worth
		// waiting on, which is exactly why the capacity refusal above
		// must not share this arm: waited out, a limit nobody was going
		// to raise cost the whole provisioning budget and then reported
		// the broker's bare text.
		//
		// Nothing was placed here either, so there is nothing to read
		// back and a not-found would only obscure the refusal that says
		// what is actually wrong.
		return nil, bucketFacts{}, createErr
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
	// So the question is re-asked rather than assumed, and RE-ASKED while
	// it answers not-found, because a peer's create is visible to this
	// member only on its next metadata update — see [jsprovision.Settle].
	// One lookup answers at an arbitrary instant inside that window and
	// fails a boot over a bucket that exists. ON ctx AND NOT createCtx,
	// because createCtx is the deadline that just expired.
	err = jsprovision.Settle(ctx, func(ctx context.Context) error {
		var e error
		bucket, e = js.KeyValue(ctx, cfg.Bucket)
		return e
	})
	if err != nil {
		// THE CREATE'S ERROR IS WHAT IS REPORTED, with the read-back's
		// beside it: the first says what went wrong and the second only
		// confirms the create really did fail.
		return nil, bucketFacts{}, fmt.Errorf("%w (and it is not there: %w)", createErr, err)
	}
	// THE PEER MADE IT, so it is the peer's replica count that is in force
	// and this node has to agree with it — the same question the adopt
	// path above asks, for the same reason.
	return observeBucket(ctx, bucket, cfg)
}

// observeBucket reads back what is IN FORCE on a bucket and returns it: the
// replica count, which it REFUSES to run short of, and the age, which the
// caller reports on.
//
// # Why a booting node refuses rather than resizing
//
// Because the durability this store promises is not something whichever node
// booted last gets to decide, and because a bucket is a stream: this is the
// rule [internal/queue/jetstream]'s observeStream already applies to one, in
// the same words — "an acknowledged publish would be proving fewer copies than
// stream.replicas promises". Nothing here is ever APPLIED to a bucket that
// exists (see openBucket's doc for what CreateOrUpdate cost), so a mismatch is
// an operator gesture, not a write.
//
// # Why replication is the one bucket field worth refusing over
//
// The rest are reported instead — the age by [reportRetention], and the lease
// bucket's by [Open], which says what it costs this store's own arithmetic.
// Replication is different in kind: every other difference changes how this
// store BEHAVES and is visible in what it does, while this one changes only
// what survives losing a node, and is visible in nothing at all until that
// happens. A fleet raised from one replica to three, whose buckets were all
// made at one, goes on holding every lease, every fencing epoch and the
// company's SECRETS on a single disk while each node reports itself correctly
// configured.
//
// Equal or higher passes, so a single-replica development node against a
// three-replica fleet's buckets still starts.
//
// # It reads at ONE replica too, where it used to skip the round trip
//
// The saving was real and it bought a blind spot: at one replica there is
// nothing to be short of, so the read was skipped — and with it the only
// chance to notice that the bucket a node adopted is not aged the way that
// node asked for. That is the single-node and the small-fleet shape, which is
// to say most of them. One metadata read per bucket per boot is what knowing
// costs.
//
// ctx IS THE BOOT'S and the term is derived inside [readBucket], for the
// reason [jsprovision.Settle] gives: the per-create deadline may be spent by
// the time this runs — a slow lookup is enough — and an observation handed it
// fails instantly on a bucket that was just found.
func observeBucket(ctx context.Context, bucket jetstream.KeyValue,
	cfg jetstream.KeyValueConfig) (jetstream.KeyValue, bucketFacts, error) {

	facts, err := readBucket(ctx, bucket)
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
// # Why this is reported and not applied
//
// A bucket that exists is ADOPTED (see [openBucket]), so the age in force is
// whichever member created it, and a booting node rewriting one would be the
// silent overwrite this package removed everywhere else — decided by boot
// order, against a metadata group that is still electing, by the node that
// came up LAST. The remedy is an operator's: align the configuration across
// the fleet and delete the bucket while the fleet is down, so the next boot
// re-creates it.
//
// THE ONE EXCEPTION IS THE DUTY BUCKET, whose age is raised and never lowered
// because it is a CEILING on the TTLs the bucket can honour rather than a
// preference — see [openDuties], which is why that bucket reports nothing
// here: an age above coord.MaxDutyTTL is its healthy adopted state, and a
// warning on a healthy state is one nobody reads twice.
//
// # Why a warning is worth a line at all
//
// Because every one of these differences is silent in exactly the way the
// bucket table warns about: a claims bucket adopted at a peer's five minutes
// under a node configured for fifteen expires a claim ten minutes before its
// caller believes it does, and nothing says so — not the write, not the read,
// not the caller. The only moment the two numbers are ever in one place is
// this one.
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
// enough members to place it.
//
// THE LOOP IS AROUND THE CREATE ALONE, which is the shape
// [internal/queue/jetstream]'s createStream already has and the reason
// [jsprovision] exists: an unplaceable create means nothing was placed, so
// re-running the lookup that preceded it would re-ask a question whose answer
// cannot have changed. Written the other way round, the retry re-issued that
// lookup every 250ms for the whole provisioning budget.
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
// # Why SEVENTEEN buckets and not one
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
//	setup      a browser round trip and two clicks, a quarter of an hour:
//	           the same first-claim-wins record as claims on THREE TIMES
//	           its horizon, which is the whole reason it is a bucket of
//	           its own — a claim taken for longer than the bucket it lands
//	           in does not last longer, it lapses with the bucket and says
//	           nothing, and a spent callback state that lapses early is a
//	           replayable one
//	attempts   the throttle's window, a quarter of an hour, plus the one
//	           EXPLICIT MESSAGE CAP in this estate: History caps a caller's
//	           record at coord.AttemptCap attempts and discards the OLDEST
//	           when it overflows. A KV bucket's stream is DiscardNew, so
//	           the cap it has by default refuses the NEWEST write, and a
//	           throttled record that stops accepting attempts un-throttles
//	           its caller as the old ones age out — under exactly the
//	           flood that filled it
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
//	follows    a chat thread's last-activity horizon, ninety days: every
//	           re-assert rewrites the record, so the bucket's age IS that
//	           stamp — and a follow that expired while the thread was still
//	           live costs at most one missed non-mention reply, which the
//	           next mention re-establishes through the ordinary path
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
//	mailboxes  none at all, for the channels' reason: a record's age cannot
//	           tell a seat still in the company from one that left, so an
//	           age would forget a mailbox that still exists and leave it
//	           retaining mail for a seat nobody runs, with nothing left to
//	           retire it
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
	setupSuffix        = "_setup_once"
	attemptsSuffix     = "_iam_attempts"
	ledgerSuffix       = "_ledger"
	cooldownSuffix     = "_cooldowns"
	statusSuffix       = "_status"
	configSuffix       = "_config"
	budgetSuffix       = "_budgets"
	channelSuffix      = "_channels"
	followsSuffix      = "_follows"
	firesSuffix        = "_fires"
	runsSuffix         = "_sandbox_runs"
	secretsSuffix      = "_secrets"
	integrationsSuffix = "_integrations"
	mailboxesSuffix    = "_mailboxes"
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

	// SetupOnceRetention is how long a spent setup-callback state stays
	// recorded. Its own bucket because it is three times ClaimTTL, and a
	// claim cannot outlive the bucket it is written to.
	SetupOnceRetention time.Duration

	// AttemptWindow is how long one failed authentication counts against
	// the caller that made it. The bucket's age IS the window: each
	// attempt is a record of its own and the broker's expiry is what ends
	// it, so nothing here sweeps a counter. The CAP on one caller's record
	// is coord.AttemptCap rather than a field here — see its doc for why
	// it is not configuration.
	AttemptWindow time.Duration

	// LedgerRetention is how long a turn completion is remembered.
	LedgerRetention time.Duration

	// FireRetention is how long a scheduled fire stays claimed. Sized from
	// the same fact as LedgerRetention — the scheduler's catchup ceiling —
	// and kept a separate knob because they are not one number.
	FireRetention time.Duration

	// FollowRetention is how long a chat thread-follow survives with no
	// activity. The bucket's age IS that horizon: every re-assert rewrites
	// the record, so the age is a true last-activity stamp.
	FollowRetention time.Duration

	// CooldownMax is the longest credential cooldown, and therefore the
	// bucket's age: a cooldown is stored as its own end instant, so the
	// bucket only has to outlive the longest one anybody sets.
	CooldownMax time.Duration

	// StatusFreshness is how long a node's apply status counts as current.
	StatusFreshness time.Duration

	// Replicas is the JetStream replica count for every bucket.
	Replicas int

	// Clustered is whether this node's broker has PEERS, which is what the
	// provisioning budgets branch on. Stated by the caller rather than
	// inferred from Replicas: a member naming peers at one replica is a
	// real deployment, and every create on it still waits on the same
	// metadata group. See [jsprovision.Clustered].
	Clustered bool
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
		{"SetupOnceRetention", c.SetupOnceRetention}, {"AttemptWindow", c.AttemptWindow},
		{"LedgerRetention", c.LedgerRetention}, {"FireRetention", c.FireRetention},
		{"FollowRetention", c.FollowRetention},
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
	setup        jetstream.KeyValue
	attempts     jetstream.KeyValue
	ledger       jetstream.KeyValue
	cooldowns    jetstream.KeyValue
	status       jetstream.KeyValue
	config       jetstream.KeyValue
	budgets      jetstream.KeyValue
	channels     jetstream.KeyValue
	follows      jetstream.KeyValue
	secrets      jetstream.KeyValue
	fires        jetstream.KeyValue
	runs         jetstream.KeyValue
	integrations jetstream.KeyValue
	mailboxes    jetstream.KeyValue

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

	rateWindow time.Duration
	freshness  time.Duration

	// attemptWindow is the age the attempts bucket was created with, held
	// so the read filters on the SAME number the broker reaps on. Two
	// sources for one window is how a record outlives the window it is
	// counted in, or is counted after the bucket has dropped its peers.
	attemptWindow time.Duration
}

var _ coord.Fleet = (*FleetStore)(nil)

// OpenFleet creates or adopts every bucket and returns the backend.
//
// Idempotent and safe to call from every node at once, like [Open]: a bucket
// that already exists is ADOPTED rather than rewritten, which is what keeps N
// booting nodes from issuing N writes against a metadata group that is still
// electing. See [openBucket] for why that is create-else-observe rather than
// CreateOrUpdate, and what a divergent retention therefore means.
//
// # One ceiling over the whole sequence
//
// The buckets below are opened one after another and each takes its own
// provisioning budget, so without a ceiling the real bound on this call is the
// PRODUCT rather than the term: a wedged cluster is rediscovered once per
// bucket, seventeen buckets in a row, and a boot that nobody meant to allow ten
// minutes gets it. Nothing declared that number, which is the shape of a limit
// that is not a decision. [jsprovision.SequenceBudget] is the decision,
// applied once here.
//
// THE COUNT IS SPELLED so the estate gate catches it: this paragraph is the
// sizing argument, and a sizing argument over the wrong number of buckets is
// worse than none — see TestEveryBucketHasALifetimeClass, which holds every
// "N buckets" in this file against the open table below.
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

	ctx, cancel := context.WithTimeout(ctx,
		jsprovision.Clustered(cfg.Clustered).SequenceBudget())
	defer cancel()

	open := func(suffix, describe string, ttl time.Duration, history uint8) (jetstream.KeyValue, error) {
		name := cfg.BucketPrefix + suffix
		bucket, facts, err := openBucket(ctx, js, cfg.Clustered, jetstream.KeyValueConfig{
			Bucket: name, Description: describe, TTL: ttl,
			History: history, Replicas: cfg.Replicas,
		})
		if err != nil {
			return nil, fmt.Errorf("coord/kv: open %s: %w", name, err)
		}
		// EVERY BUCKET, because a retention this node did not get is
		// invisible in behaviour until the day it matters — see
		// [reportRetention]. The leases bucket is the one that reports
		// its own, in [Open], because there the age is also this
		// store's arithmetic.
		reportRetention(ctx, name, ttl, facts)
		return bucket, nil
	}

	store := &FleetStore{
		rateWindow: cfg.RateWindow, freshness: cfg.StatusFreshness,
		attemptWindow: cfg.AttemptWindow,
	}
	for _, bucket := range []struct {
		into     *jetstream.KeyValue
		suffix   string
		describe string
		ttl      time.Duration
		// history is the per-record message cap, and only the attempts
		// bucket sets one: zero means the KV default of 1, which is the
		// last-write-wins register every other bucket here holds.
		history uint8
	}{
		{&store.rate, rateSuffix,
			"Crewlet notification-valve windows; the bucket TTL reaps a closed window",
			cfg.RateWindow * rateBucketFactor, 0},
		{&store.claims, claimsSuffix,
			"Crewlet inbound-delivery claims; the bucket TTL is the dedupe window",
			cfg.ClaimTTL, 0},
		{&store.setup, setupSuffix,
			"Crewlet spent setup-callback states; the bucket TTL is how long a minted state is valid",
			cfg.SetupOnceRetention, 0},
		{&store.attempts, attemptsSuffix,
			"Crewlet failed-authentication attempts; the bucket TTL is the throttle's window and " +
				"the history is the per-caller cap, which discards the oldest attempt rather than " +
				"refusing the newest",
			cfg.AttemptWindow, coord.AttemptCap},
		{&store.ledger, ledgerSuffix,
			"Crewlet turn completions; the bucket TTL is the retention horizon",
			cfg.LedgerRetention, 0},
		{&store.cooldowns, cooldownSuffix,
			"Crewlet credential cooldowns; each value carries its own end instant",
			cfg.CooldownMax, 0},
		{&store.status, statusSuffix,
			"Crewlet per-node config-apply status; a node that stops reporting ages out",
			cfg.StatusFreshness, 0},
		{&store.config, configSuffix,
			"Crewlet activation pointer; NO TTL — its revision IS the epoch", 0, 0},
		{&store.budgets, budgetSuffix,
			"Crewlet token counters; NO TTL — a cap is a ceiling for the deployment's life", 0, 0},
		{&store.channels, channelSuffix,
			"Crewlet agent-to-agent channels; NO TTL — an open ask must outlive any clock", 0, 0},
		{&store.follows, followsSuffix,
			"Crewlet chat thread-follows; the bucket TTL is the last-activity horizon",
			cfg.FollowRetention, 0},
		{&store.fires, firesSuffix,
			"Crewlet scheduled-fire claims; the bucket TTL outlasts the catchup ceiling",
			cfg.FireRetention, 0},
		{&store.runs, runsSuffix,
			"Crewlet detached sandbox runs; NO TTL — a parked run's box outlives any clock", 0, 0},
		{&store.secrets, secretsSuffix,
			"Crewlet sealed credentials; NO TTL — an expiring secret is an outage on a timer", 0, 0},
		{&store.integrations, integrationsSuffix,
			"Crewlet integration reconcile status; NO TTL, standing state rather than a short horizon", 0, 0},
		{&store.mailboxes, mailboxesSuffix,
			"Crewlet seat mailbox registry; NO TTL, a record's age cannot tell a present seat from a removed one", 0, 0},
		{&store.positions, positionsSuffix,
			"Crewlet per-node state-log positions; NO TTL — an expired position reads as a node that applied nothing", 0, 0},
	} {
		got, err := open(bucket.suffix, bucket.describe, bucket.ttl, bucket.history)
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
func (f *FleetStore) Claim(ctx context.Context, key string, now time.Time) (bool, error) {
	return claimOnce(ctx, f.claims, "claim the delivery", key, now)
}

// ClaimSetup records a spent setup-callback state and reports whether this
// caller was first.
//
// THE SAME MECHANISM ON A DIFFERENT BUCKET, and that is the entire difference:
// the horizon a claim lapses on is the bucket's age, so a caller wanting
// coord.SetupOnceRetention rather than coord.ClaimTTL needs the bucket rather
// than an argument. Written as an argument it was accepted, ignored, and
// lapsed ten minutes early.
func (f *FleetStore) ClaimSetup(ctx context.Context, key string, now time.Time) (bool, error) {
	return claimOnce(ctx, f.setup, "claim the setup state", key, now)
}

// claimOnce is first-claim-wins against one bucket.
//
// ONE IMPLEMENTATION for both registries, because the only thing that differs
// between them is which bucket — and therefore which age — the record lands
// in. Two copies would be two places for the create-not-put rule to be
// forgotten.
func claimOnce(ctx context.Context, bucket jetstream.KeyValue, what, key string,
	now time.Time) (bool, error) {

	if key == "" {
		return false, errors.New("coord/kv: a claim needs a key")
	}
	// Create is the whole mechanism: it fails when the key exists, so the
	// FIRST caller wins and every other gets ErrKeyExists. Expiry is the
	// bucket's, which means the server decides when a claim lapses and no
	// node compares its own clock to a peer's deadline.
	if _, err := bucket.Create(ctx, encodeKey(key), []byte(now.UTC().Format(time.RFC3339Nano))); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return false, nil
		}
		return false, unavailable(what, err)
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

// ---- the authentication attempts ---------------------------------------- //

// Fail records one failed authentication and reports how many are in the
// window.
//
// TWO ROUND TRIPS, and the second is the answer rather than a courtesy: a
// throttle decides on the count its own failure produced ("that was the
// fifth"), and making the caller read it back on the NEXT request would give
// it a number from before the attempt it is judging. The path is a failed
// authentication, which is rare by construction — the cost lands on the
// caller getting it wrong.
func (f *FleetStore) Fail(ctx context.Context, subject string, now time.Time) (int, error) {
	if subject == "" {
		return 0, errors.New("coord/kv: an attempt needs a subject")
	}
	// PUT, NOT CREATE: every attempt is a new REVISION of the subject's
	// record, which is what makes the bucket's own history the cap. Create
	// would refuse the second attempt, and a throttle that cannot record
	// the attempts it exists to count is no throttle at all.
	if _, err := f.attempts.Put(ctx, encodeKey(subject),
		[]byte(now.UTC().Format(time.RFC3339Nano))); err != nil {
		return 0, unavailable("record the failed attempt", err)
	}
	return f.Failures(ctx, subject, now)
}

// Failures reports how many attempts against subject are still in the window.
func (f *FleetStore) Failures(ctx context.Context, subject string, now time.Time) (int, error) {
	if subject == "" {
		return 0, errors.New("coord/kv: an attempt needs a subject")
	}
	// HISTORY, which is an ephemeral ordered consumer over ONE subject —
	// two metadata proposals on a clustered bucket, the cost this package's
	// doc names for a listing. It is paid here because the alternative
	// answers a different question: a stream's per-subject message count is
	// one request and no consumer, but it carries no instants, so the
	// window could only ever be the broker's reaping and the `now` a caller
	// passes would be a parameter one backend honoured and the other
	// ignored — which is the defect the whole of this file's claim handling
	// was just rid of. The contract keeps it off the path of every request
	// instead; see [coord.Attempts.Failures].
	history, err := f.attempts.History(ctx, encodeKey(subject))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		// NOTHING RECORDED, which is the ordinary answer: a clean
		// caller, or one whose attempts have all aged out of the
		// bucket.
		return 0, nil
	}
	if err != nil {
		return 0, unavailable("read the failed attempts", err)
	}
	cutoff := now.Add(-f.attemptWindow)
	live := 0
	for _, entry := range history {
		// A PURGE MARKER IS NOT AN ATTEMPT. Flush rolls the record up
		// and the marker it leaves is a message on the same subject, so
		// counting entries rather than puts would have a successful
		// authentication leave one failure behind it.
		if entry.Operation() != jetstream.KeyValuePut {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, string(entry.Value()))
		if err != nil {
			// UNREADABLE IS STILL AN ATTEMPT. Something wrote a
			// record here, and the direction that cannot be
			// defended is the one where a value nobody can parse
			// un-throttles the caller it was written against.
			live++
			continue
		}
		// THE INSTANT DECIDES, the way a cooldown's does: the bucket's
		// age is what keeps the record set finite and the broker reaps
		// on its own clock, while the WINDOW is judged against the
		// caller's now so every node counts the same attempts. A claim
		// cannot be read this way — see [claimOnce] — because two nodes
		// each deciding a record has lapsed would both win it; a count
		// has nothing to win.
		if at.After(cutoff) {
			live++
		}
	}
	return live, nil
}

// Flush forgets every attempt against subject.
func (f *FleetStore) Flush(ctx context.Context, subject string) error {
	if subject == "" {
		return errors.New("coord/kv: an attempt needs a subject")
	}
	// Purge, not Delete, for [FleetStore.Release]'s reason and one more:
	// Delete appends a marker and leaves every prior revision in place, so
	// the attempts it is meant to forget would still be there to count.
	if err := f.attempts.Purge(ctx, encodeKey(subject)); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("forget the failed attempts", err)
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
//
// Such a build also DROPS the key when it writes the record, because it
// re-encodes only the fields it knows: during a rolling upgrade, a charge an
// older node makes clears the stamp whether or not the scope had room. That is
// the harmless direction, and the only one available without a second key: the
// stamp is what a dashboard shows, never what the gate decides with, and the
// next refusal by an upgraded node writes it again.
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
		f.unwindOrg(ctx, tokens)
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

// PostCharge adds spend that already happened to the org's counter and the
// seat's, refusing nothing. See [coord.Budgets.PostCharge].
//
// The same two writes as an admitted [FleetStore.Charge], org first, with no
// cap to test and no refusal stamp cleared: [FleetStore.bump] carries a stamp
// through, and nothing here decided the scope had room.
func (f *FleetStore) PostCharge(ctx context.Context, agentScope string, tokens int) (coord.Spend, error) {
	if tokens <= 0 {
		return coord.Spend{OK: true}, nil
	}
	if agentScope == "" {
		return coord.Spend{}, errors.New("coord/kv: a charge needs a seat scope")
	}
	org, _, err := f.bump(ctx, coord.OrgScope, tokens, 0)
	if err != nil {
		return coord.Spend{}, err
	}
	agent, _, err := f.bump(ctx, agentScope, tokens, 0)
	if err != nil {
		f.unwindOrg(ctx, tokens)
		return coord.Spend{}, err
	}
	return coord.Spend{OK: true, OrgUsed: org.Used, AgentUsed: agent.Used}, nil
}

// unwindOrg takes back the org's half of a charge whose seat half did not land.
//
// On a context that OUTLIVES the caller's. The failure being undone is often
// the caller's own cancellation (a turn stopped mid-charge, a node draining),
// and an unwind that inherited that dead context failed with it: the company
// was billed for a round that never ran, and refused early until an operator
// reset the counter. It cannot hang in the caller's place: the client bounds
// every request made on a context with no deadline by its own API timeout.
//
// Logged rather than returned: the caller's answer is already decided, and a
// compensation that failed leaves the org over-stated, which trips the cap
// EARLY. That is the safe direction, and it is worth a line saying so rather
// than a drift nobody can later explain.
func (f *FleetStore) unwindOrg(ctx context.Context, tokens int) {
	if _, _, undo := f.bump(context.WithoutCancel(ctx), coord.OrgScope, -tokens, 0); undo != nil {
		log.ErrorContext(ctx, "coord_kv_budget_compensation_failed", "scope", coord.OrgScope,
			"tokens", tokens, "error", undo,
			"detail", "the org counter is over-stated by this charge and will refuse "+
				"early; clear it with `crewlet budgets reset`")
	}
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
//
// On a context that OUTLIVES the caller's, for the reason [FleetStore.unwindOrg]
// gives: this runs AFTER both counters have been written, so the charge is a
// fact whatever happens next, and the caller's context dying between the two
// writes and this one is ordinary — a turn cancelled, a node draining. Left on
// that context the clear failed with it, and the scope kept telling every
// dashboard it was refusing charges while it had just admitted one. It cannot
// hang in the caller's place: the client bounds a request made on a context
// with no deadline by its own API timeout.
func (f *FleetStore) clearRefusal(ctx context.Context, scope string, seen time.Time) {
	key := encodeKey(scope)
	ctx = context.WithoutCancel(ctx)
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
	if req.Expect != "" && req.ExpectAbsent {
		return coord.Activation{}, errors.New("coord/kv: an activation cannot " +
			"expect a revision and no revision at once")
	}
	// THE EXPECTATION IS RESOLVED FIRST, before anything is written: a
	// caller that has already lost the race must not leave a payload
	// behind for a revision the fleet will never point at.
	seq, err := f.expectedSeq(ctx, req)
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
	revision, err := f.flip(ctx, req, seq, raw)
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
// Zero means unconditional (see [coord.ActivationRequest.Expect]), and it is
// also what a create-only write compares against, since there is no entry to
// take a sequence from.
func (f *FleetStore) expectedSeq(ctx context.Context, req coord.ActivationRequest) (uint64, error) {
	if req.Expect == "" && !req.ExpectAbsent {
		return 0, nil
	}
	entry, err := f.config.Get(ctx, activationKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		// NOTHING TO HAVE RACED WITH. A node seeded from a file holds a
		// locally-active revision before it has published anything, and
		// treating that as a race would refuse every config write on it
		// until it did. See [coord.ActivationRequest.Expect]. It is also
		// exactly what a create-only write is waiting to see.
		return 0, nil
	}
	if err != nil {
		return 0, unavailable("read the activation to compare against", err)
	}
	var record activationRecord
	if err = json.Unmarshal(entry.Value(), &record); err != nil {
		return 0, fmt.Errorf("coord/kv: decode the activation: %w", err)
	}
	if req.ExpectAbsent {
		return 0, fmt.Errorf("%w: expected no activation, the fleet is on %s",
			coord.ErrActivationRaced, record.RevisionID)
	}
	if record.RevisionID != req.Expect {
		return 0, fmt.Errorf("%w: expected %s, the fleet is on %s",
			coord.ErrActivationRaced, req.Expect, record.RevisionID)
	}
	return entry.Revision(), nil
}

// flip writes the pointer, conditionally when there was an expectation.
//
// A CREATE-ONLY write is a Create rather than an Update: there is no sequence
// to name, and the store's own "only if this key is absent" is what makes two
// nodes both convinced the company is theirs to write resolve to one winner.
func (f *FleetStore) flip(ctx context.Context, req coord.ActivationRequest, seq uint64, raw []byte) (uint64, error) {
	switch {
	case req.ExpectAbsent:
		revision, err := f.config.Create(ctx, activationKey, raw)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) || isWrongLastSequence(err) {
				return 0, fmt.Errorf("%w: an activation was published while this "+
					"write was being prepared", coord.ErrActivationRaced)
			}
			return 0, unavailable("publish the activation", err)
		}
		return revision, nil
	case req.Expect == "":
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
				"being prepared", coord.ErrActivationRaced, req.Expect)
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
		rec, ok := decodeSecret(kve.Value())
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
	raw, err := encodeSecret(rec)
	if err != nil {
		return err
	}
	if _, err := f.secrets.Put(ctx, encodeKey(rec.Name), raw); err != nil {
		return unavailable("write the secret", err)
	}
	return nil
}

// encodeSecret refuses a row no store should hold and renders the rest.
//
// ONE CHECK FOR EVERY WRITE, conditional or not: an empty envelope is a caller
// that forgot to seal, and it is no more storable because the write was
// conditioned on a version.
func encodeSecret(rec coord.SecretRecord) ([]byte, error) {
	switch {
	case rec.Name == "":
		return nil, errors.New("coord/kv: a secret needs a name")
	case rec.Value == "":
		// An empty envelope is not an empty secret — it is a caller that
		// forgot to seal. Storing it would resolve as an empty ${VAR} on
		// every node, which is the failure this bucket exists to prevent.
		return nil, fmt.Errorf("coord/kv: secret %q has no sealed value", rec.Name)
	}
	raw, err := json.Marshal(secretRecord{
		Name: rec.Name, Value: rec.Value, KeyID: rec.KeyID,
		UpdatedAt: rec.UpdatedAt.UTC(), UpdatedBy: rec.UpdatedBy, Source: rec.Source,
	})
	if err != nil {
		return nil, fmt.Errorf("coord/kv: encode the secret: %w", err)
	}
	return raw, nil
}

// CreateSecret writes a sealed value only where none is stored.
//
// A PURGED ROW IS ABSENT, which the client's Create already reads correctly: it
// re-publishes at the purge marker's revision rather than refusing a key whose
// only history is a delete. What it does not map is that re-publish LOSING — a
// second creator landing between the two — which answers a bare
// wrong-last-sequence, so that is read as the row somebody else created too.
func (f *FleetStore) CreateSecret(ctx context.Context, rec coord.SecretRecord) (bool, error) {
	raw, err := encodeSecret(rec)
	if err != nil {
		return false, err
	}
	_, err = f.secrets.Create(ctx, encodeKey(rec.Name), raw)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyExists), isWrongLastSequence(err):
		return false, nil
	default:
		return false, unavailable("create the secret", err)
	}
}

// UpdateSecret replaces a sealed value only while it is still at version.
//
// A ZERO VERSION NAMES NO ROW and writes nothing, for [FleetStore.DeleteSecretAt]'s
// reason — and here it would be worse than a no-op: the client reads an
// expected revision of zero as "this key has never been written", which is a
// create wearing an update's name, and the one row it could resurrect is one a
// removal destroyed.
func (f *FleetStore) UpdateSecret(ctx context.Context, rec coord.SecretRecord, version uint64) (bool, error) {
	raw, err := encodeSecret(rec)
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
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyExists),
		isWrongLastSequence(err):
		return false, nil
	default:
		return false, unavailable("update the secret", err)
	}
}

// DeleteSecretAt removes a value only while it is still at version.
//
// PURGED, like [FleetStore.DeleteSecret], and conditioned the way
// [FleetStore.DeleteSandboxRun] is: the client drops a LastRevision of zero and
// purges unconditionally, so a zero version is refused here rather than handed
// over as a delete of whatever is there.
func (f *FleetStore) DeleteSecretAt(ctx context.Context, name string, version uint64) (bool, error) {
	if name == "" {
		return false, errors.New("coord/kv: a secret needs a name")
	}
	if version == 0 {
		return false, nil
	}
	err := f.secrets.Purge(ctx, encodeKey(name), jetstream.LastRevision(version))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound),
		isWrongLastSequence(err):
		return false, nil
	default:
		return false, unavailable("delete the secret", err)
	}
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
	return f.listChannels(ctx, coord.Channel.Open)
}

// AllChannels returns every channel this store still holds, by id.
func (f *FleetStore) AllChannels(ctx context.Context) ([]coord.Channel, error) {
	return f.listChannels(ctx, func(coord.Channel) bool { return true })
}

// listChannels walks the bucket and keeps what matches.
//
// ONE WALK FOR BOTH LISTINGS, so the decode, the key check and the ordering
// cannot drift between them — which is how one of two near-identical loops
// stops handling an undecodable key the way the other does.
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
	if version == 0 {
		// NO VERSION IS A LOST RACE, never an unconditional write. The
		// client reads an expected revision of 0 as "the key must not exist
		// yet", so passing one through CREATED a run for a caller that never
		// read one, where the contract and the memory twin both refuse.
		return false, nil
	}
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
	if version == 0 {
		// The client drops a LastRevision of 0 and purges unconditionally,
		// so a caller that never read a version deleted whatever was there:
		// a live run, and with it the only record that its box exists.
		return false, nil
	}
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
