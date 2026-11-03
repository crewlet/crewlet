package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TURN'S OWN SESSION, CONTINUED IN WHICHEVER BOX ITS NEXT LAUNCH GETS.
//
// Every launch of a turn keeps its harness's session in the object store
// (session.go), named by the turn's row on the executor log. So a relaunch of
// the turn — a reviewer's send-back, a person's answer to a run that parked, a
// job lost with its engine whose box was left behind, a sign-in replaced while
// it ran — continues the session of THE JOB BEFORE IT, as that job's kept
// capture holds it, in whatever box and cell the relaunch gets: the box the
// session was last worked in, its own copy kept, or any other, the archive
// restored there before its harness takes the session up ([RunRequest.Carried]).
//
// # Which session: the job before's own, or none
//
// A relaunch follows one job — the one its reviewer collected, the one whose
// question was answered, the one lost or refused — and only that job's session
// holds everything the turn's jobs were told: the round's task or the review
// that sent the round back, a person's answer, and the job's own work. A
// capture an EARLIER job took holds none of what the jobs after it were told,
// and the calls a continuation lists after it would be numbered in another
// round's order where a send-back began one. So a relaunch whose job before
// left no kept capture — its box went with its host, its take or its keep was
// refused, its record was never answered — continues none: it starts a
// session of its own, told what the jobs before it did. And only that job's
// NEWEST capture: a later one that was not kept is a session that went further
// than the kept one, and a run continued from the older would believe it knew
// everything it did.
//
// ONE DECISION, [ContinuityOf]: the turn live and not ending, the job before's
// newest capture kept and named by the turn's row on the executor log, in a
// format and by a release a runner restores ([ArchiveRestorer]). The launch's
// caller asks it before it builds the run's surface and briefs
// ([Coordinator.ContinuedCapture]), of every member of the seat's chain, since
// which of them takes the launch is the launch's to pick; the launch asks it
// again of the record as it opened the job, of the runner of the member it
// picked; and the read surface asks it of each row of a seat's sessions
// (internal/api/queries' `continuable`) through [ReadContinuityOf], of the same
// two records read as a relaunch reads them and of the member the node holding
// the seat would launch it on now ([RelaunchTarget]) — with the half a person's
// answer takes beside it, carrying a parked run on in the box it parked in where
// the kept capture is not continued, by the launch's own tests of a box it
// continues in ([PriorBox.ContinuesIn], [Coordinator.hostsPrior]). What only a
// launch can know is the launch's alone, and no reader states it: whether the
// box it got runs a harness that takes the session up
// ([SessionRestorer.CheckCarry]), and whether the seat's budget can carry the
// session's context beside the brief it hands the run (internal/engine's launch
// floor).
//
// # The job before's capture, still being kept
//
// The job's capture is taken by the read that collected it, and its upload and
// record — the keep — are made off that read ([Coordinator.keepCapture]), while
// a relaunch can follow at once: a lost job's round is carried on by the very
// frame that read its box. So the relaunch waits for the keep, but only for
// as long as an ordinary keep takes ([relaunchCaptureWait]): the frame that
// waits holds the seat and a slot of the node's admission, and a keep that has
// not landed by then is one a slow store or a stopped node is holding, which a
// relaunch does not wait out on that frame. It starts a session of its own,
// saying so.
//
// # Never a failed launch
//
// An object the store no longer holds or whose seal does not open
// ([executordomain.ErrUnreadable]), a log that does not answer, a box whose
// harness cannot take the session up: each starts the run on a session of its
// own, handed the brief written for one, and is logged with its reason
// (`executor_session_not_continued`). The session is fetched SEALED onto this
// node's disk and opened whole once before a byte of it reaches the box
// ([executordomain.SessionStore.Fetch]), so the decision is made before the
// brief is, never part way through writing the archive into the box.

// SessionRestore is what continuing a turn's kept session needs of the engine:
// the turn's row on the executor log, read through the ESTATE ROUTER'S reads
// (internal/estate's ExecutorReads) at the level the caller asks — a capture
// another node recorded is read at `linearizable`, which waits for this node's
// copy, or asks a data node whose copy has it — and the sealed object it names,
// fetched and opened through the domain's session store.
type SessionRestore interface {
	// Session reads one turn's row (executordomain.Reader.Session).
	Session(ctx context.Context, handle, turn string, fresh statelog.Freshness) (executordomain.Session, error)

	// Fetch copies the sealed object row names into dst, checked against the
	// row (executordomain.SessionStore.Fetch), and OpenSpooled opens what it
	// copied against the row's associated data
	// (executordomain.SessionStore.OpenSpooled).
	Fetch(ctx context.Context, row executordomain.Session, dst io.Writer) error
	OpenSpooled(row executordomain.Session, sealed io.Reader) (io.Reader, error)
}

// ExecutorContextWindow is the context, in tokens, at which every Claude Code
// executor run compacts its conversation (CLAUDE_CODE_AUTO_COMPACT_WINDOW,
// pinned by internal/sandbox/executor over any value an image, a repository or
// a seat sets): what one request of the run — and of every run that continues
// its session, which sends the session's whole context again first — carries
// at most, where the model's own window, a million tokens on some, would let a
// session grow ten times past it, every token of every request charged to the
// seat's budget.
//
// NOT A SIZE A SESSION IS REFUSED AT. A session past it — an OpenCode one,
// whose harness compacts near its model's own window rather than at a figure
// the engine can set, or one a harness release that ignores the variable kept
// — is continued wherever the seat's budget can carry it: the launch's floor
// decides that, from the session's context and the brief beside it
// (internal/engine), and a seat with no capped window carries any.
//
// ONE HUNDRED THOUSAND, and a judgment that trades a session's detail for what
// every request of a continued run costs: four times the launch floor
// (turn_engine.executor_min_budget_tokens), the least Claude Code accepts
// (100,000 to 1,000,000), and enough for the work in hand, since a coding
// run's durable state is its checkout and its turn's records rather than its
// transcript, so a session compacted at it keeps the recent work verbatim and
// a summary of the rest. A larger window keeps more verbatim detail and
// charges more per request.
const ExecutorContextWindow = 100_000

// SessionContinuity is whether a relaunch may continue a turn's kept session,
// and where it may not, why — [ContinuityOf]'s answer.
type SessionContinuity struct {
	Continues bool
	Why       string
}

// ArchiveRestorer is the half of a [SessionRestorer] the decision whether a
// session can be continued asks ([ContinuityOf]): whether a runner restores an
// archive at all — its harness, its archive's format, a release whose sessions
// another box continues — before any box is asked, and where it does not, why.
type ArchiveRestorer interface {
	Restores(archive SessionArchive) (bool, string)
}

// ContinuityOf is THE decision whether a relaunch that follows the job at
// launch follows ([PendingRun.LaunchSeq]) can continue the turn's kept
// session — see the file comment: the turn live and not ending, the newest
// capture of that job kept, the turn's row on the executor log naming it, and
// the runner the launch would take it up with restoring it
// ([ArchiveRestorer]: its harness, its archive's format, a release whose
// sessions another box continues). row is the turn's row on the executor log;
// run the turn's run record as the caller read it, nil where the turn has none
// — its run is over; restorer nil where no runner of this build restores the
// row's harness's sessions.
//
// A TURN WHOSE END IS DECIDED IS ENDED HERE, whatever its row on the log says
// yet ([PendingRun.endDecided]): its run's ending is decided, which every
// launch refuses, or its end is owed on its run's record, or recorded already
// and its record not yet gone, and no relaunch of it follows. Read as live, a
// reader would say its session continues for the moments between the ending's
// decision and the log's taking the end — moments a stop's read or a store's
// bad moment can stretch to minutes.
//
// What the box a launch gets says of its harness's own release, and what the
// seat's budget says of the session's context, are the launch's to ask, and
// this decides neither.
func ContinuityOf(row executordomain.Session, run *PendingRun, follows int64,
	restorer ArchiveRestorer) SessionContinuity {

	switch {
	case run == nil || row.Ended():
		return SessionContinuity{Why: "the turn has ended, and no later turn continues its session yet, " +
			"since a session is continued only by a relaunch of the turn that kept it"}
	case run.endDecided():
		return SessionContinuity{Why: "the turn is ending: its run's ending is decided, and no relaunch " +
			"of it follows to continue its session"}
	}
	kept, why := followedCapture(*run, follows)
	if why != "" {
		return SessionContinuity{Why: why}
	}
	switch {
	case !row.HasObject():
		return SessionContinuity{Why: "the turn's row on the executor log names no kept session"}
	case row.LaunchSeq != kept.LaunchSeq || row.CaptureSeq != kept.CaptureSeq:
		return SessionContinuity{Why: fmt.Sprintf("the executor log names the turn's capture (launch %d, "+
			"capture %d), and its run's record keeps (launch %d, capture %d)", row.LaunchSeq, row.CaptureSeq,
			kept.LaunchSeq, kept.CaptureSeq)}
	case restorer == nil:
		return SessionContinuity{Why: fmt.Sprintf("no runner of this build restores a session %s wrote",
			row.Harness)}
	}
	if ok, refused := restorer.Restores(ArchiveOf(row)); !ok {
		return SessionContinuity{Why: refused}
	}
	return SessionContinuity{Continues: true}
}

// followedCapture is the capture a relaunch that follows the job at launch
// follows continues — that job's newest capture, kept — or why there is none,
// in words for the log: the half of [ContinuityOf] the run's record answers
// alone, asked before the executor log is read.
func followedCapture(run PendingRun, follows int64) (SessionCapture, string) {
	kept, newest := run.Kept, run.Capture
	switch {
	case newest == nil:
		return SessionCapture{}, "the turn has kept no capture of its session"
	case newest.LaunchSeq != follows:
		// "HAS LEFT", which is true of a job still working as much as of one
		// that ended: a reader asks this of a live turn whose job runs, and a
		// relaunch now would continue nothing of it.
		return SessionCapture{}, fmt.Sprintf("the job the relaunch follows (launch %d) has left no capture "+
			"of its session, and an earlier job's (launch %d) holds none of what the jobs after it were told",
			follows, newest.LaunchSeq)
	case kept == nil || newest.newerThan(*kept):
		return SessionCapture{}, fmt.Sprintf("the newest capture of the job the relaunch follows (launch %d, "+
			"capture %d) was not kept: %s", newest.LaunchSeq, newest.CaptureSeq, unkeptWhy(*newest))
	}
	return *kept, ""
}

// ArchiveOf is the archive a turn's row names, as a restorer and a run take
// it: what a reader of whether a relaunch continues the row's session asks the
// node holding the seat about.
func ArchiveOf(row executordomain.Session) SessionArchive {
	return SessionArchive{Harness: row.Harness, SessionID: row.SessionID, Format: row.ArchiveFormat,
		Version: row.HarnessVersion}
}

// ContinuesIn is where a relaunch of a turn would continue the turn's session,
// as a reader of the session is told it ([ReadContinuityOf]).
type ContinuesIn string

const (
	// ContinuesInAnyBox is the turn's kept capture, continued in whichever box
	// the relaunch gets: restored into another, or as its own copy in the box
	// it was last worked in ([ContinuityOf] says it continues).
	ContinuesInAnyBox ContinuesIn = "any_box"

	// ContinuesInParkedBox is the session the box the turn's job parked in
	// holds, continued IN THAT BOX AND NOWHERE ELSE: what a person's answer
	// carries a parked run on in where the turn's kept capture is not
	// continued — the job's capture not kept, or of a release no other box
	// continues — for as long as the run's box is held. A pause past its
	// time-to-live reclaims the box, and the answer's relaunch then starts a
	// session of its own.
	ContinuesInParkedBox ContinuesIn = "parked_box"
)

// ContinuesIns are every place a reader is told a relaunch continues a session.
var ContinuesIns = []ContinuesIn{ContinuesInAnyBox, ContinuesInParkedBox}

// Valid reports whether w is one of [ContinuesIns]. An answer a newer build
// sent may name another, which is read as the value it is.
func (w ContinuesIn) Valid() bool { return slices.Contains(ContinuesIns, w) }

// RelaunchTarget is what a relaunch of a seat's turn would be launched on now,
// as the node holding the seat picks it (internal/engine): the harness of the
// first member of the seat's executor chain not sitting out its plan there, the
// cell that member's runs land in, that node, and whether that member's runner
// restores a session ([ArchiveRestorer]) — what every reader of whether a
// relaunch would continue a session decides by ([ReadContinuityOf]).
//
// THE HOLDER'S PICK, NEVER THE READER'S. Which members sit out their plans is
// what one node's own calls have met (internal/providers/llm/harness's bench,
// which no node shares), and which company revision says the switch is a node's
// too; a relaunch is decided on the node holding the seat, and a reader serving
// the API need not be that node.
type RelaunchTarget struct {
	// Harness and Placement are the member's harness and the cell its run
	// lands in, which decide whether a relaunch continues in the box the run
	// parked in ([PriorBox.ContinuesIn]); empty where there is no member.
	Harness   string
	Placement Placement

	// Node is the node a relaunch of the seat's turn runs on, the one holding
	// the seat: a box in a cell on an engine host is continued in only by the
	// node it was launched on ([Coordinator.hostsPrior]).
	Node string

	// Restorer is the member's runner as it takes up a session carried between
	// boxes — or the holder's answer of it, for a reader on another node —
	// nil where it takes none up, or where there is no member: which Why then
	// says.
	Restorer ArchiveRestorer
	Why      string

	// Undecided says what a relaunch would be launched on cannot be said now,
	// which Why says: every member of the seat's chain sitting out its plan,
	// where which of them takes the relaunch decides what it continues; no
	// node holding the seat; or the node holding it not answering. A relaunch
	// now waits until it can be said, and asks again then.
	Undecided bool

	// NoKept is why a relaunch of the seat's turn continues no kept session
	// whatever the turn kept, "" where it may: the company keeps none
	// ([NoKeptSession]), as the holder's revision says it and its
	// coordinator's own check says it at the launch
	// ([Coordinator.continuesNoKept]). A relaunch then continues at most the
	// session the box its run parked in holds.
	NoKept string
}

// restorer is the target's runner as [ContinuityOf] asks it: its own, or one
// that restores nothing and says why there is none.
func (t RelaunchTarget) restorer() ArchiveRestorer {
	if t.Restorer != nil || t.Why == "" {
		return t.Restorer
	}
	return refusedRestore(t.Why)
}

// Restores is whether the target's runner restores archive, and where it does
// not, why ([ArchiveRestorer]): what the node holding a seat answers a reader
// on another node, for each archive it asks about.
func (t RelaunchTarget) Restores(archive SessionArchive) (bool, string) {
	restorer := t.restorer()
	if restorer == nil {
		return false, "no runner of the member a relaunch is launched on restores a session"
	}
	return restorer.Restores(archive)
}

// refusedRestore is a restorer that takes no session up, for the reason it is:
// a relaunch target with no runner of its own to ask.
type refusedRestore string

func (r refusedRestore) Restores(SessionArchive) (bool, string) { return false, string(r) }

// ReadContinuity is whether a relaunch of a turn would continue the turn's
// session, as a reader of the session is told it ([ReadContinuityOf]).
type ReadContinuity struct {
	// Decided is false where it cannot be said now, which Why then says: a
	// relaunch now would not decide it either.
	Decided bool

	// Continues says a relaunch would continue the session, and In where.
	Continues bool
	In        ContinuesIn

	// Why is why a relaunch would not continue the session, why that cannot
	// be said now, or — where it continues only in the box the run parked in
	// — why the turn's kept capture is continued in no other.
	Why string
}

// ReadContinuityOf is whether a relaunch of the turn whose row on the executor
// log row is would continue its session, as a reader is told it — THE DECISION
// A RELAUNCH TAKES, asked of what a relaunch reads: [ContinuityOf], of row,
// the turn's run record as it stands (run, nil where its run is over), the job
// that record names as the one a relaunch would follow and target's runner —
// the member the node holding the seat would launch it on now; and, where that
// says no, the half that carries a round on in the box its job ran in
// ([inParkedBox]), by the launch's own tests of a box it continues in. row is
// read at the level a relaunch reads it ([RelaunchRead]) wherever the decision
// reaches its capture ([RereadsRow]): a copy of the log behind the run's record
// reads a capture the relaunch would continue as one the log no longer names.
//
// UNDECIDED WHERE A RELAUNCH WOULD NOT DECIDE NOW EITHER, never a no: the job a
// relaunch would follow still working, or being collected — whose session is
// taken by the read that collects it, and kept off that read; the capture of
// that job still being kept, which a relaunch waits for ([relaunchCaptureWait]);
// what the relaunch would be launched on not to be said now
// ([RelaunchTarget.Undecided]); and a relaunch that only the resume of the
// job's collection decides, as it reads how the job ended — whether a lost
// round is carried on at all, whether a box whose sign-in was replaced carries
// its round on. now is the reader's clock, against which a begun capture is in
// flight ([captureInFlight]).
//
// As far as the engine can know before a box is chosen: whether the box a
// relaunch gets runs a harness release that takes the session up, and whether
// the seat's budget can carry it, are the launch's to find out.
func ReadContinuityOf(row executordomain.Session, run *PendingRun, target RelaunchTarget,
	now time.Time) ReadContinuity {

	if run == nil || row.Ended() || run.endDecided() {
		return ReadContinuity{Decided: true, Why: ContinuityOf(row, run, 0, nil).Why}
	}
	switch {
	case jobWorking(*run):
		return ReadContinuity{Why: fmt.Sprintf("the job a relaunch would follow (launch %d) is still "+
			"working or being collected, and its session is taken and kept by the read that collects it",
			run.LaunchSeq)}
	case captureInFlight(*run, now):
		return ReadContinuity{Why: fmt.Sprintf("the capture of the job a relaunch would follow (launch %d, "+
			"capture %d) is still being kept", run.Capture.LaunchSeq, run.Capture.CaptureSeq)}
	case target.Undecided:
		return ReadContinuity{Why: target.Why}
	}
	kept := ContinuityOf(row, run, run.LaunchSeq, target.restorer())
	if target.NoKept != "" {
		kept = SessionContinuity{Why: target.NoKept}
	}
	if kept.Continues && run.Lost != nil && !KeptAfterLoss(run.RoundLosses+1) {
		// A ROUND LOST AGAIN is carried on in a session of its own, never from
		// the state it was lost in — unless its job had submitted its work
		// before it went, when the round is collected and only a review that
		// sends it back would continue it: which the job's resume decides.
		return ReadContinuity{Why: fmt.Sprintf("the job a relaunch would follow (launch %d) was lost a "+
			"second time in its round, which a relaunch carries on in a session of its own; only a review "+
			"of work it submitted before it went would continue it, which its resume decides", run.LaunchSeq)}
	}
	if kept.Continues {
		return ReadContinuity{Decided: true, Continues: true, In: ContinuesInAnyBox}
	}
	box, why := inParkedBox(*run, row, target)
	reason := kept.Why
	if why != "" {
		reason += "; " + why
	}
	switch box {
	case boxContinues:
		return ReadContinuity{Decided: true, Continues: true, In: ContinuesInParkedBox, Why: kept.Why}
	case boxUndecided:
		return ReadContinuity{Why: reason}
	}
	return ReadContinuity{Decided: true, Why: reason}
}

// RereadsRow reports whether a reader that read row below the level a relaunch
// reads it ([RelaunchRead]) must read it again there before deciding its
// continuity: only where the decision reaches the row — the turn live and not
// ending, its job neither working nor its capture still being kept — and the
// row names another capture than the one run's record keeps.
//
// A ROW THAT NAMES THE RECORD'S KEPT CAPTURE DECIDES AS ONE READ AT THE
// RELAUNCH'S LEVEL DOES: a newer capture is stamped on the run's record before
// a byte of it moves, so a record that keeps this one has seen no newer land.
// Read again everywhere else, every look at a seat's sessions — a dashboard
// asks every minute — appended a barrier to the executor log for each live row
// (statelog's linearizable read), one after another, for an answer the row it
// held already gave.
func RereadsRow(row executordomain.Session, run *PendingRun, now time.Time) bool {
	if run == nil || row.Ended() || run.endDecided() || jobWorking(*run) || captureInFlight(*run, now) {
		return false
	}
	kept := run.Kept
	return kept == nil || row.LaunchSeq != kept.LaunchSeq || row.CaptureSeq != kept.CaptureSeq
}

// jobWorking reports whether run's job is still working or being collected, so
// no reader can say yet what a relaunch following it continues: a launch not
// yet suspended, a job running whose box no collection has read back
// ([LaunchRecord.Collected]), or a claim whose read has not yet taken the job's
// final capture. The read that collects a job takes its session first; the
// claim's row then stays resumed for the whole resumed turn, its review
// included — which is when a person most wants to know — and is decided as any
// other once that capture is taken, and settled ([captureInFlight]).
func jobWorking(run PendingRun) bool {
	switch run.Status {
	case StatusLaunching:
		return true
	case StatusResumed:
		taken := run.Capture
		return taken == nil || taken.LaunchSeq != run.LaunchSeq || !taken.Final
	case StatusRunning:
		return !run.Launch.Collected
	}
	return false
}

// boxCarry is whether a relaunch carrying a run's round on in the box its job
// ran in would continue the session that box holds ([inParkedBox]).
type boxCarry int

const (
	// boxNot: no relaunch continues it there.
	boxNot boxCarry = iota
	// boxContinues: a person's answer's relaunch would.
	boxContinues
	// boxUndecided: one would where the resume of the job's collection
	// carries its round on in its box, which only that resume decides.
	boxUndecided
)

// inParkedBox is whether a relaunch carrying run's round on in the box its job
// ran in would continue the session row names, there — and where it would not,
// or cannot be said, why; "" where no relaunch carries the round on in a box.
//
// WHICH RELAUNCHES DO: a person's answer to the run's question — parked on it,
// its box reaped while it waited, answered, or that answer's claim under way
// ([PendingRun.Answer]); and the resume of a collected job whose sign-in was
// replaced while it ran, which only that resume decides as it reads how the
// job ended, so a reader is told it cannot be said. Never a lost job's, whose
// box holds what its harness was cut off in.
//
// BY THE LAUNCH'S OWN TESTS of a box it continues in, asked of the box the
// record names as the relaunch would find it, its job over by then: the same
// harness in the same cell ([PriorBox.ContinuesIn]), and a box in a cell on an
// engine host on the node the relaunch runs on, the one holding the seat
// ([Coordinator.hostsPrior]) — a seat that moved leaves its run's box on the
// host it was launched on, which the seat's next holder neither continues in
// nor reaches. AND OF THE SESSION THE ROW NAMES: the relaunch continues the
// session the box ran ([PendingRun.SessionRan]), and a box that ran another
// session of the turn continues the row's nowhere.
func inParkedBox(run PendingRun, row executordomain.Session, target RelaunchTarget) (boxCarry, string) {
	carried := boxContinues
	switch {
	case run.Status == StatusAwaiting || run.Status == StatusReseed || run.Status == StatusAnswered ||
		(run.Status == StatusResumed && run.Answer != nil):
	case run.Lost != nil:
		return boxNot, ""
	case run.Status == StatusResumed || (run.Status == StatusRunning && run.Launch.Collected):
		carried = boxUndecided
	default:
		return boxNot, ""
	}
	ran := run.SessionRan()
	switch {
	case !run.HasBox():
		return boxNot, "the box its run ran in is gone"
	case ran == "" && run.Launch.FreshSessionID != "":
		return boxNot, "the box its run ran in never said which of its launch's two sessions it started, " +
			"so neither is continued there"
	case ran == "":
		return boxNot, "its run's record names no session the box its run ran in holds"
	case ran != row.SessionID:
		return boxNot, fmt.Sprintf("the box its run ran in holds another session of the turn, %s", ran)
	case target.Harness == "":
		return boxNot, ""
	}
	prior := PriorBox{SandboxID: run.SandboxID, Placement: run.Placement, Harness: run.Harness,
		Status: StatusResumed}
	switch {
	case !prior.ContinuesIn(Spec{Placement: target.Placement, Harness: target.Harness}):
		return boxNot, fmt.Sprintf("the box its run ran in runs %s in the %s cell, and a relaunch is "+
			"launched on %s in the %s cell", run.Harness, run.Placement, target.Harness, target.Placement)
	case Placement(run.Placement).OnEngineHost() && run.LaunchNode != target.Node:
		return boxNot, fmt.Sprintf("the box its run ran in is on the host of %s, and %s, which holds "+
			"the seat, cannot continue in it", run.LaunchNode, target.Node)
	}
	if carried == boxUndecided {
		return carried, "whether the resume of its job's collection carries its round on in the box it " +
			"ran in, which it does where the job's sign-in was replaced while it ran, is decided as that " +
			"resume reads how the job ended"
	}
	return carried, ""
}

// ContinuedSession is the kept session a relaunch of a turn continues: the
// run's record's stamp of the capture — what it accounts for, where it was
// worked, what its last request carried — and the turn's row on the executor
// log that names its object.
type ContinuedSession struct {
	Capture SessionCapture
	Row     executordomain.Session

	// Context is what the last request of the session of the job the
	// relaunch follows carried, as that job's newest capture measured it,
	// KEPT OR NOT: what a launch that continues that session sends again
	// before its brief, restored from the capture or as the job's own box
	// holds it ([LaunchRequest.Continue]). Set whether or not the capture is
	// continued; zero where no capture of the job measured any.
	Context int64
}

// RelaunchRead is the level a relaunch reads its turn's row at, and so the
// level every reader of whether a relaunch would continue the turn's session
// decides on ([ReadContinuityOf]): the capture it continues may have been
// recorded by another node's keep, which this node's copy may not have applied
// yet.
func RelaunchRead() statelog.Freshness { return statelog.Freshness{Level: statelog.ReadLinearizable} }

// relaunchCaptureWait bounds how long a relaunch waits for the capture of the
// job it follows to be kept, on the frame that relaunches: see the file
// comment.
//
// FIFTEEN SECONDS, from what an ordinary keep is: the turn's operations minted
// and the seat's live sessions listed (reads of the coordination store), an
// upload of an ordinary session — a mebibyte or two — and its record, which a
// log answers in milliseconds and a publish its serving node has not yet
// applied within the publisher's five-second wait
// (statelog.DefaultResolveBudget). Three times that is room for a store having
// a slow moment; a keep past it is one a slow upload of a large session, an
// outage or a stopped node is holding, whose own budget runs to minutes
// ([SessionCaptureBudget]) — which the frame does not wait out.
const relaunchCaptureWait = 15 * time.Second

// ContinuedCapture is the kept capture a relaunch of turnID continues — the
// session of the job the relaunch follows, as [ContinuityOf] decides for
// restorer, read once that job's capture is no longer being kept, for at most
// [relaunchCaptureWait] — or why there is none, in words for the log; and
// either way what that job's session's last request carried
// ([ContinuedSession.Context]).
//
// ASKED BY THE LAUNCH'S CALLER before it builds the run's surface and briefs,
// so what the run is offered and told is the session's; the launch takes up
// exactly the capture this answered ([SessionContinuation]), or none, as the
// runner of the member it picks restores it. Never an error: every reason a
// session cannot be continued is a launch that starts one of its own.
func (c *Coordinator) ContinuedCapture(ctx context.Context, turnID string,
	restorer ArchiveRestorer) (ContinuedSession, string) {

	if why := c.continuesNoKept(); why != "" {
		return ContinuedSession{}, why
	}
	run, found, err := c.pending.Get(ctx, turnID)
	switch {
	case err != nil:
		return ContinuedSession{}, fmt.Sprintf("the run's record could not be read: %v", err)
	case !found:
		return ContinuedSession{}, "the turn has no run record"
	}
	run = c.awaitCapture(ctx, run, c.relaunchWait)
	answer := ContinuedSession{Context: followedContext(run)}
	if captureInFlight(run, c.now()) {
		return answer, fmt.Sprintf("the capture of the job the relaunch follows (launch %d, capture %d) "+
			"was still being kept %s after the relaunch asked for it, which the relaunch does not wait out",
			run.Capture.LaunchSeq, run.Capture.CaptureSeq, c.relaunchWait)
	}
	if _, why := followedCapture(run, run.LaunchSeq); why != "" {
		return answer, why
	}
	row, err := c.restore.Session(ctx, run.AgentHandle, turnID, RelaunchRead())
	if err != nil {
		return answer, fmt.Sprintf("the turn's row on the executor log could not be read: %v", err)
	}
	if continuity := ContinuityOf(row, &run, run.LaunchSeq, restorer); !continuity.Continues {
		return answer, continuity.Why
	}
	answer.Capture, answer.Row = *run.Kept, row
	return answer, ""
}

// continuesNoKept is why this coordinator continues no kept session at all,
// "" where it may: a node built with nothing to restore one from, or a company
// that keeps no session as this node's revision says ([SessionKeep.Keeps]) —
// `turn_engine.conversation_session.enabled: false`, under which no session is
// kept and a relaunch continues at most the one its own box holds, whatever a
// capture kept while the switch was on.
func (c *Coordinator) continuesNoKept() string {
	switch {
	case c.restore == nil:
		return "this node continues no kept session: it was built with nothing to restore one from"
	case c.keep != nil && !c.keep.Keeps():
		return NoKeptSession
	}
	return ""
}

// NoKeptSession is why a relaunch continues no kept session in a company that
// keeps none: what a launch logs and a reader of a session is told.
const NoKeptSession = "the company keeps no executor session (turn_engine.conversation_session.enabled " +
	"is false), so a relaunch continues at most the session its own box holds"

// followedContext is what the last request of the session of run's current
// job carried, as that job's newest capture measured it, kept or not; zero
// where it left none, or none that measured any.
func followedContext(run PendingRun) int64 {
	if newest := run.Capture; newest != nil && newest.LaunchSeq == run.LaunchSeq {
		return newest.ContextTokens
	}
	return 0
}

// unkeptWhy is why a capture is not the kept one, as its stamp says.
func unkeptWhy(c SessionCapture) string {
	switch c.State {
	case CaptureRefused:
		return "refused: " + c.Reason
	case CaptureUnresolved:
		return "its record was never answered"
	case CaptureBegun:
		return "it was still being kept, or was begun and never finished"
	}
	return string(c.State)
}

// SessionContinuation is the turn's kept session a launch continues, as its
// caller chose it ([Coordinator.ContinuedCapture]), and the briefs that tell its
// run so: in the box the session was last worked in, and in any other.
type SessionContinuation struct {
	// LaunchSeq and CaptureSeq are the capture's place: the launch takes up
	// exactly that capture, which the run's record must still keep as the
	// newest of the job this launch follows, or none.
	LaunchSeq, CaptureSeq int64

	// Brief is the run's prompt where the box is the one the session was
	// last worked in, its working tree as the job before left it.
	Brief string

	// Replaced is the run's prompt where the box is another — a fresh
	// checkout: Brief and what a box that replaced another changes (which
	// branch to check out, the uncommitted work gone, the earlier box's
	// absolute paths).
	Replaced string
}

// BriefKind is which of the three briefs a launch handed its run: a run
// that continues the turn's session in the box it was last worked in, one
// that continues it in a box that replaced that one, and one that starts a
// session of its own.
type BriefKind string

// The three briefs, as a launch's record and its collection name them: the
// turn's session in the box it was last worked in, the turn's session restored
// into another box, and a session of the run's own.
const (
	BriefContinued       BriefKind = "continued"
	BriefContinuedNewBox BriefKind = "continued_new_box"
	BriefFresh           BriefKind = "fresh"
)

// Valid reports whether k is one of the three briefs. A record a newer build
// wrote may name one this build does not know, which is read as the value it
// is rather than refused.
func (k BriefKind) Valid() bool {
	switch k {
	case BriefContinued, BriefContinuedNewBox, BriefFresh:
		return true
	}
	return false
}

// Continues reports whether k is a brief that continues a session: a run handed
// one takes the session up where its box can, wired beside it for a session of
// its own where its box cannot ([RunRequest.FreshWiring]). A brief this build
// does not know continues nothing it can tell.
func (k BriefKind) Continues() bool { return k == BriefContinued || k == BriefContinuedNewBox }

// LaunchDecision is how a launch decided its run takes up a session, told to
// its caller before the run starts ([LaunchRequest.Decided]).
type LaunchDecision struct {
	// Brief is which of the three briefs the launch hands the run.
	Brief BriefKind

	// Kept says the session the run continues is the turn's kept capture
	// ([LaunchRequest.Session]); false where it is the job before's own copy
	// in its box ([LaunchRequest.Continue]), or none.
	Kept bool
}

// continuedLaunch is how one launch continues the turn's session, if it does:
// the brief its run is started with and the one it falls back to, the session
// it takes up and the archive carried into its box, and which brief it is.
type continuedLaunch struct {
	brief, fresh, session string
	carried               *CarriedSession
	kind                  BriefKind
	// kept says the session is the turn's kept capture rather than the job
	// before's own copy in its box ([LaunchDecision.Kept]).
	kept bool
	// release removes what the launch spooled to carry the session in, once
	// the run has started (or failed to).
	release func()
}

// decision is how the launch decided, for its caller ([LaunchRequest.Decided]).
func (l continuedLaunch) decision() LaunchDecision {
	return LaunchDecision{Brief: l.kind, Kept: l.kept}
}

// freshLaunch is a launch that continues no session.
func freshLaunch(req LaunchRequest) continuedLaunch {
	return continuedLaunch{brief: req.Brief, kind: BriefFresh, release: func() {}}
}

// continueSession is how the launch req continues its turn's session in box —
// see the file comment — given the run's record as the launch opened it and
// whether box is the one the job before used (reused).
//
// IN THIS ORDER: the turn's kept capture its caller chose ([LaunchRequest.Session]),
// in any box; else, where the box is the job before's own, that job's session
// as the box holds it ([Continuation.SessionID]), as a run always could; else
// a session of its own.
func (c *Coordinator) continueSession(ctx context.Context, req LaunchRequest, opened PendingRun,
	box Sandbox, runner Runner, reused bool) continuedLaunch {

	if want := req.Session; want != nil {
		launch, why := c.continueKept(ctx, req, opened, box, runner, reused, *want)
		if why == "" {
			return launch
		}
		c.notContinued(ctx, req, why)
	}
	if continued := req.Continue; continued != nil && reused && continued.SessionID != "" {
		return continuedLaunch{brief: continued.Brief, fresh: req.Brief, session: continued.SessionID,
			kind: BriefContinued, release: func() {}}
	}
	return freshLaunch(req)
}

// continueKept continues the turn's kept capture want in box, or says why it
// cannot.
//
// THE BOX THE CAPTURE WAS TAKEN IN IS THE REUSED ONE: the capture is the job
// before's, by [ContinuityOf], and a reused box is that job's
// ([PriorBox.ContinuesIn]) — so the same-box brief is handed exactly where the
// session was last worked in this box, and the new-box brief everywhere else.
func (c *Coordinator) continueKept(ctx context.Context, req LaunchRequest, opened PendingRun,
	box Sandbox, runner Runner, reused bool, want SessionContinuation) (continuedLaunch, string) {

	if why := c.continuesNoKept(); why != "" {
		return continuedLaunch{}, why
	}
	kept := opened.Kept
	if kept == nil || kept.LaunchSeq != want.LaunchSeq || kept.CaptureSeq != want.CaptureSeq {
		return continuedLaunch{}, "the turn's kept session is not the one its launch was asked to continue"
	}
	restorer, _ := runner.(SessionRestorer)
	row, err := c.restore.Session(ctx, req.Turn.AgentHandle, req.Turn.TurnID, RelaunchRead())
	if err != nil {
		return continuedLaunch{}, fmt.Sprintf("the turn's row on the executor log could not be read: %v", err)
	}
	// ASKED AGAIN of the record as this launch opened it, which numbers this
	// job one past the job it follows: a capture of that job that began or
	// landed since its caller asked is one the chosen capture no longer is
	// the newest beside.
	if continuity := ContinuityOf(row, &opened, opened.LaunchSeq-1, restorer); !continuity.Continues {
		return continuedLaunch{}, continuity.Why
	}
	brief, kind := want.Brief, BriefContinued
	if !reused {
		brief, kind = want.Replaced, BriefContinuedNewBox
	}
	if brief == "" {
		return continuedLaunch{}, "the launch was given no brief that continues the session in this box"
	}
	// THE BOX'S OWN COPY, where its harness exited cleanly in this very box
	// and its session was taken whole there: the box holds the archive, so
	// nothing is fetched to be kept over a copy it would not touch.
	if reused && !kept.Interrupted {
		return continuedLaunch{brief: brief, fresh: req.Brief, session: row.SessionID, kind: kind,
			kept: true, release: func() {}}, ""
	}
	archive := ArchiveOf(row)
	check, err := restorer.CheckCarry(ctx, box, archive)
	switch {
	case err != nil:
		return continuedLaunch{}, fmt.Sprintf("the box could not be asked whether its harness continues "+
			"the session: %v", err)
	case !check.Continues:
		return continuedLaunch{}, check.Why
	}
	fetched, err := c.fetchSession(ctx, row)
	if err != nil {
		return continuedLaunch{}, fmt.Sprintf("the session could not be fetched: %v", err)
	}
	body, err := fetched.open(c.restore)
	if err != nil {
		fetched.remove()
		return continuedLaunch{}, fmt.Sprintf("the session's spool could not be opened: %v", err)
	}
	// THE RESTORE CARRIER, chosen here and nowhere else: the turn's own kept
	// session, of the seat whose run this is — read off the turn's own row,
	// the capture of the job this launch follows, and opened against that
	// row's associated data, so no other seat's or turn's session opens into
	// the run — which replaces the box's copy where its harness was cut off.
	carried := &CarriedSession{Archive: archive, Body: body, Replace: kept.Interrupted}
	return continuedLaunch{brief: brief, fresh: req.Brief, session: row.SessionID, kind: kind,
		kept: true, carried: carried,
		release: func() {
			_ = body.Close()
			fetched.remove()
		}}, ""
}

// notContinued logs why a launch that was asked to continue its turn's session
// starts one of its own.
func (c *Coordinator) notContinued(ctx context.Context, req LaunchRequest, why string) {
	log.InfoContext(ctx, "executor_session_not_continued", "turn_id", req.Turn.TurnID,
		"agent", req.Turn.AgentHandle, "detail", redact.Secrets(why),
		"outcome", "the run starts a session of its own, handed the brief written for one")
}

// restoreSpoolPrefix names a launch's spool of the session it carries, under
// [spoolPrefix] and in the same directory, so a spool a crash left behind is
// swept with the captures' ([Coordinator.sweepSpools]).
const restoreSpoolPrefix = spoolPrefix + "restore-"

// fetchedSession is a kept session spooled SEALED on this node's disk, every
// byte vouched for by the object store and its seal opened whole once, ready
// to be carried into a box.
type fetchedSession struct {
	row  executordomain.Session
	file string
}

// fetchSession spools row's session sealed and checks it can be read whole,
// before any of it reaches a box ([executordomain.SessionStore.Fetch]) — in the
// coordinator's spool directory ([Coordinator.spoolDirectory]), where its
// sweep looks.
func (c *Coordinator) fetchSession(ctx context.Context, row executordomain.Session) (*fetchedSession, error) {
	c.sweepSpools()
	file, err := os.CreateTemp(c.spoolDirectory(), restoreSpoolPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("a spool for the session on this node's disk: %w", err)
	}
	fetched := &fetchedSession{row: row, file: file.Name()}
	if err = errors.Join(c.restore.Fetch(ctx, row, file), file.Close()); err != nil {
		fetched.remove()
		return nil, err
	}
	// THE SEAL OPENED WHOLE ONCE: a chunk that does not authenticate is
	// found here rather than by the run's start, part way through writing
	// the archive into its box.
	body, err := fetched.open(c.restore)
	if err == nil {
		_, err = io.Copy(io.Discard, body)
		err = errors.Join(err, body.Close())
	}
	if err != nil {
		fetched.remove()
		return nil, err
	}
	return fetched, nil
}

// open streams the spooled session opened; the caller closes it.
func (f *fetchedSession) open(restore SessionRestore) (io.ReadCloser, error) {
	file, err := os.Open(f.file)
	if err != nil {
		return nil, err
	}
	plain, err := restore.OpenSpooled(f.row, file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return spooledReader{Reader: plain, Closer: file}, nil
}

// remove deletes the spool.
func (f *fetchedSession) remove() { _ = os.Remove(f.file) }

// spooledReader is an opened spool: its plaintext, and the file it closes.
type spooledReader struct {
	io.Reader
	io.Closer
}
