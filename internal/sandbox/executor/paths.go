// Package executor is the detached-run plumbing every harness runner
// shares.
//
// Both supported harnesses run headless in a box and follow the same shape: a
// background process redirects its output to a file and drops a done marker,
// Poll checks that marker and whether the process is still alive, Collect
// decodes the stream and reads the result,
// and an ask overlay surfaces a mid-run clarification. This package owns that
// plumbing so each runner supplies only what is its own: its invocation, its
// config, where its output lands, and how its stream and result are read —
// and how a person's notes to its run's turn reach it between its own steps
// (steer.go: Claude Code's hooks, [ClaudeCode]; OpenCode's plugin,
// opencode_steer.js, [OpenCode]), and how the skills a run is handed as its
// own reach its harness and its loads reach the run's bridge (skills.go: a
// plugin of the run's own on Claude Code; a skills directory and a second
// plugin, opencode_skills.js, on OpenCode).
//
// # A run's commits credit Crewlet, through git
//
// A run whose request asks for it ([sandbox.RunRequest.Attribution]) has every
// commit it authors as itself carry `Co-authored-by: Crewlet
// <noreply@crewlet.ai>` and the Crewlet trailers naming its seat, its run and
// its work item, added by a git hook the runner installs in the box and the wrapper points git at through the
// run's environment — the same for every harness, and whether or not the model
// remembered anything (attribution.go, attribution_hook.sh).
//
// # A run's output is read as a stream
//
// A run's event stream and its error stream grow with the run, so neither is
// ever read whole: the stream is decoded a line at a time in bounded memory
// and the error stream is read from its end, while the files meant to be
// read whole (the report, the question, the result line) are, and one too
// large degrades only itself. See stream.go for the reasons, and what each
// used to cost.
//
// # A Claude Code thread is read a line at a time, holding what its readers read
//
// A person's transcript (transcript.go) shows a session as well as it can,
// and a line it cannot read is an entry saying so. A reader that DECIDES
// something from what a session recorded (that a hook's note reached the
// model, that a call's result is held) cannot: one record it misreads is a
// wrong answer, not a gap on a page. Every such reader (the one that confirms
// a hook's note reached the model, the one that tells a relaunch which of its
// calls a kept session holds, and the one that bounds what a continued
// session's harness counts before its first request, at a take's context
// meter and in the read that collects a job, [resumedReading]) reads a Claude
// Code thread through one layer (claudethread.go), so the rules it is read by
// are held once, each in one walk of a thread's lines that every reading of it
// shares ([walkThread]). The launch floor's figure at the meter
// ([contextReading]), which decides nothing of what the session recorded,
// reads a thread's replies through the layer too, off the last main-thread
// reply's usage, so every reading of a thread reads a reply's usage by one
// rule ([threadLine.Usage]). The rules:
//
//   - A MEMBER IS NAMED EXACTLY ([claudeMainThread], [claudeSubagentThread],
//     [claudeSubagentMeta], [claudePrecompact], and [claudeMemberOf] the other
//     way), never by [claudeThread], which takes any session's file for the
//     main thread: a reader that decides something of one session must never
//     read another session's file as it.
//   - A LINE AT A TIME, IN ONE SCANNER PASS THAT NEVER HOLDS IT
//     ([threadReader]), so a line of any length costs its reader what its
//     readers read of it: Claude Code keeps no bound on a line (a whole-PDF
//     Read writes 55,924,056 bytes on one). A last piece with no break is
//     never a line: it is a write its harness never finished, or the NULs GNU
//     tar fills a member out with when it shrank as it was packed, and a
//     reader that read it would take half a record for one. The box cuts
//     exactly that piece away before a harness resumes the session
//     ([claudeSessionGate]'s torn), but for two files it leaves as they are:
//     one that holds no whole line, and every subagent's thread in a box that
//     has no find. There a reader reads less than the harness may, and what it
//     leaves is never a whole record, so it holds nothing a reader counts.
//   - PAST A BOUND, THE READING IS OVER, with nothing skipped, since a reader
//     that skipped a line would decide from a thread with a hole in it, and it
//     stays over: a member past its reader's bound, EVEN WHERE THE BOUND FALLS
//     RIGHT AFTER A LINE BREAK (metered with io.LimitReader, a member past its
//     bound would read as whole, the lines past the cut neither read nor
//     missed); a line whose facts pass [threadLineMax]; a line holding a value
//     it reads whole past [threadValueMax]; and a line whose type turns after a
//     frame the pass passed over for it (the type's gate, below). Each fails
//     closed: a reader turns a thread with a hole in it into no reading or
//     cannot tell, never a false hold.
//   - A MEMBER'S READER'S ERROR ENDS THE READING AS IT IS, never as the
//     member's end nor as a bound: a reader that meters a kept archive's
//     plaintext with the take's scrub's own meter fails with the scrub's bound
//     once the archive inflates past a reading, and that is its error, in its
//     own words.
//   - THE CONTEXT IS THE ONE PLACE A READING'S CALLER IS HEARD: asked before
//     each piece of at most [lineChunkBytes] a line takes, so a reading
//     overruns its caller's wait by one piece whatever the line's length, and
//     its callers ask none of their own between lines.
//   - EVERY LINE IS READ TO ITS END before anything reads it, with no keyword
//     prefilter, a late fault beating what its head said ([threadHead]): a
//     line no keyword marks can still be no record (a write that tore into
//     another), and a reader that passed it unread would decide from the
//     thread around it as though it were whole.
//   - A LIST IS READ AN ELEMENT AT A TIME AND AN OBJECT A KEY AT A TIME, in
//     the line's one pass: each frame keeps only what its readers ask, each
//     element is held to its type as it is read, and a value no reader reads
//     is passed over unheld, so no list or object a box wrote is ever held
//     whole.
//   - EVERY VALUE IS READ AS CLAUDE CODE'S OWN JSON.parse READS IT, under
//     [jsonscan], whose grammar is encoding/json/jsontext's under the options
//     the package's other readers decode by ([threadDecoding]): a key exactly
//     as written, never matched by case as encoding/json matches a struct's
//     fields; a key written twice by its last value; and a string's invalid
//     UTF-8 or lone surrogate as the replacement character, never as no
//     record.
//   - ABSENT IS NOT NULL: `parentUuid` is handed over as written
//     ([threadHead.Parent]), within [threadValueMax], since the harness's
//     bookkeeping carries none and only a root carries null.
//   - A KEY ONLY ONE READER READS IS HELD IN THAT READER'S ACCESSOR, so a
//     release that reshapes it costs that reader alone ([threadLine]); only
//     the bounds above fail a whole line.
//   - A MESSAGE THAT DOES NOT DECODE IS ITS LINE'S: a reader that decides from
//     a thread's messages asks [threadLine.Message] of every user and
//     assistant record of that thread, and takes its error as that line's, no
//     record, as it takes a *threadLineError, never as a line with no calls.
//   - A HOOK'S FIRING IS NEVER A CALL'S ID: no id leaves the layer AS an id
//     but a call's, through [threadMessage.Calls], and an attachment's
//     `toolUseID` leaves it only as a key ([threadLine.AttachmentToolUseID]),
//     since a PostToolUse hook's firing carries the id of the call it followed.
//     An id written inside a record's text (a tool's output, a call's input)
//     never leaves it as bytes: of a call's input, a result's text, a record's
//     own toolUseResult and a message's text the layer hands out only the facts
//     it reads of them ([threadCall], [threadLine.HandsBack],
//     [threadLine.AgentAnswer], [threadMessage.NamesHandback]). A task
//     notification's `<tool-use-id>`, the text's own claim, leaves it as the
//     leg it names ([threadMessage.Notification]), which its one reader matches
//     only against legs whose call it read.
//
// What these rules were measured on is Claude Code 2.1.310 and 2.1.322, driven
// as the engine drives a box's executor, and 2.1.118 beside 2.1.310 for a hook
// that follows one call (a skill's). The take's context meter (measure.go)
// reads the main thread through the layer in one walk of its lines
// ([walkThread]): it decides what a continuation of the capture is told, by
// what Claude Code counts the session at ([resumedReading]), and reads the
// launch floor's figure ([contextReading]).
//
// What Claude Code's resume hands its model of such a thread, which of its
// records and in what order, is modelled once, by [threadChain]
// (claudechain.go), for every reader that judges a resumed session by it:
// from the shipped code of the releases it was read from, a set and never a
// range ([claudeResumeRead]), per variant of the release and the flags a
// reading cannot see, with three answers, handed, not handed and cannot tell,
// and the reason for each. Its type doc is the model, each rule beside the
// offsets of the code it restates, and a thread holding a shape no model holds
// is cannot tell whole.
//
// The reading that tells a relaunch which of its round's calls a kept session
// holds (claudeholdings.go) reads a kept archive's main thread, and each
// depth-one subagent's thread and metadata, through the layer, metering the
// archive at [threadPlainMax] with the scrub's own meter and handing each
// member it reads what is left of that budget.
//
// # A thread line is read in one scanner pass, never held
//
// The reader (claudelinescan.go, [threadReader]) reads a line through
// [jsonscan] in ONE PASS that never holds it. A differential holds the pass to
// a decode of the whole line, line by line, as a test's oracle
// (claudelinescan_internal_test.go).
//
// A LINE IS ITS OWN READER ([lineSource]): it ends at the break and hands the
// scanner the line without it, so the scanner reads one JSON text a line.
// Every byte it takes of the member passes first through the member's meter
// and through the line's byte facts ([lineBytes]), a tee of the raw bytes,
// never of the tokens, since a pattern a reader of bytes reads can begin
// inside a name, through an escaped quote ([threadBytes]). The context is
// asked before each piece of at most [lineChunkBytes], so a reading overruns
// its caller's wait by one piece whatever the line's length, and a line
// refused part way is drained to its break through the same take.
//
// NOTHING OF A LINE COMMITS BEFORE ITS BREAK AND THE SCANNER'S END OF IT, and
// a piece the member ends with no break commits nothing. Then the line is
// judged in the order Claude Code's load reads it ([threadReader.commit]): a
// fault of the grammar first, wherever it lies (a NUL or a byte order mark
// first is faultLead, a cut value faultPrefix, too deep faultDepth, more after
// the value faultTrailing, else faultSyntax), since Claude Code's load parses
// the whole value before it reads its head; then one value that is no object;
// then the head, each key by its last occurrence; then the bounds below; only
// then a record, each frame settled to its type ([settle]). So a late fault
// beats what the line said before it, and a line past a bound is classed
// exactly where its grammar or head refuses it: past a bound the pass keeps
// scanning, holding nothing more.
//
// EVERY MEMBER IS READ BY ITS DECODED NAME, exactly, case kept, as
// [threadDecoding] reads a key (`"type"` is `type`; `"Type"` no key the layer
// reads), and a key written twice by its last value, wholesale: each
// occurrence replaces what the one before it left, a frame's all of it, never
// merged with it.
//
// EVERY VALUE IS READ BY ONE OF THREE MEANS, or passed over unheld
// ([jsonscan.Scanner.Skip]). A small value the layer's own code decodes (a
// uuid, a timestamp, a usage, a block's id) is CAPTURED as written within
// [threadValueMax] ([jsonscan.Scanner.Capture]), copied into the facts or held
// in the reader's buffers until its object ends; a capture past that bound
// fails the line closed, never read as a guess. A container its readers walk
// is a FRAME, read member by member, keeping only what its readers ask (a
// message's kind, its blocks counted by kind, its calls) and deciding each
// object by the layer's one rule of it ([blockCallOf]) once every key of it
// has been met. A text its readers ask a property of STREAMS into the
// automaton that answers it at any length ([textSays], [textNotice],
// [textHandback], [textResult]). A list is read an element at a time by the
// layer's one rule of an element, up to the first it refuses, the rest passed
// over; an entry read by its first characters (a retraction's, a kept uuid) is
// read one byte past them ([lineDecoder.opening]), and a skill's name streams
// into its UTF-16 length and whether its bytes were UTF-8
// ([jsonscan.Scanner.InvalidUTF8]).
//
// A RECORD'S TYPE GATES ITS FRAMES ([gates]): a frame read on records of some
// types alone is passed over where the `type` written before it names none of
// them ([lineDecoder.admits]), since an api-request-blob writes its type first
// and then a message repeating a whole request. A type is known only at the
// line's end, though (an assistant writes its message first), so a frame met
// before any type, or after one that is no string, is read; a line whose last
// type reads a frame passed over for an earlier one fails closed; and a frame
// the last type or subtype does not read is dropped at the commit.
//
// THE FACTS ARE HELD TO [threadLineMax] IN SUM ([lineMeter]), since a bound on
// each value lets a line of a million tiny calls hold a million calls: every
// allocation the facts keep is charged as it is made, an array before it is
// made ([grow]), and nothing is refunded, so a key written many times pays for
// every copy. What a line allocates and drops (a refused block's strings, an
// error's text) is not charged: a line of many blocks costs time, not memory.
//
// EIGHT MEBIBYTES, a constant of its own and never the scrub's piece
// ([scrubPieceMaxBytes]), which bounds what the scrub holds of a line rather
// than what a reader keeps of one. Every legitimate line's facts are under a
// KiB: its readers keep a block's kind, a call's id, name and target, a
// result's length and digests, a few small values, never a value's text. Of the
// 59,519 lines of the 1,091 threads the probes kept and this package's testdata
// holds, the most any line's facts hold is 753 bytes, and the longest line,
// 2,002,134 bytes, holds 51. So a reading's coincident worst is the resume's
// model of the thread at its bounds ([threadChain]) beside one line's facts at
// the bound and the reader's fixed buffers. A larger bound would serve only
// lines no release writes: what this one refuses is a line of tiny calls by the
// tens of thousands, a value read whole past [threadValueMax] (an id, a uuid, a
// usage), or a type that turns after a frame passed over for it, each failing
// toward no reading or cannot tell.
//
// MEASURED on a dedicated two-core build slot, one case a process, a time the
// spread of three runs; held is the largest live heap under GODEBUG=gctrace=1
// at GOGC=10, a peak the largest heap at the default. A reader's fixed buffers
// (bufio's 64 KiB, the scanner's 67,072 bytes, two text buffers of 4 KiB, the
// automata) hold about 146 KiB, and what no reader reads of a line costs
// nothing more, whatever its length (BenchmarkAReaderAtALineOfAnyLength): a
// whole PDF's Read, 55,924,056 bytes on one line, reads in 26 to 35 ms holding
// 146.5 to 151.5 KiB, and a 64 MiB prompt in 264 to 268 ms holding 145.8 KiB. A
// line of the smallest results, which no release writes, holds 16,384 of them
// within the bound, the reader and its facts 2,450 KiB, read in 34 to 42 ms,
// and one more is refused, a line of about 656 KiB, since a list is charged at
// every size it doubles through. A line at the bound read beside the resume's
// model at both its bounds held 39 MiB at most, its heap peaking at 63 MiB, and
// the whole-PDF line beside it 35 MiB (BenchmarkAThreadChainAtItsBounds):
// within the about 133 MiB a seat's read for words holds in the same transcript
// slot ([SeatTranscriptReadsAtOnce]).
//
// # A run's harness session, carried between boxes
//
// Every run names the harness session it works in before the harness starts
// ([Harness.Session]), records which it took up ([Paths.SessionRecord]), and
// packs it once the harness exits ([Paths.SessionCapture],
// [Runner.OpenCapture]); a run handed a session packed in another box restores
// it before its harness takes it up, never over a box whose own copy already
// holds that archive ([Paths.SessionDigests]), and starts a session of its
// own, saying why, where it can do neither. Whether a box can continue a
// carried session at all is its harness's release, asked of the box
// ([Runner.CheckCarry]). What each harness's archive holds, and never holds,
// is the harness's own: see carry.go, [ClaudeCode] and [OpenCode].
//
// A SESSION ITS HARNESS IS STILL WORKING IN is packed by the engine, beside
// it, by the pack the run's own end makes held to a stricter rule — every file
// it packs listed before the tar and checked after it, so a file the harness
// rewrote under the pack fails the pack rather than riding the archive torn
// ([claudeHeldTar]): a job a box would not end, packed on demand for the read
// that packs it alone ([Runner.PackCapture], [Paths.SessionPackHeld]), and a
// SAVE, packed to a place of that save's own, named after its taker
// ([Runner.PackSave], [Paths.SessionSave]), for the take that names it alone —
// neither ever where the job's final archive is read. Only Claude Code's
// runner makes a save ([saver]). And a job that is over and left no archive is
// packed on demand for its read alone too, by the run's own end's rule: the
// capture is the job's own script's, which nothing else writes.
//
// # A run ends on request
//
// The read that takes a stopped or lost job's session, or that of a job still
// working past its turn's cap, first asks the job to end: SIGUSR1 to the run's
// script, by the pid it recorded in its box before it did anything else
// ([Paths.JobScript], [Runner.AskEnd]). The script ends its
// harness — SIGTERM, then SIGKILL after the grace; an OpenCode run first
// interrupts its session through its own server, so the step in flight is
// settled in what it exports — packs the session as its own exit packs it,
// records that it was ended and how ([Paths.JobEnded]), and exits with its
// harness's status, which the collection never reads as a success
// ([sandbox.Result.JobEnd]). See end.go.
//
// It touches only the [sandbox.Sandbox] interface, so the in-process fake is a
// faithful substitute and every runner is testable without a real harness.
package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// Paths is every artefact path for one box, derived from its home.
//
// ONE OBJECT rather than a bag of constants, and derived rather than fixed. A
// relocated result file paired with a default-home done marker polls forever,
// and a local backend runs many boxes on one filesystem — a shared
// /home/user/.crewlet would have every run reading its neighbour's marker.
type Paths struct{ Home string }

// PathsFor returns the artefact paths for a box.
func PathsFor(box sandbox.Sandbox) Paths {
	home := sandbox.DefaultHome
	if box != nil {
		if h := strings.TrimRight(box.Home(), "/"); h != "" {
			home = h
		}
	}
	return Paths{Home: home}
}

// WorkDir is the side directory for the detached artefacts.
//
// Outside the checkout, so a `git clean` in the brief cannot wipe them.
func (p Paths) WorkDir() string { return p.Home + "/.crewlet" }

// Result is where the agent's RESULT is read from whole: its stdout, for a
// harness whose stdout is its result or its event stream; the stream's last line,
// copied there after exit, for one that keeps its stream apart ([Output]).
func (p Paths) Result() string { return p.WorkDir() + "/result.json" }

// Stream is where a harness that keeps its event stream apart from its result
// sends its stdout — a file that grows with every tool call the run makes,
// read as a stream and from its end, never whole.
func (p Paths) Stream() string { return p.WorkDir() + "/stream.jsonl" }

// BaseResult is where a run that CONTINUES a session in this box keeps the
// result the job before it left ([Paths.Result]), copied there as the run
// starts and before its own artefacts are cleared: what the session had
// already spent when this run took it up. A harness whose result states the
// SESSION's spend rather than the run's own ([sessionTotals]) is charged the
// difference, so a continued run is never charged its predecessors' calls
// again.
func (p Paths) BaseResult() string { return p.WorkDir() + "/base-result.json" }

// SessionRecord is where a run's script records, before its harness starts,
// the session the harness runs in, whether that is the one the run was asked
// to continue, why not where it was asked and could not, and the harness's own
// `--version` — one `<key> <value>` line each ([sessionRecord]): what the
// collection reports the run did ([sandbox.Result.Continued]) and whether its
// harness's result states the session's spend or the run's ([sessionTotals]).
// Written in the box, because only the run's script knows which session it
// took up, and the harness that answers `--version` is the one the box's PATH
// finds there; before the harness starts, so a run killed before it reported
// anything still names its session.
func (p Paths) SessionRecord() string { return p.WorkDir() + "/session" }

// SessionCapture is where a run's script packs the session its harness ran in
// once the harness has exited ([sandbox.Result.Captured]), in its harness's own
// archive format ([carrier]): moved into place whole from
// [Paths.SessionStaging], so a reader finds the last whole capture or none.
//
// THE SCRIPT'S ALONE, written once, as its harness exits: no pack the engine
// makes on demand moves anything here ([Paths.SessionPackHeld]), so a read that
// streams the archive the job left streams one nothing moves over while it
// reads it, and what [sandbox.Result.Captured] describes is what the script
// left.
func (p Paths) SessionCapture() string { return p.WorkDir() + "/session-capture" }

// SessionCarried is where a run that continues a session carried from another
// box has that session's archive written as it starts
// ([sandbox.RunRequest.Carried]), for its script to restore before the harness
// takes the session up. Removed by the script once it has been read.
func (p Paths) SessionCarried() string { return p.WorkDir() + "/session-carried" }

// SessionStaging is the DIRECTORY a run's script packs a capture and unpacks a
// carried session in before it moves either into place — so neither is ever
// found half written where it is read — removed whole when the script is done
// with it, and by the clear before every run ([Runner.ClearArtifacts]).
func (p Paths) SessionStaging() string { return p.WorkDir() + "/session-staging" }

// SessionPackStaging is the DIRECTORY one pack made on demand
// ([Runner.PackCapture]) packs a capture in before it moves it to its own
// place ([Paths.SessionPackHeld]), named by pack, a name the runner mints for
// that pack alone: its own, never [Paths.SessionStaging], because the run's
// script may still be working in that one — a job a box would not end is
// packed while it runs, and its script packs into the staging directory the
// moment its harness exits, which would clear a pack made there under the
// other's feet.
//
// AND NEVER ANOTHER PACK'S, for the same reason: two reads of one job at once
// — a stop's and a collection's — each take its session, and where the job
// left no archive, or its box would not end it, each packs it now. Sharing one
// directory, each pack's start would clear the other's files: one would move
// to its place an archive the other had half made, as its own, and the
// other's move would find nothing to move. Beside a working harness, too,
// a pack would read the other's lists for its own, or find its own gone, and
// the archive it moved could be one whose checks never ran ([claudeHeldTar]).
// Each in its own, the two archives are each whole, each moved into place in
// one rename.
//
// Removed whole by the pack when it is done, and by the clear before every
// run, which removes every pack's, whichever read made it ([packStagingFiles],
// [Runner.ClearArtifacts]).
func (p Paths) SessionPackStaging(pack string) string {
	return p.WorkDir() + "/session-pack-staging." + pack
}

// SessionPackHeld is where a pack on demand ([Runner.PackCapture]) keeps its
// archive — of a job that is over as much as of one STILL WORKING — moved there
// whole from its own staging directory ([Paths.SessionPackStaging]) and held
// for the one read that packed it: streamed from there, and removed once that
// read is done with it.
//
// NEVER [Paths.SessionCapture]. A Claude Code take reads its archive twice and
// opens it again by its path for the second pass ([scrubTar]), so a read that
// streams a path something else moves an archive over is refused whenever that
// move falls between its two passes. Over the capture, two reads of one job
// each moved their own archive and streamed what the capture held when they
// opened it — the other's, where its move came between; the job's own end
// pack moves the job's final archive there the moment its harness exits, which
// can come while this pack runs, a pack beside a working harness taking
// seconds to check what it read ([claudeHeldTar]); and moved there after that,
// the archive of the job as it stood would replace the final one, and a read
// of the job once over would take it as everything the job did. The pack's
// own, each read streams its own archive and nothing else.
//
// NAMED AFTER THE PACK'S STAGING DIRECTORY, so the clear before every run
// removes one a read cut short left by the same pattern ([packStagingFiles]).
func (p Paths) SessionPackHeld(pack string) string {
	return p.SessionPackStaging(pack) + ".held"
}

// packStagingFiles is the pattern every pack on demand's files match — its
// staging directory ([Paths.SessionPackStaging]), and the archive it keeps
// beside it for its read ([Paths.SessionPackHeld]) — whichever pack made them:
// what the clear before every run removes; relative to the work directory,
// which the clear quotes.
const packStagingFiles = "session-pack-staging.*"

// SessionSave is where ONE SAVE of the session a run's harness is still
// working in is packed for taker, the holder of the lease it is taken under
// (`<node>:<uuid>`, internal/sandbox's taker lease), named by pack, the name
// the runner mints for that save alone: moved into place whole from that
// save's own [Paths.SessionSaveStaging] by [Runner.PackSave], streamed by the
// take that names it ([sandbox.SessionTake.Save]), and removed once that take
// is done with it.
//
// NEVER [Paths.SessionCapture]. That path is the job's final archive to every
// read of a job that is over — its collection, a stop's, a loss's — so a save
// left there would be taken as the last archive of a job whose own exit pack
// failed, a session from before its last calls read as everything it did.
//
// AND THE SAVE'S OWN, never one path per taker: two saves of one taker at once
// — the drain's beside one the waiter asked for — each moved its archive over
// the taker's one path, and a Claude Code take reads its archive twice,
// opening it again by its path for its second pass ([scrubTar]), so a take of
// the one save was refused wherever the other's move fell between its passes.
// Each save's own, a take streams the archive its own save made and nothing
// else, as a pack on demand's take streams its own ([Paths.SessionPackHeld]);
// and two processes that each save one box — an e2b box whose waiter duty
// changed hands mid-save — never pack into, or read, one file.
//
// NAMED BY A DIGEST OF THE TAKER ([saveTag]), never the holder itself: a
// holder carries a node's id, which is the deployment's to choose and is no
// file name's to trust.
func (p Paths) SessionSave(taker, pack string) string {
	return p.WorkDir() + "/session-save." + saveTag(taker) + "." + pack
}

// SessionSaveStaging is the DIRECTORY one save for taker is packed in before it
// is moved to its own place ([Paths.SessionSave], [Runner.PackSave]), named by
// pack, the name the runner mints for that save alone, after its taker's tag:
// its own, for [Paths.SessionPackStaging]'s reasons — the run's script's
// staging directory, another pack's, ANOTHER SAVE'S, even its own taker's: two
// saves of one taker at once sharing one directory, each one's start would
// clear the other's lists and archive, and one's move could put in its place
// an archive the other had half made, whose checks never ran. Removed whole by
// the pack when it is done, and by the clear before every run, which removes
// every save's files, whoever took them ([Runner.ClearArtifacts]).
func (p Paths) SessionSaveStaging(taker, pack string) string {
	return p.WorkDir() + "/session-save-staging." + saveTag(taker) + "." + pack
}

// saveFiles is the pattern every save's files match, the archive and its
// staging directory alike, whichever taker packed them — what the clear before
// every run removes ([Runner.ClearArtifacts]); relative to the work directory,
// which the clear quotes.
const saveFiles = "session-save*"

// saveTag is the name a taker's own save files carry: the first [saveTagBytes]
// of the SHA-256 of its holder, in hex — letters and digits alone, whatever
// the holder holds, and the same on every node that names the same holder.
func saveTag(taker string) string {
	sum := sha256.Sum256([]byte(taker))
	return hex.EncodeToString(sum[:saveTagBytes])
}

// saveTagBytes is how much of a taker's digest its save files are named by.
//
// SIXTEEN BYTES, thirty-two hex digits: the takers that ever save one box are
// a handful — the node that launched a local box, the waiter duty's holders an
// e2b box passes between — so a collision of 128 bits is never met, and the
// name stays short enough to read in a listing of the box.
const saveTagBytes = 16

// SessionDigests is where a box keeps the digest of every archive of a session
// its own copy holds — one line, `<session> <cksum of the archive>`, for each
// archive a run's script packed there ([Paths.SessionCapture]) or restored
// there ([Paths.SessionCarried]) — so a run handed an archive of a session the
// box already holds can tell an archive its copy holds, which it never writes
// over, from one its copy does not, which replaces it (see carry.go).
//
// KEPT FOR THE BOX'S LIFE: the clear before every run leaves it
// ([Runner.ClearArtifacts]), since the copy it describes outlives every run,
// and nothing else reads it. Every pack of a box's session records here, so a
// pack made any other way is one this box would otherwise take for another
// box's.
func (p Paths) SessionDigests() string { return p.WorkDir() + "/session-digests" }

// ClaudeConfigDir is the directory a Claude Code run keeps its state in, which
// the run pins as CLAUDE_CONFIG_DIR ([claudeSessionLayout]): the box home's
// `.claude`, where the harness keeps it by default and where a local box is
// seeded with the entry's login (`.claude/.credentials.json`). Named by the
// box's own home, as every path the runner hands the box is, so no profile the
// login shell sources can move it.
func (p Paths) ClaudeConfigDir() string { return p.Home + "/.claude" }

// Brief is where the brief is written for a harness that reads it on stdin
// ([Output.BriefOnStdin]) rather than on its argv.
func (p Paths) Brief() string { return p.WorkDir() + "/brief.md" }

// FreshBrief is where a run asked to continue a session has the brief written
// that a session of its own is handed instead ([sandbox.RunRequest.FreshBrief]):
// its script points its harness's stdin at it where it finds the session
// neither held by its box nor restorable, and starts a new one — so a new
// session is never handed a brief written for a model that remembers.
func (p Paths) FreshBrief() string { return p.WorkDir() + "/brief-fresh.md" }

// System is where a run's standing instructions ([sandbox.RunRequest.System])
// are written for its harness to add to its own system prompt: named on Claude
// Code's command line, listed in the configuration an OpenCode run is handed
// ([Paths.OpenCodeConfig]).
//
// A FILE, never argv. The text carries the seat's org chart, policies and
// memory, and a command line is readable by every process that can list them
// — on a `direct` box, every account on the engine host — and bounded by
// ARG_MAX. Under WorkDir, out of the checkout, so a `git clean` cannot remove
// it from under a run and a commit cannot carry it into a repository.
func (p Paths) System() string { return p.WorkDir() + "/system.md" }

// ClaudeNotesSystem is where a Claude Code run wired to its turn's notes has
// its standing instructions written a second time, with its harness's instruction
// about notes after them ([ClaudeCodeSteerInstruction]): the text the run's
// script names in place of [Paths.System] on a release whose note hooks it
// starts ([claudeNotesGate]), so the instruction reaches a run its notes can
// reach and no other. Beside it, and a file for the reason it is.
func (p Paths) ClaudeNotesSystem() string { return p.WorkDir() + "/system-notes.md" }

// CredentialDocument is where a box keeps the login a harness holds in a database
// of its own: the harness's own export of it, which the engine seeds a local box
// with and writes back from when the box closes ([CredentialDocument]).
func (p Paths) CredentialDocument() string { return p.Home + "/" + CredentialDocument }

// CredentialDocument is [Paths.CredentialDocument] relative to the box home —
// the key a run's credential files map it under, because a provider seeds
// those relative to the box's home ([sandbox.Spec.CredentialFiles]).
//
// Under the artefact directory rather than the harness's own data directory: no
// harness reads this file on its own — the runner imports it into the harness's
// database before the run and exports the database back into it while the run
// works and once more after — so it lives with the rest of what the engine
// keeps in a box, where a `git clean` in the brief cannot reach it.
const CredentialDocument = ".crewlet/exported-credentials"

// Err is where its stderr goes — the transcript's fallback source.
func (p Paths) Err() string { return p.WorkDir() + "/err.log" }

// Done is the done marker AND the exit code.
//
// It must be NON-EMPTY. ReadFile returns nothing for both a missing and an
// empty file, so a zero-byte marker would read identically to "not written
// yet" and Poll would never see it — which is why the shell writes the exit
// code into it rather than touching it.
func (p Paths) Done() string { return p.WorkDir() + "/done" }

// ExitCode is the wrapper's status, surfaced on collect to explain a crash.
func (p Paths) ExitCode() string { return p.WorkDir() + "/exit_code" }

// MemoryBefore and MemoryAfter are the job's memory domain as its script read
// it before its harness started and after it exited ([sandbox.MemoryDomainScript]):
// a kill the kernel counted between them is one for memory during the job.
func (p Paths) MemoryBefore() string { return p.WorkDir() + "/memory_before" }

// MemoryAfter is the job's memory domain after its harness exited.
func (p Paths) MemoryAfter() string { return p.WorkDir() + "/memory_after" }

// JobScript is where a run's script records, before it does anything else, the
// end protocol it speaks and its own pid — `1 <pid>` — once it has set the
// handler that ends its job on SIGUSR1 (end.go): what the read that ends a
// stopped or lost job sends the request to ([Runner.AskEnd]). Moved into place
// whole, so a reader finds the record or none; removed by the clear before
// every run ([Runner.ClearArtifacts]), so a run is never asked through the last
// one's.
func (p Paths) JobScript() string { return p.WorkDir() + "/job-script" }

// JobEnded is where a run's script records that its job was ended on request,
// and how — `asked` (its harness told to stop went), `killed` (it outlived its
// grace) or `before` (the request came before its harness started) — as soon
// as its harness is over and before it packs, so the record stands where the
// pack is cut short; read by the collection ([sandbox.Result.JobEnd]) and
// removed by the clear before every run.
func (p Paths) JobEnded() string { return p.WorkDir() + "/job-ended" }

// BinDir is the box's own bin directory, first on the run's PATH
// ([withBoxBin]): where a setup step installs an executable for the run, a
// harness among them, without root.
//
// Under the box's own home, not a system path: a local backend runs as an
// unprivileged engine user with no business writing to /usr/local/bin, and two
// boxes there would overwrite each other's tools.
func (p Paths) BinDir() string { return p.WorkDir() + "/bin" }

// MCPConfig is where the scoped MCP surface is rendered. Removed by the clear
// before every run ([Runner.ClearArtifacts]), so each run's start writes its own
// as a new file and a run with no servers keeps none naming a closed bridge session.
func (p Paths) MCPConfig() string { return p.WorkDir() + "/mcp.json" }

// MCPConfigFresh is where a Claude Code run that continues a session has the
// MCP surface rendered under the job's other bridge token, for a session its
// script starts of its own in that one's place
// ([sandbox.RunRequest.FreshWiring]): the script moves it over [Paths.MCPConfig]
// where it starts one, and removes it where it continues, in both cases before
// the harness starts, so the box holds one token when its model can first read
// a file. Beside mcp.json for the reason mcp.json is where it is.
func (p Paths) MCPConfigFresh() string { return p.WorkDir() + "/mcp-fresh.json" }

// ClaudeSettings is where every Claude Code run's own settings file is written
// — the setting that holds the harness's own retention sweep off
// ([claudeSettings]) — named on its command line with `--settings`, unless the
// run is started with [Paths.ClaudeNotesSettings] in its place.
//
// BESIDE mcp.json, never in the checkout's `.claude/`: the checkout is the
// repository's, which the run edits and a commit carries, and a file there would
// replace the repository's own project settings rather than sit above them.
// `--settings` adds a layer the harness ranks above the repository's settings and
// below a managed policy, so the repository keeps its own and the run's settings
// are there whatever it holds. Under WorkDir, so a `git clean` in the brief
// cannot remove it from under the run.
func (p Paths) ClaudeSettings() string { return p.WorkDir() + "/claude-settings.json" }

// ClaudeNotesSettings is where a Claude Code run wired to its turn's notes has
// its settings written a second time, with the hooks that hand its main agent a
// person's notes between its steps ([claudeNotesSettings]): the file the run's
// script names in place of [Paths.ClaudeSettings] on a release whose note hooks
// it starts ([claudeNotesGate]). Beside it, and in the work directory for the
// reason it is.
func (p Paths) ClaudeNotesSettings() string { return p.WorkDir() + "/claude-settings-notes.json" }

// ClaudeSkillsSettings is where a Claude Code run handed skills of its own has
// its settings written with hooks switched on ([claudeHooksOnSettings]), so
// the plugin's hook that reports each load runs whatever the repository's own
// settings say: the file the run's script names in place of
// [Paths.ClaudeSettings] on a release that runs that hook ([claudeSkillsGate]),
// unless it starts the notes' hooks, whose own settings switch hooks on too
// ([Paths.ClaudeNotesSettings]). Beside the others, for their reason.
func (p Paths) ClaudeSkillsSettings() string { return p.WorkDir() + "/claude-settings-skills.json" }

// ClaudeSettingsPinned is [Paths.ClaudeSettings]'s pinned copy: the same
// settings with the compaction point and a resume's load pinned besides
// ([claudeSettingsPin]), which the run's script names in that file's place on
// a release that takes the pin ([claudePinGate]). Beside it, for its reason.
func (p Paths) ClaudeSettingsPinned() string { return p.WorkDir() + "/claude-settings-pinned.json" }

// ClaudeNotesSettingsPinned is [Paths.ClaudeNotesSettings]'s pinned copy
// ([Paths.ClaudeSettingsPinned]).
func (p Paths) ClaudeNotesSettingsPinned() string {
	return p.WorkDir() + "/claude-settings-notes-pinned.json"
}

// ClaudeSkillsSettingsPinned is [Paths.ClaudeSkillsSettings]'s pinned copy
// ([Paths.ClaudeSettingsPinned]).
func (p Paths) ClaudeSkillsSettingsPinned() string {
	return p.WorkDir() + "/claude-settings-skills-pinned.json"
}

// OpenCodeConfig is where an OpenCode run's own configuration is written
// ([OpenCode.WriteConfig]), and read from by the run's script, which hands its
// content to the run's server alone as OPENCODE_CONFIG_CONTENT
// ([OpenCode.Command]).
//
// BESIDE the run's other artefacts, never in the checkout: the checkout is the
// repository's, and an opencode.json written there would replace the
// repository's own file, where content handed to the server ranks above it and
// leaves it in place. Under WorkDir, so a `git clean` in the brief cannot
// remove it from under the run.
func (p Paths) OpenCodeConfig() string { return p.WorkDir() + "/opencode.json" }

// OpenCodeValues is the directory holding the values an OpenCode run's MCP
// servers are configured with — each server's url, each header value, each
// environment value — one file apiece, which [Paths.OpenCodeConfig] names by a
// `{file:<path>}` reference rather than carrying the value
// ([openCodeValueFiles]). A directory of the run's own, removed whole before
// every run ([Runner.ClearArtifacts]), so a run configured with fewer values
// than the last keeps none of the last one's.
func (p Paths) OpenCodeValues() string { return p.WorkDir() + "/opencode-values" }

// OpenCodeValuesFresh is the directory holding an OpenCode run's values as they
// are under the job's other bridge token, for a session its script starts of
// its own in place of the one it continues ([sandbox.RunRequest.FreshWiring]):
// the same files by the same names, so the configuration's references name
// either set, which the script moves over [Paths.OpenCodeValues] where it
// starts one and removes where it continues — before the run's server starts,
// which is what reads them.
func (p Paths) OpenCodeValuesFresh() string { return p.WorkDir() + "/opencode-values-fresh" }

// OpenCodeSteerPlugin is the DIRECTORY an OpenCode run's notes plugin is written
// into, as index.js ([OpenCode.WriteConfig]), and named by the run's
// configuration — the plugin that admits a person's notes to the run's turn into
// its session at its next step (opencode_steer.js).
//
// A DIRECTORY, because OpenCode 2 loads a configured local plugin only from one
// — a file named there is skipped with a warning — trying its `server` module
// and then its `index`. Under WorkDir, beside the configuration that names it,
// never in the checkout or a `.opencode/plugins/` OpenCode would discover on its
// own; and its own, removed whole before every run ([Runner.ClearArtifacts]):
// OpenCode watches a configured plugin's directory and reloads the plugin on any
// change inside it, so nothing is written there once the run's server starts —
// the plugin itself writes nothing at all.
func (p Paths) OpenCodeSteerPlugin() string { return p.WorkDir() + "/opencode-steer" }

// ClaudeSkillsPlugin is the DIRECTORY of the plugin a Claude Code run is handed
// its own skills in ([sandbox.RunRequest.Skills], [ClaudeCode.WriteConfig]):
// its manifest, a directory per skill, and the hook that reports each load to
// the run's bridge — named on the run's command line with `--plugin-dir`, so
// the skills are the run's alone, under the plugin's name. Under WorkDir, never
// in the checkout or the box's `.claude/skills/`, where a commit could carry
// them or the next run in the box would find them; and its own, removed whole
// before every run ([Runner.ClearArtifacts]).
func (p Paths) ClaudeSkillsPlugin() string { return p.WorkDir() + "/claude-skills" }

// OpenCodeSkills is the DIRECTORY an OpenCode run's own skills are written into
// ([sandbox.RunRequest.Skills], [OpenCode.WriteConfig]), a directory per skill,
// and named in the run's configuration's `skills`. Its own, under WorkDir, for
// the reasons [Paths.ClaudeSkillsPlugin] gives; and written before the run's
// server starts, which watches it.
func (p Paths) OpenCodeSkills() string { return p.WorkDir() + "/opencode-skills" }

// OpenCodeSkillsPlugin is the DIRECTORY of the plugin that reports each skill an
// OpenCode run's model loads to the run's bridge (opencode_skills.js), written
// and named as the notes plugin is ([Paths.OpenCodeSteerPlugin]), and for the
// same reasons a directory of its own.
func (p Paths) OpenCodeSkillsPlugin() string { return p.WorkDir() + "/opencode-skill-loads" }

// CommitAttribution is the DIRECTORY holding a run's commit attribution
// (attribution.go): the hook ([Paths.AttributionHook]) and the directory the
// run's core.hooksPath names ([Paths.GitHooks]). Under WorkDir, never in the
// checkout, where a commit could carry it and a `git clean` could remove it;
// and its own, removed whole before every run ([Runner.ClearArtifacts]).
func (p Paths) CommitAttribution() string { return p.WorkDir() + "/commit-attribution" }

// AttributionHook is the one script every hook name in [Paths.GitHooks] links
// to.
func (p Paths) AttributionHook() string { return p.CommitAttribution() + "/hook" }

// GitHooks is the directory a run's core.hooksPath names, linking
// [Paths.AttributionHook] under every hook git runs by name.
func (p Paths) GitHooks() string { return p.CommitAttribution() + "/hooks" }

// Findings is the agent's authoritative final report.
//
// THE RESULT CARRIER OF RECORD. A run whose process group died can lose its
// streamed final message, and a tool-only run parses to no text at all — but
// this file survives both, so Collect prefers it for the result text and
// treats its presence as the success signal. Under WorkDir for the same reason
// the rest is.
func (p Paths) Findings() string { return p.WorkDir() + "/findings.md" }
