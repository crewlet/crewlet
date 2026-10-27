package artifactupload_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// artboard is one board of the landing page: a whole page with a script, so a
// stored board shows whether the linter marked it.
func artboard(heading string) string {
	return `<!doctype html><html><head><meta name="viewport" content="width=device-width">` +
		`<link rel="stylesheet" href="site.css"></head><body><h1>` + heading + `</h1>` +
		`<script>document.body.dataset.board = "` + heading + `";</script></body></html>`
}

const landingCanvas = `{
  "v": 1,
  "title": "Launch landing page",
  "boards": {
    "hero.html": {"x": 0, "y": 0, "w": 1440, "h": 900, "title": "Hero"},
    "features.html": {"x": 1540, "y": 0, "w": 1440, "h": 900, "title": "Features"},
    "pricing.html": {"x": 0, "y": 1000, "w": 1440, "h": 900, "title": "Pricing"},
    "mobile.html": {"x": 1540, "y": 1000, "w": 390, "h": 844, "title": "Mobile"}
  },
  "order": ["hero.html", "features.html", "pricing.html", "mobile.html"],
  "notes": [{"id": "s1", "kind": "sticky", "x": 3100, "y": 0, "w": 240, "h": 200,
    "text": "Copy from marketing", "color": "pink"}]
}`

// landingFiles is the four-board landing page a sandbox build writes.
func landingFiles() map[string]string {
	return map[string]string{
		"canvas.json":   landingCanvas,
		"hero.html":     artboard("Ship faster"),
		"features.html": artboard("Everything included"),
		"pricing.html":  artboard("Simple pricing"),
		"mobile.html":   artboard("On the go"),
		"site.css":      "body { margin: 0 } @media (prefers-color-scheme: dark) { body { color: #eee } }",
	}
}

// createDesign makes a design from files through an upload, the path a
// sandbox build takes, answering the outcome as the pipeline does.
func createDesign(h *harness, files map[string]string) (artifactsdomain.Published, error) {
	h.t.Helper()
	c := h.person("ada", h.opKey("create"))
	up := h.upload(c, artifacts.ID{}, files)
	got, _, err := h.svc.Create(h.t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindDesign, Title: "Landing page", Owner: c.Authority.Actor.Name,
		OwnerKind: c.Authority.Actor.Kind, Upload: up, Files: specs(files), Entry: canvasformat.Path,
	})
	return got, err
}

// A DESIGN IS PUBLISHED AS ITS BOARDS: every artboard goes through the page
// linter as an html entry does (each script carries the nonce placeholder), the
// canvas is held to its format, and the gallery's text is the canvas's words
// and then each board's, in the canvas's order.
func TestADesignIsPublishedBoardByBoard(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	got, err := createDesign(h, landingFiles())
	if err != nil {
		t.Fatalf("the landing page was refused: %v", err)
	}
	v := h.version(got.Artifact, 1)
	if v.Manifest.Entry != canvasformat.Path {
		t.Errorf("the design opens on %q", v.Manifest.Entry)
	}
	for _, p := range []string{"hero.html", "features.html", "pricing.html", "mobile.html"} {
		if strings.Count(h.file(v, p), artifacts.NoncePlaceholder) != 1 {
			t.Errorf("%s was stored unmarked:\n%s", p, h.file(v, p))
		}
		e, _ := manifest.EntryAt(v.Manifest, p)
		if e.Script == nil || !e.Script.Nonced {
			t.Errorf("%s carries no script policy: %+v", p, e.Script)
		}
	}
	text := h.searchText(got.Artifact)
	order := []string{"Launch landing page", "Hero", "Copy from marketing", "Ship faster",
		"Everything included", "Simple pricing", "On the go"}
	at := 0
	for _, word := range order {
		i := strings.Index(text[at:], word)
		if i < 0 {
			t.Fatalf("the search text misses %q after byte %d:\n%s", word, at, text)
		}
		at += i
	}
	if strings.Contains(text, `"x"`) || strings.Contains(text, "1440") {
		t.Errorf("the search text indexes the canvas's layout:\n%s", text)
	}
	if h.held() != 0 || h.unnamedBytes() != 0 {
		t.Errorf("a design that landed leaves %d held and %d unnamed bytes", h.held(), h.unnamedBytes())
	}
}

// A CANVAS OUTSIDE ITS FORMAT IS A POLICY REFUSAL ON canvas.json, naming every
// problem, and nothing the run produced is left behind.
func TestACanvasOutsideItsFormatIsRefused(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		change func(map[string]string)
		want   string
	}{
		"a board with no page": {func(f map[string]string) { delete(f, "mobile.html") },
			"mobile.html is not an HTML page"},
		"a page with no board": {func(f map[string]string) { f["extra.html"] = artboard("Extra") },
			"extra.html is an HTML page of this version that no board frames"},
		"not JSON": {func(f map[string]string) { f["canvas.json"] = "{" },
			"canvas.json is not JSON"},
		"an unknown field": {func(f map[string]string) {
			f["canvas.json"] = strings.Replace(landingCanvas, `"v": 1,`, `"v": 1, "pages": [],`, 1)
		}, "pages is not a key the canvas format carries"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, options{})
			files := landingFiles()
			c.change(files)
			_, err := createDesign(h, files)
			var refusal *lint.Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("answered %v, want a policy refusal", err)
			}
			found := false
			for _, f := range refusal.Findings {
				if f.Path == canvasformat.Path && f.Code == lint.CodeDesignFormat &&
					strings.Contains(f.Message, c.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("findings %+v name no %q on %s", refusal.Findings, c.want, canvasformat.Path)
			}
			// THE UPLOAD STAYS OPEN FOR THE RETRY, so its own bytes are
			// still staged; nothing the run produced from them is left.
			var staged int64
			for _, body := range files {
				staged += int64(len(body))
			}
			if n := h.unnamedBytes(); n != staged {
				t.Errorf("a refused design leaves %d unnamed bytes, and its upload staged %d", n, staged)
			}
		})
	}
}

// A PERSON'S REARRANGEMENT IS A VERSION THAT CHANGES THE CANVAS ALONE: an edit
// to canvas.json on its base carries every board by the object it already
// was, so no board's bytes move and every board keeps its policy.
func TestARearrangementChangesOnlyTheCanvas(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created, err := createDesign(h, landingFiles())
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("publish")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
		Edits: []edits.Edit{{Path: canvasformat.Path,
			Old: `"mobile.html": {"x": 1540, "y": 1000,`, New: `"mobile.html": {"x": 3200, "y": 400,`}},
	})
	if err != nil {
		t.Fatalf("the rearrangement was refused: %v", err)
	}
	before, after := h.version(created.Artifact, 1), h.version(created.Artifact, next.Number)
	for _, e := range before.Manifest.Files {
		moved, ok := manifest.EntryAt(after.Manifest, e.Path)
		switch {
		case !ok:
			t.Errorf("%s was dropped", e.Path)
		case e.Path == canvasformat.Path && moved.Object.Hash == e.Object.Hash:
			t.Error("the canvas is unchanged")
		case e.Path != canvasformat.Path && moved.Object.Key != e.Object.Key:
			t.Errorf("%s was stored again rather than carried", e.Path)
		}
	}
	if !strings.Contains(h.file(after, canvasformat.Path), `"x": 3200`) {
		t.Errorf("the new canvas reads\n%s", h.file(after, canvasformat.Path))
	}
}

// searchText is the text the gallery indexes an artifact by, as its row holds
// it.
func (h *harness) searchText(id artifacts.ID) string {
	h.t.Helper()
	var text string
	if err := h.domain.db.Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(), `SELECT search_text FROM artifacts WHERE id = ?`,
			id.String()).Scan(&text)
	}); err != nil {
		h.t.Fatalf("read the search text: %v", err)
	}
	return text
}

// A DESIGN IS HELD TO THE THEME WARNING AS A PAGE IS: its boards are pages, so a
// version none of whose pages or stylesheets follows the reader's appearance is
// warned, the finding naming its first board rather than the canvas.
func TestADesignThatIgnoresTheThemeIsWarnedOnItsFirstBoard(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	files := landingFiles()
	files["site.css"] = "body { margin: 0 }"
	c := h.person("ada", h.opKey("create"))
	up := h.upload(c, artifacts.ID{}, files)
	_, report, err := h.svc.Create(t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindDesign, Title: "Landing page", Owner: c.Authority.Actor.Name,
		OwnerKind: c.Authority.Actor.Kind, Upload: up, Files: specs(files), Entry: canvasformat.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	var found []lint.Finding
	for _, w := range report.Warnings {
		if w.Code == lint.CodeNoDarkMode {
			found = append(found, w)
		}
	}
	if len(found) != 1 || found[0].Path != "hero.html" {
		t.Errorf("theme warnings %+v, want one on hero.html", found)
	}
}

// A DESIGN BEGUN FROM A LAYOUT is published by the engine from the boards the
// person laid out, each a placeholder page linted as any board, opening on
// its canvas; a layout on another kind, a design starter with none, and a
// layout without the starter are refused before a byte moves.
func TestADesignBeginsFromItsLayout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.person("ada", h.opKey("start"))
	layout := &canvasformat.Layout{Boards: []canvasformat.LayoutBoard{
		{Title: "Home", W: 1440, H: 1024, Brief: "Hero with a sign-up form"},
		{Title: "Home", X: 1560, W: 390, H: 844},
	}}
	got, _, err := h.svc.Create(t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindDesign, Title: "Launch site", Owner: "ada",
		OwnerKind: c.Authority.Actor.Kind, Starter: true, Layout: layout,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := h.version(got.Artifact, 1)
	if v.Manifest.Entry != canvasformat.Path || len(v.Manifest.Files) != 3 {
		t.Fatalf("the starter published %d files opening on %q", len(v.Manifest.Files), v.Manifest.Entry)
	}
	if !strings.Contains(h.file(v, "boards/home.html"), "Hero with a sign-up form") ||
		!strings.Contains(h.file(v, "boards/home-2.html"), "390 by 844") {
		t.Errorf("the placeholders do not say what each board is for:\n%s", h.file(v, "boards/home.html"))
	}
	for _, req := range []artifactupload.CreateRequest{
		{Kind: artifacts.KindSlides, Title: "Deck", Starter: true, Layout: layout},
		{Kind: artifacts.KindDesign, Title: "Design", Starter: true},
		{Kind: artifacts.KindDesign, Title: "Design", Layout: layout},
		{Kind: artifacts.KindDesign, Title: "Design", Starter: true, Layout: &canvasformat.Layout{}},
	} {
		req.Owner, req.OwnerKind = "ada", c.Authority.Actor.Kind
		if _, _, err := h.svc.Create(t.Context(), h.person("ada", h.opKey("again")), req); !errors.Is(err, artifactupload.ErrInvalid) {
			t.Errorf("a %s create (starter %v, layout %v) answered %v", req.Kind, req.Starter, req.Layout != nil, err)
		}
	}
}
