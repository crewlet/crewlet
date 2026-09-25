package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/knowledge"
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
	db, err := store.Open(t.Context(), t.TempDir()+"/index.db",
		store.Options{EmbeddingDim: width})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
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

// The prefetch takes a FUNCTION, and nil is how it learns there is no
// similarity search — so an engine that built no embedder must hand it a nil
// func rather than a live method value closing over a nil interface, which
// is not nil and panics on the first call.
func TestNoEmbedderIsANilFuncNotAPanickingOne(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	if e.embedder() != nil {
		t.Fatal("an engine that never stored an embedder handed out a callable")
	}
	var none embeddings.Embedder
	e.embeddings.Store(&none)
	if e.embedder() != nil {
		t.Fatal("a stored nil embedder handed out a callable")
	}
}

func TestAStoredEmbedderIsHandedOutAsItsEmbedMethod(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	var fake embeddings.Embedder = embeddings.NewFake(4)
	e.embeddings.Store(&fake)
	embed := e.embedder()
	if embed == nil {
		t.Fatal("a stored embedder handed out nothing")
	}
	v, err := embed(t.Context(), "the quick brown fox")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(v) != 4 {
		t.Fatalf("vector width = %d, want the embedder's 4", len(v))
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
