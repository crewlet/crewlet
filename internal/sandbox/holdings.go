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
// A relaunch continuing a cut-off capture ([SessionCapture.Interrupted]) tells
// the session every call logged after the save point, which is the newest its
// launch held, not where the session ends ([SessionCapture.Through]), so the
// list repeats calls the session holds. Its harness files each call's use and
// result under the call's id ([BridgeCall.CallID]), so [Coordinator.HoldingsOf]
// reads which of them the archive holds; internal/agent/runner asks it once per
// such relaunch (runner.HoldingsAsker), and the launch hands a brief told by it
// only to a box whose harness answers the release read for ([ExactTelling]).
// Every failure is no reading, never a refusal: the relaunch then tells every
// call after the save point, which is never wrong. ADR-0073.

// HoldingsReader is a runner that reads, off a kept capture's archive, which of
// a round's calls its harness's session holds ([Coordinator.HoldingsOf]). It
// restores what it reads ([ArchiveRestorer]), since whether the session is
// continued at all is asked of the same runner first. It reads the archive a
// second time, streamed and never spooled ([SessionRestore.Open]): a spool of
// its own would sit on the node's scratch disk beside the launch's, which is
// not what crewlet-operator sizes an engine pod's scratch volume by, and one
// handed to the launch would change the fetch crewlet-operator copies
// ([Coordinator.fetchSession]).
type HoldingsReader interface {
	ArchiveRestorer

	// ReadHoldings reads what the archive open streams holds of the calls ask
	// marks asked, or says why it reads none: a harness or writer's release
	// whose records it does not read, a resumer's release
	// ([HoldingsAsk.Resumer]) whose resume it has not read, an ask it does not
	// answer, an archive it cannot read whole within its bounds. It checks the
	// ask past what every reading shares ([Coordinator.HoldingsOf]) before it
	// opens anything, calls open at most once, closes what it answers, and
	// decides nothing before the stream reaches io.EOF. It checks ctx between
	// the bounded steps of its work, so it overruns ctx's deadline by one step
	// at most.
	ReadHoldings(ctx context.Context, archive SessionArchive,
		open func(context.Context) (io.ReadCloser, error), ask HoldingsAsk) (SessionHoldings, error)
}

// HoldingsAsk is what a relaunch asks a reading about: the round's listed calls
// with an outcome, as the run's log keeps them, save those a person approved
// (made by the engine on its host, so held by no session), with the ones its
// telling asks about marked ([LoggedCall.Asked]). The unasked ones ride along
// so a reader can refuse an ask in which an asked call's id is not that call's
// alone.
type HoldingsAsk struct {
	// Calls are 1 to [MaxBridgeCalls] calls: a round's log lists at most
	// that many.
	Calls []LoggedCall

	// Resumer is the release of the harness that continues the session, in the
	// form [CarryCheck.Version] keeps it ("2.1.310", one [ValidRelease] holds);
	// "" means unread, and no reader holds it. The relaunch knows only the
	// release that WROTE the session (the turn's row's, as the job's box
	// answered `--version`), since its own box is acquired after its briefs are
	// written, so a brief told by the reading goes only to a box whose harness
	// answers exactly this release ([ExactTelling], [Coordinator.continueKept]).
	//
	// What a session holds is what its resume hands its model, and that is the
	// resuming release's to decide: a release may load, order or drop another's
	// records otherwise. So each reader holds Resumer to the releases whose
	// resume it read, and any other is no reading.
	Resumer string
}

// LoggedCall is one listed call of a round as the run's log keeps it: what a
// reading is asked about, and what [SessionHoldings.Holds] compares with what
// the session holds.
type LoggedCall struct {
	// ID is the call's id as its harness named it ([BridgeCall.CallID]), ""
	// where the log recorded none.
	ID string

	// Asked marks a call the relaunch's telling asks about: one it would
	// otherwise list after the save point or among the owed reads (counted by
	// an earlier brief without being listed, at or below the point), whose key
	// the reader can name, valid and carried by no other call of the round
	// (internal/agent/runner's holdingsAsk). Never a late or abandoned call, a
	// skipped meta-tool's, nor one the save point holds: the telling decides
	// those before any reading.
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

	// Of its one result, where Results is 1: Live, whether the continued
	// session hands it to its model whole (not replaced by a preview); Failed,
	// its is_error, absent read as false; TextBytes, its text's length, -1 where
	// its content is neither a string nor one text block; Opening, the SHA-256
	// of its text's first min([HoldingsOpeningBytes], TextBytes) bytes; Whole,
	// the SHA-256 of its whole text where TextBytes is at most
	// [MaxBridgeOutputBytes] (the most the log keeps whole), zero otherwise.
	// Digests, so no tool output leaves the archive.
	Live      bool
	Failed    bool
	TextBytes int
	Opening   [32]byte
	Whole     [32]byte
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
// [SessionHoldings.Holds] compares by digest ([CallRecords.Opening]) with the
// text of the result its session holds where the log cut the output, beside
// the whole output's length.
//
// 256 BYTES, INSIDE WHAT THE LOG ALWAYS KEEPS OF AN OUTPUT: past
// [MaxBridgeOutputBytes] it keeps an opening and a marker ([keptCall]), 672
// bytes of opening even with every byte escaped to six, so these 256 are always
// the output's own (TestALoggedOutputKeepsTheOpeningAHoldingCompares). A result
// its harness wrote itself, without the call's outcome (a resume's for a call
// it found open, a stopped harness's, a timed-out call's), carries the
// harness's own words, and is told.
const HoldingsOpeningBytes = 256

// Holds is whether the session holds c's logged result; where it does not,
// under is the agent whose heading tells c ("" for the main list), set only
// where c's records lie in an unreported subagent's thread. Asked only of an
// asked call ([LoggedCall.Asked]); a reading of a kind this build does not
// read ([HoldingsKind.Valid]) holds none.
//
// ONLY THROUGH ONE LIVE RESULT THAT IS THE LOG'S: c's records in one thread,
// one use and one result there, live, in the main thread or a reported
// subagent's, with the log's failure and whole output's length, and the log's
// text: every byte where it kept the output whole ([CallRecords.Whole]), its
// opening where it cut it ([HoldingsOpeningBytes]). Its harness was handed
// exactly that output, so a result anything else wrote never matches.
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
	// AN OUTPUT THE LOG KEPT WHOLE IS COMPARED WHOLE, never by its opening,
	// which a result alike that far and different past it would match. A reader
	// keeps no whole digest past what the log keeps whole, so one logged whole
	// and longer, which no log writes, is told.
	if c.OutputBytes == 0 {
		return records.Whole == sha256.Sum256([]byte(c.Output)), ""
	}
	return records.Opening == sha256.Sum256([]byte(c.Output[:opening])), ""
}

// holdingsReadWait bounds a reading ([Coordinator.HoldingsOf]) from before the
// run's record is read: the record, the row, the reader's slots, open and read.
//
// FIFTEEN SECONDS, the frame's patience for one session-sized operation, as the
// keep's is ([relaunchCaptureWait]): the relaunch's frame holds the seat and an
// admission slot, so a kept session costs it thirty seconds, plus the one
// bounded step, a 64 KiB fill, a reader may overrun by ([HoldingsReader]) and
// the keep's settle writes ([Coordinator.ContinuedCapture]). A reading past it
// is no reading. BenchmarkAReadingAtItsBounds, two cores of a loaded host: an
// ordinary 1.6 MiB archive reads in ~55 ms (a burst of 32 in the seats' one
// transcript slot, ~1.8 s), 255 MiB of 64 MiB prompts (the plaintext's bound)
// in 1.05 to 1.8 s, and 8 MiB of the slowest lines measured in 0.56 to 0.59 s.
const holdingsReadWait = 15 * time.Second

// HoldingsOf is what the session a relaunch of turnID continues holds of the
// calls ask marks asked, read by reader off the capture (launchSeq,
// captureSeq) the turn's run record keeps; or, with no reading, why, which
// never refuses the relaunch (see the file comment).
//
// ONLY OF THE CAPTURE THE RELAUNCH CONTINUES, CUT OFF, while [ContinuityOf]
// still continues the turn's row with reader, as the launch's caller asks it
// ([Coordinator.ContinuedCapture]): a capture whose harness ended its own run
// holds what its save point says. INSIDE ONE WAIT ([holdingsReadWait]), taken
// before the record is read. The ask is held here only to what every reading
// shares (a reader, 1 to [MaxBridgeCalls] calls, a resumer's release); reader
// checks the rest, which is its own to say ([HoldingsReader]).
func (c *Coordinator) HoldingsOf(ctx context.Context, turnID string, launchSeq, captureSeq int64,
	ask HoldingsAsk, reader HoldingsReader) (SessionHoldings, error) {

	switch {
	case reader == nil:
		return SessionHoldings{}, errors.New("no runner of this build reads what a session its harness " +
			"wrote holds of a round's calls")
	case len(ask.Calls) == 0 || len(ask.Calls) > MaxBridgeCalls:
		return SessionHoldings{}, fmt.Errorf("the ask names %d calls, and a reading is asked about 1 to %d: "+
			"a round's log lists at most that many", len(ask.Calls), MaxBridgeCalls)
	case ask.Resumer == "":
		return SessionHoldings{}, errors.New("the ask names no release of the harness that continues the " +
			"session, and what a session holds is what that release's resume hands its model")
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
