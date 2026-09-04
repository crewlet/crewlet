package coord

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// A DOCUMENT FAMILY is a set of compare-and-set records the whole company has
// to agree on, held where every node already reads and projected into each
// node's own database for querying.
//
// # Why this exists at all
//
// The estate rule is one question — does the company have to agree on this,
// or one node? — and a work item or a wiki page answers "every node, or the
// answer is wrong on all of them". That rules out the store: it is one file
// owned by one process under an OS lock, so a duty holder could not hand it
// to its successor, and migrations 0010-0013 are the recorded cost of putting
// company-wide facts there anyway.
//
// What is new is the SIZE and the SHAPE. Every fleet record before this one
// was a counter, a claim or a small ledger row read by key; these are
// thousands of records that a person filters, sorts and searches. So the
// coordination store holds the record of truth and each node keeps a
// rebuildable projection of it — the memory changelog's shape, extended for
// the three things a company-wide dataset needs that a seat's memory did not:
// many writers rather than one owner, deletes that travel because nothing
// re-converges a deleted item, and a catch-up at node boot rather than at
// seat acquisition.
//
// # What coordination understands
//
// Nothing. A document is opaque bytes and a version, exactly as [SandboxRuns]
// is, and for the same reason: every decision taken on the fields — which
// status transitions are legal, who watches an item, whether a page's base
// version is stale — belongs to the package that owns them, where its suite
// is. This layer answers only "did this write land at the version I read".
//
// # Retention
//
// None. A family's buckets have no age: an item is a fact for the life of the
// deployment, and a bucket age is the only retention the embedded broker
// offers (a per-key TTL through this client is create-only, and an update
// clears it — measured). Removing a document is a DECISION taken by a sweep
// that reads it, never a horizon that reaps it while a person is still
// reading it.
type Family string

// The families. A closed set with a Valid method, so an unknown value off the
// wire is a value rather than a panic — and so the bucket inventory a
// retention test asserts stays exact.
const (
	// FamilyWork is the tracker: items, their comments, the change keys a
	// wake is derived from, and the per-project key counters.
	FamilyWork Family = "work"

	// FamilyPages is the knowledge base: containers, pages, revisions,
	// comments, change keys and title claims.
	FamilyPages Family = "pages"

	// FamilyKBVectors is the embedding of a page or an item, keyed on the
	// source's version.
	//
	// ITS OWN FAMILY because it is DERIVED and its lifecycle says so: it
	// can be dropped and rebuilt wholesale when the embedding provider or
	// its width changes, which is a thing you must never do to the pages
	// themselves. Absent entirely for a company that has not configured
	// embeddings.
	FamilyKBVectors Family = "kb_vectors"
)

// Valid reports whether f is a family this build serves.
func (f Family) Valid() bool {
	switch f {
	case FamilyWork, FamilyPages, FamilyKBVectors:
		return true
	}
	return false
}

// Families is every family, in bucket order.
func Families() []Family { return []Family{FamilyWork, FamilyPages, FamilyKBVectors} }

// Op is what happened to a document, as a watcher sees it.
type Op string

const (
	// OpPut is a create or an update: Value carries the document.
	OpPut Op = "put"

	// OpPurge is a removal. Value is nil, and a projector applying one
	// removes the row and everything derived from it.
	//
	// Deletes TRAVEL here, unlike the memory changelog's, and the
	// difference is that nothing re-converges these. A seat's diary row
	// resurrected by a hydration is dropped again by the next lifecycle
	// pass; a work item resurrected by a projection is on somebody's board
	// until a person deletes it a second time.
	OpPurge Op = "purge"
)

// Change is one document as a watcher observes it.
type Change struct {
	Key   string
	Value []byte
	Op    Op

	// Revision is the store's own sequence for this write. MONOTONIC
	// across the family, which is what lets a projection be idempotent by
	// comparing it and a reader name the version it is waiting for.
	Revision uint64

	// Initial marks a change delivered by the watch's opening pass rather
	// than by a live write. A projector uses it to tell "catching up" from
	// "keeping up"; nothing else should care.
	Initial bool
}

// Watcher is a live view of a family.
//
// The channel closes when the context is done or the watch fails. A nil
// Change on the channel is the CAUGHT-UP MARKER: everything the store held
// when the watch opened has been delivered, and what follows is live. A
// caller that ignores the marker is a caller that cannot tell an empty family
// from a family it has not finished reading, which is the difference between
// "this company has no work" and "do not answer yet".
type Watcher interface {
	// Changes yields every change until the watch ends.
	Changes() <-chan *Change

	// Stop ends the watch and releases what it holds.
	Stop() error
}

// Documents is the fleet's compare-and-set document store.
//
// EVERY READ RAISES rather than answering empty, on the [SandboxRuns] rule
// and for a sharper version of its reason: "there is no such item" is an
// answer a seat acts on — it files a duplicate, it abandons work it was
// asked to do, it tells a person their page is gone — and a store that could
// not be reached must never be able to say it.
type Documents interface {
	// Document reads one, reporting whether it exists.
	Document(ctx context.Context, family Family, key string) (Record, bool, error)

	// DocumentAt reads one AT AN EXACT REVISION.
	//
	// It exists because a plain read is not read-your-writes: the client
	// answers it with a direct get that any replica may serve from its own
	// store, and a follower that has not applied the sequence yet answers
	// "not found" for a document the caller holds an acknowledgement for.
	// That answer is UNKNOWN, never absent — the caller retries behind the
	// sequence rather than concluding the document is gone.
	DocumentAt(ctx context.Context, family Family, key string, revision uint64) (Record, bool, error)

	// Documents lists a family, optionally under a key prefix.
	//
	// A LISTING IS O(keys) message deliveries, so this serves duties and
	// boot passes and nothing on a request path: the read side of every
	// screen and every tool is the node's own projection.
	Documents(ctx context.Context, family Family, prefix string) ([]Record, error)

	// CreateDocument writes a new one, reporting whether it was new.
	// False is "it exists", never a fault.
	CreateDocument(ctx context.Context, family Family, key string, value []byte) (bool, error)

	// UpdateDocument writes at a version, reporting whether that version
	// still held. False is a LOST RACE: the caller re-reads and re-decides,
	// because the state it based its decision on has moved.
	UpdateDocument(ctx context.Context, family Family, key string, value []byte, version uint64) (bool, error)

	// PurgeDocument removes one at a version, reporting whether that
	// version still held.
	//
	// Purge rather than delete, because these buckets have no age: a delete
	// leaves a tombstone revision that outlives the deployment, and a
	// listing that returns tombstones is a board with ghosts on it.
	PurgeDocument(ctx context.Context, family Family, key string, version uint64) (bool, error)

	// WatchDocuments follows a family.
	//
	// From zero, the watch opens with every document the store holds — one
	// change each, Initial set — then a caught-up marker, then live
	// changes. From a revision, it replays only what was written after it,
	// with no initial pass and no marker beyond the first.
	WatchDocuments(ctx context.Context, family Family, from uint64) (Watcher, error)
}

// KeySeparator is what joins a key's segments.
//
// A dot, because a key becomes a subject token path under the bucket, and
// that is what makes a CLASS filterable: a consumer subscribed to the change
// class of a family selects every change key and nothing else. Every other
// byte in a segment is escaped, so a segment can never contain one of these
// and a key's shape is exactly the number of segments it was built from.
const KeySeparator = "."

// DocumentKey builds a key from its segments, escaping each.
//
// SEGMENT BY SEGMENT, and this is load-bearing rather than tidy. A key is a
// subject token path, so a raw segment containing a dot would split into two
// tokens and change what a filtered watch matches; a segment containing a
// space, a colon or a non-ASCII letter is refused by the store outright. Page
// titles and seat handles contain all four. Escaping each segment and joining
// with the separator is what lets a title be part of a key at all, and what
// keeps `class.a.b` distinct from a single segment that happened to spell it.
//
// AN EMPTY SEGMENT IS NOT A KEY. Every segment names something — a class, an
// id, a project, a title — so an empty one is a caller that lost a value on
// the way here, and the key it would build is one [DocumentSegments] refuses.
// That refusal is the design: a key nobody can decode is caught at the first
// listing that reads it back, where a key that silently decoded to an empty
// name would put a document belonging to nothing into a projection.
func DocumentKey(segments ...string) string {
	escaped := make([]string, len(segments))
	for i, seg := range segments {
		escaped[i] = escapeSegment(seg)
	}
	return strings.Join(escaped, KeySeparator)
}

// DocumentSegments recovers the segments of a key, reporting false for one
// this grammar did not write.
//
// A malformed key is skipped rather than guessed at, on the lease table's
// rule: a listing that invented a segment would put a document nobody wrote
// into a projection.
func DocumentSegments(key string) ([]string, bool) {
	if key == "" {
		return nil, false
	}
	parts := strings.Split(key, KeySeparator)
	out := make([]string, len(parts))
	for i, part := range parts {
		seg, ok := unescapeSegment(part)
		if !ok {
			return nil, false
		}
		out[i] = seg
	}
	return out, true
}

// KeyClass is the first segment of a key — the record kind.
func KeyClass(key string) (string, bool) {
	segs, ok := DocumentSegments(key)
	if !ok || len(segs) == 0 {
		return "", false
	}
	return segs[0], true
}

const (
	segmentEscape = '='
	segmentHex    = "0123456789ABCDEF"
)

// literalSegmentByte reports whether b may appear in a segment as itself.
//
// The separator is deliberately NOT literal: a segment that could contain one
// would make the grammar ambiguous, which is the whole thing it exists to
// prevent.
func literalSegmentByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_' || b == '-':
		return true
	}
	return false
}

func escapeSegment(seg string) string {
	needs := false
	for i := 0; i < len(seg); i++ {
		if !literalSegmentByte(seg[i]) {
			needs = true
			break
		}
	}
	if !needs {
		return seg
	}
	var b strings.Builder
	b.Grow(len(seg) * 3)
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if literalSegmentByte(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte(segmentEscape)
		b.WriteByte(segmentHex[c>>4])
		b.WriteByte(segmentHex[c&0x0f])
	}
	return b.String()
}

func unescapeSegment(seg string) (string, bool) {
	if seg == "" {
		return "", false
	}
	if !strings.ContainsRune(seg, segmentEscape) {
		for i := 0; i < len(seg); i++ {
			if !literalSegmentByte(seg[i]) {
				return "", false
			}
		}
		return seg, true
	}
	var b strings.Builder
	b.Grow(len(seg))
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if c != segmentEscape {
			if !literalSegmentByte(c) {
				return "", false
			}
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(seg) {
			return "", false
		}
		hi, ok := unhexSegment(seg[i+1])
		if !ok {
			return "", false
		}
		lo, ok := unhexSegment(seg[i+2])
		if !ok {
			return "", false
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), true
}

func unhexSegment(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	// Upper case only, so two keys can never decode to one segment set.
	return 0, false
}

// Delivery is one change handed to a feed consumer, with its outcome.
//
// EXPLICITLY ACKED, unlike a [Watcher]'s change, and that is the whole
// difference between the two: a watch is how a node keeps its own projection
// in step and may safely miss nothing but its own restart, while a feed is
// how a FLEET turns a committed record into work exactly once. A watcher that
// dropped a change costs a rebuild; a feed that dropped one costs a wake
// nobody will ever be told about.
type Delivery struct {
	Change

	// Ack marks the change handled. Called after whatever the handler
	// produced is itself durable, never before.
	Ack func() error

	// Nak returns the change for redelivery, after a delay. A handler that
	// could not reach something it needs naks; one that decided the change
	// means nothing to it ACKS, because a decision is handling.
	Nak func(delay time.Duration) error
}

// Feed is a durable, fleet-wide consumer over one class of a family's keys.
//
// # Why the class, and why create-only keys
//
// A feed exists to turn a committed record into a wake, and it must consume a
// key that is NEVER REWRITTEN. A bucket keeps one revision per key, so
// rewriting a key TERMINATES any un-acked message already delivered for it —
// no redelivery, no error, nothing anywhere saying a wake was lost. The
// change class is create-only for exactly this reason, and a feed over the
// head class would look identical and lose wakes under load.
//
// # Why a group rather than a duty
//
// Every node pulls from one durable consumer, so a change is handled once by
// whichever node gets there first. Making it a singleton DUTY instead would
// put every wake behind a lease: a flap on the duty holder would stall the
// company's notifications for a lease TTL, and the work is stateless anyway.
type Feed interface {
	// Next blocks for the next delivery until the context ends.
	//
	// A nil Delivery with a nil error means the feed has closed, which is
	// how a caller tells a shutdown from a failure.
	Next(ctx context.Context) (*Delivery, error)

	// Stop ends the feed. The durable consumer SURVIVES, which is what
	// makes a restart resume rather than replay: its position is the
	// fleet's, not this process's.
	Stop() error
}

// Feeder opens feeds. Separate from [Documents] because a backend can serve
// documents without serving feeds — the memory twin did, until its own
// suite needed one — and because the two are used by different layers.
type Feeder interface {
	// FeedDocuments opens a durable feed over one key class.
	//
	// A NEW consumer starts at the family's CURRENT HEAD, never at the
	// beginning: an upgrade that introduced a feed must not wake every
	// seat for every change the company ever made. An existing one resumes
	// where the fleet left it.
	FeedDocuments(ctx context.Context, family Family, class, group string) (Feed, error)
}

// ErrUnknownFamily names a family this build does not serve.
func ErrUnknownFamily(f Family) error {
	return fmt.Errorf("coord: %q is not a document family this build serves", string(f))
}
