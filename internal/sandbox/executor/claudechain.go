package executor

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// claudeResumeRead reports whether release is one whose resume [threadChain]
// models: 2.1.310, 2.1.322 and 2.1.330, the three whose shipped code it was
// read from, 2.1.330 the release the runner pins. A SET, NEVER A RANGE: a
// model read off three releases says nothing of a fourth.
// release is a `--version` reading as sandbox.ValidRelease holds one, compared
// whole. Exact telling asks it of the release that wrote the session, the note
// check of the job's own and the relaunch box's. Where it answers false,
// [threadChain]'s answer is no answer for that resume. TO ADD A RELEASE, read its binary as data, never run, against every
// step of the model at the offsets each step's doc gives, restate each step it
// does otherwise, and run the facility's census over archives it wrote and
// resumed; only then add it here.
func claudeResumeRead(release string) bool {
	switch release {
	case "2.1.310", "2.1.322", "2.1.330":
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
	// handedYes is a record handed under every variant the reading cannot see.
	handedYes handedness = "handed"
	// handedNo is a record handed under none.
	handedNo handedness = "not_handed"
	// handedUnknown is a record such a variant decides, or one of a thread
	// holding a shape no model holds.
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
	whyChain           chainWhy = "chain"            // on the walk from the leaf
	whyRecoveredReply  chainWhy = "recovered_reply"  // another record of a walked reply, by reply id
	whyRecoveredResult chainWhy = "recovered_result" // a result whose parent is a record of a walked reply
	// whyHookChain is a record the recovery hangs off a walked reply that made
	// a call ([threadChain.hang]), as a batch's hooks write theirs.
	whyHookChain chainWhy = "hook_chain"
	// whyLeafTail is a record the tails hang under the leaf
	// ([threadChain.tails]), as a Stop hook writes its records.
	whyLeafTail chainWhy = "leaf_tail"
)

// Not handed: why the resume hands a record under no variant.
const (
	whyOffConversation chainWhy = "off_conversation" // no walk, recovery or tail reaches it
	whyRetracted       chainWhy = "retracted"        // the retraction filter drops it
	whyRelinkDeleted   chainWhy = "relink_deleted"   // the relink deletes it ([threadChain.relink])
	// whyMalformedAttachment is an attachment whose payload is not of its
	// type's shape ([threadLine.AttachmentWellFormed]), dropped in the main
	// thread after the retraction filter ([threadChain.judge]).
	whyMalformedAttachment chainWhy = "malformed_attachment"
	// whyAnotherAgent is a record a subagent's own load reaches whose agentId,
	// or none, is not that subagent's ([threadChain.ownLeaf]).
	whyAnotherAgent chainWhy = "another_agent"
	// whyNoConversation is every record, but one the relink deleted, of a
	// thread the resume or a subagent's load finds no conversation in.
	whyNoConversation chainWhy = "no_conversation"
	// whyNoRecord answers a handle naming no record: noChainRecord, or one
	// past the handles given out.
	whyNoRecord chainWhy = "no_record"
)

// Cannot tell, one record: a variant the reading cannot see decides it.
const (
	whyLeafFlag chainWhy = "leaf_flag" // one leaf hands it, another not, where q1t moves the leaf ([chainVariants])
	whyRelease  chainWhy = "release"   // only one release's retraction filter drops it
)

// Cannot tell, the whole thread: a shape no model holds. Every record answers
// handedUnknown with the thread's reason, and the chain hands no order.
const (
	whyBound chainWhy = "bound" // more records, prefixes or call ids than filed ([threadChainMaxRecords], [threadChainMaxCalls])
	// whyLine is a line the layer refuses that may yield the load a value
	// ([threadChain.Unread]).
	whyLine chainWhy = "line"
	// whyLineGap is a line handed out of turn: the chain is not reading the
	// file the resume reads.
	whyLineGap chainWhy = "line_gap"
	// whyProgress is a progress record (2.1.330's kPe @322056043, 2.1.322's
	// L_e @293638221, 2.1.310's YBe @247033685), which re-parents every later
	// record naming it.
	whyProgress chainWhy = "progress"
	// whyUUID is a record whose uuid is absent, no string, or no lowercase
	// canonical uuid: the load keys its map by the string as written, and
	// only that form maps one to one to a uuid's 16 bytes.
	whyUUID          chainWhy = "uuid"
	whyDuplicateUUID chainWhy = "duplicate_uuid" // a uuid two records carry
	whySidechain     chainWhy = "sidechain"      // a sidechain's record in the main thread
	// whyFileSnapshot is a file's snapshot, which 2.1.322 and 2.1.330 keep out
	// of the rows and the walk's fallback (ZI @293589374, t0 @321997792) and
	// 2.1.310 out of neither, and whose trailing run 2.1.330's leaf counts
	// (va @322267791, `xs`).
	whyFileSnapshot chainWhy = "file_snapshot"
	// whyForkBriefing is a fork's briefing in the main thread (2.1.330's dsn
	// @322296717, 2.1.322's jWo @293848295, 2.1.310's Ypo @247219197), which
	// is no row.
	whyForkBriefing chainWhy = "fork_briefing"
	// whyMarkedCheckpoint is a checkpoint a rewind, a withdrawal or a fork
	// marked, which starts the resume where it names.
	whyMarkedCheckpoint chainWhy = "marked_checkpoint"
	whyCheckpointShape  chainWhy = "checkpoint_shape" // leafUuid is neither a string nor null
	// whyAdmission is a record the load drops or rewrites, a reply whose mark
	// of an API error or model is in no shape, or, in the main thread, an
	// attachment the layer cannot say the malformed-attachment drop keeps.
	whyAdmission chainWhy = "admission"
	// whyReplyID is a reply id the layer cannot read as the resume does: no
	// string, or holding U+FFFD ([threadLine.MessageID]); or, where q1t reads
	// it (2.1.322's, 2.1.330's), one that may be absent or empty.
	whyReplyID chainWhy = "reply_id"
	// whyCallID is a call id holding U+FFFD, which the layer may read
	// otherwise than the resume ([threadChain.file]).
	whyCallID chainWhy = "call_id"
	// whyTimestamp is a timestamp not in the form Claude Code writes of a real
	// time: the resume compares times as strings in one place and as dates in
	// another, one order only on that form.
	whyTimestamp       chainWhy = "timestamp"
	whyRetractionShape chainWhy = "retraction_shape" // a retraction list in no shape
	whyPreservedShape  chainWhy = "preserved_shape"  // a compaction's metadata in no shape
	// whyRelinkShape is a relink that would chain the kept records into a loop,
	// or under a record it does not keep ([threadChain.relink]).
	whyRelinkShape chainWhy = "relink_shape"
	// whyReplacementShape is a content-replacement record whose list 2.1.310's
	// load fails at ([threadLine.ReplacementCount]).
	whyReplacementShape chainWhy = "replacement_shape"
	whySpread           chainWhy = "spread"         // a list past [threadChainMaxSpread] passed whole into one call
	whyLargeFile        chainWhy = "large_file"     // past [claudeResumeLargeFile] and not path-free ([threadChain.AnySize])
	whyResultByCall     chainWhy = "result_by_call" // tengu_foamy_spring may hang the result elsewhere ([threadChain.resultByCall])
	// whyLeafRule is a checkpoint the last row does not descend through where
	// a leaf rule of 2.1.330 the chain does not hold may move the leaf to the
	// last row ([threadChain.leafRules]).
	whyLeafRule chainWhy = "leaf_rule"
	// whyPutBack is a user record naming rows to put back ahead of it, which
	// 2.1.330's walk moves under foamy_spring ([threadLine.PutsBack]).
	whyPutBack chainWhy = "put_back"
	// whyLeafTime is a leaf chosen among records none of which has a time,
	// which 2.1.330 walks from the last of and 2.1.310 and 2.1.322 not at all
	// ([threadChain.leafOf]).
	whyLeafTime chainWhy = "leaf_time"
	// whyTailTime is a tails sort the resume's comparison cannot order, so its
	// engine's sort algorithm does ([threadChain.byTime]).
	whyTailTime chainWhy = "tail_time"
	// whyForkContext is a fork's reference in a subagent's thread: one naming
	// the subagent prefixes the parent's conversation to its load's
	// ([threadChain.ownLeaf]).
	whyForkContext chainWhy = "fork_context"
	whyAgent       chainWhy = "agent" // a subagent id no member is named by ([validClaudeAgent])
)

// whyUndecided answers a question asked before End decided, or after an End
// its context cut short: a reader's misuse, answered rather than a panic,
// which would end the node's collection for every seat it holds.
const whyUndecided chainWhy = "undecided"

// chainWhys is every reason, the place each answer stores its reason by.
var chainWhys = [...]chainWhy{"", whyChain, whyRecoveredReply, whyRecoveredResult, whyHookChain, whyLeafTail,
	whyOffConversation, whyRetracted, whyRelinkDeleted, whyMalformedAttachment, whyAnotherAgent, whyNoConversation,
	whyNoRecord, whyLeafFlag, whyRelease, whyBound, whyLine, whyLineGap, whyProgress, whyUUID,
	whyDuplicateUUID, whySidechain, whyFileSnapshot, whyForkBriefing, whyMarkedCheckpoint, whyCheckpointShape,
	whyAdmission, whyReplyID, whyCallID, whyTimestamp, whyRetractionShape, whyPreservedShape, whyRelinkShape,
	whyReplacementShape, whySpread, whyLargeFile, whyResultByCall, whyLeafRule, whyPutBack, whyLeafTime, whyTailTime,
	whyForkContext, whyAgent, whyUndecided}

// Valid reports whether w is one of the reasons.
func (w chainWhy) Valid() bool { return w != "" && slices.Contains(chainWhys[:], w) }

// chainVariants is a set of the thirteen variants the reading cannot see,
// one bit each. THE VARIANTS: the relaunch box's release, 2.1.310, 2.1.322 or
// 2.1.330, and three flags its servers may set, each at its default where none
// answers: tengu_foamy_spring (2.1.330's Dc() @313873180, 2.1.322's hu(),
// 2.1.310's Ef(), on), tengu_warm_mochi (2.1.330's Exl() @314474638, ota()
// @287013832, Fri() @240966974, on, and off wherever foamy_spring is) and, in
// 2.1.322 and 2.1.330, tengu_sorted_metcalfe (@293811649, @322254277, off).
// They part in the leaf ([threadChain.q1t]), the retraction filter's names
// ([threadChain.readsRetractions]) and the tails' order ([threadChain.tails]).
// A record handed under some and not others is handedUnknown (whyLeafFlag,
// whyRelease); foamy_spring's hanging of a result by its call is refused
// (whyResultByCall), as are 2.1.330's other leaf rules and rows put back.
type chainVariants uint16

// The variants; warm_mochi and sorted_metcalfe read as off where foamy_spring
// is off.
const (
	variant310Off chainVariants = 1 << iota
	variant310Spring
	variant310SpringMochi
	variant322Off
	variant322Spring
	variant322SpringMochi
	variant322SpringMetcalfe
	variant322SpringMochiMetcalfe
	variant330Off
	variant330Spring
	variant330SpringMochi
	variant330SpringMetcalfe
	variant330SpringMochiMetcalfe

	allVariants = variant310Off | variant310Spring | variant310SpringMochi | variant322Off | variant322Spring |
		variant322SpringMochi | variant322SpringMetcalfe | variant322SpringMochiMetcalfe | variant330Off |
		variant330Spring | variant330SpringMochi | variant330SpringMetcalfe | variant330SpringMochiMetcalfe
	// variants310 is 2.1.310's, whose retraction filter reads a refusal's
	// fallback alone.
	variants310 = variant310Off | variant310Spring | variant310SpringMochi
	// variantsMetcalfe322 and variantsMetcalfe330 are each release's with
	// foamy_spring and sorted_metcalfe on, where q1t moves the leaf, each by
	// its own reading of it.
	variantsMetcalfe322 = variant322SpringMetcalfe | variant322SpringMochiMetcalfe
	variantsMetcalfe330 = variant330SpringMetcalfe | variant330SpringMochiMetcalfe
	variantsMetcalfe    = variantsMetcalfe322 | variantsMetcalfe330
	// variantsMochi has warm_mochi on: tails are a walk by depth, else by
	// breadth ([threadChain.tails]).
	variantsMochi = variant310SpringMochi | variant322SpringMochi | variant322SpringMochiMetcalfe |
		variant330SpringMochi | variant330SpringMochiMetcalfe
	// variantDefault is 2.1.330, the release the runner pins, as no flag
	// server answers it: the variant a definite answer's reason is given for.
	variantDefault = variant330SpringMochi
)

// chainReplyID is a reply a threadChain interned, from 1; 0 is none.
type chainReplyID int32

// chainCallID is a call id a threadChain interned, from 1: a key comparable
// across the thread, never the id; 0 is none.
type chainCallID int32

// THE BOUNDS. A thread at the plaintext's bound, 256 MiB at the 2,053 bytes a
// line probe P6 measured, is 130,754 lines, so 2^17 files a record a line, and
// a call, two records of a block each, about as many ids; two bounds, not one
// sum, since a thread of ordinary calls fills each about halfway. Past either,
// See files nothing more (whyBound). HELD AT MOST (BenchmarkAThreadChainAtItsBounds,
// GOGC=10, two cores): 43 MiB at both, End 33 to 34 ms of a 2.0 to 2.2 s read;
// End's worst, windows merging at the spread's bound, 1.8 to 1.9 s. THE
// COLLECTION'S PASS reads the chain, the count and the next request's estimate
// in one walk ([walkThread]) in 2.6 to 2.9 s, within MainThreadReadWait, in its
// own large slot ([CollectionLargeReadsAtOnce]; the package doc says why).
const (
	// threadChainMaxRecords is the most items the chain files of one thread:
	// its records, their retraction prefixes, and the uuids its newest
	// compaction boundary lists.
	threadChainMaxRecords = 1 << 17
	// threadChainMaxCalls is the most call-id occurrences filed of one thread:
	// each tool_use and tool_result block's id, and each attachment's string
	// toolUseID.
	threadChainMaxCalls = 1 << 17
	// threadChainMaxSpread is the longest list the resume passes whole into one
	// call's arguments (whySpread): the load's spread of a content-replacement
	// list (2.1.330 @322267061, 2.1.322 @293820306, 2.1.310 @247196171), the
	// recovery's of the walk's stretches and a walked record's hung records
	// (@322189426, @322189582; @293753056, @293753212; @247138003,
	// @247138159), and the tails' walk by breadth (@322183011, @293747616,
	// @247132204). JavaScriptCore refuses a call of more than 65,536 arguments
	// with the RangeError "Maximum call stack size exceeded." (2.1.322
	// @14459602, 2.1.310 @3746582). Half that keeps every modelled spread clear
	// of it, and is the most each binary's helpers pass to
	// String.fromCharCode.apply at once (2.1.330 @336105734, 2.1.322
	// @306148813, 2.1.310 @259142857).
	threadChainMaxSpread = 1 << 15
)

// threadChainHeldBytes is what the chain holds live at both bounds (HELD AT
// MOST above): a collection's pass past its base, or a Claude Code reading of
// a kept session ([HoldingsReadsAtOnce]), at its worst.
const threadChainHeldBytes = 43 << 20

// claudeResumeLargeFile is the most bytes of a thread Claude Code's load
// reads with its small path (2.1.330's X_e @312227922 in qie @322272433,
// 2.1.322's Jne @284268664 in oee @293824518, 2.1.310's Use @238504623 in Cne
// @247199785). THE LARGE FILE: past it a large-file reader chooses by their
// bytes the lines it hands the load: the DISPATCH (2.1.330's DPe @322233162,
// 2.1.322's ike @293792132, 2.1.310's HMt @247175414), or, where
// CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP is set (a settings file can set it
// whatever the launcher unsets), the PRUNE (jrn @322229157, F1t @293788207,
// FPt @247171899). The chain models both and answers a thread past it only
// where it is path-free ([threadChain.AnySize]), else whyLargeFile. 2.1.330's
// re-read of a file its read outran and the meta rows its dispatch skips or
// defers (`never`) touch no record of a file at rest.
const claudeResumeLargeFile = 5 << 20

// The bytes the large-file readers read a line by ([claudeResumeLargeFile]),
// as 2.1.330's DPe and jrn, 2.1.322's ike and F1t and 2.1.310's HMt and FPt
// hold them.
var (
	// slotPrefix begins a line every reader takes for a SLOT, a record read by
	// its bytes; slotParentKey is the key the dispatch takes one by where its
	// line carries it at its object's depth one.
	slotPrefix    = []byte(`{"parentUuid":`)
	slotParentKey = []byte(`"parentUuid":`)
	// slotUUIDKey precedes a slot's uuid, slotTimeAfter follows the uuid a
	// reader keys the slot by first.
	slotUUIDKey   = []byte(`"uuid":"`)
	slotTimeAfter = []byte(`","timestamp":"`)
	// slotSidechain makes a slot a sidechain's to each reader; slotSidechainHead
	// is how a subagent's record says so right after its parent.
	slotSidechain     = []byte(`"isSidechain":true`)
	slotSidechainHead = []byte(`,"isSidechain":true`)
)

// claudeFallbackMS is the most milliseconds before a record whose parent the
// walk cannot follow at which the walk takes another record in its place
// (2.1.330 @321997861, 2.1.322's aUo @293589450, 2.1.310's vpo @247132972).
const claudeFallbackMS = 5000

// chainPrefixBytes is how many bytes of a uuid the retraction filter compares
// a record by: its first 24 characters (2.1.330 @327497941, 2.1.322's cE
// @298529717, 2.1.310's Gb
// @251726667), 20 hex digits and 4 hyphens of a canonical uuid.
const chainPrefixBytes = 10

// threadChain models which records Claude Code's resume hands its model, in
// what order, for one thread read once a line at a time: main mode
// (newThreadChain("")) is `claude --resume <session>` through the retraction
// filter and malformed-attachment drop it applies to the loader's output;
// subagent mode a subagent's own load. Each rule restates, with offsets, the
// shipped code of the releases [claudeResumeRead] names, on the code that runs
// it: THE LOAD on file, THE LINES THAT ARE NO RECORD on Unread, THE LARGE FILE
// on claudeResumeLargeFile, THE RELINK on relink, THE LEAF on pick, THE LEAF
// SET on leafSet, THE WALK, THE RECOVERY, THE TAILS on walk, recover, tails, A
// SUBAGENT'S OWN LOAD on ownLeaf, THE RETRACTION FILTER and THE
// MALFORMED-ATTACHMENT DROP on judge, THE VARIANTS on chainVariants. One
// reader, no lock.
type threadChain struct {
	// agent is the subagent whose thread this is, "" for the main thread.
	agent string
	state chainState
	// next is the line number See or Unread must be handed next, from 1.
	next int
	// bytes is the lines handed so far, breaks included: the file's size where
	// See's precondition holds. A torn last piece is no line; the box cuts it
	// from a main thread before its resume ([claudeSessionGate]).
	bytes int64
	// smallOnly is whether a line was met that a large-file reader may read
	// otherwise than the small path, lastMarked whether the last record filed
	// holds a sidechain's bytes (in the main thread they move where every reader
	// starts); anySize is End's path-free ([threadChain.AnySize]).
	smallOnly, lastMarked, anySize bool
	// why is the thread's own cannot tell, the first shape met; "" while the
	// thread is modelled.
	why chainWhy
	// past is whether a bound was passed: the chain files nothing more.
	past bool
	// handles is how many handles See gave out, past the bound none.
	handles int

	recs []chainRecord // every record filed, by handle - 1
	// index is each record's uuid's handle, written each record's parent as
	// written, until End resolves them.
	index   map[[16]byte]chainHandle
	written []chainWritten
	replies map[[32]byte]chainReplyID // reply ids by digest
	// callIDs interns call ids by digest; carried says, by id - 1, whether a
	// block carries the id, rather than an attachment alone.
	callIDs map[[32]byte]chainCallID
	carried []bool
	occurs  []chainCallID // every call-id occurrence filed, from each record's calls
	// prefixes is every retraction prefix filed, from each record's prefix.
	prefixes [][chainPrefixBytes]byte
	filed    int // items counted against threadChainMaxRecords
	// sources is each result's sourceToolAssistantUUID, where canonical.
	sources map[chainHandle][16]byte
	// checkpoint is the leaf the last checkpoint since the last boundary
	// names, anyLeafKey whether any checkpoint carries leafUuid.
	checkpoint chainCheckpoint
	anyLeafKey bool
	// lastRow and newest are the session's last and newest rows; newestMS is
	// the newest's time. Every modelled main-thread record is a row, and the
	// last row, written at or after the last boundary, is never relink-deleted.
	lastRow, newest chainHandle
	newestMS        int64
	// lastBoundary is the last compaction's boundary, kept what the newest
	// naming one says for the relink ([threadChain.keptOf]).
	lastBoundary chainHandle
	kept         chainKept
	scratch      [][chainPrefixBytes]byte // one record's retraction prefixes

	// What End decided: each record's verdict (by handle - 1), the distinct
	// orders, the leaf every variant walks from, a subagent's own leaves.
	verdicts  []chainVerdict
	orders    []chainOrder
	leaf      chainHandle
	ownLeaves []chainHandle
}

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
	kindMeta // a user record's `isMeta` the JSON true
	// kindResult is a user record holding a tool_result block, kindUse an
	// assistant record holding a tool_use block.
	kindResult
	kindUse
	// kindSynthetic is a reply its harness wrote: marked an API error's, or
	// of the model "<synthetic>".
	kindSynthetic
	kindSays // a message whose text says something (SUo)
	// kindBoundary is a compaction's boundary, kindFallback a refusal's
	// fallback.
	kindBoundary
	kindFallback
	kindTimed // has a timestamp
	// kindIllFormed is, in the main thread alone, an attachment the
	// malformed-attachment drop takes.
	kindIllFormed
	kindNowhere // parent names no record
	kindDeleted // deleted by the relink, in no step after it
	// kindAgent is, in a subagent's thread, a record of that subagent's
	// agentId; with kindSideTrue, one of its own.
	kindAgent
)

// chainRecord is what the chain files of one record.
type chainRecord struct {
	// uuid is the record's own uuid, the 16 bytes of its lowercase canonical
	// form ([uuidBytes]). Not uuid.UUID: the load keys its map by the uuid as
	// written, so two strings uuid.Parse reads as one are two records to it.
	uuid [16]byte
	// ms is its timestamp's Unix milliseconds, where kindTimed
	// ([threadLine.Timestamp]). Not time.Time: on the one admitted form the
	// resume's string and Date.parse orders agree, and it costs three times the
	// bytes.
	ms int64
	// parent is the record its parent names, as End resolves it:
	// noChainRecord for a root and for a parent naming no record (kindNowhere).
	parent chainHandle
	reply  chainReplyID
	// calls and prefix are where its call-id occurrences and retraction
	// prefixes start.
	calls, prefix int32
	line          int32 // its line's number
	kind          chainKind
}

// chainWritten is a uuid as a record writes it (a parent, or one a boundary
// names) until End resolves it: named where it is a lowercase canonical uuid,
// the one form a record's own uuid takes; another string names no record.
type chainWritten struct {
	uuid  [16]byte
	named bool
}

// writtenUUID is s, a uuid as a record writes it.
func writtenUUID(s string) chainWritten {
	u, named := uuidBytes(s)
	return chainWritten{uuid: u, named: named}
}

// chainKept is what a boundary that names what it kept says
// ([threadLine.Preserved]), filed for End's relink.
type chainKept struct {
	// boundary is the boundary, noChainRecord where none names what it kept.
	boundary chainHandle
	// anchor is the record the kept records hang from. segment is whether the
	// boundary names them by a segment, from tail up by parent to head; else
	// list holds the canonical uuids it lists, listed how many entries, and
	// nowhere whether one is no canonical uuid.
	anchor, head, tail chainWritten
	segment, nowhere   bool
	list               [][16]byte
	listed             int
}

// chainCheckpoint is the leaf a checkpoint names: set where one does since the
// last boundary, named where that is a canonical uuid.
type chainCheckpoint struct {
	uuid       [16]byte
	set, named bool
}

// chainVerdict is a record's answer as End stores it: handedness and the
// reason's place in chainWhys.
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

// newThreadChain is the model of a thread not read yet: agent "" the main
// thread as `claude --resume <session>` loads it; else subagent agent's own
// load. An agent outside [validClaudeAgent]'s rule is whyAgent.
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

// filing panics where End has run: a reader's own misuse, which its tests find.
func (c *threadChain) filing(what string) {
	if c.state != chainFiling {
		panic("executor: threadChain." + what + " after End")
	}
}

// inTurn holds n, a line's number, to the next one expected: a line skipped or
// repeated is whyLineGap, never a panic, since a reader runs inside a node's
// collection.
func (c *threadChain) inTurn(n int) {
	if n != c.next {
		c.cannotTell(whyLineGap)
	}
	c.next = n + 1
}

// See files line, the thread's next whole line, and answers its handle
// (noChainRecord for a line the load keeps no record of) and, for a user or
// assistant record, its message ([threadLine.Message]) and that message's
// error, already taken as whyAdmission. Past the bound it files nothing more
// (whyBound) and still hands back each message, with pastChainBound. THE
// BYTES ARE THE RESUME'S: the lines must be the file the resume reads, every
// whole line in turn; the chain checks only their numbers (whyLineGap). The
// launcher leaves CLAUDE_CODE_REMOTE unset, so the hosted environment's gap
// bridge (2.1.330's O_r @322183061, 2.1.322's uWo @293747640, 2.1.310's wpo
// @247132270) never runs, nor 2.1.330's rows put back (whyPutBack). See
// panics after End.
func (c *threadChain) See(line threadLine) (chainHandle, threadMessage, error) {
	c.filing("See")
	c.inTurn(line.N)
	c.bytes += int64(line.Size)
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

// another files line, one the load keeps no record of: a progress record, a
// checkpoint, a fork's reference or a content replacement.
func (c *threadChain) another(line threadLine) {
	if c.binds(&line.Bytes) {
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

// Unread files a whole line [threadReader.Next] answered as no record, its N
// and Size held as See holds a line's. THE LINES THAT ARE NO RECORD: the load
// parses a chunk at a time (2.1.330's Wrn @322232985, 2.1.322's $1t
// @293791955, 2.1.310's $Pt @247175237), keeping the values before a failure
// and resuming after the next break, so a line failing before any whole value,
// a whole value no object (ftt @322052007, iVe @293635513, oMt @247031904),
// and an object whose refused head is of a
// type no resume reads yield it nothing and are skipped; any other refused
// line may yield a value (whyLine). A skipped line still binds the thread to
// the small path where a large-file reader may take it for a slot
// ([threadChain.binds]), as does one whose byte facts cover fewer bytes than
// its Size. Unread panics after End.
func (c *threadChain) Unread(fault *threadLineError) {
	c.filing("Unread")
	if fault == nil {
		c.smallOnly = true
		c.cannotTell(whyLine)
		return
	}
	c.inTurn(fault.N)
	c.bytes += int64(fault.Size)
	switch {
	case fault.Fault == faultSyntax, fault.Fault == faultNotObject, fault.Fault == faultHead && fault.Type == "":
		if fault.Bytes.Len()+1 != int64(fault.Size) || c.binds(&fault.Bytes) {
			c.smallOnly = true
		}
		return
	}
	c.smallOnly = true
	c.cannotTell(whyLine)
}

// binds reports whether b, the byte facts of a line the chain files no record
// of, binds the thread to the small path: where a large-file reader may take
// it for a slot ([threadBytes.SlotKeyed]) to start from or walk; but not, in a
// subagent's thread, where it says it is a sidechain's right after its parent
// ([sidechainHeaded]), since no reader starts from such a slot.
func (c *threadChain) binds(b *threadBytes) bool {
	return b.SlotKeyed() && (c.agent == "" || !sidechainHeaded(b.Head()))
}

// sidechainHeaded reports whether head, a line's first [lineHeadBytes]
// ([threadBytes.Head]), says the line is a sidechain's right after its
// parent: `{"parentUuid":`, null or a quoted lowercase canonical uuid, then
// `,"isSidechain":true`. Every large-file reader marks such a line a
// sidechain's: the dispatches within its first 256 bytes (gl @322236096, kl
// @293794950, @247178300), the prunes anywhere in it (@322229906, @293788960,
// @247172642).
func sidechainHeaded(head []byte) bool {
	rest, ok := bytes.CutPrefix(head, slotPrefix)
	switch {
	case !ok:
		return false
	case bytes.HasPrefix(rest, jsonNull):
		return bytes.HasPrefix(rest[len(jsonNull):], slotSidechainHead)
	case len(rest) < 36+2 || rest[0] != '"' || rest[37] != '"':
		return false
	}
	if _, canonical := uuidBytes(string(rest[1:37])); !canonical {
		return false
	}
	return bytes.HasPrefix(rest[38:], slotSidechainHead)
}

// checkpointOf files what a last-prompt record says of the leaf (2.1.330's
// Zin, 2.1.322's Mr, 2.1.310's Oo): it names the leaf where its leafUuid says something, and
// any that carries the key, null among it, turns off the newest-row start
// ([threadChain.singlePath], `leafUuid!==void 0`).
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

// file files line, a record of the map, read as message m (mErr its error),
// and answers its handle, or pastChainBound past a bound. THE LOAD (2.1.330's
// Zin @322258725, 2.1.322's Mr in Szt @293813830, 2.1.310's Oo in GMt
// @247190769) keeps every user, assistant, attachment and system object in a
// map keyed by its uuid as written, in the order first written: a handle is a
// place in that order. Its admission (Eon @322042571, DWt @293626797, sPt
// @247023276) drops or rewrites a user or assistant record whose message the
// layer refuses, or an assistant one of string content, re-parenting its
// children, and turns a lone surrogate into U+FFFD (@310404933, tIe
// @282906774, hYe @237222449). The chain refuses each such record and every
// shape a whole-thread reason names. A compaction's boundary (On @314444996,
// Kr @291612448) forgets the checkpoint.
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
	// ([threadChain.ownLeaf]).
	attachment, typeErr := line.AttachmentType()
	briefing := typeErr == nil && attachment == attachmentForkBriefing
	if briefing && c.agent == "" {
		c.cannotTell(whyForkBriefing)
	}
	if mErr != nil {
		c.cannotTell(whyAdmission)
	}
	if m.IsText() && head.Type == recordAssistant {
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
		// 2.1.330's prune reads a virtual reply that makes a call as a run's
		// link, not a reply (jrn @322229157, its runLink test first), a
		// reading the chain does not hold.
		if virtual, err := line.IsVirtual(); virtual || err != nil {
			c.smallOnly = true
		}
	}
	if line.PutsBack() {
		c.cannotTell(whyPutBack)
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
	// The malformed-attachment drop follows the main thread's load alone:
	// judge reads this mark.
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
		// A boundary carries no retraction list ([threadLine.Retracted]); its
		// room is held to the bound beside the list it names.
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
		// The newest naming boundary replaces the one before it, whose list
		// leaves the count (2.1.322's iWo and 2.1.330's C_r read the last
		// alone, `r=Ce`).
		c.filed += preserved.listed - c.kept.listed
		preserved.boundary = h
		c.kept = preserved
	}
	if src, ok := line.SourceToolAssistantUUID(); ok && r.kind&kindResult != 0 {
		c.sources[h] = src
	}
	c.slotFacts(line, id, written, r.kind)
	c.recs = append(c.recs, r)
	c.written = append(c.written, written)
	// A ROW is every record but a sidechain's, a fork's briefing and a file's
	// snapshot (2.1.322's Mr: `!ZI(Ho)`, `!Ho.isSidechain&&!jWo(Ho)`; 2.1.330's
	// Zin alike, t0 and dsn).
	if r.kind&kindSideTrue == 0 && !briefing && !snapshot {
		c.row(h, r)
	}
	return h
}

// row files record h, r, as a row: the session's last, and its newest where its
// timestamp is later, as a string (`timestamp>co`), than every row's before it.
func (c *threadChain) row(h chainHandle, r chainRecord) {
	c.lastRow = h
	if r.kind&kindTimed != 0 && (c.newest == noChainRecord || r.ms > c.newestMS) {
		c.newest, c.newestMS = h, r.ms
	}
}

// slotFacts files what the large-file readers make of line by its byte facts
// ([threadBytes]), the record whose own uuid is id, parent written and kind
// kind: a reader may read it otherwise than its decode where it is a
// compaction's boundary (the dispatch forgets the slots before it), where it
// does not begin `{"parentUuid":` and the parent its decode names ([slotHead]),
// where the readers key it by another uuid ([threadBytes.KeyedBy]), and in a
// subagent's thread where it does not say it is a sidechain's right after its
// parent. In the main thread it files whether the record holds a sidechain's
// bytes, which move every reader's start where it is the last record.
func (c *threadChain) slotFacts(line threadLine, id string, written chainWritten, kind chainKind) {
	head := line.Bytes.Head()
	end := slotHead(head, line.Head.Parent, written.named)
	switch {
	case end < 0, kind&kindBoundary != 0, !line.Bytes.KeyedBy(id):
		c.smallOnly = true
	case c.agent != "" && !bytes.HasPrefix(head[end:], slotSidechainHead):
		c.smallOnly = true
	}
	if c.agent == "" {
		c.lastMarked = line.Bytes.Has(markSidechain)
	}
}

// slotHead is where, in head ([threadBytes.Head]), the parent every
// large-file reader reads ends, where that is the parent its decode reads:
// head begins `{"parentUuid":` and then parent as written, null or, named, a
// lowercase canonical uuid quoted with no escape: the two forms every reader
// reads alike (gl @322236096, kl @293794950, Eo @247177994, jrn's, F1t's
// and FPt's keying). -1 otherwise.
func slotHead(head, parent []byte, named bool) int {
	if !bytes.Equal(parent, jsonNull) && (!named || len(parent) != 36+2) {
		return -1
	}
	if !bytes.HasPrefix(head, slotPrefix) || !bytes.HasPrefix(head[len(slotPrefix):], parent) {
		return -1
	}
	return len(slotPrefix) + len(parent)
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

// parentOf is record r's parent as written: `parentUuid` absent, null or ""
// a root, falsy to every walk; a lowercase canonical uuid to resolve at End;
// any other string a parent naming no record, kindNowhere.
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
	// THE ROOM BEFORE THE SHAPE: a list is read in order, so one whose
	// prefixes pass the room before an entry in no shape is past the bound,
	// and only one whose shape fails within it is in no shape
	// ([threadLine.Retracted]).
	r, err := line.Retracted()
	held, all := r.Prefixes()
	switch {
	case !all || len(held) > room:
		return false
	case err != nil:
		c.cannotTell(whyRetractionShape)
		return true
	}
	c.scratch = append(c.scratch, held...)
	return true
}

// keptOf reads what line, a compaction's boundary, says it kept
// ([threadLine.Preserved]), the canonical uuids it lists in an array of their
// own; and reports whether it names what it kept at all, and whether the
// boundary fits threadChainMaxRecords beside what the chain filed: with its
// list in place of the replaced boundary's, where it names what it kept, else
// alone. Every entry counts, a canonical uuid or not; metadata in no shape is
// whyPreservedShape.
func (c *threadChain) keptOf(line threadLine) (k chainKept, named, fits bool) {
	// room is how many entries the list may hold beside the boundary itself.
	room := threadChainMaxRecords - c.filed + c.kept.listed - 1
	// THE ROOM BEFORE THE SHAPE, as [threadChain.retractionsOf]'s. A list of
	// no entry passes no room; the boundary's own room is judged below.
	p, err := line.Preserved()
	switch {
	case p.More || p.Listed > max(room, 0):
		return chainKept{}, false, false
	case err != nil:
		c.cannotTell(whyPreservedShape)
		return chainKept{}, false, c.filed < threadChainMaxRecords
	case !p.Named:
		return chainKept{}, false, c.filed < threadChainMaxRecords
	}
	k.listed, k.nowhere = p.Listed, p.Nowhere
	k.list = slices.Clone(p.UUIDs)
	k.anchor, k.head, k.tail, k.segment = writtenUUID(p.Anchor), writtenUUID(p.Head), writtenUUID(p.Tail), p.Segment
	// A list of none still needs the room the boundary itself takes.
	return k, true, room >= 0
}

// chainPrefix is the first 24 characters of entry, a retraction list's, as
// the 10 bytes of a canonical uuid's first 24, and true where they are such:
// 8, 4, 4 and 4 lowercase hex digits, each run followed by a hyphen. An entry
// that begins otherwise retracts nothing, since every modelled record carries
// a lowercase canonical uuid. Entry is a string or its bytes, for
// [uuidBytes]'s reason.
func chainPrefix[T ~string | ~[]byte](entry T) ([chainPrefixBytes]byte, bool) {
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

// items is how many items the chain holds: what [threadChainMaxRecords]
// counts (records, retraction prefixes, kept uuids) and the call-id
// occurrences [threadChainMaxCalls] counts, each at most the 344 bytes a
// record holds with its call at both bounds.
func (c *threadChain) items() int { return c.filed + len(c.occurs) }

// passBound makes the thread whyBound, drops what the chain filed, and files
// nothing more.
func (c *threadChain) passBound() chainHandle {
	c.cannotTell(whyBound)
	c.past = true
	c.recs, c.index, c.written, c.replies, c.callIDs, c.carried = nil, nil, nil, nil, nil, nil
	c.occurs, c.prefixes, c.sources, c.scratch, c.kept = nil, nil, nil, nil, chainKept{}
	return pastChainBound
}

// End decides, once the reader has answered io.EOF, what the resume hands:
// nothing is decided before it, since a record's parent can be written after
// it. It checks ctx between its steps and inside its walks; ctx's error means
// no model, the error returned, and every question then answers
// handedUnknown (whyUndecided). End panics where it already ran, as See and
// Unread after it.
func (c *threadChain) End(ctx context.Context) error {
	if c.state != chainFiling {
		panic("executor: threadChain.End twice")
	}
	c.state = chainAbandoned
	err := c.decide(ctx)
	c.index, c.written, c.sources, c.scratch, c.replies = nil, nil, nil, nil, nil
	c.kept = chainKept{}
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
// can stop reading a thread the model cannot place. After an End its context
// cut short, it answers that shape if one was met, else whyUndecided.
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
// the order it checks: noChainRecord handedNo (whyNoRecord); any other handle
// before End decided or after an End its context cut short, handedUnknown
// (whyUndecided); pastChainBound handedUnknown (whyBound); a handle never given
// out handedNo (whyNoRecord); every record of a thread the model cannot place
// handedUnknown with the thread's reason; else what End decided.
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
// records, until each returns false: the default variant's first. An order
// holds the records its variants hand for certain, a record any may or may not
// hand left out. None where the thread is cannot tell, or End has not decided.
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
// blocks carry, as keys comparable across the thread: Uses an assistant
// record's, Results a user record's, nil for every other. A reader never
// writes them. An attachment's toolUseID is no block's: it is interned beside
// them for the walk up from the last row alone, and CallKey answers false for
// an id only an attachment carries.
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

// OwnLeaves is, in a subagent's thread, its own leaves in the file's order,
// those its load chose its leaf among ([threadChain.ownLeaf]); a reader never
// writes it. Nil in the main thread, before End decided, after an End its
// context cut short, and where the thread is cannot tell.
func (c *threadChain) OwnLeaves() []chainHandle {
	if c.state != chainDecided || c.why != "" {
		return nil
	}
	return c.ownLeaves[:len(c.ownLeaves):len(c.ownLeaves)]
}

// AnySize reports whether the thread is PATH-FREE: every large-file reader
// ([claudeResumeLargeFile]) hands the load what its small path does, so the
// answers hold on either side of the size and for a copy differing only
// inside string values, never one with its keys reordered; a reader of another
// copy than the resume's relies on them only where this is true. It holds
// where no record is a boundary, no line of no record may be a slot
// ([threadChain.binds]), every record's line leads with its parent and is
// keyed by its uuid ([threadChain.slotFacts]), and in the main thread the last
// record has no `"isSidechain":true` and reaches every record
// ([threadChain.covered]). False before End decided, after an End cut short,
// and where the thread is cannot tell.
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

// uf reports whether record h hangs off the reply before it (2.1.330's zL
// @321995433, 2.1.322's UF @293588888, 2.1.310's l0 @246995738): an
// attachment, a system record but a compaction's boundary, or a meta user
// record holding no result.
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

// partOf reports whether record h is part of reply id's run (2.1.330's xPe
// @322185086, 2.1.322's X_e @293748740, 2.1.310's Yce @247133691): a record
// of the reply, a meta user
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
// retraction list: 2.1.330's hPn @324043599 and 2.1.322's eGe a user,
// assistant or fallback record's, 2.1.310's qGn a fallback's alone.
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
	// are each reply's records likewise: reply r's are
	// group[groupAt[r]:groupAt[r+1]]. Both hold the records the relink left,
	// by the parent it left each.
	childAt []int32
	kids    []chainHandle
	groupAt []int32
	group   []chainHandle
	// cp is the record the checkpoint names, where the relink left it.
	cp chainHandle
	// a and b are sets of records each step clears and fills, run y1t's run
	// of a batch's reply and the leaf set's members; met is the walk's and the
	// recovery's; replied the replies the recovery met.
	a, b, run, met, replied chainMarks
	// calls and taken are sets of call ids each step clears and fills: a
	// reply's calls, and those y1t's descent took, so two lists of ids compare
	// an id at a time, never one list walked per entry of the other.
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
// the later in the file first, as 2.1.322's vjt and 2.1.330's vtn order them
// (`reverse()`, then
// a stable sort by time); prev points to the next candidate down a walk has
// not met, compressed as the walk goes.
type chainSide struct {
	recs []chainHandle
	prev []int32
}

// chainCheckEvery is how many steps End takes between looks at its ctx, a
// power of two so the count is cheap. A step is a few array reads, so 4,096
// take well under a millisecond after the caller gives End up, and a look once
// in 4,096 steps costs nothing measurable. At the bounds
// (BenchmarkAThreadChainAtItsBounds) End's longest stretch without a look was
// 11 to 15 ms, each a pass that counts no step.
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
// every record of a main thread (DPe @322233162, ike @293792132, HMt
// @247175414, no checkpoint's walk), the least every reader hands: the walk up by
// parent, ended by a root, a parent naming no record or a record met; then,
// in the file's order, each record whose parent was reached before it as one
// that hands its children on, itself handing them on where X_e reads it as
// part of that parent's group (a reply's own id, else the parent's), and not
// where the parent is UF. Parents are as written: a path-free thread holds no
// boundary, so no relink moves one. Only the dispatch's group is read: the
// prune's is the dispatch's wherever that is a reply's. Beyond this the
// readers differ only in lines no step reads (attribution snapshots, reaction
// ledgers) and surrogate escapes.
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
	// w.b holds the reached records that hand their children on; groups each
	// one's group.
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

// relink is THE RELINK (2.1.330's C_r @322178624 with A_r, 2.1.322's iWo
// @293743389 with aWo, 2.1.310's ypo @247128053 with _po), in place on parents, and
// whether it chained kept records, which turns the leaf's pick from its single
// path. Where no boundary names what it kept, nothing happens. The newest that
// does keeps records only where it is the last (`w=s===h` @293743594;
// [threadChain.keptRecords]), chained under its anchor, the anchor's other
// children re-hung from the last kept (@293744084). Every record before the
// last boundary it does not keep is deleted (`Ce<h` @293744443): in no later
// step; where it kept any, a user or assistant record under a deleted one
// hangs from the last kept, any other names no record (@293744618). Every
// later step reads parents as it left them. One that would loop, or hang the
// kept under a record it does not keep, is whyRelinkShape.
func (c *threadChain) relink(ctx context.Context, w *chainWork) (bool, error) {
	if c.kept.boundary == noChainRecord {
		return false, nil
	}
	last := c.lastBoundary
	var kept []chainHandle
	if c.kept.boundary == last {
		var resolved bool
		var err error
		if kept, resolved, err = c.keptRecords(ctx, w); err != nil || !resolved {
			return false, err
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
		// keep is deleted, and one naming no record (noChainRecord) counts as
		// written before any: either leaves the first kept record under none, or
		// under the last kept once the deletion hangs it there. The last boundary
		// among the kept records is chained under its own anchor.
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
	// An anchor among the kept records, or below one of them by parent, hangs
	// the chain under itself: the walk up from the anchor meets a kept record.
	// The walk stops at a record it met, and counts its steps against ctx.
	w.b.clear()
	for at := anchor; at != noChainRecord && !w.b.has(int(at)); at = c.recs[at-1].parent {
		if err := w.tick(ctx); err != nil {
			return false, err
		}
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
// to its head (2.1.330's A_r @322179943, 2.1.322's aWo, 2.1.310's _po); and
// false where nothing happens:
// a listed uuid naming no record of the thread, or a walk that meets a root, a
// parent naming no record or a record it walked. The walk counts its steps
// against ctx.
func (c *threadChain) keptRecords(ctx context.Context, w *chainWork) ([]chainHandle, bool, error) {
	k := c.kept
	if !k.segment {
		if k.nowhere {
			return nil, false, nil
		}
		kept := make([]chainHandle, 0, len(k.list))
		for _, u := range k.list {
			h := c.index[u]
			if h == noChainRecord {
				return nil, false, nil
			}
			kept = append(kept, h)
		}
		return kept, true, nil
	}
	head := c.handleOf(k.head)
	var kept []chainHandle
	w.b.clear()
	for at := c.handleOf(k.tail); at != noChainRecord && !w.b.has(int(at)); at = c.recs[at-1].parent {
		if err := w.tick(ctx); err != nil {
			return nil, false, err
		}
		w.b.set(int(at))
		kept = append(kept, at)
		if at == head {
			slices.Reverse(kept)
			return kept, true, nil
		}
	}
	return nil, false, nil
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
	// Path-free ([threadChain.AnySize]); lastMarked is the main thread's
	// alone.
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
		// No record: no conversation, nothing handed under every variant
		// (2.1.322's Yya and 2.1.330's Lcr, `if(s.size===0)return null`; Rye
		// and lPe, `no_agent_messages`).
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
	var leaves chainLeaves
	if main {
		if leaves, err = c.pick(ctx, w, relinked); err != nil || c.why != "" {
			return err
		}
	} else {
		own := c.ownLeaf(w)
		if c.why != "" {
			return nil
		}
		leaves = chainLeaves{own, own, own}
	}
	groups := chainGroups(main, leaves)
	var walked []chainHandle
	for _, g := range groups {
		if !slices.Contains(walked, g.leaf) {
			walked = append(walked, g.leaf)
		}
	}
	for _, leaf := range walked {
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
	if len(walked) == 1 {
		c.leaf = walked[0]
	}
	return nil
}

// chainGroups is the groups of variants the thread's answers part into: the
// variants part where a reading of q1t moves the leaf and on each release's
// retraction filter, both the main thread's alone ([chainVariants]). Variants
// that walk from one leaf under one filter share a group, the first holding
// [variantDefault].
func chainGroups(main bool, leaves chainLeaves) []chainGroup {
	if !main {
		return []chainGroup{{leaf: leaves[leafPlain], variants: allVariants}}
	}
	groups := []chainGroup{{leaf: leaves[leafPlain], variants: allVariants &^ variants310 &^ variantsMetcalfe}}
	for _, r := range []chainLeafReading{leafQ1t322, leafQ1t330} {
		at := slices.IndexFunc(groups, func(g chainGroup) bool { return g.leaf == leaves[r] })
		if at < 0 {
			groups = append(groups, chainGroup{leaf: leaves[r]})
			at = len(groups) - 1
		}
		groups[at].variants |= r.variants()
	}
	return append(groups, chainGroup{leaf: leaves[leafPlain], variants: variants310})
}

// chainGroup is the variants that walk from one leaf under one release's
// retraction filter, what they hand, and why. The tails' order parts them once
// more (warm_mochi's from the rest), never in what they hand.
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

// resultByCall reports whether the thread holds a result foamy_spring may hang
// elsewhere than under its parent (D_r @322185645, fWo, Rpo): one whose
// call ids are not all of calls its parent made, where its
// sourceToolAssistantUUID names another record, or another reply made one of
// its calls. The second test widens fWo's (per call, the last reply holding
// it where all holding it share an id) to any reply but the parent, so the
// chain refuses no less than the resume hangs. ONE PASS, linear: a reply's
// calls are marked once, each result under it held to the marks an id at a
// time (walking ids per call would be 2^32 steps at 2^16).
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
			// A result whose parent is no reply answers no call of it.
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

// chainLeafReading is a reading of THE LEAF the variants part on: q1t off,
// and on as 2.1.322's metcalfe variants read it and as 2.1.330's
// ([threadChain.q1t]).
type chainLeafReading uint8

const (
	leafPlain chainLeafReading = iota
	leafQ1t322
	leafQ1t330
	leafReadings
)

// variants is the variants that read the leaf as r does.
func (r chainLeafReading) variants() chainVariants {
	switch r {
	case leafQ1t322:
		return variantsMetcalfe322
	case leafQ1t330:
		return variantsMetcalfe330
	}
	return allVariants &^ variantsMetcalfe
}

// chainLeaves is a record per reading of the leaf, by [chainLeafReading].
type chainLeaves [leafReadings]chainHandle

// pick is THE LEAF of the main thread (2.1.330's va @322267791 and Lcr
// @322283476, 2.1.322's Gr @293820883 and Yya @293834568, 2.1.310's kr
// @247196754 and m_i @247206356), a leaf per reading: noChainRecord where the
// resume finds no conversation. Not relinked, the single path's turn record
// ([threadChain.singlePath]); relinked, or where that finds none, the leaf set
// ([threadChain.leafSet]), narrowed where it holds more than one; then
// [threadChain.leafOf]. The hosted-environment clauses (2.1.322's NWo behind
// WA(), 2.1.330's Jrn @322253812 and the leaf set's behind cT()) never answer
// here.
func (c *threadChain) pick(ctx context.Context, w *chainWork, relinked bool) (chainLeaves, error) {
	last := c.lastRow
	// sets is each reading's leaf source: the single path's turn record where
	// found, else the leaf set.
	var sets [leafReadings][]chainHandle
	if !relinked {
		turns, err := c.singlePath(ctx, w)
		if err != nil || c.why != "" {
			return chainLeaves{}, err
		}
		for r, turn := range turns {
			if turn != noChainRecord {
				sets[r] = []chainHandle{turn}
			}
		}
	}
	if !slices.ContainsFunc(sets[:], func(set []chainHandle) bool { return set == nil }) {
		return c.leavesOf(sets), nil
	}
	hr, err := c.leafSet(ctx, w)
	if err != nil {
		return chainLeaves{}, err
	}
	var picks [leafReadings][]chainHandle
	for r := range picks {
		picks[r] = hr
	}
	if len(hr) > 1 {
		// Narrowed to one: up from the checkpoint, where it is in the set and
		// the last row descends through none of its batches (Y1t, never
		// directly) nor, in a reading of q1t, holds it; else up from the last
		// row (2.1.330's va @322269880, 2.1.322's Gr @293822608, 2.1.310's kr
		// @247198349, `uo&&Hr.has(uo)&&...?uo:Eo`).
		from := chainLeaves{last, last, last}
		if _, in := slices.BinarySearch(hr, w.cp); in && w.cp != last && !c.y1t(w, last, w.cp, false) {
			var read bool
			if from, read = c.fromCheckpoint(w, last); !read {
				return chainLeaves{}, nil
			}
		}
		for r := range picks {
			if turn := c.nearestTurn(w, from[r]); turn != noChainRecord {
				picks[r] = []chainHandle{turn}
			}
		}
	}
	for r := range sets {
		if sets[r] == nil {
			sets[r] = picks[r]
		}
	}
	return c.leavesOf(sets), nil
}

// leavesOf is [threadChain.leafOf] of each reading's set.
func (c *threadChain) leavesOf(sets [leafReadings][]chainHandle) chainLeaves {
	var leaves chainLeaves
	for r := range leaves {
		leaves[r] = c.leafOf(sets[r])
	}
	return leaves
}

// fromCheckpoint is where each reading walks up from where the checkpoint
// names a record the last row does not descend through: the checkpoint, or
// the last row where the reading's q1t holds. read is false where the thread
// is a cannot tell: a leaf rule of 2.1.330 the chain does not hold may fire
// (whyLeafRule), or q1t cannot read a reply's id (whyReplyID).
func (c *threadChain) fromCheckpoint(w *chainWork, last chainHandle) (from chainLeaves, read bool) {
	cp := w.cp
	from = chainLeaves{cp, cp, cp}
	if last != noChainRecord && c.leafRules(w, cp) {
		c.cannotTell(whyLeafRule)
		return from, false
	}
	for _, r := range []chainLeafReading{leafQ1t322, leafQ1t330} {
		fires, ok := c.q1t(w, last, cp, r == leafQ1t330)
		if !ok {
			c.cannotTell(whyReplyID)
			return from, false
		}
		if fires {
			from[r] = last
		}
	}
	return from, true
}

// singlePath is the turn record the resume's single path walks from, per
// reading: the start is the last row, or, where no checkpoint carries leafUuid
// and the newest row is not the last, [threadChain.newestUnder]; a checkpoint
// naming a record the relink left other than the last row replaces it, or the
// last row does where it descends from that record ([threadChain.y1t]) or, in
// a reading of q1t, q1t holds ([threadChain.fromCheckpoint]); then the nearest
// turn record up, none where the walk finds none.
func (c *threadChain) singlePath(ctx context.Context, w *chainWork) (turns chainLeaves, err error) {
	last := c.lastRow
	start := last
	if !c.anyLeafKey && c.newest != noChainRecord && c.newest != last {
		if start, err = c.newestUnder(ctx, w, last); err != nil {
			return chainLeaves{}, err
		}
	}
	from := chainLeaves{w.cp, w.cp, w.cp}
	if w.cp != noChainRecord && w.cp != last {
		if c.y1t(w, last, w.cp, true) {
			from = chainLeaves{last, last, last}
		} else {
			var read bool
			if from, read = c.fromCheckpoint(w, last); !read {
				return chainLeaves{}, nil
			}
		}
	}
	if from[leafPlain] == noChainRecord {
		from = chainLeaves{start, start, start}
	}
	for r := range turns {
		if r > 0 && from[r] == from[r-1] {
			turns[r] = turns[r-1]
			continue
		}
		turns[r] = c.nearestTurn(w, from[r])
	}
	return turns, nil
}

// leafSet is THE LEAF SET (2.1.330's Ctn @321998799, 2.1.322's Cjt
// @293590395, 2.1.310's loop in kr @247197836), in the file's order: from
// each record the relink left that
// none names as its parent, the first user or assistant record up by parent
// through records that are neither, where no user or assistant record names
// that one as its parent. Each answer is kept once walked, as Cjt keeps it:
// one walk of the thread.
func (c *threadChain) leafSet(ctx context.Context, w *chainWork) ([]chainHandle, error) {
	// w.a holds the records any record names as parent, w.b those a user or
	// assistant record names.
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
	// up is each record's answer, by handle: 0 not walked, upNone no turn
	// record (a root, a parent naming no record, or a loop), upWalking under
	// way, else the turn record's handle.
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
// Date.parse of its timestamp, the first in the file on a tie (2.1.330's lK
// @322180523, 2.1.322's cj @293745290, 2.1.310's p0 @247129929); where set is
// empty, and only there, the record of the latest time of every one the relink
// left, of any kind, the first on a tie (2.1.330's uan @322296800, 2.1.322's
// Ozt @293848378, 2.1.310's p0 in m_i); and none where it has no member. Among
// members none of which has a time, 2.1.330 takes the last (`g===-1/0`) and
// the others none, a parting the chain refuses (whyLeafTime).
func (c *threadChain) leafOf(set []chainHandle) chainHandle {
	if len(set) == 0 {
		for i, r := range c.recs {
			if r.kind&kindDeleted == 0 {
				set = append(set, chainHandle(i+1))
			}
		}
	}
	return c.latest(set)
}

// latest is the member of set of the latest time, the first in the file on a
// tie; none where set is empty, and none, the thread whyLeafTime, where no
// member has a time ([threadChain.leafOf]).
func (c *threadChain) latest(set []chainHandle) chainHandle {
	leaf := noChainRecord
	for _, h := range set {
		if r := c.recs[h-1]; r.kind&kindTimed != 0 && (leaf == noChainRecord || r.ms > c.recs[leaf-1].ms) {
			leaf = h
		}
	}
	if leaf == noChainRecord && len(set) > 0 {
		c.cannotTell(whyLeafTime)
	}
	return leaf
}

// ownLeaf is the leaf of A SUBAGENT'S OWN LOAD (2.1.330's lPe @322293883,
// 2.1.322's Rye @293845538, 2.1.310's FBe @247216758), which loads the file
// with the main thread's loader, relink included, and reads none of its pick:
// of its OWN LEAVES, the records of its own ([threadChain.own]) no record of
// its own names as parent, a boundary aside, the one of the latest time, the
// first in the file on a tie ([threadChain.latest]); none where it has no
// record (`no_agent_messages`). From it the load walks, recovers and tails
// over every record the relink left and hands on its agentId's alone
// ([threadChain.judge]); a fork's reference prefixes the parent's
// conversation, so the chain refuses any (whyForkContext). It files the own
// leaves in order.
func (c *threadChain) ownLeaf(w *chainWork) chainHandle {
	// w.a holds the records an own record names as parent.
	w.a.clear()
	for i, r := range c.recs {
		if r.kind&kindDeleted == 0 && c.own(chainHandle(i+1)) && r.parent != noChainRecord {
			w.a.set(int(r.parent))
		}
	}
	for i, r := range c.recs {
		h := chainHandle(i + 1)
		if r.kind&(kindDeleted|kindBoundary) != 0 || !c.own(h) || w.a.has(int(h)) {
			continue
		}
		c.ownLeaves = append(c.ownLeaves, h)
	}
	return c.latest(c.ownLeaves)
}

// own reports whether record h is one of the subagent's own: of its agentId,
// and a sidechain's (`tt.agentId===e&&tt.isSidechain`).
func (c *threadChain) own(h chainHandle) bool { return c.is(h, kindAgent|kindSideTrue) }

// nearestTurn is the nearest user or assistant record up from from by
// parent, from itself; none where the walk meets a root, a parent naming no
// record, or a record twice (2.1.330's va, 2.1.322's Gr, 2.1.310's kr).
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
// where none is (2.1.330's va, 2.1.322's Gr and 2.1.310's kr, compared as strings,
// `>=`). One pass: last's descendants marked over the children index.
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
// names another record (2.1.330's isn @322255959 with ltt, 2.1.322's Y1t
// @293812412 with X1t, 2.1.310's WPt @247189354 with GPt; throughBatch
// always, since no marked checkpoint is modelled): the last row descends through the checkpoint's batch, the descent
// up from the checkpoint through UF records to a batch's results, or to the
// call its hook's attachment names, met by the walk up from the last row
// through another result or record of that batch's reply; or, where direct, as
// the single path asks and the leaf set's pick does not (`direct:!1`), from the
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
		// The batch's call, by the attachment toolUseID string the hook's last
		// record names: the first reply above, or in its run, whose calls include it.
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
	// The batch reply's calls and those the descent took, as sets.
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

// answersAnother reports whether below, the record under the batch's reply on
// the walk up, is one of its results answering a call other than the
// checkpoint's (`aj(ne).some(...)`): any of its calls (w.calls) but those taken
// (w.taken), or, where found by its hook, any where the reply made more than
// one (uses).
func (c *threadChain) answersAnother(w *chainWork, below chainHandle, uses int, byHook bool) bool {
	for _, x := range c.resultsOf(below) {
		if w.calls.has(int(x)) && (byHook && uses > 1 || !byHook && !w.taken.has(int(x))) {
			return true
		}
	}
	return false
}

// x1t is the run up from from's parent of records part of reply id's
// (2.1.330's ltt @322258040, 2.1.322's X1t @293813630, 2.1.310's GPt
// @247190569), each met once.
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

// q1t reports whether q1t holds of the last row and the checkpoint, as a
// reading of the metcalfe variants reads it: 2.1.322's q1t (@293811681), or,
// throughSystem, 2.1.330's (tsn @322254354, through Yin @322255016). The
// checkpoint is a reply that made a call; above its run (it and the records of
// its own id it hangs from) lies a record; the walk up from the last row
// reaches that record, or, throughSystem, a system record it hangs from
// through system records alone, and of the records it passes outside the run,
// the topmost (`K`) was written later than the checkpoint, and one is a reply
// saying something or making a call (LE @293601245, eC @322012881); no record
// hangs off the checkpoint, and none off the last row but one that hangs off a
// reply. read is false where the run turns on a reply's id the layer reads as
// none, which may be absent (the run stops) or empty (it goes on).
func (c *threadChain) q1t(w *chainWork, last, cp chainHandle, throughSystem bool) (fires, read bool) {
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
	// w.run holds where the walk stops: above, and, throughSystem, the records
	// it hangs from through system records alone.
	w.run.clear()
	w.run.set(int(above))
	for k := above; throughSystem && c.is(k, kindSystem) && c.recs[k-1].parent != noChainRecord; {
		k = c.recs[k-1].parent
		if w.run.has(int(k)) {
			break
		}
		w.run.set(int(k))
	}
	w.b.clear()
	var later chainHandle
	speaks := false
	at := last
	for at != noChainRecord && !w.run.has(int(at)) {
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

// leafRules reports whether a leaf rule of 2.1.330 that the chain does not
// hold may move the leaf from the checkpoint to the last row, where the last
// row does not descend through it ([threadChain.y1t]): rsn (@322254537,
// behind tengu_spicy_oboe, off), where the checkpoint, up through system
// records, is a reply saying nothing and making no call; or asn (@322257185,
// behind foamy_spring alone), where the checkpoint is a reply of an id,
// making no call, whose run up ([threadChain.x1t]) holds an attachment or
// system record below a record of its own reply, and no user record there.
// Each is the wider reading of its rule's first conditions, so a thread either
// may fire on is refused (whyLeafRule).
func (c *threadChain) leafRules(w *chainWork, cp chainHandle) bool {
	y := cp
	for steps := 0; c.is(y, kindSystem) && c.recs[y-1].parent != noChainRecord && steps < len(c.recs); steps++ {
		y = c.recs[y-1].parent
	}
	if c.is(y, kindAssistant) && !c.is(y, kindUse) && !c.is(y, kindSays) {
		return true
	}
	id := c.recs[cp-1].reply
	if !c.is(cp, kindAssistant) || c.is(cp, kindUse) || id == 0 {
		return false
	}
	run := c.x1t(w, cp, id)
	top := -1
	for i, h := range run {
		if c.is(h, kindUse) {
			return false
		}
		if c.is(h, kindAssistant) {
			top = i
		}
	}
	between := false
	for _, h := range run[:max(top, 0)] {
		switch {
		case c.is(h, kindUser):
			return false
		case !c.is(h, kindAssistant):
			between = true
		}
	}
	return between
}

// speaks reports whether record h is a reply its model wrote that makes a
// call or says something, as 2.1.322's LE and 2.1.330's eC class one
// assistant_calls or assistant_text.
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
// before the retraction filter: base, the walk with the recovery's records
// placed in it; tails, what the tails hang under the leaf in warm_mochi's
// order, bfs the same in the other order (nil where they are one); and how
// each was reached, by handle.
type chainList struct {
	base, tails, bfs []chainHandle
	how              []uint8
}

// listFrom is what the resume's walk, recovery and tails hand from leaf.
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

// tails is THE TAILS (2.1.330's P_r @322182457, 2.1.322's lWo, 2.1.310's kpo),
// run after the recovery with what it and the walk met in w.met: the records
// under leaf that are neither user nor assistant records, a boundary among
// them, through records that are neither, a met record ending its branch,
// handed after all the walk and the recovery hand. dfs is warm_mochi's order,
// a walk by depth with each record's children sorted by time; bfs the order
// without it, a walk by breadth sorted by time together, nil where the two are
// one. Each record has one parent, so both find the same records. Where a sort holds no order
// ([threadChain.byTime]) the thread is whyTailTime; where the walk by breadth
// finds more than threadChainMaxSpread, whySpread.
func (c *threadChain) tails(ctx context.Context, w *chainWork, leaf chainHandle) ([]chainHandle, []chainHandle, error) {
	// Breadth first, in w.a, so w.met is still what the walk and the recovery
	// left for the walk by depth.
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
	// The walk by depth, as the resume makes it: a record popped and already
	// met is passed over with what lies under it; children are pushed latest
	// first, so the earliest pops first.
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

// byTime sorts records as the tails' sorts do, stably by timestamp compared
// as a string (`H.timestamp<K.timestamp` @322182502, @293747102, @247131695),
// and reports
// whether that orders them at all: it reads a record of no time as equal to
// every record, so beside two of different times such a record leaves their
// order to the engine's sort algorithm, and beside records of one time leaves
// all as they came. On the one admitted form the string's order is its
// milliseconds'.
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

// walk is THE WALK (2.1.330's DIn @322180717, 2.1.322's A7t @293745408,
// 2.1.310's w$t @247130047): the
// chain from leaf by parent to a root, taking [threadChain.fallback] where a
// parent names no record or one walked and stopping where it finds none; root
// first, its records met.
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
		// A record met already ends the walk, as A7t's guard does
		// (`if(h.has(S.uuid))...break`).
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
// record or one walked already (2.1.330's vtn @321997874, 2.1.322's vjt
// @293589456, 2.1.310's Epo @247132981): of at's isSidechain as written, timed and not met, the latest
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

// side is the fallback's candidates for a record of kind k's isSidechain,
// built once from the records the relink left.
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
// records the recovery places among its walked ones by file place: those
// recovered beside the reply, and those hung under a record of the reply or a
// result beside it ([threadChain.hang]).
type chainWindow struct {
	start, end int
	recovered  []chainHandle
}

// chainAnchored is a record hung under the walked record at chain[at], which
// the resume places right after it (`Ot.push(...an.get(so.uuid))`).
type chainAnchored struct {
	at     int32
	record chainHandle
}

// recover is THE RECOVERY (2.1.330's D_r @322185645, 2.1.322's fWo, 2.1.310's
// Rpo): chain with what it reaches placed in it, how each was reached
// into how. For each reply on the walk, at its id's first meeting, every other
// record of the id and every result under one, not met before, is recovered;
// its window runs past the last record of its id on the walk and on over what
// [threadChain.partOf] reads as part of it, and opens where it recovered or
// [threadChain.hang] hung a record. Overlapping windows merge (@322189198),
// one walked record's hung lists joining, the earlier window's first. A merged
// window's recovered records are sorted by file place once: no two share one,
// so that is the resume's order without its quadratic cost
// (BenchmarkAThreadChainAtItsBounds: End 3.3 s vs 1.4 s). A walk, or a walked
// record's hung list, past [threadChainMaxSpread] is whySpread.
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
	// Each walked record's hung records join into one list, in the order
	// hung: every window holding that record merges.
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
// where a record of the reply made a call (`co` in D_r, fWo and Rpo; CPe
// @322184473, bVe @293748333): under each record of the reply, then each met result under one
// of them, then each walked record of the window past its first that is UF,
// what [threadChain.pye] hangs from each child in turn, each record hung met
// at once (`r.add(Bo.uuid)` @293752104, @322188474), so it blocks a later window, and
// reached as whyHookChain. What hangs under a record of the reply or a result
// joins recovered, placed by file order before the first walked record
// written after it; what hangs under a walked record goes to anchored, right
// after it. Each window takes children afresh, as the resume does: a record
// with two children not met can hang in a later window once a recovery met one.
func (c *threadChain) hang(ctx context.Context, w *chainWork, chain []chainHandle, id chainReplyID, start, end int,
	recovered []chainHandle, anchored []chainAnchored, how []uint8) ([]chainHandle, []chainAnchored, error) {
	group := w.records(id)
	if !slices.ContainsFunc(group, func(g chainHandle) bool { return c.is(g, kindUse) }) {
		return recovered, anchored, nil
	}
	hook := whyAt(whyHookChain)
	// under appends to into what pye hands from each of u's children, met at once.
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
// appended to into (2.1.330's Cxe @321995019, 2.1.322's pye, 2.1.310's cce): y
// and, after each record, its one child not met, while each is a UF record of
// u's isSidechain as written and not met; none at all where one is not, or
// where one has two or more children not met. A record has one parent, so the
// chain runs down u's subtree and meets no record twice: a cycle through y would run through u, which is
// met.
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

// judge decides each record's answer under g's variants: deleted by the
// relink, not handed; reached by the walk, the recovery or the tails, handed.
// In the main thread, THE RETRACTION FILTER (2.1.330's hPn @324043599,
// 2.1.322's eGe, 2.1.310's qGn) then drops every non-system record whose uuid's
// first 24 characters a record of the list names
// ([threadChain.readsRetractions], [chainPrefix]); THE MALFORMED-ATTACHMENT
// DROP (rDe @324006483, Fce, dne, the same code) then drops each attachment
// [threadChain.file] marked kindIllFormed, that record alone. Each release's
// deserializer reruns the two (F2n @324017533, e_n @295266070, TJt),
// taking nothing more. In a subagent's thread neither runs, and another
// agentId's records are whyAnotherAgent. Its orders are what it hands for
// certain, per tails order.
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
	// handed is the list, the tails in warm_mochi's order (the same records).
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
	// Malformed attachments drop after the filter, so a record the filter took
	// keeps its reason; file marks a main thread's alone.
	for _, h := range handed {
		if v := &g.verdicts[h-1]; v.handed == verdictYes && c.is(h, kindAttachment|kindIllFormed) {
			*v = chainVerdict{handed: verdictNo, why: whyAt(whyMalformedAttachment)}
		}
	}
	if !main {
		// A subagent's load hands on only what is of its agentId, whatever its side.
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
// variant's reason; and handedUnknown where the groups part, whyLeafFlag where
// a group walking from another leaf than the default's parts from it and else
// whyRelease. A group's answers are definite
// (judge answers handed or not handed alone), so cannot tell comes only from
// that parting. Each distinct order is kept once, with the variants that hand
// it, the default variant's first: a group's warm_mochi variants hand its
// order, the rest its order with the tails sorted after a walk by breadth.
func (c *threadChain) merge(groups []chainGroup) {
	def := slices.IndexFunc(groups, func(g chainGroup) bool { return g.variants&variantDefault != 0 })
	c.verdicts = make([]chainVerdict, len(c.recs))
	for i := range c.verdicts {
		var yes, no, leafFlag bool
		for _, g := range groups {
			if g.verdicts[i].handed == verdictYes {
				yes = true
			} else {
				no = true
			}
			leafFlag = leafFlag || g.leaf != groups[def].leaf && g.verdicts[i].handed != groups[def].verdicts[i].handed
		}
		switch {
		case yes && no && leafFlag:
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

// order files records as the order variants hand, where any does: added to an
// existing order of the same records, else a distinct order.
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
