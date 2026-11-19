package catalog

import (
	"fmt"
	"sync"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/provenance"
)

// FetchWebPageToolName reads a public web page, at an address the turn saw.
const FetchWebPageToolName = "fetch_web_page"

// webFetchesPerTurn is the most pages one turn reads: a hard vendor question
// needed three or four.
const webFetchesPerTurn = 8

var fetchWebPage = sync.OnceValues(func() (Tool, error) {
	t := Tool{
		Name: FetchWebPageToolName, Kind: KindRead, Tier: TierRead,
		Phrase: "read a web page",
		Description: "Read a public web page by its https address; the engine reads it, and " +
			"its text is the site's, not the person's. Only an address the person gave you, " +
			"a search result, a link of a page you read or an address a read answered is " +
			"read: never write one yourself. A long page is read in parts: pass the part's " +
			"next as from. Cite each page you rely on by its title and address.",
		schema: object([]string{"url"}, map[string]*schema{
			"url":  text("The page's https address.", 2048),
			"from": positive("Where the next part starts, as a read's next gave it.", 1<<21),
		}),
		Routes: []string{"GET /research/page"}, Fence: true, PerTurn: webFetchesPerTurn,
		Source: provenance.SourceWebPage, Trust: provenance.TrustExternal,
		Build: func(a map[string]any) (exec.Request, error) {
			return get("GET /research/page", nil, query(a).str("url", "url").
				num("from", "from", 0).values())
		},
		Cards: one(cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceWebPage, ID: "url", Title: "title", Name: "Web page",
			Prose: true, Text: "text",
			Facts: []cards.Fact{{Label: "Address", Path: "url", Type: tText, Mono: true},
				fact("Read", "read_at", tDatetime),
				{Label: "Next part from", Path: "next", Type: tNumber, Detail: true},
				{Label: "Length", Path: "length", Type: tNumber, Detail: true}}}}),
		Label: func(a map[string]any) string { return "Reading " + Args(a).String("url") },
	}
	t.Parameters = t.schema.JSON()
	if err := t.validate(); err != nil {
		return Tool{}, fmt.Errorf("catalog: %s: %w", t.Name, err)
	}
	return t, nil
})

// FetchWebPageTool is the web read. It is never in [Tools]: the orchestrator
// offers it beside them.
func FetchWebPageTool() Tool {
	t, err := fetchWebPage()
	if err != nil {
		panic(err)
	}
	return t
}

// SearchWebToolName searches the web through the model's own search run.
const SearchWebToolName = "search_web"

// webSearchesPerTurn is the most searches one turn makes: a hard vendor
// question needed two.
const webSearchesPerTurn = 4

var searchWeb = sync.OnceValues(func() (Tool, error) {
	t := Tool{
		Name: SearchWebToolName, Kind: KindRead, Tier: TierRead,
		Phrase: "search the web",
		Description: "Search the web: the results' titles and addresses, as the search " +
			"returned them. Read a result with " + FetchWebPageToolName + ". Never put the " +
			"company's data (names, keys, addresses) in a query.",
		schema: object([]string{"query"}, map[string]*schema{
			"query": text("What to find, in a few plain words.", 200),
		}),
		Routes: []string{"GET /research/search"}, Fence: true, PerTurn: webSearchesPerTurn,
		Source: provenance.SourceWebPage, Trust: provenance.TrustExternal,
		Build: func(a map[string]any) (exec.Request, error) {
			return get("GET /research/search", nil, query(a).str("query", "q").values())
		},
		Cards: one(cards.Spec{Table: &cards.Table{Title: "Search results", Rows: "results",
			Brief: []string{"title", "address"},
			Columns: []cards.Column{prose("title", "Title", "title"),
				{Key: "address", Label: "Address", Path: "url", Type: tText}}}}),
		Label: func(a map[string]any) string {
			return "Searching the web for “" + Args(a).String("query") + "”"
		},
		Scope: func(a Args) string { return "“" + a.String("query") + "”" },
	}
	t.Parameters = t.schema.JSON()
	if err := t.validate(); err != nil {
		return Tool{}, fmt.Errorf("catalog: %s: %w", t.Name, err)
	}
	return t, nil
})

// SearchWebTool is the web search. It is never in [Tools]: the orchestrator
// offers it where the model's harness has a search-only run.
func SearchWebTool() Tool {
	t, err := searchWeb()
	if err != nil {
		panic(err)
	}
	return t
}
