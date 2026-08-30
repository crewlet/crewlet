package runner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/tools"
)

// The dedicated first-turn onboarding pass, which runs BEFORE Plan.
//
// It used to happen INSIDE Plan, driven by a hint injected into the Plan
// prompt. On a genuine first turn that spent the Plan round budget on reading
// the team's pages and persisting conventions, and could starve submit_plan
// entirely — a seat's first ever turn was the one most likely to produce no
// plan at all.
//
// So it is its own phase with its own budget
// (turn_engine.onboarding_max_tool_rounds), and onboarding never competes with
// planning. Its surface is the onboarding builtins plus the discovery
// meta-tools, so the agent can find its knowledge-base server the same way
// Plan does — on a separate budget.
//
// NO REQUIRED-SKILL GUARD, deliberately: onboarding is a fixed read → persist
// → mark workflow and the hint is its own guidance, so the load-before-use tax
// would only burn rounds the pass needs for reading.
//
// There is no rescue path either. A pass that runs out of rounds simply ends
// UNMARKED and retries next turn, which is the right outcome — a rescue would
// have to invent a summary of pages the agent did not finish reading.

var onboardingLog = logging.Get("agent.onboarding")

// onboardingAlwaysOn is what the pass can call without discovering it.
//
// mark_onboarded is what ENDS the pass, so it is also the terminator: without
// it the loop asks again after the model has already marked, and the pass
// spends its remaining budget re-deciding something it has decided.
var onboardingAlwaysOn = []string{ReflectAndPersistTool, MarkOnboardedTool}

// The two builtin names this phase depends on. Named here rather than imported
// from internal/agent/builtin because that package imports tools, which
// imports turnctx — and the runner is what builds the surface both sit on.
// tests/… pins them against the builtin package's own constants, so a rename
// there fails the build here rather than silently emptying this surface.
const (
	ReflectAndPersistTool = "reflect_and_persist"
	MarkOnboardedTool     = "mark_onboarded"
)

// Markers is what the onboarding pass needs from the learning store.
type Markers interface {
	// Onboarded reports whether this seat has already onboarded for this
	// org chain. An ERROR is the third state and it means SKIP: see
	// [Runner.Onboard].
	Onboarded(ctx context.Context, agentID, chainHash string) (bool, error)

	// Claim takes the cross-process pass lease, reporting whether it was
	// taken. Release gives it back.
	Claim(ctx context.Context, agentID string, now time.Time, ttl time.Duration) (learning.Pass, error)
	Release(ctx context.Context, p learning.Pass, at time.Time) error
}

// ClaimTTL bounds the cross-process pass lease.
//
// The pass is bounded by the onboarding round budget — 10 rounds by default,
// judge-extended to a ceiling of 20, at 20-30 seconds a round worst case, so
// roughly ten minutes at the ceiling. Fifteen minutes covers that with slack
// for a slow provider. A crashed claimant blocks re-onboarding for at most
// this long, which is why the pass releases explicitly in a defer rather than
// letting the TTL do it.
const ClaimTTL = 15 * time.Minute

// Latch remembers, per process, which seats are known onboarded.
//
// The point is not speed. The marker store is best-effort, and a transient
// lookup failure must not re-run a whole onboarding pass for an agent this
// process has already SEEN marked — so once a read confirms it, or a pass
// marks it, no later read flake can re-fire.
//
// KEYED BY CHAIN HASH, which is what makes a live org restructure re-onboard
// by design: a seat that moved under a different unit is oriented to a company
// it has not read about.
type Latch struct {
	mu   sync.Mutex
	seen map[string]string // agent id -> chain hash
}

// NewLatch builds an empty latch. One per process, held by the engine.
func NewLatch() *Latch { return &Latch{seen: map[string]string{}} }

// Confirmed reports whether this process has seen the seat marked for exactly
// this chain.
func (l *Latch) Confirmed(agentID, chainHash string) bool {
	if l == nil || agentID == "" || chainHash == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[agentID] == chainHash
}

// Confirm records that the seat is onboarded for this chain.
func (l *Latch) Confirm(agentID, chainHash string) {
	if l == nil || agentID == "" || chainHash == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[agentID] = chainHash
}

// Onboarding is the per-turn wiring for the pass. Zero disables it, which is
// what a node with no marker store has.
type Onboarding struct {
	// Markers is the durable store. Nil skips the pass entirely: without
	// somewhere to mark, it would run every turn forever.
	Markers Markers

	// Latch is the process-local memory of who is already onboarded.
	Latch *Latch

	// Rounds and Ceiling are the pass's own budget, separate from Plan's.
	Rounds  int
	Ceiling int

	// Now is injectable so a suite can pin the lease clock.
	Now func() time.Time
}

// Onboard runs the first-turn pass if this seat still needs it.
//
// It reports whether the pass RAN, which the caller uses to suppress the
// Plan-prompt hint for the turn: a seat that has just been through the pass
// should not also be told to onboard.
//
// FIVE GATES, and every one of them exists because onboarding must run until
// it marks and then never again for the same chain:
//
//  1. No budget, no store, no seat — nothing to do, and a pass that could
//     never be marked would run every turn forever.
//  2. The process latch: this process has already seen this seat marked.
//  3. The durable marker, read TRI-STATE. True skips. False runs. An ERROR
//     skips and retries next turn — collapsing a failed lookup into "not
//     onboarded" would re-run full passes for already-marked agents on every
//     transient database blip, which is strictly worse than waiting a turn.
//  4. The cross-process lease. Seat inboxes are shared subscriptions, so
//     during a rolling restart two engines can each run a turn for the same
//     un-onboarded seat. The lease makes exactly one of them run the pass.
//
// The lease is also why there is no in-process lock here: it is atomic and
// excludes concurrent passes within a process as well as across, so a second
// lock would guard a path the first one already closes.
func (r *Runner) Onboard(ctx context.Context) (bool, error) {
	cfg := r.cfg.Onboarding
	seat := r.cfg.Seat.Role
	switch {
	case cfg.Rounds <= 0, cfg.Markers == nil, seat == nil, r.cfg.Seat.Org == nil:
		return false, nil
	}
	agentID, ok := r.cfg.Seat.Org.AgentIDFor(seat)
	if !ok {
		// A human seat is never spawned, so it never onboards.
		return false, nil
	}
	id := agentID.String()
	chain := learning.ChainHash(r.cfg.Seat.Org, seat)

	if cfg.Latch.Confirmed(id, chain) {
		return false, nil
	}
	done, err := cfg.Markers.Onboarded(ctx, id, chain)
	switch {
	case err != nil:
		onboardingLog.WarnContext(ctx, "onboarding_state_unknown_skipping",
			"agent", seat.Handle(), "error", err,
			"detail", "the marker could not be read, so the pass is skipped and "+
				"the check retries next turn; running it would re-onboard an "+
				"agent that is probably already marked")
		return false, nil
	case done:
		cfg.Latch.Confirm(id, chain)
		return false, nil
	}

	now := cfg.now()
	pass, err := cfg.Markers.Claim(ctx, id, now, ClaimTTL)
	if err != nil {
		onboardingLog.WarnContext(ctx, "onboarding_claim_failed",
			"agent", seat.Handle(), "error", err)
		return false, nil
	}
	if !pass.Held() {
		onboardingLog.InfoContext(ctx, "onboarding_claimed_elsewhere_skipping",
			"agent", seat.Handle())
		return false, nil
	}
	defer func() {
		// Released explicitly whether or not the pass marked. A marked
		// pass has already cleared it; an unmarked or crashed one must not
		// hold re-onboarding hostage until the TTL.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := cfg.Markers.Release(context.WithoutCancel(ctx), pass, cfg.now()); err != nil {
			onboardingLog.Warn("onboarding_release_failed",
				"agent", seat.Handle(), "error", err)
		}
	}()

	marked, err := r.onboardingPass(ctx, chain)
	if err != nil {
		return true, err
	}
	if marked {
		cfg.Latch.Confirm(id, chain)
	}
	return true, nil
}

// onboardingPass is the LLM pass itself, under the lease.
//
// ITERATION 0, so a dashboard groups it under the turn BEFORE the first Plan,
// which runs at iteration 1. The phase events, the extension judge and the
// failure path all come from runPhase — the same machinery Plan and Execute
// use, which is what stops onboarding drifting into a second turn engine.
func (r *Runner) onboardingPass(ctx context.Context, chain string) (bool, error) {
	snapshot := r.cfg.Registry.Snapshot()
	surface, err := r.surfaceWith(phase.Onboarding, snapshot, nil, onboardingAlwaysOn)
	if err != nil {
		return false, err
	}
	system := prompts.BuildOnboarding(r.cfg.Seat, prompts.OnboardingInput{
		Hint:          learning.Hint(r.cfg.Seat.Org, r.cfg.Seat.Role),
		ToolCatalogue: r.cfg.Registry.Catalogue(),
	})
	const user = "Complete your onboarding now."

	phaseCtx, res, err := r.runPhase(ctx, phaseRun{
		phase: phase.Onboarding, surface: surface, system: system, user: user,
		rounds: r.cfg.Onboarding.Rounds, ceiling: r.cfg.Onboarding.Ceiling,
		iteration:      onboardingIteration,
		terminateAfter: []string{MarkOnboardedTool},
	})
	if err != nil {
		return false, fmt.Errorf("runner: onboarding: %w", err)
	}

	// MARKED is read off what actually ran, not off the model's prose: a
	// pass that said it was done without calling the tool has not marked,
	// and treating its claim as the fact would leave a seat permanently
	// unmarked in the store while never onboarding again in this process.
	marked := calledSuccessfully(surface, MarkOnboardedTool)
	notes := "did not mark (will retry next turn)"
	decision := ""
	if marked {
		notes, decision = "marked", "done"
	}
	r.emitter().completed(phaseCtx, phaseRecord{
		Phase: phase.Onboarding, Iteration: onboardingIteration,
		System: system, User: user, Result: res.Result, Exhausted: res.Exhausted,
		Decision: decision, Notes: notes, Available: surface.Active(),
	})
	onboardingLog.InfoContext(ctx, "onboarding_phase_complete",
		"agent", r.cfg.Seat.Role.Handle(), "turn_id", r.cfg.Turn.ID,
		"marked", marked, "rounds", res.Rounds, "chain", chain)
	// RECORDED HERE, not left to the caller. The Plan prompt carries an
	// onboarding hint rendered BEFORE this pass ran — the prefetch is
	// frozen at turn start, and at that moment the seat genuinely had not
	// onboarded — so without this a seat that onboards on this very turn
	// is then told to go and read the pages it has just read. The rule
	// belongs where the fact is, because a caller can forget it and this
	// cannot.
	r.mu.Lock()
	r.onboardedThisTurn = true
	r.mu.Unlock()
	return marked, nil
}

// onboardingIteration groups the pass under the turn, before Plan's round 1.
const onboardingIteration = 0

// calledSuccessfully reports whether a named tool ran and did not fail.
func calledSuccessfully(surface *tools.Surface, name string) bool {
	for _, c := range surface.Calls() {
		if c.Name == name && !c.Failed {
			return true
		}
	}
	return false
}

func (o Onboarding) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}
