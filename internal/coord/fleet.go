package coord

import (
	"context"
	"sort"
	"time"
)

// The FLEET-SHARED state, beyond ownership.
//
// A lease answers "who runs this seat". These seven answer the other questions
// a fleet has to agree on, and they are here — beside [Backend], certified by
// the same suite — for one reason: THEY WERE ON THE NODE'S OWN DATABASE, and
// internal/store is documented "one file, one process". Every one of them was
// therefore per-node while its own comments described a fleet:
//
//   - The notification valve called itself "the shared fixed-window counter",
//     so a company on four nodes ran four valves and a seat could emit four
//     times its configured rate.
//   - The webhook dedupe registry claimed first-claim-wins across a company.
//     A vendor retrying a delivery to a different ingress node found no
//     claim, and the same push woke the same seat twice.
//   - The turn-completion ledger is what stops a redelivered trigger being
//     worked again. A redelivery that landed on a peer had no ledger to
//     consult.
//   - The config plane's apply status said it was "read back by every peer on
//     every reconcile tick AND rendered in the fleet view". Each node was
//     reading its own row and drawing a fleet of one.
//   - The agent-to-agent channel is an AUTHORIZATION record, read by the node
//     that owns the answering seat — which is precisely the node that did not
//     write it. A cross-node ask therefore woke its target and then dropped
//     the reply, so A2A worked only when both seats happened to land together.
//   - The scheduled-fire claim is what makes a cron dispatch at-most-once. The
//     scheduler is a singleton DUTY, so it moves; the new holder read an empty
//     ledger and its catchup pass re-fired what the old one had claimed.
//
// None of these is a lease: nothing here is owned, held or fenced. They are
// counters, claims and records, and the coordination store is simply where a
// fleet keeps what a fleet has to share.
//
// # Every contract here fails in a stated direction
//
// A coordination store that cannot be reached must not be able to invent a
// decision. Each interface says what an error means for its caller, because
// the safe direction is different for each: a dedupe claim that cannot be
// read must NOT suppress the delivery, while a rate valve that cannot be read
// must not open the floodgates.

// The bucket retentions.
//
// Each is a BUCKET's age rather than a per-call TTL, because that is what a
// KV backend can actually enforce — see internal/coord/kv's package doc for
// the measurement — so each is fixed when the bucket is created and a caller
// asking for something longer has to be refused rather than quietly clamped.
//
// They live here, beside the contract, so there is ONE place to read what a
// fleet retains. Each is sized from the cadence of the subsystem that reads
// it, named below; a backend takes them as configuration rather than
// defaulting them, because a bucket created with the wrong age is wrong for
// its lifetime.
const (
	// RateWindow is the notification valve's window, and the unit an
	// operator's `notification_rate_limit: 5` is written in: five per seat
	// per SECOND. Wider would let a genuine loop run longer before
	// tripping — two seats answering each other saturate a second easily
	// — and narrower would trip on an ordinary burst from one push.
	RateWindow = time.Second

	// ClaimTTL is how long an inbound delivery stays claimed.
	//
	// Sized to cover queue redelivery and an operator's replay, NOT a
	// vendor's own retry schedule — those back off for far longer (Plane
	// starts at ~600 s) and only fire when the API layer failed to answer
	// 2xx, which is exactly when the delivery was never claimed at all.
	// Too long is visible in one direction only: a deliberate replay ten
	// minutes later vanishes into a claim nothing will clear.
	ClaimTTL = 5 * time.Minute

	// LedgerRetention is how long a turn completion is remembered. It has
	// to outlast both the queue's redelivery horizon and the scheduler's
	// catchup window: a record deleted while a tick could still evaluate
	// lets that fire run twice, which is the one thing the ledger exists
	// to prevent.
	LedgerRetention = 7 * 24 * time.Hour

	// FireRetention is how long a scheduled fire stays claimed.
	//
	// The SAME horizon as LedgerRetention and for the same reason — both
	// have to outlast the scheduler's catchup ceiling, because a claim
	// expiring inside the window a tick can still evaluate lets that fire
	// run a second time. A separate constant rather than an alias: they are
	// sized from one fact but they are not one number, and a future change
	// to the redelivery horizon must not silently move the scheduler's
	// floor with it.
	FireRetention = 7 * 24 * time.Hour

	// CooldownMax is the longest credential cooldown anything sets, and
	// therefore the bucket's age. A cooldown carries its own end instant,
	// so the bucket only has to outlive the longest one.
	CooldownMax = 24 * time.Hour

	// BudgetRetention is deliberately absent, and the absence is the
	// point: a token cap is a ceiling for the LIFE of a deployment, so
	// the counter's bucket has no age at all. A counter that rolled itself
	// over would silently re-arm a company somebody had stopped on
	// purpose, on a horizon nobody chose. Clearing one is an operator
	// action — see [Budgets.Reset].

	// ChannelRetention is deliberately absent too, and for a third
	// reason: a channel's bucket can have NO age at all because an OPEN
	// channel must survive however long its ask goes unanswered. A bucket
	// TTL cannot tell an open record from a closed one, so it would reap
	// the authorization row out from under an answer still in flight —
	// which the responder then reads as "no such channel". Closing an idle
	// channel and deleting a closed one are BOTH decisions, taken by the
	// maintenance sweep against [Channels.OpenChannels] and
	// [Channels.PurgeChannels], not by a broker's clock.

	// StatusFreshness bounds how old a node's apply status may be and
	// still count. A node that stops reporting must VANISH from the fleet
	// view rather than linger as a healthy row nobody is writing, and the
	// bucket's own expiry is what does that.
	StatusFreshness = 4 * ReconcileInterval

	// ReconcileInterval is how often a node republishes its apply status.
	// It is the configplane's cadence, restated here because
	// StatusFreshness is a multiple of it and a package that imported the
	// configplane for one constant would invert the dependency.
	ReconcileInterval = 15 * time.Second
)

// Counter is the fleet's shared fixed-window counter, behind the notification
// valve.
//
// A FIXED window, not a sliding one: the operator's number reads as "N per
// seat per second", and the arithmetic a fixed window needs is one increment
// against one key — which is the only shape that stays correct when four
// nodes increment it at once.
type Counter interface {
	// Allow increments the bucket's count for the window containing now
	// and reports whether it stayed within limit.
	//
	// FAILS CLOSED: an error means "not allowed", because the valve exists
	// to stop a loop and a store outage is exactly when a loop is most
	// likely to be what is wrong. The caller logs and drops the
	// notification; nothing here retries.
	Allow(ctx context.Context, bucket string, limit int, window time.Duration, now time.Time) (bool, error)
}

// Claims is the fleet's first-claim-wins registry with a TTL, behind webhook
// deduplication.
type Claims interface {
	// Claim records key and reports whether THIS caller was first.
	//
	// FAILS OPEN — an error yields (false, err) and the caller must
	// PROCESS the delivery. A vendor's push suppressed because the store
	// blinked is a wake that never happens, and nothing else will notice;
	// a duplicated wake is a turn the completion ledger collapses.
	Claim(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error)

	// Release drops a claim, so a deliberate replay of the same delivery
	// is not suppressed by the first attempt's own record.
	Release(ctx context.Context, key string) error
}

// Ledger is the fleet's record of work already done, behind the
// turn-completion guard.
type Ledger interface {
	// Worked returns the subset of keys already recorded under scope.
	//
	// FAILS OPEN in BOTH directions, which is the property the Python
	// engine paid for twice: not knowing whether work was done has one
	// safe answer and it is the pre-ledger one — do the work. A read that
	// failed closed would make a store blip look like a company that had
	// already answered everything.
	Worked(ctx context.Context, scope string, keys []string) (map[string]bool, error)

	// Record marks one key worked. Best effort, for the same reason.
	Record(ctx context.Context, scope, key, detail string, at time.Time) error
}

// Cooldowns is the fleet's shared credential cooldown ledger.
//
// A key a peer found rate-limited is a key this node should not spend a call
// discovering is rate-limited. It is deliberately NOT on the hot path: see
// [Cooldowns.Since], which a pool refreshes on a ticker rather than reading
// per request.
type Cooldowns interface {
	// Cool records that a credential is unusable until the given instant.
	// Best effort: a cooldown that did not propagate costs one peer one
	// wasted call, which is what the situation cost before this existed.
	Cool(ctx context.Context, key string, until time.Time) error

	// Since returns every cooldown recorded across the fleet that has not
	// yet lapsed, as key to the instant it lifts.
	//
	// An error yields no cooldowns, which reads as "nothing is cooled" —
	// the pre-sharing behaviour, and the one that cannot make a healthy
	// fleet refuse to use any of its credentials.
	Since(ctx context.Context, now time.Time) (map[string]time.Time, error)
}

// OrgScope is the company-wide token counter's key.
const OrgScope = "org"

// AgentScope is one seat's counter key.
//
// Keyed on the DERIVED agent id rather than the handle, matching the diary and
// the episodes: renaming a handle then starts a fresh budget rather than
// inheriting the spend of whoever held the name before.
func AgentScope(agentID string) string { return "agent:" + agentID }

// Spend is what one charge did.
type Spend struct {
	// OK is false when a scope refused. RefusedScope, RefusedUsed and
	// RefusedLimit then say WHICH and by how much — "the company is out"
	// and "this seat is out" send an operator to different places, and a
	// bare refusal sends them to neither.
	OK           bool
	RefusedScope string
	RefusedUsed  int
	RefusedLimit int

	// OrgUsed and AgentUsed are the counters after a successful charge.
	OrgUsed   int
	AgentUsed int
}

// Usage is one scope's counter, for the operator surface.
type Usage struct {
	Scope     string
	Used      int
	UpdatedAt time.Time
}

// Budgets is the fleet's token counter.
//
// USAGE IS SHARED, CAPS ARE NOT. A cap belongs to a config epoch — a revision
// that raises a ceiling takes effect on the next turn — while the counter has
// to be one number across the fleet, because per-node counters mean N nodes
// each spend the whole allowance and an org cap of 500 000 is silently
// N x 500 000. So the limit travels IN on every call and the store holds only
// what has been spent.
type Budgets interface {
	// Charge checks and increments the seat's counter and the org's, and
	// a refusal by either leaves NEITHER charged.
	//
	// There is no transaction here — two keys, and a KV store has no way
	// to write both at once — so the atomicity is built rather than
	// borrowed: the ORG is charged first and compensated if the seat then
	// refuses.
	//
	// Org first, and not the reverse, for two reasons that point the same
	// way. It makes the refusal report ORG-FIRST for free when both scopes
	// are out of room, and "the company is out" is the fact that matters —
	// raising one seat's ceiling against an exhausted org changes nothing,
	// and an operator sent to the seat first finds that out the slow way.
	// And it puts the compensation on the path a seat refusal ALWAYS
	// takes, rather than on a race between two nodes: an unwind that only
	// a race can reach is an unwind nothing ever proves works.
	//
	// What the compensation cannot cover is a process that dies between
	// the two writes. The org is then over-stated by one round, which
	// trips the cap EARLY — the fail-closed direction, bounded by how
	// often a node dies mid-charge, and visible in the counter rather than
	// silently absorbed.
	//
	// FAILS CLOSED: an error stops the round. It is NOT a refusal, and a
	// caller must not report it as one — "the company is out of tokens"
	// is a budget event an operator acts on, and "the counter is
	// unreachable" is an outage. Money leaves the building for every
	// token, so a counter that cannot be reached must not un-cap a
	// company.
	//
	// A limit of 0 is UNLIMITED, matching the config: `token_budget: 0` is
	// how an operator says "no ceiling", and reading it as "no allowance"
	// would stop every company that never set one.
	Charge(ctx context.Context, agentScope string, tokens, orgLimit, agentLimit int) (Spend, error)

	// Used reports one scope's spend. A scope never charged has spent
	// nothing; an unreachable store is an error, never a zero.
	Used(ctx context.Context, scope string) (int, error)

	// Usage returns every counter, org first then seats by scope.
	//
	// Ordered so the operator surface does not have to sort, and so two
	// reads of an unchanged counter are byte-identical — a listing that
	// reshuffled would make a diff of two captures unreadable.
	Usage(ctx context.Context) ([]Usage, error)

	// Reset zeroes one scope, or every scope when given "", and reports
	// how many it cleared.
	//
	// An operator action, never a schedule. See [BudgetRetention]'s
	// absence above.
	Reset(ctx context.Context, scope string) (int, error)
}

// Activation is one entry of the config pointer every node converges on.
type Activation struct {
	// Epoch is the monotonic version of the pointer.
	//
	// It is the coordination store's own revision of the pointer key, not
	// a number this engine keeps: an append-only sequence the store
	// assigns is the one counter two nodes activating at the same instant
	// cannot both win.
	Epoch      int64
	RevisionID string
	At         time.Time
	Summary    string
}

// MaxApplyErrorLength bounds the failure text a node publishes.
//
// A wrapped error chain from a failed apply is a sentence or two; a driver's
// own message with a query in it can be kilobytes, and this value is read by
// every peer on every reconcile tick AND rendered in the fleet view. 2000
// characters keeps a real diagnosis intact and stops one node's stack trace
// becoming a download for everyone else.
//
// Applied by the BACKEND, not the caller, and asserted by the contract suite:
// a bound one implementation enforced and another did not would be a fleet
// view that worked until the node with the long error happened to be on the
// other backend.
const MaxApplyErrorLength = 2000

// TruncateApplyError applies [MaxApplyErrorLength].
func TruncateApplyError(detail string) string {
	if len(detail) <= MaxApplyErrorLength {
		return detail
	}
	return detail[:MaxApplyErrorLength] + "…"
}

// NodeApply is one node's last word about an epoch.
type NodeApply struct {
	NodeID string
	Epoch  int64
	// RevisionID is which revision this node applied at that epoch. It is
	// carried alongside the epoch rather than derived from it because the
	// fleet view is read while nodes are mid-transition, and a node still
	// on the previous revision is exactly what an operator is looking for.
	RevisionID string
	// Status is the apply outcome as a plain string. Not
	// configplane.ApplyStatus: the coordination contract is where the
	// engine's layers meet, and giving it a dependency on the posture
	// package would make every backend import the config plane to store a
	// word.
	Status    string
	Error     string
	UpdatedAt time.Time
}

// Plane is the fleet's config activation pointer and per-node apply status.
type Plane interface {
	// Activate publishes a new target revision and returns it with the
	// epoch the store assigned.
	//
	// The append and the flip are ONE WRITE: the pointer key's new
	// revision IS the epoch, so there is no window in which a node can
	// read an epoch whose revision has not been published, and no way for
	// two concurrent activations to be handed the same number.
	Activate(ctx context.Context, revisionID, summary string, at time.Time) (Activation, error)

	// Target reads the pointer, reporting whether one has ever been set.
	//
	// RAISES rather than answering empty: "nothing has been activated" and
	// "the store cannot be read" send a node down opposite paths, and
	// collapsing them makes an outage look like a company with no config.
	Target(ctx context.Context) (Activation, bool, error)

	// RecordApply publishes this node's status for an epoch. The record is
	// TTL-fresh: a node that stops reporting disappears from the fleet
	// view rather than lingering as a healthy row nobody is writing.
	RecordApply(ctx context.Context, status NodeApply) error

	// Fleet returns every node's last status, freshest first.
	Fleet(ctx context.Context) ([]NodeApply, error)
}

// Channel is one agent-to-agent ask, as the FLEET records it.
//
// A record rather than a lease: nothing here is owned, held or fenced. What
// makes it shared is that the two parties are usually on different nodes. The
// requester's node opens the channel; the answer is published from whichever
// node owns the TARGET's seat, and that node has to read this record to decide
// whether the reply it is about to deliver is authorized at all. On the node's
// own database that read found nothing, so a cross-node ask was accepted, woke
// the target, and then silently swallowed its answer.
type Channel struct {
	ID        string
	Requester string
	Target    string

	// Messages counts what crossed the channel. One ask and one answer is
	// the whole protocol, so a count above two is the anomaly it looks
	// like rather than ordinary traffic.
	Messages int

	OpenedAt time.Time
	LastAt   time.Time

	// ClosedAt is zero while open. A zero time rather than a status field,
	// so "when did it close" and "is it closed" cannot disagree.
	ClosedAt time.Time
}

// Open reports whether the channel still accepts messages.
func (c Channel) Open() bool { return c.ClosedAt.IsZero() }

// Channels is the fleet's agent-to-agent authorization record.
//
// RAISES rather than answering empty, everywhere. This is the contract's
// three-valued rule at its sharpest: "no such channel" is an authorization
// refusal the requester sees as a colleague who never answered, and "the store
// could not be read" is a round to retry. A backend that collapsed the second
// into the first would turn a two-second broker blip into a company where
// every agent has stopped replying to every other one.
type Channels interface {
	// OpenChannel records a new channel, IGNORING an id that already
	// exists rather than erroring or overwriting: the id is minted per
	// ask, so a collision means a retried publish of ONE ask, and
	// overwriting would reset the counter and replace the participants of
	// a channel that is already carrying an answer.
	OpenChannel(ctx context.Context, ch Channel) error

	// Channel reads one record, reporting whether it exists.
	Channel(ctx context.Context, id string) (Channel, bool, error)

	// CloseChannel marks the channel closed and returns its stored state.
	//
	// Closing an already-closed channel returns it UNCHANGED — both
	// parties may close, and the first close is when it actually happened.
	// The caller tells the two apart by comparing the returned ClosedAt
	// against the instant it passed in, which is what stops a sweep
	// reporting a close somebody else already made.
	CloseChannel(ctx context.Context, id string, at time.Time) (Channel, bool, error)

	// CountChannelMessage increments the message counter, bumps LastAt and
	// returns the stored state.
	CountChannelMessage(ctx context.Context, id string, at time.Time) (Channel, bool, error)

	// OpenChannels returns every channel still open, by id.
	//
	// The idle sweep's read half. Only the OPEN ones, because a closed
	// channel re-reported is a second close event for one channel, which
	// draws two closes on a dashboard.
	OpenChannels(ctx context.Context) ([]Channel, error)

	// PurgeChannels deletes channels closed before cutoff, returning the
	// count. An open channel is never purged however old, or a long
	// running ask loses its authorization record while its answer is still
	// in flight.
	PurgeChannels(ctx context.Context, cutoff time.Time) (int64, error)
}

// Fires is the fleet's at-most-once guard for scheduled work.
//
// One key per DISPATCH IDENTITY — scope, owner, schedule name, the local
// wall-clock minute and the runner it was addressed to — so the guarantee the
// scheduler docs make ("a restart, a slow tick, or a re-evaluated minute can
// never fire the same run twice") holds across the fleet rather than within
// one process. It was the node's own table, so the `scheduler` singleton duty
// moving to a peer handed the new node an empty ledger and its catchup pass
// re-fired what the previous holder had already claimed: every company got two
// standups.
//
// The node's own scheduled_runs table STAYS, as this node's audit record of
// what it dispatched — the same split migration 0011 made between the shared
// token counter and the per-agent token_usage rows. What the fleet has to
// agree on is "may I start", and nothing more.
type Fires interface {
	// ClaimFire records one fire identity and reports whether THIS caller
	// wrote it.
	//
	// FAILS CLOSED, which is the opposite polarity to [Ledger] and
	// deliberately so. That one asks "has this work been done", whose safe
	// answer is to re-run; this one asks "may I start", whose safe answer
	// is to wait for the next tick. An error yields (false, err) and the
	// caller must NOT dispatch.
	ClaimFire(ctx context.Context, key string, at time.Time) (bool, error)
}

// Fleet is a backend that serves all of the shared state, which is what the
// contract suite certifies and what the engine wires from.
//
// One interface at the CONSTRUCTION seam and seven at the call sites: the
// webhook edge takes a Claims and nothing else, the valve takes a Counter,
// a turn's meter takes a Budgets. A consumer that could reach the whole store
// would eventually use it.
type Fleet interface {
	Counter
	Claims
	Ledger
	Cooldowns
	Budgets
	Plane
	Channels
	Fires
}

// SortUsage puts the org counter first, then the seats by scope.
//
// Shared by the backends rather than left to each: "org" does NOT sort before
// "agent:…" alphabetically, so a backend that just sorted would put the
// company's own counter in the middle of its seats — and a listing whose order
// differed between backends would make a diff of two captures unreadable.
func SortUsage(rows []Usage) {
	sort.Slice(rows, func(i, j int) bool {
		switch {
		case rows[i].Scope == OrgScope:
			return rows[j].Scope != OrgScope
		case rows[j].Scope == OrgScope:
			return false
		default:
			return rows[i].Scope < rows[j].Scope
		}
	})
}
