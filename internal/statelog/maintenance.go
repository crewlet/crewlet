package statelog

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/words"
)

// The capacity operation's state machine: its phases, evidence, transitions
// and ordering are stated in this package and nowhere else (package doc, "The
// capacity operation's state machine"). Every other site says what an operator
// does and points here; a CLI help string that restates a transition is a
// second state machine. Nothing here does I/O or holds state, so every phase,
// crash point and illegal transition is reachable in a table test
// ([PermitPhase], [SealHolds], [Retire]).

// ControlRaiseAttempts is how many write-then-seal cycles one operation gets.
//
// It is carried on the record, not in the coordinator, so a takeover resumes
// the count: held in the process that failed, it would restart at one on every
// takeover, unbounded restarts advertised as three. Three because each attempt
// costs two fleet restarts, and a mismatch after three identical attempts is a
// broker not applying what it accepts: a person's problem, not a retry's.
const ControlRaiseAttempts = 3

// MaintenanceMode is what a node started for.
type MaintenanceMode string

const (
	// ModeNormal starts every publisher.
	ModeNormal MaintenanceMode = "normal"

	// ModeMaintenance starts the broker, its cluster routes, the
	// coordination estate and a read-only store, and NO publisher (no
	// seats, duties, scheduler, applier write side, change feed, API write
	// routes or barrier), so a linearizable read is refused
	// [RefuseMaintenance] ([ReaderDeps.Mode]).
	//
	// Fleet-wide: one node of three has no majority to commit the
	// configuration update or its own coordination record, and it is every
	// broker restarting that retires what the brokers had queued.
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

// PhaseEvidence is what a caller has established, offered to [PermitPhase]:
// facts, never a requested phase, since a caller naming its own next phase
// would be a second state machine.
type PhaseEvidence struct {
	// Now is the deciding instant.
	Now time.Time

	// BaselineWritten reports that the participant set, every
	// participant's incarnation and the journal record are persisted.
	BaselineWritten bool

	// ObservedMaxBytes is what a read-back saw, and Observed whether one
	// happened at all: zero with Observed is a real answer, a log with no
	// ceiling.
	ObservedMaxBytes uint64
	Observed         bool

	// Stopped reports that the drive runs in seal mode, so the fleet is
	// being restarted seal-only for this attempt. It is not the barrier:
	// whether every participant has been replaced is the acknowledgements'
	// to say ([SealHolds]).
	Stopped bool

	// Acks is every acknowledgement the coordinator can see.
	Acks []coord.MaintenanceAck

	// Verified reports that the final verify confirms the operation at the
	// ceiling it observed ([Verifies]): the immutable target, or anything at
	// all for an operation an operator abandoned.
	Verified bool
}

// PermitPhase is the transition table: what this operation may advance to
// from where it is, given what has been established. It answers the same
// phase when nothing new is established, which is how a resume works: a
// returning coordinator offers what it can establish and is told where it
// stands rather than reconstructing it.
//
// No branch reaches confirmed without seal evidence: that is the property the
// table exists for (TestNoPhaseReachesConfirmedWithoutSealEvidence).
func PermitPhase(op coord.MaintenanceOperation, ev PhaseEvidence) (coord.MaintenancePhase, error) {
	if !op.Phase.Valid() {
		return "", &Telling{Say: fmt.Sprintf("the capacity operation on %s is in "+
			"phase %q, which this build doesn't know. A build that can't read a "+
			"phase must not move past it, because the phase it would skip is the "+
			"one carrying the evidence rule it doesn't have. Run it from a node "+
			"on the newer build that wrote it", op.Stream, op.Phase)}
	}
	switch op.Phase {
	case coord.PhaseOpened:
		if ev.BaselineWritten {
			return coord.PhaseBaselined, nil
		}
	case coord.PhaseBaselined:
		// Only a read-back equal to the target: an accepted request is
		// not an applied one.
		if ev.Observed && ev.ObservedMaxBytes == op.TargetMaxBytes {
			return coord.PhaseApplied, nil
		}
		// A request may be in flight from here and only the barrier
		// retires one, so the seal always follows. Before it, a
		// coordinator in maintenance mode may resume the window (record
		// who is running again, send its own request with its own journal
		// record), and this phase answers it unchanged (BaselineWritten).
		// Reaching the seal from here keeps an unresolved journal record
		// from blocking the seal that clears it.
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
// Two conditions, and no comparison:
//
//  1. Every participant not excluded has an acknowledgement the seal takes
//     ([SealRestarts]).
//  2. Every journal record for this operation is completed or retired, the
//     same attempt's included: two coordinators on one attempt is the schedule
//     the journal exists to catch, and an earlier-attempt test passes over it.
func SealHolds(op coord.MaintenanceOperation, acks []coord.MaintenanceAck) (bool, string) {
	_, missing := SealRestarts(op, acks)
	for _, record := range op.Journal {
		if record.State == coord.JournalIssued {
			// Named by when it was sent too: one attempt can send several
			// requests (a resumed window's), so attempt and sender alone
			// can name two records alike.
			missing = append(missing, fmt.Sprintf(
				"attempt %d's request sent at %s by %s is unresolved",
				record.Attempt, record.At.UTC().Format(time.RFC3339), record.By))
		}
	}
	if len(missing) > 0 {
		return false, strings.Join(missing, "; ")
	}
	return true, ""
}

// SealRestarts reads the acknowledgements as the barrier does: whether every
// participant not excluded has one the seal takes (this operation and attempt,
// seal mode, and an incarnation other than the one recorded as the newest
// request left: one is minted per process, so a different one proves that
// process gone), and which process each names. It is the one statement of that
// rule: the barrier retires the journal on it and [SealHolds] holds on it, and
// two rules would retire a record the seal then refuses, or the reverse.
//
// Restarted is each participant's acknowledging process when every one is
// taken, and nil when any is not. Missing says what is wrong with each one
// that is not, in participant order. Only this log's acknowledgements are read
// ([acksOf]).
func SealRestarts(op coord.MaintenanceOperation, acks []coord.MaintenanceAck) (
	restarted map[string]string, missing []string) {

	restarted, gaps := sealGaps(op, acks)
	for _, gap := range gaps {
		missing = append(missing, gap.says)
	}
	return restarted, missing
}

// SealOutstanding is every participant the seal still waits on, by
// [SealRestarts]' rule, sorted: what a window past `opened` waits on, and so
// what a page shows there ([BaselineMissing] is the list at `opened`).
//
// The seal's own rule, never a looser one: counting any acknowledgement of this
// attempt shows a node restarted into maintenance mode, or still the recorded
// process, as owing nothing while the seal waits on it.
func SealOutstanding(op coord.MaintenanceOperation, acks []coord.MaintenanceAck) []string {
	_, gaps := sealGaps(op, acks)
	out := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		out = append(out, gap.node)
	}
	slices.Sort(out)
	return out
}

// sealGap is one participant whose acknowledgement the seal does not take, and
// what is wrong with it, said as a sentence beginning with the node.
type sealGap struct {
	node, says string
}

// sealGaps is the rule [SealRestarts] and [SealOutstanding] read.
func sealGaps(op coord.MaintenanceOperation, acks []coord.MaintenanceAck) (
	map[string]string, []sealGap) {

	byNode := acksOf(op, acks)
	restarted := map[string]string{}
	var gaps []sealGap
	for _, node := range op.Participants {
		if slices.Contains(op.Waived(), node) {
			// An operator's assertion that this participant's process
			// is stopped and holds no outstanding request, or a
			// retirement that still stands as the window was read. An
			// eviction is not one; it is about whose records apply.
			continue
		}
		ack, acked := byNode[node]
		var says string
		switch {
		case !acked:
			says = node + " has not acknowledged"
		case ack.Windowless():
			// A restart made while no window was open binds nothing: a
			// baseline reads it, a seal never takes it.
			says = node + " restarted before this operation opened and has " +
				"not acknowledged it"
		case ack.OperationID != op.OperationID:
			says = node + " acknowledged another operation"
		case ack.Attempt != op.Attempt:
			says = fmt.Sprintf("%s acknowledged attempt %d and this is attempt %d",
				node, ack.Attempt, op.Attempt)
		case ack.Mode != string(ModeSeal):
			says = fmt.Sprintf("%s acknowledged from %q rather than seal mode",
				node, ack.Mode)
		case ack.Incarnation == op.WriteIncarnations[node]:
			says = node + " is still the process that was running when the " +
				"request left, so it hasn't restarted since"
		default:
			restarted[node] = ack.Incarnation
			continue
		}
		gaps = append(gaps, sealGap{node: node, says: says})
	}
	if len(gaps) > 0 {
		return nil, gaps
	}
	return restarted, nil
}

// BarrierEvidence is what the barrier observed: the process each participant
// was replaced by, and the ceiling.
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
// It runs before the seal predicate is evaluated, so the journal never gates
// the barrier that clears it. A value never retires a record: a ceiling equal
// to the target may be another request's while an older one is still in
// flight. What retires one is every participant restarting, a fact about
// processes, so the evidence recorded is the incarnations.
func Retire(op coord.MaintenanceOperation, ev BarrierEvidence) coord.MaintenanceOperation {
	out := op
	if out.Blocked.HoldsRequests() {
		// WHAT HELD THE NEXT REQUEST BACK IS RESOLVED OR JUDGED AGAIN: the
		// seal retires every outstanding request, and the next attempt's
		// request reads the fleet afresh, so its sentence would be stale.
		out.Blocked, out.Outgrown = "", 0
	}
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

// describeBarrier renders what the barrier saw, for the record: the ceiling
// first, then the incarnations, cut to [coord.MaxJournalEvidenceBytes]. A
// post-mortem reads it and nothing compares it; the seal's own log line
// names every incarnation whole.
func describeBarrier(ev BarrierEvidence) string {
	nodes := make([]string, 0, len(ev.Incarnations))
	for node, incarnation := range ev.Incarnations {
		nodes = append(nodes, node+"="+incarnation)
	}
	slices.Sort(nodes)
	return coord.FitText(fmt.Sprintf("ceiling %d observed with incarnations %s",
		ev.ObservedMaxBytes, strings.Join(nodes, ",")), coord.MaxJournalEvidenceBytes)
}

// Journaled is journal with record after it, kept to
// [coord.MaxMaintenanceJournal]: the oldest resolved records leave first,
// which the seal no longer reads (only an issued one holds it), and an issued
// record never does. False where the journal already holds that many issued
// records: the request record stands for is not to be sent ([coord.BlockJournalFull]).
func Journaled(journal []coord.JournalRecord, record coord.JournalRecord) ([]coord.JournalRecord, bool) {
	out := append(slices.Clone(journal), record)
	for len(out) > coord.MaxMaintenanceJournal {
		oldest := slices.IndexFunc(out, func(r coord.JournalRecord) bool { return r.State != coord.JournalIssued })
		if oldest < 0 {
			return journal, false
		}
		out = slices.Delete(out, oldest, oldest+1)
	}
	return out, true
}

// Verifies reports whether the verify after the seal confirms op at the
// ceiling observed: the target exactly, or, for an operation an operator
// abandoned ([coord.BlockAbandoned]), whatever the seal found. An abandoned
// operation no longer tries to reach its target; it crosses the seal only
// because a request it sent may still be in flight, and after that the ceiling
// the log has is the outcome, not a reason for another attempt.
func Verifies(op coord.MaintenanceOperation, observed uint64) bool {
	return observed == op.TargetMaxBytes || op.Blocked == coord.BlockAbandoned
}

// NextAttempt is what a failed verify produces: the SAME immutable target
// under a new attempt number.
//
// Not a re-apply: every node is in seal mode, which refuses configuration
// writes, and phases only advance, so nothing leads from a mismatch back to
// applying. In a new attempt maintenance stays active, the fleet restarts into
// the writable maintenance mode (never a write from seal mode, which the
// barrier depends on), a fresh baseline is persisted, the unchanged target is
// applied and a fresh seal follows. The attempt number invalidates prior
// acknowledgements rather than deleting them.
func NextAttempt(op coord.MaintenanceOperation, observed uint64,
	now time.Time) (coord.MaintenanceOperation, error) {

	if op.Attempt >= ControlRaiseAttempts {
		out := op
		out.ObservedMaxBytes = observed
		out.Blocked = coord.BlockUnsealed
		// The same sentence every surface shows for the block
		// ([BlockedDetail]), so the refusal and the page agree.
		return out, &Telling{Say: BlockedDetail(out, nil, nil)}
	}
	out := op
	out.Attempt++
	out.Phase = coord.PhaseOpened
	out.ObservedMaxBytes = observed
	// The baseline is dropped with the attempt so the next records its
	// own: a previous attempt's incarnation would be satisfied by a
	// restart that already happened.
	out.WriteIncarnations = nil
	out.Journal = slices.Clone(op.Journal)
	_ = now
	return out, nil
}

// BlockedDetail is why op can't go on without a person, in a person's words:
// what blocked it and what clears it, each command as the CLI takes it. Empty
// while unblocked. acks and acksErr are the window's acknowledgements as the
// caller read them, or why they couldn't be read. Every surface showing a
// block shows this beside the code (kept for scripts), held to
// [coord.MaintenanceBlocks] by a test; who abandoned it is printed beside it,
// never in it.
//
// An abandoned window returns to `sealing` from anywhere ([AbandonPhase]), so
// [SealOutstanding], not its phase, says whether the fleet must still restart;
// none outstanding means set-capacity closes it now. Out of attempts
// ([NextAttempt]), nothing sends the target again: abandoning closes it.
func BlockedDetail(op coord.MaintenanceOperation, acks []coord.MaintenanceAck,
	acksErr error) string {

	setCapacity := fmt.Sprintf("`crewlet retention set-capacity %s %d -confirm %d`",
		op.Stream, op.TargetMaxBytes, op.TargetMaxBytes)
	switch op.Blocked {
	case "":
		return ""
	case coord.BlockAbandoned:
		said := fmt.Sprintf("This window was abandoned, so it's no longer trying "+
			"to reach %s.", bytesHuman(int64(op.TargetMaxBytes)))
		if op.Phase == coord.PhaseSealed || op.Phase == coord.PhaseConfirmed {
			return said + fmt.Sprintf(" Run %s to close it at the ceiling the "+
				"log has.", setCapacity)
		}
		switch {
		case op.RetiredUnjudged:
			// NOT FROM AN EMPTY RETIRED, which would name a retired node
			// as owing a restart.
			return said + fmt.Sprintf(" It closes at the ceiling the log has once "+
				"its seal holds and %s runs again. This node couldn't judge which "+
				"participants are retired just now, so it can't say which nodes "+
				"its seal waits on: `crewlet retention maintenance status -stream "+
				"%s` says.", setCapacity, op.Stream)
		case acksErr != nil:
			return said + fmt.Sprintf(" It closes at the ceiling the log has once "+
				"its seal holds and %s runs again. This node couldn't read which "+
				"nodes its seal still waits on, so `crewlet retention "+
				"maintenance status -stream %s` says whether it holds yet.",
				setCapacity, op.Stream)
		}
		waiting := SealOutstanding(op, acks)
		if len(waiting) == 0 {
			return said + fmt.Sprintf(" Its seal is waiting on no node, so %s "+
				"closes it at the ceiling the log has.", setCapacity)
		}
		restarted := "has restarted"
		if len(waiting) > 1 {
			restarted = "have restarted"
		}
		return said + fmt.Sprintf(" It closes at the ceiling the log has once %s "+
			"%s with `crewlet run -mode seal` and %s runs again.",
			words.And(waiting), restarted, setCapacity)
	case coord.BlockUnsealed:
		target, observed := bytesHuman(int64(op.TargetMaxBytes)),
			bytesHuman(int64(op.ObservedMaxBytes))
		return fmt.Sprintf("Crewlet stopped after %d attempts at %s, because the "+
			"broker still reports %s, and it won't send the change again. To close "+
			"the window at %s, abandon it with `crewlet retention maintenance "+
			"abandon -stream %s -confirm %s`, then run %s from a node started with "+
			"`crewlet run -mode seal`. If the log still needs %s, find out why the "+
			"broker won't take it, then open a new window for it.",
			ControlRaiseAttempts, target, observed, observed, op.Stream,
			op.OperationID, setCapacity, target)
	case coord.BlockTooManyParticipants:
		return fmt.Sprintf("Crewlet didn't send this window's next request, because %d nodes "+
			"would now have to restart for its seal (every live member and data node, and every "+
			"node the fleet has a position for that isn't retired), and a window names at most %d. "+
			"Retire each node id that is gone for good with `crewlet retention maintenance retire "+
			"-node <node-id> -confirm <node-id>` (`crewlet retention status` shows which hold no "+
			"presence), then run %s again; or abandon the window with `crewlet retention "+
			"maintenance abandon -stream %s -confirm %s`.",
			op.Outgrown, coord.MaxMaintenanceParticipants, setCapacity, op.Stream, op.OperationID)
	case coord.BlockTooManyRecorded:
		over, room := op.RecordedOver()
		if over <= 0 {
			return fmt.Sprintf("Crewlet didn't send this window's next request, because it would have "+
				"recorded more than the %d participants a window keeps. Enough have been excluded "+
				"since that it no longer would: run %s again, which sends it.",
				coord.MaxMaintenanceRecorded, setCapacity)
		}
		said := fmt.Sprintf("Crewlet didn't send this window's next request, because it would record "+
			"%d participants and a window keeps at most %d: a participant stays recorded once a "+
			"request has named it, retired or not, so retiring a recorded one won't lower it.",
			op.Outgrown, coord.MaxMaintenanceRecorded)
		abandon := fmt.Sprintf("abandon the window with `crewlet retention maintenance abandon "+
			"-stream %s -confirm %s`", op.Stream, op.OperationID)
		if over > room {
			return said + fmt.Sprintf(" It would take %d exclusions to bring it within that, and "+
				"it has room for %d more, so %s.", over, max(room, 0), abandon)
		}
		return said + fmt.Sprintf(" Exclude at least %d retired participants (`crewlet retention "+
			"maintenance status -stream %s` prints them as retired) with `crewlet retention "+
			"maintenance exclude -stream %s -node <node-id> -confirm <node-id>`, then run %s again; "+
			"or %s.", over, op.Stream, op.Stream, setCapacity, abandon)
	case coord.BlockJournalFull:
		return fmt.Sprintf("Crewlet didn't send this window's next request, because %d earlier "+
			"requests are still unresolved and a window keeps at most %d. Restart every node with "+
			"`crewlet run -mode seal` so its seal retires them, then run %s again; or abandon the "+
			"window with `crewlet retention maintenance abandon -stream %s -confirm %s`.",
			coord.MaxMaintenanceJournal, coord.MaxMaintenanceJournal, setCapacity, op.Stream,
			op.OperationID)
	}
	return "It's blocked for a reason this build of Crewlet doesn't know. A node " +
		"running the newer build that blocked it can say why."
}

// AbandonPhase is where an operator's abandonment goes from here.
//
// From `opened` it clears outright: no request has been issued, so there is
// nothing to retire. From anywhere else it seals: abandoning changes what the
// operation is trying to reach, never the barrier it must cross, and a person
// reading a status page retires nobody's outstanding request.
func AbandonPhase(op coord.MaintenanceOperation) (coord.MaintenancePhase, bool, error) {
	if !op.Phase.Valid() {
		return "", false, &Telling{Say: fmt.Sprintf("the capacity operation on %s "+
			"is in phase %q, which this build doesn't know. Abandon it from a "+
			"node on the newer build that wrote it", op.Stream, op.Phase)}
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

// ResolveDelete is what an interrupted delete concluded, read rather than
// assumed: a conditional delete whose response was lost may have committed, or
// not. Reading "absent, or a different id" as "retry" would delete the next
// operation's record and release a fleet still in maintenance.
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

// BaselineMissing is every participant a window has not excluded with no
// restart into a maintenance mode recorded at all, bound to a window or to
// none: the nodes a baseline refuses, since it baselines each participant at
// the process its acknowledgement names. Sorted.
//
// It is what a window at `opened` waits on, beside the admissions
// ([AdmissionBlocks]), and so what a page shows there. The seal's list is wrong
// before the baseline: it names restarts into seal mode not yet asked for.
func BaselineMissing(op coord.MaintenanceOperation, acks []coord.MaintenanceAck) []string {
	restarted := acksOf(op, acks)
	var missing []string
	for _, node := range op.Participants {
		if _, recorded := restarted[node]; recorded || slices.Contains(op.Waived(), node) {
			continue
		}
		missing = append(missing, node)
	}
	slices.Sort(missing)
	return missing
}

// acksOf is the acknowledgements of op's own log, by node.
//
// A window is per log and two can be open at once, so another log's
// acknowledgement is no evidence for this one: neither a restart its baseline
// reads (that boot may have failed to read this window) nor a seal.
func acksOf(op coord.MaintenanceOperation, acks []coord.MaintenanceAck) map[string]coord.MaintenanceAck {
	byNode := make(map[string]coord.MaintenanceAck, len(acks))
	for _, ack := range acks {
		if ack.Stream == op.Stream {
			byNode[ack.NodeID] = ack
		}
	}
	return byNode
}

// AdmissionBlocks names every node whose admission stands in the way of an
// operation's first request: every admission but an excluded node's.
//
// A positive record from each side, each written before the other is read (a
// node writes its admission then reads the operation; a coordinator the
// reverse), so whichever lands second sees the first and either the node does
// not publish or no request is sent. Checking for an absent publisher, or
// reading admissions before writing the operation, is check-then-act. A window
// can be open while an admission blocks it (its process shown not running by
// presence; the coordinator closes a window opened over a running one); an
// excluded node's stops counting: the exclusion, an operator's word that the
// process is stopped, is recorded on the operation.
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
