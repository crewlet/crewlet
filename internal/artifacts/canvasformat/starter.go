package canvasformat

import (
	"encoding/json"
	"fmt"
	"html"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// ---- the starter canvas --------------------------------------------------- //
//
// What a person starts a design from in the dashboard: the boards they laid
// out, each a placeholder page of its size saying what the board is for, and
// the notes they placed beside them. An agent then publishes each board's
// real page over its placeholder. It is the ENGINE'S, not the browser's,
// because a create from the dashboard carries no upload: the pipeline
// publishes these files inline and holds them to this format and to the page
// linter like any version.

// StarterDir is the folder a starter's placeholder pages are written to.
const StarterDir = "boards/"

// MaxBriefBytes bounds what one board is for: a paragraph, as a note's text.
const MaxBriefBytes = MaxNoteBytes

// maxSlugBytes bounds the name a board's page is filed under, before the
// suffix that tells two boards of one name apart.
const maxSlugBytes = 40

// Layout is the canvas a person laid out before any board was drawn.
type Layout struct {
	// Boards are the artboards in the order they are drawn, listed and
	// printed: the first is the cover.
	Boards []LayoutBoard `json:"boards"`

	// Notes are the titles and stickies placed beside the boards.
	Notes []Note `json:"notes,omitempty"`
}

// LayoutBoard is one artboard of a [Layout]: its name, its frame and what it
// is for.
type LayoutBoard struct {
	Title string `json:"title"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`

	// Brief is what the board is for, in the person's words, optional.
	Brief string `json:"brief,omitempty"`
}

// Starter is the starter canvas's files for a design titled title, laid out
// as l, by path: [Path] and one placeholder page per board under
// [StarterDir]. A layout outside the format is answered as an [*Error] naming
// every problem at its place in the layout (`boards[2].w`), before anything
// is written.
func Starter(title string, l Layout) (map[string][]byte, error) {
	if problems := checkLayout(l); len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	c := Canvas{V: Version, Title: strings.TrimSpace(title), Boards: make(map[string]Board, len(l.Boards)),
		Order: make([]string, len(l.Boards)), Notes: l.Notes}
	files := make(map[string][]byte, len(l.Boards)+1)
	taken := map[string]bool{}
	var m artifacts.Manifest
	for i, b := range l.Boards {
		name := strings.TrimSpace(b.Title)
		p := boardPath(name, taken)
		c.Boards[p] = Board{X: b.X, Y: b.Y, W: b.W, H: b.H, Title: name}
		c.Order[i] = p
		files[p] = placeholder(name, b)
		m.Files = append(m.Files, artifacts.Entry{Path: p, ContentType: "text/html; charset=utf-8"})
	}
	c.Cover = c.Order[0]
	// THE NOTES ARE HELD HERE as a publish holds them, so a note out of the
	// format is the layout's problem at its own index, not the pipeline's.
	if problems := check(c, m); len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	canvas, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		// A CANVAS OF STRINGS AND NUMBERS ALWAYS ENCODES; a failure here is
		// a broken build, not an input.
		panic(fmt.Sprintf("canvasformat: the starter canvas does not encode: %v", err))
	}
	files[Path] = canvas
	return files, nil
}

// checkLayout holds a layout's boards to the format, at their places in it.
func checkLayout(l Layout) []Problem {
	var out []Problem
	add := func(at, format string, args ...any) {
		out = append(out, Problem{At: at, Message: fmt.Sprintf(format, args...)})
	}
	switch {
	case len(l.Boards) == 0:
		add("boards", "a design starts with at least one board")
	case len(l.Boards) > MaxBoards:
		add("boards", "the layout holds %d boards and the most is %d", len(l.Boards), MaxBoards)
	}
	for i, b := range l.Boards {
		at := fmt.Sprintf("boards[%d]", i)
		if strings.TrimSpace(b.Title) == "" {
			add(at+".title", "a board has a name")
		}
		checkText(add, at+".title", b.Title, artifacts.MaxTitleBytes)
		checkRect(add, at, Rect{b.X, b.Y, b.W, b.H})
		checkText(add, at+".brief", b.Brief, MaxBriefBytes)
	}
	return out
}

// boardPath is the page a board named name is filed under: its name in lower
// case letters, digits and hyphens under [StarterDir], accents dropped (an
// "É" files as "e"), told apart from a board of the same name by a number. A
// name with none of those letters is "board".
func boardPath(name string, taken map[string]bool) string {
	var b strings.Builder
	dash := false
	for _, r := range norm.NFD.String(strings.ToLower(name)) {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
		if b.Len() >= maxSlugBytes {
			break
		}
	}
	slug := strings.TrimRight(b.String(), "-")
	if slug == "" {
		slug = "board"
	}
	p := StarterDir + slug + ".html"
	for n := 2; taken[p]; n++ {
		p = StarterDir + slug + "-" + strconv.Itoa(n) + ".html"
	}
	taken[p] = true
	return p
}

// placeholder is a board's page until its first draft: its name, its size
// and what it is for, set at the board's own scale. Everything the person
// typed is written escaped, as text.
func placeholder(name string, b LayoutBoard) []byte {
	var brief string
	if s := strings.TrimSpace(b.Brief); s != "" {
		brief = "<p class=\"brief\">" + html.EscapeString(s) + "</p>\n"
	}
	return []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + html.EscapeString(name) + `</title>
<style>
:root { color-scheme: light dark; --ground: #f7f6f2; --ink: #1d2433; --quiet: #5b6372; --rule: #d9dbe0; }
@media (prefers-color-scheme: dark) { :root { --ground: #17151f; --ink: #ecebf2; --quiet: #a3a1b3; --rule: #34313f; } }
* { box-sizing: border-box; margin: 0; }
html, body { height: 100%; }
body { display: flex; align-items: center; justify-content: center; padding: 6vw;
  background: var(--ground); color: var(--ink);
  font: 400 clamp(13px, 3vw, 28px)/1.45 system-ui, -apple-system, "Segoe UI", sans-serif; }
main { display: flex; flex-direction: column; gap: 0.75em; max-width: 36em; width: 100%;
  border: 1px dashed var(--rule); border-radius: 12px; padding: 2em; }
.size { color: var(--quiet); font-size: 0.75em; letter-spacing: 0.02em; }
h1 { font-size: clamp(20px, 6vw, 72px); font-weight: 600; line-height: 1.15; overflow-wrap: anywhere; }
.brief { white-space: pre-wrap; overflow-wrap: anywhere; }
.wait { color: var(--quiet); font-size: 0.85em; }
</style>
</head>
<body>
<main>
<p class="size">` + strconv.Itoa(b.W) + ` by ` + strconv.Itoa(b.H) + `</p>
<h1>` + html.EscapeString(name) + `</h1>
` + brief + `<p class="wait">Waiting for its first draft.</p>
</main>
</body>
</html>
`)
}
