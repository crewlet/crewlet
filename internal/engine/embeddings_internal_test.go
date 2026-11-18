package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

func companyWith(t *testing.T, doc string) *Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	c, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	return c
}

// embeddingDoc is a company whose only interesting property is its vector
// width; %d is the declared one.
const embeddingDoc = `
name: Nimbus
providers:
  sandbox: {fake: true}
  llm:
    scripted:
      model: claude-x
  embeddings:
    type: openai
    model: text-embedding-3-small
    api_key: sk-embed
    dimensions: %d
roles:
  - name: CEO
    handle: ceo
    llm: scripted
`

// noEmbeddingsDoc is the same company with the block removed rather than
// blanked: an absent provider and a misconfigured one differ.
const noEmbeddingsDoc = `
name: Nimbus
providers:
  sandbox: {fake: true}
  llm:
    scripted:
      model: claude-x
roles:
  - name: CEO
    handle: ceo
    llm: scripted
`

// A COMPANY WITH NO EMBEDDINGS gets nil, read as "no similarity search", and
// no default is invented, since the store's columns are sized from the config.
func TestNoEmbeddingsConfiguredMeansNoEmbedder(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	got, err := e.buildEmbedder(companyWith(t, noEmbeddingsDoc))
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if got != nil {
		t.Fatalf("embedder = %v, want none", got)
	}
}

func TestAConfiguredEmbedderIsBuiltAtItsDeclaredWidth(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	got, err := e.buildEmbedder(companyWith(t, fmt.Sprintf(embeddingDoc, 768)))
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if got == nil {
		t.Fatal("no embedder was built")
	}
	if got.Width() != 768 {
		t.Fatalf("width = %d, want the declared 768", got.Width())
	}
}

// engineOverStore is an engine holding a store opened at width, the only part
// of Backends any of this reads.
func engineOverStore(t *testing.T, width int) *Engine {
	t.Helper()
	// A data node's replicated estate, which every domain reader here reads.
	db, _ := storetest.OpenEstate(t, t.TempDir()+"/index.db",
		store.Options{EmbeddingDim: width}, estateLogs())
	t.Cleanup(func() { db.Close() })
	return &Engine{backends: &Backends{Store: db}}
}

// A WIDTH THAT MOVED IS REFUSED AT THE APPLY: the store's vector columns are
// sized at open, and vectors the reader cannot match fail silently.
func TestARevisionCannotResizeTheStoreItIsRunningOver(t *testing.T) {
	t.Parallel()
	e := engineOverStore(t, 1536)
	_, err := e.buildEmbedder(companyWith(t, fmt.Sprintf(embeddingDoc, 768)))
	if err == nil {
		t.Fatal("a width change was accepted against a store opened at another")
	}
	for _, want := range []string{"768", "1536", "restart"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q, which is what the "+
				"operator needs to act on it", err, want)
		}
	}
}

// The same width is not a change, so re-activating the revision to rotate a
// key is not refused.
func TestTheDeclaredWidthMatchingTheStoreIsNotAChange(t *testing.T) {
	t.Parallel()
	e := engineOverStore(t, 768)
	got, err := e.buildEmbedder(companyWith(t, fmt.Sprintf(embeddingDoc, 768)))
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if got == nil || got.Width() != 768 {
		t.Fatalf("embedder = %v, want one at 768", got)
	}
}

// A STORE WITH NO WIDTH vetoes nothing: the check protects written rows, and
// it has none.
func TestAStoreOpenedWithoutVectorsVetoesNothing(t *testing.T) {
	t.Parallel()
	e := engineOverStore(t, 0)
	got, err := e.buildEmbedder(companyWith(t, fmt.Sprintf(embeddingDoc, 3072)))
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if got == nil || got.Width() != 3072 {
		t.Fatalf("embedder = %v, want one at 3072", got)
	}
}

// NO EMBEDDER IS AN ANSWER, not a panic: an engine that never stored one, or
// stored a nil, answers learning.ErrNoEmbeddings.
func TestNoEmbedderAnswersThatNoneIsConfigured(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	if _, err := e.embedText(t.Context(), "anything"); !errors.Is(err, learning.ErrNoEmbeddings) {
		t.Fatalf("an engine that never stored an embedder answered %v", err)
	}
	var none embeddings.Embedder
	e.embeddings.Store(&none)
	if _, err := e.embedText(t.Context(), "anything"); !errors.Is(err, learning.ErrNoEmbeddings) {
		t.Fatalf("a stored nil embedder answered %v", err)
	}
}

// A STORED EMBEDDER IS USED AS ITS WHOLE-TEXT FORM: a short text is exactly
// Embed's vector, and one past the model's bound is the pool of all of it,
// never "no similarity search".
func TestAStoredEmbedderEmbedsTheWholeOfWhatItIsHanded(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	fake := embeddings.NewFake(4)
	var held embeddings.Embedder = fake
	e.embeddings.Store(&held)
	embed := e.embedText
	short := "the quick brown fox"
	v, err := embed(t.Context(), short)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	alone, err := embeddings.NewFake(4).Embed(t.Context(), short)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !slices.Equal(v.Values, alone) {
		t.Fatal("a short text is not the vector Embed gives it")
	}
	// TAGGED WITH THE SPACE IT IS IN, read off the embedder that made it.
	if v.Model != fake.Model() {
		t.Fatalf("the vector names model %q, want the embedder's %q", v.Model, fake.Model())
	}

	long := strings.Repeat("the deploy keeps failing on staging ", 400)
	if _, err := fake.Embed(t.Context(), long); !errors.Is(err, embeddings.ErrTooLong) {
		t.Fatalf("the fixture is wrong: Embed of %d bytes = %v", len(long), err)
	}
	v, err = embed(t.Context(), long)
	if err != nil {
		t.Fatalf("a text past the model's bound was not embedded whole: %v", err)
	}
	if len(v.Values) != 4 || v.Model != fake.Model() {
		t.Fatalf("vector = %d wide in %q, want the embedder's 4 in %q",
			len(v.Values), v.Model, fake.Model())
	}
}

// THE TWIN STARTS WHERE THE DEFAULT PROVIDER IS: the fake's default limits are
// those the configuration resolves for OpenAI's models, so a test certified
// against the fake is certified against what production refuses.
func TestTheFakeStartsAtTheDefaultModelsLimits(t *testing.T) {
	t.Parallel()
	got, err := (&Engine{}).buildEmbedder(companyWith(t, fmt.Sprintf(embeddingDoc, 1536)))
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if fake := embeddings.NewFake(4).Limits(); got.Limits() != fake {
		t.Fatalf("text-embedding-3-small resolves %+v; the fake starts at %+v", got.Limits(), fake)
	}
}

// THE PROVIDER IS BUILT AT THE LIMITS THE CONFIGURATION RESOLVES, a stated one
// included: a gateway that accepts less than the model is what the field is
// for.
func TestAConfiguredEmbedderIsBuiltAtTheStatedLimits(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(fmt.Sprintf(embeddingDoc, 1536),
		"    api_key: sk-embed\n",
		"    api_key: sk-embed\n    max_input_tokens: 512\n    max_batch_inputs: 16\n", 1)
	got, err := (&Engine{}).buildEmbedder(companyWith(t, doc))
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	want := embeddings.Limits{InputBytes: 512, BatchInputs: 16, BatchBytes: 300_000}
	if got.Limits() != want {
		t.Fatalf("Limits() = %+v, want %+v", got.Limits(), want)
	}
	if got.Model() != "text-embedding-3-small" {
		t.Errorf("Model() = %q", got.Model())
	}
}

// THE PROVIDER ASKS FOR ITS WIDTH ON THE CONFIGURATION'S RULE: a model whose
// endpoint documents `dimensions` as unsupported (embed-v4.0 through Cohere's
// Compatibility API) is built not to send it. The rule lives in config; this
// holds the engine's hand-off of it to the provider.
func TestTheEmbedderAsksForItsWidthOnTheConfigurationsRule(t *testing.T) {
	t.Parallel()
	for model, wantAsked := range map[string]bool{
		"embed-v4.0":             false,
		"text-embedding-3-small": true,
	} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			var (
				mu    sync.Mutex
				asked []bool
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				_, carried := req["dimensions"]
				mu.Lock()
				asked = append(asked, carried)
				mu.Unlock()
				vector := strings.TrimSuffix(strings.Repeat("0.5,", 1536), ",")
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[%s]}],"model":"m"}`, vector)
			}))
			t.Cleanup(srv.Close)
			doc := fmt.Sprintf(`
name: Nimbus
providers:
  sandbox: {fake: true}
  llm:
    scripted:
      model: claude-x
  embeddings:
    type: openai-compatible
    model: %s
    base_url: %s
    api_key: sk-embed
    max_batch_inputs: 96
    max_batch_tokens: 128000
roles:
  - name: CEO
    handle: ceo
    llm: scripted
`, model, srv.URL)
			got, err := (&Engine{}).buildEmbedder(companyWith(t, doc))
			if err != nil {
				t.Fatalf("buildEmbedder: %v", err)
			}
			if _, err := got.Embed(t.Context(), "the deploy keeps failing"); err != nil {
				t.Fatalf("Embed: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(asked) != 1 || asked[0] != wantAsked {
				t.Fatalf("the request carried dimensions: %v, want %v", asked, wantAsked)
			}
		})
	}
}

// `knowledge.vectors: false` IS READ, by the duty, the coverage gauge and a
// search's query vector at once: the one function all three read.
func TestTurningVectorsOffStopsTheDutyTheGaugeAndTheQueryVector(t *testing.T) {
	t.Parallel()
	var fake embeddings.Embedder = embeddings.NewFake(8)
	company := func(vectors *bool) *Company {
		return &Company{Config: &config.Company{
			Name: "Acme",
			Providers: config.Providers{Embeddings: &config.EmbeddingProvider{
				Model: "text-embedding-3-small", Dimensions: 8,
			}},
			Knowledge: config.Knowledge{Vectors: vectors},
		}}
	}
	off, on := false, true
	for _, tc := range []struct {
		name    string
		vectors *bool
		want    bool
	}{
		{"unset derives from the provider", nil, true},
		{"explicitly on", &on, true},
		{"explicitly off", &off, false},
	} {
		e := &Engine{}
		e.embeddings.Store(&fake)
		e.epoch.current.Store(company(tc.vectors))
		if _, _, got := e.embedModel(); got != tc.want {
			t.Errorf("%s: the duty and the gauge read configured=%v", tc.name, got)
		}
		if _, _, got := e.queryModel(); got != tc.want {
			t.Errorf("%s: a search's query vector reads configured=%v", tc.name, got)
		}
	}
}

// A QUERY VECTOR THE PROVIDER COULD NOT COMPUTE IS A DEGRADED SEARCH, and a
// company with no provider is not one: the half nobody asked for (a keyword
// search, no embeddings) must not count toward `search_degraded`, or the
// alarm is red for the life of such a deployment.
func TestAFailedQueryVectorCountsAsDegradedAndNoProviderDoesNot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		answer search.Answer
		want   string
	}{
		{search.Answer{Served: knowledge.ModeHybrid}, "full"},
		{search.Answer{Served: knowledge.ModeSemantic}, "full"},
		{search.Answer{Served: knowledge.ModeKeyword}, "off"},
		{search.Answer{Served: knowledge.ModeKeyword,
			Degraded: knowledge.DegradedNoEmbeddings}, "off"},
		{search.Answer{Degraded: knowledge.DegradedNoEmbeddings}, "off"},
		{search.Answer{Served: knowledge.ModeKeyword,
			Degraded: knowledge.DegradedEmbeddingFailed}, "skipped"},
		// A semantic search that ran nothing: losing its only half to the
		// provider is the alarm; having no provider is not.
		{search.Answer{Degraded: knowledge.DegradedEmbeddingFailed}, "skipped"},
		{search.Answer{Served: knowledge.ModeHybrid, SemanticSkipped: true,
			Degraded: knowledge.DegradedSemanticPartial}, "skipped"},
	} {
		if got := semanticState(tc.answer); got != tc.want {
			t.Errorf("served %q degraded %q skipped %v: semantic=%s, want %s",
				tc.answer.Served, tc.answer.Degraded, tc.answer.SemanticSkipped,
				got, tc.want)
		}
	}
	if searchRung(search.Answer{Served: knowledge.ModeKeyword}) != "keyword" ||
		searchRung(search.Answer{}) != "none" {
		t.Error("the scan histogram's rung is not the mode that was served")
	}
}
