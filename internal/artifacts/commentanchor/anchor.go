package commentanchor

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/words"
)

// Kind is what an anchor points at.
type Kind string

const (
	// KindVersion is the whole version: every kind takes it.
	KindVersion Kind = "version"

	// KindHTML is a place on one page of an html artifact: a name the page
	// declares, else a point on the page.
	KindHTML Kind = "html"

	// KindSlides is one slide of a deck, or one element on it.
	KindSlides Kind = "slides"

	// KindDesign is a point on one artboard of a design, in the board's own
	// pixels.
	KindDesign Kind = "design"
)

// Kinds are every anchor kind this build reads.
var Kinds = []Kind{KindVersion, KindHTML, KindSlides, KindDesign}

// Valid reports whether k is an anchor kind this build reads.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// Point is a place on a page as fractions of the page document's width and
// height, each in [0, 1], so it survives a change of window size.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Anchor is where a thread points, a union keyed by Kind: each kind uses its
// own fields and leaves the others empty, so a later kind is an added case.
type Anchor struct {
	Kind Kind `json:"kind"`

	// Page is an html anchor's HTML file, by its path in the version.
	Page string `json:"page,omitempty"`

	// Name is a name the page declares; Point is a point on the page when
	// it declares none there. An html anchor carries exactly one of them.
	Name  string `json:"name,omitempty" untrusted:"name"`
	Point *Point `json:"point,omitempty"`

	// Slide is a slides anchor's slide id. Path is the element's child
	// indexes from the slide's section ("0/2/1"), empty for the whole slide,
	// with the element's Tag and the first [MaxTextRunes] of its text.
	Slide string `json:"slide,omitempty"`
	Path  string `json:"path,omitempty"`
	Tag   string `json:"tag,omitempty"`
	Text  string `json:"text,omitempty" untrusted:"name"`

	// Board is a design anchor's artboard file, and X and Y a point in the
	// board's own pixels, its left and top edges included.
	Board string `json:"board,omitempty"`
	X     int    `json:"x,omitempty"`
	Y     int    `json:"y,omitempty"`
}

// Whole is the anchor of a comment on the whole version, what a caller stores
// when a comment names no place.
func Whole() Anchor { return Anchor{Kind: KindVersion} }

// NamePattern is the grammar of a name a page declares, shared with the page
// runtime that sets it.
const NamePattern = `^[a-z0-9][a-z0-9._-]{0,63}$`

// MaxTextRunes is how much of a slide element's text an anchor keeps: enough to
// find the element again, short enough to label the thread.
const MaxTextRunes = 200

// MaxPathSteps bounds a slide element's path: deeper than any slide's nesting.
const MaxPathSteps = 64

// ErrInvalid is every refusal [Check] answers.
var ErrInvalid = errors.New("commentanchor: invalid anchor")

var (
	nameRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(NamePattern) })
	pathRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]{1,5}(/[0-9]{1,5})*$`) })
)

// ValidName reports whether name is one a page may declare.
func ValidName(name string) bool { return nameRE().MatchString(name) }

// accepts is the table: for EVERY artifact kind, the anchor kinds its comments
// may use. No default row, so a new artifact kind fails the test until it is here.
var accepts = map[artifacts.Kind][]Kind{
	artifacts.KindHTML:      {KindVersion, KindHTML},
	artifacts.KindDoc:       {KindVersion},
	artifacts.KindSlides:    {KindVersion, KindSlides},
	artifacts.KindDashboard: {KindVersion},
	artifacts.KindSheet:     {KindVersion},
	artifacts.KindDesign:    {KindVersion, KindDesign},
	artifacts.KindBrand:     {KindVersion},
}

// Accepts is the anchor kinds an artifact kind's comments may use, and false
// for a kind the table does not name.
func Accepts(kind artifacts.Kind) ([]Kind, bool) {
	got, ok := accepts[kind]
	return slices.Clone(got), ok
}

// refuse is one refusal, wrapping [ErrInvalid].
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// Check refuses an anchor an artifact of kind may not carry: an unknown kind,
// one the artifact does not take, a field of another kind, a malformed field.
func Check(kind artifacts.Kind, a Anchor) error {
	allowed, ok := accepts[kind]
	if !ok {
		return refuse("artifact kind %q takes no comments in this build", kind)
	}
	switch {
	case a.Kind == "":
		return refuse("the anchor names no kind: one of %s, or %q for the whole version",
			words.Or(Kinds), KindVersion)
	case !a.Kind.Valid():
		return refuse("anchor kind %q is not one of %s", a.Kind, words.Or(Kinds))
	case !slices.Contains(allowed, a.Kind):
		return wrongKind(kind, allowed, a.Kind)
	}
	if field := foreign(a); field != "" {
		return refuse("a %s anchor carries no %q", a.Kind, field)
	}
	switch a.Kind {
	case KindHTML:
		return checkHTML(a)
	case KindSlides:
		return checkSlides(a)
	case KindDesign:
		return checkDesign(a)
	}
	return nil
}

// wrongKind is the refusal of an anchor kind the artifact does not take,
// naming whole-version comments, which every kind takes.
func wrongKind(kind artifacts.Kind, allowed []Kind, got Kind) error {
	others := slices.DeleteFunc(slices.Clone(allowed), func(k Kind) bool { return k == KindVersion })
	if len(others) == 0 {
		return refuse("%s artifacts take whole-version comments only: leave out the %q anchor",
			kind, got)
	}
	return refuse("%s artifacts take whole-version comments or %s anchors, not a %q anchor",
		kind, words.Or(others), got)
}

// fields are an anchor's fields by their wire names, and the kinds that use each.
var fields = []struct {
	name string
	set  func(Anchor) bool
	kind Kind
}{
	{"page", func(a Anchor) bool { return a.Page != "" }, KindHTML},
	{"name", func(a Anchor) bool { return a.Name != "" }, KindHTML},
	{"point", func(a Anchor) bool { return a.Point != nil }, KindHTML},
	{"slide", func(a Anchor) bool { return a.Slide != "" }, KindSlides},
	{"path", func(a Anchor) bool { return a.Path != "" }, KindSlides},
	{"tag", func(a Anchor) bool { return a.Tag != "" }, KindSlides},
	{"text", func(a Anchor) bool { return a.Text != "" }, KindSlides},
	{"board", func(a Anchor) bool { return a.Board != "" }, KindDesign},
	{"x", func(a Anchor) bool { return a.X != 0 }, KindDesign},
	{"y", func(a Anchor) bool { return a.Y != 0 }, KindDesign},
}

// foreign is the first field a carries that its kind does not use, empty for none.
func foreign(a Anchor) string {
	for _, f := range fields {
		if f.kind != a.Kind && f.set(a) {
			return f.name
		}
	}
	return ""
}

// checkHTML holds an html anchor to its page and exactly one of a name and a point.
func checkHTML(a Anchor) error {
	if err := checkPage("page", a.Page); err != nil {
		return err
	}
	switch {
	case a.Name == "" && a.Point == nil:
		return refuse("an html anchor names a declared name or a point on the page")
	case a.Name != "" && a.Point != nil:
		return refuse("an html anchor names a declared name or a point, not both")
	case a.Name != "" && !ValidName(a.Name):
		return refuse("name %q is not a declared name: lowercase letters, digits, '.', '_' "+
			"and '-', starting with a letter or a digit, at most 64", a.Name)
	case a.Point != nil && !(fraction(a.Point.X) && fraction(a.Point.Y)):
		return refuse("the point (%v, %v) is outside the page: each is a fraction from 0 to 1",
			a.Point.X, a.Point.Y)
	}
	return nil
}

// fraction reports f in [0, 1], which a NaN is not.
func fraction(f float64) bool { return f >= 0 && f <= 1 }

// checkPage holds field to an HTML file's path as a version stores it.
func checkPage(field, p string) error {
	if p == "" {
		return refuse("the anchor names no %s", field)
	}
	clean, err := manifest.CleanPath(p)
	if err != nil {
		return refuse("the %s: %w", field, err)
	}
	if clean != p {
		return refuse("the %s %q is not a path as the version stores it (%q)", field, p, clean)
	}
	if !isPage(p) {
		return refuse("the %s %q is not an HTML file (.html or .htm)", field, p)
	}
	return nil
}

// isPage reports an HTML file's path.
func isPage(p string) bool {
	lower := strings.ToLower(p)
	return strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm")
}

// elementTags are every element a slide may hold, HTML's and a drawing's.
var elementTags = sync.OnceValue(func() []string {
	return slices.Concat(slideformat.Elements(), slideformat.SVGElements())
})

// checkSlides holds a slides anchor to a slide id, and an element's path, tag
// and text when it names one.
func checkSlides(a Anchor) error {
	if a.Slide == "" {
		return refuse("a slides anchor names its slide")
	}
	if !slideformat.IsSlidePath(slideformat.SlidePath(a.Slide)) {
		return refuse("%q is not a slide id: letters, digits, '_' and '-', at most 64", a.Slide)
	}
	if a.Path == "" {
		if a.Tag != "" || a.Text != "" {
			return refuse("a slides anchor with no path is the whole slide, and carries no tag or text")
		}
		return nil
	}
	if !pathRE().MatchString(a.Path) || strings.Count(a.Path, "/") >= MaxPathSteps {
		return refuse("path %q is not an element's child indexes from its section, such as \"0/2/1\"",
			a.Path)
	}
	if a.Tag == "" {
		return refuse("a slides anchor on an element names the element's tag")
	}
	if !slices.Contains(elementTags(), a.Tag) {
		return refuse("tag %q is not an element a slide may hold", a.Tag)
	}
	if !utf8.ValidString(a.Text) {
		return refuse("the element's text is not UTF-8")
	}
	if n := utf8.RuneCountInString(a.Text); n > MaxTextRunes {
		return refuse("the element's text is %d characters and an anchor keeps at most %d",
			n, MaxTextRunes)
	}
	return nil
}

// checkDesign holds a design anchor to an artboard's file and a point inside
// the largest board a canvas holds.
func checkDesign(a Anchor) error {
	if err := checkPage("board", a.Board); err != nil {
		return err
	}
	if a.X < 0 || a.Y < 0 || a.X >= canvasformat.MaxSide || a.Y >= canvasformat.MaxSide {
		return refuse("the point (%d, %d) is outside every board: each is a pixel from 0 to %s",
			a.X, a.Y, strconv.Itoa(canvasformat.MaxSide-1))
	}
	return nil
}
