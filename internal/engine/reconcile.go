package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracing"
)

// Reconciler converges this node on the activation pointer.
//
// POLLED, not subscribed: every node re-reads the pointer, so a node that was
// down for an activation catches up on its next tick by reading current state
// rather than replaying an event it missed. A competing-consumer subscription
// would have exactly one node apply each revision.
//
// It also RECORDS per-node apply status: only the pointer and that status
// together tell "behind because propagation takes a moment" from "behind
// because I cannot apply this", which need opposite responses.
type Reconciler struct {
	engine  *Engine
	configs *store.Configs
	plane   coord.Plane
	queue   Nudger
	nodeID  string

	// cipher is the ENGINE's keyring, never one handed in: it opens and
	// AUTHENTICATES a peer's revision, whose body anything reaching the
	// broker can write. See [Engine.NewReconciler].
	cipher secrets.Cipher

	// progress is this node's convergence state, published as ONE value.
	//
	// Written only by the tick (Run's loop or the one-shot harness path,
	// never both, which [Engine.following] enforces) and read concurrently
	// by the seat heartbeat, the readiness probe and the runtime-state
	// handlers.
	//
	// ONE POINTER, not separate atomics: its fields are read together and
	// must agree. DecidePosture comparing an applied epoch from one moment
	// with an attempt count from another can conclude "converged but still
	// retrying", a state this node was never in.
	progress atomic.Pointer[applyProgress]

	// served is the revision this node's engine RUNS, at the instant of the
	// activation it was applied from: [ReconcilerOptions.Booted] until an
	// apply succeeds, then what that apply installed. Nil while the engine
	// runs no company.
	//
	// It is the only thing [Reconciler.serve] moves the boot row to, so a
	// node never boots on a revision it refused. NOT the pointer's revision
	// at the applied epoch: a recreated pointer can reuse an epoch for
	// another revision. Written by the tick; [Reconciler.Serving] reads it
	// from anywhere.
	served atomic.Pointer[store.Revision]

	// booting is the boot row's revision as this reconciler last READ or
	// WROTE it, empty where the row holds none. A SECOND FACT, not a reading
	// of served, since the boot-row write can fail: the apply record carries
	// it ([coord.NodeApply.Boots]) so the operator keeps the sealed
	// credentials of the revision a node would boot on, not the one it runs.
	//
	// NEVER UNKNOWN: read as the reconciler is built ([Engine.NewReconciler]
	// fails otherwise), then moved only by a read or write of the row that
	// succeeded ([Reconciler.serve]), so "could not read" is never published
	// as "holds none". The last answer stays true: while the engine runs this
	// is the row's only writer (offline commands are refused, and the boot
	// publish ran before it was built). Read through [Reconciler.Boots].
	booting atomic.Pointer[string]

	// scratch is whether this node's database is DISCARDED at every start
	// ([store.DB.Scratch]): such a node boots on nothing however many
	// revisions it applies, and [Reconciler.Boots] says so. The handle's
	// answer, never a caller's option, so it cannot disagree with the open.
	scratch bool

	// recorded is the last apply outcome written to the fleet view, which
	// every later tick restates ([Reconciler.refresh]). Tick-only, but atomic
	// because the tick is not pinned to one goroutine.
	recorded atomic.Pointer[coord.NodeApply]

	// decided is the last posture computed, for [Reconciler.Admits]. The
	// heartbeat and readiness probe call [Reconciler.Posture] and can pay its
	// two coordination reads; admission is consulted per inbound delivery
	// and per scheduler tick, where a read would put a round trip in front of
	// every webhook.
	//
	// Nil means nothing decided yet, which ADMITS: a node that has not
	// completed one pass is not evidence that it is behind.
	decided atomic.Pointer[configplane.Posture]

	// nudged carries an activation from the broadcast subscription to the
	// loop. Buffered at ONE and written non-blocking, so an activation storm
	// collapses into one early wake: the loop re-reads the pointer, so N
	// nudges and one lead to the same place.
	nudged chan struct{}

	// onApply observes every apply's outcome (the command logs them). The
	// API does not learn whether the node is configured here; it reads the
	// engine's live epoch.
	onApply func(epoch int64, status configplane.ApplyStatus)

	// log receives every line the reconciler writes.
	log *slog.Logger

	now func() time.Time
}

// ReconcilerOptions configure a reconciler.
type ReconcilerOptions struct {
	// Store holds the revision payloads and the node's boot row. Required.
	// Whether its writes outlive this start is the handle's answer
	// ([store.DB.Scratch]), not an option.
	Store *store.DB

	// Fleet is where the activation pointer and the per-node apply status
	// live. Required: they are what the fleet converges on, and a node
	// reading its own database would converge on itself.
	Fleet coord.Plane

	// Queue publishes this node's apply outcome as a durable event and
	// brings activations in as a nudge. Required: the coordination record
	// ages out in a minute, so without a publisher a bad rollout leaves no
	// post-mortem trail, and silently.
	Queue Nudger

	// Booted is the revision engine.New started on, at its activation
	// instant; zero for an engine that started unconfigured or on a company
	// no revision names. This node serves it, and its fleet-view row says
	// so, until its first apply succeeds.
	Booted store.Revision

	// NodeID identifies this node's row in the apply status. Required: the
	// table upserts on it, so an empty one makes such nodes share one row.
	NodeID string

	// OnApply observes every outcome.
	OnApply func(epoch int64, status configplane.ApplyStatus)

	// Log receives the reconciler's lines, including the admission warnings
	// for an applied revision. Nil is the engine's component logger. A
	// field, not the package logger alone, so a test can assert those
	// warnings without racing every parallel test that logs.
	Log *slog.Logger

	// Now is the reconciler's clock: the lag it measures on this node and,
	// in UTC, every instant it writes for its peers or compares with
	// theirs. Injectable so a test can pin the freshness window and move
	// the lag. Nil is time.Now, monotonic reading included.
	Now func() time.Time
}

// Nudger is the queue surface this loop uses: it publishes what it did, and
// listens for an activation so a change lands in milliseconds rather than at
// the next poll.
//
// The subscription is an EPHEMERAL BROADCAST, never a consumer group: every
// node must hear every activation, and a group hands each to one node. Losing
// a nudge costs one poll interval, never a revision, because the poll asks
// rather than replays.
type Nudger interface {
	queue.Publisher
	SubscribeStream(ctx context.Context, topicPattern string, h queue.StreamHandler) (queue.Unsubscribe, error)
}

// ErrNoStore reports a reconciler built without somewhere to read revisions.
var ErrNoStore = errors.New("engine: reconciler needs a store")

// ErrNoPlane reports a reconciler built without a coordination plane. Its own
// sentinel because the fix is a coordination store, not a database.
var ErrNoPlane = errors.New("engine: reconciler needs a coordination plane")

// ErrNoPublisher reports a reconciler built with nowhere to publish its apply
// outcome; the fix is a queue.
var ErrNoPublisher = errors.New("engine: reconciler needs a publisher")

// ErrNoKeyring reports a reconciler on an engine that holds no keyring, which
// only an Engine built by hand can be: [New] refuses to start without one.
var ErrNoKeyring = errors.New("engine: reconciler needs the engine's keyring " +
	"(secrets.keys): a peer's revision is authenticated by its seal, and a " +
	"reconciler without one would apply a body anything reaching the " +
	"coordination store could have written")

// NewReconciler builds the loop.
//
// THE KEYRING IS THE ENGINE'S, with no option to hand in another or none: a
// missing keyring would read every payload as plaintext, the forged document
// the seal exists to refuse. The engine holds the one keyring Tier A names,
// which every revision this fleet writes is sealed under.
//
// IT READS WHAT THIS NODE BOOTS ON before it returns, and fails when it
// cannot, as the boot fails on an unreadable store: every record it writes
// names that revision ([coord.NodeApply.Boots]), and learning it only on the
// first tick would publish a failed read as "boots on nothing".
func (e *Engine) NewReconciler(ctx context.Context, opts ReconcilerOptions) (*Reconciler, error) {
	if e.cipher == nil {
		return nil, ErrNoKeyring
	}
	if opts.Store == nil {
		return nil, ErrNoStore
	}
	if opts.Fleet == nil {
		return nil, ErrNoPlane
	}
	if opts.Queue == nil {
		return nil, ErrNoPublisher
	}
	if opts.NodeID == "" {
		return nil, fmt.Errorf("engine: reconciler needs a node id")
	}
	now := opts.Now
	if now == nil {
		// time.Now itself, NOT its UTC: UTC strips the monotonic reading,
		// and the lag measured here must not move on an NTP step or a
		// resumed VM. What peers read goes through [Reconciler.stamp].
		now = time.Now
	}
	logger := opts.Log
	if logger == nil {
		logger = log
	}
	r := &Reconciler{
		log:     logger,
		nudged:  make(chan struct{}, 1),
		engine:  e,
		configs: opts.Store.Configs(),
		plane:   opts.Fleet,
		queue:   opts.Queue,
		nodeID:  opts.NodeID,
		cipher:  e.cipher,
		onApply: opts.OnApply,
		scratch: opts.Store.Scratch(),
		now:     now,
	}
	if opts.Booted.ID != "" {
		booted := opts.Booted
		r.served.Store(&booted)
	}
	// THE ROW ITSELF, never [ReconcilerOptions.Booted] (what the engine
	// STARTED on): an `-import-company` boot starts on nothing while its row
	// names what the node ran before.
	boot, _, err := r.configs.Boot(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: reconciler: read the revision this node "+
			"boots on from its own database: %w", err)
	}
	r.boots(boot.ID)
	// Recorded on the engine once it can answer: [Engine.CodeUnserved] and
	// [Engine.servedFleetRevision] read its progress.
	e.reconciler.Store(r)
	return r, nil
}

// applyProgress is what the tick has achieved against what it is aiming at,
// stored and replaced whole so every reader sees one instant's progress.
//
// IT HOLDS NO OUTCOME BEFORE IT IS KNOWN: readers on other goroutines read it
// mid-apply, so a count raised before an attempt reads as that attempt's
// failure. Before an attempt only WHEN things began is written.
type applyProgress struct {
	// applied is the epoch this node is serving, 0 before its first
	// successful apply.
	applied int64

	// target is the epoch the pointer named when this node last saw it.
	target int64

	// attempts counts the attempts at target that CONCLUDED IN FAILURE,
	// reset when target moves and when one succeeds. Per epoch, so
	// re-activating a fixed revision resets the budget (the runbook's fix).
	// Counted when an attempt ends: see [configplane.FleetView.Attempts].
	attempts int

	// behindSince is when this node first saw target while serving an older
	// epoch, zero while it is not behind: it confirms lag by TIME while an
	// apply still runs, which a tick counter cannot, since a hung apply holds
	// its tick ([configplane.LagGrace]). PER EPOCH, restarted when target
	// moves: carried over, lag waited out on a failed revision would count
	// against the corrected one, and every node applying the fix would shed.
	behindSince time.Time

	// attemptStarted is when the running attempt began, zero between
	// attempts. An epoch the tick has NOT YET SEEN is late by this: a tick
	// held in an attempt at an older epoch cannot begin the new one, so a
	// hung attempt stays confirmed lag while a quick one is propagation.
	attemptStarted time.Time

	// targetRead is whether this node has read target's revision, and
	// targetCode whether it sets code.backend: gitlet: what tells a node that
	// does not serve Gitlet because the company hosts no code from one not
	// serving it YET ([Engine.CodeUnserved]). Reset with target; set once an
	// attempt has decoded the revision, before the apply that may fail.
	targetRead, targetCode bool
}

// behindFor is how long the progress has been behind on target as of now:
// since the tick first saw that epoch, or, when it has not, since the attempt
// holding the tick began. now carries the monotonic reading, so a wall-clock
// step does not move it ([Engine.NewReconciler]).
func (p applyProgress) behindFor(target int64, now time.Time) time.Duration {
	since := p.behindSince
	if target != p.target {
		since = p.attemptStarted
	}
	if since.IsZero() {
		return 0
	}
	return max(now.Sub(since), 0)
}

// failedAttempts is how many attempts at target have concluded in failure:
// none at an epoch the tick has not yet seen, whose count is the PREVIOUS
// target's and would read a node that exhausted its retries on a bad revision
// as stuck on the corrected one before it tried it.
func (p applyProgress) failedAttempts(target int64) int {
	if target != p.target {
		return 0
	}
	return p.attempts
}

// stamp is the time as other nodes read it: the wall clock, in UTC. Every
// instant this node writes for its peers, or compares with one they wrote,
// goes through here — a monotonic reading means nothing in another process.
func (r *Reconciler) stamp() time.Time { return r.now().UTC() }

// snapshot reads the progress. The zero value is a node that has not ticked.
func (r *Reconciler) snapshot() applyProgress {
	if p := r.progress.Load(); p != nil {
		return *p
	}
	return applyProgress{}
}

// publish replaces the progress. Callers are the tick alone, which is why a
// plain load-modify-store needs no compare-and-swap.
func (r *Reconciler) publish(p applyProgress) { r.progress.Store(&p) }

// noteTargetCode records whether the revision of epoch sets code.backend:
// gitlet. Tick-only, like every progress writer; an epoch the progress has
// moved past is not recorded.
func (r *Reconciler) noteTargetCode(epoch int64, hosts bool) {
	p := r.snapshot()
	if p.target != epoch {
		return
	}
	p.targetRead, p.targetCode = true, hosts
	r.publish(p)
}

// Applied is the epoch this node is serving, or 0 before its first apply.
func (r *Reconciler) Applied() int64 { return r.snapshot().applied }

// Attempt is this node's last concluded apply attempt (epoch, revision,
// outcome), false before this process has concluded one: the record its peers
// read ([Reconciler.record]). A failed attempt leaves [Reconciler.Applied]
// where it was, so the applied epoch alone cannot tell applying from refused.
func (r *Reconciler) Attempt() (coord.NodeApply, bool) {
	if row := r.recorded.Load(); row != nil {
		return *row, true
	}
	return coord.NodeApply{}, false
}

// Serving is the id of the revision this node's engine runs, or empty while it
// runs none. See [Reconciler.served].
func (r *Reconciler) Serving() string {
	if rev := r.served.Load(); rev != nil {
		return rev.ID
	}
	return ""
}

// Boots is the id of the revision this node would START ON if it restarted
// now, or empty for none. See [Reconciler.booting].
//
// EMPTY ON A SCRATCH NODE whatever its boot row holds: that database is
// deleted at the next start, and naming the revision would have a reader keep
// what this node has no use for.
func (r *Reconciler) Boots() string {
	if r.scratch {
		return ""
	}
	// Never nil on a reconciler [Engine.NewReconciler] built, which read the
	// row before returning it.
	if id := r.booting.Load(); id != nil {
		return *id
	}
	return ""
}

// boots records what the boot row holds, empty where it holds none, as a read
// or a write of it that SUCCEEDED found it — never on a failure, which leaves
// the last answer standing. See [Reconciler.booting].
func (r *Reconciler) boots(revisionID string) { r.booting.Store(&revisionID) }

// Posture is what this node should do about the gap between what it has
// applied and what the pointer names.
//
// Read on every call rather than cached: the readiness probe asks it, and a
// cached posture reports healthy through the window it stopped being so. It
// FAILS OPEN to serve on an unreadable plane; see [Reconciler.ReadPosture].
func (r *Reconciler) Posture(ctx context.Context) configplane.Posture {
	posture, err := r.ReadPosture(ctx)
	if err != nil {
		return configplane.PostureServe
	}
	return posture
}

// ReadPosture is [Reconciler.Posture] without its fail-open answer: the
// posture this node concluded, or the error that kept it from reading the
// control plane.
//
// FOR THE PEERS. The readiness probe and admission want fail-open, or a
// database blip takes every node out of rotation; the presence heartbeat must
// not broadcast that serve as a confident healthy posture on exactly the node
// an operator is looking for. The gate records serve here too, so which of the
// two a caller uses never changes what [Reconciler.Admits] answers.
func (r *Reconciler) ReadPosture(ctx context.Context) (configplane.Posture, error) {
	// BOUNDED by [ProbeReadBudget]: this is on the liveness and readiness
	// path, and the view's iteration of the fleet's keys is outside the NATS
	// client's per-API timeout, so a wedged broker would hang the probe. A
	// slow plane degrades to the error below, which Posture answers with
	// fail-open serve.
	ctx, cancel := context.WithTimeout(ctx, ProbeReadBudget)
	defer cancel()

	view, err := r.view(ctx)
	if err != nil {
		// FAILS TO SERVE, not to stuck: a node that cannot read the store
		// cannot know whether it is behind, and serve keeps a working
		// company working.
		r.log.WarnContext(ctx, "posture_unknown", "error", err,
			"detail", "cannot read the control plane; assuming this node is current")
		r.decide(configplane.PostureServe)
		return "", fmt.Errorf("read this node's posture: %w", err)
	}
	return r.decide(configplane.DecidePosture(view)), nil
}

// ProbeReadBudget bounds one coordination read made for a health probe: the
// posture read, and the envelope's presence count, which the API runs BESIDE
// it under this same value.
//
// ONE VALUE because they are one probe's reads. An EIGHTH of the reconcile
// cadence, so a probe never outlives the tick that would correct it, and well
// inside an orchestrator's default 5-second liveness timeout. Failing either
// read is SAFE: an unreadable plane is PostureServe, and an unread presence
// count is an absent `nodes`, which already means "cannot say".
const ProbeReadBudget = configplane.ReconcileInterval / 8

// decide records a posture for the admission gate and returns it.
//
// Every path out of [Reconciler.Posture] goes through here, fail-open
// included: recording only successful reads would leave a node that shed once
// and then lost the plane refusing work for the whole outage, on evidence it
// can no longer check.
func (r *Reconciler) decide(p configplane.Posture) configplane.Posture {
	r.decided.Store(&p)
	return p
}

// Admits reports whether this node should take new inbound work.
//
// It reads the posture last decided ([Reconciler.decided]) and fails OPEN: no
// decision yet admits, and an unreadable plane decides serve. Only a posture
// that concluded shed or stuck refuses, the set /ready fails on.
func (r *Reconciler) Admits() bool {
	p := r.decided.Load()
	return p == nil || p.ServesTraffic()
}

// view assembles what the posture rule needs.
func (r *Reconciler) view(ctx context.Context) (configplane.FleetView, error) {
	target, found, err := r.plane.Target(ctx)
	if err != nil {
		return configplane.FleetView{}, err
	}
	// ONE SNAPSHOT, so the applied epoch, attempt count and status describe
	// one moment. The target is read FRESH and may be newer than the
	// progress's, so everything from the progress is read against it.
	progress := r.snapshot()
	applied := progress.applied
	if !found {
		// No activation: a node with nothing to converge on is current,
		// and an unconfigured deployment is not a diverged one.
		return configplane.FleetView{TargetEpoch: applied, AppliedEpoch: applied,
			SelfStatus: configplane.StatusOK}, nil
	}
	ok, reported, err := r.peerHealth(ctx, target.Epoch)
	if err != nil {
		return configplane.FleetView{}, err
	}
	view := progress.selfView(target.Epoch, r.now())
	view.PeersOK, view.PeersReported = ok, reported
	return view, nil
}

// selfView is this node's half of the fleet view against target as of now,
// every field read from this one progress; the peers are the other half
// ([Reconciler.view]).
//
// ONE CONSTRUCTION for the posture and for the engine's irreversible
// judgements ([Engine.servedFleetRevision]), which tell applying from failed
// by the same rule ([configplane.FleetView.LagConfirmed]), so the two never
// disagree.
func (p applyProgress) selfView(target int64, now time.Time) configplane.FleetView {
	attempts := p.failedAttempts(target)
	return configplane.FleetView{
		TargetEpoch:  target,
		AppliedEpoch: p.applied,
		SelfStatus:   selfStatus(attempts),
		// MEASURED: probes and the heartbeat ask WHILE an apply runs,
		// which reads as propagation (wait) until it lasts longer than
		// propagation explains.
		BehindFor: p.behindFor(target, now),
		Attempts:  attempts,
	}
}

// selfStatus is this node's own outcome for the current target: ok with no
// failed attempt (applied, or no attempt concluded yet), error once one has
// failed, which stays its outcome while a retry runs. Derived rather than
// stored: the store row is what peers read, and an in-memory copy is one
// restart from disagreeing with it.
func selfStatus(failedAttempts int) configplane.ApplyStatus {
	if failedAttempts == 0 {
		return configplane.StatusOK
	}
	return configplane.StatusError
}

// Tick is one poll: read the pointer, and apply the revision it names when
// this node is not already on it. It RE-READS rather than replaying, so a node
// down for an activation catches up on its next tick.
//
// ONE TICK AT A TIME PER ENGINE, from the pointer read to the record of what
// the node serves ([Engine.following]): the boot row follows the engine's
// applies, so applies out of pointer order would walk it back.
func (r *Reconciler) Tick(ctx context.Context) error {
	r.engine.following.Lock()
	defer r.engine.following.Unlock()
	target, found, err := r.plane.Target(ctx)
	if err != nil {
		return fmt.Errorf("engine: reconcile: %w", err)
	}
	if !found {
		// Nothing activated: not an error, and not a state to report.
		return nil
	}
	progress := r.snapshot()
	if target.Epoch != progress.target {
		// A new target resets the attempt budget (re-activating the fixed
		// revision is the documented recovery) and restarts the lag clock:
		// this node is judged on the new epoch alone. A hung attempt holds
		// the tick; it is late by [applyProgress.attemptStarted].
		progress.target, progress.attempts, progress.behindSince = target.Epoch, 0, time.Time{}
		progress.targetRead, progress.targetCode = false, false
		if progress.applied != target.Epoch {
			progress.behindSince = r.now()
		}
		r.publish(progress)
	}
	if progress.applied == target.Epoch {
		// CONVERGED: each tick repairs the apply's best-effort writes, so
		// a failed boot-row write does not bring the node back on what it
		// served before. The boot row FIRST, so the fleet row restated
		// this tick reports the repair now ([Reconciler.booting]).
		r.serve(ctx)
		r.refresh(ctx, target)
		r.note(ctx, target)
		return nil
	}
	if progress.attempts >= configplane.MaxApplyAttempts {
		// Out of retries on this epoch: the posture already reads this
		// node as failed, and the fleet's row still follows the pointer (a
		// refused revision is still the company). The boot row is STILL
		// REPAIRED, first as above: a node refusing the fleet's revision
		// never converges again, so a boot-row write failed on the PREVIOUS
		// revision would stay unmade on the node about to be restarted.
		r.serve(ctx)
		r.refresh(ctx, target)
		r.note(ctx, target)
		return nil
	}
	// A FAILED BOOT-ROW WRITE IS REPAIRED BEFORE THE ATTEMPT TOO, before the
	// apply moves what this node serves, so a failed retry leaves the row on
	// what the node runs meanwhile. The read is of this node's own database.
	r.serve(ctx)
	// NO OUTCOME IS PUBLISHED BEFORE THE ATTEMPT, only that it began: a
	// count raised here reads as a failure for the whole of an apply that
	// then succeeds ([applyProgress]).
	progress.attemptStarted = r.now()
	r.publish(progress)
	return r.apply(ctx, target)
}

// apply loads, opens, parses and publishes one revision, then records what
// happened for the rest of the fleet to read.
//
// THE OUTCOME REACHES THIS NODE'S PROGRESS FIRST, with what it serves, before
// the record for its peers (two network writes): a node already serving the
// epoch must not report itself behind meanwhile.
//
// THE BOOT ROW MOVES BEFORE THAT RECORD, so a clean apply's record names the
// new boot revision on the same tick rather than at the next
// [Reconciler.refresh]. The order decides only that: the record never names a
// boot revision the store lacks, because [Reconciler.booting] moves only on a
// read or write of the row that succeeded.
func (r *Reconciler) apply(ctx context.Context, target coord.Activation) error {
	status, revision, applied, err := r.applyRevision(ctx, target)
	progress := r.snapshot()
	progress.attemptStarted = time.Time{}
	if status == configplane.StatusOK {
		progress.applied, progress.attempts, progress.behindSince = target.Epoch, 0, time.Time{}
		r.served.Store(&revision)
	} else {
		progress.attempts++
	}
	r.publish(progress)
	if status == configplane.StatusOK {
		r.serve(ctx)
	}
	r.record(ctx, target, status, applied, err)
	if r.onApply != nil {
		r.onApply(target.Epoch, status)
	}
	return err
}

// note keeps the fleet's row on the pointer's revision: the repair of a keep
// that failed while the revision was applied ([Reconciler.keep]).
//
// It reads this node's own store, and the coordination store only where the
// row is behind, so a converged node pays one local read a tick.
func (r *Reconciler) note(ctx context.Context, target coord.Activation) {
	seen, found, err := r.configs.Fleets(ctx)
	if err != nil {
		r.log.WarnContext(ctx, "fleet_revision_unread", "revision", target.RevisionID,
			"error", err, "detail", "this node's record of the fleet's revision could "+
				"not be read; the next tick checks again")
		return
	}
	if found && seen.ID == target.RevisionID &&
		store.EncodeTime(seen.ActivatedAt) >= store.EncodeTime(target.At) {
		return
	}
	revision, _, err := r.fleetRevision(ctx, target)
	if err != nil {
		r.log.WarnContext(ctx, "fleet_revision_not_kept", "revision", target.RevisionID,
			"error", err, "detail", "this node's record of the fleet's revision could "+
				"not be written; a lost pointer is restored from the last one it kept")
		return
	}
	// AUTHENTICATED BEFORE IT IS KEPT, as on the apply path: a body that
	// does not open under this keyring is refused there, and is not this
	// fleet's revision here either.
	if _, err := secrets.Open(r.cipher, revision.Payload); err != nil {
		return
	}
	r.keep(ctx, revision)
}

// keep records revision as the fleet's, as this node last saw it.
//
// BEST EFFORT: what the fleet is on is the pointer, and this row is only what
// the node restores when the pointer is gone, so a failure costs the restore
// one revision of currency and the next converged tick tries again
// ([Reconciler.note]).
func (r *Reconciler) keep(ctx context.Context, revision store.Revision) {
	if err := r.configs.SawFleet(ctx, revision); err != nil {
		r.log.WarnContext(ctx, "fleet_revision_not_kept", "revision", revision.ID,
			"error", err, "detail", "this node's record of the fleet's revision could "+
				"not be written; a lost pointer is restored from the last one it kept")
	}
}

// serve makes what the engine runs what this node boots on: THE ONLY ONLINE
// WRITER OF THE BOOT ROW, writing only [Reconciler.served], so a node never
// boots on a revision it refused or out of pointer order ([Engine.following]).
// Called after each successful apply and on EVERY tick, so a failed write is
// repaired next tick, even on a node refusing the fleet's revision. The same revision at another instant is served again: re-activation
// rotates credentials, and [store.Revision.ServedAt] stamps the chart at boot.
// An offline OFFER the boot did not publish is replaced here, and logged.
//
// It also READS the row every tick, before the early return, to learn
// [Reconciler.Boots] rather than infer it from what the node serves. A read
// that fails teaches nothing: the last answer stands, and nothing is written.
func (r *Reconciler) serve(ctx context.Context) {
	served := r.served.Load()
	boot, found, err := r.configs.Boot(ctx)
	if err != nil {
		r.log.WarnContext(ctx, "boot_revision_unread", "revision", r.Serving(),
			"boots_on", r.Boots(), "error", err,
			"detail", "this node could not read what it boots on, so whether it is "+
				"what it serves is unknown; the fleet goes on being told the revision "+
				"it last read there, and the next tick checks again")
		return
	}
	r.boots(boot.ID)
	// Only a revision this engine APPLIED is written, so the guard is the
	// applied epoch: before the first apply the node serves what it booted
	// on, which the row names already, and an offline offer there is still
	// published by a later start ([store.Offer]); writing would clear it.
	if served == nil || r.snapshot().applied == 0 {
		return
	}
	if found && boot.ID == served.ID && boot.Offer == nil &&
		store.EncodeTime(boot.ServedAt) == store.EncodeTime(served.ActivatedAt) {
		return
	}
	// AN OFFER THIS NODE NOW SERVES WAS PUBLISHED, whether or not the boot
	// cleared it after: Serve clears it.
	if found && boot.Offer != nil && boot.ID != served.ID {
		r.log.WarnContext(ctx, "offline_revision_not_published", "revision", boot.ID,
			"built_on", boot.Offer.String(), "fleet_runs", served.ID,
			"reason", configplane.WithheldOfferReason(*boot.Offer),
			"detail", "this node runs the fleet's revision now and boots on it; the "+
				"offline change stays in its history",
			"hint", configplane.WithheldOfferRemedy(boot.ID, boot.Offer.Base))
	}
	if err := r.configs.Serve(ctx, *served); err != nil {
		r.log.WarnContext(ctx, "boot_revision_not_moved", "revision", served.ID,
			"error", err, "detail", "this node runs the revision, and would boot on the "+
				"one before it; the next tick tries again")
		return
	}
	r.boots(served.ID)
	previous := ""
	if found {
		previous = boot.ID
	}
	r.log.InfoContext(ctx, "boot_revision_moved", "revision", served.ID,
		"previous", previous, "activated_at", served.ActivatedAt)
}

// applyRevision is the part that can fail, separated so every exit records.
// It answers the revision it applied, at the pointer's instant, for the boot
// row to follow.
//
// The subsystem list is empty on every exit before [Engine.Apply] and on its
// stopping-node refusal: empty means "nothing changed here", and the error's
// prefix says which.
func (r *Reconciler) applyRevision(ctx context.Context, target coord.Activation) (
	configplane.ApplyStatus, store.Revision, []string, error,
) {
	revision, local, err := r.fleetRevision(ctx, target)
	if err != nil {
		return configplane.StatusError, store.Revision{}, nil, err
	}
	document, err := secrets.Open(r.cipher, revision.Payload)
	if err != nil {
		return configplane.StatusError, store.Revision{}, nil,
			openRefusal(target.RevisionID, local, err)
	}
	// KEPT AS THE FLEET'S BEFORE IT IS VALIDATED: it is what the fleet is on
	// whether or not this build can run it, and what a refusing node
	// restores when the pointer is gone. The boot row moves only once
	// applied ([Reconciler.serve]).
	r.keep(ctx, revision)
	// The STORED-form reader, not the authored one: the YAML parser rejects
	// providers.llm_order, and this one is lenient about unknown fields, so
	// a newer peer's revision boots here. ${VAR} references stay verbatim,
	// resolved where a provider is constructed, so re-activating an
	// unchanged revision picks up a rotated credential.
	//
	// Validated HERE, against the RUNNABLE rules only, before [Engine.Apply]:
	// Apply refreshes the secret snapshot first, so a refusal inside it has
	// already mutated this node. A newer peer's admission rules may differ
	// mid-upgrade, so admission violations are warned about, not refused.
	cfg, err := config.DecodeCompany(document)
	if err != nil {
		return configplane.StatusError, store.Revision{}, nil,
			fmt.Errorf("engine: parse revision %s: %w", target.RevisionID, err)
	}
	// Noted before anything below can refuse it: a node failing to apply a
	// revision that turns Gitlet on must say it serves none, not that the
	// company hosts no code ([Engine.CodeUnserved]).
	r.noteTargetCode(target.Epoch, cfg.CodeBackendFor() == config.CodeGitlet)
	if invalid := cfg.ValidateRunnable(); invalid != nil {
		return configplane.StatusError, store.Revision{}, nil,
			fmt.Errorf("engine: revision %s is not a runnable company; activate a "+
				"corrected revision: %w", target.RevisionID, invalid)
	}
	status, applied, err := r.engine.Apply(ctx, cfg, target.At)
	if status == configplane.StatusOK {
		// SUCCESS ONLY, so these log once per node per applied epoch: Tick
		// never re-applies a reached epoch, while a refused one is retried.
		r.warnAdmission(ctx, target, cfg)
		reportDanglingRefs(ctx, r.log, target, cfg)
		reportCompanyAdvisories(ctx, r.log, target, cfg)
		warnTiers(ctx, r.log, r.engine.boot, cfg, r.engine.codeCacheBudget(),
			"revision", target.RevisionID, "epoch", target.Epoch)
		r.engine.reportBridgeGaps(ctx, r.log, target, cfg)
		reportContainerReach(ctx, r.log, target, r.engine.boot, cfg)
	}
	return status, revision, applied, err
}

// warnAdmission logs each admission rule an APPLIED revision breaks, once per
// node per applied epoch with no record of what was said, since Tick never
// re-applies a reached epoch; a refused revision is reported by its refusal.
//
// One line per violation, saying what to do: the next write keeping it is
// refused. The violations are [config.Company.AdmissionWarnings], the list a
// reload's or revert's answer carries, which holds one warning per entity;
// the lines group them back into one per violation with every path it names.
func (r *Reconciler) warnAdmission(ctx context.Context, target coord.Activation, cfg *config.Company) {
	for _, violation := range violations(cfg.AdmissionWarnings()) {
		r.log.WarnContext(ctx, "org_admission_warning",
			"revision", target.RevisionID, "epoch", target.Epoch,
			"detail", violation.message, "paths", violation.paths,
			"hint", "the revision is applied as it stands, and any configuration "+
				"write that keeps this is refused; correct it with PUT /config "+
				"or PATCH /config")
	}
}

// violation is one admission rule broken, with every place it is about.
type violation struct {
	message string
	paths   []string
}

// violations groups warnings that render the same message into one, in the
// order each message first appears.
func violations(warnings []config.Warning) []violation {
	var out []violation
	at := map[string]int{}
	for _, w := range warnings {
		i, seen := at[w.Message]
		if !seen {
			i = len(out)
			at[w.Message] = i
			out = append(out, violation{message: w.Message})
		}
		if w.Path != "" {
			out[i].paths = append(out[i].paths, w.Path)
		}
	}
	return out
}

// fleetRevision is the revision the pointer names, as this node's history
// keeps it ([configplane.RevisionOf]), and whether its body came from this
// node's own copy.
//
// From this node's history where it holds it, else off the coordination store,
// where Activate writes the body in the pointer's record: THE ORDINARY CASE ON
// A PEER, since a write stores its body only on the node that served it.
//
// It writes nothing: anything the broker admits can write that bucket
// (ADR-0094), so the caller opens the seal before any of it is kept
// ([Reconciler.keep]). The pointer's record supplies origin and instant
// either way, so a revision reads the same on every node.
func (r *Reconciler) fleetRevision(ctx context.Context, target coord.Activation) (
	store.Revision, bool, error,
) {
	held, found, err := r.configs.Get(ctx, target.RevisionID)
	if err != nil {
		return store.Revision{}, false, fmt.Errorf("engine: read revision %s: %w",
			target.RevisionID, err)
	}
	if found {
		return configplane.RevisionOf(target, held.Payload), true, nil
	}
	payload, found, err := r.plane.Payload(ctx, target.RevisionID)
	if err != nil {
		return store.Revision{}, false, fmt.Errorf("engine: fetch revision %s: %w",
			target.RevisionID, err)
	}
	if !found {
		// An error, not a silent skip: a fleet where every node ignores an
		// unreadable pointer converges on nothing while reporting
		// convergence.
		return store.Revision{}, false, fmt.Errorf("engine: %w: %s",
			store.ErrNoRevision, target.RevisionID)
	}
	return configplane.RevisionOf(target, payload), false, nil
}

// openRefusal is why a revision the fleet is on did not open here, and what
// fixes it. UNSEALED is refused whatever its origin: anything reaching the
// store or broker could have authored it, and an admin's whole document
// replaces it (internal/api/configapi). A key this keyring lacks is a missing
// fleet key. A seal form this build does not read most likely came from a
// newer build, and the fix is that build.
func openRefusal(revisionID string, local bool, err error) error {
	switch {
	case errors.Is(err, secrets.ErrUnknownEnvelope) && local:
		return fmt.Errorf("engine: this node's own copy of revision %s is sealed "+
			"in a form this build doesn't read, so it is not applied. A newer "+
			"build of Crewlet most likely sealed it here: run that build on this "+
			"node again: %w", revisionID, err)
	case errors.Is(err, secrets.ErrUnknownEnvelope):
		return fmt.Errorf("engine: the fleet's revision %s is sealed in a form "+
			"this build doesn't read, so it is neither applied nor kept. A newer "+
			"build of Crewlet most likely sealed it: upgrade this node to the "+
			"build the rest of the fleet runs: %w", revisionID, err)
	case errors.Is(err, secrets.ErrUnsealedWithKey) && local:
		return fmt.Errorf("engine: this node's own copy of revision %s is stored "+
			"without a seal, so it is not applied. An admin replaces it by "+
			"sending the whole document again with PUT /config or `crewlet "+
			"config import`, which stores it sealed: %w", revisionID, err)
	case errors.Is(err, secrets.ErrUnsealedWithKey):
		return fmt.Errorf("engine: the fleet's revision %s arrived through the "+
			"coordination store without a seal, so it is neither applied nor "+
			"kept. Every node of this fleet seals what it publishes, so anything "+
			"that reaches the broker could have written it. An admin replaces it "+
			"by sending the whole document with PUT /config: %w", revisionID, err)
	default:
		return fmt.Errorf("engine: revision %s does not open under this node's "+
			"keyring, so it is neither applied nor kept; every node has to hold "+
			"the key the fleet seals under in secrets.keys: %w", revisionID, err)
	}
}

// record writes this node's outcome to two surfaces with two lifetimes.
//
// The COORDINATION record is the live fleet view: re-put every tick, its
// bucket's minute-long age makes a node that stops reporting VANISH rather
// than linger as healthy, so a minute after a bad rollout the crashed node's
// outcome is gone from it. The EVENT, in the audit event log keyed by the
// envelope's Source with the subsystems applied, is the durable post-mortem.
//
// Both are best effort: the apply has happened, and failing the tick would
// retry an apply that succeeded.
func (r *Reconciler) record(ctx context.Context, target coord.Activation,
	status configplane.ApplyStatus, applied []string, cause error,
) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	row := coord.NodeApply{
		NodeID: r.nodeID, Epoch: target.Epoch, RevisionID: target.RevisionID,
		Applied: r.Applied(), Serving: r.Serving(), Boots: r.Boots(),
		Status: string(status), Error: message, UpdatedAt: r.stamp(),
	}
	r.recorded.Store(&row)
	if err := r.plane.RecordApply(ctx, row); err != nil {
		r.log.WarnContext(ctx, "apply_status_write_failed", "epoch", target.Epoch,
			"error", err, "detail", "peers will read this node as stale")
	}
	r.publishApplied(ctx, target, status, applied, message)
}

// refresh rewrites this node's last outcome for the current target with this
// tick's time.
//
// THE ROW AGES OUT BY DESIGN ([coord.StatusFreshness]), so every live node
// keeps writing it, or a converged node drops out of the fleet view and every
// peer's [Reconciler.peerHealth]. Only an outcome this process recorded for
// this epoch, and no durable event: a refresh is not an apply.
//
// Applied, Serving and Boots are restated AS OF THIS TICK, so a boot row the
// preceding [Reconciler.serve] repaired shows now, and the row is one moment's
// account of the node.
func (r *Reconciler) refresh(ctx context.Context, target coord.Activation) {
	last := r.recorded.Load()
	if last == nil || last.Epoch != target.Epoch {
		return
	}
	row := *last
	row.Applied, row.Serving, row.Boots = r.Applied(), r.Serving(), r.Boots()
	row.UpdatedAt = r.stamp()
	if err := r.plane.RecordApply(ctx, row); err != nil {
		r.log.WarnContext(ctx, "apply_status_write_failed", "epoch", target.Epoch,
			"error", err, "detail", "peers will read this node as stale")
	}
}

// publishApplied puts one node's outcome into the audit event log.
//
// The coordination record is cut to [coord.MaxApplyErrorLength] because every
// peer re-reads it every tick; this event is written once and read days later,
// so it carries up to [events.MaxDiagnosticBytes], marked when cut. Bounded at
// all only so it can be PUBLISHED: an event over the queue's payload ceiling
// is refused, losing the whole record rather than its tail.
func (r *Reconciler) publishApplied(ctx context.Context, target coord.Activation,
	status configplane.ApplyStatus, applied []string, message string,
) {
	if r.queue == nil {
		return
	}
	ev := events.New(types.ConfigRevisionApplied{
		RevisionID:        target.RevisionID,
		Status:            applyStatus(status),
		AppliedSubsystems: applied,
		Error:             events.ClipDiagnostic(message),
	}, tracing.TraceOf(ctx))
	ev.Timestamp = r.stamp()
	// The NODE, not a seat: the payload deliberately leaves the reporter to
	// the envelope (see the type's doc).
	ev.Source = r.nodeID
	if err := r.queue.Publish(ctx, topics.ConfigRevisionApplied, ev); err != nil {
		r.log.WarnContext(ctx, "apply_event_publish_failed", "epoch", target.Epoch,
			"error", err, "detail", "this apply leaves no durable trail; "+
				"the fleet view still has it for the next minute")
	}
}

// applyStatus maps the control plane's posture onto the event vocabulary.
// Two closed sets kept apart on purpose: the event's goes on the wire and can
// never change. An unknown posture becomes error rather than passing through.
func applyStatus(status configplane.ApplyStatus) types.ApplyStatus {
	switch status {
	case configplane.StatusOK:
		return types.ApplyOK
	case configplane.StatusDegraded:
		return types.ApplyDegraded
	default:
		return types.ApplyError
	}
}

// Run polls until the context ends.
//
// The interval is JITTERED, and only the interval: pods a rolling deploy boots
// together would otherwise poll and apply in unison forever. Jittering the
// APPLY would delay convergence, and so the moment a node stops being the
// anomaly the shed rule acts on.
func (r *Reconciler) Run(ctx context.Context) {
	stop := r.listen(ctx)
	defer stop()

	timer := time.NewTimer(configplane.ReconcileDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-r.nudged:
			// The tick re-reads the POINTER rather than acting on the
			// event, so a redundant nudge costs one read and a lost one
			// only latency. Stopped but NOT drained: since Go 1.23 a
			// timer's channel is unbuffered and Stop is synchronised, so
			// no stale fire can be pending.
			timer.Stop()
		}
		if err := r.Tick(ctx); err != nil {
			r.log.WarnContext(ctx, "reconcile_tick_failed", "error", err)
		}
		// RE-DECIDE AFTER EVERY TICK, so the admission gate moves on this
		// loop: the heartbeat refreshes it only on a lease holder, and an
		// ingress-only node is the one this gates. Then converge the
		// ingress consumer on it: the inbound topic has no lease to resume
		// it ([Engine.resumeInbound]).
		if r.Posture(ctx).ServesTraffic() {
			r.engine.resumeInbound(ctx)
		}
		// A resume the posture refused is retried on the tick that finds
		// it admitting, the only place the posture moves. Only those: the
		// tick is not a poll of anything else.
		if r.engine.admits() {
			r.engine.readmitResumes(waitPosture)
		}
		// A full jittered interval after every tick, nudged or not: an
		// activation storm must not become an apply storm.
		timer.Reset(configplane.ReconcileDelay())
	}
}

// listen attaches the activation nudge, returning a stop safe to call however
// the attach went. A FAILED ATTACH IS NOT FATAL: the poll reads the
// authoritative pointer, so a node that cannot hear nudges converges one
// interval later rather than not at all.
func (r *Reconciler) listen(ctx context.Context) func() {
	if r.queue == nil {
		return func() {}
	}
	unsubscribe, err := r.queue.SubscribeStream(ctx, topics.ConfigRevisionActivated,
		func(_ context.Context, _ string, ev *events.Event) {
			r.log.DebugContext(ctx, "config_activation_nudge", "revision", nudgeRevision(ev))
			select {
			case r.nudged <- struct{}{}:
			default:
				// Already pending: collapse, since the loop
				// re-reads the pointer.
			}
		})
	if err != nil {
		r.log.WarnContext(ctx, "activation_nudge_unavailable", "error", err,
			"detail", "this node converges on its reconcile interval instead")
		return func() {}
	}
	return func() {
		// WithoutCancel: the loop's context has ended, and inheriting it
		// would leave the subscription on the broker.
		if err := unsubscribe(context.WithoutCancel(ctx)); err != nil {
			r.log.WarnContext(ctx, "activation_nudge_unsubscribe_failed", "error", err)
		}
	}
}

// nudgeRevision reads the revision id off a nudge, for the log line only.
func nudgeRevision(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	if payload, ok := ev.Data.(*types.ConfigRevisionActivated); ok {
		return payload.RevisionID
	}
	return ""
}

// peerHealth counts the PEERS that reported success at an epoch, and how many
// reported at all. Derived here from the fleet view, beside the posture that
// reads its freshness bound, rather than added to the coordination contract.
//
// The ASKER IS EXCLUDED: counting its own row, a fleet of one reads as
// unanimous and never sheds; its own status is already SelfStatus.
func (r *Reconciler) peerHealth(ctx context.Context, epoch int64) (ok, reported int, err error) {
	rows, err := r.plane.Fleet(ctx)
	if err != nil {
		return 0, 0, err
	}
	floor := r.stamp().Add(-configplane.PeerStatusFreshness)
	for _, row := range rows {
		switch {
		case row.NodeID == r.nodeID, row.Epoch != epoch, row.UpdatedAt.Before(floor):
			// A stale row is NO EVIDENCE rather than a failure, so
			// one dead node cannot decide the fleet's posture.
			continue
		}
		reported++
		if configplane.ApplyStatus(row.Status) == configplane.StatusOK {
			ok++
		}
	}
	return ok, reported, nil
}
