package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/redact"
)

// A TURN'S SESSION, CONTINUED IN WHICHEVER BOX ITS NEXT LAUNCH GETS — AND A
// CONVERSATION'S, BY ITS NEXT TURN.
//
// Every launch of a turn keeps its harness's session in the object store
// (session.go), named by the turn's row on the executor log. So a relaunch of
// the turn — a reviewer's send-back, a person's answer to a run that parked, a
// job lost with its engine whose box was left behind, a sign-in replaced while
// it ran — continues the session of THE JOB BEFORE IT, as that job's kept
// capture holds it, in whatever box and cell the relaunch gets: the box the
// session was last worked in, its own copy kept, or any other, the archive
// restored there before its harness takes the session up ([RunRequest.Carried]).
// And a conversation's NEXT TURN continues the session its last turn left — the
// conversation's head, read once at the turn's dispatch and frozen by its first
// launch (conversationbase.go) — in a box of its own.
//
// # Which session: the job before's own, the conversation's, or none
//
// A relaunch follows one job — the one its reviewer collected, the one whose
// question was answered, the one lost or refused — and only that job's session
// holds everything the turn's jobs were told: the round's task or the review
// that sent the round back, a person's answer, and the job's own work. A
// capture an EARLIER job took holds none of what the jobs after it were told,
// and the calls a continuation lists after it would be numbered in another
// round's order where a send-back began one. So a relaunch whose job before
// left no kept capture — its box went with its host, its take or its keep was
// refused, its record was never answered — continues none of the turn's own:
// it starts a session of its own, told what the jobs before it did. And only
// that job's NEWEST capture: a later one that was not kept is a session that
// went further than the kept one, and a run continued from the older would
// believe it knew everything it did. And only one whose stamp states its SAVE
// POINT ([SessionCapture.Through]), a count of the round's calls the session is
// known to hold — the run's log as the capture read it once its job was over,
// where its harness ended its own run, and else the newest point its launch
// was known to hold (savepoint.go) — which is what the continuation is told the
// calls after by: one an older build stamped states only the places it read
// the log at, which cannot say which calls came after it.
//
// EXCEPT WHILE THE TURN HAS KEPT NOTHING AT ALL: its first job lost with its
// box before any capture of it landed, its captures refused or unanswered. Then
// the relaunch continues the session the turn's first launch froze — the one
// the conversation's last turn left — told where this turn begins and then
// handed its own fresh brief or reseed, which say what the turn's jobs did:
// otherwise a first job lost with its box would end the conversation's session
// line for good. The turn's row on the log HOLDS that base while the turn is
// live and has kept nothing (its `begin`), so a sibling's end, the seat's
// budget and the retention pass it over; and once the turn keeps any session
// of its own, that one holds the base and everything after, and only the rule
// above applies. A base is continued ONLY IN A BOX OF ITS OWN: a box the turn's
// earlier job ran in holds that job's working tree and session, which are the
// rule above's to continue in place.
//
// ONE DECISION EACH, [ContinuityOf] and [BaseContinuityOf]. The first: the turn
// live and not ending, the job before's newest capture kept, stating its save
// point and named by the turn's row on the executor log, in a format and by a
// release a runner restores ([ArchiveRestorer]). The second: a base the turn
// froze — or, before its first launch, the head its dispatch read — the turn
// live, not ending and having kept nothing, its row holding that base, and a
// runner that restores it. The launch's caller asks it before it builds the
// run's surface and briefs ([Coordinator.ContinuedCapture], and
// internal/engine's probe of a first launch), of every member of the seat's
// chain, since which of them takes the launch is the launch's to pick; the
// launch asks it again of the record as it opened the job, of the runner of the
// member it picked; and the read surface asks it of each row of a seat's
// sessions (internal/api/queries' `continuable`) through [ReadContinuityOf] —
// of a live turn's row, by a relaunch, of the same two records read as a
// relaunch reads them; of an ended turn's row that is its conversation's head,
// by that conversation's next turn, [BaseContinuityOf] as that turn's first
// launch asks it — each of the member the node holding the seat would launch it
// on now ([RelaunchTarget]), with the half a person's answer takes beside the
// first, carrying a parked run on in the box it parked in where the kept
// capture is not continued, by the launch's own tests of a box it continues in
// ([PriorBox.ContinuesIn], [Coordinator.hostsPrior]). What only a launch can
// know is the launch's alone, and no reader states it: whether the box it got
// runs a harness that takes the session up ([SessionRestorer.CheckCarry]),
// whether that box is one of its own, and whether the seat's budget can carry
// the session's context beside the brief it hands the run (internal/engine's
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
// relaunch does not wait out on that frame. A keep still under way is then no
// kept capture, and the relaunch goes as one whose job's keep was refused does
// ([Coordinator.continuedWithout]): it continues the base the turn's first
// launch froze, where the turn has kept nothing and its row still holds that
// base — a base holds everything a session of its own would, and the
// conversation besides — and otherwise starts a session of its own, saying
// so, or carries its run on in the session its job's box holds, where it
// relaunches in that box. Should the capture land after the relaunch read the
// row, the relaunch's first landed capture supersedes it, as any newer
// capture does: what that job did is lost to the relaunch whichever session
// it starts in.
//
// WITHIN THE SAME WAIT, WHAT IS UNSETTLED IS SETTLED BEFORE THE DECISION: a
// capture whose taker is gone — its taker lease lapsed or held at another
// epoch ([Coordinator.settleGoneTaker], session.go) — is settled from the
// turn's row on the log, so a relaunch that follows its taker's crash or
// restart continues a capture whose record the log took before that read
// rather than starting fresh beside it; and a capture whose record was sent
// and never answered is asked again under its own operation, as the turn's end
// asks it ([Coordinator.settleUnresolved]): one that lands is continued. A
// record the log takes only after the settle's read — one its taker had handed
// on before its lease's window closed — lands beside a stamp that says refused
// (session.go, "WHAT THE FENCE CANNOT REACH"): the log keeps it, and the
// relaunch starts without it, told with the row in hand as a capture the log
// keeps, and its run named the turn's own as a session it may read
// ([namedThoughRefused], [ContinuedSession.KeptTurn]).
//
// # Never a session that ran out of context
//
// Another launch of it fails the same way, so its collection records it on the run's
// record ([PendingRun.ExhaustedSessions]) and refuses its capture, and each way a
// later launch could take it up refuses it: [ContinuityOf], [BaseContinuityOf] and
// [Coordinator.continueSession]. The turn's end retires it for later turns.
//
// # Never a failed launch
//
// An object the store no longer holds or whose seal does not open
// (executordomain.ErrUnreadable), a log that does not answer, a box whose
// harness cannot take the session up: each starts the run on a session of its
// own, handed the brief written for one, and is logged with its reason
// (`executor_session_not_continued`). The session is fetched SEALED onto this
// node's disk and opened whole once before a byte of it reaches the box
// ([executorkeep.TurnRowStore.Fetch]), so the decision is made before the
// brief is, never part way through writing the archive into the box.

// SessionRestore is what continuing a turn's kept session needs of the engine:
// the turn's row on the executor log, read through the ESTATE ROUTER'S reads
// (internal/estate's ExecutorReads) at the level a relaunch reads it
// (executordomain.RelaunchRead: a capture another node recorded is read at
// `linearizable`, which waits for this node's copy, or asks a data node whose
// copy has it), as the sandbox's view of it ([executorkeep.TurnRow]); and the
// sealed object it names, fetched and opened through the domain's session
// store. The row is the leaf's type because the sandbox links no state log:
// the log's own row type carries its positions, and so its graph.
type SessionRestore interface {
	// Session reads one turn's row (executordomain.Reader.Session, made the
	// sandbox's view by executordomain.Session.Kept), wrapping
	// [ErrTurnRowNotFound] where the log holds no row for the turn.
	Session(ctx context.Context, handle, turn string) (executorkeep.TurnRow, error)

	// Fetch copies the sealed object row names into dst, checked against the
	// row (executorkeep.TurnRowStore.Fetch), and OpenSpooled opens what it
	// copied against the row's associated data
	// (executorkeep.TurnRowStore.OpenSpooled).
	Fetch(ctx context.Context, row executorkeep.TurnRow, dst io.Writer) error
	OpenSpooled(row executorkeep.TurnRow, sealed io.Reader) (io.Reader, error)
}

// ErrTurnRowNotFound is [SessionRestore.Session]'s answer for a turn the
// executor log holds no row for: a turn whose begin never landed and that
// captured nothing, whose launches hold no base against its release. The
// engine wraps the log's own refusal (executordomain.ErrNotFound) in it, since
// the sandbox links no state log to compare against that one.
var ErrTurnRowNotFound = errors.New("sandbox: the executor log holds no row for the turn")

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
// launch follows ([PendingRun.LaunchSeq]) can continue the turn's kept session
// — see the file comment: the turn live and not ending, the newest capture of
// that job kept and stating its save point ([followedCapture]), the turn's row
// on the executor log naming it, that session not one a job of the turn ran out
// of context in, and the runner the launch would take it up with restoring it
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
// A CAPTURE THE LOG KEEPS WHERE THE RUN'S RECORD STAMPS IT REFUSED is not
// continued either, and the reason says the log keeps it
// ([namedThoughRefused]): the row is in hand here, as it is not where the
// record is answered alone ([followedCapture]), whose "was not kept" would
// tell a reader of the row the session it shows kept was lost.
//
// What the box a launch gets says of its harness's own release, and what the
// seat's budget and the window of the model it runs say of the session's
// context, are the launch's to ask, and this decides none of them.
func ContinuityOf(row executorkeep.TurnRow, run *PendingRun, follows int64,
	restorer ArchiveRestorer) SessionContinuity {

	switch {
	case run == nil || row.Ended:
		return SessionContinuity{Why: "the turn has ended, so no relaunch of it continues its session: a " +
			"session its turn ended with is continued only by its conversation's next turn, where its turn is " +
			"the conversation's head, the ended turn with a kept session that read the conversation last"}
	case run.endDecided():
		return SessionContinuity{Why: "the turn is ending: its run's ending is decided, and no relaunch " +
			"of it follows to continue its session"}
	}
	kept, why := followedCapture(*run, follows)
	if why != "" {
		if namedThoughRefused(*run, row, follows) {
			newest := *run.Capture
			why = fmt.Sprintf("the run's record stamps the newest capture of the job the relaunch follows "+
				"(launch %d, capture %d) %s; %s", newest.LaunchSeq, newest.CaptureSeq, unkeptWhy(newest),
				keptThoughRefused)
		}
		return SessionContinuity{Why: why}
	}
	switch {
	case !row.HasObject():
		return SessionContinuity{Why: "the turn's row on the executor log names no kept session"}
	case row.LaunchSeq != kept.LaunchSeq || row.CaptureSeq != kept.CaptureSeq:
		return SessionContinuity{Why: fmt.Sprintf("the executor log names the turn's capture (launch %d, "+
			"capture %d), and its run's record keeps (launch %d, capture %d)", row.LaunchSeq, row.CaptureSeq,
			kept.LaunchSeq, kept.CaptureSeq)}
	case run.sessionExhausted(row.SessionID):
		return SessionContinuity{Why: executorkeep.ExhaustedWhy(row.SessionID, run.TurnID)}
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
// follows continues — that job's newest capture, kept, stating its save point —
// or why there is none, in words for the log: the half of [ContinuityOf] the
// run's record answers alone, asked before the executor log is read.
//
// A KEPT CAPTURE THAT STATES NO SAVE POINT ([SessionCapture.Through]) IS NOT
// CONTINUED: an older build stamped it, placing it by the places it read the
// log at, which this build does not read — a long round's dropped middle hides
// a call in flight from them — so which of the round's calls came after it
// cannot be stated, as a session whose place cannot be stated is not kept. The
// relaunch starts a session of its own, told what the turn's jobs did; one
// that continues in the box its job parked in continues that box's own copy,
// which needs no stamp ([PriorBox]).
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
	if _, placed := kept.SavePoint(); !placed {
		return SessionCapture{}, fmt.Sprintf("the kept capture of the job the relaunch follows (launch %d, "+
			"capture %d) states no save point: an older version of the engine stamped it, so which of the "+
			"round's calls came after it cannot be told, and a session whose place cannot be stated is not "+
			"continued", kept.LaunchSeq, kept.CaptureSeq)
	}
	return *kept, ""
}

// namedThoughRefused reports whether the turn's row on the executor log names
// the newest capture of the job at follows where run's record stamps that
// capture REFUSED: a stamp decided with its record's landing out of its sight
// — a record its taker had already handed on before its lease's window closed,
// which the log took only after another node's settle had read the row
// (session.go, "WHAT THE FENCE CANNOT REACH"), or one sent and never answered,
// refused unasked once its object was past objstore.RecordWithin
// ([Coordinator.settleCapture]). The row's place is that capture's: the log
// takes a turn's captures only in the order of their places, and the record
// holds one take at each ([stampOnto]).
//
// THE LOG KEEPS IT, AND NO RELAUNCH OF THAT JOB CONTINUES IT. A relaunch
// continues only a capture its run's record keeps, and the record keeps no
// place it holds refused, whatever a later write says ([stampOnto]), so every
// attempt at the relaunch that follows the job — the one whose wait settled
// it, and one asked again after the record landed — decides as the stamp
// does. That is ADR-0073's decision for this order, which records no refusal
// of the settle's on the log, where the applier would refuse the late capture
// and lose a session; and a decision told with the row in hand says the log
// keeps it, never that it was not kept.
func namedThoughRefused(run PendingRun, row executorkeep.TurnRow, follows int64) bool {
	newest := run.Capture
	return stampedRefused(run, follows) &&
		row.HasObject() && row.LaunchSeq == newest.LaunchSeq && row.CaptureSeq == newest.CaptureSeq
}

// stampedRefused reports whether run's record stamps the newest capture of
// the job at follows refused: [namedThoughRefused] less the turn's row, which
// a relaunch reads for it only where this holds.
func stampedRefused(run PendingRun, follows int64) bool {
	newest := run.Capture
	return newest != nil && newest.State == CaptureRefused && newest.LaunchSeq == follows
}

// keptThoughRefused is what a relaunch's decision says of a capture
// [namedThoughRefused] reports, after what refused it on the run's record.
const keptThoughRefused = "the executor log keeps it all the same, the turn's row naming it — its record " +
	"landed unseen by that stamp, which is never taken back — and a relaunch continues only a capture its " +
	"run's record keeps, so no relaunch of that job continues it"

// ArchiveOf is the archive a turn's row names, as a restorer and a run take
// it: what a reader of whether a launch continues the row's session asks the
// node holding the seat about.
func ArchiveOf(row executorkeep.TurnRow) SessionArchive {
	return SessionArchive{Harness: row.Harness, SessionID: row.SessionID, Format: row.ArchiveFormat,
		Version: row.HarnessVersion}
}

// ContinuesIn is where a launch would continue a turn's session, as a reader of
// the session is told it ([ReadContinuityOf]).
type ContinuesIn string

const (
	// ContinuesInAnyBox is a session continued in whichever box the launch
	// gets: a live turn's kept capture, by a relaunch of the turn, restored
	// into another box or as its own copy in the box it was last worked in
	// ([ContinuityOf] says it continues); or an ended turn's that is its
	// conversation's head, by the conversation's next turn, restored into a
	// box of that turn's own ([BaseContinuityOf] says it continues).
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

// ContinuesIns are every place a reader is told a launch continues a session.
var ContinuesIns = []ContinuesIn{ContinuesInAnyBox, ContinuesInParkedBox}

// Valid reports whether w is one of [ContinuesIns]. An answer a newer build
// sent may name another, which is read as the value it is.
func (w ContinuesIn) Valid() bool { return slices.Contains(ContinuesIns, w) }

// ContinuedBy is which launch would continue a turn's session, as a reader of
// the session is told it ([ReadContinuityOf]): the launch whether the session
// is continued is decided for.
type ContinuedBy string

const (
	// ContinuedByRelaunch is a relaunch of the session's own turn, while that
	// turn is live and its run is neither over nor ending ([ContinuityOf]).
	ContinuedByRelaunch ContinuedBy = "relaunch"

	// ContinuedByNextTurn is the next turn of the session's conversation,
	// once the session's own turn has ended — or is ending, its end not yet
	// on the log as the reader read it — which continues the session only
	// where it is the conversation's head ([BaseContinuityOf]). A turn that
	// served no conversation has no next turn, and its reader is told no
	// launch continues its session at all.
	ContinuedByNextTurn ContinuedBy = "next_turn"

	// ContinuedByNextFire is [ContinuedByNextTurn] of a schedule's
	// conversation: the schedule's next fire, which continues the session
	// only where it is the schedule's head on the seat — of the ended fires
	// whose rows still name a session, the one that read the schedule's
	// conversation last — a schedule is a conversation of each seat that runs
	// it, every fire a turn of it (internal/notify's ScheduleConversation). The
	// same decision, told in the schedule's words.
	ContinuedByNextFire ContinuedBy = "next_fire"
)

// ContinuedBys are every launch a reader is told continues a session.
var ContinuedBys = []ContinuedBy{ContinuedByRelaunch, ContinuedByNextTurn, ContinuedByNextFire}

// Valid reports whether b is one of [ContinuedBys]. An answer a newer build
// sent may name another, which is read as the value it is.
func (b ContinuedBy) Valid() bool { return slices.Contains(ContinuedBys, b) }

// RelaunchAsk is one session a reader asks the node holding its seat about
// ([RelaunchTarget], [TargetAsk]): the turn a relaunch of which would continue
// it — or none, for the session a conversation's next turn would continue,
// which that turn's launch takes up under the revision the holder runs when it
// comes — and the archive a launch would take up.
type RelaunchAsk struct {
	Turn    string
	Archive SessionArchive
}

// RelaunchTarget is what a launch of a seat's turn would be launched on now — a
// relaunch of a live turn, or the next turn of a conversation — as the node
// holding the seat picks it (internal/engine): the harness of the first member
// of the seat's executor chain not sitting out its plan there, the cell that
// member's runs land in, that node, and whether that member's runner restores
// a session ([ArchiveRestorer]) — what every reader of whether a launch would
// continue a session decides by ([ReadContinuityOf]).
//
// THE HOLDER'S PICK, NEVER THE READER'S. Which members sit out their plans is
// what one node's own calls have met (internal/providers/llm/harness's bench,
// which no node shares), and which company revision says the switch is a node's
// too; a relaunch is decided on the node holding the seat, and a reader serving
// the API need not be that node.
//
// AND THE TURN'S, PICKED FROM THE CHAIN ITS RELAUNCH IS LAUNCHED ON: a resumed
// segment of the turn running on the holder relaunches — its reviewer's
// send-back, the round it carries on — on the company revision it was admitted
// under, which an apply since does not move; any other turn's next segment is
// admitted under the revision the holder runs when it comes, and so is a
// conversation's next turn, whose first launch continues the session the
// conversation's last turn left. So a target is asked for per turn, and for
// that next turn by none ([RelaunchAsk]): asked by the ended turn's own id, a
// segment of it still pinned in the moments before it returns would answer for
// a revision the next turn never runs on.
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

	// Undecided says what a launch would be launched on cannot be said now,
	// which Why says: every member of the seat's chain sitting out its plan,
	// where which of them takes the launch decides what it continues; no node
	// holding the seat; or the node holding it not answering. A launch now
	// waits until it can be said, and asks again then.
	Undecided bool

	// NoKept is why a launch of the seat's turn continues no kept session
	// whatever the turn kept, "" where it may: the company keeps none
	// ([NoKeptSession]), as the holder's revision says it and its
	// coordinator's own check says it at the launch
	// ([Coordinator.continuesNoKept]). A relaunch then continues at most the
	// session the box its run parked in holds, and a conversation's next turn
	// none.
	NoKept string
}

// restorer is the target's runner as [ContinuityOf] and [BaseContinuityOf] ask
// it: its own, or one that restores nothing and says why there is none.
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
		return false, "no runner of the member a launch is launched on restores a session"
	}
	return restorer.Restores(archive)
}

// refusedRestore is a restorer that takes no session up, for the reason it is:
// a relaunch target with no runner of its own to ask.
type refusedRestore string

func (r refusedRestore) Restores(SessionArchive) (bool, string) { return false, string(r) }

// ReadContinuity is whether a launch would continue a turn's session, as a
// reader of the session is told it ([ReadContinuityOf]): a relaunch of the
// turn, or the next turn of its conversation — its schedule's next fire, for a
// schedule's — which By says.
type ReadContinuity struct {
	// By is the launch the answer is about ([ContinuedBys]); empty where no
	// launch would ever continue the session — a turn that served no
	// conversation, once no relaunch of it follows.
	By ContinuedBy

	// Decided is false where it cannot be said now, which Why then says: the
	// launch would not decide it now either.
	Decided bool

	// Continues says the launch would continue the session, and In where.
	Continues bool
	In        ContinuesIn

	// Why is why the launch would not continue the session, why that cannot
	// be said now, or — where a relaunch continues it only in the box the run
	// parked in — why the turn's kept capture is continued in no other.
	Why string
}

// ReadContinuityOf is whether a launch would continue the session of the turn
// whose row on the executor log row is, as a reader is told it — THE DECISION
// THAT LAUNCH TAKES, asked of what that launch reads — and which launch that
// is ([ReadContinuity.By]). next is the launch that continues the session an
// ended turn of row's conversation left: [ContinuedByNextFire] where that
// conversation is a schedule's, [ContinuedByNextTurn] for any other, and any
// other value is read as the second. THE CALLER SAYS WHICH, by the
// conversation's key (internal/notify's Scheduled): this package cannot ask,
// since notify reaches it through internal/tracing, and a copy of the rule
// here would be a second place that knows a schedule's key.
//
//   - A LIVE TURN'S SESSION, BY A RELAUNCH OF IT: [ContinuityOf], of row, the
//     turn's run record as it stands (run), the job that record names as the
//     one a relaunch would follow and target's runner — the member the node
//     holding the seat would launch it on now; where that says no, the half
//     that carries a round on in the box its job ran in ([inParkedBox]), by the
//     launch's own tests of a box it continues in; and where that says no too,
//     whether a relaunch in a box of its own would continue instead the session
//     the turn's first launch was handed, the turn having kept none of its own
//     ([relaunchedBase]). row is read at the level a relaunch reads it
//     (executordomain.RelaunchRead) wherever the decision reaches it ([RereadsRow]): a copy
//     of the log behind the run's record reads a capture the relaunch would
//     continue as one the log no longer names.
//   - AN ENDED TURN'S, BY ITS CONVERSATION'S NEXT TURN: [BaseContinuityOf], the
//     decision that turn's first launch takes of the head its dispatch reads,
//     of row where it is its conversation's head — head is the turn the read
//     that answered row named its conversation's head, from the same snapshot
//     (executordomain's Page.Heads, SessionRead.Head) — and of target's runner,
//     the member the node holding the seat would launch that turn on, asked by
//     no turn ([TargetAsk]). A row that names no session says why
//     (executordomain.Session.NotKept); one that is not its conversation's
//     head names the head, the session the next turn is handed instead; and a
//     turn that served no conversation is continued by no launch at all.
//   - A LIVE TURN'S, ITS RUN OVER (run nil) OR ENDING
//     ([PendingRun.endDecided]): no relaunch of it follows, and whether its
//     conversation's next turn continues the session the row names is the
//     head's to say once the turn's end is on the log, where the read that
//     answered row did not find it — undecided; and continued by no launch, a
//     turn that served no conversation.
//
// UNDECIDED WHERE THE LAUNCH WOULD NOT DECIDE NOW EITHER, never a no: the job a
// relaunch would follow still working, or being collected — whose session is
// taken by the read that collects it, and kept off that read; the capture of
// that job still being kept, which a relaunch waits for ([relaunchCaptureWait]);
// what the launch would be launched on not to be said now
// ([RelaunchTarget.Undecided]); a relaunch that only the resume of the job's
// collection decides, as it reads how the job ended, until its turn begins and
// says what it does with the round ([LaunchRecord.ResumeCarry]) — whether a
// round lost a second time is carried on in a session of its own or reviewed
// from the work it submitted, and whether a box whose sign-in was replaced
// carries its round on; and an ended row whose conversation's head the read did
// not name beside it. Once the turn has said, the row is decided on what it
// said, its review included. now is the reader's clock, against which a begun
// capture is in flight ([captureInFlight]).
//
// As far as the engine can know before a box is chosen: whether the box a
// launch gets runs a harness release that takes the session up, and whether
// the seat's budget can carry it, are the launch's to find out.
func ReadContinuityOf(row executorkeep.TurnRow, head string, run *PendingRun, target RelaunchTarget,
	next ContinuedBy, now time.Time) ReadContinuity {

	later := laterLaunchOf(next)
	switch {
	case row.Ended:
		return nextTurnContinuity(row, head, target, later)
	case run == nil || run.endDecided():
		return endingContinuity(row, run, later)
	}
	read := relaunchContinuity(row, *run, target, later, now)
	read.By = ContinuedByRelaunch
	return read
}

// laterLaunch is the launch that continues the session an ended turn of a
// conversation left, as a reader is told it: what it is answered as, and the
// words its sentences name the conversation and its turns in.
type laterLaunch struct {
	by ContinuedBy

	// whole is what the conversation is called, turn what each of its turns
	// is: a conversation's turns, or a schedule's fires.
	whole, turn string
}

// laterLaunchOf is next as a reader is told it ([ReadContinuityOf]): a
// schedule's next fire, or any other conversation's next turn — what every
// value but [ContinuedByNextFire] is read as.
func laterLaunchOf(next ContinuedBy) laterLaunch {
	if next == ContinuedByNextFire {
		return laterLaunch{by: ContinuedByNextFire, whole: "schedule", turn: "fire"}
	}
	return laterLaunch{by: ContinuedByNextTurn, whole: "conversation", turn: "turn"}
}

// launch is the launch in a reader's words: "the conversation's next turn",
// "the schedule's next fire".
func (l laterLaunch) launch() string { return "the " + l.whole + "'s next " + l.turn }

// servedNoConversation is why no launch continues the session of a turn that
// served no conversation once no relaunch of it follows: a later turn
// continues only the session its own conversation's last turn left.
const servedNoConversation = "the turn served no conversation, so no later turn continues its session"

// nextTurnContinuity is [ReadContinuityOf] of an ended turn's row: whether its
// conversation's next turn — later — would continue its session, head being the
// turn the read that answered row named its conversation's head.
func nextTurnContinuity(row executorkeep.TurnRow, head string, target RelaunchTarget,
	later laterLaunch) ReadContinuity {

	not := func(why string) ReadContinuity {
		return ReadContinuity{By: later.by, Decided: true, Why: why}
	}
	switch {
	case row.ConversationKey == "":
		return ReadContinuity{Decided: true, Why: servedNoConversation}
	case !row.HasObject():
		return not(row.NotKept)
	case head == "":
		// UNKNOWN, NEVER A NO: a read of a row that names a session of an ended
		// turn names a head for its conversation — that row, if nothing newer —
		// so a read that names none did not say which session the next turn
		// is handed, which is no reason to say it is not this one.
		return ReadContinuity{By: later.by, Why: "the read of this session did not say which of its " +
			later.whole + "'s sessions is the newest in its line; ask again shortly"}
	case head != row.Turn:
		return not(fmt.Sprintf("%s is handed %s %s's session, the newest in the %s's line, and is told "+
			"how this one ended where its brief has room", later.launch(), later.turn, head, later.whole))
	case target.Undecided:
		return ReadContinuity{By: later.by, Why: target.Why}
	case target.NoKept != "":
		return not(target.NoKept)
	}
	if continuity := BaseContinuityOf(baseSessionOf(row), nil, nil, target.restorer()); !continuity.Continues {
		return not(continuity.Why)
	}
	return ReadContinuity{By: later.by, Decided: true, Continues: true, In: ContinuesInAnyBox}
}

// endingContinuity is [ReadContinuityOf] of a live turn's row whose run is over
// (run nil) or ending: decided only for a turn that served no conversation,
// which no later turn continues; for any other, whether its conversation's next
// turn — later — continues the session the row names is decided once the turn's
// end is on the log, where the read that answered row did not find it.
func endingContinuity(row executorkeep.TurnRow, run *PendingRun, later laterLaunch) ReadContinuity {
	over := "the turn's run is over"
	if run != nil {
		over = "the turn is ending"
	}
	if row.ConversationKey == "" {
		return ReadContinuity{Decided: true, Why: over + "; " + servedNoConversation}
	}
	return ReadContinuity{By: later.by, Why: over + ", and its end is not on the executor log as this " +
		"session was read; once it is, " + later.launch() + " continues the session this row names if it " +
		"is then the newest in the " + later.whole + "'s line"}
}

// relaunchContinuity is [ReadContinuityOf] of a live turn's row whose run is
// neither over nor ending: whether a relaunch of the turn would continue its
// session — later naming the turn's base, where a relaunch would continue that
// instead, as its conversation names its turns.
func relaunchContinuity(row executorkeep.TurnRow, run PendingRun, target RelaunchTarget,
	later laterLaunch, now time.Time) ReadContinuity {

	switch {
	case jobWorking(run):
		return ReadContinuity{Why: fmt.Sprintf("the job a relaunch would follow (launch %d) is still "+
			"working or being collected, and its session is taken and kept by the read that collects it",
			run.LaunchSeq)}
	case captureInFlight(run, now):
		return ReadContinuity{Why: fmt.Sprintf("the capture of the job a relaunch would follow (launch %d, "+
			"capture %d) is still being kept", run.Capture.LaunchSeq, run.Capture.CaptureSeq)}
	case target.Undecided:
		return ReadContinuity{Why: target.Why}
	}
	kept := ContinuityOf(row, &run, run.LaunchSeq, target.restorer())
	if target.NoKept != "" {
		kept = SessionContinuity{Why: target.NoKept}
	}
	if kept.Continues && run.Lost != nil && !KeptAfterLoss(run.RoundLosses+1) {
		// A ROUND LOST AGAIN is carried on in a session of its own, never from
		// the state it was lost in — unless its job had submitted its work
		// before it went, when the round is collected and only a review that
		// sends it back would continue it: which the job's resume decides,
		// and says as its turn begins.
		switch run.Launch.ResumeCarry {
		case ResumeCarriesOn:
			return ReadContinuity{Decided: true, Why: fmt.Sprintf("the job a relaunch would follow "+
				"(launch %d) was lost a second time in its round, and its resumed turn carries the round on "+
				"in a session of its own", run.LaunchSeq)}
		case ResumeCollects:
			// THE ROUND IS COLLECTED from the work the job submitted, and a
			// review's send-back continues its kept capture: decided below.
		default:
			return ReadContinuity{Why: fmt.Sprintf("the job a relaunch would follow (launch %d) was lost a "+
				"second time in its round, which a relaunch carries on in a session of its own; only a "+
				"review of work it submitted before it went would continue it, which its resume decides",
				run.LaunchSeq)}
		}
	}
	if kept.Continues {
		return ReadContinuity{Decided: true, Continues: true, In: ContinuesInAnyBox}
	}
	box, why := inParkedBox(run, row, target, now)
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
	if base := relaunchedBase(row, run, target, later); base != "" {
		reason += "; " + base
	}
	return ReadContinuity{Decided: true, Why: reason}
}

// relaunchedBase is what a reader is told a relaunch of run's turn continues in
// place of a session of the turn's own, "" where it continues none: THE SESSION
// THE TURN'S FIRST LAUNCH WAS HANDED, where the turn has kept none and its row
// on the log still holds it — [BaseContinuityOf] of row, read as a relaunch
// reads it, the question a relaunch asks itself
// ([Coordinator.ContinuedCapture]) — for target's runner, and where the round
// may be continued from a session at all: a round lost a second time starts
// one of its own, unless the turn the job's collection resumed collects what
// the job submitted, whose review's send-back continues the base; and while
// that resume has not said which, nothing is claimed. IN A BOX OF ITS OWN, which
// is the launch's to get, and so said: a relaunch into the box the turn's job
// ran in continues the session that box holds instead
// ([Coordinator.continueBase]). The base is named as later names the turns of
// row's conversation: a schedule's base is its last fire.
func relaunchedBase(row executorkeep.TurnRow, run PendingRun, target RelaunchTarget,
	later laterLaunch) string {

	switch {
	case target.NoKept != "" || run.Base == nil || run.Base.Session == nil:
		return ""
	case run.Lost != nil && !KeptAfterLoss(run.RoundLosses+1) && run.Launch.ResumeCarry != ResumeCollects:
		return ""
	}
	base := *run.Base.Session
	if !BaseContinuityOf(base, &row, &run, target.restorer()).Continues {
		return ""
	}
	return fmt.Sprintf("a relaunch of the turn in a box of its own continues %s %s's session instead, which "+
		"the turn's first launch was handed", later.turn, base.Turn)
}

// TargetAsk is what a reader asks the node holding row's seat about it
// ([RelaunchTarget]), and false where [ReadContinuityOf] decides row by no
// target, so a reader asks about exactly the rows a launch's member decides.
//
// A LIVE TURN'S ROW whose run is neither over nor ending is asked by its turn,
// whose relaunch is launched on the revision a segment of the turn running on
// the holder pins; and of the archive a relaunch would take up — the row's,
// or, where the row names no session, the one the turn's first launch was
// handed, which a relaunch of a turn that has kept none continues
// ([relaunchedBase]): the holder's runner answers for exactly the archive it is
// asked about. AN ENDED ROW THAT IS ITS CONVERSATION'S HEAD, head being the turn
// the read that answered row named its conversation's head, is asked by NO
// turn: the conversation's next turn is admitted under the revision the holder
// runs when it comes, never one a segment of the ended turn still pins in the
// moments before that segment returns.
func TargetAsk(row executorkeep.TurnRow, head string, run *PendingRun) (RelaunchAsk, bool) {
	switch {
	case row.Ended:
		if row.ConversationKey == "" || !row.HasObject() || head != row.Turn {
			return RelaunchAsk{}, false
		}
		return RelaunchAsk{Archive: ArchiveOf(row)}, true
	case run == nil || run.endDecided():
		return RelaunchAsk{}, false
	}
	archive := ArchiveOf(row)
	if !row.HasObject() && run.Base != nil && run.Base.Session != nil {
		archive = run.Base.Session.archive()
	}
	return RelaunchAsk{Turn: row.Turn, Archive: archive}, true
}

// RereadsRow reports whether a reader that read row below the level a relaunch
// reads it (executordomain.RelaunchRead) must read it again there before deciding its
// continuity: only where the decision reaches the row — the turn live and not
// ending, its job neither working nor its capture still being kept — and the
// row names another capture than the one run's record keeps, or the record
// keeps none: whether a relaunch of a turn that has kept nothing continues the
// session its first launch was handed is decided on the row's hold as the
// relaunch reads it ([relaunchedBase]).
//
// A ROW THAT NAMES THE RECORD'S KEPT CAPTURE DECIDES AS ONE READ AT THE
// RELAUNCH'S LEVEL DOES: a newer capture is stamped on the run's record before
// a byte of it moves, so a record that keeps this one has seen no newer land.
// Read again everywhere else, every look at a seat's sessions — a dashboard
// asks every minute — appended a barrier to the executor log for each live row
// (statelog's linearizable read), one after another, for an answer the row it
// held already gave.
func RereadsRow(row executorkeep.TurnRow, run *PendingRun, now time.Time) bool {
	if run == nil || row.Ended || run.endDecided() || jobWorking(*run) || captureInFlight(*run, now) {
		return false
	}
	kept := run.Kept
	return kept == nil || row.LaunchSeq != kept.LaunchSeq || row.CaptureSeq != kept.CaptureSeq
}

// jobWorking reports whether run's job is still working or being collected, so
// no reader can say yet what a relaunch following it continues: a launch not
// yet suspended, a job running whose box no collection has read back
// ([LaunchRecord.Collected]), or a claim of a job whose read is still under
// way. The read that collects a job takes its session first, and the claim's
// turn begins once it has, saying as it does what it does with the round
// ([LaunchRecord.ResumeCarry]); the claim's row then stays resumed for the
// whole resumed turn, its review included — which is when a person most wants
// to know — and is decided as any other from the first of the two on: the
// job's final capture taken, and settled ([captureInFlight]), or the turn
// begun, whether or not its read took any capture (the switch off, a box
// holding no session). A claim of a person's answer reads no box at all: its
// job was collected when it parked on its question.
func jobWorking(run PendingRun) bool {
	switch run.Status {
	case StatusLaunching:
		return true
	case StatusResumed:
		if run.Answer != nil || run.Launch.ResumeCarry != "" {
			return false
		}
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
	// boxContinues: a person's answer's relaunch would, or the relaunch a
	// resumed turn carries its round on with at once.
	boxContinues
	// boxUndecided: one would where the resume of the job's collection
	// carries its round on in its box, which only that resume decides, and
	// has not said yet.
	boxUndecided
)

// inParkedBox is whether a relaunch carrying run's round on in the box its job
// ran in would continue the session row names, there — and where it would not,
// or cannot be said, why; "" where no relaunch carries the round on in a box.
//
// WHICH RELAUNCHES DO: a person's answer to the run's question — parked on it,
// its box reaped while it waited, answered, or that answer's claim under way
// ([PendingRun.Answer]); and the resume of a collected job whose sign-in was
// replaced while it ran, which only that resume decides, as it reads how the
// job ended, and says as its turn begins ([LaunchRecord.ResumeCarry]): before
// then a reader is told it cannot be said, and after it, what the turn said —
// carried on in the box at once, or collected, its review's send-back the
// round's only relaunch, which continues nothing there. Never a lost job's,
// whose box holds what its harness was cut off in.
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
func inParkedBox(run PendingRun, row executorkeep.TurnRow, target RelaunchTarget, now time.Time) (boxCarry, string) {
	carried, undecided := boxContinues, ""
	switch {
	case run.Status == StatusAwaiting || run.Status == StatusReseed || run.Status == StatusAnswered ||
		(run.Status == StatusResumed && run.Answer != nil):
	case run.Lost != nil:
		return boxNot, ""
	case run.Status == StatusResumed && run.Launch.ResumeCarry == ResumeCarriesOn:
		// THE RESUMED TURN CARRIES THE ROUND ON, relaunching it at once in the
		// job's box: the job refused on a sign-in replaced while it ran.
	case run.Status == StatusResumed && run.Launch.ResumeCarry == ResumeCollects:
		return boxNot, "the resumed turn collects its job, and a review's send-back, the round's only " +
			"relaunch from it, continues no session in the box the job ran in"
	case run.Status == StatusResumed && run.Launch.ResumeCarry != "":
		carried, undecided = boxUndecided, fmt.Sprintf("the resumed turn of its job's collection said "+
			"what it does with the round as %q, which this build does not read", run.Launch.ResumeCarry)
	case run.Status == StatusResumed || (run.Status == StatusRunning && run.Launch.Collected):
		carried, undecided = boxUndecided, "whether the resume of its job's collection carries its "+
			"round on in the box it ran in, which it does where the job's sign-in was replaced while it "+
			"ran, is decided as that resume reads how the job ended, and said as its turn begins"
	default:
		return boxNot, ""
	}
	ran := run.SessionRan()
	switch {
	case !run.HasBox():
		return boxNot, "the box its run ran in is gone"
	case run.sessionExhausted(ran):
		return boxNot, executorkeep.ExhaustedWhy(ran, run.TurnID)
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
		Status: StatusResumed, LaunchNode: run.LaunchNode, Since: run.BoxSince}
	switch {
	// THE BOX'S AGE FIRST, with a reason of its own: the launch's test of a box
	// it continues in refuses one past [KeptBoxMaxAge], or one the row never
	// dated, whatever its cell and harness ([PriorBox.ContinuesIn]).
	case run.BoxSince.IsZero():
		return boxNot, "the box its run ran in was never dated, so no age of it holds to the bound a " +
			"relaunch continues in a box within"
	case now.Sub(run.BoxSince) >= KeptBoxMaxAge:
		return boxNot, fmt.Sprintf("the box its run ran in was first attached %s, and a relaunch "+
			"continues only in a box younger than %d days", run.BoxSince.UTC().Format(time.RFC3339),
			int(KeptBoxMaxAge/(24*time.Hour)))
	case !prior.ContinuesIn(Spec{Placement: target.Placement, Harness: target.Harness}, now):
		return boxNot, fmt.Sprintf("the box its run ran in runs %s in the %s cell, and a relaunch is "+
			"launched on %s in the %s cell", run.Harness, run.Placement, target.Harness, target.Placement)
	case !prior.ReachableFrom(target.Node, target.Placement):
		return boxNot, fmt.Sprintf("the box its run ran in is on the host of %s, and %s, which holds "+
			"the seat, cannot continue in it", run.LaunchNode, target.Node)
	}
	return carried, undecided
}

// ContinuedSession is the session a relaunch of a turn continues: the kept
// capture of the job it follows — the run's record's stamp of it, what it
// accounts for, where it was worked, what its last request carried, and the
// turn's row on the executor log that names its object — or, where the turn has
// kept none, the session its conversation's last turn left, as its first
// launch froze it (Base).
type ContinuedSession struct {
	Capture SessionCapture
	Row     executorkeep.TurnRow

	// Base is the session the conversation's last turn left, which the
	// relaunch continues in place of a kept capture of the turn's own; nil
	// where it continues none, or a kept capture.
	Base *BaseSession

	// Context is what the last request of the session of the job the
	// relaunch follows carried, as that job's newest capture measured it,
	// KEPT OR NOT: what a launch that continues that session sends again
	// before its brief, restored from the capture or as the job's own box
	// holds it ([LaunchRequest.Continue]). Set whether or not the capture is
	// continued; zero where no capture of the job measured any.
	Context int64

	// KeptTurn is the turn whose kept session the relaunch's read found,
	// continued or not: the turn's own where its run's record says a capture of
	// it landed, once what was unsettled is settled — a live turn's row is
	// released by no budget, absorption or retention, so it still names one,
	// unless the row read says it names none — or where the turn's row, read,
	// names a capture of the turn the record does not keep: one it stamps
	// refused ([namedThoughRefused]), or one whose keep reached the log ahead of
	// its stamp ([Coordinator.continuedWithout]); else the base its first launch
	// froze, where the turn's own row was read holding it against every release
	// but a retirement ([baseStanding]); empty where neither was read so: a base
	// whose begin never landed, or found it gone, may be gone, and a run is
	// never told a session is there to read that the read did not find held.
	// What a run that starts a session of its own beside it is told it may read
	// (internal/agent/runner's SessionBasis.KeptTurn).
	KeptTurn string
}

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

// ContinuedCapture is the session a relaunch of turnID continues — the kept
// capture of the job the relaunch follows, as [ContinuityOf] decides for
// restorer, read once that job's capture is no longer being kept, for at most
// [relaunchCaptureWait]; else, where that job left no kept capture — one still
// being kept past that wait among them — and the turn has kept none at all,
// the session its conversation's last turn left as its first launch froze it,
// as [BaseContinuityOf] decides of the turn's own row ([ContinuedSession.Base],
// [Coordinator.continuedWithout]) — or why there is none, in words for the
// log; and either way what that job's session's last request carried
// ([ContinuedSession.Context]).
//
// WHAT IS UNSETTLED IS SETTLED FIRST, within that same wait — see the file
// comment: a capture whose taker is gone, from the turn's row on the log, as
// every wait for a capture settles it ([Coordinator.awaitCapture]); and the
// followed job's capture whose record was never answered, asked again under
// its own operation ([Coordinator.settleUnresolved]) — asked by the turn's end
// alone, it left a relaunch reading a record the log had taken as one never
// answered, and starting a session of its own beside it.
//
// ASKED BY THE LAUNCH'S CALLER before it builds the run's surface and briefs,
// so what the run is offered and told is the session's; the launch takes up
// exactly the session this answered ([SessionContinuation]), or none, as the
// runner of the member it picks restores it. Never an error: every reason a
// session cannot be continued is a launch that starts one of its own. NEVER A
// READ OF THE CONVERSATION'S HEAD: the base is the one the turn froze, however
// the conversation moved since.
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
	// ONE WAIT FOR ALL OF IT, on the frame that relaunches: what the wait
	// below leaves of it is what an ask of an unanswered record gets.
	settleBy := time.Now().Add(c.relaunchWait)
	run, found, err = c.awaitCapture(ctx, run, c.relaunchWait)
	if !found {
		return ContinuedSession{}, "the turn's run record was gone before the capture of the job " +
			"the relaunch follows settled"
	}
	answer := ContinuedSession{Context: followedContext(run)}
	if err != nil {
		// NO ANSWER IS NO KEPT CAPTURE: the row the wait could not read again
		// may hold a capture since kept, or refused, and a relaunch never
		// continues one it has not seen kept.
		if run.Kept != nil {
			answer.KeptTurn = turnID
		}
		return answer, fmt.Sprintf("what came of the capture of the job the relaunch follows "+
			"could not be read: %v", err)
	}
	var why string
	if captureInFlight(run, c.now()) {
		why = fmt.Sprintf("the capture of the job the relaunch follows (launch %d, capture %d) "+
			"was still being kept %s after the relaunch asked for it, which the relaunch does not wait out",
			run.Capture.LaunchSeq, run.Capture.CaptureSeq, c.relaunchWait)
	} else {
		if newest := run.Capture; newest != nil && newest.State == CaptureUnresolved &&
			newest.LaunchSeq == run.LaunchSeq {
			ask, cancel := context.WithDeadline(ctx, settleBy)
			run = c.settleUnresolved(ask, settleBy, run)
			cancel()
		}
		_, why = followedCapture(run, run.LaunchSeq)
	}
	// THE TURN'S OWN WHERE ITS RECORD KEEPS A CAPTURE OF IT, read after the
	// settle: an ask of an unanswered record that lands it makes the turn's own
	// the session a run that starts one of its own may read.
	if run.Kept != nil {
		answer.KeptTurn = turnID
	}
	if why != "" {
		return c.continuedWithout(ctx, run, restorer, answer, why)
	}
	row, err := c.restore.Session(ctx, run.AgentHandle, turnID)
	if err != nil {
		return answer, fmt.Sprintf("the turn's row on the executor log could not be read: %v", err)
	}
	if !row.HasObject() {
		answer.KeptTurn = ""
	}
	if continuity := ContinuityOf(row, &run, run.LaunchSeq, restorer); !continuity.Continues {
		return answer, continuity.Why
	}
	answer.Capture, answer.Row = *run.Kept, row
	return answer, ""
}

// continuedWithout is what a relaunch continues where the job it follows left
// no kept capture — one still being kept past [relaunchCaptureWait], or one
// not kept, why saying which as the run's record tells it: the base the turn
// froze, where it has kept nothing and its row still holds that base
// ([frozenBase]); else nothing, why going on to say why not.
//
// A KEEP STILL UNDER WAY IS NO KEPT CAPTURE, and goes the same way: the
// relaunch that will not wait it out starts from what the turn is known to
// hold, as one whose job's keep was refused does — the base, while the turn's
// row holds it, since it holds everything a session of its own would and the
// conversation besides. Should the capture land after the row was read, the
// relaunch's first landed capture supersedes it as any newer capture does,
// and what that job did is lost to the relaunch either way; read before, the
// row holds the base no longer, and the relaunch continues none.
//
// THE TURN'S ROW IS READ ONCE, and only where something asks it: the base,
// and a newest capture of the job the run's record stamps REFUSED, which the
// row may name all the same ([namedThoughRefused]) — a record its taker had
// handed on before its lease's window closed and the log took only after
// another node's settle had read the row. There the stamp still decides, and
// no relaunch of that job continues the capture; but the reason says the log
// keeps it, after the record's own account of it, "not kept", and before why
// no base is continued, which would otherwise read as a row that landed
// another capture.
//
// AND WHAT THE ROW SAYS IS WHAT THE RUN IS TOLD IT MAY READ, where it was
// read ([ContinuedSession.KeptTurn]): a capture of the turn's own that it
// names is there to read whatever the run's record stamps it — refused as
// above, or still begun where its keep reached the log ahead of its stamp —
// and a row that names none has none to read.
func (c *Coordinator) continuedWithout(ctx context.Context, run PendingRun, restorer ArchiveRestorer,
	answer ContinuedSession, why string) (ContinuedSession, string) {

	read := false
	ownRow := sync.OnceValues(func() (executorkeep.TurnRow, error) {
		read = true
		return c.restore.Session(ctx, run.AgentHandle, run.TurnID)
	})
	if stampedRefused(run, run.LaunchSeq) {
		if own, err := ownRow(); err == nil && namedThoughRefused(run, own, run.LaunchSeq) {
			why += "; " + keptThoughRefused
		}
	}
	base, held, baseWhy := frozenBase(run, ownRow, restorer)
	switch {
	case held:
		answer.KeptTurn = run.Base.Session.Turn
	case read:
		if own, err := ownRow(); err == nil {
			answer.KeptTurn = ""
			if own.HasObject() {
				answer.KeptTurn = run.TurnID
			}
		}
	}
	if base != nil {
		answer.Base = base
		return answer, ""
	}
	if baseWhy != "" {
		why += "; " + baseWhy
	}
	return answer, why
}

// frozenBase is the session run's turn froze at its first launch, where a
// relaunch of a turn that has kept nothing continues it ([BaseContinuityOf] of
// the turn's own row, as ownRow reads it at the level a relaunch reads), or why
// not; and whether that row was read holding it against every release but a
// retirement ([baseStanding]), continued or not. nil, false and "" where the
// turn froze none, which says nothing more than the kept capture's absence
// did.
func frozenBase(run PendingRun, ownRow func() (executorkeep.TurnRow, error),
	restorer ArchiveRestorer) (*BaseSession, bool, string) {

	if run.Base == nil || run.Base.Session == nil {
		return nil, false, ""
	}
	base := *run.Base.Session
	if run.Kept != nil {
		return nil, false, BaseContinuityOf(base, nil, &run, restorer).Why
	}
	own, err := ownRow()
	switch {
	case errors.Is(err, ErrTurnRowNotFound):
		return nil, false, fmt.Sprintf("the turn's row on the executor log holds nothing: its begin never "+
			"landed, so turn %s's session is held against no release", base.Turn)
	case err != nil:
		return nil, false, fmt.Sprintf("the turn's row on the executor log could not be read to see whether "+
			"it holds turn %s's session: %v", base.Turn, err)
	}
	held := baseStanding(base, &own, &run) == ""
	if continuity := BaseContinuityOf(base, &own, &run, restorer); !continuity.Continues {
		return nil, held, continuity.Why
	}
	return &base, held, ""
}

// continuesNoKept is why this coordinator continues no kept session at all,
// "" where it may: a node built with nothing to restore one from, or a company
// that keeps no session as this node's revision says ([SessionKeep.Keeps]) —
// `turn_engine.conversation_session.enabled: false`, under which no session is
// kept, a relaunch continues at most the one its own box holds, whatever a
// capture kept while the switch was on, and no turn continues the one its
// conversation's last turn left.
func (c *Coordinator) continuesNoKept() string {
	switch {
	case c.restore == nil:
		return "this node continues no kept session: it was built with nothing to restore one from"
	case c.keep != nil && !c.keep.Keeps():
		return NoKeptSession
	}
	return ""
}

// NoKeptSession is why a launch continues no kept session in a company that
// keeps none: what a launch logs and a reader of a session is told.
const NoKeptSession = "the company keeps no executor session (turn_engine.conversation_session.enabled " +
	"is false), so a relaunch continues at most the session its own box holds, and a conversation's next " +
	"turn, or a schedule's next fire, continues none"

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

// SessionContinuation is the session a launch continues, as its caller chose it
// ([Coordinator.ContinuedCapture]), and the briefs that tell its run so: in the
// box the session was last worked in, and in any other.
type SessionContinuation struct {
	// LaunchSeq and CaptureSeq are the capture's place: the launch takes up
	// exactly that capture, which the run's record must still keep as the
	// newest of the job this launch follows, or none.
	LaunchSeq, CaptureSeq int64

	// Base says the session is the one the turn's conversation's last turn
	// left, as the turn's first launch froze it ([PendingRun.Base]), in place
	// of a capture of the turn's own: continued only in a box of its own, so
	// Brief is empty and Replaced is what the run is handed
	// ([Coordinator.continueBase]).
	Base bool

	// Brief is the run's prompt where the box is the one the session was
	// last worked in, its working tree as the job before left it.
	Brief string

	// Replaced is the run's prompt where the box is another — a fresh
	// checkout: Brief and what a box that replaced another changes (which
	// branch to check out, the uncommitted work gone, the earlier box's
	// absolute paths).
	Replaced string

	// Told is what Brief and Replaced told of the round the launch carries
	// on, the same for both since they tell the same calls ([BriefTold]):
	// recorded on the job the launch opens where it hands either.
	Told BriefTold
}

// BriefKind is which of the four briefs a launch handed its run: a run that
// continues the turn's session in the box it was last worked in, one that
// continues it in a box that replaced that one, one that continues the session
// its conversation's last turn left, and one that starts a session of its own.
type BriefKind string

// The four briefs, as a launch's record and its collection name them: the
// turn's session in the box it was last worked in, the turn's session restored
// into another box, the session the conversation's last turn left, restored
// into a box of the launch's own, and a session of the run's own.
const (
	BriefContinued             BriefKind = "continued"
	BriefContinuedNewBox       BriefKind = "continued_new_box"
	BriefContinuedConversation BriefKind = "continued_conversation"
	BriefFresh                 BriefKind = "fresh"
)

// BriefKinds are the four.
var BriefKinds = []BriefKind{BriefContinued, BriefContinuedNewBox, BriefContinuedConversation, BriefFresh}

// Valid reports whether k is one of [BriefKinds]. A record a newer build wrote
// may name one this build does not know, which is read as the value it is
// rather than refused.
func (k BriefKind) Valid() bool { return slices.Contains(BriefKinds, k) }

// Continues reports whether k is a brief that continues a session: a run handed
// one takes the session up where its box can, wired beside it for a session of
// its own where its box cannot ([RunRequest.FreshWiring]). A brief this build
// does not know continues nothing it can tell.
func (k BriefKind) Continues() bool {
	return k == BriefContinued || k == BriefContinuedNewBox || k == BriefContinuedConversation
}

// LaunchDecision is how a launch decided its run takes up a session, told to
// its caller before the run starts ([LaunchRequest.Decided]).
type LaunchDecision struct {
	// Brief is which of the four briefs the launch hands the run.
	Brief BriefKind

	// Kept says the session the run continues is the one the launch was
	// asked to continue in any box ([LaunchRequest.Session]); false where it
	// is the job before's own copy in its box ([LaunchRequest.Continue]), or
	// none.
	Kept bool

	// Base says that session is the one the conversation's last turn left
	// ([SessionContinuation.Base]): always in a box of the launch's own.
	Base bool
}

// continuedLaunch is how one launch continues the turn's session, if it does:
// the brief its run is started with and the one it falls back to, the session
// it takes up and the archive carried into its box, and which brief it is.
type continuedLaunch struct {
	brief, fresh, session string
	carried               *CarriedSession
	kind                  BriefKind
	// told is what brief told of the round the launch carries on
	// ([BriefTold]): the fresh brief's where it continues no session.
	told BriefTold
	// kept says the session is the turn's kept capture rather than the job
	// before's own copy in its box ([LaunchDecision.Kept]).
	kept bool
	// release removes what the launch spooled to carry the session in, once
	// the run has started (or failed to).
	release func()
}

// decision is how the launch decided, for its caller ([LaunchRequest.Decided]).
func (l continuedLaunch) decision() LaunchDecision {
	return LaunchDecision{Brief: l.kind, Kept: l.kept, Base: l.kind == BriefContinuedConversation}
}

// omitted is what the launch req records on the job it opens of the calls of
// the round it carries on that the brief it hands its run left out, and, where
// that brief continues a session, the fresh one its box falls back to
// ([LaunchRecord.BriefOmitted], [LaunchRecord.FreshOmitted]).
//
// NONE FOR A LAUNCH THAT CARRIES NO ROUND ON ([LaunchRequest.Continue] nil): a
// turn's first launch tells no round's calls, and a send-back opens a new
// round, whose log restarts, so a set of the collected round's calls would be
// read against another round's log. A closed round's reads past the read cap
// are counted under the prior-work ledger's rule for them.
func (l continuedLaunch) omitted(req LaunchRequest) (brief, fresh []int) {
	if req.Continue == nil {
		return nil, nil
	}
	if l.kind.Continues() {
		fresh = req.Continue.Fresh.Omitted
	}
	return l.told.Omitted, fresh
}

// freshLaunch is a launch that continues no session: what its fresh brief told
// of the round it carries on where it carries one on, and nothing where it
// does not (a turn's first launch, a send-back), whose request names no
// continuation to read it off.
func freshLaunch(req LaunchRequest) continuedLaunch {
	launch := continuedLaunch{brief: req.Brief, kind: BriefFresh, release: func() {}}
	if req.Continue != nil {
		launch.told = req.Continue.Fresh
	}
	return launch
}

// continueSession is how the launch req continues its turn's session in box —
// see the file comment — given the run's record as the launch opened it,
// whether box is the one the job before used (reused), and whether this
// launch's own begin was read back holding the base its read froze (begun,
// [Coordinator.recordBegin]).
//
// IN THIS ORDER: the session its caller chose ([LaunchRequest.Session]) — the
// turn's kept capture, in any box, or the session its conversation's last turn
// left, in a box of its own ([Coordinator.continueBase]); else, where the box is
// the job before's own, that job's session as the box holds it
// ([Continuation.SessionID]), as a run always could, unless that job ran out of
// context in it; else a session of its own.
func (c *Coordinator) continueSession(ctx context.Context, req LaunchRequest, opened PendingRun,
	box Sandbox, runner Runner, reused, begun bool) continuedLaunch {

	if want := req.Session; want != nil {
		var (
			launch continuedLaunch
			why    string
		)
		if want.Base {
			launch, why = c.continueBase(ctx, req, opened, box, runner, reused, begun, *want)
		} else {
			launch, why = c.continueKept(ctx, req, opened, box, runner, reused, *want)
		}
		if why == "" {
			// WHAT ITS BRIEFS TOLD, what they left out of the round and what
			// they opened with, the same for the brief for either box
			// ([SessionContinuation.Told]).
			launch.told = want.Told
			return launch
		}
		c.notContinued(ctx, req, why)
	}
	if continued := req.Continue; continued != nil && reused && continued.SessionID != "" {
		if !opened.sessionExhausted(continued.SessionID) {
			return continuedLaunch{brief: continued.Brief, fresh: req.Brief, session: continued.SessionID,
				kind: BriefContinued, told: continued.Told, release: func() {}}
		}
		c.notContinued(ctx, req, executorkeep.ExhaustedWhy(continued.SessionID, req.Turn.TurnID))
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
	row, err := c.restore.Session(ctx, req.Turn.AgentHandle, req.Turn.TurnID)
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

// continueBase continues, in box, the session the turn's conversation's last
// turn left, as the turn's first launch froze it ([PendingRun.Base]) — or says
// why it cannot.
//
// IN THIS ORDER, each before anything the next one costs: the company keeps
// sessions; the turn froze a base, has kept no session of its own, and this box
// is a box of its own — a box the turn's earlier job ran in holds that job's
// working tree and session, which are [Coordinator.continueSession]'s to
// continue in place; the turn's own row on the executor log HOLDS the base —
// its begin landed, naming it, and no capture of the turn's has landed since:
// as this launch's own begin was read back where begun says so, which nothing
// can have changed before the run starts, and otherwise as a read of the row
// says — and the base's row still names the object the read froze, a point read
// of that one row, never a read of the conversation's head again, which guards
// against a hold that failed and is the row the fetch is checked against; the
// runner restores it ([BaseContinuityOf]); the box's harness takes it up
// ([SessionRestorer.CheckCarry]); and its object, fetched sealed onto this
// node's disk, opens whole.
//
// THE RESTORE CARRIER, chosen here and in [Coordinator.continueKept] and
// nowhere else: the base turn's own object, read off that turn's own row under
// the launching seat, and opened against that row's associated data — its
// handle, its turn and its conversation's key — so no other seat's or
// conversation's session opens into the run.
func (c *Coordinator) continueBase(ctx context.Context, req LaunchRequest, opened PendingRun,
	box Sandbox, runner Runner, reused, begun bool, want SessionContinuation) (continuedLaunch, string) {

	if why := c.continuesNoKept(); why != "" {
		return continuedLaunch{}, why
	}
	switch {
	case opened.Base == nil || opened.Base.Session == nil:
		return continuedLaunch{}, "the turn froze no session of its conversation to continue"
	case opened.Kept != nil:
		return continuedLaunch{}, "the turn has kept a session of its own, which holds everything the one its " +
			"conversation's last turn left does, so no launch of the turn continues that one any more"
	case reused:
		return continuedLaunch{}, "the launch reuses the box the turn's job before ran in, and the session " +
			"its conversation's last turn left is continued only in a box of its own"
	case want.Replaced == "":
		return continuedLaunch{}, "the launch was given no brief that continues its conversation's session"
	}
	base := *opened.Base.Session
	var own *executorkeep.TurnRow
	if !begun {
		read, err := c.restore.Session(ctx, req.Turn.AgentHandle, req.Turn.TurnID)
		switch {
		case errors.Is(err, ErrTurnRowNotFound):
			return continuedLaunch{}, fmt.Sprintf("the turn's row on the executor log holds nothing: its "+
				"begin never landed, so turn %s's session is held against no release", base.Turn)
		case err != nil:
			return continuedLaunch{}, fmt.Sprintf("the turn's row on the executor log could not be read: %v",
				err)
		}
		own = &read
	}
	row, err := c.restore.Session(ctx, req.Turn.AgentHandle, base.Turn)
	if err != nil {
		return continuedLaunch{}, fmt.Sprintf("turn %s's row on the executor log could not be read: %v",
			base.Turn, err)
	}
	if why := base.namedBy(row); why != "" {
		return continuedLaunch{}, why
	}
	restorer, _ := runner.(SessionRestorer)
	if continuity := BaseContinuityOf(base, own, &opened, restorer); !continuity.Continues {
		return continuedLaunch{}, continuity.Why
	}
	archive := base.archive()
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
	carried := &CarriedSession{Archive: archive, Body: body, Replace: base.Interrupted}
	return continuedLaunch{brief: want.Replaced, fresh: req.Brief, session: base.SessionID,
		kind: BriefContinuedConversation, kept: true, carried: carried,
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
	row  executorkeep.TurnRow
	file string
}

// fetchSession spools row's session sealed and checks it can be read whole,
// before any of it reaches a box ([executorkeep.TurnRowStore.Fetch]) — in the
// coordinator's spool directory ([Coordinator.spoolDirectory]), where its
// sweep looks.
func (c *Coordinator) fetchSession(ctx context.Context, row executorkeep.TurnRow) (*fetchedSession, error) {
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
