package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ErrMaintenanceMoved reports a compare-and-set that lost its race: the
// operation record changed since the read this write was formed from.
//
// A sentinel because the caller's answer is not a retry: re-read the record
// and re-decide from the phase it is actually in. A coordinator that lost its
// lease and came back holds a view a successor has moved past, and retrying
// its own write is what the compare-and-set exists to refuse.
var ErrMaintenanceMoved = errors.New("coord: the maintenance operation moved since it was read")

// MAINTENANCE MODE: a stream's byte ceiling changes only while no request to
// it can be outstanding, and closing a client retires nothing the broker has
// queued, so the fleet restarts into a mode that starts every broker and no
// publisher (package doc, "Maintenance mode").
//
// [MaintenanceOperation] IS the exclusion: its existence excludes publishers
// and its deletion releases them. Held as two records, an interrupted
// activation would read exactly like an interrupted cleanup. Not this record:
// the coordinator's lease ([MaintenanceResource]) expires and this never does;
// an [Admission] is a handshake rather than an absence check; and a
// [MaintenanceAck] is per node and per log, because the barrier is about
// processes and a window is per log.

// MaintenancePhase is where a capacity operation stands.
//
// The phases advance only FORWARDS within an attempt, and each is established
// by evidence rather than by a caller saying so — see [statelog.PermitPhase],
// which is the one place the transitions are stated.
type MaintenancePhase string

const (
	// PhaseOpened — the record exists and nothing else has happened. No
	// configuration request has been issued, so there is nothing to
	// retire and an abandonment here clears outright.
	PhaseOpened MaintenancePhase = "opened"

	// PhaseBaselined — the participant set, every participant's
	// incarnation and the journal record are persisted, BEFORE the first
	// request is issued, and again before every later one a resumed window
	// sends. A crash from here must assume a request MAY be in flight.
	PhaseBaselined MaintenancePhase = "baselined"

	// PhaseApplied — a read-back observed the ceiling equal to the
	// target. It is not the end: another request may still be in flight,
	// and observing the value retires nothing.
	PhaseApplied MaintenancePhase = "applied"

	// PhaseSealing — a node in seal mode is collecting the barrier for
	// this attempt: the fleet is being restarted seal-only, and the seal
	// holds once every participant's process has been replaced by one
	// booted into seal mode.
	PhaseSealing MaintenancePhase = "sealing"

	// PhaseSealed — every participant acked for this attempt with an
	// incarnation that is not the one recorded as the newest request left,
	// and every journal record for this operation is completed or retired.
	PhaseSealed MaintenancePhase = "sealed"

	// PhaseConfirmed — the verify matched. The one remaining write is the
	// delete, and that write releases the exclusion.
	PhaseConfirmed MaintenancePhase = "confirmed"
)

// MaintenancePhases are the six, in order.
var MaintenancePhases = []MaintenancePhase{
	PhaseOpened, PhaseBaselined, PhaseApplied,
	PhaseSealing, PhaseSealed, PhaseConfirmed,
}

// Valid reports whether a phase off the wire is one this build knows.
func (p MaintenancePhase) Valid() bool {
	for _, known := range MaintenancePhases {
		if p == known {
			return true
		}
	}
	return false
}

// MaintenanceBlock is why a capacity operation can't go on without a person,
// as the operation carries it.
//
// A CODE FOR SCRIPTS, NEVER FOR A PERSON: the record, the API and the report
// carry it so a script can branch on it. What a person reads is
// statelog.BlockedDetail, one sentence per block saying who blocked the window
// and what clears it, which a test holds to every block declared here.
type MaintenanceBlock string

const (
	// BlockAbandoned is an operation an operator abandoned
	// (statelog.AbandonPhase). It no longer tries to reach its target, and
	// closes once its seal holds, at whatever ceiling the log has then
	// (statelog.Verifies).
	BlockAbandoned MaintenanceBlock = "abandoned_by_operator"

	// BlockUnsealed is an operation that used every attempt at its target
	// and still finds another ceiling (statelog.NextAttempt).
	BlockUnsealed MaintenanceBlock = "capacity_operation_unsealed"

	// BlockTooManyParticipants is an operation whose next request would
	// count more participants than [MaxMaintenanceParticipants]: the fleet
	// grew while the window waited, and no request is sent until it is
	// abandoned or the fleet is back within the bound.
	BlockTooManyParticipants MaintenanceBlock = "capacity_fleet_too_large"

	// BlockTooManyRecorded is an operation whose next request would record
	// more participants than [MaxMaintenanceRecorded] while counting no more
	// than it may: retired ids stay recorded, so only excluding them or
	// abandoning the window clears it.
	BlockTooManyRecorded MaintenanceBlock = "capacity_window_records_too_many"

	// BlockJournalFull is an operation whose next request would be a ninth
	// unresolved one ([MaxMaintenanceJournal]): no request is sent until a
	// seal retires the eight outstanding.
	BlockJournalFull MaintenanceBlock = "capacity_journal_full"
)

// MaintenanceBlocks are the blocks this build writes.
var MaintenanceBlocks = []MaintenanceBlock{BlockAbandoned, BlockUnsealed, BlockTooManyParticipants,
	BlockTooManyRecorded, BlockJournalFull}

// Valid reports whether a block off the wire is one this build knows. The
// empty block is no block, and not valid: an operation that isn't blocked
// carries none.
func (b MaintenanceBlock) Valid() bool {
	for _, known := range MaintenanceBlocks {
		if b == known {
			return true
		}
	}
	return false
}

// HoldsRequests reports whether b is a block that holds the window's next
// request back ([BlockTooManyParticipants], [BlockTooManyRecorded],
// [BlockJournalFull]): lifted by the next request that fits, or by the seal,
// which resolves what is outstanding and after which the next attempt judges
// the fleet afresh.
func (b MaintenanceBlock) HoldsRequests() bool {
	return b == BlockTooManyParticipants || b == BlockTooManyRecorded || b == BlockJournalFull
}

// Index is a phase's position in the order, or -1.
func (p MaintenancePhase) Index() int {
	for i, known := range MaintenancePhases {
		if p == known {
			return i
		}
	}
	return -1
}

// JournalState is a write-attempt record's THREE-VALUED outcome.
//
// "I am about to issue a configuration request" is an UNKNOWN, as a publish
// whose call errored is: it may have landed. An unknown is RESOLVED BY a
// procedure (the seal) and is never a veto over the procedure that resolves it.
type JournalState string

const (
	// JournalIssued — a request may be in flight and the outcome is
	// unknown. Written BEFORE the request leaves.
	JournalIssued JournalState = "issued"

	// JournalCompleted — the issuing process observed the target in its
	// own read-back.
	JournalCompleted JournalState = "completed"

	// JournalRetired — no process that could deliver this request still
	// exists.
	//
	// ESTABLISHED BY THE BARRIER AND NEVER BY A VALUE: another request may
	// have set the target while this one is still in flight, and retiring
	// on value equality declares a live request dead.
	JournalRetired JournalState = "retired"
)

// JournalRecord is one write attempt against the stream's configuration.
type JournalRecord struct {
	// Attempt is which attempt of this operation issued it, and By which
	// coordinator incarnation.
	Attempt int    `json:"attempt"`
	By      string `json:"by"`

	// State is the three-valued outcome.
	State JournalState `json:"state"`

	// At is when it was written, and ResolvedAt when it reached a
	// terminal state.
	At         time.Time `json:"at"`
	ResolvedAt time.Time `json:"resolved_at,omitzero"`

	// Evidence is what established a retirement: the participant
	// incarnations the barrier observed and the ceiling it read. Empty on
	// the other two states.
	Evidence string `json:"evidence,omitempty"`
}

// MaintenanceOperation is the one record.
type MaintenanceOperation struct {
	// Stream is which log's configuration this is about, and the key.
	Stream string `json:"stream"`

	// OperationID is minted ONCE and reused across every create attempt,
	// so "the key holds my id" means "one of my attempts won" rather than
	// "somebody else's operation is here".
	OperationID string `json:"operation_id"`

	// TargetMaxBytes is chosen once and never changed. A retry re-applies
	// the SAME target under a new attempt number: a target chosen again
	// would be a second decision nothing fences.
	TargetMaxBytes uint64 `json:"target_max_bytes"`

	// OriginalMaxBytes is what the ceiling was when the operation opened.
	// It is what the PENDING classification compares against, so deleting
	// it would delete the reconcile.
	OriginalMaxBytes uint64 `json:"original_max_bytes"`

	// Phase is where this operation stands and Attempt which write-then-
	// seal cycle it is on.
	//
	// ONE COUNTER for writes and seals: two numbers that must move together
	// let a seal predicate exempt the same-attempt record the journal exists
	// to catch. The seal compares no attempts; every journal record must be
	// completed or retired.
	Phase   MaintenancePhase `json:"phase"`
	Attempt int              `json:"attempt"`

	// Participants is who must acknowledge. NOT the retention counted set:
	// that set is about whose position pins the trim, and this one is
	// about whose process could hold an outstanding request. An eviction
	// waives nothing here.
	Participants []string `json:"participants"`

	// WriteIncarnations is each participant's incarnation as this attempt's
	// newest request left, and the seal compares an ack against it.
	//
	// PERSISTED BEFORE EACH REQUEST IS ISSUED, by its issuer, in the write
	// that adds its journal record. Recorded after the read-back, a crash
	// between leaves a possible write with nothing to compare against;
	// recorded once at the baseline, a request a resumed window sent later
	// is compared against processes gone before it left, and a node that
	// restarted into seal mode in between passes without restarting after.
	WriteIncarnations map[string]string `json:"write_incarnations,omitempty"`

	// Excluded names the nodes an operator has asserted, for this window, are
	// stopped and hold no outstanding request: no longer participants, each
	// stored once here. An exclusion lapses only when the node is running
	// again as a request is about to leave, which then records it; a
	// retirement waives only while it stands ([MaintenanceOperation.Retired]).
	Excluded []string `json:"excluded,omitempty"`

	// Retired names the participants whose retirement stands as the window
	// was read ([NodeRetirement.Stands]): not running, no row since. FILLED
	// BY EVERY READ, NEVER STORED, since it is judged afresh each time: the
	// seal, the baseline and the cap waive each while it stands, and the
	// moment the node runs again it is waited for like any participant.
	Retired []string `json:"-"`

	// RetiredUnjudged says the read couldn't judge Retired, which is then
	// empty: no answer to who is retired, so nothing built on it is said.
	// Never stored, like Retired.
	RetiredUnjudged bool `json:"-"`

	// ExcludedBy is who made each of those assertions, keyed by the node
	// it names, as By and OperatorID are for whoever opened the window.
	//
	// AN EXCLUSION IS THE ONE FACT THE SEAL TAKES ON SOMEBODY'S WORD, so
	// whose word is the first question a post-mortem asks. It is written in
	// the same update that adds the node to Excluded, so every excluded
	// node has one.
	ExcludedBy map[string]MaintenanceParty `json:"excluded_by,omitempty"`

	// Journal is every write attempt against this stream's configuration
	// under this operation: one record per request, written before it
	// leaves.
	Journal []JournalRecord `json:"journal,omitempty"`

	// ObservedMaxBytes is what the last read-back saw.
	ObservedMaxBytes uint64 `json:"observed_max_bytes,omitempty"`

	// Blocked names why the operation cannot proceed without a person,
	// empty while it can.
	Blocked MaintenanceBlock `json:"blocked,omitempty"`

	// Outgrown is how many participants the next request would have counted
	// ([BlockTooManyParticipants], past [MaxMaintenanceParticipants]) or
	// recorded ([BlockTooManyRecorded], past [MaxMaintenanceRecorded]) when
	// it was refused, zero otherwise: what the block's sentence names.
	//wiregate:absent-is-zero only a window blocked for its participants counts any
	Outgrown int `json:"outgrown,omitempty"`

	// AbandonedBy is who abandoned the operation, zero until somebody does.
	// An abandonment that may have left a request outstanding keeps the
	// record, and the fleet in its window, until the seal restart, so the
	// operation names whose decision it is still carrying out. One made
	// before any request clears the record outright.
	AbandonedBy MaintenanceParty `json:"abandoned_by,omitzero"`

	// EnteredAt is when the exclusion was taken and By which operator ran
	// the verb — the author — with OperatorID the credential they ran it
	// through: the machine token or browser session they acted through, or
	// their own login where that is the credential (internal/iam's
	// ActorFor). Revision is the store's own, for the compare-and-set every
	// mutation is.
	EnteredAt  time.Time `json:"entered_at"`
	By         string    `json:"by"`
	OperatorID string    `json:"operator_id,omitempty"`
	Revision   uint64    `json:"-"`
}

// MaintenanceParty is who made one gesture on a capacity window: the author,
// and the credential they made it through — `pat:<id>` for a machine token
// acting as its owner, `session:<lineage>` for a browser session, a Tier A
// token's own login.
type MaintenanceParty struct {
	By         string `json:"by"`
	OperatorID string `json:"operator_id,omitempty"`
}

// MaintenanceAck is one participant's evidence that its process restarted,
// for one log's window.
type MaintenanceAck struct {
	// Stream is which log's window this answers, and NodeID who
	// acknowledged: the key is the two together ([MaintenanceAckKey]).
	Stream string `json:"stream"`
	NodeID string `json:"node_id"`

	// OperationID and Attempt bind the acknowledgement: the seal takes none
	// that does not name its own, or a restart for an unrelated reason
	// would satisfy it.
	//
	// BOTH EMPTY on the record a node writes booting into a maintenance
	// mode while no window is open on the log ([MaintenanceAck.Windowless]).
	// It names the process and mode, all a BASELINE reads, and binds
	// nothing, so no seal accepts it. The fleet restarts into maintenance
	// mode before anybody runs the verb, so a baseline reading only bound
	// acknowledgements would find none but the coordinator's.
	OperationID string `json:"operation_id"`
	Attempt     int    `json:"attempt"`

	// Incarnation is this PROCESS's identity, which the seal compares
	// against the one recorded as the newest request left.
	//
	// COMPARED FOR INEQUALITY, not order: an incarnation is minted per
	// process from a uuid, unique rather than ordered, and a different
	// identity is exactly the evidence that the process holding the
	// request is gone.
	Incarnation string `json:"incarnation"`

	// Mode says which start mode acknowledged. A seal acknowledgement
	// from a node that started its publishers proves the opposite of what
	// the barrier needs.
	Mode string `json:"mode"`

	At time.Time `json:"at"`
}

// Windowless reports an acknowledgement made while no window was open on its
// log, which names no operation and no attempt.
func (a MaintenanceAck) Windowless() bool { return a.OperationID == "" && a.Attempt == 0 }

// Admission is one node's positive record that it is about to publish.
//
// NO TTL, EVER: a node whose admission expired while it went on publishing is
// invisible to the coordinator's check. A crashed process's admission is
// retired by its node booting again as another process: into a maintenance
// mode, which withdraws it once the fleet's presence shows the process it
// names is not running (its presence lease lapsed, or another process of the
// node holds it), or in service as a data node, whose own admission replaces
// it.
type Admission struct {
	// NodeID is who admitted itself, and the key.
	NodeID string `json:"node_id"`

	// Incarnation is the process that holds it. A late cleanup deletes an
	// admission only against this value, so it cannot remove a newer
	// process's.
	Incarnation string `json:"incarnation"`

	At time.Time `json:"at"`
}

// MaintenanceRegister is the fleet's record of a capacity operation, the
// admissions that block one, and the acknowledgements that seal one.
type MaintenanceRegister interface {
	// OpenMaintenance creates the record, first-writer-wins.
	//
	// It reports the operation that IS there when the key is taken, which
	// may be this caller's own: an unknown create is retried with the same
	// id. Telling "mine" from "somebody else's" by that id is the caller's.
	OpenMaintenance(ctx context.Context, op MaintenanceOperation) (MaintenanceOperation, bool, error)

	// Maintenance reads one stream's operation, or reports that there is
	// none.
	Maintenance(ctx context.Context, stream string) (MaintenanceOperation, bool, error)

	// UpdateMaintenance writes it back, conditional on the revision it was
	// read at.
	//
	// A COMPARE-AND-SET ON THE REVISION, never on the operation id: a
	// successor RESUMES the same id, so an id comparison would let a
	// coordinator that lost its lease commit over a successor's progress.
	UpdateMaintenance(ctx context.Context, op MaintenanceOperation) error

	// CloseMaintenance deletes it, conditional on the same revision. That
	// single write releases the exclusion.
	CloseMaintenance(ctx context.Context, stream, operationID string, revision uint64) error

	// PutAdmission records that a node is about to publish, and
	// ForgetAdmission withdraws it: by its own process on an orderly
	// shutdown after publishers drain, by the node's NEXT process booting
	// into a maintenance mode while the one named holds no presence lease,
	// or with an operator's exclusion. Always conditional on the
	// incarnation, so none of the three can remove a newer process's.
	PutAdmission(ctx context.Context, a Admission) error
	Admissions(ctx context.Context) ([]Admission, error)
	ForgetAdmission(ctx context.Context, nodeID, incarnation string) error

	// PutMaintenanceAck records one participant's restart for one log's
	// window, and MaintenanceAcks reads every one for the log named, in
	// node order. Never another log's: a window on one log is sealed by
	// its own acknowledgements, whatever else is open.
	PutMaintenanceAck(ctx context.Context, ack MaintenanceAck) error
	MaintenanceAcks(ctx context.Context, stream string) ([]MaintenanceAck, error)
}

// MaintenanceKey addresses one stream's capacity operation, which is to say
// the exclusion itself: the key's existence is what keeps publishers off that
// stream, so there is exactly one per stream and the delete is the release.
//
// The window's three key classes (this, [AdmissionKey], [MaintenanceAckKey])
// live in the positions register, the bucket with no age, because each must
// outlive any clock: an expiring operation admits publishers, an expiring
// admission hides one, and an expiring acknowledgement un-seals a barrier that
// has already run.
func MaintenanceKey(stream string) string { return DocumentKey(MaintenanceClass, stream) }

// AdmissionKey addresses one node's admission: its POSITIVE record that it
// intends to publish, which a coordinator establishes exclusion against. Per
// node rather than per stream, because the handshake is about which processes
// are running (package doc, "Maintenance mode").
func AdmissionKey(nodeID string) string { return DocumentKey(AdmissionClass, nodeID) }

// MaintenanceAckKey addresses one participant's acknowledgement that its
// process restarted for the current attempt of one log's window. Per node
// because the seal is established from evidence each process leaves about
// itself, never a fleet-wide fact a coordinator could assert; per log because
// two windows can be open at once. The log is the second segment, so one log's
// acknowledgements are read with a filter the broker matches.
func MaintenanceAckKey(stream, nodeID string) string {
	return DocumentKey(MaintenanceAckClass, stream, nodeID)
}

// The capacity window's three key classes in the positions register: the
// operation ([MaintenanceKey]), an admission ([AdmissionKey]) and an
// acknowledgement ([MaintenanceAckKey]).
const (
	MaintenanceClass    = "maintenance"
	AdmissionClass      = "admitted"
	MaintenanceAckClass = "maintenance-ack"
)

// MaintenanceResource is the coordinator lease's resource name.
//
// AN ORDINARY LEASE, with the ordinary TTL and fencing epoch: what it decides
// is which maintenance-mode node is driving, and it never decides whether a
// publisher may run. That split is the whole reason the state above has no TTL.
func MaintenanceResource(stream string) string { return "maintenance:" + stream }

// Validate reports why an operation cannot be written.
func (op MaintenanceOperation) Validate() error {
	switch {
	case strings.TrimSpace(op.Stream) == "":
		return fmt.Errorf("coord: a maintenance operation names no stream — it " +
			"is the key, so one without it would exclude publishers from a log " +
			"nobody named")
	case strings.TrimSpace(op.OperationID) == "":
		return fmt.Errorf("coord: the maintenance operation on %s has no id: "+
			"every mutation is conditional on it, and a successor resumes by "+
			"it", op.Stream)
	case !op.Phase.Valid():
		return fmt.Errorf("coord: the maintenance operation on %s is in phase "+
			"%q, which this build does not know — a phase off the wire is a "+
			"value rather than a panic, and an unknown one must not be "+
			"advanced past", op.Stream, op.Phase)
	case op.Attempt < 1:
		return fmt.Errorf("coord: the maintenance operation on %s is on attempt "+
			"%d: attempts are numbered from one, and a zero would make the "+
			"budget carried on this record unreadable", op.Stream, op.Attempt)
	case op.TargetMaxBytes == 0:
		return fmt.Errorf("coord: the maintenance operation on %s targets zero "+
			"bytes — an unbounded log is a real setting and this verb does not "+
			"set it, so a zero here is a target nobody chose", op.Stream)
	case len(op.Participants) == 0 && (len(op.Excluded) == 0 || len(op.Journal) > 0):
		// EVERY PARTICIPANT EXCLUDED BEFORE ANY REQUEST is a set an operator
		// emptied by name with nothing outstanding. Once one has left, the
		// node that sent it is recorded, so an empty set would seal it
		// against no process at all.
		return fmt.Errorf("coord: the maintenance operation on %s names no "+
			"participants: the seal is established from their acknowledgements, "+
			"and an empty set seals immediately against nothing", op.Stream)
	}
	return op.bounded()
}

// MaxMaintenanceParticipants is the most nodes one capacity window names as
// participants. REASONED: every data node holds the whole estate and every
// broker member queues the window's request, so a fleet runs a handful of
// them, and the docs describe a dozen at most (as [MaxMCPCheckNodes] reasons);
// sixteen, with every field at its widest and [MaxMaintenanceJournal] records,
// keeps the operation a small value a boot reads at the term (ADR-0175). A
// window on a larger fleet is refused, or blocked for a person
// ([BlockTooManyParticipants]).
const MaxMaintenanceParticipants = 16

// MaxMaintenanceRecorded is the most participants a window records, those a
// retirement waives as it is read included: twice the most it may count, so
// as many again may be retired while it is open. A window past it is blocked
// for a person ([BlockTooManyRecorded]).
const MaxMaintenanceRecorded = 2 * MaxMaintenanceParticipants

// Waived is every node the window asks no acknowledgement of as it was read:
// each one excluded, and each participant whose retirement stands.
func (op MaintenanceOperation) Waived() []string {
	return append(slices.Clone(op.Excluded), op.Retired...)
}

// Counted is every participant the window counts as it was read: those a
// standing retirement waives left out.
func (op MaintenanceOperation) Counted() []string {
	return slices.DeleteFunc(slices.Clone(op.Participants), func(n string) bool {
		return slices.Contains(op.Retired, n)
	})
}

// MaxMaintenanceJournal is the most journal records an operation keeps: the
// oldest resolved records leave first, an issued one never, and a request
// that would be a ninth issued one is not sent ([BlockJournalFull]).
const MaxMaintenanceJournal = 8

// MaxJournalEvidenceBytes bounds a retired record's evidence: the engine cuts
// the barrier's account to it, the ceiling first, which nothing compares.
const MaxJournalEvidenceBytes = 256

// maxMaintenanceWordBytes bounds a phase, block, state or mode a record names:
// a word of a closed table, or a newer build's.
const maxMaintenanceWordBytes = 64

// bounded refuses an operation any of whose fields is past what a record of
// the positions register's small class holds (ADR-0175).
func (op MaintenanceOperation) bounded() error {
	words := func(ws ...string) bool {
		for _, w := range ws {
			if !plainWithin(w, maxMaintenanceWordBytes) {
				return false
			}
		}
		return true
	}
	nodes := func(ids []string) bool {
		for _, id := range ids {
			if !plainWithin(id, MaxNodeIDBytes) {
				return false
			}
		}
		return true
	}
	switch {
	case !ValidStreamName(op.Stream):
		return fmt.Errorf("coord: a maintenance operation names %q, which is no log's stream", op.Stream)
	case !plainWithin(op.OperationID, MaxIncarnationBytes):
		return fmt.Errorf("coord: the maintenance operation on %s has an id past %d plain bytes",
			op.Stream, MaxIncarnationBytes)
	case len(op.Participants) > MaxMaintenanceRecorded || len(op.Excluded) > MaxMaintenanceParticipants ||
		len(op.WriteIncarnations) > MaxMaintenanceParticipants || len(op.ExcludedBy) > MaxMaintenanceParticipants:
		return fmt.Errorf("coord: the maintenance operation on %s names %d participants and %d excluded, "+
			"past the %d recorded, %d recorded at a request and %d excluded one window holds", op.Stream,
			len(op.Participants), len(op.Excluded), MaxMaintenanceRecorded, MaxMaintenanceParticipants,
			MaxMaintenanceParticipants)
	case !nodes(op.Participants) || !nodes(op.Excluded):
		return fmt.Errorf("coord: the maintenance operation on %s names a participant that is no "+
			"node id", op.Stream)
	case len(op.Journal) > MaxMaintenanceJournal:
		return fmt.Errorf("coord: the maintenance operation on %s keeps %d journal records, past "+
			"the %d one holds", op.Stream, len(op.Journal), MaxMaintenanceJournal)
	case !words(string(op.Phase), string(op.Blocked)):
		return fmt.Errorf("coord: the maintenance operation on %s names a phase or block that is "+
			"no word of either table", op.Stream)
	}
	for _, err := range []error{
		checkActor("the maintenance operation on "+op.Stream, op.By, op.OperatorID),
		checkActor("the maintenance operation on "+op.Stream, op.AbandonedBy.By, op.AbandonedBy.OperatorID),
	} {
		if err != nil {
			return err
		}
	}
	for node, incarnation := range op.WriteIncarnations {
		if !plainWithin(node, MaxNodeIDBytes) || !plainWithin(incarnation, MaxIncarnationBytes) {
			return fmt.Errorf("coord: the maintenance operation on %s records an incarnation that "+
				"is no node's process", op.Stream)
		}
	}
	for node, party := range op.ExcludedBy {
		if !plainWithin(node, MaxNodeIDBytes) {
			return fmt.Errorf("coord: the maintenance operation on %s excludes %q, no node id",
				op.Stream, node)
		}
		if err := checkActor("the maintenance operation on "+op.Stream, party.By, party.OperatorID); err != nil {
			return err
		}
	}
	for _, r := range op.Journal {
		if !plainWithin(r.By, MaxIncarnationBytes) || !words(string(r.State)) ||
			!plainWithin(r.Evidence, MaxJournalEvidenceBytes) {
			return fmt.Errorf("coord: the maintenance operation on %s keeps a journal record whose "+
				"sender, state or evidence is past its bound (%d plain bytes of evidence)", op.Stream,
				MaxJournalEvidenceBytes)
		}
	}
	return nil
}

// Validate reports why an acknowledgement cannot be written.
func (a MaintenanceAck) Validate() error {
	switch {
	case strings.TrimSpace(a.Stream) == "":
		return fmt.Errorf("coord: node %s acknowledged no log's window: an "+
			"acknowledgement is kept per log, and one naming none would be read "+
			"beside no window at all", a.NodeID)
	case strings.TrimSpace(a.NodeID) == "":
		return fmt.Errorf("coord: a maintenance acknowledgement names no node — " +
			"it is the key, and the barrier is about which processes restarted")
	case strings.TrimSpace(a.Incarnation) == "":
		return fmt.Errorf("coord: node %s acknowledged with no incarnation, "+
			"which is the whole evidence: the seal establishes that the process "+
			"holding a request is gone, and an empty identity establishes "+
			"nothing", a.NodeID)
	case a.Windowless():
		// A RESTART MADE WHILE NO WINDOW WAS OPEN, which binds nothing and
		// which no seal accepts ([MaintenanceAck.OperationID]).
	case strings.TrimSpace(a.OperationID) == "":
		return fmt.Errorf("coord: node %s acknowledged attempt %d of no "+
			"operation: an attempt is a number within one operation, and one "+
			"that named none would be satisfied by a restart for an unrelated "+
			"reason", a.NodeID, a.Attempt)
	case a.Attempt < 1:
		return fmt.Errorf("coord: node %s acknowledged attempt %d, and prior "+
			"attempts are invalidated by the number rather than deleted — so a "+
			"zero is an ack for every attempt at once", a.NodeID, a.Attempt)
	}
	// PAST THE EMPTY ONES, so a missing field is refused as missing.
	if !ValidStreamName(a.Stream) || !plainWithin(a.NodeID, MaxNodeIDBytes) ||
		!plainWithin(a.OperationID, MaxIncarnationBytes) || !plainWithin(a.Incarnation, MaxIncarnationBytes) ||
		!plainWithin(a.Mode, maxMaintenanceWordBytes) {
		return fmt.Errorf("coord: node %q's maintenance acknowledgement names a log, node, operation, "+
			"process or mode past its bound", a.NodeID)
	}
	return nil
}

// Validate reports why an admission cannot be written.
func (a Admission) Validate() error {
	switch {
	case strings.TrimSpace(a.NodeID) == "":
		return fmt.Errorf("coord: an admission names no node — it is the key")
	case !plainWithin(a.NodeID, MaxNodeIDBytes) || !plainWithin(a.Incarnation, MaxIncarnationBytes):
		return fmt.Errorf("coord: an admission names a node or process past its bound (%d and %d "+
			"plain bytes)", MaxNodeIDBytes, MaxIncarnationBytes)
	case strings.TrimSpace(a.Incarnation) == "":
		return fmt.Errorf("coord: node %s admitted itself with no incarnation: a "+
			"late cleanup deletes an admission only against this value, and an "+
			"empty one would let it remove a newer process's", a.NodeID)
	}
	return nil
}
