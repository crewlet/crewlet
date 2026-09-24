package search_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE QUERY IS EMBEDDED ONCE, BY THE COORDINATOR, AND EVERY PARTICIPANT IS
// ASKED WITH THE SAME VECTOR.
//
// Once, because an embedding is a provider call the company is billed for,
// and a participant embedding for itself would pay it once per node for one
// value. And the same vector, in the corpus's own space: a participant handed
// the text alone runs its semantic half with nothing to rank against.
func TestTheQueryIsEmbeddedOnceAndTravelsToEveryParticipant(t *testing.T) {
	t.Parallel()
	embedder := &countingEmbedder{Fake: embeddings.NewFake(16)}
	local := &askedScan{}
	var mu sync.Mutex
	var scattered []search.FanQuery
	fan := &search.FanOut{
		Self:  "n1",
		Local: local,
		Peers: peersFunc(func(_ context.Context, q search.FanQuery,
			table []search.Assigned) ([]search.Slice, error) {
			mu.Lock()
			scattered = append(scattered, q)
			mu.Unlock()
			out := make([]search.Slice, 0, len(table))
			for _, a := range table {
				out = append(out, search.Slice{Node: a.Node, Shards: a.Shards})
			}
			return out, nil
		}),
		Space: func() (search.QuerySpace, bool) {
			return search.QuerySpace{Embedder: embedder, Model: "space-model"}, true
		},
	}
	answer := run(t, fan, search.FanQuery{Text: "rate limit backoff", Limit: 5},
		[]string{"n1", "n2"})

	if got := embedder.calls(); got != 1 {
		t.Fatalf("one search embedded its query %d time(s)", got)
	}
	mu.Lock()
	asked := append(local.queries(), scattered...)
	mu.Unlock()
	if len(asked) != 2 {
		t.Fatalf("the local scan and the scatter were asked %d time(s) together, "+
			"want once each", len(asked))
	}
	for _, q := range asked {
		if len(q.Vector) != 4*16 || q.Model != "space-model" || q.Dim != 16 {
			t.Errorf("a participant was asked with vector %d bytes, model %q, "+
				"width %d — want the query's vector in the corpus's space",
				len(q.Vector), q.Model, q.Dim)
		}
	}
	if !answer.Whole() {
		t.Errorf("a search whose query was embedded reports itself partial: %+v",
			answer)
	}
}

// A QUERY THAT CANNOT BE EMBEDDED IS ANSWERED ON ITS WORDS, AND SAYS SO.
//
// Best effort, as every search here is: a provider that is down must not stop
// a company finding what its lexical half can. What it may NOT do is answer
// as though it were whole, because what is missing is exactly the class the
// semantic half exists for — a document about the question that shares none of
// its words — and a reader with no mark takes the list for everything that
// matched.
func TestAQueryThatCannotBeEmbeddedIsAnsweredOnItsWordsAndSaysSo(t *testing.T) {
	t.Parallel()
	failing := &countingEmbedder{Fake: embeddings.NewFake(16),
		err: errors.New("the provider refused")}
	local := &askedScan{slice: search.Slice{
		Lexical: []search.Scored{{Key: "page:A", Score: 1}},
	}}
	fan := &search.FanOut{
		Self: "n1", Local: local,
		Space: func() (search.QuerySpace, bool) {
			return search.QuerySpace{Embedder: failing, Model: "space-model"}, true
		},
	}
	answer, err := fan.Search(t.Context(), search.FanQuery{Text: "anything", Limit: 5})
	if err != nil {
		t.Fatalf("a failed query embedding failed the search: %v", err)
	}
	if !slices.Equal(answer.Hits, []string{"page:A"}) {
		t.Errorf("the lexical half's answer is %v", answer.Hits)
	}
	if !answer.SemanticSkipped || answer.Whole() {
		t.Errorf("a search whose query could not be embedded reports "+
			"semantic_skipped=%v whole=%v — it answered on words alone and "+
			"says nothing about it", answer.SemanticSkipped, answer.Whole())
	}
	if q := local.queries(); len(q) != 1 || len(q[0].Vector) != 0 {
		t.Errorf("the scan was asked %+v, want a lexical query", q)
	}

	// A VECTOR OF THE WRONG WIDTH IS THE SAME FAILURE, refused before it
	// reaches a scan that could only compare it with nothing.
	narrow := &countingEmbedder{Fake: embeddings.NewFake(8)}
	fan.Space = func() (search.QuerySpace, bool) {
		return search.QuerySpace{Embedder: widthLiar{narrow, 16}, Model: "m"}, true
	}
	answer, err = fan.Search(t.Context(), search.FanQuery{Text: "anything", Limit: 5})
	if err != nil {
		t.Fatalf("a vector of the wrong width failed the search: %v", err)
	}
	if !answer.SemanticSkipped {
		t.Error("a query vector of the wrong width was not reported as skipped")
	}
}

// NO SEMANTIC HALF IS NOT A DEGRADED ONE.
//
// A company that runs its search on words alone — no embeddings provider, or
// `knowledge.vectors: false` — asks a lexical question and gets a whole
// answer to it. Marking those would put `search_degraded` in alarm for the
// life of the deployment and a caveat on every answer a seat ever reads, and
// a mark that is always there is one nobody reads.
func TestNoSemanticHalfIsNotADegradedOne(t *testing.T) {
	t.Parallel()
	embedder := &countingEmbedder{Fake: embeddings.NewFake(16)}
	for name, space := range map[string]func() (search.QuerySpace, bool){
		"no space at all": nil,
		"a space that says there is none": func() (search.QuerySpace, bool) {
			return search.QuerySpace{Embedder: embedder, Model: "m"}, false
		},
	} {
		local := &askedScan{}
		fan := &search.FanOut{Self: "n1", Local: local, Space: space}
		answer, err := fan.Search(t.Context(), search.FanQuery{Text: "anything", Limit: 5})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !answer.Whole() {
			t.Errorf("%s: a lexical search reports itself partial: %+v", name, answer)
		}
		if q := local.queries(); len(q) != 1 || len(q[0].Vector) != 0 {
			t.Errorf("%s: the scan was asked %+v, want a lexical query", name, q)
		}
	}
	if got := embedder.calls(); got != 0 {
		t.Errorf("a company with no semantic half paid the provider %d time(s)", got)
	}
}

// A HYBRID SEARCH FINDS A DOCUMENT ONLY THE VECTORS FIND, end to end over a
// real store: the lexical index built from the rows, the vectors written by
// the applier every node runs, and the scan this node answers its own range
// and its peers' with.
//
// That document is the whole reason the semantic half exists — the question
// asked in words its answer does not use — and the control is the same search
// with no embedding space, which finds nothing: the fused answer is the only
// place it can come from.
func TestAHybridSearchFindsADocumentOnlyTheVectorsFind(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	docs := map[string][2]string{
		"t-car":    {"Car maintenance", "the car needs new tyres every winter"},
		"t-budget": {"Quarterly budget", "the finance review of spending"},
	}
	for id, doc := range docs {
		item(t, db, id, "ENG", doc[0], doc[1], 1)
	}
	index := search.NewIndexerOver(db, []search.LexicalSource{search.TaskSource{}})
	indexAll(t, index)

	space := search.QuerySpace{
		Embedder: synonyms{Fake: embeddings.NewFake(64), same: map[string]string{
			"automobile": "car",
		}},
		Model: "synonym-model",
	}
	embedRows(t, db, space, docs)

	ask := func(space func() (search.QuerySpace, bool)) search.Answer {
		t.Helper()
		fan := &search.FanOut{Self: "self", Local: search.NodeScanner{Index: index},
			Space: space}
		answer, err := fan.Search(t.Context(), search.FanQuery{
			Text: "automobile", Sources: []string{string(search.SourceTask)}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		return answer
	}

	lexical := ask(nil)
	if len(lexical.Hits) != 0 {
		t.Fatalf("the control found %v on its words — no document says "+
			"\"automobile\", so this case cannot tell the halves apart",
			lexical.Hits)
	}
	hybrid := ask(func() (search.QuerySpace, bool) { return space, true })
	if len(hybrid.Hits) == 0 || hybrid.Hits[0] != search.Key(search.SourceTask, "t-car") {
		t.Fatalf("the hybrid search answered %v, want t-car first — the one "+
			"document about the question, which shares no word with it", hybrid.Hits)
	}
	if !hybrid.Whole() {
		t.Errorf("a hybrid search that ran both halves reports itself partial: %+v",
			hybrid)
	}
}

// ---- fixtures ------------------------------------------------------------

// countingEmbedder is the fake, counting its calls and failing on request.
type countingEmbedder struct {
	*embeddings.Fake
	mu  sync.Mutex
	n   int
	err error
}

func (c *countingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	return c.Fake.Embed(ctx, text)
}

func (c *countingEmbedder) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// widthLiar reports a width its vectors do not have, which is what a model
// changed behind an aggregator looks like from here.
type widthLiar struct {
	embeddings.Embedder
	width int
}

func (w widthLiar) Width() int { return w.width }

// synonyms is the fake with a thesaurus in front of it: a word it knows is
// embedded as its synonym, so a query and a document that share no word can
// share a meaning.
type synonyms struct {
	*embeddings.Fake
	same map[string]string
}

func (s synonyms) Embed(ctx context.Context, text string) ([]float32, error) {
	words := strings.Fields(strings.ToLower(text))
	for i, word := range words {
		if other, ok := s.same[word]; ok {
			words[i] = other
		}
	}
	return s.Fake.Embed(ctx, strings.Join(words, " "))
}

// askedScan answers one canned slice and records every query it was asked.
type askedScan struct {
	mu    sync.Mutex
	asked []search.FanQuery
	slice search.Slice
}

func (a *askedScan) Scan(_ context.Context, q search.FanQuery, _ search.Assignment) (search.Slice, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, q)
	return a.slice, nil
}

func (a *askedScan) queries() []search.FanQuery {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.asked)
}

// embedRows writes each work item's vector the way every node's applier does,
// one record per document at its own position — the rows a scan reads, with
// no broker in between.
func embedRows(t *testing.T, db *store.DB, space search.QuerySpace, docs map[string][2]string) {
	t.Helper()
	applier := search.NewApplier()
	ids := make([]string, 0, len(docs))
	for id := range docs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for i, id := range ids {
		vector, err := space.Embedder.Embed(t.Context(), docs[id][0]+"\n\n"+docs[id][1])
		if err != nil {
			t.Fatalf("embed %s: %v", id, err)
		}
		payload, err := search.VectorRecord{
			RecordEnvelope: search.RecordEnvelope{
				Subject: search.Subject{Source: search.SourceTask, ID: id},
				Op:      search.OpEmbed,
			},
			Container: "ENG", Model: space.Model, Dim: space.Embedder.Width(),
			SourceRev: 1, Chunks: 1, Embedding: pack(vector),
		}.Encode()
		if err != nil {
			t.Fatalf("encode the vector record for %s: %v", id, err)
		}
		record := statelog.Record{
			Payload: payload,
			Position: statelog.Position{
				Stream: search.Domain{}.Stream().Name, Seq: uint64(i + 1),
			},
		}
		if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := applier.Apply(t.Context(), tx, record, statelog.ApplyOptions{})
			return err
		}); err != nil {
			t.Fatalf("apply the vector for %s: %v", id, err)
		}
	}
}
