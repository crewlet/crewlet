package wire

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/words"
)

// PageContext is what a screen tells a turn about itself: WHICH things are on
// screen, by id, and never a record's content; and the draft the person has
// open, if any, with its current values ([PageDraft]).
//
// A HINT TO THE MODEL AND NEVER AUTHORITY. Every fact the model uses about a
// record comes from a tool call made as the person, so a page can neither
// leak a row the person could not read nor forge one; nothing here grants,
// and no grant is sent (the server derives the person from the request). The
// draft is the one exception to "ids only", because an unsaved draft exists
// nowhere a tool could read it: it is what the person typed, sent by their
// own page, and the model reads it as that. Sent with each question, or null
// when the person removed the page chip, and fenced as text a colleague may
// have written when it reaches the prompt, because titles on screen and a
// draft opened from someone else's version are.
type PageContext struct {
	// Screen is the dashboard's screen name (its route table's union).
	Screen string `json:"screen"`
	// Title is the label the frame drew for the chip ("Worklet › Tasks").
	Title string `json:"title"`
	// Params are the screen's own path parameters.
	Params map[string]string `json:"params"`
	// Query is ONLY the keys the screen declares; anything else is dropped
	// by the client and refused by the server's per-screen allowlist.
	Query map[string]string `json:"query"`
	// Resources are what the page is about and what is in view, by id.
	Resources []Resource `json:"resources"`
	// Selection is the one record the person has open, or null: the record
	// a record's screen is of, the row a screen has open beside its list, or
	// the peeked one, with what the screen calls it. A brief turn is limited
	// to it ([TurnBrief]).
	Selection *ResourceRef `json:"selection"`
	// SourceRevision is the revision the screen's state and data were
	// drawn at; a page action prepared against it is refused stale by the
	// page when it moved.
	SourceRevision string `json:"source_revision"`
	// Accepts are the page actions the screen has mounted now.
	Accepts []string `json:"accepts"`
	// ActionArgs are the arguments each accepted action takes on this
	// screen now, as the screen declares them ([ActionArgs]), by action id.
	// Every accepted action declares them, but `draft.set` may go without:
	// its arguments are the open draft's editable fields.
	ActionArgs map[string]*ActionArgs `json:"action_args,omitempty"`
	// ActionRevisions are each accepted action's own revision: what the
	// screen holds that the action depends on, so a change prepared against
	// it goes stale only when THAT moves, never because the person opened a
	// peek or another editor beside it. An action without one is bound to
	// SourceRevision.
	ActionRevisions map[string]string `json:"action_revisions,omitempty"`
	// Draft is the draft the person has open on the screen, or absent
	// where the screen edits nothing.
	Draft *PageDraft `json:"draft,omitempty"`
}

// RevisionFor is the revision a change by action is prepared against: the
// action's own ([PageContext.ActionRevisions]), or the page's where the screen
// states none for it.
func (p PageContext) RevisionFor(action string) string {
	if r, ok := p.ActionRevisions[action]; ok {
		return r
	}
	return p.SourceRevision
}

// PageDraft is what is open in a screen's editor: which draft, where the
// person is in it, and its current values, redacted and bounded. Every
// screen that holds an editable draft or form declares one
// (`useAssistantDraft` in the dashboard), so the assistant reads what is
// open before it searches for it.
type PageDraft struct {
	// Kind says where the values live, and so how a change reaches them.
	Kind DraftKind `json:"kind"`
	// ID names the saved record the draft is of (a kit, a page), absent
	// for a draft nothing is saved of yet.
	ID string `json:"id,omitempty"`
	// Revision is the saved record's version the draft was opened on.
	Revision string `json:"revision,omitempty"`
	// Step is the step or section the person is on (`colours`).
	Step string `json:"step,omitempty"`
	// Help is the screen's own word to the assistant: what it can and
	// cannot do with this draft.
	Help string `json:"help,omitempty"`
	// Fields are the draft's values, in the order the screen draws them.
	Fields []DraftField `json:"fields"`
	// More counts the fields left out past [PageDraftMaxFields] or the
	// context's byte budget.
	More int `json:"more"`
}

// DraftField is one value of an open draft.
type DraftField struct {
	// Path names the field within the draft (`colors.primary`), in the
	// page context's key grammar; a change names it.
	Path string `json:"path"`
	// Label is the field as the person reads it ("Primary colour").
	Label string `json:"label"`
	// Kind is the field's shape, and so of the value a change to it takes.
	Kind DraftFieldKind `json:"kind"`
	// Value is the current value as text: a list's items one per line, a
	// file's name. Empty, and Redacted set, for a credential.
	Value string `json:"value"`
	// Hint says what a value must be ("a #RRGGBB colour").
	Hint string `json:"hint,omitempty"`
	// Editable marks a field the screen's `draft.set` action takes.
	Editable bool `json:"editable,omitempty"`
	// Redacted marks a credential, whose value never leaves the page.
	Redacted bool `json:"redacted,omitempty"`
	// Truncated marks a value cut at [PageDraftValueMaxRunes].
	Truncated bool `json:"truncated,omitempty"`
}

// DraftFileRef is the value a change to a file field takes: a file of the
// person's own conversation, by id, which the screen fetches through the
// owner's route and keeps through its own save. Never the file's bytes or its
// markup, so nothing the model wrote is ever drawn or stored as a file.
type DraftFileRef struct {
	// Kind is always [DraftFieldFile], so a reference is told apart from a
	// value by its shape alone.
	Kind DraftFieldKind `json:"kind"`
	// FileID names the file among the conversation's.
	FileID uuid.UUID `json:"file_id"`
	// ConversationID is the conversation the file belongs to: the turn's own,
	// which is what the screen fetches it under.
	ConversationID uuid.UUID `json:"conversation_id"`
	// Name is the file's name, as the card and the screen show it.
	Name string `json:"name"`
	// MediaType is the file's type, from the store that holds it.
	MediaType string `json:"media_type"`
}

// mediaTypePattern is a media type with no parameters: `image/svg+xml`.
// Compiled at first use, not at init: the code host's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
var mediaTypePattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,63}/[a-z0-9][a-z0-9!#$&^_.+-]{0,63}$`)
})

// draftFileNameMaxBytes bounds a file reference's name: a file name, never a
// path.
const draftFileNameMaxBytes = 200

// Validate reports what is wrong with a file reference: its kind, its ids,
// a name that is a plain file name, and a media type in its grammar.
func (r DraftFileRef) Validate() error {
	switch {
	case r.Kind != DraftFieldFile:
		return fmt.Errorf("%w: a file reference has kind %q, not %q", ErrInvalid, r.Kind,
			DraftFieldFile)
	case r.FileID == uuid.Nil || r.ConversationID == uuid.Nil:
		return fmt.Errorf("%w: a file reference names no file or no conversation", ErrInvalid)
	case r.Name == "" || !plain(r.Name, draftFileNameMaxBytes) || strings.ContainsAny(r.Name, "/\\"):
		return fmt.Errorf("%w: a file reference's name is not a file name of at most %d bytes",
			ErrInvalid, draftFileNameMaxBytes)
	case !mediaTypePattern().MatchString(r.MediaType):
		return fmt.Errorf("%w: a file reference's media type %q is not one", ErrInvalid, r.MediaType)
	}
	return nil
}

// PageDraftSecretPath matches a draft field path that names a credential: a
// segment (between `.` and `_`) that is a password, a secret, a token, a
// credential or a key of the kinds that sign or authenticate. Such a field
// must arrive redacted. A backstop over the path's grammar: a screen marks
// its credential fields itself, and this catches the one it forgot. Written
// with no flag and no escape so the dashboard compiles the same text
// (`ASSISTANT_PAGE_DRAFT_SECRET_PATH`, held equal by the client gate).
const PageDraftSecretPath = "(^|[._])(password|passphrase|secret|secrets|token|tokens|" +
	"credential|credentials|(api|private|signing|access|secret)[._]?key)([._]|$)"

// Compiled at first use, not at init: the code host's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
var secretPath = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(PageDraftSecretPath)
})

// IsSecretDraftPath reports whether a draft field's path names a credential.
func IsSecretDraftPath(path string) bool { return secretPath().MatchString(path) }

// Resource is one thing in view, by id.
type Resource struct {
	Kind    ResourceKind `json:"kind"`
	ID      string       `json:"id"`
	Version string       `json:"version,omitempty"`
}

// ResourceRef names one thing.
type ResourceRef struct {
	Kind ResourceKind `json:"kind"`
	ID   string       `json:"id"`
	// Noun is what the person calls it where the screen says ([RecordNoun]):
	// a selection's word for itself ("decision" for an ask in the Inbox),
	// absent where its kind says enough.
	Noun RecordNoun `json:"noun,omitempty"`
}

// The grammars a page context's names are held to.
var (
	// Compiled at first use, not at init: the code host's push hook is this
	// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
	screenPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	})
	// Compiled at first use, not at init: the code host's push hook is this
	// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
	keyPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
	})
	// actionPattern is a page action id: dotted lowercase segments,
	// `work.filter.apply`.
	// Compiled at first use, not at init: the code host's push hook is this
	// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
	actionPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	})
)

// The bounds on a page context's strings, in bytes unless named in runes.
const (
	pageTitleMaxRunes = 120
	pageValueMaxBytes = 512
	pageIDMaxBytes    = 200
	pageMapMaxKeys    = 32
	actionIDMaxBytes  = 64
)

// ValidActionID reports whether s is a page action id a screen may declare:
// dotted lowercase segments, and never the reserved [ActionContextInvalidate].
func ValidActionID(s string) bool {
	return len(s) <= actionIDMaxBytes && actionPattern().MatchString(s) &&
		s != ActionContextInvalidate
}

// Validate reports what is wrong with a page context, naming the field: its
// size, every name's grammar and every bound. It decides nothing about which
// query keys a screen may send, which is the catalogue's per-screen table.
func (p PageContext) Validate() error {
	raw, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("%w: page context does not encode: %w", ErrInvalid, err)
	}
	switch {
	case len(raw) > PageContextMaxBytes:
		return fmt.Errorf("%w: page context is %d bytes, over %d", ErrInvalid, len(raw),
			PageContextMaxBytes)
	case !screenPattern().MatchString(p.Screen):
		return fmt.Errorf("%w: page context screen %q is not a screen name", ErrInvalid, p.Screen)
	case !plain(p.Title, 4*pageTitleMaxRunes) || utf8.RuneCountInString(p.Title) > pageTitleMaxRunes:
		return fmt.Errorf("%w: page context title is not plain text of at most %d characters",
			ErrInvalid, pageTitleMaxRunes)
	case len(p.Resources) > PageContextMaxResources:
		return fmt.Errorf("%w: page context names %d resources, over %d", ErrInvalid,
			len(p.Resources), PageContextMaxResources)
	case len(p.Accepts) > PageContextMaxAccepts:
		return fmt.Errorf("%w: page context offers %d actions, over %d", ErrInvalid,
			len(p.Accepts), PageContextMaxAccepts)
	case !plain(p.SourceRevision, pageIDMaxBytes):
		return fmt.Errorf("%w: page context source_revision is not plain text of at most %d "+
			"bytes", ErrInvalid, pageIDMaxBytes)
	}
	for field, m := range map[string]map[string]string{"params": p.Params, "query": p.Query} {
		if len(m) > pageMapMaxKeys {
			return fmt.Errorf("%w: page context %s has %d keys, over %d", ErrInvalid, field,
				len(m), pageMapMaxKeys)
		}
		for k, v := range m {
			if !keyPattern().MatchString(k) || !plain(v, pageValueMaxBytes) {
				return fmt.Errorf("%w: page context %s key %q is not a name with a plain "+
					"value of at most %d bytes", ErrInvalid, field, k, pageValueMaxBytes)
			}
		}
	}
	for _, r := range p.Resources {
		if err := (ResourceRef{Kind: r.Kind, ID: r.ID}).validate(); err != nil {
			return err
		}
		if !plain(r.Version, pageIDMaxBytes) {
			return fmt.Errorf("%w: resource %s version is not plain text", ErrInvalid, r.ID)
		}
	}
	if p.Selection != nil {
		if err := p.Selection.validate(); err != nil {
			return err
		}
	}
	for _, a := range p.Accepts {
		if !ValidActionID(a) {
			return fmt.Errorf("%w: page context accepts %q, which is not a page action id",
				ErrInvalid, a)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(p.ActionArgs)) {
		switch {
		case !slices.Contains(p.Accepts, id):
			return fmt.Errorf("%w: page context declares arguments for %q, which it does not "+
				"accept", ErrInvalid, id)
		case p.ActionArgs[id] == nil:
			return fmt.Errorf("%w: page context declares no schema for %q", ErrInvalid, id)
		}
		if err := p.ActionArgs[id].Validate(); err != nil {
			return fmt.Errorf("%w (action %s)", err, id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(p.ActionRevisions)) {
		switch {
		case !slices.Contains(p.Accepts, id):
			return fmt.Errorf("%w: page context states a revision for %q, which it does not "+
				"accept", ErrInvalid, id)
		case p.ActionRevisions[id] == "" || !plain(p.ActionRevisions[id], pageIDMaxBytes):
			return fmt.Errorf("%w: page context revision for %q is not plain text of at most "+
				"%d bytes", ErrInvalid, id, pageIDMaxBytes)
		}
	}
	if p.Draft != nil {
		return p.Draft.validate()
	}
	return nil
}

// The bounds on a draft's own strings.
const (
	draftHintMaxRunes = 200
	draftIDMaxBytes   = pageIDMaxBytes
)

// validate holds a draft to its bounds: a known kind, an id for a saved
// draft, names in the key grammar, each value text of bounded length, and
// every credential field redacted.
func (d PageDraft) validate() error {
	switch {
	case !d.Kind.Valid():
		return fmt.Errorf("%w: page context draft kind %q is not one of %s", ErrInvalid, d.Kind,
			words.Or(DraftKinds))
	case d.Kind == DraftServer && d.ID == "":
		return fmt.Errorf("%w: page context draft is saved and names no id", ErrInvalid)
	case !plain(d.ID, draftIDMaxBytes) || !plain(d.Revision, draftIDMaxBytes):
		return fmt.Errorf("%w: page context draft id or revision is not plain text of at most "+
			"%d bytes", ErrInvalid, draftIDMaxBytes)
	case d.Step != "" && !keyPattern().MatchString(d.Step):
		return fmt.Errorf("%w: page context draft step %q is not a name", ErrInvalid, d.Step)
	case !text(d.Help, PageDraftHelpMaxRunes):
		return fmt.Errorf("%w: page context draft help is not text of at most %d characters",
			ErrInvalid, PageDraftHelpMaxRunes)
	case len(d.Fields) > PageDraftMaxFields:
		return fmt.Errorf("%w: page context draft lists %d fields, over %d", ErrInvalid,
			len(d.Fields), PageDraftMaxFields)
	case d.More < 0:
		return fmt.Errorf("%w: page context draft counts %d more fields", ErrInvalid, d.More)
	}
	seen := make(map[string]bool, len(d.Fields))
	for _, f := range d.Fields {
		switch {
		case !keyPattern().MatchString(f.Path):
			return fmt.Errorf("%w: page context draft field path %q is not a name", ErrInvalid,
				f.Path)
		case seen[f.Path]:
			return fmt.Errorf("%w: page context draft lists field %q twice", ErrInvalid, f.Path)
		case !plain(f.Label, 4*pageTitleMaxRunes) ||
			utf8.RuneCountInString(f.Label) > pageTitleMaxRunes:
			return fmt.Errorf("%w: page context draft field %q label is not plain text of at "+
				"most %d characters", ErrInvalid, f.Path, pageTitleMaxRunes)
		case !text(f.Value, PageDraftValueMaxRunes):
			return fmt.Errorf("%w: page context draft field %q value is not text of at most %d "+
				"characters", ErrInvalid, f.Path, PageDraftValueMaxRunes)
		case !plain(f.Hint, 4*draftHintMaxRunes) ||
			utf8.RuneCountInString(f.Hint) > draftHintMaxRunes:
			return fmt.Errorf("%w: page context draft field %q hint is not plain text of at "+
				"most %d characters", ErrInvalid, f.Path, draftHintMaxRunes)
		case !f.Kind.Valid():
			return fmt.Errorf("%w: page context draft field %q kind %q is not one of %s",
				ErrInvalid, f.Path, f.Kind, words.Or(DraftFieldKinds))
		case f.Redacted && (f.Value != "" || f.Editable):
			return fmt.Errorf("%w: page context draft field %q is redacted and carries a value "+
				"or takes one", ErrInvalid, f.Path)
		case IsSecretDraftPath(f.Path) && !f.Redacted:
			// A CREDENTIAL NEVER REACHES A MODEL: the dashboard redacts a
			// field by this same rule before it sends, so one that arrives
			// whole is a page that skipped it.
			return fmt.Errorf("%w: page context draft field %q names a credential and is not "+
				"redacted", ErrInvalid, f.Path)
		}
		seen[f.Path] = true
	}
	return nil
}

// text reports whether s is valid UTF-8 of at most max runes whose only
// control characters are line breaks and tabs: a draft's value, which may run
// over lines. The prompt carries it JSON-encoded inside a fence, so a line
// break in it is an escape sequence and never a line of the prompt.
func text(s string, max int) bool {
	if len(s) > 4*max || !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

func (r ResourceRef) validate() error {
	if !r.Kind.Valid() {
		return fmt.Errorf("%w: resource kind %q is not one of %s", ErrInvalid, r.Kind,
			words.Or(ResourceKinds))
	}
	if r.ID == "" || !plain(r.ID, pageIDMaxBytes) {
		return fmt.Errorf("%w: a %s resource id is empty or not plain text of at most %d bytes",
			ErrInvalid, r.Kind, pageIDMaxBytes)
	}
	if r.Noun != "" && !r.Noun.Valid() {
		return fmt.Errorf("%w: resource noun %q is not one of %s", ErrInvalid, r.Noun,
			words.Or(RecordNouns))
	}
	return nil
}

// plain reports whether s is valid UTF-8 of at most max bytes with no control
// character: a value that can be fenced and logged as an id without carrying a
// line break into either.
func plain(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
