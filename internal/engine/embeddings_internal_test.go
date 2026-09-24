package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
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

// A BACKEND CARRIES THE MODEL ITS PROVIDER WAS BUILT WITH, and its revision's
// own search switch, so no reader has to take either from anywhere else.
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
	if got.embedder.Width() != 768 {
		t.Fatalf("width = %d, want the declared 768", got.embedder.Width())
	}
	if got.model != "text-embedding-3-small" || !got.search {
		t.Errorf("the backend carries model %q and search %v, want the declared "+
			"model and the switch that derives on from a configured provider",
			got.model, got.search)
	}
	off, err := e.buildEmbedder(companyWith(t, fmt.Sprintf(embeddingDoc, 768)+`
knowledge:
  vectors: false
`))
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if off == nil || off.search {
		t.Errorf("a revision with knowledge.vectors off built %+v, want a backend "+
			"its search does not use", off)
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
	if got == nil || got.embedder.Width() != 768 {
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
	if got == nil || got.embedder.Width() != 3072 {
		t.Fatalf("embedder = %v, want one at 3072", got)
	}
}

// The prefetch takes a FUNCTION, and nil is how it learns there is no
// similarity search — so an epoch with no backend, and no epoch at all, must
// hand it a nil func rather than a live method value closing over a nil
// interface, which is not nil and panics on the first call.
func TestNoEmbedderIsANilFuncNotAPanickingOne(t *testing.T) {
	t.Parallel()
	if (*Company)(nil).embed() != nil {
		t.Fatal("no epoch handed out a callable")
	}
	if (&Company{}).embed() != nil {
		t.Fatal("an epoch with no embedding backend handed out a callable")
	}
}

func TestAStoredEmbedderIsHandedOutAsItsEmbedMethod(t *testing.T) {
	t.Parallel()
	c := &Company{vectors: &vectorBackend{embedder: embeddings.NewFake(4), model: "fake"}}
	embed := c.embed()
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
