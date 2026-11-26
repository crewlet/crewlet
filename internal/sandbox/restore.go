package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
)

// A turn's session is continued here in whichever box its next launch gets,
// and a conversation's by its next turn. The argument is the package doc's,
// from "A turn's session is continued in whichever box its next launch gets"
// to "Never a failed launch".

// SessionRestore is what continuing a turn's kept session needs of the engine:
// the turn's row on the executor log, read through the ESTATE ROUTER'S reads
// (internal/estate's ExecutorReads) at the level a relaunch reads it
// (executordomain.RelaunchRead: a capture another node recorded is read at
// `linearizable`), as the sandbox's view of it ([executorkeep.TurnRow]); and
// the sealed object it names, streamed opened through the domain's session
// store, for a restore or a reading of what the session holds (holdings.go). The row is the leaf's type because the sandbox links no state
// log: the log's own row type carries its positions, and so its graph.
type SessionRestore interface {
	// Session reads one turn's row (executordomain.Reader.Session, made the
	// sandbox's view by executordomain.Session.Kept), wrapping
	// [ErrTurnRowNotFound] where the log holds no row for the turn.
	Session(ctx context.Context, handle, turn string) (executorkeep.TurnRow, error)

	// Open streams the sealed object row names, every chunk authenticated
	// against the row's associated data before a byte is handed out
	// (executordomain.SessionStore.Open). WHAT IT HANDS OUT IS PROVISIONAL
	// UNTIL io.EOF, since a stream cut short is known only at the read past its
	// last whole chunk: its reader decides nothing until the stream has ended
	// ([Coordinator.HoldingsOf]), and a restore reads it whole once before it
	// streams it again into a box, held to that read ([Coordinator.fetchSession]). A row naming no
	// object, an object the store no longer holds and bytes not sealed for the
	// row are unreadable, at the call or from the Read that reaches the fault.
	// The caller closes what it answers.
	Open(ctx context.Context, row executorkeep.TurnRow) (io.ReadCloser, error)
}

// ErrTurnRowNotFound is [SessionRestore.Session]'s answer for a turn the
// executor log holds no row for: a turn whose begin never landed and that
// captured nothing. The engine wraps the log's own refusal
// (executordomain.ErrNotFound) in it, since the sandbox links no state log.
var ErrTurnRowNotFound = errors.New("sandbox: the executor log holds no row for the turn")

// SessionContinuity is whether a relaunch may continue a turn's kept session,
// and where it may not, why — [ContinuityOf]'s answer — with Loss, that why in
// the one vocabulary ([executorkeep.Loss]): empty where nothing was lost (a
// turn that ended, a turn's own newer session), which no launch says.
type SessionContinuity struct {
	Continues bool
	Loss      executorkeep.Loss
	Why       string
}

// continues is the continuity of a session that is continued.
var continues = SessionContinuity{Continues: true}

// ArchiveRestorer is the half of a [SessionRestorer] the decision whether a
// session can be continued asks ([ContinuityOf]): whether a runner restores an
// archive at all (its harness, format, a release another box continues) before
// any box is asked, and where it does not, why and the loss it is.
type ArchiveRestorer interface {
	Restores(archive SessionArchive) SessionContinuity
}

// ContinuityOf is THE decision whether a relaunch that follows the job at
// launch follows can continue the turn's kept session: the turn live and not
// ending ([PendingRun.endDecided], since every launch refuses a decided
// ending), the newest capture that job kept stating its save point
// ([followedCapture]), the turn's row on the executor log naming it, no job
// having run out of context in it, and restorer restoring it
// ([ArchiveRestorer]). run is nil where the turn's run is over. A capture the
// log keeps where the record stamps it refused is not continued, and the reason
// says the log keeps it ([namedThoughRefused]); nor is an older one kept beside
// it, since a relaunch restores only the object the row names. The box's
// release and the seat's budget are the launch's to ask. The package doc's "A
// turn's session is continued" has the argument.
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
	kept, loss, why := followedCapture(*run, follows)
	if why != "" {
		if namedThoughRefused(*run, row, follows) {
			newest := *run.Capture
			why = fmt.Sprintf("the run's record stamps the newest capture of the job the relaunch follows "+
				"(launch %d, capture %d) %s; %s", newest.LaunchSeq, newest.CaptureSeq, unkeptWhy(newest),
				keptThoughRefused)
		}
		return refusedFor(loss, why)
	}
	switch {
	case !row.HasObject():
		return refusedFor(executorkeep.LossLogRefused, "the turn's row on the executor log names no kept session")
	case row.LaunchSeq != kept.LaunchSeq || row.CaptureSeq != kept.CaptureSeq:
		if namedThoughRefused(*run, row, follows) {
			// AN OLDER KEPT CAPTURE BESIDE A NEWER ONE THE LOG KEEPS: the
			// record keeps the older, the row names the newer, neither is
			// continued; "the log names another" would leave a reader guessing.
			newest := *run.Capture
			return refusedFor(unkeptLoss(newest), fmt.Sprintf("the run's record stamps the newest capture of "+
				"the job the relaunch follows (launch %d, capture %d) %s; %s, nor the older capture of it the "+
				"record keeps (launch %d, capture %d), which the turn's row does not name", newest.LaunchSeq,
				newest.CaptureSeq, unkeptWhy(newest), keptThoughRefused, kept.LaunchSeq, kept.CaptureSeq))
		}
		return refusedFor(executorkeep.LossLogRefused, fmt.Sprintf("the executor log names the turn's capture "+
			"(launch %d, capture %d), and its run's record keeps (launch %d, capture %d)", row.LaunchSeq,
			row.CaptureSeq, kept.LaunchSeq, kept.CaptureSeq))
	case run.spentBy(row.SessionID) != "":
		return refusedFor(run.spentBy(row.SessionID), executorkeep.SpentWhy(row.SessionID, run.TurnID,
			run.spentBy(row.SessionID)))
	case restorer == nil:
		return refusedFor(executorkeep.LossHarnessChanged, fmt.Sprintf("no runner of this build restores a "+
			"session %s wrote", row.Harness))
	}
	return restorer.Restores(ArchiveOf(row))
}

// followedCapture is the capture a relaunch that follows the job at launch
// continues, or why there is none, in words for the log: the half of
// [ContinuityOf] the run's record answers alone. THE NEWEST KEPT, whatever a
// newer one of that job is (refused, unfinished, unanswered or still being
// kept): the continuation is told every call after its save point, so it counts
// none of the newer one's calls held. One that may still land is the
// relaunch's to wait for ([Coordinator.ContinuedCapture]) and the launch's own
// check refuses a choice it moves ([Coordinator.continueKept]).
func followedCapture(run PendingRun, follows int64) (SessionCapture, executorkeep.Loss, string) {
	kept, newest := run.Kept, run.Capture
	switch {
	case newest == nil:
		return SessionCapture{}, executorkeep.LossBoxLost, "the turn has kept no capture of its session"
	case newest.LaunchSeq != follows:
		// "HAS LEFT" is true of a job still working as of one that ended: a
		// reader asks this of a live turn whose job runs.
		return SessionCapture{}, executorkeep.LossBoxLost, fmt.Sprintf("the job the relaunch follows (launch "+
			"%d) has left no capture of its session, and an earlier job's (launch %d) holds none of what the "+
			"jobs after it were told", follows, newest.LaunchSeq)
	case kept == nil || kept.LaunchSeq != follows:
		return SessionCapture{}, unkeptLoss(*newest), fmt.Sprintf("the newest capture of the job the relaunch "+
			"follows (launch %d, capture %d) was not kept: %s, and the job kept none before it",
			newest.LaunchSeq, newest.CaptureSeq, unkeptWhy(*newest))
	}
	return *kept, "", ""
}

// namedThoughRefused reports whether the turn's row on the executor log names
// the newest capture of the job at follows where run's record stamps it
// REFUSED: a record its taker handed on before its lease's window closed, which
// the log took only after another node's settle read the row (the package doc,
// "WHAT THE FENCE CANNOT REACH"). An unanswered one is no such stamp. The log
// keeps it, and no relaunch of that job continues it or an older capture
// beside it: the record keeps no place it holds refused ([stampOnto]), and
// ADR-0073 records no refusal on the log. A decision told with the row in hand
// says the log keeps it, never that it was not kept.
func namedThoughRefused(run PendingRun, row executorkeep.TurnRow, follows int64) bool {
	newest := run.Capture
	return stampedRefused(run, follows) &&
		row.HasObject() && row.LaunchSeq == newest.LaunchSeq && row.CaptureSeq == newest.CaptureSeq
}

// stampedRefused is [namedThoughRefused] less the turn's row, which a relaunch
// reads only where this holds.
func stampedRefused(run PendingRun, follows int64) bool {
	newest := run.Capture
	return newest != nil && newest.State == CaptureRefused && newest.LaunchSeq == follows
}

// keptThoughRefused is what a relaunch's decision says of a capture
// [namedThoughRefused] reports.
const keptThoughRefused = "the executor log keeps it all the same, the turn's row naming it — its record " +
	"landed unseen by that stamp, which is never taken back — and a relaunch continues only a capture its " +
	"run's record keeps, so no relaunch of that job continues it"

// ArchiveOf is the archive a turn's row names, as a restorer and a run take it.
func ArchiveOf(row executorkeep.TurnRow) SessionArchive {
	return SessionArchive{Harness: row.Harness, SessionID: row.SessionID, Entry: row.Entry,
		Format: row.ArchiveFormat, Version: row.HarnessVersion}
}

// ContinuesIn is where a launch would continue a turn's session, as a reader of
// the session is told it ([ReadContinuityOf]).
type ContinuesIn string

const (
	// ContinuesInAnyBox is a session continued in whichever box the launch
	// gets: a live turn's kept capture, by a relaunch, restored into another
	// box or as its own copy in the box it was last worked in
	// ([ContinuityOf] says it continues), though in the box its job parked in,
	// where that capture was taken while the job still worked, as the copy that
	// box holds, which went on past it ([copyWentPast]); or an ended turn's
	// that is its conversation's head, by the conversation's next turn,
	// restored into a box of that turn's own ([BaseContinuityOf]).
	ContinuesInAnyBox ContinuesIn = "any_box"

	// ContinuesInParkedBox is the session the box the turn's job parked in
	// holds, continued IN THAT BOX AND NOWHERE ELSE: what a person's answer
	// carries a parked run on in where the turn's kept capture is not continued
	// (none kept, or one of a release no other box continues), while the run's
	// box is held. A pause past its time-to-live reclaims the box, and the
	// answer's relaunch then starts a session of its own.
	ContinuesInParkedBox ContinuesIn = "parked_box"
)

// ContinuesIns are every place a reader is told a launch continues a session.
var ContinuesIns = []ContinuesIn{ContinuesInAnyBox, ContinuesInParkedBox}

// Valid reports whether w is one of [ContinuesIns]. An answer a newer build
// sent may name another, which is read as the value it is.
func (w ContinuesIn) Valid() bool { return slices.Contains(ContinuesIns, w) }

// ContinuedBy is which launch would continue a turn's session, as a reader of
// the session is told it ([ReadContinuityOf]).
type ContinuedBy string

const (
	// ContinuedByRelaunch is a relaunch of the session's own turn, while that
	// turn is live and its run is neither over nor ending ([ContinuityOf]).
	ContinuedByRelaunch ContinuedBy = "relaunch"

	// ContinuedByNextTurn is the next turn of the session's conversation, once
	// the session's own turn has ended (or is ending, its end not yet on the
	// log as the reader read it), which continues the session only where it is
	// the conversation's head ([BaseContinuityOf]). A turn that served no
	// conversation has no next turn.
	ContinuedByNextTurn ContinuedBy = "next_turn"

	// ContinuedByNextFire is [ContinuedByNextTurn] of a schedule's
	// conversation (internal/notify's ScheduleConversation): the schedule's
	// next fire, which continues the session only where it is the schedule's
	// head on the seat, the ended fire that read the conversation last. The
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
// it (none for a conversation's next turn, which is launched under the
// revision the holder runs then) and the archive a launch would take up.
type RelaunchAsk struct {
	Turn    string
	Archive SessionArchive
}

// RelaunchTarget is what a launch of a seat's turn would be launched on now, as
// the node holding the seat picks it (internal/engine): the harness of the
// first member of the seat's executor chain not sitting out its plan, its cell,
// that node, and whether its runner restores a session ([ArchiveRestorer]).
// THE HOLDER'S PICK, NEVER THE READER'S: which members sit out their plans is
// what one node's own calls have met (internal/providers/llm/harness's bench),
// and which company revision says the switch is a node's too. THE TURN'S,
// PICKED FROM THE CHAIN ITS RELAUNCH RUNS ON: a resumed segment relaunches on
// the revision it was admitted under, any other next segment and a
// conversation's next turn under the one the holder runs then; so a target is
// asked per turn, and for that next turn by none ([RelaunchAsk]).
type RelaunchTarget struct {
	// Harness and Placement are the member's harness and the cell its run
	// lands in, which decide whether a relaunch continues in the box the run
	// parked in ([PriorBox.ContinuesIn]); empty where there is no member.
	Harness   string
	Placement Placement

	// Node is the node holding the seat: a box is continued in only by the node
	// it was launched on ([PriorBox.ReachableFrom]).
	Node string

	// Entry is the member's providers.llm entry ([LaunchRequest.Entry]), and
	// LaunchRouted whether its harness resumes on its launch's route: what the
	// copy a box holds is held to ([EntryContinues]), as the launch holds it.
	Entry        string
	LaunchRouted bool

	// Restorer is the member's runner as it takes up a session carried between
	// boxes (or the holder's answer of it, for a reader on another node); nil
	// where it takes none up or there is no member, which Why then says.
	Restorer ArchiveRestorer
	Why      string

	// Undecided says what a launch would be launched on cannot be said now
	// (Why says): every member sitting out its plan, no node holding the seat,
	// or that node not answering. A launch waits and asks again.
	Undecided bool

	// NoKept is why a launch of the seat's turn continues no kept session
	// whatever the turn kept, "" where it may: the company keeps none
	// ([NoKeptSession]), as the holder's revision says and its coordinator's own
	// check at the launch ([Coordinator.continuesNoKept]). A relaunch then
	// continues at most the session the box its run parked in holds, a
	// conversation's next turn none.
	NoKept string
}

// restorer is the target's runner as [ContinuityOf] and [BaseContinuityOf] ask
// it: its own, or one that restores nothing and says why.
func (t RelaunchTarget) restorer() ArchiveRestorer {
	if t.Restorer != nil || t.Why == "" {
		return t.Restorer
	}
	return refusedRestore{why: t.Why}
}

// Restores is whether the target's runner restores archive, and where not, why
// ([ArchiveRestorer]): what the holder answers a reader on another node.
func (t RelaunchTarget) Restores(archive SessionArchive) SessionContinuity {
	restorer := t.restorer()
	if restorer == nil {
		return refusedFor(executorkeep.LossHarnessChanged,
			"no runner of the member a launch is launched on restores a session")
	}
	return restorer.Restores(archive)
}

// refusedRestore is a restorer that takes no session up, for the reason it is:
// the target names no runner that restores one, or cannot be said now.
type refusedRestore struct{ why string }

func (r refusedRestore) Restores(SessionArchive) SessionContinuity {
	return refusedFor(executorkeep.LossBoxRefused, r.why)
}

// ReadContinuity is whether a launch would continue a turn's session, as a
// reader of the session is told it ([ReadContinuityOf]): a relaunch of the
// turn, or the next turn of its conversation, which By says.
type ReadContinuity struct {
	// By is the launch the answer is about ([ContinuedBys]); empty where no
	// launch would ever continue the session (a turn that served no
	// conversation, once no relaunch of it follows).
	By ContinuedBy

	// Decided is false where it cannot be said now, which Why then says.
	Decided bool

	// Continues says the launch would continue the session, and In where.
	Continues bool
	In        ContinuesIn

	// Why is why the launch would not continue the session, why that cannot be
	// said now, or, where a relaunch continues it only in the box the run
	// parked in, why the kept capture is continued in no other.
	Why string
}

// ReadContinuityOf is whether a launch would continue the session of the turn
// whose executor-log row is row, as a reader is told it (THE DECISION THAT
// LAUNCH TAKES), and which launch ([ReadContinuity.By]). next is
// [ContinuedByNextFire] for a schedule's conversation, else
// [ContinuedByNextTurn]; THE CALLER SAYS WHICH (internal/notify's Scheduled),
// which notify's import path keeps from here. row is read at
// executordomain.RelaunchRead wherever the decision reaches it ([RereadsRow]).
// A LIVE TURN is [ContinuityOf], [inParkedBox], then [relaunchedBase]; an ENDED
// TURN is [BaseContinuityOf] of its conversation's head, asked by no turn
// ([TargetAsk]). UNDECIDED WHERE THE LAUNCH WOULD NOT DECIDE NOW EITHER, never
// a no ([relaunchContinuity]). now is the reader's clock for in-flight captures
// and the window a record may name an object in ([pastRecordWithin]).
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
// conversation left, as a reader is told it.
type laterLaunch struct {
	by ContinuedBy

	// whole is what the conversation is called, turn what each of its turns
	// is: a conversation's turns, or a schedule's fires.
	whole, turn string
}

// laterLaunchOf is next as a reader is told it ([ReadContinuityOf]): every
// value but [ContinuedByNextFire] is read as a next turn.
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
// served no conversation once no relaunch follows.
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
		// UNKNOWN, NEVER A NO: a read of an ended turn's row names a head for
		// its conversation (that row, if nothing newer), so a read naming none
		// did not say which session the next turn is handed.
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
// (run nil) or ending: decided only for a turn that served no conversation;
// for any other, whether its next turn (later) continues the session is
// decided once the turn's end is on the log.
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
// neither over nor ending: whether a relaunch would continue its session,
// naming the turn's base where it would continue that instead. UNDECIDED while
// a capture or save the relaunch waits for is still being kept
// ([relaunchWaitsOn]) or its record awaits its answer ([captureAwaitsAnswer]),
// beside an older kept capture as beside none: the relaunch waits for the one,
// asks again for the other, and continues whichever lands (not where the
// company keeps no session, [RelaunchTarget.NoKept]). A newer one that never
// will land leaves the decision to [ContinuityOf], which continues the older.
func relaunchContinuity(row executorkeep.TurnRow, run PendingRun, target RelaunchTarget,
	later laterLaunch, now time.Time) ReadContinuity {

	if jobWorking(run) {
		return ReadContinuity{Why: fmt.Sprintf("the job a relaunch would follow (launch %d) is still "+
			"working or being collected, and its session is taken and kept by the read that collects it",
			run.LaunchSeq)}
	}
	if waits := relaunchWaitsOn(run, now); waits != nil {
		if waits.LaunchSeq != run.LaunchSeq {
			return ReadContinuity{Why: fmt.Sprintf("a capture of an earlier job of the turn (launch %d, "+
				"capture %d) is still being kept, and the turn has kept none: once one lands, a relaunch "+
				"continues no session the turn's first launch was handed", waits.LaunchSeq, waits.CaptureSeq)}
		}
		return ReadContinuity{Why: fmt.Sprintf("the capture of the job a relaunch would follow (launch %d, "+
			"capture %d) is still being kept", waits.LaunchSeq, waits.CaptureSeq)}
	}
	if target.Undecided {
		return ReadContinuity{Why: target.Why}
	}
	if target.NoKept == "" {
		if asks := captureAwaitsAnswer(run, now); asks != nil {
			return ReadContinuity{Why: fmt.Sprintf("the record of the capture of the job a relaunch would "+
				"follow (launch %d, capture %d) was sent and never answered, and a relaunch asks the executor "+
				"log for it again before it decides, continuing that capture where it landed",
				asks.LaunchSeq, asks.CaptureSeq)}
		}
	}
	kept := ContinuityOf(row, &run, run.LaunchSeq, target.restorer())
	if target.NoKept != "" {
		kept = SessionContinuity{Why: target.NoKept}
	}
	if kept.Continues && run.Lost != nil && !KeptAfterLoss(run.RoundLosses+1) {
		// A ROUND LOST AGAIN is carried on in a session of its own, never from
		// the state it was lost in, unless its job had submitted its work, when
		// only a review's send-back would continue it: the job's resume decides
		// and says as its turn begins.
		switch run.Launch.ResumeCarry {
		case ResumeCarriesOn:
			return ReadContinuity{Decided: true, Why: fmt.Sprintf("the job a relaunch would follow "+
				"(launch %d) was lost a second time in its round, and its resumed turn carries the round on "+
				"in a session of its own", run.LaunchSeq)}
		case ResumeCollects:
			// COLLECTED from the submitted work; a send-back continues the kept
			// capture: decided below.
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
// on the log still holds it ([BaseContinuityOf] of row, read as a relaunch
// reads it: [Coordinator.ContinuedCapture]) for target's runner, and where the
// round may be continued from a session at all: a round lost a second time
// starts one of its own, unless the resumed turn collects what the job
// submitted, whose review's send-back continues the base; until that resume
// says which, nothing is claimed. IN A BOX OF ITS OWN, which is the launch's to
// get: a relaunch into the box the turn's job ran in continues the session that
// box holds instead ([Coordinator.continueBase]). The base is named as later
// names the turns of row's conversation.
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
// whose relaunch is launched on the revision a segment running on the holder
// pins; and of the archive a relaunch would take up: the row's, or, where the
// row names no session, the one the turn's first launch was handed
// ([relaunchedBase]). AN ENDED ROW THAT IS ITS CONVERSATION'S HEAD (head being
// the turn the read that answered row named) is asked by NO turn: the next turn
// is admitted under the revision the holder runs when it comes, never one a
// segment of the ended turn still pins just before it returns.
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
// reads it (executordomain.RelaunchRead) must read it again there before
// deciding: only where the decision reaches the row (the turn live, not ending,
// nothing it waits on in flight or awaiting an answer) and the row names
// another capture than run's record keeps, or the record keeps none, or holds a
// newer capture of the followed job it does not keep ([unkeptNewer]), which the
// log may have taken unseen by its stamp. A row naming the kept capture beside
// nothing newer decides as a read at that level would, since a newer capture is
// stamped before a byte of it moves; reading again there would append a
// barrier to the log for each live row on every look at a seat's sessions.
func RereadsRow(row executorkeep.TurnRow, run *PendingRun, now time.Time) bool {
	if run == nil || row.Ended || run.endDecided() || jobWorking(*run) || relaunchWaitsOn(*run, now) != nil ||
		captureAwaitsAnswer(*run, now) != nil {
		return false
	}
	kept := run.Kept
	return kept == nil || row.LaunchSeq != kept.LaunchSeq || row.CaptureSeq != kept.CaptureSeq ||
		unkeptNewer(*run)
}

// unkeptNewer reports whether run's record holds a capture of the followed job
// newer than the one it keeps, in any state: its newest capture, else its save
// under way ([PendingRun.savingTake]) ([RereadsRow]).
func unkeptNewer(run PendingRun) bool {
	for _, stamp := range []*SessionCapture{run.Capture, run.savingTake()} {
		if stamp != nil && stamp.LaunchSeq == run.LaunchSeq && (run.Kept == nil || stamp.newerThan(*run.Kept)) {
			return true
		}
	}
	return false
}

// jobWorking reports whether run's job is still working or being collected, so
// no reader can say yet what a relaunch following it continues: a launch not
// yet suspended, a job running whose box no collection has read back
// ([LaunchRecord.Collected]), or a claim of a job whose read is still under
// way. The read that collects a job takes its session first, and the claim's
// turn begins once it has, saying what it does with the round
// ([LaunchRecord.ResumeCarry]); the claim's row then stays resumed for the
// whole resumed turn, its review included, and is decided as any other from
// the first of two on: the job's final capture taken and settled
// ([captureInFlight]), or the turn begun, whether or not its read took any
// capture. A claim of a person's answer reads no box at all: its job was
// collected when it parked on its question.
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

// captureAwaitsAnswer is the stamp of the followed job whose record was sent
// and never answered and may still land ([SessionCapture.unresolvedWithin]):
// run's newest capture, else its save under way ([PendingRun.saving]), which
// beside a refused final take is the job's newest session once its ask lands
// it; nil where neither is. A relaunch asks it again before it decides
// ([Coordinator.settleUnresolved]), so until then no reader can say what it
// continues. An earlier job's stamp is no such question.
func captureAwaitsAnswer(run PendingRun, now time.Time) *SessionCapture {
	for _, stamp := range []*SessionCapture{run.Capture, run.saving(now)} {
		if stamp != nil && stamp.LaunchSeq == run.LaunchSeq && stamp.unresolvedWithin(now) {
			return stamp
		}
	}
	return nil
}

// relaunchWaitsOn is the stamp a relaunch of run's turn waits for before it
// decides ([Coordinator.ContinuedCapture]), and a reader waits on rather than
// decide ([relaunchContinuity], [RereadsRow]); nil for none: a stamp the end
// waits for ([stampInFlight]) that the relaunch's decision depends on
// ([relaunchDependsOn]). Only those, since the waiting frame holds the seat and
// an admission slot.
func relaunchWaitsOn(run PendingRun, now time.Time) *SessionCapture {
	return stampInFlight(run, now, relaunchAdmits(run))
}

// relaunchAdmits is [relaunchDependsOn] for run, as a wait's rule reads it.
func relaunchAdmits(run PendingRun) func(SessionCapture) bool {
	return func(stamp SessionCapture) bool { return relaunchDependsOn(run, stamp) }
}

// relaunchDependsOn reports whether what a relaunch of run's turn continues
// can turn on whether stamp lands: a stamp of the followed job (run.LaunchSeq),
// or of any job while the turn has kept nothing, since once a capture of the
// turn is kept a relaunch no longer continues the session the turn's first
// launch was handed ([frozenBase]). An earlier job's stamp beside a kept
// capture is neither ([followedCapture]).
func relaunchDependsOn(run PendingRun, stamp SessionCapture) bool {
	return stamp.LaunchSeq == run.LaunchSeq || run.Kept == nil
}

// relaunchWaits is [relaunchWaitsOn] as the rule a relaunch's wait reads
// ([Coordinator.awaitCapture]).
func relaunchWaits(run PendingRun, now time.Time) bool { return relaunchWaitsOn(run, now) != nil }

// followedBegunUnderTaker reports whether run has a stamp of the followed job
// still begun under a taker it names: its newest capture, or its save under way
// ([PendingRun.savingTake]), being kept or not. A relaunch reads the turn's row
// once past its wait for such a stamp ([Coordinator.ContinuedCapture]),
// WHATEVER ITS AGE: a host whose coordination store answers its leases and not
// its runs holds its taker lease as long as that silence lasts, and the job
// lost with it is found only once the silence has lasted the host grace, so
// a keep under way when it began can be past a capture's budget by then, and
// the log's all the same. A stamp naming no taker is read by its age alone, as
// [beganUnderTaker] has a wait read it.
func followedBegunUnderTaker(run PendingRun) bool {
	for _, stamp := range []*SessionCapture{run.Capture, run.savingTake()} {
		if stamp != nil && stamp.State == CaptureBegun && stamp.Taker != "" && stamp.LaunchSeq == run.LaunchSeq {
			return true
		}
	}
	return false
}

// boxCarry is whether a relaunch carrying a run's round on in the box its job
// ran in would continue the session that box holds ([inParkedBox]).
type boxCarry int

const (
	boxNot boxCarry = iota // no relaunch continues it there
	// boxContinues: a person's answer's relaunch would, or the relaunch a
	// resumed turn carries its round on with at once.
	boxContinues
	// boxUndecided: one would where the resume of the job's collection carries
	// its round on in its box, which only that resume decides, not said yet.
	boxUndecided
)

// inParkedBox is whether a relaunch carrying run's round on in the box its job
// ran in would continue the session row names, there, and where it would not or
// cannot be said, why; "" where no relaunch carries the round on in a box.
// WHICH RELAUNCHES DO: a person's answer to the run's question
// ([PendingRun.Answer]); and the resume of a collected job whose sign-in was
// replaced, which only that resume decides, as its turn begins
// ([LaunchRecord.ResumeCarry]). Never a lost job's, whose box holds what its
// harness was cut off in. BY THE LAUNCH'S OWN TESTS of a box it continues in
// ([PriorBox.ContinuesIn], [PriorBox.ReachableFrom]): a seat that moved leaves
// its box on the host it was launched on, which the next holder cannot reach.
// AND OF THE SESSION THE ROW NAMES ([PendingRun.SessionRan]).
func inParkedBox(run PendingRun, row executorkeep.TurnRow, target RelaunchTarget, now time.Time) (boxCarry, string) {
	carried, undecided := boxContinues, ""
	switch {
	case run.Status == StatusAwaiting || run.Status == StatusReseed || run.Status == StatusAnswered ||
		(run.Status == StatusResumed && run.Answer != nil):
	case run.Lost != nil:
		return boxNot, ""
	case run.Status == StatusResumed && run.Launch.ResumeCarry == ResumeCarriesOn:
		// CARRIES THE ROUND ON at once in the job's box (its sign-in was replaced).
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
	case run.spentBy(ran) != "":
		return boxNot, executorkeep.SpentWhy(ran, run.TurnID, run.spentBy(ran))
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
	// THE BOX'S AGE FIRST, with its own reason: the launch's test refuses a box
	// past [KeptBoxMaxAge], or never dated, whatever its cell and harness
	// ([PriorBox.ContinuesIn]).
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
	case !prior.ReachableFrom(target.Node):
		return boxNot, fmt.Sprintf("the box its run ran in is on the host of %s, and %s, which holds "+
			"the seat, cannot continue in it", run.LaunchNode, target.Node)
	}
	// AND UNDER THE ENTRY ITS JOB RAN UNDER, as the launch's in-place branch
	// holds it ([Coordinator.continueSession]).
	if entry := EntryContinues(run.Launch.Entry, target.Entry, target.Harness, target.LaunchRouted); !entry.Continues {
		return boxNot, entry.Why
	}
	return carried, undecided
}

// ContinuedSession is the session a relaunch of a turn continues: the kept
// capture of the job it follows (its stamp, what it accounts for, where it was
// worked, what its last request carried, and the turn's row naming its object)
// or, where the turn has kept none, the session its conversation's last turn
// left, as its first launch froze it (Base).
type ContinuedSession struct {
	// Capture is the kept capture continued, as the run's record stamps it: its
	// own ContextTokens is what a launch that restores it sends first, never
	// [ContinuedSession.CopyContext].
	Capture SessionCapture
	Row     executorkeep.TurnRow

	// Base is the session the conversation's last turn left, continued in place
	// of a kept capture of the turn's own; nil where it continues none, or a
	// kept capture.
	Base *BaseSession

	// NotContinued is the relaunch's verdict on the newest session of the
	// turn it does not continue: what was lost, and whether it continues an
	// older one (an older capture of the job, or the base) or a fresh one.
	// Nil where nothing was lost. The launch decides the final one
	// ([LaunchRequest.NotContinued]).
	NotContinued *executorkeep.NotContinued

	// CopyContext is what the last request of the followed job's session
	// carried, as that job's newest capture measured it, KEPT OR NOT: what a
	// launch that continues that job's own copy in its box sends again before
	// its brief ([LaunchRequest.Continue]), the copy having gone that far. Zero
	// where no capture measured any. A kept capture continued is held to its own
	// measure (Capture.ContextTokens).
	CopyContext int64

	// CopyNextRequest is the harness's own estimate of that session's next
	// request, as the same newest capture's take read it ([SessionCapture.NextRequest]):
	// what that launch is held to with its brief where it holds a figure,
	// CopyContext where none. A kept capture continued is held to its own
	// (Capture.NextRequest), never a passed-over newer capture's.
	CopyNextRequest NextRequest

	// KeptTurn is the turn whose kept session the relaunch's read found,
	// continued or not: the turn's own where its run's record says a capture of
	// it landed once what was unsettled is settled (a live turn's row is
	// released by no budget, absorption or retention) unless the row read says
	// it names none; or where the turn's row names a capture the record does
	// not keep: one it stamps refused ([namedThoughRefused]), or whose keep
	// reached the log ahead of its stamp ([Coordinator.continuedWithout]); else
	// the base its first launch froze, where the turn's own row was read holding
	// it against every release but a retirement ([baseStanding]); empty where
	// neither was read so: a run is never told a session is there to read that
	// the read did not find held. What a run that starts a session of its own
	// is told it may read (internal/agent/runner's SessionBasis.KeptTurn).
	KeptTurn string

	// Summary is the newest summary the turn's record names
	// ([PendingRun.Summary]), as the turn's end would name it: what a run that
	// does not continue the turn's own newest session is handed, at its place
	// among the turn's captures. Nil where the record names none, or one no end
	// could carry.
	Summary *executorkeep.Summary
}

// relaunchAskWait bounds a relaunch's asks, on the frame that relaunches, where
// no capture it depends on is still being kept: of the followed job's records
// sent and never answered ([Coordinator.askFollowed]), and its one read of the
// log past its wait ([Coordinator.ContinuedCapture]). [beginRecordBudget]'s
// fifteen seconds, for its reason: the frame holds the seat and a slot of the
// node's admission, and a record the log takes answers in milliseconds. Each
// settle has its own room past it ([Coordinator.settleContext]).
const relaunchAskWait = beginRecordBudget

// relaunchWaitOf is how long a relaunch of run's turn waits, from now, for what
// its decision depends on: its asks ([relaunchAskWait]), and each capture or
// save it waits for ([relaunchWaitsOn]) as long as a capture of its archive's
// size, cutting its media objects apart, may still take ([captureRestFor]), so
// it passes over none its keep may still land. The longest of them.
func (c *Coordinator) relaunchWaitOf(run PendingRun, now time.Time) time.Duration {
	wait := c.relaunchWait
	for _, stamp := range stampsInFlight(run, now, relaunchAdmits(run)) {
		wait = max(wait, c.captureRest(stamp.ArchiveSize, stamp.MediaCount))
	}
	return wait
}

// ContinuedCapture is the session a relaunch of turnID continues: the newest
// capture the followed job kept, as [ContinuityOf] decides for restorer, once
// a newer one is no longer being kept or its wait ran out
// ([Coordinator.relaunchWaitOf]; passed over, said: [Coordinator.passedOver]);
// else, where the turn has kept none, its frozen base
// ([Coordinator.continuedWithout]); or why none, and what that job's copy last
// carried and is estimated to send next. Within the wait, a gone taker's
// capture is settled from the log, an unanswered one asked again, a save read
// as the newest; past it, one read settles landed a held taker's capture the
// log names. Settles are this node's own writes in their own room. Asked before
// the run's surface and briefs are built; never an error, never a read of the
// conversation's head. The package doc's "The job before's capture".
func (c *Coordinator) ContinuedCapture(ctx context.Context, turnID string,
	restorer ArchiveRestorer) (ContinuedSession, string) {

	if why := c.continuesNoKept(); why != "" {
		return ContinuedSession{}, why
	}
	run, found, err := c.pending.Get(ctx, turnID)
	switch {
	case err != nil:
		return lostWith(ContinuedSession{}, executorkeep.LossRestoreFailed,
			fmt.Sprintf("the run's record could not be read: %v", err))
	case !found:
		return ContinuedSession{}, "the turn has no run record"
	}
	// ONE WAIT FOR ALL OF IT, on the frame that relaunches: the asks of an
	// unanswered record and the wait for a keep under way share it, as long as
	// the longest needs. The settles are this node's own writes, each in its
	// own room past it.
	wait := c.relaunchWaitOf(run, c.now())
	asked := time.Now()
	settleBy := asked.Add(wait)
	// THE FOLLOWED JOB'S UNANSWERED RECORDS ASKED FIRST, whatever else is in
	// flight: past the wait the relaunch continues the newest capture the job
	// kept beside a newer one still being kept, so an ask that lands a record
	// (a save's, behind a final take) gives it the newer to continue; asked
	// only once the wait ran out it would have no time left.
	run = c.askFollowed(ctx, settleBy, run)
	run, found, err = c.awaitCapture(ctx, run, time.Until(settleBy), relaunchWaits)
	// A COUNT STAMPED SINCE THE WAIT WAS PRICED lengthens it, from the same
	// start: a capture's media count is stamped as its keep begins
	// ([Coordinator.stampMediaCount]), often after the relaunch read it.
	for found && err == nil && relaunchWaits(run, c.now()) {
		longer := c.relaunchWaitOf(run, c.now())
		if longer <= wait {
			break
		}
		wait, settleBy = longer, asked.Add(longer)
		run, found, err = c.awaitCapture(ctx, run, time.Until(settleBy), relaunchWaits)
	}
	if found && err == nil && followedBegunUnderTaker(run) {
		// THE ONE READ PAST THE WAIT, for a capture still begun under a taker
		// whose lease holds or cannot be read: the log may hold it although no
		// stamp reached the row, as where the taker's host answers its leases
		// and not its runs. Settled landed where the turn's row names its
		// place, it is continued. Held to an ask's wait ([relaunchAskWait]).
		held, cancel := context.WithTimeout(ctx, c.relaunchWait)
		run, found, _ = c.settleGone(held, run, askEvery, true)
		cancel()
	}
	if !found {
		return lostWith(ContinuedSession{}, executorkeep.LossRestoreFailed, "the turn's run record was gone "+
			"before the capture of the job the relaunch follows settled")
	}
	answer := ContinuedSession{CopyContext: copyContext(run), CopyNextRequest: copyNextRequest(run),
		Summary: run.namedSummary()}
	if err != nil {
		// NO ANSWER IS NO KEPT CAPTURE: a relaunch never continues one it has
		// not seen kept.
		if run.Kept != nil {
			answer.KeptTurn = turnID
		}
		return lostWith(answer, executorkeep.LossRestoreFailed, fmt.Sprintf("what came of the capture of the "+
			"job the relaunch follows could not be read: %v", err))
	}
	if time.Now().Before(settleBy) {
		// AGAIN WHAT THE WAIT LEFT UNANSWERED, where it ended early.
		run = c.askFollowed(ctx, settleBy, run)
	}
	var (
		why  string
		loss executorkeep.Loss
	)
	waits := relaunchWaitsOn(run, c.now())
	switch keptOfJob := run.Kept != nil && run.Kept.LaunchSeq == run.LaunchSeq; {
	case waits != nil && waits.LaunchSeq != run.LaunchSeq:
		// AN EARLIER JOB'S, waited on only while the turn has kept nothing;
		// what the relaunch continues instead turns on its landing.
		_, _, why = followedCapture(run, run.LaunchSeq)
		loss = executorkeep.LossCaptureLate
		why += fmt.Sprintf("; a capture of an earlier job of the turn (launch %d, capture %d) was still "+
			"being kept %s after the relaunch asked for it, past what a capture of its size takes",
			waits.LaunchSeq, waits.CaptureSeq, wait)
	case waits != nil && !keptOfJob:
		// THE FOLLOWED JOB'S, beside nothing it kept: a keep under way is no kept
		// capture.
		loss, why = executorkeep.LossCaptureLate, fmt.Sprintf("the capture of the job the relaunch follows "+
			"(launch %d, capture %d) was still being kept %s after the relaunch asked for it, past what a "+
			"capture of its size takes", waits.LaunchSeq, waits.CaptureSeq, wait)
	default:
		// THE NEWEST CAPTURE THAT JOB KEPT, past a newer one that will not be
		// kept or that the wait ran out on ([followedCapture]).
		_, loss, why = followedCapture(run, run.LaunchSeq)
	}
	// THE TURN'S OWN WHERE ITS RECORD KEEPS A CAPTURE, read after the settle: an
	// ask that lands one makes it the session a fresh run may read.
	if run.Kept != nil {
		answer.KeptTurn = turnID
	}
	if why != "" {
		return c.continuedWithout(ctx, run, restorer, answer, loss, why)
	}
	row, err := c.restore.Session(ctx, run.AgentHandle, turnID)
	if err != nil {
		return lostWith(answer, executorkeep.LossRestoreFailed, fmt.Sprintf("the turn's row on the executor "+
			"log could not be read: %v", err))
	}
	if !row.HasObject() {
		answer.KeptTurn = ""
	}
	if continuity := ContinuityOf(row, &run, run.LaunchSeq, restorer); !continuity.Continues {
		return lostWith(answer, continuity.Loss, continuity.Why)
	}
	answer.NotContinued = c.passedOver(run, *run.Kept, wait)
	answer.Capture, answer.Row = *run.Kept, row
	return answer, ""
}

// askFollowed asks again, never past by, the unanswered records of the job
// run's relaunch follows ([Coordinator.settleUnresolved]): its newest capture's
// and a save's beside it, whatever its key's age, the log deciding whether the
// window a record may name it in has passed. The stamp of what an ask settled
// is this node's own write, in its own room past by ([Coordinator.settleContext]).
func (c *Coordinator) askFollowed(ctx context.Context, by time.Time, run PendingRun) PendingRun {
	follows := run.LaunchSeq
	ask, cancel := context.WithDeadline(ctx, by)
	defer cancel()
	return c.settleUnresolved(ask, run, func(stamp SessionCapture) bool { return stamp.LaunchSeq == follows })
}

// passedOver is the relaunch's verdict where it continues kept, the newest
// capture the followed job kept, past a newer one of that job: its newest
// capture, or its save under way where that is another take. Nil where there is
// none. The newer one's loss is its own where it will not be kept, and
// [executorkeep.LossCaptureLate] where it may still be past the relaunch's
// wait, waited. Said once the launch decides ([Coordinator.notContinued]).
func (c *Coordinator) passedOver(run PendingRun, kept SessionCapture,
	waited time.Duration) *executorkeep.NotContinued {

	now := c.now()
	var (
		passed []string
		loss   executorkeep.Loss
	)
	for _, stamp := range []struct {
		what    string
		capture *SessionCapture
	}{
		{what: "the newest capture of the job the relaunch follows", capture: run.Capture},
		{what: "its save under way", capture: run.savingTake()},
	} {
		newer := stamp.capture
		if newer == nil || newer.LaunchSeq != run.LaunchSeq || !newer.newerThan(kept) {
			continue
		}
		named := fmt.Sprintf("%s (launch %d, capture %d) ", stamp.what, newer.LaunchSeq, newer.CaptureSeq)
		if how := landingHow(*newer, now); how != "" {
			named += fmt.Sprintf("may still be kept: %s, and the relaunch did not wait past %s for it", how,
				waited)
			if loss == "" {
				loss = executorkeep.LossCaptureLate
			}
		} else {
			named += "was not kept: " + unkeptWhy(*newer)
			if loss == "" {
				loss = unkeptLoss(*newer)
			}
		}
		passed = append(passed, named)
	}
	if len(passed) == 0 {
		return nil
	}
	return &executorkeep.NotContinued{Loss: loss, Instead: executorkeep.InsteadOlder,
		Older:  &executorkeep.CapturePlace{LaunchSeq: kept.LaunchSeq, CaptureSeq: kept.CaptureSeq},
		Detail: redact.Secrets(strings.Join(passed, "; "))}
}

// landingHow is how c may still become a kept capture as of now
// ([SessionCapture.mayLand]), in words for the log; "" where it cannot.
func landingHow(c SessionCapture, now time.Time) string {
	switch {
	case c.keeping(now):
		return "it is still being kept"
	case c.standing(now):
		return "a read stands by to keep it in the next place"
	case c.unresolvedWithin(now):
		return "its record was sent and not yet answered"
	}
	return ""
}

// continuedWithout is what a relaunch continues where the job it follows left
// no kept capture (one still being kept past its wait,
// [Coordinator.relaunchWaitOf], with none before it, or one not kept): the
// base the turn froze while it has kept nothing and its row still holds it
// ([frozenBase]); else nothing, why saying why not. The turn's row is read
// once, only where something asks it: the base, and a refused newest capture
// the row may name all the same ([namedThoughRefused]). What the row names is
// what the run is told it may read ([ContinuedSession.KeptTurn]).
func (c *Coordinator) continuedWithout(ctx context.Context, run PendingRun, restorer ArchiveRestorer,
	answer ContinuedSession, loss executorkeep.Loss, why string) (ContinuedSession, string) {

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
	base, held, baseRefused := frozenBase(run, ownRow, restorer)
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
		// AN OLDER SESSION: the base, which ends before every job of the turn.
		answer.Base = base
		if loss != "" {
			answer.NotContinued = &executorkeep.NotContinued{Loss: loss, Instead: executorkeep.InsteadOlder,
				OlderTurn: base.Turn, Detail: why}
		}
		return answer, ""
	}
	if baseRefused.Why != "" {
		why += "; " + baseRefused.Why
	}
	if loss == "" {
		loss = baseRefused.Loss
	}
	return lostWith(answer, loss, why)
}

// lostWith is answer continuing nothing for why, its verdict loss and a fresh
// session where loss names one, none where nothing was lost.
func lostWith(answer ContinuedSession, loss executorkeep.Loss, why string) (ContinuedSession, string) {
	if loss != "" {
		answer.NotContinued = executorkeep.Lost(loss, executorkeep.InsteadFresh, why)
	}
	return answer, why
}

// frozenBase is the session run's turn froze at its first launch, where a
// relaunch of a turn that has kept nothing continues it ([BaseContinuityOf] of
// the turn's own row, as ownRow reads it at the level a relaunch reads), or why
// not; and whether that row was read holding it against every release but a
// retirement ([baseStanding]), continued or not. nil, false and "" where the
// turn froze none.
func frozenBase(run PendingRun, ownRow func() (executorkeep.TurnRow, error),
	restorer ArchiveRestorer) (*BaseSession, bool, SessionContinuity) {

	if run.Base == nil || run.Base.Session == nil {
		return nil, false, SessionContinuity{}
	}
	base := *run.Base.Session
	if run.Kept != nil {
		return nil, false, BaseContinuityOf(base, nil, &run, restorer)
	}
	own, err := ownRow()
	switch {
	case errors.Is(err, ErrTurnRowNotFound):
		return nil, false, refusedFor(executorkeep.LossLogRefused, fmt.Sprintf("the turn's row on the executor "+
			"log holds nothing: its begin never landed, so turn %s's session is held against no release",
			base.Turn))
	case err != nil:
		return nil, false, refusedFor(executorkeep.LossRestoreFailed, fmt.Sprintf("the turn's row on the "+
			"executor log could not be read to see whether it holds turn %s's session: %v", base.Turn, err))
	}
	_, standing := baseStanding(base, &own, &run)
	held := standing == ""
	if continuity := BaseContinuityOf(base, &own, &run, restorer); !continuity.Continues {
		return nil, held, continuity
	}
	return &base, held, continues
}

// continuesNoKept is why this coordinator continues no kept session at all,
// "" where it may: a node built with nothing to restore one from, or a company
// that keeps no session as this node's revision says ([SessionKeep.Keeps]):
// `turn_engine.conversation_session.enabled: false`, under which a relaunch
// continues at most the one its own box holds, whatever a capture kept while
// the switch was on, and no turn continues its conversation's last one.
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
// keeps none.
const NoKeptSession = "the company keeps no executor session (turn_engine.conversation_session.enabled " +
	"is false), so a relaunch continues at most the session its own box holds, and a conversation's next " +
	"turn, or a schedule's next fire, continues none"

// copyContext is what the last request of run's current job's session carried,
// as that job's newest capture measured it, kept or not: the measure of that
// job's own copy in its box, which went at least as far
// ([ContinuedSession.CopyContext]); zero where none measured any.
func copyContext(run PendingRun) int64 {
	if newest := run.Capture; newest != nil && newest.LaunchSeq == run.LaunchSeq {
		return newest.ContextTokens
	}
	return 0
}

// copyNextRequest is the harness's own estimate of the next request of run's
// current job's session, as that job's newest capture read it, kept or not
// ([copyContext]); zero where none estimated one.
func copyNextRequest(run PendingRun) NextRequest {
	if newest := run.Capture; newest != nil && newest.LaunchSeq == run.LaunchSeq {
		return newest.NextRequest()
	}
	return NextRequest{}
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
	// newest the followed job kept, or none. A newer one still being kept does
	// not count against it until it lands ([Coordinator.continueKept]).
	LaunchSeq, CaptureSeq int64

	// Base says the session is the one the conversation's last turn left, as
	// the turn's first launch froze it ([PendingRun.Base]), in place of a
	// capture of the turn's own: continued only in a box of its own, so Brief is
	// empty and Replaced is what the run is handed ([Coordinator.continueBase]).
	Base bool

	// Brief is the run's prompt where the box is the one the session was last
	// worked in, its working tree as the job before left it.
	Brief string

	// Replaced is the run's prompt where the box is another (a fresh checkout):
	// Brief and what a replaced box changes (which branch to check out, the
	// uncommitted work gone, the earlier box's absolute paths).
	Replaced string

	// Told is what Brief and Replaced told of the round the launch carries on,
	// the same for both ([BriefTold]): recorded on the job the launch opens.
	Told BriefTold

	// Handover is what every telling of it hands of the summary of a newer
	// session the run does not continue ([SummaryHandover]).
	Handover SummaryHandover

	// Exact is the briefs told by a reading of what the kept capture holds
	// ([HoldingsAsk]), beside Brief and Replaced, which tell by the save point;
	// nil where no reading was read or its telling is not to be handed. Only for
	// a kept capture, never Base. The launch hands one only to a box whose
	// harness answers the release the reading was read for
	// ([ExactTelling.Resumer]), and Brief or Replaced to every other box
	// ([Coordinator.continueKept]).
	Exact *ExactTelling

	// Summarized is the briefs told as a session that may be a summary, where
	// the session's first request is the launch's to judge in its box (an
	// OpenCode session whose count has a bound): handed where the box's point
	// does not prove the brief the launch would hand sends the session whole
	// ([BoxPoint.SendsWhole]). Nil where the runner judged the first
	// request itself. Count and CountProvider are what that is judged by: the
	// count the runner judged the kept path by, and the provider of the reply it
	// is anchored on.
	Summarized    *SummarizedTelling
	Count         *ResumedCount
	CountProvider string

	// Unheld is the briefs for a box whose harness answers a release whose
	// resume this build did not read ([CarryCheck.ResumeRead]); nil where they
	// would tell the same. Only for a kept capture, handed only to such a box.
	Unheld *UnheldTelling

	// UnheldDropped says the seat's budget could not carry Unheld's briefs,
	// which the engine dropped alone (internal/engine's continuedFloor): the
	// session is then continued only in a box whose release the resume was read
	// in, and any other starts a session of its own.
	UnheldDropped bool
}

// UnheldTelling is a relaunch's briefs for a box whose release this build did
// not read the resume of: a read at a hook's step is held only as the job's
// session showed it, which such a box's resume may not rebuild, so these tell
// the notes from the first such read on again, as notes the session may
// already show, and every late call a hook took ([CallTelling.Hook]); or,
// where its harness may compact the session before it reads them, they are
// the briefs told as a session that may be a summary.
type UnheldTelling struct {
	Brief, Replaced string

	// Told is what Brief and Replaced told of the round ([BriefTold]), recorded
	// in place of [SessionContinuation.Told] where the launch hands either.
	Told BriefTold
}

// ExactTelling is a relaunch's briefs told by a reading of what the session it
// continues holds ([Coordinator.HoldingsOf]): told only the calls of the round
// the reading does not find held, where the briefs beside them
// ([SessionContinuation.Brief], [SessionContinuation.Replaced]) tell every call
// after the save point. HANDED ONLY TO A BOX OF THE RELEASE IT WAS READ FOR:
// what a session holds is what the resuming release's resume hands its model
// ([HoldingsAsk.Resumer]), and the box is acquired inside the launch after the
// briefs are written, so the reading is read for the release that wrote the
// session and handed only where the box's harness answers exactly it
// ([SessionRestorer.CheckCarry]). Any other release, or an unreadable answer,
// gets the save point's briefs, which tell more and never less.
type ExactTelling struct {
	// Resumer is the release the reading was read for ([HoldingsAsk.Resumer]),
	// "2.1.310": the one a box's harness must answer for Brief or Replaced to
	// be handed to it. "" matches no box.
	Resumer string

	// Brief and Replaced are as [SessionContinuation]'s; "" where the telling
	// wrote none for that box, which hands the save point's brief there.
	Brief, Replaced string

	// Told is what Brief and Replaced told of the round ([BriefTold]), recorded
	// in place of [SessionContinuation.Told] where the launch hands either.
	Told BriefTold
}

// BriefKind is which of the four briefs a launch handed its run: a run that
// continues the turn's session in the box it was last worked in, one that
// continues it in a box that replaced that one, one that continues the session
// its conversation's last turn left, and one that starts a session of its own.
type BriefKind string

// The four briefs, as a launch's record and its collection name them: the
// turn's session in the box it was last worked in, restored into another box,
// the conversation's last turn's session restored into a box of the launch's
// own, and a session of the run's own.
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
// one takes it up where its box can, wired beside it for a session of its own
// where not ([RunRequest.FreshWiring]). An unknown brief continues nothing.
func (k BriefKind) Continues() bool {
	return k == BriefContinued || k == BriefContinuedNewBox || k == BriefContinuedConversation
}

// LaunchDecision is how a launch decided its run takes up a session, told to
// its caller before the run starts ([LaunchRequest.Decided]).
type LaunchDecision struct {
	// Brief is which of the four briefs the launch hands the run.
	Brief BriefKind

	// Kept says the session the run continues is the one the launch was asked
	// to continue in any box ([LaunchRequest.Session]); false where it is the
	// job before's own copy in its box ([LaunchRequest.Continue]), or none.
	Kept bool

	// Base says that session is the one the conversation's last turn left
	// ([SessionContinuation.Base]): always in a box of the launch's own.
	Base bool

	// Exact says the brief handed is one told by a reading of what the session
	// holds ([SessionContinuation.Exact]): its box's harness answered the
	// release the reading was read for. Unheld says it is one for a box whose
	// release the resume was not read in ([SessionContinuation.Unheld]). Both
	// false where it hands the save point's brief, or continues no kept session.
	Exact, Unheld bool

	// Summarized says the brief handed is the one told as a session that may be
	// a summary ([SessionContinuation.Summarized], [Continuation.Summarized]):
	// the box's point did not prove the session reaches its first request
	// whole, so its guard arms nothing.
	Summarized bool
}

// continuedLaunch is how one launch continues the turn's session, if it does:
// the brief its run starts with and the one it falls back to, the session it
// takes up and the archive carried into its box, and which brief it is.
type continuedLaunch struct {
	// notContinued is the launch's final verdict on the session it did not
	// continue, nil where it lost none ([Coordinator.notContinued]).
	notContinued *executorkeep.NotContinued
	// handover is what brief hands of the lost session's summary, and
	// freshHandover what fresh does ([SummaryHandover]).
	handover, freshHandover SummaryHandover

	brief, fresh, session string
	carried               *CarriedSession
	kind                  BriefKind
	// told is what brief told of the round the launch carries on
	// ([BriefTold]): the fresh brief's where it continues no session.
	told BriefTold
	// kept says the session is the turn's kept capture rather than the job
	// before's own copy in its box ([LaunchDecision.Kept]).
	kept bool
	// exact says brief is the one told by a reading of what the kept capture
	// holds ([SessionContinuation.Exact]), unheld one for a box whose release
	// the resume was not read in ([SessionContinuation.Unheld]), so told is
	// that telling's own.
	exact, unheld bool
	// summarized says brief is the one told as a session that may be a summary
	// ([LaunchDecision.Summarized]), so told is that telling's own.
	summarized bool
	// measure is what the session carried at its newest request the engine
	// measured, along its lineage: the job before's record of its own copy,
	// where the launch continues that copy in place; the kept capture's measure
	// of its archive, its job named as the archive's; the frozen base's; none
	// for a session of the run's own. Written on the job's record with its
	// command ([BoxRef.Context]), so a job that makes no request of its own is
	// measured by the copy it took up, never by a copy of another lineage.
	measure *SessionMeasure
	// restart is the newest place the session restarts at, as the launch knows
	// it: the job before's collection's read of its own copy, where that copy
	// continues in place; the kept capture's read of its archive; "" for a
	// session of the run's own; nil where not known (the conversation's base,
	// which no count describes). Written on the job's record
	// ([BoxRef.Restart]): what a later relaunch compares the job's own copy
	// with, to know whether it restarted after the brief ([RestartedSince]).
	restart *string
	// point is the save point of the kept capture the launch hands its run, with
	// that capture's launch, where the launch carries its round on and the
	// capture states one above zero; nil otherwise ([LaunchRecord.Continued]).
	// Written on the job's record ([BoxRef.Continued]).
	point *ContinuedPoint
	// release closes the session's stream into the box once the run has
	// started (or failed to).
	release func()
}

// carriedMeasure is a figure a launch carries along its session's lineage from
// source, of launch's job where source names one: none where it is no count (a
// capture that measured nothing records 0).
func carriedMeasure(tokens int64, source MeasureSource, launch string) *SessionMeasure {
	if tokens <= 0 {
		return nil
	}
	return &SessionMeasure{Tokens: tokens, Source: source, Launch: launch}
}

// decision is how the launch decided, for its caller ([LaunchRequest.Decided]).
func (l continuedLaunch) decision() LaunchDecision {
	return LaunchDecision{Brief: l.kind, Kept: l.kept, Base: l.kind == BriefContinuedConversation, Exact: l.exact,
		Summarized: l.summarized, Unheld: l.unheld}
}

// omitted is what the launch req records on the job it opens of the calls of
// the round it carries on that the brief it hands its run left out, and, where
// that brief continues a session, the fresh one its box falls back to, each of
// both kinds ([BriefTold.Omitted], [BriefTold.LateOmitted];
// [LaunchRecord.BriefOmitted], [LaunchRecord.FreshOmitted]).
//
// NONE FOR A LAUNCH THAT CARRIES NO ROUND ON ([LaunchRequest.Continue] nil): a
// turn's first launch tells no round's calls, and a send-back opens a new
// round, which numbers its calls afresh, so a set of the collected round's
// calls would be read against another round's log.
func (l continuedLaunch) omitted(req LaunchRequest) (brief, fresh BriefTold) {
	if req.Continue == nil {
		return BriefTold{}, BriefTold{}
	}
	if l.kind.Continues() {
		fresh = req.Continue.Fresh
	}
	return l.told, fresh
}

// freshLaunch is a launch that continues no session: what its fresh brief told
// of the round it carries on, and only the leads it opened with where it
// carries none on (a turn's first launch, a send-back: [LaunchRequest.FreshOwn]).
func freshLaunch(req LaunchRequest) continuedLaunch {
	// A SESSION OF THE RUN'S OWN restarts nowhere yet: its basis is none.
	launch := continuedLaunch{brief: req.Brief, kind: BriefFresh, restart: new(""), release: func() {},
		told: BriefTold{Own: req.FreshOwn}}
	if req.Continue != nil {
		launch.told = req.Continue.Fresh
	}
	return launch
}

// continueSession is how the launch req continues its turn's session in box,
// given the run's record as the launch opened it, whether box is the job
// before's (reused), and whether this launch's begin was read back holding the
// base (begun, [Coordinator.recordBegin]). In order: the session its caller
// chose ([LaunchRequest.Session]), the kept capture over no copy that went on
// past it ([copyWentPast]) or the conversation's base in a box of its own;
// else the job before's copy in its own box, unless that job ran out of
// context in it or the box's harness continues no copy ([inPlaceRefused]);
// else a session of its own. Each session not continued is
// logged once the one it falls to is decided ([Coordinator.notContinued]), and
// a reading of a capture passed over for the copy is said unused
// ([Coordinator.exactUnused]).
func (c *Coordinator) continueSession(ctx context.Context, req LaunchRequest, opened PendingRun,
	box Sandbox, runner Runner, reused, begun bool) continuedLaunch {

	var (
		refused []refusal
		// readPassedOver: the capture passed over for the box's copy
		// ([copyWentPast]) came with a reading-told brief.
		readPassedOver bool
	)
	if startsFresh(runner) {
		// A HARNESS THAT CONTINUES NO SESSION starts one of its own: handed
		// a kept one to continue, its runner would refuse the run, and the
		// session was written by another harness. AND SAYS WHAT IT LOST, as
		// the fresh path does: its own refusal of a session it was handed, or
		// the caller's verdict on one the engine dropped before the launch.
		launch := freshLaunch(req)
		if req.Session != nil {
			refused = []refusal{{executorkeep.LossHarnessChanged, "the run's harness continues no earlier session"}}
		}
		launch.notContinued = c.notContinued(ctx, req, opened, refused, startsItsOwn)
		return launch.toldFresh(req, opened)
	}
	if want := req.Session; want != nil {
		var (
			launch continuedLaunch
			loss   executorkeep.Loss
			why    string
		)
		if want.Base {
			launch, loss, why = c.continueBase(ctx, req, opened, box, runner, reused, begun, *want)
		} else if why = copyWentPast(req, opened, reused, *want); why == "" {
			launch, loss, why = c.continueKept(ctx, req, opened, box, runner, reused, *want)
		} else {
			readPassedOver = want.Exact != nil
		}
		if why == "" {
			// WHAT ITS BRIEFS TOLD, the same for either box
			// ([SessionContinuation.Told]); where the brief handed is a
			// reading's, a possible summary's, or an unheld box's, that
			// telling's own ([ExactTelling.Told], [SummarizedTelling.Told],
			// [UnheldTelling.Told]). Chosen here, the one assignment, so every
			// continuation records what its brief left out.
			switch {
			case launch.exact:
				launch.told = want.Exact.Told
			case launch.summarized:
				launch.told = want.Summarized.Told
			case launch.unheld:
				launch.told = want.Unheld.Told
			default:
				launch.told = want.Told
			}
			// THE CALLER'S VERDICT STANDS: the session it chose is continued,
			// an older one where it says so.
			launch.notContinued, launch.handover = req.NotContinued, want.Handover
			return launch.toldFresh(req, opened)
		}
		refused = append(refused, refusal{loss, why})
	}
	launch, outcome := freshLaunch(req), startsItsOwn
	if continued := req.Continue; continued != nil && reused && continued.SessionID != "" {
		if spent := opened.spentBy(continued.SessionID); spent != "" {
			refused = append(refused, refusal{spent,
				executorkeep.SpentWhy(continued.SessionID, req.Turn.TurnID, spent)})
		} else if entry := EntryContinues(opened.Prior.Entry, req.Entry, req.Spec.Harness,
			resumesOnLaunchRoute(runner)); !entry.Continues {
			refused = append(refused, refusal{entry.Loss, entry.Why})
		} else if why := inPlaceRefused(ctx, box, runner); why != "" {
			refused = append(refused, refusal{executorkeep.LossBoxRefused, why})
		} else {
			// THE COPY THE BOX HOLDS, measured as the job before's record holds
			// it ([PriorBox.Context]).
			launch = continuedLaunch{brief: continued.Brief, fresh: req.Brief, session: continued.SessionID,
				kind: BriefContinued, told: continued.Told, measure: opened.Prior.Context,
				restart: opened.Prior.Restart, release: func() {}}
			// AN OPENCODE COPY'S FIRST REQUEST, judged in this box where the
			// caller handed the summarized telling beside the held one (the
			// package doc's "first request is judged in its box"): the point is
			// paired with the release the job before's record names as the
			// copy's writer, whether or not the company keeps captures
			// ([PriorBox.Writer]).
			if continued.Summarized != "" {
				judge := &boxJudge{path: "in_place", count: continued.Count, provider: continued.CountProvider,
					ask: PointAsk{Writer: opened.Prior.Writer}}
				if !c.sendsWhole(ctx, req, box, runner, judge, continued.Brief) {
					launch.brief, launch.told, launch.summarized = continued.Summarized, continued.SummarizedTold, true
					c.mayCompact(ctx, req, judge, continued.Brief)
				}
			}
			outcome = continuesBoxCopy
		}
	}
	if outcome == startsItsOwn {
		launch.notContinued = c.notContinued(ctx, req, opened, refused, outcome)
	}
	if readPassedOver && outcome == continuesBoxCopy {
		c.exactUnused(ctx, req, copyWentPastUnused)
	}
	return launch.toldFresh(req, opened)
}

// toldFresh is l with its fresh brief told under the verdict a run reading it
// would have ([toldFresh]): l's own where it continues no session, so the brief
// names the loss the launch decided after it was built; restore_failed's of
// the session it continues where its box falls back from it ([fellBack]), the
// one loss that fallback stamps.
func (l continuedLaunch) toldFresh(req LaunchRequest, opened PendingRun) continuedLaunch {
	if !l.kind.Continues() {
		told := toldFresh(req, l.notContinued)
		l.brief, l.handover = told.Text, told.Handover
		return l
	}
	told := toldFresh(req, fellBack(l.kind, opened.Base, ""))
	l.fresh, l.freshHandover = told.Text, told.Handover
	return l
}

// refusal is why a launch did not continue a session it was offered, and the
// loss it is ("" where nothing was lost).
type refusal struct {
	loss executorkeep.Loss
	why  string
}

// inPlaceRefused is why box's harness continues no copy of a session the box
// itself holds ([SessionRestorer.CheckInPlace]), "" where it does or its
// runner restores nothing; a box that could not be asked continues nothing,
// as a carried session's check refuses one ([Coordinator.continueKept]). Each is
// the box's refusal ([executorkeep.LossBoxRefused]).
func inPlaceRefused(ctx context.Context, box Sandbox, runner Runner) string {
	restorer, ok := runner.(SessionRestorer)
	if !ok {
		return ""
	}
	check, err := restorer.CheckInPlace(ctx, box)
	switch {
	case err != nil:
		return fmt.Sprintf("the box could not be asked whether its harness continues its own copy of the "+
			"session: %v", err)
	case !check.Continues:
		return check.Why
	}
	return ""
}

// copyWentPastUnused is why a launch that continues its job's own box's copy in
// place of the kept capture that copy went past ([copyWentPast]) hands none of
// the brief told by a reading of that capture ([Coordinator.exactUnused]).
const copyWentPastUnused = "the launch continues the copy of the session its job's own box holds, which went " +
	"on past the capture the reading was read of"

// copyWentPast is why the launch req, in the box the job it follows ran in
// (reused), does not restore the turn's kept capture want over that box's own
// copy of the session; "" where nothing keeps it from trying. A capture taken
// while its job still worked (not [SessionCapture.Final]) is a point the
// session passed through: the box's copy holds all of it and all the job did
// after, so restoring it there would roll the session back. Where the launch
// is offered that copy ([Continuation.SessionID]) and it is the capture's own
// session, the copy is continued instead. A final capture is restored as ever
// (finality is the fact read, never age or place). A capture the record does
// not keep is [Coordinator.continueKept]'s to refuse, and a launch offered no
// copy tries the capture whatever session its stamp names.
func copyWentPast(req LaunchRequest, opened PendingRun, reused bool, want SessionContinuation) string {
	kept, offered := opened.Kept, req.Continue
	switch {
	case !reused || offered == nil || offered.SessionID == "":
		return ""
	case kept == nil || kept.LaunchSeq != want.LaunchSeq || kept.CaptureSeq != want.CaptureSeq:
		return ""
	case kept.Final || kept.SessionID != offered.SessionID:
		return ""
	}
	return fmt.Sprintf("the box this launch reuses holds the job it follows' own copy of the session, which "+
		"went on past the kept capture (launch %d, capture %d): that capture was taken while the job still "+
		"worked, so it is not restored over the copy", kept.LaunchSeq, kept.CaptureSeq)
}

// continueKept continues the turn's kept capture want in box, or says why it
// cannot. THE BOX THE CAPTURE WAS TAKEN IN IS THE REUSED ONE ([ContinuityOf],
// [PriorBox.ContinuesIn]): the same-box brief is handed exactly where the
// session was last worked in, the new-box brief everywhere else. WHICH TELLING,
// ONCE THE BOX'S HARNESS HAS SAID ITS RELEASE ([SessionRestorer.CheckCarry]):
// the reading's only where that answer is exactly the release it was read for
// ([exactFor]), else logged (`executor_session_holdings_unused`); the unheld
// one where the release's resume was not read ([UnheldTelling]), and no
// session where the seat's budget dropped it
// ([SessionContinuation.UnheldDropped]). The box's own copy is asked its
// release only where an unheld telling is at stake: the one that wrote it
// (DISABLE_AUTOUPDATER is pinned).
func (c *Coordinator) continueKept(ctx context.Context, req LaunchRequest, opened PendingRun,
	box Sandbox, runner Runner, reused bool, want SessionContinuation) (continuedLaunch, executorkeep.Loss, string) {

	if why := c.continuesNoKept(); why != "" {
		return continuedLaunch{}, "", why
	}
	kept := opened.Kept
	if kept == nil || kept.LaunchSeq != want.LaunchSeq || kept.CaptureSeq != want.CaptureSeq {
		return continuedLaunch{}, executorkeep.LossCaptureLate, "the turn's kept session is not the one its launch was asked to continue"
	}
	restorer, _ := runner.(SessionRestorer)
	row, err := c.restore.Session(ctx, req.Turn.AgentHandle, req.Turn.TurnID)
	if err != nil {
		return continuedLaunch{}, executorkeep.LossRestoreFailed, fmt.Sprintf("the turn's row on the executor log could not be read: %v", err)
	}
	// ASKED AGAIN of the record as this launch opened it, which numbers this job
	// one past the job it follows: a capture of that job that LANDED since the
	// caller asked, kept on the record or named by the turn's row
	// ([ContinuityOf]), refuses the chosen capture, whose briefs were built for
	// it; the launch then starts a session of its own or continues its box's
	// copy, never the newer capture. One that only began since, or is still
	// being kept, does not: the chosen capture is still the newest kept.
	continuity := ContinuityOf(row, &opened, opened.LaunchSeq-1, RestorerUnder(restorer, req.Entry))
	if !continuity.Continues {
		return continuedLaunch{}, continuity.Loss, continuity.Why
	}
	brief, kind := want.Brief, BriefContinued
	if !reused {
		brief, kind = want.Replaced, BriefContinuedNewBox
	}
	if brief == "" {
		return continuedLaunch{}, executorkeep.LossBudgetFloor, "the launch was given no brief that continues the session in this box"
	}
	// MEASURED AS THE CAPTURE MEASURED ITS ARCHIVE, never as the job's request.
	measure := carriedMeasure(kept.ContextTokens, MeasureCapture, kept.Launch)
	// AND WHERE THE ARCHIVE RESTARTS LAST, as the capture's own read found it.
	restart := restartOf(kept.ResumedCount)
	// AND ITS SAVE POINT, for a later capture of the job to be raised to: only
	// where the launch carries its round on, since a round it opens numbers its
	// calls from a new log ([LaunchRecord.Continued]).
	point := continuedPoint(req, *kept)
	// THE BOX'S OWN COPY, where its harness exited cleanly in this very box and
	// its session was taken whole there: the box holds the archive, so nothing
	// is fetched to be kept over a copy it would not touch. No reading-told
	// brief is handed here: a reading reads only a cut-off capture
	// ([Coordinator.HoldingsOf]), so [exactFor] is not asked.
	own := reused && !kept.Interrupted
	if own {
		if why := inPlaceRefused(ctx, box, runner); why != "" {
			return continuedLaunch{}, executorkeep.LossBoxRefused, why
		}
	}
	archive := ArchiveOf(row)
	// WHERE ITS FIRST REQUEST IS THIS LAUNCH'S TO JUDGE (an OpenCode session
	// handed with its summarized telling), the box's point, read once on every
	// branch, after CheckCarry where that runs: the reading-told brief where it
	// sends whole, else the held brief where it does, else the summarized one.
	var judge *boxJudge
	if want.Summarized != nil {
		judge = &boxJudge{path: "kept", count: want.Count, provider: want.CountProvider,
			ask: PointAsk{Writer: archive.Version}}
	}
	// held is brief where it may be handed, else the summarized brief for
	// this box, logged: "" and why where there is none.
	held := func(brief string) (string, bool, string) {
		if judge == nil || c.sendsWhole(ctx, req, box, runner, judge, brief) {
			return brief, false, ""
		}
		summed := want.Summarized.Replaced
		if reused {
			summed = want.Summarized.Brief
		}
		if summed == "" {
			return "", false, "the launch was given no brief told as a session that may be a summary for this box"
		}
		c.mayCompact(ctx, req, judge, brief)
		return summed, true, ""
	}
	var check CarryCheck
	if !own || want.Unheld != nil || want.UnheldDropped {
		check, err = restorer.CheckCarry(ctx, box, archive)
		switch {
		case err != nil:
			return continuedLaunch{}, executorkeep.LossBoxRefused, fmt.Sprintf("the box could not be asked whether its harness continues "+
				"the session: %v", err)
		case !own && !check.Continues:
			return continuedLaunch{}, check.refusal(), check.Why
		}
	}
	var exact, unused string
	if !own {
		exact, unused = exactFor(want.Exact, check, reused)
	}
	if judge != nil {
		judge.ask.Release = check.Version
		if exact != "" && !c.sendsWhole(ctx, req, box, runner, judge, exact) {
			exact, unused = "", exactMayCompact
		}
	}
	unheld, summarized := exact == "" && want.Unheld != nil && !check.ResumeRead, false
	switch {
	case exact != "":
		brief = exact
	case unheld:
		// A BOX WHOSE RELEASE THE RESUME WAS NOT READ IN ([UnheldTelling]).
		brief = want.Unheld.Replaced
		if reused {
			brief = want.Unheld.Brief
		}
	case want.UnheldDropped && !check.ResumeRead:
		return continuedLaunch{}, executorkeep.LossBudgetFloor, fmt.Sprintf("the seat's budget could not carry the briefs for a box whose "+
			"release the resume was not read in, and this box's harness answers %s", answeredRelease(check))
	default:
		var why string
		if brief, summarized, why = held(brief); why != "" {
			return continuedLaunch{}, executorkeep.LossBudgetFloor, why
		}
	}
	if own {
		return continuedLaunch{brief: brief, fresh: req.Brief, session: row.SessionID, kind: kind,
			kept: true, unheld: unheld, summarized: summarized, measure: measure, restart: restart,
			point: point, release: func() {}}, "", ""
	}
	fetched, err := c.fetchSession(ctx, row)
	if err != nil {
		return continuedLaunch{}, executorkeep.LossRestoreFailed, fmt.Sprintf("the session could not be fetched: %v", err)
	}
	body, err := fetched.open(ctx, c.restore)
	if err != nil {
		return continuedLaunch{}, executorkeep.LossRestoreFailed, fmt.Sprintf("the session could not be read again to carry it: %v", err)
	}
	if unused != "" {
		c.exactUnused(ctx, req, unused)
	}
	// THE RESTORE CARRIER, chosen here and nowhere else: the turn's own kept
	// session, read off the turn's own row and opened against that row's
	// associated data, so no other seat's or turn's session opens into the run;
	// it replaces the box's copy where its harness was cut off.
	carried := &CarriedSession{Archive: archive, Body: body, Replace: kept.Interrupted}
	return continuedLaunch{brief: brief, fresh: req.Brief, session: row.SessionID, kind: kind,
		kept: true, exact: exact != "", unheld: unheld, summarized: summarized, carried: carried,
		measure: measure, restart: restart, point: point, release: func() { _ = body.Close() }}, "", ""
}

// continuedPoint is the save point of kept, the capture launch req hands its
// run ([LaunchRecord.Continued]): nil where req carries no round on
// ([continuedLaunch.omitted]) or kept states no point above zero.
func continuedPoint(req LaunchRequest, kept SessionCapture) *ContinuedPoint {
	if at, stated := kept.SavePoint(); req.Continue != nil && stated && at > 0 {
		return &ContinuedPoint{LaunchSeq: kept.LaunchSeq, Through: at}
	}
	return nil
}

// answeredRelease is the release check's box answered, in a person's words.
func answeredRelease(check CarryCheck) string {
	if check.Version == "" {
		return "no release this build reads"
	}
	return fmt.Sprintf("%q", check.Version)
}

// exactFor is the brief told by a reading that a launch hands a box whose
// harness answered check, the box the session was last worked in where reused:
// "" where it hands none, and then why ("" too where it was given no such
// telling: [SessionContinuation.Exact]).
//
// EXACTLY THE RELEASE THE READING WAS READ FOR, and a release read: an unread
// answer ("", which continues no carried session: [CarryCheck.Version]) matches
// nothing, since what a session holds is what the resuming release's resume
// hands its model ([HoldingsAsk.Resumer]), and a box of another release may
// hand it otherwise. Equality, harness-neutral, rather than the reader's set of
// releases: a box of another release the reader also read costs the telling
// alone, never a call counted held.
func exactFor(exact *ExactTelling, check CarryCheck, reused bool) (brief, why string) {
	if exact == nil {
		return "", ""
	}
	switch {
	case check.Version == "":
		return "", fmt.Sprintf("the box's harness answered no release this build reads, and the reading "+
			"was read for %q", exact.Resumer)
	case check.Version != exact.Resumer:
		return "", fmt.Sprintf("the box's harness answered %q, and the reading was read for %q",
			check.Version, exact.Resumer)
	}
	brief = exact.Replaced
	if reused {
		brief = exact.Brief
	}
	if brief == "" {
		return "", "the launch was given no brief told by the reading for this box"
	}
	return brief, ""
}

// exactUnused logs why a launch that continues the turn's session hands it a
// brief other than the reading-told one: the kept capture told by its save
// point ([exactFor]), or the copy its job's own box holds, which went on past
// that capture ([copyWentPast]). Keyed as [Coordinator.notContinued]'s line.
func (c *Coordinator) exactUnused(ctx context.Context, req LaunchRequest, why string) {
	log.InfoContext(ctx, "executor_session_holdings_unused", "turn_id", req.Turn.TurnID,
		"agent", req.Turn.AgentHandle, "detail", redact.Secrets(why), "outcome", exactUnusedOutcome)
}

// exactUnusedOutcome is what a launch not handing the reading-told brief hands
// instead.
const exactUnusedOutcome = "the relaunch is handed the telling without the reading"

// exactMayCompact is why a launch does not hand an OpenCode session the
// reading-told brief where the box's point does not prove it sends the session
// whole ([Coordinator.exactUnused]).
const exactMayCompact = "its harness may compact the session in this box before it reads the brief told by the " +
	"reading"

// boxJudge is how a launch judges, in its box, whether a session it
// continues reaches its first request whole ([BoxPoint.SendsWhole]): the
// path (`kept` or `in_place`), the count and the anchoring reply's provider,
// the ask, and the point, read at most once.
type boxJudge struct {
	path     string
	count    *ResumedCount
	provider string
	ask      PointAsk
	read     bool
	point    BoxPoint
}

// sendsWhole reports whether the session j judges reaches its first request
// whole with brief, the box's point read on its first ask ([PointReader]): no
// point (no reader, or a box that could not be asked) sends nothing whole.
func (c *Coordinator) sendsWhole(ctx context.Context, req LaunchRequest, box Sandbox, runner Runner,
	j *boxJudge, brief string) bool {

	if !j.read {
		j.read = true
		j.ask.Model, j.ask.Env = req.Model, withHomePaths(req.Spec.Env, req, box)
		j.point = readPoint(ctx, box, runner, j.ask)
	}
	provider, _ := ModelProvider(req.Model)
	return j.point.SendsWhole(j.count, j.provider, provider, brief)
}

// readPoint is box's point as runner reads it ([PointReader]), no
// point where it reads none or the box could not be asked.
func readPoint(ctx context.Context, box Sandbox, runner Runner, ask PointAsk) BoxPoint {
	reader, ok := runner.(PointReader)
	if !ok {
		return BoxPoint{Mark: PointUnread, Why: "the box's runner reads no box point"}
	}
	point, err := reader.ReadBoxPoint(ctx, box, ask)
	if err != nil {
		return BoxPoint{Mark: PointUnread,
			Why: redact.Secrets("the box could not be asked for its point: " + err.Error())}
	}
	return point
}

// mayCompact logs that a launch hands the session j judged the brief told as
// one that may be a summary, the box's point not proving brief sends it whole:
// `executor_session_may_compact`, keyed as [Coordinator.notContinued]'s.
func (c *Coordinator) mayCompact(ctx context.Context, req LaunchRequest, j *boxJudge, brief string) {
	bound, _ := j.count.Bound()
	var mark CountMark
	if j.count != nil {
		mark = j.count.Mark
	}
	log.InfoContext(ctx, "executor_session_may_compact", "turn_id", req.Turn.TurnID,
		"agent", req.Turn.AgentHandle, "path", j.path, "bound", bound, "mark", string(mark),
		"provider", j.provider, "model", req.Model, "release", j.point.Release, "point", j.point.Tokens,
		"never", j.point.Never, "point_mark", string(j.point.Mark), "detail", j.point.Why,
		"box_tokens", j.point.Charge.Tokens(j.point.BoxBytes), "brief_tokens", j.point.Charge.Tokens(int64(len(brief))),
		"outcome", mayCompactOutcome)
}

// mayCompactOutcome is what a launch whose box may compact the session before
// its first request hands instead of the held telling ([Coordinator.mayCompact]).
const mayCompactOutcome = "the continuation is told as one that may be a summary, and its guard arms no skill"

// continueBase continues, in box, the session the turn's conversation's last
// turn left, as the first launch froze it ([PendingRun.Base]), or says why it
// cannot. IN THIS ORDER, each before anything the next costs: the company keeps
// sessions; the turn froze a base, kept none of its own, and this box is a box
// of its own (the earlier job's is [Coordinator.continueSession]'s); the turn's
// own row HOLDS the base (its begin landed naming it, no capture since: this
// launch's begin read back where begun says so, else a read of the row) and the
// base's row still names the object the read froze (a point read); the runner
// restores it ([BaseContinuityOf]); the box's harness takes it up
// ([SessionRestorer.CheckCarry]); the sealed object opens whole. THE RESTORE
// CARRIER, chosen here and in [Coordinator.continueKept] only: opened against
// its own row's associated data, so no other seat's session opens in.
func (c *Coordinator) continueBase(ctx context.Context, req LaunchRequest, opened PendingRun,
	box Sandbox, runner Runner, reused, begun bool, want SessionContinuation) (continuedLaunch, executorkeep.Loss, string) {

	if why := c.continuesNoKept(); why != "" {
		return continuedLaunch{}, "", why
	}
	switch {
	case opened.Base == nil || opened.Base.Session == nil:
		return continuedLaunch{}, "", "the turn froze no session of its conversation to continue"
	case opened.Kept != nil:
		return continuedLaunch{}, "", "the turn has kept a session of its own, which holds everything the one its " +
			"conversation's last turn left does, so no launch of the turn continues that one any more"
	case reused:
		return continuedLaunch{}, "", "the launch reuses the box the turn's job before ran in, and the session " +
			"its conversation's last turn left is continued only in a box of its own"
	case want.Replaced == "":
		return continuedLaunch{}, executorkeep.LossBudgetFloor, "the launch was given no brief that continues its conversation's session"
	}
	base := *opened.Base.Session
	var own *executorkeep.TurnRow
	if !begun {
		read, err := c.restore.Session(ctx, req.Turn.AgentHandle, req.Turn.TurnID)
		switch {
		case errors.Is(err, ErrTurnRowNotFound):
			return continuedLaunch{}, executorkeep.LossLogRefused, fmt.Sprintf("the turn's row on the executor log holds nothing: its "+
				"begin never landed, so turn %s's session is held against no release", base.Turn)
		case err != nil:
			return continuedLaunch{}, executorkeep.LossRestoreFailed, fmt.Sprintf("the turn's row on the executor log could not be read: %v",
				err)
		}
		own = &read
	}
	row, err := c.restore.Session(ctx, req.Turn.AgentHandle, base.Turn)
	if err != nil {
		return continuedLaunch{}, executorkeep.LossRestoreFailed, fmt.Sprintf("turn %s's row on the executor log could not be read: %v",
			base.Turn, err)
	}
	if why := base.namedBy(row); why != "" {
		return continuedLaunch{}, executorkeep.LossLogRefused, why
	}
	restorer, _ := runner.(SessionRestorer)
	if continuity := BaseContinuityOf(base, own, &opened, RestorerUnder(restorer, req.Entry)); !continuity.Continues {
		return continuedLaunch{}, continuity.Loss, continuity.Why
	}
	archive := base.archive()
	check, err := restorer.CheckCarry(ctx, box, archive)
	switch {
	case err != nil:
		return continuedLaunch{}, executorkeep.LossBoxRefused, fmt.Sprintf("the box could not be asked whether its harness continues "+
			"the session: %v", err)
	case !check.Continues:
		return continuedLaunch{}, check.refusal(), check.Why
	}
	fetched, err := c.fetchSession(ctx, row)
	if err != nil {
		return continuedLaunch{}, executorkeep.LossRestoreFailed, fmt.Sprintf("the session could not be fetched: %v", err)
	}
	body, err := fetched.open(ctx, c.restore)
	if err != nil {
		return continuedLaunch{}, executorkeep.LossRestoreFailed, fmt.Sprintf("the session could not be read again to carry it: %v", err)
	}
	carried := &CarriedSession{Archive: archive, Body: body, Replace: base.Interrupted}
	return continuedLaunch{brief: want.Replaced, fresh: req.Brief, session: base.SessionID,
		kind: BriefContinuedConversation, kept: true, carried: carried,
		measure: carriedMeasure(base.ContextTokens, MeasureBase, ""),
		release: func() { _ = body.Close() }}, "", ""
}

// notContinued is the verdict of a launch that starts a session of its own
// ([Coordinator.continueSession]), said once: its newest refusal that lost
// something, of the turn's own session or of the base its conversation left;
// else the caller's verdict, the run now fresh; else none, where nothing was
// lost. A launch that continues its box's own copy loses nothing: that copy
// went on past every capture of it.
func (c *Coordinator) notContinued(ctx context.Context, req LaunchRequest, opened PendingRun,
	refused []refusal, outcome string) *executorkeep.NotContinued {

	var verdict *executorkeep.NotContinued
	for _, r := range slices.Backward(refused) {
		if r.loss == "" {
			continue
		}
		verdict = executorkeep.Lost(r.loss, executorkeep.InsteadFresh, r.why)
		if want := req.Session; want != nil && want.Base && opened.Base != nil && opened.Base.Session != nil {
			verdict.LostTurn = opened.Base.Session.Turn
		}
		break
	}
	if verdict == nil && req.NotContinued != nil {
		fresh := *req.NotContinued
		fresh.Instead, fresh.OlderTurn, fresh.Older = executorkeep.InsteadFresh, "", nil
		verdict = &fresh
	}
	if verdict == nil {
		return nil
	}
	fitted := verdict.Fit()
	sayNotContinued(ctx, req.Turn.TurnID, req.Turn.AgentHandle, fitted, "outcome", outcome)
	return &fitted
}

// What a launch continues in place of a session it did not, as
// [Coordinator.notContinued] says it.
const (
	// continuesBoxCopy: the session the reused box holds, the job before's own
	// copy, which the run's script falls back from where its harness cannot
	// take it up.
	continuesBoxCopy = "the run continues the session the box it reuses holds, the job before's own copy, " +
		"handed the brief that continues it there, and starts a session of its own only where its harness " +
		"cannot take that copy up"
	// startsItsOwn: nothing to continue.
	startsItsOwn = "the run starts a session of its own, handed the brief written for one"
)

// fetchedSession is a kept session read whole once from the object store, its
// seal opened and the archive it holds digested, ready to be streamed into a
// box by a second read held to the first ([fetchedSession.open]).
type fetchedSession struct {
	row    executorkeep.TurnRow
	digest [sha256.Size]byte
	size   int64
}

// fetchSession reads row's session whole once and checks it opens, before any
// of it reaches a box, spooling nothing ([SessionRestore.Open]).
func (c *Coordinator) fetchSession(ctx context.Context, row executorkeep.TurnRow) (*fetchedSession, error) {
	body, err := c.restore.Open(ctx, row)
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	size, err := io.Copy(digest, body)
	if err = errors.Join(err, body.Close()); err != nil {
		return nil, err
	}
	fetched := &fetchedSession{row: row, size: size}
	digest.Sum(fetched.digest[:0])
	return fetched, nil
}

// open streams the session again, held to the read [Coordinator.fetchSession]
// made: its last byte is handed out only once every byte before it, and the
// end after it, match that read, so a box's write of it ends in an error, never
// a whole file, where the store answered otherwise. The caller closes it.
func (f *fetchedSession) open(ctx context.Context, restore SessionRestore) (io.ReadCloser, error) {
	body, err := restore.Open(ctx, f.row)
	if err != nil {
		return nil, err
	}
	return &matchedRead{body: body, want: f, digest: sha256.New()}, nil
}

// errReadAgainDiffers is a second read of a kept session that is not the one
// the first read found whole.
var errReadAgainDiffers = errors.New("sandbox: the kept session read again is not the one read whole")

// matchedRead is [fetchedSession.open]'s reader: n is how many bytes the body
// handed over, held the checked last byte, handed out once the whole matched;
// end and err are for good.
type matchedRead struct {
	body   io.ReadCloser
	want   *fetchedSession
	digest hash.Hash
	n      int64
	held   []byte
	end    bool
	err    error
}

func (m *matchedRead) Read(p []byte) (int, error) {
	switch {
	case len(m.held) > 0:
		n := copy(p, m.held)
		m.held = m.held[n:]
		return n, nil
	case m.err != nil:
		return 0, m.err
	case m.end:
		return 0, io.EOF
	case len(p) == 0:
		return 0, nil
	}
	if rest := m.want.size - m.n; rest > 1 {
		n, err := m.body.Read(p[:min(int64(len(p)), rest-1)])
		m.n += int64(n)
		_, _ = m.digest.Write(p[:n])
		switch {
		case errors.Is(err, io.EOF):
			m.err = fmt.Errorf("%w: it ended at %d of %d bytes", errReadAgainDiffers, m.n, m.want.size)
			return n, m.err
		case err != nil:
			m.err = err
			return n, err
		}
		return n, nil
	}
	if err := m.tail(); err != nil {
		m.err = err
		return 0, err
	}
	m.end = true
	return m.Read(p)
}

// tail reads what is left of the body and one byte past it, and checks the
// whole against the first read before any of it is handed out.
func (m *matchedRead) tail() error {
	left := m.want.size - m.n
	buf := make([]byte, left+1)
	n, end, err := objstore.Fill(m.body, buf)
	m.n += int64(n)
	switch {
	case err != nil:
		return err
	case !end || int64(n) != left:
		return fmt.Errorf("%w: it holds %d bytes or more, the first read %d", errReadAgainDiffers, m.n,
			m.want.size)
	}
	_, _ = m.digest.Write(buf[:n])
	if !bytes.Equal(m.digest.Sum(nil), m.want.digest[:]) {
		return fmt.Errorf("%w: its bytes differ", errReadAgainDiffers)
	}
	m.held = buf[:n]
	return nil
}

func (m *matchedRead) Close() error { return m.body.Close() }
