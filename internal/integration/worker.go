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
// read of a coordination bucket holding at most one key per surface in
// [Kinds], on a connection the process already holds. Nothing is fetched from
// a third-party app unless one is due.
const Interval = 15 * time.Second

// WakeSettle is how long a config apply waits before the tick it brings
// forward runs.
//
// A COALESCING WINDOW, not a delay for its own sake. One operator action is
// routinely several applies: the setup dialog writes one request per surface,
// so saving Atlassian writes the organization, Jira and Confluence blocks in
// three, and each one marks the loop stale. Ticking per apply would ask three
// third-party apps three times for one press of Save. This window is well
// under what a person reads as "immediately" and folds the burst into one
// pass.
//
// It is also the floor under a pass that writes the document on every run.
// Such a pass marks the loop stale from inside the tick that is running it,
// so with no window the loop would spin against a third-party app at whatever
// speed the pass returns. That is a bug rather than a design (a pass is
// expected to converge, and [Worker.MarkStale] says why the second one writes
// nothing), but the failure has to stay survivable long enough for somebody
// to notice it.
const WakeSettle = 750 * time.Millisecond

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
//
//   - MUST NOT rotate a credential that still works. A third-party app serves a token
//     once, so the tempting reading of "reconcile" is to mint every pass, and
//     that is an outage on a timer: the engine is authenticating with the old
//     value, and rotating revokes what every running agent is using. Check
//     what the sink recorded (provision.TokenSink.Value) and keep a working
//     credential. Rotation is an operator gesture on the integration subcommand,
//     where somebody typed the flag.
//
//   - MUST NOT delete anything a seat's departure implies. Decommissioning is
//     the other flag on that subcommand, for the same reason.
//
//   - MUST NOT WRITE when nothing has changed, and this stopped being a
//     SHOULD the day every reconciler was pointed at integrationtest: its
//     "a converged pass writes nothing" case is a hard failure, so this clause
//     is now checked rather than believed. It was believed for a while and
//     three vendors did not keep it — GitLab re-POSTed every membership and
//     re-PUT every hook, Mattermost re-joined every team and channel, and
//     Atlassian re-invited every seat, on every pass, for ever.
//
//     Reading is a different budget. A pass has to read enough to KNOW
//     nothing has changed, which is what replaced those writes, so the cost of
//     a converged pass is a read per seat and per project rather than nothing
//     at all. That is what the settled interval is sized against — see
//     [DefaultSchedule].
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

// Guard is the ONE claim every writer at one surface passes through, and the
// bound on the work it admits.
//
// # Why the worker takes it rather than the reconciler
//
// Three callers write at a third-party app and none can see the others: a pass
// an operator ran from the dashboard, a tick of this loop, and a disconnect's
// teardown. Two at once is one creating the account another is deleting.
//
// The loop used to take this INSIDE its reconciler and give it back the moment
// the pass returned — which left the write that RECORDS the pass outside it.
// That is a lost update with a name: an operator's disconnect, written under
// the guard on the node serving the API, silently overwritten by a tick
// folding a row it had read before the pass began. The card flipped back to
// connected and they pressed the button again. So the guard is taken out here,
// where it can span the re-read, the pass and the status write alike.
//
// # Three-valued, because the third value is the whole point
//
// held false is knowledge: somebody else is writing at this surface. An error
// is a failure to look, and a coordination store that could not answer has NOT
// said the surface is idle — acting on that guess is what creates the
// duplicate the guard exists to prevent. Collapsing them into a bool would put
// this back where CLAUDE.md's three-valued rule says never to be.
//
// # It BOUNDS the work
//
// The returned context carries the caller's own pass deadline, so a pass
// cannot outlive the lease protecting it. It must DERIVE from ctx rather than
// replace it: a worker stopping mid-pass, and the shared certification suite
// handing in a cancelled context, both have to reach the reconciler.
//
// release is non-nil exactly when held is true, and calling it is what lets the
// next writer in. Nil Guard is an unguarded worker — the shape a test builds
// and the shape a node with no keyring runs, where there is no lease to take
// and nothing to serialize against.
type Guard func(ctx context.Context, kind Kind) (
	bounded context.Context, release func(), held bool, err error)

// AdmitsFunc reports whether this node's configuration is current enough to
// converge a third-party app.
//
// # Why the loop asks at all
//
// Every reconciler reads the LIVE company document on each pass, and on a node
// whose posture is shed or stuck that document is the epoch the fleet has
// already replaced. Converging a third-party app to it undoes what the current
// revision asked for: an account a removed seat should no longer have is kept,
// a webhook is re-registered at the previous address, and the status row says
// ready. The node least able to answer for the company is the one with the
// fewest other duties competing for the lease, so it is if anything MORE likely
// to be holding this one.
//
// # Asked here rather than folded into the duty claim
//
// [schedule.Scheduler] already spells this rule for its own tick, for the same
// reason and in the same shape, and folding it into the shared worker-duty
// helper instead would gate every fleet singleton at once. That is not a
// bigger version of this fix, it is a different and worse one: a node's roles
// and its posture are decided by different subsystems that do not consult each
// other, so an ingress-only peer counts as healthy in the shed decision while
// refusing every duty on roles — and an ingress+workers fleet whose worker node
// fails one apply ends with NO node running any singleton, /ready green and
// nothing logged. Gated here, a shedding node declines this one duty, says so,
// and a worker-capable peer takes it.
//
// Nil admits, which is the single-node case and the case before a control
// plane exists.
type AdmitsFunc func() bool

// Reject marks err as a credential refusal when the third-party app answered with an
// authentication or authorization status, and returns it untouched otherwise.
//
// The POLICY lives here and the extraction stays with each third-party app, because
// they are different problems: which statuses mean "your credential is no
// good" is one rule for every third-party app, while digging the number out of a
// refusal is a question about that third-party app's own error type. Written per
// integration, the rule drifts (401 alone in one place and 401-or-403 in the
// next), and the surfaces that forgot 403 are exactly the ones that sit in
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
	// # Why only those, when the question sounds general
	//
	// A surface with a reconciler already answers it, better: the pass reads
	// the live document and returns [ErrNotConfigured], which [Observe] turns
	// into a forget. That answer knows things this one cannot — every pass
	// treats `enabled: false` as not configured, where a document-shaped test
	// sees a block and says yes — so consulting both would give one surface
	// two authorities that disagree, and a paused integration would flap
	// between them.
	//
	// A teardown-only kind has no pass to ask, and today that is Slack alone.
	// Its row is written by the setup form (an endpoint, so a moved Request
	// URL can be reported) and nothing has ever been able to remove it: the
	// row outlived the `slack:` block for the life of the deployment, and a
	// later reconnect inherited a stale address. This is the answer for that
	// one class and no other.
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
	// Datadog is the case, and today the only one: a webhook definition is
	// addressed by name, that name is a live config field, and Datadog
	// serves no listing — a GET on the collection answers 405 — so a
	// definition the engine stops managing can never be found again by
	// anything. Renaming the field creates a second definition and orphans
	// the first, silently and for ever.
	//
	// Compared with what was recorded, so the orphan is REPORTED rather
	// than deleted: the name is also the handle a monitor writes, so every
	// monitor still saying it goes on delivering correctly, and removing
	// the definition would silence exactly those. Empty for a kind with no
	// such name, and nil leaves every row's field empty.
	Registration func(Kind) string

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

	// stale is set when the document every recorded conclusion was drawn
	// from has been replaced, so the next tick reconsiders every surface
	// whatever its cadence says. See [Worker.MarkStale].
	stale atomic.Bool

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
// is asked again in minutes, which is right when the only thing that could
// have changed is at the third-party app: nobody wants a timer hammering
// GitHub. It is wrong the moment the answer changes HERE. An operator who
// installs an agent's app is redirected straight back to a card still holding
// the previous pass's finding, "this agent has no app of its own", printed
// above the same card's roster reporting that agent installed and ready: one
// screen, two answers, for as long as the settled cadence had left to run.
//
// In memory rather than written through the store, because it is not a
// conclusion to be shared: every node sees the apply, and the one holding the
// duty is the one that acts on it. A node that is not the singleton sets a
// flag and does nothing with it.
//
// A pass this triggers may itself write to the document (adopting an
// installation is exactly that) and so mark the loop stale again. It
// converges: the second pass finds nothing new to record and writes nothing.
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
		// A CHEAP FILTER ON A ROW THAT MAY BE SECONDS OLD, which is all this
		// has to be: it decides whether taking the guard is worth a round
		// trip. The row the WORK is decided on is re-read inside — see
		// [Worker.visit].
		if !stale && !state.Due(now) {
			continue
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
		w.visit(ctx, kind, stale, now)
	}

	w.forgetDeparted(ctx, states)
}

// visit takes the surface's guard and does whatever the row then says.
//
// # Everything that decides the work happens INSIDE the guard
//
// The row is re-read here, and the routing decision — converge, tear down, or
// nothing — is made from THAT row rather than from the snapshot [Worker.Tick]
// filtered on. Both halves are load-bearing and both were bugs:
//
//   - Folding a pass's outcome into a row read before the pass began loses
//     whatever landed in between. An operator's disconnect is written under
//     this same guard on whichever node served the request, and the tick put
//     its own copy back over the top.
//   - Routing on the stale row is the same race one step earlier: a
//     disconnect that lands between the load and the guard would be answered
//     by a CONVERGE pass, which finds the block still in the document (it has
//     to be — it carries the credential the teardown authenticates with),
//     converges the surface, and reports it healthy while somebody waits for
//     it to go.
//
// The due check is repeated for the same reason, and pays for itself: a
// dashboard pass that ran while this tick was working through the surfaces
// ahead of this one has already moved NextAttemptAt, and re-reading is what
// lets the loop notice rather than spend a third-party app's rate limit
// re-asking what somebody just asked.
func (w *Worker) visit(ctx context.Context, kind Kind, stale bool, now time.Time) {
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
		// SOMEBODY IS ALREADY DOING THIS — an operator's pass, or a
		// teardown. Nothing is recorded, for the reason above and one
		// more: a fault written here would describe as broken a surface
		// that is at this moment being provisioned successfully.
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
	if !stale && !state.Due(now) {
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
// EVERY SURFACE IS STAMPED FROM ONE READING OF THE CLOCK, and the settled
// interval is one number, so a company whose integrations all converge on the
// same tick becomes due on the same tick — for ever. Nothing ever pulls them
// apart again: each pass re-stamps them from the same instant with the same
// interval. What that produces is one burst of every vendor's API at once,
// six times an hour, instead of a steady trickle.
//
// A TENTH, and only downward from a full interval, because the interval is
// also a promise: [Schedule.Settled] is "the horizon on which an
// administrator who revokes an agent's access by hand is noticed", and
// stretching it would make that horizon longer than it says. Ten per cent is
// enough to decorrelate eight surfaces within one tick of each other and
// small enough that no cadence's meaning changes.
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
	err := reg.Disconnector.Disconnect(bounded, state.RemoveSeats)
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
	state.NextAttemptAt = now.Add(w.spread(w.schedule.Next(state.Report, state.Attempts)))
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := w.store.SaveIntegration(ctx, state); err != nil {
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

	state.NextAttemptAt = now.Add(w.spread(w.schedule.Next(state.Report, state.Attempts)))

	if err := w.store.SaveIntegration(ctx, state); err != nil {
		// The pass still happened, and its work at the third-party app is durable.
		// What is lost is the RECORD of it, so the next tick re-runs a
		// pass that has nothing left to do, which is the cheap failure.
		log.WarnContext(ctx, "integration_status_unrecorded",
			"integration", kind.String(), "error", err)
	}
}

// StampEndpoint records the address a pass ran against, where that pass is
// what keeps the address current.
//
// THREE-WAY on [Kind.Ingress], and the three answers are genuinely different:
//
//   - IngressEngine — a pass registers the delivery here, so the address it
//     ran against is the address the registration points at.
//   - IngressNone — nothing delivers to an address at all, so carrying one is
//     a claim waiting to become a false alarm: the moment the public base
//     moves, a stale value compares unequal and reports an action nobody can
//     take on a surface with no address to change. CLEARED rather than merely
//     not written, so a row an earlier build stamped converges on the next
//     pass.
//   - IngressOperator — a person typed the address at the third-party app.
//     Stamping it would report the base this deployment listens on as though
//     the third-party app had been told, which turns the one warning an
//     operator gets about a moved address into a green row.
//
// EXPORTED, because two writers share these rows: the loop's tick and the
// pass an operator runs from the dashboard. The rule lived inside the loop
// and the dashboard's pass stamped every surface unconditionally — so the
// same row meant one thing when a tick wrote it and another when a button
// did, and Datadog, whose webhook URL is a field on a settings page, was
// reported current by the button and left alone by the tick.
func StampEndpoint(state *State, kind Kind, current string) {
	switch kind.Ingress() {
	case IngressEngine:
		state.Endpoint = current
	case IngressNone:
		state.Endpoint = ""
	case IngressOperator:
	}
}

// StampRegistration records the name this surface's registration is now held
// under, and reports the one the previous name left behind.
//
// A NAME IS NOT AN ADDRESS, which is why this is separate from
// [StampEndpoint]. Where a registration is found by its address, moving the
// address re-points it and nothing is orphaned. Where it is found by NAME —
// Datadog's webhook definition, and only that today — changing the name
// creates a second registration and abandons the first, which goes on
// working: same address, same token, every monitor still naming it delivering
// correctly.
//
// SO IT IS REPORTED, NEVER DELETED. Removing it would silence exactly those
// monitors, and Datadog serves no listing, so nothing can find it again
// afterwards either. The finding is the only place an operator can learn it
// exists, and it is an advisory because nothing is broken — what is owed is
// repointing the monitors and then removing the definition by hand.
//
// EXPORTED for the reason [StampEndpoint] is: two writers share these rows,
// and a rule written twice is a row that means one thing when a tick wrote it
// and another when a button did.
func StampRegistration(state *State, current string) []Finding {
	previous := state.Registration
	state.Registration = current
	if previous == "" || current == "" || previous == current {
		return nil
	}
	return []Finding{{
		Kind:    FindingRegistrationOrphaned,
		Subject: previous,
		Detail: fmt.Sprintf(
			"this engine registered %q and now registers %q, so %q is still "+
				"there and nothing manages it — it keeps delivering, which is "+
				"why it was not removed. Repoint anything naming %q at %q, "+
				"then delete %q at the third-party app",
			previous, current, previous, previous, current, previous),
	}}
}

// forgetDeparted drops the row of a TEARDOWN-ONLY surface the company
// document no longer declares.
//
// # Why this is so much narrower than its name used to be
//
// It used to claim to forget any departed surface and forgot nothing at all:
// the liveness test was the REGISTRATION set, and the engine registers every
// kind in [Kinds] — a pass for the seven it converges, a teardown-only entry
// for the one it does not — so every valid kind was live and every invalid one
// was skipped. Unreachable code that a reader would have trusted.
//
// Reviving it as a general answer would have been worse than leaving it dead,
// because a general answer already exists and is better: a surface with a
// reconciler is forgotten when its own pass reports [ErrNotConfigured], read
// off the live document by the code that knows what configured MEANS for that
// third-party app — `enabled: false` included, which a block-shaped test reads
// as configured. Two authorities on one question is a paused integration
// flapping between them.
//
// So this answers for the one class that has no pass to ask, which is Slack
// alone today. Its row is written by the setup form — an endpoint, so a moved
// Request URL can be reported — and until now nothing could ever remove it: it
// outlived the `slack:` block for the life of the deployment, and a later
// reconnect through the config file inherited an address from the company
// before it.
//
// Three things are deliberately never forgotten here:
//
//   - A surface with a reconciler, per the above.
//   - A surface being torn down. A teardown that failed leaves the row in
//     [PhaseDisconnecting] on purpose and the block may already be gone, so
//     forgetting it here would abandon an unfinished disconnect — the third-party
//     app's webhooks still live, the card gone from the screen, silently.
//   - A kind this build does not know. The loop walks its own canonical order,
//     so a row a newer peer wrote is never even considered: an older node in a
//     rolling upgrade must not erase a status it cannot read, which it would
//     then watch the newer node write back on every pass.
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
