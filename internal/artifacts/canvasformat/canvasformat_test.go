package canvasformat

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/objstore"
)

// version is a manifest holding these paths, typed as the media table serves
// them.
func version(paths ...string) artifacts.Manifest {
	var m artifacts.Manifest
	for i, p := range paths {
		typ := "text/html; charset=utf-8"
		switch {
		case strings.HasSuffix(p, ".json"):
			typ = "application/json"
		case strings.HasSuffix(p, ".css"):
			typ = "text/css; charset=utf-8"
		}
		m.Files = append(m.Files, artifacts.Entry{Path: p, ContentType: typ, Object: objstore.Object{
			Key:  objstore.KeyAt(time.Unix(1_800_000_000, 0).Add(time.Duration(i) * time.Millisecond)),
			Hash: objstore.HashOf([]byte(p)), Size: int64(len(p)),
		}})
	}
	m.Entry = Path
	return m
}

// landing is the four-board landing page the exit test publishes.
const landing = `{
  "v": 1,
  "title": "Landing page",
  "boards": {
    "boards/hero.html": {"x": 0, "y": 0, "w": 1440, "h": 900, "title": "Hero"},
    "boards/features.html": {"x": 1540, "y": 0, "w": 1440, "h": 900, "title": "Features"},
    "boards/pricing.html": {"x": 0, "y": 1000, "w": 1440, "h": 900, "title": "Pricing"},
    "boards/mobile.html": {"x": 1540, "y": 1000, "w": 390, "h": 844, "title": "Mobile"}
  },
  "order": ["boards/hero.html", "boards/features.html", "boards/pricing.html", "boards/mobile.html"],
  "notes": [
    {"id": "t1", "kind": "title", "x": 0, "y": -120, "w": 900, "h": 80, "text": "Launch landing page"},
    {"id": "s1", "kind": "sticky", "x": 3100, "y": 0, "w": 240, "h": 200, "text": "Copy from marketing", "color": "pink"}
  ],
  "cover": "boards/hero.html"
}`

func landingVersion() artifacts.Manifest {
	return version("boards/features.html", "boards/hero.html", "boards/mobile.html",
		"boards/pricing.html", "boards/site.css", "canvas.json")
}

func TestALandingPageParses(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(landing), landingVersion())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Boards) != 4 || len(c.Notes) != 2 || c.CoverBoard() != "boards/hero.html" {
		t.Fatalf("parsed %+v", c)
	}
	if got := c.Bounds(); got != (Rect{0, -120, 3340, 2020}) {
		t.Fatalf("bounds %+v", got)
	}
	for _, want := range []string{"Landing page", "Hero", "Mobile", "Copy from marketing"} {
		if !strings.Contains(c.SearchText(), want) {
			t.Errorf("the search text lacks %q", want)
		}
	}
	if strings.Contains(c.SearchText(), "1440") {
		t.Error("the search text carries a coordinate")
	}
}

// A CANVAS IS REFUSED, NEVER REPAIRED, and every problem is named at its place,
// so an author fixes them all in one pass.
func TestACanvasOutsideTheFormatIsRefusedAtItsPlace(t *testing.T) {
	t.Parallel()
	board := `"a.html": {"x": 0, "y": 0, "w": 400, "h": 300}`
	wrap := func(body string) string { return `{"v": 1, ` + body + `}` }
	pages := version("a.html", "canvas.json")
	for name, tc := range map[string]struct {
		body string
		m    artifacts.Manifest
		want string
	}{
		"not JSON":          {`{"v": 1,`, pages, "canvas.json is not JSON"},
		"two documents":     {wrap(`"boards": {`+board+`}, "order": ["a.html"]`) + "{}", pages, "more after its value"},
		"an unknown field":  {wrap(`"boards": {` + board + `}, "order": ["a.html"], "pages": []`), pages, "pages is not a key the canvas format carries: the top level of canvas.json takes v, title, boards, order, notes, cover"},
		"a fractional side": {wrap(`"boards": {"a.html": {"x": 0, "y": 0, "w": 400.5, "h": 300}}, "order": ["a.html"]`), pages, `boards["a.html"].w is the number 400.5, and the format reads a whole number there`},
		"another version":   {`{"v": 2, "boards": {` + board + `}, "order": ["a.html"]}`, pages, "this build reads 1"},
		"no boards":         {wrap(`"boards": {}, "order": []`), version("canvas.json"), "at least one board"},
		"a missing page":    {wrap(`"boards": {"b.html": {"x": 0, "y": 0, "w": 400, "h": 300}}, "order": ["b.html"]`), pages, "b.html is not an HTML page"},
		"a board not HTML":  {wrap(`"boards": {"a.html": {"x":0,"y":0,"w":400,"h":300}, "s.css": {"x": 0, "y": 0, "w": 400, "h": 300}}, "order": ["a.html", "s.css"]`), version("a.html", "canvas.json", "s.css"), "s.css is not an HTML page"},
		"an unframed page":  {wrap(`"boards": {` + board + `}, "order": ["a.html"]`), version("a.html", "b.html", "canvas.json"), "b.html is an HTML page of this version that no board frames"},
		"a path not stored": {wrap(`"boards": {"./a.html": {"x": 0, "y": 0, "w": 400, "h": 300}}, "order": ["./a.html"]`), pages, "as it is stored"},
		"too narrow":        {wrap(`"boards": {"a.html": {"x": 0, "y": 0, "w": 39, "h": 300}}, "order": ["a.html"]`), pages, `boards["a.html"].w: 39 is outside 40 to 8000`},
		"too tall":          {wrap(`"boards": {"a.html": {"x": 0, "y": 0, "w": 400, "h": 8001}}, "order": ["a.html"]`), pages, "8001 is outside"},
		"too far":           {wrap(`"boards": {"a.html": {"x": 1000001, "y": 0, "w": 400, "h": 300}}, "order": ["a.html"]`), pages, "more than 1000000 pixels"},
		"order incomplete":  {wrap(`"boards": {` + board + `}, "order": []`), pages, `board "a.html" is missing`},
		"order twice":       {wrap(`"boards": {` + board + `}, "order": ["a.html", "a.html"]`), pages, "listed twice"},
		"order unknown":     {wrap(`"boards": {` + board + `}, "order": ["a.html", "z.html"]`), pages, `"z.html" is not a board`},
		"a stray cover":     {wrap(`"boards": {` + board + `}, "order": ["a.html"], "cover": "z.html"`), pages, `cover: "z.html" is not a board`},
		"a bad note id":     {wrap(`"boards": {` + board + `}, "order": ["a.html"], "notes": [{"id": "Note 1", "kind": "sticky", "x": 0, "y": 0, "w": 100, "h": 100, "text": "hi"}]`), pages, "notes[0].id"},
		"a duplicate id":    {wrap(`"boards": {` + board + `}, "order": ["a.html"], "notes": [{"id": "n", "kind": "sticky", "x": 0, "y": 0, "w": 100, "h": 100, "text": "a"}, {"id": "n", "kind": "sticky", "x": 0, "y": 0, "w": 100, "h": 100, "text": "b"}]`), pages, "names another note"},
		"an unknown kind":   {wrap(`"boards": {` + board + `}, "order": ["a.html"], "notes": [{"id": "n", "kind": "arrow", "x": 0, "y": 0, "w": 100, "h": 100, "text": "a"}]`), pages, "a note is title or sticky"},
		"a coloured title":  {wrap(`"boards": {` + board + `}, "order": ["a.html"], "notes": [{"id": "n", "kind": "title", "x": 0, "y": 0, "w": 100, "h": 100, "text": "a", "color": "pink"}]`), pages, "a title has no colour"},
		"a literal colour":  {wrap(`"boards": {` + board + `}, "order": ["a.html"], "notes": [{"id": "n", "kind": "sticky", "x": 0, "y": 0, "w": 100, "h": 100, "text": "a", "color": "#ff0000"}]`), pages, "a sticky is yellow, pink"},
		"an empty note":     {wrap(`"boards": {` + board + `}, "order": ["a.html"], "notes": [{"id": "n", "kind": "sticky", "x": 0, "y": 0, "w": 100, "h": 100, "text": "  "}]`), pages, "a note says something"},
		"a control char":    {wrap(`"boards": {` + board + `}, "order": ["a.html"], "title": "a\u0007b"`), pages, "control character"},
		"a long note":       {wrap(`"boards": {` + board + `}, "order": ["a.html"], "notes": [{"id": "n", "kind": "sticky", "x": 0, "y": 0, "w": 100, "h": 100, "text": "` + strings.Repeat("x", MaxNoteBytes+1) + `"}]`), pages, "the most is 2000"},
		"not UTF-8":         {"{\"v\": 1, \"title\": \"\xff\"}", pages, "not UTF-8"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.body), tc.m)
			var refused *Error
			if !errors.As(err, &refused) || !errors.Is(err, ErrInvalid) {
				t.Fatalf("answered %v, want a refusal", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refused with %q, which does not say %q", err, tc.want)
			}
		})
	}
}

// EVERY PROBLEM AT ONCE: a canvas with three things wrong names all three.
func TestEveryProblemIsNamedInOnePass(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"v": 1, "boards": {"a.html": {"x": 0, "y": 0, "w": 1, "h": 1}}, "order": []}`),
		version("a.html", "canvas.json"))
	var refused *Error
	if !errors.As(err, &refused) || len(refused.Problems) != 3 {
		t.Fatalf("answered %v, want three problems", err)
	}
}

// THE BOUNDS ARE THE FORMAT'S: a canvas at every bound parses, one past each is
// refused.
func TestTheBoundsAreInclusive(t *testing.T) {
	t.Parallel()
	var boards, order []string
	var paths []string
	for i := range MaxBoards {
		p := fmt.Sprintf("b%02d.html", i)
		paths = append(paths, p)
		boards = append(boards, fmt.Sprintf(`%q: {"x": %d, "y": %d, "w": %d, "h": %d}`, p,
			-MaxCoordinate, MaxCoordinate, MinSide, MaxSide))
		order = append(order, fmt.Sprintf("%q", p))
	}
	var notes []string
	for i := range MaxNotes {
		notes = append(notes, fmt.Sprintf(`{"id": "n%d", "kind": "sticky", "x": 0, "y": 0, "w": 40, "h": 40, "text": "%s"}`,
			i, strings.Repeat("x", MaxNoteBytes)))
	}
	body := `{"v": 1, "boards": {` + strings.Join(boards, ",") + `}, "order": [` +
		strings.Join(order, ",") + `], "notes": [` + strings.Join(notes, ",") + `]}`
	if _, err := Parse([]byte(body), version(append(paths, "canvas.json")...)); err != nil {
		t.Fatalf("a canvas at every bound is refused: %v", err)
	}
	over := strings.Replace(body, `"notes": [`, `"notes": [{"id": "extra", "kind": "title", "x": 0, "y": 0, "w": 40, "h": 40, "text": "x"},`, 1)
	if _, err := Parse([]byte(over), version(append(paths, "canvas.json")...)); err == nil ||
		!strings.Contains(err.Error(), "the most is 200") {
		t.Fatalf("a canvas past the note bound answered %v", err)
	}
}

// A COMMENT ANCHOR IS A POINT ON ONE BOARD, in the board's own coordinates: it
// names a board of the canvas and lies inside it, edges at the far side
// excluded, so a comment can never point at empty canvas or past a board.
func TestACommentAnchorIsAPointOnABoard(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(landing), landingVersion())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []Anchor{
		{Board: "boards/hero.html", X: 0, Y: 0},
		{Board: "boards/hero.html", X: 1439, Y: 899},
		{Board: "boards/mobile.html", X: 200, Y: 400},
	} {
		if err := c.CheckAnchor(a); err != nil {
			t.Errorf("%+v is refused: %v", a, err)
		}
	}
	for _, a := range []Anchor{
		{Board: "boards/hero.html", X: 1440, Y: 0},
		{Board: "boards/hero.html", X: 0, Y: 900},
		{Board: "boards/hero.html", X: -1, Y: 10},
		{Board: "boards/mobile.html", X: 391, Y: 10},
		{Board: "boards/site.css", X: 1, Y: 1},
		{Board: "", X: 1, Y: 1},
	} {
		if err := c.CheckAnchor(a); !errors.Is(err, ErrNoAnchor) {
			t.Errorf("%+v answered %v, want ErrNoAnchor", a, err)
		}
	}
}

// THE OUTLINE IS EVERY BOARD IN THE CANVAS'S ORDER, each with its frame, the
// cover, and the rectangle every board and note fits in.
func TestTheOutlineIsTheBoardsInOrder(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(landing), landingVersion())
	if err != nil {
		t.Fatal(err)
	}
	o := c.Outline()
	if o.Count != 4 || o.Cover != "boards/hero.html" || o.Notes != 2 || len(o.Boards) != 4 {
		t.Fatalf("the outline is %+v", o)
	}
	if b := o.Boards[3]; b.Path != "boards/mobile.html" || b.Title != "Mobile" ||
		b.Rect != (Rect{X: 1540, Y: 1000, W: 390, H: 844}) {
		t.Errorf("the last board is %+v", b)
	}
	if o.Bounds != (Rect{X: 0, Y: -120, W: 3340, H: 2020}) {
		t.Errorf("the bounds are %+v", o.Bounds)
	}
}
