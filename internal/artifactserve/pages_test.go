package artifactserve_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE RUNTIME LEAVES THE FRAME EXACTLY THE PAGES.
//
// A frame can only hold a document that says hello, which is one served with
// the runtime placed in it; a frame navigated to anything else is closed by
// the dashboard as a page that left its frame. So the runtime follows a link
// to a page and opens a link to any other file in its viewer, and the
// extensions it takes for pages (ARTIFACT_PAGE_EXTENSIONS) must be exactly
// those this origin serves with the runtime when the frame navigates to them.
// One it missed would close the frame on a click; one it added would open a
// page in the viewer instead of the frame.
func TestTheRuntimeLeavesTheFrameExactlyThePages(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "ARTIFACT_PAGE_EXTENSIONS")
	if err != nil {
		t.Fatal(err)
	}
	dashboard := clientsource.Strings(body)

	r := newRig(t)
	v := r.reads.versions[2]
	files := slices.Clone(v.Manifest.Files)
	for _, row := range mediatype.Table() {
		path := "every" + row.Extension
		data := []byte("<!doctype html><title>every type</title>")
		files = append(files, artifacts.Entry{Path: path, ContentType: row.Types[0].Served,
			Object: r.put(data),
			// EVERY FILE AS THE LINTER MARKS A PAGE, so the runtime's absence
			// is the origin's decision about the type and never the facts'.
			Script: &artifacts.ScriptPolicy{Nonced: true}})
	}
	slices.SortFunc(files, func(a, b artifacts.Entry) int { return strings.Compare(a.Path, b.Path) })
	v.Manifest.Files = files
	r.reads.versions[2] = v
	r.build()

	at := base(r.frame())
	var served []string
	for _, row := range mediatype.Table() {
		res := r.do(request{url: at + "every" + row.Extension, dest: "iframe"})
		if bytes.Contains(res.data, []byte("/_crewlet/runtime/")) {
			served = append(served, strings.TrimPrefix(row.Extension, "."))
		}
	}
	if len(served) == 0 {
		t.Fatal("no file of any type was served with the runtime, so this gate reads nothing")
	}
	if !slices.Equal(dashboard, served) {
		t.Fatalf("ARTIFACT_PAGE_EXTENSIONS is %q and the origin serves %q with the runtime, "+
			"in the media table's order", dashboard, served)
	}
}
