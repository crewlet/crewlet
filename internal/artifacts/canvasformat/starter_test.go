package canvasformat

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// launch is a layout as the dashboard's create page sends it: a desktop and a
// phone board of one name, and a sticky beside them.
func launch() Layout {
	return Layout{
		Boards: []LayoutBoard{
			{Title: "Home", X: 0, Y: 0, W: 1440, H: 1024, Brief: "Hero with a sign-up form"},
			{Title: "Home", X: 1560, Y: 0, W: 390, H: 844},
			{Title: "<Pricing & plans>", X: 2070, Y: 0, W: 1440, H: 1024, Brief: "<script>alert(1)</script>"},
		},
		Notes: []Note{{ID: "s1", Kind: NoteSticky, X: 0, Y: 1124, W: 240, H: 160, Text: "Brand: Nightshade", Color: ColorPink}},
	}
}

// THE STARTER IS A CANVAS OF THE FORMAT: its canvas parses against its own
// files, the boards keep the layout's order and names, the first is the
// cover, and two boards of one name are filed apart.
func TestTheStarterIsACanvas(t *testing.T) {
	t.Parallel()
	files, err := Starter("  Launch site ", launch())
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	c, err := Parse(files[Path], version(paths...))
	if err != nil {
		t.Fatalf("the starter canvas does not parse: %v", err)
	}
	want := []string{"boards/home.html", "boards/home-2.html", "boards/pricing-plans.html"}
	if !slices.Equal(c.Order, want) || c.Cover != want[0] || c.Title != "Launch site" {
		t.Errorf("order %v, cover %q, title %q", c.Order, c.Cover, c.Title)
	}
	if b := c.Boards["boards/home-2.html"]; b.Title != "Home" || b.W != 390 || b.X != 1560 {
		t.Errorf("the phone board is %+v", b)
	}
	if len(c.Notes) != 1 || c.Notes[0].Text != "Brand: Nightshade" {
		t.Errorf("the notes are %+v", c.Notes)
	}
}

// WHAT THE PERSON TYPED IS TEXT ON A PLACEHOLDER: a name or a brief carrying
// markup is written escaped, and the board says its size and that it waits.
func TestAPlaceholderEscapesWhatWasTyped(t *testing.T) {
	t.Parallel()
	files, err := Starter("Launch", launch())
	if err != nil {
		t.Fatal(err)
	}
	page := string(files["boards/pricing-plans.html"])
	if strings.Contains(page, "<script>") || strings.Contains(page, "<Pricing") {
		t.Errorf("the placeholder carries the typed markup:\n%s", page)
	}
	for _, want := range []string{"&lt;Pricing &amp; plans&gt;", "&lt;script&gt;alert(1)&lt;/script&gt;",
		"1440 by 1024", "Waiting for its first draft."} {
		if !strings.Contains(page, want) {
			t.Errorf("the placeholder misses %q", want)
		}
	}
	if strings.Contains(string(files["boards/home-2.html"]), `class="brief"`) {
		t.Error("a board with no brief draws an empty brief")
	}
}

// A LAYOUT OUTSIDE THE FORMAT IS REFUSED AT ITS PLACES IN THE LAYOUT, every
// problem at once, so the page that sent it can point at the board.
func TestALayoutOutsideTheFormatIsRefused(t *testing.T) {
	t.Parallel()
	l := launch()
	l.Boards[0].Title = "  "
	l.Boards[1].W = 20
	l.Boards[2].Brief = strings.Repeat("a", MaxBriefBytes+1)
	l.Notes[0].Color = "teal"
	_, err := Starter("Launch", l)
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("a broken layout answered %v", err)
	}
	var at []string
	for _, p := range e.Problems {
		at = append(at, p.At)
	}
	for _, want := range []string{"boards[0].title", "boards[1].w", "boards[2].brief"} {
		if !slices.Contains(at, want) {
			t.Errorf("no problem at %s: %v", want, at)
		}
	}
	// THE NOTES ARE HELD ONCE THE BOARDS ARE: a layout whose boards are
	// fine answers its notes at their own index.
	l = launch()
	l.Notes[0].Color = "teal"
	if _, err := Starter("Launch", l); err == nil || !strings.Contains(err.Error(), "notes[0].color") {
		t.Errorf("a teal sticky answered %v", err)
	}
	for name, l := range map[string]Layout{
		"no boards":       {},
		"too many boards": {Boards: make([]LayoutBoard, MaxBoards+1)},
	} {
		if _, err := Starter("Launch", l); err == nil || !strings.Contains(err.Error(), "boards:") {
			t.Errorf("%s answered %v", name, err)
		}
	}
}

// A NAME WITH NO LETTER TO FILE IT UNDER is a board all the same.
func TestABoardPathIsAlwaysAPage(t *testing.T) {
	t.Parallel()
	taken := map[string]bool{}
	for _, c := range []struct{ name, want string }{
		{"Écran d'accueil", "boards/ecran-d-accueil.html"},
		{"   ", "boards/board.html"},
		{"日本語", "boards/board-2.html"},
		{strings.Repeat("ab", 40), "boards/" + strings.Repeat("ab", 20) + ".html"},
	} {
		if got := boardPath(c.name, taken); got != c.want {
			t.Errorf("%q is filed as %q, want %q", c.name, got, c.want)
		}
	}
}
