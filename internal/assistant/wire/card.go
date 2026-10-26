package wire

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Card is one engine-built card: its [CardKind] and exactly that kind's body.
//
// BUILT BY THE ENGINE FROM A TOOL'S OWN ANSWER, never by the model, so a number
// on a table card is the engine's number and a proposal card shows the
// request that will be sent rather than a model's description of it.
type Card struct {
	Type      CardKind       `json:"type"`
	Table     *TableCard     `json:"table,omitempty"`
	Entity    *EntityCard    `json:"entity,omitempty"`
	Proposal  *ProposalCard  `json:"proposal,omitempty"`
	OpenInApp *OpenInAppCard `json:"open_in_app,omitempty"`
	Error     *ErrorCard     `json:"error,omitempty"`
	Notice    *NoticeCard    `json:"notice,omitempty"`
	// --- deep work (im-deep) ---
	File *FileCard `json:"file,omitempty"`
}

// TableCard is rows from a read.
type TableCard struct {
	Title string `json:"title"`
	// Window names the read's time window where it has one ("This week").
	Window  string   `json:"window,omitempty"`
	Columns []Column `json:"columns"`
	// Rows are cells in column order. A cell is a string, a number, a
	// boolean or null, typed by its column.
	Rows  [][]any `json:"rows"`
	Total []any   `json:"total,omitempty"`
	// RowCount is the rows the read answered, which is more than Rows
	// holds when Truncated.
	RowCount  int       `json:"row_count"`
	Truncated bool      `json:"truncated"`
	AsOf      time.Time `json:"as_of"`
	// Exports are the stored snapshot's downloads, re-checked against the
	// person's ownership and apps on every download.
	Exports *Exports `json:"exports,omitempty"`
	Open    *Link    `json:"open,omitempty"`
}

// Column is a table card's column.
type Column struct {
	Key   string     `json:"key"`
	Label string     `json:"label"`
	Type  ColumnType `json:"type"`
	Align Align      `json:"align,omitempty"`
	// Detail marks a column the drawer leaves out to fit its width. It
	// stays in the card, so the downloads and the model's summary keep it.
	Detail bool `json:"detail,omitempty"`
}

// Exports are a table card's download paths.
type Exports struct {
	CSV  string `json:"csv"`
	XLSX string `json:"xlsx"`
}

// ExportsOf are the download paths of the table card with this id: the
// export routes, which read the stored card again as its owner.
func ExportsOf(card uuid.UUID) *Exports {
	base := "/assistant/cards/" + card.String() + "/export"
	return &Exports{CSV: base + ".csv", XLSX: base + ".xlsx"}
}

// Link is a dashboard address the engine built from its own route table:
// Href is the path the dashboard's router resolves, never a URL a model
// wrote.
type Link struct {
	Label  string `json:"label"`
	Screen string `json:"screen"`
	Href   string `json:"href"`
}

// EntityCard is one record, or a list of records (an org chart, the people
// in a unit).
type EntityCard struct {
	// Kind is the dashboard object the record is, absent for a record that
	// is none of them: a generic read's answer, which is the API's.
	Kind     ResourceKind `json:"kind,omitempty"`
	ID       string       `json:"id"`
	Title    string       `json:"title"`
	Subtitle string       `json:"subtitle,omitempty"`
	Facts    []Fact       `json:"facts,omitempty"`
	// Text is a body the record carries, rendered in the restricted
	// Markdown subset: no images, no raw HTML, no autolinks.
	Text  string       `json:"text,omitempty"`
	Items []EntityItem `json:"items,omitempty"`
	// ItemsNoun is what one item and several are called ("policy",
	// "policies"), so the drawer can say how many a folded card holds. Absent
	// on a card with no list, and on one stored before it was carried.
	ItemsNoun *Noun `json:"items_noun,omitempty"`
	Open      *Link `json:"open,omitempty"`
}

// Noun is what one thing and several are called, in the lower case a count
// is said with: "1 policy", "6 policies".
type Noun struct {
	One  string `json:"one"`
	Many string `json:"many"`
}

// Fact is one labelled value on an entity card.
type Fact struct {
	Label string `json:"label"`
	// Value is the value as the API answered it, in the form its Type
	// names: a number as written, an RFC 3339 instant, a closed set's slug.
	// The dashboard draws it as it draws the same field on its screens.
	Value string `json:"value"`
	// Type is how the value is drawn; absent, it is text.
	Type ColumnType `json:"type,omitempty"`
	Mono bool       `json:"mono,omitempty"`
	// Detail marks a fact the card keeps for the model (a version an update
	// is proposed against) that the drawer leaves out.
	Detail bool `json:"detail,omitempty"`
}

// EntityItem is one row of an entity card's list. Depth indents a tree.
type EntityItem struct {
	// Kind is the dashboard object the item is, absent for none.
	Kind  ResourceKind `json:"kind,omitempty"`
	ID    string       `json:"id"`
	Label string       `json:"label"`
	Fact  string       `json:"fact,omitempty"`
	Depth int          `json:"depth,omitempty"`
	Open  *Link        `json:"open,omitempty"`
}

// ProposalCard is a change waiting on the person, rendered from the server's
// own preview (ADR-0051).
type ProposalCard struct {
	ProposalID uuid.UUID     `json:"proposal_id"`
	Scope      ProposalScope `json:"scope"`
	// Tool is the catalogue tool that proposed it.
	Tool string `json:"tool"`
	// Title is the action in sentence case ("Create task").
	Title string `json:"title"`
	// Target names what it changes ("ENG-142", "Worklet › Tasks").
	Target string `json:"target,omitempty"`
	// Changes is the "What will change" list, scope first, every change a
	// before and after pair (D10).
	Changes []Change `json:"changes"`
	// Note is the consequence, in one sentence.
	Note string `json:"note,omitempty"`
	// Audience is who will see the result where visibility changes (a
	// share, a comment others are notified of).
	Audience []string `json:"audience,omitempty"`
	// Taint names the outside source the turn read before proposing, so
	// the person weighs a change that text somebody else wrote may have
	// asked for (D25).
	Taint string `json:"taint,omitempty"`
	// StepUp is a change whose verb asks the person to confirm it is them
	// before it executes.
	StepUp bool `json:"step_up,omitempty"`
	// Digest is the proposal's own digest, which the approval echoes:
	// "sha256:" and 64 lowercase hex digits.
	Digest    string        `json:"digest"`
	ExpiresAt time.Time     `json:"expires_at"`
	State     ProposalState `json:"state"`
	// SourceRevision is the object's revision the preview read.
	SourceRevision string `json:"source_revision,omitempty"`
	// PageAction is a page-scope proposal's action, by name and with the
	// payload the screen would apply, so a screen on the tab that declares
	// the action draws its own preview rows and holds Approve with its own
	// refusal before the person presses anything. Changes stay the fallback
	// where no screen does. Never on a server write.
	PageAction *PageActionRef `json:"page_action,omitempty"`
	// ApproveLabel names the effect, never "OK" ("Approve and save",
	// "Apply to this list").
	ApproveLabel string `json:"approve_label"`
	// Receipt is what an applied proposal did.
	Receipt *Receipt `json:"receipt,omitempty"`
	// Failure is the engine's refusal of a failed proposal, and whether a
	// Retry is offered.
	Failure *Failure `json:"failure,omitempty"`
}

// PageActionRef is the page action a page-scope proposal applies: the action's
// name as the screen declares it, and the payload, which the screen parses
// with its own rules before it shows or applies anything.
type PageActionRef struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload"`
}

// Change is one row of a proposal's change list.
type Change struct {
	Kind  ChangeKind `json:"kind"`
	Field string     `json:"field"`
	Label string     `json:"label"`
	// From is the value before, for changed, removed and text; To the
	// value after, for fact, changed, added and text.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Words is a text change as runs of words.
	Words []Word `json:"words,omitempty"`
	// New marks an added field on a record that already exists.
	New  bool   `json:"new,omitempty"`
	Note string `json:"note,omitempty"`
}

// Word is one run of words in a text change.
type Word struct {
	Op   WordOp `json:"op"`
	Text string `json:"text"`
}

// Receipt is what an applied proposal did.
type Receipt struct {
	// Summary is the result in a line ("Created ENG-156").
	Summary string `json:"summary"`
	// Position is where the write landed, for a server write.
	Position string `json:"position,omitempty"`
	Open     *Link  `json:"open,omitempty"`
}

// Failure is a proposal that did not apply.
type Failure struct {
	Refusal   Refusal `json:"refusal"`
	Retryable bool    `json:"retryable"`
}

// OpenInAppCard is a link to a screen or a record, with whether the person may
// open it computed for them by asking the record's read as them.
type OpenInAppCard struct {
	Link      Link       `json:"link"`
	Permitted bool       `json:"permitted"`
	Reason    OpenReason `json:"reason,omitempty"`
}

// ErrorCard is a refusal or a failure in the engine's own words. Phrase is the
// catalogue tool's action phrase ("file tasks"), so "You do not have
// permission to file tasks." is never model text.
type ErrorCard struct {
	Refusal   Refusal `json:"refusal"`
	Phrase    string  `json:"phrase,omitempty"`
	Hint      string  `json:"hint,omitempty"`
	Retryable bool    `json:"retryable,omitempty"`
}

// NoticeCard is a line the engine adds.
type NoticeCard struct {
	Tone NoticeTone `json:"tone"`
	Text string     `json:"text"`
}

// bodies names each kind's body as set or not.
func (c Card) bodies() map[CardKind]bool {
	return map[CardKind]bool{
		CardTable:     c.Table != nil,
		CardEntity:    c.Entity != nil,
		CardProposal:  c.Proposal != nil,
		CardOpenInApp: c.OpenInApp != nil,
		CardError:     c.Error != nil,
		CardNotice:    c.Notice != nil,
		CardFile:      c.File != nil,
	}
}

// Validate reports what is wrong with a card: exactly one body and that it is
// the kind's, and every closed set inside it.
func (c Card) Validate() error {
	if !c.Type.Valid() {
		return fmt.Errorf("%w: card type %q is not one of %v", ErrInvalid, c.Type, CardKinds)
	}
	for kind, set := range c.bodies() {
		if set != (kind == c.Type) {
			if set {
				return fmt.Errorf("%w: a %s card carries a %s body", ErrInvalid, c.Type, kind)
			}
			return fmt.Errorf("%w: a %s card carries no %s body", ErrInvalid, c.Type, kind)
		}
	}
	switch c.Type {
	case CardTable:
		return c.Table.validate()
	case CardEntity:
		if c.Entity.Kind != "" && !c.Entity.Kind.Valid() {
			return fmt.Errorf("%w: entity kind %q is not one of %v", ErrInvalid,
				c.Entity.Kind, ResourceKinds)
		}
		for _, f := range c.Entity.Facts {
			if f.Type != "" && !f.Type.Valid() {
				return fmt.Errorf("%w: fact %q type %q is not one of %v", ErrInvalid,
					f.Label, f.Type, ColumnTypes)
			}
		}
		if n := c.Entity.ItemsNoun; n != nil && (n.One == "" || n.Many == "") {
			return fmt.Errorf("%w: an entity's items noun names both one and many", ErrInvalid)
		}
		for _, item := range c.Entity.Items {
			if item.Kind != "" && !item.Kind.Valid() {
				return fmt.Errorf("%w: entity item kind %q is not one of %v", ErrInvalid,
					item.Kind, ResourceKinds)
			}
		}
	case CardProposal:
		return c.Proposal.validate()
	case CardOpenInApp:
		if c.OpenInApp.Reason != "" && !c.OpenInApp.Reason.Valid() {
			return fmt.Errorf("%w: open reason %q is not one of %v", ErrInvalid,
				c.OpenInApp.Reason, OpenReasons)
		}
		if c.OpenInApp.Permitted != (c.OpenInApp.Reason == "") {
			return fmt.Errorf("%w: an open in app card is permitted exactly when it names "+
				"no reason", ErrInvalid)
		}
	case CardFile:
		return c.File.validate()
	case CardNotice:
		if !c.Notice.Tone.Valid() {
			return fmt.Errorf("%w: notice tone %q is not one of %v", ErrInvalid,
				c.Notice.Tone, NoticeTones)
		}
	}
	return nil
}

func (t *TableCard) validate() error {
	for _, col := range t.Columns {
		if col.Key == "" || !col.Type.Valid() || (col.Align != "" && !col.Align.Valid()) {
			return fmt.Errorf("%w: table column %q is not a key with a type of %v", ErrInvalid,
				col.Key, ColumnTypes)
		}
	}
	for i, row := range t.Rows {
		if len(row) != len(t.Columns) {
			return fmt.Errorf("%w: table row %d has %d cells for %d columns", ErrInvalid,
				i, len(row), len(t.Columns))
		}
	}
	if t.Total != nil && len(t.Total) != len(t.Columns) {
		return fmt.Errorf("%w: table total has %d cells for %d columns", ErrInvalid,
			len(t.Total), len(t.Columns))
	}
	if t.RowCount < len(t.Rows) {
		return fmt.Errorf("%w: table row_count %d is under the %d rows it carries", ErrInvalid,
			t.RowCount, len(t.Rows))
	}
	return nil
}

func (p *ProposalCard) validate() error {
	switch {
	case p.ProposalID == uuid.Nil:
		return fmt.Errorf("%w: a proposal names no proposal_id", ErrInvalid)
	case !p.Scope.Valid():
		return fmt.Errorf("%w: proposal scope %q is not one of %v", ErrInvalid, p.Scope,
			ProposalScopes)
	case !p.State.Valid():
		return fmt.Errorf("%w: proposal state %q is not one of %v", ErrInvalid, p.State,
			ProposalStates)
	case !ValidDigest(p.Digest):
		return fmt.Errorf("%w: proposal digest %q is not %s and 64 lowercase hex digits",
			ErrInvalid, p.Digest, DigestPrefix)
	case p.ApproveLabel == "" || p.Title == "":
		return fmt.Errorf("%w: a proposal names no title or no approve label", ErrInvalid)
	case len(p.Changes) == 0:
		return fmt.Errorf("%w: a proposal shows no change", ErrInvalid)
	case p.PageAction != nil && (p.Scope != ScopePage || p.PageAction.Name == ""):
		return fmt.Errorf("%w: only a page-scope proposal names a page action, and "+
			"it names it", ErrInvalid)
	}
	for _, ch := range p.Changes {
		if !ch.Kind.Valid() {
			return fmt.Errorf("%w: change kind %q is not one of %v", ErrInvalid, ch.Kind,
				ChangeKinds)
		}
		for _, w := range ch.Words {
			if !w.Op.Valid() {
				return fmt.Errorf("%w: word op %q is not one of %v", ErrInvalid, w.Op, WordOps)
			}
		}
		if (ch.Kind == ChangeText) != (len(ch.Words) > 0) {
			return fmt.Errorf("%w: change %s carries words exactly when it is a text change",
				ErrInvalid, ch.Field)
		}
	}
	return nil
}

// DigestPrefix opens a proposal's digest on the wire.
const DigestPrefix = "sha256:"

// ValidDigest reports whether s is a proposal digest as the wire carries it:
// [DigestPrefix] and 64 lowercase hex digits.
func ValidDigest(s string) bool {
	if len(s) != len(DigestPrefix)+64 || s[:len(DigestPrefix)] != DigestPrefix {
		return false
	}
	for _, r := range s[len(DigestPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
