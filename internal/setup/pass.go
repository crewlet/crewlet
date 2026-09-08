package setup

import (
	"context"
	"errors"
	"fmt"
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
	Teardown(ctx context.Context, in TeardownInput) error
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

	// Seats narrows the run to these handles, empty meaning every seat.
	Seats []string

	// DryRun plans and validates without writing at the third-party app.
	DryRun bool

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

// Start runs a pass, blocking until it finishes.
//
// BLOCKING, deliberately, and the caller decides what to do with that. A
// GitHub pass is a handful of API calls; the third-party apps whose passes
// take minutes are not on this surface yet, and giving every one of them an
// asynchronous shape today would be building the machinery for a case that
// does not exist while making the one that does harder to reason about.
func (r *Runner) Start(ctx context.Context, kind integration.Kind, in PassInput, id string) (*Run, error) {
	pass, ok := r.passes[kind]
	if !ok {
		return nil, ErrNoPass
	}
	release, held, err := r.hold(ctx, kind, id)
	if err != nil {
		// UNKNOWN IS NOT REFUSED-AND-NOT-HELD. A coordination store that
		// could not answer is not evidence that somebody else is minting,
		// and treating it as such would make a two-second blip look like
		// a conflict.
		return nil, fmt.Errorf("setup: could not claim the provisioning lease: %w", err)
	}
	if !held {
		return nil, ErrPassInFlight
	}
	defer release()

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
// held is false when somebody already has it; release is non-nil exactly when
// held is true, and calling it is what lets the next writer in. The error is
// the third value: a store that could not answer has not said the surface is
// idle.
func (r *Runner) Hold(ctx context.Context, kind integration.Kind) (func(), bool, error) {
	if r == nil {
		return func() {}, true, nil
	}
	return r.hold(ctx, kind, holdID)
}

// holdID is what the in-process claim records for a hold taken outside a run.
//
// The map's VALUE is only ever read back by a person reading a dump; what
// makes the guard work is the key's presence. A run records its own id there
// because that one is worth seeing.
const holdID = "hold"

// hold takes the in-process claim and then the fleet lease, and hands back the
// release for both.
//
// IN THAT ORDER, and it matters on the failing path: the local claim is what
// the release below gives back, so taking the remote one first would leave a
// lease held for its full TTL whenever a second goroutine on this node lost
// the local race.
func (r *Runner) hold(
	ctx context.Context, kind integration.Kind, id string,
) (func(), bool, error) {
	if err := r.claim(kind, id); err != nil {
		// A pass is already running HERE. Definitively not held rather
		// than an error: this is knowledge, not a failure to look.
		return nil, false, nil
	}
	if r.duty == nil {
		return func() { r.release(kind) }, true, nil
	}
	duty := r.duty(kind)
	if duty == nil {
		// No coordination store: a single node with nobody to be a
		// singleton among, still guarded against itself by the claim
		// above.
		return func() { r.release(kind) }, true, nil
	}
	release, held, err := duty(ctx)
	if err != nil || !held {
		r.release(kind)
		return nil, false, err
	}
	return func() { release(); r.release(kind) }, true, nil
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

func (r *Runner) claim(kind integration.Kind, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, running := r.live[kind]; running {
		return ErrPassInFlight
	}
	r.live[kind] = id
	return nil
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
