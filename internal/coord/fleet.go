package coord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/textcut"
)

// The FLEET-SHARED state, beyond ownership.
//
// A lease answers "who runs this seat". The contracts below answer the other
// questions a fleet has to agree on, and MOST of them are here — beside
// [Backend], certified by the same suite — for one reason: THEY WERE ON THE
// NODE'S OWN DATABASE, and internal/store is documented "one file, one
// process". Each of the seven was therefore per-node while its own comments
// described a fleet:
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
//     with a parked turn nothing could re-enter.
//
// None of these is a lease: nothing here is owned, held or fenced. They are
// counters, claims and records, and the coordination store is simply where a
// fleet keeps what a fleet has to share.
//
// The rest never lived on a node's own database and are here because the
// question they answer is a company's from the start: the company's sealed
// [Secrets], the [Follows] a chat thread is routed by, the [Mailboxes] a seat
// may still have, the [SeatPauses] a person set, the windowed token [Budgets]
// every node charges (budget.go), the [PositionRegister] and its neighbours
// the state log trims against, the [Rebases] a retried unit of work inherits
// its instant from, the [ObjectStores] backend the fleet's files are in, the
// [TrashReaps] account of the trash reaper's last pass, the
// [Custody] of a stateless node's event batches, the [UploadReservations]
// that hold the company's bytes before an artifact's upload moves them, and
// the [Markers] sweep that
// keeps every removal from being read again for ever. The list above is the
// migration, not the estate.
//
// The sign-in throttle is deliberately NOT here. Its curve was once shared
// through a bucket of its own, so a guessing run a load balancer rotated
// across nodes met one curve; it is each node's own now, because that sharing
// put a coordination round trip on the sign-in path to buy back a factor of N
// on a rate the curve already bounds — see internal/iam/credential.
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
// The two exceptions are the claim TTLs, [ClaimTTL] and [SetupOnceRetention]:
// what a caller hands [Claims.Claim], carried as a deadline in the claim's own
// record and bounded by the claims bucket's age, [MaxClaimTTL].
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

	// MaxRateWindow is the longest window any caller counts a [Counter]
	// over, and so how long the fleet keeps a window (a few multiples of
	// it, so a window that just closed is still readable while a straggler
	// finishes writing to it). One minute: the Dashboard Assistant's turn
	// rate is a person's questions per MINUTE (internal/assistant), counted
	// across the fleet, the widest window any caller asks for. A backend
	// refuses a wider one rather than reap it mid-window, which would
	// restart its count and pass far more than the caller asked for.
	MaxRateWindow = time.Minute

	// ClaimTTL is how long an inbound delivery stays claimed.
	//
	// Sized to cover queue redelivery and an operator's replay, NOT a
	// third-party app's own retry schedule. Those back off for far longer, in
	// minutes to hours, and only fire when the API layer failed to answer
	// 2xx, which is exactly when the delivery was never claimed at all.
	// Too long is visible in one direction only: a deliberate replay ten
	// minutes later vanishes into a claim nothing will clear.
	ClaimTTL = 5 * time.Minute

	// MaxClaimTTL is the longest TTL a [Claims] claim may ask for, and so
	// the claims bucket's age: a backend that expires a claim by its
	// bucket can only keep one for as long as the bucket keeps records, and
	// each claim shorter than that lapses at its own deadline. A claim
	// asking for more is refused with [ErrTTLTooLong] rather than clamped,
	// because a clamped dedupe window lets through the very duplicate it
	// was sized to stop.
	//
	// THIRTY MINUTES, set by the longest claim anybody takes: the
	// Mattermost socket's (mattermost.ClaimTTL), which must outlive the
	// window a reconnecting seat replays — mattermost.MaxBackfill, fifteen
	// minutes — plus the reconnect backoff ceiling and clock margin, and is
	// twice the window. This package cannot import that one, so its own
	// tests hold the two together. Longer costs only records nobody reads
	// again: every claim key names one delivery.
	MaxClaimTTL = 30 * time.Minute

	// SetupOnceRetention is how long a spent setup-callback state is
	// remembered — the TTL a setup flow takes its [Claims] claim with — and
	// therefore how long the state a setup flow mints may be valid for: the
	// record is what stops a second presentation of the same state, so a
	// token that outlived it would be replayable while its signature still
	// validates. internal/api/setupapi's manifestTTL IS this constant for
	// that reason.
	//
	// Fifteen minutes is the span of the task it covers — a browser round
	// trip to GitHub and two clicks — and it is bounded from above by a
	// fact at the vendor rather than by anything here: GitHub expires the
	// one-time code a manifest conversion returns at an hour, so a longer
	// window would let somebody complete a flow whose code is already
	// dead, and the failure they would see names the code rather than the
	// wait.
	//
	// An ordinary claim at its own TTL rather than a bucket of its own: a
	// claim carries its deadline in its record, so a horizon three times
	// [ClaimTTL]'s shares the claims bucket with it, and [MaxClaimTTL] is
	// the only bound it has to stay under — coordtest's retention guard
	// holds it there, because a claim asking for more is an error, and the
	// setup flow fails closed on every error.
	SetupOnceRetention = 15 * time.Minute

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

	// RebaseRetention is how long a recorded rebase is kept — see
	// [Rebases] — and so the bucket's age.
	//
	// THE OPERATION LEDGER'S OWN RETENTION (statelog.OpsRetention), which
	// is a day past the horizon a rebase is inherited within
	// (statelog.MintHorizon). An attempt inherits a recorded instant only
	// while it lies within that horizon of the attempt's own clock, and the
	// bucket's age is counted from the write, which is no earlier than the
	// instant it records — so a record the bucket has aged out is one the
	// rule would have replaced anyway. Shorter would forget an instant a
	// retry still has to inherit, and the retry would mint anew and write
	// again whatever the attempt before it wrote; longer keeps a record per
	// rebased unit of work that nothing can ever read. This package cannot
	// import the state log, so coordtest's retention guard holds the two
	// together.
	RebaseRetention = 30 * 24 * time.Hour

	// BudgetRetention is how long a token counter outlives its last
	// write, and the bucket's age that enforces it (ADR-0019).
	//
	// A counter is one record per scope holding a slot per calendar
	// window, and the longest window is a month: at most 31 days, plus the
	// hour a clock change can add. A record nobody has charged for longer
	// than that holds no slot for any window that is still current, so it
	// is spend nobody can be refused against and nothing will ever read
	// as current again — and without an age, every seat that ever ran
	// would keep one for the life of the deployment. Thirty-two days is
	// the first whole number of days past the longest window; the bucket
	// reaps nothing a current window counts, because a charge rewrites
	// the record and restarts its age.
	//
	// This is NOT the reset. A window's allowance comes back when the
	// window turns over, by the roll inside the charge that crosses it —
	// see [Tally.Roll] — and never on this clock: a counter that aged out
	// early would re-arm a company mid-month, and one that aged out late
	// costs a record nobody reads.
	BudgetRetention = 32 * 24 * time.Hour

	// CustodyRetention is how long the record of which data node keeps a
	// batch of a stateless node's event records lasts — see [Custody] —
	// and so the bucket's age.
	//
	// A DAY PAST THE EVENT LOG'S OWN RETENTION (store.EventRetention). A
	// node that wrote a batch and never learned whether it kept it asks
	// this record later — at its next pass, or at boot after a crash — and
	// a record aged out while the batch's rows were still in the log would
	// answer that nobody keeps it, so the node would keep a second copy of
	// a batch another node already holds. The rows go at the event log's
	// retention, so a record that outlives them by a day — the sweep's
	// slack — is never asked about a batch it no longer describes. Longer
	// keeps a record per batch nobody can ask about. This package cannot
	// import the store, so coordtest's retention guard holds the two
	// together.
	CustodyRetention = 32 * 24 * time.Hour

	// UploadRetention is how long an upload reservation's records outlive
	// their last write (see [UploadReservations]), and so the uploads
	// bucket's age.
	//
	// THIRTY HOURS: the longest a reservation must still be readable after
	// its last write is the hold it keeps on bytes nothing names yet, which
	// lasts until the object collector could have deleted them: the
	// collector's grace (a day, objstore/collect.PendingGrace), two of its
	// passes, and the transfer window an upload is given (an hour at most,
	// objstore.MaxPresignTerm) with the pace of its largest file. That is
	// twenty-seven hours and a few minutes; the rest is margin, so a record
	// the bucket ages out is one whose hold has already lapsed. Shorter would
	// forget a hold on bytes still in the store, which the ledger would then
	// stop counting; longer keeps records nobody reads. This package cannot
	// import the object store, so internal/artifactupload's tests hold the
	// two together.
	UploadRetention = 30 * time.Hour

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

	// MarkerRetention is how long the record that a key was REMOVED — a
	// delete or purge marker — stays in a store that never ages its
	// records, before [Markers.SweepMarkers] removes it.
	//
	// NOT A CORRECTNESS BOUND, and that is established rather than hoped:
	// every reader of a backend that keeps markers asks what the key holds
	// NOW before it trusts one — a listing reads a marker its pass
	// delivered again from the stream leader, and a create re-reads the key
	// before conditioning on one — so a marker swept the instant after it
	// was written changes no answer. The contract suite sweeps with a
	// cutoff in the future and holds every other answer to what it was.
	//
	// What the horizon decides is how long a removal stays VISIBLE, against
	// what that costs. Every marker still held is one leader read on each
	// listing whose pass delivers it, so a listing pays for the records
	// removed in the last horizon plus one sweep interval. And a marker is
	// the store's only trace that a key was removed rather than never
	// written, so it has to outlive every operation that could still be
	// acting on what it read before the removal: a listing runs inside the
	// tick that takes it, every read-decide-write on these records runs
	// under a claim, and no claim outlives [MaxDutyTTL]. Hence three hours —
	// every operation in flight when a key was removed has finished before
	// its marker goes, and a listing re-reads at most the removals of the
	// last three hours and a quarter, where without the sweep it re-read
	// every removal the deployment had ever made.
	MarkerRetention = MaxDutyTTL
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
	// A window wider than [MaxRateWindow] is an ERROR, never counted: the
	// fleet does not keep a window that long, so its count would restart
	// before the window closed.
	//
	// THREE ANSWERS: within the limit, over it, and — as an error — the
	// store could not say, whatever the bool beside it. CONTENTION IS
	// NEVER THE THIRD: every race a caller loses is another caller's
	// increment, so however many callers share a bucket each is answered
	// with room or with a full window. What the third answer means is the
	// caller's, and the notification path FAILS OPEN on it (notify.Valve):
	// a refused notification is dropped, not delayed, so a valve that shut
	// whenever its store could not be read dropped every seat's
	// notifications for as long as the store was away.
	Allow(ctx context.Context, bucket string, limit int, window time.Duration, now time.Time) (bool, error)
}

// Claims is the fleet's first-claim-wins registry with a TTL: behind webhook
// deduplication, the change feed's wake dedupe, the chat socket's delivery
// claim and a setup flow's one-shot callback state.
//
// # A claim carries its own deadline
//
// A claim lapses at the deadline its RECORD carries — the `now` its writer
// passed plus the `ttl` it asked for — judged against the `now` the NEXT
// caller passes, so a lapsed claim is taken over by whoever asks next and no
// node compares its own clock to the store's reaping. It cannot be a per-key
// TTL in the store: a KV's is create-only and an update clears it (see
// internal/coord/kv's package doc for the measurement), which is why a ttl
// argument this once took was validated and then ignored by the backend a
// fleet runs on, and a fifteen-minute claim taken against a five-minute bucket
// lapsed ten minutes before its caller thought it did. The suite could not see
// that, because no case ever travelled past a deadline; its claim cases travel
// the clock now. The bucket's age is the CEILING, [MaxClaimTTL], and a claim
// asking for longer is an error rather than a record the store reaps early.
//
// The POLARITY belongs to the caller rather than to this store: webhook
// dedupe fails open because a suppressed push is a wake nobody notices, while
// a setup flow's callback state is an AUTHORIZATION check and its caller fails
// closed — a registry that could not answer has not said the state is
// unspent, and the cost of refusing is one more click.
//
// The `now` a claim takes is the instant its record is MADE AT, and the
// instant an earlier record's deadline is judged against — never a deadline
// itself. A store's own reaping at [MaxClaimTTL] only ever removes a record
// whose deadline has already passed, so no caller may infer from a travelled
// clock that a record has gone, only that it no longer holds anybody off.
type Claims interface {
	// Claim records key and reports whether THIS caller was first.
	//
	// FAILS OPEN — an error yields (false, err) and the caller must
	// PROCESS the delivery. A third-party app's push suppressed because the
	// store blinked is a wake that never happens, and nothing else will
	// notice; a duplicated wake is a turn the completion ledger collapses.
	//
	// The claim lapses at now+ttl, judged against the `now` the NEXT
	// caller passes, so two callers sharing one key share one window
	// whatever else the store keeps. A ttl beyond [MaxClaimTTL] — or beyond
	// a backend's own ceiling where that is shorter — is an error wrapping
	// [ErrTTLTooLong], never a silent clamp.
	//
	// An empty key is an ERROR rather than (false, nil): a caller that
	// named nothing has not lost a race to anybody, and answering "somebody
	// else took it" would drop the delivery.
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

	// Origin is who wrote the revision, as the node that stored it first
	// recorded it. See [RevisionOrigin].
	Origin RevisionOrigin

	// Restored is whether the write that set the pointer put back a company
	// the fleet had lost rather than choosing one ([ActivationRequest.Restore]).
	// It describes this activation alone: the next write sets it again.
	Restored bool
}

// RevisionOrigin is the record a revision was WRITTEN with, travelling with
// the pointer so a peer adopting the revision keeps it.
//
// A revision is stored in the database of whichever node served the write;
// every other node meets it for the first time when the pointer names it. So
// what the pointer does not carry, a peer cannot know — and it did not carry
// this: every node but the origin recorded the revision as written by `peer`,
// from source `fleet`, at the instant it was activated, so the same revision
// had a different author on every node and the audit screen's answer depended
// on which node served it.
//
// REQUIRED: every writer knows who it is, how it made the revision and when,
// and [RevisionOrigin.Check] is what both backends hold an activation to, so
// no pointer reaches a peer naming nobody and no adopting node has an author
// to invent or a "not recorded" to render.
//
// The kind is internal/iam's [iam.ActorKind] — the one vocabulary every
// durable author column in the engine writes, the engine's own writes
// included ([iam.ActorSystem]) — and an unknown kind off the wire stays the
// value it is ([iam.ActorKind.Valid] false) rather than being mapped onto a
// kind that exists. Importing iam costs this package nothing: it is a leaf
// holding values only, so a backend carrying the word pulls in no store, no
// hasher and no client, where the store's own kind type would have made every
// backend import the store to carry a word.
//
// The PARENT is deliberately not here. It names a revision the adopting node
// may never have held — a node that joined after it was superseded — and the
// table's parent column is a foreign key into this node's own rows.
type RevisionOrigin struct {
	// Author is the name the writer was recorded under: internal/iam's
	// [iam.Actor.Name] for whoever made the revision.
	Author string

	// AuthorKind is what sort of author Author names.
	AuthorKind iam.ActorKind

	// OperatorID is the CREDENTIAL the revision was written through —
	// [iam.Actor.OperatorID]: a machine token's `pat:<id>`, a session's
	// lineage, or a login. Beside the author rather than instead of it,
	// because a person writing through their own machine token and the
	// same person signed in are one author and two credentials, and the
	// history an operator audits has to say which.
	OperatorID string

	// Source is how the revision was made: `api`, `file`, `rekey`.
	Source string

	// CreatedAt is when it was written, which a re-activation of an old
	// revision makes very different from [Activation.At].
	CreatedAt time.Time
}

// ErrIncompleteOrigin reports an activation whose origin leaves out what a
// peer adopting the revision records: the author's kind, the source or the
// creation instant. The author's NAME may be empty — its kind still says what
// sort of party wrote it — but nothing else may.
var ErrIncompleteOrigin = errors.New("coord: an activation needs its revision's origin")

// Check reports whether the origin carries everything a peer records, as an
// error wrapping [ErrIncompleteOrigin] naming each field that is missing.
//
// THE BACKEND ENFORCES IT, both of them, so the rule cannot be one a caller
// forgot: a pointer that named nobody would put a revision on every peer with
// no author and no source, which is the guess the origin exists to remove.
func (o RevisionOrigin) Check() error {
	var missing []string
	if o.AuthorKind == "" {
		missing = append(missing, "AuthorKind")
	}
	if o.Source == "" {
		missing = append(missing, "Source")
	}
	if o.CreatedAt.IsZero() {
		missing = append(missing, "CreatedAt")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: missing %s", ErrIncompleteOrigin, strings.Join(missing, ", "))
	}
	return nil
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

// ActivationAt is the instant an activation is published at: the one the
// caller asked for, or one millisecond after the activation it replaces when
// the request is not later than that.
//
// # Why the pointer forces its instants forward
//
// Because two readers decide WHICH OF TWO ACTIVATIONS IS NEWER by this instant,
// and that answer has to follow the order the pointer took them in rather than
// the clocks of the nodes that asked. The instant is the activating node's own
// clock, so an operator whose node runs a few seconds behind the one that
// activated last, a node republishing the `activated_at` it holds, and two
// activations inside one millisecond each put an instant on the pointer no
// later than the one it replaced. Each reader then got it wrong its own way:
//
//   - EVERY ROW DERIVED FROM A CONFIGURATION is stamped with it
//     (configplane.ActivationStamp) — the tracker's projects and the
//     knowledge base's containers — and a row refuses a stamp older than its
//     own, so that a node applying an older configuration late cannot walk a
//     newer one's values back. An activation whose instant was no later than
//     the stamps already on those rows was applied to neither, silently,
//     until some later activation happened to carry a later clock.
//   - THE CONTROL PLANE'S OWN BOOKKEEPING: a booting node republishes the
//     fleet's revision it last saw over a recreated pointer only when that
//     revision's instant is LATER than the pointer's, and the reconciler
//     tells the activation it applied from a new one by revision and instant
//     together (re-activating an unchanged revision is the
//     credential-rotation gesture). A node that had seen the replaced
//     revision read its own copy as the newer and published it back over the
//     fleet's.
//
// Decided INSIDE the compare-and-set that replaces the pointer, against the
// pointer it replaces, so two activations racing each other cannot both be
// told the same instant: whichever lands second is compared with the first.
//
// # At the stamp's resolution
//
// Compared in Unix MILLISECONDS, the resolution configplane.ActivationStamp
// stamps at, because an instant later by a few microseconds is the SAME stamp
// and every guard lets an equal stamp through — a finer forward step here
// would still hand those rows an activation they refuse to tell apart from the
// last. It is also coarser than every place the instant is kept — the
// pointer's nanoseconds and the node store's microseconds — so a later
// activation is still later once a node has stored its copy and read it back.
// configplane.TestTheActivationPointersForwardRuleIsLaterInStamps holds the two
// resolutions together. The zero instant — no previous activation — leaves the
// request as it is.
func ActivationAt(requested, previous time.Time) time.Time {
	if previous.IsZero() || requested.UnixMilli() > previous.UnixMilli() {
		return requested.UTC()
	}
	return time.UnixMilli(previous.UnixMilli() + 1).UTC()
}

// TruncateApplyError applies [MaxApplyErrorLength].
//
// NEVER THROUGH A RUNE. A plain byte slice splits whatever multi-byte
// character straddles the cut and yields invalid UTF-8, which the KV's JSON
// encoding then replaces with U+FFFD — so a driver error carrying a non-ASCII
// path or an accented message reached the fleet view garbled rather than
// merely shortened.
func TruncateApplyError(detail string) string {
	return textcut.Ellipsis(detail, MaxApplyErrorLength)
}

// NodeApply is one node's last word about an epoch.
type NodeApply struct {
	NodeID string
	Epoch  int64
	// RevisionID is which revision this node ATTEMPTED at that epoch —
	// applied or not, as Status says. It is carried alongside the epoch
	// rather than derived from it because the fleet view is read while
	// nodes are mid-transition, and a node still on the previous revision
	// is exactly what an operator is looking for.
	RevisionID string
	// Applied is the epoch this node last APPLIED — its reconciler's own
	// progress — or 0 before its first apply succeeds. Epoch is the one it
	// last ATTEMPTED, and the two differ exactly when Status says the
	// attempt failed: a node that applied 5 and refused 6 says Epoch 6,
	// Applied 5.
	//
	// ITS OWN FIELD rather than inferred from Epoch and Status, because the
	// inference is wrong in the case a reader is asking about: a refusal
	// says the node did not reach Epoch, never which epoch it did reach. A
	// writer that saved epoch 6 learns from this whether a node runs it —
	// the comparison a node's own health push makes with its live epoch —
	// without reading the node's own /health.
	Applied int64
	// Serving is the revision this node's engine RUNS: the one its last
	// successful apply installed, or the one it booted on; empty while it
	// runs none. A node that refused the epoch's revision goes on serving
	// the one before, and this is the row an operator reads to see it.
	//
	// NOT "SERVING DIFFERS FROM RevisionID" as the test for a refusal: a
	// refused re-activation of the revision a node already runs — the
	// credential-rotation gesture — names one revision in both. Status is
	// what says the attempt failed; Serving says what runs regardless.
	Serving string
	// Boots is the revision this node would START ON if it restarted now:
	// its boot row's, as its reconciler last read or wrote it. Empty on a
	// node that boots on nothing — one whose own database is discarded at
	// every start (a node without the `data` role), and one whose row names
	// no revision yet because it has never applied one. Never empty for a
	// row its reconciler could not read: the reconciler reads it before it
	// writes its first record, and a read that fails later leaves the last
	// answer standing.
	//
	// A FACT OF ITS OWN, never derived from Serving. The boot row moves
	// only once an apply has SUCCEEDED, and that write is local and best
	// effort: while a failed one is being retried, this node runs a
	// revision it would not come back on, and the two names differ. A
	// reader that took Serving for what a restart lands on — the
	// Kubernetes operator keeps a revision's secrets while some node still
	// names it — would drop what that node actually needs to boot, and the
	// first restart would crash-loop on a revision it can no longer build.
	Boots string
	// Status is the apply outcome as a plain string. Not
	// configplane.ApplyStatus: the coordination contract is where the
	// engine's layers meet, and giving it a dependency on the posture
	// package would make every backend import the config plane to store a
	// word.
	Status    string
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

	// Payload is the SEALED body, travelling IN the pointer's own write so
	// a peer can apply the revision it names. Required, and a JSON object —
	// a company document, which the caller has sealed into an envelope
	// that is one ([CanonicalPayload]). It is carried as JSON rather than
	// as bytes because it IS JSON: as bytes, the record encoded it as
	// base64, and the envelope's ciphertext was base64 already, so every
	// body went on the wire encoded twice and a third larger for nothing.
	Payload json.RawMessage

	// At is the instant the caller activated the revision at. The pointer
	// may carry a LATER one — see [ActivationAt] — and the [Activation]
	// Activate returns says which, so a caller keeping a local copy of the
	// instant keeps the pointer's.
	At time.Time

	// Origin is the revision's own record — its author, the author's kind,
	// the credential it was written through, its source and when it was
	// made — carried with the pointer for the reason Summary is: every node
	// that adopts the revision keeps its own copy of it, and that copy has
	// to say who wrote it. Every writer has one, and an activation whose
	// origin fails [RevisionOrigin.Check] is refused.
	Origin RevisionOrigin

	// Expect is the revision the caller read before building this one: the
	// activation lands ONLY WHILE THE POINTER NAMES IT.
	//
	// A POINTER THAT IS NOT THERE is [ErrActivationRaced] too — never set,
	// deleted, or removed by a purge — because what the caller built on is
	// gone, and the write it built is an edit of a company the fleet no
	// longer runs. Every editor reads the pointer first and passes what it
	// read: Expect when it read one, ExpectAbsent when it read none. So a
	// write cannot pass an expectation it never saw the pointer hold, and the
	// race it reports is real: the fix is to read again and rebuild.
	//
	// EMPTY IS UNCONDITIONAL, and only one caller passes it: `crewlet run
	// -import-company`, an operator replacing the company outright from a
	// file, which asserts rather than edits and has nothing to have raced
	// with. Everything else built on a pointer it read, or on its absence.
	Expect string

	// ExpectAbsent is the create-only compare-and-set: publish this only
	// if the fleet has no activation at all.
	//
	// It is what a write built WITH NO POINTER passes: a config write on a
	// node that read none, a booting node restoring the company after the
	// pointer was lost, and the bootstrap seed of a fleet's first company.
	// An empty Expect would replace whatever another node published in the
	// meantime, and the one case that matters is a company the fleet is
	// already running: a node reaches "no pointer as far as it knows"
	// legitimately, by joining a fleet before its first read, and its write
	// would replace that company outright — the company name changes, so
	// every seat id derived from it changes, and all of their memory is
	// orphaned.
	//
	// Refused with [ErrActivationRaced], like any other lost race, leaving
	// nothing behind. Set together with Expect it is a programming error
	// rather than a posture, because a write cannot have been built on a
	// revision and on nothing at once.
	ExpectAbsent bool

	// ExpectEpoch narrows Expect to the very activation the caller read: the
	// write lands only while the pointer is AT this epoch, not merely naming
	// the revision. Zero leaves Expect as it is; set without Expect it is a
	// programming error.
	//
	// The boot publish of an offline offer is the caller that needs it. It
	// records, before it publishes, the instant it will land at, so that a
	// node which crashed before recording the landing tells it by the pointer
	// carrying exactly that instant. The instant is decided against the
	// activation the write replaces ([ActivationAt]), and with Expect alone
	// another writer re-activating the same revision in between lets the
	// write land a millisecond after that one — at an instant nothing
	// recorded, so the landed publish reads as never made.
	ExpectEpoch int64

	// Restore marks a write that puts back a company the fleet lost rather
	// than choosing one: a booting node republishing what it knew after the
	// pointer was gone. The pointer says so ([Activation.Restored]) until the
	// next write replaces it.
	//
	// A peer answers the two differently. A restore is one node's memory of
	// the company, and a node that remembers the fleet having moved on to a
	// newer revision replaces it with that one. A choice — an offline change
	// published, an override, an API write — is the company, and nothing
	// replaces it but another write, whatever the instants say: they come
	// from the writers' clocks, and a deliberate choice made on a node whose
	// clock runs behind another's would otherwise read as stale.
	Restore bool
}

// ErrPayloadNotDocument reports an activation whose payload is not a JSON
// object. Both backends refuse it with this, before anything is written,
// because a pointer whose body no node can decode is a fleet that converges on
// nothing — and the body is a company document, sealed or not, so anything
// else is a caller's mistake rather than a shape to store.
var ErrPayloadNotDocument = errors.New("coord: an activation's payload must be a JSON object")

// CanonicalPayload is the form a backend stores and serves a payload in: the
// document COMPACTED, and otherwise byte for byte what the caller handed in —
// no HTML escaping, no re-ordering.
//
// ONE FORM FOR BOTH BACKENDS, because the real one encodes the payload inside
// a JSON record, and encoding/json compacts whatever it embeds: a twin that
// kept the caller's whitespace would hand back bytes the broker never would,
// and a case certified on the twin would be certifying a different store. A
// sealed envelope is compact already, so for the one producer in production
// this is the identity.
func CanonicalPayload(payload json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, fmt.Errorf("%w (got %d bytes)", ErrPayloadNotDocument, len(payload))
	}
	var out bytes.Buffer
	if err := json.Compact(&out, trimmed); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPayloadNotDocument, err)
	}
	return out.Bytes(), nil
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
	// Activate points the fleet at a revision and publishes its PAYLOAD in
	// the same write, returning the activation with the epoch the store
	// assigned.
	//
	// THE PAYLOAD TRAVELS WITH THE POINTER, and it has to: a peer applies
	// the revision the pointer names by reading it, and while the payload
	// lived only in the node's own database the peer read nothing. A live
	// config change therefore reached exactly the node it was posted to,
	// and every other node served whatever it had booted with — for the
	// life of the deployment, reporting the failure as "no such revision"
	// once per reconcile tick.
	//
	// ONE WRITE, pointer and payload together, and that is the invariant.
	// They were two keys written in two steps — the payload, then the
	// pointer — and the order was argued as the safe one, since a crash
	// between them left a body nothing pointed at. What the order could
	// not stop was a SECOND writer: two nodes activating at once each
	// wrote a body to the one payload key, the flip that landed first was
	// then pointed at a body the other had overwritten, and every peer
	// read "no such revision" for the epoch the fleet was on until
	// somebody activated again. Two booting nodes publishing what they
	// hold do exactly that, and so did an edit that LOST its
	// compare-and-set, whose body had landed before its flip was refused.
	// One record cannot come apart. The pointer key's new revision IS the
	// epoch, so there is also no window in which a node can read an epoch
	// whose pointer has not been published, and no way for two concurrent
	// activations to be handed the same number.
	//
	// What that costs is the payload's bytes on every read of the pointer,
	// which a reconcile tick makes. A company document is kilobytes, and a
	// pointer that cannot name a body its fleet can read is the one
	// failure this store exists to prevent.
	//
	// The payload is whatever the caller sealed. This store never opens it
	// — a node reads it with the Tier A keyring it was deployed with, and
	// the coordination store holds ciphertext exactly as the node's own
	// database does. It checks only that it IS a document, and refuses
	// anything else with [ErrPayloadNotDocument] before writing.
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
	// Only the current one is kept: it is the pointer's own. A node that
	// has fallen behind needs exactly the revision the pointer names —
	// never an older one — so a per-revision history here would be
	// unbounded growth in a bucket with no retention, for rows nothing
	// would ever read. Each node's own company_config table is still where
	// its history and its diffs live. What comes back is
	// [CanonicalPayload]'s form of what was activated.
	//
	// RAISES rather than answering false on an unreachable store, for the
	// same reason [Plane.Target] does: "the fleet has a revision I cannot
	// build" and "I cannot reach the store" send a node down opposite
	// paths.
	Payload(ctx context.Context, revisionID string) (json.RawMessage, bool, error)

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

// Custody decides which data node keeps each batch of a stateless node's event
// records (ADR-0025).
//
// # Why the whole company has to agree on it
//
// A node without `data` keeps no event log of its own, so it publishes its
// records in batches onto one durable topic and the data nodes take them from
// one fleet-wide group, each writing what it takes into its own log. A group
// delivers a batch whose acknowledgement was lost AGAIN, to whichever member
// asks next — so without a decision a batch can land in two logs, and every
// figure derived from a node's own log counts it twice: the usage domain sums
// each data node's day, so a stateless node's spend would be billed once per
// data node that happened to write it.
//
// # Create-only, written AFTER the rows, and the loser deletes
//
// A data node writes a batch into its own log first and claims it second:
// whoever creates the key keeps the batch, and any other node that wrote it
// deletes its copy. The other order — claim, then write — leaves a claimant
// that died between the two holding a batch nobody wrote. In this order the
// worst a crash leaves is a written copy whose claim is unknown, which the
// node settles the same way on its next pass, and a batch is acknowledged only
// once its keeper is decided, so the group keeps offering it until then —
// or once the store answers that the record it holds decides nothing
// ([ErrCustodyUndecidable]), which no redelivery changes.
type Custody interface {
	// ClaimCustody records node as the keeper of batch unless a keeper is
	// already recorded, and answers the keeper: node itself, or the node
	// that claimed first.
	//
	// An error is the third answer, and the caller must neither keep nor
	// delete its copy on any of them. Every error is UNKNOWN — the store
	// could not say, and asking again may — except one wrapping
	// [ErrCustodyUndecidable], which is the store's DEFINITE answer that a
	// keeper is recorded and nothing can read who.
	ClaimCustody(ctx context.Context, batch, node string) (string, error)
}

// ErrCustodyUndecidable is [Custody.ClaimCustody]'s answer for a batch whose
// keeper record exists and cannot be read as a keeper: it does not decode, or
// it names no node. No claim this engine makes writes such a record — every
// claim names its node and is refused without one — so it is a value that
// reached the store some other way: written into the bucket by hand or by a
// process that is not this engine, by a later build that changed the record's
// shape, or corrupted.
//
// DEFINITE, never [ErrUnavailable]: the store answered, and it will answer the
// same for as long as the record stands, so a caller that read it as a store
// it could not reach would wait on it for ever — and a caller working through
// many batches would stop at this one, leaving every batch behind it
// unsettled. It is about ONE batch, and the caller goes on to the next.
//
// And it decides NOTHING about the copy: kept, it may be a second copy of a
// batch the unreadable record's node already keeps; deleted, it may be the
// only copy there is. So the copy stays as it is, counted in no node's day,
// until the record is removed — a claim then creates it afresh — or the batch
// is past the event log's retention and goes with its rows. A backend whose
// store can only hold a record a claim wrote never answers it.
var ErrCustodyUndecidable = errors.New("coord: the record of this custody batch's keeper cannot be read")

// Rebases is the fleet's record of the instant a unit of work's derived
// operation ids are minted at, for the work whose own start is too old for the
// operation ledger to vouch for.
//
// A derived id carries the instant its work began, so a retry reproduces it;
// an attempt that finds that instant past the state log's horizon mints at its
// own instant instead (statelog.MintAt) and records it HERE, keyed by the seed
// the ids are derived from — the work key, or the run where the turn has none.
// Every later attempt at the same work reads it back and inherits it: a crash
// re-run of a dispatched turn, a failed resume retried, the next half of a
// turn a coding run parked. Each of those can run on another node — a crash
// re-run lands wherever the seat's delivery is taken next, a resume on the
// seat's next owner — so the record has to be the fleet's: on the node's own
// database the successor found nothing, minted anew, and wrote a second copy
// of every write the attempt before it made.
//
// A READ AND A CONDITIONAL WRITE, with the value typed. The rule deciding the
// instant is the state log's, applied by the caller between the two, and the
// condition is what makes two attempts racing on one seed agree: the loser's
// write is refused, it reads the winner's instant, and the rule then keeps it.
//
// Retention is the bucket's age, [RebaseRetention].
type Rebases interface {
	// Rebase reads the instant recorded for seed and the version it was
	// read at: the zero instant and version 0 where nothing is recorded.
	//
	// RAISES rather than answering "nothing", because "nothing" sends the
	// caller to mint at its own instant — which, for a retry of an attempt
	// that did record one, is a second copy of that attempt's every write.
	Rebase(ctx context.Context, seed string) (time.Time, uint64, error)

	// RecordRebase writes at as seed's instant, conditional on version:
	// version 0 writes only where nothing is recorded, any other only
	// where that version still holds. False is a LOST RACE — another
	// attempt recorded first, or moved it since — and the caller reads
	// again and re-decides, because the instant it would have replaced
	// may be one it now has to inherit.
	RecordRebase(ctx context.Context, seed string, at time.Time, version uint64) (bool, error)
}

// Record is one stored value with the version it was read at.
//
// The version is an OPAQUE token, and the store's: it names one write to one
// incarnation of a record and is never handed out twice, so a record deleted
// and created again under the same key never answers to a version an earlier
// incarnation carried. It is what makes a read-decide-write safe without a
// transaction — a write lands only where the record still stands at the
// version its decision read — and [SandboxRuns.MutateSandboxRun] is where that
// comparison is made, inside the store, rather than at every caller.
type Record struct {
	Key     string
	Value   []byte
	Version uint64
}

// SandboxRunChange is what a decision handed to [SandboxRuns.MutateSandboxRun]
// writes.
//
// ITS ZERO VALUE WRITES NOTHING, and that is a decision like the other two
// rather than a missing one: the condition the caller evaluated did not hold —
// a claim somebody else won, a fence a newer lease outranks — and the record
// is left exactly as it was read. Put and Delete together are no change at
// all, and the store refuses them as the contradiction they are.
type SandboxRunChange struct {
	// Put is the record's next value, written whole at the version the
	// decision read. Empty with Delete false writes nothing.
	Put []byte

	// Delete removes the record at the version the decision read.
	Delete bool
}

// Validate refuses a change that both writes and removes the record.
func (c SandboxRunChange) Validate() error {
	if c.Delete && len(c.Put) > 0 {
		return errors.New("coord: a change to a sandbox run either writes it or deletes it, " +
			"never both")
	}
	return nil
}

// SandboxRunUpdate is one state of a run's record that
// [SandboxRuns.WatchSandboxRun] delivers.
type SandboxRunUpdate struct {
	// Record is the record as it now stands; the zero value when Found is
	// false.
	Record Record

	// Found is false while the run has no record: before its create, and
	// once it is deleted. A watch's first update says so explicitly, which
	// is the only way a watch can say "this run has no record" — an empty
	// stream says nothing at all.
	Found bool
}

// SandboxRuns is the fleet's record of detached coding runs.
//
// THE ONE CONTRACT HERE WHOSE VALUE IS OPAQUE, and the reason is that its
// record has fields of which coordination understands none: the suspended
// executor conversation, the box and command ids, the brief, the question a
// person is being asked. Every decision taken on those fields —
// the at-most-once tail claim, the epoch fence, the conditional pause expiry —
// is sandbox's, lives in internal/sandbox where its suite is, and would be
// nothing but duplication here. So this holds bytes and a version, and every
// condition is a DECISION the owning package hands to
// [SandboxRuns.MutateSandboxRun], which the store takes again whenever another
// writer lands first.
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
//
// # A lost race is decided again, for the store's patience
//
// Every change after the create is a read-decide-write the STORE runs: it
// reads the record, hands it to the caller's decision, and writes what that
// decision chose at the version read. A write another writer beat is never
// sent again as it was — the condition it evaluated may no longer hold — but
// decided again on the record as it now stands, for as long as the store's
// patience allows, as every race the fleet store loses is (internal/coord/kv's
// race.go). Never for a count of attempts, because the writers are many and
// nothing bounds how many overlap: one run's record takes every tool call its
// box makes over the bridge — from as many as twenty subagents at once, Claude
// Code's default — beside its seat's own turns, a peer recovering it, a
// person's answer and a person's stop. A count of lost races measures how many
// of them wrote, and a bound on it answers a busy record as one that kept
// changing.
//
// # A run's record is watched by its key
//
// A watch is of ONE run ([SandboxRuns.WatchSandboxRun]), so a node that follows
// a run's record as it changes — rather than reading it again on every call it
// serves — is told of that run's writes and of no other run's, however busy
// the fleet.
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

	// MutateSandboxRun changes one run's record by a decision taken on the
	// record as it stands, answering the record as the change left it and
	// whether a change landed.
	//
	// decide is handed the record read — its key, value and version — and
	// answers the change to make ([SandboxRunChange]), whose zero value makes
	// none. The store writes the change only where the record still stands
	// at the version decide was handed; where it does not, it reads the
	// record AGAIN and asks decide again, for as long as its patience
	// allows. So decide may be called more than once, each time with the
	// record as it then stands, and must decide from that record alone:
	// anything it keeps for its caller is the last call's. It is called
	// holding nothing of the store's, so it may itself read or write the
	// store — a write it makes is one the change it answers then loses to.
	//
	// A RUN WITH NO RECORD DECIDES NOTHING: decide is not called, and the
	// answer is (Record{}, false, nil) — a run that is gone is the same
	// non-answer as a decision that changed nothing, never an error. An
	// error decide returns is returned AS IT IS, with nothing written, so a
	// caller's own refusal reaches it in its own vocabulary. A write that
	// still lost when the patience ran out is [ErrUnavailable]: the store
	// answered every attempt, and another writer changed the record first
	// each time.
	//
	// With true it answers the record as written for a Put — the new value
	// at its new version — and the record it removed for a Delete; with
	// false, the record as the last decision read it.
	MutateSandboxRun(ctx context.Context, turnID string,
		decide func(current Record) (SandboxRunChange, error)) (Record, bool, error)

	// WatchSandboxRun streams one run's record: its state when the watch
	// began — [SandboxRunUpdate.Found] false where it has none — then each
	// state it reaches after that, a delete as Found false.
	//
	// The first update is CERTIFIED as a listing's answer is: a record that
	// existed throughout the call is never answered absent, though on a
	// replicated store it can lag the newest write by a replica's lag, which
	// the next update then carries. The call returns once it is in hand,
	// and a failure to read it is the call's error. After it, no update is
	// older than one already delivered and the newest state is always
	// delivered, but a state a later write replaced before the watch reached
	// it may be passed over: a watcher holds the record's current state, not
	// its history.
	//
	// The channel is closed when ctx ends or the watch fails. A CLOSED
	// CHANNEL IS NOT A DELETE: the watcher has stopped hearing, keeps what it
	// last knew and watches again, and the new watch's first update is the
	// state then, so a change made while nobody listened is not lost.
	WatchSandboxRun(ctx context.Context, turnID string) (<-chan SandboxRunUpdate, error)
}

// SecretRecord is one stored credential, as coordination holds it.
//
// SEALED BEFORE IT ARRIVES. Value is the envelope the Tier A keyring produced
// — coordination stores bytes and never holds a key, which is what lets the
// fleet's shared store carry credentials at all. KeyID rides denormalised
// beside it so a rotation sweep can find rows sealed under a retired key
// without opening any of them.
//
// PROVENANCE IS FOUR FIELDS — who wrote it, what sort of author, the
// credential they acted through, and the write path it arrived by — because a
// record that named only the credential lost its author the week the
// credential's own row was swept. See internal/secrets' Author, and its Source
// for the fourth: held here as the plain string a store of bytes keeps, and
// stamped and checked by the write path rather than by anything here.
type SecretRecord struct {
	Name          string
	Value         string
	KeyID         string
	UpdatedAt     time.Time
	UpdatedBy     string
	UpdatedByKind string
	OperatorID    string
	Source        string

	// Version is the store's own revision of the row, set on every read
	// and ignored on every write: what [Secrets.UpdateSecret] is
	// conditioned on. Store-wide rather than
	// per row, as a KV revision is, so a row deleted and written again
	// never hands back a version an older incarnation of it already used.
	Version uint64
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

	// CreateSecret writes a sealed value only where no value is stored
	// under its name, reporting whether it wrote. False is somebody else's
	// row, not a failure.
	//
	// THE ENGINE'S OWN KEY MATERIAL NEEDS THE SANDBOX RUNS' DISCIPLINE, and
	// last-write-wins is exactly wrong for it: a company key two nodes
	// minted at once is one whose loser derived values under a key the
	// store no longer holds. So beside the operator's plain put, two
	// conditional writes: a create for a key's mint, and an update at the
	// version read for a rekey.
	CreateSecret(ctx context.Context, rec SecretRecord) (bool, error)

	// UpdateSecret replaces a sealed value only while the row is still at
	// version, reporting whether it wrote. False is a LOST RACE — the row
	// moved or went away since the caller read it — and the caller
	// re-reads and re-decides rather than writing over what it never saw.
	UpdateSecret(ctx context.Context, rec SecretRecord, version uint64) (bool, error)
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
// The bucket has no age, because a status is standing state rather than a
// short-horizon question. One that expired would make a converged integration
// read as one nobody has ever looked at, on a timer nobody chose, and the loop
// would then re-provision against a third-party app it had already agreed with.
// There are at most as many keys here as there are surfaces, so nothing grows.
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
	// Seat is the seat's durable id, and the record's KEY.
	//
	// THE ID RATHER THAN THE HANDLE, because the mailbox this record is
	// about is named by the id: a registry keyed on a handle could not
	// build the subject it is registering, and the same handle under a
	// renamed organisation is another seat with another mailbox
	// (ADR-0013), which a handle-keyed registry would fold into one record.
	Seat uuid.UUID

	// Handle is what the seat answers to, and it is a LABEL rather than an
	// identity: it is what a person reading the registry, or a retirement
	// log line, needs in order to know whose mailbox is being retired.
	// NOTHING RESOLVES BY IT — the runs a retirement ends, the lease it
	// claims and the subscriptions it deletes are all found by Seat —
	// because it can be EMPTY: a record a sweep registered from the
	// broker's listing alone, for a seat neither the roster nor the
	// registry still names, has no handle anybody can supply. The first
	// registration or sweep that finds the seat in the roster again writes
	// it, and the handle a seat's id is derived from never changes, so a
	// written label never goes stale.
	Handle string

	// AbsentSince is when a sweep first found the seat missing from the
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
// seats a company has ever had rather than by anything that grows per event.
type Mailboxes interface {
	// Mailbox reads one record, by the seat's id.
	Mailbox(ctx context.Context, seat uuid.UUID) (MailboxRecord, bool, error)

	// Mailboxes returns every record, ordered by seat id, so two backends
	// answer a sweep in the same order.
	Mailboxes(ctx context.Context) ([]MailboxRecord, error)

	// CreateMailbox writes a new record and returns it as stored, with the
	// version its next write must carry. A seat that already has a record
	// is left alone and reports false: the existing record may be mid-way
	// through a retirement, and only a conditional update may change it.
	CreateMailbox(ctx context.Context, rec MailboxRecord) (MailboxRecord, bool, error)

	// UpdateMailbox writes rec at rec.Version and returns it as stored,
	// reporting false when that version no longer holds, including when the
	// record is gone.
	UpdateMailbox(ctx context.Context, rec MailboxRecord) (MailboxRecord, bool, error)

	// DeleteMailbox removes a record at a version, reporting whether that
	// version still held. A record deleted this way can be created again.
	DeleteMailbox(ctx context.Context, seat uuid.UUID, version uint64) (bool, error)
}

// Markers is the one piece of housekeeping the shared state asks of its
// caller: removing the record that a key was removed.
//
// # Why a store that forgets on its own still needs it
//
// A KV backend removes a key by appending a delete or purge MARKER, and a
// bucket the broker ages takes the marker with everything else when it ages
// out. The families here that no clock may reap — the activation pointer, the
// runs, the channels, the mailboxes, the secrets, the integration statuses, the
// object store's backend record, the positions register — keep every marker for
// the life of the deployment, and each one is a leader read on every listing
// that meets it (internal/coord/kv walk.go says why a marker is read again
// rather than trusted). So the markers are swept, as a DECISION under the
// maintenance duty, for the reason the channels are: a bucket's age cannot
// express which records may go.
//
// # It changes no answer
//
// That is the contract, and the suite holds it: sweep every marker, and every
// read, listing, create and conditional write answers exactly as before. A
// backend that keeps no marker at all — the memory twin deletes the entry —
// satisfies it by sweeping nothing.
type Markers interface {
	// SweepMarkers removes every marker written before cutoff from the
	// record families no clock ages, reporting how many went. A failure
	// part-way reports the markers already removed beside the error.
	SweepMarkers(ctx context.Context, cutoff time.Time) (int64, error)
}

// Fleet is a backend that serves all of the shared state, which is what the
// contract suite certifies and what the engine wires from.
//
// One interface at the CONSTRUCTION seam and a narrow one at each call site:
// the webhook edge takes a Claims and nothing else, the valve takes a Counter,
// a turn's meter takes the verbs of [Budgets] it calls. A consumer that
// could reach the whole store would eventually use it.
type Fleet interface {
	Counter
	Claims
	Ledger
	Budgets
	Plane
	Channels
	Follows
	Fires
	Rebases
	SandboxRuns
	Secrets
	Integrations
	Mailboxes
	ObjectStores
	TrashReaps
	SeatPauses
	PositionRegister
	HoldRegister
	FloorRegister
	BackupRegister
	MaintenanceRegister
	Markers
	Custody
	UploadReservations
}

// ErrObjectBackendMoved is a move of the fleet's object backend from a store
// that is not the recorded one: another migration moved it, or the caller
// read a record that changed. The answer is to read the record again, never
// to retry the write.
var ErrObjectBackendMoved = errors.New("coord: the fleet's object backend is not the one the move names")

// ObjectStores is what the whole fleet has to agree on about the object store
// (ADR-0026): which backend its files are in, and what its collector last
// found.
//
// # The backend is the fleet's, and recorded once
//
// Every node reads and writes files through the backend its own Tier A names,
// and two nodes naming different ones split the company's files between them
// with nothing failing — each side's uploads read back on that side alone,
// and the collector running on one side deletes nothing the other wrote. So
// the first node to open the store records which backend it is, create-only
// and with no age, and every node after compares its own against it and
// refuses to boot on a mismatch.
//
// # Nothing else, and no lock
//
// A deletion from the store takes no lock (ADR-0027): an object is stored
// under a key minted for its one upload and named by that upload's write
// alone, so no writer is ever re-using an object the collector could be
// deleting.
type ObjectStores interface {
	// AgreeObjectBackend records identity as the fleet's object backend
	// unless one is recorded, and answers the recorded one: identity
	// itself, or what the first node recorded. An error is UNKNOWN — the
	// caller must not open a backend it could not check.
	AgreeObjectBackend(ctx context.Context, identity string) (string, error)

	// MoveObjectBackend replaces the recorded backend `from` with `to`,
	// conditional on `from` being what is recorded: the one write that
	// moves the fleet's files to another store, made by `crewlet objects
	// migrate --to s3` once every object is copied, inside a maintenance
	// window. It answers [ErrObjectBackendMoved] where another backend is
	// recorded, and nil where `to` already is (the move was made, and a
	// retry of it is the same move). An error is UNKNOWN.
	MoveObjectBackend(ctx context.Context, from, to string) error

	// RecordObjectCollection stores what the collector last found, an
	// opaque value its owner (internal/engine) encodes, replacing the last
	// one: the duty moves between nodes, and every node's status surface
	// reads the latest report whoever wrote it.
	RecordObjectCollection(ctx context.Context, value []byte) error

	// ObjectCollection reads the last report, false when none was ever
	// written.
	ObjectCollection(ctx context.Context) ([]byte, bool, error)
}

// TrashReaps is where the trash reaper's last report is kept (internal/
// trashreap): which node ran the latest pass, against which cutoff, what each
// domain's trash gave up, and when the next pass is due.
//
// # Why the fleet keeps it, and not the node that ran it
//
// The reaper is a fleet duty, so it moves: the node that ran the last pass is
// rarely the one asked about it, and a node that takes the duty over reads
// when the next pass is due from here rather than reaping again at once. The
// report is a whole account rather than a delta, so the last write is the
// answer, whoever wrote it.
//
// # Nothing else, and no lock
//
// A purge is decided by its domain against that domain's own trash, in a
// snapshot the broker arbitrates, so two reapers that overlap while the duty
// moves purge nothing twice. The record is an account of what happened and
// never a claim anybody reads to decide a purge.
type TrashReaps interface {
	// RecordTrashReap stores the reaper's last report, an opaque value its
	// owner (internal/trashreap) encodes, replacing the one before.
	RecordTrashReap(ctx context.Context, value []byte) error

	// TrashReap reads the last report, false when none was ever written.
	// An error is UNKNOWN: the caller must not read it as "never ran".
	TrashReap(ctx context.Context) ([]byte, bool, error)
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
// # The seat is named by its handle
//
// Every method's seat is the seat's handle, which is immutable (ADR-0013) —
// what internal/notify's FollowStore hands in. This store resolves nothing; it
// keys on the name it is given — and it RANKS nothing either: which of two
// reasons is the stronger is internal/notify's vocabulary, so the caller
// chooses and this store makes the choice safe against a concurrent one.
//
// # Two methods, and nothing removes a follow
//
// Retention here is the BUCKET's age, like every other aged slot: every
// re-assert rewrites the record, so the age is a true last-activity stamp and
// the broker expires what has gone quiet. A sweep would have nothing to
// delete, and there is no unfollow either: no surface offers a person or a
// seat a way to stop following a thread, so the age is the only thing that
// ends a follow.
type Follows interface {
	// Follow records that a seat follows a thread, or refreshes the follow
	// it has, and answers the reason the record holds afterwards.
	//
	// THE REASON IS CHOSEN AGAINST THE RECORD IT REPLACES: choose is handed
	// the reason held, and whether a follow is held at all, and answers the
	// reason to write — and the write lands only on the record choose was
	// shown. One that loses to another writer reads again and asks again.
	// A choice made from an earlier read is a decision about a record
	// somebody may since have rewritten, and the reason decides whether a
	// reply obliges an answer: a seat named in a thread a moment before a
	// broadcast is written there must not be left following it for the
	// broadcast. choose may run more than once, so it must be a pure
	// function of what it is handed.
	//
	// EVERY CALL WRITES, an unchanged reason included, because the write is
	// what moves the bucket's age: a follow refreshed only when its reason
	// changed would expire in a thread the seat is still active in.
	Follow(ctx context.Context, backend, seat, channel, thread string, at time.Time,
		choose func(held string, following bool) string) (string, error)

	// Following reports why a seat follows a thread, and whether it does.
	//
	// THREE ANSWERS, not two. A reason and true is a follow; an empty
	// reason and false is definitively not following; an error is UNKNOWN
	// and says nothing about either. The caller decides what to do with
	// the third — internal/notify fails closed, because a missed thread
	// reply is quiet and self-healing where a spurious wake is a burst of
	// turns that cannot be taken back.
	Following(ctx context.Context, backend, seat, channel, thread string) (string, bool, error)
}
