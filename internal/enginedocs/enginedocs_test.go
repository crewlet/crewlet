package enginedocs_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/crewlet/crewlet"
	"github.com/crewlet/crewlet/internal/enginedocs"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// tree is a small documentation tree: a guide with nested sections and a
// heading inside a code fence, and a decision record.
var tree = fstest.MapFS{
	"docs/guides/access.md": {Data: []byte("# Access\n\nHow agents get access.\n\n" +
		"## Grants for a change\n\nA grant for a change lasts its hours, 4 by default, " +
		"and 168 at most.\n\n### Asking for one\n\nAn agent calls request_access.\n\n" +
		"```\n## not a heading\n```\n\n## Levels\n\nA level is standing access.\n\n" +
		"## Levels\n\nThe second levels section.\n")},
	"adr/0001-a-grant-is-a-time-box.md": {Data: []byte("# ADR-0001: A grant is a time box\n\n" +
		"- **Status:** accepted\n\n## The decision\n\nA grant ends when its hours do.\n")},
	"docs/assets/logo.svg": {Data: []byte("<svg/>")},
}

func index(t *testing.T) *enginedocs.Index {
	t.Helper()
	ix, err := enginedocs.New(tree)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// A PAGE IS READ BY ITS SECTIONS. Each heading opens a section, named by its
// heading or its anchor (GitHub's slug, numbered where a heading repeats), a
// heading inside a fence is text, and a read hands back the section with what
// nests under it and stops at the next heading of its level. Named by none, a
// read is the page's top and lists its sections. Mutation: read headings
// inside fences, stop at a deeper heading, or hand back the whole page for its
// top, and this fails.
func TestAPageIsReadBySection(t *testing.T) {
	t.Parallel()
	ix := index(t)
	r, err := ix.Read("docs/guides/access.md", "Grants for a change")
	if err != nil {
		t.Fatal(err)
	}
	var headings []string
	for _, s := range r.Sections {
		headings = append(headings, s.Heading)
	}
	if !slices.Equal(headings, []string{"Grants for a change", "Asking for one"}) ||
		!strings.Contains(r.Sections[1].Text, "## not a heading") {
		t.Errorf("the section reads %v (%q), want it and the section nested under it, the "+
			"fenced line as text", headings, r.Sections[1].Text)
	}
	if r, err := ix.Read("docs/guides/access.md#levels-1", ""); err != nil ||
		r.Sections[0].Text != "The second levels section." {
		t.Errorf("a repeated heading's second anchor reads %+v, %v", r.Sections, err)
	}
	top, err := ix.Read("docs/guides/access.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(top.Sections) != 1 || top.Sections[0].Text != "How agents get access." ||
		!slices.Equal(top.Headings, []string{"Access", "Grants for a change", "Asking for one",
			"Levels", "Levels"}) {
		t.Errorf("the page's top reads %+v with headings %v", top.Sections, top.Headings)
	}
	if _, err := ix.Read("docs/guides/access.md", "Budgets"); !errors.Is(err,
		enginedocs.ErrNoSection) || !strings.Contains(err.Error(), "Grants for a change") {
		t.Errorf("a section the page lacks reads %v, want a refusal naming its sections", err)
	}
	if _, err := ix.Read("docs/guides/nothing.md", ""); !errors.Is(err, enginedocs.ErrNoPage) {
		t.Errorf("a page the tree lacks reads %v", err)
	}
	if slices.Contains(ix.Pages(), "docs/assets/logo.svg") {
		t.Error("an image was indexed as a page")
	}
}

// A SEARCH FINDS THE SECTION THAT ANSWERS: a section holding the query's terms
// ranks above one that does not, the page's title and the heading count as
// what a section is about, and a hit says where it is and quotes the text
// around the first term. Mutation: rank whole pages, or leave the heading out
// of what is indexed, and this fails.
func TestASearchFindsTheSectionThatAnswers(t *testing.T) {
	t.Parallel()
	hits := index(t).Search("how many hours by default", 3)
	if len(hits) == 0 {
		t.Fatal("nothing matched")
	}
	if hits[0].Where() != "docs/guides/access.md#grants-for-a-change" ||
		!strings.Contains(hits[0].Snippet, "4 by default") {
		t.Errorf("the best hit is %s (%q), want the section that says how long a grant lasts",
			hits[0].Where(), hits[0].Snippet)
	}
	if got := index(t).Search("time box", 1); len(got) != 1 ||
		got[0].Path != "adr/0001-a-grant-is-a-time-box.md" {
		t.Errorf("a decision record's title does not find it: %+v", got)
	}
	if got := index(t).Search("", 5); len(got) != 0 {
		t.Errorf("a query with no term found %d sections", len(got))
	}
}

// THE BINARY CARRIES EVERY PAGE THE TREE HAS: every Markdown page under docs/
// and adr/ is in the embedded documentation and its index, so a page added in
// a new folder is never missing from what Ask Crewlet can read. Mutation:
// drop a folder from the module root's embed, and this names its pages.
func TestTheBinaryCarriesEveryPageTheTreeHas(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	var want []string
	for _, dir := range []string{"docs", "adr"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && d.Name() == "assets":
				// THE IMAGES' FOLDER, whose note is about the pictures and
				// is not a page of the documentation.
				return fs.SkipDir
			case d.IsDir() || filepath.Ext(p) != ".md":
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			want = append(want, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ix, err := enginedocs.New(crewlet.Docs)
	if err != nil {
		t.Fatal(err)
	}
	got := ix.Pages()
	for _, p := range want {
		if !slices.Contains(got, p) {
			t.Errorf("%s is in the tree and not in the binary's documentation", p)
		}
	}
	if len(want) < 200 {
		t.Errorf("only %d pages were found in the tree; the walk is not reading it", len(want))
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "index.md")); err != nil {
		t.Fatal(err)
	}
	if hits := ix.Search("request_access grant hours", 5); len(hits) == 0 {
		t.Error("the shipped documentation answers nothing about an agent's access request")
	}
}

// THE SLUG IS GITHUB'S, as the docs gate holds every link in the tree to it.
func TestTheAnchorIsGitHubs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ heading, want string }{
		{"Information architecture", "information-architecture"},
		{"`window=` — one vocabulary for every time range", "window--one-vocabulary-for-every-time-range"},
		{"Spend › Budgets, raised in place", "spend--budgets-raised-in-place"},
		{"[Linked](other.md) heading", "linked-heading"},
		{"Café à la carte", "café-à-la-carte"},
	} {
		if got := enginedocs.Anchor(tc.heading); got != tc.want {
			t.Errorf("Anchor(%q) = %q, want %q", tc.heading, got, tc.want)
		}
	}
}
