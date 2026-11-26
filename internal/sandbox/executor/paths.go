// Package executor is the detached-run plumbing every harness runner
// shares.
//
// Every harness runs headless in a box in the same shape: a background process
// redirects its output to a file and drops a done marker, Poll checks that
// marker and whether the process is still alive, Collect decodes the stream and
// reads the result, and an ask overlay surfaces a mid-run clarification. This
// package owns that plumbing so each runner supplies only what is its own: its
// invocation, its config, where its output lands, and how its stream and result
// are read — and how a person's notes to its run's turn reach it between its
// own steps (ADR-0177; claudesteer.go's hooks, opencode_steer.js's plugin), and
// how the skills a run is handed as its own reach its harness and its loads
// reach the run's bridge (claudeskills.go's plugin; opencodeskills.go's skills
// directory and opencode_skills.js).
//
// # A run's commits credit Crewlet, through git
//
// A run whose request asks for it ([sandbox.RunRequest.Attribution]) has every
// commit it authors as itself carry a `Co-authored-by` crediting [CoAuthor] and
// the Crewlet trailers naming its seat, its run and its work item, added by a
// git hook the runner installs in the box and the wrapper points git at through
// the run's environment — the same for every harness, and whether or not the
// model remembered anything (attribution.go, attribution_hook.sh).
//
// # A run's output is read as a stream
//
// A run's event stream and its error stream grow with the run, so neither is
// ever read whole: the stream is decoded a line at a time in bounded memory
// and the error stream is read from its end, while the files meant to be
// read whole (the report, the question, the result line) are, and one too
// large degrades only itself; stream.go says why.
//
// # A Claude Code thread is read a line at a time, holding what its readers read
//
// A person's transcript (claudetranscript.go) shows a session as it can,
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
//     missed); a member of more whole lines than its reader's bound
//     ([threadLinesMax]); a line whose facts pass [threadLineMax]; a line
//     holding a value
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
//     An id written inside a record's text (a tool's output, a call's input,
//     a hook's words) never leaves it as bytes: of a call's input, a result's
//     text, a record's own toolUseResult, a message's text and a hook's words
//     (a system message, a success's stdout) the layer hands out only the
//     facts it reads of them ([threadCall], [threadLine.HandsBack],
//     [threadLine.AgentAnswer], [threadMessage.NamesHandback], and the asked
//     take tokens a hook's words name, each whole and exactly:
//     [threadReader.Asking], [threadLine.HookTakes], [claudeTakeToken]),
//     beside the hook's event ([threadLine.HookEvent]). A task notification's
//     `<tool-use-id>`, the text's own claim, leaves it as the leg it names
//     ([threadMessage.Notification]), which its one reader matches only
//     against legs whose call it read.
//
// These rules were measured on Claude Code 2.1.310, 2.1.322 and 2.1.330,
// driven as the engine drives a box's executor, and 2.1.118 beside 2.1.310
// for a hook that follows one call (a skill's). The take's context meter (measure.go)
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
// member it reads what is left of that budget, and of [threadLinesMax].
//
// # A session's calls are read by what they were
//
// OpenCode, Qwen Code and Gemini CLI name no call to the engine, so a reading
// (opencodeholdings.go, qwenholdings.go, geminiholdings.go, whose rules are
// ADR-0073's; the verdict, contentverdict.go, shared) tells a relaunch which of
// its round's calls a cut-off kept session holds by CLASS ([sandbox.CallClass]):
// the tool, the arguments' key ([toolargs.Key], the log's rule), whether it
// failed, the output's key. A class is held only where every call of it in the
// pool has a part of its own after the run's start and the newest compaction,
// handed to the model verbatim, and no call the log cannot state may be of it.
// Which of two identical calls a part is goes undecided: a class is held or
// told whole.
//
// OPENCODE'S: ONE WALK, THREE VISITORS. Its reading visits the one export walk
// ([scanExport]), as the take's meter and a person's transcript are, on the
// scanner, holding no value whole: it asks each member by a means
// ([openCodeHoldingsWalk] lists them), an answer's text and a failure's
// message by their digest ([textDigest]). A VALUE PAST ITS CAPTURE OR ITS WORD
// IS THERE, of its kind, and equal to no word: an `executed` past the bound
// passes its part, and a `type` past the word is a shape not read.
//
// WHAT IS READ, as the measured releases write it, the reading ending at
// anything else: `info` once, an object, its id the session's; `messages` once;
// every message an object of one of the eleven kinds; the run's start
// ([sandbox.RunStart]) found once, the start message itself none of the run's;
// at a compaction of any status ([openCodeRestart]) every part fact dropped; an
// assistant step's names within its set and none twice, no `metadata`, its
// `time` names within `created`, `streamed`, `completed` and `firstToken`
// (2.3.2's), once each, `completed` a number a double reads above zero, as
// JSON.parse does (a settled step, all the export holds), the rest read for
// nothing; each item an object of type text, reasoning or tool, the list whole.
//
// A PART COUNTS only where it is `crewlet_` and a name the bridge files a tool
// by, `executed` absent or false, its names within OpenCode's six and its
// `time`'s within `created`, `ran` and `completed`, each once; its `state`,
// read whatever its key order ([openCodeHoldingsWalk.stateEnd]), exactly
// `status`, `input`, `content`, `metadata` for a completed call, with
// `metadata` exactly `{"truncated": false}` and one text item, or exactly
// `status`, `input`, `error` for a failed one, `error` a `tool.execution` and
// its message; its input an object of at most [sandbox.MaxSubmissionArgsBytes].
// Anything else passes the part, which only lowers its class's parts, and so
// can only tell a call.
//
// GEMINI CLI'S replays its chat file as 0.70.0's loader does
// ([geminiHoldingsWalk]); a part counts by [geminiHoldingsWalk.countReply],
// its text inside 0.70.0's one wrapper.
//
// THE OUTCOME: the text (a failure's message) is matched by its digest
// ([matchedClass]): its size first, never a class's of size zero. A whole
// output is matched every byte; a cut one by its size and all the opening the
// log kept, never where the text holds the scrub's marker, since the scrub may
// have rewritten what the log did not keep. A completed part's empty or blank
// text never counts (an empty answer reaches the model as words of OpenCode's
// own), and a failure counts only where its message is the log's output exactly
// (OpenCode trims it, and an empty or blank one reads as words of its own). A
// part two classes match counts for neither.
//
// THE POOL is every call of the ask but those a person's approval held, asked
// or not, a call held at the save point included: a save of a job's own
// running session holds calls the run made, so leaving one out could let one
// call's part hold another identical call. A call whose class cannot be stated
// (arguments cut, withheld, unread, or "" with no digest; an opening that does
// not read back), one in flight, and one the log dropped are WILDCARDS of their
// tool, apart by digest only where every call of the class carries one; a held
// call is one of its tool, failure and held answer; a count of dropped calls
// with no name is one of every class, or of every failed one where it counts
// failures only ([contentCallPool.wild]). A held class's calls in admission
// order meet its parts in steps never going back, else no reading: a step's
// calls run at once, and the next step's only after they joined
// ([contentInOrder]). The verdict is a pure function of the walk's facts and
// the ask ([contentVerdict]).
//
// WHY IT NEVER HOLDS FALSELY: every counted part lies after the run's start,
// so the run made it, and every call the run made is in the pool, in flight,
// dropped, or a wildcard; no other server joins to `crewlet_`, since the
// run's script refuses one that would ([openCodeShadowCheck]); and each call
// leaves at most one part. So a held class's parts are its own calls' parts,
// content-equal to the log's, but for a refusal in OpenCode's own words equal
// to a logged output, which is the text-equality standard ADR-0073 states.
// A Gemini CLI part lies past the lines the take's cksum checked, is the
// first answer of its id the resume hands, under a count ruling out masking
// and collapse, in a box whose guard ruled out another server
// ([geminiShadowGuard]); a call has one part at most, a failed call none.
// ADR-0073 lists what a box editing either file can make of a reading, as two
// OpenCode messages given one id.
//
// THE BOUNDS: the plaintext at the scrub's meter ([boundedPlain]), no message
// or value held whole (a step's `completed` past the capture is no reading,
// toward telling), the ask at [sandbox.MaxBridgeCalls], its unlisted calls at
// the log's own bounds. What it keeps: whether `info` matched, the start, the
// step, per class at most Calls+1 step numbers and the parts the message it is
// in counts; Gemini CLI's, 2^14 ids and 2^13 run records, calls and answers.
// Each reads in a slot of its own ([HoldingsReadsAtOnce]).
//
// TO ADD A RELEASE to either set, it must first be one the count reads; then
// drive the engine's bridge with it, writer and box alike: empty, blank,
// padded and failed outputs, arguments (numbers, escapes, a key twice, a
// closed schema), parallel and identical calls, truncation, an end and a kill
// mid-call, compaction mid-job, every record's key set, and OpenCode's render,
// both model routes, Code Mode and a child session; and check every part these
// rules count, as the take keeps it, reaches the continuation's first request
// exactly as the log has the call.
//
// # A collection's read of a thread: a base, then one slot
//
// A pass over a session's main thread ([walkThread]) — a collection's read of
// its job's copy ([ClaudeCode]'s readMainThread) and a take, its scrub and
// its meter one pass — holds no token while what it holds stays within its
// BASE, and holds ONE slot of a pool of its own ([CollectionLargeReadsAtOnce],
// [passBudget]) while it holds more. A walk's triggers: a line whose facts
// charge the meter past [collectionBaseLineCharge] ([threadReader.Gate]), a
// reading keeping more than [collectionBaseRecords] records ([recordKeeper],
// the chain, counted by every item it files, its call ids among them), and a
// read's summary words past [summaryBaseBytes] ([summaryClaim]).
//
// THE LINE'S TRIGGER IS ITS FACTS' CHARGE ([lineMeter]), the measure the layer
// bounds a line's hold by, never its length: what no reader reads of a line is
// passed over unheld, so a long line of few facts (a large file's Read, a
// document's base64, a line of blocks of an empty kind) holds the reader's
// buffers alone and reads with no slot. The charge that passes the base waits
// for the slot before it is counted, and an array is charged before it is
// made, so what the facts keep past the base is kept under the slot, never
// before it, but for the strings the charge covers, decoded just before it
// (THE COEFFICIENTS). What the reader holds within a line is uncharged and
// bounded whatever the line, so it comes off the base whole. The take's scrub
// holds no piece whole, and its trigger is a TEXT IT REDACTS AS ONE past its
// own base, [scrubPieceBaseBytes], whose redaction may hold up to its length
// ([pieceGate.pieces], [lineRun]): a transcript's line, a `.json` file (read
// as text too), any other file, and an export's name or string; and its plan
// past the base, the one thing it holds across pieces, held to the take's end
// ([tarPlan.holdPast]), which only an archive of millions of short lines
// reaches.
//
// HELD PER PIECE, KEPT ONLY WHERE A READING KEEPS RECORDS: a pass whose
// readings keep records (the collection's chain, which may keep a line's
// facts; [chatFileLines]'s Gemini CLI ids; a summary's words) keeps the slot
// from its first hold to its end, so one keeping more than
// [collectionBaseRecords] records holds it to its end whatever its lines;
// every other reader, the take's scrub and meter, a collection's count alone,
// holds it for its piece past the base and drops it once done (a walk's line,
// at the next), the slot going back with the pass's last hold: kept to a
// take's end, it would be held minutes while the takes behind waited.
// The scrub and the meter share one budget: with
// two, the scrub's write would wait on the meter and the meter on the slot the
// scrub holds. A wait its reader's context ends is [errLargeWait], a failure
// of the read, never a reading of it. A take's Close ends both readers' waits.
//
// THE COEFFICIENTS are the layer's own worst over its own bounds. The chain
// at both bounds holds 43 MiB at End's peak, 344 bytes a record of 2^17 with
// its call ([threadChain], THE BOUNDS), more than any one item holds alone. A
// line, read in the layer's one pass, holds three things whatever its length.
// The reader, 1.7 MiB at most: its fixed buffers, 146 KiB, and the scanner's
// capture, 64 KiB ("A thread line is read in one scanner pass, never held");
// sixteen scratch buffers each holding a value as written, at most
// [threadValueMax], 80 KiB once append has grown one there (past 256 bytes it
// overshoots a need by a quarter, in whole 8 KiB pages); and the type's
// decoded text, at most three bytes a byte as written (an invalid byte decodes
// to U+FFFD), 240 KiB grown so. The strings one charge covers ([lineMeter]),
// decoded just before it and held while it waits for the slot: three at most
// at once (a head's type, subtype and agentId; a block's type, id and name),
// 192 KiB each so decoded, the last beside the buffer json v2 unquotes it
// through, made by AppendUnquote(nil), grown by append from 64 KiB to 232 KiB
// and live while the string is copied out of it: 808 KiB. And its facts, at
// most 1.25 bytes a byte they charge: nothing is refunded, so the charge counts
// every allocation they keep, and a charged byte holds the most as a string
// just past 32 KiB, which takes whole 8 KiB pages. [readerLineBytes] and
// [chargeStringsBytes] are the two a test measures.
//
// THE BASE: a pass at its base holds about 5.3 MiB at its worst, by no token
// ([baseTierPassBytes]; a pass at both bases past it does not build): the chain
// at 2^13 of its items (its records, retraction prefixes, kept uuids and call
// ids), 2.7 MiB; a line's reader, 1.7 MiB, and the strings a charge covers with
// their decode, 808 KiB, both at their worst; the facts of a line charging the
// base, 128 KiB, holding 160 KiB, over 170 times the 753 bytes the facts of any
// probe line hold; and the count's carry, a uuid and a digest, within the
// reader's figure, rounded up. The record trigger is judged once a line is
// filed, so a walk waits for the slot holding one line's items past 2^13, as
// many as the line's base lets its facts hold (about 4,096 kept uuids, 6,500
// prefixes or 256 calls), filed at tens of bytes each: less than the decode's
// buffer, gone by then. 2^13 items are a thread of at least 4,096 records,
// about 8 MiB at the 2,053 bytes a line probe P6 measured, four times a day's
// session, since a thread of ordinary calls files one id a record at most;
// counted by its records alone, a few records would file the bound's calls with
// no slot. So a typical collection never waits, nor one over the census's
// longest lines (67,871 lines in 9,434 real threads, the longest 2,002,134
// bytes; each of the 28 threads holding a line past 256 KiB had a large file
// read or written into it, which its facts do not keep). MEASURED on a
// dedicated two-core slot (BenchmarkACollectionPassAtItsBase): a main thread
// whose chain files 2^13 items, one a user's line growing every scratch buffer
// to its most beside results under ids of 32,769 bytes whose facts charge the
// base exactly, read by the chain, the count and the next request's estimate
// in one walk, in 0.13 to 0.17 s, holding 4 MiB at most at GOGC=10 and its
// heap peaking at 5 to 8 MiB at the default, within the arithmetic, and
// taking no slot.
//
// THE BASE TIER'S COUNT: no token of this pool holds it, so it is every read in
// flight at once: the node's own, at most its turns (internal/sandbox's
// collections), up to the box host's boxReadsHeld (64) reads for its peers,
// each 5.6 MiB, its pass and its summary's share, 320 KiB
// ([baseTierReadBytes]), their passes 341 MiB, and one per take. A take at its
// base holds at most 11.6 MiB, its scrub's 8.2 (BenchmarkATakeAtItsBase) and
// its meter's 3.3 (BenchmarkAMeterAtItsBase), measured apart, so the sum
// over-counts: the meter's worst line is past the scrub's base, but the scrub
// may read on at its base while the meter is still on it. So every take
// holds one of [SessionTakesAtOnce] (29), a pool of its own sized so the takes
// at their base hold at most those 341 MiB.
//
// THE LARGE TIER, ONE SLOT IN A POOL APART: a collection's pass past its base
// holds 43 MiB at most, the chain at both bounds; a take, its meter beside it
// under the same slot, 32 to 34 MiB over five runs
// (BenchmarkATakeAtItsLargeTier: as at its base, each piece of [threadLineMax]
// bytes, 3.8 to 4.2 s on two cores). Shared with
// [TranscriptReadsAtOnce]'s two, a collection's large pass waits behind two
// transcript reads of up to 58 s each and spends internal/sandbox's
// MainThreadReadWait (27 s), so a person paging a transcript would decide how
// a relaunch is told; apart, never. One slot, not two: the process's thread
// readers hold at most 2 x 32 + 43 + 43 = 150 MiB (the reads peak at 2 x 58)
// beside the base tier, a reading's 43 apart ([HoldingsReadsAtOnce]).
// One waiter behind a pass over ordinary lines at the bound (2.6 to 2.8 s with
// the count and the next request's estimate, 5.7 s loaded) is through in
// 11.4 s, within the 27 s. Behind the slot's slowest holder, loaded, it is not:
// a pass keeping more than [collectionBaseRecords] records holds the slot to
// its end whatever its lines, so a collection whose chain keeps the records of
// 2^20 short lines of blocks of an empty kind, at the bound (18.3 to 19.7 s
// with the count alone, 20.7 to 21.6 s with the next request's estimate,
// BenchmarkTheMainThreadReadAtItsBound, twice that loaded), holds it until its
// own MainThreadReadWait cuts it. One such line to the bound, 18.6 to 18.8 s
// (20.8 to 21.7 s estimating), one record of few facts, holds none. That
// waiter, a later one, or one behind a take's piece (which holds the slot as
// its first reading redacts it, 183 s at the bound) ends with [errLargeWait]
// and reads as no reading (count unread), the safe side, as a read past
// MainThreadReadWait is, the wait spent inside it. The longest piece the
// archive's bound admits, a 46 MiB OpenCode image (a 64.3 MB data URI), held
// it 8.7 s (4.8 from the memo) and 19 MiB in a real take.
//
// A summary is read on its runner's pinned release alone: a box on another
// answers unread, naming it ([summaryReleaseWhy]).
//
// # The memory a node keeps for these pools
//
// [NodeWork.MemoryPools] adds up the pools a node runs, each at its worst by
// the constant beside its measurement, half again, the heap's room under the
// memory limit: where it hosts boxes, the base tier at its turns
// (node.max_concurrent, bounding every read of its runs' boxes) and its peers'
// reads, the takes, the large slot at its larger holder ([largeReadHeldBytes]),
// and the readings' at a chain; and the transcript reads its API or its seats
// make. A Gitlet node keeps them first (gitproc's Options.Reserve), from the
// engine, which alone reads the roles: a registry filled at run time would size
// them by whichever pools had started first.
//
// # A hook's note, confirmed off its job's main thread
//
// A note a Claude Code hook served counts as read only once the job's session
// shows it: the read of the job's main thread ([ClaudeCode]'s readMainThread)
// asks the served takes (internal/sandbox's MainThreadAsk), and one reading in
// the same walk ([hookReading]), beside the resume's model ([chainReading],
// fed first on each line so the reading reads that line's handle), answers
// each take Held, Restarted, Absent, or none of these with why. The release
// and the saved compaction mark the count alone, never the receipts.
//
// A PAIR is a main-thread chained `hook_system_message` at event E whose
// content names the take's token ([threadLine.HookTakes]), then the
// `hook_additional_context` of the same firing at E after it. A firing is keyed
// by the digest of its `toolUseID` and E, never the string, so what the reading
// keeps is fixed in size whatever the thread wrote: per asked take (at most 64,
// [unreadTakes]) the firing's digest, its event as one of two constants, the
// two records' handles, the system message's line, and the firing of the first
// success whose stdout names the take. A context before its system message
// pairs nothing. The success's stdout is the hook's whole answer, so it is
// never a confirmation.
//
// HELD where the pair's two records are handed ([threadChain.Handed]) and read
// alike by every size path of the resume ([threadChain.Alike]: the box's copy
// is read, and the archive's resume may take another path), the job's release
// is one whose resume was read ([claudeResumeRead]), and no restart
// ([threadRestarts.See]) lies after the system message. RESTARTED
// where the same holds but one does, or, for a BATCH pair, a main reply after
// it says its request cleared tool results or says it unreadably
// ([threadLine.ToolResultsCleared]): a batch's note rides its batch's last
// tool result into the request and is cleared with it, where a Stop's note is
// a turn of its own. A pair the resume does not hand (a withdrawn record, a
// parent naming no record) is neither, and is evidence its take arrived.
//
// A THREAD THAT COMPACTED DECIDES ITS TAKES BY ITS LAST BOUNDARY (#1532):
// every size path hands the same records past it (measured on 2.1.310,
// 2.1.322 and 2.1.330: c1 resumed small, by the dispatch and by the prune). A
// pair after it is held as above; one before it RESTARTED where a reply of
// the model followed it with no restart between ([hookReading.answer]), else
// undecided, as only the summary carried it. A later root's thread still binds
// the whole thread to the small path.
//
// No walk is made for takes on a release whose resume was not read, where
// none can be held and so none absent, nor for more than 64, which a hook
// record's masks cannot name ([unreadTakes]): each is undecided.
//
// ABSENT only where the thread proves it: its harness ended its own run, it
// was read to its end with no refused line and no torn last piece, it never
// restarts, another asked take at the same event is held (each event writes
// its own shapes, so a release that rewords one event's pair proves nothing
// at that event, and a held take at the other licenses nothing), no asked
// take is named outside its own pair (a success's stdout of another firing,
// or a system message whose firing never gets its context or names a take of
// another event), every hook record's type, firing and event read, and the
// chain places the thread (implied by the held take: a thread the chain
// cannot place hands no record, so holds no take). A take whose pair was
// found is never absent, and a `hook_cancelled` line names no take, so it
// denies nothing. Anything else is undecided: the safe side, a note told
// again.
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
// [textHandback], [textResult]), and a hook's words into the asked take
// tokens they name ([textTakes], [threadReader.Asking]). A length the estimate
// reads is COUNTED as the scanner takes the value ([jsonscan.Scanner.Units],
// [jsonscan.Scanner.TextUnits]), for a reading that asks
// ([threadReader.Estimating]), at any length. A list is read an
// element at a time by the layer's one rule of an element, up to the first it
// refuses, the rest passed over; an entry read by its first characters (a
// retraction's, a kept uuid) is read one byte past them
// ([lineDecoder.opening]), and a skill's name streams into its UTF-16 length
// and whether its bytes were UTF-8 ([jsonscan.Scanner.InvalidUTF8]).
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
// EIGHT MEBIBYTES, a constant of its own, which bounds what a reader keeps of
// a line and never what a session holds: the take's scrub keeps a line of any
// length ("The scrub holds no value whole"). Every legitimate line's facts are
// under a KiB: its readers keep a block's kind, a call's id, name and target, a
// result's length and digests, a few small values, never a value's text. Of the
// 59,519 lines of the 1,091 JSONL files the probes kept and this package's
// testdata holds, 564 of them threads, the most any line's facts hold is 753
// bytes, and the longest thread line, 2,001,196 bytes, holds 556. A larger
// bound would serve only lines no release writes: what this one refuses is a
// line of tiny calls by the tens of thousands, a value read whole past
// [threadValueMax] (an id, a uuid, a usage), or a type that turns after a
// frame passed over for it, each failing toward no reading or cannot tell.
//
// MEASURED on a dedicated two-core build slot, one case a process, a time the
// spread of three runs; held is the largest live heap under GODEBUG=gctrace=1
// at GOGC=10, a peak the largest heap at the default. A reader's fixed buffers
// (bufio's 64 KiB, the scanner's 67,072 bytes, two text buffers of 4 KiB, the
// automata) hold about 146 KiB, and what no reader reads of a line costs
// nothing more, whatever its length (BenchmarkAReaderAtALineOfAnyLength): a
// whole PDF's Read, 55,924,056 bytes on one line, reads in 32 to 47 ms holding
// 147.1 to 152.2 KiB, 31 to 38 ms with its estimate, and a 64 MiB prompt in 238
// to 262 ms holding 146.4 KiB, 246 to 259 ms with its estimate, 344 to 369 ms
// for one of CJK and emoji, every character counted. A content nested as deep
// as a line is read, through results or documents' sources, is read with its
// estimate in 3 to 8 ms holding 147.5 KiB, its estimate the nesting fault, and
// no case moves the stack in use (352 to 384 KiB in all). Between lines a
// reader keeps two buffers more once a line's values grow them, the scanner's
// capture to [threadValueMax] and its scratch up to [lineScratchKeep] each,
// about 280 KiB in all; within a line that grows every scratch buffer it holds
// up to 1.7 MiB ("A collection's read of a thread", THE COEFFICIENTS). A line
// of the smallest results, which no release writes, holds 16,384 of them within
// the bound, the reader and its facts 2,451 to 2,456 KiB, read in 26 to 48 ms,
// 40 to 52 ms with its estimate, and one more is refused, a line of about 656
// KiB, since a list is charged at every size it doubles through. A reading's
// coincident worst is the resume's model of the thread at both its bounds
// ([threadChain]) alone, 43 MiB held, peaking at 62 MiB: that line as the
// record that fills both, read while End decides, holds 41 MiB at most, its
// heap peaking at 55 MiB, since its results replace calls of longer ids
// (BenchmarkAThreadChainAtItsBounds); the most a reading holds, in its own
// slot ([HoldingsReadsAtOnce]).
// Lines cost a reading time, not memory, about 2.5 µs a line at the least: tiny
// lines to [threadLinesMax] read in 2.5 to 2.8 s, a line of blocks of an empty
// kind to [threadPlainMax] in 17.7 to 18.0 s, and the slowest measured, a
// subagent's thread of 256-byte lines of such blocks to both bounds at once,
// which the reading reads on past every line that leaves it not whole, in 21.4
// to 22.6 s holding 15 MiB (BenchmarkAReadingAtItsBounds).
//
// THE ESTIMATE COSTS ITS PASS what counting costs (jsonscan's package doc),
// and a walk of every element of a result, which a pass that does not ask
// passes over unread. A collection's walk with the count and the next
// request's estimate (BenchmarkTheMainThreadReadAtItsBound, one case a
// process on a dedicated two-core slot, three runs, against the count alone):
// ordinary lines to [threadPlainMax] in 2.6 to 2.8 s against 1.8 to 2.2; a
// tool result of text blocks to it, the slowest shape a session writes, 9.45
// to 9.49 s against 6.7 to 6.8; tiny lines to [threadLinesMax] 2.9 to 3.0 s
// against 2.4 to 2.9; and the shapes no release writes, a result of empty
// strings 17.7 to 18.0 s against 16.4 to 16.7, a line of blocks of an empty
// kind 20.8 to 21.7 s against 18.6 to 18.8, and such lines to both bounds 20.7
// to 21.6 s against 18.3 to 19.7. The same machine read the holdings' slowest,
// which never estimates, in 23.5 to 25.3 s against the 21.4 to 22.6 above, so
// it ran about a tenth slower than when those were taken. The take's meter,
// which estimates, stays under the scrub (BenchmarkTheMeterAtItsBounds,
// likewise): 1.61 to 1.75 s against 10.7 to 10.8 s on ordinary lines, 2.19 to
// 2.22 s against 26.1 to 27.0 s on results of text blocks.
//
// # A run's harness session, carried between boxes
//
// Every run names the harness session it works in before the harness starts
// ([Harness.Session]; a harness minting its id reports it, [sessionMinter]),
// records which it took up ([Paths.SessionRecord]), and packs it once the
// harness exits ([Paths.SessionCapture], [Runner.OpenCapture]); a run handed a
// session packed in another box restores it before its harness takes it up,
// never over a box whose copy holds that archive ([Paths.SessionDigests]), and
// starts its own, saying why, where it can do neither. Whether a box can
// continue a carried session is its harness's release, asked of the box
// ([Runner.CheckCarry]). What each archive holds is its harness's (carry.go,
// each runner's own).
//
// A SESSION ITS HARNESS IS STILL WORKING IN is packed by the engine, beside
// it, by the pack the run's own end makes held to a stricter rule: a file the
// harness rewrote under the pack fails it rather than riding the archive torn
// ([claudeHeldTar]): a job a box would not end, packed on demand for its read
// alone ([Runner.PackCapture], [Paths.SessionPackHeld]), and a SAVE, packed
// where its taker names ([Runner.PackSave], [Paths.SessionSave]) for that take
// alone; neither where the job's final archive is read. Every runner saves
// ([saver], [Runner.Saves]), OpenCode by its own export (below);
// whether a job moved since its last save is how far its harness's event
// stream ran ([Runner.Streamed]); and a save no take will read is removed
// from its box ([Runner.ReleaseSave]). A job over with no archive is packed on
// demand for its read alone, by the run's own end's rule.
//
// # An OpenCode session saved beside its running server
//
// An OpenCode save is the pack on demand's own export ([openCodePackNow])
// through a private server (`--standalone`), beside the run's own, which only
// the run's script reaches. Every release opens its database in WAL mode with
// a five-second busy timeout. On 2.3.2 a standalone probe and export complete
// beside a server answering its own small writes, 7 to 8 saves in 30 s with
// none failed and the slowest at 5.8 s on a loaded 2-core slot (measured;
// beside one stopped mid-step or killed, untested). A save holds the
// session to its last settled message: the step in flight is absent, and its
// calls are the job's own, told past any point the save states. Every
// standalone call runs with the models fetch and the updater off
// ([openCodeStandalonePins]); a proxy that never answers held up no 2.3.2
// export (measured).
//
// BOUNDED BY THE EXPORT'S BYTES: one empty or past [heldPlainMax], what a kept
// session's scrub reads, is refused before gzip. The database bounds nothing,
// since it holds every session in the box.
//
// ONE AT A TIME IN A NODE'S OWN MEMORY, AND WAITING FOR ROOM. An export peaks
// at up to about ten times its session. Measured as the process's own VmHWM
// beside a live server, less 90 MiB, over the database's bytes and its WAL's
// (2.3.2):
//
//	ASCII only: 3.4 to 4.7; 64 MiB peaks at 331 to 391 MiB,
//	  200 MiB at 881 MiB
//	mixed non-ASCII text in every text part: 6.5 to 6.6; 75 MiB at
//	  579 MiB
//	a character past Latin-1 in every string, or a dash past ASCII in every
//	  text part: 8.4 to 9.8, and between 16 and 64 MiB a
//	  slope of 7.6 to 8.6 with intercepts of 105 to 125 MiB; 66 MiB at 645 to
//	  693 MiB
//
// The export takes 0.8 to 14.8 s at 16 to 64 MiB (most for 70,000 small
// messages), and 3.8 s at 200 MiB of ASCII. So a save in a box that shares the node's memory takes the node's
// one pack slot (internal/sandbox's Coordinator), and every save first reads
// its box's room ([openCodeSaveRoom]): its memory domain's limit less what its
// kernel cannot reclaim, capped by MemAvailable. A save whose need, twelve
// times the database and its WAL plus 128 MiB ([openCodeSaveNeedFactor]),
// exceeds a room read waits ([sandbox.ErrSaveNoRoom]); a room not read refuses
// nothing. A take's pack on demand ([OpenCode.Pack]) is never held to the
// room: refused, it would lose the job's newest state on an estimate that
// counts every session in the box.
//
// SAVED ONLY ON A RELEASE MEASURED: saves make the pack on demand, once rare,
// one every running job takes each interval, and releases differ in what a
// standalone call writes (2.3.2 changes the database file) and in what an
// export holds. So a record naming a release
// outside [openCodeCountReleases] is refused before its pack
// ([OpenCode.PackSave]), and a release added there is measured for its saves
// too (below).
//
// # A kept session leaves its box scrubbed
//
// A session is a harness's whole transcript, every file it read and every
// command's output, and what leaves a box passes the engine's secret
// redaction, as every other text read out of one does (code-sandbox.md). The
// take's scrub (scrub.go) makes two passes, in this order:
//
//  1. BY VALUE: every secret the engine itself put in the box, its login and
//     its environment's credentials, is taken out wherever it appears, in its
//     raw form and in the form a JSON string holds it in, so a value redact's
//     rules would never recognise (an operator's opaque token) is gone too.
//     First, so the second pass sees the marker where the value was rather
//     than a half it could not place.
//  2. BY SHAPE: redact.Secrets, the one denylist every surface uses, which
//     catches what the engine did not put there: a key the run printed.
//
// EACH FILE AS WHAT IT IS, so what is kept is still what its harness restores:
//
//   - A TRANSCRIPT, one JSON value a line (`.jsonl`): each value rewritten
//     string by string, every string and every key of every object scrubbed
//     and redacted as the text it decodes to (where a key's lines are lines
//     again, and a secret a tool's structured result names a member by goes
//     too), and a string or a number under a credential's own key taken out
//     whole. A line that is not one JSON value (the last of a job packed while
//     it wrote, or a line of blanks) is scrubbed and redacted as text, the
//     values in the forms a JSON string escapes them in among those it takes
//     out. A RECORD KEEPS ITS LINE AND ITS KEYS, every key in the order
//     written, since Claude Code reads a session file past 5 MiB by where
//     each line's keys sit; what a record can lose is a loaded tool
//     definition the scrub renamed (below). Every key and string is written
//     again ([jsonStringWriter]) as the text it decodes to, changed only
//     where it held a secret, so an escape can come out spelled otherwise
//     than its harness wrote it, and a lone surrogate's escape or a byte that
//     is no UTF-8 comes out as U+FFFD, which each decodes to.
//   - A DOCUMENT (`.json`, and OpenCode's whole export): rewritten the same
//     way a token at a time as it streams, and one of blanks alone written as
//     nothing ([rewriteDocument]), since blanks hold nothing a harness reads.
//     A `.json` file the rewrite cannot read as one JSON value (a task file
//     its harness was killed writing) is redacted as text, as a transcript's
//     torn line is, rather than costing the session; an export read on
//     internal/jsonscan holds none of its strings.
//   - ANYTHING ELSE, as text, scrubbed and redacted as it streams, so a rule
//     that reads across lines sees them all.
//
// AND THE TAR'S OWN WORDS: a member's name is scrubbed by value and by shape
// as the transcript's paths to it are ([memberName]), its header carries
// nothing but its kind, name, mode, size and time (no owner, link target or
// extended record of the box's), and a member that is neither a file nor a
// directory (a link the box's own code made) is left out, since no harness
// keeps its session in one.
//
// A CREDENTIAL'S KEY ([credentialKey]) is read on the key's own words,
// lowercase: a password's, as redact names them (redact.NamesPassword);
// `token`, `key` and `auth` only as the whole key; or a name a credential
// carries anywhere in it (an API key or token, an access, refresh, session,
// security, auth, bearer, identity or registry token, a private key or token,
// an authorization, a secret, a credential, an OpenID Connect ID token). The
// names are read WITH THE KEY'S SEPARATORS DROPPED, since each format joins a
// name's words its own way: `aws_session_token`, `SessionToken` and
// `sessionToken` are one key, and AWS's `X-Amz-Security-Token` header is read
// by its security token; spelled out per separator, the names miss every
// header's. Never `token`, `key` or `auth` at large: a transcript counts its
// tokens and names its sessions in keys of its own (`input_tokens`,
// `sessionId`), an access key's id is no secret (`AccessKeyId`), and a
// commit's author and a server's OAuth settings are no credential (`author`,
// `oauth_provider`); an ID token's `id` is read only as a word of its own,
// since `paid_tokens` spells it. The rule has ONE implementation,
// [keyReading], handed a key's decoded text a piece at a time, cut anywhere,
// its state never growing with the key; [credentialKey] is a keyReading handed
// the whole key.
//
// # The scrub holds no value whole
//
// A harness keeps no bound on one value: Claude Code writes a whole PDF it
// read on one line of its transcript, its base64 twice, 55,924,056 bytes at
// its 20 MiB file limit, and OpenCode keeps a message part of 27 MiB. So the
// scrub holds NO LINE, STRING OR STRETCH OF TEXT WHOLE, and refuses none for
// its length: JSON is read on internal/jsonscan, which hands a name's or a
// string's decoded text over a piece at a time ([jsonRewrite]); every text is
// taken out by value through a [valueStream], and redacted through
// redact.Stream, which writes exactly redact.Secrets of the whole text within
// its stages' windows ([textScrub]); and a transcript's line is never held to
// learn what it is ([measureLines]).
//
// A [valueStream] writes what strings.Replacer writes of the whole text
// ([scrubber.apply]), however it is cut: a value taken out leftmost, and
// longest first at one place, as [newScrubber] orders the replacer's pairs.
// It HOLDS FEWER BYTES THAN THE LONGEST VALUE between pieces: a place is
// decided once that many bytes past it are read, or the text has ended, and a
// value taken out lies within them.
//
// TWO READINGS, THE SECOND A COPY. What a line, or a `.json` file, is, JSON or
// text, is known only at its end, and what a transcript's line loses of its
// loaded tool definitions only once its last key is read; and a tar names each
// file's size before its bytes. So the first reading rewrites the archive into
// the take's SPOOL ([unitSpool]: gzip at the kept archive's level, in its
// process's spool directory), counting each file's size as kept, and the
// second writes the tar from it, never the box ([spoolCopy]): a header names
// the size of what follows it, whatever the box's archive does meanwhile. A
// reading resumes in a member checked by name, kind and length, those before
// kept ([tarPlan.memberAt]). The first reads each line and `.json` file as JSON
// alone; the first that is no JSON ends it, and the next goes on from it,
// reading it as text and every later one both ways ([tarPlan.unitAhead]), each
// way held to [wayRoomBytes] until the unit's is known ([candidate]): a JSON
// way past it goes into a gzip member of its own, cut back out of the spool
// where the text is kept, and a unit whose text was let go before its JSON
// failed is read once more, as text, all within the first reading. The spool
// is removed on every way out, before the take's body ends, and held to the
// room the take holds for it (internal/sandbox's takeRoomBytes): a spool that
// would pass it refuses the take.
//
// THE PLAN ([tarPlan]) is two streams of bits, each written by the first
// reading in the order it decides them and read back by the second in that
// order: for each member its kind, and for a regular file its size as kept;
// and each line that loses a definition: where the spool holds it, which list,
// how many elements it holds, the bytes the cut takes and a bit each, set for
// one cut; and each payload kept apart ([mediaPlan]). So it is AT MOST A
// SIXTEENTH OF THE PLAINTEXT: a member's header is 512 bytes against a few
// dozen bits, a list's element, a byte and its comma, one bit, and its cut a
// few dozen more, which the record it is cut out of pays for. A record of its
// own for each member, line or cut would let a box's archive of a mebibyte
// hold the engine host to gibibytes between the two readings.
//
// AN EXPORT IS SPOOLED TOO ([scrubDocument]): one is refused only once read.
//
// A TEXT A TAKE FOUND UNCHANGED IS NOT REDACTED AGAIN. The redaction is the
// scrub's cost, and every turn's take scrubs the whole session: a whole-PDF
// read, its base64 twice, cost 6.1 s a take. So the process remembers what its
// own full scrubs found unchanged ([scrubMemo]): of a text of
// [scrubPieceBaseBytes] or more (a line read as text, a name or string, a
// file), its length and the digest of its chunks' SHA-256 digests, under its
// first chunk's digest and the take's agent, never the text. A first reading
// writes a text whose first chunk the memo knows as it is, and checks the
// whole at its end; one the memo does not know whole reads the archive again,
// its spool emptied, without that chunk's answer ([firstReadings]). Every
// digest is of the by-value pass's output under the take's own values. SOUND
// because the redaction's answer for a text is the text's alone
// (redact.Stream; its rules are the build's): each text has a redaction of its
// own, a key's credential is judged on its raw name, and a credential's value
// never reaches it. In memory alone: a restart, a deploy or a seat's move
// costs one take at most what it costs without. On two cores of
// the build machine, two runs each (BenchmarkASecondTakeOfAPDFSession), that
// read's take came to 6.1 s with no memo, 5.2 s as a process's first and 3.4 s
// as its second.
//
// # The scrub's bounds, and what it holds
//
// The archive is the box's own: its code can write a gzip that expands a
// thousandfold, a value nested a million deep, or a session of nothing but text
// the redaction has to read. So the scrub reads at most [scrubPlainMax] of
// plaintext in one reading ([scrubBounds]) and rewrites no value nested deeper
// than jsonscan.MaxDepth as JSON (a transcript's line or a `.json` file so
// nested is text, and an export is refused). A session past the plaintext, or
// one whose text the redaction refuses ([errRedaction], what the redaction said
// read off its error, and redact.ErrDecidedEarly whether its stream decided a
// window before what it read there was known, at a write or at its close), is
// refused, stamped with why, rather than costing the host its memory;
// and the whole scrub is held to the take's time (internal/sandbox's
// SessionScrubWait). A panic in the scrub is CONTAINED on the take's own
// goroutine (scrubContained, take.go), which reads every byte of the archive, a
// `.json` file's two readings included, and refuses the take with the panic's
// message; one in the meter beside it measures nothing (meterContained,
// measure.go); each is told under panics.ScopeSessionTake.
//
// AT MOST [scrubHeldBytes] IS HELD: two text scrubs at once (a line or a
// `.json` file read both ways), each its redaction's stages (redact.Stream's
// figure) beside its stage's chunk, a [valueStream] and a string's encoder; two
// JSON rewrites, each its scanner, its read buffer and a frame a container
// open; the readers and writers of the archive's gzip and tar, and of the
// spool; a unit's two ways; and the plan, a sixteenth of the plaintext. About
// 132 MiB at the bound and [scrubLongestMax], 16 MiB of it the plan and 12 MiB
// that value, near nothing in a real session. A test holds it at a smaller
// bound, and BenchmarkTheScrubAtThePlaintextBound measures it at the real one,
// both readings from disk.
// On two cores of the build machine:
//
//	one string to the bound:   16.2 s a scrub, held 17.8 MiB (256 MiB from 28 MiB)
//	a million short lines:     22.1 s a scrub, held 1.4 MiB (160 MiB from 19 MiB)
//	tiny definitions:          21.2 to 21.9 s a scrub, held 17.4 MiB (256 MiB from 0.5 MiB)
//	a string of `pwd=x `:      136 s a scrub
//	a file of `pwd=x` lines:   123 s a scrub
//	`pwd=` lines, torn:        183 s a scrub
//
// The last three, a harness's tool results, set SessionScrubWait (its sum and
// why a take past it fails closed are in code-sandbox.md); the torn string,
// read on from its last line, the slowest. A string is redacted once, as JSON
// in its first reading.
//
// # A kept session's media is kept apart
//
// Each large payload a take keeps apart is found after the scrub ([mediaRun])
// and cut into a gzip member alone ([memberSplit]); no JSON text changes. The
// box digests and restores the archive whole, so its gzip must read several
// members, as GNU's and BusyBox's do
// (TestASessionSplicedFromItsMembersIsRestoredAsOne).
//
// # A definition the scrub renamed is cut out of its line
//
// Claude Code keeps the tool definitions a session loaded through tool search
// in its transcript, an attachment of type [loadedToolsRecord], and a session
// resumed from it sends each one again as a tool of every request it makes
// (measured on 2.1.322). A definition whose name the rewrite left outside what
// the provider takes ([providerToolName]), a marker standing where part of the
// name was, has every request of the restored session refused, so it is cut;
// left out, the session loads the tool again by search when it needs it. A
// line that holds no marker loses nothing, since the rewrite renamed nothing
// in it, a definition its writer named outside the pattern included; one that
// holds a marker loses every definition named outside the pattern, whoever
// named it so; a line that is not one JSON object loses nothing. The record is
// read as Claude Code's JSON.parse reads it: its attachment, the attachment's
// type and its list each the last value written under its key, each key by its
// name exactly, and a definition judged by its `name` exactly and the last
// written, a value that is no string naming nothing, so a definition that is
// no object, or names nothing, is cut where its line holds a marker.
//
// CUT OUT OF THE LINE AS THE REWRITE WROTE IT, each definition with the comma
// that joined it to its list, every other byte where it was: never the record
// decoded and written again from a map, which writes its keys sorted, the
// attachment (a subagent's agentId before it) first and its list inside it.
// Claude Code reads a session file past 5 MiB by the bytes of its lines, and
// finds a record by where its keys sit, in two readings (read in 2.1.310,
// 2.1.322 and 2.1.330):
//
//   - 2.1.310, reading such a file as it does by default, takes a record for
//     a subagent's only where `"isSidechain":true` sits in its line's first
//     256 bytes, and walks the thread back from the last record it takes for
//     the main thread's. A subagent's record written sorted, its list before
//     that key, is taken for the main thread's, and the records the subagent
//     wrote after it are left out of the thread a relaunch resumes. The
//     others look past those bytes too.
//   - Under CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP (a switch a managed settings
//     file can set whatever a launch unsets), both prune such a file, taking a
//     line for a record only where it begins `{"parentUuid":` and walking the
//     main thread back from its last record through such lines alone. A
//     record's line that begins otherwise ends that walk, and where the
//     records the walk reached held half the file or less, every record above
//     that line is pruned from the session a relaunch resumes.
//
// DECIDED IN THE FIRST READING, APPLIED IN THE SECOND: whether a line loses a
// definition is known only at its end (the attachment's last `type`, its last
// `entries`, a marker anywhere), and the first reading spools the list before
// it reads that end. Its half ([definitionsReading]) keeps a bit for each
// element of the list being read and of the last one read before it, which a
// later `entries` or `attachment` replaces, so at most a bit for each two bytes
// of the line; the second's ([cutFilter]) copies the line less the elements
// cut and their commas.
//
// # An OpenCode continuation's first request, judged in its box
//
// OpenCode compacts a session before a request where its count of it reaches
// the point its box sets for the run's model: the model's limit, read off the
// box's catalog and documents, less a buffer. Only the box the run is handed
// can say that point, so the launch asks it there ([Runner.ReadBoxPoint],
// opencodepoint.go), in ONE exec under the run's own environment, the
// engine's own document set aside as the run's script sets it aside
// (OPENCODE_CONFIG_CONTENT), in the run's working directory: the box's login
// imported first, as the run's script imports it before its server reads a
// provider's credential; `opencode --version` where the launch has no
// release; `api model.list` and `api config.get` each answered into a file,
// read back apart from the exec's own lines; and a
// listing of every plugin folder a release of the set reads. Each answer is
// held to 4 MiB and removed from the box once read: config.get answers a
// document's references resolved, so nothing it answered is logged or kept
// but the values the point is decided by and the plugins' names.
//
// THE POINT ([openCodePointAt]), read in 2.3.2's code and measured at its
// boundaries: the model's limit (the smaller of its input and context
// limits, else its input limit, else its context limit), less a buffer the
// highest document sets below it (tokens, or a `"N%"` share), else less the
// larger of fifteen hundredths of it and 16,000 tokens once the limit is
// twice that. `compaction.auto: false` skips
// every compaction but a manual one, the provider's refusal included, so the
// session is sent whole whatever its count.
//
// NO POINT, EACH A MARK ([sandbox.PointMark]), wherever the read cannot prove
// what OpenCode will do: a box release outside the set or other than the
// writer's ([openCodeCountRead]: no pair has been measured); a plugin other
// than the engine's own, by a document or a folder, or a configuration
// directory the listing did not cover, since a plugin runs in the server and
// can compact the session or add to it before the first request, which no
// read of a release's hooks rules out; a provider's contextManagement option;
// a model nothing states the limit of (OpenCode's default record holds it to
// 200,000 tokens, which may not be its server's window: stated means a
// document states its limit, or the catalog listed it, which its record's
// release time says unless a document set that time); a zero limit with
// compaction on (OpenCode's point is then infinite, and only the provider's
// refusal compacts it); a native compaction; a model the catalog does not
// list; a launch whose model names no provider; a read that failed.
//
// NOTHING IS HANDED OVER BEFORE THE BRIEF: a continuation keeping its box's
// copy re-imports it first, and an import keeps no record of what OpenCode
// told the session, so the launch judges the count and the brief alone.
//
// # A continued OpenCode session's standing instructions
//
// OpenCode renders a session's instruction sources once and leads its
// requests with them, telling a later launch's change only as a message, for
// a longer file a diff (measured on 2.3.2). So the gate re-imports the box's
// copy before the run's server starts ([openCodeRenew]): `session export
// --standalone`, held to a carried export's checks, the copy deleted only
// then, and `session import --standalone --directory "$PWD"`. No write to its
// database's instruction state: the box has no client for it, and the schema
// is OpenCode's. The resumed request then leads with this launch's text whole,
// the history kept (measured), for about 3 seconds a continuation, three
// standalone starts. Child sessions are deleted with the copy and not
// imported, as on the carried path; nothing reads a finished run's children.
// A failure starts the minted session, saying why.
//
// # What Claude Code estimates a line's content at
//
// Before a request, Claude Code decides whether to compact by its own count of
// the conversation: the last reply whose usage counts, and an ESTIMATE of every
// record after it (2.1.322's Xh and k7o @294311543, 2.1.310's Zg and KFo
// @247646809). These are the facts a reader of that count reads of a line
// ([threadLine.Estimate], [threadLine.AttachmentEstimate]), filled as the line
// is read, for a reading that asks ([threadReader.Estimating]), by the
// harness's own rules:
//
//   - A USER OR ASSISTANT RECORD is its message's content (EX, 2.1.322
//     @292455010; zY in 2.1.310): a string at its length, a list element by
//     element (n7e @292455428; GJn): a string or a `text` block at its text's
//     length; `thinking` at its text, `redacted_thinking` at its data (the
//     signature is not charged); a `tool_use` at its name's length plus its
//     input's JSON, which is the input as the line writes it, JSON.stringify's
//     own on every line the harness writes, since it writes a line with
//     JSON.stringify (on a line written otherwise, blanks, a key written twice,
//     a needless escape and `\/` count longer, an exponent's number shorter:
//     `1e20` is 4 units, JavaScript's 21); a `tool_result` at its
//     content, by these rules again; a `document` whose source is text at its
//     data, at TWO characters a token where its title's last dotted part is
//     `json`, `jsonl` or `jsonc` in any case, and whose source is content at
//     that content (a string, or its list's objects alone); an `image`, or a
//     `document` of any other source, 2,000 flat (`m=2000`); and any other
//     element at its JSON as written. On 2.1.322 the record also charges
//     2,000 for each image its MCP task delivery carries (Ebs), which 2.1.310
//     does not, so a figure read on that release holds 2,000 an image more
//     than its own count did: the floor's safe side.
//   - AN ATTACHMENT RECORD is what its record stores as its rendering, the
//     record's own `rendered` (rTe and qLe @298643213, its shape held by
//     Cje's schema @298645370): every rendered message's content, a non-empty
//     string or a non-empty list of `text` blocks of non-empty text; plus, on
//     a queued command, 2,000 for each image its task delivery carries. A PDF
//     read is 3,000 a page whatever it stores (vbs, `kbs=3000` @292454941).
//     Where no rendering is stored, or the one stored does not hold to that
//     shape, the harness renders the attachment again by its type (VCt
//     @298645939), a template this layer does not hold, so the estimate does
//     not read ([threadLine.AttachmentEstimate]'s false): so too always for
//     a type the harness renders again whatever it stores (`poll_events`,
//     `project_doc_reads`; L_r @298641216). Only an attachment of no type,
//     and one of the five types the harness renders as no message (`Nh`
//     @283080639), reads as nothing where it stores no rendering, as none of
//     the five does.
//
// LENGTH IS JAVASCRIPT'S: a string's length in UTF-16 code units, two for a
// character past the basic plane, never capped ([jsonscan.Scanner.TextUnits];
// the skill listing's [textUnits] stops past a name's bound). And EACH ELEMENT
// IS ROUNDED AS THE HARNESS ROUNDS IT, Math.round(length / n) (y @292454158),
// half up, before the elements are summed, so a list of short texts charges
// less than their summed length would ([estimateUnits]).
//
// AT BOTH RATES ([threadEstimate]): the harness reads four characters a token
// for exactly fourteen older models and three for every other (2.1.322's F_
// @286842850, 2.1.310's ry @240809635), and which model counts is the reader's
// to say, so the facts hold the two sums.
//
// IN THE LINE'S ONE PASS, HOLDING NO TEXT (claudelineestimatescan.go): each
// length is the scanner's count as the value passes ([jsonscan.Scanner.Units]
// as written, [jsonscan.Scanner.TextUnits] decoded), at any length, so a value
// costs its bytes once however deeply its elements nest, and nothing of it is
// kept but its length (and a PDF read's pages, 20 bytes at most). Every key a
// rule reads is read by its decoded name and last occurrence whatever the
// type written before it, since an element's type is known only at its end.
// NESTED TO [estimateNestMax] ELEMENT LISTS, through a result's content or a
// document's source's, the only two the rules recurse through: past it an
// object a rule charges is no figure, never a guess, where the harness would
// charge it; the walk recurses a frame a list, so the bound holds a read's
// stack to kilobytes. And READ ONLY FOR A READING THAT ASKS
// ([threadReader.Estimating], the next request's): counting costs the pass
// (MEASURED in "A thread line is read in one scanner pass"), and a line read
// otherwise answers [errNotEstimated], never an estimate of nothing.
//
// # What Claude Code estimates a session's next request at
//
// A launch that continues a session sends it again before a word of its
// brief, so its floor is that request with the brief (internal/engine's
// continuedFloor). The last request's input ([contextReading]) is not that
// request: it lacks the reply that request was answered with and every record
// after it, and past a compaction no request has followed it is the
// compacted-away conversation's. So the floor reads the harness's OWN ESTIMATE
// of the next request, by the rule Claude Code decides whether to compact by
// (claudecount.go's preamble), run forward over the thread:
//
//   - THE ANCHOR, by the count's own rule ([countedReplyOf]): the last
//     main-thread reply whose usage counts, at its input, cache writes, cache
//     reads and OUTPUT, placed at the first record of its message id;
//   - EVERY CHAINED MAIN-THREAD RECORD AFTER THAT PLACE at the harness's own
//     estimate of it: a user or assistant record's content
//     ([threadLine.Estimate]), an attachment's stored rendering or a PDF
//     read's pages ([threadLine.AttachmentEstimate]), and an attachment the
//     harness renders again by its type at two bytes a token of its line
//     ([claudeChargeBytes]), which bounds any rendering of what the line
//     holds; a system record and every other record nothing, as the harness
//     charges them;
//   - A MAIN COMPACT BOUNDARY restarts it: no anchor, and the estimate of the
//     stretch the compaction kept before the boundary, which the harness
//     relinks after its summary, beside every record after the boundary
//     ([nextReading.preserved]). Where that stretch cannot be found in what
//     the reading kept of the thread, there is no figure. Until a reply
//     anchors after the boundary the figure holds no system prompt and no
//     tool definitions, which only a reply's input counts, so it is short of
//     the request by those: a residual the floor carries.
//
// AT BOTH RATES the harness counts text at ([threadEstimate]), since which
// model the next request goes to is the launch's to pick
// ([sandbox.NextRequest.At]); the anchor's counts are the same at both.
//
// NO FIGURE, where the thread was not read whole (a line the layer refused, a
// walk ended on an error, no chained line), where a reply's usage does not read
// since the last boundary, where a record after the anchor holds a content the
// harness's own estimate throws on, one nesting an object a rule charges past
// [estimateNestMax], a PDF read whose pages do not read, or a line its walk did
// not ask the estimate of ([errNotEstimated]), where the release that wrote the
// thread is none the count's rules were read in ([claudeCountFloor]), and where
// the box's read marked the count before it opened the thread
// ([claudePrecompact]): the floor then reads the last request's input. A
// server's clearing of old tool results is not charged: the figure is what the
// request the server is sent carries once the clearing has dropped them, which
// is what a window has to hold, while the harness counts them back only to
// decide when to compact.
//
// WHAT IT KEEPS FROM LINE TO LINE IS BOUNDED, whatever a line holds: its sums,
// the digest of its run's message id, and of the records since the last
// boundary at most [threadChainMaxRecords], each the 16 bytes of its uuid and
// the running sums through it (32 bytes an entry, 4 MiB at the bound), from
// which the stretch a boundary kept is charged ([nextReading.stretch]). Never a
// string the thread chose the length of.
//
// # What OpenCode counts a session at before its next request
//
// OpenCode decides before every request, the first of a continued run
// included, whether to compact the session first: it counts the conversation
// from its last counted reply (that reply's input, cache reads and writes,
// output and reasoning, as its provider reported them) and an estimate of
// every message after it, the reply's own tool results among them, at a
// rounded quarter of their text's UTF-16 length; and it restarts that count at
// its newest completed compaction (read in the shipped code of every release
// in [openCodeCountReleases], and measured to the token at each one's
// boundaries). The engine bounds that count FROM ABOVE, at both places a
// session's export is read whole: the take's meter, over the archive it keeps
// ([newContextMeter]), and the collection's read of the export the job's
// script left in its box ([OpenCode.readMainThread]).
//
// THE TAIL IS CHARGED AT A QUARTER OF ITS BYTES, rounded up: every message
// after the reply at its whole span as the export holds it, and the reply's
// own tool results at the spans of their answers and failures. OpenCode
// renders a message to its model from the strings the message stores and at
// most a fixed template, whose bytes the message's own keys exceed; a
// string's UTF-8 bytes are never fewer than its UTF-16 units; and JSON's
// escapes only add. So the charge is never under OpenCode's estimate. It is
// over it by the export's keys and indentation, about 95 tokens a message on
// the real exports this package keeps (testdata/opencode), which holds a
// launch to a little more and never less. A file is the exception: OpenCode
// charges a PDF or an image by what it is, not by its bytes, so a file after
// the reply marks the count ([sandbox.CountPDF], [sandbox.CountMedia]).
//
// AND WHERE IT RESTARTS: the id of the newest compaction message, whatever
// its status ([openCodeRestart]), the transcript's own restart rule, so a job
// whose compaction failed reads as restarted (the side that tells more) while
// its count goes on from its last reply, as OpenCode's does.
//
// AND THE PROVIDER OF THE REPLY IT IS ANCHORED ON, beside it: the figure is
// what that provider reported, and a continuation on another provider's
// model is not held to it.
//
// ONE WALK of the export ([scanExport]), on the scanner (internal/jsonscan),
// which never holds a string. It owns the export's grammar, names and frames
// and hands each member to its visitor by the means the visitor asks
// ([exportAsks]): passed, read as a word, captured within
// [exportValueMaxBytes], or walked as a frame; a value past its word or
// capture is there, of its kind. The meter ([exportMeter]) asks what the walk
// it read by before read, its oracle (TestTheMeterReadsAnExportAsItsWalkDid):
// each message's small values captured, its content walked for the spans of
// its tool results and the files it names, the rest passed unheld. The
// floor's last-request figure ([measuredPrompt]) is read in the same walk. The
// take's pipe waits on the meter, so the walk keeps up with its oracle
// (BenchmarkTheMeterAtItsBounds, 255 MiB of plaintext a case): within 10% on
// every case, `{}` messages in 7.8 to 8.2 s against 27.9 to 31.3, holding at
// most 2 MiB live.
//
// # The OpenCode releases the count reads
//
// The count, the point and what a box hands over first are read only for the
// OpenCode releases in one set ([openCodeCountReleases], [openCodeCountRead]),
// each read in its shipped code and measured at the boundaries its rules
// predict. A SET, NEVER A RANGE: a release beside these is none of them, since
// any release may move a rule the count rests on (earlier OpenCode 2 releases
// moved the share of the window held back and how a model's limits combine),
// and nothing here can tell which one did.
//
// TO ADD A RELEASE, re-probe it: read its bundle, as data, at the point
// function, the count, the estimator, the compaction check (whether `auto`
// skips the overflow route too), the catalog record's builder and a
// document's overlay of it (what states a limit, what sets `time.released`),
// the default model record, the anchor's qualification and its provider, the
// compaction statuses and the native compaction's mark, the model-message
// conversion of every message kind, the seven context sources and their
// renders, the instruction discovery, the plugin locations, and the overflow
// phrases; then continue, in a box of that release, exports placed one token
// under and at the predicted point for a limit it defaults, a limit stated,
// an integer buffer and a tool result after the reply, each of which must
// split there; a first request refused in the overflow phrases; and read its
// model.list and config.get answers as the launch reads them. For its saves:
// beside a server importing message-sized sessions into a 200 MiB database,
// standalone probes and exports that all complete, the server's log holding
// no busy or locked line; a standalone call that opens no connection beside a
// proxy that never answers; the export's peak on a session with a character
// past Latin-1 in every string, at 16 and 64 MiB, within the need
// ([openCodeSaveNeedFactor]); and an export holding a compaction, imported,
// its newest compaction's identity (internal/sandbox's ResumedCount.Restart)
// compared before and after, since a release that changes it raises no
// continued save's point. Only then is it added to [openCodeCountReleases].
//
// THE WRITER AND THE BOX ARE ONE RELEASE: a session one release wrote is
// judged in a box of another only once a probe has continued that pair's
// sessions (a text step, a tool step and a completed compaction) at the box's
// point, which none has yet. So the set admits each release alone, and a pair
// of two differing releases is none.
//
// # Copilot's internal client
//
// A refused launch keeps its session, which a relaunch would resume and the
// store keeps. The extension SDK has no delete, so the run's extension calls
// sessions.bulkDelete through a second client on the internal
// `_internalConnection` (copilot_steer.js); a refusal costs 4.4 s on 1.0.114,
// 65 s on 1.0.115. Extensions off refuse only a run whose notes or cap need
// it; one needing it for checkouts alone runs but is not relaunched. A release
// moved re-measures the client, the warningType, the store's tables.
//
// # A box's release is pinned
//
// A box is asked its harness's release before a run continues a session in it
// ([Runner.CheckCarry], [Runner.CheckInPlace]) or its point is read
// ([Runner.ReadBoxPoint]). The answer holds only while it names the release
// the run runs, so:
//   - Every read runs under its launch's switches ([releaseAsk]): the unsets
//     and pins of the harness's [Harness.Output] and [Harness.Pins], never a
//     list kept beside them. Copilot 1.0.114 with auto-update on runs a newer
//     cached package (measured). No pin a request moves (a model, an
//     endpoint) picks a release.
//   - No release changes under a session: Claude Code and Cursor Agent update
//     themselves in the background by default, so every Claude Code run pins
//     DISABLE_AUTOUPDATER=1 in the wrapper ([claudeSessionLayout]) and its
//     settings pin ([claudeSettingsPin]), a managed settings file still
//     winning, and every Cursor run passes --disable-auto-update.
//
// # A run ends on request
//
// Taking a stopped, lost or capped job's session first asks the job to end
// ([Runner.AskEnd]): SIGUSR1 to its script's recorded pid
// ([Paths.JobScript]), which ends its harness, packs the session
// as its exit does, records how ([Paths.JobEnded]) and exits with the
// harness's status, never a success ([sandbox.Result.JobEnd]). See end.go.
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
// and two processes saving one box — a draining engine and its successor on
// the node — never pack into, or read, one file.
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
// a handful — the processes of the node that launched it, one after another —
// so a collision of 128 bits is never met, and the name stays short enough to
// read in a listing of the box.
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

// ProgramStaging is the DIRECTORY the engine writes a program's bytes into
// before the box's own process copies them into place ([writeProgram]),
// removed whole before every run ([Runner.ClearArtifacts]), so a write whose
// copy never ran leaves nothing past the next run's clear.
func (p Paths) ProgramStaging() string { return p.WorkDir() + "/program-staging" }

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
