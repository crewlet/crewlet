package wire

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/words"
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
	Oversize  *OversizeCard  `json:"oversize,omitempty"`
	// --- deep work (im-deep) ---
	File *FileCard `json:"file,omitempty"`
	// Brief is a brief turn's answer ([TurnBrief]).
	Brief *BriefCard `json:"brief,omitempty"`
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
	RowCount  int  `json:"row_count"`
	Truncated bool `json:"truncated"`
	// More is a read whose answer says rows lie past the page it read (a
	// cursor, a "has more"), so RowCount is a page and never the total: the
	// card's screen shows the rest. The drawer says so on the card itself
	// rather than in a notice beside it (ADR-0061).
	More bool      `json:"more,omitempty"`
	AsOf time.Time `json:"as_of"`
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
	Kind ResourceKind `json:"kind,omitempty"`
	ID   string       `json:"id"`
	// Key is the short name the record is written by in prose, where it has
	// one: a task's key ("GOP-11"), a project's ("GOP"), a room's name with
	// its "#" ("#launch"). The drawer makes the answer's first mention of it
	// the record's chip. A record with no key is matched by its Title, and
	// only where the title is a name a person writes (a seat's, a team's),
	// never a task's sentence (dashboard app/assistant/mentions.ts). A seat
	// has none: the answer names a seat by its name, never its handle. Nor
	// has a direct conversation, which has no name: it is headed by its
	// people, which no answer is matched against.
	Key      string `json:"key,omitempty"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	Facts    []Fact `json:"facts,omitempty"`
	// Text is a body the record carries, rendered in the restricted
	// Markdown subset: no images, no raw HTML, no autolinks.
	Text  string       `json:"text,omitempty"`
	Items []EntityItem `json:"items,omitempty"`
	// ItemsTotal is how many items the record holds where Items is only the
	// first of them; absent where Items is the whole list.
	ItemsTotal int `json:"items_total,omitempty"`
	// ItemsMore is a record whose answer says items lie past the page it
	// read (a thread's older comments), so Items is a page and never the
	// whole list: the record's screen shows the rest (ADR-0061).
	ItemsMore bool `json:"items_more,omitempty"`
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
	// Summary is what the change does, in a sentence above the rows, where
	// the title alone does not say it: what a destructive change takes away.
	Summary string `json:"summary,omitempty"`
	// Note is the consequence, in one sentence, under the rows: whether and
	// how the change can be reverted.
	Note string `json:"note,omitempty"`
	// Risk is how much care the approval asks ([ProposalRisk]); absent for
	// an ordinary change.
	Risk ProposalRisk `json:"risk,omitempty"`
	// Technical is the exact request the approval sends, for an approver
	// who administers the company: what the rows describe, in the API's own
	// terms. Absent for everybody else, who reviews the rows, which are
	// built from the same request.
	Technical *TechnicalRequest `json:"technical,omitempty"`
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

// TechnicalRequest is a proposal's request as an administrator reads it behind
// the card's "Technical details": its method, its path with the query, and its
// body, indented and redacted.
type TechnicalRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body,omitempty"`
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
	// Items is To as a list, one entry per item, where the value is a list of
	// plain values (a seat's responsibilities, a voice's rules); FromItems is
	// From as one. To and From still hold the list as one line, for a reader
	// that draws one.
	Items     []string `json:"items,omitempty"`
	FromItems []string `json:"from_items,omitempty"`
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

// OversizeCard is a read whose answer was past what one step reads, so none
// of it is shown: what it read, where all of it is, and what would narrow it.
// The drawer lists it with the turn's other reads (ADR-0061), never as a
// result.
type OversizeCard struct {
	// Title is what the read was, as its card would have been titled
	// ("Matching tasks, \"brand\"") or its step said ("Read this agent").
	Title string `json:"title"`
	// Open is the screen that shows the whole of it, where the engine's link
	// table has one the read names.
	Open *Link `json:"open,omitempty"`
	// Narrow names what the read takes that would narrow it, in the words a
	// person reads ("project", "status"), where it takes any.
	Narrow []string `json:"narrow,omitempty"`
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
		CardOversize:  c.Oversize != nil,
		CardFile:      c.File != nil,
		CardBrief:     c.Brief != nil,
	}
}

// Validate reports what is wrong with a card: exactly one body and that it is
// the kind's, and every closed set inside it.
func (c Card) Validate() error {
	if !c.Type.Valid() {
		return fmt.Errorf("%w: card type %q is not one of %s", ErrInvalid, c.Type, words.Or(CardKinds))
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
			return fmt.Errorf("%w: entity kind %q is not one of %s", ErrInvalid,
				c.Entity.Kind, words.Or(ResourceKinds))
		}
		for _, f := range c.Entity.Facts {
			if f.Type != "" && !f.Type.Valid() {
				return fmt.Errorf("%w: fact %q type %q is not one of %s", ErrInvalid,
					f.Label, f.Type, words.Or(ColumnTypes))
			}
		}
		if n := c.Entity.ItemsNoun; n != nil && (n.One == "" || n.Many == "") {
			return fmt.Errorf("%w: an entity's items noun names both one and many", ErrInvalid)
		}
		for _, item := range c.Entity.Items {
			if item.Kind != "" && !item.Kind.Valid() {
				return fmt.Errorf("%w: entity item kind %q is not one of %s", ErrInvalid,
					item.Kind, words.Or(ResourceKinds))
			}
		}
	case CardProposal:
		return c.Proposal.validate()
	case CardOpenInApp:
		if c.OpenInApp.Reason != "" && !c.OpenInApp.Reason.Valid() {
			return fmt.Errorf("%w: open reason %q is not one of %s", ErrInvalid,
				c.OpenInApp.Reason, words.Or(OpenReasons))
		}
		if c.OpenInApp.Permitted != (c.OpenInApp.Reason == "") {
			return fmt.Errorf("%w: an open in app card is permitted exactly when it names "+
				"no reason", ErrInvalid)
		}
	case CardFile:
		return c.File.validate()
	case CardBrief:
		return c.Brief.Validate()
	case CardOversize:
		if c.Oversize.Title == "" {
			return fmt.Errorf("%w: an oversize card names what it read", ErrInvalid)
		}
	case CardNotice:
		if !c.Notice.Tone.Valid() {
			return fmt.Errorf("%w: notice tone %q is not one of %s", ErrInvalid,
				c.Notice.Tone, words.Or(NoticeTones))
		}
	}
	return nil
}

func (t *TableCard) validate() error {
	for _, col := range t.Columns {
		if col.Key == "" || !col.Type.Valid() || (col.Align != "" && !col.Align.Valid()) {
			return fmt.Errorf("%w: table column %q is not a key with a type of %s", ErrInvalid,
				col.Key, words.Or(ColumnTypes))
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
		return fmt.Errorf("%w: proposal scope %q is not one of %s", ErrInvalid, p.Scope,
			words.Or(ProposalScopes))
	case !p.State.Valid():
		return fmt.Errorf("%w: proposal state %q is not one of %s", ErrInvalid, p.State,
			words.Or(ProposalStates))
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
	case p.Risk != "" && !p.Risk.Valid():
		return fmt.Errorf("%w: proposal risk %q is not one of %s", ErrInvalid, p.Risk,
			words.Or(ProposalRisks))
	case p.Technical != nil && (p.Technical.Method == "" || p.Technical.Path == ""):
		return fmt.Errorf("%w: a proposal's technical details name the method and the path",
			ErrInvalid)
	}
	for _, ch := range p.Changes {
		if !ch.Kind.Valid() {
			return fmt.Errorf("%w: change kind %q is not one of %s", ErrInvalid, ch.Kind,
				words.Or(ChangeKinds))
		}
		for _, w := range ch.Words {
			if !w.Op.Valid() {
				return fmt.Errorf("%w: word op %q is not one of %s", ErrInvalid, w.Op, words.Or(WordOps))
			}
		}
		if (ch.Kind == ChangeText) != (len(ch.Words) > 0) {
			return fmt.Errorf("%w: change %s carries words exactly when it is a text change",
				ErrInvalid, ch.Field)
		}
	}
	return nil
}

// BriefCard is a brief: what the page or its open record is about, and the
// things the person has to do, ranked, each with the one press that deals
// with it.
//
// THE VERDICT IS NOT A FIELD. Whether anything needs the person is the
// number of rows ([BriefCard.Verdict]), so no writer can put "nothing needs
// you" beside a row that needs them. The first row is the recommendation: a
// separate next step would say it again.
//
// THE MODEL WRITES THE WORDS, THE ENGINE EVERYTHING ELSE. Each row's record is
// one this turn read and resolved for the person, its address is the link
// table's, its danger tone is a fact the engine read, and an action can only
// open that address or ask a question. A brief never writes.
type BriefCard struct {
	// Scope is what the brief covers: the page, or the one record it has
	// open.
	Scope BriefScope `json:"scope"`
	// About is what the page or record is about right now, in one line.
	About string `json:"about"`
	// Needs are what the person has to do, most pressing first, at most
	// [BriefMaxNeeds]; none is "Nothing needs you".
	Needs []BriefNeed `json:"needs"`
	// Points summarise what was said, each citing the message it came
	// from, at most [BriefMaxPoints]: a brief of a CHAT ROOM's alone (its
	// catch up), absent on every other. The messages the person has not
	// read, or the latest when nothing is unread.
	Points []BriefPoint `json:"points,omitempty"`
	// Context is one fact from elsewhere that bears on what to do, absent
	// where none does.
	Context string `json:"context,omitempty"`
	// FollowUps are questions to ask next, at most [BriefMaxFollowUps].
	FollowUps []string `json:"follow_ups"`
	// AsOf is when the engine finished reading for it.
	AsOf time.Time `json:"as_of"`
}

// Verdict is whether anything needs the person, in the words the
// conversation the model re-reads says it: derived from the rows, never
// written.
func (b *BriefCard) Verdict() string {
	switch n := len(b.Needs); n {
	case 0:
		return "Nothing needs you"
	case 1:
		return "1 thing needs you"
	default:
		return fmt.Sprintf("%d things need you", n)
	}
}

// BriefScope is what a brief covers: the screen, and the record it has open
// where the brief is of that one record.
type BriefScope struct {
	Screen string `json:"screen"`
	// Title is the page's chip ("Worklet › Tasks").
	Title string `json:"title"`
	// Record is the open record the brief is limited to, absent for a brief
	// of the whole page.
	Record *ResourceRef `json:"record,omitempty"`
}

// BriefNeed is one thing the person has to do, citing its record.
type BriefNeed struct {
	Ref ResourceRef `json:"ref"`
	// Text is what to do, as an instruction naming the record.
	Text string `json:"text"`
	// Detail is why now, in one fact.
	Detail string       `json:"detail,omitempty"`
	Tone   BriefTone    `json:"tone"`
	Action *BriefAction `json:"action"`
}

// BriefPoint is one point of a room's summary, citing its message.
//
// THE MODEL WRITES THE WORDS AND NAMES THE MESSAGE; THE ENGINE KEEPS THE
// POINT only when the message appears in what this turn's own reads answered
// and its own read, sent again as the person, answers it, and builds the
// source from that read: the room, the message and the thread it is in.
type BriefPoint struct {
	Text   string      `json:"text"`
	Source BriefSource `json:"source"`
}

// BriefSource is the message a point came from, as the engine read it.
type BriefSource struct {
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"`
	// ThreadRoot is the thread the message is in, absent on a room post:
	// what lets a screen open the thread rather than the room.
	ThreadRoot string `json:"thread_root,omitempty"`
}

// BriefAction is the one press a row offers: open a record's screen at the
// address the engine built, or ask a question.
type BriefAction struct {
	Kind  BriefActionKind `json:"kind"`
	Label string          `json:"label"`
	// Link is an open action's address, from the link table.
	Link *Link `json:"link,omitempty"`
	// Question is an ask action's question.
	Question string `json:"question,omitempty"`
}

// The bounds on a brief, in characters: each line one glance in a 420 pixel
// drawer (about 60 characters a line at the card's size), so the about line
// and a row's instruction are at most two lines, a row's reason and every
// question one or two. Three rows is a short list the reader takes in at
// once; two follow-ups are the next questions, not a menu. Together they draw
// at most [BriefBudgetRunes] characters of the model's words, where the shape
// before them allowed 1,520.
const (
	BriefMaxNeeds         = 3
	BriefMaxFollowUps     = 2
	BriefAboutMaxRunes    = 90
	BriefTextMaxRunes     = 80
	BriefDetailMaxRunes   = 60
	BriefContextMaxRunes  = 100
	BriefQuestionMaxRunes = 80
	briefLabelMaxRunes    = 40

	// BriefMaxPoints and BriefPointMaxRunes bound a room's summary: four or
	// five points is what a catch up of an afternoon's unread holds, and a
	// point is one or two sentences, about two lines of the pane (a
	// decision and who leans which way, a result and what follows it).
	BriefMaxPoints     = 5
	BriefPointMaxRunes = 140
)

// BriefBudgetRunes is the most characters of the model's words a brief
// draws, every cap at once: the about line, three rows of an instruction and
// a reason, the context line and two follow-ups (90 + 3 × (80 + 60) + 100 +
// 2 × 80), and a room's five summary points (5 × 140). An ask's question is
// not counted: it is asked, never drawn. A cap raised past this budget is a
// brief made longer, and a test refuses it until the budget is raised on
// purpose: it was, by the points, for a room's catch up, which is read in the
// room's own pane rather than in the drawer.
const BriefBudgetRunes = 1470

// Validate reports what is wrong with a brief: its bounds, every closed set,
// and that each row carries the one action its kind needs.
func (b *BriefCard) Validate() error {
	switch {
	case !screenPattern.MatchString(b.Scope.Screen):
		return fmt.Errorf("%w: a brief's scope screen %q is not a screen name", ErrInvalid,
			b.Scope.Screen)
	case !briefText(b.Scope.Title, pageTitleMaxRunes):
		return fmt.Errorf("%w: a brief's scope title is not one line of at most %d characters",
			ErrInvalid, pageTitleMaxRunes)
	case b.About == "" || !briefText(b.About, BriefAboutMaxRunes):
		return fmt.Errorf("%w: a brief's about is not one line of 1 to %d characters",
			ErrInvalid, BriefAboutMaxRunes)
	case !briefText(b.Context, BriefContextMaxRunes):
		return fmt.Errorf("%w: a brief's context is not one line of at most %d characters",
			ErrInvalid, BriefContextMaxRunes)
	case len(b.Needs) > BriefMaxNeeds:
		return fmt.Errorf("%w: a brief lists %d needs, over %d", ErrInvalid, len(b.Needs),
			BriefMaxNeeds)
	case len(b.FollowUps) > BriefMaxFollowUps:
		return fmt.Errorf("%w: a brief offers %d follow-ups, over %d", ErrInvalid,
			len(b.FollowUps), BriefMaxFollowUps)
	case b.AsOf.IsZero():
		return fmt.Errorf("%w: a brief says no as_of", ErrInvalid)
	}
	if b.Scope.Record != nil {
		if err := b.Scope.Record.validate(); err != nil {
			return err
		}
	}
	cited := map[ResourceRef]bool{}
	for i, n := range b.Needs {
		switch {
		case cited[n.Ref]:
			return fmt.Errorf("%w: brief need %d cites %s %s, which an earlier need cites",
				ErrInvalid, i, n.Ref.Kind, n.Ref.ID)
		case n.Text == "" || !briefText(n.Text, BriefTextMaxRunes):
			return fmt.Errorf("%w: brief need %d is not one line of 1 to %d characters",
				ErrInvalid, i, BriefTextMaxRunes)
		case !briefText(n.Detail, BriefDetailMaxRunes):
			return fmt.Errorf("%w: brief need %d's detail is not one line of at most %d "+
				"characters", ErrInvalid, i, BriefDetailMaxRunes)
		case !n.Tone.Valid():
			return fmt.Errorf("%w: brief need %d tone %q is not one of %s", ErrInvalid, i,
				n.Tone, words.Or(BriefTones))
		case n.Action == nil:
			return fmt.Errorf("%w: brief need %d offers no action", ErrInvalid, i)
		}
		if err := n.Ref.validate(); err != nil {
			return err
		}
		if err := n.Action.validate(); err != nil {
			return fmt.Errorf("%w (brief need %d)", err, i)
		}
		cited[n.Ref] = true
	}
	if len(b.Points) > 0 && (b.Scope.Record == nil || b.Scope.Record.Kind != ResourceRoom) {
		return fmt.Errorf("%w: a brief carries points, and only a chat room's brief "+
			"summarises what was said", ErrInvalid)
	}
	if len(b.Points) > BriefMaxPoints {
		return fmt.Errorf("%w: a brief carries %d points, over %d", ErrInvalid,
			len(b.Points), BriefMaxPoints)
	}
	for i, p := range b.Points {
		switch {
		case p.Text == "" || !briefText(p.Text, BriefPointMaxRunes):
			return fmt.Errorf("%w: brief point %d is not one line of 1 to %d characters",
				ErrInvalid, i, BriefPointMaxRunes)
		case p.Source.ChannelID == "" || p.Source.MessageID == "":
			return fmt.Errorf("%w: brief point %d names no message", ErrInvalid, i)
		}
	}
	for i, q := range b.FollowUps {
		if q == "" || !briefText(q, BriefQuestionMaxRunes) {
			return fmt.Errorf("%w: brief follow-up %d is not one line of 1 to %d characters",
				ErrInvalid, i, BriefQuestionMaxRunes)
		}
	}
	return nil
}

// validate holds an action to its kind: an open carries a link from the link
// table's grammar and no question, an ask a question and no link.
func (a *BriefAction) validate() error {
	switch {
	case !a.Kind.Valid():
		return fmt.Errorf("%w: brief action kind %q is not one of %s", ErrInvalid, a.Kind,
			words.Or(BriefActionKinds))
	case a.Label == "" || !briefText(a.Label, briefLabelMaxRunes):
		return fmt.Errorf("%w: a brief action's label is not one line of 1 to %d characters",
			ErrInvalid, briefLabelMaxRunes)
	case a.Kind == BriefOpen && (a.Link == nil || a.Question != "" ||
		!strings.HasPrefix(a.Link.Href, "#/") || !screenPattern.MatchString(a.Link.Screen)):
		return fmt.Errorf("%w: a brief's open action carries a dashboard link and no question",
			ErrInvalid)
	case a.Kind == BriefAsk && (a.Link != nil || a.Question == "" ||
		!briefText(a.Question, BriefQuestionMaxRunes)):
		return fmt.Errorf("%w: a brief's ask action carries a question of at most %d "+
			"characters and no link", ErrInvalid, BriefQuestionMaxRunes)
	}
	return nil
}

// briefText reports whether s is one plain line of at most max characters: a
// brief's words are drawn as they are, never as Markdown, and never over
// lines.
func briefText(s string, max int) bool {
	return plain(s, 4*max) && utf8.RuneCountInString(s) <= max
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
