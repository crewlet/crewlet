package types

import (
	"slices"
	"unicode/utf8"
)

// WrittenRecord is one record a turn's committed write created or wrote, named
// AS A WAKE ABOUT IT NAMES IT, so whether a wake is about what a running run
// wrote is an exact compare, never a likeness (internal/engine's router,
// ADR-0167): a work item by its [WorkItem.Ref], the identity a wake about the
// item is charged by; every other record by the conversation identity a wake
// about it is stamped with (internal/notify's ConversationIdentityOf): a
// change's, a chat thread's, a page's, an artifact's. The json tags are a wire
// format.
type WrittenRecord struct {
	Kind WrittenKind `json:"kind"`
	ID   string      `json:"id"`
}

// WrittenKind is which identity a [WrittenRecord] is named by.
type WrittenKind string

const (
	// WrittenItem is a work item, by its [WorkItem.Ref].
	WrittenItem WrittenKind = "item"
	// WrittenConversation is a record a wake names by its conversation
	// identity, namespaced by its source.
	WrittenConversation WrittenKind = "conversation"
)

// WrittenKinds is the closed set.
var WrittenKinds = []WrittenKind{WrittenItem, WrittenConversation}

// Valid reports whether k is one of [WrittenKinds].
func (k WrittenKind) Valid() bool { return slices.Contains(WrittenKinds, k) }

// MaxWrittenIDBytes bounds a [WrittenRecord]'s id. An identity is a source's
// name and ids (`chatlet:<room>:<thread>`, `gitlet:change:<id>`, a work item's
// `<backend>:<id>`), well under a hundred bytes; 256 leaves a source with
// longer ids room. A record past it is not recorded, NEVER CUT: two identities
// cut alike would name two records as one.
const MaxWrittenIDBytes = 256

// MaxWrittenRecords bounds a set of [WrittenRecord]s: what one run's calls
// wrote, as its log keeps it (internal/sandbox's BridgeLog.Wrote), and what one
// turn's set holds (internal/agent/turnctx's Written). 128 IS A BULK
// OPERATION'S THRESHOLD, the figure turnctx's MaxWritten defends for items: a
// run of hours writes to a handful of threads, items and changes, and one past
// 128 is a sweep. At it, every id at [MaxWrittenIDBytes] in the bytes that
// escape the most, the set weighs about 200 KiB of the 1.69 MiB the log leaves
// beside its calls (sandbox.LogRestBytes, which its budget test measures). A
// set at its bound takes no more and drops nothing it holds.
const MaxWrittenRecords = 128

// ItemWritten is item as a written record, the zero record for an item with no
// id, which names nothing.
func ItemWritten(item WorkItem) WrittenRecord {
	if item.ID == "" {
		return WrittenRecord{}
	}
	return WrittenRecord{Kind: WrittenItem, ID: item.Ref()}
}

// ConversationWritten is the record a wake stamped with conversation identity
// names, the zero record for none.
func ConversationWritten(identity string) WrittenRecord {
	if identity == "" {
		return WrittenRecord{}
	}
	return WrittenRecord{Kind: WrittenConversation, ID: identity}
}

// Valid reports whether r is a record a set holds: a known kind and an id of
// valid UTF-8 within [MaxWrittenIDBytes].
func (r WrittenRecord) Valid() bool {
	return r.Kind.Valid() && r.ID != "" && len(r.ID) <= MaxWrittenIDBytes && utf8.ValidString(r.ID)
}

// FoldWritten is have with more taken in: each valid record once, in
// first-write order, while the set holds fewer than [MaxWrittenRecords]; full
// reports a valid record left out for want of room. Nothing have holds is
// dropped, and have's own array is never written.
func FoldWritten(have []WrittenRecord, more ...WrittenRecord) (out []WrittenRecord, full bool) {
	out = have
	for _, r := range more {
		switch {
		case !r.Valid() || slices.Contains(out, r):
			continue
		case len(out) >= MaxWrittenRecords:
			full = true
			continue
		}
		out = append(slices.Clip(out), r)
	}
	return out, full
}
