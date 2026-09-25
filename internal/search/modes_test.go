package search_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/search"
)

// The three modes, the query's own vector, and what an answer says it did.

// countingEmbedder is the fake embedder, counting the provider calls a search
// made, and failing when told to.
type countingEmbedder struct {
	*embeddings.Fake
	calls atomic.Int64
	fail  error
}

func (c *countingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	c.calls.Add(1)
	if c.fail != nil {
		return nil, c.fail
	}
	return c.Fake.Embed(ctx, text)
}

// provider is a company's embeddings configuration as the fan-out reads it:
// the embedder, the model id, and whether semantic search is on.
type provider struct {
	mu    sync.Mutex
	embed *countingEmbedder
	model string
	on    bool
}

func (p *provider) read() (embeddings.Embedder, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.embed, p.model, p.on
}

func (p *provider) set(model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.model = model
}

func newProvider() *provider {
	return &provider{embed: &countingEmbedder{Fake: embeddings.NewFake(16)}, model: "m1", on: true}
}

// recordingScan answers a canned slice and records every query it was asked.
type recordingScan struct {
	mu    sync.Mutex
	asked []search.FanQuery
	slice search.Slice
}

func (r *recordingScan) Scan(_ context.Context, q search.FanQuery, _ search.Assignment) (search.Slice, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, q)
	return r.slice, nil
}

func (r *recordingScan) Scatter(_ context.Context, q search.FanQuery, _ []search.Assigned) ([]search.Slice, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, q)
	return []search.Slice{r.slice}, nil
}

func (r *recordingScan) queries() []search.FanQuery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked)
}

// bothHalves is one participant that found A by its words and B by its
// meaning — so which of the two reach the fused answer says which rankers ran.
func bothHalves(node string, shards search.Assignment) search.Slice {
	return search.Slice{
		Node: node, Shards: shards,
		Lexical:  []search.Scored{{Key: "page:A", Score: 3.2}},
		Semantic: []search.Scored{{Key: "page:B", Score: -0.1}},
	}
}

// THE QUERY'S VECTOR REACHES EVERY PARTICIPANT, and it is the one piece the
// semantic half was missing: the corpus was embedded, replicated and paid for,
// while both callers of the fan-out passed text alone and every search skipped
// its semantic slice.
func TestTheQueryVectorReachesEveryParticipant(t *testing.T) {
	t.Parallel()
	p := newProvider()
	local := &recordingScan{slice: bothHalves("n1", search.Assignment{From: 0, To: 32})}
	peer := &recordingScan{slice: bothHalves("n2", search.Assignment{From: 32, To: 64})}
	fan := &search.FanOut{Self: "n1", Local: local, Peers: peer,
		Vectors: search.NewQueryVectors(p.read)}

	answer := run(t, fan, search.FanQuery{Text: "rollback the deploy", Limit: 10},
		[]string{"n1", "n2"})

	for who, scan := range map[string]*recordingScan{"the local scan": local, "the peer": peer} {
		asked := scan.queries()
		if len(asked) != 1 {
			t.Fatalf("%s was asked %d times", who, len(asked))
		}
		q := asked[0]
		if len(q.Vector) != 4*16 || q.Model != "m1" || q.Dim != 16 {
			t.Errorf("%s was asked with a %d-byte vector at model %q dim %d — "+
				"a participant with no vector cannot rank by meaning",
				who, len(q.Vector), q.Model, q.Dim)
		}
		if !slices.Equal(q.Methods, []search.Method{search.MethodLexical, search.MethodSemantic}) {
			t.Errorf("%s was asked to run %v", who, q.Methods)
		}
	}
	if answer.Served != knowledge.ModeHybrid || answer.Degraded != knowledge.NotDegraded {
		t.Errorf("served %q degraded %q, want an undegraded hybrid answer",
			answer.Served, answer.Degraded)
	}
	if !slices.Equal(answer.Modes, knowledge.Modes) {
		t.Errorf("modes = %v, want all three with a provider", answer.Modes)
	}
	if !slices.Contains(answer.Hits, "page:B") {
		t.Errorf("the fused answer %v lost the document only its meaning found", answer.Hits)
	}
}

// EACH MODE FUSES EXACTLY THE RANKERS IT NAMES.
//
// Hybrid is both lists by reciprocal rank fusion; keyword is the words alone;
// semantic is meaning alone. A participant on an older build runs both halves
// whatever it is asked — so the coordinator, not the participant, is what
// keeps a keyword search from quietly becoming a hybrid one.
func TestEachModeFusesExactlyTheRankersItNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode knowledge.Mode
		want []string
	}{
		{knowledge.ModeHybrid, []string{"page:A", "page:B"}},
		{knowledge.ModeKeyword, []string{"page:A"}},
		{knowledge.ModeSemantic, []string{"page:B"}},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()
			// BOTH HALVES IN EVERY SLICE, as an older participant
			// that ignores Methods would send them.
			local := &recordingScan{slice: bothHalves("n1", search.Everything())}
			fan := &search.FanOut{Self: "n1", Local: local,
				Vectors: search.NewQueryVectors(newProvider().read)}
			answer, err := fan.Search(t.Context(), search.FanQuery{
				Text: "rollback", Mode: tc.mode, Limit: 10,
			})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if !slices.Equal(answer.Hits, tc.want) {
				t.Errorf("%s answered %v, want %v", tc.mode, answer.Hits, tc.want)
			}
			if answer.Served != tc.mode {
				t.Errorf("served %q, want %q", answer.Served, tc.mode)
			}
			if tc.mode == knowledge.ModeKeyword && len(local.queries()[0].Vector) != 0 {
				t.Error("a keyword search sent a vector, which costs every " +
					"participant a vector scan nobody asked for")
			}
		})
	}
}

// NO PROVIDER IS NOT A SILENT KEYWORD SEARCH.
//
// Hybrid serves its keyword half and says so; semantic serves NOTHING, because
// the keyword ranking is the one ranking guaranteed not to find what somebody
// asking for meaning is looking for — and runs no scan to do it. `knowledge.
// vectors: false` is the same answer as no provider at all.
func TestWithNoProviderHybridServesKeywordAndSemanticServesNothing(t *testing.T) {
	t.Parallel()
	off := newProvider()
	off.on = false
	for name, vectors := range map[string]*search.QueryVectors{
		"no provider":     nil,
		"vectors off":     search.NewQueryVectors(off.read),
		"no model config": search.NewQueryVectors(nil),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			local := &recordingScan{slice: bothHalves("n1", search.Everything())}
			fan := &search.FanOut{Self: "n1", Local: local, Vectors: vectors}

			hybrid, err := fan.Search(t.Context(), search.FanQuery{Text: "rollback", Limit: 10})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if hybrid.Served != knowledge.ModeKeyword ||
				hybrid.Degraded != knowledge.DegradedNoEmbeddings {
				t.Errorf("hybrid with no provider served %q degraded %q, want "+
					"keyword and no_embeddings", hybrid.Served, hybrid.Degraded)
			}
			if !slices.Equal(hybrid.Hits, []string{"page:A"}) {
				t.Errorf("hybrid with no provider answered %v", hybrid.Hits)
			}
			if !slices.Equal(hybrid.Modes, []knowledge.Mode{knowledge.ModeKeyword}) {
				t.Errorf("modes = %v, want keyword alone", hybrid.Modes)
			}

			semantic, err := fan.Search(t.Context(), search.FanQuery{
				Text: "rollback", Mode: knowledge.ModeSemantic, Limit: 10,
			})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if semantic.Served != "" || len(semantic.Hits) != 0 ||
				semantic.Degraded != knowledge.DegradedNoEmbeddings {
				t.Errorf("semantic with no provider served %q with %v degraded %q",
					semantic.Served, semantic.Hits, semantic.Degraded)
			}
			if n := len(local.queries()); n != 1 {
				t.Errorf("the scanner ran %d times, want once — a semantic "+
					"search with nothing to rank by must not scan", n)
			}
		})
	}
}

// A FAILED QUERY EMBEDDING IS A DEGRADATION, and a different one from having
// no provider: nothing is misconfigured, and the next search asks again —
// which is why a failure is never cached.
func TestAFailedQueryEmbeddingServesKeywordAndIsNotCached(t *testing.T) {
	t.Parallel()
	p := newProvider()
	p.embed.fail = errors.New("the provider is rate limiting")
	vectors := search.NewQueryVectors(p.read)
	local := &recordingScan{slice: bothHalves("n1", search.Everything())}
	fan := &search.FanOut{Self: "n1", Local: local, Vectors: vectors}

	for range 2 {
		answer, err := fan.Search(t.Context(), search.FanQuery{Text: "rollback", Limit: 10})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if answer.Served != knowledge.ModeKeyword ||
			answer.Degraded != knowledge.DegradedEmbeddingFailed {
			t.Errorf("served %q degraded %q, want keyword and embedding_failed",
				answer.Served, answer.Degraded)
		}
	}
	if got := p.embed.calls.Load(); got != 2 {
		t.Errorf("two searches made %d provider calls — a failure that was "+
			"cached would degrade every search on this phrase until eviction", got)
	}
	if vectors.Len() != 0 {
		t.Errorf("%d failed vectors were cached", vectors.Len())
	}
}

// A SEARCH THAT RAN NOTHING IS STILL AN ANSWER, AND IT IS REPORTED.
//
// A semantic search whose query vector could not be computed runs no scan and
// returns early — and the early return used to skip the report hook, so the
// purest degraded search there is never reached `search_degraded`'s fraction,
// while the hybrid search that failed the same way did.
func TestASemanticSearchThatRanNothingIsStillReported(t *testing.T) {
	t.Parallel()
	failing := newProvider()
	failing.embed.fail = errors.New("the provider is rate limiting")
	for _, tc := range []struct {
		name    string
		vectors *search.QueryVectors
		want    knowledge.Degradation
	}{
		{"no provider", nil, knowledge.DegradedNoEmbeddings},
		{"the provider failed", search.NewQueryVectors(failing.read),
			knowledge.DegradedEmbeddingFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var reported []search.Answer
			fan := &search.FanOut{Self: "n1", Vectors: tc.vectors,
				Local: &recordingScan{slice: bothHalves("n1", search.Everything())},
				Report: func(a search.Answer, _ time.Duration) {
					reported = append(reported, a)
				}}
			if _, err := fan.Search(t.Context(), search.FanQuery{
				Text: "rollback", Mode: knowledge.ModeSemantic, Limit: 10,
			}); err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(reported) != 1 {
				t.Fatalf("the hook was told about %d answers, want 1 — an "+
					"answer nobody counted is an alarm that cannot fire",
					len(reported))
			}
			if got := reported[0]; got.Served != "" || got.Degraded != tc.want {
				t.Errorf("reported served %q degraded %q, want nothing served "+
					"and %q", got.Served, got.Degraded, tc.want)
			}
		})
	}
}

// A QUERY VECTOR IS COMPUTED ONCE PER MODEL, and a model change empties the
// cache rather than ranking the old model's vector against the new model's
// rows — [search.Semantic] filters by model, so that ranking would match only
// the rows not yet re-embedded and shrink as the refill progressed.
func TestAQueryVectorIsCachedUntilTheModelMoves(t *testing.T) {
	t.Parallel()
	p := newProvider()
	vectors := search.NewQueryVectors(p.read)

	first, deg := vectors.Vector(t.Context(), "rollback the deploy")
	if deg != knowledge.NotDegraded || first.Model != "m1" {
		t.Fatalf("first vector: %+v degraded %q", first, deg)
	}
	again, _ := vectors.Vector(t.Context(), "  rollback the deploy ")
	if p.embed.calls.Load() != 1 {
		t.Errorf("the same phrase twice made %d provider calls, want 1",
			p.embed.calls.Load())
	}
	if !slices.Equal(first.Vector, again.Vector) {
		t.Error("a cache hit answered a different vector")
	}

	p.set("m2")
	moved, _ := vectors.Vector(t.Context(), "rollback the deploy")
	if p.embed.calls.Load() != 2 || moved.Model != "m2" {
		t.Errorf("after a model change the phrase made %d calls in model %q — "+
			"a vector from the retired model was served from the cache",
			p.embed.calls.Load(), moved.Model)
	}
	if vectors.Len() != 1 {
		t.Errorf("the cache holds %d vectors after the model moved, want the "+
			"one computed in the new space", vectors.Len())
	}
}

// THE CACHE IS BOUNDED, least recently used first out, so a node that has
// served a year of distinct queries holds QueryEmbeddingCacheEntries of them
// rather than every one.
func TestTheQueryVectorCacheIsBounded(t *testing.T) {
	t.Parallel()
	p := newProvider()
	vectors := search.NewQueryVectors(p.read)
	for i := range search.QueryEmbeddingCacheEntries + 10 {
		if _, deg := vectors.Vector(t.Context(), fmt.Sprintf("phrase %d", i)); deg != "" {
			t.Fatalf("phrase %d: %q", i, deg)
		}
	}
	if vectors.Len() != search.QueryEmbeddingCacheEntries {
		t.Fatalf("the cache holds %d vectors, want the bound %d",
			vectors.Len(), search.QueryEmbeddingCacheEntries)
	}
	// THE OLDEST WENT, the newest stayed.
	calls := p.embed.calls.Load()
	vectors.Vector(t.Context(), fmt.Sprintf("phrase %d", search.QueryEmbeddingCacheEntries+9))
	if p.embed.calls.Load() != calls {
		t.Error("the most recent phrase was evicted")
	}
	vectors.Vector(t.Context(), "phrase 0")
	if p.embed.calls.Load() != calls+1 {
		t.Error("the least recently used phrase survived past the bound")
	}
}

// AN ABSENT NODE MAKES THE ANSWER INCOMPLETE, BY NAME.
//
// The partial fan-out used to be a log line on the coordinating node. It now
// rides in the answer's coverage — every participant, whether it answered, and
// why not — which is what a screen and a seat read to tell a short answer from
// a short corpus.
func TestAnAbsentNodeMakesCoverageIncompleteByName(t *testing.T) {
	t.Parallel()
	mine := bothHalves("n1", search.Assignment{From: 0, To: 22})
	building := search.Slice{Node: "n2", Shards: search.Assignment{From: 22, To: 43}, Building: true}
	fan := &search.FanOut{Self: "n1", Local: fixed{mine}, Peers: fixedMany{building}}
	answer := run(t, fan, search.FanQuery{Text: "rollback", Limit: 10},
		[]string{"n1", "n2", "n3"})

	cov := answer.Coverage()
	if cov.Complete || cov.BucketsMissing != 42 {
		t.Fatalf("coverage = %+v, want incomplete with n2's and n3's 42 buckets", cov)
	}
	want := map[string]bool{"n1": true, "n2": false, "n3": false}
	if len(cov.Nodes) != 3 {
		t.Fatalf("coverage names %d nodes, want every participant", len(cov.Nodes))
	}
	for _, node := range cov.Nodes {
		if node.Answered != want[node.ID] {
			t.Errorf("node %s answered=%v", node.ID, node.Answered)
		}
		if !node.Answered && node.Error == "" {
			t.Errorf("node %s did not answer and says nothing about why", node.ID)
		}
	}
	if cov.Nodes[1].Error == cov.Nodes[2].Error {
		t.Errorf("a building index and a silent node read the same: %q",
			cov.Nodes[1].Error)
	}

	whole := run(t, &search.FanOut{Self: "n1", Local: fixed{bothHalves("n1",
		search.Assignment{From: 0, To: 32})}, Peers: fixed{bothHalves("n2",
		search.Assignment{From: 32, To: 64})}}, search.FanQuery{Text: "x"},
		[]string{"n1", "n2"}).Coverage()
	if !whole.Complete || whole.BucketsMissing != 0 {
		t.Errorf("a fully answered search reports %+v", whole)
	}
}

// A PARTICIPANT THAT LOST ITS SEMANTIC HALF DEGRADES THE ANSWER, and a keyword
// search — which never asked for that half — is not degraded by it.
func TestALostSemanticHalfIsReportedOnlyWhereItWasAsked(t *testing.T) {
	t.Parallel()
	lost := bothHalves("n1", search.Everything())
	lost.SemanticSkipped = true
	fan := &search.FanOut{Self: "n1", Local: fixed{lost},
		Vectors: search.NewQueryVectors(newProvider().read)}

	hybrid, err := fan.Search(t.Context(), search.FanQuery{Text: "rollback"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if hybrid.Degraded != knowledge.DegradedSemanticPartial {
		t.Errorf("hybrid degraded %q, want semantic_partial", hybrid.Degraded)
	}
	keyword, err := fan.Search(t.Context(), search.FanQuery{Text: "rollback",
		Mode: knowledge.ModeKeyword})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if keyword.Degraded != knowledge.NotDegraded || keyword.SemanticSkipped {
		t.Errorf("a keyword search reports degraded %q skipped %v over a half "+
			"it never asked for", keyword.Degraded, keyword.SemanticSkipped)
	}
}

// THE RANKERS TRAVEL ON THE WIRE, so a peer of a keyword search runs no vector
// scan and a peer of a semantic one runs no BM25.
func TestTheAskedRankersTravelToAPeer(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	coordinator := startClient(t, broker)
	peer := &recordingScan{}
	stop, err := search.ServeSlices(t.Context(), startClient(t, broker), "n2", peer)
	if err != nil {
		t.Fatalf("serve slices: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })

	fan := &search.FanOut{
		Self:    "n1",
		Local:   fixed{search.Slice{Node: "n1"}},
		Peers:   search.Broker{Queue: coordinator},
		Roster:  func(context.Context) ([]string, error) { return []string{"n1", "n2"}, nil },
		Corpus:  func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
		Budget:  5 * time.Second,
		Vectors: search.NewQueryVectors(newProvider().read),
	}
	if _, err := fan.Search(t.Context(), search.FanQuery{Text: "rollback",
		Mode: knowledge.ModeSemantic}); err != nil {
		t.Fatalf("search: %v", err)
	}
	asked := peer.queries()
	if len(asked) != 1 {
		t.Fatalf("the peer was asked %d times", len(asked))
	}
	if !slices.Equal(asked[0].Methods, []search.Method{search.MethodSemantic}) ||
		len(asked[0].Vector) != 4*16 {
		t.Errorf("the peer was asked to run %v with a %d-byte vector",
			asked[0].Methods, len(asked[0].Vector))
	}
}

// THE SEMANTIC HALF IS QUERIED, END TO END: the duty embeds the corpus, the
// record is applied, and a search with the company's provider ranks by meaning
// through the real two-stage statement — the path that until now no live
// search ever took.
func TestASearchRanksByMeaningOverWhatTheDutyEmbedded(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.seedTasks(map[string]string{
		"t-rate":  "rate limits and 429 backoff in the GitLab client",
		"t-cache": "the page cache size the store asks for at open",
	})
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	h.drain()
	x := search.NewIndexerOver(h.db, []search.LexicalSource{search.TaskSource{}})
	indexAll(t, x)

	fan := &search.FanOut{Self: "n1", Local: search.NodeScanner{Index: x},
		Vectors: search.NewQueryVectors(func() (embeddings.Embedder, string, bool) {
			return h.embedder, embedModel, true
		})}
	want := search.Key(search.SourceTask, "t-rate")
	for _, mode := range knowledge.Modes {
		answer, err := fan.Search(t.Context(), search.FanQuery{
			Text: "429 rate limit backoff", Mode: mode, Limit: 5,
		})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if answer.Served != mode || answer.Degraded != knowledge.NotDegraded ||
			answer.SemanticSkipped {
			t.Errorf("%s served %q degraded %q skipped %v", mode, answer.Served,
				answer.Degraded, answer.SemanticSkipped)
		}
		if len(answer.Hits) == 0 || answer.Hits[0] != want {
			t.Errorf("%s answered %v, want %s first", mode, answer.Hits, want)
		}
	}
}
