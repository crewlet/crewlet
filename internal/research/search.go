package research

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/mcp"
)

// SearchTool is the curated Tavily server's search, the tool a research search
// calls (internal/mcp/catalogue's tavily entry, read at its pinned release).
const SearchTool = "tavily_search"

// The bounds of a search (ADR-0151).
const (
	// MaxQuery is the longest query a search sends.
	MaxQuery = 200
	// MaxResults is how many results a search asks for: two searches in a
	// hard question each needed a handful.
	MaxResults = 5
	// SnippetBytes is the most of a result's content kept.
	SnippetBytes = 600
)

// ErrSearch is a search the server could not answer, ErrSearchOff one on a
// company that names no search server, and ErrQuery a query out of bounds.
var (
	ErrSearch    = errors.New("research: the search could not be made")
	ErrQuery     = fmt.Errorf("research: a query is 1 to %d characters", MaxQuery)
	ErrSearchOff = errors.New("research: web search is off: an administrator names the " +
		"company's Tavily server in assistant.research.search.mcp_server")
)

// Result is one search result on a listed host.
type Result struct {
	Title   string `json:"title" untrusted:"name"`
	URL     string `json:"url"`
	Snippet string `json:"snippet" untrusted:"fence"`
}

// Results are a search's results on the listed hosts, and how many it dropped
// because they were on another.
type Results struct {
	Query   string   `json:"query"`
	Results []Result `json:"results"`
	Dropped int      `json:"dropped,omitempty"`
}

// Searcher runs this node's research instance of the company's search server
// on a bridge of its own, so no seat's catalogue ever holds it.
type Searcher struct {
	mu     sync.Mutex
	bridge *mcp.Bridge
}

// NewSearcher is a searcher with no instance running yet.
func NewSearcher() *Searcher { return &Searcher{} }

// Search asks the server spec runs for query, held to hosts: the query is
// bounded, the server is asked for those domains alone, and any result on
// another host is dropped.
func (s *Searcher) Search(ctx context.Context, spec mcp.Spec, hosts []string,
	query string) (Results, error) {

	query = strings.Join(strings.Fields(query), " ")
	if query == "" || utf8.RuneCountInString(query) > MaxQuery {
		return Results{}, ErrQuery
	}
	bridge := s.bridgeOf()
	if _, err := bridge.Reconcile(ctx, spec); err != nil {
		return Results{}, fmt.Errorf("%w: start %s: %w", ErrSearch, spec.Name, err)
	}
	blocks, err := bridge.Call(ctx, spec.Name, SearchTool, map[string]any{
		"query": query, "max_results": MaxResults, "include_domains": hosts,
	})
	if err != nil {
		return Results{}, fmt.Errorf("%w: %w", ErrSearch, err)
	}
	var text strings.Builder
	for _, b := range blocks {
		if b.Type == mcp.BlockText {
			text.WriteString(b.Text)
			text.WriteByte('\n')
		}
	}
	out := Results{Query: query, Results: []Result{}}
	for _, r := range parseResults(text.String()) {
		if _, err := Allowed(hosts, r.URL); err != nil {
			out.Dropped++
			continue
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

// Stop ends the research instance, if one runs.
func (s *Searcher) Stop(ctx context.Context) error {
	s.mu.Lock()
	bridge := s.bridge
	s.mu.Unlock()
	if bridge == nil {
		return nil
	}
	_, err := bridge.StopAll(ctx)
	return err
}

func (s *Searcher) bridgeOf() *mcp.Bridge {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bridge == nil {
		s.bridge = mcp.NewBridge(nil, mcp.Supervision{})
	}
	return s.bridge
}

// parseResults reads the server's text answer: a record per result, its
// `Title:`, `URL:` and `Content:` lines, content running on until the next
// field. Anything before the first title (a generated answer) is not a result.
func parseResults(text string) []Result {
	var out []Result
	var cur *Result
	field := ""
	for _, line := range strings.Split(text, "\n") {
		key, value, keyed := strings.Cut(line, ": ")
		switch {
		case keyed && key == "Title":
			out = append(out, Result{Title: strings.TrimSpace(value)})
			cur, field = &out[len(out)-1], "title"
		case cur == nil:
		case keyed && key == "URL":
			cur.URL, field = strings.TrimSpace(value), "url"
		case keyed && key == "Content":
			cur.Snippet, field = strings.TrimSpace(value), "content"
		case keyed && (key == "Raw Content" || key == "Favicon" || key == "ID"):
			field = ""
		case strings.TrimSpace(line) == "Images:":
			cur, field = nil, ""
		case field == "content" && strings.TrimSpace(line) != "":
			cur.Snippet += " " + strings.TrimSpace(line)
		}
	}
	for i := range out {
		if len(out[i].Snippet) > SnippetBytes {
			end := SnippetBytes
			for end > 0 && !utf8.RuneStart(out[i].Snippet[end]) {
				end--
			}
			out[i].Snippet = out[i].Snippet[:end] + "…"
		}
	}
	return out
}
