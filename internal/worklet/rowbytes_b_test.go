package worklet_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A FILE PAGE IS HELD TO ITS ANSWER'S BYTES AND STILL WALKS EVERY FILE: a page
// stops before the row that would take it past the bound the context carries,
// says so with `next` on its last row kept, and the walk from there reaches
// every file once, in path order, even under a bound smaller than one row.
// The largest page this package's estate ops answer (up to MaxFilePage rows).
//
// Mutations, each red: drop the Take in Reader.Files (the first page holds all
// five); drop the `held.Cut()` branch that sets Next (the walk stops after one
// page); set Next to the refused row's path (the walk skips a file); keep
// reading after a refusal instead of breaking (a page holds a later row).
func TestAFilePageIsHeldToItsBytesAndItsCursorResumesAfterTheLastRowKept(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	paths := []string{"a.md", "bb/c.md", "d-longer-name.md", "e.md", "folder/f.md"}
	for _, p := range paths {
		r.putFile("op-"+p, p, []byte(p))
	}
	whole, err := r.reader.Files(t.Context(), worklet.FileQuery{Project: "ENG", Freshness: stale})
	if err != nil {
		t.Fatal(err)
	}
	if len(whole.Files) != len(paths) || whole.Next != "" {
		t.Fatalf("the unbounded listing = %d files, next %q; want %d and none",
			len(whole.Files), whole.Next, len(paths))
	}
	var two int
	for _, f := range whole.Files[:2] {
		raw, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		two += len(raw)
	}

	page, err := r.reader.Files(statelog.WithAnswerBytes(t.Context(), two),
		worklet.FileQuery{Project: "ENG", Freshness: stale})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Files) != 2 || page.Next != paths[1] {
		t.Fatalf("a page bounded to two rows' bytes = %d files, next %q; want 2 and %q",
			len(page.Files), page.Next, paths[1])
	}

	var walked []string
	after := ""
	for pages := 0; ; pages++ {
		if pages > len(paths) {
			t.Fatalf("a walk under a one-byte bound never ended; read %v", walked)
		}
		page, err := r.reader.Files(statelog.WithAnswerBytes(t.Context(), 1),
			worklet.FileQuery{Project: "ENG", After: after, Freshness: stale})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Files) != 1 {
			t.Fatalf("a page under a one-byte bound held %d files, want the first "+
				"row alone", len(page.Files))
		}
		walked = append(walked, page.Files[0].Path)
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	if !slices.Equal(walked, paths) {
		t.Fatalf("the walk under a one-byte bound read %v, want %v", walked, paths)
	}
}

// A STRIP AND EVERY VIEW A PERSON SEES PAGE AT THEIR BYTES: under a bound one
// row fits, every view comes back a row a page and once, and the pages put in
// the whole read's order (the implicit views, then the viewer's pins first)
// are the whole read.
//
// Mutations, each red: drop the Take check over the saved views or over every
// saved view (one page holds them all); name the refused view as the next
// page's start (one skipped); send the implicit views again on a later page.
func TestAStripAndEveryViewPageAtTheirBytes(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	eng := worklet.Container{Kind: worklet.ContainerProject, ID: "ENG"}
	for _, id := range []string{"v-a", "v-b", "v-c"} {
		if _, err := save(t, r.writer, "op-"+id, aView(id, func(v *worklet.View) { v.Name = id })); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		r.drain()
	}
	one := statelog.WithAnswerBytes(t.Context(), 1)
	whole := r.strip(eng, "ana")
	var strip []worklet.ViewRow
	pages := 0
	for after := ""; pages < 20; {
		listing, next, err := r.reader.ViewsPage(one, worklet.ViewQuery{Container: eng, Viewer: "ana",
			Level: statelog.ReadStale}, after)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		strip = append(strip, listing.Views...)
		if after = next; after == "" {
			break
		}
	}
	// THE IMPLICIT VIEWS ride the first page whole, then a saved view a page.
	saved := len(savedIDs(whole))
	if got, want := stripKeys(worklet.ViewListing{Views: strip}), stripKeys(whole); !slices.Equal(got, want) ||
		saved != 3 || pages != 1+saved {
		t.Errorf("paged the strip %v over %d pages, want %v over %d", got, pages, want, 1+saved)
	}

	every, err := r.reader.EveryView(t.Context(), worklet.EveryViewQuery{Viewer: "ana", Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	var rows []worklet.ViewRow
	pages = 0
	for after := ""; pages < 20; {
		listing, next, err := r.reader.EveryViewPage(one, worklet.EveryViewQuery{Viewer: "ana",
			Level: statelog.ReadStale}, after)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		rows = append(rows, listing.Views...)
		if after = next; after == "" {
			break
		}
	}
	if got, want := savedIDs(worklet.ViewListing{Views: worklet.PinnedFirst(rows)}), savedIDs(every); !slices.Equal(got, want) ||
		pages != len(want) {
		t.Errorf("paged every view %v over %d pages, want %v over %d", got, pages, want, len(want))
	}
}
