package sandbox

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/words"
)

// CoordStore is the pending-run store, on the FLEET's coordination store.
//
// A node's own database is no option: a detached run OUTLIVES its turn and
// sometimes its node, and the next seat owner's recovery must find it (a
// per-node store would leave a box running for nobody). A
// single-node company runs the in-memory twin of the same certified contract.
//
// Every mutation is a CONDITIONAL FLIP on the run's own fields, a DECISION handed
// to the store's read-decide-write ([coord.SandboxRuns.MutateSandboxRun]) so the
// SECOND writer sees the first's. A lost race is the store's to retry, never a
// count of attempts here (up to twenty subagents write one record): the bound is
// internal/coord/kv's casRetry, then [coord.ErrUnavailable].
type CoordStore struct {
	runs coord.SandboxRuns
	now  func() time.Time
	// undecodable reports the records a listing skips; nil is the
	// process's own ([undecodableRuns]).
	undecodable *undecodableReport
}

// NewCoordStore wraps the fleet's run records.
func NewCoordStore(runs coord.SandboxRuns) *CoordStore {
	return &CoordStore{runs: runs}
}

var _ PendingStore = (*CoordStore)(nil)

// RowCrossing is how long a change of a run's value at its bound takes to
// cross this node's connection to the coordination store, its read and every
// send of its write — see [PendingStore.RowCrossing].
func (s *CoordStore) RowCrossing() time.Duration { return s.runs.ValueCrossing() }

// RowReadCrossing is how long the store's reply to a read of a run's value at
// its bound takes to cross this node's connection to it
// ([PendingStore.RowReadCrossing]).
func (s *CoordStore) RowReadCrossing() time.Duration { return s.runs.ValueReadCrossing() }

// WithClock pins the clock, for a suite that asserts about ages.
func (s *CoordStore) WithClock(now func() time.Time) *CoordStore {
	s.now = now
	return s
}

func (s *CoordStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// launchRounds bounds [CoordStore.BeginLaunch]'s create-or-reset loop.
//
// A round repeats only when the row VANISHED between the create that found it
// and the reset that read it (the turn's previous run finishing), and the next
// create then wins unless a racing launch of the same turn created it first,
// which the next reset takes. A row vanishing after the launch's own write
// landed never repeats: it names this launch, and only its ending removes it
// ([ErrRunEnding]). One round is ordinary, two met a finishing run, three also
// a racing launch; a fourth needs another vanish inside the window, which
// nothing here does repeatedly (the reasoning of internal/coord/kv's
// createRounds). Exhaustion is an error, never a launch.
const launchRounds = 3

// BeginLaunch opens a launch on this turn's row. See the contract on
// [PendingStore].
//
// A ROW THAT VANISHES BETWEEN THE CREATE AND THE RESET is tried again: the
// previous run finishing ends the row under the reset, and a reset that found
// nothing would report a launch open on a row that does not exist.
//
// A ROW THAT VANISHES ONCE THE LAUNCH'S OWN WRITE LANDED is never created
// again: it names this launch, so what removed it is an ending decided for
// this job (a person's stop) and its finish. The launch is refused
// [ErrRunEnding]. Recreated, the row would carry no ending and start a box
// for a stopped turn.
func (s *CoordStore) BeginLaunch(ctx context.Context, run PendingRun, fence Fence) (_ PendingRun, err error) {
	defer origin.Stamp(&err)
	if run.TurnID == "" {
		return PendingRun{}, fmt.Errorf("sandbox: a pending run needs a turn id")
	}
	now := s.clock()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	// THE LAUNCH FLOOR IS THE FLOOR IT OPENS AT, never the caller's: what the
	// run pushed is read above it, so any other value hides or miscounts a push.
	run.CodeLaunch = run.CodeFloor
	// THE TURN'S CODE HOLDS, each once within the row's bound; a reset unions
	// them ([CoordStore.resetLaunch]).
	holds, fits := unionHolds(nil, run.CodeHolds)
	if !fits {
		return PendingRun{}, fmt.Errorf("sandbox: begin launch %s: %d holds: %w", run.TurnID,
			len(holds), ErrCodeHoldsFull)
	}
	run.CodeHolds = holds
	// Never the caller's: a row to launch into is launching.
	run.Status = StatusLaunching
	// OWNED BY THE LEASE THAT LAUNCHED IT — see [PendingStore.BeginLaunch].
	if fence.Fenced() {
		run.Owner, run.OwnerEpoch = fence.Owner, fence.Epoch
	}
	// Nor the job's name, new on every launch: a completion claims only the
	// job it names.
	run.LaunchID = uuid.NewString()
	// NOR ITS ORDINAL: the first is 1, a reset numbers later ones past the
	// row's ([PendingRun.LaunchSeq]).
	run.LaunchSeq = 1
	run.UpdatedAt = now
	// The job's own record starts here; a previous job's is not this one's.
	// Only the model, entry (its key and its identity), sign-in, follow-up
	// reason and round cap are the caller's.
	run.Launch = LaunchRecord{StartedAt: now, Model: run.Launch.Model,
		ProviderKey: run.Launch.ProviderKey, SignInID: run.Launch.SignInID, Entry: run.Launch.Entry,
		FollowUp: run.Launch.FollowUp, TurnCap: max(run.Launch.TurnCap, 0)}
	// THE TURN'S NOTES START HERE: the sealed seat anchor, the brief's notes
	// (read by this job), then the notes the box still held, waiting — see
	// [PendingStore.BeginLaunch]. Nothing else of the caller's.
	created := run
	// A CREATED ROW OPENS A ROUND named by its first job ([PendingRun.Round]).
	created.Round, created.AnsweredJob = run.LaunchID, ""
	// A CREATED ROW NAMES ITS RUN FOR ITS LEADS by its job ([PendingRun.LeadsRun]),
	// never the caller's nor the count: no earlier lead in the turn's leads part
	// is this run's. The create writes NO part; readers tell another run's part
	// by the name.
	created.LeadsRun, created.LeadsAppended = run.LaunchID, 0
	// NOR WHAT ITS CALLS WROTE, which only its log's appends fold in
	// ([PendingRun.WroteKeys]).
	created.WroteKeys = nil
	created.Steer = RunSteer{Seat: bytes.Clone(run.Steer.Seat)}
	openNotes(&created.Steer, run, run.LaunchID, now)
	// A TRIGGER'S WORDS, HELD TO THEIR SHARE OF THE ROW ([MaxRunTitleBytes]):
	// the run's two names and the asking colleague's question, which nothing
	// else bounds below the broker's payload. Here because identity fields are
	// never rewritten: a relaunch keeps the row's.
	created.TaskDescription, created.Label = keptTitle(run.TaskDescription), keptTitle(run.Label)
	if ask := run.ColleagueAsk; ask != nil {
		held := ask.WithQuestion(keptQuestion(ask.question))
		created.ColleagueAsk = &held
	}
	raw, err := encodeRun(created)
	if err != nil {
		return PendingRun{}, err
	}
	standing := standingOf(created)
	for range launchRounds {
		made, err := s.runs.CreateSandboxRun(ctx, run.TurnID, raw, &standing)
		if err != nil {
			// A CREATE THE STORE DID NOT ANSWER may have landed, naming the job.
			return PendingRun{}, unopened(run.LaunchID,
				fmt.Errorf("sandbox: create run %s: %w", run.TurnID, err))
		}
		if made {
			// AND THE JOB'S LOG, new: a prior key's content is no call of this run's.
			last, _, opened, openErr := s.openLog(ctx, run.TurnID, run.LaunchID,
				logOpening{round: created.Round})
			if openErr != nil {
				return PendingRun{}, unopened(run.LaunchID, openErr)
			}
			if !opened {
				return PendingRun{}, endedUnderLaunch(run.TurnID)
			}
			created.OpenedAfter = last
			return created, nil
		}
		opened, reset, err := s.resetLaunch(ctx, run, fence)
		if err != nil {
			return PendingRun{}, err
		}
		if reset {
			return opened, nil
		}
		// The row went between the create and the reset: create it again.
	}
	return PendingRun{}, fmt.Errorf(
		"sandbox: begin launch %s: the record kept appearing and vanishing under the launch",
		run.TurnID)
}

// resetLaunch is [CoordStore.BeginLaunch]'s half for a row already there, a
// relaunch its turn's resumed frame makes, reporting the row as it reset it and
// whether it did; a kick-off delivered again has a turn id and row of its own
// (ADR-0017). Only launch-scoped state resets: identity, [PendingRun.Base] and
// the box reference stay until a box is attached, handed to the launch as
// [PendingRun.Prior]; the previous job's command and session go. A reset the
// row refuses ([ErrSeatNotHeld], [ErrRunEnding]) is never a launch that went
// ahead. See the package doc, "Launching a run".
func (s *CoordStore) resetLaunch(ctx context.Context, run PendingRun, fence Fence) (PendingRun, bool, error) {
	full := false
	var (
		prior   PriorBox
		opening logOpening
	)
	// WHAT THE BOX WAS HANDED is kept beside the row ([CoordStore.Handed]):
	// read before the reset, taken inside it only where it names the job being
	// replaced. Any other set carries nothing, so the launch refuses its job's
	// captures rather than scrub less than its box holds
	// ([Coordinator.sealHanded]); never another job's set as the box's.
	handed, err := s.readHanded(ctx, run.TurnID)
	if err != nil {
		return PendingRun{}, false, fmt.Errorf("sandbox: begin launch %s: %w", run.TurnID, err)
	}
	opened, reset, err := s.mutateFenced(ctx, run.TurnID, fence, func(existing *PendingRun) bool {
		// READ inside the same compare-and-swap, so the box handed to the
		// launch is exactly the one the write replaced.
		prior = PriorBox{
			SandboxID: existing.SandboxID, Placement: existing.Placement,
			Harness: existing.Harness, Status: existing.Status,
			LaunchNode: existing.LaunchNode, Since: existing.BoxSince,
			Handed: handed.of(existing.LaunchID), Entry: existing.Launch.Entry,
			// What the box's copy carried, as the replaced record measured it
			// ([PriorBox.Context]), and where it restarts last, with the release
			// that wrote it ([PriorBox.Restart], [PriorBox.Writer]).
			Context: existing.Launch.Context,
			Restart: restartOf(existing.Launch.ResumedCount), Writer: existing.Launch.CountRelease,
			// And the wakes that job was handed and never read, which this
			// write removes ([openNotes]) for the launch to report.
			Wakes: unreadWakes(existing.Steer.Notes),
		}
		// THE TURN'S CODE HOLDS ARE UNIONED, never reset: the turn may still
		// depend on what an earlier job named, and once parked this row is
		// the only place Gitlet's expiry reads them.
		holds, fits := unionHolds(existing.CodeHolds, run.CodeHolds)
		if !fits {
			full = true
			return false
		}
		existing.CodeHolds = holds
		// THE ROUND AND THE ANSWER ITS LOG IS TO BE CUT FOR are decided in
		// the reset's own write, so the opening, or the next relaunch's if
		// it never lands, keeps what the round made ([PendingRun.Round]).
		// Read before the question and held call are cleared below.
		heldRound := existing.Round
		switch {
		case !run.Continues:
			existing.Round, existing.AnsweredJob = run.LaunchID, ""
			existing.RoundRefusals = nil
		case existing.asked():
			existing.AnsweredJob = existing.LaunchID
			// A REFUSAL THE ROUND CARRIES ON FROM is kept for the round's
			// review before it is cleared below; the reviewer reads calls,
			// where the question asked again looks sent twice
			// ([PendingRun.RoundRefusals]).
			if refusal, refused := existing.Refused(); refused {
				existing.RoundRefusals = withRoundRefusal(existing.RoundRefusals, existing.LaunchID, refusal)
			}
		}
		// THE CALL A PERSON APPROVED IN THE REPLACED JOB, cleared with its held
		// call below, is carried to the opening in this write so a log its own
		// append never reached still takes it ([OwedCall]); an earlier reset's
		// owed call stays only while its job never launched.
		owed := existing.OwedCall
		if existing.Status != StatusLaunching {
			owed = nil
		}
		if h := existing.HeldCall; h != nil && h.Made() && h.Call != nil {
			owed = &OwedCall{Round: heldRound, Call: *h.Call}
		}
		existing.OwedCall = owed
		opening = logOpening{round: existing.Round, answered: existing.AnsweredJob, owed: owed,
			told: run.BriefRead}
		existing.Status = StatusLaunching
		existing.LaunchID = run.LaunchID
		// ONE PAST THE ROW'S, in the write that names the job, so ordinal and
		// name are never apart.
		existing.LaunchSeq++
		// THE JOB'S VERDICT, folded into the turn's before its record goes.
		existing.NotContinued = existing.turnVerdicts()
		existing.Launch = run.Launch
		// The turn's cap is the same every launch; written so the row says what
		// this launch was told.
		existing.Deadline = run.Deadline
		// THE JOB'S GRANTS AND FLOORS ARE ITS OWN, replaced: a still-pushing box
		// of the previous job is refused by the launch id it was minted for.
		// Unlike the holds above, which the turn keeps. The colleague it works
		// for is the one its own resolution read.
		existing.Repos = run.Repos
		existing.CodeFloor, existing.CodeLaunch = run.CodeFloor, run.CodeLaunch
		existing.CodeCeiling, existing.CodeWorkingFor = run.CodeCeiling, run.CodeWorkingFor
		// THE BOX'S HOST STAYS WITH THE BOX: the row still names the previous
		// job's box, on the node that launched it; the attach naming this job's
		// box names this node. A row with no box names this job's node.
		if existing.SandboxID == "" {
			existing.LaunchNode = run.LaunchNode
		}
		// A LOSS IS COUNTED BY THE ROUND IT CARRIES ON; the verdict itself was
		// the previous job's. A continuing round adds that job if lost, a new
		// round has lost nothing ([PendingRun.RoundLosses]).
		switch {
		case !run.Continues:
			existing.RoundLosses = 0
		case existing.Lost != nil:
			existing.RoundLosses++
		}
		existing.Lost = nil
		// THE PREVIOUS JOB'S COMMAND AND SESSION ARE NOT THIS JOB'S, nor what its
		// box was handed: the set beside the row names its job, so it reads as
		// none here, and the attach writes its own, carrying the old forward
		// when continuing in the same box ([PriorBox.Handed]).
		existing.CommandID, existing.SessionID = "", ""
		// A hold was on the previous job's resume, which is over.
		existing.HeldFor, existing.HeldUntil = "", time.Time{}
		if fence.Fenced() {
			existing.Owner, existing.OwnerEpoch = fence.Owner, fence.Epoch
		}
		// The previous suspension is worse than absent: a completion claimed
		// before the new one lands would resume the conversation the LAST call
		// suspended, splicing this run's findings into a loop that moved on.
		existing.ExecuteState = nil
		// Its question is answered or never asked; a reply now is the new job's.
		existing.Question, existing.Audience = "", ""
		existing.QuestionContext, existing.Decision = "", nil
		// The held call and its audience are the old job's, and so are its
		// fold-ins: an answer to one would let a request join work it was
		// never asked for.
		existing.HeldCall, existing.WaitsFor, existing.ParkedCalls, existing.WaitOver = nil, nil, nil, nil
		existing.FoldIns = nil
		existing.AudienceHandles, existing.AudienceFallback = nil, false
		// EVERYTHING ITS ANSWER WAS MEASURED AGAINST: the next question is
		// anchored on its own asking, and a reply one question declined may be
		// what the next waits for.
		existing.AskedAt, existing.Answer, existing.DeclinedAnswers = time.Time{}, nil, nil
		existing.AnswerRefusal = nil
		// ITS COST: a parked job's tokens are paid by the resume its answer
		// drives, done by the time a new job opens.
		existing.ParkedInputTokens, existing.ParkedOutputTokens = 0, 0
		existing.ParkedCacheReadTokens, existing.ParkedCacheWriteTokens = 0, 0
		// Its tool calls are the new job's only if the round carries on: decided
		// by the write that opens the log, which folds a replaced round's calls
		// into the turn's earlier ones ([CoordStore.openLog], [BridgeLog.PriorCalls]).
		// ITS CHARGE IS NOT THIS JOB'S: carried over, the completion would see
		// its spend as counted and the company would never be billed.
		existing.Charged, existing.CompanyCharged = false, false
		// THE TURN'S NOTES ARE THE TURN'S: every note, its numbering and the seat
		// anchor (written only where none) stay. The job's is whether its harness
		// asked for them ([RunSteer.Pulled]). The brief's notes are read BY THIS
		// JOB in the write that names it, so its harness never takes them again,
		// then the box's are filed. An item for the job before alone
		// ([RunNote.For]) goes, but a wake item read, which is the turn's:
		// [openNotes] removes every other one first.
		existing.Steer.Notes = slices.Clone(existing.Steer.Notes)
		if len(existing.Steer.Seat) == 0 {
			existing.Steer.Seat = bytes.Clone(run.Steer.Seat)
		}
		existing.Steer.Pulled = ""
		// TELLINGS ARE THE ROUND'S: a new round numbers its calls afresh
		// ([RunSteer.Tellings]).
		if !run.Continues {
			existing.Steer.Tellings = nil
		}
		openNotes(&existing.Steer, run, run.LaunchID, s.clock())
		return true
	})
	switch {
	case errors.Is(err, ErrSeatNotHeld), errors.Is(err, ErrRunEnding):
		return PendingRun{}, false, fmt.Errorf("sandbox: relaunch run %s: %w", run.TurnID, err)
	case err != nil:
		// A RESET THE STORE DID NOT ANSWER may have landed, naming the job.
		return PendingRun{}, false, unopened(run.LaunchID,
			fmt.Errorf("sandbox: relaunch run %s: %w", run.TurnID, err))
	case full:
		return PendingRun{}, false, fmt.Errorf("sandbox: relaunch run %s: %w", run.TurnID,
			ErrCodeHoldsFull)
	}
	if !reset {
		return opened, false, nil
	}
	opened.Prior = prior
	// THEN THE LOG IS OPENED TO THE JOB, never before: a refused reset must
	// leave the replaced job's log as it was. Between the two writes the log
	// takes the replaced job's late calls as that job's own (the round's when
	// carried on, counted before the answer's cut; dropped otherwise); after
	// the opening it lists one marked late ([BridgeLog.take]).
	last, walked, logged, err := s.openLog(ctx, run.TurnID, opened.LaunchID, opening)
	switch {
	case err != nil:
		return PendingRun{}, false, unopened(opened.LaunchID, err)
	case !logged:
		return PendingRun{}, false, endedUnderLaunch(run.TurnID)
	}
	// EVERY CALL THE OPENING WALKED is handed to the new job in a write after
	// it: the reset removed the old job's items, and only the opening decides
	// which calls are late. Never the launch's failure ([CoordStore.fileLateItems]).
	s.fileLateItems(ctx, run.TurnID, opened.LaunchID, opening.round, walked)
	opened.OpenedAfter = last
	return opened, true, nil
}

// endedUnderLaunch is [CoordStore.BeginLaunch]'s refusal of a launch whose
// own row was removed before its log was opened to it.
func endedUnderLaunch(turnID string) error {
	return fmt.Errorf("sandbox: begin launch %s: the run ended under its launch: %w",
		turnID, ErrRunEnding)
}

// UnopenedLaunchError is [PendingStore.BeginLaunch]'s failure past a write
// the store did not answer (create, reset or log opening) that may have named
// its job on the row. The row may then name LaunchID with no log, a launching
// run nothing polls or claims, so the launch ends that one job
// ([Coordinator.Launch]); a row that does not name it stays whoever's it is.
type UnopenedLaunchError struct {
	LaunchID string
	Err      error
}

func (e *UnopenedLaunchError) Error() string { return e.Err.Error() }

func (e *UnopenedLaunchError) Unwrap() error { return e.Err }

// unopened is err from a write that may have named job on the row.
func unopened(job string, err error) error {
	return &UnopenedLaunchError{LaunchID: job, Err: err}
}

// logOpening is what the write that opens a job's log keeps of the round
// before it, as the row's create or reset decided it — see
// [CoordStore.openLog].
type logOpening struct {
	// round is the round the job belongs to ([PendingRun.Round]).
	round string

	// answered is the job whose answered question the round is cut for
	// ([PendingRun.AnsweredJob]); empty for a round no answer carried on.
	answered string

	// owed is the call a person approved that its round's log is owed
	// ([PendingRun.OwedCall]), nil for none: appended in the opening's own
	// write where the log lacks it, before the opening decides what the job
	// keeps.
	owed *OwedCall

	// told is the read of the round's log the relaunch's briefs were built
	// from ([PendingRun.BriefRead]), nil for none known: applied only to a log
	// still open to the job it read ([logOpening.reads]). After a reset whose
	// opening never landed the caller holds the empty answer for a log on
	// another job, which no opening applies: unread calls are folded by name
	// alone, absorbed calls carried nowhere, or, if the round carries on, left
	// unmarked.
	told *LogRead
}

// reads reports whether the opening o applies the briefs' read to l: known and
// of the job l is open to, so no opening has moved the log since (each moves it
// to its own job). Applying the empty answer for another job's log would carry
// or mark a whole round an earlier read told.
func (o logOpening) reads(l BridgeLog) bool { return o.told != nil && o.told.Launch == l.Launch }

// opened is l as the opening o of job launch leaves it ([CoordStore.openLog]'s
// one write) and the calls o walked past the briefs' read: of a round carried
// on, every call taken after the read ([BridgeLog.lateAfter]); into a new
// round, the calls it carried ([BridgeLog.carried]). Never a person-approved
// call, which the relaunch's lead tells.
func (l BridgeLog) opened(launch string, o logOpening) (BridgeLog, []BridgeCall) {
	if o.owed != nil {
		l, _ = l.withApproved(o.owed.Round, o.owed.Call)
	}
	read := o.reads(l)
	if l.Round != o.round {
		next := BridgeLog{Launch: launch, Round: o.round, Committed: slices.Clone(l.Committed),
			Wrote: slices.Clone(l.Wrote), WroteMore: l.WroteMore}
		next.PriorCalls, next.PriorCallsElided = l.priorFor(o.round)
		if !read {
			return next, nil
		}
		return l.carried(next, *o.told)
	}
	next := l
	next.Launch = launch
	if o.answered != "" && l.CutFor != o.answered {
		next.AnsweredThrough, next.CutFor = l.LastSeq(), o.answered
	}
	if !read {
		return next, nil
	}
	next = next.lateAfter(o.told.Took)
	var walked []BridgeCall
	for _, call := range next.Calls {
		if call.Appended > o.told.Took && !call.Approved {
			walked = append(walked, call)
		}
	}
	return next, walked
}

// carried is next, a new round's log, with what l, the round it replaces, took
// after read: every call l lists past it but a person-approved one (the
// collection read it, [BridgeLog.withHeld]) appended late, with no place and
// Abandoned kept, and the calls l absorbed past it ([unreadAbsorbed]). Answers
// the carried calls, which the opening walked. See the package doc, "A call
// that outlives its round's collection is a late call of the round after".
func (l BridgeLog) carried(next BridgeLog, read LogRead) (BridgeLog, []BridgeCall) {
	next.AbsorbedCalls = unreadAbsorbed(l.Absorbed(), read.Absorbed)
	for _, call := range l.Calls {
		if call.Appended <= read.Took || call.Approved {
			continue
		}
		call.Seq, call.Late = 0, true
		next.appendCall(call, nil)
	}
	return next, slices.Clone(next.Calls)
}

// unreadAbsorbed is what of all, a round's absorbed tally, read did not hold:
// each identity's count less the read's, never below zero. One folded into its
// tool's entry or the unnamed one after the read keeps its whole count, an
// over-count, the safe direction ([ElidedCall]). Never the whole tally: the
// collection seeded the read's part into the turn's call log.
func unreadAbsorbed(all, read []ElidedCall) []ElidedCall {
	var out []ElidedCall
	for _, e := range all {
		if i := slices.IndexFunc(read, func(r ElidedCall) bool {
			return r.Name == e.Name && r.Digest == e.Digest
		}); i >= 0 {
			e.Calls -= read[i].Calls
		}
		if e.Calls > 0 {
			out = append(out, e)
		}
	}
	return out
}

// lateAfter is l with every call it took after told marked late
// ([BridgeCall.Late]), but a person-approved one, and every such call it no
// longer lists counted late ([BridgeLog.LateElided]): what an opening carrying
// the round on makes of calls after the read its briefs were built from
// ([PendingRun.BriefRead]).
//
// THE SAME MARK AS A CALL TAKEN AFTER THE OPENING ([BridgeLog.take]): such a
// call belongs to the replaced job (a running worker, a call its harness gave
// up on) and landed after the briefs, never told it. Unmarked, a later capture
// counts it among its session's calls and no continuation is told it. The list
// is its own, since l is a copy. A call with no number (an older build's
// append) is counted among the dropped: an over-count, the safe direction.
func (l BridgeLog) lateAfter(told int) BridgeLog {
	l.Calls = slices.Clone(l.Calls)
	listed := 0
	for i, call := range l.Calls {
		if call.Appended <= told {
			continue
		}
		listed++
		if !call.Approved {
			l.Calls[i].Late = true
		}
	}
	l.LateElided += max(l.Appends()-told-listed, 0)
	return l
}

// openLog opens turnID's log to job launch, answering the place of the last
// call kept from before the job ([BridgeLog.LastSeq]), the calls walked
// ([BridgeLog.opened]) and whether it was written (false: the record is gone).
//
// ONE COMPARE-AND-SWAP decides what the job keeps, so no append of the replaced
// job lands between. A NEW ROUND STARTS THE LOG AFRESH of what its collection
// read, so a second executor round (a reviewer's self_iterate) never replays the
// FIRST round's submit_work; a job that CONTINUES the round keeps its calls
// ([PendingRun.Continues]). The round ([PendingRun.Round], [PendingRun.AnsweredJob]),
// not the job before, decides, so a failed opening leaves the log for the next
// relaunch. A replaced round is FOLDED INTO THE TURN'S EARLIER CALLS IN THE SAME
// WRITE ([BridgeLog.PriorCalls]); the owed approved call ([OwedCall]) goes first.
func (s *CoordStore) openLog(ctx context.Context, turnID, launch string, o logOpening) (int, []BridgeCall, bool,
	error) {
	last := 0
	var walked []BridgeCall
	_, opened, err := s.runs.MutateSandboxRunPart(ctx, turnID, coord.RunLog, func(current coord.Record, found bool) ([]byte, error) {
		next := BridgeLog{Launch: launch, Round: o.round}
		walked = nil
		if found {
			var held BridgeLog
			if err := json.Unmarshal(current.Value, &held); err != nil {
				return nil, fmt.Errorf("sandbox: decode the log of run %s: %w", turnID, err)
			}
			next, walked = held.opened(launch, o)
		}
		last = next.LastSeq()
		return json.Marshal(next)
	})
	if err != nil {
		return 0, nil, false, fmt.Errorf("sandbox: open the log of run %s to its job: %w", turnID, err)
	}
	return last, walked, opened, nil
}

// unionHolds is held with keys added — each key once, in the order first
// taken — and whether the union fits [MaxCodeHolds].
func unionHolds(held, keys []string) ([]string, bool) {
	out := make([]string, 0, len(held)+len(keys))
	seen := make(map[string]bool, len(held)+len(keys))
	for _, list := range [][]string{held, keys} {
		for _, key := range list {
			if !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	return out, len(out) <= MaxCodeHolds
}

// AddCodeHolds adds keys to a live run's code holds. See the contract on
// [PendingStore].
func (s *CoordStore) AddCodeHolds(ctx context.Context, turnID string, keys []string) (bool, error) {
	if len(keys) == 0 || slices.Contains(keys, "") {
		return false, fmt.Errorf("sandbox: a hold on run %s names no key", turnID)
	}
	full, already := false, false
	_, wrote, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		holds, fits := unionHolds(run.CodeHolds, keys)
		switch {
		case !fits:
			full = true
			return false
		case len(holds) == len(run.CodeHolds):
			// EVERY KEY ALREADY HELD: the row says it, and nothing is
			// written.
			already = true
			return false
		}
		run.CodeHolds = holds
		return true
	})
	switch {
	case err != nil:
		return false, fmt.Errorf("sandbox: hold %d keys on run %s: %w", len(keys), turnID, err)
	case full:
		return false, fmt.Errorf("sandbox: hold %d keys on run %s: %w", len(keys), turnID, ErrCodeHoldsFull)
	}
	return wrote || already, nil
}

// RaiseCodeFloor raises a live run's code floor while the row holds launch.
// See the contract on [PendingStore].
func (s *CoordStore) RaiseCodeFloor(ctx context.Context, turnID, launchID string,
	raise func(current string) (string, bool),
) (bool, error) {
	if launchID == "" {
		return false, fmt.Errorf("sandbox: a floor raise on run %s names no launch", turnID)
	}
	_, wrote, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launchID {
			return false
		}
		next, ok := raise(run.CodeFloor)
		if !ok || next == "" || next == run.CodeFloor {
			return false
		}
		run.CodeFloor = next
		return true
	})
	if err != nil {
		return false, fmt.Errorf("sandbox: raise the code floor of run %s: %w", turnID, err)
	}
	return wrote, nil
}

// Get returns one run by turn id.
func (s *CoordStore) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	run, _, found, err := s.read(ctx, turnID)
	return run, found, err
}

// StampCapture records one capture of the turn's session on its row. See the
// contract on [PendingStore].
//
// THROUGH THE WRITE EVERY STATE TAKES ([CoordStore.mutateAny]), as a bridged
// call's append is: a capture is a fact about what left the box, and a stop's
// is stamped once its ending is decided.
//
// A STAMP THE ROW ALREADY HOLDS IS ANSWERED TAKEN, writing nothing, so an
// asker whose earlier ask was applied but never answered learns the place is
// its own.
func (s *CoordStore) StampCapture(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	return s.stampCapture(ctx, turnID, capture, nil)
}

// SettleBegun records a settle of the turn's newest capture over the begun
// stamp it was decided from. See the contract on [PendingStore].
//
// THE SAME WRITE AS [CoordStore.StampCapture], its condition decided on the row
// as it stands ([stillBegun]), never the copy the settle was decided from,
// which a taker's stamp landing between would leave stale.
func (s *CoordStore) SettleBegun(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	return s.stampCapture(ctx, turnID, capture, stillBegun)
}

// stillBegun reports whether run's newest capture, or its save under way
// ([PendingRun.Saving]), is capture's own take, still begun: the stamp a settle
// may be written over ([PendingStore.SettleBegun]). A save a final take
// overtook is settled in its own section.
func stillBegun(run PendingRun, capture SessionCapture) bool {
	begun := func(section *SessionCapture) bool {
		return section != nil && capture.sameTake(*section) && section.State == CaptureBegun
	}
	return begun(run.Capture) || begun(run.Saving)
}

// ReserveSave reserves a save's place on the turn's row. See the contract on
// [PendingStore].
//
// ONE READ-DECIDE-WRITE ([CoordStore.writeFound]), fenced by nothing: any node
// may take a save's place, licensed by the row's state ([licensesSave]) on the
// writing node's clock. Taken, the save holds the newest place by
// [stampOnto] with its save under way beside it.
func (s *CoordStore) ReserveSave(ctx context.Context, turnID string, save SessionCapture) (bool, error) {
	if err := reservable(turnID, save); err != nil {
		return false, err
	}
	// SET ON EVERY ASK OF decide, as a stamp's is ([CoordStore.stampCapture]).
	var held bool
	_, won, found, err := s.writeFound(ctx, turnID, false, Fence{}, func(run *PendingRun) bool {
		held = holdsSave(*run, save)
		if held || licensesSave(*run, save, s.clock()) != "" {
			return false
		}
		stampOnto(run, save)
		reserved := save
		run.Saving = &reserved
		return true
	})
	if err != nil {
		return false, err
	}
	return won || (found && held), nil
}

// StandBy records capture's take as the read standing by for the row's newest
// capture. See the contract on [PendingStore].
//
// THE SAME READ-DECIDE-WRITE AS EVERY STAMP ([CoordStore.writeFound]): two
// reads finding one place taken each ask, and the store lets one stand.
func (s *CoordStore) StandBy(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	if err := stampable(turnID, capture); err != nil {
		return false, err
	}
	var held bool
	_, won, found, err := s.writeFound(ctx, turnID, false, Fence{}, func(run *PendingRun) bool {
		newest := run.Capture
		held = newest != nil && newest.standsBy(capture)
		if held || newest == nil || !newest.samePlace(capture) || newest.Launch != capture.Launch ||
			capture.sameTake(*newest) || newest.State == CaptureLanded || !newest.StandbyAt.IsZero() {
			return false
		}
		stood := *newest
		stood.StandbyTaker, stood.StandbyTakerEpoch, stood.StandbyAt = capture.Taker, capture.TakerEpoch, capture.At
		run.Capture = &stood
		return true
	})
	if err != nil {
		return false, err
	}
	return won || (found && held), nil
}

// TakeOver stamps capture in the place after the capture standby stood by for.
// See the contract on [PendingStore].
//
// THE SAME WRITE AS [CoordStore.StampCapture], its condition decided on the
// row as it stands ([stoodByUnlanded]).
func (s *CoordStore) TakeOver(ctx context.Context, turnID string, standby, capture SessionCapture) (bool, error) {
	return s.stampCapture(ctx, turnID, capture, func(run PendingRun, _ SessionCapture) bool {
		return stoodByUnlanded(run, standby)
	})
}

// stoodByUnlanded reports whether run's newest capture is one standby stands by
// for and that did not land: the capture a standby may take the next place
// after ([PendingStore.TakeOver]).
func stoodByUnlanded(run PendingRun, standby SessionCapture) bool {
	newest := run.Capture
	return newest != nil && newest.State != CaptureLanded && newest.standsBy(standby)
}

// StandDown withdraws standby from the row's newest capture. See the contract
// on [PendingStore].
func (s *CoordStore) StandDown(ctx context.Context, turnID string, standby SessionCapture) (bool, error) {
	if err := stampable(turnID, standby); err != nil {
		return false, err
	}
	_, won, _, err := s.writeFound(ctx, turnID, false, Fence{}, func(run *PendingRun) bool {
		return standDownOnto(run, standby)
	})
	if err != nil {
		return false, err
	}
	return won, nil
}

// stampable is why capture cannot be stamped on run turnID's row, nil where it
// can: placed from (1, 1), in a state this build knows, within the bounds
// ([SessionCapture.Bounded]) every stamp write holds it to.
func stampable(turnID string, capture SessionCapture) error {
	if capture.LaunchSeq <= 0 || capture.CaptureSeq <= 0 || !capture.State.Valid() {
		return fmt.Errorf("sandbox: a capture of run %s is placed at (%d, %d) in state %q, and "+
			"every capture is placed from (1, 1) in one of %s", turnID, capture.LaunchSeq,
			capture.CaptureSeq, capture.State, words.Or(CaptureStates))
	}
	if err := capture.Bounded(); err != nil {
		return fmt.Errorf("sandbox: a capture of run %s: %w", turnID, err)
	}
	return nil
}

// stampCapture is the write [CoordStore.StampCapture], [CoordStore.SettleBegun]
// and [CoordStore.TakeOver] make: the row's own rule ([stampOnto]), where over
// — nil for none — holds of the row as the write finds it.
func (s *CoordStore) stampCapture(ctx context.Context, turnID string, capture SessionCapture,
	over func(PendingRun, SessionCapture) bool) (bool, error) {

	if err := stampable(turnID, capture); err != nil {
		return false, err
	}
	// SET ON EVERY ASK OF decide, which reruns on a lost race: the last is the
	// row the answer is about, if the last attempt found one
	// ([CoordStore.writeFound]).
	var held, stamped bool
	_, _, found, err := s.writeFound(ctx, turnID, false, Fence{}, func(run *PendingRun) bool {
		held, stamped = holdsStamp(*run, capture), false
		named := nameSummary(run, capture)
		if over != nil && !over(*run, capture) {
			return named
		}
		stamped = stampOnto(run, capture)
		return stamped || named
	})
	if err != nil {
		return false, err
	}
	return found && (stamped || held), nil
}

// nameSummary names capture's summary, the newest its take's read kept, on
// run's row where it is newer than the one the row names
// ([SummaryStamp.newerThan]), reporting whether it did. PLACED AT THE CAPTURE,
// or where the row's names the same summary, at that one's place: a summary
// found again (copied past its key's age, [SummaryStamp.names]) stays where it
// was first found, so a capture between the two places is not told it lacks
// what it holds.
func nameSummary(run *PendingRun, capture SessionCapture) bool {
	if capture.Summary == nil {
		return false
	}
	named := *capture.Summary
	named.LaunchSeq, named.CaptureSeq = capture.LaunchSeq, capture.CaptureSeq
	if run.Summary != nil && run.Summary.Digest == named.Digest {
		named.LaunchSeq, named.CaptureSeq = run.Summary.LaunchSeq, run.Summary.CaptureSeq
	}
	if named.Bounded() != nil || (run.Summary != nil && !named.newerThan(*run.Summary)) {
		return false
	}
	run.Summary = &named
	return true
}

// RetireSessions records the sessions a job of the turn spent, and how. See the
// contract on [PendingStore].
func (s *CoordStore) RetireSessions(ctx context.Context, turnID string, loss executorkeep.Loss,
	sessions []string) error {
	_, _, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		grown := withSpent(run.SpentSessions, loss, sessions)
		if slices.Equal(grown, run.SpentSessions) {
			return false
		}
		run.SpentSessions = grown
		return true
	})
	if err != nil {
		return fmt.Errorf("sandbox: recording the sessions run %s spent (%s): %w", turnID, loss, err)
	}
	return nil
}

// OweEnd records the end the run's turn owes the executor log. See the contract
// on [PendingStore].
//
// THROUGH THE WRITE EVERY STATE TAKES ([CoordStore.mutateAny]): a loss's or
// stop's end is decided by that ending's own steps, on a row whose ending is
// decided.
func (s *CoordStore) OweEnd(ctx context.Context, turnID string, ending json.RawMessage) (PendingRun, bool, error) {
	if len(bytes.TrimSpace(ending)) == 0 {
		return PendingRun{}, false, fmt.Errorf("sandbox: run %s is owed an end with no ending", turnID)
	}
	// Where an end is already owed the caller records that one, so the row as
	// it stands is answered, not the zero row an unchanged write answers.
	var seen PendingRun
	written, won, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		seen = *run
		if len(run.OwedEnd) > 0 || run.EndRecorded {
			return false
		}
		run.OwedEnd = bytes.Clone(ending)
		return true
	})
	if err != nil || won {
		return written, won, err
	}
	return seen, false, nil
}

// ClearEnd removes the end a row owes once the log has taken it, and marks it
// recorded in the same write. See the contract on [PendingStore].
func (s *CoordStore) ClearEnd(ctx context.Context, turnID string, owed json.RawMessage) (bool, error) {
	_, cleared, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		if len(run.OwedEnd) == 0 || !bytes.Equal(run.OwedEnd, owed) {
			return false
		}
		run.OwedEnd, run.EndRecorded = nil, true
		return true
	})
	return cleared, err
}

// foldPriorCalls is the turn's earlier successful calls, by name, plus l's
// round's (kept calls and dropped identities), and whether any call is not
// accounted for by name ([BridgeLog.PriorCalls]).
//
// A SORTED SET OF NAMES, never a log: whether a round delivered is whether a
// delivering tool's call succeeded, which a name and outcome answer, and the
// set is bounded by the catalogue, not by call count; still held to
// [MaxPriorCalls], past which it reports itself incomplete.
//
// A CALL IN FLIGHT MAKES IT INCOMPLETE ([BridgeLog.InFlight]): its outcome is
// unknown, so no reader may conclude the round did not reply.
func foldPriorCalls(l BridgeLog) ([]string, bool) {
	names := slices.Clone(l.PriorCalls)
	incomplete := l.PriorCallsElided || !l.ElisionKnown() || len(l.InFlight) > 0
	add := func(name string) {
		switch {
		case name == "" || slices.Contains(names, name):
		case len(names) >= MaxPriorCalls:
			incomplete = true
		default:
			names = append(names, name)
		}
	}
	for _, call := range l.Calls {
		if !call.Failed {
			add(call.Name)
		}
	}
	for _, e := range l.Unlisted() {
		if e.Calls > e.Failed {
			add(e.Name)
		}
	}
	slices.Sort(names)
	return names, incomplete
}

// ClaimForResume flips a run holding the tail's launch, in one of the tail's
// statuses, to resumed, reporting the row IFF THIS CALL WON.
//
// The at-most-once tail guard: two nodes can be handed the same completion (a
// redelivery, a zombie finishing between fence checks) and exactly one must run
// the tail. Launch, status, lease and write are one compare-and-swap, so the
// loser sees `resumed`, a job no longer its own, or a newer lease, and reports
// false. A fenced claim stamps its lease on the row, a zero one claims only a
// row no lease owns — see [PendingStore]. A row whose resume is held, at this
// store's instant, is no claim's.
func (s *CoordStore) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	var before string
	now := s.clock()
	run, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != tail.Launch || !slices.Contains(tail.From, run.Status) ||
			!slices.Contains(Claimable, run.Status) || superseded(*run, fence) || run.Held(now) {
			return false
		}
		before = run.Status
		run.Status = StatusResumed
		if fence.Fenced() {
			run.Owner, run.OwnerEpoch = fence.Owner, fence.Epoch
		}
		return true
	})
	if err != nil || !won {
		return PendingRun{}, false, err
	}
	// Carried back so a failed dispatch can restore the row. Never persisted,
	// and not inferable from other fields: a reused run keeps its old question.
	run.ClaimedFrom = before
	return run, true, nil
}

// ReleaseClaim hands a claimed run back while the claim still stands. See the
// contract on [PendingStore].
func (s *CoordStore) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	if !slices.Contains(Claimable, release.To) {
		return false, fmt.Errorf("sandbox: a claim is never taken out of %q, so it cannot be released to it",
			release.To)
	}
	if release.Collected && !release.CollectFailedAt.IsZero() {
		return false, errors.New("sandbox: a release cannot report a collection that both read its box " +
			"back and failed to")
	}
	if release.BoxAnswered && !release.CollectFailedAt.IsZero() {
		return false, errors.New("sandbox: a release cannot both count a failed collection and end the run " +
			"of failures it counts onto")
	}
	if !release.HostStoreSilentSince.IsZero() && release.HostStoreAnswered {
		return false, errors.New("sandbox: a release cannot say both that the host of the job's box found its " +
			"coordination store silent and that it answered")
	}
	if hold := release.Hold; hold != nil && len(hold.Condition) > MaxOwedConditionBytes {
		return false, fmt.Errorf("sandbox: the condition a hold on run %s names is %d bytes, past the %d an "+
			"owed refusal records", turnID, len(hold.Condition), MaxOwedConditionBytes)
	}
	now := s.clock().UTC()
	_, released, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusResumed || run.LaunchID != release.Launch || outranked(*run, release.Fence) {
			return false
		}
		if release.To == StatusAnswered && run.Answer == nil {
			// AN ENDING LET THE ANSWER GO under the claim: no answer is owed a
			// resume, and the row is that ending's to finish.
			return false
		}
		run.Status = release.To
		run.Charged = run.Charged || release.Charged
		run.CompanyCharged = run.CompanyCharged || release.CompanyCharged
		if run.Answer != nil && run.Answer.Taken() {
			// A retry gave the claim back, so the answer is the run's again
			// ([RecordedAnswer.TakenAt]).
			answer := *run.Answer
			answer.TakenAt = time.Time{}
			run.Answer = &answer
		}
		if run.WaitOver.Taken() {
			// And so is the engine's carrying on ([WaitOver.TakenAt]).
			over := *run.WaitOver
			over.TakenAt = time.Time{}
			run.WaitOver = &over
		}
		// Every fact below goes onto THIS job's record: the release names the
		// job the row holds or was refused above. What the claim's turn said it
		// does with the round is handed back; the next says it again
		// ([LaunchRecord.ResumeCarry]).
		run.Launch.ResumeCarry = ""
		if release.Published {
			run.Launch.Published = true
		}
		if at := release.CollectFailedAt; !at.IsZero() {
			run.Launch.CollectFailures++
			if run.Launch.CollectFailingSince.IsZero() {
				run.Launch.CollectFailingSince = at.UTC()
			}
		}
		if at := release.ResumeFailedAt; !at.IsZero() {
			run.Launch.ResumeFailures++
			if run.Launch.ResumeFailingSince.IsZero() {
				run.Launch.ResumeFailingSince = at.UTC()
			}
		}
		// THE RUN OF FAILURES ENDS AT A COLLECTION THAT READ THE BOX; the job is
		// its turn's from then on ([LaunchRecord.Collected]).
		if release.Collected {
			run.Launch.CollectFailures = 0
			run.Launch.CollectFailingSince = time.Time{}
			run.Launch.Collected = true
			if run.Launch.CollectedAt.IsZero() && !release.CollectedAt.IsZero() {
				run.Launch.CollectedAt = release.CollectedAt.UTC()
			}
		}
		// AND AT ONE WHOSE READ ANSWERED though a later step handed it back
		// uncounted: the job is not collected ([Release.BoxAnswered]).
		if release.BoxAnswered {
			run.Launch.CollectFailures = 0
			run.Launch.CollectFailingSince = time.Time{}
		}
		// HOW ITS HARNESS ENDED, in the write that returns the row to running.
		run.Launch.HarnessEnd = run.Launch.HarnessEnd.after(release.HarnessEnd)
		// WHAT THE COLLECTION LEARNED OF THE BOX HOST'S STORE
		// ([LaunchRecord.HostStoreSilentSince]): the latest clock's start, since
		// an earlier waiter's announcement says nothing of the silence found.
		switch {
		case !release.HostStoreSilentSince.IsZero():
			run.Launch.HostStoreSilentSince = release.HostStoreSilentSince.UTC()
		case release.HostStoreAnswered:
			run.Launch.HostStoreSilentSince = time.Time{}
		}
		// HELD, or not: every release decides afresh, so an earlier hold does not
		// outlast the attempt it held back.
		run.HeldFor, run.HeldUntil = "", time.Time{}
		if hold := release.Hold; hold != nil {
			run.HeldFor, run.HeldUntil = hold.Condition, hold.Until.UTC()
			run.Holds++
			// THE COMPLETION OWED, in the same write, where none is yet
			// ([LaunchRecord.RefusalOwed]): the hold spent its delivery and a lift
			// clears it, so this tells a restart or the seat's next holder the job
			// is owed its re-fire once the hold is gone.
			if hold.Condition != "" && run.Launch.RefusalOwed == "" {
				run.Launch.RefusalOwed, run.Launch.RefusalOwedSince = hold.Condition, now
			}
		}
		// THE WINDOW ITS RELAUNCH WAS REFUSED IN, unlike the hold, is KEPT by a
		// release naming none: the next relaunch reads it, and a resume that
		// failed otherwise has not tried that relaunch
		// ([LaunchRecord.RelaunchRefusedIn]). An invalid window is never written.
		if w := release.RelaunchRefusedIn; w.Valid() {
			run.Launch.RelaunchRefusedIn = w
		}
		return true
	})
	return released, err
}

// LiftHold clears a run's hold for cond — see [PendingStore.LiftHold].
func (s *CoordStore) LiftHold(ctx context.Context, turnID string, cond Condition, hold uint64) (bool, error) {
	_, lifted, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.HeldFor == "" || run.HeldFor != cond || run.Holds != hold {
			return false
		}
		run.HeldFor, run.HeldUntil = "", time.Time{}
		return true
	})
	return lifted, err
}

// MarkAwaiting parks a run until a person answers.
func (s *CoordStore) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	if q.AskedAt.IsZero() {
		return fmt.Errorf("sandbox: parking run %s on a question with no instant it was asked at", turnID)
	}
	_, _, err := s.mutateLive(ctx, turnID, func(run *PendingRun) bool {
		run.Status = StatusAwaiting
		// THE FIRST OF THEM, held to [MaxAudienceHandles], in resolution order (a
		// `team`: its lead then people in chart order; the fallback: nearest lead
		// first), so past the bound a unit keeps its lead and first members.
		run.AudienceHandles = slices.Clone(q.Answerers.Handles[:min(len(q.Answerers.Handles),
			MaxAudienceHandles)])
		run.AudienceFallback = q.Answerers.Fallback
		run.ParkedInputTokens, run.ParkedOutputTokens = q.InputTokens, q.OutputTokens
		run.ParkedCacheReadTokens, run.ParkedCacheWriteTokens = q.CacheReadTokens, q.CacheWriteTokens
		// A NEW QUESTION, measured from its own asking, with no answer or declined
		// reply: both belong to the question they were matched to.
		run.AskedAt = q.AskedAt
		run.Answer, run.DeclinedAnswers, run.AnswerRefusal, run.WaitOver = nil, nil, nil, nil
		// The cost of condensing the parking collection ([LaunchRecord.Condensed]).
		run.Launch.Condensed = q.Condensed
		// THE PARK SETTLES THE JOB'S COMPLETION: a refusal the seat's holder owed
		// is owed no more ([LaunchRecord.RefusalOwed]).
		run.Launch.RefusalOwed, run.Launch.RefusalOwedSince = "", time.Time{}
		return true
	})
	return err
}

// OweRefusal records on a job's record that the seat's holder acknowledged a
// refused delivery of its completion. See the contract on [PendingStore].
func (s *CoordStore) OweRefusal(ctx context.Context, turnID, launch string, owed OwedRefusal, fence Fence,
) (bool, error) {
	switch {
	case owed.Condition == "":
		return false, fmt.Errorf("sandbox: a refusal of run %s's completion names no condition to owe", turnID)
	case len(owed.Condition) > MaxOwedConditionBytes:
		return false, fmt.Errorf("sandbox: the condition a refusal of run %s's completion names is %d bytes, "+
			"past the %d an owed refusal records", turnID, len(owed.Condition), MaxOwedConditionBytes)
	case owed.Since.IsZero():
		return false, fmt.Errorf("sandbox: a refusal of run %s's completion owed since no instant", turnID)
	case launch == "":
		return false, nil
	}
	_, written, err := s.write(ctx, turnID, true, fence, func(run *PendingRun) bool {
		if run.LaunchID != launch || !run.completionUnsettled() || run.Launch.RefusalOwed != "" {
			return false
		}
		run.Launch.RefusalOwed, run.Launch.RefusalOwedSince = owed.Condition, owed.Since.UTC()
		return true
	})
	if errors.Is(err, ErrRunEnding) {
		return false, nil
	}
	return written, err
}

// RecordAnswer records a reply as the answer to a parked run's question, under
// the recording node's lease. See the contract on [PendingStore].
func (s *CoordStore) RecordAnswer(ctx context.Context, turnID, launch string, answer RecordedAnswer,
	fence Fence,
) (PendingRun, bool, error) {
	return s.recordOutcome(ctx, turnID, launch, answer, nil, fence)
}

// RecordRefusal records a reply too large to be the answer as the outcome of a
// parked run's question, under the recording node's lease. See the contract on
// [PendingStore].
func (s *CoordStore) RecordRefusal(ctx context.Context, turnID, launch string, answer RecordedAnswer,
	refusal AnswerRefusal, fence Fence,
) (PendingRun, bool, error) {
	if len(answer.Events) > 0 {
		return PendingRun{}, false, fmt.Errorf("sandbox: the refusal of a reply to run %s carries %d of "+
			"its deliveries, which are the bytes the row could not carry", turnID, len(answer.Events))
	}
	if err := refusal.check(answer.EventIDs); err != nil {
		return PendingRun{}, false, fmt.Errorf("sandbox: the refusal of a reply to run %s: %w", turnID, err)
	}
	return s.recordOutcome(ctx, turnID, launch, answer, &refusal, fence)
}

// check reports what is wrong with a refusal as one recorded against the
// answer naming answer's deliveries: it names exactly those, and its figures
// are a reply past a share.
func (r AnswerRefusal) check(answer []string) error {
	switch {
	case len(r.EventIDs) == 0 || !slices.Equal(r.EventIDs, answer):
		return fmt.Errorf("it names deliveries %s, and its answer names %s",
			words.And(r.EventIDs), words.And(answer))
	case r.MaxBytes <= 0 || r.Bytes <= r.MaxBytes:
		return fmt.Errorf("it states a reply of %d bytes against a share of %d, which is no refusal",
			r.Bytes, r.MaxBytes)
	}
	if n := addressBytes(r.Address); n > MaxRefusalAddressBytes {
		return fmt.Errorf("its address weighs %s on the row, and a refusal's may weigh %s",
			kept.SizeUp(n), kept.Size(MaxRefusalAddressBytes))
	}
	return nil
}

// recordOutcome is [CoordStore.RecordAnswer] and [CoordStore.RecordRefusal]:
// one compare-and-set recording answer as the question's outcome, the refusal
// it is beside it where refusal is set, and no refusal where it is not.
func (s *CoordStore) recordOutcome(ctx context.Context, turnID, launch string, answer RecordedAnswer,
	refusal *AnswerRefusal, fence Fence,
) (PendingRun, bool, error) {
	if len(answer.EventIDs) == 0 {
		return PendingRun{}, false, fmt.Errorf("sandbox: an answer to run %s names no delivery", turnID)
	}
	if !answer.Via.Valid() {
		return PendingRun{}, false, fmt.Errorf("sandbox: an answer to run %s names no route it came by "+
			"(via %q)", turnID, answer.Via)
	}
	if len(answer.Sealed) == 0 {
		// The words live only inside the seal ([RecordedAnswer.Sealed]), so
		// an answer without one is no answer any reader could resume with.
		return PendingRun{}, false, fmt.Errorf("sandbox: an answer to run %s carries no seal", turnID)
	}
	if answer.RecordedAt.IsZero() {
		answer.RecordedAt = s.clock()
	}
	// HELD TO ITS SHARE OF THE ROW, as written and as its decline would owe it
	// ([MaxAnswerBytes]). MEASURED ONCE here (its weight is the answer's alone),
	// but REFUSED INSIDE THE WRITE, only once the row still waits on the question
	// ([PendingRun.takesAnswer]): a reply to a run answered, moved on or ending
	// is that run's ordinary loss whatever it weighs, not "too large" for a
	// question nobody awaits.
	fits := answerFits(turnID, answer)
	if fits != nil && !errors.Is(fits, ErrAnswerTooLarge) {
		return PendingRun{}, false, fits
	}
	ownedBy := int64(0)
	var refused error
	recorded, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		// EVERY ATTEMPT DECIDES AFRESH: a refusal the last read did not make
		// is no answer.
		refused = nil
		if superseded(*run, fence) {
			ownedBy = run.OwnerEpoch
			return false
		}
		ownedBy = 0
		if !run.takesAnswer(launch, answer.EventIDs) {
			return false
		}
		if fits != nil {
			refused = fits
			return false
		}
		recorded := answer
		recorded.From = run.Status
		run.Status = StatusAnswered
		run.Answer = &recorded
		// THE QUESTION'S OUTCOME IS THIS ANSWER, not the engine's carrying on
		// a claim of it gave back ([WaitOver]).
		run.WaitOver = nil
		// THE REFUSAL IN THE SAME WRITE, never half an answer ([PendingRun.Refused]).
		run.AnswerRefusal = nil
		if refusal != nil {
			kept := *refusal
			kept.EventIDs, kept.Address = slices.Clone(refusal.EventIDs), maps.Clone(refusal.Address)
			run.AnswerRefusal = &kept
		}
		return true
	})
	switch {
	case err == nil && ownedBy > 0:
		err = fmt.Errorf("sandbox: recording an answer to run %s, which a newer lease (epoch %d) "+
			"than the recording node's (%d) owns: %w", turnID, ownedBy, fence.Epoch, ErrSeatNotHeld)
	case err == nil && refused != nil:
		return PendingRun{}, false, refused
	}
	return recorded, won, err
}

// DeclineAnswer lets go of a recorded answer, recording the copies it owes
// the seat's inbox in the same write. See the contract on [PendingStore].
func (s *CoordStore) DeclineAnswer(ctx context.Context, turnID, launch string, answer []string,
	handBack []HandedBack, fence Fence,
) (PendingRun, bool, error) {
	if len(answer) == 0 {
		return PendingRun{}, false, fmt.Errorf("sandbox: declining an answer to run %s that names no delivery", turnID)
	}
	owed := false
	declined, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusAnswered || run.LaunchID != launch || run.Answer == nil ||
			outranked(*run, fence) || !slices.Equal(run.Answer.EventIDs, answer) {
			return false
		}
		if owed = len(run.HandBack) > 0; owed {
			return false
		}
		ids := slices.Clone(answer)
		for _, copied := range handBack {
			ids = append(ids, copied.ID)
		}
		run.Status = run.Answer.declinedTo()
		run.Answer, run.AnswerRefusal = nil, nil
		run.DeclinedAnswers = boundedDeclined(run.DeclinedAnswers, ids)
		run.HandBack = slices.Clone(handBack)
		return true
	})
	if err == nil && owed {
		err = fmt.Errorf("sandbox: declining the answer to run %s: %w", turnID, ErrHandBackOwed)
	}
	return declined, won, err
}

// TakeAnswer records that a claimed run's turn took its recorded answer. See
// the contract on [PendingStore].
//
// SUPERSEDED, NOT OUTRANKED: the fence must be the row's own lease or newer,
// a zero fence included, the one write here a zero fence does not exempt (see
// the contract). Never once the ending is decided, which [CoordStore.mutate]
// refuses first.
func (s *CoordStore) TakeAnswer(ctx context.Context, turnID, launch string, fence Fence) (bool, error) {
	taken := false
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusResumed || run.LaunchID != launch || run.Answer == nil ||
			superseded(*run, fence) {
			return false
		}
		if taken = run.Answer.Taken(); taken {
			return false
		}
		answer := *run.Answer
		answer.TakenAt = s.clock()
		run.Answer = &answer
		return true
	})
	return won || (err == nil && taken), err
}

// ErrHeldCallUnclaimed is a held call's mark or result asked of a row that is
// not the claim of its answer (moved on, let go of, or fenced by another
// lease). Nothing was written and the call is not to be made.
var ErrHeldCallUnclaimed = errors.New("sandbox: the run is not the claim that holds the held call's answer")

// heldClaim reports whether run is the claim of launch's answer that may make
// its held call: [PendingStore.MarkHeldCall]'s terms.
func heldClaim(run PendingRun, launch string, fence Fence) bool {
	return run.Status == StatusResumed && run.LaunchID == launch && run.Answer != nil &&
		run.HeldCall != nil && run.HeldCall.LaunchID == launch && !superseded(run, fence)
}

// MarkHeldCall records that a claim's resume is making its run's held call. See
// the contract on [PendingStore].
func (s *CoordStore) MarkHeldCall(ctx context.Context, turnID, launch string, fence Fence) (HeldCall, bool, error) {
	var held HeldCall
	claimed := false
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if claimed = heldClaim(*run, launch, fence); !claimed {
			return false
		}
		held = *run.HeldCall
		if held.Calling() {
			return false
		}
		held.CallingAt = s.clock()
		run.HeldCall = &held
		return true
	})
	switch {
	case err != nil:
		return HeldCall{}, false, err
	case !claimed:
		return HeldCall{}, false, fmt.Errorf("%w: run %s, launch %s", ErrHeldCallUnclaimed, turnID, launch)
	}
	return held, won, nil
}

// waitsOnDelegates reports whether run's held call heldID waits on its
// delegates on launch: [PendingStore.GiveDelegateVerdict]'s terms.
func waitsOnDelegates(run PendingRun, launch, heldID string) bool {
	return slices.Contains(Awaiting, run.Status) && run.Ending == nil && run.LaunchID == launch &&
		run.HeldCall != nil && run.HeldCall.ID == heldID && run.HeldCall.Delegated()
}

// GiveDelegateVerdict records one delegate's answer. See the contract on
// [PendingStore].
func (s *CoordStore) GiveDelegateVerdict(ctx context.Context, turnID, launch, heldID string,
	v HeldVerdict) (HeldCall, bool, error) {

	if err := v.validate(); err != nil {
		return HeldCall{}, false, fmt.Errorf("sandbox: a verdict on held call %s of run %s: %w", heldID, turnID, err)
	}
	// REDACTED AFTER THE BOUNDS, which are the verdict tool's own and count
	// what the delegate wrote.
	given := v.Redacted()
	var held HeldCall
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if !waitsOnDelegates(*run, launch, heldID) {
			return false
		}
		d := *run.HeldCall.Delegation
		if !slices.Contains(d.Delegates, v.By) ||
			slices.ContainsFunc(d.Given, func(g HeldVerdict) bool { return g.By == v.By }) {
			return false
		}
		d.Given = append(slices.Clone(d.Given), given)
		held = *run.HeldCall
		held.Delegation = &d
		run.HeldCall = &held
		return true
	})
	if err != nil || !won {
		return HeldCall{}, false, err
	}
	return held, true, nil
}

// EscalateHeld puts a call waiting on its delegates to people. See the
// contract on [PendingStore].
func (s *CoordStore) EscalateHeld(ctx context.Context, turnID, launch, heldID string,
	e HeldEscalation) (bool, error) {

	if err := e.validate(); err != nil {
		return false, fmt.Errorf("sandbox: escalating held call %s of run %s: %w", heldID, turnID, err)
	}
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if !waitsOnDelegates(*run, launch, heldID) {
			return false
		}
		d := *run.HeldCall.Delegation
		escalated := HeldEscalation{Overruled: slices.Clone(e.Overruled), Engine: slices.Clone(e.Engine), At: e.At}
		d.Escalated = &escalated
		held := *run.HeldCall
		held.Delegation = &d
		run.HeldCall = &held
		return true
	})
	return won, err
}

// RecordHeldResult records what a marked held call came to: its result and the
// call as its round's log keeps it, on the row, then the call in that log. See
// the contract on [PendingStore].
func (s *CoordStore) RecordHeldResult(ctx context.Context, turnID, launch string, made HeldMade,
	fence Fence) (bool, error) {

	claimed, already := false, false
	var (
		held  HeldCall
		round string
	)
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if claimed = heldClaim(*run, launch, fence) && run.HeldCall.Calling(); !claimed {
			return false
		}
		held, round = *run.HeldCall, run.Round
		if already = held.Made(); already {
			return false
		}
		held.Result, held.MadeAt = made.Result, s.clock()
		call := approvedCall(made.Call, held)
		held.Call = &call
		run.HeldCall = &held
		return true
	})
	switch {
	case err != nil:
		return false, err
	case !claimed:
		return false, fmt.Errorf("%w: run %s, launch %s", ErrHeldCallUnclaimed, turnID, launch)
	case !won && !already:
		return false, nil
	case held.Call == nil:
		// A RESULT RECORDED BY A BUILD WITHOUT THE CALL'S ENTRY: nothing to append.
		return true, nil
	}
	// THE CALL, AFTER ITS RESULT, in its round's log, once by the held call's
	// id: by the attempt that recorded the result, or a retry after one that
	// stopped before this append. Failing that, the next job's opening appends
	// it ([OwedCall]).
	if _, _, err := s.runs.MutateSandboxRunPart(ctx, turnID, coord.RunLog, func(current coord.Record, found bool) ([]byte, error) {
		if !found {
			return nil, nil
		}
		var log BridgeLog
		if err := json.Unmarshal(current.Value, &log); err != nil {
			return nil, fmt.Errorf("sandbox: decode the log of run %s: %w", turnID, err)
		}
		next, appended := log.withApproved(round, *held.Call)
		if !appended {
			return nil, nil
		}
		return json.Marshal(next)
	}); err != nil {
		return false, fmt.Errorf("sandbox: append run %s's approved call to its log: %w", turnID, err)
	}
	return true, nil
}

// approvedCall is the call a person approved as its round's log keeps it
// ([HeldCall.Call]): the held call's tool and id, stamped with its result's
// instant, marked approved, with no arguments, digest or output (they ride the
// row sealed, [BridgeCall.Withheld]); every other field held to a call's share
// ([keptCall]). No place and no mark: the log gives both ([BridgeLog.withApproved]);
// never late nor Abandoned (only the bridge marks that), and no harness
// [BridgeCall.CallID] (the box's session holds the HELD call under its own id).
// No [BridgeCall.OutputBytes]: a size beside no output reads as an output cut
// to nothing. Nor a runner or wait bound ([keptCall]). Every field is one this
// writer keeps or drops, never left to a caller (the engine's resume hands only
// name, failure, answerer and refusal, internal/engine's heldCommit.Record).
func approvedCall(made BridgeCall, held HeldCall) BridgeCall {
	made.Name, made.Approved, made.At, made.HeldID = held.Tool, true, held.MadeAt, held.ID
	made.Args, made.Output, made.OutputBytes, made.Digest, made.ArgsUnparsed = "", "", 0, "", false
	made.Held, made.Seq, made.Appended, made.Late, made.Abandoned, made.CallID = false, 0, 0, false, false, ""
	return keptCall(made, MaxBridgeArgsBytes)
}

// OweHandBack lets go of the recorded answer a run whose ending is decided still
// holds, owing its copies to the seat in the same write. See the contract on
// [PendingStore].
func (s *CoordStore) OweHandBack(ctx context.Context, turnID string, letGo LetGo) (PendingRun, bool, error) {
	if len(letGo.Answer) == 0 {
		return PendingRun{}, false, fmt.Errorf("sandbox: letting go of an answer to run %s that "+
			"names no delivery", turnID)
	}
	var owing PendingRun
	taken, owed := false, false
	written, won, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		if run.Ending == nil || run.Ending.ID != letGo.Ending || run.Answer == nil ||
			!slices.Equal(run.Answer.EventIDs, letGo.Answer) {
			return false
		}
		if taken = !run.answerOwed(); taken {
			owing = *run
			return false
		}
		if owed = len(run.HandBack) > 0; owed {
			owing = *run
			return false
		}
		run.Answer, run.AnswerRefusal = nil, nil
		run.HandBack = nil
		if len(letGo.HandBack) > 0 {
			run.HandBack = slices.Clone(letGo.HandBack)
		}
		return true
	})
	switch {
	case err != nil:
		return PendingRun{}, false, err
	case taken:
		return owing, false, fmt.Errorf("sandbox: letting go of run %s's answer: %w", turnID, ErrAnswerTaken)
	case owed:
		return owing, false, fmt.Errorf("sandbox: letting go of run %s's answer: %w", turnID, ErrHandBackOwed)
	}
	return written, won, nil
}

// ReviveAnswer gives a claim no resume holds back to its recorded answer,
// fenced to the reviving node and — where the claim was lost — counted. See the
// contract on [PendingStore].
func (s *CoordStore) ReviveAnswer(ctx context.Context, turnID string, revival Revival) (PendingRun, bool, error) {
	if len(revival.Answer) == 0 {
		return PendingRun{}, false, fmt.Errorf("sandbox: reviving an answer to run %s that names no "+
			"delivery", turnID)
	}
	var taken PendingRun
	wasTaken := false
	written, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusResumed || run.LaunchID != revival.Launch || run.Answer == nil ||
			!slices.Equal(run.Answer.EventIDs, revival.Answer) || outranked(*run, revival.Fence) {
			return false
		}
		if wasTaken = run.Answer.Taken(); wasTaken {
			taken = *run
			return false
		}
		answer := *run.Answer
		if revival.Lost {
			answer.LostClaims++
			if answer.FirstLostAt.IsZero() {
				answer.FirstLostAt = s.clock()
			}
		}
		run.Answer = &answer
		run.Status = StatusAnswered
		if revival.Fence.Fenced() {
			run.Owner, run.OwnerEpoch = revival.Fence.Owner, revival.Fence.Epoch
		}
		return true
	})
	switch {
	case err != nil:
		return PendingRun{}, false, err
	case wasTaken:
		return taken, false, fmt.Errorf("sandbox: reviving run %s's answer: %w", turnID, ErrAnswerTaken)
	}
	return written, won, nil
}

// DecideEnding records a run's ending, or hands back the one already decided.
// See the contract on [PendingStore].
func (s *CoordStore) DecideEnding(ctx context.Context, turnID string, d Decision) (PendingRun, bool, error) {
	var decided PendingRun
	already := false
	written, won, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		license := d.License
		if outranked(*run, license.Fence) || !slices.Contains(license.WhileIn, run.Status) ||
			(license.Launch != EveryLaunch && run.LaunchID != license.Launch) {
			return false
		}
		// A collected job is its turn's to end ([LaunchRecord.Collected]).
		if d.Stop != nil && run.Ending == nil && run.Launch.Collected {
			return false
		}
		if run.Ending != nil {
			decided, already = *run, true
			return false
		}
		ending := &RecordedEnding{
			ID: uuid.NewString(), At: s.clock(),
			Reason: d.Reason, Detail: d.Detail, ErrorKind: d.ErrorKind,
			Unused: license.Unused, Reclaim: d.Reclaim,
		}
		run.Ending = ending
		// Who stopped it ([PendingRun.Stop]), in the same write.
		if d.Stop != nil {
			stop := *d.Stop
			run.Stop = &stop
		}
		// WHAT GOES BACK TO THE SEAT, settled with the ending: copies already
		// owed, and the held answer if owed, with a copy to hand back or a
		// refusal (its reply goes back as the delivery it came in, kept on the
		// seat's inbox until the refusal leaves the run, [AnswerRefused]).
		_, refused := run.Refused()
		owedRefusal := run.answerOwed() && refused
		ending.Returned = len(run.HandBack) > 0 ||
			(run.answerOwed() && len(run.Answer.Events) > 0) || owedRefusal
		run.EndingReturnsRefusal = owedRefusal
		return true
	})
	switch {
	case err != nil:
		return PendingRun{}, false, err
	case already:
		return decided, true, nil
	}
	return written, won, nil
}

// CollectStop records what a stop's ending read of its run before reclaiming
// the box. See the contract on [PendingStore].
func (s *CoordStore) CollectStop(ctx context.Context, turnID string, c StopCollection) (PendingRun, bool, error) {
	return s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		if run.Ending == nil || run.Ending.ID != c.Ending || run.Stop == nil || run.Stop.Collected {
			return false
		}
		stop := *run.Stop
		stop.Collected = true
		stop.InputTokens, stop.OutputTokens = c.InputTokens, c.OutputTokens
		stop.CacheReadTokens, stop.CacheWriteTokens = c.CacheReadTokens, c.CacheWriteTokens
		stop.Condensed = c.Condensed
		run.Stop = &stop
		// RECORDED, NEVER CLEARED, as a release does: a charge or publish an
		// earlier attempt made stays made.
		run.Charged = run.Charged || c.Charged
		run.CompanyCharged = run.CompanyCharged || c.CompanyCharged
		run.Launch.Published = run.Launch.Published || c.Published
		return true
	})
}

// ClearHandBack removes published copies from a run's hand-back. See the
// contract on [PendingStore].
func (s *CoordStore) ClearHandBack(ctx context.Context, turnID string, ids []string) (bool, error) {
	_, won, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		kept := slices.DeleteFunc(slices.Clone(run.HandBack), func(h HandedBack) bool {
			return slices.Contains(ids, h.ID)
		})
		if len(kept) == len(run.HandBack) {
			return false
		}
		if len(kept) == 0 {
			kept = nil
		}
		run.HandBack = kept
		return true
	})
	return won, err
}

// boundedDeclined is the declined ids once a decline adds its own: EVERY id of
// the newest decline, and as many earlier ones, newest first, as fit within
// [maxDeclinedAnswers]. A dropped earlier id's copies were long since delivered
// (a decline lands only on a row owing none, [ErrHandBackOwed]), and the bound
// stops a repeatedly answered and declined question growing its row forever.
//
// THE NEWEST DECLINE WHOLE, even past the bound: its copies are still owed or
// just published, and a dropped id would let a copy arriving on the seat's
// inbox be recorded as the answer to the question that let it go, circling the
// run (a batch is up to twenty deliveries, so a plain newest-sixteen cut did).
func boundedDeclined(earlier, newest []string) []string {
	keep := max(maxDeclinedAnswers-len(newest), 0)
	if len(earlier) > keep {
		earlier = earlier[len(earlier)-keep:]
	}
	return append(slices.Clone(earlier), newest...)
}

// ClaimOwnership moves a run to this node, refusing to steal a newer lease.
func (s *CoordStore) ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error) {
	_, won, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		if run.OwnerEpoch > epoch {
			// A newer lease owns it; taking it would put two engines on one box.
			return false
		}
		run.Owner, run.OwnerEpoch = owner, epoch
		return true
	})
	return won, err
}

// SetStatus moves a run to a new lifecycle state, fenced on the epoch.
func (s *CoordStore) SetStatus(ctx context.Context, turnID, status string, fence Fence) error {
	if !slices.Contains(Active, status) {
		return fmt.Errorf("sandbox: unknown status %q", status)
	}
	_, _, err := s.mutateLive(ctx, turnID, func(run *PendingRun) bool {
		if outranked(*run, fence) {
			return false
		}
		run.Status = status
		return true
	})
	return err
}

// ExpirePause flips a parked run to reseed — or, for an answered one, keeps
// the answer and forgets the box — and clears its box record. See the
// contract on [PendingStore].
func (s *CoordStore) ExpirePause(ctx context.Context, turnID string) (bool, error) {
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		switch run.Status {
		case StatusAwaiting:
			run.Status = StatusReseed
		case StatusAnswered:
			// The answer stays recorded and owed; only the box goes, and the
			// resume re-seeds from the branch. From moves with it, so a
			// let-go answer returns the run to reseed, not to a park naming
			// a vanished box.
			if run.Answer == nil {
				return false
			}
			run.Answer.From = StatusReseed
		default:
			return false
		}
		// Cleared in the SAME write as the flip: two writes expose a reseeded
		// run still naming the box an arriving answer would continue in.
		run.SandboxID, run.CommandID = "", ""
		run.PausedAt, run.BoxSince = time.Time{}, time.Time{}
		return true
	})
	return won, err
}

// MarkLost records the verdict that a running job was lost with the host it
// ran on. See the contract on [PendingStore].
//
// NO FENCE: the verdict is the waiter's, which holds no seat, and a fact about
// a job and its host, not an ownership decision; what it licenses is the seat
// holder's carrying on, under that holder's lease. The job, status and judged
// node guard it.
func (s *CoordStore) MarkLost(ctx context.Context, turnID, launch string, loss JobLoss) (bool, error) {
	if launch == "" {
		return false, nil
	}
	stands := false
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launch || run.Status != StatusRunning || run.LaunchNode != loss.Node {
			return false
		}
		if run.Lost != nil {
			stands = true
			return false
		}
		at := loss.At
		if at.IsZero() {
			at = s.clock()
		}
		recorded := loss
		recorded.At = at.UTC()
		run.Lost = &recorded
		return true
	})
	if err != nil {
		return false, fmt.Errorf("sandbox: record run %s's job as lost: %w", turnID, err)
	}
	return won || stands, nil
}

// MarkClaimLost records on a claimed job the verdict that it was lost with the
// host its box is on, for the claim's own collection. See the contract on
// [PendingStore].
//
// FENCED, unlike [CoordStore.MarkLost]: the verdict is the claim holder's,
// made under its lease; a newer lease owns the run and its collection from the
// moment it takes the seat.
func (s *CoordStore) MarkClaimLost(ctx context.Context, turnID, launch string, loss JobLoss,
	fence Fence,
) (bool, error) {
	if launch == "" {
		return false, nil
	}
	stands := false
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launch || run.Status != StatusResumed || outranked(*run, fence) ||
			run.LaunchNode != loss.Node {
			return false
		}
		if run.Lost != nil {
			stands = true
			return false
		}
		at := loss.At
		if at.IsZero() {
			at = s.clock()
		}
		recorded := loss
		recorded.At = at.UTC()
		run.Lost = &recorded
		return true
	})
	if err != nil {
		return false, fmt.Errorf("sandbox: record run %s's claimed job as lost: %w", turnID, err)
	}
	return won || stands, nil
}

// AttachSandbox records which box a run is using, fenced on the epoch.
//
// A REFUSED ATTACH IS AN ERROR, like a refused relaunch ([CoordStore.BeginLaunch]):
// a row that did not take it (a newer lease owns the run, [ErrSeatNotHeld], or
// its ending is decided, [ErrRunEnding]) names some other box, so the launch
// abandons its box rather than run a job nothing will reclaim. The attach
// follows a box setup that can take minutes while the seat moves, but the next
// holder ends a launching run without fencing it ([Coordinator.RecoverSeat]),
// so the move reaches the attach as the ending or no record, not a newer lease.
func (s *CoordStore) AttachSandbox(ctx context.Context, turnID string, box BoxRef, fence Fence) (err error) {
	defer origin.Stamp(&err)
	// THE BRIEF'S LEADS ARE TAKEN INTO THE RUN'S LEADS FIRST, beside the row
	// ([CoordStore.takeLeads]), and marked on the job's record by the write
	// below: a take landing beside an attach that does not leaves leads no job
	// marks, never a job marked with leads the part does not keep. A refused or
	// unwritable take refuses the attach before anything is written.
	var took leadsTaken
	if box.CommandID != "" && (len(box.Leads.Own) > 0 || len(box.Leads.Fresh) > 0 || len(box.Leads.Told) > 0 ||
		len(box.Leads.ToldUnread) > 0) {
		if took, err = s.takeLeads(ctx, turnID, box.Leads, fence); err != nil {
			return fmt.Errorf("sandbox: attach box %s to run %s: %w", box.SandboxID, turnID, err)
		}
	}
	var job handedSet
	_, attached, err := s.mutateFenced(ctx, turnID, fence, func(run *PendingRun) bool {
		job = handedSet{}
		// A BOX NEW TO THE ROW IS DATED HERE, one it names keeps its date
		// ([PendingRun.BoxSince]): a continued job attaches its turn's kept box
		// twice, and a re-dated box would never age past [KeptBoxMaxAge].
		if run.SandboxID != box.SandboxID {
			run.BoxSince = s.clock()
		}
		run.SandboxID = box.SandboxID
		run.CommandID = box.CommandID
		// A COMMAND IS A JOB THAT STARTED, on the job's own record where nothing
		// forgetting the box reaches it ([LaunchRecord.Started]). Only ever set:
		// the attach before the job names no command and unsays nothing.
		if box.CommandID != "" {
			run.Launch.Started = true
			// WHAT THE JOB WAS HANDED, named for the job: what a capture of its
			// box scrubs out, written beside the row once this write lands
			// ([CoordStore.Handed]).
			if box.Handed != "" {
				job = handedSet{Launch: run.LaunchID, LaunchSeq: run.LaunchSeq, Sealed: box.Handed}
			}
			// The brief and what it and the fresh one omitted from the carried
			// round ([LaunchRecord.Brief], [LaunchRecord.BriefOmitted]).
			run.Launch.Brief, run.Launch.NotContinued = box.Brief, box.NotContinued
			run.Launch.BriefOmitted = slices.Clone(box.BriefOmitted)
			run.Launch.FreshOmitted = slices.Clone(box.FreshOmitted)
			run.Launch.BriefLateOmitted = slices.Clone(box.BriefLateOmitted)
			run.Launch.FreshLateOmitted = slices.Clone(box.FreshLateOmitted)
			// WHICH LEADS ITS BRIEF TOLD, by number in the run's leads part
			// ([LaunchRecord.LeadMarks]): only where the row still names the job
			// and run they were kept for, so no job is marked with another's
			// leads, and the count only grows.
			if took.landed && run.LaunchID == box.Leads.Job && run.LeadsRun == box.Leads.Run {
				run.Launch.LeadMarks = slices.Clone(took.marks)
				run.LeadsAppended = max(run.LeadsAppended, took.appends)
			}
			// The session its script starts of its own in a continued one's
			// place ([PendingRun.SessionRan]).
			run.Launch.FreshSessionID = box.FreshSessionID
			// What the handed session carried, for the collecting read to keep
			// where the job makes no request of its own ([LaunchRecord.Context]).
			run.Launch.Context = box.Context
			// Where that session restarts last ([LaunchRecord.BasisRestart]).
			run.Launch.BasisRestart = box.Restart
			// The kept capture it continued, for a later capture to be raised to
			// ([LaunchRecord.Continued]).
			run.Launch.Continued = box.Continued
			// A browser seat's browser as the launch readied it
			// ([LaunchRecord.Browser]).
			run.Launch.Browser = box.Browser
		}
		// The box's cell and host move with the box ([BoxRef.Placement], [BoxRef.Node]).
		if box.Placement != "" {
			run.Placement = box.Placement
		}
		if box.Node != "" {
			run.LaunchNode = box.Node
		}
		run.Harness = box.Harness
		run.SessionID = box.SessionID
		run.PauseTTLSeconds = box.PauseTTLSec
		// An attached box is RUNNING, so the pause stamp goes: a reused box
		// still carried paused_at from its collect, which the operator board
		// draws a held box from, so a live run would show as paused.
		run.PausedAt = time.Time{}
		return true
	})
	switch {
	case err != nil:
		return fmt.Errorf("sandbox: attach box %s to run %s: %w", box.SandboxID, turnID, err)
	case !attached:
		return fmt.Errorf("sandbox: attach box %s to run %s: the run has no record", box.SandboxID, turnID)
	}
	if job.Launch != "" {
		s.writeHanded(ctx, turnID, job)
	}
	return nil
}

// leadsTaken is what [CoordStore.takeLeads] answers the attach that marks the
// job's record: the numbers of the leads the brief told that the run's leads
// part keeps, how many leads the part has taken in all, and whether the part
// stands as the take decided it.
type leadsTaken struct {
	marks   []int
	appends int
	landed  bool
}

// leadsSeen is a leads part a take found and may not take into: another run's,
// by its name, or one that does not decode.
type leadsSeen struct {
	run       string
	undecoded bool
}

// takeLeads takes the leads a brief opened with into the turn's leads part
// ([leadsPart.took]) BEFORE the row write naming the job's command
// ([CoordStore.AttachSandbox]), answering what that write marks.
//
// ONE COMPARE-AND-SWAP OF THE PART: none starts b's run's; b's run's own is
// taken into; any other is overwritten only once the ROW, read outside the
// decision, names b's run (the part is an earlier run's left by coord's
// removeRunPart, or unreadable), the take rerunning cleared to replace exactly
// it. A row naming another run, or a differing part, is a launch of an ended
// run, refused with the contract's two answers. A part started anew or holding
// fewer than the row counts numbers its first lead past [BriefLeads.Counted],
// so a reader counts the lost ones not kept ([PendingStore.Leads]).
func (s *CoordStore) takeLeads(ctx context.Context, turnID string, b BriefLeads, fence Fence) (leadsTaken, error) {
	if b.Run == "" || b.Job == "" {
		return leadsTaken{}, errors.New("the leads of its brief name no run or no job: a launch names both " +
			"from the row it opened")
	}
	taken, foreign, err := s.takeLeadsOnce(ctx, turnID, b, nil)
	if err != nil || foreign == nil {
		return taken, err
	}
	row, found, err := s.Get(ctx, turnID)
	switch {
	case err != nil:
		return leadsTaken{}, fmt.Errorf("reading the record a leads part of another run is beside: %w", err)
	case !found:
		// THE RUN IS GONE: nothing is taken, and the row's write says so.
		return leadsTaken{}, nil
	case row.LeadsRun != b.Run:
		refusal := ErrRunEnding
		if outranked(row, fence) {
			refusal = ErrSeatNotHeld
		}
		return leadsTaken{}, fmt.Errorf("the leads of its brief are run %s's, and the record names run %s: the "+
			"launch is of a run that ended: %w", b.Run, row.LeadsRun, refusal)
	}
	taken, again, err := s.takeLeadsOnce(ctx, turnID, b, foreign)
	if err != nil || again == nil {
		// REPLACED where the take had a lead to start the run's part with and
		// the store kept it; a take only retelling leads writes nothing and
		// leaves the other run's part to the next take with one.
		if err == nil && taken.landed && len(b.Own)+len(b.Fresh) > 0 {
			log.InfoContext(ctx, "sandbox_leads_part_replaced", "turn_id", turnID, "run", b.Run,
				"replaced_run", foreign.run, "undecoded", foreign.undecoded)
		}
		return taken, err
	}
	return leadsTaken{}, fmt.Errorf("the leads of its brief are run %s's, and beside the record is a part of "+
		"another run than the one the record cleared it to replace: the turn's runs moved under the launch: %w",
		b.Run, ErrRunEnding)
}

// takeLeadsOnce is one decision of [CoordStore.takeLeads] on the part as it
// stands, cleared to replace the part replacing names (nil for none): what it
// took, or the part it found and may not take into.
//
// LANDED is what the attach marks by: true where nothing was written (the part
// already keeps every lead) or the write was kept; false where the store undid
// it because the run ended, and the row's write then refuses the attach.
func (s *CoordStore) takeLeadsOnce(ctx context.Context, turnID string, b BriefLeads,
	replacing *leadsSeen) (leadsTaken, *leadsSeen, error) {

	// SET ON EVERY ASK OF decide, which runs again on a lost race.
	var (
		taken   leadsTaken
		foreign *leadsSeen
		wrote   bool
	)
	_, changed, err := s.runs.MutateSandboxRunPart(ctx, turnID, coord.RunLeads,
		func(current coord.Record, found bool) ([]byte, error) {
			taken, foreign, wrote = leadsTaken{}, nil, false
			part := leadsPart{Run: b.Run}
			if found {
				var held leadsPart
				undecoded := json.Unmarshal(current.Value, &held) != nil
				seen := leadsSeen{run: held.Run, undecoded: undecoded}
				switch {
				case !undecoded && held.Run == b.Run:
					part = held
				case replacing != nil && *replacing == seen:
					// THE PART THE RECORD CLEARED THIS TAKE TO REPLACE: started anew.
				default:
					foreign = &seen
					return nil, nil
				}
			}
			part.Appends = max(part.Appends, b.Counted)
			next, marks, grew := part.took(b.Own, b.Fresh, b.Told, b.ToldUnread)
			taken = leadsTaken{marks: marks, appends: next.Appends}
			if !grew {
				return nil, nil
			}
			wrote = true
			return json.Marshal(next)
		})
	switch {
	case err != nil:
		return leadsTaken{}, nil, fmt.Errorf("keeping the leads its brief opened with: %w", err)
	case foreign != nil:
		return leadsTaken{}, foreign, nil
	}
	taken.landed = !wrote || changed
	return taken, nil, nil
}

// handedSet is the part beside a run's row holding what its job's box was
// handed, sealed ([CoordStore.Handed]), named for its launch, plus every token
// the completion poll renewed since ([CoordStore.AddHanded]).
//
// BESIDE THE ROW, NEVER ON IT ([coord.RunHanded]): up to [MaxHandedBytes]
// before sealing, which every row read, git request of a coding box and
// listing would carry though only three read it: a capture of the job's
// session (to scrub it), the next launch's opening (to carry it into a
// continued box) and the poll recording a renewed token, which grows it by one
// compare-and-swap or marks it ([handedSet.Incomplete]).
type handedSet struct {
	// Launch is the job the set was written for ([PendingRun.LaunchID]): a
	// set read for another job is none.
	Launch string `json:"launch"`
	// LaunchSeq is that job's ordinal ([PendingRun.LaunchSeq]), so a late write
	// never replaces a later job's set.
	LaunchSeq int64 `json:"launch_seq"`
	// Sealed is the set as [SessionKeep.SealHanded] sealed it.
	Sealed string `json:"sealed"`
	// Incomplete is why the set no longer holds every value its box holds,
	// where a refresh could not add a token ([PendingStore.AddHanded]); Sealed
	// is then empty. Engine words, never a value, since it sits unsealed. AN
	// EMPTY SEAL IS WHAT REFUSES, so an older build reading none of this refuses
	// captures as this one does, and a later job continuing in the box carries
	// nothing and refuses its own ([Coordinator.carryHanded]).
	Incomplete string `json:"incomplete,omitempty"`

	// Extra is every key this build does not know, kept verbatim through a
	// renewal's write (wirekeys.go) and dropped with the set by the next job's
	// attach. Filled by the decode, never by a caller.
	Extra map[string]json.RawMessage `json:"-"`
}

// of is the sealed set where it was written for launch, and "" otherwise.
func (h handedSet) of(launch string) string {
	if launch == "" || h.Launch != launch {
		return ""
	}
	return h.Sealed
}

// incompleteFor is why launch's set holds less than its box does, or "" where
// the set is not launch's or not marked so.
func (h handedSet) incompleteFor(launch string) string {
	if launch == "" || h.Launch != launch {
		return ""
	}
	return h.Incomplete
}

// readHanded reads the handed set beside turnID's run — the zero set where
// there is none.
func (s *CoordStore) readHanded(ctx context.Context, turnID string) (handedSet, error) {
	record, found, err := s.runs.SandboxRunPart(ctx, turnID, coord.RunHanded)
	switch {
	case err != nil:
		return handedSet{}, fmt.Errorf("read what run %s's box was handed: %w", turnID, err)
	case !found:
		return handedSet{}, nil
	}
	var held handedSet
	if err := json.Unmarshal(record.Value, &held); err != nil {
		return handedSet{}, fmt.Errorf("decode what run %s's box was handed: %w", turnID, err)
	}
	return held, nil
}

// writeHanded writes job's set beside turnID's run once the attach naming the
// job has landed, never before: a set for an attach that did not land would be
// read by the next opening as what the row's box was handed. NEVER OVER A LATER
// JOB'S set, NOR OVER ITS OWN JOB'S MARK ([handedSet.Incomplete], written by a
// poll after a token found no set to join), which a late write would lose.
//
// A FAILURE IS LOGGED, NOT RETURNED: the attach landed and its job runs, and a
// refusal would abandon a box the row names. A job without its set has every
// capture refused ([ErrHandedUnknown]): the session is not kept, rather than
// kept scrubbed of less than its box was handed.
func (s *CoordStore) writeHanded(ctx context.Context, turnID string, job handedSet) {
	raw, err := json.Marshal(job)
	if err == nil {
		_, _, err = s.runs.MutateSandboxRunPart(ctx, turnID, coord.RunHanded,
			func(current coord.Record, found bool) ([]byte, error) {
				if found {
					var held handedSet
					if json.Unmarshal(current.Value, &held) == nil && (held.LaunchSeq > job.LaunchSeq ||
						held.incompleteFor(job.Launch) != "") {
						return nil, nil
					}
				}
				return raw, nil
			})
	}
	if err != nil {
		log.WarnContext(ctx, "sandbox_handed_not_kept", "turn_id", turnID, "launch", job.Launch,
			"error", err.Error(),
			"detail", "the job runs, and every capture of its session is refused, since what its box "+
				"was handed cannot be read back to scrub it out")
	}
}

// Handed reads what run's job's box was handed, sealed. See the contract on
// [PendingStore].
func (s *CoordStore) Handed(ctx context.Context, run PendingRun) (string, error) {
	held, err := s.readHanded(ctx, run.TurnID)
	if err != nil {
		return "", fmt.Errorf("sandbox: %w", err)
	}
	if why := held.incompleteFor(run.LaunchID); why != "" {
		return "", fmt.Errorf("%w: %s", ErrHandedIncomplete, why)
	}
	return held.of(run.LaunchID), nil
}

// Leads reads the turn's leads kept beside run's row. See the contract on
// [PendingStore].
//
// THE ROW SAYS WHAT THE PART SHOULD HOLD: its run ([PendingRun.LeadsRun]) and
// count taken ([PendingRun.LeadsAppended]), so another run's part, or none,
// reads as none of this run's with every counted lead not kept, and a part
// holding more than the row counts (an attach that took leads and did not land)
// is read whole. A cut is never below none.
func (s *CoordStore) Leads(ctx context.Context, run PendingRun) (TurnLeads, error) {
	record, found, err := s.runs.SandboxRunPart(ctx, run.TurnID, coord.RunLeads)
	switch {
	case err != nil:
		return TurnLeads{}, fmt.Errorf("sandbox: read the leads of run %s: %w", run.TurnID, err)
	case !found:
		return TurnLeads{Cut: run.LeadsAppended}, nil
	}
	var part leadsPart
	if err := json.Unmarshal(record.Value, &part); err != nil {
		return TurnLeads{}, fmt.Errorf("sandbox: decode the leads of run %s: %w", run.TurnID, err)
	}
	if part.Run != run.LeadsRun {
		return TurnLeads{Cut: run.LeadsAppended}, nil
	}
	return TurnLeads{Kept: part.Leads, Cut: max(0, max(part.Appends, run.LeadsAppended)-len(part.Leads))}, nil
}

// AddHanded hands run's job's sealed set to add before a refresh writes a
// token into the job's box. See the contract on [PendingStore].
//
// THE ROW FIRST, THEN THE PART. The part's compare-and-swap decides the set
// (the job's, by launch, [handedSet.of]); the earlier row read refuses a poll
// whose listing predates the job's end: a relaunch's opening reads the set
// before resetting the row ([CoordStore.resetLaunch]) and a stop's or loss's
// capture reads it after the ending's decision, so a later token would land in
// a set nobody reads. The gap between the reads is one round trip, which no
// claim, collection and relaunch fits in.
func (s *CoordStore) AddHanded(ctx context.Context, run PendingRun,
	add func(sealed string) (next, incomplete string)) (bool, error) {

	current, found, err := s.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		return false, fmt.Errorf("sandbox: read run %s to record what its box is handed: %w", run.TurnID, err)
	case !found || current.LaunchID != run.LaunchID || current.LaunchSeq != run.LaunchSeq ||
		current.Status != StatusRunning || current.Launch.Collected || current.Lost != nil ||
		current.endDecided():
		return false, nil
	}
	// SET ON EVERY ASK OF decide, which runs again on a lost race.
	later, wrote := false, false
	_, changed, err := s.runs.MutateSandboxRunPart(ctx, run.TurnID, coord.RunHanded,
		func(record coord.Record, found bool) ([]byte, error) {
			later, wrote = false, false
			var held handedSet
			if found && json.Unmarshal(record.Value, &held) != nil {
				// A SET THAT DOES NOT DECODE is none the job's captures
				// could read either: marked below, like a set never written.
				held = handedSet{}
			}
			switch {
			case held.Launch == run.LaunchID && held.Sealed == "":
				// MARKED ALREADY, or kept empty: every capture of the job
				// is refused, so the token adds nothing to keep.
				return nil, nil
			case held.Launch == run.LaunchID:
				// THE JOB'S OWN PART, written back with a later build's keys
				// (wirekeys.go): a renewal changes only the seal and the mark.
				next, why := add(held.Sealed)
				if next == held.Sealed {
					return nil, nil
				}
				held.Sealed, held.Incomplete = next, ""
				if next == "" {
					held.Incomplete = why
				}
			case held.LaunchSeq > run.LaunchSeq:
				// A LATER JOB'S SET: this job is over, whatever the row read said.
				later = true
				return nil, nil
			default:
				// NONE FOR THE JOB (sealed none, or its write did not land) is
				// MARKED as the job's, refusing every capture, so a late write
				// of the launch's own set cannot land without this token.
				held = handedSet{Launch: run.LaunchID, LaunchSeq: run.LaunchSeq, Incomplete: handedUnkept}
			}
			wrote = true
			return json.Marshal(held)
		})
	switch {
	case err != nil:
		return false, fmt.Errorf("sandbox: record what run %s's box is handed: %w", run.TurnID, err)
	case later, wrote && !changed:
		// A LATER JOB'S SET, or a run that ended under the write (undone).
		return false, nil
	}
	return true, nil
}

// MarkBoxPaused stamps the box as paused, with the instant the pause TTL runs
// from.
func (s *CoordStore) MarkBoxPaused(ctx context.Context, turnID string, at time.Time) error {
	_, _, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if at.IsZero() {
			at = s.clock()
		}
		run.PausedAt = at
		return true
	})
	return err
}

// ReleaseBox forgets the box a run was using.
func (s *CoordStore) ReleaseBox(ctx context.Context, turnID string) error {
	_, _, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		// Together: a paused_at naming no box is a paused box the reaper seeks
		// every tick and never finds. The box's date goes with it.
		run.SandboxID, run.CommandID, run.PausedAt = "", "", time.Time{}
		run.BoxSince = time.Time{}
		return true
	})
	return err
}

// AppendBridgeCall makes one step of a bridged tool call ([AppendStep]): its
// entry written ahead, its one update, or the withdrawal of a call not run.
//
// NO FENCE, deliberately: this is a LOG APPEND of a call that already ran, and
// refusing it because the seat moved would lose true evidence (the log's
// version guards the write). Each step is one compare-and-swap on the log's own
// fields. After a record lands the job's spend (own job only, [BridgeLog.take])
// and the keys of what the turn wrote ([PendingRun.WroteKeys]) are written, and
// a late or abandoned call is handed to the log's job, never failing the append
// ([CoordStore.fileLateItems]). THE ONE WRITER of a call's steps and spend. A
// gone log, a stale-round begin or withdrawal, or a jobless append is dropped,
// no error (see [PendingStore]); a late call is recorded with [ErrJobReplaced].
func (s *CoordStore) AppendBridgeCall(ctx context.Context, turnID string, a BridgeAppend) (bool, error) {
	if !a.Step.Valid() {
		return false, fmt.Errorf("sandbox: %q is no step of a bridged call", a.Step)
	}
	if a.Launch == "" || a.Call.Name == "" {
		return false, nil
	}
	stamped := func(call BridgeCall) BridgeCall {
		if call.At.IsZero() {
			call.At = s.clock()
		}
		return call
	}
	// SET ON EVERY ASK OF decide, which reruns on a lost race: what the last
	// attempt's log takes the append for, whether it wrote, the log decided on
	// and whether its written set first refused a record ([BridgeLog.WroteMore]).
	taken, wrote, filled := takeNone, false, false
	var logged BridgeLog
	_, won, err := s.runs.MutateSandboxRunPart(ctx, turnID, coord.RunLog, func(current coord.Record, found bool) ([]byte, error) {
		taken, wrote, filled, logged = takeNone, false, false, BridgeLog{}
		if !found {
			return nil, nil
		}
		if err := json.Unmarshal(current.Value, &logged); err != nil {
			return nil, fmt.Errorf("sandbox: decode the log of run %s: %w", turnID, err)
		}
		full := logged.WroteMore
		var refused error
		if taken, wrote, refused = logged.take(a, stamped); refused != nil || !wrote {
			return nil, refused
		}
		filled = logged.WroteMore && !full
		return json.Marshal(logged)
	})
	if won && filled {
		// ONCE A RUN, by the write that marked the set: the mark rides the log.
		log.WarnContext(ctx, "sandbox_run_written_set_full", "turn_id", turnID, "launch_id", a.Launch,
			"held", len(logged.Wrote), "detail", "the run's calls wrote more records than its log keeps "+
				"(types.MaxWrittenRecords); a wake about one past them waits for a turn of its own")
	}
	switch {
	case err != nil:
		// Includes a begin with no room ([ErrCallsInFlight]): nothing written.
		return false, err
	case taken == takeNone, wrote && !won:
		// NO LOG OPEN FOR THE JOB OR ROUND, or the run's end removed it under
		// the write (undone): a gone run, or a begin or withdrawal of a round
		// that is over.
		return false, nil
	case a.Step != AppendRecord:
		// A begin or withdrawal: no spend rides it, nor anything it wrote.
		return true, nil
	case taken != takeOwn:
		// A LATE CALL, in the log and paid by nobody: the row holds a later job,
		// whose resume would pay it as its own. Said, so a caller asking again
		// for an earlier attempt's spend stops. What it wrote reaches the row's
		// keys in a write of its own, as a late item is filed. HANDED TO THE JOB
		// THE LOG IS OPEN TO as the log lists it, which files it once however
		// often asked.
		if len(a.Wrote) > 0 {
			s.keepWrittenKeys(ctx, turnID, logged)
		}
		listed, ok := logged.listedAs(a.Call)
		if taken == takeEarlier {
			listed, ok = logged.listedEarlierAs(a.Call)
		}
		if ok {
			s.fileLateItems(ctx, turnID, logged.Launch, logged.Round, []BridgeCall{listed})
		}
		return true, ErrJobReplaced
	}
	// Onto THIS job's record, only while the row holds it: the log's opening
	// follows the reset naming the next job ([CoordStore.BeginLaunch]), so an
	// append the log still takes as its own may find the row moved on, its spend
	// dropped with the job. Written whether or not this append moved the log: an
	// append asked again may be the first whose spend reaches the record. IN THE
	// SAME WRITE, whatever job the row holds, the keys of the log's whole set.
	_, _, err = s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		var changed bool
		run.WroteKeys, changed = foldWrittenKeys(run.WroteKeys, logged.Wrote)
		if a.Launch != run.LaunchID {
			return changed
		}
		if newest := run.Launch.Bridged.Newest(a.Spent); newest != run.Launch.Bridged {
			run.Launch.Bridged, changed = newest, true
		}
		return changed
	})
	if err != nil {
		return true, fmt.Errorf("sandbox: record what run %s's bridged calls cost and wrote: %w", turnID, err)
	}
	// A CALL ITS HARNESS GAVE UP ON is handed to its job once its spend is
	// written: an error above stands, and the ask again files it.
	if listed, ok := logged.listedAs(a.Call); ok && listed.Abandoned {
		s.fileLateItems(ctx, turnID, a.Launch, logged.Round, []BridgeCall{listed})
	}
	return true, nil
}

// keepWrittenKeys folds the keys of what logged's set holds onto turnID's row
// ([PendingRun.WroteKeys]) in a write of its own, for a late call, whose spend
// rides no write. NEVER AN ERROR TO ITS CALLER, whose own write landed: said,
// and the next record of the log's own job, or of a late call that wrote,
// folds the whole set again.
func (s *CoordStore) keepWrittenKeys(ctx context.Context, turnID string, logged BridgeLog) {
	_, _, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		var changed bool
		run.WroteKeys, changed = foldWrittenKeys(run.WroteKeys, logged.Wrote)
		return changed
	})
	if err != nil {
		log.WarnContext(ctx, "sandbox_run_written_keys_unkept", "turn_id", turnID, "error", err.Error(),
			"detail", "the run's row lacks the keys of what a late call wrote until its next recorded call, so "+
				"another seat's run on that work is not shown it meanwhile")
	}
}

// logTake is what a run's log takes one bridged append for
// ([BridgeLog.take]).
type logTake int

const (
	// takeNone is an append the log takes nothing of: no job, a job the log is
	// not on and no round, or a begin or withdrawal of a round the log moved on
	// from (a call of a closed round never starts).
	takeNone logTake = iota
	// takeOwn is an append of the log's own job ([BridgeLog.Launch]): calls
	// listed, spend written onto the job's record.
	takeOwn
	// takeLate is an append of an earlier job of the log's round
	// ([BridgeLog.Round]): calls listed, marked late ([BridgeCall.Late]), spend
	// written nowhere.
	takeLate
	// takeEarlier is a RECORD of a job of an earlier round of the turn (an
	// earlier run under the same turn id included; [BridgeAppend.Round] set and
	// not the log's): a call in flight as its round's collection read the log,
	// landing after a later round's opening. Listed as a late call of this
	// round with NO PLACE ([BridgeCall.Seq] zero), its spend nobody's (the
	// package doc says why).
	takeEarlier
)

// take decides what l does with one bridged append, a, and does it: THE ONE
// DECISION, once per ask of the compare-and-swap ([CoordStore.AppendBridgeCall]):
// whether the log takes it, whether its call is late, whether its spend is the
// job's ([logTake]). Answers whether it wrote (an append asked again does not)
// and refuses a begin with no room ([ErrCallsInFlight]). BY THE LOG'S OWN job
// and round ([BridgeLog.opened]): a RECORD of an earlier round lands in the
// round after, late with no place ([takeEarlier]); an append naming no round is
// taken by its own job's log alone; a BEGIN is taken as a record is. BY THE
// STEP ([AppendStep]): a BEGIN writes the entry in flight ([keptEntry]), none
// past [MaxCallsInFlight]; a RECORD is the call's ONE UPDATE (entry out, call
// listed unless already), MARKED LATE by the log; a WITHDRAWAL drops the entry
// of a call not run.
func (l *BridgeLog) take(a BridgeAppend, stamp func(BridgeCall) BridgeCall) (logTake, bool, error) {
	if a.Launch == "" || a.Call.Name == "" {
		return takeNone, false, nil
	}
	var taken logTake
	switch {
	case a.Launch == l.Launch:
		taken = takeOwn
	case a.Round != "" && a.Round == l.Round:
		taken = takeLate
	case a.Round != "" && a.Step == AppendRecord:
		return takeEarlier, l.takeEarlier(a, stamp), nil
	default:
		return takeNone, false, nil
	}
	at := l.inFlightAt(a.Call)
	switch a.Step {
	case AppendBegin:
		if at >= 0 || l.lists(a.Call) {
			return taken, false, nil
		}
		if len(l.InFlight) >= MaxCallsInFlight {
			return taken, false, ErrCallsInFlight
		}
		l.InFlight = append(slices.Clip(l.InFlight), stamp(keptEntry(a.Call)))
		return taken, true, nil
	case AppendWithdraw:
		if at < 0 {
			return taken, false, nil
		}
		l.InFlight = slices.Delete(slices.Clone(l.InFlight), at, at+1)
		return taken, true, nil
	}
	listed := l.lists(a.Call)
	if at < 0 && listed {
		return taken, false, nil
	}
	if at >= 0 {
		l.InFlight = slices.Delete(slices.Clone(l.InFlight), at, at+1)
	}
	if !listed {
		call := a.Call
		call.Late = taken == takeLate
		// A FINISHED CALL IS NOBODY'S TO WAIT FOR: kept without its entry's
		// runner or bound ([keptCall]).
		l.appendCall(stamp(call), a.Absorbed)
	}
	l.wrote(a.Wrote)
	return taken, true, nil
}

// wrote folds records into the turn's written set ([BridgeLog.Wrote]), marking
// it full where one was refused for room; nothing it holds is dropped.
func (l *BridgeLog) wrote(records []types.WrittenRecord) {
	var full bool
	l.Wrote, full = types.FoldWritten(l.Wrote, records...)
	l.WroteMore = l.WroteMore || full
}

// takeEarlier lists a, a record of an earlier round ([takeEarlier]), as a late
// call of l's round with no place, absorbed calls folded in and the bridge's
// [BridgeCall.Abandoned] kept; reports whether it wrote (not where l lists it
// already). No entry lookup: an earlier round's entry went with its log.
func (l *BridgeLog) takeEarlier(a BridgeAppend, stamp func(BridgeCall) BridgeCall) bool {
	if l.listsEarlier(a.Call) {
		return false
	}
	call := a.Call
	call.Seq, call.Late = 0, true
	l.appendCall(stamp(call), a.Absorbed)
	l.wrote(a.Wrote)
	return true
}

// WatchLog follows the bridged tool log beside run turnID's record. See the
// contract on [PendingStore].
//
// THE FIRST STATE IS A READ FROM THE STREAM'S LEADER
// ([coord.SandboxRuns.SandboxRunPart]) once the part's watch is open, replacing
// the watch's own first state, which may lag by a replica's lag: all before the
// read is in it, all after reaches the watch, which delivers only newer states
// or the log gone. THE WATCH IS THIS CALL'S OWN, under a context it cancels on
// a failed read and when its goroutine ends, so nothing outlives it.
func (s *CoordStore) WatchLog(ctx context.Context, turnID string) (<-chan LogView, error) {
	ctx, stop := context.WithCancel(ctx)
	updates, err := s.runs.WatchSandboxRunPart(ctx, turnID, coord.RunLog)
	if err != nil {
		stop()
		return nil, fmt.Errorf("sandbox: watch the log of run %s: %w", turnID, err)
	}
	read, found, err := s.runs.SandboxRunPart(ctx, turnID, coord.RunLog)
	if err != nil {
		stop()
		return nil, fmt.Errorf("sandbox: read the log of run %s: %w", turnID, err)
	}
	out := make(chan LogView, 1)
	go func() {
		defer stop()
		defer close(out)
		var last uint64
		deliver := func(u coord.SandboxRunUpdate) bool {
			if u.Found && u.Record.Version <= last {
				return true
			}
			view := LogView{Found: u.Found, Version: u.Record.Version}
			if u.Found {
				if err := json.Unmarshal(u.Record.Value, &view.Log); err != nil {
					// A STATE THIS BUILD CANNOT READ is passed over, never
					// delivered as an empty log.
					log.WarnContext(ctx, "sandbox_log_view_unread", "turn_id", turnID, "error", err.Error())
					return true
				}
				last = u.Record.Version
			}
			// THE NEWEST STATE WINS, as the notes' watch has it ([CoordStore.WatchNotes]).
			select {
			case <-out:
			default:
			}
			select {
			case out <- view:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !deliver(coord.SandboxRunUpdate{Found: found, Record: read}) {
			return
		}
		first := true
		for u := range updates {
			if first {
				// THE WATCH'S OWN FIRST STATE, which the read above stands in for.
				first = false
				continue
			}
			if !deliver(u) {
				return
			}
		}
	}()
	return out, nil
}

// BridgeLog reads the bridged tool log of run's job. See the contract on
// [PendingStore].
func (s *CoordStore) BridgeLog(ctx context.Context, run PendingRun) (BridgeLog, error) {
	none := BridgeLog{Launch: run.LaunchID, Round: run.Round}
	record, found, err := s.runs.SandboxRunPart(ctx, run.TurnID, coord.RunLog)
	switch {
	case err != nil:
		return BridgeLog{}, fmt.Errorf("sandbox: read the log of run %s: %w", run.TurnID, err)
	case !found:
		return none, nil
	}
	var held BridgeLog
	if err := json.Unmarshal(record.Value, &held); err != nil {
		return BridgeLog{}, fmt.Errorf("sandbox: decode the log of run %s: %w", run.TurnID, err)
	}
	if held.Launch != run.LaunchID {
		return none, nil
	}
	return held, nil
}

// TurnLog reads the calls of run's turn as its job stands to hold them. See
// the contract on [PendingStore].
func (s *CoordStore) TurnLog(ctx context.Context, run PendingRun) (BridgeLog, error) {
	record, found, err := s.runs.SandboxRunPart(ctx, run.TurnID, coord.RunLog)
	switch {
	case err != nil:
		return BridgeLog{}, fmt.Errorf("sandbox: read the log of run %s: %w", run.TurnID, err)
	case !found:
		return BridgeLog{Launch: run.LaunchID, Round: run.Round}, nil
	}
	var held BridgeLog
	if err := json.Unmarshal(record.Value, &held); err != nil {
		return BridgeLog{}, fmt.Errorf("sandbox: decode the log of run %s: %w", run.TurnID, err)
	}
	// A call approved in this job, on the row but not yet in the log, is still
	// a call of the round.
	held = held.withHeld(run)
	// The log as the job's opening leaves it, where that has not landed: the
	// round's calls and the turn's earlier ones, never none.
	if held.Launch != run.LaunchID {
		held, _ = held.opened(run.LaunchID, logOpening{round: run.Round, answered: run.AnsweredJob,
			owed: run.OwedCall})
	}
	return held, nil
}

// RecordAsk records the question a running job asked. See the contract on
// [PendingStore].
//
// THROUGH THE LIVE WRITE, so an ended run is given no question it can never
// park on.
func (s *CoordStore) RecordAsk(ctx context.Context, turnID, launch string, ask Ask) (AskOutcome, error) {
	if launch == "" {
		return AskNotRunning, nil
	}
	if err := ask.Validate(); err != nil {
		return "", fmt.Errorf("sandbox: recording the question run %s asked: %w", turnID, err)
	}
	if ask.Held != nil {
		// Held to its share of the row ([MaxHeldArgsBytes]).
		if err := HeldArgsFit(ask.Held.Arguments); err != nil {
			return "", fmt.Errorf("sandbox: recording the call run %s held: %w", turnID, err)
		}
	}
	ask = ask.Redacted()
	// WHY NOT, per the last decision. Never "recorded": only the store's answer
	// says a write landed; a decision agreeing to write can lose to a writer
	// that removed the row, after which no decision runs.
	refusal, full := AskNotRunning, false
	_, won, err := s.mutateLive(ctx, turnID, func(run *PendingRun) bool {
		switch {
		case run.LaunchID != launch ||
			(run.Status != StatusLaunching && run.Status != StatusRunning):
			refusal = AskNotRunning
			return false
		case ask.Call != nil:
			// A PARK ON THE JOB'S OWN CALL IS NEVER REFUSED AS ASKED: it joins
			// what the job asked, whose resume then tells it, or is the job's
			// ask where it asked nothing (callwait.go).
			if full = len(run.ParkedCalls) >= MaxParkedCalls; full {
				return false
			}
			call := *ask.Call
			call.Ask, call.Job = !run.asked(), launch
			if call.Ask {
				run.Question = CallWaitQuestion(call.Tool)
			}
			run.ParkedCalls = append(slices.Clip(run.ParkedCalls), call)
			return true
		case run.asked():
			refusal = AskAlreadyAsked
			return false
		}
		run.Question, run.QuestionContext = ask.Question, ask.Context
		run.Decision, run.Audience = ask.Decision, ask.To
		run.WaitsFor = ask.Wait
		if ask.Held != nil {
			// A HELD CALL NAMES NO BRANCH (the run did not choose to stop): an
			// earlier question's branch stays for the job carrying on.
			held := *ask.Held
			held.LaunchID = launch
			run.HeldCall = &held
		} else {
			run.Branch = ask.Branch
		}
		refusal = AskNotRunning
		return true
	})
	switch {
	case errors.Is(err, ErrRunEnding):
		return AskNotRunning, nil
	case err != nil:
		return "", err
	case won:
		return AskRecorded, nil
	case full:
		return "", fmt.Errorf("%w (%d): run %s's call is not parked", ErrParkedCallsFull, MaxParkedCalls, turnID)
	}
	return refusal, nil
}

// appendBounded adds one call and drops from the MIDDLE past the cap, answering
// the dropped calls so their identities are kept ([BridgeLog.appendCall]). The
// start and end explain a run, so the middle goes; the count rides along, since
// a silent skip lies.
//
// ORDINARY CALLS GO FIRST. A call ending the run ([EndsRun]) is what the
// reviewer reads the outcome by. A call NO SAVE POINT HOLDS ([BridgeCall.unheld])
// is told by its mark, and once dropped keeps none: it is counted
// ([BridgeLog.LateElided]) and told as a count. So the oldest ordinary call
// after the head goes, never the one just made; with none left, the oldest of
// the others, so the log stays at its cap.
func appendBounded(calls []BridgeCall, elided int, next BridgeCall) ([]BridgeCall, int, []BridgeCall) {
	calls = append(calls, next)
	if len(calls) <= MaxBridgeCalls {
		return calls, elided, nil
	}
	head := MaxBridgeCalls / 2
	drop := len(calls) - MaxBridgeCalls
	// The candidates: everything after the head but the call just made.
	candidates := calls[head : len(calls)-1]
	// outlasts is a candidate the middle gives up only once no ordinary one
	// is left: a call that ended its job, or one no save point holds.
	outlasts := func(call BridgeCall) bool { return call.Ends() || call.unheld() }
	ordinary := 0
	for _, call := range candidates {
		if !outlasts(call) {
			ordinary++
		}
	}
	dropOrdinary := min(drop, ordinary)
	dropAny := drop - dropOrdinary
	kept := make([]BridgeCall, 0, MaxBridgeCalls)
	kept = append(kept, calls[:head]...)
	var dropped []BridgeCall
	for _, call := range candidates {
		switch {
		case !outlasts(call) && dropOrdinary > 0:
			dropOrdinary--
			dropped = append(dropped, call)
		case outlasts(call) && dropAny > 0:
			dropAny--
			dropped = append(dropped, call)
		default:
			kept = append(kept, call)
		}
	}
	kept = append(kept, next)
	return kept, elided + drop, dropped
}

// MarkSuspended writes the parked turn's state and opens the run to the
// completion poll. See the contract on [PendingStore].
//
// THROUGH THE LIVE WRITE, so a run ended under its launch is told [ErrRunEnding]
// rather than a plain false (see the contract).
func (s *CoordStore) MarkSuspended(ctx context.Context, turnID string, suspension Suspension) (bool, error) {
	if n := len(suspension.State); n > ExecuteStateBytes {
		return false, fmt.Errorf("sandbox: the parked state of run %s is %d bytes, and its share of "+
			"the row is %d: %w", turnID, n, ExecuteStateBytes, ErrStateTooLarge)
	}
	_, won, err := s.mutateLive(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusLaunching {
			return false
		}
		run.ExecuteState = bytes.Clone(suspension.State)
		run.Status = StatusRunning
		run.Launch.Iteration = suspension.Iteration
		return true
	})
	return won, err
}

// ListRuns returns every run the store holds, whatever its status. See the
// contract on [PendingStore].
func (s *CoordStore) ListRuns(ctx context.Context) ([]PendingRun, error) {
	return s.list(ctx, func(PendingRun) bool { return true })
}

// MarkSessionStart records which session a job's box said it started. See the
// contract on [PendingStore].
func (s *CoordStore) MarkSessionStart(ctx context.Context, turnID, launch string, start SessionStart) (bool, error) {
	if !start.Valid() {
		return false, fmt.Errorf("sandbox: %q is no session a job's box starts", start)
	}
	if launch == "" {
		return false, nil
	}
	_, marked, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launch || run.Launch.SessionStart != "" {
			return false
		}
		run.Launch.SessionStart = start
		return true
	})
	return marked, err
}

// MarkBridgeConnected records that a job's harness connected its bridge. See
// the contract on [PendingStore].
func (s *CoordStore) MarkBridgeConnected(ctx context.Context, turnID, launch string) (bool, error) {
	if launch == "" {
		return false, nil
	}
	_, marked, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launch || run.Launch.BridgeConnected {
			return false
		}
		run.Launch.BridgeConnected = true
		return true
	})
	return marked, err
}

// BindSession binds a minting job's reported session to its record. See the
// contract on [PendingStore].
func (s *CoordStore) BindSession(ctx context.Context, turnID string, bind SessionBind) (PendingRun, error) {
	if bind.Launch == "" || !ValidSessionID(bind.ID) {
		return PendingRun{}, fmt.Errorf("%w: %q is no session id a harness names, for job %q",
			ErrSessionBindRefused, bind.ID, bind.Launch)
	}
	// THE RECORD'S RULES FIRST, so a report it refuses claims nothing; the same
	// job again (a recovery, a reattach, a second save) is answered as bound,
	// its claim made again where none stands, asking the log nothing. Then THE
	// CLAIM, THE FRESH CHECK AND THE RECORD, in that order, so no second job
	// checks the log in the window before this one's claim; a claim this call
	// took is released where the record does not follow. One a crash leaves
	// goes with the record.
	run, found, err := s.Get(ctx, turnID)
	switch {
	case err != nil:
		return PendingRun{}, err
	case !found:
		return PendingRun{}, fmt.Errorf("%w: turn %s holds no record", ErrSessionBindRefused, turnID)
	}
	if refused := sessionBindRefusal(run, bind); refused != nil {
		return PendingRun{}, refused
	}
	if run.Launch.Minted == bind.ID {
		// BOUND ALREADY, by a write whose answer may have been lost: its claim
		// is made again where none stands, for the job the record binds.
		if err = s.reclaimBound(ctx, turnID, bind); err != nil {
			return PendingRun{}, err
		}
		return run, nil
	}
	took, err := s.claimSession(ctx, turnID, bind)
	if err != nil {
		return PendingRun{}, err
	}
	release := func() { s.undoClaim(context.WithoutCancel(ctx), turnID, bind, took) }
	if bind.Fresh != nil {
		if err = bind.Fresh(ctx, bind.ID); err != nil {
			release()
			return PendingRun{}, err
		}
	}
	var (
		refused error
		held    PendingRun // the record as decided, written or already bound
	)
	_, _, found, err = s.writeFound(ctx, turnID, false, Fence{}, func(run *PendingRun) bool {
		refused = sessionBindRefusal(*run, bind)
		bound := refused == nil && run.Launch.Minted == ""
		if bound {
			run.Launch.Minted = bind.ID
		}
		held = *run
		return bound
	})
	switch {
	case err != nil:
		// A WRITE THAT ERRED MAY HAVE LANDED: the claim stays where the record,
		// read again, binds the id (or cannot be read to say).
		if again, there, readErr := s.Get(context.WithoutCancel(ctx), turnID); readErr == nil &&
			(!there || again.LaunchID != bind.Launch || again.Launch.Minted != bind.ID) {
			release()
		}
		return PendingRun{}, err
	case !found:
		release()
		return PendingRun{}, fmt.Errorf("%w: turn %s holds no record", ErrSessionBindRefused, turnID)
	case refused != nil:
		release()
		return PendingRun{}, refused
	}
	return held, nil
}

// sessionClaimAttempts bounds a claim's races: each one lost is another job's
// claim or move of the same id, so a few settle it either way.
const sessionClaimAttempts = 4

// claimSession claims bind's session for its job ([coord.SessionClaims]): an
// id nobody claims, one its own job holds, one its turn's earlier job holds (a
// relaunch continuing it: the claim moves with the turn), and one a run with no
// record holds (a claim carries its record's lifetime) are its job's; one
// another live run holds is [ErrSessionBindRefused]. It reports what this call
// took, so a bind whose record does not follow undoes only that
// ([CoordStore.undoClaim]).
func (s *CoordStore) claimSession(ctx context.Context, turnID string, bind SessionBind) (claimTaken, error) {
	ours := coord.SessionClaim{Session: bind.ID, Turn: turnID, Launch: bind.Launch}
	for range sessionClaimAttempts {
		held, found, err := s.runs.SessionClaim(ctx, bind.ID)
		if err != nil {
			return claimTaken{}, fmt.Errorf("sandbox: reading the claim of session %q: %w", bind.ID, err)
		}
		if !found {
			created, createErr := s.runs.CreateSessionClaim(ctx, ours)
			if createErr != nil || created {
				return claimTaken{created: created}, createErr
			}
			continue
		}
		if held.Turn == turnID && held.Launch == bind.Launch {
			return claimTaken{}, nil
		}
		if held.Turn != turnID {
			if _, live, readErr := s.runs.SandboxRun(ctx, held.Turn); readErr != nil {
				return claimTaken{}, fmt.Errorf("sandbox: reading the run claiming session %q: %w", bind.ID,
					readErr)
			} else if live {
				return claimTaken{}, fmt.Errorf("%w: session %q is bound to turn %s's job %s",
					ErrSessionBindRefused, bind.ID, held.Turn, held.Launch)
			}
		}
		moved, err := s.runs.MoveSessionClaim(ctx, ours, held.Revision)
		if err != nil || moved {
			taken := claimTaken{}
			if moved {
				taken.movedFrom = &held
			}
			return taken, err
		}
	}
	return claimTaken{}, fmt.Errorf("sandbox: claiming session %q lost every race: %w", bind.ID,
		coord.ErrUnavailable)
}

// reclaimBound makes the claim of a session turnID's record binds already,
// for the job it binds, where none stands; one standing is left as it is.
func (s *CoordStore) reclaimBound(ctx context.Context, turnID string, bind SessionBind) error {
	_, found, err := s.runs.SessionClaim(ctx, bind.ID)
	if err == nil && !found {
		_, err = s.runs.CreateSessionClaim(ctx, coord.SessionClaim{Session: bind.ID, Turn: turnID,
			Launch: bind.Launch})
	}
	if err != nil {
		return fmt.Errorf("sandbox: claiming bound session %q again: %w", bind.ID, err)
	}
	return nil
}

// claimTaken is what a bind's claim did ([CoordStore.claimSession]): made the
// claim, or moved it from movedFrom (the turn's earlier job, or a run with no
// record), or neither (its own job held it already).
type claimTaken struct {
	created   bool
	movedFrom *coord.SessionClaim
}

// undoClaim undoes what a bind whose record did not follow took: a claim it
// made is removed, and one it moved goes back to its earlier owner, so a
// relaunch's failed bind never strips the session its turn continues of its
// claim. A claim no longer the bind's is left alone.
func (s *CoordStore) undoClaim(ctx context.Context, turnID string, bind SessionBind, taken claimTaken) {
	if !taken.created && taken.movedFrom == nil {
		return
	}
	held, found, err := s.runs.SessionClaim(ctx, bind.ID)
	if err != nil || !found || held.Turn != turnID || held.Launch != bind.Launch {
		return
	}
	if taken.created {
		_, _ = s.runs.DeleteSessionClaim(ctx, bind.ID, held.Revision)
		return
	}
	_, _ = s.runs.MoveSessionClaim(ctx, *taken.movedFrom, held.Revision)
}

// ReleaseSessionClaim removes turnID's claim of a session. See the contract on
// [PendingStore].
func (s *CoordStore) ReleaseSessionClaim(ctx context.Context, turnID, sessionID string) error {
	return s.dropSessionClaim(ctx, turnID, sessionID)
}

// dropSessionClaim removes turnID's claim of sessionID, nil where turnID holds
// none; one a failure leaves goes with the record ([coord.SessionClaims]).
func (s *CoordStore) dropSessionClaim(ctx context.Context, turnID, sessionID string) error {
	held, found, err := s.runs.SessionClaim(ctx, sessionID)
	switch {
	case err != nil:
		return fmt.Errorf("sandbox: reading the claim of session %q: %w", sessionID, err)
	case !found || held.Turn != turnID:
		return nil
	}
	if _, err := s.runs.DeleteSessionClaim(ctx, sessionID, held.Revision); err != nil {
		return fmt.Errorf("sandbox: releasing the claim of session %q: %w", sessionID, err)
	}
	return nil
}

// sessionBindRefusal is why run's record will not bind bind's report, nil where
// it binds it or holds it bound already.
func sessionBindRefusal(run PendingRun, bind SessionBind) error {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrSessionBindRefused}, args...)...)
	}
	switch {
	case run.LaunchID != bind.Launch:
		return refuse("the record holds job %q, not %q", run.LaunchID, bind.Launch)
	case run.Launch.Minted == bind.ID:
		return nil
	case run.Launch.Minted != "":
		return refuse("job %s bound session %q already, and a job binds one, once", bind.Launch,
			run.Launch.Minted)
	case bind.Continued && bind.ID != run.SessionID:
		return refuse("job %s continued session %q and reported %q", bind.Launch, run.SessionID, bind.ID)
	case !bind.Continued && run.SessionID != "" && bind.ID == run.SessionID:
		return refuse("job %s started a session of its own and reported the one it was asked to "+
			"continue, %q", bind.Launch, bind.ID)
	}
	return nil
}

// MarkEndedAtDeadline records that a job's collection saw it still working past
// its turn's cap and is ending it. See the contract on [PendingStore].
func (s *CoordStore) MarkEndedAtDeadline(ctx context.Context, turnID, launch string) (bool, error) {
	if launch == "" {
		return false, nil
	}
	_, marked, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launch || run.Launch.EndedAtDeadline {
			return false
		}
		run.Launch.EndedAtDeadline = true
		return true
	})
	return marked, err
}

// MarkHarnessEnd records how a job's harness ended and what its newest request
// carried, as a read that collected the job found them. See the contract on
// [PendingStore].
//
// A WRITE WHERE ANY CHANGES: an end already recorded beside a new measure,
// count, provider, writer or next-request estimate is still one to record,
// which testing the end alone would drop.
func (s *CoordStore) MarkHarnessEnd(ctx context.Context, turnID, launch string, read EndRead) (bool, error) {
	if !read.End.Valid() {
		return false, fmt.Errorf("sandbox: %q is no way a job's harness ends; it is one of %s",
			read.End, words.Or(HarnessEnds))
	}
	if launch == "" {
		return false, nil
	}
	_, marked, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launch {
			return false
		}
		next, measure := run.Launch.HarnessEnd.after(read.End), run.Launch.measured(read.Request, launch)
		count, provider, release := run.Launch.counted(read.Count, read.CountProvider, read.CountRelease)
		estimate := run.Launch.nextRequested(read.Next)
		if next == run.Launch.HarnessEnd && measure.same(run.Launch.Context) &&
			count.same(run.Launch.ResumedCount) && provider == run.Launch.CountProvider &&
			release == run.Launch.CountRelease && estimate == run.Launch.NextRequest() {
			return false
		}
		run.Launch.HarnessEnd, run.Launch.Context, run.Launch.ResumedCount = next, measure, count
		run.Launch.CountProvider, run.Launch.CountRelease = provider, release
		run.Launch.setNextRequest(estimate)
		return true
	})
	return marked, err
}

// MarkResumeBegun records what a claim's resumed turn does with its job's
// round. See the contract on [PendingStore].
//
// THROUGH THE LIVE WRITE ([CoordStore.mutate]): an ending run begins no turn.
func (s *CoordStore) MarkResumeBegun(ctx context.Context, turnID, launch string, fence Fence,
	carry ResumeCarry) (bool, error) {

	if !carry.Valid() {
		return false, fmt.Errorf("sandbox: %q is nothing a resumed turn does with its round; it is one of %s",
			carry, words.Or(ResumeCarries))
	}
	_, marked, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusResumed || run.LaunchID != launch || outranked(*run, fence) ||
			run.Launch.ResumeCarry == carry {
			return false
		}
		run.Launch.ResumeCarry = carry
		return true
	})
	return marked, err
}

// ListActive returns every run that has not finished.
func (s *CoordStore) ListActive(ctx context.Context) ([]PendingRun, error) {
	return s.list(ctx, func(r PendingRun) bool { return slices.Contains(Active, r.Status) })
}

// OwnerEpochs returns, for each seat, the highest lease epoch any of its run
// records carries, ended runs included: the floor the seat's fencing counter
// must stay above so its next holder is never older than a row it must write
// ([coord/kv.Epochs.Floor] raises it at boot). A row naming no seat or lease
// carries none. Read off the runs' STANDING, never their records, but for a
// record stating none (an older build's write), read whole; an undecodable
// one is skipped ([CoordStore.list]).
func (s *CoordStore) OwnerEpochs(ctx context.Context) (map[uuid.UUID]int64, error) {
	stood, err := s.runs.SandboxRunStandings(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox: list the runs' standing: %w", err)
	}
	out := map[uuid.UUID]int64{}
	for _, r := range stood {
		standing := r.Standing
		if standing == nil {
			run, ok, readErr := s.readStood(ctx, r.TurnID)
			if readErr != nil {
				return nil, readErr
			}
			if !ok {
				continue
			}
			whole := standingOf(run)
			standing = &whole
		}
		if standing.Seat == uuid.Nil || standing.OwnerEpoch <= 0 {
			continue
		}
		out[standing.Seat] = max(out[standing.Seat], standing.OwnerEpoch)
	}
	return out, nil
}

// standingOf is what run's record states of itself beside its value
// ([coord.RunStanding]): every write of a record states it.
func standingOf(run PendingRun) coord.RunStanding {
	return coord.StandingOf(seatOf(run.AgentID), run.AgentHandle, run.OwnerEpoch)
}

// listStood returns the unfinished runs whose standing keep selects, and
// every run stating none, read whole and held to match, oldest first: one
// seat's runs, never every record ([CoordStore.list]'s rules otherwise).
func (s *CoordStore) listStood(ctx context.Context, keep func(coord.RunStanding) bool,
	match func(PendingRun) bool) ([]PendingRun, error) {

	stood, err := s.runs.SandboxRunStandings(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox: list the runs' standing: %w", err)
	}
	var out []PendingRun
	for _, r := range stood {
		if r.Standing != nil && !keep(*r.Standing) {
			continue
		}
		run, ok, readErr := s.readStood(ctx, r.TurnID)
		if readErr != nil {
			return nil, readErr
		}
		if ok && match(run) && slices.Contains(Active, run.Status) {
			out = append(out, run)
		}
	}
	sortRuns(out)
	return out, nil
}

// readStood reads one run a standing listing named, false where it is gone
// since or does not decode (said as [CoordStore.list] says it).
func (s *CoordStore) readStood(ctx context.Context, turnID string) (PendingRun, bool, error) {
	record, found, err := s.runs.SandboxRun(ctx, turnID)
	if err != nil {
		return PendingRun{}, false, fmt.Errorf("sandbox: read run %s: %w", turnID, err)
	}
	if !found {
		return PendingRun{}, false, nil
	}
	run, ok := s.decodedOrSkipped(ctx, record)
	return run, ok, nil
}

// ListActiveForSeat returns one seat's unfinished runs: the read a seat's new
// owner makes inside on_acquire, and why this store is the fleet's. Selected
// on the runs' standing, so only this seat's records are read whole.
func (s *CoordStore) ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error) {
	return s.listStood(ctx, func(st coord.RunStanding) bool { return st.Names(handle) },
		func(r PendingRun) bool { return r.AgentHandle == handle })
}

// ListActiveForSeatID returns one seat's unfinished runs by the seat's id: the
// read a mailbox retirement makes. See the contract on [PendingStore].
//
// Through [seatOf], the parse a run's control and inbox subjects use, so the
// runs a retirement ends are exactly those on the subscriptions it deletes;
// selected on the runs' standing, which states that parse.
func (s *CoordStore) ListActiveForSeatID(ctx context.Context, seat uuid.UUID) ([]PendingRun, error) {
	if seat == uuid.Nil {
		return nil, errors.New("sandbox: a seat's runs are listed by its id, and none was given")
	}
	return s.listStood(ctx, func(st coord.RunStanding) bool { return st.Seat == seat },
		func(r PendingRun) bool { return seatOf(r.AgentID) == seat })
}

// Finish deletes the record of a run whose ending is decided, once it owes the
// seat nothing. See the contract on [PendingStore].
//
// A read-decide-delete under the record's version, like every flip here, so an
// in-between write (a let-go, a clear) is seen and the delete decided again.
func (s *CoordStore) Finish(ctx context.Context, turnID, ending string) (PendingRun, bool, error) {
	var (
		run     PendingRun
		refused error // the decision's own error, returned as it is
	)
	_, gone, err := s.runs.MutateSandboxRun(ctx, turnID, func(record coord.Record) (coord.SandboxRunChange, error) {
		decoded, err := decodeRun(record)
		if err != nil {
			refused = err
			return coord.SandboxRunChange{}, err
		}
		run = decoded
		switch {
		case run.Ending == nil || run.Ending.ID != ending:
			return coord.SandboxRunChange{}, nil
		case len(run.HandBack) > 0:
			refused = fmt.Errorf("sandbox: finish run %s: %w", turnID, ErrHandBackOwed)
			return coord.SandboxRunChange{}, refused
		case run.answerOwed():
			refused = fmt.Errorf("sandbox: finish run %s: %w", turnID, ErrAnswerOwed)
			return coord.SandboxRunChange{}, refused
		case len(run.OwedEnd) > 0:
			refused = fmt.Errorf("sandbox: finish run %s: %w", turnID, ErrEndOwed)
			return coord.SandboxRunChange{}, refused
		}
		return coord.SandboxRunChange{Delete: true}, nil
	})
	switch {
	case err != nil && refused != nil && errors.Is(err, refused):
		// The row rides an owed refusal so the ending can hand back or record
		// what it owes and finish again.
		if errors.Is(err, ErrHandBackOwed) || errors.Is(err, ErrAnswerOwed) || errors.Is(err, ErrEndOwed) {
			return run, false, err
		}
		return PendingRun{}, false, err
	case err != nil:
		return PendingRun{}, false, fmt.Errorf("sandbox: finish run %s: %w", turnID, err)
	case !gone:
		return PendingRun{}, false, nil
	}
	if id := run.Launch.Minted; id != "" {
		_ = s.dropSessionClaim(ctx, turnID, id)
	}
	return run, true, nil
}

// read decodes one record and the version it was read at.
func (s *CoordStore) read(ctx context.Context, turnID string) (_ PendingRun, _ uint64, _ bool, err error) {
	defer origin.Stamp(&err)
	record, found, err := s.runs.SandboxRun(ctx, turnID)
	if err != nil {
		return PendingRun{}, 0, false, fmt.Errorf("sandbox: read run %s: %w", turnID, err)
	}
	if !found {
		return PendingRun{}, 0, false, nil
	}
	run, err := decodeRun(record)
	if err != nil {
		return PendingRun{}, 0, false, err
	}
	return run, record.Version, true, nil
}

// mutate is the read-decide-write every conditional flip of a LIVE run runs
// through: a run whose ending is decided ([PendingRun.Ending]) is refused before
// decide is asked, so only the ending's own steps move it (see [RecordedEnding]:
// that is what makes an ending announceable before its delete).
//
// decide returns whether to write. FALSE IS NOT A FAILURE: the condition did
// not hold (a claim somebody won, an outranked fence, a pause the reaper is too
// late for, an ending run). A run that does not exist is the same non-answer.
func (s *CoordStore) mutate(ctx context.Context, turnID string, decide func(*PendingRun) bool) (PendingRun, bool, error) {
	run, won, err := s.mutateLive(ctx, turnID, decide)
	if errors.Is(err, ErrRunEnding) {
		return PendingRun{}, false, nil
	}
	return run, won, err
}

// mutateLive is [CoordStore.mutate] for a write whose caller must hear the
// ending refused it (a park, a status move: work follows that assumes it
// landed). The refusal is [ErrRunEnding].
func (s *CoordStore) mutateLive(ctx context.Context, turnID string, decide func(*PendingRun) bool) (PendingRun, bool, error) {
	return s.write(ctx, turnID, true, Fence{}, decide)
}

// mutateFenced is [CoordStore.mutateLive] for a launch's own writes (its
// relaunch and its attach), which carry the launching lease: a row a newer
// lease owns refuses them with [ErrSeatNotHeld], whether or not its ending is
// decided too.
//
// THE LEASE IS ASKED FIRST. The seat's next holder fences a claimed run to its
// own lease before ending it ([Coordinator.RecoverSeat]), so a relaunch on the
// node that lost the seat usually met an ending as well as the newer lease and
// was told [ErrRunEnding] (its own turn closing) rather than the true cause, a
// lost seat. A launching run is ended without fencing, so an attach meets that
// ending or no record, never this refusal. A zero fence asks nothing
// ([outranked]).
func (s *CoordStore) mutateFenced(ctx context.Context, turnID string, fence Fence,
	decide func(*PendingRun) bool) (PendingRun, bool, error) {

	return s.write(ctx, turnID, true, fence, decide)
}

// mutateAny is [CoordStore.mutate] for the writes a run whose ending is decided
// still takes: the ending's own steps (its decision, its let-go, the clear of
// what it handed back), and the facts that are true whoever holds the row (a
// lease stamped on it, a bridged call it made).
func (s *CoordStore) mutateAny(ctx context.Context, turnID string, decide func(*PendingRun) bool) (PendingRun, bool, error) {
	return s.write(ctx, turnID, false, Fence{}, decide)
}

// write is the read-decide-write itself. live refuses a run whose ending is
// decided, with [ErrRunEnding], and a fence a newer lease outranks refuses the
// write before that, with [ErrSeatNotHeld].
//
// THE STORE RUNS IT ([coord.SandboxRuns.MutateSandboxRun]), and on a lost race
// RE-READS AND RE-DECIDES rather than rewriting what was computed: the other
// writer may have taken the claim, moved the lease, unparked the run or decided
// its ending. So decide may run more than once, on a fresh copy each time, and
// the answer is the last run's.
func (s *CoordStore) write(ctx context.Context, turnID string, live bool, fence Fence,
	decide func(*PendingRun) bool,
) (PendingRun, bool, error) {
	next, won, _, err := s.writeFound(ctx, turnID, live, fence, decide)
	return next, won, err
}

// writeFound is [CoordStore.write], also answering whether the LAST attempt
// found the record: decide is not asked about a gone record, so a caller keeping
// an earlier attempt's decision (before a write removed the record) reads this
// to know it is not the answer.
func (s *CoordStore) writeFound(ctx context.Context, turnID string, live bool, fence Fence,
	decide func(*PendingRun) bool,
) (PendingRun, bool, bool, error) {
	var (
		next    PendingRun
		refused error // the decision's own error, returned as it is
	)
	last, won, err := s.runs.MutateSandboxRun(ctx, turnID, func(record coord.Record) (coord.SandboxRunChange, error) {
		run, err := decodeRun(record)
		if err != nil {
			refused = err
			return coord.SandboxRunChange{}, err
		}
		if outranked(run, fence) {
			refused = fmt.Errorf("sandbox: run %s is held by a newer lease (epoch %d) than "+
				"this write's (%d): %w", turnID, run.OwnerEpoch, fence.Epoch, ErrSeatNotHeld)
			return coord.SandboxRunChange{}, refused
		}
		if live && run.Ending != nil {
			refused = fmt.Errorf("sandbox: run %s: %w", turnID, ErrRunEnding)
			return coord.SandboxRunChange{}, refused
		}
		next = run
		if !decide(&next) {
			return coord.SandboxRunChange{}, nil
		}
		next.UpdatedAt = s.clock()
		raw, err := encodeRun(next)
		if err != nil {
			refused = err
			return coord.SandboxRunChange{}, err
		}
		standing := standingOf(next)
		return coord.SandboxRunChange{Put: raw, Standing: &standing}, nil
	})
	found := last.Key != ""
	switch {
	case err != nil && refused != nil && errors.Is(err, refused):
		return PendingRun{}, false, true, err
	case err != nil:
		return PendingRun{}, false, found, fmt.Errorf("sandbox: update run %s: %w", turnID, err)
	case !won:
		return PendingRun{}, false, found, nil
	}
	return next, true, true, nil
}

// list decodes every record and returns the ones that match, oldest first.
//
// The filters are the caller's (coordination cannot see the fields they read);
// the set is bounded by the seats mid-run, so one read and a local filter fit.
//
// A record that does not decode is SKIPPED, never the listing failed: no filter
// can match it and no write can touch it (every write decodes first), while
// failing the listing failed every reader of every seat's runs (boot epoch
// floor, seat recovery, the waiter's poll, retirement, the dashboard). It stays
// for the documented repair, said once per process
// ([CoordStore.decodedOrSkipped], [undecodableReport]). A listing failing as a
// whole is still an error.
func (s *CoordStore) list(ctx context.Context, match func(PendingRun) bool) ([]PendingRun, error) {
	records, err := s.runs.SandboxRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox: list runs: %w", err)
	}
	var out []PendingRun
	for _, record := range records {
		run, ok := s.decodedOrSkipped(ctx, record)
		if !ok {
			continue
		}
		if match(run) {
			out = append(out, run)
		}
	}
	sortRuns(out)
	return out, nil
}

// sortRuns orders runs oldest first, so recovery works runs in launch order; a
// listing reordering every boot would make one failure look like two.
func sortRuns(runs []PendingRun) {
	slices.SortStableFunc(runs, func(a, b PendingRun) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.TurnID, b.TurnID))
	})
}

// superseded reports whether a newer lease than the fence owns the run, the
// zero fence included on a row any lease owns. It is [outranked] for writes
// only the seat's holder makes (a claim, taking or recording an answer), where
// a leaseless writer on a leased row lost the seat, not a recovery yet to take one.
func superseded(run PendingRun, fence Fence) bool {
	return run.OwnerEpoch > fence.Epoch
}

// outranked reports whether a fence has been overtaken by a newer lease.
//
// A zero fence constrains nothing, deliberately: recovery and the boot pass
// hold no lease yet. What must never happen is writing under a lease LOST.
func outranked(run PendingRun, fence Fence) bool {
	return fence.Fenced() && run.OwnerEpoch > fence.Epoch
}

// encodeRun writes a run back whole, unknown keys included (top level from
// [PendingRun.Extra], sections' own, wirekeys.go) under [marshalCarrying]'s
// rule: declared fields win. Its colleague's ask is written as stored
// ([storedRun]), never as served.
func encodeRun(run PendingRun) ([]byte, error) {
	raw, err := marshalCarrying(storedRunOf(run), pendingRunKeys(), run.Extra)
	if err != nil {
		return nil, fmt.Errorf("sandbox: encode run %s: %w", run.TurnID, err)
	}
	return raw, nil
}

// decodedOrSkipped decodes a record a listing returned, or reports it as one no
// reader can act on and answers false: the one place a listing decides that
// ([CoordStore.list]).
func (s *CoordStore) decodedOrSkipped(ctx context.Context, record coord.Record) (PendingRun, bool) {
	run, err := decodeRun(record)
	if err != nil {
		report := s.undecodable
		if report == nil {
			report = undecodableRuns
		}
		report.skipped(ctx, record.Key, err)
		return PendingRun{}, false
	}
	return run, true
}

// undecodableRuns is the process's report of skipped run records, shared by
// every store it opens.
var undecodableRuns = &undecodableReport{log: log}

// undecodableReport says each skipped record once.
//
// AT ERROR THE FIRST TIME A PROCESS SEES IT, AT DEBUG AFTER. Runs are listed on
// every waiter poll, seat recount, reply lookup and dashboard home load, so an
// error per listing was a line every few seconds for as long as the record
// stays (on the embedded broker until something removes it). One per process
// tells an operator the record, why and the repair at each boot; the debug
// repetition serves whoever traces a listing. The set holds only keys seen,
// bounded by the bucket's records.
type undecodableReport struct {
	log  *slog.Logger
	mu   sync.Mutex
	seen map[string]struct{}
}

func (u *undecodableReport) skipped(ctx context.Context, key string, err error) {
	u.mu.Lock()
	_, again := u.seen[key]
	if !again {
		if u.seen == nil {
			u.seen = map[string]struct{}{}
		}
		u.seen[key] = struct{}{}
	}
	u.mu.Unlock()
	level := slog.LevelError
	if again {
		level = slog.LevelDebug
	}
	u.log.Log(ctx, level, "sandbox_run_undecodable", "key", key, "error", err,
		"detail", "this run record cannot be read, so no listing returns it: no recovery, "+
			"poll, answer, retirement or epoch floor acts on it, and no write can change it",
		"remedy", "remove the record from the sandbox runs bucket once you know its run "+
			"is over (see the deployment guide)")
}

// decodeRun reads a run and KEEPS what it cannot read (see
// [PendingRun.Extra] for the rolling upgrade that loses it otherwise). Its
// colleague's ask is read as stored ([storedRun]).
func decodeRun(record coord.Record) (_ PendingRun, err error) {
	defer origin.Stamp(&err)
	var stored storedRun
	extra, err := unmarshalCarrying(record.Value, &stored, pendingRunKeys())
	if err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: decode run %s: %w", record.Key, err)
	}
	run := stored.run()
	run.Extra = extra
	run.forgetOtherJobs()
	// A null state is NO state: the struct writes null for a run with none, and
	// a raw field reads it back as four bytes, which every `len(...) == 0` test
	// of "is a conversation parked here?" would answer wrongly.
	if bytes.Equal(bytes.TrimSpace(run.ExecuteState), []byte("null")) {
		run.ExecuteState = nil
	}
	// The KEY is the identity, not the field: a body disagreeing with its key
	// would hand a caller a run it cannot write back.
	run.TurnID = record.Key
	return run, nil
}

// pendingRunKeys is every wire key [PendingRun] declares ([wireKeys]).
func pendingRunKeys() map[string]bool { return wireKeys(reflect.TypeFor[PendingRun]()) }
