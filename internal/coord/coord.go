// Package coord is cross-process resource ownership: TTL leases with a
// fencing token, plus the shared counters and ledgers a fleet coordinates
// through.
//
// This is the primitive every multi-node duty is built on. A node claims a
// resource, renews it on a heartbeat, and loses it by crashing or releasing.
// A resource name is SEGMENTED, and its leading segment is its [Class] — the
// kind of thing the lease is for. That is not decoration: the key a resource
// becomes carries its class as a subject token of its own, so a whole class
// is a wildcard the broker can match and a listing asks for one kind rather
// than reading every lease in the fleet. Five classes name five kinds:
//
//   - seat:{id} — one agent seat this node runs, named by the agent id
//     derived from its handle (ADR-0013), as its mailbox is.
//   - worker:{duty} — a fleet singleton: the maintenance sweep, the
//     scheduler tick, the lifecycle pass. A duty's TTL is sized from its own
//     cadence rather than from the seat heartbeat, so it runs from seconds to
//     hours, and every backend must honour any of them up to [MaxDutyTTL]
//     whatever its seat lease TTL is. See [MaxDutyTTL] for why that is a
//     contract rather than a backend's choice.
//   - node:{id} — the node's own PRESENCE, which is the kind that is easy to
//     forget and the one the placement math counts. Membership is not work:
//     a node holds it to say it is alive, ListLive(ClassNode) is the fleet
//     roster, and the fair-share target every node computes for itself is
//     ceil(seats / that count). A node that stops renewing its presence is
//     not merely idle — it raises everyone else's share.
//   - taker:{incarnation} — one engine PROCESS saying it is still keeping
//     the executor sessions it began taking ([ClassTaker]): held from its
//     sandbox coordinator's start until its last keep ends, through a drain
//     that has given presence up, and claimed afresh by every process, so a
//     restart never waits on its predecessor's. Its tokens come from ONE
//     counter for the whole class ([CounterResource]).
//   - browser_control:{launch} — a person's control of one executor launch's
//     browser ([BrowserControls]), its tokens from ONE counter, as a taker's.
//
// A caller may lease under a class of its own as well — the tracker's walk
// claims are `move:` and `merge:` — and every such class is held to
// the same ceiling a duty is, never to the seat lease TTL: only a seat and a
// presence lease run on the seat heartbeat ([HeldToSeatTTL]).
//
// What belongs here rather than in a node's own database is ADR-0003, the
// tri-state below is ADR-0005, why [ProtocolVersion] REFUSES a lower-protocol
// peer where an event envelope round-trips one is ADR-0016, and why a token
// budget is counted per calendar window — rolled inside the charge that
// crosses a boundary, with no reset — is ADR-0019 (budget.go). Why every node
// publishes what its MCP servers list, under its presence epoch, and the fleet
// reads the union of the live nodes' listings is ADR-0074 (mcplisting.go).
//
// Three rules carry the correctness of everything above:
//
//  1. The epoch is the fencing token, and TryAcquire returns it so callers
//     can thread it into their conditional writes. It is MONOTONIC FOR THE
//     LIFETIME OF THE RESOURCE — releasing a lease must not reset it,
//     because a counter that restarts hands the next owner a token the
//     previous one is still using. A counter may serve more than one
//     resource ([CounterResource]): every token it mints is above every
//     token it minted before, for whichever resource, so each of them is
//     monotonic all the same.
//
//     What is actually fenced BY THE WRITE is every coordination write here
//     and each executor's row in the sandbox runs bucket, whose owner_epoch
//     a lower one cannot outrank. A seat-scoped SQL write is not: those are
//     made safe by being keyed on the work rather than on the writer, and
//     the per-table inventory is in docs/concepts/seat-ownership.md rather
//     than restated here. What stops the zombie MAKING them is the seat
//     fence — [seat.Host.Fence], checked at the top of every tool-loop
//     round and before each of that round's calls — which is a different
//     guarantee from a write that bounces, and the difference is the
//     window between two checks.
//
//  2. A lapsed lease cannot be renewed, only re-acquired — and re-acquiring
//     bumps the epoch even for the same owner. During the gap the owner's
//     in-flight work was unprotected, so it must be fenced against its own
//     past self. Only an unbroken same-owner hold keeps its epoch.
//
//  3. Owner identifies a process INCARNATION, not a machine. Two processes
//     sharing an owner string would both hold the resource at the same
//     epoch. The stable node id goes in Preferred, where restart-stability
//     is what you actually want.
//
// # The tri-state, and why Go gets it for free
//
// Every method that answers "do I hold this?" has three answers, and
// conflating two of them is the single most incident-hardened lesson carried
// into this engine. (value, error) expresses it natively:
//
//	(lease, nil)  — held. Proceed.
//	(nil, nil)    — definitively NOT held: lapsed, moved, or advanced.
//	                Shed the work it covered, now. (A claim's definite
//	                "no" also says why — see [Refusal] — and is still
//	                never an error.)
//	(nil, err)    — UNKNOWN. The store could not be reached or did not
//	                answer. This says NOTHING about ownership: the record
//	                is untouched and probably still held. Keep the seats,
//	                stop admitting new work, and retry until the TTL is
//	                genuinely elapsed.
//
// Treating unknown as loss tears a healthy company down over a two-second
// store blip. Treating unknown as refusal makes a node stop refreshing its
// own presence during exactly the outage it should ride out. Any non-nil
// error means unknown — there is no error worth special-casing into a
// definite answer about who holds what.
//
// ONE CLAIM'S ERROR IS A DEFINITE FACT ABOUT WHAT THE STORE HOLDS, and still
// not a fourth answer: [ErrCustodyUndecidable], a keeper record nothing can
// read. It decides no ownership, so the caller keeps and deletes nothing, as
// on unknown; but the store answered, and answers the same until the record
// goes, so waiting on it as on an outage would stall every batch behind it.
// So it is told apart, and the caller goes on to the next batch.
//
// # A read is given the time its bytes take to cross
//
// A read across a leaf's link is given the time its key class's bound
// ([HoldValue]) takes to cross at the slowest pace allowed, every key's
// class declared by its bucket (ADR-0175). A reader that
// cannot wait (a probe, a boot) reads a small record naming large bytes by
// size and digest; only the reader that needs them pays their crossing
// (ADR-0175). So the pointer names its body ([BodyRef]).
//
// # A definite answer is a CURRENT one
//
// The two definite answers are only worth their names if they reflect every
// write acknowledged before the read began — this node's own and every
// peer's. A backend whose store is replicated therefore answers from the copy
// that has applied every acknowledged write, and from no other: an answer read
// off a copy that is behind is "held as of that copy", which is the fourth
// answer argued against below, delivered as one of the two definite ones.
// coord/kv reads every record from its bucket's stream LEADER for exactly this
// reason, and certifies every listing against the leader's own key index (its
// kv.go, "Every single-key read is the leader's", and walk.go carry the
// measurements and the one residue: a leader cut off from its quorum answers
// from its own copy until it notices, while every write through it fails).
// The memory twin has one copy and meets it trivially.
//
// # A lease a read hands out is the store's word
//
// Get, ListLive and ListOwned hand out only leases that were live by the
// STORE's clock as it read them, and a reader acts on each as it is: who holds
// a resource is the owner Get names ([Holder]). Judging the lease again, its
// ExpiresAt against the reader's own clock, turns the store's "held" into a
// "not held" nobody said, on every node whose clock runs ahead of the store's
// by more than a lease's remaining time — at most its TTL, 45 s for a seat at
// the default — while a node and the broker member leading the bucket may each
// stand an hour from the fleet's clock, two hours from each other, before
// anything raises clock_skew (objstore.AssumedClockSkew). Such a node finds no
// holder for any seat, so it relays a box's call to nobody and asks no holder
// what only the holder can answer.
//
// How long a lease has left is the store's reading too: its ExpiresAt less the
// store's own clock ([Backend.Now]), which [TimeLeft] reads. Held against the
// reader's clock instead, every lease reads as run out on a node whose clock
// runs ahead of the store's by more than its TTL, and a 45-second lease as
// having an hour on a node an hour behind. The clock is read for a class,
// because a store need not keep one for every class: coord/kv keeps seats and
// presence in one bucket and every other class in another, each lease on its
// own bucket's clock.
//
// So a reader never reads a lease's deadline itself: a backend judges a record
// on its own clock, and [TimeLeft] is the one reading of a deadline outside a
// backend, against the store's clock.
// TestNoNodeJudgesALeaseTheStoreHandedOutOnItsOwnClock refuses a selection of
// [Lease.ExpiresAt] in every non-test file but a backend's (coord/kv's,
// coord/memory's), the contract suite's (coordtest) and [TimeLeft]'s, and Lease
// has no method that judges one.
//
// # Why coordination is not itself a replicated log
//
// The engine grows a durable-state framework — internal/statelog — whose
// shape is very close to this one: an ordered log of records, a conditional
// append per subject, and N identical SQL copies replaying it. A conditional
// append per subject IS this package's compare-and-set one layer down, so
// "it would not work" is not available as an answer and the refusal has to be
// argued. Five arguments, and each one is fatal on its own:
//
//  1. THE FRAMEWORK'S CENTRAL PROPERTY IS THE ONE A LEASE MUST NOT HAVE. A
//     replica can only ever be BEHIND, and that is what makes its answers
//     safe. A lease answers NOW. A replicated read of a lease table could
//     only ever say "held as of my checkpoint" — a FOURTH answer beside the
//     three above, and the one no caller can act on: a node ten seconds
//     behind would believe it holds a seat another node took nine seconds
//     ago, and would keep running turns on it.
//
//  2. A FENCING EPOCH MUST SURVIVE THE PAST BEING DELETED, AND A LOG DELETES
//     ITS PAST BY CONSTRUCTION. Rule 1 above needs the epoch monotone for the
//     resource's LIFETIME; coord/kv states the same thing from the other end
//     ("gaps in the counter are harmless; resets are not"). A node that
//     re-derived an epoch from a trimmed log would derive a LOWER one — the
//     exact reset the design forbids, produced by the retention mechanism
//     rather than by a bug. No amount of care in the framework fixes it,
//     because a log's whole contract is that old records go away.
//
//  3. THE ACTIVATION POINTER'S READERS ARE THE NODES THAT ARE BEHIND, so
//     putting it on a log is circular. Its revision is the epoch, and
//     internal/configplane reads it to decide whether THIS NODE IS BEHIND. A
//     node cannot use its position on a log to discover that its position on
//     that log is stale. There is no base case.
//
//  4. THESE RECORDS ARE BOUNDED, MUTABLE AND SHORT-LIVED — a bucket's shape
//     and a log's anti-shape. Presence renews on the 15-second reconcile
//     interval: twenty seats over five nodes is ≈ 50 M lease writes a year of
//     pure lock traffic, every byte of it retained for the log's age floor.
//     The framework makes this same argument about the tracker's own walk
//     claims at a four-hundredth of the scale, and moves them INTO
//     coordination: a claim is a lock, not state, and reproducing another
//     node's claim on a replica is meaningless because a replica must not act
//     on it.
//
//  5. THE FRAMEWORK'S OWN RETENTION GATE READS COORDINATION, so coordination
//     cannot read the framework. Every term that decides how far a log may be
//     trimmed — the positions register, the live trim holds, the eviction
//     tombstones and the backup floor — is a coordination read taken BEFORE
//     the log is purged. Putting them on the log would make the log's
//     retention a function of the log's own contents: a cycle with no base
//     case. It is also why the positions register is an AGELESS bucket, which
//     is the exact inversion of the presence bucket beside it, whose whole
//     purpose is that a node that stops reporting vanishes.
//
// # A person's own record here is their SEAT's
//
// Three records here are a PERSON'S, not the company's or a node's: their
// chat read state ([ChatReads]), Home board ([HomeBoards]) and Ask Crewlet
// instructions ([AssistantInstructions]). Each is keyed by the name
// iam.RecordOwner gives a principal's own record, which for everybody they
// admit is the human seat they act as, so each follows the rule every personal
// record follows (ADR-0032): it belongs to the SEAT, and the seat's next holder
// inherits it (the person themselves once reinstated, or a successor once they
// are moved to another seat or removed), exactly as the seat's tracker inbox,
// pins and personal views pass. Each ends when the seat leaves the company,
// never when its holder does; see chatread.go for why neither a person id nor
// an agent id is the key.
//
// # A seat's save ledger lives in the uploads bucket
//
// [SaveAllowances] keeps one ledger per seat of the executor sessions its runs
// saved as they worked (internal/sandbox). The WHOLE COMPANY has to agree on
// it: a save uploads a session's archive to the object store the fleet
// shares, and the archive it supersedes stays there, named by nothing, until
// the object collector could have deleted it. The ledger counts those bytes
// against the seat's session budget. Any node that drives one of the seat's
// jobs may save it, and a handover moves a job between nodes, so nodes
// counting alone would let a seat hold its budget once per node; one value
// written by compare-and-set lets two saves racing for the last of an
// allowance never both win.
//
// It is the UPLOADS BUCKET's [SaveLedgerClass], beside [UploadLedgerKey] and
// [UploadRecordClass], each bounded in uploads.go. What it counts is what an
// upload reservation counts, bytes nothing names until the collector could
// have deleted them, and the bucket's age ([UploadRetention]) is that hold
// already: a ledger nobody wrote for that long holds only lapsed entries, and
// a removed seat's ledger goes on its own, with no sweep.
//
// Every error is UNKNOWN, and the safe direction is not to save: a save its
// node could not count is one that may pass the seat's budget.
//
// # A node's runs waiting for an operator
//
// A node's status record (nodestatusrecord.go) says how many runs of the seats
// it holds wait for an operator ([NodeStatusRecord.ResumesBlocked]): runs whose
// executor's job finished and whose last resume there failed on a condition
// only an operator clears (internal/sandbox's Coordinator.ResumesBlocked). It
// is ON THE RECORD because the node holding the seat is the only one that
// knows, and it may be a satellite (`roles: [seats]`), whose own table judges
// only what it measures of itself (statelog.LocalKinds). So the
// `executor_resume_blocked` alarm is judged by every data node off the live
// records, as the model alarms are. The record carries THE COUNT, AND THE RUN
// WAITING LONGEST BY ITS SEAT, ITS CONDITION AND SINCE WHEN, the seat and the
// kind bounded ([MaxResumeBlockedKindBytes]), never how long it has waited,
// which moves on every pass and would rewrite the record on each. It is absent
// where nothing waits, and a node running no executor, or a build from before
// it, says nothing.
//
// # A live node not saying keeps what it said
//
// A reader that judges the fleet over and over, an alarm evaluation above all,
// reads [LiveNodeStatusesKept]: a live node whose record is not counted now
// keeps the record the reader counted on its previous read, while its presence
// stands at the epoch that record was written under. A LIVE NODE WHOSE RECORD
// IS NOT COUNTED IS A NODE NOT SAYING, never one whose rows went away: a record
// whose rewrites failed while the node lives ages out of its bucket
// ([StatusFreshness]) before the presence lapses, and read as a node holding
// nothing, a condition only that node held (a model's sit-out, a server's
// outage, a run waiting for the operator) cleared for one evaluation and was
// raised again on the next, with its lines and its wakes. A node whose
// presence is gone, or was claimed again at a new epoch, keeps nothing: what
// it held moves to holders that say their own, and a restarted node's memory
// went with its process. A reader that decides once, rather than judging a
// standing condition, reads [LiveNodeStatuses] itself, and one with a truer
// word for a node than its last record, its own memory for the node it runs
// on, leaves that node out of what it kept.
//
// # Maintenance mode
//
// A stream's byte ceiling is changed only while no request to it can be
// outstanding, whichever way the number moves: the broker refuses a reduction
// below current usage, so usage must be observed while nothing can move it,
// and a raise is as unsafe, because what makes a configuration write safe is
// knowing which requests are in flight. Closing a client does not retire one:
// a request the broker has received and queued lives in a server's receive
// buffer and connection state, and closing cancels only the client's wait.
// Both die with the broker PROCESS, so the fleet restarts into a mode that
// starts every broker and no publisher, and the exclusion is structural
// rather than observed.
//
// The exclusion and the operation are ONE record ([MaintenanceOperation]),
// because maintenance exists exactly while an operation is unresolved. As two
// records, an interrupted activation (state written, operation not yet
// created) is byte-identical to a finished resize whose cleanup was cut short,
// so any rule reading one releases the exclusion while a coordinator is still
// preparing; one record has no halves to crash between. Three things are
// deliberately not this record:
//
//   - THE COORDINATOR'S LEASE ([MaintenanceResource]) has a TTL, so another
//     maintenance-mode node can take the operation over. The record has none:
//     a key that forgets admits a publisher, and a coordinator that is paused,
//     GC-stalled or partitioned has not finished its operation.
//   - AN ADMISSION ([Admission]) is one node's POSITIVE record that it intends
//     to publish: a handshake, not an absence check. A node writes its
//     admission, then re-reads the operation; a coordinator writes the
//     operation, then reads the admissions, deleting the operation again over
//     one whose process still holds its node's presence and issuing no
//     request while any other stands. Either order is safe, because a node
//     that slipped in between leaves a durable key the coordinator's later
//     read sees, where an absence would have let it proceed.
//   - AN ACK ([MaintenanceAck]) is one participant's evidence that its
//     process restarted for the operation's current attempt; the seal is
//     established from these. Per node, because the barrier is about
//     processes, not a fleet-wide fact anybody could assert. Per LOG, because
//     a fleet resizing two logs restarts once for both and each boot
//     acknowledges each window: one per node, one window's acknowledgement
//     would replace the other's.
package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ProtocolVersion is the seat-host protocol this build speaks. A node refuses
// to claim while a live presence or seat lease ([ProtocolGateCounts]) is held
// at a LOWER protocol (ADR-0016): builds that disagree on what holding a lease
// means are each correct and jointly wrong. Looking only down keeps older
// nodes working, so schema evolution is additive-only and a downgrade across a
// bump needs a full drain: a lower build takes over lapsed leases unchecked.
//
// Bump it when the MEANING of holding a lease changes, never for a new field.
// At 1, holding a seat means consulting the completion ledger before a
// delivery, satisfying the role's placement, charging the WINDOWED token
// counters, and naming its lease, mailbox and subjects by the seat's ID
// (ADR-0013), never its handle.
const ProtocolVersion = 1

// ProtocolGateCounts reports whether a lease on resource is one the protocol
// gate judges, and so one [Backend.FleetProtocolFloor] counts; every backend
// asks this one function so the twin and the store agree.
//
// PRESENCE AND SEATS ONLY: the gate asks whether a lower-protocol node is
// alive or still running seats. A seat lease outlives presence through a
// drain, so presence alone would let a newer node claim beside a draining
// older one. A DUTY (`worker:`) or any other class is not counted: it is
// claimed ungated ([AcquireOptions.Ungated]) and outlives its holder by up to
// [MaxDutyTTL], so counting it stalls every newer seat claim for hours after
// an older node crashes, over a lease its presence and seats already speak for.
func ProtocolGateCounts(resource string) bool {
	return ClassNode.Holds(resource) || ClassSeat.Holds(resource)
}

// ErrUnavailable is the canonical "store could not answer" error. Backends
// wrap their transport failures in it; it gives the common case one name in
// logs and lets tests assert the tri-state.
//
// Callers do not switch on it: an error is unknown unless its method's
// contract names it a definite answer — a lost compare-and-set
// ([ErrActivationRaced], [ErrMaintenanceMoved], [ErrObjectBackendMoved]), a
// request the store will not honour ([ErrTTLTooLong]), an unreadable custody
// record ([ErrCustodyUndecidable]). Reading "not ErrUnavailable" as definite
// would take every failure a backend did not wrap for a fact.
var ErrUnavailable = errors.New("coordination store unavailable")

// ErrWriteUnknown marks an [ErrUnavailable] that came AFTER a write was sent:
// the store's answer to it was lost, so the write may have landed.
//
// For methods that read before they write, whose caller otherwise cannot tell
// a failed read (nothing changed: ask again) from a lost put reply (it may
// have changed: look first). A method that marks it says so in its contract
// ([ChatReads] does), and every error it returns WITHOUT it changed nothing.
var ErrWriteUnknown = errors.New("coord: the write was sent and its answer was lost")

// ErrClaimInFlight is a claim answered while another claim of the same
// resource BY THE SAME OWNER is still minting its fencing epoch: neither a
// grant nor a refusal is true yet, and the sibling is one round trip from
// committing or giving the record back.
//
// It is an [ErrUnavailable] under errors.Is, the tri-state's unknown arm, so
// every caller branching on that treats it alike; named so a caller that
// knows the sibling is its own can tell it from an unreachable store.
var ErrClaimInFlight error = claimInFlight{}

// claimInFlight is [ErrClaimInFlight]'s type: comparable, so errors.Is finds
// the sentinel itself, and an ErrUnavailable by its Is.
type claimInFlight struct{}

func (claimInFlight) Error() string {
	return ErrUnavailable.Error() + ": a claim by this owner is still minting its epoch"
}

// Is reports that the in-flight claim is the unknown arm, [ErrUnavailable].
func (claimInFlight) Is(target error) bool { return target == ErrUnavailable }

// ErrTTLTooLong reports a claim or renew asking for a TTL the store cannot
// honour. An error, never a [Refusal] (nobody else holds the resource), and
// never a silent clamp: a holder renews and counts on its lease from the TTL
// it asked for ([ExclusiveThrough]), so a deadline quietly cut short has it
// renewing too late and counting on a lease that has lapsed.
var ErrTTLTooLong = errors.New("coord: ttl exceeds what the store can honour")

// MaxDutyTTL is the longest TTL a lease outside seats and presence
// ([HeldToSeatTTL]) — a `worker:` duty, a walk's claim, any caller's class —
// may ask for, and every backend honours up to it whatever its seat lease TTL:
// a duty outlives several of its own ticks, unrelated to how fast a dead
// node's seats move. The embedded KV caps a lease at its bucket's age, so
// every backend, the twin included, enforces it through [CheckLeaseTTL], and
// the contract suite certifies both sides of it.
//
// Three hours: the hourly learning passes' lease survives three ticks, as
// every duty's does (an engine test pins it to the longest duty TTL). It is
// also [MarkerRetention]'s horizon by definition: no operation outlives the
// claim it runs under. Raising it is safe on a running fleet; lowering, free.
const MaxDutyTTL = 3 * time.Hour

// HeldToSeatTTL reports whether a lease on resource is held to the backend's
// SEAT lease TTL — a seat or a node's presence — rather than to [MaxDutyTTL];
// every backend asks this one function so the twin and the store agree.
//
// Only those two renew on the seat heartbeat. `coordination.lease_ttl_seconds`
// is how fast a dead node's seats move, and holding any other lease to it
// would make a failover knob silently refuse or permit work nobody tuned it
// for. The set matches [ProtocolGateCounts] for a different reason, so a class
// added to one is not thereby added to the other.
func HeldToSeatTTL(resource string) bool {
	return Class(ResourceSegments(resource)[0]).HeldToSeatTTL()
}

// CheckLeaseTTL refuses a claim or renew whose TTL exceeds [MaxDutyTTL] on any
// lease the seat lease TTL does not hold ([HeldToSeatTTL]); a seat or presence
// lease is left to the backend. Backends MUST call it rather than compare the
// constant, so the rule has one message and a backend able to honour longer
// TTLs (the twin) still refuses what another backend cannot.
func CheckLeaseTTL(resource string, ttl time.Duration) error {
	if HeldToSeatTTL(resource) || ttl <= MaxDutyTTL {
		return nil
	}
	return fmt.Errorf("%w: %q asked for a %v lease, and a lease outside seats and presence "+
		"may be held for at most coord.MaxDutyTTL (%v), the longest duty's; shorten the TTL "+
		"its caller asks for, or, for a duty, raise coord.MaxDutyTTL together with it",
		ErrTTLTooLong, resource, ttl, MaxDutyTTL)
}

// ExclusiveThrough is the last instant a holder may count on a lease a claim
// or renewal answered as held, having been ASKED at asked: asked plus ttl,
// less margin.
//
// From the asking, never the answer: the store stamps expiry later, when it
// applies the call, so on the caller's own clock the lease ends no earlier —
// and the caller never compares its clock with the store's ([Lease.ExpiresAt]).
// The margin, one interval of the caller's cadence, covers the store's clock
// stepping ahead and a check up to an interval late. Past the window the
// holder cannot claim exclusivity, the unknown ADR-0005 stops at; a margin at
// or past ttl leaves no window. One rule for every caller (ADR-0008): the
// launch-node watch, the engine's kept duty, the taker lease ([ClassTaker]).
func ExclusiveThrough(asked time.Time, ttl, margin time.Duration) time.Time {
	return asked.Add(ttl - margin)
}

// KeptRenewals is how many times a lease is renewed per TTL where the work it
// guards goes on only inside the window its renewals confirm: internal/engine's
// kept duty and the taker lease ([ClassTaker]), one count for both (ADR-0008).
//
// FOUR, SO THE WORK SURVIVES ONE MISSED RENEWAL. Work stops at
// [ExclusiveThrough] of the last confirmed renewal less one interval, so a
// confirmation buys three of the four intervals: the next renewal may go
// unanswered for a whole interval and the one after is still asked inside the
// window (at the 45-second seat TTL: 11.25 s intervals, a 33.75 s window). At
// three a TTL (seat.HeartbeatRatio) the renewal after one miss lands exactly
// on the window's end, so one store blip fences every keep. More buys a
// longer outage with more writes; four leave a quarter as clock margin.
const KeptRenewals = 4

// Lease is a held lease. Epoch is the fencing token: thread it into writes.
type Lease struct {
	Resource string
	Owner    string
	Epoch    int64

	// ExpiresAt is the UTC deadline as the STORE understands it, on the
	// store's clock and never the caller's: a node never holds it against its
	// own clock, to decide ownership or to say how long a lease has left. A
	// holder renews and counts on its lease from the TTL it asked for
	// ([ExclusiveThrough]); a reader takes a lease a read handed out as live
	// ([Holder]) and reads its time left on the store's clock ([TimeLeft]).
	ExpiresAt time.Time

	// AcquiredAt is when THIS TENURE began, on the store's clock: stamped
	// when the epoch is minted and carried through every renewal and live
	// re-claim, so it moves exactly when Epoch does — "node-2 since 08:02"
	// reads a tenure, not the last heartbeat. Every read path carries it
	// (the contract suite asserts so). Nothing decides ownership by it;
	// Epoch alone fences.
	AcquiredAt time.Time

	// Preferred is a stickiness hint naming the node that last held this
	// resource. It ORDERS claims and never gates them — the hint outlives
	// the node that set it, so gating on it would strand a dead node's
	// seats forever.
	Preferred string

	Protocol int

	// Meta is what the holder IS, beyond that it holds this: node presence
	// carries its roles and labels so a peer can judge seat eligibility
	// with no membership service. A key the holder did not write reads as
	// absent, and each reader states what absent means (placement reads
	// absent roles as every role).
	//
	// A CALLER MAY NOT DEPEND ON A META VALUE'S GO TYPE: both stores hand
	// back what a JSON round trip leaves, not what was written (an int
	// reads back as float64, a []string as []any), the twin putting every
	// meta through one at the door (coord/memory's wireCopy). Read it as
	// placement.rolesFromMeta does: accept either shape, and treat anything
	// else as absent.
	Meta map[string]any
}

// EffectiveProtocol resolves the protocol a claim is made at, normalising
// the zero value to this build. Backends MUST call this rather than reading
// the field, so the safe-zero rule has one implementation.
func (o AcquireOptions) EffectiveProtocol() int {
	if o.Protocol <= 0 {
		return ProtocolVersion
	}
	return o.Protocol
}

// AcquireOptions carries the non-identity inputs to a claim.
//
// The zero value is deliberately usable and SAFE: an AcquireOptions naming
// only an owner and a TTL claims at this build's protocol, ungated only if
// the caller says so. Every field whose zero value would be dangerous says
// what its zero means.
type AcquireOptions struct {
	// Owner is who claims the resource, the name every renewal and release of
	// it must give: a node's process incarnation, or a node id and a uuid for
	// one hold of its work, and for a browser control a person's take
	// ([BrowserControlOwner.Key]).
	Owner string
	// TTL is how long the claim survives without a renew.
	TTL time.Duration
	// Preferred is the stable node id recorded as the stickiness hint.
	Preferred string
	// Protocol is the claiming build's protocol version. ZERO MEANS THIS
	// BUILD ([ProtocolVersion]), not "oldest": read as oldest, one
	// AcquireOptions{Owner, TTL} anywhere would hold a live lease below
	// every newer node's floor and stall the fleet's claims like an upgrade
	// that never finishes.
	Protocol int
	// Meta rides with the record; see Lease.Meta.
	Meta map[string]any

	// Ungated skips the lower-protocol refusal, which guards what HOLDING A
	// SEAT means ([ProtocolVersion]). A claim skips it only where it is not
	// a seat's and must go on through a rolling upgrade, saying why at its
	// call; one that may wait, as a tracker walk's does, stays gated. Node
	// presence: a newer node unable to register is missing from the roster,
	// so its peers over-take shares. Non-seat work: a fleet duty and a hold
	// on one (internal/schedule), a server's OAuth renewal lease
	// (internal/engine), the taker lease ([ClassTaker]) and a person's control
	// of a run's browser.
	//
	// It skips the check, never the stamp: on presence the stamp is what the
	// gate reads; on a duty it holds nothing back ([ProtocolGateCounts]).
	Ungated bool
}

// Backend is the lease surface: the in-memory twin, the embedded KV or any
// external store, each certified by ONE contract suite (coordtest), and every
// method follows the package doc's tri-state.
//
// A BACKEND SERVES ITS OWN WRITES, PER RESOURCE: a claim, renew or release
// that returned is visible to this caller's next read of that resource, or a
// claim excludes nobody (package doc, "A definite answer is a CURRENT one";
// coordtest.RunShared checks it across members). A class listing may LAG but
// never DROP a key live throughout it (coordtest's
// "…_throughout_a_listing_is_in_every_listing" cases); one that cannot
// establish that answers UNKNOWN, never a shorter list (internal/coord/kv's
// walk.go). No answer rests on a removal marker, which lets [Markers] sweep.
type Backend interface {
	// TryAcquire claims resource for the owner. Exactly one of the three is
	// set: the lease, a [Refusal] (definitively not granted, and why), or an
	// error (UNKNOWN).
	//
	// It succeeds when the resource is unclaimed, lapsed, or already this
	// owner's — then it doubles as a renew and KEEPS the epoch; an ownership
	// change or a same-owner re-claim after expiry bumps it. It refuses
	// [RefusedHeld] while another owner holds it, whatever the gate would
	// say, else [RefusedProtocol] while a lease [ProtocolGateCounts] counts
	// is held at a lower protocol, unless Ungated. A lease outside
	// [HeldToSeatTTL] is honoured up to [MaxDutyTTL] and refused beyond it
	// with [ErrTTLTooLong]; a seat or presence lease, past the seat TTL.
	TryAcquire(ctx context.Context, resource string, opts AcquireOptions) (*Lease, Refusal, error)

	// Renew extends a lease the caller already holds at this epoch.
	// Reports false when the lease is definitively no longer theirs. Its
	// TTL is bounded exactly as TryAcquire's is.
	Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error)

	// Release gives up a lease the caller holds.
	//
	// It must EXPIRE THE RECORD IN PLACE, never delete it: a deleted
	// record restarts the epoch counter, which is the one token a zombie
	// from the released tenure is still fencing with.
	Release(ctx context.Context, resource, owner string, epoch int64) (bool, error)

	// Get reads a resource's current lease, or nil when unheld: a lapsed or
	// released record reads as nil. A lease it hands out is LIVE, judged on
	// the store's clock as it read the record, and the reader takes the
	// store's word ([Holder]): it never judges ExpiresAt on its own clock,
	// which may run ahead of the store's by more than any lease's TTL (the
	// package doc, "A lease a read hands out is the store's word").
	Get(ctx context.Context, resource string) (*Lease, error)

	// ListOwned returns the live leases this owner holds, each live by the
	// store's word as Get's is.
	ListOwned(ctx context.Context, owner string) ([]Lease, error)

	// ListLive returns the live leases of one resource class, each live by
	// the store's word as Get's is: a reader judges none of them again on
	// its own clock. The membership read is built on this: ListLive(ClassNode)
	// is the fleet's roster.
	ListLive(ctx context.Context, class Class) ([]Lease, error)

	// Now reads the STORE's clock for class: the clock that class's
	// ExpiresAt is on, and so the one a reader holds a deadline against to
	// say how long a lease has left ([TimeLeft]), never its own. Two classes
	// need not share one (the package doc, "A lease a read hands out is the
	// store's word").
	Now(ctx context.Context, class Class) (time.Time, error)

	// WatchLive streams every write to a lease of class made after it
	// returns (claim, renewal, takeover, release) as a [LeaseWrite], in
	// store order, until ctx ends or the watch fails and the channel
	// closes; a closed channel says nothing about any lease.
	//
	// A HINT, NEVER AN ANSWER: a lapse is no write, nothing before the
	// watch is told, and a delivered write may be superseded, so ListLive
	// still says what the roster IS; the watch buys LATENCY ([LeaseView]).
	// A claim's state before it has a fencing token (the KV's claiming
	// record) is not delivered. Starting is bounded by ctx or the store's
	// request budget and fails as UNKNOWN, never a silent channel; the
	// consumer drains it, as a backend may hold later writes behind it.
	WatchLive(ctx context.Context, class Class) (<-chan LeaseWrite, error)

	// PreferredResources returns resources of this class whose stickiness
	// hint names this node, INCLUDING lapsed ones: the hint's whole
	// purpose is to bring a restarted node's own seats back to it.
	PreferredResources(ctx context.Context, class Class, nodeID string) (map[string]struct{}, error)

	// EverPresent returns every node id that has held presence ([ClassNode])
	// in this store, LAPSED AND RELEASED ONES INCLUDED, sorted: the fleet's
	// one roll of every node that ever ran, on every build, read off the
	// fencing counter's records, which no lease's end removes.
	EverPresent(ctx context.Context) ([]string, error)

	// FleetProtocolFloor returns the lowest protocol among the live leases
	// the gate counts ([ProtocolGateCounts]), and whether there were any. A
	// claim refused [RefusedProtocol] names this floor, so it counts EXACTLY
	// what the gate counts, or an operator goes looking for an older node
	// that is not there.
	FleetProtocolFloor(ctx context.Context) (int, bool, error)
}

// LeaseWrite is one write to a lease as a watch delivers it
// ([Backend.WatchLive]): who holds the resource once the write has landed,
// and under which fencing token.
type LeaseWrite struct {
	// Resource is the lease's resource name.
	Resource string

	// Owner holds the resource after the write, and is EMPTY where the
	// write left nobody holding it — a release, or a record the store
	// removed.
	Owner string

	// Epoch is the holder's fencing token, which a renewal leaves as it
	// was. With no Owner it is the ended tenure's or zero, and a reader
	// takes nothing from it.
	Epoch int64
}

// Refusal is why a claim was definitively not granted. The zero value is "not
// refused": a granted claim, or an unknown one.
//
// The backend knows which rule refused at no cost, while telling "a peer holds
// it" from "the gate stopped me" through [Backend.FleetProtocolFloor] costs a
// standing view of every live presence and seat lease write, on every sweep.
type Refusal string

const (
	// RefusedHeld is the refusal of a claim on a resource another owner
	// holds a live lease on. It takes precedence over the gate, because a
	// claim that cannot write has nothing for a gate to stop — and because
	// it is what lets a claim on a held resource judge no gate at all.
	RefusedHeld Refusal = "held"
	// RefusedProtocol is the refusal of a claim while a live presence or
	// seat lease ([ProtocolGateCounts]) is held at a lower protocol than the
	// claim's (ADR-0016). Every gated claim this node makes is refused the
	// same way until that lease goes, which is what a caller reports.
	RefusedProtocol Refusal = "protocol"
)

// Refusals are the two.
var Refusals = []Refusal{RefusedHeld, RefusedProtocol}

// Valid reports whether a refusal is one this build knows.
func (r Refusal) Valid() bool { return slices.Contains(Refusals, r) }

// --- resource naming ------------------------------------------------------

// ResourceSeparator joins the segments of a resource name. [DocumentKey] makes
// each segment a subject token, so the CLASS is a token of its own and a whole
// class is a broker wildcard every class listing filters on.
const ResourceSeparator = ":"

// Class is the leading segment of a resource name: what kind of thing the
// lease is for. A named type because a class that is not a single subject
// token silently selects NOTHING, and an empty listing looks like a class with
// no members; [Class.Valid] refuses one.
//
// Not enumerated, or every caller's vocabulary would land here: this package
// owns the five the fleet leases, the tracker leases under classes of its own,
// and only [Class.HeldToSeatTTL] says which classes the seat lease TTL holds.
type Class string

// The classes the fleet leases directly.
const (
	ClassSeat   Class = "seat"
	ClassWorker Class = "worker"
	ClassNode   Class = "node"

	// ClassTaker is an engine process's TAKER lease: the process still keeps
	// the executor sessions it began taking (internal/sandbox's
	// SessionCapture.Taker), so a capture it left begun is waited for while
	// the lease holds and settled from the executor log once it lapses. The
	// whole company must agree on it now (ADR-0003): any node may wait on a
	// capture another process began.
	//
	// One per process incarnation ([TakerResource]), so a restart never
	// waits out its predecessor. Not presence, which a drain gives up while
	// keeps still upload. Held to [MaxDutyTTL] like any caller's class,
	// renewed [KeptRenewals] times a TTL, and ungated and uncounted by the
	// gate ([ProtocolGateCounts]): it says nothing about holding a seat.
	ClassTaker Class = "taker"

	// ClassBrowserControl is a person's control of one executor launch's
	// browser ([BrowserControls], [BrowserControlResource]): taken and renewed
	// by the node serving the person, ended by it or by the node holding the
	// seat. Its tokens come from one counter for the class ([CounterResource]),
	// since a launch is a resource nobody leases again once its job ends;
	// ungated and uncounted by the gate. Coord's own class rather than a
	// caller's, because [CounterResource] must know its counter and its record
	// is a typed contract both stores implement.
	ClassBrowserControl Class = "browser_control"
)

// TakerResource names the taker lease of one process incarnation
// ([ClassTaker]): `taker:{node}:{suffix}`, the incarnation whole.
func TakerResource(incarnation string) string { return ClassTaker.Resource(incarnation) }

// CounterResource is the resource whose fencing counter mints resource's
// tokens; every backend asks this one function so the twin and the store
// agree.
//
// The resource itself, except a taker lease or a browser control, which counts
// on its class's one counter keyed by the class name: a counter is never
// removed (a removed one restarts at 1), so one per process start or per
// launch would grow forever. A class counter keeps the guarantee — every
// token above every earlier one, so a re-claim is fenced against its past
// self — at the cost of contention its compare-and-set absorbs. It keeps no
// placement hint.
func CounterResource(resource string) string {
	for _, class := range classCounted {
		if class.Holds(resource) {
			return string(class)
		}
	}
	return resource
}

// classCounted is every class whose tokens one counter of the class mints.
var classCounted = []Class{ClassTaker, ClassBrowserControl}

// Valid reports whether this class can address a key: SHAPE, not membership.
// It must be non-empty and survive the key grammar's escaping UNCHANGED, or
// its filter (built from the class as written) stops matching its keys (built
// escaped) and the listing goes quietly empty. That one test also refuses the
// separator, which always escapes.
func (c Class) Valid() bool {
	return c != "" && DocumentKey(string(c)) == string(c)
}

// HeldToSeatTTL reports whether this class's leases are held to the seat
// lease TTL: seats and presence, and nothing else. See [HeldToSeatTTL] for
// the resource form and the reason; this is the one place the set is spelled.
func (c Class) HeldToSeatTTL() bool {
	return c == ClassSeat || c == ClassNode
}

// Resource names the lease for one member of this class.
//
// One part, not variadic: a variadic also spells `Resource()` with no parts,
// a key nothing can decode, whose lease no listing returns and every node
// reads as a free seat.
func (c Class) Resource(part string) string {
	return string(c) + ResourceSeparator + part
}

// Prefix is what every resource in this class starts with.
func (c Class) Prefix() string { return string(c) + ResourceSeparator }

// Holds reports whether a resource belongs to this class.
func (c Class) Holds(resource string) bool {
	return strings.HasPrefix(resource, c.Prefix())
}

// Name recovers everything after the class, reporting false for a resource of
// another class.
//
// The whole remainder, unsplit: a caller that wants one part of a multi-part
// name knows how many there are, and a helper that guessed would turn a
// handle containing a colon into a name nobody wrote.
func (c Class) Name(resource string) (string, bool) {
	if !c.Holds(resource) {
		return "", false
	}
	return strings.TrimPrefix(resource, c.Prefix()), true
}

// CheckResource refuses a resource name that cannot address a key: an empty
// segment is a value lost on the way here, whose key [DocumentSegments]
// refuses, and one past [MaxLeaseNameBytes]. Checked at the surface, not the
// encoder, because a lease under an undecodable key is one no listing returns,
// so its seat looks free to every node and is handed out twice.
func CheckResource(resource string) error {
	if resource == "" {
		return fmt.Errorf("coord: a resource name is required")
	}
	if err := HoldLeaseName("resource", resource); err != nil {
		return err
	}
	for i, seg := range ResourceSegments(resource) {
		if seg == "" {
			return fmt.Errorf("coord: resource %q has an empty segment at position %d: "+
				"every %q-separated part names something, so an empty one is a "+
				"value lost on the way here", resource, i, ResourceSeparator)
		}
	}
	return nil
}

// ResourceSegments splits a resource into the segments a key is built from.
//
// The inverse of [Class.Resource]: the leading segment is the class and the
// rest is its name, however many parts that took.
func ResourceSegments(resource string) []string {
	return strings.Split(resource, ResourceSeparator)
}

// SeatResource names the lease for an agent seat.
//
// BY THE SEAT'S ID: the agent id derived from its handle (ADR-0013), which is
// what its mailbox and its consumer group are named by too.
func SeatResource(seat uuid.UUID) string { return ClassSeat.Resource(seat.String()) }

// WorkerResource names the lease for a per-company singleton duty.
func WorkerResource(duty string) string { return ClassWorker.Resource(duty) }

// NodeResource names a node's own presence lease, renewed on its seats'
// heartbeat. Counting the live ones is how a node learns the fleet size to
// divide seats by: there is no membership service, and counting SEAT owners
// reads a fresh fleet as zero nodes, so every node would take every seat.
func NodeResource(nodeID string) string { return ClassNode.Resource(nodeID) }

// IsSeatResource reports whether a resource names a seat.
func IsSeatResource(resource string) bool { return ClassSeat.Holds(resource) }

// IsNodeResource reports whether a resource names a node's presence.
func IsNodeResource(resource string) bool { return ClassNode.Holds(resource) }

// SeatID recovers the seat id from a seat resource name.
//
// It reports false for a resource of another class AND for one whose name is
// not a canonical uuid, so a key written by hand is answered as "not a seat
// lease" rather than as a seat with an unreadable name.
func SeatID(resource string) (uuid.UUID, bool) {
	name, ok := ClassSeat.Name(resource)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(name)
	if err != nil || id == uuid.Nil || id.String() != name {
		return uuid.Nil, false
	}
	return id, true
}

// NodeID recovers the node id from a presence resource name.
func NodeID(resource string) (string, bool) { return ClassNode.Name(resource) }
