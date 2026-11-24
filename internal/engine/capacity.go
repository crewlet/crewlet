package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// THE CAPACITY COORDINATOR drives [statelog.PermitPhase] against a real broker
// and fleet. It states no transitions of its own: it reaches every phase by
// offering evidence to the table, so the procedure has one executable copy.
//
// It runs in a MAINTENANCE-MODE node, because the verb needs a broker and no
// publisher (on the default topology the broker binds no socket). Which one is
// deliberately undecided: the operation RECORD is both the operation and the
// exclusion, so two callers contend on its compare-and-set create, and matching
// its immutable target IS the resume. A coordinator lease would be a second
// thing to hold, with its own expiry, over state that has none: whoever calls
// next with the same target picks up an interrupted drive.

// CapacityRequest is what an operator asked for.
type CapacityRequest struct {
	// Stream is which log's ceiling changes, and TargetMaxBytes what to.
	Stream         string
	TargetMaxBytes uint64

	// By names the operator (the AUTHOR, iam's ActorFor name) and OperatorID the
	// credential they acted through, kept apart because a machine token is swept a
	// week after it lapses, and `pat:<id>` alone would then name nobody.
	By         string
	OperatorID string

	// Assert is the operator's explicit statement that they have excluded
	// every publisher, required only on a broker the engine does not run.
	Assert bool
}

// SetCapacity drives one capacity operation as far as the current mode allows,
// and reports where it stands.
//
// The procedure needs three fleet-wide restarts and this call runs inside one:
// from MAINTENANCE mode it opens, baselines, applies and stops; from SEAL mode
// it collects the barrier, retires the journal, seals, verifies and confirms.
// Both advance the same table with whatever evidence the mode can produce.
func (e *Engine) SetCapacity(ctx context.Context, req CapacityRequest) (
	coord.MaintenanceOperation, error) {

	if e.mode.Publishes() {
		return coord.MaintenanceOperation{}, refused(
			"this node is publishing, so a capacity change can't run here. " +
				"Restart every node of the fleet with `-mode maintenance` " +
				"first: the usage a resize is decided against has to be a " +
				"quantity nothing can move, and what retires a request the " +
				"broker already queued is the broker process restarting")
	}
	s, err := e.stateLogOf()
	if e.backends == nil || e.backends.Fleet == nil || err != nil {
		return coord.MaintenanceOperation{}, &UnknownStreamError{Stream: req.Stream}
	}
	running := s.logOf(req.Stream)
	if running == nil {
		return coord.MaintenanceOperation{}, &UnknownStreamError{Stream: req.Stream,
			Streams: maintenanceStreams()}
	}
	if req.TargetMaxBytes == 0 {
		return coord.MaintenanceOperation{}, refused(
			"a capacity target of zero would be an unbounded log, which is a " +
				"real setting this verb doesn't set")
	}
	// A TARGET PAST int64 wraps negative on the wire, which is how JetStream
	// spells "unbounded": refused for zero's reason, naming the ceiling.
	if req.TargetMaxBytes > math.MaxInt64 {
		return coord.MaintenanceOperation{}, refused(
			"a capacity target of %d is past the %d bytes a stream's ceiling "+
				"can carry. It would be sent as a negative, which is how an "+
				"unbounded log is spelled, the one setting this verb doesn't set",
			req.TargetMaxBytes, int64(math.MaxInt64))
	}
	// AN EXTERNAL BROKER is one the engine cannot tell who else is connected to,
	// so only the operator's explicit assertion stands in for the exclusion.
	if e.dialsExternalBroker() && !req.Assert {
		return coord.MaintenanceOperation{}, refused(
			"this fleet dials an external NATS cluster, so stopping every " +
				"Crewlet node doesn't establish that nothing is publishing: the " +
				"engine doesn't run that broker and can't know who else holds a " +
				"connection. Pass the explicit assertion that you've excluded " +
				"every publisher, or run the change on the embedded topology " +
				"where the exclusion is structural")
	}

	// READ ONCE, HERE: the original ceiling is shown beside the target and is
	// what an abandon returns to.
	stats, err := running.log.Stats(ctx)
	if err != nil {
		return coord.MaintenanceOperation{}, fmt.Errorf(
			"engine: read %s's current ceiling: %w", req.Stream, err)
	}
	op, err := e.openCapacity(ctx, req, stats, e.growthRoom(ctx))
	if err != nil {
		return coord.MaintenanceOperation{}, err
	}
	return e.driveCapacity(ctx, running.log, op)
}

// capacityLog is the two calls a capacity window makes on the log it resizes:
// set its ceiling, and read its ceiling and usage back.
//
// Declared here, as its consumer, so the whole drive (request, seal, verify,
// release) runs against a log a test holds. A nil one is a drive in a mode or a
// phase that touches no log.
type capacityLog interface {
	SetMaxBytes(ctx context.Context, maxBytes uint64) error
	Stats(ctx context.Context) (jetstream.LogStats, error)
}

// growthRoom is how far this node's broker, the one holding the stream being
// resized ([Engine.capacityHost]), will let a log's ceiling grow, or unstated
// where it cannot be read (an engine with no core runtime has no such stream).
//
// UNREAD IS UNSTATED, and the window still opens: the check only spares an
// operator restarts spent learning a refusal late. The broker still refuses a
// raise it cannot reserve, while refusing on a failed read would block a raise
// it would grant.
func (e *Engine) growthRoom(ctx context.Context) jetstream.StorageBudget {
	unstated := jetstream.StorageBudget{Limit: -1, Source: jetstream.BudgetUnstated}
	host := e.capacityHost()
	if host == nil {
		return unstated
	}
	room, err := host.GrowthBudget(ctx)
	if err != nil {
		log.WarnContext(ctx, "capacity_growth_room_unread", "error", err.Error(),
			"detail", "a raise is not checked against the broker before the "+
				"window opens; the broker still refuses one it cannot reserve "+
				"when the window applies it")
		return unstated
	}
	return room
}

// capacityBounds is what a capacity target on the domain log named stream is
// held to: how its ceiling divides between ordinary writes and the gate
// reserve ([statelog.ReservationOf]), and the floor Tier A holds its ceiling
// field to, with the field's name. Both are properties of the DOMAIN, so read
// from the register rather than from a running log.
//
// THE FLOOR IS THE FIELD'S ([domainCeiling.Floor]), never one number for every
// log: Tier A takes the identity estate's and the usage log's down to 64 MiB
// ([config.IamLogMaxBytesFloor], [config.UsageLogMaxBytesFloor]).
func capacityBounds(stream string) (statelog.Reservation, domainCeiling, bool) {
	for _, domain := range registeredDomains() {
		if domain.Stream().Name != stream {
			continue
		}
		entry, found := registrationFor(domain.Name())
		if !found || entry.Ceiling == nil {
			return statelog.Reservation{}, domainCeiling{}, false
		}
		// The declaration does not depend on the field's value or the volume, so
		// an unset stream and no free bytes serve.
		return statelog.ReservationOf(domain), entry.Ceiling(config.Stream{}, 0), true
	}
	return statelog.Reservation{}, domainCeiling{}, false
}

// smallestTargetAbove is the smallest ceiling a log holding bytes can be given
// whose ordinary writes are not refused the moment it applies, and never below
// floor.
//
// SEARCHED rather than solved: the reserve is the larger of two rounded terms
// ([statelog.Reservation.Bytes]), so a closed form is off by a byte either way,
// and an operator is told to type the exact number. The ordinary ceiling never
// falls as the target grows, so bisection between bytes and bytes plus its own
// reserve finds it in sixty-odd steps.
func smallestTargetAbove(bytes uint64, reservation statelog.Reservation, floor uint64) uint64 {
	low := bytes
	high := bytes + 1 + reservation.Bytes(2*(bytes+1)) + statelog.GateRoom
	for reservation.Ordinary(high) <= bytes {
		high += high - bytes
	}
	for high-low > 1 {
		mid := low + (high-low)/2
		if reservation.Ordinary(mid) > bytes {
			high = mid
		} else {
			low = mid
		}
	}
	return max(high, floor)
}

// openCapacity takes the exclusion, or resumes the operation already holding
// it.
//
// THE ID IS MINTED ONCE and reused across create attempts: an unknown create
// may still be outstanding, so a fresh id could open a SECOND window.
//
// IT READS NO ADMISSION BEFORE THE RECORD EXISTS: the record is this side's
// half of the handshake ([Engine.admissionsClear]), so admissions are read only
// after it is written ([Engine.keepOpen]).
func (e *Engine) openCapacity(ctx context.Context, req CapacityRequest,
	current jetstream.LogStats, room jetstream.StorageBudget) (coord.MaintenanceOperation, error) {

	held, found, err := e.backends.Fleet.Maintenance(ctx, req.Stream)
	if err != nil {
		return coord.MaintenanceOperation{}, err
	}
	if found {
		if held.TargetMaxBytes != req.TargetMaxBytes {
			return coord.MaintenanceOperation{}, refused(
				"a capacity operation on %s is already open, targeting %d "+
					"bytes rather than %d. Its target is chosen once and never "+
					"changed: resolve it with `crewlet retention maintenance "+
					"status`, or abandon it, before choosing a different one",
				req.Stream, held.TargetMaxBytes, req.TargetMaxBytes)
		}
		// THE SAME TARGET is a resume, which is the ordinary path after
		// a restart into the next mode.
		return e.judgeWindow(ctx, held)
	}

	// A CEILING ALREADY AT THE TARGET opens no window, which would cost three
	// restarts to reach the starting number; the usual case is the verb retried
	// after its own window closed and its answer was lost.
	if req.TargetMaxBytes == current.MaxBytes {
		return coord.MaintenanceOperation{}, refused("%s's ceiling is already "+
			"%d bytes, so there's nothing to change and no window was opened",
			req.Stream, current.MaxBytes)
	}

	// A TARGET BELOW THE LOG'S OWN FLOOR is refused as Tier A refuses that
	// field's explicit value: below it the log refuses appends before anybody
	// could act. A resume is not asked, for the reason the usage below is not.
	reservation, field, known := capacityBounds(req.Stream)
	if !known {
		return coord.MaintenanceOperation{}, &UnknownStreamError{Stream: req.Stream,
			Streams: maintenanceStreams()}
	}
	if req.TargetMaxBytes < uint64(field.Floor) {
		return coord.MaintenanceOperation{}, refused(
			"a %d-byte ceiling is below the %d bytes %s's ceiling is floored "+
				"at, the floor Tier A enforces on an explicit %s. Choose a "+
				"target of at least %d bytes",
			req.TargetMaxBytes, field.Floor, req.Stream, field.Field, field.Floor)
	}

	// A TARGET WHOSE ORDINARY CEILING ([statelog.Reservation.Ordinary]) IS AT OR
	// BELOW WHAT THE LOG HOLDS is a full log the moment it applies: every ordinary
	// append and linearizable read refused fleet-wide, a log only an eviction can
	// reach. In this mode nothing moves the usage, so it is decided here, once,
	// before the exclusion is taken. A resume is not asked again: its target was
	// accepted at the open, and refusing now would strand a window whose request
	// may already be in flight.
	if ordinary := reservation.Ordinary(req.TargetMaxBytes); ordinary <= current.Bytes {
		held := ""
		if reservation.Kept {
			held = fmt.Sprintf(" (ordinary writes are held to %d of it; the top "+
				"%d is kept for the records that install or lift a gate)",
				ordinary, reservation.Bytes(req.TargetMaxBytes))
		}
		return coord.MaintenanceOperation{}, refused(
			"%s holds %d bytes, so a %d-byte ceiling%s would refuse every "+
				"ordinary append the moment it applied. Choose a target of at "+
				"least %d bytes, or let the trim release some of the log first",
			req.Stream, current.Bytes, req.TargetMaxBytes, held,
			smallestTargetAbove(current.Bytes, reservation, uint64(field.Floor)))
	}

	// AND A RAISE THE BROKER CANNOT RESERVE: the broker would refuse it at apply,
	// and a failed apply is an unknown only the seal retires, so the operator would
	// spend the window's restarts learning a number readable now. Only an exact
	// limit is held against the target ([jetstream.Queue.GrowthBudget]); an
	// unstated one leaves the broker to answer.
	if req.TargetMaxBytes > current.MaxBytes {
		grow := req.TargetMaxBytes - current.MaxBytes
		if left := room.Available(); left >= 0 && grow > uint64(left) {
			return coord.MaintenanceOperation{}, refused(
				"raising %s from %d to %d bytes reserves %d more, and "+
					"the broker has %d left to reserve (%d of its %d-byte limit "+
					"already reserved). Choose a target of at most %d bytes, or "+
					"give the broker more room first",
				req.Stream, current.MaxBytes, req.TargetMaxBytes, grow, left,
				room.Committed, room.Limit, current.MaxBytes+uint64(left))
		}
	}

	fleet, err := e.capacityParticipants(ctx)
	if err != nil {
		return coord.MaintenanceOperation{}, err
	}
	participants := fleet.participants
	// A FLEET PAST WHAT ONE WINDOW NAMES opens none: its record would be no
	// small value, which every data node's boot reads at the term.
	if len(participants) > coord.MaxMaintenanceParticipants {
		log.WarnContext(ctx, "capacity_fleet_too_large", "stream", req.Stream,
			"participants", participants, "limit", coord.MaxMaintenanceParticipants)
		return coord.MaintenanceOperation{}, refused("%s, so no window was opened",
			fleet.now.outgrown(req.Stream, participants))
	}
	op := coord.MaintenanceOperation{
		Stream:           req.Stream,
		OperationID:      config.NewIncarnation("capacity"),
		TargetMaxBytes:   req.TargetMaxBytes,
		OriginalMaxBytes: current.MaxBytes,
		Phase:            coord.PhaseOpened,
		Attempt:          1,
		Participants:     participants,
		EnteredAt:        time.Now().UTC(),
		By:               req.By,
		OperatorID:       req.OperatorID,
	}
	// RETRIED WITH THE SAME ID, bounded, because an absent read after an
	// unknown create establishes nothing at all.
	var last error
	for attempt := range 3 {
		if attempt > 0 {
			sleep(ctx, time.Duration(attempt)*250*time.Millisecond)
		}
		held, won, err := e.backends.Fleet.OpenMaintenance(ctx, op)
		switch {
		case err != nil:
			last = err
		case won:
			log.InfoContext(ctx, "capacity_operation_opened",
				"stream", op.Stream, "operation", op.OperationID,
				"target", op.TargetMaxBytes, "was", op.OriginalMaxBytes,
				"participants", op.Participants, "by", op.By,
				"operator", op.OperatorID)
			return e.keepOpen(ctx, held)
		case held.OperationID == op.OperationID:
			// AN EARLIER ATTEMPT OF MINE WON and its answer was lost.
			return e.keepOpen(ctx, held)
		default:
			return coord.MaintenanceOperation{}, refused(
				"another capacity operation (%s, opened by %s) holds %s",
				held.OperationID, held.By, req.Stream)
		}
	}
	return coord.MaintenanceOperation{}, stopped(last,
		"couldn't establish whether the capacity window on %s was opened: the "+
			"create may never have been received and may still be outstanding, "+
			"so this fleet may already be excluded. Read `crewlet retention "+
			"maintenance status -stream %s` before trying again", req.Stream,
		req.Stream)
}

// driveCapacity advances the operation as far as this mode's evidence allows.
func (e *Engine) driveCapacity(ctx context.Context, resized capacityLog,
	op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {

	for range len(coord.MaintenancePhases) + 1 {
		next, err := e.advanceCapacity(ctx, resized, op)
		if err != nil {
			// THE WINDOW AS THE STEP LEFT IT, when the step wrote before refusing,
			// so the caller shows it beside the refusal rather than a stale one.
			if next.OperationID != "" {
				return next, err
			}
			return op, err
		}
		if next.Phase == op.Phase && next.Revision == op.Revision {
			return next, nil
		}
		op = next
	}
	// AN INVARIANT BREACH, NOT A REFUSAL: every transition the table answers
	// moves to a later phase, so this is returned as a plain error, which the
	// route answers as a fault rather than a 409 in the table's terms.
	return op, fmt.Errorf("engine: the capacity operation on %s didn't settle "+
		"in %d transitions, more than the table has phases",
		op.Stream, len(coord.MaintenancePhases)+1)
}

// advanceCapacity takes one step: gather what this mode can establish, ask the
// table, and persist whatever it answered.
func (e *Engine) advanceCapacity(ctx context.Context, resized capacityLog,
	op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {

	switch op.Phase {
	case coord.PhaseOpened:
		if !e.mode.WritesConfiguration() {
			return op, nil
		}
		return e.request(ctx, resized, op)
	case coord.PhaseBaselined:
		if e.mode.WritesConfiguration() {
			return e.request(ctx, resized, op)
		}
		return e.seal(ctx, resized, op)
	case coord.PhaseApplied:
		if e.mode.WritesConfiguration() {
			// APPLIED IS NOT DONE and this mode cannot finish it: the
			// seal is established by processes that cannot write
			// configuration, which is this mode's own capability.
			return op, nil
		}
		return e.seal(ctx, resized, op)
	case coord.PhaseSealing:
		return e.seal(ctx, resized, op)
	case coord.PhaseSealed:
		return e.verify(ctx, resized, op)
	case coord.PhaseConfirmed:
		return e.release(ctx, op)
	}
	return op, refused("the capacity operation on %s is in phase %q, which "+
		"this build doesn't know. Drive it from a node on the newer build that "+
		"wrote it", op.Stream, op.Phase)
}

// request records who is running and sends the window's configuration request:
// [Engine.recordRequest], then [Engine.apply], in one step and never apart.
//
// The seal's evidence is that the process each participant ran when a request
// left is gone ([statelog.SealRestarts]), which holds only if the processes it
// compares against are the ones running as the request left. Recorded at the
// baseline and sent later, a node restarted into seal mode in between would
// pass the seal on a restart older than the request. So every request, first
// or resumed, is sent by the step that has just recorded who is running, with
// its own journal record.
func (e *Engine) request(ctx context.Context, resized capacityLog,
	op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {

	recorded, record, err := e.recordRequest(ctx, op)
	if err != nil {
		return op, err
	}
	return e.apply(ctx, resized, recorded, record)
}

// recordRequest persists the participant set, each participant's incarnation
// and a journal record for the request about to leave, BEFORE it is issued
// (from `opened` the baseline, from `baselined` a resumed request): recorded
// after, a crash would leave a write with nothing to compare against. At
// `opened` it reads the admissions ([Engine.admissionsClear]): the first step
// with the window's record written and the last before a request. No later
// step reads one: a node booting in service finds the window and withdraws.
//
// PARTICIPANTS ARE THE RECORDED SET UNION THE FLEET NOW: a broker member that
// came up while the window waited holds the request too and, left out, could
// apply it after confirmation. Never narrowed: a node gone silent is a process
// nobody showed stopped; only an exclusion waives one.
func (e *Engine) recordRequest(ctx context.Context, op coord.MaintenanceOperation) (
	coord.MaintenanceOperation, coord.JournalRecord, error) {

	if op.Phase == coord.PhaseOpened {
		if err := e.admissionsClear(ctx, op); err != nil {
			return op, coord.JournalRecord{}, err
		}
	}
	fleet, err := e.capacityParticipants(ctx)
	if err != nil {
		return op, coord.JournalRecord{}, err
	}
	widened := withFleet(op, fleet)
	if counted := widened.Counted(); len(counted) > coord.MaxMaintenanceParticipants ||
		len(widened.Participants) > coord.MaxMaintenanceRecorded {
		return e.blockRequest(ctx, op, fleet.now, widened)
	}
	if lapsed := slices.DeleteFunc(cloneStrings(op.Excluded), func(n string) bool {
		return contains(widened.Excluded, n)
	}); len(lapsed) > 0 {
		log.WarnContext(ctx, "capacity_exclusion_lapsed", "stream", op.Stream,
			"operation", op.OperationID, "nodes", lapsed, "detail", "an excluded node is "+
				"running as this request leaves, so the window records it and its seal waits "+
				"for it to restart")
	}
	incarnations, participants, err := e.capacityIncarnations(ctx, widened, fleet.now)
	if err != nil {
		return op, coord.JournalRecord{}, err
	}
	record := coord.JournalRecord{
		Attempt: op.Attempt, By: e.incarnation,
		State: coord.JournalIssued, At: time.Now().UTC(),
	}
	journal, kept := statelog.Journaled(op.Journal, record)
	if !kept {
		return e.blockRequest(ctx, op, fleetNow{}, coord.MaintenanceOperation{})
	}
	phase, err := statelog.PermitPhase(op, statelog.PhaseEvidence{
		Now: record.At, BaselineWritten: true,
	})
	if err != nil {
		return op, coord.JournalRecord{}, err
	}
	next := widened
	next.Phase = phase
	next.Participants = participants
	next.WriteIncarnations = incarnations
	next.Journal = journal
	if op.Blocked.HoldsRequests() {
		// WHAT HELD IT HAS CLEARED: the fleet is back within the bound, or a
		// seal retired the outstanding requests.
		next.Blocked, next.Outgrown = "", 0
	}
	if err = e.backends.Fleet.UpdateMaintenance(ctx, next); err != nil {
		return op, coord.JournalRecord{}, err
	}
	log.InfoContext(ctx, "capacity_request_recorded", "stream", op.Stream,
		"operation", op.OperationID, "attempt", op.Attempt, "was", op.Phase,
		"participants", participants, "incarnations", incarnations)
	recorded, err := e.reread(ctx, op.Stream)
	return recorded, record, err
}

// withFleet is op taking part as the fleet now stands: its participants are
// the ones it recorded and the fleet's now, each once, less every node an
// operator excluded, with Retired judged afresh. Never otherwise narrowed: a
// node gone silent is a process nobody showed stopped, and a retired one is
// waived only while its retirement stands.
//
// AN EXCLUSION LAPSES FOR A NODE RUNNING AS THE REQUEST LEAVES ([fleetNow.
// running]), this node first: the request can reach that process, so the seal
// must wait for it to restart, and the operator's word that it was stopped is
// no longer true. Kept, the window sent a request it sealed against nobody.
func withFleet(op coord.MaintenanceOperation, fleet capacityFleet) coord.MaintenanceOperation {
	out := op
	out.Excluded, out.ExcludedBy = nil, nil
	var lapsed []string
	for _, node := range op.Excluded {
		if fleet.now.running(node) {
			lapsed = append(lapsed, node)
			continue
		}
		out.Excluded = append(out.Excluded, node)
		if party, ok := op.ExcludedBy[node]; ok {
			if out.ExcludedBy == nil {
				out.ExcludedBy = map[string]coord.MaintenanceParty{}
			}
			out.ExcludedBy[node] = party
		}
	}
	everyone := map[string]bool{}
	for _, node := range slices.Concat(op.Participants, fleet.participants, lapsed) {
		if !contains(out.Excluded, node) {
			everyone[node] = true
		}
	}
	out.Participants = sortedKeys(everyone)
	return fleet.now.judged(out)
}

// blockRequest leaves op blocked for a person rather than send its next
// request: [coord.BlockTooManyParticipants] where widened, op as fleet would
// take it part, counts past what a window may, [coord.BlockTooManyRecorded]
// where it counts within it and records past what a window keeps, else
// [coord.BlockJournalFull] (no widened). Written, logged as an error, and
// refused in a sentence naming what to do with each node. A window blocked
// for one is driven again by the next set-capacity, which sends once what
// blocked it has cleared.
func (e *Engine) blockRequest(ctx context.Context, op coord.MaintenanceOperation,
	fleet fleetNow, widened coord.MaintenanceOperation) (coord.MaintenanceOperation, coord.JournalRecord, error) {

	block, counted, outgrown := coord.BlockJournalFull, widened.Counted(), 0
	switch {
	case widened.OperationID == "":
	case len(counted) > coord.MaxMaintenanceParticipants:
		block, outgrown = coord.BlockTooManyParticipants, len(counted)
	default:
		block, outgrown = coord.BlockTooManyRecorded, len(widened.Participants)
	}
	if op.Blocked == coord.BlockAbandoned {
		// AN ABANDONED WINDOW SENDS NOTHING, and keeps its operator's block.
		return op, coord.JournalRecord{}, refused("%s", statelog.BlockedDetail(op, nil, nil))
	}
	next := op
	next.Blocked, next.Outgrown = block, outgrown
	if next.Blocked != op.Blocked || next.Outgrown != op.Outgrown {
		if err := e.backends.Fleet.UpdateMaintenance(ctx, next); err != nil {
			return op, coord.JournalRecord{}, err
		}
	}
	said := statelog.BlockedDetail(next, nil, nil)
	switch block {
	case coord.BlockTooManyParticipants:
		// THE NODES BY NAME, each with what to do, which the block's own
		// sentence, a fact of the record alone, cannot say.
		said = "Crewlet didn't send this window's next request: " + fleet.outgrown(op.Stream, counted) + "."
	case coord.BlockTooManyRecorded:
		said = recordedPast(next, widened)
	}
	log.ErrorContext(ctx, "capacity_request_blocked", "stream", op.Stream,
		"operation", op.OperationID, "block", string(block), "participants", widened.Participants,
		"detail", said)
	reread, err := e.reread(ctx, op.Stream)
	if err != nil {
		return next, coord.JournalRecord{}, err
	}
	return reread, coord.JournalRecord{}, refused("%s", said)
}

// keepOpen decides whether a window this node just opened may stand, now its
// record is written and the admissions can be read against it (the handshake's
// order is [Engine.admissionsClear]'s).
//
// A RUNNING PUBLISHER CLOSES IT ([Engine.judgeAdmissions]): left open, the
// window would take every node booting in service out of service while the
// fleet kept writing beneath it. It closes at the revision it opened at, which
// sends nothing, and the verb is refused; an unreadable presence or admission
// closes it too. A STOPPED ONE KEEPS IT OPEN at `opened`, the case the window
// exists to get past, until the node's restart into maintenance mode or an
// exclusion retires the claim. A close that does not land leaves the window
// standing, and the refusal says how to close it.
func (e *Engine) keepOpen(ctx context.Context, op coord.MaintenanceOperation) (
	coord.MaintenanceOperation, error) {

	standing, err := e.judgeAdmissions(ctx, op)
	if err == nil && len(standing.running) == 0 {
		return op, nil
	}
	if closeErr := e.backends.Fleet.CloseMaintenance(ctx, op.Stream,
		op.OperationID, op.Revision); closeErr != nil {
		log.WarnContext(ctx, "capacity_operation_not_closed", "stream", op.Stream,
			"operation", op.OperationID, "running", standing.running,
			"err", closeErr, "judged", err,
			"detail", "the window was opened while a node may be publishing and "+
				"could not be closed again, so it stands and refuses every boot "+
				"in service until somebody abandons it")
		return op, stopped(closeErr, "the capacity window on %s was opened while "+
			"a node may still be publishing, and closing it again failed, so it's "+
			"still open and refuses every node that starts in service. Close it "+
			"with `crewlet retention maintenance abandon -stream %s -confirm %s`",
			op.Stream, op.Stream, op.OperationID)
	}
	log.InfoContext(ctx, "capacity_operation_closed", "stream", op.Stream,
		"operation", op.OperationID, "running", standing.running, "err", err,
		"detail", "a node holding an admission may be publishing, so the window "+
			"was closed again before anything was sent")
	if err != nil {
		return coord.MaintenanceOperation{}, stopped(err, "couldn't tell whether "+
			"the nodes holding an admission are running, so no window was opened "+
			"on %s and nothing was sent. Run this again in a moment", op.Stream)
	}
	return coord.MaintenanceOperation{}, refused("%s, so no window was opened on "+
		"%s and nothing was sent. Restart %s with `-mode maintenance`, then run "+
		"this again", runningAdmissions(standing.running), op.Stream,
		eachOrIt(standing.running))
}

// admissionStanding is what the fleet's presence says of the processes the
// admissions a window has not excluded name ([Engine.judgeAdmissions]).
type admissionStanding struct {
	// running is every node whose admission names the process that holds
	// the node's presence: a process that is running, and may be
	// publishing.
	running []string
	// silent is every node whose admission names a process nobody can see
	// running (nobody holds the node's presence): stopped without giving it
	// back, or still starting. Only a restart of the node or an operator's
	// word can retire it.
	silent []string
}

// judgeAdmissions reads the admissions a window has not excluded against who
// holds each node's presence now ([Engine.presenceHolders]), the evidence
// [Engine.supersede] reads at boot.
//
// AN ADMISSION WHOSE NODE ANOTHER PROCESS NOW HOLDS IS WITHDRAWN HERE, on the
// incarnation it names so a newer claim is never touched: a node restarted
// inside its crashed process's lease TTL saw the old process still present at
// boot and left its claim, and nothing else would come back for it.
//
// UNREADABLE IS AN ERROR: an unread admission may be a publisher's, and an
// unread presence makes every process look stopped.
func (e *Engine) judgeAdmissions(ctx context.Context, op coord.MaintenanceOperation) (
	admissionStanding, error) {

	admissions, err := e.backends.Fleet.Admissions(ctx)
	if err != nil {
		return admissionStanding{}, fmt.Errorf("read the admissions: %w", err)
	}
	owners, err := e.presenceHolders(ctx)
	if err != nil {
		return admissionStanding{}, fmt.Errorf("read which process holds each "+
			"node's presence: %w", err)
	}
	byNode := make(map[string]coord.Admission, len(admissions))
	for _, a := range admissions {
		byNode[a.NodeID] = a
	}
	var out admissionStanding
	for _, node := range statelog.AdmissionBlocks(admissions, op.Waived()) {
		held, owner := byNode[node], owners[node]
		switch {
		case owner == held.Incarnation:
			out.running = append(out.running, node)
		case owner == "":
			out.silent = append(out.silent, node)
		default:
			if err := e.backends.Fleet.ForgetAdmission(ctx, node, held.Incarnation); err != nil {
				return admissionStanding{}, fmt.Errorf("withdraw the admission "+
					"%s's earlier process left: %w", node, err)
			}
			log.InfoContext(ctx, "admission_superseded", "node", node,
				"previous", held.Incarnation, "present", owner,
				"stream", op.Stream, "operation", op.OperationID,
				"detail", "another process of this node holds its presence, so "+
					"the process its admission named is gone, and the window "+
					"withdrew the admission")
		}
	}
	return out, nil
}

// admissionsClear refuses while any node the window has not excluded holds an
// admission: a process that may be publishing.
//
// READ WITH THE WINDOW'S RECORD ALREADY WRITTEN: a node writes its admission
// then reads the window ([Engine.admit]); the coordinator writes the window then
// reads the admissions ([Engine.keepOpen], and here), so whichever write lands
// second, its writer's read sees the other's. The reverse is a check-then-act.
//
// AND IT LEAVES THE WINDOW OPEN: `opened` has issued nothing, and only an open
// window lets a restart ([Engine.supersede]) or [Engine.ExcludeParticipant]
// retire a dead node's admission. It refuses every boot in service meanwhile,
// so the refusal names the abandonment.
func (e *Engine) admissionsClear(ctx context.Context, op coord.MaintenanceOperation) error {
	standing, err := e.judgeAdmissions(ctx, op)
	if err != nil {
		return stopped(err, "couldn't read which nodes hold an admission, so "+
			"nothing was sent: one nobody can read may be a publisher's. Run this "+
			"again in a moment")
	}
	if len(standing.running) == 0 && len(standing.silent) == 0 {
		return nil
	}
	var said []string
	if len(standing.running) > 0 {
		said = append(said, fmt.Sprintf("%s, so %s may be publishing: restart %s "+
			"with `-mode maintenance`", runningAdmissions(standing.running),
			itOrThey(standing.running), eachOrIt(standing.running)))
	}
	if len(standing.silent) > 0 {
		exclude := "<node-id>"
		if len(standing.silent) == 1 {
			exclude = standing.silent[0]
		}
		said = append(said, fmt.Sprintf("%s and no process of %s is running: "+
			"restart %s with `-mode maintenance`, or if %s gone for good, record "+
			"that with `crewlet retention maintenance exclude -stream %s -node %s "+
			"-confirm %s`", holdAdmission(standing.silent), itOrThem(standing.silent),
			eachOrIt(standing.silent), oneIsOrItIs(standing.silent), op.Stream,
			exclude, exclude))
	}
	return refused("%s. Nothing has been sent. The window stays open at opened "+
		"meanwhile and refuses every node that starts in service; `crewlet "+
		"retention maintenance abandon -stream %s -confirm %s` closes it",
		strings.Join(said, ". And "), op.Stream, op.OperationID)
}

// runningAdmissions, holdAdmission and the rest say a set of nodes as the
// subject or object of a sentence, in the number the set has: one node is
// "it", never "these nodes".
func runningAdmissions(nodes []string) string {
	if len(nodes) == 1 {
		return words.And(nodes) + " is running and holds an admission"
	}
	return words.And(nodes) + " are running and hold an admission"
}

func holdAdmission(nodes []string) string {
	if len(nodes) == 1 {
		return words.And(nodes) + " holds an admission"
	}
	return words.And(nodes) + " hold an admission"
}

func itOrThey(nodes []string) string {
	if len(nodes) == 1 {
		return "it"
	}
	return "they"
}

func itOrThem(nodes []string) string {
	if len(nodes) == 1 {
		return "it"
	}
	return "them"
}

func eachOrIt(nodes []string) string {
	if len(nodes) == 1 {
		return "it"
	}
	return "each"
}

func oneIsOrItIs(nodes []string) string {
	if len(nodes) == 1 {
		return "it's"
	}
	return "one is"
}

// apply issues the configuration request and reads the ceiling back.
//
// THE READ-BACK IS THE EVIDENCE, never the acknowledgement: what the phase
// establishes is that the ceiling IS the target, which is a value the broker
// reports rather than a request it accepted.
func (e *Engine) apply(ctx context.Context, resized capacityLog,
	op coord.MaintenanceOperation, record coord.JournalRecord) (
	coord.MaintenanceOperation, error) {

	if err := resized.SetMaxBytes(ctx, op.TargetMaxBytes); err != nil {
		return op, e.applyFailure(ctx, op, err)
	}
	stats, err := resized.Stats(ctx)
	if err != nil {
		return op, fmt.Errorf("engine: read %s's ceiling back: %w", op.Stream, err)
	}
	next, err := statelog.PermitPhase(op, statelog.PhaseEvidence{
		Now: time.Now().UTC(), Observed: true, ObservedMaxBytes: stats.MaxBytes,
	})
	if err != nil {
		return op, err
	}
	out := op
	out.Phase = next
	out.ObservedMaxBytes = stats.MaxBytes
	if next == coord.PhaseApplied {
		out.Journal = completeJournal(op.Journal, record)
	}
	if err = e.backends.Fleet.UpdateMaintenance(ctx, out); err != nil {
		return op, err
	}
	log.InfoContext(ctx, "capacity_applied", "stream", op.Stream,
		"operation", op.OperationID, "attempt", op.Attempt,
		"target", op.TargetMaxBytes, "observed", stats.MaxBytes, "phase", next)
	recorded, err := e.reread(ctx, op.Stream)
	if err != nil || next == coord.PhaseApplied {
		return recorded, err
	}
	// A READ-BACK THAT ISN'T THE TARGET ENDS THE DRIVE, its request left issued
	// for the seal: the window stays at `baselined`, which in this mode sends a
	// request, so each further pass would send another. One drive sends one
	// request; the seal retires it and checks the ceiling again.
	return recorded, refused("%s's ceiling reads back as %d bytes, not the %d "+
		"this window asked for, and its request may still land, so nothing more "+
		"is sent from here. Restart every node with `crewlet run -mode seal`, "+
		"then run `crewlet retention set-capacity %s %d -confirm %d` again: the "+
		"seal checks the ceiling again", op.Stream, stats.MaxBytes,
		op.TargetMaxBytes, op.Stream, op.TargetMaxBytes, op.TargetMaxBytes)
}

// applyFailure is what an operator is told when the configuration request did
// not succeed, and the two answers are not the same fact.
//
// A REFUSAL ([jetstream.ErrInsufficientStorage]) IS AN ANSWER: the broker
// declined to propose the ceiling, so nothing is in flight, and the number
// that was wrong is named. EVERYTHING ELSE IS AN UNKNOWN: a transport error
// may still have landed, and the barrier resolves it.
//
// The journal record stays `issued` on both, so even a refusal says abandon:
// the record cannot tell this attempt's refusal from an earlier attempt's
// outstanding request, and abandoning past `opened` still crosses the barrier.
func (e *Engine) applyFailure(ctx context.Context, op coord.MaintenanceOperation,
	err error) error {

	if !errors.Is(err, jetstream.ErrInsufficientStorage) {
		return stopped(err, "setting %s's ceiling to %d: the outcome is unknown "+
			"and the operation stays open. Restart the fleet with `-mode seal` to "+
			"retire it", op.Stream, op.TargetMaxBytes)
	}
	// THE ABANDONMENT NAMED IN FULL, with its -confirm: the node refuses one
	// that doesn't repeat the operation's id.
	return stopped(err, "the broker refused to set %s's ceiling to %d and "+
		"wrote nothing%s. A target is chosen once for the life of an operation, "+
		"so abandon this one with `crewlet retention maintenance abandon "+
		"-stream %s -confirm %s` and open another at a ceiling that fits",
		op.Stream, op.TargetMaxBytes, e.capacityRefusal(ctx, op.TargetMaxBytes),
		op.Stream, op.OperationID)
}

// streamVolume is the directory the state logs' ceilings were derived from,
// which [limitSource] names where that volume is what bounds the broker. Empty
// on an engine with no core runtime, where no such limit can be the answer
// either.
func (e *Engine) streamVolume() string {
	s, err := e.stateLogOf()
	if err != nil {
		return ""
	}
	return s.volume
}

// capacityHost is the broker a capacity question is put to, or nil on an
// engine that runs no core runtime.
//
// ASSERTED OFF THE ENGINE'S OWN QUEUE on every call, never cached: a copy
// needs a writer to set it, and one missed silently turns the preflight off.
// The state log decides WHETHER to ask (a node with none has no stream to
// resize), and behind that guard the assertion cannot fail, since
// startStateLog refuses a broker that does not satisfy domainHost.
func (e *Engine) capacityHost() domainHost {
	if _, err := e.stateLogOf(); err != nil || e.backends == nil {
		return nil
	}
	host, _ := e.backends.Queue.(domainHost)
	return host
}

// capacityRefusal is what the broker had left to grant when it refused the
// raise, or nothing where this node cannot ask.
//
// THE GROWTH BUDGET, not the create budget: an update is checked against the
// metadata leader rather than placement ([jetstream.Queue.GrowthBudget]). Read
// AT the refusal, because an operation spans restarts and what matters is what
// the broker had when it said no. EMPTY where nothing was read or the limit is
// unstated: the refusal already carries the broker's own error.
func (e *Engine) capacityRefusal(ctx context.Context, want uint64) string {
	room := e.growthRoom(ctx)
	if room.Limit < 0 {
		return ""
	}
	// THE PROVISIONING REFUSAL'S SENTENCE ([roomLeft]) over the budget just
	// read: one wording of the rule, about the moment it explains.
	return fmt.Sprintf(". It asked to reserve %d bytes, and %s",
		want, roomLeft(room, nil, e.streamVolume()))
}

// seal collects the barrier, retires the journal against it, and asks the
// table whether the seal holds.
//
// RETIREMENT RUNS BEFORE THE PREDICATE, which breaks the cycle a
// journal-as-veto makes: the barrier needs nothing from the journal, so the
// seal is tested against a journal with nothing unresolved.
func (e *Engine) seal(ctx context.Context, resized capacityLog,
	op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {

	if e.mode != statelog.ModeSeal {
		// THIS MODE CANNOT ESTABLISH THE BARRIER, which is the answer rather than
		// an error: the operator's next step is the second restart.
		return op, nil
	}
	// JUDGED HERE, as the seal decides: a retired participant running again
	// since the window was read is waited for like any other.
	op, err := e.judgeWindow(ctx, op)
	if err != nil {
		return op, err
	}
	acks, err := e.backends.Fleet.MaintenanceAcks(ctx, op.Stream)
	if err != nil {
		return op, fmt.Errorf("engine: read the maintenance acknowledgements: %w", err)
	}
	out := op
	out.Phase = coord.PhaseSealing

	stats, err := resized.Stats(ctx)
	if err != nil {
		return op, fmt.Errorf("engine: read %s's ceiling: %w", op.Stream, err)
	}
	retired := false
	if incarnations, missing := statelog.SealRestarts(op, acks); len(missing) == 0 {
		out = statelog.Retire(out, statelog.BarrierEvidence{
			Incarnations: incarnations, ObservedMaxBytes: stats.MaxBytes,
			At: time.Now().UTC(),
		})
		retired = slices.ContainsFunc(op.Journal, func(r coord.JournalRecord) bool {
			return r.State == coord.JournalIssued
		})
	}
	out.ObservedMaxBytes = stats.MaxBytes

	next, err := statelog.PermitPhase(out, statelog.PhaseEvidence{
		Now: time.Now().UTC(), Stopped: true, Acks: acks,
	})
	if err != nil {
		return op, err
	}
	out.Phase = next
	if held, missing := statelog.SealHolds(out, acks); !held {
		log.InfoContext(ctx, "capacity_seal_pending", "stream", op.Stream,
			"operation", op.OperationID, "attempt", op.Attempt, "missing", missing)
	}
	// A PASS THAT MOVED NOTHING WRITES NOTHING: the drive takes an unchanged
	// window as settled, and a seal still waiting on a node is a normal answer,
	// not one that "didn't settle".
	if out.Phase == op.Phase && !retired && out.ObservedMaxBytes == op.ObservedMaxBytes &&
		out.Blocked == op.Blocked {
		return op, nil
	}
	if err := e.backends.Fleet.UpdateMaintenance(ctx, out); err != nil {
		return op, err
	}
	return e.reread(ctx, op.Stream)
}

// verify compares the observed ceiling against the immutable target, or, for a
// window an operator abandoned, records it as the outcome ([statelog.Verifies]).
//
// A MISMATCH IS A NUMBERED ATTEMPT rather than a re-apply: every node is in
// seal mode, which refuses configuration writes, so there is nowhere legal for
// a re-apply to run.
func (e *Engine) verify(ctx context.Context, resized capacityLog,
	op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {

	stats, err := resized.Stats(ctx)
	if err != nil {
		return op, fmt.Errorf("engine: read %s's ceiling to verify: %w", op.Stream, err)
	}
	if !statelog.Verifies(op, stats.MaxBytes) {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		next, err := statelog.NextAttempt(op, stats.MaxBytes, time.Now().UTC())
		if writeErr := e.backends.Fleet.UpdateMaintenance(ctx, next); writeErr != nil {
			return op, writeErr
		}
		if err != nil {
			return next, err
		}
		log.WarnContext(ctx, "capacity_attempt_retried", "stream", op.Stream,
			"operation", op.OperationID, "attempt", next.Attempt,
			"target", op.TargetMaxBytes, "observed", stats.MaxBytes,
			"detail", "restart the fleet with `-mode maintenance` to apply the "+
				"same target again")
		return e.reread(ctx, op.Stream)
	}
	out := op
	out.ObservedMaxBytes = stats.MaxBytes
	next, err := statelog.PermitPhase(out, statelog.PhaseEvidence{
		Now: time.Now().UTC(), Verified: true,
	})
	if err != nil {
		return op, err
	}
	out.Phase = next
	if err := e.backends.Fleet.UpdateMaintenance(ctx, out); err != nil {
		return op, err
	}
	return e.reread(ctx, op.Stream)
}

// release deletes the record, which is the one write that ends the exclusion.
//
// A DELETE THAT LOST ITS RACE IS NOT A RELEASE: the delete is conditional on
// the revision this drive read, so a window changed since (an exclusion, an
// abandonment, another coordinator) refuses it with [coord.ErrMaintenanceMoved].
// That is resolved by reading, like a lost answer: gone or another operation's
// is released; still this one is handed back at its current revision for the
// drive to decide again.
//
// A WINDOW THIS DRIVE ALREADY RELEASED HAS NO REVISION, and is not deleted
// again.
func (e *Engine) release(ctx context.Context, op coord.MaintenanceOperation) (
	coord.MaintenanceOperation, error) {

	if op.Revision == 0 {
		return op, nil
	}
	err := e.backends.Fleet.CloseMaintenance(ctx, op.Stream, op.OperationID, op.Revision)
	if err != nil {
		// AN UNKNOWN DELETE IS RESOLVED BY READING. It may have
		// committed with its response lost, and assuming either way is
		// how a retry deletes the NEXT operation's record.
		current, found, readErr := e.backends.Fleet.Maintenance(ctx, op.Stream)
		if readErr != nil {
			return op, err
		}
		switch statelog.ResolveDelete(op.OperationID, current, found) {
		case statelog.DeleteDone, statelog.DeleteNotMine:
			out := op
			out.Revision = 0
			return out, nil
		}
		if errors.Is(err, coord.ErrMaintenanceMoved) {
			return current, nil
		}
		return op, err
	}
	log.InfoContext(ctx, "capacity_operation_confirmed", "stream", op.Stream,
		"operation", op.OperationID, "attempt", op.Attempt,
		"target", op.TargetMaxBytes, "observed", op.ObservedMaxBytes,
		"abandoned", op.Blocked == coord.BlockAbandoned,
		"detail", "the exclusion on this log is released; once no log has a "+
			"window open, restart every node without -mode to return the fleet "+
			"to service")
	out := op
	out.Revision = 0
	return out, nil
}

// AbandonCapacity is the operator's exit.
//
// FROM `opened` IT CLEARS OUTRIGHT (no request has been issued) and answers no
// operation, since no window is left to show. From everywhere else it ENTERS
// THE SEAL: abandoning changes what the operation reaches, never the barrier
// it must cross, since a paused coordinator's request may be outstanding.
//
// OPERATIONID IS THE CONFIRMATION that the person read which window they
// abandon, checked against the operation read here BEFORE anything changes
// ([ConfirmMismatchError]). BY is recorded on the operation while it lives
// ([coord.MaintenanceOperation.AbandonedBy]) and on the log line when not.
func (e *Engine) AbandonCapacity(ctx context.Context, stream, operationID string,
	by iam.Actor) (coord.MaintenanceOperation, error) {

	if err := windowStream(stream); err != nil {
		return coord.MaintenanceOperation{}, err
	}
	op, found, err := e.backends.Fleet.Maintenance(ctx, stream)
	if err != nil {
		return coord.MaintenanceOperation{}, err
	}
	if !found {
		return coord.MaintenanceOperation{}, refused(
			"there's no capacity operation on %s to abandon", stream)
	}
	if op.OperationID != operationID {
		return op, &ConfirmMismatchError{Stream: stream, Open: op.OperationID,
			Named: operationID}
	}
	phase, clears, err := statelog.AbandonPhase(op)
	if err != nil {
		return op, err
	}
	if clears {
		if err := e.backends.Fleet.CloseMaintenance(ctx, stream,
			op.OperationID, op.Revision); err != nil {
			return op, err
		}
		log.InfoContext(ctx, "capacity_operation_abandoned", "stream", stream,
			"operation", op.OperationID, "phase", op.Phase,
			"by", by.Name, "operator", by.OperatorID,
			"detail", "no configuration request had been issued, so the "+
				"exclusion on this log is released outright")
		return coord.MaintenanceOperation{}, nil
	}
	out := op
	out.Phase = phase
	out.Blocked = coord.BlockAbandoned
	out.AbandonedBy = partyOf(by)
	if err := e.backends.Fleet.UpdateMaintenance(ctx, out); err != nil {
		return op, err
	}
	log.WarnContext(ctx, "capacity_operation_abandoning", "stream", stream,
		"operation", op.OperationID, "was", op.Phase,
		"by", by.Name, "operator", by.OperatorID,
		"detail", "a request may be outstanding, so the window closes once its "+
			"seal holds, which needs every node restarted with `-mode seal` unless "+
			"they already are: the observed ceiling is then recorded as the "+
			"outcome whatever it is")
	return e.reread(ctx, stream)
}

// ConfirmMismatchError is an abandonment whose confirmation names another
// operation than the one open on the stream: nothing was abandoned.
type ConfirmMismatchError struct {
	Stream string
	// Open is the operation open on the stream, and Named the one the
	// confirmation repeated.
	Open, Named string
}

func (e *ConfirmMismatchError) Error() string { return "engine: " + e.Tell() }

// Tell is the refusal as the operator who sent the confirmation reads it
// ([statelog.Told]).
func (e *ConfirmMismatchError) Tell() string {
	return fmt.Sprintf("the capacity operation open on %s is %s, and the "+
		"confirmation named %s, so nothing was abandoned", e.Stream, e.Open, e.Named)
}

// RetireNode records an operator's assertion that node is gone for good: no
// process will run under its id again, so no capacity window counts it while
// it runs nothing ([fleetNow.running]) and writes no row ([coord.NodeRetirement]).
//
// JUDGED BEFORE IT IS WRITTEN, as an eviction is: a running node
// ([Engine.nodeRunning]) is refused, this one included; a fleet nobody could
// read retires nothing. Its author is on the record and the log.
func (e *Engine) RetireNode(ctx context.Context, node string, by iam.Actor) (coord.NodeRetirement, error) {
	switch {
	case !config.ValidNodeID(node):
		return coord.NodeRetirement{}, refused("%q is no id a node could run under (`node.id`: a letter "+
			"or digit, then letters, digits, `.`, `_` or `-`, at most 64 characters), so nothing was "+
			"retired", node)
	case node == e.id:
		return coord.NodeRetirement{}, refused("%s is this node, which is running, so it was not retired",
			node)
	}
	switch now, err := e.nodeRunning(ctx, node); {
	case err != nil:
		return coord.NodeRetirement{}, stopped(err, "couldn't read which nodes are running, so %s "+
			"was not retired: ask again in a moment", node)
	case now.heardOnly(node):
		return coord.NodeRetirement{}, refused("%s was not retired: %s", node,
			heardOnly([]string{node}, "a retirement"))
	case now.running(node):
		return coord.NodeRetirement{}, refused("%s is running (it holds a live presence lease, or its "+
			"broker is a member the fleet's broker hears), so it was not retired. Retire a node once it "+
			"has stopped for good", node)
	}
	r := coord.NodeRetirement{NodeID: node, By: by.Name, ByKind: by.Kind, OperatorID: by.OperatorID,
		At: time.Now().UTC()}
	if err := e.backends.Fleet.RetireNode(ctx, r); err != nil {
		return coord.NodeRetirement{}, err
	}
	log.WarnContext(ctx, "node_retired", "node", node, "by", by.Name, "operator", by.OperatorID,
		"detail", "an operator asserted no process will run under this node id again; capacity "+
			"windows no longer count it until it runs again or writes its position")
	return r, nil
}

// ExcludeParticipant records an operator's assertion that a participant's
// process is stopped and holds no outstanding request.
//
// THE ONLY THING THAT WAIVES AN ACKNOWLEDGEMENT (an eviction is about whose
// records apply, not whose process runs), and the one way a dead node's
// admission is withdrawn: one that never boots again never retires its claim
// ([Engine.supersede]), so the window opens with it standing and sends nothing
// ([Engine.admissionsClear]). A node need not be a participant to be excluded.
// BY is recorded against the node ([coord.MaintenanceOperation.ExcludedBy]).
// JUDGED BEFORE IT IS WRITTEN, as a retirement is: the assertion is false of a
// running node ([Engine.nodeRunning]), which is refused.
func (e *Engine) ExcludeParticipant(ctx context.Context, stream, node string,
	by iam.Actor) (coord.MaintenanceOperation, error) {

	if err := windowStream(stream); err != nil {
		return coord.MaintenanceOperation{}, err
	}
	if !config.ValidNodeID(node) {
		return coord.MaintenanceOperation{}, refused("%q is no id a node could run under (`node.id`: a "+
			"letter or digit, then letters, digits, `.`, `_` or `-`, at most 64 characters), so nothing "+
			"was excluded", node)
	}
	op, found, err := e.backends.Fleet.Maintenance(ctx, stream)
	if err != nil {
		return coord.MaintenanceOperation{}, err
	}
	if !found {
		return coord.MaintenanceOperation{}, refused(
			"there's no capacity operation on %s", stream)
	}
	// JUDGED AGAINST THE WINDOW AS READ, and written conditional on it below:
	// a window that moved meanwhile refuses, never an exclusion judged before.
	switch now, err := e.nodeRunning(ctx, node); {
	case err != nil:
		return op, stopped(err, "couldn't read which nodes are running, so %s was not excluded: "+
			"ask again in a moment", node)
	case now.heardOnly(node):
		return op, refused("%s was not excluded: %s", node, heardOnly([]string{node}, "an exclusion"))
	case now.running(node):
		return op, refused("%s is running (it holds a live presence lease, its broker is a member "+
			"the fleet's broker hears, or it is this node), so it was not excluded: an exclusion "+
			"asserts its process is stopped. Restart it into the window's mode instead (`crewlet "+
			"retention maintenance status -stream %s` says which)", node, stream)
	}
	out := op
	if !contains(op.Excluded, node) {
		out.Excluded = append(cloneStrings(op.Excluded), node)
	}
	// STORED ONCE: an excluded node is no participant, and has no process
	// the seal compares against.
	out.Participants = slices.DeleteFunc(cloneStrings(op.Participants), func(n string) bool { return n == node })
	switch {
	case len(out.Excluded) > coord.MaxMaintenanceParticipants:
		return op, refused("the capacity operation on %s already excludes %d nodes, the most one "+
			"window keeps, so %s was not excluded. Abandon it with `crewlet retention maintenance "+
			"abandon -stream %s -confirm %s`", stream, len(op.Excluded), node, stream, op.OperationID)
	case len(out.Participants) == 0 && len(op.Journal) > 0:
		return op, refused("%s is the last participant of the capacity operation on %s, which has "+
			"sent a request, so it was not excluded: the seal then needs its restart, or your word that "+
			"no process will run under it again, which a retirement records for the whole fleet and "+
			"withdraws if it runs. Restart it with `crewlet run -mode seal`; if it never will, retire "+
			"it with `crewlet retention maintenance retire -node %s -confirm %s`", node, stream, node, node)
	}
	out.WriteIncarnations = maps.Clone(op.WriteIncarnations)
	delete(out.WriteIncarnations, node)
	// THE LATEST ASSERTION'S AUTHOR: repeating one makes it again, and the seal
	// takes it from whoever said it last.
	out.ExcludedBy = maps.Clone(op.ExcludedBy)
	if out.ExcludedBy == nil {
		out.ExcludedBy = map[string]coord.MaintenanceParty{}
	}
	out.ExcludedBy[node] = partyOf(by)
	if err := e.backends.Fleet.UpdateMaintenance(ctx, out); errors.Is(err, coord.ErrMaintenanceMoved) {
		return op, refused("the capacity operation on %s moved while %s was being judged, so it was "+
			"not excluded: read `crewlet retention maintenance status -stream %s` and ask again",
			stream, node, stream)
	} else if err != nil {
		return op, err
	}
	// AND ITS ADMISSION GOES WITH IT, because an operator asserting a
	// process is stopped is asserting exactly what the admission denies.
	e.withdrawExcluded(ctx, node)
	log.WarnContext(ctx, "capacity_participant_excluded", "stream", stream,
		"operation", op.OperationID, "node", node,
		"by", by.Name, "operator", by.OperatorID,
		"detail", "an operator asserted this node's process is stopped and "+
			"holds no outstanding request; it no longer has to acknowledge")
	return e.reread(ctx, stream)
}

// withdrawExcluded withdraws an excluded node's admission, whichever process of
// it holds one.
//
// ONE IT CANNOT READ OR CLEAR IS LOGGED AND LEFT: the window passes over an
// excluded node's admission ([statelog.AdmissionBlocks]), so the key stops
// nothing, but the failure is never silent.
func (e *Engine) withdrawExcluded(ctx context.Context, node string) {
	held, found, err := heldAdmission(ctx, e.backends.Fleet, node)
	if err == nil && found {
		err = e.backends.Fleet.ForgetAdmission(ctx, node, held.Incarnation)
	}
	if err != nil {
		log.WarnContext(ctx, "excluded_admission_not_withdrawn", "node", node, "err", err)
	}
}

// CapacityState is one log's capacity window as an operator reads it: the
// window or the absence of one, and the two registers its next step reads,
// each as read or with why it couldn't be.
//
// EACH PART IS ITS OWN ANSWER: an acknowledgement or admission read that fails
// after the window was read still leaves the window known.
type CapacityState struct {
	// Op is the window, and Found whether there is one.
	Op    coord.MaintenanceOperation
	Found bool
	// Acks is every acknowledgement of this log's window, or AcksErr why
	// they couldn't be read.
	Acks    []coord.MaintenanceAck
	AcksErr error
	// Admissions is every admission, or AdmissionsErr why they couldn't be
	// read.
	Admissions    []coord.Admission
	AdmissionsErr error
	// RetiredErr is why the window's retirements couldn't be judged
	// ([Engine.judgeWindow]): Op is then marked RetiredUnjudged.
	RetiredErr error
}

// CapacityStatus is one log's window as an operator reads it ([CapacityState]).
// Its error is about the window itself: a log no window can be opened on
// ([UnknownStreamError]), a node with no coordination store, or a window read
// that failed. A part read after it that failed is the state's to say.
func (e *Engine) CapacityStatus(ctx context.Context, stream string) (CapacityState, error) {
	if err := windowStream(stream); err != nil {
		return CapacityState{}, err
	}
	if e.backends == nil || e.backends.Fleet == nil {
		return CapacityState{}, errNoFleet
	}
	var state CapacityState
	var err error
	state.Op, state.Found, err = e.backends.Fleet.Maintenance(ctx, stream)
	if err != nil {
		return CapacityState{}, err
	}
	state.Op, state.RetiredErr = e.judgeWindow(ctx, state.Op)
	state.Acks, state.AcksErr = e.backends.Fleet.MaintenanceAcks(ctx, stream)
	state.Admissions, state.AdmissionsErr = e.backends.Fleet.Admissions(ctx)
	return state, nil
}

// CapacityAcks is every acknowledgement of one log's window, which an answer
// carrying a blocked window reads to say what closes it
// ([statelog.BlockedDetail]).
func (e *Engine) CapacityAcks(ctx context.Context, stream string) (
	[]coord.MaintenanceAck, error) {

	if e.backends == nil || e.backends.Fleet == nil {
		return nil, errNoFleet
	}
	return e.backends.Fleet.MaintenanceAcks(ctx, stream)
}

// CapacityWindows is every capacity window open on a log, the oldest first,
// and every log whose window this node couldn't read.
//
// IT IS WHAT STANDS BETWEEN THE FLEET AND SERVICE: a node refuses to start in
// service while any log has a window open or unreadable ([Engine.admit]), so a
// client says to return to service only when both are empty, never after each
// close. It is the one read every reader of a window takes ([readWindows]).
func (e *Engine) CapacityWindows(ctx context.Context) (open []coord.MaintenanceOperation,
	unread []string) {

	var fleet coord.Fleet
	if e.backends != nil {
		fleet = e.backends.Fleet
	}
	read := readWindows(ctx, fleet)
	for _, stream := range read.unread {
		log.WarnContext(ctx, "capacity_window_unreadable", "stream", stream,
			"err", read.errs[stream], "detail", "this log may have a capacity "+
				"window open, and a node won't start in service while it does")
	}
	return read.open, read.unread
}

// windowStream refuses a stream no capacity window can ever be open on with an
// [UnknownStreamError] naming the logs one can be: a window is only opened on a
// registered domain's log ([Engine.SetCapacity]), so another name is a typo,
// which read as a log with no window the operator would never be told.
func windowStream(stream string) error {
	streams := maintenanceStreams()
	if !slices.Contains(streams, stream) {
		return &UnknownStreamError{Stream: stream, Streams: streams}
	}
	return nil
}

// reread returns the operation at its current revision, which every later
// compare-and-set is formed from.
//
// A RETIREMENT IT COULDN'T JUDGE IS MARKED SO ([coord.MaintenanceOperation.
// RetiredUnjudged]), logged, and never fails the gesture whose write has
// landed: its answer says it couldn't judge, a step deciding on it waits on
// more nodes, never fewer, and the seal and a request judge afresh themselves.
func (e *Engine) reread(ctx context.Context, stream string) (
	coord.MaintenanceOperation, error) {

	op, found, err := e.backends.Fleet.Maintenance(ctx, stream)
	if err != nil {
		return coord.MaintenanceOperation{}, err
	}
	if !found {
		return coord.MaintenanceOperation{}, nil
	}
	judged, err := e.judgeWindow(ctx, op)
	if err != nil {
		log.WarnContext(ctx, "capacity_window_unjudged", "stream", stream,
			"operation", op.OperationID, "err", err, "detail", "this node couldn't judge which "+
				"participants are retired, so this answer says so and waives none of them")
	}
	return judged, nil
}

// capacityParticipants is who must acknowledge, read when the window opens and
// before every request ([Engine.recordRequest]): EVERY NODE THE FLEET HAS A
// POSITION FOR but one whose retirement stands ([coord.NodeRetirement]), union
// every live node whose PROCESS could hold an outstanding request, union this
// node. NOT the retention counted set, since an evicted node can still run
// one. A live node takes part if it holds the estate ([capacityHolders]) or
// its broker is a member, which queues requests whatever its roles
// ([capacityMembers]); one whose presence does not say counts as a member. A
// leaf holding no data runs no JetStream.
func (e *Engine) capacityParticipants(ctx context.Context) (capacityFleet, error) {
	now, err := e.fleetNow(ctx)
	if err != nil {
		return capacityFleet{}, err
	}
	seen := map[string]bool{}
	fleet := capacityFleet{now: now}
	for _, id := range capacityHolders(now.held) {
		seen[id] = true
	}
	for _, id := range capacityMembers(now.held) {
		seen[id] = true
	}
	for id := range now.rows {
		switch {
		case now.running(id):
			seen[id] = true
		case now.retiredStands(id):
		default:
			seen[id] = true
		}
	}
	// EVERY NODE WHOSE BROKER THE METADATA GROUP HEARS, which joins at boot
	// before a node acknowledges or claims presence ([fleetNow.voters]).
	for id := range now.members {
		seen[id] = true
	}
	// AND THIS NODE AS IT IS, whether or not its presence or a position has
	// landed. A maintenance-mode node KEEPS its presence, which is how members
	// holding no data are found above (maintenance_mode.go).
	seen[e.id] = true
	fleet.participants = sortedKeys(seen)
	return fleet, nil
}

// capacityFleet is who takes part in a capacity window as the fleet stands,
// and the fleet it was read from.
type capacityFleet struct {
	participants []string
	now          fleetNow
}

// fleetNow is the fleet as a window judges it, read afresh: when each node's
// position row was stored, each node's retirement, who holds presence, and
// whose broker the metadata group hears: voters by raft peer id, and members
// by the node id a voter names.
type fleetNow struct {
	rows        map[string]time.Time
	retirements map[string]coord.NodeRetirement
	live        map[string]bool
	held        []coord.Lease
	voters      map[string]bool
	members     map[string]bool
}

// fleetNow reads the fleet as a window judges it. This node is live, whether
// or not its presence has landed.
func (e *Engine) fleetNow(ctx context.Context) (fleetNow, error) {
	now := fleetNow{rows: map[string]time.Time{}, retirements: map[string]coord.NodeRetirement{},
		live: map[string]bool{e.id: true}, voters: map[string]bool{}, members: map[string]bool{}}
	rows, err := e.backends.Fleet.Positions(ctx)
	if err != nil {
		return now, fmt.Errorf("engine: read the fleet's positions to establish "+
			"who must acknowledge: %w", err)
	}
	for _, row := range rows {
		now.rows[row.NodeID] = row.StoredAt
	}
	retirements, err := e.backends.Fleet.NodeRetirements(ctx)
	if err != nil {
		return now, fmt.Errorf("engine: read the retired nodes to establish who "+
			"must acknowledge: %w", err)
	}
	for _, r := range retirements {
		now.retirements[r.NodeID] = r
	}
	return now, e.readRunning(ctx, &now)
}

// readRunning reads into now who is running: who holds presence, and whose
// broker the metadata group hears ([Engine.brokerVoters]).
func (e *Engine) readRunning(ctx context.Context, now *fleetNow) error {
	if e.backends.Coord != nil {
		var err error
		if now.held, err = e.backends.Coord.ListLive(ctx, coord.ClassNode); err != nil {
			return fmt.Errorf("engine: list the live nodes: %w", err)
		}
		for _, lease := range now.held {
			if id, ok := coord.ClassNode.Name(lease.Resource); ok {
				now.live[id] = true
			}
		}
	}
	return e.brokerVoters(ctx, now)
}

// nodeRunning reports whether a process under node is running as a window
// judges it ([fleetNow.running]): this node, one holding presence, or one whose
// broker the metadata group hears. What an exclusion or a retirement asserts is
// false of it.
func (e *Engine) nodeRunning(ctx context.Context, node string) (fleetNow, error) {
	now := fleetNow{live: map[string]bool{e.id: true}, voters: map[string]bool{},
		members: map[string]bool{}}
	return now, e.readRunning(ctx, &now)
}

// brokerOfflineWithin is how long the fleet's broker may go on hearing a member
// whose process died without stopping: nats-server marks a server offline on
// its shutdown event, and one that sent none only once its orphan sweep finds
// it silent 150 s, a sweep that runs every 90 s (server/events.go orphMax and
// chkOrph, five and three of its 30 s heartbeats), so at most four minutes.
const brokerOfflineWithin = 4 * time.Minute

// heardOnly is the refusal's account of node, running only because the fleet's
// broker still hears it: it may be starting, or have died without stopping,
// which the broker says within [brokerOfflineWithin]. Never a restart to make.
func heardOnly(nodes []string, gesture string) string {
	return fmt.Sprintf("%s %s no presence, but the fleet's broker still hears %s %s as %s, so "+
		"%s may be starting, and %s would be false of %s. A member that died without stopping is "+
		"marked offline within %d minutes: ask again then, or read `crewlet fleet broker list`",
		words.And(nodes), hasOrHave(nodes), itsOrTheir(nodes), oneOr(nodes, "broker", "brokers"),
		oneOr(nodes, "a member", "members"), oneOr(nodes, "it", "they"), gesture,
		oneOr(nodes, "it", "them"), int(brokerOfflineWithin.Minutes()))
}

// heardOnly reports whether id is running only because the fleet's broker
// hears it: no presence, its voter not offline.
func (f fleetNow) heardOnly(id string) bool {
	return !f.live[id] && f.voters[jetstream.PeerIDOf(id)]
}

// brokerVoters reads into now every voter of the fleet's metadata group that is
// not offline: a node's embedded broker joins it as the node boots, before the
// node acknowledges a window or claims presence, and can hold a request from
// then on. Read in process on a member, else from a member that reports it
// ([FleetBroker.readGroup]); a lone member has no peers to read.
//
// UNREAD IS AN ERROR, never "nobody runs": a judgement that missed a member
// would waive the process holding a request. A node with no embedded broker
// (one on an external stream) has none of the fleet's processes to read.
func (e *Engine) brokerVoters(ctx context.Context, now *fleetNow) error {
	if e.backends.broker == nil || e.profile.Broker == placement.BrokerClient {
		return nil
	}
	live := map[string]placement.NodeProfile{e.id: e.profile}
	for _, lease := range now.held {
		if profile, ok := placement.FromLease(lease); ok && profile.ID != e.id {
			live[profile.ID] = profile
		}
	}
	group, _, why, solo := (&FleetBroker{e: e}).readGroup(ctx, live)
	switch {
	case solo:
		return nil
	case group == nil:
		return fmt.Errorf("engine: read which nodes' brokers are members of the fleet's: %s", why)
	}
	for _, peer := range group.Peers {
		if peer.Offline {
			continue
		}
		now.voters[peer.Peer] = true
		if peer.Name != "" {
			now.members[peer.Name] = true
		}
	}
	return nil
}

// running reports whether a process under id is running as f was read: it
// holds presence, or its broker is a voter the metadata group hears.
func (f fleetNow) running(id string) bool {
	return f.live[id] || f.voters[jetstream.PeerIDOf(id)]
}

// retiredStands reports whether id's retirement stands: it isn't running and
// stored no row after it ([coord.NodeRetirement.Stands]).
func (f fleetNow) retiredStands(id string) bool {
	r, retired := f.retirements[id]
	return retired && !f.running(id) && r.Stands(f.rows[id])
}

// judged is op with its Retired judged against f: each participant whose
// retirement stands as f was read, waived until the node runs again.
func (f fleetNow) judged(op coord.MaintenanceOperation) coord.MaintenanceOperation {
	op.Retired, op.RetiredUnjudged = nil, false
	for _, node := range op.Participants {
		if f.retiredStands(node) {
			op.Retired = append(op.Retired, node)
		}
	}
	return op
}

// judgeWindow is op with its Retired judged against the fleet as it stands
// now ([coord.MaintenanceOperation.Retired]): every read of a window does it,
// since a retirement waives a participant only while it stands. Unjudged, it
// is op marked so, with Retired empty, beside why.
func (e *Engine) judgeWindow(ctx context.Context, op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {
	if op.OperationID == "" {
		return op, nil
	}
	now, err := e.fleetNow(ctx)
	if err != nil {
		op.Retired, op.RetiredUnjudged = nil, true
		return op, err
	}
	return now.judged(op), nil
}

// outgrown is the refusal of a window on stream that would count the nodes
// counted, past [coord.MaxMaintenanceParticipants], said from what f holds
// of each: those counted for a position row alone, to retire if gone for good;
// those recorded and now neither live nor holding a row, to exclude; and only
// where the live ones are themselves too many, the bound itself.
func (f fleetNow) outgrown(stream string, counted []string) string {
	var dormant, absent []string
	alive := 0
	for _, node := range counted {
		_, row := f.rows[node]
		switch {
		case f.running(node):
			alive++
		case row:
			dormant = append(dormant, node)
		default:
			absent = append(absent, node)
		}
	}
	said := fmt.Sprintf("%d nodes would have to restart for the seal of a capacity window on %s, and "+
		"a window names at most %d", len(counted), stream, coord.MaxMaintenanceParticipants)
	if len(dormant) > 0 {
		said += fmt.Sprintf(". %s %s no live presence and %s counted for %s position %s alone: "+
			"retire each that is gone for good with `crewlet retention maintenance retire -node "+
			"<node-id> -confirm <node-id>`, then run set-capacity again", words.And(dormant),
			hasOrHave(dormant), isOrAre(dormant), itsOrTheir(dormant), rowOrRows(dormant))
	}
	if len(absent) > 0 {
		said += fmt.Sprintf(". %s, recorded by the window, %s no presence and no position row: "+
			"exclude each that is stopped for good with `crewlet retention maintenance exclude "+
			"-stream %s -node <node-id> -confirm <node-id>`", words.And(absent), hasOrHave(absent),
			stream)
	}
	if alive > coord.MaxMaintenanceParticipants {
		said += fmt.Sprintf(". %d of them are live, and a capacity window serves a fleet of at most %d "+
			"member and data nodes", alive, coord.MaxMaintenanceParticipants)
	}
	return said
}

// recordedPast is the refusal of a request widened would send recording more
// participants than [coord.MaxMaintenanceRecorded] while counting within the
// bound, blocked as next: retiring lowers only the count, so it names the
// retired participants to exclude, and how many, where the window has room for
// them ([coord.MaintenanceOperation.RecordedOver], as its stored sentence).
func recordedPast(next, widened coord.MaintenanceOperation) string {
	said := fmt.Sprintf("Crewlet didn't send this window's next request: it would record %d "+
		"participants, past the %d one window keeps, though it counts only %d. A participant stays "+
		"recorded, retired or not, so retiring more ids won't lower it", len(widened.Participants),
		coord.MaxMaintenanceRecorded, len(widened.Counted()))
	abandon := fmt.Sprintf("abandon the window with `crewlet retention maintenance abandon -stream %s "+
		"-confirm %s`", next.Stream, next.OperationID)
	over, room := next.RecordedOver()
	if over > room {
		return said + fmt.Sprintf(". It would take %d exclusions to bring it within that, and it has "+
			"room for %d more, so %s.", over, max(room, 0), abandon)
	}
	return said + fmt.Sprintf(". Exclude at least %d of the retired participants (%s) with `crewlet "+
		"retention maintenance exclude -stream %s -node <node-id> -confirm <node-id>`, then run "+
		"set-capacity again; or %s.", over, words.And(widened.Retired), next.Stream, abandon)
}

// The words [capacityFleet.outgrown] agrees with its count of nodes in.
func hasOrHave(nodes []string) string  { return oneOr(nodes, "holds", "hold") }
func isOrAre(nodes []string) string    { return oneOr(nodes, "is", "are") }
func itsOrTheir(nodes []string) string { return oneOr(nodes, "its", "their") }
func rowOrRows(nodes []string) string  { return oneOr(nodes, "row", "rows") }
func oneOr(nodes []string, one, more string) string {
	if len(nodes) == 1 {
		return one
	}
	return more
}

// capacityHolders is every live node holding the replicated estate (every data
// node holds it whole), as its presence says: the publishers of its records.
//
// PRESENCE AND THE POSITIONS REGISTER BETWEEN THEM, since neither alone sees
// every publisher: a drain gives presence up while its state log still runs
// (the register's rows, [Engine.capacityParticipants], cover it), and a node
// not yet heartbeating a position is on presence alone. Presence whose roles
// this build cannot read counts as every role: an uncounted publisher is what
// this set exists to prevent. A data node publishes whatever its company
// keeps, since the core runs every registered log on it from boot
// ([Engine.startCore]), so [placement.NodeProfile.HoldsData] is the whole test.
func capacityHolders(held []coord.Lease) []string {
	out := make([]string, 0, len(held))
	for _, lease := range held {
		if profile, ok := placement.FromLease(lease); ok && profile.HoldsData() {
			out = append(out, profile.ID)
		}
	}
	return out
}

// capacityMembers is every live node whose broker is a member of the fleet's,
// or does not say — see [Engine.capacityParticipants] for why a node that does
// not say is counted.
func capacityMembers(held []coord.Lease) []string {
	var out []string
	for _, lease := range held {
		profile, ok := placement.FromLease(lease)
		if !ok {
			continue
		}
		switch profile.Broker {
		case placement.BrokerMember, placement.BrokerUnknown:
			out = append(out, profile.ID)
		}
	}
	return out
}

// capacityIncarnations is each participant's identity as a request is about to
// leave, read from the acknowledgements every maintenance-mode node writes for
// this log at boot, bound to the window open then or WINDOWLESS (the ordinary
// case for a fleet restarted before the verb ran). Either names the process;
// which one the seal takes is [statelog.SealRestarts]' question.
//
// A PARTICIPANT WITH NO ACKNOWLEDGEMENT HAS NO RESTART RECORDED
// ([statelog.BaselineMissing], the rule the status pages read too) and is
// refused rather than recorded as "": a request leaves only once every
// participant's process is named. "Recorded", because a node that restarted
// but could not write its acknowledgement is refused the same, and its own
// log says why.
func (e *Engine) capacityIncarnations(ctx context.Context, op coord.MaintenanceOperation,
	fleet fleetNow) (map[string]string, []string, error) {

	acks, err := e.backends.Fleet.MaintenanceAcks(ctx, op.Stream)
	if err != nil {
		return nil, nil, err
	}
	// THIS NODE AS IT IS, last so it wins over any record of its own.
	acks = append(acks, coord.MaintenanceAck{
		Stream: op.Stream, NodeID: e.id, Incarnation: e.incarnation, Mode: string(e.mode),
	})
	missing := statelog.BaselineMissing(op, acks)
	if heard := slices.DeleteFunc(slices.Clone(missing), func(n string) bool {
		return !fleet.heardOnly(n)
	}); len(heard) > 0 {
		return nil, nil, refused("no restart into a maintenance mode is recorded for %s, so the "+
			"window can't record which process is running there before its request leaves, and it "+
			"sends nothing until it can. %s", words.And(heard), heardOnly(heard, "an exclusion"))
	}
	if len(missing) > 0 {
		exclude := "<node-id>"
		if len(missing) == 1 {
			exclude = missing[0]
		}
		return nil, nil, refused("no restart into a maintenance mode "+
			"is recorded for %s, so the window can't record which process is "+
			"running there before its request leaves, and it sends nothing until "+
			"it can for every node. Restart %s with `-mode "+
			"maintenance` (a node that did restart says in its log why it wasn't "+
			"recorded), or if %s gone for good, record that with `crewlet "+
			"retention maintenance exclude -stream %s -node %s -confirm %s`",
			words.And(missing), eachOrIt(missing),
			oneIsOrItIs(missing), op.Stream, exclude, exclude)
	}
	byNode := map[string]string{}
	for _, ack := range acks {
		byNode[ack.NodeID] = ack.Incarnation
	}
	incarnations := map[string]string{}
	for _, node := range op.Participants {
		if !contains(op.Waived(), node) {
			incarnations[node] = byNode[node]
		}
	}
	return incarnations, op.Participants, nil
}

// completeJournal marks one request's own record completed, on its own
// read-back.
//
// THAT RECORD AND NO OTHER, matched on attempt, issuer and instant: completing
// an earlier request whose outcome is unknown on a later one's read-back would
// retire it by value, which the journal refuses ([coord.JournalRetired]); only
// the seal resolves it.
func completeJournal(journal []coord.JournalRecord, record coord.JournalRecord) []coord.JournalRecord {
	out := cloneJournal(journal)
	for i := range out {
		if out[i].Attempt == record.Attempt && out[i].By == record.By &&
			out[i].At.Equal(record.At) && out[i].State == coord.JournalIssued {
			out[i].State = coord.JournalCompleted
			out[i].ResolvedAt = time.Now().UTC()
		}
	}
	return out
}

func cloneJournal(in []coord.JournalRecord) []coord.JournalRecord {
	return append([]coord.JournalRecord(nil), in...)
}

func cloneStrings(in []string) []string { return append([]string(nil), in...) }

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// sleep waits, or returns early when the context ends.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// dialsExternalBroker reports whether this fleet's stream lives on a broker
// the engine does not run, where stopping every Crewlet node establishes no
// exclusion (the embedded broker binds no socket; see [Engine.SetCapacity]).
func (e *Engine) dialsExternalBroker() bool {
	return e.backends != nil && e.backends.Queue != nil &&
		e.backends.Queue.Backend() != "jetstream-embedded"
}

// partyOf is a party as a capacity window records one.
func partyOf(by iam.Actor) coord.MaintenanceParty {
	return coord.MaintenanceParty{By: by.Name, OperatorID: by.OperatorID}
}
