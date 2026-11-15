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
//     ([threadRestarts.See]), and every user and assistant record's message
//     decoded ([threadLine.Message]) and its calls counted, wherever it lies
//     and whoever's it is ([holdingsWalk.mainRecord]); rule 2 reads in
//     subagent A's thread the records of A's `agentId` and `isSidechain`
//     true. Every other line counts for nothing; but every line of the main
//     thread is handed to the model of Claude Code's resume ([threadChain]),
//     and every line of A's thread to the model of A's own load of it
//     (newThreadChain(A)), each of which decides once its thread is read
//     whole which records that load hands its model and in what orders;
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
// LIVE ONLY WHERE THE RESUME HANDS ITS MODEL BOTH RECORDS AND ITS REQUEST
// PAIRS THEM. Which records Claude Code's resume hands its model of a thread,
// and in what orders, is the model's, which the reading cites and never
// restates ([threadChain]): in the main thread the walk from its leaf, what it
// recovers and hangs off the replies on it and its tails, less what the
// retraction filter and the drop of malformed attachments take; in a
// subagent's thread what that subagent's own load of it hands
// (newThreadChain(agent)); each read from the shipped code of the releases
// [claudeResumeRead] names, under every variant the reading cannot see. The
// reading adds two rules of its own, and nothing else:
//
//   - THE PAIRING RULE, the conservative rule (C) ([holdingsPairing]). A call
//     is held only where the record of its use carries a reply id (C0); one
//     use of its id and one result are counted over every user and assistant
//     record of its thread's file, chained or not, the thread's own or not, no
//     batch's list or split id folds it into another call, and its id is none
//     Claude Code's request writes of its own (C1, [holdingsWalk.mainRecord],
//     [holdingsWalk.idsFold]); the resume hands both records under every
//     variant (C2); and on EVERY order it hands ([threadChain.EachOrder]), the
//     result comes after the first record of the use's reply, every record
//     from that one to the later of the use and the result is a plain record
//     of that reply or a plain results record (C3, C4), no record before that
//     first one holds a result of a call the reply made (C5), no compaction's
//     boundary lies at or after it (C6), and where the call is the Poll
//     tool's, the reply holds no other Poll call (C7). Never by where a record
//     lies in the file: an attempt the harness withdrew can lie past every
//     restart and be handed to no model, and a result handed after another
//     reply is handed with no call. The rule TELLS, knowingly, shapes the
//     request does pair that it cannot place, none of them measured: a call it
//     tells that the session held is only told again, the safe direction.
//   - THE LAST RESTART, the reading's own forward guard beside the model's
//     truth: in the main thread a result, a leg's own answer and a task
//     notification each count only where its line follows the thread's last
//     restart ([threadRestarts.See], [holdingsWalk.live]), and a subagent's
//     thread that restarts at all is not whole (rule 2). The model says what
//     the releases it was read from hand; a later one that cut a thread at a
//     microcompaction's boundary, a span replaced or a later root would hand
//     less than lies before it, and a result there is told, the safe
//     direction.
//
// A leg's own answer is paired by the same rule in the main thread, and a
// task notification counts only where the resume hands its record under every
// variant. Measured (probe T, both releases): a continuation hands its model
// each whole-line result of its main thread the chain or its recovery holds
// after its last restart, verbatim and with its call, the results of parallel
// calls off the chain among them. In a subagent's thread a call is held only
// where the subagent is reported and the pairing rule holds of the call on
// every order its own load hands ([holdingsWalk.subagentEnd]).
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
// whatever chain it lies on ([threadChain]'s retraction filter), so the
// pairing rule's C2 tells a call such a record holds. No load of a subagent's
// thread runs that filter, so a subagent's thread holding any such list is not
// whole (rule 2): what its model was handed is not what its file holds. A
// record that retracts in no shape Claude Code writes is no reading in the
// main thread.
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
// where all five of these hold, each read off the records alone, and off the
// ONE CONVERSATION the subagent's own load of its thread hands, as the model
// of that load says it ([threadChain] in its subagent mode), which is what a
// SendMessage continuation hands its model.
//
//  1. LINKED: its metadata is one object, read by exact keys, naming the call
//     that started it (`toolUseId`, an id the log keeps), a depth of exactly
//     the integer 1, a request shape, and a description or none; and that
//     call is exactly one counted Agent or Task call of the main thread.
//  2. WHOLE: no line of its thread is no record, and its member does not end
//     in a torn piece, which its own load may read as one more record; every
//     user and assistant record is its own, its `agentId` and `isSidechain`
//     true, and its message, its stop reason, its mark of an API error and
//     the model of its reply decode; no record names another agent, replaces
//     results or lists a batch's calls in no shape Claude Code writes; its
//     thread never restarts; no reply of it is one its model refused, and no
//     record of it takes records back; the model of its own load places it
//     ([threadChain.Unknown]); and THE STRICT RULE: every record of its own
//     that no record of its own continues is that load's leaf or one it hands
//     ([wholeOf]), so its file holds no branch of its conversation its model
//     was not handed, and a thread in which the load finds no conversation
//     fails it.
//  3. EVERY LEG ANSWERED, LIVE IN THE MAIN THREAD. Its legs are the call that
//     started it, and every SendMessage call of the main thread to it, but one
//     its own result refused (`success`, the literal false); a SendMessage
//     whose result is missing stays a leg, and one sent under an id the log
//     keeps none of is a leg nothing can answer ([holdingsWalk.leg]). The
//     call that started it is answered by its own result's record (`status`
//     "completed" naming it, no error, of a subagent its metadata says the
//     call waited for), where the pairing rule pairs the call's use with that
//     one, the id's one use and one result counted over every user and
//     assistant record of the main thread, which holds no batch's list and no
//     result of a split id at all, since a leg's id is known only once its
//     call is read ([holdingsWalk.mainEnd]); or by a task notification of it
//     the resume hands its model under every variant. A SendMessage is
//     answered only by such a notification of it. Each answer counts only
//     past the main thread's last restart. AN ANSWER CARRYING A HANDBACK
//     ANSWERS NO LEG, the call's own or a notification, nor does any answer
//     of a leg one of whose answers carries one: under a handback the
//     subagent's report never reaches the main thread's model through it
//     ([carriesHandback], [handbackNotice]).
//     A NOTIFICATION IS AN ANSWER: a continuation hands its model the
//     notification's record verbatim (probe T, T10, on 2.1.310 and 2.1.322),
//     and its lines name the subagent and the leg ([taskNotification]). An
//     answer is read off a record of that one result alone, since a record's
//     own toolUseResult is that record's, never one call's of several.
//  4. NO UNSEEN LEG: its file starts no more legs (a user record of its own
//     that is not the results of its calls alone, its prompt among them,
//     wherever it lies) than the main thread gave it; a message the main
//     thread holds no call of, a skill's text among them, is one more.
//  5. EVERY LEG FINISHED, along every order its own load hands: the first of
//     its user and assistant records starts a leg, and each stretch from one
//     leg's start to the next ends on a reply of text or thinking alone,
//     whose text says something, with which its model ended its turn: its
//     stop reason "end_turn", no API error and a model other than
//     "<synthetic>", Claude Code's own whole test of a reply with which its
//     model ended its turn. A subagent answers with the text of its last reply
//     that says something, so a stretch ending on one of thinking alone
//     answers with an earlier reply's text ([wholeOf]). A reply that wrote
//     text and then called a tool is two records, the text's first, each
//     saying it stopped for the call: a thread cut between the two ends on
//     text, which only the stop reason tells from a finished reply. A request
//     that failed for good leaves a reply the harness wrote itself, the
//     error's text, stopped at "stop_sequence" and marked an API error
//     ([threadLine.APIError]), on which the harness ends the subagent and
//     answers the call that started it as completed: no model read what that
//     request carried, a call's result among it. And every reply the harness
//     writes itself in place of a model's is of the model "<synthetic>"
//     ([threadLine.Model]), which its test passes over whatever it says of its
//     stop.
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
// none the log keeps or is carried by another call of the ask; THE GATE: the
// release that continues the session ([sandbox.HoldingsAsk.Resumer], the
// relaunch box's, as its `--version` answered before the brief was chosen) is
// unread, or is none of the releases the model of the resume was read from
// ([claudeResumeRead], a set and never a range, since a release before, between
// or after them may hand its model otherwise and the model says nothing of it);
// its writer is older than [holdingsWriterFloor], or unknown; its caller left
// while it waited for its slots, or between members or lines; the archive did
// not open, is no gzip or no tar, or inflates past its bound; a member is named
// absolutely or through `..`; a member it reads is held twice; the main thread
// is no regular file, or is not there; a line of it is past its bound or is no
// record, a user or assistant record's message does not decode, or a record
// replaces results or lists a batch's calls in no shape Claude Code writes; the
// model of Claude Code's resume cannot place the main thread, whatever shape it
// met first ([threadChain.Unknown]: among them a progress record, a marked
// checkpoint, a sidechain's record, a record of no lowercase canonical uuid, a
// retraction list or a compaction's metadata in no shape Claude Code writes, a
// result the resume may hang under another record than its parent, a thread
// past 5,242,880 bytes a large-file reader may read otherwise, and a thread
// past the model's bounds); a line of a subagent's thread is past its bound, or
// its member or metadata past the archive's budget; more subagents than
// [holdingsMaxAgents], or more legs than [holdingsMaxLegs]; or the stream does
// not end at io.EOF. The error says which, in words a relaunch logs
// ([holdingsError]). WHY A LINE'S OR THE ARCHIVE'S BOUND PASSED IN A SUBAGENT'S
// THREAD ENDS THE WHOLE READING: a call's threads, uses and results are counted
// over the whole archive, and a reading cut part way vouches for none of them.
// The model's bounds in a subagent's thread end nothing: past them the model
// files nothing more, but every message is still decoded and its calls counted
// ([holdingsWalk.subagentThread]), and the subagent is not whole.
//
// THE GATE READS THE RELEASE ALONE, never the box's settings. A managed
// settings file, which Claude Code applies after the run's own pinned settings,
// can still switch on either of the resume's two switches
// ([claudeResumeSwitches]). The precompact switch moves which large-file reader
// loads a thread past 5,242,880 bytes, and the model reads both, so a main
// thread the two may load otherwise is no reading, and a subagent's not whole
// ([threadChain], THE LARGE FILE). The interrupted turn's switch has the
// continuation run again the turn its last process was cut off in, which acts
// at its first request: its caller's question rather than the reading's, and
// one neither can see in the box, which ADR-0073's amendment on this reading
// leaves.
//
// THE BOUNDS, AND WHAT A READING HOLDS. The archive's plaintext at
// [threadPlainMax], what its read members hand out at the same budget, a line
// at [threadLineMax], what the model of Claude Code's resume, or of a
// subagent's own load, files of a thread at its bounds
// ([threadChainMaxRecords], [threadChainMaxCalls]), a metadata at
// [holdingsMetaMaxBytes], the asked calls at [sandbox.MaxBridgeCalls],
// subagents at [holdingsMaxAgents], legs at [holdingsMaxLegs], and the writer
// at its floor. At its worst a reading holds the line reader's buffer; one line
// and the slice that gathered it; the line's raw keys and its message's decode,
// each at most a line; what the message's calls keep (a line of the smallest
// results keeps about three times the line); one result's text; a metadata; the
// model of one thread, at most [threadChainMaxRecords] records and
// [threadChainMaxCalls] call ids, past either of which it files nothing more
// ([threadChain], THE BOUNDS), and the pairing rule's mark of each of its
// records, two bytes a record ([holdingsPairing]), one thread's at a time; and
// its facts: a few words and an id of at most [sandbox.MaxBridgeCallIDBytes]
// for each asked call, each leg and each notification, and for each subagent a
// few more and a description of at most [holdingsDescriptionBytes], about 2 MiB
// at the bounds. EVERY ID IT KEEPS IS ONE THE CALL-ID RULE HOLDS: a call under
// any other is kept as no leg ([holdingsWalk.leg]), and a metadata naming one
// keeps none ([subagentMetaOf]), since an id is the box's word, as long as a
// line, and nothing can link, answer or notice a call under one. MEASURED on
// two cores (BenchmarkAReadingAtItsBounds, whose doc says how each figure is
// read off the runtime's GC trace), one case a process, in three runs: a time
// is the slower of two at the default GOGC, what is HELD AT MOST the largest
// live heap a collection left during the read in any of the three, one with the
// collector run at every tenth of the heap's growth (GOGC=10), and a peak the
// largest heap at the default. ITS WORST is the coincident one: a line at the
// line's bound of the smallest results a message holds, read after a main
// thread at both bounds of the model of the resume, so decoded beside all the
// model filed before the line passes its bounds, 102 MiB held at most, its heap
// peaking at 181 MiB, in 1.7 s: within the about 133 MiB a seat's read for
// words, the other taker of the seats' one slot, holds at its worst
// ([SeatTranscriptReadsAtOnce]). That line alone, 75 MiB held at most and a
// peak of 123 MiB, in 0.22 s; the thread at both bounds alone, 2^17 replies
// each making a call and naming a parent written after it, a reading in 1.5 s,
// 42 MiB held at most; the model's other bounds, its calls and its retractions
// and kept uuids, 0.26 s and 0.36 s, 12 and 11 MiB. 255 MiB of ordinary lines,
// the plaintext's bound, each reply under its own id, read in 2.7 s holding 33
// MiB; the slowest lines measured for their size, a line at the line's bound of
// blocks of a kind no reader names, 8 MiB in 0.34 s, so an archive of them to
// the plaintext's bound, which gzip shrinks to almost nothing, reads in about
// 11 s by that rate (computed, not measured whole); 1,024 subagents in 0.13 s;
// 96 Agent calls under ids of a mebibyte and 1,024 metadata naming ids of
// 65,000 bytes, which held 97.7 and 65.3 MiB live before the reading kept no
// such id, in 0.94 s and 0.16 s holding 8 and 2 MiB; and an ordinary kept
// session, a 1.6 MiB archive, in 68 ms.

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
	// claudeRefused is the stop reason of a reply its model refused, which
	// leaves a subagent's thread not whole ([holdingsWalk.subagentLine]).
	claudeRefused = "refusal"
	// claudeToolHandback is the tool through which a subagent under a handback
	// contract delivers its report, which every answer of a leg that carries
	// a handback names ([handbackNotice]).
	claudeToolHandback = "SubagentHandback"
)

// holdingsFloor is [holdingsWriterFloor].
func (ClaudeCode) holdingsFloor() string { return holdingsWriterFloor }

// checkHoldings holds ask, and the session archive names, to what a reading
// answers, before anything is opened: a session whose id names a thread
// ([claudeMainThread]); a resumer's release whose resume [threadChain] models
// ([claudeResumeRead]); and an ask of 1 to [sandbox.MaxBridgeCalls]
// calls, at least one of them asked, each asked call's id one the log keeps
// ([sandbox.ValidCallID]) and carried by no other call of the ask, asked or
// not.
//
// ONLY A RESUMER WHOSE RESUME THE MODEL WAS READ FROM, never one at or after a
// floor: a call is held only where the resume hands its model the call's
// records ([threadChain]), and that is the resuming release's code, which the
// model was read from for a set of releases alone. A release before, between or
// after them may load, relink, walk or drop records otherwise, and a reading of
// it could hold a call its model was never handed. A release this code cannot
// read, and an unread one, are outside the set as well.
//
// AN ID ANOTHER CALL CARRIES IS NO READING, NEVER A GUESS: the session files
// both calls' records under the one id, so no record says whose it is, and an
// ask that holds two such calls is a telling that broke its own rule.
func (ClaudeCode) checkHoldings(archive sandbox.SessionArchive, ask sandbox.HoldingsAsk) error {
	if _, ok := claudeMainThread(archive.SessionID); !ok {
		return &holdingsError{why: "the session's id names no Claude Code thread"}
	}
	if !claudeResumeRead(ask.Resumer) {
		return &holdingsError{why: fmt.Sprintf("Claude Code %.64q continues the session, and a reading "+
			"reads only for the releases whose resume the line layer models: a release joins them once its "+
			"shipped resume is re-read against that model and a census of archives it wrote and resumed "+
			"holds to it (claudeResumeRead, internal/sandbox/executor)", ask.Resumer)}
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
	// poll is whether its first use calls the Poll tool, and folds whether a
	// record of a thread read names it where the request may fold it into
	// another call, or another into it ([holdingsWalk.foldsOf]).
	poll, folds bool
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
	// the record holds that one result alone ([agentAnswer]).
	results    int
	resultRec  int32
	resultLine int
	failed     bool
	completed  bool
	agent      string
	refused    bool
	// handback is whether a record of a result of it, or a task notification
	// of it, carries a handback: no answer of the leg is then its subagent's
	// report ([carriesHandback], [handbackNotice]).
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
	lines := newThreadReader(&handedReader{r: member, handed: &w.handed}, w.bounds.plain-w.handed)
	chain, pairing := newThreadChain(""), &holdingsPairing{}
	for {
		// BETWEEN LINES, since a line's read takes no context: a reading
		// overruns its caller's wait by one line's read and decode at most.
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := lines.Next()
		switch {
		case errors.Is(err, io.EOF):
			return w.mainEnd(ctx, chain, pairing)
		case err != nil:
			return &holdingsError{why: "the session's main thread was not read whole", cause: err}
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

// folded reports whether the request may fold id's call into another, or
// another into it, by what the whole thread holds, chain's once End decided
// it: an id a batch's split call or result reads as split from another
// call's carried by a block of the thread ([splitBase]), and one Claude Code
// writes of its own ([harnessID]).
func folded(chain *threadChain, id string) bool {
	if base, split := splitBase(id); split {
		if _, carried := chain.CallKey(base); carried {
			return true
		}
	}
	return harnessID(id)
}

// mainEnd decides, once the main thread is read whole into chain and pairing,
// which of its calls the request a resumed session sends pairs, and files it
// on each fact of the main thread: each asked call's and each leg's first use
// and first result paired by the pairing rule ([holdingsPairing]); a leg's
// own answer, and each task notification, only where its line follows the
// main thread's last restart, as a result's is ([holdingsWalk.live]); and a
// notification only where the resume hands its model its record under every
// variant. No reading where End's context ended, or where the model cannot
// place the thread ([threadChain.Unknown]).
//
// THE LAST RESTART IS THE READING'S OWN FORWARD GUARD, beside the model's
// truth: the model says what 2.1.310 and 2.1.322 hand, and a release after
// them that cut the thread at a microcompaction's boundary, a span replaced
// or a later root would hand less than lies before one.
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
			asks = append(asks, &pairAsk{use: chainHandle(f.useRec), result: chainHandle(f.resultRec), poll: f.poll})
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
		f.answered = asks[i].paired && !f.folds && !folded(chain, ids[i])
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
// thread's own or not, before or after any restart: the resume loads and
// walks each such record whatever its `isSidechain` or `agentId` says, and its
// request keeps the first use of a call's id and the first result of it in
// the message after that use. So a second record carrying an asked call's id
// is another carrier of it, which may stand in its place, and counting it
// tells the call, its uses or results past one; a leg's the same
// ([holdingsWalk.mainEnd]). MEASURED: no user or assistant record of the
// probes' main threads lacks `isSidechain` or carries an `agentId` but a
// sidechain's, which is no reading ([threadChain]), and none of the 11,727
// user, assistant, attachment and system records of their 521 thread files
// lacks a `parentUuid`, so nothing measured moves.
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
			facts.useThread, facts.useRec, facts.poll = thread, record, call.Name == claudeToolPoll
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
	leg.handback = leg.handback || carriesHandback(line.ToolUseResult())
	leg.results++
	if leg.results > 1 {
		return
	}
	leg.resultRec, leg.resultLine, leg.failed = record, line.N, call.IsError != nil && *call.IsError
	if alone {
		leg.completed, leg.agent, leg.refused = agentAnswer(line.ToolUseResult())
	}
}

// notification files line, a user record of the main thread whose
// message is message, record in its chain, as a task notification of a
// subagent's leg where it is one: a text whose origin says so, its lines
// naming the subagent and a leg whose call was read ([taskNotification]).
func (w *holdingsWalk) notification(line threadLine, message threadMessage, record int32) error {
	text, ok := message.Text()
	if !ok {
		return nil
	}
	if originOf(line) != claudeTaskNotification {
		return nil
	}
	agent, leg, ok := taskNotification(text)
	if !ok || w.legs[leg] == nil {
		return nil
	}
	if handbackNotice(text) {
		w.legs[leg].handback = true
	}
	// CLONED, since each is a piece of a text up to a line long, which a kept
	// substring would hold until the walk ends.
	key := legNotice{agent: strings.Clone(agent), leg: strings.Clone(leg)}
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
// a whole line at a time, into a, the model of the subagent's own load of it
// ([threadChain], newThreadChain(agent)) and the pairing rule's marks
// ([holdingsPairing]), and once it is read whole, what rule 2 and the pairing
// rule read off the one conversation that load hands
// ([holdingsWalk.subagentEnd]).
//
// IT READS ON past every line that leaves the subagent not whole, a line that
// is no record and every line past the model's bound among them: the calls
// each user and assistant record names are counted whatever came before it,
// so a call no reading of the thread could hold is still told under its
// heading, and a second carrier of an asked id past the bound still tells it.
// Only the line reader's own bounds, or a fault of the member's reader, end the
// reading.
func (w *holdingsWalk) subagentThread(ctx context.Context, agent string, a *subagentFacts,
	member io.Reader) error {

	lines := newThreadReader(&handedReader{r: member, handed: &w.handed}, w.bounds.plain-w.handed)
	chain, pairing := newThreadChain(agent), &holdingsPairing{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := lines.Next()
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

// errTakenBack is a subagent's record naming records taken back out of its
// conversation ([holdingsWalk.subagentLine]).
var errTakenBack = errors.New("it names records taken back")

// subagentLine reads line, one of subagent agent's thread, into a, chain and
// pairing.
//
// A RETRACTION LIST LEAVES IT NOT WHOLE, whatever it names: a harness writes
// one only beside a refused reply's fallback or a malformed reply's retry, or
// on a failed request's reply, and no load of a subagent's thread filters by
// it ([threadLine.Retracted]), so what its model was handed is not what its
// file holds. And so does A REPLY ITS MODEL REFUSED (its stop reason
// "refusal"): on a refusal Claude Code moves the request to another model and
// takes the refused attempt's records back, a call among them, where the
// subagent's file then holds that attempt on its conversation and records no
// fallback (2.1.322's refusal lanes @296208832, 2.1.310's @249540334).
func (w *holdingsWalk) subagentLine(agent string, a *subagentFacts, chain *threadChain, pairing *holdingsPairing,
	line threadLine) {

	head := line.Head
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
	switch err := line.Retracted(func(string) error { return errTakenBack }); {
	case errors.Is(err, errTakenBack):
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
		modelErr == nil && finishedReply(message.Kinds(), stop, model, apiError) {
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
			asks = append(asks, &pairAsk{use: chainHandle(f.useRec), result: chainHandle(f.resultRec), poll: f.poll})
			calls, ids = append(calls, f), append(ids, id)
		}
	}
	pairing.pairs(chain, asks)
	for i, f := range calls {
		f.answered = asks[i].paired && !f.folds && !folded(chain, ids[i])
	}
	return nil
}

// wholeOf files on a, once its subagent's thread is read whole into chain and
// pairing, why its thread is not whole where it is not, and else the first
// leg of its work that ends on no finished reply along a conversation its own
// load hands, if any (rule 2's conditions 2 and 5, the file comment):
//
//   - the model of its own load places its thread ([threadChain.Unknown]);
//   - THE STRICT RULE: every record of its own that no record of its own
//     continues, a compaction's boundary aside, is its load's leaf or one
//     that load hands ([threadChain.OwnLeaves]), so its file holds no branch
//     of its conversation its model was not handed: a reply that finished a
//     leg on another branch than the one its call's result lies on is one;
//   - ALONG EVERY ORDER its load hands ([threadChain.EachOrder]), the first of
//     its user and assistant records starts a leg, and each stretch from one
//     leg's start to the next, or to the end, ends on a reply that finished
//     it ([finishedReply]) and says something ([threadMessage.Says]): a
//     subagent answers its caller with the text of its last reply that says
//     something, so a stretch ending on a reply that says nothing answers
//     with an earlier one's, perhaps written before its call's result was
//     (2.1.322's pkn @302170267 through t5t @302157349, 2.1.310's Owr
//     @255311767 through POt @255299110).
//
// No record's answer turns on a variant there: its own load runs neither q1t
// nor a retraction filter, and the tails' two orders hand the same records,
// so every record is handed or not under every variant alike, and the orders
// part only where the tails do.
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

// carriesHandback reports whether a leg's result record's own toolUseResult,
// raw, carries a handback: a `handback` or a `handbackReport` key, any value,
// read by its exact key.
//
// A HANDBACK ANSWERS NO LEG. Under a handback contract, which both releases
// arm on a run of a subagent behind a flag on by default (2.1.322's BFt
// @288117159, 2.1.310's p_t @245867640, armed @302126445), the subagent
// delivers its report through a call of its own ([claudeToolHandback]), and
// its completion drops the text of its last reply and answers its caller
// with words of the harness's own in its place: that the report was delivered
// as a message, that it has not reported yet, or that none was delivered
// (2.1.322's pkn @302170267, `if(r!==void 0){Z=[];...}`, 2.1.310's Owr
// @255311767). A call's own answer then carries the `handback` key beside
// that text (`...ee!==void 0&&{handback:ee}`, spread into its result at
// @302347284), so no answer of the leg is the subagent's report.
func carriesHandback(raw json.RawMessage) bool {
	if !wireObject(raw) {
		return false
	}
	var wire struct {
		Handback       json.RawMessage `json:"handback"`
		HandbackReport json.RawMessage `json:"handbackReport"`
	}
	// ONE THAT DOES NOT DECODE IS READ AS CARRYING ONE, the safe direction.
	if wireDecode(raw, &wire) != nil {
		return true
	}
	return wire.Handback != nil || wire.HandbackReport != nil
}

// handbackNotice reports whether a task notification's text, whose leading
// lines name a subagent's leg ([taskNotification]), carries a handback
// ([carriesHandback]): where it names [claudeToolHandback] anywhere.
//
// THE NOTIFICATION MARKS A HANDBACK BY ITS WORDS ALONE. Both releases build a
// completion's notification (2.1.322's MXt @298314707 through Pi @292640802,
// 2.1.310's W0t @251577375 through Di) with the subagent's answer, joined, as
// its `<result>`, and no tag of a handback's own: so under a handback its
// result is the harness's words in the report's place, and each of their
// three forms names the tool (2.1.322's H3i, tOr and iOr @288119298 and
// @287819913, 2.1.310's Eqs, kqs and jQn @245869660 and @242470305, the tool
// SubagentHandback in each). A report of the subagent's own that names it
// costs only its leg, the safe direction.
func handbackNotice(text string) bool { return strings.Contains(text, claudeToolHandback) }

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
	err := eachElement(content, func(raw json.RawMessage, _ int) error {
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
