package artifactdata_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PAGE OF A COLLECTION HOLDS ITS DOCUMENTS TO THE BYTES ITS READ CARRIES,
// short of its limit where they run out, and says so with a cursor that
// resumes at the first document it left out: paging to the end reads every
// document exactly once.
//
// Mutations, each red: drop the Take in scanDocs (a page holds all five);
// answer no cursor on a cut page (paging stops after the first); cut the
// cursor from the refused document (one is skipped).
func TestAPageOfACollectionStopsAtItsBytesAndResumesWhereItStopped(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	chunk := `"` + strings.Repeat("x", 3500) + `"`
	unread := map[string]bool{}
	var ops []artifacts.DataOp
	for i := range 5 {
		id := fmt.Sprintf("d%d", i)
		ops = append(ops, set("rows", id, map[string]string{"a": chunk, "b": chunk, "c": chunk}))
		unread[id] = true
	}
	n.applied(jane, ops...)

	// Room for two documents of about ten kibibytes each, never three.
	ctx := statelog.WithAnswerBytes(t.Context(), 25<<10)
	cursor, pages := "", 0
	for {
		page, err := n.reader.Docs(ctx, sheet, reading("jane", artifacts.RoleOwner), "rows", 0, cursor, 100)
		if err != nil {
			t.Fatalf("read page %d of the collection: %v", pages+1, err)
		}
		pages++
		if len(page.Docs) > 2 {
			t.Fatalf("page %d held %d documents of ten kibibytes within 25, want at most two",
				pages, len(page.Docs))
		}
		for _, d := range page.Docs {
			if !unread[d.ID] {
				t.Fatalf("page %d read document %s twice, or one never written", pages, d.ID)
			}
			delete(unread, d.ID)
		}
		if page.After == "" {
			break
		}
		if pages == 5 {
			t.Fatal("paging five documents did not end within five pages")
		}
		cursor = page.After
	}
	if len(unread) != 0 {
		t.Errorf("paging to the end left %d documents unread", len(unread))
	}
	if pages != 3 {
		t.Errorf("read %d pages, want three: two documents, two, then one", pages)
	}
}

// A VIEWER'S STATE PAGES AT ITS BYTES: under a bound one key fits, every key
// the viewer keeps comes back a key a page, each once and in order, as the
// whole read answers it.
//
// Mutations, each red: drop the Take check in StatePage (one page holds every
// key); answer a cut page no next document (paging stops after one).
func TestAViewersStatePagesAtItsBytes(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	bob := person("bob", artifacts.RoleViewer)
	for _, key := range []string{"filter", "sort", "theme"} {
		doc, err := artifactdata.StateDocID("bob", key)
		if err != nil {
			t.Fatal(err)
		}
		n.applied(bob, set(artifactdata.StateCollection, doc, map[string]string{artifactdata.StateField: `"x"`}))
	}
	whole, err := n.reader.State(t.Context(), sheet, reading("bob", artifacts.RoleViewer))
	if err != nil || len(whole) != 3 {
		t.Fatalf("bob's state is %+v, %v; want three keys", whole, err)
	}
	one := statelog.WithAnswerBytes(t.Context(), 1)
	var keys []string
	pages := 0
	for after := ""; pages < 10; {
		entries, next, err := n.reader.StatePage(one, sheet, reading("bob", artifacts.RoleViewer), after)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range entries {
			keys = append(keys, e.Key)
		}
		if after = next; after == "" {
			break
		}
	}
	if want := []string{"filter", "sort", "theme"}; strings.Join(keys, ",") != strings.Join(want, ",") || pages != 3 {
		t.Errorf("paged bob's state %v over %d pages, want %v over three", keys, pages, want)
	}
}
