package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
)

// conceptWidth is the fake provider's vector width, and the store's.
const conceptWidth = 64

// conceptServer is an OpenAI-compatible embeddings endpoint whose arithmetic
// is a MEANING rather than a spelling: every word maps to a concept, synonyms
// to the same one, and a text's vector is its bag of concepts. So "automobile
// upkeep" and "car maintenance" embed identically while sharing no word —
// which is the one document a lexical ranking can never find and a semantic
// one must.
//
// It counts the two kinds of request apart, because they are the two callers
// under test: a query is embedded one string at a time, and the duty embeds
// the corpus in batches.
type conceptServer struct {
	*httptest.Server

	mu          sync.Mutex
	queries     int
	batches     int
	failQueries bool
	models      map[string]bool
	askedWidths map[int]bool
}

// concepts folds the words the fixture uses onto shared meanings.
var concepts = map[string]string{
	"car": "vehicle", "automobile": "vehicle", "vehicle": "vehicle",
	"maintenance": "upkeep", "upkeep": "upkeep", "servicing": "upkeep",
}

func newConceptServer(t *testing.T) *conceptServer {
	t.Helper()
	s := &conceptServer{models: map[string]bool{}, askedWidths: map[int]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *conceptServer) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model      string          `json:"model"`
		Input      json.RawMessage `json:"input"`
		Dimensions int             `json:"dimensions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		!strings.HasSuffix(r.URL.Path, "/embeddings") {

		http.Error(w, "not an embeddings request", http.StatusBadRequest)
		return
	}
	var texts []string
	one := ""
	if json.Unmarshal(req.Input, &one) == nil {
		texts = []string{one}
	} else if err := json.Unmarshal(req.Input, &texts); err != nil {
		http.Error(w, "unreadable input", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	query := one != ""
	if query {
		s.queries++
	} else {
		s.batches++
	}
	s.models[req.Model] = true
	s.askedWidths[req.Dimensions] = true
	fail := query && s.failQueries
	s.mu.Unlock()
	if fail {
		http.Error(w, `{"error":{"message":"the provider is down"}}`,
			http.StatusServiceUnavailable)
		return
	}
	type item struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	}
	out := struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Data   []item `json:"data"`
		Usage  struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}{Object: "list", Model: req.Model}
	for i, text := range texts {
		out.Data = append(out.Data, item{Object: "embedding", Index: i,
			Embedding: conceptVector(text)})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// conceptVector is a text's bag of concepts, normalised.
func conceptVector(text string) []float64 {
	vector := make([]float64, conceptWidth)
	for word := range strings.FieldsSeq(strings.ToLower(text)) {
		word = strings.Trim(word, ".,:;!?")
		if concept, known := concepts[word]; known {
			word = concept
		}
		h := fnv.New32a()
		h.Write([]byte(word))
		vector[h.Sum32()%conceptWidth]++
	}
	var sum float64
	for _, v := range vector {
		sum += v * v
	}
	if sum == 0 {
		vector[0], sum = 1, 1
	}
	for i := range vector {
		vector[i] /= math.Sqrt(sum)
	}
	return vector
}

// counts is how many query and batch requests the server has answered.
func (s *conceptServer) counts() (queries, batches int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries, s.batches
}

func (s *conceptServer) failing(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failQueries = fail
}

// vectorCompany is a company on the native backends with the fake provider;
// extra is appended as it stands.
func vectorCompany(t *testing.T, provider, extra string) *config.Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
  embeddings:
    type: openai-compatible
    model: concepts-64
    base_url: %s
    api_key: sk-test
    dimensions: %d
roles:
  - name: CEO
    handle: ceo
    llm: zulu
%s`, provider, conceptWidth, extra)))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	return cfg
}

// vectorEngine boots a node on that company and hands back its embedding duty
// with the duty's own loop stopped, so a case ticks it when it chooses: the
// loop ticks once at boot and then once a minute, and a case that waited on
// it would be a case about the clock.
func vectorEngine(t *testing.T, cfg *config.Company) (*Engine, *embedDuty) {
	t.Helper()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	waitUntil(t, 20*time.Second, "the native backends to hydrate", e.NativeHydrated)
	duty := e.embedding
	if duty == nil {
		t.Fatal("a node on the native backends armed no embedding duty")
	}
	e.stopEmbedding()
	return e, duty
}

// writePage creates one page and waits for this node to apply it.
func writePage(t *testing.T, e *Engine, title, body string) pages.Page {
	t.Helper()
	got, err := e.PagesStore().Create(t.Context(),
		pages.Actor{Kind: pages.AuthorOperator, OperatorID: "ops-1"},
		pages.NewPage{Container: "ENG", Title: title, Body: body})
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	if err := e.WaitCommitted(t.Context(), got.Outcome.Position); err != nil {
		t.Fatalf("wait for %q to apply: %v", title, err)
	}
	return got.Page
}

// A HYBRID SEARCH FINDS WHAT ONLY THE VECTORS FIND, through the searchers the
// engine builds: the duty embeds the pages with the company's provider, and
// the search embeds its query ONCE with the same provider, model and width and
// fuses the two lists — so the page that shares no word with the query but
// means what it asks is the answer, above a page that means something else.
//
// Mutation: drop the query space from the knowledge searcher's fan-out and the
// query is never embedded — nothing matches its words, so nothing is found,
// and no query reaches the provider.
func TestAHybridSearchFindsWhatOnlyTheVectorsFind(t *testing.T) {
	t.Parallel()
	provider := newConceptServer(t)
	e, duty := vectorEngine(t, vectorCompany(t, provider.URL, ""))
	fleet := writePage(t, e, "Fleet handbook", "car maintenance schedule")
	writePage(t, e, "Budget notes", "quarterly budget review")
	searcher := e.Knowledge()
	org := e.Company().Org

	// LEXICALLY SEARCHABLE FIRST, so what the ranking answers below is the
	// ranking's rather than an index still on its first lap.
	waitUntil(t, 20*time.Second, "both pages to be searchable by their words", func() bool {
		cars := searcher.Search(t.Context(), knowledge.Query{Text: "car maintenance", Org: org})
		budgets := searcher.Search(t.Context(), knowledge.Query{Text: "quarterly budget", Org: org})
		return len(cars.Hits) == 1 && len(budgets.Hits) == 1
	})
	duty.tick(t.Context())
	if _, batches := provider.counts(); batches == 0 {
		t.Fatal("the duty embedded nothing with vectors on and two pages unembedded")
	}

	// NO WORD IN COMMON with either page, so only the meaning half can
	// answer at all.
	const meaning = "automobile upkeep"
	var answer knowledge.Answer
	waitUntil(t, 20*time.Second, "the pages' vectors to be applied", func() bool {
		answer = searcher.Search(t.Context(), knowledge.Query{Text: meaning, Org: org})
		return len(answer.Hits) > 0
	})
	if answer.Hits[0].PageID != fleet.ID {
		t.Errorf("%q ranked %q first, want the page only its meaning matches",
			meaning, answer.Hits[0].Title)
	}
	if answer.Partial != nil {
		t.Errorf("a hybrid answer that ran both halves carries %+v", answer.Partial)
	}

	// ONCE PER SEARCH, at the model and width the corpus was embedded at —
	// for the knowledge search and the work search alike.
	before, _ := provider.counts()
	searcher.Search(t.Context(), knowledge.Query{Text: meaning, Org: org})
	if _, err := WorkSearcher(e).Search(t.Context(), meaning, 5); err != nil {
		t.Fatalf("work search: %v", err)
	}
	if after, _ := provider.counts(); after-before != 2 {
		t.Errorf("two searches embedded %d queries, want one each", after-before)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.models) != 1 || !provider.models["concepts-64"] ||
		len(provider.askedWidths) != 1 || !provider.askedWidths[conceptWidth] {

		t.Errorf("the provider was asked for models %v at widths %v, want the "+
			"one the corpus is embedded at", provider.models, provider.askedWidths)
	}
}

// `knowledge.vectors: false` EMBEDS NOTHING, for the duty or for a query: a
// company that has a provider for its diary and keeps its search lexical pays
// for no page and no query. And the answer is whole, not degraded — a search
// that was never asked to be semantic has not skipped anything.
//
// Mutation: drop the `knowledge.vectors` check from [Engine.vectorSpace] and
// both counts below go above zero.
func TestVectorsOffEmbedsNothingForTheDutyOrTheQuery(t *testing.T) {
	t.Parallel()
	provider := newConceptServer(t)
	e, duty := vectorEngine(t, vectorCompany(t, provider.URL, `
knowledge:
  vectors: false
`))
	writePage(t, e, "Fleet handbook", "car maintenance schedule")
	searcher := e.Knowledge()
	org := e.Company().Org

	var answer knowledge.Answer
	waitUntil(t, 20*time.Second, "the page to be searchable by its words", func() bool {
		answer = searcher.Search(t.Context(), knowledge.Query{Text: "car maintenance", Org: org})
		return len(answer.Hits) == 1
	})
	duty.tick(t.Context())
	if _, err := WorkSearcher(e).Search(t.Context(), "car maintenance", 5); err != nil {
		t.Fatalf("work search: %v", err)
	}
	if queries, batches := provider.counts(); queries != 0 || batches != 0 {
		t.Errorf("with vectors off the provider was asked for %d queries and %d "+
			"batches, want none", queries, batches)
	}
	if answer.Partial != nil {
		t.Errorf("a search with no semantic half carries %+v", answer.Partial)
	}
}

// A QUERY THE PROVIDER WILL NOT EMBED IS ANSWERED ON ITS WORDS, AND SAYS SO.
// The search still runs — a turn must not lose its knowledge block because a
// provider is down — but its answer names the half that did not, so a seat
// reading a short list does not take it for every page on the subject.
func TestAQueryThatCannotBeEmbeddedIsAnsweredLexicallyAndMarked(t *testing.T) {
	t.Parallel()
	provider := newConceptServer(t)
	e, _ := vectorEngine(t, vectorCompany(t, provider.URL, ""))
	page := writePage(t, e, "Fleet handbook", "car maintenance schedule")
	searcher := e.Knowledge()
	org := e.Company().Org
	waitUntil(t, 20*time.Second, "the page to be searchable by its words", func() bool {
		answer := searcher.Search(t.Context(), knowledge.Query{Text: "car maintenance", Org: org})
		return len(answer.Hits) == 1
	})

	provider.failing(true)
	answer := searcher.Search(t.Context(), knowledge.Query{Text: "car maintenance", Org: org})
	if len(answer.Hits) != 1 || answer.Hits[0].PageID != page.ID {
		t.Errorf("the lexical answer is %+v, want the page its words match", answer.Hits)
	}
	if answer.Partial == nil || !answer.Partial.SemanticSkipped {
		t.Errorf("an answer whose query was not embedded carries %+v, want the "+
			"meaning half named as skipped", answer.Partial)
	}
	ranking, err := WorkSearcher(e).Search(t.Context(), "car maintenance", 5)
	if err != nil {
		t.Fatalf("work search: %v", err)
	}
	if ranking.Partial == nil || !ranking.Partial.SemanticSkipped {
		t.Errorf("a work ranking whose query was not embedded carries %+v", ranking.Partial)
	}
}
