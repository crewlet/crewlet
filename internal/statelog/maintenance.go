package statelog

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE CAPACITY OPERATION'S STATE MACHINE, and it is the ONLY place its phases,
// its evidence, its transitions and its ordering are stated.
//
// # Why that sentence is a rule rather than a preference
//
// This procedure was written four times in prose beside the mechanism it
// describes, and each time a later change amended the mechanism and left the
// prose. The copies then disagreed about which set of nodes had to
// acknowledge, when the baseline was recorded, whether the exit was one
// restart or two, and whether an applied operation could be confirmed without
// a seal — and the prose is the thing an implementer follows.
//
// So: every other site says what an OPERATOR does — the verb, its arguments,
// the refusals they will see, what it costs — and points here. A CLI help
// string that restates a transition is a second state machine.
//
// # The shape, in one paragraph
//
// A resize is unsafe while anything publishes, so the fleet restarts into a
// mode with every broker and no publisher. The operation's record IS the
// exclusion ([coord.MaintenanceOperation]). It opens, records a baseline
// BEFORE issuing anything, applies, and is then SEALED by a verified global
// stop — because what retires a request the broker has already queued is the
// broker process restarting, not a client closing. Only a sealed operation may
// be verified, and only a verified one is confirmed and deleted.
//
// # What the arithmetic here does NOT do
//
// It performs no I/O and holds no state. Every function is a decision over
// values, so every phase, every crash point and every illegal transition is
// reachable in a table test with no broker, no fleet and no coordinator — see
// [PermitPhase], [SealHolds] and [Retire].

// ControlRaiseAttempts is how many write-then-seal cycles one operation gets.
//
// THREE, and it is carried ON THE RECORD rather than in the coordinator, so a
// coordinator failure cannot silently reset the budget: a takeover resumes the
// same operation, and a count held in the process that failed would start
// again at one on every takeover — which is an unbounded number of restarts
// advertised as three.
//
// Three because each attempt costs the fleet two restarts and a mismatch after
// three identical attempts is a broker that is not applying what it accepts,
// which is a person's problem rather than a retry's.
const ControlRaiseAttempts = 3

// MaintenanceMode is what a node started for.
type MaintenanceMode string

const (
	// ModeNormal starts every publisher.
	ModeNormal MaintenanceMode = "normal"

	// ModeMaintenance starts the broker, its cluster routes, the
	// coordination estate and a read-only store — and NO publisher: no
	// seats, no duties, no scheduler, no applier write side, no change
	// feed, no API write routes.
	//
	// FLEET-WIDE. One node of three has no majority, so a single
	// maintenance node can commit neither the configuration update nor
	// its own coordination record — and it is every BROKER restarting
	// that retires what the brokers had queued.
	ModeMaintenance MaintenanceMode = "maintenance"

	// ModeSeal is ModeMaintenance with configuration writes refused too.
	// It exists so the stop barrier can be established by processes that
	// demonstrably cannot have issued the request being retired.
	ModeSeal MaintenanceMode = "seal"
)

// MaintenanceModes are the three.
var MaintenanceModes = []MaintenanceMode{ModeNormal, ModeMaintenance, ModeSeal}

// Valid reports whether a mode off the wire is one this build knows.
func (m MaintenanceMode) Valid() bool { return slices.Contains(MaintenanceModes, m) }

// Publishes reports whether a node in this mode starts anything that writes.
func (m MaintenanceMode) Publishes() bool { return m == ModeNormal }

// WritesConfiguration reports whether a node in this mode may issue a stream
// configuration request. Seal mode may not, which is what makes an ack from it
// evidence rather than a claim.
func (m MaintenanceMode) WritesConfiguration() bool { return m == ModeMaintenance }

// PhaseEvidence is what a caller has established, offered to [PermitPhase].
//
// A STRUCT OF FACTS rather than a caller naming the phase it wants: the
// transition table is what decides, and a caller that could name its own next
// phase would be a second state machine.
type PhaseEvidence struct {
	// Now is the deciding instant.
	Now time.Time

	// BaselineWritten reports that the participant set, every
	// participant's incarnation and the journal record are persisted.
	BaselineWritten bool

	// ObservedMaxBytes is what a read-back saw, and Observed whether one
	// happened at all. Zero-with-Observed is a real answer — a log with no
	// ceiling — and the two are separate for that reason.
	ObservedMaxBytes uint64
	Observed         bool

	// Stopped reports that the fleet was verified stopped and restarted
	// seal-only for this attempt.
	Stopped bool

	// Acks is every acknowledgement the coordinator can see.
	Acks []coord.MaintenanceAck

	// Verified reports that the final verify compared the observed
	// ceiling against the immutable target and matched.
	Verified bool
}

// PermitPhase is the transition table: what this operation may advance to
// from where it is, given what has been established.
//
// It answers the SAME phase when nothing has been established, which is how a
// resume works — a coordinator that crashed and came back re-reads the record,
// offers what it can establish, and is told where it stands rather than having
// to reconstruct it.
//
// # No branch reaches confirmed without seal evidence
//
// That is the property the whole table exists for, and the failure it replaces
// is a recovery paragraph that read "applied leads to confirm and clear" —
// written before the seal existed and left in place after, so a recovery
// following it would bypass the protection and never know.
func PermitPhase(op coord.MaintenanceOperation, ev PhaseEvidence) (coord.MaintenancePhase, error) {
	if !op.Phase.Valid() {
		return "", fmt.Errorf("statelog: the operation on %s is in phase %q, "+
			"which this build does not know: a build that cannot read a phase "+
			"must not advance past it, because the phase it would skip is the "+
			"one carrying the evidence rule it does not have", op.Stream, op.Phase)
	}
	switch op.Phase {
	case coord.PhaseOpened:
		if ev.BaselineWritten {
			return coord.PhaseBaselined, nil
		}
	case coord.PhaseBaselined:
		// A READ-BACK EQUAL TO THE TARGET, and nothing else. A request
		// that was accepted is not an applied one: what this phase
		// establishes is that the ceiling IS the target, which is a
		// value the broker reports rather than an acknowledgement it
		// sent.
		if ev.Observed && ev.ObservedMaxBytes == op.TargetMaxBytes {
			return coord.PhaseApplied, nil
		}
		// A CRASH HERE HAS EXACTLY ONE SAFE NEXT STEP, and it is the
		// seal: a request may be in flight, and the only thing that
		// retires one is the barrier. Reaching the seal from here is
		// what breaks the cycle an earlier draft closed, where the
		// unresolved journal record blocked the very seal that clears
		// it.
		if ev.Stopped {
			return coord.PhaseSealing, nil
		}
	case coord.PhaseApplied:
		if ev.Stopped {
			return coord.PhaseSealing, nil
		}
	case coord.PhaseSealing:
		if ok, _ := SealHolds(op, ev.Acks); ok {
			return coord.PhaseSealed, nil
		}
	case coord.PhaseSealed:
		if ev.Verified {
			return coord.PhaseConfirmed, nil
		}
	case coord.PhaseConfirmed:
		// TERMINAL. The one remaining write is the delete.
	}
	return op.Phase, nil
}

// SealHolds reports whether the barrier is established, and names what is
// missing when it is not.
//
// TWO CONDITIONS AND NO COMPARISON.
//
//  1. Every participant that is not excluded has acknowledged THIS operation
//     and THIS attempt, from seal mode, with an incarnation that is not the
//     one recorded in the baseline. Inequality is the evidence: an incarnation
//     is minted per process, so a different one means the process holding any
//     outstanding request is gone.
//  2. Every journal record for this operation is completed or retired.
//
// The second used to be written as "no record from an attempt earlier than
// this one is unresolved", which exempted the same-attempt record the journal
// exists to catch: two coordinators working one attempt of one operation is
// the schedule the mechanism was built for, and a strictly-earlier comparison
// passes straight over it. There is nothing to compare now.
func SealHolds(op coord.MaintenanceOperation, acks []coord.MaintenanceAck) (bool, string) {
	byNode := make(map[string]coord.MaintenanceAck, len(acks))
	for _, ack := range acks {
		byNode[ack.NodeID] = ack
	}
	var missing []string
	for _, node := range op.Participants {
		if slices.Contains(op.Excluded, node) {
			// AN OPERATOR'S ASSERTION that this participant's process
			// is stopped and holds no outstanding request. It is the
			// only thing that waives an acknowledgement — an eviction
			// does not, because eviction is about whose records
			// apply and this is about whose process is running.
			continue
		}
		ack, acked := byNode[node]
		switch {
		case !acked:
			missing = append(missing, node+" has not acknowledged")
		case ack.OperationID != op.OperationID:
			missing = append(missing, node+" acknowledged another operation")
		case ack.Attempt != op.Attempt:
			missing = append(missing, fmt.Sprintf(
				"%s acknowledged attempt %d and this is attempt %d",
				node, ack.Attempt, op.Attempt))
		case ack.Mode != string(ModeSeal):
			missing = append(missing, fmt.Sprintf(
				"%s acknowledged from %q rather than seal mode", node, ack.Mode))
		case ack.Incarnation == op.WriteIncarnations[node]:
			missing = append(missing, node+" acknowledged with the same "+
				"incarnation it was baselined at, so its process did not restart")
		}
	}
	for _, record := range op.Journal {
		if record.State == coord.JournalIssued {
			missing = append(missing, fmt.Sprintf(
				"a write attempt from attempt %d by %s is unresolved",
				record.Attempt, record.By))
		}
	}
	if len(missing) > 0 {
		return false, strings.Join(missing, "; ")
	}
	return true, ""
}

// BarrierEvidence is what a verified global stop observed.
type BarrierEvidence struct {
	// Incarnations is each participant's incarnation as the barrier saw
	// it, and ObservedMaxBytes the ceiling it read.
	Incarnations     map[string]string
	ObservedMaxBytes uint64

	At time.Time
}

// Retire marks every journal record resolved by a barrier, and returns the
// operation with the journal advanced.
//
// # It runs BEFORE the seal predicate is evaluated
//
// That ordering is the clause that breaks the cycle. The journal is not a gate
// on the mechanism that clears it: the barrier needs nothing from the journal,
// it runs, it retires what it covered, and only then is the seal tested
// against a journal with no unresolved entries left.
//
// # And retirement is never established by a value
//
// Observing the ceiling equal to the target retires nothing — another request
// may have set it while an older one is still in flight. What retires a record
// is that every participant restarted, which is a fact about processes, so the
// evidence recorded is the incarnations rather than the number.
func Retire(op coord.MaintenanceOperation, ev BarrierEvidence) coord.MaintenanceOperation {
	out := op
	out.Journal = slices.Clone(op.Journal)
	for i := range out.Journal {
		if out.Journal[i].State != coord.JournalIssued {
			continue
		}
		out.Journal[i].State = coord.JournalRetired
		out.Journal[i].ResolvedAt = ev.At.UTC()
		out.Journal[i].Evidence = describeBarrier(ev)
	}
	return out
}

// describeBarrier renders what the barrier saw, for the record.
func describeBarrier(ev BarrierEvidence) string {
	nodes := make([]string, 0, len(ev.Incarnations))
	for node, incarnation := range ev.Incarnations {
		nodes = append(nodes, node+"="+incarnation)
	}
	slices.Sort(nodes)
	return fmt.Sprintf("ceiling %d observed with incarnations %s",
		ev.ObservedMaxBytes, strings.Join(nodes, ","))
}

// NextAttempt is what a failed verify produces: the SAME immutable target
// under a new attempt number.
//
// # Why a new attempt rather than a re-apply
//
// Every node is in seal mode, which refuses configuration writes, and phases
// advance only forwards — so there is no legal transition from a mismatch back
// to applying. An earlier draft promised a retry its own rules could not
// execute.
//
// A numbered attempt is what makes it legal: maintenance stays active
// throughout, the fleet restarts into the WRITABLE maintenance mode (never a
// write from seal-only mode, which is the rule the barrier depends on), a
// fresh baseline is persisted, the unchanged target is applied, and a fresh
// seal follows. Prior acknowledgements are invalidated by the attempt number
// rather than deleted.
func NextAttempt(op coord.MaintenanceOperation, observed uint64,
	now time.Time) (coord.MaintenanceOperation, error) {

	if op.Attempt >= ControlRaiseAttempts {
		out := op
		out.ObservedMaxBytes = observed
		out.Blocked = "capacity_operation_unsealed"
		return out, fmt.Errorf("statelog: the capacity operation on %s has used "+
			"all %d attempts at %d bytes and the broker reports %d: this is a "+
			"person's decision now — `crewlet retention maintenance status` "+
			"names the attempt count and every participant that did not "+
			"acknowledge",
			op.Stream, ControlRaiseAttempts, op.TargetMaxBytes, observed)
	}
	out := op
	out.Attempt++
	out.Phase = coord.PhaseOpened
	out.ObservedMaxBytes = observed
	// THE BASELINE IS DROPPED WITH THE ATTEMPT, so the next one records
	// its own: an incarnation from the previous attempt compared against
	// an ack from this one would be satisfied by a restart that already
	// happened.
	out.WriteIncarnations = nil
	out.Journal = slices.Clone(op.Journal)
	_ = now
	return out, nil
}

// AbandonPhase is where an operator's abandonment goes from here.
//
// FROM `opened` IT CLEARS OUTRIGHT, because the seal retires configuration
// writes and no request has been issued — there is nothing to retire.
//
// FROM EVERYWHERE ELSE IT SEALS. Abandoning changes what the operation is
// trying to reach; it never changes the barrier it must cross. Reading a
// status page retires nobody's outstanding request, and an earlier draft that
// cleared "deliberately, after the operator has read it" was treating a person
// looking as a process stopping.
func AbandonPhase(op coord.MaintenanceOperation) (coord.MaintenancePhase, bool, error) {
	if !op.Phase.Valid() {
		return "", false, fmt.Errorf("statelog: the operation on %s is in phase "+
			"%q, which this build does not know", op.Stream, op.Phase)
	}
	if op.Phase == coord.PhaseOpened {
		return coord.PhaseOpened, true, nil
	}
	return coord.PhaseSealing, false, nil
}

// DeleteOutcome is what an actor concluded about a delete whose response was
// lost.
type DeleteOutcome string

const (
	// DeleteDone — the key is absent and holds this operation no more, so
	// the delete committed. The work is finished.
	DeleteDone DeleteOutcome = "done"

	// DeleteRetry — the record is still there, still this operation, at
	// the revision this actor read. A successor completes it.
	DeleteRetry DeleteOutcome = "retry"

	// DeleteNotMine — the key holds a DIFFERENT operation. Somebody else's
	// window is open and this actor touches nothing.
	DeleteNotMine DeleteOutcome = "not_mine"
)

// ResolveDelete is what an interrupted delete concluded, READ rather than
// assumed.
//
// A conditional delete whose response was lost may have committed, so "the
// record is unchanged" is not a safe assumption — and neither is its opposite.
// Reading "absent, or a different id" as "my delete failed, retry" is worse
// than either: it deletes the NEXT operation's record and silently releases a
// fleet that is still in maintenance.
func ResolveDelete(operationID string, current coord.MaintenanceOperation, found bool) DeleteOutcome {
	switch {
	case !found:
		return DeleteDone
	case current.OperationID != operationID:
		return DeleteNotMine
	default:
		return DeleteRetry
	}
}

// AdmissionBlocks names every node whose admission stands in the way of taking
// the exclusion.
//
// A POSITIVE RECORD FROM EACH SIDE. A coordinator that merely checked for the
// ABSENCE of a publisher would be back to the check-then-act it replaced: a
// node that read the operation absent and started publishing in the interval
// leaves a durable admission that blocks the coordinator, where a silence
// would have let it proceed.
func AdmissionBlocks(admissions []coord.Admission, excluded []string) []string {
	var blocking []string
	for _, a := range admissions {
		if slices.Contains(excluded, a.NodeID) {
			continue
		}
		blocking = append(blocking, a.NodeID)
	}
	slices.Sort(blocking)
	return blocking
}
