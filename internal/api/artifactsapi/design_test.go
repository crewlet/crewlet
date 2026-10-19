package artifactsapi_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactserve"
)

// A DESIGN'S OUTLINE IS ITS BOARDS, in the canvas's order with each one's frame
// and the cover the gallery titles, read from the canvas alone: no artboard
// page is opened to draw a cover, and drawing one records no opening.
func TestADesignIsOutlinedFromItsCanvasAlone(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	before := g.objects.opens()
	w := g.do(person("bob"), "GET", at(design, "/versions/1/outline"), ``)
	f := fields(t, w)
	d, _ := f["design"].(map[string]any)
	if w.Code != http.StatusOK || f["kind"] != "design" || d["count"] != float64(2) ||
		d["cover"] != "mobile.html" || d["notes"] != float64(1) {
		t.Fatalf("the outline: %d %s", w.Code, w.Body)
	}
	boards, _ := d["boards"].([]any)
	first, _ := boards[0].(map[string]any)
	if first["path"] != "hero.html" || first["title"] != "Hero" || first["w"] != float64(1440) {
		t.Errorf("the first board is %v", first)
	}
	if bounds, _ := d["bounds"].(map[string]any); bounds["y"] != float64(-100) || bounds["w"] != float64(1930) {
		t.Errorf("the bounds are %v, want every board and note", bounds)
	}
	if n := g.objects.opens() - before; n != 1 {
		t.Errorf("the outline opened %d objects, want the canvas alone", n)
	}
	if len(g.marks.opened) != 0 {
		t.Error("a gallery cover recorded an opening")
	}
	if w := g.do(person("dana"), "GET", at(design, "/versions/1/outline"), ``); w.Code != http.StatusNotFound {
		t.Errorf("a stranger's outline: %d", w.Code)
	}
}

// A DESIGN IS OPENED BY READING ITS CANVAS, which the dashboard draws whether
// or not any artboard is framed, so the open is recorded there; and each
// artboard's frame names its board, which the ticket route hands the framer
// and refuses as invalid when the framer finds no such page.
func TestADesignFrameNamesItsBoard(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	if w := g.do(person("bob"), "GET", at(design, "/versions/1/source"), ``); w.Code != http.StatusOK ||
		fields(t, w)["text"] != string(canvasJSON) {
		t.Fatalf("the canvas: %d %s", w.Code, w.Body)
	}
	if len(g.marks.opened) != 1 {
		t.Errorf("reading the canvas recorded %d openings", len(g.marks.opened))
	}
	if w := g.do(person("bob"), "POST", at(design, "/tickets"), `{"document":"mobile.html"}`); w.Code != http.StatusCreated {
		t.Fatalf("a board's frame: %d %s", w.Code, w.Body)
	}
	if r := g.framer.requests[0]; r.Document != "mobile.html" || r.Head.ID != design {
		t.Errorf("the frame was asked for %q of %s", r.Document, r.Head.ID)
	}
	g.framer.refuse = fmt.Errorf("%w: no such page", artifactserve.ErrNoDocument)
	if w := g.do(person("bob"), "POST", at(design, "/tickets"), `{"document":"canvas.json"}`); w.Code != http.StatusUnprocessableEntity ||
		errorOf(t, w) != "invalid" {
		t.Errorf("a frame of no page: %d %s", w.Code, w.Body)
	}
}
