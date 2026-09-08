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
// # What a pass is, and what makes this different from the loop
//
// The reconcile loop runs a third-party app's own Reconcile every few minutes
// with NO sink and NO webhook base, and those two absences are what make it
// safe to run unattended: a base is permission to register a webhook, and a
// sink is permission to mint a credential. Neither is something a timer may
// decide.
//
// A pass is the same function with both supplied, and it runs because a
// person pressed a button. That is the whole distinction, and it is why this
// exists rather than the loop simply being given more to do.
//
// # It is a fleet singleton for the duration
//
// Two operators pressing Connect at once, or one pressing it twice, would run
// two passes that both mint. A pass therefore takes the same named duty lease
// the loop uses, under its own name, and a second caller is refused rather
// than queued: minting twice is not something a retry should paper over.
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

// Teardowner is a [Pass] that can also remove what it registered.
//
// OPTIONAL, and the third-party apps that do not satisfy it are not
// oversights: Slack, Mattermost and Datadog register no webhook from this
// engine, so a teardown for them would have nothing to withdraw. A type
// assertion is what asks, which keeps a third-party app's answer in one place
// (its own package) rather than in a list here that has to be kept in step
// with it.
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
	// Sink records a minted credential. Always present here, which is the
	// difference from a loop tick.
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

// ErrNoTeardown reports a third-party app that registers nothing to remove.
// Slack and Mattermost register no webhook from this engine, so a disconnect
// has only the company document to change.
var ErrNoTeardown = errors.New("setup: nothing to remove at this integration")

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

// Duty is the fleet lease a pass holds while it runs.
//
// The consumer's own interface: one call, answering whether this node may act
// now. Three-valued through the error, like every other ownership question in
// this engine, because "somebody else holds it" and "the store could not be
// reached" lead to opposite actions.
type Duty func(ctx context.Context) (bool, error)

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
	if err := r.claim(kind, id); err != nil {
		return nil, err
	}
	defer r.release(kind)

	if r.duty != nil {
		if duty := r.duty(kind); duty != nil {
			held, err := duty(ctx)
			if err != nil {
				// UNKNOWN IS NOT REFUSED-AND-NOT-HELD. A coordination
				// store that could not answer is not evidence that
				// somebody else is minting, and treating it as such
				// would make a two-second blip look like a conflict.
				return nil, fmt.Errorf("setup: could not claim the provisioning lease: %w", err)
			}
			if !held {
				return nil, ErrPassInFlight
			}
		}
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

// StartTeardown removes what a third-party app holds, under the same guard a
// pass runs beneath.
//
// THE SAME LEASE, deliberately. A teardown and a provisioning pass are the
// two operations that write at the third-party app, and letting them overlap
// is how a disconnect deletes a webhook the pass beside it is registering.
// Sharing the claim means one of them waits, whichever arrives second.
//
// Returns [ErrNoTeardown] for a third-party app that registers nothing to
// remove, which the caller reads as "there was nothing to do" rather than as
// a failure: the disconnect still finishes.
func (r *Runner) StartTeardown(
	ctx context.Context, kind integration.Kind, in TeardownInput, id string,
) (*Run, error) {
	pass, ok := r.passes[kind]
	if !ok {
		return nil, ErrNoPass
	}
	tearer, ok := pass.(Teardowner)
	if !ok {
		return nil, ErrNoTeardown
	}
	if err := r.claim(kind, id); err != nil {
		return nil, err
	}
	defer r.release(kind)

	if r.duty != nil {
		if duty := r.duty(kind); duty != nil {
			held, err := duty(ctx)
			if err != nil {
				// Three-valued, as everywhere: a store that could not
				// answer is not evidence somebody else is minting.
				return nil, fmt.Errorf("setup: could not claim the teardown lease: %w", err)
			}
			if !held {
				return nil, ErrPassInFlight
			}
		}
	}

	run := &Run{ID: id, Kind: kind, State: RunRunning, StartedAt: r.now()}
	r.remember(run)

	err := tearer.Teardown(ctx, in)
	ended := r.now()
	run.EndedAt = &ended
	if err != nil {
		run.State = RunFailed
		run.Error = err.Error()
		return run, err
	}
	run.State = RunDone
	return run, nil
}

// Tears reports whether this build can remove what a third-party app holds.
func (r *Runner) Tears(kind integration.Kind) bool {
	if r == nil {
		return false
	}
	pass, ok := r.passes[kind]
	if !ok {
		return false
	}
	_, ok = pass.(Teardowner)
	return ok
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
