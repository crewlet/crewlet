package executor

import (
	"encoding/json"
	"fmt"
)

// A thread line's accessors ([threadLine]) read its facts ([lineFacts]), never
// the line: each fact is filled ONCE, in the line's one pass
// ([threadReader.Next]), so a reader that asks twice pays for one reading. A
// fact is a small value as written (its accessor holds it to its type), a
// value's kind ([valueKind]), a frame's answers (what each reader's rule reads
// of a container or a text, the asked takes a hook's words name among them,
// with the error it meets, worded as its accessor words it), or a list's
// entries up to the first that fails and that failure, nothing after.
//
// A frame its record's type does not read ([gates] names which) is empty, its
// accessor answering as on a record without the key. A line's facts are its
// own, valid while the line is held; nil facts are a line of no keys.

// lineFacts is what a line's accessors read of it ([threadLine]), filled once
// in the line's one pass.
type lineFacts struct {
	// The record's small values as written, nil where the key is absent.
	uuid, version, origin, apiError, leafUUID, explicit, rewound json.RawMessage
	isVirtual, interruptedByShutdown, isCompactSummary           json.RawMessage
	timestamp, isMeta, sourceToolAssistantUUID                   json.RawMessage
	// taskDelivery is whether the record carries `taskDelivery`, whatever it
	// holds.
	taskDelivery bool
	// putBack is whether the record carries `rowsAheadOfPutBack`, whatever it
	// holds.
	putBack bool
	// estimating is whether the line's reader asked for its estimate
	// ([threadReader.Estimating]), which the estimate's accessors answer
	// [errNotEstimated] without.
	estimating bool

	message      messageFacts
	result       toolResultFacts
	attachment   attachmentFacts
	compacted    compactFacts
	replacements replacementFacts
	batch        batchFacts
	// retracted and retractedErr are [threadLine.Retracted]'s answer.
	retracted    threadRetracted
	retractedErr error
}

// noLineFacts is the facts of a line of no keys ([threadLine.facts]).
var noLineFacts lineFacts

// facts is the line's facts, or [noLineFacts] where it holds none, as the zero
// line does.
func (l threadLine) facts() *lineFacts {
	if l.f == nil {
		return &noLineFacts
	}
	return l.f
}

// messageFacts is a user or assistant record's `message`, read once
// ([threadLine.Message]).
type messageFacts struct {
	// kind is the message's kind; every accessor refuses one that is no object.
	kind valueKind
	// err is what decoding the message's object met: every accessor answers it.
	err error
	// id, model, stopReason, usage and contextManagement are the message's
	// `id`, `model`, `stop_reason`, `usage` and `context_management`, as
	// written.
	id, model, stopReason, usage, contextManagement json.RawMessage
	// content is the message its `content` makes, holding calls of the kind its
	// record makes, and contentErr what reading it met; neither names the
	// record.
	content    threadMessage
	contentErr error
	// charge is its content's charge, and emptyText whether that content is
	// the empty string, as the line's pass read them. estimate is what Claude
	// Code estimates the record at, and estimateErr why its estimate does not
	// read ([threadLine.Estimate]), from them and the images the record's task
	// delivery carries once the line ends ([lineDecoder.finishEstimate]):
	// filled only of an object whose content its rule reads, so not where
	// contentErr is, and only for a reader that asked.
	charge      estimateCharge
	emptyText   bool
	estimate    threadEstimate
	estimateErr error
}

// object is the error every accessor of the message answers before reading a
// key of it, naming the record as record: the message is no object, or what
// decoding it met; nil where it is a well-read object.
func (m *messageFacts) object(record string) error {
	if m.kind != '{' {
		return fmt.Errorf("%s's message is %s, not an object", record, m.kind)
	}
	if m.err != nil {
		return fmt.Errorf("%s's message: %w", record, m.err)
	}
	return nil
}

// toolResultFacts is what a user record's own `toolUseResult` says
// ([threadLine.HandsBack], [threadLine.AgentAnswer]).
type toolResultFacts struct {
	handsBack bool
	completed bool
	agent     string
	refused   bool
}

// attachmentFacts is an attachment record's `attachment`: its small values as
// written, and what each attachment type's shape rule asks of its lists
// ([threadLine.AttachmentWellFormed]).
type attachmentFacts struct {
	// kind is the attachment's kind, and err what decoding its object met.
	kind valueKind
	err  error
	// typ, toolUseID, by and event are its `type`, `toolUseID`, `by` and
	// `hookEvent`, as written.
	typ, toolUseID, by, event json.RawMessage
	// contentTokens and stdoutTokens are which asked take tokens
	// ([threadReader.Asking]) a hook record's `content` ([hookRecordPrefix]),
	// and a hook_success's `stdout`, name where each is a string
	// ([textTakes]): bit i for token i. A uint64, the width an ask is held to
	// ([maxAskedTakes]), which the row's own bounds stay under.
	contentTokens, stdoutTokens uint64
	// content is its `content`'s kind; contentStrings and contentObjects
	// whether that is a list whose every element is a string, or an object.
	content                        valueKind
	contentStrings, contentObjects bool
	// skillsShaped is whether `skills` is a list whose every element is an
	// object or a list.
	skillsShaped bool
	// namesKept and namesErr are what the skill listing's rule answers of
	// `names` ([lineDecoder.skillNames]).
	namesKept bool
	namesErr  error
	// entriesKept is whether `entries` is a list whose every element is an
	// entry a withheld memory keeps ([withheldKept]).
	entriesKept bool
	// pdf is its `reads` as a PDF read's pages, and images how many images
	// its `taskDelivery` carries, as the line's pass read them. estimate is
	// what Claude Code estimates the record at, by what the record stores as
	// its rendering or, of a PDF read, its pages, and estimated whether that
	// reads ([threadLine.AttachmentEstimate]), once the line ends
	// ([lineDecoder.finishEstimate]); only for a reader that asked.
	pdf       pdfPages
	images    int64
	estimate  threadEstimate
	estimated bool
}

// compactFacts is a compaction boundary's `compactMetadata`
// ([threadLine.Preserved]): its kind and decode error, `preservedMessages`'
// kind and the list's answer ([lineDecoder.preservedMessages]), and
// `preservedSegment` as written.
type compactFacts struct {
	kind     valueKind
	err      error
	messages valueKind
	list     threadPreserved
	listErr  error
	segment  json.RawMessage
}

// replacementFacts is a content-replacement record's `replacements`
// ([threadLine.ReplacedResults], [threadLine.ReplacementCount]).
type replacementFacts struct {
	// kind is the key's kind: every accessor refuses one that is no list, an
	// absent key included.
	kind valueKind
	// count is the list's length, whatever each element is.
	count int
	// ids is the toolUseId of each entry that replaces a tool's result, in
	// order, up to the first entry its rule refuses ([replacedID]); err is
	// that entry's error.
	ids []string
	err error
}

// list is the error the list's accessors answer first, where the key is no
// list; nil where it is one.
func (r *replacementFacts) list() error {
	if r.kind != '[' {
		return fmt.Errorf("is %s, not a list", r.kind)
	}
	return nil
}

// batchFacts is an assistant record's `batchToolUses`
// ([threadLine.BatchToolUses]).
type batchFacts struct {
	present bool
	// ids is each entry's id, in order, up to the first entry its rule
	// refuses ([batchEntryID]); err is that entry's error, or the list's own.
	ids []string
	err error
}
