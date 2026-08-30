package turn

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("agent.turn")

// PlanDecision is what the planner concluded about the trigger.
type PlanDecision string

const (
	// PlanRun — run these steps through Execute, then Review.
	PlanRun PlanDecision = "plan"

	// PlanDirect — a one-tool task with no multi-step plan. Execute runs
	// against the full surface and Review is skipped, subject to the
	// engine's own safety net: see [phase.Gate.MustReview].
	PlanDirect PlanDecision = "direct"

	// PlanSkip — nobody was asking this seat to do anything. The turn ends
	// immediately and nothing is posted back.
	//
	// Deliberately NOT the way to decline. A seat that was mentioned,
	// assigned or asked and is saying no must do that as a plan with one
	// reply step, so the requester learns the message was received rather
	// than waiting in silence for an answer that is never coming.
	PlanSkip PlanDecision = "skip"
)

// Plan is what the Plan phase produced.
type Plan struct {
	Decision  PlanDecision
	Reasoning string
	Summary   string

	// ToolsNeeded is every tool Execute will call, as the planner named
	// them — research AND the final delivery tool. Kept RAW: the split
	// into resolved and phantom happens against the live catalogue, and
	// the raw list is what says whether the plan intended to act at all.
	ToolsNeeded []string

	SuccessCriteria []string

	// Calls is what Plan itself called during recon.
	Calls []ledger.Call

	// Rescued marks a decision the ENGINE synthesised because the planner
	// never submitted one.
	//
	// It is not decoration. `direct` is the one decision that skips
	// Review, and skipping it is only safe when a planner CHOSE it — that
	// choice is the planner saying "Execute finishes this in one shot".
	// A rescued plan carries the same word without the commitment behind
	// it, and it also carries no ToolsNeeded, so the delivery gate that
	// would otherwise force Review reads it as a turn that intended
	// nothing and lets it through. Observed: a seat was addressed on
	// chat, ran a full turn, produced an answer, called nothing, and
	// reported done — a silent no-op, which is the exact failure the gate
	// exists to prevent.
	Rescued bool
}

// Execution is what the Execute phase produced.
type Execution struct {
	Text  string
	Calls []ledger.Call

	// MissingTools are names the phase called that the surface did not
	// have. Surfaced to Review, which is what turns "the model hallucinated
	// a tool" into a re-plan naming the real one.
	MissingTools []string

	// ExhaustedRounds marks a phase that hit its round cap. Review sees it
	// and normally iterates.
	ExhaustedRounds bool

	// Suspended marks an Execute that parked on a detached sandbox run. The
	// turn ends here and its completion starts a new one, so the loop must
	// hand the accumulated ledger back rather than treat this as an ending.
	Suspended bool
}

// Review is what the Review phase produced.
type Review struct {
	Decision phase.Decision
	Notes    string

	// CompletedWork is what already landed, in the reviewer's own words —
	// the semantic layer over the engine-built call ledger. Empty whenever
	// Review never chose self_iterate itself, above all on the engine's
	// done→self_iterate override, which is exactly why the ledger and not
	// this field carries the guarantee.
	CompletedWork string

	// FinalArtifact is what Review wants returned. Empty reuses Execute's
	// text.
	FinalArtifact string
}

// Surface is what the loop knows about the tools available this round.
type Surface struct {
	// Catalogue is every tool name the phase could reach.
	Catalogue []string
	// MCPTools is every tool backed by an MCP server.
	MCPTools []string
	// KnownReads is every tool POSITIVELY annotated read-only.
	KnownReads []string
}

// Phases is the model-facing work the loop drives. Everything that needs a
// provider, a tool registry or a network lives behind it.
type Phases interface {
	// Plan runs the planning pass. Round is 1-based.
	Plan(ctx context.Context, round int, notes string, history []ledger.Iteration) (Plan, Surface, error)

	// Execute runs the plan. The surface it reports is the one the delivery
	// gate judges against, which is why it comes back from the phase rather
	// than being assumed from Plan's: activating a tool mid-Execute changes
	// it, and judging against a stale catalogue turns a real delivery into
	// a phantom.
	Execute(ctx context.Context, round int, p Plan, history []ledger.Iteration) (Execution, Surface, error)

	// Review judges the round. It is not called when the loop skips Review.
	Review(ctx context.Context, round int, p Plan, e Execution, history []ledger.Iteration) (Review, error)

	// Resume re-enters an Execute phase that suspended on a detached sandbox
	// run, with the run's result spliced in as the pending call's reply.
	//
	// Called INSTEAD OF Plan and Execute for the first round of a resumed
	// turn — a resumed turn that re-planned would re-derive a plan for work
	// already half-done. Everything after that round is an ordinary turn.
	Resume(ctx context.Context, history []ledger.Iteration) (Execution, Surface, error)
}

// Input is one turn's starting state.
type Input struct {
	TurnID string

	// Depth is the delegation depth this turn inherited.
	Depth int

	// History is the ledger carried across a sandbox suspend. The resumed
	// turn re-enters mid-loop, so without this it would forget every round
	// the suspended one closed and could re-fire their deliveries.
	History []ledger.Iteration

	// Resume re-enters a suspended Execute phase rather than starting from
	// Plan. The SAME turn id continues (d-402): the resumed conversation is
	// the one the sandbox call left waiting, not a fresh turn that would
	// re-plan and re-investigate from scratch.
	Resume bool

	// ResumePlan is the plan the suspended turn was executing, so the
	// delivery gate and Review judge the round against what it INTENDED
	// rather than against nothing. Rebuilt from the pending row; a zero
	// value means the plan could not be recovered, which downgrades the
	// gate rather than failing the resume.
	ResumePlan Plan
}

// Settings is the turn's pinned configuration.
//
// A struct of exactly what the loop reads, not the whole config: the loop's
// contract is then visible in its signature, and a caller cannot accidentally
// hand it a live cell.
type Settings struct {
	// MaxIterations caps Plan→Execute→Review rounds. 0 or less means one
	// round, because a turn that runs no rounds cannot be what anyone
	// configured — and treating it as unbounded would let a misconfiguration
	// spend a company's whole budget on one trigger.
	MaxIterations int

	// DelegationDepthLimit caps colleague-to-colleague chains. 0 disables.
	DelegationDepthLimit int

	// StallThreshold is how many identical rounds end the turn. 0 takes the
	// detector's own default.
	StallThreshold int

	// SkipNames are meta-tools filtered from the ledger — never a delivery,
	// so pure noise in a record of what already happened that matters.
	SkipNames []string
}

func (s Settings) iterations() int {
	if s.MaxIterations < 1 {
		return 1
	}
	return s.MaxIterations
}

// Result is the turn's outcome.
type Result struct {
	Decision phase.Decision
	Artifact string

	// Iterations is the closed-round ledger. Carried out so a suspend can
	// persist it and the resumed turn can carry on knowing what already
	// fired.
	Iterations []ledger.Iteration

	// Rounds is how many Plan passes actually ran.
	Rounds int

	// Breach names the guard that ended the turn, if one did.
	Breach *Breach

	// LastReview is the reviewer's last word. A `done` round appends no
	// ledger entry, so without this the reviewer's own prose about what
	// landed never reaches the conversation ledger written at turn end.
	LastReview *Review

	// Suspended marks a turn parked on a detached sandbox run.
	Suspended bool
}

// Run drives the turn.
//
// It returns an error only when a phase itself broke. A turn that failed its
// guards, ran out of rounds, or was reviewed as not-done is a RESULT, not an
// error: those are ordinary outcomes the caller records and reports, and
// collapsing them into err would make "the model did not finish" and "the
// process is broken" one condition.
func Run(ctx context.Context, ph Phases, set Settings, in Input) (Result, error) {
	if ph == nil {
		return Result{}, fmt.Errorf("turn: no phases")
	}
	if err := CheckDepth(in.Depth, set.DelegationDepthLimit); err != nil {
		//nolint:nilerr // A breach is a turn OUTCOME, not a broken process — the
		// distinction this function's doc comment exists to keep.
		return Result{
			Decision: phase.Failed,
			Breach:   &Breach{Kind: BreachDepth, Detail: err.Error()},
		}, nil
	}

	res := Result{Decision: phase.Failed, Iterations: slices.Clone(in.History)}
	stall := StallDetector{Threshold: set.StallThreshold}
	notes := ""
	maxRounds := set.iterations()

	resuming := in.Resume
	for round := 1; round <= maxRounds; round++ {
		res.Rounds = round

		var (
			p           Plan
			planSurface Surface
			exec        Execution
			execSurface Surface
			err         error
		)
		if resuming {
			// The first round of a resumed turn re-enters the suspended
			// Execute conversation. Plan is skipped, and only for this
			// round: if the resumed executor self-iterates, round two is an
			// ordinary planned round again.
			resuming = false
			p = in.ResumePlan
			exec, execSurface, err = ph.Resume(ctx, res.Iterations)
			if err != nil {
				return res, fmt.Errorf("turn: resume round %d: %w", round, err)
			}
		} else {
			p, planSurface, err = ph.Plan(ctx, round, notes, res.Iterations)
			if err != nil {
				return res, fmt.Errorf("turn: plan round %d: %w", round, err)
			}

			if p.Decision == PlanSkip {
				// Nothing was being asked. The turn ends with the planner's
				// reasoning as its output and nothing reaches the requester.
				log.InfoContext(ctx, "turn_skipped", "turn_id", in.TurnID, "reason", p.Reasoning)
				res.Decision = phase.Skipped
				res.Artifact = p.Reasoning
				return res, nil
			}

			exec, execSurface, err = ph.Execute(ctx, round, p, res.Iterations)
			if err != nil {
				return res, fmt.Errorf("turn: execute round %d: %w", round, err)
			}
		}
		if exec.Suspended {
			// The turn parks here and its completion starts a new one. The
			// ledger goes out so that turn inherits it — this round is NOT
			// closed and must not be appended, because its Review has not
			// run and appending it would tell the resumed turn a delivery
			// was judged when nothing judged it.
			log.InfoContext(ctx, "turn_suspended", "turn_id", in.TurnID, "round", round)
			res.Suspended = true
			res.Decision = phase.SelfIterate
			res.Artifact = exec.Text
			return res, nil
		}

		// The gate judges against the surface EXECUTE reported. Activating a
		// tool mid-phase changes the catalogue, and judging a real delivery
		// against Plan's stale view reads it as a phantom.
		surface := execSurface
		if len(surface.Catalogue) == 0 {
			surface = planSurface
		}
		resolved, phantom := phase.ResolvePlanned(p.ToolsNeeded, surface.Catalogue)
		gate := phase.Gate{
			// Keyed off the RAW list: a plan naming only tools that do not
			// exist still intended to act, and reading it as intending
			// nothing turns a failed delivery into a clean turn.
			ExpectedAction:  len(p.ToolsNeeded) > 0,
			PlannedResolved: resolved,
			PlannedPhantom:  phantom,
			PlanCalled:      names(p.Calls),
			ExecuteCalled:   names(exec.Calls),
			MCPTools:        surface.MCPTools,
			KnownReads:      surface.KnownReads,
		}

		// A RESCUED plan never skips Review. The engine wrote that
		// `direct`, not the planner, so there is no commitment to honour
		// — and because a rescue names no tools, the gate below cannot
		// catch it either: ExpectedAction is false, so a rescued turn
		// that delivered nothing would complete as done.
		skipReview := p.Decision == PlanDirect && !p.Rescued
		if p.Decision == PlanDirect && p.Rescued {
			log.WarnContext(ctx, "review_forced_plan_was_rescued",
				"turn_id", in.TurnID, "round", round,
				"detail", "the planner never submitted a decision, so the engine "+
					"cannot honour one; Review judges what Execute actually did")
		}
		if skipReview && gate.MustReview() {
			log.WarnContext(ctx, "review_forced_execute_skipped_delivery",
				"turn_id", in.TurnID, "round", round,
				"tools_needed", p.ToolsNeeded, "phantom", phantom,
				"called", gate.ExecuteCalled)
			skipReview = false
		}
		if skipReview {
			log.InfoContext(ctx, "review_skipped_per_plan", "turn_id", in.TurnID, "round", round)
			res.Decision = phase.Done
			res.Artifact = exec.Text
			return res, nil
		}

		rev, err := ph.Review(ctx, round, p, exec, res.Iterations)
		if err != nil {
			return res, fmt.Errorf("turn: review round %d: %w", round, err)
		}
		res.LastReview = &rev

		decision := rev.Decision
		notes = rev.Notes
		artifact := rev.FinalArtifact
		if artifact == "" {
			artifact = exec.Text
		}
		res.Artifact = artifact

		if decision == phase.Done {
			if override, correction := gate.OverrideDone(); override {
				log.WarnContext(ctx, "review_done_overridden_undelivered",
					"turn_id", in.TurnID, "round", round,
					"tools_needed", p.ToolsNeeded, "phantom", phantom,
					"called", gate.Called())
				decision = phase.SelfIterate
				notes = phase.AppendCorrection(notes, correction)
			}
		}

		if decision == phase.Done {
			res.Decision = phase.Done
			return res, nil
		}
		if decision == phase.Failed {
			// The reviewer gave up. Its own notes are the record; no guard
			// fired, so none is reported.
			res.Decision = phase.Failed
			return res, nil
		}

		stall.Observe(artifact)
		if stall.ShouldAbort() {
			log.InfoContext(ctx, "turn_stall_aborted", "turn_id", in.TurnID, "round", round)
			res.Decision = phase.Failed
			res.Breach = &Breach{
				Kind:   BreachStall,
				Detail: "consecutive self_iterate rounds produced the same artifact",
			}
			return res, nil
		}

		// A closed round. Two layers, deliberately: the call lists are
		// ENGINE-recorded so they cannot be forgotten — which matters most
		// on the override path just above, where Review said done and wrote
		// no completed_work at all, yet is exactly where a partial delivery
		// may already have landed — and CompletedWork is the reviewer's
		// prose gloss the mechanical log cannot express.
		res.Iterations = append(res.Iterations, ledger.Iteration{
			Iteration:    round,
			PlanSummary:  p.Summary,
			PlanCalls:    p.Calls,
			ExecuteCalls: exec.Calls,
			// Only the reads actually CALLED, not the whole surface's
			// annotation set. Rendering is a membership test either way,
			// so the block reads the same — but the row is persisted
			// across a sandbox suspend, and carrying every read-only tool
			// on a large MCP surface makes it grow with the catalogue
			// rather than with what the round did.
			Reads:         calledReads(p.Calls, exec.Calls, surface.KnownReads),
			ExecuteText:   exec.Text,
			ReviewNotes:   notes,
			CompletedWork: rev.CompletedWork,
		})
	}

	log.InfoContext(ctx, "turn_max_iterations_exhausted", "turn_id", in.TurnID, "max", maxRounds)
	res.Decision = phase.Failed
	res.Breach = &Breach{
		Kind: BreachMaxIterations,
		Detail: fmt.Sprintf("plan/execute/review loop exhausted at %d rounds without done",
			maxRounds),
	}
	return res, nil
}

// calledReads narrows a surface's read-only annotations to the ones this
// round actually used.
func calledReads(planCalls, execCalls []ledger.Call, known []string) []string {
	var out []string
	seen := make(map[string]bool, len(known))
	for _, c := range slices.Concat(planCalls, execCalls) {
		if !seen[c.Name] && slices.Contains(known, c.Name) {
			seen[c.Name] = true
			out = append(out, c.Name)
		}
	}
	slices.Sort(out)
	return out
}

func names(calls []ledger.Call) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return out
}
