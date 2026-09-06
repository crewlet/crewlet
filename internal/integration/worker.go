package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("integration")

// Interval is how often the loop wakes to see what is due.
//
// Fifteen seconds, which is the FINEST CADENCE [Schedule] can ask for
// ([Schedule.AdminBase]): a tick slower than that would make the shortest
// wait in the system a lie, and the whole point of the brisk admin cadence is
// that an operator who installs an app sees provisioning continue without
// pressing anything.
//
// It is also what the control plane's own reconcile poll ticks at, and the
// two cost about the same: a tick with nothing due is one duty claim and one
// read of a coordination bucket holding at most seven keys, on a connection
// the process already holds. Nothing is fetched from a vendor unless a
// vendor is due.
const Interval = 15 * time.Second

// Reconciler is one surface's convergence step.
//
// # Level triggered, never told what changed
//
// Handed nothing and asked what the world looks like now. An event can be
// lost, refused, or arrive twice, and each of those has already cost an
// integration that silently never finished. A pass that reads the world
// cannot be lost.
//
// # The safety contract, which is not optional
//
// This runs unattended, every few minutes, for the life of the deployment. So
// an implementation:
//
//   - MUST be idempotent. Every pass does the same work, and only what the
//     vendor already has separates a repair from a no-op.
//   - MUST NOT rotate a credential that still works. A vendor serves a token
//     once, so the tempting reading of "reconcile" is to mint every pass, and
//     that is an outage on a timer: the engine is authenticating with the old
//     value, and rotating revokes what every running agent is using. Check
//     what the sink recorded (provision.TokenSink.Value) and keep a working
//     credential. Rotation is an operator gesture on the vendor subcommand,
//     where somebody typed the flag.
//   - MUST NOT delete anything a seat's departure implies. Decommissioning is
//     the other flag on that subcommand, for the same reason.
//   - SHOULD cost nothing when nothing has changed. A converged company is
//     the steady state and it is the state this loop spends most of its life
//     in, so a pass that re-reads every seat from the vendor every ten
//     minutes is the one design that makes the feature too expensive to leave
//     switched on.
type Reconciler interface {
	// Kind names the surface this converges.
	Kind() Kind

	// Reconcile brings the surface in line with the company and reports
	// what it found.
	//
	// The two return values are different KINDS of answer and must not be
	// collapsed. Findings are statements about the operator's world, which
	// the loop classifies and reports. An error is the engine or the vendor
	// failing to look at that world at all, which the loop records as a
	// fault and retries.
	//
	// [ErrNotConfigured] is the third answer, and it is neither of those.
	Reconcile(ctx context.Context) ([]Finding, error)
}

// ErrNotConfigured reports a surface whose block has left the company.
//
// # Why a sentinel rather than a registration the apply rebuilds
//
// The company document is edited live, so the set of configured surfaces
// moves under a running loop. The obvious answer is to tear the worker down
// and build a new one on every apply, and it is worse than it looks: the
// worker is a fleet singleton holding a lease, so rebuilding it on an
// unrelated config change drops that lease and hands a peer a duty it will
// hold until the TTL lapses, for a company whose integrations did not change.
//
// So registration is static, every reconciler reads the LIVE config on each
// pass, and one that finds its own block gone says so. The loop then forgets
// its status, exactly as it forgets a kind nothing registered.
var ErrNotConfigured = errors.New("integration: this surface is not configured")

// Registration is one reconciler and what is specific to its cadence.
type Registration struct {
	Reconciler Reconciler

	// Settled overrides [Schedule.Settled] for this surface. Zero takes the
	// schedule's own value, which is the right answer for a vendor with no
	// reason to differ. See [Schedule.next] for the one that does.
	Settled time.Duration
}

// DutyFunc claims the single-owner reconcile duty for one tick.
//
// Nil means "no fleet", which is the single-node case: there is nobody to be
// a singleton among. That is NOT the same as a node whose roles exclude
// worker duties, which must refuse rather than assume ownership.
type DutyFunc func(ctx context.Context) (bool, error)

// Options builds a [Worker].
type Options struct {
	// Registrations are the surfaces to converge. A kind registered twice
	// is refused by [New]: two reconcilers for one surface would each
	// overwrite the other's state every tick, and the last writer would
	// decide what an operator sees.
	Registrations []Registration

	Store    Store
	Schedule Schedule

	// ClaimDuty gates each tick on holding the fleet's reconcile duty.
	ClaimDuty DutyFunc

	// Interval overrides [Interval]. Zero takes it.
	Interval time.Duration

	// Now is the clock, for tests. Nil is time.Now.
	Now func() time.Time
}

// Worker runs the registered reconcilers against whatever is due.
//
// # A fleet singleton, and for a sharper reason than the retention sweep's
//
// The sweep is a singleton because N nodes doing idempotent range deletes is
// N times the writes for one table's benefit. Here it is correctness: two
// nodes reconciling one surface at the same moment both read a vendor that
// has no account for a seat, and both create one. The vendor ends up with two
// identities for one agent and the engine records whichever wrote last, which
// is not a state any later pass can detect or repair.
type Worker struct {
	byKind   map[Kind]Registration
	order    []Kind
	store    Store
	schedule Schedule
	claim    DutyFunc
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	stop    context.CancelFunc
	stopped chan struct{}
}

// New builds the worker, refusing a registration set it could not run.
func New(opts Options) (*Worker, error) {
	byKind := make(map[Kind]Registration, len(opts.Registrations))
	order := make([]Kind, 0, len(opts.Registrations))
	for _, reg := range opts.Registrations {
		if reg.Reconciler == nil {
			return nil, errors.New("integration: a registration has no reconciler")
		}
		kind := reg.Reconciler.Kind()
		if !kind.Valid() {
			return nil, fmt.Errorf(
				"integration: %q is not a surface this build converges; "+
					"add it to integration.Kinds or drop the registration", kind)
		}
		if _, dup := byKind[kind]; dup {
			return nil, fmt.Errorf(
				"integration: %s is registered twice; two reconcilers for one "+
					"surface would each overwrite the other's status", kind)
		}
		byKind[kind] = reg
		order = append(order, kind)
	}
	if opts.Store == nil && len(byKind) > 0 {
		return nil, errors.New(
			"integration: reconcilers are registered but there is nowhere to " +
				"record what they find; give Options.Store a coordination store")
	}
	// The registration order is the caller's, and the caller's is the
	// config's. Sorted into the canonical order instead, so two nodes with
	// the same company converge their surfaces in the same sequence and a
	// status listing does not reshuffle when the duty moves.
	slices.SortFunc(order, func(a, b Kind) int {
		return slices.Index(Kinds, a) - slices.Index(Kinds, b)
	})

	interval := opts.Interval
	if interval <= 0 {
		interval = Interval
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Worker{
		byKind: byKind, order: order, store: opts.Store,
		schedule: opts.Schedule.withDefaults(), claim: opts.ClaimDuty,
		interval: interval, now: now,
	}, nil
}

// Start runs the loop until Stop, or until ctx is done.
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		return
	}
	if len(w.byKind) == 0 {
		log.InfoContext(ctx, "integration_reconciler_idle",
			"detail", "no integration in this company declares provisioning")
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	w.stop = cancel
	w.stopped = make(chan struct{})
	go w.run(ctx, w.stopped)
}

// Stop ends the loop, waiting for a tick already in flight.
func (w *Worker) Stop() {
	w.mu.Lock()
	stop, stopped := w.stop, w.stopped
	w.stop, w.stopped = nil, nil
	w.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	<-stopped
}

func (w *Worker) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	log.InfoContext(ctx, "integration_reconciler_started",
		"interval", w.interval, "surfaces", len(w.byKind))

	// A pass immediately, so a restart picks up whatever came due while the
	// process was down rather than waiting out a full tick. Its own failure
	// is not fatal for the same reason the control plane's first tick is
	// not: a node that cannot reach the coordination store still runs the
	// company it booted with.
	w.Tick(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.InfoContext(ctx, "integration_reconciler_stopping")
			return
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

// Tick runs one pass over whatever is due.
//
// Exported so the loop can be driven directly by a test and by an operator's
// explicit "reconcile now", without either having to wait out an interval or
// start a goroutine.
func (w *Worker) Tick(ctx context.Context) {
	if w.claim != nil {
		held, err := w.claim(ctx)
		if err != nil {
			// UNKNOWN IS NOT LOST. A coordination store that could not
			// answer must not be read as "the duty is mine": that is
			// precisely the two-nodes-one-vendor case this singleton
			// exists to rule out, and it would be entered by a store
			// blip rather than by a decision.
			log.WarnContext(ctx, "integration_duty_unknown", "error", err,
				"detail", "skipping this tick rather than risking a second node "+
					"reconciling the same surface")
			return
		}
		if !held {
			return
		}
	}

	states, err := w.load(ctx)
	if err != nil {
		log.WarnContext(ctx, "integration_state_unreadable", "error", err,
			"detail", "no surface is reconciled this tick; the previously "+
				"recorded status is still what the API serves")
		return
	}

	now := w.now().UTC()
	for _, kind := range w.order {
		if ctx.Err() != nil {
			return
		}
		state, known := states[kind]
		if !known {
			// FIRST SIGHT IS DUE NOW. A surface that has never been
			// reconciled has a zero NextAttemptAt, which is already in
			// the past, so this is only an explicit statement of what
			// the zero value already means.
			state = State{Kind: kind}
		}
		if !state.Due(now) {
			continue
		}
		w.reconcile(ctx, kind, state, now)
	}

	w.forgetDeparted(ctx, states)
}

// load reads the recorded state, keyed by kind.
func (w *Worker) load(ctx context.Context) (map[Kind]State, error) {
	rows, err := w.store.LoadIntegrations(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[Kind]State, len(rows))
	for _, row := range rows {
		out[row.Kind] = row
	}
	return out, nil
}

// reconcile runs one surface and records what it found.
func (w *Worker) reconcile(ctx context.Context, kind Kind, state State, now time.Time) {
	reg := w.byKind[kind]
	findings, err := reg.Reconciler.Reconcile(ctx)

	state.Kind = kind
	state.LastAttemptAt = now

	switch {
	case errors.Is(err, ErrNotConfigured):
		// The block left the company document between this pass being
		// scheduled and it running. Forgotten rather than recorded: a
		// status for a surface nobody configured would sit in the fleet
		// view describing an integration that is gone.
		if err := w.store.ForgetIntegration(ctx, kind); err != nil {
			log.WarnContext(ctx, "integration_status_not_forgotten",
				"integration", kind.String(), "error", err)
		}
		return
	case err != nil:
		// A FAULT IS A WAIT. Almost every one is a vendor briefly
		// unreachable, and none of the rest is fixed by giving up. The
		// findings are dropped rather than kept: a pass that failed did
		// not observe the world, and rendering a previous pass's
		// observations under this pass's timestamp would age a stale
		// answer into a current one.
		state.Report = Report{
			Phase: PhaseActivating, Actor: ActorEngine,
			Detail: "the last pass could not read this integration",
		}
		state.Findings = nil
		state.Attempts++
		state.LastError = truncateError(err.Error())
		log.WarnContext(ctx, "integration_reconcile_failed",
			"integration", kind.String(), "attempts", state.Attempts, "error", err)
	default:
		state.Report = Classify(findings)
		state.Findings = findings
		state.LastError = ""
		if state.Report.Phase == PhaseReady {
			state.Attempts = 0
			state.SettledAt = now
		} else {
			state.Attempts++
		}
	}

	state.Outcome = state.Report.Outcome()
	state.NextAttemptAt = now.Add(w.schedule.next(state.Report, state.Attempts, reg.Settled))

	if err := w.store.SaveIntegration(ctx, state); err != nil {
		// The pass still happened, and its work at the vendor is durable.
		// What is lost is the RECORD of it, so the next tick re-runs a
		// pass that has nothing left to do, which is the cheap failure.
		log.WarnContext(ctx, "integration_status_unrecorded",
			"integration", kind.String(), "error", err)
	}
}

// forgetDeparted drops state for a surface the company document no longer
// declares.
//
// A row nothing reconciles would otherwise sit in the fleet's status forever,
// reporting whatever it last found about an integration that is gone. It
// removes the RECORD and nothing else: see [Store.ForgetIntegration].
func (w *Worker) forgetDeparted(ctx context.Context, states map[Kind]State) {
	for kind := range states {
		if _, live := w.byKind[kind]; live {
			continue
		}
		if err := w.store.ForgetIntegration(ctx, kind); err != nil {
			log.WarnContext(ctx, "integration_status_not_forgotten",
				"integration", kind.String(), "error", err)
		}
	}
}
