package statelog

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Outcome is what a write's caller is told: exactly three answers for three
// facts, [internal/coord]'s three-valued discipline one layer down. "Held",
// "definitively not held" and "the store could not be reached" must not
// collapse into a bool; here they are a PubAck, a wrong-last-sequence refusal,
// and no answer at all.
//
// No boolean says whether the apply wait SUCCEEDED: durability is the position
// being set and success is the enum, so it could only disagree. [Result.Waited]
// differs: "a wait was attempted" is not derivable once [Request.NoWait]
// exists, as its two pendings carry one outcome and mean opposite things.
type Outcome string

const (
	// OutcomeApplied says both halves at once: the record is durable at
	// its position AND this node has applied it, so the rows the caller
	// is about to read are the rows the record produced.
	//
	// Not PERMANENT, and the difference matters exactly once: a deletion
	// marker committed above this position retroactively drops it.
	OutcomeApplied Outcome = "applied"

	// OutcomePending says the record is DURABLE at its position and what
	// it produced is unresolved HERE. Every other node will apply it.
	//
	// A COPY IS JUDGED BEFORE IT IS ANSWERED PENDING. A write the broker
	// collapsed onto another node's copy of its operation is durable at that
	// copy, which applies nowhere if its writer was evicted in flight — so it
	// is pending only where that writer still stands at the copy's position,
	// and unknown otherwise ([Publisher.unresolved]).
	//
	// NEVER a failure and never "applied": the caller has a position to
	// resolve with later, which is not a retry.
	OutcomePending Outcome = "pending"

	// OutcomeUnknown says nothing can be established about this record
	// from this node. The op id travels with it, because retrying under
	// the SAME op id is the only safe retry — a fresh one would defeat
	// the ledger that exists for exactly this case.
	OutcomeUnknown Outcome = "unknown"
)

// Outcomes is every outcome, for validation and for a test that walks them.
var Outcomes = []Outcome{OutcomeApplied, OutcomePending, OutcomeUnknown}

// Valid reports whether an outcome off the wire is one this build knows.
func (o Outcome) Valid() bool {
	for _, known := range Outcomes {
		if o == known {
			return true
		}
	}
	return false
}

// Result is what a write answers with.
type Result struct {
	// Outcome is the three-valued answer.
	Outcome Outcome

	// Position is where the record landed. Set for applied and pending,
	// zero for unknown — which is the whole content of unknown.
	Position Position

	// OpID is the operation this write belongs to, and what a caller
	// retries under.
	OpID string

	// Waited says an apply wait was ATTEMPTED, which is what tells the
	// two pendings apart.
	//
	// NOT "the wait succeeded" (that is `applied`): it says whether anybody
	// asked. A pending that was waited for says this NODE is behind; one
	// that was not says the caller declined to find out ([Request.NoWait]).
	// Reported as one, a deliberate skip reads as a node falling behind.
	//
	// FALSE ON EVERY OUTCOME THAT DID NOT REACH THE WAIT — a refusal, a
	// round that lost its race: no wait happened there either.
	Waited bool

	// Version is the object's new version on an applied write: the
	// position packed, which is the same number the row carries.
	Version int64

	// Rounds is how many compare-and-set rounds this write took, for the
	// instrument that says whether an object is contended.
	Rounds int

	// Collapsed says this call was a RETRY of an operation that had
	// already landed under the same op id, and THIS call's decision was
	// never stored: Position is the earlier copy's, found in the ledger
	// ([Snap.Held]), the broker's duplicate acknowledgement, or the ledger
	// when the publish is resolved. An ambiguous publish answered from the
	// ledger is collapsed even when the record is this call's: nothing tells
	// the two apart. A caller whose ANSWER is computed inside its decision
	// must not build on it: it describes a decision nothing published (a
	// counter a create minted from: the earlier copy's number is on the log,
	// this call's is not). The outcome is still the operation's own.
	Collapsed bool

	// Unvouched says an `unknown` was answered because THIS NODE'S
	// operation ledger cannot vouch for the operation — it was minted
	// before the instant the ledger may have lost rows from (a retention
	// sweep, this node's or a donor's inherited on an adoption) and holds
	// no row for it — rather than because an acknowledgement was lost.
	// Always false beside any other outcome. The two send a retry opposite
	// ways: a lost acknowledgement is resolved by the same id retried HERE; an
	// unvouched operation meets the same silence here every time (the row it
	// needs is the one the loss took) and the first run may have landed, so
	// only another node whose ledger lost nothing that far back can answer.
	// Told only "unknown", a walk said "call again" for ever and a create said
	// "not made" of a task it may have filed.
	Unvouched bool
}

// Wrote reports whether this result speaks for a record a caller has to
// account for: one that was appended (a position is set) or may have been
// (`unknown`, whose position is zero by definition). A write that decided it
// had nothing to append answers neither, and folding its outcome into a
// call's answer would claim a record that does not exist.
func (r Result) Wrote() bool {
	return !r.Position.IsZero() || r.Outcome == OutcomeUnknown
}

// LessCertain is the weaker of two write outcomes: `unknown` over `pending`
// over `applied`, and an empty outcome — nothing appended — under all three,
// since it claims nothing a caller must wait for.
//
// # Why a call that appended several records needs it
//
// A tool call, a dependency sequence, a save then a rename: each is ONE answer
// over several records, read as "may I stop looking at this write?". It has to
// hold for EVERY record the call made — `applied` over a record the broker
// never confirmed breaks the rule every transport documents, that an unknown
// write is never reported applied. The first or last record's outcome breaks
// it the moment the records disagree.
func LessCertain(a, b Outcome) Outcome {
	rank := func(o Outcome) int {
		switch o {
		case OutcomeUnknown:
			return 3
		case OutcomePending:
			return 2
		case OutcomeApplied:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// Later is the later of two positions on one stream, the zero position losing
// to any other. It is the position a call that appended several records must
// answer, because a caller barriers on it before its next read: the earlier
// one is a floor BELOW a record the call made.
//
// # Why it asks [Position.Before] rather than comparing packed values
//
// A packed value is a coordinate in ONE log's number space, and two logs'
// spaces are unrelated: compared bare, one log's position would "follow"
// another's for no reason but the sizes of two counters. [Position.Before]
// refuses that comparison, so a position on ANOTHER stream never replaces the
// first: a second log's position is no floor on the log the caller barriers on.
func Later(a, b Position) Position {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	}
	if before, err := a.Before(b); err == nil && before {
		return b
	}
	return a
}

// Reason says why a write was refused, and each value names a different thing
// for the caller to do.
type Reason string

const (
	// ReasonEvicted — the record's writer has been removed from the fleet:
	// this node, or — when [Unavailable.CopyWriter] names one — the node whose
	// copy this node's append collapsed onto. Nothing an evicted node
	// publishes is applied anywhere.
	//
	// THIS NODE'S OWN: no retry HERE; another node takes the write. A refusal
	// naming no position preceded any append, so that node reuses the id. One
	// naming a position is a landed record that applies nowhere and holds the
	// id for the duplicate window ([StreamSpec.Duplicates]): use a fresh id,
	// or this one after the window. ANOTHER NODE'S COPY says nothing about
	// this node, which passed its own fences: it takes the write itself, under
	// a fresh id, or the same one once the window has passed.
	ReasonEvicted Reason = "evicted"

	// ReasonEvictionUnknown — this node's own eviction state could not be
	// read, which is the third value and it BLOCKS: publishing under it
	// could produce durable records every node drops. It clears when the
	// state is read again.
	//
	// ITS OWN REASON RATHER THAN [ReasonEvicted], because the two send a
	// caller opposite ways: an eviction is permanent until an operator
	// readmits the node, while a coordination blip is gone in seconds.
	// Folded into one, every write on the node during a blip was a 503
	// saying "waiting cannot clear this" — no Retry-After — which is the
	// signal a client stops asking on.
	ReasonEvictionUnknown Reason = "eviction_unknown"

	// ReasonDeferred — this node holds a record it cannot decode whose
	// scope covers this object, so its rows are stale and any decision
	// taken from them is unsafe. Another node can serve this write.
	ReasonDeferred Reason = "deferred"

	// ReasonBehind — a snapshot here would decide from a state below a
	// position the write needs: the caller's own previous write, a peer's
	// write the broker has already accepted, or — at an expectation of
	// zero ([ZeroFence]) — the published trim floor, while the log still
	// holds every record this node lacks and it is replaying them, the
	// state [FloorReplaying] names on the read path. Position is the
	// position the write needs this node to have applied through. It
	// clears on its own.
	ReasonBehind Reason = "behind"

	// ReasonBelowFloor — the record this node's applier needs next is gone
	// from the log, so a retry-at-zero here could overwrite a peer's
	// committed write and no replay can supply what the node lacks. It
	// clears when the node adopts a peer's snapshot; another node can make
	// the write meanwhile. Produced by [ZeroFence], on the one branch where
	// being below the floor is a lost update rather than a stale read — and
	// only when the node is below the LOG: below the published floor alone
	// it is replaying, and the refusal is [ReasonBehind].
	ReasonBelowFloor Reason = "below_floor"

	// ReasonFloorUnknown — the bound an expectation of zero is cleared
	// against could not be read: the published trim floor, or the log's
	// own ends, which carry its first surviving sequence. Either is half
	// of that bound, and an unknown half is the third value, and it
	// BLOCKS. Failing open here is a lost update, which is not
	// recoverable; failing open where a claim is deduplicated is a
	// duplicate delivery, which is.
	ReasonFloorUnknown Reason = "floor_unknown"

	// ReasonUsageUnknown says the log's usage could not be read before an
	// ordinary append, so the reserve above its ordinary ceiling could not
	// be checked ([Reserve.Admit]). The third value, and it BLOCKS:
	// admitting blind could spend the room the gate records land in.
	// Nothing was appended, and it clears when the usage reads again, as
	// it does once a broker election ends (a meta group with no leader
	// answers JetStream's 503, err_code 10008).
	ReasonUsageUnknown Reason = "usage_unknown"

	// ReasonDeleted — this object carries a permanent deletion marker.
	// It stays deleted; a guarding row's absence below the trim floor is
	// not permission to recreate it. A fact about the OBJECT, the same for
	// every writer ([Reason.BlamesWriter] is false), so a refusal of another
	// node's copy under it names no [Unavailable.CopyWriter]: this node's
	// own record would be dropped the same way.
	ReasonDeleted Reason = "deleted"

	// ReasonAbandoned — the record was written in a generation a reanchor
	// ABANDONED: one only a node the fleet has since evicted held, whose
	// history is on no disk the fleet still has. It was decided from rows
	// nobody holds, so it produces rows nowhere the reanchor's checkpoint is
	// followed from — see [ReanchorPlan.From].
	ReasonAbandoned Reason = "abandoned"

	// ReasonOvertaken — the record was written after a RESTORED reanchor's
	// generation record, in a generation below the one that reanchor opened:
	// by a node the move had overtaken before it learned of it, from the rows
	// the reanchor did not keep — the copy's age. It produces rows nowhere the
	// reanchor's checkpoint is followed from — see [ReanchorPlan.StaleAfter].
	ReasonOvertaken Reason = "overtaken"

	// ReasonLogFull — the log is at its byte ceiling and refuses
	// appends rather than dropping records. An operator raises the
	// ceiling or unblocks the trim. On a log that keeps a gate reserve
	// an ordinary write meets this at the ceiling less the reserve
	// ([Reservation.Ordinary]), and a gate record only at the broker's own.
	ReasonLogFull Reason = "log_full"

	// ReasonRecordTooLarge — the record is larger than the broker takes in
	// one message, and nothing about the log's size is involved. No node
	// can place it and no wait changes it. The detail names which limit
	// refused it, because each has its own remedy: the server's max_payload
	// (raised on every server of an external cluster; a node refuses at boot
	// a connection held to less than the transport's contract), the stream's
	// max_msg_size (reconfigured; restore it), or the file store's per-record
	// limit (nothing raises it). Splitting the change answers all three.
	//
	// ITS OWN REASON RATHER THAN [ReasonLogFull], whose remedy (raise a
	// ceiling, unblock the trim) is wrong for a log with room to spare.
	ReasonRecordTooLarge Reason = "record_too_large"

	// ReasonBrokerRefused — the broker refused to store the record, named
	// a reason, and it is none this framework has a remedy for: a sealed
	// stream, a JetStream store with no resources left, a limit nobody
	// declared on a state log. The detail carries the broker's own code
	// and words, which are the remedy. Nothing about waiting changes it.
	ReasonBrokerRefused Reason = "broker_refused"

	// ReasonSkew — the broker answered a last sequence BELOW an
	// expectation this node formed, which cannot happen on a healthy
	// stream and means a store or stream was restored out of step.
	ReasonSkew Reason = "skew"

	// ReasonOpReused — the write's operation id already names a record on
	// another object in this node's ledger: an operation id names one
	// write, and the caller sent it with a different one. Nothing was
	// written for this write, and a fresh id is what it needs.
	ReasonOpReused Reason = "op_reused"

	// ReasonLogTruncated — the log lost records a peer's rows hold
	// ([ErrLogTruncated]): a broker restored from an older copy, and a peer
	// whose rows are newer than the copy. This node's own rows are the log's
	// history, so only its WRITES refuse, and they do until the operator
	// settles which history the fleet keeps — a write made before that is one
	// the restored reanchor of the peer would apply nowhere.
	ReasonLogTruncated Reason = "log_truncated"

	// ReasonWrongStream — the log under this domain's name is not the
	// one this node's rows were derived from, by one of: deletion and
	// rebuild ([ErrStreamRecreated]); a checkpoint past the log's end
	// ([ErrAheadOfLog], a restore from an older copy, which keeps the creation
	// instant so only the end shows it); another record at the checkpoint than
	// the one consumed ([ErrLogDiverged], the same restore written past its
	// rows); a peer's re-anchor past this node's generation
	// ([ErrGenerationPassed]). Every expectation this node could form is from
	// a history the broker does not hold, so what it appends its applier never
	// applies. Waiting does not clear it: an operator re-anchors, or for a
	// passed generation as the refusal names (adoption, re-anchor, or re-run
	// of its own reanchor). Same word as the read refusal.
	ReasonWrongStream Reason = "wrong_stream"

	// ReasonSuperseded — this operation's record landed, and a later record
	// on the same object has undone or replaced it since, so a retry of the
	// operation is not a new write of it and cannot be answered as though
	// its record were still the one in force. A caller that still wants the
	// effect starts a NEW operation, under a fresh id. Produced by a write's
	// [Request.Standing] — the node gate's ([GateStanding]): an eviction
	// retried under its id after a readmission has taken the node back.
	ReasonSuperseded Reason = "superseded"
)

// Reasons is every [Reason] this build names, in declaration order.
//
// THE ENUMERATION IS WHAT A SURFACE IS HELD TO. A refusal's reason is a label
// on the refusal counter and a field in every write route's answer, and each
// value has its own remedy — so a reference that lists some of them reads as
// complete and is not, and the operator writing a collector rule from it has
// no row for the value that fires. The metrics catalogue's refusal instrument
// is checked against this list ([TestEveryRefusalReasonIsInTheMetricsReference]).
//
// A RECORD A GATE DROPPED is refused under the gate that dropped it — evicted,
// deleted, abandoned or overtaken — rather than under a generic word, because
// the gate is what says why: which is why there is no `gated` here.
func Reasons() []Reason {
	return []Reason{
		ReasonEvicted, ReasonEvictionUnknown, ReasonDeferred, ReasonBehind,
		ReasonBelowFloor, ReasonFloorUnknown, ReasonUsageUnknown, ReasonDeleted,
		ReasonAbandoned, ReasonOvertaken, ReasonLogFull, ReasonRecordTooLarge,
		ReasonBrokerRefused, ReasonSkew, ReasonOpReused, ReasonLogTruncated,
		ReasonWrongStream, ReasonSuperseded,
	}
}

// Valid reports whether r is a reason this build names — an unknown one off
// the wire is a value to show rather than one to switch on.
func (r Reason) Valid() bool { return slices.Contains(Reasons(), r) }

// Retryable reports whether this refusal clears on THIS node without anybody
// doing anything — the write-side twin of [ReadRefusal.Retryable].
//
// FIVE CLEAR ON THEIR OWN: a node BEHIND the caller's previous write applies
// it, a node BELOW the trim floor catches up or adopts a snapshot, and an
// unreadable floor, eviction state or log usage is read again.
// [ReasonDeferred] is not one: ANOTHER node can serve the write, this one not
// until upgraded. Nor is a refusal whose remedy is a NEW operation or an
// operator's gesture. A reason spelled like a [ReadRefusal] AGREES WITH ITS
// TWIN ([TestAWriteRefusalAndItsReadTwinAgreeOnWaiting]): both describe one
// state of one node.
func (r Reason) Retryable() bool {
	switch r {
	case ReasonBehind, ReasonBelowFloor, ReasonFloorUnknown, ReasonEvictionUnknown,
		ReasonUsageUnknown:
		return true
	}
	return false
}

// BlamesWriter reports whether a gate answering r dropped a record for what its
// WRITER was or did — evicted, or stamped with a generation a reanchor voided
// (abandoned, overtaken) — rather than for something every writer's record meets
// alike: the object's deletion marker (deleted). False for a non-gate reason.
// It decides whose standing a refusal of ANOTHER node's copy states
// ([Unavailable.CopyWriter]). Where it blames the writer, the refusing node
// passed its own fences and finishes the write itself once the duplicate
// window lets go. Where not, its own record meets the same gate, and naming
// the copy's writer would read as "retry here", never true. An unnamed reason
// answers false, so [TestOnlyAGateThatHoldsAWriterBlamesIt] holds an explicit
// answer for every value [Reasons] names.
func (r Reason) BlamesWriter() bool {
	switch r {
	case ReasonEvicted, ReasonAbandoned, ReasonOvertaken:
		return true
	}
	return false
}

// FromGate reports whether r is a GATE's: a rule under which a record the
// broker ACCEPTED applies on no node — evicted, deleted, abandoned or
// overtaken, the reasons a `statelog_record_gated` line and the records-gated
// counter carry. Every other reason refuses a write before or instead of an
// append. evicted and deleted are ALSO refused before an append (this node's
// own eviction, an object's marker read in the snapshot), and after one where
// a lost answer found another operation's record, at which the gate holds this
// node — so a refusal says whether a record of its own landed by
// [Unavailable.Landed], never by its reason alone.
//
// Declared per reason beside [Reason.BlamesWriter], whose set it contains, and
// held by the same table ([TestOnlyAGateThatHoldsAWriterBlamesIt]).
func (r Reason) FromGate() bool {
	switch r {
	case ReasonEvicted, ReasonDeleted, ReasonAbandoned, ReasonOvertaken:
		return true
	}
	return false
}

// RefusesTheWrite reports whether r refuses a write for WHAT THE WRITE IS OR
// NAMES, which every node and every attempt meets alike and only a different
// write gets past: record_too_large, deleted, op_reused or superseded. False
// for every refusal of the node, the log or the broker the write was sent
// through, which another node, a later attempt or an operator gets past, and
// for a reason this build does not name. It decides whose failure a refusal is
// (the work's, or the node's), declared per reason beside [Reason.BlamesWriter]
// and held for every reason by TestOnlyARefusalOfTheWriteItselfIsTheWorks (the
// package doc's "Whose failure a refusal is").
func (r Reason) RefusesTheWrite() bool {
	switch r {
	case ReasonRecordTooLarge, ReasonDeleted, ReasonOpReused, ReasonSuperseded:
		return true
	}
	return false
}

// RetryAfter is how long a caller should wait before bringing the request err
// refused back to THIS node: ZERO when waiting cannot change the answer, the
// refusal's own derived hint where it has one, and otherwise `otherwise` (it
// is retryable and derived nothing, or not a refusal this package made).
// ONE RULE FOR EVERY SURFACE that answers in a status code. Written at each it
// drifted: a refusal waiting cannot clear (an undecodable record, an eviction)
// became `Retry-After: 2` and a client polled a node that would not answer
// until upgraded or readmitted. Zero is the only honest hint there
// ([Refused.RetryAfter] for a read, [Reason.Retryable] for a write); a write's
// one hint of its own is another node's copy under a gate that blames its
// writer ([Unavailable.RetryAfter]), which this node takes after the window.
func RetryAfter(err error, otherwise time.Duration) time.Duration {
	var refused *Refused
	if errors.As(err, &refused) {
		switch {
		case !refused.Code.Retryable():
			return 0
		case refused.RetryAfter > 0:
			return refused.RetryAfter
		}
		return otherwise
	}
	var unavailable *Unavailable
	if errors.As(err, &unavailable) {
		switch {
		case unavailable.RetryAfter > 0:
			return unavailable.RetryAfter
		case !unavailable.Reason.Retryable():
			return 0
		}
	}
	return otherwise
}

// ErrUnavailable is what a refusal wraps, so a caller can tell a refusal from
// a conflict with errors.Is before it looks at the reason.
var ErrUnavailable = errors.New("statelog: unavailable")

// ErrConflict reports a write that lost its race for the whole round budget.
// It is the refusal a model reads as "somebody else is editing this", and it
// is the same refusal the tracker's own writes give today.
//
// NOTHING BUT A LOST RACE MAY RETURN IT. A fence that refuses this node — an
// eviction, a floor it is below, a log it is past the end of — returns an
// [*Unavailable] instead, because the two send a caller opposite ways: a
// conflict says re-read and try again here, and every fence refusal says this
// node cannot make this write however often it re-reads. Both fences once
// refused an evicted node with this, so a model was told a colleague was
// editing an object on a node the fleet had removed.
var ErrConflict = errors.New("statelog: conflict")

// Unavailable is a refusal naming what it is about.
//
// A REASON AND A DETAIL rather than a message, for two readers: a surface
// switches on the reason to retry elsewhere, and whoever runs the node reads
// the detail to find what to change.
//
// THE DETAIL IS WRITTEN FOR WHOEVER RUNS THE NODE: composed by this package,
// naming positions, versions, settings and the broker's own words, never an
// error's text (a store's error is a driver's message or a database path). It
// goes to the node's log and the operator's gate and retention tooling; a
// person gets [ConditionSentence]'s words instead. [Refused.Detail] is held
// to the same rule.
type Unavailable struct {
	Reason   Reason
	Detail   string
	Position Position
	OpID     string

	// RetryAfter is how long to wait before bringing the same operation
	// back, set only where waiting clears a refusal its reason alone says it
	// does not ([Reason.Retryable]): another node's copy, named by
	// CopyWriter, which the log's duplicate window collapses the operation
	// id onto until it lets go of it — the window's length, the longest
	// that can take from when the copy landed. Zero everywhere else.
	RetryAfter time.Duration

	// CopyWriter names ANOTHER node when the record at Position is that node's
	// copy of this operation, AND the gate that dropped it blames the copy's
	// writer ([Reason.BlamesWriter]). The broker collapsed this node's append
	// onto the copy inside the duplicate window, or a write whose answer was
	// lost found it newest on its subject. The reason then states THE COPY'S
	// WRITER's standing, not the refusing node's, which passed its own fences
	// and takes the write itself once the window lets go. A FIELD, not Detail
	// text, because the remedy turns on it: read as this node's own `evicted`,
	// an operator was sent elsewhere. EMPTY WHEN THE REFUSAL IS ABOUT THE NODE
	// THAT MADE IT, and for another node's copy under a gate blaming no writer
	// (`deleted`: the marker holds every writer's record for ever). Detail
	// still says whose copy the record is.
	CopyWriter string

	// Dropped says the record at Position is THIS OPERATION'S and a gate
	// dropped it: this write's own append, or a copy the broker collapsed it
	// onto, or one a write whose answer was lost found newest carrying its
	// id. So the record is durable there and applies nowhere
	// ([Unavailable.Landed]). FALSE WHEN POSITION IS ANOTHER OPERATION'S
	// RECORD: a lost answer found ANOTHER operation's record newest and the
	// gates, asked about THIS NODE there, hold it. Position is then only where
	// the gate was judged (maybe a healthy peer's record that applied
	// everywhere), and whether this write's own append landed is unknown; read
	// as "durable and applies nowhere" it sent away a write another node could
	// make. A FIELD, since a position cannot say whose record it names; set by
	// the resolution that read it ([Publisher.Publish]).
	Dropped bool

	// Cause is the error a refusal was concluded from, when there is one
	// a caller may want to recognise without switching on the reason —
	// [ErrStreamRecreated] or [ErrAheadOfLog] behind `wrong_stream`, which
	// are the same sentences the applier's stop and a read's refusal
	// carry, and the unreadable read behind `floor_unknown` or an
	// unreadable `evicted`, so a store's or a broker's own sentinel still
	// answers errors.Is through the refusal. Nil for the reasons that are
	// their own whole answer.
	Cause error
}

func (u *Unavailable) Error() string {
	if u.Detail == "" {
		return fmt.Sprintf("statelog: unavailable (%s)", u.Reason)
	}
	return fmt.Sprintf("statelog: unavailable (%s): %s", u.Reason, u.Detail)
}

// Landed reports a refusal made AFTER an append whose record is known: a gate
// ([Reason.FromGate]) dropped THIS OPERATION'S record, durable at Position —
// this write's own, or a copy of it the broker collapsed the append onto
// ([Unavailable.Dropped]) — so the operation applies nowhere, and its id is
// held for the log's duplicate window. NEVER JUDGED BY THE POSITION ALONE:
// refusals made before any append carry positions too, none a record of this
// write (the point a `behind` write needs applied, the record a `deferred`
// node cannot decode, a `skew`'s last sequence, the record an `op_reused` id
// names); read as landed, each told a caller its write was durable and lost
// when nothing was sent. A gate's reason with NO position, or WITH one that is
// ANOTHER operation's record ([Unavailable.Dropped]), is about this node.
func (u *Unavailable) Landed() bool {
	return u.Dropped && u.Reason.FromGate() && !u.Position.IsZero()
}

// Unwrap makes every refusal answer errors.Is(err, ErrUnavailable), and one
// with a cause answer for the cause too.
func (u *Unavailable) Unwrap() []error {
	if u.Cause == nil {
		return []error{ErrUnavailable}
	}
	return []error{ErrUnavailable, u.Cause}
}
