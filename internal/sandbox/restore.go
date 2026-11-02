package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TURN'S OWN SESSION, CONTINUED IN WHICHEVER BOX ITS NEXT LAUNCH GETS.
//
// Every launch of a turn keeps its harness's session in the object store
// (session.go), named by the turn's row on the executor log. So a relaunch of
// the turn — a reviewer's send-back, a person's answer to a run that parked, a
// job lost with its host, a sign-in replaced while it ran — continues the turn's
// own NEWEST kept capture, in whatever box and cell it gets: the box the
// session was last worked in, its own copy kept, or any other, the archive
// restored there before its harness takes the session up ([RunRequest.Carried]).
//
// # Which session, and who decides
//
// ONE DECISION, [ContinuityOf]: a row that names a kept session, of a turn
// still live, in a format and by a release this build's runner for its harness
// restores. The launch's caller asks it before it builds the run's surface and
// briefs ([Coordinator.ContinuedCapture]), so what the run is offered and told
// is the session's; the launch asks it again against the box it got, and asks
// that box's harness whether it can take the session up
// ([SessionRestorer.CheckCarry]); and the run's own script restores it or
// starts a new session, handed both briefs ([RunRequest.FreshBrief]). Every
// reader that says whether a session continues — a seat's sessions, read —
// asks [ContinuityOf] too, so no second copy of the rule can disagree with the
// launch.
//
// ONLY THE NEWEST. A turn whose newest capture was refused, or never answered,
// continues none: the session that capture would have held went further than
// any kept one, and a run continued from an older one would believe it knew
// everything it did. A relaunch that carries its round on continues only a
// capture its round took ([roundsOwn]). And only within its turn: an ended
// turn's session is continued by nothing yet.
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

// ExecutorContextWindow is the most context, in tokens, an executor run's
// session is carried at: every Claude Code run compacts its conversation at it
// (CLAUDE_CODE_AUTO_COMPACT_WINDOW, internal/sandbox/executor), and a session
// whose last request carried more — an OpenCode session, whose compaction
// threshold is its model's own window less a share, never a figure the engine
// can set, or a harness release that ignores the variable — is not continued:
// its next launch starts a session of its own.
//
// ONE HUNDRED THOUSAND, and a judgment that trades a session's detail for what
// every request of a continued run costs. A continued run sends its session's
// whole context again on every request, and a seat's budget counts each one's
// whole input, its cached share included; so the window bounds what one
// request of a continued run can charge — four times the launch floor
// (turn_engine.executor_min_budget_tokens) — where a model's own window, a
// million tokens on some, would let a session carry ten times that. It is the
// least Claude Code accepts (100,000 to 1,000,000), and a coding run's durable
// state is its checkout and its turn's records rather than its transcript, so a
// session compacted at it keeps the work in hand and the summary of the rest.
// A larger window keeps more verbatim detail and charges more per request.
const ExecutorContextWindow = 100_000

// SessionContinuity is whether a launch may continue a turn's kept session, and
// where it may not, why — [ContinuityOf]'s answer.
type SessionContinuity struct {
	Continues bool
	Why       string
}

// ContinuityOf is THE decision whether a turn's kept session can be continued:
// the turn's row on the executor log names a kept session; the turn is LIVE —
// its run still running, its end not recorded — since a session is continued
// only by a later launch of the turn that kept it; and the runner a launch
// would take it up with restores it ([SessionRestorer.Restores]: its harness,
// its archive's format, a release whose sessions another box continues).
// restorer is that runner, nil where none of this build restores the row's
// harness's sessions. What only the box can say — whether its harness's own
// release takes the session up — is the launch's to ask of the box
// ([SessionRestorer.CheckCarry]).
func ContinuityOf(row executordomain.Session, live bool, restorer SessionRestorer) SessionContinuity {
	switch {
	case !row.HasObject():
		return SessionContinuity{Why: "the turn's row on the executor log names no kept session"}
	case !live || row.Ended():
		return SessionContinuity{Why: "the turn has ended, and a session is continued only by a later " +
			"launch of the turn that kept it"}
	case restorer == nil:
		return SessionContinuity{Why: fmt.Sprintf("no runner of this build restores a session %s wrote",
			row.Harness)}
	}
	if ok, why := restorer.Restores(archiveOf(row)); !ok {
		return SessionContinuity{Why: why}
	}
	return SessionContinuity{Continues: true}
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
}

// readThrough is the level a relaunch reads its turn's row at: the capture it
// continues may have been recorded by another node's keep, which this node's
// copy may not have applied yet.
var readThrough = statelog.Freshness{Level: statelog.ReadLinearizable}

// ContinuedCapture is the kept capture a relaunch of turnID continues — the
// turn's NEWEST kept capture, read once no capture of the turn is still being
// taken or kept ([Coordinator.awaitCapture]), where it is also the newest the
// turn attempted, its row on the executor log names it and [ContinuityOf]
// passes it for restorer — or why there is none, in words for the log.
//
// ASKED BY THE LAUNCH'S CALLER before it builds the run's surface and briefs,
// so what the run is offered and told is the session's; the launch takes up
// exactly the capture this answered ([SessionContinuation]), or none. Never an
// error: every reason a session cannot be continued is a launch that starts
// one of its own.
//
// carries says the relaunch CARRIES ITS ROUND ON ([LaunchRequest.Continue]: a
// person's answer, a loss, a replaced sign-in), which continues only a capture
// its round took ([roundsOwn]). A reviewer's send-back begins a round, and
// continues the session the round before it ended in.
func (c *Coordinator) ContinuedCapture(ctx context.Context, turnID string, restorer SessionRestorer,
	carries bool) (ContinuedSession, string) {

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
	// A CAPTURE STILL BEING KEPT is waited for, bounded by its own budget:
	// the job before this launch was collected moments ago, and its session
	// is the one this launch continues once its keep lands.
	run = c.awaitCapture(ctx, run)
	kept, newest := run.Kept, run.Capture
	switch {
	case kept == nil && newest == nil:
		return ContinuedSession{}, "the turn has kept no capture of its session"
	case kept == nil:
		return ContinuedSession{}, fmt.Sprintf("the turn's only capture of its session (launch %d, "+
			"capture %d) was not kept: %s", newest.LaunchSeq, newest.CaptureSeq, unkeptWhy(*newest))
	case newest != nil && newest.newerThan(*kept):
		return ContinuedSession{}, fmt.Sprintf("the turn's newest capture of its session (launch %d, "+
			"capture %d) was not kept: %s; the kept one before it is older than the session it "+
			"would continue", newest.LaunchSeq, newest.CaptureSeq, unkeptWhy(*newest))
	}
	if carries {
		if why := roundsOwn(*kept, run); why != "" {
			return ContinuedSession{}, why
		}
	}
	row, err := c.restore.Session(ctx, run.AgentHandle, turnID, readThrough)
	if err != nil {
		return ContinuedSession{}, fmt.Sprintf("the turn's row on the executor log could not be read: %v", err)
	}
	if row.LaunchSeq != kept.LaunchSeq || row.CaptureSeq != kept.CaptureSeq {
		return ContinuedSession{}, fmt.Sprintf("the executor log names the turn's capture (launch %d, "+
			"capture %d), and its run's record keeps (launch %d, capture %d)", row.LaunchSeq, row.CaptureSeq,
			kept.LaunchSeq, kept.CaptureSeq)
	}
	if continuity := ContinuityOf(row, true, restorer); !continuity.Continues {
		return ContinuedSession{}, continuity.Why
	}
	return ContinuedSession{Capture: *kept, Row: row}, ""
}

// roundsOwn is why kept, the turn's newest kept capture, cannot be continued by
// a relaunch that carries run's round on, "" where it can: it was taken in an
// earlier round than the one the relaunch carries on ([PendingRun.RoundLaunch]).
//
// A ROUND'S OWN SESSION OR NONE. A session an earlier round left holds neither
// the brief this round began with — a reviewer's send-back, what it asked of
// the round — nor this round's work, and the calls that continuation lists
// after it are numbered in this round's own order, which says nothing of where
// that session stopped. The newest capture can be an earlier round's only where
// this round's jobs left none, its box gone with its host before anything could
// be read off it.
func roundsOwn(kept SessionCapture, run PendingRun) string {
	if kept.LaunchSeq >= run.RoundLaunch {
		return ""
	}
	return fmt.Sprintf("the turn's newest kept session was saved by launch %d, before its round began at "+
		"launch %d, so it holds neither the brief the round began with nor the round's work", kept.LaunchSeq,
		run.RoundLaunch)
}

// unkeptWhy is why a capture is not the kept one, as its stamp says.
func unkeptWhy(c SessionCapture) string {
	switch c.State {
	case CaptureRefused:
		return "refused: " + c.Reason
	case CaptureUnresolved:
		return "its record was never answered"
	case CaptureBegun:
		return "it was begun and never finished"
	}
	return string(c.State)
}

// SessionContinuation is the turn's kept session a launch continues, as its
// caller chose it ([Coordinator.ContinuedCapture]), and the briefs that tell its
// run so: in the box the session was last worked in, and in any other.
type SessionContinuation struct {
	// LaunchSeq and CaptureSeq are the capture's place: the launch takes up
	// exactly that capture, which the run's record must still keep as the
	// turn's newest, or none.
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

// continuedLaunch is how one launch continues the turn's session, if it does:
// the brief its run is started with and the one it falls back to, the session
// it takes up and the archive carried into its box, and which brief it is.
type continuedLaunch struct {
	brief, fresh, session string
	carried               *CarriedSession
	kind                  BriefKind
	// release removes what the launch spooled to carry the session in, once
	// the run has started (or failed to).
	release func()
}

// freshLaunch is a launch that continues no session.
func freshLaunch(req LaunchRequest) continuedLaunch {
	return continuedLaunch{brief: req.Brief, kind: BriefFresh, release: func() {}}
}

// continueSession is how the launch req continues its turn's session in box —
// see the file comment — given the run's record as the launch opened it and
// whether box is the one the turn's previous job used (reused).
//
// IN THIS ORDER: the turn's kept capture its caller chose ([LaunchRequest.Session]),
// in any box; else, where the box is the asking or refused job's own and the
// turn kept no capture to continue, that job's session as the box holds it
// ([Continuation.SessionID]), as a run always could; else a session of its own.
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
func (c *Coordinator) continueKept(ctx context.Context, req LaunchRequest, opened PendingRun,
	box Sandbox, runner Runner, reused bool, want SessionContinuation) (continuedLaunch, string) {

	if c.restore == nil {
		return continuedLaunch{}, "this node continues no kept session"
	}
	kept := opened.Kept
	if kept == nil || kept.LaunchSeq != want.LaunchSeq || kept.CaptureSeq != want.CaptureSeq {
		return continuedLaunch{}, "the turn's kept session is not the one its launch was asked to continue"
	}
	// A ROUND CARRIED ON continues only a session of its round, as its caller
	// was told: the record as this launch opened it keeps the launch the round
	// began at.
	if req.Continue != nil {
		if why := roundsOwn(*kept, opened); why != "" {
			return continuedLaunch{}, why
		}
	}
	// AND STILL THE NEWEST, as the record opened this launch: a capture past
	// it that was not kept is a session that went further than the kept one.
	if newest := opened.Capture; newest != nil && newest.newerThan(*kept) {
		return continuedLaunch{}, fmt.Sprintf("the turn's newest capture of its session (launch %d, "+
			"capture %d) was not kept: %s", newest.LaunchSeq, newest.CaptureSeq, unkeptWhy(*newest))
	}
	restorer, _ := runner.(SessionRestorer)
	row, err := c.restore.Session(ctx, req.Turn.AgentHandle, req.Turn.TurnID, readThrough)
	switch {
	case err != nil:
		return continuedLaunch{}, fmt.Sprintf("the turn's row on the executor log could not be read: %v", err)
	case row.LaunchSeq != want.LaunchSeq || row.CaptureSeq != want.CaptureSeq:
		return continuedLaunch{}, "the executor log names another capture of the turn than the one the " +
			"launch was asked to continue"
	}
	if continuity := ContinuityOf(row, true, restorer); !continuity.Continues {
		return continuedLaunch{}, continuity.Why
	}
	brief, kind := want.Brief, BriefContinued
	if !reused {
		brief, kind = want.Replaced, BriefContinuedNewBox
	}
	if brief == "" {
		return continuedLaunch{}, "the launch was given no brief that continues the session in this box"
	}
	// THE BOX'S OWN COPY, where the job before ran in this very box and its
	// session was taken whole at its harness's exit: the box holds the
	// archive, so nothing is fetched to be kept over a copy it would not touch.
	if reused && kept.LaunchSeq == opened.LaunchSeq-1 && !kept.Interrupted {
		return continuedLaunch{brief: brief, fresh: req.Brief, session: row.SessionID, kind: kind,
			release: func() {}}, ""
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
	return continuedLaunch{brief: brief, fresh: req.Brief, session: row.SessionID, kind: kind,
		carried: carriedSession(archive, body, *kept),
		release: func() {
			_ = body.Close()
			fetched.remove()
		}}, ""
}

// carriedSession is the archive a launch hands its run to restore: the turn's
// own kept session, of the seat whose run this is — read off the turn's own row
// and opened against that row's associated data, so no other seat's or turn's
// session opens here — which replaces the box's copy where its harness was cut
// off ([CarriedSession.Replace]).
func carriedSession(archive SessionArchive, body io.Reader, kept SessionCapture) *CarriedSession {
	return &CarriedSession{Archive: archive, Body: body, Replace: kept.Interrupted}
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
