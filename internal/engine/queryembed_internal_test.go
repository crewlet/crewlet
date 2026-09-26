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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tools"
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

	// embedded is every text the server was asked for, with the models it
	// was asked for under — what lets a case say WHICH caller embedded with
	// which model, where models alone says only that somebody did.
	embedded map[string]map[string]bool
}

// concepts folds the words the fixture uses onto shared meanings.
var concepts = map[string]string{
	"car": "vehicle", "automobile": "vehicle", "vehicle": "vehicle",
	"maintenance": "upkeep", "upkeep": "upkeep", "servicing": "upkeep",
}

func newConceptServer(t *testing.T) *conceptServer {
	t.Helper()
	s := &conceptServer{models: map[string]bool{}, askedWidths: map[int]bool{},
		embedded: map[string]map[string]bool{}}
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
	for _, text := range texts {
		if s.embedded[text] == nil {
			s.embedded[text] = map[string]bool{}
		}
		s.embedded[text][req.Model] = true
	}
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

// embeddedUnder is the set of models text was embedded under, copied.
func (s *conceptServer) embeddedUnder(text string) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for model := range s.embedded[text] {
		out[model] = true
	}
	return out
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

// sandboxedVectorCompany is a company on the native backends with the fake
// embeddings provider at model, and sandbox as its providers.sandbox block.
//
// Its learning passes that call a model are off, so a turn reflected on here
// runs only the passes that call none — the episodist, which embeds, and the
// skill use stamp. The model the company names answers nobody in a test.
func sandboxedVectorCompany(t *testing.T, provider, model, sandbox string) *config.Company {
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
    model: %s
    base_url: %s
    api_key: sk-test
    dimensions: %d
  sandbox:
%s
learning:
  reflect:
    persist_decider: false
  skill_synthesis:
    enabled: false
  skill_refinement:
    enabled: false
  counterparty:
    enabled: false
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`, model, provider, conceptWidth, sandbox)))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	return cfg
}

// A REVISION AN APPLY REFUSED CHANGES NOTHING THE VECTORS ARE MADE OR READ BY.
//
// The refused revision moves the embedding model and is refused at its
// sandbox block, whose e2b key resolves to nothing. Everything on this node
// that embeds goes on embedding with the model the node serves, and stores
// what it made under that model's id:
//
//   - the duty and a search read the backend off the published epoch;
//   - the diary `reflect_and_persist` writes through is equipped into the
//     epoch it belongs to, which for a refused revision is never published;
//   - the reflection workers — the episodist, and the persist decider whose
//     diary is built in the same call — are handed over only in the apply's
//     commit, after every step that can refuse. A worker set handed over
//     before a refusal would be the refused revision's, reflecting every
//     completed turn as a company the node does not serve.
//
// Mutation: in [Engine.Apply], hand the reflection workers over before the
// revision's sandbox is built, and the completed turn is reflected by the
// refused revision's episodist — built from an epoch never equipped, so it
// files the turn with no vector and no model.
func TestARefusedRevisionMovesNoVector(t *testing.T) {
	t.Parallel()
	provider := newConceptServer(t)
	e, duty := vectorEngine(t, sandboxedVectorCompany(t, provider.URL, "concepts-a",
		"    fake: true"))
	searcher := e.Knowledge()
	org := e.Company().Org

	refused := sandboxedVectorCompany(t, provider.URL, "concepts-b", `    e2b:
      api_key: "${CREWLET_TEST_NO_SUCH_E2B_KEY}"`)
	if _, _, err := e.Apply(t.Context(), refused); err == nil {
		t.Fatal("a revision whose sandbox cannot be built was applied, so this " +
			"case cannot say what a refused one leaves behind")
	}
	if got := e.Company().Config.Providers.Embeddings.Model; got != "concepts-a" {
		t.Fatalf("the node serves model %q after a refused apply — the premise", got)
	}

	// A PAGE WRITTEN AFTER THE REFUSAL, so the duty has something to embed
	// under whichever backend it reads now.
	writePage(t, e, "Fleet handbook", "car maintenance schedule")
	waitUntil(t, 20*time.Second, "the page to be searchable by its words", func() bool {
		answer := searcher.Search(t.Context(), knowledge.Query{Text: "car maintenance", Org: org})
		return len(answer.Hits) == 1
	})
	duty.tick(t.Context())
	searcher.Search(t.Context(), knowledge.Query{Text: "automobile upkeep", Org: org})

	// THE DIARY, through the tool a seat keeps a note with.
	seat := org.AgentSeatByHandle("ceo")
	entry, found := e.Company().Tools.Lookup(builtin.ReflectAndPersistTool)
	if !found {
		t.Fatal("the served epoch offers no reflect_and_persist, so this case " +
			"cannot say which model the diary embeds with")
	}
	keep, ok := entry.Tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("reflect_and_persist is a %T, not a seat's tool", entry.Tool)
	}
	const note = "the fleet vehicles are serviced on fridays"
	kept, err := keep.CallForTurn(t.Context(),
		&turnctx.Turn{RunID: "run-note", Seat: seat, Org: org},
		map[string]any{"content": note})
	if err != nil || kept.Failed {
		t.Fatalf("keep a note: %v (%s)", err, kept.Output)
	}

	// THE EPISODIST, through the dispatcher every completed turn reaches.
	const summary = "car maintenance planned for the fleet"
	reflected := e.reflector.Reflect(t.Context(), types.TurnCompleted{
		AgentHandle: "ceo", RoleName: "CEO", TurnID: "run-episode",
		ReviewOutcome: "done", ToolSequence: []string{"search_knowledge"},
		TaskSummary: summary,
	}, events.TraceContext{})
	if !slices.Contains(reflected.Ran, learning.EpisodistSource) {
		t.Fatalf("the episodist did not run on a completed turn (%+v), so this "+
			"case cannot say which model it embeds with", reflected)
	}

	for what, text := range map[string]string{"the diary": note, "the episodist": summary} {
		if got := provider.embeddedUnder(text); len(got) != 1 || !got["concepts-a"] {
			t.Errorf("%s embedded %q under %v, want the served model alone",
				what, text, got)
		}
	}
	agentID, _ := org.AgentIDFor(seat)
	notes, err := learning.NewDiary(e.backends.Store).Recent(t.Context(),
		agentID.String(), time.Now().UTC(), 10)
	if err != nil || len(notes) != 1 || notes[0].EmbeddingModel != "concepts-a" {
		t.Errorf("the diary holds %+v (%v), want the note filed under the served model",
			notes, err)
	}
	episodes, err := learning.NewEpisodes(e.backends.Store).Recent(t.Context(), "ceo", 10)
	if err != nil || len(episodes) != 1 || episodes[0].EmbeddingModel != "concepts-a" {
		t.Errorf("the episode store holds %+v (%v), want the turn filed under the "+
			"served model", episodes, err)
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.models["concepts-b"] {
		t.Errorf("the provider was asked for the refused revision's model: %v",
			provider.models)
	}
	if !provider.models["concepts-a"] {
		t.Errorf("nothing was embedded with the served model, so this case "+
			"cannot tell a backend that moved from one that never ran: %v",
			provider.models)
	}
}
