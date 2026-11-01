package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
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
//
// And EVERY BOOT INTO A MAINTENANCE MODE, before it acknowledges, retires the
// admission a previous process of this node left, once the fleet's presence
// shows that process is not running ([Engine.supersede]). A process that
// crashed never withdrew its own, and a claim about a process that no longer
// exists would otherwise stand in every window's way for good.
//
// # What a maintenance-mode node keeps, and what it withholds
//
// It keeps its PRESENCE LEASE. Presence is membership, not work: it says the
// process is running, and three things read it during a window — the capacity
// operation's participant set (a broker member that holds no data is found
// nowhere else), the estate's routing of a stateless node's reads to a data
// node that still serves them, and the fleet views an operator reads to see who
// is up and whom the broker still counts. So the seat host runs in every mode,
// renewing it.
//
// It withholds the two things that write the company's records: SEATS, which
// the seat host never claims here ([Engine.seatsAdmitted]), and WRITERS — this
// node's own, and the router's to a data node on a node that holds no data —
// which no surface is handed ([Engine.writers]). And everything that
// would append to a log on its own: the duties and schedulers the constructor's
// gate never starts, the object store's collector, which pins the estate
// with a barrier ([Engine.startNativeCollector]), and every barrier a read
// would append ([statelog.RefuseMaintenance]).

// ErrNotPublishing is an operator gesture that appends a record to a state log
// — an eviction, a readmission, a reanchor — asked of a node in a mode that
// publishes nothing. It wraps a sentence naming the mode and the gesture.
//
// REFUSED RATHER THAN CARRIED OUT, for the reason a linearizable read is
// ([statelog.RefuseMaintenance]): each is a record on a log, and the mode
// exists so the fleet's logs hold still while a capacity window measures them.
// A gate record is no smaller a publish than a seat's write because an
// operator asked for it. None of the three is what a window needs, either: a
// participant that will not acknowledge is excluded from the operation
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

// handshake is a boot's half of the capacity window, run BEFORE any publisher
// starts: in normal mode this process admits itself ([Engine.admit]); in a
// maintenance mode the admission this node's previous process left is retired
// ([Engine.supersede]) and this process acknowledges ([Engine.acknowledge]).
//
// ONE CALL, and the boot makes no other, so the steps of each mode are stated
// once: a maintenance boot that skipped the retirement is how a crashed
// process's admission outlived every restart into maintenance.
func (e *Engine) handshake(ctx context.Context, streams []string) error {
	e.supersede(ctx)
	if err := e.admit(ctx, streams); err != nil {
		return err
	}
	e.acknowledge(ctx, streams)
	return nil
}

// supersede withdraws the admission this node's id holds under ANOTHER
// incarnation: the one a previous process of this node wrote and never gave
// back, once the fleet's presence shows that process is not running.
//
// # What an admission is evidence of, and what refutes it
//
// That ONE PROCESS, the incarnation it names, may be publishing. An
// incarnation is minted per process, so the process booting now is one the
// admission does not name. That alone is not proof the one it names is gone:
// a second process started under a live node's id (a misconfigured duplicate,
// or a host brought up into maintenance mode before the old process stopped)
// boots exactly like this, and withdrawing on its word baselined a window past
// a publisher that was still running. So the withdrawal takes a second fact
// the fleet keeps rather than the booting process's inference: the process
// the admission names holds no PRESENCE LEASE for its node ([Engine.presenceHolders]),
// because its lease lapsed or another process of the node holds it. A running
// process renews its presence for as long as it runs, in every mode, so a
// presence held by the incarnation the admission names is that process alive,
// and its admission is left exactly where it is.
//
// Nothing else retires such a claim. An admission has no age, and the orderly
// stop that withdraws one ([Engine.withdraw]) is exactly what a crash never
// ran.
//
// # A process that crashed a moment ago
//
// Its presence lease runs out its TTL first, so a restart inside that TTL
// finds the dead process still present and leaves the claim. A window retires
// it on the same evidence once this process holds the node's presence in its
// place ([Engine.judgeAdmissions]); until then it stands in the window's way
// by name, as any admission does.
//
// # Why in the modes that enter a window, and only there
//
// A node restarted into a maintenance mode admits nothing, so it never touched
// the key and the crashed process's admission outlived it. The coordinator
// then refused the window naming a node already in maintenance mode, told the
// operator to restart it into maintenance mode, and the restart changed
// nothing. That boot is the one a window needs the claim gone by: the
// procedure restarts every node into maintenance mode before anything else,
// and a node that never comes back is excluded instead, which withdraws its
// admission too ([Engine.ExcludeParticipant]).
//
// A boot in service needs no retirement of its own. A data node's admission
// replaces the stale one under the same key ([Engine.admit]), and a node
// without `data` admits nothing and publishes nothing, so a claim its earlier
// process left (it held data then) stands in no one's way until a window, and
// that window's first restart retires it. Asked on every boot in service, it
// would be a coordination read on every node's start for a key that is almost
// never there.
//
// # Why a live process's admission is not weakened
//
// The withdrawal is CONDITIONAL on the incarnation read here, so it removes
// that one claim and never a newer process's, it never touches another node's
// key, and it never touches a claim whose process the fleet still sees
// running.
//
// A FAILURE IS LOGGED AND THE BOOT GOES ON, and so is a presence this boot
// cannot read, which leaves the claim: nobody can say its process is gone. A
// node in a maintenance mode publishes nothing, so refusing its boot over a
// key it could not clear would cost the window the one participant it needs
// to hear from, and the stale admission then does what it always did: it
// stands in the window's way, and the refusal names the node.
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

// presenceHolders is which process holds each node's presence lease, as the fleet
// reads it now: the node's id to the incarnation that owns the lease (the seat
// host takes it under [Engine.incarnation]). A node absent from the answer
// holds none: its last process's lease lapsed, or no process of it has taken
// one yet. It is the evidence an admission is judged by, because a running
// process renews its presence in every mode for as long as it runs.
//
// UNREADABLE IS AN ERROR, never an empty fleet, because an empty answer reads
// as every admission's process being gone. A node with no lease store (a test
// that builds the engine around a fleet register alone) holds nobody's
// presence.
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
//
// A SENTINEL, because the caller's answer is specific and is not a retry: the
// fleet is mid-window, and this node joins it when the window closes or when
// an operator restarts it into the mode its window's phase is in.
type ErrExcluded struct {
	Stream      string
	OperationID string
	Phase       coord.MaintenancePhase
	By          string
}

// Mode is the mode a node joins this window in: maintenance mode while it is
// at `opened`, where nothing has been sent and the next step is a request only
// that mode sends, and seal mode from `baselined` on, where a request may be
// outstanding and every next step is the seal restart's.
//
// THE PHASE DECIDES, never a fixed mode: told maintenance mode at every phase,
// a node restarted that way during the seal wrote an acknowledgement the seal
// refuses, and cost the fleet one more restart of it.
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
	if !e.profile.HoldsData() {
		// NO STATE-LOG PUBLISHER TO ADMIT: this node's seats write through
		// a data node, which is admitted in its own right. An admission
		// here would be a publisher a capacity operation waited on for
		// ever — and that holds for a node whose broker is a member too,
		// since what its broker queues is retired by the restart it
		// ACKNOWLEDGES, not by anything an admission says.
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
//
// ON A SHARE OF ITS OWN ([Engine.admissionShare]) and never a step of the
// stop's allowance ([seat.StopBudget]). Every step on the allowance falls back
// to a lease lapsing on its TTL, and an admission has no TTL — it lives in a
// register with no age, because an expiring admission hides a publisher — so
// one this misses stays until the node restarts under the same id or an
// operator excludes it, which is what `admission_not_withdrawn` tells them. On
// the allowance, a store that had stopped answering for the give-backs before
// it, and was answering again by now, left the one round trip with nothing to
// fall back on no time at all.
func (e *Engine) withdraw(ctx context.Context) error {
	if e.backends == nil || e.backends.Fleet == nil || e.mode != statelog.ModeNormal {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.admissionShare())
	defer cancel()
	return e.backends.Fleet.ForgetAdmission(ctx, e.id, e.incarnation)
}

// admissionShare is the part of a stop's coordination time reserved for
// withdrawing this node's admission ([Engine.withdraw]): a third of the stop's
// allowance ([seat.StopAllowance]) of this node's lease TTL — five seconds at
// the shipped 45 s — and the steps on the allowance get the other two thirds
// ([Engine.stopping]).
//
// CARVED OUT OF THE ALLOWANCE RATHER THAN ADDED BESIDE IT, so a stop against a
// store that answers nothing still spends one allowance in total, which is
// the reason there is one: the give-backs fail inside their two thirds and the
// withdrawal inside its third, where a bound of its own beside the allowance
// would make that stop one share longer.
//
// A THIRD because the withdrawal is two round trips — a read from the
// register's leader and a conditional purge — and at the shipped TTL a third
// is the five seconds the JetStream client gives one request with no deadline
// of its own, which a store answering at all answers in milliseconds. The two
// thirds left are ten seconds there for the presence, the seats and the
// duties, each also milliseconds of work on a store that answers.
func (e *Engine) admissionShare() time.Duration {
	return seat.StopAllowance(e.stopTTL()) / 3
}

// acknowledge is the maintenance-mode half: this node's evidence that its
// process restarted, for each log's window.
//
// # Why every mode but normal writes one
//
// The seal is established only from SEAL mode, because that is the mode that
// cannot write configuration. But a coordinator in maintenance mode needs to
// know who is participating before it can baseline, and an acknowledgement
// carrying its own mode is what lets one record answer both questions: the
// barrier filters on the mode, and the participant set is read from the same
// place.
//
// # And every node whose restart is evidence
//
// A DATA NODE, whose process publishes records, AND A BROKER MEMBER, whose
// broker holds requests queued against the log whatever this node's roles —
// the two the participant set counts ([Engine.capacityParticipants]). A member
// that holds no data and did not acknowledge would be named a participant the
// seal waits on for ever, since nothing else it could write says it restarted.
//
// # One per log
//
// A window is per log, and a fleet resizing two logs restarts once for both,
// so the boot acknowledges each log's window under that log's own key
// ([coord.MaintenanceAckKey]). Kept one per node, the acknowledgement of the
// second window replaced the first's, and the first window read "acknowledged
// another operation" from every node for as long as both were open.
//
// # And a log with no window open still says it restarted
//
// A fleet restarted into maintenance mode before the verb ran is the ordinary
// first step of the procedure, so this boot finds no window, and it used to
// write nothing. The baseline then found no record for any node but the
// coordinator, refused every other one as never having restarted into a
// maintenance mode, and sent the operator to restart a fleet that just had:
// a fourth fleet-wide restart the procedure does not have. So a log the boot
// read with no window open gets a WINDOWLESS acknowledgement
// ([coord.MaintenanceAck.Windowless]): the process and its mode, bound to
// nothing. A baseline reads which process a node is from it, and no seal takes
// it, so the evidence the seal stands on is exactly what it was. A log the boot
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
			// WHAT THE MISSING RECORD COSTS DEPENDS ON WHETHER A WINDOW IS
			// OPEN: an open one's baseline and seal see no restart from this
			// process, and with none open it is the next window's baseline
			// that finds nothing for this node until it restarts again.
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
// This node's own kind is always known — it is derived from its stream block —
// so there is no unknown to resolve here.
func acknowledges(p placement.NodeProfile) bool {
	return p.HoldsData() || p.Broker == placement.BrokerMember
}

// seatsAdmitted is the seat host's claim gate: whether this node may take on a
// NEW seat now.
//
// NEVER IN A MODE THAT DOES NOT PUBLISH. A seat is the first publisher there
// is — its mailbox attaches, its turns run and its tools write the company's
// records — and the host that claims seats is started in every mode, because it
// is also what renews this node's presence. So the claim half is where the mode
// is enforced: a maintenance-mode node boots holding nothing, and a gate that
// never opens keeps it that way, while the presence the same host renews goes
// on saying the process is running.
//
// Otherwise it is hydration ([Engine.StateLogHydrated]), the one admission
// gate: a node whose copy of the company's records is behind claims nothing
// new until it has caught up.
func (e *Engine) seatsAdmitted(ctx context.Context) bool {
	return e.mode.Publishes() && e.StateLogHydrated(ctx)
}

// writeSides are this node's own write authorities over the estate, one per
// domain a surface writes: Worklet's writer, Doclet's store and Chatlet's
// store, and the code host's — the committer deciding the pushes this node is
// asked to commit, changes the writes of changes' conversations it is asked
// beside them, checks the writes of repositories' checks, marks a person's
// code notices' marks, and code the lifecycle store the repositories'
// reconcile and retention write through. A nil one is a domain this node does
// not write.
type writeSides struct {
	tracker   *worklet.Writer
	pages     *doclet.Store
	chat      *chatlet.Store
	committer *code.Committer
	changes   *code.ChangeWriter
	checks    *code.ChecksWriter
	marks     *code.Marks
	code      *code.Store
}

// writeSide is this node's own write authorities, each nil where the runtime
// does not run that domain natively, and all of them nil in a mode that does
// not publish.
//
// ONE GATE FOR EVERY WRITE THIS NODE'S OWN COPY MAKES — a seat's tools, the
// operator's MCP, the purge route, the chart's, the containers' and the unit
// rooms' applies, the chat prune, the code host's committer deciding a push
// for whichever node received it, and the estate server writing for a peer —
// and, through [Engine.writers], for whether a seat's tools, the operator's
// MCP and the write routes are handed the router's writer at all. Each is
// handed its writer from here, so a maintenance-mode node hands out none — for
// [New]'s reason for gating its publishers in one place: a check per surface
// is a list somebody maintains, and the surface it forgets is a publish during
// the window the mode exists to rule out, into a log whose usage a resize is
// being decided against. A committer handed to the estate server here and not
// there would be exactly that surface: a peer's push decided and appended on a
// node whose operator restarted it to append nothing.
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

// maintenanceStreams is every stream a capacity operation could be open on.
//
// EVERY REGISTERED DOMAIN'S, because an operation on any one of them excludes
// this node from publishing at all: the appliers write into one estate and a
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
