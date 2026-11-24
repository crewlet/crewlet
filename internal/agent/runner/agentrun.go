package runner

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// The executor is its harness's own run in a box: its loop, shell and edits.
// Its tools reach it over the MCP bridge, through the SAME tools.Surface every
// engine-driven phase calls; the engine keeps the rest, refusing to fork it:
//   - the PROMPT (identity, chart, memory, skills, workers), built once here;
//   - the SURFACE and its skill guard: a tool denied to the seat is denied it;
//   - the SUBMISSION (submit_work over the bridge), its outcomes and rescue;
//   - the REVIEWER, the engine's own loop: it is not the thing reviewed.
//
// The run outlives its turn, so the phase returns suspended and the collection
// re-enters the turn, maybe on another node. No conversation is carried: only
// [execstate.State] and the context the turn froze at its start.

// AgentLauncher starts the executor as its harness's own agentic run.
//
// Declared by the consumer, and narrow on purpose: the runner knows what the
// executor should be asked to do and which tools it may use; the engine knows
// what a detached run is, which member of the seat's executor chain takes it,
// where it goes and how to reach it. Neither has to learn the other's half.
type AgentLauncher interface {
	// LaunchExecutor starts the run and returns once it is going, naming
	// the job it opened. It does NOT wait for it: the phase suspends and the
	// run's collection re-enters the turn later.
	//
	// An error fails the phase: there is no other way to run the executor,
	// and the error says why this run could not start. It still names the
	// job where the launch had opened one before it failed — see
	// [Launched.LaunchID].
	LaunchExecutor(ctx context.Context, req AgentRunRequest) (Launched, error)
}

// Launched is what a launch answers.
type Launched struct {
	// LaunchID is the JOB the launch opened — the name the run's record
	// holds it under, which only the launch learns — so the notes its brief
	// carried are recorded delivered to that job.
	//
	// SET ON AN ERROR TOO, where the launch failed after the write that
	// opened the job: that write recorded the brief's notes read by the job
	// ([AgentRunRequest.Notes]), and the record, whose ending reports every
	// note no job ran with, answers for them from then on — so they are not
	// handed back to the turn's source ([SteerSource.Return]), which would
	// report them a second time. Empty with an error is a launch that wrote
	// nothing, whose notes are the turn's still.
	LaunchID string

	// BridgeFrom is the last call the run's record held when the job's
	// session over the bridge opened — the round's calls before it, which
	// the job numbers its own after — recorded on the parked state
	// ([execstate.State.BridgeFrom]) so every later rebuild of the run's
	// guard replays the loads of this job's own session and of no other
	// ([Runner.rebuiltSurface]).
	BridgeFrom int
}

// AgentRunRequest is one executor handed to its harness's agent loop.
//
// THE PROMPT ARRIVES IN TWO HALVES, because the harnesses take it in two: Claude
// Code appends a file to its own system prompt, OpenCode adds an instruction
// file to its default agent's. Both keep the vendor's harness prompt — the
// part that teaches its shell, editor and loop, which the run is FOR — and put
// the seat's standing instructions on the system side of the conversation
// beneath it. Joined into one document, as they once were, the seat's
// identity, policies and memory reached the model as the first user message,
// which a model weighs as a request rather than as who it is.
type AgentRunRequest struct {
	// System is the executor's system prompt: the seat's standing
	// instructions.
	System string

	// Brief is the turn's own ask — with its earlier rounds, every note a
	// person sent the turn that it has already read, and then Notes — and
	// the whole of what the harness is prompted with; of a round carried on
	// ([AgentRunRequest.Continue]), what became of the run before it and every
	// call of the round so far too.
	Brief string

	// Notes are the notes this brief is the FIRST to read, in the order the
	// brief renders them ([SteerSource.Brief]): the launch records them read
	// by the job it opens, in the write that opens it, so the job's own harness
	// never takes them again.
	Notes []steer.Note

	// Retold is every note the turn has read whose read the run's record
	// marked unanswered as the brief was built (steer.Reading.Unanswered: a
	// take recorded it read and its answer never reached its harness), by id
	// ([Runner.retold]). EVERY BRIEF THE LAUNCH MAY HAND ITS RUN TELLS EACH OF
	// THEM AGAIN: the fresh one among every note the turn has read, and a
	// continued session's, its job's own copy's included, from the first such
	// read on ([Runner.firstUnheld], [Runner.inPlaceNotes]). So the launch
	// records each read by the job it opens, told again, at its take's own
	// place among the reads (sandbox.NoteRead.Retold), and the turn's ending
	// says it was delivered there where the job started. None of them is
	// among Notes.
	Retold []string

	// Surface is what the run may call, bridged; see the package rule
	// above.
	Surface *tools.Surface

	// Skills are the Tool Skills the run's harness is handed as its own
	// (handedskills.go): written into its box, listed to its model, and
	// loaded with its own skill tool — each load its harness reports
	// recorded on the run's bridge session as the run's load_tool_skill
	// call of the skill's key ([HandedSkills.Listed]). Nil hands none.
	Skills HandedSkills

	// Spend is what the calls Surface serves cost the engine, counted
	// apart from this segment because they happen after it — see
	// [BridgedSpend]. The launcher records its running total with every
	// bridged call, for the segment that resumes from the run to pay.
	Spend *BridgedSpend

	// Round is the turn iteration this executor is, so the run's record and
	// the resumed phase agree about which round they are in.
	Round int

	// OnFallback publishes a launch-time hand-off against THIS turn: the
	// launch walks the seat's executor chain for a member that can take the
	// run ([chain.Pick]), and each member it skips is a provider_fallback
	// like a call's. The launcher has no publisher of its own and must not
	// grow one — which turn, phase and iteration a hand-off belongs to is
	// the runner's to say — so the runner hands it this, bound to the
	// launch. Never nil.
	OnFallback func(chain.Fallback)

	// Continue is set when this launch CARRIES ON a run that parked on a
	// person's question, now answered, or whose job was lost with its host —
	// the same round, relaunched: the asking run's harness session, and the
	// prompt that continues it, which the launch uses where it runs in the
	// asking run's box (a lost job's session is never continued, and names
	// none). Brief is then what a fresh run is told instead, the question and
	// the answer, or how the run before it was cut off, included (see [Asked]
	// and [Lost]), and every call of the round so far
	// ([prompts.CalledBefore]). Nil for every other launch.
	Continue *Continuation

	// BriefRead is the read of the round's log every brief of the launch was
	// built from ([Resume.BridgedRead]), nil on a turn's first launch, which no
	// read precedes. Its opening marks late every call of the round the log
	// took after it, or carries them late into the new round a send-back
	// starts (sandbox.LaunchRequest.BriefRead): calls of the job before that
	// landed while the relaunch was readied, which no brief told. Every
	// relaunch hands it, a send-back's as much as one carrying the round on.
	BriefRead *sandbox.LogRead

	// Session is the turn's kept session the launch continues, in whatever box
	// it gets — the session of the job the relaunch follows, as the launcher
	// said it would before the run's briefs were written ([SessionProber]) —
	// with the briefs that continue it: in the box it was last worked in, and
	// in any other. Set on every relaunch whose job before left a kept
	// session that can be continued: a reviewer's send-back as much as a
	// round carried on. Nil starts a session of its own, or continues one in
	// place ([AgentRunRequest.Continue]); and a launch that cannot take it up
	// after all — the object unreadable, the box's harness too old, the
	// seat's budget unable to carry it — starts fresh with Brief.
	Session *SessionContinuation

	// FreshOwn is the leads Brief opened with of its own where Continue is
	// nil: a turn's first launch and a send-back, whose fresh brief opens a
	// session of the run's own with the turn's task (freshLead). The launch
	// keeps them whichever brief it hands its run (sandbox.LaunchRequest.FreshOwn).
	FreshOwn []sandbox.Lead

	// Decided is told how the launch decided its run takes up a session —
	// the turn's kept capture, the job before's own copy in its box, or none
	// ([SessionDecision]) — once the launch has its box and before the run
	// starts there: the runner arms the run's guard with the skills that
	// session holds, and publishes the prompt it was handed. A launcher calls
	// it once per launch that reaches its run's start, and never for one that
	// does not.
	Decided func(SessionDecision)

	// Started is told which session the job's box started, once its harness
	// first reaches the seat's tools and before any call of the run is made
	// there: the session the launch decided to continue (continued), or one
	// of its own its script started in that one's place, its restore having
	// failed in the box. The box says it by the token its harness dials
	// (internal/api/mcpbridge, started.go), which its model cannot forge. The
	// runner holds the run's guard to that session, and where it is one of
	// its own, publishes the fresh brief the run was handed in the
	// continuation's place ([Runner.sessionStarted]). A launcher calls it
	// after Decided, at most once per opening of the job's tools; a box whose
	// harness never reaches them never says, and its run makes no call.
	Started func(continued bool)

	// Fence is the turn's own fence ([Config.Fence]), which the launch asks
	// again immediately before it opens anything: the prompt built since
	// the phase opened can take a model call of its own, and a seat that
	// moved or was stopped meanwhile must not still buy a box. THE SAME
	// CLOSURE, never one the launcher builds: a fence compares the grant it
	// was built under with the one held when it is called, so it is the
	// turn's admission that the launch is held to. Nil where the runner was
	// built without one.
	Fence func() error
}

// Continuation is how a launch carries on an asking run — see
// [AgentRunRequest.Continue].
type Continuation struct {
	// SessionID is the asking run's harness session; Brief its next prompt,
	// which carries it on. Where the job's harness is not known to have ended
	// its run, Brief says the session was or may have been cut off and tells
	// every call of the round and note the turn has read. Where it did, Brief
	// says a successful submission among the calls the copy holds does not
	// stand, and tells the reads its brief counted without listing
	// ([Resume.Owed]), the late calls ([Runner.inPlaceAfter]) and the notes
	// from a read no session of its job holds on. Either way it tells
	// [AgentRunRequest.Notes], a served one as a note it may already show
	// ([Runner.inPlaceNotes]). The launch's floor counts all of it beside the
	// copy's measured context ([Continuation.ContextTokens]), else beside the
	// static floor (internal/engine's continuedFloor).
	SessionID string
	Brief     string

	// SignInReplaced is a launch carrying on a round whose job was refused
	// on a sign-in replaced while it ran ([Rotated]): the launch records it
	// on the new job, so that job is not carried on again for it.
	SignInReplaced bool

	// RequestRefused is a launch carrying on a round whose job's continued
	// session its provider refused ([Refused]): recorded on the new job, so
	// that job is not carried on again for it.
	RequestRefused bool

	// ContextTokens is what the last request of that session carried: the
	// larger of its newest capture's measure, kept or not
	// ([SessionBasis.CopyContext]), and the job's record's, which the read
	// that collected the job took off its run's own account of its requests,
	// or carried along the copy's lineage where the job made none
	// ([Resume.ContextTokens]). What the relaunch sends again before Brief
	// where it continues the session in its box, which its floor is made of.
	// Zero where neither measured it, which holds the launch to the static
	// floor with Brief beside it.
	ContextTokens int64

	// NextRequest is the harness's own estimate of that session's next
	// request: the job's record's, which the read that collected the job made
	// of the copy its box holds ([Resume.NextRequest]), where it holds a
	// figure, else its newest capture's ([SessionBasis.CopyNextRequest]).
	// What the relaunch's floor is made of where it holds a figure, in place
	// of ContextTokens, which under-counts it by the last request's reply and
	// everything after it, and over-counts it past a compaction no request
	// has followed; never the larger of the two, which would put that
	// compacted figure back.
	NextRequest sandbox.NextRequest

	// Told is what Brief told of the round, and Fresh what the launch's fresh
	// brief, [AgentRunRequest.Brief], told of it (every call of the round,
	// through [prompts.CalledBefore]), each by the reads it counted without
	// listing them, its read cap spent ([Runner.calledAfter]): the launch
	// records the set of the brief it hands its run, and of the fresh one its
	// box falls back to beside a brief that continues a session, on the job it
	// opens (sandbox.LaunchRecord.BriefOmitted, FreshOmitted), so the next
	// brief to that job's copy, or to a capture of it placed by its read, tells
	// them ([Resume.Owed], [SessionBasis.Owed]): a session holds what its brief
	// told, never what its budgets left out.
	Told, Fresh sandbox.BriefTold

	// Summarized is the copy's brief told as a session that may be a
	// summary, beside Brief, where the launch judges its first request in the
	// box ([firstRequest.byBox]), and SummarizedTold what it told of the
	// round: handed where the box's point does not prove Brief whole. ""
	// elsewhere. Count and CountProvider are the job's record's count of the
	// copy and its reply's provider ([Resume.Count]), which it is judged by.
	Summarized     string
	SummarizedTold sandbox.BriefTold
	Count          *sandbox.ResumedCount
	CountProvider  string
}

// inPlaceNextRequest is the estimate of the next request of the copy a job's
// box holds that a relaunch continuing it in place is held to
// ([Continuation.NextRequest]): record's, the read of the copy itself as its
// job ended, where it holds a figure, and else captured's, the job's newest
// capture's.
func inPlaceNextRequest(record, captured sandbox.NextRequest) sandbox.NextRequest {
	if record.Tokens > 0 {
		return record
	}
	return captured
}

// SessionDecision is how a launch decided its run takes up a session, told
// before the run starts ([AgentRunRequest.Decided]).
type SessionDecision struct {
	// Continues says the run continues a session rather than starting one
	// of its own; Kept that the session is the one the launch was asked to
	// continue in any box ([AgentRunRequest.Session]) rather than the job
	// before's own copy in its box ([AgentRunRequest.Continue]); and NewBox
	// that the box is not the one the session was last worked in, so the run
	// is handed the continuation that says so ([SessionContinuation.Replaced]).
	Continues, Kept, NewBox bool

	// Base says the session is the one the conversation's last turn left
	// ([SessionBasis.Base]) — always in a new box, and never one whose
	// skills this turn's guard holds, so the guard is armed empty.
	Base bool

	// Exact says the brief the launch handed is the one told by a reading
	// of what the kept session holds ([SessionContinuation.Exact]): its
	// box's harness answered the release the reading was read for, so the
	// run reads that telling's brief and holds what that telling held.
	// Unheld says it is the one for a box whose release the resume was not
	// read in ([SessionContinuation.Unheld]), whose telling the run reads
	// in turn. Both false where the launch handed the brief told by the
	// save point, or continues no kept session.
	Exact, Unheld bool

	// Summarized says the brief the launch handed is the one told as a
	// session that may be a summary ([SessionContinuation.Summarized],
	// [Continuation.Summarized]): the box's point did not prove the held one
	// whole, so the run's guard arms nothing.
	Summarized bool
}

// SessionContinuation is how a launch continues a session in whatever box it
// gets — the turn's kept capture, or the session its conversation's last turn
// left — see [AgentRunRequest.Session].
type SessionContinuation struct {
	// Basis is the session the launch continues, as the launcher answered it
	// ([SessionProber]); the launch takes up exactly that one.
	Basis SessionBasis

	// Brief is the run's prompt in the box the session was last worked in,
	// and Replaced in any other — Brief and what a box that replaced that
	// one changes (continued.go). A session the conversation's last turn
	// left is continued only in a box of its own, so its Brief is empty.
	Brief, Replaced string

	// Told is what Brief and Replaced told of the round the launch carries
	// on, which is the same for both: the reads they counted without listing
	// them ([Continuation.Told]). Of the session its conversation's last turn
	// left, the fresh brief's, which Replaced holds whole.
	Told sandbox.BriefTold

	// Exact is the briefs told by a reading of what the kept capture holds
	// ([HoldingsAsker]), beside Brief and Replaced, which tell every call
	// after the session's save point; nil where no reading was read, or its
	// telling is not to be handed. Only for a kept capture of the turn's
	// own, never for a Base: a reading is read only of a capture whose
	// harness was cut off ([SessionBasis.MayLackBrief]). The launch hands
	// one of them only to a box whose harness answers the release the
	// reading was read for ([ExactTelling.Resumer]), and Brief or Replaced
	// to every other box, and drops it alone where the seat's budget cannot
	// carry it but carries the rest (internal/engine's continuedFloor).
	Exact *ExactTelling

	// Summarized is the briefs told as a session that may be a summary,
	// beside Brief and Replaced, where the launch judges the first request
	// in its box ([firstRequest.byBox]): handed where that box's point does
	// not prove the brief it would hand sends the session whole. Never
	// dropped alone: the floor counts it as it counts Brief. Count and
	// CountProvider are what the launch judges by ([keptCount]).
	Summarized    *SummarizedTelling
	Count         *sandbox.ResumedCount
	CountProvider string

	// Unheld is the briefs for a box whose release the resume was not read
	// in, beside Brief and Replaced; nil where they would tell the same
	// ([UnheldTelling]). The launch hands one of them only to such a box.
	Unheld *UnheldTelling

	// UnheldDropped says the seat's budget could not carry Unheld's briefs,
	// which internal/engine's continuedFloor dropped alone on its copy: the
	// launch continues the session only in a box whose release the resume
	// was read in. The runner never sets it.
	UnheldDropped bool
}

// SummarizedTelling is a kept path's briefs told as a session that may be a
// summary ([SessionContinuation.Summarized]): Brief for the box the session
// was last worked in, Replaced for any other, and what they told of the round.
type SummarizedTelling struct {
	Brief, Replaced string
	Told            sandbox.BriefTold
}

// UnheldTelling is a relaunch's briefs for a box whose harness answers a
// release whose resume this build did not read (internal/sandbox/executor's
// claudeResumeRead): a read at a hook's step is confirmed by what the job's
// session showed, as a release the reading was read for resumes it, so such a
// box holds none of them. Told as [SessionContinuation]'s briefs are, but the
// notes from the first read at a hook's step on, as notes the session may
// already show ([Runner.firstUnheld]), and every late call a hook took, which
// they credit no telling of ([Telling.Hook]). Built only where they differ.
type UnheldTelling struct {
	Brief, Replaced string

	// Told is what Brief and Replaced told of the round, as
	// [SessionContinuation.Told] is theirs.
	Told sandbox.BriefTold
}

// ExactTelling is a relaunch's briefs told by a reading of what the kept
// session it continues holds ([HoldingsAsker]): told only the calls of the
// round the reading does not find the session holding, where the briefs
// beside them ([SessionContinuation.Brief], [SessionContinuation.Replaced])
// tell every call after the session's save point.
//
// READ FOR THE RELEASE THAT WROTE THE SESSION ([SessionBasis.Release]), the
// best word for the release that resumes it, since the box is acquired after
// the briefs are written: these briefs go only to a box whose harness answers
// exactly Resumer (internal/sandbox's continueKept); every other box gets the
// save-point briefs, which tell more and never less.
type ExactTelling struct {
	// Resumer is the release the reading was read for
	// (sandbox.HoldingsAsk.Resumer), "2.1.310": the one a box's harness must
	// answer for Brief or Replaced to be handed to it. "" matches no box.
	Resumer string

	// Brief is the run's prompt in the box the session was last worked in,
	// and Replaced in any other, as [SessionContinuation]'s are; "" where
	// the telling wrote none for that box, which hands the brief told by the
	// save point there.
	Brief, Replaced string

	// Told is what Brief and Replaced told of the round, the reads they
	// counted without listing them ([Continuation.Told]): recorded on the
	// job the launch opens in place of [SessionContinuation.Told] where it
	// hands either.
	Told sandbox.BriefTold
}

// SessionBasis is the kept session a relaunch of the turn continues, or why it
// continues none — a [SessionProber]'s answer.
type SessionBasis struct {
	// LaunchSeq and CaptureSeq are the capture's place among the turn's,
	// zero where the relaunch continues none; SessionID is its harness's
	// session.
	LaunchSeq, CaptureSeq int64
	SessionID             string

	// Harness is the harness that wrote the session, the only one that
	// continues it: a launch hands the continuation on only where the member
	// it picks as it launches runs it, and starts a session of its own where
	// that member runs another.
	Harness string

	// Entry is the providers.llm entry the session ran under, as the turn's
	// row on the executor log records it (sandbox.SessionArchive.Entry): a
	// launch hands the continuation on under another entry only where its
	// harness resumes on its launch's route (sandbox.EntryContinues). ""
	// where the row recorded none, which no other entry matches.
	Entry string

	// Release is the release of Harness that wrote the session, as the
	// turn's row on the executor log records it (the `--version` its job's
	// box answered before its harness started): what a reading of what a
	// cut-off session holds is read for ([HoldingsAsker],
	// [ExactTelling.Resumer]), the relaunch's best word for the release
	// that resumes it. "" for a base and where the row recorded none, which
	// no reading is read for.
	Release string

	// Through and NoteCursor are what the session accounts for, so the brief
	// that continues it carries what came after. Through is the save point:
	// how many of the round's calls the log had taken at the read the capture
	// was placed by, or an earlier capture's where the harness may not have
	// written every call (sandbox.SessionCapture.Through); later calls
	// ([BridgedCall.Appended] past it) may be unanswered. Zero holds no call.
	// It is the one test ([heldAt]); a reading of a cut-off session's archive
	// may find more, by call key ([HoldingsAsker]), and its brief omits those.
	// NoteCursor is the last note the record says it holds, read only where the
	// harness ended its run ([Runner.keptSession]); it stops before the first
	// read no session of the job holds, and again by carried reads marked after
	// the capture was stamped ([Runner.notesHeldBy]).
	Through    int
	NoteCursor int

	// Owed is the reads of the round at or below Through that the session
	// holds only as its job's brief's count, by their places in the round's
	// log (sandbox.SessionCapture.Omitted): that brief counted them without
	// listing them, its read cap spent, so the session was never handed what
	// they returned. Held at no point ([heldAt]), and told by the brief that
	// continues the session ([Runner.keptSession]). Nil where the brief left
	// none out, or the capture states none, an older build's.
	Owed []int

	// LateOwed is the late calls that brief counted without listing them
	// (sandbox.SessionCapture.LateOmitted), apart from Owed: every brief tells
	// them again, and the session, holding only that count of each, is
	// credited no telling of one ([heldByTelling]).
	LateOwed []int

	// Interrupted says the session was captured from a harness that was cut
	// off, so its transcript may end mid-call, and may lack the brief its job
	// was handed and the notes its job read.
	Interrupted bool

	// WentOn says the kept capture the basis continues was taken while its
	// job still worked (sandbox.SessionCapture.Final unset): its job's harness
	// went on in its box past it, so the session ends where it was saved, not
	// where its run stopped, and CapturedAt is when it was saved
	// (sandbox.SessionCapture.At), zero where the capture states none. Told so
	// in place of the cut-off words ([prompts.SavedSession], [Runner.keptSession]):
	// told only that it was cut off, the run takes the session's end for where
	// its work stopped. Such a capture is always cut off as well (Interrupted),
	// so how its calls, leads and notes are told does not move. The names and
	// shape of [BaseBasis.WentOn] and CapturedAt, so both bases say "the run
	// went on past the session above" alike; false and zero, which say
	// nothing, on a basis that continues no kept capture of the turn's.
	WentOn     bool
	CapturedAt time.Time

	// Home is where the session was last worked: absolute paths in its
	// transcript are under it.
	Home string

	// ContextTokens is what the last request of the kept capture the basis
	// continues carried, as that capture measured it
	// (sandbox.SessionCapture.ContextTokens): what a launch that restores it
	// sends first, in whatever box it gets, which the floor of the session it
	// was asked to continue in any box is made of (internal/engine's
	// keptFloor). The capture's OWN, as Count, Owed and Interrupted are, and
	// as [BaseBasis.ContextTokens] is the base's: a session restored from a
	// capture sends what that capture holds, never what its job's copy went
	// on to carry past it ([SessionBasis.CopyContext]). Zero where the basis
	// continues no capture of the turn's, or the capture measured none.
	ContextTokens int64

	// NextRequest is the harness's own estimate of the next request of the
	// same kept capture's session, as its take read it
	// (sandbox.SessionCapture.NextRequest): what that launch's floor is made
	// of where it holds a figure, in place of ContextTokens. The capture's
	// OWN, as ContextTokens is, never a newer capture's of its job it was
	// continued past ([SessionBasis.CopyNextRequest]). Zero where the basis
	// continues no capture of the turn's, or the capture estimated none.
	NextRequest sandbox.NextRequest

	// CopyContext is what the last request of the session of the job the
	// relaunch follows carried, as that job's newest capture measured it,
	// KEPT OR NOT, so set where the basis continues no kept capture too: the
	// job's own copy in its box, which went as far as that newest capture
	// and maybe further, and which a launch that carries the round on in that
	// box sends first. Read only for the floor of that in-place session,
	// beside what the job's record measured ([Continuation.ContextTokens]);
	// never for a kept capture's, which is its own
	// ([SessionBasis.ContextTokens]). Zero where no capture of that job
	// measured any.
	CopyContext int64

	// CopyNextRequest is the harness's own estimate of the next request of
	// that same copy, as the same newest capture's take read it, kept or not
	// (sandbox.ContinuedSession.CopyNextRequest): the in-place floor's figure
	// where the job's record holds none ([Continuation.NextRequest]); never a
	// kept capture's ([SessionBasis.NextRequest]). Zero where that capture
	// estimated none.
	CopyNextRequest sandbox.NextRequest

	// Count is what the session's harness counts the kept capture's session
	// at before its next request, as the take read its archive whole
	// (sandbox.SessionCapture.ResumedCount): what the kept path's first
	// request is judged by ([firstRequest], [keptCount]). Nil where the
	// capture counted nothing, which reads as a session that may compact.
	// CountProvider is the provider of the reply an OpenCode count is
	// anchored on (sandbox.SessionCapture.CountProvider), "" for any other.
	Count         *sandbox.ResumedCount
	CountProvider string

	// Window is what the seat's executor chain may hold a run to:
	// the least standard window over its members whose harness compacts by
	// its window, on each one's route, since the launch picks its member
	// only as it launches; Known only where every one is known
	// ([sandbox.ExecutorWindow.Least]). What every path's first request is
	// judged under ([firstRequest]), lowered where the copy's run reported a
	// smaller window. Set on every basis the launcher answers, whether or not
	// it names a session; zero, not Known, for a launcher that cannot say,
	// which reads as a session that may compact.
	Window sandbox.ExecutorWindow

	// Traits is what the relaunch reads of each harness a session it may
	// continue was written by ([sandbox.SessionTraits]), by name: the kept
	// session's, and the job's whose copy it continues in place. Set on every
	// basis the launcher answers; a harness it lacks is judged as none.
	Traits map[string]sandbox.SessionTraits

	// Base is the session the conversation's last turn left, which the launch
	// continues where it names no kept capture of the turn's own: on a
	// turn's first launch, the head its dispatch read, and on a relaunch of a
	// turn that has kept nothing yet, the one its first launch froze. Nil
	// where it continues none. SessionID and Harness are then that session's.
	Base *BaseBasis

	// Why is why the launch continues no kept session of the turn's own, ""
	// where it continues one; beside a Base, why the turn's own is not it.
	Why string

	// KeptTurn is the turn whose kept session the probe read, continued or
	// not: the turn's own where a capture of it is kept, else the session
	// its conversation's last turn left where the turn read one — on a
	// relaunch, where the turn's row was read holding it; empty where none
	// is kept, the probe read nothing, or what it read may be gone. A run
	// that starts a session of its own beside it is told it may read it
	// ([prompts.KeptSession]), which its model cannot otherwise know — a
	// round lost a second time too, which continues none.
	KeptTurn string
}

// Continues reports whether the basis names a kept session of the turn's own to
// continue.
func (b SessionBasis) Continues() bool { return b.CaptureSeq > 0 }

// ContinuesBase reports whether the basis names, in place of a kept session of
// the turn's own, the session its conversation's last turn left.
func (b SessionBasis) ContinuesBase() bool { return b.CaptureSeq == 0 && b.Base != nil }

// MayLackBrief reports whether the kept session the basis names may lack the
// brief its job was handed by the capture's own word: a capture its harness was
// cut off in ([SessionBasis.Interrupted]), which is not known to hold that
// brief. THE RECORD'S HALF of what the kept path's telling may count held:
// every telling of the path reads the one value decided from it, a restart in
// the capture's job and whether its harness may compact it beside it
// ([sessionHolds]), since that verdict needs the brief this is asked before.
func (b SessionBasis) MayLackBrief() bool { return b.Interrupted }

// BaseBasis is the session a conversation's last turn left, as a launch that
// continues it tells its run: the ended turn it is, where that session was last
// worked, whether its harness was cut off when it was saved, what its last
// request carried — which the launch's floor is made of — and when that turn
// ended, which is where the new turn begins; where that turn's end said it went
// on past the session (WentOn), when the session was kept, which is where the
// session above ends instead; and whether the turn was lost (Lost), so the
// session may not hold the last of what it did.
type BaseBasis struct {
	Turn          string
	Home          string
	Interrupted   bool
	ContextTokens int64
	EndedAt       time.Time
	CapturedAt    time.Time
	WentOn        bool
	Lost          bool
}

// SessionProber is an [AgentLauncher] that says, before a launch writes its
// briefs, which session it may continue: on a relaunch, the kept capture of the
// job it follows where a runner the launch can pick restores it; else, and on
// a first launch, the session the conversation's last turn left; or why
// neither. The member picked and the budget's room are the launch's to decide
// once the briefs exist.
//
// ASKED FIRST, by every launch, because a continued session is told what it
// lacks rather than everything. OPTIONAL: without it no session is continued
// and every launch starts fresh, or in place.
type SessionProber interface {
	ProbeSession(ctx context.Context) SessionBasis
}

// HoldingsAsker is an [AgentLauncher] that reads which of a round's calls the
// cut-off kept session a relaunch continues holds (internal/sandbox's
// HoldingsOf), off the capture basis names, through the runner of the member
// that drives basis.Harness, for ask's resumer: a relaunch that continues such
// a session is then told only the calls the session lacks ([ExactTelling]).
//
// A VALUE ONLY, NEVER AN ERROR: the zero value is no reading
// (sandbox.SessionHoldings), which the launcher logs with its reason, and the
// relaunch is then told every call after the session's save point, which is
// never wrong. OPTIONAL: a launcher without it is asked nothing, and its
// relaunches are told every call after the save point.
type HoldingsAsker interface {
	SessionHoldings(ctx context.Context, basis SessionBasis, ask sandbox.HoldingsAsk) sandbox.SessionHoldings
}

// executeAsAgentRun launches the executor as a detached agentic run.
//
// It builds the surface and prompt, hands them to the launcher, and parks. The
// recorded state carries no conversation ([execstate.State]) but all a resume
// needs: the round, the ledger, the usable surface, the loaded skills.
//
// A LAUNCH THAT DOES NOT HAPPEN STILL CLOSES ITS PHASE. The started record is
// published before the launch with the fresh brief; where a relaunch continues
// a session, its own brief follows as the opening frame once the launch has
// chosen it ([Runner.sessionDecided]). A launch refused after that publishes
// the completion too (failed, or stopped by a person), or the phase reads as
// running for ever.
func (r *Runner) executeAsAgentRun(ctx context.Context, round int, notes string,
	history []ledger.Iteration,
) (_ turn.Work, _ turn.Surface, err error) {
	defer origin.Stamp(&err)
	snapshot := r.executorUniverse(r.seatSnapshot())

	// The submission tool is on the surface because it is how the executor
	// says what it did. The harness calls it over the bridge, and the value it
	// captures here is NEVER READ: every collection, in this process or
	// another, replays submit_work from the run's durable bridged-call log
	// into a fresh decoder ([Runner.resumeAgentRun]). What this tool decides
	// is only what the run is told, and whether the log records the call as
	// failed, which the replay skips.
	var surface *tools.Surface
	submit := structured.NewContext(SubmitWorkTool, submitWorkDescription, workSchema,
		typedPickup(r.cfg.Reply, r.pickupAnswer, keptWhole(makingWork(r.cfg.Reply,
			func() []ledger.Call { return turn.Record(history, calls(surface)) },
			func() turn.Surface { return describe(surface) }))))

	// ITS OWN METER: every call this surface serves is the harness's, made
	// over the bridge after this segment has suspended and charged what it
	// spent — see [BridgedSpend].
	bridged := NewBridgedSpend()

	// THE NOTES THIS BRIEF IS THE FIRST TO READ, after every one the turn
	// read before — and the launch's from here: handed back on every way out
	// that reaches no run's record ([Runner.steerReturn]), and recorded
	// delivered only once the launch has opened its job ([Runner.steerBriefed]).
	fresh := r.steerBrief(ctx)
	user := r.executorBrief(ctx, notes, history, fresh)
	// AND THE TURN'S ANSWERS, of a reviewer's send-back, in a segment that
	// collected the turn's last run ([Runner.turnsAnswers]): a run that starts
	// a session of its own on this brief holds none of the briefs before it,
	// and its closed rounds' ledger carries none of a person's replies, so an
	// answer that carried an earlier round on, an instruction to the turn,
	// would be lost to it. After the turn's own brief, as a section of its own;
	// and the base's continuation takes it whole below. Never on a turn's
	// first launch, which nothing has carried on yet.
	user = user.Then(prompts.KeyAnswersBefore,
		r.turnsAnswers(round, history, r.cfg.Turn.Context.FenceBoundary()).section.Text)

	// EVERY LAUNCH ASKS WHICH SESSION IT CONTINUES (continued.go): a
	// relaunch — a reviewer's send-back, in a segment that collected the
	// turn's last run — the session of the job it follows where its launcher
	// says it can, else the one the conversation's last turn left where the
	// turn has kept none; and a turn's first launch the one its conversation's
	// last turn left. Asked before the continuation is written, which is built
	// from what that session lacks. THE SURFACE'S GUARD STARTS EMPTY either
	// way: it holds what the session the launch decides its run takes up
	// holds, and the launch decides that once it has its box
	// ([Runner.sessionDecided]).
	basis := r.sessionBasis(ctx)
	// AND THE RUNS IN FLIGHT AROUND IT, read once for its brief and for
	// whether its surface offers the tools that reach them.
	around := r.runsAround(ctx)
	user = user.Then(prompts.KeyRunsAround, prompts.RunsAround(around, r.cfg.Turn.Context.FenceBoundary()))
	built, err := r.surfaceWith(ctx, r.meterFor(bridged), phase.Execute, round, snapshot, submit,
		append(executorActive(snapshot, r.boxServers()), r.runTools(around)...))
	if err != nil {
		r.steerReturn(ctx, Launched{}, fresh)
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built
	briefs := launchBriefs{fresh: user, freshReported: true}
	switch {
	case basis.Continues() && r.cfg.Resume != nil:
		// THE COLLECTED ROUND'S CALLS, the ones its log dropped from the
		// middle among them: the session continued is that round's job's.
		collected := roundCalls{of: r.cfg.Resume.State.Round, bridged: r.cfg.Resume.Bridged,
			unfinished: r.cfg.Resume.Unfinished, elided: r.cfg.Resume.Elided,
			lateElided: r.cfg.Resume.LateElided, absorbed: r.cfg.Resume.Absorbed}
		// A SEND-BACK'S LEAD TELLS NO HELD CALL, so every call a person
		// approved after the save is told among the rest, but one a lead it
		// tells again tells ([Runner.keptSession]).
		// ITS LEAD IS THE REVIEW, which the run keeps beside its record as the
		// review of the round it closed, for a later brief to a session that
		// may lack this one to tell again.
		// AND JUDGED ON THAT LEAD, known before the budget is asked: where
		// its harness may hand the session to its model as a summary, the
		// telling of a session that may be one is handed ([firstRequest]).
		briefs.kept, briefs.keptHolds, briefs.exact, briefs.unheldHolds = r.keptTellings(ctx, basis, collected,
			round, nil, history, built, fresh, "").led(ctx, r, r.sendBackLead(notes).Text, reviewLead(history))
		// THE COLLECTED JOB'S STATE AS ITS SESSION HOLDS IT ([Resume.parked]):
		// the session this continues is that job's, and holds the skills its
		// launch armed only where its box said it took them up.
		briefs.state, briefs.bridged = r.cfg.Resume.parked(), r.cfg.Resume.Bridged
	case basis.ContinuesBase():
		// THE SESSION THE CONVERSATION'S LAST TURN LEFT, told where this turn
		// begins and then handed this launch's own fresh brief — the turn's
		// task, its earlier rounds and notes, and how the conversation's
		// earlier turns ended — which that session never read.
		// ITS LEAD IS THE TURN'S TASK, which the run keeps beside its record
		// with the closed rounds this brief rendered and the review it carried,
		// for a later brief to a session that may lack this one to tell again,
		// with the turn's answers this brief tells after it, or is to tell where
		// its read of them failed ([Runner.taskAgain]).
		briefs.kept = r.baseSession(basis, user.Text, sandbox.BriefTold{Own: taskLead(round, history, notes)})
	}
	// AND THE FRESH BRIEF NAMES THE KEPT SESSION IT DOES NOT CONTINUE, after
	// the base's continuation took it whole: a run that starts a session of
	// its own cannot otherwise know one it could read is there.
	user = r.namingKeptSession(user, basis, built)
	briefs.fresh = user
	system := r.executorSystem(built)
	// ITS TOOL SKILLS, handed to its harness as the harness's own rather
	// than listed in its prompt (handedskills.go), matched against the
	// surface it is launched with.
	handed := r.handSkills(ctx, built)
	// AND ASKED AGAIN once the brief exists, before anything is on the
	// record: assembling it rewrites the prior rounds' payloads past their
	// budgets with the seat's auxiliary model — and the calls after a
	// continued session's save ([Runner.keptSession]) — charged to the same
	// counters, and the rewrite can be what fills the window. Asked only at
	// the top of the phase, that run was launched and paid for whole past the
	// ceiling.
	if refused := toolloop.Refusal(ctx, r.cfg.Budget); refused != nil {
		r.steerReturn(ctx, Launched{}, fresh)
		return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: %s: %w", phase.Execute, refused)
	}
	// THE PROMPT REACHES THE RECORD, published here because the collection's
	// record carries no prompt: otherwise an operator sees a finished run and
	// not what it was asked.
	//
	// The surface travels too; its definitions are NOT in the prompt but reach
	// the harness over the MCP bridge, billed to its model. See
	// [emitter.promptSize].
	//
	// ONE ROUND OF ONE, the cap the collected record states too: the engine's
	// exchange is this launch and its answer; the harness's own turns are
	// bounded by `max_turns`. See [agentRunResult].
	emit := r.emitter()
	began := time.Now()
	caps := roundCaps{max: runRound, ceiling: runRound}
	emit.started(ctx, phase.Execute, round, system, user, surface, caps)
	decided := &launchDecided{}

	launched, err := r.cfg.Launcher.LaunchExecutor(ctx, AgentRunRequest{
		System: system.Text,
		Brief:  user.Text,
		Notes:  fresh,
		Retold: r.retold(),
		// A SEND-BACK'S READ, the collection's this segment made, whose round
		// its opening carries the unread calls of into the new one; none on a
		// turn's first launch.
		BriefRead:  sendBackRead(r.cfg.Resume),
		Session:    briefs.kept,
		FreshOwn:   freshLead(round, history, notes),
		Decided:    r.sessionDecided(ctx, emit, round, system, surface, caps, briefs, decided),
		Started:    r.sessionStarted(ctx, emit, round, system, surface, caps, briefs, decided),
		Surface:    surface,
		Skills:     handed,
		Spend:      bridged,
		Round:      round,
		OnFallback: func(f chain.Fallback) { emit.fallback(ctx, phase.Execute, round, f) },
		Fence:      r.cfg.Fence,
	})
	if err != nil {
		r.steerReturn(ctx, launched, fresh)
		err = fmt.Errorf("runner: %s: launching the executor's run: %w", phase.Execute, err)
		// A PHASE A PERSON STOPPED DID NOT FAIL, exactly as for every phase
		// the engine drives: it carries the stop as its error and is not
		// marked failed, since a turn somebody deliberately ended is not one
		// that broke.
		emit.completed(ctx, phaseRecord{
			Phase: phase.Execute, Iteration: round, System: system, User: user,
			Available: surface.Active(),
			Elapsed:   time.Since(began),
			StartedAt: began.UTC(), Caps: caps,
			Failed: !turn.Stopped(err), Err: err,
		})
		return turn.Work{}, describe(surface), err
	}
	r.steerBriefed(ctx, round, launched.LaunchID, fresh)

	r.recordAgentSuspension(round, surface, history, began, time.Since(began), handed, launched.BridgeFrom,
		decided.skills())
	return turn.Work{Calls: calls(surface), Suspended: true}, describe(surface), nil
}

// sendBackRead is the read of the collected round's log a send-back's briefs
// were built from ([Resume.BridgedRead]), nil where no collection precedes the
// launch: a turn's first.
func sendBackRead(resume *Resume) *sandbox.LogRead {
	if resume == nil {
		return nil
	}
	read := resume.BridgedRead
	return &read
}

// continueAgentRun carries on a run that parked on a person's question, now
// answered ([Asked]), lost with its host ([Lost]), refused on a replaced
// sign-in ([Rotated]), or whose continued session was refused ([Refused]): the
// SAME round, relaunched rather than collected, continuing the followed job's
// session or else fresh. A round that may not be carried on ([Lost.Limit])
// closes here as failed and ends the turn ([turn.ErrPermanent]).
//
// The surface is the job's as it left it, its calls seeded into the call log
// ([Runner.rebuiltSurface]). ITS GUARD IS THE RELAUNCH'S: empty until the
// launch decides ([Runner.sessionDecided]); the approved call is made before
// that, under the asking session's guard. The round's launch time keeps
// accumulating from its first launch; no new phase opens.
func (r *Runner) continueAgentRun(ctx context.Context, history []ledger.Iteration,
	state execstate.State, carry carried, bridgedCalls []BridgedCall,
) (_ turn.Work, _ turn.Surface, err error) {
	defer origin.Stamp(&err)
	began := time.Now()
	// THE CALL A PARK ON ITS OWN CALL WAITED FOR, as the round's log keeps it:
	// what the relaunch tells the run it returned ([Asked.Called]).
	if asked, ok := carry.(*Asked); ok {
		carry = asked.withReturned(bridgedCalls, r.cfg.Resume.Unfinished)
	}
	logged := bridgedLedger(bridgedCalls)
	elided, absorbed := r.cfg.Resume.Elided, r.cfg.Resume.Absorbed
	read := r.cfg.Resume.BridgedRead
	snapshot := r.executorUniverse(r.seatSnapshot())
	var surface *tools.Surface
	// A SUBMISSION CITES THE CARRIED RUN'S CALLS TOO: the relaunched run is
	// the same round, and the collection judges its submission against
	// every call the round made ([Runner.resumeAgentRun]) — so the check it
	// gets as it submits reads them as well, its record's dropped middle
	// included, or a delivery made before the question, or before the loss,
	// would be refused here and accepted there.
	submit := structured.NewContext(SubmitWorkTool, submitWorkDescription, workSchema,
		typedPickup(r.cfg.Reply, r.pickupAnswer, keptWhole(makingWork(r.cfg.Reply,
			func() []ledger.Call {
				carried := append(slices.Clone(logged), ledger.Called(elided)...)
				return turn.Record(history, append(carried, calls(surface)...))
			},
			func() turn.Surface { return describe(surface) }))))
	bridged := NewBridgedSpend()
	// THE TURN'S CALL LOG STARTS FROM WHAT THE TURN ALREADY CALLED, the
	// carried run's calls included, as the surface is rebuilt and so BEFORE
	// the launch: the relaunched run's bridged calls are admitted against
	// this log from its first one.
	round := roundCalls{of: state.Round, bridged: bridgedCalls, unfinished: inAdmissionOrder(r.cfg.Resume.Unfinished),
		elided: elided, lateElided: r.cfg.Resume.LateElided, absorbed: absorbed}
	built, err := r.rebuiltSurface(ctx, r.meterFor(bridged), state, snapshot, submit, history, round)
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built

	emit := r.emitter()
	caps := roundCaps{max: runRound, ceiling: runRound}
	if refused := carry.refusal(); refused != nil {
		// THE ROUND ENDS HERE, and its phase with it: it was opened when the
		// round first launched, and nothing else will close it now.
		//
		// AND THE TURN ENDS BY THE REFUSAL ITSELF ([turn.ErrPermanent]), which
		// [turn.Abandon] reads before anything the attempt did, so how the
		// round's calls are counted decides nothing here. They are an EARLIER
		// ATTEMPT's all the same, as on every other way out of a carried round,
		// since the jobs before made them.
		refusedErr := fmt.Errorf("runner: %s: %w", phase.Execute, refused)
		emit.completed(ctx, phaseRecord{
			Phase: phase.Execute, Iteration: state.Round,
			Available: surface.Active(),
			Elapsed:   time.Duration(state.LaunchMS)*time.Millisecond + time.Since(began),
			StartedAt: began.UTC(), Caps: caps,
			Failed: true, Err: refusedErr,
		})
		// AND ITS CALLS WHOSE OUTCOME IS UNKNOWN, after its calls, as the
		// record of every round lists them ([Resume.Unfinished]): all of
		// them an earlier attempt's ([turn.Work.Inherited]).
		calls := append(slices.Clone(logged), bridgedLedger(round.unfinished)...)
		return turn.Work{Calls: calls, Inherited: len(calls), Elided: elided, Absorbed: absorbed},
			describe(surface), refusedErr
	}

	// THE NOTES WAITING SINCE THE RUN ASKED, which either brief carries:
	// a fresh run's after every note the turn read, as any brief does; the
	// asking run's own session after the answer — and, where its harness is
	// not known to have ended its own run, after every note the turn has
	// read, which such a copy may lack ([Runner.inPlaceNotes]): a copy whose
	// harness ended its own run holds every note it read, and its brief's,
	// but a read its record marks unanswered or restarted.
	fresh := r.steerBrief(ctx)
	system, user := r.executorPrompt(ctx, "", history, built, fresh)
	// ITS TOOL SKILLS HANDED AGAIN, as every launch hands them: the box's
	// copy is the run's artefact, written afresh at every start, and a
	// session carried on keeps what it loaded, which the rebuilt guard
	// already holds.
	handed := r.handSkills(ctx, built)
	// AND THE SESSION OF THE JOB IT FOLLOWS, CONTINUED in whatever box the
	// launch gets (continued.go) where the launcher says it can; a round lost
	// a second time starts fresh rather than replay the state it was lost in
	// ([Lost]). ITS BRIEF IS BUILT HERE, all but its lead
	// ([Runner.keptSession]): telling the calls after the save can call the
	// auxiliary model, which the budget asked below must count, while the lead
	// says what the approved call returned, made only after that. So neither
	// brief lists that call; a later attempt finds it in the round's log
	// ([carried.tellsHeld]). THE PROBE IS ASKED OF EVERY RELAUNCH: even a
	// twice-lost round continues nothing, but the session read is the only
	// record of what the jobs did, and the run may read it
	// ([Runner.namingKeptSession]).
	basis := r.sessionBasis(ctx)
	// UNDER THE TURN'S OWN FENCE TOKEN, which the relaunched run's tools fence
	// what they read with: a reply carried on is somebody's words, fenced as
	// the message that woke the turn is ([Asked.Continuation]), and every call
	// the briefs below tell is defused under it.
	fence := r.cfg.Turn.Context.FenceBoundary()
	// EACH PATH'S TELLINGS ITS VERDICT MAY HAND (firstrequest.go): the held one,
	// and the one told as a session its harness may hand its model as a
	// summary, each only where the verdict before any brief does not rule it
	// out, both built here so every rewrite either makes is spent before the
	// budget's last ask; which one is handed is judged once the lead exists.
	var kept keptTellings
	if carry.keeps() {
		kept = r.keptTellings(ctx, basis, round, state.Round, leadsHeld(carry, state.Round), history, built, fresh,
			carry.branch())
	}
	// AND WHAT THE JOB'S OWN COPY OF ITS SESSION MAY LACK, for the launch
	// that carries the round on in the job's box rather than from its kept
	// capture, built here for the same reason ([Runner.inPlaceAfter]), with
	// the notes it tells after them ([Runner.inPlaceNotes]).
	inPlace := r.inPlaceTellings(ctx, carry, basis, round, state.Round, history, built, fresh)
	// AND THE TURN'S ANSWERS BEFORE THE ONE CARRYING THE ROUND ON NOW, for a
	// run that starts a session of its own ([Runner.turnsAnswers]): it holds
	// none of the briefs before it, so told only the newest carry's lead, an
	// answer that carried an earlier job on, in this round or one a review sent
	// back, would be lost to it. Built before the round's calls, so the call a
	// person approved that one of them tells is in no list of them.
	answers := r.turnsAnswers(state.Round, history, fence)
	// AND EVERY CALL OF THE ROUND, for a run that starts a session of its own
	// ([prompts.CalledBefore]): it remembers none, and told only that the run
	// before it used the seat's tools, it could repeat a post or a delegation.
	// Told from the round's start ([Runner.calledInPlace]), within the
	// prior-work ledger's budgets, and built for EVERY relaunch, since its box
	// falls back to this brief wherever it cannot take the session up. Never
	// the approved call the reseed's lead tells (made after this), nor one an
	// earlier answer approved. Closed rounds are not listed again: the brief
	// already renders each as work done.
	freshTold, freshOmitted, _ := r.calledAfter(ctx, round, new(0), nil, history, toldAs{fresh: true,
		leads: slices.Concat(leadsHeld(carry, state.Round), answers.held), rendered: history}, built, fence)
	// ASKED ONCE EVERY BRIEF'S REWRITES ARE SPENT, as a launch asks it
	// ([Runner.executeAsAgentRun]): the prior rounds' payloads and the calls
	// each brief tells, each past its budget, are rewritten by the seat's
	// auxiliary model, charged to the same counters, and the rewrite can be
	// what fills the window.
	if refused := toolloop.Refusal(ctx, r.cfg.Budget); refused != nil {
		r.steerReturn(ctx, Launched{}, fresh)
		return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: %s: %w", phase.Execute, refused)
	}
	// THE CALL A PERSON APPROVED IS MADE HERE, once, through the surface the
	// run carries on with, as the last step before the relaunch: after every
	// check that could refuse it, and before the prompt, which tells the run
	// what it returned (ADR-0077). UNDER THE GUARD OF THE SESSION THAT ASKED
	// IT, as the rebuild armed it ([Resume.parked]): the surface still asks
	// the guard of an approved call, and a guard emptied for the relaunch
	// refused a skill-covered call, recorded the refusal as its result and
	// used up the approval. A guard that refuses anyway is asked before the
	// call is marked, leaving the approval unused ([makeHeld]). Only a call no
	// attempt has marked is guarded: one an earlier attempt made is answered
	// from its mark, since the guard decides whether an unmade call may be
	// made, never whether one was.
	if asked, ok := carry.(*Asked); ok && asked.Held != nil {
		if heldErr := makeHeld(ctx, surface, asked.Held); heldErr != nil {
			r.steerReturn(ctx, Launched{}, fresh)
			return turn.Work{Calls: logged, Inherited: len(logged), Elided: elided, Absorbed: absorbed},
				describe(surface),
				fmt.Errorf("runner: %s: %s: %w", phase.Execute, carry.what(), heldErr)
		}
	}
	// AND WHAT CARRIES THE ROUND ON, AS THE RUN KEEPS IT BESIDE ITS RECORD, now
	// that the call a person approved is made: every brief below opens with it,
	// and a later brief to a session that may lack the one its run reads tells
	// it again (leads.go). None for a loss or a replaced sign-in, whose words
	// every later brief's cut-off section says too, nor for a provider's
	// refusal, whose run did no work for a later session to lack.
	own := ownLeads(state.Round, carry.lead())
	// THE GUARD IS THE RELAUNCH'S FROM HERE, empty until its launch decides
	// which session its run takes up ([Runner.sessionDecided]): the rebuild
	// armed it as the job before left it, which is what a run that continues
	// that job's session holds and a fresh one does not. Before the launch,
	// so the relaunched run's first call meets the guard of the session it
	// runs in, never the asking one's.
	r.armSession(built, nil, nil)
	// THE COPY'S TELLING, judged on its lead: what follows the lead, then the
	// notes, what the copy may lack of those the turn has read and the ones
	// this brief is the first to read ([Runner.inPlaceNotes]).
	lead := carry.continuation(fence)
	picked, inPlaceHolds, summed := inPlace.led(ctx, r, lead)
	continued := picked.led(lead)
	// AND, WHERE THE LAUNCH JUDGES THE COPY'S FIRST REQUEST IN ITS BOX, the
	// copy told as a session that may be a summary, which it hands where the
	// box's point does not prove the held brief whole.
	var summarized string
	var summarizedTold sandbox.BriefTold
	var count *sandbox.ResumedCount
	var countProvider string
	if summed != nil {
		summarized = summed.led(lead)
		summarizedTold = sandbox.BriefTold{Omitted: summed.omitted.reads, LateOmitted: summed.omitted.late,
			Own: own, Told: summed.told.refs, ToldUnread: summed.told.unread}
		count, countProvider = r.cfg.Resume.Count, r.cfg.Resume.CountProvider
	}
	// A FRESH RUN'S BRIEF is the round's own, the turn's answers that carried
	// its work on before, what became of the run before it (what it asked and
	// was told, or how it was cut off) and every call of the round so far: it
	// remembers none of it. Each a section of its own, so the prompt published
	// as the phase's opening frame where the box starts a session of its own
	// keeps its outline ([prompts.Prompt.Then]).
	reseed := user.Then(prompts.KeyAnswersBefore, answers.section.Text).
		Then(prompts.KeyCarriedOn, carry.reseed(fence)).
		Then(prompts.KeyRoundSoFar, prompts.CalledBefore(freshTold))
	// AND THE SESSION OF THE JOB IT FOLLOWS, as its launcher answered it
	// above, led by what carries it on: what cut the run off, or the answer
	// to its question and what the call the person approved returned, now
	// that it is made.
	inPlaceSession := carry.session()
	briefs := launchBriefs{fresh: reseed, state: state, bridged: bridgedCalls, inPlaceHolds: inPlaceHolds,
		owed: r.cfg.Resume.Owed}
	if carry.keeps() {
		switch {
		case basis.Continues():
			briefs.kept, briefs.keptHolds, briefs.exact, briefs.unheldHolds = kept.led(ctx, r, lead, own)
		case basis.ContinuesBase():
			// A TURN THAT HAS KEPT NO SESSION OF ITS OWN — its job lost with
			// its box before anything of it was kept — continues the one its
			// first launch froze, handed the reseed, which says what the
			// round's jobs did and what became of the last, and so leaves out
			// what the reseed leaves out; and the turn's answers it tells
			// after the task are told again with the task ([Runner.taskAgain]).
			briefs.kept = r.baseSession(basis, reseed.Text, sandbox.BriefTold{Omitted: freshOmitted.reads,
				LateOmitted: freshOmitted.late, Own: append(taskLead(state.Round, history, ""), own...)})
		}
	} else {
		// A ROUND LOST A SECOND TIME CONTINUES NOTHING, but the session its
		// probe read is still kept and still the only record of what the
		// round's jobs did in their boxes: its run is told it may read it,
		// and nothing else the probe answered reaches the launch.
		basis, inPlaceSession = SessionBasis{KeptTurn: basis.KeptTurn}, ""
	}
	// AND THE RESEED NAMES THE KEPT SESSION IT DOES NOT CONTINUE, as a fresh
	// launch's brief does.
	reseed = r.namingKeptSession(reseed, basis, built)
	briefs.fresh = reseed
	if inPlaceSession != "" {
		briefs.inPlace, briefs.inPlaceSummarized = continued, summarized
	}
	decided := &launchDecided{}
	launched, err := r.cfg.Launcher.LaunchExecutor(ctx, AgentRunRequest{
		System: system.Text,
		Brief:  reseed.Text,
		Notes:  fresh,
		Retold: r.retold(),
		Continue: &Continuation{
			SessionID: inPlaceSession, Brief: continued,
			SignInReplaced: isRotated(carry),
			RequestRefused: isRefused(carry),
			// WHAT THAT SESSION SENDS FIRST, as its newest capture measured
			// it whether the turn kept it or not, or as the read that
			// collected its job found its newest request carried, the larger
			// of the two: the floor of a launch that continues it in its box.
			// THE COPY'S FIGURE, never the kept capture's own: the copy went
			// as far as its job's newest capture, kept or not, and a kept
			// capture is held to what it holds itself.
			ContextTokens: max(basis.CopyContext, r.cfg.Resume.ContextTokens),
			// AND WHAT ITS HARNESS ESTIMATES ITS NEXT REQUEST AT, the job's
			// record's off the copy itself where it holds one, else the
			// newest capture's: the floor's figure where either holds one.
			NextRequest: inPlaceNextRequest(r.cfg.Resume.NextRequest, basis.CopyNextRequest),
			// AND WHAT THE COPY'S BRIEF AND THE FRESH ONE LEFT OUT OF THE
			// ROUND, which the launch records on the job it opens for the brief
			// it hands its run.
			// AND WHAT EACH OPENED WITH: its own leads, and the copy's the
			// leads of its job's brief it told again, and those it was to and
			// could not read.
			Told: sandbox.BriefTold{Omitted: picked.omitted.reads, LateOmitted: picked.omitted.late, Own: own,
				Told: picked.told.refs, ToldUnread: picked.told.unread},
			// The fresh one opens with the turn's task besides, which a session
			// its run starts on it may lack once a compaction summarizes it.
			Fresh: sandbox.BriefTold{Omitted: freshOmitted.reads, LateOmitted: freshOmitted.late,
				Own: append(freshLead(state.Round, history, ""), own...)},
			// AND THE SAME COPY TOLD AS A SESSION THAT MAY BE A SUMMARY, with
			// the count the launch judges the held brief by in its box.
			Summarized: summarized, SummarizedTold: summarizedTold, Count: count, CountProvider: countProvider,
		},
		// AND THE READ EVERY BRIEF ABOVE WAS BUILT FROM, so the job's opening
		// marks late what the log took after it: the job before's calls that
		// landed while this relaunch was readied, the call a person approved
		// but excepted, which the lead tells.
		BriefRead:  &read,
		Session:    briefs.kept,
		Decided:    r.sessionDecided(ctx, emit, state.Round, system, surface, caps, briefs, decided),
		Started:    r.sessionStarted(ctx, emit, state.Round, system, surface, caps, briefs, decided),
		Surface:    surface,
		Skills:     handed,
		Spend:      bridged,
		Round:      state.Round,
		OnFallback: func(f chain.Fallback) { emit.fallback(ctx, phase.Execute, state.Round, f) },
		Fence:      r.cfg.Fence,
	})
	if err != nil {
		r.steerReturn(ctx, launched, fresh)
		err = fmt.Errorf("runner: %s: %s: %w", phase.Execute, carry.what(), err)
		emit.completed(ctx, phaseRecord{
			Phase: phase.Execute, Iteration: state.Round, System: system, User: user,
			Available: surface.Active(),
			Elapsed:   time.Duration(state.LaunchMS)*time.Millisecond + time.Since(began),
			StartedAt: began.UTC(), Caps: caps,
			Failed: !turn.Stopped(err), Err: err,
		})
		// The asking run's calls, an earlier attempt's: see below.
		return turn.Work{Calls: logged, Inherited: len(logged), Elided: elided, Absorbed: absorbed},
			describe(surface), err
	}
	r.steerBriefed(ctx, state.Round, launched.LaunchID, fresh)
	launchedAt := state.LaunchedAt
	if launchedAt.IsZero() {
		launchedAt = began
	}
	r.recordAgentSuspension(state.Round, surface, history, launchedAt,
		time.Duration(state.LaunchMS)*time.Millisecond+time.Since(began), handed, launched.BridgeFrom,
		decided.skills())
	// THE ROUND'S CALLS SO FAR, the run's before it asked or was cut off:
	// they happened and stay on the round's record, as an EARLIER ATTEMPT's
	// ([turn.Work.Inherited]) — the job before made them, and a retry of this
	// continuation relaunches the run rather than making them again.
	return turn.Work{
		Calls: logged, Inherited: len(logged), Elided: elided, Absorbed: absorbed, Suspended: true,
	}, describe(surface), nil
}

// recordAgentSuspension captures what the run's collection needs: armed is the
// skills the launch armed the run's guard with as it decided
// ([launchDecided.skills]).
func (r *Runner) recordAgentSuspension(round int, surface *tools.Surface, history []ledger.Iteration,
	began time.Time, launched time.Duration, handed HandedSkills, bridgeFrom int, armed []string,
) {
	state := execstate.State{
		Version:     execstate.Version,
		ActiveTools: surface.Active(),
		// THE SKILLS THE SESSION THE LAUNCH DECIDED ON HOLDS, as its guard
		// was armed ([Runner.sessionDecided]), and the call the job's own
		// calls are numbered after: every rebuild of the run's guard replays
		// this job's loads on top of these, and no other job's. AS ARMED,
		// never read off the guard once the launch returns: the run starts
		// inside the launch, so a load its harness reported by then is one
		// of the job's own calls, and recorded here as one its launch armed
		// it would be armed by every rebuild that arms none of them — the
		// job's copy in its box continued after its harness was cut off, or
		// a capture of it saved before the load.
		LoadedSkills: armed,
		BridgeFrom:   bridgeFrom,
		// THE NAMES ITS HARNESS WAS HANDED ITS SKILLS UNDER, which every
		// later opening of its bridge reads a reported load by, and every
		// rebuild of its guard names a refusal by (handedskills.go).
		NativeSkills: handed.Listed(),
		Round:        round,
		// THE LAUNCH'S HALF OF THE PHASE, and where it began, for the
		// record the collection publishes. See [execstate.State.LaunchMS]
		// and [execstate.State.LaunchedAt].
		LaunchMS:   int(launched / time.Millisecond),
		LaunchedAt: began.UTC(),
		Iterations: history,
		Task:       r.cfg.Task,
		// AND WHAT THE TURN STARTED WITH, for the launches after it. See
		// [execstate.State.Context].
		Context: frozen(r.cfg.Context, r.cfg.dispatched()),
	}
	if err := state.Validate(); err != nil {
		// Not recorded, and the absence is what the engine reports: a run
		// whose state cannot be written is one nothing can resume, and it
		// must fail while the launch is still in this process's hands.
		log.Error("agent_run_suspension_invalid", "round", round, "error", err.Error())
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.suspension = &Suspension{State: state}
}

// ReopenAgentRun rebuilds the tool surface an executor run's box is still
// calling, for a node that has taken the run over, so the bridge session can
// be reopened (internal/api/mcpbridge). It returns the surface and the meter.
//
// Built from the run's own state by the one rebuild every re-entering segment
// makes ([Runner.rebuiltSurface]): from the launch's record alone, tools the
// run activated would be gone and skill-unlocked ones refused again. The call
// log is seeded with every bridged call, elided and absorbed ones included
// ([seedCalls]), so a call made here counts repeats across the whole run and
// derives the operation id the run's own process would have. The meter starts
// from spent, the row's bridged cost ([NewBridgedSpendFrom]).
func (r *Runner) ReopenAgentRun(ctx context.Context, state execstate.State,
	bridged, unfinished []BridgedCall, elided, absorbed []ledger.Elided, spent Bridged, continued bool,
) (ReopenedRun, error) {
	history := state.Iterations
	called := append(bridgedLedger(inAdmissionOrder(bridged)), ledger.Called(elided)...)
	held := Resume{State: state, Continued: continued}.parked()

	snapshot := r.executorUniverse(r.seatSnapshot())
	var surface *tools.Surface
	// THE SUBMISSION IT CAPTURES IS NEVER READ: every collection replays
	// submit_work from the durable log ([Runner.resumeAgentRun]), which skips a
	// call recorded as failed.
	submit := structured.NewContext(SubmitWorkTool, submitWorkDescription, workSchema,
		typedPickup(r.cfg.Reply, r.pickupAnswer, keptWhole(makingWork(r.cfg.Reply,
			func() []ledger.Call {
				return turn.Record(history, append(slices.Clone(called), calls(surface)...))
			},
			func() turn.Surface { return describe(surface) }))))

	meter := NewBridgedSpendFrom(spent)
	// UNFINISHED CALLS ([Resume.Unfinished]) count by identity: one still
	// running on the node it left is a call made, and a different call made
	// here must not derive its operation id.
	built, err := r.rebuiltSurface(ctx, r.meterFor(meter), held, snapshot, submit, history,
		roundCalls{of: state.Round, bridged: inAdmissionOrder(bridged), unfinished: inAdmissionOrder(unfinished),
			elided: elided, absorbed: absorbed})
	if err != nil {
		return ReopenedRun{}, err
	}
	surface = built
	own := ownCalls(inAdmissionOrder(bridged), state.BridgeFrom, nil, nil)
	// THE GUARD IS THE SESSION'S ITS BOX STARTED: the launch's armed skills
	// only where the box took up the continued session ([Resume.Continued]),
	// else only its own loads ([Resume.parked]). A box that has not said yet
	// says it on its first request, before any call is made.
	return ReopenedRun{Surface: surface, Meter: meter,
		Started: func(boxContinued bool) {
			var loaded []string
			if boxContinued {
				loaded = state.LoadedSkills
			}
			r.armSession(surface, loaded, own)
		}}, nil
}

// ReopenedRun is an executor run's tool surface as a node that took the run
// over rebuilt it ([Runner.ReopenAgentRun]): the surface, the meter its
// session's calls are counted on, and what its guard is held to once the run's
// box says which session it started.
type ReopenedRun struct {
	Surface *tools.Surface
	Meter   *BridgedSpend

	// Started holds the run's guard to the session its box says it started,
	// on its first request to the opening this node serves, before any call
	// there: the skills its launch armed and the job's own loads where it
	// continued the session its launch chose, the job's own loads alone where
	// it started one of its own (internal/api/mcpbridge, Session.Started). The
	// box's word over the record's, which a box that had not said when the run
	// was taken over left empty.
	Started func(continued bool)
}

// rebuiltSurface rebuilds a run's surface as it left it and seeds the call log
// with the run's place in the turn: the one rebuild every re-entering segment
// makes ([Runner.resumeAgentRun], [Runner.continueAgentRun],
// [Runner.ReopenAgentRun]). A rebuild replaying half of what the run did is the
// drift this stops: the collected surface is also what the record states the
// run held (tools_available), so the guard is replayed though it calls nothing.
//
// The surface is the launch's tools (all it can reach, so no bridge call adds
// one) and guard, plus every skill its own job loaded since, read off its calls
// ([Runner.observeLoads]) past [execstate.State.BridgeFrom], never an earlier
// job's. The closed rounds and the round's calls are seeded through it
// ([seedCalls]).
func (r *Runner) rebuiltSurface(ctx context.Context, m meter, state execstate.State,
	snapshot tools.Snapshot, submit tools.Callable, history []ledger.Iteration, round roundCalls,
) (_ *tools.Surface, err error) {
	defer origin.Stamp(&err)
	surface, err := r.surfaceWith(ctx, m, phase.Execute, state.Round, snapshot, submit,
		state.ActiveTools, state.LoadedSkills...)
	if err != nil {
		return nil, err
	}
	seedCalls(r.cfg.Turn.Context.CallLog(), surface, history, round)
	r.observeLoads(surface, ownCalls(round.bridged, state.BridgeFrom, nil, nil))
	// AND THE NAMES ITS HARNESS WAS HANDED ITS SKILLS UNDER, so a call the
	// rebuilt guard refuses names the skill as the run's harness lists it.
	r.handedAs(state.NativeSkills)
	return surface, nil
}

// observeLoads tells the guard the surface was just armed with every skill a
// run's bridged calls loaded, by the guard's own reading of a call — the one
// rule for which call unlocks what. Called once the surface is built, because
// whether a guard is armed at all depends on the loader being active. A call is
// read by its
// canonical arguments, made through the surface ([tools.Surface.ArgsOf]), as
// the guard reads every call the surface admits.
func (r *Runner) observeLoads(surface *tools.Surface, called []ledger.Call) {
	r.mu.Lock()
	guard := r.guard
	r.mu.Unlock()
	for _, call := range called {
		if !call.Failed {
			guard.Observe(call.Name, surface.ArgsOf(call.Name, call.Args))
		}
	}
}

// dispatched is what a turn's dispatch rendered once for every launch of it:
// how the conversation's earlier turns ended, the seat's access, and what
// changed in it since the seat was last told.
type dispatched struct{ endings, access, changes string }

// dispatched is the config's dispatch-rendered sections.
func (c Config) dispatched() dispatched {
	return dispatched{endings: c.Endings, access: c.Access, changes: c.CapabilityChanges}
}

// frozen is the turn-start context a runner builds every launch's prompt from,
// as a park carries it: the blocks' rendered text and what the dispatch
// rendered, what [Runner.executorPrompt] reads of them and nothing else.
func frozen(blocks prefetch.Blocks, d dispatched) execstate.Frozen {
	return execstate.Frozen{
		ThreadContext:       blocks.ThreadContext,
		PersonalMemory:      blocks.PersonalMemory,
		RelevantKnowledge:   blocks.RelevantKnowledge,
		EpisodeRecall:       blocks.EpisodeRecall,
		CounterpartyProfile: blocks.CounterpartyProfile,
		SynthesizedSkills:   blocks.SynthesizedSkills,
		OpenAsks:            blocks.OpenAsks,
		Systems:             blocks.Systems,
		Endings:             d.endings,
		Access:              d.access,
		CapabilityChanges:   d.changes,
	}
}

// thawed is a parked turn's frozen context back in the shape a runner's
// prompts read: the inverse of [frozen].
func thawed(f execstate.Frozen) (prefetch.Blocks, dispatched) {
	return prefetch.Blocks{
		ThreadContext:       f.ThreadContext,
		PersonalMemory:      f.PersonalMemory,
		RelevantKnowledge:   f.RelevantKnowledge,
		EpisodeRecall:       f.EpisodeRecall,
		CounterpartyProfile: f.CounterpartyProfile,
		SynthesizedSkills:   f.SynthesizedSkills,
		OpenAsks:            f.OpenAsks,
		Systems:             f.Systems,
	}, dispatched{endings: f.Endings, access: f.Access, changes: f.CapabilityChanges}
}

// resumeAgentRun turns a finished agentic run into the turn's Work.
//
// THE RUN'S OWN TOOL CALLS ARE THE RECORD, replayed from the bridge's durable
// log, not the surface: the resuming process may not be the launching one, so
// its surface ([Runner.rebuiltSurface]) has served no call. The submission is
// in that log too; replaying submit_work through a fresh tool recovers the
// declared outcome. An absent submission is NOT a value: a run that stopped
// silent is rescued as incomplete and judged on its record, never given a
// delivery inferred from its closing prose.
//
// `history` is the closed rounds ([turn.Record]); `bridged` the run's calls in
// admission order ([inAdmissionOrder]).
func (r *Runner) resumeAgentRun(ctx context.Context, history []ledger.Iteration,
	state execstate.State, answer string, bridged []BridgedCall,
) (_ turn.Work, _ turn.Surface, err error) {
	defer origin.Stamp(&err)
	// THIS SEGMENT'S START, which the collection's record states as its
	// started_at: a start read off neither segment would put the record at
	// the epoch, a day before the turn existed. Its DURATION is the
	// phase's — both segments, the launch's carried on the park — and
	// never the run between them, which is each launch's own fact, on its
	// types.ExecutorCollected. See types.AgentPhaseCompleted.
	began := time.Now()
	snapshot := r.executorUniverse(r.seatSnapshot())
	logged := bridgedLedger(bridged)
	elided, absorbed := r.cfg.Resume.Elided, r.cfg.Resume.Absorbed
	unfinished := inAdmissionOrder(r.cfg.Resume.Unfinished)

	// THE REPLAY'S DECODER IS NOT HELD TO WHAT THE LOG KEEPS WHOLE
	// ([keptWhole]), NOR DOES IT REFUSE A NO_ACTION AFTER AN ACT
	// ([makingWork]): the submission is already made. One a build before
	// that bound recorded whole is replayed as it stands, and a no_action
	// the run made before it acted is collected as the run's own account,
	// which [turn.Check] corrects from the same record.
	var surface *tools.Surface
	submit := structured.NewContext(SubmitWorkTool, submitWorkDescription, workSchema,
		typedPickup(r.cfg.Reply, r.pickupAnswer, decodeWork(r.cfg.Reply,
			func() []ledger.Call {
				return turn.Record(history, append(slices.Clone(logged), ledger.Called(elided)...))
			},
			func() turn.Surface { return describe(surface) })))

	// THE SURFACE THE RUN LEFT, the tools it activated over the bridge
	// included, which the round's record states as what the run held and
	// what it could still have discovered ([Runner.finishWork]); and the
	// turn's call log, which starts from what the turn already called.
	built, err := r.rebuiltSurface(ctx, r.ownMeter(), state, snapshot, submit, history,
		roundCalls{of: state.Round, bridged: bridged, unfinished: unfinished, elided: elided, absorbed: absorbed})
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built

	replaySubmission(ctx, submit, sinceAnswer(bridged, r.cfg.Resume.Answered))

	// THE ROUND'S RECORD LISTS ITS CALLS WHOSE OUTCOME IS UNKNOWN after its
	// calls ([Resume.Unfinished]): the reviewer reads them, and the turn's
	// tool log keeps them, each failed with the words that say what it may
	// have done ([UnfinishedNote]) — and a later segment's call log counts them
	// off the closed round's record by their identity.
	recorded := append(slices.Clone(bridged), unfinished...)
	run := r.cfg.Resume.Run
	res := agentRunResult(answer, recorded, elided, run, state.LaunchedAt, began)
	// THE NOTES THE ROUND'S RUN READ, off the record every read is written
	// on: in its briefs, and at the boundaries of its harness's own loop — the
	// reads of this round's jobs, at the run's one round, each with where it
	// was read.
	res.RunSteers = runReads(r.cfg.Resume.Steered, state.Round)
	res.StartedAt = began.UTC()
	res.Elapsed = time.Duration(state.LaunchMS)*time.Millisecond + time.Since(began)
	work, described := r.finishWork(ctx, state.Round, work{
		submit:  submit,
		res:     res,
		surface: surface,
		run:     run,
	})
	// The RUN's calls, not this process's surface's — see the doc above —
	// and every one an EARLIER ATTEMPT's ([turn.Work.Inherited]): the run
	// made them before this segment began, and a retry of the collection
	// collects the same run and makes none of them again. They are on the
	// record, for the delivery check and the ledger; what they are not is
	// a write this attempt would repeat. Counted as one, a collection that
	// met its reviewer's usage limit before writing anything was settled
	// for good as a turn that had written, and the run's result never
	// reached the seat.
	work.Calls = bridgedLedger(recorded)
	work.Inherited = len(work.Calls)
	// AND THE CALLS THE LOG DID NOT KEEP, by identity: the round's all the
	// same, for the delivery checks, the closed round's record and the
	// reviewer, who is told how many there were — and the ones the round's
	// own calls' work made, which the closed round's record carries for the
	// next segment's call log and nothing else reads.
	work.Elided, work.Absorbed = elided, absorbed
	// A JOB ITS PROVIDER REFUSED DID NO WORK TO JUDGE: the round carries the
	// provider's message, and the loop ends the turn on it.
	work.RequestRefused = r.cfg.Resume.RequestRefused
	return work, described, nil
}

// agentRunResult turns what the run actually did into the phase's record.
//
// ONE ROUND, the honest count: a round is an iteration of the ENGINE's
// exchange with a model, and the executor's is one request, the launch, and one
// answer however many turns the harness took inside. A round per call would
// claim a structure nobody observed; the cap is that round too (the run's
// turns are bounded by `max_turns`). It is timed from launchedAt to answered
// (two clocks across nodes, so a negative skew states no span) and carries NO
// TOKENS: those are each launch's fact on `executor_collected`, and repeating
// them would charge every rollup twice. The member is the entry the launch
// picked, not the chain's head; elided calls ([phaseResult.RunElided]) are
// counted so the kept head and tail are not read as all it did.
func agentRunResult(answer string, bridged []BridgedCall, elided []ledger.Elided, run RunRecord,
	launchedAt, answered time.Time,
) phaseResult {
	res := toolloop.Result{
		Text: answer, RoundsUsed: runRound, MaxRounds: runRound,
		Model: run.Model, ProviderKey: run.ProviderKey,
	}
	if !launchedAt.IsZero() {
		res.Rounds = []toolloop.Round{{
			Round: runRound, StartedAt: launchedAt.UTC(),
			Duration: max(answered.Sub(launchedAt), 0),
			Model:    run.Model, ToolCalls: len(bridged) + ledger.Count(elided),
		}}
	}
	for _, call := range bridged {
		res.Executions = append(res.Executions, toolloop.Execution{
			Round: runRound, Name: call.Name, Args: call.Args,
			Output: call.Result, OutputBytes: call.OutputBytes, Failed: call.Failed,
			StartedAt: call.StartedAt, Duration: call.Duration,
			Origin: call.Origin, Server: call.Server,
			Source: call.Source, Refusal: call.Refusal,
			// WHICH OF THE TWO CALLS WHOSE ARGUMENTS ARE SEALED, so the record
			// says so in their place rather than drawing a call made with none.
			Held: call.Held, Approved: call.Approved,
			// AND WHY A CALL'S ARGUMENTS ARE NOT KNOWN, for the same reason.
			ArgsUnknown: argsUnknown(call.Call),
		})
	}
	if strings.TrimSpace(answer) != "" {
		// The run's own last word, on the round its calls are on, so the
		// ledger reads as one block rather than as prose beside orphaned
		// tool rows. There is no separate reasoning to report: a harness's
		// thinking stays inside its own loop.
		//
		// A COPY OF THE ANSWER, which the record carries whole as its
		// response: held to the record's share of prose (recordbytes.go),
		// its cut saying the response holds the rest, so a long report is
		// not carried twice and its copy never reads as the whole lost.
		copied, _ := answerCopy.Text(answer, answerCopyBytes)
		res.Narration = []toolloop.Narration{{Round: runRound, Content: copied}}
	}
	return phaseResult{Text: answer, Rounds: runRound, Result: res,
		Caps: roundCaps{max: runRound, ceiling: runRound}, RunElided: elided}
}

// argsUnknown is why the record of an executor run's call does not say what
// it was made with, as the ledger's line of it says ([ledger.FormatCalls]):
// its arguments were in text that could not be read ([ledger.Call.ArgsUnread]),
// or it was still in flight as its round was read and its entry kept none
// ([ledger.Call.Unfinished]). "" on every other call: a withheld one says
// which of its two it is instead (Held, Approved), and an unfinished call that
// carries arguments is drawn with them.
func argsUnknown(call ledger.Call) toolloop.ArgsUnknown {
	switch {
	case call.Withheld:
		return ""
	case call.ArgsUnread:
		return toolloop.ArgsUnread
	case call.Unfinished && len(call.Args) == 0:
		return toolloop.ArgsUnkept
	}
	return ""
}

// runReads is the notes the jobs of a collected round read, in the order read,
// as its execute record lists them: at the run's one round ([runRound]), each
// with the boundary that read it. Nil for a round whose jobs read none.
//
// THE ROUND'S JOBS, NOT THE COLLECTED JOB'S ALONE: a job carried on with a
// person's answer continues the asking job's round, and the record lists both
// jobs' calls as one order (sandbox.OpenedJob.LastSeq); keyed on the collected
// job it would drop the notes read between them. A read of the round's execute
// phase naming a job is one of those jobs'; a reviewer's reads are review's.
// NEVER A READ MARKED UNANSWERED (steer.Reading.Unanswered): its answer never
// reached the harness, so the turn's later phases carry it. A restarted read
// is listed: the harness took the note at that step.
func runReads(reads []steer.Read, iteration int) []types.PhaseSteer {
	var out []types.PhaseSteer
	for _, read := range reads {
		if read.Phase != types.PhaseExecute || read.Iteration != iteration || read.Launch == "" ||
			read.Unanswered {
			continue
		}
		out = append(out, types.PhaseSteer{Round: runRound, NoteID: read.ID, Boundary: read.Boundary})
	}
	return out
}

// runRound is the one round an executor run's record has, which every call
// the run makes is recorded on — see [agentRunResult] for why there is one.
// The workers a run's delegate call spawns nest under it too, so they sit
// under the call that spawned them.
const runRound = 1

// sinceAnswer is the calls of a round its submission may be read from, for the
// replay ([replaySubmission]) and [Resume.carriedRound]: those placed after
// `answered` ([Resume.Answered]), or all where no answer came.
//
// ONLY WHAT THE RUN DID KNOWING THE ANSWER: an earlier `submit_work` (calls
// are kept on carry-on, ADR-0068) would report the run as that earlier work.
// The cut is the place the row recorded when the continuing job opened, never
// a search for the last ask, which the log may drop (sandbox.MaxBridgeCalls).
// NEVER A LATE CALL ([ledger.Call.Late]): taken after its job was replaced,
// its place can pass the cut though it predates the answer. Briefs tell it as
// no submission of the round ([prompts.AfterSave.LateSubmission]); read as
// one, a lost round would be collected on it rather than carried on.
func sinceAnswer(bridged []BridgedCall, answered int) []BridgedCall {
	out := make([]BridgedCall, 0, len(bridged))
	for _, call := range bridged {
		if !call.Late && (answered <= 0 || call.Seq > answered) {
			out = append(out, call)
		}
	}
	return out
}

// inAdmissionOrder is a run's bridged calls in the order the bridge admitted
// them — the order the harness made them in — where the log holds them in the
// order they FINISHED: each is appended once it returns, and a harness that makes
// several calls at once finishes them in whatever order their tools take. The
// record states the order the run acted in, and the replay below reads a
// submission made after another as the correction, whichever returned first.
// Stable, so calls the log carries no place for keep the log's order.
func inAdmissionOrder(bridged []BridgedCall) []BridgedCall {
	out := slices.Clone(bridged)
	slices.SortStableFunc(out, func(a, b BridgedCall) int { return cmp.Compare(a.Seq, b.Seq) })
	return out
}

// replaySubmission feeds the run's own submit_work call back through a fresh
// submission tool, so the outcome the harness declared is the one reported.
// LAST WRITE WINS: a second call is a correction, so calls replay in admission
// order ([sinceAnswer]'s). A call the decoder rejects is skipped: a malformed
// submission is the "said nothing" case the rescue path is for.
//
// A SUBMISSION THE RECORD DID NOT KEEP WHOLE IS NEVER REPLAYED, and where it is
// the newest, nothing is (rescued as incomplete): its marker would read as the
// whole summary, and an older submission would report a corrected outcome. The
// log keeps the newest successful one whole (sandbox's MaxSubmissionArgsBytes)
// and the live tool refuses one past that ([keptWhole]), so this is a run
// whose own spelling slipped past what the log holds.
func replaySubmission(ctx context.Context, submit *structured.Tool[workPayload], bridged []BridgedCall) {
	newest := -1
	for i, call := range bridged {
		if call.Name == SubmitWorkTool && !call.Failed {
			newest = i
		}
	}
	if newest >= 0 && bridged[newest].ArgsBytes > 0 {
		log.WarnContext(ctx, "agent_run_submission_not_kept", "seq", bridged[newest].Seq,
			"args_bytes", bridged[newest].ArgsBytes,
			"detail", "the run's newest submission is not on its record whole, so it is not replayed "+
				"and the round is collected as one that submitted nothing")
		return
	}
	for _, call := range bridged {
		if call.Name != SubmitWorkTool || call.Failed || call.ArgsBytes > 0 {
			continue
		}
		if _, _, err := tools.Invoke(ctx, tools.Invocation{Name: SubmitWorkTool, Tool: submit,
			Args: tools.ArgsOf(submit, false, call.Args), Where: "replay"}); err != nil {
			log.WarnContext(ctx, "agent_run_submission_replay_failed", "error", err.Error())
		}
	}
}
