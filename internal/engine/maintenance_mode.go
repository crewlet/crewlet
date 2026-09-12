package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHAT A MAINTENANCE-MODE NODE DOES, AND WHAT IT REFUSES TO DO.
//
// A stream's byte ceiling cannot be changed safely while anything publishes,
// and what retires a request the broker has already queued is the broker
// PROCESS restarting rather than a client closing. So a capacity change is a
// fleet-wide restart into a mode that starts every broker and no publisher.
//
// The exclusion is therefore STRUCTURAL rather than observed: on the default
// topology the embedded broker binds no socket, so nothing outside these
// processes can reach it at all, and these processes start nothing that
// writes. There is no check to pass — there is nobody who could publish.
//
// # The two halves this file owns
//
// A node in NORMAL mode must not start publishing into a fleet whose capacity
// operation is unresolved, and it establishes that with a HANDSHAKE rather
// than an absence check: it writes its own admission, re-reads the operation,
// and withdraws and refuses if one is there. Either order is then safe — a
// node that slipped between a coordinator's check and its own start has left a
// durable record that blocks the coordinator, where a silence would have let
// the coordinator proceed.
//
// A node in a MAINTENANCE mode acknowledges instead: it publishes its own
// incarnation against the operation's current attempt, which is what the seal
// is established from. An acknowledgement from seal mode is EVIDENCE — that
// mode cannot write configuration, so the process making the claim cannot be
// the one holding the request it is retiring.

// ErrExcluded is a boot refused because a capacity operation is unresolved.
//
// A SENTINEL, because the caller's answer is specific and is not a retry: the
// fleet is mid-window, and this node joins it when the window closes or when
// an operator restarts it into the same mode as its peers.
type ErrExcluded struct {
	Stream      string
	OperationID string
	Phase       coord.MaintenancePhase
	By          string
}

func (e *ErrExcluded) Error() string {
	return fmt.Sprintf("engine: this node may not start publishers: a capacity "+
		"operation on %s (%s, phase %s, opened by %s) is unresolved, and a "+
		"publisher during the window would make the usage it observes a moving "+
		"quantity. Restart this node in the same mode as its peers "+
		"(`crewlet run -mode maintenance`), or read "+
		"`crewlet retention maintenance status`",
		e.Stream, e.OperationID, e.Phase, e.By)
}

// admit is the normal-mode handshake, run BEFORE any publisher starts.
//
// # Why the write comes first
//
// A node that read the operation absent and THEN started publishing has a gap
// between the two instants in which a coordinator can establish exclusion —
// the same check-then-act shape this estate has removed four times elsewhere.
// Writing first inverts it: whichever of the two writes lands first wins, and
// the loser can see that it did.
//
// # And why this is at boot rather than on the heartbeat
//
// The population the operation has to exclude is exactly the nodes that were
// NOT running when it was written. A node that reads it on a heartbeat never
// reads it at all, because it was down when the key appeared.
func (e *Engine) admit(ctx context.Context, streams []string) error {
	if e.backends == nil || e.backends.Fleet == nil || e.mode != statelog.ModeNormal {
		return nil
	}
	admission := coord.Admission{
		NodeID: e.id, Incarnation: e.incarnation, At: time.Now().UTC(),
	}
	if err := e.backends.Fleet.PutAdmission(ctx, admission); err != nil {
		// REFUSED RATHER THAN CARRIED ON WITHOUT. An admission that
		// could not be written is a publisher a coordinator cannot see,
		// which is the one thing the handshake exists to make
		// impossible.
		return fmt.Errorf("engine: record this node's admission before starting "+
			"publishers: without it a capacity operation could take the "+
			"exclusion while this node is publishing: %w", err)
	}
	for _, stream := range streams {
		op, found, err := e.backends.Fleet.Maintenance(ctx, stream)
		if err != nil {
			// UNREADABLE IS NOT ABSENT. Reading a coordination failure
			// as "no maintenance" is the admission event the whole
			// design removes.
			_ = e.withdraw(ctx)
			return fmt.Errorf("engine: read the capacity operation on %s before "+
				"starting publishers: %w", stream, err)
		}
		if found {
			_ = e.withdraw(ctx)
			return &ErrExcluded{
				Stream: stream, OperationID: op.OperationID,
				Phase: op.Phase, By: op.By,
			}
		}
	}
	return nil
}

// withdraw removes this node's admission.
//
// ON AN ORDERLY SHUTDOWN, AFTER PUBLISHERS DRAIN AND NEVER BEFORE: the key
// says "this process may be publishing", and withdrawing it while a seat is
// still finishing a turn would tell a coordinator the opposite of the truth.
//
// It is conditional on this incarnation, so a late withdrawal cannot remove a
// replacement process's admission.
func (e *Engine) withdraw(ctx context.Context) error {
	if e.backends == nil || e.backends.Fleet == nil || e.mode != statelog.ModeNormal {
		return nil
	}
	return e.backends.Fleet.ForgetAdmission(
		context.WithoutCancel(ctx), e.id, e.incarnation)
}

// acknowledge is the maintenance-mode half: this node's evidence that its
// process restarted for the operation's current attempt.
//
// # Why every mode but normal writes one
//
// The seal is established only from SEAL mode, because that is the mode that
// cannot write configuration. But a coordinator in maintenance mode needs to
// know who is participating before it can baseline, and an acknowledgement
// carrying its own mode is what lets one record answer both questions: the
// barrier filters on the mode, and the participant set is read from the same
// place.
func (e *Engine) acknowledge(ctx context.Context, streams []string) {
	if e.backends == nil || e.backends.Fleet == nil || e.mode == statelog.ModeNormal {
		return
	}
	for _, stream := range streams {
		op, found, err := e.backends.Fleet.Maintenance(ctx, stream)
		if err != nil {
			log.WarnContext(ctx, "maintenance_operation_unreadable",
				"stream", stream, "err", err,
				"detail", "this node cannot acknowledge an operation it cannot "+
					"read, so the seal will report it as not having restarted")
			continue
		}
		if !found {
			// NO OPERATION IS NOT A FAILURE. A fleet restarted into
			// maintenance mode before the verb ran is the ordinary
			// first step of the procedure.
			continue
		}
		ack := coord.MaintenanceAck{
			NodeID: e.id, OperationID: op.OperationID, Attempt: op.Attempt,
			Incarnation: e.incarnation, Mode: string(e.mode), At: time.Now().UTC(),
		}
		if err := e.backends.Fleet.PutMaintenanceAck(ctx, ack); err != nil {
			log.WarnContext(ctx, "maintenance_ack_not_recorded",
				"stream", stream, "operation", op.OperationID, "err", err,
				"detail", "the seal is established from these, so the operation "+
					"will report this node as not having restarted")
			continue
		}
		log.InfoContext(ctx, "maintenance_acknowledged",
			"stream", stream, "operation", op.OperationID, "attempt", op.Attempt,
			"mode", e.mode, "incarnation", e.incarnation)
	}
}

// Mode is what this node started for.
func (e *Engine) Mode() statelog.MaintenanceMode {
	if e.mode == "" {
		return statelog.ModeNormal
	}
	return e.mode
}

// maintenanceStreams is every stream a capacity operation could be open on.
//
// EVERY REGISTERED DOMAIN'S, because an operation on any one of them excludes
// this node from publishing at all: the applier writes into one estate and a
// seat's tools reach every domain through it, so a node that started because
// only the vector log was clear would be publishing into the tracker's.
func maintenanceStreams() []string {
	domains := registeredDomains()
	out := make([]string, 0, len(domains))
	for _, domain := range domains {
		out = append(out, domain.Stream().Name)
	}
	return out
}
