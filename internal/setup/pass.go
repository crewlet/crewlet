package setup

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
)

// Running a third-party app's provisioning pass from inside the engine.
//
// # What a pass is, and who else runs one
//
// A pass is a third-party app's own Reconcile with a sink and a webhook base
// supplied: a base is permission to register a webhook and a sink is
// permission to mint a credential, so those two arguments are the whole of
// what separates provisioning from reading.
//
// It used to be what separated this package from the reconcile loop, which
// ran every tick with neither. It no longer does: CONNECTING IS THE
// PERMISSION, so the loop supplies both as well (see
// [github.com/crewlet/crewlet/internal/engine] `startIntegrations`), and what
// still cannot happen unattended is anything a person has not asked for — a
// company with no block is not reconciled, a block with no credential reports
// a finding rather than acting, and nothing is deleted except through a
// disconnect somebody pressed.
//
// So this package is no longer "the writing one". It is the surface an
// operator's button runs a pass through, and — through [Runner.Hold] — the
// one guard every other writer at a surface takes as well.
//
// # One writer per surface, on this node and across the fleet
//
// Two operators pressing Connect at once, a loop tick landing on the pass an
// operator just started, or a disconnect deleting the webhook a pass beside it
// is registering: three shapes of one collision, at a third-party app where a
// write creates an account or mints a credential.
//
// [Runner.Hold] is the only thing standing between them, and it is two guards
// rather than one because either alone is a hole. The named duty lease stops
// two NODES; it cannot stop two goroutines here, because a claim by an owner
// that already holds the lease doubles as a renew, so both would be told yes.
// An in-process claim stops those two goroutines and knows nothing of a peer.
// A writer takes both or writes nothing.
//
// # Its result speaks the reconcile vocabulary
//
// A pass reports findings, exactly as a loop tick does, and the caller writes
// them to the same status the loop writes. So a pass triggered from the
// dashboard and a tick that ran a minute later cannot disagree about what an
// integration's state is: there is one classifier and one status row.

// Pass runs one integration's provisioning, with permission to write.
//
// Kind is here rather than inferred so the registry is keyed by the
// integration's own answer, the same shape [integration.Reconciler] uses.
type Pass interface {
	Kind() integration.Kind

	// Run executes the third-party app's existing Reconcile with a sink and a
	// base. It returns findings in the shared vocabulary, and an error only for
	// a fault: something the engine or the third-party app could not do at all,
	// which is a different fact from anything the findings can express.
	Run(ctx context.Context, in PassInput) ([]integration.Finding, error)

	// Needs reports the transient third-party app credential this pass requires,
	// or nil. A group Owner token or a Mattermost admin PAT is asked for on
	// every run and never stored: it can create accounts, and a permanently held
	// one turns a one-time grant into a standing power.
	Needs() *Requirement
}

// Teardowner is a [Pass] that can also remove what it created.
//
// OPTIONAL, and today every [Pass] satisfies it. That is not an argument for
// folding Teardown into Pass: what decides membership is whether this engine
// PUT something at the surface, and the one kind that has nothing — Slack,
// whose apps are created from the command line — has no pass here at all, so
// it never reaches this assertion. A kind that gains a read-only pass gains
// one without a teardown.
//
// A WEBHOOK IS NOT WHAT DECIDES IT. Mattermost withdraws none — it revokes
// the tokens it issued and disables the bots it created — and Datadog
// withdraws the webhook its own pass registers as well as disabling the
// accounts. Both are destructive, which is the property that matters here.
//
// A type assertion is what asks, which keeps a third-party app's answer in one
// place (its own package) rather than in a list here that has to be kept in
// step with it.
type Teardowner interface {
	Pass

	// Teardown removes what this third-party app's passes created. It is the
	// only operation in this package that DESTROYS at a third-party app, so it
	// takes the operator's own answer about how far to go rather than inferring
	// it.
	//
	// An error holds the surface in [integration.PhaseDisconnecting] and
	// the loop tries again, so a partial teardown must be safe to repeat:
	// every step is "remove this if it is there".
	//
	// # It reports what it removed, and that is an END STATE
	//
	// A teardown knows which seats' accounts went — it walks its own plan,
	// and each [provision.Seat] carries the `${VAR}` names its credentials
	// live in. That knowledge used to die at an error-only return, and
	// nothing downstream could delete the values those accounts left
	// sealed. See [provision.Removed].
	//
	// The result names the state this teardown ESTABLISHED, not the delta
	// this call performed. Every step is already "remove this if it is
	// there" and the whole thing is retried on failure — so if the result
	// were a delta, a retry after a failed secret deletion would find the
	// accounts already gone, report nothing removed, and let the block drop
	// with the credentials still in the store. "Already absent" counts as
	// removed.
	//
	// AND ONLY WHAT IS GENUINELY DEAD. A merely DISABLED account does not
	// belong here: a token on one works again the moment anybody re-enables
	// it, so naming it would delete a company's only record of a live
	// credential.
	Teardown(ctx context.Context, in TeardownInput) (provision.Removed, error)
}

// TeardownInput is what a teardown pass is told.
type TeardownInput struct {
	// RemoveSeats is the operator's answer to "also remove the accounts
	// Crewlet created".
	//
	// FALSE BY DEFAULT and never inferred. The webhooks come out either
	// way — this engine registered them, nothing else uses them, and one
	// left behind delivers to a company that no longer has a block to
	// route it. An ACCOUNT is different: it may be a colleague in that
	// third-party app with history attached, and deleting one because somebody
	// pressed Disconnect is not a decision a button gets to make.
	RemoveSeats bool

	// Operator is the transient third-party app credential, for an
	// integration whose [Pass.Needs] asks for one. Removing an account
	// usually needs the same authority creating it did.
	Operator string
}

// PassInput is what a pass is given.
type PassInput struct {
	// Sink records a minted credential.
	//
	// NIL IS A DRY RUN, and it is the honest posture for a caller that
	// could not have recorded what it created: a node with no keyring can
	// still read a surface and report what it finds, which is most of what
	// a pass is for.
	Sink provision.TokenSink

	// WebhookBase is the address third-party apps reach this deployment on, and
	// supplying it IS the permission to register a hook. Empty runs the
	// pass read-only.
	WebhookBase string

	// There is deliberately no DryRun and no Seats here.
	//
	// Both existed, both were forwarded straight from the HTTP request, and
	// NO pass read either — so `{"dry_run": true}` ran a full pass that
	// created accounts and minted live tokens, and a seat-scoped run touched
	// every seat. A field that means "do not write" while writing is worse
	// than no field: it is an operator acting on a promise the code never
	// made.
	//
	// Removed rather than stubbed, because no tag has ever shipped this
	// surface, so there is nobody on the other side of a compatibility path.
	// Honouring them is a real feature — every one of the seven passes has to
	// implement plan-without-write, and a partial answer is the same lie in a
	// smaller font — so it goes back when somebody builds it, with the passes
	// that read it.

	// Recreate re-registers hooks with a fresh secret. DESTRUCTIVE across
	// deployments: the previous secret stops working everywhere else this
	// company runs, so a caller must confirm it deliberately.
	Recreate bool

	// Operator is the transient third-party app credential, when the pass needs
	// one. Never persisted, and never logged.
	Operator string
}

// ErrPassInFlight reports that this third-party app's pass is already
// running.
var ErrPassInFlight = errors.New("setup: a pass for this integration is already running")

// ErrNoPass reports a third-party app this build cannot provision from the
// API.
var ErrNoPass = errors.New("setup: no provisioning pass for this integration")

// RunState is where one pass got to.
type RunState string

// The three states a run reaches. Terminal is done or failed; a run that is
// neither is still holding its lease.
const (
	RunRunning RunState = "running"
	RunDone    RunState = "done"
	RunFailed  RunState = "failed"
)

// Run is one pass, live or finished.
type Run struct {
	ID        string                `json:"run_id"`
	Kind      integration.Kind      `json:"key"`
	State     RunState              `json:"state"`
	StartedAt time.Time             `json:"started_at"`
	EndedAt   *time.Time            `json:"ended_at,omitempty"`
	Findings  []integration.Finding `json:"findings,omitempty"`

	// Error is the fault, if the pass could not run. NEVER the third-party app's
	// raw message when that message can quote a config value: the caller
	// maps it before it lands here.
	Error string `json:"error,omitempty"`

	// Report is what the findings classify to, so a caller renders the
	// same phase and actor the reconcile status carries.
	Report *integration.Report `json:"report,omitempty"`
}

// LeaseTTL is how long one writer may hold a surface before the fleet
// assumes it died.
//
// A BACKSTOP, NOT A DEADLINE FOR THE WORK. The lease is given back when the
// work ends ([Runner.Hold]'s release), so this value is only reached by a node
// that stopped existing mid-pass. Shorter would lock a surface out for less
// time after a crash and risk nothing, because nothing runs this long on
// purpose; much longer would leave a third-party app unwritable for the whole
// window after one.
const LeaseTTL = 5 * time.Minute

// PassDeadline bounds the work that lease admits — every pass, whatever
// started it.
//
// # Why a pass has a deadline at all
//
// The lease is taken once and NEVER RENEWED mid-pass, so a pass that outlived
// it would go on writing at a third-party app with nothing left excluding a
// peer: two nodes creating an account for one seat, which no later pass can
// detect or repair. Bounding the work strictly inside the lease is what makes
// "the lease protects this pass" true by construction rather than by
// measurement.
//
// # Why it is one minute, and why the minute is not slack for its own sake
//
// The status write that RECORDS the pass runs under the same lease — see
// [Runner.Hold] — so the margin is what that write has to complete in. A
// coordination round trip is milliseconds; a minute is that with room for a
// broker having a bad afternoon.
//
// The margin is real rather than nominal because the clock starts AFTER the
// lease is acquired ([Runner.hold]). It used to start at the caller, before
// the claim, so however long the claim took came out of the minute.
//
// # It is one value, and the loop's duty is derived from it
//
// A second, shorter deadline for the reconcile loop's own passes was the
// obvious alternative and is worse: two numbers that have to stay in a
// relationship nothing checks. Instead the loop's singleton TTL is derived
// from this one — see `integrationDutyTTL` in internal/engine — so a pass
// cannot outlive the duty that scheduled it either.
const PassDeadline = 4 * time.Minute

// RecordDeadline bounds the write that RECORDS a pass, and is the margin
// [PassDeadline] deliberately leaves behind inside [LeaseTTL].
//
// DERIVED, never chosen. The whole reason the pass stops a minute early is that
// the status write runs under the same lease and needs somewhere to run; a
// second hand-written number here could drift out of that relationship with
// nothing checking, so the relationship IS the definition.
//
// What it protects against is narrow and real: the caller that most needs it
// has detached from an HTTP request on purpose, so a browser tab closing cannot
// cancel a third-party app write half-way. That detachment removes the only
// cancellation the write had, and an unbounded write into a coordination store
// having a bad afternoon leaks the goroutine that was serving the request.
const RecordDeadline = LeaseTTL - PassDeadline

// Duty is the fleet lease a pass holds WHILE IT RUNS, and gives back when it
// is done.
//
// The consumer's own interface: one call, answering whether this node may act
// now and handing back the release. Three-valued through the error, like every
// other ownership question in this engine, because "somebody else holds it"
// and "the store could not be reached" lead to opposite actions.
//
// THE RELEASE IS THE HALF THAT MAKES THE LEASE SHAREABLE. Held to its TTL
// after the work finished, this lock is indistinguishable from an outage to
// every other caller: the loop reconciles a surface every few seconds, so a
// lease it kept for five minutes would refuse an operator's pass on any node
// but its own, permanently. release is non-nil exactly when held is true.
type Duty func(ctx context.Context) (release func(), held bool, err error)

// Runner executes passes and remembers what they did.
type Runner struct {
	// Passes are the third-party apps this build can provision, keyed by kind.
	passes map[integration.Kind]Pass

	// Duty claims the fleet lease for one kind. Nil is a single node with
	// nothing to be a singleton among, which still gets the in-process
	// guard below.
	duty func(kind integration.Kind) Duty

	now func() time.Time

	mu sync.Mutex
	// live is the pass running per kind, which is the in-process half of
	// the guard: the lease stops two NODES, and this stops two goroutines
	// on one node racing for it.
	live map[integration.Kind]string
	runs map[string]*Run
}

// NewRunner builds a runner over the passes this build serves.
func NewRunner(passes []Pass, duty func(integration.Kind) Duty, now func() time.Time) *Runner {
	byKind := map[integration.Kind]Pass{}
	for _, p := range passes {
		byKind[p.Kind()] = p
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Runner{
		passes: byKind, duty: duty, now: now,
		live: map[integration.Kind]string{},
		runs: map[string]*Run{},
	}
}

// Serves reports whether this build can provision a third-party app from the
// API.
func (r *Runner) Serves(kind integration.Kind) bool {
	if r == nil {
		return false
	}
	_, ok := r.passes[kind]
	return ok
}

// Needs is the transient credential this third-party app's pass asks for, or
// nil.
func (r *Runner) Needs(kind integration.Kind) *Requirement {
	if r == nil {
		return nil
	}
	pass, ok := r.passes[kind]
	if !ok {
		return nil
	}
	return pass.Needs()
}

// Execute runs a pass, blocking until it finishes.
//
// # THE CALLER MUST ALREADY HOLD THE SURFACE
//
// ctx is the bounded context [Runner.Hold] returned, and the caller keeps the
// hold until it has written what the pass found. This method used to take the
// guard itself and give it back the moment the pass returned, which put the
// write that RECORDS the pass outside it — and a status row written outside
// the lease is exactly the lost update coord.Integrations forbids: a loop tick
// holding a row it read a moment earlier puts its own copy back over an
// operator's disconnect, the card flips from Disconnecting to connected, and
// they press the button again.
//
// So the guard spans the pass AND its record, and the caller owns it because
// only the caller knows when it has finished writing. Both callers take it the
// same way — the reconcile loop through the Guard its worker holds, the
// dashboard through [Runner.Hold] directly.
//
// BLOCKING, deliberately. What bounds it is [PassDeadline], carried by ctx,
// rather than anything here.
func (r *Runner) Execute(ctx context.Context, kind integration.Kind, in PassInput, id string) (*Run, error) {
	pass, ok := r.passes[kind]
	if !ok {
		return nil, ErrNoPass
	}
	// A CANCELLED PASS IS A FAULT, NEVER A CLEAN REPORT, and this is the
	// dashboard's half of that rule — the reconcile loop's half is in
	// engine.passConverger.
	//
	// The caller's context reaches here detached from the HTTP request, so
	// what expires it is [PassDeadline] rather than a closing tab. A pass
	// started with that already spent would run against a lease about to
	// lapse, and every third-party app whose pass concludes from the company
	// document before its first network call would answer with no findings
	// and no error — which the fold records as ready, on a surface nobody
	// looked at.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	run := &Run{ID: id, Kind: kind, State: RunRunning, StartedAt: r.now()}
	r.remember(run)

	findings, err := pass.Run(ctx, in)
	ended := r.now()
	run.EndedAt = &ended
	run.Findings = findings
	if err != nil {
		run.State = RunFailed
		run.Error = err.Error()
		return run, err
	}
	run.State = RunDone
	report := integration.Classify(findings)
	run.Report = &report
	return run, nil
}

// Hold is THE GUARD every writer at one surface passes through.
//
// Three callers write at a third-party app and none of them can see the
// others: an operator's pass here, a tick of the reconcile loop, and a
// disconnect's teardown. Two at once is one creating the account the other is
// deleting, or a disconnect withdrawing the webhook a pass beside it just
// registered.
//
// BOTH HALVES, because each alone leaves the collision reachable:
//
//   - The named duty lease stops two NODES. It cannot stop two goroutines in
//     this one, because [coord.Backend.TryAcquire] doubles as a renew for the
//     owner that already holds the record — so a loop tick and a button press
//     on one node are both told yes. It is also absent entirely on a node with
//     no coordination store, which is the single-node install.
//   - The in-process claim stops those two goroutines and knows nothing about
//     a peer.
//
// It also covers the writes ABOUT a surface, not only the ones at it. The
// status rows are last-write-wins and [coord.Integrations] says why: "the duty
// makes one node the only writer, so there is no second writer to race." That
// premise is restored rather than replaced — the dashboard records a pass's
// outcome, stamps an endpoint and marks a disconnect on whichever node served
// the request, so it takes this same hold, and a caller that cannot take it
// does not write.
//
// # It also BOUNDS the work it admits
//
// The lease is taken once and never renewed, so the returned context carries
// [PassDeadline] and the work must run on it. That is what makes "the lease
// protects this pass" true by construction: a pass cannot outlive its own
// protection, because the context it was handed dies first. The bound derives
// from the CALLER's context rather than replacing it, so cancellation still
// reaches the pass — a worker stopping, or a suite handing in a dead context,
// must still stop the work.
//
// held is false when somebody already has it; the context and release are
// non-nil exactly when held is true, and calling release is what lets the next
// writer in. The error is the third value: a store that could not answer has
// not said the surface is idle.
func (r *Runner) Hold(
	ctx context.Context, kind integration.Kind,
) (context.Context, func(), bool, error) {
	if r == nil {
		// NO RUNNER, SO NO LEASE — and the deadline still applies. What it
		// bounds here is not a lease but the pass: a node with no keyring
		// reads and reports, and a read that never returns wedges the loop
		// just as thoroughly as one that outlived a lease would.
		bounded, cancel := context.WithTimeout(ctx, PassDeadline)
		return bounded, cancel, true, nil
	}
	return r.hold(ctx, kind, holdID)
}

// holdID is what the in-process claim records for a hold taken outside a run.
//
// The map's VALUE is only ever read back by a person reading a dump; what
// makes the guard work is the key's presence. A run records its own id there
// because that one is worth seeing.
const holdID = "hold"

// hold takes the in-process claim, then the fleet lease, then the deadline,
// and hands back one release for all three.
//
// IN THAT ORDER, and every step of it matters on a failing path:
//
//   - The local claim first, because the release below is what gives it back:
//     taking the remote one first would leave a lease held for its full
//     [LeaseTTL] whenever a second goroutine on this node lost the local race.
//   - The lease on the CALLER's context, never on the bounded one built after
//     it. The release closes over whatever context it was given, and
//     schedule.HoldNamedDuty already wraps that in context.WithoutCancel so a
//     cancelled pass still gives its lease back. Build it from a context that
//     has hit [PassDeadline] and every release after a timeout is a no-op —
//     locking the surface out for the rest of [LeaseTTL] at exactly the moment
//     it most needs releasing.
//   - The deadline last, so its margin inside [LeaseTTL] is the whole of
//     [PassDeadline]'s minute rather than a minute minus however long the
//     claim took.
func (r *Runner) hold(
	ctx context.Context, kind integration.Kind, id string,
) (context.Context, func(), bool, error) {
	if !r.claim(kind, id) {
		// A pass is already running HERE. Definitively not held rather
		// than an error: this is knowledge, not a failure to look.
		return nil, nil, false, nil
	}
	release := func() { r.release(kind) }
	if r.duty != nil {
		// A nil Duty for this kind is no coordination store: a single node
		// with nobody to be a singleton among, still guarded against itself
		// by the claim above.
		if duty := r.duty(kind); duty != nil {
			leaseRelease, held, err := duty(ctx)
			if err != nil || !held {
				r.release(kind)
				return nil, nil, false, err
			}
			release = func() { leaseRelease(); r.release(kind) }
		}
	}
	bounded, cancel := context.WithTimeout(ctx, PassDeadline)
	return bounded, func() { cancel(); release() }, true, nil
}

// Get is one run by id.
func (r *Runner) Get(id string) (*Run, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[id]
	return run, ok
}

// claim takes the in-process half of the guard, reporting whether it got it.
//
// A BOOL, NOT AN ERROR, because "somebody here is already writing at this
// surface" is KNOWLEDGE rather than a failure to look — and the difference is
// load-bearing one caller up, where an error means the coordination store
// could not answer and a false means it answered no. Returning ErrPassInFlight
// here made [Runner.hold] discard an error to say "not held", which is the
// shape every three-valued answer in this engine exists to avoid.
func (r *Runner) claim(kind integration.Kind, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, running := r.live[kind]; running {
		return false
	}
	r.live[kind] = id
	return true
}

func (r *Runner) release(kind integration.Kind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, kind)
}

// remember keeps a run readable after it ends.
//
// BOUNDED, because a process that never restarts would otherwise keep every
// run it ever executed. What a caller comes back for is the one it just
// started, so a short history is the whole requirement.
const maxRuns = 32

func (r *Runner) remember(run *Run) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.runs) >= maxRuns {
		// The oldest by start time, which is deterministic where map
		// order is not.
		var oldest *Run
		for _, candidate := range r.runs {
			if oldest == nil || candidate.StartedAt.Before(oldest.StartedAt) {
				oldest = candidate
			}
		}
		if oldest != nil {
			delete(r.runs, oldest.ID)
		}
	}
	r.runs[run.ID] = run
}
