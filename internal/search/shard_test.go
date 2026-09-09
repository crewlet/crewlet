package search_test

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
)

// A SEARCH SCANS ONLY ITS ASSIGNED BUCKETS, which is the whole point of the
// column: without the predicate the rows a node reads are unchanged by the
// fan-out, and every node still pays for the whole corpus.
func TestASearchScansOnlyItsAssignedBuckets(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	// One document per bucket, each carrying the same word, so a search
	// over an assignment returns exactly the documents in it.
	inBucket := map[int]string{}
	for i := range 200 {
		id := fmt.Sprintf("p.%03d", i)
		shard := search.ShardOf("page", id)
		if _, held := inBucket[shard]; held {
			continue
		}
		inBucket[shard] = id
		page(t, db, id, "ENG", "Doc "+id, "the migration plan is here", 1)
	}
	if len(inBucket) < 8 {
		t.Fatalf("only %d of %d buckets were populated — the fixture cannot "+
			"demonstrate a narrowed scan", len(inBucket), search.SearchShards)
	}
	indexAll(t, x)

	whole, err := x.Search(t.Context(), search.SearchQuery{
		Text: "migration plan", Limit: 500,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(whole) != len(inBucket) {
		t.Fatalf("the unassigned search found %d of %d documents — the zero "+
			"assignment must be EVERY bucket, because one meaning none would "+
			"answer every search with nothing", len(whole), len(inBucket))
	}

	// HALF THE RANGE, and the answer is exactly the documents in it.
	half := search.SearchShards / 2
	part, err := x.Search(t.Context(), search.SearchQuery{
		Text: "migration plan", Limit: 500,
		Shards: search.Assignment{From: 0, To: half},
	})
	if err != nil {
		t.Fatalf("assigned search: %v", err)
	}
	want := 0
	for shard := range inBucket {
		if shard < half {
			want++
		}
	}
	if len(part) != want {
		t.Fatalf("an assignment of buckets [0,%d) returned %d documents, want "+
			"%d — a scan that ignores its assignment reads the whole corpus "+
			"whatever the fan-out says", half, len(part), want)
	}
	for _, hit := range part {
		if got := search.ShardOf(hit.Source, hit.ID); got >= half {
			t.Errorf("%s is in bucket %d and was returned by an assignment of "+
				"[0,%d)", hit.ID, got, half)
		}
	}

	// AND THE TWO HALVES PARTITION THE WHOLE, which is what makes a
	// fan-out lossless: a document in neither half is one no node returns.
	rest, err := x.Search(t.Context(), search.SearchQuery{
		Text: "migration plan", Limit: 500,
		Shards: search.Assignment{From: half, To: search.SearchShards},
	})
	if err != nil {
		t.Fatalf("the second half: %v", err)
	}
	if len(part)+len(rest) != len(whole) {
		t.Fatalf("two halves returned %d + %d documents and the whole corpus is "+
			"%d — a fan-out that loses a document loses it silently",
			len(part), len(rest), len(whole))
	}
}

// SHARDS ARE EVEN AND STABLE.
//
// EVEN, because a skewed hash puts a company's corpus in a few buckets and the
// node holding them scans as much as the whole fleet did. STABLE, because a
// source that MOVES must keep its bucket: a query scanning the old one misses
// it, and a search returning one fewer result looks exactly like a corpus with
// one fewer document.
func TestSearchShardsAreEvenAndStable(t *testing.T) {
	t.Parallel()

	// THE DISTRIBUTION, over a corpus larger than any single bucket's
	// expected count, checked with a chi-squared statistic against the
	// 99.9% critical value for 63 degrees of freedom.
	const corpus = 100_000
	counts := make([]int, search.SearchShards)
	for i := range corpus {
		counts[search.ShardOf("page", fmt.Sprintf("%08x-0000-4000-8000-000000000000", i))]++
	}
	expected := float64(corpus) / float64(search.SearchShards)
	chi := 0.0
	for _, got := range counts {
		d := float64(got) - expected
		chi += d * d / expected
	}
	// 103.442 is the 99.9th percentile of chi-squared with 63 degrees of
	// freedom. A hash this far from even would put a measurable share of
	// the corpus in the wrong place.
	const critical = 103.442
	if chi > critical {
		t.Errorf("chi-squared over %d buckets is %.1f, past the 99.9%% critical "+
			"value %.3f — the corpus is not evenly divided, so one node's "+
			"share of a fan-out is not one node's share of the work",
			search.SearchShards, chi, critical)
	}
	if math.IsNaN(chi) {
		t.Fatal("the statistic is NaN")
	}

	// STABLE ACROSS EVERYTHING THAT IS NOT THE IDENTITY. A document's
	// bucket is a function of its source kind and its id, so moving it
	// between containers, re-filing it, re-embedding it or changing its
	// title cannot move it — none of those are inputs.
	const id = "b1b2b3b4-0000-4000-8000-000000000001"
	first := search.ShardOf("page", id)
	for range 100 {
		if got := search.ShardOf("page", id); got != first {
			t.Fatalf("one id hashed to %d and then to %d", first, got)
		}
	}

	// AND THE SOURCE KIND IS IN THE HASH, because the ids are not one
	// namespace: a page is a uuid and a work item is a project key, and
	// two corpora hashed separately would each be even while their union
	// was not.
	same := 0
	for i := range 1000 {
		key := fmt.Sprintf("ENG-%d", i)
		if search.ShardOf("page", key) == search.ShardOf("item", key) {
			same++
		}
	}
	if same > 1000/search.SearchShards*3 {
		t.Errorf("%d of 1000 ids landed in the same bucket for both source "+
			"kinds — the kind is not reaching the hash", same)
	}
}

// ONE REPLICA SCANS EVERYTHING, EXACTLY AS BEFORE.
//
// The column and the predicate are a measurement, not a behaviour change: a
// node that holds the whole corpus must return exactly what it returned
// before, or the step that added the column changed what a company can find.
func TestOneReplicaScansEverythingExactlyAsToday(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)
	for i := range 40 {
		page(t, db, fmt.Sprintf("p.%02d", i), "ENG", fmt.Sprintf("Doc %02d", i),
			"the migration plan is here", 1)
	}
	indexAll(t, x)

	unassigned, err := x.Search(t.Context(), search.SearchQuery{
		Text: "migration plan", Limit: 100,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	whole, err := x.Search(t.Context(), search.SearchQuery{
		Text: "migration plan", Limit: 100,
		Shards: search.Assignment{From: 0, To: search.SearchShards},
	})
	if err != nil {
		t.Fatalf("search over every bucket: %v", err)
	}
	if len(unassigned) != 40 || len(whole) != 40 {
		t.Fatalf("unassigned returned %d and every-bucket returned %d, want 40 "+
			"each", len(unassigned), len(whole))
	}
	// AND IN THE SAME ORDER. A predicate that changed the ranking would be
	// a search that answers differently depending on how the fleet is
	// divided, which is the one thing a fan-out must never do.
	for i := range unassigned {
		if unassigned[i].ID != whole[i].ID {
			t.Fatalf("rank %d is %s unassigned and %s over every bucket — the "+
				"predicate moved the ranking", i, unassigned[i].ID, whole[i].ID)
		}
	}
}

// EVERY INDEXED DOCUMENT CARRIES ITS OWN BUCKET, in both estates.
//
// The lexical row and the vector rows are written by different writers into
// different databases, and a document whose two shards disagreed would be
// scanned in one bucket and reranked in another — found by neither.
func TestBothEstatesAgreeOnADocumentsBucket(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)
	const id = "p.agree"
	page(t, db, id, "ENG", "Agreement", "the migration plan is here", 1)
	indexAll(t, x)

	var stored int
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT search_shard FROM kb_docs WHERE source = ? AND source_id = ?`,
			"page", id).Scan(&stored)
	}); err != nil {
		t.Fatalf("read the indexed shard: %v", err)
	}
	if want := search.ShardOf("page", id); stored != want {
		t.Fatalf("the index stored bucket %d and the function says %d — a row "+
			"whose shard drifted from its id is scanned in one bucket and "+
			"looked for in another", stored, want)
	}
}

// THE CANDIDATE POOL IS NARROWED TOO.
//
// The vector scan is a SEPARATE statement in a separate estate, and it is the
// one the fan-out exists for: the lexical index is a node's own, while the
// vectors are replicated, so every node holds every vector and stage one reads
// all of them. A predicate the lexical half has and this half does not is a
// fan-out that divides the cheap scan and leaves the expensive one whole.
func TestTheCandidatePoolIsNarrowedByTheAssignment(t *testing.T) {
	t.Parallel()
	db, dim, model := seedVectors(t, 300)
	rng := rand.New(rand.NewPCG(21, 21))
	query := randomEmbedding(rng, dim)

	scan := func(a search.Assignment) []search.SemanticHit {
		t.Helper()
		var hits []search.SemanticHit
		if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
			var err error
			hits, err = search.Semantic(t.Context(), tx, search.SemanticQuery{
				Vector: query, Model: model, Dim: dim,
				Limit: 500, Candidates: 500, Shards: a,
			})
			return err
		}); err != nil {
			t.Fatalf("semantic scan: %v", err)
		}
		return hits
	}

	whole := scan(search.Everything())
	if len(whole) != 300 {
		t.Fatalf("the unassigned scan returned %d of 300 vectors — the zero "+
			"assignment must be EVERY bucket", len(whole))
	}

	half := search.SearchShards / 2
	low := scan(search.Assignment{From: 0, To: half})
	high := scan(search.Assignment{From: half, To: search.SearchShards})
	if len(low)+len(high) != len(whole) {
		t.Fatalf("two halves of the candidate pool returned %d + %d vectors and "+
			"the whole pool is %d — a rerank that loses a candidate loses it "+
			"silently", len(low), len(high), len(whole))
	}
	if len(low) == 0 || len(high) == 0 || len(low) == len(whole) {
		t.Fatalf("a half of the bucket range returned %d of %d vectors — the "+
			"predicate is not reaching the narrow table", len(low), len(whole))
	}
	for _, h := range low {
		if got := search.ShardOf(string(h.Source), h.ID); got >= half {
			t.Errorf("%s/%s is in bucket %d and was returned by an assignment "+
				"of [0,%d)", h.Source, h.ID, got, half)
		}
	}

	// AND THE RANKING WITHIN AN ASSIGNMENT IS THE RANKING THE WHOLE SCAN
	// GAVE THOSE SAME DOCUMENTS. A predicate that reordered its survivors
	// would make a hit's rank depend on how the fleet is divided.
	var wantOrder []string
	for _, h := range whole {
		if search.ShardOf(string(h.Source), h.ID) < half {
			wantOrder = append(wantOrder, search.Key(h.Source, h.ID))
		}
	}
	var gotOrder []string
	for _, h := range low {
		gotOrder = append(gotOrder, search.Key(h.Source, h.ID))
	}
	if !slices.Equal(gotOrder, wantOrder) {
		t.Fatalf("the assigned scan ranked\n  %v\nand the whole scan ranks those "+
			"same documents\n  %v", gotOrder, wantOrder)
	}
}

// A DOCUMENT'S TWO ROWS CARRY ONE BUCKET.
//
// The narrow row is what stage one scans and the wide row is what the rerank
// reads, and they are written by one applier into one transaction. A shard
// that differed between them would put a candidate in one bucket and its
// vector in another, so the assignment holding the candidate could not rerank
// it and the assignment holding the vector never proposed it.
func TestTheVectorsTwoRowsCarryOneBucket(t *testing.T) {
	t.Parallel()
	db, _, _ := seedVectors(t, 20)
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT v.source, v.source_id, v.search_shard, b.search_shard
			   FROM kb_vectors v JOIN kb_vectors_bin b
			     ON b.source = v.source AND b.source_id = v.source_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			var source, id string
			var wide, narrow int
			if err := rows.Scan(&source, &id, &wide, &narrow); err != nil {
				return err
			}
			seen++
			want := search.ShardOf(source, id)
			if wide != want || narrow != want {
				return fmt.Errorf("%s/%s is bucket %d wide, %d narrow, and the "+
					"function says %d", source, id, wide, narrow, want)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if seen != 20 {
			return fmt.Errorf("%d of 20 documents have both rows", seen)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
