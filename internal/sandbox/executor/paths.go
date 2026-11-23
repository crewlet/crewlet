// Package executor is the detached-run plumbing every harness runner shares.
//
// A harness runs headless in a box in one shape: a background process writes
// its output to a file and drops a done marker, Poll checks the marker and the
// process, Collect decodes the stream and reads the result, and an ask overlay
// surfaces a mid-run question. Each runner supplies only its own: invocation,
// config, output paths and readers, how a person's notes to its run's turn
// reach it between its steps (ADR-0177; claudesteer.go, opencode_steer.js), and
// how the run's skills reach its harness (claudeskills.go, opencodeskills.go).
//
// # A run's commits credit Crewlet, through git
//
// A run whose request asks for it ([sandbox.RunRequest.Attribution]) has every
// commit it authors credit [CoAuthor] and name its seat, run and work item, by
// a git hook the runner installs: the same for every harness, whatever the
// model remembers (attribution.go).
//
// # A run's output is read as a stream
//
// The event and error streams grow with the run, so neither is read whole
// (stream.go).
//
// # A Claude Code thread is read a line at a time, holding what its readers read
//
// A person's transcript shows what it can; a reader that DECIDES from what a
// session recorded cannot, since one misread record is a wrong answer. So every
// such reader ([hookReading], [resumedReading], the holdings) reads a thread
// through one layer (claudethread.go) in one shared walk ([walkThread]), by
// rules held once:
//
//   - A MEMBER IS NAMED EXACTLY ([claudeMainThread], [claudeMemberOf]), never
//     by [claudeThread], which takes any session's file for the main thread.
//   - A LINE AT A TIME, IN ONE SCANNER PASS THAT NEVER HOLDS IT
//     ([threadReader]), since Claude Code bounds no line; a last piece with no
//     break is never a line (a torn write, tar's NUL padding).
//   - PAST A BOUND, THE READING IS OVER, never skipped, since a skipped line
//     leaves a hole a reader would decide across ([threadLinesMax],
//     [threadLineMax], [threadValueMax]): it fails closed, to no reading or
//     cannot tell, never a false hold.
//   - THE CONTEXT IS THE ONE PLACE A READING'S CALLER IS HEARD, asked before
//     each piece of at most [lineChunkBytes].
//   - EVERY LINE IS READ TO ITS END, with no keyword prefilter: a line no
//     keyword marks can still be torn.
//   - EVERY VALUE IS READ AS CLAUDE CODE'S OWN JSON.parse READS IT
//     ([threadDecoding]): a key exactly, never by case, and by its last value;
//     invalid UTF-8 as U+FFFD. Absent is not null ([threadHead.Parent]).
//   - A KEY ONLY ONE READER READS IS HELD IN THAT READER'S ACCESSOR
//     ([threadLine]), so a release that reshapes it costs that reader alone. A
//     message that does not decode fails its line ([threadLine.Message]).
//   - A HOOK'S FIRING IS NEVER A CALL'S ID: only a call's id leaves the layer
//     as one ([threadMessage.Calls]), since a PostToolUse firing carries its
//     call's; of a record's text it hands out only facts ([threadCall]).
//
// What a resume hands the model is modelled once, by [threadChain], from the
// shipped code of a set of releases, never a range ([claudeResumeRead]).
//
// # An OpenCode session's calls are read by what they were
//
// OpenCode names no call to the engine, so its holdings reading
// (opencodeholdings.go, contentverdict.go) holds a call by CLASS
// ([sandbox.CallClass]): tool, arguments' key, failure and output. A class is
// held only where every call of it in the pool has a part of its own after the
// run's start and the newest compaction, handed to the model verbatim; which of
// two identical calls a part is goes undecided, so a class is held or told
// whole.
//
// The reading visits the one export walk ([scanExport]) holding no value whole,
// and ends on any shape the measured releases do not write. A PART COUNTS only
// where it is a `crewlet_` tool's, settled exactly as a completed or failed
// call ([openCodeHoldingsWalk]); any other is passed, which can only tell a
// call. Its text is matched by digest ([matchedClass]).
//
// THE POOL is every call of the ask but those an approval held; one whose class
// cannot be stated, in flight or dropped is a WILDCARD
// ([contentCallPool.wild]), and held calls meet their parts in step order
// ([contentInOrder]). WHY IT NEVER HOLDS FALSELY: every counted part follows
// the run's start, every call the run made is in the pool, no other server
// joins to `crewlet_` ([openCodeShadowCheck]), and a call leaves one part:
// ADR-0073's text-equality standard. A release is added to
// [openCodeHoldingsReleases] only once the count reads it and the engine's
// bridge has driven it through every outcome, argument shape, end and
// compaction these rules count.
//
// # A collection's read of a thread: a base, then one slot
//
// A pass over a main thread, a collection's or a take's (its scrub and meter
// one pass, one budget), holds no token within its BASE and ONE slot of a pool
// of its own past it ([CollectionLargeReadsAtOnce], [passBudget]): past
// [collectionBaseLineCharge] of a line's facts ([lineMeter], never its length),
// [collectionBaseRecords] records, or the scrub's [scrubPieceBaseBytes]. A
// reading that keeps records holds the slot to its end; any other drops it
// after its piece, since a take holding the only slot through its scrub stalls
// everything behind it. A wait its context ends is [errLargeWait], never a
// reading.
//
// THE COEFFICIENTS are the layer's own worst over its bounds: the chain at both
// bounds 43 MiB ([threadChain]), a line's reader 1.7 MiB, a charge's strings
// 808 KiB, the facts 1.25 bytes a charged byte; [readerLineBytes] and
// [chargeStringsBytes] are measured by a test.
//
// THE BASE holds about 5.3 MiB ([baseTierReadBytes]); 2^13 chain items take at
// least 4,096 records, four times a day's session, so a typical collection
// never waits. No token holds the base tier, so it is every read in flight: a
// node's turns and up to 64 peer reads, 341 MiB, takes sized to match
// ([SessionTakesAtOnce]).
//
// THE LARGE TIER, ONE SLOT IN A POOL APART from [TranscriptReadsAtOnce]:
// shared, a person paging a transcript would decide how a relaunch is told. A
// waiter past internal/sandbox's MainThreadReadWait reads as no reading, the
// safe side.
//
// # The memory a node keeps for these pools
//
// [NodeWork.MemoryPools] adds up the pools a node runs, each at its worst plus
// half again for the heap's room. A Gitlet node reserves them first (gitproc's
// Options.Reserve), from the engine, which alone reads the roles: a registry
// filled at run time would size them by whichever pools started first.
//
// # A hook's note, confirmed off its job's main thread
//
// A note a Claude Code hook served counts as read only once the session shows
// it: [hookReading], beside the resume's model, answers each served take Held,
// Restarted, Absent, or none with why.
//
// A PAIR is a chained `hook_system_message` naming the take's token
// ([threadLine.HookTakes]), then the same firing's `hook_additional_context`; a
// firing is keyed by a digest, so the reading's state is fixed in size. A
// success's stdout is the hook's own answer, never a confirmation.
//
// HELD where both records are handed ([threadChain.Handed]) on a thread every
// size path of the resume reads alike ([threadChain.AnySize]), on a release
// whose resume was read, with no restart after. RESTARTED where one follows or,
// for a batch's pair, whose note rides a tool result, a later reply cleared
// them. SO A THREAD THAT COMPACTED DECIDES NO TAKE: a compaction binds it to
// the small path. ABSENT only where the thread proves it: read whole and
// unrestarted, its harness ending its own run, another take at the same event
// held (a release may reword one event's pair), and no take named outside its
// pair. Anything else is undecided: the safe side, a note told again.
//
// # A thread line is read in one scanner pass, never held
//
// [threadReader] reads a line through [jsonscan] in ONE PASS, held to a
// whole-line decode by a differential test. Its byte facts are a tee of raw
// bytes ([lineBytes]): a pattern can begin inside a name, past an escape.
//
// NOTHING OF A LINE COMMITS BEFORE ITS BREAK. It is judged in the order Claude
// Code's load reads it ([threadReader.commit]): a grammar fault anywhere first,
// since the load parses the whole value; then the head; then the bounds; then
// the record. So a late fault beats what the line said first.
//
// A value is CAPTURED within [threadValueMax], walked as a FRAME, STREAMED into
// an automaton, or passed over unheld. A RECORD'S TYPE GATES ITS FRAMES, since
// an api-request-blob repeats a whole request; a type is known only at the
// line's end, so a line whose last type reads a frame passed over fails closed.
//
// THE FACTS ARE HELD TO [threadLineMax] IN SUM ([lineMeter]), since a bound on
// each value lets a line of a million tiny calls hold a million calls: every
// allocation is charged as made, nothing refunded. EIGHT MEBIBYTES bounds what
// a reader keeps, never what a session holds: a real line's facts are under a
// KiB, so it refuses only what no release writes. Lines cost a reading time,
// not memory (BenchmarkAReadingAtItsBounds). THE ESTIMATE COSTS ITS PASS what
// counting costs, paid only by a reading that asks.
//
// # A run's harness session, carried between boxes
//
// Every run names its harness session before the harness starts
// ([Harness.Session]) and packs it once the harness exits
// ([Paths.SessionCapture]). A run handed one packed elsewhere restores it
// first, never over a copy holding that archive ([Paths.SessionDigests]), else
// starts its own, saying why (carry.go, [Runner.CheckCarry]).
//
// A SESSION ITS HARNESS IS STILL WORKING IN is packed by the engine under a
// stricter rule, every file checked after the tar, so one rewritten under the
// pack fails it ([claudeHeldTar]): on demand for one read
// ([Runner.PackCapture]) or as a SAVE for one take ([Runner.PackSave]), never
// where the job's final archive is read.
//
// # An OpenCode session saved beside its running server
//
// An OpenCode save is its own export ([openCodePackNow]) through a private
// `--standalone` server beside the run's own (both WAL, with a busy timeout),
// to the last settled message, the models fetch and updater off
// ([openCodeStandalonePins]). An export empty or past [heldPlainMax] is
// refused.
//
// ONE AT A TIME IN A NODE'S OWN MEMORY, AND WAITING FOR ROOM: an export peaks
// at up to ten times its session, so a save in a box sharing the node's memory
// takes the node's one pack slot and waits ([sandbox.ErrSaveNoRoom]) where its
// need ([openCodeSaveNeedFactor]) exceeds the box's room ([openCodeSaveRoom]).
// A take's pack ([OpenCode.Pack]) never waits: refused, it would lose the job's
// newest state on an estimate. SAVED ONLY ON A RELEASE MEASURED
// ([openCodeCountReleases]), since releases differ in what a standalone call
// writes.
//
// # A kept session leaves its box scrubbed
//
// A session holds every file its harness read and every command's output, so it
// leaves a box redacted (scrub.go): first BY VALUE, every secret the engine put
// in the box, raw and JSON-escaped, so an opaque token goes too and the second
// pass sees a marker, not half a value; then BY SHAPE, redact.Secrets.
//
// EACH FILE AS WHAT IT IS, so what is kept is what its harness restores: a
// transcript's line and a `.json` document rewritten string by string, a value
// under a credential's key taken out whole, and anything not JSON as text. A
// RECORD KEEPS ITS LINE AND ITS KEYS in the order written, since Claude Code
// reads a file past 5 MiB by where each line's keys sit. The tar's own header
// carries only kind, name, mode, size and time, its names scrubbed.
//
// A CREDENTIAL'S KEY ([credentialKey]) is read on its own words with its
// separators dropped, since each format joins them its own way
// (`aws_session_token`, `X-Amz-Security-Token`); never `token`, `key` or `auth`
// at large, which `input_tokens`, `AccessKeyId` and `author` hold.
//
// # The scrub holds no value whole
//
// A harness bounds no value (Claude Code writes a read PDF's base64 on one
// line), so the scrub holds NO LINE, STRING OR STRETCH OF TEXT WHOLE, and
// refuses none for its length: JSON on internal/jsonscan, values out through a
// [valueStream] and redact.Stream, which writes exactly redact.Secrets of the
// whole text.
//
// TWO READINGS, THE SECOND A COPY: what a unit is, and what a line loses, is
// known only at its end, and a tar names a size before its bytes. So the first
// reading rewrites into the take's SPOOL ([unitSpool]) and the second writes
// the tar from it ([spoolCopy]), never the box, whatever the box does
// meanwhile. THE PLAN ([tarPlan]) is bits, SO IT IS AT MOST A SIXTEENTH OF THE
// PLAINTEXT: a record a member would let a box's archive of a mebibyte cost the
// host gibibytes.
//
// A TEXT A TAKE FOUND UNCHANGED IS NOT REDACTED AGAIN ([scrubMemo]): every take
// scrubs the whole session, and a whole-PDF read cost 6.1 s a take. SOUND
// because a text's redaction is the text's alone, the memo keyed by digests
// under the take's own values, never the text; in memory alone, so a restart
// costs a take what it would without.
//
// # The scrub's bounds, and what it holds
//
// The archive is the box's own: a gzip bomb, a value nested a million deep. So
// a reading reads at most [scrubPlainMax] ([scrubBounds]), nothing deeper than
// jsonscan.MaxDepth as JSON, within internal/sandbox's SessionScrubWait, and
// holds at most [scrubHeldBytes]; a session past a bound, or one the redaction
// refuses ([errRedaction]), is refused with why.
//
// # A definition the scrub renamed is cut out of its line
//
// A Claude Code session resumed sends every tool definition it loaded by search
// ([loadedToolsRecord]) with each request, and one whose name the rewrite left
// outside what the provider takes ([providerToolName]) has every request
// refused: so a line holding a marker loses it, and the session searches again.
// CUT OUT OF THE LINE AS THE REWRITE WROTE IT, every other byte in place, never
// re-encoded from a map, which sorts the keys: 2.1.310 reads a subagent's
// record by `"isSidechain":true` in its first 256 bytes, and a pruning load
// walks only lines beginning `{"parentUuid":`. DECIDED IN THE FIRST READING,
// APPLIED IN THE SECOND ([definitionsReading], [cutFilter]).
//
// # An OpenCode continuation's first request, judged in its box
//
// OpenCode compacts before a request where its count reaches the point its box
// sets for the model, its limit less a buffer ([openCodePointAt]). Only the box
// can say it, so the launch reads it there ([Runner.ReadBoxPoint],
// opencodepoint.go), keeping only the values the point is decided by, since
// config.get resolves references.
//
// NO POINT, EACH A MARK ([sandbox.PointMark]), wherever the read cannot prove
// what OpenCode will do: a release outside the set or other than the writer's,
// a plugin not the engine's (it can compact first), a model whose limit nothing
// states, and any read that failed. In the box holding the session's copy, the
// context sources OpenCode hands over first are charged at their whole render
// ([openCodeBoxBytes]); one the read cannot size is no point.
//
// # What Claude Code estimates a line's content at
//
// Claude Code decides whether to compact by the last reply whose usage counts
// plus an ESTIMATE of every record after it. A line's facts hold that estimate
// by the harness's own rules, read in its shipped code ([threadLine.Estimate],
// [threadLine.AttachmentEstimate]): content element by element, a `tool_use` at
// its input's JSON as written, an image 2,000 flat, an attachment at its stored
// rendering, else not read. LENGTH IS JAVASCRIPT'S, UTF-16 code units, and EACH
// ELEMENT IS ROUNDED AS THE HARNESS ROUNDS IT before the sum, AT BOTH RATES
// ([threadEstimate]), the model being the reader's to say. Nested past
// [estimateNestMax] is no figure, never a guess; read only for a reading that
// asks, else [errNotEstimated].
//
// # What Claude Code estimates a session's next request at
//
// A continued session is sent again before the brief, so a launch's floor is
// that request (internal/engine's continuedFloor), and the last request's input
// ([contextReading]) lacks the reply and every record after it. So the floor
// reads the harness's OWN ESTIMATE, run forward ([nextReading]): THE ANCHOR,
// the last counted reply's input and OUTPUT ([countedReplyOf]); every chained
// record after it at its estimate, an attachment re-rendered at two bytes a
// token ([claudeChargeBytes]); a compact boundary restarting it. NO FIGURE, and
// the floor reads the last input, wherever any of that does not read or the
// release is none the count was read in ([claudeCountFloor]). WHAT IT KEEPS
// FROM LINE TO LINE IS BOUNDED ([threadChainMaxRecords]).
//
// # What OpenCode counts a session at before its next request
//
// OpenCode counts from its last counted reply plus a rounded quarter of the
// UTF-16 length of every message after it, restarting at its newest compaction.
// The engine bounds that FROM ABOVE ([newContextMeter],
// [OpenCode.readMainThread]): THE TAIL IS CHARGED AT A QUARTER OF ITS BYTES,
// which a message's render never exceeds, since UTF-8 bytes are never fewer
// than UTF-16 units and escapes only add; a file after the reply marks the
// count ([sandbox.CountPDF]). It restarts at the newest compaction of any
// status ([openCodeRestart]), the side that tells more, and keeps the reply's
// provider, since another provider's model is not held to its figure. ONE WALK
// of the export ([scanExport], [exportMeter]) serves every reader.
//
// # The OpenCode releases the count reads
//
// The count, the point and what a box hands over first are read only for the
// releases in one set ([openCodeCountReleases]). A SET, NEVER A RANGE: any
// release may move a rule the count rests on. TO ADD A RELEASE, read each rule
// above in its bundle, as data; continue sessions in a box of it one token
// under and at the predicted point; and for its saves, export beside a busy
// server and a dead proxy, within its need, keeping the newest compaction's
// identity across an import. THE WRITER AND THE BOX ARE ONE RELEASE: no pair
// has been probed.
//
// # A box's release is pinned
//
// The release a box answers for a session holds only while it cannot change
// under it, and Claude Code updates itself by default, so every Claude Code run
// pins DISABLE_AUTOUPDATER=1 in the wrapper ([claudeSessionLayout]) and its
// settings ([claudeSettingsPin]). A managed settings file still wins: its
// operator's policy.
//
// # A run ends on request
//
// The read that takes a stopped or lost job's session, or one past its turn's
// cap, first asks it to end: SIGUSR1 to the run's script by its recorded pid
// ([Paths.JobScript], [Runner.AskEnd]). The script ends its harness, packs the
// session as its exit does, records how ([Paths.JobEnded]), and exits with the
// harness's status, never read as a success ([sandbox.Result.JobEnd]). See
// end.go.
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
// ([Runner.PackCapture]) packs a capture in before moving it to
// [Paths.SessionPackHeld], named by pack, a name the runner mints for that pack
// alone. Never [Paths.SessionStaging]: a job a box would not end is packed
// while it runs, and its script packs there the moment its harness exits,
// clearing a pack made there. Never another pack's: two reads of one job at
// once (a stop's and a collection's) would each clear the other's files and
// move the other's half-made archive, unchecked ([claudeHeldTar]), as its own.
// Removed whole by the pack when done, and by the clear before every run
// ([packStagingFiles], [Runner.ClearArtifacts]).
func (p Paths) SessionPackStaging(pack string) string {
	return p.WorkDir() + "/session-pack-staging." + pack
}

// SessionPackHeld is where a pack on demand ([Runner.PackCapture]) keeps its
// archive, of a job over or STILL WORKING, moved there whole from its own
// [Paths.SessionPackStaging] and streamed to the one read that packed it,
// which removes it when done.
//
// NEVER [Paths.SessionCapture]: a Claude Code take opens its archive again by
// path for its second pass ([scrubTar]), so another read's pack, or the job's
// own end pack, moving an archive there between the passes would refuse it,
// and a pack of a working job moved over the final archive would be read as
// everything the job did. Named after its staging directory, so the clear
// before every run removes one a read cut short left ([packStagingFiles]).
func (p Paths) SessionPackHeld(pack string) string {
	return p.SessionPackStaging(pack) + ".held"
}

// packStagingFiles is the pattern every pack on demand's files match — its
// staging directory ([Paths.SessionPackStaging]), and the archive it keeps
// beside it for its read ([Paths.SessionPackHeld]) — whichever pack made them:
// what the clear before every run removes; relative to the work directory,
// which the clear quotes.
const packStagingFiles = "session-pack-staging.*"

// SessionSave is where ONE SAVE of the session a run's harness is still working
// in is packed for taker, the holder of the lease it is taken under
// (`<node>:<uuid>`, internal/sandbox's taker lease), named by pack, a name the
// runner mints for that save alone: moved there whole from its own
// [Paths.SessionSaveStaging] by [Runner.PackSave], streamed by the take that
// names it ([sandbox.SessionTake.Save]), and removed once that take is done.
// NEVER [Paths.SessionCapture], the final archive to every read of a job over,
// where a save would pass for a job whose exit pack failed. NEVER ONE PATH A
// TAKER: two saves at once (the drain's and a waiter's, or two processes' as an
// e2b box's waiter duty moves) would move over a take between its two passes
// ([scrubTar]). Named by a digest of the taker ([saveTag]): a node's id is the
// deployment's to choose, and no file name's to trust.
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
