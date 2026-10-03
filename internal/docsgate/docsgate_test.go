package docsgate

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestEveryRelativeLinkResolves holds every relative link in the tree's
// markdown to a file that exists. A page renamed or deleted leaves every link
// to it behind, and markdown has no compiler to notice.
func TestEveryRelativeLinkResolves(t *testing.T) {
	t.Parallel()
	c := load(t)
	links := 0
	for _, page := range c.pages {
		for _, l := range page.links {
			if l.path == "" {
				continue
			}
			links++
			target := filepath.Join(filepath.Dir(page.abs), filepath.FromSlash(l.path))
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s:%d links %q, and there is no such file.\n"+
					"\tA link renders live whether or not its target exists — point it at "+
					"the page that holds the subject now, or drop the link.",
					page.rel, l.line, l.raw)
			}
		}
	}
	// A FLOOR: a walk that found nothing, or a link matcher that stopped
	// matching, would pass this test by checking nothing.
	if links < 100 {
		t.Fatalf("checked only %d relative links — this gate is asserting nothing", links)
	}
}

// TestEveryAnchorNamesAHeading holds every `#fragment` — on another page or on
// the same one — to a heading the target renders under that slug. A heading
// renamed in one commit breaks every anchor to it with no symptom but a reader
// landing at the top of a long page.
func TestEveryAnchorNamesAHeading(t *testing.T) {
	t.Parallel()
	c := load(t)
	anchors := 0
	for _, page := range c.pages {
		for _, l := range page.links {
			if l.fragment == "" {
				continue
			}
			target := page
			if l.path != "" {
				if !strings.HasSuffix(l.path, ".md") {
					continue
				}
				abs := filepath.Clean(filepath.Join(filepath.Dir(page.abs), filepath.FromSlash(l.path)))
				found, ok := c.byAbs[abs]
				if !ok {
					// Reported by TestEveryRelativeLinkResolves.
					continue
				}
				target = found
			}
			anchors++
			if !target.anchors[l.fragment] {
				t.Errorf("%s:%d links %q, and %s has no heading with the slug %q.\n"+
					"\tThe anchor is GitHub's slug of the heading's text (see the package "+
					"doc) — rename the anchor to the heading's current slug, or link the "+
					"page that holds the subject now.",
					page.rel, l.line, l.raw, target.rel, l.fragment)
			}
		}
	}
	if anchors < 50 {
		t.Fatalf("checked only %d anchors — this gate is asserting nothing", anchors)
	}
}

// TestEveryDocsPageIsInTheIndex holds docs/index.md to the pages under docs/.
// The index is the docs site's navigation, so a page it does not list is a
// page the site does not build a way to — and one it lists that is gone is a
// dead entry on the site's front page.
func TestEveryDocsPageIsInTheIndex(t *testing.T) {
	t.Parallel()
	c := load(t)
	index, ok := c.byRel["docs/index.md"]
	if !ok {
		t.Fatal("docs/index.md is not in the walk — this gate is asserting nothing")
	}
	listed := map[string]bool{}
	for _, l := range index.links {
		if l.path == "" {
			continue
		}
		abs := filepath.Clean(filepath.Join(filepath.Dir(index.abs), filepath.FromSlash(l.path)))
		listed[abs] = true
	}
	pages := 0
	for _, page := range c.pages {
		if !strings.HasPrefix(page.rel, "docs/") || page.rel == "docs/index.md" {
			continue
		}
		// docs/assets/ is the site's image directory; its README describes the
		// files for somebody editing them and is not a page of the product.
		if strings.HasPrefix(page.rel, "docs/assets/") {
			continue
		}
		pages++
		if !listed[page.abs] {
			t.Errorf("%s is not linked from docs/index.md.\n"+
				"\tThe index is the docs site's navigation: add the page under the "+
				"section that holds its subject, with a one-line summary of what it "+
				"answers.", page.rel)
		}
	}
	if pages < 50 {
		t.Fatalf("found only %d pages under docs/ — this gate is asserting nothing", pages)
	}
}

// TestTheSlugIsGitHubs pins the slug rule to headings whose GitHub anchors are
// known, including the shapes a private approximation gets wrong: an em dash
// between two spaces leaves TWO hyphens, a symbol vanishes without a trace,
// an underscore survives, and a repeated heading is numbered.
func TestTheSlugIsGitHubs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ heading, want string }{
		{"Information architecture", "information-architecture"},
		{"`window=` — one vocabulary for every time range", "window--one-vocabulary-for-every-time-range"},
		{"⌘K: search, answer, act", "k-search-answer-act"},
		{"Spend › Budgets, raised in place", "spend--budgets-raised-in-place"},
		{"The `budget_meters` push", "the-budget_meters-push"},
		{"Step 4. Open the dashboard", "step-4-open-the-dashboard"},
		{"[Linked](other.md) heading", "linked-heading"},
		{"**Bold** and *italic*", "bold-and-italic"},
		{"Café à la carte", "café-à-la-carte"},
	} {
		if got := slug(tc.heading); got != tc.want {
			t.Errorf("slug(%q) = %q, want %q", tc.heading, got, tc.want)
		}
	}
	got := anchorsOf([]string{"# A", "## Setup", "```", "## Not a heading", "```", "## Setup", "### Setup"})
	for _, want := range []string{"a", "setup", "setup-1", "setup-2"} {
		if !got[want] {
			t.Errorf("anchors %v lack %q", keys(got), want)
		}
	}
	if got["not-a-heading"] {
		t.Error("a heading inside a fenced block produced an anchor")
	}
}

// TestALinkInCodeIsNotALink pins the extractor's two exclusions: a link written
// inside a fenced block or an inline code span is an EXAMPLE, and following it
// would fail the gate on documentation about writing links.
func TestALinkInCodeIsNotALink(t *testing.T) {
	t.Parallel()
	lines := []string{
		"See [the guide](guide.md#setup) and [here](#local).",
		"Write `[x](nowhere.md)` to link.",
		"```",
		"[y](also-nowhere.md)",
		"```",
		"[ref]: reference.md",
		"[^note]: A footnote, not a link.",
		"[spaced](A%20Page.md)",
		"An [absolute](https://example.com/x.md) link.",
	}
	var got []string
	for _, l := range linksOf(lines) {
		got = append(got, fmt.Sprintf("%d:%s#%s", l.line, l.path, l.fragment))
	}
	want := []string{"1:guide.md#setup", "1:#local", "6:reference.md#", "8:A Page.md#"}
	if !slices.Equal(got, want) {
		t.Errorf("links = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------

type link struct {
	raw      string
	path     string // relative file path, "" for a same-page anchor
	fragment string // "" when the link names no anchor
	line     int
}

type page struct {
	rel, abs string
	links    []link
	anchors  map[string]bool
}

type corpus struct {
	pages []*page
	byAbs map[string]*page
	byRel map[string]*page
}

func load(t *testing.T) *corpus {
	t.Helper()
	root := sourcetree.Root(t)
	c := &corpus{byAbs: map[string]*page{}, byRel: map[string]*page{}}
	// sourcetree.Walk rather than a walk of this gate's own: VCS metadata, a
	// nested checkout (.claude/worktrees/ holds full copies of this tree at
	// other commits) and node_modules are not this tree's pages, and a gate
	// that read them would report a copy's dead link as this tree's, or pass
	// an anchor on a copy's heading. What stays here is this gate's own
	// decision: the committed bundle, build output and the rendering fixtures.
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if d.IsDir() {
			switch {
			case d.Name() == "dist", rel == "static", rel == "dashboard/src/test/markdown":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(raw), "\n")
		p := &page{rel: rel, abs: filepath.Clean(path), links: linksOf(lines), anchors: anchorsOf(lines)}
		c.pages = append(c.pages, p)
		c.byAbs[p.abs] = p
		c.byRel[p.rel] = p
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return c
}

var (
	inlineLinkRE = regexp.MustCompile(`\]\(\s*<?([^()\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	refDefRE     = regexp.MustCompile(`^\s{0,3}\[[^\]^][^\]]*\]:\s+<?([^\s>]+)>?`)
	codeSpanRE   = regexp.MustCompile("`+[^`]*`+")
	fenceRE      = regexp.MustCompile("^\\s{0,3}(```|~~~)")
	headingRE    = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$`)
	explicitRE   = regexp.MustCompile(`<a\s+(?:id|name)="([^"]+)"`)
	schemeRE     = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

// linksOf is every link a page renders, outside fenced blocks and code spans,
// that names a file in this tree or an anchor.
func linksOf(lines []string) []link {
	var out []link
	fenced := false
	for i, line := range lines {
		if fenceRE.MatchString(line) {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		prose := codeSpanRE.ReplaceAllString(line, "")
		var targets []string
		for _, m := range inlineLinkRE.FindAllStringSubmatch(prose, -1) {
			targets = append(targets, m[1])
		}
		if m := refDefRE.FindStringSubmatch(prose); m != nil {
			targets = append(targets, m[1])
		}
		for _, target := range targets {
			if schemeRE.MatchString(target) || strings.HasPrefix(target, "//") {
				continue
			}
			path, fragment, _ := strings.Cut(target, "#")
			path, _, _ = strings.Cut(path, "?")
			if unescaped, err := url.PathUnescape(path); err == nil {
				path = unescaped
			}
			out = append(out, link{raw: target, path: path, fragment: fragment, line: i + 1})
		}
	}
	return out
}

// anchorsOf is every anchor a page renders: each heading's slug, numbered on
// repetition, plus any explicit `<a id>`/`<a name>`.
func anchorsOf(lines []string) map[string]bool {
	out := map[string]bool{}
	seen := map[string]int{}
	fenced := false
	for _, line := range lines {
		if fenceRE.MatchString(line) {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		for _, m := range explicitRE.FindAllStringSubmatch(line, -1) {
			out[m[1]] = true
		}
		m := headingRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		s := slug(m[2])
		if n := seen[s]; n > 0 {
			out[fmt.Sprintf("%s-%d", s, n)] = true
		} else {
			out[s] = true
		}
		seen[s]++
	}
	return out
}

var (
	headingLinkRE = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	htmlTagRE     = regexp.MustCompile(`<[^>]+>`)
)

// slug is GitHub's anchor for a heading: the RENDERED text (a link is its
// label, a tag is nothing), lower-cased, keeping only letters, marks, digits,
// connector punctuation, spaces and hyphens, with each space a hyphen.
func slug(heading string) string {
	text := headingLinkRE.ReplaceAllString(heading, "$1")
	text = htmlTagRE.ReplaceAllString(text, "")
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-', unicode.IsLetter(r), unicode.IsMark(r), unicode.IsDigit(r),
			unicode.Is(unicode.Pc, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
