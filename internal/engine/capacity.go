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

// THE CAPACITY COORDINATOR: what drives [statelog.PermitPhase] against a real
// broker and a real fleet.
//
// It states no transitions of its own. Every phase it reaches, it reaches by
// offering evidence and being told where it stands — which is what makes the
// table executable rather than descriptive, and what stops this file becoming
// the fifth prose copy of a procedure that has already forked four times.
//
// # Where it runs
//
// In a MAINTENANCE-MODE node, because the verb needs a broker and no
// publisher, and on the default topology the broker binds no socket so there
// is nowhere else it could run at all. WHICH maintenance-mode node is not
// decided anywhere, and that is the design rather than an omission: the
// operation RECORD is both the operation and the exclusion, so two operators
// calling two nodes contend on its compare-and-set create — one opens the
// window, and the other is either told who holds it or handed the same one
// back to resume, because the target is immutable and matching it IS the
// resume. A coordinator lease beside that record would be a second thing to
// hold, with an expiry of its own, over state that deliberately has none: an
// interrupted drive is picked up by whoever calls next with the same target,
// and nothing has to decide whether the previous caller is alive.

// CapacityRequest is what an operator asked for.
type CapacityRequest struct {
	// Stream is which log's ceiling changes, and TargetMaxBytes what to.
	Stream         string
	TargetMaxBytes uint64

	// By names the operator, for the record — the AUTHOR, internal/iam's
	// ActorFor name — and OperatorID the credential they acted through,
	// kept apart for the reason every other trail keeps them apart: a
	// person's machine token is swept a week after it lapses, and an
	// operation that named only `pat:<id>` would name nobody by then.
	By         string
	OperatorID string

	// Assert is the operator's explicit statement that they have excluded
	// every publisher, required only on a broker the engine does not run.
	Assert bool
}

// SetCapacity drives one capacity operation as far as the current mode allows,
// and reports where it stands.
//
// # What "as far as the current mode allows" means
//
// The procedure needs three fleet-wide restarts and this call runs inside one
// of them. From MAINTENANCE mode it opens, baselines, applies and stops —
// the next step is the operator restarting the fleet into seal mode. From SEAL
// mode it collects the barrier, retires the journal, seals, verifies and
// confirms. That is not a state machine of its own: it is the same table,
// advanced with whatever evidence this mode can produce.
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
	// AND A TARGET PAST int64 IS THE SAME SETTING WEARING A LARGE NUMBER.
	// A stream's ceiling is an int64 on the wire, so anything above that
	// wraps NEGATIVE on the way out — and a negative MaxBytes is how
	// JetStream spells "unbounded". Refused for the reason zero is, and
	// with the ceiling named, because the two produce an identical stream
	// from opposite-looking inputs.
	if req.TargetMaxBytes > math.MaxInt64 {
		return coord.MaintenanceOperation{}, refused(
			"a capacity target of %d is past the %d bytes a stream's ceiling "+
				"can carry. It would be sent as a negative, which is how an "+
				"unbounded log is spelled, the one setting this verb doesn't set",
			req.TargetMaxBytes, int64(math.MaxInt64))
	}
	// AN EXTERNAL BROKER IS ONE THE ENGINE DOES NOT OWN, so it cannot
	// establish who else holds a connection to it. A refusal an operator
	// can override knowingly is honest; a check that quietly proves
	// nothing is not.
	if e.dialsExternalBroker() && !req.Assert {
		return coord.MaintenanceOperation{}, refused(
			"this fleet dials an external NATS cluster, so stopping every " +
				"Crewlet node doesn't establish that nothing is publishing: the " +
				"engine doesn't run that broker and can't know who else holds a " +
				"connection. Pass the explicit assertion that you've excluded " +
				"every publisher, or run the change on the embedded topology " +
				"where the exclusion is structural")
	}

	// READ ONCE, HERE. The original ceiling is what an operator is shown
	// beside the target and what an abandon returns to, and the domain was
	// already resolved above — a second lookup inside the open path was
	// the same map read written twice, with only one of them guarded.
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
// DECLARED HERE, AS THEIR CONSUMER, so the window's whole drive (the request,
// the seal, the verify and the release) runs against a log a test holds,
// where the concrete broker log tied every step past the baseline to a
// running cluster and the tests forged the phases those steps write instead.
// A nil one is a drive in a mode or a phase that touches no log.
type capacityLog interface {
	SetMaxBytes(ctx context.Context, maxBytes uint64) error
	Stats(ctx context.Context) (jetstream.LogStats, error)
}

// growthRoom is how far the broker will let a running log's ceiling grow, as
// far as this node can read it, and unstated where it cannot.
//
// ASKED OF THIS NODE'S OWN BROKER, which is the one holding the stream being
// resized — see [Engine.capacityHost]. An engine with no core runtime has no
// such stream either, so the two absences are the same one.
//
// UNREAD IS UNSTATED, and the window still opens. The check this feeds only
// spares an operator the restarts of learning a refusal late; the broker is the
// authority either way and still refuses a raise it cannot reserve, while a
// window refused because a read failed would block a raise it would grant.
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
// field to, with the field's name — both properties of the DOMAIN, so read
// from the register rather than from a running log.
//
// THE FLOOR IS THE FIELD'S, read off the register entry that declares the
// field ([domainCeiling.Floor]), and never one number for every log: the
// verb refused every target under a gibibyte on every log, calling it the
// floor Tier A enforces, while Tier A takes the identity estate's and the
// usage log's down to 64 MiB ([config.IamLogMaxBytesFloor],
// [config.UsageLogMaxBytesFloor]) — so the one way to give either log back a
// reservation was refused, with a reason that was not true of it.
func capacityBounds(stream string) (statelog.Reservation, domainCeiling, bool) {
	for _, domain := range registeredDomains() {
		if domain.Stream().Name != stream {
			continue
		}
		entry, found := registrationFor(domain.Name())
		if !found || entry.Ceiling == nil {
			return statelog.Reservation{}, domainCeiling{}, false
		}
		// THE FIELD'S DECLARATION, which does not depend on what the
		// field is set to or on the volume: an unset stream and no free
		// bytes name the field and its floor as well as any.
		return statelog.ReservationOf(domain), entry.Ceiling(config.Stream{}, 0), true
	}
	return statelog.Reservation{}, domainCeiling{}, false
}

// smallestTargetAbove is the smallest ceiling a log holding bytes can be given
// whose ordinary writes are not refused the moment it applies, and never below
// floor.
//
// SEARCHED rather than solved, because the reserve is the larger of two terms
// and each rounds ([statelog.Reservation.Bytes]): a closed form is right to
// within a byte either way, and a number an operator is told to type has to be
// the exact one. The ordinary ceiling never falls as the target grows, so the
// least target above bytes is found by bisection — between bytes, whose
// ordinary ceiling is at most bytes, and bytes plus its own reserve, which is
// above it — in sixty-odd steps at any size.
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
// THE ID IS MINTED ONCE and reused across create attempts, because an unknown
// create establishes nothing: the request may never have been received and may
// still be outstanding, so a retry with a fresh id would open a SECOND window
// on a stream that may already have one.
//
// IT READS NO ADMISSION BEFORE THE RECORD EXISTS. The window's record is this
// side's half of the handshake, so it is written before the admissions are
// read ([Engine.keepOpen]). Read before the record existed, they were a check
// a node could slip past (it admitted itself after the read and found no
// window before the create), and they kept the window shut over a dead node's
// admission, which only an exclusion on an open window can withdraw.
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
		return held, nil
	}

	// A CEILING ALREADY AT THE TARGET HAS NOTHING TO CHANGE, and a window
	// opened for it costs the fleet three restarts to reach the number it
	// started at. The ordinary way here is the verb run again after its own
	// window closed, its answer lost on the way back, which is the retry
	// an operator reaches for.
	if req.TargetMaxBytes == current.MaxBytes {
		return coord.MaintenanceOperation{}, refused("%s's ceiling is already "+
			"%d bytes, so there's nothing to change and no window was opened",
			req.Stream, current.MaxBytes)
	}

	// A TARGET BELOW THE LOG'S OWN FLOOR is refused as Tier A refuses one
	// on an explicit value of that log's field: below it the log is a
	// window that refuses appends before anybody could act, and the floor
	// is the field's, so it differs by log. A resume is not asked, for the
	// reason the usage below is not.
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

	// A TARGET WHOSE ORDINARY CEILING IS AT OR BELOW WHAT THE LOG HOLDS IS A
	// FULL LOG the moment it applies: every ordinary append and every
	// linearizable read refused, fleet-wide, after three restarts spent
	// reaching it. On a log that keeps a gate reserve that ceiling is the
	// target less the reserve ([statelog.Reservation.Ordinary]), since that is
	// where ordinary writes are refused; a target that only the reserve is
	// above leaves a log an eviction can reach and nothing else can. The
	// usage is what a resize is decided against, and in this mode nothing
	// moves it, so it is decided here, once, before the exclusion is taken.
	// A resume is not asked again: its target was accepted when the window
	// opened, and refusing it now would strand a window whose request may
	// already be in flight.
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

	// AND A RAISE THE BROKER CANNOT RESERVE, for the same reason at the other
	// end. The broker refuses that update when the window applies it, and an
	// apply that returned an error is an unknown only the seal retires, so the
	// operator would spend the window's restarts, and its attempts, learning
	// a number this node can read now. Only a limit read exactly is held
	// against the target ([jetstream.Queue.GrowthBudget]); an unstated one
	// opens the window and leaves the broker to answer.
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

	participants, err := e.capacityParticipants(ctx)
	if err != nil {
		return coord.MaintenanceOperation{}, err
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
			// THE WINDOW AS THE STEP LEFT IT, where the step wrote it before
			// it refused (a read-back that isn't the target, a verify out of
			// attempts): the caller shows it beside the refusal, and the one
			// this pass read would show a window from before the step.
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
	// moves to a later phase, so nothing an operator asked for can loop, and
	// told as a refusal it answered 409 "the capacity change was refused" in
	// the table's own terms. Untold, the route answers it as the fault it is
	// and sends the reader to the status read.
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

// request sends the window's configuration request, and records who is running
// immediately before it leaves: [Engine.recordRequest], then [Engine.apply], in
// one step and never apart.
//
// # Why the record and the request are one step
//
// The seal's evidence is that the process each participant was running when a
// request left is gone ([statelog.SealRestarts]): an acknowledgement from seal
// mode by any other process. That holds only if the processes it compares
// against are the ones running as the request left. They were recorded once,
// at the baseline, and the request was a later step: a coordinator that stopped
// between them left the request to whoever ran the verb next, maybe much later,
// and a node restarted into seal mode in between was running as that request
// left while differing from its baseline, so the seal passed on a restart
// older than the request. So every request, the first and every resumed one, is
// sent by the step that has just recorded who is running, with its own journal
// record, and a resumed window is recorded again before its request leaves.
func (e *Engine) request(ctx context.Context, resized capacityLog,
	op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {

	recorded, record, err := e.recordRequest(ctx, op)
	if err != nil {
		return op, err
	}
	return e.apply(ctx, resized, recorded, record)
}

// recordRequest persists the participant set, every participant's incarnation
// and a journal record for the request about to leave, BEFORE it is issued:
// from `opened` that is the baseline, and from `baselined` it records again for
// a request a resumed window sends.
//
// The order is the whole of it. Recorded after the read-back, a crash in
// between leaves an operation that may have written with nothing to compare
// against, and an abandonment before any apply has no defined outcome at all.
//
// AT `opened` IT IS WHERE THE ADMISSIONS ARE READ ([Engine.admissionsClear]),
// because it is the last step before the first request leaves and the first
// that runs with the window's record already written. Past it no step reads
// one: a node that boots in service finds the window and withdraws.
//
// # Who takes part is taken again here
//
// The set the window recorded UNION the set the fleet shows now
// ([Engine.capacityParticipants]). A window can wait at `opened` for as long as
// an admission or a missing acknowledgement holds it, and at `baselined` for as
// long as its coordinator is gone, and a broker member that came up in the
// meantime holds the request this step is about to send as surely as one that
// was there at the open: left out, the seal never waited for its restart, and
// its broker could apply the request after the window confirmed. Never
// narrowed, because a node recorded earlier that has gone silent since is a
// process nobody has shown stopped; only an exclusion waives one.
func (e *Engine) recordRequest(ctx context.Context, op coord.MaintenanceOperation) (
	coord.MaintenanceOperation, coord.JournalRecord, error) {

	if op.Phase == coord.PhaseOpened {
		if err := e.admissionsClear(ctx, op); err != nil {
			return op, coord.JournalRecord{}, err
		}
	}
	now, err := e.capacityParticipants(ctx)
	if err != nil {
		return op, coord.JournalRecord{}, err
	}
	everyone := map[string]bool{}
	for _, node := range append(cloneStrings(op.Participants), now...) {
		everyone[node] = true
	}
	widened := op
	widened.Participants = sortedKeys(everyone)
	incarnations, participants, err := e.capacityIncarnations(ctx, widened)
	if err != nil {
		return op, coord.JournalRecord{}, err
	}
	record := coord.JournalRecord{
		Attempt: op.Attempt, By: e.incarnation,
		State: coord.JournalIssued, At: time.Now().UTC(),
	}
	phase, err := statelog.PermitPhase(op, statelog.PhaseEvidence{
		Now: record.At, BaselineWritten: true,
	})
	if err != nil {
		return op, coord.JournalRecord{}, err
	}
	next := op
	next.Phase = phase
	next.Participants = participants
	next.WriteIncarnations = incarnations
	next.Journal = append(cloneJournal(op.Journal), record)
	if err = e.backends.Fleet.UpdateMaintenance(ctx, next); err != nil {
		return op, coord.JournalRecord{}, err
	}
	log.InfoContext(ctx, "capacity_request_recorded", "stream", op.Stream,
		"operation", op.OperationID, "attempt", op.Attempt, "was", op.Phase,
		"participants", participants, "incarnations", incarnations)
	recorded, err := e.reread(ctx, op.Stream)
	return recorded, record, err
}

// keepOpen decides whether a window this node has just opened may stand, now
// that its record is written and the admissions can be read against it: the
// coordinator's half of the handshake is to write the window and then read the
// admissions ([Engine.admissionsClear] says why that order).
//
// # A running publisher closes it again
//
// A node whose admission names the process holding its presence is running,
// and may be publishing ([Engine.judgeAdmissions]). A window left open over it
// would refuse every node that started in service meanwhile and tell it to
// restart into maintenance mode, taking a serving node out of service, while
// the fleet went on writing beneath a window whose alarm says nothing is. So
// the window is closed again at the revision it was opened at, which sends
// nothing since nothing has been sent at `opened`, and the verb is refused as
// it was before the window existed. A presence or an admission this node
// cannot read closes it too: nobody can say the fleet stopped.
//
// # A stopped one keeps it open
//
// An admission whose process isn't running is a node that stopped without
// giving it back, the case the window exists to get past: it stays open at
// `opened`, where the node's restart into maintenance mode or an operator's
// exclusion retires the claim, and the baseline names it until then.
//
// A CLOSE THAT DOES NOT LAND leaves the window standing, and the refusal says
// how to close it, since a running publisher is still behind it.
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
	// running, because nobody holds the node's presence: a process that
	// stopped without giving its admission back, or one still starting
	// that has not taken its lease yet. Only a restart of the node or an
	// operator's word can retire it.
	silent []string
}

// judgeAdmissions reads the admissions a window has not excluded against who
// holds each node's presence now ([Engine.presenceHolders]), the evidence
// [Engine.supersede] reads at boot.
//
// AN ADMISSION WHOSE NODE ANOTHER PROCESS NOW HOLDS IS WITHDRAWN HERE. The
// process it names is gone on that evidence, and a boot that could not see it
// yet is the ordinary case: a node restarted into maintenance mode inside its
// crashed process's lease TTL found the old process still present and left
// its claim, and nothing else would come back for it. Withdrawn conditional on
// the incarnation it names, so a newer process's claim is never touched.
//
// UNREADABLE IS AN ERROR, the admissions or the presence alike: an admission
// nobody can read may be a publisher's, and a presence nobody can read makes
// every process look stopped.
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
	for _, node := range statelog.AdmissionBlocks(admissions, op.Excluded) {
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
// # Read with the window's record already written, and that is the rule
//
// The handshake is two writes and two reads. A node writes its admission and
// then reads the window ([Engine.admit]); the coordinator writes the window and
// then reads the admissions, at its open ([Engine.keepOpen]) and again here,
// before the first request. Whichever write lands second, its writer's read
// sees the other's, so either the node refuses to publish or this refuses to
// issue a request. The opposite order on this side (read the admissions, then
// write the window) was a check-then-act: a node could admit itself just after
// the read, find no window just before the create, and publish through the
// resize unseen.
//
// # And it leaves the window open
//
// A window at `opened` has issued nothing, so keeping it while an admission
// stands costs no request, and keeping it is what gives the operator a way
// out: a node restarted into a maintenance mode retires its own last
// process's admission ([Engine.supersede], or [Engine.judgeAdmissions] here
// once the restart holds the node's presence), and one that will never come
// back is excluded on the operator's word ([Engine.ExcludeParticipant]), which
// only an open window can record. Refused before the window existed, a dead
// node's admission kept the fleet out of every window for good. What it does
// cost is every boot in service, which an open window refuses, so the refusal
// names the abandonment that closes it.
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
	// A READ-BACK THAT ISN'T THE TARGET ENDS THE DRIVE, with the request it
	// sent left issued for the seal. The window stays at `baselined`, which
	// in this mode is a step that sends a request, so the drive's next pass
	// sent another, and another, each with a journal record the seal then had
	// to retire, until the drive gave up on seven transitions and answered a
	// window doing what the broker made it do as a fault. One drive sends one
	// request: what settles this one is the seal, which retires it and checks
	// the ceiling again.
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
// A REFUSAL IS AN ANSWER. The broker checked the new ceiling against its limit
// and declined to propose it, so nothing was written and nothing is in flight
// — and the number that was wrong is nameable. Reported as an unknown, that
// sent an operator to the seal for evidence they already had, with the one
// number they needed left out.
//
// EVERYTHING ELSE IS AN UNKNOWN, and the journal record stays `issued` for it:
// a request that returned a transport error may still have landed, and what
// resolves that is the barrier rather than a guess.
//
// THE JOURNAL RECORD STAYS `issued` ON BOTH, which is why even the terminal
// half tells the operator to abandon rather than to walk away: the record
// cannot distinguish THIS attempt's refusal from an earlier attempt's
// outstanding request, and abandoning from any phase past `opened` still
// crosses the barrier. What changes is what they are told went wrong.
func (e *Engine) applyFailure(ctx context.Context, op coord.MaintenanceOperation,
	err error) error {

	if !errors.Is(err, jetstream.ErrInsufficientStorage) {
		return stopped(err, "setting %s's ceiling to %d: the outcome is unknown "+
			"and the operation stays open. Restart the fleet with `-mode seal` to "+
			"retire it", op.Stream, op.TargetMaxBytes)
	}
	// THE ABANDONMENT NAMED IN FULL, its confirmation included: the node
	// refuses one that doesn't repeat the operation's id, so the command
	// this named was refused as written.
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
// ASSERTED OFF THE ENGINE'S OWN QUEUE rather than read from a copy the state
// log keeps. It was that copy, and the copy had ONE writer — a field in one
// struct literal — which the literal did not set, so this answered nil on
// every node that ran a log and the preflight it feeds was dead code from the
// day it was written. A cached type assertion of a handle its owner holds for
// the life of the process is state that can only ever be wrong; deriving it
// here is what makes that omission unrepresentable rather than merely fixed.
//
// The state log is still what decides WHETHER to ask: a node with none has no
// stream to resize, and the assertion cannot fail behind that guard, since
// startStateLog refuses to build one on a broker that does not satisfy this.
func (e *Engine) capacityHost() domainHost {
	if _, err := e.stateLogOf(); err != nil || e.backends == nil {
		return nil
	}
	host, _ := e.backends.Queue.(domainHost)
	return host
}

// capacityRefusal is what the broker had left to grant when it refused the
// raise, or nothing at all where this node cannot ask.
//
// THE GROWTH BUDGET, not the create budget, because a raise is what was
// refused: the broker checks an update against the server leading the metadata
// group rather than against placement, which is the whole of
// [jetstream.Queue.GrowthBudget]'s distinction. Read AT the refusal rather than
// carried from the window's opening, because an operation spans restarts and
// the number that matters is what the broker had when it said no.
//
// AN EMPTY CLAUSE IS THE HONEST ANSWER where this node has nothing to read:
// the refusal it would decorate already carries the broker's own error, and a
// sentence invented here would be numbers nobody read. An unstated limit is
// the same case — an external broker's account may set none, and "the limit is
// -1" is not a sentence.
func (e *Engine) capacityRefusal(ctx context.Context, want uint64) string {
	room := e.growthRoom(ctx)
	if room.Limit < 0 {
		return ""
	}
	// THE SAME SENTENCE THE PROVISIONING REFUSAL BUILDS, over the budget
	// this call just read. A second wording here would be the rule written
	// twice, and a second READ would be a message about a different moment
	// than the one it explains.
	return fmt.Sprintf(". It asked to reserve %d bytes, and %s",
		want, roomLeft(room, nil, e.streamVolume()))
}

// seal collects the barrier, retires the journal against it, and asks the
// table whether the seal holds.
//
// # Retirement runs BEFORE the predicate
//
// That ordering is what breaks the cycle a journal-as-veto produced: the
// barrier needs nothing from the journal, it runs, it retires what it covered,
// and only then is the seal tested against a journal with no unresolved
// entries left.
func (e *Engine) seal(ctx context.Context, resized capacityLog,
	op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {

	if e.mode != statelog.ModeSeal {
		// THIS MODE CANNOT ESTABLISH THE BARRIER, and saying so is the
		// answer rather than an error: the operator's next step is the
		// second of the three restarts.
		return op, nil
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
	// A PASS THAT MOVED NOTHING WRITES NOTHING. A seal still waiting on a
	// node is the answer to the verb run before every node restarted into
	// this mode, and the drive takes an unchanged window as settled: written
	// back on every pass, it never settled, and the verb answered a window
	// doing what it should as an operation that "didn't settle", a fault.
	if out.Phase == op.Phase && !retired && out.ObservedMaxBytes == op.ObservedMaxBytes {
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
// # A delete that lost its race is not a release
//
// The delete is conditional on the revision this drive read, so a window
// somebody changed since (an exclusion or an abandonment landing after the
// verify, or a second coordinator's write) refuses it with
// [coord.ErrMaintenanceMoved]. That refusal was taken as the release: the node
// logged that the exclusion was released while the record still held the fleet,
// and the drive's next pass asked to delete it at no revision, which the store
// refuses, so the verb answered a fault for a window still open. It is read
// instead, like a delete whose answer was lost: gone, or holding another
// operation, is released; still this operation is handed back at the revision
// it is now at, and the drive decides again from where it stands.
//
// A WINDOW THIS DRIVE ALREADY RELEASED HAS NO REVISION, and asking the store to
// delete it again asked a question its answer was already known to.
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
		"abandoned", op.Blocked == statelog.BlockedAbandoned,
		"detail", "the exclusion on this log is released; once no log has a "+
			"window open, restart every node without -mode to return the fleet "+
			"to service")
	out := op
	out.Revision = 0
	return out, nil
}

// AbandonCapacity is the operator's exit.
//
// FROM `opened` IT CLEARS OUTRIGHT — no request has been issued, so there is
// nothing to retire — and answers no operation, since there is no window left
// to show: answered at the phase it cleared from, the CLI printed a window at
// `opened` and told the operator to run set-capacity again for the window they
// had just abandoned. From everywhere else it ENTERS THE SEAL: abandoning
// changes what the operation is trying to reach, never the barrier it must
// cross, and a paused coordinator's request is outstanding whether or not a
// person has read a status page.
//
// OPERATIONID IS THE CONFIRMATION, checked against the operation this read
// finds BEFORE anything changes: it says the person read which window they are
// abandoning, so one that names another operation abandons nothing
// ([ConfirmMismatchError]). Checked by the caller after the fact, as the CLI
// did, it reported a mismatch for an abandonment that had already run.
//
// BY IS WHO ABANDONED IT, recorded on the operation while it lives
// ([coord.MaintenanceOperation.AbandonedBy]) and on the log line when it does
// not.
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
	out.Blocked = statelog.BlockedAbandoned
	out.AbandonedBy = partyOf(by)
	if err := e.backends.Fleet.UpdateMaintenance(ctx, out); err != nil {
		return op, err
	}
	log.WarnContext(ctx, "capacity_operation_abandoning", "stream", stream,
		"operation", op.OperationID, "was", op.Phase,
		"by", by.Name, "operator", by.OperatorID,
		"detail", "a request may be outstanding, so the fleet still has to "+
			"restart with `-mode seal`: the observed ceiling is then recorded "+
			"as the outcome whatever it is")
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

// ExcludeParticipant records an operator's assertion that a participant's
// process is stopped and holds no outstanding request.
//
// THE ONLY THING THAT WAIVES AN ACKNOWLEDGEMENT. An eviction does not: that is
// about whose records apply, and this is about whose process is running.
//
// AND THE ONE WAY A DEAD NODE'S ADMISSION IS WITHDRAWN: a node that never boots
// again never retires its last process's claim ([Engine.supersede]), so the
// window opens with the claim standing, sends nothing while it does
// ([Engine.admissionsClear]), and the exclusion withdraws it. A node need not
// be a participant to be excluded for that.
//
// BY IS WHO ASSERTED IT, recorded against the node it names
// ([coord.MaintenanceOperation.ExcludedBy]).
func (e *Engine) ExcludeParticipant(ctx context.Context, stream, node string,
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
			"there's no capacity operation on %s", stream)
	}
	out := op
	if !contains(op.Excluded, node) {
		out.Excluded = append(cloneStrings(op.Excluded), node)
	}
	// THE LATEST ASSERTION'S AUTHOR, because repeating one is making it
	// again: whoever last said the process is stopped is who the seal is
	// taking it from.
	out.ExcludedBy = maps.Clone(op.ExcludedBy)
	if out.ExcludedBy == nil {
		out.ExcludedBy = map[string]coord.MaintenanceParty{}
	}
	out.ExcludedBy[node] = partyOf(by)
	if err := e.backends.Fleet.UpdateMaintenance(ctx, out); err != nil {
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
// excluded node's admission ([statelog.AdmissionBlocks]), so the key no longer
// stops anything and the warning is the whole of what it leaves behind. It
// read the admissions through a helper that answered a failed read as no
// admission at all, so a failure here withdrew nothing and said nothing.
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
// EACH PART IS ITS OWN ANSWER. The window is read first and the
// acknowledgements and the admissions after it, and one failing after the
// window answered still leaves the window known: folded into one error, the
// page said it couldn't tell whether a window was open on a log it had just
// read one open on.
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
	state.Acks, state.AcksErr = e.backends.Fleet.MaintenanceAcks(ctx, stream)
	state.Admissions, state.AdmissionsErr = e.backends.Fleet.Admissions(ctx)
	return state, nil
}

// CapacityWindows is every capacity window open on a log, the oldest first,
// and every log whose window this node couldn't read.
//
// IT IS WHAT STANDS BETWEEN THE FLEET AND SERVICE once a gesture has closed a
// window: a node refuses to start in service while any log has a window open,
// and while it can't read whether one has ([Engine.admit]). So the step that
// returns the fleet to service is one a client says only when both are empty.
// Said after every close, it sent an operator resizing two logs to restart in
// service while the second window was still open, and every node refused to
// boot. It is the one read every reader of a window takes ([readWindows]),
// which never reads an unreadable log as one with no window, for admit's
// reason.
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
// [UnknownStreamError] naming the logs one can be: a window is only ever opened
// on a registered domain's log ([Engine.SetCapacity]), so a name outside them is
// a mistyped one. Read as a log that has no window, it answered an abandonment
// and an exclusion "there's no capacity operation on X" and a status an empty
// window, and the operator who mistyped it was never told so.
func windowStream(stream string) error {
	streams := maintenanceStreams()
	if !slices.Contains(streams, stream) {
		return &UnknownStreamError{Stream: stream, Streams: streams}
	}
	return nil
}

// reread returns the operation at its current revision, which every later
// compare-and-set is formed from.
func (e *Engine) reread(ctx context.Context, stream string) (
	coord.MaintenanceOperation, error) {

	op, found, err := e.backends.Fleet.Maintenance(ctx, stream)
	if err != nil {
		return coord.MaintenanceOperation{}, err
	}
	if !found {
		return coord.MaintenanceOperation{}, nil
	}
	return op, nil
}

// capacityParticipants is who must acknowledge: read when the window opens, and
// again before every request it sends, where the two are joined
// ([Engine.recordRequest]).
//
// EVERY NODE THE FLEET HAS A POSITION FOR, union every live node whose PROCESS
// could hold an outstanding request, union this node — NOT the retention
// counted set, which is about whose position pins the trim. This set is about
// whose process could hold a request the seal has to retire, and a node that
// was evicted from the first is still a machine that can run one.
//
// # Two questions among the live nodes, asked apart
//
// The seal's proof is that every BROKER process restarted, because a request
// the broker has already queued is retired by the process holding it going
// away, and every PUBLISHER must be admitted. So a live node takes part if it
// holds the estate — it publishes records ([capacityHolders]) — or if its
// broker is a member, which holds queued
// requests whatever its roles ([capacityMembers]). Counted over data nodes
// alone, a member that holds no data was never waited for, and the seal could
// pass while its broker still held a request that would resize the log after
// it.
//
// A node whose presence does not say what its broker is counts as a member:
// excluding it could pass a seal it should hold, where including one that is
// not a member only waits for an acknowledgement an operator can exclude.
//
// A leaf that holds no data takes no part: its broker runs no JetStream and
// queues nothing, and its seats publish through a data node, which is admitted
// in its own right.
func (e *Engine) capacityParticipants(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	rows, err := e.backends.Fleet.Positions(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: read the fleet's positions to establish "+
			"who must acknowledge: %w", err)
	}
	for _, row := range rows {
		seen[row.NodeID] = true
	}
	if e.backends.Coord != nil {
		held, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
		if err != nil {
			return nil, fmt.Errorf("engine: list the live nodes: %w", err)
		}
		for _, id := range capacityHolders(held) {
			seen[id] = true
		}
		for _, id := range capacityMembers(held) {
			seen[id] = true
		}
	}
	// AND THIS NODE AS IT IS, whether or not its presence lease has
	// landed yet and whether or not it has published a position. It is in
	// a maintenance mode, and a node there KEEPS its presence — which is
	// how the members that hold no data are found above, since their
	// presence is the only record they leave (see maintenance_mode.go).
	seen[e.id] = true
	return sortedKeys(seen), nil
}

// capacityHolders is every live node holding the replicated estate — the
// publishers of its records — as its presence says: every data node holds the
// one estate whole.
//
// PRESENCE AND THE POSITIONS REGISTER BETWEEN THEM, because neither alone sees
// every publisher. A drain gives presence up at its first step while the
// node's state log still runs, and a node that has not yet heartbeated its
// first position is on presence alone; the register's rows
// ([Engine.capacityParticipants]) cover the first and presence the second. A
// presence whose roles this build cannot read is read as every role, so the
// node is counted: a publisher that went uncounted is the one thing this set
// exists to prevent.
//
// A DATA NODE IS A PUBLISHER WHATEVER ITS COMPANY KEEPS: the core runs every
// registered log on every node holding the estate from boot — with no company,
// in a maintenance mode, on a vendor's tracker and wiki ([Engine.startCore]) —
// so a presence that holds data is exactly a node that publishes records, and
// [placement.NodeProfile.HoldsData] is the whole test. A node without `data`
// runs no log and is found only as a broker member ([capacityMembers]).
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
// this log at boot: bound to the window open when it booted, or WINDOWLESS
// where none was, which is the ordinary case for a fleet restarted before the
// verb ran. Either names the process, and that is all the record of who is
// running needs; which one the seal takes is [statelog.SealRestarts]'
// question.
//
// A PARTICIPANT WITH NO ACKNOWLEDGEMENT HAS NO RESTART RECORDED into the mode
// ([statelog.BaselineMissing], the one rule the status pages read too), and is
// refused rather than recorded at an empty string: the record names the
// process each participant was running as the request left, and an empty one
// names none, so a request sent past it leaves with nobody shown to have
// reached the procedure's first restart on that node. A request leaves only
// once every participant's process is named. Said as no restart RECORDED
// rather than as no restart, because a node that restarted and could not write
// its acknowledgement (its boot could not read the window, or the write
// failed) is the same refusal, and its own log says which.
func (e *Engine) capacityIncarnations(ctx context.Context, op coord.MaintenanceOperation) (
	map[string]string, []string, error) {

	acks, err := e.backends.Fleet.MaintenanceAcks(ctx, op.Stream)
	if err != nil {
		return nil, nil, err
	}
	// THIS NODE AS IT IS, last so it wins over any record of its own.
	acks = append(acks, coord.MaintenanceAck{
		Stream: op.Stream, NodeID: e.id, Incarnation: e.incarnation, Mode: string(e.mode),
	})
	if missing := statelog.BaselineMissing(op, acks); len(missing) > 0 {
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
		if !contains(op.Excluded, node) {
			incarnations[node] = byNode[node]
		}
	}
	return incarnations, op.Participants, nil
}

// completeJournal marks one request's own record completed, on its own
// read-back.
//
// THAT RECORD AND NO OTHER, matched on its attempt, its issuer and the instant
// it was written. A process that runs the verb twice issues two requests, each
// with its own record, and the first may have ended with its outcome unknown:
// completed on the second's read-back, it would be resolved by a value the
// second request set while it may still be in flight, which is the retirement
// by value the journal refuses ([coord.JournalRetired]). Only the seal
// resolves it.
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
// the engine does not run.
//
// It is what decides whether stopping every Crewlet node establishes anything:
// on the embedded topology the broker binds no socket, so the exclusion is
// structural; on an external cluster the engine cannot know who else holds a
// connection, and saying so is more honest than a check that proves nothing.
func (e *Engine) dialsExternalBroker() bool {
	return e.backends != nil && e.backends.Queue != nil &&
		e.backends.Queue.Backend() != "jetstream-embedded"
}

// partyOf is a party as a capacity window records one.
func partyOf(by iam.Actor) coord.MaintenanceParty {
	return coord.MaintenanceParty{By: by.Name, OperatorID: by.OperatorID}
}
