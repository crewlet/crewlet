package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
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
// is nowhere else it could run at all. Which maintenance-mode node is decided
// by an ordinary lease, and it never decides whether a publisher may run —
// that is the operation record's job, and the two have different lifetimes for
// exactly that reason.

// capacityLeaseTTL is how long a coordinator holds the drive without
// re-claiming.
//
// A MINUTE, and it is short on purpose. What expiry costs here is a takeover,
// which resumes the same operation id and the same immutable target — so a
// coordinator that has stalled hands over quickly rather than holding a fleet
// in maintenance while somebody works out whether it is alive. The state it
// was driving has no TTL at all, which is what makes a fast handover safe.
const capacityLeaseTTL = time.Minute

// CapacityRequest is what an operator asked for.
type CapacityRequest struct {
	// Stream is which log's ceiling changes, and TargetMaxBytes what to.
	Stream         string
	TargetMaxBytes uint64

	// By names the operator, for the record.
	By string

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
		return coord.MaintenanceOperation{}, fmt.Errorf(
			"engine: this node is publishing, so a capacity change cannot run " +
				"here: restart every node of the fleet with `-mode maintenance` " +
				"first. The usage a resize is decided against has to be a " +
				"quantity nothing can move, and what retires a request the " +
				"broker already queued is the broker process restarting")
	}
	if e.backends == nil || e.backends.Fleet == nil || e.native == nil || e.native.log == nil {
		return coord.MaintenanceOperation{}, errors.New(
			"engine: this node runs no state log, so it has no stream to resize")
	}
	running := e.native.log.domains[e.native.log.domainOf(req.Stream)]
	if running == nil {
		return coord.MaintenanceOperation{}, fmt.Errorf(
			"engine: %q is not a domain log this build runs — the streams a "+
				"capacity change applies to are %v",
			req.Stream, maintenanceStreams())
	}
	if req.TargetMaxBytes == 0 {
		return coord.MaintenanceOperation{}, errors.New(
			"engine: a capacity target of zero would be an unbounded log, which " +
				"is a real setting this verb does not set")
	}
	// AN EXTERNAL BROKER IS ONE THE ENGINE DOES NOT OWN, so it cannot
	// establish who else holds a connection to it. A refusal an operator
	// can override knowingly is honest; a check that quietly proves
	// nothing is not.
	if e.dialsExternalBroker() && !req.Assert {
		return coord.MaintenanceOperation{}, errors.New(
			"engine: this fleet dials an external NATS cluster, so stopping " +
				"every Crewlet node does not establish that nothing is " +
				"publishing — the engine does not run that broker and cannot " +
				"know who else holds a connection. Pass the explicit assertion " +
				"that you have excluded every publisher, or run the change on " +
				"the embedded topology where the exclusion is structural")
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
	op, err := e.openCapacity(ctx, req, stats.MaxBytes)
	if err != nil {
		return coord.MaintenanceOperation{}, err
	}
	return e.driveCapacity(ctx, running, op)
}

// openCapacity takes the exclusion, or resumes the operation already holding
// it.
//
// THE ID IS MINTED ONCE and reused across create attempts, because an unknown
// create establishes nothing: the request may never have been received and may
// still be outstanding, so a retry with a fresh id would open a SECOND window
// on a stream that may already have one.
func (e *Engine) openCapacity(ctx context.Context, req CapacityRequest,
	originalMaxBytes uint64) (coord.MaintenanceOperation, error) {

	held, found, err := e.backends.Fleet.Maintenance(ctx, req.Stream)
	if err != nil {
		return coord.MaintenanceOperation{}, err
	}
	if found {
		if held.TargetMaxBytes != req.TargetMaxBytes {
			return coord.MaintenanceOperation{}, fmt.Errorf(
				"engine: a capacity operation on %s is already open, targeting "+
					"%d bytes rather than %d. Its target is chosen once and "+
					"never changed — resolve it with `crewlet retention "+
					"maintenance status`, or abandon it, before choosing a "+
					"different one",
				req.Stream, held.TargetMaxBytes, req.TargetMaxBytes)
		}
		// THE SAME TARGET is a resume, which is the ordinary path after
		// a restart into the next mode.
		return held, nil
	}

	// EVERY ADMISSION BLOCKS. A node that read the operation absent and
	// started publishing in the interval has left a durable record here,
	// which is the whole reason the handshake is two-sided.
	admissions, err := e.backends.Fleet.Admissions(ctx)
	if err != nil {
		return coord.MaintenanceOperation{}, fmt.Errorf(
			"engine: read the admissions before taking the exclusion: an "+
				"unreadable one is a publisher this operation cannot see: %w", err)
	}
	if blocking := statelog.AdmissionBlocks(admissions, nil); len(blocking) > 0 {
		return coord.MaintenanceOperation{}, fmt.Errorf(
			"engine: %v still hold an admission, so they may be publishing. "+
				"Restart them with `-mode maintenance`, or — if a node is gone "+
				"rather than running — record that with `crewlet retention "+
				"maintenance --exclude`", blocking)
	}

	participants, err := e.capacityParticipants(ctx)
	if err != nil {
		return coord.MaintenanceOperation{}, err
	}
	op := coord.MaintenanceOperation{
		Stream:           req.Stream,
		OperationID:      config.NewIncarnation("capacity"),
		TargetMaxBytes:   req.TargetMaxBytes,
		OriginalMaxBytes: originalMaxBytes,
		Phase:            coord.PhaseOpened,
		Attempt:          1,
		Participants:     participants,
		EnteredAt:        time.Now().UTC(),
		By:               req.By,
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
				"participants", op.Participants, "by", op.By)
			return held, nil
		case held.OperationID == op.OperationID:
			// AN EARLIER ATTEMPT OF MINE WON and its answer was lost.
			return held, nil
		default:
			return coord.MaintenanceOperation{}, fmt.Errorf(
				"engine: another capacity operation (%s, opened by %s) holds %s",
				held.OperationID, held.By, req.Stream)
		}
	}
	return coord.MaintenanceOperation{}, fmt.Errorf(
		"engine: could not establish whether the capacity window on %s was "+
			"opened: the create may never have been received and may still be "+
			"outstanding, so this fleet may already be excluded. Read `crewlet "+
			"retention maintenance status` before trying again: %w",
		req.Stream, last)
}

// driveCapacity advances the operation as far as this mode's evidence allows.
func (e *Engine) driveCapacity(ctx context.Context, running *runningDomain,
	op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {

	for range len(coord.MaintenancePhases) + 1 {
		next, err := e.advanceCapacity(ctx, running, op)
		if err != nil {
			return op, err
		}
		if next.Phase == op.Phase && next.Revision == op.Revision {
			return next, nil
		}
		op = next
	}
	return op, fmt.Errorf("engine: the capacity operation on %s did not settle "+
		"in %d transitions, which is more than the table has phases",
		op.Stream, len(coord.MaintenancePhases)+1)
}

// advanceCapacity takes one step: gather what this mode can establish, ask the
// table, and persist whatever it answered.
func (e *Engine) advanceCapacity(ctx context.Context, running *runningDomain,
	op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {

	switch op.Phase {
	case coord.PhaseOpened:
		if !e.mode.WritesConfiguration() {
			return op, nil
		}
		return e.baseline(ctx, op)
	case coord.PhaseBaselined:
		if e.mode.WritesConfiguration() {
			return e.apply(ctx, running, op)
		}
		return e.seal(ctx, running, op)
	case coord.PhaseApplied:
		if e.mode.WritesConfiguration() {
			// APPLIED IS NOT DONE and this mode cannot finish it: the
			// seal is established by processes that cannot write
			// configuration, which is this mode's own capability.
			return op, nil
		}
		return e.seal(ctx, running, op)
	case coord.PhaseSealing:
		return e.seal(ctx, running, op)
	case coord.PhaseSealed:
		return e.verify(ctx, running, op)
	case coord.PhaseConfirmed:
		return e.release(ctx, op)
	}
	return op, fmt.Errorf("engine: the capacity operation on %s is in phase %q, "+
		"which this build does not know", op.Stream, op.Phase)
}

// baseline persists the participant set, every participant's incarnation and
// the journal record — BEFORE any request is issued.
//
// The order is the whole of it. Recorded after the read-back, a crash in
// between leaves an operation that may have written with nothing to compare
// against, and an abandonment before any apply has no defined outcome at all.
func (e *Engine) baseline(ctx context.Context, op coord.MaintenanceOperation) (
	coord.MaintenanceOperation, error) {

	incarnations, participants, err := e.capacityIncarnations(ctx, op)
	if err != nil {
		return op, err
	}
	next := op
	next.Participants = participants
	next.WriteIncarnations = incarnations
	next.Journal = append(cloneJournal(op.Journal), coord.JournalRecord{
		Attempt: op.Attempt, By: e.incarnation,
		State: coord.JournalIssued, At: time.Now().UTC(),
	})
	next.Phase = coord.PhaseBaselined
	if err := e.backends.Fleet.UpdateMaintenance(ctx, next); err != nil {
		return op, err
	}
	log.InfoContext(ctx, "capacity_baselined", "stream", op.Stream,
		"operation", op.OperationID, "attempt", op.Attempt,
		"participants", participants)
	return e.reread(ctx, op.Stream)
}

// apply issues the configuration request and reads the ceiling back.
//
// THE READ-BACK IS THE EVIDENCE, never the acknowledgement: what the phase
// establishes is that the ceiling IS the target, which is a value the broker
// reports rather than a request it accepted.
func (e *Engine) apply(ctx context.Context, running *runningDomain,
	op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {

	if err := running.log.SetMaxBytes(ctx, op.TargetMaxBytes); err != nil {
		// THE JOURNAL RECORD STAYS `issued`. A request that returned an
		// error is an UNKNOWN: it may have landed. What resolves it is
		// the barrier, and the operation stays where it is so the seal
		// is the next step.
		return op, fmt.Errorf("engine: set %s's ceiling to %d: the outcome is "+
			"unknown and the operation stays open — restart the fleet with "+
			"`-mode seal` to retire it: %w", op.Stream, op.TargetMaxBytes, err)
	}
	stats, err := running.log.Stats(ctx)
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
		out.Journal = completeJournal(op.Journal, op.Attempt, e.incarnation)
	}
	if err := e.backends.Fleet.UpdateMaintenance(ctx, out); err != nil {
		return op, err
	}
	log.InfoContext(ctx, "capacity_applied", "stream", op.Stream,
		"operation", op.OperationID, "attempt", op.Attempt,
		"target", op.TargetMaxBytes, "observed", stats.MaxBytes, "phase", next)
	return e.reread(ctx, op.Stream)
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
func (e *Engine) seal(ctx context.Context, running *runningDomain,
	op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {

	if e.mode != statelog.ModeSeal {
		// THIS MODE CANNOT ESTABLISH THE BARRIER, and saying so is the
		// answer rather than an error: the operator's next step is the
		// second of the three restarts.
		return op, nil
	}
	acks, err := e.backends.Fleet.MaintenanceAcks(ctx)
	if err != nil {
		return op, fmt.Errorf("engine: read the maintenance acknowledgements: %w", err)
	}
	out := op
	out.Phase = coord.PhaseSealing

	stats, err := running.log.Stats(ctx)
	if err != nil {
		return op, fmt.Errorf("engine: read %s's ceiling: %w", op.Stream, err)
	}
	if restarted, incarnations := barrierEvidence(op, acks); restarted {
		out = statelog.Retire(out, statelog.BarrierEvidence{
			Incarnations: incarnations, ObservedMaxBytes: stats.MaxBytes,
			At: time.Now().UTC(),
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
	if err := e.backends.Fleet.UpdateMaintenance(ctx, out); err != nil {
		return op, err
	}
	if held, missing := statelog.SealHolds(out, acks); !held {
		log.InfoContext(ctx, "capacity_seal_pending", "stream", op.Stream,
			"operation", op.OperationID, "attempt", op.Attempt, "missing", missing)
	}
	return e.reread(ctx, op.Stream)
}

// verify compares the observed ceiling against the immutable target.
//
// A MISMATCH IS A NUMBERED ATTEMPT rather than a re-apply: every node is in
// seal mode, which refuses configuration writes, so there is nowhere legal for
// a re-apply to run.
func (e *Engine) verify(ctx context.Context, running *runningDomain,
	op coord.MaintenanceOperation) (coord.MaintenanceOperation, error) {

	stats, err := running.log.Stats(ctx)
	if err != nil {
		return op, fmt.Errorf("engine: read %s's ceiling to verify: %w", op.Stream, err)
	}
	if stats.MaxBytes != op.TargetMaxBytes {
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
func (e *Engine) release(ctx context.Context, op coord.MaintenanceOperation) (
	coord.MaintenanceOperation, error) {

	err := e.backends.Fleet.CloseMaintenance(ctx, op.Stream, op.OperationID, op.Revision)
	if err != nil && !errors.Is(err, coord.ErrMaintenanceMoved) {
		// AN UNKNOWN DELETE IS RESOLVED BY READING. It may have
		// committed with its response lost, and assuming either way is
		// how a retry deletes the NEXT operation's record.
		current, found, readErr := e.backends.Fleet.Maintenance(ctx, op.Stream)
		if readErr != nil {
			return op, err
		}
		switch statelog.ResolveDelete(op.OperationID, current, found) {
		case statelog.DeleteDone, statelog.DeleteNotMine:
			return op, nil
		}
		return op, err
	}
	log.InfoContext(ctx, "capacity_operation_confirmed", "stream", op.Stream,
		"operation", op.OperationID, "attempt", op.Attempt,
		"target", op.TargetMaxBytes,
		"detail", "the exclusion is released; restart every node without -mode "+
			"to return the fleet to service")
	out := op
	out.Revision = 0
	return out, nil
}

// AbandonCapacity is the operator's exit.
//
// FROM `opened` IT CLEARS OUTRIGHT — no request has been issued, so there is
// nothing to retire. From everywhere else it ENTERS THE SEAL: abandoning
// changes what the operation is trying to reach, never the barrier it must
// cross, and a paused coordinator's request is outstanding whether or not a
// person has read a status page.
func (e *Engine) AbandonCapacity(ctx context.Context, stream string) (
	coord.MaintenanceOperation, error) {

	op, found, err := e.backends.Fleet.Maintenance(ctx, stream)
	if err != nil {
		return coord.MaintenanceOperation{}, err
	}
	if !found {
		return coord.MaintenanceOperation{}, fmt.Errorf(
			"engine: there is no capacity operation on %s to abandon", stream)
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
			"detail", "no configuration request had been issued, so the "+
				"exclusion is released outright")
		out := op
		out.Revision = 0
		return out, nil
	}
	out := op
	out.Phase = phase
	out.Blocked = "abandoned_by_operator"
	if err := e.backends.Fleet.UpdateMaintenance(ctx, out); err != nil {
		return op, err
	}
	log.WarnContext(ctx, "capacity_operation_abandoning", "stream", stream,
		"operation", op.OperationID, "was", op.Phase,
		"detail", "a request may be outstanding, so the fleet still has to "+
			"restart with `-mode seal`: the observed ceiling is then recorded "+
			"as the outcome whatever it is")
	return e.reread(ctx, stream)
}

// ExcludeParticipant records an operator's assertion that a participant's
// process is stopped and holds no outstanding request.
//
// THE ONLY THING THAT WAIVES AN ACKNOWLEDGEMENT. An eviction does not: that is
// about whose records apply, and this is about whose process is running.
func (e *Engine) ExcludeParticipant(ctx context.Context, stream, node string) (
	coord.MaintenanceOperation, error) {

	op, found, err := e.backends.Fleet.Maintenance(ctx, stream)
	if err != nil {
		return coord.MaintenanceOperation{}, err
	}
	if !found {
		return coord.MaintenanceOperation{}, fmt.Errorf(
			"engine: there is no capacity operation on %s", stream)
	}
	out := op
	if !contains(op.Excluded, node) {
		out.Excluded = append(cloneStrings(op.Excluded), node)
	}
	if err := e.backends.Fleet.UpdateMaintenance(ctx, out); err != nil {
		return op, err
	}
	// AND ITS ADMISSION GOES WITH IT, because an operator asserting a
	// process is stopped is asserting exactly what the admission denies.
	if err := e.backends.Fleet.ForgetAdmission(ctx, node, admissionOf(ctx, e, node)); err != nil {
		log.WarnContext(ctx, "excluded_admission_not_withdrawn", "node", node, "err", err)
	}
	log.WarnContext(ctx, "capacity_participant_excluded", "stream", stream,
		"operation", op.OperationID, "node", node,
		"detail", "an operator asserted this node's process is stopped and "+
			"holds no outstanding request; it no longer has to acknowledge")
	return e.reread(ctx, stream)
}

// admissionOf is the incarnation currently holding a node's admission, so the
// conditional withdrawal has something to condition on. Empty when there is
// none, which makes the withdrawal a no-op.
func admissionOf(ctx context.Context, e *Engine, node string) string {
	admissions, err := e.backends.Fleet.Admissions(ctx)
	if err != nil {
		return ""
	}
	for _, a := range admissions {
		if a.NodeID == node {
			return a.Incarnation
		}
	}
	return ""
}

// CapacityStatus is the operation as an operator reads it, or the absence of
// one.
func (e *Engine) CapacityStatus(ctx context.Context, stream string) (
	coord.MaintenanceOperation, []coord.MaintenanceAck, []coord.Admission, bool, error) {

	if e.backends == nil || e.backends.Fleet == nil {
		return coord.MaintenanceOperation{}, nil, nil, false,
			errors.New("engine: this node has no coordination store")
	}
	op, found, err := e.backends.Fleet.Maintenance(ctx, stream)
	if err != nil {
		return coord.MaintenanceOperation{}, nil, nil, false, err
	}
	acks, err := e.backends.Fleet.MaintenanceAcks(ctx)
	if err != nil {
		return op, nil, nil, found, err
	}
	admissions, err := e.backends.Fleet.Admissions(ctx)
	if err != nil {
		return op, acks, nil, found, err
	}
	return op, acks, admissions, found, nil
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

// capacityParticipants is who must acknowledge.
//
// EVERY NODE THE FLEET HAS A POSITION FOR, union everything currently holding
// a presence lease — NOT the retention counted set, which is about whose
// position pins the trim. This set is about whose process could hold an
// outstanding request, and a node that was evicted from the first is still a
// machine that can run one.
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
		leases, err := e.backends.Coord.ListLive(ctx, coord.NodePrefix)
		if err != nil {
			return nil, fmt.Errorf("engine: list the live nodes: %w", err)
		}
		for _, lease := range leases {
			if id, ok := coord.NodeID(lease.Resource); ok {
				seen[id] = true
			}
		}
	}
	// AND THIS NODE, which is in a maintenance mode and therefore holds
	// no presence lease and may have published no position yet.
	seen[e.id] = true
	return sortedKeys(seen), nil
}

// capacityIncarnations is each participant's identity as of the baseline, read
// from the acknowledgements every maintenance-mode node writes at boot.
//
// A PARTICIPANT WITH NO ACKNOWLEDGEMENT HAS NOT RESTARTED into the mode, and
// is refused rather than baselined at an empty string: an empty baseline
// compares unequal to every later ack, so the seal would be satisfied by a
// node that never came back.
func (e *Engine) capacityIncarnations(ctx context.Context, op coord.MaintenanceOperation) (
	map[string]string, []string, error) {

	acks, err := e.backends.Fleet.MaintenanceAcks(ctx)
	if err != nil {
		return nil, nil, err
	}
	byNode := map[string]string{}
	for _, ack := range acks {
		byNode[ack.NodeID] = ack.Incarnation
	}
	byNode[e.id] = e.incarnation

	incarnations := map[string]string{}
	var missing []string
	for _, node := range op.Participants {
		if contains(op.Excluded, node) {
			continue
		}
		incarnation, acked := byNode[node]
		if !acked {
			missing = append(missing, node)
			continue
		}
		incarnations[node] = incarnation
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("engine: %v have not restarted into a "+
			"maintenance mode, so this operation has no baseline for them — and "+
			"a baseline of nothing compares unequal to every later "+
			"acknowledgement, which would let the seal be satisfied by a node "+
			"that never came back. Restart them with `-mode maintenance`, or "+
			"exclude one that is gone", missing)
	}
	return incarnations, op.Participants, nil
}

// barrierEvidence reports whether every participant has restarted for this
// attempt, and what their incarnations are.
//
// THE SAME EVIDENCE THE SEAL PREDICATE READS, gathered once: a retirement
// established from a different reading than the seal would retire a record the
// seal then refuses to accept.
func barrierEvidence(op coord.MaintenanceOperation, acks []coord.MaintenanceAck) (
	bool, map[string]string) {

	incarnations := map[string]string{}
	byNode := map[string]coord.MaintenanceAck{}
	for _, ack := range acks {
		byNode[ack.NodeID] = ack
	}
	for _, node := range op.Participants {
		if contains(op.Excluded, node) {
			continue
		}
		ack, acked := byNode[node]
		if !acked || ack.OperationID != op.OperationID || ack.Attempt != op.Attempt ||
			ack.Mode != string(statelog.ModeSeal) ||
			ack.Incarnation == op.WriteIncarnations[node] {
			return false, nil
		}
		incarnations[node] = ack.Incarnation
	}
	return true, incarnations
}

// completeJournal marks this attempt's own record completed, on the issuing
// process's own read-back.
func completeJournal(journal []coord.JournalRecord, attempt int, by string) []coord.JournalRecord {
	out := cloneJournal(journal)
	for i := range out {
		if out[i].Attempt == attempt && out[i].By == by && out[i].State == coord.JournalIssued {
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
