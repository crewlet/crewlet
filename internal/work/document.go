package work

import (
	"encoding/json"
	"fmt"
	"maps"
	"time"
)

// DocumentVersion is the shape version every record here carries.
//
// # Why a version and an extra map, when nothing else in coordination has one
//
// A rolling upgrade puts two builds on one bucket. Today no coordination
// record preserves fields it does not know, and for a lease or a counter that
// costs nothing — those records are rewritten wholly by whoever holds them.
// These are different: a work item is READ, MODIFIED and WRITTEN BACK by
// whichever node the request landed on, so an older build's dashboard PATCH
// would strip a newer build's field out of the head and the fleet would
// silently lose it.
//
// So every record round-trips what it does not understand, and a version
// above this one is REFUSED on write rather than downgraded: preserving
// unknown fields is enough for an additive change, and a reshape needs a
// build that knows the new shape to be the one writing it.
const DocumentVersion = 1

// Item is one work item's head.
//
// The whole record travels as the document; the projection extracts columns
// from it for filtering and sorting, and those columns are a cache of what
// this says rather than a second source.
type Item struct {
	V int `json:"v"`

	ID string `json:"id"`

	// Key is <PROJECT>-<n>, minted once from the project counter and
	// IMMUTABLE. People paste it into chat, agents put it in commit
	// messages, and it is the conversation key — a key that changed would
	// orphan every one of those.
	Key string `json:"key"`

	Project string `json:"project"`
	Type    Type   `json:"type"`

	// ParentID is the item above this one, empty at the top.
	ParentID string `json:"parent_id,omitempty"`

	Title string `json:"title"`
	Body  string `json:"body,omitempty"`

	Status      Status      `json:"status"`
	CloseReason CloseReason `json:"close_reason,omitempty"`

	// DuplicateOf names the surviving item when CloseReason is duplicate.
	DuplicateOf string `json:"duplicate_of,omitempty"`

	Priority Priority `json:"priority,omitempty"`

	// Reporter is the handle that filed it, Assignee the one seat that owns
	// it. ONE ASSIGNEE, because "who is doing this" must have one answer:
	// two assignees is two seats each assuming the other has it, which is
	// the failure the whole tracker exists to prevent.
	Reporter string `json:"reporter,omitempty"`
	Assignee string `json:"assignee,omitempty"`

	// Watchers hear about every change. Muted records an explicit unwatch
	// and is kept as its own list rather than as an absence, because
	// absence is the state every automatic re-add fills.
	Watchers []string `json:"watchers,omitempty"`
	Muted    []string `json:"muted,omitempty"`

	Labels []string `json:"labels,omitempty"`

	// Links are the ends this item AUTHORED. The projection derives the
	// other direction; nothing stores both.
	Links []Link `json:"links,omitempty"`

	Due *time.Time `json:"due,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`

	// ChangeSeq counts changes on this item, so a change record can be
	// ordered within the item without a clock two nodes disagree about.
	ChangeSeq int `json:"change_seq"`

	// Reassignments counts AGENT-INITIATED assignee changes since the last
	// human touch. See [ReassignmentBudget].
	Reassignments int `json:"reassignments,omitempty"`

	// LastChange is the complete record of what most recently happened, so
	// a projector can repair a missing change key from the head alone.
	LastChange *Change `json:"last_change,omitempty"`

	// Extra carries fields this build does not know, verbatim.
	Extra map[string]json.RawMessage `json:"-"`
}

// Comment is one remark on an item.
type Comment struct {
	V int `json:"v"`

	ID     string `json:"id"`
	ItemID string `json:"item_id"`

	Author     string     `json:"author"`
	AuthorKind AuthorKind `json:"author_kind"`

	Body string `json:"body"`

	// Mentions are the handles this comment names, resolved at write time
	// against the party registry rather than at read time: the roster moves,
	// and a mention that resolved differently later would wake a different
	// person than the one the author addressed.
	Mentions []string `json:"mentions,omitempty"`

	// ReplyTo threads this under another comment. One level, because a
	// deeper tree is a structure a notification card cannot render and a
	// model cannot follow.
	ReplyTo string `json:"reply_to,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// LastChange is this comment's own complete change record, for the same
	// repair the item's serves.
	LastChange *Change `json:"last_change,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Change is one entry in the record a wake is derived from.
//
// CREATE-ONLY AND NEVER REWRITTEN. The feed consumes these keys, and a bucket
// with history 1 terminates an un-acked message when its key is rewritten —
// so a feed over anything mutable would silently lose wakes.
type Change struct {
	V int `json:"v"`

	ID     string     `json:"id"`
	ItemID string     `json:"item_id"`
	Kind   ChangeKind `json:"kind"`

	// Actor is the seat or person who made the change; ActorKind says
	// which. OperatorID is the API token's own label, recorded for audit
	// beside the seat it acts as — a person and the credential they used
	// are two different facts.
	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`

	// Fields are the deltas, field name to before/after.
	Fields map[string]Delta `json:"fields,omitempty"`

	CommentID string `json:"comment_id,omitempty"`

	// Excerpt is at most [MaxExcerpt] bytes of what a card should show,
	// cut rune-safely.
	Excerpt string `json:"excerpt,omitempty"`

	Mentions []string `json:"mentions,omitempty"`

	// TurnID and Chain are PROVENANCE, carried so an audit can walk from an
	// item back to the turn that wrote it. They do NOT bound anything: a
	// hand-off is charged to the item's own reassignment counter, never to
	// the delegation depth this chain belongs to.
	TurnID string   `json:"turn_id,omitempty"`
	Chain  []string `json:"chain,omitempty"`

	// Quiet marks a change nobody should be woken for — a bulk import, a
	// migration. It is on the record rather than a parameter to the feed,
	// so a redelivery months later still knows not to wake anybody.
	Quiet bool `json:"quiet,omitempty"`

	// HeadRevision is the coordination revision the head write produced,
	// so a reader can tell whether the head it is looking at already
	// includes this change.
	HeadRevision uint64 `json:"head_revision,omitempty"`

	// Snapshot is what routing needs, copied at write time.
	//
	// COPIED RATHER THAN LOOKED UP, and this is what lets the node that
	// wins a feed message route without reading anything: a projection
	// that had not caught up would otherwise route from a stale head, or
	// block the feed until it had.
	Snapshot Snapshot `json:"snapshot"`

	CreatedAt time.Time `json:"created_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Delta is one field's before and after, as text.
//
// TEXT RATHER THAN THE TYPED VALUE, because a change record is read by a
// notification card, a person and a model, and every one of them wants "todo
// → in_progress". The typed value is on the head for anything that needs it.
type Delta struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Snapshot is the routing state of an item at the moment of a change.
type Snapshot struct {
	Key      string `json:"key"`
	Project  string `json:"project"`
	Title    string `json:"title"`
	Status   Status `json:"status"`
	Assignee string `json:"assignee,omitempty"`
	Reporter string `json:"reporter,omitempty"`

	// Watchers is the set MINUS the muted, so the feed never has to
	// subtract and can never forget to.
	Watchers []string `json:"watchers,omitempty"`
}

// Counter is a project's key sequence.
type Counter struct {
	V       int    `json:"v"`
	Project string `json:"project"`
	Last    int    `json:"last"`

	Extra map[string]json.RawMessage `json:"-"`
}

// ErrUnknownVersion reports a document a newer build wrote.
//
// REFUSED ON WRITE, SKIPPED WITH A WARNING ON READ. Preserving unknown FIELDS
// is enough for an additive change and is what [Item.Extra] does; a version
// bump means the shape itself moved, and a build that cannot read it must not
// be the one rewriting it.
type ErrUnknownVersion struct {
	Got  int
	Want int
}

func (e ErrUnknownVersion) Error() string {
	return fmt.Sprintf("work: document version %d was written by a newer build "+
		"(this one writes %d) — it is left alone rather than rewritten, because "+
		"a rewrite from here would drop whatever the new shape added",
		e.Got, e.Want)
}

// ---- encoding --------------------------------------------------------- //

// The known field names per record, so decoding can tell "a field this build
// has" from "a field to carry through". Kept as a var rather than derived by
// reflection because the reflective version needs the same list to be correct
// and hides where to add to it.
var (
	itemFields    = fieldSet(Item{})
	commentFields = fieldSet(Comment{})
	changeFields  = fieldSet(Change{})
	counterFields = fieldSet(Counter{})
)

// EncodeItem renders an item, merging back whatever it carried.
func EncodeItem(item Item) ([]byte, error) { return encode(item, item.Extra) }

// DecodeItem reads an item, keeping unknown fields.
func DecodeItem(data []byte) (Item, error) {
	var item Item
	extra, err := decode(data, &item, itemFields)
	if err != nil {
		return Item{}, fmt.Errorf("work: decode item: %w", err)
	}
	if item.V > DocumentVersion {
		return Item{}, ErrUnknownVersion{Got: item.V, Want: DocumentVersion}
	}
	item.Extra = extra
	return item, nil
}

// EncodeComment renders a comment.
func EncodeComment(c Comment) ([]byte, error) { return encode(c, c.Extra) }

// DecodeComment reads a comment.
func DecodeComment(data []byte) (Comment, error) {
	var c Comment
	extra, err := decode(data, &c, commentFields)
	if err != nil {
		return Comment{}, fmt.Errorf("work: decode comment: %w", err)
	}
	if c.V > DocumentVersion {
		return Comment{}, ErrUnknownVersion{Got: c.V, Want: DocumentVersion}
	}
	c.Extra = extra
	return c, nil
}

// EncodeChange renders a change.
func EncodeChange(c Change) ([]byte, error) { return encode(c, c.Extra) }

// DecodeChange reads a change.
func DecodeChange(data []byte) (Change, error) {
	var c Change
	extra, err := decode(data, &c, changeFields)
	if err != nil {
		return Change{}, fmt.Errorf("work: decode change: %w", err)
	}
	if c.V > DocumentVersion {
		return Change{}, ErrUnknownVersion{Got: c.V, Want: DocumentVersion}
	}
	c.Extra = extra
	return c, nil
}

// EncodeCounter renders a counter.
func EncodeCounter(c Counter) ([]byte, error) { return encode(c, c.Extra) }

// DecodeCounter reads a counter.
func DecodeCounter(data []byte) (Counter, error) {
	var c Counter
	extra, err := decode(data, &c, counterFields)
	if err != nil {
		return Counter{}, fmt.Errorf("work: decode counter: %w", err)
	}
	if c.V > DocumentVersion {
		return Counter{}, ErrUnknownVersion{Got: c.V, Want: DocumentVersion}
	}
	c.Extra = extra
	return c, nil
}

// encode marshals a record and folds unknown fields back in.
//
// THE UNKNOWN FIELDS LOSE TO THE KNOWN ONES on a collision, which can only
// happen if a build decoded a field it knows into Extra — a bug — and the
// alternative would let a stale carried value overwrite what this write
// decided.
func encode(record any, extra map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("work: encode: %w", err)
	}
	if len(extra) == 0 {
		return data, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, fmt.Errorf("work: encode: %w", err)
	}
	for name, value := range extra {
		if _, known := merged[name]; !known {
			merged[name] = value
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("work: encode: %w", err)
	}
	return out, nil
}

// decode unmarshals into out and returns the fields out has no home for.
func decode(data []byte, out any, known map[string]bool) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	var extra map[string]json.RawMessage
	for name, value := range all {
		if known[name] {
			continue
		}
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		extra[name] = value
	}
	return extra, nil
}

// fieldSet is the JSON names a struct defines.
func fieldSet(v any) map[string]bool {
	data, err := json.Marshal(v)
	if err != nil {
		panic("work: a record type does not marshal: " + err.Error())
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(data, &named); err != nil {
		panic("work: a record type does not marshal to an object: " + err.Error())
	}
	// A zero value omits its `omitempty` fields, so the marshalled set is
	// incomplete. The names are listed explicitly instead — the cost is one
	// line per field, and the alternative silently carries a known field
	// through Extra and then loses the write that set it.
	out := map[string]bool{}
	for name := range named {
		out[name] = true
	}
	for _, name := range omittedNames(v) {
		out[name] = true
	}
	return out
}

// omittedNames lists the omitempty fields a zero value does not marshal.
//
// EXPLICIT, because the failure of getting it wrong is silent and permanent:
// a name missing here is decoded into the struct AND carried in Extra, so the
// next encode writes the stale carried copy back over what the caller set.
// A test marshals a fully-populated value of each type and asserts every name
// it produces is covered.
func omittedNames(v any) []string {
	switch v.(type) {
	case Item:
		return []string{"parent_id", "body", "close_reason", "duplicate_of",
			"priority", "reporter", "assignee", "watchers", "muted", "labels",
			"links", "due", "closed_at", "reassignments", "last_change"}
	case Comment:
		return []string{"mentions", "reply_to", "last_change"}
	case Change:
		return []string{"actor", "actor_kind", "operator_id", "fields",
			"comment_id", "excerpt", "mentions", "turn_id", "chain", "quiet",
			"head_revision"}
	case Counter:
		return nil
	}
	return nil
}

// CloneExtra copies a carried field map, so a caller can hold a record
// without sharing its unknown fields with the decoder's.
func CloneExtra(in map[string]json.RawMessage) map[string]json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	return maps.Clone(in)
}
