package wire

import (
	"encoding/json"
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// PageContext is what a screen tells a turn about itself: WHICH things are on
// screen, by id, and never their content.
//
// A HINT TO THE MODEL AND NEVER AUTHORITY. Every fact the model uses comes
// from a tool call made as the person, so a page can neither leak a row the
// person could not read nor forge one; nothing here grants, and no grant is
// sent (the server derives the person from the request). Sent with each
// question, or null when the person removed the page chip, and fenced as text
// a colleague may have written when it reaches the prompt, because titles on
// screen are.
type PageContext struct {
	// Screen is the dashboard's screen name (its route table's union).
	Screen string `json:"screen"`
	// Title is the label the frame drew for the chip ("Work › Tasks").
	Title string `json:"title"`
	// Params are the screen's own path parameters.
	Params map[string]string `json:"params"`
	// Query is ONLY the keys the screen declares; anything else is dropped
	// by the client and refused by the server's per-screen allowlist.
	Query map[string]string `json:"query"`
	// Resources are what the page is about and what is in view, by id.
	Resources []Resource `json:"resources"`
	// Selection is the peeked or selected row, or null.
	Selection *ResourceRef `json:"selection"`
	// SourceRevision is the revision the screen's state and data were
	// drawn at; a page action prepared against it is refused stale by the
	// page when it moved.
	SourceRevision string `json:"source_revision"`
	// Accepts are the page actions the screen has mounted now.
	Accepts []string `json:"accepts"`
}

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
}

// The grammars a page context's names are held to.
var (
	screenPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	keyPattern    = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
	// actionPattern is a page action id: dotted lowercase segments,
	// `work.filter.apply`.
	actionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
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
	return len(s) <= actionIDMaxBytes && actionPattern.MatchString(s) &&
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
	case !screenPattern.MatchString(p.Screen):
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
			if !keyPattern.MatchString(k) || !plain(v, pageValueMaxBytes) {
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
	return nil
}

func (r ResourceRef) validate() error {
	if !r.Kind.Valid() {
		return fmt.Errorf("%w: resource kind %q is not one of %v", ErrInvalid, r.Kind, ResourceKinds)
	}
	if r.ID == "" || !plain(r.ID, pageIDMaxBytes) {
		return fmt.Errorf("%w: a %s resource id is empty or not plain text of at most %d bytes",
			ErrInvalid, r.Kind, pageIDMaxBytes)
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
