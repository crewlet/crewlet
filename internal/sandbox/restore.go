package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
// format and by a release the runner for its harness restores. The launch's
// caller asks it before it builds the run's surface and briefs
// ([Coordinator.ContinuedCapture]), the launch asks it again of the record as
// it opened the job, and the read surface asks it of each row of a seat's
// sessions (internal/api/queries' `continuable`), of the same two records. What
// only a launch can know is the launch's alone, and no reader states it:
// whether the box it got runs a harness that takes the session up
// ([SessionRestorer.CheckCarry]), and whether the seat's budget can carry the
// session's context beside the brief it hands the run (internal/engine's
// launch floor).
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

// ContinuityOf is THE decision whether a relaunch that follows the job at
// launch follows ([PendingRun.LaunchSeq]) can continue the turn's kept
// session — see the file comment: the turn live and not ending, the newest
// capture of that job kept, the turn's row on the executor log naming it, and
// the runner the launch would take it up with restoring it
// ([SessionRestorer.Restores]: its harness, its archive's format, a release
// whose sessions another box continues). row is the turn's row on the executor
// log; run the turn's run record as the caller read it, nil where the turn has
// none — its run is over; restorer nil where no runner of this build restores
// the row's harness's sessions.
//
// A TURN WHOSE END IS DECIDED IS ENDED HERE, whatever its row on the log says
// yet ([PendingRun.endDecided]): its end is owed on its run's record, or
// recorded already and its record not yet gone, and no relaunch of it follows.
// Read as live, a reader would say its session continues for the moments
// between the end's decision and the log's taking it.
//
// What the box a launch gets says of its harness's own release, and what the
// seat's budget says of the session's context, are the launch's to ask, and
// this decides neither.
func ContinuityOf(row executordomain.Session, run *PendingRun, follows int64,
	restorer SessionRestorer) SessionContinuity {

	switch {
	case run == nil || row.Ended():
		return SessionContinuity{Why: "the turn has ended, and no later turn continues its session yet, " +
			"since a session is continued only by a relaunch of the turn that kept it"}
	case run.endDecided():
		return SessionContinuity{Why: "the turn is ending: its end is decided, and no relaunch of it " +
			"follows to continue its session"}
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
	if ok, refused := restorer.Restores(archiveOf(row)); !ok {
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

// archiveOf is the archive a turn's row names, as a restorer and a run take it.
func archiveOf(row executordomain.Session) SessionArchive {
	return SessionArchive{Harness: row.Harness, SessionID: row.SessionID, Format: row.ArchiveFormat,
		Version: row.HarnessVersion}
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

// readThrough is the level a relaunch reads its turn's row at: the capture it
// continues may have been recorded by another node's keep, which this node's
// copy may not have applied yet.
var readThrough = statelog.Freshness{Level: statelog.ReadLinearizable}

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
// exactly the capture this answered ([SessionContinuation]), or none. Never an
// error: every reason a session cannot be continued is a launch that starts
// one of its own.
func (c *Coordinator) ContinuedCapture(ctx context.Context, turnID string,
	restorer SessionRestorer) (ContinuedSession, string) {

	if c.restore == nil {
		return ContinuedSession{}, "this node continues no kept session: it was built with nothing to restore one from"
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
	row, err := c.restore.Session(ctx, run.AgentHandle, turnID, readThrough)
	if err != nil {
		return answer, fmt.Sprintf("the turn's row on the executor log could not be read: %v", err)
	}
	if continuity := ContinuityOf(row, &run, run.LaunchSeq, restorer); !continuity.Continues {
		return answer, continuity.Why
	}
	answer.Capture, answer.Row = *run.Kept, row
	return answer, ""
}

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

	if c.restore == nil {
		return continuedLaunch{}, "this node continues no kept session"
	}
	kept := opened.Kept
	if kept == nil || kept.LaunchSeq != want.LaunchSeq || kept.CaptureSeq != want.CaptureSeq {
		return continuedLaunch{}, "the turn's kept session is not the one its launch was asked to continue"
	}
	restorer, _ := runner.(SessionRestorer)
	row, err := c.restore.Session(ctx, req.Turn.AgentHandle, req.Turn.TurnID, readThrough)
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
	archive := archiveOf(row)
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
// [spoolPrefix] so a spool a crash left behind is swept with the captures'
// ([Coordinator.sweepSpools]).
const restoreSpoolPrefix = spoolPrefix + "restore-"

// fetchedSession is a kept session spooled SEALED on this node's disk, every
// byte vouched for by the object store and its seal opened whole once, ready
// to be carried into a box.
type fetchedSession struct {
	row  executordomain.Session
	file string
}

// fetchSession spools row's session sealed and checks it can be read whole,
// before any of it reaches a box ([executordomain.SessionStore.Fetch]).
func (c *Coordinator) fetchSession(ctx context.Context, row executordomain.Session) (*fetchedSession, error) {
	c.sweepSpools()
	file, err := os.CreateTemp(os.TempDir(), restoreSpoolPrefix+"*")
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
