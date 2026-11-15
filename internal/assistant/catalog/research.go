package catalog

import (
	"fmt"
	"sync"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/provenance"
)

// FetchWebPageToolName reads a page from a host the company lists (ADR-0151).
const FetchWebPageToolName = "fetch_web_page"

// webFetchesPerTurn is the most pages one turn reads: a hard vendor question
// needed three or four.
const webFetchesPerTurn = 8

var fetchWebPage = sync.OnceValues(func() (Tool, error) {
	t := Tool{
		Name: FetchWebPageToolName, Kind: KindRead, Tier: TierRead,
		Phrase: "read a web page",
		Description: "Read a page of a vendor's documentation, by its https address, from " +
			"the hosts the company lists for research; the engine reads it, and its text is " +
			"the site's, not the person's. A long page is read in parts: pass the part's " +
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
		Cards: one(cards.Spec{Entity: &cards.Entity{ID: "url", Title: "title", Name: "Web page",
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
// offers it while the company lists research hosts.
func FetchWebPageTool() Tool {
	t, err := fetchWebPage()
	if err != nil {
		panic(err)
	}
	return t
}
