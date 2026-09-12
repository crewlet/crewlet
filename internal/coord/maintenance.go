package coord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrMaintenanceMoved reports a compare-and-set that lost its race: the
// operation record changed between the read this write was formed from and the
// write itself.
//
// A SENTINEL rather than a message, because the caller's answer is specific
// and not a retry: re-read the record and re-decide from the phase it is
// actually in. A coordinator that lost its lease and came back holds a view
// that a successor has moved past, and blindly retrying its own write is
// exactly what the compare-and-set exists to refuse.
var ErrMaintenanceMoved = errors.New("coord: the maintenance operation moved since it was read")

// MAINTENANCE MODE: what a fleet does when a stream's own configuration has to
// change, and why it is one record rather than several.
//
// # What the mode is for
//
// A stream's byte ceiling can be raised or lowered, and neither direction is
// safe while anything is publishing. A reduction below current usage is
// refused by the broker, so the usage has to be an OBSERVATION of a quantity
// nothing can move — and a raise is no different, because what makes a
// configuration write safe is knowing which requests are outstanding, not
// which way the number went.
//
// What retires an outstanding request is not a closed client. A request the
// broker has RECEIVED AND QUEUED lives in a server's receive buffer and its
// connection state; closing the client cancels the client's wait and not the
// server's work. Both die with the broker PROCESS — so the fleet restarts
// into a mode that starts every broker and no publisher, and the exclusion is
// structural rather than observed.
//
// # Why the exclusion and the operation are ONE record
//
// They have one lifetime. Maintenance exists exactly while an operation is
// unresolved; there is no maintenance without an operation and no unresolved
// operation without maintenance. Held as two records, an interrupted
// activation — state written, operation not yet created — is byte-identical to
// a finished resize whose cleanup was interrupted, and any rule that reads one
// reads the other: it releases the exclusion while a coordinator is still
// preparing.
//
// So [MaintenanceOperation] IS the exclusion. Its existence excludes
// publishers, its deletion releases them, and there is no crash-between-the-
// halves state to interpret because there are no halves.
//
// # The three things that are NOT this record, and why
//
//   - THE COORDINATOR'S LEASE has a TTL and this record must not. They have
//     different lifetimes on purpose: the lease expires so another
//     maintenance-mode node can take over the same operation, and the state
//     must never expire, because a key that forgets is a key that admits a
//     publisher. A coordinator that is merely paused, GC-stalled or
//     partitioned has not finished its operation.
//   - AN ADMISSION is one node's positive record that it intends to publish.
//     Admission is a HANDSHAKE rather than an absence check: a node writes
//     [Admission] and then re-reads this record, and a coordinator establishes
//     exclusion only when no admission exists. Either order is safe, which is
//     the point — a node that slipped between the coordinator's check and its
//     own start leaves a durable key that blocks the coordinator, where an
//     absence would have let it proceed.
//   - AN ACK is one participant's evidence that its process restarted for this
//     operation's current attempt. It is what the seal is established from,
//     and it is per node because the barrier is about processes rather than
//     about a fleet-wide fact anybody could assert.

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
	// incarnation and the journal record are persisted, BEFORE the
	// request is issued. A crash from here must assume a request MAY be
	// in flight.
	PhaseBaselined MaintenancePhase = "baselined"

	// PhaseApplied — a read-back observed the ceiling equal to the
	// target. It is not the end: another request may still be in flight,
	// and observing the value retires nothing.
	PhaseApplied MaintenancePhase = "applied"

	// PhaseSealing — the fleet has been stopped, verified stopped, and
	// restarted seal-only for this attempt.
	PhaseSealing MaintenancePhase = "sealing"

	// PhaseSealed — every participant acked for this attempt with an
	// incarnation that is not its baselined one, and every journal record
	// for this operation is completed or retired.
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
// A record of "I am about to issue a configuration request" is an UNKNOWN in
// exactly the sense a publish whose call returned an error is: it may have
// landed and it may not. An unknown is RESOLVED BY a procedure; it is never a
// veto over the procedure that resolves it — which is the shape that made an
// earlier draft's journal block the very seal that clears it.
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
	// ESTABLISHED BY THE BARRIER AND NEVER BY A VALUE. Observing the
	// ceiling equal to the target retires nothing: another request may
	// have set it while this one is still in flight, and a record retired
	// on value equality declares a live request dead because somebody
	// else wrote the same number.
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
	// ONE COUNTER. An earlier draft carried a separate seal attempt, and
	// two numbers that must move together is this record's own two-record
	// bug in miniature — it produced a seal predicate whose comparison
	// exempted the same-attempt record the journal exists to catch. There
	// is nothing to compare now: every journal record must be completed or
	// retired.
	Phase   MaintenancePhase `json:"phase"`
	Attempt int              `json:"attempt"`

	// Participants is who must acknowledge. NOT the retention counted set:
	// that set is about whose position pins the trim, and this one is
	// about whose process could hold an outstanding request. An eviction
	// waives nothing here.
	Participants []string `json:"participants"`

	// WriteIncarnations is each participant's incarnation as of the
	// baseline, and the seal compares an ack against it.
	//
	// PERSISTED BEFORE THE REQUEST IS ISSUED. Recorded after the
	// read-back, a crash in between leaves an operation that may have
	// written with nothing to compare against.
	WriteIncarnations map[string]string `json:"write_incarnations,omitempty"`

	// Excluded names participants an operator has asserted are stopped and
	// hold no outstanding request. It is the ONLY thing that waives an
	// acknowledgement.
	Excluded []string `json:"excluded,omitempty"`

	// Journal is every write attempt against this stream's configuration
	// under this operation.
	Journal []JournalRecord `json:"journal,omitempty"`

	// ObservedMaxBytes is what the last read-back saw, and Verified
	// whether the final verify matched the target.
	ObservedMaxBytes uint64 `json:"observed_max_bytes,omitempty"`

	// Blocked names why the operation cannot proceed without a person,
	// empty while it can.
	Blocked string `json:"blocked,omitempty"`

	// EnteredAt is when the exclusion was taken and By which operator ran
	// the verb. Revision is the store's own, for the compare-and-set every
	// mutation is.
	EnteredAt time.Time `json:"entered_at"`
	By        string    `json:"by"`
	Revision  uint64    `json:"-"`
}

// MaintenanceAck is one participant's evidence that its process restarted.
type MaintenanceAck struct {
	// NodeID is who acknowledged, and the key.
	NodeID string `json:"node_id"`

	// OperationID and Attempt bind the acknowledgement. An ack that named
	// neither would be satisfied by a restart that happened for an
	// unrelated reason.
	OperationID string `json:"operation_id"`
	Attempt     int    `json:"attempt"`

	// Incarnation is this PROCESS's identity, which the seal compares
	// against the baselined one.
	//
	// COMPARED FOR INEQUALITY rather than for order, because an
	// incarnation is minted per process from a uuid: it is unique rather
	// than ordered, and an ordering would be a second property nothing
	// mints. What the seal needs is that the process holding the request
	// is gone, and a different identity is exactly that evidence.
	Incarnation string `json:"incarnation"`

	// Mode says which start mode acknowledged. A seal acknowledgement
	// from a node that started its publishers proves the opposite of what
	// the barrier needs.
	Mode string `json:"mode"`

	At time.Time `json:"at"`
}

// Admission is one node's positive record that it is about to publish.
//
// NO TTL, EVER. A key that forgets is a key that admits: a node whose
// admission expired while it went on publishing is invisible to the
// coordinator's check, which is the whole failure the handshake replaces.
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
	// It reports the operation that IS there when the key is taken —
	// which may be this caller's own, because the id is minted once and
	// reused across create attempts, and an unknown create is retried
	// with the same id. Distinguishing "mine" from "somebody else's" is
	// the caller's, on that id.
	OpenMaintenance(ctx context.Context, op MaintenanceOperation) (MaintenanceOperation, bool, error)

	// Maintenance reads one stream's operation, or reports that there is
	// none.
	Maintenance(ctx context.Context, stream string) (MaintenanceOperation, bool, error)

	// UpdateMaintenance writes it back, conditional on the revision it was
	// read at.
	//
	// A COMPARE-AND-SET ON THE REVISION, never on the operation id: a
	// successor RESUMES the same id, so an id comparison checks the one
	// quantity guaranteed not to change and would let a coordinator that
	// lost its lease commit over a successor's progress.
	UpdateMaintenance(ctx context.Context, op MaintenanceOperation) error

	// CloseMaintenance deletes it, conditional on the same revision. That
	// single write releases the exclusion.
	CloseMaintenance(ctx context.Context, stream, operationID string, revision uint64) error

	// PutAdmission records that a node is about to publish, and
	// ForgetAdmission withdraws it — by its own node on an orderly
	// shutdown after publishers drain, or by a coordinator against a
	// node's own strictly-later acknowledgement.
	PutAdmission(ctx context.Context, a Admission) error
	Admissions(ctx context.Context) ([]Admission, error)
	ForgetAdmission(ctx context.Context, nodeID, incarnation string) error

	// PutMaintenanceAck records one participant's restart, and
	// MaintenanceAcks reads them all.
	PutMaintenanceAck(ctx context.Context, ack MaintenanceAck) error
	MaintenanceAcks(ctx context.Context) ([]MaintenanceAck, error)
}

// MaintenanceKey, AdmissionKey and MaintenanceAckKey are the three key
// classes, in the positions register beside the trim's own.
//
// IN THAT REGISTER because it is the one with no age at all, and every one of
// these must outlive any clock: an expiring operation admits publishers, an
// expiring admission hides one, and an expiring acknowledgement un-seals a
// barrier that has already run.
func MaintenanceKey(stream string) string { return DocumentKey("maintenance", stream) }

// AdmissionKey is a node's admission key.
func AdmissionKey(nodeID string) string { return DocumentKey("admitted", nodeID) }

// MaintenanceAckKey is a node's acknowledgement key.
func MaintenanceAckKey(nodeID string) string { return DocumentKey("maintenance-ack", nodeID) }

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
	case len(op.Participants) == 0:
		return fmt.Errorf("coord: the maintenance operation on %s names no "+
			"participants: the seal is established from their acknowledgements, "+
			"and an empty set seals immediately against nothing", op.Stream)
	}
	return nil
}

// Validate reports why an acknowledgement cannot be written.
func (a MaintenanceAck) Validate() error {
	switch {
	case strings.TrimSpace(a.NodeID) == "":
		return fmt.Errorf("coord: a maintenance acknowledgement names no node — " +
			"it is the key, and the barrier is about which processes restarted")
	case strings.TrimSpace(a.OperationID) == "":
		return fmt.Errorf("coord: node %s acknowledged no operation: an ack that "+
			"named none would be satisfied by a restart for an unrelated "+
			"reason", a.NodeID)
	case a.Attempt < 1:
		return fmt.Errorf("coord: node %s acknowledged attempt %d, and prior "+
			"attempts are invalidated by the number rather than deleted — so a "+
			"zero is an ack for every attempt at once", a.NodeID, a.Attempt)
	case strings.TrimSpace(a.Incarnation) == "":
		return fmt.Errorf("coord: node %s acknowledged with no incarnation, "+
			"which is the whole evidence: the seal establishes that the process "+
			"holding a request is gone, and an empty identity establishes "+
			"nothing", a.NodeID)
	}
	return nil
}

// Validate reports why an admission cannot be written.
func (a Admission) Validate() error {
	switch {
	case strings.TrimSpace(a.NodeID) == "":
		return fmt.Errorf("coord: an admission names no node — it is the key")
	case strings.TrimSpace(a.Incarnation) == "":
		return fmt.Errorf("coord: node %s admitted itself with no incarnation: a "+
			"late cleanup deletes an admission only against this value, and an "+
			"empty one would let it remove a newer process's", a.NodeID)
	}
	return nil
}
