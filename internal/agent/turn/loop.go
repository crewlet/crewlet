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

// Work is what the executor phase produced.
//
// ONE PHASE DECIDES AND ACTS. The turn used to plan in one conversation and
// act in another, which cost it everything the planner learned: the executor
// could not see what the plan had read, so content was smuggled through the
// plan's own steps, and the planner had to NAME the tools it expected — on a
// catalogue it was never shown, so it guessed. Every guess that missed became
// a "phantom" the engine then had to reason about, and the delivery gate was
// built entirely out of reconciling those guesses against reality.
type Work struct {
	// Outcome is the executor's own account of the round. Engine-checked
	// against the record before anything acts on it — see [Check].
	Outcome Outcome

	// Summary is what the executor says it did, in its own words. It is
	// the intent line the next round and the reviewer read.
	Summary string

	// Deliveries are the tools the executor cites as having delivered.
	// Reported rather than trusted: the record is what [Check] reads.
	Deliveries []string

	// Evidence is what the executor tried and what stopped it, required
	// when the outcome is blocked. It reaches the reviewer, which is the
	// whole point of demanding it: "blocked" with no account of what was
	// tried is a round the reviewer can only send back blind.
	Evidence string

	// OpenQuestions is whatever the executor thinks the reviewer or the
	// next round should know that the summary does not cover.
	OpenQuestions string

	// Text is the phase's final prose — the answer, when the answer is
	// prose, and the fallback artifact when the reviewer names none.
	Text string

	// Calls is what the phase actually invoked, engine-recorded.
	Calls []ledger.Call

	// MissingTools are names the phase called that the surface did not
	// have. Surfaced to the reviewer, which is what turns "the model
	// hallucinated a tool" into a next round naming the real one.
	MissingTools []string

	// ExhaustedRounds marks a phase that hit its round cap.
	ExhaustedRounds bool

	// Suspended marks an executor that parked on a detached sandbox run.
	// The turn ends here and its completion resumes it, so the loop hands
	// the accumulated ledger back rather than treating this as an ending.
	Suspended bool

	// Rescued marks an outcome the ENGINE synthesised because the executor
	// never submitted one. See [OutcomeIncomplete].
	Rescued bool
}

// Review is what the reviewer produced.
type Review struct {
	Decision phase.Decision
	Notes    string

	// CompletedWork is what already landed, in the reviewer's own words —
	// the semantic layer over the engine-built call ledger. Empty whenever
	// the reviewer never chose self_iterate itself, above all on the
	// engine's done→self_iterate override, which is exactly why the ledger
	// and not this field carries the guarantee.
	CompletedWork string

	// FinalArtifact is what the reviewer wants returned. Empty reuses the
	// executor's text.
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
	// Execute runs the executor: one agentic pass that decides what to do
	// and does it. Round is 1-based; notes carry the previous round's
	// correction.
	//
	// The surface it reports is the one the delivery check judges against,
	// which is why it comes back from the phase rather than being assumed:
	// activating a tool mid-run changes the catalogue, and judging a real
	// delivery against a stale one reads it as no delivery at all.
	Execute(ctx context.Context, round int, notes string, history []ledger.Iteration) (Work, Surface, error)

	// Review judges the round against the engine's record of it.
	Review(ctx context.Context, round int, w Work, history []ledger.Iteration) (Review, error)

	// Resume re-enters an executor that suspended on a detached sandbox
	// run, with the run's result spliced in as the pending call's reply.
	//
	// Called INSTEAD OF Execute for the first round of a resumed turn. A
	// resumed turn that started over would re-derive work already half
	// done; everything after that round is an ordinary round.
	Resume(ctx context.Context, history []ledger.Iteration) (Work, Surface, error)
}

// Input is one turn's starting state.
type Input struct {
	TurnID string

	// Depth is the delegation depth this turn inherited.
	Depth int

	// Reply says who is waiting for this turn — the engine's own reading
	// of the trigger, and the half of the delivery question a model cannot
	// get wrong. See [Reply].
	Reply Reply

	// History is the ledger carried across a sandbox suspend. The resumed
	// turn re-enters mid-loop, so without this it would forget every round
	// the suspended one closed and could re-fire their deliveries.
	History []ledger.Iteration

	// Resume re-enters a suspended executor rather than starting a fresh
	// round. The SAME turn id continues: the resumed conversation is the
	// one the sandbox call left waiting.
	Resume bool
}

// Settings is the turn's pinned configuration.
//
// A struct of exactly what the loop reads, not the whole config: the loop's
// contract is then visible in its signature, and a caller cannot accidentally
// hand it a live cell.
type Settings struct {
	// MaxIterations caps executor→reviewer rounds. 0 or less means one
	// round, because a turn that runs no rounds cannot be what anyone
	// configured — and treating it as unbounded would let a misconfiguration
	// spend a company's whole budget on one trigger.
	MaxIterations int

	// DelegationDepthLimit caps colleague-to-colleague chains. 0 disables.
	DelegationDepthLimit int

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

	// Rounds is how many executor passes actually ran.
	Rounds int

	// Breach names the guard that ended the turn, if one did.
	Breach *Breach

	// LastReview is the reviewer's last word, and LastWork the executor's.
	//
	// A `done` round appends no ledger entry — it ends the turn instead —
	// so without these two the last round is invisible to everything after
	// the loop, and the conversation ledger written at turn end recorded a
	// reply with no account of what produced it.
	LastReview *Review
	LastWork   *Work

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
	stall := StallDetector{}
	notes := ""
	maxRounds := set.iterations()

	resuming := in.Resume
	for round := 1; round <= maxRounds; round++ {
		res.Rounds = round

		var (
			work    Work
			surface Surface
			err     error
		)
		if resuming {
			// The first round of a resumed turn re-enters the suspended
			// conversation, and only this round: if the resumed executor
			// loops back, round two is an ordinary round.
			resuming = false
			work, surface, err = ph.Resume(ctx, res.Iterations)
			if err != nil {
				return res, fmt.Errorf("turn: resume round %d: %w", round, err)
			}
		} else {
			work, surface, err = ph.Execute(ctx, round, notes, res.Iterations)
			if err != nil {
				return res, fmt.Errorf("turn: execute round %d: %w", round, err)
			}
		}

		if work.Suspended {
			// The turn parks here and its completion resumes it. The
			// ledger goes out so that turn inherits it — this round is NOT
			// closed and must not be appended, because its review has not
			// run and appending it would tell the resumed turn a delivery
			// was judged when nothing judged it.
			log.InfoContext(ctx, "turn_suspended", "turn_id", in.TurnID, "round", round)
			res.Suspended = true
			res.Decision = phase.SelfIterate
			res.Artifact = work.Text
			return res, nil
		}

		// The engine's own reading of the round, from the record rather
		// than from the executor's account of it. Two of its three answers
		// cost no model call at all.
		verdict := Check(work, in.Reply, surface)
		if verdict.Skip {
			log.InfoContext(ctx, "turn_no_action", "turn_id", in.TurnID,
				"round", round, "summary", work.Summary)
			res.Decision = phase.Skipped
			res.Artifact = work.Summary
			return res, nil
		}

		var rev Review
		if verdict.Correction != "" {
			// The executor's own account failed the check, so the round is
			// sent back WITHOUT spending a review call on it. Recorded as
			// the reviewer's word for the ledger's sake — the next round
			// has to read a correction from somewhere — but marked as the
			// engine's, since no model judged it.
			log.WarnContext(ctx, "round_corrected_before_review",
				"turn_id", in.TurnID, "round", round, "outcome", string(work.Outcome),
				"correction", verdict.Correction)
			rev = Review{Decision: phase.SelfIterate, Notes: verdict.Correction}
		} else {
			rev, err = ph.Review(ctx, round, work, res.Iterations)
			if err != nil {
				return res, fmt.Errorf("turn: review round %d: %w", round, err)
			}
		}
		res.LastReview = &rev
		res.LastWork = &work

		decision := rev.Decision
		notes = rev.Notes
		artifact := rev.FinalArtifact
		if artifact == "" {
			artifact = work.Text
		}
		res.Artifact = artifact

		if decision == phase.Done {
			if override, correction := OverrideDone(work, in.Reply, surface); override {
				log.WarnContext(ctx, "review_done_overridden_undelivered",
					"turn_id", in.TurnID, "round", round,
					"cited", work.Deliveries, "called", names(work.Calls))
				decision = phase.SelfIterate
				notes = AppendCorrection(notes, correction)
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

		// A closed round. Two layers, deliberately: the call list is
		// ENGINE-recorded so it cannot be forgotten — which matters most
		// on the override path just above, where the reviewer said done
		// and wrote no completed_work at all, yet is exactly where a
		// partial delivery may already have landed — and CompletedWork is
		// the reviewer's prose gloss the mechanical log cannot express.
		res.Iterations = append(res.Iterations, ledger.Iteration{
			Iteration: round,
			Intent:    work.Summary,
			Calls:     work.Calls,
			// Only the reads actually CALLED, not the whole surface's
			// annotation set. Rendering is a membership test either way,
			// so the block reads the same — but the row is persisted
			// across a sandbox suspend, and carrying every read-only tool
			// on a large MCP surface makes it grow with the catalogue
			// rather than with what the round did.
			Reads:         calledReads(work.Calls, surface.KnownReads),
			Text:          work.Text,
			ReviewNotes:   notes,
			CompletedWork: rev.CompletedWork,
		})
	}

	log.InfoContext(ctx, "turn_max_iterations_exhausted", "turn_id", in.TurnID, "max", maxRounds)
	res.Decision = phase.Failed
	res.Breach = &Breach{
		Kind: BreachMaxIterations,
		Detail: fmt.Sprintf("executor/review loop exhausted at %d rounds without done",
			maxRounds),
	}
	return res, nil
}

// calledReads narrows a surface's read-only annotations to the ones this
// round actually used.
func calledReads(calls []ledger.Call, known []string) []string {
	var out []string
	seen := make(map[string]bool, len(known))
	for _, c := range calls {
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
