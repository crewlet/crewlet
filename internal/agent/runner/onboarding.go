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
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// The first-turn onboarding pass runs BEFORE the executor, as its own phase
// with its own budget (turn_engine.onboarding_max_tool_rounds), so reading the
// team's pages never starves the work. Its surface is the onboarding builtins
// plus the discovery meta-tools, and its prompt lists every other tool it can
// reach, so it activates its knowledge-base tools as the executor would.
//
// NO REQUIRED-SKILL GUARD: the pass is a fixed read → persist → mark workflow,
// so the load-before-use tax would only burn its reading rounds. NO RESCUE: a
// pass out of rounds ends UNMARKED and retries next turn, since a rescue would
// invent a summary of pages it did not finish reading.

var onboardingLog = logging.Get("agent.onboarding")

// onboardingAlwaysOn is what the pass can call without discovering it.
// mark_onboarded must be here: it ends the pass, and without it the loop asks
// again after the model has marked.
var onboardingAlwaysOn = []string{toolname.ReflectAndPersist, toolname.MarkOnboarded}

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

// ClaimTTL bounds the cross-process pass lease: the round ceiling of 20 at
// 20-30 seconds a round worst case is about ten minutes, plus slack for a slow
// provider. A crashed claimant blocks re-onboarding for at most this long; a
// live pass releases explicitly in a defer rather than leaving it to the TTL.
const ClaimTTL = 15 * time.Minute

// Latch remembers, per process, which seats are known onboarded, so that a
// flaky read of the best-effort marker store cannot re-run a pass for a seat
// this process has already SEEN marked. Not for speed.
//
// KEYED BY CHAIN HASH, so a seat moved under a different unit by a live org
// restructure re-onboards by design.
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
	// Markers is the durable store. Nil skips the pass: without somewhere to
	// mark, it would run every turn forever.
	Markers Markers

	// Latch is the process-local memory of who is already onboarded.
	Latch *Latch

	// Rounds and Ceiling are the pass's own budget; it is the only
	// engine-driven phase the round-cap extension reaches.
	Rounds  int
	Ceiling int

	// Now is injectable so a suite can pin the lease clock.
	Now func() time.Time
}

// Onboard runs the first-turn pass if this seat still needs it, and reports
// whether it RAN (only logged). Five gates, so the pass runs until it marks
// and then never again for the chain:
//
//  1. No budget, store or seat: a pass never marked would run forever.
//  2. A human seat: never spawned, and no agent id to key a marker on.
//  3. The process latch ([Latch]).
//  4. The marker, TRI-STATE: an ERROR skips and retries next turn rather than
//     re-onboard marked seats on every database blip.
//  5. The cross-process lease: shared seat inboxes let two engines run the
//     same seat's turn in a rolling restart. It also excludes passes within
//     a process, so no in-process lock.
func (r *Runner) Onboard(ctx context.Context) (bool, error) {
	cfg := r.cfg.Onboarding
	seat := r.cfg.Seat.Role
	switch {
	case cfg.Rounds <= 0, cfg.Markers == nil, seat == nil, r.cfg.Seat.Org == nil:
		return false, nil
	}
	agentID, ok := r.cfg.Seat.Org.AgentIDFor(seat)
	if !ok {
		// A human seat.
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
		// Released whether or not the pass marked, so an unmarked one does
		// not block re-onboarding until the TTL.
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

// onboardingPass is the LLM pass itself, under the lease, at
// [onboardingIteration]. It runs through runPhase, as the reviewer does, so
// onboarding never drifts into a second turn engine.
func (r *Runner) onboardingPass(ctx context.Context, chain string) (bool, error) {
	snapshot := r.seatSnapshot()
	surface, err := r.surfaceWith(ctx, r.ownMeter(), phase.Onboarding, 0, snapshot, nil,
		onboardingAlwaysOn)
	if err != nil {
		return false, err
	}
	system := prompts.BuildOnboarding(r.cfg.Seat, prompts.OnboardingInput{
		Hint: learning.Hint(r.cfg.Seat.Org, r.cfg.Seat.Role),
		// Its knowledge-base tools are found here.
		Discoverable: snapshot.Discoverable(surface.Active()),
	})
	user := prompts.BuildOnboardingUserMessage()

	phaseCtx, res, err := r.runPhase(ctx, phaseRun{
		phase: phase.Onboarding, surface: surface, system: system, user: user,
		rounds: r.cfg.Onboarding.Rounds, ceiling: r.cfg.Onboarding.Ceiling,
		iteration: onboardingIteration,
		// Arms the corrective re-prompt for a pass that stops without
		// marking, which would otherwise re-fire on every turn this seat
		// takes. Never forced (phaseRun.terminateAfter).
		terminateAfter: []string{toolname.MarkOnboarded},
	})
	if err != nil {
		return false, fmt.Errorf("runner: onboarding: %w", err)
	}

	// MARKED is read off what ran, not the model's prose: latching a claim
	// would leave the seat unmarked in the store yet never re-onboarded here.
	marked := calledSuccessfully(surface, toolname.MarkOnboarded)
	notes := "did not mark (will retry next turn)"
	decision := ""
	if marked {
		notes, decision = "marked", "done"
	}
	r.emitter().completed(phaseCtx, phaseRecord{
		Phase: phase.Onboarding, Iteration: onboardingIteration,
		System: system, User: user, Result: res.Result, Exhausted: res.Exhausted,
		Elapsed: res.Elapsed, StartedAt: res.StartedAt, Caps: res.Caps,
		Decision: decision, Notes: notes, Available: surface.Active(), Picks: res.Picks,
	})
	onboardingLog.InfoContext(ctx, "onboarding_phase_complete",
		"agent", r.cfg.Seat.Role.Handle(), "turn_id", r.cfg.Turn.RunID,
		"marked", marked, "rounds", res.Rounds, "chain", chain)
	return marked, nil
}

// onboardingIteration groups the pass under the turn, before the executor's
// round 1.
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
