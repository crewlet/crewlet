package executor

import (
	"encoding/json"
	"fmt"
)

// WHAT A THREAD LINE'S ACCESSORS READ: ITS FACTS.
//
// A line's accessors ([threadLine]) read the line's facts ([lineFacts]),
// never the line: each fact filled ONCE, as the line is read, by the one
// producer that reads it ([factsOf], over the line's whole decode), and
// handed out on every call of its accessor as it was filled. So a reader that
// asks twice pays for one reading, and what an accessor answers is what the
// facts say, whichever producer filled them.
//
// A FACT IS ONE OF FOUR SHAPES, by what its readers read of the value:
//
//   - A SMALL VALUE AS WRITTEN, its raw (a uuid, a timestamp, a reply's usage,
//     an attachment's type), which its accessor holds to its type and reads
//     by the layer's own code at each call, as it read the line's raw before.
//   - A VALUE'S KIND ([valueKind]) where its readers ask only what kind of
//     JSON it is (an attachment's content is a string, or an object).
//   - A FRAME'S ANSWERS: of a container its readers walk (a message's content,
//     an attachment's lists, a record's own toolUseResult), what each reader
//     asks of it, read by that reader's own rule, and the error that rule
//     meets, worded as the accessor words it.
//   - A LIST'S ENTRIES, each read by its rule, in order, up to the first that
//     fails it, with that failure: its accessor replays them and then answers
//     the failure, so a reader is handed what a walk of the list handed it
//     before it met the failure, and no entry after it.
//
// A FRAME ITS RECORD'S TYPE DOES NOT READ IS EMPTY: a message is filled on a
// user or an assistant record alone, a toolUseResult on a user record, an
// attachment on an attachment record, compactMetadata on a compaction's
// boundary, retractedMessageUuids on a user, an assistant or a refusal's
// fallback, replacements on a content-replacement record and batchToolUses on
// an assistant record: the records each accessor answers on. Every other
// record's accessor answers as it does on a record without the key, so what
// the line holds there is never read.
//
// ONE LINE'S FACTS ARE ITS OWN: nothing of them is shared with another line
// or reused by the reader's next line, so a line's facts stay valid however
// long its reader holds the line. A nil facts is a line of no keys
// ([threadLine.facts]), so the zero line answers as a record of none.

// lineFacts is what a line's accessors read of it ([threadLine]), filled once
// as the line is read: the record's small values as written, and per frame
// its answers.
type lineFacts struct {
	// The record's small values each accessor reads by the layer's own code
	// ([threadLine.UUID], [threadLine.Timestamp]...): each as written, nil
	// where the key is absent.
	uuid, version, origin, apiError, leafUUID, explicit, rewound json.RawMessage
	isVirtual, interruptedByShutdown, isCompactSummary           json.RawMessage
	timestamp, isMeta, sourceToolAssistantUUID                   json.RawMessage
	// taskDelivery is whether the record carries `taskDelivery`, whatever it
	// holds ([threadLine.TaskDelivery]).
	taskDelivery bool

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

// facts is the line's facts, or those of a line of no keys where the line
// holds none, as the zero line does.
func (l threadLine) facts() *lineFacts {
	if l.f == nil {
		return &noLineFacts
	}
	return l.f
}

// messageFacts is a user or assistant record's `message`: its kind, the small
// values its accessors read as written, and its content, read once
// ([threadLine.Message]).
type messageFacts struct {
	// kind is the message's kind; every accessor of a message refuses one that
	// is no object.
	kind valueKind
	// err is what the message's object met as it was read, before any key of
	// it: every accessor of the message answers it.
	err error
	// id, model, stopReason, usage and contextManagement are the message's
	// `id`, `model`, `stop_reason`, `usage` and `context_management`, as
	// written ([threadLine.MessageID], [threadLine.Model],
	// [threadLine.StopReason], [threadLine.Usage],
	// [threadLine.ToolResultsCleared]).
	id, model, stopReason, usage, contextManagement json.RawMessage
	// content is the message its `content` makes, its calls of the kind its
	// record makes, and contentErr what reading it met
	// ([threadLine.Message]), neither naming the record.
	content    threadMessage
	contentErr error
}

// object is the error every accessor of the message answers before it reads a
// key of it, record what it calls the record: the message no object, or what
// its object met; nil where it is one.
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

// attachmentFacts is an attachment record's `attachment`: its kind, the small
// values its accessors read as written, and what each attachment's shape
// rule asks of its lists ([threadLine.AttachmentWellFormed]).
type attachmentFacts struct {
	// kind is the attachment's kind, and err what its object met as it was
	// read, before any key of it.
	kind valueKind
	err  error
	// typ, toolUseID and by are its `type`, `toolUseID` and `by`, as written.
	typ, toolUseID, by json.RawMessage
	// content is its `content`'s kind; contentStrings and contentObjects
	// whether that is a list whose every element is a string, or an object.
	content                        valueKind
	contentStrings, contentObjects bool
	// skillsShaped is whether `skills` is a list whose every element is an
	// object or a list.
	skillsShaped bool
	// namesKept and namesErr are what the skill listing's rule answers of
	// `names` ([skillNamesKept]).
	namesKept bool
	namesErr  error
	// entriesKept is whether `entries` is a list whose every element is an
	// entry a withheld memory keeps ([withheldKept]).
	entriesKept bool
}

// compactFacts is a compaction's boundary's `compactMetadata`
// ([threadLine.Preserved]): its kind, what its object met, its
// `preservedMessages`' kind and the list's answer ([preservedList]), and its
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
	// absent key among them.
	kind valueKind
	// count is how many elements the list holds, whatever each is.
	count int
	// ids is the toolUseId of each entry that replaces a tool's result, in
	// order, up to the first entry its rule refuses ([replacedID]); err is
	// that entry's error.
	ids []string
	err error
}

// list is the error the accessors of the list answer before anything of it:
// the key no list; nil where it is one.
func (r *replacementFacts) list() error {
	if r.kind != '[' {
		return fmt.Errorf("is %s, not a list", r.kind)
	}
	return nil
}

// batchFacts is an assistant record's `batchToolUses`
// ([threadLine.BatchToolUses]).
type batchFacts struct {
	// present is whether the record carries the key.
	present bool
	// ids is each entry's id, in order, up to the first entry its rule
	// refuses ([batchEntryID]); err is that entry's error, or the list's own.
	ids []string
	err error
}
