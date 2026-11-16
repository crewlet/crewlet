// Package enginedocs searches and reads the engine's own docs/ and adr/ pages,
// embedded from the build's tree, by section: each heading opens one, named by
// its heading or GitHub's anchor for it ([Anchor]).
package enginedocs

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/crewlet/crewlet/internal/textindex"
)

// Section is one heading of a page and the text beneath it, up to the next
// heading of any level.
type Section struct {
	// Path is the page's path in the tree ("docs/concepts/assistant.md",
	// "adr/0118-…md").
	Path string
	// Title is the page's own title, its first top-level heading.
	Title string
	// Heading is the section's heading as written, without its marker; empty
	// for the text above the page's first heading.
	Heading string
	// Anchor is the heading's slug as GitHub renders it, numbered on
	// repetition ([Anchor]); empty for the text above the first heading.
	Anchor string
	// Level is the heading's level, 1 to 6, and 0 for the text above it.
	Level int
	// Text is the section's own text: everything after its heading line up to
	// the next heading of any level, fenced blocks included.
	Text string
}

// Where is the section as a reader names it: the page's path and, for a
// section below its top, the anchor after a `#`.
func (s Section) Where() string {
	if s.Anchor == "" {
		return s.Path
	}
	return s.Path + "#" + s.Anchor
}

// Hit is one section a search found, best first.
type Hit struct {
	Section
	// Score is its BM25 score, summed over the query's terms.
	Score float64
	// Snippet is the stretch of its text around the first term found.
	Snippet string
}

// Read is what a read answers: the sections asked for, the first being the
// one named (or the page's top), followed by every section that nests under
// it, and the headings of the page beside them.
type Read struct {
	Path  string
	Title string
	// Sections are the section read and those nested under it, in order.
	Sections []Section
	// Headings are every heading of the page, in order, so a reader can ask
	// for another.
	Headings []string
}

// ErrNoPage is a read naming a path the tree does not hold; ErrNoSection one
// naming a section its page does not have. Each refusal names what is there.
var (
	ErrNoPage    = errors.New("enginedocs: no such page")
	ErrNoSection = errors.New("enginedocs: no such section")
)

// Index is the tree's sections and the inverted list over them.
type Index struct {
	sections []Section
	// pages are each page's sections, by path, as indexes into sections.
	pages map[string][]int
	// postings are each term's sections; a posting's DocID is the section's
	// index.
	postings map[string][]textindex.Posting
	corpus   textindex.Corpus
}

// New indexes every Markdown page in tree, named by its path in tree. Pages are
// cut and analysed concurrently and merged in path order, so the index is the
// same whatever the scheduling.
func New(tree fs.FS) (*Index, error) {
	var paths []string
	err := fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && path.Ext(p) == ".md" {
			paths = append(paths, p)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	type analysed struct {
		sections []Section
		terms    []map[string]int
		err      error
	}
	pages := make([]analysed, len(paths))
	var wg sync.WaitGroup
	next := make(chan int)
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for i := range next {
				body, readErr := fs.ReadFile(tree, paths[i])
				if readErr != nil {
					pages[i].err = fmt.Errorf("enginedocs: read %s: %w", paths[i], readErr)
					continue
				}
				pages[i].sections = sectionsOf(paths[i], string(body))
				for _, sec := range pages[i].sections {
					// The page's title and the heading are part of what a section is about.
					pages[i].terms = append(pages[i].terms,
						textindex.Analyze(sec.Title+"\n"+sec.Heading+"\n"+sec.Text))
				}
			}
		})
	}
	for i := range paths {
		next <- i
	}
	close(next)
	wg.Wait()
	ix := &Index{pages: map[string][]int{}, postings: map[string][]textindex.Posting{}}
	total := 0
	for i, page := range pages {
		if page.err != nil {
			return nil, page.err
		}
		for j, sec := range page.sections {
			at := len(ix.sections)
			ix.pages[paths[i]] = append(ix.pages[paths[i]], at)
			ix.sections = append(ix.sections, sec)
			length := 0
			for _, n := range page.terms[j] {
				length += n
			}
			total += length
			id := strconv.Itoa(at)
			for term, n := range page.terms[j] {
				ix.postings[term] = append(ix.postings[term],
					textindex.Posting{DocID: id, Freq: n, Length: length})
			}
		}
	}
	ix.corpus = textindex.Corpus{Docs: len(ix.sections)}
	if len(ix.sections) > 0 {
		ix.corpus.AvgLength = float64(total) / float64(len(ix.sections))
	}
	return ix, nil
}

// Pages are the paths the index holds, in order.
func (ix *Index) Pages() []string {
	out := make([]string, 0, len(ix.pages))
	for p := range ix.pages {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// snippetBytes is the most of a section a hit quotes: two lines of a reader's
// screen, enough to tell a section that answers from one that only names the
// words.
const snippetBytes = 240

// Search is the sections that hold any of query's terms, best first, at most
// limit: each term's BM25 contribution under the prose profile, summed. A
// query with no term is none.
func (ix *Index) Search(query string, limit int) []Hit {
	terms := textindex.Terms(query)
	scores := map[int]float64{}
	for _, term := range terms {
		postings := ix.postings[term]
		if len(postings) == 0 {
			continue
		}
		idf := textindex.IDF(ix.corpus.Docs, len(postings))
		for _, p := range postings {
			i, _ := strconv.Atoi(p.DocID)
			scores[i] += textindex.ProseProfile.Score(idf, p, ix.corpus)
		}
	}
	hits := make([]Hit, 0, len(scores))
	for i, score := range scores {
		hits = append(hits, Hit{Section: ix.sections[i], Score: score})
	}
	// BEST FIRST, and in one order for two equal scores, so a search asked
	// twice answers the same list.
	sort.Slice(hits, func(a, b int) bool {
		if hits[a].Score != hits[b].Score {
			return hits[a].Score > hits[b].Score
		}
		return hits[a].Where() < hits[b].Where()
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	for i := range hits {
		hits[i].Snippet = textindex.Snippet(hits[i].Text, terms, snippetBytes)
	}
	return hits
}

// Read is the named section of page p (by heading or anchor, or p's `#anchor`)
// with what nests under it; naming none, it is the page's top and its headings.
func (ix *Index) Read(p, section string) (Read, error) {
	if before, anchor, cut := strings.Cut(p, "#"); cut && section == "" {
		p, section = before, anchor
	}
	at, ok := ix.pages[p]
	if !ok {
		return Read{}, fmt.Errorf("%w: %s is not a page of Crewlet's documentation; search "+
			"for what it covers to find the page", ErrNoPage, p)
	}
	page := make([]Section, 0, len(at))
	for _, i := range at {
		page = append(page, ix.sections[i])
	}
	out := Read{Path: p, Title: page[0].Title}
	for _, s := range page {
		if s.Heading != "" {
			out.Headings = append(out.Headings, s.Heading)
		}
	}
	start := 0
	if want := strings.TrimSpace(section); want != "" {
		start = slices.IndexFunc(page, func(s Section) bool {
			return s.Anchor != "" && (strings.EqualFold(s.Heading, want) ||
				s.Anchor == strings.TrimPrefix(want, "#") || s.Anchor == Anchor(want))
		})
		if start < 0 {
			return Read{}, fmt.Errorf("%w: %s has no section %q; its sections are %s",
				ErrNoSection, p, want, strings.Join(out.Headings, "; "))
		}
	}
	// Nested sections run to the next heading of its level; the page's top stops
	// at the first section, since a reference page is a megabyte.
	top := page[start].Level < 2
	out.Sections = append(out.Sections, page[start])
	for _, s := range page[start+1:] {
		if (top && s.Level >= 2) || (!top && s.Level <= page[start].Level) {
			break
		}
		out.Sections = append(out.Sections, s)
	}
	return out, nil
}

// The shapes of a page's lines. Compiled at first use, not at init: Gitlet's
// push hook is this binary, and pays every package's init (cmd/crewlet's
// hookinit_test.go).
var (
	headingRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$`)
	})
	fenceRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile("^\\s{0,3}(```|~~~)")
	})
)

// sectionsOf cuts one page into its sections: the text above its first heading
// (where it holds any), then one per heading, outside fenced blocks.
func sectionsOf(p, body string) []Section {
	var out []Section
	current := Section{Path: p}
	var text []string
	seen := map[string]int{}
	flush := func() {
		current.Text = strings.TrimSpace(strings.Join(text, "\n"))
		if current.Heading != "" || current.Text != "" {
			out = append(out, current)
		}
		text = text[:0]
	}
	fenced := false
	title := ""
	for _, line := range strings.Split(body, "\n") {
		// Only a line led by at most three spaces and a fence or a # can be one.
		lead := strings.TrimLeft(line, " ")
		indented := len(line)-len(lead) > 3
		if !indented && (strings.HasPrefix(lead, "```") || strings.HasPrefix(lead, "~~~")) &&
			fenceRE().MatchString(line) {
			fenced = !fenced
		}
		var m []string
		if !fenced && !indented && strings.HasPrefix(lead, "#") {
			m = headingRE().FindStringSubmatch(line)
		}
		if m == nil {
			text = append(text, line)
			continue
		}
		flush()
		heading := m[2]
		anchor := Anchor(heading)
		if n := seen[anchor]; n > 0 {
			anchor = anchor + "-" + strconv.Itoa(n)
		}
		seen[Anchor(heading)]++
		if title == "" && len(m[1]) == 1 {
			title = heading
		}
		current = Section{Path: p, Heading: heading, Anchor: anchor, Level: len(m[1])}
	}
	flush()
	if title == "" {
		title = p
	}
	for i := range out {
		out[i].Title = title
	}
	return out
}

// The parts of a heading GitHub does not render as text, compiled at first
// use as the line shapes above are.
var (
	headingLinkRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	})
	htmlTagRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`<[^>]+>`) })
)

// Anchor is GitHub's slug of a heading's rendered text: lower-cased, keeping
// letters, marks, digits, `_`, spaces as hyphens and hyphens. [New] numbers repeats.
func Anchor(heading string) string {
	text := headingLinkRE().ReplaceAllString(heading, "$1")
	text = htmlTagRE().ReplaceAllString(text, "")
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
