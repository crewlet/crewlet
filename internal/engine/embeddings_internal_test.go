package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
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
  llm:
    scripted:
      type: anthropic
      model: claude-x
      api_keys: ["sk-test"]
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
// blanked, since an absent provider and a misconfigured one are exactly what
// these two cases separate.
const noEmbeddingsDoc = `
name: Nimbus
providers:
  llm:
    scripted:
      type: anthropic
      model: claude-x
      api_keys: ["sk-test"]
roles:
  - name: CEO
    handle: ceo
    llm: scripted
`

// A COMPANY WITH NO EMBEDDINGS gets nil, which every consumer reads as "no
// similarity search" rather than as a fault — and no default is invented,
// because the store's columns are sized from the config and an embedder
// nobody asked for would write rows at whatever width its model produces.
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

// engineOverStore is an engine holding a store opened at width, which is the
// only part of Backends any of this reads.
func engineOverStore(t *testing.T, width int) *Engine {
	t.Helper()
	db, err := store.OpenNode(t.Context(), t.TempDir()+"/index.db",
		store.Options{EmbeddingDim: width})
	if err != nil {
		t.Fatalf("store.OpenNode: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// A DATA NODE'S REPLICATED ESTATE, which it opens at boot and every
	// domain-level reader here reads through.
	if _, err := db.OpenReplicated(t.Context(), estateLogs()); err != nil {
		t.Fatalf("open the replicated estate: %v", err)
	}
	return &Engine{backends: &Backends{Store: db}}
}

// A WIDTH THAT MOVED IS REFUSED AT THE APPLY, not discovered at the first
// recall weeks later. The store's vector columns are sized when it opens, so
// a revision that changes the width would have the writer producing vectors
// the reader cannot match — silently, since neither side errors on a
// dimension it never compares.
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

// The same width is not a change, and an apply that merely re-activates the
// revision — the documented rotation gesture — must not be refused by it.
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

// A STORE WITH NO WIDTH does not veto anything: it is a node whose company
// had no embeddings at open, and the check exists to protect written rows,
// of which there are none.
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

// NO EMBEDDER IS AN ANSWER, not a panic: the seam is a method value that
// reads the engine's current embedder on every call, so an engine that never
// stored one — or stored a nil — answers learning.ErrNoEmbeddings, which every
// consumer reads as "no similarity search" rather than as a fault.
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

// A STORED EMBEDDER IS USED AS ITS WHOLE-TEXT FORM: what the prefetch and the
// episodist embed — a turn's ask, a completed turn — has an end that matters
// as much as its beginning, and the provider refuses an input past the model's
// bound before sending it. So a short text is exactly Embed's vector and a
// long one is the pool of all of it, never "no similarity search".
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

// THE TWIN STARTS WHERE THE DEFAULT PROVIDER IS: the limits a fake embedder
// enforces out of the box are the ones the configuration resolves for
// OpenAI's models, so a test certified against the fake was certified against
// what production refuses — and a change to either side that the other did
// not follow fails here rather than in a company's corpus.
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
// included — a gateway that accepts less than the model is what the field is
// for, and a provider built at the model's own would send what it refuses.
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

// `knowledge.vectors: false` IS READ, and by every consumer of the corpus's
// embedding space at once.
//
// The switch was declared, validated and documented — "an explicit false keeps
// the search purely lexical" — and nothing read it: the duty went on embedding
// every document, billed, and the coverage gauge went on measuring it. It is
// now the one function the duty, the gauge and a search's query vector all
// read, so turning it off stops the three together.
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
// company with no provider is not one.
//
// `search_degraded` is a fraction of the answers. The half a search ASKED for
// and did not get counts toward it, whichever node lost it; the half nobody
// asked for — a keyword search, a company with no embeddings — must not, or
// the alarm is red for the life of such a deployment.
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
		// A SEMANTIC SEARCH THAT RAN NOTHING: the half it asked for is
		// the only half, and losing it to the provider is the alarm;
		// having no provider is not.
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
