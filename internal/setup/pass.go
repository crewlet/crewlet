package setup

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
)

// Running a third-party app's provisioning pass from inside the engine.
//
// A pass is a third-party app's own Reconcile with a sink (permission to mint
// a credential) and a webhook base (permission to register a webhook).
// CONNECTING IS THE PERMISSION, so the reconcile loop supplies both too
// ([github.com/crewlet/crewlet/internal/engine] `startIntegrations`);
// unattended, nothing happens a person has not asked for: no block, no
// reconcile; no credential, a finding; deletion only through a disconnect
// somebody pressed. [Runner.Hold] is the one writer guard per surface, and a
// pass reports findings as a tick does, into the same status row.

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
// OPTIONAL: membership is whether this engine PUT something at the surface, so
// a kind that gains a read-only pass gains one without a teardown (Slack, whose
// apps are made from the command line, has no pass at all). A webhook is not
// what decides it: Mattermost withdraws none yet revokes tokens and disables
// bots, which is destructive too. A type assertion asks, keeping each app's
// answer in its own package rather than in a list here.
type Teardowner interface {
	Pass

	// Teardown removes what this app's passes created: the only operation here
	// that DESTROYS at a third-party app, so it takes the operator's own answer
	// about how far to go. An error holds the surface in
	// [integration.PhaseDisconnecting] and the loop retries, so every step is
	// "remove this if it is there".
	//
	// It reports the END STATE ESTABLISHED ([provision.Removed]), not this
	// call's delta: after a failed secret deletion a delta-reporting retry
	// would find the accounts gone and let the block drop with the credentials
	// stored. "Already absent" counts as removed. ONLY WHAT IS GENUINELY DEAD:
	// a merely DISABLED account works again when re-enabled, so naming it
	// deletes a live credential's record.
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

	// By is who asked for the teardown, which a step that writes the
	// company's own document or store records the write under — Slack's,
	// which clears each seat's sealed app credentials. A step that only
	// removes things at the third-party app has nothing here to record.
	By iam.Actor
}

// PassInput is what a pass is given.
type PassInput struct {
	// Sink records a minted credential.
	//
	// NIL IS A DRY RUN: a check reads a surface and reports what it
	// finds, which is most of what a pass is for, and creates nothing it
	// could not record.
	Sink provision.TokenSink

	// WebhookBase is the address third-party apps reach this deployment on
	// ([config.API.WebhookBase]): the public listener's
	// `api.public.external_url` where `api.public` is set, and
	// `api.external_url` otherwise. Empty is a deployment with no such
	// address, which a pass REPORTS (`ingress_blocked`) rather than
	// registering a hook at nothing.
	//
	// It is not what makes a pass read-only — a nil Sink is — so a check
	// is handed it too: without it a pass cannot tell a webhook pointing
	// elsewhere, or a deployment with no address at all, from a healthy one.
	WebhookBase string

	// WebhookBaseKey is the Tier A key WebhookBase is read from
	// ([config.API.WebhookBaseKey]), which a pass with no address names as
	// the setting to fill in. See [BaseKey] for what empty names.
	//
	// BESIDE THE ADDRESS AND FROM THE SAME ACCESSOR, never spelled by a
	// vendor: the key depends on whether a public listener is set, and a
	// pass naming `api.external_url` on a deployment that reads
	// `api.public.external_url` sends an operator to change a setting
	// nothing reads.
	WebhookBaseKey string

	// There is deliberately no DryRun and no Seats here.
	//
	// No pass reads either, so a `dry_run` field would run a full pass that
	// creates accounts and mints live tokens: a field meaning "do not write"
	// while writing is worse than none. Honouring them means every pass
	// implementing plan-without-write; they return with the passes that read
	// them.

	// Recreate re-registers hooks with a fresh secret. DESTRUCTIVE across
	// deployments: the previous secret stops working everywhere else this
	// company runs, so a caller must confirm it deliberately.
	//
	// SPELLED BY THE CALLER that set it ([provision.FreshSwitch]) — the
	// setup route's own body field — and handed to the vendor's pass as
	// it is, so a refusal of it names the field that caller sent rather
	// than a command-line flag. Empty is not asked, which is what the
	// reconcile loop hands every pass.
	Recreate provision.FreshSwitch

	// Operator is the transient third-party app credential, when the pass needs
	// one. Never persisted, and never logged.
	Operator string

	// By is who the run is for: the person who pressed the button, or the
	// reconcile loop. It is what a credential the run seals is recorded as
	// sealed by, where the Agents tab says who sealed a seat's key and when.
	// The sink records the same party on every row it writes; this carries
	// it to the records the sink cannot see.
	By iam.Actor
}

// BaseKey is the Tier A key a webhook base was read from, as a pass names it
// when it had no address to register a delivery path at: key as its caller
// handed it ([PassInput.WebhookBaseKey], or a vendor's Options field), or the
// key a deployment with NO PUBLIC LISTENER reads when key is empty.
//
// EMPTY IS THAT DEPLOYMENT, not a guess: it is what the zero [config.API]
// answers, the only deployment a caller with no Tier A file can describe. A
// caller that holds a Tier A takes the key from [config.API.WebhookBaseKey],
// beside the address from the same block, so the two cannot name different
// listeners. ONE FUNCTION, as vendors' own literals named `api.external_url`
// where webhooks are served only on `api.public.external_url`.
func BaseKey(key string) string {
	if key != "" {
		return key
	}
	var unset config.API
	return unset.WebhookBaseKey()
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
// The lease is NEVER RENEWED mid-pass, so a pass outliving it would keep
// writing with nothing excluding a peer: two nodes creating an account for one
// seat, which no later pass can repair. The one-minute margin is what the
// status write that RECORDS the pass (same lease, [Runner.Hold]) completes in,
// room for a bad broker afternoon; real because the clock starts AFTER the
// lease is acquired ([Runner.hold]). ONE VALUE: the reconcile loop's singleton
// TTL derives from it (`integrationDutyTTL` in internal/engine), so a pass
// cannot outlive the duty that scheduled it.
const PassDeadline = 4 * time.Minute

// RecordDeadline bounds the write that RECORDS a pass, and is the margin
// [PassDeadline] deliberately leaves behind inside [LeaseTTL].
//
// DERIVED, never chosen: a second hand-written number could drift out of the
// relationship (the pass stops early so the status write has room under the
// same lease), so the relationship IS the definition.
//
// The caller that most needs it detached from an HTTP request on purpose, so a
// closing tab cannot cancel a third-party write half-way; that removes the
// write's only cancellation, and an unbounded write into a struggling
// coordination store would leak the serving goroutine.
const RecordDeadline = LeaseTTL - PassDeadline

// Duty is the fleet lease a pass holds WHILE IT RUNS, and gives back when it
// is done.
//
// Three-valued through the error, as every ownership question here: "somebody
// else holds it" and "the store could not be reached" lead to opposite actions.
//
// THE RELEASE MAKES THE LEASE SHAREABLE. Held to its TTL after the work
// finished, the lock looks like an outage to every other caller: the loop
// reconciles a surface every few seconds, so a lease it kept five minutes
// would refuse an operator's pass on any other node. release is non-nil
// exactly when held is true.
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
// THE CALLER MUST ALREADY HOLD THE SURFACE: ctx is the bounded context
// [Runner.Hold] returned, and the caller keeps the hold until it has written
// what the pass found: a status row written outside
// the lease is the lost update coord.Integrations forbids (a loop tick puts
// its stale copy back over an operator's disconnect). The guard spans the pass
// AND its record, and only the caller knows when it finished writing; the loop
// holds it through its worker's Guard, the dashboard through [Runner.Hold].
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
// A pass, a reconcile tick and a teardown cannot see each other; two at once
// is one creating the account the other deletes. BOTH HALVES, each alone
// leaving the collision reachable: the duty lease stops two NODES but not two
// goroutines here ([coord.Backend.TryAcquire] renews for the current owner,
// and it is absent on a single-node install), the in-process claim the
// reverse. It covers the writes ABOUT a surface too: status rows are
// last-write-wins ([coord.Integrations]), so recording an outcome takes this
// hold; no hold, no write. The returned context carries [PassDeadline], derived
// from the CALLER's. held is false when somebody has it; context and release
// are non-nil exactly when held is true. An error is not "idle".
func (r *Runner) Hold(
	ctx context.Context, kind integration.Kind,
) (context.Context, func(), bool, error) {
	if r == nil {
		// NO RUNNER, SO NO LEASE — and the deadline still applies. What it
		// bounds here is not a lease but the pass: an engine built with no
		// fleet store has no runner, and a read that never returns wedges
		// the loop just as thoroughly as one that outlived a lease would.
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
// IN THAT ORDER:
//   - The local claim first: taking the remote lease first would hold it for
//     its full [LeaseTTL] whenever a second goroutine here lost the local race.
//   - The lease on the CALLER's context, never the bounded one built after it:
//     schedule.HoldNamedDuty wraps it in context.WithoutCancel so a cancelled
//     pass still releases, but from a context that hit [PassDeadline] every
//     release is a no-op, locking the surface out for the rest of [LeaseTTL].
//   - The deadline last, so its margin inside [LeaseTTL] is the whole minute,
//     not a minute minus the claim's duration.
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

// Recent is the passes this node remembers for one surface, newest first.
//
// PER NODE AND IT SAYS SO. A run is executed by whichever node held the
// surface's lease and is remembered in that node's own process, bounded by
// [maxRuns] — so this is "what did THIS node last do here", not the fleet's
// history. The fleet's own record is the reconcile State, which carries the
// conclusion rather than the findings.
//
// The reason it exists: `GET /setup/integrations/{kind}/runs/{id}` could
// answer one pass and nothing could name an id, so the route was reachable
// only by a caller that had just started a pass itself. An operator opening a
// Findings tab had no way in at all.
func (r *Runner) Recent(kind integration.Kind, limit int) []*Run {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// NIL WHEN THERE IS NOTHING, deliberately: the JSON surface above is
	// the one place that owes a caller an allocated list, and two places
	// normalising it is how one of them stops. A nil Runner and a runner
	// that has executed nothing here answer alike, which is what makes the
	// handler's own guard the single rule rather than a second one.
	var out []*Run
	for _, run := range r.runs {
		if run.Kind == kind {
			out = append(out, run)
		}
	}
	// NEWEST FIRST, and the id breaks a tie: map order is not
	// deterministic, and two passes can start inside one clock tick.
	slices.SortFunc(out, func(a, b *Run) int {
		return cmp.Or(b.StartedAt.Compare(a.StartedAt), cmp.Compare(a.ID, b.ID))
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
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
