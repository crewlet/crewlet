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

	// Calls is what the phase actually invoked, engine-recorded — for the
	// executor, the calls its run made to the seat's tools over the bridge.
	Calls []ledger.Call

	// Inherited is how many of Calls, at the front, an EARLIER ATTEMPT
	// made. A resumed executor's record is its run's calls over the bridge,
	// made before the segment that collects it, because the delivery check
	// and the iteration ledger are about the whole phase. Those writes are
	// on the record and stay there; what they are not is THIS attempt's, so
	// they do not make it one a retry would repeat ([Result.Acted]): a retry
	// collects the same run and calls none of them again.
	Inherited int

	// Refs are the branches and pull requests the executor's run reported
	// delivering, and RefsElided how many more it reported than Refs lists.
	// The run's own shell, edit and git work happened in its box and is in
	// no tool log, so these and its report are what that work is judged on.
	Refs       []string
	RefsElided int

	// Suspended marks an executor whose run was launched: the work happens
	// in the run, detached in its box, and this segment of the turn ends
	// here. The run's collection resumes the turn, so the loop hands the
	// accumulated ledger back rather than treating this as an ending.
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
	// leaves the process without answering anybody — it wakes a colleague.
	KnownOpenWorld []string
	// Discoverable is every tool the phase reaches only by DISCOVERING it
	// — an MCP server's, which `list_mcp_server_tools` lists and
	// `activate_tool` promotes onto the next round — as against a
	// first-party tool, which the executor holds from its launch.
	//
	// It exists for one sentence: the one a delivery refusal ends with,
	// saying how to reach the surface the asker is waiting on (see
	// [Remedy]). That sentence used to send every seat to discovery, which
	// is right for a vendor's surface and wrong for Worklet and Doclet: a
	// seat woken by a work-item comment followed it to the only MCP server
	// it held and posted in a chat channel the check could never count.
	Discoverable []string
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

	// Resume collects the executor run the turn parked on: what it
	// submitted, the calls it made over the bridge, what it delivered.
	//
	// Called INSTEAD OF Execute for the first round of a resumed turn. A
	// resumed turn that started over would launch the paid-for run again;
	// everything after that round is an ordinary round.
	Resume(ctx context.Context, history []ledger.Iteration) (Work, Surface, error)
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

	// History is the ledger carried across a park. The resumed turn
	// re-enters mid-loop, so without this it would forget every round the
	// parked one closed and could re-fire their deliveries.
	History []ledger.Iteration

	// Resume collects the executor run the turn parked on rather than
	// starting a fresh round. The SAME turn id continues: the round being
	// collected is the one whose run was launched.
	Resume bool

	// Round is the round that executor parked in, so the resumed turn
	// carries on the budget rather than being handed a fresh one. Read
	// only when Resume is set, and zero everywhere else.
	//
	// SEPARATE FROM History, although the two arrive together, because
	// they answer different questions and only one of them is a count of
	// rounds. History is what CLOSED before the park, and the round that
	// launched the run closed nothing — [Run] returns on a park without
	// appending it — so `len(History)+1` is the parked round by
	// coincidence rather than by rule, and stops being it the moment a
	// round ends any other way. The number itself is already durable:
	// [execstate.State.Round], which the runner also stamps the collected
	// phase's own record with, so the loop and the phase events agree
	// about which round this is instead of differing by however many
	// rounds ran before the box did.
	//
	// Zero or less is read as one: the value comes off a durable row, and
	// taking a non-positive one literally would run the loop extra rounds
	// and file them under iteration zero.
	Round int
}

// firstRound is the round this turn's counter starts at.
//
// One for an ordinary turn. For a resumed one it is the round the park
// happened in — a RE-ENTRY of that round rather than the one after it, which
// is what the rest of the engine already says: [Phases.Resume] is called
// instead of Execute for that round, the round appended no ledger entry when
// it parked, and the runner stamps the collected phase's record with the same
// number. A counter that restarted at one instead re-used every iteration
// number the segments before had already spent, so the collected phase
// records collided with them on the one key a screen has for a phase.
func (in Input) firstRound() int {
	if !in.Resume || in.Round < 1 {
		return 1
	}
	return in.Round
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

	// Now is the clock the wall-clock cap reads, injectable so a test can
	// drive it without sleeping. Nil takes time.Now.
	Now func() time.Time

	// Deadline is the instant the turn's wall-clock cap ends: a scheduled
	// fire's TimeoutSeconds from the moment the turn BEGAN, in its first
	// segment. Zero is unbounded, which is every trigger but a scheduled
	// fire.
	//
	// AN INSTANT, NOT A SPAN FROM THIS CALL, because a turn is several
	// calls: every executor launch parks the turn, and the segment that
	// collects the run is a later call — often in another process, hours
	// on. A cap measured from each call's own start bounded nothing at all:
	// the launching segment ends at its first round, before the cap can be
	// read, and every resumed segment started its clock afresh. Fixed once,
	// when the turn begins, and carried across each park
	// (execstate.State.Deadline), it bounds the turn the fire started.
	//
	// Checked BETWEEN ROUNDS, never mid-phase. A phase that is running has
	// possibly already fired side effects, and abandoning it there would
	// leave a post half-written with nothing recording that it happened —
	// the same reason the drain lets a running turn finish. So the cap
	// refuses to start another round rather than interrupting one. The one
	// phase that can outlast it by much — the executor's run, detached in a
	// box — carries the same instant on its row, and the poll ends a run
	// that passes it and hands back what it did (internal/sandbox's
	// PendingRun.Deadline).
	Deadline time.Time
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

	// Iterations is the closed-round ledger. Carried out so a park can
	// persist it and the resumed turn can carry on knowing what already
	// fired.
	Iterations []ledger.Iteration

	// Rounds is the round number the turn reached.
	//
	// Which is how many executor passes ran, for a turn that started at
	// round one — and deliberately NOT that for a resumed one, which
	// re-enters the round it parked in and so counts the rounds that ran
	// before the box did. The turn is what the number describes rather
	// than this process's share of it: it is published as the completion
	// event's `iterations`, and a turn that parked at round three used to
	// close reporting one.
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

	// Suspended marks a turn parked on its executor's run.
	Suspended bool

	// Acted reports that this attempt's own record PROVES it already
	// reached outside the engine (see [Acted]): what a retry of THIS attempt
	// would repeat. A resumed attempt answers for its own calls, never for
	// those its executor's run made before the segment that collects it
	// ([Work.Inherited]).
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
	// Nor is it what the delivery GATES ask. [Check] and [OverrideDone]
	// read the whole turn, because theirs is "was the person waiting
	// reached at all" — a fact a later round cannot undo, and one the
	// prior-work ledger forbids that round to establish a second time.
	//
	// It exists for the conversation ledger, which files the turn's
	// artifact as the seat's own "You replied" and had no way to ask. On a
	// turn that delivered nothing it wrote a reply that never happened into
	// the one record the seat's NEXT turn on that thread reads back — so
	// the failure sealed itself, and a follow-up asking "did you do it?"
	// was answered against a fiction.
	Delivered bool

	// WorkedThrough are completion-ledger keys for EARLIER triggers this
	// turn answered without being woken for them: the chat messages its
	// thread block showed it that were still waiting for an answer.
	//
	// FILLED BY THE ENGINE, which assembled the block, and never by the
	// loop, which does not know what a prompt showed. The dispatch records
	// them beside the turn's own triggers when the turn completes, so a
	// message that comes round on its own afterwards — a failed delivery
	// returns behind its conversation's newer mail — is dropped as already
	// worked instead of being answered a second time, out of order.
	WorkedThrough []string
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
	// THE ROUNDS BEFORE THIS SEGMENT COUNT TOWARDS A STALL. Every executor
	// launch parks the turn, so a segment reviews at most one round before
	// the next launch parks it again: a detector that saw only its own
	// segment's rounds could never see two, and a turn re-deriving the same
	// artifact on every round ran to its round budget instead. Seeded from
	// the closed rounds, in order, the detector sees the sequence one
	// uninterrupted run of the loop would have — a closed round is exactly
	// one the loop observed on its way to closing it.
	stall := StallDetector{}
	for _, closed := range in.History {
		if closed.ArtifactHash != "" {
			stall.observeHash(closed.ArtifactHash)
		}
	}
	notes := ""
	maxRounds := set.iterations()

	// WHERE THE BUDGET PICKS UP. A turn that parked on its executor's run
	// re-enters the round it parked in, so the counter starts there rather
	// than at one: starting at one handed every resumed turn a whole fresh
	// MaxIterations, and a turn that parked at round three of three came
	// back from the box with three more executor→reviewer rounds than the
	// company had configured. It also re-used iteration numbers the earlier
	// segments had already published phase records under.
	first := in.firstRound()
	if maxRounds < first {
		// THE RE-ENTERED ROUND ALWAYS RUNS, for the reason the first round
		// always outruns the wall-clock cap below: it is not a new round
		// but the second half of one whose executor run already ran and is
		// waiting to be collected. Refusing it would throw away a run the
		// company has already paid for and judge nothing it did. The case
		// is real rather than theoretical — MaxIterations is founder-owned
		// and edited live, and a detached run outlives an edit — so the
		// budget bounds the rounds a turn may START and the parked one
		// already did.
		maxRounds = first
	}

	started := set.now()
	resuming := in.Resume
	for round := first; round <= maxRounds; round++ {
		// The wall-clock cap, at a ROUND BOUNDARY. THIS SEGMENT's first round
		// always runs — which is `first` rather than one: on a resumed turn
		// it is the second half of a round whose run already ran, and
		// refusing it would throw away a run the company has already paid
		// for and judge nothing it did; on a fresh turn, a cap that refused
		// before any work started would report a turn as timed out having
		// done nothing, which is a misconfiguration rather than a slow turn
		// and reads better as one. Every round after it is refused once the
		// TURN's deadline has passed, however little of it this segment
		// spent.
		if now := set.now(); round > first && !set.Deadline.IsZero() && !now.Before(set.Deadline) {
			res.Breach = &Breach{
				Kind: types.GuardScheduledTimeout,
				Detail: fmt.Sprintf(
					"the turn passed its wall-clock cap at %s, %s ago, after this segment "+
						"ran %s across %d round(s), so no further round was started",
					set.Deadline.UTC().Format(time.RFC3339), now.Sub(set.Deadline).Round(time.Second),
					now.Sub(started).Round(time.Second), round-first),
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
			// The first round of a resumed turn collects the parked run,
			// and only this round: if the reviewer sends the work back,
			// the next round launches a run of its own.
			resuming, entered = false, "resume"
			err = guarded(ctx, in.RunID, entered, round, func() (callErr error) {
				work, surface, callErr = ph.Resume(ctx, res.Iterations)
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
		// must not un-say what round one posted.
		//
		// THIS ATTEMPT'S CALLS ONLY. A resumed executor's record opens
		// with its run's calls over the bridge, made before the segment
		// that collects it ([Work.Inherited]), and a retry of the resume
		// does not make those again: it collects the same run. Counted,
		// every resume that broke before writing anything new (a model at
		// its usage limit, the reviewer's or the relaunch's, a runner that
		// could not be built) was settled for good as a turn that had
		// written, and the coding run's result never reached the seat.
		res.Acted = res.Acted || Acted(work.Calls[min(work.Inherited, len(work.Calls)):], surface)
		// ASSIGNED, never accumulated — the opposite of the line above, and
		// see [Result.Delivered] for why the two questions differ.
		res.Delivered = Answered(work.Calls, surface, in.Reply)
		if err != nil {
			return broke(res, fmt.Errorf("turn: %s round %d: %w", entered, round, err))
		}

		if work.Suspended {
			// The turn parks here and the run's collection resumes it. The
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
		//
		// Against the TURN's record, closed rounds included: see [Check]
		// for why the obligation is the turn's and not the round's.
		verdict := Check(work, res.Iterations, in.Reply, surface)
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
			if override, correction := OverrideDone(work, res.Iterations, in.Reply, surface); override {
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
			// across a park, and carrying every read-only tool
			// on a large MCP surface makes it grow with the catalogue
			// rather than with what the round did.
			Reads:         calledReads(work.Calls, surface.KnownReads),
			Text:          work.Text,
			ReviewNotes:   notes,
			CompletedWork: rev.CompletedWork,
			ArtifactHash:  ArtifactHash(artifact),
		})
	}

	log.InfoContext(ctx, "turn_max_iterations_exhausted", "turn_id", in.RunID, "max", maxRounds)
	res.Decision = phase.Failed
	res.Breach = &Breach{
		Kind: types.GuardMaxIter,
		Detail: fmt.Sprintf("executor/review loop exhausted at %d rounds without done",
			maxRounds),
	}
	return res, nil
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
// and the breach names the invariant that ended the turn, so the seat carries
// the failure as its `last_error` with a cause rather than sitting in `working`
// until its next turn. Any other
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
