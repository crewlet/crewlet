package catalog

import (
	"fmt"
	"strings"
	"sync"

	"github.com/crewlet/crewlet"
	"github.com/crewlet/crewlet/internal/enginedocs"
)

// The engine's own docs and decision records as two docs tools, answered in
// process from the embedded tree (internal/enginedocs), like describe_api.

// The two docs tools.
const (
	SearchEngineDocsTool = "search_engine_docs"
	ReadEngineDocTool    = "read_engine_doc"
)

// engineDocsHits is how many sections a search answers unless asked: eight
// short hits with their snippets, about two kilobytes, enough to see which
// page holds the answer, and engineDocsMaxHits the most it answers.
const (
	engineDocsHits    = 8
	engineDocsMaxHits = 20
)

// engineDocs is the tree's index, built once, at the first search or read: an
// index of the whole tree is a few megabytes, and a turn that never reads the
// documentation never pays for it.
var engineDocs = sync.OnceValues(func() (*enginedocs.Index, error) {
	return enginedocs.New(crewlet.Docs)
})

func engineDocTools() []Tool {
	return []Tool{
		{
			Name: SearchEngineDocsTool, Kind: KindDocs, Tier: TierRead,
			Phrase: "search Crewlet's documentation",
			Description: "Search Crewlet's own documentation and decision records, as this " +
				"build ships them, for how the engine itself works: what a setting, an " +
				"agent's tool, a limit, a permission or a screen does, and why it was decided " +
				"that way. Answers the matching sections, best first, each with where it is; " +
				"read one whole with " + ReadEngineDocTool + ". Search here before you explain " +
				"the engine's behaviour, rather than answering from memory.",
			schema: object([]string{"query"}, map[string]*schema{
				"query": text("What to find, in plain words.", queryBytes),
				"limit": positive(fmt.Sprintf("How many sections, at most. Default %d.",
					engineDocsHits), engineDocsMaxHits),
			}),
			Label: func(a map[string]any) string {
				return "Searching Crewlet's documentation for “" + Args(a).String("query") + "”"
			},
			Docs: searchEngineDocs,
		},
		{
			Name: ReadEngineDocTool, Kind: KindDocs, Tier: TierRead,
			Phrase: "read Crewlet's documentation",
			Description: "Read one section of Crewlet's own documentation or decision records, " +
				"with what nests under it, by the page and section " + SearchEngineDocsTool +
				" gave. With no section, it reads the top of the page and lists its sections.",
			schema: object([]string{"path"}, map[string]*schema{
				"path": text("The page's path, docs/... or adr/..., as the search gave it; a "+
					"#section after it reads that section.", lineBytes),
				"section": text("The section's heading or anchor.", lineBytes),
			}),
			Label: func(a map[string]any) string {
				return "Reading Crewlet's documentation"
			},
			Docs: readEngineDoc,
		},
	}
}

// searchEngineDocs is a [SearchEngineDocsTool] call's answer.
func searchEngineDocs(args Args) (string, error) {
	ix, err := engineDocs()
	if err != nil {
		return "", err
	}
	query := args.String("query")
	limit, ok := args.Int("limit")
	if !ok {
		limit = engineDocsHits
	}
	hits := ix.Search(query, min(limit, engineDocsMaxHits))
	if len(hits) == 0 {
		return fmt.Sprintf("Nothing in Crewlet's documentation or decision records matches "+
			"%q. Search again in other words.", query), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Sections of Crewlet's documentation and decision records matching %q, "+
		"best first. Read one whole with %s and its path.\n", query, ReadEngineDocTool)
	for i, h := range hits {
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n", i+1, h.Where(), sectionTitle(h.Section))
		if h.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", strings.Join(strings.Fields(h.Snippet), " "))
		}
	}
	return cut(b.String(), docsBytes, "More sections matched; narrow the search."), nil
}

// readEngineDoc is a [ReadEngineDocTool] call's answer.
func readEngineDoc(args Args) (string, error) {
	ix, err := engineDocs()
	if err != nil {
		return "", err
	}
	read, err := ix.Read(args.String("path"), args.String("section"))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n", read.Sections[0].Where(), sectionTitle(read.Sections[0]))
	for _, s := range read.Sections {
		if s.Heading != "" && s.Level > 1 {
			fmt.Fprintf(&b, "\n%s %s\n", strings.Repeat("#", s.Level), s.Heading)
		}
		if s.Text != "" {
			fmt.Fprintf(&b, "\n%s\n", s.Text)
		}
	}
	if len(read.Sections) == 1 && read.Sections[0].Level < 2 && len(read.Headings) > 1 {
		fmt.Fprintf(&b, "\nSections of this page: %s\n", strings.Join(read.Headings[1:], "; "))
	}
	return cut(b.String(), docsBytes, "The section goes on; read the sections under it "+
		"one at a time by their headings."), nil
}

// sectionTitle is a section as a reader names it: its page's title and its
// heading.
func sectionTitle(s enginedocs.Section) string {
	if s.Heading == "" || s.Heading == s.Title {
		return s.Title
	}
	return s.Title + " › " + s.Heading
}
