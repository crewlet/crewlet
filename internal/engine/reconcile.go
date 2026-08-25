package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// Reconciler converges this node on the activation pointer.
//
// A POLLED RECONCILER, not a subscription, and that is the whole design.
// Config used to arrive over a competing-consumer subscription: with N
// processes exactly ONE applied a revision and the rest ran the previous
// company indefinitely, while the dashboard reported success because the one
// node that applied it said so. A pointer every node re-reads has no such
// failure — a node that was down for an activation catches up on its next
// tick, because it reads the current state rather than replaying an event it
// missed.
//
// It also RECORDS. Reading the pointer says where the fleet should be; the
// per-node apply status says where each node actually is, and only the two
// together can tell "behind because propagation takes a moment" from "behind
// because I cannot apply this" — which need opposite responses.
type Reconciler struct {
	engine  *Engine
	configs *store.Configs
	plane   coord.Plane
	nodeID  string
	cipher  secrets.Cipher

	// applied is the epoch this node is serving, and 0 before its first
	// successful apply. Atomic because the health surface reads it from
	// whatever goroutine answered a probe.
	applied atomic.Int64

	// attempts counts tries at the CURRENT target, reset when the target
	// moves. Per epoch, not per node lifetime: re-activating a fixed
	// revision resets the budget, so the runbook's fix actually works.
	attempts int
	target   int64

	// onApply is called after every apply with the outcome, so the API
	// half of a merged node learns whether it is configured without
	// polling the pointer a second time.
	onApply func(epoch int64, status configplane.ApplyStatus)

	now func() time.Time
}

// ReconcilerOptions configure a reconciler.
type ReconcilerOptions struct {
	// Store is where the revision PAYLOADS live. Required.
	Store *store.DB

	// Fleet is where the activation pointer and the per-node apply status
	// live. Required: they are what the fleet converges on, and a node
	// reading its own database would converge on itself.
	Fleet coord.Plane

	// NodeID identifies this node's row in the apply status. Required: the
	// table upserts on it, so an empty one makes every node that forgot it
	// share a row, and the fleet view shows one anonymous entry where a
	// dozen nodes should be.
	NodeID string

	// Cipher opens a sealed revision. Nil reads plaintext payloads and
	// refuses sealed ones — a deployment that lost its keyring must say so
	// rather than boot onto an empty company.
	Cipher secrets.Cipher

	// OnApply observes every outcome.
	OnApply func(epoch int64, status configplane.ApplyStatus)

	// Now is injectable so a test can pin the freshness window.
	Now func() time.Time
}

// ErrNoStore reports a reconciler built without somewhere to read revisions.
var ErrNoStore = errors.New("engine: reconciler needs a store")

// ErrNoPlane reports a reconciler built without a coordination plane. Its own
// sentinel because the two are different deployments to fix: one is a missing
// database, the other a node with no coordination store to converge through.
var ErrNoPlane = errors.New("engine: reconciler needs a coordination plane")

// NewReconciler builds the loop.
func (e *Engine) NewReconciler(opts ReconcilerOptions) (*Reconciler, error) {
	if opts.Store == nil {
		return nil, ErrNoStore
	}
	if opts.Fleet == nil {
		return nil, ErrNoPlane
	}
	if opts.NodeID == "" {
		return nil, fmt.Errorf("engine: reconciler needs a node id")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Reconciler{
		engine:  e,
		configs: opts.Store.Configs(),
		plane:   opts.Fleet,
		nodeID:  opts.NodeID,
		cipher:  opts.Cipher,
		onApply: opts.OnApply,
		now:     now,
	}, nil
}

// Applied is the epoch this node is serving, or 0 before its first apply.
func (r *Reconciler) Applied() int64 { return r.applied.Load() }

// Posture is what this node should do about the gap between what it has
// applied and what the pointer names.
//
// Read from the store on every call rather than cached, because it is asked by
// the readiness probe and a cached posture is a node that reports healthy
// through the whole window in which it stopped being so.
func (r *Reconciler) Posture(ctx context.Context) configplane.Posture {
	view, err := r.view(ctx)
	if err != nil {
		// FAILS TO SERVE, not to stuck. A store this node cannot read is
		// a node that cannot know whether it is behind — and the safe
		// answer to "am I behind?" is the one that keeps a working
		// company working, because the alternative takes every node out
		// of rotation on a database blip.
		log.WarnContext(ctx, "posture_unknown", "error", err,
			"detail", "cannot read the control plane; assuming this node is current")
		return configplane.PostureServe
	}
	return configplane.DecidePosture(view)
}

// view assembles what the posture rule needs.
func (r *Reconciler) view(ctx context.Context) (configplane.FleetView, error) {
	target, found, err := r.plane.Target(ctx)
	if err != nil {
		return configplane.FleetView{}, err
	}
	applied := r.applied.Load()
	if !found {
		// No activation at all. A node with nothing to converge on is
		// current by definition — an unconfigured deployment is not a
		// diverged one.
		return configplane.FleetView{TargetEpoch: applied, AppliedEpoch: applied,
			SelfStatus: configplane.StatusOK}, nil
	}
	ok, reported, err := r.peerHealth(ctx, target.Epoch)
	if err != nil {
		return configplane.FleetView{}, err
	}
	return configplane.FleetView{
		TargetEpoch:  target.Epoch,
		AppliedEpoch: applied,
		SelfStatus:   r.selfStatus(),
		// ALWAYS ZERO, and that is the honest value here rather than an
		// unset field. TicksBehind exists so a reconciler whose apply is
		// asynchronous can distinguish "behind and still working on it"
		// from "behind and stalled". This one applies synchronously
		// inside the tick, so by the time a posture is asked for, the
		// node has either reached the epoch or recorded a failure — and
		// the failure is what SelfStatus already carries. A counter
		// incremented beside it would move only when SelfStatus was
		// already error, so it could never change a decision. Mutation
		// testing found exactly that: removing the increment changed no
		// outcome.
		TicksBehind:   0,
		Attempts:      r.attempts,
		PeersOK:       ok,
		PeersReported: reported,
	}, nil
}

// selfStatus is this node's own last outcome for the current target.
//
// Derived rather than stored: the store row is what PEERS read, and a second
// copy of the same fact in memory is one restart away from disagreeing with
// it. A node that has applied the target reports ok; one that has tried and
// not reached it reports error, which is the honest reading of "still serving
// the prior epoch".
func (r *Reconciler) selfStatus() configplane.ApplyStatus {
	if r.attempts == 0 {
		return configplane.StatusOK
	}
	return configplane.StatusError
}

// Tick is one poll: read the pointer, and apply the revision it names when
// this node is not already on it.
//
// It RE-READS rather than replaying, which is what makes a node that was down
// for an activation catch up on its next tick instead of waiting for an event
// that has already been delivered to somebody else.
func (r *Reconciler) Tick(ctx context.Context) error {
	target, found, err := r.plane.Target(ctx)
	if err != nil {
		return fmt.Errorf("engine: reconcile: %w", err)
	}
	if !found {
		// Nothing has been activated. Not an error and not a state to
		// report: a fresh deployment sits here until the first import.
		return nil
	}
	if target.Epoch != r.target {
		// A new target resets the attempt budget, which is what makes
		// "re-activate the fixed revision" the documented recovery.
		r.target, r.attempts = target.Epoch, 0
	}
	if r.applied.Load() == target.Epoch {
		return nil
	}
	if r.attempts >= configplane.MaxApplyAttempts {
		// Out of retries on this epoch. The posture already reads this
		// node as having tried and failed, so it moves toward stuck or
		// isolated without another counter to keep.
		return nil
	}
	r.attempts++
	return r.apply(ctx, target)
}

// apply loads, opens, parses and publishes one revision, then records what
// happened for the rest of the fleet to read.
func (r *Reconciler) apply(ctx context.Context, target coord.Activation) error {
	status, err := r.applyRevision(ctx, target)
	r.record(ctx, target, status, err)
	if status == configplane.StatusOK {
		r.applied.Store(target.Epoch)
		r.attempts = 0
	}
	if r.onApply != nil {
		r.onApply(target.Epoch, status)
	}
	return err
}

// applyRevision is the part that can fail, separated so every exit records.
func (r *Reconciler) applyRevision(ctx context.Context, target coord.Activation) (configplane.ApplyStatus, error) {
	revision, found, err := r.configs.Get(ctx, target.RevisionID)
	if err != nil {
		return configplane.StatusError, fmt.Errorf("engine: read revision %s: %w",
			target.RevisionID, err)
	}
	if !found {
		// The pointer names a revision that is not there. An error
		// rather than a silent skip: this node cannot reach the epoch,
		// and a fleet where every node quietly ignores an unreadable
		// pointer converges on nothing while reporting convergence.
		return configplane.StatusError, fmt.Errorf("engine: %w: %s",
			store.ErrNoRevision, target.RevisionID)
	}
	document, err := secrets.Open(r.cipher, revision.Payload)
	if err != nil {
		return configplane.StatusError, fmt.Errorf("engine: open revision %s: %w",
			target.RevisionID, err)
	}
	// The STORED-form reader, not the authored one. A revision carries
	// providers.llm_order — the declaration order of a Go map, which exists
	// only while the YAML document does — and the YAML parser rejects it as
	// an unknown setting. It is also lenient about fields this build does
	// not know, so a revision activated by a newer peer boots here instead
	// of taking the older half of a fleet down.
	//
	// ${VAR} references stay verbatim through all of it: they are resolved
	// where a provider is CONSTRUCTED, which is what makes re-activating an
	// unchanged revision pick up a rotated credential rather than rebuild
	// the same values.
	cfg, err := config.DecodeCompany(document)
	if err != nil {
		return configplane.StatusError, fmt.Errorf("engine: parse revision %s: %w",
			target.RevisionID, err)
	}
	return r.engine.Apply(ctx, cfg)
}

// record writes this node's outcome where every peer reads it.
//
// Best effort by necessity: the apply has already happened, and failing the
// tick because the note could not be written would retry an apply that
// succeeded. What it costs is that peers see this node as stale, which the
// freshness bound already treats as "no evidence" rather than as failure.
func (r *Reconciler) record(ctx context.Context, target coord.Activation, status configplane.ApplyStatus, cause error) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	if err := r.plane.RecordApply(ctx, coord.NodeApply{
		NodeID: r.nodeID, Epoch: target.Epoch, RevisionID: target.RevisionID,
		Status: string(status), Error: message, UpdatedAt: r.now(),
	}); err != nil {
		log.WarnContext(ctx, "apply_status_write_failed", "epoch", target.Epoch,
			"error", err, "detail", "peers will read this node as stale")
	}
}

// Run polls until the context ends.
//
// The interval is JITTERED, and only the interval: a rolling deploy boots every
// pod within the same second, and without jitter they would poll — and then
// apply, and then restart their children — in unison forever. Jittering the
// APPLY was considered and rejected, because it delays convergence, and under
// the shed rule delaying convergence means delaying the moment a node stops
// being the anomaly.
func (r *Reconciler) Run(ctx context.Context) {
	timer := time.NewTimer(configplane.ReconcileDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := r.Tick(ctx); err != nil {
			log.WarnContext(ctx, "reconcile_tick_failed", "error", err)
		}
		timer.Reset(configplane.ReconcileDelay())
	}
}

// peerHealth counts the PEERS that reported success at an epoch, and how many
// reported at all.
//
// Derived from the fleet view rather than asked of the coordination store,
// because it is a QUESTION about the data rather than a thing a store can do
// better: the contract stays four methods, and the freshness bound stays here
// beside the posture that reads it.
//
// The ASKER IS EXCLUDED, which is the part that matters. A node counting its
// own row would read a fleet of one as unanimous and shed nothing however far
// behind its peers were — and a node is never evidence about itself, because
// its own status is already SelfStatus.
func (r *Reconciler) peerHealth(ctx context.Context, epoch int64) (ok, reported int, err error) {
	rows, err := r.plane.Fleet(ctx)
	if err != nil {
		return 0, 0, err
	}
	floor := r.now().Add(-configplane.PeerStatusFreshness)
	for _, row := range rows {
		switch {
		case row.NodeID == r.nodeID, row.Epoch != epoch, row.UpdatedAt.Before(floor):
			// A stale row is NO EVIDENCE rather than a failure: a
			// node that stopped reporting may be gone, may be slow,
			// and treating either as a vote would let one dead node
			// decide the fleet's posture.
			continue
		}
		reported++
		if configplane.ApplyStatus(row.Status) == configplane.StatusOK {
			ok++
		}
	}
	return ok, reported, nil
}
