package turn

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events/types"
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

	// Carried is how many of Calls, from the front, were answered before
	// this round re-entered a suspended conversation: the calls the phase
	// made before it suspended, the call it suspended on, and — for an
	// agentic run — every call the run made in its box. Zero on a round
	// that re-entered nothing.
	//
	// Every question about DELIVERY reads the whole of Calls, because a
	// post made before the suspend reached the person whichever round made
	// it. The one question that must not is [Acted]'s — would running this
	// round again repeat something irreversible — because a retry re-enters
	// the conversation the pending-run row holds, in which every carried
	// call is already answered and none is made again. See [Work.Made].
	Carried int

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

// Made is the calls this round made itself: [Work.Calls] after its carried
// prefix, and what a retry of the round would make again.
//
// A Carried outside the list is clamped rather than trusted: past the end it
// leaves nothing, which [Acted] reads as proving nothing — the direction that
// keeps a trigger's retry — and below zero it leaves every call.
func (w Work) Made() []ledger.Call {
	return w.Calls[min(max(w.Carried, 0), len(w.Calls)):]
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
	// Deliveries is every tool whose successful call could reach
	// somebody outside this turn, MAPPED TO THE SURFACE it reaches them on
	// — see [Deliverable] and [DeliveredTo].
	//
	// A map rather than a set, because "did this turn reach anybody" and
	// "did the person waiting get told" are different questions and only
	// the second one matters to the asker. A flat set can only answer the
	// first, and answering the first is how a seat closed out a founder's
	// chat thread by writing a row in the tracker.
	Deliveries map[string]string
	// KnownReads is every tool POSITIVELY annotated read-only.
	KnownReads []string
	// KnownOpenWorld is every tool POSITIVELY annotated open-world — one
	// whose own annotations say it reaches outside this process.
	//
	// Separate from Deliveries because the two answer different halves of
	// the same question and neither contains the other: a deliverable is a
	// tool whose call reaches somebody WAITING on this turn, and `a2a_ask`
	// and `run_sandbox` leave the process without answering anybody — one
	// wakes a colleague, the other starts a billed box.
	KnownOpenWorld []string
}

// Reaches reports whether this surface holds any tool that delivers to the
// named surface.
//
// What makes [DeliveredTo] safe to narrow. A seat holding no tool for the
// surface an ask arrived on cannot answer there at all, so a gate that
// insisted would spend the turn's whole budget on a delivery that was never
// available — an operator's missing integration turned into a failing seat.
func (s Surface) Reaches(surface string) bool {
	for _, on := range s.Deliveries {
		if on == surface {
			return true
		}
	}
	return false
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
	//
	// Round is the number the loop gives the round that re-enters — one
	// past the highest round in history, see [Input.History] — and the
	// phase files its record under it, so the re-entered round and its
	// review are one round on every record.
	//
	// The Work it returns carries the whole phase's calls and says, in
	// [Work.Carried], how many of them were answered before the re-entry.
	Resume(ctx context.Context, round int, history []ledger.Iteration) (Work, Surface, error)
}

// Input is one turn's starting state.
type Input struct {
	// RunID names THIS EXECUTION of the turn — see [turnctx.Turn.RunID]
	// and ADR-0017. Log lines here name it `turn_id`, which is what every
	// event and every screen calls it.
	RunID string

	// Depth is the delegation depth this turn inherited.
	Depth int

	// Reply says who is waiting for this turn — the engine's own reading
	// of the trigger, and the half of the delivery question a model cannot
	// get wrong. See [Reply].
	Reply Reply

	// History is the ledger carried across a sandbox suspend. The resumed
	// turn re-enters mid-loop, so without this it would forget every round
	// the suspended one closed and could re-fire their deliveries.
	//
	// It is also where this run's round numbers start: the rounds in it are
	// the same turn's, under the same turn id, so this run numbers its own
	// on from the highest of them, and [Settings.MaxIterations] counts them.
	// A resumed turn that numbered from 1 again would file a second round 1
	// in the ledger recall_iteration reads and beside the first one's
	// records, and its cap would bound each resume rather than the turn.
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
	//
	// It caps the TURN, the rounds in [Input.History] included, with one
	// exception: the first round a run starts always runs. A turn parked on
	// a detached run while its cap was lowered to or below the rounds it
	// had already closed still re-enters its conversation, because leaving
	// the coding run's answer unread would lose the work the run did.
	MaxIterations int

	// DelegationDepthLimit caps colleague-to-colleague chains. 0 disables.
	DelegationDepthLimit int

	// SkipNames are meta-tools filtered from the ledger — never a delivery,
	// so pure noise in a record of what already happened that matters.
	SkipNames []string

	// Now is the clock the wall-clock cap reads, injectable so a test can
	// drive it without sleeping. Nil takes time.Now.
	Now func() time.Time

	// MaxWallClock bounds the whole turn. 0 or less is unbounded, which is
	// every trigger but a scheduled fire.
	//
	// Checked BETWEEN ROUNDS, never mid-phase. A phase that is running has
	// possibly already fired side effects, and abandoning it there would
	// leave a post half-written with nothing recording that it happened —
	// the same reason the drain lets a running turn finish. So the cap
	// refuses to start another round rather than interrupting one, and a
	// single round that outruns it is bounded by the phase timeouts beneath.
	MaxWallClock time.Duration
}

// now is the clock, injectable so a test can drive the wall-clock cap without
// sleeping through it.
func (s Settings) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
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

	// Rounds is the number of the last round the turn reached. A resumed
	// turn numbers its rounds on from the ones it closed before it
	// suspended (see [Input.History]), so this names a round of the whole
	// turn rather than counting this run's.
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

	// Acted reports that this turn's own record PROVES it already reached
	// outside the engine — see [Acted].
	//
	// It exists for the callers that have to tell a turn which broke
	// having done nothing from one which broke having already posted,
	// asked a colleague or started a box. Those are two different facts
	// and a redelivery is right for exactly one of them; the dispatcher
	// used to have only `err != nil`, which is neither. Both callers read
	// it through [Abandon] rather than directly.
	//
	// The zero value is the safe answer and the honest one: a turn that
	// proved nothing gets today's behaviour, which is to come back. Set
	// wherever the loop holds a round's record — never in a defer, because
	// [Run]'s results are unnamed and a deferred write to `res` would be
	// discarded at every `return res, …` in this file.
	Acted bool

	// Delivered reports whether the party waiting on this turn has its
	// answer, as of the LAST round — see [Answered], which is not simply
	// [DeliveredTo]: an A2A ask is answered by the engine and an
	// unprompted turn has nobody to answer, so neither is a question the
	// tool record can settle.
	//
	// THE LAST ROUND'S, not the turn's, and that is the whole point of it
	// being a separate field from [Result.Acted]. Acted accumulates,
	// because its question is "would a redelivery repeat something
	// irreversible" and an earlier round's write is irreversible forever.
	// This one must NOT, because its question is "does the waiting party
	// know how the turn ENDED" — and a turn that answered a question in
	// round one and then changed the answer in round two has left them
	// holding the wrong one. Accumulating it would report that turn as
	// having communicated its outcome.
	//
	// It exists for the conversation ledger, which files the turn's
	// artifact as the seat's own "You replied" and had no way to ask. On a
	// turn that delivered nothing it wrote a reply that never happened into
	// the one record the seat's NEXT turn on that thread reads back — so
	// the failure sealed itself, and a follow-up asking "did you do it?"
	// was answered against a fiction.
	Delivered bool
}

// Run drives the turn.
//
// It returns an error only when a phase itself broke. A turn that failed its
// guards, ran out of rounds, or was reviewed as not-done is a RESULT, not an
// error: those are ordinary outcomes the caller records and reports, and
// collapsing them into err would make "the model did not finish" and "the
// process is broken" one condition.
//
// A phase that PANICKED is a phase that broke: Run recovers it, returns a
// [PanicError] and names [types.GuardUnhandledException] on the result, so no
// panic in a phase unwinds past the loop. Whether the caller may run the turn
// again is [Abandon]'s answer, not a check of its own.
func Run(ctx context.Context, ph Phases, set Settings, in Input) (Result, error) {
	if ph == nil {
		return Result{}, fmt.Errorf("turn: no phases")
	}
	// REFUSED RATHER THAN DEFAULTED, because there is no safe default: see
	// [ReplyUnset]. Every delivery gate below turns on this field, so a
	// caller that omits it does not get a weaker turn — it gets a turn with
	// no delivery gate at all, and nothing about the result says so. That
	// shipped: the ordinary dispatch path omitted it while this package's
	// suite set it on every case.
	if !in.Reply.Valid() {
		return Result{}, fmt.Errorf(
			"turn: Input.Reply is %q, which is not one of %q, %q or %q — the "+
				"caller must derive it from the trigger (engine.ReplyFor) and "+
				"pass it, because every delivery check reads it",
			in.Reply, ReplyNone, ReplyTool, ReplyEngine)
	}
	if err := CheckDepth(in.Depth, set.DelegationDepthLimit); err != nil {
		//nolint:nilerr // A breach is a turn OUTCOME, not a broken process — the
		// distinction this function's doc comment exists to keep.
		return Result{
			Decision: phase.Failed,
			Breach:   &Breach{Kind: types.GuardDepthCap, Detail: err.Error()},
		}, nil
	}

	res := Result{Decision: phase.Failed, Iterations: slices.Clone(in.History)}
	stall := StallDetector{}
	notes := ""
	// THE TURN'S NUMBERING, not this run's: see [Input.History]. The cap is
	// the turn's too, except that the first round this run starts always
	// runs — see [Settings.MaxIterations].
	first := nextRound(in.History)
	lastRound := max(set.iterations(), first)

	started := set.now()
	resuming := in.Resume
	for round := first; round <= lastRound; round++ {
		// The wall-clock cap, at a ROUND BOUNDARY. The first round always
		// runs: a cap that refused before any work started would report a
		// turn as timed out having done nothing, which is a
		// misconfiguration rather than a slow turn and reads better as one.
		if elapsed := set.now().Sub(started); round > first &&
			set.MaxWallClock > 0 && elapsed >= set.MaxWallClock {
			res.Breach = &Breach{
				Kind: types.GuardScheduledTimeout,
				Detail: fmt.Sprintf(
					"the turn ran %s across %d round(s), past its %s cap, so no "+
						"further round was started", elapsed.Round(time.Second),
					round-first, set.MaxWallClock),
			}
			return res, nil
		}
		res.Rounds = round

		var (
			work    Work
			surface Surface
			err     error
		)
		// Named before the call, because `resuming` is cleared by it and
		// the error below has to say which phase actually broke.
		entered := "execute"
		if resuming {
			// The first round of a resumed turn re-enters the suspended
			// conversation, and only this round: if the resumed executor
			// loops back, the round after it is an ordinary round.
			resuming, entered = false, "resume"
			err = guarded(ctx, in.RunID, entered, round, func() (callErr error) {
				work, surface, callErr = ph.Resume(ctx, round, res.Iterations)
				return callErr
			})
		} else {
			err = guarded(ctx, in.RunID, entered, round, func() (callErr error) {
				work, surface, callErr = ph.Execute(ctx, round, notes, res.Iterations)
				return callErr
			})
		}
		// BEFORE THE ERROR CHECK, and that ordering is the point. A phase
		// that broke halfway through its tool loop hands back the calls it
		// made before it broke, and this is the only frame that sees them:
		// the error returns below carry `res`, and everything downstream
		// reads the turn through it.
		//
		// Accumulated with ||, never assigned: a round that read nothing
		// must not un-say what round one posted. And read off the calls the
		// round MADE, never the ones it carried into a re-entered
		// conversation: a retry re-enters that conversation with every
		// carried call already answered, so none of them is what a retry
		// would repeat — see [Work.Carried].
		res.Acted = res.Acted || Acted(work.Made(), surface)
		// ASSIGNED, never accumulated — the opposite of the line above, and
		// see [Result.Delivered] for why the two questions differ.
		res.Delivered = Answered(work.Calls, surface, in.Reply)
		if err != nil {
			return broke(res, fmt.Errorf("turn: %s round %d: %w", entered, round, err))
		}

		if work.Suspended {
			// The turn parks here and its completion resumes it. The
			// ledger goes out so that turn inherits it — this round is NOT
			// closed and must not be appended, because its review has not
			// run and appending it would tell the resumed turn a delivery
			// was judged when nothing judged it.
			log.InfoContext(ctx, "turn_suspended", "turn_id", in.RunID, "round", round)
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
			log.InfoContext(ctx, "turn_no_action", "turn_id", in.RunID,
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
				"turn_id", in.RunID, "round", round, "outcome", string(work.Outcome),
				"correction", verdict.Correction)
			rev = Review{Decision: phase.SelfIterate, Notes: verdict.Correction}
		} else {
			err = guarded(ctx, in.RunID, "review", round, func() (callErr error) {
				rev, callErr = ph.Review(ctx, round, work, res.Iterations)
				return callErr
			})
			if err != nil {
				return broke(res, fmt.Errorf("turn: review round %d: %w", round, err))
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
					"turn_id", in.RunID, "round", round,
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
			log.InfoContext(ctx, "turn_stall_aborted", "turn_id", in.RunID, "round", round)
			res.Decision = phase.Failed
			res.Breach = &Breach{
				Kind:   types.GuardStall,
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

	log.InfoContext(ctx, "turn_max_iterations_exhausted", "turn_id", in.RunID, "max", lastRound)
	res.Decision = phase.Failed
	res.Breach = &Breach{
		Kind: types.GuardMaxIter,
		Detail: fmt.Sprintf("executor/review loop exhausted at %d rounds without done",
			lastRound),
	}
	return res, nil
}

// nextRound is the number of the first round a run starts: one past the
// highest round in history, or 1 when there is none.
//
// The HIGHEST rather than the last entry's, so a ledger that holds one number
// twice still yields a number no closed round has — a ledger that crossed a
// build which numbered a resumed turn's rounds from 1 holds exactly that.
func nextRound(history []ledger.Iteration) int {
	highest := 0
	for _, it := range history {
		highest = max(highest, it.Iteration)
	}
	return highest + 1
}

// guarded runs one phase call, turning a panic inside it into a [PanicError].
//
// THE PHASE IS THE BOUNDARY because it is where the loop hands control to code
// it does not own: a provider SDK, an MCP client, every tool handler a seat can
// reach. The loop's own decisions between phases are plain arithmetic over
// values it holds. Recovered here, a panic becomes the one thing the rest of
// the engine already knows how to handle, a phase that broke, so the turn is
// closed, published and answered exactly as a broken phase's is.
//
// The stack is logged here and only here, because this is the frame that
// recovered it: [PanicError.Error] carries the value alone, and that is what
// reaches the turn's events.
func guarded(ctx context.Context, turnID, phaseName string, round int, call func() error) (err error) {
	defer func() {
		if panicked := Recovered(recover()); panicked != nil {
			log.ErrorContext(ctx, "turn_phase_panicked", "turn_id", turnID,
				"phase", phaseName, "round", round, "panic", panicked.Value,
				"stack", panicked.Stack)
			err = panicked
		}
	}()
	return call()
}

// broke returns a turn whose phase broke, naming the guard when it panicked.
//
// A panic is BOTH a breach and an error, which is the pairing
// [types.GuardUnhandledException] was declared for: the error says what broke
// and the breach names the invariant that ended the turn, so the seat goes AFK
// with a cause rather than sitting in `working` until its next turn. Any other
// broken phase is an error alone, as it always was.
func broke(res Result, err error) (Result, error) {
	var panicked *PanicError
	if errors.As(err, &panicked) {
		res.Decision = phase.Failed
		res.Breach = &Breach{Kind: types.GuardUnhandledException, Detail: panicked.Error()}
	}
	return res, err
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
