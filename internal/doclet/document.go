package doclet

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/jsoncarry"
)

// DocumentVersion is the shape version every stored document carries. A page
// head is decoded and re-encoded by every later apply on this node, so a field
// this build does not know is carried through unchanged rather than dropped,
// and an unknown VERSION is refused rather than downgraded: a rewrite from here
// would lose whatever that shape added.
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

	// ChartEpoch is the configuration activation Name and Purpose were last
	// derived from ([configplane.ActivationStamp]). See
	// [Store.EnsureContainer].
	ChartEpoch int64 `json:"chart_epoch,omitempty"`

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
	Title string `json:"title" untrusted:"name"`

	// Body is markdown, the canonical and only format.
	//
	// ONE FORMAT, deliberately: a knowledge base that stored storage-format
	// XHTML beside markdown would have every reader — the search indexer,
	// the skill parser, an agent, a person — needing to know which, and the
	// one that guessed wrong would render markup as prose.
	Body string `json:"body,omitempty" untrusted:"fence"`

	Status Status   `json:"status"`
	Labels []string `json:"labels,omitempty" untrusted:"name"`

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

	Body     string   `json:"body" untrusted:"fence"`
	Mentions []string `json:"mentions,omitempty"`
	ReplyTo  string   `json:"reply_to,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Change is one page history row's document: what a change was, by whom, and
// the excerpt a card shows. Create-only and never rewritten — a history row
// records something that happened.
type Change struct {
	V int `json:"v"`

	ID     string     `json:"id"`
	PageID string     `json:"page_id"`
	Kind   ChangeKind `json:"kind"`

	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`

	CommentID string `json:"comment_id,omitempty"`

	// Excerpt is at most [MaxExcerpt] bytes of what a card should show.
	Excerpt string `json:"excerpt,omitempty"`

	TurnID string   `json:"turn_id,omitempty"`
	Chain  []string `json:"chain,omitempty"`

	Quiet bool `json:"quiet,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// ErrUnknownVersion reports a document a newer build wrote.
type ErrUnknownVersion struct {
	Got  int
	Want int
}

func (e ErrUnknownVersion) Error() string {
	return fmt.Sprintf("doclet: document version %d was written by a newer build "+
		"(this one writes %d) — it is left alone rather than rewritten, because "+
		"a rewrite from here would drop whatever the new shape added",
		e.Got, e.Want)
}

// ---- encoding --------------------------------------------------------- //

// The known field names per record, read off each struct's own tags — see
// [jsoncarry.Names] for what a hand-kept list of the omitempty ones cost.
var (
	containerFields = jsoncarry.Names(Container{})
	pageFields      = jsoncarry.Names(Page{})
	revisionFields  = jsoncarry.Names(Revision{})
	commentFields   = jsoncarry.Names(Comment{})
	changeFields    = jsoncarry.Names(Change{})
)

// EncodeContainer renders a container.
func EncodeContainer(c Container) ([]byte, error) { return jsoncarry.Encode(c, c.Extra) }

// DecodeContainer reads a container.
func DecodeContainer(data []byte) (Container, error) {
	var c Container
	extra, err := jsoncarry.Decode(data, &c, containerFields)
	if err != nil {
		return Container{}, fmt.Errorf("doclet: decode container: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Container{}, err
	}
	c.Extra = extra
	return c, nil
}

// EncodePage renders a page head.
func EncodePage(p Page) ([]byte, error) { return jsoncarry.Encode(p, p.Extra) }

// DecodePage reads a page head.
func DecodePage(data []byte) (Page, error) {
	var p Page
	extra, err := jsoncarry.Decode(data, &p, pageFields)
	if err != nil {
		return Page{}, fmt.Errorf("doclet: decode page: %w", err)
	}
	if err := checkVersion(p.V); err != nil {
		return Page{}, err
	}
	p.Extra = extra
	return p, nil
}

// EncodeRevision renders a revision.
func EncodeRevision(r Revision) ([]byte, error) { return jsoncarry.Encode(r, r.Extra) }

// DecodeRevision reads a revision.
func DecodeRevision(data []byte) (Revision, error) {
	var r Revision
	extra, err := jsoncarry.Decode(data, &r, revisionFields)
	if err != nil {
		return Revision{}, fmt.Errorf("doclet: decode revision: %w", err)
	}
	if err := checkVersion(r.V); err != nil {
		return Revision{}, err
	}
	r.Extra = extra
	return r, nil
}

// EncodeComment renders a comment.
func EncodeComment(c Comment) ([]byte, error) { return jsoncarry.Encode(c, c.Extra) }

// DecodeComment reads a comment.
func DecodeComment(data []byte) (Comment, error) {
	var c Comment
	extra, err := jsoncarry.Decode(data, &c, commentFields)
	if err != nil {
		return Comment{}, fmt.Errorf("doclet: decode comment: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Comment{}, err
	}
	c.Extra = extra
	return c, nil
}

// EncodeChange renders a change.
func EncodeChange(c Change) ([]byte, error) { return jsoncarry.Encode(c, c.Extra) }

// DecodeChange reads a change.
func DecodeChange(data []byte) (Change, error) {
	var c Change
	extra, err := jsoncarry.Decode(data, &c, changeFields)
	if err != nil {
		return Change{}, fmt.Errorf("doclet: decode change: %w", err)
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
