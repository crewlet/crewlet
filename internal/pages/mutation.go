package pages

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TYPED PAYLOADS, one per (kind, op).
//
// A small whole-document object — a container, a title claim, a gate — travels
// as FULL POST-STATE: one document field, one upsert, and no patch semantics
// to get wrong. A PAGE travels as a TYPED PATCH, because full post-state would
// put a 512 KiB body on the wire for a label change.
//
// The patch's rule is the tracker's, and it is the right one: a nil field is
// unchanged, and the POINTER is what tells "set this to empty" from "leave it
// alone" — which a plain string cannot. Two rules travel with it:
//
//  1. A named scalar present in the patch carries its COMPLETE new value,
//     never an excerpt and never a diff. The 600-byte excerpts live in the
//     notification, where a wake prompt is what they are for.
//  2. A collection the write TOUCHES is carried whole; one it does not touch
//     is absent. That is what makes a record able to rebuild the row, and it
//     is affordable because every collection here is capped.

// CreatePayload makes a page. Its subject is the TITLE.
//
// The whole create is ONE record: the applier writes the title row, the page
// head, its first revision and the history entry in one transaction. That is
// what removes the create's two-key sequence — there is no orphan claim,
// because there is no window in which a title is held by a page that was never
// written.
type CreatePayload struct {
	V int `json:"v"`

	// PageID is the uuid the applier files the head under. MINTED BY THE
	// WRITER and carried, never derived: a create that lost its race must
	// not have written a page under an id the winner also chose.
	PageID string `json:"page_id"`

	// Container and Title are the address. Title is the DISPLAYED form,
	// keeping the author's own capitalisation; the subject carries the
	// normalised one it was arbitrated on.
	Container string `json:"container"`
	Title     string `json:"title"`

	ParentID string   `json:"parent_id,omitempty"`
	Body     string   `json:"body,omitempty"`
	Status   Status   `json:"status"`
	Labels   []string `json:"labels,omitempty"`
	Watchers []string `json:"watchers,omitempty"`

	Author string `json:"author,omitempty"`
}

// RenamePayload moves a page to a new address. Its subject is the NEW title.
//
// It carries the OLD title because the apply releases that claim, and a
// release the record did not state would be a row deleted on one node's
// authority rather than the log's.
type RenamePayload struct {
	V int `json:"v"`

	PageID string `json:"page_id"`

	// Container and Title are the NEW address, and the two the subject was
	// arbitrated on — the applier recomputes the subject from them and
	// refuses a record that took one address and claims another.
	Container string `json:"container"`
	Title     string `json:"title"`

	// FormerContainer and FormerTitle are the claim this record RELEASES.
	// Both, because a rename may move a page between spaces, and a release
	// that assumed the old container was the new one would leave the
	// original address held for ever.
	//
	// The old TOKEN is not carried: it is a pure function of the title,
	// and a stated one would be a second value that could disagree with
	// the title beside it.
	FormerContainer string `json:"former_container"`
	FormerTitle     string `json:"former_title"`
}

// PagePatch is a change to a page head. Every field is a pointer or a
// collection, and absent means unchanged.
type PagePatch struct {
	V int `json:"v"`

	// A save states the version it edited — the broker enforces it, as
	// the page's own arbitration anchor — and the applier writes an
	// immutable revision AT THE VERSION THIS SAVE PRODUCES, in the same
	// transaction. So every version has exactly one revision row and
	// "open version 7" works for the newest as well as the oldest.
	Body    *string `json:"body,omitempty"`
	Message *string `json:"message,omitempty"`

	ParentID *string `json:"parent_id,omitempty"`
	Status   *Status `json:"status,omitempty"`

	// Labels, Watchers and Muted are carried WHOLE when touched, absent
	// when not. A delta map could not represent a write that replaces the
	// set, and every one of them is capped.
	Labels   []string `json:"labels,omitempty"`
	Watchers []string `json:"watchers,omitempty"`
	Muted    []string `json:"muted,omitempty"`

	// Comment is one comment added, edited or removed. A comment rides the
	// page for the reason the tracker's rides its task: it changes what
	// the page's card shows and what its history says.
	Comment *CommentPatch `json:"comment,omitempty"`

	// RetiredRevisions is the exact list of revision versions this apply
	// deletes.
	//
	// THE PRUNE RIDES THE COMMIT as an explicit list rather than a
	// "keep the last hundred" rule each node evaluates, so every node
	// deletes exactly the same rows at exactly the same position and no
	// node deletes a durable row on its own authority.
	RetiredRevisions []int `json:"retired_revisions,omitempty"`
}

// CommentPatch is one comment's create, edit or removal.
type CommentPatch struct {
	ID string `json:"id"`

	// Removed is the tombstone. Body is nil on a removal and non-nil on a
	// create or an edit, which is what tells the three apart without a
	// fourth field naming the operation.
	Removed bool `json:"removed,omitempty"`

	Body       *string    `json:"body,omitempty"`
	Author     string     `json:"author,omitempty"`
	AuthorKind AuthorKind `json:"author_kind,omitempty"`
	ReplyTo    string     `json:"reply_to,omitempty"`
	Mentions   []string   `json:"mentions,omitempty"`
}

// ContainerPayload is a space's settings, as FULL POST-STATE.
type ContainerPayload struct {
	V int `json:"v"`

	Key     string `json:"key"`
	Name    string `json:"name,omitempty"`
	Purpose string `json:"purpose,omitempty"`
}

// StatusPayload is a trash, a restore or a purge — the three ops that change
// what a reader sees without changing what a page says.
//
// A PURGE CARRIES ITS REASON, because a permanent deletion is the one apply
// whose row survives its object: the deletion marker is how a node that was
// away tells a page that never existed from one that was destroyed.
type StatusPayload struct {
	V      int    `json:"v"`
	Reason string `json:"reason,omitempty"`
}

// Eviction is a node's eviction from this log, or its readmission.
//
// PINNED AT [GateRecordVersion] FOR EVER. See the constant.
type Eviction struct {
	V      int    `json:"v"`
	NodeID string `json:"node_id"`

	EvictedBy string    `json:"evicted_by,omitempty"`
	EvictedAt time.Time `json:"evicted_at"`

	// Readmitted makes this the INVERSE COMMIT rather than a delete, so an
	// eviction's whole history survives a replay — and a node that was
	// evicted, readmitted and evicted again reads correctly rather than as
	// one long absence.
	Readmitted bool `json:"readmitted,omitempty"`
}

// Generation is a reanchor's record: this log was recreated, and every
// position below this number is comparable and safely stale.
type Generation struct {
	V          int    `json:"v"`
	Generation uint32 `json:"generation"`

	By string `json:"by,omitempty"`

	// PrevHighest is the highest sequence the fleet had seen on the OLD
	// stream, recorded so an operator can see what the transition stepped
	// over. Provenance only.
	PrevHighest uint64 `json:"prev_highest,omitempty"`

	// StreamCreatedAt is the new stream's own creation instant, which is
	// what detected the recreation.
	StreamCreatedAt time.Time `json:"stream_created_at"`
}

// DecodeMutation reads the typed payload for one record.
//
// THE DISPATCH IS ON (kind, op) AND NOTHING ELSE, so a record whose pair this
// build does not know is an error naming both rather than a nil payload the
// applier would treat as an empty patch.
func DecodeMutation(rec MutationRecord) (any, error) {
	switch {
	case rec.Subject.Kind == KindTitle && rec.Op == OpCreate:
		return decodePayload[CreatePayload](rec)
	case rec.Subject.Kind == KindTitle && rec.Op == OpRename:
		return decodePayload[RenamePayload](rec)
	case rec.Subject.Kind == KindPage && rec.Op == OpPatch:
		return decodePayload[PagePatch](rec)
	case rec.Subject.Kind == KindPage &&
		(rec.Op == OpTombstone || rec.Op == OpRestore || rec.Op == OpPurge):
		return decodePayload[StatusPayload](rec)
	case rec.Subject.Kind == KindContainer && rec.Op == OpPatch:
		return decodePayload[ContainerPayload](rec)
	case rec.Subject.Kind == KindContainer && rec.Op == OpPurge:
		return decodePayload[StatusPayload](rec)
	case rec.Subject.Kind == KindEviction && rec.Op == OpEviction:
		return decodePayload[Eviction](rec)
	case rec.Subject.Kind == KindGeneration && rec.Op == OpGeneration:
		return decodePayload[Generation](rec)
	case rec.Subject.Kind == KindBarrier && rec.Op == OpBarrier:
		// THE ONE PAYLOAD-FREE RECORD, and nil is its value rather than
		// an absence: the applier's switch has a case for it that
		// writes nothing, which is what keeps "this kind wrote nothing"
		// distinguishable from "nobody classified this kind".
		return nil, nil
	}
	return nil, fmt.Errorf("pages: no payload shape for (%s, %s) — a record's "+
		"kind and op together name its payload, and a pair this build does not "+
		"know is a newer peer's record rather than an empty one",
		rec.Subject.Kind, rec.Op)
}

// decodePayload reads one typed payload, refusing a shape from a newer build.
func decodePayload[T any](rec MutationRecord) (T, error) {
	var out T
	if len(rec.Mutation) == 0 {
		return out, fmt.Errorf("pages: the record on %s carries no payload, and "+
			"(%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return out, fmt.Errorf("pages: decode the %s payload on %s: %w",
			rec.Op, rec.Subject, err)
	}
	return out, nil
}

// EncodeBarrier renders the read index's payload-free append.
//
// # Why the framework cannot write this itself
//
// The read index owns WHEN a barrier goes out and what its acknowledgement
// proves — a quorum-committed position, which is the only client-visible fact
// this broker offers that the responder confirmed its own authority for. The
// DOMAIN owns what a record on its log looks like: this one's envelope, its
// version gate, its subject grammar and the scope alphabet its applier files
// deferrals under. Neither can write the other's half, and this function is
// where they meet.
//
// Every other part of a barrier was already here — [KindBarrier], [OpBarrier],
// [BarrierSubject], the applier's case, the empty [BarrierTables] and the
// scope sentinel — and the encoder was the one piece missing, which is why the
// index could not be built and every `linearizable` read on this domain
// refused for want of one.
//
// AN OP ID IS REFUSED, and that is a correctness rule rather than tidiness. An
// op id becomes the Nats-Msg-Id; a repeat inside the duplicate window is
// answered from the dedupe cache with no quorum round trip at all, and the
// sequence it returns is then a position nothing confirmed — which is exactly
// the claim a barrier exists to make and the one it must never fake.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("pages: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("pages: a barrier carries no op id and this one " +
			"has one — an op id becomes a message id, and a duplicate ack is " +
			"served with no quorum round trip at all")
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V:       RecordVersion,
			Subject: BarrierSubject(),
			Op:      OpBarrier,
			Scope:   ScopeSet{Subject: true},
			Gen:     env.Gen,
		},
	})
}
