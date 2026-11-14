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
	// whyHookChain is a record the recovery hangs off a reply on the walk that
	// made a call: one of a run of records that hang off a reply, each the one
	// child not met yet of the one before, under a record of the reply, a
	// result beside it, or a record of its window on the walk that hangs off a
	// reply itself ([threadChain], THE RECOVERY), as a batch's hooks write
	// their records under its results.
	whyHookChain chainWhy = "hook_chain"
	// whyLeafTail is a record the tails hang under the leaf: neither a user
	// nor an assistant record, under the leaf through records that are neither
	// ([threadChain], THE TAILS), as a Stop hook writes its records under the
	// turn's last reply.
	whyLeafTail chainWhy = "leaf_tail"
)

// Not handed: why the resume hands a record under no variant.
const (
	// whyOffConversation is a record no walk, recovery or tail reaches.
	whyOffConversation chainWhy = "off_conversation"
	// whyRetracted is a record the retraction filter drops.
	whyRetracted chainWhy = "retracted"
	// whyRelinkDeleted is a record the resume's relink deletes: written before
	// the last compaction's boundary, where a boundary names what it kept,
	// and not among the records it kept ([threadChain], THE RELINK).
	whyRelinkDeleted chainWhy = "relink_deleted"
	// whyMalformedAttachment is an attachment the resume's drop of malformed
	// attachments takes, after its retraction filter: one whose payload is not
	// of its type's shape ([threadLine.AttachmentWellFormed]; [threadChain],
	// THE MALFORMED-ATTACHMENT DROP). The main thread's alone: no such drop
	// follows a subagent's own load.
	whyMalformedAttachment chainWhy = "malformed_attachment"
	// whyAnotherAgent is a record a subagent's own load walks, recovers, hangs
	// or tails whose agentId is not that subagent's, none among it: the load
	// hands on the records of the subagent's own id alone ([threadChain], A
	// SUBAGENT'S OWN LOAD).
	whyAnotherAgent chainWhy = "another_agent"
	// whyNoConversation is every record of a thread the resume, or a
	// subagent's own load, finds no conversation in, and so hands nothing of,
	// but one its relink deleted.
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
	// of an API error or model is in no shape, or, in the main thread, an
	// attachment the layer cannot say the malformed-attachment drop keeps.
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
	// whyRelinkShape is a relink that would chain the records a compaction
	// kept into a loop, or under a record it does not keep: an anchor naming
	// no record or one the relink deletes, an anchor among the kept records
	// or below one of them, the last boundary among them, or a record listed
	// twice ([threadChain], THE RELINK).
	whyRelinkShape chainWhy = "relink_shape"
	// whyReplacementShape is a content-replacement record whose list 2.1.310's
	// load fails at ([threadLine.ReplacementCount]).
	whyReplacementShape chainWhy = "replacement_shape"
	// whySpread is a list the resume passes into one call's arguments whole
	// longer than [threadChainMaxSpread].
	whySpread chainWhy = "spread"
	// whyLargeFile is a thread past 5,242,880 bytes that is not path-free
	// ([threadChain], THE LARGE FILE): one of the load's large-file readers
	// may hand the load other lines of it than its small path does.
	whyLargeFile chainWhy = "large_file"
	// whyResultByCall is a result 2.1.322's and 2.1.310's flag
	// tengu_foamy_spring may hang elsewhere than under its parent.
	whyResultByCall chainWhy = "result_by_call"
	// whyTailTime is a sort the tails make by time that holds a record of no
	// time beside two records of different times: the resume's comparison
	// reads the record of no time as equal to each of them, which orders none,
	// so its engine's sort algorithm decides their order ([threadChain], THE
	// TAILS).
	whyTailTime chainWhy = "tail_time"
	// whyForkContext is a fork's reference in a subagent's thread, any
	// agent's: where it names the subagent, its own load hands the parent's
	// conversation before its own ([threadChain], A SUBAGENT'S OWN LOAD).
	whyForkContext chainWhy = "fork_context"
	// whyAgent is the thread of a subagent id no member is named by
	// ([validClaudeAgent]).
	whyAgent chainWhy = "agent"
)

// whyUndecided is the answer to a question asked of a chain End has not
// decided: before End, or after an End its context cut short. A reader's
// misuse, answered as cannot tell rather than a panic, since a reader runs
// inside a node's collection, which a panic would end for every seat the node
// holds.
const whyUndecided chainWhy = "undecided"

// chainWhys is every reason, the place each answer stores its reason by.
var chainWhys = [...]chainWhy{"", whyChain, whyRecoveredReply, whyRecoveredResult, whyHookChain, whyLeafTail,
	whyOffConversation, whyRetracted, whyRelinkDeleted, whyMalformedAttachment, whyAnotherAgent, whyNoConversation,
	whyNoRecord, whyLeafFlag, whyRelease, whyBound, whyLine, whyLineGap, whyProgress, whyUUID,
	whyDuplicateUUID, whySidechain, whyFileSnapshot, whyForkBriefing, whyMarkedCheckpoint, whyCheckpointShape,
	whyAdmission, whyReplyID, whyCallID, whyTimestamp, whyRetractionShape, whyPreservedShape, whyRelinkShape,
	whyReplacementShape, whySpread, whyLargeFile, whyResultByCall, whyTailTime, whyForkContext, whyAgent,
	whyUndecided}

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
	// variantsMochi is those with warm_mochi on, whose tails are a walk by
	// depth; every other sorts the records of a walk by breadth (THE TAILS).
	variantsMochi = variant310SpringMochi | variant322SpringMochi | variant322SpringMochiMetcalfe
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
	// its records, the retraction prefixes they carry, and the uuids its
	// newest compaction's boundary that names what it kept lists.
	threadChainMaxRecords = 1 << 17
	// threadChainMaxCalls is the most call-id occurrences the chain files of
	// one thread: each tool_use and tool_result block's id, and each
	// attachment's string toolUseID.
	threadChainMaxCalls = 1 << 17
	// threadChainMaxSpread is the longest list the chain reads the resume as
	// passing into one call's arguments whole (whySpread): the load's spread
	// of a content-replacement record's list (2.1.322's `rr.push(...Ks)`
	// @293820306, 2.1.310's `Kr.push(...Eo.replacements)` @247196171); the
	// recovery's of each stretch of the walk outside its windows and of the
	// records it hangs under one walked record (`Ot.push(...n.slice(Ct,Dt+1))`
	// @293753056, `Ot.push(...n.slice(Ct))` @293753294,
	// `Ot.push(...an.get(so.uuid)??[])` @293753212; 2.1.310's @247138003,
	// @247138241, @247138159); and the tails' walk by breadth's
	// (`n.push(...h)` @293747616, @247132204). The recovery spreads a reply's
	// records into Math.max as well (@293751323, @247136270), only where it
	// hangs a result by its call's id, which the chain refuses
	// (whyResultByCall).
	//
	// JavaScriptCore, the engine Claude Code's runtime runs on, refuses a
	// call more than 65,536 arguments (its maxArguments, 0x10000, read off
	// its source: a constant compiled into the binaries, which no string in
	// them states), throwing its stack's overflow, the RangeError "Maximum
	// call stack size exceeded." (2.1.322 @14459602, beside its other error
	// in sizing a call's arguments, "second argument to
	// Function.prototype.apply must be an Array-like object", @14459636;
	// 2.1.310 @3746582, @3010140). Half that bound, so a thread the chain
	// models is one no such spread comes near. And the most the code both
	// binaries ship passes into one call where it cuts a list to pass it:
	// each helper that hands a long list to String.fromCharCode.apply cuts it
	// into 32,768 elements or fewer (2.1.322 @306148813, @315444399 and
	// @330630853, 16,384 @309815132, 8,192 @329652122; 2.1.310 @259142857,
	// @265881921 and @279618288, 16,384 @262021569, 8,192 @280205435).
	threadChainMaxSpread = 1 << 15
)

// claudeResumeLargeFile is the most bytes of a thread Claude Code's load
// reads with its small path; a thread past it goes to a large-file reader
// (2.1.322's Jne @284268664 in oee @293824518, `if(Ae>Jne)` @293827191;
// 2.1.310's Use @238504623 in Cne @247199785, `if(K>Use)` @247202183).
const claudeResumeLargeFile = 5 << 20

// The bytes Claude Code's large-file readers read a line by ([threadChain],
// THE LARGE FILE), as 2.1.322's ike and F1t and 2.1.310's HMt and FPt hold
// them.
var (
	// slotPrefix is how a line begins that every reader takes for a SLOT, a
	// record it reads by its bytes, and slotParentKey the key the dispatch
	// takes one by wherever its line carries it at its object's depth one.
	slotPrefix    = []byte(`{"parentUuid":`)
	slotParentKey = []byte(`"parentUuid":`)
	// slotUUIDKey is what a slot's uuid follows, and slotTimeAfter what
	// follows the uuid a reader keys the slot by first.
	slotUUIDKey   = []byte(`"uuid":"`)
	slotTimeAfter = []byte(`","timestamp":"`)
	// slotSidechain is what makes a slot a sidechain's to each reader, and
	// slotSidechainHead how a subagent's record says so right after its
	// parent.
	slotSidechain     = []byte(`"isSidechain":true`)
	slotSidechainHead = []byte(`,"isSidechain":true`)
)

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
// Rye @293845538, 2.1.310's FBe @247216758): what it hands on, the walk, the
// recovery and the tails from its own leaf, of the subagent's own id, which
// no retraction filter or drop of malformed attachments follows (A
// SUBAGENT'S OWN LOAD); every pass after it is its reader's.
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
// variants in all ([chainVariants]). Within this model they part in three
// places: the leaf, where q1t moves it (2.1.322 with foamy_spring and
// sorted_metcalfe on); the retraction filter, whose names each release reads
// off other records, both of them the main thread's; and the order of the
// tails, which warm_mochi turns from a walk by breadth sorted by time into a
// walk by depth (THE TAILS), in a subagent's thread as well, over the same
// records either way. The drop of malformed attachments, the same code in
// both releases, parts none. A record handed under some variants and not
// others is handedUnknown (whyLeafFlag, whyRelease); each distinct order is
// handed once, with the variants that hand it ([threadChain.EachOrder]).
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
//     out of the session's rows (ZI @293589374) and out of the walk's
//     fallback (`if(ZI(ne))continue` @293589741), and 2.1.310 out of
//     neither: whyFileSnapshot;
//   - a reply whose mark of an API error or model the layer cannot read, and,
//     in the main thread, an attachment whose payload the layer cannot judge
//     by the malformed-attachment drop's rules (whyAdmission); a timestamp in
//     no form Claude Code writes, since the resume compares times as strings
//     in one place and as dates in another, one order only on that form
//     (whyTimestamp);
//   - a retraction list, a compaction's metadata or a checkpoint's leaf in no
//     shape (whyRetractionShape, whyPreservedShape, whyCheckpointShape), and a
//     checkpoint a rewind, a withdrawal or a fork marked (whyMarkedCheckpoint),
//     which starts the resume where it names whatever came after;
//   - a content-replacement record whose list is none, which 2.1.310's load
//     spreads and fails at (2.1.310 @247196072, where 2.1.322 @293820138
//     passes it over: whyReplacementShape), or that lists more than
//     [threadChainMaxSpread] entries, which either may fail to spread
//     (whySpread);
//   - in a subagent's thread, a fork's reference (whyForkContext), any
//     agent's, though its load prefixes the parent's conversation by the
//     subagent's own alone (A SUBAGENT'S OWN LOAD).
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
// boundary (2.1.322's Kr @291612448) forgets the checkpoint, and the last
// boundary and the newest one that names what it kept are filed for the
// relink; a CHECKPOINT (last-prompt) names the leaf where its leafUuid says
// something (`if(leafUuid)`), the uuid as written, and any last-prompt that
// carries the key at all, null among it, turns off the newest-row rule below
// (`leafUuid!==void 0`).
//
// THE LARGE FILE, which End judges first (2.1.322's oee @293824518, 2.1.310's
// Cne @247199785). The load reads a thread of more than 5,242,880 bytes
// ([claudeResumeLargeFile]) with a large-file reader, which chooses by their
// bytes the lines it hands the load, where its small path hands it every line
// in turn. Which reader turns on CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP
// (@293824834, @247199946), which a settings file can set whatever the
// launcher unsets, so the chain models both. A subagent's own load reads its
// file with the same loader (Rye @293845538, FBe @247216758).
//
// Unset, the DISPATCH (2.1.322's ike @293792132 through TWo @293804963, or,
// where the resume asks for the side file resume-head.jsonl, which only the
// hosted environment reads (czt @293805243), through pzt @293806041 by way of
// RWo @293807189, the same dispatch; 2.1.310's HMt @247175414 through Fpo
// @247185752) takes for a SLOT every line that begins `{"parentUuid":`, or
// carries that key at its object's depth one outside every string (2.1.322's
// N1t @293787634, 2.1.310's Ipo @247171334), and holds a `"uuid":"` (kl
// @293794950, Eo @247177994). It keys a slot by the 36 bytes after its first
// `"uuid":"` they follow with `","timestamp":"`, of several such the first at
// depth one, else the last of them, and where none is, the first `"uuid":"`
// (pa @293793061 with dzt @293787942, vs @247176207 with $Mt @247171638);
// reads its parent as the 36 bytes after the key's opening quote, none where
// `null` follows the key, and any other value as one neither a string nor
// null; and reads it as a sidechain's where its bytes say
// `"isSidechain":true`: 2.1.322 the first `"isSidechain":` within the first
// 256 bytes of a line that begins `{"parentUuid":`, else the one at its depth
// one (@293795256); 2.1.310 those bytes anywhere within its first 256
// (@247178300). It starts from the last slot that is no sidechain's
// (@293799473, @247182021; 2.1.322 passing over, and walking as well, one
// whose parent is null that is no object or a file's snapshot), and walks up
// by parent from there and from the checkpoint (@293799773, @247182158). A
// walk that meets a parent naming no slot, one neither a string nor null, or
// a slot it met itself hands the load every line. Else the dispatch hands it,
// in the file's order, every line that is no slot, every walked slot, and
// each slot whose parent it handed before it as one that hands its children
// on (@293801822 with @293796186, @247183770 with @247179135): where X_e
// reads the slot as part of its parent's group, the slot hands its own
// children on in turn, a reply's group its own id and any other slot's its
// parent's, where that parent was handed so before it (tzt @293749089, OMt
// @247134040); where the parent is UF, the slot is handed and hands none on
// (nzt @293749143, IMt @247134094); and a user or assistant slot whose
// message is no object it never hands so (ozt @293749187, DMt @247134138), as
// the load's admission drops one.
//
// Set, the PRUNE (2.1.322's F1t @293788207, called @293830949; 2.1.310's FPt
// @247171899, called @247203013) reads the whole file and takes for a slot
// only a line that begins `{"parentUuid":` and holds a `"uuid":"`, keyed
// alike, its parent the 36 bytes after the key's opening quote and none for
// any other value. It starts from the last slot whose line holds
// `"isSidechain":true` nowhere (@293788960, @247172642; 2.1.322 passing over
// one whose parent is no string that is no object or a file's snapshot), and
// walks up from it alone, to a root, a parent naming no slot or a slot it met.
// It keeps the whole file where that walk holds more than half its bytes
// (@293789349, @247172956), and else every line that is no slot, every walked
// slot, and each slot whose parent it walked, or kept before it as one that
// keeps its children (@293790130, @247173737): one such too where X_e reads
// the slot as part of its group, for a reply the id of the nearest reply up
// by parent from its parent, and one kept alone where the parent is UF; a
// slot whose line is no object, or a user or assistant one whose message is
// none, never.
//
// So the chain judges, of every thread whatever its size, whether it is
// PATH-FREE ([threadChain.AnySize]): whether every one of those readers, of
// either release, hands the load what its small path does. It judges on the
// decode and on bytes held to it, conservatively, so only fewer threads are
// path-free than the readers allow. A thread is path-free where:
//
//   - no record is a compaction's boundary, at which the dispatch forgets the
//     slots before it and indexes those after it otherwise, and from which
//     the side file starts;
//   - no line is one the layer refuses ([threadChain.Unread]), which a reader
//     may take for a slot by its bytes;
//   - no line of no record carries `"parentUuid":` at its depth one, so no
//     such line is a slot;
//   - every record's line begins `{"parentUuid":`, then null or the quoted
//     lowercase canonical uuid of the parent its decode names, and the 36
//     bytes every reader keys it by are its own uuid's;
//   - in the main thread, the last record's line holds no
//     `"isSidechain":true`, so every reader starts from it (each reads those
//     bytes to choose its start alone, back from the last slot), and the
//     DISPATCH FROM IT ALONE reaches every record: the walk up from it, which a
//     parent naming no record ends too, then, in the file's order, each record
//     whose parent it reached before it as one that hands its children on, as
//     the dispatch hands them. Every reader hands at least that: the
//     checkpoint's walk, a parent naming no slot and the half rule hand more,
//     the prune keeps a slot under a walked parent written after it too, and
//     the prune's group, the nearest reply's up by parent, is the dispatch's
//     wherever the dispatch's is a reply's id, since both read up the same
//     parents, the dispatch's through parents met before their children alone;
//   - in a subagent's thread, every record's line says it is a sidechain's
//     right after its parent (`{"parentUuid":`, its parent, then
//     `,"isSidechain":true`), so no reader has a slot to start from, and
//     every one hands the load every line.
//
// Where all of those hold, every reader hands the load the lines its small
// path does, in the same order, but for the attribution snapshots, of which
// the dispatch applies the last alone, and the artifact reaction ledgers it
// reads apart (tye @293557899), no line of which this model reads; and where
// the small path has the load's processEntry make every string of every line
// well-formed (tIe @282906774, in Szt @293813830), the dispatch has it do so
// only on a line that writes a surrogate's escape (`\ud` or `\uD`), which
// changes no value this model reads. A thread past 5,242,880 bytes that is not
// path-free is whyLargeFile; one that is answers as its small path does. Its
// size is the bytes of the lines See and Unread were handed, each with its
// break: the file's, where the caller holds to THE BYTES ARE THE RESUME'S. A
// torn last piece is no line, which the chain neither counts nor reads, and
// which the box cuts from a main thread before its resume
// ([claudeSessionGate]): a file that keeps one is not the file the chain was
// handed. MEASURED by the design's ports of the four readers: of the 432 main
// threads the probes kept, the 410 that hold no boundary are reached whole by
// the dispatch from their last record alone under both groups, the 13 past the
// bound among them, and no record line of them holds `"isSidechain":true`;
// every record line of them and of the 89 subagent threads begins
// `{"parentUuid":`, and each of the subagent threads' 1,353 says it is a
// sidechain's right after its parent.
//
// THE RELINK, End's first step after that (2.1.322's iWo @293743389 with aWo
// @293744710, 2.1.310's ypo @247128053 with _po @247129357, the same code),
// where a compaction's boundary names what it kept ([threadPreserved.Named]),
// read off the newest such boundary; where none does, nothing happens. Where
// that boundary is the last of all, it KEEPS the records its list names, in
// the list's order, or, where it carries no list, those of its segment, walked
// from its tail up by parent to its head; where a listed uuid names no record,
// or that walk meets a root, a parent naming no record or a record it walked,
// nothing happens at all. A boundary that lists none, or is not the last,
// keeps none. The relink CHAINS the kept records under the boundary's anchor,
// the first's parent the anchor and each next one's the one before it;
// RE-HANGS from the last kept record every other record whose parent is the
// anchor; and DELETES every record written before the last boundary that it
// does not keep (whyRelinkDeleted). Where it kept records and deleted any, a
// user or assistant record whose parent it deleted is hung from the last kept
// record too, and an attachment or system record keeps that parent, which then
// names no record. A deleted record is in no later step, and every later step
// reads each record's parent as the relink left it. A relink that would chain
// its kept records into a loop, or under a record it does not keep, is refused
// whole (whyRelinkShape): an anchor naming no record or one the relink
// deletes, an anchor among the kept records or below one of them by parent,
// the last boundary among them, or a record listed twice, which the chain
// hangs under itself. Each of the 22 relinked threads the probes kept anchors
// its kept records at the compaction's summary, after its boundary.
//
// THE LEAF of the main thread (2.1.322's Gr @293820883 and Yya @293834568,
// 2.1.310's kr @247196754 and m_i @247206356, alike but for q1t), read off the
// thread as the relink left it; a subagent's own load chooses its own (A
// SUBAGENT'S OWN LOAD). Where the resume did not relink, THE SINGLE PATH:
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
//     @293811681): the checkpoint is a reply that made a call; the walk up
//     from the last row reaches the record above the checkpoint's run (the
//     checkpoint and the records of its id it hangs from), and of the records
//     it passes outside that run, one is a reply that says something or
//     calls (LE @293601245) and the last, the topmost, was written after the
//     checkpoint (`K.timestamp>h.timestamp`); no record hangs off the
//     checkpoint, and none off the last row but one that hangs off a reply,
//     UF;
//   - else the checkpoint, where it names a record the relink left; else the
//     start;
//   - the leaf's set is the nearest user or assistant record up from there by
//     parent, the walk ending at a root, a parent naming no record, or a
//     record it met.
//
// Where the resume relinked, or the single path finds no such record, THE
// LEAF SET (2.1.322's Cjt @293590395 called @293822490, 2.1.310's loop in kr
// @247197836): from each record no record names as its parent, in the file's
// order, the first user or assistant record up by parent through records that
// are neither, the record itself where it is one, and none where that walk
// meets a root, a parent naming no record or a loop; each such record that no
// user or assistant record names as its parent. Where it holds more than one,
// the resume narrows it (2.1.322 @293822608, 2.1.310 @247198349) to the
// nearest user or assistant record up from the checkpoint, where the
// checkpoint is in the set and the last row descends through no batch of the
// checkpoint's (Y1t as the single path asks it, but never by a plain descent:
// `direct:!1`) and, in the metcalfe variants, q1t does not hold; else up from
// the last row; and keeps the set where that walk finds no such record.
// (2.1.322 first takes the last row on a clause of the hosted environment's
// alone, NWo behind WA(), which never answers here.)
//
// THE LEAF is the member of the set of the latest Date.parse of its
// timestamp, the first in the file on a tie (2.1.322's cj @293745290 in Yya
// @293835409, 2.1.310's p0 @247129929 in m_i @247207161, `y>s`), a record of
// no time no member; where the set is empty, the record of the latest time of
// every one the relink left, of any kind, the first in the file on a tie
// (2.1.322's Ozt @293848378, 2.1.310's p0 in m_i); and where no such record
// has a time, the resume finds no conversation (whyNoConversation).
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
// parent is one of them, neither walked nor recovered before, is recovered
// (whyRecoveredReply, whyRecoveredResult); its window runs from the reply to
// past the last record of its id on the walk and on over every record X_e
// reads as part of it (2.1.322 @293748740, 2.1.310's Yce @247133691: a reply
// of its id, a meta user record or one of results, any attachment, a system
// record but a compaction's boundary). Where a record of the reply made a
// call (2.1.322's bVe @293748333, 2.1.310's xMt @247133284), the recovery then
// HANGS records off it (`co`, whyHookChain): under each record of the reply,
// then each result under one of them walked or recovered by now, then each
// walked record of the window past the reply's first that is UF (2.1.322
// @293588888, 2.1.310's l0 @246995738: an attachment, a system record but a
// compaction's boundary, a meta user record holding no result), it takes from
// each child in turn, in the file's order, what pye hands (2.1.322
// @293588633, 2.1.310's cce @246995483): the child and, after each, its one
// child not met yet, every one a UF record whose isSidechain, as written, is
// that of the record the chain hangs under (absent, false and true apart) and
// none met, or nothing where one is not or has two children not met; each
// record hung is met at once (`r.add(Bo.uuid)` @293752104, @247137051), so it
// blocks what a later window would hang. A window OPENS where it recovered or
// hung a record (`xn.length===0&&Tn.size===0` @293752359, @247137306 skips
// the rest), one of hung records alone among them. Each places its records
// among the walked ones of its window: what it recovered, and what it hung
// under a record of the reply or a result (`co(uo,xn)` @293752239,
// @247137186), by their place in the file (`xn.sort(_t)` @293752430,
// @247137377), each before the first walked record written after it, the
// rest at the window's end; what it hung under a walked record right after
// that record, in the order hung (`Ot.push(...an.get(so.uuid)??[])`
// @293753212, @247138159). Windows that overlap merge (@293752828,
// @247137775), their recovered records sorted together and the records each
// hung under one walked record joined, the earlier window's first
// (@293752913, @247137860), so a window of hung records alone moves where a
// window before it places its records. A result whose ids are not all of
// calls its parent made, where a reply other than its parent made one of
// them, or its sourceToolAssistantUUID names a record other than its parent,
// is one foamy_spring may hang elsewhere: whyResultByCall, the whole thread.
// And where a window exists and the walk is longer than
// [threadChainMaxSpread], or the records hung under one walked record are,
// whySpread: the recovery spreads stretches of the walk, and each walked
// record's hung records, into one call's arguments. MEASURED by the design's
// census of the 432 main threads the probes kept: 13 records of a reply and
// 66 results recovered beside it, and 104 records hung under a reply's
// records or results (a prompt's snapshot, a token reminder), none under a
// walked record; none recovered by a call's id.
//
// THE TAILS (2.1.322's lWo @293747057 with XGt @293746868, 2.1.310's kpo
// @247131650 with CMt @247131461, the same code, run after the recovery with
// what the walk and the recovery met, `lWo(n,w,h,r??XGt(e))` @293745892,
// @247130444): every record under the leaf that is neither a user nor an
// assistant record (`r.type!=="user"&&r.type!=="assistant"` @293746939,
// @247131532), a compaction's boundary among them and whatever its side,
// through records that are neither, a record met already ending its branch,
// is handed after all the walk and the recovery hand (whyLeafTail). Their
// order turns on warm_mochi. With it, a walk by depth from the leaf
// (@293747195, @247131788): each record's children sorted by time, the
// earliest taken first (`K.length>1?[...K].sort(S):K` @293747281,
// @247131874). Without it, a walk by breadth (@293747466, @247132054) whose
// records are then sorted by time together (@293747532, @247132120): the
// same records, in another order only where the tails run two deep and their
// times interleave. Each sort is stable, of the timestamp compared as a
// string (`H.timestamp<K.timestamp` @293747102, `D.timestamp<K.timestamp`
// @247131695), which reads a record of no time as equal to every record, so
// a sort holding one beside two records of different times orders none of
// them, and its engine's sort algorithm does (whyTailTime, the whole
// thread); one beside records of one time leaves them all as they came. The
// walk by depth sorts each record's every child, met or not. And the walk by
// breadth spreads what it found into one call's arguments (`n.push(...h)`
// @293747616, @247132204): more records than [threadChainMaxSpread] are
// whySpread. MEASURED by the design's census of the 432 main threads the
// probes kept: 78 and 74 tails under the leaf, 2.1.310's and 2.1.322's (a
// Stop hook's records, those the compact command leaves, a prompt's
// snapshot), none with two children, both orders one on every one of them;
// and no record of the probes' 397 thread files lacks a timestamp.
//
// THE RETRACTION FILTER (2.1.322's eGe @295286851, 2.1.310's qGn
// @248511058), main mode alone, over each variant's list: every record but a
// system one whose uuid's first 24 characters a record of the list names in
// its retractedMessageUuids is dropped (whyRetracted); 2.1.322 reads the names
// off user, assistant and model_refusal_fallback records, 2.1.310 off the
// last alone, so a record only one release drops is whyRelease. An entry of
// fewer than 24 characters retracts nothing. The list is every record the
// walk, the recovery and the tails hand, those hung and the tails among them.
//
// THE MALFORMED-ATTACHMENT DROP (2.1.322's Fce @295256716 with SCr
// @295255626, 2.1.310's dne @248487226 with cJo @248486136, the same code and
// bounds), main mode alone, over each variant's list after the filter, and so
// alike in every variant: every attachment of the list whose payload is not
// of its type's shape, by the rules [threadLine.AttachmentWellFormed] reads
// (2.1.322's `s.type==="attachment"&&!SCr(s.attachment)` @295256760,
// 2.1.310's with cJo @248487270), is dropped (whyMalformedAttachment), one the
// recovery hung or a tail as well as one the walk took; a record the filter
// dropped keeps the filter's reason. It drops that record alone: every other
// stays where the walk, the recovery and the tails placed it, those that hang
// under it among them. Each release's deserializer runs the filter and the
// drop again (e_n's `Fce(Ve)` @295266070, TJt's `dne(be)` @248494866), which
// then take nothing more. MEASURED by the layer's census of the probes' 521
// thread files: the drop takes none of their 7,519 attachments.
//
// A SUBAGENT'S OWN LOAD (2.1.322's Rye @293845538, 2.1.310's FBe
// @247216758), the thread of newThreadChain(agent). It loads the subagent's
// file with the main thread's own loader (`oee(h,S)` @293845856, `Cne(h,S)`
// @247217076), so THE LINES THAT ARE NO RECORD, THE LOAD and THE RELINK hold
// of it as written above, the relink being the loader's last step (`let
// Dr=iWo(r,n)` @293820833, `let vs=ypo(n)` @247196708), and THE LARGE FILE by
// its rule for a subagent's thread. The loader then picks a leaf from the
// session's rows as the main thread's resume does, and the subagent's load
// reads none of that pick; so a fork's briefing, which the loader passes over
// only in choosing those rows (`!Ho.isSidechain&&!jWo(Ho)` @293815308,
// `!Eo.isSidechain&&!Ypo(Eo)` @247192112), is an attachment like any other
// there. The subagent's load chooses its own leaf:
//
//   - its OWN records are those of the subagent's agentId whose isSidechain is
//     true (`tt.agentId===e&&tt.isSidechain` @293846307, @247217526); where
//     there is none, it finds no conversation (`no_agent_messages`
//     @293846356, @247217574);
//   - its OWN LEAVES ([threadChain.OwnLeaves]) are the own records no own
//     record names as its parent, each as the relink left it, a compaction's
//     boundary aside (@293846434 and @293846471, @247217652 and @247217688):
//     a record of another agent, or one that is no sidechain's, naming a
//     record makes it no parent;
//   - its LEAF is, of its own leaves that have a time, the one of the latest
//     Date.parse of it, the first in the file on a tie (2.1.322's cj
//     @293745290, 2.1.310's p0 @247129929, `y>s` from -Infinity); where none
//     has one, it finds no conversation (`no_leaf` @293846585, @247217801).
//
// From that leaf it makes THE WALK, THE RECOVERY and THE TAILS as the main
// thread's resume makes them (`Ce=A7t(H,Ee)` @293846609, `w$t(D,be)`
// @247217828), over every record of the file the relink left, whatever its
// agent or side, and hands on the records of the subagent's agentId alone,
// whatever their side (`.filter((tt)=>tt.agentId===e)` @293846627,
// @247217837): every other record the walk, the recovery or the tails reach
// is not handed (whyAnotherAgent). No retraction filter and no drop of
// malformed attachments runs on that load: the continuation runs the drop
// only after a trim of its own that turns on its own state (2.1.322's
// `Fce(rr)` @302266975 after `[...L5t(G.messages)]` @302266907, 2.1.310's
// `dne(bn)` @268540138 after `[...wMt(I.messages)]` @268540070), which, as
// every pass after the load, is its reader's. Where a fork's reference names
// the subagent, its load hands the parent's conversation before its own
// (`He=Y.get(e)` @293846722, `xe=V.get(e)` @247217932); the chain refuses a
// fork's reference in a subagent's thread whichever agent it names
// (whyForkContext). MEASURED by the design's census of the 89 subagent
// threads the probes kept: each hands one list under both releases and every
// flag, and its leaf is the last of its own leaves in the file; the 74
// 2.1.310 threads hang records under parents written after them, so their
// handed order is not the file's; 22 hold a second own leaf, 21 of them
// handed (19 results recovered beside their reply, 2 attachments) and one a
// reply its run dropped; and the walk takes its fallback in 2, past a line a
// torn write left.
//
// THE BOUNDS: [threadChainMaxRecords] records, retraction prefixes and uuids
// the newest boundary that names what it kept lists, and
// [threadChainMaxCalls] call-id occurrences, 2^17 each. A thread at the
// plaintext's bound, 256 MiB of lines at the 2,053 bytes a line probe P6
// measured, is 130,754 lines, so 2^17 files a record a line of it; a call is
// at least two records, each one block, so its blocks are at most about its
// lines too. Two bounds rather than one sum, since a thread of ordinary calls
// fills each about halfway. Past either, See files nothing more, the thread
// is whyBound and the chain drops what it filed, and See still decodes and
// hands back each user and assistant message, with pastChainBound.
//
// MEASURED on two cores, one case a process (BenchmarkAThreadChainAtItsBounds;
// the heap off the runtime's GC trace, what is held sampled at every tenth of
// its growth):
//
//   - at both bounds, 2^17 replies each of its own id making a call of its
//     own and naming a parent written after it: read in 1.3 s, End's 32 ms of
//     it; the chain holds 35 MiB as End starts, End allocates 12 MiB more, and
//     18 MiB stay once it decided: 43 MiB held at most;
//   - a node's collection's read of a thread at the plaintext's bound,
//     130,242 lines of a call and its result in turn: 2.1 s, End's 29 ms,
//     33 MiB held at most;
//   - END'S WORST, a walk of [threadChainMaxSpread] records whose 10,922
//     replies' windows each open before the one before ends: 1.4 s, nearly
//     all of it the recovery's rescan of each window's walked records, which
//     the resume makes as well; End took every other case at a bound in
//     32 ms or less;
//   - THE COINCIDENT WORST, the reading's worst line (one at [threadLineMax]
//     of the smallest results a message holds) decoded by See beside the
//     chain at both bounds, before it passes them: 101 MiB held at most. By
//     sum, exact telling's reading measured 88 MiB for that line, its own
//     decode of it among them, which beside the 35 MiB the chain holds while
//     it files is 123 MiB; End's 43 MiB comes after the last line, beside
//     only what a reading keeps of the thread. Each is within the about
//     133 MiB a seat's read for words holds at its worst in the one slot
//     both take ([SeatTranscriptReadsAtOnce]), so both bounds stay at 2^17.
//     The heap peaks over what is held by the collector's headroom: at its
//     default, 157 MiB for that line and 63 MiB at both bounds.
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
	// smallOnly is whether a line was met that a large-file reader may read
	// otherwise than the small path (THE LARGE FILE), and lastMarked whether
	// the last record filed holds a sidechain's bytes, which in the main
	// thread move where every reader starts; anySize is what End decided of
	// the thread: path-free.
	smallOnly, lastMarked, anySize bool
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
	// the newest's time. Every record of a main thread the chain models is a
	// row, so a thread of any record has a last row, which no relink deletes:
	// it is written at or after the last boundary, a row itself.
	lastRow, newest chainHandle
	newestMS        int64
	// lastBoundary is the last compaction's boundary, and kept what the
	// newest one that names what it kept says, for the relink.
	lastBoundary chainHandle
	kept         chainKept
	// scratch collects one record's retraction prefixes, and keptScratch one
	// boundary's listed uuids.
	scratch     [][chainPrefixBytes]byte
	keptScratch [][16]byte

	// What End decided: each record's verdict, by handle - 1, the distinct
	// orders, the leaf every variant walks from, and, in a subagent's thread,
	// its own leaves.
	verdicts  []chainVerdict
	orders    []chainOrder
	leaf      chainHandle
	ownLeaves []chainHandle
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
	// kindIllFormed is, in the main thread alone, an attachment the
	// malformed-attachment drop takes: no such drop follows a subagent's own
	// load, so the chain neither marks nor judges one there.
	kindIllFormed
	// kindNowhere is a record whose parent names no record.
	kindNowhere
	// kindDeleted is a record the relink deleted, in no step after it.
	kindDeleted
	// kindAgent is, in a subagent's thread, a record of that subagent's
	// agentId; with kindSideTrue, one of its own.
	kindAgent
)

// chainRecord is what the chain files of one record.
type chainRecord struct {
	// uuid is the record's own uuid, as the 16 bytes of its lowercase
	// canonical form ([uuidBytes]). Not CLAUDE.md's uuid.UUID, though it is as
	// wide: the load keys its map by the uuid as written, so two strings
	// uuid.Parse reads as one UUID are two records to it, and these bytes
	// stand for the one canonical string alone, in 16 bytes rather than its
	// 36.
	uuid [16]byte
	// ms is its timestamp's milliseconds since the Unix epoch, where kindTimed
	// ([threadLine.Timestamp]). Not CLAUDE.md's time.Time: the resume orders
	// records by the timestamp string and by Date.parse's milliseconds, which
	// on the one form the chain admits are one order and exact to the
	// millisecond, so a time.Time would hold nothing more, in three times the
	// bytes a record.
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

// chainWritten is a uuid as a record writes it, a record's parent or one a
// boundary names, until End resolves it: its 16 bytes, named where it is a
// lowercase canonical uuid, the one form a record's own uuid takes, so
// another string names no record.
type chainWritten struct {
	uuid  [16]byte
	named bool
}

// writtenUUID is s, a uuid as a record writes it.
func writtenUUID(s string) chainWritten {
	u, named := uuidBytes(s)
	return chainWritten{uuid: u, named: named}
}

// chainKept is what a compaction's boundary that names what it kept says
// ([threadLine.Preserved]), as See files it for End's relink.
type chainKept struct {
	// boundary is the boundary, noChainRecord where none named what it kept.
	boundary chainHandle
	// anchor is the record the kept records hang from. segment is whether the
	// boundary names them by a segment, from tail up by parent to head; else
	// it lists them: list is the canonical uuids it lists, in order, listed
	// how many entries it lists, and nowhere whether one of them is no
	// canonical uuid, which names no record.
	anchor, head, tail chainWritten
	segment, nowhere   bool
	list               [][16]byte
	listed             int
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
	default:
		c.another(line)
		return noChainRecord, m, mErr
	}
	if c.past {
		return pastChainBound, m, mErr
	}
	return c.file(line, m, mErr), m, mErr
}

// another files line, a line the load keeps no record of in its map: what a
// progress record, a checkpoint, a fork's reference and a content
// replacement say, and whether a large-file reader may take the line for a
// slot all the same (THE LARGE FILE).
func (c *threadChain) another(line threadLine) {
	if slotKeyed(line.Raw) {
		c.smallOnly = true
	}
	switch line.Head.Type {
	case recordProgress:
		c.cannotTell(whyProgress)
	case recordLastPrompt:
		if !c.past {
			c.checkpointOf(line)
		}
	case recordForkContextRef:
		if c.agent != "" {
			c.cannotTell(whyForkContext)
		}
	case recordContentReplacement:
		c.replacementsOf(line)
	}
}

// Unread files a whole line [threadReader.Next] answered as no record: its N
// is held as See holds line.N, and its Size counted with the lines' bytes. A
// line that yields the load no value is skipped; any other is whyLine
// ([threadChain], THE LINES THAT ARE NO RECORD). Either way the thread is not
// path-free, since a large-file reader may take the line for a slot by its
// bytes (THE LARGE FILE). Unread panics after End.
func (c *threadChain) Unread(fault *threadLineError) {
	c.filing("Unread")
	c.smallOnly = true
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

// errChainFull ends a walk of a record's list, its retractions or the uuids
// its boundary kept, that would pass the chain's bound.
var errChainFull = errors.New("executor: a record's list runs past the chain's bound")

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
	if c.agent != "" && head.AgentID == c.agent {
		r.kind |= kindAgent
	}
	snapshot := head.Type == recordSystem && head.Subtype == subtypeFileSnapshot
	if snapshot {
		c.cannotTell(whyFileSnapshot)
	}
	// A fork's briefing is no row; only the main thread's leaf reads rows
	// (THE LOAD, A SUBAGENT'S OWN LOAD).
	attachment, typeErr := line.AttachmentType()
	briefing := typeErr == nil && attachment == attachmentForkBriefing
	if briefing && c.agent == "" {
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
	}
	if head.Type == recordSystem && head.Subtype == subtypeRefusalFallback {
		r.kind |= kindFallback
	}
	// The malformed-attachment drop follows the main thread's load alone (THE
	// MALFORMED-ATTACHMENT DROP, A SUBAGENT'S OWN LOAD): judge reads this mark.
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
	var (
		preserved   chainKept
		named, fits bool
	)
	if r.kind&kindBoundary != 0 {
		// A boundary carries no retraction list ([threadLine.Retracted]): its
		// own room is held to the bound beside the list it names, which
		// replaces the one before it.
		c.scratch = c.scratch[:0]
		preserved, named, fits = c.keptOf(line)
	} else {
		fits = c.retractionsOf(line)
	}
	if !fits || len(c.occurs)+occurrences > threadChainMaxCalls {
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
	if r.kind&kindBoundary != 0 {
		c.lastBoundary = h
	}
	if named {
		// The newest boundary that names what it kept replaces the one before
		// it, whose list leaves the chain's count with it (2.1.322's iWo reads
		// the last such boundary alone, `r=Ce`).
		c.filed += preserved.listed - c.kept.listed
		preserved.boundary, preserved.list, c.keptScratch = h, c.keptScratch, c.kept.list[:0]
		c.kept = preserved
	}
	if src, ok := line.SourceToolAssistantUUID(); ok && r.kind&kindResult != 0 {
		c.sources[h] = src
	}
	c.slotFacts(line, id, written, r.kind)
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

// slotFacts files what the large-file readers make of line by its bytes, the
// record of the map whose own uuid is id, whose parent is written as its
// decode reads it and whose kind is kind ([threadChain], THE LARGE FILE): a
// reader may read it otherwise than its decode where it is a compaction's
// boundary, where it does not begin `{"parentUuid":` and then the parent its
// decode names, written null or as the quoted canonical uuid alone
// ([slotHead]), where the readers key it by another uuid than its own
// ([keyedBy]), and, in a subagent's thread, where it does not say it is a
// sidechain's right after its parent. In the main thread it files too
// whether the record holds a sidechain's bytes anywhere, which move every
// reader's start where it is the last record.
func (c *threadChain) slotFacts(line threadLine, id string, written chainWritten, kind chainKind) {
	raw := line.Raw
	end := slotHead(raw, line.Head.Parent, written.named)
	switch {
	case end < 0, kind&kindBoundary != 0, !keyedBy(raw, id):
		c.smallOnly = true
	case c.agent != "" && !bytes.HasPrefix(raw[end:], slotSidechainHead):
		c.smallOnly = true
	}
	if c.agent == "" {
		c.lastMarked = bytes.Contains(raw, slotSidechain)
	}
}

// slotHead is where, in raw, a record's line, the parent every large-file
// reader reads off it ends, where that is the parent its decode reads: raw
// begins `{"parentUuid":` and then parent, the value as written, which is null
// or, named, a lowercase canonical uuid quoted with no escape. Those two alone
// read alike to every reader, which reads a parent as the 36 bytes after the
// key's opening quote or as null (2.1.322's kl in ike @293794950, 2.1.310's
// Eo in HMt @247177994, and F1t's and FPt's keying). -1 for any other line.
func slotHead(raw, parent []byte, named bool) int {
	if !bytes.Equal(parent, jsonNull) && (!named || len(parent) != 36+2) {
		return -1
	}
	if !bytes.HasPrefix(raw, slotPrefix) || !bytes.HasPrefix(raw[len(slotPrefix):], parent) {
		return -1
	}
	return len(slotPrefix) + len(parent)
}

// keyedBy reports whether the uuid every large-file reader keys raw, a
// record's line, by is id, the record's own: the 36 bytes after the first
// `"uuid":"` they follow with `","timestamp":"`, of several such the first at
// the line's object depth one ([depthOneOf]), else the last of them, and where
// none is, after the first `"uuid":"` (2.1.322's pa in ike @293793061, keyed
// alike in F1t @293788207; 2.1.310's vs in HMt @247176207, alike in FPt
// @247171899). A reader reads those bytes as latin1 and compares them as
// written, so only the same bytes are the same uuid.
func keyedBy(raw []byte, id string) bool {
	first, good := -1, -1
	var more []int
	for from := 0; ; {
		i := bytes.Index(raw[from:], slotUUIDKey)
		if i < 0 {
			break
		}
		i += from
		if first < 0 {
			first = i
		}
		if after := i + len(slotUUIDKey) + 36; after+len(slotTimeAfter) <= len(raw) &&
			bytes.Equal(raw[after:after+len(slotTimeAfter)], slotTimeAfter) {
			if good < 0 {
				good = i
			} else {
				more = append(more, i)
			}
		}
		from = i + len(slotUUIDKey)
	}
	at := first
	switch {
	case more != nil:
		at = depthOneOf(raw, append([]int{good}, more...))
	case good >= 0:
		at = good
	}
	if at < 0 || at+len(slotUUIDKey)+36 > len(raw) {
		return false
	}
	at += len(slotUUIDKey)
	return string(raw[at:at+36]) == id
}

// slotKeyed reports whether raw, a line the load keeps no record of, is one a
// large-file dispatch may take for a slot all the same: it carries
// `"parentUuid":` at its object's depth one, outside every string (2.1.322's
// N1t @293787634, 2.1.310's Ipo @247171334), as a line that begins
// `{"parentUuid":`, the prune's slot, does too.
func slotKeyed(raw []byte) bool {
	if !bytes.Contains(raw, slotParentKey) {
		return false
	}
	var s jsonDepth
	for i, b := range raw {
		if s.one() && b == '"' && bytes.HasPrefix(raw[i:], slotParentKey) {
			return true
		}
		s.step(b)
	}
	return false
}

// depthOneOf is the first of at, offsets into raw in their order, that lies
// at the line's object depth one outside every string, else the last of them
// (2.1.322's dzt @293787942, 2.1.310's $Mt @247171638).
func depthOneOf(raw []byte, at []int) int {
	var s jsonDepth
	for i, k := 0, 0; k < len(at); i++ {
		if i == at[k] {
			if s.one() {
				return at[k]
			}
			k++
		}
		s.step(raw[i])
	}
	return at[len(at)-1]
}

// jsonDepth is the scan both large-file readers make of a line's bytes where
// they look for a key at its object's depth one: how many objects deep it
// stands outside every string, a string ending at its next quote that no
// backslash escapes (2.1.322's N1t and dzt, 2.1.310's Ipo and $Mt).
type jsonDepth struct {
	depth            int
	inString, escape bool
}

// step reads b, the line's next byte.
func (s *jsonDepth) step(b byte) {
	switch {
	case s.escape:
		s.escape = false
	case s.inString:
		switch b {
		case '\\':
			s.escape = true
		case '"':
			s.inString = false
		}
	case b == '"':
		s.inString = true
	case b == '{':
		s.depth++
	case b == '}':
		s.depth--
	}
}

// one reports whether the scan stands at depth one, outside every string.
func (s *jsonDepth) one() bool { return s.depth == 1 && !s.inString }

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

// keptOf reads what line, a compaction's boundary, says it kept
// ([threadLine.Preserved]), the canonical uuids it lists into keptScratch,
// and reports whether it names what it kept at all, and whether the boundary
// fits threadChainMaxRecords beside what the chain filed: with its list in
// place of the list of the boundary it replaces, where it names what it kept,
// else alone. Every entry counts, a canonical uuid or not; metadata in no
// shape is whyPreservedShape.
func (c *threadChain) keptOf(line threadLine) (k chainKept, named, fits bool) {
	c.keptScratch = c.keptScratch[:0]
	// room is how many entries the list may hold beside the boundary itself.
	room := threadChainMaxRecords - c.filed + c.kept.listed - 1
	p, err := line.Preserved(func(entry string) error {
		if k.listed >= room {
			return errChainFull
		}
		k.listed++
		if u, ok := uuidBytes(entry); ok {
			c.keptScratch = append(c.keptScratch, u)
		} else {
			k.nowhere = true
		}
		return nil
	})
	switch {
	case errors.Is(err, errChainFull):
		return chainKept{}, false, false
	case err != nil:
		c.cannotTell(whyPreservedShape)
		return chainKept{}, false, c.filed < threadChainMaxRecords
	case !p.Named:
		return chainKept{}, false, c.filed < threadChainMaxRecords
	}
	k.anchor, k.head, k.tail, k.segment = writtenUUID(p.Anchor), writtenUUID(p.Head), writtenUUID(p.Tail), p.Segment
	// Each entry was held to the room as it was read, so no list grows past it
	// in memory before it is refused; a list of none still needs the room the
	// boundary itself takes.
	return k, true, room >= 0
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
	c.occurs, c.prefixes, c.sources, c.scratch, c.kept, c.keptScratch = nil, nil, nil, nil, chainKept{}, nil
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
	c.kept, c.keptScratch = chainKept{}, nil
	if err != nil {
		c.verdicts, c.orders, c.leaf, c.ownLeaves = nil, nil, noChainRecord, nil
		return err
	}
	c.state = chainDecided
	return nil
}

// Unknown is the thread's own cannot tell: the first shape no model holds
// that it met, and true; "" and false where the thread is modelled. Asked
// while the chain files, it answers the first shape met so far, so a reader
// can stop reading a thread the model already cannot place. After an End its
// context cut short, it answers that shape where the thread met one before
// End stopped, and whyUndecided where it met none.
func (c *threadChain) Unknown() (chainWhy, bool) {
	switch {
	case c.why != "":
		return c.why, true
	case c.state == chainAbandoned:
		return whyUndecided, true
	}
	return "", false
}

// Handed is what the resume makes of record h; Why says why. It answers in
// the order it checks: noChainRecord, whenever it is asked, handedNo
// (whyNoRecord); every other handle, asked before End decided or after an End
// its context cut short, handedUnknown (whyUndecided); then pastChainBound
// handedUnknown (whyBound), a handle the chain never gave out handedNo
// (whyNoRecord), every record of a thread the model cannot place
// handedUnknown, with the thread's reason, and every other record what End
// decided of it.
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
// every variant walks from, in a subagent's thread its own load's leaf. False
// where q1t's variants walk from another, and where no record was walked.
func (c *threadChain) Leaf() (chainHandle, bool) {
	if c.state != chainDecided || c.why != "" || c.leaf == noChainRecord {
		return noChainRecord, false
	}
	return c.leaf, true
}

// OwnLeaves is, in a subagent's thread, every record of the subagent's own
// that no such record names as its parent, a compaction's boundary aside, in
// the file's order: the records its own load chose its leaf among
// ([threadChain], A SUBAGENT'S OWN LOAD), the leaf one of them where it found
// one. The chain's own: a reader reads it and never writes it. Nil in the main
// thread, before End decided, after an End its context cut short, and where
// the thread is cannot tell.
func (c *threadChain) OwnLeaves() []chainHandle {
	if c.state != chainDecided || c.why != "" {
		return nil
	}
	return c.ownLeaves[:len(c.ownLeaves):len(c.ownLeaves)]
}

// AnySize reports whether the thread is PATH-FREE ([threadChain], THE LARGE
// FILE): every large-file reader of both releases hands the load what its
// small path does, so what the chain answers of it holds whichever side of
// 5,242,880 bytes a copy of these bytes lies on, and holds for a copy that
// differs from them only inside string values, since what a string holds is
// no key and no object to any of those readers' tests; never for a copy whose
// keys were written in another order. A reader that reads another copy than
// the one the resume reads (the box's own, where a relaunch restores the
// take's archive over it) relies on the answers only where this is true.
// False before End decided, after an End its context cut short, and where
// the thread is cannot tell.
func (c *threadChain) AnySize() bool {
	return c.state == chainDecided && c.why == "" && c.anySize
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
	// order: record h's are kids[childAt[h]:childAt[h+1]]. groupAt and group
	// are each reply's records, in the file's order: reply r's are
	// group[groupAt[r]:groupAt[r+1]]. Each of the records the relink left, by
	// the parent it left each.
	childAt []int32
	kids    []chainHandle
	groupAt []int32
	group   []chainHandle
	// cp is the record the checkpoint names, where it names one the relink
	// left.
	cp chainHandle
	// a and b are sets of records each step clears and fills, run y1t's run
	// of a batch's reply and the leaf set's members; met is the walk's and the
	// recovery's; replied the replies the recovery met.
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
// and one in 4,096 steps is nothing End could measure. MEASURED at the
// chain's bounds (BenchmarkAThreadChainAtItsBounds), End never ran 13 ms
// without a look, its longest stretches each a pass that counts no step,
// one pass over the records or one sort of them (resolving every parent,
// sorting the fallback's candidates), and through its worst case, 1.4 s of
// steps, it looked at least every 11 ms; so the count stands.
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

// work resolves every record's parent and the checkpoint by uuid.
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
	return w
}

// covered reports whether the dispatch from the last record alone reaches
// every record of a main thread ([threadChain], THE LARGE FILE; 2.1.322's ike
// @293792132 and 2.1.310's HMt @247175414, with no checkpoint's walk): the
// walk up from it by parent, which a root, a parent naming no record and a
// record it met end, as the prune's walk ends; then, in the file's order, each
// record whose parent it reached before it as one that hands its children on,
// where X_e reads the record as part of that parent's group (a reply's own id,
// else its parent's where its parent came before it), when it hands its own on
// in turn, or where the parent is UF, when it hands none on. Read off each
// record's parent as written: no relink moves one, since a thread holding a
// boundary is no path-free one. A user or assistant record whose message is no
// object, which neither reader hands off its walk (2.1.322's ozt @293749187,
// 2.1.310's DMt @247134138), is in no thread the chain models (whyAdmission).
//
// THE DISPATCH'S GROUP ALONE IS READ, not the prune's beside it: the prune's
// is the reply of the nearest reply up by parent, which is the dispatch's
// wherever the dispatch's is a reply's at all, since the dispatch reads up the
// same parents, through parents met before their children alone; so a record
// X_e reads as part of the dispatch's group, it reads as part of the prune's.
func (c *threadChain) covered(ctx context.Context, w *chainWork) (bool, error) {
	n := len(c.recs)
	// w.a holds the walk.
	w.a.clear()
	for at := chainHandle(n); at != noChainRecord && !w.a.has(int(at)); at = c.recs[at-1].parent {
		if err := w.tick(ctx); err != nil {
			return false, err
		}
		w.a.set(int(at))
	}
	// w.b holds the records reached that hand their children on, by now only
	// those before the record at hand, and groups each one's group.
	w.b.clear()
	groups := make([]chainReplyID, n+1)
	for i, r := range c.recs {
		if err := w.tick(ctx); err != nil {
			return false, err
		}
		h, p := chainHandle(i+1), r.parent
		var from chainReplyID
		if p != noChainRecord && w.b.has(int(p)) {
			from = groups[p]
		}
		switch {
		case w.a.has(int(h)):
		case p == noChainRecord || !w.b.has(int(p)):
			return false, nil
		case c.partOf(h, from):
		case c.uf(p):
			continue
		default:
			return false, nil
		}
		w.b.set(int(h))
		groups[h] = from
		if r.kind&kindAssistant != 0 {
			groups[h] = r.reply
		}
	}
	return true, nil
}

// handleOf is the record u names, where it names one.
func (c *threadChain) handleOf(u chainWritten) chainHandle {
	if !u.named {
		return noChainRecord
	}
	return c.index[u.uuid]
}

// setParent makes p record h's parent, a record of the thread.
func (c *threadChain) setParent(h, p chainHandle) {
	c.recs[h-1].parent = p
	c.recs[h-1].kind &^= kindNowhere
}

// relink is the resume's relink ([threadChain], THE RELINK; 2.1.322's iWo
// @293743389 with aWo @293744710, 2.1.310's ypo @247128053 with _po
// @247129357), made in place on each record's parent, the records it deletes
// marked kindDeleted, and whether it relinked: chained records it kept, which
// turns the leaf's pick from its single path. Its steps, 2.1.322's then
// 2.1.310's: the newest boundary naming what it kept read only where it is the
// last (`w=s===h` @293743594, @247128256); nothing done where a listed uuid
// names no record (@293743702, @247128362); the chain (@293744040,
// @247128693) and the anchor's other children re-hung (@293744084,
// @247128737); every record before the last boundary not kept deleted
// (`Ce<h` @293744443, @247129095); and a user or assistant record whose parent
// it deleted re-hung (@293744618, @247129265). A relink it cannot follow is
// whyRelinkShape.
func (c *threadChain) relink(ctx context.Context, w *chainWork) (bool, error) {
	if c.kept.boundary == noChainRecord {
		return false, nil
	}
	last := c.lastBoundary
	var kept []chainHandle
	if c.kept.boundary == last {
		var resolved bool
		if kept, resolved = c.keptRecords(w); !resolved {
			return false, nil
		}
	}
	// w.a holds the kept records.
	w.a.clear()
	for _, h := range kept {
		if w.a.has(int(h)) {
			c.cannotTell(whyRelinkShape)
			return false, nil
		}
		w.a.set(int(h))
	}
	var anchor, tail chainHandle
	if len(kept) > 0 {
		anchor, tail = c.handleOf(c.kept.anchor), kept[len(kept)-1]
		// An anchor written before the last boundary that the relink does not
		// keep is one it deletes, and one naming no record (noChainRecord) is
		// written before any: either leaves the first kept record under none,
		// or under the last of them once the deletion hangs it there. And the
		// last boundary among the kept records is chained under its own
		// anchor.
		if anchor < last || w.a.has(int(last)) {
			c.cannotTell(whyRelinkShape)
			return false, nil
		}
		at := anchor
		for _, h := range kept {
			c.setParent(h, at)
			at = h
		}
		for i := range c.recs {
			if h := chainHandle(i + 1); h != kept[0] && c.recs[i].parent == anchor {
				c.setParent(h, tail)
			}
		}
	}
	deleted := false
	for h := chainHandle(1); h < last; h++ {
		if err := w.tick(ctx); err != nil {
			return false, err
		}
		if !w.a.has(int(h)) {
			c.recs[h-1].kind |= kindDeleted
			deleted = true
		}
	}
	if deleted {
		for i := range c.recs {
			h, p := chainHandle(i+1), c.recs[i].parent
			if c.is(h, kindDeleted) || p == noChainRecord || !c.is(p, kindDeleted) {
				continue
			}
			if len(kept) > 0 && c.turn(h) {
				c.setParent(h, tail)
				continue
			}
			c.recs[i].parent = noChainRecord
			c.recs[i].kind |= kindNowhere
		}
	}
	if len(kept) == 0 {
		return false, nil
	}
	// An anchor among the kept records, or below one of them by parent as the
	// relink left each, hangs the chain under itself: the walk up from the
	// anchor meets a kept record.
	w.b.clear()
	for at := anchor; at != noChainRecord && !w.b.has(int(at)); at = c.recs[at-1].parent {
		if w.a.has(int(at)) {
			c.cannotTell(whyRelinkShape)
			return false, nil
		}
		w.b.set(int(at))
	}
	return true, nil
}

// keptRecords is the records the newest boundary that names what it kept
// keeps, where it is the last boundary: those its list names, in order, or
// where it carries no list, its segment's, walked from its tail up by parent
// to its head (2.1.322's aWo, 2.1.310's _po); and false where nothing happens:
// a listed uuid naming no record of the thread, or a walk that meets a root, a
// parent naming no record or a record it walked.
func (c *threadChain) keptRecords(w *chainWork) ([]chainHandle, bool) {
	k := c.kept
	if !k.segment {
		if k.nowhere {
			return nil, false
		}
		kept := make([]chainHandle, 0, len(k.list))
		for _, u := range k.list {
			h := c.index[u]
			if h == noChainRecord {
				return nil, false
			}
			kept = append(kept, h)
		}
		return kept, true
	}
	head := c.handleOf(k.head)
	var kept []chainHandle
	w.b.clear()
	for at := c.handleOf(k.tail); at != noChainRecord && !w.b.has(int(at)); at = c.recs[at-1].parent {
		w.b.set(int(at))
		kept = append(kept, at)
		if at == head {
			slices.Reverse(kept)
			return kept, true
		}
	}
	return nil, false
}

// indexRecords indexes the records the relink left by parent, as it left
// each, and by reply.
func (c *threadChain) indexRecords(w *chainWork) {
	n := len(c.recs)
	if w.cp != noChainRecord && c.is(w.cp, kindDeleted) {
		w.cp = noChainRecord
	}
	w.childAt = make([]int32, n+2)
	w.groupAt = make([]int32, len(c.replies)+2)
	for _, r := range c.recs {
		if r.kind&kindDeleted != 0 {
			continue
		}
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
		if r.kind&kindDeleted != 0 {
			continue
		}
		if r.parent != noChainRecord {
			w.kids[childNext[r.parent]] = h
			childNext[r.parent]++
		}
		if r.reply != 0 {
			w.group[groupNext[r.reply]] = h
			groupNext[r.reply]++
		}
	}
}

// decide is End's work: each step of the model in the order the resume runs
// it ([threadChain]).
func (c *threadChain) decide(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.why != "" {
		return nil
	}
	// THE LARGE FILE: path-free where no line binds the thread to the small
	// path and, in the main thread, every reader starts from the last record
	// (lastMarked is the main thread's alone) and the dispatch from it alone
	// reaches every record; in a subagent's thread no reader has a slot to
	// start from, and every one reads every line.
	main := c.agent == ""
	free := !c.smallOnly && !c.lastMarked
	var w *chainWork
	if len(c.recs) > 0 {
		w = c.work()
		if free && main {
			var err error
			if free, err = c.covered(ctx, w); err != nil {
				return err
			}
		}
	}
	if c.bytes > claudeResumeLargeFile && !free {
		c.cannotTell(whyLargeFile)
		return nil
	}
	c.anySize = free
	if len(c.recs) == 0 {
		// No record: the resume, or a subagent's own load, finds no
		// conversation, and hands nothing under every variant (2.1.322's Yya,
		// `if(s.size===0)return null`; Rye, `no_agent_messages`).
		c.orders = []chainOrder{{variants: allVariants}}
		return nil
	}
	relinked, err := c.relink(ctx, w)
	if err != nil || c.why != "" {
		return err
	}
	c.indexRecords(w)
	switch byCall, byCallErr := c.resultByCall(ctx, w); {
	case byCallErr != nil:
		return byCallErr
	case byCall:
		c.cannotTell(whyResultByCall)
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var leaf0, leaf1 chainHandle
	if main {
		if leaf0, leaf1, err = c.pick(ctx, w, relinked); err != nil || c.why != "" {
			return err
		}
	} else {
		leaf0 = c.ownLeaf(w)
		leaf1 = leaf0
	}
	// The variants part where q1t moves the leaf and where each release's
	// retraction filter reads its own names, both the main thread's alone
	// (THE VARIANTS).
	groups := []chainGroup{{leaf: leaf0, variants: allVariants}}
	if main {
		groups = []chainGroup{{leaf: leaf0, variants: variants310}, {leaf: leaf0, variants: allVariants &^ variants310}}
		if leaf1 != leaf0 {
			groups[1].variants &^= variantsMetcalfe
			groups = append(groups, chainGroup{leaf: leaf1, variants: variantsMetcalfe})
		}
	}
	leaves := []chainHandle{leaf0}
	if leaf1 != leaf0 {
		leaves = append(leaves, leaf1)
	}
	for _, leaf := range leaves {
		list, listErr := c.listFrom(ctx, w, leaf)
		if listErr != nil || c.why != "" {
			return listErr
		}
		for i := range groups {
			if groups[i].leaf == leaf {
				groups[i].list = list
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
// retraction filter, what they hand, and why. The tails' order parts the
// group's variants once more, warm_mochi's from the rest, but never what they
// hand: the two orders hold the same records.
type chainGroup struct {
	leaf     chainHandle
	variants chainVariants
	// list is what the walk, the recovery and the tails hand from the leaf,
	// before the filter.
	list chainList
	// verdicts is each record's answer under these variants, by handle - 1;
	// order the records they hand for certain, in warm_mochi's order of the
	// tails, and bfs in the order without it, nil where the two are one.
	verdicts   []chainVerdict
	order, bfs []chainHandle
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
// the resume hangs. Read off the thread as the relink left it, each record's
// parent as it left that, as fWo reads its map (so a relink that hangs a
// call's result from the records it kept makes that result one too).
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
			if s := c.index[source]; s != noChainRecord && !c.is(s, kindDeleted) && s != parent {
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
		case c.is(h, kindDeleted):
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

// holders is, by call id, the first two replies the relink left whose
// tool_use blocks carry it.
func (c *threadChain) holders() [][2]chainHandle {
	holders := make([][2]chainHandle, len(c.callIDs)+1)
	for i := range c.recs {
		h := chainHandle(i + 1)
		if !c.is(h, kindAssistant) || c.is(h, kindDeleted) {
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
// ([threadChain], THE LEAF). relinked is whether the relink chained records
// it kept, which turns the resume from its single path to its leaf set. Where
// q1t cannot read a reply's id, the thread is whyReplyID.
func (c *threadChain) pick(ctx context.Context, w *chainWork, relinked bool) (leaf0, leaf1 chainHandle,
	err error) {
	last := c.lastRow
	// set0 and set1 are the set each setting of q1t takes its leaf from: the
	// single path's turn record, where it finds one, else the leaf set.
	var set0, set1 []chainHandle
	if !relinked {
		var turn0, turn1 chainHandle
		if turn0, turn1, err = c.singlePath(ctx, w); err != nil || c.why != "" {
			return noChainRecord, noChainRecord, err
		}
		if turn0 != noChainRecord {
			set0 = []chainHandle{turn0}
		}
		if turn1 != noChainRecord {
			set1 = []chainHandle{turn1}
		}
	}
	if set0 == nil || set1 == nil {
		hr, setErr := c.leafSet(ctx, w)
		if setErr != nil {
			return noChainRecord, noChainRecord, setErr
		}
		pick0, pick1 := hr, hr
		if len(hr) > 1 {
			// The leaf set narrowed to one: up from the checkpoint, where it is
			// in the set and the last row descends through none of its batches
			// (Y1t, never directly) nor, in the metcalfe variants, holds q1t;
			// else up from the last row (2.1.322's Gr @293822608, 2.1.310's kr
			// @247198349, `uo&&Hr.has(uo)&&...?uo:Eo`).
			from0, from1 := last, last
			if _, in := slices.BinarySearch(hr, w.cp); in && w.cp != last && !c.y1t(w, last, w.cp, false) {
				from0, from1 = w.cp, w.cp
				fires, read := c.q1t(w, last, w.cp)
				if !read {
					c.cannotTell(whyReplyID)
					return noChainRecord, noChainRecord, nil
				}
				if fires {
					from1 = last
				}
			}
			if turn := c.nearestTurn(w, from0); turn != noChainRecord {
				pick0 = []chainHandle{turn}
			}
			if turn := c.nearestTurn(w, from1); turn != noChainRecord {
				pick1 = []chainHandle{turn}
			}
		}
		if set0 == nil {
			set0 = pick0
		}
		if set1 == nil {
			set1 = pick1
		}
	}
	return c.leafOf(set0), c.leafOf(set1), nil
}

// singlePath is the turn record the resume's single path walks from, with
// q1t's variants off (turn0) and on (turn1), where it did not relink; none
// where its walk up finds none ([threadChain], THE LEAF). Where q1t cannot
// read a reply's id, the thread is whyReplyID.
func (c *threadChain) singlePath(ctx context.Context, w *chainWork) (turn0, turn1 chainHandle, err error) {
	last := c.lastRow
	start := last
	if !c.anyLeafKey && c.newest != noChainRecord && c.newest != last {
		if start, err = c.newestUnder(ctx, w, last); err != nil {
			return noChainRecord, noChainRecord, err
		}
	}
	from0, from1 := w.cp, w.cp
	if w.cp != noChainRecord && w.cp != last {
		if c.y1t(w, last, w.cp, true) {
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
	turn0 = c.nearestTurn(w, from0)
	turn1 = turn0
	if from1 != from0 {
		turn1 = c.nearestTurn(w, from1)
	}
	return turn0, turn1, nil
}

// leafSet is the resume's leaf set ([threadChain], THE LEAF; 2.1.322's Cjt
// @293590395, 2.1.310's loop in kr @247197836), in the file's order: from each
// record the relink left that none names as its parent, the first user or
// assistant record up by parent through records that are neither, where no
// user or assistant record names that one as its parent. Each record's answer
// up is kept once walked, as Cjt keeps it, so the set takes one walk of the
// thread.
func (c *threadChain) leafSet(ctx context.Context, w *chainWork) ([]chainHandle, error) {
	// w.a holds the records some record names as its parent, w.b those a user
	// or assistant record names.
	w.a.clear()
	w.b.clear()
	for i, r := range c.recs {
		if r.kind&kindDeleted != 0 || r.parent == noChainRecord {
			continue
		}
		w.a.set(int(r.parent))
		if c.turn(chainHandle(i + 1)) {
			w.b.set(int(r.parent))
		}
	}
	// up is each record's answer, by handle: 0 not walked yet, upNone no turn
	// record (a root, a parent naming no record, or a loop), upWalking on the
	// walk under way, else the turn record's handle.
	const upNone, upWalking = -1, -2
	up := make([]int32, len(c.recs)+1)
	var set, path []chainHandle
	w.run.clear()
	for i, r := range c.recs {
		t := chainHandle(i + 1)
		if r.kind&kindDeleted != 0 || w.a.has(int(t)) {
			continue
		}
		found := noChainRecord
		path = path[:0]
		for at := t; at != noChainRecord; at = c.recs[at-1].parent {
			if err := w.tick(ctx); err != nil {
				return nil, err
			}
			if c.turn(at) {
				found = at
				break
			}
			if v := up[at]; v != 0 {
				if v > 0 {
					found = chainHandle(v)
				}
				break
			}
			up[at] = upWalking
			path = append(path, at)
		}
		answer := int32(upNone)
		if found != noChainRecord {
			answer = int32(found)
		}
		for _, p := range path {
			up[p] = answer
		}
		if found != noChainRecord && !w.b.has(int(found)) && !w.run.has(int(found)) {
			w.run.set(int(found))
			set = append(set, found)
		}
	}
	slices.Sort(set)
	return set, nil
}

// leafOf is the leaf the resume walks from of set, its single path's turn
// record or its leaf set, in the file's order: the member of the latest
// Date.parse of its timestamp, the first in the file on a tie (2.1.322's cj
// @293745290, 2.1.310's p0 @247129929, `y>s` from -Infinity), no record of no
// time a member; where set is empty, the record of the latest time of every
// one the relink left, of any kind, the first on a tie (2.1.322's Ozt
// @293848378, 2.1.310's p0 in m_i); and none, where the resume finds no
// conversation, where no record it reads has a time.
func (c *threadChain) leafOf(set []chainHandle) chainHandle {
	leaf := noChainRecord
	later := func(h chainHandle) {
		if r := c.recs[h-1]; r.kind&kindTimed != 0 && (leaf == noChainRecord || r.ms > c.recs[leaf-1].ms) {
			leaf = h
		}
	}
	if len(set) > 0 {
		for _, h := range set {
			later(h)
		}
		return leaf
	}
	for i, r := range c.recs {
		if r.kind&kindDeleted == 0 {
			later(chainHandle(i + 1))
		}
	}
	return leaf
}

// ownLeaf is the leaf a subagent's own load walks from ([threadChain], A
// SUBAGENT'S OWN LOAD; 2.1.322's Rye with cj, 2.1.310's FBe with p0): of its
// own leaves, the records of its own that no record of its own names as its
// parent, as the relink left each, a compaction's boundary aside, the one of
// the latest time, the first in the file on a tie; none where none has a time,
// when the load finds no conversation. It files the own leaves, in the file's
// order.
func (c *threadChain) ownLeaf(w *chainWork) chainHandle {
	// w.a holds the records a record of the subagent's own names as its parent.
	w.a.clear()
	for i, r := range c.recs {
		if r.kind&kindDeleted == 0 && c.own(chainHandle(i+1)) && r.parent != noChainRecord {
			w.a.set(int(r.parent))
		}
	}
	leaf := noChainRecord
	for i, r := range c.recs {
		h := chainHandle(i + 1)
		if r.kind&(kindDeleted|kindBoundary) != 0 || !c.own(h) || w.a.has(int(h)) {
			continue
		}
		c.ownLeaves = append(c.ownLeaves, h)
		if r.kind&kindTimed != 0 && (leaf == noChainRecord || r.ms > c.recs[leaf-1].ms) {
			leaf = h
		}
	}
	return leaf
}

// own reports whether record h is one of the subagent's own: of its agentId,
// and a sidechain's (`tt.agentId===e&&tt.isSidechain`).
func (c *threadChain) own(h chainHandle) bool { return c.is(h, kindAgent|kindSideTrue) }

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
// throughBatch always, since no checkpoint the chain models was ever marked):
// the last row descends through the checkpoint's batch, or, where direct, as
// the single path asks it and the leaf set's pick does not, from the
// checkpoint itself.
func (c *threadChain) y1t(w *chainWork, last, cp chainHandle, direct bool) bool {
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
		if direct && cur == cp {
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
// that made a call; above its run (it and the records of its own id it hangs
// from) lies a record; the walk up from the last row reaches that record, and
// of the records it passes outside the run, the last, the topmost (`K`), was
// written later than the checkpoint, and one is a reply saying something or
// making a call (LE @293601245, `H`); no record hangs off the checkpoint, and
// none off the last row but one that hangs off a reply. read is false where
// the run turns on a reply's id the layer reads as none, which may be absent,
// where the run stops, or empty, where it goes on (`message?.id!==void 0`).
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

// chainList is what the resume's walk, recovery and tails hand from one leaf,
// before the retraction filter: base, the walk with what the recovery reached
// placed in it; tails, what the tails hang under the leaf in warm_mochi's
// order, and bfs the same records in the order without it, nil where the two
// are one; and how each record of them was reached, by handle.
type chainList struct {
	base, tails, bfs []chainHandle
	how              []uint8
}

// listFrom is what the resume's walk, recovery and tails hand from leaf: none
// where leaf is none.
func (c *threadChain) listFrom(ctx context.Context, w *chainWork, leaf chainHandle) (chainList, error) {
	if leaf == noChainRecord {
		return chainList{}, nil
	}
	chain, err := c.walk(ctx, w, leaf)
	if err != nil {
		return chainList{}, err
	}
	list := chainList{how: make([]uint8, len(c.recs)+1)}
	if list.base, err = c.recover(ctx, w, chain, list.how); err != nil || c.why != "" {
		return chainList{}, err
	}
	if list.tails, list.bfs, err = c.tails(ctx, w, leaf); err != nil || c.why != "" {
		return chainList{}, err
	}
	tail := whyAt(whyLeafTail)
	for _, h := range list.tails {
		list.how[h] = tail
	}
	return list, nil
}

// tails is what the resume hangs under leaf after the walk and the recovery,
// with what they met in w.met ([threadChain], THE TAILS; 2.1.322's lWo
// @293747057, 2.1.310's kpo @247131650): dfs in warm_mochi's order, a walk by
// depth whose every record's children are sorted by time; bfs in the order
// without it, the records a walk by breadth finds, sorted by time together,
// nil where the two orders are one. Both walks take the records under leaf
// that are neither user nor assistant records, through records that are
// neither, and pass over a record met and what lies under it, so they find
// the same records: each record has one parent, so each is found from it
// alone. Where a sort holds no order, the thread is whyTailTime; where the
// walk by breadth finds more than threadChainMaxSpread records, whySpread.
func (c *threadChain) tails(ctx context.Context, w *chainWork, leaf chainHandle) ([]chainHandle, []chainHandle, error) {
	// The walk by breadth first, w.a holding what it found, so w.met is still
	// what the walk and the recovery left for the walk by depth.
	w.a.clear()
	var dfs, bfs []chainHandle
	queue := []chainHandle{leaf}
	for q := 0; q < len(queue); q++ {
		for _, k := range w.children(queue[q]) {
			if err := w.tick(ctx); err != nil {
				return nil, nil, err
			}
			if c.turn(k) || w.met.has(int(k)) || w.a.has(int(k)) {
				continue
			}
			w.a.set(int(k))
			bfs = append(bfs, k)
			queue = append(queue, k)
		}
	}
	switch {
	case !c.byTime(bfs):
		c.cannotTell(whyTailTime)
		return nil, nil, nil
	case len(bfs) > threadChainMaxSpread:
		c.cannotTell(whySpread)
		return nil, nil, nil
	}
	// The walk by depth, as the resume makes it: a record popped and met
	// already is passed over with what lies under it, and each record's
	// children are pushed latest first, so the earliest pops first.
	stack := []chainHandle{leaf}
	var kids []chainHandle
	for len(stack) > 0 {
		if err := w.tick(ctx); err != nil {
			return nil, nil, err
		}
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if h != leaf {
			if w.met.has(int(h)) {
				continue
			}
			w.met.set(int(h))
			dfs = append(dfs, h)
		}
		kids = kids[:0]
		for _, k := range w.children(h) {
			if !c.turn(k) {
				kids = append(kids, k)
			}
		}
		if len(kids) > 1 && !c.byTime(kids) {
			c.cannotTell(whyTailTime)
			return nil, nil, nil
		}
		for i := len(kids) - 1; i >= 0; i-- {
			if !w.met.has(int(kids[i])) {
				stack = append(stack, kids[i])
			}
		}
	}
	if slices.Equal(dfs, bfs) {
		bfs = nil
	}
	return dfs, bfs, nil
}

// byTime sorts records by time as the tails' sorts do, a stable sort of each
// record's timestamp compared as a string, and reports whether that
// comparison orders them at all: it reads a record of no time as equal to
// every record, so beside two records of different times such a record leaves
// their order to the engine's sort algorithm, and beside records of one time
// it leaves every record as it came. On the one form of a time the chain
// admits, the string's order is its milliseconds'.
func (c *threadChain) byTime(records []chainHandle) bool {
	untimed, times := false, 0
	var first int64
	for _, h := range records {
		switch r := c.recs[h-1]; {
		case r.kind&kindTimed == 0:
			untimed = true
		case times == 0:
			first, times = r.ms, 1
		case r.ms != first:
			times = 2
		}
	}
	switch {
	case untimed && times > 1:
		return false
	case untimed:
		return true
	}
	slices.SortStableFunc(records, func(x, y chainHandle) int { return cmp.Compare(c.recs[x-1].ms, c.recs[y-1].ms) })
	return true
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
// built once, of the records the relink left.
func (c *threadChain) side(w *chainWork, k chainKind) *chainSide {
	if !w.built {
		w.built = true
		for i, r := range c.recs {
			if r.kind&kindTimed != 0 && r.kind&kindDeleted == 0 {
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
// records the recovery places among its walked ones by their place in the
// file: those it recovered beside the reply, and those it hung under a record
// of the reply or a result beside it ([threadChain.hang]).
type chainWindow struct {
	start, end int
	recovered  []chainHandle
}

// chainAnchored is a record the recovery hung under the walked record at
// chain[at], which the resume places right after that record
// (`Ot.push(...an.get(so.uuid))`).
type chainAnchored struct {
	at     int32
	record chainHandle
}

// recover is chain with the records the recovery reaches placed in it
// ([threadChain], THE RECOVERY), and how each record of it was reached, into
// how by handle: those it recovers beside a reply, and those it hangs off one.
//
// THE RECORDS HUNG UNDER A WALKED RECORD ARE KEPT IN ONE LIST FOR THE THREAD,
// in the order hung, rather than in a list a window: every window whose
// walked records hold that record merges with the others into one (the later
// starts before the earlier ends), so the list a walked record gathers
// across the thread is the one the resume joins as it merges them, the
// earlier window's first.
//
// A MERGED WINDOW'S RECOVERED RECORDS ARE SORTED ONCE, by their place in the
// file, after its windows merge. The resume sorts each window's
// (`xn.sort(_t)` @293752430, @247137377) and sorts them again at each merge
// (`Ft.recovered=[...Ft.recovered,...Dt.recovered].sort(_t)` @293752828,
// @247137775): no two records share a place, so one sort of the merged
// window's hands the same order, where a sort at each merge costs a run of
// windows, each opening before the one before it ends, the square of the
// run's length, in a pass that counts no step. Measured at the chain's
// bounds (BenchmarkAThreadChainAtItsBounds, windows_merging_at_the_bound),
// End ran 3.3 s so, 1.9 s of it without a look at its context; with one
// sort, 1.4 s, looking at it at least every 11 ms.
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
		windows  []chainWindow
		anchored []chainAnchored
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
		hung := len(anchored)
		var err error
		if recovered, anchored, err = c.hang(ctx, w, chain, id, i, end, recovered, anchored, how); err != nil {
			return nil, err
		}
		if len(recovered) > 0 || len(anchored) > hung {
			windows = append(windows, chainWindow{start: i, end: end, recovered: recovered})
		}
	}
	if len(windows) == 0 {
		return chain, nil
	}
	// Each walked record's hung records together, in the order hung: the
	// lists of the windows that merge, joined.
	slices.SortStableFunc(anchored, func(x, y chainAnchored) int { return cmp.Compare(x.at, y.at) })
	longest := 0
	for i := 0; i < len(anchored); {
		run := i + 1
		for run < len(anchored) && anchored[run].at == anchored[i].at {
			run++
		}
		longest, i = max(longest, run-i), run
	}
	if len(chain) > threadChainMaxSpread || longest > threadChainMaxSpread {
		c.cannotTell(whySpread)
		return nil, nil
	}
	merged := make([]chainWindow, 0, len(windows))
	placed := len(chain) + len(anchored)
	for _, win := range windows {
		placed += len(win.recovered)
		if n := len(merged); n > 0 && win.start < merged[n-1].end {
			last := &merged[n-1]
			last.end = max(last.end, win.end)
			last.recovered = append(last.recovered, win.recovered...)
			continue
		}
		merged = append(merged, win)
	}
	for i := range merged {
		slices.Sort(merged[i].recovered)
	}
	out := make([]chainHandle, 0, placed)
	prev, a := 0, 0
	for _, win := range merged {
		out = append(out, chain[prev:win.start+1]...)
		j := 0
		for k := win.start + 1; k < win.end; k++ {
			for j < len(win.recovered) && win.recovered[j] < chain[k] {
				out = append(out, win.recovered[j])
				j++
			}
			out = append(out, chain[k])
			for a < len(anchored) && anchored[a].at == int32(k) {
				out = append(out, anchored[a].record)
				a++
			}
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
// record hung met at once and reached as whyHookChain into how. What it hangs
// under a record of the reply or a result it appends to recovered, which the
// window places by the file's order; what it hangs under a walked record, to
// anchored, at that record's place on the walk.
//
// Each window takes its records' children afresh, as the resume does, rather
// than once a thread: a chain one window leaves, a record with two children
// not met, can hang in a later one once a recovery between them met one.
func (c *threadChain) hang(ctx context.Context, w *chainWork, chain []chainHandle, id chainReplyID, start, end int,
	recovered []chainHandle, anchored []chainAnchored, how []uint8) ([]chainHandle, []chainAnchored, error) {
	group := w.records(id)
	if !slices.ContainsFunc(group, func(g chainHandle) bool { return c.is(g, kindUse) }) {
		return recovered, anchored, nil
	}
	hook := whyAt(whyHookChain)
	// under appends to into what pye hands from each of u's children, every
	// record of it met at once.
	under := func(u chainHandle, into []chainHandle) ([]chainHandle, error) {
		for _, y := range w.children(u) {
			from := len(into)
			var err error
			if into, err = c.pye(ctx, w, y, u, into); err != nil {
				return nil, err
			}
			for _, x := range into[from:] {
				w.met.set(int(x))
				how[x] = hook
			}
		}
		return into, nil
	}
	var err error
	for _, g := range group {
		if recovered, err = under(g, recovered); err != nil {
			return nil, nil, err
		}
	}
	for _, g := range group {
		for _, x := range w.children(g) {
			if !c.is(x, kindUser|kindResult) || !w.met.has(int(x)) {
				continue
			}
			if recovered, err = under(x, recovered); err != nil {
				return nil, nil, err
			}
		}
	}
	var hung []chainHandle
	for k := start + 1; k < end; k++ {
		if err = w.tick(ctx); err != nil {
			return nil, nil, err
		}
		u := chain[k]
		if !c.uf(u) {
			continue
		}
		if hung, err = under(u, hung[:0]); err != nil {
			return nil, nil, err
		}
		for _, x := range hung {
			anchored = append(anchored, chainAnchored{at: int32(k), record: x})
		}
	}
	return recovered, anchored, nil
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

// judge decides each record's answer under g's variants: the records the
// relink deleted not handed, what the walk, the recovery and the tails hand
// handed; then, in the main thread, the retraction filter of g's release, read
// off every record of that list, and after it the drop of that list's
// malformed attachments, what each takes not handed; and in a subagent's
// thread the hand-off to the subagent's own id ([threadChain], THE RELINK,
// THE RETRACTION FILTER, THE MALFORMED-ATTACHMENT DROP and A SUBAGENT'S OWN
// LOAD). Its orders are the records it hands for certain, in each order of
// the tails.
func (c *threadChain) judge(ctx context.Context, w *chainWork, g *chainGroup) error {
	g.verdicts = make([]chainVerdict, len(c.recs))
	off, deleted := chainVerdict{handed: verdictNo, why: whyAt(whyOffConversation)},
		chainVerdict{handed: verdictNo, why: whyAt(whyRelinkDeleted)}
	if g.leaf == noChainRecord {
		off.why = whyAt(whyNoConversation)
	}
	for i := range g.verdicts {
		g.verdicts[i] = off
		if c.recs[i].kind&kindDeleted != 0 {
			g.verdicts[i] = deleted
		}
	}
	if g.leaf == noChainRecord {
		return nil
	}
	// handed is the list, the tails in warm_mochi's order: the same records in
	// either.
	handed := slices.Concat(g.list.base, g.list.tails)
	for _, h := range handed {
		g.verdicts[h-1] = chainVerdict{handed: verdictYes, why: g.list.how[h]}
	}
	main := c.agent == ""
	if main {
		var names map[[chainPrefixBytes]byte]bool
		for _, h := range handed {
			if err := w.tick(ctx); err != nil {
				return err
			}
			if !c.readsRetractions(h, g.variants) {
				continue
			}
			for _, p := range c.prefixesOf(h) {
				if names == nil {
					names = map[[chainPrefixBytes]byte]bool{}
				}
				names[p] = true
			}
		}
		for _, h := range handed {
			if c.is(h, kindSystem) {
				continue
			}
			var p [chainPrefixBytes]byte
			copy(p[:], c.recs[h-1].uuid[:chainPrefixBytes])
			if names[p] {
				g.verdicts[h-1] = chainVerdict{handed: verdictNo, why: whyAt(whyRetracted)}
			}
		}
	}
	// The drop of malformed attachments, after the filter, so a record the
	// filter took keeps the filter's reason. It tests no mode itself: file
	// marks a main thread's attachments alone.
	for _, h := range handed {
		if v := &g.verdicts[h-1]; v.handed == verdictYes && c.is(h, kindAttachment|kindIllFormed) {
			*v = chainVerdict{handed: verdictNo, why: whyAt(whyMalformedAttachment)}
		}
	}
	if !main {
		// The hand-off: a subagent's own load hands on what the walk, the
		// recovery and the tails reach of its agentId alone, whatever its side.
		another := chainVerdict{handed: verdictNo, why: whyAt(whyAnotherAgent)}
		for i := range g.verdicts {
			if v := &g.verdicts[i]; v.handed != verdictNo && c.recs[i].kind&kindAgent == 0 {
				*v = another
			}
		}
	}
	uncertain := func(h chainHandle) bool { return g.verdicts[h-1].handed != verdictYes }
	g.order = slices.DeleteFunc(handed, uncertain)
	if g.list.bfs != nil {
		g.bfs = slices.DeleteFunc(slices.Concat(g.list.base, g.list.bfs), uncertain)
	}
	return nil
}

// merge folds each group's answers into the thread's: handedYes where every
// group hands a record, handedNo where none does, each with the default
// variant's reason; handedUnknown where a group cannot tell, with its reason,
// or where the groups part, whyLeafFlag where q1t's leaf parts them and else
// whyRelease. Each distinct order is kept once, with the variants that hand
// it, the default variant's first: a group's warm_mochi variants hand its
// order, the rest its order with the tails sorted after a walk by breadth.
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
		bfs := g.order
		if g.bfs != nil {
			bfs = g.bfs
		}
		c.order(g.order, g.variants&variantsMochi)
		c.order(bfs, g.variants&^variantsMochi)
	}
}

// order files records as the order variants hand, where any does: added to
// the variants of an order of the same records in the same order, else a
// distinct order of its own.
func (c *threadChain) order(records []chainHandle, variants chainVariants) {
	if variants == 0 {
		return
	}
	if at := slices.IndexFunc(c.orders, func(o chainOrder) bool { return slices.Equal(o.records, records) }); at >= 0 {
		c.orders[at].variants |= variants
		return
	}
	o := chainOrder{records: records, at: make([]int32, len(c.recs)), variants: variants}
	for i := range o.at {
		o.at[i] = -1
	}
	for i, h := range records {
		o.at[h-1] = int32(i)
	}
	c.orders = append(c.orders, o)
}
