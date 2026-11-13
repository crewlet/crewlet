package sandbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
)

// WHICH OF A ROUND'S CALLS A CUT-OFF SESSION HOLDS, READ OFF ITS KEPT ARCHIVE.
//
// A relaunch that continues a kept capture whose harness was cut off
// ([SessionCapture.Interrupted]) tells the session every call the run's log
// took after the capture's save point, and a cut-off capture's save point is
// the newest its launch held rather than where its session ends
// ([SessionCapture.Through]): so that list repeats calls the session holds,
// and is headed as calls it may not show. Yet its harness names each call it
// makes ([BridgeCall.CallID]) and files the call's use and its result under
// that name in the session it keeps, so the capture's own archive can say
// which of those calls the session holds the logged result of.
// [Coordinator.HoldingsOf] is that reading, for the relaunch's telling to ask
// before it writes the brief, and to tell the session exactly the calls it
// lacks.
//
// FACTS, AND NEVER A REFUSAL. A reading answers what the archive holds of each
// call it was asked about ([SessionHoldings]), and one rule turns those facts
// into a verdict ([SessionHoldings.Holds]). Every failure, of the record, the
// row, the store or the archive, is NO READING: the relaunch then tells the
// session every call after its save point, as it does without one, which is
// never wrong, and no launch is refused for it.
//
// ONLY OF THE CAPTURE THE RELAUNCH CONTINUES: its run's record keeps exactly
// that capture, cut off, and [ContinuityOf], THE decision, still continues the
// turn's row with the runner that reads it. A capture whose harness ended its
// own run holds what its save point says, and is never read.
//
// ONE WAIT, TAKEN BEFORE THE RECORD IS READ ([holdingsReadWait]): the record,
// the row, the reader's slots, the open and the read all fall inside it, on a
// frame that holds the seat and a slot of the node's admission and has
// already waited for the capture's keep ([relaunchCaptureWait]).
//
// THE OBJECT IS READ A SECOND TIME, STREAMED AND NEVER SPOOLED
// ([SessionRestore.Open]): a second object-store read and opening of what the
// launch that continues the session fetches again, at most
// executorkeep.SealedMaxBytes and ordinarily a mebibyte or two. A spool of the
// reading's own would sit on the node's scratch disk beside the launch's,
// which is not what crewlet-operator sizes an engine pod's scratch volume by,
// and one handed on to the launch would change the fetch crewlet-operator
// copies ([Coordinator.fetchSession]). What a stream hands out is provisional
// until io.EOF, so a reader decides nothing before its stream has ended.
//
// A CALL IS HELD ONLY THROUGH ITS LOGGED RESULT ([SessionHoldings.Holds]): one
// live result, in the main thread or in the thread of a subagent the reading
// reports, whose failure is the log's, whose text is as long as the output the
// log took and whose opening is the log's, compared by a digest, so no tool
// output leaves the archive. ADR-0073 records the reading.

// HoldingsReader is a runner that reads, off a kept capture's archive, which of
// a round's calls its harness's session holds ([Coordinator.HoldingsOf]). It
// restores what it reads ([ArchiveRestorer]), since whether the session is
// continued at all is asked of the same runner first.
type HoldingsReader interface {
	ArchiveRestorer

	// ReadHoldings reads what the archive open streams holds of the calls
	// ask marks asked, or says why it reads none: a harness or a writer's
	// release whose records it does not read, an ask it does not answer, an
	// archive it cannot read whole within its bounds. It checks the ask
	// past what every reading shares ([Coordinator.HoldingsOf]) before it
	// opens anything, calls open at most once and closes what it answers,
	// and decides only once that stream has reached io.EOF. It answers
	// within ctx, which it checks between the bounded steps of its work, so
	// a reading overruns ctx's deadline by one such step at most.
	ReadHoldings(ctx context.Context, archive SessionArchive,
		open func(context.Context) (io.ReadCloser, error), ask HoldingsAsk) (SessionHoldings, error)
}

// HoldingsAsk is what a relaunch asks a reading about: the round's listed
// calls that have an outcome, as the run's log keeps them, the ones its
// telling asks about marked ([LoggedCall.Asked]). The unasked ones ride it
// too, so a reader can refuse an ask in which an asked call's id is not that
// call's alone.
type HoldingsAsk struct {
	// Calls are 1 to [MaxBridgeCalls] calls: a round's log lists at most
	// that many.
	Calls []LoggedCall
}

// LoggedCall is one listed call of a round as the run's log keeps it: what a
// reading is asked about, and what [SessionHoldings.Holds] compares with what
// the session holds.
type LoggedCall struct {
	// ID is the call's id as its harness named it ([BridgeCall.CallID]), ""
	// where the log recorded none.
	ID string

	// Asked marks a call the relaunch's telling asks about: a listed call
	// after the save point that is neither late nor abandoned, nor the call
	// a person approved, each of which the telling decides before any
	// reading.
	Asked bool

	// Failed, Output and OutputBytes are the call's outcome as the log keeps
	// it ([keptCall]): whether it failed; its output whole, or, where the log
	// cut it, its opening and a marker; and the whole output's size where it
	// was cut, 0 where it is whole.
	Failed      bool
	Output      string
	OutputBytes int
}

// HoldingsKind is which facts a reading holds, by which
// [SessionHoldings.Holds] reads it: each harness's session names its calls in
// a way of its own.
type HoldingsKind string

// HoldingsByID is a reading of the records a session files under each call's
// own id ([LoggedCall.ID]): Claude Code's, whose transcript files a call's use
// and its result under the id its harness sent the bridge.
const HoldingsByID HoldingsKind = "by_id"

// HoldingsKinds are the kinds of reading this build reads.
var HoldingsKinds = []HoldingsKind{HoldingsByID}

// Valid reports whether k is one of [HoldingsKinds]: a reading of any other
// kind holds no call.
func (k HoldingsKind) Valid() bool { return slices.Contains(HoldingsKinds, k) }

// SessionHoldings is what a reading found, in the archive of a kept capture,
// of the calls a relaunch asked about: facts, which [SessionHoldings.Holds]
// alone turns into a verdict. The zero value is no reading.
type SessionHoldings struct {
	// Read is set where the reading read the archive to its stream's end
	// within every bound: a reading cut part way vouches for none of its
	// counts.
	Read bool

	// Kind is which facts the reading holds.
	Kind HoldingsKind

	// Calls are, by id, the records of each asked call ([HoldingsByID]):
	// missing where no record the reading counts names it.
	Calls map[string]CallRecords

	// Subagents are, by agent id, every subagent whose thread holds a
	// record naming an asked call ([HoldingsByID]).
	Subagents map[string]SubagentThread
}

// CallRecords is what a reading counted of the records naming one asked call.
type CallRecords struct {
	// Thread is the agent id of the subagent whose thread holds the records
	// naming the call, "" for the main thread; and "" where Threads is other
	// than 1.
	Thread string

	// Threads is how many of the threads the reading read hold a record
	// naming the call; Uses and Results how many tool_use and tool_result
	// blocks of them name it.
	Threads int
	Uses    int
	Results int

	// Of its one result, where Results is 1: Live, whether its session hands
	// it to its model whole when the session is continued, which a result its
	// harness hands its model replaced by a preview is not; Failed, its is_error,
	// absent read as false; TextBytes, its text's length, -1 where its
	// content is neither a string nor one text block; and Opening, the
	// SHA-256 of its text's first min([HoldingsOpeningBytes], TextBytes)
	// bytes.
	Live      bool
	Failed    bool
	TextBytes int
	Opening   [32]byte
}

// SubagentThread is what a reading says of one subagent whose thread holds a
// record naming an asked call.
type SubagentThread struct {
	// Reported is whether the session holds the subagent's work as done and
	// answered, so that a result its thread holds is one the continued
	// session holds; where it is not, Why says which condition failed, in
	// words for the log.
	Reported bool
	Why      string

	// Description is the subagent's description as its harness recorded
	// it, cut to the reader's bound as read: the box's words, which a
	// telling that names the subagent defuses before a model reads them.
	Description string `untrusted:"name"`
}

// HoldingsOpeningBytes is how much of a call's output, from its start,
// [SessionHoldings.Holds] compares with the text of the result its session
// holds, by digest ([CallRecords.Opening]), beside the whole output's length.
//
// 256 BYTES, INSIDE WHAT THE LOG ALWAYS KEEPS OF AN OUTPUT: past
// [MaxBridgeOutputBytes] the log keeps an output's opening and a marker
// ([keptCall]), and an output whose every byte the log's encoding escapes to
// six still keeps 672 bytes of its opening, so the first 256 bytes of every
// kept output are the output's own (TestALoggedOutputKeepsTheOpeningAHoldingCompares
// holds the margin). A result whose text is as long as the output and opens
// alike for those bytes is read as the output; one its harness wrote itself,
// without the call's outcome (a resume's for a call it found open, a stopped
// harness's, a timed-out call's), carries the harness's own words, and is
// told.
const HoldingsOpeningBytes = 256

// Holds is whether the session holds c's logged result, and where it does not,
// under is the agent whose heading tells c: set only where c's records lie in
// the thread of a subagent the reading does not report, "" telling c in the
// main list.
//
// ONLY THROUGH ONE LIVE RESULT THAT IS THE LOG'S: records naming c in one
// thread alone, one use and one result there, the result live, in the main
// thread or in the thread of a subagent the reading reports, its failure the
// log's, its text as long as the whole output the log took, and its opening
// the log's ([HoldingsOpeningBytes]). Its harness was handed exactly the output
// the log took, so a result anything else wrote never matches, and is told.
// A reading of no kind this build reads ([HoldingsKind.Valid]) holds no call.
// Asked only of a call its reading was asked about ([LoggedCall.Asked]).
func (h SessionHoldings) Holds(c LoggedCall) (held bool, under string) {
	switch {
	case !h.Read || !h.Kind.Valid():
		return false, ""
	case h.Kind == HoldingsByID:
		return h.heldByID(c)
	}
	return false, ""
}

// heldByID is [SessionHoldings.Holds] of a reading of the records its session
// files under each call's id.
func (h SessionHoldings) heldByID(c LoggedCall) (bool, string) {
	records, named := h.Calls[c.ID]
	if c.ID == "" || !named || records.Threads != 1 {
		return false, ""
	}
	// UNDER ITS SUBAGENT ONLY WHERE THAT SUBAGENT IS UNREPORTED: a call the
	// thread of a reported one holds, whose result is not the log's, is told
	// in the main list, as any other call the session does not hold.
	if records.Thread != "" && !h.Subagents[records.Thread].Reported {
		return false, records.Thread
	}
	if records.Uses != 1 || records.Results != 1 || !records.Live || records.Failed != c.Failed {
		return false, ""
	}
	whole := c.OutputBytes
	if whole == 0 {
		whole = len(c.Output)
	}
	opening := min(HoldingsOpeningBytes, whole)
	if whole < 0 || records.TextBytes != whole || opening > len(c.Output) {
		return false, ""
	}
	return records.Opening == sha256.Sum256([]byte(c.Output[:opening])), ""
}

// holdingsReadWait bounds a reading of what a cut-off session holds
// ([Coordinator.HoldingsOf]), from before the run's record is read: the
// record, the row, the reader's slots, the open and the read.
//
// FIFTEEN SECONDS, the frame's patience for one operation the size of a
// session, as the keep's is ([relaunchCaptureWait]): the relaunch's frame
// holds the seat and a slot of the node's admission, so what it spends on a
// kept session is thirty seconds at most, fifteen for its keep and fifteen
// for its reading. An ordinary kept session is a mebibyte or two sealed: one
// object-store read, and one pass over its lines. A reader checks the wait
// between the bounded steps of its work ([HoldingsReader]), so a reading
// overruns it by one such step at most; a reading past it is no reading, and
// the relaunch tells the session every call after its save point, which is
// never wrong. MEASURED on two cores (the executor's
// BenchmarkAReadingAtItsBounds): Claude Code's reading of an ordinary kept
// session, 1.6 MiB sealed, takes about 50 ms, so a burst of 32 relaunches,
// whose readings take turns on the seats' one transcript slot, reads in about
// a second and a half; and the slowest reading at its bounds, 255 MiB of
// lines, takes 2 s, inside the wait too.
const holdingsReadWait = 15 * time.Second

// HoldingsOf is what the session a relaunch of turnID continues holds of the
// calls ask marks asked, read by reader off the capture (launchSeq,
// captureSeq) the turn's run record keeps; or, with no reading, why: never a
// refusal of the relaunch, which then tells the session every call after its
// save point. See the file comment.
//
// ONLY OF THE CAPTURE THE RELAUNCH CONTINUES, CUT OFF: the run's record keeps
// exactly that capture, its harness cut off, and [ContinuityOf] still
// continues the turn's row with reader, as the launch's caller asks it
// ([Coordinator.ContinuedCapture]). INSIDE ONE WAIT ([holdingsReadWait]),
// taken before the record is read. The ask is held here only to what every
// reading shares, a reader and 1 to [MaxBridgeCalls] calls; reader checks the
// rest, since what its harness's records answer is its own to say
// ([HoldingsReader]).
func (c *Coordinator) HoldingsOf(ctx context.Context, turnID string, launchSeq, captureSeq int64,
	ask HoldingsAsk, reader HoldingsReader) (SessionHoldings, error) {

	switch {
	case reader == nil:
		return SessionHoldings{}, errors.New("no runner of this build reads what a session its harness " +
			"wrote holds of a round's calls")
	case len(ask.Calls) == 0 || len(ask.Calls) > MaxBridgeCalls:
		return SessionHoldings{}, fmt.Errorf("the ask names %d calls, and a reading is asked about 1 to %d: "+
			"a round's log lists at most that many", len(ask.Calls), MaxBridgeCalls)
	}
	if why := c.continuesNoKept(); why != "" {
		return SessionHoldings{}, errors.New(why)
	}
	ctx, cancel := context.WithTimeout(ctx, c.holdingsWait)
	defer cancel()
	run, found, err := c.pending.Get(ctx, turnID)
	switch {
	case err != nil:
		return SessionHoldings{}, fmt.Errorf("the run's record could not be read: %w", err)
	case !found:
		return SessionHoldings{}, errors.New("the turn has no run record")
	}
	switch kept := run.Kept; {
	case kept == nil || kept.LaunchSeq != launchSeq || kept.CaptureSeq != captureSeq:
		return SessionHoldings{}, fmt.Errorf("the run's record does not keep the capture the relaunch "+
			"continues (launch %d, capture %d)", launchSeq, captureSeq)
	case !kept.Interrupted:
		return SessionHoldings{}, fmt.Errorf("the kept capture (launch %d, capture %d) is of a harness that "+
			"ended its own run, whose session holds what its save point says, and is not read",
			launchSeq, captureSeq)
	}
	row, err := c.restore.Session(ctx, run.AgentHandle, turnID)
	if err != nil {
		return SessionHoldings{}, fmt.Errorf("the turn's row on the executor log could not be read: %w", err)
	}
	if continuity := ContinuityOf(row, &run, run.LaunchSeq, reader); !continuity.Continues {
		return SessionHoldings{}, errors.New(continuity.Why)
	}
	holdings, err := reader.ReadHoldings(ctx, ArchiveOf(row), func(ctx context.Context) (io.ReadCloser, error) {
		return c.restore.Open(ctx, row)
	}, ask)
	if err != nil {
		return SessionHoldings{}, err
	}
	return holdings, nil
}
