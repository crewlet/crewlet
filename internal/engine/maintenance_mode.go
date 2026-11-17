package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A maintenance-mode node starts every broker and no publisher, so a stream's
// byte ceiling can change with nothing publishing: the exclusion is structural,
// not observed (package doc, "Maintenance mode"). This file owns the two boot
// halves: a normal-mode node admits itself, then re-reads the operation and
// refuses if one is open ([Engine.admit]); a maintenance-mode node retires its
// previous process's stale admission ([Engine.supersede]) and acknowledges
// ([Engine.acknowledge]). It keeps its presence lease, and withholds seats
// ([Engine.seatsAdmitted]), writers ([Engine.writers]), the object collector
// ([Engine.startNativeCollector]) and every appending read
// ([statelog.RefuseMaintenance]).

// ErrNotPublishing is an operator gesture that appends a record to a state log
// (an eviction, a readmission, a reanchor) asked of a node in a mode that
// publishes nothing. It wraps a sentence naming the mode and the gesture.
//
// Refused for a linearizable read's reason ([statelog.RefuseMaintenance]): the
// mode holds the logs still while a capacity window measures them, whoever
// asks. A participant that will not acknowledge is excluded from the operation
// (`crewlet retention maintenance exclude`), not evicted from the logs.
var ErrNotPublishing = errors.New("engine: this node publishes nothing in its mode")

// appends refuses gesture, naming the mode and the way back, unless this
// node's mode publishes.
func (s *stateLog) appends(gesture string) error {
	if s.mode.Publishes() {
		return nil
	}
	return NotPublishing(gesture, s.mode)
}

// NotPublishing is [ErrNotPublishing] for gesture asked of a node in mode, told
// to the operator who asked ([statelog.Told]): what the mode refuses and the
// way back.
func NotPublishing(gesture string, mode statelog.MaintenanceMode) error {
	return &statelog.Telling{Say: fmt.Sprintf("%s adds a record to the logs, and "+
		"this node runs in %s mode, which adds nothing while a capacity window "+
		"measures them. Run it once the fleet is back in normal mode (`crewlet "+
		"run` without -mode)", gesture, mode), Err: ErrNotPublishing}
}

// handshake is a boot's half of the capacity window, run before any publisher
// starts: in normal mode this process admits itself ([Engine.admit]); in a
// maintenance mode it retires its previous process's admission
// ([Engine.supersede]) and acknowledges ([Engine.acknowledge]). The boot's one
// call, so no maintenance boot can skip the retirement.
func (e *Engine) handshake(ctx context.Context, streams []string) error {
	e.supersede(ctx)
	if err := e.admit(ctx, streams); err != nil {
		return err
	}
	e.acknowledge(ctx, streams)
	return nil
}

// supersede withdraws the admission this node's id holds under another
// incarnation once that process holds no presence lease ([Engine.presenceHolders]):
// a crash never ran [Engine.withdraw], and an admission has no age. A new
// incarnation alone is no proof, since a duplicate process under a live node's
// id boots the same way; one that crashed within its lease TTL is left for the
// window to retire ([Engine.judgeAdmissions]). Maintenance modes only, the
// boot a window needs it gone by: in service a data node's admission replaces
// the key ([Engine.admit]), and a node without data blocks nobody until then.
// The withdrawal is conditional on the incarnation read, never a newer claim.
// A failure, or an unreadable presence, is logged and the boot goes on: the
// claim stands in the window's way by name, and refusing would cost the window
// a participant.
func (e *Engine) supersede(ctx context.Context) {
	if e.backends == nil || e.backends.Fleet == nil || e.Mode().Publishes() {
		return
	}
	held, found, err := heldAdmission(ctx, e.backends.Fleet, e.id)
	if err == nil && (!found || held.Incarnation == e.incarnation) {
		return
	}
	var owners map[string]string
	if err == nil {
		owners, err = e.presenceHolders(ctx)
	}
	if err == nil && owners[e.id] == held.Incarnation {
		log.WarnContext(ctx, "admission_not_superseded", "node", e.id,
			"previous", held.Incarnation, "mode", e.Mode(),
			"detail", "the process this node's admission names still holds the "+
				"node's presence, so it may still be running and publishing; "+
				"the admission stays until a capacity window sees this process "+
				"hold the presence in its place, or an operator excludes the "+
				"node")
		return
	}
	if err == nil {
		err = e.backends.Fleet.ForgetAdmission(ctx, e.id, held.Incarnation)
	}
	if err != nil {
		log.WarnContext(ctx, "admission_not_superseded", "node", e.id,
			"previous", held.Incarnation, "err", err,
			"detail", "an admission a previous process of this node left may "+
				"still stand, and a capacity window sees it as a possible "+
				"publisher until this node restarts again or an operator "+
				"excludes it")
		return
	}
	log.InfoContext(ctx, "admission_superseded", "node", e.id,
		"previous", held.Incarnation, "admitted_at", held.At, "mode", e.Mode(),
		"detail", "a previous process of this node stopped without withdrawing "+
			"its admission and no longer holds the node's presence; this "+
			"process withdraws it, so it no longer stands in a capacity "+
			"window's way")
}

// presenceHolders maps each node's id to the incarnation holding its presence
// lease now (the seat host takes it under [Engine.incarnation]); an absent node
// holds none. It is the evidence an admission is judged by, since a running
// process renews its presence in every mode. Unreadable is an error, never an
// empty fleet, which would read as every admission's process gone. With no
// lease store (a test engine with a fleet register alone) nobody is present.
func (e *Engine) presenceHolders(ctx context.Context) (map[string]string, error) {
	owners := map[string]string{}
	if e.backends.Coord == nil {
		return owners, nil
	}
	held, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, err
	}
	for _, lease := range held {
		if node, ok := coord.NodeID(lease.Resource); ok {
			owners[node] = lease.Owner
		}
	}
	return owners, nil
}

// heldAdmission is the admission a node holds, or none. Unreadable is an
// error and never an absence, for [Engine.admit]'s reason: a key nobody could
// read may be a publisher's.
func heldAdmission(ctx context.Context, fleet coord.MaintenanceRegister, node string) (
	coord.Admission, bool, error) {

	admissions, err := fleet.Admissions(ctx)
	if err != nil {
		return coord.Admission{}, false, err
	}
	for _, a := range admissions {
		if a.NodeID == node {
			return a, true, nil
		}
	}
	return coord.Admission{}, false, nil
}

// ErrExcluded is a boot refused because a capacity operation is unresolved.
// Its own type because the answer is not a retry: the node joins when the
// window closes, or when restarted into the mode its window's phase is in.
type ErrExcluded struct {
	Stream      string
	OperationID string
	Phase       coord.MaintenancePhase
	By          string
}

// Mode is the mode a node joins this window in, decided by the phase:
// maintenance at `opened`, where the next step is a request only that mode
// sends, and seal from `baselined` on, where a request may be outstanding. A
// node restarted into maintenance mode during the seal writes an
// acknowledgement the seal refuses, costing one more restart.
func (e *ErrExcluded) Mode() statelog.MaintenanceMode {
	if e.Phase == coord.PhaseOpened {
		return statelog.ModeMaintenance
	}
	return statelog.ModeSeal
}

func (e *ErrExcluded) Error() string {
	return fmt.Sprintf("engine: this node may not start publishers: a capacity "+
		"operation on %s (%s, phase %s, opened by %s) is unresolved, and a "+
		"publisher during the window would make the usage it observes a moving "+
		"quantity. Restart this node with `crewlet run -mode %s`, the mode the "+
		"window is in at %s, or read `crewlet retention maintenance status "+
		"-stream %s`",
		e.Stream, e.OperationID, e.Phase, e.By, e.Mode(), e.Phase, e.Stream)
}

// admit is the normal-mode handshake, run before any publisher starts. It
// writes the admission first and then reads the operation, so whichever of
// this and a coordinator's write lands first wins and the loser sees it;
// reading first leaves a check-then-act gap. At boot, not on a heartbeat,
// because the nodes an operation must exclude are those that were down when
// it was written.
func (e *Engine) admit(ctx context.Context, streams []string) error {
	if e.backends == nil || e.backends.Fleet == nil || e.mode != statelog.ModeNormal {
		return nil
	}
	if !e.profile.HoldsData() {
		// No state-log publisher to admit: its seats write through a data
		// node admitted in its own right, and an admission here would be
		// waited on for ever. A broker member too: what its broker queues
		// is retired by the restart it acknowledges.
		return nil
	}
	admission := coord.Admission{
		NodeID: e.id, Incarnation: e.incarnation, At: time.Now().UTC(),
	}
	if err := e.backends.Fleet.PutAdmission(ctx, admission); err != nil {
		// Refused: an unwritten admission is a publisher no coordinator
		// can see.
		return fmt.Errorf("engine: record this node's admission before starting "+
			"publishers: without it a capacity operation could take the "+
			"exclusion while this node is publishing: %w", err)
	}
	for _, stream := range streams {
		op, found, err := e.backends.Fleet.Maintenance(ctx, stream)
		if err != nil {
			// Unreadable is not absent: a coordination failure read as
			// "no maintenance" would admit a publisher mid-window.
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

// withdraw removes this node's admission, conditional on this incarnation so a
// late call never removes a replacement's. On an orderly stop it runs after
// publishers drain, never before: the key says this process may be publishing.
//
// It has a share of its own ([Engine.admissionShare]), not a step of the
// stop's allowance ([seat.StopBudget]): every step there falls back to a lease
// lapsing, and an admission has no TTL (an expiring one would hide a
// publisher), so a missed one stays until a restart under the same id or an
// exclusion, which `admission_not_withdrawn` tells the operator.
func (e *Engine) withdraw(ctx context.Context) error {
	if e.backends == nil || e.backends.Fleet == nil || e.mode != statelog.ModeNormal {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.admissionShare())
	defer cancel()
	return e.backends.Fleet.ForgetAdmission(ctx, e.id, e.incarnation)
}

// admissionShare is the part of a stop's coordination time reserved for
// [Engine.withdraw]: a third of the stop's allowance ([seat.StopAllowance]) of
// this node's lease TTL, five seconds at the shipped 45 s; the steps on the
// allowance get the other two thirds ([Engine.stopping]). Carved out of the
// allowance, not added beside it, so a stop against a silent store still
// spends one allowance in total. A third because the withdrawal is two round
// trips (a leader read and a conditional purge), and five seconds is what the
// JetStream client gives a request with no deadline of its own.
func (e *Engine) admissionShare() time.Duration {
	return seat.StopAllowance(e.stopTTL()) / 3
}

// acknowledge is the maintenance-mode half: this node's evidence, for each
// log's window, that its process restarted. It carries its mode: the seal reads
// only seal mode's (that mode cannot write configuration), and a baseline reads
// the participant set from the same records. Data nodes and broker members
// acknowledge ([acknowledges]), the two the participant set counts
// ([Engine.capacityParticipants]); a member that did not would be waited on
// for ever. One per log ([coord.MaintenanceAckKey]): one restart serves every
// open window, and a per-node key let one window's replace another's. A log
// with no window open gets a windowless one
// ([coord.MaintenanceAck.Windowless]), so a fleet restarted before the verb ran
// is found restarted by the baseline, and no seal takes it. A log the boot
// could not read gets none, since a window may be open there.
func (e *Engine) acknowledge(ctx context.Context, streams []string) {
	if e.backends == nil || e.backends.Fleet == nil || e.mode == statelog.ModeNormal ||
		!acknowledges(e.profile) {
		return
	}
	for _, stream := range streams {
		op, found, err := e.backends.Fleet.Maintenance(ctx, stream)
		if err != nil {
			log.WarnContext(ctx, "maintenance_operation_unreadable",
				"stream", stream, "err", err,
				"detail", "this node can't acknowledge an operation it can't "+
					"read, and records no restart either since a window may be "+
					"open there, so a window's baseline and its seal will both "+
					"see no restart from this node until it restarts again")
			continue
		}
		ack := coord.MaintenanceAck{
			Stream: stream, NodeID: e.id, Incarnation: e.incarnation,
			Mode: string(e.mode), At: time.Now().UTC(),
		}
		if found {
			ack.OperationID, ack.Attempt = op.OperationID, op.Attempt
		}
		if err := e.backends.Fleet.PutMaintenanceAck(ctx, ack); err != nil {
			detail := "the window's baseline and its seal are established from " +
				"these, so the window will find no restart recorded for this " +
				"process"
			if !found {
				detail = "no window is open on this log, so a window opened " +
					"here before this node restarts again will find no restart " +
					"into a maintenance mode recorded for it"
			}
			log.WarnContext(ctx, "maintenance_ack_not_recorded",
				"stream", stream, "operation", ack.OperationID, "err", err,
				"detail", detail)
			continue
		}
		if !found {
			log.InfoContext(ctx, "maintenance_acknowledged", "stream", stream,
				"mode", e.mode, "incarnation", e.incarnation,
				"detail", "no window is open on this log, so this records the "+
					"restart for the baseline of the next one and binds nothing")
			continue
		}
		log.InfoContext(ctx, "maintenance_acknowledged",
			"stream", stream, "operation", op.OperationID, "attempt", op.Attempt,
			"mode", e.mode, "incarnation", e.incarnation)
	}
}

// acknowledges reports whether a node's restart into a maintenance mode is
// evidence a capacity seal needs: it holds data, or its broker is a member.
// This node's own kind, derived from its stream block, is never unknown.
func acknowledges(p placement.NodeProfile) bool {
	return p.HoldsData() || p.Broker == placement.BrokerMember
}

// seatsAdmitted is the seat host's claim gate: whether this node may take on a
// new seat now. Never in a mode that does not publish: a seat is the first
// publisher there is, and the host runs in every mode to renew presence, so
// the claim gate is where the mode is enforced. Otherwise it is hydration
// ([Engine.StateLogHydrated]): a node whose copy is behind claims nothing new.
func (e *Engine) seatsAdmitted(ctx context.Context) bool {
	return e.mode.Publishes() && e.StateLogHydrated(ctx)
}

// writeSides are this node's own write authorities over the estate, one per
// domain a surface writes: Worklet's writer, Doclet's and Chatlet's stores,
// and Gitlet's committer (pushes), change writer (changes' conversations),
// checks writer, marks (a person's code notices) and store (the repositories'
// reconcile and retention). A nil one is a domain this node does not write.
type writeSides struct {
	tracker   *worklet.Writer
	pages     *doclet.Store
	chat      *chatlet.Store
	committer *gitlet.Committer
	changes   *gitlet.ChangeWriter
	checks    *gitlet.ChecksWriter
	marks     *gitlet.Marks
	code      *gitlet.Store
}

// writeSide is this node's own write authorities, each nil where the runtime
// does not run that domain natively, and all nil in a mode that does not
// publish.
//
// The one gate for every write this node's copy makes (a seat's tools, the
// operator's MCP, the purge route, the applies, the chat prune, Gitlet's
// committer deciding a peer's push, the estate server writing for a peer) and,
// through [Engine.writers], for the router's writer: for [New]'s reason, a
// check per surface is a list, and the surface it forgets publishes during the
// window.
func (e *Engine) writeSide() writeSides {
	n := e.native.Load()
	if n == nil || !e.mode.Publishes() {
		return writeSides{}
	}
	return writeSides{tracker: n.writer, pages: n.pages, chat: n.chat,
		committer: n.committer, changes: n.changeWriter, checks: n.checksWriter,
		marks: n.marks, code: n.codeStore}
}

// Mode is what this node started for.
func (e *Engine) Mode() statelog.MaintenanceMode {
	if e.mode == "" {
		return statelog.ModeNormal
	}
	return e.mode
}

// maintenanceStreams is every stream a capacity operation could be open on:
// every registered domain's, because an operation on any one excludes this
// node from publishing at all, since a seat's tools reach every domain through
// the one estate.
func maintenanceStreams() []string {
	domains := registeredDomains()
	out := make([]string, 0, len(domains))
	for _, domain := range domains {
		out = append(out, domain.Stream().Name)
	}
	return out
}
