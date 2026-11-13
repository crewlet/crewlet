package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
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

// WHICH OF A ROUND'S CALLS A CLAUDE CODE SESSION HOLDS, READ OFF ITS KEPT
// ARCHIVE ([Runner.ReadHoldings]).
//
// Claude Code files a model's call under the id it sent the bridge
// ([sandbox.BridgeCall.CallID]): the call's use in an assistant record, its
// result in a user record after it, in the thread of whoever made the call,
// the session's main thread or one of its subagents'. So a kept archive says,
// of each call a relaunch asks about, which records name it and in which
// thread ([sandbox.CallRecords]), and of each subagent whose thread names one,
// whether the session holds that subagent's work as done and answered
// ([sandbox.SubagentThread]); [sandbox.SessionHoldings.Holds] alone turns
// those facts into a verdict. The reading reads the session's main thread and
// each depth-one subagent's thread and metadata. A deeper thread (a
// Workflow's, a nested agent's) is a member it does not read, so a call
// recorded only there is absent from the reading, and told in the main list.
//
// THE WALK, member by member as the take's scrub wrote the archive
// ([eachMember]), the reading's context asked before each:
//
//   - every member's RAW name first: one named by an absolute path or through
//     a `..` element is no reading, whichever member it is, since a tar that
//     restores it can write over the file a resumed session reads
//     ([claudeMemberOf]);
//   - each name, cleaned, is named through the line layer and never by a rule
//     of the reading's own ([claudeMemberOf]): the session's main thread, and
//     each subagent's thread and metadata beside it, are read, and every other
//     member (another session's file, a deeper thread, `agent-.jsonl`, the
//     session's saved compaction under either of its names) is passed over,
//     its bytes read through the archive's meter as tar moves past it;
//   - a member the reading reads, held twice under two spellings, is no
//     reading, since tar restores the last copy and the reading could have
//     read the other; a main thread that is no regular file is no reading, as
//     the box refuses one, and a subagent's member that is no regular file is
//     passed over, which leaves that subagent unread or unlinked;
//   - more subagents named than [holdingsMaxAgents] is no reading;
//   - a thread is read a whole line at a time ([threadReader]), the context
//     asked between lines, every head fed to that thread's own restarts
//     ([threadRestarts.See]), and every counted record's message decoded
//     ([threadLine.Message]): in the main thread a user or assistant record,
//     chained and the main thread's own ([threadHead.Main]); in subagent A's
//     thread one chained, of A's `agentId` and `isSidechain` true. Every other
//     line counts for nothing, but its user, assistant, attachment and system
//     records are filed by their uuid for the walk of the thread's chain
//     ([threadChain]), which is decided once the thread is read whole;
//   - a subagent's metadata is one JSON document, read whole within
//     [holdingsMetaMaxBytes] and decoded by the reading itself
//     ([subagentMetaOf]).
//
// MEMBER ORDER IS THE BOX'S: a subagent's thread can come before the main
// thread, and its metadata before its thread or after it. So every fact is
// gathered thread by thread, and nothing is decided before the stream has
// ended. A result, an answer or a notification is filed only under a fact
// that already exists: an asked call's, which exists from the start, or a
// leg's, once the main thread's call of it was read ([legFact]). One read
// before its call answers nothing and is kept nowhere: no thread measured
// writes one so, and a leg left unanswered leaves its subagent unreported, the
// safe direction. So no fact grows with a thread's results.
//
// A SAVED COMPACTION IS NO QUESTION OF THE READING'S ([claudePrecompact]): it
// bears on whether a continuation's first request hands its model a summary in
// place of the history the archive holds, which a reading's caller is to judge
// before it uses a reading at all (ADR-0073's amendment on this reading).
//
// TWO METERS, ONE BUDGET. The archive's gzip stream is metered at
// [threadPlainMax] by the scrub's own meter ([boundedPlain]), which also bounds
// what tar passes over unread; and each member the reading reads is handed
// what is left of that budget once what the members before it handed out is
// counted ([handedReader]), so the reading's bound is the reader's own,
// whatever members an archive holds: a sparse member hands out holes its
// stream does not hold, which only the second meter sees.
//
// THE STREAM IS READ TO ITS END BEFORE ANYTHING IS DECIDED. What the session
// store streams is provisional until io.EOF, and tar stops at its end of
// archive, so the reading drains the stream itself. gzip may already have read
// past tar's end and met the store's fault there: the drain meets it again
// because the store's stream latches its error. The gzip trailer is not
// checked: every byte under it came from a chunk the store authenticated.
//
// LIVE ONLY WHERE THE RESUME HANDS IT ITS MODEL (claudechain.go): a call's use
// and its result, and a leg's answer, count only where Claude Code's resume
// hands the record over, on the chain it walks from its leaf or recovered off
// it through the id of a reply on it, as 2.1.310 and 2.1.322 build what a
// resumed session's model is handed ([threadChain.handed]); never by where a
// record lies in the file, since an attempt the harness withdrew can lie past
// every restart and be handed to no model. And only past the thread's last
// restart: a result before one is told, the safe direction, a compaction's
// preserved segment included. Measured (probe T, both releases): a
// continuation hands its model each whole-line result of its main thread the
// chain or its recovery holds after its last restart, verbatim and with its
// call, the results of parallel calls off the chain among them. In a
// subagent's thread a result is live where the thread has no restart at all
// and lies on the chain from its last record of its own, or is recovered
// beside it: a restart there leaves the subagent unreported (rule 2).
//
// AND A RESULT IS LIVE ONLY PAST ITS CALL'S USE, IN ITS OWN THREAD: what a
// continuation sends pairs a result with the call before it in the one
// conversation, and the API refuses a result no call of its own precedes, so
// a result whose call's use comes after it, or lies in another thread, is
// handed to no model with that call. No harness writes either.
//
// A RESULT ITS HARNESS REPLACED IS NOT LIVE, in whichever thread: behind a
// flag Anthropic's servers set, Claude Code hands its model a preview in
// place of the largest of the results a reply's calls returned past a budget,
// records the replacement beside the result's own record, which keeps the
// whole output, and hands it again on resume ([threadLine.ReplacedResults]).
// So a result such a record names was never handed whole, and its call is
// told, which is no more than telling again where a continuation's flag is
// off. A record that replaces in no shape Claude Code writes is no reading in
// the main thread, and leaves a subagent's thread not whole.
//
// NOR IS A RECORD A RETRACTION NAMES, in whichever thread
// ([threadLine.Retracted]): where a model's reply is refused as it streams and
// Claude Code moves the request to another model, or it retries a reply whose
// call it could not parse, it takes that attempt's records back out of the
// conversation and names them, and a resume drops every record so named,
// whatever chain it lies on (claudechain.go). The live run dropped it too
// before it went on, so a subagent's model had not seen it either. A record
// that retracts in no shape Claude Code writes is no reading in the main
// thread, and leaves a subagent's thread not whole.
//
// RULE 4, THE LOGGED RESULT: a call is held only through its one result, whose
// is_error is the log's failure and whose text ([resultText]) is the log's
// output: every byte of it where the log kept the output whole, and else as
// long as it and opening alike ([sandbox.HoldingsOpeningBytes]), compared by
// digests the reading keeps of the text, its whole where it is no longer than
// the log keeps an output whole ([sandbox.CallRecords.Whole]); a result whose
// content is no string and no one text block has no text a log's output can
// match. Rule 3 is met by it: a result written by a process that never saw
// the call's outcome (a resume's for a call it found open, a stopped harness's
// own, the text of a call its harness gave up on) never carries the log's
// text, so no mark such a process writes is read.
//
// RULE 2, A SUBAGENT CUT OFF WITH THE SESSION. A call in a subagent's thread
// is one the continued session holds only through that subagent's answer in
// the main thread: so the reading REPORTS a subagent ([subagentFacts]) only
// where all five of these hold, each read off the records alone.
//
//  1. LINKED: its metadata is one object, read by exact keys, naming the call
//     that started it (`toolUseId`, an id the log keeps), a depth of exactly
//     the integer 1, a request shape, and a description or none; and that
//     call is exactly one counted Agent or Task call of the main thread.
//  2. WHOLE: no line of its thread is no record, or a progress record; every
//     user and assistant record is its own, its `agentId` and `isSidechain`
//     true, and its message, its stop reason, its mark of an API error and
//     the model of its reply decode; no record names another agent; and its
//     thread never restarts, so its first chained record roots its chain.
//  3. EVERY LEG ANSWERED, LIVE IN THE MAIN THREAD. Its legs are the call that
//     started it, and every SendMessage call of the main thread to it, but one
//     its own result refused (`success`, the literal false); a SendMessage
//     whose result is missing stays a leg, and one sent under an id the log
//     keeps none of is a leg nothing can answer ([holdingsWalk.leg]). The
//     call that started it is answered by its own result's record (`status`
//     "completed" naming it, no error, of a subagent its metadata says the
//     call waited for), the resume handing its model both that record and
//     the call's, or by a task notification of it on the main thread's
//     chain; a SendMessage only by a task notification of it on that chain.
//     A NOTIFICATION IS AN ANSWER: a continuation hands its model the
//     notification's record verbatim (probe T, T10, on 2.1.310 and 2.1.322),
//     and its lines name the subagent and the leg ([taskNotification]). An
//     answer is read off a record of that one result alone, since a record's
//     own toolUseResult is that record's, never one call's of several.
//  4. NO UNSEEN LEG: its thread starts no more legs (a user record that is not
//     the results of its calls alone, its prompt among them) than the main
//     thread gave it; a message the main thread holds no call of, a skill's
//     text among them, is one more.
//  5. EVERY LEG FINISHED: each stretch of its thread from one leg's start to
//     the next ends on a reply of text or thinking alone with which its model
//     ended its turn: its stop reason "end_turn", no API error and a model
//     other than "<synthetic>", Claude Code's own whole test of a reply with
//     which its model ended its turn. A reply that wrote text and then called
//     a tool is two records, the text's first, each saying it stopped for the
//     call: a thread cut between the two ends on text, which only the stop
//     reason tells from a finished reply. A request that failed for good
//     leaves a reply the harness wrote itself, the error's text, stopped at
//     "stop_sequence" and marked an API error ([threadLine.APIError]), on
//     which the harness ends the subagent and answers the call that started
//     it as completed: no model read what that request carried, a call's
//     result among it. And every reply the harness writes itself in place of
//     a model's is of the model "<synthetic>" ([threadLine.Model]), which its
//     test passes over whatever it says of its stop.
//
// A subagent not reported says which condition failed first, in words for
// the log ([sandbox.SubagentThread.Why]). An origin, an answer's toolUseResult
// or a SendMessage's `to` that does not decode costs only the fact it could
// have been: no notification, no answer, a send to no subagent, each the safe
// direction, and condition 4 the backstop of the last.
//
// EVERY OUTCOME. A reading: the stream read to its end within every bound, and
// each asked call's records counted. No reading, whole, and never a refusal of
// the relaunch, where the session's id names no thread; the ask asks about no
// call, holds more than a round's log lists, or asks about a call whose id is
// none the log keeps or is carried by another call of the ask; its writer is
// older than [holdingsWriterFloor], or unknown; its caller left while it
// waited for its slots, or between members or lines; the archive did not open,
// is no gzip or no tar, or inflates past its bound; a member is named
// absolutely or through `..`; a member it reads is held twice; the main thread
// is no regular file, or is not there; a line of it is past its bound or is no
// record, a user or assistant record's message does not decode, a record
// replaces results or retracts records in no shape Claude Code writes, or a
// progress record, which Claude Code's load drops and links the records after
// it past ([recordProgress]), is among them; the main thread's leaf may
// not be the one the reading walks from (a checkpoint a rewind, a withdrawal
// or a fork marked, or that names its leaf in no shape Claude Code writes, or
// that names a record its last record does not descend from; a sidechain's
// record, a fork's briefing, a file's snapshot or a reply whose content is a
// string in it), or its chain cannot be walked as the resume walks it (a
// record of no lowercase canonical uuid, a uuid held twice, or more than
// [holdingsMaxRecords] records); a subagent's thread is past its bound, or its
// metadata past the archive's budget; more subagents than [holdingsMaxAgents],
// or more legs than [holdingsMaxLegs]; or the stream does not end at io.EOF.
// The error says which, in words a relaunch logs ([holdingsError]). WHY A
// BOUND PASSED IN A SUBAGENT'S THREAD ENDS THE WHOLE READING: a call's
// threads, uses and results are counted over the whole archive, and a reading
// cut part way vouches for none of them.
//
// THE BOUNDS, AND WHAT A READING HOLDS. The archive's plaintext at
// [threadPlainMax], what its read members hand out at the same budget, a line
// at [threadLineMax], a thread's records at [holdingsMaxRecords], a metadata
// at [holdingsMetaMaxBytes], the asked calls at [sandbox.MaxBridgeCalls],
// subagents at [holdingsMaxAgents], legs at [holdingsMaxLegs], and the writer
// at its floor. At its worst a reading holds the line reader's buffer; one
// line and the slice that gathered it; the line's raw keys and its message's
// decode, each at most a line; what the message's calls keep (a line of the
// smallest results keeps about three times the line); one result's text; a
// metadata; the chain of the thread it reads, one thread's at a time (each
// record's uuid filed in a map, its parent and its marks, the uuid of a parent
// that came later, each reply's id as a digest, and the first ten bytes of
// each uuid its records retract: [threadChain]); and its
// facts: a few words and an id of at most [sandbox.MaxBridgeCallIDBytes] for
// each asked call, each leg and each notification, and for each subagent a few
// more and a description of at most [holdingsDescriptionBytes], about 2 MiB
// at the bounds. EVERY ID IT KEEPS IS ONE THE CALL-ID RULE HOLDS: a call under
// any other is kept as no leg ([holdingsWalk.leg]), and a metadata naming one
// keeps none ([subagentMetaOf]), since an id is the box's word, as long as a
// line, and nothing can link, answer or notice a call under one. MEASURED on
// two cores (BenchmarkAReadingAtItsBounds): its worst for one line is a line
// at the line's bound of the smallest results a message holds, 88 MiB live,
// its heap peaking at 94 MiB, read in a fifth of a second; its worst for the
// chain is a thread at [holdingsMaxRecords], each record a reply of its own id
// naming a parent that comes later, 35 MiB live, its heap peaking at 69 MiB,
// read in 1.4 s; so a thread that held both would hold about 120 MiB by their
// sum (computed, not measured), within what a seat's read for words, the other
// taker of the seats' one slot, holds at its worst
// ([SeatTranscriptReadsAtOnce]). 255 MiB of ordinary lines, the plaintext's
// bound, read in 1.8 s holding 9 MiB live; the slowest lines measured for
// their size, a line at the line's bound of blocks of a kind no reader names,
// 8 MiB in 0.30 s, so an archive of them to the plaintext's bound, which gzip
// shrinks to almost nothing, reads in about 10 s by that rate (computed, not
// measured whole); 1,024 subagents in 81 ms; 96 Agent calls under ids of a
// mebibyte and 1,024 metadata naming ids of 65,000 bytes, which held 97.7 and
// 65.3 MiB live before the reading kept no such id, in 0.52 s and 0.15 s
// holding 6 and 2 MiB live; and an ordinary kept session, a 1.6 MiB archive,
// in 52 ms.

// holdingsWriterFloor is the oldest Claude Code release whose sessions a
// reading reads ([ClaudeCode.holdingsFloor]).
//
// 2.1.310, THE OLDEST RELEASE THE READING'S RULES WERE MEASURED ON: the probes
// that settled them (T among them) ran 2.1.310 and 2.1.322. The reading
// decides from the shape of what a release recorded, and a release older than
// any measured could record a restart in a shape the restart rule does not
// know, which would read as none and hold a result its continuation is not
// handed. Not the carry floor a writer is held to ([claudeCarriedWriterFloor],
// 2.1.300), which says what another box can continue: a question of the
// harness's resume, not of how its records read. A writer below it, none
// recorded, and one whose release this code cannot read, a number past what
// an int holds among it, are no reading ([readsRelease]). The carry gate
// ([ClaudeCode.Continues]) takes a release it cannot read for a current one
// on purpose ([releaseAtLeast]), failing open toward continuing a session,
// where the reading fails closed, because what it counts held is never told
// again.
const holdingsWriterFloor = "2.1.310"

// holdingsMaxAgents is the most subagents a reading reads the members of.
//
// 1,024, AS MANY CALLS AS A PERSON'S READ REMEMBERS WHILE THEIR RESULTS ARE TO
// COME ([maxPendingCalls]): the probes ran at most two subagents a session,
// and a real corpus's main threads (probe P6) started 64 at depth one in all.
// Each costs a reading a few words and a description of at most
// [holdingsDescriptionBytes]. Past it, no reading.
const holdingsMaxAgents = 1024

// holdingsMaxLegs is the most legs a reading keeps of the main thread: its
// Agent, Task and SendMessage calls, each task notification it files under
// one, and each subagent sent a message under an id the call-id rule refuses.
//
// FOUR FOR EACH SUBAGENT A READING READS ([holdingsMaxAgents]): a real
// corpus's main threads (probe P6) made 50 Agent calls in all. Each costs a
// reading a few words and ids the call-id rule holds, at most
// [sandbox.MaxBridgeCallIDBytes] each: a call under any other id is kept as no
// leg ([holdingsWalk.leg]). Past it, no reading.
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
// ledger.ValueLimit, 200 runes, at most 800 bytes, so a kibibyte keeps at
// least 256 runes, and a longer description still reads as longer. Kept whole,
// every metadata's description would be held until the walk ends, 1,024 of
// 64 KiB; cut, a mebibyte at most.
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
	// a subagent's end ([taskNotification]).
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
	// claudeSyntheticModel is the model of a reply Claude Code writes itself
	// in place of a model's ([threadLine.Model]), which finishes no
	// subagent's leg whatever it says of its stop ([finishedReply]).
	claudeSyntheticModel = "<synthetic>"
)

// holdingsFloor is [holdingsWriterFloor].
func (ClaudeCode) holdingsFloor() string { return holdingsWriterFloor }

// checkHoldings holds ask, and the session archive names, to what a reading
// answers, before anything is opened: a session whose id names a thread
// ([claudeMainThread]), and an ask of 1 to [sandbox.MaxBridgeCalls] calls, at
// least one of them asked, each asked call's id one the log keeps
// ([sandbox.ValidCallID]) and carried by no other call of the ask, asked or
// not.
//
// AN ID ANOTHER CALL CARRIES IS NO READING, NEVER A GUESS: the session files
// both calls' records under the one id, so no record says whose it is, and an
// ask that holds two such calls is a telling that broke its own rule.
func (ClaudeCode) checkHoldings(archive sandbox.SessionArchive, ask sandbox.HoldingsAsk) error {
	if _, ok := claudeMainThread(archive.SessionID); !ok {
		return &holdingsError{why: "the session's id names no Claude Code thread"}
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
	// read have handed out of bounds.plain between them; records is the most
	// records of one thread it walks the chain of ([holdingsMaxRecords]).
	bounds  scrubBounds
	handed  int64
	records int
	// read is every member read, by its cleaned name: a member named twice is
	// one the reading may have read in place of the one a resume reads.
	read map[string]bool
	// calls are the asked calls' facts, by id.
	calls map[string]*callFacts
	// restarts follows the main thread's heads, and lastRestart is the line
	// of its last restart, 0 where it has none.
	restarts    threadRestarts
	lastRestart int
	// checkpoint is the uuid the main thread's checkpoint names, where
	// checkpointed: the leaf of its last last-prompt record naming one since
	// its last compaction's boundary ([holdingsWalk.checkpointOf]); compacted
	// is whether it holds such a boundary.
	checkpoint              [16]byte
	checkpointed, compacted bool
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
	// useThread, useLine and useRec are the thread, the line and the record
	// in that thread's chain of its first use, and resultThread, line and
	// resultRec its first result's; failed, textBytes, opening and whole are
	// that result's ([sandbox.CallRecords]).
	useThread    string
	useLine      int
	useRec       int32
	resultThread string
	line         int
	resultRec    int32
	failed       bool
	textBytes    int
	opening      [32]byte
	whole        [32]byte
	// useHanded and resultHanded are whether the resume hands its model the
	// first use's record and the first result's ([threadChain.handed]),
	// filed once their thread was read whole.
	useHanded, resultHanded bool
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
	// legStarts is how many legs its thread starts; open whether a leg's
	// stretch has a counted record, and finished whether that stretch's last
	// one is a finished reply so far; unfinished is the first leg whose
	// stretch ended on none, 0 where none did.
	legStarts      int
	open, finished bool
	unfinished     int
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
	// results is how many counted results name it; resultRec, failed,
	// completed, agent and refused are the first's: its record, its is_error,
	// and what its record's own toolUseResult says where the record holds that
	// one result alone ([agentAnswer]).
	results   int
	resultRec int32
	failed    bool
	completed bool
	agent     string
	refused   bool
	// answered is whether the resume hands its model the first call's record
	// and the first result's ([threadChain.handed]), filed once the main
	// thread was read whole.
	answered bool
}

// legNotice is a task notification's subagent and leg.
type legNotice struct{ agent, leg string }

// noticeFact is a task notification as the main thread holds it: its record
// in the main thread's chain, and live, whether the resume hands its model
// that record, filed once the main thread was read whole.
type noticeFact struct {
	record int32
	live   bool
}

// newHoldingsWalk is a reading of session's archive, within bounds, for the
// calls ask marks asked.
func newHoldingsWalk(session string, ask sandbox.HoldingsAsk, bounds scrubBounds) *holdingsWalk {
	main, _ := claudeMainThread(session)
	w := &holdingsWalk{session: session, main: main, bounds: bounds, records: holdingsMaxRecords,
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
// whole line at a time, and once it is read whole, which of its records the
// resume hands its model ([holdingsWalk.mainEnd]).
func (w *holdingsWalk) mainThread(ctx context.Context, member io.Reader) error {
	lines := newThreadReader(&handedReader{r: member, handed: &w.handed}, w.bounds.plain-w.handed)
	chain := newThreadChain(w.records)
	for {
		// BETWEEN LINES, since a line's read takes no context: a reading
		// overruns its caller's wait by one line's read and decode at most.
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := lines.Next()
		switch {
		case errors.Is(err, io.EOF):
			return w.mainEnd(chain)
		case err != nil:
			return &holdingsError{why: "the session's main thread was not read whole", cause: err}
		}
		restart := w.restarts.See(line.Head)
		if err = line.ReplacedResults(w.replaced); err != nil {
			return &holdingsError{why: fmt.Sprintf("line %d of the session's main thread replaces results in no "+
				"shape Claude Code writes", line.N), cause: err}
		}
		if err = line.Retracted(chain.retract); errors.Is(err, errChainBound) {
			return &holdingsError{why: fmt.Sprintf("line %d of the session's main thread is a record the reading "+
				"cannot walk to as the resume does", line.N), cause: err}
		} else if err != nil {
			return &holdingsError{why: fmt.Sprintf("line %d of the session's main thread retracts records in no "+
				"shape Claude Code writes", line.N), cause: err}
		}
		if err = w.checkpointOf(line); err != nil {
			return err
		}
		message, record, err := w.mainLine(chain, line)
		if err != nil {
			return err
		}
		if restart {
			w.lastRestart = line.N
			chain.restart()
		}
		if head := line.Head; (head.Type != recordUser && head.Type != recordAssistant) || !head.Chained() ||
			!head.Main() {
			continue
		}
		if err = w.mainRecord(line, message, record); err != nil {
			return err
		}
	}
}

// mainLine files line, one of the main thread's, in chain where it is a user,
// assistant, attachment or system record, and answers its record there, -1
// where it is none, and a user or assistant record's message. No reading
// where it is a record Claude Code's resume keeps out of a session's last
// record's place (a sidechain's, a fork's briefing, a file's snapshot), a
// user or assistant record whose message does not decode, which its load
// drops, or a reply whose content is a string, which its load drops where it
// is blank and rewrites where it is not; where it is a progress record, whose
// uuid that load hands each record after it that names it to the progress
// record's own parent, a link the reading does not follow
// ([recordProgress]); or where chain cannot walk to it ([threadChain.add]). No
// main thread the probes kept holds any of these.
func (w *holdingsWalk) mainLine(chain *threadChain, line threadLine) (threadMessage, int32, error) {
	refused := func(what string, cause error) error {
		return &holdingsError{why: fmt.Sprintf("line %d of the session's main thread %s", line.N, what), cause: cause}
	}
	head := line.Head
	var (
		message threadMessage
		marks   uint8
		err     error
	)
	switch {
	case head.Type == recordProgress:
		return threadMessage{}, -1, refused("is a progress record, which Claude Code's load drops, handing each "+
			"record after it that names it to its parent, a link the reading does not follow", nil)
	case head.Type != recordUser && head.Type != recordAssistant && head.Type != recordAttachment &&
		head.Type != recordSystem:
		return threadMessage{}, -1, nil
	case head.Sidechain != nil && *head.Sidechain:
		return threadMessage{}, -1, refused("is a sidechain's record, which the resume keeps out of a session's "+
			"last record's place", nil)
	case head.Type == recordSystem && head.Subtype == subtypeFileSnapshot:
		return threadMessage{}, -1, refused("is a file's snapshot, which the resume keeps out of a session's "+
			"last record's place", nil)
	case head.Type == recordAttachment:
		// AN ATTACHMENT IN NO SHAPE IS NO BRIEFING, as the harness's own test
		// of one reads it ([threadLine.AttachmentType]).
		if kind, kindErr := line.AttachmentType(); kindErr == nil && kind == attachmentForkBriefing {
			return threadMessage{}, -1, refused("is a fork's briefing, which the resume keeps out of a "+
				"session's last record's place", nil)
		}
	default:
		if message, err = line.Message(); err != nil {
			return threadMessage{}, -1, refused("is no record", err)
		}
		if _, text := message.Text(); text && head.Type == recordAssistant {
			return threadMessage{}, -1, refused("is a reply whose content is a string, which the resume drops "+
				"or rewrites", nil)
		}
		marks = markUserOrAssistant
		if head.Type == recordUser && len(message.Calls()) > 0 {
			marks |= markResults
		}
	}
	record, err := chain.add(line, false, marks)
	if err != nil {
		return threadMessage{}, -1, refused("is a record the reading cannot walk to as the resume does", err)
	}
	// A REPLY'S ID THAT DOES NOT DECODE costs only what it could recover: the
	// reply recovers no record off the chain, and none recovers it.
	if head.Type == recordAssistant && head.Chained() && head.Main() {
		if id, idErr := line.MessageID(); idErr == nil && id != "" {
			chain.reply(record, id)
		}
	}
	return message, record, nil
}

// checkpointOf follows the main thread's checkpoint as Claude Code's load
// follows it ([threadLine.Checkpoint]): a compaction's boundary clears it,
// and the last last-prompt record since that names a leaf sets it, one
// naming no lowercase canonical UUID setting it to name no record. No reading
// where one is marked by a rewind, a withdrawal or a fork, or names its leaf
// in no shape Claude Code writes: each can move the leaf the resume starts
// from wherever it names, or to nothing.
func (w *holdingsWalk) checkpointOf(line threadLine) error {
	if line.Head.Type == recordSystem && line.Head.Subtype == subtypeCompactBoundary {
		w.checkpoint, w.checkpointed, w.compacted = [16]byte{}, false, true
		return nil
	}
	leaf, marked, err := line.Checkpoint()
	switch {
	case err != nil:
		return &holdingsError{why: fmt.Sprintf("line %d of the session's main thread names the leaf its resume "+
			"starts from in no shape Claude Code writes", line.N), cause: err}
	case marked:
		return &holdingsError{why: fmt.Sprintf("line %d of the session's main thread is a checkpoint a rewind, a "+
			"withdrawal or a fork marked, which can move the leaf its resume starts from", line.N)}
	case leaf != "":
		w.checkpoint, w.checkpointed = canonicalUUID(leaf)
	}
	return nil
}

// mainEnd decides, once the main thread is read whole, which of its records
// the resume hands its model, and files it on each fact of the main thread
// (claudechain.go's file comment): the chain from the leaf, the nearest user
// or assistant record up from the thread's last record, past its last
// restart. No reading where the checkpoint names a record the last record
// does not descend from, since the resume then starts from the checkpoint;
// the checkpoint the leaf where the thread holds a compaction's boundary and
// it is a user or assistant record other than that one; and nothing handed
// where no user or assistant record up from the last record lies past the
// last restart.
func (w *holdingsWalk) mainEnd(chain *threadChain) error {
	chain.resolve()
	if last := int32(len(chain.parent)) - 1; last >= 0 {
		userOrAssistant := func(record int32) bool { return chain.marks[record]&markUserOrAssistant != 0 }
		leaf, _ := chain.up(last, true, userOrAssistant)
		if checkpoint, named := chain.index[w.checkpoint]; leaf >= 0 && w.checkpointed && named {
			// THE WHOLE WALK, past the last restart too, as the harness's own
			// test of a last record that descends from its checkpoint walks.
			if found, end := chain.up(last, false, func(record int32) bool { return record == checkpoint }); found < 0 {
				return &holdingsError{why: "the session's main thread holds a checkpoint its last record does not " +
					"descend from, its walk up ending at " + end.String() + ", so its resume starts there"}
			}
			if w.compacted && userOrAssistant(checkpoint) && checkpoint != leaf {
				leaf = checkpoint
			}
		}
		chain.done()
		if leaf >= 0 {
			chain.walk(leaf, true)
		}
	}
	chain.done()
	for _, f := range w.calls {
		if f.uses > 0 && f.useThread == "" {
			f.useHanded = chain.handed(f.useRec)
		}
		if f.results > 0 && f.resultThread == "" {
			f.resultHanded = chain.handed(f.resultRec)
		}
	}
	for _, leg := range w.legs {
		leg.answered = leg.results > 0 && chain.handed(leg.callRec) && chain.handed(leg.resultRec)
	}
	for key, notice := range w.notices {
		notice.live = chain.handed(notice.record)
		w.notices[key] = notice
	}
	return nil
}

// mainRecord files what line, a counted record of the main thread whose
// message is message, its record in the main thread's chain, says: each asked
// call's use or result, each leg's call and its first result, and a task
// notification.
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
	// THE RECORD'S OWN toolUseResult IS ONE CALL'S ONLY ON A RECORD OF THAT
	// ONE RESULT ALONE: on a record of two it is the record's, and says
	// nothing of either call.
	alone := len(message.Kinds()) == 1 && len(calls) == 1
	for _, call := range calls {
		w.legResult(line, call, alone, record)
	}
	return w.notification(line, message, record)
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
			facts.useThread, facts.useLine, facts.useRec = thread, n, record
		}
	case blockToolResult:
		facts.results++
		if facts.results > 1 {
			return
		}
		facts.resultThread, facts.line, facts.resultRec = thread, n, record
		facts.failed, facts.textBytes = call.IsError != nil && *call.IsError, -1
		if text, ok := resultText(call.Content); ok {
			facts.textBytes = len(text)
			facts.opening = sha256.Sum256([]byte(text[:min(sandbox.HoldingsOpeningBytes, len(text))]))
			// WHOLE ONLY WHERE A LOG COULD HAVE KEPT IT WHOLE: a longer text
			// is compared by its opening alone, so its whole is never hashed.
			if len(text) <= sandbox.MaxBridgeOutputBytes {
				facts.whole = sha256.Sum256([]byte(text))
			}
		}
	}
}

// leg files call, a model's call in the main thread, record in its chain, as a
// leg of subagents' work where it is one: an Agent or Task call, or a
// SendMessage call.
//
// ONLY UNDER AN ID THE LOG KEEPS ([sandbox.ValidCallID]), the rule a
// metadata's `toolUseId` and a notification's leg are held to, so a leg under
// any other id is one nothing can link, answer or notice; and an id is the
// box's word, as long as a line, which a leg filed under it would hold until
// the walk ends. So an Agent or Task call under such an id is kept as nothing,
// its subagent linked to no call; and a SendMessage under one keeps only the
// subagent it was sent to ([holdingsWalk.sentUnnamed]), which it leaves
// unreported: its result is filed under that id too, so not even a refusal of
// it is read, and that subagent's calls stay told, the safe direction.
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
		return w.sentUnnamed(sendTarget(call.Input))
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
		leg.to = sendTarget(call.Input)
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
	leg.results++
	if leg.results > 1 {
		return
	}
	leg.resultRec, leg.failed = record, call.IsError != nil && *call.IsError
	if alone {
		leg.completed, leg.agent, leg.refused = agentAnswer(line.ToolUseResult())
	}
}

// notification files line, a counted user record of the main thread whose
// message is message, record in its chain, as a task notification of a
// subagent's leg where it is one: a text whose origin says so, its lines
// naming the subagent and a leg whose call was read ([taskNotification]).
func (w *holdingsWalk) notification(line threadLine, message threadMessage, record int32) error {
	text, ok := message.Text()
	if !ok {
		return nil
	}
	// AN ORIGIN THAT DOES NOT DECODE IS NO NOTIFICATION, the one fact it could
	// have been, and costs the reading nothing more.
	if kind, err := line.OriginKind(); err != nil || kind != claudeTaskNotification {
		return nil
	}
	agent, leg, ok := taskNotification(text)
	if !ok || w.legs[leg] == nil {
		return nil
	}
	// CLONED, since each is a piece of a text up to a line long, which a kept
	// substring would hold until the walk ends.
	key := legNotice{agent: strings.Clone(agent), leg: strings.Clone(leg)}
	if _, filed := w.notices[key]; !filed {
		if err := w.keep(); err != nil {
			return err
		}
	}
	w.notices[key] = noticeFact{record: record}
	return nil
}

// subagentThread reads subagent agent's thread, whose bytes member hands out,
// a whole line at a time, into a, and once it is read whole, which of its
// records its model had seen when its last was written
// ([holdingsWalk.subagentEnd]).
func (w *holdingsWalk) subagentThread(ctx context.Context, agent string, a *subagentFacts,
	member io.Reader) error {

	lines := newThreadReader(&handedReader{r: member, handed: &w.handed}, w.bounds.plain-w.handed)
	chain := newThreadChain(w.records)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := lines.Next()
		var broken *threadLineError
		switch {
		case errors.Is(err, io.EOF):
			a.endLeg()
			w.subagentEnd(agent, a, chain)
			return nil
		case errors.As(err, &broken):
			// READ ON PAST IT: the subagent is not whole, and the calls its
			// later records name are still counted, so a call no reading of
			// its thread could hold is still told under its heading.
			a.notWhole(fmt.Sprintf("line %d of its thread is no record", broken.N), broken.Err)
			continue
		case err != nil:
			return &holdingsError{why: "a subagent's thread was not read whole", cause: err}
		}
		w.subagentLine(agent, a, chain, line)
	}
}

// subagentRecord files line, one of subagent agent's thread, in chain where
// it is a user, assistant, attachment or system record, and answers its record
// there: -1 where it is none, or where chain cannot walk to it
// ([threadChain.add]), which leaves a not whole and files nothing more, so
// what the walk holds stays within its bound.
func (w *holdingsWalk) subagentRecord(agent string, a *subagentFacts, chain *threadChain, line threadLine) int32 {
	head := line.Head
	switch {
	case chain.broken:
		return -1
	case head.Type != recordUser && head.Type != recordAssistant && head.Type != recordAttachment &&
		head.Type != recordSystem:
		return -1
	}
	marks := uint8(0)
	if head.Type == recordUser || head.Type == recordAssistant {
		marks = markUserOrAssistant
	}
	record, err := chain.add(line, head.AgentID == agent && head.Sidechain != nil && *head.Sidechain, marks)
	if err != nil {
		chain.broken = true
		a.notWhole(fmt.Sprintf("line %d of its thread is a record the reading cannot walk to as the resume does",
			line.N), err)
	}
	return record
}

// subagentEnd decides, once subagent agent's thread is read whole, which of
// its records its model had seen when its last record of its own was written,
// and files it on each asked call of the thread: the chain from that record,
// which must reach the thread's root, else a is not whole (claudechain.go's
// file comment).
func (w *holdingsWalk) subagentEnd(agent string, a *subagentFacts, chain *threadChain) {
	if !chain.broken {
		chain.resolve()
		chain.done()
		switch {
		case chain.lastOwn < 0:
			a.notWhole("its thread holds no record of its own", nil)
		default:
			if end := chain.walk(chain.lastOwn, false); end != endRoot {
				a.notWhole(fmt.Sprintf("its chain from its last record ends at %s, short of its root", end), nil)
			}
		}
	}
	chain.done()
	for _, f := range w.calls {
		if f.uses > 0 && f.useThread == agent {
			f.useHanded = chain.handed(f.useRec)
		}
		if f.results > 0 && f.resultThread == agent {
			f.resultHanded = chain.handed(f.resultRec)
		}
	}
}

// subagentLine reads line, one of subagent agent's thread, into a and chain.
func (w *holdingsWalk) subagentLine(agent string, a *subagentFacts, chain *threadChain, line threadLine) {
	head := line.Head
	if a.restarts.See(head) {
		a.restarted = true
		a.notWhole(fmt.Sprintf("its thread restarts at line %d", line.N), nil)
	}
	if head.AgentID != "" && head.AgentID != agent {
		a.notWhole(fmt.Sprintf("line %d of its thread is another agent's record", line.N), nil)
	}
	if head.Type == recordProgress {
		a.notWhole(fmt.Sprintf("line %d of its thread is a progress record, which Claude Code's load drops, "+
			"handing each record after it that names it to its parent", line.N), nil)
	}
	if err := line.ReplacedResults(w.replaced); err != nil {
		a.notWhole(fmt.Sprintf("line %d of its thread replaces results in no shape Claude Code writes", line.N),
			err)
	}
	// FILED WHILE THE CHAIN STANDS, as its records are
	// ([holdingsWalk.subagentRecord]): past its bound the chain files nothing
	// more, so what it holds stays within it.
	if !chain.broken {
		if err := line.Retracted(chain.retract); errors.Is(err, errChainBound) {
			chain.broken = true
			a.notWhole(fmt.Sprintf("line %d of its thread is a record the reading cannot walk to as the resume does",
				line.N), err)
		} else if err != nil {
			a.notWhole(fmt.Sprintf("line %d of its thread retracts records in no shape Claude Code writes", line.N),
				err)
		}
	}
	record := w.subagentRecord(agent, a, chain, line)
	if head.Type != recordUser && head.Type != recordAssistant {
		return
	}
	if head.AgentID != agent || head.Sidechain == nil || !*head.Sidechain {
		a.notWhole(fmt.Sprintf("line %d of its thread is a %s record that is not its own", line.N, head.Type), nil)
		return
	}
	if !head.Chained() {
		return
	}
	message, err := line.Message()
	if err != nil {
		a.notWhole(fmt.Sprintf("line %d of its thread is no record", line.N), err)
		return
	}
	stop, stopErr := line.StopReason()
	if stopErr != nil {
		a.notWhole(fmt.Sprintf("line %d of its thread says no stop reason", line.N), stopErr)
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
	if record >= 0 {
		if head.Type == recordUser && len(message.Calls()) > 0 {
			chain.mark(record, markResults)
		}
		// A REPLY'S ID THAT DOES NOT DECODE costs only what it could recover,
		// as in the main thread ([holdingsWalk.mainLine]).
		if id, idErr := line.MessageID(); head.Type == recordAssistant && idErr == nil && id != "" {
			chain.reply(record, id)
		}
	}
	for _, call := range message.Calls() {
		w.count(agent, call, line.N, record)
	}
	if head.Type == recordUser && legStart(message) {
		a.endLeg()
		a.legStarts++
	}
	a.open = true
	a.finished = head.Type == recordAssistant && stopErr == nil && apiErr == nil && modelErr == nil &&
		finishedReply(message.Kinds(), stop, model, apiError)
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

// endLeg ends the stretch of a's thread its last leg start began.
func (a *subagentFacts) endLeg() {
	if a.open && !a.finished && a.unfinished == 0 {
		a.unfinished = max(a.legStarts, 1)
	}
	a.open = false
}

// legStart reports whether message, a counted user record's of a subagent's
// thread, starts a leg of its work: anything but the results of its calls
// alone, so a text, a list of no blocks, or a block of another kind, the
// subagent's prompt and every message sent to it among them.
func legStart(message threadMessage) bool {
	if _, text := message.Text(); text {
		return true
	}
	kinds := message.Kinds()
	return len(kinds) == 0 || slices.ContainsFunc(kinds, func(kind string) bool { return kind != blockToolResult })
}

// finishedReply reports whether an assistant record whose blocks are kinds,
// whose reply stopped for stop and names model as the model that wrote it,
// and which its harness wrote in place of a failed request's reply where
// apiError, ends a leg: a reply with which its model ended its turn, as
// Claude Code itself tests one (its stop reason "end_turn", no API error and
// a model other than "<synthetic>"), of blocks of text and thinking alone, at
// least one.
//
// "END_TURN" ALONE, never any stop but a call's: every reply the harness
// writes itself stops at "stop_sequence", a failed request's error and a
// resume's "No response requested." among them, and no model read what came
// before either; and a reply cut at its token bound ("max_tokens") or refused
// ended no turn its model chose to end. Each leaves its leg unfinished, the
// safe direction.
//
// AND A MODEL'S OWN, never a reply of the model "<synthetic>", whatever it
// says of its stop: that is the model of every reply the harness writes in
// place of a model's ([threadLine.Model]), and the harness's own test passes
// over each such reply before it reads a stop. No such reply 2.1.310 or
// 2.1.322 builds says "end_turn", so the stop alone already leaves each one's
// leg unfinished; the test is still the harness's whole one, so a release
// that builds one saying so finishes no leg no model ended.
func finishedReply(kinds []string, stop, model string, apiError bool) bool {
	if len(kinds) == 0 || apiError || stop != claudeEndedTurn || model == claudeSyntheticModel {
		return false
	}
	return !slices.ContainsFunc(kinds, func(kind string) bool {
		return kind != blockText && kind != blockThinking && kind != blockRedactedThinking
	})
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

// sendTarget is the subagent a SendMessage call's input, raw, sends to: its
// `to`, matched exactly and a key written twice by its last value, where it
// is a string naming an agent ([validClaudeAgent]); "" else, a send to no
// subagent a reading reads.
func sendTarget(input json.RawMessage) string {
	if !wireObject(input) {
		return ""
	}
	var wire struct {
		To json.RawMessage `json:"to"`
	}
	if wireDecode(input, &wire) != nil {
		return ""
	}
	to, err := wireRequiredString("to", wire.To)
	if err != nil || !validClaudeAgent(to) {
		return ""
	}
	return to
}

// agentAnswer is what a leg's result record's own toolUseResult, raw, says,
// read by its exact keys and a key written twice by its last value: whether
// an Agent or Task call's subagent completed (`status` "completed"), the
// subagent it names (`agentId`, "" where it names none a reading reads), and
// whether a SendMessage was refused (`success`, the literal false). A result
// of another shape says none of them: no answer, and no refusal.
func agentAnswer(raw json.RawMessage) (completed bool, agent string, refused bool) {
	if !wireObject(raw) {
		return false, "", false
	}
	var wire struct {
		Status  json.RawMessage `json:"status"`
		AgentID json.RawMessage `json:"agentId"`
		Success json.RawMessage `json:"success"`
	}
	if wireDecode(raw, &wire) != nil {
		return false, "", false
	}
	status, err := wireRequiredString("status", wire.Status)
	completed = err == nil && status == claudeAgentCompleted
	if agent, err = wireRequiredString("agentId", wire.AgentID); err != nil || !validClaudeAgent(agent) {
		agent = ""
	}
	return completed, agent, string(wire.Success) == "false"
}

// taskNotification is the subagent and the leg a task notification's text
// names: its lines, in order, `<task-notification>`, `<task-id>A</task-id>`,
// `<tool-use-id>L</tool-use-id>`, any number of
// `<output-file>…</output-file>`, and `<status>completed</status>`, nothing
// after the status read. False for any other text, an agent outside
// [validClaudeAgent]'s rule, or a leg outside the call-id rule
// ([sandbox.ValidCallID]).
func taskNotification(text string) (agent, leg string, ok bool) {
	line, rest := nextLine(text)
	if line != "<task-notification>" {
		return "", "", false
	}
	line, rest = nextLine(rest)
	if agent, ok = enclosed(line, "<task-id>", "</task-id>"); !ok || !validClaudeAgent(agent) {
		return "", "", false
	}
	line, rest = nextLine(rest)
	if leg, ok = enclosed(line, "<tool-use-id>", "</tool-use-id>"); !ok || !sandbox.ValidCallID(leg) {
		return "", "", false
	}
	for {
		line, rest = nextLine(rest)
		if _, file := enclosed(line, "<output-file>", "</output-file>"); !file {
			break
		}
	}
	if line != claudeNotificationCompleted {
		return "", "", false
	}
	return agent, leg, true
}

// nextLine is text's first line, its break dropped, and what follows it.
func nextLine(text string) (line, rest string) {
	line, rest, _ = strings.Cut(text, "\n")
	return line, rest
}

// enclosed is what line holds between opening and closing, where line is
// exactly that.
func enclosed(line, opening, closing string) (string, bool) {
	inner, ok := strings.CutPrefix(line, opening)
	if !ok {
		return "", false
	}
	return strings.CutSuffix(inner, closing)
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
// names, live where its first result is ([holdingsWalk.live]), its first use
// comes before it in its thread, the resume hands its model the records of
// both ([callFacts.useHanded]), and no record replaced it
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
		records.Live = f.results > 0 && f.uses > 0 && f.useThread == f.resultThread && f.useLine < f.line &&
			!f.replaced && w.live(f.resultThread, f.line) && f.useHanded && f.resultHanded
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
	// 2. WHOLE. A thread with no chained record never comes to be asked: the
	// subagent of a reading's answer is one a counted record of whose thread,
	// a chained one, names an asked call.
	if a.broken != "" {
		return "its thread is not whole: " + a.broken
	}
	// 3. EVERY LEG ANSWERED, LIVE.
	own := start.results == 1 && start.completed && start.agent == agent && !start.failed &&
		a.meta.foreground && start.answered
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
// subagent agent's leg: one the resume hands its model, on its chain past its
// last restart ([threadChain.handed]).
func (w *holdingsWalk) noticed(agent, leg string) bool {
	return w.notices[legNotice{agent: agent, leg: leg}].live
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

// errNotOneText is a result's content that is neither a string nor one text
// block ([resultText]).
var errNotOneText = errors.New("not one text block")

// resultText is the text a tool_result's content, raw, holds: a JSON string, or
// a list of exactly one element, an object whose `type` is "text" and whose
// `text` is a string, other keys passed over, each key matched exactly and a
// key written twice read by its last value ([wireDecode]); false for every
// other content, absent and null among it.
//
// THE LIST IS READ NO FURTHER THAN ITS SECOND ELEMENT ([eachElement]), so a
// list a line long costs what its first two elements do.
func resultText(content json.RawMessage) (string, bool) {
	if len(content) > 0 && content[0] == '"' {
		var text string
		if err := wireDecode(content, &text); err != nil {
			return "", false
		}
		return text, true
	}
	text, blocks := "", 0
	err := eachElement(content, func(raw json.RawMessage) error {
		blocks++
		if blocks > 1 || !wireObject(raw) {
			return errNotOneText
		}
		var block struct {
			Type json.RawMessage `json:"type"`
			Text json.RawMessage `json:"text"`
		}
		if err := wireDecode(raw, &block); err != nil {
			return err
		}
		kind, err := wireRequiredString("type", block.Type)
		if err != nil || kind != blockText {
			return errNotOneText
		}
		text, err = wireRequiredString("text", block.Text)
		return err
	})
	return text, err == nil && blocks == 1
}
