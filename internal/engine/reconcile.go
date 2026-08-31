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
	queue   Nudger
	nodeID  string
	cipher  secrets.Cipher

	// progress is this node's convergence state, published as ONE value.
	//
	// Written only by the tick — Run's loop, or the one-shot CLI path,
	// never both at once — and read by every surface that reports on this
	// node: the seat heartbeat through SetPosture, the readiness probe,
	// and each runtime-state HTTP handler, all on their own goroutines.
	//
	// ONE POINTER rather than three fields, because the three are read
	// TOGETHER and have to agree. `applied` alone was atomic, with a
	// comment naming this exact hazard; `attempts` and `target` were left
	// bare beside it and were a plain data race — invisible to -race only
	// because no test drove Posture concurrently with Run. Three separate
	// atomics would fix the race and leave the torn read: DecidePosture
	// comparing an applied epoch from one moment against an attempt count
	// from another can conclude "converged but still retrying", which is
	// not a state this node was ever in.
	progress atomic.Pointer[applyProgress]

	// decided is the last posture this reconciler computed, for the one
	// reader that cannot afford to compute its own.
	//
	// The heartbeat and the readiness probe call [Reconciler.Posture]
	// directly: they run on their own cadence and can pay its two
	// coordination reads. ADMISSION cannot — it is consulted once per
	// inbound delivery and once per scheduler tick, and a control-plane
	// read there would put a network round trip in front of every
	// webhook. So the value the loop already computes is cached here and
	// [Reconciler.Admits] reads that, which is also the model the design
	// describes: the refusal runs on the delivery path while the posture
	// moves on the reconcile loop.
	//
	// Zero means "nothing decided yet", which ADMITS. A node that has not
	// completed one pass is not evidence that it is behind.
	decided atomic.Pointer[configplane.Posture]

	// nudged carries an activation from the broadcast subscription to the
	// loop. Buffered at ONE and written non-blocking: a storm of
	// activations must collapse into a single early wake, not into a queue
	// of them — the loop re-reads the pointer when it runs, so N nudges and
	// one nudge lead to the same place.
	nudged chan struct{}

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

	// Queue carries this node's apply outcome out as a durable event and
	// brings activations in as a nudge. Required: the coordination record
	// the outcome accompanies ages out in a minute by design, so a
	// reconciler without a publisher leaves a bad rollout with no
	// post-mortem trail at all — and that absence is invisible, which is
	// how the event type came to exist with nothing publishing it.
	Queue Nudger

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

// Nudger is the queue surface this loop uses: it publishes what it did, and
// listens for an activation so an operator's change lands in milliseconds
// rather than at the next poll.
//
// The subscription is an EPHEMERAL BROADCAST, never a consumer group. Every
// node has to hear every activation — a competing group would hand each one to
// exactly one node, which is the delivery shape that made config a fleet of
// one before the pointer existed. Losing a nudge costs one poll interval and
// never a revision, because the poll asks rather than replays.
type Nudger interface {
	queue.Publisher
	SubscribeStream(ctx context.Context, topicPattern string, h queue.StreamHandler) (queue.Unsubscribe, error)
}

// ErrNoStore reports a reconciler built without somewhere to read revisions.
var ErrNoStore = errors.New("engine: reconciler needs a store")

// ErrNoPlane reports a reconciler built without a coordination plane. Its own
// sentinel because the two are different deployments to fix: one is a missing
// database, the other a node with no coordination store to converge through.
var ErrNoPlane = errors.New("engine: reconciler needs a coordination plane")

// ErrNoPublisher reports a reconciler built with nowhere to publish its apply
// outcome. Its own sentinel for the same reason as ErrNoPlane: the fix is a
// queue, not a database or a coordination store.
var ErrNoPublisher = errors.New("engine: reconciler needs a publisher")

// NewReconciler builds the loop.
func (e *Engine) NewReconciler(opts ReconcilerOptions) (*Reconciler, error) {
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
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Reconciler{
		nudged:  make(chan struct{}, 1),
		engine:  e,
		configs: opts.Store.Configs(),
		plane:   opts.Fleet,
		queue:   opts.Queue,
		nodeID:  opts.NodeID,
		cipher:  opts.Cipher,
		onApply: opts.OnApply,
		now:     now,
	}, nil
}

// applyProgress is what the tick has achieved against what it is aiming at.
//
// A value type, stored and replaced whole, so every reader sees a triple
// that was true at one instant.
type applyProgress struct {
	// applied is the epoch this node is serving, 0 before its first
	// successful apply.
	applied int64

	// target is the epoch the pointer named when this node last saw it.
	target int64

	// attempts counts tries at target, reset when target moves. Per epoch,
	// not per node lifetime: re-activating a fixed revision resets the
	// budget, so the runbook's fix actually works.
	attempts int
}

// snapshot reads the triple. The zero value is a node that has not ticked.
func (r *Reconciler) snapshot() applyProgress {
	if p := r.progress.Load(); p != nil {
		return *p
	}
	return applyProgress{}
}

// publish replaces the triple. Callers are the tick alone, which is why a
// plain load-modify-store needs no compare-and-swap.
func (r *Reconciler) publish(p applyProgress) { r.progress.Store(&p) }

// Applied is the epoch this node is serving, or 0 before its first apply.
func (r *Reconciler) Applied() int64 { return r.snapshot().applied }

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
		return r.decide(configplane.PostureServe)
	}
	return r.decide(configplane.DecidePosture(view))
}

// decide records a posture for the admission gate and returns it.
//
// Every path out of [Reconciler.Posture] goes through here, the fail-open one
// included: a node that cannot read the plane reports serve, and serve is the
// answer admission must see too. Recording only the successful reads would
// leave a node that shed once and then lost the plane refusing work for as
// long as the outage lasted, on evidence it could no longer check.
func (r *Reconciler) decide(p configplane.Posture) configplane.Posture {
	r.decided.Store(&p)
	return p
}

// Admits reports whether this node should take new inbound work.
//
// It reads the posture the reconcile loop last decided rather than computing
// a fresh one — see [Reconciler.decided] — and fails OPEN in both uncertain
// directions: no decision yet admits, and Posture already reports serve when
// it cannot read the plane. Only a posture that positively concluded shed or
// stuck refuses, which is exactly the set /ready fails on.
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
	// ONE SNAPSHOT for the whole view, so the applied epoch, the attempt
	// count and the status derived from it all describe the same moment.
	progress := r.snapshot()
	applied := progress.applied
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
		SelfStatus:   selfStatus(progress),
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
		Attempts:      progress.attempts,
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
func selfStatus(p applyProgress) configplane.ApplyStatus {
	if p.attempts == 0 {
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
	progress := r.snapshot()
	if target.Epoch != progress.target {
		// A new target resets the attempt budget, which is what makes
		// "re-activate the fixed revision" the documented recovery.
		progress.target, progress.attempts = target.Epoch, 0
		r.publish(progress)
	}
	if progress.applied == target.Epoch {
		return nil
	}
	if progress.attempts >= configplane.MaxApplyAttempts {
		// Out of retries on this epoch. The posture already reads this
		// node as having tried and failed, so it moves toward stuck or
		// isolated without another counter to keep.
		return nil
	}
	progress.attempts++
	r.publish(progress)
	return r.apply(ctx, target)
}

// apply loads, opens, parses and publishes one revision, then records what
// happened for the rest of the fleet to read.
func (r *Reconciler) apply(ctx context.Context, target coord.Activation) error {
	status, applied, err := r.applyRevision(ctx, target)
	r.record(ctx, target, status, applied, err)
	if status == configplane.StatusOK {
		progress := r.snapshot()
		progress.applied, progress.attempts = target.Epoch, 0
		r.publish(progress)
	}
	if r.onApply != nil {
		r.onApply(target.Epoch, status)
	}
	return err
}

// applyRevision is the part that can fail, separated so every exit records.
//
// The subsystem list is empty on every exit before [Engine.Apply] is reached:
// nothing on this node has been mutated yet when the revision cannot be read,
// opened or parsed, and an empty list says exactly that.
func (r *Reconciler) applyRevision(ctx context.Context, target coord.Activation) (configplane.ApplyStatus, []string, error) {
	revision, found, err := r.configs.Get(ctx, target.RevisionID)
	if err != nil {
		return configplane.StatusError, nil, fmt.Errorf("engine: read revision %s: %w",
			target.RevisionID, err)
	}
	if !found {
		// THE ORDINARY CASE ON A PEER, not an anomaly: the payload is
		// written to the database of whichever node served the write,
		// and every other node meets the revision for the first time
		// here. Fetching it from the coordination store — where
		// Activate puts it before it moves the pointer — is what makes
		// a live config change reach a fleet at all. Before this, a
		// peer read nothing and reported "no such revision" once per
		// tick for the life of the deployment.
		revision, err = r.fetchRevision(ctx, target)
		if err != nil {
			return configplane.StatusError, nil, err
		}
	}
	document, err := secrets.Open(r.cipher, revision.Payload)
	if err != nil {
		return configplane.StatusError, nil, fmt.Errorf("engine: open revision %s: %w",
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
		return configplane.StatusError, nil, fmt.Errorf("engine: parse revision %s: %w",
			target.RevisionID, err)
	}
	return r.engine.Apply(ctx, cfg)
}

// fetchRevision pulls the target's payload off the coordination store and
// keeps this node's own copy of it.
//
// PERSISTED, not merely used: company_config is where this node's history,
// its diffs and its revert targets live, and a node that applied a revision it
// never recorded would serve an epoch its own operator surface cannot show.
// The local write is best effort for the same reason the apply status is —
// the revision is applied either way, and a node that could not write its copy
// re-fetches on its next miss.
func (r *Reconciler) fetchRevision(ctx context.Context, target coord.Activation) (store.Revision, error) {
	payload, found, err := r.plane.Payload(ctx, target.RevisionID)
	if err != nil {
		return store.Revision{}, fmt.Errorf("engine: fetch revision %s: %w",
			target.RevisionID, err)
	}
	if !found {
		// The pointer names a revision whose body is nowhere. An error
		// rather than a silent skip: this node cannot reach the epoch,
		// and a fleet where every node quietly ignores an unreadable
		// pointer converges on nothing while reporting convergence.
		return store.Revision{}, fmt.Errorf("engine: %w: %s",
			store.ErrNoRevision, target.RevisionID)
	}
	revision := store.Revision{
		ID: target.RevisionID, Source: "fleet", CreatedBy: "peer",
		Summary: target.Summary, Payload: payload, CreatedAt: target.At,
	}
	if err := r.configs.Adopt(ctx, revision); err != nil {
		log.WarnContext(ctx, "revision_not_cached", "revision", target.RevisionID,
			"error", err, "detail", "the revision is applied; this node's config "+
				"history will not show it and it will be re-fetched next time")
	}
	return revision, nil
}

// record writes this node's outcome twice, to two surfaces with two
// lifetimes, and both are needed.
//
// The COORDINATION record is the live fleet view: one key per node, re-put
// every tick, and its bucket's own age is what makes a node that stops
// reporting VANISH rather than linger as a healthy row nobody is writing. That
// age is a minute — so sixty seconds after a bad rollout, the epoch, status and
// error text of the node that crashed or was scaled in are gone, which is
// precisely the node somebody reviewing the incident is looking for.
//
// The EVENT is the durable trail that answers them. It goes to the audit event
// log like every other event, keyed by the node in the envelope's Source and
// carrying the subsystems the apply got through, and it outlives both the
// node and the bucket. ConfigRevisionApplied has been a registered event type
// with its own topic since the control plane shipped and NOTHING EVER
// PUBLISHED IT, so the post-mortem the fleet view was never meant to serve had
// no other home either.
//
// Both are best effort by necessity: the apply has already happened, and
// failing the tick because a note could not be written would retry an apply
// that succeeded.
func (r *Reconciler) record(ctx context.Context, target coord.Activation,
	status configplane.ApplyStatus, applied []string, cause error,
) {
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
	r.publishApplied(ctx, target, status, applied, message)
}

// publishApplied puts one node's outcome into the audit event log.
//
// TWO DIFFERENT BOUNDS, and the difference is the point. The coordination
// record is cut to [coord.MaxApplyErrorLength] (2000 bytes) because every peer
// re-reads it on every reconcile tick, so its size is paid continuously. This
// event is written once and kept for the event store's retention horizon — it
// is the durable copy an operator reads days later — so it carries up to
// [events.MaxDiagnosticBytes] (64 KiB), thirty times more, marked when it cuts.
//
// It is bounded at all only so that it can be PUBLISHED: an event over the
// queue's payload ceiling is refused, and the publisher below logs and moves
// on, so an unbounded failure text would cost the operator the whole record
// rather than its tail.
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
	ev.Timestamp = r.now()
	// The NODE, not a seat. Every other field of the payload describes the
	// revision; which node is reporting lives in the envelope, and the
	// payload deliberately does not repeat it (see the type's own doc).
	ev.Source = r.nodeID
	if err := r.queue.Publish(ctx, topics.ConfigRevisionApplied, ev); err != nil {
		log.WarnContext(ctx, "apply_event_publish_failed", "epoch", target.Epoch,
			"error", err, "detail", "this apply leaves no durable trail; "+
				"the fleet view still has it for the next minute")
	}
}

// applyStatus maps the control plane's posture onto the event vocabulary.
//
// Two closed sets that happen to share their spellings, kept apart on purpose:
// configplane's is what a node reports to its peers and the event's is what
// goes on the wire and can never change. An unknown posture becomes error
// rather than passing through, because the payload's own doc says an unset
// status reads as not-ok and a value nothing recognises is worse than that.
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
// The interval is JITTERED, and only the interval: a rolling deploy boots every
// pod within the same second, and without jitter they would poll — and then
// apply, and then restart their children — in unison forever. Jittering the
// APPLY was considered and rejected, because it delays convergence, and under
// the shed rule delaying convergence means delaying the moment a node stops
// being the anomaly.
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
			// An activation arrived. The tick below re-reads the
			// POINTER — it does not act on the event — so a nudge for
			// a revision this node already serves costs one wasted
			// read, and a lost nudge costs nothing but latency.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if err := r.Tick(ctx); err != nil {
			log.WarnContext(ctx, "reconcile_tick_failed", "error", err)
		}
		// RE-DECIDE AFTER EVERY TICK, so the admission gate moves on this
		// loop rather than on whichever other caller happens to ask. The
		// heartbeat also refreshes it, but only on a node that holds a
		// lease — and an ingress-only node that never claims one is
		// precisely the node whose admission this gates.
		//
		// Then converge the ingress consumer on the answer: a seat's
		// mailbox is resumed by its own lease admission, and the inbound
		// topic has no lease to do it. See [Engine.resumeInbound].
		if r.Posture(ctx).ServesTraffic() {
			r.engine.resumeInbound(ctx)
		}
		// A full jittered interval after every tick, nudged or not: an
		// activation storm must not become an apply storm.
		timer.Reset(configplane.ReconcileDelay())
	}
}

// listen attaches the activation nudge, returning a stop that is safe to call
// however the attach went.
//
// A FAILED ATTACH IS NOT FATAL, and saying so is the whole design: the pointer
// is the authoritative path and the poll is what reads it, so a node that
// cannot hear nudges converges one interval later than its peers rather than
// not at all. That is the property that let this event be a nudge in the first
// place.
func (r *Reconciler) listen(ctx context.Context) func() {
	if r.queue == nil {
		return func() {}
	}
	unsubscribe, err := r.queue.SubscribeStream(ctx, topics.ConfigRevisionActivated,
		func(_ context.Context, _ string, ev *events.Event) {
			log.DebugContext(ctx, "config_activation_nudge", "revision", nudgeRevision(ev))
			select {
			case r.nudged <- struct{}{}:
			default:
				// Already pending. Collapsing is correct: the
				// loop re-reads the pointer, so the second
				// nudge would send it to the same place.
			}
		})
	if err != nil {
		log.WarnContext(ctx, "activation_nudge_unavailable", "error", err,
			"detail", "this node converges on its reconcile interval instead")
		return func() {}
	}
	return func() {
		// WithoutCancel: the loop's context is what ended, and a
		// teardown that inherited it would leave the subscription behind
		// on the broker.
		if err := unsubscribe(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "activation_nudge_unsubscribe_failed", "error", err)
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
	id, _ := ev.Payload["revision_id"].(string)
	return id
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
