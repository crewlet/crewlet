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
// list repeats calls the session holds. [Coordinator.HoldingsOf] reads which of
// them the archive holds, by id or by content as its harness names a call
// ([HoldingsKind]); internal/agent/runner asks it once per such relaunch, and
// the launch hands a brief told by it only to a box whose harness answers the
// release read for ([ExactTelling]). Every failure is no reading, which tells
// every call after the save point and is never wrong. The package doc's "A
// cut-off session's calls, read by id or by content" has the rules; ADR-0073.

// HoldingsReader is a runner that reads, off a kept capture's archive, which of
// a round's calls its harness's session holds ([Coordinator.HoldingsOf]). It
// restores what it reads ([SessionRestorer.RestoresUnder]), since whether the
// session is continued at all is asked of the same runner first. It streams the archive
// and never spools it ([SessionRestore.Open]), as a restore does
// ([Coordinator.fetchSession]), so the spool room ([spoolRoomBytes]) is all
// the sessions a node holds on its scratch disk.
type HoldingsReader interface {
	archiveJudge

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

	// Unlisted are the round's calls the log lists no outcome of, by what it
	// knows of each ([CallIdentity]): those in flight at the read, and those
	// it dropped from its middle, late or not. A content reading reads them
	// as wildcards ([HoldingsByContent]); a reading by id reads none.
	Unlisted []CallIdentity
}

// CallIdentity is a call the log lists no outcome of, by what it knows of it:
// its tool's name and its arguments' digest, "" where it knows neither.
//
// InFlight marks one in flight at the read, which has no outcome yet; any
// other is an entry of the tally of calls the log dropped from its middle
// ([ElidedCall]), OnlyFailed where every call that entry counts failed and
// none is unfinished. A reader holds the in-flight ones to [MaxCallsInFlight]
// and the dropped ones to the tally's own bound ([MaxElidedIdentityBytes]).
type CallIdentity struct {
	Name, Digest         string
	InFlight, OnlyFailed bool
}

// LoggedCall is one listed call of a round as the run's log keeps it: what a
// reading is asked about, and what [SessionHoldings.Holds] compares with what
// the session holds.
type LoggedCall struct {
	// ID is the call's key as the ask names it (internal/agent/runner's
	// callKey): a Claude Code call's id as its harness named it
	// ([BridgeCall.CallID]); an OpenCode or Qwen Code call's place in its log,
	// Appended in decimal, which the reader checks before it keys its answer
	// by it. ""
	// where the call has none.
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

	// Appended and Seq are the call's place in its round's log and its
	// admission place ([BridgeCall.Appended], [BridgeCall.Seq]); Name its
	// tool's; Digest the log's digest of its arguments, "" where it has none
	// ([BridgeCall.Digest]); Args its arguments' key. A content reading alone
	// reads them ([HoldingsByContent]).
	Appended, Seq int
	Name, Digest  string
	Args          ArgsKey

	// Held marks a call held for a person's approval: its arguments withheld
	// ([BridgeCall.Withheld]) and its output the answer it was held with. Its
	// class is never stated ([ClassOf]), and it is never asked.
	Held bool
}

// ArgsKey is a call's arguments' key as the log keeps their text: Sum is
// internal/tools/toolargs' Key of that text, and Whole says the log kept the
// text whole and it states the arguments. Where Whole is false the call's
// class is not stated ([ClassOf]): arguments cut, withheld, unparsed or
// unreadable, or "" with no digest, which is the empty object only for an
// admitted call.
type ArgsKey struct {
	Whole bool
	Sum   [32]byte
}

// HoldingsKind is which facts a reading holds, by which
// [SessionHoldings.Holds] reads it: each harness's session names its calls in
// a way of its own.
type HoldingsKind string

// HoldingsByID is a reading of the records a session files under each call's
// own id ([LoggedCall.ID]): Claude Code's, whose transcript files a call's use
// and its result under the id its harness sent the bridge.
const HoldingsByID HoldingsKind = "by_id"

// HoldingsByContent is a reading of classes of calls by what they were
// ([CallClass]): OpenCode's, whose session names no call by an id the log
// keeps.
const HoldingsByContent HoldingsKind = "by_content"

// HoldingsKinds are the kinds of reading this build reads.
var HoldingsKinds = []HoldingsKind{HoldingsByID, HoldingsByContent}

// Valid reports whether k is one of [HoldingsKinds]: a reading of any other
// kind holds no call.
func (k HoldingsKind) Valid() bool { return slices.Contains(HoldingsKinds, k) }

// SessionHoldings is what a reading found, in the archive of a kept capture,
// of the calls a relaunch asked about: facts, and a content reading's verdict
// on each class, which [SessionHoldings.Holds] alone turns into an answer for a
// call. The zero value is no reading.
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

	// Members are, by the ask's key ([LoggedCall.ID]), the class of each asked
	// call the reading pooled with its class stated; Classes the verdict on
	// each class ([HoldingsByContent]).
	Members map[string]CallClass
	Classes map[CallClass]ClassRecords

	// Compacted is whether the session compacted after the run's start
	// ([HoldingsByContent]), for the reading's log line.
	Compacted bool
}

// CallClass is a class of calls by what they were: the tool's name, the
// arguments' key ([ArgsKey]), whether it failed, and the output's text key.
// Calls of one class are alike to the model that reads their parts, so a
// reading counts them, never pairs them ([ClassOf]).
type CallClass struct {
	Name   string
	Args   [32]byte
	Failed bool
	Text   TextKey
}

// TextKey is what a class's output is matched by: Bytes, the whole output's
// size; Whole, whether the log kept it whole; Opening, where it cut it, how
// many of its first bytes it kept (0 where whole); Sum, the SHA-256 of the
// whole output, or of all the opening the log kept.
type TextKey struct {
	Bytes   int
	Whole   bool
	Opening int
	Sum     [32]byte
}

// ClassRecords is a reading's verdict on one class: Calls, the pool's calls of
// it; Parts, the run's live parts of it the reading counted, counted to
// Calls+1; Wild, whether a call the log cannot state the class of may be of
// it; Held, whether the reader holds it (Calls at least 1, Parts equal to
// Calls, not Wild, and its parts in the log's order).
type ClassRecords struct {
	Calls, Parts int
	Wild, Held   bool
}

// ClassOf is c's class, and false where c's arguments or output cannot be
// stated: arguments the log did not keep whole ([ArgsKey]), a call held for a
// person's approval, or a cut output whose opening is not read back exactly
// (internal/kept's Record.Opening). A cut output is keyed by ALL the log kept
// of it, never [HoldingsOpeningBytes]: with no id to pair by, the text is the
// pairing.
func ClassOf(c LoggedCall) (CallClass, bool) {
	if !c.Args.Whole || c.Held {
		return CallClass{}, false
	}
	text, ok := TextKeyOf(c.Output, c.OutputBytes)
	if !ok {
		return CallClass{}, false
	}
	return CallClass{Name: c.Name, Args: c.Args.Sum, Failed: c.Failed, Text: text}, true
}

// TextKeyOf is the text key of an output the log keeps as output, cut from a
// whole of outputBytes where that is above 0 ([LoggedCall.OutputBytes]), and
// false where it cannot be stated: a negative size, or a cut whose opening is
// not read back exactly. A content reading keys a held call's answer by it.
func TextKeyOf(output string, outputBytes int) (TextKey, bool) {
	switch {
	case outputBytes < 0:
		return TextKey{}, false
	case outputBytes == 0:
		return TextKey{Bytes: len(output), Whole: true, Sum: sha256.Sum256([]byte(output))}, true
	}
	opening, ok := OutputOpening(output, outputBytes)
	if !ok {
		return TextKey{}, false
	}
	return TextKey{Bytes: outputBytes, Opening: len(opening), Sum: sha256.Sum256([]byte(opening))}, true
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
	// words for the log: the reading's own, ids the call-id rule holds, and a
	// decode error's first line, redacted and cut as a reading's errors are,
	// so it holds none of the box's words and needs no untrusted tag.
	Reported bool
	Why      string

	// Description is the subagent's description as its harness recorded
	// it, cut to the reader's bound as read: the box's words, which a
	// telling that names the subagent defuses before a model reads them. The
	// one text of the box's a reading hands on, so it alone is declared.
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
// read ([HoldingsKind.Valid]) holds none. A reading by id holds c only
// through one live result that is the log's ([heldByID]); a content reading
// only as a member of a held class ([heldByContent]).
func (h SessionHoldings) Holds(c LoggedCall) (held bool, under string) {
	switch {
	case !h.Read || !h.Kind.Valid():
		return false, ""
	case h.Kind == HoldingsByID:
		return h.heldByID(c)
	case h.Kind == HoldingsByContent:
		return h.heldByContent(c), ""
	}
	return false, ""
}

// heldByContent is [SessionHoldings.Holds] of a reading of classes: c, by the
// ask's key, is a member whose class is c's own ([ClassOf]) and held whole.
// ALWAYS IN THE MAIN LIST: a subagent's calls lie in a child session, which
// the reading never reads, so none is told under a subagent.
func (h SessionHoldings) heldByContent(c LoggedCall) bool {
	class, member := h.Members[c.ID]
	own, stated := ClassOf(c)
	if c.ID == "" || !member || !stated || own != class {
		return false
	}
	records := h.Classes[class]
	return records.Held && records.Calls >= 1 && records.Parts == records.Calls && !records.Wild
}

// heldByID is [SessionHoldings.Holds] of a reading of the records its session
// files under each call's id.
//
// ONLY THROUGH ONE LIVE RESULT THAT IS THE LOG'S: c's records in one thread,
// one use and one result there, live, in the main thread or a reported
// subagent's, with the log's failure and whole output's length, and the log's
// text: every byte where it kept the output whole ([CallRecords.Whole]), its
// opening where it cut it ([HoldingsOpeningBytes]). Its harness was handed
// exactly that output, so a result anything else wrote never matches.
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
// run's record is read: the record, the row, the reading's slot, open and read.
//
// THIRTY SECONDS, so a reading at its bounds is one; past it is none. On two
// dedicated cores the slowest, a subagent's lines of blocks of an empty kind to
// both bounds, reads in 21.4 to 22.6 s (BenchmarkAReadingAtItsBounds); OpenCode's
// 255 MiB of `0` items 10.7 to 11.0 s; Qwen Code's least records 3.5 to 3.9 s;
// Gemini CLI's `$patch` of 255 MiB of `{}` parts 7.5 s (each its harness's
// Benchmark…AtItsBounds). A second reading waits behind the first, up to 22.6
// s; no seat's read does. A kept session costs the relaunch's frame this
// beside its wait for the keep ([Coordinator.relaunchWaitOf]), a reader's
// overrun step, an unread member's discard and the keep's settle writes.
const holdingsReadWait = 30 * time.Second

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
	kept := run.Kept
	switch {
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
	// UNDER THE ENTRY THE SESSION RAN UNDER: the reading is asked before the
	// launch picks its member, whose entry the launch judges itself
	// ([EntryContinues]); a reading never continues anything.
	continuity := ContinuityOf(row, &run, run.LaunchSeq, RestorerUnder(reader, row.Entry))
	if !continuity.Continues {
		return SessionHoldings{}, errors.New(continuity.Why)
	}
	// WHERE THE RUN BEGAN IS THE KEPT CAPTURE'S WORD, the capture ContinuityOf
	// has just held the row to, and never the caller's.
	archive := ArchiveOf(row)
	archive.RunStart, archive.Unshadowed = kept.RunStart(), kept.RunUnshadowed
	holdings, err := reader.ReadHoldings(ctx, archive, func(ctx context.Context) (io.ReadCloser, error) {
		return c.restore.Open(ctx, row)
	}, ask)
	if err != nil {
		return SessionHoldings{}, err
	}
	return holdings, nil
}
