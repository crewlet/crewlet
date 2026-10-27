package slideformat

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/jsonform"
)

// ---- the files ------------------------------------------------------------ //

// IndexPath is the deck's index, its entry.
const IndexPath = "deck.json"

// SlidesDir holds one file a slide, named by the slide's id.
const SlidesDir = "slides/"

// SlidePath is where slide id's file is.
func SlidePath(id string) string { return SlidesDir + id + ".html" }

// MaxSlides bounds one deck, as Claude's format does (500). The version's own
// file bound (511) holds a deck that large with its index and a few images.
const MaxSlides = 500

// MaxFaces bounds the typefaces a deck declares, as Claude's format does.
const MaxFaces = 4

// MaxTitleBytes bounds the deck's title and a section's description.
const MaxTitleBytes = 120

// MaxSectionBytes bounds one section's sentence of the outline.
const MaxSectionBytes = 300

// fontEssences are the font types a face's file may be.
var fontEssences = []string{"font/woff2", "font/woff", "font/ttf", "font/otf"}

// ---- the index ------------------------------------------------------------ //

// Index is `deck.json` as written: Claude's deck index, less the design
// systems a Crewlet deck does not install.
type Index struct {
	// V is the index format's version stamp Claude's runtime writes. It is
	// accepted and kept; nothing here reads it.
	V int `json:"v,omitempty"`

	// CreatedOnFiles and Lists are Claude's own bookkeeping: accepted, kept
	// as written, never read.
	CreatedOnFiles json.RawMessage `json:"createdOnFiles,omitempty"`
	Lists          string          `json:"lists,omitempty"`

	Title    string                  `json:"title"`
	Order    []string                `json:"order"`
	Sections map[string]IndexSection `json:"sections,omitempty"`
	Cover    string                  `json:"cover,omitempty"`
	Faces    map[string]IndexFace    `json:"faces,omitempty"`

	// Theme is the look the deck began in ([Themes]): Crewlet's own key, not
	// Claude's. Optional: a deck that names none is drawn just the same, since
	// every slide holds its own styles, and the editor adds a slide to it by
	// duplicating one rather than from a theme's layouts.
	Theme string `json:"theme,omitempty"`

	// DesignSystems is Claude's record of installed design systems. A
	// Crewlet deck installs none: its look is the company's artifact kit.
	DesignSystems []json.RawMessage `json:"designSystems,omitempty"`
}

// IndexSection is one run of the outline: its sentence and the slide it
// starts at.
type IndexSection struct {
	Description string `json:"description"`
	Start       string `json:"start"`
}

// IndexFace is one typeface: its family and the font file of the version
// that holds it.
type IndexFace struct {
	Family string `json:"family"`
	Src    string `json:"src,omitempty"`
	Href   string `json:"href,omitempty"`
}

// ---- the deck ------------------------------------------------------------- //

// Deck is a version's deck as the dashboard draws it: the index resolved and
// every slide read, in order.
type Deck struct {
	Title    string    `json:"title"`
	Theme    string    `json:"theme,omitempty"`
	Cover    string    `json:"cover"`
	Sections []Section `json:"sections"`
	Faces    []Face    `json:"faces"`
	Slides   []Slide   `json:"slides"`
}

// Section is one run of the outline, in deck order.
type Section struct {
	Description string `json:"description"`
	Start       string `json:"start"`
}

// Face is one typeface the deck loads: the family its slides name and the font
// file the dashboard reads it from.
type Face struct {
	Family string `json:"family"`
	Src    string `json:"src"`
}

// Outline is a deck without its slides' trees: what the gallery's cover, an
// agent reading a deck one slide at a time, and the filmstrip's labels need.
type Outline struct {
	Title    string         `json:"title"`
	Count    int            `json:"count"`
	Cover    string         `json:"cover"`
	Sections []Section      `json:"sections"`
	Slides   []OutlineSlide `json:"slides"`
}

// OutlineSlide is one slide of an outline.
type OutlineSlide struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Title  string `json:"title"`
	Hidden bool   `json:"hidden,omitempty"`
	Notes  bool   `json:"notes,omitempty"`
}

// Outline is the deck's outline.
func (d Deck) Outline() Outline {
	out := Outline{Title: d.Title, Count: len(d.Slides), Cover: d.Cover, Sections: d.Sections,
		Slides: make([]OutlineSlide, len(d.Slides))}
	for i, s := range d.Slides {
		out.Slides[i] = OutlineSlide{ID: s.ID, Path: s.Path, Title: s.Title, Hidden: s.Hidden,
			Notes: s.Notes != ""}
	}
	return out
}

// CoverSlide is the slide the gallery draws: the index's cover, else the
// first.
func (d Deck) CoverSlide() (Slide, bool) {
	for _, s := range d.Slides {
		if s.ID == d.Cover {
			return s, true
		}
	}
	return Slide{}, false
}

// Text is the deck's readable text, for the gallery's search: its title, then
// each slide's text and notes, in order.
func (d Deck) Text() string {
	var b strings.Builder
	b.WriteString(d.Title)
	for _, s := range d.Slides {
		b.WriteString("\n")
		b.WriteString(strings.Join(strings.Fields(strings.Join(s.text, " ")), " "))
		if s.Notes != "" {
			b.WriteString("\n")
			b.WriteString(s.Notes)
		}
	}
	return b.String()
}

// ---- reading -------------------------------------------------------------- //

// Source is how a deck's files are read: the version's file list, and the
// bytes of a file in it.
type Source = artifacts.FileSource

// Refused is a deck outside the format: every finding, in file order, so an
// author fixes them in one pass.
type Refused struct {
	Findings []Finding
}

func (r *Refused) Error() string {
	lines := make([]string, len(r.Findings))
	for i, f := range r.Findings {
		lines[i] = f.String()
	}
	return "the deck is outside the slide format: " + strings.Join(lines, "; ")
}

// ErrRefused is every refusal this package answers, wrapped by [*Refused].
var ErrRefused = errors.New("slideformat: refused")

// Unwrap makes every refusal an [ErrRefused].
func (r *Refused) Unwrap() error { return ErrRefused }

// MaxFindings bounds the findings one refusal carries: past it the author has
// enough to fix, and an answer naming every one of a thousand would bury the
// first.
const MaxFindings = 50

// Read reads a whole stored deck: the index ([jsonform.Stored], so a
// deck accepted before ADR-0059 tightened its keys reads as it did), every
// slide it orders, and every file the slides and faces name. A deck outside
// the format is a [*Refused]; an error reading a file is answered as itself.
func Read(src Source) (Deck, error) { return read(src, jsonform.Stored(jsonform.UntilEOF)) }

// Check is [Read] as a publish holds a deck whose index it writes: the index
// read strictly ([ParseIndex]).
func Check(src Source) (Deck, error) { return read(src, jsonform.Decode) }

func read(src Source, decode jsonform.Reader) (Deck, error) {
	var found []Finding
	add := func(f ...Finding) { found = append(found, f...) }
	raw, err := src.Read(IndexPath)
	if err != nil {
		return Deck{}, fmt.Errorf("slideformat: read %s: %w", IndexPath, err)
	}
	idx, problems := parseIndex(raw, decode)
	add(problems...)
	if idx == nil {
		return Deck{}, &Refused{Findings: found}
	}
	deck := Deck{Title: idx.Title, Theme: idx.Theme, Cover: idx.Cover}
	if deck.Cover == "" && len(idx.Order) > 0 {
		deck.Cover = idx.Order[0]
	}
	ordered := map[string]bool{}
	for _, id := range idx.Order {
		ordered[id] = true
	}
	for _, p := range src.Paths() {
		id, isSlide := slideID(p)
		switch {
		case isSlide && !ordered[id]:
			add(Finding{Path: p, Rule: RuleIndex, Message: fmt.Sprintf("slide %q is not in "+
				"deck.json's order: add it, or remove the file", id)})
		case !isSlide && strings.HasSuffix(strings.ToLower(p), ".html"):
			add(Finding{Path: p, Rule: RuleIndex, Message: "a deck holds HTML only as its slides, " +
				"each slides/<id>.html"})
		}
	}
	files := func(p string) (string, bool) { return src.Essence(p) }
	for _, id := range idx.Order {
		p := SlidePath(id)
		if _, ok := src.Essence(p); !ok {
			add(Finding{Path: IndexPath, Rule: RuleReference, Message: fmt.Sprintf("order names "+
				"slide %q and the version holds no %s", id, p)})
			continue
		}
		body, err := src.Read(p)
		if err != nil {
			return Deck{}, fmt.Errorf("slideformat: read %s: %w", p, err)
		}
		slide, slideFindings := ParseSlide(p, id, body, files)
		add(slideFindings...)
		deck.Slides = append(deck.Slides, slide)
	}
	deck.Sections = sections(idx)
	faces, problems := facesOf(idx, files)
	add(problems...)
	deck.Faces = faces
	if len(found) > 0 {
		if len(found) > MaxFindings {
			found = append(found[:MaxFindings], Finding{Path: IndexPath, Rule: RuleBound,
				Message: fmt.Sprintf("and %d more", len(found)-MaxFindings)})
		}
		return Deck{}, &Refused{Findings: found}
	}
	return deck, nil
}

// IsSlidePath reports whether p is a slide file's path.
func IsSlidePath(p string) bool {
	_, ok := slideID(p)
	return ok
}

// slideID reads a slide file's id from its path.
func slideID(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, SlidesDir)
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, ".html")
	return id, ok && idRE.MatchString(id)
}

var faceKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// IndexForm is every key `deck.json` may carry and the type of each, derived
// from [Index] ([jsonform.Of]): what [ParseIndex] admits and [Reference] lists.
var IndexForm = jsonform.Of[Index]()

// ParseIndex reads `deck.json` as a publish holds it, answering nil with the
// findings where it cannot be read at all.
func ParseIndex(raw []byte) (*Index, []Finding) { return parseIndex(raw, jsonform.Decode) }

func parseIndex(raw []byte, decode jsonform.Reader) (*Index, []Finding) {
	var found []Finding
	bad := func(format string, args ...any) {
		found = append(found, Finding{Path: IndexPath, Rule: RuleIndex, Message: fmt.Sprintf(format, args...)})
	}
	var idx Index
	// EVERY STRUCTURAL FAULT AT ONCE ([jsonform.Decode]): an unknown key
	// names the keys its object takes, a wrong type the shape read there.
	d := decode(raw, IndexForm, jsonform.Subject{File: IndexPath, Format: "the deck index"}, &idx)
	for _, f := range d.Faults {
		found = append(found, Finding{Path: IndexPath, Line: f.Line, Rule: RuleIndex, Message: f.Message})
	}
	if len(d.Faults) > 0 {
		return nil, found
	}
	switch {
	case strings.TrimSpace(idx.Title) == "":
		bad("title is empty: name the deck")
	case len(idx.Title) > MaxTitleBytes || !utf8.ValidString(idx.Title):
		bad("title is longer than %d bytes", MaxTitleBytes)
	}
	if _, ok := ThemeByID(idx.Theme); idx.Theme != "" && idx.Theme != BrandTheme && !ok {
		bad("theme is %q, and a deck's theme is one of %s", idx.Theme, strings.Join(themeIDs(), ", "))
	}
	if idx.Lists != "" && idx.Lists != "css" {
		bad("lists is %q, and the only list style is css", idx.Lists)
	}
	if len(idx.DesignSystems) > 0 {
		bad("designSystems names a design system, and a Crewlet deck installs none: its look " +
			"is the company's artifact kit")
	}
	switch {
	case len(idx.Order) == 0:
		bad("order is empty: a deck holds at least one slide")
	case len(idx.Order) > MaxSlides:
		bad("order names %d slides and a deck holds at most %d", len(idx.Order), MaxSlides)
	}
	seen := map[string]bool{}
	for _, id := range idx.Order {
		switch {
		case !idRE.MatchString(id):
			bad("order names %q, and a slide id is 1 to 64 letters, digits, _ and -", id)
		case seen[id]:
			bad("order names slide %q twice", id)
		}
		seen[id] = true
	}
	if idx.Cover != "" && !seen[idx.Cover] {
		bad("cover is %q, which is not in order", idx.Cover)
	}
	starts := map[string]string{}
	for key, s := range idx.Sections {
		switch {
		case !seen[s.Start]:
			bad("section %q starts at %q, which is not in order", key, s.Start)
		case starts[s.Start] != "":
			bad("sections %q and %q both start at %q", starts[s.Start], key, s.Start)
		case len(s.Description) > MaxSectionBytes:
			bad("section %q's description is longer than %d bytes", key, MaxSectionBytes)
		}
		starts[s.Start] = key
	}
	if len(idx.Faces) > MaxFaces {
		bad("faces names %d typefaces and a deck declares at most %d", len(idx.Faces), MaxFaces)
	}
	for key, f := range idx.Faces {
		if !faceKeyRE.MatchString(key) {
			bad("face %q: a face's key is its family lowercased, spaces as -", key)
		}
		if !familyRE.MatchString(f.Family) {
			bad("face %q's family %q: a letter, then letters, digits, spaces, _ and -, at most 40",
				key, f.Family)
		}
	}
	return &idx, found
}

// sections is the outline in deck order.
func sections(idx *Index) []Section {
	out := make([]Section, 0, len(idx.Sections))
	for _, s := range idx.Sections {
		out = append(out, Section(s))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return slices.Index(idx.Order, out[i].Start) < slices.Index(idx.Order, out[j].Start)
	})
	return out
}

// facesOf resolves each face to the font file of the version it loads from.
func facesOf(idx *Index, files Files) ([]Face, []Finding) {
	var found []Finding
	bad := func(format string, args ...any) {
		found = append(found, Finding{Path: IndexPath, Rule: RuleReference, Message: fmt.Sprintf(format, args...)})
	}
	keys := make([]string, 0, len(idx.Faces))
	for k := range idx.Faces {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]Face, 0, len(keys))
	for _, k := range keys {
		f := idx.Faces[k]
		switch {
		case f.Href != "":
			bad("face %q loads from %s: a deck's typeface is a font file of the version (woff2, "+
				"woff, ttf or otf), named in src, so the deck draws the same wherever it is opened",
				k, f.Href)
			continue
		case f.Src == "":
			bad("face %q names no src: the font file of the version it loads from", k)
			continue
		}
		essence, ok := files(f.Src)
		switch {
		case !ok:
			bad("face %q's src is %s, and the version holds no file there", k, f.Src)
		case !slices.Contains(fontEssences, essence):
			bad("face %q's src %s is %s, and a face is woff2, woff, ttf or otf", k, f.Src, essence)
		default:
			out = append(out, Face{Family: f.Family, Src: f.Src})
		}
	}
	return out, found
}
