// Package canvasformat is a design's `canvas.json`: where each artboard of the
// version sits on the canvas, the order they are drawn in, and the notes
// (titles and stickies) beside them. The dashboard draws the canvas and the
// notes natively; every artboard is an HTML page of the same version, framed
// on the artifact origin under a ticket of its own (adr/0035).
//
// # A closed format, refused rather than repaired
//
// The canvas carries no author code, so it is safe to draw on the dashboard's
// origin only because nothing in it is anything but numbers, paths of the
// version and plain text. [Parse] decodes it strictly (a field this build does
// not know is refused, not dropped), holds every number to its bound and every
// path to the version, and answers every problem at once, each naming its
// place in the file, so an author fixes them all in one pass.
//
// # Every page is a board, and every board a page
//
// A board names an HTML file of the version, and every HTML file of the
// version is a board. The second half is what makes "the version's HTML pages"
// and "the documents a design's tickets may open" one set: a page no board
// names would be linted, stored and never shown, and the artifact origin would
// have no frame to serve it in.
//
// # What it is not
//
// Not Claude's Design Component format (D59): an artboard is a plain page held
// to the html page contract, linted as an `html` entry is, with no runtime of
// its own. The canvas has no pages, no groups and no live board layer yet: the
// board layer of shapes, connectors and stickies a room edits live is the
// datastore's (D60), and arrives as a reserved collection, never as fields
// here. A field added to this format later is a new `v`.
package canvasformat

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/jsonform"
)

// Path is where a design version keeps its canvas: the kind's entry.
const Path = "canvas.json"

// Version is the one canvas format this build reads and writes.
const Version = 1

// The bounds a canvas is held to, each with its reason.
const (
	// MaxBoards is the most artboards one canvas holds. A landing page is
	// four to twelve; sixty-four is a whole product's screens, and past it
	// a canvas is a site that wants an `html` artifact of its own.
	MaxBoards = 64

	// MaxNotes is the most notes one canvas holds: titles over groups of
	// boards and stickies beside them, which is annotation, not a board
	// layer (the live board layer is the datastore's).
	MaxNotes = 200

	// MinSide and MaxSide bound an artboard's or a note's width and height,
	// in CSS pixels. Forty is the smallest control a person can still point
	// at; eight thousand is past a 5K display at two to one and every print
	// format up to A0 at 96 dots per inch.
	MinSide = 40
	MaxSide = 8000

	// MaxCoordinate bounds where anything sits, in CSS pixels either side
	// of the origin: sixty-four of the largest boards side by side, with
	// room, and small enough that every sum the viewer makes stays exact.
	MaxCoordinate = 1_000_000

	// MaxNoteBytes bounds a note's text: a paragraph, not a document.
	MaxNoteBytes = 2000

	// MaxNoteIDBytes bounds a note's id.
	MaxNoteIDBytes = 32
)

// NoteKind is what a note is drawn as.
type NoteKind string

const (
	// NoteTitle is a heading laid on the canvas, over a group of boards.
	NoteTitle NoteKind = "title"

	// NoteSticky is a coloured note beside the boards.
	NoteSticky NoteKind = "sticky"
)

// NoteKinds are every note kind this build draws.
var NoteKinds = []NoteKind{NoteTitle, NoteSticky}

// Valid reports whether k is a note kind this build draws.
func (k NoteKind) Valid() bool { return slices.Contains(NoteKinds, k) }

// NoteColor is a sticky's colour, a name the dashboard maps to its own
// palette, so a sticky is themed and accessible in every scheme rather than
// painted in the author's literal.
type NoteColor string

// The sticky colours.
const (
	ColorYellow NoteColor = "yellow"
	ColorPink   NoteColor = "pink"
	ColorBlue   NoteColor = "blue"
	ColorGreen  NoteColor = "green"
	ColorPurple NoteColor = "purple"
)

// NoteColors are every sticky colour, the first being a sticky's default.
var NoteColors = []NoteColor{ColorYellow, ColorPink, ColorBlue, ColorGreen, ColorPurple}

// Valid reports whether c is a sticky colour this build draws.
func (c NoteColor) Valid() bool { return slices.Contains(NoteColors, c) }

// Canvas is one design version's `canvas.json`.
type Canvas struct {
	// V is the format version, [Version].
	V int `json:"v"`

	// Title is the canvas's own heading, optional.
	Title string `json:"title,omitempty"`

	// Boards are the artboards, keyed by the path of the HTML page each
	// one frames.
	Boards map[string]Board `json:"boards"`

	// Order is every board's path once, back to front: the order boards
	// are drawn, listed and printed in.
	Order []string `json:"order"`

	// Notes are the titles and stickies on the canvas, back to front, all
	// drawn over the boards.
	Notes []Note `json:"notes,omitempty"`

	// Cover is the board the gallery's cover titles, optional: the first
	// board in Order when unset.
	Cover string `json:"cover,omitempty"`
}

// Board is one artboard: a frame of the canvas, in canvas pixels.
type Board struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`

	// Title is what the frame's label says, optional: the page's file name
	// when unset.
	Title string `json:"title,omitempty"`
}

// Note is a title or a sticky on the canvas.
type Note struct {
	// ID names the note across versions, so an edit to one note is an edit
	// to that note and not a delete and an add.
	ID   string   `json:"id"`
	Kind NoteKind `json:"kind"`
	X    int      `json:"x"`
	Y    int      `json:"y"`
	W    int      `json:"w"`
	H    int      `json:"h"`
	Text string   `json:"text"`

	// Color is a sticky's colour; a title has none.
	Color NoteColor `json:"color,omitempty"`
}

// Rect is a rectangle on the canvas, in canvas pixels.
type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Rect is the board's frame.
func (b Board) Rect() Rect { return Rect{b.X, b.Y, b.W, b.H} }

// Problem is one thing wrong with a canvas, at a place in it.
type Problem struct {
	// At names where in the canvas, as a reader finds it: `boards["a.html"].w`,
	// `notes[3].text`, `order`.
	At string

	// Message says what is wrong and what to write instead.
	Message string
}

func (p Problem) String() string {
	if p.At == "" {
		return p.Message
	}
	return p.At + ": " + p.Message
}

// Error is a canvas [Parse] refused, with every problem in the order found.
type Error struct{ Problems []Problem }

func (e *Error) Error() string {
	lines := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		lines[i] = p.String()
	}
	return "canvasformat: " + Path + " is not a canvas this build draws: " + strings.Join(lines, "; ")
}

// ErrInvalid is every canvas [Parse] refuses, wrapped by [*Error].
var ErrInvalid = errors.New("canvasformat: invalid canvas")

// Unwrap makes every [*Error] an [ErrInvalid].
func (e *Error) Unwrap() error { return ErrInvalid }

// noteID is a note's id: short, lower case, readable in a diff.
var noteID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Parse reads a canvas and holds it to the format and to the version it ships
// in, m: every board an HTML page of m, and every HTML page of m a board. Every
// problem is answered at once as an [*Error].
//
// Parse is the publish's read; a stored canvas is read with [Read].
func Parse(data []byte, m artifacts.Manifest) (Canvas, error) {
	return parse(data, m, jsonform.Decode)
}

func parse(data []byte, m artifacts.Manifest, read jsonform.Reader) (Canvas, error) {
	c, problems := decode(data, read)
	if problems == nil {
		problems = check(c, m)
	}
	if len(problems) > 0 {
		return Canvas{}, &Error{Problems: problems}
	}
	return c, nil
}

// Form is every key `canvas.json` may carry and the type of each, derived from
// [Canvas] ([jsonform.Of]): what [Parse] admits and [Reference] lists.
var Form = jsonform.Of[Canvas]()

// decode reads the JSON strictly: one document, no field this build does not
// know, and the right type for every field, every structural fault at once
// ([jsonform.Decode]).
func decode(data []byte, read jsonform.Reader) (Canvas, []Problem) {
	if !utf8.Valid(data) {
		return Canvas{}, []Problem{{Message: "the file is not UTF-8 text"}}
	}
	var c Canvas
	d := read(data, Form, jsonform.Subject{File: Path, Format: "the canvas format"}, &c)
	if len(d.Faults) == 0 {
		return c, nil
	}
	out := make([]Problem, len(d.Faults))
	for i, f := range d.Faults {
		out[i] = Problem{Message: f.Message}
	}
	return Canvas{}, out
}

// check holds a decoded canvas to every rule of the format.
func check(c Canvas, m artifacts.Manifest) []Problem {
	var out []Problem
	add := func(at, format string, args ...any) {
		out = append(out, Problem{At: at, Message: fmt.Sprintf(format, args...)})
	}
	if c.V != Version {
		add("v", "the format version is %d, and this build reads %d", c.V, Version)
	}
	checkText(add, "title", c.Title, artifacts.MaxTitleBytes)

	pages := htmlPages(m)
	switch {
	case len(c.Boards) == 0:
		add("boards", "a canvas holds at least one board")
	case len(c.Boards) > MaxBoards:
		add("boards", "the canvas holds %d boards and the most is %d", len(c.Boards), MaxBoards)
	}
	for _, p := range sortedKeys(c.Boards) {
		at := fmt.Sprintf("boards[%q]", p)
		b := c.Boards[p]
		if clean, err := manifest.CleanPath(p); err != nil || clean != p {
			add(at, "a board is keyed by the path of a page of the version, as it is stored")
		} else if !slices.Contains(pages, p) {
			add(at, "%s is not an HTML page of this version: a board frames one", p)
		}
		checkRect(add, at, b.Rect())
		checkText(add, at+".title", b.Title, artifacts.MaxTitleBytes)
	}
	for _, p := range pages {
		if _, ok := c.Boards[p]; !ok {
			add("boards", "%s is an HTML page of this version that no board frames: add a "+
				"board for it, or leave it out of the version", p)
		}
	}

	seen := map[string]bool{}
	for i, p := range c.Order {
		at := fmt.Sprintf("order[%d]", i)
		switch _, ok := c.Boards[p]; {
		case !ok:
			add(at, "%q is not a board", p)
		case seen[p]:
			add(at, "%q is listed twice", p)
		}
		seen[p] = true
	}
	for _, p := range sortedKeys(c.Boards) {
		if !seen[p] {
			add("order", "board %q is missing: order lists every board once, back to front", p)
		}
	}
	if c.Cover != "" {
		if _, ok := c.Boards[c.Cover]; !ok {
			add("cover", "%q is not a board", c.Cover)
		}
	}

	if len(c.Notes) > MaxNotes {
		add("notes", "the canvas holds %d notes and the most is %d", len(c.Notes), MaxNotes)
	}
	ids := map[string]bool{}
	for i, n := range c.Notes {
		at := fmt.Sprintf("notes[%d]", i)
		switch {
		case len(n.ID) == 0 || len(n.ID) > MaxNoteIDBytes || !noteID.MatchString(n.ID):
			add(at+".id", "a note id is 1 to %d lower-case letters, digits and hyphens", MaxNoteIDBytes)
		case ids[n.ID]:
			add(at+".id", "%q names another note too", n.ID)
		}
		ids[n.ID] = true
		if !n.Kind.Valid() {
			add(at+".kind", "a note is %s", strings.Join(noteKindNames(), " or "))
		}
		switch {
		case n.Kind == NoteTitle && n.Color != "":
			add(at+".color", "a title has no colour; only a sticky does")
		case n.Kind == NoteSticky && n.Color != "" && !n.Color.Valid():
			add(at+".color", "a sticky is %s", strings.Join(colorNames(), ", "))
		}
		checkRect(add, at, Rect{n.X, n.Y, n.W, n.H})
		if strings.TrimSpace(n.Text) == "" {
			add(at+".text", "a note says something")
		}
		checkText(add, at+".text", n.Text, MaxNoteBytes)
	}
	return out
}

// checkRect holds a frame to the canvas's bounds.
func checkRect(add func(string, string, ...any), at string, r Rect) {
	for _, side := range []struct {
		name string
		v    int
	}{{"w", r.W}, {"h", r.H}} {
		if side.v < MinSide || side.v > MaxSide {
			add(at+"."+side.name, "%d is outside %d to %d pixels", side.v, MinSide, MaxSide)
		}
	}
	for _, pos := range []struct {
		name string
		v    int
	}{{"x", r.X}, {"y", r.Y}} {
		if pos.v < -MaxCoordinate || pos.v > MaxCoordinate {
			add(at+"."+pos.name, "%d is more than %d pixels from the origin", pos.v, MaxCoordinate)
		}
	}
}

// checkText holds free text to its bound, with no control character but a
// line break or a tab.
func checkText(add func(string, string, ...any), at, s string, max int) {
	if len(s) > max {
		add(at, "%d bytes, and the most is %d", len(s), max)
	}
	if strings.ContainsFunc(s, func(r rune) bool {
		return r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f)
	}) {
		add(at, "carries a control character")
	}
}

// htmlPages is every HTML page of the version, in path order.
func htmlPages(m artifacts.Manifest) []string {
	var out []string
	for _, e := range m.Files {
		typ, err := mediatype.Resolve(e.Path, e.ContentType)
		if err == nil && typ.Essence() == "text/html" {
			out = append(out, e.Path)
		}
	}
	return out
}

func sortedKeys(m map[string]Board) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func noteKindNames() []string {
	out := make([]string, len(NoteKinds))
	for i, k := range NoteKinds {
		out[i] = string(k)
	}
	return out
}

func colorNames() []string {
	out := make([]string, len(NoteColors))
	for i, c := range NoteColors {
		out[i] = string(c)
	}
	return out
}

// CoverBoard is the board the gallery's cover titles: Cover, else the first
// board in Order.
func (c Canvas) CoverBoard() string {
	if c.Cover != "" {
		return c.Cover
	}
	if len(c.Order) > 0 {
		return c.Order[0]
	}
	return ""
}

// Bounds is the smallest rectangle holding every board and note, the zero
// rectangle for an empty canvas.
func (c Canvas) Bounds() Rect {
	var rects []Rect
	for _, p := range c.Order {
		rects = append(rects, c.Boards[p].Rect())
	}
	for _, n := range c.Notes {
		rects = append(rects, Rect{n.X, n.Y, n.W, n.H})
	}
	if len(rects) == 0 {
		return Rect{}
	}
	minX, minY := rects[0].X, rects[0].Y
	maxX, maxY := rects[0].X+rects[0].W, rects[0].Y+rects[0].H
	for _, r := range rects[1:] {
		minX, minY = min(minX, r.X), min(minY, r.Y)
		maxX, maxY = max(maxX, r.X+r.W), max(maxY, r.Y+r.H)
	}
	return Rect{minX, minY, maxX - minX, maxY - minY}
}

// SearchText is what the canvas itself says, for the gallery's index: its
// title, each board's title in order, and each note's text.
func (c Canvas) SearchText() string {
	var parts []string
	if c.Title != "" {
		parts = append(parts, c.Title)
	}
	for _, p := range c.Order {
		if t := c.Boards[p].Title; t != "" {
			parts = append(parts, t)
		}
	}
	for _, n := range c.Notes {
		parts = append(parts, n.Text)
	}
	return strings.Join(parts, "\n")
}

// Anchor is where a comment on a design points (D65): one artboard, by its
// path, and a point in that artboard's own coordinates, so a comment stays on
// the spot it was made on however the board is moved on the canvas. A point
// is inside the board, its left and top edges included.
//
// THE SHAPE IS FIXED NOW, before comments land (Phase 3), because an anchor
// added after the first tag would be a change to the comment record.
type Anchor struct {
	Board string `json:"board"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
}

// ErrNoAnchor reports an anchor that is not a point of any board of the
// canvas.
var ErrNoAnchor = errors.New("canvasformat: the anchor is not a point on a board")

// CheckAnchor holds a to the canvas: it names a board, and its point lies
// inside that board as the canvas sizes it.
func (c Canvas) CheckAnchor(a Anchor) error {
	b, ok := c.Boards[a.Board]
	switch {
	case !ok:
		return fmt.Errorf("%w: %q is not a board", ErrNoAnchor, a.Board)
	case a.X < 0 || a.Y < 0 || a.X >= b.W || a.Y >= b.H:
		return fmt.Errorf("%w: (%d, %d) is outside %q, which is %d by %d", ErrNoAnchor,
			a.X, a.Y, a.Board, b.W, b.H)
	}
	return nil
}

// Source is a stored version's files as the canvas reads them.
type Source interface {
	// Read answers the bytes of the version's file at path.
	Read(path string) ([]byte, error)
}

// Read reads a stored version's canvas from src and holds it to the version,
// m, as [Parse] held it at publish, its JSON read as stored
// ([jsonform.Stored]) so a canvas accepted before ADR-0059 tightened its
// keys reads as it did.
func Read(src Source, m artifacts.Manifest) (Canvas, error) {
	return readWith(src, m, jsonform.Stored(jsonform.NoMoreValues))
}

// Check is [Read] as a publish holds a canvas it writes ([Parse]).
func Check(src Source, m artifacts.Manifest) (Canvas, error) {
	return readWith(src, m, jsonform.Decode)
}

func readWith(src Source, m artifacts.Manifest, read jsonform.Reader) (Canvas, error) {
	data, err := src.Read(Path)
	if err != nil {
		return Canvas{}, err
	}
	return parse(data, m, read)
}

// Outline is a design's structure without its pages: what the gallery draws
// its cover from (a schematic of every board, the cover titled) and what an
// agent reads before it opens the boards it changes.
type Outline struct {
	Title string `json:"title,omitempty"`

	// Count is how many boards the canvas holds.
	Count int `json:"count"`

	// Cover is the board the cover titles: [Canvas.CoverBoard].
	Cover string `json:"cover"`

	// Bounds is the rectangle holding every board and note.
	Bounds Rect `json:"bounds"`

	// Boards are every board in the canvas's order.
	Boards []OutlineBoard `json:"boards"`

	// Notes is how many notes the canvas holds.
	Notes int `json:"notes"`
}

// OutlineBoard is one board of an outline: its page, its label and its frame.
type OutlineBoard struct {
	Path  string `json:"path"`
	Title string `json:"title,omitempty"`
	Rect
}

// Outline is the canvas's outline.
func (c Canvas) Outline() Outline {
	out := Outline{Title: c.Title, Count: len(c.Order), Cover: c.CoverBoard(), Bounds: c.Bounds(),
		Boards: make([]OutlineBoard, len(c.Order)), Notes: len(c.Notes)}
	for i, p := range c.Order {
		b := c.Boards[p]
		out.Boards[i] = OutlineBoard{Path: p, Title: b.Title, Rect: b.Rect()}
	}
	return out
}
