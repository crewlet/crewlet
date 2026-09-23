package search

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textindex"
)

// WHAT A CAPPED READ LEAVES OUT HAS TO BE THE BOTTOM OF ITS LIST.
//
// [Indexer.Search] decides whether its first read was enough on one bound:
// that nothing a capped read left out scores more than the first posting it
// left out. That holds only if the read is sorted by the term's own BM25
// contribution — sorted any other way, the posting after the cut is no bound
// on the ones after it, and a search that trusts it can skip the whole read it
// needed.
//
// It was `ORDER BY p.freq DESC`, which is that order only if every document
// is the same length. This test is the fixture where the two disagree: a long
// document mentioning the term more often than a short one, where BM25 —
// which divides by length — ranks the short one first. Ordering by raw
// frequency puts the runbook at the head of the list and the page that is
// ABOUT the term at the tail, which is where a LIMIT cuts.
//
// AN INTERNAL TEST because the property is about `postings`, and through the
// exported surface it takes a corpus past [maxPostingScan] to see at all — an
// index of thousands of documents to assert what three rows already show.
func TestACappedPostingListKeepsTheHighestScoringPostings(t *testing.T) {
	t.Parallel()

	db := openInternalStore(t)
	x := NewIndexer(db)

	// One term, three documents. `dense` says it twice in fifty terms;
	// `bulky` says it three times in six hundred. Raw frequency ranks
	// bulky first; BM25 ranks it last.
	indexDoc(t, db, "dense", 2, 50)
	indexDoc(t, db, "bulky", 3, 600)
	indexDoc(t, db, "middling", 2, 200)

	corpus, err := x.corpus(t.Context())
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	list, err := x.postings(t.Context(), "deploy", LexicalQuery{}, corpus, maxPostingScan)
	if err != nil {
		t.Fatalf("postings: %v", err)
	}
	got := list.postings
	if len(got) != 3 || list.capped {
		t.Fatalf("read %d postings (capped %v) of a term three documents hold",
			len(got), list.capped)
	}

	// THE ORDER THE SCORER ITSELF WOULD PUT THEM IN, computed here rather
	// than written down: an expectation spelled as a list of ids would
	// pass a SQL expression that agreed with it by accident and say
	// nothing about whether it agrees with [textindex.Score].
	idf := textindex.IDF(corpus.Docs, 3)
	if list.idf != idf {
		t.Fatalf("the term's IDF is %v, want %v over the three documents that "+
			"hold it", list.idf, idf)
	}
	want := slices.Clone(got)
	slices.SortStableFunc(want, func(a, b textindex.Posting) int {
		sa, sb := textindex.Score(idf, a, corpus), textindex.Score(idf, b, corpus)
		switch {
		case sa > sb:
			return -1
		case sa < sb:
			return 1
		}
		return 0
	})
	for i := range got {
		if got[i].DocID != want[i].DocID {
			t.Fatalf("read %v, want %v — the cut is not the bottom of the list",
				ids(got), ids(want))
		}
	}

	// AND THE FIXTURE DISCRIMINATES. Without this the case passes on a
	// corpus where raw frequency and BM25 happen to agree, which is every
	// corpus whose documents are all one length.
	if got[0].DocID == "bulky" {
		t.Fatal("the densest document did not come first: this fixture no " +
			"longer separates BM25 order from raw-frequency order, so it " +
			"would pass under the ordering it exists to reject")
	}
}

func ids(ps []textindex.Posting) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.DocID)
	}
	return out
}

// indexDoc writes one document and one posting straight into the index,
// rather than driving a sweep: the property under test is the READ's order,
// and a fixture that went through the analyzer would have to be prose chosen
// to produce a term count instead of stating one.
func indexDoc(t *testing.T, db *store.DB, id string, freq, length int) {
	t.Helper()
	if _, err := db.SQL().ExecContext(t.Context(),
		`INSERT INTO kb_docs (id, source, source_id, search_shard, container,
		                      title, excerpt, length, source_rev, indexed_at)
		 VALUES (?, 'pages', ?, 0, 'ENG', ?, '', ?, 1, 0)`,
		id, id, id, length); err != nil {
		t.Fatalf("insert doc %s: %v", id, err)
	}
	if _, err := db.SQL().ExecContext(t.Context(),
		`INSERT INTO kb_postings (doc_id, term, freq) VALUES (?, 'deploy', ?)`,
		id, freq); err != nil {
		t.Fatalf("insert posting %s: %v", id, err)
	}
}

func openInternalStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return db
}
