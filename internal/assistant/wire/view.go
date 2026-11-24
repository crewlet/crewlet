package wire

import (
	"encoding/json"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/words"
)

// PageView is an outline of what the screen shows now, read by the dashboard
// from the rendered accessibility tree: an open dialog first, then the main
// pane, in reading order. It says what is on screen (each control's label,
// whether a field is filled, which step is current) and NEVER a field's
// value, so the assistant names controls as the person sees them.
//
// Words on a screen can be anybody's (a task title an agent wrote, a vendor's
// error), so the outline is fenced with the rest of the page context as data
// about the screen, never as instructions.
type PageView struct {
	// Nodes are the outline's entries, in reading order.
	Nodes []ViewNode `json:"nodes"`
	// Trimmed says entries past [PageViewMaxNodes] or [PageViewMaxBytes]
	// were left out.
	Trimmed bool `json:"trimmed,omitempty"`
}

// ViewNode is one entry of a [PageView].
type ViewNode struct {
	Role ViewRole `json:"role"`
	// Name is the entry's accessible name: a heading's or a notice's text,
	// a control's label.
	Name string `json:"name"`
	// Depth is how many named sections or dialogs it sits in.
	Depth int `json:"depth,omitempty"`
	// Level is a heading's level, 1 to 6.
	Level int `json:"level,omitempty"`
	// Field is a field's kind; set exactly on a field.
	Field ViewFieldKind `json:"field,omitempty"`
	// Filled says whether a field holds a value (a box ticked, a choice
	// made); set exactly on a field. The value itself is never sent.
	Filled   *bool `json:"filled,omitempty"`
	Required bool  `json:"required,omitempty"`
	Disabled bool  `json:"disabled,omitempty"`
	// Current marks the selected tab or the step the person is on.
	Current bool `json:"current,omitempty"`
	// Error is the error a field shows under it.
	Error string `json:"error,omitempty"`
}

// ViewRole is what an outline entry is.
type ViewRole string

// The outline's roles: an alert is a notice the page raised as urgent (an
// error), a notice any other status line or callout.
const (
	ViewHeading ViewRole = "heading"
	ViewSection ViewRole = "section"
	ViewDialog  ViewRole = "dialog"
	ViewField   ViewRole = "field"
	ViewButton  ViewRole = "button"
	ViewLink    ViewRole = "link"
	ViewTab     ViewRole = "tab"
	ViewStep    ViewRole = "step"
	ViewAlert   ViewRole = "alert"
	ViewNotice  ViewRole = "notice"
)

// ViewRoles are every outline role, in declaration order.
var ViewRoles = []ViewRole{ViewHeading, ViewSection, ViewDialog, ViewField, ViewButton,
	ViewLink, ViewTab, ViewStep, ViewAlert, ViewNotice}

// Valid reports whether r is a role this build knows.
func (r ViewRole) Valid() bool { return slices.Contains(ViewRoles, r) }

// ViewFieldKind is the shape of a field in the outline.
type ViewFieldKind string

// The field kinds: a secret is a password or key field, a check a box or a
// switch, a choice a select, a combobox or a radio group.
const (
	ViewFieldText   ViewFieldKind = "text"
	ViewFieldSecret ViewFieldKind = "secret"
	ViewFieldChoice ViewFieldKind = "choice"
	ViewFieldCheck  ViewFieldKind = "check"
	ViewFieldFile   ViewFieldKind = "file"
)

// ViewFieldKinds are every field kind, in declaration order.
var ViewFieldKinds = []ViewFieldKind{ViewFieldText, ViewFieldSecret, ViewFieldChoice,
	ViewFieldCheck, ViewFieldFile}

// Valid reports whether k is a field kind this build knows.
func (k ViewFieldKind) Valid() bool { return slices.Contains(ViewFieldKinds, k) }

// The outline's bounds, mirrored in `contract/assistant.ts` and held equal by
// the client gate.
const (
	// PageViewMaxBytes is the most an outline encodes to: half the page
	// context's own budget again, which holds a long setup form's every
	// field, button and notice.
	PageViewMaxBytes = 8 << 10
	// PageViewMaxNodes is the most entries an outline lists.
	PageViewMaxNodes = 200
	// PageViewNameMaxRunes is the longest name an entry carries: a notice
	// of a few sentences.
	PageViewNameMaxRunes = 300
	// PageViewErrorMaxRunes is the longest error a field carries.
	PageViewErrorMaxRunes = 200
	// pageViewMaxDepth bounds nesting: deeper is a layout, not an outline.
	pageViewMaxDepth = 8
)

// validate holds an outline to its bounds: known roles and kinds, a kind and
// a filled state exactly on a field, plain bounded names, and its size.
func (v PageView) validate() error {
	raw, err := json.Marshal(v)
	switch {
	case err != nil:
		return fmt.Errorf("%w: page context view does not encode: %w", ErrInvalid, err)
	case len(raw) > PageViewMaxBytes:
		return fmt.Errorf("%w: page context view is %d bytes, over %d", ErrInvalid, len(raw),
			PageViewMaxBytes)
	case len(v.Nodes) > PageViewMaxNodes:
		return fmt.Errorf("%w: page context view lists %d entries, over %d", ErrInvalid,
			len(v.Nodes), PageViewMaxNodes)
	}
	for i, n := range v.Nodes {
		field := n.Role == ViewField
		switch {
		case !n.Role.Valid():
			return fmt.Errorf("%w: page context view entry %d role %q is not one of %s",
				ErrInvalid, i, n.Role, words.Or(ViewRoles))
		case !plain(n.Name, 4*PageViewNameMaxRunes) ||
			utf8.RuneCountInString(n.Name) > PageViewNameMaxRunes:
			return fmt.Errorf("%w: page context view entry %d name is not plain text of at "+
				"most %d characters", ErrInvalid, i, PageViewNameMaxRunes)
		case !plain(n.Error, 4*PageViewErrorMaxRunes) ||
			utf8.RuneCountInString(n.Error) > PageViewErrorMaxRunes:
			return fmt.Errorf("%w: page context view entry %d error is not plain text of at "+
				"most %d characters", ErrInvalid, i, PageViewErrorMaxRunes)
		case n.Depth < 0 || n.Depth > pageViewMaxDepth:
			return fmt.Errorf("%w: page context view entry %d depth %d is not 0 to %d",
				ErrInvalid, i, n.Depth, pageViewMaxDepth)
		case n.Role == ViewHeading && (n.Level < 1 || n.Level > 6) ||
			n.Role != ViewHeading && n.Level != 0:
			return fmt.Errorf("%w: page context view entry %d level %d is not a heading's 1 "+
				"to 6", ErrInvalid, i, n.Level)
		case field != (n.Field != "") || field != (n.Filled != nil):
			return fmt.Errorf("%w: page context view entry %d states a field kind and whether "+
				"it is filled exactly when it is a field", ErrInvalid, i)
		case field && !n.Field.Valid():
			return fmt.Errorf("%w: page context view entry %d field kind %q is not one of %s",
				ErrInvalid, i, n.Field, words.Or(ViewFieldKinds))
		case !field && (n.Required || n.Error != ""):
			return fmt.Errorf("%w: page context view entry %d is required or in error and "+
				"is not a field", ErrInvalid, i)
		}
	}
	return nil
}
