package integration

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/provision"
)

var log = logging.Get("integration")

// Interval is how often the loop wakes to see what is due.
//
// Fifteen seconds, the FINEST CADENCE [Schedule] can ask for
// ([Schedule.AdminBase]): a slower tick would make the shortest wait a lie.
// It is also the control plane's reconcile poll interval. A tick with nothing
// due is one duty claim and a read of the activation pointer plus one status
// row per node, with nothing fetched from a third-party app.
const Interval = 15 * time.Second

// WakeSettle is how long a config apply waits before the tick it brings
// forward runs.
//
// A COALESCING WINDOW. One action is routinely several applies (saving
// Atlassian writes three blocks), each marking the loop stale; ticking per
// apply would query the third-party apps three times for one Save.
//
// It is also the floor under a pass that writes the document on every run:
// that marks the loop stale from inside its own tick, so with no window the
// loop would spin against the app. A pass is expected to converge
// ([Worker.MarkStale] says why), but the failure must stay survivable.
const WakeSettle = 750 * time.Millisecond

// Reconciler is one surface's convergence step.
//
// Level triggered: asked what the world looks like now, since an event can be
// lost or arrive twice. It runs unattended, so it:
//   - MUST be idempotent.
//   - MUST NOT rotate a credential that still works: an app serves a token
//     once, and minting every pass revokes what running agents use. Check the
//     sink (provision.TokenSink.Value). Rotation is an operator gesture.
//   - MUST NOT delete anything a seat's departure implies (an operator flag).
//   - MUST NOT WRITE when nothing changed (integrationtest's "a converged
//     pass writes nothing"), though it reads enough to KNOW that; the settled
//     interval is sized against it: see [DefaultSchedule].
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

// Paced is a [Reconciler] whose surface holds something that lapses on a
// clock, such as an agent's grant with an expiry: it names when the next pass
// is due at the latest, so the lapse is taken then rather than on the settled
// interval. Zero is nothing due.
type Paced interface {
	DueBy(now time.Time) time.Time
}

// nextAttempt is when a surface is due again: its cadence's wait, or sooner
// where its reconciler names a lapse before that ([Paced]), never in the past.
func nextAttempt(r Reconciler, now time.Time, wait time.Duration) time.Time {
	next := now.Add(wait)
	if paced, ok := r.(Paced); ok {
		if due := paced.DueBy(now); !due.IsZero() && due.Before(next) {
			next = due
			if next.Before(now) {
				next = now
			}
		}
	}
	return next
}

// ErrNotConfigured reports a surface whose block has left the company.
//
// # Why a sentinel rather than a registration the apply rebuilds
//
// The company document is edited live, so configured surfaces move under a
// running loop. Rebuilding the worker on every apply would drop its fleet
// singleton lease on an unrelated config change and hand a peer the duty
// until the TTL lapses.
//
// So registration is static, every reconciler reads the LIVE config on each
// pass, and one that finds its own block gone says so. The loop then forgets
// its status, as it does for a kind nothing registered.
var ErrNotConfigured = errors.New("integration: this surface is not configured")

// ErrCredentialRejected reports a pass that failed because the third-party app
// REFUSED the credential, rather than because it could not be reached.
//
// A third-party app wraps this around its own error when it can tell the
// difference (an auth probe that came back 401 or 403), and [Observe] then
// reports the surface as the operator's to fix instead of folding it in with
// the transport faults that clear on their own. Without it every refusal read
// as "the engine is working on it", which is the one thing that is certainly
// not happening: the credential will be refused identically on every pass
// until a person changes it.
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

// ErrBlockNotRemoved reports a disconnect whose own teardown finished and
// whose last step, the revision that removes the integration from the company
// document, was refused.
//
// A DIFFERENT FAILURE FROM THE VENDOR'S, and said as one: nothing at the
// third-party app is in the way, so "the vendor has not let the disconnect
// finish" would send a person to look in the wrong place. What refused it is
// the company's own settings, and the error beneath names the setting.
var ErrBlockNotRemoved = errors.New("integration: the company's settings refused the change that removes this integration")

// Guard is the ONE claim every writer at one surface passes through, and the
// bound on the work it admits. The worker takes it, not the reconciler: freed
// when the pass returns, it would leave the status write outside, and a tick
// folding a stale row could overwrite an operator's disconnect.
//
// Three-valued: held false is knowledge (somebody else is writing, and two at
// once is one creating the account another deletes); an error is a failure to
// look, not a statement that the surface is idle.
//
// The returned context carries the pass deadline and must DERIVE from ctx, so
// a stopping worker reaches the reconciler. release is non-nil exactly when
// held. A nil Guard is unguarded.
type Guard func(ctx context.Context, kind Kind) (
	bounded context.Context, release func(), held bool, err error)

// AdmitsFunc reports whether this node's configuration is current enough to
// converge a third-party app.
//
// On a shed or stuck node the LIVE company document is an epoch the fleet has
// replaced; converging an app to it undoes the current revision.
//
// Asked here, not folded into the shared worker-duty helper, which would gate
// every fleet singleton: roles and posture come from subsystems that do not
// consult each other, so an ingress+workers fleet whose worker node fails one
// apply would run NO singleton with /ready green. Gated here, a shedding node
// declines this one duty and a peer takes it. Nil admits.
type AdmitsFunc func() bool

// Reject marks err as a credential refusal when the third-party app answered with an
// authentication or authorization status, and returns it untouched otherwise.
//
// The POLICY lives here and the extraction stays with each third-party app:
// which statuses mean "your credential is no good" is one rule, while digging
// the number out of a refusal depends on that app's error type. Written per
// integration the rule drifts (401 alone vs 401-or-403), and a surface that
// forgot 403 sits in "the engine is working on it" forever.
//
// 401 and 403 only. A 429 clears, a 404 is usually the wrong host or project
// rather than the wrong key, and a 5xx is the app's own problem: genuine waits.
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
	Disconnect(ctx context.Context, req TeardownRequest) (provision.Removed, error)
}

// TeardownRequest is what a disconnect was asked to do, as its row records it.
type TeardownRequest struct {
	// RemoveSeats is [State.RemoveSeats]: whether the accounts go too.
	RemoveSeats bool

	// By is who asked ([State.RequestedBy]), and every write the teardown
	// makes is recorded under them.
	By iam.Actor
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

	// Guard serializes every writer at one surface and bounds the pass. See
	// [Guard]; nil runs the loop unguarded.
	Guard Guard

	// Admits gates the tick on this node's config posture, BEFORE the duty
	// is claimed so a shedding node's lease lapses and a peer can take it.
	// See [AdmitsFunc]; nil admits.
	Admits AdmitsFunc

	// Configured reports whether the company document still declares a
	// surface, and is consulted for TEARDOWN-ONLY kinds alone.
	//
	// A surface with a reconciler answers better: its pass returns
	// [ErrNotConfigured] (and knows `enabled: false` counts), and consulting
	// both would give a paused integration two disagreeing authorities.
	// A teardown-only kind (Slack) has no pass, and its setup-form row would
	// outlive the `slack:` block, so a reconnect inherits a stale address.
	//
	// Nil means this node cannot say, and nothing is forgotten on a guess.
	Configured func(Kind) bool

	// Interval overrides [Interval]. Zero takes it.
	Interval time.Duration

	// WakeSettle overrides [WakeSettle], the window a config apply's tick
	// waits out first. Zero takes it; a test shrinks it so a suite that
	// exercises the wake does not spend its life in timers.
	WakeSettle time.Duration

	// Endpoint reports the public base URL third-party apps reach this
	// deployment on, as the applied revision holds it right now.
	//
	// STAMPED ON EVERY ROW A PASS WRITES, so a later read can tell the
	// address a surface was registered against from the one in force. Nil
	// leaves the field empty, which reads as "not recorded" and never as
	// "moved".
	Endpoint func() string

	// Registration reports the NAME this surface's registration is held
	// under at the third-party app right now, for a surface where the
	// address is not enough to find it again.
	//
	// Datadog is the case: a webhook definition is addressed by a live config
	// name and Datadog serves no listing (a GET on the collection answers
	// 405), so a renamed field orphans the first definition for good.
	//
	// Compared with what was recorded, so the orphan is REPORTED, not
	// deleted: monitors still naming it deliver correctly, and removing it
	// would silence exactly those. Empty for a kind with no such name; nil
	// leaves every row's field empty.
	Registration func(Kind) string

	// SettledInterval is how long a CONVERGED surface is trusted before it
	// is read back, as the applied revision holds it right now.
	//
	// READ FRESH ON EVERY PASS, not folded into [Options.Schedule]: the
	// document is edited live, and a captured value would make an operator
	// who shortened the interval wait out the OLD one, the very wait they
	// meant to shorten.
	//
	// It overrides [Schedule.Settled] only: the other waits are retries of
	// something known wrong and their cost does not scale with the company.
	// Nil, or a non-positive value, takes the Schedule's own.
	SettledInterval func() time.Duration

	// Now is the clock, for tests. Nil is time.Now.
	Now func() time.Time

	// Spread scatters a computed wait, so surfaces that settled together do
	// not stay together. Nil is [spreadWait]; a test pins it to identity so
	// it can assert an exact instant.
	Spread func(time.Duration) time.Duration
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
	byKind       map[Kind]Registration
	order        []Kind
	store        Store
	schedule     Schedule
	claim        DutyFunc
	guard        Guard
	admits       AdmitsFunc
	configured   func(Kind) bool
	endpoint     func() string
	registration func(Kind) string
	settled      func() time.Duration
	interval     time.Duration
	spread       func(time.Duration) time.Duration
	settle       time.Duration
	now          func() time.Time

	// wake carries a config apply to the loop, so the tick that
	// reconsiders comes now rather than at the end of the cadence. Buffered
	// by one and never blocked on: a queued wake and two queued wakes are
	// the same tick. See [Worker.MarkStale].
	wake chan struct{}

	mu      sync.Mutex
	stop    context.CancelFunc
	stopped chan struct{}

	// unconfigured is every surface whose last pass said the company has no
	// block for it.
	//
	// IN MEMORY AND NOT A ROW: the absence of a row is what [Observe]'s
	// `forget` means, and a row would put every unconfigured vendor on an
	// operator's screen. But forgetting also un-paced it (no row, so a zero
	// [State] whose NextAttemptAt is past), and a company configuring none
	// reconciled all of them every interval.
	//
	// Cleared by `stale`, the one thing that can change the answer: a surface
	// becomes configured by a document edit, and an apply marks the loop stale.
	unconfigured sync.Map

	// stale is set when the document every recorded conclusion was drawn
	// from has been replaced, so the next tick reconsiders every surface
	// whatever its cadence says. See [Worker.MarkStale].
	stale atomic.Bool

	// refreshing holds the surfaces something has said to look at NOW,
	// whatever their cadence. See [Worker.Refresh].
	//
	// A SET RATHER THAN A SECOND BOOL, because the reason it exists is that
	// it is narrower than [Worker.stale]: a config apply changes the answer
	// for every surface, while a person finishing something at ONE
	// third-party app changes it for one. Sweeping all eight on a redirect
	// from GitHub spends seven other vendors' rate limit on a click that
	// said nothing about them.
	refreshing sync.Map

	// shed remembers that the last tick was declined on posture, so the
	// refusal is logged on the TRANSITION rather than every fifteen seconds
	// for as long as a node stays behind. A silent refusal is what makes a
	// stalled loop indistinguishable from a converged one, which is the
	// failure this whole subsystem exists to remove — so it is logged once
	// going in and once coming out, and never in between.
	shed atomic.Bool
}

// MarkStale says the company configuration has changed, so what the loop last
// concluded was drawn from a document that is no longer current.
//
// THE CADENCE IS FOR A THIRD-PARTY APP, NOT FOR A DOCUMENT. A settled surface
// is asked again in minutes, wrong once the answer changes HERE: an operator
// who installs an agent's app is redirected to a card still holding the old
// finding ("this agent has no app of its own").
//
// In memory, not written through the store: every node sees the apply and
// only the one holding the duty acts on it. A pass this triggers may itself
// write the document (adopting an installation) and mark the loop stale
// again; it converges, as the second pass writes nothing.
func (w *Worker) MarkStale() {
	if w == nil {
		return
	}
	w.stale.Store(true)
	// AND THE TICK COMES FORWARD, which is the half the flag alone cannot
	// do. Marked and left to the cadence, the promise is only that the
	// NEXT tick reconsiders: an operator who just pressed Save watched a
	// card describe the configuration they had replaced for as long as the
	// interval had left to run, and read it as the save not working.
	//
	// Never blocking, on a channel that may be nil on a Worker a test
	// built by hand: a send that is not ready takes the default, and the
	// flag above is what the ordinary tick reads anyway.
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Refresh says ONE surface's state AT THE THIRD-PARTY APP may have changed,
// so the next tick asks it again rather than waiting out its cadence.
//
// The cadence is a BACKOFF ([Schedule.Next]: fifteen seconds to ten minutes)
// from asking a person to act at their app, so the moment they DO act is when
// it is longest and most wrong. Nothing about the caller is believed: this
// says LOOK NOW, and the pass that follows is the ordinary verified one, so it
// is safe from an unauthenticated redirect (a rate concern, not a trust one).
// An unknown kind is kept, not refused: it is simply never taken out.
func (w *Worker) Refresh(kind Kind) {
	if w == nil {
		return
	}
	w.refreshing.Store(kind, struct{}{})
	// Same non-blocking wake as [Worker.MarkStale], for the same reason:
	// the flag alone only promises that the NEXT tick reconsiders, and the
	// next tick is the interval this exists to not wait for.
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// takeRefresh reports whether this surface was asked to look now, and clears
// the ask.
//
// CLEARED AS IT IS READ, so one redirect costs one pass: left set, every
// later tick would ignore the cadence for that surface for ever, which is
// the cadence deleted rather than brought forward.
func (w *Worker) takeRefresh(kind Kind) bool {
	_, found := w.refreshing.LoadAndDelete(kind)
	return found
}

// New builds the worker, refusing a registration set it could not run.
func New(opts Options) (*Worker, error) {
	byKind := make(map[Kind]Registration, len(opts.Registrations))
	order := make([]Kind, 0, len(opts.Registrations))
	for _, reg := range opts.Registrations {
		// A REGISTRATION MUST DO ONE OF THE TWO THINGS. Most do both:
		// converge a surface and, when asked, remove it. A surface this
		// build has no pass for can only be removed: Slack's apps are
		// created from the command line, so there is nothing to converge,
		// and it registers a Disconnector alone, which is what
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
	//
	// [ConvergeOrder], NOT [Kinds]: the second is the order an operator
	// READS them and the first is the order they depend on each other in.
	// Atlassian creates the account Jira and Confluence authenticate as, so
	// visiting the products first asks about an account one pass away from
	// existing — which the card drew as a 401 an operator had to act on.
	slices.SortFunc(order, func(a, b Kind) int {
		return slices.Index(ConvergeOrder, a) - slices.Index(ConvergeOrder, b)
	})

	interval := opts.Interval
	if interval <= 0 {
		interval = Interval
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	settle := opts.WakeSettle
	if settle <= 0 {
		settle = WakeSettle
	}
	spread := opts.Spread
	if spread == nil {
		spread = spreadWait
	}
	return &Worker{
		byKind: byKind, order: order, store: opts.Store,
		schedule: opts.Schedule.WithDefaults(), claim: opts.ClaimDuty,
		guard:        opts.Guard,
		admits:       opts.Admits,
		configured:   opts.Configured,
		endpoint:     opts.Endpoint,
		registration: opts.Registration,
		settled:      opts.SettledInterval,
		interval:     interval, settle: settle, now: now, spread: spread,
		wake: make(chan struct{}, 1),
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
		case <-w.wake:
			// A CONFIG APPLY. The cadence is for asking a third-party app
			// again; this is the answer changing here, so it does not
			// wait. See [Worker.MarkStale].
			if !w.settleWake(ctx) {
				log.InfoContext(ctx, "integration_reconciler_stopping")
				return
			}
			w.Tick(ctx)
		}
	}
}

// settleWake waits out the coalescing window, absorbing the applies that
// arrive inside it, and reports whether the loop should go on.
//
// A wake landing DURING the tick that follows is not absorbed here: it sits
// in the buffer and fires afterwards, which is what makes a document a pass
// changed get reconsidered rather than swallowed by the pass that changed it.
func (w *Worker) settleWake(ctx context.Context) bool {
	timer := time.NewTimer(w.settle)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-w.wake:
			// Another apply, already covered by the tick this one is
			// waiting for. Absorbed rather than queued: one operator
			// action is one pass.
		case <-timer.C:
			return true
		}
	}
}

// Tick runs one pass over whatever is due.
//
// Exported so the loop can be driven directly by a test and by an operator's
// explicit "reconcile now", without either having to wait out an interval or
// start a goroutine.
func (w *Worker) Tick(ctx context.Context) {
	// POSTURE FIRST, BEFORE THE CLAIM. A node whose configuration the fleet
	// has already replaced must not converge a third-party app to it — see
	// [AdmitsFunc]. Asked ahead of the duty deliberately: declining without
	// claiming lets this node's lease lapse, so a worker-capable peer takes
	// the loop over rather than waiting behind a holder that does nothing.
	if w.admits != nil && !w.admits() {
		if !w.shed.Swap(true) {
			log.WarnContext(ctx, "integration_reconcile_shed",
				"detail", "this node's configuration is behind the fleet's, so it "+
					"is not converging any integration; a peer holding the current "+
					"revision takes the duty when this node's lease lapses")
		}
		return
	}
	if w.shed.Swap(false) {
		log.InfoContext(ctx, "integration_reconcile_resumed",
			"detail", "this node's configuration is current again")
	}

	if w.claim != nil {
		held, err := w.claim(ctx)
		if err != nil {
			// UNKNOWN IS NOT LOST. A coordination store that could not
			// answer must not be read as "the duty is mine": that is
			// precisely the two-nodes-one-integration case this singleton
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
	// READ ONCE, AND CLEARED, so one apply costs one sweep rather than
	// leaving every later tick ignoring the cadence for ever.
	stale := w.stale.Swap(false)
	if stale {
		// THE DOCUMENT MOVED, so every "this company has no such
		// integration" is a conclusion drawn from a document that is no
		// longer the current one.
		w.unconfigured.Clear()
	}
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
		// TAKEN BEFORE THE FILTER, AND ALWAYS, so an ask that arrives on a
		// tick already sweeping for a config apply is spent rather than
		// left to force a second one. `||` is short-circuit and this is
		// its left operand for exactly that reason.
		forced := w.takeRefresh(kind) || stale
		// A CHEAP FILTER ON A ROW THAT MAY BE SECONDS OLD, which is all this
		// has to be: it decides whether taking the guard is worth a round
		// trip. The row the WORK is decided on is re-read inside — see
		// [Worker.visit].
		if !forced && !state.Due(now) {
			continue
		}
		// A SURFACE THE COMPANY DOES NOT HAVE IS NOT ASKED AGAIN until
		// the document that would give it one changes. Its pass is the
		// only thing that can say so, and saying so forgets the row that
		// would otherwise pace it — see [Worker.unconfigured].
		if !stale {
			if _, absent := w.unconfigured.Load(kind); absent {
				continue
			}
		}
		// STILL OURS? The duty was claimed once, before this loop, and its
		// TTL is derived from the deadline one pass may take — while the
		// loop below makes network calls to every configured surface in
		// turn. A sweep across eight vendors outlives that TTL easily, and
		// a duty that lapsed mid-tick is a second node already sweeping the
		// surfaces this one has not reached.
		//
		// RE-CLAIMED HERE, past the not-due check, so the cost lands only
		// on a tick with work to do: a claim is one round trip on a
		// connection the process already holds, and a tick that reconciles
		// nothing makes none. For the same owner it doubles as the renew,
		// so the holder keeps the duty by using it.
		if !w.stillHoldsDuty(ctx) {
			return
		}
		w.visit(ctx, kind, forced, now)
	}

	w.forgetDeparted(ctx, states)
}

// visit takes the surface's guard and does whatever the row then says.
//
// Everything that decides the work happens INSIDE the guard: the row is
// re-read and routed (converge, tear down, nothing) from THAT row, not the
// snapshot [Worker.Tick] filtered on. Folding into an earlier row overwrites
// an operator's disconnect; routing on it hands a disconnect landing between
// load and guard a CONVERGE pass, which finds the block still present and
// reports the surface healthy. The due check is repeated too: a dashboard
// pass meanwhile has moved NextAttemptAt, sparing the app a repeated ask.
func (w *Worker) visit(ctx context.Context, kind Kind, forced bool, now time.Time) {
	bounded, release, held, err := w.hold(ctx, kind)
	switch {
	case err != nil:
		// UNKNOWN IS NOT FREE. A coordination store that could not answer
		// has not said the surface is idle, and acting on that guess is
		// what creates the duplicate. Nothing is recorded: an attempt
		// counted here would back off a cadence for a pass that never ran.
		log.WarnContext(ctx, "integration_surface_unknown",
			"integration", kind.String(), "error", err,
			"detail", "this tick could not learn whether another writer holds "+
				"this surface, so it did not touch it")
		return
	case !held:
		// SOMEBODY IS ALREADY DOING THIS (an operator's pass, an access sync,
		// a teardown). Nothing is recorded: a fault written here would
		// describe as broken a surface being provisioned successfully.
		//
		// A FORCED LOOK STAYS OWED. Its ask (a config apply's sweep, a
		// refresh) was spent on the way here, so dropping it would leave the
		// card describing the replaced setting for the settled cadence (a
		// save starts an access sync that takes the lease first). Kept as a
		// refresh, the next tick asks again.
		if forced {
			w.refreshing.Store(kind, struct{}{})
		}
		log.InfoContext(ctx, "integration_reconcile_deferred",
			"integration", kind.String(),
			"detail", "another writer holds this surface; the next tick tries again")
		return
	}
	defer release()

	state, found, err := w.store.LoadIntegration(ctx, kind)
	if err != nil {
		log.WarnContext(ctx, "integration_state_unreadable",
			"integration", kind.String(), "error", err,
			"detail", "this surface is not reconciled this tick; nothing is "+
				"written over a row this node could not read")
		return
	}
	if !found {
		state = State{Kind: kind}
	}
	if !forced && !state.Due(now) {
		return
	}
	if state.TearingDown() {
		w.tearDown(ctx, bounded, kind, state, now)
		return
	}
	if w.byKind[kind].Reconciler == nil {
		// TEARDOWN-ONLY. There is nothing to converge here, so a due
		// surface with no disconnect in flight has nothing for this tick
		// to do. Its row is not necessarily empty — the setup form stamps
		// an address on exactly this class of surface — but only
		// [Worker.forgetDeparted] has anything to say about that.
		return
	}
	w.reconcile(ctx, bounded, kind, state, now)
}

// hold takes the surface's guard, or waves the tick through where there is
// none.
//
// A nil Guard is a worker with no lease to take: a test's, and a node with no
// keyring, where there is no second writer in the process to serialize
// against. The context is handed back unchanged there, because the deadline
// belongs to the guard that would have been protecting the pass.
func (w *Worker) hold(ctx context.Context, kind Kind) (
	context.Context, func(), bool, error,
) {
	if w.guard == nil {
		return ctx, func() {}, true, nil
	}
	return w.guard(ctx, kind)
}

// stillHoldsDuty re-claims the singleton before a unit of work.
//
// UNKNOWN STOPS THE SWEEP, exactly as it stops the tick that began it: a
// coordination store that could not answer has not said the duty is still
// ours, and carrying on would be entering the two-nodes-one-surface case on a
// guess rather than on a decision.
func (w *Worker) stillHoldsDuty(ctx context.Context) bool {
	if w.claim == nil {
		return true
	}
	held, err := w.claim(ctx)
	if err != nil {
		log.WarnContext(ctx, "integration_duty_unknown", "error", err,
			"detail", "stopping this sweep rather than risking a second node "+
				"reconciling the surfaces it has not reached")
		return false
	}
	if !held {
		log.InfoContext(ctx, "integration_duty_lost",
			"detail", "another node holds the reconcile duty; this sweep stops here")
	}
	return held
}

// spreadWait scatters a wait by up to a tenth of itself, so surfaces that
// settled together do not stay together.
//
// Surfaces are stamped from one clock reading with one settled interval, so
// integrations converging on the same tick stay due together for ever: a
// burst of every vendor's API at once instead of a trickle.
//
// A TENTH, and only downward, because the interval is also a promise:
// [Schedule.Settled] is the horizon on which a hand-revoked agent access is
// noticed, and stretching it would make that longer than it says. It
// decorrelates every surface of [Kinds] within one tick of each other.
func spreadWait(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - time.Duration(rand.Int64N(int64(d)/10+1))
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
func (w *Worker) tearDown(
	ctx, bounded context.Context, kind Kind, state State, now time.Time,
) {
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

	// THE THIRD-PARTY APP ON THE BOUNDED CONTEXT, the record on the tick's.
	// They are different deadlines on purpose: the pass runs inside the lease
	// that protects it, and the write that records the pass runs in the margin
	// the lease deliberately keeps behind it. Recording on the bounded one
	// would lose the record of every teardown that used its whole budget.
	by, _ := state.RequestedBy.Actor()
	removed, err := reg.Disconnector.Disconnect(bounded, TeardownRequest{
		RemoveSeats: state.RemoveSeats, By: by,
	})
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
		// THE ONLY RECORD A DESTRUCTIVE ACT LEAVES. This line carried the
		// kind and a bool, so "we deleted eleven service accounts and
		// deleted the credentials they held" and "we removed a webhook"
		// were the same sentence in an operator's log.
		//
		// NAMES, NEVER VALUES: the secrets are the variable names whose
		// rows this disconnect deleted.
		log.InfoContext(ctx, "integration_disconnected",
			"integration", kind.String(), "removed_seats", state.RemoveSeats,
			"by", by.Name, "operator", by.OperatorID,
			"accounts", removed.Handles(), "secrets", removed.Secrets())
		return
	}

	log.WarnContext(ctx, "integration_teardown_failed",
		"integration", kind.String(), "attempts", state.Attempts, "error", err)
	state.NextAttemptAt = now.Add(w.spread(w.scheduleNow().Next(state.Report, state.Attempts)))
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := w.store.RecordPass(ctx, state); err != nil {
		// The third-party app work that DID land is durable; what is lost is the
		// record of the attempt, so the next tick tries again over a
		// teardown that is safe to repeat.
		log.WarnContext(ctx, "integration_status_unrecorded",
			"integration", kind.String(), "error", err)
	}
}

// currentEndpoint is the public base in force, or empty where this node
// cannot say.
func (w *Worker) currentEndpoint() string {
	if w.endpoint == nil {
		return ""
	}
	return w.endpoint()
}

// currentRegistration is the name this surface's registration is held under
// right now, or empty where the question does not apply or this node cannot
// say.
func (w *Worker) currentRegistration(kind Kind) string {
	if w.registration == nil {
		return ""
	}
	return w.registration(kind)
}

// reconcile runs one surface and records what it found.
//
// Both halves run under the guard [Worker.visit] holds, which is what makes
// the status write safe: coord.Integrations is last-write-wins with no
// compare-and-set, and the lease held across the read, the pass and the write
// is the whole of why no second writer can lose an update here.
func (w *Worker) reconcile(
	ctx, bounded context.Context, kind Kind, state State, now time.Time,
) {
	reg := w.byKind[kind]
	// THE PASS ON THE BOUNDED CONTEXT, so it cannot outlive the lease
	// protecting it; the record below on the tick's, so the margin the lease
	// keeps behind that deadline is what the write actually gets to use.
	findings, err := reg.Reconciler.Reconcile(bounded)

	// WHAT THE REGISTRATION IS HELD UNDER, taken BEFORE the fold because
	// the orphan a rename leaves behind is a finding like any other and has
	// to be classified with the rest. See [StampRegistration].
	findings = append(findings, StampRegistration(&state, w.currentRegistration(kind))...)

	// THE FOLD IS integration.Observe, shared with the pass an operator
	// runs from the dashboard: a status row must not depend on which
	// surface produced it. It carries the field stamped above through.
	LogWhole(ctx, kind, findings, err)
	state, forget := Observe(state, kind, findings, err, now)
	// THE ADDRESS THIS PASS RAN AGAINST, on every pass rather than only a
	// successful one: what it answers is "where is this surface's
	// registration pointing", and a pass that failed still registered
	// against the base it was given. Which surfaces that applies to is
	// [StampEndpoint]'s to decide, because the dashboard's pass writes these
	// same rows and the two must not disagree.
	StampEndpoint(&state, kind, w.currentEndpoint())
	switch {
	case forget:
		// AND IT IS NOT ASKED AGAIN until the company document moves.
		// Forgetting the row is right — there is nothing to report about
		// a surface nobody configured — and it is also what left the
		// pass running every tick, because the row is what paces it.
		if errors.Is(err, ErrNotConfigured) {
			w.unconfigured.Store(kind, struct{}{})
		}
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

	state.NextAttemptAt = nextAttempt(reg.Reconciler, now, w.spread(w.scheduleNow().Next(state.Report, state.Attempts)))

	if err := w.store.RecordPass(ctx, state); err != nil {
		// The pass still happened, and its work at the third-party app is durable.
		// What is lost is the RECORD of it, so the next tick re-runs a
		// pass that has nothing left to do, which is the cheap failure.
		log.WarnContext(ctx, "integration_status_unrecorded",
			"integration", kind.String(), "error", err)
	}
}

// scheduleNow is the cadence as the applied revision holds it.
//
// A COPY WITH ONE FIELD SUBSTITUTED, because [Schedule] is a value and
// [Schedule.Next] reads Settled only on the settled branch. Nothing is stored
// back: the next pass asks again, which is what makes an operator's edit take
// effect on the pass after it rather than after the interval they replaced.
func (w *Worker) scheduleNow() Schedule {
	schedule := w.schedule
	if w.settled == nil {
		return schedule
	}
	if d := w.settled(); d > 0 {
		schedule.Settled = d
	}
	return schedule
}

// StampEndpoint records the address a pass ran against, where that pass is
// what keeps the address current.
//
// THREE-WAY on [Kind.Ingress]: IngressEngine (a pass registers the delivery,
// so the address it ran against is the registered one) stamps; IngressNone
// (nothing delivers) records none, or it would become an unactionable false
// alarm once the base moved; IngressOperator (a person typed it at the app)
// must not, or the one warning about a moved address becomes a green row.
//
// EXPORTED because two writers share these rows, the loop's tick and an
// operator's dashboard pass, and the rule must not differ between them.
func StampEndpoint(state *State, kind Kind, current string) {
	if kind.Ingress() == IngressEngine {
		state.Endpoint = current
	}
}

// StampRegistration records the name this surface's registration is now held
// under, and reports the one the previous name left behind.
//
// A NAME IS NOT AN ADDRESS, hence separate from [StampEndpoint]: changing a
// name (Datadog's webhook definition) creates a second registration and
// abandons the first, which keeps delivering for monitors still naming it.
// SO IT IS REPORTED, NEVER DELETED: removal would silence them, and Datadog
// serves no listing to find it again. The advisory is the operator's only way
// to learn it exists. EXPORTED for [StampEndpoint]'s reason.
func StampRegistration(state *State, current string) []Finding {
	previous := state.Registration
	state.Registration = current
	if previous == "" || current == "" || previous == current {
		return nil
	}
	return []Finding{{
		Kind:    FindingRegistrationOrphaned,
		Subject: previous,
		Title:   "An old registration is still delivering",
		Detail: fmt.Sprintf(
			"this engine registered %q and now registers %q. %q is still "+
				"there, unmanaged, and keeps delivering, so it was not removed",
			previous, current, previous),
		Remedy: fmt.Sprintf("Repoint anything naming %q at %q, then delete %q "+
			"at the third-party app", previous, current, previous),
	}}
}

// forgetDeparted drops the row of a TEARDOWN-ONLY surface the company
// document no longer declares.
//
// Narrow on purpose: a surface with a reconciler is forgotten by its own pass
// ([ErrNotConfigured]; it knows `enabled: false` counts), and two authorities
// would flap a paused integration. This answers only for Slack, which has no
// pass and whose setup-form row would leak its address into a reconnect.
// Never forgotten here: a surface with a reconciler; a surface being torn
// down (a failed teardown holds [PhaseDisconnecting] while the block may be
// gone, and forgetting would abandon it with webhooks live); a kind this
// build does not know, so an older node in a rolling upgrade never erases a
// newer peer's status.
func (w *Worker) forgetDeparted(ctx context.Context, states map[Kind]State) {
	if w.configured == nil {
		// This node cannot say what the document declares, and a guess
		// here deletes the one warning an operator gets about a moved
		// address.
		return
	}
	for _, kind := range w.order {
		state, recorded := states[kind]
		switch {
		case !recorded, w.byKind[kind].Reconciler != nil,
			state.TearingDown(), w.configured(kind):
			continue
		}
		w.forget(ctx, kind)
	}
}

// forget removes one departed surface's row, under the surface's own guard.
//
// GUARDED AND RE-READ like every other write about a surface: the row is
// deleted only if it still says what the tick's snapshot said. Between the two,
// somebody may have pressed Disconnect — which writes an intent under this same
// guard — and deleting over the top of that is the abandoned teardown above.
func (w *Worker) forget(ctx context.Context, kind Kind) {
	_, release, held, err := w.hold(ctx, kind)
	if err != nil || !held {
		// Nothing is written on "somebody else is here" or on "the store
		// could not say". The row has waited this long; it waits a tick.
		return
	}
	defer release()

	state, found, err := w.store.LoadIntegration(ctx, kind)
	switch {
	case err != nil, !found, state.TearingDown():
		return
	}
	if err := w.store.ForgetIntegration(ctx, kind); err != nil {
		log.WarnContext(ctx, "integration_status_not_forgotten",
			"integration", kind.String(), "error", err)
		return
	}
	log.InfoContext(ctx, "integration_status_forgotten",
		"integration", kind.String(),
		"detail", "this surface has left the company document")
}
