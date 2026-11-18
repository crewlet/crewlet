package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHICH OF A ROUND'S CALLS A CLAUDE CODE SESSION HOLDS, read off its kept
// archive ([Runner.ReadHoldings]). The argument, its measurements and what it
// leaves are ADR-0073's amendment "a cut-off session's archive is read for the
// calls whose logged results it holds"; this comment names the rules the code
// below cites.
//
// Claude Code files a call's use and its result under the id it sent the
// bridge ([sandbox.BridgeCall.CallID]), in the thread of whoever made it. The
// reading gathers facts ([sandbox.CallRecords], [sandbox.SubagentThread]), and
// [sandbox.SessionHoldings.Holds] alone turns them into a verdict. It reads the
// main thread and each depth-one subagent's thread and metadata, so a call
// recorded only in a deeper thread is absent from the reading, and told.

// THE WALK ([eachMember]), ctx asked before each member and line: a RAW name
// that is absolute or holds `..` is no reading, since a restore could write
// over the session's own files; a cleaned name is classed by
// [claudeMemberOf] alone, every other member passed over through the
// archive's meter; and a member the reading reads held twice is no reading,
// since tar restores the last copy. MEMBER ORDER IS THE BOX'S, so facts are
// gathered thread by thread and nothing is decided before the stream ends: a
// result, an answer or a notification is filed only under a fact that already
// exists (an asked call's, or a leg's once its call was read, [legFact]), and
// one read before its call is kept nowhere, the safe direction, so no fact
// grows with a thread's results.

// TWO METERS, ONE BUDGET: the gzip stream is metered at [threadPlainMax] by
// the scrub's meter ([boundedPlain]), and the members read share what is left
// of that budget ([handedReader]), since a sparse member hands out holes its
// stream does not hold. THE STREAM IS READ TO io.EOF before anything is
// decided: the store's stream is provisional until then and latches its
// error, so the drain meets again a fault gzip met past tar's end. The gzip
// trailer is not checked: the store authenticated every chunk under it.

// THE GATE: the resuming release ([sandbox.HoldingsAsk.Resumer]) is one
// [claudeResumeRead] names, a set and never a range, and every chained
// record's writer is at or past [holdingsWriterFloor] ([writerBelowFloor]). It
// reads the release, never the box's settings: a managed settings file can
// switch either resume switch back on ([claudeResumeSwitches]), the precompact
// one read by the model ([threadChain], THE LARGE FILE), the interrupted
// turn's the caller's question (the amendment's WHAT THE GATE CANNOT CLOSE), as
// are a saved compaction ([claudePrecompact]) and a continuation's first
// request. Every failure is no reading, never a refusal ([holdingsError]). A
// line's or the archive's bound passed in a subagent's thread ends the whole
// reading, since uses and results are counted over the whole archive; the
// model's bounds there leave that subagent not whole.

// LIVE ONLY WHERE THE RESUME HANDS ITS MODEL BOTH RECORDS AND ITS REQUEST
// PAIRS THEM. What the resume hands, and in what orders, is [threadChain]'s,
// cited and never restated: in the main thread `claude --resume`'s load, in a
// subagent's that subagent's own (newThreadChain(agent)). The reading adds two
// rules of its own, each telling where in doubt. THE LAST RESTART, its forward
// guard: in the main thread a result, a leg's answer and a task notification
// count only past the last restart ([threadRestarts.See], [holdingsWalk.live]),
// and a subagent's thread that restarts at all is not whole (rule 2), since a
// later release that cut a thread there would hand less than lies before it.

// THE PAIRING RULE, the conservative rule (C) ([holdingsPairing]), read off
// the orders the model hands, never the file's: a call is held only where its
// use carries a reply id (C0); one use and one result of its id are counted
// over every user and assistant record of its thread's file, no batch's list
// or split id folds it, its id is none Claude Code writes of its own (held by
// C4), and its use names a bridged tool (C1, [claudeBridgedToolPrefix],
// [holdingsWalk.idsFold]); the resume hands both under every variant (C2);
// and on EVERY order it hands, the result follows the first record of the
// use's reply, each record from there to the later of the two is a plain
// record of that reply or a plain results record (C3, C4), none before that
// first record holds a result of the reply's calls (C5), and no compaction's
// boundary lies at or after it (C6).

// NOT LIVE in any thread: a result its harness replaced with a preview
// ([threadLine.ReplacedResults]), or a record a retraction names
// ([threadLine.Retracted]), which the resume drops (so C2 tells it); no load
// of a subagent's thread filters retractions, so a subagent's thread holding
// one is not whole. RULE 4, THE LOGGED RESULT: a call is held only through its
// one result, whose is_error is the log's failure and whose text
// ([lineDecoder.resultContent]) matches the log's output by digest, whole or
// by length and opening ([sandbox.CallRecords.Whole],
// [sandbox.HoldingsOpeningBytes]). It meets rule 3: a result written without
// the call's outcome (a resume's, a stopped harness's) never carries the log's
// text, so no mark is read.

// RULE 2, A SUBAGENT CUT OFF WITH THE SESSION: a call in a subagent's thread is
// held only where the subagent is reported ([holdingsWalk.unreported]) and the
// pairing rule holds on every order its own load hands
// ([holdingsWalk.subagentEnd]). It is reported only where all five hold:
//
//  1. LINKED: its metadata ([subagentMetaOf]) names the call that started it,
//     exactly one counted Agent or Task call of the main thread.
//  2. WHOLE: every line a record of its own, no torn tail, no restart, no
//     refused reply, no retraction, every writer at the floor, the model of its
//     load places it ([holdingsWalk.subagentLine] has each check), and THE
//     STRICT RULE ([wholeOf]).

//  3. EVERY LEG ANSWERED, LIVE past the main thread's last restart: the call
//     that started it by its own completed foreground result the pairing rule
//     pairs ([holdingsWalk.mainEnd]) or by a task notification the resume
//     hands under every variant ([textNotice]); each SendMessage to it its
//     result did not refuse ([holdingsWalk.leg]) by a notification. AN ANSWER
//     CARRYING A HANDBACK ANSWERS NO LEG, nor does any answer of a leg one of
//     whose answers does ([lineDecoder.toolResult], [textHandback]).
//  4. NO UNSEEN LEG: its file starts no more legs ([legStart]) than the main
//     thread gave it.
//  5. EVERY LEG FINISHED along every order its own load hands
//     ([unfinishedLeg], [finishedReply]).

// A subagent not reported says which condition failed first
// ([sandbox.SubagentThread.Why]). An origin, an answer's toolUseResult or a
// SendMessage's `to` that does not decode costs only the fact it could have
// been, the safe direction, condition 4 the backstop of the last.

// THE BOUNDS: the plaintext and what read members hand out at
// [threadPlainMax], read in one reader's buffers, a line's facts at
// [threadLineMax], the model at [threadChainMaxRecords] and
// [threadChainMaxCalls], a metadata at [holdingsMetaMaxBytes], the ask at
// [sandbox.MaxBridgeCalls], subagents at [holdingsMaxAgents], legs at
// [holdingsMaxLegs]; every id kept is one the call-id rule holds
// ([holdingsWalk.leg], [subagentMetaOf]), since an id is the box's word, as
// long as [threadValueMax]. MEASURED (BenchmarkAReadingAtItsBounds, whose doc
// says how): the worst, a line of the smallest results at its bound after a
// main thread at both of the model's bounds, holds 102 MiB live and peaks at
// 181 MiB in 1.7 s, within the about 133 MiB a seat's read for words holds at
// its worst ([SeatTranscriptReadsAtOnce]); an ordinary session reads in 68 ms.

// holdingsWriterFloor is the oldest Claude Code release whose sessions a
// reading reads ([ClaudeCode.holdingsFloor]).
//
// 2.1.310, the oldest release the reading's rules were measured on (the probes
// ran 2.1.310 and 2.1.322): an older one could record a restart in a shape the
// restart rule does not know, and hold a result its continuation is not
// handed. Not the carry floor ([claudeCarriedWriterFloor]), which asks what a
// box can continue. A writer below it, unrecorded or unreadable is no reading
// ([readsRelease]): the carry gate fails open on an unreadable release
// ([releaseAtLeast]), the reading closed, since what it holds is never told
// again. Held of the row's last writer before anything is opened
// ([Runner.ReadHoldings]) and of every chained record's ([writerBelowFloor]).
const holdingsWriterFloor = "2.1.310"

// holdingsMaxAgents is the most subagents a reading reads the members of; past
// it, no reading.
//
// 1,024, as many calls as a person's read remembers pending
// ([maxPendingCalls]): the probes ran at most two subagents a session, and a
// real corpus's main threads (probe P6) started 64 at depth one in all. Each
// costs a few words and a description ([holdingsDescriptionBytes]).
const holdingsMaxAgents = 1024

// holdingsMaxLegs is the most legs a reading keeps of the main thread: its
// Agent, Task and SendMessage calls, each task notification it files under
// one, and each subagent sent a message under an id the call-id rule refuses.
// Past it, no reading.
//
// Four per subagent a reading reads ([holdingsMaxAgents]): a real corpus's
// main threads (probe P6) made 50 Agent calls in all. Each costs a few words
// and ids of at most [sandbox.MaxBridgeCallIDBytes] ([holdingsWalk.leg]).
const holdingsMaxLegs = 4 * holdingsMaxAgents

// holdingsMetaMaxBytes is the most of a subagent's metadata a reading reads.
//
// 64 KIB: every metadata the probes kept is 176 to 275 bytes (90 files of
// probes P2 and T, 19 distinct). One past it leaves its subagent unlinked.
const holdingsMetaMaxBytes = 64 << 10

// holdingsDescriptionBytes is the most of a subagent's description a reading
// keeps ([sandbox.SubagentThread.Description]), cut on a rune boundary as it is
// read.
//
// ONE KIBIBYTE: the relaunch's telling cuts a description to
// ledger.ValueLimit, 200 runes (at most 800 bytes), so a kibibyte keeps at
// least 256 runes and a longer one still reads as longer. Held until the walk
// ends, 1,024 cut descriptions are a mebibyte at most rather than 64 MiB.
const holdingsDescriptionBytes = 1 << 10

// The tools whose calls are a subagent's legs: the two that start one, and the
// one that sends a subagent a message, resuming it.
const (
	claudeToolAgent       = "Agent"
	claudeToolTask        = "Task"
	claudeToolSendMessage = "SendMessage"
)

// What the records rule 2 reads say, as Claude Code writes it.
const (
	// claudeTaskNotification is the origin kind of the main thread's record of
	// a subagent's end ([textNotice]).
	claudeTaskNotification = "task-notification"
	// claudeAgentCompleted is the status of an Agent or Task call's answer
	// whose subagent finished, and [claudeNotificationCompleted] a task
	// notification's line saying so.
	claudeAgentCompleted        = "completed"
	claudeNotificationCompleted = "<status>completed</status>"
	// claudeForeground is the request shape of a subagent its call waited for,
	// whose result is its answer.
	claudeForeground = "foreground"
	// claudeEndedTurn is the stop reason of a reply with which its model
	// ended its turn ([threadLine.StopReason]), the one stop that finishes a
	// subagent's leg ([finishedReply]).
	claudeEndedTurn = "end_turn"
	// claudeBridgedToolPrefix opens the name Claude Code gives each tool of
	// the bridge's MCP server, served as config.ToolServerName, which config
	// reserves: `mcp__<server>__<tool>` (2.1.322's kk @283568191 and
	// `mcp__${bn(n)}__` @283570915, its bn @282974138 keeping letters,
	// digits, `_` and `-`; 2.1.310's IH @237581955 and `mcp__${En(n)}__`
	// @237584365). Spelled out because config imports this package; a test
	// holds the two to each other. Every logged call's use names such a tool
	// unless a box rewrote its transcript, and one that does not is told
	// (C1, [holdingsPairing]): Claude Code's request clears or withholds
	// other tools' results by their names.
	claudeBridgedToolPrefix = "mcp__crewlet__"
	// claudeSyntheticModel is the model of a reply Claude Code writes itself
	// in place of a model's ([threadLine.Model]), which finishes no
	// subagent's leg whatever it says of its stop ([finishedReply]).
	claudeSyntheticModel = "<synthetic>"
	// claudeRefused is the stop reason of a reply its model refused, which
	// leaves a subagent's thread not whole ([holdingsWalk.subagentLine]).
	claudeRefused = "refusal"
	// claudeToolHandback is the tool through which a subagent under a handback
	// contract delivers its report, which every answer of a leg that carries
	// a handback names ([textHandback]).
	claudeToolHandback = "SubagentHandback"
)

// holdingsFloor is [holdingsWriterFloor].
func (ClaudeCode) holdingsFloor() string { return holdingsWriterFloor }

// checkHoldings holds ask, and the session archive names, to what a reading
// answers, before anything is opened: a session whose id names a thread
// ([claudeMainThread]); a resumer [claudeResumeRead] names, never one at or
// after a floor, since a release outside the set the model was read from may
// load, relink, walk or drop records otherwise; and 1 to
// [sandbox.MaxBridgeCalls] calls, at least one asked, each asked call's id one
// the log keeps ([sandbox.ValidCallID]) and carried by no other call of the
// ask: the session files both calls' records under that one id, so no record
// says whose it is.
func (ClaudeCode) checkHoldings(archive sandbox.SessionArchive, ask sandbox.HoldingsAsk) error {
	if _, ok := claudeMainThread(archive.SessionID); !ok {
		return &holdingsError{why: "the session's id names no Claude Code thread"}
	}
	if !claudeResumeRead(ask.Resumer) {
		return &holdingsError{why: fmt.Sprintf("Claude Code %.64q continues the session, and a reading "+
			"reads only for the releases whose resume its model of the resume was read from (threadChain): a "+
			"release joins them once its shipped resume is re-read against that model and a census of archives "+
			"it wrote and resumed holds to it (claudeResumeRead, internal/sandbox/executor)", ask.Resumer)}
	}
	if len(ask.Calls) == 0 || len(ask.Calls) > sandbox.MaxBridgeCalls {
		return &holdingsError{why: fmt.Sprintf("the ask names %d calls, and a reading answers 1 to %d: "+
			"a round's log lists at most that many", len(ask.Calls), sandbox.MaxBridgeCalls)}
	}
	carried, asked := make(map[string]int, len(ask.Calls)), 0
	for _, c := range ask.Calls {
		carried[c.ID]++
		if c.Asked {
			asked++
		}
	}
	if asked == 0 {
		return &holdingsError{why: "the ask asks about none of its calls"}
	}
	for i, c := range ask.Calls {
		switch {
		case !c.Asked:
		case !sandbox.ValidCallID(c.ID):
			return &holdingsError{why: fmt.Sprintf("call %d of the ask is asked about, and names no id "+
				"the log keeps", i+1)}
		case carried[c.ID] > 1:
			return &holdingsError{why: fmt.Sprintf("call %d of the ask is asked about, and its id %q is "+
				"carried by another call of the ask, so no record says whose it is", i+1, c.ID)}
		}
	}
	return nil
}

// readHoldings reads body, archive's stream, within bounds, for the calls ask
// marks asked. See the file comment.
func (ClaudeCode) readHoldings(ctx context.Context, body io.Reader, archive sandbox.SessionArchive,
	ask sandbox.HoldingsAsk, bounds scrubBounds) (sandbox.SessionHoldings, error) {

	w := newHoldingsWalk(archive.SessionID, ask, bounds)
	if err := eachMember(body, bounds, func(h *tar.Header, member io.Reader) error {
		return w.member(ctx, h, member)
	}); err != nil {
		return sandbox.SessionHoldings{}, w.failed(ctx, err)
	}
	// DRAINED TO io.EOF, whatever tar left of it: what the store hands out
	// is provisional until then, and its stream latches its error, so a fault
	// gzip met past tar's end is met here again (the file comment).
	if _, err := io.Copy(io.Discard, body); err != nil {
		return sandbox.SessionHoldings{}, w.failed(ctx, &holdingsError{
			why: "the session's stream did not end whole", cause: err})
	}
	if !w.read[w.main] {
		return sandbox.SessionHoldings{}, &holdingsError{why: "the session's archive holds no main thread"}
	}
	return w.holdings(), nil
}

// holdingsWalk is one reading's state ([ClaudeCode.readHoldings]).
type holdingsWalk struct {
	// session is the session read, and main its main thread's member.
	session, main string
	// bounds is what the reading is held to, and handed what the members it
	// read have handed out of bounds.plain between them.
	bounds scrubBounds
	handed int64
	// read is every member read, by its cleaned name: a member named twice is
	// one the reading may have read in place of the one a resume reads.
	read map[string]bool
	// calls are the asked calls' facts, by id.
	calls map[string]*callFacts
	// restarts follows the main thread's heads, and lastRestart is the line
	// of its last restart, 0 where it has none.
	restarts    threadRestarts
	lastRestart int
	// batched is whether a record of the main thread carries a batch's calls
	// ([threadLine.BatchToolUses]), and splitResults whether one holds a
	// result whose id is a batch's split one ([splitBase]): the request may
	// fold a call another record names into another, so no leg is answered
	// ([holdingsWalk.idsFold]).
	batched, splitResults bool
	// agents are the subagents whose members the reading read, by agent id.
	agents map[string]*subagentFacts
	// legs are the main thread's legs of subagents' work, by the id of the
	// call each is, an id the call-id rule holds; notices the task
	// notifications filed under one, each the last's; unnamed the subagents
	// the main thread sent a message under an id that rule refuses
	// ([holdingsWalk.leg]); and kept how many of the three the reading keeps.
	legs    map[string]*legFact
	notices map[legNotice]noticeFact
	unnamed map[string]bool
	kept    int
	// lines is the one line reader every thread member the walk reads is
	// read through ([holdingsWalk.reader]).
	lines *threadReader
}

// reader is the walk's one line reader, made to read member within what the
// members read so far left of the reading's budget ([threadPlainMax]): one
// reader for every member, so a session of many subagents costs the walk one
// reader's buffers ([threadReader.Reset]), never one a member.
func (w *holdingsWalk) reader(member io.Reader) *threadReader {
	handed := &handedReader{r: member, handed: &w.handed}
	if w.lines == nil {
		w.lines = newThreadReader(handed, w.bounds.plain-w.handed)
	} else {
		w.lines.Reset(handed, w.bounds.plain-w.handed)
	}
	return w.lines
}

// callFacts is what the counted records of the threads read say of one asked
// call.
type callFacts struct {
	uses, results int
	// threads is how many threads hold a counted record naming the call:
	// thread is the first of them and last the latest, "" the main thread and
	// else a subagent's id. Each thread is read whole, once, so a change of
	// thread is a thread not counted before.
	threads      int
	thread, last string
	// useThread and useRec are the thread and the record in that thread's
	// chain of its first use, and resultThread, line and resultRec its first
	// result's thread, line and record; failed, textBytes, opening and whole
	// are that result's ([sandbox.CallRecords]).
	useThread    string
	useRec       int32
	resultThread string
	line         int
	resultRec    int32
	failed       bool
	textBytes    int
	opening      [32]byte
	whole        [32]byte
	// bridged is whether its first use names a tool of the bridge's
	// ([claudeBridgedToolPrefix]), and folds whether a record of a thread
	// read names it where the request may fold it into another call, or
	// another into it ([holdingsWalk.foldsOf]).
	bridged, folds bool
	// answered is whether the request a resumed session sends pairs the first
	// use with the first result, both of one thread, by the pairing rule
	// ([holdingsPairing]) on every order the main thread's resume, or the
	// subagent's own load, hands; filed once that thread was read whole.
	answered bool
	// replaced is whether a content-replacement record of a thread read
	// names it: its harness hands its model a preview in its result's place
	// ([threadLine.ReplacedResults]).
	replaced bool
}

// subagentFacts is what a reading gathered of one subagent: its metadata, and
// its thread as it was read (rule 2, in the file comment).
type subagentFacts struct {
	// metaRead is whether its metadata was read, and meta what it says;
	// unlinked why it says nothing, "" where it does.
	metaRead bool
	meta     subagentMeta
	unlinked string
	// namesAsked is whether a counted record of its thread names an asked
	// call, and broken why its thread is not whole, the first reason read, ""
	// where it is.
	namesAsked bool
	broken     string
	// restarts follows its thread's heads, and restarted is whether one of
	// them restarts it.
	restarts  threadRestarts
	restarted bool
	// legStarts is how many legs the user records of its own its file holds
	// start, wherever they lie (condition 4); unfinished is the first leg
	// that ends on no finished reply along a conversation its own load hands,
	// 0 where none does (condition 5, [wholeOf]).
	legStarts  int
	unfinished int
}

// subagentMeta is what a subagent's metadata says ([subagentMetaOf]).
type subagentMeta struct {
	// call is the id of the call that started it, foreground whether that call
	// waited for it, and description its description, cut as read.
	call        string
	foreground  bool
	description string
}

// legFact is one leg of subagents' work as the main thread holds it: an Agent
// or Task call, which starts a subagent, or a SendMessage call, which resumes
// one, with the first of its results.
type legFact struct {
	// spawns and sends are how many counted calls of the id are an Agent or
	// Task call and a SendMessage call; to is the subagent the first
	// SendMessage was sent to, "" where it names none; callRec is the first
	// call's record in the main thread's chain.
	spawns, sends int
	to            string
	callRec       int32
	// results is how many counted results name it; resultRec, resultLine,
	// failed, completed, agent and refused are the first's: its record, its
	// line, its is_error, and what its record's own toolUseResult says where
	// the record holds that one result alone ([answerOf]).
	results    int
	resultRec  int32
	resultLine int
	failed     bool
	completed  bool
	agent      string
	refused    bool
	// handback is whether a record of a result of it, or a task notification
	// of it, carries a handback: no answer of the leg is then its subagent's
	// report ([lineDecoder.toolResult], [textHandback]).
	handback bool
	// answered is whether the request a resumed session sends pairs the
	// first call with the first result, by the pairing rule
	// ([holdingsPairing]), past the main thread's last restart, filed once
	// the main thread was read whole.
	answered bool
}

// legNotice is a task notification's subagent and leg.
type legNotice struct{ agent, leg string }

// noticeFact is a task notification as the main thread holds it: its record
// in the main thread's chain and its line, and live, whether the resume hands
// its model that record under every variant, past the main thread's last
// restart, filed once the main thread was read whole; each the last's.
type noticeFact struct {
	record int32
	line   int
	live   bool
}

// newHoldingsWalk is a reading of session's archive, within bounds, for the
// calls ask marks asked.
func newHoldingsWalk(session string, ask sandbox.HoldingsAsk, bounds scrubBounds) *holdingsWalk {
	main, _ := claudeMainThread(session)
	w := &holdingsWalk{session: session, main: main, bounds: bounds,
		read: map[string]bool{}, calls: map[string]*callFacts{}, agents: map[string]*subagentFacts{},
		legs: map[string]*legFact{}, notices: map[legNotice]noticeFact{}, unnamed: map[string]bool{}}
	for _, c := range ask.Calls {
		if c.Asked {
			w.calls[c.ID] = &callFacts{useRec: -1, resultRec: -1}
		}
	}
	return w
}

// member reads one member of the archive, its header h and its bytes member.
func (w *holdingsWalk) member(ctx context.Context, h *tar.Header, member io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// THE RAW NAME, BEFORE IT IS CLEANED: cleaned, `projects/crewlet/x/../y`
	// is a name with no `..` in it, which a tar that restores the raw one
	// need not write where the cleaned one says.
	if outsideArchive(h.Name) {
		return &holdingsError{why: "the session's archive names a member " + memberNamed(h.Name) +
			" by an absolute path or through `..`, which a restore can write over the session's own files"}
	}
	name := path.Clean(h.Name)
	m, ok := claudeMemberOf(w.session, name)
	if !ok || m.Kind == memberPrecompact {
		return nil
	}
	if w.read[name] {
		return &holdingsError{why: "the session's archive holds " + memberWhat(m.Kind) + " twice, and a " +
			"restore keeps the last copy"}
	}
	w.read[name] = true
	if m.Kind == memberMainThread {
		if h.Typeflag != tar.TypeReg {
			return &holdingsError{why: "the session's main thread is no regular file, which no box restores"}
		}
		return w.mainThread(ctx, member)
	}
	a := w.agents[m.Agent]
	if a == nil {
		if len(w.agents) == holdingsMaxAgents {
			return &holdingsError{why: fmt.Sprintf("the session's archive names more than %d subagents",
				holdingsMaxAgents)}
		}
		a = &subagentFacts{}
		w.agents[m.Agent] = a
	}
	// A SUBAGENT'S MEMBER THAT IS NO REGULAR FILE IS PASSED OVER: its thread
	// unread holds none of the calls asked, and its metadata unread links its
	// subagent to no call, each the safe direction.
	if h.Typeflag != tar.TypeReg {
		return nil
	}
	if m.Kind == memberSubagentMeta {
		return w.readSubagentMeta(a, member)
	}
	return w.subagentThread(ctx, m.Agent, a, member)
}

// memberWhat is what a member of kind is to the session, as an error says it.
func memberWhat(kind threadMemberKind) string {
	switch kind {
	case memberSubagentThread:
		return "a subagent's thread"
	case memberSubagentMeta:
		return "a subagent's metadata"
	}
	return "its main thread"
}

// outsideArchive reports whether name, a member's raw name, is absolute or
// holds a `..` element.
func outsideArchive(name string) bool {
	if strings.HasPrefix(name, "/") {
		return true
	}
	for element := range strings.SplitSeq(name, "/") {
		if element == ".." {
			return true
		}
	}
	return false
}

// mainThread reads the session's main thread, whose bytes member hands out, a
// whole line at a time, into Claude Code's resume's model ([threadChain]) and
// the pairing rule's marks ([holdingsPairing]), and once it is read whole,
// which of its calls the request a resumed session sends pairs
// ([holdingsWalk.mainEnd]). No reading at once where a line is no record, a
// user or assistant record's message does not decode, a record replaces
// results or carries a batch in no shape Claude Code writes, or the model
// cannot place the thread ([threadChain.Unknown]): read on, the reading
// would vouch for a thread no model says the resume hands its model.
func (w *holdingsWalk) mainThread(ctx context.Context, member io.Reader) error {
	lines := w.reader(member)
	chain, pairing := newThreadChain(""), &holdingsPairing{}
	for {
		line, err := lines.Next(ctx)
		switch {
		case errors.Is(err, io.EOF):
			return w.mainEnd(ctx, chain, pairing)
		case err != nil:
			return &holdingsError{why: "the session's main thread was not read whole", cause: err}
		}
		if why, cause := writerBelowFloor(line); why != "" {
			return &holdingsError{why: fmt.Sprintf("line %d of the session's main thread %s", line.N, why),
				cause: cause}
		}
		record, message, err := chain.See(line)
		if err != nil {
			return &holdingsError{why: fmt.Sprintf("line %d of the session's main thread is no record", line.N),
				cause: err}
		}
		if why, unknown := chain.Unknown(); unknown {
			return unplaced(fmt.Sprintf("line %d of the session's main thread", line.N), why)
		}
		restart := w.restarts.See(line.Head)
		if err = line.ReplacedResults(w.replaced); err != nil {
			return &holdingsError{why: fmt.Sprintf("line %d of the session's main thread replaces results in no "+
				"shape Claude Code writes", line.N), cause: err}
		}
		if restart {
			w.lastRestart = line.N
		}
		pairing.see(record, line, message)
		if line.Head.Type != recordUser && line.Head.Type != recordAssistant {
			continue
		}
		if err = w.idsFold(line, message); err != nil {
			return err
		}
		if err = w.mainRecord(line, message, int32(record)); err != nil {
			return err
		}
	}
}

// writerBelowFloor is why line, a whole line of a thread the reading reads, is
// a chained record ([threadHead.Chained]) no release at or after
// [holdingsWriterFloor] wrote (its `version` absent, no string, unreadable or
// older, [readsRelease]), and what gave it where something did; "" otherwise.
//
// CHAINED AS CLAUDE CODE'S RESUME READS IT, never by `parentUuid` alone: a
// user, assistant, attachment or system record without one roots a chain the
// resume walks ([threadRestarts.See]), and the reading reads it. Every chained
// record, not the last writer alone, since a box continues a session an older
// release wrote ([claudeCarriedWriterFloor]). Any other record without a
// `parentUuid` is bookkeeping no rule reads, held to nothing.
func writerBelowFloor(line threadLine) (string, error) {
	if !line.Head.Chained() {
		return "", nil
	}
	release, err := line.Version()
	switch {
	case err != nil:
		return "is a record that names its writer in no shape Claude Code writes", err
	case release == "":
		return fmt.Sprintf("is a record that names no writer, where a reading reads only the records of Claude Code "+
			"%s and later", holdingsWriterFloor), nil
	case !readsRelease(release, holdingsWriterFloor):
		return fmt.Sprintf("is a record of Claude Code %.64q, where a reading reads only the records of %s and "+
			"later, the oldest they were measured on", release, holdingsWriterFloor), nil
	}
	return "", nil
}

// unplaced is no reading of a main thread where Claude Code's resume's model
// cannot place it, for why ([threadChain.Unknown]), what words where.
func unplaced(where string, why chainWhy) error {
	return &holdingsError{why: fmt.Sprintf("%s holds a shape the model of Claude Code's resume cannot place "+
		"(%s), so what its resume hands its model is unknown", where, why)}
}

// idsFold files what line, a user or assistant record of the main thread whose
// message is message, says of the ids the request may fold into another
// call's ([holdingsWalk.foldsOf]), wherever the record lies and whoever's it
// is; and since a leg's id is known only once its call is read, a main thread
// holding a batch's list or a result of a batch's split id at all answers no
// leg ([holdingsWalk.mainEnd]). No reading where a batch's list is in no
// shape Claude Code writes: its request throws on one.
func (w *holdingsWalk) idsFold(line threadLine, message threadMessage) error {
	batch, split, err := w.foldsOf(line, message)
	if err != nil {
		return &holdingsError{why: fmt.Sprintf("line %d of the session's main thread lists a batch's calls in no "+
			"shape Claude Code writes", line.N), cause: err}
	}
	w.batched = w.batched || batch
	w.splitResults = w.splitResults || split
	return nil
}

// foldsOf files what line, a user or assistant record of a thread whose
// message is message, says of the ids its request may fold into another
// call's (the pairing rule's C1, [holdingsPairing]): each batch's call it
// lists, every one of which the request folds the split calls of that batch
// into (2.1.322's GWi @291388775, 2.1.310's x2s @244965716), and each result
// of a batch's split id, which it folds into the result of the call that id
// was split from (2.1.322's zWi @291389478, 2.1.310's P2s @244966419). Each
// such id that is asked is told. It answers whether line lists a batch's
// calls, whether it holds a result of a split id, and the error of a list in
// no shape Claude Code writes.
func (w *holdingsWalk) foldsOf(line threadLine, message threadMessage) (batch, split bool, err error) {
	batch, err = line.BatchToolUses(func(id string) {
		if f := w.calls[id]; f != nil {
			f.folds = true
		}
	})
	if err != nil || line.Head.Type != recordUser {
		return batch, false, err
	}
	for _, call := range message.Calls() {
		base, ok := splitBase(call.ID)
		if !ok {
			continue
		}
		split = true
		if f := w.calls[base]; f != nil {
			f.folds = true
		}
	}
	return batch, split, nil
}

// folded reports whether the request may fold id's call into another, by
// what the whole thread holds, chain's once End decided it: an id a batch's
// split call or result reads as split from another call's carried by a block
// of the thread ([splitBase]).
//
// AN ID CLAUDE CODE WRITES OF ITS OWN is C1's too, and needs no test here:
// the result of a call under one is a results record answering such an id,
// which is no plain one ([pairPlainResults]), and C4 pairs no call whose
// result is not plain, so the call is told on every order.
func folded(chain *threadChain, id string) bool {
	base, split := splitBase(id)
	if !split {
		return false
	}
	_, carried := chain.CallKey(base)
	return carried
}

// mainEnd decides, once the main thread is read whole into chain and pairing,
// which of its calls the request a resumed session sends pairs, and files it
// on each fact of the main thread: each asked call's and each leg's first use
// and first result by the pairing rule ([holdingsPairing]); a leg's own
// answer and each task notification only past the last restart
// ([holdingsWalk.live], THE LAST RESTART in the file comment); and a
// notification only where the resume hands its record under every variant.
// No reading where End's context ended, or where the model cannot place the
// thread ([threadChain.Unknown]).
func (w *holdingsWalk) mainEnd(ctx context.Context, chain *threadChain, pairing *holdingsPairing) error {
	if err := chain.End(ctx); err != nil {
		return err
	}
	if why, unknown := chain.Unknown(); unknown {
		return unplaced("the session's main thread", why)
	}
	var (
		asks  []*pairAsk
		calls []*callFacts
		legs  []*legFact
		ids   []string
	)
	for id, f := range w.calls {
		if f.uses > 0 && f.results > 0 && f.useThread == "" && f.resultThread == "" {
			asks = append(asks, &pairAsk{use: chainHandle(f.useRec), result: chainHandle(f.resultRec)})
			calls = append(calls, f)
			ids = append(ids, id)
		}
	}
	// A LEG'S CALL IS ONE USE AND ONE RESULT OF ITS ID over every user and
	// assistant record of the thread, whatever tool another use of the id
	// names and wherever a result of it lies: the request keeps the first use
	// of an id and the first result of it in the message after it (C1).
	type keyCount struct{ uses, results int }
	counts := map[chainCallID]*keyCount{}
	legIDs := make([]string, 0, len(w.legs))
	for id, leg := range w.legs {
		if leg.results == 0 {
			continue
		}
		asks = append(asks, &pairAsk{use: chainHandle(leg.callRec), result: chainHandle(leg.resultRec)})
		legs, legIDs = append(legs, leg), append(legIDs, id)
		if key, carried := chain.CallKey(id); carried && counts[key] == nil {
			counts[key] = &keyCount{}
		}
	}
	pairing.counts(chain, func(key chainCallID, use bool) {
		if c := counts[key]; c != nil && use {
			c.uses++
		} else if c != nil {
			c.results++
		}
	})
	pairing.pairs(chain, asks)
	for i, f := range calls {
		f.answered = asks[i].paired && f.bridged && !f.folds && !folded(chain, ids[i])
	}
	for i, leg := range legs {
		key, _ := chain.CallKey(legIDs[i])
		c := counts[key]
		leg.answered = asks[len(calls)+i].paired && c != nil && c.uses == 1 && c.results == 1 &&
			!w.batched && !w.splitResults && !folded(chain, legIDs[i]) && leg.resultLine > w.lastRestart
	}
	for key, notice := range w.notices {
		notice.live = chain.Handed(chainHandle(notice.record)) == handedYes && notice.line > w.lastRestart
		w.notices[key] = notice
	}
	return nil
}

// mainRecord files what line, a user or assistant record of the main thread
// whose message is message, its record in the model of the resume, says: each
// asked call's use or result, each leg's call and its first result, and a
// task notification.
//
// EVERY USER AND ASSISTANT RECORD OF THE FILE, chained or not, the main
// thread's own or not, before or after any restart: the resume loads each
// whatever its `isSidechain` or `agentId` says, and its request keeps the
// first use of an id and the first result after it, so a second carrier of an
// asked or a leg's id may stand in its place, and counting it tells the call
// ([holdingsWalk.mainEnd]). Nothing the probes' 521 thread files hold moves by
// it.
func (w *holdingsWalk) mainRecord(line threadLine, message threadMessage, record int32) error {
	calls := message.Calls()
	for _, call := range calls {
		w.count("", call, line.N, record)
	}
	if line.Head.Type == recordAssistant {
		for _, call := range calls {
			if err := w.leg(call, record); err != nil {
				return err
			}
		}
		return nil
	}
	alone := resultAlone(message)
	for _, call := range calls {
		w.legResult(line, call, alone, record)
	}
	return w.notification(line, message, record)
}

// resultAlone reports whether message, a user record's, is one result alone:
// THE RECORD'S OWN toolUseResult IS ONE CALL'S ONLY ON A RECORD OF THAT ONE
// RESULT ALONE, its one block. On a record of two blocks, two results or a
// result beside a text, it is the record's, and says nothing of either.
func resultAlone(message threadMessage) bool {
	return message.Blocks().Len() == 1 && len(message.Calls()) == 1
}

// count files call, a block of line n of thread ("" the main thread, else a
// subagent's id), record in that thread's chain, under its asked id: a use, or
// a result, the first result's facts kept.
func (w *holdingsWalk) count(thread string, call threadCall, n int, record int32) {
	facts := w.calls[call.ID]
	if facts == nil {
		return
	}
	if facts.threads == 0 || facts.last != thread {
		if facts.threads == 0 {
			facts.thread = thread
		}
		facts.threads++
		facts.last = thread
	}
	if thread != "" {
		w.agents[thread].namesAsked = true
	}
	switch call.Kind {
	case blockToolUse:
		facts.uses++
		if facts.uses == 1 {
			facts.useThread, facts.useRec = thread, record
			facts.bridged = strings.HasPrefix(call.Name, claudeBridgedToolPrefix)
		}
	case blockToolResult:
		facts.results++
		if facts.results > 1 {
			return
		}
		facts.resultThread, facts.line, facts.resultRec = thread, n, record
		facts.failed = call.Failed
		facts.textBytes, facts.opening, facts.whole = int(call.Text.Bytes()), call.Text.Opening, call.Text.Whole
	}
}

// leg files call, a model's call in the main thread, record in its chain, as a
// leg of subagents' work where it is an Agent, Task or SendMessage call.
//
// ONLY UNDER AN ID THE LOG KEEPS ([sandbox.ValidCallID]), the rule a
// metadata's `toolUseId` and a notification's leg are held to: nothing can
// link, answer or notice a leg under any other, and that id, as long as a
// line, would be held until the walk ends. So an Agent or Task call under one
// is kept as nothing, and a SendMessage under one keeps only its subagent
// ([holdingsWalk.sentUnnamed]), which stays unreported, the safe direction.
func (w *holdingsWalk) leg(call threadCall, record int32) error {
	switch call.Name {
	case claudeToolAgent, claudeToolTask, claudeToolSendMessage:
	default:
		return nil
	}
	if !sandbox.ValidCallID(call.ID) {
		if call.Name != claudeToolSendMessage {
			return nil
		}
		return w.sentUnnamed(call.To)
	}
	leg := w.legs[call.ID]
	if leg == nil {
		if err := w.keep(); err != nil {
			return err
		}
		leg = &legFact{callRec: record, resultRec: -1}
		w.legs[call.ID] = leg
	}
	if call.Name != claudeToolSendMessage {
		leg.spawns++
		return nil
	}
	leg.sends++
	if leg.sends == 1 {
		leg.to = call.To
	}
	return nil
}

// sentUnnamed files a SendMessage the main thread sent under an id the call-id
// rule refuses, to subagent to, "" where its input names none
// ([holdingsWalk.leg]): that subagent is one a reading does not report. Each
// subagent filed so is one more of what a reading keeps ([holdingsWalk.keep]).
func (w *holdingsWalk) sentUnnamed(to string) error {
	if to == "" || w.unnamed[to] {
		return nil
	}
	if err := w.keep(); err != nil {
		return err
	}
	w.unnamed[to] = true
	return nil
}

// keep takes one more of what a reading keeps of the main thread's legs (a
// leg's call, a notification filed under one, and a subagent sent a message
// under an id the call-id rule refuses), or says the main thread holds more
// than [holdingsMaxLegs].
func (w *holdingsWalk) keep() error {
	if w.kept == holdingsMaxLegs {
		return &holdingsError{why: fmt.Sprintf("the session's main thread holds more than %d legs of "+
			"subagents' work", holdingsMaxLegs)}
	}
	w.kept++
	return nil
}

// legResult files call, a result in line of the main thread, record in its
// chain, under the leg it answers, where that leg's call was read: its first
// result's record, its failure and, where its record holds that one result
// alone, what its own toolUseResult says.
func (w *holdingsWalk) legResult(line threadLine, call threadCall, alone bool, record int32) {
	leg := w.legs[call.ID]
	if leg == nil {
		return
	}
	leg.handback = leg.handback || line.HandsBack()
	leg.results++
	if leg.results > 1 {
		return
	}
	leg.resultRec, leg.resultLine, leg.failed = record, line.N, call.Failed
	if alone {
		leg.completed, leg.agent, leg.refused = line.AgentAnswer()
	}
}

// notification files line, a user record of the main thread whose
// message is message, record in its chain, as a task notification of a
// subagent's leg where it is one: a text whose origin says so, naming the
// subagent and a leg whose call was read ([threadMessage.Notification]), and
// whether it carries a handback ([threadMessage.NamesHandback]).
func (w *holdingsWalk) notification(line threadLine, message threadMessage, record int32) error {
	if !message.IsText() || originOf(line) != claudeTaskNotification {
		return nil
	}
	agent, leg, ok := message.Notification()
	if !ok || w.legs[leg] == nil {
		return nil
	}
	if message.NamesHandback() {
		w.legs[leg].handback = true
	}
	key := legNotice{agent: agent, leg: leg}
	if _, filed := w.notices[key]; !filed {
		if err := w.keep(); err != nil {
			return err
		}
	}
	w.notices[key] = noticeFact{record: record, line: line.N}
	return nil
}

// originOf is the kind line's origin names, "" where its origin does not
// decode. AN ORIGIN THAT DOES NOT DECODE IS NO NOTIFICATION, the one fact it
// could have been, and costs the reading nothing more, so its error is the
// answer "none" rather than the reading's.
func originOf(line threadLine) string {
	kind, err := line.OriginKind()
	if err != nil {
		return ""
	}
	return kind
}

// subagentThread reads subagent agent's thread, whose bytes member hands out,
// a whole line at a time, into a, the model of its own load ([threadChain],
// newThreadChain(agent)) and the pairing rule's marks ([holdingsPairing]), and
// once it is read whole, what rule 2 and the pairing rule read off the one
// conversation that load hands ([holdingsWalk.subagentEnd]).
//
// IT READS ON past every line that leaves the subagent not whole, the model's
// bound among them, so every record's calls are still counted: each is told
// under its heading, and a second carrier of an asked id still tells it. Only
// the line reader's own bounds, or a fault of the member's reader, end it.
func (w *holdingsWalk) subagentThread(ctx context.Context, agent string, a *subagentFacts,
	member io.Reader) error {

	lines := w.reader(member)
	chain, pairing := newThreadChain(agent), &holdingsPairing{}
	for {
		line, err := lines.Next(ctx)
		var broken *threadLineError
		switch {
		case errors.Is(err, io.EOF):
			// A TORN MEMBER IS NOT WHOLE: the box cuts a subagent's torn last
			// piece only where it has find, and the subagent's own load can
			// read a whole value with no break after it as one more record,
			// which this reader never hands out.
			if lines.Torn() {
				a.notWhole("its thread ends in a piece with no line break, which its own load may read as one "+
					"more record", nil)
			}
			return w.subagentEnd(ctx, agent, a, chain, pairing)
		case errors.As(err, &broken):
			// NOT WHOLE, WHATEVER THE LOAD MAKES OF IT: the model hands the
			// resume's truth of such a thread, but rule 2's ground is the live
			// run, which was handed the record the file lost.
			chain.Unread(broken)
			a.notWhole(fmt.Sprintf("line %d of its thread is no record", broken.N), broken.Err)
			continue
		case err != nil:
			return &holdingsError{why: "a subagent's thread was not read whole", cause: err}
		}
		w.subagentLine(agent, a, chain, pairing, line)
	}
}

// subagentLine reads line, one of subagent agent's thread, into a, chain and
// pairing.
//
// A RETRACTION LIST LEAVES IT NOT WHOLE, whatever it names: no load of a
// subagent's thread filters by one ([threadLine.Retracted]), so what its model
// was handed is not what its file holds. So does A REPLY ITS MODEL REFUSED:
// Claude Code takes the refused attempt's records back and moves to another
// model, and the subagent's file keeps that attempt and records no fallback
// (2.1.322's refusal lanes @296208832, 2.1.310's @249540334).
func (w *holdingsWalk) subagentLine(agent string, a *subagentFacts, chain *threadChain, pairing *holdingsPairing,
	line threadLine) {

	head := line.Head
	if why, cause := writerBelowFloor(line); why != "" {
		a.notWhole(fmt.Sprintf("line %d of its thread %s", line.N, why), cause)
	}
	if a.restarts.See(head) {
		a.restarted = true
		a.notWhole(fmt.Sprintf("its thread restarts at line %d", line.N), nil)
	}
	if head.AgentID != "" && head.AgentID != agent {
		a.notWhole(fmt.Sprintf("line %d of its thread is another agent's record", line.N), nil)
	}
	if err := line.ReplacedResults(w.replaced); err != nil {
		a.notWhole(fmt.Sprintf("line %d of its thread replaces results in no shape Claude Code writes", line.N),
			err)
	}
	// AN ENTRY BEFORE ANY FAULT TAKES RECORDS BACK: the list is read in order,
	// so one that names an entry before an entry in no shape takes back, and
	// only one whose first entry is in no shape, or which is none, is in no
	// shape.
	switch retracted, err := line.Retracted(); {
	case retracted.Entries > 0:
		a.notWhole(fmt.Sprintf("line %d of its thread takes records back out of its conversation, which no load "+
			"of a subagent's thread filters", line.N), nil)
	case err != nil:
		a.notWhole(fmt.Sprintf("line %d of its thread retracts records in no shape Claude Code writes", line.N),
			err)
	}
	record, message, err := chain.See(line)
	pairing.see(record, line, message)
	if head.Type != recordUser && head.Type != recordAssistant {
		return
	}
	if err != nil {
		a.notWhole(fmt.Sprintf("line %d of its thread is no record", line.N), err)
		return
	}
	if _, _, err = w.foldsOf(line, message); err != nil {
		a.notWhole(fmt.Sprintf("line %d of its thread lists a batch's calls in no shape Claude Code writes",
			line.N), err)
	}
	// EVERY USER AND ASSISTANT RECORD'S CALLS ARE COUNTED, its own or not,
	// handed or not, as the main thread's are ([holdingsWalk.mainRecord]): a
	// record that is not its own leaves the subagent not whole besides.
	for _, call := range message.Calls() {
		w.count(agent, call, line.N, int32(record))
	}
	if head.AgentID != agent || head.Sidechain == nil || !*head.Sidechain {
		a.notWhole(fmt.Sprintf("line %d of its thread is a %s record that is not its own", line.N, head.Type), nil)
		return
	}
	stop, stopErr := line.StopReason()
	if stopErr != nil {
		a.notWhole(fmt.Sprintf("line %d of its thread says no stop reason", line.N), stopErr)
	}
	if stop == claudeRefused {
		a.notWhole(fmt.Sprintf("line %d of its thread is a reply its model refused, whose attempt Claude Code "+
			"takes back out of the conversation it goes on with", line.N), nil)
	}
	apiError, apiErr := line.APIError()
	if apiErr != nil {
		a.notWhole(fmt.Sprintf("line %d of its thread marks an API error in no shape Claude Code writes", line.N),
			apiErr)
	}
	model, modelErr := line.Model()
	if modelErr != nil {
		a.notWhole(fmt.Sprintf("line %d of its thread names the model of its reply in no shape Claude Code writes",
			line.N), modelErr)
	}
	// LEG STARTS ARE COUNTED OVER THE WHOLE FILE (condition 4), every user
	// record of its own wherever it lies, and filed by record for condition 5,
	// which reads them along each order its own load hands.
	if head.Type == recordUser && legStart(message) {
		a.legStarts++
		pairing.flag(record, pairLegStart)
	}
	if text, _ := message.Says(); head.Type == recordAssistant && text && stopErr == nil && apiErr == nil &&
		modelErr == nil && finishedReply(message.Blocks(), stop, model, apiError) {
		pairing.flag(record, pairFinished)
	}
}

// subagentEnd decides, once subagent agent's thread is read whole into chain
// and pairing, whether a's thread is whole and every leg of it finished along
// the one conversation the subagent's own load hands ([wholeOf]),
// and files on each asked call whose use and result lie in it whether its
// request pairs them, by the pairing rule on every order that load hands
// ([holdingsPairing]). No reading where End's context ended.
func (w *holdingsWalk) subagentEnd(ctx context.Context, agent string, a *subagentFacts, chain *threadChain,
	pairing *holdingsPairing) error {

	if err := chain.End(ctx); err != nil {
		return err
	}
	wholeOf(a, chain, pairing)
	var (
		asks  []*pairAsk
		calls []*callFacts
		ids   []string
	)
	for id, f := range w.calls {
		if f.uses > 0 && f.results > 0 && f.useThread == agent && f.resultThread == agent {
			asks = append(asks, &pairAsk{use: chainHandle(f.useRec), result: chainHandle(f.resultRec)})
			calls, ids = append(calls, f), append(ids, id)
		}
	}
	pairing.pairs(chain, asks)
	for i, f := range calls {
		f.answered = asks[i].paired && f.bridged && !f.folds && !folded(chain, ids[i])
	}
	return nil
}

// wholeOf files on a, once its subagent's thread is read whole into chain and
// pairing, why its thread is not whole, else the first leg that ends on no
// finished reply along an order its own load hands (rule 2's conditions 2 and
// 5): the model of its load places it ([threadChain.Unknown]); THE STRICT
// RULE, every own record no own record continues, a compaction's boundary
// aside, is its load's leaf or handed ([threadChain.OwnLeaves]); and along
// every order ([threadChain.EachOrder]) each leg ends on a reply that finished
// it ([finishedReply]) and says something ([threadMessage.Says]), since a
// subagent answers with its last reply that says something (2.1.322's pkn
// @302170267 through t5t @302157349, 2.1.310's Owr @255311767 through POt
// @255299110). No answer turns on a variant: its load runs neither q1t nor a
// retraction filter, and the tails' two orders hand the same records.
func wholeOf(a *subagentFacts, chain *threadChain, pairing *holdingsPairing) {
	if why, unknown := chain.Unknown(); unknown {
		a.notWhole(fmt.Sprintf("the model of its own load cannot place it (%s)", why), nil)
		return
	}
	// WHERE ITS LOAD FINDS NO CONVERSATION, the leaf is none: every record of
	// its own is then handed to no model, so each of its own leaves fails the
	// strict rule, and the order, which holds no leg's start, condition 5.
	leaf, _ := chain.Leaf()
	for _, h := range chain.OwnLeaves() {
		if h != leaf && chain.Handed(h) != handedYes {
			a.notWhole("a record of its own that no record of its own continues is one its own load does not "+
				"hand: its file holds a branch of its conversation its model was not handed", nil)
			return
		}
	}
	chain.EachOrder(func(o chainOrder) bool {
		leg := unfinishedLeg(o, pairing)
		if leg != 0 && a.unfinished == 0 {
			a.unfinished = leg
		}
		return leg == 0
	})
}

// unfinishedLeg is the first leg of a subagent's work that ends on no
// finished reply along o, one order its own load hands, by the marks rule 2
// filed in pairing ([holdingsWalk.subagentLine]), counted from 1: the first
// where o's first user or assistant record starts none; 0 where every leg
// ends on one ([wholeOf]).
func unfinishedLeg(o chainOrder, pairing *holdingsPairing) int {
	leg, finished := 0, false
	for i := range o.Len() {
		mark := pairing.mark(o.At(i))
		switch {
		case mark&(pairUser|pairAssistant) == 0:
			continue
		case mark&pairLegStart != 0:
			if leg > 0 && !finished {
				return leg
			}
			leg++
		case leg == 0:
			return 1
		}
		finished = mark&pairFinished != 0
	}
	if !finished {
		return max(leg, 1)
	}
	return 0
}

// replaced files id, a call's whose result a content-replacement record says
// its harness hands its model replaced ([threadLine.ReplacedResults]), where
// that call is asked: its result is not live.
func (w *holdingsWalk) replaced(id string) {
	if facts := w.calls[id]; facts != nil {
		facts.replaced = true
	}
}

// notWhole says why a's thread is not whole, where nothing has yet.
func (a *subagentFacts) notWhole(why string, cause error) {
	if a.broken == "" {
		a.broken = whyOf(why, cause)
	}
}

// legStart reports whether message, a counted user record's of a subagent's
// thread, starts a leg of its work: anything but the results of its calls
// alone, so a text, a list of no blocks, or a block of another kind, the
// subagent's prompt and every message sent to it among them.
func legStart(message threadMessage) bool {
	if message.IsText() {
		return true
	}
	blocks := message.Blocks()
	return blocks.Len() == 0 || !blocks.All(blockToolResult)
}

// finishedReply reports whether an assistant record of blocks, stopped for
// stop and written by model, ends a leg: text and thinking blocks alone, at
// least one, with which its model ended its turn as Claude Code tests it
// (stop "end_turn", no apiError, a model other than "<synthetic>").
//
// "END_TURN" ALONE: a reply that wrote text then called a tool is two records,
// each stopped for the call, so a thread cut between them ends on text only
// the stop tells apart; one cut at "max_tokens" or refused ended no turn. An
// apiError reply is the harness's own to a failed request, after which it
// answers the starting call completed though no model read what that request
// carried. "<synthetic>" marks every reply the harness writes itself
// ([threadLine.Model]): none says "end_turn", but the test is its whole one.
func finishedReply(blocks threadBlocks, stop, model string, apiError bool) bool {
	if blocks.Len() == 0 || apiError || stop != claudeEndedTurn || model == claudeSyntheticModel {
		return false
	}
	return blocks.All(blockText, blockThinking, blockRedactedThinking)
}

// readSubagentMeta reads a subagent's metadata, whose bytes member hands out,
// into a: read whole within [holdingsMetaMaxBytes], and within what is left of
// the archive's budget.
func (w *holdingsWalk) readSubagentMeta(a *subagentFacts, member io.Reader) error {
	left := w.bounds.plain - w.handed
	raw, err := io.ReadAll(io.LimitReader(&handedReader{r: member, handed: &w.handed},
		min(holdingsMetaMaxBytes, left)+1))
	switch {
	case err != nil:
		return &holdingsError{why: "a subagent's metadata was not read whole", cause: err}
	case int64(len(raw)) > left:
		return &holdingsError{why: fmt.Sprintf("the session's archive hands a reading more than the %d "+
			"bytes it reads", w.bounds.plain)}
	}
	a.metaRead = true
	if len(raw) > holdingsMetaMaxBytes {
		a.unlinked = fmt.Sprintf("its metadata runs past %d bytes", holdingsMetaMaxBytes)
		return nil
	}
	a.meta, a.unlinked = subagentMetaOf(raw)
	return nil
}

// subagentMetaOf is what a subagent's metadata, raw, says of the call that
// started it, or why it says nothing: ONE JSON OBJECT, read by exact keys under
// the layer's options ([threadDecoding]), a key written twice by its last
// value: `toolUseId`, a string the call-id rule holds ([sandbox.ValidCallID]);
// `spawnDepth`, the integer 1 as JSON writes it; `requestShape`, a string;
// and `description`, a string or absent, cut to [holdingsDescriptionBytes].
// Other keys are passed over; a null, or another type, where one of those
// belongs says nothing.
func subagentMetaOf(raw []byte) (subagentMeta, string) {
	if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return subagentMeta{}, "its metadata is no JSON object"
	}
	var wire struct {
		ToolUseID    json.RawMessage `json:"toolUseId"`
		SpawnDepth   json.RawMessage `json:"spawnDepth"`
		RequestShape json.RawMessage `json:"requestShape"`
		Description  json.RawMessage `json:"description"`
	}
	if err := wireDecode(raw, &wire); err != nil {
		return subagentMeta{}, whyOf("its metadata does not decode", err)
	}
	var (
		meta subagentMeta
		err  error
	)
	if meta.description, err = wireString("description", wire.Description); err != nil {
		return subagentMeta{}, whyOf("its metadata's description is no string", err)
	}
	meta.description = cutDescription(meta.description)
	// AN ID THE CALL-ID RULE REFUSES IS NOT KEPT: it links nothing, and a
	// metadata's is up to its bound long, which every subagent's kept until
	// the walk ends would hold a thousand times over.
	if meta.call, err = wireRequiredString("toolUseId", wire.ToolUseID); err != nil ||
		!sandbox.ValidCallID(meta.call) {
		return subagentMeta{description: meta.description},
			"its metadata names the call that started it by no id the log keeps"
	}
	if !bytes.Equal(wire.SpawnDepth, []byte("1")) {
		return meta, "its metadata says no depth of 1"
	}
	shape, err := wireRequiredString("requestShape", wire.RequestShape)
	if err != nil {
		return meta, "its metadata says no request shape"
	}
	meta.foreground = shape == claudeForeground
	return meta, ""
}

// cutDescription is description cut to [holdingsDescriptionBytes] on a rune
// boundary: copied where it is cut, so what was cut away is not held until the
// walk ends.
func cutDescription(description string) string {
	if len(description) <= holdingsDescriptionBytes {
		return description
	}
	cut := holdingsDescriptionBytes
	for cut > 0 && !utf8.RuneStart(description[cut]) {
		cut--
	}
	return strings.Clone(description[:cut])
}

// failed is err, which ended the reading, as the reading answers it: its
// caller's own error where its caller left, whatever the stream made of the
// reading stopping under it; the reading's own words where it has them; and
// else the reading's words around it.
func (w *holdingsWalk) failed(ctx context.Context, err error) error {
	var own *holdingsError
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &own):
		return err
	}
	return &holdingsError{why: "the session's archive was not read whole", cause: err}
}

// holdings is what the reading found: every asked call some counted record
// names, live where its first result is ([holdingsWalk.live]), the resume
// hands its model its first use's record and then that result's, both of one
// thread ([callFacts.answered]), and no record replaced it
// ([callFacts.replaced]); and every subagent whose thread names one,
// reported or not (rule 2).
func (w *holdingsWalk) holdings() sandbox.SessionHoldings {
	calls := make(map[string]sandbox.CallRecords, len(w.calls))
	for id, f := range w.calls {
		if f.threads == 0 {
			continue
		}
		records := sandbox.CallRecords{Threads: f.threads, Uses: f.uses, Results: f.results, Failed: f.failed,
			TextBytes: f.textBytes, Opening: f.opening, Whole: f.whole}
		if f.threads == 1 {
			records.Thread = f.thread
		}
		records.Live = f.results > 0 && f.uses > 0 && f.useThread == f.resultThread && f.answered && !f.replaced &&
			w.live(f.resultThread, f.line)
		calls[id] = records
	}
	sends := w.sendsTo()
	subagents := map[string]sandbox.SubagentThread{}
	for agent, a := range w.agents {
		if !a.namesAsked {
			continue
		}
		why := w.unreported(agent, a, sends[agent])
		subagents[agent] = sandbox.SubagentThread{Reported: why == "", Why: why, Description: a.meta.description}
	}
	return sandbox.SessionHoldings{Read: true, Kind: sandbox.HoldingsByID, Calls: calls, Subagents: subagents}
}

// live reports whether a result at line of thread is one a continued session
// hands its model: in the main thread ("") where its line follows the last
// restart, and in a subagent's where that thread has no restart at all.
func (w *holdingsWalk) live(thread string, line int) bool {
	if thread == "" {
		return line > w.lastRestart
	}
	return !w.agents[thread].restarted
}

// sendsTo is, by subagent, the SendMessage legs of the main thread sent to it
// that its own result did not refuse, each by its call's id, in order.
func (w *holdingsWalk) sendsTo() map[string][]string {
	sends := map[string][]string{}
	for id, leg := range w.legs {
		if leg.sends > 0 && leg.to != "" && !leg.refused {
			sends[leg.to] = append(sends[leg.to], id)
		}
	}
	for _, ids := range sends {
		slices.Sort(ids)
	}
	return sends
}

// unreported is why rule 2 does not report subagent agent, whose facts are a
// and whose SendMessage legs are sends: the first of its five conditions that
// fails, in words for the log; "" where all five hold (the file comment).
func (w *holdingsWalk) unreported(agent string, a *subagentFacts, sends []string) string {
	// 1. LINKED.
	switch {
	case !a.metaRead:
		return "it is linked to no call: no metadata of it was read"
	case a.unlinked != "":
		return "it is linked to no call: " + a.unlinked
	}
	start := w.legs[a.meta.call]
	if start == nil || start.spawns != 1 {
		return fmt.Sprintf("it is linked to no call: its metadata names %s, which is not one Agent or Task "+
			"call of the main thread", a.meta.call)
	}
	// 2. WHOLE. A thread with no record naming an asked call never comes to
	// be asked: the subagent of a reading's answer is one a user or assistant
	// record of whose thread names one.
	if a.broken != "" {
		return "its thread is not whole: " + a.broken
	}
	// 3. EVERY LEG ANSWERED, LIVE.
	own := start.results == 1 && start.completed && start.agent == agent && !start.failed &&
		a.meta.foreground && start.answered && !start.handback
	if !own && !w.noticed(agent, a.meta.call) {
		return fmt.Sprintf("the call that started it, %s, has no live answer in the main thread", a.meta.call)
	}
	if w.unnamed[agent] {
		return "a message sent to it under an id the log keeps none of has no live answer in the main thread: " +
			"no notification can name it"
	}
	for _, send := range sends {
		if !w.noticed(agent, send) {
			return fmt.Sprintf("the message sent to it by %s has no live answer in the main thread", send)
		}
	}
	// 4. NO UNSEEN LEG.
	if legs := 1 + len(sends); a.legStarts > legs {
		return fmt.Sprintf("its thread starts %d legs of work, and the main thread holds %d", a.legStarts, legs)
	}
	// 5. EVERY LEG FINISHED.
	if a.unfinished != 0 {
		return fmt.Sprintf("leg %d of its work ends on no finished reply", a.unfinished)
	}
	return ""
}

// noticed reports whether the main thread holds a live task notification of
// subagent agent's leg: one the resume hands its model under every variant,
// past its last restart ([holdingsWalk.mainEnd]), of a leg no answer of which
// carries a handback ([legFact.handback]).
func (w *holdingsWalk) noticed(agent, leg string) bool {
	l := w.legs[leg]
	return w.notices[legNotice{agent: agent, leg: leg}].live && l != nil && !l.handback
}

// whyOf is why, and the first line of what gave it where something did,
// redacted and cut ([firstLine]), as a reading's error words it
// ([holdingsError]).
func whyOf(why string, cause error) string {
	if cause == nil {
		return why
	}
	return why + ": " + firstLine(cause.Error(), transcriptDetailLimit)
}

// handedReader is a read member's bytes, counted into handed as they are
// handed out: what the members a reading read have handed out between them,
// of the one budget they share ([threadPlainMax]).
type handedReader struct {
	r      io.Reader
	handed *int64
}

// Read reads the member, counting what it hands out.
func (h *handedReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	*h.handed += int64(n)
	return n, err
}
