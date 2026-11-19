package harness

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// MaxSearchHits is the most results one search run answers.
const MaxSearchHits = 8

// SearchHit is one result of a search run: a page's title and address, taken
// from the search tool's own result and never from the model's prose.
type SearchHit struct {
	Title string
	URL   string
}

// ErrNoSearch is a provider whose profile has no search-only run.
var ErrNoSearch = errors.New("harness: this harness has no measured search-only run")

// Search runs the harness's search-only run ([harnessprofile.WebSearch]) on
// query, handed the query alone, and answers the results its search tool
// returned, at most [MaxSearchHits], each an https address once, and the
// tokens the run spent.
func (p *Provider) Search(ctx context.Context, query string) ([]SearchHit, int, error) {
	w := p.profile.WebSearch
	if p.profile.EffectiveWebTools() != harnessprofile.WebToolsSearch || w == nil {
		return nil, 0, ErrNoSearch
	}
	reader := &searchReader{format: w.Format, tool: w.Tool, calls: map[string]bool{},
		seen: map[string]bool{}}
	comp, err := p.Complete(withLineObserver(ctx, reader.read), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: searchPrompt(query)},
	}})
	if err != nil {
		return nil, 0, err
	}
	return reader.result(), comp.InputTokens + comp.OutputTokens, nil
}

// CanSearch reports whether the provider runs a search-only run.
func (p *Provider) CanSearch() bool {
	return p.profile.EffectiveWebTools() == harnessprofile.WebToolsSearch && p.profile.WebSearch != nil
}

// searchPrompt asks the run for one search and nothing else. The query is the
// only thing the run is handed.
func searchPrompt(query string) string {
	return fmt.Sprintf("Search the web once with your web search tool for the query below, "+
		"then reply with only the word done.\nQuery: %s", strings.Join(strings.Fields(query), " "))
}

// searchReader reads a search run's event stream line by line: the calls the
// model made to the search tool, and each result that answered one of them.
type searchReader struct {
	format harnessprofile.SearchFormat
	tool   string
	mu     sync.Mutex
	calls  map[string]bool
	seen   map[string]bool
	hits   []SearchHit
}

// read takes one line of the stream.
func (r *searchReader) read(line string) {
	doc, ok := decodeObject(strings.TrimSpace(line))
	if !ok || r.format != harnessprofile.SearchClaudeStreamJSON {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch doc["type"] {
	case "assistant":
		for _, block := range contentOf(doc) {
			if block["type"] == "tool_use" && block["name"] == r.tool {
				if id, _ := block["id"].(string); id != "" {
					r.calls[id] = true
				}
			}
		}
	case "user":
		answered := false
		for _, block := range contentOf(doc) {
			if id, _ := block["tool_use_id"].(string); block["type"] == "tool_result" && r.calls[id] {
				answered = true
			}
		}
		result, _ := doc["tool_use_result"].(map[string]any)
		if !answered || result == nil {
			return
		}
		results, _ := result["results"].([]any)
		for _, each := range results {
			// A STRING IS THE MODEL'S OWN PROSE between searches, never a result.
			search, _ := each.(map[string]any)
			links, _ := search["content"].([]any)
			for _, link := range links {
				r.add(link)
			}
		}
	}
}

// add keeps one result: an https address with a host, once, up to the bound.
func (r *searchReader) add(link any) {
	entry, _ := link.(map[string]any)
	raw, _ := entry["url"].(string)
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		r.seen[u.String()] || len(r.hits) >= MaxSearchHits {
		return
	}
	title, _ := entry["title"].(string)
	r.seen[u.String()] = true
	r.hits = append(r.hits, SearchHit{Title: strings.TrimSpace(title), URL: u.String()})
}

// result is what the run's search tool returned.
func (r *searchReader) result() []SearchHit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SearchHit(nil), r.hits...)
}

// contentOf is the content blocks of an event's message.
func contentOf(doc map[string]any) []map[string]any {
	message, _ := doc["message"].(map[string]any)
	blocks, _ := message["content"].([]any)
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		if block, ok := b.(map[string]any); ok {
			out = append(out, block)
		}
	}
	return out
}

// lineObserverKey carries a call's line observer ([withLineObserver]).
type lineObserverKey struct{}

// withLineObserver is ctx carrying observe, which a call made with it hands
// every line of its harness's event stream.
func withLineObserver(ctx context.Context, observe func(string)) context.Context {
	return context.WithValue(ctx, lineObserverKey{}, observe)
}

// lineObserverOf is ctx's line observer, or nil.
func lineObserverOf(ctx context.Context) func(string) {
	observe, _ := ctx.Value(lineObserverKey{}).(func(string))
	return observe
}
