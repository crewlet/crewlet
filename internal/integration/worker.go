package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
// the process already holds. Nothing is fetched from a third-party app unless a
// third-party app is due.
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
//     third-party app already has separates a repair from a no-op.
//   - MUST NOT rotate a credential that still works. A third-party app serves a token
//     once, so the tempting reading of "reconcile" is to mint every pass, and
//     that is an outage on a timer: the engine is authenticating with the old
//     value, and rotating revokes what every running agent is using. Check
//     what the sink recorded (provision.TokenSink.Value) and keep a working
//     credential. Rotation is an operator gesture on the third-party app subcommand,
//     where somebody typed the flag.
//   - MUST NOT delete anything a seat's departure implies. Decommissioning is
//     the other flag on that subcommand, for the same reason.
//   - SHOULD cost nothing when nothing has changed. A converged company is
//     the steady state and it is the state this loop spends most of its life
//     in, so a pass that re-reads every seat from the third-party app every ten
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
	// the loop classifies and reports. An error is the engine or the third-party app
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

// ErrCredentialRejected reports a pass that failed because the VENDOR refused
// the credential, rather than because the third-party app could not be reached.
//
// A third-party app wraps this around its own error when it can tell the difference —
// an auth probe that came back 401 or 403 — and [Observe] then reports the
// surface as the operator's to fix instead of folding it in with the
// transport faults that clear on their own. Without it every refusal read as
// "the engine is working on it", which is the one thing that is certainly not
// happening: the credential will be refused identically on every pass until a
// person changes it.
var ErrCredentialRejected = errors.New("integration: the third-party app refused this credential")

// ErrDisconnectUnavailable reports a node that cannot complete a disconnect
// RIGHT NOW, as distinct from one that failed to.
//
// The loop is armed when the engine is constructed, and the surface a
// disconnect removes a block through is installed later, when the API is
// wired. A tick in that window must leave the row exactly as it found it: a
// recorded attempt would back the retry off for the node that is about to be
// able to do it, and a recorded fault would put an error on the screen for a
// disconnect that is simply early.
var ErrDisconnectUnavailable = errors.New("integration: this node cannot complete a disconnect yet")

// Reject marks err as a credential refusal when the third-party app answered with an
// authentication or authorization status, and returns it untouched otherwise.
//
// The POLICY lives here and the extraction stays with each third-party app, because
// they are different problems: which statuses mean "your credential is no
// good" is one rule for every third-party app, while digging the number out of a
// refusal is a question about that third-party app's own error type. Written per
// third-party app, the rule drifts — 401 alone in one place and 401-or-403 in the
// next — and the surfaces that forgot 403 are exactly the ones that sit in
// "the engine is working on it" forever.
//
// 401 and 403 only. A 429 is rate limiting and clears, a 404 is usually the
// wrong host or project rather than the wrong key, and a 5xx is the third-party app's
// own problem: every one of those is a genuine wait.
func Reject(err error, status int) error {
	if err == nil || (status != http.StatusUnauthorized && status != http.StatusForbidden) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrCredentialRejected, err)
}

// Refusal says how to describe a failed credential check.
//
// A REFUSAL IS A CLAIM, and it is only true when the third-party app actually
// made it. Three reconcilers had "was refused" written into the sentence, so
// every other way a check can fail — a 404 from a path this build got wrong, a
// 500, a timeout, a name that does not resolve — reported a working credential
// as rejected and sent an operator to rotate a key that was fine. Exactly that
// happened: Datadog's verify call named a route that does not exist, answered
// 404, and the pass reported the organization credentials refused.
//
// Pass the error AFTER [Reject] has classified it; the wording follows the
// classification rather than guessing at it a second time.
func Refusal(err error) string {
	if errors.Is(err, ErrCredentialRejected) {
		return "was refused"
	}
	return "could not be verified"
}

// kind is the surface a registration is for.
//
// The reconciler answers when there is one, because a reconciler that
// disagreed with a hand-written kind would converge one surface under
// another's status row. [Registration.Only] answers when there is not.
func (r Registration) kind() Kind {
	if r.Reconciler != nil {
		return r.Reconciler.Kind()
	}
	return r.Only
}

// Disconnector removes a surface: what it holds at the third-party app, and then its
// block in the company document.
//
// ONE CALL FOR BOTH, and the ORDER inside it is the whole reason this is a
// single seam rather than two. The block holds the credential the third-party app
// teardown authenticates with, so removing it first strands whatever the
// third-party app still has. A caller holding two seams could do them the wrong way
// round; one cannot.
//
// The implementation lives where config writes do. This package knows only
// that the surface is gone when it returns nil.
type Disconnector interface {
	// Disconnect removes what this surface holds and then its block.
	//
	// An error leaves everything in place and the surface disconnecting,
	// so it is retried. Every step must be safe to repeat.
	Disconnect(ctx context.Context, removeSeats bool) error
}

// Registration is one reconciler and what is specific to its cadence.
type Registration struct {
	Reconciler Reconciler

	// Only names the surface for a registration with no reconciler.
	//
	// Ignored when Reconciler is set, which is the authority on its own
	// kind. It exists for the teardown-only case: a surface the loop can
	// REMOVE but must never converge, because converging it would mint.
	Only Kind

	// Disconnector removes this surface, or nil for a build that cannot.
	//
	// OPTIONAL because the loop must keep running for every other surface
	// on a node that cannot tear one down: a fleet whose passes are not
	// wired still reconciles, and a disconnect asked for there waits for
	// a node that can rather than failing the tick.
	Disconnector Disconnector

	// Settled overrides [Schedule.Settled] for this surface. Zero takes the
	// schedule's own value, which is the right answer for a third-party app with no
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
// nodes reconciling one surface at the same moment both read a third-party app that
// has no account for a seat, and both create one. The third-party app ends up with two
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
		// A REGISTRATION MUST DO ONE OF THE TWO THINGS. Most do both:
		// converge a surface and, when asked, remove it. Some can only
		// remove it, because their pass mints credentials and a timer
		// must not — GitLab and Mattermost create accounts, so a loop
		// that reconciled them would provision on a schedule nobody
		// asked for. Those register a Disconnector alone, which is what
		// [Registration.Only] names.
		if reg.Reconciler == nil && reg.Disconnector == nil {
			return nil, errors.New(
				"integration: a registration neither converges a surface nor " +
					"removes one, so the loop would have nothing to do with it")
		}
		kind := reg.kind()
		if kind == "" {
			return nil, errors.New(
				"integration: a teardown-only registration has no Only kind; " +
					"with no reconciler to ask, it has to name its own surface")
		}
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
		schedule: opts.Schedule.WithDefaults(), claim: opts.ClaimDuty,
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
			// precisely the two-nodes-one-third-party app case this singleton
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
		// A SURFACE BEING TAKEN AWAY IS NOT RECONCILED. Its block is
		// still in the document for the whole teardown — it carries the
		// credential the teardown authenticates with — so a reconcile
		// here would find it configured, converge it, and report it
		// healthy while somebody was waiting for it to go.
		if state.TearingDown() {
			w.tearDown(ctx, kind, state, now)
			continue
		}
		if w.byKind[kind].Reconciler == nil {
			// TEARDOWN-ONLY. There is nothing to converge here and a
			// row exists only while a disconnect is in flight, so a due
			// surface with no intent has nothing for this tick to do.
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

// tearDown removes one surface and records how far it got.
//
// The counterpart of [Worker.reconcile] for a surface somebody asked to
// disconnect, and it folds through [ObserveTeardown] for the same reason
// reconcile folds through [Observe]: what a status row says must not depend
// on which path produced it.
func (w *Worker) tearDown(ctx context.Context, kind Kind, state State, now time.Time) {
	reg := w.byKind[kind]
	if reg.Disconnector == nil {
		// THIS NODE CANNOT, which is not a failure of the disconnect. A
		// node whose roles leave the passes unwired still runs the loop,
		// and the surface waits for one that can rather than being
		// reported stuck. Nothing is written, so nothing has to be
		// undone when a capable node picks it up.
		log.WarnContext(ctx, "integration_teardown_unavailable",
			"integration", kind.String(),
			"detail", "this node cannot disconnect; another will")
		return
	}

	err := reg.Disconnector.Disconnect(ctx, state.RemoveSeats)
	if errors.Is(err, ErrDisconnectUnavailable) {
		// NOT YET, which is not the same as failed. Nothing is written,
		// for the reason the nil-disconnector case above states: an
		// attempt counted here backs off a retry that was about to work.
		log.InfoContext(ctx, "integration_teardown_deferred",
			"integration", kind.String(), "detail", err.Error())
		return
	}
	state, forget := ObserveTeardown(state, kind, err, now)
	if forget {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := w.store.ForgetIntegration(ctx, kind); err != nil {
			log.WarnContext(ctx, "integration_status_not_forgotten",
				"integration", kind.String(), "error", err)
		}
		log.InfoContext(ctx, "integration_disconnected",
			"integration", kind.String(), "removed_seats", state.RemoveSeats)
		return
	}

	log.WarnContext(ctx, "integration_teardown_failed",
		"integration", kind.String(), "attempts", state.Attempts, "error", err)
	state.NextAttemptAt = now.Add(w.schedule.Next(state.Report, state.Attempts, reg.Settled))
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := w.store.SaveIntegration(ctx, state); err != nil {
		// The third-party app work that DID land is durable; what is lost is the
		// record of the attempt, so the next tick tries again over a
		// teardown that is safe to repeat.
		log.WarnContext(ctx, "integration_status_unrecorded",
			"integration", kind.String(), "error", err)
	}
}

// reconcile runs one surface and records what it found.
func (w *Worker) reconcile(ctx context.Context, kind Kind, state State, now time.Time) {
	reg := w.byKind[kind]
	findings, err := reg.Reconciler.Reconcile(ctx)

	// THE FOLD IS integration.Observe, shared with the pass an operator
	// runs from the dashboard: a status row must not depend on which
	// surface produced it.
	state, forget := Observe(state, kind, findings, err, now)
	switch {
	case forget:
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := w.store.ForgetIntegration(ctx, kind); err != nil {
			log.WarnContext(ctx, "integration_status_not_forgotten",
				"integration", kind.String(), "error", err)
		}
		return
	case err != nil:
		log.WarnContext(ctx, "integration_reconcile_failed",
			"integration", kind.String(), "attempts", state.Attempts, "error", err)
	}

	state.NextAttemptAt = now.Add(w.schedule.Next(state.Report, state.Attempts, reg.Settled))

	if err := w.store.SaveIntegration(ctx, state); err != nil {
		// The pass still happened, and its work at the third-party app is durable.
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
