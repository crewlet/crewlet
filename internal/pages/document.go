package pages

import (
	"encoding/json"
	"fmt"
	"time"
)

// DocumentVersion is the shape version every record here carries, on the same
// terms as [work.DocumentVersion]: a page is read, modified and written back
// by whichever node the request landed on, so an older build's save would
// strip a newer build's field out of the head. Unknown fields round-trip; an
// unknown VERSION is refused rather than downgraded.
const DocumentVersion = 1

// Container is a space: a unit's, the org root's, or the skills container.
type Container struct {
	V int `json:"v"`

	// Key is the container's own name, upper-case, and the identity every
	// page carries. IMMUTABLE: a unit's `space:` names it, links are built
	// from it, and a key that changed would orphan both.
	Key string `json:"key"`

	Name    string `json:"name,omitempty"`
	Purpose string `json:"purpose,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Page is one page's head.
type Page struct {
	V int `json:"v"`

	ID        string `json:"id"`
	Container string `json:"container"`
	ParentID  string `json:"parent_id,omitempty"`

	// Title is the page's ADDRESS within its container, unique and claimed.
	// The displayed form keeps the author's own capitalisation;
	// [NormalizeTitle] is what the claim and every comparison use.
	Title string `json:"title"`

	// Body is markdown, the canonical and only format.
	//
	// ONE FORMAT, deliberately: a knowledge base that stored storage-format
	// XHTML beside markdown would have every reader — the search indexer,
	// the skill parser, an agent, a person — needing to know which, and the
	// one that guessed wrong would render markup as prose.
	Body string `json:"body,omitempty"`

	Status Status   `json:"status"`
	Labels []string `json:"labels,omitempty"`

	Watchers []string `json:"watchers,omitempty"`
	Muted    []string `json:"muted,omitempty"`

	// Version is a monotonic integer a save must state. It is what makes
	// "somebody else edited this while you were writing" a refusal rather
	// than a silent overwrite.
	Version int `json:"version"`

	Author string `json:"author,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	TrashedAt *time.Time `json:"trashed_at,omitempty"`

	// LastChange is the complete record of what most recently happened, so
	// a projector can repair a missing change key from the head alone.
	LastChange *Change `json:"last_change,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Revision is one immutable past body.
type Revision struct {
	V int `json:"v"`

	ID      string `json:"id"`
	PageID  string `json:"page_id"`
	Version int    `json:"version"`

	Title string `json:"title"`
	Body  string `json:"body"`

	// Message is the author's one-line note about the edit.
	Message string `json:"message,omitempty"`

	Author    string    `json:"author,omitempty"`
	CreatedAt time.Time `json:"created_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Comment is one remark on a page.
type Comment struct {
	V int `json:"v"`

	ID     string `json:"id"`
	PageID string `json:"page_id"`

	Author     string     `json:"author"`
	AuthorKind AuthorKind `json:"author_kind"`

	Body     string   `json:"body"`
	Mentions []string `json:"mentions,omitempty"`
	ReplyTo  string   `json:"reply_to,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	LastChange *Change `json:"last_change,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// TitleClaim is one container's hold on one normalised title.
//
// ITS OWN RECORD, first-writer-wins, rather than a uniqueness check before
// the page write: two nodes creating "Deploy Runbook" at once would both
// check, both find nothing, and both create. The claim is the only thing that
// makes a title an address.
type TitleClaim struct {
	V int `json:"v"`

	Container string `json:"container"`

	// Title is the NORMALISED form — the key's own content, kept on the
	// record so a listing can report what a claim holds without decoding
	// the key.
	Title string `json:"title"`

	PageID    string    `json:"page_id"`
	CreatedAt time.Time `json:"created_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Change is one entry in the record a wake is derived from. Create-only and
// never rewritten, for the reason [work.Change] is.
type Change struct {
	V int `json:"v"`

	ID     string     `json:"id"`
	PageID string     `json:"page_id"`
	Kind   ChangeKind `json:"kind"`

	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`

	Fields map[string]Delta `json:"fields,omitempty"`

	CommentID string `json:"comment_id,omitempty"`

	// Excerpt is at most [MaxExcerpt] bytes of what a card should show.
	Excerpt string `json:"excerpt,omitempty"`

	Mentions []string `json:"mentions,omitempty"`

	TurnID string   `json:"turn_id,omitempty"`
	Chain  []string `json:"chain,omitempty"`

	Quiet bool `json:"quiet,omitempty"`

	HeadRevision uint64 `json:"head_revision,omitempty"`

	Snapshot Snapshot `json:"snapshot"`

	CreatedAt time.Time `json:"created_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Delta is one field's before and after, as text.
type Delta struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Snapshot is the routing state of a page at the moment of a change, copied
// at write time so the node that wins a feed message routes without reading
// anything.
type Snapshot struct {
	Container string `json:"container"`
	Title     string `json:"title"`
	Status    Status `json:"status"`
	Author    string `json:"author,omitempty"`
	Version   int    `json:"version"`

	// Watchers is the set MINUS the muted, computed once at write time so
	// the feed never has to subtract and can never forget to.
	Watchers []string `json:"watchers,omitempty"`
}

// ErrUnknownVersion reports a document a newer build wrote.
type ErrUnknownVersion struct {
	Got  int
	Want int
}

func (e ErrUnknownVersion) Error() string {
	return fmt.Sprintf("pages: document version %d was written by a newer build "+
		"(this one writes %d) — it is left alone rather than rewritten, because "+
		"a rewrite from here would drop whatever the new shape added",
		e.Got, e.Want)
}

// ---- encoding --------------------------------------------------------- //

// The known field names per record. Explicit rather than reflective, for the
// reason [work] gives: a name missing here is decoded into the struct AND
// carried as unknown, so the next encode writes the stale carried copy back
// over what the caller set. A test asserts every declared name is covered.
var (
	containerFields = fieldSet(Container{}, "name", "purpose")
	pageFields      = fieldSet(Page{}, "parent_id", "body", "labels", "watchers",
		"muted", "author", "trashed_at", "last_change")
	revisionFields = fieldSet(Revision{}, "message", "author")
	commentFields  = fieldSet(Comment{}, "mentions", "reply_to", "last_change")
	claimFields    = fieldSet(TitleClaim{})
	changeFields   = fieldSet(Change{}, "actor", "actor_kind", "operator_id",
		"fields", "comment_id", "excerpt", "mentions", "turn_id", "chain",
		"quiet", "head_revision")
)

// EncodeContainer renders a container.
func EncodeContainer(c Container) ([]byte, error) { return encode(c, c.Extra) }

// DecodeContainer reads a container.
func DecodeContainer(data []byte) (Container, error) {
	var c Container
	extra, err := decodeInto(data, &c, containerFields, c.V)
	if err != nil {
		return Container{}, fmt.Errorf("pages: decode container: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Container{}, err
	}
	c.Extra = extra
	return c, nil
}

// EncodePage renders a page head.
func EncodePage(p Page) ([]byte, error) { return encode(p, p.Extra) }

// DecodePage reads a page head.
func DecodePage(data []byte) (Page, error) {
	var p Page
	extra, err := decodeInto(data, &p, pageFields, p.V)
	if err != nil {
		return Page{}, fmt.Errorf("pages: decode page: %w", err)
	}
	if err := checkVersion(p.V); err != nil {
		return Page{}, err
	}
	p.Extra = extra
	return p, nil
}

// EncodeRevision renders a revision.
func EncodeRevision(r Revision) ([]byte, error) { return encode(r, r.Extra) }

// DecodeRevision reads a revision.
func DecodeRevision(data []byte) (Revision, error) {
	var r Revision
	extra, err := decodeInto(data, &r, revisionFields, r.V)
	if err != nil {
		return Revision{}, fmt.Errorf("pages: decode revision: %w", err)
	}
	if err := checkVersion(r.V); err != nil {
		return Revision{}, err
	}
	r.Extra = extra
	return r, nil
}

// EncodeComment renders a comment.
func EncodeComment(c Comment) ([]byte, error) { return encode(c, c.Extra) }

// DecodeComment reads a comment.
func DecodeComment(data []byte) (Comment, error) {
	var c Comment
	extra, err := decodeInto(data, &c, commentFields, c.V)
	if err != nil {
		return Comment{}, fmt.Errorf("pages: decode comment: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Comment{}, err
	}
	c.Extra = extra
	return c, nil
}

// EncodeClaim renders a title claim.
func EncodeClaim(c TitleClaim) ([]byte, error) { return encode(c, c.Extra) }

// DecodeClaim reads a title claim.
func DecodeClaim(data []byte) (TitleClaim, error) {
	var c TitleClaim
	extra, err := decodeInto(data, &c, claimFields, c.V)
	if err != nil {
		return TitleClaim{}, fmt.Errorf("pages: decode title claim: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return TitleClaim{}, err
	}
	c.Extra = extra
	return c, nil
}

// EncodeChange renders a change.
func EncodeChange(c Change) ([]byte, error) { return encode(c, c.Extra) }

// DecodeChange reads a change.
func DecodeChange(data []byte) (Change, error) {
	var c Change
	extra, err := decodeInto(data, &c, changeFields, c.V)
	if err != nil {
		return Change{}, fmt.Errorf("pages: decode change: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Change{}, err
	}
	c.Extra = extra
	return c, nil
}

func checkVersion(got int) error {
	if got > DocumentVersion {
		return ErrUnknownVersion{Got: got, Want: DocumentVersion}
	}
	return nil
}

// encode marshals a record and folds unknown fields back in. A carried field
// LOSES to a known one, so a stale carried copy can never undo the write that
// set it.
func encode(record any, extra map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("pages: encode: %w", err)
	}
	if len(extra) == 0 {
		return data, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, fmt.Errorf("pages: encode: %w", err)
	}
	for name, value := range extra {
		if _, known := merged[name]; !known {
			merged[name] = value
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("pages: encode: %w", err)
	}
	return out, nil
}

// decodeInto unmarshals into out and returns the fields out has no home for.
func decodeInto(data []byte, out any, known map[string]bool, _ int) (map[string]json.RawMessage, error) {
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

// fieldSet is the JSON names a struct defines: the ones a zero value
// marshals, plus the omitempty names given explicitly.
func fieldSet(v any, omitted ...string) map[string]bool {
	data, err := json.Marshal(v)
	if err != nil {
		panic("pages: a record type does not marshal: " + err.Error())
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(data, &named); err != nil {
		panic("pages: a record type does not marshal to an object: " + err.Error())
	}
	out := make(map[string]bool, len(named)+len(omitted))
	for name := range named {
		out[name] = true
	}
	for _, name := range omitted {
		out[name] = true
	}
	return out
}
