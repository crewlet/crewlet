package coord

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/textcut"
)

// The FLEET-SHARED state, beyond ownership.
//
// A lease answers "who runs this seat". These eight answer the other questions
// a fleet has to agree on, and they are here — beside [Backend], certified by
// the same suite — for one reason: THEY WERE ON THE NODE'S OWN DATABASE, and
// internal/store is documented "one file, one process". Every one of them was
// therefore per-node while its own comments described a fleet:
//
//   - The notification valve called itself "the shared fixed-window counter",
//     so a company on four nodes ran four valves and a seat could emit four
//     times its configured rate.
//   - The webhook dedupe registry claimed first-claim-wins across a company.
//     A third-party app retrying a delivery to a different ingress node
//     found no claim, and the same push woke the same seat twice.
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
//   - A detached coding run outlives its turn, its process and sometimes its
//     node, and is recovered by whichever node owns the seat NEXT. That node
//     read its own database, found nothing, and left a billed sandbox running
//     with a suspended conversation nothing could re-enter.
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
	// third-party app's own retry schedule. Those back off for far longer, in
	// minutes to hours, and only fire when the API layer failed to answer
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

	// SandboxRunRetention is absent for the same reason as the channel
	// bucket's, one step sharper: a detached coding run can sit parked on
	// a person's answer for DAYS (see sandbox.StatusAwaiting), and its
	// record is the only thing that knows a billed box exists. A bucket
	// age would reap it and leak the box for ever. The run's own reaper
	// and its terminal delete are what end it.

	// ChannelRetention is deliberately absent too, and for a third
	// reason: a channel's bucket can have NO age at all because an OPEN
	// channel must survive however long its ask goes unanswered. A bucket
	// TTL cannot tell an open record from a closed one, so it would reap
	// the authorization row out from under an answer still in flight —
	// which the responder then reads as "no such channel". Closing an idle
	// channel and deleting a closed one are BOTH decisions, taken by the
	// maintenance sweep against [Channels.OpenChannels] and
	// [Channels.PurgeChannels], not by a broker's clock.

	// FollowRetention is how long a seat's chat thread-follow survives
	// with no activity, and the bucket's own age is what enforces it: every
	// re-assert — a mention, a collective address, the seat posting into
	// the thread — rewrites the record, so the age IS a last-activity
	// stamp and no sweep has anything to delete.
	//
	// Ninety days is the point past which a chat thread has stopped being
	// a live conversation on every backend that ships one: Slack and
	// Mattermost both surface a quarter-old thread only through search.
	//
	// The asymmetry decides the value. Dropping a stale follow costs at
	// most one missed NON-mention reply, and the very next mention
	// re-follows through the ordinary path — while keeping every follow
	// for ever costs unbounded growth on a record read on the hot path of
	// every inbound chat message. A cheap, self-healing miss beats an
	// unbounded read.
	FollowRetention = 90 * 24 * time.Hour

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
	// PROCESS the delivery. A third-party app's push suppressed because the
	// store blinked is a wake that never happens, and nothing else will
	// notice; a duplicated wake is a turn the completion ledger collapses.
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
	// FAILS OPEN in BOTH directions, a property paid for twice: not
	// knowing whether work was done has one safe answer and it is the
	// pre-ledger one — do the work. A read that
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

	// RefusedAt is when this scope last turned a charge away, and zero
	// once it has admitted one since.
	//
	// It is what "exhausted" means, and Used compared against the cap is
	// not: a refused charge increments nothing, so a seat charged in
	// 3 000-token rounds against a 100 000 cap stops near 99 000 and never
	// reads as full. Kept HERE, in the shared counter, because the refusal
	// is the gate's own decision and every node reports this counter: a
	// stamp one node kept in memory would appear and vanish on a dashboard
	// as the reports of different nodes arrived.
	//
	// Cleared by an ADMITTED charge and by nothing weaker, plus a
	// [Budgets.Reset], which drops the scope's whole record and this stamp
	// with it — an operator zeroing a counter has made room, so a scope
	// still listed as refusing would be one nothing could clear. A charge
	// that was refused overall leaves every other scope's stamp alone, even
	// when that scope would have had room, so the answer does not depend
	// on which scope a backend happens to test first. A [Budgets.PostCharge]
	// neither stamps nor clears it: it is not a decision about room.
	RefusedAt time.Time
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
	//
	// A refusal stamps the refusing scope's [Usage.RefusedAt], and an
	// admitted charge clears the stamp on both scopes it charged. See
	// that field for why nothing weaker clears it.
	Charge(ctx context.Context, agentScope string, tokens, orgLimit, agentLimit int) (Spend, error)

	// PostCharge adds spend that has ALREADY HAPPENED to the seat's counter
	// and the org's, and never refuses. The answer is OK with both counters
	// after the write, for the caller to compare with its caps — except for
	// a charge of nothing, which writes nothing and answers OK with both
	// figures at zero rather than reading two counters to report what it
	// did not change.
	//
	// Charge is the gate: it decides whether a round may run, before the
	// round has spent anything. Some spend is only known after it happened
	// (a detached coding run is collected minutes or hours after it
	// started, possibly on another node), and no answer can un-spend it.
	// Put through the gate, it was recorded NOT AT ALL whenever it did not
	// fit, which is exactly when a cap binds: the counter under-stated the
	// company's spend by the whole run, and the next round was admitted
	// against room the run had already used.
	//
	// It leaves both scopes' refusal stamps alone, because it is not a
	// decision about room: it neither says the gate turned a charge away
	// nor that it had room for one. A counter it takes past a cap is
	// refused by the next Charge, which stamps it then.
	//
	// All or nothing, as Charge is: an error takes the org's half back, so
	// a caller that retries does not count the company twice. The
	// compensation is the same BEST-EFFORT one Charge's is — two keys and
	// no transaction — and a backend that cannot make it says so in its log
	// rather than in the answer, because the caller's answer is already
	// decided. It errs in the one safe direction: the org reads HIGH, so a
	// cap trips early rather than late.
	PostCharge(ctx context.Context, agentScope string, tokens int) (Spend, error)

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

// MaxApplyErrorLength bounds, in BYTES, the failure text a node publishes on
// its apply status, the marker of a cut included.
//
// A wrapped error chain from a failed apply is a sentence or two; a driver's
// own message with a query in it can be kilobytes, and this value is read by
// every peer on every reconcile tick AND rendered in the fleet view. 2000
// bytes keeps a real diagnosis intact and stops one node's stack trace
// becoming a download for everyone else.
//
// # Where the whole text is
//
// Not here. This record is the LIVE copy, and it is cut because its size is
// paid on every tick by every reader. The node publishes the same failure text
// in the same step on its config_revision_applied event, which goes to the
// audit event log and is kept for the event store's retention horizon; that
// copy is bounded only by
// [github.com/crewlet/crewlet/internal/events.MaxDiagnosticBytes] (64 KiB),
// and is marked where that bound cuts it. The engine's reconciler publishes
// both, and docs/concepts/control-plane.md shows how to read the event back.
//
// Applied by the BACKEND, not the caller, and asserted by the contract suite:
// a bound one implementation enforced and another did not would be a fleet
// view that worked until the node with the long error happened to be on the
// other backend.
const MaxApplyErrorLength = 2000

// TruncateApplyError applies [MaxApplyErrorLength], marking a cut with "…".
//
// WITHIN THE BOUND, marker included ([textcut.Within]), because the bound is
// a ceiling the contract suite asserts on what a backend stores rather than a
// guide to how much content to keep.
//
// NEVER THROUGH A RUNE. A plain byte slice splits whatever multi-byte
// character straddles the cut and yields invalid UTF-8, which the KV's JSON
// encoding then replaces with U+FFFD — so a driver error carrying a non-ASCII
// path or an accented message reached the fleet view garbled rather than
// merely shortened.
func TruncateApplyError(detail string) string {
	return textcut.Within(detail, MaxApplyErrorLength)
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
	Status string
	// Error is the apply's failure text as the backend stored it: at most
	// [MaxApplyErrorLength] bytes, ending in "…" where it was cut. See that
	// bound for where the whole text is.
	Error     string
	UpdatedAt time.Time
}

// ActivationRequest is one activation, and what the caller believed it was
// replacing.
//
// A STRUCT rather than five positional arguments, because the fifth was the
// one that changed the meaning of the call: an Activate that expects nothing
// and an Activate that expects a particular revision are different operations,
// and a bare string in the fifth position is how a caller passes the wrong one
// without noticing.
type ActivationRequest struct {
	// RevisionID is what the fleet will be pointed at. Required.
	RevisionID string

	Summary string

	// Payload is the SEALED body, travelling with the pointer so a peer
	// can apply the revision it names.
	Payload []byte

	At time.Time

	// Expect is the revision the caller read before building this one.
	//
	// EMPTY IS UNCONDITIONAL, and that is a real posture rather than a
	// missing value: a node publishing its own locally-active revision at
	// boot is ASSERTING, not editing — it has nothing to have raced with
	// and no earlier state to compare against. An editor always has one,
	// and passing it is what makes a concurrent edit visible.
	//
	// # An UNSET pointer is not a race, whatever Expect says
	//
	// The comparison is "if a pointer exists, it must still name this".
	// With no pointer at all there is nothing to have lost a race to, and
	// refusing would break the state a node reaches legitimately and often:
	// a node seeded from a file has a locally-active revision before it has
	// published anything, and every config write on it would fail until it
	// did. The lost-update this exists to catch needs a winner, and an
	// empty pointer has none.
	Expect string

	// ExpectAbsent is the create-only compare-and-set: publish this only
	// if the fleet has no activation at all.
	//
	// It is what Expect cannot say. An empty Expect is unconditional, and
	// an Expect naming a revision passes when there is no pointer, so
	// neither refuses the write that matters here: a node whose OWN store
	// is empty, writing the company it believes is the first, while the
	// fleet is already running one. A node reaches that state
	// legitimately, by joining a fleet and not having reconciled yet, or
	// by failing the best-effort copy of the pointer into its own store,
	// and its write would replace a running company outright: the company
	// name changes, so every seat id derived from it changes, and all of
	// their memory is orphaned.
	//
	// Refused with [ErrActivationRaced], like any other lost race, leaving
	// nothing behind. Set together with Expect it is a programming error
	// rather than a posture, because a write cannot have been built on a
	// revision and on nothing at once.
	ExpectAbsent bool
}

// ErrActivationRaced reports an activation whose Expect no longer matches.
//
// Its own sentinel because the caller's answer is specific: re-read the
// pointer and rebuild the edit on what is actually current. Collapsing it into
// an unavailable-store error would have an API answer 500 for something the
// operator can fix by retrying, and a store outage look like a lost race.
var ErrActivationRaced = errors.New("coord: the activation moved under this write")

// Plane is the fleet's config activation pointer and per-node apply status.
type Plane interface {
	// Activate publishes a revision's PAYLOAD and then points the fleet at
	// it, returning the activation with the epoch the store assigned.
	//
	// THE PAYLOAD TRAVELS WITH THE POINTER, and it has to: a peer applies
	// the revision the pointer names by reading it, and while the payload
	// lived only in the node's own database the peer read nothing. A live
	// config change therefore reached exactly the node it was posted to,
	// and every other node served whatever it had booted with — for the
	// life of the deployment, reporting the failure as "no such revision"
	// once per reconcile tick.
	//
	// TWO WRITES, in this order, and the order is the invariant: the
	// payload first, then the pointer. A crash between them leaves a
	// payload nothing points at, which the next activation replaces. The
	// other order points the fleet at bytes no node can read — the exact
	// thing the seeding path's own comment says must never happen.
	//
	// The flip itself is still ONE write: the pointer key's new revision
	// IS the epoch, so there is no window in which a node can read an
	// epoch whose pointer has not been published, and no way for two
	// concurrent activations to be handed the same number.
	//
	// The payload is whatever the caller sealed. This store never opens it
	// — a node reads it with the Tier A keyring it was deployed with, and
	// the coordination store holds ciphertext exactly as the node's own
	// database does.
	//
	// # COMPARE-AND-SET, when the caller says what it was editing
	//
	// [ActivationRequest.Expect] names the revision the caller read before
	// building this one. There is no leader here — any node's API may
	// write — so without it two operators editing at the same moment both
	// succeed and the later write silently wins. With it the loser gets
	// [ErrActivationRaced] and can re-read, which is the whole difference
	// between a lost edit and a 409.
	//
	// [ActivationRequest.ExpectAbsent] is that same guard for a write built
	// on NOTHING: it lands only while the fleet has no activation, so a
	// node that has not caught up cannot replace the company the fleet is
	// running with the first one it is handed.
	Activate(ctx context.Context, req ActivationRequest) (Activation, error)

	// Payload returns the sealed payload of the revision the fleet is
	// pointed at, and false when the store holds a DIFFERENT revision's.
	//
	// Only the current one is kept. A node that has fallen behind needs
	// exactly the revision the pointer names — never an older one — so a
	// per-revision history here would be unbounded growth in a bucket with
	// no retention, for rows nothing would ever read. Each node's own
	// company_config table is still where its history and its diffs live.
	//
	// RAISES rather than answering false on an unreachable store, for the
	// same reason [Plane.Target] does: "the fleet has a revision I cannot
	// build" and "I cannot reach the store" send a node down opposite
	// paths.
	Payload(ctx context.Context, revisionID string) ([]byte, bool, error)

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

	// AllChannels returns every channel this store still holds, open and
	// closed alike, by id.
	//
	// SEPARATE FROM OpenChannels RATHER THAN A FLAG ON IT, because the two
	// have opposite correctness rules and one signature would let a caller
	// pick the wrong one. The idle sweep must see ONLY the open ones — a
	// closed channel re-reported is a second close for one channel — while
	// a READ SURFACE must see both, or the record a company keeps until
	// the purge horizon is one nothing can ever show. The retained history
	// was reachable only through the event log, one event at a time.
	//
	// Bounded by the same purge the open listing is: a closed channel is
	// deleted once it is older than the horizon, so this is the same walk
	// with one fewer predicate rather than an unbounded one.
	AllChannels(ctx context.Context) ([]Channel, error)

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
// what it dispatched — the same split migration 0011 made for the token
// counter. What the fleet has to agree on is "may I start", and nothing more.
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

// Record is one stored value with the version it was read at.
//
// The version is an OPAQUE token: pass back exactly what a read handed you.
// It is what makes a read-modify-write safe without a transaction — the write
// lands only if nothing has changed since — and it is the whole of the
// concurrency story [SandboxRuns] offers, because a detached run's mutations
// are conditional flips whose conditions are the run's OWN fields.
type Record struct {
	Key     string
	Value   []byte
	Version uint64
}

// SandboxRuns is the fleet's record of detached coding runs.
//
// THE ONE CONTRACT HERE WHOSE VALUE IS OPAQUE, and the reason is that its
// record has fields of which coordination understands none: the suspended
// executor conversation, the box and command ids, the brief, the question a
// person is being asked. Every decision taken on those fields —
// the at-most-once tail claim, the epoch fence, the conditional pause expiry —
// is sandbox's, lives in internal/sandbox where its suite is, and would be
// nothing but duplication here. So this holds bytes and a version, and the
// conditions are expressed as compare-and-swap by the package that owns them.
//
// It is shared because a run is DETACHED and its seat MOVES. The run outlives
// its turn, its process and sometimes its node; the node that owns the seat
// afterwards is the one that recovers it. On the node's own database that
// successor's recovery pass found nothing, so a suspended Execute
// conversation became unreachable and a billed box was neither resumed nor
// reaped — the very case internal/sandbox's release path documents as safe
// ("a detached run belongs to its row, not to this process"), which only
// holds if the row is visible to the successor.
//
// RAISES rather than answering empty, on every read: "there is no run" starts
// the work again and abandons a box, and a store that could not be read must
// never be able to say that.
type SandboxRuns interface {
	// SandboxRun reads one run's record.
	SandboxRun(ctx context.Context, turnID string) (Record, bool, error)

	// SandboxRuns returns every record, by turn id.
	//
	// Every listing this serves — the seat's busy check, the boot recovery
	// pass, the pause reaper, the clarification match — filters on fields
	// coordination cannot see, so there is one read and the caller decodes.
	// The set is bounded by the number of seats that can be mid-run at
	// once, which is what makes that affordable.
	SandboxRuns(ctx context.Context) ([]Record, error)

	// CreateSandboxRun writes a new record, reporting whether it was new.
	// A turn id that already exists is left alone: the id is the kick-off
	// turn's, so a second create is a retried launch, not a second run.
	CreateSandboxRun(ctx context.Context, turnID string, value []byte) (bool, error)

	// UpdateSandboxRun writes at a version, reporting whether that version
	// still held. False is a LOST RACE, not a failure — the caller re-reads
	// and re-decides, because the condition it evaluated may no longer be
	// true.
	UpdateSandboxRun(ctx context.Context, turnID string, value []byte, version uint64) (bool, error)

	// DeleteSandboxRun removes a record at a version, reporting whether
	// that version still held.
	DeleteSandboxRun(ctx context.Context, turnID string, version uint64) (bool, error)
}

// SecretRecord is one stored credential, as coordination holds it.
//
// SEALED BEFORE IT ARRIVES. Value is the envelope the Tier A keyring produced
// — coordination stores bytes and never holds a key, which is what lets the
// fleet's shared store carry credentials at all. KeyID rides denormalised
// beside it so a rotation sweep can find rows sealed under a retired key
// without opening any of them.
type SecretRecord struct {
	Name      string
	Value     string
	KeyID     string
	UpdatedAt time.Time
	UpdatedBy string
	Source    string
}

// Secrets is the fleet's encrypted credential store.
//
// # Why it is here and not in the node's database
//
// It was the last piece of company-wide state that was not. The
// company CONFIG already travels this way — [Plane.Activate] writes a payload
// sealed with the very same keyring into the very same bucket family, and a
// company document may itself carry credentials inline — so the secret store
// being per node was an asymmetry rather than a safeguard. A rotation reached
// the one node an operator pointed the CLI at, and every other node kept what
// it booted with until somebody noticed a seat failing to authenticate.
//
// # Coordination never sees plaintext
//
// Every value is an envelope produced by the Tier A cipher before it gets
// here, and Tier A lives on each node's disk and is never written to the
// store it opens. So the KV holds ciphertext whose key it does not have, and
// a peer that could read the bucket learns which names exist and when they
// changed — not what they are.
//
// # RAISES rather than answering empty
//
// A read that failed must never resolve as "this company has no such
// credential": that renders as an unset ${VAR}, which downstream is an empty
// string handed to a provider, which is an auth failure attributed to the
// vendor. The three-valued answer is the whole point — held, definitively
// absent, or unknown.
type Secrets interface {
	// Secret reads one sealed value.
	Secret(ctx context.Context, name string) (SecretRecord, bool, error)

	// SecretValues returns every sealed value, by name.
	//
	// ONE READ, because the engine takes a SNAPSHOT: ${VAR} expansion
	// happens per role, per provider, per MCP server, and a round trip
	// there would put the fleet's store on the path of every config read.
	SecretValues(ctx context.Context) ([]SecretRecord, error)

	// PutSecret writes a sealed value, replacing any prior one.
	//
	// LAST WRITE WINS, deliberately and unlike the sandbox runs beside it:
	// rotation is the common path, and a compare-and-swap would make two
	// operators rotating at once produce a failure for one of them rather
	// than a store holding the newer credential. Neither ordering loses a
	// secret — both values were valid when written — and the one that
	// lands is the one whose write arrived second.
	PutSecret(ctx context.Context, rec SecretRecord) error

	// DeleteSecret removes a value, reporting whether it was there.
	DeleteSecret(ctx context.Context, name string) (bool, error)
}

// Integrations is the fleet's record of where each external surface got to.
//
// # Why the fleet holds it rather than the node that produced it
//
// The reconcile loop is a worker duty, so exactly one node writes this. The
// node that READS it is usually a different one: the dashboard and the REST
// API answer from whichever node serves ingress, and an operator running
// `-roles ingress` has put those on separate hosts deliberately. On the
// producing node's own database the status would be invisible from the
// surface that exists to show it, which is the same fan-out failure the
// activation pointer beside it was built to remove.
//
// # The value is OPAQUE, like a sandbox run's and unlike everything else here
//
// A status is a phase, an actor, a sentence, a list of findings and four
// timestamps, and every one of those is a term in internal/integration's
// vocabulary. Modelling it here would put a second copy of that vocabulary in
// the one package the whole engine depends on, and the two would have to be
// kept equal forever for no reader's benefit. So this carries bytes: the
// producer marshals, the reader unmarshals, and coordination stores what it
// cannot interpret.
//
// # No retention
//
// The bucket has no age, for the reason the budget counter has none: a status
// is standing state rather than a short-horizon question. One that expired
// would make a converged integration read as one nobody has ever looked at,
// on a timer nobody chose, and the loop would then re-provision against a
// third-party app it had already agreed with. There are at most as many keys
// here as there are surfaces, so nothing grows.
type Integrations interface {
	// IntegrationStatuses returns every recorded status, keyed by the
	// surface it describes.
	//
	// RAISES rather than answering empty on an unreachable store. "No
	// surface has ever been reconciled" and "the store cannot be read"
	// send the loop down opposite paths: the first is a fleet that should
	// start converging, and the second is one that must conclude nothing
	// about a company it cannot see.
	IntegrationStatuses(ctx context.Context) (map[string][]byte, error)

	// PutIntegrationStatus records one surface's status, replacing any
	// prior one.
	//
	// LAST WRITE WINS, with no compare-and-set, and what makes that safe is
	// a LEASE rather than the duty alone.
	//
	// The reconcile loop is a singleton, so its own writes cannot race each
	// other. It is not the only writer: the dashboard records a pass's
	// outcome, stamps an endpoint and marks a disconnect, on whichever node
	// served the request. Two writers and one key with no version is a lost
	// update — a disconnect an operator asked for, overwritten by a tick
	// that read the row just before it.
	//
	// So every writer takes the surface's own provisioning lease AND STILL
	// HOLDS IT WHEN IT WRITES, and one that cannot take it does not write.
	//
	// The second clause is the one that had to be added, because "first" was
	// satisfied by the shape that lost the update: both writers took the
	// lease, ran the pass, RELEASED it, and wrote afterwards from a row read
	// before the pass began. An operator's disconnect landing in that window
	// was overwritten by a reconcile tick, the card went from Disconnecting
	// back to connected, and they pressed the button again.
	//
	// It is the same lease the pass itself runs under, which is why it is a
	// lease and not a version: the writes here are the tail of work already
	// serialized by it, and the row each writer folds into is re-read while
	// that lease is held.
	PutIntegrationStatus(ctx context.Context, kind string, value []byte) error

	// DeleteIntegrationStatus drops a surface's status once its block has
	// left the company document. It removes the RECORD and nothing at the
	// third-party app: see internal/integration's package doc for why a
	// deleted block is not a request to destroy what a pass created.
	DeleteIntegrationStatus(ctx context.Context, kind string) error
}

// MailboxRecord is the fleet's record of one agent seat's durable mailbox.
//
// # Why the fleet has to remember a mailbox at all
//
// A mailbox is a durable subscription whose name is derived from the seat's
// handle, so while the seat is in the company nothing needs a record of it:
// every node can compute the name. The moment the seat LEAVES the company that
// stops being true. The handle is gone from the org every node derives names
// from, and the subscription goes on retaining whatever is still addressed to
// the seat, for ever. A seat later added under the same handle then resumes
// that backlog under a role definition that never wrote it.
//
// So every node records a handle here BEFORE it creates the subscription, which
// makes this bucket the fleet's list of mailboxes that may exist, and the
// maintenance sweep retires the ones whose seat has been gone long enough. The
// broker can list its subscriptions too, and the sweep uses that to register a
// mailbox this bucket missed; but only a record can carry the absence stamp,
// the retirement mark and the version every writer's compare-and-set is taken
// against, which is why the record exists at all.
//
// # Coordination stores the stamps; it does not interpret them
//
// What an absence means, how long it is tolerated and when a retirement counts
// as abandoned are internal/maintenance's decisions, made against its own
// constants. This package keeps two instants and a version, and certifies only
// that they round-trip and that every write is conditional.
type MailboxRecord struct {
	// Handle is the seat's handle, and the record's key.
	Handle string

	// AbsentSince is when a sweep first found the handle missing from the
	// active revision. Zero while the seat is in it.
	AbsentSince time.Time

	// RetiringSince is when a sweep began deleting the mailbox. Zero unless
	// a retirement is in flight or was abandoned part way.
	RetiringSince time.Time

	// Version is the store's version of the record as it was read. OPAQUE,
	// like [Record.Version]: pass back exactly what a read or a write handed
	// you. Ignored by CreateMailbox.
	Version uint64
}

// Present reports whether the record describes a seat in the active revision
// with no retirement begun.
func (r MailboxRecord) Present() bool {
	return r.AbsentSince.IsZero() && r.RetiringSince.IsZero()
}

// Retiring reports whether a sweep has begun deleting the mailbox.
func (r MailboxRecord) Retiring() bool { return !r.RetiringSince.IsZero() }

// Mailboxes is the fleet's registry of seat mailboxes, behind the retirement of
// a removed seat's mailbox.
//
// # RAISES rather than answering empty, on every read
//
// "There is no record" lets a sweep conclude that nothing is left to retire,
// and lets a registering node write a fresh record over a retirement that is
// still deleting the subscription it is about to create. A store that could
// not be read must never be able to say either.
//
// # COMPARE-AND-SET on every change
//
// The writers are real and concurrent: every node registers the seats of the
// revision it applied, and the sweep marks, retires and deletes. A write
// carries the version its caller read, a false answer is a LOST RACE to re-read
// and re-decide rather than a failure, and the version a successful write
// returns is the one the caller's next write must carry. Nothing here is last
// write wins, because the one lost update that matters is a returning seat's
// registration overwritten by a sweep that read the record a moment earlier.
//
// # No retention
//
// The bucket has no age, for the channel bucket's reason: a record's age cannot
// tell a seat that is present from one that left, so removing a record is the
// sweep's decision rather than a broker's clock. The set is bounded by the
// handles a company has ever used rather than by anything that grows per event.
type Mailboxes interface {
	// Mailbox reads one record.
	Mailbox(ctx context.Context, handle string) (MailboxRecord, bool, error)

	// Mailboxes returns every record, ordered by handle, so two backends
	// answer a sweep in the same order.
	Mailboxes(ctx context.Context) ([]MailboxRecord, error)

	// CreateMailbox writes a new record and returns it as stored, with the
	// version its next write must carry. A handle that already has a record
	// is left alone and reports false: the existing record may be mid-way
	// through a retirement, and only a conditional update may change it.
	CreateMailbox(ctx context.Context, rec MailboxRecord) (MailboxRecord, bool, error)

	// UpdateMailbox writes rec at rec.Version and returns it as stored,
	// reporting false when that version no longer holds, including when the
	// record is gone.
	UpdateMailbox(ctx context.Context, rec MailboxRecord) (MailboxRecord, bool, error)

	// DeleteMailbox removes a record at a version, reporting whether that
	// version still held. A record deleted this way can be created again.
	DeleteMailbox(ctx context.Context, handle string, version uint64) (bool, error)
}

// Fleet is a backend that serves all of the shared state, which is what the
// contract suite certifies and what the engine wires from.
//
// One interface at the CONSTRUCTION seam and a narrow one at each call site:
// the webhook edge takes a Claims and nothing else, the valve takes a Counter,
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
	Follows
	Fires
	SandboxRuns
	Secrets
	Integrations
	Mailboxes
	PositionRegister
	HoldRegister
	FloorRegister
	BackupRegister
	MaintenanceRegister
}

// Follows is which chat threads each seat is following.
//
// # Why this is company-wide rather than a node's own record
//
// No chat backend exposes per-bot thread subscription state, so this IS that
// state — and the node that writes it is rarely the node that reads it. An
// inbound chat message is claimed and parsed by ONE node of the fleet, chosen
// by a competing-consumer group, and the next reply in the same thread is
// claimed by whichever node wins that time. A follow held in the node's own
// database is therefore a follow the next reply's node cannot see, so a thread
// reply that is not a mention reaches its seat only by chance — and the more
// nodes a company runs, the less often that is.
//
// It lived in the node's own database until it did not, which is the shape
// migration 0012 moved `a2a_channels` out for and the one node migration 0010
// moved four tables out for before that. See ADR-0003.
//
// # Three methods, and no purge
//
// Retention here is the BUCKET's age, like every other aged slot: every
// re-assert rewrites the record, so the age is a true last-activity stamp and
// the broker expires what has gone quiet. A sweep would have nothing to delete.
type Follows interface {
	// Follow records that a seat follows a thread, or refreshes an
	// existing follow's reason.
	//
	// The reason is OVERWRITTEN rather than kept: a seat first pulled into
	// a thread by a collective shout and later named personally is now
	// following for the stronger reason, and an operator asking why it
	// answered should see the mention rather than the shout that happened
	// to come first.
	Follow(ctx context.Context, backend, handle, channel, thread, reason string, at time.Time) error

	// Following reports why a seat follows a thread, and whether it does.
	//
	// THREE ANSWERS, not two. A reason and true is a follow; an empty
	// reason and false is definitively not following; an error is UNKNOWN
	// and says nothing about either. The caller decides what to do with
	// the third — internal/notify fails closed, because a missed thread
	// reply is quiet and self-healing where a spurious wake is a burst of
	// turns that cannot be taken back.
	Following(ctx context.Context, backend, handle, channel, thread string) (string, bool, error)

	// Unfollow drops a follow, reporting whether one was there.
	//
	// The counterpart of an explicit subscription: a seat told to stop
	// watching a thread must actually stop, and waiting out the retention
	// horizon is not stopping.
	//
	// SERIALIZABLE, which is the property both backends have to reach by
	// different means: `true` is reported exactly when this call removed
	// something, and every outcome is one some sequential order of the
	// concurrent calls would have produced. A follow re-asserted while an
	// unfollow is in flight is therefore removed, exactly as it is when the
	// re-assert loses by a nanosecond — and the next mention re-follows
	// through the ordinary path.
	Unfollow(ctx context.Context, backend, handle, channel, thread string) (bool, error)

	// FollowIfAbsent records a follow only where none exists, reporting
	// whether this call created it.
	//
	// THE ONE-TIME HANDOFF'S WRITE — see internal/notify/followsync — and
	// create-only is what makes it safe to run while inbound chat is live.
	// A plain Follow would overwrite whatever the fleet already holds: a
	// stale local row landing on top of a fresh mention downgrades the
	// reason an operator reads, and one whose fleet copy was unfollowed
	// after the move began would be resurrected by a node that booted late.
	//
	// It is also what makes two nodes handing off at once correct with
	// nothing agreed between them: both hold their own local table, the
	// keys overlap, exactly one create wins, and the loser removes its own
	// row having learned the fleet already has the record.
	FollowIfAbsent(ctx context.Context, backend, handle, channel, thread, reason string, at time.Time) (bool, error)
}

// SortUsage puts the org counter first, then the seats by scope.
//
// Shared by the backends rather than left to each: "org" does NOT sort before
// "agent:…" alphabetically, so a backend that just sorted would put the
// company's own counter in the middle of its seats — and a listing whose order
// differed between backends would make a diff of two captures unreadable.
func SortUsage(rows []Usage) {
	// The org counter ranks 0 and everything else 1, so cmp.Or falls
	// through to the alphabetical compare only among the seats.
	rank := func(u Usage) int {
		if u.Scope == OrgScope {
			return 0
		}
		return 1
	}
	slices.SortFunc(rows, func(a, b Usage) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.Scope, b.Scope))
	})
}
