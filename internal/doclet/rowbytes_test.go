package doclet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A LISTING CUT AT ITS BYTES SAYS SO WITH ITS CURSOR, and the cursor resumes
// at the first row refused: a page is one estate reply, refused whole past the
// transport's message, so a window ends at the bytes the read is held to
// (statelog.RowsOf) and its walk reaches every row once. Held to one byte,
// each window keeps only its first row, so the walk is one page per row.
//
// Mutations, each red: drop the TakeEncoded check in the reader's list, and
// the first window holds all three; mint the cut's cursor from the refused row
// rather than the last kept, and the walk skips Beta; leave the cursor unset
// on a cut, and the walk ends at Alpha.
func TestAListingCutAtItsBytesResumesAtTheRowItRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, title := range []string{"Alpha", "Beta", "Gamma"} {
		r.write(author("jane"), doclet.NewPage{Title: title, Body: "prose"})
	}
	fresh := statelog.Freshness{Level: statelog.ReadSession}

	whole := r.list(doclet.Filter{Container: "ENG", Limit: 10})
	if got := titles(whole); len(got) != 3 || whole.After != "" {
		t.Fatalf("unbounded, the listing is %v after %q, want all three and no cursor", got, whole.After)
	}

	held := statelog.WithAnswerBytes(t.Context(), 1)
	var walked []string
	after := ""
	for range 4 {
		page, err := r.reader.List(held, doclet.Filter{Container: "ENG", Limit: 10, After: after}, fresh)
		if err != nil {
			t.Fatalf("list after %q: %v", after, err)
		}
		if len(page.Pages) != 1 {
			t.Fatalf("a window held to one byte is %v, want its first row alone", titles(page))
		}
		if page.Total != 3 {
			t.Fatalf("a cut window says total %d, want 3: the total is the listing's", page.Total)
		}
		walked = append(walked, titles(page)...)
		if after = page.After; after == "" {
			break
		}
	}
	if want := []string{"Alpha", "Beta", "Gamma"}; !slices.Equal(walked, want) {
		t.Fatalf("the cut windows walked %v, want %v: each once, none skipped", walked, want)
	}
}

// THE CONTAINERS AND A CONTAINER'S SKILL PAGES PAGE AT THEIR BYTES: a node
// holding no data reads each a page at a time, and under a bound one row fits
// every container and every skill page comes back a row a page, each once and
// in order, as the whole read answers them.
//
// Mutations, each red: drop the Take check in ContainersPage or SkillPagesPage
// (one page holds every row); name the refused row as the next page's start
// (one row skipped).
func TestTheContainersAndSkillPagesPageAtTheirBytes(t *testing.T) {
	t.Parallel()
	r, _ := hooked(t)
	for _, title := range []string{"Alpha", "Bravo", "Charlie"} {
		r.write(author("jane"), doclet.NewPage{Title: title, Body: skillBody})
	}
	r.write(author("jane"), doclet.NewPage{Container: "OPS", Title: "Oncall", Body: "prose"})
	for i, key := range []string{"ENG", "OPS", "SALES"} {
		r.ensure(activation(i), key, key, "")
	}
	fresh := statelog.Freshness{Level: statelog.ReadSession}
	one := statelog.WithAnswerBytes(t.Context(), 1)

	var keys []string
	for _, c := range r.containers() {
		keys = append(keys, c.Key)
	}
	var paged []string
	for after, pages := "", 0; pages < 10; pages++ {
		list, next, err := r.reader.ContainersPage(one, after, fresh)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 {
			t.Errorf("a page of containers under one row's bytes held %d", len(list))
		}
		for _, c := range list {
			paged = append(paged, c.Key)
		}
		if after = next; after == "" {
			break
		}
	}
	if len(keys) < 2 || !slices.Equal(paged, keys) {
		t.Errorf("paged the containers %v, want %v", paged, keys)
	}

	whole, err := r.reader.SkillPages(t.Context(), "ENG", fresh)
	if err != nil || len(whole) != 3 {
		t.Fatalf("the skills are %d pages, %v; want three", len(whole), err)
	}
	var skills []string
	for after, pages := "", 0; pages < 10; pages++ {
		list, next, err := r.reader.SkillPagesPage(one, "ENG", after, fresh)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 {
			t.Errorf("a page of skills under one row's bytes held %d", len(list))
		}
		for _, p := range list {
			skills = append(skills, p.Title)
		}
		if after = next; after == "" {
			break
		}
	}
	if want := []string{"Alpha", "Bravo", "Charlie"}; !slices.Equal(skills, want) {
		t.Errorf("paged the skills %v, want %v", skills, want)
	}
}
