package executor

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// claudeResumeRead reports whether release is a Claude Code release whose
// resume [threadChain] models: 2.1.310 and 2.1.322, the two whose shipped
// code the model was read from, and no other. A SET, NEVER A RANGE: a release
// before, between or after them may load, relink, pick, walk, recover, tail,
// retract or drop otherwise, and a model read off two releases says nothing of
// a third. A release is as the engine reads one off a harness's `--version`
// (three runs of digits joined by dots, as sandbox.ValidRelease holds one to),
// compared whole.
//
// ITS READERS ask it of the release that resumes: the reading that tells a
// relaunch which of its calls a kept session holds (exact telling), of the
// relaunch box's, and the one that confirms a hook's note reached the model,
// of the job's own and of the relaunch box's. Where it answers false, what
// [threadChain] answers is no answer for that resume.
//
// TO ADD A RELEASE, re-probe it, as 2.1.310 and 2.1.322 were: read the new
// binary's shipped code, as data and never run, against every step of
// [threadChain]'s model, at the offsets its doc gives for each release
// (the load and its admission, the relink, the leaf set and the pick, the
// walk and its fallback, the recovery, the tails, the retraction filter, the
// malformed-attachment drop, a subagent's own load, the large-file readers,
// and every flag a step reads), and restate each step the new release does
// otherwise, with its offset; then run the facility's census over archives
// that release wrote and resumed, every thread answering as the resume
// handed it. Only then is the release added here, with its offsets beside
// each rule of [threadChain]'s doc.
func claudeResumeRead(release string) bool {
	switch release {
	case "2.1.310", "2.1.322":
		return true
	}
	return false
}

// chainHandle is a record's place among the records a threadChain files, in
// the thread's order, counted from 1. Its zero value, noChainRecord, names no
// record: a line the chain files none for, or a reader's handle never set.
type chainHandle int32

const (
	// noChainRecord names no record.
	noChainRecord chainHandle = 0
	// pastChainBound is the handle of a record See met past the chain's
	// bound: Handed answers handedUnknown for it, Why whyBound.
	pastChainBound chainHandle = -1
)

// handedness is what Claude Code's resume makes of one record of a thread.
type handedness string

const (
	// handedYes is a record the resume hands its model under every variant
	// the reading cannot see.
	handedYes handedness = "handed"
	// handedNo is a record it hands under none.
	handedNo handedness = "not_handed"
	// handedUnknown is a record a variant the reading cannot see decides, or
	// one of a thread holding a shape no model holds.
	handedUnknown handedness = "cannot_tell"
)

// Valid reports whether h is one of the three answers.
func (h handedness) Valid() bool {
	switch h {
	case handedYes, handedNo, handedUnknown:
		return true
	}
	return false
}

// chainWhy says why a record is handed, is not, or cannot be told. Its zero
// value is no reason.
type chainWhy string

// Handed: why the resume hands a record under every variant.
const (
	// whyChain is a record on the walk from the leaf.
	whyChain chainWhy = "chain"
	// whyRecoveredReply is another record of a reply on the walk, found by its
	// reply id and recovered beside it.
	whyRecoveredReply chainWhy = "recovered_reply"
	// whyRecoveredResult is a result whose parent is a record of a reply on
	// the walk, recovered beside it.
	whyRecoveredResult chainWhy = "recovered_result"
)

// Not handed: why the resume hands a record under no variant.
const (
	// whyOffConversation is a record no walk, recovery or tail reaches.
	whyOffConversation chainWhy = "off_conversation"
	// whyRetracted is a record the retraction filter drops.
	whyRetracted chainWhy = "retracted"
	// whyNoConversation is every record of a thread the resume finds no
	// conversation in, and so hands nothing of.
	whyNoConversation chainWhy = "no_conversation"
	// whyNoRecord is the answer for a handle that names no record of the
	// thread: noChainRecord, or one past the handles the chain gave out.
	whyNoRecord chainWhy = "no_record"
)

// Cannot tell, one record: a variant the reading cannot see decides it.
const (
	// whyLeafFlag is a record one leaf hands and another does not, where the
	// leaf turns on 2.1.322's tengu_sorted_metcalfe ([threadChain], THE
	// VARIANTS).
	whyLeafFlag chainWhy = "leaf_flag"
	// whyRelease is a record one release's retraction filter drops and the
	// other's does not.
	whyRelease chainWhy = "release"
	// whyTail is a record the resume's tails hang or may hang under the
	// conversation: one the recovery hangs off a reply (a hook's records),
	// which this model finds but does not hand yet, and one that may hang
	// under the leaf, which it does not follow yet; or one only such a
	// record's retraction list names. Gone once a later commit of this change
	// models the tails (`feat(executor): model what a resume hangs off a reply
	// and its leaf`).
	whyTail chainWhy = "tail"
	// whyAttachmentDrop is a handed attachment the resume's drop of malformed
	// attachments may take, which this model does not follow yet; gone once a
	// later commit of this change models that drop (`feat(executor): model the
	// malformed attachments a resume drops`).
	whyAttachmentDrop chainWhy = "attachment_drop"
)

// Cannot tell, the whole thread: a shape no model holds. Every record answers
// handedUnknown with the thread's reason, and the chain hands no order.
const (
	// whyBound is a thread of more records, retraction prefixes or call ids
	// than the chain files ([threadChainMaxRecords], [threadChainMaxCalls]).
	whyBound chainWhy = "bound"
	// whyLine is a line the load reads otherwise than a line at a time: one
	// the layer refuses that may yield the load a value ([threadChain.Unread]).
	whyLine chainWhy = "line"
	// whyLineGap is a line handed out of turn: its reader dropped or repeated
	// a line, so the chain is not reading the file the resume reads.
	whyLineGap chainWhy = "line_gap"
	// whyProgress is a progress record, which the load bridges later records
	// past.
	whyProgress chainWhy = "progress"
	// whyUUID is a record whose uuid is absent, no string, or no lowercase
	// canonical uuid.
	whyUUID chainWhy = "uuid"
	// whyDuplicateUUID is a uuid two records carry.
	whyDuplicateUUID chainWhy = "duplicate_uuid"
	// whySidechain is a sidechain's record in a session's main thread.
	whySidechain chainWhy = "sidechain"
	// whyFileSnapshot is a file's snapshot, which the releases place apart.
	whyFileSnapshot chainWhy = "file_snapshot"
	// whyForkBriefing is a fork's briefing.
	whyForkBriefing chainWhy = "fork_briefing"
	// whyMarkedCheckpoint is a checkpoint a rewind, a withdrawal or a fork
	// marked.
	whyMarkedCheckpoint chainWhy = "marked_checkpoint"
	// whyCheckpointShape is a checkpoint whose leafUuid is neither a string
	// nor null.
	whyCheckpointShape chainWhy = "checkpoint_shape"
	// whyAdmission is a record the load drops or rewrites, a reply whose mark
	// of an API error or model is in no shape, or an attachment the layer
	// cannot say the malformed-attachment drop keeps.
	whyAdmission chainWhy = "admission"
	// whyReplyID is a reply id the layer cannot read as the resume does: no
	// string, or holding U+FFFD ([threadLine.MessageID]); or, where 2.1.322's
	// q1t reads it, one that may be absent or empty.
	whyReplyID chainWhy = "reply_id"
	// whyCallID is a call's id holding U+FFFD, which the layer may read
	// otherwise than the resume ([threadChain], THE LOAD).
	whyCallID chainWhy = "call_id"
	// whyTimestamp is a timestamp present and not the form Claude Code writes
	// of a real time.
	whyTimestamp chainWhy = "timestamp"
	// whyRetractionShape is a retraction list in no shape.
	whyRetractionShape chainWhy = "retraction_shape"
	// whyPreservedShape is a compaction's metadata in no shape.
	whyPreservedShape chainWhy = "preserved_shape"
	// whyReplacementShape is a content-replacement record whose list 2.1.310's
	// load fails at ([threadLine.ReplacementCount]).
	whyReplacementShape chainWhy = "replacement_shape"
	// whySpread is a list the resume passes into one call's arguments whole
	// longer than [threadChainMaxSpread].
	whySpread chainWhy = "spread"
	// whyLargeFile is a thread past 5,242,880 bytes, which the load reads
	// with one of its large-file readers. Every such thread, until a later
	// commit of this change models those readers (`feat(executor): model what
	// Claude Code loads of a thread past 5 MiB`).
	whyLargeFile chainWhy = "large_file"
	// whyResultByCall is a result 2.1.322's and 2.1.310's flag
	// tengu_foamy_spring may hang elsewhere than under its parent.
	whyResultByCall chainWhy = "result_by_call"
	// whyForkContext is a fork's reference in a subagent's thread.
	whyForkContext chainWhy = "fork_context"
	// whyAgent is the thread of a subagent id no member is named by
	// ([validClaudeAgent]).
	whyAgent chainWhy = "agent"
	// whyRelink is a thread holding a compaction's boundary that names what
	// it kept, which the resume relinks or cuts by; gone once a later commit
	// of this change models the relink (`feat(executor): model a compacted
	// resume's relink and its leaf set`).
	whyRelink chainWhy = "relink"
	// whyLeafSet is a thread whose single path finds no turn record, where the
	// resume picks its leaf from a leaf set; gone with whyRelink.
	whyLeafSet chainWhy = "leaf_set"
	// whySubagentMode is every subagent's thread, until a later commit of
	// this change models a subagent's own load (`feat(executor): model the
	// conversation a subagent's own load hands it`).
	whySubagentMode chainWhy = "subagent_mode"
)

// whyUndecided is the answer to a question asked of a chain End has not
// decided: before End, or after an End its context cut short. A reader's
// misuse, answered as cannot tell rather than a panic, since a reader runs
// inside a node's collection, which a panic would end for every seat the node
// holds.
const whyUndecided chainWhy = "undecided"

// chainWhys is every reason, the place each answer stores its reason by.
var chainWhys = [...]chainWhy{"", whyChain, whyRecoveredReply, whyRecoveredResult, whyOffConversation,
	whyRetracted, whyNoConversation, whyNoRecord, whyLeafFlag, whyRelease, whyTail, whyAttachmentDrop, whyBound,
	whyLine, whyLineGap, whyProgress, whyUUID, whyDuplicateUUID, whySidechain, whyFileSnapshot, whyForkBriefing,
	whyMarkedCheckpoint, whyCheckpointShape, whyAdmission, whyReplyID, whyCallID, whyTimestamp, whyRetractionShape,
	whyPreservedShape, whyReplacementShape, whySpread, whyLargeFile, whyResultByCall, whyForkContext, whyAgent,
	whyRelink, whyLeafSet, whySubagentMode, whyUndecided}

// Valid reports whether w is one of the reasons.
func (w chainWhy) Valid() bool { return w != "" && slices.Contains(chainWhys[:], w) }

// chainVariants is a set of the eight variants the reading cannot see
// ([threadChain], THE VARIANTS), one bit each.
type chainVariants uint8

// The variants: a release, 2.1.310 or 2.1.322, and its flags
// tengu_foamy_spring, tengu_warm_mochi and, in 2.1.322 alone,
// tengu_sorted_metcalfe. warm_mochi reads as off where foamy_spring is off.
const (
	variant310Off chainVariants = 1 << iota
	variant310Spring
	variant310SpringMochi
	variant322Off
	variant322Spring
	variant322SpringMochi
	variant322SpringMetcalfe
	variant322SpringMochiMetcalfe

	// allVariants is every variant.
	allVariants = variant310Off | variant310Spring | variant310SpringMochi | variant322Off | variant322Spring |
		variant322SpringMochi | variant322SpringMetcalfe | variant322SpringMochiMetcalfe
	// variants310 is 2.1.310's, whose retraction filter reads a refusal's
	// fallback alone.
	variants310 = variant310Off | variant310Spring | variant310SpringMochi
	// variantsMetcalfe is 2.1.322's with foamy_spring and sorted_metcalfe on,
	// the variants q1t moves the leaf in.
	variantsMetcalfe = variant322SpringMetcalfe | variant322SpringMochiMetcalfe
	// variantDefault is 2.1.322 as no flag server answers it, the variant a
	// definite answer's reason is given for.
	variantDefault = variant322SpringMochi
)

// chainReplyID is a reply a threadChain interned, counted from 1; 0 is none.
type chainReplyID int32

// chainCallID is a call's id a threadChain interned, counted from 1, a key
// comparable across the thread and never the id itself; 0 is none.
type chainCallID int32

// The chain's bounds (THE BOUNDS, [threadChain]).
const (
	// threadChainMaxRecords is the most items the chain files of one thread:
	// its records and the retraction prefixes they carry.
	threadChainMaxRecords = 1 << 17
	// threadChainMaxCalls is the most call-id occurrences the chain files of
	// one thread: each tool_use and tool_result block's id, and each
	// attachment's string toolUseID.
	threadChainMaxCalls = 1 << 17
	// threadChainMaxSpread is the longest list the chain reads the resume as
	// passing into one call's arguments whole (whySpread). JavaScriptCore, the
	// engine Claude Code's runtime runs on, refuses a call more arguments than
	// its bound on them (its maxArguments, 65,536), and so does the spread
	// that would pass them: the load spreads a content-replacement record's
	// list, and the recovery spreads stretches of the walked chain. Half that
	// bound, so a thread the chain models is one no such spread comes near.
	threadChainMaxSpread = 1 << 15
)

// claudeResumeLargeFile is the most bytes of a thread Claude Code's load
// reads with its small path; a thread past it goes to a large-file reader
// (2.1.322's Jne @284268664 in oee @293824518, 2.1.310's Use @238504623 in
// Cne @247199785, each `size > bound`).
const claudeResumeLargeFile = 5 << 20

// claudeFallbackMS is the most milliseconds before a record whose parent the
// walk cannot follow at which the walk takes another record in its place
// (2.1.322's aUo @293589450, 2.1.310's vpo @247132972).
const claudeFallbackMS = 5000

// chainPrefixBytes is how many bytes of a uuid the retraction filter compares
// a record by: its first 24 characters (2.1.322's cE @298529717, 2.1.310's Gb
// @251726667), 20 hex digits and 4 hyphens of a canonical uuid.
const chainPrefixBytes = 10

// threadChain models, for one Claude Code thread read once a line at a time,
// which records the resume hands its model and in what order. ITS TYPE DOC IS
// THE MODEL'S AUTHORITY: every rule below restates the shipped code of the two
// releases it was read from, 2.1.322 and 2.1.310 ([claudeResumeRead]), with
// each step's offset into each release's binary (2.1.322's first, read as
// data and never run), and a rule no measured thread exercises is still the
// binaries' code. The model's readers: the reading that tells a relaunch which
// of its calls a kept session holds (exact telling), and the one that
// confirms a hook's note reached the model.
//
// WHAT "HANDED" MEANS. Main mode (newThreadChain("")) is `claude --resume
// <session>`, by its id (2.1.322's see @293834308 and Yya @293834568 over the
// load Szt @293813830; 2.1.310's m_i @247206356 over GMt @247190769): what the
// loader's caller hands on, the log's messages after the walk, the recovery
// and the tails, then the retraction filter and the drop of malformed
// attachments, which both releases apply back to back to the loader's output
// (2.1.322's tGe @295288285, `w=eGe(w),w=Fce(w)` @295289720, and again in its
// deserializer e_n @295265705; 2.1.310's cMe @248512380 and TJt @248494717).
// Every pass after those is the reader's own. Subagent mode
// (newThreadChain(agent)) is a subagent's own load of its thread (2.1.322's
// Rye @293845538, 2.1.310's FBe @247216758).
//
// THE BYTES ARE THE RESUME'S: what See and Unread are handed must be the file
// the resume reads, every whole line of it in turn. That is the caller's
// precondition; the chain checks only that no line is skipped or repeated
// (whyLineGap). Its environment is the launcher's: CLAUDE_CODE_REMOTE unset,
// so the hosted environment's gap bridge never runs (2.1.322's uWo
// @293747640 behind WA() @284488580, 2.1.310's wpo @247132270 behind vM()
// @238713786).
//
// THREE ANSWERS. A record is handedYes, handedNo or handedUnknown, each with
// its reason ([chainWhy]): handedUnknown for the record alone where a variant
// the reading cannot see decides it, and for every record of a thread holding
// a shape no model holds, the thread's reason ([threadChain.Unknown]).
//
// THE VARIANTS. The relaunch box's release is unseen, 2.1.310 or 2.1.322, and
// so are three flags its servers may set, each read with its default where no
// server answers: tengu_foamy_spring (2.1.322's hu(), 2.1.310's Ef(), on),
// tengu_warm_mochi (2.1.322's ota() @287013832, 2.1.310's Fri() @240966974,
// on, and off wherever foamy_spring is) and, in 2.1.322 alone,
// tengu_sorted_metcalfe (Upe("tengu_sorted_metcalfe") @293811649, off). Eight
// variants in all ([chainVariants]). Within this model they part in two
// places: the leaf, where q1t moves it (2.1.322 with foamy_spring and
// sorted_metcalfe on), and the retraction filter, whose names each release
// reads off other records. A record handed under some variants and not others
// is handedUnknown (whyLeafFlag, whyRelease); each distinct order is handed
// once, with the variants that hand it ([threadChain.EachOrder]).
// foamy_spring's other effect, a result hung under a call it names rather than
// under its parent, is refused wherever it could hang one (whyResultByCall).
//
// ONE PASS. The caller reads the member once with [threadReader]: every whole
// line to See, every *threadLineError to Unread, and at io.EOF, End. See and
// Unread file what End needs; End decides. Nothing is decided before End,
// since a record's parent can be written after it.
//
// THE LINES THAT ARE NO RECORD ([threadChain.Unread]). The load parses a
// thread a chunk at a time (2.1.322's $1t @293791955 through $4e @283654212,
// 2.1.310's $Pt @247175237): where a value fails, it keeps the values before
// the failure and resumes after the next line break. So a line whose JSON
// fails before any whole value, and one whole value that is no object
// (2.1.322's iVe @293635513, 2.1.310's oMt @247031904), yield it nothing, as
// does an object whose head the layer refuses, of a type the load files
// nowhere a resume reads; the chain skips each. Every other line the layer
// refuses (a value cut at the break,
// one followed by more, a lead NUL or byte order mark, a value nested too
// deep, and a refused head of a kind the load reads, a content-replacement
// record's among them) may yield the load a value: whyLine.
//
// THE LOAD (See; 2.1.322's processEntry Mr in Szt, 2.1.310's Oo in GMt).
// It keeps every object whose type is user, assistant, attachment or system
// (2.1.322's $yo @289067089 over @289067034, 2.1.310's GN @247031512) in a
// map keyed by its uuid as written, a record each, in the order first
// written: a handle is a record's place in that order. Before keeping one it
// admits it (2.1.322's DWt @293626797 with NWt @293627800, 2.1.310's sPt
// @247023276 with iPt @247024252): a user or assistant record whose message
// is no object, whose content is neither a string nor a list, one of whose
// blocks is no object of a string type, or an assistant one whose content is
// a string, is dropped or rewritten, and its children re-parented; the chain
// reads every user and assistant message and refuses each such record, and
// every message [threadLine.Message] refuses (whyAdmission). A system record
// is repaired in place (UWt @293630140, cPt @247026592), which touches no key
// the model reads but a compaction's metadata and a refusal's retraction
// list, each refused in any shape the repair would touch. And it makes every
// string of a record well-formed, a lone surrogate U+FFFD (2.1.322's tIe
// @282906774, 2.1.310's hYe @237222449), as the layer's decode reads one; a
// byte that is no UTF-8 may read otherwise, so a call's id holding U+FFFD is
// refused (whyCallID) as a reply's is ([threadLine.MessageID], whyReplyID).
// The load refuses nothing else the model reads, and the chain refuses, the
// whole thread at once:
//
//   - a progress record (2.1.322's L_e @293638221, 2.1.310's YBe
//     @247033685), which re-parents every later record naming it: whyProgress;
//   - a record whose uuid is absent or no lowercase canonical uuid, the one
//     form a uuid's 16 bytes map to one to one, since the map is keyed by the
//     string as written: whyUUID; one a record already carries:
//     whyDuplicateUUID;
//   - in the main thread, a sidechain's record (whySidechain), a fork's
//     briefing (2.1.322's jWo @293848295, 2.1.310's Ypo @247219197:
//     whyForkBriefing), and anywhere, a file's snapshot, which 2.1.322 keeps
//     out of the session's rows (ZI @293589374) and 2.1.310 does not:
//     whyFileSnapshot;
//   - a reply whose mark of an API error or model the layer cannot read, and
//     an attachment whose payload the layer cannot judge by the
//     malformed-attachment drop's rules (whyAdmission); a timestamp in no form
//     Claude Code writes, since the
//     resume compares times as strings in one place and as dates in another,
//     one order only on that form (whyTimestamp);
//   - a retraction list, a compaction's metadata or a checkpoint's leaf in no
//     shape (whyRetractionShape, whyPreservedShape, whyCheckpointShape), and a
//     checkpoint a rewind, a withdrawal or a fork marked (whyMarkedCheckpoint),
//     which starts the resume where it names whatever came after;
//   - a content-replacement record whose list is none, which 2.1.310's load
//     spreads and fails at (2.1.310 @247196072, where 2.1.322 @293820138
//     passes it over: whyReplacementShape), or that lists more than
//     [threadChainMaxSpread] entries, which either may fail to spread
//     (whySpread);
//   - in a subagent's thread, a fork's reference (whyForkContext), whose
//     parent's walk its load prefixes.
//
// For every record kept the chain files its parent as written (`parentUuid`
// absent, null or "" a root, falsy to every walk; a lowercase canonical uuid
// the record it names; any other string none), its timestamp's milliseconds,
// its reply (`message.id` where it is a string that says something), its call
// ids (as keys, never the ids), its kind and, in the main thread, whether the
// malformed-attachment drop keeps it ([threadLine.AttachmentWellFormed]). The
// session's ROWS are its records but a sidechain's, a fork's briefing and, in
// 2.1.322, a file's snapshot: the last row, and the NEWEST, the first row of
// the latest timestamp, compared as a string (`timestamp>co`). A compaction's
// boundary (2.1.322's Kr @291612448) forgets the checkpoint; a CHECKPOINT
// (last-prompt) names the leaf where its leafUuid says something
// (`if(leafUuid)`), the uuid as written, and any last-prompt that carries the
// key at all, null among it, turns off the newest-row rule below
// (`leafUuid!==void 0`).
//
// THE LEAF, the single path (2.1.322's Gr @293820883 and Yya, 2.1.310's kr
// @247196754 and m_i, alike but for q1t):
//
//   - the start is the last row; but where no checkpoint carries leafUuid and
//     the newest row is not the last, it is the record of the latest
//     timestamp among those whose walk by parent reaches the last row (the
//     later in the file on a tie), or the last row where none does;
//   - where the checkpoint names a record that is not the last row, the last
//     row is the start in its place where it descends from that record by
//     parent, or through the batch of the checkpoint's turn (2.1.322's Y1t
//     @293812412 with X1t @293813630, 2.1.310's WPt @247189354 with GPt
//     @247190569: the descent from the checkpoint through the records a reply
//     hangs off itself, UF, to the results of a batch, or to the call an
//     attachment of a batch's hook names, and the walk up from the last row
//     to that batch's reply through another of its results or another of its
//     records); and, in the metcalfe variants alone, where q1t holds (2.1.322
//     @293811681): the checkpoint is a reply that made a call, the last row
//     descends through a later reply that says something or calls, above
//     that reply's own records, no record hangs off the checkpoint, and none
//     off the last row but one that hangs off a reply, UF;
//   - else the checkpoint, where it names a record; else the start;
//   - the leaf is the nearest user or assistant record up from there by
//     parent, where it has a timestamp: cj (2.1.322 @293745290, 2.1.310's p0
//     @247129929) takes the latest Date.parse of the set, and a record of none
//     is no member. Where it has none, the resume finds no conversation
//     (whyNoConversation); where no such record is found, the resume picks
//     from its leaf set, which this commit does not model (whyLeafSet).
//
// THE WALK (2.1.322's A7t @293745408 with vjt @293589456, 2.1.310's w$t
// @247130047 with Epo @247132981): from the leaf to the record its parent
// names, stopping at a root; where the parent names no record, or a record
// already walked, the walk takes the record of the latest time no later than
// the current one's and at most 5,000 ms before it ([claudeFallbackMS]), of
// the same `isSidechain` as written (absent, false and true three groups),
// not walked, the first in the file on a tie, and stops where there is none
// or the current record has no time. The walk is reversed, root first.
//
// THE RECOVERY (2.1.322's fWo @293749272, 2.1.310's Rpo @247134223, the same
// code): for each reply on the walk, in its order, the first time its id is
// met (`message.id` truthy), every other record of that id anywhere in the
// thread and every result (a user record holding a tool_result block) whose
// parent is one of them, neither walked nor recovered before, is recovered;
// its window runs from the reply to past the last record of its id on the
// walk and on over every record X_e reads as part of it (2.1.322 @293748740,
// 2.1.310's Yce @247133691: a reply of its id, a meta user record or one of
// results, any attachment, a system record but a compaction's boundary).
// Where a record of the reply made a call (2.1.322's bVe @293748333, 2.1.310's
// xMt @247133284), the recovery then HANGS records off it (`co`): under each
// record of the reply, then each result under one of them walked or
// recovered by now, then each walked record of the window past the reply's
// first that is UF (2.1.322 @293588888, 2.1.310's l0 @246995738: an
// attachment, a system record but a compaction's boundary, a meta user record
// holding no result), it takes from each child in turn what pye hands
// (2.1.322 @293588633, 2.1.310's cce @246995483): the child and, after each,
// its one child not met yet, every one a UF record of the parent's
// isSidechain as written (absent, false and true apart) and none met, or
// nothing where one is not or has two children not met; each record hung is
// met at once, so it blocks what a later window would hang. A window OPENS
// where it recovered or hung a record (`xn.length===0&&Tn.size===0` skips the
// rest), one of hung records alone among them. Windows that overlap merge, so
// a window of hung records alone moves where a window before it places its
// records; each places its recovered records among the walked ones of its
// window by their place in the file, the rest at its end. This commit hands
// no record the recovery hangs: each is handedUnknown (whyTail), as is every
// attachment, system record and meta user record holding no result that the
// walk and the recovery do not hand; an order holds the records it hands for
// certain in the place the resume gives them. A result whose ids are not all
// of calls its parent made, where a reply other than its parent made one of
// them, or its sourceToolAssistantUUID names a record other than its parent,
// is one foamy_spring may hang elsewhere: whyResultByCall, the whole thread.
// And where a window exists and the walk is longer than
// [threadChainMaxSpread], or the records hung under one walked record are,
// whySpread: the recovery spreads stretches of the walk, and each walked
// record's hung records, into one call's arguments.
//
// THE TAILS (2.1.322's lWo @293747057, 2.1.310's kpo @247131650): the records
// hanging under the leaf that are neither user nor assistant ones, which this
// commit does not model either (whyTail).
//
// THE RETRACTION FILTER (2.1.322's eGe @295286851, 2.1.310's qGn
// @248511058), main mode alone, over each variant's list: every record but a
// system one whose uuid's first 24 characters a record of the list names in
// its retractedMessageUuids is dropped (whyRetracted); 2.1.322 reads the names
// off user, assistant and model_refusal_fallback records, 2.1.310 off the
// last alone, so a record only one release drops is whyRelease. An entry of
// fewer than 24 characters retracts nothing. Where a record whose retraction
// list is read is itself one the tails may hang, what its list names is
// whyTail.
//
// THE MALFORMED-ATTACHMENT DROP (2.1.322's Fce @295256716 with SCr
// @295255626, 2.1.310's dne with cJo @248486136), main mode alone, after the
// filter: an attachment its rules take is handedUnknown until a later commit
// of this change models the drop (whyAttachmentDrop).
//
// WHAT THIS COMMIT LEAVES TO LATER ONES, each the whole thread cannot tell
// until its own commit models it: a thread past 5,242,880 bytes, which the
// load reads with one of its large-file readers (whyLargeFile); a compaction's
// boundary that names what it kept, by which the resume relinks the thread or
// drops what lies before its last boundary ([threadPreserved.Named],
// whyRelink); a single path that finds no turn record (whyLeafSet); and every
// subagent's thread (whySubagentMode).
//
// THE BOUNDS: [threadChainMaxRecords] records and retraction prefixes, and
// [threadChainMaxCalls] call-id occurrences, 2^17 each. A thread at the
// plaintext's bound, 256 MiB of lines at the 2,053 bytes a line probe P6
// measured, is 130,754 lines, so 2^17 files a record a line of it; a call is
// at least two records, each one block, so its blocks are at most about its
// lines too. Two bounds rather than one sum, since a thread of ordinary calls
// fills each about halfway. Past either, See files nothing more, the thread
// is whyBound and the chain drops what it filed, and See still decodes and
// hands back each user and assistant message, with pastChainBound.
//
// MISUSE IS ANSWERED, NEVER A PANIC, where a node's collection could reach
// it: a line handed out of turn is whyLineGap; a question asked before End, or
// after an End its context cut short, is handedUnknown (whyUndecided); a
// handle out of range is handedNo (whyNoRecord). Only See or Unread after End,
// and End twice, panic: a reader's own code does each always or never, and its
// tests find it.
//
// One goroutine reads one thread; the chain holds no lock and starts none.
type threadChain struct {
	// agent is the subagent whose thread this is, "" for the main thread.
	agent string
	// state is where the chain is in its pass.
	state chainState
	// next is the line number See or Unread must be handed next, from 1.
	next int
	// bytes is how many bytes of the member the lines handed so far take,
	// their breaks included.
	bytes int64
	// why is the thread's own cannot tell, the first shape met; "" while the
	// thread is modelled.
	why chainWhy
	// past is whether a bound was passed: the chain files nothing more.
	past bool
	// handles is how many handles See gave out, past the bound none.
	handles int

	// recs is every record filed, by handle - 1.
	recs []chainRecord
	// index is each record's uuid's handle, and written each record's parent
	// as written, until End resolves them.
	index   map[[16]byte]chainHandle
	written []chainWritten
	// replies interns reply ids by their digests.
	replies map[[32]byte]chainReplyID
	// callIDs interns call ids by their digests; carried says, by id - 1,
	// whether a block carries the id, rather than an attachment alone.
	callIDs map[[32]byte]chainCallID
	carried []bool
	// occurs is every call-id occurrence filed, each record's from its calls.
	occurs []chainCallID
	// prefixes is every retraction prefix filed, each record's from its
	// prefix.
	prefixes [][chainPrefixBytes]byte
	// filed is how many items count against threadChainMaxRecords.
	filed int
	// sources is each result's sourceToolAssistantUUID, where canonical.
	sources map[chainHandle][16]byte
	// checkpoint is the leaf the last checkpoint since the last boundary
	// names, and anyLeafKey whether any checkpoint carries leafUuid.
	checkpoint chainCheckpoint
	anyLeafKey bool
	// lastRow and newest are the session's last and newest rows; newestMS is
	// the newest's time.
	lastRow, newest chainHandle
	newestMS        int64
	// relinks is whether a compaction's boundary names what it kept.
	relinks bool
	// scratch collects one record's retraction prefixes.
	scratch [][chainPrefixBytes]byte

	// What End decided: each record's verdict, by handle - 1, the distinct
	// orders, and the leaf every variant walks from.
	verdicts []chainVerdict
	orders   []chainOrder
	leaf     chainHandle
}

// chainState is where a threadChain is in its one pass.
type chainState uint8

const (
	chainFiling chainState = iota
	chainDecided
	chainAbandoned
)

// chainKind is what the chain keeps of a record's kind, a bit a fact.
type chainKind uint32

const (
	kindUser chainKind = 1 << iota
	kindAssistant
	kindAttachment
	kindSystem
	// kindSideFalse and kindSideTrue are `isSidechain` false and true;
	// neither is absent.
	kindSideFalse
	kindSideTrue
	// kindMeta is a user record's `isMeta` the JSON true.
	kindMeta
	// kindResult is a user record holding a tool_result block, kindUse an
	// assistant record holding a tool_use block.
	kindResult
	kindUse
	// kindSynthetic is a reply its harness wrote: marked an API error's, or
	// of the model "<synthetic>".
	kindSynthetic
	// kindSays is a message whose text says something (SUo).
	kindSays
	// kindBoundary is a compaction's boundary, kindFallback a refusal's
	// fallback.
	kindBoundary
	kindFallback
	// kindTimed is a record with a timestamp.
	kindTimed
	// kindIllFormed is an attachment the malformed-attachment drop takes.
	kindIllFormed
	// kindNowhere is a record whose parent names no record.
	kindNowhere
)

// chainRecord is what the chain files of one record.
type chainRecord struct {
	// uuid is the record's own uuid's 16 bytes.
	uuid [16]byte
	// ms is its timestamp's milliseconds, where kindTimed.
	ms int64
	// parent is the record its parent names, as End resolves it:
	// noChainRecord for a root and for a parent naming no record (kindNowhere).
	parent chainHandle
	reply  chainReplyID
	// calls and prefix are where its call-id occurrences and its retraction
	// prefixes start.
	calls, prefix int32
	// line is its line's number.
	line int32
	kind chainKind
}

// chainWritten is a record's parent as written, until End resolves it.
type chainWritten struct {
	uuid  [16]byte
	named bool
}

// chainCheckpoint is the leaf a checkpoint names: set where one names a leaf
// since the last boundary, named where that is a canonical uuid.
type chainCheckpoint struct {
	uuid       [16]byte
	set, named bool
}

// chainVerdict is a record's answer, as End stores it: its handedness and
// its reason's place in chainWhys.
type chainVerdict struct {
	handed uint8
	why    uint8
}

// The handedness codes a chainVerdict stores.
const (
	verdictYes uint8 = iota + 1
	verdictNo
	verdictUnknown
)

// newThreadChain is the model of a thread not read yet: agent "" the
// session's main thread as `claude --resume <session>` loads it; else the
// thread of subagent agent as its own load builds it. An agent outside
// [validClaudeAgent]'s rule is cannot tell for every record (whyAgent).
func newThreadChain(agent string) *threadChain {
	c := &threadChain{agent: agent, next: 1, index: map[[16]byte]chainHandle{},
		replies: map[[32]byte]chainReplyID{}, callIDs: map[[32]byte]chainCallID{},
		sources: map[chainHandle][16]byte{}}
	if agent != "" && !validClaudeAgent(agent) {
		c.cannotTell(whyAgent)
	}
	return c
}

// cannotTell makes w the thread's cannot tell, where it has none yet.
func (c *threadChain) cannotTell(w chainWhy) {
	if c.why == "" {
		c.why = w
	}
}

// filing panics where End has run: a reader's own misuse, which its tests
// find, since its code does it always or never.
func (c *threadChain) filing(what string) {
	if c.state != chainFiling {
		panic("executor: threadChain." + what + " after End")
	}
}

// inTurn holds n, a line's number, to the next one the chain must be handed:
// a line skipped or repeated is the thread's cannot tell (whyLineGap), never
// a panic, since a reader runs inside a node's collection.
func (c *threadChain) inTurn(n int) {
	if n != c.next {
		c.cannotTell(whyLineGap)
	}
	c.next = n + 1
}

// See files line, the thread's next whole line, and answers its handle
// (noChainRecord for a line the load keeps no record of) and, for a user or
// assistant record, its message, decoded once here so its caller decodes it
// no second time; mErr is that decode's error, which the chain has already
// taken for the thread's cannot tell (whyAdmission). It never fails the
// reading. PAST THE BOUND it files nothing more, the thread is cannot tell
// (whyBound), and it still decodes and hands back every user and assistant
// message, with pastChainBound as the handle. line.N must be the thread's
// next line number: a line out of turn is the thread's cannot tell
// (whyLineGap). See panics after End.
func (c *threadChain) See(line threadLine) (chainHandle, threadMessage, error) {
	c.filing("See")
	c.inTurn(line.N)
	c.bytes += int64(len(line.Raw)) + 1
	var (
		m    threadMessage
		mErr error
	)
	switch line.Head.Type {
	case recordUser, recordAssistant:
		m, mErr = line.Message()
	case recordAttachment, recordSystem:
	case recordProgress:
		c.cannotTell(whyProgress)
		return noChainRecord, m, mErr
	case recordLastPrompt:
		if !c.past {
			c.checkpointOf(line)
		}
		return noChainRecord, m, mErr
	case recordForkContextRef:
		if c.agent != "" {
			c.cannotTell(whyForkContext)
		}
		return noChainRecord, m, mErr
	case recordContentReplacement:
		c.replacementsOf(line)
		return noChainRecord, m, mErr
	default:
		return noChainRecord, m, mErr
	}
	if c.past {
		return pastChainBound, m, mErr
	}
	return c.file(line, m, mErr), m, mErr
}

// Unread files a whole line [threadReader.Next] answered as no record: its N
// is held as See holds line.N, and its Size counted with the lines' bytes. A
// line that yields the load no value is skipped; any other is whyLine
// ([threadChain], THE LINES THAT ARE NO RECORD). Unread panics after End.
func (c *threadChain) Unread(fault *threadLineError) {
	c.filing("Unread")
	if fault == nil {
		c.cannotTell(whyLine)
		return
	}
	c.inTurn(fault.N)
	c.bytes += int64(fault.Size)
	switch {
	case fault.Fault == faultSyntax, fault.Fault == faultNotObject:
		return
	case fault.Fault == faultHead && fault.Type == "":
		return
	}
	c.cannotTell(whyLine)
}

// checkpointOf files what a last-prompt record says of the leaf (the
// last-prompt branch of 2.1.322's Mr and 2.1.310's Oo).
func (c *threadChain) checkpointOf(line threadLine) {
	leaf, marked, err := line.Checkpoint()
	keyed, keyErr := line.LeafKeyed()
	switch {
	case err != nil || keyErr != nil:
		c.cannotTell(whyCheckpointShape)
		return
	case marked:
		c.cannotTell(whyMarkedCheckpoint)
		return
	}
	c.anyLeafKey = c.anyLeafKey || keyed
	if leaf != "" {
		u, named := uuidBytes(leaf)
		c.checkpoint = chainCheckpoint{uuid: u, set: true, named: named}
	}
}

// replacementsOf refuses a content-replacement record the load may fail at
// ([threadLine.ReplacementCount]).
func (c *threadChain) replacementsOf(line threadLine) {
	n, err := line.ReplacementCount()
	switch {
	case err != nil:
		c.cannotTell(whyReplacementShape)
	case n > threadChainMaxSpread:
		c.cannotTell(whySpread)
	}
}

// errChainFull ends a walk of a record's retraction list that would pass the
// chain's bound.
var errChainFull = errors.New("executor: a retraction list runs past the chain's bound")

// file files line, a record of the map, read as message m (mErr its error),
// and answers its handle, or pastChainBound where it would pass a bound.
func (c *threadChain) file(line threadLine, m threadMessage, mErr error) chainHandle {
	head := line.Head
	r := chainRecord{line: int32(line.N)}
	switch head.Type {
	case recordUser:
		r.kind |= kindUser
	case recordAssistant:
		r.kind |= kindAssistant
	case recordAttachment:
		r.kind |= kindAttachment
	case recordSystem:
		r.kind |= kindSystem
	}
	id, uuidErr := line.UUID()
	u, canonical := uuidBytes(id)
	switch {
	case uuidErr != nil || !canonical:
		c.cannotTell(whyUUID)
	case c.index[u] != noChainRecord:
		c.cannotTell(whyDuplicateUUID)
	}
	r.uuid = u
	if side := head.Sidechain; side != nil && *side {
		r.kind |= kindSideTrue
	} else if side != nil {
		r.kind |= kindSideFalse
	}
	if c.agent == "" && r.kind&kindSideTrue != 0 {
		c.cannotTell(whySidechain)
	}
	snapshot := head.Type == recordSystem && head.Subtype == subtypeFileSnapshot
	if snapshot {
		c.cannotTell(whyFileSnapshot)
	}
	attachment, typeErr := line.AttachmentType()
	briefing := typeErr == nil && attachment == attachmentForkBriefing
	if briefing {
		c.cannotTell(whyForkBriefing)
	}
	if mErr != nil {
		c.cannotTell(whyAdmission)
	}
	if _, isText := m.Text(); isText && head.Type == recordAssistant {
		c.cannotTell(whyAdmission)
	}
	if head.Type == recordAssistant {
		c.replyOf(line, &r)
	}
	if ms, present, err := line.Timestamp(); err != nil {
		c.cannotTell(whyTimestamp)
	} else if present {
		r.kind |= kindTimed
		r.ms = ms
	}
	if meta, err := line.IsMeta(); err == nil && meta {
		r.kind |= kindMeta
	}
	calls := m.Calls()
	if len(calls) > 0 && head.Type == recordUser {
		r.kind |= kindResult
	}
	if len(calls) > 0 && head.Type == recordAssistant {
		r.kind |= kindUse
	}
	if _, says := m.Says(); says {
		r.kind |= kindSays
	}
	for _, call := range calls {
		if strings.ContainsRune(call.ID, utf8.RuneError) {
			c.cannotTell(whyCallID)
		}
	}
	attached, isString := line.AttachmentToolUseID()
	if head.Type == recordSystem && head.Subtype == subtypeCompactBoundary {
		r.kind |= kindBoundary
		c.checkpoint = chainCheckpoint{}
		p, err := line.Preserved(func(string) error { return nil })
		switch {
		case err != nil:
			c.cannotTell(whyPreservedShape)
		case p.Named:
			c.relinks = true
		}
	}
	if head.Type == recordSystem && head.Subtype == subtypeRefusalFallback {
		r.kind |= kindFallback
	}
	if c.agent == "" && head.Type == recordAttachment {
		switch kept, err := line.AttachmentWellFormed(); {
		case err != nil:
			c.cannotTell(whyAdmission)
		case !kept:
			r.kind |= kindIllFormed
		}
	}
	written := c.parentOf(head, &r)
	occurrences := len(calls)
	if isString {
		occurrences++
	}
	if !c.retractionsOf(line) || len(c.occurs)+occurrences > threadChainMaxCalls {
		return c.passBound()
	}

	h := chainHandle(len(c.recs) + 1)
	c.handles = int(h)
	if canonical && c.index[u] == noChainRecord {
		c.index[u] = h
	}
	r.calls, r.prefix = int32(len(c.occurs)), int32(len(c.prefixes))
	for _, call := range calls {
		c.occurs = append(c.occurs, c.callID(sha256.Sum256([]byte(call.ID)), true))
	}
	if isString {
		c.occurs = append(c.occurs, c.callID(attached, false))
	}
	c.prefixes = append(c.prefixes, c.scratch...)
	c.filed += 1 + len(c.scratch)
	if src, ok := line.SourceToolAssistantUUID(); ok && r.kind&kindResult != 0 {
		c.sources[h] = src
	}
	c.recs = append(c.recs, r)
	c.written = append(c.written, written)
	// A ROW is every record but a sidechain's, a fork's briefing and a file's
	// snapshot (2.1.322's Mr: `!ZI(Ho)`, `!Ho.isSidechain&&!jWo(Ho)`).
	if r.kind&kindSideTrue == 0 && !briefing && !snapshot {
		c.row(h, r)
	}
	return h
}

// row files record h, r, as a row of the session: its last, and its newest
// where its timestamp is later, as a string, than every row's before it.
func (c *threadChain) row(h chainHandle, r chainRecord) {
	c.lastRow = h
	if r.kind&kindTimed != 0 && (c.newest == noChainRecord || r.ms > c.newestMS) {
		c.newest, c.newestMS = h, r.ms
	}
}

// replyOf files a reply's id and whether its harness wrote it.
func (c *threadChain) replyOf(line threadLine, r *chainRecord) {
	id, err := line.MessageID()
	switch {
	case err != nil:
		c.cannotTell(whyReplyID)
	case id != "":
		key := sha256.Sum256([]byte(id))
		reply, ok := c.replies[key]
		if !ok {
			reply = chainReplyID(len(c.replies) + 1)
			c.replies[key] = reply
		}
		r.reply = reply
	}
	failed, failedErr := line.APIError()
	model, modelErr := line.Model()
	switch {
	case failedErr != nil || modelErr != nil:
		c.cannotTell(whyAdmission)
	case failed || model == "<synthetic>":
		r.kind |= kindSynthetic
	}
}

// parentOf is record r's parent as written: a root, a canonical uuid to
// resolve at End, or a parent naming no record, kindNowhere.
func (c *threadChain) parentOf(head threadHead, r *chainRecord) chainWritten {
	if head.Parentless() {
		return chainWritten{}
	}
	parent := head.Parent
	var text string
	if bytes.IndexByte(parent, '\\') < 0 && len(parent) >= 2 {
		text = string(parent[1 : len(parent)-1])
	} else if s, err := wireRequiredString("parentUuid", parent); err == nil {
		text = s
	}
	if u, ok := uuidBytes(text); ok {
		return chainWritten{uuid: u, named: true}
	}
	r.kind |= kindNowhere
	return chainWritten{}
}

// retractionsOf collects into scratch the prefixes line's retraction list
// names that a canonical uuid can begin with, and reports whether the record
// and they fit threadChainMaxRecords; a list in no shape is
// whyRetractionShape.
func (c *threadChain) retractionsOf(line threadLine) bool {
	c.scratch = c.scratch[:0]
	// room is how many prefixes may be filed beside the record itself.
	room := threadChainMaxRecords - c.filed - 1
	if room < 0 {
		return false
	}
	err := line.Retracted(func(entry string) error {
		p, ok := chainPrefix(entry)
		if !ok {
			return nil
		}
		if len(c.scratch) >= room {
			return errChainFull
		}
		c.scratch = append(c.scratch, p)
		return nil
	})
	switch {
	case errors.Is(err, errChainFull):
		return false
	case err != nil:
		c.cannotTell(whyRetractionShape)
		c.scratch = c.scratch[:0]
	}
	return true
}

// chainPrefix is the first 24 characters of entry, a retraction list's, as
// the 10 bytes of a canonical uuid's first 24, and true where they are such:
// 8, 4, 4 and 4 lowercase hex digits, each run followed by a hyphen. An entry
// that begins otherwise retracts nothing, since every record the chain models
// carries a lowercase canonical uuid, and the filter compares their first 24
// characters as strings.
func chainPrefix(entry string) ([chainPrefixBytes]byte, bool) {
	var p [chainPrefixBytes]byte
	if len(entry) < 24 {
		return p, false
	}
	n := 0
	for i := 0; i < 24; {
		switch i {
		case 8, 13, 18, 23:
			if entry[i] != '-' {
				return [chainPrefixBytes]byte{}, false
			}
			i++
			continue
		}
		hi, lo := lowercaseHexDigit(entry[i]), lowercaseHexDigit(entry[i+1])
		if hi < 0 || lo < 0 {
			return [chainPrefixBytes]byte{}, false
		}
		p[n] = byte(hi<<4 | lo)
		n, i = n+1, i+2
	}
	return p, true
}

// callID is the call id key interns, a block's where carried.
func (c *threadChain) callID(key [32]byte, carried bool) chainCallID {
	id, ok := c.callIDs[key]
	if !ok {
		id = chainCallID(len(c.callIDs) + 1)
		c.callIDs[key] = id
		c.carried = append(c.carried, false)
	}
	c.carried[id-1] = c.carried[id-1] || carried
	return id
}

// passBound makes the thread whyBound, drops what the chain filed, and files
// nothing more.
func (c *threadChain) passBound() chainHandle {
	c.cannotTell(whyBound)
	c.past = true
	c.recs, c.index, c.written, c.replies, c.callIDs, c.carried = nil, nil, nil, nil, nil, nil
	c.occurs, c.prefixes, c.sources, c.scratch = nil, nil, nil, nil
	return pastChainBound
}

// End decides, once the reader has answered io.EOF, what the resume hands. It
// checks ctx between its steps and inside its walks; ctx's error means no
// model, the error returned, and every question then answers handedUnknown
// (whyUndecided). End panics where it already ran.
func (c *threadChain) End(ctx context.Context) error {
	if c.state != chainFiling {
		panic("executor: threadChain.End twice")
	}
	c.state = chainAbandoned
	err := c.decide(ctx)
	c.index, c.written, c.sources, c.scratch, c.replies = nil, nil, nil, nil, nil
	if err != nil {
		c.verdicts, c.orders, c.leaf = nil, nil, noChainRecord
		return err
	}
	c.state = chainDecided
	return nil
}

// Unknown is the thread's own cannot tell: the first shape no model holds
// that it met, and true; "" and false where the thread is modelled. Asked
// while the chain files, it answers the first shape met so far, so a reader
// can stop reading a thread the model already cannot place; after an End
// its context cut short, whyUndecided.
func (c *threadChain) Unknown() (chainWhy, bool) {
	switch {
	case c.why != "":
		return c.why, true
	case c.state == chainAbandoned:
		return whyUndecided, true
	}
	return "", false
}

// Handed is what the resume makes of record h; Why says why. noChainRecord,
// and a handle the chain never gave out, answer handedNo (whyNoRecord);
// pastChainBound answers handedUnknown (whyBound); every record of a thread
// the model cannot place answers handedUnknown, with the thread's reason; and
// asked before End decided, every record answers handedUnknown
// (whyUndecided).
func (c *threadChain) Handed(h chainHandle) handedness {
	handed, _ := c.verdict(h)
	return handed
}

// Why is why record h is handed, is not, or cannot be told ([threadChain.Handed]).
func (c *threadChain) Why(h chainHandle) chainWhy {
	_, why := c.verdict(h)
	return why
}

// verdict is Handed's and Why's answer for h.
func (c *threadChain) verdict(h chainHandle) (handedness, chainWhy) {
	switch {
	case h == noChainRecord:
		return handedNo, whyNoRecord
	case c.state != chainDecided:
		return handedUnknown, whyUndecided
	case h == pastChainBound:
		return handedUnknown, whyBound
	case h < 0 || int(h) > c.handles:
		return handedNo, whyNoRecord
	case c.why != "":
		return handedUnknown, c.why
	}
	v := c.verdicts[h-1]
	switch v.handed {
	case verdictYes:
		return handedYes, chainWhys[v.why]
	case verdictNo:
		return handedNo, chainWhys[v.why]
	}
	return handedUnknown, chainWhys[v.why]
}

// EachOrder hands each distinct order in which the variants hand the thread's
// records, first to last, until each returns false: the order the default
// variant hands first. Each holds the records the variants that hand it hand
// for certain, a record any of them may or may not hand left out of it. None
// where the thread is cannot tell, or End has not decided.
func (c *threadChain) EachOrder(each func(o chainOrder) bool) {
	if c.state != chainDecided || c.why != "" {
		return
	}
	for _, o := range c.orders {
		if !each(o) {
			return
		}
	}
}

// chainOrder is one handed order: At(i) the i-th record handed, Pos(h)
// record h's place in it (false where this order hands no h), Variants the
// variants that hand it.
type chainOrder struct {
	records  []chainHandle
	at       []int32 // by handle - 1: the record's place, -1 where not in this order
	variants chainVariants
}

// Len is how many records the order hands.
func (o chainOrder) Len() int { return len(o.records) }

// At is the i-th record the order hands, from 0; noChainRecord out of range.
func (o chainOrder) At(i int) chainHandle {
	if i < 0 || i >= len(o.records) {
		return noChainRecord
	}
	return o.records[i]
}

// Pos is record h's place in the order, from 0, and true; false where the
// order does not hand h.
func (o chainOrder) Pos(h chainHandle) (int, bool) {
	if h <= 0 || int(h) > len(o.at) || o.at[h-1] < 0 {
		return 0, false
	}
	return int(o.at[h-1]), true
}

// Variants is the set of variants that hand the order.
func (o chainOrder) Variants() chainVariants { return o.variants }

// Reply is the reply record h belongs to (all records of one message.id
// share it), where h is an assistant record whose id says one; ids count from
// 1.
func (c *threadChain) Reply(h chainHandle) (chainReplyID, bool) {
	r, ok := c.record(h)
	if !ok || r.reply == 0 {
		return 0, false
	}
	return r.reply, true
}

// Uses and Results are the call ids record h's tool_use and tool_result
// blocks carry, as keys comparable across the thread (never the ids
// themselves): Uses an assistant record's, Results a user record's, nil for
// every other. The chain's own: a reader reads them and never writes them.
// An attachment's toolUseID is no block's: it is interned beside them for
// the walk up from the last row alone, and CallKey answers false for an id
// only an attachment carries.
func (c *threadChain) Uses(h chainHandle) []chainCallID {
	r, ok := c.record(h)
	if !ok || r.kind&kindAssistant == 0 {
		return nil
	}
	return c.callsOf(h)
}

// Results is a user record's call ids ([threadChain.Uses]).
func (c *threadChain) Results(h chainHandle) []chainCallID {
	r, ok := c.record(h)
	if !ok || r.kind&kindUser == 0 {
		return nil
	}
	return c.callsOf(h)
}

// CallKey is id's key, where a block of the thread carries id.
func (c *threadChain) CallKey(id string) (chainCallID, bool) {
	key, ok := c.callIDs[sha256.Sum256([]byte(id))]
	if !ok || !c.carried[key-1] {
		return 0, false
	}
	return key, true
}

// Leaf is the record the resume walked from: in the main thread the leaf
// every variant walks from. False where q1t's variants walk from another, and
// where no record was walked.
func (c *threadChain) Leaf() (chainHandle, bool) {
	if c.state != chainDecided || c.why != "" || c.leaf == noChainRecord {
		return noChainRecord, false
	}
	return c.leaf, true
}

// record is h's filed record, where the chain holds it.
func (c *threadChain) record(h chainHandle) (chainRecord, bool) {
	if h <= 0 || int(h) > len(c.recs) {
		return chainRecord{}, false
	}
	return c.recs[h-1], true
}

// callsOf is record h's call-id occurrences, clipped so no append reaches
// the next record's.
func (c *threadChain) callsOf(h chainHandle) []chainCallID {
	start, end := c.recs[h-1].calls, int32(len(c.occurs))
	if int(h) < len(c.recs) {
		end = c.recs[h].calls
	}
	if start == end {
		return nil
	}
	return c.occurs[start:end:end]
}

// prefixesOf is record h's retraction prefixes.
func (c *threadChain) prefixesOf(h chainHandle) [][chainPrefixBytes]byte {
	start, end := c.recs[h-1].prefix, int32(len(c.prefixes))
	if int(h) < len(c.recs) {
		end = c.recs[h].prefix
	}
	return c.prefixes[start:end:end]
}

// is reports whether record h has every kind bit of k.
func (c *threadChain) is(h chainHandle, k chainKind) bool { return c.recs[h-1].kind&k == k }

// turn reports whether record h is a user or an assistant record.
func (c *threadChain) turn(h chainHandle) bool {
	return c.recs[h-1].kind&(kindUser|kindAssistant) != 0
}

// uf reports whether record h hangs off the reply before it (2.1.322's UF
// @293588888, 2.1.310's l0 @246995738): an attachment, a system record but a
// compaction's boundary, or a meta user record holding no result.
func (c *threadChain) uf(h chainHandle) bool {
	k := c.recs[h-1].kind
	switch {
	case k&kindAttachment != 0:
		return true
	case k&kindSystem != 0:
		return k&kindBoundary == 0
	case k&kindUser != 0:
		return k&kindMeta != 0 && k&kindResult == 0
	}
	return false
}

// partOf reports whether record h is part of reply id's run (2.1.322's X_e
// @293748740, 2.1.310's Yce @247133691): a record of the reply, a meta user
// record or one of results, any attachment, a system record but a
// compaction's boundary.
func (c *threadChain) partOf(h chainHandle, id chainReplyID) bool {
	r := c.recs[h-1]
	switch {
	case r.kind&kindAssistant != 0:
		return id != 0 && r.reply == id
	case r.kind&kindUser != 0:
		return r.kind&(kindMeta|kindResult) != 0
	case r.kind&kindAttachment != 0:
		return true
	case r.kind&kindSystem != 0:
		return r.kind&kindBoundary == 0
	}
	return false
}

// tailable reports whether record h is one the tails may hang: one that is
// neither a user nor an assistant record, under the leaf, or a UF record
// under a reply ([threadChain], THE RECOVERY and THE TAILS).
func (c *threadChain) tailable(h chainHandle) bool {
	return c.recs[h-1].kind&(kindAttachment|kindSystem) != 0 || c.uf(h)
}

// readsRetractions reports whether the release of variants reads record h's
// retraction list: 2.1.322's eGe a user, assistant or fallback record's,
// 2.1.310's qGn a fallback's alone.
func (c *threadChain) readsRetractions(h chainHandle, variants chainVariants) bool {
	k := c.recs[h-1].kind
	if k&kindFallback != 0 {
		return true
	}
	return variants&variants310 == 0 && k&(kindUser|kindAssistant) != 0
}

// chainWork is what End works with, dropped once it decided.
type chainWork struct {
	// childAt and kids are each record's children by parent, in the file's
	// order: record h's are kids[childAt[h]:childAt[h+1]].
	childAt []int32
	kids    []chainHandle
	// groupAt and group are each reply's records, in the file's order: reply
	// r's are group[groupAt[r]:groupAt[r+1]].
	groupAt []int32
	group   []chainHandle
	// cp is the record the checkpoint names, where it names one.
	cp chainHandle
	// a and b are sets of records each step clears and fills, run y1t's run
	// of a batch's reply; met is the walk's and the recovery's; replied the
	// replies the recovery met.
	a, b, run, met, replied chainMarks
	// calls and taken are sets of call ids each step clears and fills: a
	// reply's calls, and those y1t's descent took, so a list of ids is held
	// to another an id at a time, never one list walked for each of the
	// other's.
	calls, taken chainMarks
	// pos is each record's place on the walk, -1 off it, by handle - 1.
	pos []int32
	// sides are the walk's fallback's candidates, by isSidechain as written.
	sides [3]chainSide
	built bool
	// ticks counts the steps since End last asked ctx.
	ticks int
}

// chainMarks is a set of small numbers that clears at once.
type chainMarks struct {
	gen uint32
	at  []uint32
}

// newChainMarks is an empty set of the numbers 0 to n.
func newChainMarks(n int) chainMarks { return chainMarks{gen: 1, at: make([]uint32, n+1)} }

// clear empties the set.
func (m *chainMarks) clear() {
	m.gen++
	if m.gen == 0 {
		clear(m.at)
		m.gen = 1
	}
}

// set adds i.
func (m *chainMarks) set(i int) { m.at[i] = m.gen }

// has reports whether i is in the set.
func (m *chainMarks) has(i int) bool { return m.at[i] == m.gen }

// chainSide is the timed records of one isSidechain value, by time and then
// the later in the file first, as 2.1.322's vjt orders them (`reverse()`, then
// a stable sort by time), and prev, its pointer to the next candidate down
// that a walk has not met yet, which the walk compresses as it goes.
type chainSide struct {
	recs []chainHandle
	prev []int32
}

// chainCheckEvery is how many steps End takes between two looks at its ctx,
// a power of two so the count is cheap to test. It bounds how long End runs
// on once its caller gives it up: a step is a few reads of the chain's
// arrays, so 4,096 of them take well under a millisecond. And it bounds what
// the looks cost: each is a call through every context the caller wrapped,
// and one in 4,096 steps is nothing End could measure. End's time at the
// chain's bounds is what the facility's benchmark measures (`test(executor):
// measure the chain facility at its bounds`), which restates this value
// where it finds a step dearer than that.
const chainCheckEvery = 1 << 12

// tick counts a step, and answers ctx's error every chainCheckEvery steps.
func (w *chainWork) tick(ctx context.Context) error {
	w.ticks++
	if w.ticks%chainCheckEvery != 0 {
		return nil
	}
	return ctx.Err()
}

// children is record h's children by parent.
func (w *chainWork) children(h chainHandle) []chainHandle {
	return w.kids[w.childAt[h]:w.childAt[h+1]]
}

// records is reply r's records.
func (w *chainWork) records(r chainReplyID) []chainHandle {
	return w.group[w.groupAt[r]:w.groupAt[r+1]]
}

// work resolves every record's parent and the checkpoint by uuid, and indexes
// the records by parent and by reply.
func (c *threadChain) work() *chainWork {
	n := len(c.recs)
	w := &chainWork{pos: make([]int32, n), a: newChainMarks(n), b: newChainMarks(n), run: newChainMarks(n),
		met: newChainMarks(n), replied: newChainMarks(len(c.replies)), calls: newChainMarks(len(c.callIDs)),
		taken: newChainMarks(len(c.callIDs))}
	for i := range c.recs {
		if written := c.written[i]; written.named {
			if p := c.index[written.uuid]; p != noChainRecord {
				c.recs[i].parent = p
			} else {
				c.recs[i].kind |= kindNowhere
			}
		}
	}
	if c.checkpoint.set && c.checkpoint.named {
		w.cp = c.index[c.checkpoint.uuid]
	}
	w.childAt = make([]int32, n+2)
	w.groupAt = make([]int32, len(c.replies)+2)
	for _, r := range c.recs {
		if r.parent != noChainRecord {
			w.childAt[r.parent+1]++
		}
		if r.reply != 0 {
			w.groupAt[r.reply+1]++
		}
	}
	for i := 1; i < len(w.childAt); i++ {
		w.childAt[i] += w.childAt[i-1]
	}
	for i := 1; i < len(w.groupAt); i++ {
		w.groupAt[i] += w.groupAt[i-1]
	}
	w.kids, w.group = make([]chainHandle, w.childAt[n+1]), make([]chainHandle, w.groupAt[len(c.replies)+1])
	childNext, groupNext := slices.Clone(w.childAt), slices.Clone(w.groupAt)
	for i, r := range c.recs {
		h := chainHandle(i + 1)
		if r.parent != noChainRecord {
			w.kids[childNext[r.parent]] = h
			childNext[r.parent]++
		}
		if r.reply != 0 {
			w.group[groupNext[r.reply]] = h
			groupNext[r.reply]++
		}
	}
	return w
}

// decide is End's work: each step of the model in the order the resume runs
// it ([threadChain]).
func (c *threadChain) decide(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case c.why != "":
	case c.agent != "":
		c.cannotTell(whySubagentMode)
	case c.bytes > claudeResumeLargeFile:
		c.cannotTell(whyLargeFile)
	case c.relinks:
		c.cannotTell(whyRelink)
	}
	if c.why != "" {
		return nil
	}
	if len(c.recs) == 0 {
		// No record: the resume finds no conversation, and hands nothing
		// under every variant (2.1.322's Yya, `if(s.size===0)return null`).
		c.orders = []chainOrder{{variants: allVariants}}
		return nil
	}
	w := c.work()
	switch byCall, err := c.resultByCall(ctx, w); {
	case err != nil:
		return err
	case byCall:
		c.cannotTell(whyResultByCall)
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	leaf0, leaf1, err := c.pick(ctx, w)
	if err != nil || c.why != "" {
		return err
	}
	groups := []chainGroup{{leaf: leaf0, variants: variants310}, {leaf: leaf0, variants: allVariants &^ variants310}}
	if leaf1 != leaf0 {
		groups[1].variants &^= variantsMetcalfe
		groups = append(groups, chainGroup{leaf: leaf1, variants: variantsMetcalfe})
	}
	leaves := []chainHandle{leaf0}
	if leaf1 != leaf0 {
		leaves = append(leaves, leaf1)
	}
	for _, leaf := range leaves {
		base, how, listErr := c.listFrom(ctx, w, leaf)
		if listErr != nil || c.why != "" {
			return listErr
		}
		for i := range groups {
			if groups[i].leaf == leaf {
				groups[i].base, groups[i].how = base, how
			}
		}
	}
	for i := range groups {
		if judgeErr := c.judge(ctx, w, &groups[i]); judgeErr != nil {
			return judgeErr
		}
	}
	c.merge(groups)
	if leaf0 == leaf1 {
		c.leaf = leaf0
	}
	return nil
}

// chainGroup is the variants that walk from one leaf under one release's
// retraction filter, what they hand, and why.
type chainGroup struct {
	leaf     chainHandle
	variants chainVariants
	// base is the walk and the recovery from the leaf, the handed list before
	// the filter, and how each record of it was reached, by handle.
	base []chainHandle
	how  []uint8
	// verdicts is each record's answer under these variants, by handle - 1,
	// and order the records they hand for certain.
	verdicts []chainVerdict
	order    []chainHandle
}

// whyAt is w's place in chainWhys.
func whyAt(w chainWhy) uint8 { return uint8(slices.Index(chainWhys[:], w)) }

// resultByCall reports whether a result foamy_spring may hang elsewhere than
// under its parent is in the thread: a result whose call ids are not all of
// calls its parent made, where its sourceToolAssistantUUID names a record other
// than its parent, or another reply than its parent made one of its calls
// (2.1.322's fWo @293749272 and 2.1.310's Rpo @247134223, the odd results:
// with the flag on, each is hung under that record too, and may be recovered
// with its reply). The second test widens fWo's, which takes per call the last
// reply holding it where every reply holding it is of one id: any reply but
// its parent, whatever its side or agent, so the chain refuses no less than
// the resume hangs.
//
// ONE PASS, linear in the records and their call ids: a reply's calls are
// marked once, as the pass meets the reply, and each result under it is held
// to those marks an id at a time, never its ids walked for each of the
// reply's (a reply of 2^16 calls and its result of as many would be 2^32
// steps). It looks at ctx as it goes.
func (c *threadChain) resultByCall(ctx context.Context, w *chainWork) (bool, error) {
	var holders [][2]chainHandle
	// odd reports whether result r, whose ids are not all of calls its parent
	// made, has a record foamy_spring may hang it under.
	odd := func(r chainHandle) bool {
		parent := c.recs[r-1].parent
		if source, ok := c.sources[r]; ok {
			if s := c.index[source]; s != noChainRecord && s != parent {
				return true
			}
		}
		if holders == nil {
			holders = c.holders()
		}
		for _, id := range c.callsOf(r) {
			if hold := holders[id]; hold[0] != noChainRecord && (hold[0] != parent || hold[1] != noChainRecord) {
				return true
			}
		}
		return false
	}
	for i := range c.recs {
		if err := w.tick(ctx); err != nil {
			return false, err
		}
		h := chainHandle(i + 1)
		switch {
		case c.is(h, kindUser|kindResult):
			// A result whose parent is no reply answers no call of its
			// parent's.
			if p := c.recs[i].parent; (p == noChainRecord || !c.is(p, kindAssistant)) && odd(h) {
				return true, nil
			}
		case c.is(h, kindAssistant):
			marked := false
			for _, x := range w.children(h) {
				if !c.is(x, kindUser|kindResult) {
					continue
				}
				if !marked {
					w.calls.clear()
					for _, id := range c.callsOf(h) {
						if err := w.tick(ctx); err != nil {
							return false, err
						}
						w.calls.set(int(id))
					}
					marked = true
				}
				for _, id := range c.callsOf(x) {
					if err := w.tick(ctx); err != nil {
						return false, err
					}
					if !w.calls.has(int(id)) {
						if odd(x) {
							return true, nil
						}
						break
					}
				}
			}
		}
	}
	return false, nil
}

// holders is, by call id, the first two replies whose tool_use blocks carry
// it.
func (c *threadChain) holders() [][2]chainHandle {
	holders := make([][2]chainHandle, len(c.callIDs)+1)
	for i := range c.recs {
		h := chainHandle(i + 1)
		if !c.is(h, kindAssistant) {
			continue
		}
		for _, id := range c.callsOf(h) {
			switch hold := &holders[id]; {
			case hold[0] == noChainRecord:
				hold[0] = h
			case hold[0] != h && hold[1] == noChainRecord:
				hold[1] = h
			}
		}
	}
	return holders
}

// pick is the leaf the resume walks from, with q1t's variants off (leaf0)
// and on (leaf1): noChainRecord where the resume finds no conversation
// ([threadChain], THE LEAF). Where the single path finds no turn record the
// thread is whyLeafSet, and where q1t cannot read a reply's id whyReplyID.
func (c *threadChain) pick(ctx context.Context, w *chainWork) (leaf0, leaf1 chainHandle, err error) {
	last := c.lastRow
	if last == noChainRecord {
		c.cannotTell(whyLeafSet)
		return noChainRecord, noChainRecord, nil
	}
	start := last
	if !c.anyLeafKey && c.newest != noChainRecord && c.newest != last {
		if start, err = c.newestUnder(ctx, w, last); err != nil {
			return noChainRecord, noChainRecord, err
		}
	}
	from0, from1 := w.cp, w.cp
	if w.cp != noChainRecord && w.cp != last {
		if c.y1t(w, last, w.cp) {
			from0, from1 = last, last
		} else {
			fires, read := c.q1t(w, last, w.cp)
			if !read {
				c.cannotTell(whyReplyID)
				return noChainRecord, noChainRecord, nil
			}
			if fires {
				from1 = last
			}
		}
	}
	if from0 == noChainRecord {
		from0, from1 = start, start
	}
	turn0 := c.nearestTurn(w, from0)
	turn1 := turn0
	if from1 != from0 {
		turn1 = c.nearestTurn(w, from1)
	}
	if turn0 == noChainRecord || turn1 == noChainRecord {
		c.cannotTell(whyLeafSet)
		return noChainRecord, noChainRecord, nil
	}
	return c.timedLeaf(turn0), c.timedLeaf(turn1), nil
}

// timedLeaf is turn, where it has a timestamp: the resume takes its leaf as
// the latest Date.parse of its set (2.1.322's cj @293745290, 2.1.310's p0
// @247129929, `y>s` from -Infinity), which no record of none is, and finds no
// conversation where none is.
func (c *threadChain) timedLeaf(turn chainHandle) chainHandle {
	if !c.is(turn, kindTimed) {
		return noChainRecord
	}
	return turn
}

// nearestTurn is the nearest user or assistant record up from from by
// parent, from itself; none where the walk meets a root, a parent naming no
// record, or a record twice (2.1.322's Gr, 2.1.310's kr).
func (c *threadChain) nearestTurn(w *chainWork, from chainHandle) chainHandle {
	w.a.clear()
	for at := from; at != noChainRecord; at = c.recs[at-1].parent {
		if w.a.has(int(at)) {
			return noChainRecord
		}
		w.a.set(int(at))
		if c.turn(at) {
			return at
		}
	}
	return noChainRecord
}

// newestUnder is the record of the latest timestamp whose walk by parent
// reaches last, the later in the file on a tie, not a sidechain's; last
// where none is (2.1.322's Gr and 2.1.310's kr, `Ks`, compared as strings,
// `>=`). Its walks are one: last's descendants marked over the children
// index.
func (c *threadChain) newestUnder(ctx context.Context, w *chainWork, last chainHandle) (chainHandle, error) {
	w.a.clear()
	w.a.set(int(last))
	stack := []chainHandle{last}
	for len(stack) > 0 {
		at := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, kid := range w.children(at) {
			if err := w.tick(ctx); err != nil {
				return noChainRecord, err
			}
			if !w.a.has(int(kid)) {
				w.a.set(int(kid))
				stack = append(stack, kid)
			}
		}
	}
	start, found := last, false
	var ms int64
	for i, r := range c.recs {
		h := chainHandle(i + 1)
		if h == last || !w.a.has(int(h)) || r.kind&kindTimed == 0 || r.kind&kindSideTrue != 0 {
			continue
		}
		if !found || r.ms >= ms {
			start, ms, found = h, r.ms, true
		}
	}
	return start, nil
}

// y1t reports whether the last row stays the start though the checkpoint
// names another record (2.1.322's Y1t @293812412, 2.1.310's WPt @247189354,
// as the single path asks it, direct, and throughBatch always, since no
// checkpoint the chain models was ever marked): the last row descends from
// the checkpoint, or through the checkpoint's batch.
func (c *threadChain) y1t(w *chainWork, last, cp chainHandle) bool {
	var (
		y      chainHandle
		taken  []chainCallID
		byHook bool
	)
	// THE DESCENT: from the checkpoint up through the records that hang off a
	// reply, to the first that does not.
	w.a.clear()
	var hung chainHandle
	at := cp
	for at != noChainRecord && c.uf(at) && !w.a.has(int(at)) {
		w.a.set(int(at))
		hung, at = at, c.recs[at-1].parent
	}
	switch {
	case at != noChainRecord && c.is(at, kindUser|kindResult):
		taken, y = c.callsOf(at), c.recs[at-1].parent
	case hung != noChainRecord:
		// The batch's call, by the toolUseID the last of its hook's records
		// names, where that is an attachment's string: the first of the reply
		// above and its run up whose calls include it.
		var call chainCallID
		if calls := c.callsOf(hung); c.is(hung, kindAttachment) && len(calls) == 1 {
			call = calls[0]
		}
		if at != noChainRecord && c.is(at, kindAssistant) && c.recs[at-1].reply != 0 && call != 0 {
			for _, x := range append([]chainHandle{at}, c.x1t(w, at, c.recs[at-1].reply)...) {
				if slices.Contains(c.usesOf(x), call) {
					y = x
					break
				}
			}
		}
		if y != noChainRecord && call != 0 {
			taken, byHook = []chainCallID{call}, true
		}
	default:
		y = at
	}
	uses := c.usesOf(y)
	var id chainReplyID
	if len(uses) > 0 {
		id = c.recs[y-1].reply
	}
	// The batch's reply's calls and those the descent took, as sets: the walk
	// up holds a results record to them an id at a time.
	w.calls.clear()
	for _, x := range uses {
		w.calls.set(int(x))
	}
	w.taken.clear()
	shared := false
	for _, x := range taken {
		w.taken.set(int(x))
		shared = shared || w.calls.has(int(x))
	}
	w.run.clear()
	if id != 0 {
		for _, x := range c.x1t(w, y, id) {
			w.run.set(int(x))
		}
	}
	// THE WALK UP from the last row.
	w.a.clear()
	sibling := false
	var below chainHandle
	for cur := last; cur != noChainRecord && !w.a.has(int(cur)); cur = c.recs[cur-1].parent {
		if cur == cp {
			return true
		}
		w.a.set(int(cur))
		if id != 0 {
			if sibling && cur == y {
				return true
			}
			if c.is(cur, kindAssistant|kindUse) && cur != y && c.recs[cur-1].reply == id {
				if w.run.has(int(cur)) {
					return true
				}
				sibling = true
			} else if !c.partOf(cur, id) {
				sibling = false
			}
		}
		if y != noChainRecord && cur == y && below != noChainRecord && shared &&
			c.answersAnother(w, below, len(uses), byHook) {
			return true
		}
		below = cur
	}
	return false
}

// answersAnother reports whether below, the record under the batch's reply
// on the walk up, is one of its results answering a call of it other than
// the checkpoint's (`aj(ne).some(...)`): any of its calls (w.calls) but those
// taken (w.taken), or, where the batch was found by its hook, any where the
// reply made more than one, uses being how many it made.
func (c *threadChain) answersAnother(w *chainWork, below chainHandle, uses int, byHook bool) bool {
	for _, x := range c.resultsOf(below) {
		if w.calls.has(int(x)) && (byHook && uses > 1 || !byHook && !w.taken.has(int(x))) {
			return true
		}
	}
	return false
}

// x1t is the run up from from's parent of records part of reply id's
// (2.1.322's X1t @293813630, 2.1.310's GPt @247190569), each met once.
func (c *threadChain) x1t(w *chainWork, from chainHandle, id chainReplyID) []chainHandle {
	w.b.clear()
	w.b.set(int(from))
	var run []chainHandle
	for at := c.recs[from-1].parent; at != noChainRecord && !w.b.has(int(at)) && c.partOf(at, id); at = c.recs[at-1].parent {
		w.b.set(int(at))
		run = append(run, at)
	}
	return run
}

// q1t reports whether 2.1.322's q1t (@293811681) holds of the last row and
// the checkpoint, as the metcalfe variants read it: the checkpoint is a reply
// that made a call; above its run of records of its own id lies a record; the
// walk up from the last row reaches that record through a record that is no
// part of the run, of a later timestamp than the checkpoint's, beside one
// that is a reply saying something or making a call (LE @293601245); no
// record hangs off the checkpoint, and none off the last row but one that
// hangs off a reply. read is false where the run turns on a reply's id the
// layer reads as none, which may be absent, where the run stops, or empty,
// where it goes on (`message?.id!==void 0`).
func (c *threadChain) q1t(w *chainWork, last, cp chainHandle) (fires, read bool) {
	if !c.is(cp, kindAssistant|kindUse) {
		return false, true
	}
	w.a.clear()
	w.a.set(int(cp))
	id, above := c.recs[cp-1].reply, c.recs[cp-1].parent
	if id == 0 {
		if above != noChainRecord && c.is(above, kindAssistant) && c.recs[above-1].reply == 0 {
			return false, false
		}
	} else {
		for above != noChainRecord && c.is(above, kindAssistant) && c.recs[above-1].reply == id &&
			!w.a.has(int(above)) {
			w.a.set(int(above))
			above = c.recs[above-1].parent
		}
	}
	if above == noChainRecord {
		return false, true
	}
	w.b.clear()
	var later chainHandle
	speaks := false
	at := last
	for at != noChainRecord && at != above {
		if w.b.has(int(at)) {
			return false, true
		}
		w.b.set(int(at))
		if !w.a.has(int(at)) {
			later = at
			speaks = speaks || c.speaks(at)
		}
		at = c.recs[at-1].parent
	}
	if at == noChainRecord || !speaks || later == noChainRecord || !c.after(later, cp) {
		return false, true
	}
	if len(w.children(cp)) > 0 {
		return false, true
	}
	for _, kid := range w.children(last) {
		if !c.uf(kid) && !c.is(kid, kindSideTrue) {
			return false, true
		}
	}
	return true, true
}

// speaks reports whether record h is a reply its model wrote that makes a
// call or says something, as 2.1.322's LE classes one assistant_calls or
// assistant_text.
func (c *threadChain) speaks(h chainHandle) bool {
	k := c.recs[h-1].kind
	return k&kindAssistant != 0 && k&kindSynthetic == 0 && k&(kindUse|kindSays) != 0
}

// after reports whether record h was written later than record of, as the
// resume compares their timestamps as strings: each present, and h's later.
func (c *threadChain) after(h, of chainHandle) bool {
	return c.is(h, kindTimed) && c.is(of, kindTimed) && c.recs[h-1].ms > c.recs[of-1].ms
}

// usesOf is record h's tool_use ids, an assistant record's; none for every
// other, and for noChainRecord.
func (c *threadChain) usesOf(h chainHandle) []chainCallID {
	if h == noChainRecord || !c.is(h, kindAssistant) {
		return nil
	}
	return c.callsOf(h)
}

// resultsOf is record h's tool_result ids, a user record's; none for every
// other.
func (c *threadChain) resultsOf(h chainHandle) []chainCallID {
	if h == noChainRecord || !c.is(h, kindUser) {
		return nil
	}
	return c.callsOf(h)
}

// listFrom is what the resume's walk and recovery hand from leaf, and how
// each record of it was reached, by handle: none where leaf is none.
func (c *threadChain) listFrom(ctx context.Context, w *chainWork, leaf chainHandle) ([]chainHandle, []uint8, error) {
	if leaf == noChainRecord {
		return nil, nil, nil
	}
	chain, err := c.walk(ctx, w, leaf)
	if err != nil {
		return nil, nil, err
	}
	how := make([]uint8, len(c.recs)+1)
	base, err := c.recover(ctx, w, chain, how)
	return base, how, err
}

// walk is the chain from leaf to its root, root first, its records met
// ([threadChain], THE WALK).
func (c *threadChain) walk(ctx context.Context, w *chainWork, leaf chainHandle) ([]chainHandle, error) {
	w.met.clear()
	for i := range w.sides {
		for j := range w.sides[i].prev {
			w.sides[i].prev[j] = int32(j)
		}
	}
	var chain []chainHandle
	for at := leaf; at != noChainRecord; {
		if err := w.tick(ctx); err != nil {
			return nil, err
		}
		// A record met already ends the walk, as A7t's own guard ends it
		// (`if(h.has(S.uuid))...break`): the walk reaches a record only
		// through a parent or a fallback that never hands it one walked.
		if w.met.has(int(at)) {
			break
		}
		w.met.set(int(at))
		chain = append(chain, at)
		r := c.recs[at-1]
		if r.parent == noChainRecord && r.kind&kindNowhere == 0 {
			break
		}
		next := r.parent
		if next == noChainRecord || w.met.has(int(next)) {
			next = c.fallback(w, at)
		}
		at = next
	}
	slices.Reverse(chain)
	return chain, nil
}

// fallback is the record the walk takes for at's parent where that names no
// record or one walked already (2.1.322's vjt @293589456, 2.1.310's Epo
// @247132981): of at's isSidechain as written, timed and not met, the latest
// at or before at's time and at most claudeFallbackMS before it, the first in
// the file on a tie; none where at has no time.
func (c *threadChain) fallback(w *chainWork, at chainHandle) chainHandle {
	r := c.recs[at-1]
	if r.kind&kindTimed == 0 {
		return noChainRecord
	}
	side := c.side(w, r.kind)
	end := sort.Search(len(side.recs), func(i int) bool { return c.recs[side.recs[i]-1].ms > r.ms })
	i := end - 1
	for i >= 0 {
		if p := int(side.prev[i]); p != i {
			i = p
			continue
		}
		if !w.met.has(int(side.recs[i])) {
			break
		}
		side.prev[i] = int32(i - 1)
		i--
	}
	for k := end - 1; k > i; {
		p := int(side.prev[k])
		side.prev[k] = int32(i)
		k = p
	}
	if i < 0 || r.ms-c.recs[side.recs[i]-1].ms > claudeFallbackMS {
		return noChainRecord
	}
	return side.recs[i]
}

// side is the fallback's candidates of a record of kind k's isSidechain,
// built once.
func (c *threadChain) side(w *chainWork, k chainKind) *chainSide {
	if !w.built {
		w.built = true
		for i, r := range c.recs {
			if r.kind&kindTimed != 0 {
				s := &w.sides[sideOf(r.kind)]
				s.recs = append(s.recs, chainHandle(i+1))
			}
		}
		for i := range w.sides {
			s := &w.sides[i]
			slices.SortFunc(s.recs, func(x, y chainHandle) int {
				if order := cmp.Compare(c.recs[x-1].ms, c.recs[y-1].ms); order != 0 {
					return order
				}
				return cmp.Compare(y, x)
			})
			s.prev = make([]int32, len(s.recs))
			for j := range s.prev {
				s.prev[j] = int32(j)
			}
		}
	}
	return &w.sides[sideOf(k)]
}

// sideOf is the place of a record of kind k's isSidechain: absent, false or
// true.
func sideOf(k chainKind) int {
	switch {
	case k&kindSideTrue != 0:
		return 2
	case k&kindSideFalse != 0:
		return 1
	}
	return 0
}

// chainWindow is a reply's window of the walk, chain[start:end], and the
// records the recovery placed in it among the walked ones: those it hands,
// never the ones it hangs ([threadChain.hang]).
type chainWindow struct {
	start, end int
	recovered  []chainHandle
}

// chainAnchors counts the records the recovery hangs under each walked record
// of a window, summed over every window holding that record, which all merge
// into one: the list the resume spreads into one call's arguments as it
// places them (`Ot.push(...an.get(so.uuid))`).
type chainAnchors struct {
	// under is the count by handle - 1, made at the first record hung.
	under []int32
	// most is the largest count.
	most int32
}

// add counts n records hung under u, a record of a thread of records.
func (a *chainAnchors) add(u chainHandle, n, records int) {
	if a.under == nil {
		a.under = make([]int32, records)
	}
	a.under[u-1] += int32(n)
	a.most = max(a.most, a.under[u-1])
}

// recover is chain with the records the recovery reaches placed in it
// ([threadChain], THE RECOVERY), and how each record of it was reached, into
// how by handle. The records it hangs open windows and are met, as the
// resume's are, but are no record of the list it answers.
func (c *threadChain) recover(ctx context.Context, w *chainWork, chain []chainHandle, how []uint8) ([]chainHandle, error) {
	for i := range w.pos {
		w.pos[i] = -1
	}
	walked, sibling, result := whyAt(whyChain), whyAt(whyRecoveredReply), whyAt(whyRecoveredResult)
	for i, h := range chain {
		w.pos[h-1] = int32(i)
		how[h] = walked
	}
	w.replied.clear()
	var (
		windows []chainWindow
		anchors chainAnchors
	)
	for i, d := range chain {
		if err := w.tick(ctx); err != nil {
			return nil, err
		}
		id := c.recs[d-1].reply
		if !c.is(d, kindAssistant) || id == 0 || w.replied.has(int(id)) {
			continue
		}
		w.replied.set(int(id))
		var recovered []chainHandle
		for _, g := range w.records(id) {
			if !w.met.has(int(g)) {
				recovered = append(recovered, g)
				how[g] = sibling
			}
		}
		for _, g := range w.records(id) {
			for _, x := range w.children(g) {
				if c.is(x, kindUser|kindResult) && !w.met.has(int(x)) {
					recovered = append(recovered, x)
					how[x] = result
				}
			}
		}
		for _, x := range recovered {
			w.met.set(int(x))
		}
		end := i + 1
		for _, g := range w.records(id) {
			if p := int(w.pos[g-1]); p >= end {
				end = p + 1
			}
		}
		for end < len(chain) && c.partOf(chain[end], id) {
			end++
		}
		hung, err := c.hang(ctx, w, chain, id, i, end, &anchors)
		if err != nil {
			return nil, err
		}
		if len(recovered) > 0 || hung {
			slices.Sort(recovered)
			windows = append(windows, chainWindow{start: i, end: end, recovered: recovered})
		}
	}
	if len(windows) == 0 {
		return chain, nil
	}
	if len(chain) > threadChainMaxSpread || anchors.most > threadChainMaxSpread {
		c.cannotTell(whySpread)
		return nil, nil
	}
	merged := make([]chainWindow, 0, len(windows))
	for _, win := range windows {
		if n := len(merged); n > 0 && win.start < merged[n-1].end {
			last := &merged[n-1]
			last.end = max(last.end, win.end)
			last.recovered = append(last.recovered, win.recovered...)
			slices.Sort(last.recovered)
			continue
		}
		merged = append(merged, win)
	}
	out := make([]chainHandle, 0, len(chain)+len(windows))
	prev := 0
	for _, win := range merged {
		out = append(out, chain[prev:win.start+1]...)
		j := 0
		for k := win.start + 1; k < win.end; k++ {
			for j < len(win.recovered) && win.recovered[j] < chain[k] {
				out = append(out, win.recovered[j])
				j++
			}
			out = append(out, chain[k])
		}
		out = append(out, win.recovered[j:]...)
		prev = win.end
	}
	return append(out, chain[prev:]...), nil
}

// hang is what the recovery hangs off reply id's window, chain[start:end],
// where a record of the reply made a call ([threadChain], THE RECOVERY; the
// `co` of 2.1.322's fWo and 2.1.310's Rpo): under each record of the reply,
// then under each result whose parent is one of them and that is met by now,
// then under each walked record of the window past its first that is UF, what
// [threadChain.pye] hangs from each of the record's children in turn, every
// record hung met at once. It reports whether it hung any record, and counts
// into anchors those it hung under the window's walked records.
//
// Each window takes its records' children afresh, as the resume does, rather
// than once a thread: a chain one window leaves, a record with two children
// not met, can hang in a later one once a recovery between them met one.
func (c *threadChain) hang(ctx context.Context, w *chainWork, chain []chainHandle, id chainReplyID, start, end int,
	anchors *chainAnchors) (bool, error) {
	group := w.records(id)
	if !slices.ContainsFunc(group, func(g chainHandle) bool { return c.is(g, kindUse) }) {
		return false, nil
	}
	var hung []chainHandle
	// under hangs under u what pye hands from each of u's children, and
	// answers how many records it hung.
	under := func(u chainHandle) (int, error) {
		n := 0
		for _, y := range w.children(u) {
			var err error
			if hung, err = c.pye(ctx, w, y, u, hung[:0]); err != nil {
				return 0, err
			}
			for _, x := range hung {
				w.met.set(int(x))
			}
			n += len(hung)
		}
		return n, nil
	}
	opens := false
	for _, g := range group {
		n, err := under(g)
		if err != nil {
			return false, err
		}
		opens = opens || n > 0
	}
	for _, g := range group {
		for _, x := range w.children(g) {
			if !c.is(x, kindUser|kindResult) || !w.met.has(int(x)) {
				continue
			}
			n, err := under(x)
			if err != nil {
				return false, err
			}
			opens = opens || n > 0
		}
	}
	for k := start + 1; k < end; k++ {
		if err := w.tick(ctx); err != nil {
			return false, err
		}
		u := chain[k]
		if !c.uf(u) {
			continue
		}
		n, err := under(u)
		if err != nil {
			return false, err
		}
		if n > 0 {
			anchors.add(u, n, len(c.recs))
			opens = true
		}
	}
	return opens, nil
}

// pye is what the recovery hangs under u from y, one of u's children,
// appended to into (2.1.322's pye @293588633, 2.1.310's cce @246995483): y
// and, after each record, its one child not met, while each is a UF record of
// u's isSidechain as written and not met; none at all where one is not, or
// where one has two or more children not met. A record has one parent, so the
// chain runs down u's subtree and meets no record twice: a cycle through y
// would run through u, which is met, and so never the child taken next.
func (c *threadChain) pye(ctx context.Context, w *chainWork, y, u chainHandle,
	into []chainHandle) ([]chainHandle, error) {
	side, from := sideOf(c.recs[u-1].kind), len(into)
	for y != noChainRecord {
		if err := w.tick(ctx); err != nil {
			return nil, err
		}
		if w.met.has(int(y)) || sideOf(c.recs[y-1].kind) != side || !c.uf(y) {
			return into[:from], nil
		}
		into = append(into, y)
		next := noChainRecord
		for _, kid := range w.children(y) {
			if w.met.has(int(kid)) {
				continue
			}
			if next != noChainRecord {
				return into[:from], nil
			}
			next = kid
		}
		y = next
	}
	return into, nil
}

// judge decides each record's answer under g's variants: g's base handed,
// the records the tails may hang cannot tell, the retraction filter of g's
// release, and the attachments the malformed-attachment drop may take cannot
// tell ([threadChain], THE TAILS, THE RETRACTION FILTER and THE
// MALFORMED-ATTACHMENT DROP).
func (c *threadChain) judge(ctx context.Context, w *chainWork, g *chainGroup) error {
	g.verdicts = make([]chainVerdict, len(c.recs))
	if g.leaf == noChainRecord {
		none := chainVerdict{handed: verdictNo, why: whyAt(whyNoConversation)}
		for i := range g.verdicts {
			g.verdicts[i] = none
		}
		return nil
	}
	off, tail := chainVerdict{handed: verdictNo, why: whyAt(whyOffConversation)},
		chainVerdict{handed: verdictUnknown, why: whyAt(whyTail)}
	for i := range g.verdicts {
		g.verdicts[i] = off
	}
	for _, h := range g.base {
		g.verdicts[h-1] = chainVerdict{handed: verdictYes, why: g.how[h]}
	}
	var names, tails map[[chainPrefixBytes]byte]bool
	for i := range g.verdicts {
		if err := w.tick(ctx); err != nil {
			return err
		}
		h := chainHandle(i + 1)
		v := &g.verdicts[i]
		if v.handed == verdictNo && c.tailable(h) {
			*v = tail
		}
		if v.handed == verdictNo || !c.readsRetractions(h, g.variants) {
			continue
		}
		for _, p := range c.prefixesOf(h) {
			switch {
			case v.handed == verdictYes:
				if names == nil {
					names = map[[chainPrefixBytes]byte]bool{}
				}
				names[p] = true
			default:
				if tails == nil {
					tails = map[[chainPrefixBytes]byte]bool{}
				}
				tails[p] = true
			}
		}
	}
	for _, h := range g.base {
		v := &g.verdicts[h-1]
		if c.is(h, kindSystem) {
			continue
		}
		var p [chainPrefixBytes]byte
		copy(p[:], c.recs[h-1].uuid[:chainPrefixBytes])
		switch {
		case names[p]:
			*v = chainVerdict{handed: verdictNo, why: whyAt(whyRetracted)}
		case tails[p]:
			*v = tail
		}
	}
	for _, h := range g.base {
		if v := &g.verdicts[h-1]; v.handed == verdictYes && c.is(h, kindAttachment|kindIllFormed) {
			*v = chainVerdict{handed: verdictUnknown, why: whyAt(whyAttachmentDrop)}
		}
	}
	for _, h := range g.base {
		if g.verdicts[h-1].handed == verdictYes {
			g.order = append(g.order, h)
		}
	}
	return nil
}

// merge folds each group's answers into the thread's: handedYes where every
// group hands a record, handedNo where none does, each with the default
// variant's reason; handedUnknown where a group cannot tell, with its reason,
// or where the groups part, whyLeafFlag where q1t's leaf parts them and else
// whyRelease. Each distinct order is kept once, with the variants that hand
// it, the default variant's first.
func (c *threadChain) merge(groups []chainGroup) {
	def := slices.IndexFunc(groups, func(g chainGroup) bool { return g.variants&variantDefault != 0 })
	metcalfe := slices.IndexFunc(groups, func(g chainGroup) bool { return g.variants == variantsMetcalfe })
	c.verdicts = make([]chainVerdict, len(c.recs))
	for i := range c.verdicts {
		var yes, no bool
		unknown := -1
		for gi, g := range groups {
			switch g.verdicts[i].handed {
			case verdictYes:
				yes = true
			case verdictNo:
				no = true
			default:
				if unknown < 0 || gi == def {
					unknown = gi
				}
			}
		}
		switch {
		case unknown >= 0:
			c.verdicts[i] = groups[unknown].verdicts[i]
		case yes && no && metcalfe >= 0 && groups[metcalfe].verdicts[i].handed != groups[def].verdicts[i].handed:
			c.verdicts[i] = chainVerdict{handed: verdictUnknown, why: whyAt(whyLeafFlag)}
		case yes && no:
			c.verdicts[i] = chainVerdict{handed: verdictUnknown, why: whyAt(whyRelease)}
		default:
			c.verdicts[i] = groups[def].verdicts[i]
		}
	}
	ordered := append([]chainGroup{groups[def]}, slices.Delete(slices.Clone(groups), def, def+1)...)
	for _, g := range ordered {
		at := slices.IndexFunc(c.orders, func(o chainOrder) bool { return slices.Equal(o.records, g.order) })
		if at >= 0 {
			c.orders[at].variants |= g.variants
			continue
		}
		o := chainOrder{records: g.order, at: make([]int32, len(c.recs)), variants: g.variants}
		for i := range o.at {
			o.at[i] = -1
		}
		for i, h := range g.order {
			o.at[h-1] = int32(i)
		}
		c.orders = append(c.orders, o)
	}
}
