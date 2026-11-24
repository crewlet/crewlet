package coord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// A lease answers "who runs this seat"; these contracts answer what else a
// fleet must agree on. Most were on a node's own database (internal/store is
// "one file, one process") though their comments described a fleet: a per-node
// valve let a seat emit N times its rate, and the claims, ledgers, channel and
// executor records were invisible to the peer that needed them. The sign-in
// throttle is deliberately each node's own: sharing it put a coordination round
// trip on the sign-in path to buy back a factor of N (internal/iam/credential).
// Every contract fails in a stated direction, because an unreachable store must
// not invent a decision: an unreadable dedupe claim must NOT suppress the
// delivery, and an unreadable rate valve must not open the floodgates.

// The bucket retentions.
//
// Each is a bucket's age, not a per-call TTL, because that is all a KV backend
// can enforce (internal/coord/kv's package doc), so it is fixed at creation and
// a longer ask is refused, never clamped. The exceptions are the claim TTLs,
// [ClaimTTL] and [SetupOnceRetention]: a deadline in the claim's own record,
// bounded by the claims bucket's age, [MaxClaimTTL].
//
// Each is sized from the cadence of the subsystem that reads it. A backend
// takes them as configuration, never a default: a bucket created with the
// wrong age is wrong for its lifetime.
const (
	// RateWindow is the notification valve's window, and the unit an
	// operator's `notification_rate_limit: 5` is written in: five per seat
	// per SECOND. Wider would let a genuine loop run longer before
	// tripping — two seats answering each other saturate a second easily
	// — and narrower would trip on an ordinary burst from one push.
	RateWindow = time.Second

	// MaxRateWindow is the longest window the fleet counts a [Counter] in
	// among the valve's seconds, and so how long that store keeps a window (a
	// few multiples of it, so a window that just closed is still readable
	// while a straggler finishes writing to it). One minute: the Dashboard
	// Assistant's turn rate is a person's questions per MINUTE
	// (internal/assistant), counted across the fleet, the widest of the short
	// windows. A wider window, up to [MaxCounterWindow], is kept apart in a
	// store whose age outlasts it: the short store would reap it mid-window,
	// which would restart its count and pass far more than the caller asked
	// for.
	MaxRateWindow = time.Minute

	// MaxCounterWindow is the longest window [Counter.Allow] counts: a DAY,
	// the window of a seat's atlas records (internal/agent/builtin's
	// AtlasRecordsPerDay), the longest any caller counts in. A backend
	// keeps a window's count until the window has closed, and a window up to
	// a day long is kept beside the valve's seconds in a store whose age
	// outlasts it — never in theirs, which reaps a second-wide window within
	// seconds so that a seat's notifications leave one record a second
	// behind for as long as they last, and never theirs in it. A longer
	// window is an error: nothing keeps it.
	MaxCounterWindow = 24 * time.Hour

	// ClaimTTL is how long an inbound delivery stays claimed.
	//
	// Sized to cover queue redelivery and an operator's replay, NOT a
	// third-party app's own retry schedule. Those back off for far longer, in
	// minutes to hours, and only fire when the API layer failed to answer
	// 2xx, which is exactly when the delivery was never claimed at all.
	// Too long is visible in one direction only: a deliberate replay ten
	// minutes later vanishes into a claim nothing will clear.
	ClaimTTL = 5 * time.Minute

	// MaxClaimTTL is the longest TTL a [Claims] claim may ask for, and so the
	// claims bucket's age; shorter claims lapse at their own deadline. A longer
	// ask is refused with [ErrTTLTooLong], not clamped, because a clamped
	// dedupe window lets through the duplicate it was sized to stop.
	//
	// Thirty minutes, set by the longest claim anybody takes: the Mattermost
	// socket's (mattermost.ClaimTTL), which must outlive the window a
	// reconnecting seat replays (mattermost.MaxBackfill, fifteen minutes) plus
	// the backoff ceiling and clock margin. This package cannot import that
	// one, so its own tests hold the two together. Longer costs only unread
	// records: every claim key names one delivery.
	MaxClaimTTL = 30 * time.Minute

	// SetupOnceRetention is how long a spent setup-callback state is
	// remembered (the TTL a setup flow takes its [Claims] claim with), and so
	// how long the state it mints may be valid: the record stops a second
	// presentation, so a token outliving it would be replayable while its
	// signature validates. internal/api/setupapi's manifestTTL IS this constant.
	//
	// Fifteen minutes spans the task (a GitHub round trip and two clicks); the
	// vendor bounds it above: GitHub expires a manifest conversion's one-time
	// code at an hour. An ordinary claim in the claims bucket, so only
	// [MaxClaimTTL] bounds it, held by coordtest's retention guard because a
	// longer claim is an error and the setup flow fails closed on errors.
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
	// The operation ledger's own retention (statelog.OpsRetention), a day past
	// the horizon a rebase is inherited within (statelog.MintHorizon). An
	// attempt inherits an instant only within that horizon of its own clock,
	// and the age counts from the write, no earlier than the instant it
	// records, so an aged-out record is one the rule would have replaced.
	// Shorter forgets an instant a retry must inherit, and the retry mints
	// anew and writes again; longer keeps unreadable records. This package
	// cannot import the state log, so coordtest's retention guard holds the
	// two together.
	RebaseRetention = 30 * 24 * time.Hour

	// BudgetRetention is how long a token counter outlives its last
	// write, and the bucket's age that enforces it (ADR-0019).
	//
	// A counter holds a slot per calendar window, the longest a month: 31 days
	// plus the hour a clock change can add. A record uncharged for longer
	// holds no current slot, and without an age every seat that ever ran would
	// keep one for ever. Thirty-two days is the first whole day past it; a
	// charge restarts the age, so nothing a current window counts is reaped.
	//
	// This is NOT the reset: an allowance returns when the window turns over,
	// by the roll inside the charge that crosses it ([Tally.Roll]). Aging out
	// early would re-arm a company mid-month; late costs only an unread record.
	BudgetRetention = 32 * 24 * time.Hour

	// CustodyRetention is how long the record of which data node keeps a
	// batch of a stateless node's event records lasts — see [Custody] —
	// and so the bucket's age.
	//
	// A day past the event log's own retention (store.EventRetention). A node
	// that never learned whether it kept a batch asks this record at its next
	// pass or at boot, and a record aged out while the rows were still logged
	// would say nobody keeps it, so the node would keep a second copy. The
	// rows go at the log's retention, so a record outliving them by the
	// sweep's slack day is never asked about a batch it no longer describes.
	// Longer keeps unaskable records. This package cannot import the store, so
	// coordtest's retention guard holds the two together.
	CustodyRetention = 32 * 24 * time.Hour

	// UploadRetention is how long an upload reservation's records outlive
	// their last write (see [UploadReservations]), and a seat's save ledger
	// its own ([SaveAllowances]), and so the uploads bucket's age.
	//
	// Thirty hours: a reservation must stay readable as long as its hold on
	// bytes nothing names yet, until the collector could have deleted them: its
	// grace (a day, objstore/collect.PendingGrace), two passes, and the
	// transfer window (an hour at most, objstore.MaxPresignTerm) at the pace of
	// the largest file, about twenty-seven hours; the rest is margin. Shorter
	// would forget a hold on bytes still stored; longer keeps unread records.
	// This package cannot import the object store, so internal/artifactupload's
	// tests hold the two together, and internal/sandbox's a save entry's hold.
	UploadRetention = 30 * time.Hour

	// SandboxRunRetention is absent for the same reason as the channel
	// bucket's, one step sharper: a detached executor can sit parked on
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

	// FollowRetention is how long a seat's chat thread-follow survives with no
	// activity, enforced by the bucket's age: every re-assert (a mention, a
	// collective address, the seat posting) rewrites the record, so the age is
	// a last-activity stamp.
	//
	// Ninety days: Slack and Mattermost both surface a quarter-old thread only
	// through search. Dropping a stale follow costs at most one missed
	// non-mention reply, healed by the next mention, while keeping every follow
	// grows without bound on a record read for every inbound chat message.
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

	// MarkerRetention is how long a delete or purge marker stays in a store
	// that never ages its records, before [Markers.SweepMarkers] removes it.
	//
	// Not a correctness bound: every reader asks what the key holds NOW before
	// trusting a marker, so one swept at once changes no answer; the contract
	// suite sweeps with a future cutoff and holds every answer to what it was.
	//
	// Each held marker is a leader read on every listing delivering it, but
	// it is the only trace that a key was removed rather than never written,
	// so it must outlive every operation acting on what it read before the
	// removal: a listing runs inside its tick, every read-decide-write under a
	// claim, and none outlives [MaxDutyTTL]. Hence three hours.
	MarkerRetention = MaxDutyTTL
)

// Counter is the fleet's shared fixed-window counter, behind the notification
// valve (a second wide), the Dashboard Assistant's turn rate (a minute),
// delegated work (an hour) and a seat's atlas records (a day).
//
// A FIXED window, not a sliding one: the operator's number reads as "N per
// seat per second" or "N records a day", and the arithmetic a fixed window
// needs is one increment against one key — which is the only shape that stays
// correct when four nodes increment it at once. A window is now truncated to a
// multiple of its width from the zero time, which is UTC: a day's window runs
// from one UTC midnight to the next.
type Counter interface {
	// Allow increments the bucket's count for the window containing now
	// and reports whether it stayed within limit. A window longer than
	// [MaxCounterWindow] is an error, since no backend keeps its count
	// until it closes.
	//
	// Three answers: within the limit, over it, and, as an error, the store
	// could not say. Contention is never the third: a lost race is another
	// caller's increment, so each caller gets room or a full window. The
	// notification path FAILS OPEN on the third (notify.Valve): a refusal is
	// dropped, not delayed, so a valve shut by an unreadable store would drop
	// every seat's notifications while it was away.
	Allow(ctx context.Context, bucket string, limit int, window time.Duration, now time.Time) (bool, error)
}

// Claims is the fleet's first-claim-wins registry with a TTL: behind webhook
// deduplication, the change feed's wake dedupe, the chat socket's delivery
// claim and a setup flow's one-shot callback state.
//
// A claim lapses at the deadline its record carries (its writer's `now` plus
// `ttl`), judged against the next caller's `now`, so no node compares its clock
// to the store's reaping. A per-key store TTL cannot do it: a KV's is
// create-only and an update clears it (internal/coord/kv's package doc). The
// bucket's age is the ceiling, [MaxClaimTTL]; a longer ask is an error. `now`
// is never a deadline: reaping removes only records already past theirs.
// Polarity belongs to the caller: webhook dedupe fails open; a setup flow's
// callback state is an authorization check and fails closed.
type Claims interface {
	// Claim records key and reports whether THIS caller was first.
	//
	// FAILS OPEN: an error yields (false, err) and the caller must PROCESS the
	// delivery, since a push suppressed by a store blink is a wake nobody
	// notices, while a duplicate is a turn the completion ledger collapses.
	//
	// The claim lapses at now+ttl, judged against the next caller's `now`. A
	// ttl beyond [MaxClaimTTL], or a backend's shorter ceiling, is an error
	// wrapping [ErrTTLTooLong], never a clamp. An empty key is an error, not
	// (false, nil): naming nothing loses no race, and "somebody else took it"
	// would drop the delivery.
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

	// WorkedAt is Worked with when each key was worked: the instant its
	// first record stated, zero where a record states none. Fails open the
	// same way.
	WorkedAt(ctx context.Context, scope string, keys []string) (map[string]time.Time, error)

	// WorkedBy is Worked with the detail each key's first record stated (the
	// completion ledger's: the turn that worked it), "" where it states none.
	// Fails open the same way.
	WorkedBy(ctx context.Context, scope string, keys []string) (map[string]string, error)

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
	// cannot both win. INCREASING, NEVER CONSECUTIVE: the body's write takes
	// a revision of the same sequence, so no reader counts on a gap's size.
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

	// Body names the revision's sealed document, which rides a key of its own
	// ([Plane.Body]). Zero on a pointer written in the layout before bodies
	// had keys, which no read of the body takes.
	Body BodyRef
}

// BodyRef names an activation's body: its length and its SHA-256, which is
// also its key, so one key is one set of bytes and two activations of the
// same document share it. The length sizes the body's read
// ([Plane.BodyCrossing]); the digest is what that read checks.
type BodyRef struct {
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// BodyOf is the reference to payload, a body in [CanonicalPayload]'s form.
func BodyOf(payload json.RawMessage) BodyRef {
	sum := sha256.Sum256(payload)
	return BodyRef{Bytes: len(payload), SHA256: hex.EncodeToString(sum[:])}
}

// Named reports whether r names a body at all: a pointer written before
// bodies had keys names none.
func (r BodyRef) Named() bool { return r.SHA256 != "" }

// Check refuses a reference no write makes, with [ErrBodyCorrupt]: a length
// outside 1..[MaxValueBytes], or a digest that is not 64 lowercase hex digits.
// Any admitted connection can write the pointer (ADR-0094), and the length
// sizes a read and the posture's grace ([Plane.BodyCrossing]), so it is held
// to its shape before either trusts it.
func (r BodyRef) Check() error {
	if r.Bytes < 1 || r.Bytes > MaxValueBytes {
		return fmt.Errorf("%w: it names a body of %d bytes, outside 1 to %d",
			ErrBodyCorrupt, r.Bytes, MaxValueBytes)
	}
	if len(r.SHA256) != sha256.Size*2 || strings.Trim(r.SHA256, "0123456789abcdef") != "" {
		return fmt.Errorf("%w: it names the digest %q, which is not 64 lowercase hex digits",
			ErrBodyCorrupt, textcut.Within(r.SHA256, sha256.Size*2))
	}
	return nil
}

// Describes reports whether payload is exactly the body r names, by length
// and digest.
func (r BodyRef) Describes(payload []byte) bool {
	return len(payload) == r.Bytes && BodyOf(payload).SHA256 == r.SHA256
}

// ErrBodyless is an activation pointer that names no body: written by a build
// from before bodies had keys of their own. Definite, never unavailable: no
// retry changes it, one new write does.
var ErrBodyless = errors.New("coord: the activation pointer names no body. Every node must run " +
	"this build; then POST /config/reload publishes the company in the layout it reads")

// ErrBodyCorrupt is a body whose bytes are not the ones its activation names:
// another length or another digest. Definite, never unavailable: the key holds
// the same bytes until it is written again.
var ErrBodyCorrupt = errors.New("coord: the activation's body is not the bytes its pointer names")

// RevisionOrigin is the record a revision was WRITTEN with, travelling with
// the pointer so a peer adopting the revision keeps it.
//
// A peer cannot know what the pointer does not carry: peers recorded it as
// written by `peer` from source `fleet`, a different author on every node.
// Required: [RevisionOrigin.Check] holds both backends to it. The kind is
// internal/iam's [iam.ActorKind]; an unknown kind off the wire stays that value
// ([iam.ActorKind.Valid] false). The parent is deliberately absent: it may name
// a revision the adopting node never held, and its column is a foreign key
// into that node's own rows.
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

// UTC is the origin with its instant in UTC, as every other time the store
// keeps; a zero instant stays zero, so an origin that named none still encodes
// without one.
func (o RevisionOrigin) UTC() RevisionOrigin {
	if !o.CreatedAt.IsZero() {
		o.CreatedAt = o.CreatedAt.UTC()
	}
	return o
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

// ActivationAt is the instant an activation is published at: the one asked
// for, or a millisecond after the activation it replaces if not later.
//
// Readers decide which activation is newer by this instant, so it must follow
// the pointer's order, not the nodes' clocks. Derived rows carry it
// (configplane.ActivationStamp) and refuse an older stamp, so a no-later
// instant is applied to none, and a booting node's republish and the reconciler
// compare it too. Decided inside the compare-and-set that replaces
// the pointer. Compared in Unix milliseconds, the stamp's resolution, coarser
// than the pointer's nanoseconds and the node store's microseconds, so a later
// instant stays later
// (configplane.TestTheActivationPointersForwardRuleIsLaterInStamps).
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
	// Boots is the revision this node would START ON if it restarted now: its
	// boot row's, as its reconciler last read or wrote it. Empty on a node
	// that boots on nothing (a database discarded at every start, as without
	// the `data` role, or no revision applied yet). A later failed read leaves
	// the last answer standing.
	//
	// A fact of its own, never derived from Serving: the boot row moves only
	// after a SUCCEEDED, local, best-effort write, so while a failed one is
	// retried the node runs a revision it would not come back on. A reader
	// taking Serving for the restart target (the Kubernetes operator keeps a
	// revision's secrets while a node names it) would drop what that node
	// needs, and its restart would crash-loop.
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

	// Payload is the SEALED body, written under its own digest before the
	// pointer that names it ([Plane.Activate]). Required, and a JSON object —
	// a company document, which the caller has sealed into an envelope that
	// is one ([CanonicalPayload]); held to [MaxValueBytes].
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
	// A missing pointer (never set, deleted, purged) is [ErrActivationRaced]
	// too: what the caller built on is gone. Every editor reads the pointer
	// first and passes Expect or, having read none, ExpectAbsent, so the race
	// reported is real and the fix is to read again and rebuild.
	//
	// EMPTY IS UNCONDITIONAL, and only `crewlet run -import-company` passes
	// it: an operator replacing the company outright from a file asserts
	// rather than edits and has nothing to have raced with.
	Expect string

	// ExpectAbsent is the create-only compare-and-set: publish this only
	// if the fleet has no activation at all.
	//
	// What a write built with no pointer passes: a config write on a node
	// that read none, a booting node restoring a lost pointer, a fleet's
	// first company. An empty Expect would replace a company the fleet is
	// already running, which a node joining before its first read reaches
	// legitimately; the company name changes, every derived seat id changes,
	// and all their memory is orphaned.
	//
	// Refused with [ErrActivationRaced] like any lost race, leaving nothing
	// behind. Set with Expect it is a programming error.
	ExpectAbsent bool

	// ExpectEpoch narrows Expect to the very activation the caller read: the
	// write lands only while the pointer is AT this epoch. Zero leaves Expect
	// as it is; set without Expect it is a programming error.
	//
	// The boot publish of an offline offer records the instant it will land
	// at before publishing, so a node that crashed first can tell the landing
	// by the pointer carrying that instant. That instant is decided against
	// the activation replaced ([ActivationAt]); with Expect alone, another
	// writer re-activating the same revision in between lands this one at an
	// unrecorded instant, and the publish reads as never made.
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
// ONE FORM FOR BOTH BACKENDS, and the form a body's digest is taken over
// ([BodyOf]): one document written with other whitespace is the same body
// under the same key. A sealed envelope is compact already, so for the one
// producer in production this is the identity.
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

// ActivationRecord is the activation pointer's stored form: what a backend
// keeps under the pointer's key ([ActivationValue]). The EPOCH IS NOT IN IT:
// the key's revision is the epoch, so storing one too would give two answers
// that could disagree.
//
// The ORIGIN fields travel beside the revision so a peer adopting it records
// who wrote it ([RevisionOrigin]). The record evolves additively: a later build
// keeps every key with its meaning, because this build reads its pointer during
// a rolling upgrade. In the contract, not the KV backend, because the pointer
// is held to [MaxPointerBytes]: the twin measures the bytes the KV would write,
// so a write one backend takes the other takes (internal/api/configapi checks
// the bound first).
type ActivationRecord struct {
	RevisionID string    `json:"revision_id"`
	At         time.Time `json:"at"`
	Summary    string    `json:"summary,omitempty"`

	// The revision's ORIGIN — see [RevisionOrigin] — plus the credential it
	// was written through. The kind is read as the value it is: an unknown
	// spelling stays unknown ([iam.ActorKind.Valid]).
	Author     string        `json:"author,omitempty"`
	AuthorKind iam.ActorKind `json:"author_kind,omitempty"`
	OperatorID string        `json:"operator_id,omitempty"`
	Source     string        `json:"source,omitempty"`
	CreatedAt  time.Time     `json:"created_at,omitzero"`

	// Body names the revision's sealed body, which rides its own key so
	// every read of the pointer stays small ([MaxPointerBytes]). Every pointer
	// this build writes names one.
	Body BodyRef `json:"body"`

	// Restored is [Activation.Restored]. Absent on a choice, which is every
	// pointer but a boot's restore.
	Restored bool `json:"restored,omitempty"`
}

// Origin is the record's revision origin.
func (r ActivationRecord) Origin() RevisionOrigin {
	return RevisionOrigin{
		Author: r.Author, AuthorKind: r.AuthorKind, OperatorID: r.OperatorID,
		Source: r.Source, CreatedAt: r.CreatedAt,
	}
}

// ActivationValue is the pointer's stored form for req — its origin, in UTC,
// and the body it names — at the instant at, which is decided against the
// pointer the write replaces ([ActivationAt]) and so is known only inside the
// write: the one encoding every backend stores, and holds to
// [MaxPointerBytes].
//
// NO HTML ESCAPING, so a summary is measured as written ([SummaryBytes]).
func ActivationValue(req ActivationRequest, body BodyRef, at time.Time) ([]byte, error) {
	origin := req.Origin.UTC()
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ActivationRecord{
		RevisionID: req.RevisionID, At: at, Summary: req.Summary,
		Author: origin.Author, AuthorKind: origin.AuthorKind, OperatorID: origin.OperatorID,
		Source: origin.Source, CreatedAt: origin.CreatedAt,
		Body: body, Restored: req.Restore,
	}); err != nil {
		return nil, fmt.Errorf("encode the activation: %w", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// ActivationBytes is how large the pointer's value for req is at its widest:
// [ActivationValue] naming the largest body a key holds, with both its
// instants, the pointer's and its origin's, written with every fractional
// digit an instant encodes. A
// store moves the pointer's instant forward past the pointer it replaces
// ([ActivationAt]), and a later instant can encode longer — a millisecond on,
// ".1Z" is ".101Z" — while a config write's dry run sizes the pointer its
// write would make at an instant before that write's own, the origin's
// included. So a request this sizes within [MaxPointerBytes] is one the store
// takes at whatever instants it is written at. For a caller holding a write to
// the bound before it commits to anything (internal/api/configapi), which has
// no instant but its own, and for both backends.
func ActivationBytes(req ActivationRequest) (int, error) {
	if !req.Origin.CreatedAt.IsZero() {
		// A ZERO ORIGIN INSTANT STAYS ZERO: an origin that names none
		// encodes none ([RevisionOrigin.UTC]).
		req.Origin.CreatedAt = widestIn(req.Origin.CreatedAt)
	}
	value, err := ActivationValue(req, widestBody, widestIn(req.At))
	if err != nil {
		return 0, err
	}
	return len(value), nil
}

// widestBody names a body at [MaxValueBytes], the longest reference a pointer
// carries.
var widestBody = BodyRef{Bytes: MaxValueBytes, SHA256: strings.Repeat("f", sha256.Size*2)}

// widestIn is the last instant of t's second, in UTC: the instant in it that
// encodes with every fractional digit an instant has.
func widestIn(t time.Time) time.Time {
	return t.UTC().Truncate(time.Second).Add(time.Second - time.Nanosecond)
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
	// Activate points the fleet at a revision, returning the activation with
	// the store's epoch (the pointer key's revision) and the body it names.
	//
	// THE BODY FIRST, under its digest; then the pointer, which names it; and
	// no body is removed while a pointer names it or a flip could still name
	// it ([BodyRetention]). Content addressed, two writers' bodies are two keys
	// unless they are one document, so no flip names a body another overwrote.
	// The payload is sealed by the caller; only that it IS a document is
	// checked ([ErrPayloadNotDocument]). [ActivationRequest.Expect] gives the
	// loser of two concurrent edits [ErrActivationRaced].
	Activate(ctx context.Context, req ActivationRequest) (Activation, error)

	// Body returns the sealed body a's pointer names, in [CanonicalPayload]'s
	// form, read given the time its recorded size takes to arrive
	// ([Plane.BodyCrossing]). False when the key is gone: the pointer moved on
	// and the sweep removed it, so the caller reads the pointer again.
	//
	// Exactly the bytes a names, or [ErrBodyCorrupt]; a pointer naming no body
	// is [ErrBodyless]. RAISES on an unreachable store, as [Plane.Target]
	// does: "a revision I cannot build" and "I cannot reach the store" are
	// opposite paths.
	Body(ctx context.Context, a Activation) (json.RawMessage, bool, error)

	// BodyCrossing is how long a's body takes to cross this node's connection
	// to the store at the slowest pace allowed: none in process. No I/O. A
	// node is behind for at least this while it reads the body, which the
	// posture counts (internal/configplane's FleetView).
	BodyCrossing(a Activation) time.Duration

	// Target reads the pointer, reporting whether one has ever been set. The
	// pointer is small, so the read is.
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
	// Separate from OpenChannels rather than a flag on it, because the rules
	// are opposite: the idle sweep must see only open channels, while a read
	// surface must see both, or the history kept until the purge horizon is
	// shown by nothing. Bounded by the same purge: a closed channel is deleted
	// past the horizon.
	AllChannels(ctx context.Context) ([]Channel, error)

	// PurgeChannels deletes channels closed before cutoff, returning the
	// count. An open channel is never purged however old, or a long
	// running ask loses its authorization record while its answer is still
	// in flight.
	PurgeChannels(ctx context.Context, cutoff time.Time) (int64, error)
}

// Fires is the fleet's at-most-once guard for scheduled work.
//
// One claim per dispatch identity, a minute and the runner it was addressed
// to, so the scheduler's at-most-once guarantee holds across the fleet: a
// per-node ledger handed the `scheduler` duty's new holder an empty one, and
// its catchup pass re-fired what the old holder had claimed.
//
// The minute is the schedule's opaque key (scope, owner, schedule name and the
// local minute, joined injectively). A store keeps a minute's claims together
// because catchup asks about a minute as a whole ([Fires.FireMinuteClaimed]).
//
// The node's scheduled_runs table stays as its audit record.
type Fires interface {
	// ClaimFire records one runner's claim on a minute and reports whether
	// THIS caller wrote it. The runner may be empty, which is the claim a
	// catchup skip makes: it resolved no runner, and an empty runner is a
	// claim of its own beside every named one.
	//
	// FAILS CLOSED, which is the opposite polarity to [Ledger] and
	// deliberately so. That one asks "has this work been done", whose safe
	// answer is to re-run; this one asks "may I start", whose safe answer
	// is to wait for the next tick. An error yields (false, err) and the
	// caller must NOT dispatch. An empty minute is an error.
	ClaimFire(ctx context.Context, minute, runner string, at time.Time) (bool, error)

	// FireMinuteClaimed reports whether any claim on the minute is held,
	// for any runner or for none.
	//
	// The scheduler's first tick after a restart asks it before recording
	// the most recent fire as missed: a minute any runner was claimed for
	// ran, or was deliberately held back, and was not missed. An error is
	// UNKNOWN and never a false: a store that could not be read and a
	// minute nobody claimed are different answers, and the second one
	// writes a skip into the schedule's history. An empty minute is an
	// error, because it names no minute at all.
	FireMinuteClaimed(ctx context.Context, minute string) (bool, error)
}

// Custody decides which data node keeps each stateless event batch (ADR-0025).
//
// A batch whose acknowledgement was lost is redelivered to whichever data node
// asks next, so it could land in two logs and the usage domain would bill a
// stateless node's spend twice.
//
// Create-only, written AFTER the rows, and the loser deletes: a node writes the
// batch, then claims it; whoever creates the key keeps it. Claim-then-write
// would leave a claimant that died between the two holding a batch nobody
// wrote; this order leaves a written copy with an unknown claim, settled next
// pass. A batch is acknowledged once its keeper is decided, or the store
// answers [ErrCustodyUndecidable], which no redelivery changes.
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
// it names no node. No claim this engine makes writes one, so the value came
// by hand, from another process, a later build's shape, or corruption.
//
// DEFINITE, never [ErrUnavailable]: it answers the same while the record
// stands, so reading it as an unreachable store would wait for ever. It is
// about ONE batch; go on to the next. It decides NOTHING about the copy: kept,
// it may duplicate the unreadable record's node's; deleted, it may be the only
// one. So the copy stays, counted in no node's day, until the record is removed
// or the batch passes the event log's retention. A backend that can only hold
// a record a claim wrote never answers it.
var ErrCustodyUndecidable = errors.New("coord: the record of this custody batch's keeper cannot be read")

// Rebases is the fleet's record of the instant a unit of work's derived
// operation ids are minted at, for the work whose own start is too old for the
// operation ledger to vouch for.
//
// A derived id carries the instant its work began, so a retry reproduces it;
// an attempt that finds that instant past the state log's horizon mints at its
// own (statelog.MintAt) and records it HERE, keyed by the id seed. Every later
// attempt inherits it and can run on another node, so the record is the
// fleet's: a successor on its own database minted anew and wrote a second copy
// of every write. A read and a conditional write, the state log's rule applied
// between them; the loser of two racing attempts is refused and keeps the
// winner's instant. Retention is the bucket's age, [RebaseRetention].
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

	// Standing is what the record written states of itself, written in the
	// same message as Put ([RunStanding]); nil states none.
	Standing *RunStanding
}

// Validate refuses a change that both writes and removes the record, and a
// standing beside anything but a value written.
func (c SandboxRunChange) Validate() error {
	switch {
	case c.Delete && len(c.Put) > 0:
		return errors.New("coord: a change to a sandbox run either writes it or deletes it, " +
			"never both")
	case c.Standing != nil && len(c.Put) == 0:
		return errors.New("coord: a sandbox run's standing is stated with the value written, " +
			"never beside a delete or no write")
	}
	return nil
}

// RunStanding is what a run's record states of itself beside its value: its
// seat, its handle's digest and the lease epoch stamped on it, the fields the
// boot's epoch floor and a seat's listings select on. Written in the SAME
// message as the value, so the two never disagree, and listed without the
// value ([SandboxRuns.SandboxRunStandings]): a record is up to [MaxValueBytes],
// a standing a few hundred bytes. The handle is a digest because a handle has
// no bound on its length, and a standing's every field has a widest form.
type RunStanding struct {
	// Seat is the seat the record names; [uuid.Nil] names none.
	Seat uuid.UUID
	// Handle is the SHA-256 of the handle the record names ([RunStanding.Names]).
	Handle [sha256.Size]byte
	// OwnerEpoch is the lease epoch stamped on the record; 0 is none.
	OwnerEpoch int64
}

// StandingOf is the standing of a record naming seat and handle, stamped at epoch.
func StandingOf(seat uuid.UUID, handle string, epoch int64) RunStanding {
	return RunStanding{Seat: seat, Handle: sha256.Sum256([]byte(handle)), OwnerEpoch: epoch}
}

// Names reports whether the standing's record names handle.
func (s RunStanding) Names(handle string) bool { return s.Handle == sha256.Sum256([]byte(handle)) }

// StoodRun is one record's standing as its last write stated it.
type StoodRun struct {
	TurnID  string
	Version uint64
	// Standing is nil where the last write stated none: an older build's,
	// or one stating nothing. Such a record is judged on its value, read whole.
	Standing *RunStanding
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

// SandboxRuns is the fleet's record of detached executors, one row per turn.
//
// The value is opaque: every decision on its fields (tail claim, epoch fence,
// pause expiry) is internal/sandbox's, handed to [SandboxRuns.MutateSandboxRun].
// Every read raises rather than answering empty: "no run" would start the work
// again and abandon a box.
//
// Shared because a run is detached and its seat moves: the successor node
// recovers it, and a per-node store left a billed box running (internal/
// sandbox: "a detached run belongs to its row"). A watch is of one run's key
// or one part's, so a log waiter hears the log's writes alone. What a job can
// make large is a [RunPart] beside the record, never on it.
type SandboxRuns interface {
	// SandboxRun reads one run's record.
	SandboxRun(ctx context.Context, turnID string) (Record, bool, error)

	// SandboxRuns returns every record, by turn id — the records ALONE,
	// never a part beside one.
	//
	// Every listing this serves (the boot recovery pass, the pause reaper,
	// the clarification match) filters on fields coordination cannot see,
	// so there is one read and the caller decodes. The set is bounded by the
	// number of seats that can be mid-run at once, which is what makes that
	// affordable. One seat's runs and the epoch floor select on the standing
	// instead ([SandboxRuns.SandboxRunStandings]).
	SandboxRuns(ctx context.Context) ([]Record, error)

	// SandboxRunStandings returns every RECORD's standing as its last write
	// stated it, by turn id, certified as [SandboxRuns.SandboxRuns] is and
	// raising rather than answering short. No value is read but a record's the
	// listing missed in its pass; a record stating none is listed with a nil
	// [StoodRun.Standing], for its reader to read whole.
	SandboxRunStandings(ctx context.Context) ([]StoodRun, error)

	// CreateSandboxRun writes a new record, reporting whether it was new,
	// stating standing in the same write (nil states none). A turn id that
	// already exists is left alone: the id is the kick-off turn's, so a second
	// create is a retried launch, not a second run.
	CreateSandboxRun(ctx context.Context, turnID string, value []byte, standing *RunStanding) (bool, error)

	// MutateSandboxRun changes one run's record by a decision taken on it as it
	// stands, answering the record as left and whether a change landed.
	// decide is handed the record and answers the change ([SandboxRunChange],
	// zero value none). A write another writer beat is decided again on the
	// record as it now stands, within the attempts every fleet race gets
	// (internal/coord/kv casline.go), so decide may run more than once, must
	// decide from that record alone and holds nothing of the store's. No
	// record: decide is not called, the answer (Record{}, false, nil). decide's
	// error returns as it is; a last attempt lost is [ErrUnavailable]. A Put
	// answers the record as written, a Delete the one removed. A Delete removes
	// the record first, then its parts, never a part of a relaunched turn's new
	// record; a part left behind is the sweep's.
	MutateSandboxRun(ctx context.Context, turnID string,
		decide func(current Record) (SandboxRunChange, error)) (Record, bool, error)

	// WatchSandboxRun streams one run's record: its state when the watch
	// began — [SandboxRunUpdate.Found] false where it has none — then each
	// state it reaches after that, a delete as Found false.
	//
	// The first update is certified as a listing's answer is: a record that
	// existed throughout the call is never answered absent, though it can lag
	// the newest write by a replica's lag; a failure to read it is the call's
	// error. After it no update is older than
	// one delivered and the newest state is always delivered, but a state a
	// later write replaced may be passed over: a watcher holds the current
	// state, not the history. A CLOSED CHANNEL (ctx ended, watch failed) IS
	// NOT A DELETE: the watcher keeps what it knew and watches again.
	WatchSandboxRun(ctx context.Context, turnID string) (<-chan SandboxRunUpdate, error)

	// WatchSandboxRunPart streams the part kept beside turnID's run:
	// [SandboxRuns.WatchSandboxRun]'s contract on the part's own key: the
	// certified first state ([SandboxRunUpdate.Found] false where the run has
	// no such part), then each write of the part and its removal, which the
	// record's delete makes, as Found false. A write of the record or of
	// another part is none of this watch's. Record.Key is the turn id. A
	// CLOSED CHANNEL IS NOT A REMOVAL: the watcher stopped hearing and reads
	// the part again as it watches again. A part that is not one of
	// [RunParts] is an error.
	WatchSandboxRunPart(ctx context.Context, turnID string, part RunPart) (<-chan SandboxRunUpdate, error)

	// SandboxRunPart reads the part kept beside turnID's run, as the store's
	// newest write left it: a log appended a moment ago is in the answer.
	// Record.Key is the turn id. False is a run with no such part — one
	// whose record has none written yet, or one that is gone. A part that is
	// not one of [RunParts] is an error.
	SandboxRunPart(ctx context.Context, turnID string, part RunPart) (Record, bool, error)

	// MutateSandboxRunPart is [SandboxRuns.MutateSandboxRun]'s read-decide-
	// write on the part's own key, answering the part as left and whether a
	// change landed. decide gets the part and whether there is one, and
	// answers its next value whole; empty writes nothing. It never reads the
	// record: a caller whose condition is on the record keeps the facts in the
	// part, so one compare-and-swap holds condition and write together.
	//
	// Only beside a record: a write that finds, once landed, that the record
	// was removed removes what it wrote and answers (Record{}, false, nil).
	// An UPDATE does not read the record, so a part a removal left behind
	// still takes updates until the sweep removes it; every reader comes
	// through a record and each part names the job or run it was written for.
	MutateSandboxRunPart(ctx context.Context, turnID string, part RunPart,
		decide func(current Record, found bool) ([]byte, error)) (Record, bool, error)

	// SessionClaims are the runs the minted session ids are bound to
	// ([SessionClaim]), kept beside the runs' records and carrying their
	// lifetime.
	SessionClaims

	// ValueCrossing is how long a change of a run's value at what one key
	// holds ([MaxValueBytes]) takes to cross this store's connection, for the
	// read its decision is taken on and every send of the write:
	// [MaxValueCrossing] where the connection crosses a link (a node without
	// `data`, an external cluster), NONE in the store's own process.
	//
	// A caller bounding a change for a reason of its own (a slow store, an
	// election) bounds it by that reason PLUS this, never the reason alone:
	// every change moves the value whole and the store applies the crossing
	// only to a write whose caller set no deadline, so a bare reason cut a
	// multi-megabyte value mid-crossing and read it as an unanswering store
	// (crewlet/crewlet#1076, #1149). Sized at the bound, not the value.
	ValueCrossing() time.Duration
}

// RunPart names one part kept beside a sandbox run's record under a key of its
// own, because every reader decides on the record whole: every flip is a
// read-decide-write of it, every listing and git request of a coding box reads
// it. A part lives and dies with its record, and the store holds that:
// [SandboxRuns.MutateSandboxRunPart] undoes a write that raced the record's
// removal, a delete removes every part, and a part left by a node that stopped
// between the two is swept with the record's marker ([Markers.SweepMarkers]).
type RunPart string

const (
	// RunLog is the run's bridged tool log, up to internal/sandbox's two
	// hundred calls. On the record a full log was half a megabyte, moved
	// through the stream leader by every write; beside it, the record is a
	// few kilobytes.
	RunLog RunPart = "log"
	// RunHanded is the secrets the run's job's box was handed, sealed, up to
	// internal/sandbox's 64 KiB before sealing; few readers need it.
	RunHanded RunPart = "handed"
	// RunLeads is the turn's leads, each sealed by the job whose brief first
	// told it, up to 512 KiB: what carried each session of the run on, kept
	// for a later brief to a session that may lack its own. Written by one
	// compare-and-swap BEFORE the record's write; relaunches keep it.
	RunLeads RunPart = "leads"
)

// RunParts is every part a run's record can have beside it: what a delete of
// the record removes, and what the sweep looks for.
func RunParts() []RunPart { return []RunPart{RunLog, RunHanded, RunLeads} }

// Valid reports whether p is one of [RunParts].
func (p RunPart) Valid() bool { return p == RunLog || p == RunHanded || p == RunLeads }

// SecretRecord is one stored credential, as coordination holds it.
//
// Sealed before it arrives: Value is the Tier A keyring's envelope, so
// coordination never holds a key. KeyID rides beside it so a rotation sweep
// finds rows sealed under a retired key without opening any.
//
// Provenance is four fields (author, author kind, the credential acted
// through, the write path; internal/secrets' Author and Source), because a
// record naming only the credential lost its author when that credential's row
// was swept. They are plain strings, stamped and checked by the write path.
type SecretRecord struct {
	Name          string
	Value         string
	KeyID         string
	UpdatedAt     time.Time
	UpdatedBy     string
	UpdatedByKind string
	OperatorID    string
	Source        string

	// OpID is the operation key of the create-only write that sealed the
	// row (internal/api/opkey, scoped by its principal), so the same
	// request retried can finish what it sealed without the store being
	// read for a value. Empty for every other write, which clears it.
	// NEVER ANSWERED: no route, listing, log or audit carries it.
	OpID string

	// WriteOnly marks a value only the engine's resolver opens; every reveal
	// refuses it. Kept by later writes and a rekey (internal/fleetsecrets).
	WriteOnly bool

	// Version is the store's own revision of the row, set on every read
	// and ignored on every write: what [Secrets.UpdateSecret] is
	// conditioned on. Store-wide rather than
	// per row, as a KV revision is, so a row deleted and written again
	// never hands back a version an older incarnation of it already used.
	Version uint64
}

// Secrets is the fleet's encrypted credential store.
//
// Fleet-wide like the company config ([Plane.Activate]); a per-node store meant
// a rotation reached only the node the CLI pointed at. Coordination never sees
// plaintext: every value is an envelope the Tier A cipher sealed, whose key
// lives on each node's disk, so a peer reading the bucket learns which names
// exist and when they changed, not what they are.
//
// Every read raises rather than answering empty: "no such credential" would
// render as an unset ${VAR}, an auth failure blamed on the vendor. Held,
// definitively absent, or unknown.
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

// Integrations is the fleet's record of where each external surface got to,
// read by whichever node serves the API (rarely the duty holder): per surface
// a small status and, apart, the findings it names (integrations.go). Both
// values are opaque, internal/integration's vocabulary, and never age: an
// expired status would re-provision a surface the loop had agreed with.
//
// Last write wins: every writer holds the surface's provisioning lease across
// its re-read of the status and its write, so no second writer loses an
// update. Every read RAISES on an unreachable store rather than answering
// empty: "never reconciled" and "cannot be read" send the loop opposite ways.
type Integrations interface {
	// IntegrationStatuses returns every recorded status, keyed by the
	// surface it describes, and never a surface's findings.
	IntegrationStatuses(ctx context.Context) (map[string][]byte, error)

	// IntegrationStatus returns one surface's status, false where none is
	// recorded.
	IntegrationStatus(ctx context.Context, kind string) ([]byte, bool, error)

	// IntegrationFindings returns every recorded findings record, keyed by
	// surface.
	IntegrationFindings(ctx context.Context) (map[string][]byte, error)

	// IntegrationFindingsOf returns one surface's findings record, false
	// where none is recorded.
	IntegrationFindingsOf(ctx context.Context, kind string) ([]byte, bool, error)

	// PutIntegrationStatus records one surface's status, replacing any
	// prior one, and leaves its findings record as it is.
	PutIntegrationStatus(ctx context.Context, kind string, value []byte) error

	// RecordIntegrationPass records what a pass found: its findings, or
	// none (nil removes the record), and then the status naming them, so
	// a status never names findings a reader cannot find written.
	RecordIntegrationPass(ctx context.Context, kind string, status, findings []byte) error

	// DeleteIntegrationStatus drops a surface's status and its findings once
	// its block has left the company document. It removes the RECORDS and
	// nothing at the third-party app: see internal/integration's package doc
	// for why a deleted block is not a request to destroy what a pass
	// created.
	DeleteIntegrationStatus(ctx context.Context, kind string) error
}

// AtlasSources is where the atlas collect duty keeps each source's standing
// (internal/atlas/collect): when its vendor was last read whole, its digest,
// the calls and withheld values it cost, and, as a record of its own held to
// a class of its own (atlasclasses.go), the entries it stopped seeing.
//
// The duty moves between nodes, and the reader of a standing is rarely the
// node that ran the read. Only the current value is kept: an unchanged read
// writes nothing to the atlas log, so the last read and its digest live here.
// Its own bucket: the integration statuses' reader forgets every key the
// document does not declare. Opaque and unaged for their reasons: an expired
// standing would read as a source never read, and the duty would read it all.
type AtlasSources interface {
	// AtlasSourceStatuses returns every recorded standing, keyed by the
	// source it describes, and none of their misses. RAISES rather than
	// answering empty on an unreachable store, for the integration
	// statuses' reason: "no source was ever read" and "the store cannot be
	// read" send the duty down opposite paths.
	AtlasSourceStatuses(ctx context.Context) (map[string][]byte, error)

	// PutAtlasSourceStatus records one source's standing, replacing any
	// prior one. LAST WRITE WINS: the collect duty is the one writer, a
	// fleet singleton that re-claims its lease before every source it
	// visits, so no two writers hold one key.
	PutAtlasSourceStatus(ctx context.Context, kind string, value []byte) error

	// AtlasSourceMisses reads one source's misses, false where none is
	// recorded. RAISES on an unreachable store: "nothing missed" would
	// restart the day every miss waits out before it retires.
	AtlasSourceMisses(ctx context.Context, kind string) ([]byte, bool, error)

	// PutAtlasSourceMisses records one source's misses, replacing any prior
	// ones, the last write winning for the standing's reason.
	PutAtlasSourceMisses(ctx context.Context, kind string, value []byte) error

	// DeleteAtlasSourceStatus drops a source's standing and its misses once
	// the company no longer connects it: the misses first, so a failure
	// leaves the standing that names the source to the next sweep. It
	// removes the records and nothing on the atlas log: what the source
	// recorded stays until a read retires it or a person does.
	DeleteAtlasSourceStatus(ctx context.Context, kind string) error
}

// AtlasCodeScans is where the atlas's scan of each repository's deploy and
// dependency files keeps its standing (internal/atlas/codescan): the default
// branch's tip last read whole, files listed and read, and what it refused.
// Each is filed under the host it is read from ([AtlasCodeScanKey]), beside
// a forge's own standing of its last pass (repo ""), and held to
// [SmallValueBytes].
//
// A repository's home moves between Gitlet nodes, and the node taking over
// must know which tip was read. Only the current value is kept; what the
// files declared is on the atlas log. Its own bucket, since the atlas
// sources' reader forgets every key that is no source; opaque and unaged for
// the atlas sources' reasons.
type AtlasCodeScans interface {
	// AtlasCodeScans returns the standing of every repository of host,
	// keyed by the repository. RAISES rather than answering empty on an
	// unreachable store: "never scanned" would read every repository again.
	AtlasCodeScans(ctx context.Context, host string) (map[string][]byte, error)

	// AtlasCodeScan reads one standing, false where none is recorded:
	// host's repository repo, or host's own for repo "". RAISES as the
	// listing does.
	AtlasCodeScan(ctx context.Context, host, repo string) ([]byte, bool, error)

	// PutAtlasCodeScan records one standing, replacing any prior one. LAST
	// WRITE WINS: a repository's home is its one writer, and two nodes that
	// each take themselves for home write the same reading of the same
	// commit; a forge's standings are the collect duty's, a fleet singleton.
	PutAtlasCodeScan(ctx context.Context, host, repo string, value []byte) error

	// DeleteAtlasCodeScan drops one standing once it is no longer kept.
	// One that is not there is the outcome asked for.
	DeleteAtlasCodeScan(ctx context.Context, host, repo string) error
}

// AtlasSourceRefreshes is where a person's ask that an atlas source be read
// now waits for the collect duty (internal/atlas/collect): who asked, and
// when.
//
// The node a person asks on is rarely the one holding the duty, so the ask is
// a record the whole company reads. Current value only.
//
// Its own bucket: the duty replaces the atlas sources' standing whole, so an
// ask written beside a visit in flight would be lost to the visit's save. The
// value is opaque, with no retention: an age would drop an unreached ask
// silently. Each ask is held to [SmallValueBytes].
type AtlasSourceRefreshes interface {
	// AtlasSourceRefreshes returns every waiting ask, keyed by the source
	// it names. RAISES rather than answering empty on an unreachable store:
	// "nobody asked" and "the store cannot be read" are different answers
	// to a person who just asked.
	AtlasSourceRefreshes(ctx context.Context) (map[string][]byte, error)

	// PutAtlasSourceRefresh records an ask for one source, replacing any
	// prior one. LAST WRITE WINS: two people asking at once ask for the
	// same read.
	PutAtlasSourceRefresh(ctx context.Context, kind string, value []byte) error

	// DeleteAtlasSourceRefresh drops a source's ask once it is honoured or
	// its source is gone. One that is not there is the outcome asked for.
	DeleteAtlasSourceRefresh(ctx context.Context, kind string) error
}

// MailboxRecord is the fleet's record of one agent seat's durable mailbox.
//
// A mailbox is a durable subscription named from the seat's handle. Once the
// seat leaves the company the subscription retains what is addressed to it for
// ever, and a seat later added under the same handle would resume that backlog
// under a role that never wrote it.
//
// So every node records a handle here BEFORE creating the subscription, and the
// maintenance sweep retires those whose seat has been gone long enough. Only a
// record carries the absence stamp, retirement mark and compare-and-set
// version. What an absence means is internal/maintenance's decision; this
// package stores two instants and a version, certifying that they round-trip.
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
// Every read raises rather than answering empty: "no record" would let a sweep
// conclude nothing is left to retire, or a registering node write over a
// retirement still deleting the subscription it is about to create.
//
// Every change is compare-and-set: a write carries the version read, false is
// a lost race to re-read and re-decide, and a success returns the next version.
// Last write wins would let a sweep overwrite a returning seat's registration.
// No retention, for the channel bucket's reason: age cannot tell a present seat
// from one that left, so removal is the sweep's decision.
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
// A KV backend removes a key by appending a delete or purge marker. Families
// no clock may reap (activation pointer, runs, channels, mailboxes, secrets,
// statuses, registers, read state) keep every marker for the deployment's
// life, each a leader read on every listing meeting it (internal/coord/kv
// walk.go). So markers are swept as a decision under the maintenance duty.
//
// It changes no answer: after sweeping every marker, every read, listing,
// create and conditional write answers as before. A backend with no markers
// satisfies it by sweeping nothing.
type Markers interface {
	// SweepMarkers removes every marker written before cutoff from the
	// record families no clock ages, reporting how many went. A failure
	// part-way reports the markers already removed beside the error.
	//
	// It also removes a part ([RunParts]) written before cutoff whose run's
	// record, read after the part is found, is gone ([SandboxRuns]).
	SweepMarkers(ctx context.Context, cutoff time.Time) (int64, error)
}

// ActivationBodies is the sweep of the activation's bodies no pointer names,
// at its own horizon ([BodyRetention]), under the maintenance duty.
type ActivationBodies interface {
	// SweepBodies removes every body ([Plane.Body]) written before cutoff
	// that the pointer, read after the body is found, does not name,
	// reporting how many went. A flip names a body written at most half of
	// [BodyRetention] before it, so a cutoff [BodyRetention] back never
	// removes a body a flip in flight will name. The one answer it changes:
	// the body of an activation the pointer moved past becomes absent.
	SweepBodies(ctx context.Context, cutoff time.Time) (int64, error)
}

// Fleet is a backend that serves all of the shared state, which is what the
// contract suite certifies and what the engine wires from.
//
// One interface at the construction seam and a narrow one at each call site,
// because a consumer that could reach the whole store would eventually use it.
type Fleet interface {
	Counter
	Claims
	Ledger
	SeenMarks
	Budgets
	Plane
	Channels
	Follows
	ChatReads
	HomeBoards
	Fires
	Rebases
	SandboxRuns
	Secrets
	Integrations
	AtlasSources
	AtlasSourceRefreshes
	AtlasCodeScans
	SeatAccess
	GrantRequests
	AccessReviews
	AtlasWakes
	ConversationHolds
	BoxReadings
	Mailboxes
	ObjectStores
	TrashReaps
	SeatPauses
	ModelRetries
	DeviceSignIns
	ModelChecks
	MCPListings
	NodeStatusRecords
	MCPRestarts
	MCPChecks
	SSHTests
	IssueReports
	InsightRecords
	OAuthGrants
	OAuthSignIns
	GoogleIdentities
	MCPAccepted
	PositionRegister
	HoldRegister
	FloorRegister
	FeedMarkRegister
	BackupRegister
	MaintenanceRegister
	Markers
	ActivationBodies
	Custody
	UploadReservations
	SaveAllowances
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
// Two nodes naming different backends in Tier A would split the company's
// files with nothing failing, and each side's collector would delete nothing
// the other wrote. So the first node to open the store records its backend,
// create-only and with no age, and every later node compares its own against
// it and refuses to boot on a mismatch.
//
// No lock (ADR-0027): an object's key is minted for its one upload, so no
// writer re-uses an object the collector could be deleting.
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
// The reaper is a fleet duty, so it moves: a node taking it over reads when
// the next pass is due from here rather than reaping again at once. The report
// is a whole account, so the last write is the answer, whoever wrote it.
//
// No lock: a domain decides a purge against its own trash in a snapshot the
// broker arbitrates, so overlapping reapers purge nothing twice. This record
// is never a claim anybody reads to decide a purge.
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
// Company-wide: no chat backend exposes per-bot thread state, and one node
// claims each inbound message, so a follow in a node's own database is
// invisible to the node claiming the next reply (ADR-0003). The seat is its
// handle (ADR-0013); which reason is stronger is internal/notify's vocabulary.
//
// Retention is the bucket's age: every re-assert rewrites the record, so the
// age is a last-activity stamp. Nothing else removes a follow.
type Follows interface {
	// Follow records that a seat follows a thread, or refreshes the follow
	// it has, and answers the reason the record holds afterwards.
	//
	// The reason is chosen against the record it replaces: choose gets the
	// reason held and whether a follow is held, and the write lands only on
	// that record; a loser re-reads and asks again. Deciding from an earlier
	// read could leave a seat named a moment before a broadcast following it
	// for the broadcast. choose may run more than once, so it must be pure.
	//
	// Every call writes, an unchanged reason included: the write moves the
	// bucket's age, or an active thread's follow would expire.
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
