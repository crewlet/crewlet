package pages

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/jsoncarry"
)

// DocumentVersion is the shape version every record here carries: a page is
// read, modified and written back by whichever node the request landed on, so
// an older build's save would strip a newer build's field out of the head.
// Unknown fields round-trip; an unknown VERSION is refused rather than
// downgraded.
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

// Change is one entry in a page's history — what the activity feed renders —
// kept as a row's document beside the columns the feed reads. Create-only and
// never rewritten: its insert does nothing when the row is already there.
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
	return fmt.Sprintf("pages: document version %d was written by a newer build "+
		"(this one writes %d) — it is left alone rather than rewritten, because "+
		"a rewrite from here would drop whatever the new shape added",
		e.Got, e.Want)
}

// ---- encoding --------------------------------------------------------- //

// A page is read, modified and written back by whichever node the request
// landed on, so every document here carries the members this build does not
// know, through [github.com/crewlet/crewlet/internal/jsoncarry] — whose
// package doc is the contract — and a save by an older build writes back what
// a newer one added.

// EncodeContainer renders a container.
//
// THROUGH THE CARRY DIRECTLY rather than a MarshalJSON of its own, which every
// other document here has: [ContainerListing] embeds a container, and a method
// on it would be promoted onto the listing and write it as the bare container,
// without the page count beside it.
func EncodeContainer(c Container) ([]byte, error) {
	data, err := jsoncarry.Marshal(c, c.Extra)
	if err != nil {
		return nil, fmt.Errorf("pages: encode: %w", err)
	}
	return data, nil
}

// DecodeContainer reads a container.
func DecodeContainer(data []byte) (Container, error) {
	var c Container
	if err := jsoncarry.Unmarshal(data, &c, &c.Extra); err != nil {
		return Container{}, fmt.Errorf("pages: decode container: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Container{}, err
	}
	return c, nil
}

// EncodePage renders a page head.
func EncodePage(p Page) ([]byte, error) { return encode(p) }

// DecodePage reads a page head.
func DecodePage(data []byte) (Page, error) {
	var p Page
	if err := json.Unmarshal(data, &p); err != nil {
		return Page{}, fmt.Errorf("pages: decode page: %w", err)
	}
	if err := checkVersion(p.V); err != nil {
		return Page{}, err
	}
	return p, nil
}

// EncodeRevision renders a revision.
func EncodeRevision(r Revision) ([]byte, error) { return encode(r) }

// DecodeRevision reads a revision.
func DecodeRevision(data []byte) (Revision, error) {
	var r Revision
	if err := json.Unmarshal(data, &r); err != nil {
		return Revision{}, fmt.Errorf("pages: decode revision: %w", err)
	}
	if err := checkVersion(r.V); err != nil {
		return Revision{}, err
	}
	return r, nil
}

// EncodeComment renders a comment.
func EncodeComment(c Comment) ([]byte, error) { return encode(c) }

// DecodeComment reads a comment.
func DecodeComment(data []byte) (Comment, error) {
	var c Comment
	if err := json.Unmarshal(data, &c); err != nil {
		return Comment{}, fmt.Errorf("pages: decode comment: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Comment{}, err
	}
	return c, nil
}

// EncodeClaim renders a title claim.
func EncodeClaim(c TitleClaim) ([]byte, error) { return encode(c) }

// DecodeClaim reads a title claim.
func DecodeClaim(data []byte) (TitleClaim, error) {
	var c TitleClaim
	if err := json.Unmarshal(data, &c); err != nil {
		return TitleClaim{}, fmt.Errorf("pages: decode title claim: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return TitleClaim{}, err
	}
	return c, nil
}

// EncodeChange renders a change.
func EncodeChange(c Change) ([]byte, error) { return encode(c) }

// DecodeChange reads a change.
func DecodeChange(data []byte) (Change, error) {
	var c Change
	if err := json.Unmarshal(data, &c); err != nil {
		return Change{}, fmt.Errorf("pages: decode change: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Change{}, err
	}
	return c, nil
}

func encode(document any) ([]byte, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("pages: encode: %w", err)
	}
	return data, nil
}

func checkVersion(got int) error {
	if got > DocumentVersion {
		return ErrUnknownVersion{Got: got, Want: DocumentVersion}
	}
	return nil
}

// MarshalJSON writes [Page.Extra] back beside the members this build knows.
func (p Page) MarshalJSON() ([]byte, error) {
	type fields Page
	return jsoncarry.Marshal(fields(p), p.Extra)
}

// UnmarshalJSON keeps every member of the page head this build does not know in
// [Page.Extra].
func (p *Page) UnmarshalJSON(b []byte) error {
	type fields Page
	return jsoncarry.Unmarshal(b, (*fields)(p), &p.Extra)
}

// MarshalJSON writes [Revision.Extra] back beside the members this build knows.
func (r Revision) MarshalJSON() ([]byte, error) {
	type fields Revision
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member of the revision this build does not know in
// [Revision.Extra].
func (r *Revision) UnmarshalJSON(b []byte) error {
	type fields Revision
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [Comment.Extra] back beside the members this build knows.
func (c Comment) MarshalJSON() ([]byte, error) {
	type fields Comment
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// UnmarshalJSON keeps every member of the comment this build does not know in
// [Comment.Extra].
func (c *Comment) UnmarshalJSON(b []byte) error {
	type fields Comment
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes [TitleClaim.Extra] back beside the members this build
// knows.
func (c TitleClaim) MarshalJSON() ([]byte, error) {
	type fields TitleClaim
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// UnmarshalJSON keeps every member of the title claim this build does not know
// in [TitleClaim.Extra].
func (c *TitleClaim) UnmarshalJSON(b []byte) error {
	type fields TitleClaim
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes [Change.Extra] back beside the members this build knows.
func (c Change) MarshalJSON() ([]byte, error) {
	type fields Change
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// UnmarshalJSON keeps every member of the change this build does not know in
// [Change.Extra].
func (c *Change) UnmarshalJSON(b []byte) error {
	type fields Change
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}
