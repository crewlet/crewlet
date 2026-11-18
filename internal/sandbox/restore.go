package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
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
// `linearizable`, which waits for this node's copy, or asks a data node whose
// copy has it), as the sandbox's view of it ([executorkeep.TurnRow]); and the
// sealed object it names, fetched and opened through the domain's session
// store, or streamed opened for a reading of what the session holds
// (holdings.go). The row is the leaf's type because the sandbox links no state
// log: the log's own row type carries its positions, and so its graph.
type SessionRestore interface {
	// Session reads one turn's row (executordomain.Reader.Session, made the
	// sandbox's view by executordomain.Session.Kept), wrapping
	// [ErrTurnRowNotFound] where the log holds no row for the turn.
	Session(ctx context.Context, handle, turn string) (executorkeep.TurnRow, error)

	// Fetch copies the sealed object row names into dst, checked against the
	// row (executordomain.SessionStore.Fetch), and OpenSpooled opens what it
	// copied against the row's associated data
	// (executordomain.SessionStore.OpenSpooled).
	Fetch(ctx context.Context, row executorkeep.TurnRow, dst io.Writer) error
	OpenSpooled(row executorkeep.TurnRow, sealed io.Reader) (io.Reader, error)

	// Open streams the sealed object row names, opened as it streams
	// (executordomain.SessionStore.Open): checked against the row, and every
	// chunk authenticated against the row's associated data before a byte of
	// it is handed out. WHAT IT HANDS OUT IS PROVISIONAL UNTIL io.EOF, since
	// a stream cut short is known to be cut only at the read past its last
	// whole chunk: so it serves a reader that decides nothing until its
	// stream has ended ([Coordinator.HoldingsOf]), never a restore, which
	// writes what it reads into a box and so fetches the object first
	// ([Coordinator.fetchSession]). A row naming no object, an object the
	// store no longer holds and bytes not sealed for the row are unreadable,
	// at the call or from the Read that reaches the fault. The caller closes
	// what it answers.
	Open(ctx context.Context, row executorkeep.TurnRow) (io.ReadCloser, error)
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
		if namedThoughRefused(*run, row, follows) {
			// AN OLDER KEPT CAPTURE BESIDE A NEWER ONE THE LOG KEEPS: the
			// record keeps the older, the row names the newer, and neither
			// is continued, which "the log names another" would leave a
			// reader to guess.
			newest := *run.Capture
			return SessionContinuity{Why: fmt.Sprintf("the run's record stamps the newest capture of the job "+
				"the relaunch follows (launch %d, capture %d) %s; %s, nor the older capture of it the record "+
				"keeps (launch %d, capture %d), which the turn's row does not name", newest.LaunchSeq,
				newest.CaptureSeq, unkeptWhy(newest), keptThoughRefused, kept.LaunchSeq, kept.CaptureSeq)}
		}
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
// follows continues, or why there is none, in words for the log: the half of
// [ContinuityOf] the run's record answers alone. THE NEWEST KEPT, whatever a
// newer one of that job is (refused, unfinished, unanswered or still being
// kept): the continuation is told every call after its save point and that
// its run went on, so it counts none of the newer one's calls held. One that
// may still land is the relaunch's to wait for ([Coordinator.ContinuedCapture])
// and the launch's own check refuses a choice it moves
// ([Coordinator.continueKept]). A kept capture stating no save point
// ([SessionCapture.Through]) is not continued: an older build stamped it, and
// which calls came after it cannot be stated.
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
	case kept == nil || kept.LaunchSeq != follows:
		return SessionCapture{}, fmt.Sprintf("the newest capture of the job the relaunch follows (launch %d, "+
			"capture %d) was not kept: %s, and the job kept none before it", newest.LaunchSeq,
			newest.CaptureSeq, unkeptWhy(*newest))
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
// the newest capture of the job at follows where run's record stamps it
// REFUSED: a record its taker handed on before its lease's window closed, which
// the log took only after another node's settle read the row (the package doc,
// "WHAT THE FENCE CANNOT REACH"). An unanswered one is no such stamp: its ask
// stamps it landed. The log keeps it, and no relaunch of that job continues it
// or an older capture beside it: the record keeps no place it holds refused
// ([stampOnto]), and ADR-0073 records no refusal on the log, where the applier
// would refuse the late capture and lose a session. A decision told with the
// row in hand says the log keeps it, never that it was not kept.
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
	// ([ContinuityOf] says it continues), though in the box its job parked
	// in, where that capture was taken while the job still worked, as the
	// copy that box holds, which went on past it ([copyWentPast]); or an
	// ended turn's that is its conversation's head, by the conversation's
	// next turn, restored into a box of that turn's own ([BaseContinuityOf]
	// says it continues).
	ContinuesInAnyBox ContinuesIn = "any_box"

	// ContinuesInParkedBox is the session the box the turn's job parked in
	// holds, continued IN THAT BOX AND NOWHERE ELSE: what a person's answer
	// carries a parked run on in where the turn's kept capture is not
	// continued (no capture of the job kept, or the kept one of a release no
	// other box continues) for as long as the run's box is held. A pause past its
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
//     (executordomain.RelaunchRead) wherever the decision reaches it
//     ([RereadsRow]): a copy of the log behind the run's record reads a capture
//     the relaunch would continue as one the log no longer names.
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
// that job, or a save under way beside it, still being kept, which a relaunch
// waits for ([relaunchCaptureWait]), or its record unanswered, which a relaunch
// asks again ([captureAwaitsAnswer]); an earlier job's capture still being kept
// while the turn has kept none ([relaunchDependsOn]); what the launch would be
// launched on not to be said now ([RelaunchTarget.Undecided]); a relaunch that
// only the resume of the job's collection decides, as it reads how the job
// ended, until its turn begins and says what it does with the round
// ([LaunchRecord.ResumeCarry]) — whether a round lost a second time is carried
// on in a session of its own or reviewed from the work it submitted, and
// whether a box whose sign-in was replaced carries its round on; and an ended
// row whose conversation's head the read did not name beside it. Once the turn
// has said, the row is decided on what it said, its review included. now is the
// reader's clock, against which a begun capture is in flight
// ([relaunchWaitsOn]) and an unanswered one's object within the window a record
// may name it in as any node's clock may read it ([pastRecordWithin]).
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
// session, naming the turn's base where a relaunch would continue that instead.
// UNDECIDED while a capture or save the relaunch waits for is still being kept
// ([relaunchWaitsOn]) or its record awaits its answer ([captureAwaitsAnswer]),
// each named by its own place, beside an older kept capture as beside none:
// the relaunch waits for the one and asks again for the other, and continues
// whichever lands (not where the company keeps no session,
// [RelaunchTarget.NoKept]). A newer one that never will land leaves the
// decision to [ContinuityOf], which continues the older.
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
// reads it (executordomain.RelaunchRead) must read it again there before
// deciding: only where the decision reaches the row (the turn live, not ending,
// nothing it waits on in flight or awaiting an answer) and the row names
// another capture than run's record keeps, or the record keeps none, or holds a
// newer capture of the followed job it does not keep ([unkeptNewer]), which the
// log may have taken unseen by its stamp. A row naming the kept capture beside
// nothing newer decides as a read at that level would, since a newer capture
// is stamped before a byte of it moves; read again there too, every look at a
// seat's sessions would append a barrier to the log for each live row.
func RereadsRow(row executorkeep.TurnRow, run *PendingRun, now time.Time) bool {
	if run == nil || row.Ended || run.endDecided() || jobWorking(*run) || relaunchWaitsOn(*run, now) != nil ||
		captureAwaitsAnswer(*run, now) != nil {
		return false
	}
	kept := run.Kept
	return kept == nil || row.LaunchSeq != kept.LaunchSeq || row.CaptureSeq != kept.CaptureSeq ||
		unkeptNewer(*run)
}

// unkeptNewer reports whether run's record holds a capture of the job a
// relaunch would follow newer than the one it keeps, in any state: its newest
// capture, else its save under way ([PendingRun.savingTake]). One the record
// keeps is no such capture, and a reader that holds a row naming it reads it
// again only for one of these ([RereadsRow]).
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

// captureAwaitsAnswer is the stamp of the job a relaunch would follow whose
// record was sent and never answered and may still land
// ([SessionCapture.unresolvedWithin]): run's newest capture, else its save
// under way ([PendingRun.saving]), which beside a refused final take is the
// job's newest session once its ask lands it; nil where neither is. A
// relaunch asks it again before it decides ([Coordinator.settleUnresolved]),
// so until then no reader can say what that relaunch continues. An earlier
// job's stamp is no such question: a relaunch continues only a capture of the
// job it follows.
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
// ([relaunchDependsOn]), answered as itself so that a reason names it by its
// own place. Only those, since the waiting frame holds the seat and an
// admission slot, and a keep that changes nothing the relaunch decides buys
// nothing.
func relaunchWaitsOn(run PendingRun, now time.Time) *SessionCapture {
	return stampInFlight(run, now, func(stamp SessionCapture) bool { return relaunchDependsOn(run, stamp) })
}

// relaunchDependsOn reports whether what a relaunch of run's turn continues
// can turn on whether stamp lands: a stamp of the job the relaunch follows
// (run.LaunchSeq), whose capture is the one it continues; or a stamp of any
// job while the turn has kept nothing, since once a capture of the turn is
// kept a relaunch no longer continues the session the turn's first launch was
// handed ([frozenBase]). An earlier job's stamp beside a capture the turn kept
// is neither: a relaunch continues only a capture of the job it follows
// ([followedCapture]), and the turn has already kept one, so its landing
// changes no relaunch's decision.
func relaunchDependsOn(run PendingRun, stamp SessionCapture) bool {
	return stamp.LaunchSeq == run.LaunchSeq || run.Kept == nil
}

// relaunchWaits is [relaunchWaitsOn] as the rule a relaunch's wait reads
// ([Coordinator.awaitCapture]): whether it still waits.
func relaunchWaits(run PendingRun, now time.Time) bool { return relaunchWaitsOn(run, now) != nil }

// followedBegunUnderTaker reports whether run has a stamp of the job its
// relaunch follows still begun under a taker it names: its newest capture, or
// its save under way ([PendingRun.savingTake]), still being kept or not. A
// relaunch reads the turn's row once past its wait for such a stamp
// ([Coordinator.ContinuedCapture]), WHATEVER ITS AGE: a host whose
// coordination store answers its leases and not its runs holds its taker
// lease for as long as that silence lasts, and the job lost with it is found
// only once the silence has lasted the box's timeout, so a keep under way
// when the silence began can be past a capture's budget by the time its
// relaunch reads it, and the log's all the same. A stamp that names no taker
// is read by its age alone, as [beganUnderTaker] has a wait read it.
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
	// Capture is the kept capture continued, as the run's record stamps it:
	// its own ContextTokens is what a launch that restores it sends first,
	// which the floor of that launch is made of, never
	// [ContinuedSession.CopyContext].
	Capture SessionCapture
	Row     executorkeep.TurnRow

	// Base is the session the conversation's last turn left, which the
	// relaunch continues in place of a kept capture of the turn's own; nil
	// where it continues none, or a kept capture.
	Base *BaseSession

	// CopyContext is what the last request of the session of the job the
	// relaunch follows carried, as that job's newest capture measured it,
	// KEPT OR NOT: what a launch that continues that job's own copy in its
	// box sends again before its brief ([LaunchRequest.Continue]), the copy
	// having gone as far as that capture. Set whether or not a capture is
	// continued; zero where no capture of the job measured any. A kept
	// capture continued is held to its own measure (Capture.ContextTokens).
	CopyContext int64

	// CopyNextRequest is the harness's own estimate of that session's next
	// request, as the same newest capture's take read it, kept or not
	// ([SessionCapture.NextRequest]): what that launch is held to with its
	// brief where it holds a figure, CopyContext where it holds none. Set
	// beside CopyContext. A kept capture continued is held to its own
	// (Capture.NextRequest), never this one, a passed-over newer capture's.
	CopyNextRequest NextRequest

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

// relaunchCaptureWait bounds how long a relaunch waits, on the frame that
// relaunches, for a stamp its decision depends on ([relaunchWaitsOn]); past it
// the relaunch continues the newest capture its job kept before it, if any.
// FIFTEEN SECONDS: an ordinary keep (a few coordination reads, a mebibyte or
// two of upload and a record the log answers within the publisher's
// five-second wait, statelog.DefaultResolveBudget) three times over; a keep
// past it is one a slow upload, an outage or a stopped node holds, whose life
// runs past an hour ([captureLife]). As long again bounds the one read past it
// ([Coordinator.ContinuedCapture]), a lease read and a log read each answered
// in milliseconds; its settle has its own room ([Coordinator.settleContext]).
const relaunchCaptureWait = 15 * time.Second

// ContinuedCapture is the session a relaunch of turnID continues: the newest
// capture the followed job kept, as [ContinuityOf] decides for restorer, once
// a newer one is no longer being kept or [relaunchCaptureWait] has run out
// (passed over and logged, [Coordinator.passedOver]); else, where the turn
// has kept none, its frozen base ([Coordinator.continuedWithout]); or why
// none, and what that job's copy last carried and is estimated to send next.
// Within the wait, what is unsettled is settled first: a gone taker's capture
// from the log, an unanswered one asked again, a save as the newest; past it,
// one read of its own settles landed a held taker's capture the log names.
// Settles are this node's own writes in their own room past the wait. Asked
// before the run's surface and briefs are built; never an error, never a read
// of the conversation's head. The package doc's "The job before's capture".
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
	// ONE WAIT FOR ALL OF IT, on the frame that relaunches: the asks of an
	// unanswered record and the wait for a keep under way share it. The
	// settles it decides are this node's own writes, each in its own room
	// past it.
	settleBy := time.Now().Add(c.relaunchWait)
	// THE FOLLOWED JOB'S UNANSWERED RECORDS ASKED FIRST, whatever else is in
	// flight: past the wait, the relaunch continues the newest capture that
	// job kept beside a newer one still being kept, so an ask that lands a
	// record (a save's, behind a final take still being kept) is what gives
	// it the newer of the two to continue, and asked only once the wait ran
	// out it would have no time left.
	run = c.askFollowed(ctx, settleBy, run)
	run, found, err = c.awaitCapture(ctx, run, time.Until(settleBy), relaunchWaits)
	if found && err == nil && followedBegunUnderTaker(run) {
		// THE ONE READ PAST THE WAIT, for a capture of the followed job still
		// begun under a taker it names whose lease holds or cannot be read:
		// the log may hold it although no stamp of it reached the row, as
		// where its taker's host answers its leases and not its runs. Settled
		// landed where the turn's row names its place, it is continued.
		// Held to as long again as the relaunch's wait; its settle has its
		// own room ([Coordinator.settleContext]).
		held, cancel := context.WithTimeout(ctx, c.relaunchWait)
		run, found, _ = c.settleGone(held, run, askEvery, true)
		cancel()
	}
	if !found {
		return ContinuedSession{}, "the turn's run record was gone before the capture of the job " +
			"the relaunch follows settled"
	}
	answer := ContinuedSession{CopyContext: copyContext(run), CopyNextRequest: copyNextRequest(run)}
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
	if time.Now().Before(settleBy) {
		// AND AGAIN WHAT THE WAIT LEFT UNANSWERED, where it ended early: a keep
		// whose record went unanswered while the relaunch waited for it.
		run = c.askFollowed(ctx, settleBy, run)
	}
	var why string
	waits := relaunchWaitsOn(run, c.now())
	switch keptOfJob := run.Kept != nil && run.Kept.LaunchSeq == run.LaunchSeq; {
	case waits != nil && waits.LaunchSeq != run.LaunchSeq:
		// AN EARLIER JOB'S, waited on only while the turn has kept nothing:
		// the job the relaunch follows kept nothing either, and what it
		// continues instead turns on that capture's landing.
		_, why = followedCapture(run, run.LaunchSeq)
		why += fmt.Sprintf("; a capture of an earlier job of the turn (launch %d, capture %d) was still "+
			"being kept %s after the relaunch asked for it, which the relaunch does not wait out",
			waits.LaunchSeq, waits.CaptureSeq, c.relaunchWait)
	case waits != nil && !keptOfJob:
		// THE FOLLOWED JOB'S, beside nothing that job kept: a keep still under
		// way is no kept capture.
		why = fmt.Sprintf("the capture of the job the relaunch follows (launch %d, capture %d) "+
			"was still being kept %s after the relaunch asked for it, which the relaunch does not wait out",
			waits.LaunchSeq, waits.CaptureSeq, c.relaunchWait)
	default:
		// THE NEWEST CAPTURE THAT JOB KEPT, past a newer one of it that will
		// not be kept, or that the wait has run out on ([followedCapture]).
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
	c.passedOver(ctx, run, *run.Kept)
	answer.Capture, answer.Row = *run.Kept, row
	return answer, ""
}

// askFollowed asks again, never past by, the unanswered records of the job
// run's relaunch follows ([Coordinator.settleUnresolved]): its newest
// capture's and a save's beside it, whatever its key's age, the log deciding
// whether the window a record may name it in has passed. The stamp of what an
// ask settled is this node's own write, in its own room past by
// ([Coordinator.settleContext]).
func (c *Coordinator) askFollowed(ctx context.Context, by time.Time, run PendingRun) PendingRun {
	follows := run.LaunchSeq
	ask, cancel := context.WithDeadline(ctx, by)
	defer cancel()
	return c.settleUnresolved(ask, run, func(stamp SessionCapture) bool { return stamp.LaunchSeq == follows })
}

// passedOver logs each capture of the job run's relaunch follows that is
// newer than kept, the capture the relaunch continues: its newest capture,
// and its save under way where that is another take
// (`executor_session_not_continued`, once per decision). Each is named as one
// that was not kept and why, or as one that may still be kept and how, which
// the relaunch did not wait out. Without it, a relaunch from an older kept
// capture said nothing of the newer one it rolled the session back past.
func (c *Coordinator) passedOver(ctx context.Context, run PendingRun, kept SessionCapture) {
	now := c.now()
	var passed []string
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
				c.relaunchWait)
		} else {
			named += "was not kept: " + unkeptWhy(*newer)
		}
		passed = append(passed, named)
	}
	if len(passed) == 0 {
		return
	}
	log.InfoContext(ctx, "executor_session_not_continued", "turn_id", run.TurnID, "agent", run.AgentHandle,
		"detail", redact.Secrets(strings.Join(passed, "; ")),
		"outcome", fmt.Sprintf("the relaunch continues the newest capture that job kept (launch %d, capture "+
			"%d), and is told what came after it", kept.LaunchSeq, kept.CaptureSeq))
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
// no kept capture (one still being kept past [relaunchCaptureWait] with none
// before it, or one not kept): the base the turn froze while it has kept
// nothing and its row still holds it ([frozenBase]), since the base holds all
// a session of its own would and the conversation besides; else nothing, why
// saying why not. The turn's row is read once, only where something asks it:
// the base, and a refused newest capture the row may name all the same
// ([namedThoughRefused]), whose reason then says the log keeps it. What the row
// names is what the run is told it may read ([ContinuedSession.KeptTurn]).
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

// copyContext is what the last request of the session of run's current job
// carried, as that job's newest capture measured it, kept or not: the measure
// of that job's own copy in its box, which went at least as far
// ([ContinuedSession.CopyContext]); zero where it left none, or none that
// measured any.
func copyContext(run PendingRun) int64 {
	if newest := run.Capture; newest != nil && newest.LaunchSeq == run.LaunchSeq {
		return newest.ContextTokens
	}
	return 0
}

// copyNextRequest is the harness's own estimate of the next request of the
// session of run's current job, as that job's newest capture's take read it,
// kept or not ([copyContext]); zero where it left none, or none that
// estimated one.
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
	// newest capture the job this launch follows kept, or none. A newer one
	// of that job still being kept does not count against it until it lands
	// ([Coordinator.continueKept]).
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

	// Exact is the briefs told by a reading of what the kept capture holds
	// ([HoldingsAsk]), beside Brief and Replaced, which tell by the save
	// point; nil where no reading was read, or where its telling is not to
	// be handed. Only for a kept capture, never for Base. The launch hands
	// one of them only to a box whose harness answers the release the
	// reading was read for ([ExactTelling.Resumer]), and Brief or Replaced
	// to every other box ([Coordinator.continueKept]).
	Exact *ExactTelling

	// Summarized is the briefs told as a session that may be a summary,
	// beside Brief and Replaced, where the session's first request is the
	// launch's to judge in its box (an OpenCode session whose count has a
	// bound): handed where the box's point does not prove the brief the
	// launch would hand sends the session whole ([OpenCodePoint.SendsWhole]).
	// Nil where the runner judged the first request itself. Count and
	// CountProvider are what that is judged by: the count the runner judged
	// the kept path by, and the provider of the reply it is anchored on.
	Summarized    *SummarizedTelling
	Count         *ResumedCount
	CountProvider string

	// Unheld is the briefs for a box whose harness answers a release whose
	// resume this build did not read ([CarryCheck.ResumeRead]), beside Brief
	// and Replaced; nil where they would tell the same. Only for a kept
	// capture. The launch hands one of them only to such a box.
	Unheld *UnheldTelling

	// UnheldDropped says the seat's budget could not carry Unheld's briefs,
	// which the engine dropped alone (internal/engine's continuedFloor): the
	// session is then continued only in a box whose release the resume was
	// read in, and any other starts a session of its own.
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

	// Told is what Brief and Replaced told of the round ([BriefTold]):
	// recorded on the job the launch opens in place of
	// [SessionContinuation.Told] where it hands either.
	Told BriefTold
}

// ExactTelling is a relaunch's briefs told by a reading of what the session it
// continues holds ([Coordinator.HoldingsOf]): told only the calls of the round
// the reading does not find the session holding, where the briefs beside them
// ([SessionContinuation.Brief], [SessionContinuation.Replaced]) tell every
// call after the session's save point.
//
// HANDED ONLY TO A BOX OF THE RELEASE IT WAS READ FOR. What a session holds is
// what the resuming release's resume hands its model ([HoldingsAsk.Resumer]),
// and the relaunch's box is acquired inside the launch, after its briefs are
// written: so the reading is read for the release that wrote the session, the
// relaunch's best word for the release that resumes it, and the launch hands
// these briefs only where the box's harness, asked before its brief is chosen
// ([SessionRestorer.CheckCarry]), answers exactly that release. A box of any
// other release, or one whose answer this build cannot read, is handed the
// briefs told by the save point, which tell more and never less.
type ExactTelling struct {
	// Resumer is the release the reading was read for ([HoldingsAsk.Resumer]),
	// "2.1.310": the one a box's harness must answer for Brief or Replaced to
	// be handed to it. "" matches no box.
	Resumer string

	// Brief is the run's prompt where the box is the one the session was
	// last worked in, and Replaced where it is another, as
	// [SessionContinuation]'s are; "" where the telling wrote none for that
	// box, which hands the brief told by the save point there.
	Brief, Replaced string

	// Told is what Brief and Replaced told of the round ([BriefTold]):
	// recorded on the job the launch opens in place of
	// [SessionContinuation.Told] where it hands either.
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

	// Exact says the brief the launch hands is one told by a reading of what
	// the session holds ([SessionContinuation.Exact]): its box's harness
	// answered the release the reading was read for. Unheld says it is one
	// for a box whose release the resume was not read in
	// ([SessionContinuation.Unheld]). Both false where it hands the brief
	// told by the save point, or continues no kept session.
	Exact, Unheld bool

	// Summarized says the brief handed is the one told as a session that may
	// be a summary ([SessionContinuation.Summarized],
	// [Continuation.Summarized]): the box's point did not prove the session
	// reaches its first request whole, so its guard arms nothing.
	Summarized bool
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
	// exact says brief is the one told by a reading of what the kept
	// capture holds ([SessionContinuation.Exact], [LaunchDecision.Exact]),
	// and unheld that it is one for a box whose release the resume was not
	// read in ([SessionContinuation.Unheld]), so told is that telling's own.
	exact, unheld bool
	// summarized says brief is the one told as a session that may be a
	// summary ([LaunchDecision.Summarized]), so told is that telling's own.
	summarized bool
	// measure is what the session carried at its newest request the engine
	// measured, along its lineage, saying where it came from: the job
	// before's record of its own copy, where the launch continues that copy
	// in place; the kept capture's own measure of its archive, its job named
	// as the archive's; the frozen base's, for the session the conversation's
	// last turn left; and none for a session of the run's own. Written on the
	// job's record with its command ([BoxRef.Context]), so a job that makes no
	// request of its own is measured by the copy it took up, and never by a
	// copy of another lineage.
	measure *SessionMeasure
	// restart is the newest place the session restarts at, as the launch
	// knows it: the job before's collection's read of its own copy, where the
	// launch continues that copy in place; the kept capture's own read of its
	// archive; a session of the run's own, which restarts nowhere ("");
	// nil where it is not known, the session the conversation's last turn
	// left among it, which no count describes. Written on the job's record
	// with its command ([BoxRef.Restart]): what a later relaunch compares the
	// job's own copy with, to know whether the job restarted it after its
	// brief ([RestartedSince]).
	restart *string
	// point is the save point of the kept capture the launch hands its run,
	// with that capture's launch, where the launch carries its round on and
	// the capture states one above zero; nil otherwise ([LaunchRecord.Continued]).
	// Written on the job's record with its command ([BoxRef.Continued]).
	point *ContinuedPoint
	// release removes what the launch spooled to carry the session in, once
	// the run has started (or failed to).
	release func()
}

// carriedMeasure is a figure a launch carries along its session's lineage from
// source, of launch's job where source names one: none where it is no count of
// a session's context, as a capture that measured nothing records 0.
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
// round, whose log restarts, so a set of the collected round's calls would be
// read against another round's log. A closed round's reads past the read cap
// are counted under the prior-work ledger's rule for them.
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
// of the round it carries on where it carries one on, and only the leads it
// opened with where it does not (a turn's first launch, a send-back), whose
// request names no continuation to read them off ([LaunchRequest.FreshOwn]).
func freshLaunch(req LaunchRequest) continuedLaunch {
	// A SESSION OF THE RUN'S OWN restarts nowhere yet: its basis is known, and
	// is none.
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
// context in it; else a session of its own. Each session not continued is
// logged once the one it falls to is decided ([Coordinator.notContinued]),
// naming it, and a reading of a capture passed over for the copy is said
// unused ([Coordinator.exactUnused]).
func (c *Coordinator) continueSession(ctx context.Context, req LaunchRequest, opened PendingRun,
	box Sandbox, runner Runner, reused, begun bool, sources RunSources) continuedLaunch {

	var (
		refused []string
		// readPassedOver says the kept capture passed over for the box's
		// copy ([copyWentPast]) came with a brief told by a reading of it.
		readPassedOver bool
	)
	if want := req.Session; want != nil {
		var (
			launch continuedLaunch
			why    string
		)
		if want.Base {
			launch, why = c.continueBase(ctx, req, opened, box, runner, reused, begun, *want)
		} else if why = copyWentPast(req, opened, reused, *want); why == "" {
			launch, why = c.continueKept(ctx, req, opened, box, runner, reused, *want, sources)
		} else {
			readPassedOver = want.Exact != nil
		}
		if why == "" {
			// WHAT ITS BRIEFS TOLD, what they left out of the round and what
			// they opened with, the same for the brief for either box
			// ([SessionContinuation.Told]); and where the brief handed is
			// one told by a reading, one told as a session that may be a
			// summary, or one for a box whose release the resume was not read
			// in, that telling's own ([ExactTelling.Told],
			// [SummarizedTelling.Told], [UnheldTelling.Told]). Chosen here, the
			// one assignment, so every continuation records what the brief it
			// handed left out, the box's own copy among them.
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
			return launch
		}
		refused = append(refused, why)
	}
	launch, outcome := freshLaunch(req), startsItsOwn
	if continued := req.Continue; continued != nil && reused && continued.SessionID != "" {
		if opened.sessionExhausted(continued.SessionID) {
			refused = append(refused, executorkeep.ExhaustedWhy(continued.SessionID, req.Turn.TurnID))
		} else {
			// THE COPY THE BOX HOLDS, measured as the job before's record
			// holds it, read by the reset that opened this job in the write
			// that replaced that record ([PriorBox.Context]).
			launch = continuedLaunch{brief: continued.Brief, fresh: req.Brief, session: continued.SessionID,
				kind: BriefContinued, told: continued.Told, measure: opened.Prior.Context,
				restart: opened.Prior.Restart, release: func() {}}
			// AN OPENCODE COPY'S FIRST REQUEST, judged in this box where its
			// caller handed the summarized telling beside the held one (the
			// package doc's "first request is judged in its box"): sources
			// are charged, the box holding the copy, and the point paired
			// with the release the job before's record names as the copy's
			// writer, whether or not the company keeps captures
			// ([PriorBox.Writer]).
			if continued.Summarized != "" {
				judge := &boxJudge{path: "in_place", count: continued.Count, provider: continued.CountProvider,
					ask: PointAsk{Writer: opened.Prior.Writer, Sources: &sources}}
				if !c.sendsWhole(ctx, req, box, runner, judge, continued.Brief) {
					launch.brief, launch.told, launch.summarized = continued.Summarized, continued.SummarizedTold, true
					c.mayCompact(ctx, req, judge, continued.Brief)
				}
			}
			outcome = continuesBoxCopy
		}
	}
	for _, why := range refused {
		c.notContinued(ctx, req, why, outcome)
	}
	if readPassedOver && outcome == continuesBoxCopy {
		c.exactUnused(ctx, req, copyWentPastUnused)
	}
	return launch
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
// session, the copy is continued instead. A final capture is restored as ever;
// finality is the fact read, never age or place. A capture the record does not
// keep is [Coordinator.continueKept]'s to refuse, and a launch offered no copy
// tries the capture whatever session its stamp names.
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
// cannot. THE BOX THE CAPTURE WAS TAKEN IN IS THE REUSED ONE: the capture is
// the job before's ([ContinuityOf]), and a reused box is that job's
// ([PriorBox.ContinuesIn]), so the same-box brief is handed exactly where the
// session was last worked in, and the new-box brief everywhere else.
//
// WHICH TELLING, CHOSEN ONCE THE BOX'S HARNESS HAS SAID ITS RELEASE
// ([SessionRestorer.CheckCarry]): the one told by a reading only where that
// answer is exactly the release it was read for ([exactFor]), else logged
// (`executor_session_holdings_unused`); the unheld one where it is a release
// whose resume this build did not read ([UnheldTelling]), and no session there
// where the seat's budget dropped it ([SessionContinuation.UnheldDropped]). The
// box's own copy, read by no reading ([Coordinator.HoldingsOf]), is asked its
// release only where an unheld telling is at stake: the one that wrote it, as
// the executor's DISABLE_AUTOUPDATER pins it.
func (c *Coordinator) continueKept(ctx context.Context, req LaunchRequest, opened PendingRun,
	box Sandbox, runner Runner, reused bool, want SessionContinuation, sources RunSources) (continuedLaunch, string) {

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
	// job one past the job it follows: a capture of that job that LANDED
	// since its caller asked, kept on the record (refused above) or named by
	// the turn's row ([ContinuityOf]), refuses the chosen capture, whose
	// briefs were built for it; the launch then starts a session of its own,
	// or continues its box's copy, never the newer capture. One that only
	// began since, or is still being kept, does not: the chosen capture is
	// still the newest that job kept.
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
	// MEASURED AS THE CAPTURE MEASURED ITS ARCHIVE, the capture's job named as
	// the archive's, never as that job's request.
	measure := carriedMeasure(kept.ContextTokens, MeasureCapture, kept.Launch)
	// AND WHERE THE ARCHIVE RESTARTS LAST, as the capture's own read of it
	// found it.
	restart := restartOf(kept.ResumedCount)
	// AND ITS SAVE POINT, for a later capture of the job to be raised to: only
	// where the launch carries its round on, since a round the launch opens
	// numbers its calls from a new log ([LaunchRecord.Continued]).
	point := continuedPoint(req, *kept)
	// THE BOX'S OWN COPY, where its harness exited cleanly in this very box
	// and its session was taken whole there: the box holds the archive, so
	// nothing is fetched to be kept over a copy it would not touch.
	own := reused && !kept.Interrupted
	archive := ArchiveOf(row)
	// WHERE ITS FIRST REQUEST IS THIS LAUNCH'S TO JUDGE (an OpenCode session
	// handed with its summarized telling), the box's point, read once on
	// every branch, after CheckCarry where that runs, with what the box hands
	// over first where it is the one the capture was taken in: the brief
	// told by a reading where it sends whole, else the held brief where it
	// does, else the summarized one.
	var judge *boxJudge
	if want.Summarized != nil {
		judge = &boxJudge{path: "kept", count: want.Count, provider: want.CountProvider,
			ask: PointAsk{Writer: archive.Version}}
		if reused {
			judge.ask.Sources = &sources
		}
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
			return continuedLaunch{}, fmt.Sprintf("the box could not be asked whether its harness continues "+
				"the session: %v", err)
		case !own && !check.Continues:
			return continuedLaunch{}, check.Why
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
		// A BOX WHOSE RELEASE THE RESUME WAS NOT READ IN holds no read at a
		// hook's step as the job's session showed it ([UnheldTelling]).
		brief = want.Unheld.Replaced
		if reused {
			brief = want.Unheld.Brief
		}
	case want.UnheldDropped && !check.ResumeRead:
		return continuedLaunch{}, fmt.Sprintf("the seat's budget could not carry the briefs for a box whose "+
			"release the resume was not read in, and this box's harness answers %s", answeredRelease(check))
	default:
		var why string
		if brief, summarized, why = held(brief); why != "" {
			return continuedLaunch{}, why
		}
	}
	if own {
		return continuedLaunch{brief: brief, fresh: req.Brief, session: row.SessionID, kind: kind,
			kept: true, unheld: unheld, summarized: summarized, measure: measure, restart: restart,
			point: point, release: func() {}}, ""
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
	if unused != "" {
		c.exactUnused(ctx, req, unused)
	}
	// THE RESTORE CARRIER, chosen here and nowhere else: the turn's own kept
	// session, of the seat whose run this is — read off the turn's own row,
	// the capture of the job this launch follows, and opened against that
	// row's associated data, so no other seat's or turn's session opens into
	// the run — which replaces the box's copy where its harness was cut off.
	carried := &CarriedSession{Archive: archive, Body: body, Replace: kept.Interrupted}
	return continuedLaunch{brief: brief, fresh: req.Brief, session: row.SessionID, kind: kind,
		kept: true, exact: exact != "", unheld: unheld, summarized: summarized, carried: carried,
		measure: measure, restart: restart, point: point, release: func() {
			_ = body.Close()
			fetched.remove()
		}}, ""
}

// continuedPoint is the save point of kept, the capture launch req hands its
// run, with kept's launch ordinal ([LaunchRecord.Continued]): nil where req
// carries no round on, the rule [continuedLaunch.omitted] reads, and where kept
// states no point above zero.
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
// "" where it hands none, and then why, "" too where it was given no such
// telling ([SessionContinuation.Exact]).
//
// EXACTLY THE RELEASE THE READING WAS READ FOR, and a release read: an
// unread answer ("", which [CarryCheck.Version] takes for a current release)
// matches nothing, since what a session holds is what the resuming release's
// resume hands its model ([HoldingsAsk.Resumer]), and a box of another release
// may hand it otherwise. Equality, harness-neutral, rather than the reader's
// set of releases: a box of another release the reader also read costs the
// telling alone, never a call counted held.
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
// brief other than the one told by a reading of what its kept capture holds:
// the kept capture, told by its save point ([exactFor]), or the copy its job's
// own box holds, which went on past that capture, told by the brief that
// continues the copy there ([copyWentPast]). Keyed as
// [Coordinator.notContinued]'s line. The one place the launch says so.
func (c *Coordinator) exactUnused(ctx context.Context, req LaunchRequest, why string) {
	log.InfoContext(ctx, "executor_session_holdings_unused", "turn_id", req.Turn.TurnID,
		"agent", req.Turn.AgentHandle, "detail", redact.Secrets(why), "outcome", exactUnusedOutcome)
}

// exactUnusedOutcome is what a launch that does not hand the brief told by a
// reading hands instead ([Coordinator.exactUnused]).
const exactUnusedOutcome = "the relaunch is handed the telling without the reading"

// exactMayCompact is why a launch does not hand an OpenCode session the brief
// told by a reading where the box's point does not prove that brief sends the
// session whole ([Coordinator.exactUnused]).
const exactMayCompact = "its harness may compact the session in this box before it reads the brief told by the " +
	"reading"

// boxJudge is how a launch judges, in its box, whether an OpenCode session it
// continues reaches its first request whole ([OpenCodePoint.SendsWhole]): the
// path (`kept` or `in_place`), the count it is judged by and the provider of
// its anchoring reply, the ask, and the point once read, read at most once.
type boxJudge struct {
	path     string
	count    *ResumedCount
	provider string
	ask      PointAsk
	read     bool
	point    OpenCodePoint
}

// sendsWhole reports whether the session j judges reaches its first request
// whole with brief, the box's point read on its first ask ([PointReader]): no
// point, a runner that reads none and a box that could not be asked among
// them, sends nothing whole.
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

// readPoint is box's OpenCode point as runner reads it ([PointReader]), no
// point where it reads none or the box could not be asked.
func readPoint(ctx context.Context, box Sandbox, runner Runner, ask PointAsk) OpenCodePoint {
	reader, ok := runner.(PointReader)
	if !ok {
		return OpenCodePoint{Mark: PointUnread, Why: "the box's runner reads no OpenCode point"}
	}
	point, err := reader.ReadOpenCodePoint(ctx, box, ask)
	if err != nil {
		return OpenCodePoint{Mark: PointUnread,
			Why: redact.Secrets("the box could not be asked for its OpenCode point: " + err.Error())}
	}
	return point
}

// mayCompact logs that a launch hands the session j judged the brief told as
// one that may be a summary, the box's point not proving brief, the one it
// would otherwise hand, sends it whole: `executor_session_may_compact`, the
// launch's form of the runner's line, keyed as [Coordinator.notContinued]'s.
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
		"box_tokens", OpenCodeCharge(j.point.BoxBytes), "brief_tokens", OpenCodeCharge(int64(len(brief))),
		"outcome", mayCompactOutcome)
}

// mayCompactOutcome is what a launch whose box may compact the session before
// its first request hands instead of the held telling ([Coordinator.mayCompact]).
const mayCompactOutcome = "the continuation is told as one that may be a summary, and its guard arms no skill"

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
		measure: carriedMeasure(base.ContextTokens, MeasureBase, ""),
		release: func() {
			_ = body.Close()
			fetched.remove()
		}}, ""
}

// notContinued logs why a launch did not continue a session it was offered,
// and outcome, what it continues instead ([Coordinator.continueSession]).
func (c *Coordinator) notContinued(ctx context.Context, req LaunchRequest, why, outcome string) {
	log.InfoContext(ctx, "executor_session_not_continued", "turn_id", req.Turn.TurnID,
		"agent", req.Turn.AgentHandle, "detail", redact.Secrets(why), "outcome", outcome)
}

// What a launch continues in place of a session it did not, as
// [Coordinator.notContinued] says it.
const (
	// continuesBoxCopy: the session the box it reuses holds, the job
	// before's own copy, which the run's script falls back from to a
	// session of its own where its harness cannot take it up.
	continuesBoxCopy = "the run continues the session the box it reuses holds, the job before's own copy, " +
		"handed the brief that continues it there, and starts a session of its own only where its harness " +
		"cannot take that copy up"
	// startsItsOwn: nothing to continue.
	startsItsOwn = "the run starts a session of its own, handed the brief written for one"
)

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
// before any of it reaches a box (executordomain.SessionStore.Fetch) — in the
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
